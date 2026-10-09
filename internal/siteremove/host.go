package siteremove

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/appremove"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/hostprep"
	"github.com/paisans-software/paisans-stack/internal/ownership"
	"github.com/paisans-software/paisans-stack/internal/registry"
)

// inspect reads the host's inventory. It is a variable so that this
// package's tests can hand in a fake host's Docker objects and manifest:
// hostcheck's own probes are tested in internal/hostcheck.
var inspect = func(t apply.Transport, cfg *config.Config) (*hostcheck.Inventory, error) {
	return hostcheck.Inspect(t, cfg.Deployment())
}

// hostPlan is what stage 3 does on the site's host, decided from what the
// probes read.
type hostPlan struct {
	dockerPresent bool
	containers    []string
	networks      []string
	volumes       []string
	// images are the IDs of the images only this deployment's containers
	// run, sorted.
	images    []string
	files     []appremove.File
	manifest  bool
	wireguard bool
	units     []string
	dropins   []string
	rules     []string
	keys      keyPlan
	registry  bool
	root      dirState
	// deleteRoot is --delete-data with no edited file under the root.
	deleteRoot bool
	handover   *handover
	// verified is set once the run has checked the host, before the keys.
	verified bool
}

// dirState is what appremove.DirCommand read of a directory.
type dirState struct {
	exists  bool
	bytes   int64
	files   int
	entries []string
}

// keyPlan is what happens to the authorized keys this deployment's record
// lists.
type keyPlan struct {
	user   string
	file   string
	record string
	// found is whether the record exists.
	found bool
	// lines are the key lines to delete from file.
	lines []string
}

// unitsCommand lists every unit and drop-in named for this deployment's
// token under /etc/systemd/system: what host prepare writes there is all
// named paisans-<token>-*. A glob that matches nothing is left as the
// pattern, so each match is printed only when it exists.
func (p *Plan) unitsCommand() string {
	prefix := p.dep().Prefix() + "-"
	return fmt.Sprintf(`for f in /etc/systemd/system/%[1]s*; do [ -e "$f" ] && echo "unit $f"; done; for f in /etc/systemd/system/*.d/%[1]s*; do [ -e "$f" ] && echo "dropin $f"; done; true`, prefix)
}

// removeUnitsCommand disables, stops and deletes each unit, deletes each
// drop-in and its directory once empty, and reloads systemd, reading the
// same globs again so a re-run finds nothing left.
func (p *Plan) removeUnitsCommand() string {
	prefix := p.dep().Prefix() + "-"
	return fmt.Sprintf(`for f in /etc/systemd/system/%[1]s*; do [ -e "$f" ] || continue; systemctl disable --now "$(basename "$f")" >/dev/null 2>&1 || true; rm -f -- "$f"; done; for f in /etc/systemd/system/*.d/%[1]s*; do [ -e "$f" ] || continue; rm -f -- "$f"; rmdir "$(dirname "$f")" 2>/dev/null || true; done; systemctl daemon-reload`, prefix)
}

// wireguardCommand stops this deployment's mesh interface and keeps it from
// starting at boot. The unit is wireguard-tools' template, named for the
// interface, so disabling the instance touches no other deployment's.
func (p *Plan) wireguardCommand() string {
	unit := p.dep().WireGuardUnit()
	return fmt.Sprintf(`systemctl disable --now %[1]s >/dev/null 2>&1 || true; if systemctl is-active --quiet %[1]s; then echo "%[1]s is still active"; exit 1; fi`, unit)
}

// containersCommand stops and removes every container and network carrying
// this deployment's label, whatever its project, selected by Docker's own
// label filter so that nothing without the label is ever selected.
func (p *Plan) containersCommand() string {
	filter := "--filter " + quote(p.dep().LabelFilter())
	rm := "docker rm"
	if p.DeleteData {
		rm += " -v"
	}
	return fmt.Sprintf(`set -e; ids=$(docker ps -aq --no-trunc %[1]s); if [ -n "$ids" ]; then docker stop $ids >/dev/null; %[2]s $ids >/dev/null; fi; nets=$(docker network ls -q --no-trunc %[1]s); if [ -n "$nets" ]; then docker network rm $nets >/dev/null; fi`, filter, rm)
}

// imagesCommand removes each image by ID, without -f, and answers per image,
// so an image Docker refuses (one a container still runs from) is reported
// rather than failing the stage.
func imagesCommand(ids []string) string {
	var q []string
	for _, id := range ids {
		q = append(q, quote(id))
	}
	return fmt.Sprintf(`for i in %s; do if out=$(docker image rm "$i" 2>&1); then echo "removed $i"; else echo "kept $i $(printf %%s "$out" | tr '\n' ' ')"; fi; done`, strings.Join(q, " "))
}

func (p *Plan) volumesCommand() string {
	return fmt.Sprintf(`set -e; vols=$(docker volume ls -q --filter %s); if [ -n "$vols" ]; then docker volume rm $vols >/dev/null; fi`, quote(p.dep().LabelFilter()))
}

// removeKeyLines drops exactly these lines from authorized_keys and nothing
// else, through a temporary file given the original's owner and mode and
// renamed over it, as host prepare removes a key. grep exits 1 when no line
// is left, which is an empty file rather than a failure.
func removeKeyLines(file string, lines []string) string {
	f := quote(file)
	tmp := quote(file + ".paisans-tmp")
	var patterns []string
	for _, l := range lines {
		patterns = append(patterns, "-e "+quote(l))
	}
	return fmt.Sprintf("set -e; [ -f %[1]s ] || exit 0; grep -vxF %[3]s %[1]s > %[2]s || [ $? -eq 1 ]; chown --reference=%[1]s %[2]s; chmod --reference=%[1]s %[2]s; mv %[2]s %[1]s",
		f, tmp, strings.Join(patterns, " "))
}

func parseDir(out string) (dirState, error) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "present" {
		return dirState{}, nil
	}
	if len(lines) < 3 {
		return dirState{}, fmt.Errorf("a cut short answer")
	}
	var d dirState
	var err error
	d.exists = true
	if d.bytes, err = strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64); err != nil {
		return dirState{}, err
	}
	if d.files, err = strconv.Atoi(strings.TrimSpace(lines[2])); err != nil {
		return dirState{}, err
	}
	for _, l := range lines[3:] {
		if l = strings.TrimSpace(l); l != "" {
			d.entries = append(d.entries, l)
		}
	}
	return d, nil
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// buildHost is stage 3. It probes the host and plans every removal, each
// proven this deployment's, and lists what it keeps.
func (p *Plan) buildHost() (*Stage, error) {
	st := &Stage{Number: 3, Name: "clean the host", Short: "nothing of this deployment is left"}
	if p.HostGone {
		st.Skipped = "--host-gone: " + p.Site + "'s host is not reached"
		p.Kept = append(p.Kept, p.hostGoneLeft())
		return st, nil
	}
	t := p.transports[p.Site]
	d := p.dep()
	hp := &hostPlan{}
	p.host = hp

	inv, err := inspect(t, p.cfg)
	if err != nil {
		return nil, fmt.Errorf("site remove %s: %w", p.Site, err)
	}
	if inv.Manifest && inv.ManifestFiles == nil {
		return nil, fmt.Errorf("site remove %s: %s is not readable as a manifest, so no file there is provably this deployment's. Nothing was changed", p.Site, inv.ManifestPath)
	}
	hp.dockerPresent = inv.Docker.Present
	hp.manifest = inv.Manifest
	for _, c := range inv.Containers {
		if c.Deployment == d.ID {
			hp.containers = append(hp.containers, c.Name)
		} else {
			p.Kept = append(p.Kept, containerKept(p.Site, c.Name))
		}
	}
	ours, theirs := map[string]bool{}, map[string]bool{}
	for _, c := range inv.Containers {
		if c.Image == "" {
			continue
		}
		if c.Deployment == d.ID {
			ours[c.Image] = true
		} else {
			theirs[c.Image] = true
		}
	}
	for id := range ours {
		if theirs[id] {
			p.Kept = append(p.Kept, imageKept(p.Site, id, "a container this deployment does not own runs from it"))
			continue
		}
		hp.images = append(hp.images, id)
	}
	sort.Strings(hp.images)
	for _, n := range inv.Networks {
		switch {
		case n.Deployment == d.ID:
			hp.networks = append(hp.networks, n.Name)
		case n.Name == "bridge" || n.Name == "host" || n.Name == "none":
		default:
			p.Kept = append(p.Kept, networkKept(p.Site, n.Name))
		}
	}
	for _, v := range inv.Volumes {
		if v.Deployment == d.ID && !v.Anonymous {
			hp.volumes = append(hp.volumes, v.Name)
			if !p.DeleteData {
				p.Kept = append(p.Kept, volumeKept(p.Site, v.Name))
			}
		}
	}

	edited := false
	for _, e := range inv.ManifestFiles {
		if strings.ContainsAny(e.Path, "'\n") {
			return nil, fmt.Errorf("site remove %s: the manifest names %q, which this command will not put in a command line", p.Site, e.Path)
		}
		f := appremove.File{Entry: e}
		content, found, err := t.ReadFile("/" + e.Path)
		if err != nil {
			return nil, fmt.Errorf("site remove %s: reading /%s: %w", p.Site, e.Path, err)
		}
		switch {
		case !found:
			f.State = appremove.Gone
		case sum(content) == e.SHA256:
			f.State = appremove.Remove
		default:
			f.State = appremove.Edited
			edited = true
			p.Kept = append(p.Kept, editedFileKept(p.Site, e.Path))
		}
		hp.files = append(hp.files, f)
	}
	sort.Slice(hp.files, func(i, j int) bool { return hp.files[i].Entry.Path < hp.files[j].Entry.Path })
	hp.wireguard = inv.ManifestWireGuard || contains(inv.Links, d.Interface())

	out, err := t.Run(p.unitsCommand())
	if err != nil {
		return nil, fmt.Errorf("site remove %s: listing its units: %w: %s", p.Site, err, lastLines(out, 2))
	}
	for _, line := range strings.Split(out, "\n") {
		kind, path, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch kind {
		case "unit":
			hp.units = append(hp.units, path)
		case "dropin":
			hp.dropins = append(hp.dropins, path)
		}
	}

	out, err = t.Run(hostprep.AddedRulesProbe)
	if err != nil {
		return nil, fmt.Errorf("site remove %s: reading ufw's rules: %w: %s", p.Site, err, lastLines(out, 2))
	}
	rules, _ := hostprep.ParseAddedRules(d, out)
	for _, r := range rules {
		switch {
		case r.Owned && r.SSH:
			p.Kept = append(p.Kept, sshAllowKept(p.Site, r.Line))
		case r.Owned:
			hp.rules = append(hp.rules, r.Line)
		default:
			p.Kept = append(p.Kept, ufwRuleKept(p.Site, r.Line))
		}
	}

	if err := p.probeKeys(t, hp); err != nil {
		return nil, err
	}

	reg, err := registry.Read(t)
	if err != nil {
		return nil, fmt.Errorf("site remove %s: %w", p.Site, err)
	}
	_, hp.registry = reg.Deployments[d.ID]

	out, err = t.Run(appremove.DirCommand(d.Root()))
	if err != nil {
		return nil, fmt.Errorf("site remove %s: reading %s: %w", p.Site, d.Root(), err)
	}
	if hp.root, err = parseDir(out); err != nil {
		return nil, fmt.Errorf("site remove %s: reading %s: %w", p.Site, d.Root(), err)
	}
	hp.deleteRoot = p.DeleteData && !edited
	switch {
	case !hp.root.exists:
	case p.DeleteData && edited:
		p.Kept = append(p.Kept, rootEditedKept(p.Site, d.Root()))
	case !p.DeleteData:
		rendered := 0
		for _, e := range inv.ManifestFiles {
			if strings.HasPrefix("/"+e.Path, d.Root()+"/") {
				rendered++
			}
		}
		if hp.manifest {
			rendered++
		}
		if data := hp.root.files - rendered; data > 0 {
			p.Kept = append(p.Kept, dataLeft(p.Site, d.Root(), data))
		}
	}

	report, err := ownership.Classify(p.cfg, p.Site, inv, inv.ManifestFiles)
	if err != nil {
		return nil, err
	}
	if p.cfg.Sites[p.Site].Has(config.RoleGateway) && report.Foreign() {
		if hp.handover, err = p.probeHandover(t, inv, report); err != nil {
			return nil, err
		}
	}

	p.hostSteps(st)
	st.Gate = "no container, network or unit of this deployment's on the host, no manifest, no ufw rule with its tag but the SSH allow, no registry entry, checked before the keys go; then the keys its record lists, unless another deployment's record lists them too or they are the user's last"
	if hp.handover != nil {
		st.Gate = "the handed over Caddy runs and validates; " + st.Gate
	}
	st.run = p.runHost
	// The run checks the host before the keys go and records that it did;
	// a run with nothing left to do checks it here.
	st.gate = func() error {
		if hp.verified {
			return nil
		}
		return p.verifyHost(t)
	}
	return st, nil
}

// probeKeys reads the user's authorized_keys, this deployment's record of the
// keys host prepare added, and every other deployment's record, and plans
// which lines go.
func (p *Plan) probeKeys(t apply.Transport, hp *hostPlan) error {
	d := p.dep()
	user := p.cfg.Sites[p.Site].SSH.User
	kp := keyPlan{user: user, record: hostprep.OwnedKeysPath(d, user)}
	content, found, err := t.ReadFile(kp.record)
	if err != nil {
		return fmt.Errorf("site remove %s: reading %s: %w", p.Site, kp.record, err)
	}
	kp.found = found
	if !found {
		hp.keys = kp
		return nil
	}
	ours := hostprep.ParseOwnedKeys(content)

	out, err := t.Run(fmt.Sprintf(`for f in %s; do [ -f "$f" ] && echo "$f"; done; true`, hostprep.OwnedKeysGlob(user)))
	if err != nil {
		return fmt.Errorf("site remove %s: listing key records: %w", p.Site, err)
	}
	others := map[string]bool{}
	for _, path := range strings.Fields(out) {
		if path == kp.record {
			continue
		}
		c, _, err := t.ReadFile(path)
		if err != nil {
			return fmt.Errorf("site remove %s: reading %s: %w", p.Site, path, err)
		}
		for _, fp := range hostprep.ParseOwnedKeys(c) {
			others[fp] = true
		}
	}

	entry, err := t.Run("getent passwd " + quote(user) + " || true")
	if err != nil {
		return fmt.Errorf("site remove %s: reading %s's passwd entry: %w", p.Site, user, err)
	}
	fields := strings.Split(strings.TrimSpace(entry), ":")
	if len(fields) < 7 || fields[5] == "" {
		p.Kept = append(p.Kept, keysNoPasswd(p.Site, kp.record, user))
		hp.keys = kp
		return nil
	}
	kp.file = strings.TrimSuffix(fields[5], "/") + "/.ssh/authorized_keys"
	keys, _, err := t.ReadFile(kp.file)
	if err != nil {
		return fmt.Errorf("site remove %s: reading %s: %w", p.Site, kp.file, err)
	}
	total := 0
	var candidates []string
	for _, raw := range strings.Split(keys, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, options, err := config.ParseKeyLine(trimmed)
		if err != nil {
			continue
		}
		total++
		if len(options) > 0 || !contains(ours, key.Fingerprint) {
			continue
		}
		if others[key.Fingerprint] {
			p.Kept = append(p.Kept, keySharedKept(p.Site, key.Fingerprint, kp.file))
			continue
		}
		candidates = append(candidates, raw)
	}
	if len(candidates) > 0 && len(candidates) == total {
		p.Kept = append(p.Kept, keyLinesKept(p.Site, kp.file, kp.record, user))
		candidates = nil
	}
	kp.lines = candidates
	hp.keys = kp
	return nil
}

func (p *Plan) hostSteps(st *Stage) {
	hp := p.host
	d := p.dep()
	add := func(verb, title, format string, args ...any) {
		st.Steps = append(st.Steps, Step{Site: p.Site, Verb: verb, Title: title, Text: fmt.Sprintf(format, args...)})
	}
	if h := hp.handover; h != nil && !h.done {
		add("hand over", "hand over Caddy", "Caddy to the host's owner in %s, for what relies on it (%s): write compose.yaml (project caddy, no deployment label, %s, host networking) and a Caddyfile importing %s, validate it in a one-off container, stop this deployment's Caddy, move its data and config directories there so certificates survive, start it, and record %s", handoverDir, strings.Join(h.users, ", "), h.image, "/etc/caddy.d/*.caddy", markerPath)
		if h.env {
			add("copy", "hand over Caddy", "the DNS provider token into %s/caddy.env, on the host: the owner's certificates were issued through DNS-01. It is a credential left on the host for the owner", handoverDir)
		}
	}
	if len(hp.containers) > 0 || len(hp.networks) > 0 {
		add("remove", "remove containers and networks", "every container and network with this deployment's label: %s", strings.Join(append(append([]string{}, hp.containers...), hp.networks...), ", "))
	}
	if p.DeleteData && len(hp.volumes) > 0 {
		add("delete", "delete volumes", "volumes %s", strings.Join(hp.volumes, ", "))
	}
	if len(hp.images) > 0 {
		add("remove", "remove images", "images %s, which only this deployment's containers ran, by ID and without -f, so Docker refuses one in use", strings.Join(hp.images, ", "))
	}
	if hp.wireguard {
		add("stop", "stop "+d.WireGuardUnit(), "%s, and disable it at boot", d.WireGuardUnit())
	}
	for _, f := range hp.files {
		switch f.State {
		case appremove.Remove:
			add("delete", "delete files", "/%s", f.Entry.Path)
		case appremove.Edited:
			add("keep", "keep edited files", "/%s: edited on the host since apply wrote it", f.Entry.Path)
		}
	}
	if hp.manifest {
		add("delete", "delete the manifest", "%s, the manifest", d.Manifest())
	}
	for _, u := range hp.units {
		add("remove", "remove units", "unit %s, disabled and stopped first", u)
	}
	for _, u := range hp.dropins {
		add("remove", "remove units", "drop-in %s", u)
	}
	for _, r := range hp.rules {
		add("delete", "delete ufw rules", "ufw rule `%s`", r)
	}
	switch {
	case hp.root.exists && hp.deleteRoot:
		add("delete", "delete "+d.Root(), "%s and everything in it: %d file(s), %d bytes", d.Root(), hp.root.files, hp.root.bytes)
	case hp.root.exists:
		add("remove", "remove empty directories", "the empty directories under %s", d.Root())
	}
	if hp.registry {
		add("remove", "remove registry entry", "deployment %s from %s", d.ID, registry.Path)
	}
	if len(hp.keys.lines) > 0 {
		add("remove", "remove SSH keys", "%d key line(s) from %s that %s lists, last, since they may be what this command reaches the host with", len(hp.keys.lines), hp.keys.file, hp.keys.record)
	}
	if hp.keys.found {
		add("delete", "remove SSH keys", "%s", hp.keys.record)
	}
}

// runHost cleans the host, in the order the plan lists, each step reading
// live state or keyed by the label, so a re-run does only what is left.
func (p *Plan) runHost() error {
	hp := p.host
	t := p.transports[p.Site]
	d := p.dep()
	run := func(what, command string) error {
		if out, err := t.Run(command); err != nil {
			return fmt.Errorf("%s: %s: %w: %s", p.Site, what, err, lastLines(out, 3))
		}
		return nil
	}
	if h := hp.handover; h != nil && !h.done {
		if err := p.handOver(t, h); err != nil {
			return err
		}
	}
	if hp.dockerPresent {
		p.work("remove containers and networks")
		if err := run("removing this deployment's containers and networks", p.containersCommand()); err != nil {
			return err
		}
		if p.DeleteData {
			p.work("delete volumes")
			if err := run("removing this deployment's volumes", p.volumesCommand()); err != nil {
				return err
			}
		}
		if len(hp.images) > 0 {
			p.work("remove images")
			out, err := t.Run(imagesCommand(hp.images))
			if err != nil {
				return fmt.Errorf("%s: removing images: %w: %s", p.Site, err, lastLines(out, 3))
			}
			for _, line := range strings.Split(out, "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "kept "); ok {
					id, why, _ := strings.Cut(rest, " ")
					p.Kept = append(p.Kept, imageKept(p.Site, id, "Docker refused: "+why))
				}
			}
		}
	}
	if hp.wireguard {
		p.work("stop " + d.WireGuardUnit())
		if err := run("stopping "+d.WireGuardUnit(), p.wireguardCommand()); err != nil {
			return err
		}
	}
	if len(hp.files) > 0 {
		p.work("delete files")
		out, err := t.Run(appremove.FilesCommand(hp.files))
		if err != nil {
			return fmt.Errorf("%s: removing files: %w: %s", p.Site, err, lastLines(out, 3))
		}
		answered := map[string]bool{}
		for _, line := range strings.Split(out, "\n") {
			if _, path, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
				answered[strings.TrimPrefix(path, "/")] = true
			}
		}
		for _, f := range hp.files {
			if !answered[f.Entry.Path] {
				return fmt.Errorf("%s: removing files: no answer for /%s, so the manifest is kept", p.Site, f.Entry.Path)
			}
		}
	}
	if hp.manifest {
		p.work("delete the manifest")
		if err := run("deleting the manifest", "rm -f -- "+quote(d.Manifest())); err != nil {
			return err
		}
	}
	if len(hp.units) > 0 || len(hp.dropins) > 0 {
		p.work("remove units")
		if err := run("removing units", p.removeUnitsCommand()); err != nil {
			return err
		}
	}
	if len(hp.rules) > 0 {
		p.work("delete ufw rules")
		out, err := t.Run(hostprep.AddedRulesProbe)
		if err != nil {
			return fmt.Errorf("%s: reading ufw's rules: %w: %s", p.Site, err, lastLines(out, 2))
		}
		rules, _ := hostprep.ParseAddedRules(d, out)
		for _, r := range rules {
			if r.Owned && !r.SSH {
				if err := run("deleting a ufw rule", "ufw delete "+r.Line); err != nil {
					return err
				}
			}
		}
	}
	if hp.root.exists {
		if hp.deleteRoot {
			p.work("delete " + d.Root())
		} else {
			p.work("remove empty directories")
		}
		if err := run("removing "+d.Root(), appremove.DirCommandFor(d.Root(), hp.deleteRoot)); err != nil {
			return err
		}
	}
	if hp.registry {
		p.work("remove registry entry")
		if err := registry.Unclaim(t, d.ID); err != nil {
			return err
		}
	}
	p.work("check the host before the keys go")
	if err := p.verifyHost(t); err != nil {
		return fmt.Errorf("%s: before the keys: %w", p.Site, err)
	}
	hp.verified = true
	if len(hp.keys.lines) > 0 || hp.keys.found {
		p.work("remove SSH keys")
	}
	if len(hp.keys.lines) > 0 {
		if err := run("removing keys from "+hp.keys.file, removeKeyLines(hp.keys.file, hp.keys.lines)); err != nil {
			return err
		}
	}
	if hp.keys.found {
		if err := run("deleting "+hp.keys.record, "rm -f -- "+quote(hp.keys.record)); err != nil {
			return err
		}
	}
	return nil
}

// verifyHost is the host stage's gate, checked before the keys go, since
// once they have the host may not answer again.
func (p *Plan) verifyHost(t apply.Transport) error {
	d := p.dep()
	inv, err := inspect(t, p.cfg)
	if err != nil {
		return err
	}
	var left []string
	for _, c := range inv.Containers {
		if c.Deployment == d.ID {
			left = append(left, "container "+c.Name)
		}
	}
	for _, n := range inv.Networks {
		if n.Deployment == d.ID {
			left = append(left, "network "+n.Name)
		}
	}
	if p.DeleteData {
		for _, v := range inv.Volumes {
			if v.Deployment == d.ID && !v.Anonymous {
				left = append(left, "volume "+v.Name)
			}
		}
	}
	if inv.Manifest {
		left = append(left, d.Manifest())
	}
	out, err := t.Run(p.unitsCommand())
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(out); s != "" {
		left = append(left, strings.Split(s, "\n")...)
	}
	out, err = t.Run(hostprep.AddedRulesProbe)
	if err != nil {
		return err
	}
	rules, _ := hostprep.ParseAddedRules(d, out)
	for _, r := range rules {
		if r.Owned && !r.SSH {
			left = append(left, "ufw rule "+r.Line)
		}
	}
	reg, err := registry.Read(t)
	if err != nil {
		return err
	}
	if _, ok := reg.Deployments[d.ID]; ok {
		left = append(left, "its registry entry")
	}
	if h := p.host.handover; h != nil {
		if err := p.handoverRunning(t); err != nil {
			left = append(left, "a running handed over Caddy: "+err.Error())
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("still on the host: %s", strings.Join(left, "; "))
	}
	return nil
}

// hostGoneLeft is what a host left alone with --host-gone still holds, for
// the report.
func (p *Plan) hostGoneLeft() string {
	d := p.dep()
	return fmt.Sprintf("%s: host not reached (--host-gone); clean it by hand, or reinstall it. If it ever comes back it still holds this deployment's containers and networks (label %s), %s and its manifest, %s and /%s, units and drop-ins named %s-* under /etc/systemd/system, ufw rules commented %s:, the keys %s lists and that record, and its entry in %s. Nothing it runs can reach the cluster: no remaining site has it as a WireGuard peer, and its etcd member is gone",
		p.Site, d.LabelFilter(), d.Root(), d.WireGuardUnit(), d.WireGuardConf(), d.Prefix(), d.Prefix(), hostprep.OwnedKeysPath(d, p.cfg.Sites[p.Site].SSH.User), registry.Path)
}

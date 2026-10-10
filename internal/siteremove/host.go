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
	"github.com/paisans-software/paisans-stack/internal/deployrecord"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/hostprep"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/render"
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
	// keyRecords are this deployment's records of the authorized keys
	// host prepare added, one per login user.
	keyRecords []string
	registry   bool
	root       dirState
	// record is whether the host holds this deployment's record, a gateway's.
	record bool
	// deleteRoot is --delete-data with no edited file under the root.
	deleteRoot bool
	// caddy is the gateway's Caddy kept for the host owner's sites, nil
	// when it goes like everything else.
	caddy *caddyPlan
	// verified is set once the run has checked the host.
	verified bool
}

// dirState is what appremove.DirCommand read of a directory.
type dirState struct {
	exists  bool
	bytes   int64
	files   int
	entries []string
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
// label filter so that nothing without the label is ever selected. A kept
// Caddy, by its full ID, is left out.
func (p *Plan) containersCommand() string {
	d := p.dep()
	filter := "--filter " + quote(d.LabelFilter())
	rm := "docker rm"
	if p.DeleteData {
		rm += " -v"
	}
	guard, keep := "", ""
	switch cp := p.host.caddy; {
	case cp != nil:
		// A separate step, so a docker ps that fails still stops the command.
		keep = `ids=$(printf '%s\n' $ids | grep -vxF ` + quote(cp.container.ID) + ` || true); `
	case p.cfg.Sites[p.Site].Has(config.RoleGateway):
		// The plan removes the Caddy. A site of the host owner's that
		// appeared since would lose it, so the run stops before anything.
		guard = fmt.Sprintf(`if { { [ -d %[1]s ] && ! { [ -r %[1]s ] && [ -x %[1]s ]; }; } || ls -d %[1]s/*.caddy >/dev/null 2>&1; } && [ -n "$(docker ps -aq %[2]s --filter %[3]s --filter %[4]s)" ]; then echo "a site of the host owner's is in %[1]s now, served by this deployment's Caddy, which this plan removes. Nothing was removed; run again, and the Caddy is kept for it"; exit 3; fi; `,
			render.HostSitesDir, filter, quote("label=com.docker.compose.project="+d.Project("infra")), quote("label=com.docker.compose.service=caddy"))
	}
	return fmt.Sprintf(`set -e; %[4]sids=$(docker ps -aq --no-trunc %[1]s); %[3]sif [ -n "$ids" ]; then docker stop $ids >/dev/null; %[2]s $ids >/dev/null; fi; nets=$(docker network ls -q --no-trunc %[1]s); if [ -n "$nets" ]; then docker network rm $nets >/dev/null; fi`, filter, rm, keep, guard)
}

// planImages finds the images only this deployment ran: those its containers
// run from, and those its compose files name, which the manifest proves are
// its own. The files find them even after a run stopped between removing the
// containers and removing their images. An image any other container uses,
// running or stopped, whoever's it is, is kept.
func (p *Plan) planImages(t apply.Transport, hp *hostPlan, inv *hostcheck.Inventory, refs []string) error {
	if !hp.dockerPresent {
		return nil
	}
	d := p.dep()
	ours, theirs := map[string]bool{}, map[string]bool{}
	running := map[string]bool{}
	for _, c := range inv.Containers {
		if c.Image == "" {
			continue
		}
		switch {
		case hp.caddy != nil && c.ID == hp.caddy.container.ID:
			running[c.Image] = true
		case c.Deployment == d.ID:
			ours[c.Image] = true
		default:
			theirs[c.Image] = true
		}
	}
	ids, err := apply.ProbeImages(dedupe(refs), t)
	if err != nil {
		return fmt.Errorf("site remove %s: %w", p.Site, err)
	}
	for _, id := range ids {
		if id != "" {
			ours[id] = true
		}
	}
	for id := range ours {
		if running[id] {
			// The kept Caddy runs from it.
			continue
		}
		if theirs[id] {
			p.Kept = append(p.Kept, imageKept(p.Site, id, "a container this deployment does not own runs from it"))
			continue
		}
		hp.images = append(hp.images, id)
	}
	sort.Strings(hp.images)
	return nil
}

func dedupe(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
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
	// Before anything else is planned: a kept Caddy changes what goes.
	if hp.caddy, err = p.planCaddy(t, inv); err != nil {
		return nil, err
	}
	cp := hp.caddy
	for _, c := range inv.Containers {
		if cp != nil && c.ID == cp.container.ID {
			continue
		}
		if c.Deployment == d.ID {
			hp.containers = append(hp.containers, c.Name)
		} else {
			p.Kept = append(p.Kept, containerKept(p.Site, c.Name))
		}
	}
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
	var refs []string
	proven := map[string]bool{}
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
			proven[e.Path] = true
			if strings.HasSuffix(e.Path, "/compose.yaml") {
				named, err := apply.ComposeImages(content)
				if err != nil {
					return nil, fmt.Errorf("site remove %s: reading the images /%s names: %w", p.Site, e.Path, err)
				}
				refs = append(refs, named...)
			}
		default:
			f.State = appremove.Edited
			if cp == nil || !p.keptFile(e.Path) {
				edited = true
				p.Kept = append(p.Kept, editedFileKept(p.Site, e.Path))
			}
		}
		if cp != nil && p.keptFile(e.Path) {
			continue
		}
		hp.files = append(hp.files, f)
	}
	if cp != nil {
		// The manifest stays, listing the kept files, which proves them
		// this deployment's to the run that removes them.
		hp.manifest = false
		want, err := p.keptManifest(inv.ManifestFiles, proven, cp.reduced)
		if err != nil {
			return nil, err
		}
		have, _, err := t.ReadFile(d.Manifest())
		if err != nil {
			return nil, fmt.Errorf("site remove %s: reading %s: %w", p.Site, d.Manifest(), err)
		}
		if have != want {
			cp.manifest = want
		}
	}
	sort.Slice(hp.files, func(i, j int) bool { return hp.files[i].Entry.Path < hp.files[j].Entry.Path })
	if err := p.planImages(t, hp, inv, refs); err != nil {
		return nil, err
	}
	hp.wireguard = inv.ManifestWireGuard || contains(inv.Links, d.Interface())
	if _, hp.record, err = t.ReadFile(deployrecord.Path(d)); err != nil {
		return nil, fmt.Errorf("site remove %s: reading %s: %w", p.Site, deployrecord.Path(d), err)
	}

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
		case r.Owned && r.Web && cp != nil:
			// The kept Caddy's way in: ufw denies incoming by default.
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
	entry, registered := reg.Deployments[d.ID]
	hp.registry = registered
	if cp != nil {
		// The entry stays, marked kept: the Caddy still holds the gateway
		// role and the root.
		hp.registry = false
		cp.entry, cp.mark = entry, entry.Kept != registry.KeptCaddy
		if !registered {
			// A host whose entry is gone gets one: the kept Caddy holds the
			// gateway role and the root.
			_, cp.entry = registry.For(p.cfg, p.Site, now())
		}
		err = p.planKeptRoot(t, hp, edited)
	} else {
		err = p.planRoot(t, hp, inv, edited)
	}
	if err != nil {
		return nil, err
	}

	p.hostSteps(st)
	st.Gate = "no container, network or unit of this deployment's on the host, no manifest, no ufw rule with its tag but the SSH allow, no registry entry"
	if hp.caddy != nil {
		st.Gate = "no container, network or unit of this deployment's on the host but its Caddy, whose Caddyfile is reduced to the host owner's sites, no ufw rule with its tag but the SSH allow, and its registry entry marked kept"
	}
	st.run = p.runHost
	// The run checks the host last and records that it did; a run with
	// nothing left to do checks it here.
	st.gate = func() error {
		if hp.verified {
			return nil
		}
		return p.verifyHost(t)
	}
	return st, nil
}

// planRoot plans the deployment's directory: its empty directories, or with
// --delete-data all of it unless a file in it was edited.
func (p *Plan) planRoot(t apply.Transport, hp *hostPlan, inv *hostcheck.Inventory, edited bool) error {
	d := p.dep()
	out, err := t.Run(appremove.DirCommand(d.Root()))
	if err != nil {
		return fmt.Errorf("site remove %s: reading %s: %w", p.Site, d.Root(), err)
	}
	if hp.root, err = parseDir(out); err != nil {
		return fmt.Errorf("site remove %s: reading %s: %w", p.Site, d.Root(), err)
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
	return nil
}

// planKeptRoot plans the deployment's directory around a kept Caddy: with
// --delete-data everything but Caddy's paths, unless a file was edited;
// without it, the empty directories once the files are gone.
func (p *Plan) planKeptRoot(t apply.Transport, hp *hostPlan, edited bool) error {
	d, cp := p.dep(), hp.caddy
	out, err := t.Run(p.keptProbe())
	if err != nil {
		return fmt.Errorf("site remove %s: reading %s: %w: %s", p.Site, d.Root(), err, lastLines(out, 2))
	}
	others, empty, files := p.parseKeptProbe(out)
	cp.others, cp.empty = others, empty
	hp.deleteRoot = p.DeleteData && !edited
	switch {
	case p.DeleteData && edited:
		p.Kept = append(p.Kept, rootEditedKept(p.Site, d.Root()))
	case !p.DeleteData:
		removed := map[string]bool{}
		for _, f := range hp.files {
			if f.State == appremove.Remove {
				removed["/"+f.Entry.Path] = true
			}
		}
		for _, f := range files {
			if !removed[f] {
				cp.dataFiles++
			}
		}
		if cp.dataFiles > 0 {
			p.Kept = append(p.Kept, dataLeft(p.Site, d.Root(), cp.dataFiles))
		}
	}
	return nil
}

// probeKeys finds this deployment's records of the authorized keys host
// prepare added. The keys themselves are never removed: they may be the only
// way into the host, and nothing here can tell whether another exists. The
// records are this deployment's own files, so they go with it.
func (p *Plan) probeKeys(t apply.Transport, hp *hostPlan) error {
	out, err := t.Run(fmt.Sprintf(`for f in %s; do [ -f "$f" ] && echo "$f"; done; true`, hostprep.OwnedKeysPrefixGlob(p.dep())))
	if err != nil {
		return fmt.Errorf("site remove %s: listing this deployment's key records: %w", p.Site, err)
	}
	hp.keyRecords = strings.Fields(out)
	return nil
}

func (p *Plan) hostSteps(st *Stage) {
	hp := p.host
	d := p.dep()
	add := func(verb, title, format string, args ...any) {
		st.Steps = append(st.Steps, Step{Site: p.Site, Verb: verb, Title: title, Text: fmt.Sprintf(format, args...)})
	}
	cp := hp.caddy
	if cp != nil && cp.reduce() {
		add("reduce", "reduce Caddy to the host's sites", "Caddy %s, kept because it serves the host owner's sites (%s): its Caddyfile reduced to the global options, the snippets they may import and `import %s/*.caddy`, staged where the container sees it, validated in it and loaded with caddy reload, then written over %s in place; a failure puts the original back and removes nothing else. Its compose file, Caddyfile, caddy.env, data and config stay, kept with --delete-data too", cp.container.Name, strings.Join(cp.sites, ", "), render.HostSitesMount, p.caddyfile())
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
	if cp != nil && cp.manifest != "" {
		add("rewrite", "rewrite the manifest", "%s, to list only the kept Caddy's files, which proves them this deployment's to the run that removes them", d.Manifest())
	}
	if hp.record {
		add("delete", "delete the deployment record", "%s, this deployment's record of what it deployed", deployrecord.Path(d))
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
	case cp != nil && hp.deleteRoot && len(cp.others) > 0:
		add("delete", "delete "+d.Root()+" but Caddy's files", "everything in %s but the kept Caddy's files, which are kept with --delete-data: %s", d.Root(), strings.Join(cp.others, ", "))
	case cp != nil && !p.DeleteData && (len(cp.empty) > 0 || len(hp.files) > 0):
		add("remove", "remove empty directories", "the empty directories under %s, but never one the kept Caddy mounts", d.Root())
	case cp != nil:
	case hp.root.exists && hp.deleteRoot:
		add("delete", "delete "+d.Root(), "%s and everything in it: %d file(s), %d bytes", d.Root(), hp.root.files, hp.root.bytes)
	case hp.root.exists:
		add("remove", "remove empty directories", "the empty directories under %s", d.Root())
	}
	if hp.registry {
		add("remove", "remove registry entry", "deployment %s from %s", d.ID, registry.Path)
	}
	if cp != nil && cp.mark {
		add("mark", "mark the registry entry kept", "deployment %s in %s, as %q: the kept Caddy still holds the gateway role and %s", d.ID, registry.Path, "kept: "+registry.KeptCaddy, d.Root())
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
	// The Caddy first: a failure there leaves the deployment as it was.
	cp := hp.caddy
	if cp != nil && cp.reduce() {
		if err := p.reduceCaddy(t, cp); err != nil {
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
	if cp != nil && cp.manifest != "" {
		p.work("rewrite the manifest")
		if err := t.WriteFile(d.Manifest(), cp.manifest, 0o600); err != nil {
			return err
		}
	}
	if hp.manifest {
		p.work("delete the manifest")
		if err := run("deleting the manifest", "rm -f -- "+quote(d.Manifest())); err != nil {
			return err
		}
	}
	if hp.record {
		p.work("delete the deployment record")
		if err := run("deleting the deployment record", deployrecord.RemoveCommand(d)); err != nil {
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
			if r.Owned && !r.SSH && !(r.Web && cp != nil) {
				if err := run("deleting a ufw rule", "ufw delete "+r.Line); err != nil {
					return err
				}
			}
		}
	}
	switch {
	case cp != nil && hp.deleteRoot && len(cp.others) > 0:
		p.work("delete " + d.Root() + " but Caddy's files")
		if err := run("removing "+d.Root()+" but Caddy's files", p.deleteBesideKept()); err != nil {
			return err
		}
	case cp != nil && !p.DeleteData && (len(cp.empty) > 0 || len(hp.files) > 0):
		p.work("remove empty directories")
		if err := run("removing the empty directories under "+d.Root(), p.emptyBesideKept()); err != nil {
			return err
		}
	case cp != nil:
	case hp.root.exists:
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
	if cp != nil && cp.mark {
		p.work("mark the registry entry kept")
		if err := registry.Keep(t, d.ID, cp.entry); err != nil {
			return err
		}
	}
	if len(hp.keyRecords) > 0 {
		q := make([]string, len(hp.keyRecords))
		for i, r := range hp.keyRecords {
			q[i] = quote(r)
		}
		if err := run("deleting the key records", "rm -f -- "+strings.Join(q, " ")); err != nil {
			return err
		}
	}
	p.work("check the host")
	if err := p.verifyHost(t); err != nil {
		return fmt.Errorf("%s: %w", p.Site, err)
	}
	hp.verified = true
	return nil
}

// verifyHost is the host stage's gate: nothing of this deployment's is left
// on the host.
func (p *Plan) verifyHost(t apply.Transport) error {
	d := p.dep()
	inv, err := inspect(t, p.cfg)
	if err != nil {
		return err
	}
	cp := p.host.caddy
	var left []string
	for _, c := range inv.Containers {
		if c.Deployment == d.ID && (cp == nil || c.ID != cp.container.ID) {
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
	if inv.Manifest && cp == nil {
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
		if r.Owned && !r.SSH && !(r.Web && cp != nil) {
			left = append(left, "ufw rule "+r.Line)
		}
	}
	reg, err := registry.Read(t)
	if err != nil {
		return err
	}
	e, ok := reg.Deployments[d.ID]
	switch {
	case cp == nil && ok:
		left = append(left, "its registry entry")
	case cp != nil && (!ok || e.Kept != registry.KeptCaddy):
		left = append(left, "its registry entry, marked kept")
	}
	if cp != nil {
		left = append(left, p.verifyCaddy(t, cp)...)
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

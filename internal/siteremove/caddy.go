package siteremove

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/ownership"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// What follows keeps a gateway's Caddy on the host while it serves the host
// owner's sites in render.HostSitesDir, reduced to those sites, and removes
// everything else of the deployment's around it
// (docs/specs/2026-10-10-gateway-caddy-kept.md).

// caddySnippets are the snippets the deployment's Caddyfile defines that a
// file in render.HostSitesDir may import, kept in the reduced Caddyfile so
// those files still validate.
var caddySnippets = []string{"upstream_unavailable", "upstream_failover", "upstream_single"}

// stagedName is the reduced Caddyfile's name while it is validated and
// loaded, in the snippets directory, which the container mounts. No
// Caddyfile imports it: snippets are imported by name.
const stagedName = ".paisans-kept-Caddyfile"

// containerConfig is where the container reads its Caddyfile, and
// containerSnippets where it sees the snippets directory.
const (
	containerConfig   = "/etc/caddy/Caddyfile"
	containerSnippets = "/etc/caddy/snippets"
)

// caddyPlan is what keeping the Caddy does.
type caddyPlan struct {
	// container is the kept Caddy's container: its full ID, name and image.
	container hostcheck.Container
	// sites are the host owner's files the Caddy serves.
	sites []string
	// original is the Caddyfile on the host, reduced what it becomes.
	original, reduced string
	// manifest is the manifest listing only the kept files, empty when the
	// host's already is it.
	manifest string
	// entry is the registry entry, and mark whether it is still to be
	// marked kept.
	entry registry.Entry
	mark  bool
	// others are the paths under the root that are not Caddy's, and empty
	// the empty directories among them, from keptProbe.
	others, empty []string
	// dataFiles counts the files among others no step deletes without
	// --delete-data.
	dataFiles int
}

// reduce reports whether the Caddyfile is still to be reduced.
func (c *caddyPlan) reduce() bool { return c.original != c.reduced }

func (p *Plan) caddyfile() string { return p.dep().Path("infra", "caddy", "Caddyfile") }

func (p *Plan) staged() string { return p.dep().Path("infra", "caddy", "snippets", stagedName) }

// keptPaths are the paths under the root the kept Caddy needs, with
// everything under each: its compose file, Caddyfile, DNS token, data and
// config, the snippets directory it mounts, and the manifest.
func (p *Plan) keptPaths() []string {
	d := p.dep()
	return []string{
		d.Manifest(),
		d.Compose("infra"),
		p.caddyfile(),
		d.Path("infra", "caddy", "caddy.env"),
		d.Path("infra", "caddy", "data"),
		d.Path("infra", "caddy", "config"),
		d.Path("infra", "caddy", "snippets"),
	}
}

// keptFile reports whether a manifest entry is one of the kept files.
func (p *Plan) keptFile(rel string) bool {
	d := p.dep()
	switch "/" + rel {
	case d.Compose("infra"), p.caddyfile(), d.Path("infra", "caddy", "caddy.env"):
		return true
	}
	return false
}

// keptCaddy is this deployment's Caddy container, if the host has it: the
// one with this id's label in the infrastructure project, service caddy.
func (p *Plan) keptCaddy(inv *hostcheck.Inventory) (hostcheck.Container, bool) {
	d := p.dep()
	for _, c := range inv.Containers {
		if c.Deployment == d.ID && c.Project == d.Project("infra") && c.Service == "caddy" && c.ID != "" {
			return c, true
		}
	}
	return hostcheck.Container{}, false
}

// planCaddy decides whether the Caddy is kept: the site is a gateway, the
// host owner has a site file in render.HostSitesDir, which is what
// internal/ownership counts as relying on the gateway's Caddy, and this
// deployment's Caddy container is there to serve it. It returns nil when
// the Caddy goes like everything else.
func (p *Plan) planCaddy(t apply.Transport, inv *hostcheck.Inventory) (*caddyPlan, error) {
	if !p.cfg.Sites[p.Site].Has(config.RoleGateway) {
		return nil, nil
	}
	report := ownership.Report{Site: p.Site, HostSites: inv.HostSites}
	if !report.Foreign() {
		return nil, nil
	}
	c, ok := p.keptCaddy(inv)
	if !ok {
		return nil, nil
	}
	cp := &caddyPlan{container: c, sites: append([]string(nil), inv.HostSites...)}
	content, found, err := t.ReadFile(p.caddyfile())
	if err != nil {
		return nil, fmt.Errorf("site remove %s: reading %s: %w", p.Site, p.caddyfile(), err)
	}
	if !found {
		return nil, fmt.Errorf("site remove %s: its Caddy serves the host owner's sites in %s, and %s is not there to reduce to them. Nothing was changed", p.Site, render.HostSitesDir, p.caddyfile())
	}
	cp.original = content
	if cp.reduced, err = reduceCaddyfile(content, p.dep().ID); err != nil {
		return nil, fmt.Errorf("site remove %s: its Caddy serves the host owner's sites in %s, and %s cannot be reduced to them: %v. Nothing was changed", p.Site, render.HostSitesDir, p.caddyfile(), err)
	}
	if cp.reduce() && c.PID == 0 {
		return nil, fmt.Errorf("site remove %s: its Caddy, %s, serves the host owner's sites in %s and is not running, so its reduced configuration cannot be validated and loaded in it. Nothing was changed. Start it (docker start %s) and run again", p.Site, c.Name, render.HostSitesDir, c.Name)
	}
	p.CaddyKept = cp.sites
	p.Kept = append(p.Kept, caddyKept(p.Site, c.Name, cp.sites, p.dep().Dir("infra")))
	return cp, nil
}

// reduceCaddyfile is the Caddyfile for the host owner's sites alone: the
// global options block, which holds the ACME email and DNS provider, the
// snippets their files may import, and the import of their directory. No
// site block of the deployment's is in it. Reducing its result gives the
// same file.
func reduceCaddyfile(content, id string) (string, error) {
	global, err := globalBlock(content)
	if err != nil {
		return "", err
	}
	snippets, err := snippetBlocks(content, caddySnippets)
	if err != nil {
		return "", err
	}
	imp := "import " + render.HostSitesMount + "/*.caddy"
	if !strings.Contains("\n"+content+"\n", "\n"+imp+"\n") {
		return "", fmt.Errorf("it has no line %q", imp)
	}
	header := fmt.Sprintf(`# Reduced by paisans site remove when deployment %s left this host. This
# Caddy is kept for the host owner's sites in %s alone, and a later
# paisans site remove --force removes it once that directory holds none.
`, id, render.HostSitesDir)
	return header + global + "\n\n" + snippets + "\n" + imp + "\n", nil
}

// globalBlock copies the global options block out of a Caddyfile: from its
// `{` line to the `}` that closes it at the start of a line.
func globalBlock(caddyfile string) (string, error) {
	lines := strings.Split(caddyfile, "\n")
	for i, l := range lines {
		if l != "{" {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == "}" {
				return strings.Join(lines[i:j+1], "\n"), nil
			}
		}
		return "", fmt.Errorf("its global options block has no closing brace")
	}
	return "", fmt.Errorf("it has no global options block")
}

// snippetBlocks copies the named snippets out of a Caddyfile: each from its
// `(name) {` line to the `}` that closes it at the start of a line.
func snippetBlocks(caddyfile string, names []string) (string, error) {
	lines := strings.Split(caddyfile, "\n")
	var out []string
	for _, name := range names {
		start := -1
		for i, l := range lines {
			if l == "("+name+") {" {
				start = i
			}
		}
		if start < 0 {
			return "", fmt.Errorf("it defines no snippet %s", name)
		}
		end := -1
		for i := start + 1; i < len(lines); i++ {
			if lines[i] == "}" {
				end = i
				break
			}
		}
		if end < 0 {
			return "", fmt.Errorf("its snippet %s has no closing brace", name)
		}
		out = append(out, strings.Join(lines[start:end+1], "\n"))
	}
	return strings.Join(out, "\n\n") + "\n", nil
}

// keptManifest is the manifest of the kept files: each that the manifest
// proved, as it was, and the Caddyfile with the reduced file's hash. They
// are proven this deployment's when a later run removes them.
func (p *Plan) keptManifest(entries []render.ManifestFile, proven map[string]bool, reduced string) (string, error) {
	var files []render.ManifestFile
	for _, e := range entries {
		switch {
		case "/"+e.Path == p.caddyfile():
			e.SHA256 = sum(reduced)
			files = append(files, e)
		case p.keptFile(e.Path) && proven[e.Path]:
			files = append(files, e)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	data, err := json.MarshalIndent(render.Manifest{Version: 1, Files: files}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

// keptProbe lists every path under the root that no kept path covers, each
// as find's type letter and the path, an empty directory as e.
func (p *Plan) keptProbe() string {
	var prune []string
	for _, k := range p.keptPaths() {
		prune = append(prune, "-path "+quote(k))
	}
	return fmt.Sprintf(`d=%s; if [ -d "$d" ]; then find "$d" -mindepth 1 \( %s \) -prune -o \( -type d -empty -printf 'e %%p\n' \) -o -printf '%%y %%p\n'; fi`, quote(p.dep().Root()), strings.Join(prune, " -o "))
}

// parseKeptProbe reads keptProbe's answer: the paths that are not Caddy's,
// leaving out the directories above a kept path, and the empty directories
// among them.
func (p *Plan) parseKeptProbe(out string) (others, empty []string, files []string) {
	kept := p.keptPaths()
	for _, line := range strings.Split(out, "\n") {
		kind, path, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		above := false
		for _, k := range kept {
			above = above || strings.HasPrefix(k, path+"/")
		}
		if above {
			continue
		}
		others = append(others, path)
		switch kind {
		case "e":
			empty = append(empty, path)
		case "f", "l":
			files = append(files, path)
		}
	}
	return others, empty, files
}

// deleteBesideKept deletes everything under the root but the kept paths:
// in the root and each directory above a kept path, every entry that is
// neither kept nor above one, and everything in the snippets directory,
// whose files were the deployment's routes.
func (p *Plan) deleteBesideKept() string {
	root := p.dep().Root()
	keep := map[string]map[string]bool{root: {}}
	for _, k := range p.keptPaths() {
		for child := k; child != root; child = path.Dir(child) {
			parent := path.Dir(child)
			if keep[parent] == nil {
				keep[parent] = map[string]bool{}
			}
			keep[parent][path.Base(child)] = true
		}
	}
	snippets := p.dep().Path("infra", "caddy", "snippets")
	keep[snippets] = map[string]bool{}
	dirs := make([]string, 0, len(keep))
	for dir := range keep {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	var loops []string
	for _, dir := range dirs {
		q := quote(dir)
		head := fmt.Sprintf(`for f in %[1]s/* %[1]s/.[!.]* %[1]s/..?*; do [ -e "$f" ] || [ -L "$f" ] || continue;`, q)
		if len(keep[dir]) == 0 {
			loops = append(loops, head+` rm -rf -- "$f"; done`)
			continue
		}
		var names []string
		for n := range keep[dir] {
			names = append(names, quote(n))
		}
		sort.Strings(names)
		loops = append(loops, head+fmt.Sprintf(` case "${f##*/}" in %s) ;; *) rm -rf -- "$f";; esac; done`, strings.Join(names, "|")))
	}
	return "set -e; " + strings.Join(loops, "; ")
}

// emptyBesideKept removes the empty directories under the root, but never a
// kept one or one inside it: the container mounts them, and a missing mount
// source keeps it from starting again.
func (p *Plan) emptyBesideKept() string {
	var not []string
	for _, k := range p.keptPaths() {
		not = append(not, "! -path "+quote(k), "! -path "+quote(k+"/*"))
	}
	return fmt.Sprintf(`d=%s; if [ -d "$d" ]; then find "$d" -mindepth 1 -depth -type d -empty %s -delete; fi`, quote(p.dep().Root()), strings.Join(not, " "))
}

func caddyCommand(container hostcheck.Container, verb, config string) string {
	return fmt.Sprintf("docker exec %s caddy %s --config %s --adapter caddyfile", quote(container.ID), verb, quote(config))
}

// reduceCaddy reduces the running Caddy to the host owner's sites without a
// moment in which they are not served: the reduced file is staged where the
// container sees it, validated in it, and loaded with caddy reload, which
// leaves the running configuration as it was if it fails. Only then is it
// copied over the Caddyfile on the host, in place, since that is a single
// file bind mount and a new file renamed over it would not reach the
// container, with the original copied aside first, and read back. A failure
// at any point puts the original back, loaded, and touches nothing else of
// the deployment's.
func (p *Plan) reduceCaddy(t apply.Transport, cp *caddyPlan) error {
	p.work("reduce Caddy to the host's sites").Detail("Caddy %s, for %s", cp.container.Name, strings.Join(cp.sites, ", "))
	staged, backup := p.staged(), p.caddyfile()+".paisans-original"
	tidy := func() { _, _ = t.Run("rm -f -- " + quote(staged) + " " + quote(backup)) }
	stop := func(what string, err error, out string) error {
		tidy()
		return fmt.Errorf("%s: Caddy, kept for the host owner's sites: %s: %w: %s. Its Caddyfile is the original, it still serves every site it did, and nothing else was removed", p.Site, what, err, lastLines(out, 3))
	}
	if err := t.WriteFile(staged, cp.reduced, 0o644); err != nil {
		return stop("staging the reduced Caddyfile", err, "")
	}
	inContainer := containerSnippets + "/" + stagedName
	if out, err := t.Run(caddyCommand(cp.container, "validate", inContainer)); err != nil {
		return stop("the reduced Caddyfile does not validate", err, out)
	}
	if out, err := t.Run(caddyCommand(cp.container, "reload", inContainer)); err != nil {
		if back, berr := t.Run(caddyCommand(cp.container, "reload", containerConfig)); berr != nil {
			tidy()
			return fmt.Errorf("%s: Caddy, kept for the host owner's sites: loading the reduced Caddyfile: %v: %s. Reloading the original failed too: %v: %s. Nothing else was removed. Check the container", p.Site, err, lastLines(out, 2), berr, lastLines(back, 2))
		}
		return stop("loading the reduced Caddyfile", err, out)
	}
	if out, err := t.Run("cp -p -- " + quote(p.caddyfile()) + " " + quote(backup)); err != nil {
		return p.restoreCaddy(t, cp, tidy, "copying the original Caddyfile aside", err, out)
	}
	if out, err := t.Run("cat -- " + quote(staged) + " > " + quote(p.caddyfile())); err != nil {
		return p.restoreCaddy(t, cp, tidy, "writing the reduced Caddyfile in place", err, out)
	}
	back, _, err := t.ReadFile(p.caddyfile())
	if err == nil && back != cp.reduced {
		err = fmt.Errorf("it reads back otherwise")
	}
	if err != nil {
		return p.restoreCaddy(t, cp, tidy, "reading the reduced Caddyfile back", err, "")
	}
	tidy()
	return nil
}

// restoreCaddy puts the original Caddyfile back after a failure once the
// reduced one is loaded: from the copy on the host when there is one, in
// place, and reloads it.
func (p *Plan) restoreCaddy(t apply.Transport, cp *caddyPlan, tidy func(), what string, cause error, out string) error {
	backup := p.caddyfile() + ".paisans-original"
	fail := func(how string, err error, o string) error {
		return fmt.Errorf("%s: Caddy, kept for the host owner's sites: %s: %v: %s. Putting the original back failed: %s: %v: %s. Nothing else was removed. The original Caddyfile is in %s if it was copied there; check %s and the container before anything restarts it", p.Site, what, cause, lastLines(out, 2), how, err, lastLines(o, 2), backup, p.caddyfile())
	}
	put := "if [ -f " + quote(backup) + " ]; then cat -- " + quote(backup) + " > " + quote(p.caddyfile()) + "; else exit 4; fi"
	if o, err := t.Run(put); err != nil {
		if _, ierr := t.RunInput("cat > "+quote(p.caddyfile()), cp.original); ierr != nil {
			return fail("writing it back", err, o)
		}
	}
	if back, _, err := t.ReadFile(p.caddyfile()); err != nil || back != cp.original {
		return fail("reading it back", fmt.Errorf("it is not the original"), "")
	}
	if o, err := t.Run(caddyCommand(cp.container, "reload", containerConfig)); err != nil {
		return fail("reloading it", err, o)
	}
	tidy()
	return fmt.Errorf("%s: Caddy, kept for the host owner's sites: %s: %w: %s. The original Caddyfile is back and loaded, it still serves every site it did, and nothing else was removed", p.Site, what, cause, lastLines(out, 3))
}

// verifyCaddy is what the gate checks of a kept Caddy: its Caddyfile is the
// reduced one.
func (p *Plan) verifyCaddy(t apply.Transport, cp *caddyPlan) []string {
	content, _, err := t.ReadFile(p.caddyfile())
	switch {
	case err != nil:
		return []string{"an unreadable " + p.caddyfile() + ": " + err.Error()}
	case content != cp.reduced:
		return []string{p.caddyfile() + " not reduced to the host owner's sites"}
	}
	return nil
}

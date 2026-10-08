package render_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/render"
)

// The gateway's Caddy imports the host's own site blocks: its compose file
// mounts HostSitesDir read only, its Caddyfile imports every *.caddy there as
// the last top level statement (after every snippet a block may import), and
// nothing is rendered into the directory. A monitor's Caddy does neither.
func TestGatewayImportsTheHostsOwnSites(t *testing.T) {
	plan := build(t)
	content := map[string]string{}
	for _, f := range plan.Files {
		content[f.Path] = f.Content
		if _, rel, _ := strings.Cut(f.Path, "/"); strings.HasPrefix("/"+rel, render.HostSitesDir+"/") {
			t.Errorf("%s is rendered into the host's own %s", f.Path, render.HostSitesDir)
		}
	}
	importLine := "import " + render.HostSitesMount + "/*.caddy"
	mountLine := "- " + render.HostSitesDir + ":" + render.HostSitesMount + ":ro"

	caddyfile := content["vm/srv/paisans/f2a9/infra/caddy/Caddyfile"]
	lines := strings.Split(strings.TrimRight(caddyfile, "\n"), "\n")
	if lines[len(lines)-1] != importLine {
		t.Errorf("the gateway's Caddyfile does not end with %q:\n%s", importLine, caddyfile)
	}
	if i := strings.Index(caddyfile, importLine); i < strings.LastIndex(caddyfile, "(upstream_single)") || i < strings.LastIndex(caddyfile, "\nimport /etc/caddy/snippets/") {
		t.Errorf("%q comes before a snippet a host's site block may import", importLine)
	}
	if !strings.Contains(content["vm/srv/paisans/f2a9/infra/compose.yaml"], mountLine) {
		t.Errorf("the gateway's Caddy does not mount %s read only", render.HostSitesDir)
	}

	if strings.Contains(content["watch/srv/paisans/f2a9/infra/caddy/Caddyfile"], render.HostSitesMount) {
		t.Error("a monitor's Caddyfile imports the gateway host's own sites")
	}
	if strings.Contains(content["watch/srv/paisans/f2a9/infra/compose.yaml"], render.HostSitesDir) {
		t.Error("a monitor's Caddy mounts the gateway host's own sites")
	}
}

// SiteStacks names exactly the stack directories Build renders for each site,
// without the secrets Build needs.
func TestSiteStacksMatchesWhatIsRendered(t *testing.T) {
	cfg := fixture(t)
	rendered := map[string]map[string]bool{}
	for _, f := range build(t).Files {
		site, rel, _ := strings.Cut(f.Path, "/")
		rest, ok := strings.CutPrefix(rel, "srv/paisans/f2a9/")
		if !ok {
			continue
		}
		if stack, _, nested := strings.Cut(rest, "/"); nested {
			if rendered[site] == nil {
				rendered[site] = map[string]bool{}
			}
			rendered[site][stack] = true
		}
	}
	for _, site := range cfg.SiteNames() {
		got, err := render.SiteStacks(cfg, site)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(rendered[site]) {
			t.Errorf("%s: SiteStacks = %v, rendered %v", site, got, rendered[site])
		}
		for _, stack := range got {
			if !rendered[site][stack] {
				t.Errorf("%s: SiteStacks names %s, which is not rendered there", site, stack)
			}
		}
	}
	if _, err := render.SiteStacks(cfg, "nowhere"); err == nil {
		t.Error("SiteStacks answered for an undeclared site")
	}
}

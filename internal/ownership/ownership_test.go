package ownership_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/ownership"
	"github.com/paisans-software/paisans-stack/internal/render"
)

const (
	ours   = "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"
	theirs = "0c1d2e3f-4a5b-4c6d-8e7f-8091a2b3c4d5"
)

func fixture(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func container(name, id, project, service string, pid int, networks ...string) hostcheck.Container {
	return hostcheck.Container{Name: name, Deployment: id, Project: project, Service: service, PID: pid, Networks: networks}
}

// On an apps site: a stack the configuration no longer places there is left
// over, running or stopped; one it still renders, another deployment's, an
// unlabelled one and one whose project is not paisans-<token>-... are not.
// Only manifest entries marked left over are listed.
func TestLeftoversAreOnlyWhatIsProvablyOurs(t *testing.T) {
	cfg := fixture(t)
	delete(cfg.Apps, "docs")
	inv := &hostcheck.Inventory{Containers: []hostcheck.Container{
		container("paisans-f2a9-talk-app-1", ours, "paisans-f2a9-talk", "app", 10),
		container("paisans-f2a9-docs-app-1", ours, "paisans-f2a9-docs", "app", 11),
		container("paisans-f2a9-docs-redis-1", ours, "paisans-f2a9-docs", "redis", 0),
		container("paisans-f2a9-old-app-1", ours, "paisans-f2a9-old", "app", 0),
		container("paisans-0c1d-docs-app-1", theirs, "paisans-0c1d-docs", "app", 12),
		container("docs-app-1", "", "paisans-f2a9-docs", "app", 13),
		container("hand-made", ours, "shop", "app", 14),
	}}
	manifest := []render.ManifestFile{
		{Path: "srv/paisans/f2a9/talk/.env"},
		{Path: "srv/paisans/f2a9/docs/.env", Leftover: true, LeftoverSince: "2026-10-01T00:00:00Z"},
	}
	r, err := ownership.Classify(cfg, "home-a", inv, manifest)
	if err != nil {
		t.Fatal(err)
	}
	want := []ownership.Stack{
		{Name: "docs", Project: "paisans-f2a9-docs", Containers: []string{"paisans-f2a9-docs-app-1", "paisans-f2a9-docs-redis-1"}, Running: 1},
		{Name: "old", Project: "paisans-f2a9-old", Containers: []string{"paisans-f2a9-old-app-1"}},
	}
	if !reflect.DeepEqual(r.Stacks, want) {
		t.Errorf("stacks = %+v, want %+v", r.Stacks, want)
	}
	if len(r.Files) != 1 || r.Files[0].Path != "srv/paisans/f2a9/docs/.env" {
		t.Errorf("files = %+v", r.Files)
	}
	if !r.Leftover() || r.Foreign() {
		t.Errorf("Leftover %v, Foreign %v", r.Leftover(), r.Foreign())
	}
	lines := strings.Join(r.LeftoverLines(), "\n")
	for _, want := range []string{
		"stack docs (compose project paisans-f2a9-docs, 1 of 2 running): paisans-f2a9-docs-app-1, paisans-f2a9-docs-redis-1",
		"stack old (compose project paisans-f2a9-old, stopped): paisans-f2a9-old-app-1",
		"file /srv/paisans/f2a9/docs/.env, left over since 2026-10-01T00:00:00Z",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("lines lack %q:\n%s", want, lines)
		}
	}
}

// On the gateway: every site block in the host's own directory, and every
// container without this deployment's label on a network the Caddy is
// attached to, is foreign and relies on it. The host's own namespace is not
// such a network.
func TestForeignUsersOfTheGatewaysCaddy(t *testing.T) {
	cfg := fixture(t)
	inv := &hostcheck.Inventory{
		HostSites: []string{"/srv/caddy.d/blog.caddy"},
		Containers: []hostcheck.Container{
			container("paisans-f2a9-infra-caddy-1", ours, "paisans-f2a9-infra", "caddy", 10, "host", "web"),
			container("paisans-f2a9-chat-app-1", ours, "paisans-f2a9-chat", "app", 11, "web"),
			container("blog-ghost-1", "", "blog", "ghost", 12, "web", "blog_default"),
			container("paisans-0c1d-talk-app-1", theirs, "paisans-0c1d-talk", "app", 13, "web"),
			container("node-exporter", "", "", "", 14, "host"),
			container("db", "", "blog", "db", 15, "blog_default"),
		},
	}
	r, err := ownership.Classify(cfg, "vm", inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Leftover() {
		t.Errorf("the gateway's own stacks read as left over: %+v", r.Stacks)
	}
	if !reflect.DeepEqual(r.HostSites, []string{"/srv/caddy.d/blog.caddy"}) {
		t.Errorf("host sites = %v", r.HostSites)
	}
	want := []ownership.Neighbour{{Container: "blog-ghost-1", Network: "web"}, {Container: "paisans-0c1d-talk-app-1", Network: "web"}}
	if !reflect.DeepEqual(r.Neighbours, want) {
		t.Errorf("neighbours = %+v, want %+v", r.Neighbours, want)
	}
	lines := strings.Join(r.ForeignLines(), "\n")
	if !strings.Contains(lines, "site block /srv/caddy.d/blog.caddy, served by this deployment's Caddy") || !strings.Contains(lines, "container blog-ghost-1, on network web with this deployment's Caddy") {
		t.Errorf("foreign lines:\n%s", lines)
	}

	// The same directory on a site whose Caddy is not the gateway's is not
	// served by this deployment.
	r, err = ownership.Classify(cfg, "home-a", inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HostSites) != 0 {
		t.Errorf("a site without the gateway role lists %v", r.HostSites)
	}
}

func TestAnUndeclaredSiteIsAnError(t *testing.T) {
	if _, err := ownership.Classify(fixture(t), "nowhere", &hostcheck.Inventory{}, nil); err == nil {
		t.Error("Classify answered for an undeclared site")
	}
}

// The apps leftovers belong to, for `paisans app remove`: a stack's name, a
// file's stack directory, and the app a route is named for. The
// infrastructure stack is never one.
func TestLeftoversNameTheirApps(t *testing.T) {
	r := ownership.Report{
		Stacks: []ownership.Stack{{Name: "old"}, {Name: "infra"}},
		Files: []render.ManifestFile{
			{Path: "srv/paisans/f2a9/docs/.env"},
			{Path: "srv/paisans/f2a9/infra/caddy/snippets/wiki-media.caddy"},
			{Path: "srv/paisans/f2a9/infra/caddy/snippets/gate-gates.caddy"},
			{Path: "srv/paisans/f2a9/infra/haproxy/haproxy.cfg"},
		},
	}
	if got, want := r.Apps(), []string{"docs", "gate", "old", "wiki"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Apps = %v, want %v", got, want)
	}
}

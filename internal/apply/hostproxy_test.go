package apply_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// A command other than apply renders the monitor as the last apply left it:
// joined to the web server's network, and trusting that server's address,
// read from what that apply deployed. Rendered without it, the reseed at the
// end of a topology change would recreate the app off that network.
func TestHostProxyOptionsKeepTheLastAppliedNetwork(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(filepath.Join("..", "render", "testdata", "secrets.fixture.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	watch := cfg.Sites["watch"]
	watch.Ingress = &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"}
	cfg.Sites["watch"] = watch

	applied, err := render.Build(cfg, secrets, render.WithHostProxy("status", render.HostProxy{Network: "web_default", Address: "172.20.0.5"}))
	if err != nil {
		t.Fatal(err)
	}
	h := newHost()
	for _, f := range applied.Files {
		if rel, ok := strings.CutPrefix(f.Path, "watch/"); ok {
			h.files["/"+rel] = f.Content
		}
	}

	options, err := apply.HostProxyOptions(cfg, map[string]apply.Transport{"watch": h})
	if err != nil || len(options) != 1 {
		t.Fatalf("%d options, %v", len(options), err)
	}
	again, err := render.Build(cfg, secrets, options...)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range applied.Files {
		if !strings.HasPrefix(f.Path, "watch/srv/paisans/f2a9/status/") {
			continue
		}
		for _, g := range again.Files {
			if g.Path == f.Path && g.Content != f.Content {
				t.Errorf("%s renders differently from what apply deployed:\n%s\nwant:\n%s", f.Path, g.Content, f.Content)
			}
		}
	}

	// Nothing deployed yet, or deployed without a network: as declared.
	if options, err := apply.HostProxyOptions(cfg, map[string]apply.Transport{"watch": newHost()}); err != nil || len(options) != 0 {
		t.Fatalf("%d options, %v", len(options), err)
	}
}

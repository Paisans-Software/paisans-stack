package render_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/kinds"
	"gopkg.in/yaml.v3"
)

// Pocket ID with cluster placement runs on every apps site, and every one of
// them runs through the standby wrapper: which site wins the database is
// decided at start, not by the render.
func TestPocketIDStandsByOnEveryAppsSite(t *testing.T) {
	files := planFiles(build(t))
	image, _ := kinds.DefaultImage("pocket-id", "app")
	marker, known := kinds.StandbyMarker(image)
	if !known {
		t.Fatalf("no marker for %s", image)
	}
	for _, site := range []string{"home-a", "home-b"} {
		wrapper, ok := files[site+"/srv/paisans/f2a9/auth/paisans-standby.sh"]
		if !ok {
			t.Fatalf("%s renders no standby wrapper for auth", site)
		}
		if !strings.Contains(wrapper, "\nmarker='"+marker+"'\n") {
			t.Errorf("%s's wrapper does not carry the table's marker %q", site, marker)
		}
		var doc struct {
			Services map[string]struct {
				Entrypoint  []string `yaml:"entrypoint"`
				Command     []string `yaml:"command"`
				Volumes     []string `yaml:"volumes"`
				StopGrace   string   `yaml:"stop_grace_period"`
				Healthcheck struct {
					Test []string `yaml:"test"`
				} `yaml:"healthcheck"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal([]byte(files[site+"/srv/paisans/f2a9/auth/compose.yaml"]), &doc); err != nil {
			t.Fatalf("%s auth compose: %v", site, err)
		}
		app := doc.Services["app"]
		if got := strings.Join(app.Entrypoint, " "); got != "/bin/sh /paisans/pocket-id-standby.sh" {
			t.Errorf("%s: entrypoint is %q, not the wrapper", site, got)
		}
		// Setting an entrypoint clears the image's command, so both halves of
		// what the image ran must come back as the wrapper's arguments.
		if got := strings.Join(app.Command, " "); got != "/app/docker/entrypoint.sh /app/pocket-id" {
			t.Errorf("%s: command is %q, not the image's entrypoint and command", site, got)
		}
		if !contains(app.Volumes, "/srv/paisans/f2a9/auth/paisans-standby.sh:/paisans/pocket-id-standby.sh:ro") {
			t.Errorf("%s: the wrapper is not mounted read only: %q", site, app.Volumes)
		}
		want := []string{"CMD-SHELL", "[ -f /tmp/paisans-standby ] || /app/pocket-id healthcheck"}
		if strings.Join(app.Healthcheck.Test, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s: healthcheck is %q, want %q", site, app.Healthcheck.Test, want)
		}
		if app.StopGrace != "30s" {
			t.Errorf("%s: stop_grace_period is %q; a clean stop needs longer than Pocket ID's 10s actor grace", site, app.StopGrace)
		}
	}
}

// The gateway finds the active site by passive health alone: a dial to a
// standby's port is refused, which marks only the standby down. The README
// rejects active checks for every app; this pins it for the one app where
// every site but one refuses by design.
func TestPocketIDRouteHasNoActiveHealthCheck(t *testing.T) {
	snippet := planFiles(build(t))["vm/srv/paisans/f2a9/infra/caddy/snippets/auth.caddy"]
	if !strings.Contains(snippet, "10.44.0.1:1411 10.44.0.2:1411") {
		t.Fatalf("auth is not routed to both apps sites in order:\n%s", snippet)
	}
	for _, active := range []string{"health_uri", "health_port", "health_interval"} {
		if strings.Contains(snippet, active) {
			t.Errorf("auth's route sets %s:\n%s", active, snippet)
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

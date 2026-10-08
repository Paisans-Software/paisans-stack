package ingress_test

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/ingress"
)

// fixture is the render fixture: status pinned to watch, a monitor in
// ingress mode paisans with public_address 203.0.113.20.
func fixture(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// external is the fixture's monitor behind the operator's web server.
func external(t *testing.T, listen string) ingress.Target {
	t.Helper()
	cfg := fixture(t)
	watch := cfg.Sites["watch"]
	watch.Ingress = &config.Ingress{Mode: config.IngressExternal, Listen: listen}
	cfg.Sites["watch"] = watch
	target, err := ingress.For(cfg, "status")
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestForReadsTheMonitor(t *testing.T) {
	target := external(t, "127.0.0.1:8480")
	want := ingress.Target{
		App: "status", Site: "watch", Hostname: "status.example.org",
		Mode: config.IngressExternal, Listen: "127.0.0.1:8480", ListenHost: "127.0.0.1", ListenPort: 8480,
		PublicAddress: "203.0.113.20", HealthPath: "/healthz", HealthExpect: "200",
	}
	if target != want {
		t.Fatalf("got  %+v\nwant %+v", target, want)
	}
}

// Only an app a monitor serves has an ingress to hand off: the gateway's
// apps are the toolkit's own Caddy's.
func TestForRefusesAnAppTheGatewayServes(t *testing.T) {
	cfg := fixture(t)
	if _, err := ingress.For(cfg, "talk"); err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("talk: %v", err)
	}
	if _, err := ingress.For(cfg, "nothing"); err == nil || !strings.Contains(err.Error(), "declares no app") {
		t.Fatalf("an undeclared app: %v", err)
	}
}

func TestShowHandsOffAnExternalApp(t *testing.T) {
	var buf bytes.Buffer
	ingress.Show(&buf, external(t, "127.0.0.1:8480"))
	out := buf.String()
	for _, want := range []string{
		"status.example.org", "http://127.0.0.1:8480", "/healthz",
		"terminate TLS", "Host", "X-Forwarded-For", "X-Forwarded-Proto",
		"203.0.113.20",
		"reverse_proxy 127.0.0.1:8480",
		"proxy_pass http://127.0.0.1:8480;",
		"proxy_set_header X-Forwarded-Proto $scheme;",
		"ProxyPass        / http://127.0.0.1:8480/",
		`RequestHeader set X-Forwarded-Proto "https"`,
		"paisans ingress check --app status",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "YOUR CERTIFICATE"); n != 3 {
		t.Errorf("want a certificate marker in each of the three snippets, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "ufw") {
		t.Errorf("a loopback listen needs no firewall warning:\n%s", out)
	}
}

func TestShowWarnsWhenListenIsNotLoopback(t *testing.T) {
	var buf bytes.Buffer
	ingress.Show(&buf, external(t, "192.168.1.20:8480"))
	if !strings.Contains(buf.String(), "ufw") || !strings.Contains(buf.String(), "proxy_pass http://192.168.1.20:8480;") {
		t.Fatal(buf.String())
	}
}

func TestShowHasNothingToHandOffInPaisansMode(t *testing.T) {
	target, err := ingress.For(fixture(t), "status")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ingress.Show(&buf, target)
	out := buf.String()
	if !strings.Contains(out, "nothing to hand off") || strings.Contains(out, "proxy_pass") || strings.Contains(out, "YOUR CERTIFICATE") {
		t.Fatal(out)
	}
}

// The toolkit's own edge refuses the monitor's status page, badges, /metrics
// and token API whatever the app's settings say. Behind the operator's web
// server that edge is theirs, so each snippet carries the same refusals.
func TestEverySnippetRefusesWhatTheToolkitsEdgeRefuses(t *testing.T) {
	var buf bytes.Buffer
	ingress.Show(&buf, external(t, "127.0.0.1:8480"))
	out := buf.String()
	for _, want := range []string{
		"@refused path /status* /badge/* /metrics /metrics/ /api/v1/*",
		"location ~* ^/(status|badge/|metrics/?$|api/v1/) {",
		`<LocationMatch "(?i)^/(status|badge/|metrics/?$|api/v1/)">`,
		"refuse /status*, /badge/*, /metrics and /api/v1/*",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// refusalCases are paths the monitor's Express app routes to its status
// page, badges, metrics or token API, in the forms its non strict, case
// insensitive routing still accepts, and paths that must pass.
var refusalCases = map[string]bool{
	"/status": true, "/STATUS": true, "/status/x": true, "/badge/1": true, "/Badge/1": true,
	"/metrics": true, "/metrics/": true, "/Metrics": true, "/api/v1/x": true, "/api/V1/x": true,
	"/": false, "/healthz": false, "/login/oidc": false, "/api/sites": false, "/metricsx": false,
}

// caddyPathMatches is Caddy's path matcher for the forms the snippets use:
// exact, or a prefix when the pattern ends in *, and case insensitive
// ("Path matches are exact but case-insensitive", caddyserver.com/docs/
// caddyfile/matchers).
func caddyPathMatches(patterns []string, path string) bool {
	path = strings.ToLower(path)
	for _, p := range patterns {
		p = strings.ToLower(p)
		if strings.HasSuffix(p, "*") && strings.HasPrefix(path, strings.TrimSuffix(p, "*")) || p == path {
			return true
		}
	}
	return false
}

func TestEachSnippetRefusesEveryFormOfTheRefusedPaths(t *testing.T) {
	var buf bytes.Buffer
	ingress.Show(&buf, external(t, "127.0.0.1:8480"))
	out := buf.String()

	caddy := regexp.MustCompile(`@refused path (.+)`).FindStringSubmatch(out)
	nginx := regexp.MustCompile(`location ~\* (\S+) \{`).FindStringSubmatch(out)
	apache := regexp.MustCompile(`<LocationMatch "([^"]+)">`).FindStringSubmatch(out)
	if caddy == nil || nginx == nil || apache == nil {
		t.Fatalf("a snippet has no refusal:\n%s", out)
	}
	// nginx's ~* is a case insensitive match.
	nginxRe := regexp.MustCompile("(?i)" + nginx[1])
	apacheRe := regexp.MustCompile(apache[1])
	for path, refused := range refusalCases {
		if got := caddyPathMatches(strings.Fields(caddy[1]), path); got != refused {
			t.Errorf("caddy: %s refused %v, want %v", path, got, refused)
		}
		if got := nginxRe.MatchString(path); got != refused {
			t.Errorf("nginx: %s refused %v, want %v", path, got, refused)
		}
		if got := apacheRe.MatchString(path); got != refused {
			t.Errorf("apache: %s refused %v, want %v", path, got, refused)
		}
	}
}

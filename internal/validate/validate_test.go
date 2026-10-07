package validate_test

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

func load(t *testing.T, name string) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("testdata", name+".yaml"))
	if err != nil {
		t.Fatalf("loading %s: %v", name, err)
	}
	return cfg
}

// Each fixture is the valid one with exactly one thing broken, so a rule that
// fires on the wrong fixture is a test failure rather than a puzzle.
func TestRulesFire(t *testing.T) {
	cases := []struct {
		fixture string
		rule    string
		level   validate.Level
	}{
		{"witness-shares-failure-domain", "witness-shares-failure-domain", validate.Refuse},
		{"two-etcd-voters", "two-etcd-voters", validate.Refuse},
		{"undeclared-site", "undeclared-site", validate.Refuse},
		{"invalid-placement", "invalid-placement", validate.Refuse},
		{"outline-bucket-named-outline", "outline-bucket-named-outline", validate.Refuse},
		{"sso-dashboard-link-not-a-path", "sso-dashboard-link-not-a-path", validate.Refuse},
		{"cluster-site-without-data-role", "cluster-site-without-data-role", validate.Refuse},
		{"cluster-app-without-apps-site", "cluster-app-without-apps-site", validate.Refuse},
		{"garage-replication-exceeds-sites", "garage-replication-exceeds-sites", validate.Refuse},
		{"garage-consistency-unknown", "garage-consistency-unknown", validate.Refuse},
		{"garage-consistency-dangerous", "garage-consistency-dangerous", validate.Warn},
		{"garage-consistency-degraded-is-consistent", "garage-consistency-degraded-is-consistent", validate.Warn},
		{"garage-single-copy", "garage-single-copy", validate.Warn},
		{"garage-two-sites-stop-uploads", "garage-two-sites-stop-uploads", validate.Warn},
		{"garage-partial-uploads", "garage-partial-uploads", validate.Warn},
		{"site-address-outside-mesh", "site-address-outside-mesh", validate.Refuse},
		{"unknown-image-service", "unknown-image-service", validate.Refuse},
		{"floating-image-tag", "floating-image-tag", validate.Refuse},
		{"cluster-placement-without-a-cluster", "cluster-placement-without-a-cluster", validate.Refuse},
		{"even-etcd-voters", "even-etcd-voters", validate.Warn},
		{"mesh-subnet-is-not-private", "mesh-subnet-is-not-private", validate.Warn},
		{"pinned-app-on-witness", "pinned-app-on-witness", validate.Warn},
		{"gateway-on-data-site", "gateway-on-data-site", validate.Warn},
		{"pocket-id-file-backend", "pocket-id-file-backend", validate.Warn},
		{"image-for-absent-postgres", "image-for-absent-postgres", validate.Warn},
		{"watchdog-off-on-data-site", "watchdog-off-on-data-site", validate.Warn},
		{"acme-provider-needs-an-image", "acme-provider-needs-an-image", validate.Refuse},
		{"acme-image-is-stock-caddy", "acme-image-is-stock-caddy", validate.Refuse},
		{"acme-image-is-floating", "acme-image-is-floating", validate.Refuse},
		{"unknown-hostname-role", "unknown-hostname-role", validate.Refuse},
		{"duplicate-hostname", "duplicate-hostname", validate.Refuse},
		{"gate-without-a-gate-app", "gate-without-a-gate-app", validate.Refuse},
		{"gated-matrix-hostname", "gated-matrix-hostname", validate.Refuse},
		{"homeserver-must-be-pinned", "homeserver-must-be-pinned", validate.Refuse},
		{"media-hostname-is-not-a-hostname", "media-hostname-is-not-a-hostname", validate.Refuse},
		{"media-hostname-outside-the-domain", "media-hostname-outside-the-domain", validate.Refuse},
		{"media-hostname-under-an-app-hostname", "media-hostname-under-an-app-hostname", validate.Refuse},
		{"outline-bucket-in-media-url", "outline-bucket-in-media-url", validate.Refuse},
		{"duplicate-derived-media-hostname", "duplicate-hostname", validate.Refuse},
		{"config-key-is-nested-in-an-env-file", "config-key-is-nested-in-an-env-file", validate.Refuse},
		{"config-key-looks-like-a-secret", "config-key-looks-like-a-secret", validate.Refuse},
		{"config-key-steers-compose", "config-key-steers-compose", validate.Refuse},
		{"uptime-needs-an-admin-group", "uptime-needs-an-admin-group", validate.Refuse},
		{"smtp-on-a-kind-without-mail", "smtp-on-a-kind-without-mail", validate.Refuse},
		{"uptime-without-smtp", "uptime-without-smtp", validate.Warn},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			result := validate.Check(load(t, tc.fixture))
			if !result.Has(tc.rule) {
				t.Fatalf("rule %s did not fire. findings: %v", tc.rule, result.Findings)
			}
			for _, f := range result.Findings {
				if f.Rule != tc.rule {
					continue
				}
				if f.Level != tc.level {
					t.Fatalf("rule %s fired at %s, want %s", tc.rule, f.Level, tc.level)
				}
				if f.Key == "" {
					t.Fatalf("rule %s fired without naming a key", tc.rule)
				}
				if len(f.Message) < 40 {
					t.Fatalf("rule %s message is too short to act on: %q", tc.rule, f.Message)
				}
			}
			// A refusal blocks rendering; a warning must not.
			if tc.level == validate.Refuse && !result.Refused() {
				t.Fatal("a refusal did not block rendering")
			}
			if tc.level == validate.Warn && result.Refused() {
				t.Fatalf("a warning blocked rendering: %v", result.Refusals())
			}
		})
	}
}

// withConfig is the valid fixture plus the element app of the secret fixture,
// so that env (docs, auth) and json (web) kinds are all present, with every
// passthrough map emptied and then one app's replaced by keys.
func withConfig(t *testing.T, app string, keys map[string]any) *config.Config {
	t.Helper()
	cfg := load(t, "config-key-looks-like-a-secret")
	for name, a := range cfg.Apps {
		a.Config = nil
		cfg.Apps[name] = a
	}
	if result := validate.Check(cfg); len(result.Findings) != 0 {
		t.Fatalf("the base for these cases is not quiet: %v", result.Findings)
	}
	a, ok := cfg.Apps[app]
	if !ok {
		t.Fatalf("the fixture declares no app %q", app)
	}
	a.Config = keys
	cfg.Apps[app] = a
	return cfg
}

// refusedFor reports whether rule refused exactly the given key.
func refusedFor(result validate.Result, rule, key string) bool {
	for _, f := range result.Refusals() {
		if f.Rule == rule && f.Key == key {
			return true
		}
	}
	return false
}

// The secret check reads a name the way people write one, so the separator a
// convention puts between the words does not hide the word. The env spelling
// API_KEY is the likeliest real case, and the camel case privateKey the json
// one.
func TestTheSecretCheckIgnoresSeparatorsAndCase(t *testing.T) {
	cases := []struct {
		app, key string
		refused  bool
	}{
		{"docs", "SENDGRID_API_KEY", true},
		{"web", "privateKey", true},
		{"web", "signing.private-key", true},
		{"docs", "SMTP_PASSWORD", true},
		{"web", "app.api_token", true},
		// A control: an ordinary name in the same places is not refused, so
		// the rows above are not passing because everything is.
		{"docs", "DEFAULT_LANGUAGE", false},
		{"web", "default_theme", false},
	}
	for _, tc := range cases {
		result := validate.Check(withConfig(t, tc.app, map[string]any{tc.key: "x"}))
		got := refusedFor(result, "config-key-looks-like-a-secret", "apps."+tc.app+".config."+tc.key)
		if got != tc.refused {
			t.Errorf("%s on %s: refused=%v, want %v. findings: %v", tc.key, tc.app, got, tc.refused, result.Findings)
		}
	}
}

// Both prefixes are refused on an env kind, whose .env compose also reads,
// and on no other kind, whose config file compose never sees.
func TestComposeSteeringKeysAreRefusedOnEnvKindsOnly(t *testing.T) {
	cases := []struct {
		app, key string
		refused  bool
	}{
		{"docs", "COMPOSE_PROJECT_NAME", true},
		{"docs", "COMPOSE_PROFILES", true},
		{"auth", "DOCKER_HOST", true},
		{"web", "COMPOSE_PROJECT_NAME", false},
		{"docs", "MY_COMPOSE_NOTE", false},
	}
	for _, tc := range cases {
		result := validate.Check(withConfig(t, tc.app, map[string]any{tc.key: "x"}))
		got := refusedFor(result, "config-key-steers-compose", "apps."+tc.app+".config."+tc.key)
		if got != tc.refused {
			t.Errorf("%s on %s: refused=%v, want %v. findings: %v", tc.key, tc.app, got, tc.refused, result.Findings)
		}
	}
}

// The valid fixture must be quiet. A rule that fires on a coherent
// configuration trains an operator to ignore the output.
func TestValidFixtureIsQuiet(t *testing.T) {
	result := validate.Check(load(t, "valid"))
	if len(result.Findings) != 0 {
		t.Fatalf("valid fixture produced findings: %v", result.Findings)
	}
}

// The shipped example must validate. It deliberately demonstrates three
// warnings: its Synapse stack and its uptime monitor are both pinned to the
// site that also holds the witness role (the monitor with VACUUM off, which is
// what makes that a reasonable neighbour), and its two Garage sites at
// replication 2 stop uploads while either is down.
func TestExampleValidates(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "paisans.example.yaml"))
	if err != nil {
		t.Fatalf("loading the example: %v", err)
	}
	result := validate.Check(cfg)
	if result.Refused() {
		t.Fatalf("the example was refused: %v", result.Refusals())
	}
	warnings := result.Warnings()
	var rules []string
	for _, w := range warnings {
		rules = append(rules, w.Rule)
	}
	sort.Strings(rules)
	if strings.Join(rules, ",") != "garage-two-sites-stop-uploads,pinned-app-on-witness,pinned-app-on-witness" {
		t.Fatalf("unexpected warnings on the example: %v", warnings)
	}
}

// One fixture per refusal must name its rule in the failure, because the
// message is the only thing an operator sees.
func TestRefusalMessagesAreActionable(t *testing.T) {
	result := validate.Check(load(t, "two-etcd-voters"))
	var found bool
	for _, f := range result.Refusals() {
		if f.Rule != "two-etcd-voters" {
			continue
		}
		found = true
		if !strings.Contains(f.Message, "one member, or three") {
			t.Fatalf("the refusal does not say what to do instead: %q", f.Message)
		}
	}
	if !found {
		t.Fatal("two-etcd-voters did not fire")
	}
}

// Findings are sorted, so that two runs over one file print the same thing and
// a diff between runs means something.
func TestFindingsAreDeterministic(t *testing.T) {
	cfg := load(t, "witness-shares-failure-domain")
	first := validate.Check(cfg)
	second := validate.Check(cfg)
	if len(first.Findings) != len(second.Findings) {
		t.Fatal("two runs produced different numbers of findings")
	}
	for i := range first.Findings {
		if first.Findings[i] != second.Findings[i] {
			t.Fatalf("finding %d differs between runs", i)
		}
	}
}

// withMedia is the valid fixture with docs's media hostname declared, or left
// to the derivation when media is empty, and one more app's hostname set.
func withMedia(t *testing.T, media string) *config.Config {
	t.Helper()
	cfg := load(t, "valid")
	docs := cfg.Apps["docs"]
	docs.Hostnames = nil
	if media != "" {
		docs.Hostnames = map[string]string{"media": media}
	}
	cfg.Apps["docs"] = docs
	return cfg
}

// The derived name and a declared one are checked alike, because both become
// a site address. A derived name can be wrong without anything in the file
// being wrong: a label of 60 characters is legal, and is not once -media is
// appended to it.
func TestADerivedMediaHostnameIsCheckedLikeADeclaredOne(t *testing.T) {
	cfg := withMedia(t, "")
	if result := validate.Check(cfg); len(result.Findings) != 0 {
		t.Fatalf("the derived docs-media.example.org should be quiet, got %v", result.Findings)
	}

	long := strings.Repeat("d", 60)
	docs := cfg.Apps["docs"]
	docs.Hostname = long + ".example.org"
	cfg.Apps["docs"] = docs
	result := validate.Check(cfg)
	if !refusedFor(result, "media-hostname-is-not-a-hostname", "apps.docs.hostnames.media") {
		t.Fatalf("%s-media is a 66 character label and must be refused, got %v", long, result.Findings)
	}
	for _, f := range result.Refusals() {
		if f.Rule == "media-hostname-is-not-a-hostname" && !strings.Contains(f.Message, "derived") {
			t.Errorf("a refusal of a name nobody wrote must say it was derived, got %q", f.Message)
		}
	}
}

// Each declared shape that cannot work, against the key an operator edits.
func TestADeclaredMediaHostnameIsAHostnameUnderTheDomain(t *testing.T) {
	for _, tc := range []struct {
		media, rule string
	}{
		{"Docs-Media.example.org", "media-hostname-is-not-a-hostname"},
		{"https://docs-media.example.org", "media-hostname-is-not-a-hostname"},
		{"docs-media.example.org:8443", "media-hostname-is-not-a-hostname"},
		{"-docs.example.org", "media-hostname-is-not-a-hostname"},
		{"docs..example.org", "media-hostname-is-not-a-hostname"},
		{"localhost", "media-hostname-is-not-a-hostname"},
		{"docs-media.example.net", "media-hostname-outside-the-domain"},
		// A suffix match on the bare domain would accept this one.
		{"docs-media.notexample.org", "media-hostname-outside-the-domain"},
		{"media.docs.example.org", "media-hostname-under-an-app-hostname"},
		// A child of another app's hostname is refused too.
		{"media.id.example.org", "media-hostname-under-an-app-hostname"},
	} {
		result := validate.Check(withMedia(t, tc.media))
		if !refusedFor(result, tc.rule, "apps.docs.hostnames.media") {
			t.Errorf("hostnames.media %q: want %s, got %v", tc.media, tc.rule, result.Findings)
		}
	}
	for _, ok := range []string{"docs-media.example.org", "attachments.example.org", "a.b.example.org"} {
		if result := validate.Check(withMedia(t, ok)); len(result.Findings) != 0 {
			t.Errorf("hostnames.media %q should be accepted, got %v", ok, result.Findings)
		}
	}
}

// A declared media hostname that another app already uses as its own, and a
// derived one that collides with a declared one, are the same refusal from two
// directions.
func TestAMediaHostnameCannotBeAnotherAppsHostname(t *testing.T) {
	result := validate.Check(withMedia(t, "id.example.org"))
	if !refusedFor(result, "duplicate-hostname", "apps.docs.hostnames.media") && !refusedFor(result, "duplicate-hostname", "apps.auth.hostname") {
		t.Errorf("docs's media hostname is auth's own hostname and must be refused, got %v", result.Findings)
	}

	cfg := load(t, "duplicate-derived-media-hostname")
	result = validate.Check(cfg)
	var found bool
	for _, f := range result.Refusals() {
		if f.Rule == "duplicate-hostname" && strings.Contains(f.Message, "derived") {
			found = true
		}
	}
	if !found {
		t.Errorf("a clash with a derived media hostname must say the name was derived, got %v", result.Findings)
	}
}

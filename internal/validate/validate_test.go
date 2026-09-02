package validate_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/josephquigley/paisans-stack/internal/config"
	"github.com/josephquigley/paisans-stack/internal/validate"
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
		{"cluster-site-without-data-role", "cluster-site-without-data-role", validate.Refuse},
		{"cluster-app-without-apps-site", "cluster-app-without-apps-site", validate.Refuse},
		{"garage-replication-exceeds-sites", "garage-replication-exceeds-sites", validate.Refuse},
		{"site-address-outside-mesh", "site-address-outside-mesh", validate.Refuse},
		{"even-etcd-voters", "even-etcd-voters", validate.Warn},
		{"mesh-subnet-is-not-private", "mesh-subnet-is-not-private", validate.Warn},
		{"pinned-app-on-witness", "pinned-app-on-witness", validate.Warn},
		{"gateway-on-data-site", "gateway-on-data-site", validate.Warn},
		{"pocket-id-file-backend", "pocket-id-file-backend", validate.Warn},
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

// The valid fixture must be quiet. A rule that fires on a coherent
// configuration trains an operator to ignore the output.
func TestValidFixtureIsQuiet(t *testing.T) {
	result := validate.Check(load(t, "valid"))
	if len(result.Findings) != 0 {
		t.Fatalf("valid fixture produced findings: %v", result.Findings)
	}
}

// The shipped example must validate. It deliberately demonstrates one warning:
// its Synapse stack is pinned to the site that also holds the witness role.
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
	if len(warnings) != 1 || warnings[0].Rule != "pinned-app-on-witness" {
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

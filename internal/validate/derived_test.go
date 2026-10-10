package validate_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/validate"
)

// findingFor returns the first finding of rule, failing t when there is none.
func findingFor(t *testing.T, result validate.Result, rule string) validate.Finding {
	t.Helper()
	for _, f := range result.Findings {
		if f.Rule == rule {
			return f
		}
	}
	t.Fatalf("rule %s did not fire. findings: %v", rule, result.Findings)
	return validate.Finding{}
}

// Leaving both lists out of the valid fixture derives what it writes, so it
// validates exactly as the valid fixture does.
func TestDerivedListsValidateAsTheWrittenOnes(t *testing.T) {
	got := validate.Check(load(t, "derived-lists"))
	want := validate.Check(load(t, "valid"))
	if len(got.Findings) != len(want.Findings) {
		t.Fatalf("derived lists found %v, the written ones %v", got.Findings, want.Findings)
	}
	for i := range got.Findings {
		if got.Findings[i].Rule != want.Findings[i].Rule {
			t.Fatalf("derived lists found %v, the written ones %v", got.Findings, want.Findings)
		}
	}
}

// The comparisons between cluster.sites and the data role are about a list
// someone wrote. A derived list cannot disagree with the roles it came from,
// so the rules hold their fire on one even when the list is changed after the
// load, and still fire on a written list.
func TestClusterComparisonsApplyToAWrittenListOnly(t *testing.T) {
	for _, rule := range []string{"data-site-not-in-cluster", "cluster-site-without-data-role"} {
		cfg := load(t, "derived-lists")
		if rule == "data-site-not-in-cluster" {
			cfg.Cluster.Sites = []string{"home-a"}
		} else {
			cfg.Cluster.Sites = []string{"home-a", "home-b", "vm"}
		}
		if validate.Check(cfg).Has(rule) {
			t.Errorf("%s fired on a derived cluster.sites", rule)
		}
		cfg.Cluster.SitesDerived = false
		if !validate.Check(cfg).Has(rule) {
			t.Errorf("%s did not fire on the same list written", rule)
		}
	}
}

// Two derived voters are refused, and the message says what to change in a
// file that never wrote etcd.members: a witness in a third location, or one
// voter written out.
func TestTwoDerivedVotersSayHowToFixTheRoles(t *testing.T) {
	f := findingFor(t, validate.Check(load(t, "two-etcd-voters-derived")), "two-etcd-voters")
	if f.Level != validate.Refuse {
		t.Fatalf("two derived voters fired at %s", f.Level)
	}
	if f.Key != "etcd.members (derived from the roles)" {
		t.Errorf("key %q does not say the list was derived", f.Key)
	}
	for _, want := range []string{"witness role", "third location", "fails independently", "etcd.members: [home-a]", "automatic failover"} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("message does not say %q: %s", want, f.Message)
		}
	}
}

// One data site and a witness derive two voters too, and there the witness
// has no tie to break, so the way out is to take its role off.
func TestOneDataSiteAndAWitnessSayDropTheWitness(t *testing.T) {
	cfg := load(t, "derived-lists")
	site := cfg.Sites["home-b"]
	site.Roles = []config.Role{config.RoleApps}
	cfg.Sites["home-b"] = site
	cfg.Etcd.Members = []string{"vm", "home-a"}
	cfg.Cluster.Sites = []string{"home-a"}
	f := findingFor(t, validate.Check(cfg), "two-etcd-voters")
	if !strings.Contains(f.Message, "witness role off vm") {
		t.Errorf("message does not say to take the witness role off vm: %s", f.Message)
	}
}

// A written pair keeps its message, which names the key the operator wrote.
func TestTwoWrittenVotersKeepTheirMessage(t *testing.T) {
	f := findingFor(t, validate.Check(load(t, "two-etcd-voters")), "two-etcd-voters")
	if f.Key != "etcd.members" || !strings.Contains(f.Message, "Declare one member, or three") {
		t.Errorf("written pair reported as %q: %s", f.Key, f.Message)
	}
}

// An even derived count names the list as derived and says how to change it.
func TestEvenDerivedVotersSayHowToChangeThem(t *testing.T) {
	f := findingFor(t, validate.Check(load(t, "even-etcd-voters-derived")), "even-etcd-voters")
	if f.Key != "etcd.members (derived from the roles)" {
		t.Errorf("key %q does not say the list was derived", f.Key)
	}
	if !strings.Contains(f.Message, "witness role") || !strings.Contains(f.Message, "write etcd.members") {
		t.Errorf("message does not say how to change a derived count: %s", f.Message)
	}
}

// Four derived voters with no witness cannot lose a witness role: the advice
// is a fifth voter in a third location, or a written list.
func TestEvenDerivedVotersWithoutAWitness(t *testing.T) {
	cfg := load(t, "even-etcd-voters-derived")
	for _, name := range []string{"vm", "extra"} {
		s := cfg.Sites[name]
		var roles []config.Role
		for _, r := range s.Roles {
			if r != config.RoleWitness {
				roles = append(roles, r)
			}
		}
		s.Roles = append(roles, config.RoleData)
		cfg.Sites[name] = s
	}
	cfg.Etcd.Members = cfg.DerivedEtcdMembers()
	f := findingFor(t, validate.Check(cfg), "even-etcd-voters")
	if strings.Contains(f.Message, "witness role off") || !strings.Contains(f.Message, "third location") || !strings.Contains(f.Message, "write etcd.members") {
		t.Errorf("advice for four data sites and no witness: %s", f.Message)
	}
}

// With a witness, the advice says when taking its role off is safe.
func TestEvenDerivedVotersWithAWitnessSayWhen(t *testing.T) {
	f := findingFor(t, validate.Check(load(t, "even-etcd-voters-derived")), "even-etcd-voters")
	if !strings.Contains(f.Message, "before the deployment is founded") || !strings.Contains(f.Message, "paisans site remove") {
		t.Errorf("advice with witnesses: %s", f.Message)
	}
}

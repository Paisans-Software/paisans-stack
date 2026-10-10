package secretsgen_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployrecord"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
)

func TestOrphans(t *testing.T) {
	cfg := &config.Config{
		Sites: map[string]config.Site{"home-a": {}},
		Apps: map[string]config.App{
			"talk":  {Kind: config.KindMbin},
			"auth":  {Kind: config.KindPocketID, Settings: map[string]any{"signup_default_groups": []any{"provisional"}}},
			"auth2": {Kind: config.KindPocketID, Settings: map[string]any{"signup_default_groups": []any{"members"}}},
		},
	}
	s := &config.Secrets{
		Sites:          map[string]config.SiteSecrets{"home-a": {}, "monitor-a": {WireGuardPrivateKey: "x"}},
		Apps:           map[string]map[string]any{"talk": {}, "uptime": {"database_password": "x"}},
		OIDCClients:    map[string]config.OIDCClient{"talk": {}, "uptime": {ClientID: "c-1d2e", ClientSecret: "s3cr3t"}},
		PocketIDGroups: map[string]string{"provisional": "1", "members": "2", "old": "3"},
	}
	var keys []string
	leaves := 0
	for _, o := range secretsgen.Orphans(cfg, s, nil) {
		keys = append(keys, o.Key)
		if o.Leaves != "" {
			leaves++
		}
		for _, value := range []string{"x", "c-1d2e", "s3cr3t"} {
			if strings.Contains(o.Why+" "+o.Leaves, value+" ") {
				t.Errorf("%s reports a value: %q %q", o.Key, o.Why, o.Leaves)
			}
		}
	}
	want := []string{"apps.uptime", "oidc_clients.uptime", "pocket_id_groups.old", "sites.monitor-a"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", keys, want)
	}
	if leaves != 1 {
		t.Errorf("%d orphan(s) say they leave something, want only the OIDC client", leaves)
	}

	secretsgen.Prune(s, secretsgen.Orphans(cfg, s, nil))
	if len(secretsgen.Orphans(cfg, s, nil)) != 0 {
		t.Error("orphans left after Prune")
	}
	if _, ok := s.Sites["home-a"]; !ok {
		t.Error("Prune removed a declared site's secrets")
	}
	if s.PocketIDGroups["members"] != "2" {
		t.Error("Prune removed a group the second Pocket ID app names")
	}
}

func TestNoOrphansWhenTheSecretsMatch(t *testing.T) {
	cfg := &config.Config{Sites: map[string]config.Site{"home-a": {}}, Apps: map[string]config.App{"talk": {Kind: config.KindMbin}}}
	s := &config.Secrets{
		Sites:       map[string]config.SiteSecrets{"home-a": {}},
		Apps:        map[string]map[string]any{"talk": {}},
		OIDCClients: map[string]config.OIDCClient{"talk": {}},
	}
	if o := secretsgen.Orphans(cfg, s, nil); len(o) != 0 {
		t.Errorf("got %v", o)
	}
}

func TestOrphansKeepWhatAnyRecordLists(t *testing.T) {
	cfg := &config.Config{Sites: map[string]config.Site{"home-a": {}}, Apps: map[string]config.App{}}
	s := &config.Secrets{
		Sites:          map[string]config.SiteSecrets{"home-a": {}, "monitor-a": {}, "monitor-b": {}},
		Apps:           map[string]map[string]any{"uptime": {}},
		OIDCClients:    map[string]config.OIDCClient{"uptime": {}},
		PocketIDGroups: map[string]string{"old": "1"},
	}
	one := deployrecord.Record{Sites: []string{"home-a", "monitor-a"}, Apps: []string{"uptime"}}
	two := deployrecord.Record{Sites: []string{"home-a"}, PocketIDGroups: []string{"old"}}
	u := deployrecord.Union(one, two)
	var keys []string
	for _, o := range secretsgen.Orphans(cfg, s, &u) {
		keys = append(keys, o.Key)
	}
	if strings.Join(keys, ",") != "sites.monitor-b" {
		t.Errorf("got %v, want only sites.monitor-b", keys)
	}
	if n := len(secretsgen.Orphans(cfg, s, nil)); n != 5 {
		t.Errorf("without a record: %d orphans, want 5", n)
	}
}

func TestDroppedIsRegardlessOfSecrets(t *testing.T) {
	cfg := &config.Config{Sites: map[string]config.Site{"home-a": {}}, Apps: map[string]config.App{}}
	rec := deployrecord.Record{Sites: []string{"home-a", "monitor-a"}, Apps: []string{"uptime"}, PocketIDGroups: []string{"old"}}
	var keys []string
	for _, o := range secretsgen.Dropped(cfg, &rec) {
		keys = append(keys, o.Key)
		if !strings.Contains(o.Why, "still deployed") && !strings.Contains(o.Why, "is deployed") {
			t.Errorf("%s: %q", o.Key, o.Why)
		}
		if o.Leaves == "" {
			t.Errorf("%s says nothing about what to do", o.Key)
		}
	}
	if strings.Join(keys, ",") != "apps.uptime,pocket_id_groups.old,sites.monitor-a" {
		t.Errorf("got %v", keys)
	}
	if len(secretsgen.Dropped(cfg, nil)) != 0 {
		t.Error("no record dropped something")
	}
}

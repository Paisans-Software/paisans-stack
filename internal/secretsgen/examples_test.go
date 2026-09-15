package secretsgen

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// examples/secrets.example.yaml exists so a reader can see WHICH secrets a
// deployment needs without holding any of them. That purpose fails silently
// the moment the example drifts from appSecretKeys: nothing else in the repo
// reads the example, so a kind that grows a new secret leaves the example
// looking complete while it is not. This is a white-box test, in the package
// rather than package secretsgen_test, because appSecretKeys is unexported
// and the whole point is to check the example against the real list rather
// than a copy of it typed into the test.
func TestExampleSecretsMatchAppSecretKeys(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "paisans.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(filepath.Join("..", "..", "examples", "secrets.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range cfg.AppNames() {
		want := appSecretKeys(cfg.Apps[name])
		sort.Strings(want)

		var got []string
		for key := range secrets.Apps[name] {
			got = append(got, key)
		}
		sort.Strings(got)

		if !equal(want, got) {
			t.Errorf("apps.%s (kind %s): appSecretKeys wants %v, examples/secrets.example.yaml has %v",
				name, cfg.Apps[name].Kind, want, got)
		}
	}
}

// oidc_clients.<name> is owed by every app that is not the identity provider
// itself, per the owed() reasoning below. The example shows a captured value
// for every app that needs one, including the homeserver's, whose client
// belongs to Matrix Authentication Service rather than to Synapse.
func TestExampleOIDCClientsCoverEveryApp(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "paisans.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(filepath.Join("..", "..", "examples", "secrets.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range cfg.AppNames() {
		if cfg.Apps[name].Kind == config.KindPocketID {
			continue // the identity provider has no client at itself
		}
		if cfg.Apps[name].Kind == config.KindElement {
			continue // a Matrix client authenticates at the homeserver, not here
		}
		if _, ok := secrets.OIDCClients[name]; !ok {
			t.Errorf("apps.%s (kind %s): no oidc_clients entry in examples/secrets.example.yaml",
				name, cfg.Apps[name].Kind)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

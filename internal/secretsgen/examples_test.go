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

// The example has to be loadable, not merely complete.
//
// TestExampleSecretsMatchAppSecretKeys above compares key names and nothing
// else, and that is exactly how the example shipped with "generated-at-init"
// in both S3 credential slots while this branch's own garage-key-is-malformed
// refused it. A reader following the command sequence in docs/development.md
// hit `paisans: garage-key-is-malformed: apps.docs.s3_access_key_id is
// "generated-at-init"` on the very first command.
//
// Fill is what `paisans init` runs, and it is the path that refused, so this
// runs it rather than reimplementing the check. It also asserts the second
// half of the promise: a value already set is kept, so the example's fixed
// placeholders are what come back out rather than freshly generated keys.
func TestExampleSecretsLoadAndPassEveryCheck(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "paisans.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", "examples", "secrets.example.yaml")
	secrets, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}

	before := map[string]map[string]string{}
	for _, name := range cfg.AppNames() {
		before[name] = map[string]string{}
		for _, key := range []string{"s3_access_key_id", "s3_secret_access_key"} {
			if v, ok := secrets.Apps[name][key].(string); ok {
				before[name][key] = v
			}
		}
	}

	if _, err := Fill(cfg, secrets); err != nil {
		t.Fatalf("`paisans init` refuses the example this repository ships, so the sequence in docs/development.md fails on its first command: %v", err)
	}

	for name, keys := range before {
		for key, want := range keys {
			got, _ := secrets.Apps[name][key].(string)
			if got != want {
				t.Errorf("apps.%s.%s was replaced, but nothing already set may ever be replaced", name, key)
			}
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

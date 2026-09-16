package secretsgen_test

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
)

func load(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// An empty file gets everything the configuration needs, and the result is
// usable: rendering the same deployment against the generated secrets has to
// succeed, or the generator produced values in the wrong shape.
func TestFillProducesRenderableSecrets(t *testing.T) {
	cfg := load(t)
	secrets := &config.Secrets{}

	filled, err := secretsgen.Fill(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if !filled.Changed() {
		t.Fatal("an empty file generated nothing")
	}
	if len(filled.Kept) != 0 {
		t.Fatalf("an empty file kept %d secrets", len(filled.Kept))
	}

	for _, site := range cfg.SiteNames() {
		key := secrets.Sites[site].WireGuardPrivateKey
		raw, err := base64.StdEncoding.DecodeString(key)
		if err != nil {
			t.Errorf("site %s got a key that is not base64: %v", site, err)
			continue
		}
		if len(raw) != 32 {
			t.Errorf("site %s got a %d byte key, and WireGuard keys are 32", site, len(raw))
		}
	}

	// The whole point of generating is that rendering then works.
	if _, err := render.Build(cfg, secrets); err != nil {
		t.Fatalf("rendering against generated secrets failed: %v", err)
	}
}

// Re-running init is the intended way to fill in a site or an app added later,
// so it must never replace a value. A regenerated WireGuard key breaks every
// peer that trusted the old one; a regenerated database password locks an app
// out of a role that still holds the old one.
func TestFillNeverReplacesAnExistingValue(t *testing.T) {
	cfg := load(t)
	secrets := &config.Secrets{}
	if _, err := secretsgen.Fill(cfg, secrets); err != nil {
		t.Fatal(err)
	}

	before := map[string]string{
		"superuser": secrets.Cluster.SuperuserPassword,
		"garage":    secrets.Storage.Garage.SecretAccessKey,
		"home-a-wg": secrets.Sites["home-a"].WireGuardPrivateKey,
	}
	talkBefore, _ := secrets.Apps["talk"]["database_password"].(string)

	second, err := secretsgen.Fill(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed() {
		t.Errorf("a second pass generated %v, and should have generated nothing", second.Generated)
	}
	if secrets.Cluster.SuperuserPassword != before["superuser"] {
		t.Error("the superuser password was replaced")
	}
	if secrets.Storage.Garage.SecretAccessKey != before["garage"] {
		t.Error("the object storage key was replaced")
	}
	if secrets.Sites["home-a"].WireGuardPrivateKey != before["home-a-wg"] {
		t.Error("a WireGuard key was replaced, which would break every peer that trusted the old one")
	}
	if got, _ := secrets.Apps["talk"]["database_password"].(string); got != talkBefore {
		t.Error("an app's database password was replaced")
	}
}

// A site or an app added to the configuration later is exactly the case init
// is re-run for.
func TestFillCoversWhatWasAddedLater(t *testing.T) {
	cfg := load(t)
	secrets := &config.Secrets{}
	if _, err := secretsgen.Fill(cfg, secrets); err != nil {
		t.Fatal(err)
	}

	cfg.Sites["home-c"] = config.Site{
		Roles:   []config.Role{config.RoleData, config.RoleApps},
		Address: "10.44.0.4",
		SSH:     "home-c.local",
	}
	filled, err := secretsgen.Fill(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if secrets.Sites["home-c"].WireGuardPrivateKey == "" {
		t.Fatal("a site added later got no key")
	}
	if len(filled.Generated) != 1 || filled.Generated[0] != "sites.home-c.wireguard_private_key" {
		t.Fatalf("expected exactly the new site's key, got %v", filled.Generated)
	}
}

// Secrets differ per app and per kind. WriteFreely has no database role at all,
// and only Mbin runs a broker and a cache of its own, so generating a password
// for a role nobody creates would be noise in a file an operator has to trust.
func TestFillMatchesEachKindsNeeds(t *testing.T) {
	cfg := load(t)
	secrets := &config.Secrets{}
	if _, err := secretsgen.Fill(cfg, secrets); err != nil {
		t.Fatal(err)
	}

	if _, ok := secrets.Apps["blog"]["database_password"]; ok {
		t.Error("WriteFreely was given a database password, and it has never supported Postgres")
	}
	for _, key := range []string{"database_password", "mercure_jwt_secret", "rabbitmq_password", "valkey_password"} {
		if _, ok := secrets.Apps["talk"][key]; !ok {
			t.Errorf("mbin is missing %s", key)
		}
	}
	if _, ok := secrets.Apps["docs"]["rabbitmq_password"]; ok {
		t.Error("outline was given a broker password for a broker it does not run")
	}
}

// What the toolkit will not invent has to be said out loud. A pasted DNS token
// and a minted OIDC client are both somebody else's to produce, and an install
// that looks finished without them has no certificates and no sign in.
func TestOwedSecretsAreReported(t *testing.T) {
	cfg := load(t)
	secrets := &config.Secrets{}
	filled, err := secretsgen.Fill(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	owed := map[string]string{}
	for _, o := range filled.Owed {
		owed[o.Name] = o.Why
		if o.Why == "" {
			t.Errorf("%s is owed with no reason given", o.Name)
		}
	}
	if _, ok := owed["external.acme_dns_token"]; !ok {
		t.Error("the DNS token was not reported as owed, and certificates need it")
	}
	if _, ok := owed["oidc_clients.talk"]; !ok {
		t.Error("an app's OIDC client was not reported as owed")
	}
	if _, ok := owed["oidc_clients.auth"]; ok {
		t.Error("the identity provider was told it owes itself a client")
	}
}

// A homeserver's client is registered with a redirect URI that no other kind
// uses, and an operator needs it BEFORE anything is rendered: the only other
// place it appears is a 0600 file that exists after apply, which is after they
// needed it. A client registered with the wrong one fails at the end of the
// first sign in, after the member has already authenticated.
func TestAHomeserversOwedClientNamesItsRedirectURI(t *testing.T) {
	cfg := load(t)
	secrets := &config.Secrets{}
	filled, err := secretsgen.Fill(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	var why string
	for _, o := range filled.Owed {
		if o.Name == "oidc_clients.chat" {
			why = o.Why
		}
	}
	if why == "" {
		t.Fatal("the homeserver's client was not reported as owed at all")
	}
	want := kinds.MASRedirectURI(cfg.Apps["chat"].Hostname, "chat")
	if !strings.Contains(why, want) {
		t.Errorf("the owed client does not name the redirect URI to register (%s):\n%s", want, why)
	}
	// Naming the other kinds' callback as the contrast is fine; offering it as
	// the URI to register is not.
	if strings.Contains(why, "https://"+cfg.Apps["chat"].Hostname+"/oauth/callback") {
		t.Errorf("the owed client offers the callback every other kind uses:\n%s", why)
	}
}

package secretsgen_test

import (
	"encoding/base64"
	"path/filepath"
	"regexp"
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
		"garage":    secrets.Storage.Garage.RPCSecret,
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
	if secrets.Storage.Garage.RPCSecret != before["garage"] {
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

// Secrets differ per app and per kind. The writefreely-wisp fork keeps its
// data in Postgres, and only Mbin runs a broker and a cache of its own, so
// generating a password for a role nobody creates would be noise in a file an
// operator has to trust.
func TestFillMatchesEachKindsNeeds(t *testing.T) {
	cfg := load(t)
	secrets := &config.Secrets{}
	if _, err := secretsgen.Fill(cfg, secrets); err != nil {
		t.Fatal(err)
	}

	if _, ok := secrets.Apps["blog"]["database_password"]; !ok {
		t.Error("the wisp fork keeps its data in Postgres and needs a database password")
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

// Garage refuses anything else, and it refuses it at provisioning time on a
// host rather than here, which is the worst place to discover a format.
// Established by running dxflrs/garage:v1.0.1: "The specified key ID is not a
// valid Garage key ID (starts with `GK`, followed by 12 hex-encoded bytes)".
func TestGeneratedS3CredentialsMatchGaragesFormat(t *testing.T) {
	cfg := load(t)
	secrets := &config.Secrets{}
	if _, err := secretsgen.Fill(cfg, secrets); err != nil {
		t.Fatalf("filling secrets: %v", err)
	}
	keyID := regexp.MustCompile(`^GK[0-9a-f]{24}$`)
	secret := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, name := range cfg.AppNames() {
		app := cfg.Apps[name]
		if !kinds.UsesObjectStorage(app.Kind) {
			if _, ok := secrets.Apps[name]["s3_access_key_id"]; ok {
				t.Errorf("%s is a %s and stores no objects, but was given an S3 key", name, app.Kind)
			}
			continue
		}
		id, _ := secrets.Apps[name]["s3_access_key_id"].(string)
		if !keyID.MatchString(id) {
			t.Errorf("%s's s3_access_key_id is %q, which Garage will refuse", name, id)
		}
		sec, _ := secrets.Apps[name]["s3_secret_access_key"].(string)
		if !secret.MatchString(sec) {
			t.Errorf("%s's s3_secret_access_key is not 32 hex encoded bytes", name)
		}
	}
}

// storage.garage.rpc_secret is not an opaque string, whatever the spec used to
// say. Garage parses it as a hex encoded 32 byte key and stops at startup with
// "Invalid RPC secret key: expected 32 bits of entropy" against anything else,
// so a base64 password there is a node that never comes up. admin_token is
// genuinely unconstrained and is asserted to be the base64 password it has
// always been, so that this test says which of the two is which rather than
// tightening both by accident.
func TestGarageRPCSecretIsHexAndAdminTokenIsNot(t *testing.T) {
	cfg := load(t)
	secrets := &config.Secrets{}
	if _, err := secretsgen.Fill(cfg, secrets); err != nil {
		t.Fatalf("filling secrets: %v", err)
	}
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if !hex64.MatchString(secrets.Storage.Garage.RPCSecret) {
		t.Errorf("storage.garage.rpc_secret must be 64 hex characters, which Garage refuses to start without. It is %d characters and does not match", len(secrets.Storage.Garage.RPCSecret))
	}
	if secrets.Storage.Garage.AdminToken == "" {
		t.Error("storage.garage.admin_token was not generated")
	}
	if _, err := base64.RawURLEncoding.DecodeString(secrets.Storage.Garage.AdminToken); err != nil {
		t.Errorf("storage.garage.admin_token should still be the base64 password it has always been: %v", err)
	}
}

// Nothing already set is ever replaced, and that rule has to hold for the hex
// generated secret too: a re-run of `init` that rotated rpc_secret would split
// a cluster whose other nodes still carry the old one.
func TestFillKeepsAnExistingRPCSecret(t *testing.T) {
	cfg := load(t)
	const existing = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	secrets := &config.Secrets{}
	secrets.Storage.Garage.RPCSecret = existing
	if _, err := secretsgen.Fill(cfg, secrets); err != nil {
		t.Fatalf("filling secrets: %v", err)
	}
	if secrets.Storage.Garage.RPCSecret != existing {
		t.Error("storage.garage.rpc_secret was replaced, which would split a cluster whose other nodes still hold the old one")
	}
}

// garageKeyIsMalformed: generated credentials cannot trip this, because
// garageKeyID and garageSecretKey only ever produce the accepted shape. A
// hand edited secrets file can, and the failure it prevents is a provisioning
// run that dies halfway through with a message about hex encoding, after it
// has already imported some keys. validate.Check takes only a *config.Config
// and never sees secrets, so this cannot live there: it lives here, where a
// kept (not generated) app secret is the only place the toolkit ever looks at
// a hand written key.
func TestGarageKeyIsMalformed(t *testing.T) {
	cfg := load(t)
	validSecret := strings.Repeat("ab", 32)

	cases := []struct {
		name  string
		entry map[string]any
		want  string
	}{
		{
			name: "base64 key ID",
			entry: map[string]any{
				"s3_access_key_id":     "not-a-valid-garage-key-id",
				"s3_secret_access_key": validSecret,
			},
			want: "docs.s3_access_key_id",
		},
		{
			name: "uppercase hex in the key ID",
			entry: map[string]any{
				"s3_access_key_id":     "GK" + strings.Repeat("AB", 12),
				"s3_secret_access_key": validSecret,
			},
			want: "docs.s3_access_key_id",
		},
		{
			name: "short secret key",
			entry: map[string]any{
				"s3_access_key_id":     "GK" + strings.Repeat("ab", 12),
				"s3_secret_access_key": "tooshort",
			},
			want: "docs.s3_secret_access_key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secrets := &config.Secrets{Apps: map[string]map[string]any{
				"docs": tc.entry,
			}}
			_, err := secretsgen.Fill(cfg, secrets)
			if err == nil {
				t.Fatalf("expected a malformed Garage key to be refused")
			}
			if !strings.Contains(err.Error(), "garage-key-is-malformed") {
				t.Errorf("the error should name the rule garage-key-is-malformed, got: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error should name the field %s, got: %v", tc.want, err)
			}
		})
	}
}

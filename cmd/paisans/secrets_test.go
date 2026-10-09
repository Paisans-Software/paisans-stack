package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// secretsDir is a deployment directory with the fixture configuration and a
// secrets file holding only what is given.
func secretsDir(t *testing.T, secrets string) (configPath, secretsPath string) {
	t.Helper()
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(dir, "paisans.yaml")
	secretsPath = filepath.Join(dir, "secrets.enc.yaml")
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretsPath, []byte(secrets), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, secretsPath
}

func set(t *testing.T, configPath, secretsPath, key, stdin string) (string, error) {
	t.Helper()
	var err error
	printed := captureStdout(t, func() {
		err = runSecretsSet([]string{key, "--config", configPath, "--secrets", secretsPath}, strings.NewReader(stdin))
	})
	return printed, err
}

// A pasted credential goes in through a pipe and comes out only as its name.
func TestSecretsSetWritesAnOwedKeyAndPrintsOnlyItsName(t *testing.T) {
	configPath, secretsPath := secretsDir(t, "version: 1\n")
	const token = "not-a-real-token-0001"

	printed, err := set(t, configPath, secretsPath, "external.acme_dns_token", token+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(printed, "Set external.acme_dns_token.\n") || strings.Contains(printed, token) {
		t.Errorf("printed %q, want the key and never the value", printed)
	}
	secrets, err := config.LoadSecrets(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := secrets.External["acme_dns_token"]; got != token {
		t.Errorf("stored %q, want the value less its one trailing newline", got)
	}

	// An OIDC client is owed for every app with a sign in, so it may be
	// created here too.
	if _, err := set(t, configPath, secretsPath, "oidc_clients.talk.client_secret", "s3cret"); err != nil {
		t.Errorf("an owed OIDC client secret was refused: %v", err)
	}
}

func TestSecretsSetRefuses(t *testing.T) {
	configPath, secretsPath := secretsDir(t, "version: 1\napps:\n  talk:\n    valkey_password: kept\n")
	for _, tc := range []struct {
		name, key, stdin, want string
	}{
		{"an empty value", "external.smtp_password", "\n", "nothing on stdin"},
		{"an unknown section", "nope.thing", "x", `no "nope" section`},
		{"a generated key nobody created", "apps.talk.database_password", "x", "paisans init"},
		{"a key that names no place", "cluster.root_password", "x", "neither set nor owed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			printed, err := set(t, configPath, secretsPath, tc.key, tc.stdin)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error containing %q", err, tc.want)
			}
			if printed != "" {
				t.Errorf("a refusal printed %q", printed)
			}
		})
	}

	// Replacing a key that exists is a rotation, and allowed.
	if _, err := set(t, configPath, secretsPath, "apps.talk.valkey_password", "rotated"); err != nil {
		t.Errorf("rotating an existing key was refused: %v", err)
	}

	// The value never comes from an argument.
	err := runSecretsSet([]string{"external.smtp_password", "--secrets", secretsPath, "hunter2"}, strings.NewReader(""))
	if err == nil {
		t.Error("a value given as an argument was accepted")
	}
}

// An encrypted file stays encrypted, to the recipients beside it.
func TestSecretsSetReencrypts(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	configPath, secretsPath := secretsDir(t, "")
	dir := filepath.Dir(secretsPath)
	keyFile := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(keyFile, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	sops := "creation_rules:\n  - age: " + identity.Recipient().String() + "\n"
	if err := os.WriteFile(filepath.Join(dir, config.SOPSConfigName), []byte(sops), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteSecrets(secretsPath, &config.Secrets{Version: 1}, []string{identity.Recipient().String()}); err != nil {
		t.Fatal(err)
	}

	const token = "not-a-real-token-0002"
	if _, err := set(t, configPath, secretsPath, "external.acme_dns_token", token); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("the value was written in plaintext")
	}
	secrets, err := config.LoadSecrets(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !secrets.Encrypted || secrets.External["acme_dns_token"] != token {
		t.Errorf("encrypted %v, token round tripped %v", secrets.Encrypted, secrets.External["acme_dns_token"] == token)
	}
}

// A gateway rendered without the DNS token is a Caddy that can obtain no
// certificate, which only a visitor would discover. It is refused, and the
// refusal says how to supply it.
func TestRenderRefusesAGatewayWithoutItsACMEToken(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "render", "testdata", "secrets.fixture.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	without := strings.Replace(string(fixture), "acme_dns_token:", "unused_token:", 1)
	configPath, secretsPath := secretsDir(t, without)

	out := t.TempDir()
	err = runRender([]string{"--config", configPath, "--secrets", secretsPath, "--out", out})
	if err == nil {
		t.Fatal("a gateway was rendered with no DNS token")
	}
	if !strings.Contains(err.Error(), "paisans secrets set external.acme_dns_token") {
		t.Errorf("the refusal does not say how to fix it:\n%v", err)
	}

	// apply of a site without the gateway role does not need the token, and
	// is stopped later by ssh rather than by this check.
	if err := requireACMEToken(mustLoad(t, configPath), mustSecrets(t, secretsPath), []string{"home-a"}); err != nil {
		t.Errorf("a site with no gateway was refused for the gateway's token: %v", err)
	}
	if err := requireACMEToken(mustLoad(t, configPath), mustSecrets(t, secretsPath), []string{"vm"}); err == nil {
		t.Error("apply of the gateway site was not refused")
	}
	// The monitor runs the same Caddy for its own hostname.
	if err := requireACMEToken(mustLoad(t, configPath), mustSecrets(t, secretsPath), []string{"watch"}); err == nil || !strings.Contains(err.Error(), "watch runs the toolkit's Caddy") {
		t.Errorf("apply of a monitor in ingress mode paisans was not refused: %v", err)
	}
}

func mustLoad(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func mustSecrets(t *testing.T, path string) *config.Secrets {
	t.Helper()
	s, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// writeFixtureSecrets is a deployment directory with the fixture
// configuration and the fixture secrets, changed by edit.
func writeFixtureSecrets(t *testing.T, edit func(*config.Secrets)) (configPath, secretsPath string) {
	t.Helper()
	configPath, secretsPath = secretsDir(t, "")
	secrets, err := config.LoadSecrets(fixtureSecretsPath())
	if err != nil {
		t.Fatal(err)
	}
	edit(secrets)
	if err := config.WriteSecrets(secretsPath, secrets, nil); err != nil {
		t.Fatal(err)
	}
	return configPath, secretsPath
}

func TestSecretsPruneListsThenRemovesOnlyOrphans(t *testing.T) {
	configPath, secretsPath := writeFixtureSecrets(t, func(s *config.Secrets) {
		if s.Sites == nil {
			s.Sites = map[string]config.SiteSecrets{}
		}
		s.Sites["monitor-a"] = config.SiteSecrets{WireGuardPrivateKey: "x"}
		if s.OIDCClients == nil {
			s.OIDCClients = map[string]config.OIDCClient{}
		}
		s.OIDCClients["uptime"] = config.OIDCClient{ClientID: "abc", ClientSecret: "do-not-print"}
	})
	out := captureStdout(t, func() {
		if err := runSecretsPrune([]string{"--config", configPath, "--secrets", secretsPath}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"sites.monitor-a", "oidc_clients.uptime", "still exists at Pocket ID"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "do-not-print") {
		t.Fatal("a secret value was printed")
	}
	if s, _ := config.LoadSecrets(secretsPath); s.Sites["monitor-a"].WireGuardPrivateKey == "" {
		t.Fatal("the dry run changed the file")
	}
	captureStdout(t, func() {
		if err := runSecretsPrune([]string{"--config", configPath, "--secrets", secretsPath, "--execute"}); err != nil {
			t.Fatal(err)
		}
	})
	s, err := config.LoadSecrets(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Sites["monitor-a"]; ok {
		t.Error("sites.monitor-a is still there")
	}
	if _, ok := s.OIDCClients["uptime"]; ok {
		t.Error("oidc_clients.uptime is still there")
	}
	if _, ok := s.Sites["home-a"]; !ok {
		t.Error("a declared site's secrets were pruned")
	}
}

// An encrypted file with no recipient beside it is refused rather than
// written back in plaintext.
func TestSecretsPruneNeverDecryptsTheFile(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	configPath, secretsPath := secretsDir(t, "")
	keyFile := filepath.Join(filepath.Dir(secretsPath), "key.txt")
	if err := os.WriteFile(keyFile, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	secrets := &config.Secrets{Version: 1, Sites: map[string]config.SiteSecrets{"monitor-a": {WireGuardPrivateKey: "x"}}}
	if err := config.WriteSecrets(secretsPath, secrets, []string{identity.Recipient().String()}); err != nil {
		t.Fatal(err)
	}
	var runErr error
	captureStdout(t, func() {
		runErr = runSecretsPrune([]string{"--config", configPath, "--secrets", secretsPath, "--execute"})
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "plaintext") {
		t.Fatalf("err = %v", runErr)
	}
	if s, _ := config.LoadSecrets(secretsPath); !s.Encrypted || s.Sites["monitor-a"].WireGuardPrivateKey == "" {
		t.Error("the file was changed")
	}
}

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
	if printed != "set external.acme_dns_token\n" {
		t.Errorf("printed %q, want only the key", printed)
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

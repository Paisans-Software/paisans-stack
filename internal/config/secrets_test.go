package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/keys"
	sopsyaml "github.com/getsops/sops/v3/stores/yaml"
	"github.com/getsops/sops/v3/version"

	"github.com/josephquigley/paisans-stack/internal/config"
)

const plaintextSecrets = `version: 1
cluster:
    superuser_password: fixture-not-a-secret-superuser
    admin_password: fixture-not-a-secret-admin
    standby_password: fixture-not-a-secret-standby
sites:
    home-a:
        wireguard_private_key: QEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEA=
external:
    cloudflare_api_token: fixture-not-a-secret-cloudflare
`

// A plaintext file is a legitimate input for fixtures and examples, and the
// caller is told so it can say it out loud.
func TestPlaintextSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.yaml")
	if err := os.WriteFile(path, []byte(plaintextSecrets), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	if secrets.Encrypted {
		t.Fatal("a plaintext file was reported as encrypted")
	}
	if secrets.Cluster.AdminPassword != "fixture-not-a-secret-admin" {
		t.Fatalf("the admin password did not load: %q", secrets.Cluster.AdminPassword)
	}
}

// sops and age are embedded rather than shelled out to: neither binary is a
// dependency of a workstation. This proves the whole path, from an encrypted
// file on disk to values in memory, with no process started.
func TestSOPSEncryptedSecrets(t *testing.T) {
	dir := t.TempDir()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "keys.txt")
	if err := os.WriteFile(keyFile, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)

	path := filepath.Join(dir, "secrets.enc.yaml")
	if err := os.WriteFile(path, encryptForTest(t, identity), 0o600); err != nil {
		t.Fatal(err)
	}

	secrets, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	if !secrets.Encrypted {
		t.Fatal("an encrypted file was not reported as encrypted")
	}
	if secrets.Cluster.SuperuserPassword != "fixture-not-a-secret-superuser" {
		t.Fatalf("decryption produced %q", secrets.Cluster.SuperuserPassword)
	}
	if secrets.External["cloudflare_api_token"] != "fixture-not-a-secret-cloudflare" {
		t.Fatal("a pasted secret did not survive the round trip")
	}
}

// A file altered after it was encrypted is refused. These values become a
// running deployment's credentials, so a decrypt that ignores the
// authentication code reads a tampered file happily.
func TestTamperedSecretsAreRefused(t *testing.T) {
	dir := t.TempDir()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "keys.txt")
	if err := os.WriteFile(keyFile, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)

	encrypted := string(encryptForTest(t, identity))
	// Swap two encrypted values, which a MAC check catches and a naive
	// decrypt does not.
	tampered := strings.Replace(encrypted, "superuser_password", "superuser_password_x", 1)
	path := filepath.Join(dir, "secrets.enc.yaml")
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadSecrets(path); err == nil {
		t.Fatal("a modified file was accepted")
	}
}

// No age identity means a clear instruction, not a stack trace.
func TestMissingKeyIsExplained(t *testing.T) {
	dir := t.TempDir()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "secrets.enc.yaml")
	if err := os.WriteFile(path, encryptForTest(t, identity), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", filepath.Join(dir, "absent.txt"))
	_, err = config.LoadSecrets(path)
	if err == nil {
		t.Fatal("a file we hold no key for was decrypted")
	}
	if !strings.Contains(err.Error(), "SOPS_AGE_KEY_FILE") {
		t.Fatalf("the error does not say what to do:\n%v", err)
	}
}

// encryptForTest builds an encrypted fixture in memory. No encrypted file is
// ever committed: this makes one, uses it, and throws it away with the temp
// directory.
func encryptForTest(t *testing.T, identity *age.X25519Identity) []byte {
	t.Helper()
	store := &sopsyaml.Store{}
	branches, err := store.LoadPlainFile([]byte(plaintextSecrets))
	if err != nil {
		t.Fatal(err)
	}
	masterKeys, err := sopsage.MasterKeysFromRecipients(identity.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}
	var group sops.KeyGroup
	for _, key := range masterKeys {
		group = append(group, keys.MasterKey(key))
	}
	tree := sops.Tree{
		Branches: branches,
		Metadata: sops.Metadata{
			KeyGroups:    []sops.KeyGroup{group},
			Version:      version.Version,
			LastModified: time.Unix(0, 0).UTC(),
		},
	}
	dataKey, errs := tree.GenerateDataKey()
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	cipher := aes.NewCipher()
	mac, err := tree.Encrypt(dataKey, cipher)
	if err != nil {
		t.Fatal(err)
	}
	encryptedMAC, err := cipher.Encrypt(mac, dataKey, tree.Metadata.LastModified.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	tree.Metadata.MessageAuthenticationCode = encryptedMAC
	out, err := store.EmitEncryptedFile(tree)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Writing is the other half of the path that reading already covers: a file
// this toolkit encrypts must be one it, and sops itself, can decrypt again.
// Nothing encrypted is ever committed, so the fixture is made and thrown away.
func TestWriteSecretsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "keys.txt")
	if err := os.WriteFile(keyFile, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)

	path := filepath.Join(dir, "secrets.enc.yaml")
	original := &config.Secrets{
		Version: 1,
		Cluster: config.ClusterSecrets{SuperuserPassword: "written-not-a-secret"},
		Sites: map[string]config.SiteSecrets{
			"home-a": {WireGuardPrivateKey: "QEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEA="},
		},
		External: map[string]string{"cloudflare_api_token": "written-not-a-secret-token"},
	}
	if err := config.WriteSecrets(path, original, []string{identity.Recipient().String()}); err != nil {
		t.Fatal(err)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "written-not-a-secret") {
		t.Fatal("a value was written in the clear to a file that claims to be encrypted")
	}

	back, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Encrypted {
		t.Error("the file we wrote was not recognised as encrypted")
	}
	if back.Cluster.SuperuserPassword != original.Cluster.SuperuserPassword {
		t.Error("a generated password did not survive the round trip")
	}
	if back.Sites["home-a"].WireGuardPrivateKey != original.Sites["home-a"].WireGuardPrivateKey {
		t.Error("a WireGuard key did not survive the round trip")
	}
	if back.External["cloudflare_api_token"] != original.External["cloudflare_api_token"] {
		t.Error("a pasted secret did not survive the round trip")
	}
}

// Without an age recipient the file is written in plaintext rather than not at
// all: refusing would leave an operator holding generated secrets that went
// nowhere. The caller says so loudly, and this test pins the behaviour so that
// nobody quietly changes which of the two it is.
func TestWriteSecretsWithoutRecipientsIsPlaintext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.yaml")
	if err := config.WriteSecrets(path, &config.Secrets{Version: 1}, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("a plaintext secrets file is %04o, not 0600", info.Mode().Perm())
	}
	back, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Encrypted {
		t.Error("a plaintext file reported itself as encrypted")
	}
}

// Recipients come from a committed .sops.yaml beside the secrets, so adding an
// admin is an edit plus a re-encrypt rather than a rotation of every secret.
func TestRecipientsFromSOPSConfig(t *testing.T) {
	dir := t.TempDir()
	body := "creation_rules:\n" +
		"  - path_regex: secrets\\.enc\\.yaml$\n" +
		"    age: age1one,age1two\n" +
		"  - age: age1two\n"
	if err := os.WriteFile(filepath.Join(dir, config.SOPSConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := config.Recipients(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"age1one", "age1two"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	// No file at all is an ordinary state, not an error: it is what a first
	// look at the tool has.
	empty, err := config.Recipients(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("a directory with no %s returned %v", config.SOPSConfigName, empty)
	}
}

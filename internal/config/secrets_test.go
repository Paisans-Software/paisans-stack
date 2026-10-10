package config_test

import (
	"errors"
	"io/fs"
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

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/ui"
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
	var p *ui.Problem
	if !errors.As(err, &p) {
		t.Fatalf("not a ui.Problem: %v", err)
	}
	if p.Hint != "secrets.enc.yaml cannot be decrypted: no usable age key was found" && !strings.HasSuffix(p.Hint, "/secrets.enc.yaml cannot be decrypted: no usable age key was found") {
		t.Errorf("hint: %q", p.Hint)
	}
	if !strings.Contains(p.Explain, "SOPS_AGE_KEY_FILE") {
		t.Fatalf("the explanation does not say what to do:\n%v", p.Explain)
	}
	// The embedded sops (v3.13.3, age/keysource.go) also reads a key from a
	// command, which is how a key kept in a keychain is used.
	// SOPS_AGE_KEY_CMD first, then every other place sops reads a key from.
	cmd, file, env, def := strings.Index(p.Explain, "SOPS_AGE_KEY_CMD"), strings.Index(p.Explain, "SOPS_AGE_KEY_FILE"), strings.Index(p.Explain, "SOPS_AGE_KEY "), strings.Index(p.Explain, "sops/age/keys.txt")
	if cmd < 0 || file < cmd || env < cmd || def < cmd {
		t.Errorf("the explanation does not name every way a key is found, SOPS_AGE_KEY_CMD first:\n%v", p.Explain)
	}
	if !strings.Contains(p.Explain, "Set SOPS_AGE_KEY_CMD") {
		t.Errorf("the explanation does not mention SOPS_AGE_KEY_CMD:\n%v", p.Explain)
	}
	// sops' own words and the full path are the cause, for -v.
	if p.Cause == nil || !strings.Contains(p.Cause.Error(), "Error getting data key") || !strings.Contains(p.Cause.Error(), path) {
		t.Errorf("the cause lacks sops' error or the path: %v", p.Cause)
	}
	if len(p.Hint) > 80 && !strings.Contains(p.Hint, "/") {
		t.Errorf("hint longer than 80: %q", p.Hint)
	}
}

// A secrets file that is not there is said plainly, and is still
// fs.ErrNotExist to a caller that starts from nothing in that case.
func TestMissingSecretsFileIsAProblem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc.yaml")
	_, err := config.LoadSecrets(path)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("not fs.ErrNotExist: %v", err)
	}
	var p *ui.Problem
	if !errors.As(err, &p) || !strings.HasSuffix(p.Hint, "secrets.enc.yaml does not exist") || !strings.Contains(p.Explain, "paisans init") {
		t.Errorf("got %#v", p)
	}
}

// A secrets file at a version this toolkit does not read says so.
func TestSecretsVersionIsAProblem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc.yaml")
	if err := os.WriteFile(path, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.LoadSecrets(path)
	var p *ui.Problem
	if !errors.As(err, &p) || !strings.Contains(p.Hint, "version 2") {
		t.Errorf("got %v", err)
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

// A write that fails partway leaves the file as it was, and nothing beside
// it: the new contents go to a temporary file renamed over it once synced.
func TestWriteSecretsFailureLeavesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.enc.yaml")
	if err := config.WriteSecrets(path, &config.Secrets{Version: 1, External: map[string]string{"a": "kept-value-0001"}}, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	restore := config.SetSyncFile(func(interface{ Sync() error }) error { return errors.New("disk full") })
	defer restore()
	if err := config.WriteSecrets(path, &config.Secrets{Version: 1}, nil); err == nil {
		t.Fatal("a failed sync was not an error")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a failed write changed the file")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("left beside it: %v", entries)
	}
}

// A file that cannot be written is refused and left as it was, as writing it
// in place would be, although its directory could take a new file.
func TestWriteSecretsRefusesAReadOnlyFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read only file")
	}
	path := filepath.Join(t.TempDir(), "secrets.enc.yaml")
	if err := config.WriteSecrets(path, &config.Secrets{Version: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := config.WriteSecrets(path, &config.Secrets{Version: 1, External: map[string]string{"a": "b"}}, nil); err == nil {
		t.Error("a read only file was written")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a read only file changed")
	}
}

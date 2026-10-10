package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/ui"
	"gopkg.in/yaml.v3"
)

// dropGenerated takes four generated secrets out of the fixture's: the
// dry run is to generate exactly these.
func dropGenerated(s *config.Secrets) {
	s.Cluster.AdminPassword = ""
	s.Cluster.StandbyPassword = ""
	s.Storage.Garage.AdminToken = ""
	site := s.Sites["home-a"]
	site.HeartbeatToken = ""
	s.Sites["home-a"] = site
}

// encryptedDeployment is a deployment directory with the fixture
// configuration and the fixture secrets, changed by edit and encrypted to a
// test age key named in the .sops.yaml beside them, which this test can
// decrypt with.
func encryptedDeployment(t *testing.T, edit func(*config.Secrets)) (configPath, secretsPath string) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	configPath, secretsPath = secretsDir(t, "")
	dir := filepath.Dir(secretsPath)
	keyFile := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(keyFile, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	sops := "creation_rules:\n  - age: " + identity.Recipient().String() + "\n"
	if err := os.WriteFile(filepath.Join(dir, config.SOPSConfigName), []byte(sops), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(fixtureSecretsPath())
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(secrets)
	}
	if err := config.WriteSecrets(secretsPath, secrets, []string{identity.Recipient().String()}); err != nil {
		t.Fatal(err)
	}
	return configPath, secretsPath
}

// dryRun is apply without --site and without --execute on configPath, every
// step's check and host read faked, reported to a recorder. It returns the
// recorder, what reached stdout, and the steps checked.
func dryRun(t *testing.T, configPath, secretsPath string) (*ui.Recorder, string, []string) {
	t.Helper()
	fakeConverge(t, nil)
	checked := fakeChecks(t, nil)
	rec := recordConverge(t)
	var err error
	args := []string{"--config", configPath, "--sudo=false"}
	if secretsPath != "" {
		args = append(args, "--secrets", secretsPath)
	}
	out := captureStdout(t, func() { err = runApply(args) })
	if err != nil {
		t.Fatal(err)
	}
	return rec, out, *checked
}

// initLine is init's line as "mark: result".
func initLine(rec *ui.Recorder) string {
	for _, e := range rec.Events {
		if e.Text == "init" {
			mark := map[string]string{"done": "✓", "pending": "○", "waiting": "·", "fail": "✗"}[e.Kind]
			if mark != "" {
				return mark + " " + e.Extra
			}
		}
	}
	return ""
}

func yamlOf(t *testing.T, s *config.Secrets) string {
	t.Helper()
	out, err := yaml.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// every value in s, as text, for a test that none was printed.
func secretValues(s *config.Secrets) []string {
	var out []string
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.String:
			if len(v.String()) >= 8 {
				out = append(out, v.String())
			}
		case reflect.Struct:
			for i := range v.NumField() {
				// Path and the like are not in the file.
				if f := v.Type().Field(i); f.IsExported() && f.Tag.Get("yaml") != "-" {
					walk(v.Field(i))
				}
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				walk(v.MapIndex(k))
			}
		case reflect.Interface, reflect.Pointer:
			if !v.IsNil() {
				walk(v.Elem())
			}
		}
	}
	walk(reflect.ValueOf(*s))
	return out
}

// A dry run whose secrets file lacks generated secrets writes them, encrypted
// as before; every value that was there is kept; init's line is done and
// names them without a value; and the later steps are checked.
func TestConvergeDryRunGeneratesMissingSecrets(t *testing.T) {
	configPath, secretsPath := encryptedDeployment(t, dropGenerated)
	before := mustSecrets(t, secretsPath)

	rec, out, checked := dryRun(t, configPath, secretsPath)

	after := mustSecrets(t, secretsPath)
	if !after.Encrypted {
		t.Error("the secrets file was written unencrypted")
	}
	if after.Cluster.AdminPassword == "" || after.Cluster.StandbyPassword == "" || after.Storage.Garage.AdminToken == "" || after.Sites["home-a"].HeartbeatToken == "" {
		t.Errorf("a missing generated secret was not written: %+v %+v", after.Cluster, after.Sites["home-a"])
	}
	// Every value that was there is there still.
	kept := *after
	kept.Cluster.AdminPassword, kept.Cluster.StandbyPassword, kept.Storage.Garage.AdminToken = "", "", ""
	site := kept.Sites["home-a"]
	site.HeartbeatToken = ""
	sites := map[string]config.SiteSecrets{}
	for k, v := range kept.Sites {
		sites[k] = v
	}
	sites["home-a"] = site
	kept.Sites = sites
	kept.Path, kept.Encrypted = before.Path, before.Encrypted
	// Fill gives an app with no generated secret an empty entry, as init
	// does; that holds no value.
	apps := map[string]map[string]any{}
	for k, v := range kept.Apps {
		if _, was := before.Apps[k]; was || len(v) > 0 {
			apps[k] = v
		}
	}
	kept.Apps = apps
	if a, b := yamlOf(t, &kept), yamlOf(t, before); a != b {
		t.Errorf("a secret that had a value was changed:\n%s\nwas:\n%s", a, b)
	}

	want := "✓ generated 4 secrets into secrets.enc.yaml: cluster.admin_password, cluster.standby_password, sites.home-a.heartbeat_token and 1 more"
	if got := initLine(rec); got != want {
		t.Errorf("init's line:\n%s\nwant:\n%s", got, want)
	}
	printed := rec.Lines() + out
	for _, v := range secretValues(after) {
		if strings.Contains(printed, v) {
			t.Fatalf("a secret's value was printed:\n%s", printed)
		}
	}
	if len(checked) == 0 {
		t.Error("no later step was checked")
	}
	for _, l := range statuses(rec) {
		if !strings.HasPrefix(l, "✓ ") {
			t.Errorf("a later step is not checked: %s", l)
		}
	}
}

// The closing line says nothing changed on the servers, and that the secrets
// were added, rather than that nothing changed.
func TestConvergeDryRunSaysTheSecretsWereAdded(t *testing.T) {
	configPath, secretsPath := encryptedDeployment(t, dropGenerated)
	rec, _, _ := dryRun(t, configPath, secretsPath)
	i := rec.Index("result", "")
	if i < 0 {
		t.Fatalf("no closing line:\n%s", rec.Lines())
	}
	got := rec.Events[i].Text
	if !strings.HasPrefix(got, "Nothing changed on the servers. Added 4 generated secrets to secrets.enc.yaml. Re-run with --execute to apply.") {
		t.Errorf("closing line: %s", got)
	}
}

// With nothing to generate, the closing line is as it was.
func TestConvergeDryRunWithEverySecretSaysNothingChanged(t *testing.T) {
	configPath, secretsPath := encryptedDeployment(t, nil)
	raw, _ := os.ReadFile(secretsPath)
	rec, _, _ := dryRun(t, configPath, secretsPath)
	if got := rec.Events[rec.Index("result", "")].Text; !strings.HasPrefix(got, "Nothing changed. Re-run with --execute") {
		t.Errorf("closing line: %s", got)
	}
	if now, _ := os.ReadFile(secretsPath); !bytes.Equal(raw, now) {
		t.Error("a complete secrets file was rewritten")
	}
	if initLine(rec) != "" {
		t.Errorf("init was shown: %s", initLine(rec))
	}
}

// A write that fails marks init ✗ with the problem, says what to do, and the
// later steps wait on init. The file is as it was.
func TestConvergeDryRunSecretsWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read only file")
	}
	configPath, secretsPath := encryptedDeployment(t, dropGenerated)
	if err := os.Chmod(secretsPath, 0o400); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(secretsPath)

	rec, out, checked := dryRun(t, configPath, secretsPath)

	got := initLine(rec)
	if !strings.HasPrefix(got, "✗ ") || !strings.Contains(got, "secrets.enc.yaml") {
		t.Errorf("init's line: %s", got)
	}
	if rec.Index("note", "before init can run") < 0 {
		t.Errorf("the failure does not say what to do:\n%s", rec.Lines())
	}
	lines := statuses(rec)
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "✗ init: ") {
		t.Fatalf("statuses: %v", lines)
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "· ") || !strings.HasSuffix(l, ": after init") {
			t.Errorf("does not wait on init: %s", l)
		}
	}
	if len(checked) != 0 {
		t.Errorf("checked %v", checked)
	}
	if now, _ := os.ReadFile(secretsPath); !bytes.Equal(raw, now) {
		t.Error("the secrets file changed")
	}
	if got := rec.Events[rec.Index("result", "")].Text; !strings.HasPrefix(got, "Nothing changed. Re-run with --execute") {
		t.Errorf("closing line: %s", got)
	}
	if strings.Contains(out+rec.Lines(), "generated 4") {
		t.Error("a failed write was reported as written")
	}
	for _, v := range secretValues(mustSecrets(t, secretsPath)) {
		if strings.Contains(out+rec.Lines(), v) {
			t.Fatalf("a secret's value was printed: %q", v)
		}
	}
}

// noSubnet is an encrypted deployment lacking generated secrets whose
// configuration declares no mesh subnet.
func noSubnet(t *testing.T) (string, string) {
	t.Helper()
	configPath, secretsPath := encryptedDeployment(t, dropGenerated)
	data, _ := os.ReadFile(configPath)
	body := strings.Replace(string(data), "  subnet: 10.44.0.0/24\n", "", 1)
	if body == string(data) {
		t.Fatal("the fixture has no subnet to remove")
	}
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, secretsPath
}

// What init has to do besides generated secrets stays init's: the dry run
// plans it as pending and writes no file.
func TestConvergeDryRunLeavesTheRestToInit(t *testing.T) {
	noFile := func(t *testing.T) (string, string) {
		configPath, secretsPath := encryptedDeployment(t, nil)
		if err := os.Remove(secretsPath); err != nil {
			t.Fatal(err)
		}
		return configPath, secretsPath
	}
	noRecipients := func(t *testing.T) (string, string) {
		configPath, secretsPath := encryptedDeployment(t, dropGenerated)
		if err := os.Remove(filepath.Join(filepath.Dir(secretsPath), config.SOPSConfigName)); err != nil {
			t.Fatal(err)
		}
		return configPath, secretsPath
	}
	for name, world := range map[string]func(*testing.T) (string, string){
		"no secrets file":                 noFile,
		"encrypted, no .sops.yaml beside": noRecipients,
	} {
		t.Run(name, func(t *testing.T) {
			configPath, secretsPath := world(t)
			files := snapshot(t, filepath.Dir(configPath))
			rec, _, checked := dryRun(t, configPath, secretsPath)
			got := initLine(rec)
			if !strings.HasPrefix(got, "○ ") {
				t.Errorf("init's line: %s", got)
			}
			if strings.HasPrefix(name, "encrypted") && !strings.Contains(got, "plaintext") {
				t.Errorf("init's line does not say why: %s", got)
			}
			if len(checked) != 0 {
				t.Errorf("checked %v", checked)
			}
			if now := snapshot(t, filepath.Dir(configPath)); !reflect.DeepEqual(files, now) {
				t.Error("a file was written")
			}
			if got := rec.Events[rec.Index("result", "")].Text; !strings.HasPrefix(got, "Nothing changed. Re-run with --execute") {
				t.Errorf("closing line: %s", got)
			}
		})
	}
}

// With no subnet the configuration does not load, as for every command:
// apply stops, pointing at init, and no file is written.
func TestConvergeDryRunWithoutASubnetWritesNothing(t *testing.T) {
	configPath, secretsPath := noSubnet(t)
	files := snapshot(t, filepath.Dir(configPath))
	fakeConverge(t, nil)
	var err error
	captureStdout(t, func() {
		err = runApply([]string{"--config", configPath, "--secrets", secretsPath, "--sudo=false"})
	})
	if err == nil || !strings.Contains(err.Error(), "paisans init") {
		t.Errorf("err = %v, want it to point at init", err)
	}
	if now := snapshot(t, filepath.Dir(configPath)); !reflect.DeepEqual(files, now) {
		t.Error("a file was written")
	}
}

// With no id, init is pending alone and no file is written.
func TestConvergeDryRunWithoutAnIDWritesNothing(t *testing.T) {
	path := noID(t)
	files := snapshot(t, filepath.Dir(path))
	rec, _, _ := dryRun(t, path, "")
	if got := initLine(rec); !strings.HasPrefix(got, "○ ") {
		t.Errorf("init's line: %s", got)
	}
	if now := snapshot(t, filepath.Dir(path)); !reflect.DeepEqual(files, now) {
		t.Error("a file was written")
	}
}

// snapshot is every file in dir, by name, with its contents.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

// A --execute after the dry run has nothing left for init: it is not run, and
// filling the file again generates nothing.
func TestConvergeExecuteAfterTheDryRunFillsNothingMore(t *testing.T) {
	configPath, secretsPath := encryptedDeployment(t, dropGenerated)
	dryRun(t, configPath, secretsPath)
	raw, _ := os.ReadFile(secretsPath)

	ran := fakeConverge(t, nil)
	var err error
	captureStdout(t, func() {
		err = runApply([]string{"--config", configPath, "--secrets", secretsPath, "--sudo=false", "--execute"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*ran) == 0 || (*ran)[0] == "init" {
		t.Errorf("ran %v", *ran)
	}
	filled, _, err := fillSecrets(mustLoad(t, configPath), secretsPath, mustSecrets(t, secretsPath))
	if err != nil || filled.Changed() {
		t.Errorf("init would fill %v, %v", filled.Generated, err)
	}
	if now, _ := os.ReadFile(secretsPath); !bytes.Equal(raw, now) {
		t.Error("the secrets file changed")
	}
}

// A secrets file holding a key the toolkit does not read is left to init,
// since writing it would drop that key: init is pending, saying why, and the
// file is as it was.
func TestConvergeDryRunLeavesAFileWithUnknownKeysToInit(t *testing.T) {
	secrets, err := config.LoadSecrets(fixtureSecretsPath())
	if err != nil {
		t.Fatal(err)
	}
	dropGenerated(secrets)
	body, err := yaml.Marshal(secrets)
	if err != nil {
		t.Fatal(err)
	}
	configPath, secretsPath := secretsDir(t, string(body)+"kept_by_hand: not-a-secret\n")
	files := snapshot(t, filepath.Dir(configPath))
	rec, _, checked := dryRun(t, configPath, secretsPath)
	if got := initLine(rec); !strings.HasPrefix(got, "○ ") || !strings.Contains(got, "keys this toolkit does not read") {
		t.Errorf("init's line: %s", got)
	}
	if len(checked) != 0 {
		t.Errorf("checked %v", checked)
	}
	if now := snapshot(t, filepath.Dir(configPath)); !reflect.DeepEqual(files, now) {
		t.Error("a file was written")
	}
}

// The file --secrets names is the one filled, encrypted to the .sops.yaml
// beside it; nothing is written beside the configuration.
func TestConvergeDryRunFillsTheFileSecretsNames(t *testing.T) {
	configPath, inPlace := encryptedDeployment(t, dropGenerated)
	elsewhere := filepath.Join(t.TempDir(), "other.enc.yaml")
	if err := os.Rename(inPlace, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(filepath.Dir(inPlace), config.SOPSConfigName), filepath.Join(filepath.Dir(elsewhere), config.SOPSConfigName)); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := dryRun(t, configPath, elsewhere)
	if got := initLine(rec); !strings.HasPrefix(got, "✓ generated 4 secrets into other.enc.yaml") {
		t.Errorf("init's line: %s", got)
	}
	after := mustSecrets(t, elsewhere)
	if !after.Encrypted || after.Cluster.AdminPassword == "" {
		t.Errorf("the named file was not filled, encrypted %t", after.Encrypted)
	}
	if _, err := os.Stat(inPlace); err == nil {
		t.Error("a secrets file was written beside the configuration")
	}
}

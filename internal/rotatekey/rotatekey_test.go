package rotatekey_test

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/rotatekey"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
)

// The fixture's talk key, and the pair every test rotates to.
const (
	oldID     = "GK001122334455001122334455"
	oldSecret = "00112233445566778899aabbccddeeff000112233445566778899aabbccddeef"
	newID     = "GKfeedfacefeedfacefeedface"
	newSecret = "feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"
	bucket    = "talk-uploads"
)

// world is one Garage cluster, the sites running talk, the secrets file and
// a log of everything done to any of them, in order.
type world struct {
	t *testing.T
	// keys maps a live key's ID to its secret; ever holds every ID Garage
	// has seen, deleted or not, because it never lets one be imported again.
	keys    map[string]string
	ever    map[string]bool
	granted map[string]bool
	objects map[string]string
	// running is the key ID each site's talk was last started with.
	running map[string]string
	saved   *config.Secrets
	saves   int
	log     []string
	// fail names one action that fails once, then works.
	fail string
}

func newWorld(t *testing.T) *world {
	return &world{
		t:       t,
		keys:    map[string]string{oldID: oldSecret},
		ever:    map[string]bool{oldID: true},
		granted: map[string]bool{oldID: true},
		objects: map[string]string{},
		running: map[string]string{"home-a": oldID, "home-b": oldID},
	}
}

func (w *world) failing(action string) error {
	if w.fail == action {
		w.fail = ""
		return fmt.Errorf("%s failed", action)
	}
	return nil
}

// site is one host's transport into the world.
type site struct {
	w    *world
	name string
}

var importCmd = regexp.MustCompile(`key import (\S+) (\S+) --yes -n (\S+)$`)

func (s site) Run(command string) (string, error) {
	w := s.w
	w.log = append(w.log, s.name+": "+command)
	args := command[strings.Index(command, "/garage ")+len("/garage "):]
	switch {
	case strings.HasPrefix(args, "key info "):
		id := strings.TrimPrefix(args, "key info ")
		if _, ok := w.keys[id]; ok {
			return "Key name: talk\nKey ID: " + id + "\n", nil
		}
		return "Error: 0 matching keys\n", errors.New("exit status 1")
	case strings.HasPrefix(args, "key import "):
		if err := w.failing("import"); err != nil {
			// Garage quotes the command back, as the transport's error does.
			return "Error: " + command, err
		}
		m := importCmd.FindStringSubmatch(args)
		if m == nil {
			w.t.Fatalf("malformed import: %s", args)
		}
		if w.ever[m[1]] {
			return "Error: Key " + m[1] + " already exists in data store", errors.New("exit status 1")
		}
		w.keys[m[1]], w.ever[m[1]] = m[2], true
		return "Key name: talk\n", nil
	case strings.HasPrefix(args, "bucket info "):
		var b strings.Builder
		b.WriteString("Bucket: 0f1e\n\nWebsite access: true\n\nAuthorized keys:\n")
		for id := range w.granted {
			fmt.Fprintf(&b, "  RWO  %s  talk\n", id)
		}
		return b.String(), nil
	case strings.HasPrefix(args, "bucket allow --read --write --owner "+bucket+" --key "):
		if err := w.failing("grant"); err != nil {
			return "", err
		}
		w.granted[strings.TrimPrefix(args, "bucket allow --read --write --owner "+bucket+" --key ")] = true
		return "", nil
	case strings.HasPrefix(args, "key delete --yes "):
		if err := w.failing("delete"); err != nil {
			return "", err
		}
		id := strings.TrimPrefix(args, "key delete --yes ")
		delete(w.keys, id)
		delete(w.granted, id)
		return "Key " + id + " was deleted successfully.\n", nil
	}
	w.t.Fatalf("unexpected command on %s: %s", s.name, command)
	return "", nil
}

var curlUser = regexp.MustCompile(`user = "([^:]+):([^"]+)"`)
var curlRequest = regexp.MustCompile(`request = "([A-Z]+)"`)
var curlBody = regexp.MustCompile(`data-binary = "([^"]*)"`)

func (s site) RunInput(command, stdin string) (string, error) {
	w := s.w
	if command != "curl -fsS --max-time 20 -K -" {
		w.t.Fatalf("unexpected input command: %s", command)
	}
	user := curlUser.FindStringSubmatch(stdin)
	method := curlRequest.FindStringSubmatch(stdin)[1]
	w.log = append(w.log, fmt.Sprintf("%s: curl %s as %s", s.name, method, user[1]))
	if err := w.failing("probe"); err != nil {
		return "403 for " + user[2], err
	}
	if w.keys[user[1]] != user[2] || !w.granted[user[1]] {
		return "403", errors.New("exit status 22")
	}
	switch method {
	case "PUT":
		w.objects["probe"] = curlBody.FindStringSubmatch(stdin)[1]
	case "GET":
		return w.objects["probe"], nil
	case "DELETE":
		delete(w.objects, "probe")
	}
	return "", nil
}

func (s site) ReadFile(string) (string, bool, error)  { return "", false, nil }
func (s site) WriteFile(string, string, uint32) error { return nil }
func (s site) Describe() string                       { return s.name }

// switcher is `apply --only talk`: a site is pending while its talk runs a key
// other than the one the secrets name.
type switcher struct{ w *world }

func (sw switcher) Pending(name string, secrets *config.Secrets) ([]string, error) {
	want, _ := secrets.Apps["talk"]["s3_access_key_id"].(string)
	if sw.w.running[name] != want {
		return []string{"update srv/paisans/f2a9/talk/.env", "recreate talk"}, nil
	}
	return nil, nil
}

func (sw switcher) Apply(name string, secrets *config.Secrets) error {
	sw.w.log = append(sw.w.log, name+": apply --only talk")
	if err := sw.w.failing("apply " + name); err != nil {
		return err
	}
	sw.w.running[name], _ = secrets.Apps["talk"]["s3_access_key_id"].(string)
	return nil
}

func loadFixture(t *testing.T) (*config.Config, *config.Secrets) {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(filepath.Join("..", "render", "testdata", "secrets.fixture.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg, secrets
}

// run builds and executes one rotation against the world, the way the
// command does, with the secrets read back from the last save. It returns the
// plan as printed, the progress, the error, and how many pairs were generated.
func (w *world) run(t *testing.T, cfg *config.Config, secrets *config.Secrets, generated *int) (string, string, error) {
	t.Helper()
	if w.saved != nil {
		secrets = w.saved
	}
	plan, err := rotatekey.Build(w.options(cfg, secrets, generated))
	if err != nil {
		return "", "", err
	}
	var printed, progress bytes.Buffer
	plan.Print(&printed)
	plan.Progress = &progress
	err = rotatekey.Execute(plan)
	return printed.String(), progress.String(), err
}

func (w *world) options(cfg *config.Config, secrets *config.Secrets, generated *int) rotatekey.Options {
	return rotatekey.Options{
		App:     "talk",
		Config:  cfg,
		Secrets: secrets,
		Transports: map[string]apply.Transport{
			"home-a": site{w, "home-a"}, "home-b": site{w, "home-b"},
		},
		Switch: switcher{w},
		Save: func(s *config.Secrets) error {
			if err := w.failing("save"); err != nil {
				return err
			}
			w.saves++
			w.saved = s
			w.log = append(w.log, "save secrets")
			return nil
		},
		Generate: func() (string, string, error) {
			*generated++
			return newID, newSecret, nil
		},
	}
}

func indexOf(log []string, substr string) int {
	return slices.IndexFunc(log, func(l string) bool { return strings.Contains(l, substr) })
}

// assertDone is the end state of every rotation: talk runs the new key on
// both sites, Garage holds only the new key and grants it, and the secrets
// hold the new pair and no previous one.
func (w *world) assertDone(t *testing.T) {
	t.Helper()
	if _, ok := w.keys[oldID]; ok {
		t.Error("the old key is still in Garage")
	}
	if !w.granted[newID] || w.keys[newID] != newSecret {
		t.Error("the new key is not in Garage with its grant")
	}
	for _, s := range []string{"home-a", "home-b"} {
		if w.running[s] != newID {
			t.Errorf("%s runs talk with %s", s, w.running[s])
		}
	}
	got := w.saved.Apps["talk"]
	if got["s3_access_key_id"] != newID || got["s3_secret_access_key"] != newSecret {
		t.Error("the secrets do not hold the new pair")
	}
	if _, ok := got[secretsgen.PreviousKeyID]; ok {
		t.Error("the secrets still hold a previous key ID")
	}
	if _, ok := got[secretsgen.PreviousSecretKey]; ok {
		t.Error("the secrets still hold a previous secret")
	}
	if len(w.objects) != 0 {
		t.Error("the probe was left in the bucket")
	}
}

// assertOldKeyKept is what every failure before the deletion must leave: the
// old key in Garage, still granted, so talk keeps working.
func (w *world) assertOldKeyKept(t *testing.T) {
	t.Helper()
	if w.keys[oldID] != oldSecret || !w.granted[oldID] {
		t.Error("a failure before the app was switched and the new key proven took the old key away")
	}
	if indexOf(w.log, "key delete") >= 0 {
		t.Error("a key delete ran")
	}
}

// The whole rotation, in the order that keeps the app working throughout:
// secrets, import, grant, switch every site, prove, then delete the old key
// and only then forget it.
func TestAFullRotationRunsInOrder(t *testing.T) {
	cfg, secrets := loadFixture(t)
	w := newWorld(t)
	var generated int
	printed, progress, err := w.run(t, cfg, secrets, &generated)
	if err != nil {
		t.Fatal(err)
	}
	w.assertDone(t)
	if generated != 1 || w.saves != 2 {
		t.Errorf("generated %d pairs and saved %d times, want 1 and 2", generated, w.saves)
	}
	order := []string{
		"save secrets",
		"key import " + newID,
		"bucket allow --read --write --owner talk-uploads --key " + newID,
		"home-a: apply --only talk",
		"home-b: apply --only talk",
		"home-a: curl PUT as " + newID,
		"home-b: curl DELETE as " + newID,
		"key delete --yes " + oldID,
	}
	last := -1
	for _, step := range order {
		i := indexOf(w.log, step)
		if i < 0 {
			t.Fatalf("%q never ran:\n%s", step, strings.Join(w.log, "\n"))
		}
		if i < last {
			t.Fatalf("%q ran out of order:\n%s", step, strings.Join(w.log, "\n"))
		}
		last = i
	}
	if strings.LastIndex(strings.Join(w.log, "\n"), "save secrets") < indexOf(w.log, "key delete") {
		t.Error("the previous pair was forgotten before the old key was deleted")
	}

	// The plan names the retiring key ID, never a secret.
	if !strings.Contains(printed, "retiring  "+oldID) {
		t.Errorf("the plan does not show the retiring key:\n%s", printed)
	}
	for _, secret := range []string{oldSecret, newSecret} {
		if strings.Contains(printed+progress, secret) {
			t.Error("a secret reached the plan or the progress")
		}
	}
}

// No command line carries a secret, except `key import`, which takes the new
// one positionally because Garage offers no other form.
func TestNoSecretInACommandButTheImport(t *testing.T) {
	cfg, secrets := loadFixture(t)
	w := newWorld(t)
	var generated int
	if _, _, err := w.run(t, cfg, secrets, &generated); err != nil {
		t.Fatal(err)
	}
	for _, line := range w.log {
		if strings.Contains(line, oldSecret) {
			t.Errorf("the old secret is in a command: %s", line)
		}
		if strings.Contains(line, newSecret) && !strings.Contains(line, "key import") {
			t.Errorf("the new secret is in a command: %s", line)
		}
	}
}

// A dry run changes nothing and names the key it would retire.
func TestADryRunChangesNothing(t *testing.T) {
	cfg, secrets := loadFixture(t)
	w := newWorld(t)
	var generated int
	plan, err := rotatekey.Build(w.options(cfg, secrets, &generated))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	plan.Print(&out)
	if generated != 0 || w.saves != 0 {
		t.Error("building the plan generated or saved")
	}
	for _, line := range w.log {
		if !strings.Contains(line, " key info ") && !strings.Contains(line, " bucket info ") {
			t.Errorf("building the plan ran %s", line)
		}
	}
	for _, want := range []string{"retiring  " + oldID, "generated at stage 1", "garage key delete --yes " + oldID, "home-b: as `paisans apply --site home-b --only talk`"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plan lacks %q:\n%s", want, out.String())
		}
	}
}

// A failure at any stage stops the run there; a stage before the deletion
// leaves the old key in Garage and granted; and the next run finishes from
// the secrets file without generating a second key.
func TestARotationResumesAfterAFailureAtEveryStage(t *testing.T) {
	for _, tc := range []struct {
		fail, stage string
		// beforeDelete is whether the old key must still be there.
		beforeDelete bool
	}{
		{"save", "stage 1 (new key in the secrets)", true},
		{"import", "stage 2 (new key in Garage)", true},
		{"grant", "stage 2 (new key in Garage)", true},
		{"apply home-b", "stage 3 (switch talk to the new key)", true},
		{"probe", "stage 4 (prove the new key)", true},
		{"delete", "stage 5 (retire the old key)", false},
	} {
		t.Run(tc.fail, func(t *testing.T) {
			cfg, secrets := loadFixture(t)
			w := newWorld(t)
			w.fail = tc.fail
			var generated int
			_, _, err := w.run(t, cfg, secrets, &generated)
			if err == nil || !strings.Contains(err.Error(), tc.stage) {
				t.Fatalf("want a stop at %s, got %v", tc.stage, err)
			}
			for _, secret := range []string{oldSecret, newSecret} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("the error carries a secret: %v", err)
				}
			}
			if tc.beforeDelete {
				w.assertOldKeyKept(t)
				if !strings.Contains(err.Error(), oldID+" is still in Garage") {
					t.Errorf("the error does not say the old key was kept: %v", err)
				}
			}
			if _, _, err := w.run(t, cfg, secrets, &generated); err != nil {
				t.Fatalf("the resumed run: %v\n%s", err, strings.Join(w.log, "\n"))
			}
			w.assertDone(t)
			if generated != 1 && tc.fail != "save" {
				t.Errorf("generated %d pairs; a resumed rotation must keep the one it saved", generated)
			}
			for _, line := range w.log {
				if strings.Contains(line, "key import") && !strings.Contains(line, "key import "+newID) {
					t.Errorf("a resumed run imported another key: %s", line)
				}
			}
			if len(w.ever) != 2 {
				t.Errorf("Garage has seen %d keys, want the old and one new", len(w.ever))
			}
		})
	}
}

// The key is deleted but the secrets write after it fails: the next run finds
// the old key gone and the new one in place, and only forgets the pair.
func TestARotationResumesAfterTheOldKeyIsDeleted(t *testing.T) {
	cfg, secrets := loadFixture(t)
	w := newWorld(t)
	var generated int
	// The second save is the one after the deletion.
	opts := w.options(cfg, secrets, &generated)
	save := opts.Save
	calls := 0
	opts.Save = func(s *config.Secrets) error {
		calls++
		if calls == 2 {
			return errors.New("disk full")
		}
		return save(s)
	}
	plan, err := rotatekey.Build(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := rotatekey.Execute(plan); err == nil || !strings.Contains(err.Error(), "stage 5") {
		t.Fatalf("want a stop at stage 5, got %v", err)
	}
	if _, ok := w.keys[oldID]; ok {
		t.Fatal("the old key was not deleted")
	}
	before := len(w.log)
	printed, _, err := w.run(t, cfg, secrets, &generated)
	if err != nil {
		t.Fatal(err)
	}
	w.assertDone(t)
	if strings.Contains(printed, "garage key delete") {
		t.Errorf("the resumed plan deletes a key that is already gone:\n%s", printed)
	}
	for _, line := range w.log[before:] {
		if strings.Contains(line, "key import") || strings.Contains(line, "apply --only") || strings.Contains(line, "key delete") {
			t.Errorf("the resumed run repeated %s", line)
		}
	}
}

// Secrets that cannot describe a coherent rotation are refused before
// anything reaches Garage.
func TestIncoherentSecretsAreRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(map[string]any)
		want string
	}{
		"half a previous pair": {func(m map[string]any) { m[secretsgen.PreviousKeyID] = newID }, "half of a previous S3 pair"},
		"previous is current": {func(m map[string]any) {
			m[secretsgen.PreviousKeyID], m[secretsgen.PreviousSecretKey] = oldID, oldSecret
		}, "are the same key"},
		"no key": {func(m map[string]any) { delete(m, "s3_access_key_id") }, "has no S3 key"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, secrets := loadFixture(t)
			tc.edit(secrets.Apps["talk"])
			w := newWorld(t)
			var generated int
			_, err := rotatekey.Build(w.options(cfg, secrets, &generated))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if len(w.log) != 0 {
				t.Errorf("a refused rotation reached Garage: %v", w.log)
			}
		})
	}
}

// A current key Garage does not hold has nothing to retire: storage init is
// what imports it.
func TestAKeyGarageDoesNotHoldIsNotRotated(t *testing.T) {
	cfg, secrets := loadFixture(t)
	w := newWorld(t)
	delete(w.keys, oldID)
	var generated int
	_, err := rotatekey.Build(w.options(cfg, secrets, &generated))
	if err == nil || !strings.Contains(err.Error(), "storage init") {
		t.Fatalf("want a refusal naming storage init, got %v", err)
	}
}

// An app that stores nothing has no key.
func TestAnAppWithoutStorageIsRefused(t *testing.T) {
	cfg, secrets := loadFixture(t)
	w := newWorld(t)
	var generated int
	opts := w.options(cfg, secrets, &generated)
	opts.App = "auth"
	if _, err := rotatekey.Build(opts); err == nil || !strings.Contains(err.Error(), "stores no objects") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

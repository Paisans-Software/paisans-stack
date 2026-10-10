package deployrecord_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/deployrecord"
)

var dep = deployment.Deployment{ID: "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"}

// fakeHost runs the record's write command as the shell would: refuse on a
// changed hash, else replace the file.
type fakeHost struct{ files map[string]string }

var (
	pathRe = regexp.MustCompile(`f='([^']+)'`)
	sumRe  = regexp.MustCompile(`\[ "\$cur" = '([^']*)' \]`)
	dataRe = regexp.MustCompile(`printf %s '([^']*)' \| base64 -d`)
)

func (h *fakeHost) Describe() string { return "ubuntu@vm.example.org" }
func (h *fakeHost) ReadFile(p string) (string, bool, error) {
	c, ok := h.files[p]
	return c, ok, nil
}
func (h *fakeHost) Run(command string) (string, error) {
	path := pathRe.FindStringSubmatch(command)[1]
	cur := "none"
	if c, ok := h.files[path]; ok {
		s := sha256.Sum256([]byte(c))
		cur = hex.EncodeToString(s[:])
	}
	if sumRe.FindStringSubmatch(command)[1] != cur {
		return deployrecord.ChangedMarker + "\n", errors.New("exit status 1")
	}
	data, _ := base64.StdEncoding.DecodeString(dataRe.FindStringSubmatch(command)[1])
	h.files[path] = string(data)
	return "", nil
}

func TestAddIsAUnionAndForgetTakesOnlyWhatIsNamed(t *testing.T) {
	h := &fakeHost{files: map[string]string{}}
	if changed, err := deployrecord.Add(h, dep, deployrecord.Record{Sites: []string{"vm", "home-a"}, Apps: []string{"talk"}}); err != nil || !changed {
		t.Fatalf("first add: %v %v", changed, err)
	}
	if _, err := deployrecord.Add(h, dep, deployrecord.Record{Sites: []string{"home-b"}}); err != nil {
		t.Fatal(err)
	}
	r, found, err := deployrecord.Read(h, dep)
	if err != nil || !found || strings.Join(r.Sites, ",") != "home-a,home-b,vm" || !r.Lists("apps", "talk") {
		t.Fatalf("%+v %v %v", r, found, err)
	}
	if changed, _ := deployrecord.Add(h, dep, deployrecord.Record{Sites: []string{"vm"}}); changed {
		t.Error("adding what is listed wrote the file")
	}
	if _, err := deployrecord.Forget(h, dep, deployrecord.Record{Sites: []string{"home-b"}}); err != nil {
		t.Fatal(err)
	}
	r, _, _ = deployrecord.Read(h, dep)
	if strings.Join(r.Sites, ",") != "home-a,vm" || !r.Lists("apps", "talk") {
		t.Errorf("after forget: %+v", r)
	}
}

func TestForgetWithNoRecordWritesNothing(t *testing.T) {
	h := &fakeHost{files: map[string]string{}}
	if changed, err := deployrecord.Forget(h, dep, deployrecord.Record{Sites: []string{"x"}}); err != nil || changed || len(h.files) != 0 {
		t.Errorf("%v %v %v", changed, err, h.files)
	}
}

func TestAWriteRefusesAChangedRecord(t *testing.T) {
	h := &fakeHost{files: map[string]string{}}
	racing := &racer{fakeHost: h}
	_, err := deployrecord.Add(racing, dep, deployrecord.Record{Sites: []string{"vm"}})
	if err == nil || !strings.Contains(err.Error(), "changed while it was read") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.files[deployrecord.Path(dep)], "home-b") {
		t.Error("the other writer's record was overwritten")
	}
}

// racer is a host where another writer lands between the read and the write.
type racer struct{ *fakeHost }

func (r *racer) ReadFile(p string) (string, bool, error) {
	c, ok, err := r.fakeHost.ReadFile(p)
	r.files[p] = `{"version":1,"sites":["home-b"],"apps":[],"pocket_id_groups":[]}` + "\n"
	return c, ok, err
}

func TestAMalformedRecordIsAnError(t *testing.T) {
	h := &fakeHost{files: map[string]string{deployrecord.Path(dep): "{"}}
	if _, _, err := deployrecord.Read(h, dep); err == nil {
		t.Error("read a malformed record")
	}
}

func TestUnion(t *testing.T) {
	u := deployrecord.Union(deployrecord.Record{Sites: []string{"a"}}, deployrecord.Record{Sites: []string{"b", "a"}, Apps: []string{"x"}})
	if strings.Join(u.Sites, ",") != "a,b" || !u.Lists("apps", "x") {
		t.Errorf("%+v", u)
	}
}

func TestThePathCarriesTheToken(t *testing.T) {
	if got := deployrecord.Path(dep); got != "/var/lib/paisans/deployed.f2a9.json" {
		t.Errorf("got %s", got)
	}
}

func TestFromConfig(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	r := deployrecord.FromConfig(cfg)
	if strings.Join(r.Sites, ",") != "home-a,home-b,vm,watch" || !r.Lists("pocket_id_groups", "members") || !r.Lists("apps", "talk") {
		t.Errorf("%+v", r)
	}
}

// A malformed record is ErrMalformed, which callers name with the way out.
func TestAMalformedRecordIsErrMalformed(t *testing.T) {
	h := &fakeHost{files: map[string]string{deployrecord.Path(dep): `{"version":2}`}}
	if _, _, err := deployrecord.Read(h, dep); !errors.Is(err, deployrecord.ErrMalformed) {
		t.Errorf("err = %v", err)
	}
}

// failingWrite is a host whose write fails the way SSHTransport's does: the
// error already carries the remote output.
type failingWrite struct{ *fakeHost }

func (f failingWrite) Run(string) (string, error) {
	return "mv: cannot move\n", errors.New("ubuntu@vm.example.org: exit status 1: mv: cannot move")
}

func TestAFailedWriteSaysItsOutputOnce(t *testing.T) {
	h := failingWrite{&fakeHost{files: map[string]string{}}}
	_, err := deployrecord.Add(h, dep, deployrecord.Record{Sites: []string{"vm"}})
	if err == nil || strings.Count(err.Error(), "mv: cannot move") != 1 {
		t.Errorf("err = %v", err)
	}
}

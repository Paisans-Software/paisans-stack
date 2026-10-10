package deployrecord_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
	"github.com/paisans-software/paisans-stack/internal/deployrecord"
	"github.com/paisans-software/paisans-stack/internal/registry"
)

var dep = deployment.Deployment{ID: "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"}

// fakeHost runs the record's write command as the shell would: refuse on a
// changed hash, else replace the file.
type fakeHost struct {
	files map[string]string
	down  bool
}

var errDown = errors.New("ssh: connect to host vm.example.org port 22: Operation timed out")

var (
	pathRe = regexp.MustCompile(`f='([^']+)'`)
	sumRe  = regexp.MustCompile(`\[ "\$cur" = '([^']*)' \]`)
	dataRe = regexp.MustCompile(`printf %s '([^']*)' \| base64 -d`)
)

func (h *fakeHost) Describe() string { return "ubuntu@vm.example.org" }
func (h *fakeHost) ReadFile(p string) (string, bool, error) {
	if h.down {
		return "", false, errDown
	}
	c, ok := h.files[p]
	return c, ok, nil
}
func (h *fakeHost) Run(command string) (string, error) {
	if h.down {
		return "", errDown
	}
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

func gateways(hs ...*fakeHost) map[string]registry.Runner {
	out := map[string]registry.Runner{}
	for i, h := range hs {
		out[fmt.Sprintf("gw%d", i+1)] = h
	}
	return out
}

var now = time.Date(2026, 10, 9, 18, 40, 0, 0, time.UTC)

func TestUpdateCatchesUpAStaleGateway(t *testing.T) {
	fresh := &fakeHost{files: map[string]string{deployrecord.Path(dep): `{"version":1,"revision":5,"sites":["vm"],"apps":[],"pocket_id_groups":[]}`}}
	stale := &fakeHost{files: map[string]string{deployrecord.Path(dep): `{"version":1,"revision":3,"sites":["vm","monitor-a"],"apps":[],"pocket_id_groups":[]}`}}
	res, err := deployrecord.Update(gateways(fresh, stale), dep, deployrecord.Adding(deployrecord.Record{}), now)
	if err != nil || res.Changed || strings.Join(res.Wrote, ",") != "gw2" {
		t.Fatalf("%+v %v", res, err)
	}
	r, _, _ := deployrecord.Read(stale, dep)
	if r.Revision != 5 || r.Lists("sites", "monitor-a") {
		t.Errorf("stale gateway not caught up: %+v", r)
	}
}

func TestAChangeStartsFromTheNewestAndRaisesTheRevision(t *testing.T) {
	fresh := &fakeHost{files: map[string]string{deployrecord.Path(dep): `{"version":1,"revision":5,"sites":["vm"],"apps":[],"pocket_id_groups":[]}`}}
	stale := &fakeHost{files: map[string]string{deployrecord.Path(dep): `{"version":1,"revision":3,"sites":["vm","monitor-a"],"apps":[],"pocket_id_groups":[]}`}}
	res, err := deployrecord.Update(gateways(fresh, stale), dep, deployrecord.Adding(deployrecord.Record{Sites: []string{"home-a"}}), now)
	if err != nil || !res.Changed || len(res.Wrote) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	for _, h := range []*fakeHost{fresh, stale} {
		r, _, _ := deployrecord.Read(h, dep)
		if r.Revision != 6 || r.UpdatedAt != "2026-10-09T18:40:00Z" || strings.Join(r.Sites, ",") != "home-a,vm" {
			t.Errorf("%+v", r)
		}
	}
}

func TestUpdateReportsAGatewayItMissed(t *testing.T) {
	up := &fakeHost{files: map[string]string{}}
	down := &fakeHost{files: map[string]string{}, down: true}
	res, err := deployrecord.Update(gateways(up, down), dep, deployrecord.Adding(deployrecord.Record{Sites: []string{"vm"}}), now)
	if err != nil || res.Missed["gw2"] == nil || strings.Join(res.Wrote, ",") != "gw1" {
		t.Errorf("%+v %v", res, err)
	}
}

func TestUpdateWithEveryGatewayDownWritesNothing(t *testing.T) {
	down := &fakeHost{files: map[string]string{}, down: true}
	res, err := deployrecord.Update(gateways(down), dep, deployrecord.Adding(deployrecord.Record{Sites: []string{"vm"}}), now)
	if err != nil || len(res.Wrote) != 0 || res.Missed["gw1"] == nil {
		t.Errorf("%+v %v", res, err)
	}
}

func TestUpdateStopsAtAMalformedRecord(t *testing.T) {
	ok := &fakeHost{files: map[string]string{}}
	bad := &fakeHost{files: map[string]string{deployrecord.Path(dep): "{"}}
	_, err := deployrecord.Update(gateways(ok, bad), dep, deployrecord.Adding(deployrecord.Record{Sites: []string{"vm"}}), now)
	if !errors.Is(err, deployrecord.ErrMalformed) || len(ok.files) != 0 {
		t.Errorf("err %v, wrote %v", err, ok.files)
	}
}

func TestUpdateWithNothingToChangeWritesNothing(t *testing.T) {
	rec := `{"version":1,"revision":2,"updated_at":"x","sites":["vm"],"apps":[],"pocket_id_groups":[]}` + "\n"
	a := &fakeHost{files: map[string]string{deployrecord.Path(dep): rec}}
	b := &fakeHost{files: map[string]string{deployrecord.Path(dep): rec}}
	res, err := deployrecord.Update(gateways(a, b), dep, deployrecord.Adding(deployrecord.Record{Sites: []string{"vm"}}), now)
	if err != nil || res.Changed || len(res.Wrote) != 0 {
		t.Errorf("%+v %v", res, err)
	}
}

func TestEqualRevisionsReadAsTheirUnion(t *testing.T) {
	n := deployrecord.Newest(
		deployrecord.Record{Revision: 4, Sites: []string{"a"}},
		deployrecord.Record{Revision: 4, Sites: []string{"b"}},
		deployrecord.Record{Revision: 3, Sites: []string{"c"}},
	)
	if n.Revision != 4 || strings.Join(n.Sites, ",") != "a,b" {
		t.Errorf("%+v", n)
	}
}

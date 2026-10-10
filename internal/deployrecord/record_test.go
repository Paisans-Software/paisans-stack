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

func gateways(hs ...*fakeHost) map[string]registry.Runner {
	out := map[string]registry.Runner{}
	for i, h := range hs {
		out[fmt.Sprintf("gw%d", i+1)] = h
	}
	return out
}

var now = time.Date(2026, 10, 9, 18, 40, 0, 0, time.UTC)

func view(t *testing.T, hs ...*fakeHost) deployrecord.Record {
	t.Helper()
	r, _, missing := deployrecord.Gather(gateways(hs...), dep)
	for gw, err := range missing {
		if !errors.Is(err, deployrecord.ErrNoRecord) {
			t.Fatalf("%s: %v", gw, err)
		}
	}
	return r
}

func change(t *testing.T, ch deployrecord.Change, hs ...*fakeHost) deployrecord.Result {
	t.Helper()
	res, err := deployrecord.Update(gateways(hs...), dep, ch, now)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func sites(names ...string) deployrecord.Record { return deployrecord.Record{Sites: names} }

func TestAReAddOnAGatewayThatMissedTheRemovalStays(t *testing.T) {
	a, b := &fakeHost{files: map[string]string{}}, &fakeHost{files: map[string]string{}}
	change(t, deployrecord.Adding(sites("vm", "x")), a, b)
	b.down = true
	change(t, deployrecord.Forgetting(sites("x")), a, b)
	a.down, b.down = true, false
	change(t, deployrecord.Adding(sites("vm", "x")), a, b)
	a.down = false
	if r := view(t, a, b); !r.Lists("sites", "x") {
		t.Errorf("x lost: %v", r.Sites)
	}
}

func TestASplitThroughUpdateKeepsEveryAddAndEveryRemoval(t *testing.T) {
	a, b := &fakeHost{files: map[string]string{}}, &fakeHost{files: map[string]string{}}
	change(t, deployrecord.Adding(sites("vm", "m1")), a, b)
	a.down = true
	change(t, deployrecord.Adding(sites("y")), a, b)
	a.down, b.down = false, true
	change(t, deployrecord.Forgetting(sites("m1")), a, b)
	b.down = false
	if r := view(t, a, b); strings.Join(r.Sites, ",") != "vm,y" {
		t.Fatalf("merged %v", r.Sites)
	}
	change(t, deployrecord.Forgetting(sites()), a, b)
	if a.files[deployrecord.Path(dep)] != b.files[deployrecord.Path(dep)] {
		t.Error("the gateways differ after a write both answered")
	}
	if strings.Contains(a.files[deployrecord.Path(dep)], `"m1"`) {
		t.Error("not compacted: m1 is still in the file")
	}
}

func TestALoneGatewaysAddsReachTheOthers(t *testing.T) {
	a, c := &fakeHost{files: map[string]string{}}, &fakeHost{files: map[string]string{}}
	change(t, deployrecord.Adding(sites("vm")), a)
	a.down = true
	change(t, deployrecord.Adding(sites("vm2")), a, c)
	a.down = false
	change(t, deployrecord.Forgetting(sites()), a, c)
	if r := view(t, a); strings.Join(r.Sites, ",") != "vm,vm2" {
		t.Errorf("a holds %v", r.Sites)
	}
}

func TestNoCompactionWhileAGatewayIsDown(t *testing.T) {
	a, b := &fakeHost{files: map[string]string{}}, &fakeHost{files: map[string]string{}}
	change(t, deployrecord.Adding(sites("vm", "x")), a, b)
	b.down = true
	change(t, deployrecord.Forgetting(sites("x")), a, b)
	if !strings.Contains(a.files[deployrecord.Path(dep)], `"x"`) {
		t.Fatal("x's removal was compacted away while b was down")
	}
	b.down = false
	if r := view(t, a, b); r.Lists("sites", "x") {
		t.Error("b's stale add brought x back")
	}
}

func TestAddingNeverRemovesAndForgettingTakesOnlyWhatIsNamed(t *testing.T) {
	h := &fakeHost{files: map[string]string{}}
	change(t, deployrecord.Adding(deployrecord.Record{Sites: []string{"vm", "home-a"}, Apps: []string{"talk"}}), h)
	change(t, deployrecord.Adding(sites("home-b")), h)
	if r := view(t, h); strings.Join(r.Sites, ",") != "home-a,home-b,vm" || !r.Lists("apps", "talk") {
		t.Fatalf("%+v", r)
	}
	change(t, deployrecord.Forgetting(sites("home-b")), h)
	if r := view(t, h); strings.Join(r.Sites, ",") != "home-a,vm" || !r.Lists("apps", "talk") {
		t.Errorf("after forgetting: %+v", r)
	}
}

func TestForgettingWithNoRecordWritesNothing(t *testing.T) {
	h := &fakeHost{files: map[string]string{}}
	if res := change(t, deployrecord.Forgetting(sites("x")), h); res.Changed || len(h.files) != 0 {
		t.Errorf("%+v %v", res, h.files)
	}
}

func TestAWriteRefusesAChangedRecord(t *testing.T) {
	h := &fakeHost{files: map[string]string{}}
	racing := &racer{fakeHost: h}
	res, err := deployrecord.Update(map[string]registry.Runner{"vm": racing}, dep, deployrecord.Adding(sites("vm")), now)
	if err != nil || !errors.Is(res.Missed["vm"], deployrecord.ErrChanged) {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(h.files[deployrecord.Path(dep)], "home-b") {
		t.Error("the other writer's record was overwritten")
	}
}

// racer is a host where another writer lands between the read and the write.
type racer struct{ *fakeHost }

func (r *racer) ReadFile(p string) (string, bool, error) {
	c, ok, err := r.fakeHost.ReadFile(p)
	r.files[p] = deployrecord.Encode(deployrecord.Record{Sites: []string{"home-b"}})
	return c, ok, err
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

// failingWrite is a host whose write fails the way SSHTransport's does: the
// error already carries the remote output.
type failingWrite struct{ *fakeHost }

func (f failingWrite) Run(string) (string, error) {
	return "mv: cannot move\n", errors.New("ubuntu@vm.example.org: exit status 1: mv: cannot move")
}

func TestAFailedWriteSaysItsOutputOnce(t *testing.T) {
	h := failingWrite{&fakeHost{files: map[string]string{}}}
	res, _ := deployrecord.Update(map[string]registry.Runner{"vm": h}, dep, deployrecord.Adding(sites("vm")), now)
	if err := res.Missed["vm"]; err == nil || strings.Count(err.Error(), "mv: cannot move") != 1 {
		t.Errorf("err = %v", err)
	}
}

func TestUpdateReportsAGatewayItMissed(t *testing.T) {
	up := &fakeHost{files: map[string]string{}}
	down := &fakeHost{files: map[string]string{}, down: true}
	res := change(t, deployrecord.Adding(sites("vm")), up, down)
	if res.Missed["gw2"] == nil || strings.Join(res.Wrote, ",") != "gw1" {
		t.Errorf("%+v", res)
	}
}

func TestUpdateWithEveryGatewayDownWritesNothing(t *testing.T) {
	down := &fakeHost{files: map[string]string{}, down: true}
	res := change(t, deployrecord.Adding(sites("vm")), down)
	if len(res.Wrote) != 0 || res.Missed["gw1"] == nil {
		t.Errorf("%+v", res)
	}
}

func TestUpdateStopsAtAMalformedRecord(t *testing.T) {
	ok := &fakeHost{files: map[string]string{}}
	bad := &fakeHost{files: map[string]string{deployrecord.Path(dep): "{"}}
	_, err := deployrecord.Update(gateways(ok, bad), dep, deployrecord.Adding(sites("vm")), now)
	if !errors.Is(err, deployrecord.ErrMalformed) || len(ok.files) != 0 {
		t.Errorf("err %v, wrote %v", err, ok.files)
	}
}

func TestForgettingNothingWithEveryGatewayCurrentWritesNothing(t *testing.T) {
	a, b := &fakeHost{files: map[string]string{}}, &fakeHost{files: map[string]string{}}
	change(t, deployrecord.Adding(sites("vm")), a, b)
	if res := change(t, deployrecord.Forgetting(sites()), a, b); res.Changed || len(res.Wrote) != 0 {
		t.Errorf("%+v", res)
	}
}

func TestAddingMakesANewTagEvenWhenDeployed(t *testing.T) {
	h := &fakeHost{files: map[string]string{}}
	b := &fakeHost{files: map[string]string{}, down: true}
	change(t, deployrecord.Adding(sites("vm")), h, b)
	first := h.files[deployrecord.Path(dep)]
	change(t, deployrecord.Adding(sites("vm")), h, b)
	if h.files[deployrecord.Path(dep)] == first {
		t.Error("a second add of a deployed name made no new tag")
	}
}

func TestOldLayoutsAreMalformed(t *testing.T) {
	for _, content := range []string{
		"{",
		`{"version":2}`,
		`{"version":1,"sites":["vm"],"apps":[],"pocket_id_groups":[]}`,
		`{"version":1,"revision":2,"sites":{"vm":{"added":1}},"apps":{},"pocket_id_groups":{}}`,
	} {
		h := &fakeHost{files: map[string]string{deployrecord.Path(dep): content}}
		if _, _, missing := deployrecord.Gather(gateways(h), dep); !errors.Is(missing["gw1"], deployrecord.ErrMalformed) {
			t.Errorf("%s: %v", content, missing["gw1"])
		}
	}
}

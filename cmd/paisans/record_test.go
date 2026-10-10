package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployrecord"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// recordFake is a gateway that runs the deployment record's write command as
// the shell would: refused on a changed hash, else the file replaced.
type recordFake struct {
	files map[string]string
	ran   []string
	down  bool
}

var (
	recordPathRe = regexp.MustCompile(`f='([^']+)'`)
	recordSumRe  = regexp.MustCompile(`\[ "\$cur" = '([^']*)' \]`)
	recordDataRe = regexp.MustCompile(`printf %s '([^']*)' \| base64 -d`)
)

func (h *recordFake) Describe() string { return "ubuntu@vm.example.org" }

func (h *recordFake) ReadFile(p string) (string, bool, error) {
	if h.down {
		return "", false, errors.New("ssh: connect to host vm.example.org port 22: Operation timed out")
	}
	c, ok := h.files[p]
	return c, ok, nil
}

func (h *recordFake) Run(command string) (string, error) {
	if h.down {
		return "", errors.New("ssh: connect to host vm.example.org port 22: Operation timed out")
	}
	h.ran = append(h.ran, command)
	path := recordPathRe.FindStringSubmatch(command)[1]
	cur := "none"
	if c, ok := h.files[path]; ok {
		s := sha256.Sum256([]byte(c))
		cur = hex.EncodeToString(s[:])
	}
	if recordSumRe.FindStringSubmatch(command)[1] != cur {
		return deployrecord.ChangedMarker + "\n", errors.New("exit status 1")
	}
	data, _ := base64.StdEncoding.DecodeString(recordDataRe.FindStringSubmatch(command)[1])
	h.files[path] = string(data)
	return "", nil
}

func TestApplyRecordsTheDeploymentOnAGateway(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	gw := &recordFake{files: map[string]string{}}
	if err := recordApplied(&ui.Recorder{}, cfg, "vm", func(string) registry.Runner { return gw }); err != nil {
		t.Fatal(err)
	}
	rec, found, _ := deployrecord.Read(gw, cfg.Deployment())
	if !found || !rec.Lists("sites", "home-b") || !rec.Lists("apps", "talk") {
		t.Errorf("%+v %v", rec, found)
	}
	other := &recordFake{files: map[string]string{}}
	if err := recordApplied(&ui.Recorder{}, cfg, "home-b", func(string) registry.Runner { return other }); err != nil || len(other.files) != 0 || len(other.ran) != 0 {
		t.Errorf("a non gateway was written: %v %v", err, other.files)
	}
}

func TestForgetInRecordsReachesEveryGatewayAndWarnsForOneDown(t *testing.T) {
	cfg, _ := config.Load(fixtureConfig())
	vm := &recordFake{files: map[string]string{deployrecord.Path(cfg.Deployment()): `{"version":1,"sites":["vm","monitor-a"],"apps":[],"pocket_id_groups":[]}`}}
	saved := registryHost
	registryHost = func(name string, _ config.Site, _ string, _ bool) registry.Runner { return vm }
	t.Cleanup(func() { registryHost = saved })
	rec := &ui.Recorder{}
	forgetInRecords(rec, cfg, deployrecord.Record{Sites: []string{"monitor-a"}}, "monitor-a", true, true)
	if strings.Contains(vm.files[deployrecord.Path(cfg.Deployment())], "monitor-a") {
		t.Error("vm still lists monitor-a")
	}
	vm.down = true
	rec = &ui.Recorder{}
	forgetInRecords(rec, cfg, deployrecord.Record{Sites: []string{"monitor-b"}}, "monitor-b", true, true)
	if !strings.Contains(rec.Lines(), "vm missed this change to the deployment record") {
		t.Errorf("no warning:\n%s", rec.Lines())
	}
}

func TestForgetAppCleansTheRecordWhenNothingElseIsLeft(t *testing.T) {
	cfg, _ := config.Load(fixtureConfig())
	deployed := deployrecord.Record{Apps: []string{"talk", "uptime"}, PocketIDGroups: []string{"members", "retired"}}
	names := appRecordNames(cfg, "uptime", deployed)
	if strings.Join(names.Apps, ",") != "uptime" || strings.Join(names.PocketIDGroups, ",") != "retired" {
		t.Errorf("%+v", names)
	}
}

// A dry run that would take a name out of a record says so, so a removal
// with nothing else left still points at --execute.
func TestForgetInRecordsDryRunSaysWhetherARecordListsIt(t *testing.T) {
	cfg, _ := config.Load(fixtureConfig())
	vm := &recordFake{files: map[string]string{deployrecord.Path(cfg.Deployment()): `{"version":1,"sites":["vm","monitor-a"],"apps":[],"pocket_id_groups":[]}`}}
	saved := registryHost
	registryHost = func(string, config.Site, string, bool) registry.Runner { return vm }
	t.Cleanup(func() { registryHost = saved })
	if !forgetInRecords(&ui.Recorder{}, cfg, deployrecord.Record{Sites: []string{"monitor-a"}}, "monitor-a", false, true) {
		t.Error("a listed name is not reported as left")
	}
	if forgetInRecords(&ui.Recorder{}, cfg, deployrecord.Record{Sites: []string{"monitor-b"}}, "monitor-b", false, true) {
		t.Error("an unlisted name is reported as left")
	}
	if len(vm.ran) != 0 {
		t.Error("a dry run wrote")
	}
}

// A malformed record stops a gateway's apply with the way out, not "run it
// again".
func TestApplyNamesTheWayOutOfAMalformedRecord(t *testing.T) {
	cfg, _ := config.Load(fixtureConfig())
	gw := &recordFake{files: map[string]string{deployrecord.Path(cfg.Deployment()): "{"}}
	err := recordApplied(&ui.Recorder{}, cfg, "vm", func(string) registry.Runner { return gw })
	if err == nil || !strings.Contains(err.Error(), "rm "+deployrecord.Path(cfg.Deployment())) || strings.Contains(err.Error(), "run it again") {
		t.Errorf("err = %v", err)
	}
}

// The record lines sit under a section of their own, not the last site's.
func TestForgetInRecordsHasItsOwnSection(t *testing.T) {
	cfg, _ := config.Load(fixtureConfig())
	vm := &recordFake{files: map[string]string{deployrecord.Path(cfg.Deployment()): `{"version":1,"sites":["monitor-a"],"apps":[],"pocket_id_groups":[]}`}}
	saved := registryHost
	registryHost = func(string, config.Site, string, bool) registry.Runner { return vm }
	t.Cleanup(func() { registryHost = saved })
	rec := &ui.Recorder{}
	forgetInRecords(rec, cfg, deployrecord.Record{Sites: []string{"monitor-a"}}, "monitor-a", false, true)
	s, i := rec.Index("section", "deployment record"), rec.Index("item", "forget monitor-a")
	if s < 0 || i < s {
		t.Errorf("section %d, item %d:\n%s", s, i, rec.Lines())
	}
}

// withGateway is cfg with one more site, name, holding the gateway role.
func withGateway(cfg *config.Config, name string) *config.Config {
	out := *cfg
	out.Sites = map[string]config.Site{}
	for k, v := range cfg.Sites {
		out.Sites[k] = v
	}
	out.Sites[name] = config.Site{Roles: []config.Role{config.RoleGateway}}
	return &out
}

// apply of a gateway writes every gateway it reaches, and reports one down.
func TestApplyRecordsOnEveryGatewayAndReportsOneDown(t *testing.T) {
	cfg, _ := config.Load(fixtureConfig())
	vm := &recordFake{files: map[string]string{}}
	down := &recordFake{files: map[string]string{}, down: true}
	rec := &ui.Recorder{}
	hosts := func(gw string) registry.Runner { return map[string]registry.Runner{"vm": vm, "vm2": down}[gw] }
	if err := recordApplied(rec, withGateway(cfg, "vm2"), "vm", hosts); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := deployrecord.Read(vm, cfg.Deployment()); !found {
		t.Error("vm was not written")
	}
	if !strings.Contains(rec.Lines(), "vm2 missed this change") {
		t.Errorf("no warning:\n%s", rec.Lines())
	}
}

// With every gateway down the step does not claim the record was updated.
func TestARecordChangeThatReachedNoGatewaySaysSo(t *testing.T) {
	cfg, _ := config.Load(fixtureConfig())
	down := &recordFake{files: map[string]string{}, down: true}
	rec := &ui.Recorder{}
	if err := recordApplied(rec, cfg, "vm", func(string) registry.Runner { return down }); err != nil {
		t.Fatal(err)
	}
	if rec.Has("done", "record the deployment") {
		t.Errorf("the step ended as done:\n%s", rec.Lines())
	}
	if !rec.Has("fail", "record the deployment") {
		t.Errorf("the step did not say it reached no gateway:\n%s", rec.Lines())
	}
}

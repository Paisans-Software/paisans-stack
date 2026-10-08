package siteadd_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/siteadd"
)

// withReplica is a join of home-b to a cluster that already has a replica:
// home-a leads, home-c is its Sync Standby, vm the witness.
func withReplica(t *testing.T) *world {
	t.Helper()
	cfg, secrets := endState(t)
	withHomeC := func(cfg *config.Config) {
		c := cfg.Sites["home-b"]
		c.Address, c.Endpoint = "10.44.0.5", "198.51.100.30:51820"
		cfg.Sites["home-c"] = c
	}
	withHomeC(cfg)
	cfg.Cluster.Sites = []string{"home-a", "home-b", "home-c"}
	cfg.Etcd.Members = []string{"home-a", "home-b", "home-c", "vm"}
	secrets.Sites["home-c"] = config.SiteSecrets{WireGuardPrivateKey: "REREREREREREREREREREREREREREREREREREREREREQ="}

	w := &world{t: t, cfg: cfg, secrets: secrets, hosts: map[string]*host{}, nextID: 3,
		running: map[string]bool{"home-a": true, "home-c": true, "vm": true},
		patroni: map[string]bool{"home-a": true, "home-c": true},
		served:  map[string]string{}, syncMode: true, etcdHosts: map[string]string{}, starting: map[string]int{}}
	for _, name := range cfg.SiteNames() {
		w.hosts[name] = &host{w: w, name: name, files: map[string]string{}}
	}

	before, _ := endState(t)
	withHomeC(before)
	delete(before.Sites, "home-b")
	before.Cluster.Sites = []string{"home-a", "home-c"}
	before.Etcd.Members = []string{"home-a", "home-c", "vm"}
	rendered, err := render.Build(before, secrets)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"home-a", "home-c", "vm"} {
		p, err := apply.Build(name, rendered, "", w.hosts[name])
		if err != nil {
			t.Fatal(err)
		}
		if err := apply.Execute(p, w.hosts[name]); err != nil {
			t.Fatal(err)
		}
		w.hosts[name].commands = nil
		w.hosts[name].writes = 0
	}
	for _, name := range []string{"home-a", "home-c"} {
		w.etcdHosts[name] = envLine(w.hosts[name].files["/srv/paisans/f2a9/infra/patroni.env"], "ETCD3_HOSTS")
	}
	w.served["home-a"] = w.hosts["home-a"].files["/srv/paisans/f2a9/infra/haproxy/haproxy.cfg"]
	w.etcd = []apply.EtcdMember{
		{ID: 1, Name: "home-a", PeerURLs: []string{"http://10.44.0.1:2380"}, ClientURLs: []string{"http://10.44.0.1:2379"}},
		{ID: 2, Name: "home-c", PeerURLs: []string{"http://10.44.0.5:2380"}, ClientURLs: []string{"http://10.44.0.5:2379"}},
		{ID: 3, Name: "vm", PeerURLs: []string{"http://10.44.0.3:2380"}, ClientURLs: []string{"http://10.44.0.3:2379"}},
	}
	w.members = []map[string]any{
		{"Member": "home-a", "Role": "Leader", "State": "running", "Replay Lag": ""},
		{"Member": "home-c", "Role": "Sync Standby", "State": "streaming", "Replay Lag": float64(0)},
	}
	return w
}

// An existing replica's patroni.env is applied and its Patroni recreated,
// gated on it streaming again; the leader's is noted for its next restart.
func TestAJoinBringsAReplicasPatroniEnvUpToDate(t *testing.T) {
	defer siteadd.SetFast()()
	w := withReplica(t)
	p := build(t, w)
	if !hasStep(p, 7, "home-c", "update") || !hasStep(p, 7, "home-c", "recreate") || !hasStep(p, 7, "home-c", "check") {
		t.Fatalf("stage 7 does not apply home-c's patroni.env:\n%v", steps(p, 7))
	}
	for _, s := range steps(p, 7) {
		if s.Site == "home-a" {
			t.Errorf("the leader is planned for: %v", s)
		}
	}
	notes := strings.Join(p.Notes, "\n")
	if !strings.Contains(notes, "home-a leads") || !strings.Contains(notes, "next restart") {
		t.Errorf("the leader's patroni.env is not noted:\n%s", notes)
	}
	var progress bytes.Buffer
	p.Progress = &progress
	if err := siteadd.Execute(p); err != nil {
		t.Fatalf("%v\n%s", err, progress.String())
	}
	if strings.Join(w.recreated, ",") != "home-c" {
		t.Errorf("recreated %v", w.recreated)
	}
	if !strings.Contains(w.etcdHosts["home-c"], "10.44.0.2:2379") {
		t.Errorf("home-c's Patroni runs with %q", w.etcdHosts["home-c"])
	}
	if strings.Contains(w.etcdHosts["home-a"], "10.44.0.2:2379") {
		t.Error("the leader's Patroni was recreated")
	}
	// Stage 7's write came after stage 2's and stage 6's on the same host,
	// and the manifest records all of them as they now are.
	h := w.hosts["home-c"]
	var m render.Manifest
	if err := json.Unmarshal([]byte(h.files["/srv/paisans/f2a9/.paisans-manifest.json"]), &m); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range m.Files {
		if e.Path == "etc/wireguard/psns-f2a9.conf" || e.Path == "srv/paisans/f2a9/infra/patroni.env" {
			checked++
			if sum := sha256.Sum256([]byte(h.files["/"+e.Path])); e.SHA256 != hex.EncodeToString(sum[:]) {
				t.Errorf("home-c's manifest records /%s as it was before", e.Path)
			}
		}
	}
	if checked != 2 {
		t.Errorf("home-c's manifest has %d of the two entries", checked)
	}

	again := build(t, w)
	if len(steps(again, 7)) != 0 || again.Pending() {
		var out bytes.Buffer
		again.Print(&out)
		t.Errorf("a finished join plans the replica again:\n%s", out.String())
	}
}

package siteadd_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// world is a small deployment in maps: three hosts, an etcd membership, a
// Patroni cluster and what each HAProxy serves. Every gate in site add is
// about what a command on some host answers, so a fake that answers from
// shared state is the same test as three machines with the failures made
// reachable.
type world struct {
	t        *testing.T
	cfg      *config.Config
	secrets  *config.Secrets
	hosts    map[string]*host
	etcd     []apply.EtcdMember
	nextID   uint64
	running  map[string]bool // etcd running, by site
	patroni  map[string]bool // Patroni running, by site
	members  []map[string]any
	syncMode bool
	// served is the haproxy.cfg each site's running HAProxy read at its
	// last start.
	served map[string]string

	// Knobs that break one thing.
	noPing          bool
	promoteRefusals int
	// addRefusals is how many learner adds etcd refuses as an unhealthy
	// cluster, as it does until every voter has been connected five seconds.
	addRefusals   int
	lags          []float64
	noSyncStandby bool
	replicaUp     bool
	failOnce      string
	// unreachable makes every command containing it fail as ssh does when
	// it cannot connect, after its retries.
	unreachable string
	// unhealthy names an app stack whose container has exited, once it has
	// been started.
	unhealthy string
}

type host struct {
	w        *world
	name     string
	files    map[string]string
	commands []string
	writes   int
	wgUp     bool
}

func (h *host) Describe() string { return h.name }

func (h *host) ReadFile(path string) (string, bool, error) {
	c, ok := h.files[path]
	return c, ok, nil
}

func (h *host) WriteFile(path, content string, mode uint32) error {
	h.files[path] = content
	h.writes++
	return nil
}

func (h *host) RunInput(command, stdin string) (string, error) { return h.Run(command) }

func (h *host) ran(sub string) int {
	n := 0
	for _, c := range h.commands {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

var (
	memberAdd = regexp.MustCompile(`member add (\S+) --peer-urls=(\S+) --learner`)
	promote   = regexp.MustCompile(`member promote ([0-9a-f]+)`)
	serverRe  = regexp.MustCompile(`(?m)^\s+server (\S+) `)
)

func (h *host) Run(command string) (string, error) {
	w := h.w
	h.commands = append(h.commands, command)
	if w.failOnce != "" && strings.Contains(command, w.failOnce) {
		w.failOnce = ""
		return "failed by the test", fmt.Errorf("exit status 1")
	}
	if w.unreachable != "" && strings.Contains(command, w.unreachable) {
		return "ssh: connect to host 203.0.113.10 port 22: Operation timed out\n",
			fmt.Errorf("%s: %w after 3 attempts: exit status 255", h.name, apply.ErrUnreachable)
	}
	switch {
	case strings.HasPrefix(command, "for r in ") && strings.Contains(command, ".Config.Volumes"):
		// What each image declares: nothing, for every image in this world.
		list, _, _ := strings.Cut(strings.TrimPrefix(command, "for r in "), "; do")
		var b strings.Builder
		for _, q := range strings.Fields(list) {
			fmt.Fprintf(&b, "volumes %s null\n", strings.Trim(q, "'"))
		}
		return b.String(), nil
	case strings.HasPrefix(command, "for r in "):
		list, _, _ := strings.Cut(strings.TrimPrefix(command, "for r in "), "; do")
		var b strings.Builder
		for _, q := range strings.Fields(list) {
			ref := strings.Trim(q, "'")
			d := sha256.Sum256([]byte(ref))
			fmt.Fprintf(&b, "present sha256:%s %s\n", hex.EncodeToString(d[:]), ref)
		}
		return b.String(), nil
	case strings.Contains(command, "docker info --format"):
		return "/var/lib/docker\n", nil
	case strings.HasPrefix(command, "df -B1"):
		return "Avail\n999999999999\n", nil
	case w.unhealthy != "" && strings.Contains(command, "/srv/"+w.unhealthy+"/compose.yaml ps --all --format json"):
		return `{"Service":"app","Name":"x","State":"exited","ExitCode":1}` + "\n", nil
	case strings.Contains(command, " ps --all --format json"):
		return `{"Service":"x","Name":"x","State":"running","Health":""}` + "\n", nil
	case strings.Contains(command, "ip link show wg0"):
		if h.wgUp {
			return "up\n", nil
		}
		return "down\n", nil
	case strings.Contains(command, "enable --now wg-quick@wg0"), strings.Contains(command, "restart wg-quick@wg0"):
		h.wgUp = true
		return "", nil
	case strings.HasPrefix(command, "ping "):
		if w.noPing {
			return "100% packet loss", fmt.Errorf("exit status 1")
		}
		return "3 received", nil
	case strings.Contains(command, "latest-handshakes"):
		return w.handshakes(h), nil
	case strings.Contains(command, "--quiet etcd"):
		if !w.running[h.name] {
			return "__PAISANS_NO_ETCD__\n", nil
		}
		return w.memberList(), nil
	case strings.Contains(command, "member list -w json"):
		return w.memberList(), nil
	case memberAdd.MatchString(command) && w.addRefusals > 0:
		w.addRefusals--
		return "Error: etcdserver: unhealthy cluster", fmt.Errorf("exit status 1")
	case memberAdd.MatchString(command):
		m := memberAdd.FindStringSubmatch(command)
		w.nextID++
		w.etcd = append(w.etcd, apply.EtcdMember{ID: w.nextID, PeerURLs: []string{m[2]}, IsLearner: true})
		return "Member added\n", nil
	case strings.HasSuffix(command, "up -d etcd"):
		w.running[h.name] = true
		for i := range w.etcd {
			if w.etcd[i].PeerURLs[0] == render.EtcdPeerURL(w.cfg.Sites[h.name].Address) {
				w.etcd[i].Name = h.name
			}
		}
		return "", nil
	case promote.MatchString(command):
		if w.promoteRefusals > 0 {
			w.promoteRefusals--
			return "etcdserver: can only promote a learner member which is in sync with leader", fmt.Errorf("exit status 1")
		}
		id := promote.FindStringSubmatch(command)[1]
		for i := range w.etcd {
			if w.etcd[i].HexID() == id {
				w.etcd[i].IsLearner = false
			}
		}
		return "promoted\n", nil
	case strings.Contains(command, "endpoint health --cluster"):
		var out []map[string]any
		for _, m := range w.etcd {
			site := apply.EtcdMemberSite(w.cfg, m)
			out = append(out, map[string]any{"endpoint": "http://" + site + ":2379", "health": w.running[site]})
		}
		data, _ := json.Marshal(out)
		return string(data), nil
	case strings.Contains(command, "--quiet patroni"):
		if !w.patroni[h.name] {
			return "__PAISANS_NO_PATRONI__\n", nil
		}
		return w.patroniList(), nil
	case strings.Contains(command, "patronictl") && strings.Contains(command, "list -f json"):
		return w.patroniList(), nil
	case strings.Contains(command, "show-config"):
		return fmt.Sprintf("loop_wait: 10\nsynchronous_mode: %v\n", w.syncMode), nil
	case strings.Contains(command, "edit-config"):
		w.syncMode = true
		if !w.noSyncStandby {
			for _, m := range w.members {
				if m["Role"] == "Replica" {
					m["Role"] = "Sync Standby"
				}
			}
		}
		return "", nil
	case strings.HasSuffix(command, "compose.yaml up -d"):
		if h.name == "home-b" && !w.patroni["home-b"] {
			w.patroni["home-b"] = true
			w.members = append(w.members, map[string]any{"Member": "home-b", "Role": "Replica", "State": "streaming", "Replay Lag": float64(0)})
		}
		return "", nil
	case strings.HasSuffix(command, "restart haproxy"):
		w.served[h.name] = h.files["/srv/infra/haproxy/haproxy.cfg"]
		return "", nil
	case strings.Contains(command, "/stats;csv"):
		return w.stats(h.name)
	}
	return "", nil
}

func (w *world) memberList() string {
	data, _ := json.Marshal(map[string]any{"header": map[string]any{}, "members": w.etcd})
	return string(data)
}

func (w *world) patroniList() string {
	for _, m := range w.members {
		if m["Member"] == "home-b" && len(w.lags) > 0 {
			m["Replay Lag"] = w.lags[0]
			if len(w.lags) > 1 {
				w.lags = w.lags[1:]
			}
		}
	}
	data, _ := json.Marshal(w.members)
	return string(data)
}

func (w *world) stats(site string) (string, error) {
	cfg := w.served[site]
	if !strings.Contains(cfg, "listen stats") {
		return "curl: (7) Failed to connect", fmt.Errorf("exit status 7")
	}
	var b strings.Builder
	b.WriteString("# pxname,svname,qcur,qmax,scur,smax,slim,stot,bin,bout,dreq,dresp,ereq,econ,eresp,wretr,wredis,status,\n")
	b.WriteString("postgres,FRONTEND,,,0,0,500,0,0,0,0,0,0,,,,,OPEN,\n")
	for _, m := range serverRe.FindAllStringSubmatch(cfg, -1) {
		status := "DOWN"
		if m[1] == "home-a" || w.replicaUp {
			status = "UP"
		}
		fmt.Fprintf(&b, "postgres,%s,0,0,0,0,100,0,0,0,,0,,0,0,0,0,%s,\n", m[1], status)
	}
	b.WriteString("postgres,BACKEND,0,0,0,0,50,0,0,0,0,0,,0,0,0,0,UP,\n")
	return b.String(), nil
}

// handshakes answers `wg show wg0 latest-handshakes` from the files: two
// sites have a fresh handshake when each lists the other's key and both
// interfaces are up.
func (w *world) handshakes(h *host) string {
	var b strings.Builder
	mine := h.files["/etc/wireguard/wg0.conf"]
	for _, name := range w.cfg.SiteNames() {
		if name == h.name {
			continue
		}
		other := w.hosts[name]
		if other == nil {
			continue
		}
		key := w.key(name)
		if !strings.Contains(mine, key) {
			continue
		}
		stamp := 0
		if h.wgUp && other.wgUp && strings.Contains(other.files["/etc/wireguard/wg0.conf"], w.key(h.name)) {
			stamp = 990
		}
		fmt.Fprintf(&b, "%s\t%d\n", key, stamp)
	}
	b.WriteString("now 1000\n")
	return b.String()
}

func (w *world) key(site string) string {
	k, err := render.PublicKey(w.secrets.Sites[site].WireGuardPrivateKey)
	if err != nil {
		w.t.Fatal(err)
	}
	return k
}

func (w *world) transports() map[string]apply.Transport {
	out := map[string]apply.Transport{}
	for name, h := range w.hosts {
		out[name] = h
	}
	return out
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

// endState is the staging shaped join: vm is the gateway and gains the
// witness, home-a is data and apps, home-b is new and data only. Every site
// has an endpoint.
func endState(t *testing.T) (*config.Config, *config.Secrets) {
	cfg, secrets := loadFixture(t)
	for name, endpoint := range map[string]string{"home-a": "198.51.100.10:51820", "home-b": "198.51.100.20:51820"} {
		s := cfg.Sites[name]
		s.Endpoint = endpoint
		cfg.Sites[name] = s
	}
	b := cfg.Sites["home-b"]
	b.Roles = []config.Role{config.RoleData}
	cfg.Sites["home-b"] = b
	for name, app := range cfg.Apps {
		if app.Placement.Site == "home-b" {
			app.Placement.Site = "home-a"
			cfg.Apps[name] = app
		}
	}
	cfg.Storage.Garage.Sites = []string{"home-a"}
	cfg.Storage.Garage.Replication = 1
	return cfg, secrets
}

// newWorld is the deployment before the join: home-a alone in the cluster
// and in etcd, vm a gateway only, home-b prepared and empty. home-a and vm
// were applied by a whole apply of the configuration as it was then.
func newWorld(t *testing.T) *world {
	t.Helper()
	cfg, secrets := endState(t)
	w := &world{t: t, cfg: cfg, secrets: secrets, hosts: map[string]*host{}, nextID: 1,
		running: map[string]bool{"home-a": true}, patroni: map[string]bool{"home-a": true},
		served: map[string]string{}, syncMode: false}
	for _, name := range cfg.SiteNames() {
		w.hosts[name] = &host{w: w, name: name, files: map[string]string{}}
	}

	before, _ := endState(t)
	delete(before.Sites, "home-b")
	before.Cluster.Sites = []string{"home-a"}
	before.Etcd.Members = []string{"home-a"}
	vm := before.Sites["vm"]
	vm.Roles = []config.Role{config.RoleGateway}
	before.Sites["vm"] = vm
	rendered, err := render.Build(before, secrets)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"home-a", "vm"} {
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
	w.served["home-a"] = w.hosts["home-a"].files["/srv/infra/haproxy/haproxy.cfg"]
	w.etcd = []apply.EtcdMember{{ID: 1, Name: "home-a", PeerURLs: []string{"http://10.44.0.1:2380"}, ClientURLs: []string{"http://10.44.0.1:2379"}}}
	w.members = []map[string]any{{"Member": "home-a", "Role": "Leader", "State": "running", "Replay Lag": ""}}
	return w
}

func (w *world) voters() []string {
	var out []string
	for _, m := range w.etcd {
		if !m.IsLearner {
			out = append(out, apply.EtcdMemberSite(w.cfg, m))
		}
	}
	sort.Strings(out)
	return out
}

// mustRender renders the end state as the live hosts record it.
func mustRender(t *testing.T, w *world) *render.Plan {
	t.Helper()
	var opts []render.Option
	for name, h := range w.hosts {
		if in, ok := render.ParseEtcdInitial(h.files["/"+render.EtcdInitialPath]); ok {
			opts = append(opts, render.WithEtcdInitial(name, in))
		}
	}
	rendered, err := render.Build(w.cfg, w.secrets, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return rendered
}

package failover

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
)

// world is the cluster every fake host shares: who leads, what each replica
// reports, and whether a switchover takes effect.
type world struct {
	mu        sync.Mutex
	leader    string
	members   []string
	state     map[string]string
	lag       map[string]string
	noSync    bool
	ignore    bool // a switchover that patronictl accepts and Patroni never does
	unhealthy map[string]bool
	ran       []string
	// failRestart names an app whose restart fails.
	failRestart string
}

func newWorld() *world {
	return &world{
		leader:    "home-a",
		members:   []string{"home-a", "home-b"},
		state:     map[string]string{"home-b": "streaming", "home-a": "streaming"},
		lag:       map[string]string{"home-a": "0", "home-b": "0"},
		unhealthy: map[string]bool{},
	}
}

func (w *world) document() string {
	var parts []string
	for _, name := range w.members {
		if name == w.leader {
			parts = append(parts, fmt.Sprintf(`{"name":%q,"role":"leader","state":"running"}`, name))
			continue
		}
		role := "sync_standby"
		if w.noSync {
			role = "replica"
		}
		parts = append(parts, fmt.Sprintf(`{"name":%q,"role":%q,"state":%q,"lag":%s}`, name, role, w.state[name], w.lag[name]))
	}
	return `{"members":[` + strings.Join(parts, ",") + `],"scope":"fixture"}`
}

type host struct {
	name string
	w    *world
}

func (h host) Describe() string { return "ubuntu@" + h.name }

func (h host) Run(command string) (string, error) {
	w := h.w
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ran = append(w.ran, h.name+": "+command)
	switch {
	case strings.Contains(command, "/cluster"):
		return w.document(), nil
	case strings.Contains(command, "patronictl"):
		if !strings.Contains(command, "--leader "+w.leader+" ") {
			return "Error: Member is not the leader", errors.New("exit status 1")
		}
		if !w.ignore {
			i := strings.Index(command, "--candidate ")
			w.leader = strings.Fields(command[i+len("--candidate "):])[0]
		}
		return "Successfully switched over", nil
	case strings.HasSuffix(command, "/compose.yaml restart"):
		if w.failRestart != "" && strings.Contains(command, "/srv/"+w.failRestart+"/") {
			return "Error response from daemon: no such container", errors.New("exit status 1")
		}
		return "", nil
	case strings.Contains(command, " ps --all --format json"):
		stack := strings.TrimPrefix(strings.Fields(command)[3], "/srv/")
		stack = strings.TrimSuffix(stack, "/compose.yaml")
		if w.unhealthy[stack] {
			return `{"Service":"app","State":"restarting","ExitCode":1}`, nil
		}
		return `{"Service":"app","State":"running","Health":"healthy"}`, nil
	}
	return "", fmt.Errorf("fake: no rule for %s", command)
}

func (h host) RunInput(command, _ string) (string, error) { return h.Run(command) }
func (h host) ReadFile(string) (string, bool, error)      { return "", false, nil }
func (h host) WriteFile(string, string, uint32) error     { return errors.New("no writes") }

// setup returns the fixture, a world, options pointing at a local server
// standing in for the gateway, and the paths it was asked for.
func setup(t *testing.T, status func(path string) int) (*config.Config, *world, Options, *[]string) {
	t.Helper()
	cfg, err := config.Load("testdata/cluster.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld()
	var mu sync.Mutex
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		mu.Lock()
		asked = append(asked, req.Host+req.URL.Path)
		mu.Unlock()
		rw.WriteHeader(status(req.URL.Path))
	}))
	t.Cleanup(server.Close)
	transports := map[string]apply.Transport{}
	for _, name := range cfg.SiteNames() {
		transports[name] = host{name: name, w: w}
	}
	o := Options{
		Transports: transports,
		Client:     server.Client(),
		// The hostname rides in a query so the server can be one listener.
		URL: func(hostname, path string) string { return server.URL + path + "?host=" + hostname },
	}
	oldWait, oldPoll, oldLag, oldSleep := gateWait, gatePoll, lagPoll, sleep
	gateWait, gatePoll, lagPoll, sleep = 30*time.Millisecond, 10*time.Millisecond, time.Millisecond, func(time.Duration) {}
	t.Cleanup(func() { gateWait, gatePoll, lagPoll, sleep = oldWait, oldPoll, oldLag, oldSleep })
	return cfg, w, o, &asked
}

func ok(string) int { return http.StatusOK }

func TestDryRunChecksAndChangesNothing(t *testing.T) {
	cfg, w, o, asked := setup(t, ok)
	var out strings.Builder
	o.Out = &out
	if err := Run(cfg, o); err != nil {
		t.Fatal(err)
	}
	for _, c := range w.ran {
		if strings.Contains(c, "patronictl") {
			t.Fatalf("a dry run ran a switchover: %s", c)
		}
	}
	if w.leader != "home-a" {
		t.Fatalf("leader moved to %s", w.leader)
	}
	text := out.String()
	for _, want := range []string{
		"on home-a: " + SwitchoverCommand("home-a", "home-b"),
		"on home-b: " + SwitchoverCommand("home-b", "home-a"),
		"expected interruption",
		"Nothing was changed",
		"auth on home-a healthy",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("dry run does not say %q:\n%s", want, text)
		}
	}
	var discovery, root bool
	for _, a := range *asked {
		discovery = discovery || strings.HasSuffix(a, "/.well-known/openid-configuration")
		root = root || strings.HasSuffix(a, "/")
	}
	if !discovery || !root {
		t.Errorf("apps not asked through the gateway as expected: %q", *asked)
	}
}

func TestSwitchoverCommandFlags(t *testing.T) {
	got := SwitchoverCommand("home-a", "home-b")
	want := "docker compose -f /srv/infra/compose.yaml exec -T patroni patronictl -c /home/postgres/postgres.yml switchover --leader home-a --candidate home-b --force"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestExecuteSwitchesOverAndBack(t *testing.T) {
	cfg, w, o, _ := setup(t, ok)
	o.Execute = true
	var out strings.Builder
	o.Out = &out
	if err := Run(cfg, o); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	var switches []string
	for _, c := range w.ran {
		if strings.Contains(c, "patronictl") {
			switches = append(switches, c)
		}
	}
	if len(switches) != 2 ||
		!strings.HasPrefix(switches[0], "home-a: ") || !strings.Contains(switches[0], "--candidate home-b") ||
		!strings.HasPrefix(switches[1], "home-b: ") || !strings.Contains(switches[1], "--candidate home-a") {
		t.Fatalf("switchovers: %q", switches)
	}
	if w.leader != "home-a" {
		t.Fatalf("leader ended on %s", w.leader)
	}
	if strings.Count(out.String(), "gate passed") != 2 {
		t.Errorf("want two gates passed:\n%s", out.String())
	}
}

func TestPreflightRefuses(t *testing.T) {
	cases := map[string]struct {
		setup  func(w *world)
		status func(string) int
		want   string
	}{
		"replica not streaming": {setup: func(w *world) { w.state["home-b"] = "starting" }, want: "home-b is starting"},
		"lag":                   {setup: func(w *world) { w.lag["home-b"] = "16384" }, want: "lags by 16384 bytes"},
		"lag unknown":           {setup: func(w *world) { w.lag["home-b"] = `"unknown"` }, want: "lags by unknown"},
		"no sync standby":       {setup: func(w *world) { w.noSync = true }, want: "no member is a Sync Standby"},
		"member missing":        {setup: func(w *world) { w.members = []string{"home-a"} }, want: "home-b is not a cluster member"},
		"stack unhealthy":       {setup: func(w *world) { w.unhealthy["blog"] = true }, want: "blog on home-a"},
		"app answers 502": {
			status: func(path string) int {
				if path == "/.well-known/openid-configuration" {
					return http.StatusBadGateway
				}
				return http.StatusFound
			},
			want: "answered 502",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			status := tc.status
			if status == nil {
				status = ok
			}
			cfg, w, o, _ := setup(t, status)
			if tc.setup != nil {
				tc.setup(w)
			}
			o.Execute = true
			var out strings.Builder
			o.Out = &out
			err := Run(cfg, o)
			if err == nil {
				t.Fatalf("not refused:\n%s", out.String())
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("output does not say %q:\n%s", tc.want, out.String())
			}
			for _, c := range w.ran {
				if strings.Contains(c, "patronictl") {
					t.Fatalf("switched over despite a refusal: %s", c)
				}
			}
		})
	}
}

// patronictl reports a failed switchover and exits 0, so only the gate can
// catch it, and it must stop before switching back.
func TestAGateThatDoesNotPassStops(t *testing.T) {
	cfg, w, o, _ := setup(t, ok)
	w.ignore = true
	o.Execute = true
	err := Run(cfg, o)
	if err == nil || !strings.Contains(err.Error(), "leader is home-a, not home-b") {
		t.Fatalf("want the gate to stop on the leader, got %v", err)
	}
	var switches int
	for _, c := range w.ran {
		if strings.Contains(c, "patronictl") {
			switches++
		}
	}
	if switches != 1 {
		t.Fatalf("ran %d switchovers, want the first only", switches)
	}
}

func TestOneDataSiteIsRefused(t *testing.T) {
	cfg, _, o, _ := setup(t, ok)
	cfg.Cluster.Sites = []string{"home-a"}
	if err := Run(cfg, o); err == nil || !strings.Contains(err.Error(), "another data site") {
		t.Fatalf("got %v", err)
	}
}

// After each switch passes its cluster gate, every app that uses the cluster
// database is restarted on every apps site, since a leader change drops every
// client's connection to the old primary wherever the client runs; then the
// apps are checked. A pinned app is left alone.
func TestEachSwitchRestartsTheDatabaseAppsOnEveryAppsSite(t *testing.T) {
	cfg, w, o, _ := setup(t, ok)
	b := cfg.Sites["home-b"]
	b.Roles = append(b.Roles, config.RoleApps)
	cfg.Sites["home-b"] = b
	pinned := cfg.Apps["blog"]
	pinned.Placement = config.Placement{Mode: config.PlacementPinned, Site: "home-a"}
	cfg.Apps["blog"] = pinned
	o.Execute = true
	var out strings.Builder
	o.Out = &out
	if err := Run(cfg, o); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{
		"restart the apps that use the cluster database, on every apps site",
		"on home-a: docker compose -f /srv/auth/compose.yaml restart",
		"on home-b: docker compose -f /srv/auth/compose.yaml restart",
		"restarted auth on home-b",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not say %q:\n%s", want, out.String())
		}
	}

	// Per switch: the switchover, the cluster settling, the restarts on both
	// sites, then the apps gate.
	var switches []int
	for i, c := range w.ran {
		if strings.Contains(c, "patronictl") {
			switches = append(switches, i)
		}
	}
	if len(switches) != 2 {
		t.Fatalf("switchovers at %v", switches)
	}
	for n, start := range switches {
		end := len(w.ran)
		if n+1 < len(switches) {
			end = switches[n+1]
		}
		window := w.ran[start:end]
		restarts := map[string]int{}
		firstRestart, lastRestart, firstPs := -1, -1, -1
		for i, c := range window {
			if strings.HasSuffix(c, "/compose.yaml restart") {
				restarts[c]++
				if firstRestart < 0 {
					firstRestart = i
				}
				lastRestart = i
			}
			if strings.Contains(c, "ps --all") && firstPs < 0 {
				firstPs = i
			}
		}
		if restarts["home-a: docker compose -f /srv/auth/compose.yaml restart"] != 1 || restarts["home-b: docker compose -f /srv/auth/compose.yaml restart"] != 1 || len(restarts) != 2 {
			t.Errorf("switch %d restarted %v", n+1, restarts)
		}
		cluster := -1
		for i, c := range window {
			if strings.Contains(c, "/cluster") {
				cluster = i
				break
			}
		}
		if cluster < 0 || cluster > firstRestart {
			t.Errorf("switch %d restarted the apps before reading the cluster", n+1)
		}
		if firstPs < lastRestart {
			t.Errorf("switch %d checked an app before every restart was done", n+1)
		}
	}
}

// A restart that fails stops the test where it is.
func TestAFailedAppRestartStops(t *testing.T) {
	cfg, w, o, _ := setup(t, ok)
	w.failRestart = "auth"
	o.Execute = true
	err := Run(cfg, o)
	if err == nil || !strings.Contains(err.Error(), "restarting auth on home-a failed") {
		t.Fatalf("got %v", err)
	}
	var switches int
	for _, c := range w.ran {
		if strings.Contains(c, "patronictl") {
			switches++
		}
	}
	if switches != 1 {
		t.Fatalf("ran %d switchovers, want the first only", switches)
	}
}

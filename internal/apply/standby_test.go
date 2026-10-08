package apply_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployment"
)

// instanceHost answers the instance question from a script of states, one
// per look, repeating the last; "unreachable" is an ssh failure.
type instanceHost struct {
	name   string
	states []string
	looks  *int
	asked  []string
}

func (h *instanceHost) Describe() string { return "ubuntu@" + h.name }
func (h *instanceHost) Run(command string) (string, error) {
	h.asked = append(h.asked, command)
	i := *h.looks
	if i >= len(h.states) {
		i = len(h.states) - 1
	}
	if h.states[i] == "unreachable" {
		return "", errors.New("ssh: connect to host " + h.name + ": Connection timed out")
	}
	return h.states[i] + "\n", nil
}
func (h *instanceHost) RunInput(command, _ string) (string, error) { return h.Run(command) }
func (h *instanceHost) ReadFile(string) (string, bool, error)      { return "", false, nil }
func (h *instanceHost) WriteFile(string, string, uint32) error     { return errors.New("no writes") }

func standbyFixture(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// world builds both apps sites' hosts and a sleeper that advances the look.
func standbyWorld(t *testing.T, a, b []string) (map[string]apply.Transport, *int, *[]time.Duration) {
	t.Helper()
	looks := 0
	var slept []time.Duration
	restore := apply.SetStandbyWait(30*time.Second, 5*time.Second, func(d time.Duration) {
		slept = append(slept, d)
		looks++
	})
	t.Cleanup(restore)
	return map[string]apply.Transport{
		"home-a": &instanceHost{name: "home-a", states: a, looks: &looks},
		"home-b": &instanceHost{name: "home-b", states: b, looks: &looks},
	}, &looks, &slept
}

func TestStandbyAppsAreThePocketIDsOnMoreThanOneSite(t *testing.T) {
	if got := apply.StandbyApps(standbyFixture(t)); strings.Join(got, ",") != "auth" {
		t.Fatalf("StandbyApps = %v, want [auth]", got)
	}
}

func TestTheInstanceQuestionAsksTheStateFileThenTheMeshPort(t *testing.T) {
	cmd := apply.InstanceCommand(deployment.Deployment{ID: "f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"}, "auth", "10.44.0.1", 1411)
	for _, want := range []string{
		"if [ ! -f /srv/paisans/f2a9/auth/compose.yaml ]; then echo absent;",
		"docker ps -q --filter label=community.paisans.deployment=f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01 --filter label=com.docker.compose.project=paisans-f2a9-auth --filter label=com.docker.compose.service=app",
		`docker exec "$c" test -f /tmp/paisans-standby`,
		"curl --silent --fail --max-time 5 --output /dev/null http://10.44.0.1:1411/healthz",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the command lacks %q:\n%s", want, cmd)
		}
	}
	if strings.Index(cmd, "paisans-standby") > strings.Index(cmd, "/healthz") {
		t.Error("the state file must be asked before /healthz: a standby's port refuses, so the other order would call it down")
	}
	if strings.Contains(cmd, "docker compose") {
		t.Error("the state file must not be asked through docker compose, which reads the root only .env and fails without sudo")
	}
}

func TestOneActiveAndOneStandbyPasses(t *testing.T) {
	transports, _, slept := standbyWorld(t, []string{"active"}, []string{"standby"})
	var out bytes.Buffer
	if err := apply.CheckOneActive(standbyFixture(t), "auth", transports, &out); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 0 {
		t.Errorf("waited %v with one active already", *slept)
	}
	for _, want := range []string{"pocket-id auth: one active instance", "home-a   active      /healthz answered on 10.44.0.1:1411", "home-b   standby     another instance holds the database"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// A takeover in progress: nobody active for two looks, then the standby.
func TestNoActiveIsWaitedFor(t *testing.T) {
	transports, looks, _ := standbyWorld(t, []string{"down", "down", "standby"}, []string{"standby", "down", "active"})
	if err := apply.CheckOneActive(standbyFixture(t), "auth", transports, nil); err != nil {
		t.Fatal(err)
	}
	if *looks != 2 {
		t.Errorf("passed after %d sleeps, want 2", *looks)
	}
}

func TestNoActiveFailsAfterTheWait(t *testing.T) {
	transports, _, slept := standbyWorld(t, []string{"standby"}, []string{"down"})
	var out bytes.Buffer
	err := apply.CheckOneActive(standbyFixture(t), "auth", transports, &out)
	if err == nil || !strings.Contains(err.Error(), "no site has an active instance within 30s") {
		t.Fatalf("err = %v", err)
	}
	if len(*slept) != 5 {
		t.Errorf("slept %d times, want 5 (30s at 5s, the last look not followed by a sleep)", len(*slept))
	}
	if !strings.Contains(out.String(), "home-b   down") {
		t.Errorf("the last look is not shown:\n%s", out.String())
	}
}

// Two active is what the standby exists to prevent, and waiting would leave
// both serving, so it fails on the first look.
func TestTwoActiveFailAtOnce(t *testing.T) {
	transports, _, slept := standbyWorld(t, []string{"active"}, []string{"active"})
	err := apply.CheckOneActive(standbyFixture(t), "auth", transports, nil)
	if err == nil || !strings.Contains(err.Error(), "2 sites have an active instance (home-a, home-b)") {
		t.Fatalf("err = %v", err)
	}
	if len(*slept) != 0 {
		t.Errorf("waited %v before failing on two active", *slept)
	}
}

// The first apply of a deployment reaches one site before the other has a
// stack: that site's instance is the one, and the other is not a failure.
func TestAnAbsentSiteIsNotCounted(t *testing.T) {
	transports, _, _ := standbyWorld(t, []string{"active"}, []string{"absent"})
	var out bytes.Buffer
	if err := apply.CheckOneActive(standbyFixture(t), "auth", transports, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "home-b   absent      no /srv/paisans/f2a9/auth on this site yet") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestAnUnreachableSiteIsReportedNotCounted(t *testing.T) {
	transports, _, _ := standbyWorld(t, []string{"unreachable"}, []string{"active"})
	var out bytes.Buffer
	if err := apply.CheckOneActive(standbyFixture(t), "auth", transports, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "home-a   unreachable ssh: connect to host home-a") {
		t.Errorf("output:\n%s", out.String())
	}
}

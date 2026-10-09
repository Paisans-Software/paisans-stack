package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
)

// edgeHost is a host with a host network Caddy on 443 whose Caddyfile, bind
// mounted on its own, imports nothing.
type edgeHost struct {
	files map[string]string
	ran   []string
}

const edgeCaddyID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func (h *edgeHost) Describe() string { return "ubuntu@watch.example.org" }
func (h *edgeHost) Run(command string) (string, error) {
	h.ran = append(h.ran, command)
	switch {
	case strings.Contains(command, "docker inspect"):
		return fmt.Sprintf(`{"id":%q,"image":"caddy:2.11","path":"caddy","args":["run","--config","/etc/caddy/Caddyfile","--adapter","caddyfile"],"network_mode":"host","running":true,"networks":{"host":{}},"mounts":[{"Type":"bind","Source":"/opt/web/Caddyfile","Destination":"/etc/caddy/Caddyfile"}]}`, edgeCaddyID), nil
	case strings.Contains(command, "caddy version"):
		return "v2.11.4 h1:abc=", nil
	case strings.Contains(command, "caddy validate"), strings.Contains(command, "caddy reload"), strings.HasPrefix(command, "cp -p "):
		return "", nil
	case strings.Contains(command, "curl "):
		return "200", nil
	}
	return "", fmt.Errorf("fake: no answer for %q", command)
}
func (h *edgeHost) RunInput(command, stdin string) (string, error) {
	h.ran = append(h.ran, command)
	_, path, _ := strings.Cut(command, "cat > ")
	h.files[strings.Trim(path, "'")] = stdin
	return "", nil
}
func (h *edgeHost) ReadFile(path string) (string, bool, error) {
	c, ok := h.files[path]
	return c, ok, nil
}
func (h *edgeHost) WriteFile(path, content string, mode uint32) error {
	return errors.New("fake: the edge step never writes through WriteFile")
}

const edgeCaddyfile = "blog.example.org {\n\treverse_proxy 127.0.0.1:2368\n}\n"

func edgeFixture(t *testing.T, only []string, approved bool) (*edgeStep, *edgeHost) {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "internal", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	watch := cfg.Sites["watch"]
	watch.Ingress = &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"}
	cfg.Sites["watch"] = watch
	_, mesh, _ := net.ParseCIDR(cfg.Mesh.Subnet)
	inv := &hostcheck.Inventory{
		Host:       "ubuntu@watch.example.org",
		Containers: []hostcheck.Container{{ID: edgeCaddyID, Name: "caddy", Project: "web", PID: 900, Networks: []string{"host"}}},
		Sockets:    []hostcheck.Socket{{Proto: "tcp", Port: 443, Process: "caddy", PID: 900}},
		Cgroups:    map[int]string{900: edgeCaddyID},
	}
	report := hostcheck.Classify(hostcheck.Claims{Site: "watch", Deployment: cfg.Deployment(), Mesh: mesh, Interface: cfg.Deployment().Interface()}, inv)
	h := &edgeHost{files: map[string]string{"/opt/web/Caddyfile": edgeCaddyfile}}
	step, options, err := planEdge(cfg, "watch", report, h, only, approved)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 0 {
		t.Errorf("a host network Caddy needs no render option, got %d", len(options))
	}
	savedWait, savedSleep := edgeProbeWait, edgeSleep
	edgeProbeWait, edgeSleep = 0, func(time.Duration) {}
	t.Cleanup(func() { edgeProbeWait, edgeSleep = savedWait, savedSleep })
	return step, h
}

func noTerminal(t *testing.T) {
	saved := openTTY
	openTTY = func() (*os.File, error) { return nil, errors.New("no tty") }
	t.Cleanup(func() { openTTY = saved })
}

func answer(t *testing.T, yes bool) *string {
	t.Helper()
	savedTTY, savedTerm, savedConfirm := openTTY, isTerminal, confirmOnTerminal
	openTTY = func() (*os.File, error) { return os.Open(os.DevNull) }
	isTerminal = func(*os.File) bool { return true }
	var shown string
	confirmOnTerminal = func(show, question string) (bool, error) {
		shown = show + question
		return yes, nil
	}
	t.Cleanup(func() { openTTY, isTerminal, confirmOnTerminal = savedTTY, savedTerm, savedConfirm })
	return &shown
}

// With no terminal to ask on, nothing is written to the server's
// configuration, the rest of the run finishes, and the run ends in an error
// that prints the block and says how to finish.
func TestEdgeWithNoTerminalWritesNothing(t *testing.T) {
	step, h := edgeFixture(t, nil, false)
	noTerminal(t)
	if !step.pending() {
		t.Fatal("nothing planned")
	}
	var out bytes.Buffer
	if err := step.execute(h, &out); err != nil {
		t.Fatal(err)
	}
	if h.files["/opt/web/Caddyfile"] != edgeCaddyfile || len(h.files) != 1 {
		t.Fatalf("written: %v", h.files)
	}
	for _, c := range h.ran {
		if strings.Contains(c, "caddy reload") || strings.Contains(c, "cp -p") {
			t.Errorf("ran %q", c)
		}
	}
	err := step.result(&out)
	if err == nil || !strings.Contains(err.Error(), "no terminal") || !strings.Contains(err.Error(), "--approve-external-proxy") {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(out.String(), "held back from this apply:") || !strings.Contains(out.String(), "+status.example.org {") {
		t.Errorf("printed:\n%s", out.String())
	}
}

// A no on the terminal writes nothing and is reported as held back, not as
// a failure: the operator chose it.
func TestEdgeAnsweredNoWritesNothing(t *testing.T) {
	step, h := edgeFixture(t, nil, false)
	shown := answer(t, false)
	var out bytes.Buffer
	if err := step.execute(h, &out); err != nil {
		t.Fatal(err)
	}
	if h.files["/opt/web/Caddyfile"] != edgeCaddyfile {
		t.Fatal("written after a no")
	}
	if !strings.Contains(*shown, "+status.example.org {") || !strings.Contains(*shown, "not this toolkit's") {
		t.Errorf("the prompt showed:\n%s", *shown)
	}
	if err := step.result(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "because the answer was no") {
		t.Errorf("printed:\n%s", out.String())
	}
}

// A yes writes, validates, reloads and checks the hostname.
func TestEdgeAnsweredYesWritesAndReloads(t *testing.T) {
	step, h := edgeFixture(t, nil, false)
	answer(t, true)
	var out bytes.Buffer
	if err := step.execute(h, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.files["/opt/web/Caddyfile"], "reverse_proxy 127.0.0.1:8480") {
		t.Fatalf("not written:\n%s", h.files["/opt/web/Caddyfile"])
	}
	for _, want := range []string{"backed up", "validated", "reloaded", "answered  https://status.example.org/healthz with 200"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
	if err := step.result(&out); err != nil {
		t.Fatal(err)
	}
}

// --approve-external-proxy answers yes without asking.
func TestEdgeApprovedIsNotAsked(t *testing.T) {
	step, h := edgeFixture(t, nil, true)
	noTerminal(t)
	saved := confirmOnTerminal
	confirmOnTerminal = func(string, string) (bool, error) { t.Fatal("asked"); return false, nil }
	t.Cleanup(func() { confirmOnTerminal = saved })
	if err := step.execute(h, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.files["/opt/web/Caddyfile"], "# BEGIN paisans-f2a9") {
		t.Fatal("not written")
	}
}

// --only leaving the monitor out leaves its server alone too.
func TestEdgeSkippedByOnly(t *testing.T) {
	step, _ := edgeFixture(t, []string{"infra"}, false)
	if step.pending() {
		t.Fatal("pending under --only infra")
	}
	var out bytes.Buffer
	step.print(&out)
	if !strings.Contains(out.String(), "--only leaves out") {
		t.Errorf("printed:\n%s", out.String())
	}
}

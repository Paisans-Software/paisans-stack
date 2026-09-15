package apply_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/josephquigley/paisans-stack/internal/apply"
	"github.com/josephquigley/paisans-stack/internal/config"
	"github.com/josephquigley/paisans-stack/internal/render"
)

// fakeHost is a machine in a map. Every gate this package has is about what is
// already on a host and what a command returns, so a fake is not a weaker test
// than a real machine: it is the same test with the failures made reachable.
type fakeHost struct {
	files    map[string]string
	commands []string
	// fail makes any command containing this substring return non zero, which
	// is how the gates are exercised.
	fail string
}

func newHost() *fakeHost { return &fakeHost{files: map[string]string{}} }

func (h *fakeHost) Describe() string { return "fake" }

func (h *fakeHost) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	if h.fail != "" && strings.Contains(command, h.fail) {
		return "refused by the fake host", fmt.Errorf("exit status 1")
	}
	return "", nil
}

func (h *fakeHost) ReadFile(path string) (string, bool, error) {
	content, ok := h.files[path]
	return content, ok, nil
}

func (h *fakeHost) WriteFile(path, content string, mode uint32) error {
	h.files[path] = content
	return nil
}

func (h *fakeHost) ran(substring string) bool {
	for _, command := range h.commands {
		if strings.Contains(command, substring) {
			return true
		}
	}
	return false
}

func plan(t *testing.T) *render.Plan {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(filepath.Join("..", "render", "testdata", "secrets.fixture.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	built, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	return built
}

// A first apply creates everything and records what it wrote. The record is
// what every later gate depends on.
func TestFirstApplyCreatesAndRecords(t *testing.T) {
	host := newHost()
	rendered := plan(t)

	p, err := apply.Build("home-a", rendered, host)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Conflicts()) != 0 {
		t.Fatalf("an empty host produced conflicts: %v", p.Conflicts())
	}
	for _, change := range p.Changes {
		if change.Kind != apply.Create {
			t.Errorf("%s on an empty host is %s, want create", change.Path, change.Kind)
		}
		if !strings.HasPrefix(change.Path, "/") {
			t.Errorf("%s is not an absolute path on the host", change.Path)
		}
	}

	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if _, ok := host.files["/srv/talk/.env"]; !ok {
		t.Error("an app's environment was not written")
	}
	if _, ok := host.files["/srv/.paisans-manifest.json"]; !ok {
		t.Fatal("no manifest was recorded, so the next apply cannot tell its own writes from an edit")
	}

	// Applying the same thing twice writes nothing and runs nothing. An apply
	// that restarted containers on every run would make "apply" a thing
	// operators avoid.
	second, err := apply.Build("home-a", rendered, host)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Writes()) != 0 {
		t.Errorf("a second apply would rewrite %d file(s)", len(second.Writes()))
	}
	if len(second.Actions) != 0 {
		t.Errorf("a second apply would restart %v", second.Actions)
	}
}

// A file edited on the host is a conflict, and a conflict stops the whole
// apply rather than overwriting the edit or applying around it.
func TestAnEditOnTheHostIsRefused(t *testing.T) {
	host := newHost()
	rendered := plan(t)
	first, err := apply.Build("home-a", rendered, host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}

	host.files["/srv/talk/.env"] += "\nSOMEONE_EDITED_THIS=1\n"
	host.commands = nil

	p, err := apply.Build("home-a", rendered, host)
	if err != nil {
		t.Fatal(err)
	}
	conflicts := p.Conflicts()
	if len(conflicts) != 1 || conflicts[0].Path != "/srv/talk/.env" {
		t.Fatalf("expected one conflict on the edited file, got %v", conflicts)
	}

	err = apply.Execute(p, host)
	if err == nil {
		t.Fatal("an apply overwrote a file that was edited on the host")
	}
	if !strings.Contains(err.Error(), "/srv/talk/.env") {
		t.Errorf("the refusal does not name the file:\n%v", err)
	}
	if strings.Contains(host.files["/srv/talk/.env"], "SOMEONE_EDITED_THIS") == false {
		t.Error("the edited file was overwritten despite the refusal")
	}
	if len(host.commands) != 0 {
		t.Errorf("a refused apply still ran %v", host.commands)
	}
}

// A file the host has that we never wrote is somebody else's, and is treated
// as a conflict rather than adopted. This is the case on a host that was set up
// by hand before the toolkit existed, which is every early deployment.
func TestAPreexistingFileIsNotAdopted(t *testing.T) {
	host := newHost()
	host.files["/srv/talk/.env"] = "HAND_WRITTEN=1\n"

	p, err := apply.Build("home-a", plan(t), host)
	if err != nil {
		t.Fatal(err)
	}
	conflicts := p.Conflicts()
	if len(conflicts) != 1 || conflicts[0].Path != "/srv/talk/.env" {
		t.Fatalf("a hand written file was not treated as a conflict: %v", conflicts)
	}
}

// The narrower action is taken: a bind mounted configuration file needs a
// restart at most, and only an environment or compose change needs the
// container replaced, because Compose passes environment at start.
func TestTheNarrowerActionIsChosen(t *testing.T) {
	host := newHost()
	rendered := plan(t)
	first, err := apply.Build("home-a", rendered, host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}

	// Blog is WriteFreely: a bind mounted config.ini and no environment.
	host.files["/srv/blog/config.ini"] = "; drifted\n"
	// Talk's environment changed, which a restart cannot pick up.
	host.files["/srv/talk/.env"] = "DRIFTED=1\n"
	// Pretend both drifts were ours, so this test is about the action rather
	// than about the conflict gate, which has its own test.
	adopt(t, host, "/srv/blog/config.ini", "/srv/talk/.env")

	p, err := apply.Build("home-a", rendered, host)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, action := range p.Actions {
		actions[action.Stack] = action.Recreate
		if action.Reason == "" {
			t.Errorf("%s would act with no reason given", action.Stack)
		}
	}
	if recreate, ok := actions["blog"]; !ok {
		t.Error("a changed config.ini produced no action at all")
	} else if recreate {
		t.Error("a bind mounted file change recreated the container, which is an outage it did not need")
	}
	if recreate, ok := actions["talk"]; !ok {
		t.Error("a changed .env produced no action")
	} else if !recreate {
		t.Error("a changed .env only restarted, and Compose passes environment at start, so the new value would not be live")
	}
}

// The gateway file is assembled from per app snippets, so a wrong snippet is a
// wrong configuration for every hostname at once. It is validated before any
// reload, and a failure stops the reload rather than being logged.
func TestAnInvalidGatewayConfigurationIsNotReloaded(t *testing.T) {
	host := newHost()
	host.fail = "caddy validate"

	p, err := apply.Build("vm", plan(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if !p.GatewayReload {
		t.Fatal("a gateway site with new routing did not plan a reload")
	}
	err = apply.Execute(p, host)
	if err == nil {
		t.Fatal("a configuration that does not validate was reloaded anyway")
	}
	if !strings.Contains(err.Error(), "does not validate") {
		t.Errorf("the error does not say what happened:\n%v", err)
	}
	if host.ran("caddy reload") {
		t.Error("the gateway was reloaded after validation failed")
	}
}

// adopt records the host's current content as ours, which is what a successful
// apply does. Tests about something other than the conflict gate use it to get
// past that gate honestly, rather than by disabling it.
func adopt(t *testing.T, host *fakeHost, paths ...string) {
	t.Helper()
	var manifest render.Manifest
	if err := json.Unmarshal([]byte(host.files["/srv/.paisans-manifest.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		rel := strings.TrimPrefix(path, "/")
		digest := sha256.Sum256([]byte(host.files[path]))
		replaced := false
		for i, file := range manifest.Files {
			if file.Path == rel {
				manifest.Files[i].SHA256 = hex.EncodeToString(digest[:])
				replaced = true
			}
		}
		if !replaced {
			t.Fatalf("%s is not in the manifest, so this test is adopting a file no apply wrote", path)
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	host.files["/srv/.paisans-manifest.json"] = string(data)
}

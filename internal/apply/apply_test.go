package apply_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/acme"
	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/render"
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
	// running is whether this host already has a gateway container up. A fresh
	// host has none, which is the ordinary case on a first apply and the one
	// that used to make apply impossible to complete.
	running bool
}

func newHost() *fakeHost { return &fakeHost{files: map[string]string{}} }

func (h *fakeHost) Describe() string { return "fake" }

func (h *fakeHost) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	if h.fail != "" && strings.Contains(command, h.fail) {
		return "refused by the fake host", fmt.Errorf("exit status 1")
	}
	if strings.Contains(command, "ps --status running") {
		if h.running {
			return "c0ffee\n", nil
		}
		// What `docker compose ps --quiet` prints when the service has no
		// running container: nothing, and a zero exit.
		return "\n", nil
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

// planWith renders the fixture with one thing changed, for a test about what
// an apply does when a single file moves.
func planWith(t *testing.T, change func(*config.Config)) *render.Plan {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := config.LoadSecrets(filepath.Join("..", "render", "testdata", "secrets.fixture.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	change(cfg)
	built, err := render.Build(cfg, secrets)
	if err != nil {
		t.Fatal(err)
	}
	return built
}

// acmeModule is the module this fixture's declared provider needs, computed
// the same way cmd/paisans/main.go computes it for a real apply. The fixture
// declares "desec" (internal/render/testdata/deployment.yaml).
func acmeModule(t *testing.T) string {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return acme.Module(cfg.ACME.Provider)
}

// A first apply creates everything and records what it wrote. The record is
// what every later gate depends on.
func TestFirstApplyCreatesAndRecords(t *testing.T) {
	host := newHost()
	rendered := plan(t)

	p, err := apply.Build("home-a", rendered, acmeModule(t), host)
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
	second, err := apply.Build("home-a", rendered, acmeModule(t), host)
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
	first, err := apply.Build("home-a", rendered, acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}

	host.files["/srv/talk/.env"] += "\nSOMEONE_EDITED_THIS=1\n"
	host.commands = nil

	p, err := apply.Build("home-a", rendered, acmeModule(t), host)
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

	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
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
	first, err := apply.Build("home-a", rendered, acmeModule(t), host)
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

	p, err := apply.Build("home-a", rendered, acmeModule(t), host)
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

	p, err := apply.Build("vm", plan(t), acmeModule(t), host)
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

// A gateway is only reloaded once its binary is known to carry the provider's
// module. This is the only check in the whole change that is evidence rather
// than inference: an image reference cannot tell you what was compiled into it.
func TestAGatewayWithoutItsProviderModuleIsNotReloaded(t *testing.T) {
	host := newHost()
	host.fail = "list-modules"

	p, err := apply.Build("vm", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if p.ACMEModule == "" {
		t.Fatal("a gateway plan carries no module to check for")
	}

	err = apply.Execute(p, host)
	if err == nil {
		t.Fatal("a gateway was reloaded without the module its configuration needs")
	}
	if !strings.Contains(err.Error(), p.ACMEModule) {
		t.Errorf("the refusal does not name the missing module:\n%v", err)
	}
	if host.ran("caddy reload") {
		t.Error("the gateway was reloaded after the module check failed")
	}
	if host.ran("caddy validate") {
		t.Error("the configuration was validated before the binary was known to support it, which wastes the clearer error")
	}
}

// A first apply to a fresh host has nothing running: every file is a create,
// and the container that would serve them is started by the Actions loop at
// the very end. Validating or reloading through `exec` there asks a container
// that does not exist yet, which made `paisans apply` unable to complete a
// first install of a gateway site, and made recovery impossible afterwards,
// since a dead gateway makes every later apply refuse at the same step.
func TestAFirstApplyToAFreshHostCompletes(t *testing.T) {
	host := newHost()
	host.running = false

	p, err := apply.Build("vm", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatalf("a first apply to a host with nothing running was aborted: %v", err)
	}

	if !host.ran("caddy validate") {
		t.Error("the gateway configuration was never validated")
	}
	if host.ran("exec -T caddy caddy validate") {
		t.Error("validation ran through exec, which needs a container this host does not have")
	}
	if !host.ran("run --rm --no-deps --entrypoint caddy caddy validate") {
		t.Error("validation did not use the form that works without a running container")
	}
	if host.ran("caddy reload") {
		t.Error("a container that does not exist was told to reload")
	}
	if !host.ran("up -d") {
		t.Error("the infrastructure stack was never started, so the validated configuration is not live")
	}
}

// A gateway that is already up is reloaded rather than left, since the Actions
// loop may only restart a stack whose environment did not change, and a bind
// mounted Caddyfile change would otherwise not be picked up.
func TestARunningGatewayIsReloaded(t *testing.T) {
	host := newHost()
	first, err := apply.Build("vm", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}

	// A routing change and nothing else: a new hostname for an app.
	moved := planWith(t, func(cfg *config.Config) {
		app := cfg.Apps["blog"]
		app.Hostname = "words.example.org"
		cfg.Apps["blog"] = app
	})
	host.running = true
	host.commands = nil

	p, err := apply.Build("vm", moved, acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if !p.GatewayReload {
		t.Fatal("a routing change planned no reload")
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if !host.ran("caddy reload") {
		t.Error("a running gateway was not reloaded, so the new routing is not live")
	}
}

// Bumping the gateway's image changes exactly one file, srv/infra/compose.yaml,
// and that is an environment path rather than a routing one: no routing file
// changes, so nothing is reloaded and the container is replaced by the Actions
// loop instead. `up -d` returns as soon as the container starts, so a Caddy
// without the module would die a moment later and the apply would report
// success. The check has to run for this path, and has to run before the
// recreate.
func TestAnImageOnlyChangeIsStillChecked(t *testing.T) {
	host := newHost()
	first, err := apply.Build("vm", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}

	// An image carrying the same provider, declared rather than published:
	// the one line the sibling repository's digest bump would move.
	moved := planWith(t, func(cfg *config.Config) {
		cfg.ACME.Image = "ghcr.io/example-org/caddy-desec:2.11.4"
	})
	host.commands = nil
	host.fail = "list-modules"

	p, err := apply.Build("vm", moved, acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	writes := p.Writes()
	if len(writes) != 1 || writes[0].Path != "/srv/infra/compose.yaml" {
		t.Fatalf("an image bump should move one file, got %v", writes)
	}
	if p.GatewayReload {
		t.Error("an image change planned a reload, but a new image arrives by recreate")
	}
	if !p.GatewayChanging {
		t.Fatal("the gateway's own compose file changed and the plan does not call that a gateway change")
	}
	if p.ACMEModule == "" {
		t.Fatal("an image change carries no module to check for, so the image is taken on trust")
	}

	err = apply.Execute(p, host)
	if err == nil {
		t.Fatal("a gateway image with no DNS module was deployed anyway")
	}
	if !strings.Contains(err.Error(), p.ACMEModule) {
		t.Errorf("the refusal does not name the missing module:\n%v", err)
	}
	if host.ran("up -d") {
		t.Error("the gateway container was replaced although the new image failed the check")
	}
}

// A provider the toolkit publishes no image for is the one the check exists
// for: the adopter built or chose that image themselves and nobody here has
// run it. The gate has to fire for it exactly as it does for a published
// provider, which it only can because acme.Module names a module for it.
func TestAnUnknownProvidersModuleIsCheckedToo(t *testing.T) {
	module := acme.Module("route53")
	if module == "" {
		t.Fatal("an unknown provider has no module, so there is nothing for apply to check")
	}

	host := newHost()
	host.fail = "list-modules"

	p, err := apply.Build("vm", plan(t), module, host)
	if err != nil {
		t.Fatal(err)
	}
	if p.ACMEModule != module {
		t.Fatalf("the plan carries module %q, want %q", p.ACMEModule, module)
	}

	err = apply.Execute(p, host)
	if err == nil {
		t.Fatal("a gateway running an adopter's own image was reloaded without checking it")
	}
	if !strings.Contains(err.Error(), module) {
		t.Errorf("the refusal does not name the missing module:\n%v", err)
	}
	if host.ran("caddy reload") {
		t.Error("the gateway was reloaded after the module check failed")
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

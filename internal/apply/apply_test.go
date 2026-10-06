package apply_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// wgUp is whether wg0 exists. Starting or restarting the unit brings it
	// up, the way systemd would.
	wgUp bool
	// inputs is what each RunInput call sent on stdin, in order, beside its
	// command in commands.
	inputs []string
	// leader is the Patroni member /cluster reports as leader, empty for a
	// cluster with none yet. newHost makes it home-a.
	leader string
}

func newHost() *fakeHost { return &fakeHost{files: map[string]string{}, leader: "home-a"} }

func (h *fakeHost) RunInput(command, stdin string) (string, error) {
	h.inputs = append(h.inputs, stdin)
	out, err := h.Run(command)
	if err != nil {
		// psql quotes the failing line back, so a failure's output can carry
		// whatever was sent. The fake does the worst version of that.
		return out + "\n" + stdin, err
	}
	return out, nil
}

func (h *fakeHost) Describe() string { return "fake" }

func (h *fakeHost) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	if h.fail != "" && strings.Contains(command, h.fail) {
		return "refused by the fake host", fmt.Errorf("exit status 1")
	}
	if strings.Contains(command, ":8008/cluster") {
		if h.leader == "" {
			return `{"members":[{"name":"home-a","role":"replica","state":"starting"}]}`, nil
		}
		return fmt.Sprintf(`{"members":[{"name":%q,"role":"leader","state":"running"},{"name":"other","role":"replica","state":"streaming"}],"scope":"fixture"}`, h.leader), nil
	}
	if strings.HasPrefix(command, "rm -f ") {
		delete(h.files, strings.Trim(strings.TrimPrefix(command, "rm -f "), "'"))
		return "", nil
	}
	if strings.Contains(command, "ip link show wg0") {
		if h.wgUp {
			return "up\n", nil
		}
		return "down\n", nil
	}
	if strings.Contains(command, "enable --now wg-quick@wg0") || strings.Contains(command, "restart wg-quick@wg0") {
		h.wgUp = true
		return "", nil
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

	p, err := apply.Build("home-a", rendered, acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	// Build may probe, which reads and changes nothing. What is under test is
	// that a refused Execute runs nothing at all.
	host.commands = nil
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

	// Blog is WriteFreely: the application itself reads no environment, so
	// everything it is configured with is in this bind mounted file. It does
	// now render a .env as well, but only for the Postgres container's own
	// password, and that file is left alone here so that the action under
	// test comes from the bind mount and nothing else.
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

// indexOf returns where the first command containing substring ran, or -1.
func (h *fakeHost) indexOf(substring string) int {
	for i, command := range h.commands {
		if strings.Contains(command, substring) {
			return i
		}
	}
	return -1
}

// firstCompose is where the first Docker Compose command ran, or -1.
func (h *fakeHost) firstCompose() int { return h.indexOf("docker compose") }

// applied runs a first apply of the fixture on site, so that a test about a
// later apply starts from a host that has everything and records it.
func applied(t *testing.T, site string) *fakeHost {
	t.Helper()
	host := newHost()
	first, err := apply.Build(site, plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}
	host.commands = nil
	return host
}

// Every service binds the site's mesh address, so on a first apply wg0 has to
// be up before anything is started or checked. It used to be written and
// never started at all, which left every container failing to bind.
func TestAFirstApplyBringsUpTheMeshFirst(t *testing.T) {
	for _, site := range []string{"home-a", "vm"} {
		host := newHost()
		p, err := apply.Build(site, plan(t), acmeModule(t), host)
		if err != nil {
			t.Fatal(err)
		}
		if p.WireGuard != apply.WireGuardStart {
			t.Fatalf("%s: a first apply plans %v for wg0, want a start", site, p.WireGuard)
		}
		if err := apply.Execute(p, host); err != nil {
			t.Fatal(err)
		}
		start := host.indexOf("systemctl enable --now wg-quick@wg0")
		if start < 0 {
			t.Fatalf("%s: wg0 was written and never started", site)
		}
		if compose := host.firstCompose(); compose >= 0 && compose < start {
			t.Errorf("%s: %q ran before wg0 was up", site, host.commands[compose])
		}
	}
}

// A dry run may ask whether wg0 is up, and must do nothing else to it.
func TestPlanningOnlyProbesTheMesh(t *testing.T) {
	host := applied(t, "home-a")
	host.wgUp = false
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if p.WireGuard != apply.WireGuardStart {
		t.Errorf("an unchanged wg0.conf on a host whose wg0 is down plans %v, want a start", p.WireGuard)
	}
	for _, command := range host.commands {
		if !strings.Contains(command, "ip link show wg0") {
			t.Errorf("building a plan ran %q, which is more than a probe", command)
		}
	}
}

// An unchanged file with the interface up is nothing to do: the second apply
// of the same thing runs nothing.
func TestAnUpMeshWithAnUnchangedFileIsLeftAlone(t *testing.T) {
	host := applied(t, "home-a")
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if p.WireGuard != apply.WireGuardNone {
		t.Errorf("an up wg0 with an unchanged file plans %v", p.WireGuard)
	}
}

// A peer change is applied in place. Restarting the interface would partition
// etcd and Patroni for as long as it is down, which on a data site can be long
// enough to start an election.
func TestAPeerChangeIsSyncedNotRestarted(t *testing.T) {
	host := applied(t, "home-a")
	host.files["/etc/wireguard/wg0.conf"] = strings.Replace(
		host.files["/etc/wireguard/wg0.conf"], "PersistentKeepalive = 25", "PersistentKeepalive = 30", 1)
	adopt(t, host, "/etc/wireguard/wg0.conf")

	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if p.WireGuard != apply.WireGuardSync {
		t.Fatalf("a peer change plans %v, want a sync", p.WireGuard)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if !host.ran("wg syncconf wg0") {
		t.Error("the new peers were never handed to wg0")
	}
	if host.ran("restart wg-quick@wg0") {
		t.Error("a peer change took the mesh down")
	}
}

// An address is applied by wg-quick, not by wg, so syncconf would silently
// leave the old one in place. That change needs a restart.
func TestAnAddressChangeRestartsTheMesh(t *testing.T) {
	host := applied(t, "home-a")
	host.files["/etc/wireguard/wg0.conf"] = strings.Replace(
		host.files["/etc/wireguard/wg0.conf"], "Address = 10.44.0.1/24", "Address = 10.44.0.9/24", 1)
	adopt(t, host, "/etc/wireguard/wg0.conf")

	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if p.WireGuard != apply.WireGuardRestart {
		t.Fatalf("an address change plans %v, want a restart", p.WireGuard)
	}
}

// A mesh that will not come up stops the apply before any container moves,
// since every one of them would fail to bind.
func TestAMeshThatWillNotStartStopsTheApply(t *testing.T) {
	host := newHost()
	host.fail = "wg-quick@wg0"
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	err = apply.Execute(p, host)
	if err == nil {
		t.Fatal("an apply carried on with wg0 down")
	}
	if !strings.Contains(err.Error(), "wg0") {
		t.Errorf("the error does not name the interface:\n%v", err)
	}
	if host.firstCompose() >= 0 {
		t.Errorf("a container was touched with the mesh down: %v", host.commands)
	}
	if _, ok := host.files["/srv/.paisans-manifest.json"]; ok {
		t.Error("a failed apply recorded a manifest")
	}
}

// The infrastructure stack is acted on before any app stack. Sorted order
// alone put "blog" and "docs" ahead of "infra", starting applications before
// the proxy and database they connect to.
func TestInfrastructureMovesBeforeApps(t *testing.T) {
	host := newHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Actions) == 0 || p.Actions[0].Stack != "infra" {
		t.Fatalf("the first action is not the infrastructure stack: %v", p.Actions)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	infra := host.indexOf("/srv/infra/compose.yaml up -d")
	for _, app := range []string{"blog", "docs", "talk"} {
		if i := host.indexOf("/srv/" + app + "/compose.yaml up -d"); i < infra {
			t.Errorf("%s was started before the infrastructure stack", app)
		}
	}
}

// An apply that stopped after writing resumes. The files already match the
// render, so without a record of what was owed the next apply would see
// nothing to do, and a stack held back by a failed gate would stay down.
func TestAStoppedApplyResumes(t *testing.T) {
	host := newHost()
	host.fail = "/srv/talk/compose.yaml up -d"
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err == nil {
		t.Fatal("the failing action did not stop the apply")
	}

	host.fail = ""
	host.commands = nil
	again, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Writes()) != 0 {
		t.Fatalf("the files were written the first time, yet %d would be written again", len(again.Writes()))
	}
	owed := map[string]bool{}
	for _, action := range again.Actions {
		owed[action.Stack] = action.Recreate
	}
	if recreate, ok := owed["talk"]; !ok || !recreate {
		t.Fatalf("the stopped stack is not owed a recreate: %v", again.Actions)
	}
	if err := apply.Execute(again, host); err != nil {
		t.Fatal(err)
	}
	if !host.ran("/srv/talk/compose.yaml up -d") {
		t.Error("the resumed apply did not start the stack the first one stopped at")
	}

	third, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Actions) != 0 {
		t.Errorf("a completed apply still owes %v", third.Actions)
	}
}

// fixture loads the render fixture's configuration and secrets, for tests
// that need what render does not carry, such as the database bootstrap.
func fixture(t *testing.T) (*config.Config, *config.Secrets) {
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

// bootstrapPlan is a home-a plan with its database work attached, exactly as
// cmd/paisans/main.go attaches it.
func bootstrapPlan(t *testing.T, host *fakeHost) *apply.Plan {
	t.Helper()
	cfg, secrets := fixture(t)
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	b, err := apply.Databases(cfg, secrets, "home-a")
	if err != nil {
		t.Fatal(err)
	}
	p.WithDatabases(b)
	return p
}

// clusteredPasswords are the fixture's clustered apps' database passwords,
// which are what the bootstrap must never put on a command line.
func clusteredPasswords(t *testing.T) map[string]string {
	t.Helper()
	cfg, secrets := fixture(t)
	out := map[string]string{}
	for _, name := range cfg.AppNames() {
		if cfg.Apps[name].Placement.Mode == config.PlacementCluster {
			out[name] = secrets.Apps[name]["database_password"].(string)
		}
	}
	return out
}

func noWait(t *testing.T) {
	t.Helper()
	t.Cleanup(apply.SetPrimaryWait(9*time.Second, 3*time.Second, func(time.Duration) {}))
}

// The fixture's clustered apps are talk, docs and auth; the pinned ones keep
// their own Postgres and are none of the cluster's business.
func TestOnlyClusteredPostgresAppsAreBootstrapped(t *testing.T) {
	cfg, secrets := fixture(t)
	b, err := apply.Databases(cfg, secrets, "home-a")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, db := range b.Databases {
		got = append(got, db.App+"="+db.Role+"/"+db.Name)
	}
	if want := "auth=auth/auth docs=docs/docs talk=talk/talk"; strings.Join(got, " ") != want {
		t.Errorf("bootstrapped %v, want %s", got, want)
	}
	if b.Patroni != "10.44.0.1:8008" {
		t.Errorf("Patroni is asked at %s, want the site's mesh address", b.Patroni)
	}

	none, err := apply.Databases(cfg, secrets, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if none != nil {
		t.Errorf("a site with no Patroni plans database work: %+v", none)
	}
}

// The order is the gate: infrastructure, then a primary, then roles and
// databases, and only then any app.
func TestDatabasesExistBeforeAnyAppStarts(t *testing.T) {
	noWait(t)
	host := newHost()
	p := bootstrapPlan(t, host)
	if p.Bootstrap == nil {
		t.Fatal("a first apply on a cluster site plans no database work")
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	infra := host.indexOf("/srv/infra/compose.yaml up -d")
	wait := host.indexOf(":8008/cluster")
	psql := host.indexOf("psql")
	if infra < 0 || wait < 0 || psql < 0 {
		t.Fatalf("missing a step: infra %d, wait %d, psql %d in %v", infra, wait, psql, host.commands)
	}
	if !(infra < wait && wait < psql) {
		t.Errorf("out of order: infra %d, wait %d, psql %d", infra, wait, psql)
	}
	for _, app := range []string{"auth", "blog", "docs", "talk"} {
		if i := host.indexOf("/srv/" + app + "/compose.yaml up -d"); i < psql {
			t.Errorf("%s was started before its database existed", app)
		}
	}
}

// Passwords travel on stdin. A command line is visible in `ps` to every user
// on the host, and an ssh error message quotes the command it ran.
func TestNoPasswordIsOnACommandLine(t *testing.T) {
	noWait(t)
	host := newHost()
	if err := apply.Execute(bootstrapPlan(t, host), host); err != nil {
		t.Fatal(err)
	}
	passwords := clusteredPasswords(t)
	for app, password := range passwords {
		for _, command := range host.commands {
			if strings.Contains(command, password) {
				t.Errorf("%s's database password is on a command line: %s", app, command)
			}
		}
		found := false
		for _, input := range host.inputs {
			if strings.Contains(input, password) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s's password never reached Postgres", app)
		}
	}
}

// The script converges on a re-run: create only what is missing, always set
// the password so a rotation is an apply, and never put CREATE DATABASE in a
// transaction, which Postgres refuses.
func TestTheBootstrapSQLIsIdempotent(t *testing.T) {
	cfg, secrets := fixture(t)
	secrets.Apps["talk"]["database_password"] = "it's"
	b, err := apply.Databases(cfg, secrets, "home-a")
	if err != nil {
		t.Fatal(err)
	}
	sql := apply.BootstrapSQL(b)
	for _, want := range []string{
		"SET log_statement = 'none';",
		`SELECT 'CREATE ROLE "talk" LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'talk')\gexec`,
		`ALTER ROLE "talk" WITH LOGIN PASSWORD 'it''s';`,
		`SELECT 'CREATE DATABASE "talk" OWNER "talk"' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'talk')\gexec`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("the script is missing\n  %s\nin\n%s", want, sql)
		}
	}
	for _, refused := range []string{"BEGIN", "DO $", "CREATE DATABASE \"talk\" OWNER \"talk\";"} {
		if strings.Contains(sql, refused) {
			t.Errorf("the script contains %q, which cannot hold CREATE DATABASE or is not conditional", refused)
		}
	}
	if strings.Index(sql, "SET log_statement") > strings.Index(sql, "PASSWORD") {
		t.Error("statement logging is switched off after a password was sent, so the server log has it")
	}
}

// No primary in time stops the apply before any app, and the next apply
// resumes from the same place.
func TestNoPrimaryStopsTheApply(t *testing.T) {
	noWait(t)
	host := newHost()
	host.leader = ""
	err := apply.Execute(bootstrapPlan(t, host), host)
	if err == nil {
		t.Fatal("apps were started with no Patroni primary")
	}
	if !strings.Contains(err.Error(), "no Patroni primary after 9s") {
		t.Errorf("the timeout does not say what happened:\n%v", err)
	}
	polls := 0
	for _, command := range host.commands {
		if strings.Contains(command, ":8008/cluster") {
			polls++
		}
	}
	if polls != 3 {
		t.Errorf("polled %d times, want 3 in 9s at 3s", polls)
	}
	if host.ran("psql") || host.ran("/srv/talk/compose.yaml up -d") {
		t.Errorf("work went on past the gate: %v", host.commands)
	}

	host.leader = "home-a"
	again := bootstrapPlan(t, host)
	if again.Bootstrap == nil {
		t.Fatal("the next apply does not resume the database work")
	}
	if err := apply.Execute(again, host); err != nil {
		t.Fatal(err)
	}
	if !host.ran("psql") || !host.ran("/srv/talk/compose.yaml up -d") {
		t.Error("the resumed apply did not finish")
	}
}

// A replica leaves the work to the leader's site and says so. Its apps still
// start: they reach the leader through HAProxy, and creating roles there from
// a replica is not something Postgres allows.
func TestAReplicaLeavesTheDatabasesToTheLeader(t *testing.T) {
	noWait(t)
	host := newHost()
	host.leader = "home-b"
	p := bootstrapPlan(t, host)
	var said strings.Builder
	p.Progress = &said
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if host.ran("psql") {
		t.Error("a replica tried to create roles")
	}
	if !strings.Contains(said.String(), "home-b") {
		t.Errorf("the skip does not name the leader's site:\n%s", said.String())
	}
	if !host.ran("/srv/talk/compose.yaml up -d") {
		t.Error("a replica's apps were not started")
	}
}

// A failed bootstrap is a gate, and its error does not echo a password even
// when psql quotes the line back.
func TestAFailedBootstrapStopsTheAppsAndHidesThePassword(t *testing.T) {
	noWait(t)
	host := newHost()
	host.fail = "psql"
	err := apply.Execute(bootstrapPlan(t, host), host)
	if err == nil {
		t.Fatal("apps were started after the bootstrap failed")
	}
	if host.ran("/srv/talk/compose.yaml up -d") {
		t.Error("an app was started after its database could not be created")
	}
	for app, password := range clusteredPasswords(t) {
		if strings.Contains(err.Error(), password) {
			t.Errorf("%s's password is in the error", app)
		}
	}
}

// A dry run asks nothing of Patroni or Postgres: the bootstrap is planned,
// not probed.
func TestPlanningTheBootstrapTouchesNoDatabase(t *testing.T) {
	host := newHost()
	bootstrapPlan(t, host)
	if host.ran(":8008") || host.ran("psql") {
		t.Errorf("building a plan reached the database: %v", host.commands)
	}
}

// An app whose role would be one the cluster uses itself is refused: the
// bootstrap would set that app's password on the cluster's admin role.
func TestAnAppCannotTakeAClusterRole(t *testing.T) {
	cfg, secrets := fixture(t)
	cfg.Apps["admin"] = cfg.Apps["talk"]
	secrets.Apps["admin"] = secrets.Apps["talk"]
	if _, err := apply.Databases(cfg, secrets, "home-a"); err == nil || !strings.Contains(err.Error(), "apps.admin") {
		t.Errorf("an app named admin was given the cluster's admin role: %v", err)
	}
}

// A second apply of the same thing runs nothing, database work included.
func TestANoopApplyBootstrapsNothing(t *testing.T) {
	noWait(t)
	host := newHost()
	if err := apply.Execute(bootstrapPlan(t, host), host); err != nil {
		t.Fatal(err)
	}
	if p := bootstrapPlan(t, host); p.Bootstrap != nil {
		t.Error("an apply with nothing to do still plans database work")
	}
}

// An env_file reaches its container when Compose creates it, so a changed
// patroni.env or caddy.env needs the container replaced. A restart kept the
// old values, and caddy.env, sitting beside the Caddyfile, was read as
// routing and answered with a reload that cannot see a rotated DNS token.
func TestAnEnvFileChangeRecreates(t *testing.T) {
	for _, tc := range []struct{ site, path string }{
		{"home-a", "/srv/infra/patroni.env"},
		{"vm", "/srv/infra/caddy/caddy.env"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			host := applied(t, tc.site)
			host.files[tc.path] = "DRIFTED=1\n"
			adopt(t, host, tc.path)

			p, err := apply.Build(tc.site, plan(t), acmeModule(t), host)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Actions) != 1 || p.Actions[0].Stack != "infra" || !p.Actions[0].Recreate {
				t.Fatalf("a changed %s plans %+v, want the infra stack recreated", tc.path, p.Actions)
			}
			if p.GatewayReload {
				t.Error("an environment change planned a reload, which rereads the Caddyfile and not the environment")
			}
			if tc.site == "vm" && !p.GatewayChanging {
				t.Error("the gateway is about to be replaced and its gates would not run")
			}
		})
	}
}

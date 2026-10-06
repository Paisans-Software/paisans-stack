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
	// ps is what `docker compose ps --format json` prints, by stack. A stack
	// not in it answers with one running container and no healthcheck.
	ps map[string]string
	// logs is what `docker compose logs` prints, by service.
	logs map[string]string
	// absent names images the host does not have. Every other image is
	// present, so a test that is not about pulls never meets the disk check.
	absent map[string]bool
	// free is what `df` reports available on Docker's data root, in bytes.
	free int64
	// reclaimable is what `docker system df` prints.
	reclaimable string
	// images is what `docker image ls --format json` prints.
	images string
	// containers is what the containers' images probe prints: names and IDs.
	containers string
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
	if strings.Contains(command, "docker image inspect") {
		return h.imageProbe(command), nil
	}
	if strings.Contains(command, "docker info --format") {
		return "/var/lib/docker\n", nil
	}
	if strings.HasPrefix(command, "df -B1 --output=avail ") {
		return fmt.Sprintf("       Avail\n%d\n", h.free), nil
	}
	if strings.HasPrefix(command, "docker image ls ") {
		return h.images, nil
	}
	if strings.HasPrefix(command, "docker ps -a ") {
		return h.containers, nil
	}
	if strings.HasPrefix(command, "docker system df") {
		return h.reclaimable, nil
	}
	if strings.Contains(command, ":8008/cluster") {
		if h.leader == "" {
			return `{"members":[{"name":"home-a","role":"replica","state":"starting"}]}`, nil
		}
		return fmt.Sprintf(`{"members":[{"name":%q,"role":"leader","state":"running"},{"name":"other","role":"replica","state":"streaming"}],"scope":"fixture"}`, h.leader), nil
	}
	if strings.Contains(command, " ps --all --format json") {
		stack := strings.TrimSuffix(strings.TrimPrefix(command, "docker compose -f /srv/"), "/compose.yaml ps --all --format json")
		if out, ok := h.ps[stack]; ok {
			return out, nil
		}
		return `{"Service":"app","Name":"` + stack + `-app-1","State":"running","Health":""}` + "\n", nil
	}
	if strings.Contains(command, " logs --no-color --tail 30 ") {
		for service, out := range h.logs {
			if strings.HasSuffix(command, "'"+service+"'") {
				return out, nil
			}
		}
		return "", nil
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

// imageProbe answers the loop apply sends to ask which images are present:
// each quoted reference between `for r in` and `; do`.
func (h *fakeHost) imageProbe(command string) string {
	list, _, _ := strings.Cut(strings.TrimPrefix(command, "for r in "), "; do")
	var b strings.Builder
	for _, quoted := range strings.Fields(list) {
		ref := strings.Trim(quoted, "'")
		if h.absent[ref] {
			fmt.Fprintf(&b, "absent %s\n", ref)
			continue
		}
		digest := sha256.Sum256([]byte(ref))
		fmt.Fprintf(&b, "present sha256:%s %s\n", hex.EncodeToString(digest[:]), ref)
	}
	return b.String()
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
	// The files are on the host and are this apply's, so they are recorded;
	// what it still owes is in the pending record, so the next apply resumes.
	if _, ok := host.files["/srv/.paisans-manifest.json"]; !ok {
		t.Error("the files a failed apply wrote were not recorded")
	}
	if _, ok := host.files["/srv/.paisans-pending.json"]; !ok {
		t.Error("a failed apply left no record of the actions it owes")
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
	owed := map[string]apply.Action{}
	for _, action := range again.Actions {
		owed[action.Stack] = action
	}
	if action, ok := owed["talk"]; !ok || !action.Recreate || !action.Force {
		t.Fatalf("the stopped stack is not owed a forced recreate: %+v", again.Actions)
	}
	// infra finished before talk failed, so it left the record. Owing it
	// again would force-recreate a stack that was fine.
	if _, ok := owed["infra"]; ok {
		t.Errorf("a stack that finished is still owed: %+v", again.Actions)
	}
	if err := apply.Execute(again, host); err != nil {
		t.Fatal(err)
	}
	if !host.ran("/srv/talk/compose.yaml up -d --force-recreate") {
		t.Error("the resumed apply did not force-recreate the stack the first one stopped at")
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

// A leader that is not a declared cluster site is not a replica: it means a
// member's name is not its site's name, and skipping as if it were left every
// app started with no database. It stops the apply before any app stack.
func TestAnUnknownLeaderStopsTheApply(t *testing.T) {
	noWait(t)
	host := newHost()
	host.leader = "some-machine"
	err := apply.Execute(bootstrapPlan(t, host), host)
	if err == nil {
		t.Fatal("an unknown leader was taken for a replica")
	}
	for _, want := range []string{`"some-machine"`, "home-a, home-b", "must equal its site's name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
	if host.ran("psql") {
		t.Error("roles were created under an unknown leader")
	}
	for _, command := range host.commands {
		if strings.Contains(command, "docker compose") && !strings.Contains(command, "/srv/infra/") {
			t.Errorf("an app stack was touched past the gate: %s", command)
		}
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

// A first apply that stops after writing its files must still record them.
// On the first real host the manifest was written only on success, so when the
// next apply carried a fix to one of those files, it found an unrecorded file
// that differed from the render and refused it as somebody's host edit.
func TestAFailedApplyStillRecordsItsFiles(t *testing.T) {
	host := newHost()
	host.fail = "/srv/infra/compose.yaml up -d"
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err == nil {
		t.Fatal("the failing action did not stop the apply")
	}

	fixed := plan(t)
	for i, file := range fixed.Files {
		if file.Path == "home-a/srv/infra/patroni.env" {
			fixed.Files[i].Content = file.Content + "# a fix carried by the next apply\n"
		}
	}
	host.fail = ""
	again, err := apply.Build("home-a", fixed, acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if conflicts := again.Conflicts(); len(conflicts) != 0 {
		t.Fatalf("files the failed apply wrote are treated as host edits: %v", conflicts)
	}
	for _, change := range again.Writes() {
		if change.Path == "/srv/infra/patroni.env" && change.Kind == apply.Update {
			return
		}
	}
	t.Fatalf("the fixed patroni.env is not planned as an update: %v", again.Writes())
}

// --overwrite turns exactly the named conflict into a write, and nothing else.
func TestOverwriteReplacesOnlyTheNamedConflict(t *testing.T) {
	host := newHost()
	host.files["/srv/infra/patroni.env"] = "edited on the host\n"
	host.files["/srv/infra/haproxy/haproxy.cfg"] = "also edited\n"

	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Overwrite("/srv/infra/patroni.env"))
	if err != nil {
		t.Fatal(err)
	}
	conflicts := p.Conflicts()
	if len(conflicts) != 1 || conflicts[0].Path != "/srv/infra/haproxy/haproxy.cfg" {
		t.Fatalf("only the unnamed file should still conflict: %v", conflicts)
	}
	var overwritten bool
	for _, change := range p.Writes() {
		if change.Path == "/srv/infra/patroni.env" {
			overwritten = change.Overwritten
		}
	}
	if !overwritten {
		t.Fatal("the named conflict is not marked as an overwrite")
	}
	if err := apply.Execute(p, host); err == nil {
		t.Fatal("a remaining conflict did not stop the apply")
	}
	if host.files["/srv/infra/patroni.env"] != "edited on the host\n" {
		t.Error("a refused apply still wrote the overwritten file")
	}

	p, err = apply.Build("home-a", plan(t), acmeModule(t), host, apply.Overwrite("/srv/infra/patroni.env", "/srv/infra/haproxy/haproxy.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if host.files["/srv/infra/patroni.env"] == "edited on the host\n" {
		t.Error("the named file was not replaced")
	}
	next, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Conflicts()) != 0 {
		t.Errorf("an overwritten file was not recorded: %v", next.Conflicts())
	}
}

// A path that is not a conflict is refused, so a typo cannot pass for consent.
func TestOverwriteRefusesAPathThatIsNoConflict(t *testing.T) {
	host := newHost()
	_, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Overwrite("/srv/infra/patroni.evn"))
	if err == nil || !strings.Contains(err.Error(), "names no conflicting file") {
		t.Fatalf("a mistyped --overwrite was accepted: %v", err)
	}
}

// A resumed stack is force-recreated even when its files did not change. On a
// real host an `up -d` failed part way and left Mbin's app container created
// with no network; the resumed plain `up -d` saw an unchanged configuration
// and started that container as it was.
func TestAResumedStackIsForceRecreated(t *testing.T) {
	host := applied(t, "home-a")
	host.files["/srv/.paisans-pending.json"] = `{"version":1,"actions":[{"stack":"talk","recreate":true}]}`
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Actions) != 1 || p.Actions[0].Stack != "talk" || !p.Actions[0].Force {
		t.Fatalf("an owed stack plans %+v, want talk force-recreated", p.Actions)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if !host.ran("docker compose -f /srv/talk/compose.yaml up -d --force-recreate") {
		t.Errorf("the owed stack was not force-recreated: %v", host.commands)
	}
}

// An ordinary change is never forced: that would replace every container of
// a stack on every apply.
func TestAnOrdinaryChangeIsNotForced(t *testing.T) {
	host := newHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range p.Actions {
		if action.Force {
			t.Errorf("a first apply forces %s", action.Stack)
		}
	}
}

// --recreate plans a named stack although nothing changed, keeps the
// infrastructure first, and still creates databases before an app starts.
func TestRecreateForcesANamedStack(t *testing.T) {
	host := applied(t, "home-a")
	cfg, secrets := fixture(t)
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Recreate("talk", "infra"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Actions) != 2 || p.Actions[0].Stack != "infra" || p.Actions[1].Stack != "talk" {
		t.Fatalf("--recreate talk infra plans %+v, want infra then talk", p.Actions)
	}
	for _, action := range p.Actions {
		if !action.Force {
			t.Errorf("%s was named and is not forced", action.Stack)
		}
	}
	b, err := apply.Databases(cfg, secrets, "home-a")
	if err != nil {
		t.Fatal(err)
	}
	p.WithDatabases(b)
	noWait(t)
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	infra := host.indexOf("/srv/infra/compose.yaml up -d --force-recreate")
	psql := host.indexOf("psql")
	talk := host.indexOf("/srv/talk/compose.yaml up -d --force-recreate")
	if infra < 0 || psql < 0 || talk < 0 || !(infra < psql && psql < talk) {
		t.Errorf("want infra, then the databases, then talk: %v", host.commands)
	}
	if host.ran("/srv/docs/compose.yaml") {
		t.Error("a stack nobody named was acted on")
	}
}

// A stack this site does not render is refused, so a typo is not a no-op.
func TestRecreateRefusesAnUnknownStack(t *testing.T) {
	host := newHost()
	_, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Recreate("tlak"))
	if err == nil || !strings.Contains(err.Error(), "names no stack this site renders") {
		t.Fatalf("an unknown --recreate was accepted: %v", err)
	}
}

// fastHealth makes the health gate give up after three polls without sleeping,
// and counts the polls it slept between.
func fastHealth(t *testing.T) *int {
	t.Helper()
	slept := 0
	t.Cleanup(apply.SetHealthWait(15*time.Second, 5*time.Second, func(time.Duration) { slept++ }))
	return &slept
}

// A stack whose healthchecks pass lets the apply carry on, in either output
// shape Compose has printed.
func TestAHealthyStackPasses(t *testing.T) {
	fastHealth(t)
	host := newHost()
	host.ps = map[string]string{
		"infra": `[{"Service":"patroni","State":"running","Health":"healthy"},{"Service":"caddy","State":"running","Health":""}]`,
		"talk":  "{\"Service\":\"app\",\"State\":\"running\",\"Health\":\"healthy\"}\n{\"Service\":\"db\",\"State\":\"running\",\"Health\":\"healthy\"}\n",
	}
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	for _, stack := range []string{"infra", "talk", "docs"} {
		if !host.ran("/srv/" + stack + "/compose.yaml ps --all --format json") {
			t.Errorf("%s was never checked", stack)
		}
	}
	if _, ok := host.files["/srv/.paisans-pending.json"]; ok {
		t.Error("a healthy apply left owed actions behind")
	}
}

// A container without a healthcheck counts once it runs; one still starting
// is waited for, not failed.
func TestARunningContainerWithoutAHealthcheckPasses(t *testing.T) {
	slept := fastHealth(t)
	host := applied(t, "home-a")
	polls := 0
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Recreate("talk"))
	if err != nil {
		t.Fatal(err)
	}
	host.ps = map[string]string{"talk": `{"Service":"app","State":"created","Health":""}`}
	wrapped := &pollingHost{fakeHost: host, onPoll: func() {
		polls++
		if polls == 1 {
			host.ps["talk"] = `{"Service":"app","State":"running","Health":""}`
		}
	}}
	if err := apply.Execute(p, wrapped); err != nil {
		t.Fatal(err)
	}
	if *slept != 1 {
		t.Errorf("slept %d times, want one wait between a created and a running container", *slept)
	}
}

// pollingHost calls onPoll on every health poll, so a test can change what
// the next one sees.
type pollingHost struct {
	*fakeHost
	onPoll func()
}

func (h *pollingHost) Run(command string) (string, error) {
	out, err := h.fakeHost.Run(command)
	if strings.Contains(command, " ps --all --format json") {
		h.onPoll()
	}
	return out, err
}

// An unhealthy stack stops the apply with its services and their logs, no
// later stack starts, and the record keeps the stack owed so the next apply
// resumes and force-recreates it. On a real host an apply reported success
// while Mbin could not reach its database.
func TestAnUnhealthyStackStopsTheApply(t *testing.T) {
	for _, tc := range []struct{ name, ps string }{
		{"unhealthy", `{"Service":"app","State":"running","Health":"unhealthy"}`},
		{"restarting", `{"Service":"app","State":"restarting","Health":"","ExitCode":1}`},
		{"timeout", `{"Service":"app","State":"running","Health":"starting"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// noWait first: the two waits share one sleep, and the counting
			// sleeper must be the one left in place.
			noWait(t)
			slept := fastHealth(t)
			host := newHost()
			host.ps = map[string]string{"infra": tc.ps}
			host.logs = map[string]string{"app": "SQLSTATE[08006] could not connect to server\n"}
			p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
			if err != nil {
				t.Fatal(err)
			}
			err = apply.Execute(p, host)
			if err == nil {
				t.Fatal("an unhealthy stack let the apply succeed")
			}
			for _, want := range []string{"infra", "app", "could not connect to server"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not carry %q:\n%v", want, err)
				}
			}
			if tc.name == "timeout" && *slept != 2 {
				t.Errorf("a starting container was polled with %d sleeps, want the whole wait", *slept)
			}
			if tc.name != "timeout" && *slept != 0 {
				t.Errorf("a failed container was waited on (%d sleeps)", *slept)
			}
			for _, stack := range []string{"blog", "docs", "talk"} {
				if host.ran("/srv/" + stack + "/compose.yaml up -d") {
					t.Errorf("%s was started after the infrastructure stack failed its check", stack)
				}
			}
			if host.ran("psql") {
				t.Error("databases were created behind an unhealthy infrastructure stack")
			}

			host.ps = nil
			again, err := apply.Build("home-a", plan(t), acmeModule(t), host)
			if err != nil {
				t.Fatal(err)
			}
			if len(again.Actions) == 0 || again.Actions[0].Stack != "infra" || !again.Actions[0].Force {
				t.Fatalf("the failed stack is not owed a forced recreate: %+v", again.Actions)
			}
		})
	}
}

// Both shapes Compose prints for `ps --format json` are read: one array, and
// one object per line.
func TestComposePsOutputIsReadInBothShapes(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      int
	}{
		{"array", `[{"Service":"a","State":"running"},{"Service":"b","State":"running"}]`, 2},
		{"lines", "{\"Service\":\"a\",\"State\":\"running\"}\n{\"Service\":\"b\",\"State\":\"running\"}\n", 2},
		{"empty", "\n", 0},
		{"empty array", "[]", 0},
	} {
		n, err := apply.ParseContainers(tc.out)
		if err != nil || n != tc.want {
			t.Errorf("%s: read %d containers, %v; want %d", tc.name, n, err, tc.want)
		}
	}
	if _, err := apply.ParseContainers("not json"); err == nil {
		t.Error("unreadable output was accepted")
	}
}

// talkImage is the image the fixture pins for the Mbin app.
const talkImage = "ghcr.io/example-org/mbin:v1.10.1-fork"

// A stack about to pull an image onto a host short of space is refused before
// anything is written, and the refusal carries the numbers and what could be
// reclaimed. A pull that fills the disk fails part way, with the old
// containers already stopped.
func TestAPullOntoAFullDiskIsRefused(t *testing.T) {
	host := newHost()
	host.absent = map[string]bool{talkImage: true}
	host.free = 1932735283 // 1.8 GiB, what the first real host had left
	host.reclaimable = "Images: 1.4GB (31%) reclaimable of 4.5GB\nContainers: 0B (0%) reclaimable of 12MB\n"

	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if p.Disk == nil || !p.Disk.Short() {
		t.Fatalf("a pull onto 1.8 GiB free is not flagged: %+v", p.Disk)
	}
	if len(p.Disk.Pulls) != 1 || p.Disk.Pulls[0] != talkImage {
		t.Errorf("pulls are %v, want only %s", p.Disk.Pulls, talkImage)
	}
	line := p.Disk.Describe()
	for _, want := range []string{"1.8 GiB free on /var/lib/docker", "3.0 GiB required", talkImage, "refuses"} {
		if !strings.Contains(line, want) {
			t.Errorf("the plan line does not say %q: %s", want, line)
		}
	}

	host.commands = nil
	err = apply.Execute(p, host)
	if err == nil {
		t.Fatal("an apply pulled onto a disk without room for it")
	}
	for _, want := range []string{"1.8 GiB free", "at least 3.0 GiB", "Images: 1.4GB (31%) reclaimable of 4.5GB", "--min-free"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if len(host.commands) != 0 || len(host.files) != 0 {
		t.Errorf("a refused apply still ran %v or wrote %d file(s)", host.commands, len(host.files))
	}
}

// --min-free lowers the threshold for one run, for an operator who knows the
// pull fits.
func TestMinFreeLowersTheThreshold(t *testing.T) {
	host := newHost()
	host.absent = map[string]bool{talkImage: true}
	host.free = 1932735283
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.MinFree(1<<30))
	if err != nil {
		t.Fatal(err)
	}
	if p.Disk == nil || p.Disk.Short() {
		t.Fatalf("1.8 GiB free against --min-free 1G is flagged: %+v", p.Disk)
	}
	if host.ran("docker system df") {
		t.Error("what is reclaimable was read although nothing is short")
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
}

// Free space is only asked about when something will be pulled: an image
// the host already has costs nothing, and a restart pulls nothing.
func TestNoPullAsksNothingAboutDisk(t *testing.T) {
	host := newHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if p.Disk != nil || host.ran("docker info") || host.ran("df -B1") {
		t.Errorf("a plan pulling nothing checked the disk: %+v", p.Disk)
	}
}

// An image probe that does not answer for every image is an error, not a
// reason to assume the image is there and skip the check.
func TestAnUnreadableImageProbeIsAnError(t *testing.T) {
	host := newHost()
	host.fail = "docker image inspect"
	if _, err := apply.Build("home-a", plan(t), acmeModule(t), host); err == nil {
		t.Error("a failed image probe was read as every image present")
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"2G": 2 << 30, "2GiB": 2 << 30, "2GB": 2 << 30, "1.5G": 3 << 29, "500M": 500 << 20, "1024": 1024, "1T": 1 << 40,
	} {
		got, err := apply.ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "two", "-1G", "G"} {
		if _, err := apply.ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) was accepted", in)
		}
	}
}

// fakeID is the ID the fake host gives an image, the same one its probe
// reports for a present reference.
func fakeID(ref string) string {
	digest := sha256.Sum256([]byte(ref))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// imageLine is one `docker image ls --format json` line.
func imageLine(repo, tag, id string) string {
	return fmt.Sprintf(`{"Containers":"N/A","Digest":"<none>","ID":%q,"Repository":%q,"Tag":%q,"Size":"1.4GB"}`, id, repo, tag) + "\n"
}

// supersededHost has the rendered Mbin image, an older one nothing uses, two
// older ones a container still uses (one by name, one by ID only, as a
// container whose tag moved on shows), and an image of another repository.
func supersededHost() *fakeHost {
	host := newHost()
	const repo = "ghcr.io/example-org/mbin"
	host.images = imageLine(repo, "v1.10.1-fork", fakeID(talkImage)) +
		imageLine(repo, "v1.9.0", fakeID("old")) +
		imageLine(repo, "v1.8.0", fakeID("named")) +
		imageLine(repo, "<none>", fakeID("byid")) +
		imageLine("example/unrelated", "1", fakeID("unrelated"))
	host.containers = repo + ":v1.8.0\n" + talkImage + "\n" + fakeID("byid") + "\n"
	return host
}

// Superseded images of a stack's own repositories are removed once that
// stack is healthy, and only those no container uses. Another repository's
// images are never touched.
func TestSupersededImagesArePrunedAfterHealth(t *testing.T) {
	host := supersededHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	var planned []string
	for _, prune := range p.Prunes {
		if prune.Stack != "talk" {
			t.Errorf("%s is planned under %s, want talk", prune.Ref, prune.Stack)
		}
		planned = append(planned, prune.Ref)
	}
	want := "ghcr.io/example-org/mbin:v1.9.0 ghcr.io/example-org/mbin:v1.8.0 ghcr.io/example-org/mbin@" + strings.TrimPrefix(fakeID("byid"), "sha256:")[:12]
	if strings.Join(planned, " ") != want {
		t.Errorf("the plan prunes %v, want %s", planned, want)
	}

	var progress strings.Builder
	p.Progress = &progress
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, command := range host.commands {
		if strings.HasPrefix(command, "docker image rm ") {
			removed = append(removed, command)
		}
	}
	if len(removed) != 1 || !strings.Contains(removed[0], fakeID("old")) {
		t.Fatalf("removed %v, want only the unused v1.9.0", removed)
	}
	if rm, gate := host.indexOf("docker image rm "), host.indexOf("/srv/talk/compose.yaml ps --all"); gate < 0 || rm < gate {
		t.Error("an image was removed before its stack passed the health gate")
	}
	if !strings.Contains(progress.String(), "pruned    ghcr.io/example-org/mbin:v1.9.0") {
		t.Errorf("the prune was not reported:\n%s", progress.String())
	}
}

// --keep-images leaves every image in place and plans no prune.
func TestKeepImagesPrunesNothing(t *testing.T) {
	host := supersededHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.KeepImages())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Prunes) != 0 {
		t.Errorf("--keep-images still plans %v", p.Prunes)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if host.ran("docker image rm") || host.ran("docker image ls") {
		t.Error("--keep-images still listed or removed images")
	}
}

// A removal that fails is a warning: the stack is already healthy, and an
// image left behind costs disk, not service.
func TestAFailedPruneIsAWarning(t *testing.T) {
	host := supersededHost()
	host.fail = "docker image rm"
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	var progress strings.Builder
	p.Progress = &progress
	if err := apply.Execute(p, host); err != nil {
		t.Fatalf("a failed prune failed the apply: %v", err)
	}
	if !strings.Contains(progress.String(), "warning   talk: could not remove ghcr.io/example-org/mbin:v1.9.0") {
		t.Errorf("the failed prune was not reported:\n%s", progress.String())
	}
	if _, owed := host.files["/srv/.paisans-pending.json"]; owed {
		t.Error("a failed prune left the apply owing work")
	}
}

// A gateway image that cannot be pulled is reported as a pull failure, not as
// a Caddy without its DNS module. The first real gateway's image was private,
// and the old message sent the operator looking for the wrong problem.
func TestAnUnpullableGatewayImageSaysSo(t *testing.T) {
	host := newHost()
	host.fail = "compose.yaml pull caddy"
	p, err := apply.Build("vm", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	err = apply.Execute(p, host)
	if err == nil || !strings.Contains(err.Error(), "could not be pulled") {
		t.Fatalf("a failed pull was not reported as one: %v", err)
	}
	if host.ran("list-modules") {
		t.Error("the module check ran after the pull failed")
	}
}

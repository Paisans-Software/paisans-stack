package hostcaddy_test

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/hostcaddy"
	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// fixture is the render fixture with its monitor, watch, in ingress mode
// external: status (uptime) published on listen for the host's web server.
func fixture(t *testing.T, listen string) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "render", "testdata", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	watch := cfg.Sites["watch"]
	watch.Ingress = &config.Ingress{Mode: config.IngressExternal, Listen: listen}
	cfg.Sites["watch"] = watch
	return cfg
}

var caddyID = strings.Repeat("c", 64)

// host is a fake host: commands answered by the first key they contain,
// files in a map, and `cat > path` (stdin), `cp -p a b`, `cat a > b` and
// `rm -f path` carried out on that map, so a test reads what was written.
type host struct {
	answers map[string]string
	fail    map[string]string
	files   map[string]string
	ran     []string
}

func (h *host) Describe() string { return "ubuntu@watch.example.org" }

func (h *host) Run(command string) (string, error) {
	h.ran = append(h.ran, command)
	for key, out := range h.fail {
		if strings.Contains(command, key) {
			return out, errors.New("exit status 1")
		}
	}
	if args, ok := strings.CutPrefix(command, "cp -p "); ok {
		from, to := unquote2(args)
		h.files[to] = h.files[from]
		return "", nil
	}
	if args, ok := strings.CutPrefix(command, "rm -f "); ok {
		delete(h.files, strings.Trim(args, "'"))
		return "", nil
	}
	if args, ok := strings.CutPrefix(command, "cat "); ok {
		from, to, _ := strings.Cut(args, " > ")
		h.files[strings.Trim(to, "'")] = h.files[strings.Trim(from, "'")]
		return "", nil
	}
	for key, out := range h.answers {
		if strings.Contains(command, key) {
			return out, nil
		}
	}
	return "fake: no answer", fmt.Errorf("fake: no answer for %q", command)
}

func (h *host) RunInput(command, stdin string) (string, error) {
	h.ran = append(h.ran, command)
	_, path, ok := strings.Cut(command, "cat > ")
	if !ok {
		return "", fmt.Errorf("fake: unexpected input command %q", command)
	}
	h.files[strings.Trim(path, "'")] = stdin
	return "", nil
}

func (h *host) ReadFile(path string) (string, bool, error) {
	content, ok := h.files[path]
	return content, ok, nil
}

func unquote2(args string) (string, string) {
	parts := strings.SplitN(args, "' '", 2)
	return strings.Trim(parts[0], "'"), strings.Trim(parts[1], "'")
}

// report is the host check's finding: a Caddy container holding 443/tcp,
// on the host's network or publishing the port from a network of its own.
func report(t *testing.T, cfg *config.Config, hostNetwork bool) *hostcheck.Report {
	t.Helper()
	_, mesh, _ := net.ParseCIDR(cfg.Mesh.Subnet)
	c := hostcheck.Container{ID: caddyID, Name: "caddy", Project: "web", PID: 900}
	inv := &hostcheck.Inventory{Host: "ubuntu@watch.example.org", Cgroups: map[int]string{}}
	if hostNetwork {
		c.Networks = []string{"host"}
		inv.Sockets = []hostcheck.Socket{{Proto: "tcp", Port: 443, Process: "caddy", PID: 900}, {Proto: "tcp", Port: 80, Process: "caddy", PID: 900}}
		inv.Cgroups[900] = caddyID
	} else {
		c.Networks = []string{"web_default"}
		c.Bindings = []render.Listener{{Proto: "tcp", Port: 443}, {Proto: "tcp", Port: 80}}
		inv.Networks = []hostcheck.Network{{Name: "web_default", Project: "web", Subnets: []string{"172.20.0.0/16"}}, {Name: "other", Subnets: []string{"172.21.0.0/16"}}}
	}
	inv.Containers = []hostcheck.Container{c}
	return hostcheck.Classify(hostcheck.Claims{Site: "watch", Deployment: cfg.Deployment(), Mesh: mesh, Interface: cfg.Deployment().Interface()}, inv)
}

// inspect is `docker inspect` of the Caddy container.
func inspect(network string, mounts string) string {
	networks := `{}`
	switch network {
	case "host":
		networks = `{"host":{"IPAddress":""}}`
	case "bridge":
		networks = `{"bridge":{"IPAddress":"172.17.0.2"}}`
	default:
		networks = `{"other":{"IPAddress":"172.21.0.9"},"web_default":{"IPAddress":"172.20.0.5"}}`
		network = "web_default"
	}
	return fmt.Sprintf(`{"id":%q,"image":"caddy:2.11","path":"caddy","args":["run","--config","/etc/caddy/Caddyfile","--adapter","caddyfile"],"workdir":"/srv","network_mode":%q,"running":true,"networks":%s,"mounts":%s}`,
		caddyID, network, networks, mounts)
}

const (
	fileMount = `[{"Type":"bind","Source":"/opt/web/Caddyfile","Destination":"/etc/caddy/Caddyfile"},{"Type":"volume","Source":"/var/lib/docker/volumes/data/_data","Destination":"/data"}]`
	dirMount  = `[{"Type":"bind","Source":"/opt/web/caddy","Destination":"/etc/caddy"},{"Type":"bind","Source":"/opt/web/sites","Destination":"/etc/caddy/sites"}]`
)

const ownersCaddyfile = `{
	email ops@example.org
	# admin is left at its default
}

blog.example.org {
	reverse_proxy 127.0.0.1:2368
}
`

func caddyHost(network, mounts string, files map[string]string) *host {
	return &host{
		answers: map[string]string{
			"docker inspect": inspect(network, mounts),
			"caddy version":  "v2.11.4 h1:abc=\n",
			"caddy validate": "Valid configuration\n",
			"caddy reload":   "",
			"curl ":          "200",
		},
		fail:  map[string]string{},
		files: files,
	}
}

func plan(t *testing.T, cfg *config.Config, r *hostcheck.Report, h *host) *hostcaddy.Plan {
	t.Helper()
	f, err := hostcaddy.Detect(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if f == nil {
		t.Fatal("no Caddy found")
	}
	sites, err := hostcaddy.Wanted(cfg, "watch", f)
	if err != nil {
		t.Fatal(err)
	}
	p, err := hostcaddy.PlanChange(f, cfg.Deployment(), sites, h)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func printed(p *hostcaddy.Plan) string {
	var b bytes.Buffer
	p.Print(&b)
	return b.String()
}

var at = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// A host network Caddy whose Caddyfile imports a directory bind mounted
// from the host gets a file of the deployment's own there, proxying to the
// declared listen, and the Caddyfile is not touched. Applied, a second plan
// finds nothing to do.
func TestHostNetworkCaddyWithAnImportDirectoryGetsAFileOfItsOwn(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	main := "{\n\temail ops@example.org\n}\n\nimport sites/*.caddy\n\nblog.example.org {\n\treverse_proxy 127.0.0.1:2368\n}\n"
	h := caddyHost("host", dirMount, map[string]string{"/opt/web/caddy/Caddyfile": main})
	p := plan(t, cfg, report(t, cfg, true), h)
	if p.Kind != hostcaddy.CreateFile || p.Path != "/opt/web/sites/paisans-f2a9.caddy" || p.Foreign {
		t.Fatalf("kind %s path %s foreign %v:\n%s", p.Kind, p.Path, p.Foreign, printed(p))
	}
	for _, want := range []string{"status.example.org {", "reverse_proxy 127.0.0.1:8480", "respond @refused 404", "deployment f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01"} {
		if !strings.Contains(p.After, want) {
			t.Errorf("the file has no %q:\n%s", want, p.After)
		}
	}
	if strings.Contains(p.After, "tls ") {
		t.Errorf("the block names certificate files, which this Caddy obtains itself:\n%s", p.After)
	}
	if !strings.Contains(printed(p), "+status.example.org {") {
		t.Errorf("the plan does not show the block:\n%s", printed(p))
	}

	var out bytes.Buffer
	if err := hostcaddy.Execute(p, h, at, &out); err != nil {
		t.Fatal(err)
	}
	if h.files["/opt/web/caddy/Caddyfile"] != main {
		t.Error("the owner's Caddyfile changed")
	}
	if h.files[p.Path] != p.After {
		t.Errorf("wrote %q", h.files[p.Path])
	}
	for path := range h.files {
		if strings.Contains(path, "paisans-backup") {
			t.Errorf("a backup %s was made of a file that is the deployment's own", path)
		}
	}
	wantRan(t, h, "docker exec '"+caddyID+"' caddy validate --config '/etc/caddy/Caddyfile' --adapter caddyfile")
	wantRan(t, h, "docker exec '"+caddyID+"' caddy reload --config '/etc/caddy/Caddyfile' --adapter caddyfile")
	for _, c := range h.ran {
		if strings.Contains(c, "restart") || strings.Contains(c, "compose") || strings.Contains(c, "docker rm") || strings.Contains(c, "docker stop") {
			t.Errorf("ran %q on a container the toolkit does not own", c)
		}
	}

	again := plan(t, cfg, report(t, cfg, true), h)
	if again.Kind != hostcaddy.Nothing || again.Pending() {
		t.Fatalf("a second plan is %s:\n%s", again.Kind, printed(again))
	}
}

// A host network Caddy whose Caddyfile imports nothing gets a marked block
// appended to the Caddyfile, after a timestamped backup beside it, and
// everything before the block stays byte for byte.
func TestHostNetworkCaddyWithoutAnImportGetsAMarkedBlockAppended(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	h := caddyHost("host", fileMount, map[string]string{"/opt/web/Caddyfile": ownersCaddyfile})
	p := plan(t, cfg, report(t, cfg, true), h)
	if p.Kind != hostcaddy.AppendBlock || p.Path != "/opt/web/Caddyfile" || !p.Foreign {
		t.Fatalf("kind %s path %s:\n%s", p.Kind, p.Path, printed(p))
	}
	if !strings.HasPrefix(p.After, ownersCaddyfile) {
		t.Fatalf("the owner's lines changed:\n%s", p.After)
	}
	added := strings.TrimPrefix(p.After, ownersCaddyfile)
	if !strings.HasPrefix(added, "\n# BEGIN paisans-f2a9 deployment f2a9c4e1-0b7d-4c3a-9e2f-5a6b7c8d9e01") || !strings.HasSuffix(added, "# END paisans-f2a9\n") {
		t.Fatalf("appended:\n%s", added)
	}

	var out bytes.Buffer
	if err := hostcaddy.Execute(p, h, at, &out); err != nil {
		t.Fatal(err)
	}
	backup := "/opt/web/Caddyfile.paisans-backup-20261009T120000Z"
	if h.files[backup] != ownersCaddyfile {
		t.Errorf("backup %q", h.files[backup])
	}
	if h.files["/opt/web/Caddyfile"] != p.After {
		t.Errorf("wrote %q", h.files["/opt/web/Caddyfile"])
	}
	// Written in place, never moved over: a Caddyfile bind mounted on its
	// own is mounted by inode.
	wantRan(t, h, "umask 022; cat > '/opt/web/Caddyfile'")

	again := plan(t, cfg, report(t, cfg, true), h)
	if again.Pending() {
		t.Fatalf("a second plan is %s:\n%s", again.Kind, printed(again))
	}
	if !strings.Contains(printed(again), "unchanged") {
		t.Errorf("a second plan does not say it is unchanged:\n%s", printed(again))
	}
}

// A changed listen replaces the deployment's own block and nothing outside
// its markers, wherever the owner has added lines since.
func TestAChangedListenReplacesOnlyTheMarkedBlock(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	h := caddyHost("host", fileMount, map[string]string{"/opt/web/Caddyfile": ownersCaddyfile})
	first := plan(t, cfg, report(t, cfg, true), h)
	if err := hostcaddy.Execute(first, h, at, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	later := "shop.example.org {\n\treverse_proxy 127.0.0.1:9000\n}\n"
	h.files["/opt/web/Caddyfile"] += later

	cfg = fixture(t, "127.0.0.1:8490")
	p := plan(t, cfg, report(t, cfg, true), h)
	if p.Kind != hostcaddy.ReplaceBlock {
		t.Fatalf("kind %s:\n%s", p.Kind, printed(p))
	}
	if !strings.Contains(p.Old, "127.0.0.1:8480") || !strings.Contains(p.New, "127.0.0.1:8490") {
		t.Fatalf("old %q new %q", p.Old, p.New)
	}
	if !strings.HasPrefix(p.After, ownersCaddyfile) || !strings.HasSuffix(p.After, later) || strings.Contains(p.After, "8480") {
		t.Fatalf("after:\n%s", p.After)
	}
	diff := p.Diff()
	if !strings.Contains(diff, "-\treverse_proxy 127.0.0.1:8480") || !strings.Contains(diff, "+\treverse_proxy 127.0.0.1:8490") || strings.Contains(diff, "blog.example.org") {
		t.Errorf("diff:\n%s", diff)
	}
}

// A Caddy on a network of its own cannot reach the host's loopback. The
// app joins that network, the block proxies to the app's alias and port
// there, and the app trusts the Caddy's address on it.
func TestABridgedCaddyReachesTheAppOnItsOwnNetwork(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	h := caddyHost("bridged", fileMount, map[string]string{"/opt/web/Caddyfile": ownersCaddyfile})
	r := report(t, cfg, false)
	f, err := hostcaddy.Detect(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if f.HostNetwork || f.Network != "web_default" || f.Address != "172.20.0.5" || f.Unsupported != "" {
		t.Fatalf("%+v", f)
	}
	proxy, ok := hostcaddy.Proxy(f)
	if !ok || proxy != (render.HostProxy{Network: "web_default", Address: "172.20.0.5"}) {
		t.Fatalf("proxy %+v %v", proxy, ok)
	}
	p := plan(t, cfg, r, h)
	if !strings.Contains(p.New, "reverse_proxy paisans-f2a9-status:3001") {
		t.Fatalf("block:\n%s", p.New)
	}

	secrets, err := config.LoadSecrets(filepath.Join("..", "render", "testdata", "secrets.fixture.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := render.Build(cfg, secrets, render.WithHostProxy("status", proxy))
	if err != nil {
		t.Fatal(err)
	}
	compose, env := file(t, rendered, "watch/srv/paisans/f2a9/status/compose.yaml"), file(t, rendered, "watch/srv/paisans/f2a9/status/.env")
	for _, want := range []string{"      proxy:\n        aliases:\n          - paisans-f2a9-status", "  proxy:\n    name: web_default\n    external: true", "subnet: 10.255.255.0/29", "127.0.0.1:8480:3001"} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose has no %q:\n%s", want, compose)
		}
	}
	if !strings.Contains(env, "TRUST_PROXY=10.255.255.1/32,172.20.0.5/32\n") {
		t.Errorf("env:\n%s", env)
	}
}

// A Caddy on Docker's default bridge alone has no network the app can join
// by name, and cannot reach the host's loopback: nothing is planned, and
// the block is printed for the owner.
func TestACaddyOnTheDefaultBridgeIsHeld(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	h := caddyHost("bridge", fileMount, map[string]string{"/opt/web/Caddyfile": ownersCaddyfile})
	p := plan(t, cfg, report(t, cfg, false), h)
	if p.Pending() || !strings.Contains(p.Held, "default bridge") {
		t.Fatalf("%s %q", p.Kind, p.Held)
	}
	if !strings.Contains(printed(p), "reverse_proxy 127.0.0.1:8480") {
		t.Errorf("the held plan does not print the block:\n%s", printed(p))
	}
}

// A container on 443 that is not Caddy is not edited. Nothing is planned,
// and the block is printed for its owner to translate.
func TestAServerThatIsNotCaddyIsHeld(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	h := caddyHost("host", fileMount, map[string]string{"/opt/web/Caddyfile": ownersCaddyfile})
	delete(h.answers, "caddy version")
	h.fail["caddy version"] = `OCI runtime exec failed: exec failed: unable to start container process: exec: "caddy": executable file not found in $PATH`
	p := plan(t, cfg, report(t, cfg, true), h)
	if p.Pending() || !strings.Contains(p.Held, "is not Caddy") {
		t.Fatalf("%s %q", p.Kind, p.Held)
	}
	if err := hostcaddy.Execute(p, h, at, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if h.files["/opt/web/Caddyfile"] != ownersCaddyfile || len(h.files) != 1 {
		t.Errorf("files %v", h.files)
	}
}

// A docker exec that fails for any other reason is a failure to look, not
// a finding that the server is not Caddy.
func TestAFailedProbeIsNotAFinding(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	h := caddyHost("host", fileMount, map[string]string{"/opt/web/Caddyfile": ownersCaddyfile})
	delete(h.answers, "caddy version")
	h.fail["caddy version"] = "Cannot connect to the Docker daemon"
	if _, err := hostcaddy.Detect(report(t, cfg, true), h); err == nil {
		t.Fatal("a failed probe was read as a finding")
	}
}

// Configuration the toolkit cannot add a block to is held, each with its
// reason: JSON, a Caddyfile inside the container, and an admin endpoint
// turned off.
func TestConfigurationTheToolkitCannotEditIsHeld(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	for _, tc := range []struct {
		name, inspect, caddyfile, want string
	}{
		{"json", strings.Replace(inspect("host", fileMount), `"caddyfile"`, `"json"`, 1), ownersCaddyfile, "json adapter"},
		{"in the image", inspect("host", `[]`), ownersCaddyfile, "not from a file bind mounted"},
		{"admin off", inspect("host", fileMount), "{\n\tadmin off\n}\nblog.example.org {\n}\n", "admin off"},
		{"no config", strings.Replace(inspect("host", fileMount), `"--config","/etc/caddy/Caddyfile",`, ``, 1), ownersCaddyfile, "without --config"},
	} {
		h := caddyHost("host", fileMount, map[string]string{"/opt/web/Caddyfile": tc.caddyfile})
		h.answers["docker inspect"] = tc.inspect
		p := plan(t, cfg, report(t, cfg, true), h)
		if p.Pending() || !strings.Contains(p.Held, tc.want) {
			t.Errorf("%s: %s %q", tc.name, p.Kind, p.Held)
		}
	}
}

// A site block for the hostname that the toolkit did not write is the
// owner's: a second would make Caddy refuse the whole configuration.
func TestAHandWrittenBlockForTheHostnameIsLeftAlone(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	main := ownersCaddyfile + "\nhttps://status.example.org:443 {\n\treverse_proxy 127.0.0.1:8480\n}\n"
	h := caddyHost("host", fileMount, map[string]string{"/opt/web/Caddyfile": main})
	p := plan(t, cfg, report(t, cfg, true), h)
	if p.Pending() || !strings.Contains(p.Held, "already has a site block for status.example.org") {
		t.Fatalf("%s %q", p.Kind, p.Held)
	}
}

// A configuration the server's own Caddy refuses is put back from the
// backup, and the server is not reloaded.
func TestAFailedValidationRestoresTheBackup(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	h := caddyHost("host", fileMount, map[string]string{"/opt/web/Caddyfile": ownersCaddyfile})
	p := plan(t, cfg, report(t, cfg, true), h)
	h.fail["caddy validate"] = "Error: adapting config using caddyfile: ambiguous site definition"
	err := hostcaddy.Execute(p, h, at, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "put back as it was") || !strings.Contains(err.Error(), "ambiguous site definition") {
		t.Fatalf("%v", err)
	}
	if h.files["/opt/web/Caddyfile"] != ownersCaddyfile {
		t.Errorf("not restored:\n%s", h.files["/opt/web/Caddyfile"])
	}
	for _, c := range h.ran {
		if strings.Contains(c, "caddy reload") {
			t.Error("reloaded after a failed validation")
		}
	}
}

// A file the toolkit created is removed again when validation fails.
func TestAFailedValidationRemovesANewFile(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	main := "import sites/*.caddy\n"
	h := caddyHost("host", dirMount, map[string]string{"/opt/web/caddy/Caddyfile": main})
	p := plan(t, cfg, report(t, cfg, true), h)
	h.fail["caddy validate"] = "Error"
	if err := hostcaddy.Execute(p, h, at, &bytes.Buffer{}); err == nil {
		t.Fatal("no error")
	}
	if _, ok := h.files[p.Path]; ok {
		t.Error("the new file is still there")
	}
}

// A file changed between the plan and the write is not written.
func TestAFileChangedSinceThePlanIsNotWritten(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	h := caddyHost("host", fileMount, map[string]string{"/opt/web/Caddyfile": ownersCaddyfile})
	p := plan(t, cfg, report(t, cfg, true), h)
	h.files["/opt/web/Caddyfile"] += "# edited\n"
	if err := hostcaddy.Execute(p, h, at, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(h.files["/opt/web/Caddyfile"], "BEGIN") {
		t.Error("written anyway")
	}
}

// The check after the reload reports a site that has not answered yet, and
// never fails: the certificate may still be on its way.
func TestProbeReportsAPendingCertificateWithoutFailing(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	h := caddyHost("host", fileMount, map[string]string{"/opt/web/Caddyfile": ownersCaddyfile})
	p := plan(t, cfg, report(t, cfg, true), h)
	delete(h.answers, "curl ")
	h.fail["curl "] = "curl: (35) TLS connect error: tlsv1 alert internal error"
	var out bytes.Buffer
	slept := 0
	hostcaddy.Probe(p, h, &out, 3, time.Second, func(time.Duration) { slept++ })
	if slept != 2 || !strings.Contains(out.String(), "not yet") || !strings.Contains(out.String(), "paisans ingress check --app status") {
		t.Fatalf("slept %d:\n%s", slept, out.String())
	}
	wantRan(t, h, "--resolve 'status.example.org:443:127.0.0.1' https://status.example.org/healthz")
}

// Another deployment's marked block, under a colliding token, is not this
// deployment's to replace.
func TestAnotherDeploymentsBlockIsNotReplaced(t *testing.T) {
	cfg := fixture(t, "127.0.0.1:8480")
	other := ownersCaddyfile + "\n# BEGIN paisans-f2a9 deployment f2a90000-0000-4000-8000-000000000000: written by `paisans apply`.\nx.example.org {\n}\n# END paisans-f2a9\n"
	h := caddyHost("host", fileMount, map[string]string{"/opt/web/Caddyfile": other})
	p := plan(t, cfg, report(t, cfg, true), h)
	if p.Pending() || !strings.Contains(p.Held, "names another deployment") {
		t.Fatalf("%s %q", p.Kind, p.Held)
	}
}

func TestParseReadsTheTopLevelOnly(t *testing.T) {
	p := hostcaddy.Parse(`{
	admin 127.0.0.1:2020
	acme_dns cloudflare {$CF_API_TOKEN}
}
(common) {
	import inner/*
}
import conf.d/*.caddy # sites
a.example.org, http://b.example.org:8080 {
	import common
	# a { comment
	respond "}" 200
}
`)
	if p.Admin != "127.0.0.1:2020" || p.AdminOff {
		t.Errorf("admin %q off %v", p.Admin, p.AdminOff)
	}
	if strings.Join(p.Imports, ",") != "conf.d/*.caddy" {
		t.Errorf("imports %q", p.Imports)
	}
	if strings.Join(p.Sites, ",") != "a.example.org,b.example.org" {
		t.Errorf("sites %q", p.Sites)
	}
}

func wantRan(t *testing.T, h *host, command string) {
	t.Helper()
	for _, c := range h.ran {
		if strings.Contains(c, command) {
			return
		}
	}
	t.Errorf("never ran %q; ran:\n%s", command, strings.Join(h.ran, "\n"))
}

func file(t *testing.T, plan *render.Plan, path string) string {
	t.Helper()
	for _, f := range plan.Files {
		if f.Path == path {
			return f.Content
		}
	}
	t.Fatalf("no rendered %s", path)
	return ""
}

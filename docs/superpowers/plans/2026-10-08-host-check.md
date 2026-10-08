# Host Check Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Before `host prepare`, `apply`, `site add` and `prune` change a host, inventory what is already on it, compare that with what the site will claim, and refuse a conflict, or proceed touching only the toolkit's own things on a shared host.

**Architecture:** A new package `internal/hostcheck` with three parts, each testable alone: `ClaimsFor` (from `paisans.yaml` only, built on `render.SiteListeners`), `Inspect` (read only probes through the existing transport) and `Classify` (ownership, then clean, shared or conflict). `cmd/paisans` runs one gate helper first in each of the four commands and passes "shared" on as options that already exist (`apply.KeepImages`) or are added here (`hostprep.Shared`, a `shared` argument to `apply.BuildVolumePrune`, `siteadd.Plan.KeepImages`). `preflight` drops its own port list for the claims and gains a `host` check.

**Tech Stack:** Go 1.26, standard library only, the repository's fake transport test style.

**Spec:** `docs/specs/2026-10-08-monitor-role-and-host-check.md`, Part 2 and the hostcheck, hostprep, apply and prune lines of *Testing*.

## Global Constraints

- No em dashes in anything written: docs, comments, commit messages.
- Never write rejected alternatives in docs, comments or commits.
- Commit subjects: `<type>: <lowercase imperative subject>`, body wrapped at 80, ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Fixture addresses: the existing fixture mesh `10.44.0.0/24` (private), and documentation ranges `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24` for anything else. No real hostname or address.
- `render/ports.go` stays the single list of listeners. A new claim is a line in `render.SiteListeners`, with its `Key`.
- conflict: refuse, nothing changed, one line per conflict naming resource, claiming key and holder. No override flag.
- shared: ufw must be active with `Default: deny (incoming)` and firewalld inactive, else refuse with the reason; never `ufw default` or `ufw --force enable`; no image cleanup; prune removes only `paisans-*` labelled volumes.
- clean: today's behaviour unchanged. An empty Docker install is clean.
- The class is computed every run and never stored.
- No `git stash`. Do not push.

## Review Focus

1. **Our own deployed stack must read as clean.** A host already running the toolkit's containers (host network etcd and Postgres, published app ports through `docker-proxy`, the kernel's WireGuard socket with no process) must not be called shared or conflicting with itself. Pinned by `TestOurOwnDeploymentIsClean` in Task 4.
2. **A stock Ubuntu cloud VM must be clean.** `systemd-networkd`'s DHCP socket (`systemd-network` in `ss`, which truncates names to 15 characters), `systemd-resolve` and `chronyd` must not make a fresh host shared, or every first install would demand a hand-enabled firewall. Pinned by `TestAnEmptyDockerInstallIsClean` in Task 4, whose fixture carries those sockets.
3. **A foreign published port with Docker's userland proxy disabled has no listener in `ss`.** Ports are therefore also read from each foreign container's `HostConfig.PortBindings`, which also covers a stopped container that will bind again when it starts. Pinned by `TestAForeignPublishedPortConflictsWithoutAListener` in Task 4.
4. **A loopback listener on a claimed port still blocks the bind.** A base system listener does not make a host shared, but `127.0.0.1:5432` held by anything is a conflict for a data site, because the toolkit binds the same address. Pinned by `TestALoopbackListenerOnAClaimIsStillAConflict` in Task 4.
5. **Docker installed but not answering is "could not look".** The inventory fails with a reason rather than reading an empty container list as clean. Pinned by `TestADockerThatDoesNotAnswerIsAnError` in Task 3.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/render/ports.go` (modify) | `Listener.Key`, set for every listener in `SiteListeners` |
| `internal/render/ports_test.go` (create) | keys are set |
| `internal/hostcheck/hostcheck.go` | package doc, `Run` |
| `internal/hostcheck/claims.go` | `Claims`, `ClaimsFor` |
| `internal/hostcheck/inventory.go` | `Transport`, `Inventory` and its parts, the probes, `Inspect` |
| `internal/hostcheck/parse.go` | one parser per probe |
| `internal/hostcheck/classify.go` | ownership, `Classify`, `Report`, `Print`, `Refusal` |
| `internal/hostcheck/*_test.go`, `testdata/hostcheck.yaml` | tests and fixture |
| `internal/hostprep/hostprep.go`, `ubuntu.go` (modify) | `Option`, `Shared()`, `Firewall(..., hostWide bool)` |
| `internal/apply/volumeprune.go` (modify) | `BuildVolumePrune(site, t, shared)`, `SharedPruneHeader` |
| `internal/preflight/preflight.go` (modify) | `host` check, ports from claims |
| `internal/siteadd/siteadd.go`, `patroni.go` (modify) | `Plan.KeepImages` |
| `cmd/paisans/hostcheck.go` (create) | `hostGate` |
| `cmd/paisans/main.go`, `site.go`, `prune.go` (modify) | the gate first in each command |
| `docs/development.md`, `README.md` (modify) | describe the behaviour |

---

### Task 1: Every listener names the key that claims it

**Files:**
- Modify: `internal/render/ports.go`
- Create: `internal/render/ports_test.go`

**Interfaces:**
- Produces: `render.Listener.Key string`. Keys: `mesh` (WireGuard), `etcd.members`, `sites.<site>.roles (data)` (Postgres, Patroni, bg_mon), `cluster.port` (HAProxy, its stats too), `storage.garage.sites`, `sites.<site>.roles (gateway)` (Caddy), `apps.<name>` (each app).

- [ ] **Step 1: Write the failing test**

```go
package render_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/render"
)

// Every listener names the paisans.yaml key that makes the site bind it, so
// a refusal over a port the host already uses can tell the operator what to
// change.
func TestEveryListenerNamesTheKeyThatClaimsIt(t *testing.T) {
	cfg := fixture(t)
	want := map[string]map[string]string{
		"home-a": {"WireGuard": "mesh", "etcd client": "etcd.members", "Postgres": "sites.home-a.roles (data)", "Garage S3 API": "storage.garage.sites"},
		"vm":     {"Caddy": "sites.vm.roles (gateway)"},
	}
	for site, owners := range want {
		listeners := render.SiteListeners(cfg, site)
		for _, l := range listeners {
			if l.Key == "" {
				t.Errorf("%s: %s %s names no key", site, l.Owner, l)
			}
			if strings.HasPrefix(l.Owner, "app ") {
				name := strings.Fields(l.Owner)[1]
				if l.Key != "apps."+name {
					t.Errorf("%s: %s has key %q, want apps.%s", site, l.Owner, l.Key, name)
				}
			}
		}
		for owner, key := range owners {
			found := false
			for _, l := range listeners {
				if l.Owner == owner {
					found = true
					if l.Key != key {
						t.Errorf("%s: %s has key %q, want %q", site, owner, l.Key, key)
					}
				}
			}
			if !found {
				t.Errorf("%s: no %s listener", site, owner)
			}
		}
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./internal/render -run TestEveryListenerNamesTheKeyThatClaimsIt`
Expected: FAIL to compile, `l.Key undefined`.

- [ ] **Step 3: Add `Key` and set it**

In `Listener` add, after `Owner`:

```go
	// Key is the paisans.yaml key that makes the site bind it, for a
	// message that has to tell the operator what to change.
	Key string
```

In `SiteListeners`, `add` takes the key second and every call passes one:

```go
	add := func(owner, key, proto, address string, port int) {
		out = append(out, Listener{Owner: owner, Key: key, Proto: proto, Address: address, Port: port})
	}
	roles := func(role string) string { return fmt.Sprintf("sites.%s.roles (%s)", site, role) }

	add("WireGuard", "mesh", "udp", "", wireguardPort)
	// etcd: "etcd.members"; data: roles("data"); HAProxy: "cluster.port";
	// Garage: "storage.garage.sites"; Caddy: roles("gateway");
	// each app: "apps."+name, its extra listeners included.
```

- [ ] **Step 4: Run the render and validate tests**

Run: `go test ./internal/render ./internal/validate`
Expected: PASS.

- [ ] **Step 5: Commit** `feat: name the paisans.yaml key behind every site listener`

---

### Task 2: Claims

**Files:**
- Create: `internal/hostcheck/hostcheck.go` (package doc only for now), `internal/hostcheck/claims.go`, `internal/hostcheck/claims_test.go`, `internal/hostcheck/testdata/hostcheck.yaml`

**Interfaces:**
- Consumes: `render.SiteListeners`, `render.Listener.Key`.
- Produces:

```go
type Claims struct {
	Site      string
	Listeners []render.Listener
	Interface string     // "wg0"
	Mesh      *net.IPNet // mesh.subnet
}
func ClaimsFor(cfg *config.Config, site string) (Claims, error)
```

How a later change adds a claim: add the listener to `render.SiteListeners` with its `Key` (Part 1 adds Caddy's 80 and 443 for a `mode: paisans` monitor and the `listen` port for `mode: external` there). `ClaimsFor` and every caller pick it up unchanged.

- [ ] **Step 1: Fixture** `internal/hostcheck/testdata/hostcheck.yaml`: sites `home-a` (`[data, apps]`, 10.44.0.1, public 203.0.113.10), `edge` (`[gateway, witness]`, 10.44.0.3, public 203.0.113.30); mesh `10.44.0.0/24`; cluster on home-a, port 5000; garage on home-a; etcd members `[home-a, edge]`; apps `auth` (pocket-id) and `blog` (writefreely), both `placement: cluster`.

- [ ] **Step 2: Write the failing test**

```go
func TestClaimsFollowRoles(t *testing.T) {
	cfg := fixture(t)
	data, err := hostcheck.ClaimsFor(cfg, "home-a")
	if err != nil {
		t.Fatal(err)
	}
	has := func(c hostcheck.Claims, spec, key string) bool {
		for _, l := range c.Listeners {
			if l.String() == spec && l.Key == key {
				return true
			}
		}
		return false
	}
	for spec, key := range map[string]string{
		"*:51820/udp":       "mesh",
		"10.44.0.1:5432/tcp": "sites.home-a.roles (data)",
		"127.0.0.1:5432/tcp": "sites.home-a.roles (data)",
		"10.44.0.1:3900/tcp": "storage.garage.sites",
		"10.44.0.1:2379/tcp": "etcd.members",
	} {
		if !has(data, spec, key) {
			t.Errorf("home-a does not claim %s for %s", spec, key)
		}
	}
	if has(data, "*:80/tcp", "sites.home-a.roles (gateway)") {
		t.Error("a site without the gateway role claims 80")
	}
	gateway, _ := hostcheck.ClaimsFor(cfg, "edge")
	for _, spec := range []string{"*:80/tcp", "*:443/tcp"} {
		if !has(gateway, spec, "sites.edge.roles (gateway)") {
			t.Errorf("the gateway does not claim %s", spec)
		}
	}
	if data.Interface != "wg0" || data.Mesh.String() != "10.44.0.0/24" {
		t.Errorf("interface %q, mesh %v", data.Interface, data.Mesh)
	}
	if _, err := hostcheck.ClaimsFor(cfg, "nowhere"); err == nil {
		t.Error("an undeclared site has claims")
	}
}
```

- [ ] **Step 3: Run it to see it fail** `go test ./internal/hostcheck` → undefined `hostcheck.ClaimsFor`.

- [ ] **Step 4: Implement**

```go
// ClaimsFor is what the toolkit will take on one site's host, from the
// configuration alone: no host is reached.
func ClaimsFor(cfg *config.Config, site string) (Claims, error) {
	if _, ok := cfg.Sites[site]; !ok {
		return Claims{}, fmt.Errorf("host check: no site %q is declared. Declared sites are %s", site, strings.Join(cfg.SiteNames(), ", "))
	}
	_, mesh, err := net.ParseCIDR(cfg.Mesh.Subnet)
	if err != nil {
		return Claims{}, fmt.Errorf("host check: mesh.subnet %q is not a network", cfg.Mesh.Subnet)
	}
	return Claims{Site: site, Listeners: render.SiteListeners(cfg, site), Interface: meshInterface, Mesh: mesh}, nil
}
```

- [ ] **Step 5: Pass** `go test ./internal/hostcheck`. **Commit** `feat: compute what a site claims on its host`.

---

### Task 3: Inventory

**Files:**
- Create: `internal/hostcheck/inventory.go`, `internal/hostcheck/parse.go`, `internal/hostcheck/inventory_test.go`

**Interfaces:**
- Produces:

```go
type Transport interface {
	Run(command string) (string, error)
	ReadFile(path string) (content string, found bool, err error)
	Describe() string
}
type Inventory struct {
	Host              string
	Docker            Docker
	Containers        []Container
	Volumes           []Volume
	Networks          []Network
	Sockets           []Socket
	Cgroups           map[int]string // pid -> container ID
	Links             []string
	Routes            []Route
	Firewall          Firewall
	Manifest          bool
	ManifestWireGuard bool
}
type Docker struct { Present bool; Version string; Packages []string }
type Container struct { ID, Name, Project string; PID int; Bindings []render.Listener }
type Volume struct { Name, Project string; Anonymous bool }
type Network struct { ID, Name, Project string; Subnets []string }
type Socket struct { Proto, Address string; Port int; Process string; PID int }
func (s Socket) Listener() render.Listener
type Route struct { Dst string `json:"dst"`; Dev string `json:"dev"` }
type Firewall struct { UFW, Active bool; Incoming string; Firewalld bool }
func Inspect(t Transport) (*Inventory, error)
```

The probes, one command each:

```go
const (
	dockerProbe    = `command -v docker >/dev/null 2>&1 || { echo absent; exit 0; }; docker version --format '{{.Server.Version}}'`
	packageProbe   = `dpkg-query -W -f='${Package} ${Status}\n' docker-ce docker.io 2>/dev/null; snap list docker 2>/dev/null; true`
	containerProbe = `docker ps -aq --no-trunc | xargs -r docker inspect --format '{"id":{{json .Id}},"name":{{json .Name}},"pid":{{.State.Pid}},"labels":{{json .Config.Labels}},"ports":{{json .HostConfig.PortBindings}}}'`
	volumeProbe    = `docker volume ls -q | xargs -r docker volume inspect --format '{"name":{{json .Name}},"labels":{{json .Labels}}}'`
	networkProbe   = `docker network ls -q --no-trunc | xargs -r docker network inspect --format '{"id":{{json .Id}},"name":{{json .Name}},"labels":{{json .Labels}},"ipam":{{json .IPAM.Config}}}'`
	socketProbe    = "ss -Hltnup"
	linkProbe      = "ip -o link"
	routeProbe     = "ip -j route"
	ufwProbe       = `command -v ufw >/dev/null 2>&1 || { echo 'ufw absent'; exit 0; }; ufw status verbose`
	firewalldProbe = "systemctl is-active firewalld || true"
)

func cgroupProbe(pids []int) string // for p in ...; do printf '%s ' "$p"; tr '\n' ' ' < /proc/$p/cgroup 2>/dev/null; echo; done
```

- [ ] **Step 1: Write the failing tests** in `inventory_test.go`, with a `fakeHost` answering each probe by a substring only it contains (`docker version`, `dpkg-query`, `docker inspect`, `docker volume inspect`, `docker network inspect`, `ss -Hltnup`, `/proc/`, `ip -o link`, `ip -j route`, `ufw status verbose`, `is-active firewalld`) and failing any other command:

```go
func TestInspectReadsEveryFact(t *testing.T) {
	h := caddyHost()
	inv, err := hostcheck.Inspect(h)
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Docker.Present || inv.Docker.Version != "27.3.1" || strings.Join(inv.Docker.Packages, ",") != "docker-ce" {
		t.Errorf("docker %+v", inv.Docker)
	}
	if len(inv.Containers) != 2 || inv.Containers[0].Name != "web-caddy-1" || inv.Containers[0].Project != "web" || inv.Containers[0].PID != 812 {
		t.Errorf("containers %+v", inv.Containers)
	}
	if b := inv.Containers[1].Bindings; len(b) != 1 || b[0].String() != "127.0.0.1:8081/tcp" {
		t.Errorf("bindings %+v", b)
	}
	if inv.Cgroups[812] != caddyID {
		t.Errorf("cgroups %v", inv.Cgroups)
	}
	// ... sockets (caddy *:80 pid 812), links, routes, ufw active deny,
	// firewalld inactive, volumes and networks with their projects.
	for _, c := range h.ran {
		if strings.Contains(c, "rm ") || strings.Contains(c, "ufw default") || strings.Contains(c, "enable") {
			t.Errorf("the inventory changed something: %s", c)
		}
	}
}

func TestADockerThatDoesNotAnswerIsAnError(t *testing.T) {
	h := cleanHost()
	h.fail = map[string]error{"docker version": errors.New("exit status 1")}
	if _, err := hostcheck.Inspect(h); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("got %v", err)
	}
}

func TestNoDockerSkipsTheContainerProbes(t *testing.T) {
	h := cleanHost()
	h.answers["docker version"] = "absent\n"
	inv, err := hostcheck.Inspect(h)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Docker.Present {
		t.Error("docker read as present")
	}
	for _, c := range h.ran {
		if strings.Contains(c, "xargs") {
			t.Errorf("probed Docker that is not there: %s", c)
		}
	}
}

func TestParseSockets(t *testing.T) // *:80, 0.0.0.0:22, [::]:5432, 127.0.0.53%lo:53, a kernel udp socket with no users
```

- [ ] **Step 2: Fail** `go test ./internal/hostcheck` (undefined `Inspect`).

- [ ] **Step 3: Implement** parsers in `parse.go` (`parseSockets`, `bindAddress`, `parseContainers`, `parseVolumes`, `parseNetworks`, `parseRoutes`, `parseLinks`, `parseUFW`, `parsePackages`, `parseCgroups`) and `Inspect` in `inventory.go`. `Inspect` runs the Docker probe first; `absent` means no Docker and the three container probes are skipped; an error is "Docker is installed and did not answer". Listening PIDs are collected from the sockets and their cgroups read in one probe. The manifest is read with `ReadFile` and parsed as `render.Manifest`; `ManifestWireGuard` is true when it lists `etc/wireguard/wg0.conf`.

- [ ] **Step 4: Pass**, **Commit** `feat: inventory what a host already runs, read only`.

---

### Task 4: Ownership and classification

**Files:**
- Create: `internal/hostcheck/classify.go`, `internal/hostcheck/classify_test.go`; add `Run` to `hostcheck.go`.

**Interfaces:**
- Produces:

```go
type Class int
const ( Clean Class = iota; Shared; Conflicted )
type Conflict struct { Resource, Key, Holder string }
type Report struct {
	Site, Host string
	Class      Class
	Conflicts  []Conflict
	Foreign    []string
	Inventory  *Inventory
}
func Classify(claims Claims, inv *Inventory) *Report
func Run(cfg *config.Config, site string, t Transport) (*Report, error)
func (r *Report) Shared() bool
func (r *Report) Print(w io.Writer)
func (r *Report) Refusal() error
```

Rules:
- A container is ours iff its `com.docker.compose.project` starts `paisans-`.
- A socket's holder: the container its PID's cgroup names; else, for `docker-proxy`, the container with the same binding; else, a process-less udp socket on 51820 when the manifest records wg0.conf and wg0 exists, ours; else the process. Base system: loopback address, or process `sshd`, `systemd-resolve(d)`, `systemd-network(d)`, `chronyd`, `tailscaled`.
- Conflict: a claim overlapping (render's `Overlaps`) any socket not ours, or any binding of a foreign container; `wg0` present without the manifest recording wg0.conf; a foreign Docker network subnet, or a route not through wg0 or a Docker bridge, overlapping the mesh.
- Foreign (makes the host shared): a container not ours; a socket neither ours nor base; a network not ours other than Docker's `bridge`, `host`, `none`; a volume labelled by another project, or named with no project. Anonymous unlabelled volumes are neutral.

- [ ] **Step 1: Write the failing tests** (fixtures in `inventory_test.go`):

```go
func TestAnEmptyDockerInstallIsClean(t *testing.T)                      // home-a, cleanHost: Clean, no Foreign, Refusal nil
func TestOurOwnDeploymentIsClean(t *testing.T)                          // paisans-infra host network, docker-proxy app port, kernel wg socket, manifest
func TestAForeignCaddyBesideASiteThatDoesNotClaimItIsShared(t *testing.T) // home-a vs caddyHost: Shared, Refusal nil
func TestAForeignCaddyConflictsWithAGateway(t *testing.T)               // edge vs caddyHost: Conflicted, 80 and 443 lines
func TestAForeignPostgresConflictsWithADataSite(t *testing.T)           // 0.0.0.0:5432 postgres pid 1200
func TestALoopbackListenerOnAClaimIsStillAConflict(t *testing.T)        // 127.0.0.1:5432 postgres
func TestAForeignPublishedPortConflictsWithoutAListener(t *testing.T)   // stopped container binding 0.0.0.0:443 vs edge
func TestAWireGuardTheToolkitDidNotWriteConflicts(t *testing.T)
func TestAForeignNetworkOverlappingTheMeshConflicts(t *testing.T)       // lab_default 10.44.0.0/16
func TestASharedHostNeedsTheFirewallUp(t *testing.T)                    // inactive, default allow, firewalld: each refused with its reason
func TestTheReportNamesEachConflict(t *testing.T)                       // Print: "CONFLICT  *:80/tcp (Caddy): claimed by sites.edge.roles (gateway), held by container web-caddy-1 (compose project web)"
```

For example:

```go
func TestAForeignCaddyConflictsWithAGateway(t *testing.T) {
	r := check(t, "edge", caddyHost())
	if r.Class != hostcheck.Conflicted {
		t.Fatalf("class %s, want conflict:\n%s", r.Class, printed(r))
	}
	for _, want := range []string{
		"*:80/tcp (Caddy): claimed by sites.edge.roles (gateway), held by container web-caddy-1 (compose project web)",
		"*:443/tcp (Caddy): claimed by sites.edge.roles (gateway), held by container web-caddy-1 (compose project web)",
	} {
		if !strings.Contains(printed(r), want) {
			t.Errorf("no line %q:\n%s", want, printed(r))
		}
	}
	if err := r.Refusal(); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("refusal %v", err)
	}
}
```

- [ ] **Step 2: Fail**, **Step 3: Implement** `classify.go` as the rules above, `Refusal`:

```go
func (r *Report) Refusal() error {
	switch r.Class {
	case Conflicted:
		return fmt.Errorf("host check: %s holds %d thing(s) %s claims, each listed above as CONFLICT, so nothing was changed. Move what holds each one, or change the paisans.yaml key it names, and run again", r.Host, len(r.Conflicts), r.Site)
	case Shared:
		if why := firewallGap(r.Inventory.Firewall); why != "" {
			return fmt.Errorf("host check: %s runs services the deployment does not own, and on a shared host the toolkit never sets ufw's default policy or enables it, so both must already be in place: %s. Allow what those services need, then enable ufw with incoming denied by default (ufw default deny incoming; ufw enable) and run again", r.Host, why)
		}
	}
	return nil
}
```

- [ ] **Step 4: Pass**, **Commit** `feat: classify a host as clean, shared or in conflict`.

---

### Task 5: host prepare leaves a shared host's firewall policy alone

**Files:** Modify `internal/hostprep/hostprep.go`, `internal/hostprep/ubuntu.go`; test in `internal/hostprep/hostprep_test.go`.

**Interfaces:**
- Produces: `type Option func(*options)`, `func Shared() Option`, `func Build(site string, cfg *config.Config, t Transport, opts ...Option) (*Plan, error)`, `Profile.Firewall(t Transport, rules []Rule, hostWide bool) (Section, error)`.

- [ ] **Step 1: Failing test**

```go
// On a shared host, prepare adds and removes only its own commented rules:
// the default policy and enabling ufw are host wide, and something else
// lives there.
func TestASharedHostNeverSetsTheFirewallsDefaults(t *testing.T) {
	cfg := fixture(t)
	host := freshHost()
	host.responses[probeFirewall] = "ufw present\nstatus inactive\n"
	host.files["/etc/default/ufw"] = "DEFAULT_INPUT_POLICY=\"ACCEPT\"\nDEFAULT_OUTPUT_POLICY=\"DROP\"\n"
	dedicated, err := hostprep.Build("home-a", cfg, host)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := hostprep.Build("home-a", cfg, host, hostprep.Shared())
	if err != nil {
		t.Fatal(err)
	}
	if indexOf(commands(dedicated), "ufw --force enable") < 0 {
		t.Fatal("the control plan does not enable ufw, so this test proves nothing")
	}
	for _, c := range commands(shared) {
		if strings.HasPrefix(c, "ufw default") || strings.Contains(c, "ufw --force enable") {
			t.Errorf("a shared host's plan runs %q", c)
		}
	}
	if indexOf(commands(shared), "ufw allow 22/tcp comment 'paisans: ssh, the bootstrap route'") < 0 {
		t.Errorf("a shared host's plan does not add its own rules: %q", commands(shared))
	}
}
```

- [ ] **Step 2-4:** Add the option; `Build` calls `profile.Firewall(t, rules, !o.shared)`; in `ubuntu.Firewall`, when `!hostWide`, skip the three policy steps and add `Present: "firewall: default policy and enabled state left alone, since the host is shared"`. Pass. **Commit** `feat: leave a shared host's firewall policy alone in host prepare`.

---

### Task 6: prune keeps unlabelled volumes on a shared host; apply keeps images

**Files:** Modify `internal/apply/volumeprune.go`, `cmd/paisans/prune.go`; tests in `internal/apply/volumeprune_test.go`, `internal/apply/apply_test.go`, `cmd/paisans/prune_test.go`.

**Interfaces:**
- Produces: `func BuildVolumePrune(site string, t Transport, shared bool) (*VolumePrune, error)`, `VolumePrune.Shared bool`, `const SharedPruneHeader`.

- [ ] **Step 1: Failing tests**

```go
// On a shared host only a volume a paisans-* compose project labelled goes.
// An anonymous one may be another project's, so it is listed and kept.
func TestASharedHostPrunesOnlyLabelledVolumes(t *testing.T) {
	host := &volumeHost{listing: listing(
		"volume\t"+leaked1+"\t48234496\t{\"com.docker.volume.anonymous\":\"\"}\tcache log",
		"volume\t"+oldName+"\t1024\tnull\t",
		"volume\tpaisans-talk_data\t4096\t{\"com.docker.compose.project\":\"paisans-talk\"}\tx",
	)}
	p, err := apply.BuildVolumePrune("home-a", host, true)
	// want: only paisans-talk_data removed; ExecuteVolumePrune issues one rm.
}

// apply on a shared host runs with KeepImages: no image is listed or
// removed, and a dangling volume its own containers did not leave is never
// removed.
func TestASharedHostApplyRemovesNoImageAndNoForeignVolume(t *testing.T)
```

- [ ] **Step 2-4:** `pruneVerdict(name, labels, shared)` keeps an anonymous volume when shared, with the reason. `printVolumePrune` prints `SharedPruneHeader` when `plan.Shared`. Pass. **Commit** `feat: prune only labelled volumes on a shared host`.

---

### Task 7: preflight on the claims, with a host check

**Files:** Modify `internal/preflight/preflight.go`, `internal/preflight/preflight_test.go`.

- [ ] **Step 1: Failing test**

```go
// A gateway being added is checked for 80 and 443, which the old port list
// lacked.
func TestAGatewaysWebPortsAreChecked(t *testing.T) {
	prepared(t)
	h := hosts()
	h["vm"].override(rule{match: "ss -Hltnu", out: "tcp LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:((\"nginx\",pid=900,fd=6))\n"})
	r, err := Run(fixture(t), "vm", transportsOf(h))
	if err != nil {
		t.Fatal(err)
	}
	if got := refusedDetail(t, r, "vm", "ports"); !strings.Contains(got, "80/tcp (Caddy)") {
		t.Errorf("detail %q", got)
	}
	if got := refusedDetail(t, r, "vm", "host"); !strings.Contains(got, "nginx") {
		t.Errorf("detail %q", got)
	}
}
```

- [ ] **Step 2-4:** Replace `port`, `wanted()` and `listening()`. A new `host` check runs `hostcheck.Run` (through `var inspectHost = hostcheck.Run`) and refuses on `Refusal()`; `hostPrepare` passes `hostprep.Shared()` when the report says shared; `ports` compares every claim with every socket the inventory read, by `Overlaps`, and names each taken one as `<port>/<proto> (<owner>)`. `healthy()` answers the inventory's probes. Pass. **Commit** `feat: check preflight's ports against the host check's claims`.

---

### Task 8: The gate in each command

**Files:** Create `cmd/paisans/hostcheck.go`, `cmd/paisans/hostcheck_test.go`; modify `cmd/paisans/main.go` (`runHostPrepare`, `runApply`), `cmd/paisans/site.go`, `cmd/paisans/prune.go`, `internal/siteadd/siteadd.go`, `internal/siteadd/patroni.go`.

```go
// hostGate runs the host check on one site before a command changes
// anything, in a dry run as well as with --execute, prints the report, and
// returns the refusal for a conflict or for a shared host without a
// firewall. It is the first thing each command that changes a host does.
func hostGate(w io.Writer, cfg *config.Config, site string, t hostcheck.Transport) (*hostcheck.Report, error) {
	report, err := hostcheck.Run(cfg, site, t)
	if err != nil {
		return nil, err
	}
	report.Print(w)
	fmt.Fprintln(w)
	if err := report.Refusal(); err != nil {
		return nil, err
	}
	return report, nil
}
```

- [ ] **Step 1: Failing test**: a conflicted host returns the error, prints the CONFLICT line, and the fake's commands are only the inventory's probes; a shared host with ufw inactive returns the firewall refusal; a clean host returns nil.
- [ ] **Step 2-4:** Wire: `runHostPrepare` passes `hostprep.Shared()`; `runApply` appends `apply.KeepImages()`; `runPrune` passes `report.Shared()`; `runSiteAdd` gates the new site before `siteadd.Build` and sets `plan.KeepImages`, which the replica stage's whole apply passes as `apply.KeepImages()`. Pass. **Commit** `feat: run the host check first in host prepare, apply, site add and prune` (Founder decision, in the approved spec).

---

### Task 9: Documentation

- [ ] `docs/development.md`: a section *The host check* (claims, inventory, ownership, classes, what each class does in each command); the prune paragraph and the image prune paragraph say what a shared host changes; the run list mentions the report.
- [ ] `README.md`: the prune paragraph's "dedicated" premise becomes the host check's class; the preflight table's Ports row lists 80 and 443 for a gateway and names the claims as its source, and gains a Host row.
- [ ] `gofmt -l .`, `go vet ./...`, `go test ./...`. **Commit** `docs: describe the host check`.

---

### Task 10: Review follow-up

Found in review of the branch; each is a new commit with its failing test first.

- [x] A route broader than the mesh is a note; a route equal to it or inside it conflicts. Docker subnets keep any overlap. (`TestABroaderRouteIsANoteNotAConflict`)
- [x] `reject (incoming)` satisfies a shared host's firewall, and the refusal's advice allows `ssh.port` before enabling ufw. (`Claims.SSHPort`, `Report.SSHPort`)
- [x] Every probe runs with `LC_ALL=C`; `id -u` must be 0; the cgroup read's `2>/dev/null` precedes its `<`; the three inspects tolerate what vanished since the listing.
- [x] IPv4-mapped addresses fold to IPv4 before any comparison.
- [x] `storage rotate-key` and `storage add` run the host check first (`gateSites`) and keep images on a shared site.
- [x] host prepare refuses a Docker snap, as it refuses `docker.io`.
- [x] HAProxy's stats listener is keyed `sites.<site>.roles (apps)`.

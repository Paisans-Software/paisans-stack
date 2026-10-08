# Monitor Role Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the `uptime` kind a site of its own: a `monitor` role that is never the gateway, serves its own apps (through the toolkit's Caddy, or behind the operator's own web server), is published in DNS at its own address, checks its own public URL, and comes with `paisans ingress show` and `paisans ingress check` for an operator handing it to a web server the toolkit does not own.

**Architecture:** `config` gains the role and a per site `ingress` block; `validate` gains the role and ingress rules in a file of their own; `render` decides per app whether the gateway or a monitor serves it (`render.ServedBy`), renders the monitor's Caddy from the gateway's templates with only its own routes, publishes a `mode: external` app on `listen`, and adds the monitor's self check to the seed. `render.SiteListeners` gains the monitor's claims, so the host check, preflight and `port-collision` see them with no change of their own. `dns.Desired` points a monitor app's hostnames at the monitor. A new package `internal/ingress` builds the hand-off sheet and runs the read only checks through injectable probes, and `cmd/paisans/ingress.go` wires both commands.

**Tech Stack:** Go 1.26, standard library only (`net/http`, `crypto/tls`, `net/http/httptest` in tests).

**Spec:** `docs/specs/2026-10-08-monitor-role-and-host-check.md`, Part 1, and the config, validate, render, dns and ingress check lines of *Testing*. Part 2 (the host check) is already on the base branch; Part 3 is another branch and is not touched here.

## Global Constraints

- No em dashes in anything written: docs, comments, commit messages, test names.
- Never write rejected alternatives in docs, comments or commits.
- Commit subjects: `<type>: <lowercase imperative subject>`, body wrapped at 80 and saying why; the role rules carry "Founder decision."; every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Rule names, exactly: `monitor-on-gateway` (refuse), `monitor-on-witness` (refuse), `monitor-shares-a-site` (warn, with data or apps), `monitor-without-uptime` (refuse), `uptime-needs-a-monitor-site` (refuse), `monitor-without-public-address` (refuse), `ingress-outside-monitor` (refuse), `ingress-listen-mode` (refuse), `ingress-listen-public` (refuse). No override for any refusal.
- `roles: []` with a pinned app stays legal for other kinds. `roles: [monitor]` needs no other role.
- `ingress.mode` is `paisans` (default) or `external`; `listen` is `host:port`, required with external and refused with paisans. Unknown keys stay an error (`KnownFields`).
- `acme.provider` is required when any site is a gateway or a monitor in mode paisans.
- The monitor's Caddy is the same image as the gateway's (`caddyImage()`), host network, in `infra`, DNS-01 through `acme.provider`.
- `render/ports.go` stays the single list of listeners; every new one carries its `Key`.
- Fixture addresses: mesh `10.44.0.0/24`, documentation ranges `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`. No real hostname or address.
- `ingress check` never writes anything and never contacts a host over ssh. Tests never touch real DNS or the network: resolver, HTTP client and dialer are injected.
- No `git stash`. Do not push. Do not commit a built binary (`go build -o /dev/null ./...`).
- Goldens change only through `go test ./internal/render -update`, and the diff is read before it is committed.

## Review Focus

1. **A mode paisans monitor must not be read as a second gateway by DNS or routing.** Its hostnames must leave the gateway's Caddyfile and point at its own address, while every other app still points at the gateway. Pinned by `TestMonitorAppsLeaveTheGatewayCaddyfile` (Task 5) and `TestMonitorAppsPointAtTheMonitor` (Task 7).
2. **`TRUST_PROXY` for a loopback `listen` behind Docker.** A port Docker publishes on `127.0.0.1` reaches the container through `docker-proxy` (or, with the userland proxy off, a masqueraded hairpin), so the container sees the compose network's gateway address, never loopback. `loopback` alone would leave every request keyed to one address in the login rate limiter. The rendered value is `loopback,uniquelocal`. Pinned by `TestTrustProxyFollowsWhereTheProxyConnectsFrom` (Task 6).
3. **A `listen` that collides with something else on the site.** `127.0.0.1:5432` on a monitor that also holds `data` must be a `port-collision`, not a container that fails to start. Pinned by `TestAnExternalListenIsClaimed` (Task 4), which also checks the collision.
4. **`ingress check` against a server whose certificate is for another name.** The check must fail item 2 and say the certificate does not cover the hostname, rather than erroring out of the whole run. Pinned by `TestCheckFailsACertificateForAnotherName` (Task 9).
5. **A redirect whose `redirect_uri` is `http://`.** Item 3 must fail and name the fix, and a redirect back to `/login` (sign in not configured) must fail with its own reason. Pinned by `TestCheckFailsAnHTTPCallback` and `TestCheckFailsWhenSignInIsNotConfigured` (Task 9).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/config.go` (modify) | `RoleMonitor`, `Ingress`, `IngressMode`, `Site.IngressMode`, `Site.RunsCaddy`, `Ingress.ListenHostPort`, structural checks, acme requirement |
| `internal/config/config_test.go` (modify) | structural tests |
| `internal/validate/monitor.go` (create) | the role and ingress rules |
| `internal/validate/validate.go` (modify) | call them |
| `internal/validate/testdata/*.yaml` (create, modify) | one fixture per rule; uptime fixtures moved onto a monitor site |
| `internal/render/ports.go` (modify) | monitor claims |
| `internal/render/monitor.go` (create) | `ServedBy`, `trustProxy`, the monitor's routes |
| `internal/render/plan.go`, `site.go`, `appview.go`, `uptime.go` (modify) | routing split, monitor Caddy, values, self check |
| `internal/render/templates/infra-compose.yaml.tmpl`, `uptime/compose.yaml.tmpl`, `uptime/.env.secret.tmpl` (modify) | caddy on `RunsCaddy`, the listen publish, `TRUST_PROXY` |
| `internal/render/testdata/deployment.yaml`, `secrets.fixture.yaml`, `golden/` (modify) | a `watch` monitor site |
| `internal/render/monitor_test.go` (create) | render tests for the monitor |
| `internal/hostprep/firewall.go` (modify) | 80 and 443 on a mode paisans monitor |
| `internal/secretsgen/secretsgen.go`, `cmd/paisans/secrets.go` (modify) | the ACME token owed and required for a monitor's Caddy |
| `internal/dns/dns.go` (modify) | monitor apps at the monitor's address |
| `internal/ingress/ingress.go`, `show.go`, `check.go` (create) | target, hand-off sheet, read only checks |
| `internal/ingress/*_test.go` (create) | tests, `httptest` TLS servers |
| `cmd/paisans/ingress.go`, `main.go` (modify) | the two commands |
| `examples/paisans.example.yaml`, `examples/secrets.example.yaml` (modify) | a monitor site |
| `docs/guides/behind-your-own-web-server.md` (create) | the guide |
| `README.md`, `docs/development.md`, `docs/specs/2026-10-07-uptime-monitoring.md` (modify) | the design, the code map, the amendment |

---

### Task 1: The `monitor` role and the `ingress` block load

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.RoleMonitor`; `config.IngressMode` with `IngressPaisans`, `IngressExternal`; `config.Ingress{Mode IngressMode; Listen string}`; `Site.Ingress *Ingress` (`yaml:"ingress"`); `func (s Site) IngressMode() IngressMode` (paisans when unset); `func (s Site) RunsCaddy() bool` (gateway, or monitor in mode paisans); `func (i Ingress) ListenHostPort() (string, int, bool)`; `func (c *Config) MonitorSites() []string`.

- [ ] **Step 1: Write the failing tests**

```go
func TestAMonitorSiteLoadsWithIngress(t *testing.T) {
	cfg, err := config.Load(write(t, validConfig+monitorSite("    ingress:\n      mode: external\n      listen: 127.0.0.1:8480\n")+"apps:\n"+statusApp("watch", "")))
	if err != nil {
		t.Fatal(err)
	}
	watch := cfg.Sites["watch"]
	if !watch.Has(config.RoleMonitor) || watch.IngressMode() != config.IngressExternal {
		t.Fatalf("watch: %+v", watch)
	}
	host, port, ok := watch.Ingress.ListenHostPort()
	if !ok || host != "127.0.0.1" || port != 8480 {
		t.Fatalf("listen: %s %d %v", host, port, ok)
	}
	if watch.RunsCaddy() {
		t.Fatal("an external monitor runs no Caddy")
	}
}

func TestIngressModeDefaultsToPaisansAndRunsCaddy(t *testing.T) {
	cfg, err := config.Load(write(t, validConfig+monitorSite("")+"apps:\n"+statusApp("watch", "")))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sites["watch"].IngressMode(); got != config.IngressPaisans || !cfg.Sites["watch"].RunsCaddy() {
		t.Fatalf("mode %s, caddy %v", got, cfg.Sites["watch"].RunsCaddy())
	}
}

func TestIngressShapeIsChecked(t *testing.T) {
	for _, tc := range []struct{ ingress, want string }{
		{"    ingress:\n      mode: nginx\n", "sites.watch.ingress.mode: unknown mode"},
		{"    ingress:\n      mode: external\n      listen: localhost:8480\n", "sites.watch.ingress.listen"},
		{"    ingress:\n      mode: external\n      listen: 127.0.0.1:0\n", "sites.watch.ingress.listen"},
		{"    ingress:\n      proxy: nginx\n", "field proxy not found"},
	} {
		msg := loadErr(t, validConfig+monitorSite(tc.ingress)+"apps:\n"+statusApp("watch", ""))
		if !strings.Contains(msg, tc.want) {
			t.Errorf("%q: %s", tc.ingress, msg)
		}
	}
}

func TestACMEProviderIsRequiredForAMonitorsOwnCaddy(t *testing.T) {
	body := strings.Replace(validConfig, "acme:\n  provider: desec\n", "", 1)
	body = strings.Replace(body, "roles: [gateway, witness]", "roles: [witness]", 1)
	if msg := loadErr(t, body+monitorSite("")+"apps:\n"+statusApp("watch", "")); !strings.Contains(msg, "acme.provider: required") {
		t.Fatalf("paisans mode: %s", msg)
	}
	if _, err := config.Load(write(t, body+monitorSite("    ingress:\n      mode: external\n      listen: 127.0.0.1:8480\n")+"apps:\n"+statusApp("watch", ""))); err != nil {
		t.Fatalf("external mode needs no provider: %v", err)
	}
}
```

`monitorSite(extra)` is a helper in the test file returning a `watch` site with `roles: [monitor]`, address `10.44.0.9`, `public_address: 203.0.113.20`, the ssh section the other sites use, and `extra` appended. The exact form of `validConfig` decides whether `apps:` must be appended or merged; read it first.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/config -run 'Monitor|Ingress' -count=1`
Expected: FAIL, `config.RoleMonitor` undefined.

- [ ] **Step 3: Implement**

```go
	// RoleMonitor is a host that runs the uptime monitor. It is never the
	// gateway or the witness, because its job is to report their failures,
	// and it serves its own apps rather than routing them through the
	// gateway. See docs/specs/2026-10-08-monitor-role-and-host-check.md.
	RoleMonitor Role = "monitor"

// IngressMode is what sits in front of a monitor site's apps.
type IngressMode string

const (
	// IngressPaisans runs the toolkit's own Caddy on the monitor.
	IngressPaisans IngressMode = "paisans"
	// IngressExternal leaves TLS to a web server the operator runs, which
	// proxies to Listen.
	IngressExternal IngressMode = "external"
)

// Ingress is how the internet reaches a monitor site's apps.
type Ingress struct {
	Mode   IngressMode `yaml:"mode"`
	Listen string      `yaml:"listen"`
}

func (s Site) IngressMode() IngressMode {
	if s.Ingress == nil || s.Ingress.Mode == "" {
		return IngressPaisans
	}
	return s.Ingress.Mode
}

func (s Site) RunsCaddy() bool {
	return s.Has(RoleGateway) || (s.Has(RoleMonitor) && s.IngressMode() == IngressPaisans)
}

func (i Ingress) ListenHostPort() (string, int, bool) {
	host, port, err := net.SplitHostPort(i.Listen)
	if err != nil || !isIPv4(host) {
		return "", 0, false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", 0, false
	}
	return host, n, true
}
```

Structural: the role list and messages gain `monitor`; an `ingress.mode` that is neither value is refused by name; a non-empty `listen` that `ListenHostPort` rejects is refused ("an IPv4 address and a port, for example 127.0.0.1:8480"). The acme check becomes "any site `RunsCaddy()`", with the message naming a gateway or a monitor in mode paisans.

- [ ] **Step 4: Run them to see them pass**

Run: `go test ./internal/config -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat: add the monitor role and its ingress block"
```

---

### Task 2: The role rules

**Files:**
- Create: `internal/validate/monitor.go`
- Modify: `internal/validate/validate.go` (two lines in `Check`)
- Create: `internal/validate/testdata/{monitor-on-gateway,monitor-on-witness,monitor-shares-a-site,monitor-without-uptime,uptime-needs-a-monitor-site,monitor-without-public-address}.yaml`
- Modify: `internal/validate/testdata/{uptime-needs-an-admin-group,uptime-without-smtp,smtp-on-a-kind-without-mail}.yaml` (their uptime app moves to a `watch` monitor site)
- Test: `internal/validate/validate_test.go` (rows in `TestRulesFire`)

**Interfaces:**
- Consumes: Task 1's `RoleMonitor`, `Site.PublicAddress`.
- Produces: `func (c *checker) monitorRoles()`.

- [ ] **Step 1: Write the failing tests**

Rows in `TestRulesFire`:

```go
		{"monitor-on-gateway", "monitor-on-gateway", validate.Refuse},
		{"monitor-on-witness", "monitor-on-witness", validate.Refuse},
		{"monitor-shares-a-site", "monitor-shares-a-site", validate.Warn},
		{"monitor-without-uptime", "monitor-without-uptime", validate.Refuse},
		{"uptime-needs-a-monitor-site", "uptime-needs-a-monitor-site", validate.Refuse},
		{"monitor-without-public-address", "monitor-without-public-address", validate.Refuse},
```

Each fixture is `valid.yaml` plus the smallest change that breaks the rule: `monitor-on-gateway` puts `monitor` on `vm` (`[gateway, witness, monitor]`, so `monitor-on-witness` fires too, which the table allows); `monitor-on-witness` adds a `watch` site `[witness, monitor]` and keeps etcd at three members by dropping `vm` from them; `monitor-shares-a-site` gives `home-a` `[data, apps, monitor]` with `public_address: 203.0.113.20` and pins the uptime app there; `monitor-without-uptime` adds a `watch` site `[monitor]` with nothing pinned; `uptime-needs-a-monitor-site` pins the uptime app to `home-a`; `monitor-without-public-address` is a `watch` monitor site with the uptime app and no `public_address`.

Also:

```go
// A role-less site hosting something other than the monitor stays legal, and
// so does a monitor site holding nothing but the monitor.
func TestAMonitorOnlySiteIsQuiet(t *testing.T) {
	result := validate.Check(load(t, "uptime-without-smtp"))
	for _, f := range result.Findings {
		if f.Rule != "uptime-without-smtp" {
			t.Errorf("unexpected finding: %s", f)
		}
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/validate -count=1`
Expected: FAIL, each new rule "did not fire".

- [ ] **Step 3: Implement `monitorRoles`**

```go
func (c *checker) monitorRoles() {
	for _, name := range c.cfg.SiteNames() {
		site := c.cfg.Sites[name]
		if !site.Has(config.RoleMonitor) {
			continue
		}
		key := fmt.Sprintf("sites.%s.roles", name)
		if site.Has(config.RoleGateway) {
			c.refuse("monitor-on-gateway", key, "...")
		}
		if site.Has(config.RoleWitness) {
			c.refuse("monitor-on-witness", key, "...")
		}
		if site.Has(config.RoleData) || site.Has(config.RoleApps) {
			c.warn("monitor-shares-a-site", key, "...")
		}
		if !c.hostsUptime(name) {
			c.refuse("monitor-without-uptime", key, "...")
		}
		if site.PublicAddress == "" {
			c.refuse("monitor-without-public-address", fmt.Sprintf("sites.%s.public_address", name), "...")
		}
	}
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Kind != config.KindUptime || app.Placement.Mode != config.PlacementPinned {
			continue
		}
		site, ok := c.cfg.Sites[app.Placement.Site]
		if ok && !site.Has(config.RoleMonitor) {
			c.refuse("uptime-needs-a-monitor-site", fmt.Sprintf("apps.%s.placement", name), "...")
		}
	}
}
```

Each message says what fails and what to do (the spec's reasons: a gateway's failures are what the monitor reports; etcd's tiebreaker and the thing reporting etcd losing quorum must not fail together; the warning names which failures the monitor then misses; the monitor's hostname is published at its own address).

- [ ] **Step 4: Move the three uptime fixtures onto a monitor site**

Add to each:

```yaml
  watch:
    roles: [monitor]
    address: 10.44.0.4
    public_address: 203.0.113.20
    ssh:
      host: watch.local
      user: ubuntu
      public_key: |
        ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org
```

and `placement: { pinned: watch }` for the uptime app.

- [ ] **Step 5: Run the package**

Run: `go test ./internal/validate -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/validate
git commit -m "feat: refuse a monitor on the gateway or witness, and uptime off one"
```

---

### Task 3: The ingress rules

**Files:**
- Modify: `internal/validate/monitor.go`, `validate.go`
- Create: `internal/validate/testdata/{ingress-outside-monitor,ingress-listen-mode,ingress-listen-public,ingress-listen-bypasses-firewall,ingress-external-serves-one-app}.yaml`
- Test: `internal/validate/validate_test.go`

**Interfaces:**
- Produces: `func (c *checker) ingress()`. Extra rule names beyond the spec's three, both following from it: `ingress-listen-bypasses-firewall` (warn, the spec's ufw warning) and `ingress-external-serves-one-app` (refuse: one `listen` publishes one app, and any second app pinned to an external monitor would be routed by nothing).

- [ ] **Step 1: Write the failing tests**

```go
		{"ingress-outside-monitor", "ingress-outside-monitor", validate.Refuse},
		{"ingress-listen-mode", "ingress-listen-mode", validate.Refuse},
		{"ingress-listen-public", "ingress-listen-public", validate.Refuse},
		{"ingress-listen-bypasses-firewall", "ingress-listen-bypasses-firewall", validate.Warn},
		{"ingress-external-serves-one-app", "ingress-external-serves-one-app", validate.Refuse},
```

and a table test over `listen` values on the `uptime-without-smtp` fixture set to `mode: external`:

```go
func TestIngressListenAddresses(t *testing.T) {
	for _, tc := range []struct {
		listen         string
		refused, warns bool
	}{
		{"127.0.0.1:8480", false, false},
		{"192.168.1.20:8480", false, true},
		{"10.44.0.4:8480", false, true}, // the site's own mesh address
		{"10.44.0.1:8480", true, false}, // another site's mesh address
		{"203.0.113.20:8480", true, false},
		{"0.0.0.0:8480", true, false},
	} {
		cfg := load(t, "uptime-without-smtp")
		site := cfg.Sites["watch"]
		site.Ingress = &config.Ingress{Mode: config.IngressExternal, Listen: tc.listen}
		cfg.Sites["watch"] = site
		result := validate.Check(cfg)
		if result.Has("ingress-listen-public") != tc.refused || result.Has("ingress-listen-bypasses-firewall") != tc.warns {
			t.Errorf("%s: %v", tc.listen, result.Findings)
		}
	}
}
```

`10.44.0.1` is inside the mesh and RFC 1918 alike, so "the site's mesh address" has to be checked before "private": an address on another site's mesh is refused because nothing on this host binds it.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/validate -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
func (c *checker) ingress() {
	for _, name := range c.cfg.SiteNames() {
		site := c.cfg.Sites[name]
		if site.Ingress == nil {
			continue
		}
		key := fmt.Sprintf("sites.%s.ingress", name)
		if !site.Has(config.RoleMonitor) {
			c.refuse("ingress-outside-monitor", key, "...")
			continue
		}
		mode, listen := site.IngressMode(), site.Ingress.Listen
		switch {
		case mode == config.IngressPaisans && listen != "":
			c.refuse("ingress-listen-mode", key+".listen", "...")
			continue
		case mode == config.IngressExternal && listen == "":
			c.refuse("ingress-listen-mode", key+".listen", "...")
			continue
		case mode == config.IngressPaisans:
			continue
		}
		if pinned := c.cfg.PinnedTo(name); len(pinned) > 1 {
			c.refuse("ingress-external-serves-one-app", key+".listen", "...")
		}
		host, _, ok := site.Ingress.ListenHostPort()
		if !ok {
			continue // structural
		}
		ip := net.ParseIP(host)
		switch {
		case ip.IsLoopback():
		case host == site.Address, c.cfg.Mesh.Contains(host) == false && ip.IsPrivate():
			c.warn("ingress-listen-bypasses-firewall", key+".listen", "...")
		default:
			c.refuse("ingress-listen-public", key+".listen", "...")
		}
	}
}
```

- [ ] **Step 4: Run them to see them pass**

Run: `go test ./internal/validate -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/validate
git commit -m "feat: check a monitor's ingress block"
```

---

### Task 4: What a monitor claims on its host

**Files:**
- Modify: `internal/render/ports.go`
- Test: `internal/render/ports_test.go`

**Interfaces:**
- Produces: for a monitor in mode paisans, `Caddy` on tcp `""`:80 and `""`:443, key `sites.<s>.roles (monitor)`; for mode external, each app pinned there on tcp `<listen host>:<listen port>`, owner the app's own owner string, key `sites.<s>.ingress.listen`.

- [ ] **Step 1: Write the failing tests**

```go
func TestAPaisansMonitorClaimsEightyAndFourFortyThree(t *testing.T) {
	cfg := monitorConfig(t, nil)
	want := map[int]bool{80: false, 443: false}
	for _, l := range render.SiteListeners(cfg, "watch") {
		if l.Owner == "Caddy" && l.Address == "" && l.Key == "sites.watch.roles (monitor)" {
			want[l.Port] = true
		}
	}
	if !want[80] || !want[443] {
		t.Fatalf("%v", render.SiteListeners(cfg, "watch"))
	}
}

func TestAnExternalListenIsClaimed(t *testing.T) {
	cfg := monitorConfig(t, &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"})
	var found bool
	for _, l := range render.SiteListeners(cfg, "watch") {
		if l.Port == 80 || l.Port == 443 {
			t.Errorf("an external monitor claims %s", l)
		}
		if l.Address == "127.0.0.1" && l.Port == 8480 && l.Key == "sites.watch.ingress.listen" {
			found = true
		}
	}
	if !found {
		t.Fatal("no claim for listen")
	}
	// On a site that also holds data, a listen on Postgres's loopback port
	// is a collision validate reports.
	site := cfg.Sites["watch"]
	site.Roles = append(site.Roles, config.RoleData)
	site.Ingress.Listen = "127.0.0.1:5432"
	cfg.Sites["watch"] = site
	if !validate.Check(cfg).Has("port-collision") {
		t.Fatal("a listen on 5432 beside Postgres is not a collision")
	}
}
```

`monitorConfig(t, ingress)` (in `monitor_test.go`, Task 5 adds the site to the fixture; until then it adds `watch` to `fixture(t)` itself and pins `status` there).

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/render -run 'Monitor|Listen' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement** in `SiteListeners`, after the gateway's Caddy:

```go
	if s.Has(config.RoleMonitor) && s.IngressMode() == config.IngressPaisans {
		add("Caddy", roles(config.RoleMonitor), "tcp", "", caddyHTTPPort)
		add("Caddy", roles(config.RoleMonitor), "tcp", "", caddyHTTPSPort)
	}
```

and in the app loop:

```go
		if host, port, ok := ExternalListen(cfg, name); ok {
			add(owner, fmt.Sprintf("sites.%s.ingress.listen", site), "tcp", host, port)
		}
```

`ExternalListen(cfg, app) (string, int, bool)` lives in `monitor.go`: the listen of the external monitor the app is pinned to.

- [ ] **Step 4: Run them to see them pass**

Run: `go test ./internal/render ./internal/hostcheck ./internal/validate -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/render
git commit -m "feat: claim a monitor's web ports"
```

---

### Task 5: The monitor serves its own apps

**Files:**
- Create: `internal/render/monitor.go`, `internal/render/monitor_test.go`
- Modify: `internal/render/plan.go`, `site.go`, `templates/infra-compose.yaml.tmpl`
- Modify: `internal/render/testdata/deployment.yaml`, `secrets.fixture.yaml`, `golden/`

**Interfaces:**
- Produces: `func ServedBy(cfg *config.Config, app string) (site string, monitor bool)` (the monitor site the app is pinned to, or `"", false` for the gateway); `siteView.RunsCaddy`; `(p *planner) routesFor(site *siteView) []route`.

- [ ] **Step 1: Move the fixture's monitor onto a `watch` site**

`deployment.yaml` gains

```yaml
  watch:
    roles: [monitor]
    address: 10.44.0.4
    endpoint: watch.example.org:51820
    public_address: 203.0.113.20
    ssh: (as vm's, host watch.example.org)
```

and `status` moves to `placement: { pinned: watch }`. `secrets.fixture.yaml` gains `watch: { wireguard_private_key: "Q0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0M=" }` (32 bytes of `C`).

- [ ] **Step 2: Write the failing tests**

```go
func TestMonitorAppsLeaveTheGatewayCaddyfile(t *testing.T) {
	files := planFiles(build(t))
	gateway := files["vm/srv/infra/caddy/Caddyfile"]
	if strings.Contains(gateway, "status.example.org") {
		t.Fatalf("the gateway routes the monitor:\n%s", gateway)
	}
	if _, ok := files["vm/srv/infra/caddy/snippets/status.caddy"]; ok {
		t.Fatal("the gateway carries the monitor's snippet")
	}
	monitor := files["watch/srv/infra/caddy/Caddyfile"]
	if !strings.Contains(monitor, "status.example.org {") || strings.Contains(monitor, "talk.example.org") {
		t.Fatalf("the monitor's Caddyfile:\n%s", monitor)
	}
	if !strings.Contains(files["watch/srv/infra/caddy/snippets/status.caddy"], "reverse_proxy 10.44.0.4:3001") {
		t.Fatal("the monitor's snippet does not reach the app on its own mesh address")
	}
}

func TestAPaisansMonitorRunsTheGatewaysCaddy(t *testing.T) {
	files := planFiles(build(t))
	image := func(path string) string {
		for _, line := range strings.Split(files[path], "\n") {
			if strings.Contains(line, "caddy") && strings.Contains(line, "image:") {
				return strings.TrimSpace(line)
			}
		}
		return ""
	}
	if image("watch/srv/infra/compose.yaml") == "" || image("watch/srv/infra/compose.yaml") != image("vm/srv/infra/compose.yaml") {
		t.Fatalf("monitor %q, gateway %q", image("watch/srv/infra/compose.yaml"), image("vm/srv/infra/compose.yaml"))
	}
	if !strings.Contains(files["watch/srv/infra/compose.yaml"], "network_mode: host") {
		t.Fatal("the monitor's Caddy is not on the host network")
	}
	if !strings.Contains(files["watch/srv/infra/caddy/Caddyfile"], "acme_dns desec") {
		t.Fatal("the monitor's Caddy does not use DNS-01")
	}
	if files["watch/srv/infra/caddy/caddy.env"] == "" {
		t.Fatal("no token for the monitor's Caddy")
	}
}

func TestAnExternalMonitorRunsNoCaddy(t *testing.T) {
	cfg := monitorConfig(t, &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"})
	files := planFiles(mustBuild(t, cfg))
	for path := range files {
		if strings.HasPrefix(path, "watch/srv/infra/") {
			t.Errorf("an external monitor renders %s", path)
		}
	}
}
```

- [ ] **Step 3: Run them to see them fail**

Run: `go test ./internal/render -run 'Monitor' -count=1`
Expected: FAIL (fixture fails to validate until Tasks 2 to 4 exist; they do).

- [ ] **Step 4: Implement**

`monitor.go`:

```go
// ServedBy is the monitor site that serves an app, when the app is pinned to
// one. Every other app is served by the gateway.
func ServedBy(cfg *config.Config, app string) (string, bool) {
	a, ok := cfg.Apps[app]
	if !ok || a.Placement.Mode != config.PlacementPinned {
		return "", false
	}
	site, ok := cfg.Sites[a.Placement.Site]
	if !ok || !site.Has(config.RoleMonitor) {
		return "", false
	}
	return a.Placement.Site, true
}
```

`routes()` becomes `routesFor(site)`: on a gateway, every app `ServedBy` says is not a monitor's; on a monitor, only the apps pinned to it. `renderSnippets`, `renderGateSnippets` and `gateSnippetMounts` take the route list so a monitor renders only its own (gate snippets only when one of its routes is gated). `renderSite` resolves the Caddy image and renders the Caddyfile, snippets and `caddy.env` on `site.RunsCaddy`, and the infra stack exists when `RunsCaddy` is set. `infra-compose.yaml.tmpl` gates `caddy:` on `.Site.RunsCaddy`.

- [ ] **Step 5: Regenerate the goldens and read the diff**

Run: `go test ./internal/render -update -count=1 && git diff --stat internal/render/testdata/golden && git diff internal/render/testdata/golden/vm/srv/infra/caddy/Caddyfile`
Expected: `status` leaves `vm/`, a new `watch/` tree (wg0.conf, infra compose with caddy, Caddyfile with one block, snippets/status.caddy, caddy.env, srv/status/), and every other site's `wg0.conf` gains the `watch` peer.

- [ ] **Step 6: Run everything that reads the fixture**

Run: `go test ./internal/render ./internal/apply ./cmd/paisans -count=1`
Expected: PASS; any test counting sites or listing the gateway's hostnames is updated to the fourth site, not skipped.

- [ ] **Step 7: Commit**

```bash
git add internal/render
git commit -m "feat: serve a monitor's apps from the monitor, not the gateway"
```

---

### Task 6: The app behind a proxy, the listen publish, and the self check

**Files:**
- Modify: `internal/render/monitor.go`, `appview.go`, `uptime.go`, `templates/uptime/compose.yaml.tmpl`, `templates/uptime/.env.secret.tmpl`
- Test: `internal/render/monitor_test.go`, `uptime_test.go`

**Interfaces:**
- Produces: `appValues.TrustProxy string`, `appValues.IngressListen string` (`host:port`, empty unless mode external); `func trustProxy(cfg *config.Config, app string) string`; `ExternalListen`.

- [ ] **Step 1: Write the failing tests**

```go
func TestTrustProxyFollowsWhereTheProxyConnectsFrom(t *testing.T) {
	for _, tc := range []struct {
		ingress *config.Ingress
		want    string
	}{
		{nil, "TRUST_PROXY=10.44.0.0/24"},
		{&config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"}, "TRUST_PROXY=loopback,uniquelocal"},
		{&config.Ingress{Mode: config.IngressExternal, Listen: "192.168.1.20:8480"}, "TRUST_PROXY=192.168.0.0/16"},
		{&config.Ingress{Mode: config.IngressExternal, Listen: "10.44.0.4:8480"}, "TRUST_PROXY=10.44.0.0/24"},
	} {
		env := planFiles(mustBuild(t, monitorConfig(t, tc.ingress)))["watch/srv/status/.env"]
		if !strings.Contains(env, tc.want+"\n") || !strings.Contains(env, "PUBLIC_BASE_URL=https://status.example.org\n") {
			t.Errorf("%+v:\n%s", tc.ingress, env)
		}
	}
}

func TestAnExternalAppIsPublishedOnListenAsWell(t *testing.T) {
	compose := planFiles(mustBuild(t, monitorConfig(t, &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"})))["watch/srv/status/compose.yaml"]
	for _, want := range []string{`"10.44.0.4:3001:3001"`, `"127.0.0.1:8480:3001"`} {
		if !strings.Contains(compose, want) {
			t.Errorf("missing %s:\n%s", want, compose)
		}
	}
}

func TestTheMonitorChecksItsOwnPublicURL(t *testing.T) {
	monitors := byName(renderSeed(t, ...))
	self := monitors[render.PublicCheckName("status")]
	if self == nil || self["url"] != "https://status.example.org/healthz" || self["expected_status"] != "200" {
		t.Fatalf("self check: %v", self)
	}
	for name := range monitors {
		if strings.HasPrefix(name, render.DirectCheckName("status", "")) {
			t.Errorf("the monitor checks its own container: %s", name)
		}
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/render -run 'TrustProxy|Listen|OwnPublic' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// trustProxy is what the app trusts to report the client's address: the
// network the proxy in front of it connects from.
func trustProxy(cfg *config.Config, app string) string {
	host, _, ok := ExternalListen(cfg, app)
	if !ok {
		return cfg.Mesh.Subnet
	}
	ip := net.ParseIP(host)
	switch {
	case ip.IsLoopback():
		// Docker publishes a loopback port through docker-proxy, or with the
		// userland proxy off through a masqueraded hairpin; either way the
		// container sees its compose network's gateway, a private address,
		// and never 127.0.0.1.
		return "loopback,uniquelocal"
	case cfg.Mesh.Contains(host):
		return cfg.Mesh.Subnet
	}
	for _, block := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if _, n, _ := net.ParseCIDR(block); n.Contains(ip) {
			return block
		}
	}
	return cfg.Mesh.Subnet
}
```

`values()` sets `TrustProxy`, `IngressListen`, and `UpstreamElsewhere = false` for an app `ServedBy` a monitor (its Caddy is on the same machine). The uptime compose template adds `- "{{ .IngressListen }}:{{ .App.Port }}"` under `{{ if .IngressListen }}`; the env template writes `TRUST_PROXY={{ .TrustProxy }}`. The seed appends, for `self` only:

```go
	if health, ok := kinds.HealthFor(config.KindUptime); ok {
		monitors = append(monitors, seedMonitor{
			Name: PublicCheckName(self), MonitorType: "active", Method: "GET", CheckType: "status",
			URL: "https://" + p.cfg.Apps[self].Hostname + health.Path, ExpectedStatus: health.Expect,
			FollowRedirects: &noRedirects,
			IntervalSeconds: seedInterval, TimeoutMS: seedTimeoutMS, FailureThreshold: seedThreshold,
		})
	}
```

`PublicCheckName(app)` and `DirectCheckName(app, site)` move the two existing name formats out of `uptimeSeed` unchanged: the fork reconciles monitors by name, so a changed format would replace every deployed monitor and drop its channel links.

- [ ] **Step 4: Regenerate goldens, read the diff, run the package**

Run: `go test ./internal/render -update -count=1 && git diff internal/render/testdata/golden && go test ./internal/render -count=1`
Expected: only `watch/srv/status/monitors.json` gains the self check; `.env` unchanged in mode paisans.

- [ ] **Step 5: Commit**

```bash
git add internal/render
git commit -m "feat: render the monitor for a proxy in front of it, and check its own url"
```

---

### Task 7: Firewall, ACME token and DNS follow the monitor

**Files:**
- Modify: `internal/hostprep/firewall.go`, `internal/secretsgen/secretsgen.go`, `cmd/paisans/secrets.go`, `internal/dns/dns.go`
- Test: `internal/hostprep/hostprep_test.go`, `internal/secretsgen/secretsgen_test.go`, `cmd/paisans/secrets_test.go`, `internal/dns/dns_test.go`

**Interfaces:**
- Consumes: `Site.RunsCaddy`, `render.ServedBy`.

- [ ] **Step 1: Write the failing tests**

```go
func TestAPaisansMonitorOpensEightyAndFourFortyThree(t *testing.T) {
	site := config.Site{Roles: []config.Role{config.RoleMonitor}}
	if !hasPorts(hostprep.Rules(site), 80, 443) {
		t.Fatal("paisans mode")
	}
	site.Ingress = &config.Ingress{Mode: config.IngressExternal, Listen: "127.0.0.1:8480"}
	if hasPort(hostprep.Rules(site), 80) || hasPort(hostprep.Rules(site), 443) {
		t.Fatal("external mode opens nothing for the web")
	}
}

func TestMonitorAppsPointAtTheMonitor(t *testing.T) {
	cfg := fixtureWithAddresses(t) // vm 203.0.113.10, watch 203.0.113.20 and 2001:db8::20
	wants, err := dns.Desired(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, w := range wants {
		got[w.Type+" "+w.Name] = w.Content
	}
	if got["A status.example.org"] != "203.0.113.20" || got["AAAA status.example.org"] != "2001:db8::20" || got["A talk.example.org"] != "203.0.113.10" {
		t.Fatalf("%v", got)
	}
}
```

plus: `requireACMEToken` refuses `watch` with no token; `init` lists `external.acme_dns_token` owed for a deployment whose only Caddy is a monitor's.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/hostprep ./internal/dns ./internal/secretsgen ./cmd/paisans -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**

`Rules`: `if site.RunsCaddy()` with the reason "the gateway" or "the monitor" by role. `requireACMEToken` and `owed`: any site `RunsCaddy()`. `Desired`: each hostname claim carries its app; `ServedBy` decides between the gateway and the monitor; the gateway problems are raised only when some hostname is the gateway's.

- [ ] **Step 4: Run them to see them pass**

Run: `go test ./internal/hostprep ./internal/dns ./internal/secretsgen ./cmd/paisans -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/hostprep internal/dns internal/secretsgen cmd/paisans
git commit -m "feat: open, certify and publish a monitor's own hostnames"
```

---

### Task 8: `ingress show`

**Files:**
- Create: `internal/ingress/ingress.go`, `show.go`, `show_test.go`

**Interfaces:**
- Produces:

```go
type Target struct {
	App, Site, Hostname string
	Mode                config.IngressMode
	Listen              string // host:port, external only
	ListenHost          string
	ListenPort          int
	PublicAddress       string
	PublicAddress6      string
	HealthPath          string
}
func For(cfg *config.Config, app string) (Target, error)
func Show(w io.Writer, t Target)
```

- [ ] **Step 1: Write the failing tests**

```go
func TestShowHandsOffAnExternalApp(t *testing.T) {
	var buf bytes.Buffer
	ingress.Show(&buf, external("127.0.0.1:8480"))
	out := buf.String()
	for _, want := range []string{
		"status.example.org", "http://127.0.0.1:8480", "/healthz",
		"terminate TLS", "Host", "X-Forwarded-For", "X-Forwarded-Proto",
		"reverse_proxy 127.0.0.1:8480",
		"proxy_pass http://127.0.0.1:8480;",
		"ProxyPass        / http://127.0.0.1:8480/",
		"YOUR CERTIFICATE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ufw") {
		t.Error("a loopback listen needs no firewall warning")
	}
}

func TestShowWarnsWhenListenIsNotLoopback(t *testing.T) {
	var buf bytes.Buffer
	ingress.Show(&buf, external("192.168.1.20:8480"))
	if !strings.Contains(buf.String(), "ufw") {
		t.Fatal(buf.String())
	}
}

func TestShowHasNothingToHandOffInPaisansMode(t *testing.T) {
	var buf bytes.Buffer
	ingress.Show(&buf, paisansTarget())
	if !strings.Contains(buf.String(), "nothing to hand off") || strings.Contains(buf.String(), "proxy_pass") {
		t.Fatal(buf.String())
	}
}

func TestForRefusesAnAppTheGatewayServes(t *testing.T) { /* For(cfg, "talk") errors naming the gateway */ }
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/ingress -count=1`
Expected: FAIL, package does not exist.

- [ ] **Step 3: Implement** `For` from `render.ServedBy`, the site, and `kinds.HealthFor`; `Show` prints the sheet with the three snippets filled from the target, each with a `YOUR CERTIFICATE` marker where the operator's own lines go.

- [ ] **Step 4: Run them to see them pass**

Run: `go test ./internal/ingress -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ingress
git commit -m "feat: print a hand-off sheet for a monitor behind another web server"
```

---

### Task 9: `ingress check`

**Files:**
- Create: `internal/ingress/check.go`, `check_test.go`

**Interfaces:**
- Produces:

```go
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}
type Probes struct {
	Resolver Resolver
	Client   *http.Client // must not follow redirects; Check sets CheckRedirect on a copy
	Dial     func(ctx context.Context, network, address string) (net.Conn, error)
	Now      func() time.Time
}
func DefaultProbes() Probes
type Result struct {
	Name   string
	OK     bool
	Detail string
	Fix    string // empty when OK
}
func Check(ctx context.Context, t Target, p Probes) []Result
func Print(w io.Writer, results []Result) (failed bool)
```

- [ ] **Step 1: Write the failing tests**

A helper starts an `httptest.NewUnstartedServer` with a TLS certificate generated in the test for a given name and lifetime (`crypto/ecdsa`, `x509.CreateCertificate`, self signed, `IsCA` so it can be its own root), and returns `Probes` whose client trusts that certificate and whose transport dials the server whatever the hostname:

```go
func TestCheckPassesAWellConfiguredProxy(t *testing.T)            // 4 results, all OK, "days" in item 2's detail
func TestCheckFailsWhenTheNameDoesNotResolveToTheSite(t *testing.T) // item 1 fails, Fix names the A record and paisans dns init
func TestCheckFailsACertificateForAnotherName(t *testing.T)        // item 2 fails, Detail says the certificate does not cover the hostname
func TestCheckFailsAnHTTPCallback(t *testing.T)                     // item 3 fails on redirect_uri=http://...
func TestCheckFailsWhenSignInIsNotConfigured(t *testing.T)          // /login/oidc -> /login, item 3 fails with its own reason
func TestCheckFailsAnUpstreamReachableAroundTheProxy(t *testing.T)  // Dial succeeds, item 4 fails, Fix names listen and DOCKER-USER
func TestCheckRunsOnlyTheFirstTwoInPaisansMode(t *testing.T)       // 2 results
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/ingress -run Check -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement** each item as its own function returning one `Result`; a probe error is a failed item with its reason, never an aborted run. Item 2 reads `resp.TLS.PeerCertificates[0].NotAfter` for days to expiry, and recognises `x509.HostnameError` to say the certificate covers other names. Item 3 sends no cookies, follows nothing, and reads `redirect_uri` from the `Location` query. Item 4 dials `<public_address>:<listen port>` with a 3 second timeout and passes on any error.

- [ ] **Step 4: Run them to see them pass**

Run: `go test ./internal/ingress -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ingress
git commit -m "feat: check a monitor's web server from the operator's machine"
```

---

### Task 10: The commands

**Files:**
- Create: `cmd/paisans/ingress.go`, `cmd/paisans/ingress_test.go`
- Modify: `cmd/paisans/main.go` (usage, dispatch)

**Interfaces:**
- Consumes: `ingress.For`, `ingress.Show`, `ingress.Check`, `ingress.Print`, `loadChecked`.
- Produces: `var ingressProbes = ingress.DefaultProbes` (tests replace it).

Usage lines:

```
  paisans ingress show  --app <name> [--config paisans.yaml]
  paisans ingress check --app <name> [--config paisans.yaml]
```

- [ ] **Step 1: Write the failing test**: `runIngress([]string{"show", "--app", "status", "--config", fixture})` prints the sheet; `check` with `ingressProbes` replaced by a fake that fails item 1 returns an error; an unknown subcommand is refused.
- [ ] **Step 2: Run it to see it fail**: `go test ./cmd/paisans -run Ingress -count=1`
- [ ] **Step 3: Implement** the dispatch and both subcommands; `check` exits non zero when any item fails.
- [ ] **Step 4: Run it to see it pass**: `go test ./cmd/paisans -count=1`
- [ ] **Step 5: Commit**: `git commit -m "feat: add ingress show and ingress check"`

---

### Task 11: Example, guide, README, development notes, spec amendment

**Files:**
- Modify: `examples/paisans.example.yaml`, `examples/secrets.example.yaml`
- Create: `docs/guides/behind-your-own-web-server.md`
- Modify: `README.md` (*Monitoring*, the role list in *Configuration* where it is listed), `docs/development.md` (*Run*, *Layout*, *Refuse and warn* where roles are named), `docs/specs/2026-10-07-uptime-monitoring.md` (an amendment pointing at the new spec; the "Any declared site" and role-less monitor site paragraphs marked as superseded)

- [ ] **Step 1:** Example gains a `watch` site `roles: [monitor]` with `public_address: 203.0.113.20` and a commented `ingress` block; `status` moves there; secrets example gains `watch`'s key. Run `go test ./internal/config ./internal/secretsgen -count=1` and `go run ./cmd/paisans validate --config examples/paisans.example.yaml` (expect the one existing warning).
- [ ] **Step 2:** Write the guide and README, in the house style: no em dashes, reasons given, no rejected alternatives.
- [ ] **Step 3:** Check every link and every fenced YAML block parses.
- [ ] **Step 4: Commit**: `git commit -m "docs: describe the monitor role and its ingress"`

---

### Task 12: Verify

- [ ] `gofmt -l .` prints nothing.
- [ ] `go vet ./...` is clean.
- [ ] `go test -count=1 ./...` passes.
- [ ] `go build -o /dev/null ./cmd/paisans`.
- [ ] `git diff feat/host-check --stat` read through; grep the diff for an em dash in new prose and for any address outside the fixture ranges.

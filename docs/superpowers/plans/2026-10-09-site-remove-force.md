# `site remove --force` and orphaned secrets: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- `paisans site remove <site> --force [--ssh user@host[:port]]` cleans one host of this deployment, ignoring the cluster and leaving both files alone.
- Cleaning also removes images only this deployment's containers ran.
- `init` and `apply` warn about secrets naming things `paisans.yaml` no longer declares, and `paisans secrets prune` removes them.

**Architecture:**
- **Stage 3 reused:** `internal/siteremove`'s stage 3 (`buildHost`, `runHost`, `verifyHost`) is reused as it is.
- **The forced plan:** a new `BuildForced` builds a plan of that one stage. It runs against a copy of the configuration in which the site's roles come from the host's registry entry and its ssh section from the chosen destination.
- **Orphans:** they are computed by `secretsgen.Orphans`, which sits beside the code that knows which secrets a configuration needs.

**Tech Stack:** Go, the existing fake `world` in `internal/siteremove/world_test.go`, sops-encrypted secrets via `config.WriteSecrets`.

**Spec:** `docs/specs/2026-10-09-site-remove-force.md`. Its parent spec, for stage 3, is `docs/specs/2026-10-08-site-remove.md`.

## Global Constraints

- **Public repo:** use no real hostnames, IPs or deployment facts. Use 203.0.113.0/24, 198.51.100.0/24 and 192.0.2.0/24, and site names from the fixtures (`home-a`, `home-b`, `vm`, `watch`).
- **No rejected alternatives:** docs, comments, commits and PRs never describe them.
- **No migration code.**
- **`--force` never edits files:** neither `paisans.yaml` nor `secrets.enc.yaml`.
- **`--force --ssh` accepts any host:** nothing on it is touched unless it carries this deployment's id or token.
- **Confirmation on the site's own host:** `--force` on the declared site's own host asks for the site's name at a terminal at `--execute`. No flag answers it.
- **Images:** removed by ID with `docker image rm` and without `-f`. A Docker refusal is reported as kept, never as a failure.
- **Orphaned secrets:** reported by key, never by value, as a warning and never a refusal. Only `secrets prune --execute` removes them.
- **Commits** end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- **After every task**, `go test ./...` and `go vet ./...` pass.

## Review Focus

1. **`--force` against a host whose registry has no entry for this deployment** (never deployed, or already cleaned). Expected: an empty plan and a clean exit. Pinned in Task 4, `TestForcedOnACleanHostPlansNothing`.
2. **A forced, undeclared gateway site whose host serves the owner's `/srv/caddy.d` sites.** Expected: the Caddy hand-over runs, because roles come from the registry. Pinned in Task 4, `TestForcedUndeclaredGatewayHandsOverCaddy`.
3. **An image shared between one of our containers and a foreign container.** Expected: kept, with a reason. Pinned in Task 3, `TestImagesSharedWithAForeignContainerStay`.
4. **`--ssh` without a port, or with an ssh alias.** Expected: `user@host` gets `:22`. A bare alias with no `@` is refused with the expected form. Pinned in Task 1, `TestParseDestination`.
5. **A Pocket ID group still named by a second Pocket ID app.** Expected: not an orphan. Pinned in Task 5, `TestOrphans`.

---

### Task 1: `config.Destination`

**Files:**
- Modify: `internal/config/ssh.go` (add `Destination`, `ParseDestination`, `Site.Destination`)
- Test: `internal/config/destination_test.go`

**Interfaces:**
- Produces:
  - `type Destination struct { User, Host string; Port int }`
  - `func ParseDestination(s string) (Destination, error)`
  - `func (d Destination) String() string`
  - `func (s Site) Destination() Destination`

- [ ] **Step 1: Write the failing tests**

```go
package config_test

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

func TestParseDestination(t *testing.T) {
	for in, want := range map[string]string{
		"admin@203.0.113.9":      "admin@203.0.113.9:22",
		"admin@203.0.113.9:2222": "admin@203.0.113.9:2222",
		"admin@[2001:db8::1]:22": "admin@[2001:db8::1]:22",
		"admin@host.example.org": "admin@host.example.org:22",
	} {
		d, err := config.ParseDestination(in)
		if err != nil || d.String() != want {
			t.Errorf("%s: got %q, %v; want %q", in, d.String(), err, want)
		}
	}
	for _, bad := range []string{"203.0.113.9", "myalias", "admin@", "admin@h:0", "admin@h:x"} {
		if _, err := config.ParseDestination(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestSiteDestinationUsesTheSSHSection(t *testing.T) {
	s := config.Site{SSH: config.SSH{User: "admin", Host: "203.0.113.9"}}
	if got := s.Destination().String(); got != "admin@203.0.113.9:22" {
		t.Errorf("got %q", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/config/ -run Destination -v`
Expected: FAIL to compile, with `undefined: config.ParseDestination`.

- [ ] **Step 3: Implement**

Append to `internal/config/ssh.go`, and add `"strconv"` to its imports:

```go
// Destination is where a host is reached: user, host and port. Its string
// form is user@host:port, with the port always written.
type Destination struct {
	User string
	Host string
	Port int
}

func (d Destination) String() string {
	return d.User + "@" + net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
}

// ParseDestination reads user@host or user@host:port, with an IPv6 host in
// brackets. The user is required, because it is whose authorized keys a
// cleaning reads, so an ssh alias is refused.
func ParseDestination(s string) (Destination, error) {
	user, rest, ok := strings.Cut(s, "@")
	if !ok || user == "" || rest == "" {
		return Destination{}, fmt.Errorf("%q is not user@host[:port], Eg: admin@203.0.113.9 or admin@203.0.113.9:2222", s)
	}
	host, port := rest, DefaultSSHPort
	if h, p, err := net.SplitHostPort(rest); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Destination{}, fmt.Errorf("%q: the port must be a number from 1 to 65535", s)
		}
		host, port = h, n
	} else if strings.HasPrefix(rest, "[") || strings.Count(rest, ":") == 1 {
		return Destination{}, fmt.Errorf("%q is not user@host[:port]: %v", s, err)
	}
	return Destination{User: user, Host: host, Port: port}, nil
}

// Destination is where the site's ssh section reaches it.
func (s Site) Destination() Destination {
	return Destination{User: s.SSH.User, Host: s.SSHHost(), Port: s.SSH.PortOrDefault()}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/config/ ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat: parse a user@host[:port] destination

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: the image each container runs, in the host inventory

**Files:**
- Modify: `internal/hostcheck/inventory.go:79` (`containerProbe`)
- Modify: `internal/hostcheck/parse.go:105-170` (`Container.Image`, `inspectedContainer.Image`)
- Test: `internal/hostcheck/parse_test.go`, or whichever test file covers `parseContainers` (`grep -n parseContainers internal/hostcheck/*_test.go`)

**Interfaces:**
- Produces: `hostcheck.Container.Image string`, the image ID (`sha256:…`) from `docker inspect`'s `.Image`

- [ ] **Step 1: Write the failing test**

Add it beside the existing `parseContainers` test, in the same package that test uses:

```go
func TestContainerCarriesItsImageID(t *testing.T) {
	out := `{"id":"abc","name":"/paisans-f2a9-talk-app-1","image":"sha256:1111","pid":0,"labels":{},"networks":{},"ports":{}}` + "\n"
	list, err := parseContainers(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Image != "sha256:1111" {
		t.Fatalf("got %+v", list)
	}
}
```

If the existing tests are in `package hostcheck_test`, call the exported wrapper they already use. Find it with `grep -n "func.*Parse.*Containers\|parseContainers" internal/hostcheck/*.go`.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/hostcheck/ -run TestContainerCarriesItsImageID -v`
Expected: FAIL, `list[0].Image undefined`.

- [ ] **Step 3: Implement**

In `Container`, after `Deployment`:

```go
	// Image is the ID of the image it was created from, sha256:..., which
	// cleaning a host removes once no container on it uses the image.
	Image string
```

In `inspectedContainer`, add `Image string \`json:"image"\``. In `parseContainers`, set `Image: in.Image` in the `Container{...}` literal. In `containerProbe`, add `"image":{{json .Image}},` after `"name":{{json .Name}},`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/hostcheck/ ./... `
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/hostcheck
git commit -m "feat: read the image each container was created from

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: cleaning a host removes the images only our containers used

**Files:**
- Modify: `internal/siteremove/host.go`: `hostPlan.images`; `buildHost`, `hostSteps` and `runHost`
- Modify: `internal/siteremove/remains.go` (`imageKept`)
- Modify: `internal/siteremove/export_test.go` (`WorstCaseRemains` gains `imageKept`)
- Modify: `internal/siteremove/world_test.go`: images on fixture containers; a `docker image rm` case in `host.Run`; `host.images`
- Test: `internal/siteremove/images_test.go`

**Interfaces:**
- Consumes: `hostcheck.Container.Image` (Task 2)
- Produces: stage 3 gains a step with verb `remove` and title `remove images`. Its text is `images <id>, <id>`. `Plan.Kept` gains `imageKept` lines.

- [ ] **Step 1: Give the fake world images**

In `world_test.go`:
- Add `images map[string]bool` to `host` (image IDs present on the host). In `newWorld`, give every host `images: map[string]bool{"sha256:etcd": true}`, and give the shared etcd container `Image: "sha256:etcd"`.
- On `home-b`:
  - set `Image: "sha256:talk"` on `paisans-f2a9-talk-app-1`, and `Image: "sha256:theirs"` on `someone-elses-db`;
  - add `"sha256:talk"` and `"sha256:theirs"` to `b.images`.

Add this case to `host.Run`, before the `default`:

```go
	case strings.HasPrefix(command, "for i in ") && strings.Contains(command, "docker image rm"):
		list, _, _ := strings.Cut(strings.TrimPrefix(command, "for i in "), "; do")
		var b strings.Builder
		for _, q := range strings.Fields(list) {
			id := strings.Trim(q, "'")
			used := false
			for _, c := range h.containers {
				used = used || c.Image == id
			}
			switch {
			case used:
				fmt.Fprintf(&b, "kept %s Error response from daemon: conflict: unable to delete (image is being used by running container)\n", id)
			default:
				delete(h.images, id)
				fmt.Fprintf(&b, "removed %s\n", id)
			}
		}
		return b.String(), nil
```

- [ ] **Step 2: Write the failing tests**

```go
package siteremove_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/hostcheck"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

// An image only this deployment's containers ran goes with them.
func TestCleaningRemovesImagesOnlyOurContainersUsed(t *testing.T) {
	w := setup(t)
	p := w.mustBuild("home-b", siteremove.Options{})
	if !hasStep(p, 3, "home-b", "remove", "sha256:talk") {
		t.Fatalf("no image removal planned:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	b := w.hosts["home-b"]
	if b.images["sha256:talk"] {
		t.Error("sha256:talk is still on the host")
	}
	if !b.images["sha256:theirs"] {
		t.Error("the owner's image was removed")
	}
}

// An image a container this deployment does not own also runs from stays.
func TestImagesSharedWithAForeignContainerStay(t *testing.T) {
	w := setup(t)
	b := w.hosts["home-b"]
	b.containers = append(b.containers, hostcheck.Container{Name: "their-talk", Project: "theirs", Image: "sha256:talk"})
	p := w.mustBuild("home-b", siteremove.Options{})
	if hasStep(p, 3, "home-b", "remove", "sha256:talk") {
		t.Fatalf("a shared image is planned for removal:\n%s", printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if !b.images["sha256:talk"] {
		t.Error("a shared image was removed")
	}
	if !strings.Contains(strings.Join(p.Remains(), "\n"), "image kept") {
		t.Errorf("the report does not say why:\n%s", strings.Join(p.Remains(), "\n"))
	}
}

// Docker refusing an image (a container started in between) is a kept image,
// not a failed removal.
func TestDockerRefusingAnImageIsReportedNotFatal(t *testing.T) {
	w := setup(t)
	p := w.mustBuild("home-b", siteremove.Options{})
	b := w.hosts["home-b"]
	b.containers = append(b.containers, hostcheck.Container{Name: "late", Project: "theirs", Image: "sha256:talk"})
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Remains(), "\n"), "conflict") {
		t.Errorf("Docker's reason is not reported:\n%s", strings.Join(p.Remains(), "\n"))
	}
}
```

The fake's `docker ps -aq` case removes `late` along with ours only if it carries our label. It doesn't, so it survives and the image is in use.

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/siteremove/ -run 'Image' -v`
Expected: FAIL, with no image removal planned.

- [ ] **Step 4: Implement**

In `hostPlan`, add `images []string` (IDs, sorted).

In `buildHost`, after the containers loop:

```go
	ours, theirs := map[string]bool{}, map[string]bool{}
	for _, c := range inv.Containers {
		if c.Image == "" {
			continue
		}
		if c.Deployment == d.ID {
			ours[c.Image] = true
		} else {
			theirs[c.Image] = true
		}
	}
	for id := range ours {
		if theirs[id] {
			p.Kept = append(p.Kept, imageKept(p.Site, id, "a container this deployment does not own runs from it"))
			continue
		}
		hp.images = append(hp.images, id)
	}
	sort.Strings(hp.images)
```

In `hostSteps`, right after the containers-and-networks step:

```go
	if len(hp.images) > 0 {
		add("remove", "remove images", "images %s, which only this deployment's containers ran, by ID and without -f, so Docker refuses one in use", strings.Join(hp.images, ", "))
	}
```

Add `imagesCommand` beside `volumesCommand`:

```go
// imagesCommand removes each image by ID, without -f, and answers per image,
// so an image Docker refuses (one a container still runs from) is reported
// rather than failing the stage.
func imagesCommand(ids []string) string {
	var q []string
	for _, id := range ids {
		q = append(q, quote(id))
	}
	return fmt.Sprintf(`for i in %s; do if out=$(docker image rm "$i" 2>&1); then echo "removed $i"; else echo "kept $i $(printf %%s "$out" | tr '\n' ' ')"; fi; done`, strings.Join(q, " "))
}
```

In `runHost`, inside `if hp.dockerPresent`, after the containers command (and after volumes, when `DeleteData`):

```go
		if len(hp.images) > 0 {
			p.work("remove images")
			out, err := t.Run(imagesCommand(hp.images))
			if err != nil {
				return fmt.Errorf("%s: removing images: %w: %s", p.Site, err, lastLines(out, 3))
			}
			for _, line := range strings.Split(out, "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "kept "); ok {
					id, why, _ := strings.Cut(rest, " ")
					p.Kept = append(p.Kept, imageKept(p.Site, id, "Docker refused: "+why))
				}
			}
		}
```

In `remains.go`:

```go
func imageKept(site, id, why string) string {
	return fmt.Sprintf("%s: image kept; remove it yourself once nothing runs from it. It is %s: %s", site, id, why)
}
```

Add `imageKept(site, "sha256:"+strings.Repeat("f", 64), strings.Repeat("w", 80))` to `WorstCaseRemains`.

`verifyHost` is unchanged. An image kept is not this deployment's leftover in the gate's sense.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/siteremove/ -v -run 'Image|Remains' && go test ./...`
Expected: PASS. If a line-width test over `WorstCaseRemains` fails, shorten the `imageKept` hint (the text before the first `. `) until it passes.

- [ ] **Step 6: Commit**

```bash
git add internal/siteremove
git commit -m "feat: site remove deletes the images only this deployment's containers ran

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: `siteremove.BuildForced`

**Files:**
- Create: `internal/siteremove/force.go`
- Modify: `internal/siteremove/host.go`: `buildHost` calls `ownership.Classify` only for a gateway
- Modify: `internal/siteremove/siteremove.go` (`Plan.Current`)
- Test: `internal/siteremove/force_test.go`; `hasStepIn` and `stageNamed` helpers in `siteremove_test.go`

**Interfaces:**
- Consumes: `config.Destination`, `Site.Destination` (Task 1); `buildHost`, `runHost` and `verifyHost` (existing).
- Produces:
  - `func BuildForced(cfg *config.Config, secrets *config.Secrets, site string, dest config.Destination, t apply.Transport, o Options) (*Plan, error)`
  - `Plan.Current bool`: true when `dest` is the declared site's own host
  - The plan's `Stages` is exactly `[clean the host]`, with `Number` 3.

- [ ] **Step 1: Write the failing tests**

Add to `siteremove_test.go`:

```go
func stageNamed(p *siteremove.Plan, name string) *siteremove.Stage {
	for _, st := range p.Stages {
		if st.Name == name {
			return st
		}
	}
	return nil
}

func hasStepIn(st *siteremove.Stage, site, verb, text string) bool {
	if st == nil {
		return false
	}
	for _, s := range st.Steps {
		if s.Site == site && s.Verb == verb && strings.Contains(s.Text, text) {
			return true
		}
	}
	return false
}
```

Create `force_test.go`:

```go
package siteremove_test

import (
	"os"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

func forced(t *testing.T, w *world, cfg *config.Config, site string, dest config.Destination, o siteremove.Options) *siteremove.Plan {
	t.Helper()
	p, err := siteremove.BuildForced(cfg, w.secrets, site, dest, w.hosts[site], o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// None of the cluster's refusals stop --force: home-a has apps pinned to it
// and etcd is unhealthy. Only home-a is reached, and paisans.yaml is not
// touched.
func TestForcedIgnoresTheClustersRefusals(t *testing.T) {
	w := setup(t)
	w.etcdDown["home-c"] = true
	before, err := os.ReadFile(w.configPath)
	if err != nil {
		t.Fatal(err)
	}
	p := forced(t, w, w.cfg, "home-a", w.cfg.Sites["home-a"].Destination(), siteremove.Options{})
	if len(p.Stages) != 1 || stageNamed(p, "clean the host") == nil {
		t.Fatalf("stages:\n%s", printed(p))
	}
	if !p.Current {
		t.Error("home-a's declared host is not marked as its own")
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	for _, h := range w.hosts {
		if h.name != "home-a" && len(h.commands) > 0 {
			t.Errorf("%s was reached: %v", h.name, h.commands)
		}
	}
	after, _ := os.ReadFile(w.configPath)
	if string(after) != string(before) {
		t.Error("--force edited paisans.yaml")
	}
}

// The host stage of a forced plan is the unforced plan's host stage.
func TestForcedHostStageMatchesTheFullRemoval(t *testing.T) {
	w := setup(t)
	full := w.mustBuild("home-b", siteremove.Options{})
	f := forced(t, w, w.cfg, "home-b", w.cfg.Sites["home-b"].Destination(), siteremove.Options{})
	var a []siteremove.Step
	for _, s := range stageNamed(full, "clean the host").Steps {
		if s.Verb != "note" {
			a = append(a, s)
		}
	}
	var b []siteremove.Step
	for _, s := range stageNamed(f, "clean the host").Steps {
		if s.Verb != "note" {
			b = append(b, s)
		}
	}
	if len(a) != len(b) {
		t.Fatalf("full:\n%s\nforced:\n%s", printed(full), printed(f))
	}
	for i := range a {
		if a[i].Text != b[i].Text {
			t.Errorf("step %d: %q vs %q", i, a[i].Text, b[i].Text)
		}
	}
}

// A site no longer declared is cleaned through any destination, its roles
// read from the registry entry on the host.
func TestForcedUndeclaredGatewayHandsOverCaddy(t *testing.T) {
	w := setup(t)
	vm := w.hosts["vm"]
	vm.files[render.HostSitesDir+"/blog.caddy"] = "blog.example.org { respond 200 }\n"
	dest, _ := config.ParseDestination("ubuntu@192.0.2.10")
	p := forced(t, w, w.cfg.WithoutSite("vm"), "vm", dest, siteremove.Options{})
	if p.Current {
		t.Error("an undeclared site's host is marked as its own")
	}
	if !hasStepIn(stageNamed(p, "clean the host"), "vm", "hand over", "Caddy") {
		t.Fatalf("no hand over:\n%s", printed(p))
	}
}

// A host with nothing of this deployment's on it plans nothing.
func TestForcedOnACleanHostPlansNothing(t *testing.T) {
	w := setup(t)
	b := w.hosts["home-b"]
	b.containers, b.networks, b.volumes, b.rules = nil, nil, nil, nil
	b.files = map[string]string{registry.Path: encode(t, registry.Registry{Version: registry.Version, Deployments: map[string]registry.Entry{}})}
	b.wgUp = false
	dest, _ := config.ParseDestination("ubuntu@192.0.2.11")
	p := forced(t, w, w.cfg, "home-b", dest, siteremove.Options{})
	if n := len(stageNamed(p, "clean the host").Steps); n != 0 {
		t.Fatalf("%d step(s) planned:\n%s", n, printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
}

// The plan for the declared site's own host says what the cluster loses.
func TestForcedOwnHostSaysWhatTheClusterLoses(t *testing.T) {
	w := setup(t)
	p := forced(t, w, w.cfg, "home-b", w.cfg.Sites["home-b"].Destination(), siteremove.Options{})
	if !strings.Contains(printed(p), "still declared") {
		t.Errorf("no warning:\n%s", printed(p))
	}
}

func TestForcedRefusesHostGone(t *testing.T) {
	w := setup(t)
	_, err := siteremove.BuildForced(w.cfg, w.secrets, "home-b", w.cfg.Sites["home-b"].Destination(), nil, siteremove.Options{HostGone: true})
	if err == nil || !strings.Contains(err.Error(), "Drop one of them") {
		t.Errorf("err = %v", err)
	}
}
```

`TestForcedHostStageMatchesTheFullRemoval` filters out `note` steps: the full plan carries a Pocket ID note, and the forced plan carries the cluster note.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/siteremove/ -run Forced -v`
Expected: FAIL to compile, with `undefined: siteremove.BuildForced`.

- [ ] **Step 3: Make `buildHost` independent of declared roles for non-gateways**

In `buildHost`, replace:

```go
	report, err := ownership.Classify(p.cfg, p.Site, inv, inv.ManifestFiles)
	if err != nil {
		return nil, err
	}
	if p.cfg.Sites[p.Site].Has(config.RoleGateway) && report.Foreign() {
		if hp.handover, err = p.probeHandover(t, inv, report); err != nil {
			return nil, err
		}
	}
```

with:

```go
	if p.cfg.Sites[p.Site].Has(config.RoleGateway) {
		report, err := ownership.Classify(p.cfg, p.Site, inv, inv.ManifestFiles)
		if err != nil {
			return nil, err
		}
		if report.Foreign() {
			if hp.handover, err = p.probeHandover(t, inv, report); err != nil {
				return nil, err
			}
		}
	}
```

- [ ] **Step 4: Implement**

Add to `Plan` in `siteremove.go`, after `Kept`:

```go
	// Current is set by BuildForced when the host is the declared site's
	// own, which its cluster still counts on: the command asks for the
	// site's name before cleaning it.
	Current bool
```

Create `force.go`:

```go
package siteremove

import (
	"errors"
	"fmt"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/registry"
)

// BuildForced plans `site remove --force`: the host at dest cleaned of this
// deployment, and nothing else. No other site is reached, the cluster's
// refusals are not checked, and neither paisans.yaml nor the secrets file is
// edited, because --force is for a host the cluster cannot be asked about or
// no longer counts on (docs/specs/2026-10-09-site-remove-force.md).
//
// dest may be any host. What is removed is only what the host proves is this
// deployment's, by its id or its token, so a host it never used plans
// nothing.
//
// The host stage runs against a copy of the configuration in which the site
// is declared with dest as its ssh section and the roles its registry entry on
// the host records. Those are what the host was deployed with, which is what
// cleaning it needs, for an undeclared site and for a host the site has left
// alike.
func BuildForced(cfg *config.Config, secrets *config.Secrets, site string, dest config.Destination, t apply.Transport, o Options) (*Plan, error) {
	if o.HostGone {
		return nil, fmt.Errorf("site remove %s: --force cleans one host, and --host-gone reaches none. Drop one of them", site)
	}
	declared, isDeclared := cfg.Sites[site]
	p := &Plan{Site: site, Options: o, secrets: secrets, transports: map[string]apply.Transport{site: t}}
	p.Current = isDeclared && declared.Destination() == dest

	if out, err := t.Run("true"); err != nil {
		if errors.Is(err, apply.ErrUnreachable) {
			return nil, fmt.Errorf("site remove %s: %s does not answer over ssh (%v), so what is on it cannot be read or cleaned. Fix ssh and run again", site, dest, err)
		}
		return nil, fmt.Errorf("site remove %s: %s answered `true` with an error: %v: %s", site, dest, err, lastLines(out, 2))
	}
	s := config.Site{SSH: config.SSH{User: dest.User, Host: dest.Host, Port: dest.Port}}
	if isDeclared {
		s.Roles = declared.Roles
	}
	reg, err := registry.Read(t)
	if err != nil {
		return nil, fmt.Errorf("site remove %s: %w", site, err)
	}
	if e, ok := reg.Deployments[cfg.ID]; ok {
		s.Roles = nil
		for _, r := range strings.Split(e.Roles, ",") {
			if r != "" {
				s.Roles = append(s.Roles, config.Role(r))
			}
		}
	}
	forcedCfg := *cfg
	forcedCfg.Sites = map[string]config.Site{}
	for name, v := range cfg.Sites {
		forcedCfg.Sites[name] = v
	}
	forcedCfg.Sites[site] = s
	p.cfg = &forcedCfg

	host, err := p.buildHost()
	if err != nil {
		return nil, err
	}
	if p.Current && len(host.Steps) > 0 {
		host.Steps = append([]Step{{Site: site, Verb: "note", Title: "note the cluster", Text: fmt.Sprintf("%s is still declared and this is its host: its etcd member, Patroni replica and Garage node, and its place in every other site's mesh, stay in the cluster until a full `paisans site remove %s`, and its next `paisans apply --site %s` deploys it again", site, site, site)}}, host.Steps...)
	}
	p.Stages = []*Stage{host}
	return p, nil
}
```

`Show`, `Remains` and `Execute` iterate `p.Stages`. Check whether `Show` or `Remains` read `p.end`, `p.full`, `p.rendered` or `p.witness` (`grep -n "p.end\|p.full\|p.rendered" internal/siteremove/siteremove.go`). Guard any such line with `if p.end != nil`.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/siteremove/ -v -run Forced && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/siteremove
git commit -m "feat: site remove --force cleans one host and nothing else

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: `secretsgen.Orphans` and `secretsgen.Prune`

**Files:**
- Create: `internal/secretsgen/orphans.go`
- Test: `internal/secretsgen/orphans_test.go`

**Interfaces:**
- Produces:
  - `type Orphan struct { Key string; Why string; Leaves string }`. `Key` is a dotted key such as `sites.monitor-a`. `Leaves` is non-empty when pruning leaves something elsewhere: the Pocket ID client.
  - `func Orphans(cfg *config.Config, secrets *config.Secrets) []Orphan`, sorted by `Key`
  - `func Prune(secrets *config.Secrets, orphans []Orphan)`

- [ ] **Step 1: Write the failing test**

```go
package secretsgen_test

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/secretsgen"
)

func TestOrphans(t *testing.T) {
	cfg := &config.Config{
		Sites: map[string]config.Site{"home-a": {}},
		Apps: map[string]config.App{
			"talk": {Kind: config.KindMbin},
			"auth": {Kind: config.KindPocketID, Settings: map[string]any{"signup_default_groups": []any{"provisional"}}},
			"auth2": {Kind: config.KindPocketID, Settings: map[string]any{"signup_default_groups": []any{"members"}}},
		},
	}
	s := &config.Secrets{
		Sites:          map[string]config.SiteSecrets{"home-a": {}, "monitor-a": {WireGuardPrivateKey: "x"}},
		Apps:           map[string]map[string]any{"talk": {}, "uptime": {"database_password": "x"}},
		OIDCClients:    map[string]config.OIDCClient{"talk": {}, "uptime": {ClientID: "c"}},
		PocketIDGroups: map[string]string{"provisional": "1", "members": "2", "old": "3"},
	}
	var keys []string
	leaves := 0
	for _, o := range secretsgen.Orphans(cfg, s) {
		keys = append(keys, o.Key)
		if o.Leaves != "" {
			leaves++
		}
	}
	want := []string{"apps.uptime", "oidc_clients.uptime", "pocket_id_groups.old", "sites.monitor-a"}
	if len(keys) != len(want) {
		t.Fatalf("got %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("got %v, want %v", keys, want)
		}
	}
	if leaves != 1 {
		t.Errorf("%d orphan(s) say they leave something, want only the OIDC client", leaves)
	}

	secretsgen.Prune(s, secretsgen.Orphans(cfg, s))
	if len(secretsgen.Orphans(cfg, s)) != 0 {
		t.Error("orphans left after Prune")
	}
	if _, ok := s.Sites["home-a"]; !ok {
		t.Error("Prune removed a declared site's secrets")
	}
	if s.PocketIDGroups["members"] != "2" {
		t.Error("Prune removed a group the second Pocket ID app names")
	}
}
```

Check the actual names of `config.App`, `config.KindMbin` and `config.KindPocketID` with `grep -n "Kind[A-Z][a-z]* *Kind\|type App struct" internal/config/*.go`, and adjust the literal.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/secretsgen/ -run TestOrphans -v`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

```go
package secretsgen

import (
	"sort"
	"strings"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// Orphan is a secret naming something the configuration no longer declares:
// a site or an app taken out of paisans.yaml, or a Pocket ID group no
// signup_default_groups names. Nothing reads it any more. It is reported by
// key and never by value, and only `paisans secrets prune --execute` removes
// it.
type Orphan struct {
	Key string
	// Why is what it names that is gone, for the report.
	Why string
	// Leaves is what removing it leaves elsewhere, empty when nothing.
	Leaves string
}

// Orphans is every orphaned secret, sorted by key.
func Orphans(cfg *config.Config, secrets *config.Secrets) []Orphan {
	var out []Orphan
	for name := range secrets.Sites {
		if _, ok := cfg.Sites[name]; !ok {
			out = append(out, Orphan{Key: "sites." + name, Why: "names a site paisans.yaml does not declare"})
		}
	}
	for name := range secrets.Apps {
		if _, ok := cfg.Apps[name]; !ok {
			out = append(out, Orphan{Key: "apps." + name, Why: "names an app paisans.yaml does not declare"})
		}
	}
	for name, client := range secrets.OIDCClients {
		if _, ok := cfg.Apps[name]; !ok {
			out = append(out, Orphan{Key: "oidc_clients." + name, Why: "names an app paisans.yaml does not declare",
				Leaves: "client " + client.ClientID + " still exists at Pocket ID, if that Pocket ID still runs; delete it there"})
		}
	}
	named := map[string]bool{}
	for _, name := range cfg.AppNames() {
		if cfg.Apps[name].Kind == config.KindPocketID {
			for _, g := range kinds.PocketIDSignupGroups(cfg.Apps[name].Settings) {
				named[g] = true
			}
		}
	}
	for group := range secrets.PocketIDGroups {
		if !named[group] {
			out = append(out, Orphan{Key: "pocket_id_groups." + group, Why: "names a group no signup_default_groups names"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Prune removes each orphan from secrets. The caller writes the file.
func Prune(secrets *config.Secrets, orphans []Orphan) {
	for _, o := range orphans {
		kind, name, _ := strings.Cut(o.Key, ".")
		switch kind {
		case "sites":
			delete(secrets.Sites, name)
		case "apps":
			delete(secrets.Apps, name)
		case "oidc_clients":
			delete(secrets.OIDCClients, name)
		case "pocket_id_groups":
			delete(secrets.PocketIDGroups, name)
		}
	}
}
```

If `cfg.AppNames()` does not exist, range over `cfg.Apps`. A site name can't contain a dot (the config refuses one), so `strings.Cut` is safe; check with `grep -n "siteName\|nameRe" internal/validate/*.go | head`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/secretsgen/ ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/secretsgen
git commit -m "feat: find secrets naming what paisans.yaml no longer declares

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: `paisans secrets prune`, and the warning in `init` and `apply`

**Files:**
- Modify: `cmd/paisans/secrets.go`: dispatch `prune`; `runSecretsPrune`; `warnOrphans`
- Modify: `cmd/paisans/main.go`: `runInit` and `runApply` call `warnOrphans` after loading secrets
- Modify: `internal/siteremove/remains.go` (`secretsLeft`)
- Modify: the usage text in `cmd/paisans/main.go` (`grep -n "secrets set" cmd/paisans/main.go`)
- Test: `cmd/paisans/secrets_test.go`

**Interfaces:**
- Consumes: `secretsgen.Orphans`, `secretsgen.Prune` (Task 5).
- Produces:
  - `func runSecretsPrune(args []string) error`
  - `func warnOrphans(r ui.Reporter, cfg *config.Config, secrets *config.Secrets)`

- [ ] **Step 1: Write the failing tests**

Look first at how `secrets_test.go` builds a plaintext secrets file beside `fixtureConfig()`, and reuse that helper. The test below assumes one named `writeFixtureSecrets(t) (dir, configPath, secretsPath string)`. If none exists, write it: copy `fixtureConfig()` into `t.TempDir()`, then write a plaintext secrets file with `config.WriteSecrets(path, s, nil)`.

```go
func TestSecretsPruneListsThenRemovesOnlyOrphans(t *testing.T) {
	configPath, secretsPath := writeFixtureSecrets(t, func(s *config.Secrets) {
		if s.Sites == nil {
			s.Sites = map[string]config.SiteSecrets{}
		}
		s.Sites["monitor-a"] = config.SiteSecrets{WireGuardPrivateKey: "x"}
		if s.OIDCClients == nil {
			s.OIDCClients = map[string]config.OIDCClient{}
		}
		s.OIDCClients["uptime"] = config.OIDCClient{ClientID: "abc", ClientSecret: "do-not-print"}
	})
	out := captureStdout(t, func() {
		if err := runSecretsPrune([]string{"--config", configPath, "--secrets", secretsPath}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"sites.monitor-a", "oidc_clients.uptime", "still exists at Pocket ID"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "do-not-print") {
		t.Fatal("a secret value was printed")
	}
	if s, _ := config.LoadSecrets(secretsPath); s.Sites["monitor-a"].WireGuardPrivateKey == "" {
		t.Fatal("the dry run changed the file")
	}
	if err := runSecretsPrune([]string{"--config", configPath, "--secrets", secretsPath, "--execute"}); err != nil {
		t.Fatal(err)
	}
	s, err := config.LoadSecrets(secretsPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Sites["monitor-a"]; ok {
		t.Error("sites.monitor-a is still there")
	}
	if _, ok := s.OIDCClients["uptime"]; ok {
		t.Error("oidc_clients.uptime is still there")
	}
	if _, ok := s.Sites["home-a"]; !ok {
		t.Error("a declared site's secrets were pruned")
	}
}
```

`home-a` must carry secrets in the fixture; if it does not, use any site the fixture secrets file holds under `sites`.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cmd/paisans/ -run SecretsPrune -v`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

Dispatch in `runSecrets`:

```go
func runSecrets(args []string) error {
	switch {
	case len(args) >= 1 && args[0] == "set":
		return runSecretsSet(args[1:], os.Stdin)
	case len(args) >= 1 && args[0] == "prune":
		return runSecretsPrune(args[1:])
	}
	return fmt.Errorf("secrets takes one subcommand, set or prune: paisans secrets set <dotted.key> [--secrets path] < value, or paisans secrets prune [--execute]")
}
```

```go
// runSecretsPrune removes the secrets that name something paisans.yaml no
// longer declares: a removed site's WireGuard key and heartbeat token, a
// removed app's passwords and sign-in client, a Pocket ID group nothing
// names. It lists them by key, never by value, and changes nothing without
// --execute. A sign-in client's secret going does not delete the client at
// Pocket ID, which it says.
func runSecretsPrune(args []string) error {
	fs := flag.NewFlagSet("secrets prune", flag.ContinueOnError)
	reporter := commonFlags(fs)
	configPath := fs.String("config", "paisans.yaml", "path to the deployment declaration")
	secretsPath := fs.String("secrets", "", "path to the secrets file (default: secrets.enc.yaml beside the config)")
	execute := fs.Bool("execute", false, "actually remove them and write the file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := reporter()
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *secretsPath == "" {
		*secretsPath = filepath.Join(filepath.Dir(*configPath), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(*secretsPath)
	if err != nil {
		return err
	}
	orphans := secretsgen.Orphans(cfg, secrets)
	if len(orphans) == 0 {
		r.Result("%s names nothing paisans.yaml does not declare. Nothing to prune.", *secretsPath)
		return nil
	}
	s := r.Step("prune secrets")
	for _, o := range orphans {
		s.Detail("- %s: %s", o.Key, o.Why)
		if o.Leaves != "" {
			s.Detail("  %s", o.Leaves)
		}
	}
	if !*execute {
		s.Done(fmt.Sprintf("%d to remove", len(orphans)))
		r.Result("Nothing changed. Re-run with --execute to remove them.")
		return nil
	}
	secretsgen.Prune(secrets, orphans)
	recipients, err := config.Recipients(filepath.Dir(*secretsPath))
	if err != nil {
		s.Fail(err)
		return err
	}
	if err := config.WriteSecrets(*secretsPath, secrets, recipients); err != nil {
		s.Fail(err)
		return err
	}
	s.Done(fmt.Sprintf("%d removed", len(orphans)))
	r.Result("%s no longer names anything paisans.yaml does not declare.", *secretsPath)
	return nil
}

// warnOrphans warns, once per orphaned secret, that it names something
// paisans.yaml does not declare. Never a refusal: nothing reads it.
func warnOrphans(r ui.Reporter, cfg *config.Config, secrets *config.Secrets) {
	for _, o := range secretsgen.Orphans(cfg, secrets) {
		r.Warn("secrets: "+o.Key+" "+o.Why, "`paisans secrets prune` removes it.")
	}
}
```

Check what a `ui.Step` offers (`Detail`, `Done`, `Fail`) in `internal/ui/ui.go`, and match what other commands call.

Call `warnOrphans(r, cfg, secrets)`:
- in `runApply`, right after `warnUnencrypted`'s `if` block;
- in `runInit`, after the secrets are loaded (or created empty) and before `secretsgen.Fill`.

In `internal/siteremove/remains.go`:

```go
func secretsLeft(site string) string {
	return fmt.Sprintf("secrets: run `paisans secrets prune` once nothing needs sites.%s. It holds its WireGuard key and heartbeat token, and this command never edits the secrets file", site)
}
```

Add `secrets prune [--execute]` to the usage text, beside `secrets set`.

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/paisans/ ./internal/siteremove/ ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/paisans internal/siteremove
git commit -m "feat: paisans secrets prune, and a warning for secrets nothing declares

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: `paisans site remove --force`

**Files:**
- Modify: `cmd/paisans/siteremove.go`
- Test: `cmd/paisans/siteremove_test.go`

**Interfaces:**
- Consumes: `siteremove.BuildForced`, `Plan.Current` (Task 4); `config.ParseDestination`, `Site.Destination` (Task 1).
- Produces:
  - `func chooseHost(cfg *config.Config, site, ssh string) (config.Destination, error)`
  - `func confirmSiteFor(stdin io.Reader, stdout io.Writer, site, what string) error`

- [ ] **Step 1: Write the failing tests**

```go
func TestChooseHost(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	declared := cfg.Sites["home-b"].Destination().String()
	for _, tc := range []struct{ site, ssh, want, err string }{
		{site: "home-b", want: declared},
		{site: "home-b", ssh: "admin@198.51.100.4", want: "admin@198.51.100.4:22"},
		{site: "never", ssh: "admin@192.0.2.1:2222", want: "admin@192.0.2.1:2222"},
		{site: "never", err: "name its host with --ssh"},
		{site: "home-b", ssh: "myalias", err: "not user@host"},
	} {
		got, err := chooseHost(cfg, tc.site, tc.ssh)
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s %s: err = %v, want %q", tc.site, tc.ssh, err, tc.err)
		case tc.err == "" && (err != nil || got.String() != tc.want):
			t.Errorf("%s %s: got %v, %v; want %s", tc.site, tc.ssh, got, err, tc.want)
		}
	}
}

// Refusals that need no host reach none.
func TestSiteRemoveForceRefusesBeforeReachingAHost(t *testing.T) {
	noSiteHosts(t)
	for args, want := range map[string]string{
		"home-b --ssh admin@192.0.2.1":    "only --force takes",
		"home-b --force --host-gone":      "Drop one of them",
		"never --force":                   "name its host with --ssh",
		"home-b --force --execute":        "terminal",
	} {
		err := runSiteRemove(append(strings.Fields(args), "--config", fixtureConfig()), strings.NewReader("home-b\n"), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", args, err, want)
		}
	}
}
```

`home-b --force --execute` is the site's own host with a non-terminal stdin. It must be refused before `removeSiteHost` is called, so `noSiteHosts` holds.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./cmd/paisans/ -run 'ChooseHost|ForceRefuses' -v`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

Flags in `runSiteRemove`:

```go
	force := fs.Bool("force", false, "clean this deployment off one host and nothing else: no cluster stage or refusal, and neither paisans.yaml nor the secrets file edited")
	sshFlag := fs.String("ssh", "", "with --force: the host to clean, user@host[:port], any host; only what carries this deployment's id or token is removed")
```

Update the extra-argument usage string to `paisans site remove <site> [--execute] [--host-gone] [--delete-data] [--force [--ssh user@host[:port]]]`.

Right after the `site == ""` check:

```go
	if *sshFlag != "" && !*force {
		return fmt.Errorf("site remove: --ssh names the host to clean, which only --force takes. A full removal reaches the site through its ssh section")
	}
	if *force {
		return runSiteRemoveForced(r, site, forcedArgs{config: *configPath, ssh: *sshFlag, execute: *execute, hostGone: *hostGone, deleteData: *deleteData, sudo: *sudo, secrets: *secretsPath}, stdin, stdout)
	}
```

Then:

```go
type forcedArgs struct {
	config, secrets, ssh                string
	execute, hostGone, deleteData, sudo bool
}

// runSiteRemoveForced is `site remove --force`: one host cleaned of this
// deployment, nothing else read or changed.
func runSiteRemoveForced(r ui.Reporter, site string, a forcedArgs, stdin io.Reader, stdout io.Writer) error {
	if a.hostGone {
		return fmt.Errorf("site remove %s: --force cleans one host, and --host-gone reaches none. Drop one of them", site)
	}
	cfg, err := config.Load(a.config)
	if err != nil {
		return err
	}
	result := validate.Check(cfg)
	reportFindings(r, a.config, result)
	if result.Refused() {
		return fmt.Errorf("%s was refused: %d problem(s) above", a.config, len(result.Refusals()))
	}
	dest, err := chooseHost(cfg, site, a.ssh)
	if err != nil {
		return err
	}
	own := false
	if s, ok := cfg.Sites[site]; ok && s.Destination() == dest {
		own = true
	}
	if a.execute && (own || a.deleteData) && !stdinIsTerminal(stdin) {
		return fmt.Errorf("site remove %s --force: it asks for the site's name at a terminal (%s), and stdin is not one. Run it from an interactive shell. Nothing was changed", site, map[bool]string{true: "this is the host the site runs on", false: "--delete-data deletes member data"}[own])
	}
	if a.secrets == "" {
		a.secrets = filepath.Join(filepath.Dir(a.config), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(a.secrets)
	if err != nil {
		return err
	}
	t := removeSiteHost(site, config.Site{SSH: config.SSH{User: dest.User, Host: dest.Host, Port: dest.Port}}, a.sudo)
	opts := siteremove.Options{DeleteData: a.deleteData, ConfigPath: a.config}
	plan, err := siteremove.BuildForced(cfg, secrets, site, dest, t, opts)
	if err != nil {
		return err
	}
	if !a.execute || r.Verbose() {
		plan.Show(r)
	}
	if !a.execute {
		reportRemains(r, plan.Remains())
		r.Result("Nothing changed. Re-run with --execute to apply.")
		return nil
	}
	if plan.Current {
		if err := confirmSiteFor(stdin, stdout, site, fmt.Sprintf("This cleans %s, the host %s runs on, out from under its cluster.", dest, site)); err != nil {
			return err
		}
	}
	if a.deleteData {
		if err := confirmSite(stdin, stdout, site); err != nil {
			return err
		}
	}
	plan.Report = r
	if err := siteremove.Execute(plan); err != nil {
		return err
	}
	reportRemains(r, plan.Remains())
	r.Result("%s is cleaned of this deployment. %s and the secrets file are unchanged.", dest, a.config)
	return nil
}

// chooseHost is the host --force cleans: --ssh when given, any host, else the
// declared site's own. Any host is safe to name, since only what carries this
// deployment's id or token is removed.
func chooseHost(cfg *config.Config, site, ssh string) (config.Destination, error) {
	if ssh != "" {
		d, err := config.ParseDestination(ssh)
		if err != nil {
			return config.Destination{}, fmt.Errorf("site remove %s: --ssh: %w", site, err)
		}
		return d, nil
	}
	if s, ok := cfg.Sites[site]; ok {
		return s.Destination(), nil
	}
	return config.Destination{}, fmt.Errorf("site remove %s: it is not declared, so name its host with --ssh user@host[:port]", site)
}
```

`removeSiteHost` builds the transport from a `config.Site` through `siteTransport`, which uses `site.SSH.Keys()`. With no `public_key`, that gives no `-i` keys and ssh uses the operator's agent. Check that `siteTransport` copes with an empty key list; it should, since `Keys()` on an empty section returns none.

Generalise `confirmSite`:

```go
// confirmSiteFor says what is about to happen, asks for the site's name at
// the terminal, and refuses anything else. There is no flag to answer it.
func confirmSiteFor(stdin io.Reader, stdout io.Writer, site, what string) error {
	if !stdinIsTerminal(stdin) {
		return fmt.Errorf("site remove: it asks for the site's name at a terminal, and stdin is not one. Nothing was changed")
	}
	fmt.Fprintf(stdout, "\n%s Type %s to go on: ", what, site)
	answer, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && answer == "" {
		return fmt.Errorf("site remove: no answer read. Nothing was changed")
	}
	if strings.TrimSpace(answer) != site {
		return fmt.Errorf("site remove: %q is not %s. Nothing was changed", strings.TrimSpace(answer), site)
	}
	return nil
}

func confirmSite(stdin io.Reader, stdout io.Writer, site string) error {
	return confirmSiteFor(stdin, stdout, site, fmt.Sprintf("This deletes this deployment's data on %s above, for good.", site))
}
```

Run `TestConfirmSiteWantsTheSitesName` and keep the error strings it checks. Add `"github.com/paisans-software/paisans-stack/internal/ui"` to the imports if needed.

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/paisans/ ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/paisans
git commit -m "feat: paisans site remove --force

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: README

**Files:**
- Modify: `README.md`, in the `site remove` section (`grep -n "site remove" README.md | head`) and the secrets section (`grep -n "secrets set" README.md | head`)

- [ ] **Step 1: Document it**

In the `site remove` section:
- add one line under stage 3: images only this deployment's containers ran are removed, by ID, without `-f`;
- add a subsection, ``### `--force` cleans one host and nothing else``, that covers:
  - what it skips: stages 1 and 2, their refusals, and both file edits;
  - `--ssh` accepting any host, with the matching table from the spec;
  - the typed confirmation for the site's own host;
  - the lost-deployment flow from the spec's *Where this leaves a lost deployment*, with documentation-range addresses.

In the secrets section, add `paisans secrets prune`: what counts as an orphan (the spec's table), the warning from `init`/`apply`, the dry run then `--execute`, and the Pocket ID client it leaves.

- [ ] **Step 2: Check the flags are real**

Run: `go run ./cmd/paisans site remove --help 2>&1 | grep -E -- '--force|--ssh'` and `go run ./cmd/paisans secrets prune --help 2>&1 | grep -- '--execute'`
Expected: every flag is listed.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: site remove --force and secrets prune

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

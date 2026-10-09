# `site remove --force` and the record of a site's hosts: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `paisans site remove <site> --force` cleans one host of this deployment, ignoring the cluster and leaving `paisans.yaml` alone. `secrets.enc.yaml` records every host each site was deployed to, so the toolkit can name, and clean, hosts the configuration no longer explains. Cleaning also removes images only this deployment's containers used.

**Architecture:** Stage 3 of `internal/siteremove` (`buildHost`, `runHost`, `verifyHost`) is reused unchanged in shape. A new `BuildForced` builds a plan of two stages: that host stage, then a record stage. It runs against a configuration copy in which the site's roles come from the host's registry entry and its ssh section from the chosen destination. The record is `SiteSecrets.Hosts`, a list of `config.Destination` strings. It is written by `apply`/`host prepare` and removed through an `Options.Forget` callback, so `siteremove` never touches the file itself.

**Tech Stack:** Go, the existing fake `world` in `internal/siteremove/world_test.go`, sops-encrypted secrets via `config.WriteSecrets`.

**Spec:** `docs/specs/2026-10-09-site-remove-force.md`. Its parent spec, for stage 3, is `docs/specs/2026-10-08-site-remove.md`.

## Global Constraints

- The repo is public. Use no real hostnames, IPs or deployment facts; use 203.0.113.0/24, 198.51.100.0/24, and site names from the fixtures (`home-a`, `home-b`, `vm`, `watch`).
- Docs, comments, commits and PRs never describe rejected alternatives.
- No migration code. Hosts prepared before the record existed are reached with `--ssh`; nothing back-fills the record.
- `--force` never edits `paisans.yaml`.
- `--force` on the declared site's current host asks for the site's name at a terminal at `--execute`. No flag answers it.
- The record format is `user@host:port` with the port always written; IPv6 is `user@[addr]:port`.
- The record is written before the first change on a host, and only with `--execute`.
- Images are removed by ID with `docker image rm` and without `-f`. A Docker refusal is reported as kept, never as a failure.
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- `go test ./...` and `go vet ./...` pass after every task.

## Review Focus

1. **`--force` against a host whose registry has no entry for this deployment** (never deployed, or already cleaned). Expected: a plan with nothing to remove, and the record entry still dropped. Pinned in Task 5, `TestForcedOnACleanHostOnlyForgets`.
2. **A forced, undeclared gateway site whose host serves the owner's `/srv/caddy.d` sites.** Expected: the Caddy hand-over runs, because roles come from the registry. Pinned in Task 5, `TestForcedUndeclaredGatewayHandsOverCaddy`.
3. **An image shared between one of our containers and a foreign container.** Expected: kept, with a reason. Pinned in Task 3, `TestImagesSharedWithAForeignContainerStay`.
4. **`--ssh` without a port, or with an ssh alias.** Expected: `user@host` gets `:22`. A bare alias with no `@` is refused with the expected form. Pinned in Task 1, `TestParseDestination`.
5. **A full `site remove --host-gone`.** Expected: the declared destination is dropped from the record even though no host was reached. Pinned in Task 4, `TestFullRemovalForgetsTheDeclaredHost`.

---

### Task 1: `config.Destination` and `SiteSecrets.Hosts`

**Files:**
- Modify: `internal/config/ssh.go` (add `Destination`, `ParseDestination`, `Site.Destination`)
- Modify: `internal/config/secrets.go:65-71` (`SiteSecrets.Hosts`)
- Create: `internal/config/hosts.go` (`RecordHost`, `ForgetHost`, `HostsOf`, `RecordedSites`)
- Test: `internal/config/hosts_test.go`

**Interfaces:**
- Produces:
  - `type Destination struct { User, Host string; Port int }`
  - `func ParseDestination(s string) (Destination, error)`
  - `func (d Destination) String() string`
  - `func (s Site) Destination() Destination`
  - `func (s *Secrets) RecordHost(site string, d Destination) bool`, which is true when the destination was added
  - `func (s *Secrets) ForgetHost(site string, d Destination) bool`, which is true when it was removed
  - `func (s *Secrets) HostsOf(site string) []Destination`
  - `func (s *Secrets) RecordedSites() []string`, sorted, only sites with at least one host

- [ ] **Step 1: Write the failing tests**

```go
package config_test

import (
	"path/filepath"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

func TestParseDestination(t *testing.T) {
	for in, want := range map[string]string{
		"admin@203.0.113.9":       "admin@203.0.113.9:22",
		"admin@203.0.113.9:2222":  "admin@203.0.113.9:2222",
		"admin@[2001:db8::1]:22":  "admin@[2001:db8::1]:22",
		"admin@host.example.org":  "admin@host.example.org:22",
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

func TestRecordAndForgetHosts(t *testing.T) {
	s := &config.Secrets{}
	a, _ := config.ParseDestination("admin@203.0.113.9")
	b, _ := config.ParseDestination("admin@198.51.100.4")
	if !s.RecordHost("home-b", a) || s.RecordHost("home-b", a) {
		t.Fatal("RecordHost did not add once and only once")
	}
	s.RecordHost("home-b", b)
	if got := s.HostsOf("home-b"); len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("HostsOf = %v", got)
	}
	if !s.ForgetHost("home-b", a) || s.ForgetHost("home-b", a) {
		t.Fatal("ForgetHost did not remove once and only once")
	}
	if got := s.RecordedSites(); len(got) != 1 || got[0] != "home-b" {
		t.Errorf("RecordedSites = %v", got)
	}
	s.ForgetHost("home-b", b)
	if len(s.RecordedSites()) != 0 {
		t.Error("a site with no hosts left is still listed")
	}
}

func TestHostsRoundTripAndAreNotADottedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.yaml")
	s := &config.Secrets{}
	d, _ := config.ParseDestination("admin@203.0.113.9")
	s.RecordHost("home-b", d)
	if err := config.WriteSecrets(path, s, nil); err != nil {
		t.Fatal(err)
	}
	back, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.HostsOf("home-b"); len(got) != 1 || got[0] != d {
		t.Fatalf("after a round trip: %v", got)
	}
	if err := back.Set("sites.home-b.hosts", "x"); err == nil {
		t.Error("Set accepted sites.<name>.hosts, which only the toolkit writes")
	}
	if _, ok := back.Get("sites.home-b.hosts"); ok {
		t.Error("Get answered for sites.<name>.hosts")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/config/ -run 'Destination|Hosts' -v`
Expected: FAIL to compile, with `undefined: config.ParseDestination`.

- [ ] **Step 3: Implement**

Add to `internal/config/secrets.go`, inside `SiteSecrets`, after `HeartbeatToken`:

```go
	// Hosts is every host the site has been prepared or applied on and not
	// yet cleaned, as Destination strings, oldest first. host prepare and
	// apply add the site's declared host before changing it, and site
	// remove takes out each host it cleans or is told is gone, so a host
	// the configuration no longer names is still known. It holds no
	// credential; it is here because this is the one file every admin
	// shares that the toolkit already writes.
	Hosts []string `yaml:"hosts,omitempty"`
```

Append to `internal/config/ssh.go`:

```go
// Destination is where a site's host is reached: user, host and port. Its
// string form, user@host:port with the port always written, is what
// secrets.enc.yaml records under sites.<name>.hosts.
type Destination struct {
	User string
	Host string
	Port int
}

func (d Destination) String() string {
	return d.User + "@" + net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
}

// ParseDestination reads user@host or user@host:port, with an IPv6 host in
// brackets. The user is required, because the record has to say who logs in,
// and an ssh alias is refused for the same reason.
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

Add `"strconv"` to `ssh.go`'s imports.

Create `internal/config/hosts.go`:

```go
package config

import "sort"

// RecordHost adds d to the site's recorded hosts, and reports whether it was
// not there already.
func (s *Secrets) RecordHost(site string, d Destination) bool {
	if s.Sites == nil {
		s.Sites = map[string]SiteSecrets{}
	}
	entry := s.Sites[site]
	for _, h := range entry.Hosts {
		if h == d.String() {
			return false
		}
	}
	entry.Hosts = append(entry.Hosts, d.String())
	s.Sites[site] = entry
	return true
}

// ForgetHost takes d out of the site's recorded hosts, and reports whether it
// was there. The site's other secrets stay.
func (s *Secrets) ForgetHost(site string, d Destination) bool {
	entry, ok := s.Sites[site]
	if !ok {
		return false
	}
	var kept []string
	for _, h := range entry.Hosts {
		if h != d.String() {
			kept = append(kept, h)
		}
	}
	if len(kept) == len(entry.Hosts) {
		return false
	}
	entry.Hosts = kept
	s.Sites[site] = entry
	return true
}

// HostsOf is the site's recorded hosts, oldest first. An entry that does not
// parse is skipped: the toolkit wrote every one, so it is a hand edit.
func (s *Secrets) HostsOf(site string) []Destination {
	var out []Destination
	for _, h := range s.Sites[site].Hosts {
		if d, err := ParseDestination(h); err == nil {
			out = append(out, d)
		}
	}
	return out
}

// RecordedSites is every site with at least one recorded host, sorted.
func (s *Secrets) RecordedSites() []string {
	var out []string
	for name, entry := range s.Sites {
		if len(entry.Hosts) > 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
```

`Get` and `Set` need no change: neither has a `hosts` case, so `Set` refuses the key and `Get` returns not set. The test pins that.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test ./internal/config/ -v -run 'Destination|Hosts' && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat: record the hosts each site was deployed to in the secrets file

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

### Task 4: a full removal forgets the site's declared host

**Files:**
- Modify: `internal/siteremove/siteremove.go`: `Options.Forget`; `buildConfig`
- Modify: `internal/siteremove/remains.go` (`secretsLeft` text)
- Test: `internal/siteremove/record_test.go`

**Interfaces:**
- Produces: `Options.Forget func(site string, d config.Destination) error`. It is called once the site is out of `paisans.yaml`. A nil `Forget` skips it, which is what the existing tests rely on.

- [ ] **Step 1: Write the failing test**

```go
package siteremove_test

import (
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

func forgetting(got *[]string) func(string, config.Destination) error {
	return func(site string, d config.Destination) error {
		*got = append(*got, site+" "+d.String())
		return nil
	}
}

func TestFullRemovalForgetsTheDeclaredHost(t *testing.T) {
	for _, hostGone := range []bool{false, true} {
		w := setup(t)
		if hostGone {
			w.unreachable["home-b"] = true
			w.etcdDown["home-b"] = true
			w.member("home-b")["Role"], w.member("home-b")["State"] = "Replica", "stopped"
			w.member("home-c")["Role"] = "Sync Standby"
		}
		var forgot []string
		want := "home-b " + w.cfg.Sites["home-b"].Destination().String()
		p := w.mustBuild("home-b", siteremove.Options{HostGone: hostGone, Forget: forgetting(&forgot)})
		if err := siteremove.Execute(p); err != nil {
			t.Fatal(err)
		}
		if len(forgot) != 1 || forgot[0] != want {
			t.Errorf("host-gone=%v: forgot %v, want [%s]", hostGone, forgot, want)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/siteremove/ -run TestFullRemovalForgets -v`
Expected: FAIL to compile, with `unknown field Forget`.

- [ ] **Step 3: Implement**

In `Options`:

```go
	// Forget takes a host out of the site's recorded hosts in the secrets
	// file. Nil leaves the record alone.
	Forget func(site string, d config.Destination) error
```

In `buildConfig`:
- Add a step: `Step{Site: p.Site, Verb: "forget", Title: "forget " + p.Site + "'s host", Text: fmt.Sprintf("%s from sites.%s.hosts in the secrets file", p.cfg.Sites[p.Site].Destination(), p.Site)}`.
- At the end of `st.run`, after the config edit succeeds:

```go
		if p.Forget != nil {
			p.work("forget " + p.Site + "'s host")
			return p.Forget(p.Site, p.cfg.Sites[p.Site].Destination())
		}
		return nil
```

Restructure the existing `return config.RemoveSite...` lines into `err := ...; if err != nil { return err }` first.

In `remains.go`, change `secretsLeft` to:

```go
func secretsLeft(site string) string {
	return fmt.Sprintf("secrets: remove sites.%s from the secrets file with sops. It is its WireGuard key; this command takes only its host out of sites.%s.hosts, so remove the rest once nothing needs it", site, site)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/siteremove/ ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/siteremove
git commit -m "feat: site remove takes the cleaned host out of the secrets file's record

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: `siteremove.BuildForced`

**Files:**
- Create: `internal/siteremove/force.go`
- Modify: `internal/siteremove/host.go`: `buildHost` calls `ownership.Classify` only for a gateway, and reads the key user from `p.cfg`, which `force.go` sets
- Test: `internal/siteremove/force_test.go`

**Interfaces:**
- Consumes: `config.Destination`, `Options.Forget` (Tasks 1, 4); `buildHost`, `runHost`, `verifyHost` and `hostGoneLeft` (existing).
- Produces:
  - `func BuildForced(cfg *config.Config, secrets *config.Secrets, site string, dest config.Destination, t apply.Transport, o Options) (*Plan, error)`. `t` is nil when `o.HostGone`.
  - `Plan.Current bool`: true when `dest` is the declared site's own host. The command asks for the site's name when it is.
  - The plan's stages are `[clean the host (Number 3), record (Number 4)]`.

- [ ] **Step 1: Write the failing tests**

```go
package siteremove_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/registry"
	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

func stageNamed(p *siteremove.Plan, name string) *siteremove.Stage {
	for _, st := range p.Stages {
		if st.Name == name {
			return st
		}
	}
	return nil
}

func forced(t *testing.T, w *world, site string, o siteremove.Options) *siteremove.Plan {
	t.Helper()
	dest := w.cfg.Sites[site].Destination()
	var tr apply.Transport
	if !o.HostGone {
		tr = w.hosts[site]
	}
	p, err := siteremove.BuildForced(w.cfg, w.secrets, site, dest, tr, o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// None of the cluster's refusals stop --force: home-a has apps pinned to it
// and etcd is unhealthy.
func TestForcedIgnoresTheClustersRefusals(t *testing.T) {
	w := setup(t)
	w.etcdDown["home-c"] = true
	var forgot []string
	p := forced(t, w, "home-a", siteremove.Options{Forget: forgetting(&forgot)})
	if len(p.Stages) != 2 || stageNamed(p, "clean the host") == nil || stageNamed(p, "record") == nil {
		t.Fatalf("stages:\n%s", printed(p))
	}
	if !p.Current {
		t.Error("home-a's declared host is not marked current")
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	for _, h := range w.hosts {
		if h.name != "home-a" && len(h.commands) > 0 {
			t.Errorf("%s was reached: %v", h.name, h.commands)
		}
	}
	if len(forgot) != 1 {
		t.Errorf("forgot %v", forgot)
	}
	cfg, err := config.Load(w.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Sites["home-a"]; !ok {
		t.Error("--force edited paisans.yaml")
	}
}

// The host stage of a forced plan is the unforced plan's host stage.
func TestForcedHostStageMatchesTheFullRemoval(t *testing.T) {
	w := setup(t)
	full := w.mustBuild("home-b", siteremove.Options{})
	f := forced(t, w, "home-b", siteremove.Options{})
	a, b := stageNamed(full, "clean the host").Steps, stageNamed(f, "clean the host").Steps
	if len(a) != len(b) {
		t.Fatalf("full:\n%s\nforced:\n%s", printed(full), printed(f))
	}
	for i := range a {
		if a[i].Text != b[i].Text {
			t.Errorf("step %d: %q vs %q", i, a[i].Text, b[i].Text)
		}
	}
}

// A site no longer declared is cleaned from what its host proves, its roles
// read from the registry entry.
func TestForcedUndeclaredGatewayHandsOverCaddy(t *testing.T) {
	w := setup(t)
	vm := w.hosts["vm"]
	vm.files[render.HostSitesDir+"/blog.caddy"] = "blog.example.org { respond 200 }\n"
	dest := w.cfg.Sites["vm"].Destination()
	cfg := w.cfg.WithoutSite("vm")
	p, err := siteremove.BuildForced(cfg, w.secrets, "vm", dest, vm, siteremove.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Current {
		t.Error("an undeclared site's host is marked current")
	}
	if !hasStepIn(stageNamed(p, "clean the host"), "vm", "hand over", "Caddy") {
		t.Fatalf("no hand over:\n%s", printed(p))
	}
}

// A host with nothing of this deployment's on it plans nothing and is still
// forgotten.
func TestForcedOnACleanHostOnlyForgets(t *testing.T) {
	w := setup(t)
	b := w.hosts["home-b"]
	b.containers, b.networks, b.volumes, b.rules = nil, nil, nil, nil
	b.files = map[string]string{registry.Path: encode(t, registry.Registry{Version: registry.Version, Deployments: map[string]registry.Entry{}})}
	b.wgUp = false
	var forgot []string
	p := forced(t, w, "home-b", siteremove.Options{Forget: forgetting(&forgot)})
	if n := len(stageNamed(p, "clean the host").Steps); n != 0 {
		t.Fatalf("%d step(s) planned:\n%s", n, printed(p))
	}
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if len(forgot) != 1 {
		t.Errorf("forgot %v", forgot)
	}
}

// --host-gone with --force reaches nothing and only forgets.
func TestForcedHostGoneOnlyForgets(t *testing.T) {
	w := setup(t)
	var forgot []string
	p := forced(t, w, "home-b", siteremove.Options{HostGone: true, Forget: forgetting(&forgot)})
	if err := siteremove.Execute(p); err != nil {
		t.Fatal(err)
	}
	if n := len(w.hosts["home-b"].commands); n != 0 {
		t.Errorf("home-b was sent %d command(s)", n)
	}
	if len(forgot) != 1 {
		t.Errorf("forgot %v", forgot)
	}
}

// The plan for the declared site's own host says what the cluster loses.
func TestForcedCurrentHostSaysWhatTheClusterLoses(t *testing.T) {
	w := setup(t)
	p := forced(t, w, "home-b", siteremove.Options{})
	if !strings.Contains(printed(p), "still declared") {
		t.Errorf("no warning:\n%s", printed(p))
	}
}
```

Add `hasStepIn` to `siteremove_test.go`:

```go
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

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/siteremove/ -run Forced -v`
Expected: FAIL to compile, with `undefined: siteremove.BuildForced`.

- [ ] **Step 3: Make `buildHost` independent of declared roles**

In `buildHost`, replace:

```go
	report, err := ownership.Classify(p.cfg, p.Site, inv, inv.ManifestFiles)
	if err != nil {
		return nil, err
	}
	if p.cfg.Sites[p.Site].Has(config.RoleGateway) && report.Foreign() {
```

with:

```go
	if p.cfg.Sites[p.Site].Has(config.RoleGateway) {
		report, err := ownership.Classify(p.cfg, p.Site, inv, inv.ManifestFiles)
		if err != nil {
			return nil, err
		}
		if report.Foreign() {
```

Keep the closing braces balanced. `report` is used nowhere else; check with `grep -n report internal/siteremove/host.go`.

- [ ] **Step 4: Implement `force.go`**

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
// deployment, and dest taken out of the site's recorded hosts. Nothing else
// is read or changed. No other site is reached, the cluster's refusals are
// not checked, and paisans.yaml is not edited, because --force is for a host
// the cluster cannot be asked about or no longer counts on
// (docs/specs/2026-10-09-site-remove-force.md).
//
// The host stage runs against a copy of the configuration in which the site
// is declared with dest as its ssh section and the roles its registry entry on
// the host records. Those are what the host was deployed with, which is what
// cleaning it needs, for an undeclared site and for a host the site has left
// alike.
func BuildForced(cfg *config.Config, secrets *config.Secrets, site string, dest config.Destination, t apply.Transport, o Options) (*Plan, error) {
	if o.HostGone && o.DeleteData {
		return nil, fmt.Errorf("site remove %s: --delete-data deletes data on the host, and --host-gone does not reach it. Drop one of them", site)
	}
	declared, isDeclared := cfg.Sites[site]
	p := &Plan{Site: site, Options: o, secrets: secrets, transports: map[string]apply.Transport{}}
	p.Current = isDeclared && declared.Destination() == dest

	s := config.Site{SSH: config.SSH{User: dest.User, Host: dest.Host, Port: dest.Port}}
	if isDeclared {
		s.Roles = declared.Roles
	}
	if !o.HostGone {
		if out, err := t.Run("true"); err != nil {
			if errors.Is(err, apply.ErrUnreachable) {
				return nil, fmt.Errorf("site remove %s: %s does not answer over ssh (%v), so what is on it cannot be read or cleaned. Fix ssh and run again, or, if the host is never coming back, run with --host-gone: it is taken out of the record and not reached", site, dest, err)
			}
			return nil, fmt.Errorf("site remove %s: %s answered `true` with an error: %v: %s", site, dest, err, lastLines(out, 2))
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
		p.transports[site] = t
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
	if p.Current && host.Skipped == "" {
		host.Steps = append([]Step{{Site: site, Verb: "note", Title: "note the cluster", Text: fmt.Sprintf("%s is still declared and this is its host: its etcd member, Patroni replica and Garage node, and its place in every other site's mesh, stay in the cluster until a full `paisans site remove %s`, and its next `paisans apply --site %s` deploys it again", site, site, site)}}, host.Steps...)
	}
	p.Stages = append(p.Stages, host, p.buildRecord(dest))
	return p, nil
}

// buildRecord is a forced removal's last stage: dest out of the site's
// recorded hosts.
func (p *Plan) buildRecord(dest config.Destination) *Stage {
	st := &Stage{
		Number: 4,
		Name:   "record",
		Short:  "the record no longer lists the host",
		Gate:   fmt.Sprintf("sites.%s.hosts in the secrets file no longer lists %s", p.Site, dest),
		Steps:  []Step{{Site: p.Site, Verb: "forget", Title: "forget " + dest.String(), Text: fmt.Sprintf("%s from sites.%s.hosts in the secrets file; paisans.yaml is not edited", dest, p.Site)}},
	}
	st.run = func() error {
		p.work("forget " + dest.String())
		if p.Forget == nil {
			return nil
		}
		return p.Forget(p.Site, dest)
	}
	st.gate = func() error {
		for _, h := range p.secrets.HostsOf(p.Site) {
			if h == dest && p.Forget != nil {
				return fmt.Errorf("sites.%s.hosts still lists %s", p.Site, dest)
			}
		}
		return nil
	}
	return st
}
```

Add `Current bool` to `Plan`, after `Kept`:

```go
	// Current is set by BuildForced when the host is the declared site's
	// own, which the cluster still counts on: the command asks for the
	// site's name before cleaning it.
	Current bool
```

`p.Show`, `Remains` and `Execute` iterate `p.Stages` and need no change. Check that `Remains` does not dereference `p.end`, `p.full` or `p.rendered` (`grep -n "p.end\|p.full\|p.rendered" internal/siteremove/siteremove.go`). If it does, guard those lines with `if p.end != nil`.

For a forced plan, `Forget` must also update the in-memory `secrets` that the gate reads. The command's `Forget` calls `secrets.ForgetHost` before writing (Task 7). In tests, `forgetting` does not, so the gate checks `p.Forget != nil` and tests that need the gate to bite call `w.secrets.ForgetHost` inside their callback.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/siteremove/ -v -run 'Forced' && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/siteremove
git commit -m "feat: site remove --force cleans one host and nothing else

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: `apply` and `host prepare` record the host; `validate` and `apply` report unexplained hosts

**Files:**
- Create: `cmd/paisans/hosts.go`
- Modify: `cmd/paisans/main.go`: `runApply` after `claimHosts`; `runHostPrepare` after `claimHosts`; `runValidate` gains `--secrets` and the notes
- Test: `cmd/paisans/hosts_test.go`

**Interfaces:**
- Consumes: `config.Secrets.RecordHost`, `HostsOf`, `RecordedSites`, and `config.Site.Destination` (Task 1).
- Produces:
  - `func recordHost(secretsPath string, secrets *config.Secrets, site string, d config.Destination) error`, which writes only when the destination is new
  - `func hostNotes(cfg *config.Config, secrets *config.Secrets) []string`

- [ ] **Step 1: Write the failing tests**

```go
package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

func TestHostNotesNameEveryUnexplainedHost(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	s := &config.Secrets{}
	gone, _ := config.ParseDestination("admin@203.0.113.9")
	old, _ := config.ParseDestination("admin@198.51.100.4")
	s.RecordHost("monitor-a", gone)
	s.RecordHost("home-b", old)
	s.RecordHost("home-b", cfg.Sites["home-b"].Destination())

	notes := strings.Join(hostNotes(cfg, s), "\n")
	for _, want := range []string{
		"monitor-a was deployed to admin@203.0.113.9:22 and is no longer declared: paisans site remove monitor-a --force --execute",
		"home-b was deployed to admin@198.51.100.4:22 and now names " + cfg.Sites["home-b"].Destination().String() + ": paisans site remove home-b --force --ssh admin@198.51.100.4:22 --execute",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("missing %q in:\n%s", want, notes)
		}
	}
	if strings.Count(notes, "\n") != 1 {
		t.Errorf("want two notes, got:\n%s", notes)
	}
}

func TestRecordHostWritesOnlyANewHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.yaml")
	s := &config.Secrets{}
	d, _ := config.ParseDestination("admin@203.0.113.9")
	if err := recordHost(path, s, "home-b", d); err != nil {
		t.Fatal(err)
	}
	back, err := config.LoadSecrets(path)
	if err != nil || len(back.HostsOf("home-b")) != 1 {
		t.Fatalf("not written: %v %v", back, err)
	}
	if err := recordHost(filepath.Join(t.TempDir(), "missing-dir", "x.yaml"), s, "home-b", d); err != nil {
		t.Errorf("a host already recorded was written again: %v", err)
	}
}
```

The second `recordHost` call points at an unwritable path, so it errors only if it tries to write.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./cmd/paisans/ -run 'HostNotes|RecordHost' -v`
Expected: FAIL to compile.

- [ ] **Step 3: Implement `cmd/paisans/hosts.go`**

```go
package main

import (
	"fmt"
	"path/filepath"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// recordHost adds the site's host to sites.<name>.hosts and writes the
// secrets file, only when the host is not recorded yet. apply and host
// prepare call it once the host answers and before they change anything on
// it, so a run that fails part way still leaves the host known to
// `site remove --force` (docs/specs/2026-10-09-site-remove-force.md).
func recordHost(secretsPath string, secrets *config.Secrets, site string, d config.Destination) error {
	if !secrets.RecordHost(site, d) {
		return nil
	}
	recipients, err := config.Recipients(filepath.Dir(secretsPath))
	if err != nil {
		return err
	}
	if err := config.WriteSecrets(secretsPath, secrets, recipients); err != nil {
		return fmt.Errorf("recording %s as a host of %s in %s: %w", d, site, secretsPath, err)
	}
	return nil
}

// hostNotes is one line for each recorded host the configuration no longer
// explains: a site no longer declared, or a host a declared site has moved
// away from. Each names the command that cleans it.
func hostNotes(cfg *config.Config, secrets *config.Secrets) []string {
	var out []string
	for _, site := range secrets.RecordedSites() {
		declared, ok := cfg.Sites[site]
		for _, d := range secrets.HostsOf(site) {
			switch {
			case !ok:
				out = append(out, fmt.Sprintf("%s was deployed to %s and is no longer declared: paisans site remove %s --force --execute", site, d, site))
			case d != declared.Destination():
				out = append(out, fmt.Sprintf("%s was deployed to %s and now names %s: paisans site remove %s --force --ssh %s --execute, or --host-gone if that host no longer exists", site, d, declared.Destination(), site, d))
			}
		}
	}
	return out
}
```

If an undeclared site has several recorded hosts, its note must name `--ssh`. Make the first case emit `--force --ssh <d> --execute` when `len(secrets.HostsOf(site)) > 1`, and add that case to the test.

- [ ] **Step 4: Wire it in**

In `runApply`, directly after `claimHosts(...)` succeeds:

```go
	if *execute {
		if err := recordHost(*secretsPath, secrets, *site, declared.Destination()); err != nil {
			return err
		}
	}
	for _, line := range hostNotes(cfg, secrets) {
		r.Note("%s", line)
	}
```

Check the reporter's method for a note line: `grep -n "func (.*) Note\|Warn(" internal/ui/*.go`. Use the one `reportFindings` uses for warnings if `Note` does not exist.

In `runHostPrepare`, add a `--secrets` flag like `runApply`'s. After `claimHosts`, with `*execute`, load the secrets only if the file exists (`os.Stat`). A missing file means `init` has not run, so the host is recorded by its first `apply`. Then call `recordHost`.

In `runValidate`, add a `--secrets` flag, defaulting to `secrets.enc.yaml` beside the config. When the file exists, load it. A load error becomes a warning line ("the record of hosts could not be read: …"), never a refusal. Then print `hostNotes` through the same reporter.

- [ ] **Step 5: Run tests**

Run: `go test ./cmd/paisans/ ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/paisans
git commit -m "feat: apply and host prepare record each site's host, and validate and apply name the ones left behind

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: `paisans site remove --force`

**Files:**
- Modify: `cmd/paisans/siteremove.go`
- Test: `cmd/paisans/siteremove_test.go`

**Interfaces:**
- Consumes: `siteremove.BuildForced`, `Plan.Current`, `Options.Forget` (Tasks 4, 5); `recordHost`'s write path (Task 6); `config.ParseDestination` (Task 1).
- Produces:
  - `func chooseHost(cfg *config.Config, secrets *config.Secrets, site, ssh string) (config.Destination, error)`
  - `func forgetHost(secretsPath string, secrets *config.Secrets) func(string, config.Destination) error`

- [ ] **Step 1: Write the failing tests**

```go
func TestChooseHost(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	declared := cfg.Sites["home-b"].Destination()
	old, _ := config.ParseDestination("admin@198.51.100.4")
	other, _ := config.ParseDestination("admin@198.51.100.5")
	s := &config.Secrets{}
	s.RecordHost("home-b", old)
	s.RecordHost("gone", old)
	s.RecordHost("twice", old)
	s.RecordHost("twice", other)

	for _, tc := range []struct{ site, ssh, want, err string }{
		{site: "home-b", want: declared.String()},
		{site: "home-b", ssh: "admin@198.51.100.4", want: "admin@198.51.100.4:22"},
		{site: "home-b", ssh: "admin@192.0.2.1", err: "neither"},
		{site: "gone", want: "admin@198.51.100.4:22"},
		{site: "twice", err: "pass --ssh"},
		{site: "twice", ssh: "admin@198.51.100.5", want: "admin@198.51.100.5:22"},
		{site: "never", err: "name its host with --ssh"},
		{site: "never", ssh: "admin@192.0.2.1", want: "admin@192.0.2.1:22"},
		{site: "home-b", ssh: "myalias", err: "not user@host"},
	} {
		got, err := chooseHost(cfg, s, tc.site, tc.ssh)
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s %s: err = %v, want %q", tc.site, tc.ssh, err, tc.err)
		case tc.err == "" && (err != nil || got.String() != tc.want):
			t.Errorf("%s %s: got %v, %v; want %s", tc.site, tc.ssh, got, err, tc.want)
		}
	}
}

// --force on the declared site's own host asks for its name, and without a
// terminal is refused before anything changes.
func TestForcedCurrentHostNeedsATerminal(t *testing.T) {
	saved := removeSiteHost
	removeSiteHost = func(string, config.Site, bool) apply.Transport { return answeringHost{} }
	t.Cleanup(func() { removeSiteHost = saved })
	err := runSiteRemove([]string{"home-a", "--config", fixtureConfig(), "--force", "--execute"}, strings.NewReader("home-a\n"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("err = %v", err)
	}
}
```

`answeringHost` is a minimal `apply.Transport` that answers `true` and returns an empty registry. Check whether `cmd/paisans/*_test.go` already has one (`grep -n "apply.Transport = \|Transport{}" cmd/paisans/*_test.go`). Write one only if it is missing.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./cmd/paisans/ -run 'ChooseHost|ForcedCurrent' -v`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

Add flags in `runSiteRemove`:

```go
	force := fs.Bool("force", false, "clean this deployment off the site's host and nothing else: no cluster stage, no refusal about the cluster, paisans.yaml left as it is (docs/specs/2026-10-09-site-remove-force.md)")
	sshFlag := fs.String("ssh", "", "with --force: the host to clean, user@host[:port], when it is not the site's declared one")
```

Update the usage string in the extra-argument error to include `[--force [--ssh user@host[:port]]]`.

Branch right after the `site == ""` check. That is before `config.Load`'s refusal path for undeclared sites; the validate check still runs on the whole file:

```go
	if *force {
		return runSiteRemoveForced(r, site, forcedArgs{config: *configPath, secrets: *secretsPath, ssh: *sshFlag, execute: *execute, hostGone: *hostGone, deleteData: *deleteData, sudo: *sudo}, stdin, stdout)
	}
	if *sshFlag != "" {
		return fmt.Errorf("site remove: --ssh names the host to clean, which only --force takes. A full removal reaches the site through its ssh section")
	}
```

Then add:

```go
type forcedArgs struct {
	config, secrets, ssh           string
	execute, hostGone, deleteData, sudo bool
}

// runSiteRemoveForced is `site remove --force`: one host cleaned of this
// deployment and taken out of the record, nothing else read or changed.
func runSiteRemoveForced(r ui.Reporter, site string, a forcedArgs, stdin io.Reader, stdout io.Writer) error {
	cfg, err := config.Load(a.config)
	if err != nil {
		return err
	}
	result := validate.Check(cfg)
	reportFindings(r, a.config, result)
	if result.Refused() {
		return fmt.Errorf("%s was refused: %d problem(s) above", a.config, len(result.Refusals()))
	}
	if a.execute && a.deleteData && !stdinIsTerminal(stdin) {
		return fmt.Errorf("site remove: --delete-data deletes member data, which nothing brings back, so it asks for the site's name at a terminal and stdin is not one. Run it from an interactive shell. Nothing was changed")
	}
	if a.secrets == "" {
		a.secrets = filepath.Join(filepath.Dir(a.config), "secrets.enc.yaml")
	}
	secrets, err := config.LoadSecrets(a.secrets)
	if err != nil {
		return err
	}
	dest, err := chooseHost(cfg, secrets, site, a.ssh)
	if err != nil {
		return err
	}
	current := false
	if s, ok := cfg.Sites[site]; ok && s.Destination() == dest {
		current = true
	}
	if a.execute && current && !a.hostGone && !stdinIsTerminal(stdin) {
		return fmt.Errorf("site remove %s --force: %s is the declared site's own host, which its cluster still counts on, so it asks for the site's name at a terminal and stdin is not one. Run it from an interactive shell. Nothing was changed", site, dest)
	}
	var t apply.Transport
	if !a.hostGone {
		t = removeSiteHost(site, config.Site{SSH: config.SSH{User: dest.User, Host: dest.Host, Port: dest.Port}}, a.sudo)
	}
	opts := siteremove.Options{HostGone: a.hostGone, DeleteData: a.deleteData, ConfigPath: a.config, Forget: forgetHost(a.secrets, secrets)}
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
	if plan.Current && !a.hostGone {
		if err := confirmSiteFor(stdin, stdout, site, fmt.Sprintf("This cleans %s, the host %s still runs on, out from under its cluster.", dest, site)); err != nil {
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
	r.Result("%s is cleaned of this deployment and no longer recorded for %s; %s is unchanged.", dest, site, a.config)
	return nil
}

// chooseHost is the host --force cleans: --ssh when given, else the declared
// site's own, else the undeclared site's one recorded host. For a declared
// site, --ssh must be its own host or a recorded one, so a typo cannot clean
// a host the configuration says nothing about while the site runs elsewhere.
func chooseHost(cfg *config.Config, secrets *config.Secrets, site, ssh string) (config.Destination, error) {
	declared, isDeclared := cfg.Sites[site]
	recorded := secrets.HostsOf(site)
	if ssh != "" {
		d, err := config.ParseDestination(ssh)
		if err != nil {
			return config.Destination{}, fmt.Errorf("site remove %s: --ssh: %w", site, err)
		}
		if !isDeclared || d == declared.Destination() || slices.Contains(recorded, d) {
			return d, nil
		}
		return config.Destination{}, fmt.Errorf("site remove %s: %s is neither its declared host (%s) nor one it was deployed to (%s). Check the address", site, d, declared.Destination(), joinDestinations(recorded))
	}
	if isDeclared {
		return declared.Destination(), nil
	}
	switch len(recorded) {
	case 0:
		return config.Destination{}, fmt.Errorf("site remove %s: it is not declared and no host is recorded for it, so name its host with --ssh user@host[:port]", site)
	case 1:
		return recorded[0], nil
	}
	return config.Destination{}, fmt.Errorf("site remove %s: it was deployed to %s; pass --ssh with the one to clean", site, joinDestinations(recorded))
}

func joinDestinations(list []config.Destination) string {
	if len(list) == 0 {
		return "none recorded"
	}
	out := make([]string, len(list))
	for i, d := range list {
		out[i] = d.String()
	}
	return strings.Join(out, ", ")
}

// forgetHost takes a host out of the record and writes the secrets file.
func forgetHost(secretsPath string, secrets *config.Secrets) func(string, config.Destination) error {
	return func(site string, d config.Destination) error {
		if !secrets.ForgetHost(site, d) {
			return nil
		}
		recipients, err := config.Recipients(filepath.Dir(secretsPath))
		if err != nil {
			return err
		}
		return config.WriteSecrets(secretsPath, secrets, recipients)
	}
}
```

Generalise `confirmSite` into `confirmSiteFor(stdin, stdout, site, what string)`, which prints `what`, then `Type <site> to go on: `. Keep `confirmSite` as a call to it with the existing data-deletion sentence, so `TestConfirmSiteWantsTheSitesName` keeps passing.

Wire `Forget: forgetHost(*secretsPath, secrets)` into the existing non-force `opts`. `secrets` is loaded after `opts` is built, so set `opts.Forget` after `LoadSecrets`.

Add `"slices"` and `internal/ui` to the imports as needed.

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
- Modify: `README.md`, in the section on `site remove` (find it with `grep -n "site remove" README.md | head`)

- [ ] **Step 1: Document it**

Add a subsection after the existing `--host-gone` text, titled ``### `--force` cleans one host and nothing else``. It covers:
- what it skips (stages 1 and 2, their refusals, the yaml edit) and what it keeps doing (stage 3 with images, then forgetting the host);
- the three host-selection rules and `--ssh`;
- the typed confirmation for the site's current host;
- `--force --host-gone` for a destroyed host;
- the `sites.<name>.hosts` record: who writes it, who removes it, and that `validate`/`apply` print a note per unexplained host;
- the lost-deployment flow from the spec's *Where this leaves a lost deployment*, with documentation-range addresses.

Add one line under stage 3's description: images only this deployment's containers ran are removed, by ID, without `-f`.

- [ ] **Step 2: Check the commands in it are real**

Run: `go run ./cmd/paisans site remove --help 2>&1 | grep -E -- '--force|--ssh'`
Expected: both flags are listed.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: site remove --force and the record of hosts

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

# Apply-Created OIDC Clients Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `paisans apply` ensures the OIDC client of every app it starts whose kind has a client shape, recording the credentials before the app renders, so `oidc client create` is no longer a separate step before an apply.

**Architecture:** `internal/apply` gains one option, `Except`, which holds named app stacks back from an apply while the mesh and every other stack apply as usual, and keeps a held stack owed if a stopped apply owed it. `cmd/paisans` gains an identity step (`clientStep`, in `clients.go`) built on helpers factored out of `oidc.go`, so the command and `apply` share one code path into `internal/oidcclient`, which is unchanged. On `--execute`, a site running Pocket ID applies in two passes: everything but the other app stacks, a wait for Pocket ID's `/healthz`, the clients, then the rest re-rendered with the recorded credentials. A site without Pocket ID ensures the clients first and applies once.

**Tech Stack:** Go 1.26, standard library tests (`go test ./...`).

**Spec:** `docs/specs/2026-10-08-monitor-role-and-host-check.md`, Part 3 only. Parts 1 and 2 are other branches.

## Global Constraints

- Work only in `/Users/wash/Developer/paisans.community/src/paisans-stack/.worktrees/apply-oidc-clients`, branch `feat/apply-oidc-clients`.
- `internal/oidcclient` is unchanged in behaviour: the secret is generated on the workstation, written into the secrets file first, then sent, and never printed.
- `apply` never rotates a secret: `Desired.RotateSecret` is always false there. `oidc client create --rotate-secret` stays the way to rotate.
- Kinds with a client shape are whatever `kinds.OIDCClient` answers for (today `mbin` and `uptime`). No list of kinds is repeated in the new code.
- A mismatched existing client (`checkExisting`, surfaced as a `Build` error) refuses that app only; the rest of the site is applied and `apply` exits non zero at the end.
- Pocket ID unreachable, or `apps.<pocket-id>.static_api_key` empty: each app needing a client with none recorded is held back with the reason; an app with a recorded client goes ahead on it; `apply` exits zero.
- Secrets file writes go through `secretsRecorder`, re-encrypted to the recipients in `.sops.yaml`. An encrypted file with no recipient is refused before Pocket ID is contacted, when some app on the site has no recorded client.
- Commits: `<type>: <lowercase subject>`, a why-body wrapped at 80, ending with exactly `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. No em dashes. No rejected alternatives in docs, comments or commits. Never `git stash`.

## Review Focus

1. **A held app that a stopped apply still owed.** Releasing it later must force-recreate it, not trust a half built container: Task 1 tests the pending record keeps it.
2. **The secret reaching a terminal through an error.** An `Execute` failure after the secret was generated must not print it: Task 3 asserts on stdout and stderr together after a real create.
3. **`--only` naming no app with a client.** The identity step must not run, and nothing contacts Pocket ID: Task 3 tests `newClientStep` returns nil.
4. **A Pocket ID that answers but whose probe fails part way.** Treated as unreachable, not as a refusal: Task 3's unreachable test goes through the same `probeError` path as a curl failure.
5. **The first pass of a site running Pocket ID moving an app early.** Only infrastructure and Pocket ID move before the clients exist: Task 3 asserts the first pass holds every other app stack.

---

### Task 1: `apply.Except` holds app stacks back

**Files:**
- Modify: `internal/apply/apply.go` (options struct, new `Except`, `Build`, `Plan`, `writePending`, end of `Execute`)
- Create: `internal/apply/except_test.go`

**Interfaces:**
- Produces: `func Except(stacks ...string) Option`. With no names it changes nothing.

- [ ] **Step 1: Write the failing tests** in `internal/apply/except_test.go`:

```go
package apply_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
)

// A held stack's files are neither compared nor written and its action does
// not run, while the mesh and every other stack apply as usual. The next
// whole apply sees the held stack's files as a first write, not a conflict.
func TestExceptHoldsAStackBackAndAppliesTheRest(t *testing.T) {
	host := newHost()
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Except("talk"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Changes {
		if c.Stack == "talk" {
			t.Errorf("talk is held back, yet %s is planned", c.Path)
		}
	}
	for _, a := range p.Actions {
		if a.Stack == "talk" {
			t.Errorf("talk is held back, yet it is acted on")
		}
	}
	if p.WireGuard == apply.WireGuardNone {
		t.Error("holding an app back held the mesh back too")
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	if host.ran("/srv/talk/compose.yaml") {
		t.Error("talk's stack was acted on")
	}
	if _, ok := host.files["/srv/talk/.env"]; ok {
		t.Error("talk's files were written")
	}

	whole, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(whole.Conflicts()) != 0 {
		t.Fatalf("the whole apply after a held one sees conflicts: %v", whole.Conflicts())
	}
	if len(whole.Actions) != 1 || whole.Actions[0].Stack != "talk" {
		t.Errorf("the whole apply after a held one plans %v, want talk alone", whole.Actions)
	}
}

// A held stack a stopped apply still owes stays owed, so the apply that
// releases it force-recreates it.
func TestExceptKeepsAHeldStackOwed(t *testing.T) {
	host := applied(t, "home-a")
	host.files["/srv/.paisans-pending.json"] = `{"version":1,"actions":[{"stack":"docs","recreate":true},{"stack":"talk","recreate":true}]}`
	p, err := apply.Build("home-a", plan(t), acmeModule(t), host, apply.Except("talk"))
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(p, host); err != nil {
		t.Fatal(err)
	}
	record, ok := host.files["/srv/.paisans-pending.json"]
	if !ok || !strings.Contains(record, `"talk"`) || strings.Contains(record, `"docs"`) {
		t.Fatalf("after holding talk back the record of owed actions is %q, want talk alone", record)
	}
	next, err := apply.Build("home-a", plan(t), acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Actions) != 1 || next.Actions[0].Stack != "talk" || !next.Actions[0].Force {
		t.Errorf("releasing talk plans %+v, want talk force-recreated", next.Actions)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/apply -run TestExcept`
Expected: FAIL, `undefined: apply.Except`.

- [ ] **Step 3: Implement** in `internal/apply/apply.go`.

Add to `options`: `except []string`. Add to `Plan`, beside `recorded`:

```go
	// heldOwed is what the record of owed actions held for stacks this plan
	// holds back with Except. Execute keeps it there, so the apply that
	// releases a held stack still force-recreates it.
	heldOwed []pendingAction
```

Add the option after `Only`:

```go
// Except holds the named app stacks back from this apply: their files are
// not compared or written, their actions and gates do not run, and the
// manifest keeps their entries as the last apply recorded them. Everything
// else on the site, the mesh included, applies as usual. apply's identity
// step uses it for an app whose client at Pocket ID is not in place, and to
// start Pocket ID before the apps that sign in through it. A held stack that
// a stopped apply still owes stays owed.
func Except(stacks ...string) Option {
	return func(o *options) { o.except = append(o.except, stacks...) }
}
```

In `Build`, after the `onlySet` block:

```go
	var exceptSet map[string]bool
	if len(o.except) > 0 {
		exceptSet = map[string]bool{}
		for _, stack := range o.except {
			exceptSet[stack] = true
		}
		out.partial = true
		out.recorded = entries
	}
```

In the file loop, after `if onlySet != nil && !onlySet[change.Stack] { continue }`:

```go
		if exceptSet[change.Stack] {
			continue
		}
```

After the `onlySet` stack filter and before the `stackOrder` loop:

```go
	for _, action := range resumed.Actions {
		if exceptSet[action.Stack] {
			out.heldOwed = append(out.heldOwed, action)
		}
	}
	for stack := range exceptSet {
		delete(stacks, stack)
	}
```

Replace `out.restartDatabaseApps(o.dbApps, rendered)` with:

```go
	var dbApps []string
	for _, app := range o.dbApps {
		if !exceptSet[app] {
			dbApps = append(dbApps, app)
		}
	}
	out.restartDatabaseApps(dbApps, rendered)
```

In `writePending`, after the loop over `actions`: `p.Actions = append(p.Actions, plan.heldOwed...)`.

At the end of `Execute`, replace the `rm -f` block with:

```go
	if owes {
		if len(plan.heldOwed) > 0 {
			// Everything this plan moved is done; what it held back is still
			// owed, and nothing else is.
			data, err := json.MarshalIndent(pending{Version: 1, Actions: plan.heldOwed}, "", "  ")
			if err != nil {
				return err
			}
			if err := t.WriteFile(pendingPath, string(data)+"\n", 0o600); err != nil {
				return fmt.Errorf("%s: everything this apply moved was applied, but the record of the stacks it held back could not be written: %w", plan.Site, err)
			}
		} else if _, err := t.Run("rm -f " + shellQuote(pendingPath)); err != nil {
			return fmt.Errorf("%s: everything was applied, but the record of owed actions could not be removed, so the next apply will repeat them: %w", plan.Site, err)
		}
	}
```

- [ ] **Step 4: Run the package**

Run: `go test ./internal/apply`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/apply/apply.go internal/apply/except_test.go
git commit  # feat: apply.Except holds app stacks back from an apply
```

---

### Task 2: one code path for an app's client

**Files:**
- Create: `cmd/paisans/clients.go` (shared helpers)
- Modify: `cmd/paisans/oidc.go` (use them; recorder guard)
- Create: `cmd/paisans/clients_test.go`

**Interfaces:**
- Produces:
  - `func pocketIDApp(cfg *config.Config) string`
  - `func clientDesired(name string, app config.App, rotate bool) (oidcclient.Desired, bool)`
  - `func recordedClient(secrets *config.Secrets, app string) oidcclient.Recorded`
  - `func clientAPI(cfg *config.Config, site, destination, key string) *pocketid.Client`
  - `type probeError struct{ err error }` with `Error` and `Unwrap`
  - `func planClient(api *pocketid.Client, d oidcclient.Desired, rec oidcclient.Recorded) (*oidcclient.Plan, error)`, a failed probe returned as `*probeError`, a `Build` refusal redacted
  - `func printClientPlan(app, idp, where string, plan *oidcclient.Plan)`, lines to stdout, warnings to stderr
  - `secretsRecorder.Record` refuses an encrypted file with no recipient

- [ ] **Step 1: Write the failing test** in `cmd/paisans/clients_test.go`:

```go
package main

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
)

// The recorder is the one way a client's credentials reach the secrets file,
// for `oidc client create` and for apply. Written with no recipient, an
// encrypted file would come back as plaintext.
func TestSecretsRecorderRefusesAnEncryptedFileWithoutARecipient(t *testing.T) {
	path := tempSecrets(t)
	secrets, err := config.LoadSecrets(path)
	if err != nil {
		t.Fatal(err)
	}
	secrets.Encrypted = true
	rec := &secretsRecorder{app: "talk", path: path, secrets: secrets}
	err = rec.Record("c-1", "not-a-real-secret-0001")
	if err == nil || !strings.Contains(err.Error(), "names a recipient") {
		t.Fatalf("got %v", err)
	}
	if rec.wrote {
		t.Error("the recorder says it wrote")
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./cmd/paisans -run TestSecretsRecorderRefuses`
Expected: FAIL, the plaintext write succeeds.

- [ ] **Step 3: Implement.** `cmd/paisans/clients.go`:

```go
package main

import (
	"fmt"
	"os"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
	"github.com/paisans-software/paisans-stack/internal/oidcclient"
	"github.com/paisans-software/paisans-stack/internal/pocketid"
)

// pocketIDApp names the deployment's pocket-id app, empty when it declares
// none.
func pocketIDApp(cfg *config.Config) string {
	idp := ""
	for _, name := range cfg.AppNames() {
		if cfg.Apps[name].Kind == config.KindPocketID {
			idp = name
		}
	}
	return idp
}

// clientDesired is the client an app needs at Pocket ID, from its kind and
// its declaration, and whether this toolkit knows its kind's client.
func clientDesired(name string, app config.App, rotate bool) (oidcclient.Desired, bool) {
	spec, ok := kinds.OIDCClient(app.Kind, app.Hostname)
	if !ok {
		return oidcclient.Desired{}, false
	}
	adminGroup, memberGroup := spec.Groups(app)
	d := oidcclient.Desired{
		App:               name,
		CallbackURL:       spec.CallbackURL,
		LaunchURL:         spec.LaunchURL,
		ToolkitLaunchURLs: kinds.ToolkitLaunchURLs(app.Kind, app.Hostname),
		PKCE:              spec.PKCE,
		AdminGroup:        adminGroup,
		MemberGroup:       memberGroup,
		RotateSecret:      rotate,
	}
	// validate.Check has refused a malformed one already.
	if link, ok := app.Settings[kinds.DashboardLinkSetting].(string); ok {
		d.LaunchURL = kinds.LaunchURL(app.Kind, app.Hostname, link)
		d.LaunchURLChosen = true
	}
	return d, true
}

// recordedClient is what the secrets file holds for an app's client.
func recordedClient(secrets *config.Secrets, app string) oidcclient.Recorded {
	return oidcclient.Recorded{
		ClientID:     secrets.OIDCClients[app].ClientID,
		ClientSecret: secrets.OIDCClients[app].ClientSecret,
	}
}

// clientAPI is Pocket ID's API as the host on site reaches it, over the
// site's ssh section or destination verbatim, and without sudo: curl needs
// no root.
func clientAPI(cfg *config.Config, site, destination, key string) *pocketid.Client {
	return &pocketid.Client{Transport: oidcTransport(siteTransport(cfg.Sites[site], destination, false)), BaseURL: pocketIDBase(cfg, site), APIKey: key}
}

// probeError is a client plan that failed because Pocket ID could not be
// asked, which apply treats as Pocket ID being unreachable rather than as a
// refusal of the app.
type probeError struct{ err error }

func (e *probeError) Error() string { return e.err.Error() }
func (e *probeError) Unwrap() error { return e.err }

// planClient probes Pocket ID for one app's client and plans it. The probe
// only reads.
func planClient(api *pocketid.Client, d oidcclient.Desired, rec oidcclient.Recorded) (*oidcclient.Plan, error) {
	state, err := oidcclient.Probe(api, d)
	if err != nil {
		return nil, &probeError{err}
	}
	plan, err := oidcclient.Build(d, rec, state)
	if err != nil {
		return nil, pocketid.Redact(err, rec.ClientSecret)
	}
	return plan, nil
}

// printClientPlan shows one app's client: what is present, and each
// mutation with what it sends. Warnings go to stderr.
func printClientPlan(app, idp, where string, plan *oidcclient.Plan) {
	fmt.Fprintf(os.Stdout, "%s's client at %s on %s (pocket-id)\n", app, idp, where)
	for _, line := range plan.Present {
		fmt.Fprintf(os.Stdout, "  %s\n", line)
	}
	for _, step := range plan.Steps {
		fmt.Fprintf(os.Stdout, "  %s\n", step.Line)
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(os.Stderr, "paisans: warning: %s\n", w)
	}
}
```

In `cmd/paisans/oidc.go`, `runOIDCClientCreate` keeps its flags, validation and refusals, and replaces its own copies with the helpers:

```go
	desired, ok := clientDesired(*appName, app, *rotate)
	if !ok {
		return fmt.Errorf("oidc client create: this toolkit does not know what a %s client looks like yet. Implemented kinds: mbin, uptime", app.Kind)
	}
	idp := pocketIDApp(cfg)
	...
	recorded := recordedClient(secrets, *appName)
	api := clientAPI(cfg, where, *destination, key)
	plan, err := planClient(api, desired, recorded)
	if err != nil {
		return fmt.Errorf("oidc client create: %w", err)
	}
	printClientPlan(*appName, idp, where, plan)
```

and `secretsRecorder.Record` starts with:

```go
	if r.secrets.Encrypted && len(r.recipients) == 0 {
		return fmt.Errorf("%s is encrypted, but no %s beside it names a recipient, so the client's credentials could not be written back encrypted", r.path, config.SOPSConfigName)
	}
```

- [ ] **Step 4: Run the package**

Run: `go test ./cmd/paisans`
Expected: PASS, the `oidc client create` tests included.

- [ ] **Step 5: Commit**

```bash
git add cmd/paisans/clients.go cmd/paisans/clients_test.go cmd/paisans/oidc.go
git commit  # refactor: share oidc client create's client path for apply
```

---

### Task 3: apply's identity step

**Files:**
- Modify: `cmd/paisans/clients.go` (`clientStep`, `sitePass`, `executeWithClients`)
- Modify: `cmd/paisans/main.go` (`runApply`)
- Modify: `cmd/paisans/clients_test.go`

**Interfaces:**
- Consumes: Task 1's `apply.Except`; Task 2's helpers.
- Produces:
  - `type clientStep struct{...}`; `func newClientStep(cfg *config.Config, site, destination, secretsPath string, secrets *config.Secrets, only []string) (*clientStep, error)`, nil when the site runs no app with a client shape
  - `func (c *clientStep) ensure(execute, waiting bool)`
  - `func (c *clientStep) pocketIDHere() bool`, `holdForPocketID() []string`, `heldApps() []string`, `result() error`
  - `type sitePass struct { plan func(hold []string) (*apply.Plan, error); execute func(*apply.Plan) error }`
  - `func executeWithClients(c *clientStep, pass sitePass) ([]*apply.Plan, error)`

- [ ] **Step 1: Write the failing tests** in `cmd/paisans/clients_test.go`:

```go
// passLog is a sitePass that renders the site as apply would, records what
// each pass held back and whether talk rendered with a client, and executes
// nothing.
type passLog struct{ events []string }

func (l *passLog) pass(t *testing.T, cfg *config.Config, secrets *config.Secrets, site, app string) sitePass {
	return sitePass{
		plan: func(hold []string) (*apply.Plan, error) {
			rendered, err := render.Build(cfg, secrets)
			if err != nil {
				t.Fatal(err)
			}
			id := secrets.OIDCClients[app].ClientID
			has := false
			for _, f := range rendered.Files {
				if f.Path == site+"/srv/"+app+"/.env" && id != "" && strings.Contains(f.Content, id) {
					has = true
				}
			}
			l.events = append(l.events, fmt.Sprintf("plan hold=%s client=%t", strings.Join(hold, ","), has))
			return &apply.Plan{Site: site}, nil
		},
		execute: func(*apply.Plan) error {
			l.events = append(l.events, "execute")
			return nil
		},
	}
}

func loadFixture(t *testing.T, secretsPath string, forget ...string) (*config.Config, *config.Secrets) { ... }

func TestApplyCreatesAndRecordsTheClientBeforeTheAppRenders(t *testing.T)
func TestApplyDryRunSendsNoMutation(t *testing.T)
func TestApplyHoldsBackOnlyAppsWithoutAClientWhenPocketIDIsUnreachable(t *testing.T)
func TestApplyRefusesOnlyTheAppWhoseClientDiffers(t *testing.T)
func TestApplyRefusesAnUnwritableSecretsFileBeforeContactingPocketID(t *testing.T)
func TestApplyHasNoIdentityStepWithoutAnAppThatSignsIn(t *testing.T)
```

Assertions, in the fixture (`auth` is Pocket ID on home-a and home-b, home-a active; `talk` is mbin on both; `status` is uptime on vm):

- create: talk's client removed from a temp secrets file; on home-a, events are exactly `plan hold=blog,docs,gate,talk client=false`, `execute`, `plan hold= client=true`, `execute`; the fake was sent a create and a secret; the recorded secret is in the file and in neither stdout nor stderr.
- dry run: `ensure(false, true)` on home-a with talk's client removed prints `create client talk`, the fake was sent no mutation, the file is byte for byte unchanged.
- unreachable: every instance `down`; on vm, with status's client absent, the final pass holds `status` and nothing else and output says `skip`; with a recorded client, the final pass holds nothing and output says `unchecked`. `result()` is nil in both.
- mismatch: talk recorded, the fake holds a talk client whose callback is wrong; the final pass holds `talk`, `result()` names talk and says it differs.
- unwritable: secrets marked encrypted, talk's client removed, no `.sops.yaml`: `newClientStep` refuses with "names a recipient" and the fake saw no command.
- none: `newClientStep` with `only = ["docs"]` on home-a is nil.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./cmd/paisans -run 'TestApply'`
Expected: FAIL, `undefined: sitePass`.

- [ ] **Step 3: Implement** `clientStep` in `cmd/paisans/clients.go`:

```go
// clientStep is apply's identity step for one site. Every app the site runs
// whose kind has a client shape (kinds.OIDCClient) gets its client ensured at
// the deployment's Pocket ID before it renders, with the same plan and the
// same recorder as `oidc client create`. Declaring the app in paisans.yaml is
// the approval for its client (founder decision, 2026-10-08), so apply does
// not ask again. It never rotates a secret.
type clientStep struct {
	cfg         *config.Config
	site        string
	destination string
	secretsPath string
	secrets     *config.Secrets
	recipients  []string
	idp         string
	apps        []string
	held        map[string]string
	refused     map[string]error
	steps       int
}
```

`newClientStep` selects `apps` as every name in `cfg.AppNames()` that `render.AppSites(cfg)` places on `site`, that `only` names when it names any, and that `kinds.OIDCClient` knows; it reads `config.Recipients` beside the secrets file and refuses an encrypted file with none when some app has no recorded client:

```go
		return nil, fmt.Errorf("apply: %s is encrypted, but no %s beside it names a recipient, so the client %s needs could not be written back encrypted. Nothing was changed", secretsPath, config.SOPSConfigName, strings.Join(missing, ", "))
```

`ensure(execute, waiting)` resets `held`, `refused` and `steps`, prints a heading, and then:

1. no Pocket ID app: `cannotAsk(c.apps, "the configuration declares no pocket-id app to create it in", false)`;
2. empty `static_api_key`: `cannotAsk(c.apps, "secrets apps.<idp>.static_api_key is empty; `paisans init` generates it", false)`;
3. `pocketIDSite(cfg, idp, "", "apply")` fails: `cannotAsk(c.apps, err, waiting)`;
4. for each app: `planClient`; a `*probeError` is `cannotAsk(c.apps[i:], ..., waiting)` and ends the loop; another error is `refuse(app, err)`; otherwise `printClientPlan`, count the steps, and with `execute` run `oidcclient.Execute(plan, api, rec, secretsgen.ClientSecret)` through a `secretsRecorder`, refusing the app on failure and printing `recorded oidc_clients.<app>.client_id and oidc_clients.<app>.client_secret` when it wrote.

`cannotAsk(apps, why, waiting)`: with `waiting`, print one `ensure` line saying the clients are made once Pocket ID on this site answers, before the apps start, and hold nothing. Otherwise, for each app, a recorded client prints `unchecked <app>: <why>; its recorded client is used as it is`, and a missing one is held with `skip <app>: <why>. A re-run once Pocket ID answers creates its client and starts it`.

`refuse(app, err)` records the refusal, holds the app, and prints `refuse <app>: <err>. The rest of the site is applied`.

`result()` returns nil unless an app was refused:

```go
	return fmt.Errorf("apply: the rest of %s was applied, but %s was refused, and stays held back until its client at Pocket ID is fixed:\n  %s", c.site, strings.Join(names, ", "), strings.Join(reasons, "\n  "))
```

`waitForPocketID` is `apply.CheckOneActive` over every site of the Pocket ID app, each reached by `standbyLook(siteTransport(site, destination-if-this-site, false))`, the same look `pocketIDSite` takes.

`executeWithClients`:

```go
func executeWithClients(c *clientStep, pass sitePass) ([]*apply.Plan, error) {
	var plans []*apply.Plan
	if c.pocketIDHere() {
		first, err := pass.plan(c.holdForPocketID())
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stdout, "\nstarting %s's Pocket ID, and what it runs on, before the apps that sign in through it\n", c.site)
		if err := pass.execute(first); err != nil {
			return nil, err
		}
		plans = append(plans, first)
		if err := c.waitForPocketID(); err != nil {
			c.reset()
			c.cannotAsk(c.apps, err.Error(), false)
		} else {
			c.ensure(true, false)
		}
	} else {
		c.ensure(true, false)
	}
	final, err := pass.plan(c.heldApps())
	if err != nil {
		return plans, err
	}
	fmt.Fprintf(os.Stdout, "\nthe site, rendered with the clients in place\n")
	printPlan(final)
	if err := pass.execute(final); err != nil {
		return plans, err
	}
	return append(plans, final), nil
}
```

In `runApply` (`cmd/paisans/main.go`): build `clients` with `newClientStep` right after `transport`; when non nil, `clients.ensure(false, clients.pocketIDHere())` and plan with `apply.Except(clients.heldApps()...)`; a dry run with client steps planned says `Nothing was changed`, and returns `clients.result()`; `--execute` uses `executeWithClients` with a `sitePass` whose `plan` calls `planSiteApply` with the same options plus `apply.Except(hold...)`, copies `EtcdUnstarted` and sets `Progress`, and whose `execute` is `apply.Execute(p, transport)`; `checkStandby` sees every pass's actions; the command returns `clients.result()` last.

- [ ] **Step 4: Run the package**

Run: `go test ./cmd/paisans`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/paisans/clients.go cmd/paisans/clients_test.go cmd/paisans/main.go
git commit  # feat: apply creates and records each app's oidc client
```

---

### Task 4: tell the operator apply does it

**Files:**
- Modify: `internal/secretsgen/secretsgen.go` (owed reason for mbin and uptime), and its test if it pins the text
- Modify: `README.md` (*`oidc client create` makes an app's client at Pocket ID*, the secrets kinds table, the captured paragraph)
- Modify: `docs/development.md` (`init` owed bullet, `oidc client create` paragraph, the `apply` section)

- [ ] **Step 1: Failing test.** In `internal/secretsgen`, the owed reason for an mbin app names `paisans apply`:

```go
func TestAnOwedMbinClientNamesApply(t *testing.T) { /* Owed for talk with no client: Why contains "paisans apply" */ }
```

- [ ] **Step 2: Run it**: `go test ./internal/secretsgen -run TestAnOwedMbinClientNamesApply`, expected FAIL.

- [ ] **Step 3: Implement.** The mbin and uptime reasons become: created at the deployment's Pocket ID by `paisans apply` on the site that runs the app, which records the ID and secret here itself before the app renders; declaring the app is the approval. `oidc client create --app <name>` runs the step alone and rotates. Keep the redirect URI and PKCE facts. Update README and development.md to match, without sweeping unrelated em dashes.

- [ ] **Step 4: Run** `go test ./...`, expected PASS.

- [ ] **Step 5: Commit** (`docs: apply creates oidc clients, so oidc client create is optional`).

---

## Verification

```bash
gofmt -l .
go vet ./...
go test ./...
```

All three clean before the branch is handed back.

# `paisans apply` converges the whole deployment: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `paisans apply` with no `--site` plans and runs init, host prepare, the founding or joining of etcd members, the other sites, storage, a second apply pass and DNS, in that order, stopping at the first failure.

**Architecture:** `cmd/paisans/converge.go`. A pure `convergePlan(convergeState) []convergeStep` decides the steps; `readConvergeState` reads what decides them (init needed, which etcd members are founded); `runConverge` prints the plan, and with `--execute` runs each step through a replaceable `convergeRun` that calls the existing `run*` function with the step's flags. `apply.ErrFoundingWait` marks the founding stop so the run can tell it from a failure.

**Tech Stack:** Go.

**Spec:** `docs/specs/2026-10-09-apply-converge.md`.

## Global Constraints

- `apply --site <s>` and every other command are unchanged.
- Dry run by default; `--execute` runs. Nothing is removed.
- A step's error stops the run, except `apply.ErrFoundingWait` in the founding phase.
- The resume hint is `Run paisans apply --execute again to resume.`
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`; `go test ./...` and `go vet ./...` pass after each task.

## Review Focus

1. **A deployment where the witness is also the gateway** (staging). Expected: applied once in founding, not again in phase 3. Pinned in Task 2, `TestConvergeAppliesEachSiteOnceBeforePassTwo`.
2. **No etcd members declared.** Expected: no founding or joining phase; every site in phase 3. Pinned in Task 2, `TestConvergeWithoutEtcdAppliesEverySiteInPhaseThree`.
3. **The founding stop.** Expected: the run continues; the site is applied again in pass two. Pinned in Task 3, `TestTheFoundingStopDoesNotStopTheRun`.
4. **A failing step.** Expected: nothing after it runs; the error names the step and the resume hint. Pinned in Task 3, `TestAFailingStepStopsTheRun`.
5. **`--ssh` without `--site`.** Expected: refused. Pinned in Task 3, `TestConvergeRefusesSSH`.

---

### Task 1: `apply.ErrFoundingWait`

**Files:** `internal/apply/database.go`, `internal/apply/database_test.go` (or the test that covers `runBootstrap`'s founding stop: `grep -rn EtcdUnstarted internal/apply/*_test.go`)

- [ ] Test: the founding stop's error satisfies `errors.Is(err, apply.ErrFoundingWait)`. Implement `var ErrFoundingWait = errors.New("waiting on the other founding etcd members")` and wrap it in `runBootstrap`'s error with `%w`, keeping the message. Commit `feat: the founding stop is apply.ErrFoundingWait`.

### Task 2: the plan

**Files:** `cmd/paisans/converge.go`, `cmd/paisans/converge_test.go`

**Interfaces:**

```go
type convergeState struct {
	NeedsInit bool
	Founded   map[string]bool // etcd member -> holds infra/etcd-initial
}

type convergeStep struct {
	Phase string   // "configuration", "hosts", "founding", "joining", "other sites", "storage", "pass two", "dns"
	Title string   // what the operator reads, Eg: "apply --site vm"
	Why   string
	Args  []string // the command and its flags, without --config/--secrets/--execute/--sudo
	Founding bool  // an apply whose ErrFoundingWait is expected
}

func convergePlan(cfg *config.Config, st convergeState) []convergeStep
```

Rules: phase 0 `init` when `NeedsInit`; phase 1 `host prepare --site s` for every site; if no member of `cfg.Etcd.Members` is founded, phase "founding": witnesses among the members (in `etcd.members` order) then the other members, each `apply --site`, `Founding: true`; otherwise phase "joining": `site add s` for each unfounded member; phase "other sites": `apply --site s` for every site not in `etcd.members`, non-monitor sites in name order then monitor sites; phase "storage": none for no Garage sites, `storage init --site g` for one, `storage add` for several; phase "pass two": `apply --site s` for every site, non-monitor first then monitor; phase "dns": `dns init`.

- [ ] Tests (fixture `render/testdata/deployment.yaml`, etcd members `home-a, home-b, vm`, `vm` a witness, `watch` a monitor): blank (founding order `vm, home-a, home-b`; other sites `watch`; pass two all, `watch` last); founded except `home-b` (joining `site add home-b`, no founding phase); `NeedsInit` adds `init` first; Review Focus 1 and 2 (build the configs by editing the fixture's etcd members). Commit `feat: apply with no site plans the whole deployment`.

### Task 3: the run

**Files:** `cmd/paisans/converge.go`, `cmd/paisans/main.go` (`runApply` hands over when `--site` is empty), `cmd/paisans/converge_test.go`

- [ ] `var convergeRun = func(args []string) error` dispatching `init`, `host prepare`, `apply`, `site add`, `storage init`, `storage add`, `dns init` to their `run*`. `runConverge(args)`: flags `--config`, `--secrets`, `--execute`, `--sudo`, `--ssh` (refused); validate; `readConvergeState` (NeedsInit: no id, no subnet, no secrets file, or `secretsgen.Fill` on a copy changes it; Founded: `apply.ReadEtcdInitial` through `siteTransport` for each member, an unreachable one refusing); print the plan; without `--execute` end `Nothing changed. Re-run with --execute to apply.`; with it, run each step with `--config`, `--secrets`, `--execute`, `--sudo=<v>` appended, tolerate `ErrFoundingWait` on a `Founding` step, stop on any other error with `converge stopped at <title>: <err>. Run paisans apply --execute again to resume.`, and end `<config> is converged.`
- [ ] Tests with `convergeRun` replaced: order recorded; founding wait tolerated; a failure stops it; `--ssh` refused. Commit `feat: apply with no site runs the whole deployment`.

### Task 4: README and usage

- [ ] Usage line `paisans apply [--site <name>] ...`; README section on converging next to *A new deployment is applied witness first*, and the quick start's first lines. Commit `docs: apply converges the whole deployment`.

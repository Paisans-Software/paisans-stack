# Installer-style output, with `--verbose` for the detail

Date: 2026-10-09. Status: design approved by the founder in session; this
written spec awaits review. Every command's output is rewritten to read like
an installer: one short line per step, a result at the end, and the reasoning
and raw evidence kept for `--verbose`.

## Why

An apply today prints the plan, the reason for each action, every image
digest, every API request body, and the explanation behind each warning, and on
`--execute` with an identity step it prints the plan twice. All of it is
accurate, and most of it is noise on a run that works. An operator reading it
cannot find the line that matters: what is happening now, and whether it
worked.

The detail is still worth having when something is wrong or when the operator
wants to know why. It moves behind a flag rather than out of the toolkit.

## Scope

In: every command that prints, in one pull request into `develop` made of
reviewable commits.

* Every subcommand `main.go` dispatches: `validate`, `init`, `render`,
  `apply`, `host`, `site`, `secrets`, `app`, `oidc`, `ingress`, `storage`,
  `prune`, `preflight`, `failover`, `doctor` and `dns`, with their
  subcommands.
* The validate findings, which gain a short hint beside their explanation.
* `README.md` and `docs/development.md`, where they show or describe output.

Out:

* **What a command does.** Gates, ordering, refusals and the plan itself are
  unchanged. This is a change to what is said, not to what is done.
* **Machine-readable output** (Eg: `--json`). Nothing needs it yet.
* **Errors.** A failure always prints in full, at every verbosity: an operator
  must not have to re-run a failed apply to learn why it failed.

## The two levels

**Default.** One line per step, in installer voice: a verb and its object,
and at most a short result (`13.6 GiB free`, `4.1s`, `created`). No
rationale, no configuration values, no request bodies, no digests, no
sub-steps. Warnings print their hint; refusals print their hint and their
explanation; errors print everything.

**`--verbose` (`-v`).** Everything printed today, attached under the step it
belongs to, plus the raw output of the commands and API calls behind each step
(Eg: the host check's docker and ufw inventory, a compose pull's output). No
detail is lost relative to today's output.

## What a run looks like

Dry run:

```
paisans.yaml
  ! garage consistency is dangerous: reads can miss recent uploads while a site is behind
luthen-rael (ubuntu@192.0.2.10)
  ✓ host check              clean
  ✓ disk space              13.6 GiB free
  write 8 files
  create OIDC client uptime2
  start mesh psns-566c
  recreate infra
  recreate uptime2
  reload gateway
Nothing changed. Re-run with --execute to apply.
```

`--execute` skips the plan and reports progress only:

```
luthen-rael (ubuntu@192.0.2.10)
  ✓ host check              clean
  ✓ claim site
  ✓ disk space              13.6 GiB free
  ✓ OIDC client uptime2     created
  ✓ write 8 files
  ✓ start mesh psns-566c
  ✓ recreate infra          4.1s
  ✓ recreate uptime2        6.3s
  ✓ reload gateway
Applied 8 files to luthen-rael.
```

A refusal:

```
paisans.yaml
  ✗ witness shares a failure domain with its only voter
    home-a and home-b are both in rack-1, so losing the rack loses quorum.
    Put the witness in another failure domain.
paisans.yaml: 1 refusal. Nothing was changed.
```

## Line style

On a terminal: `✓` green for a finished step, `✗` red for a failed step or a
refusal, `!` yellow for a warning, and a spinner with the elapsed time while a
step runs. The spinner line is rewritten in place and replaced by the finished
line, so a pull that takes minutes shows that it is alive without filling the
screen.

When stdout is not a terminal, or `NO_COLOR` is set, or `TERM=dumb`: no colour,
no cursor movement, no spinner, and words instead of glyphs (`ok`, `FAIL`,
`WARN`). A step prints one line when it ends. Logs from cron or CI then read
cleanly.

The second column is aligned within a block, so results line up.

## The `internal/ui` package

One `Reporter` that every command writes through. Packages stop formatting
text themselves; they report what happened and the reporter decides how it
looks.

```go
type Reporter interface {
    Section(title string)                 // a site, "paisans.yaml", a stage
    Step(title string) Step               // starts a step; spinner on a TTY
    Item(title string)                    // a dry-run plan line, no outcome
    Warn(hint, detail string)             // detail shown only with --verbose
    Refuse(hint, explanation string)      // both shown always
    Detail(format string, args ...any)    // shown only with --verbose
    Trace(label, output string)           // raw command or API output, --verbose only
    Result(format string, args ...any)    // the closing line
}

type Step interface {
    Done(result string)                   // "", "4.1s", "created", "13.6 GiB free"
    Fail(err error)
    Detail(format string, args ...any)    // attached under this step, --verbose only
}
```

* `ui.New(w io.Writer, verbose bool)` detects the terminal and colour.
  `ui.Discard` reports nothing, for callers that want silence (as a nil
  `Progress` writer does today).
* `ui.Recorder` records calls as structured events, so tests assert on step
  titles, outcomes and which details were attached, not on spacing.
* A note written while a step is open (today's `Plan.say`) becomes a `Detail`
  of that step, so it never breaks the step's line.
* The step's elapsed time is the default result for any step that takes over
  a second and reports no result of its own.
* Writes are serialised by a mutex; the spinner runs on its own goroutine and
  is stopped by `Done` or `Fail`.

## The flag

`-v` and `--verbose` on every command, registered by one helper,
`commonFlags(fs)`, which each `run*` calls on its own `flag.FlagSet`. The
helper returns the reporter. Commands that print nothing worth hiding still
accept the flag, so an operator never has to remember which commands take it.

## Validate findings

`validate.Finding` gains `Hint` beside `Message`:

* `Hint` is one line, at most about 80 characters, saying what is wrong in an
  operator's words.
* `Message` keeps the explanation: why it matters and what to do, as today.

`c.warn` and `c.refuse` take `(rule, key, hint, format, args...)`, and all of
their call sites (about 90, in `validate.go`, `monitor.go`, `pocketid.go` and
`public_address.go`) get a hint written for them. The rule ID stays the
stable identifier tests assert on; it is shown only with `--verbose`.

The count line becomes `paisans.yaml: 1 warning` (plural only when needed),
and it is not printed when there are no findings at all.

## Plans and progress

* **`Plan.Progress io.Writer` becomes `Plan.Report ui.Reporter`**, and
  `apply.Step(w, ...)` becomes `Reporter.Step`. `Plan.step` and `Plan.say`
  route through it.
* **Step titles are short.** `recreate infra`, `reload gateway`, `write 8
  files`, `start mesh psns-566c`. `Action.Reason`, the `Describe()` texts of
  the disk and volume checks, image references and the OIDC request bodies
  become `Detail`s of their step.
* **A dry run** prints the plan as `Item`s and ends with `Nothing changed.
  Re-run with --execute to apply.`
* **`--execute` does not print the plan.** It prints progress only. With
  `--verbose` the full plan prints before executing, as today.
* **A second pass prints only its new steps.** `executeWithClients` builds the
  site twice; the second pass no longer reprints the plan, and planning reads
  (`reading N rendered files`, the image probe) are shown once per run.
* **Separate writes become one step.** Each file written is a `Detail` of
  `write N files`, not a line of its own.
* **Host check** is one step whose result is the class (`clean`, `shared`,
  `foreign`); the docker, firewall, foreign and note lines are its details. A
  `CONFLICT` refuses, and so prints in full.
* **The claim** is a step, `claim site`, with the registry path as its detail.
  It moves from stderr to the reporter.
* **Staged commands** (`site add`, `site remove`, `storage add`,
  `rotate-key`) print each stage as a `Section` and its work as steps, and a
  gate that passes is a step whose result is `passed`.

## Wording

Default-mode text is rewritten in installer voice: an imperative verb and its
object, no rationale, no first-person asides, no "that is the trade". Sentences
that explain why move to a `Detail`, a refusal's explanation, a code comment
or the README, whichever is the right home. Lines that only an engineer
debugging the toolkit would want move to `Trace`.

## stdout and stderr

Everything a run reports goes to stdout through the reporter, including
warnings and claims, which go to stderr today. Errors that end the command go
to stderr, as they do now. A command whose stdout carries data, where one
does, keeps that data alone on stdout and sends its report to stderr.

## Testing

* `internal/ui` has its own tests for both renderings (terminal and plain),
  verbose and default, the spinner being replaced by the finished line, and
  `NO_COLOR`.
* Tests that matched exact output text move to `ui.Recorder` and assert on
  events. Where a test is about wording a human reads (the refusal for a
  witness in a voter's failure domain, say), it asserts on the hint and the
  explanation, not on padding.
* A test per command checks that default output contains no line longer than
  about 100 characters on a fixture run, so the noise does not creep back.
* `go vet ./...` and `go test ./...` pass.

## Documentation

* `docs/development.md`, *Every step on the host is announced as it starts*,
  is rewritten for the reporter and the two levels.
* `README.md`'s sample output (the OIDC dry run, `app admin create`, the sudo
  prompt section's context) is updated to the new form.

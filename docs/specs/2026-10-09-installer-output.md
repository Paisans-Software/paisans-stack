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
* **What a failure says.** A failure always says what went wrong and what to
  do, at every verbosity: an operator must not have to re-run a failed apply
  to learn why it failed. How it says it is in scope; see *Errors*.

## The two levels

**Default.** One line per step, in installer voice: a verb and its object,
and at most a short result (`13.6 GiB free`, `4.1s`, `created`). No
rationale, no configuration values, no request bodies, no digests, no
sub-steps. Warnings print their hint; refusals print their hint and their
explanation; an error prints its hint and explanation (see *Errors*).

**`--verbose` (`-v`).** Everything printed today, attached under the step it
belongs to, plus the raw output of the commands and API calls behind each step
(Eg: the host check's docker and ufw inventory, a compose pull's output). No
detail is lost relative to today's output.

## What a run looks like

Dry run:

```
paisans.yaml
  ! garage consistency is dangerous: an upload is confirmed even if only 1 of 2 copies is written
luthen-rael (ubuntu@192.0.2.10)
  ✓ host check: clean
  ✓ disk space: 13.6 GiB free
  · write 8 files
  · create OIDC client uptime2
  · start mesh psns-566c
  · recreate infra
  · recreate uptime2
  · reload gateway
Nothing changed. Re-run with --execute to apply.
```

`--execute` skips the plan and reports progress only:

```
luthen-rael (ubuntu@192.0.2.10)
  ✓ host check: clean
  ✓ claim site
  ✓ disk space: 13.6 GiB free
  ✓ OIDC client uptime2: created
  ✓ write 8 files
  ✓ start mesh psns-566c
  ✓ recreate infra: 4.1s
  ✓ recreate uptime2: 6.3s
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
refusal, `!` yellow for a warning, `○` yellow for a step a dry run found
pending, a dim `·` line for a step that waits on an earlier one, and a spinner with the elapsed time while a
step runs. The spinner line is rewritten in place and replaced by the finished
line, so a pull that takes minutes shows that it is alive without filling the
screen.

When stdout is not a terminal, or `NO_COLOR` is set, or `TERM=dumb`: no colour,
no cursor movement, no spinner, and words instead of glyphs (`ok`, `FAIL`,
`WARN`, `todo` for pending, `wait` for waiting). A step prints one line when it ends. Logs from cron or CI then read
cleanly.

A step's result follows its title after a colon, `✓ disk space: 13.6 GiB free`,
with no column to pad to, so a short title is not followed by a run of spaces
sized to a long one.

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
    End(m Mark, result string)            // ends with OK, Failed, Pending or Waiting, and a result
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
  is stopped by `Done`, `Fail` or `End`.

## Errors

The error that ends a command is its last word, and it prints in the same
visual language as a refusal, on stderr:

```
✗ secrets.enc.yaml cannot be decrypted: no usable age key was found
  Set SOPS_AGE_KEY_CMD to a command that prints your key, Eg: a Keychain lookup,
  so the key is never stored in a file or the environment. SOPS_AGE_KEY_FILE
  (its path), SOPS_AGE_KEY (the key itself) and sops/age/keys.txt in your config
  directory are read too. Your public key must be a recipient in .sops.yaml; if
  it was added recently, run sops updatekeys on the file.
```

* The first line is the mark and a hint: `✗` red on a terminal, `FAIL` off
  one, decided for stderr as the report's are for stdout. The mark is what a
  script recognises; there is no `paisans: ` prefix.
* The explanation follows, indented two spaces and word-wrapped.
* With `-v` the cause follows the explanation, one indented line per link of
  the chain: the context each caller added, then the underlying error's own
  text (sops', ssh's, the API's), with full paths.
* Exit codes are unchanged: 1 for a failure, 2 for usage, 75 for a wait on
  Garage. Each is decided with `errors.Is` on the error, so wrapping one in a
  `ui.Problem` changes nothing about it.

### `ui.Problem`

An error a user is likely to meet is a `ui.Problem`:

```go
type Problem struct {
    Hint    string // one line, at most 80 characters: what is wrong
    Explain string // what to do, and the reason when the hint is not enough
    Cause   error  // the underlying error, shown only with -v
}
```

* `Hint` is one line, at most about 80 characters, naming the problem in an
  operator's words rather than the mechanism that found it (`no usable age key
  was found`, not `0 successful groups required`).
* `Explain` is what to do. Everything an operator needs to act is here or in
  the hint, never only in the cause, since the cause is hidden by default.
  A list (each conflict a host check found, each problem in a file) is one
  line per item, each starting `- `.
* `Cause` is what the problem was found from. `Unwrap` returns it, so
  `errors.Is` and `errors.As` see through a `Problem` to a sentinel such as
  `apply.ErrUnreachable` or `storageadd.ErrWaiting`.
* `Error()` is the hint, the cause's text and the explanation, so a caller
  that logs or embeds an error loses nothing.
* A typed error that has its own wording (`storageadd.Waiting`,
  `config.LoadError`) unwraps to a `Problem`, so the printer finds it and the
  type stays for callers that ask for it with `errors.As`.
* The printer uses the outermost `Problem` in the chain. Context a caller
  added around it (`apply: `) shows with `-v`. A step that stops a staged
  command (`apply stopped at <step>`) makes its own `Problem` whose
  explanation carries the inner problem's hint and explanation, then how to
  resume.

**Any other error** prints in full, in the same form: its first line is the
hint, or, when the first line is longer than 80 characters, the text up to its
first `. `. The rest is the explanation, wrapped. This is the fallback; an
error an operator meets often earns a `Problem`.

### Wrapping

Prose wraps at the terminal's width when it is known, at most 80 columns, and
at 80 when it is not: the final error's explanation, a refusal's explanation,
a note's detail, and a warning's detail under `-v`. Lines break only at spaces,
so a path, a command or a URL is never split; a token longer than the width
gets a line of its own. A line that fits is left as written, so aligned
columns keep their spacing. A line keeps its indentation when wrapped, and a list
item's continuation lines align under its text after `- `. Step lines, details
and traces are not wrapped: a trace is a command's own output, and a step line
is short by design. A warning's or a refusal's hint is one line,
whole, unless the terminal is known to be narrower than that line: then it
wraps at spaces, its continuation lines aligned under the hint's text, rather
than being broken mid-word by the terminal. Output whose width is not known,
a log for one, keeps it whole.

### Paths

In default output a path is shown relative to the current directory when it is
under it, and otherwise as given; `$HOME` shortens to `~` in text a human
reads. `ui.ShortPath` does both. A command meant to be copied never gets `~`,
and gets the path as given. With `-v` the full path appears: a `Problem`'s
hint names the short form, and its cause, shown with `-v`, carries the path as
the toolkit used it.

## The flag

`-v` and `--verbose` on every command, registered by one helper,
`commonFlags(fs)`, which each `run*` calls on its own `flag.FlagSet`. The
helper returns the reporter. Commands that print nothing worth hiding still
accept the flag, so an operator never has to remember which commands take it.

## Validate findings

`validate.Finding` gains `Hint` beside `Message`:

* `Hint` is one line, at most 100 characters, saying what is wrong in an
  operator's words. Most are far shorter; the cap leaves room for a hint that
  must name a number and its consequence in the same sentence to be accurate.
* `Message` keeps the explanation: why it matters and what to do, as today.

`c.warn` and `c.refuse` take `(rule, key, hint, format, args...)`, and all of
their call sites (about 90, in `validate.go`, `monitor.go`, `pocketid.go` and
`public_address.go`) get a hint written for them. The rule ID stays the
stable identifier tests assert on; it is shown only with `--verbose`.

Findings print under a section that says what they are about, then a count
line, `paisans.yaml: 1 refusal, 1 warning` (plural only when needed):

* `paisans validate` prints them under the file's path, since the file is its
  subject, and always ends with the count line, or with `no problems found`.
* Every other command prints them under the file's path and prints the count
  line only when there is a refusal, since the command then stops on it. A
  warning-only count would name the file a second time and say nothing the
  lines above it do not.
* `paisans apply` without `--site` prints them under its `configuration`
  section, the one its `init` step belongs to: the header once, the findings,
  then `init` when it has work. When `--execute` has just run `init`, init
  has reported them, under the file's path, and they are not repeated. The path is not a section there, and appears
  only in the count line of a refusal.
* Nothing is printed for a file without findings.

## Plans and progress

* **`Plan.Progress io.Writer` becomes `Plan.Report ui.Reporter`**, and
  `apply.Step(w, ...)` becomes `Reporter.Step`. `Plan.step` and `Plan.say`
  route through it.
* **Step titles are short.** `recreate infra`, `reload gateway`, `write 8
  files`, `start mesh psns-566c`. `Action.Reason`, the `Describe()` texts of
  the disk and volume checks, image references and the OIDC request bodies
  become `Detail`s of their step.
* **A dry run** prints the plan as `Item`s and ends with `Nothing changed.
  Re-run with --execute to apply.` The dry run of `apply` without `--site`
  changes nothing on the servers but may add missing generated secrets to the
  local secrets file; when it does, its closing line says nothing changed on
  the servers and how many secrets it added to which file, and its `init`
  line names them (see the converge spec).
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

## Prompts

A question asked on the terminal holds the reporter's drawing while it waits
(`ui.Hold`), so the spinner cannot erase it.

* **The sudo password prompt is erased once it is answered**, right or wrong,
  on a terminal: the newline that ends it, a carriage return, then for each
  row the prompt took a cursor up and a line clear. The rows are the prompt's
  length over the terminal's width, rounded up, so a prompt that wrapped is
  erased whole. Nothing else is
  written, so the step lines go on directly under their section header, and
  the open step's spinner redraws on the row the prompt took. A password that
  was refused, or could not be read, is explained by the error that follows.
  Off a terminal nothing is written, and a terminal that is not drawn on
  (`TERM=dumb`, `NO_COLOR`) keeps the prompt, followed by a blank line. So
  does a terminal whose stdout or stderr is piped (`paisans apply | tee log`):
  the report then reaches the terminal through another process at its own
  pace, so the rows above the cursor are not known to be the prompt's.
* **ssh's question about a host key is followed by a blank line**, since it is
  ssh's own text, over several lines, and cannot be erased reliably. The blank
  line is written before the drawing resumes, so the spinner redraws below it
  rather than leaving a frame behind.

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
* The error printer is tested in both renderings, with and without `-v`, for
  a `Problem` and for the fallback; wrapping is tested never to split a path
  or a command; and the common errors are tested end to end through the
  printer, with the exit code each gets.
* `go vet ./...` and `go test ./...` pass.

## Documentation

* `docs/development.md`, *Every step on the host is announced as it starts*,
  is rewritten for the reporter and the two levels.
* `README.md`'s sample output (the OIDC dry run, `app admin create`, the sudo
  prompt section's context) is updated to the new form.

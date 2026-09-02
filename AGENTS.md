# AGENTS.md

Directives for an agent working in this repository. Read this before the first
edit, not after.

`AGENTS.md` and `CLAUDE.md` are the same instructions; if both exist, this file
is the source and the other is a pointer to it.

## What this repository is

A toolkit for installing and operating a paisans community stack, plus the
design that toolkit follows. **Nothing is implemented yet.** Today the
repository holds `README.md`, `docs/`, and `examples/`, with no code, no tests,
no build and no release.

That matters for scope. Most work here is *documentation of a design*, and the
design is argued rather than asserted: every rule in `README.md` states the
alternative it rejects and why. Match that. A change that adds a rule without
the reasoning behind it is incomplete, no matter how correct the rule is.

This is also **not** the `paisans.community` deployment. Generic material is
canonical here; a specific community's hosts, decision log and governance stay
with that community. See *Where documentation lives* in `README.md`.

## `docs/deployment-agent-rules.md` is not your rulebook

This file is your rulebook. `docs/deployment-agent-rules.md` is a **shipped
artifact**, and it governs a different agent: one operating a *deployed* paisans
stack, with containers to stop, member data to touch, and a live identity
provider. Its blast-radius tiers describe actions that do not exist here.

You are editing a repository. Nothing you do here reaches a running deployment,
because this repository has no deployment attached to it. Do not read its
"requires a human present" tiers as constraints on your edits, and do not
announce that you are "operating from the agent rules template". That sentence
belongs to a recovery, not to a documentation change.

You do still **maintain** that file, and edits to it carry more weight than
ordinary documentation, because the wording becomes an operational rule on
someone else's machine at the worst moment they will ever have. Three
consequences:

* **A tier change is a human decision.** Moving an action between free,
  approved, and present alters what an operator's agent is permitted to do
  during an outage. Propose it; do not decide it.
* **It must stay generic.** No hosts, no products, no community's particulars.
  It is a floor that any community can adopt and edit.
* **It is a rulebook, not a Charter.** A community's Charter is its mission,
  values and code of conduct, it lives in that community's wiki, and it governs
  these rules. Do not let values, mission statements or governance drift into
  this file; they are not generic and they are not ours to write.

## What you decide, and what the human decides

Here, in this repository:

* **You decide alone:** file layout, document structure, wording, naming,
  which runbook to write, how to organise a section.
* **The human decides:** which software fills a capability; anything changing
  the identity model or federation policy; anything that locks in a migration
  path; whether a design rule is adopted at all; the content of the deployment
  agent rules' tiers; anything published or released.

Researching two candidate applications and recommending one is your work.
Choosing between them is not.

The one hard line that is genuinely dangerous here is **publishing**: a push to
`main`, a release, or anything leaving this repository. That is where a mistake
becomes irreversible, and it is the human's action.

## What must never enter this repository

The `.gitignore` encodes this and you should understand it rather than trust it.

* **No real hostnames, addresses, or a deployment's shape.** Examples use
  `example.org`, `home-a`, `home-b`, `vm`. Publishing a live deployment's
  topology here is the leak the repository boundary exists to prevent.
* **No secrets in any form.** Not a decrypted `secrets.yaml`, not a `.env`, not
  a key, not a token, not "just an example one that is already rotated". This is
  the one mistake with no undo once pushed.
* **No member data.** Ever, under any framing.

If a change requires naming a real host to be understandable, the change is
wrong; generalise it.

## Making a change

1. **Branch.** Never commit directly to `main` unless the human says so in this
   session. Name it for the change: `docs/backup-contract`, `feat/site-add`.
2. **Read the surrounding argument first.** `README.md` is one continuous line
   of reasoning. A new rule that contradicts rule 2 four sections later is the
   most common way to damage this document.
3. **Make the change small and self-contained.** One decision per commit.
4. **Verify** (see below).
5. **Commit** in the house style (see below).
6. **Open a pull request**, or hand the branch to the human. Do not merge your
   own work to `main` without approval.

### Documentation changes specifically

* Keep tables as tables and prose as prose; the document uses each deliberately.
* State the rejected alternative. "X, because Y" is the minimum; "X. It would
  be simpler to Z, but Z is a migration rather than an addition" is the voice.
* Cross-reference by section name, not by line number.
* **No em dashes in anything you write.** Use a comma, a semicolon, a colon, a
  full stop, or a real parenthetical instead. Where the aside is an
  illustration, make it an explicit one (Eg: `cluster`, `pinned` or a mix of
  both). This applies to every word you produce here: documentation, commit
  messages, pull request descriptions and code comments alike.
  Existing text predates the rule and is full of them. Do not sweep it. Fix
  em dashes only in lines your change already touches for another reason.
* British/American spelling is mixed in the existing text; do not sweep it.
  Spelling normalisation is not a change worth a commit here.
* **Deleting documentation requires explicit human approval.** Rewriting a
  section in place is an edit; removing it is a deletion.

### Code changes, when code exists

You are *authoring* the tool that other agents and operators will run against
live deployments. You are not running it. The care goes into what the code
permits, because the blast radius lands on someone else's stack.

None of this is exercised yet. When the first scripts land, these hold:

* **A pinned stack lays out under `/srv/<stack>/` with bind mounts**, not named
  volumes. `README.md` explains why (`app move` becomes one `tar`).
* **`.env` files are build artifacts.** Nothing hand-edits them; `apply` renders
  them and refuses to clobber local modifications.
* **`sops`/`age` run on the workstation only.** No code path may put a
  decryption key on a host, and no code path may decrypt on a host, unless the
  human has explicitly chosen that mode.
* **Anything that mutates a live deployment is staged and gated**, and every
  gate stops on failure rather than warning and continuing. `site add` is the
  worked example: the stage-2 handshake gate exists because stage 3 can take a
  healthy cluster down.
* **Destructive and topology-changing operations are idempotent and
  resumable.** Re-running after a fixed network problem must resume.
* **Refuse rather than warn** where the configuration is incoherent (a witness
  sharing a failure domain with its only voter). **Warn rather than refuse**
  where it is merely risky (a pinned app on the witness host). Do not blur these.

## Verifying before you claim it is done

There is no test suite. Verification is therefore manual and you must actually
do it, not assert it:

* `git diff` the full change and read it.
* Every internal link resolves: check `docs/` and `examples/` paths exist.
* Every fenced YAML block parses. Every command example uses the flags the
  document defines elsewhere.
* No real hostname, address, key or secret appears in the diff. Grep the diff,
  do not eyeball it.
* Claims about upstream software (S3 support, environment variable names,
  known bugs) are cited or verified. `README.md` makes specific factual claims
  about Mbin, Outline, Synapse, Pocket ID and WriteFreely. Do not add one from
  memory.

When code exists, add: it runs; the linter passes; the test you wrote fails
before your change and passes after.

**Report what you actually ran.** If a check was skipped, say which and why.

## Commits

Format follows the existing log exactly. Read `git log` before writing one.

```
<type>: <subject, lowercase, imperative, no trailing period>

<body: what changed and, more importantly, why. Name the alternative that was
rejected. Wrap at 80 columns. Attribute the decision when a human made it
(Eg: "Founder decision." or "Founder direction."), so the log records who
chose.>

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
```

Types in use: `docs`. Add `feat`, `fix`, `refactor`, `chore` when code arrives.

The bodies here are long on purpose. This log is a decision record; a subject
line alone throws away the part that will be needed in a year.

Never commit without the human asking, and never `push` or `git commit --amend`
on a shared branch unasked.

## Pull requests

* One decision per PR, same as commits.
* The description explains the reasoning; the diff shows the change.
* End the description with:

  ```
  🤖 Generated with [Claude Code](https://claude.com/claude-code)
  ```
* Do not merge your own PR without approval.

## Releases

**There have been none. No tags exist, and no versioning scheme is settled.**
The shape below is provisional and needs a founder decision before the first
release; do not treat it as ratified, and do not cut a release on your own
initiative under any circumstances.

When it is settled, the expectation is:

* **Semantic versioning on git tags** (`v0.1.0`), with `0.x` while command
  names remain provisional (`README.md` says they are).
* **`CHANGELOG.md` written by hand** from the commit bodies, grouped by what an
  operator has to *do*, not by commit type. The categories that matter to this
  project: changes requiring an `apply`, changes requiring a config edit,
  changes affecting an existing deployment's data or topology, and everything
  else.
* **A migration note for anything that changes an existing deployment.** This
  toolkit's entire design premise is that additions must not become migrations;
  a release that quietly requires one has broken the premise and must say so at
  the top.
* **Release is a human action.** Tagging and publishing leave this repository
  and cannot be walked back. Prepare the changelog and the exact tag command;
  let the human run it.

## When you are unsure

Ask, and keep working on the part that does not depend on the answer.

The failure mode to avoid here is not recklessness with a deployment, since you
have no deployment to be reckless with. It is **writing something into this
repository that reads as settled when it was your inference**: a design rule the
founder never adopted, a factual claim about upstream software taken from
memory, or a blast-radius tier you moved because it seemed reasonable. Those
travel.
Mark what is provisional as provisional.

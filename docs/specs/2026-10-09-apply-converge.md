# `paisans apply` converges the whole deployment

Date: 2026-10-09. Status: approved by the founder in session.

Bringing a deployment up from `paisans.yaml` takes a dozen commands in an order
the operator has to know: `init`, `host prepare` for every site, `apply` for
the witness first and then the data sites, one of them twice, `storage init`
or `storage add`, `dns init`. Nothing runs them in that order, and a first
install that stops halfway leaves the operator to work out which of them is
left.

`paisans apply` with no `--site` does it: it reads the deployment and its
hosts, works out what each needs, and runs the existing commands in the order
the deployment needs them, stopping at the first failure. Run again, it reads
live state again and carries on, so the same command is the first install,
the resume after a failure, and every later change. `apply --site <name>` and
every other command are unchanged, for building a deployment piece by piece.

## Scope

In: one command that takes `paisans.yaml` to a complete, running stack, and
keeps it there, for every site and app it declares.

Out:

* **Taking anything out.** A site or an app the yaml no longer declares but a
  host still runs is reported, with the command that removes it (`site
  remove`, `app remove`); nothing is removed. A yaml edited by mistake must
  never take a running site down.
* **What the operator alone can decide.** The first Pocket ID admin
  (`app admin create`), a credential the toolkit cannot generate (the DNS
  provider's token, an SMTP password: `secrets set`), a DNS record that points
  somewhere else (changed at the provider by hand). Each is named, with its
  command; the first admin only while it is actionable (see The first Pocket
  ID admin).

## The plan

| Phase | Step | When |
|---|---|---|
| 0. Configuration | `init`: the deployment id, the mesh subnet, every generated secret | the yaml has no id or no subnet, or the secrets file is missing or lacks a generated secret; once it has run, the rest is read and planned again |
| 1. Hosts | `host prepare --site <s>` | every site; a prepared host plans nothing |
| 2. Founding | `apply --site <w>` for each witness in `etcd.members`, then `apply --site <d>` for each other member, of those still being founded; a monitor among them too, since etcd needs it | a member holds no `infra/etcd-initial`, and either no member holds one or one's `--initial-cluster` lists it: the founding set is fixed by the first apply that wrote a record |
| 2. Joining | `site add <s>` | a member holds no record and no record lists it: the cluster was founded without it |
| 3. Other sites | `apply --site <s>` for every site not in `etcd.members`, monitor sites last | every such site; an applied site plans nothing |
| 4. Storage | `storage init --site <g>` for one Garage site; `storage add` for several | `storage.garage.sites` is not empty |
| 5. Pass two | `apply --site <s>` for every site again, monitor sites last, a monitor in `etcd.members` among them | always: it resumes the data site the founding stop left, and moves what an earlier step changed, such as the keys storage made |
| 6. DNS | `dns init` | always; it creates only what is missing, and stops on a record pointing elsewhere |

A data site's apply that stops at the founding stop (its etcd is up and
another founding member's is not) is not a failure in phase 2: pass two
applies it again. Any other failure stops the run, naming the step and the
command, with `Run paisans apply --execute again to resume.` Two stops get the
hint that fits them instead, since running again alone does not get past
them: a DNS record pointing elsewhere is changed at the provider by hand
first, and a `storage add` gate waiting on Garage to finish moving data is not
a failure, so the run exits with status 75 as `storage add` does, to be run
again later.

Each step runs the existing command in the same process, with the flags the
plan sets and those of apply's that reach it (`-v` every step, `--keep-images`
and `--min-free` each `apply`), so it plans, gates, claims, reports and refuses exactly as typed by
hand would, and asks each host's sudo password once for the whole run.

## Dry run

Without `--execute` it changes nothing and prints the plan, phase by phase,
one line per step with the step's status:

| Mark | Status | Line |
|---|---|---|
| `✓` green | up to date: its own dry run finds nothing to do | the title |
| `○` yellow | pending: `--execute` would change something | the title and what, Eg: `3 changes`, `2 records to create` |
| `·` dim | waiting: it cannot be planned until an earlier step has run | the title and that step, Eg: `after host prepare --site home-a` |
| `✗` red | the check failed, Eg: a host did not answer | the title and the error, on one line; the dry run carries on with the rest |

Each status comes from the step's own dry run, run in this process with
nothing it prints shown, which says how much it would change. That dry run is
the same read only path `paisans <command>` without `--execute` takes, so a
check changes nothing on a host. With `-v` each line is followed by why the
step is in the plan, and by what its check reported: its warnings, its
refusal's explanation, and its ssh retries. A note the check reported,
something left for the operator, follows the line at every verbosity. The
detail of a pending step is its own dry run, Eg:
`paisans apply --site <s>`, which the closing line names.

A step is checked only once every step it waits on is up to date, since until
then its own dry run would describe a host that is about to change:

| Step | Waits on |
|---|---|
| `init` | nothing; its status is what init has to do |
| every other step | `init`, when init has work: it writes the subnet and the secrets they read |
| `host prepare --site <s>` | nothing else |
| `apply --site <s>` | `host prepare --site <s>`; a founding member that is not a witness, each founding witness's apply too, since its gate refuses one founded before the witness; in pass two, the site's earlier `apply` or `site add` and the storage step |
| `site add <s>` | `host prepare --site <s>` |
| `storage init`, `storage add` | the host prepare and the first `apply` or `site add` of each Garage site: Garage has to be running to be asked |
| `dns init` | nothing else: it reads the configuration and the provider, never a host |

A command is checked once: an `apply` in pass two takes the status its
site's earlier `apply` was checked to have. This reads every host, each step
through its own dry run.

With no deployment id yet it prints `init` alone as pending, since nothing on
a host can be read for a deployment that has no id. Both a dry run and a run
end by naming what only the operator can do, the first Pocket ID admin as the
next section says.

## The first Pocket ID admin

A credential the toolkit cannot generate is named whenever it is owed. The
first Pocket ID admin is named only when it is actionable, for each
`pocket-id` app:

* **Dry run.** When any step that brings the app up is not `✓` (`init`, or a
  step about a site the app runs on: its `host prepare`, its `apply` in
  founding, other sites or pass two, its `site add`), nothing is named: the
  app is not running yet, and the plan already says so. Since pass two waits
  on the storage step, a storage step that is not `✓` names nothing either.
  When every one is `✓`, the app should be running, and it is asked.
* **Run.** Once every step has run, the app is asked. A run that stopped
  names nothing.

Asking is a step of its own, `check <app> for an admin`, under a `pocket id`
section, so a spinner shows while it works. It is read only: it lists Pocket
ID's users through its API, with the API key, the site and the API address
`app admin create` uses, reaching the host as every step of the run does,
with its sudo, and stops at the first that is an admin. Then:

| Answer | Named |
|---|---|
| no user is an admin | `Pocket ID <app> has no admin yet`, with the `app admin create` command |
| a user is an admin | nothing |
| the check failed, Eg: the API did not answer, or the key is missing | `Pocket ID <app>: could not check for an admin (<why, on one line>)`, with the `app admin create` command |

A failed check does not fail the run or the dry run: it is about what is
left for the operator, not about a step. No key, token or secret is printed.

## Progress

Validation findings print first. Then each etcd member's founding record is
read, one step per member, `read <s>'s etcd record`, so a spinner shows while
ssh works; then the plan. A sudo password prompt or an ssh host key question
pauses the spinner while it waits on the operator, during these reads,
during each step's check, and during the check for a Pocket ID admin.

## Refusals

| Refused | Because |
|---|---|
| a configuration `validate` refuses | as every command |
| a secrets file that is there and does not decrypt, or whose missing secrets cannot be generated | init needs the same age key, so nothing is planned and no host is read; the error says where the key is looked for |
| a host that does not answer, while deciding the plan | founding or joining cannot be decided |
| `--ssh` | a destination names one host, and this reaches all of them; use `apply --site <s> --ssh` |

## Testing

The plan is a pure function of what was read, tested without a host: a blank
deployment; one founded (joining, not founding); a member not yet founded
while the others are (site add); no Garage site; one Garage site and several;
monitor sites last in phases 3 and 5, and a monitor in `etcd.members` founded
with the members and last in phase 5; init needed or not. Each is compared
with the whole plan, in order.

The run, with its steps replaced by fakes: steps run in order; the founding
stop in phase 2 does not stop the run and the site is applied again in pass
two; any other failure stops it with the resume hint, running nothing after,
and a DNS conflict or a wait on Garage with its own hint. The dry run, with
each step's check replaced by a fake: an up to date step `✓`, a pending one
`○` with its summary, a step waiting on an earlier one `·` naming it, a check
that fails `✗` with the rest still checked, and each etcd record read inside an
open step. The first Pocket ID admin, with the check replaced by a fake: a
dry run whose Pocket ID steps are not all `✓` names nothing and asks nothing;
one whose steps are all `✓` names it when there is no admin, nothing when
there is one, and that it could not check when the check fails; a run that
completed names it when there is no admin, and one that stopped names nothing.
The check itself, against a fake Pocket ID: read only, with the key and the
run's sudo, and a failure said on one line. Every step of a real
plan, with the flags the run adds, is put through its command's own flag
parsing, stopped before the command does anything.

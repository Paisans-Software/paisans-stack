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
  command.

## The plan

| Phase | Step | When |
|---|---|---|
| 0. Configuration | `init`: the deployment id, the mesh subnet, every generated secret | the yaml has no id or no subnet, or the secrets file is missing or lacks a generated secret |
| 1. Hosts | `host prepare --site <s>` | every site; a prepared host plans nothing |
| 2. Founding | `apply --site <w>` for each witness in `etcd.members`, then `apply --site <d>` for each other member | no member of `etcd.members` has been founded (none holds `infra/etcd-initial`) |
| 2. Joining | `site add <s>` | the cluster has been founded and a member of `etcd.members` has not |
| 3. Other sites | `apply --site <s>` for every site not in `etcd.members`, monitor sites last | every such site; an applied site plans nothing |
| 4. Storage | `storage init --site <g>` for one Garage site; `storage add` for several | `storage.garage.sites` is not empty |
| 5. Pass two | `apply --site <s>` for every site again, monitor sites last | always: it resumes the data site the founding stop left, and moves what an earlier step changed, such as the keys storage made |
| 6. DNS | `dns init` | always; it creates only what is missing, and stops on a record pointing elsewhere |

A data site's apply that stops at the founding stop (its etcd is up and
another founding member's is not) is not a failure in phase 2: pass two
applies it again. Any other failure stops the run, naming the step and the
command, with `Run paisans apply --execute again to resume.`

Each step runs the existing command in the same process, with the flags the
plan sets, so it plans, gates, claims, reports and refuses exactly as typed by
hand would, and asks each host's sudo password once for the whole run.

## Dry run

Without `--execute` it prints the plan, phase by phase, one line per step with
why it is there, and changes nothing. It reads what decides the plan (whether
init has work, which etcd members have been founded) and no more: a step's own
detail needs the steps before it to have run (a host prepared before its files
can be planned, Garage running before its keys can), so it is the step's own
dry run, `paisans <command> --site <s>`, that shows it.

## Refusals

| Refused | Because |
|---|---|
| a configuration `validate` refuses | as every command |
| a host that does not answer, while deciding the plan | founding or joining cannot be decided |
| `--ssh` | a destination names one host, and this reaches all of them; use `apply --site <s> --ssh` |

## Testing

The plan is a pure function of what was read, tested without a host: a blank
deployment; one founded (joining, not founding); a member not yet founded
while the others are (site add); no Garage site; one Garage site and several;
monitor sites last in phases 3 and 5; init needed or not.

The run, with its steps replaced by fakes: steps run in order; the founding
stop in phase 2 does not stop the run and the site is applied again in pass
two; any other failure stops it with the resume hint, running nothing after.

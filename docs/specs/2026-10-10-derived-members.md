# `etcd.members` and `cluster.sites` follow from the roles

Date: 2026-10-10. Status: approved by the founder in session.

A site's roles already say what it does: `data` runs Patroni and stores the
database, `witness` runs an etcd member and nothing else. `etcd.members` and
`cluster.sites` then said the same thing a second time, and `validate` refused
the file whenever the two copies disagreed (`data-site-not-in-cluster`,
`cluster-site-without-data-role`). Adding a data site meant three edits that
had to agree. Both keys are now optional: left out, each is derived from the
roles; written, it is used exactly as written.

## Scope

In: `etcd.members` and `cluster.sites`, the commands that read them, the two
yaml writers that edit them in place, `validate`, the example and the README.

Out:

* **`storage.garage.sites`.** It is a preference order, not a set: its first
  site is the node every app writes through, and a new site joins at the end.
  Roles carry no order, so nothing can be derived from them.
* **Converting a file.** A configuration that declares either key keeps working
  as it is, and nothing rewrites it.

## Rules

| Key | Left out | Written |
|---|---|---|
| `etcd.members` | every site with the `witness` role, in name order, then every site with the `data` role, in name order | used exactly as written |
| `cluster.sites` | every site with the `data` role, in name order | used exactly as written |

Witnesses come first because that is the order a new deployment is founded in
(README, *A new deployment is applied witness first*). Nothing reads either
list for its order otherwise: renders sort it.

The lists are resolved when the file is loaded, into the same fields a written
key fills, and the configuration records which of them were derived. Every
command reads the field, so there is one list, and no command can see the
written key's absence as an empty list. The record is for the few places where
a written list and a derived one must be told apart:

* `validate`'s comparisons between `cluster.sites` and the `data` role, which a
  derived list satisfies by construction;
* the wording of a finding about a derived list;
* the two yaml writers, which edit a key only when the file has it.

A key is written when the file has it at all. `etcd.members: []`,
`etcd.members:` with no value or `~`, and the same for `cluster.sites`, are
written and empty, and are refused (below). A list behind a yaml merge key
counts as written too: a list the file gives is never replaced by a derived
one.

A site holding both `data` and `witness`, which `validate` refuses, is one
derived voter, listed as a data site, so no quorum sum counts it twice.

## Refusals

| Refused | Because | What to do |
|---|---|---|
| `etcd.members` written and empty | no site would run etcd, so Patroni on every data site has nowhere to keep its leader key | remove the key so it follows the roles, or list at least one site |
| `cluster.sites` written and empty | with a data site it is a replica nothing routes to, which `validate` refuses anyway; without one it says nothing a left out key does not | remove the key, or list the data sites |

Both are refused when the file is loaded, with every other structural problem,
since each is a shape the file cannot mean rather than a policy.

## `validate`

Every rule still runs, on the list each command reads.

* `data-site-not-in-cluster` and `cluster-site-without-data-role` run only on a
  written `cluster.sites`.
* `undeclared-site` runs on each written list. A derived list names only
  declared sites.
* A finding about a derived list names its key as `etcd.members (derived from
  the roles)` or `cluster.sites (derived from the roles)`, so an operator who
  never wrote the key is not sent looking for it.
* `two-etcd-voters` on a derived list says what to do in the file's own terms:
  give a site in a third location, one that fails independently of both, the
  `witness` role, or write `etcd.members: [<one site>]` to run one voter, which
  gives up automatic failover. When the two are one data site and a witness,
  the witness has no tie to break, and the message says to take its role off.
* `even-etcd-voters` on a derived list says the same in its terms. With a
  witness among the voters: take its role off before the deployment is
  founded, take it out with `paisans site remove` once it runs, or write
  `etcd.members`. With none: a witness in a third location makes the count
  odd, or write `etcd.members`.
* `site remove`'s refusal of two remaining data voters, on a derived list,
  says to give a site in a third location the witness role first.

## The yaml writers

`site remove`'s last stage edits `paisans.yaml` in place (`RemoveSite`,
`RemoveSiteAndWitness`). Each edits `etcd.members` and `cluster.sites` only when
the file has the key. When it does not, the site's block going, or the
`witness` role coming off the witness, is the whole edit, and the derived lists
follow from it. The end state `site remove` validates and renders from is the
same either way: a derived list without the site is the written list without
it.

No other command writes either key. `init` writes the id, the mesh subnet and
addresses; `site add` and `apply` read the file and never write it.

## Converge and `site add`

`paisans apply` without `--site` plans from the list it reads, so a site given
the `data` role on a founded cluster is a derived member with no founding record
that no record lists, and is planned as `site add`, as it would be had it been
written into `etcd.members` by hand.

`site add` refuses a new site that a written `cluster.sites` or `etcd.members`
leaves out. With the key left out it cannot, since a `data` site is in both.
The refusal names the key as written and that removing it derives the list.

## Example and README

`examples/paisans.example.yaml` leaves both keys out, with a comment saying
they are derived and when to write them: one voter by choice, or fewer voters
than data sites. The README's configuration sections, *Adding a site*, *The
witness is a third location*, *`cluster.sites` and the `data` role name the
same sites* and the converge section say the same.

## Testing

* Loading: both keys left out derive, in the order above; written keys are kept
  exactly, including an order other than the derived one; each written and
  empty, or written with no value, is refused; the record of which were
  derived is right for each case.
* `validate`: a valid file with both keys left out has no refusal; the two
  `cluster.sites` comparisons do not fire on a derived list and still fire on a
  written one; `two-etcd-voters` on a derived list fires with the witness and
  the one voter advice; a finding about a derived list names it as derived;
  `undeclared-site` still fires on a written list. The existing fixtures, which
  write both keys, test the override and are kept.
* Writers: `RemoveSite` and `RemoveSiteAndWitness` on a file without the keys
  edit only the site's block and the witness's roles, and the file loads with
  the lists derived as expected; on a file with them, as before.
* `site remove`: the end state of a file without the keys matches that of the
  same file with them written out.
* Converge: a site given the `data` role on a founded cluster, with the keys
  left out, is planned as `site add`.

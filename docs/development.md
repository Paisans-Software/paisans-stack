# Development

The toolkit is a single Go binary. Contributors need a Go toolchain;
operators need nothing.

## Build

```
go build ./cmd/paisans
```

Go 1.26 or newer. There is no code generation step and no Makefile.

## Run

validate, init and render touch nothing outside the working directory.
`host prepare`, `apply`, `prune`, `site add`, `storage init`, `storage add`,
`storage rotate-key`, `app admin create` and `oidc client create` reach a
machine, and each changes
it only with `--execute`. `preflight` and `doctor`
reach every site and never change one; `failover test` changes which site
is primary, only with `--execute`. `dns init` and `dns prune` reach no
machine, only the DNS provider's API, and change it only with `--execute`.

```
paisans validate --config examples/paisans.example.yaml
paisans init     --config examples/paisans.example.yaml
paisans render   --config examples/paisans.example.yaml \
                 --secrets secrets.enc.yaml --out ./out
paisans host prepare --site home-a        # the host check, then what a blank host lacks
paisans apply    --site home-a            # shows what would change
paisans apply    --site home-a --execute  # does it
paisans prune    --site home-a            # lists dangling volumes, and which go
paisans preflight --site home-b           # site add's read only checks
paisans storage add                       # every Garage stage, from live state
paisans storage rotate-key --app talk     # replaces one app's S3 key, staged
paisans failover test                     # checks, and prints the plan
paisans doctor                            # what is stuck, and how to recover
paisans dns init                          # shows which records it would create
paisans dns prune                         # shows which records it would delete
security find-generic-password -s acme -w \
  | paisans secrets set external.acme_dns_token
```

`validate` loads a declaration and prints every problem it finds, rather than
stopping at the first, because fixing a file one error per run is miserable. It
exits non zero when anything was refused.

`init` generates the secrets a declaration needs and writes them, encrypted to
the age recipients named in a `.sops.yaml` beside the file. It prints names and
never values, because a secret printed to a terminal is in a scrollback buffer
and often in a multiplexer's log as well. Three things about it are load
bearing:

* **It never replaces a value that exists.** Regenerating a WireGuard key breaks
  every peer that trusted the old one; regenerating a database password locks an
  application out of a role that still holds the old one. Re-running `init` is
  therefore the intended way to fill in a site or an app added to the
  configuration later, and the second run of an unchanged deployment writes
  nothing at all.
* **It generates only what it can.** A DNS token is issued by a provider and an
  OIDC client lives at a running identity provider, where creating one is a
  mutation a human approves. Both are reported as owed, with the reason,
  rather than invented or left silent. An owed Mbin client names the command
  that creates it, `oidc client create`.
* **Without an age recipient it writes plaintext and says so loudly.** Refusing
  would leave an operator holding generated secrets that went nowhere, and a
  first look at the tool must not require a key.

`secrets set <dotted.key>` writes one value read from stdin into the secrets
file, re-encrypted to the recipients in `.sops.yaml`, and prints only `set
<key>`. It strips one trailing newline, refuses an empty value, refuses a
terminal on stdin, and accepts a key only if it is already set, owed by `init`
(`external.acme_dns_token`, `oidc_clients.<app>.*`), or under `external`. It
exists so a credential issued elsewhere never touches a terminal or an editor:
an argument is in shell history and `ps`, a prompt is in scrollback, and `sops`
opens the whole decrypted file in an editor.

`app admin create --app <pocket-id app> --username <u> --email <e>` makes
sure one user exists, is verified, is an administrator of Pocket ID, and is in
every admin group the apps signing in through it read (`kinds.OIDCClient`),
creating a group that does not exist yet. It probes, prints the plan or
`present`, and changes nothing without `--execute`. Pocket ID is the one kind
that implements it. An `mbin` app is refused before any host is reached: its
administrators come only through single sign on, so they are made here. See *`app admin create` makes an app's first administrator* in
`README.md`.

There is no password: the command creates the user as an administrator, with
its email marked verified, through Pocket ID's API and prints a one-time login
link once, on `--execute`, valid twenty minutes; `--login-link` issues a fresh
one for an account that exists. It refuses a piped password rather than dropping it. It
reads `apps.<app>.static_api_key` from the secrets file, so it takes
`--secrets`, and it never uses sudo, because curl needs no root.

`oidc client create --app <name>` creates an app's client at the deployment's
Pocket ID, with the groups the app's `config` names and a launch URL so
Pocket ID's dashboard lists it, and records its ID and
secret under `oidc_clients.<app>`. Every step is printed with what it sends and
nothing changes without `--execute`. The secret is generated on the
workstation and written to the secrets file before Pocket ID is sent it, and is
never printed. `--rotate-secret` adds a new secret, leaving the old one valid. Only Mbin's
client is known so far (`kinds.OIDCClient`). The launch URL is the app's
hostname plus the kind's dashboard path (`kinds.DashboardPath`, Mbin's
`/oauth/oidc/connect` so the tile signs the member in, otherwise `/`), or
`apps.<app>.settings.sso_dashboard_link`, which `validate` refuses unless it is
a path. An existing client's launch URL is moved only when it is empty or a
toolkit default (`kinds.ToolkitLaunchURLs`); any other value is left, with a
warning if `sso_dashboard_link` asks for something else. See *`oidc client
create` makes an app's client at Pocket ID* in `README.md`.

`preflight --site <new>` makes every check `site add` makes before it changes
anything, on the site being added and on every site already running, and
prints each as `ok`, `WARNING` or `REFUSED`. It exits non zero on a refusal.
It reaches every site through its ssh section and takes no `--ssh`, since one
override cannot name several hosts. The table of checks and the reasoning for
each are in *Preflight* in `README.md`.

`failover test` switches the Patroni primary to another data site and back,
and only with `--execute`; without it, it checks the cluster and every app and
prints both switchover commands and the expected write interruption. Each
switch is gated on the new leader, the old one streaming, and every app stack
healthy and answering through the gateway, polled for three minutes. See
*`failover test`: a switchover on purpose* in `README.md`.

`doctor` reaches every site, or the ones `--site` names, and reports what is
stuck with how to recover: sites that do not answer and what is lost while
they are gone, etcd quorum and cluster version, the Patroni leader or why no
replica promotes, containers that are down, Pocket ID's active instance, and
clock drift. It has no `--execute` and sends only reads, which its command
level test checks against an allow list. The judgements are pure functions in
`internal/doctor`, tested with what live hosts answered; `cmd/paisans` runs the
commands. It exits 1 on any `FAIL`. See *`paisans doctor` reports what is
stuck and how to recover* in `README.md`.

`render` and `apply` refuse a gateway site when `external.acme_dns_token` is
empty. `init` lists it as owed, but rendering without it produced a Caddy that
starts and then fails every DNS-01 challenge, which a visitor finds rather than
the operator.

A decryption failure names both `SOPS_AGE_KEY_FILE` and `SOPS_AGE_KEY_CMD`; the
embedded sops (v3.13.3, `age/keysource.go`) reads either, and the second lets
the age key live in a keychain rather than a file.

`render` validates, then writes per site artifacts under `--out`. It writes
files and stops: pushing them to a host is a later slice.

The example validates with exactly one warning, and that warning is
deliberate. Its Synapse stack is pinned to the site that also holds the witness
role, which the design warns about rather than refuses.

## The host check, before anything changes

`host prepare`, `apply`, `site add` (on the site being added) and `prune`
each run `internal/hostcheck` first, and so do `storage rotate-key` (on every
site the app runs on) and `storage add` (every Garage site and the gateway), in a dry run as well as with `--execute`, and
print its report above their own plan. `cmd/paisans/hostcheck.go` holds the
one gate they share, `hostGate`. README.md *The host check: what is already on
a host decides how much is touched* has the reasoning; what the code does:

* **Claims** (`hostcheck.ClaimsFor`) are `render.SiteListeners`, each
  listener carrying the `paisans.yaml` key behind it (`render.Listener.Key`),
  plus `wg0` and the mesh subnet. A new listener is added to
  `render.SiteListeners` with its key, and the host check, preflight's ports
  check and validate's collision check all see it.
* **Inventory** (`hostcheck.Inspect`) runs one probe per fact and changes
  nothing. Every probe is prefixed `export LC_ALL=C; `, and the first is
  `id -u`, refused unless it answers 0. A failed probe is an error, and Docker
  installed but not answering is an error rather than an empty list; the three
  `docker ... inspect` probes discard stderr and their exit status, because a
  container removed between the listing and the inspect is gone, not a
  failure. Each probe is told apart in tests by a substring only it contains
  (`id -u`, `docker version`, `dpkg-query`,
  `docker inspect`, `docker volume inspect`, `docker network inspect`,
  `ss -Hltnup`, `/proc/`, `ip -o link`, `ip -j route`, `ufw status verbose`,
  `is-active firewalld`); a fake transport elsewhere that reaches one of
  these commands has to answer it.
* **Classification** (`hostcheck.Classify`) is a pure function of the
  claims and the inventory, tested with recorded output: a clean host with
  an empty Docker, the toolkit's own running stack, a foreign host network
  Caddy against a site that claims 80 and 443 and one that does not, a
  foreign Postgres, a published port with no listener, a foreign `wg0`, a
  network over the mesh, and the three ways a shared host's firewall is
  refused.

What "shared" changes, and where:

| Command | On a shared host |
|---|---|
| `host prepare` | `hostprep.Shared()`: the profile's `Firewall` gets `hostWide` false and plans only `paisans:` rules, never `ufw default` or `ufw --force enable` |
| `apply` | `apply.KeepImages()`, as `--keep-images` |
| `site add` | `siteadd.Plan.KeepImages`, passed to the replica stage's whole apply |
| `storage rotate-key` | `apply.KeepImages()` in the switch's apply on that site |
| `storage add` | `storageadd.Options.SharedSites`: `apply.KeepImages()` in every scoped apply on that site |
| `prune` | `apply.BuildVolumePrune(site, t, true)`: only `paisans-*` labelled volumes go, and `SharedPruneHeader` says so |
| `preflight` | a `host` check line; the `prepared` check asks for the shared plan |

A conflict, and a shared host whose ufw is not active with incoming denied or
rejected by default (or whose firewalld is active), is refused by `Report.Refusal` before
anything else is asked of the host. There is no flag to skip the check.

## `apply`, and the gates in it

`apply` is one site at a time, and a dry run unless `--execute` is given. Both
are deliberate: a staged change that half succeeds across three machines is
worse than one that failed on one, and the difference between "show me" and "do
it" should be a flag an operator typed rather than a habit they formed.

The order inside it is the design, not an implementation detail. Everything is
compared first, so a conflict is found before a single byte is written; an apply
that wrote files as it discovered them could leave a stack half updated and then
refuse.

**A file edited on the host is a conflict, and a conflict stops the whole
apply.** Rendered files are build artifacts and nothing edits them in place, so
a file that differs from what the last apply recorded is a change somebody made
on the machine. The record is a manifest at `/srv/.paisans-manifest.json`,
written after every successful apply; without it, every apply would be a blind
overwrite. A file present on the host that no apply ever wrote is somebody
else's too, and is a conflict rather than something to adopt, which is the case
on any host that was set up by hand before the toolkit existed.

**The narrower action wins.** A changed bind mounted configuration file needs a
restart at most, and the container keeps its identity. Only a changed `.env`, any
other `*.env` handed over as an `env_file` (`patroni.env`, `caddy/caddy.env`),
or `compose.yaml` needs `up -d`, because Compose passes environment at start and
a running container cannot be told about a new value. `caddy.env` is not a
routing file even though it sits beside the Caddyfile: a reload rereads the
Caddyfile and never the environment, so a rotated DNS token arrives only by
recreating the gateway, behind the same gates as an image change. A recreate is an outage,
however brief, so it is not the default action for every change.

The gateway's routing files make a reload and no stack action, so Caddy gets
one action per apply (README, *A routing change gets exactly one action on
Caddy*). When the infrastructure stack is also an `up -d`, `runAction` in
`internal/apply/refresh.go` reads the container IDs before and after, reloads
a Caddy `up -d` left in place, and restarts any other service whose bind
mounted file changed and whose container was left in place (`Action.Refresh`).
`gatewayaction_test.go` and `infraservices_test.go` pin both with a fake whose
`up -d` replaces only the containers a test names.

**`wg0` is up before any container moves.** Every service binds the site's
mesh address, so the mesh comes up right after the files are written and before
the gateway checks and stack actions below. A first apply enables and starts
`wg-quick@wg0`; a peer change is handed over with `wg syncconf` so the mesh
stays up; a change to a line only wg-quick applies restarts it; and an
unchanged file on a host whose interface is down starts it. A failure stops the
apply there. `README.md` has the table under "`apply` brings `wg0` up before
anything binds to it".

**The infrastructure stack moves first, and app stacks only after their
databases exist.** Sorted order alone started `blog` and `docs` before
`infra`. Between the infrastructure stack and the first app stack, a site in
`cluster.sites` waits for a Patroni primary and creates clustered apps' roles
and databases; see "Every app has its own database credential" below.

**A stopped apply resumes.** Files written before a gate stopped the apply
already match the render, so the next apply would otherwise see nothing to do.
`/srv/.paisans-pending.json` records the owed stack actions and gateway checks
before the first write, drops each stack as its action finishes (the last one
stays until the end, so a bootstrap failing after it is still owed), and is
removed on success; `Build` folds it into the next plan.

**An owed stack is force-recreated.** On a real host an `up -d` failed part way
("failed to bind host port ... address already in use") and left Mbin's `app`
container created with no network attached. The resumed apply ran a plain
`up -d`, Compose saw an unchanged configuration and only started that
container, which came up with no networks and an empty route table. So a stack
the pending record owes runs `up -d --force-recreate`, and the plan shows it as
`recreate <stack> (forced)`. `--recreate <stack>` does the same for a stack
named by the operator when no record exists; it is repeatable, one stack per
flag, a stack this site does not render is refused, and a named stack is
planned even when nothing changed. Only those stacks are forced, because a
forced recreate replaces every container of a stack, an outage each time.

**Each stack must come up healthy before the next one moves.** An apply
reported success while Mbin's app could not reach its database, because `up -d`
and `restart` return as soon as containers start. After each stack's action,
`apply` polls `docker compose ps --all --format json` every 5 seconds for up to
5 minutes until every container of the project is running and every one with a
healthcheck reports `healthy`; a container without one counts once running.
A container exited, restarting or `unhealthy` stops the apply at once, a
timeout stops it too, and either error names the stack and services and carries
the last 30 log lines of each. The stack stays in the pending record, so the
next apply resumes there and force-recreates it. The infrastructure stack is
checked like any other, which covers the gateway's Caddy; Patroni's own wait
still gates the database bootstrap. `ps` output is read in both shapes Compose
has printed: one JSON object per line (current, `cmd/formatter/container.go`)
and one JSON array (older v2 releases, `cmd/formatter/formatter.go`).

**A pull onto a nearly full disk is refused before anything is written.** On
the first real host (10 GB root disk) the deployed stacks left 1.8 GB free,
and an Mbin upgrade pulls a 1.4 GB image beside the old one. `Build` probes
every rendered image with `docker image inspect` in one loop that prints
`present <id> <ref>` or `absent <ref>` per image; an output missing any image
is an error rather than "present", so a garbled probe cannot skip the check.
When a recreated stack names an absent image, `Build` reads Docker's data root
and its free space, and `Plan.Disk` carries the numbers; the dry run prints
them as `check disk:`, and `Execute` refuses first, before the pending record.
The default is 3 GiB, `apply --min-free <size>` overrides it for one run, and
the refusal quotes `docker system df`. README.md "`apply` checks free space
before it pulls" has the reasoning.

**A healthy stack's superseded images are pruned.** After the health gate,
`Execute` lists images (`docker image ls --no-trunc --format json`), reprobes
the rendered images' IDs and what every container uses (`docker ps -a` names
plus `docker container inspect` IDs), and runs `docker image rm <id>` for each
image from that stack's repositories that no stack of the site renders and no
container uses. A failed removal goes to `Progress` as a warning. `Build` lists
the same candidates, without the container filter, as `Plan.Prunes`, printed
as `prune` lines. `apply --keep-images` skips both, and so does a host the host
check found shared. IDs are compared by
prefix with `sha256:` stripped, since Docker prints them full or 12
characters short. README.md "`apply` prunes the images it superseded" has the
reasoning.

**An image's declared volume must be mounted at exactly its path.** Docker
gives every new container an anonymous volume for a `VOLUME` nothing mounts,
and Compose abandons it on the next recreate; a real apps site collected 18
under Mbin's `/app/var/`. Three pieces, each easy to undo by accident:

* `kinds.ImageVolumes` records what every pinned reference declares, read
  with `docker image inspect --platform linux/amd64` (the platform matters on
  an arm64 workstation: the containerd image store answered null for images
  that declare volumes). `TestEveryDeclaredImageVolumeIsMounted` in
  `internal/render` renders every app pinned and clustered at Postgres 16, 17
  and 18 and fails on an unrecorded image or an unmounted path. Bumping an
  image means inspecting the new reference and adding its row.
* `Build` inspects every image of every acted on stack (`VolumeCheck`, in
  `internal/apply/volumes.go`) with `kinds.ComposeMounts` and
  `kinds.Uncovered`. Only an exact path counts, because a bind at a parent was
  observed to leave Docker's volume beneath it. An absent image is owed and
  `Execute` pulls and checks it after the disk check and before the pending
  record; any uncovered path refuses there. The fakes in `internal/apply`,
  `internal/siteadd` and `cmd/paisans` answer the probe, which is a `for r in`
  loop like the presence probe and is told apart by `.Config.Volumes`.
* Before a recreate, `Execute` records the stack's containers' anonymous
  volumes, and after the health gate removes those now dangling
  (`internal/apply/anonvolumes.go`). Warnings only, and nothing on a failed
  gate.

`prune --site <s>` (`internal/apply/volumeprune.go`) lists every dangling
volume with size and top level entries and removes, with `--execute`, the
anonymous ones and those of a `paisans-*` compose project; another project's,
or a named one, is kept. On a host the host check found shared, an anonymous
volume is kept as well, since it may be another project's. The listing ends with an `end` marker, so a cut
short answer is an error rather than a shorter list. README.md "Every volume
an image declares is mounted, and `apply` checks it" has the audit table and
the reasoning.

**Files are recorded as soon as they land.** The manifest is written right
after the files, and again at the end, not only on success. A manifest written
only on success made every file of a failed first apply look like somebody
else's: on the first real host, the next apply carried a fix to `patroni.env`
and refused it as a host edit.

**`--overwrite <path>` replaces one named conflict.** A conflict stops the
apply, and the way out used to be deleting the file on the host by hand.
Naming the path is the same decision made through the toolkit, and it is
repeatable, one path per flag, so a single choice never covers files the
operator did not look at. A path that is not a conflict is refused rather than
ignored, so a typo cannot pass for consent.

**The assembled gateway configuration is validated before any reload, and a
failure stops the reload.** It is built from per app snippets, so a wrong
snippet is a wrong configuration for every hostname at once. The cost of that
mistake should be an error message on the workstation, not the public address of
every application.

**Before any of that, the gateway's Caddy is asked which DNS modules it has.**
A provider is a module compiled into the binary, so a wrong image and a provider
no module answers to both produce a gateway that cannot load its own
configuration, and neither is visible in an image reference. This check runs
before the configuration check because its error says what to fix.

A DNS provider's Caddyfile syntax is provider specific rather than uniform: the
cloudflare module takes the API token as a bare argument, while the deSEC module
requires a block with a `token` subdirective and rejects a bare argument. That
is why `internal/acme` holds the exact lines for each provider rather than
building one shared form (github.com/caddy-dns/cloudflare, README "Caddyfile"
section; github.com/caddy-dns/desec, README "Caddyfile" section).

**After an apply that acted on a Pocket ID stack running on more than one site,
every one of those sites is asked whether its instance is active.** Each
stack passed its own gate whether it is active or on standby, so this is the
only check that can see two active or none. It lives in `cmd/paisans`
(`checkStandby`) rather than in `apply.Execute`, because it reaches other
sites and `Execute` is one site's. The rule is `apply.OneActive`, shared with
`failover test` and with the admin commands, which use it to find the site
to call. README: "Pocket ID runs on every apps site, and one of them is
active".

## `site add`, and why it is not a loop of applies

```
paisans site add home-b             # every stage, its steps and its gate
paisans site add home-b --execute
```

`internal/siteadd` builds the join from live state and runs it stage by stage;
README.md's *`site add` is built for sites that dial each other* has the
design. What the code relies on, and what is easy to undo:

* **Existing sites are changed by scoped applies** (`apply.Scope`), never whole
  ones. A whole apply of the primary's site would recreate Patroni for its
  new `ETCD3_HOSTS`, a failover in the middle of a join. A scoped plan records
  its files in the same manifest, keeping every other entry, so the next whole
  apply sees them as its own, and `apply.Rollback` restores what one updated.
* **Every etcd host has `/srv/infra/etcd-initial`**, the flags its member was
  born with. `apply` and `site add` read it (`apply.ReadEtcdInitial`, falling
  back to the compose file's own flags) and hand it to `render.Build` with
  `render.WithEtcdInitial`; without it a member renders as a founder of
  `etcd.members`. `isRecord` in `internal/apply` keeps its write from acting on
  the stack.
* **`apply` refuses a half grown etcd** (`apply.EtcdRefusal`), probing the
  site and then the other configured members.
* **Every wait is a count of polls**, the wait over the interval, so the tests
  replace `sleep` with nothing (`SetFast` in `export_test.go`) and still end.
* **`internal/preflight` is a stub here**, with the agreed signature, replaced
  by the preflight branch.

The tests in `internal/siteadd` run a three host world in maps: etcd's
membership, Patroni's members, each HAProxy's served configuration and the
handshakes implied by each `wg0.conf`. They cover the dry run changing nothing,
a whole join leaving nothing to do, the mesh rollback, the promotion retry and
its limit, resuming after a stopped learner and after a failed replica gate,
and the stage 5 and 6 gates failing.

## `storage add`, and what lives where

`internal/storageadd` is shaped like `internal/siteadd`: Build reads every
Garage site and the gateway and changes nothing, each stage carries its steps
and a gate, and Execute runs the steps only when there are some and the gate
always. `docs/specs/2026-10-07-multisite-garage.md` is the approved design and
cites the Garage v1.0.1 source behind every gate. What is easy to undo by
accident:

* **Two gates wait on Garage, not on the toolkit** (`Stage.Waits`: settle and
  sync). They return a `*Waiting`, which unwraps to `ErrWaiting`, and
  `cmd/paisans` exits 75 on it rather than 1. A failure that is really a wait
  makes an operator chase a problem that is not there; a wait that is really a
  failure makes them wait for ever. Pick deliberately.
* **Every piece of resume state is on a host.** The deployed factor in
  `garage.toml`, `meta/cluster_layout.rf<N>` beside a set aside layout, and
  the object counts in `/srv/infra/garage/replication-change.counts` on the
  anchor's host. Nothing is kept on the operator's machine, so a run resumed
  from another machine sees the same state.
* **The reset stops every node at the old factor before it changes any.**
  Garage exits a node that meets a peer at a higher factor
  (`src/rpc/system.rs:583-587`), so stopping and rewriting one node at a time
  takes the others down mid-run. A unit test asserts the order.
* **`garage.toml` is rewritten through `apply`, scoped, with
  `apply.ReplicationChange()`**, which is the only way past `apply`'s refusal
  of another factor. Writing it any other way leaves the manifest stale and
  makes the next `apply` call it somebody's edit.
* **The probe's key goes to curl on stdin** (`curl -K -`), never in a command
  line, and a failure is redacted. A unit test makes the fake host echo stdin
  into the error, which is the worst case.
* **The media routes are rendered after the join, not before.** A node with no
  role answers every bucket as missing, and the routes prefer the first
  listed node. The stage writes every `<app>-media.caddy` the gateway renders,
  one per app that stores objects.

* **The stop test always starts the node again**, in a deferred call, even
  when a read failed, and only ever stops the last listed site. A refactor that
  returns early from `stopTest` without the deferred start leaves a site down.
* **Capacities are compared in bytes, within 2%**, because `layout show`
  rounds. Comparing the strings would reassign, and rebalance, on every run.

The tests in `internal/storageadd` run a Garage cluster in maps: each host's
files, its node, its copy of the layout and the peers it has met, with output
shaped like `dxflrs/garage:v1.0.1`'s. They cover the refusal without
`--change-replication`, a reset that waits at the sync and resumes, `--wait`,
a reset interrupted between rewriting and starting, the order rule, a site
with no `garage.toml`, a join without a reset, the probe's secret, and a
shortfall in object counts.

## `storage rotate-key`

`internal/rotatekey` has the same shape as `internal/storageadd`: Build reads
the secrets and Garage and changes nothing, each stage carries its steps and a
gate, Execute runs the steps only when there are some and the gate always. The
rules are in `README.md` under *Replacing an app's key*. What is easy to undo
by accident:

* **The resume state is the secrets file.** A previous pair beside the current
  one means a rotation is in progress. Nothing generates a pair while one
  exists, and half a pair, or a previous key equal to the current one, is
  refused before Garage is asked anything.
* **The deletion is last and is never skipped to.** A failure at any stage
  before it must leave the old key in Garage and granted; a test fails each
  stage in turn and asserts that, then resumes.
* **Keys are matched by ID only.** `garage key info`, `key delete` and
  `bucket allow` take a pattern that matches an ID prefix or an exact name
  (`src/model/key_table.rs`, `KeyFilter::MatchesAndNotDeleted`, at v1.0.1),
  so the code passes a full ID every time, and the grant check reads only the
  ID column of `bucket info`.
* **The switch is `apply` itself.** `cmd/paisans` implements
  `rotatekey.Switch` with `planSiteApply`, the function `runApply` uses, and
  `apply.Only`. A second way of writing an app's `.env` would leave the
  manifest stale.
* **Secrets stay out of output.** `key import` takes the secret positionally,
  so it goes through `garage.ImportStep`, whose failures are redacted; the
  probe sends the key on curl's stdin through `garage.S3Object`. A test
  asserts that no other command carries either secret.

The tests run Garage's keys and grants, the secrets file and each site's
running key in maps: a whole rotation's order, a dry run that changes nothing,
a resume after a failure at every stage and after the deletion, and the
refusals.

## `dns init`, `dns prune`, and why init can only create

`dns init` is the one command that talks to something other than a host or the
local disk: the DNS provider's API, from the workstation. It is a dry run unless
`--execute` is given, like `apply` and `storage init`, and it takes no `--site`
because it reaches no site.

```
paisans dns init                 # shows present, create and conflict per record
paisans dns init --execute       # creates what is missing, then reads each back
paisans dns prune                # shows remove and keep per toolkit record
paisans dns prune --execute      # deletes the removes, then lists the zone again
```

The rules are in `README.md` under *`dns init` creates the records a deployment
needs*. Four things about the code are easy to undo by accident:

* **`internal/dns.Provider` has no update.** A method that does not exist
  cannot be called by mistake, and a conflict is reported for a human to
  resolve rather than handed to an overwrite. Adding one is a design change,
  not a convenience.
* **`Delete` exists for prune alone.** Its one caller is `ExecutePrune`, which
  deletes only what `BuildPrune` marked remove after every rule in
  `internal/dns/prune.go`. Loosening a rule, and above all deciding by the
  comment alone, reopens a founder decision; the README section says why.
* **The provider's base URL is a struct field, not a flag.** Tests point it at
  an `httptest` server; an operator has no reason to send the zone token
  anywhere but the provider. No test calls a real provider, and none may.
* **The token never leaves the request header.** Every error from the
  Cloudflare client passes through a redaction of the token, and a test
  asserts it, including against a fake provider that echoes the header back.

Cloudflare's request and response shapes are cited in
`internal/dns/cloudflare.go` against its API reference. Nothing in this package
has been run against the real API yet.

A deployment that uses the challenge only zone arrangement described under
*Certificates use DNS-01, everywhere* holds a token that cannot see the main
zone. `dns init` then reports that no zone it can see holds the name, which is
correct: that arrangement trades this command for a narrower credential on the
gateway, and the records are created by hand.

### Why ssh is shelled out to and sops is not

The opposite choice in each case, for the same reason: what the operator already
has.

`sops` and `age` are embedded so that an operator need not
install two binaries before they can render anything, and because no code path
here may put a decryption key on a host.

`ssh` is shelled out to because every operator already has one, and theirs
already knows things this toolkit should never learn: their agent, their keys,
their `~/.ssh/config` with its jump hosts and per host users, their
`known_hosts`. An embedded client would have to reimplement that or, far worse,
invite a toolkit specific way to hand it a private key.

What the toolkit adds to the operator's ssh comes from the site's `ssh`
section, and is pinned by a test of the exact argument list
(`internal/apply/transport_test.go`):

```
ssh -p 22 -o IdentitiesOnly=yes -i /tmp/paisans-ssh-XXXX/key-1.pub -i /tmp/paisans-ssh-XXXX/key-2.pub ubuntu@203.0.113.10 <command>
```

The `-i` files are the listed **public** keys, written per invocation into a
0700 directory and removed after. With `IdentitiesOnly`, ssh offers exactly
those and signs with whichever one's private half is in the operator's agent;
README's *How the operator's ssh uses them* cites ssh(1) and ssh_config(5) for
it. `paisans.yaml` never names a private key, because it is shared by every
admin and a private key is one admin's. `--ssh <destination>` replaces the
section whole and is passed verbatim, as it was before the section existed.

File contents go to the host over stdin rather than in a command line, because a
rendered file carries credentials and a command line is visible in `ps` to every
user on the host. Each write lands in a temporary file that is then moved, so a
failed transfer leaves the previous file intact rather than a truncated one an
application would happily read.

A connection that never opened is retried, twice, inside `SSHTransport` itself
(README, *A connection that never opened is tried again*), so every caller gets
it without asking. Running the ssh binary is a package variable, `runSSH`, and
the tests in `transport_test.go` replace it with a fake that chooses the output
and exit status, which is how the "255 and a connection error, nothing else"
rule is pinned. When every attempt fails the error wraps `apply.ErrUnreachable`,
which is what a caller tests with `errors.Is` before it reads a failed probe as
anything at all.

The fakes in `internal/storageadd` and `internal/siteadd` have an
`unreachable` knob that fails matching commands with that error, and the tests
using it pin README's *Never infer host state from a failed probe*: each probe
stops with "could not read host state", and none is read as an answer. A new
probe that ignores an error, or reads `err != nil` as "absent", is the pattern
to avoid; check `apply.ErrUnreachable` first, or make the remote script print
a marker for every answer it can give, as the set aside script does.

## Refuse and warn

The distinction is the point of the tool and it is not a matter of severity
taste.

**Refuse** means the configuration is incoherent: it describes something that
cannot work, so rendering it would produce artifacts that damage a deployment.
A witness sharing a failure domain with its only voter is the worked example.

**Warn** means the configuration is legitimate but risky, and the risk is fine
when it is chosen rather than stumbled into. Pinning an app onto the witness
host is the worked example.

Every rule traces to a rule in `README.md`. If you believe one is missing, say
so in the pull request rather than adding it: policy that is not in the README
is policy nobody agreed to.

## Tests

```
go vet ./...
go test ./...
```

Three kinds of test carry most of the weight.

**Rule tests** live in `internal/validate`. Each fixture in
`internal/validate/testdata` is the valid one with exactly one thing broken and
is named for the rule it breaks, so a rule that fires on the wrong fixture is a
failure rather than a puzzle.

**Golden tree tests** live in `internal/render`. The tree under
`internal/render/testdata/golden` is checked in, because it is the
specification of what an operator receives: a diff there is a change in what
lands on a host. Regenerate it deliberately, and read the diff:

```
go test ./internal/render -update
```

**A determinism test** renders twice and compares byte for byte. Nothing
rendered may carry a timestamp or depend on map iteration order, or a diff
between two renders stops meaning anything.

**Docker tests** run real `dxflrs/garage:v1.0.1` containers, and are behind a
build tag because they need Docker and take seconds rather than milliseconds:

```
go test -tags garage_integration ./internal/garage/ ./internal/storageadd/
```

`internal/garage`'s starts the rendered `garage.toml`, provisions a node, and
routes media through the rendered snippet in the real gateway image,
including a read with the first listed node stopped. `internal/storageadd`'s
drives `storage add` from one node at replication 1 to two at replication 2,
`dangerous`, through the reset, and reads an object written before it back
through both nodes. A second grows to three nodes at replication 3, one a
`storage` host with a smaller capacity, and runs `--stop-test`. Under the tag
the Docker tests run alone and at real timing: the fake-cluster tests, whose
`TestMain` makes every wait instant, are left out of that build. It runs its probe in `curlimages/curl`, which it pulls.

Pocket ID's standby wrapper has Docker tests of its own, behind their own tag:

```
go test -tags standby_integration -timeout 10m -run TestStandbyWrapper ./internal/render/
```

Four run the rendered wrapper in the pinned Pocket ID image, through the
image's entrypoint and `su-exec`, with a fake `/app/pocket-id` bind mounted
over the binary: standing by and then serving, a prompt stop that reaches the
child, any other exit passed on, and the 50 line window. The fifth runs two
real instances against a `postgres:17-alpine` and stops the active one. Rerun
it whenever the Pocket ID image is bumped: it is what proves the marker
against the binary. The image is `linux/amd64`, emulated on an arm64
workstation.

## Fixtures and secrets

`internal/render/testdata/secrets.fixture.yaml` is plaintext on purpose. Every
value in it is a fixed placeholder, and its WireGuard private keys are repeated
bytes chosen so the derived public keys are stable across runs. Nothing in this
repository is a credential, and nothing in it may become one.

The encrypted path is exercised without an encrypted file ever being committed:
`internal/config/secrets_test.go` generates an age identity, encrypts a fixture
in memory, writes it to a temporary directory, and reads it back. That test is
also the proof that sops and age are genuinely embedded, because it starts no
process.

`sops` and `age` are Go libraries here, not binaries. Neither has to be
installed, on a workstation or anywhere else.

## Layout

| Path | Holds |
|------|-------|
| `cmd/paisans` | the command, flag parsing, and how findings are printed |
| `internal/config` | loading `paisans.yaml`, and decrypting `secrets.enc.yaml` |
| `internal/validate` | the rules, and nothing else |
| `internal/secretsgen` | what a deployment's secrets are, and which of them the toolkit may invent |
| `internal/kinds` | what an application kind is: its compose services, and the image each runs by default |
| `internal/render` | placement, templates, and the writer |
| `internal/dns` | which public records a deployment needs, creating the missing ones at the DNS provider, and pruning the ones it created that nothing wants |
| `internal/apply` | what to push to a host, what to restart, and the gates before either |
| `internal/siteadd` | joining a new data site: six staged, gated, resumable stages |
| `internal/appadmin` | an app's first administrator: probe, plan, and the per kind API calls |
| `internal/pocketid` | Pocket ID's REST API, called through curl on the host with everything variable on stdin |
| `internal/oidcclient` | an app's client at Pocket ID: probe, plan, and record its credentials before sending its secret |
| `internal/hostcheck` | what a site claims on its host, what the host already runs, and whether that is clean, shared or a conflict |
| `internal/preflight` | `site add`'s first stage: read only checks on the new site and every running one, as a report |
| `internal/failover` | `failover test`: its checks, the switchover and its gates |
| `internal/doctor` | `doctor`: the read commands it sends, and the findings and recovery advice made from their answers |
| `internal/patroni` | reading a Patroni cluster through the Spilo container: `/cluster`, the leader, lag, database size |
| `internal/hostprep` | taking a blank host to what `apply` assumes; one profile per operating system, its shell under `profiles/<id>-<version>/` |
| `internal/render/templates` | the infrastructure templates, plus one directory per kind |

Templating is `text/template` from the standard library. No template engine is
inherited, per the language decision in `docs/decisions.md`.

### A kind ships a template directory

`internal/render/templates/<kind>/` is a set, and every file in it is rendered.
Two conventions make a set need no code of its own.

**A template's path is its destination.** `templates/synapse/initdb.d/
01-mas-database.sql.tmpl` lands at
`/srv/<stack>/initdb.d/01-mas-database.sql`, so where a file goes is
read off the tree rather than held in a mapping somewhere else. Adding a file
to a set is adding a file.

**A `.secret.tmpl` suffix means the rendered file is 0600.** Everything else is
0644. The mode travels with the template for the same reason the destination
does, and 0644 is the default because a rendered file the container's own user
cannot read is a stack that does not start.

The set is given one value, `appValues` in `internal/render/appview.go`, which
holds what needs the whole deployment to know: the database host for this app's
placement, the object storage endpoint, this app's own secrets and its client at
the identity provider. **The template decides what goes into which file, under
what names, in what format; Go decides nothing about an application's
configuration.** That is why two of the five kinds can be configured by files
rather than environment at all.

One file in a set is not rendered beside the app: `caddy.snippet.tmpl` lands on
every gateway, at `/srv/infra/caddy/snippets/<app>.caddy`, because that is where
it is read. The gateway's own `Caddyfile` keeps only what is cross cutting,
certificates and the trusted proxy range, and gives each app a host block that
imports its snippet. A snippet never hardcodes where its application runs: it
receives the upstreams from the inventory, which is what keeps a pinned app and
a clustered app the same shape.

An app can answer on more than one hostname, and a gate can sit in front of
it, and both are properties of the app declared in `paisans.yaml` rather than
anything encoded in a template's filename alone. `internal/kinds/hostnames.go`
is the closed set of extra hostname roles a kind understands beyond the
primary; `SnippetFor` maps a role to `caddy.snippet.<role>.tmpl`, so a role a
kind's template directory ships no file for cannot be declared, and a role
outside that set is refused at validate rather than discovered at render, the
same way an unknown `images` key is. `synapse` has a second role,
`wellknown`, for a homeserver's delegation documents. Every kind that stores
objects has `media`, and it is the one role that exists without being
declared: `kinds.MediaHostname` derives `<label>-media.<domain>` from the
app's own hostname, and `hostnames.media` only overrides it. It is also the one
route the app's gate does not follow, because a media hostname is never gated. `gate` works
the other way: it names which of the `oauth2-proxy` kind's two named snippets,
`gate_provisional` or `gate_members`, an app's own snippet imports.
`internal/render/site.go`'s `renderGateSnippets` renders those named snippets
onto the gateway once per `oauth2-proxy` app declared, however many other
apps' snippets go on to import them by name. Both are read in
`internal/render/appview.go`'s `values`, the same function that assembles
everything else a template set is given, so a snippet template sees them as
ordinary fields rather than as a special case.

Embedding uses `//go:embed all:templates`. Without `all:` embed skips files
beginning with a dot, and every environment configured kind ships a `.env`
template.

### Where a claim about upstream software comes from

Every environment variable name, configuration key and image tag in a template
set is checked against upstream before it is written, and the check is recorded
in a comment where it is not obvious. Some of it is counter intuitive and does
not survive being recalled: WriteFreely's `[oauth.generic]` endpoints are paths
appended to `host` rather than URLs, upstream Mbin ships named OAuth providers
and no generic OIDC one (which is why the mbin kind's default image is the
paisans fork, which adds one, and its callback is `/oauth/oidc/verify`), and
Spilo publishes a separate image repository per Postgres major version whose
tags do not run in step.

## Certificates use DNS-01, everywhere

With HTTP-01 a server can only obtain a certificate for a name that already
points at it. A new gateway could therefore not hold valid certificates until
DNS moved, and DNS should not be moved to a server without them. DNS-01 proves
control through the provider's API and needs no inbound reachability, so a new
gateway can be fully ready before a single record changes. That is what makes
moving the gateway an overlap rather than a cutover, and reversible at every
step until the old one is stopped.

It is also the only option for a hostname served behind a VPN, where nothing on
the internet can reach the host to answer a challenge.

The cost is a token with DNS edit rights on the zone, sitting on the gateway,
which is the most internet exposed machine in the deployment. Two things bound
it. The token is scoped to one zone, never an account wide credential. And an
operator who wants it narrower can CNAME every `_acme-challenge` record into a
challenge only zone and scope the token to that zone, leaving a credential on
the gateway that can write challenges and nothing else. The toolkit does not
require that arrangement, and it does not prevent it.

The token is acceptable on the gateway because anyone with root there already
terminates TLS for every hostname and holds every private key, so they can
already read and alter all traffic. The token adds reach past that machine,
which is why it is scoped to one zone.

## Every app has its own database credential

There is no fallback and no shared password. One cluster holds every app's
database, so a credential shared between apps is a credential that reads every
other app's data, and the admin password is worse again: it creates and drops
roles, and an operator uses it.

`secrets.enc.yaml` therefore carries `apps.<name>.database_password` for every
app that uses Postgres, clustered or pinned, and rendering fails by name when
one is missing. WriteFreely is the exception and needs none: it has never
supported Postgres and runs on a SQLite file in its own data directory, which
is what keeps one blog from adding a second database engine to operate.

`apply` creates the roles and databases for clustered apps, on a site in
`cluster.sites`, after the infrastructure stack and before any app stack. It
waits up to three minutes for Patroni's `/cluster` to name a running leader,
then sends one psql script on stdin to the Spilo container: create the role if
missing, set its password every time (so rotation is editing the secret and
applying), create the database owned by it if missing. A replica skips the work
and says which site holds the leader; a leader that is not one of
`cluster.sites` is not a replica and stops the apply. A timeout or a psql
failure stops it too, before any app starts, and the next apply resumes there. `README.md` has
the reasoning under "`apply` creates each clustered app's role and database".
A pinned app's own Postgres creates its role from the image's environment, as
before.

## Things the code enforces that are easy to undo by accident

* **Trusted proxies are the mesh subnet**, never a host address. This is
  expensive to retrofit and fails quietly, so it is not configurable and a test
  asserts it.
* **The mesh subnet is declared in `mesh.subnet`**, not derived from the site
  addresses. Deriving the tightest network that fits them would widen it the
  moment a site was added, silently changing `TRUSTED_PROXIES` in every app.
  Declaring it also lets an operator avoid a range their hosts already route.
  Every site address must sit inside it, and that is a refusal.
* **Every port a site binds is listed once, in `render.SiteListeners`**, from
  the constants the templates render. `validate`'s `port-collision` reads it,
  so a new listener added to a template without a constant, or a new kind's
  port added to `appPort` without a look at the others, escapes the check.
  `TestPublishedPortsAreDistinctPerSite` in `internal/render` catches the
  second against the fixture; nothing catches the first.
* **A pinned stack uses bind mounts under `/srv/<stack>/`**, never named
  volumes, so relocating it is one `tar`. A test walks every rendered compose
  file to confirm it.
* **A clustered app has no Postgres service of its own** and connects to the
  HAProxy on its own site, at that site's mesh address and the cluster port
  (Eg: `10.44.0.1:5000`), never `127.0.0.1`, which inside the app's container
  is the container itself. A pinned app gets its own container: an app that is
  pinned must be pinned all the way down.
* **Failover and the 503 live in the gateway file**, as the named snippets
  `upstream_failover` and `upstream_unavailable`, not in each kind. A kind's
  snippet imports `upstream_failover` when it has more than one upstream and
  never sets `lb_policy` itself; every host block imports
  `upstream_unavailable`. `TestMultiSiteAppsShareOneFailoverSetting` and
  `TestEveryHostBlockAnswers503WhenNoSiteCan` hold both. README's "Failing
  over, and saying so when nothing can serve" has the reasoning.
* **Mbin's `LOCK_DSN` stays `flock`**, and only `CLUSTER_LOCK_DSN` points at
  the database. Every Symfony rate limiter takes the default lock store in
  FrankenPHP's long lived web workers, where a database store's private
  connection is never reopened once HAProxy closes it; a first version of this
  change made exactly that mistake. `CLUSTER_LOCK_DSN` is `DATABASE_URL`
  unchanged, for the fork's planned named `cluster` lock resource, and is inert
  until a fork release reads it. `TestMbinSplitsItsLocks` asserts both for
  clustered and pinned placement. `README.md` has the reasoning under "Mbin's
  locks are split: flock by default, Postgres for the few that cross sites".
* **Mbin's queues default to Postgres** (`settings.queue`, `kinds.MbinQueue`),
  with no broker in the stack; `queue: rabbitmq` renders the broker stack as
  before. `TestMbinQueuesInPostgresByDefault` and `TestMbinRabbitMQIsAnOptIn`
  hold both, and `validate` refuses an unknown value and warns for RabbitMQ on
  more than one apps site. The default needs a paisans fork image with the
  Doctrine transport decorator. README: "Mbin's queues are in Postgres by
  default".
* **Pocket ID's standby is keyed on its log text**, pinned per image in
  `kinds.PocketIDStandbyMarker`. `TestDefaultPocketIDImageHasItsStandbyMarker`
  fails on an image bump until the new text is recorded; record it from the
  release's `bootstrap.go`, then run the `standby_integration` tests above.
  The instance check asks for the wrapper's state file **before** `/healthz`:
  a standby's port is closed, so the other order reads every standby as
  down. `TestTheInstanceQuestionAsksTheStateFileThenTheMeshPort` holds the
  order. And the compose file names the image's entrypoint and command as the
  wrapper's arguments because setting `entrypoint` clears the image's
  command; dropping them leaves the wrapper running nothing.
* **A published port binds the site's mesh address**, never every interface,
  because Docker's iptables rules bypass a host firewall. A test walks every
  rendered compose file to confirm it.
* **Nothing rendered carries a floating image tag.** `latest` is refused in an
  operator's configuration, so a default that floated would be the toolkit
  refusing what it writes itself. A test walks every rendered compose file.
* **A file carrying a credential is 0600.** A bind mounted file at 0600 is not
  readable through `docker inspect` or `/proc/<pid>/environ`, which is the whole
  reason a file configured application is the better case. A test asserts it
  against the fixture's placeholder credentials.
* **Output is deterministic.** Sort before you iterate a map.
* **host prepare removes only authorized keys it added**, as recorded in
  `/etc/paisans/authorized_keys.<user>.owned`, and never rewrites a key's
  comment to mark it. Removals run last, and a plan that would leave the user
  with none of the listed keys is refused. Tests in
  `internal/hostprep/hostprep_test.go` cover add, adopt, forget, remove, a
  restricted key and that refusal.
* **The SSH allow follows `ssh.port` and is never removed**, including the
  allow for a port the site used before. A test moves the port and asserts the
  old allow is kept and noted.

## What is not here yet

No `backup`. `site add` exists for sites that all declare an endpoint; the
relay for sites behind NAT is still design. `storage add` joins Garage nodes,
changes the replication factor, sizes each node and can stop one to prove
failover, and has run against containers; removing or replacing a Garage node
is not built. Its first stage is `preflight`, and
`failover test` exists; both are described above. `apply`
pushes files, brings up `wg0`, creates clustered apps' roles and databases, and
takes the narrowest action that makes the rest live. `site add` has never been
run against a real host, so every command it sends is reasoned from upstream
source and documentation, not observed.

Pocket ID on more than one apps site has run only in containers on one
machine: two real instances against one Postgres, through the rendered
wrapper and compose file. It has not run across the mesh, so the gateway's
handling of a standby's closed port is reasoned, not observed.

Mbin's media reverse proxy is rendered, as one of the per app media
hostnames: each kind that stores objects ships a `caddy.snippet.media.tmpl`,
and the gateway gives each such app a site block for its own
`<label>-media.<domain>`. See rule 3 in `README.md`.

Mbin's own OAuth2 server keypair, which API clients and mobile apps need, is
generated by `init` and rendered to `/srv/<app>/oauth/` on every site running
Mbin; README's *Three kinds of secret* says why it is 0644. Nothing chowns any
of the stack's bind mounted directories; the header of
`templates/mbin/compose.yaml.tmpl` says which containers cope with a root
owned directory and which do not.

If you find yourself writing a transport layer, that is the next slice and it
wants its own review.

Two claims used to live in this section and are corrected here rather than
left to go on being read. **Secret generation exists**: `internal/secretsgen`
and `paisans init` are implemented, described above under `init`. **The
gateway validation gate did not land with `apply`, because it did not need
to.** It is enforced today, described above under "`apply`, and the gates in
it": the assembled gateway configuration is validated before any reload, and
a failure stops the reload rather than taking every hostname down with it.

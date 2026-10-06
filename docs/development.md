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
`host prepare`, `apply`, `storage init` and `app admin create` reach a machine,
and each changes it only with `--execute`. `dns init` reaches no machine, only
the DNS provider's API, and changes it only with `--execute`.

```
paisans validate --config examples/paisans.example.yaml
paisans init     --config examples/paisans.example.yaml
paisans render   --config examples/paisans.example.yaml \
                 --secrets secrets.enc.yaml --out ./out
paisans host prepare --site home-a        # shows what a blank host lacks
paisans apply    --site home-a            # shows what would change
paisans apply    --site home-a --execute  # does it
paisans dns init                          # shows which records it would create
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
  OIDC client secret is minted by a running identity provider, where creating
  one is a mutation a human approves. Both are reported as owed, with the reason,
  rather than invented or left silent.
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

`app admin create --app <name> --username <u> --email <e>` makes sure one user
exists, is verified and is an administrator of one app, reading the password
from stdin under the same rules as `secrets set`. It probes, prints `create
user`, `verify`, `grant admin` or `present`, and changes nothing without
`--execute`; an existing password changes only with `--reset-password`. Only
Mbin implements it. See *`app admin create` makes an app's first
administrator* in `README.md` for why the password never reaches a command
line.

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
planned even when nothing changed. Forcing every recreate was rejected: it
would replace every container of every acted on stack on every apply, an
outage each time for the one case that needs it.

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
before it pulls" has the rejected alternatives.

**A healthy stack's superseded images are pruned.** After the health gate,
`Execute` lists images (`docker image ls --no-trunc --format json`), reprobes
the rendered images' IDs and what every container uses (`docker ps -a` names
plus `docker container inspect` IDs), and runs `docker image rm <id>` for each
image from that stack's repositories that no stack of the site renders and no
container uses. A failed removal goes to `Progress` as a warning. `Build` lists
the same candidates, without the container filter, as `Plan.Prunes`, printed
as `prune` lines. `apply --keep-images` skips both. IDs are compared by
prefix with `sha256:` stripped, since Docker prints them full or 12
characters short. README.md "`apply` prunes the images it superseded" has the
rejected alternatives (`docker image prune -a`, keeping N versions).

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
ignored, so a typo cannot pass for consent. A blanket `--force` was rejected
for that reason.

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

## `dns init`, and why it can only create

`dns init` is the one command that talks to something other than a host or the
local disk: the DNS provider's API, from the workstation. It is a dry run unless
`--execute` is given, like `apply` and `storage init`, and it takes no `--site`
because it reaches no site.

```
paisans dns init                 # shows present, create and conflict per record
paisans dns init --execute       # creates what is missing, then reads each back
```

The rules are in `README.md` under *`dns init` creates the records a deployment
needs*. Three things about the code are easy to undo by accident:

* **`internal/dns.Provider` has no update and no delete.** A method that does
  not exist cannot be called by mistake, and a conflict is reported for a human
  to resolve rather than handed to an overwrite. Adding either is a design
  change, not a convenience.
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

`sops` and `age` are embedded because their alternative is telling an operator to
install two binaries before they can render anything, and because no code path
here may put a decryption key on a host.

`ssh` is shelled out to because every operator already has one, and theirs
already knows things this toolkit should never learn: their agent, their keys,
their `~/.ssh/config` with its jump hosts and per host users, their
`known_hosts`. An embedded client would have to reimplement that or, far worse,
invite a toolkit specific way to hand it a private key.

File contents go to the host over stdin rather than in a command line, because a
rendered file carries credentials and a command line is visible in `ps` to every
user on the host. Each write lands in a temporary file that is then moved, so a
failed transfer leaves the previous file intact rather than a truncated one an
application would happily read.

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
| `internal/dns` | which public records a deployment needs, and creating the missing ones at the DNS provider |
| `internal/apply` | the only package that reaches a host: what to push, what to restart, and the gates before either |
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
same way an unknown `images` key is. `synapse` is the one kind with a second
role today, `wellknown`, for a homeserver's delegation documents. `gate` works
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

Weighed against the alternative: anyone with root on the gateway already
terminates TLS for every hostname and holds every private key, so they can
already read and alter all traffic. The token adds reach past that machine,
which is why scoping it matters and why account wide credentials are not
acceptable here.

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
* **A pinned stack uses bind mounts under `/srv/<stack>/`**, never named
  volumes, so relocating it is one `tar`. A test walks every rendered compose
  file to confirm it.
* **A clustered app has no Postgres service of its own** and connects to the
  HAProxy on its own site, at that site's mesh address and the cluster port
  (Eg: `10.44.0.1:5000`), never `127.0.0.1`, which inside the app's container
  is the container itself. A pinned app gets its own container: an app that is
  pinned must be pinned all the way down.
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

## What is not here yet

No etcd, no preflight, and none of `site add`, `failover` or `backup`. `apply`
pushes files, brings up `wg0`, creates clustered apps' roles and databases, and
takes the narrowest action that makes the rest live. It has never been run
against a real host, so every command it sends is reasoned from upstream source
and documentation, not observed.

Mbin's media reverse proxy, which this section used to list as missing, is
rendered: `storage.media_hostname` becomes the gateway's `media.caddy`, and
Mbin's `KBIN_STORAGE_URL` points at it.

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

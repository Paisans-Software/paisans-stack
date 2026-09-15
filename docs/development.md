# Development

The toolkit is a single Go binary. Contributors need a Go toolchain;
operators need nothing.

## Build

```
go build ./cmd/paisans
```

Go 1.26 or newer. There is no code generation step and no Makefile.

## Run

Four commands exist so far. Three touch nothing outside the working directory.
`apply` is the exception and is the only code path here that reaches a machine.

```
paisans validate --config examples/paisans.example.yaml
paisans init     --config examples/paisans.example.yaml
paisans render   --config examples/paisans.example.yaml \
                 --secrets secrets.enc.yaml --out ./out
paisans apply    --site home-a            # shows what would change
paisans apply    --site home-a --execute  # does it
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
restart at most, and the container keeps its identity. Only a changed `.env` or
`compose.yaml` needs `up -d`, because Compose passes environment at start and a
running container cannot be told about a new value. A recreate is an outage,
however brief, so it is not the default action for every change.

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
| `internal/apply` | the only package that reaches a host: what to push, what to restart, and the gates before either |
| `internal/render/templates` | the infrastructure templates, plus one directory per kind |

Templating is `text/template` from the standard library. No template engine is
inherited, per the language decision in `docs/decisions.md`.

### A kind ships a template directory

`internal/render/templates/<kind>/` is a set, and every file in it is rendered.
Two conventions make a set need no code of its own.

**A template's path is its destination.** `templates/mbin/config/packages/
oneup_flysystem.yaml.tmpl` lands at
`/srv/<stack>/config/packages/oneup_flysystem.yaml`, so where a file goes is
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

Embedding uses `//go:embed all:templates`. Without `all:` embed skips files
beginning with a dot, and every environment configured kind ships a `.env`
template.

### Where a claim about upstream software comes from

Every environment variable name, configuration key and image tag in a template
set is checked against upstream before it is written, and the check is recorded
in a comment where it is not obvious. Some of it is counter intuitive and does
not survive being recalled: WriteFreely's `[oauth.generic]` endpoints are paths
appended to `host` rather than URLs, upstream Mbin ships named OAuth providers
and no generic OIDC one, and Spilo publishes a separate image repository per
Postgres major version whose tags do not run in step.

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

Creating those roles in Postgres is not implemented. Nothing in this slice
touches a running database, so the credentials are rendered and the roles that
use them are a job for `apply`.

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
* **A clustered app has no Postgres service of its own** and connects to
  `127.0.0.1:5000`. A pinned app gets its own container: an app that is pinned
  must be pinned all the way down.
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
pushes files and takes the narrowest action that makes them live; it does not
bootstrap a site that has nothing on it, and it has never been run against a
real host. The gateway configuration is assembled from per app
snippets, so `apply` will have to validate the assembled file and refuse to
reload one that does not validate: a wrong snippet should cost an error message
on the workstation rather than the public address of every application at once.
There is nothing to reload yet, so that gate lands with `apply`.

Mbin's media reverse proxy is not rendered either. Upstream advises one on a
hostname of its own so media URLs survive a change of storage provider, and
remote instances cache those URLs, so adding it later is a migration rather than
an addition. It needs a hostname in the configuration and a site block of its
own, which is a decision to take deliberately. Secret generation does not exist either. If you find
yourself writing a transport layer, that is the next slice and it wants its own
review.

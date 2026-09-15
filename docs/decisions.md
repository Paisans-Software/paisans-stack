# Toolkit decisions

One entry per settled decision about **this toolkit**. Newest last.

Decisions about a *community* — which software fills a capability, federation
policy, how that community governs itself — do not belong here. They belong in
that community's own records. This file is for choices that every adopter
inherits.

---

## 2026-09-02 — The toolkit is written in Go, as a single binary

**Decided.** Not Terraform, not Ansible, not shell.

### Why not Terraform

Terraform provisions cloud resources through provider APIs, converging a
dependency graph against a state file. This toolkit renders configuration files,
pushes them to Linux hosts, runs commands, and verifies results between stages.
Three mismatches:

* **A staged join is not a dependency graph.** "Verify the WireGuard handshake in
  both directions, and only then change etcd membership" is a procedure with a
  gate and a rollback. A graph expresses ordering, not verification-with-abort.
  The real logic would end up inside `remote-exec` provisioners, which upstream
  itself calls a last resort.
* **State becomes another artifact to keep consistent.** The backup contract
  already names four; a state file that must stay in step with the encrypted
  secrets would be a fifth, and the one that goes stale quietly.
* **Licence.** BUSL since 2023, which is an avoidable complication for something
  meant to be handed to people we have never met. OpenTofu exists if it is ever
  wanted.

Terraform *is* right for provisioning the cloud instance itself — create the
machine, firewall rules, DNS records. That should stay **optional**, because most
operators will click a button in a provider's console and requiring otherwise
raises the floor for no benefit.

### Why not Ansible

The closest fit of the alternatives, and a reasonable choice: agentless over SSH,
idempotent, good templating. Two things counted against it.

The staged operations need verification loops, conditional rollback and precise
failure messages, and those degrade quickly when expressed in YAML. And it puts
a working Python environment in front of every adopter, which is exactly the kind
of step where people stop.

### Why not shell

It matches what already exists and adds no dependency, which is a real argument.
But YAML parsing, templating and staged rollback in shell get bad fast, and this
toolkit's whole value is in the parts that would be worst.

### Why Go

**Distribution.** One static binary, no runtime, cross-compiled for macOS and
Linux. An operator downloads it and runs it. This is the same constraint that
ruled out Kubernetes: the floor for an adopter has to stay low, and every install
step is somewhere they can stop.

**The policy is the product.** Most of the design is refusals and warnings —
refuse a witness sharing a failure domain with a voter; refuse a two-voter
cluster; refuse to overwrite a locally edited `.env`; warn when pinning an app
onto the witness; verify tunnels before touching etcd membership. Each is a place
where a novice operator is caught before doing damage. That is program logic with
good error messages, not declarative configuration.

**sops and age are both Go**, so encryption and decryption can be embedded rather
than shelled out. That removes two installation steps from every workstation and
makes the secrets path harder to get wrong. *Verify the library surfaces before
relying on this.*

### What it is not

* **Not a daemon.** Every command is operator-invoked. Patroni and etcd are the
  only things that watch anything, deliberately.
* **Not an orchestrator.** It renders and pushes; Docker Compose runs things. The
  moment it starts scheduling, it is re-inventing what was already rejected.

### Consequences

* Contributors need a Go toolchain; operators need nothing.
* Templating uses the standard library, so no template engine is inherited.
* Anything shelled out to — `docker`, `wg`, `etcdctl`, `patronictl` — is a
  dependency on the *host*, and belongs in preflight.

---

## 2026-09-15 — This repository lives in the Paisans-Software organisation

**Decided.** `josephquigley/paisans-stack` became
`Paisans-Software/paisans-stack`, private, and the Go module path became
`github.com/paisans-software/paisans-stack`.

### Why

This toolkit is meant to be handed to organizers nobody here has met. A
repository under one person's account says the opposite of what the project
intends: that it is somebody's side project, that its continuity depends on one
account, and that an adopter is trusting an individual rather than a project.
An organisation can gain maintainers, survive a person losing interest, and own
the packages the toolkit tells adopters to pull.

The forks, `mbin-paisans` and `writefreely-wisp`, deliberately did not move.
Their audience is upstream maintainers who already recognise the account that
opens pull requests against them, and moving a fork under an organisation buys
nothing there while costing that recognition.

### Consequences

* GitHub redirects the old path, so open pull requests and existing clones
  survive. A redirect is a courtesy rather than a contract: a clone that
  predates the move has its remote updated rather than relying on one.
* The module path changed, which touched every import. It is renamed rather
  than left as a path that names a repository that no longer exists, because a
  module path that lies is worse than one that is inconvenient to change.
* `paisans.community`'s own `CLAUDE.md` names this repository as the one
  sanctioned fallback when its wiki is unreachable. That reference is part of a
  recovery path, so it is updated in the same change rather than left pointing
  at a redirect that a future outage would have to survive.
* Packages this project publishes now belong to the organisation, which is what
  makes `ghcr.io/paisans-software/...` a name an adopter can be asked to trust.

---

## 2026-09-15: The ACME DNS provider is declared, and its Caddy image is published

**Decided.** A deployment names its DNS provider in `acme.provider`, and pulls a
Caddy image with that provider's module compiled in from
`ghcr.io/paisans-software/caddy`. Nothing is built on a host.

### What this fixed as well as abstracted

The toolkit rendered upstream's `caddy:2-alpine` against a configuration saying
`acme_dns cloudflare`. A DNS provider in Caddy is a module compiled in with
xcaddy, and upstream's image carries none, so **the rendered gateway could not
have obtained a certificate at all.** Nobody had run it.

### Why not build on the gateway

It is what a deployment's own stack does, and it works anywhere with no registry
account. It was declined because the suggested topology puts the gateway role on
a small machine that is also the etcd witness, and etcd's stability is a
function of fsync latency. A compile there competes for exactly the resource
that placement was chosen around, and it repeats on every Caddy upgrade.

The cost is that a deployment's gateway now depends on our registry and on those
images being rebuilt. `acme.image` is what keeps that from being lock-in.

### Why one image with both modules

Switching provider is then a configuration edit with no image change, no rebuild
and no repull, which is the point of the abstraction. An adopter carries a module
they do not use, a few megabytes.

### Why the default is a digest

Every tag `caddy-dns` publishes moves, mirroring upstream, which rebuilds even a
patch tag when its base image gets a security fix. A digest is the only
reference that makes two runs of `apply` deploy the same bytes.

### Why an unknown provider is allowed

A closed set would be consistent with how application kinds are treated, and it
would leave an adopter on another DNS provider waiting for us or forking.
Instead the provider is declared with an image that carries its module, and the
module is verified at apply time by asking the binary, since what is compiled
into a binary is not a property of its name.

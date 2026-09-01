# paisans-stack

Toolkit for installing and operating a paisans community stack: a private,
federated community on hardware its organizers control.

**Status: nothing is implemented yet.** This repository holds the design and
will hold the scripts. Do not expect anything here to run.

## What it is meant to do

Install a complete community stack on one machine, then let a second site be
added later as a manually invoked, additive step — without touching application
configuration and without a migration.

```
paisans init --domain example.org          # site 1, standalone, HA-ready
paisans site add --role witness  vm.example.org
paisans site add --role standby  home-b.example.org
paisans site remove home-b.example.org
paisans failover status
paisans failover switchover
paisans check
```

Command names are provisional.

## Two design rules that everything else follows from

**1. The cluster always runs, even when it has one node.**

A single-site install runs Postgres under Patroni, with etcd and a local
HAProxy, as a cluster of one. It would be simpler to install plain Postgres and
add Patroni when a second site appears — but that is a *conversion*, performed
on live member data, not an addition. Running the control plane from the start
costs a single-site adopter roughly 150 MB and some extra moving parts. It buys
an upgrade path that cannot corrupt anything.

**2. Applications always talk to a local HAProxy.**

Every stack connects to `127.0.0.1:5000` from the first install, where a local
HAProxy holds a backend list that initially has one entry. Adding a site grows
that list. No application configuration changes, ever. This one indirection is
what makes a second site an operation rather than a project.

## Adding a site, and the constraint that shapes it

etcd quorum is a majority: 1 member survives nothing but works; 3 members
survive one loss; **2 members survive nothing and are strictly worse than 1.**

So a join must take the cluster from one voter to three in a single operation —
the second site and a witness together. It can never rest at two.

A witness is a voting member that stores no data and serves no traffic. It
exists so that when two sites cannot reach each other, exactly one of them can
still form a majority and know it is safe to serve.

## The witness is a third failure domain, not a cloud account

A small always-on cloud VM is the **default**, because it is the easiest third
failure domain for most organizers to obtain, and because it doubles as the
public entry point for sites on residential connections with changing addresses.

It is not required. What is required is a third location that fails
independently of both sites. Another organizer's house works. A machine at a
workplace or a friend's spare room works.

Declining a witness entirely is supported, and means giving up automatic
failover: two sites with two voters cannot safely promote anyone, so promotion
becomes a documented manual step. That is a legitimate choice. It is not a
degraded version of the same thing, and the toolkit should say so plainly
rather than pretending otherwise.

## Design documents

The full design lives in the community's Outline instance, in Operations. This
repository will carry implementation notes and runbooks specific to the toolkit
as they are written.

## Relationship to `paisans.community`

`paisans.community` is one deployment: its compose files, its Caddy config, its
operational scripts. This repository is the generalisation — the part another
organizer could adopt without inheriting this community's specifics.

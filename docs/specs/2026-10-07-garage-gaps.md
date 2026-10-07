# Garage gaps after multi-site: storage hosts, capacities, the stop test

Date: 2026-10-07. Status: **approved by the founder in session**, with the
decisions recorded below. Follows `2026-10-07-multisite-garage.md`, which
listed these as not yet built. Every claim about Garage is about
`dxflrs/garage:v1.0.1` and is cited to its source at that tag or observed on
real containers.

## Scope

In:

* A site that runs only Garage, declared with a new `storage` role.
* Per-site Garage capacity, so nodes of different sizes can share a layout.
* An opt-in node-stop failover check in `storage add`.

Out, by founder decision: **removing or replacing a Garage node.** What was
learned while deciding, for whoever builds it:

* `garage layout remove <id>` then `layout apply` moves a node's role out of
  the layout. Metadata migrates with the layout (`layout history` showed a
  single live version within seconds on a small cluster), but blocks do not
  leave at once: a block's reference on the removed node becomes deletable
  only after `BLOCK_GC_DELAY`, 600 seconds (`src/block/manager.rs:52`), and is
  then offered to the remaining nodes and deleted (`src/block/resync.rs:369-453`).
  **Observed:** three nodes at replication 2, 30 objects, one node removed.
  Its 20 blocks stayed for over six minutes and were gone between about 10
  and 20 minutes after the apply, `garage stats` on it then reporting "number
  of RC entries: 0". A gate would read that, and wait.
* `garage layout assign <new> --replace <old>` stages the old node's removal
  and the new node's role in one version (`src/garage/cli/layout.rs:75-94`).
* A dead node needs `layout skip-dead-nodes`, with `--allow-missing-data` as
  the decision to lose what only it held (`layout.rs:412-469`).

## Decisions (founder)

1. **A storage-only site has the role `storage`.** Rejected: accepting
   `roles: []` for a site listed in `storage.garage.sites`, which keeps one
   source of truth but makes an empty role list mean something. The role
   makes the host's purpose readable in the site's own entry.
2. **Per-site capacity is `storage.garage.capacities`**, a map from site to
   size; `storage.garage.capacity` stays the default for sites it does not
   name. Rejected: a field on each site, which spreads Garage's configuration
   across site entries.
3. **`storage add --stop-test` runs only where uploads survive one node
   down**: replication 3 or more with at least three Garage sites, or
   replication 2 or more at consistency `dangerous`. Elsewhere it is refused.
   Under `dangerous` it creates the single-copy window decision 1 of the
   multi-site spec accepted, for about a minute.
4. **Removing or replacing a node is deferred** (above).

## The `storage` role

* `storage` joins `data`, `apps`, `gateway` and `witness` as a known role.
* **Refused:** a site with the `storage` role that `storage.garage.sites`
  does not list. The role would promise Garage and the render would give it
  none. The converse is allowed: a `data` or `apps` site runs Garage when the
  list names it, as today, and needs no `storage` role.
* What a storage-only site renders: WireGuard, its firewall rules, and an
  infrastructure stack holding Garage alone. `infra-compose.yaml.tmpl`
  already renders each service from the site's flags, so nothing else
  appears. `host prepare` gives it the mesh rules every site has and nothing
  for apps or the gateway.
* `site add` stays scoped to `[data]`; a storage host joins through `apply`
  and `storage add`, as the multi-site spec's growth path describes.

## Capacities

```yaml
storage:
  garage:
    sites: [store, home-a, home-b]
    replication: 3
    capacity: 100G          # the default
    capacities:
      store: 2T
```

* **Refused:** a key that is not a Garage site, and a value that is not a
  positive size with a unit.
* `storage add`'s layout stage assigns each node its own capacity. A node
  already in the layout at another capacity is assigned again: Garage keeps
  its zone and changes only the capacity when an existing role is assigned
  with `-c` (`src/garage/cli/layout.rs:104-121`), and the next `layout apply`
  rebalances. The sync gate then waits as for a join.
* **Compared in bytes, with a tolerance.** `layout show` prints capacity in
  decimal units to one decimal place. Observed: `3G` and `3GB` show as
  `3.0 GB`, `3GiB` as `3.2 GB`, `1T` as `1000.0 GB`, `500M` as `500.0 MB`. So
  the configured value and the shown one are both converted to bytes (`G`,
  `GB` decimal; `GiB` binary) and a node is reassigned only when they differ
  by more than 2%, which is the most one decimal place can hide on any value
  this deployment will use.

## `storage add --stop-test`

Appended to the smoke stage, after the probe has read back everywhere and
before it is deleted:

1. Stop Garage on the **last** site in `storage.garage.sites`, never the
   first, which serves media and takes the apps' writes.
2. Read the probe through every remaining node's S3 API and web endpoint and
   through its app's media hostname.
3. Write a second probe through the first node and read it back, which is
   the point of the decision: uploads must survive.
4. Start Garage on the stopped site again, and gate on every node listing
   every other as healthy.

The node is started again even when a read fails, so the failure is the only
thing left to fix. The second probe is deleted with the first.

Refused, before anything stops, when the configuration does not keep uploads
working with one node down (decision 3), naming the rule.

## Testing

* Validation: each new refusal, by fixture.
* Render: a storage-only site's tree has `wg0.conf` and an infrastructure
  compose file with Garage alone.
* `storage add`: capacities assigned and reassigned on a change, not
  reassigned within the tolerance; the stop test's refusal, the node
  restarted after a failed read, and a passing run.
* The Docker suite gains a third node with its own capacity and runs the stop
  test.

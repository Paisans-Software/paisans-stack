# Running the monitor behind your own web server

The uptime monitor runs on a site of its own, with the `monitor` role, and
serves its own hostname rather than going through the gateway. By default the
toolkit runs its own Caddy there (`ingress` mode `paisans`), which needs ports
80 and 443 to itself. This guide is for the other case: the machine you want
the monitor on already runs a web server on 80 and 443, and that web server is
going to stay in front of it.

In this mode the toolkit runs the monitor's container and nothing in front of
it. Your web server terminates TLS and proxies to the container. The
certificate, and renewing it, are yours: the toolkit cannot know how your
server obtains one, and it does not edit a configuration it does not own.

`README.md` has the reasoning, under *The monitor has a site of its own*.

## 1. Declare the site

```yaml
sites:
  watch:
    roles: [monitor]
    address: 10.44.0.4                # its mesh address
    endpoint: watch.example.org:51820
    public_address: 203.0.113.20      # what the internet reaches it on
    ingress:
      mode: external
      listen: 127.0.0.1:8480          # where your web server proxies to

apps:
  status:
    kind: uptime
    hostname: status.example.org
    placement: { pinned: watch }
    settings:
      admin_group: admins
```

**Choose `listen` carefully.** Docker publishes a port with its own iptables
rules, which sit in front of ufw, so the firewall does not protect a published
port. Use `127.0.0.1` whenever the web server runs on the same machine: then
only that machine can reach the app. `paisans validate` refuses a public
address, and warns about a private LAN address or the site's mesh address,
because anything that can reach those reaches the app around your web server.
Pick a port nothing else on the machine uses; `validate` refuses one the
toolkit already binds there.

The site needs no other role, and should have none of `gateway` or `witness`,
which are refused: the monitor exists to report their failures.

## 2. Check the configuration and apply it

```sh
paisans validate
paisans host prepare --site watch            # shows what it would do
paisans host prepare --site watch --execute
paisans apply --site watch                   # shows what would change
paisans apply --site watch --execute
```

`host prepare` and `apply` start with the host check. A host already running a
web server is *shared*, which is fine here: nothing the monitor claims is
taken. On a shared host the toolkit leaves ufw's default policy alone, so ufw
must already be active and deny incoming by default, and it only adds its own
rules, each commented `paisans:`. It does not open 80 or 443 in this mode;
your web server's rules are yours.

## 3. Get the hand-off sheet

```sh
paisans ingress show --app status
```

It prints, from `paisans.yaml` alone:

* the hostname, the upstream (`http://127.0.0.1:8480`) and the health path,
  `/healthz`;
* what your web server must do:
  * terminate TLS for the hostname, with a certificate you obtain and renew;
  * proxy every path to the upstream, but for the four below;
  * pass the `Host` header through unchanged;
  * set `X-Forwarded-For` to the client's address and `X-Forwarded-Proto` to
    `https`, because the monitor's sign in rate limit is keyed on the client's
    address;
  * refuse `/status*`, `/badge/*`, `/metrics` and `/api/v1/*`, as the
    toolkit's own edge does: the status page lists every monitor, mesh
    addresses included, and `/metrics` is public whenever no API token exists;
* a filled in snippet for Caddy, nginx and Apache. Each has a line marked
  `YOUR CERTIFICATE` where your own certificate lines go;
* a warning when `listen` is not loopback.

Copy the snippet for your server into its configuration, put your certificate
lines where it says, and reload the server.

## 4. Publish the name

The hostname must resolve to the monitor's `public_address`, not the
gateway's.

```sh
paisans dns init              # shows the records it would create
paisans dns init --execute
```

`dns init` points every hostname of an app pinned to a monitor site at that
site's `public_address`, and at `public_address6` too when it is set. If you
manage DNS by hand, create the same records.

## 5. Check it from your machine

```sh
paisans ingress check --app status
```

It changes nothing and reaches no host over ssh: it looks at the monitor as a
visitor would, and reports each item as `PASS` or `FAIL` with what fixes it.

| Check | Passes when | When it fails |
|---|---|---|
| `dns` | the hostname resolves to `public_address` (and `public_address6`) | create the records, step 4 |
| `certificate` | `https://<hostname>/healthz` answers 200 with a certificate valid for the hostname; prints the days left | the certificate lines, step 3, or the proxy if the answer is not 200 |
| `sign in` | `/login/oidc` redirects to the identity provider with a callback under `https://<hostname>/` | a redirect back to `/login` means the app's client at Pocket ID is missing (`paisans oidc client create --app status`) or the identity provider is down; a wrong callback means apply has not reached the app, or the web server rewrites `Host` or `Location` |
| `upstream` | a connection to `public_address` on the `listen` port fails | the app is reachable around your web server: use a loopback `listen`, or drop the port in Docker's `DOCKER-USER` chain |

Run it again after every change until every line says `PASS`.

## Afterwards

The monitor checks its own public URL, `https://<hostname>/healthz`, every
minute, and the fork warns 14 days before the certificate there expires. In
this mode that check is the only thing that notices a renewal that silently
stopped, so keep the monitor's email channel working.

Changing your web server, its certificate or its address needs nothing from
the toolkit. Changing `listen` does: edit `paisans.yaml`, apply the site, and
update the upstream in your web server to match, then run `ingress check`
again.

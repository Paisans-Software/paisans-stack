# Named keys, and key claims shared between deployments

Date: 2026-10-10. Status: approved by the founder in session.

Several deployments can share one host and one login user, and so one
`authorized_keys`. Each deployment's `host prepare` records the keys it
manages in its own sidecar, `/etc/paisans/authorized_keys.<user>.paisans-<token>.owned`.
Until now a deployment that stopped listing a key deleted its line whenever
its own sidecar recorded it, without looking at any other deployment's
sidecar. A key one deployment wrote and another still needed was deleted from
under the second, which then could not log in. This makes a key line's removal
depend on every deployment's claim, and gives each key a name an operator can
trace from the host back to `paisans.yaml`.

## Scope

In: the `ssh` section of `paisans.yaml`, the sidecar format, `host prepare`'s
plan for authorized keys, the lock around every write to a sidecar or to
`authorized_keys`, `site remove`'s deletion of its own sidecars, the example
and the README.

Out:

* **`authorized_keys` itself.** Lines are still written exactly as pasted.
  Neither the name nor any claim is written into a key's comment.
* **Keys paisans did not write.** A key already in `authorized_keys` that no
  sidecar lists (the provider's, or one added by hand) is never recorded,
  renamed or removed, as before.
* **Converting a file or a sidecar.** An `ssh.public_key` is refused with the
  section to write instead. A sidecar line in an older format is not a claim
  this deployment can act on (see *Sidecar*).

## `paisans.yaml`

`ssh.public_key` is replaced by `ssh.keys`, a mapping from a key's name to its
`.pub` line:

```yaml
ssh:
  user: ubuntu
  keys:
    alice: ssh-ed25519 AAAA... alice@laptop
    bob: ssh-ed25519 AAAA... bob@desktop
```

| Rule | Because |
|---|---|
| at least one key | `host prepare` makes these the user's authorized keys, and the transport offers only these |
| a name matches `^[a-z0-9][a-z0-9._-]{0,31}$` | it goes into a command line and a file on the host |
| names are unique | a mapping cannot hold one twice; yaml refuses the file |
| one key under two names is refused | a key's identity is its fingerprint, and the sidecar holds one name for it |
| a line is a plain OpenSSH public key, with no options | unchanged from `public_key`: a restricted key is added by hand |
| `ssh.public_key` is refused, and the refusal prints the `keys` section to write, each name taken from the part of the key's comment before `@` | there is one way to write a site's access |

The name is the deployment's own label for the key. Two deployments may call
the same key by different names; each sidecar keeps its own.

## Sidecar

One line per key, three fields separated by single spaces:

```
<mark> <fingerprint> <name>
```

| Mark | Meaning |
|---|---|
| `added` | this deployment appended the line to `authorized_keys` |
| `shared` | the line was already there, and another deployment's sidecar listed the key when this one recorded it |

The `.pub` comment is not kept: the line in `authorized_keys` still carries it,
and the fingerprint links the two. A line with any other shape, including an
older format, is not one of this deployment's claims. Another deployment's
sidecar counts as claiming a key when its file contains the key's fingerprint
anywhere, whatever the line's shape, so that a format this version does not
read keeps a key rather than losing it.

## `host prepare`

For each listed key, by the plain (option free) lines of `authorized_keys`:

| `authorized_keys` | This sidecar | Another sidecar | Plan | What happens |
|---|---|---|---|---|
| no plain line, no line with options | | | `add` | appended verbatim, recorded `added` |
| the key | lists it | | `present` | nothing; the name is rewritten if it changed |
| the key | does not list it | lists it | `share` | recorded `shared` |
| the key | does not list it | does not list it | `present` | nothing, never recorded |
| the key, only with options | | | `present (not paisans)` | nothing |

For each key this sidecar lists that the configuration no longer does:

| `authorized_keys` | Mark | Another sidecar | Plan | What happens |
|---|---|---|---|---|
| no plain line | any | | `forget` | dropped from this sidecar |
| the key | `shared` | | `release` | dropped from this sidecar, the line kept |
| the key | `added` | lists it | `release` | dropped from this sidecar, the line kept |
| the key | `added` | does not list it | `remove` | the line deleted, last of all, then dropped from this sidecar |

A line is therefore deleted only when this deployment added it and no other
deployment claims it. A key whose last claim is released stays on the host,
recorded nowhere, until someone deletes it by hand.

The guards that already hold still hold: removals run after every addition and
after the firewall; a removal goes through a temporary file renamed into place;
a plan that would leave the user with none of the listed keys is refused; the
key the current connection used is listed, so it is not removed.

## The lock

The plan reads the sidecars before the steps run, and another deployment's
`host prepare` or `site remove` can change them in between. So every command
that writes a sidecar or `authorized_keys` runs under
`flock /etc/paisans/authorized_keys.lock`, one lock for every user and every
deployment on the host, and a `remove` decides again inside the lock: it
deletes the line only if no other sidecar for the user then contains the
fingerprint, and otherwise only drops this sidecar's claim. A sidecar that
cannot be read inside the lock counts as a claim, so an error keeps the key.

## `site remove`

Unchanged in what it does to keys: it never edits `authorized_keys`, and it
deletes this deployment's sidecars, for every user, with no step of its own.
The deletion runs under the same lock.

## Example and README

`examples/paisans.example.yaml` and every fixture use `ssh.keys`. The README's
*host prepare manages the login user's authorized keys* gets both tables above,
the sidecar format, the lock, and how to trace a key:
`sudo grep -H <fingerprint> /etc/paisans/*.owned`.

## Testing

* `config`: names, a bad name, one key under two names, an empty `keys`, a
  `public_key` refused with the section to write.
* `hostprep`: `add` records `added` with the name; `share` when another
  sidecar lists a present key; `present` and nothing recorded when none does;
  `release` for `shared`; `release` for `added` claimed elsewhere; `remove`
  for `added` claimed nowhere, its command taking the lock and rechecking the
  other sidecars; a renamed key rewrites the sidecar; another user's sidecar is
  not a claim; an older-format line is not this deployment's claim but is
  another deployment's.
* `siteremove`: the sidecars are deleted under the lock, and
  `authorized_keys` is untouched.

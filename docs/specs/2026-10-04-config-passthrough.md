# A deployment can set a config key the toolkit does not know

Status: approved in session on 2026-10-04, not implemented.

Every value an application reads today has to be a value the toolkit already
reasons about. This lets a deployment set one the toolkit has never heard of,
in whatever file that application actually reads, without forking a template.

## Why

`settings` looks general and is not. Each key only works because some template
asks for it by name:

```go
Bucket: v.Setting("s3_bucket", planned.Name+"-uploads")
Region: v.Setting("s3_region", "garage")
```

A key no template reads is silently ignored. So an adopter who wants
WriteFreely's `app.max_blogs`, or Synapse's `url_preview_enabled`, or one more
variable in Mbin's environment, has exactly two options: fork the toolkit's
templates, or edit the rendered file on the host and have the next `apply`
overwrite it. The second is worse, because `apply`'s conflict gate will then
refuse the whole stack.

This is the gap an adopter hits earliest and most often, because the toolkit's
opinions cover what a community needs to work and not what a particular
community wants.

## What `config` is, and what it is not

A new per app map, beside `settings` rather than replacing it:

```yaml
apps:
  blog:
    config:
      app.max_blogs: 3
      email.enabled: true
  chat:
    config:
      url_preview_enabled: true
      retention.enabled: true
  talk:
    config:
      KBIN_META_DESCRIPTION: A place to talk
```

**`settings` stays what it is: inputs the toolkit reasons about.** It reads
them, validates some of them, and decides things with them. `s3_bucket` is a
setting because `validate` refuses one named `outline` and the provisioner
creates it.

**`config` is passthrough.** The toolkit does not interpret the key. It places
it, in the right syntax, in the file that kind renders, and otherwise has no
opinion about it. That boundary is the whole point: a mechanism that let an
adopter reach anything would also let them reach the things this toolkit
deliberately decides.

## Four formats, one rule each

The kinds do not share a configuration format, so a dotted key means something
different per kind. This is a fact about the applications rather than a choice.

| Format | Kinds | A key means |
|---|---|---|
| env | mbin, outline, pocket-id, oauth2-proxy | the variable name. **A dot is refused**: env has no nesting, so `a.b` is a typo rather than a path |
| ini | writefreely | exactly `section.key`, one dot, no more |
| yaml | synapse, in `homeserver.yaml` | a nested path, any depth |
| json | element, in `config.json` | a nested path, any depth |

A kind that renders no config file at all accepts no `config` keys, and saying
so is a refusal rather than silence.

## The merge happens in Go, after rendering

The template renders first. The operator's keys are then merged into its
output, in that format's own syntax.

The rejected alternative was a trailing block in each template. It cannot nest
a yaml path, cannot put an ini key inside an existing section, and cannot
notice that the template already wrote the same key. All three matter.

* **yaml and json** parse, set the path, and re-emit. `yaml.v3` is already a
  direct dependency and `encoding/json` is stdlib, so neither adds one.
* **env** appends a labelled block. Order does not matter and later
  assignments do not shadow earlier ones in a file nothing sources twice.
* **ini inserts into the existing section** rather than appending a second one
  with the same name. That is deliberate: whether a repeated `[section]` merges
  or shadows is a property of whichever ini parser the application uses, and
  this project has twice been wrong about a format's behaviour by reasoning
  instead of running it. Inserting into the section that is already there needs
  no such assumption. The writer preserves comments and blank lines, because
  the rendered file explains itself and a merge that strips that is a
  regression.

Nothing here parses to a general data structure and re-emits for env or ini.
Both files are line oriented, the insertion is small and testable, and a full
round trip would reformat a file an operator reads.

## A key the template already writes is refused

New refusal, **`config-key-already-rendered`**. It names the key, the file and
the template that owns it, and when a sanctioned `settings` input exists for
the same value it names that instead.

Two sources of truth for one value is how a deployment ends up with a setting
nobody can locate. It is also how the toolkit's own decisions get quietly
undone: `oidc_providers` back into a homeserver that is supposed to be a
resource server, `type = sqlite3` into a blog that is supposed to be in the
cluster, `uploads.enabled = false` into the one that just had it turned on.
Those are refusals in `validate` precisely because they are not an operator's
to make casually, and a passthrough that overrode a rendered key would route
around every one of them.

**The cost, stated rather than buried:** an operator cannot change a template
owned default through `config`. Changing one is a `settings` key or a template
change, and both are reviewable. This is the part of the design most likely to
irritate an adopter, and the answer to "I need to override X" is that X should
become a setting, which is a pull request rather than a local edit.

## A credential shaped key is refused

`paisans.yaml` is plaintext and meant to be read, committed and diffed.
`secrets.enc.yaml` is sops encrypted and exists for the other thing. A
passthrough map is exactly where somebody pastes an API token at the end of a
long day.

**`config-key-looks-like-a-secret`** refuses a key whose name contains
`password`, `secret`, `token`, `apikey` or `private_key`, and says which file
the value belongs in.

It is a name heuristic and it will not catch a credential called `foo`. That
is stated in the message and in the README, the same way
`acme-image-is-stock-caddy` refuses the one thing it can prove rather than
claiming to be a policy. A narrow check that says what it is beats a broad one
that cannot be trusted.

## What this does not change

* **`apply` still owns every rendered file.** A passthrough key changes what
  the toolkit renders, not who writes it, so the conflict gate and the manifest
  are untouched. This is the mechanism that replaces editing a file on the
  host, which is what the gate refuses.
* **No existing key moves.** Every `settings` key keeps working and keeps
  meaning what it meant.
* **Secrets stay in the secrets file**, including ones a passthrough key might
  want to reference. Interpolating a secret into a `config` value is not part
  of this and would reintroduce the plaintext problem the refusal above exists
  to prevent.

## Tests

* One golden key per format, so the rendered output for each of the four is
  visible in the tree an operator receives.
* Refusal fixtures: a dotted key on an env kind, a key the template already
  writes, a credential shaped name, and a `config` block on a kind that renders
  no config file.
* Comment and blank line preservation for the ini and env writers, asserted
  against a rendered file rather than a synthetic one.
* **One check against real software.** The ini insertion must be proven to take
  effect, not merely to look right: boot the pinned WriteFreely fork against a
  rendered `config.ini` carrying a passthrough key and confirm the application
  reports the value. Every format claim in the last two branches that was
  reasoned rather than run turned out to need correcting, and ini is the format
  with the most room to be wrong.

## What will remain unverified

The yaml and json merges are proven against their own parsers, which are the
same libraries the applications use in spirit but not the same code. Synapse
reads `homeserver.yaml` with Python's yaml and Element reads `config.json` in a
browser, and nothing here runs either. What the tests can prove is that the
rendered file parses and carries the value at the path asked for, which is the
part the toolkit is responsible for.

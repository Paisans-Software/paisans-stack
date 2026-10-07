# Uptime Monitoring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A pinned-only `uptime` kind that runs the `josephquigley/uptime` fork, seeded from `paisans.yaml` with a public and a direct check per app and a ping per site, sending mail through a declared, per-app-overridable `smtp:` block.

**Architecture:** Two repositories. The fork gains a channel flag, a boot-time seed reconciler, an entrypoint that drops from root, and `TRUST_PROXY`. The toolkit gains config (`smtp`, `KindUptime`, role-less pinned sites), a kind entry with health routes, validation, generated secrets, a template set whose seed file is built in Go, and a firewall rule.

**Tech Stack:** Go 1.26 (toolkit, `go test ./...`), Node 20 + better-sqlite3 (fork, `node scripts/test-*.js`, no test framework), Docker for the fork's image.

**Spec:** `docs/specs/2026-10-07-uptime-monitoring.md`, including its four amendments. Read it first.

## Global Constraints

- Fork repo: `/Users/wash/Developer/paisans.community/src/uptime`, worktrees in its `.worktrees/` (excluded via `.git/info/exclude`). Toolkit worktree: `/Users/wash/Developer/paisans.community/src/paisans-stack/.worktrees/uptime`, branch `feat/uptime-monitoring`.
- Never `git stash`. Never push, tag, or open a pull request without the founder asking: pushing a `v*` tag on the fork publishes an image.
- Kind default image stays `ghcr.io/josephquigley/uptime:1.1.0-oidc.1` (no `v`; the git tag has one).
- App port `3001`. Data at `/data`, bind mounted from `{{ .DataPath }}/data`. Seed at `/seed/monitors.json`, rendered 0600.
- `RETENTION_VACUUM=false`. `OIDC_DISABLE_PASSWORD_LOGIN=false`. `ADMIN_USER=admin`.
- `smtp.security` ∈ {`starttls`, `tls`}; port 1–65535; default port 587 for starttls, 465 for tls.
- Monitor names: `<app> — public`, `<app> — direct (<site>)`, `<site> — ping`. Seeded monitors carry tag `managed`. Interval 60 s, timeout 10000 ms, failure threshold 2.
- Health table (kind → path, expect): pocket-id `/healthz` `204`; outline `/_health` `200`; synapse `/health` `200`; element `/version` `200`; oauth2-proxy `/ping` `200`; mbin `/` `200,302`; writefreely `/` `200,302`; uptime `/healthz` `200`.
- Rule ids: `uptime-needs-an-admin-group` (refuse), `smtp-on-a-kind-without-mail` (refuse), `uptime-without-smtp` (warn).
- Comments in this codebase explain *why*, cite file:line and dates for anything established externally, and never state recalled facts as checked. Match that.

## Review Focus

1. **A seed whose JSON the fork rejects** (a field misspelt, a header object where a string is wanted) must not delete the managed monitor it fails to update: Task 2 tests an invalid entry survives.
2. **A hand-made monitor sharing a seeded name** must not be adopted or overwritten: Task 2 tests it is skipped and left as it was.
3. **An app removed while its monitor has channel links**: links go with the monitor and nothing else changes: Task 2 tests sibling links survive.
4. **Two apps on the monitor's site with the same port** (mbin and writefreely both 8080) must yield one firewall rule, not two: Task 9 tests dedupe.
5. **A deployment with `smtp:` declared but no password anywhere** must be told at `init`, not discovered when the first alert fails: Task 7 tests the owed entry.

---

## Part A — the fork (`src/uptime`)

Setup once: `cd /Users/wash/Developer/paisans.community/src/uptime && npm ci` (compiles better-sqlite3 natively; needed to run the test scripts).

### Task 1: Channel flag "attach to new managed monitors"

Branch `feat/channel-auto-attach` from `origin/main`, worktree `.worktrees/channel-auto-attach`.

**Files:**
- Modify: `src/lib/migrations.js` (before `logger.info('migrations.complete')`)
- Modify: `src/lib/channels.js` (`createChannel`, `updateChannel`, new `listAutoAttachChannelIds`, exports)
- Modify: `src/routes/channels.js` (`buildPayload` return object)
- Modify: `views/channel-form.ejs` (checkbox beside Enabled)
- Create: `scripts/test-channel-auto-attach.js`

**Interfaces:**
- Produces: `channels.listAutoAttachChannelIds(): Promise<number[]>` (ids ascending); column `channels.auto_attach_managed` 0/1; `createChannel({..., auto_attach_managed})`, `updateChannel(id, {..., auto_attach_managed})` where `undefined` keeps the current value.

- [ ] **Step 1: Failing test** — `scripts/test-channel-auto-attach.js`:

```js
#!/usr/bin/env node
'use strict';

// Exercises the auto_attach_managed channel flag against a throwaway SQLite
// database:   node scripts/test-channel-auto-attach.js

const assert = require('assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'uptime-autoattach-'));
Object.assign(process.env, { DB_DRIVER: 'sqlite', SQLITE_PATH: path.join(dir, 'test.sqlite') });

const db = require('../src/db');
const channels = require('../src/lib/channels');

async function main() {
  await db.ensureSchema();
  await require('../src/lib/migrations').run();

  const email = { to: 'ops@example.test', templates: channels.emptyTemplates() };
  const a = await channels.createChannel({ name: 'a', type: 'email', enabled: 1, config: email, auto_attach_managed: 1 });
  const b = await channels.createChannel({ name: 'b', type: 'email', enabled: 1, config: email });
  assert.deepStrictEqual(await channels.listAutoAttachChannelIds(), [a]);

  // An update that does not mention the flag keeps it: backup import and any
  // other caller written before the flag existed must not clear it.
  await channels.updateChannel(a, { name: 'a2', enabled: 1, config: email });
  assert.deepStrictEqual(await channels.listAutoAttachChannelIds(), [a]);

  await channels.updateChannel(b, { name: 'b', enabled: 1, config: email, auto_attach_managed: 1 });
  await channels.updateChannel(a, { name: 'a2', enabled: 1, config: email, auto_attach_managed: 0 });
  assert.deepStrictEqual(await channels.listAutoAttachChannelIds(), [b]);
  console.log('ok');
}

main().then(() => process.exit(0), (err) => { console.error(err); process.exit(1); });
```

Before writing, read `sanitizeConfig` for type `email` in `src/lib/channels.js` and use the config keys it actually accepts.

- [ ] **Step 2: Run, expect failure** — `node scripts/test-channel-auto-attach.js` → `TypeError: channels.listAutoAttachChannelIds is not a function` (or a SQL error on the missing column).

- [ ] **Step 3: Implement.** Migration:

```js
  // Paisans: a channel that opts in is attached to every monitor a seed file
  // creates (src/lib/seed.js), so an admin who subscribes once hears about
  // apps added later instead of finding them silently unalerted.
  await addColumn('channels', 'auto_attach_managed', 'INTEGER NOT NULL DEFAULT 0', 'TINYINT(1) NOT NULL DEFAULT 0');
```

`channels.js`:

```js
async function createChannel({ name, type, enabled, config, auto_attach_managed }) {
  if (!CHANNEL_TYPES.includes(type)) throw new Error('Invalid channel type');
  const cfg = sanitizeConfig(type, config);
  const result = await db.query(
    `INSERT INTO channels (name, type, enabled, config, auto_attach_managed) VALUES (?, ?, ?, ${db.castJson()}, ?)`,
    [name, type, enabled ? 1 : 0, JSON.stringify(cfg), auto_attach_managed ? 1 : 0]
  );
  logger.info({ channelId: result.insertId, type, name }, 'channels.created');
  return result.insertId;
}

async function updateChannel(id, { name, enabled, config, type, auto_attach_managed }) {
  const cur = await getChannel(id);
  if (!cur) throw new Error('Channel not found');
  const useType = type || cur.type;
  const cfg = sanitizeConfig(useType, config);
  // undefined keeps the stored value, so callers that predate the flag
  // (backup import) cannot clear it by not knowing about it.
  const autoAttach = auto_attach_managed === undefined
    ? (cur.auto_attach_managed ? 1 : 0)
    : (auto_attach_managed ? 1 : 0);
  await db.query(
    `UPDATE channels SET name = ?, enabled = ?, config = ${db.castJson()}, auto_attach_managed = ? WHERE id = ?`,
    [name, enabled ? 1 : 0, JSON.stringify(cfg), autoAttach, id]
  );
  logger.info({ channelId: id, name }, 'channels.updated');
}

// Channels an admin asked to have attached to every newly seeded monitor.
async function listAutoAttachChannelIds() {
  const rows = await db.query('SELECT id FROM channels WHERE auto_attach_managed = 1 ORDER BY id');
  return rows.map((r) => Number(r.id));
}
```

Add `listAutoAttachChannelIds` to `module.exports`. In `src/routes/channels.js` `buildPayload`, add to the returned object `auto_attach_managed: body.auto_attach_managed === '1' || body.auto_attach_managed === 'on' ? 1 : 0`. In `views/channel-form.ejs` after the Enabled `<label>`:

```html
        <label class="form-check form-switch mb-2 ms-3">
          <input class="form-check-input" type="checkbox" name="auto_attach_managed" value="1" <%= channel.auto_attach_managed ? 'checked' : '' %>>
          <span class="form-check-label">Attach to new managed monitors</span>
        </label>
```

- [ ] **Step 4: Run, expect `ok`.** Also re-run `node scripts/test-oidc-users.js` (expect its own success output).
- [ ] **Step 5: Commit** — `Channels: opt in to new managed monitors`.

### Task 2: Seed reconciler

Branch `feat/seed-file` on top of `feat/channel-auto-attach`, worktree `.worktrees/seed-file`.

**Files:**
- Create: `src/lib/seed.js`
- Modify: `src/server.js` (after `migrations.run()`, before `monitor.start()`)
- Create: `scripts/test-seed.js`

**Interfaces:**
- Consumes: `channels.listAutoAttachChannelIds`, `sitePayload.buildPayload/validateForApi/insertSite/updateSite`, `tags.getTagByName/createTag`.
- Produces: `seed.applySeedFile(path) → {settings: string[], monitors: {created, updated, deleted, skipped}|null}`; tag name `managed`.

- [ ] **Step 1: Failing test** — `scripts/test-seed.js` covers: first seed creates three monitors tagged `managed` and attaches only the auto-attach channel; a second seed with a changed URL updates in place and keeps a channel link an admin added; a seed missing one monitor deletes it and nothing else; a hand-made monitor with a seeded name is skipped and untouched; an invalid entry for an existing managed monitor neither updates nor deletes it; a seed without `monitors` touches no monitor; `settings` writes only the keys present, and `smtp_secure`/`status_page_enabled` are stored 0/1.

```js
#!/usr/bin/env node
'use strict';

// Exercises src/lib/seed.js against a throwaway SQLite database:
//   node scripts/test-seed.js

const assert = require('assert');
const fs = require('fs');
const os = require('os');
const path = require('path');

const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'uptime-seed-'));
Object.assign(process.env, { DB_DRIVER: 'sqlite', SQLITE_PATH: path.join(dir, 'test.sqlite') });

const db = require('../src/db');
const channels = require('../src/lib/channels');
const sitePayload = require('../src/lib/sitePayload');
const seed = require('../src/lib/seed');

const file = path.join(dir, 'seed.json');
const write = (obj) => fs.writeFileSync(file, JSON.stringify(obj));
const http = (name, url, extra = {}) => ({
  name, monitor_type: 'active', url, method: 'GET', check_type: 'status',
  expected_status: '200', follow_redirects: false, interval_seconds: 60,
  timeout_ms: 10000, failure_threshold: 2, ...extra,
});
const ping = (name, host) => ({ name, monitor_type: 'ping', ping_host: host, interval_seconds: 60, timeout_ms: 10000, failure_threshold: 2 });
const byName = async (name) => (await db.query('SELECT * FROM sites WHERE name = ?', [name]))[0];

async function main() {
  await db.ensureSchema();
  await require('../src/lib/migrations').run();

  const email = { to: 'ops@example.test', templates: channels.emptyTemplates() };
  const auto = await channels.createChannel({ name: 'auto', type: 'email', enabled: 1, config: email, auto_attach_managed: 1 });
  const manual = await channels.createChannel({ name: 'manual', type: 'email', enabled: 1, config: email });

  // A hand-made monitor that happens to share a name the seed will use.
  const hand = sitePayload.buildPayload(http('docs — public', 'https://hand.example.test/'));
  const handId = (await sitePayload.insertSite(hand, {})).id;

  write({
    settings: { smtp_host: 'smtp.example.test', smtp_port: 465, smtp_secure: true, status_page_enabled: false },
    monitors: [
      http('talk — public', 'https://talk.example.test/'),
      http('talk — direct (home-a)', 'http://10.44.0.1:8080/', { request_headers: { Host: 'talk.example.test' } }),
      ping('home-b — ping', '10.44.0.2'),
      http('docs — public', 'https://docs.example.test/'),
    ],
  });
  let r = await seed.applySeedFile(file);
  assert.deepStrictEqual(r.monitors.created.sort(), ['home-b — ping', 'talk — direct (home-a)', 'talk — public']);
  assert.deepStrictEqual(r.monitors.skipped.map((s) => s.name), ['docs — public']);
  assert.strictEqual((await byName('docs — public')).url, 'https://hand.example.test/');
  assert.strictEqual((await db.query('SELECT COUNT(*) AS c FROM sites WHERE name = ?', ['docs — public']))[0].c, 1);

  const s = (await db.query('SELECT * FROM settings WHERE id = 1'))[0];
  assert.strictEqual(s.smtp_host, 'smtp.example.test');
  assert.strictEqual(Number(s.smtp_port), 465);
  assert.strictEqual(Number(s.smtp_secure), 1);
  assert.strictEqual(Number(s.status_page_enabled), 0);

  const talk = await byName('talk — public');
  assert.deepStrictEqual(await channels.listSiteChannelIds(talk.id), [auto]);
  const direct = await byName('talk — direct (home-a)');
  assert.deepStrictEqual(JSON.parse(direct.request_headers), { Host: 'talk.example.test' });

  // An admin subscribes their own channel; reseeding must not undo it.
  await channels.setSiteChannels(talk.id, [auto, manual]);
  write({ monitors: [
    http('talk — public', 'https://talk2.example.test/'),
    http('talk — direct (home-a)', 'http://10.44.0.1:8080/'),
    ping('home-b — ping', '10.44.0.2'),
  ] });
  r = await seed.applySeedFile(file);
  assert.deepStrictEqual(r.settings, []);
  assert.strictEqual((await byName('talk — public')).id, talk.id);
  assert.strictEqual((await byName('talk — public')).url, 'https://talk2.example.test/');
  assert.deepStrictEqual((await channels.listSiteChannelIds(talk.id)).sort(), [auto, manual].sort());

  // An entry the fork rejects keeps the existing monitor exactly as it was.
  write({ monitors: [
    http('talk — public', 'https://talk2.example.test/'),
    { name: 'talk — direct (home-a)', monitor_type: 'nonsense' },
    ping('home-b — ping', '10.44.0.2'),
  ] });
  r = await seed.applySeedFile(file);
  assert.deepStrictEqual(r.monitors.skipped.map((x) => x.name), ['talk — direct (home-a)']);
  assert.ok(await byName('talk — direct (home-a)'));

  // A removed site's monitor goes; nothing else does.
  write({ monitors: [http('talk — public', 'https://talk2.example.test/'), http('talk — direct (home-a)', 'http://10.44.0.1:8080/')] });
  r = await seed.applySeedFile(file);
  assert.deepStrictEqual(r.monitors.deleted, ['home-b — ping']);
  assert.strictEqual(await byName('home-b — ping'), undefined);
  assert.ok(await byName('docs — public'));
  assert.deepStrictEqual((await channels.listSiteChannelIds(talk.id)).sort(), [auto, manual].sort());

  // No monitors key: nothing about monitors changes.
  write({ settings: { status_page_enabled: false } });
  r = await seed.applySeedFile(file);
  assert.strictEqual(r.monitors, null);
  assert.ok(await byName('talk — public'));
  assert.strictEqual((await byName('docs — public')).id, handId);
  console.log('ok');
}

main().then(() => process.exit(0), (err) => { console.error(err); process.exit(1); });
```

- [ ] **Step 2: Run, expect failure** — `Cannot find module '../src/lib/seed'`.

- [ ] **Step 3: Implement `src/lib/seed.js`:**

```js
'use strict';

// Reconciles settings and monitors from a file the deployment renders and
// mounts read only, named by SEED_FILE. It runs at every boot, after
// migrations and before the monitor starts, so a deployment that changes the
// file and restarts the container converges without anyone opening the UI.
//
// Ownership is by tag. A monitor tagged `managed` belongs to the file: it is
// created, updated and deleted to match it. Anything else belongs to whoever
// made it and is never touched, even when its name collides with one the
// file uses. Channel links on an existing monitor are never touched either:
// they are the admins' subscriptions, and a reseed that reset them would
// undo every admin's choices on every restart (which is what
// backup.importConfig's `replace` path does, so it is not reused here).

const fs = require('fs');
const db = require('../db');
const logger = require('../logger');
const sitePayload = require('./sitePayload');
const tagsLib = require('./tags');
const channels = require('./channels');

const MANAGED_TAG = 'managed';

// The settings the file may carry, and nothing else: a file that could set
// any column could change sign in or branding without anyone deciding to.
const SETTINGS_FIELDS = [
  'smtp_host', 'smtp_port', 'smtp_secure', 'smtp_user', 'smtp_pass',
  'smtp_from_address', 'smtp_from_name', 'status_page_enabled',
];
const BOOLEAN_SETTINGS = new Set(['smtp_secure', 'status_page_enabled']);

async function applySettings(settings) {
  const keys = SETTINGS_FIELDS.filter((k) => Object.prototype.hasOwnProperty.call(settings, k));
  if (!keys.length) return [];
  const values = keys.map((k) => {
    const v = settings[k];
    if (BOOLEAN_SETTINGS.has(k)) return v ? 1 : 0;
    if (k === 'smtp_port') return Number(v) || 587;
    return v == null ? null : String(v);
  });
  await db.query(`UPDATE settings SET ${keys.map((k) => `${k} = ?`).join(', ')} WHERE id = 1`, values);
  return keys;
}

async function managedTagId() {
  const tag = await tagsLib.getTagByName(MANAGED_TAG);
  if (tag) return Number(tag.id);
  return Number(await tagsLib.createTag(MANAGED_TAG));
}

async function reconcileMonitors(monitors) {
  const tagId = await managedTagId();
  const owned = await db.query(
    `SELECT s.id, s.name FROM sites s JOIN site_tags st ON st.site_id = s.id WHERE st.tag_id = ?`,
    [tagId]
  );
  const ownedByName = new Map(owned.map((r) => [r.name, Number(r.id)]));
  const autoChannels = await channels.listAutoAttachChannelIds();
  const summary = { created: [], updated: [], deleted: [], skipped: [] };
  // Every name the file mentions, valid or not: an entry the fork rejects
  // must not cost the deployment the monitor it already has under that name.
  const named = new Set();

  for (const raw of monitors) {
    const name = String((raw && raw.name) || '').trim();
    named.add(name);
    let data;
    let errors;
    try {
      data = sitePayload.buildPayload(raw);
      errors = sitePayload.validateForApi(data, raw);
    } catch (err) {
      errors = [err.message];
    }
    if (errors.length) {
      summary.skipped.push({ name, errors });
      continue;
    }
    const id = ownedByName.get(data.name);
    if (id) {
      // No channelIds and no tagIds: updateSite leaves both as they are.
      await sitePayload.updateSite(id, data, {});
      summary.updated.push(data.name);
      continue;
    }
    const clash = await db.query('SELECT id FROM sites WHERE name = ? LIMIT 1', [data.name]);
    if (clash.length) {
      summary.skipped.push({ name: data.name, errors: ['a monitor not tagged managed already has this name'] });
      continue;
    }
    await sitePayload.insertSite(data, { channelIds: autoChannels, tagIds: [tagId] });
    summary.created.push(data.name);
  }

  for (const [name, id] of ownedByName) {
    if (named.has(name)) continue;
    // ON DELETE CASCADE takes its checks, incidents and channel links.
    await db.query('DELETE FROM sites WHERE id = ?', [id]);
    summary.deleted.push(name);
  }
  return summary;
}

async function applySeedFile(file) {
  const raw = JSON.parse(fs.readFileSync(file, 'utf8'));
  const settings = raw.settings ? await applySettings(raw.settings) : [];
  // Absent means "this file says nothing about monitors", not "none".
  const monitors = Array.isArray(raw.monitors) ? await reconcileMonitors(raw.monitors) : null;
  for (const s of (monitors && monitors.skipped) || []) {
    logger.warn({ name: s.name, errors: s.errors }, 'seed.monitor_skipped');
  }
  // Setting names only: smtp_pass must never reach a log.
  logger.info({ file, settings, monitors: monitors && {
    created: monitors.created.length, updated: monitors.updated.length,
    deleted: monitors.deleted.length, skipped: monitors.skipped.length,
  } }, 'seed.applied');
  return { settings, monitors };
}

module.exports = { applySeedFile, applySettings, reconcileMonitors, MANAGED_TAG };
```

`src/server.js`, after `await require('./lib/migrations').run();`:

```js
  // A deployment that renders its monitors (the paisans toolkit) names the
  // file here. A seed that cannot be read stops the boot on purpose: running
  // on yesterday's monitors without saying so is worse than not running.
  if (process.env.SEED_FILE) {
    await require('./lib/seed').applySeedFile(process.env.SEED_FILE);
  }
```

- [ ] **Step 4: Run** `node scripts/test-seed.js` → `ok`; `node scripts/test-channel-auto-attach.js` → `ok`.
- [ ] **Step 5: Verify the Host override is honoured** (Review Focus has it unverified). Throwaway check, not committed: start `node -e "require('http').createServer((q,s)=>{s.end(q.headers.host)}).listen(18099)"` in the background, then run a one-off script that calls the fork's checker on `http://127.0.0.1:18099/` with `request_headers: {Host: 'talk.example.test'}` (read `src/lib/checker.js` for the exported entry point) and prints the response body. Expect `talk.example.test`. If undici drops it, stop and report: the direct-check design depends on it.
- [ ] **Step 6: Commit** — `Seed settings and managed monitors from SEED_FILE at boot`.

### Task 3: Entrypoint that drops from root, and `TRUST_PROXY`

Same branch `feat/seed-file`.

**Files:**
- Create: `docker-entrypoint.sh`
- Modify: `Dockerfile` (copy entrypoint, `ENTRYPOINT`)
- Modify: `src/server.js:26` (`trust proxy`)
- Modify: `README.md`, `.env.example`, `.env.docker.example` (document `SEED_FILE`, `TRUST_PROXY`)

- [ ] **Step 1: Write `docker-entrypoint.sh`:**

```sh
#!/bin/sh
# Started as root, as the paisans toolkit starts it, this fixes what a
# deployment that writes its files through sudo cannot: /data is a bind mount
# Docker created as root, and SEED_FILE is a root-owned 0600 file because it
# carries the SMTP password. It hands both to `node` and then drops to `node`
# for good. Started as anyone else it does nothing, so the image behaves as it
# always did everywhere else.
set -eu
if [ "$(id -u)" = "0" ]; then
  mkdir -p /data
  chown -R node:node /data
  if [ -n "${SEED_FILE:-}" ] && [ -f "$SEED_FILE" ]; then
    install -o node -g node -m 0600 "$SEED_FILE" /tmp/uptime-seed.json
    SEED_FILE=/tmp/uptime-seed.json
    export SEED_FILE
  fi
  exec setpriv --reuid=node --regid=node --init-groups "$@"
fi
exec "$@"
```

- [ ] **Step 2: Dockerfile** — before `USER node`: `COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh` and `RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh`; change `ENTRYPOINT ["/usr/bin/tini", "--"]` to `ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/docker-entrypoint.sh"]`.
- [ ] **Step 3: `src/server.js`** — replace `app.set('trust proxy', 'loopback');` with:

```js
  // Behind a reverse proxy on another host every request arrives from that
  // proxy, and the login rate limiter is keyed on req.ip: one client could
  // lock every admin out. TRUST_PROXY names the proxy's network (Express's
  // `trust proxy` syntax); unset, nothing but loopback is trusted, as before.
  app.set('trust proxy', process.env.TRUST_PROXY || 'loopback');
```

- [ ] **Step 4: Build and verify** — `docker build -t uptime:seed-test .` then, in a scratch directory with `mkdir data && sudo`-free equivalent `chmod 700 data`, a seed file `seed.json` mode 0600 containing one ping monitor:
  `docker run -d --name uptime-seed-test --user 0:0 --cap-add NET_RAW -e SEED_FILE=/seed/monitors.json -e SESSION_SECRET=x -e ADMIN_PASS=$(openssl rand -hex 16) -v "$PWD/data:/data" -v "$PWD/seed.json:/seed/monitors.json:ro" uptime:seed-test`.
  Expect: `docker logs` shows `seed.applied` with `created: 1`; `docker exec uptime-seed-test id -u` prints `1000`... (exec starts as the image's USER, so instead check `docker top uptime-seed-test` shows the node process as uid 1000); `docker exec uptime-seed-test ping -c1 127.0.0.1` succeeds; `ls -ln data` shows uid 1000. Remove the container and image afterwards.
- [ ] **Step 5: Docs** — README: a short "Seeding from a file" section (ownership by the `managed` tag, channels untouched, auto-attach flag) and `TRUST_PROXY`; add both to the two `.env` examples with one-line comments.
- [ ] **Step 6: Commit** — `Entrypoint: drop from root after handing /data and the seed to node; TRUST_PROXY`.

---

## Part B — the toolkit (`.worktrees/uptime`, branch `feat/uptime-monitoring`)

Run `go test ./...` after every task; it must pass before the commit.

### Task 4: Config — `smtp`, `KindUptime`, role-less pinned sites

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.KindUptime Kind = "uptime"`; `type SMTP struct{Host string; Port int; Security string; Username string; FromAddress string; FromName string}` (yaml `host, port, security, username, from_address, from_name`); `Config.SMTP SMTP`; `App.SMTP *SMTP`; `(c *Config) SMTPFor(app string) SMTP`; consts `SMTPStartTLS = "starttls"`, `SMTPTLS = "tls"`; `(s SMTP) PortOrDefault() int`; `(c *Config) PinnedTo(site string) []string`.

- [ ] **Step 1: Failing tests** in `config_test.go`:

```go
func TestSMTPForInheritsAndOverridesFieldByField(t *testing.T) {
	cfg, err := config.Load(write(t, validConfig+`
smtp:
  host: smtp.example.org
  port: 587
  security: starttls
  username: robot@example.org
  from_address: hello@example.org
  from_name: Fixture
`+"  status:\n    kind: uptime\n    hostname: status.example.org\n    placement: { pinned: home-a }\n    smtp:\n      from_address: alerts@example.org\n      security: tls\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.SMTPFor("status")
	want := config.SMTP{Host: "smtp.example.org", Port: 587, Security: "tls", Username: "robot@example.org", FromAddress: "alerts@example.org", FromName: "Fixture"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if cfg.SMTPFor("talk") != cfg.SMTP {
		t.Fatal("an app without an override did not get the deployment's block")
	}
}

func TestSMTPPortDefaultsBySecurity(t *testing.T) {
	if p := (config.SMTP{Security: "starttls"}).PortOrDefault(); p != 587 {
		t.Fatalf("starttls: %d", p)
	}
	if p := (config.SMTP{Security: "tls"}).PortOrDefault(); p != 465 {
		t.Fatalf("tls: %d", p)
	}
	if p := (config.SMTP{Security: "tls", Port: 2465}).PortOrDefault(); p != 2465 {
		t.Fatalf("declared: %d", p)
	}
}

func TestSMTPShapeIsChecked(t *testing.T) {
	_, err := config.Load(write(t, validConfig+"smtp:\n  host: smtp.example.org\n  security: none\n  port: 70000\n"))
	if err == nil {
		t.Fatal("loaded")
	}
	for _, want := range []string{"smtp.security", "smtp.port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no problem named %s in %v", want, err)
		}
	}
}

func TestARolelessSiteIsAcceptedOnlyWhenSomethingIsPinnedToIt(t *testing.T) {
	site := "  mon:\n    roles: []\n    address: 10.44.0.9\n    ssh:\n      host: mon.local\n      user: ubuntu\n      public_key: |\n        ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA alice@example.org\n"
	withSite := strings.Replace(validConfig, "etcd:\n", site+"etcd:\n", 1)
	if _, err := config.Load(write(t, withSite)); err == nil || !strings.Contains(err.Error(), "sites.mon.roles") {
		t.Fatalf("a role-less site with nothing pinned to it loaded: %v", err)
	}
	pinned := withSite + "  status:\n    kind: uptime\n    hostname: status.example.org\n    placement: { pinned: mon }\n"
	if _, err := config.Load(write(t, pinned)); err != nil {
		t.Fatalf("a role-less site hosting a pinned app was refused: %v", err)
	}
}
```

`validConfig` ends inside `apps:`, so appended app stanzas land there; the `smtp:` block appended after them is a new top-level key. Adjust the string building if the fixture's trailing layout differs.

- [ ] **Step 2: Run** `go test ./internal/config/` → compile failure (`config.SMTP` undefined).
- [ ] **Step 3: Implement.** Add `KindUptime` to the const block and `knownKinds`, and add `uptime` to the unknown-kind message's list. Types and methods:

```go
// SMTP is how an app sends mail. The deployment's block is the default for
// every app that sends any; an app's own block overrides it field by field, so
// one app can use a different sender or a different account without
// repeating the rest. The password is not here: it is a secret, under
// apps.<app>.smtp_password or else external.smtp_password.
type SMTP struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	Security    string `yaml:"security"`
	Username    string `yaml:"username"`
	FromAddress string `yaml:"from_address"`
	FromName    string `yaml:"from_name"`
}

// SMTP security modes. There is no "none": no consumer needs it, and the
// uptime fork cannot express it, since its smtp_secure is nodemailer's
// boolean `secure` and false already means STARTTLS.
const (
	SMTPStartTLS = "starttls"
	SMTPTLS      = "tls"
)

// PortOrDefault is the declared port, or the conventional one for the
// security mode: 465 for implicit TLS, 587 for STARTTLS.
func (s SMTP) PortOrDefault() int {
	if s.Port != 0 {
		return s.Port
	}
	if s.Security == SMTPTLS {
		return 465
	}
	return 587
}

// SMTPFor is an app's effective SMTP settings: the deployment's block with
// every field the app declares replacing the deployment's.
func (c *Config) SMTPFor(app string) SMTP {
	out := c.SMTP
	o := c.Apps[app].SMTP
	if o == nil {
		return out
	}
	if o.Host != "" {
		out.Host = o.Host
	}
	if o.Port != 0 {
		out.Port = o.Port
	}
	if o.Security != "" {
		out.Security = o.Security
	}
	if o.Username != "" {
		out.Username = o.Username
	}
	if o.FromAddress != "" {
		out.FromAddress = o.FromAddress
	}
	if o.FromName != "" {
		out.FromName = o.FromName
	}
	return out
}

// PinnedTo returns the apps pinned to a site, sorted.
func (c *Config) PinnedTo(site string) []string {
	var out []string
	for _, name := range c.AppNames() {
		if p := c.Apps[name].Placement; p.Mode == PlacementPinned && p.Site == site {
			out = append(out, name)
		}
	}
	return out
}
```

Fields: `SMTP SMTP \`yaml:"smtp"\`` on `Config` (after `Storage`), `SMTP *SMTP \`yaml:"smtp"\`` on `App` (after `Gate`), each with a comment. In `structural`, replace the roles-required problem with:

```go
		if len(site.Roles) == 0 && len(c.PinnedTo(name)) == 0 {
			add("sites.%s.roles: required. Give the site at least one of data, apps, gateway, witness. A site may have none only when an app is pinned to it, because then it exists to host that app.", name)
		}
```

and add an `smtpProblems(key string, s SMTP) []string` helper called for `smtp` and each `apps.<name>.smtp` that is non-nil: security must be empty, `starttls` or `tls`; port, when non-zero, 1–65535.

- [ ] **Step 4: Run** `go test ./internal/config/` → PASS; `go test ./...` → PASS (other packages may now fail on every-kind tables; those rows are Task 5. If anything outside `internal/kinds` fails, fix it here).
- [ ] **Step 5: Commit** — `config: smtp block with per-app overrides, uptime kind, role-less sites that host a pinned app`.

### Task 5: Kinds — catalogue, health routes, mail, config file

**Files:**
- Modify: `internal/kinds/kinds.go` (catalogue entry, `configFiles` row, `SendsMail`)
- Create: `internal/kinds/health.go`
- Modify: `internal/kinds/volumes.go` (`ImageVolumes` row), `internal/kinds/volumes_test.go` (kind list)
- Modify: `internal/kinds/kinds_test.go` (config-file table)
- Create: `internal/kinds/health_test.go`

**Interfaces:**
- Produces: `type Health struct{ Path, Expect string }`; `kinds.HealthFor(kind) (Health, bool)`; `kinds.SendsMail(kind) bool`; `kinds.UptimeImage` not exported (catalogue only).

- [ ] **Step 1: Failing tests.** `health_test.go`:

```go
package kinds_test

import (
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// Every kind has a health route recorded, so a new kind cannot be seeded with
// a guessed one: it fails here until somebody establishes it.
func TestEveryKindHasAHealthRoute(t *testing.T) {
	for _, kind := range config.Kinds() {
		h, ok := kinds.HealthFor(kind)
		if !ok {
			t.Errorf("%s has no health route recorded", kind)
			continue
		}
		if !strings.HasPrefix(h.Path, "/") {
			t.Errorf("%s: path %q is not absolute", kind, h.Path)
		}
		for _, code := range strings.Split(h.Expect, ",") {
			if len(code) != 3 {
				t.Errorf("%s: %q is not a comma list of exact codes; the fork has no ranges", kind, h.Expect)
			}
		}
	}
}

func TestPocketIDAnswersNoContent(t *testing.T) {
	if h, _ := kinds.HealthFor(config.KindPocketID); h.Expect != "204" {
		t.Fatalf("pocket-id expects %q", h.Expect)
	}
}

func TestOnlyUptimeSendsMailToday(t *testing.T) {
	for _, kind := range config.Kinds() {
		if got, want := kinds.SendsMail(kind), kind == config.KindUptime; got != want {
			t.Errorf("%s: SendsMail = %v", kind, got)
		}
	}
}
```

Add `config.KindUptime: {kinds.ConfigEnv, ".env"}` to `TestEachKindsConfigFileAndFormat`, and `config.KindUptime` to the list in `TestEveryDefaultImageHasItsVolumesRecorded`.

- [ ] **Step 2: Run** `go test ./internal/kinds/` → FAIL (`HealthFor` undefined).
- [ ] **Step 3: Implement.** Catalogue entry:

```go
	config.KindUptime: {
		// The josephquigley/uptime fork: OIDC sign in gated on a group, and
		// (from the release after 1.1.0-oidc.1) a seed file and an entrypoint
		// that drops from root. Its database is SQLite under /data, which is
		// why the kind is pinned only.
		//
		// NOTE THE MISSING `v`: the git tag is v1.1.0-oidc.1 and
		// docker/metadata-action's semver pattern strips it. Checked against
		// ghcr.io on 2026-10-07 by requesting the manifest for this exact tag
		// anonymously: 200, an OCI image index for linux/amd64 and arm64.
		{Name: "app", Image: "ghcr.io/josephquigley/uptime:1.1.0-oidc.1", Purpose: "the monitor"},
	},
```

`configFiles`: `config.KindUptime: {ConfigEnv, ".env"}`. `ImageVolumes`: `"ghcr.io/josephquigley/uptime:1.1.0-oidc.1": {"/data"},` (inspected 2026-10-07: `{"/data":{}}`). `SendsMail`:

```go
// SendsMail reports whether a kind reads the deployment's smtp settings. An
// app-level smtp block on any other kind is refused, because an override
// nothing reads is ignored without a word. Pocket ID keeps its SMTP settings
// in its own database today; rendering them from the same block would add it
// here.
func SendsMail(kind config.Kind) bool { return kind == config.KindUptime }
```

`health.go`: a `Health` type and a `health` map with exactly the Global Constraints health table, each row commented with its evidence as recorded in the spec's last amendment (file:line at tag, or "ran the image", date 2026-10-07), and `HealthFor`.

- [ ] **Step 4: Run** `go test ./internal/kinds/` → PASS (`TestDefaultsArePinned` accepts `1.1.0-oidc.1`; if it does not, read why before changing anything). `go test ./...`.
- [ ] **Step 5: Commit** — `kinds: uptime, with health routes for every kind and SendsMail`.

### Task 6: Validate — admin group, smtp on a kind without mail, uptime without smtp

**Files:**
- Modify: `internal/validate/validate.go` (three checkers, registered in `Check`)
- Create: `internal/validate/testdata/uptime-needs-an-admin-group.yaml`, `smtp-on-a-kind-without-mail.yaml`, `uptime-without-smtp.yaml` (each `valid.yaml` plus one change)
- Modify: `internal/validate/validate_test.go` (three `TestRulesFire` rows)

- [ ] **Step 1: Fixtures and table rows.** Each fixture is `valid.yaml` with an `uptime` app `status` pinned to `home-a` and:
  - `uptime-needs-an-admin-group`: a top-level `smtp:` with a host, and no `settings.admin_group`.
  - `smtp-on-a-kind-without-mail`: `admin_group: admins`, top-level `smtp:`, and `smtp: { from_name: Docs }` on the `docs` (outline) app.
  - `uptime-without-smtp`: `admin_group: admins` and no `smtp:` anywhere.
  Rows: `{"uptime-needs-an-admin-group", "uptime-needs-an-admin-group", validate.Refuse}`, `{"smtp-on-a-kind-without-mail", ..., validate.Refuse}`, `{"uptime-without-smtp", ..., validate.Warn}`.
- [ ] **Step 2: Run** `go test ./internal/validate/` → FAIL (rules do not fire).
- [ ] **Step 3: Implement** (register the two refusals beside `homeserverMustBePinned`, the warning beside `pocketIDFileBackend`):

```go
// uptimeNeedsAnAdminGroup refuses an uptime app that names no admin group.
//
// The fork admits exactly the members of the groups it is given and refuses
// everyone else, and group names are the community's own, so there is no
// default to fall back to: without one, nobody but the break glass password
// could ever sign in.
func (c *checker) uptimeNeedsAnAdminGroup() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.Kind != config.KindUptime {
			continue
		}
		if group, _ := app.Settings["admin_group"].(string); strings.TrimSpace(group) != "" {
			continue
		}
		c.refuse("uptime-needs-an-admin-group", fmt.Sprintf("apps.%s.settings.admin_group", name),
			"is required. Name the identity provider group whose members may sign in to the monitor, for example admins. Only that group is admitted, and group names are this community's own, so there is no default.")
	}
}

// smtpOnAKindWithoutMail refuses an app level smtp block that nothing reads.
func (c *checker) smtpOnAKindWithoutMail() {
	for _, name := range c.cfg.AppNames() {
		app := c.cfg.Apps[name]
		if app.SMTP == nil || kinds.SendsMail(app.Kind) {
			continue
		}
		c.refuse("smtp-on-a-kind-without-mail", fmt.Sprintf("apps.%s.smtp", name),
			"is declared, but %s does not read the toolkit's smtp settings, so this override would be ignored without a word. Remove it, or configure that application's mail where it keeps it.",
			app.Kind)
	}
}

// uptimeWithoutSMTP warns that a monitor with no mail server cannot email.
func (c *checker) uptimeWithoutSMTP() {
	for _, name := range c.cfg.AppNames() {
		if c.cfg.Apps[name].Kind != config.KindUptime || c.cfg.SMTPFor(name).Host != "" {
			continue
		}
		c.warn("uptime-without-smtp", fmt.Sprintf("apps.%s.smtp", name),
			"resolves no SMTP host, so no email channel in the monitor can send. It still checks and records incidents. Declare a top level smtp block, or one on this app, or set SMTP in the monitor's own settings page, which the toolkit then leaves alone.")
	}
}
```

- [ ] **Step 4: Run** `go test ./internal/validate/` → PASS, including `TestValidFixtureIsQuiet`.
- [ ] **Step 5: Commit** — `validate: uptime needs an admin group; smtp only on kinds that send mail; warn when uptime cannot email`.

### Task 7: Secrets — generated, required, owed

**Files:**
- Modify: `internal/secretsgen/secretsgen.go` (`appSecretKeys`, `owed`)
- Modify: `internal/render/database.go` (`requiredAppSecrets`)
- Test: `internal/secretsgen/secretsgen_test.go`

- [ ] **Step 1: Failing tests** (read the file's existing helpers first and build the config with them): an uptime app gets `admin_password` and `session_secret` generated and nothing else; `owed` lists `external.smtp_password` when an uptime app resolves an SMTP host and neither `external.smtp_password` nor `apps.status.smtp_password` is set, and does not list it when either is; the uptime app's `oidc_clients.status` owed reason names `https://status.example.org/login/oidc/callback`.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** In `appSecretKeys`:

```go
	if app.Kind == config.KindUptime {
		// ADMIN_PASS is the break glass sign in for when the identity
		// provider is down, which is one of the things this app watches; the
		// fork falls back to the literal "admin" when it is unset, so it is
		// generated rather than optional. SESSION_SECRET signs the session
		// cookie; unset, the fork invents one per boot and every restart
		// signs everyone out.
		keys = append(keys, "admin_password", "session_secret")
	}
```

In `owed`, after the acme token: for any app with `kinds.SendsMail` whose `cfg.SMTPFor(name).Host != ""`, if `secrets.External["smtp_password"] == ""` and the app has no `smtp_password` string in `secrets.Apps[name]`, append `{Name: "external.smtp_password", Why: "issued by the mail provider for <host>. <app> sends mail through it and has no password of its own under apps.<app>.smtp_password"}` once (first such app). In the OIDC loop, for `config.KindUptime` append to `why`: `". Register its redirect URI as https://<hostname>/login/oidc/callback (src/lib/oidc.js:26 in the fork) and restrict the client to the group named in settings.admin_group"`. In `render/database.go` `requiredAppSecrets`, add `config.KindUptime: {{"admin_password", "the fork falls back to the password `admin` when ADMIN_PASS is unset"}, {"session_secret", "without it every restart signs every admin out"}}`.
- [ ] **Step 4: Run** `go test ./...` → PASS (update `examples/secrets.example.yaml` only in Task 10).
- [ ] **Step 5: Commit** — `secrets: uptime's generated secrets, and smtp_password owed when mail is declared`.

### Task 8: Render — the seed, the template set, and the golden tree

**Files:**
- Create: `internal/render/uptime.go` (seed builder)
- Create: `internal/render/uptime_test.go`
- Modify: `internal/render/plan.go` (`appPort[config.KindUptime] = 3001`)
- Modify: `internal/render/appview.go` (`UptimeSeed string`, `SMTP smtpValues` on `appValues`, populated in `values`)
- Create: `internal/render/templates/uptime/compose.yaml.tmpl`, `.env.secret.tmpl`, `monitors.json.secret.tmpl`, `caddy.snippet.tmpl`
- Modify: `internal/render/testdata/deployment.yaml`, `internal/render/testdata/secrets.fixture.yaml`; regenerate `testdata/golden`

**Interfaces:**
- Produces: `(p *planner) uptimeSeed(app string) (string, error)` returning indented JSON ending in a newline; `smtpValues{Host string; Port int; Secure bool; Username, Password, FromAddress, FromName string}` (zero when no host).

- [ ] **Step 1: Failing tests** in `uptime_test.go` (package `render`, so it can call the planner; build the planner the way `render.Build` does, from `testdata/deployment.yaml` plus `secrets.fixture.yaml` with an uptime app `status` pinned to `vm`):
  - every non-uptime app has `<app> — public` at `https://<hostname><health path>` with `follow_redirects: false`, and the gated app's `expected_status` ends `,302`;
  - a clustered app (`talk`) has `talk — direct (home-a)` and `talk — direct (home-b)` at `http://<site address>:<port><path>` with `request_headers` `{"Host": <hostname>, "X-Forwarded-Proto": "https"}`;
  - `home-a — ping` and `home-b — ping` exist, `vm — ping` does not, and no monitor is named `status — …`;
  - `settings.status_page_enabled` is `false` and present; SMTP keys present with `smtp_secure` true for `tls` and the per-app password winning over `external.smtp_password`;
  - with no SMTP host anywhere, the settings object carries `status_page_enabled` only;
  - renaming `talk`'s hostname changes no monitor name.
- [ ] **Step 2: Run** `go test ./internal/render/ -run Uptime` → FAIL.
- [ ] **Step 3: Implement `uptime.go`:**

```go
package render

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/kinds"
)

// The uptime kind's seed file: what the fork reconciles its monitors and
// settings from at every boot. See docs/specs/2026-10-07-uptime-monitoring.md.
//
// Field names are the fork's own column names (src/lib/sitePayload.js
// buildPayload), because the fork feeds each entry through the same builder
// and validator its API uses.
type seedMonitor struct {
	Name             string            `json:"name"`
	MonitorType      string            `json:"monitor_type"`
	URL              string            `json:"url,omitempty"`
	Method           string            `json:"method,omitempty"`
	CheckType        string            `json:"check_type,omitempty"`
	ExpectedStatus   string            `json:"expected_status,omitempty"`
	FollowRedirects  *bool             `json:"follow_redirects,omitempty"`
	RequestHeaders   map[string]string `json:"request_headers,omitempty"`
	PingHost         string            `json:"ping_host,omitempty"`
	IntervalSeconds  int               `json:"interval_seconds"`
	TimeoutMS        int               `json:"timeout_ms"`
	FailureThreshold int               `json:"failure_threshold"`
}

// Two consecutive failures before an incident, so one dropped packet on a
// residential line does not wake anyone.
const (
	seedInterval  = 60
	seedTimeoutMS = 10000
	seedThreshold = 2
)

func (p *planner) uptimeSeed(self string) (string, error) {
	monitorSite := p.cfg.Apps[self].Placement.Site
	placed := AppSites(p.cfg)
	noRedirects := false

	var monitors []seedMonitor
	for _, name := range p.cfg.AppNames() {
		app := p.cfg.Apps[name]
		if app.Kind == config.KindUptime {
			// A monitor cannot report its own death, and a check that can
			// only ever pass is noise.
			continue
		}
		health, ok := kinds.HealthFor(app.Kind)
		if !ok {
			return "", fmt.Errorf("apps.%s: kind %s has no health route recorded in internal/kinds, so the monitor cannot check it", name, app.Kind)
		}
		public := health.Expect
		if app.Gate != "" && app.Gate != "none" {
			// The gate redirects to sign in before the app is reached.
			public += ",302"
		}
		monitors = append(monitors, seedMonitor{
			Name: name + " — public", MonitorType: "active", Method: "GET", CheckType: "status",
			URL: "https://" + app.Hostname + health.Path, ExpectedStatus: public,
			FollowRedirects: &noRedirects,
			IntervalSeconds: seedInterval, TimeoutMS: seedTimeoutMS, FailureThreshold: seedThreshold,
		})
		sites := append([]string(nil), placed[name]...)
		sort.Strings(sites)
		for _, site := range sites {
			monitors = append(monitors, seedMonitor{
				Name: fmt.Sprintf("%s — direct (%s)", name, site), MonitorType: "active", Method: "GET", CheckType: "status",
				URL:            fmt.Sprintf("http://%s:%d%s", p.sites[site].Address, appPort[app.Kind], health.Path),
				ExpectedStatus: health.Expect, FollowRedirects: &noRedirects,
				// What the gateway would send, minus the gateway.
				RequestHeaders:  map[string]string{"Host": app.Hostname, "X-Forwarded-Proto": "https"},
				IntervalSeconds: seedInterval, TimeoutMS: seedTimeoutMS, FailureThreshold: seedThreshold,
			})
		}
	}
	for _, site := range p.order {
		if site == monitorSite {
			continue
		}
		monitors = append(monitors, seedMonitor{
			Name: site + " — ping", MonitorType: "ping", PingHost: p.sites[site].Address,
			IntervalSeconds: seedInterval, TimeoutMS: seedTimeoutMS, FailureThreshold: seedThreshold,
		})
	}

	settings := map[string]any{"status_page_enabled": false}
	if smtp := p.smtpFor(self); smtp.Host != "" {
		settings["smtp_host"] = smtp.Host
		settings["smtp_port"] = smtp.Port
		settings["smtp_secure"] = smtp.Secure
		settings["smtp_user"] = smtp.Username
		settings["smtp_pass"] = smtp.Password
		settings["smtp_from_address"] = smtp.FromAddress
		settings["smtp_from_name"] = smtp.FromName
	}
	out, err := json.MarshalIndent(map[string]any{"settings": settings, "monitors": monitors}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

// smtpFor is an app's effective SMTP settings with its password: the app's
// own smtp_password secret when set, else external.smtp_password.
func (p *planner) smtpFor(app string) smtpValues {
	s := p.cfg.SMTPFor(app)
	if s.Host == "" {
		return smtpValues{}
	}
	password := p.appSecret(app, "smtp_password")
	if password == "" {
		password = p.secrets.External["smtp_password"]
	}
	return smtpValues{
		Host: s.Host, Port: s.PortOrDefault(), Secure: s.Security == config.SMTPTLS,
		Username: s.Username, Password: password, FromAddress: s.FromAddress, FromName: s.FromName,
	}
}
```

`json.MarshalIndent` escapes `—` only if `SetEscapeHTML`... it does not escape non-ASCII; it does escape `<>&`, which no value here contains. Check `AppSites` is the exported function in `defaults.go` (it is). In `appview.go`, add `SMTP smtpValues` and `UptimeSeed string` with comments; in `values`, `v.SMTP = p.smtpFor(planned.Name)` when `kinds.SendsMail(planned.Kind)`, and for `config.KindUptime` set `v.UptimeSeed, err = p.uptimeSeed(planned.Name)` (return the error).

- [ ] **Step 4: Templates.** `compose.yaml.tmpl`:

```yaml
# Rendered by paisans. Do not edit: `paisans apply` overwrites this file.
#
# The uptime monitor, pinned to {{ .App.Site }}. Its monitors are not configured
# here or in its UI: they are seeded from monitors.json, which `apply` renders
# from paisans.yaml, at every start.
#
# OWNERSHIP. It starts as root on purpose. data/ is a bind mount Docker
# creates as root, and monitors.json is a root owned 0600 file because it
# carries the SMTP password. The fork's entrypoint hands both to `node` and
# drops to it for good. An image older than that entrypoint (1.1.0-oidc.1)
# ignores the seed and simply runs as root.
name: paisans-{{ .App.Name }}

services:
  app:
    image: {{ .Image "app" }}
    restart: unless-stopped
    user: "0:0"
    # The image declares its own healthcheck on /healthz (Dockerfile at
    # 1.1.0-oidc.1), which apply's health gate reads.
    env_file:
      - .env
    # ICMP for the per site ping checks.
    cap_add:
      - NET_RAW
    # The mesh address only. Docker's own iptables rules bypass a host
    # firewall, so a port published on every interface of a host with a
    # public address is reachable from the internet whatever ufw says.
    ports:
      - "{{ .MeshAddress }}:{{ .App.Port }}:{{ .App.Port }}"
    volumes:
      - {{ .DataPath }}/data:/data
      - {{ .DataPath }}/monitors.json:/seed/monitors.json:ro
```

`.env.secret.tmpl`:

```
# Rendered by paisans. Do not edit.
#
# Every name below was read from the fork's src/config.js and src/server.js
# at 1.1.0-oidc.1, plus SEED_FILE and TRUST_PROXY from the release after it.
PAISANS_APP={{ .App.Name }}
PAISANS_SITE={{ .App.Site }}
# Not 3000, the fork's default: Outline publishes 3000, and two apps on one
# site publish on the same mesh address.
PORT={{ .App.Port }}
PUBLIC_BASE_URL={{ .PublicURL }}
# The mesh subnet: the gateway reaches this app from another host, and the
# login rate limiter is keyed on the client address.
TRUST_PROXY={{ .TrustedProxies }}
SESSION_SECRET={{ .Secret "session_secret" }}
# The break glass sign in, for when the identity provider is down.
ADMIN_USER=admin
ADMIN_PASS={{ .Secret "admin_password" }}
DB_DRIVER=sqlite
SQLITE_PATH=/data/uptime.sqlite
# A full VACUUM rewrites the whole file every six hours and buys nothing at
# steady state; beside etcd it is the one burst of I/O worth refusing. See
# docs/specs/2026-10-07-uptime-monitoring.md, "Beside etcd".
RETENTION_VACUUM=false
SEED_FILE=/seed/monitors.json
{{- if .OIDC.Present }}
OIDC_ISSUER={{ .OIDC.Issuer }}
OIDC_CLIENT_ID={{ .OIDC.ClientID }}
OIDC_CLIENT_SECRET={{ .OIDC.ClientSecret }}
OIDC_BUTTON_LABEL=Sign in with {{ .Domain }}
# Only this group is admitted; editor and viewer stay unset, so everyone else
# is refused (src/lib/oidc.js roleFromClaims).
OIDC_ADMIN_GROUP={{ .Setting "admin_group" "" }}
OIDC_DISABLE_PASSWORD_LOGIN=false
{{- end }}
```

`monitors.json.secret.tmpl`: `{{ .UptimeSeed }}` (no trailing newline after the action; the seed carries its own). `caddy.snippet.tmpl`:

```
# Rendered by paisans. Do not edit: `paisans apply` overwrites this file.
#
# Routing for {{ .App.Name }}, imported by the gateway's block for
# {{ .Hostname }}. The UI, sign in, the OIDC callback and heartbeat URLs
# (/ping/*) pass. The status page, badges, /metrics and the API are refused
# here whatever the app's own settings say: the status page lists every
# monitor, mesh addresses included, and /metrics is public whenever no API
# token exists, which under this toolkit is always.
@refused path /status* /badge/* /metrics /api/*
respond @refused 404
reverse_proxy{{ range .Upstreams }} {{ . }}{{ end }} {
	header_up X-Forwarded-Proto https
}
```

- [ ] **Step 5: Fixture and golden.** Add to `testdata/deployment.yaml` an `smtp:` block (host `smtp.example.org`, security `starttls`, username, from_address, from_name) and an app `status` (kind `uptime`, hostname `status.example.org`, `placement: { pinned: vm }`, `settings: { admin_group: admins }`, `smtp: { from_name: Example Status }`); to `secrets.fixture.yaml` `apps.status.admin_password`, `apps.status.session_secret` (fixture strings) and `oidc_clients.status`. Run `go test ./internal/render/ -run TestGolden -update` (use the actual golden test's name), then **read** the diff of `testdata/golden`: expect new `vm/srv/status/{compose.yaml,.env,monitors.json}`, `vm/srv/infra/caddy/snippets/status.caddy`, a `status.example.org` block in the Caddyfile, and no other change. Any other change is a bug to understand, not to accept.
- [ ] **Step 6: Run** `go test ./...` → PASS.
- [ ] **Step 7: Commit** — `render: uptime template set with a seed built from the deployment`.

### Task 9: Firewall — the monitor reaches apps on its own site

**Files:**
- Modify: `internal/hostprep/firewall.go` (`ContainerRules`)
- Test: `internal/hostprep/hostprep_test.go`

- [ ] **Step 1: Failing test:**

```go
// A site hosting the monitor lets its containers reach every app port on that
// site's own mesh address, once per port, because the monitor's direct checks
// to a local app arrive on a compose bridge rather than on wg0.
func TestContainerRulesLetTheMonitorReachLocalApps(t *testing.T) {
	cfg := fixture(t)
	cfg.Apps["status"] = config.App{Kind: config.KindUptime, Hostname: "status.example.org",
		Placement: config.Placement{Mode: config.PlacementPinned, Site: "home-a"}}
	var got []string
	for _, r := range hostprep.ContainerRules(cfg, "home-a") {
		if r.Why == "the uptime monitor to apps on this site" {
			got = append(got, fmt.Sprint(r.Port))
		}
	}
	// home-a runs the clustered mbin (8080), outline (3000) and pocket-id
	// (1411), and the pinned writefreely (8080) and oauth2-proxy (4180):
	// 8080 twice, listed once.
	want := "1411,3000,4180,8080"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
}
```

`3001` is absent because the monitor does not check itself.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** Restructure `ContainerRules` so the existing apps-role rules stay as they are and a monitor rule set is appended for any site an `uptime` app is pinned to:

```go
func ContainerRules(cfg *config.Config, name string) []Rule {
	site, ok := cfg.Sites[name]
	if !ok {
		return nil
	}
	var rules []Rule
	if site.Has(config.RoleApps) {
		// … the existing body, unchanged, appending to rules …
	}
	return append(rules, monitorRules(cfg, name, site)...)
}

// monitorRules lets an uptime monitor's direct checks reach the apps that run
// on its own site. To another site they leave over wg0, which is let in whole;
// to its own site they arrive on a compose bridge, where the default deny drops
// them, and on a single site that would fail every direct check.
func monitorRules(cfg *config.Config, name string, site config.Site) []Rule {
	hosts := false
	for _, app := range cfg.PinnedTo(name) {
		if cfg.Apps[app].Kind == config.KindUptime {
			hosts = true
		}
	}
	if !hosts {
		return nil
	}
	ports := map[int]bool{}
	for app, sites := range render.AppSites(cfg) {
		kind := cfg.Apps[app].Kind
		if kind == config.KindUptime {
			continue
		}
		for _, s := range sites {
			if s == name {
				ports[render.AppPort(kind)] = true
			}
		}
	}
	var sorted []int
	for port := range ports {
		sorted = append(sorted, port)
	}
	sort.Ints(sorted)
	var rules []Rule
	for _, port := range sorted {
		rules = append(rules, Rule{Interface: containerBridges, To: site.Address, Port: port, Proto: "tcp", Why: "the uptime monitor to apps on this site"})
	}
	return rules
}
```

Confirm `internal/render` does not import `internal/hostprep` (it does not as of `c3f7eb4`), so the import adds no cycle.
- [ ] **Step 4: Run** `go test ./...` → PASS, including `TestContainerRulesReachOnlyWhatAppsUse`.
- [ ] **Step 5: Commit** — `hostprep: let the uptime monitor reach apps on its own site`.

### Task 10: Example, README, decisions

**Files:**
- Modify: `examples/paisans.example.yaml` (top-level `smtp:`; app `status` pinned to `vm` with `admin_group` and a commented `smtp:` override)
- Modify: `examples/secrets.example.yaml` (`apps.status` generated keys; `oidc_clients.status`; a comment that `external.smtp_password` is now read)
- Modify: `internal/validate/validate_test.go` `TestExampleValidates` (expected warnings now include a second `pinned-app-on-witness`; update its comment to say why)
- Modify: `README.md` (a "Monitoring" section: what is checked, pinned-only, the seed and ownership, VACUUM off, the Caddy refusals, break glass; and the `smtp:` block with per-app overrides and password lookup in the configuration section)

- [ ] **Step 1:** Edit the two examples; run `go test ./...` — `TestExampleValidates`, `TestExampleLoads` and the secretsgen example tests tell you what is still inconsistent. Fix the examples, not the tests, except the one expected-warning string.
- [ ] **Step 2:** README sections, written in the README's voice (reasons, not instructions alone), linking the spec.
- [ ] **Step 3:** `go vet ./... && go test ./...` → PASS.
- [ ] **Step 4: Commit** — `docs: uptime monitoring and the smtp block in the example and README`.

### Task 11: Whole-branch review

- [ ] Dispatch one reviewer over `feat/uptime-monitoring` (toolkit, against `origin/develop`) and `feat/seed-file` (fork, against `origin/main`) with the spec and this plan; fix what it confirms; re-run both test suites.
- [ ] Release the Current State claim: remove it from `## In flight`, add one line to `## Recently done` naming both branches, unpushed, and that the fork release and default-image bump await the founder. Re-read the page after writing.

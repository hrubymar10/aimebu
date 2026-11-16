// The browser's event stream (web/js/events.js) and the update bar (web/js/update-bar.js), against a fake socket in a page of a scratch server:
// a socket that goes quiet is replaced when the server promised heartbeats, one that never promised any is left alone, a page woken after a long
// sleep checks its socket at once, and a reconnect to a different build asks for a reload. Part of `make test-ui`.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { scratch, OWNER, MODULES } = require('./scratch-server.cjs');

async function main() {
  const server = await scratch({ env: MODULES });
  try {
    const page = await server.open({ width: 1000, height: 700, context: await server.context() });
    await page.goto(server.origin);
    const result = await page.evaluate(async () => {
      const { EventStream } = await import('/js/events.js');
      const { UpdateBar, reloadIsSafe } = await import('/js/update-bar.js');
      const out = {};
      const original = window.WebSocket;
      const sockets = [];
      class FakeSocket {
        constructor(url) { this.url = url; this.closed = false; sockets.push(this); }
        close() { this.closed = true; this.onclose?.(); }
        emit(frame) { this.onmessage?.({ data: JSON.stringify(frame) }); }
      }
      window.WebSocket = FakeSocket;
      const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
      const make = () => { sockets.length = 0; const states = []; const stream = new EventStream(() => {}, async () => 0, state => states.push(state), 0); stream.start(); return { stream, states }; };
      try {
        // 1. Quiet after a promise of heartbeats: three missed beats and the socket is replaced (and said so).
        {
          const { stream, states } = make();
          sockets[0].emit({ type: 'ready', seq: 0, heartbeat: 1 }); await stream.pending;
          await sleep(3600);
          out.quiet = { sockets: sockets.length, firstClosed: sockets[0].closed, state: states.at(-1) };
          stream.stop();
        }
        // 2. Heartbeats keep it: the same socket after four seconds.
        {
          const { stream } = make();
          sockets[0].emit({ type: 'ready', seq: 0, heartbeat: 1 }); await stream.pending;
          for (let i = 0; i < 8; i++) { await sleep(500); sockets[0].emit({ type: 'heartbeat' }); }
          out.beating = { sockets: sockets.length, closed: sockets[0].closed };
          stream.stop();
        }
        // 3. No promise, no judgement: a server that sends no heartbeat is never replaced for being quiet.
        {
          const { stream } = make();
          sockets[0].emit({ type: 'ready', seq: 0 }); await stream.pending;
          stream.lastFrame = Date.now() - 600000; await sleep(1500);
          document.dispatchEvent(new Event('visibilitychange'));
          await sleep(100);
          out.unpromised = { sockets: sockets.length };
          stream.stop();
        }
        // 4. Waking after a long sleep checks at once, without waiting for the interval.
        {
          const { stream } = make();
          sockets[0].emit({ type: 'ready', seq: 0, heartbeat: 20 }); await stream.pending;
          stream.lastFrame = Date.now() - 61000;
          document.dispatchEvent(new Event('visibilitychange'));
          await sleep(100);
          out.woken = { sockets: sockets.length, firstClosed: sockets[0].closed };
          // a socket that spoke a moment ago is left alone
          sockets[1].emit({ type: 'ready', seq: 0, heartbeat: 20 }); await stream.pending;
          document.dispatchEvent(new Event('visibilitychange')); await sleep(100);
          out.wokenFresh = { sockets: sockets.length };
          stream.stop();
        }
        // 5. The build: the first one heard is the page's own; another one after a reconnect is reported once.
        {
          const { stream } = make(); const seen = [];
          stream.onVersionChange = (now, was) => seen.push([now, was]);
          sockets[0].emit({ type: 'ready', seq: 0, version: 'v1' }); await stream.pending;
          sockets[0].close(); await sleep(400);
          sockets[1].emit({ type: 'ready', seq: 0, version: 'v1' }); await stream.pending;
          out.sameBuild = seen.length;
          sockets[1].close(); await sleep(700);
          sockets[2].emit({ type: 'ready', seq: 0, version: 'v2' }); await stream.pending;
          sockets[2].close(); await sleep(1200);
          sockets[3].emit({ type: 'ready', seq: 0, version: 'v3' }); await stream.pending;
          out.newBuild = seen;
          stream.stop();
        }
        // 5b. A baseline (the build that served the page) older than the server's is reported on the first ready; one that matches is not.
        {
          const { stream } = make(); const seen = [];
          stream.baseline = () => 'v1'; stream.onVersionChange = (now, was) => seen.push([now, was]);
          sockets[0].emit({ type: 'ready', seq: 0, version: 'v2' }); await stream.pending;
          out.baselineOlder = seen; stream.stop();
          const second = make(); const calm = [];
          second.stream.baseline = () => 'v2'; second.stream.onVersionChange = () => calm.push(1);
          sockets[0].emit({ type: 'ready', seq: 0, version: 'v2' }); await second.stream.pending;
          out.baselineSame = calm.length; second.stream.stop();
        }
        // 5c. The catch-up of a connection opened straight after a snapshot is applied as live events; any other connection's takes a snapshot.
        {
          const run = async (label, setup) => {
            const applied = [], snapshots = [];
            sockets.length = 0;
            const stream = new EventStream(event => { applied.push(event.seq); }, async () => { snapshots.push(1); return 100; }, () => {}, 0);
            await setup({ stream, applied, snapshots });
            stream.stop();
            return { applied, snapshots: snapshots.length, seq: stream.seq };
          };
          // the first connection: three events and ready: applied in order, no snapshot
          out.liveReplay = await run('first', async ({ stream }) => {
            stream.start();
            for (const seq of [3, 4, 5]) sockets[0].emit({ type: 'event', event: { seq } });
            sockets[0].emit({ type: 'ready', seq: 5 }); await stream.pending;
          });
          // more than a few: taken as a gap, a snapshot is read instead
          out.liveReplayGap = await run('gap', async ({ stream }) => {
            stream.start();
            for (let seq = 1; seq <= 101; seq++) sockets[0].emit({ type: 'event', event: { seq } });
            sockets[0].emit({ type: 'ready', seq: 101 }); await stream.pending;
          });
          // a reconnect after a drop: a snapshot, never replayed event by event
          out.reconnectReplay = await run('reconnect', async ({ stream }) => {
            stream.start();
            sockets[0].emit({ type: 'ready', seq: 0 }); await stream.pending;
            sockets[0].close(); await sleep(400);
            sockets[1].emit({ type: 'event', event: { seq: 1 } }); sockets[1].emit({ type: 'event', event: { seq: 2 } });
            sockets[1].emit({ type: 'ready', seq: 2 }); await stream.pending;
          });
          // after snapshot_required the next connection starts from a new snapshot, so its catch-up is live again
          out.afterSnapshotRequired = await run('required', async ({ stream }) => {
            stream.start();
            sockets[0].emit({ type: 'snapshot_required' }); await stream.pending; await sleep(50);
            sockets[1].emit({ type: 'event', event: { seq: 101 } }); sockets[1].emit({ type: 'event', event: { seq: 102 } });
            sockets[1].emit({ type: 'ready', seq: 102 }); await stream.pending;
          });
        }
        // A burst of usage snapshots re-reads Inbox only when the alert
        // revision changes; the usages view still hears every snapshot.
        {
          const { stream } = make(); let usageUpdates = 0, inboxReads = 0;
          const fetchBefore = window.fetch;
          window.fetch = (input, options) => {
            if (String(input) === '/api/inbox') { inboxReads++; return Promise.resolve(new Response('[]', { headers: { 'Content-Type': 'application/json' } })); }
            return fetchBefore(input, options);
          };
          try {
            stream.onUsagesUpdated = () => { usageUpdates++; };
            stream.onInboxAlertsUpdated = () => fetch('/api/inbox');
            sockets[0].emit({ type: 'ready', seq: 0 }); await stream.pending;
            for (let i = 0; i < 30; i++) sockets[0].emit({ type: 'usages_updated', alert_revision: 1 });
            await stream.pending;
            const first = inboxReads;
            sockets[0].emit({ type: 'usages_updated', alert_revision: 2 }); await stream.pending;
            const changed = inboxReads;
            sockets[0].emit({ type: 'usages_updated' }); await stream.pending;
            const legacy = inboxReads;
            sockets[0].emit({ type: 'usages_updated', alert_revision: 2 }); await stream.pending;
            const beforeReconnect = inboxReads;
            sockets[0].close(); await sleep(400);
            sockets[1].emit({ type: 'ready', seq: 0 }); await stream.pending;
            sockets[1].emit({ type: 'usages_updated', alert_revision: 2 }); await stream.pending;
            out.usageInboxReads = { first, changed, legacy, beforeReconnect, afterReconnect: inboxReads, usageUpdates };
          } finally { stream.stop(); window.fetch = fetchBefore; }
        }
      } finally { window.WebSocket = original; }
      // 6. The update bar: it asks, a click reloads, and a hidden page with nothing typed reloads by itself.
      {
        const hidden = value => Object.defineProperty(document, 'hidden', { configurable: true, get: () => value });
        let reloads = 0; const bar = new UpdateBar({ reload: () => { reloads++; } });
        hidden(false); bar.show();
        out.bar = { text: document.querySelector('#update-bar')?.textContent, role: document.querySelector('#update-bar')?.getAttribute('role'), reloadsWhileVisible: reloads };
        document.querySelector('#update-bar button').click(); out.bar.afterClick = reloads;
        const draft = document.createElement('textarea'); draft.value = 'half a thought'; document.body.append(draft);
        out.safe = { withDraft: reloadIsSafe() };
        hidden(true); document.dispatchEvent(new Event('visibilitychange')); out.bar.hiddenWithDraft = reloads;
        draft.value = ''; out.safe.empty = reloadIsSafe();
        document.dispatchEvent(new Event('visibilitychange')); out.bar.hiddenClean = reloads;
        const dialog = document.createElement('dialog'); document.body.append(dialog); dialog.setAttribute('open', ''); out.safe.dialogOpen = reloadIsSafe(); dialog.remove(); draft.remove();
        hidden(false); document.querySelector('#update-bar').remove();
        delete document.hidden;
      }
      return out;
    });
    assert.deepEqual(result.quiet, { sockets: 2, firstClosed: true, state: 'Connecting…' }, 'a socket quiet for three heartbeats is replaced');
    assert.deepEqual(result.beating, { sockets: 1, closed: false }, 'heartbeats keep the socket');
    assert.deepEqual(result.unpromised, { sockets: 1 }, 'a server that never promised heartbeats is not judged by silence');
    assert.deepEqual(result.woken, { sockets: 2, firstClosed: true }, 'a page woken after a long silence replaces its socket at once');
    assert.deepEqual(result.wokenFresh, { sockets: 2 }, 'a socket that spoke a moment ago is kept when the page wakes');
    assert.deepEqual(result.baselineOlder, [['v2', 'v1']], 'a page older than the server says so on its first ready');
    assert.equal(result.baselineSame, 0, 'a page of the same build as the server says nothing');
    assert.deepEqual(result.liveReplay, { applied: [3, 4, 5], snapshots: 0, seq: 5 }, 'the catch-up of the first connection is applied as live events, in order');
    assert.deepEqual(result.liveReplayGap, { applied: [], snapshots: 1, seq: 101 }, 'a catch-up of more than 100 events is a gap and takes a snapshot');
    assert.deepEqual(result.reconnectReplay, { applied: [], snapshots: 1, seq: 100 }, 'a reconnect after a drop takes a snapshot, not a replay');
    assert.deepEqual(result.afterSnapshotRequired, { applied: [101, 102], snapshots: 1, seq: 102 }, 'the connection after a snapshot replays live again');
    assert.deepEqual(result.usageInboxReads, { first: 1, changed: 2, legacy: 3, beforeReconnect: 4, afterReconnect: 5, usageUpdates: 34 }, 'usage bursts read Inbox on alert changes and after a reconnect, with old-server fallback');
    assert.equal(result.sameBuild, 0, 'the same build after a reconnect is not news');
    assert.deepEqual(result.newBuild, [['v2', 'v1']], 'a different build is reported once, against the one the page started with');
    assert.deepEqual(result.bar, { text: 'aimebu was updated. Reload to use the new version.Reload', role: 'alert', reloadsWhileVisible: 0, afterClick: 1, hiddenWithDraft: 1, hiddenClean: 2 }, 'the update bar asks, reloads on a click, and reloads a hidden page only when nothing typed would be lost');
    assert.deepEqual(result.safe, { withDraft: false, empty: true, dialogOpen: false }, 'a draft or an open dialog makes a reload unsafe');
    // The app with the formatted composer on (the default): the draft is in the editor, whose textarea is out of sight, and the chat still knows it.
    {
      const app = await server.open({ width: 1280, height: 900, context: await server.context() });
      await app.evaluateOnNewDocument(() => { window.__exposeChat = true; try { localStorage.setItem('aimebu2.composer.formatting', 'on'); } catch { /* storage off */ } });
      await app.goto(server.origin);
      await app.waitForSelector('#login-view:not([hidden])');
      await app.type('#login-form [name=login]', OWNER.login); await app.type('#login-form [name=password]', OWNER.password);
      await app.$eval('#login-form', form => form.requestSubmit());
      await app.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
      if (await app.$('#welcome-continue')) { await app.waitForSelector('#welcome-continue:not([inert])'); await app.click('#welcome-continue'); }
      await app.waitForSelector('#app-view:not([hidden])');
      // A server with no harness offers the first-run harness card over the app: skipped, as the person's own stored choice.
      await app.evaluate(async () => {
        const csrf = (await (await fetch('/api/me')).json()).csrf_token;
        await fetch('/api/me/preferences', { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ preferences: { harness_setup_skipped: '1' } }) });
      });
      const folder = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'aimebu-events-'));
      execFileSync('git', ['init', '-q'], { cwd: folder });
      const call = (url, method, payload) => app.evaluate(async (url, method, payload) => {
        const csrf = (await (await fetch('/api/me')).json()).csrf_token;
        const response = await fetch(url, { method, headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: payload ? JSON.stringify(payload) : undefined });
        const text = await response.text(); return { status: response.status, body: text ? JSON.parse(text) : null };
      }, url, method, payload);
      const project = (await call('/api/projects', 'POST', { name: 'Drafts', folder })).body;
      const flow = (await call(`/api/projects/${project.id}/team-flow`, 'GET')).body;
      assert.equal((await call(`/api/projects/${project.id}/team-flow`, 'PUT', { key: 'custom', rev: flow.rev })).status, 200);
      await app.reload(); await app.waitForSelector('#app-view:not([hidden])'); // the first project made, the app shows its rooms instead of the onboarding
      const home = (await call(`/api/projects/${project.id}/rooms`, 'GET')).body.find(room => room.kind === 'home');
      await app.waitForSelector(`[data-room-id="${home.id}"] .room-item-name`);
      await app.evaluate(id => document.querySelector(`[data-room-id="${id}"] .room-item-name`).click(), home.id);
      await app.waitForFunction(() => window.__chat?.room);
      await app.waitForSelector('#composer .ProseMirror');
      assert.equal(await app.$eval('#composer textarea', el => getComputedStyle(el).display), 'none', 'the formatted composer keeps its textarea out of sight');
      assert.equal(await app.evaluate(() => window.__chat.hasDraft()), false, 'an empty composer holds no draft');
      await new Promise(resolve => setTimeout(resolve, 800)); await app.bringToFront(); await app.click('#composer .ProseMirror'); await app.keyboard.type('half a thought');
      await app.waitForFunction(() => document.querySelector('#composer textarea').value.includes('half a thought'), { timeout: 5000 }); // the editor keeps the hidden textarea in step
      const draft = await app.evaluate(async () => {
        const { UpdateBar, reloadIsSafe } = await import('/js/update-bar.js');
        let reloads = 0;
        const bar = new UpdateBar({ reload: () => { reloads++; }, hasDraft: () => window.__chat.hasDraft() });
        const hidden = value => Object.defineProperty(document, 'hidden', { configurable: true, get: () => value });
        const out = { has: window.__chat.hasDraft(), safe: reloadIsSafe(document, () => window.__chat.hasDraft()), scraped: reloadIsSafe(document) };
        hidden(true); bar.show(); document.dispatchEvent(new Event('visibilitychange'));
        out.reloadsWithDraft = reloads;
        // a file waiting to go counts too
        window.__chat.pendingAttachments = [{ id: 'att_pending' }];
        out.attachmentCounts = window.__chat.hasDraft();
        window.__chat.pendingAttachments = [];
        window.__bar = bar; window.__reloads = () => reloads; window.__hidden = hidden;
        return out;
      });
      assert.deepEqual(draft, { has: true, safe: false, scraped: true, reloadsWithDraft: 0, attachmentCounts: true }, 'text typed into the formatted editor, or a file waiting to go, keeps the page from reloading itself (the textarea is out of sight, so a scrape of the page cannot see it)');
      await app.keyboard.down('Control'); await app.keyboard.press('KeyA'); await app.keyboard.up('Control'); await app.keyboard.press('Backspace');
      const cleared = await app.evaluate(() => { const had = window.__chat.hasDraft(); document.dispatchEvent(new Event('visibilitychange')); return { had, reloads: window.__reloads() }; });
      assert.deepEqual(cleared, { had: false, reloads: 1 }, 'with the draft gone, a hidden page reloads');
      await app.evaluate(() => { delete document.hidden; });
    }
    // A sign-in from an address that is not a valid escape sequence (#%E0) opens the app like any other: the address names no room.
    {
      const bad = await server.open({ width: 1000, height: 700, context: await server.context() });
      await bad.goto(`${server.origin}/#%E0`);
      await bad.waitForSelector('#login-view:not([hidden])');
      await bad.type('#login-form [name=login]', OWNER.login); await bad.type('#login-form [name=password]', OWNER.password);
      await bad.$eval('#login-form', form => form.requestSubmit());
      await bad.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
      if (await bad.$('#welcome-continue')) { await bad.waitForSelector('#welcome-continue:not([inert])'); await bad.click('#welcome-continue'); }
      await bad.waitForSelector('#app-view:not([hidden])');
      await bad.waitForFunction(() => !document.querySelector('#app-view').hasAttribute('aria-busy') && document.querySelector('#room-title')?.textContent.length > 0, { timeout: 15000 }); // finished starting, with a room open
      await bad.close();
    }
    assert.deepEqual(server.errors, [], 'no page errors');
    console.log('events page: ok');
  } finally { await server.stop(); }
}
main().catch(error => { console.error(error); process.exit(1); });

// Settings -> General in Chromium (appearance, chat switches, message expiry, settings backup): what a person's saved choices show, what
// choosing writes (the page, the browser's storage, the server) and the order of answers, which the test chooses by holding requests.
// web/ is served as is with its import map; no Go server. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const importMap = /<script type="importmap">.*?<\/script>/s.exec(fs.readFileSync(path.join(root, 'index.html'), 'utf8'))[0];
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css">${importMap}</head>
<body><button id="system-events-toggle" hidden>bell</button><dialog id="settings-dialog" open><section id="settings-general" class="settings-section active"></section></dialog></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  // `stored` is what the browser already holds for the person; the server answers every preference request.
  const mount = async (stored = {}, { auto = {}, rights = { owner: true, admin: true } } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async (stored, auto, rights) => {
      localStorage.clear();
      for (const [key, value] of Object.entries(stored)) localStorage.setItem(key, value);
      window.__puts = []; window.__previews = 0;
      // A preference save is answered at once; any other request waits for __answer(index, status, body) unless an `auto` rule answers it.
      window.__requests = []; window.__auto = auto;
      window.fetch = (input, options = {}) => new Promise(resolve => {
        const request = { path: String(input), method: options.method || 'GET', body: options.body ? JSON.parse(options.body) : undefined, answered: false };
        request.answer = (status, body) => { request.answered = true; resolve(new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })); };
        if (request.path === '/api/me/preferences' && request.method === 'PUT') { window.__puts.push(request.body); request.answer(200, { preferences: {} }); return; }
        window.__requests.push(request);
        const rule = window.__auto[`${request.method} ${request.path}`];
        if (rule) request.answer(200, rule);
      });
      window.__answer = (index, status, body) => window.__requests[index].answer(status, body);
      document.addEventListener('aimebu:sidebar-preview', () => { window.__previews++; });
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'u1' });
      await (await import('/js/preferences.js')).loadPreferences('u1', { send: async () => ({ preferences: {} }) });
      const { GeneralSettings } = await import('/js/pages/general/page.js');
      window.__general = new GeneralSettings();
      window.__general.setRights(rights);
    }, stored, auto, rights);
    return page;
  };
  const view = page => page.evaluate(() => ({ theme: document.querySelector('#theme-select').value, attribute: document.documentElement.dataset.theme ?? null, preview: document.querySelector('#sidebar-preview').checked,
    stored: [localStorage.getItem('aimebu2.ui.theme.u1'), localStorage.getItem('aimebu2.ui.sidebar_preview.u1')], puts: window.__puts, previews: window.__previews }));
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 60)));
  try {
    // The block is built at once, as settings rows named and described, with every theme on offer.
    {
      const page = await mount();
      assert.deepEqual(await page.evaluate(() => ({ heading: document.querySelector('#settings-appearance > h3').textContent, group: document.querySelector('#settings-appearance .settings-group').getAttribute('aria-label'),
        options: [...document.querySelectorAll('#theme-select option')].map(option => option.value).join('|'), label: document.querySelector('#theme-select').labels[0].textContent, desc: document.querySelector('#theme-select').getAttribute('aria-describedby'),
        previewLabel: document.querySelector('#sidebar-preview').labels[0].textContent, previewDesc: document.querySelector('#sidebar-preview').getAttribute('aria-describedby'), toggle: document.querySelector('#sidebar-preview').closest('.settings-row').classList.contains('settings-row-toggle') })),
        { heading: 'Appearance', group: 'Appearance', options: 'dark|light|red-dark|red-light|blue-dark|blue-light|green-dark|green-light|high-contrast-dark|high-contrast-light', label: 'Theme', desc: 'theme-select-desc', previewLabel: 'Show the last message under every room', previewDesc: 'sidebar-preview-desc', toggle: true });
      assert.deepEqual(await view(page).then(({ theme, preview }) => ({ theme, preview })), { theme: 'dark', preview: false }, 'dark and off until someone is known');
      await page.close();
    }
    // Signing in shows what that person chose in this browser and applies the theme; a theme that is no longer offered falls back to dark.
    {
      const page = await mount({ 'aimebu2.ui.theme.u1': 'high-contrast-light', 'aimebu2.ui.sidebar_preview.u1': '1' });
      await page.evaluate(() => window.__general.setUser('u1'));
      await settle(page);
      assert.deepEqual(await view(page).then(({ theme, attribute, preview }) => ({ theme, attribute, preview })), { theme: 'high-contrast-light', attribute: 'high-contrast-light', preview: true });
      assert.equal(await page.evaluate(() => localStorage.getItem('aimebu2.ui.last-user')), 'u1');
      await page.close();
      const gone = await mount({ 'aimebu2.ui.theme.u1': 'retired' });
      await gone.evaluate(() => window.__general.setUser('u1'));
      await settle(gone);
      assert.deepEqual(await view(gone).then(({ theme, attribute, preview }) => ({ theme, attribute, preview })), { theme: 'dark', attribute: null, preview: false });
      await gone.close();
    }
    // Choosing a theme applies it, keeps it in the browser for this person and saves it to the account; dark is no attribute.
    {
      const page = await mount();
      await page.evaluate(() => window.__general.setUser('u1'));
      await page.select('#theme-select', 'green-light');
      await settle(page);
      assert.deepEqual(await view(page).then(({ theme, attribute, stored, puts }) => ({ theme, attribute, stored, puts })), { theme: 'green-light', attribute: 'green-light', stored: ['green-light', null], puts: [{ preferences: { theme: 'green-light' } }] });
      await page.select('#theme-select', 'dark');
      await settle(page);
      assert.deepEqual(await view(page).then(({ attribute, stored, puts }) => ({ attribute, stored, puts: puts.length })), { attribute: null, stored: ['dark', null], puts: 2 });
      await page.close();
    }
    // The room-preview switch is kept the same way and tells the room list once per change.
    {
      const page = await mount();
      await page.evaluate(() => window.__general.setUser('u1'));
      await page.$eval('#sidebar-preview', el => el.click());
      await settle(page);
      assert.deepEqual(await view(page).then(({ preview, stored, puts, previews }) => ({ preview, stored, puts, previews })), { preview: true, stored: [null, '1'], puts: [{ preferences: { sidebar_preview: '1' } }], previews: 1 });
      await page.$eval('#sidebar-preview', el => el.click());
      await settle(page);
      assert.deepEqual(await view(page).then(({ preview, stored, previews }) => ({ preview, stored, previews })), { preview: false, stored: [null, '0'], previews: 2 });
      await page.close();
    }
    // Before anyone is known nothing is written to the browser's storage under a person's name, and destroying the block empties it.
    {
      const page = await mount();
      await page.select('#theme-select', 'blue-dark');
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [Object.keys(localStorage).filter(key => key.startsWith('aimebu2.ui.theme.') || key.startsWith('aimebu2.ui.sidebar_preview.')), document.documentElement.dataset.theme]), [[], 'blue-dark']);
      await page.evaluate(() => window.__general.destroy());
      assert.equal(await page.evaluate(() => document.querySelector('#settings-general').childElementCount), 0);
      await page.close();
    }
    // The warm window: an owner types minutes and it is saved when the field is left (one PUT with only that field); a bad value is not sent; anyone
    // else sees the field disabled, with the reason.
    {
      const page = await mount({}, { auto: { 'GET /api/agent-isolation': { docker_only: false, can_edit: true, warm_idle_minutes: 0, can_edit_warm: true } } });
      await page.evaluate(() => window.__general.open());
      await page.waitForFunction(() => !document.querySelector('#agent-warm-idle').disabled);
      assert.equal(await page.$eval('#agent-warm-idle', el => el.value), '0', 'off by default');
      await page.$eval('#agent-warm-idle', el => { el.focus(); el.value = '9999'; el.dispatchEvent(new Event('input', { bubbles: true })); });
      await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50))); // the draft is state: the blur must see the render that follows the typing
      await page.$eval('#agent-warm-idle', el => el.blur());
      await page.waitForFunction(() => /whole number of minutes/.test(document.querySelector('#agent-warm-idle-error').textContent));
      assert.equal(await page.evaluate(() => window.__requests.filter(request => request.method === 'PUT').length), 0, 'a value out of range is not sent');
      await page.$eval('#agent-warm-idle', el => { el.focus(); el.value = '5'; el.dispatchEvent(new Event('input', { bubbles: true })); });
      await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50)));
      await page.$eval('#agent-warm-idle', el => el.blur());
      await page.waitForFunction(() => window.__requests.some(request => request.method === 'PUT' && request.path === '/api/agent-isolation' && !request.answered));
      const put = await page.evaluate(() => window.__requests.findIndex(request => request.method === 'PUT' && request.path === '/api/agent-isolation'));
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body, put), { warm_idle_minutes: 5 });
      await page.evaluate(index => window.__answer(index, 200, { docker_only: false, can_edit: true, warm_idle_minutes: 5, can_edit_warm: true }), put);
      await page.waitForFunction(() => document.querySelector('#agent-warm-idle').value === '5' && !document.querySelector('#agent-warm-idle').disabled);
      assert.equal(await page.$eval('#agent-warm-idle-error', el => el.textContent), '');
      await page.close();
      const admin = await mount({}, { auto: { 'GET /api/agent-isolation': { docker_only: false, can_edit: true, warm_idle_minutes: 5, can_edit_warm: false } } });
      await admin.evaluate(() => window.__general.open());
      await admin.waitForFunction(() => document.querySelector('#agent-warm-idle').value === '5');
      assert.deepEqual(await admin.evaluate(() => [document.querySelector('#agent-warm-idle').disabled, document.querySelector('#agent-warm-idle').title]), [true, 'Only the owner can change this.']);
      await admin.close();
    }
    // The prompt cache keep-alive: on by default; the owner changes one field at a time (one PUT with only that field in keep_alive), a bad value is not
    // sent, and anyone else sees it all disabled with the reason.
    {
      const page = await mount({}, { auto: { 'GET /api/agent-isolation': { docker_only: false, can_edit: true, warm_idle_minutes: 0, can_edit_warm: true, keep_alive: { enabled: true, claude_minutes: 50, codex_minutes: 10, stop_after_hours: 24 } } } });
      await page.evaluate(() => window.__general.open());
      await page.waitForFunction(() => !document.querySelector('#agent-keepalive-enabled').disabled);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#agent-keepalive-enabled').checked, document.querySelector('#agent-keepalive-claude-minutes').value, document.querySelector('#agent-keepalive-codex-minutes').value, document.querySelector('#agent-keepalive-stop-after-hours').value]), [true, '50', '10', '24'], 'on by default, with the defaults');
      const last = () => page.evaluate(() => window.__requests.findLastIndex(request => request.method === 'PUT' && request.path === '/api/agent-isolation'));
      await page.$eval('#agent-keepalive-enabled', el => el.click());
      await page.waitForFunction(() => window.__requests.some(request => request.method === 'PUT' && request.path === '/api/agent-isolation' && !request.answered));
      let put = await last();
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body, put), { keep_alive: { enabled: false } });
      await page.evaluate(index => window.__answer(index, 200, { docker_only: false, can_edit: true, warm_idle_minutes: 0, can_edit_warm: true, keep_alive: { enabled: false, claude_minutes: 50, codex_minutes: 10, stop_after_hours: 24 } }), put);
      await page.waitForFunction(() => !document.querySelector('#agent-keepalive-enabled').checked && !document.querySelector('#agent-keepalive-enabled').disabled);
      await page.$eval('#agent-keepalive-claude-minutes', el => { el.focus(); el.value = '99999'; el.dispatchEvent(new Event('input', { bubbles: true })); });
      await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50)));
      await page.$eval('#agent-keepalive-claude-minutes', el => el.blur());
      await page.waitForFunction(() => /whole number from 0 to 55/.test(document.querySelector('#agent-keepalive-error').textContent));
      const sent = await page.evaluate(() => window.__requests.filter(request => request.method === 'PUT').length);
      assert.equal(sent, 1, 'a value out of range is not sent');
      await page.$eval('#agent-keepalive-claude-minutes', el => { el.focus(); el.value = '30'; el.dispatchEvent(new Event('input', { bubbles: true })); });
      await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50)));
      await page.$eval('#agent-keepalive-claude-minutes', el => el.blur());
      await page.waitForFunction(() => window.__requests.filter(request => request.method === 'PUT').length === 2);
      put = await last();
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body, put), { keep_alive: { claude_minutes: 30 } });
      await page.close();
      const admin = await mount({}, { auto: { 'GET /api/agent-isolation': { docker_only: false, can_edit: true, warm_idle_minutes: 0, can_edit_warm: false, keep_alive: { enabled: true, claude_minutes: 50, codex_minutes: 10, stop_after_hours: 24 } } } });
      await admin.evaluate(() => window.__general.open());
      await admin.waitForFunction(() => document.querySelector('#agent-keepalive-claude-minutes').value === '50');
      assert.deepEqual(await admin.evaluate(() => ['#agent-keepalive-enabled', '#agent-keepalive-claude-minutes', '#agent-keepalive-codex-minutes', '#agent-keepalive-stop-after-hours'].map(id => document.querySelector(id).disabled)), [true, true, true, true]);
      await admin.close();
    }
    // The sections come in the order they have always had, each with the ids the page and its styles know.
    {
      const page = await mount();
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('#settings-general > div')].map(div => `${div.id}:${div.querySelector('h3').textContent}`)),
        ['settings-appearance:Appearance', 'chat-preferences:Chat', 'message-retention:Messages', 'agent-isolation:Agents', 'prompt-cache:Prompt cache', 'settings-backup:Settings backup']);
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('#settings-general .settings-row')].filter(row => !row.hidden).map(row => (row.querySelector('input, select, button')).id)),
        ['theme-select', 'sidebar-preview', 'pref-system-events', 'pref-debug-inspector', 'message-expiry-seconds', 'agent-docker-only', 'agent-warm-idle', 'agent-keepalive-enabled', 'agent-keepalive-claude-minutes', 'agent-keepalive-codex-minutes', 'agent-keepalive-stop-after-hours', 'settings-backup-export', 'settings-backup-file']);
      await page.close();
    }
    // The chat switches show what this person has and change the page at once: the bell and the debug button follow them.
    {
      const page = await mount({ 'aimebu2.ui.show-system-events.u1': '0', 'aimebu2.ui.debug-inspector.u1': '1' });
      await page.evaluate(() => window.__general.setUser('u1'));
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#pref-system-events').checked, document.querySelector('#pref-debug-inspector').checked]), [false, true]);
      await page.$eval('#pref-system-events', el => el.click());
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#system-events-toggle').hidden, localStorage.getItem('aimebu2.ui.show-system-events.u1'), window.__puts.at(-1), document.documentElement.dataset.debugInspector]), [false, '1', { preferences: { system_events: '1' } }, 'on']);
      await page.$eval('#pref-debug-inspector', el => el.click());
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.documentElement.dataset.debugInspector, window.__puts.at(-1)]), ['off', { preferences: { debug_inspector: '0' } }]);
      await page.evaluate(() => { localStorage.setItem('aimebu2.ui.debug-inspector.u1', '1'); window.__general.open(); });
      await settle(page);
      assert.equal(await page.$eval('#pref-debug-inspector', el => el.checked), true, 'showing General again reads the switches');
      await page.close();
    }
    // Message expiry: the window is read each time General is shown; what is typed is kept apart until Save; a bad value is refused before
    // anything is sent; one save at a time; a failed save keeps what was typed; a person who may not edit sees it disabled and told so.
    {
      const read = { messages_considered_expired_after_seconds: 21600, can_edit: true };
      const page = await mount({}, { auto: { 'GET /api/retention': read } });
      await page.evaluate(() => window.__general.open());
      await page.waitForFunction(() => document.querySelector('#message-expiry-seconds').value === '21600');
      const type = value => page.$eval('#message-expiry-seconds', (el, value) => { el.value = value; el.dispatchEvent(new Event('input', { bubbles: true })); }, value);
      await type('5');
      await page.$eval('#message-expiry-save', el => el.click());
      await page.waitForFunction(() => /60/.test(document.querySelector('#message-expiry-error').textContent));
      assert.equal(await page.evaluate(() => window.__requests.filter(request => request.method === 'PUT').length), 0, 'a refused value sends nothing');
      await page.evaluate(() => window.__general.open());
      await settle(page);
      assert.equal(await page.$eval('#message-expiry-seconds', el => el.value), '5', 'reading the window again leaves what is typed');
      await type('3600');
      await page.evaluate(() => { delete window.__auto['GET /api/retention']; document.querySelector('#message-expiry-save').click(); document.querySelector('#message-expiry-save').click(); });
      await page.waitForFunction(() => window.__requests.some(request => request.method === 'PUT' && !request.answered));
      const put = await page.evaluate(() => window.__requests.findIndex(request => request.method === 'PUT'));
      assert.deepEqual(await page.evaluate(() => [window.__requests.filter(request => request.method === 'PUT').length, document.querySelector('#message-expiry-save').disabled, window.__requests.find(request => request.method === 'PUT').body]), [1, true, { messages_considered_expired_after_seconds: 3600 }]);
      await page.evaluate(index => window.__answer(index, 400, { message: 'Not now' }), put);
      await page.waitForFunction(() => document.querySelector('#message-expiry-error').textContent === 'Not now');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#message-expiry-seconds').value, document.querySelector('#message-expiry-save').disabled]), ['3600', false], 'a failed save keeps what was typed');
      await page.$eval('#message-expiry-save', el => el.click());
      const again = await page.evaluate(() => window.__requests.findLastIndex(request => request.method === 'PUT'));
      await page.evaluate(index => window.__answer(index, 200, { messages_considered_expired_after_seconds: 3600, can_edit: true }), again);
      await page.waitForFunction(() => document.querySelector('#message-retention [role=status]').textContent === 'Saved.');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#message-expiry-seconds').value, document.querySelector('#message-expiry-error').textContent]), ['3600', '']);
      await page.close();
      const viewer = await mount({}, { auto: { 'GET /api/retention': { messages_considered_expired_after_seconds: 600, can_edit: false } } });
      await viewer.evaluate(() => window.__general.open());
      await viewer.waitForFunction(() => document.querySelector('#message-expiry-seconds').value === '600');
      assert.deepEqual(await viewer.evaluate(() => [document.querySelector('#message-expiry-seconds').disabled, document.querySelector('#message-expiry-save').disabled, document.querySelector('#message-retention [role=status]').textContent]), [true, true, 'Only the owner or an admin can change this.']);
      await viewer.close();
      const gone = await mount({}, { auto: { 'GET /api/retention': read } });
      await gone.evaluate(() => window.__general.open());
      await gone.waitForFunction(() => document.querySelector('#message-expiry-seconds').value === '21600');
      await gone.evaluate(() => { delete window.__auto['GET /api/retention']; document.querySelector('#message-expiry-save').click(); });
      const held = await gone.evaluate(() => window.__requests.findIndex(request => request.method === 'PUT'));
      await gone.evaluate(() => window.__general.destroy());
      await gone.evaluate(index => window.__answer(index, 200, { messages_considered_expired_after_seconds: 21600, can_edit: true }), held);
      await settle(gone);
      assert.equal(await gone.evaluate(() => document.querySelector('#settings-general').childElementCount), 0, 'a save answered after destroy draws nothing');
      await gone.close();
    }
    // Only run agents in docker: read when General is shown; an admin changes it with one click, saved at once; a refused save puts the switch back and says
    // why; a person who may not edit sees it disabled and told so.
    {
      const page = await mount({}, { auto: { 'GET /api/agent-isolation': { docker_only: false, can_edit: true } } });
      await page.evaluate(() => window.__general.open());
      await page.waitForFunction(() => !document.querySelector('#agent-docker-only').disabled);
      assert.equal(await page.$eval('#agent-docker-only', el => el.checked), false, 'off by default');
      await page.$eval('#agent-docker-only', el => el.click());
      await page.waitForFunction(() => window.__requests.some(request => request.method === 'PUT' && request.path === '/api/agent-isolation' && !request.answered));
      const put = await page.evaluate(() => window.__requests.findIndex(request => request.method === 'PUT' && request.path === '/api/agent-isolation'));
      assert.deepEqual(await page.evaluate(index => [window.__requests[index].body, document.querySelector('#agent-docker-only').disabled], put), [{ docker_only: true }, true], 'one request, and the switch waits for it');
      await page.evaluate(index => window.__answer(index, 400, { message: 'Not now' }), put);
      await page.waitForFunction(() => document.querySelector('#agent-docker-only-error').textContent === 'Not now');
      assert.equal(await page.$eval('#agent-docker-only', el => el.checked), false, 'a refused save puts the switch back');
      await page.$eval('#agent-docker-only', el => el.click());
      const again = await page.evaluate(() => window.__requests.findLastIndex(request => request.method === 'PUT' && request.path === '/api/agent-isolation'));
      await page.evaluate(index => window.__answer(index, 200, { docker_only: true, can_edit: true }), again);
      await page.waitForFunction(() => document.querySelector('#agent-docker-only').checked && !document.querySelector('#agent-docker-only').disabled);
      assert.equal(await page.$eval('#agent-docker-only-error', el => el.textContent), '');
      await page.close();
      const viewer = await mount({}, { auto: { 'GET /api/agent-isolation': { docker_only: true, can_edit: false } } });
      await viewer.evaluate(() => window.__general.open());
      await viewer.waitForFunction(() => document.querySelector('#agent-docker-only').checked);
      assert.deepEqual(await viewer.evaluate(() => [document.querySelector('#agent-docker-only').disabled, document.querySelector('#agent-isolation [role=status]').textContent]), [true, 'Only the owner or an admin can change this.']);
      await viewer.close();
    }
    // Settings backup: who sees it, export, and the two-step import with the order of answers chosen.
    {
      const sections = [{ section: 'macros', new: 1, overwrite: 0, unchanged: 0 }, { section: 'roles', new: 0, overwrite: 0, unchanged: 2 }, { section: 'ui_prefs', new: 0, overwrite: 1, unchanged: 0 }];
      const preview = { digest: 'd1', sections, ignored: ['spawn_models', 'mystery'] };
      const file = (name, body) => { const target = path.join(os.tmpdir(), `${process.pid}-${name}.json`); fs.writeFileSync(target, JSON.stringify(body)); return target; };
      const visible = page => page.evaluate(() => [!document.querySelector('#settings-backup').hidden, !document.querySelector('#settings-backup-file').closest('.settings-row').hidden, document.querySelector('#settings-backup-export-desc').textContent.includes('quota switcher')]);
      for (const [rights, expected] of [[{ owner: true, admin: true }, [true, true, true]], [{ owner: false, admin: true }, [true, false, false]], [{ owner: false, admin: false }, [false, false, false]]]) {
        const page = await mount({}, { rights });
        assert.deepEqual(await visible(page), expected, JSON.stringify(rights));
        await page.close();
      }
      const page = await mount({}, { auto: { 'GET /api/settings-backup': { schema: 'aimebu-settings-backup', version: 1 } } });
      await page.$eval('#settings-backup-export', el => el.click());
      await page.waitForFunction(() => document.querySelector('#settings-backup-status').textContent === 'Settings exported.');
      // Two files chosen in a row: only the second one's preview is drawn, whichever answers first.
      const first = file('one', { schema: 'x', macros: { a: '1' } }), second = file('two', { schema: 'x', macros: { b: '2' } });
      await (await page.$('#settings-backup-file')).uploadFile(first);
      await page.waitForFunction(() => window.__requests.some(request => request.path === '/api/settings-backup/preview'));
      await (await page.$('#settings-backup-file')).uploadFile(second);
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/settings-backup/preview').length === 2);
      await page.evaluate((preview, other) => { const ids = window.__requests.flatMap((request, index) => request.path === '/api/settings-backup/preview' ? [index] : []); window.__answer(ids[1], 200, preview); window.__answer(ids[0], 200, other); }, preview, { ...preview, digest: 'older', sections: [] });
      await page.waitForFunction(() => !document.querySelector('#settings-backup-plan').hidden);
      await settle(page);
      assert.deepEqual(await page.evaluate(() => ({ boxes: [...document.querySelectorAll('#settings-backup-plan .backup-section input')].map(box => `${box.value}:${box.checked}:${box.disabled}`), text: [...document.querySelectorAll('#settings-backup-plan .backup-section span')].map(span => span.textContent),
        notes: [...document.querySelectorAll('#settings-backup-plan > p')].map(p => p.textContent), apply: document.querySelector('#settings-backup-apply').disabled })),
        { boxes: ['macros:true:false', 'roles:false:true', 'ui_prefs:true:false'], text: ['Macros: 1 new', 'Roles: 2 already the same', 'Interface preferences: 1 to overwrite'], notes: ['Model lists are per harness now (Settings → Harnesses), so the model catalog in this file is not imported.', 'Not understood by this version and ignored: mystery.'], apply: false });
      // Untick everything: Apply says so and sends nothing. Then tick macros and apply; the answer is shown and the page is told.
      await page.$$eval('#settings-backup-plan .backup-section input:not(:disabled)', boxes => boxes.forEach(box => box.click()));
      await page.$eval('#settings-backup-apply', el => el.click());
      assert.equal(await page.$eval('#settings-backup-error', el => el.textContent), 'Tick at least one section.');
      assert.equal(await page.evaluate(() => window.__requests.filter(request => request.path === '/api/settings-backup/apply').length), 0);
      await page.evaluate(() => { window.__imported = 0; document.addEventListener('aimebu:settings-imported', () => { window.__imported++; }); });
      await page.$eval('#settings-backup-plan input[value=macros]', el => el.click());
      await page.$eval('#settings-backup-apply', el => el.click());
      await page.waitForFunction(() => window.__requests.some(request => request.path === '/api/settings-backup/apply'));
      assert.deepEqual(await page.evaluate(() => [window.__requests.find(request => request.path === '/api/settings-backup/apply').body.sections, window.__requests.find(request => request.path === '/api/settings-backup/apply').body.digest, document.querySelector('#settings-backup-apply').disabled]), [['macros'], 'd1', true], 'Apply sends the ticked sections once and waits');
      const apply = await page.evaluate(() => window.__requests.findIndex(request => request.path === '/api/settings-backup/apply'));
      await page.evaluate(index => window.__answer(index, 409, { error: 'preview_stale', message: 'stale' }), apply);
      await page.waitForFunction(() => document.querySelector('#settings-backup-error').textContent.startsWith('The settings changed since the preview'));
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#settings-backup-plan').hidden, document.querySelector('#settings-backup-file').value]), [true, ''], 'a stale preview is dropped and the file cleared');
      await page.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('general page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

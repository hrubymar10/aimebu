// Settings -> Usages (the usages view and the account switcher) in Chromium. The source is the app's own mock of the usages API, with
// every method that reads or writes recorded and, where a test asks, held until the test lets it go, so the order of answers is chosen.
// web/ is served as is with its import map; no Go server. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const importMap = /<script type="importmap">.*?<\/script>/s.exec(fs.readFileSync(path.join(root, 'index.html'), 'utf8'))[0];
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css"><link rel="stylesheet" href="/v2-only.css">${importMap}</head>
<body><dialog id="settings-dialog" class="settings-modal" aria-label="Settings" open><div class="settings-pane"><nav class="settings-nav" aria-label="Settings sections"></nav><div class="settings-body"><header class="settings-body-header"></header><section id="settings-usages" class="settings-section active"></section></div></div></dialog></body></html>`;
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
  // `held` lists the source methods to hold: a held call waits for window.__release(name) (the oldest first).
  const mount = async ({ held = [], owner = true, load = true, holdQuota = false } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async (held, owner, load, holdQuota) => {
      const { MockUsagesSource } = await import('/js/usages-source.js');
      const { UsagesSettings } = await import('/js/pages/usages/page.js');
      const source = new MockUsagesSource();
      window.__source = source; window.__calls = []; window.__waiting = {}; window.__synced = 0;
      for (const name of ['list', 'switcher', 'settings', 'refresh', 'provider', 'use', 'add', 'import', 'rename', 'remove', 'enable', 'mistral', 'ollama', 'copilotStart', 'copilotPoll', 'copilotLogout']) {
        if (!source[name]) continue; // the mock leaves out the writes a test does not need
        const original = source[name].bind(source);
        source[name] = (...args) => {
          window.__calls.push([name, ...args]);
          if (!held.includes(name)) return original(...args);
          return new Promise((resolve, reject) => { (window.__waiting[name] ||= []).push(() => original(...args).then(resolve, reject)); });
        };
      }
      window.__release = name => { (window.__waiting[name] || []).shift()?.(); };
      if (!owner) { const switcher = source.switcher.bind(source); source.switcher = async () => ({ ...(await switcher()), owner: false }); }
      const section = document.querySelector('#settings-usages'), dialog = document.querySelector('#settings-dialog');
      const absent = { load: async () => { const error = new Error('not served'); error.status = 404; window.__synced++; throw error; } };
      window.__reads = { quota: 0, guard: 0, fetch: 0 };
      const realFetch = window.fetch.bind(window);
      window.fetch = (...args) => { window.__reads.fetch++; return realFetch(...args); };
      const quotaSource = !holdQuota ? absent : { load: () => { window.__reads.quota++; return new Promise(resolve => { window.__quotaRelease = () => resolve({ tools: {} }); }); } };
      const guardSource = { load: async () => { window.__reads.guard++; return absent.load(); } };
      window.__usages = new UsagesSettings({ source, quotaSource, guardSource, dialog, section });
      window.__usages.setOwner(owner); // the owner has the switcher and its blocks; an admin reads the limits alone
      if (load) await window.__usages.load();
    }, held, owner, load, holdQuota);
    return page;
  };
  const calls = (page, name) => page.evaluate(name => window.__calls.filter(call => call[0] === name).length, name);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
  const click = (page, selector) => page.$eval(selector, el => el.click());
  const dialog = page => page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
  // Opens a tile's management panel (the gear), unless it is open already.
  const manage = (page, key) => page.evaluate(selector => { const tile = document.querySelector(selector); if (!tile.querySelector('.usages-b-manage-panel')) tile.querySelector('.usages-b-manage').click(); }, cardOf(key));
  const cardOf = key => `#usages-list .usages-b-tile[data-provider="${key}"]`;
  try {
    // The owner's page: the switcher and its forms above, the providers in order below, each with its profiles and their limit windows.
    {
      const page = await mount();
      assert.deepEqual(await page.evaluate(() => ({
        controls: [...document.querySelector('#usages-switcher').children].map(node => node.className || node.tagName.toLowerCase()),
        toggle: document.querySelector('#usage-switcher-disable').textContent,
        providers: [...document.querySelectorAll('#usages-list .usages-b-tile')].map(card => [card.dataset.provider, card.querySelector('.usages-provider-name').textContent, card.querySelector('.usages-provider-updated').textContent.replace(/^Updated .*/, 'Updated'), card.querySelectorAll('.usages-b-profile').length]),
        settings: ['#usage-refresh-interval', '#usage-percent-display', '#usage-alert-warning', '#usage-alert-critical'].map(selector => document.querySelector(selector).value),
        synced: window.__synced })),
      { controls: ['usage-config-controls', 'settings-actions usage-switcher-danger'], toggle: 'Disable account switcher', providers: [['codex', 'Codex', 'Updated', 2], ['claude-code', 'Claude Code', 'ok', 1], ['github-copilot', 'GitHub Copilot', 'Not configured', 0], ['mistral', 'Mistral', 'Not configured', 0], ['ollama-cloud', 'Ollama Cloud', 'Not configured', 0]],
        settings: ['120', 'left', '20', '10'], synced: 2 });
      assert.deepEqual(await page.evaluate(() => {
        const card = document.querySelector('#usages-list [data-provider=codex] .usages-b-profile');
        const window = card.querySelector('.usages-window-row');
        return { name: card.querySelector('.switcher-tile-name').textContent, active: !!card.querySelector('.switcher-tile-active'), windows: card.querySelectorAll('.usages-window-row').length, label: window.querySelector('.usages-progress').getAttribute('aria-label'), now: window.querySelector('.usages-progress').getAttribute('aria-valuenow'),
          marker: !!window.querySelector('.usages-pace-tick'), spark: !!window.querySelector('svg.usages-b-spark polyline'), pace: window.querySelector('.usages-pace-text').textContent, credits: card.querySelector('.usages-credits-row').textContent, switchHidden: !card.querySelector('.usages-b-switch') };
      }), { name: 'main', active: true, windows: 3, label: 'Session left', now: '58', marker: true, spark: true, pace: '13% in reserve·Lasts until reset', credits: 'Credits: 12.50', switchHidden: true });
      await click(page, `${cardOf('codex')} .usages-b-manage`);
      assert.equal(await page.$eval(`${cardOf('codex')} .usage-affected`, el => el.textContent), 'Switch affects: milo@mock', 'the gear opens the tile\'s management panel');
      assert.ok(/expected pace/.test(await page.$eval('#usages-list .usages-legend', el => el.textContent)) && /In reserve/.test(await page.$eval('#usages-list .usages-legend', el => el.textContent)), 'the marks on a card are explained');
      await page.close();
    }
    // An admin who is not the owner reads the limits: the cards, with no controls and no switcher, the quota and limit-guard blocks gone, and a line that says
    // only the owner changes Usages. The switcher is never asked for (it is the owner's, and a 403 would be shown as an error).
    {
      const page = await mount({ owner: false });
      assert.deepEqual(await page.evaluate(() => ({
        note: document.querySelector('#usages-readonly')?.textContent, switcherBox: document.querySelector('#usages-switcher'), quota: !!document.querySelector('.quota-settings'), guard: !!document.querySelector('.limit-guard-settings'),
        cards: document.querySelectorAll('#usages-list .usages-b-tile').length > 0, actions: document.querySelectorAll('#usages-list .usages-b-tile-actions button').length, rename: document.querySelectorAll('#usages-list .usages-b-rename').length,
        remove: document.querySelectorAll('#usages-list .usages-b-remove').length, switchButtons: document.querySelectorAll('#usages-list .usages-b-switch').length, asked: window.__calls.filter(call => call[0] === 'switcher').length, error: document.querySelector('#usages-error').textContent })),
        { note: 'Only the owner can change Usages. You can see the limits here.', switcherBox: null, quota: false, guard: false, cards: true, actions: 0, rename: 0, remove: 0, switchButtons: 0, asked: 0, error: '' });
      await page.close();
      const owner = await mount();
      assert.deepEqual(await owner.evaluate(() => [!!document.querySelector('#usages-readonly'), !!document.querySelector('#usages-switcher')]), [false, true], 'the owner has no read-only note, and has the switcher');
      await owner.close();
    }
    // Switching asks first and names who is affected; the switch, then a read, follows. A failure says why.
    {
      const page = await mount();
      // Switch is a framed rectangular button, not the rounded status pill that ACTIVE is, and a bar track has no border, as in v1.
      assert.deepEqual(await page.evaluate(card => { const button = document.querySelector(`${card} .usages-b-profile[data-profile=backup] .usages-b-switch`), style = getComputedStyle(button), active = document.querySelector(`${card} .switcher-tile-active, ${card} .switcher-tile-active`), track = getComputedStyle(document.querySelector('.usages-progress'));
        return { radius: style.borderTopLeftRadius, border: style.borderTopWidth, upper: style.textTransform, activeUpper: active ? getComputedStyle(active).textTransform : 'none', track: track.borderTopWidth }; }, cardOf('codex')),
        { radius: '6px', border: '1px', upper: 'none', activeUpper: 'uppercase', track: '0px' }, 'Switch is a real button, the status pill is a different shape, and the track has no border (v1)');
      assert.deepEqual(await page.evaluate(card => { const [first, second] = [...document.querySelectorAll(`${card} .usages-b-profile`)]; const say = node => { const label = node.querySelector('.switcher-tile-active, .usages-b-inactive'); return label?.textContent; }; return [say(first), say(second)]; }, cardOf('codex')),
        ['Active', 'Inactive'], 'an account says in words whether it is the active one');
      await click(page, `${cardOf('codex')} .usages-b-profile[data-profile=backup] .usages-b-switch`);
      await dialog(page);
      assert.match(await page.$eval('#app-dialog .app-dialog-message', el => el.textContent), /Affected agents: milo@mock\. Switch to backup\?/);
      await click(page, '#app-dialog .app-dialog-cancel');
      await settle(page);
      assert.equal(await calls(page, 'use'), 0, 'declining switches nothing');
      const reads = await calls(page, 'list');
      await click(page, `${cardOf('codex')} .usages-b-profile[data-profile=backup] .usages-b-switch`);
      await dialog(page);
      await click(page, '#app-dialog .app-dialog-ok');
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'use'));
      await page.waitForFunction(reads => window.__calls.filter(call => call[0] === 'list').length > reads, {}, reads);
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'use')), ['use', 'codex', 'backup']);
      await page.evaluate(() => { window.__source.use = async () => { throw new Error('The switch was refused'); }; });
      await manage(page, 'claude-code');
      await click(page, `${cardOf('claude-code')} .usages-b-rename`);
      await dialog(page);
      await page.$eval('#app-dialog input', el => { el.value = 'renamed'; el.dispatchEvent(new Event('input', { bubbles: true })); });
      await click(page, '#app-dialog .app-dialog-ok');
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'rename'));
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'rename')), ['rename', 'claude', 'local', 'renamed']);
      await page.evaluate(() => { window.__source.enable = async () => { throw new Error('Cannot switch now'); }; });
      await click(page, '#usage-switcher-disable'); await dialog(page); await click(page, '#app-dialog .app-dialog-ok');
      await page.waitForFunction(() => document.querySelector('#usages-error').textContent === 'Cannot switch now');
      await page.close();
    }
    // A stored account needing reauthentication offers an explicit install path.
    {
      const page = await mount({ load: false });
      await page.evaluate(async () => {
        const list = window.__source.list.bind(window.__source);
        window.__source.list = async () => ({ ...(await list()), credential_alerts: [{ kind: 'reauth_required', tool: 'codex', profile: 'backup', role: 'store', message: 'Sign in again' }] });
        await window.__usages.load();
      });
      const selector = `${cardOf('codex')} .usages-b-profile[data-profile=backup] .usages-b-install-anyway`;
      assert.match(await page.$eval(selector, el => el.title), /^Install anyway/);
      await click(page, selector);
      await dialog(page);
      assert.match(await page.$eval('#app-dialog .app-dialog-message', el => el.textContent), /without refreshing/);
      await click(page, '#app-dialog .app-dialog-ok');
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'use'));
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'use')), ['use', 'codex', 'backup', true]);
      await page.close();
    }
    // Settings refetches both the usage cards and switcher list after its own
    // removal, and after the live event for a CLI removal in another process.
    for (const external of [false, true]) {
      const page = await mount();
      await page.evaluate(() => {
        let removed = false;
        const list = window.__source.list.bind(window.__source);
        const switcher = window.__source.switcher.bind(window.__source);
        window.__source.list = async () => {
          const out = await list();
          if (removed) out.providers.find(item => item.provider_name === 'codex').profiles = out.providers.find(item => item.provider_name === 'codex').profiles.filter(item => item.profile_name !== 'backup');
          return out;
        };
        window.__source.switcher = async () => {
          const out = await switcher();
          if (removed) out.profiles = out.profiles.filter(item => item.tool !== 'codex' || item.name !== 'backup');
          return out;
        };
        window.__source.remove = async (tool, name) => { window.__calls.push(['remove', tool, name]); removed = true; };
        window.__removeOutsidePage = () => { removed = true; window.__usages.changed(); };
      });
      if (external) await page.evaluate(() => window.__removeOutsidePage());
      else {
        await manage(page, 'codex');
        await click(page, `${cardOf('codex')} .usages-b-profile[data-profile=backup] .usages-b-remove`);
        await dialog(page);
        await click(page, '#app-dialog .app-dialog-ok');
      }
      await page.waitForFunction(() => ![...document.querySelectorAll('[data-provider="codex"] .switcher-tile-name')].some(node => node.textContent === 'backup'));
      assert.equal(await page.evaluate(() => window.__usages.store.switcher.value.profiles.some(item => item.tool === 'codex' && item.name === 'backup')), false);
      if (!external) assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'remove')), ['remove', 'codex', 'backup']);
      await page.close();
    }

    // Two reads answered out of order: the later request's answer is the one drawn.
    {
      const page = await mount({ held: ['list'] , load: false });
      await page.evaluate(() => { window.__usages.load(); window.__usages.load(); });
      await page.waitForFunction(() => window.__waiting.list?.length === 2);
      await page.evaluate(() => { window.__source.preferences.percent_display = 'used'; window.__release('list'); }); // the first read
      await settle(page);
      assert.equal(await page.evaluate(() => document.querySelectorAll('#usages-list .usages-b-tile').length), 0, 'a read superseded by a later one draws nothing, even answered first');
      await page.evaluate(() => window.__release('list'));
      await page.waitForFunction(() => document.querySelectorAll('#usages-list .usages-b-tile').length === 5);
      await page.close();
    }
    // A push from the event stream: applied at once, held while a field has focus (and applied when focus leaves), and held while a read runs.
    {
      const page = await mount();
      const reads = () => calls(page, 'list');
      let before = await reads();
      await page.evaluate(() => { window.__section = document.querySelector('#settings-usages'); window.__usages.store.isShown = () => true; window.__usages.changed(); });
      await page.waitForFunction(before => window.__calls.filter(call => call[0] === 'list').length > before, {}, before);
      before = await reads();
      await page.focus('#usage-refresh-interval');
      await page.evaluate(() => window.__usages.changed());
      await settle(page);
      assert.equal(await reads(), before, 'a push while a field has focus waits');
      await page.evaluate(() => document.activeElement.blur());
      await page.waitForFunction(before => window.__calls.filter(call => call[0] === 'list').length > before, {}, before);
      await page.close();
      const slow = await mount({ held: ['list'], load: false });
      await slow.evaluate(() => { window.__usages.store.isShown = () => true; window.__usages.changed(); });
      await slow.waitForFunction(() => window.__waiting.list?.length === 1);
      await slow.evaluate(() => { window.__usages.changed(); window.__usages.changed(); });
      await settle(slow);
      assert.equal(await calls(slow, 'list'), 1, 'pushes during a read are not read again one by one');
      await slow.evaluate(() => window.__release('list'));
      await slow.waitForFunction(() => window.__calls.filter(call => call[0] === 'list').length === 2);
      await slow.close();
      const hidden = await mount();
      await hidden.evaluate(() => { window.__usages.store.isShown = () => false; });
      const seen = await calls(hidden, 'list');
      await hidden.evaluate(() => window.__usages.changed());
      await settle(hidden);
      assert.equal(await calls(hidden, 'list'), seen, 'a push while the section is not shown reads nothing');
      await hidden.close();
    }
    // The settings form saves what is typed; a refresh answered "too soon" says when to try again.
    {
      const page = await mount();
      await page.$eval('#usage-refresh-interval', el => { el.value = '90'; });
      await page.select('#usage-percent-display', 'used');
      await click(page, '.usage-config-controls .settings-actions .btn-primary');
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'settings'));
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'settings')[1]), { refresh_interval_sec: 90, percent_display: 'used', alert_warning_remaining: 20, alert_critical_remaining: 10 });
      await page.waitForFunction(() => document.querySelector('#usage-percent-display').value === 'used');
      await page.evaluate(() => { window.__source.refresh = async () => { const error = new Error('x'); error.status = 429; error.retryAfter = 30; throw error; }; });
      await click(page, '.usage-config-controls .settings-actions .btn:not(.btn-primary)');
      await page.waitForFunction(() => document.querySelector('#usages-error').textContent === 'Refreshed moments ago; try again in 30s');
      await page.close();
    }
    // Token auto-refresh and warm-up: the switch reads again until the server has converted the logins; warm-up shows its schedule only when scheduled.
    {
      const page = await mount();
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#usage-token-auto-refresh').checked, [...document.querySelectorAll('.usage-token-state')].map(node => node.textContent), document.querySelector('#usage-warmup-schedule')]), [false, ['Claude: off', 'Codex: off'], null]);
      await page.evaluate(() => {
        const settings = window.__source.settings.bind(window.__source);
        window.__source.settings = async body => { await settings(body); if ('token_auto_refresh' in body) window.__pending = 2; return window.__source.list(); };
        const list = window.__source.list.bind(window.__source);
        window.__source.list = async () => { const answer = await list(); if (window.__pending > 0 && answer.settings.token_auto_refresh) { window.__pending--; answer.settings.token_auto_refresh_state = { claude: { status: 'pending' }, codex: { status: 'pending' } }; } return answer; };
      });
      await page.$eval('#usage-token-auto-refresh', el => el.click());
      await page.waitForFunction(() => [...document.querySelectorAll('.usage-token-state')].map(node => node.textContent).join() === 'Claude: pending,Codex: pending');
      await page.waitForFunction(() => [...document.querySelectorAll('.usage-token-state')].map(node => node.textContent).join() === 'Claude: migrated,Codex: waiting: codex is running', { timeout: 20000 }).catch(async () => { throw new Error(`not settled: ${await page.$$eval('.usage-token-state', nodes => nodes.map(node => node.textContent))}`); });
      await page.select('#usage-warmup-mode', 'scheduled');
      await page.waitForFunction(() => document.querySelector('#usage-warmup-schedule'));
      assert.deepEqual(await page.evaluate(() => window.__calls.filter(call => call[0] === 'settings').at(-1)[1]), { warmup_mode: 'scheduled', warmup_schedule: '0 9 * * *' });
      await page.close();
    }
    // GitHub sign-in: the code and link show while it waits, a pending answer polls again, success clears it, and destroy stops the polling.
    {
      const page = await mount();
      await page.evaluate(() => {
        window.__polls = 0;
        window.__source.copilotPoll = async () => { window.__polls++; return window.__polls < 2 ? { status: 'pending', interval: 1 } : { status: 'success' }; };
      });
      await manage(page, 'github-copilot');
      await page.$eval(`${cardOf('github-copilot')} input[type=url]`, node => { node.value = 'https://github.example.test'; node.dispatchEvent(new Event('input', { bubbles: true })); });
      await click(page, `${cardOf('github-copilot')} .btn-primary`);
      await page.waitForFunction(() => document.querySelector('.usage-copilot-flow span'));
      assert.deepEqual(await page.evaluate(() => [window.__usages.store.drafts.copilotHost, document.querySelector('[data-provider=github-copilot] input[type=url]').value]),
        ['https://github.example.test', 'https://github.example.test'], 'sign-in keeps the host draft while polling');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('.usage-copilot-flow span').textContent, document.querySelector('.usage-copilot-flow a').href, document.querySelector('.usage-copilot-flow a').rel]), ['Code: ABCD-1234 · pending', 'https://github.com/login/device', 'noopener noreferrer']);
      await page.waitForFunction(() => !document.querySelector('.usage-copilot-flow span'), { timeout: 8000 });
      assert.equal(await page.evaluate(() => window.__polls), 2);
      assert.equal(await page.evaluate(() => window.__usages.store.drafts.copilotHost), undefined, 'success clears the host draft');
      await page.evaluate(() => { window.__polls = 0; window.__source.copilotPoll = async () => { window.__polls++; return { status: 'pending', interval: 1 }; }; });
      await manage(page, 'github-copilot');
      await click(page, `${cardOf('github-copilot')} .btn-primary`);
      await page.waitForFunction(() => window.__polls === 1);
      await page.evaluate(() => window.__usages.destroy());
      await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 1500)));
      assert.equal(await page.evaluate(() => window.__polls), 1, 'a destroyed page stops polling');
      await page.close();
    }
    // Signing out clears a saved Copilot host draft after the request succeeds.
    {
      const page = await mount();
      await page.evaluate(async () => { window.__source.flags['github-copilot'].token_configured = true; await window.__usages.load(); });
      await manage(page, 'github-copilot');
      await page.$eval(`${cardOf('github-copilot')} input[type=url]`, node => { node.value = 'https://github.example.test'; node.dispatchEvent(new Event('input', { bubbles: true })); });
      await click(page, `${cardOf('github-copilot')} .usage-provider-actions button:not(.btn-primary)`);
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'copilotLogout') && window.__usages.store.drafts.copilotHost === undefined);
      assert.equal(await page.evaluate(() => window.__usages.store.drafts.copilotHost), undefined, 'sign-out clears the host draft');
      await page.close();
    }
    // A verification address that is not https is shown as no link at all.
    for (const address of ['javascript:alert(1)', 'http://github.com/login/device', 'https://user:pw@github.com/login/device']) {
      const page = await mount();
      await page.evaluate(uri => {
        window.__source.copilotStart = async () => ({ flow_id: 'mock-flow', user_code: 'ABCD-1234', verification_uri: uri, interval: 60, expires_at: new Date(Date.now() + 600000).toISOString() });
        window.__source.copilotPoll = async () => ({ status: 'pending', interval: 60 });
      }, address);
      await manage(page, 'github-copilot');
      await click(page, `${cardOf('github-copilot')} .btn-primary`);
      await page.waitForFunction(() => document.querySelector('.usage-copilot-flow span'));
      assert.equal(await page.evaluate(() => document.querySelector('.usage-copilot-flow a')), null, `${address} became a link`);
      await page.close();
    }
    // GitHub sign-in started and the page destroyed before the start is answered: nothing is drawn and no poll is sent.
    {
      const page = await mount({ held: ['copilotStart'] });
      await manage(page, 'github-copilot');
      await click(page, `${cardOf('github-copilot')} .btn-primary`);
      await page.waitForFunction(() => window.__waiting.copilotStart?.length === 1);
      await page.evaluate(() => window.__usages.destroy());
      const reads = await calls(page, 'list');
      await page.evaluate(() => window.__release('copilotStart'));
      await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 300)));
      assert.deepEqual(await page.evaluate(() => [window.__calls.filter(call => call[0] === 'copilotPoll').length, document.querySelector('.usage-copilot-flow')?.children.length ?? 0]), [0, 0], 'a start answered after destroy sends no poll and draws no code');
      assert.equal(await calls(page, 'list'), reads, 'and reads nothing');
      await page.close();
    }
    // Any write answered after the page is destroyed is not followed by a read (the page's own reads are counted: the mock's own answer
    // to a save is a read of its own), and the token auto-refresh switch sends nothing more.
    {
      const counted = page => page.evaluate(() => { const store = window.__usages.store, load = store.load.bind(store); window.__loads = 0; store.load = () => { window.__loads++; return load(); }; });
      const page = await mount({ held: ['settings'] });
      await counted(page);
      await click(page, '.usage-config-controls .settings-actions .btn-primary');
      await page.waitForFunction(() => window.__waiting.settings?.length === 1);
      await page.evaluate(() => { window.__usages.destroy(); window.__release('settings'); });
      await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 300)));
      assert.equal(await page.evaluate(() => window.__loads), 0, 'a save answered after destroy reads nothing');
      await page.close();
      const token = await mount({ held: ['settings'] });
      await counted(token);
      await token.$eval('#usage-token-auto-refresh', el => el.click());
      await token.waitForFunction(() => window.__waiting.settings?.length === 1);
      await token.evaluate(() => { window.__usages.destroy(); window.__release('settings'); });
      await token.evaluate(() => new Promise(resolve => setTimeout(resolve, 300)));
      assert.equal(await token.evaluate(() => window.__loads), 0, 'a token auto-refresh save answered after destroy reads nothing');
      await token.close();
    }
    // Credential alerts: the owner sees the server's sentences (one line each, the kind only as a class), others see none.
    {
      const page = await mount();
      assert.equal(await page.evaluate(() => document.querySelectorAll('.usage-credential-alerts').length), 0, 'no alert while the server has none');
      await page.evaluate(async () => { window.__source.preferences.token_auto_refresh = true; await window.__usages.load(); });
      assert.deepEqual(await page.evaluate(() => { const box = document.querySelector('.usage-credential-alerts'); return [box.getAttribute('role'), [...box.children].map(p => [p.className, p.textContent])]; }),
        ['alert', [['usage-credential-alert usage-credential-alert-expiring', 'claude live: the access token expires in 4 min.']]]);
      // Refresher conflicts of several tools are one alert in plain words with the paths folded under Details, in a neutral style (an error is red; this is information).
      await page.evaluate(async () => {
        const list = window.__source.list.bind(window.__source);
        window.__source.list = async () => { const out = await list(); out.credential_alerts = [...(out.credential_alerts || []),
          { kind: 'refresher_conflict', message: 'claude: another token refresher may own the same live credentials: /h/.aimebu/switcher; this tool will not refresh' },
          { kind: 'refresher_conflict', message: 'codex: another token refresher may own the same live credentials: /h/.aimebu/switcher; this tool will not refresh' }]; return out; };
        await window.__usages.load();
      });
      assert.deepEqual(await page.evaluate(() => { const box = document.querySelector('.usage-credential-alerts'), conflict = box.querySelector('.usage-credential-alert-refresher_conflict');
        return { alerts: box.children.length, text: conflict.querySelector(':scope > p').textContent.slice(0, 60), details: conflict.querySelector('details > summary').textContent, lines: conflict.querySelectorAll('details p').length, pathInSentence: /\/h\/\.aimebu/.test(conflict.querySelector(':scope > p').textContent),
          neutral: getComputedStyle(conflict).color === getComputedStyle(document.querySelector('.usage-credential-alert-expiring')).color ? 'same as an error' : 'differs from an error', bar: getComputedStyle(conflict).borderLeftColor !== getComputedStyle(document.querySelector('.usage-credential-alert-expiring')).borderLeftColor }; }),
        { alerts: 2, text: 'Token refresh is paused for claude, codex: another aimebu (v', details: 'Details', lines: 2, pathInSentence: false, neutral: 'differs from an error', bar: true });
      await page.close();
      const other = await mount({ owner: false });
      await other.evaluate(async () => { window.__source.preferences.token_auto_refresh = true; await window.__usages.load(); });
      assert.equal(await other.evaluate(() => document.querySelectorAll('.usage-credential-alerts').length), 0);
      await other.close();
    }
    // The owner signs out and an admin signs in on the same tab: nothing of the owner's snapshot (e-mails, alerts, profiles, the switcher and its blocks) is left in the page,
    // even when the admin's own read fails with something other than a 403.
    {
      const page = await mount();
      await page.evaluate(async () => { window.__source.preferences.token_auto_refresh = true; await window.__usages.load(); });
      assert.deepEqual(await page.evaluate(() => ({ visible: document.querySelector('.usages-page').textContent.includes('main@example.test'), tooltip: document.querySelector('.usages-b-profile[data-profile=main] .switcher-tile-plan').title })),
        { visible: false, tooltip: 'Pro · main@example.test' }, 'the owner sees the e-mail only in the plan tooltip');
      await page.evaluate(() => { window.__source.list = async () => { throw new Error('boom'); }; window.__usages.setOwner(false); window.__usages.forget(); });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => { const text = document.querySelector('.usages-page').textContent; return { email: text.includes('main@example.test'), alerts: document.querySelectorAll('.usage-credential-alert').length, cards: document.querySelectorAll('#usages-list .usages-b-tile').length, switcher: !!document.querySelector('#usages-switcher'), plans: /Pro\b/.test(text) }; }),
        { email: false, alerts: 0, cards: 0, switcher: false, plans: false });
      await page.close();
    }
    // A read of the default daily limits that is still out when the page forgets its data (a sign-out) draws nothing when it answers, and nor does a save.
    {
      const page = await mount();
      const result = await page.evaluate(async () => {
        const { LimitDefaultStore } = await import('/js/pages/usages/limit-default.js');
        let release; const held = new Promise(resolve => { release = resolve; });
        const store = new LimitDefaultStore({ send: async (path, options) => { await held; return { daily_cost_cap: 5, daily_turn_cap: null, can_edit: true, ...(options?.body || {}) }; } });
        const read = store.sync(), write = store.save({ daily_cost_cap: 9, daily_turn_cap: 1 });
        store.forget();
        release(); await read; await write.catch(() => {});
        return [store.state.value, store.saved.value];
      });
      assert.deepEqual(result, [null, ''], 'an answer that arrives after the page forgot its data draws nothing');
      await page.close();
    }
    // Data older than twice the refresh interval is drawn grey and says how old it is, with a Refresh for the owner (an admin has none); fresh data does neither.
    {
      for (const owner of [true, false]) {
        const page = await mount({ owner });
        await page.evaluate(async () => {
          const list = window.__source.list.bind(window.__source);
          window.__source.list = async () => { const out = await list(); out.providers[0].profiles[0].last_refresh_at = new Date(Date.now() - 2 * 86400000).toISOString(); return out; };
          await window.__usages.load();
        });
        const view = await page.evaluate(() => ({ stale: document.querySelectorAll('.usages-b-profile.is-stale').length, fresh: document.querySelectorAll('.usages-b-profile:not(.is-stale)').length > 0, text: document.querySelector('.usages-stale-line')?.textContent.replace(/\s+/g, ' ').trim(),
          refresh: !!document.querySelector('.usages-stale-line .usages-b-refresh'), color: getComputedStyle(document.querySelector('.usages-b-profile.is-stale .usages-progress span')).backgroundColor }));
        assert.deepEqual([view.stale, view.fresh, view.text, view.refresh], [1, true, owner ? 'Stale, last update 2d ago' : 'Stale, last update 2d ago', owner], `${owner ? 'owner' : 'admin'}: ${JSON.stringify(view)}`);
        const look = await page.evaluate(() => {
          const color = value => { const probe = document.createElement('i'); probe.style.color = value; document.body.append(probe); const out = getComputedStyle(probe).color; probe.remove(); return out; };
          const card = document.querySelector('.usages-b-profile.is-stale'), fresh = document.querySelector('.usages-b-profile:not(.is-stale)');
          return { fill: getComputedStyle(card.querySelector('.usages-progress span')).backgroundColor, muted: color(getComputedStyle(document.documentElement).getPropertyValue('--text-muted').trim()), freshFill: getComputedStyle(fresh.querySelector('.usages-progress span')).backgroundColor,
            pace: card.querySelectorAll('.usages-pace-text, .usages-pace-tick').length, reset: [...card.querySelectorAll('.usages-window-meta span')].map(node => node.textContent) };
        });
        assert.equal(look.fill, look.muted, `a stale card's bars are the muted grey: ${JSON.stringify(look)}`);
        assert.notEqual(look.freshFill, look.muted, 'a fresh card keeps its colour');
        assert.equal(look.pace, 0, 'a stale card claims no pace');
        assert.ok(look.reset.every(text => /^(Reset passed|Resets .+)$/.test(text) && !/ in /.test(text)), `a stale card's reset is a fact, not a countdown: ${JSON.stringify(look.reset)}`);
        if (owner) {
          const before = await calls(page, 'refresh');
          await click(page, '.usages-stale-line .usages-b-refresh'); await settle(page);
          assert.equal(await calls(page, 'refresh'), before + 1, 'the stale card refreshes its provider');
        }
        await page.close();
      }
    }
    // A stale profile says why it is not refreshing: a backoff names the retry time (local), a failed fetch has the error in the tooltip, an admin has no error text.
    {
      const cases = [
        { name: 'backoff', patch: { status: 'backing_off', error_kind: 'rate_limited', retry_at: '2026-05-01T16:06:00Z', error: 'Usage endpoint failed; retrying after 16:06 UTC (Claude usage endpoint returned HTTP 429.)' }, owner: /^Stale, last update 2d ago · Rate limited by the provider, retrying at \d{1,2}:\d{2}/, admin: /^Stale, last update 2d ago · Rate limited by the provider, retrying at \d{1,2}:\d{2}/ },
        { name: 'http 429', patch: { status: 'fetch_error', error_kind: 'rate_limited', error: 'Claude usage endpoint returned HTTP 429.' }, owner: /· Rate limited by the provider$/, admin: /^Stale, last update 2d ago · Rate limited by the provider$/ },
        { name: 'fetch_error', patch: { status: 'fetch_error', error_kind: 'fetch_failed', error: 'connection reset' }, owner: /· Last fetch failed$/, admin: /· Last fetch failed$/ },
        { name: 'token_expired', patch: { status: 'token_expired', error_kind: 'token_expired' }, owner: /^Stale, last update 2d ago$/, admin: /^Stale, last update 2d ago$/ },
      ];
      for (const owner of [true, false]) for (const c of cases) {
        const page = await mount({ owner });
        await page.evaluate(async ({ patch, owner }) => {
          const list = window.__source.list.bind(window.__source);
          window.__source.list = async () => {
            const out = await list(); const profile = out.providers[0].profiles[0];
            profile.last_refresh_at = new Date(Date.now() - 2 * 86400000).toISOString(); Object.assign(profile, patch);
            if (!owner) delete profile.error;
            return out;
          };
          await window.__usages.load();
        }, { patch: c.patch, owner });
        const view = await page.evaluate(() => { const node = document.querySelector('.usages-stale-line'); return { text: node?.textContent.replace(/\s+/g, ' ').trim(), title: node?.title }; });
        assert.match(view.text, owner ? c.owner : c.admin, `${c.name} ${owner ? 'owner' : 'admin'}: ${JSON.stringify(view)}`);
        if (owner && c.name === 'fetch_error') assert.equal(view.title, 'connection reset', 'the error is the tooltip');
        if (!owner) assert.equal(view.title, '', `${c.name}: an admin has no error text`);
        await page.close();
      }
    }
    // Disabling the account switcher is a danger button at the bottom of the page behind a confirm, and Enable is the one primary button at the top of a disabled one.
    {
      const page = await mount();
      assert.deepEqual(await page.evaluate(() => ({ last: document.querySelector('#usages-switcher').lastElementChild.className, danger: document.querySelector('#usage-switcher-disable').classList.contains('btn-danger'),
        primaries: [...document.querySelectorAll('#usages-switcher .btn-primary')].map(node => node.textContent) })), { last: 'settings-actions usage-switcher-danger', danger: true, primaries: ['Save usage settings'] });
      await click(page, '#usage-switcher-disable');
      await dialog(page);
      await click(page, '#app-dialog .app-dialog-cancel'); await settle(page);
      assert.equal(await calls(page, 'enable'), 0, 'declining the confirm disables nothing');
      await click(page, '#usage-switcher-disable');
      await dialog(page);
      await click(page, '#app-dialog .app-dialog-ok'); await settle(page);
      assert.equal(await calls(page, 'enable'), 1, 'confirming disables it');
      assert.deepEqual(await page.evaluate(() => ({ enable: document.querySelector('#usage-switcher-enable')?.classList.contains('btn-primary'), disable: !!document.querySelector('#usage-switcher-disable') })), { enable: true, disable: false });
      await page.close();
    }
    // Mistral and Ollama credentials: what is typed is sent, and clearing appears once something is stored.
    {
      const page = await mount();
      await manage(page, 'mistral');
      await page.$eval(`${cardOf('mistral')} textarea`, el => { el.value = 'cookie=1'; });
      await click(page, `${cardOf('mistral')} .btn-primary`);
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'mistral'));
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'mistral')), ['mistral', 'cookie=1']);
      await page.waitForFunction(() => [...document.querySelectorAll('[data-provider=mistral] button')].some(button => button.textContent === 'Clear cookie'));
      await manage(page, 'ollama-cloud');
      await page.select(`${cardOf('ollama-cloud')} select`, 'api_key');
      // A refresh of the panel between choosing and saving (a live push, another provider's save) rebuilds the form, and the rebuilt form
      // keeps the mode the person chose instead of the stored one.
      const reads = await calls(page, 'list');
      await page.evaluate(() => window.__usages.load());
      await page.waitForFunction(reads => window.__calls.filter(call => call[0] === 'list').length > reads, {}, reads);
      assert.equal(await page.$eval(`${cardOf('ollama-cloud')} select`, el => el.value), 'api_key', 'a refresh put the stored mode back over the chosen one');
      await page.$eval(`${cardOf('ollama-cloud')} input[type=password]`, el => { el.value = 'sk-1'; });
      await click(page, `${cardOf('ollama-cloud')} .btn-primary`);
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'ollama'));
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'ollama')[1]), { auth_mode: 'api_key', api_key: 'sk-1' });
      await page.close();
    }
    // Moving a provider in the order redraws the list in place: the moved card stays where it was pressed (the section scrolls only by the
    // move, never to the top), the card stays on screen, and the pressed button keeps working, so the order can be walked click by click.
    {
      const page = await mount();
      const under = (direction, offset) => page.evaluate((direction, offset) => {
        const section = document.querySelector('#settings-usages'), card = section.querySelector('[data-provider="claude-code"]');
        section.scrollTop += card.getBoundingClientRect().top - section.getBoundingClientRect().top - offset;
        const button = [...card.querySelectorAll('.usages-b-manage-row button')].find(node => (node.getAttribute('aria-label') || '').endsWith(direction));
        const box = button.getBoundingClientRect();
        return { x: box.x + box.width / 2, y: box.y + box.height / 2 };
      }, direction, offset);
      const drawn = () => page.evaluate(() => {
        const section = document.querySelector('#settings-usages'), card = section.querySelector('[data-provider="claude-code"]');
        const box = card.getBoundingClientRect(), frame = section.getBoundingClientRect();
        return { order: [...section.querySelectorAll('.usages-b-tile')].map(item => item.dataset.provider), top: Math.round(box.top - frame.top), scrollTop: section.scrollTop, inView: box.top >= frame.top && box.bottom <= frame.bottom, focus: document.activeElement?.getAttribute('aria-label') || document.activeElement?.tagName };
      });
      await manage(page, 'claude-code');
      const at = await under('up', 40);
      const before = await drawn();
      await page.mouse.click(at.x, at.y);
      await page.waitForFunction(() => document.querySelector('#usages-list .usages-b-tile')?.dataset.provider === 'claude-code');
      await settle(page);
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'settings')), ['settings', { provider_order: ['claude-code', 'codex', 'github-copilot', 'mistral', 'ollama-cloud'] }]);
      const after = await drawn();
      assert.deepEqual(after.order, ['claude-code', 'codex', 'github-copilot', 'mistral', 'ollama-cloud']);
      assert.equal(after.inView, true, 'the moved card is still on screen ' + JSON.stringify([before, after]));
      assert.ok(Math.abs(after.top - before.top) <= 2, `the moved card stays where it was pressed, the scroll never jumps to the top (top ${before.top} -> ${after.top}, scrollTop ${before.scrollTop} -> ${after.scrollTop})`);
      // Moving it back: the same gesture keeps working, and focus stays on the pressed button.
      const back = await under('down', 40);
      await page.mouse.click(back.x, back.y);
      await page.waitForFunction(() => document.querySelector('#usages-list .usages-b-tile')?.dataset.provider === 'codex');
      await settle(page);
      const restored = await drawn();
      assert.deepEqual(restored.order, ['codex', 'claude-code', 'github-copilot', 'mistral', 'ollama-cloud']);
      assert.equal(restored.focus, 'Move Claude Code down', 'focus stays on the pressed button, so repeated clicks keep working');
      await page.close();
    }
    // An answer arriving after the page is destroyed draws nothing and starts nothing.
    {
      const page = await mount({ held: ['list'], load: false });
      await page.evaluate(() => { window.__usages.load(); });
      await page.waitForFunction(() => window.__waiting.list?.length === 1);
      await page.evaluate(() => window.__usages.destroy());
      await page.evaluate(() => window.__release('list'));
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelectorAll('#usages-list *').length, window.__synced]), [0, 0]);
      await page.close();
    }
    // Destroyed while the automatic-switching read is held: releasing it starts no limit-guard read, and the default limits are not read.
    {
      const page = await mount({ load: false, holdQuota: true });
      await page.evaluate(() => { window.__usages.load(); });
      await page.waitForFunction(() => window.__reads.quota === 1);
      await page.evaluate(() => window.__usages.destroy());
      await page.evaluate(() => { window.__quotaRelease(); window.__reads.fetch = 0; window.__usages.syncDefault(); window.__usages.load(); });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [window.__reads.guard, window.__reads.quota, window.__reads.fetch, window.__calls.filter(call => call[0] === 'list').length]), [0, 1, 0, 1], 'a destroyed page sends no further reads');
      await page.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('usages page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

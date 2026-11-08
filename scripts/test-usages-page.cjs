// Settings -> Usages (the provider cards and the account switcher) in Chromium. The source is the app's own mock of the usages API, with
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
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css">${importMap}</head>
<body><dialog id="settings-dialog" open><section id="settings-usages" class="settings-section active"></section></dialog></body></html>`;
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
  const cardOf = key => `#usages-list .usage-provider[data-provider="${key}"]`;
  try {
    // The owner's page: the switcher and its forms above, the providers in order below, each with its profiles and their limit windows.
    {
      const page = await mount();
      assert.deepEqual(await page.evaluate(() => ({
        controls: [...document.querySelector('#usages-switcher').children].map(node => node.className || node.tagName.toLowerCase()),
        toggle: document.querySelector('#usage-switcher-disable').textContent,
        providers: [...document.querySelectorAll('#usages-list .usage-provider')].map(card => [card.dataset.provider, card.querySelector('.usage-provider-name').textContent, card.querySelector('.usage-provider-status').textContent, card.querySelectorAll('.usage-profile').length]),
        affected: document.querySelector('#usages-list .usage-provider .usage-affected').textContent,
        settings: ['#usage-refresh-interval', '#usage-percent-display', '#usage-alert-warning', '#usage-alert-critical'].map(selector => document.querySelector(selector).value),
        synced: window.__synced })),
      { controls: ['usage-config-controls', 'settings-actions usage-switcher-danger'], toggle: 'Disable account switcher', providers: [['codex', 'Codex', 'Enabled', 2], ['claude-code', 'Claude Code', 'Enabled', 1], ['github-copilot', 'GitHub Copilot', 'Not configured', 1], ['mistral', 'Mistral', 'Not configured', 1], ['ollama-cloud', 'Ollama Cloud', 'Not configured', 1]],
        affected: 'Switch affects: milo@mock', settings: ['120', 'left', '20', '10'], synced: 2 });
      assert.deepEqual(await page.evaluate(() => {
        const card = document.querySelector('#usages-list [data-provider=codex] .usage-profile');
        const window = card.querySelector('.usage-window');
        return { name: card.querySelector('.usage-profile-name').textContent, active: card.querySelector('.usage-profile-active').hidden, windows: card.querySelectorAll('.usage-window').length, label: window.querySelector('.usage-track').getAttribute('aria-label'), now: window.querySelector('.usage-track').getAttribute('aria-valuenow'),
          marker: !!window.querySelector('.usage-pace-marker'), spark: !!window.querySelector('svg.usage-spark polyline'), pace: window.querySelector('.usages-pace-text').textContent, credits: card.querySelector('.usage-profile-credits').textContent, switchHidden: card.querySelector('.usage-switch').hidden };
      }), { name: 'main', active: false, windows: 3, label: 'Session left', now: '58', marker: true, spark: true, pace: '13% in reserve·Lasts until reset', credits: 'Credits: 12.50', switchHidden: true });
      assert.equal(await page.$eval('#usages-list [data-provider=codex] .usage-profile', el => getComputedStyle(el).borderTopWidth), '1px', 'the first profile card keeps its top edge (v1 drew none above the first of a tile)');
      assert.ok(/expected pace/.test(await page.$eval('#usages-list .usages-legend', el => el.textContent)) && /In reserve/.test(await page.$eval('#usages-list .usages-legend', el => el.textContent)), 'the marks on a card are explained');
      await page.close();
    }
    // An admin who is not the owner reads the limits: the cards, with no controls and no switcher, the quota and limit-guard blocks gone, and a line that says
    // only the owner changes Usages. The switcher is never asked for (it is the owner's, and a 403 would be shown as an error).
    {
      const page = await mount({ owner: false });
      assert.deepEqual(await page.evaluate(() => ({
        note: document.querySelector('#usages-readonly')?.textContent, switcherBox: document.querySelector('#usages-switcher'), quota: !!document.querySelector('.quota-settings'), guard: !!document.querySelector('.limit-guard-settings'),
        cards: document.querySelectorAll('#usages-list .usage-provider').length > 0, actions: document.querySelectorAll('#usages-list .usage-provider-actions button').length, rename: document.querySelectorAll('#usages-list .usage-rename:not([hidden])').length,
        remove: document.querySelectorAll('#usages-list .usage-remove:not([hidden])').length, switchButtons: document.querySelectorAll('#usages-list .usage-switch:not([hidden])').length, asked: window.__calls.filter(call => call[0] === 'switcher').length, error: document.querySelector('#usages-error').textContent })),
        { note: 'Only the owner can change Usages. You can see the limits here.', switcherBox: null, quota: false, guard: false, cards: true, actions: 0, rename: 0, remove: 0, switchButtons: 0, asked: 0, error: '' });
      await page.close();
      const owner = await mount();
      assert.deepEqual(await owner.evaluate(() => [!!document.querySelector('#usages-readonly'), !!document.querySelector('#usages-switcher')]), [false, true], 'the owner has no read-only note, and has the switcher');
      await owner.close();
    }
    // Switching asks first and names who is affected; the switch, then a read, follows. A failure says why.
    {
      const page = await mount();
      await click(page, `${cardOf('codex')} .usage-profile:nth-child(2) .usage-switch`);
      await dialog(page);
      assert.match(await page.$eval('#app-dialog .app-dialog-message', el => el.textContent), /Affected agents: milo@mock\. Switch to backup\?/);
      await click(page, '#app-dialog .app-dialog-cancel');
      await settle(page);
      assert.equal(await calls(page, 'use'), 0, 'declining switches nothing');
      const reads = await calls(page, 'list');
      await click(page, `${cardOf('codex')} .usage-profile:nth-child(2) .usage-switch`);
      await dialog(page);
      await click(page, '#app-dialog .app-dialog-ok');
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'use'));
      await page.waitForFunction(reads => window.__calls.filter(call => call[0] === 'list').length > reads, {}, reads);
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'use')), ['use', 'codex', 'backup']);
      await page.evaluate(() => { window.__source.use = async () => { throw new Error('The switch was refused'); }; });
      await click(page, `${cardOf('claude-code')} .usage-rename`);
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
    // Two reads answered out of order: the later request's answer is the one drawn.
    {
      const page = await mount({ held: ['list'] , load: false });
      await page.evaluate(() => { window.__usages.load(); window.__usages.load(); });
      await page.waitForFunction(() => window.__waiting.list?.length === 2);
      await page.evaluate(() => { window.__source.preferences.percent_display = 'used'; window.__release('list'); }); // the first read
      await settle(page);
      assert.equal(await page.evaluate(() => document.querySelectorAll('#usages-list .usage-provider').length), 0, 'a read superseded by a later one draws nothing, even answered first');
      await page.evaluate(() => window.__release('list'));
      await page.waitForFunction(() => document.querySelectorAll('#usages-list .usage-provider').length === 5);
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
      await click(page, `${cardOf('github-copilot')} .btn-primary`);
      await page.waitForFunction(() => document.querySelector('.usage-copilot-flow span'));
      assert.deepEqual(await page.evaluate(() => [document.querySelector('.usage-copilot-flow span').textContent, document.querySelector('.usage-copilot-flow a').href, document.querySelector('.usage-copilot-flow a').rel]), ['Code: ABCD-1234 · pending', 'https://github.com/login/device', 'noopener noreferrer']);
      await page.waitForFunction(() => !document.querySelector('.usage-copilot-flow span'), { timeout: 8000 });
      assert.equal(await page.evaluate(() => window.__polls), 2);
      await page.evaluate(() => { window.__polls = 0; window.__source.copilotPoll = async () => { window.__polls++; return { status: 'pending', interval: 1 }; }; });
      await click(page, `${cardOf('github-copilot')} .btn-primary`);
      await page.waitForFunction(() => window.__polls === 1);
      await page.evaluate(() => window.__usages.destroy());
      await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 1500)));
      assert.equal(await page.evaluate(() => window.__polls), 1, 'a destroyed page stops polling');
      await page.close();
    }
    // A verification address that is not https is shown as no link at all.
    for (const address of ['javascript:alert(1)', 'http://github.com/login/device', 'https://user:pw@github.com/login/device']) {
      const page = await mount();
      await page.evaluate(uri => {
        window.__source.copilotStart = async () => ({ flow_id: 'mock-flow', user_code: 'ABCD-1234', verification_uri: uri, interval: 60, expires_at: new Date(Date.now() + 600000).toISOString() });
        window.__source.copilotPoll = async () => ({ status: 'pending', interval: 60 });
      }, address);
      await click(page, `${cardOf('github-copilot')} .btn-primary`);
      await page.waitForFunction(() => document.querySelector('.usage-copilot-flow span'));
      assert.equal(await page.evaluate(() => document.querySelector('.usage-copilot-flow a')), null, `${address} became a link`);
      await page.close();
    }
    // GitHub sign-in started and the page destroyed before the start is answered: nothing is drawn and no poll is sent.
    {
      const page = await mount({ held: ['copilotStart'] });
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
      // Refresher conflicts of several tools are one alert in plain words with the paths folded under Details, in a red box.
      await page.evaluate(async () => {
        const list = window.__source.list.bind(window.__source);
        window.__source.list = async () => { const out = await list(); out.credential_alerts = [...(out.credential_alerts || []),
          { kind: 'refresher_conflict', message: 'claude: another token refresher may own the same live credentials: /h/.aimebu/switcher; this tool will not refresh' },
          { kind: 'refresher_conflict', message: 'codex: another token refresher may own the same live credentials: /h/.aimebu/switcher; this tool will not refresh' }]; return out; };
        await window.__usages.load();
      });
      assert.deepEqual(await page.evaluate(() => { const box = document.querySelector('.usage-credential-alerts'), conflict = box.querySelector('.usage-credential-alert-refresher_conflict');
        return { alerts: box.children.length, text: conflict.querySelector(':scope > p').textContent.slice(0, 60), details: conflict.querySelector('details > summary').textContent, lines: conflict.querySelectorAll('details p').length, pathInSentence: /\/h\/\.aimebu/.test(conflict.querySelector(':scope > p').textContent),
          red: getComputedStyle(box).borderTopColor !== getComputedStyle(document.body).borderTopColor && getComputedStyle(conflict).color }; }),
        { alerts: 2, text: 'Token refresh is paused for claude, codex: another aimebu (v', details: 'Details', lines: 2, pathInSentence: false, red: await page.evaluate(() => getComputedStyle(document.querySelector('.usage-credential-alert')).color) });
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
      assert.equal(await page.evaluate(() => document.querySelector('#usages-page, .usages-page').textContent.includes('main@example.test')), true, 'the owner sees the e-mail of a login');
      await page.evaluate(() => { window.__source.list = async () => { throw new Error('boom'); }; window.__usages.setOwner(false); window.__usages.forget(); });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => { const text = document.querySelector('.usages-page').textContent; return { email: text.includes('main@example.test'), alerts: document.querySelectorAll('.usage-credential-alert').length, cards: document.querySelectorAll('#usages-list .usage-provider').length, switcher: !!document.querySelector('#usages-switcher'), plans: /Pro\b/.test(text) }; }),
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
        const view = await page.evaluate(() => ({ stale: document.querySelectorAll('.usage-profile.is-stale').length, fresh: document.querySelectorAll('.usage-profile:not(.is-stale)').length > 0, text: document.querySelector('.usage-profile-stale')?.textContent.replace(/\s+/g, ' ').trim(),
          refresh: !!document.querySelector('.usage-profile-stale .usage-refresh'), color: getComputedStyle(document.querySelector('.usage-profile.is-stale .usage-fill')).backgroundColor }));
        assert.deepEqual([view.stale, view.fresh, view.text, view.refresh], [1, true, owner ? 'Stale, last update 2d ago Refresh' : 'Stale, last update 2d ago', owner], `${owner ? 'owner' : 'admin'}: ${JSON.stringify(view)}`);
        const look = await page.evaluate(() => {
          const color = value => { const probe = document.createElement('i'); probe.style.color = value; document.body.append(probe); const out = getComputedStyle(probe).color; probe.remove(); return out; };
          const card = document.querySelector('.usage-profile.is-stale'), fresh = document.querySelector('.usage-profile:not(.is-stale)');
          return { fill: getComputedStyle(card.querySelector('.usage-fill')).backgroundColor, muted: color(getComputedStyle(document.documentElement).getPropertyValue('--text-muted').trim()), freshFill: getComputedStyle(fresh.querySelector('.usage-fill')).backgroundColor,
            pace: card.querySelectorAll('.usages-pace-text, .usage-pace-marker').length, reset: [...card.querySelectorAll('.usage-window-foot span')].map(node => node.textContent) };
        });
        assert.equal(look.fill, look.muted, `a stale card's bars are the muted grey: ${JSON.stringify(look)}`);
        assert.notEqual(look.freshFill, look.muted, 'a fresh card keeps its colour');
        assert.equal(look.pace, 0, 'a stale card claims no pace');
        assert.ok(look.reset.every(text => /^(Reset passed|Resets .+)$/.test(text) && !/ in /.test(text)), `a stale card's reset is a fact, not a countdown: ${JSON.stringify(look.reset)}`);
        if (owner) {
          const before = await calls(page, 'refresh');
          await click(page, '.usage-profile-stale .usage-refresh'); await settle(page);
          assert.equal(await calls(page, 'refresh'), before + 1, 'the stale card refreshes its provider');
        }
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
      await page.$eval(`${cardOf('mistral')} textarea`, el => { el.value = 'cookie=1'; });
      await click(page, `${cardOf('mistral')} .btn-primary`);
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'mistral'));
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'mistral')), ['mistral', 'cookie=1']);
      await page.waitForFunction(() => [...document.querySelectorAll('[data-provider=mistral] button')].some(button => button.textContent === 'Clear cookie'));
      await page.$eval(`${cardOf('ollama-cloud')} input[type=password]`, el => { el.value = 'sk-1'; });
      await page.select(`${cardOf('ollama-cloud')} select`, 'api_key');
      await click(page, `${cardOf('ollama-cloud')} .btn-primary`);
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'ollama'));
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'ollama')[1]), { auth_mode: 'api_key', api_key: 'sk-1' });
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

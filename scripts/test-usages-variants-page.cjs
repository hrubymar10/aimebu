// Settings -> Usages: the A | B | C pill and variant B (v1's tiles), in Chromium. The source is the app's own mock of the usages API, with
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
<body><span id="usages-variant-slot"></span><dialog id="settings-dialog" class="settings-modal" aria-label="Settings" open><div class="settings-pane"><nav class="settings-nav" aria-label="Settings sections"></nav><div class="settings-body"><header class="settings-body-header"></header><section id="settings-usages" class="settings-section active"></section></div></div></dialog></body></html>`;
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
      (await import('/js/pages/usages/variant-pill.js')).mountUsagesVariantPill(document.querySelector('#usages-variant-slot'));
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
  const tab = (page, name) => click(page, `.usages-variant-option[data-variant=${name}]`);
  const tile = key => `#usages-list .usages-b-tile[data-provider="${key}"]`;
  try {
    // The pill: A by default, and the choice is kept per browser.
    {
      const page = await mount();
      assert.deepEqual(await page.evaluate(() => ({ options: [...document.querySelectorAll('.usages-variant-option')].map(el => [el.textContent, el.getAttribute('aria-pressed')]), a: !!document.querySelector('#usages-list .usage-provider'), b: !!document.querySelector('.usages-b') })),
        { options: [['A', 'true'], ['B', 'false'], ['C', 'false']], a: true, b: false }, 'A is the default and is the page as it was');
      await tab(page, 'c');
      assert.deepEqual(await page.evaluate(() => ({ variant: document.querySelector('#usages-list').dataset.variant, headline: !!document.querySelector('#usages-list .usages-c .uc-headline'), providers: document.querySelectorAll('#usages-list .usages-c .uc-provider').length, a: !!document.querySelector('#usages-list .usage-provider') })),
        { variant: 'c', headline: true, providers: 5, a: false }, 'C draws its headline and a group per provider in place of the list');
      assert.equal(await page.evaluate(() => localStorage.getItem('aimebu2.usages.variant')), 'c');
      await tab(page, 'a');
      assert.equal(await page.evaluate(() => !!document.querySelector('#usages-list .usage-provider') && !document.querySelector('.usages-b')), true, 'back to A');
      await tab(page, 'b');
      assert.equal(await page.evaluate(() => localStorage.getItem('aimebu2.usages.variant')), 'b');
      await page.close();
      const again = await mount();
      assert.equal(await again.evaluate(() => !!document.querySelector('.usages-b') && document.querySelector('.usages-variant-option.active').textContent), 'B', 'B is remembered when the page is opened again');
      // B draws from the same data as A: a tile per provider, in the same order, each profile under its provider.
      assert.deepEqual(await again.evaluate(() => [...document.querySelectorAll('#usages-list .usages-b-tile')].map(tile => [tile.dataset.provider, tile.querySelector('.usages-provider-name').textContent, tile.querySelector('.usages-provider-updated').textContent.replace(/^Updated .*/, 'Updated'), tile.querySelectorAll('.usages-b-profile').length])),
        [['codex', 'Codex', 'Updated', 2], ['claude-code', 'Claude Code', 'ok', 1], ['github-copilot', 'GitHub Copilot', 'Not configured', 0], ['mistral', 'Mistral', 'Not configured', 0], ['ollama-cloud', 'Ollama Cloud', 'Not configured', 0]]);
      // One profile in v1's shape: name, expiry icon, switch, Active, plan; each limit a bar with a pace tick and a hairline of the last 48 h, "N% left" and the reset, then the pace words.
      const main = await again.evaluate(selector => {
        const row = document.querySelector(selector), window = row.querySelector('.usages-window-row');
        return { name: row.querySelector('.switcher-tile-name').textContent, active: row.querySelector('.switcher-tile-active')?.textContent, plan: row.querySelector('.switcher-tile-plan').textContent, expiry: row.querySelector('.switcher-expiry-icon')?.dataset.level,
          windows: row.querySelectorAll('.usages-window-row').length, label: window.querySelector('.usages-window-top').textContent, now: window.querySelector('.usages-window-meta strong').textContent, tick: !!window.querySelector('.usages-pace-tick--reserve'),
          spark: !!window.querySelector('svg.usages-b-spark polyline'), pace: window.querySelector('.usages-pace-text').textContent, credits: row.querySelector('.usages-credits-row').textContent, switchHidden: !row.querySelector('.usages-b-switch'), textButtons: [...row.querySelectorAll('button')].filter(button => button.textContent.trim()).length,
          untitled: [...row.querySelectorAll('button')].filter(button => !button.title).length };
      }, `${tile('codex')} .usages-b-profile[data-profile=main]`);
      assert.deepEqual(main, { name: 'main', active: 'Active', plan: 'Pro', expiry: 'ok', windows: 3, label: 'Session', now: '58% left', tick: true, spark: true, pace: '13% in reserve·Lasts until reset', credits: 'Credits: 12.50', switchHidden: true, textButtons: 0, untitled: 0 });
      assert.deepEqual(await again.evaluate(selector => { const row = document.querySelector(selector); return { inactive: row.querySelector('.usages-b-inactive').textContent, switchLabel: row.querySelector('.usages-b-switch').title, expiry: row.querySelector('.switcher-expiry-icon').dataset.level }; }, `${tile('codex')} .usages-b-profile[data-profile=backup]`),
        { inactive: 'Inactive', switchLabel: 'Switch to backup', expiry: 'expiring' }, 'an inactive account has the switch icon and its token state');
      // v1's look: the tile is the v1 card, the bar is a thin rounded track, and there is no spark caption.
      assert.deepEqual(await again.evaluate(selector => { const t = document.querySelector(selector), tr = t.querySelector('.usages-progress'); return { radius: getComputedStyle(t).borderTopLeftRadius !== '0px', trackH: getComputedStyle(tr).height, caption: !!t.querySelector('.usage-spark-caption'), legend: /expected pace/.test(document.querySelector('.usages-legend').textContent) }; }, tile('codex')),
        { radius: true, trackH: '6px', caption: false, legend: true });
      // Switching asks first, then writes: the same flow as A.
      await click(again, `${tile('codex')} .usages-b-profile[data-profile=backup] .usages-b-switch`);
      await dialog(again);
      assert.match(await again.$eval('#app-dialog .app-dialog-message', el => el.textContent), /Affected agents: milo@mock\. Switch to backup\?/);
      await click(again, '#app-dialog .app-dialog-ok');
      await again.waitForFunction(() => window.__calls.some(call => call[0] === 'use'));
      assert.deepEqual(await again.evaluate(() => window.__calls.find(call => call[0] === 'use')), ['use', 'codex', 'backup']);
      // What changes a provider is behind one gear on the tile; an icon with a tooltip each.
      assert.equal(await again.$eval(tile('mistral'), el => !!el.querySelector('.usages-b-manage-panel')), false, 'closed by default');
      await click(again, `${tile('mistral')} .usages-b-manage`);
      assert.deepEqual(await again.evaluate(selector => { const panel = document.querySelector(selector); return { buttons: [...panel.querySelectorAll('button')].map(button => button.title || button.textContent.trim()).slice(0, 3), area: !!panel.querySelector('textarea') }; }, `${tile('mistral')} .usages-b-manage-panel`),
        { buttons: ['Enable Mistral', 'Move Mistral up', 'Move Mistral down'], area: true });
      await click(again, `${tile('claude-code')} .usages-b-manage`);
      await click(again, `${tile('claude-code')} .usages-b-profile .usages-b-rename`);
      await dialog(again);
      await click(again, '#app-dialog .app-dialog-cancel');
      // Group by active first: the active accounts, a divider, then the rest.
      await click(again, '.usages-b-group');
      assert.deepEqual(await again.evaluate(() => ({ grids: document.querySelectorAll('.usages-b-grid').length, divider: !!document.querySelector('.usages-group-divider'), pressed: document.querySelector('.usages-b-group').getAttribute('aria-pressed'), stored: localStorage.getItem('aimebu2.usages.group') })),
        { grids: 2, divider: true, pressed: 'true', stored: 'active' });
      await click(again, '.usages-b-group');
      // A phone: one column, nothing cut off sideways.
      await again.setViewport({ width: 390, height: 800 });
      assert.deepEqual(await again.evaluate(() => { const grid = document.querySelector('.usages-b-grid'); return { columns: getComputedStyle(grid).gridTemplateColumns.split(' ').length, overflow: document.documentElement.scrollWidth > window.innerWidth }; }), { columns: 1, overflow: false });
      if (process.env.USAGES_SHOTS) for (const theme of ['dark', 'light', 'high-contrast-dark', 'high-contrast-light', 'blue-dark', 'blue-light', 'green-dark', 'green-light', 'red-dark', 'red-light']) { await again.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme); await again.setViewport({ width: 900, height: 1100 }); await settle(again); await new Promise(resolve => setTimeout(resolve, 700)); await again.screenshot({ path: path.join(process.env.USAGES_SHOTS, `usages-b-${theme}.png`) }); }
      await again.close();
    }
    // An admin who is not the owner reads the limits: no gear, no switch, no rename.
    {
      const page = await mount({ owner: false });
      await tab(page, 'b');
      assert.deepEqual(await page.evaluate(() => ({ tiles: document.querySelectorAll('.usages-b-tile').length, gear: document.querySelectorAll('.usages-b-manage').length, sw: document.querySelectorAll('.usages-b-switch').length, refresh: document.querySelectorAll('.usages-b-refresh-all').length })), { tiles: 5, gear: 0, sw: 0, refresh: 0 });
      await page.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('usages variants page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

// Settings -> Usages: the opt-in usage alerts in settings, fixed-width digits, the privacy toggle and the compact density. The source is the app's own mock of the
// usages API. web/ is served as is with its import map; no Go server. Part of `make test-ui`.
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
  const mount = async () => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async () => {
      localStorage.clear();
      const { MockUsagesSource } = await import('/js/usages-source.js');
      const { UsagesSettings } = await import('/js/pages/usages/page.js');
      const source = new MockUsagesSource();
      window.__source = source; window.__calls = [];
      for (const name of ['list', 'switcher', 'settings', 'refresh']) {
        if (!source[name]) continue;
        const original = source[name].bind(source);
        source[name] = (...args) => { window.__calls.push([name, ...args]); return original(...args); };
      }
      const section = document.querySelector('#settings-usages'), dialog = document.querySelector('#settings-dialog');
      const absent = { load: async () => { const error = new Error('not served'); error.status = 404; throw error; } };
      const quotaSource = absent, guardSource = absent;
      window.__usages = new UsagesSettings({ source, quotaSource, guardSource, dialog, section });
      window.__usages.setOwner(true);
      await window.__usages.load();
    });
    return page;
  };
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
  const click = (page, selector) => page.$eval(selector, el => el.click());
  try {
    // The toggles appear off by default and post their own change, the digits are fixed-width, the view is compact.
    {
      const page = await mount();
      assert.deepEqual(await page.evaluate(() => {
        const pace = document.querySelector('#usage-alert-pace');
        const meta = document.querySelector('.usages-window-meta strong');
        return {
          group: document.querySelector('.usage-config-controls .settings-group[aria-label="Usage alerts"]') !== null,
          checks: ['#usage-alert-pace', '#usage-alert-reset', '#usage-alert-expiry'].map(selector => document.querySelector(selector)?.checked ?? null),
          privacy: !!document.querySelector('.usages-b-privacy'),
          root: document.querySelector('.usages-b').className,
          digits: meta ? getComputedStyle(meta).fontVariantNumeric : null,
        };
      }), { group: true, checks: [false, false, false], privacy: true, root: 'usages-b', digits: 'tabular-nums' });
      await click(page, '#usage-alert-pace');
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'settings'));
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'settings')[1]), { alert_pace_enabled: true });
      await page.waitForFunction(() => document.querySelector('#usage-alert-pace').checked === true, { timeout: 8000 });
      await page.close();
    }
    // The privacy toggle masks profile names and the plan tooltip's email, and unmasks again.
    {
      const page = await mount();
      const main = '.usages-b-tile[data-provider="codex"] .usages-b-profile';
      assert.deepEqual(await page.evaluate(selector => {
        const card = document.querySelector(selector);
        return { name: card.querySelector('.switcher-tile-name').textContent, tooltip: card.querySelector('.switcher-tile-plan').title, pressed: document.querySelector('.usages-b-privacy').getAttribute('aria-pressed') };
      }, main), { name: 'main', tooltip: 'Pro · main@example.test', pressed: 'false' });
      await click(page, '.usages-b-privacy');
      await page.waitForFunction(selector => document.querySelector(`${selector} .switcher-tile-name`).textContent !== 'main', {}, main);
      assert.deepEqual(await page.evaluate(selector => {
        const card = document.querySelector(selector);
        return { name: card.querySelector('.switcher-tile-name').textContent, tooltip: card.querySelector('.switcher-tile-plan').title, pressed: document.querySelector('.usages-b-privacy').getAttribute('aria-pressed'), label: document.querySelector('.usages-b-privacy').getAttribute('aria-label') };
      }, main), { name: 'm•••', tooltip: 'Pro · m•••@e•••', pressed: 'true', label: 'Show account names and emails' });
      await click(page, '.usages-b-privacy');
      await page.waitForFunction(selector => document.querySelector(`${selector} .switcher-tile-name`).textContent === 'main', {}, main);
      assert.equal(await page.evaluate(() => document.querySelector('.usages-b-privacy').getAttribute('aria-label')), 'Mask account names and emails');
      await page.close();
    }
    // Credential-alert sentences mask their profile names while privacy is on.
    {
      const page = await mount();
      await page.evaluate(async () => {
        const list = window.__source.list.bind(window.__source);
        window.__source.list = async () => ({ ...(await list()), credential_alerts: [{ kind: 'expiring', tool: 'claude', profile: 'main', role: 'store', message: 'claude profile main: the access token expires in 4 min.' }] });
        await window.__usages.load();
      });
      assert.match(await page.$eval('.usage-credential-alerts p', el => el.textContent), /claude profile main:/, 'without privacy shows the name');
      await click(page, '.usages-b-privacy');
      await page.waitForFunction(() => /claude profile m•••:/.test(document.querySelector('.usage-credential-alerts p').textContent));
      await page.close();
    }
    // The Switch, Install anyway, Rename and Delete dialogs mask the profile name in their text while privacy is on; Rename's input keeps the real one.
    {
      const page = await mount();
      const tile = '.usages-b-tile[data-provider="codex"]', profile = `${tile} .usages-b-profile:not(.is-active)`; // data-profile carries the masked name, so pick the non-active one by position
      const dialogText = () => page.evaluate(() => ({ message: document.querySelector('.app-dialog-message').textContent, fields: [...document.querySelectorAll('.app-dialog-fields input')].map(input => input.value) }));
      const cancel = async () => { await click(page, '#app-dialog .app-dialog-cancel'); await page.waitForFunction(() => !document.querySelector('#app-dialog')?.open); };
      await click(page, '.usages-b-privacy');
      await click(page, `${tile} .usages-b-manage`);
      await page.waitForSelector(`${profile} .usages-b-remove`);
      assert.equal(await page.$eval(`${profile} .switcher-tile-name`, el => el.textContent), 'b•••');
      await click(page, `${profile} .usages-b-remove`);
      await page.waitForSelector('#app-dialog[open] .app-dialog-message:not([hidden])');
      let shown = await dialogText();
      assert.match(shown.message, /Delete b•••\?/);
      assert.ok(!shown.message.includes('backup'), 'Delete masks the name');
      await cancel();
      await click(page, `${profile} .usages-b-rename`);
      await page.waitForSelector('#app-dialog[open] .app-dialog-fields input');
      shown = await dialogText();
      assert.ok(!(await page.$eval('#app-dialog-title', el => el.textContent)).includes('backup'));
      assert.deepEqual(shown.fields, ['backup'], 'Rename keeps the real name in its input');
      await cancel();
      await click(page, `${profile} .usages-b-switch`);
      await page.waitForSelector('#app-dialog[open] .app-dialog-message:not([hidden])');
      shown = await dialogText();
      assert.match(shown.message, /Switch to b•••\?/);
      assert.ok(!shown.message.includes('backup'), 'Switch masks the name');
      await cancel();
      await page.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('usages extras page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

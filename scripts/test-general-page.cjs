// Settings -> General's Appearance block in Chromium: what a person's saved choices show, and what choosing writes (the page, the browser's
// storage, the server's preferences). web/ is served as is with its import map; no Go server. Part of `make test-ui`.
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
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css">${importMap}</head>
<body><dialog id="settings-dialog" open><section id="settings-general" class="settings-content"><div class="settings-section" id="settings-appearance"></div></section></dialog></body></html>`;
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
  const mount = async (stored = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async stored => {
      localStorage.clear();
      for (const [key, value] of Object.entries(stored)) localStorage.setItem(key, value);
      window.__puts = []; window.__previews = 0;
      window.fetch = async (input, options = {}) => {
        if (options.method === 'PUT') window.__puts.push(JSON.parse(options.body));
        return new Response(JSON.stringify({ preferences: {} }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      };
      document.addEventListener('aimebu:sidebar-preview', () => { window.__previews++; });
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'u1' });
      await (await import('/js/preferences.js')).loadPreferences('u1', { send: async () => ({ preferences: {} }) });
      const { AppearanceSettings } = await import('/js/pages/general/page.js');
      window.__appearance = new AppearanceSettings();
    }, stored);
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
      await page.evaluate(() => window.__appearance.setUser('u1'));
      await settle(page);
      assert.deepEqual(await view(page).then(({ theme, attribute, preview }) => ({ theme, attribute, preview })), { theme: 'high-contrast-light', attribute: 'high-contrast-light', preview: true });
      assert.equal(await page.evaluate(() => localStorage.getItem('aimebu2.ui.last-user')), 'u1');
      await page.close();
      const gone = await mount({ 'aimebu2.ui.theme.u1': 'retired' });
      await gone.evaluate(() => window.__appearance.setUser('u1'));
      await settle(gone);
      assert.deepEqual(await view(gone).then(({ theme, attribute, preview }) => ({ theme, attribute, preview })), { theme: 'dark', attribute: null, preview: false });
      await gone.close();
    }
    // Choosing a theme applies it, keeps it in the browser for this person and saves it to the account; dark is no attribute.
    {
      const page = await mount();
      await page.evaluate(() => window.__appearance.setUser('u1'));
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
      await page.evaluate(() => window.__appearance.setUser('u1'));
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
      await page.evaluate(() => window.__appearance.destroy());
      assert.equal(await page.evaluate(() => document.querySelector('#settings-appearance').childElementCount), 0);
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

// The three bars (web/js/pages/banners/) in Chromium: each bar's element, where it is put, its role and label, its icon buttons, and that a
// call draws at once. Their rules are in scripts/test-update-banner.cjs and scripts/test-notification-banner.cjs (vm); the update bar's reload
// rules in scripts/test-events-page.cjs. web/ is served as is with its import map; no Go server. Part of `make test-ui`.
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
<body><main id="first"></main><div class="chat"><header>Room</header><div id="messages"></div></div></body></html>`;
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
  try {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);

    // The notification banner sits right after the room header and the update banner right after it (app.js anchors it on `.bar`, which exists as
    // soon as the banner is constructed); both hidden, labelled regions with icon buttons (a tooltip and a name each, no text).
    const placed = await page.evaluate(async () => {
      const { NotificationBanner, UpdateBanner } = await import('/js/pages/banners/page.js');
      window.__asks = [];
      window.__notify = new NotificationBanner({ notifications: { available: true, requestPermission: () => new Promise(resolve => window.__asks.push(resolve)) }, saveDismissed: () => { window.__saved = (window.__saved || 0) + 1; }, permission: () => 'default' });
      const anchored = !!window.__notify.bar;
      window.__acks = [];
      window.__update = new UpdateBanner({ getStatus: async () => window.__status, ack: async kind => { window.__acks.push(kind); if (window.__refuse) throw new Error('refused'); }, anchor: window.__notify.bar });
      const header = document.querySelector('.chat > header');
      const bar = node => ({ hidden: node.hidden, role: node.getAttribute('role'), label: node.getAttribute('aria-label'), glyph: node.firstElementChild?.className, glyphHidden: node.firstElementChild?.getAttribute('aria-hidden'),
        buttons: [...node.querySelectorAll('button')].map(button => ({ cls: button.className, title: button.title, named: button.getAttribute('aria-label') === button.title, text: button.textContent, type: button.type })) });
      return { anchored, order: [header.nextElementSibling === window.__notify.bar, window.__notify.bar.nextElementSibling === window.__update.bar], notify: bar(window.__notify.bar), update: bar(window.__update.bar),
        texts: [window.__notify.bar.querySelector('.notification-banner-text')?.textContent, window.__notify.bar.className, window.__update.bar.className] };
    });
    assert.equal(placed.anchored, true, 'the notification banner has its element at once');
    assert.deepEqual(placed.order, [true, true], 'right after the header, then the update banner right after it');
    assert.deepEqual(placed.texts, ['Get a browser notification when something needs you.', 'notification-banner', 'update-banner']);
    assert.deepEqual([placed.notify.hidden, placed.notify.role, placed.notify.label, placed.notify.glyph, placed.notify.glyphHidden], [true, 'region', 'Browser notifications', 'icon-mask icon-mask-bell', 'true']);
    assert.deepEqual([placed.update.hidden, placed.update.role, placed.update.label, placed.update.glyph, placed.update.glyphHidden], [true, 'region', 'aimebu update', 'icon-mask icon-mask-health', 'true']);
    assert.deepEqual(placed.notify.buttons.map(button => button.cls), ['btn-icon notification-banner-enable', 'btn-icon notification-banner-dismiss']);
    assert.deepEqual(placed.update.buttons.map(button => button.cls), ['btn-icon update-banner-copy', 'btn-icon update-banner-remind', 'btn-icon update-banner-skip']);
    for (const button of [...placed.notify.buttons, ...placed.update.buttons]) assert.ok(button.title && button.named && button.text === '' && button.type === 'button', `icon button: ${JSON.stringify(button)}`);

    // A call draws at once: attention shows the bar, Enable is off while the browser asks, and the bar closes once it answered; Dismiss is saved.
    const notified = await page.evaluate(async () => {
      const out = {}; const bar = window.__notify.bar, enable = () => bar.querySelector('.notification-banner-enable');
      window.__notify.attention(); out.shown = !bar.hidden;
      enable().click(); out.asking = [enable().disabled, window.__asks.length];
      window.__asks[0](); await new Promise(resolve => setTimeout(resolve, 0)); out.answered = [bar.hidden, enable().disabled];
      window.__notify.reset(false); window.__notify.attention(); bar.querySelector('.notification-banner-dismiss').click(); out.dismissed = [bar.hidden, window.__saved, window.__notify.dismissed];
      return out;
    });
    assert.deepEqual(notified, { shown: true, asking: [true, 1], answered: [true, false], dismissed: [true, 1, true] });

    // The update banner says what the server found, with the version in bold and the command as code; a refused answer is in the bar's tooltip.
    const updated = await page.evaluate(async () => {
      const bar = window.__update.bar, out = {};
      window.__status = { checked: true, state: 'available', version: 'v2.1.0' };
      await window.__update.setAccount({ admin: true, owner: false });
      out.shown = [bar.hidden, bar.querySelector('.update-banner-text').innerHTML];
      window.__refuse = true; bar.querySelector('.update-banner-remind').click(); await new Promise(resolve => setTimeout(resolve, 0));
      out.refused = [bar.hidden, bar.title, window.__acks.slice()];
      window.__refuse = false; bar.querySelector('.update-banner-skip').click(); await new Promise(resolve => setTimeout(resolve, 0));
      out.skipped = [bar.hidden, bar.title, window.__acks.slice()];
      window.__status = { checked: true, state: 'unbuilt', version: 'v2.2.0' };
      await window.__update.refresh(); out.unbuilt = bar.querySelector('.update-banner-text').innerHTML;
      return out;
    });
    assert.deepEqual(updated.shown, [false, '<strong>v2.1.0</strong> is available on this channel — run <code>aimebu update</code> on the server host.']);
    assert.deepEqual(updated.refused, [false, 'Could not save your answer: refused', ['remind']]);
    assert.deepEqual(updated.skipped, [true, '', ['remind', 'skip']]);
    assert.equal(updated.unbuilt, '<strong>v2.2.0</strong> is checked out but the built binary is older — delete the cached binary in the clone (<code>aimebu-&lt;os&gt;-&lt;arch&gt;</code>), run <code>aimebu version</code> once to rebuild it, then restart the server.');

    // The update bar is made on its first call and put first in the body: stale() keeps it up while a draft would be lost, with a reload icon
    // button; a later show() keeps what it says.
    const stale = await page.evaluate(async () => {
      const { UpdateBar } = await import('/js/pages/banners/page.js');
      let reloads = 0; const bar = new UpdateBar({ reload: () => { reloads++; }, hasDraft: () => true });
      const before = [bar.bar, document.querySelector('#update-bar')];
      const answer = bar.stale(); bar.show();
      const node = document.querySelector('#update-bar');
      const out = { before, answer, first: document.body.firstElementChild === node && bar.bar === node, cls: node.className, role: node.getAttribute('role'), text: node.querySelector('span').textContent,
        button: [node.querySelector('button').className, node.querySelector('button').getAttribute('aria-label'), node.querySelector('button').title, node.querySelector('button .icon-mask-refresh') !== null] };
      node.querySelector('button').click(); out.reloads = reloads; node.remove();
      return out;
    });
    assert.deepEqual(stale, { before: [null, null], answer: 'bar', first: true, cls: 'update-bar', role: 'alert', text: 'A new version is ready, reload to continue.', button: ['btn-icon update-bar-reload', 'Reload', 'Reload', true], reloads: 1 });

    assert.deepEqual(errors, [], 'no page errors');
    console.log('banners page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

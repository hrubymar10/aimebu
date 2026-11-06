// A click on the backdrop of a modal dialog closes it, like Escape: a plain <dialog>, a full-screen overlay with a pane (Settings), a dialog
// that guards its closing, a drag that starts inside and ends outside, and a dialog that is not modal. web/ is served as is; no Go server.
// Part of `make test-ui`.
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
<body>
<dialog id="plain" class="dialog-card"><h2>Plain</h2><input id="plain-input" value="some text to select"><button id="plain-close">Close</button></dialog>
<dialog id="settings-dialog" class="settings-modal" aria-label="Settings"><div class="settings-pane"><nav class="settings-nav"></nav><div class="settings-body"><section class="settings-section active"><input id="pane-input" value="text"></section></div></div></dialog>
<dialog id="guarded" class="dialog-card"><h2>Guarded</h2><input id="guarded-input"></dialog>
<dialog id="modeless" class="dialog-card"><h2>Modeless</h2></dialog>
<script type="module">import '/js/backdrop-close.js'; window.__ready = true;</script>
</body></html>`;
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
  const page = await browser.newPage();
  await page.setViewport({ width: 1100, height: 800 });
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
  try {
    await page.goto(`${origin}/fixture.html`);
    await page.waitForFunction(() => window.__ready);
    const open = id => page.evaluate(id => { const dialog = document.querySelector(id); dialog.showModal(); }, id);
    const isOpen = id => page.evaluate(id => document.querySelector(id).open, id);
    const box = id => page.evaluate(id => { const r = document.querySelector(id).getBoundingClientRect(); return { left: r.left, top: r.top, right: r.right, bottom: r.bottom }; }, id);

    // a plain dialog: a click outside its box closes it; a click inside does not
    await open('#plain');
    let b = await box('#plain');
    await page.mouse.click(b.left + 20, b.top + 20);
    assert.equal(await isOpen('#plain'), true, 'a click inside the dialog leaves it open');
    await page.mouse.click(b.right + 40, b.top + 20);
    assert.equal(await isOpen('#plain'), false, 'a click on the backdrop closes it');

    // a drag that starts in a field and ends on the backdrop is a selection, not a click on the backdrop
    await open('#plain');
    b = await box('#plain');
    const input = await page.evaluate(() => { const r = document.querySelector('#plain-input').getBoundingClientRect(); return { x: r.left + 10, y: r.top + r.height / 2 }; });
    await page.mouse.move(input.x, input.y); await page.mouse.down(); await page.mouse.move(b.right + 60, input.y, { steps: 5 }); await page.mouse.up();
    assert.equal(await isOpen('#plain'), true, 'a drag from inside to outside does not close it');
    // a press on the backdrop that is released inside the dialog is no click on the backdrop either
    await page.mouse.move(b.right + 40, b.top + 20); await page.mouse.down(); await page.mouse.move(b.left + 30, b.top + 30, { steps: 5 }); await page.mouse.up();
    assert.equal(await isOpen('#plain'), true, 'a press outside that ends inside does not close it');
    await page.mouse.click(b.left - 30, b.top + 20);
    assert.equal(await isOpen('#plain'), false, 'a click on the backdrop to the left closes it too');

    // a full-screen overlay with a pane (Settings): the overlay is the dialog element, the pane is its content
    await open('#settings-dialog');
    b = await box('.settings-pane');
    await page.mouse.click(b.left + 30, b.top + 30);
    assert.equal(await isOpen('#settings-dialog'), true, 'a click on the pane leaves Settings open');
    await page.mouse.click(Math.max(5, b.left - 40), b.top + 60);
    assert.equal(await isOpen('#settings-dialog'), false, 'a click on the overlay beside the pane closes it');

    // a dialog that guards its closing: the backdrop click asks the same way Escape does, and the guard can keep it open
    await page.evaluate(() => { window.__asked = 0; window.__allow = false; document.querySelector('#guarded').addEventListener('cancel', event => { window.__asked++; if (!window.__allow) event.preventDefault(); }); });
    await open('#guarded');
    b = await box('#guarded');
    await page.mouse.click(b.right + 40, b.top + 20);
    assert.deepEqual([await isOpen('#guarded'), await page.evaluate(() => window.__asked)], [true, 1], 'the guard is asked and keeps the dialog open');
    await page.keyboard.press('Escape');
    assert.deepEqual([await isOpen('#guarded'), await page.evaluate(() => window.__asked)], [true, 2], 'Escape asks the same guard');
    await page.evaluate(() => { window.__allow = true; });
    await page.mouse.click(b.right + 40, b.top + 20);
    assert.deepEqual([await isOpen('#guarded'), await page.evaluate(() => window.__asked)], [false, 3], 'once the guard allows it, the backdrop click closes it');

    // a dialog opened without showModal has no backdrop to click
    await page.evaluate(() => document.querySelector('#modeless').show());
    await page.mouse.click(5, 790);
    assert.equal(await isOpen('#modeless'), true, 'a non-modal dialog is not closed by a click elsewhere');
    assert.deepEqual(errors, [], 'no page errors');
    console.log('backdrop page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

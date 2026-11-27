// The tooltip on a relative time (data-time-tip, web/js/time-tip.js) on a touch screen: a long press shows the absolute date, a touch that moves
// or ends early shows nothing, and the bubble goes away by itself. The product's own timings (500 ms hold, 2.5 s show) are waited out with generous
// deadlines that only detect a hang: the test asserts that the state occurred, never how long it took. web/ is served as is; no Go server.
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
const TIP = 'Mar 5, 2026, 9:07:09 AM EST';
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css"><link rel="stylesheet" href="/v2-only.css">${importMap}</head>
<body style="padding:60px"><p>Sent <time id="stamp" datetime="2026-03-05T14:07:09Z" data-time-tip="${TIP}">5m ago</time> by someone.</p><p id="plain">Not a time</p>
<script type="module">import '/js/time-tip.js'; window.__ready = true;</script>
</body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});
const HANG = 20000; // a deadline to detect a hang, not a measurement

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  const page = await browser.newPage();
  await page.setViewport({ width: 390, height: 800, hasTouch: true, isMobile: true });
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
  try {
    await page.goto(`${origin}/fixture.html`);
    await page.waitForFunction(() => window.__ready);
    const touch = (type, id = 'stamp') => page.evaluate((type, id) => {
      const el = document.getElementById(id), r = el.getBoundingClientRect();
      const point = new Touch({ identifier: 1, target: el, clientX: r.left + 3, clientY: r.top + 3 });
      el.dispatchEvent(new TouchEvent(type, { bubbles: true, cancelable: true, touches: type === 'touchend' ? [] : [point], changedTouches: [point] }));
    }, type, id);
    const bubble = () => page.$eval('.time-tip', node => node.textContent).catch(() => null);

    // a long press shows the absolute date; the bubble goes away by itself
    await touch('touchstart');
    await page.waitForSelector('.time-tip', { timeout: HANG });
    assert.equal(await bubble(), TIP, 'a long press shows the absolute date');
    await page.waitForFunction(() => !document.querySelector('.time-tip'), { timeout: HANG });

    // a touch that moves before the hold is over is a scroll, not a long press: no bubble (looked for after the whole hold has passed)
    await touch('touchstart'); await touch('touchmove');
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 1500)));
    assert.equal(await bubble(), null, 'a touch that moved shows no bubble');

    // a touch that ends before the hold shows no bubble either
    await touch('touchstart'); await touch('touchend');
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 1500)));
    assert.equal(await bubble(), null, 'a tap that ended early shows no bubble');

    // an element without a tip shows none
    await touch('touchstart', 'plain');
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 1500)));
    assert.equal(await bubble(), null, 'an element with no tip shows no bubble');

    // and a long press works again after all that
    await touch('touchstart');
    await page.waitForSelector('.time-tip', { timeout: HANG });
    assert.equal(await bubble(), TIP, 'a later long press still shows the date');
    assert.deepEqual(errors, [], 'no page errors');
    console.log('time tip page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

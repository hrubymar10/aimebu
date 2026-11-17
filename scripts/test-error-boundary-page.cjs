// A render error inside a page's ErrorBoundary is reported to Sentry (tag `source` = render) while reporting runs, once per cause, and the page it wraps is gone
// from the screen as before. Chromium on web/ served statically, with a stand-in for the SDK. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const importMap = fs.readFileSync(path.join(root, 'index.html'), 'utf8').match(/<script type="importmap">.*?<\/script>/s)[0];
const fixture = `<!doctype html><html><head><meta charset="utf-8">${importMap}</head><body><div id="host"></div></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  try {
    const page = await browser.newPage();
    await page.goto(`http://127.0.0.1:${server.address().port}/fixture.html`);
    const result = await page.evaluate(async () => {
      const { html, render } = await import('/js/ui.js');
      const { ErrorBoundary } = await import('/js/components/error-boundary.js');
      const { startTelemetry } = await import('/js/telemetry.js');
      const sent = [];
      const fake = { setUser() {}, init() {}, browserTracingIntegration: () => 'tracing', captureException: error => sent.push(error.message), withScope: run => run({ setTag: (name, value) => sent.push(`${name}=${value}`), setFingerprint() {}, setLevel() {} }) };
      globalThis.Sentry = fake;
      await startTelemetry({ dsn: 'https://k@h.example/1' }, { load: async () => {}, Sentry: () => fake });
      const Boom = () => { throw new Error('render exploded'); };
      const host = document.querySelector('#host'), seen = [];
      render(html`<${ErrorBoundary} onError=${error => seen.push(error.message)}><${Boom} /><//>`, host);
      await new Promise(resolve => setTimeout(resolve, 100));
      render(html`<${ErrorBoundary} onError=${error => seen.push(error.message)}><${Boom} /><//>`, host); // the same cause again
      await new Promise(resolve => setTimeout(resolve, 100));
      return { sent, seen, empty: host.textContent === '' };
    });
    assert.deepEqual(result.sent, ['source=render', 'render exploded'], 'the render error is reported once, tagged with its place');
    assert.equal(result.empty, true, 'the failed page is gone from the screen, as before');
    assert.ok(result.seen.includes('render exploded'), "the page's own onError still hears of it");
  } finally { await browser.close(); server.close(); }
  console.log('error boundary page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });

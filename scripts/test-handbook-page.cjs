// Read-only project handbook: home-header entry, Markdown, paginated history
// and snapshots. The fixture serves web/ directly so it needs no Go server.
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
<body><div class="room-heading"><button type="button" id="room-info" class="btn-icon" aria-label="Room settings">Settings</button></div></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' };
const server = http.createServer((req, res) => {
  const pathname = new URL(req.url, 'http://x').pathname;
  if (pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const handbook = { rev: 3, sections: { current_state: 'Building **Alpha**.', open_backlog: '- Ship the page', conventions: '', gotchas: '', timeline: 'Started.' }, markdown: '', updated_at: '2026-02-03T02:00:00Z', updated_by: 'leader', line_count: 4, over_budget: false, nudge: '' };
const history = { revisions: [{ rev: 3, section: 'current_state', operation: 'replace', updated_at: handbook.updated_at, updated_by: 'leader', diff: '- Old\n+ Building Alpha' }], next_before_rev: 2 };
const older = { revisions: [{ rev: 2, section: 'timeline', operation: 'append', updated_at: '2026-02-02T02:00:00Z', updated_by: 'leader', diff: '+ Started.' }], next_before_rev: null };
const snapshot = { ...handbook, rev: 2, sections: { ...handbook.sections, current_state: 'Earlier state' }, over_budget: true, nudge: 'Condense older Timeline entries.' };

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  try {
    await page.goto(`http://127.0.0.1:${server.address().port}/fixture.html`);
    await page.evaluate(async responses => {
      const { setSession } = await import('/js/api.js');
      setSession({ csrf_token: 'token', id: 'usr_1' });
      window.__requests = [];
      window.fetch = (input, options = {}) => new Promise(resolve => {
        const request = { path: String(input), method: options.method || 'GET' };
        window.__requests.push(request);
        const response = responses[`${request.method} ${request.path}`];
        if (response) resolve(new Response(JSON.stringify(response), { status: 200, headers: { 'Content-Type': 'application/json' } }));
      });
      const { HandbookView } = await import('/js/pages/project/handbook-view.js');
      window.__handbook = new HandbookView(() => 'Alpha');
      window.__handbook.setRoom({ id: 'task', project_id: 'p1', kind: 'task' });
    }, {
      'GET /api/projects/p1/handbook': handbook,
      'GET /api/projects/p1/handbook/history?limit=20': history,
      'GET /api/projects/p1/handbook/history?limit=20&before_rev=2': older,
      'GET /api/projects/p1/handbook/history/2': snapshot,
    });
    assert.equal(await page.$eval('#room-handbook', node => node.hidden), true, 'task rooms have no project handbook button');
    await page.evaluate(() => window.__handbook.setRoom({ id: 'home', project_id: 'p1', kind: 'home' }));
    assert.deepEqual(await page.$eval('#room-handbook', node => [node.hidden, node.previousElementSibling.id]), [false, 'room-info'], 'the book button sits next to room settings on home');
    await page.$eval('#room-handbook', node => node.click());
    await page.waitForFunction(() => document.querySelector('#project-handbook .handbook-section strong')?.textContent === 'Alpha');
    const view = await page.evaluate(() => ({ title: document.querySelector('#project-handbook-title').textContent, sections: [...document.querySelectorAll('#project-handbook .handbook-section h4')].map(node => node.textContent), writes: document.querySelectorAll('#project-handbook :is(input, textarea, [contenteditable])').length, icon: getComputedStyle(document.querySelector('#room-handbook .icon-mask')).maskImage }));
    assert.deepEqual([view.title, view.sections, view.writes], ['Project handbook', ['Current state', 'Open backlog', 'Conventions', 'Gotchas', 'Timeline'], 0]);
    assert.match(view.icon, /book-open\.svg/);
    assert.match(await page.$eval('.handbook-meta', node => node.textContent), /Updated by leader · .* · Revision 3/);
    await page.$eval('#project-handbook .handbook-actions button', node => node.click());
    await page.waitForFunction(() => document.querySelectorAll('.handbook-revisions li').length === 1);
    await page.$eval('.handbook-revisions summary', node => node.click());
    assert.match(await page.$eval('.handbook-diff', node => node.textContent), /Building Alpha/);
    await page.$eval('.handbook-history > button', node => node.click());
    await page.waitForFunction(() => document.querySelectorAll('.handbook-revisions li').length === 2);
    await page.$eval('.handbook-revisions li:nth-child(2) button', node => node.click());
    await page.waitForFunction(() => document.querySelector('.handbook-section')?.textContent.includes('Earlier state'));
    assert.match(await page.$eval('.handbook-budget', node => node.textContent), /Condense older Timeline/);
    await page.$eval('.handbook-actions button', node => node.click());
    assert.match(await page.$eval('.handbook-section', node => node.textContent), /Building Alpha/);
    await page.setViewport({ width: 320, height: 640 });
    assert.deepEqual(await page.$eval('#handbook-dialog', node => { const rect = node.getBoundingClientRect(); return [Math.round(rect.width), Math.round(rect.height)]; }), [320, 640], 'the handbook fills a phone without clipping');
    await page.evaluate(() => window.__handbook.setRoom({ id: 'task', project_id: 'p1', kind: 'task' }));
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#handbook-dialog').open, document.querySelector('#room-handbook').hidden]), [false, true]);
    assert.deepEqual(await page.evaluate(() => window.__requests.map(request => [request.method, request.path])), [
      ['GET', '/api/projects/p1/handbook'], ['GET', '/api/projects/p1/handbook/history?limit=20'], ['GET', '/api/projects/p1/handbook/history?limit=20&before_rev=2'], ['GET', '/api/projects/p1/handbook/history/2'],
    ]);
    await page.evaluate(() => window.__handbook.destroy());
    assert.deepEqual(errors, []);
    console.log('handbook page tests passed');
  } finally { await page.close(); await browser.close(); server.close(); }
}
main().catch(error => { console.error(error); process.exit(1); });

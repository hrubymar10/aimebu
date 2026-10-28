// The Claude Code sessions import dialog in Chromium, over a source whose calls are held until the test lets them go. web/ is served as is
// with its import map; no Go server. Part of `make test-ui`.
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
<body><dialog id="cc-import-dialog" class="dialog-card cc-sessions" aria-labelledby="cc-import-title"></dialog></body></html>`;
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
  // `held` names the source calls to hold; the machine list answers the /api/machines request by rule.
  const mount = async ({ held = [], sessions = 3 } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async ({ held, sessions }) => {
      window.__calls = []; window.__waiting = {};
      window.fetch = async input => {
        window.__calls.push(['machines', String(input)]);
        return new Response(JSON.stringify({ machines: [{ id: 'm1', name: 'Box', is_host: true, approved: true, connected: true }, { id: 'm2', name: 'Away', approved: true, connected: false }, { id: 'm3', name: 'Gone', approved: true, revoked_at: 'x' }] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
      };
      const wrap = (name, run) => (...args) => {
        window.__calls.push([name, ...args.map(arg => (typeof arg === 'object' ? JSON.stringify(arg).slice(0, 60) : arg))]);
        if (!held.includes(name)) return Promise.resolve(run(...args));
        return new Promise((resolve, reject) => { (window.__waiting[name] ||= []).push(() => Promise.resolve(run(...args)).then(resolve, reject)); });
      };
      const list = Array.from({ length: sessions }, (_, index) => ({ id: `aaaaaaaa-0000-4000-8000-00000000000${index + 1}`, title: `Session ${index + 1}`, cwd: '/p', modified_at: '2026-10-24T10:00:00Z', size: 100 }));
      const source = {
        roles: wrap('roles', async () => [{ role_key: 'worker', label: 'Worker' }, { role_key: 'reviewer', label: 'Reviewer' }, { role_key: 'lead', label: 'Lead', cap_is_lead: true }]),
        catalog: wrap('catalog', async machine => ({ tools: [{ tool: 'claude', variant: 'native', launcher_id: `l_${machine}`, containerized: false, available: true, label: 'Claude Code', models: [{ id: 'opus', label: 'Opus' }, { id: 'haiku', label: 'Haiku', default: true }] }] })),
        claudeSessions: wrap('sessions', async () => ({ sessions: list })),
        spawn: wrap('spawn', async (room, params) => ({ slug: `copy-${params.resume_session.slice(-1)}` })),
      };
      window.__release = name => (window.__waiting[name] || []).shift()?.();
      const { ClaudeSessionImport } = await import('/js/pages/claude-import/page.js');
      window.__import = new ClaudeSessionImport(source);
    }, { held, sessions });
    return page;
  };
  const names = page => page.evaluate(() => window.__calls.map(call => call[0]));
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 60)));
  try {
    // The dialog opens on the host machine with the native harness, the default model and the sessions with preset roles.
    {
      const page = await mount();
      await page.evaluate(() => window.__import.open({ projectID: 'p1', homeRoomID: 'home' }));
      await page.waitForFunction(() => document.querySelectorAll('#cc-import-list .cc-import-row').length === 3);
      assert.deepEqual(await page.evaluate(() => ({ open: document.querySelector('#cc-import-dialog').open,
        machines: [...document.querySelectorAll('#cc-import-machine option')].map(o => [o.textContent, o.disabled]), machine: document.querySelector('#cc-import-machine').value,
        models: [...document.querySelectorAll('#cc-import-model option')].map(o => [o.value, o.selected]), roles: [...document.querySelectorAll('#cc-import-list select')].map(s => s.value),
        status: document.querySelector('#cc-import-status').textContent, run: [document.querySelector('#cc-import-run').disabled, document.querySelector('#cc-import-run').title], all: document.querySelector('#cc-import-all').title,
        order: [...document.querySelector('#cc-import-dialog').children].map(n => n.tagName.toLowerCase() + (n.id ? `#${n.id}` : '')) })),
        { open: true, machines: [['Box · Host', false], ['Away · Offline', true]], machine: 'm1', models: [['opus', false], ['haiku', true]], roles: ['worker', 'reviewer', 'reviewer'], status: '3 sessions',
          run: [true, 'Import the selected sessions as agents'], all: 'Select all or none', order: ['h2#cc-import-title', 'p', 'div', 'div', 'div#cc-import-list', 'p#cc-import-error', 'div'] });
      assert.equal((await page.evaluate(() => window.__calls.find(call => call[0] === 'machines')[1])), '/api/machines?project_id=p1');
      await page.close();
    }
    // Reads answered out of order: the newer one draws. Going to another harness or machine reads the sessions again.
    {
      const page = await mount({ held: ['sessions'] });
      await page.evaluate(() => { window.__import.open({ projectID: 'p1', homeRoomID: 'home' }); });
      await page.waitForFunction(() => window.__waiting.sessions?.length === 1);
      await page.evaluate(() => { window.__import.store.loadSessions(); });
      await page.waitForFunction(() => window.__waiting.sessions?.length === 2);
      await page.evaluate(() => { window.__release('sessions'); window.__release('sessions'); });
      await page.waitForFunction(() => document.querySelectorAll('#cc-import-list .cc-import-row').length === 3);
      await settle(page);
      assert.equal(await page.$$eval('#cc-import-list .cc-import-row', rows => rows.length), 3);
      await page.close();
    }
    // Starting: the role boxes are read when it starts, started rows lock, and Close and the boxes are off while it runs.
    {
      const page = await mount({ held: ['spawn'] });
      await page.evaluate(() => { window.__import.open({ projectID: 'p1', homeRoomID: 'home' }); });
      await page.waitForFunction(() => document.querySelectorAll('#cc-import-list .cc-import-row').length === 3);
      await page.$eval('#cc-import-all', el => el.click());
      await page.$eval('#cc-import-list select', el => { el.value = 'reviewer'; });
      await page.$eval('#cc-import-run', el => el.click());
      await page.waitForFunction(() => window.__waiting.spawn?.length === 1);
      assert.deepEqual(await page.evaluate(() => ({ run: document.querySelector('#cc-import-run').disabled, close: document.querySelector('#cc-import-close').disabled, boxes: [...document.querySelectorAll('#cc-import-list input, #cc-import-list select, #cc-import-machine, #cc-import-harness, #cc-import-model')].every(el => el.disabled),
        state: document.querySelector('.cc-import-state').textContent, call: window.__calls.find(call => call[0] === 'spawn') })),
        { run: true, close: true, boxes: true, state: 'Starting…', call: ['spawn', 'home', JSON.stringify({ role: 'reviewer', tool: 'claude', variant: 'native', launcher_id: 'l_m1', model: 'haiku', machine_id: 'm1', resume_session: 'aaaaaaaa-0000-4000-8000-000000000001', resume_mode: 'fork', new_room: true }).slice(0, 60)] });
      for (let i = 0; i < 3; i++) { await page.evaluate(() => window.__release('spawn')); await page.waitForFunction(count => window.__calls.filter(call => call[0] === 'spawn').length > count || /started/.test(document.querySelector('#cc-import-status').textContent), {}, i); }
      await page.waitForFunction(() => /3 of 3 sessions started\./.test(document.querySelector('#cc-import-status').textContent));
      assert.deepEqual(await page.evaluate(() => [[...document.querySelectorAll('.cc-import-state')].map(s => s.textContent), [...document.querySelectorAll('#cc-import-list input')].map(i => i.disabled), document.querySelector('#cc-import-close').disabled]),
        [['Started as copy-1', 'Started as copy-2', 'Started as copy-3'], [true, true, true], false]);
      await page.close();
    }
    // A page destroyed while a read is held sends nothing more; one destroyed between two starts sends no more agents.
    {
      const page = await mount({ held: ['roles'] });
      await page.evaluate(() => { window.__import.open({ projectID: 'p1', homeRoomID: 'home' }); });
      await page.waitForFunction(() => window.__waiting.roles?.length === 1);
      await page.evaluate(() => { window.__import.destroy(); window.__release('roles'); });
      await settle(page);
      assert.deepEqual(await names(page), ['machines', 'roles'], 'no catalog read after destroy');
      await page.evaluate(() => { window.__import.open({ projectID: 'p1', homeRoomID: 'home' }); window.__import.store.loadCatalog(); window.__import.store.loadSessions(); });
      await settle(page);
      assert.deepEqual(await names(page), ['machines', 'roles'], 'a destroyed dialog reads nothing');
      await page.close();
      const spawning = await mount({ held: ['spawn'] });
      await spawning.evaluate(() => { window.__import.open({ projectID: 'p1', homeRoomID: 'home' }); });
      await spawning.waitForFunction(() => document.querySelectorAll('#cc-import-list .cc-import-row').length === 3);
      await spawning.$eval('#cc-import-all', el => el.click());
      await spawning.$eval('#cc-import-run', el => el.click());
      await spawning.waitForFunction(() => window.__waiting.spawn?.length === 1);
      await spawning.evaluate(() => { window.__import.destroy(); window.__release('spawn'); });
      await settle(spawning);
      assert.equal((await names(spawning)).filter(name => name === 'spawn').length, 1, 'no further agent is started once the dialog is gone');
      await spawning.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('claude import page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

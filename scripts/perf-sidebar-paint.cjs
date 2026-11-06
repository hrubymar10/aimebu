// Measures when the room list first paints and when it is complete, on a scratch server with 12 projects of 8 rooms, with 100 ms added to every
// request (Chrome DevTools Protocol network emulation) and a warm cache. Not part of `make test-ui`: it reports numbers, it asserts nothing.
//   node scripts/perf-sidebar-paint.cjs [runs]        (default 7)
// first = the first row or placeholder of the list is in the page; rooms = the first project drawn with its own rooms; complete = every project shows its own rooms (no placeholder left).
'use strict';
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { scratch } = require('./scratch-server.cjs');

const PROJECTS = 12, ROOMS = 7, LATENCY_MS = 100;
const runs = Number(process.argv[2]) || 7;
const median = values => [...values].sort((a, b) => a - b)[Math.floor(values.length / 2)];

async function main() {
  const server = await scratch();
  const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'aimebu-perf-'));
  try {
    const call = await server.ownerClient();
    for (let i = 0; i < PROJECTS; i++) {
      const folder = path.join(root, `p${i}`); fs.mkdirSync(folder); execFileSync('git', ['init', '-q'], { cwd: folder });
      const project = (await call('/api/projects', { method: 'POST', body: { name: `Project ${String(i + 1).padStart(2, '0')}`, folder } })).body;
      for (let r = 0; r < ROOMS; r++) await call(`/api/projects/${project.id}/rooms`, { method: 'POST', body: { title: `room-${r + 1}` } });
    }
    const context = await server.context();
    const page = await server.open({ width: 1280, height: 900, context });
    await page.goto(server.origin);
    await page.waitForSelector('#login-view:not([hidden])');
    await server.signIn(page);
    await page.evaluateOnNewDocument(() => {
      // The first sign-in's welcome is already dismissed by the owner's setup; only the list is timed.
      window.__paint = { first: 0, rooms: 0, complete: 0 };
      const look = () => {
        const list = document.querySelector('#projects');
        if (!list) return;
        const groups = list.querySelectorAll(':scope > .room-group');
        if (groups.length && !window.__paint.first) window.__paint.first = performance.now();
        if (list.querySelector(':scope > .room-group:not(.project-placeholder)') && !window.__paint.rooms) window.__paint.rooms = performance.now();
        if (groups.length >= 12 && !list.querySelector('.project-placeholder') && !window.__paint.complete) window.__paint.complete = performance.now();
      };
      new MutationObserver(look).observe(document, { childList: true, subtree: true });
    });
    // The owner's first sign-in shows the welcome once: dismiss it before anything is timed.
    await page.goto(server.origin);
    await page.waitForSelector('#welcome-continue');
    await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue');
    await page.waitForFunction(() => document.querySelectorAll('#projects > .room-group').length > 0);
    const client = await page.createCDPSession();
    await client.send('Network.enable');
    await client.send('Network.emulateNetworkConditions', { offline: false, latency: LATENCY_MS, downloadThroughput: -1, uploadThroughput: -1 });
    const results = [];
    for (let run = -1; run < runs; run++) { // the first load warms the cache and is not counted
      await page.goto(server.origin, { waitUntil: 'load' });
      await page.waitForFunction(() => window.__paint.complete > 0, { timeout: 60000 }).catch(async error => { throw new Error(`${error.message}\n${JSON.stringify(await page.evaluate(() => ({ paint: window.__paint, welcome: !!document.querySelector('#welcome-continue'), app: document.querySelector('#app-view')?.hidden, rows: document.querySelectorAll('#projects > .room-group').length, text: document.body.innerText.slice(0, 200) })))}`); });
      const paint = await page.evaluate(() => ({ ...window.__paint, rows: document.querySelectorAll('#projects > .room-group').length }));
      if (run >= 0) results.push(paint);
    }
    const first = results.map(r => Math.round(r.first)), rooms = results.map(r => Math.round(r.rooms)), complete = results.map(r => Math.round(r.complete));
    console.log(`projects=${PROJECTS} rooms=${ROOMS + 1} latency=${LATENCY_MS}ms runs=${runs}`);
    console.log(`first paint of the list: median ${median(first)} ms  (${first.join(', ')})`);
    console.log(`first project with its rooms: median ${median(rooms)} ms  (${rooms.join(', ')})`);
    console.log(`complete list:           median ${median(complete)} ms  (${complete.join(', ')})`);
  } finally { await server.stop(); fs.rmSync(root, { recursive: true, force: true }); }
}
main().catch(error => { console.error(error); process.exit(1); });

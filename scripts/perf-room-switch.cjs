// Measures how long opening a busy room takes, for several first-page sizes, on a scratch server with the same data each time. The page asks for
// `size` newest messages (window.__openPageSize); the time runs from the click on the room until its newest message is on screen and painted. Not part
// of `make test-ui`: it reports numbers, it asserts nothing.
//   node scripts/perf-room-switch.cjs [runs] [sizes, comma separated]        (default 7 runs, sizes 100,40,25)
//   LATENCY_MS=50 CPU_RATE=4 change the emulation.
// Chrome DevTools Protocol: 4x CPU slowdown and 50 ms of latency on every request by default, so the difference between the sizes is not lost in the noise.
'use strict';
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { scratch } = require('./scratch-server.cjs');

const MESSAGES = 400;
const LATENCY = Number(process.env.LATENCY_MS ?? 50), CPU = Number(process.env.CPU_RATE || 4);
const runs = Number(process.argv[2]) || 7;
const sizes = (process.argv[3] || '100,40,25').split(',').map(Number);
const median = values => [...values].sort((a, b) => a - b)[Math.floor(values.length / 2)];

async function main() {
  const server = await scratch();
  const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'aimebu-perf-room-'));
  try {
    const call = await server.ownerClient();
    execFileSync('git', ['init', '-q'], { cwd: root });
    const project = (await call('/api/projects', { method: 'POST', body: { name: 'Busy', folder: root } })).body;
    const busy = (await call(`/api/projects/${project.id}/rooms`, { method: 'POST', body: { title: 'busy' } })).body;
    const quiet = (await call(`/api/projects/${project.id}/rooms`, { method: 'POST', body: { title: 'quiet' } })).body;
    await call(`/api/rooms/${quiet.id}/messages`, { method: 'POST', body: { body: 'hello' } });
    for (let i = 0; i < MESSAGES; i++) {
      const body = i % 7 === 0 ? `### Update ${i}\n\n| a | b |\n|---|---|\n| ${i} | **bold** |\n\n\`\`\`js\nconst n = ${i};\n\`\`\`\n` : `message ${i} with **markdown**, \`code\` and a [link](https://example.com/${i}) ${'text '.repeat(i % 40)}`;
      await call(`/api/rooms/${busy.id}/messages`, { method: 'POST', body: { body } });
    }
    const results = {};
    await call(`/api/rooms/${busy.id}/messages`, { method: 'POST', body: { body: 'the newest message of the busy room' } });
    for (const size of sizes) {
      const page = await server.open({ width: 1280, height: 900, context: await server.context() }); // a fresh browser context: signed out, empty caches
      await page.evaluateOnNewDocument(size => { window.__openPageSize = size; }, size);
      await page.goto(server.origin);
      await page.waitForSelector('#login-view:not([hidden])');
      await server.signIn(page);
      await page.goto(server.origin);
      await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
      if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
      await page.waitForSelector('#app-view:not([hidden])');
      await page.waitForFunction(() => document.querySelectorAll('.room-item').length >= 2);
      const client = await page.createCDPSession();
      await client.send('Network.enable');
      await client.send('Network.emulateNetworkConditions', { offline: false, latency: LATENCY, downloadThroughput: -1, uploadThroughput: -1 });
      await client.send('Emulation.setCPUThrottlingRate', { rate: CPU });
      // From the click until `marker` is the text of the newest row on screen and a frame has been painted after it.
      const open = (title, marker) => page.evaluate((title, marker) => new Promise(resolve => {
        const row = [...document.querySelectorAll('.room-item')].find(item => item.textContent.startsWith(`#${title}`));
        const start = performance.now();
        row.click();
        const poll = () => {
          if (performance.now() - start > 20000) { resolve({ ms: -1, rows: 0, dump: [document.querySelector('#chat-title')?.textContent, [...document.querySelectorAll('#messages > article.chat-msg')].slice(-2).map(node => node.textContent.slice(0, 80))] }); return; }
          const all = document.querySelectorAll('#messages > article.chat-msg'), last = all[all.length - 1];
          if (last && last.textContent.includes(marker)) requestAnimationFrame(() => requestAnimationFrame(() => resolve({ ms: performance.now() - start, rows: document.querySelectorAll('#messages > article.chat-msg').length })));
          else setTimeout(poll, 2);
        };
        poll();
      }), title, marker);
      const times = [], rows = [];
      for (let run = -1; run < runs; run++) { // the first switch warms the page and is not counted
        await open('quiet', 'hello'); await new Promise(resolve => setTimeout(resolve, 400));
        const result = await open('busy', 'the newest message of the busy room');
        if (result.dump) throw new Error(`the busy room's newest message never appeared: ${JSON.stringify(result.dump)}`);
        if (run >= 0) { times.push(Math.round(result.ms)); rows.push(result.rows); }
        await new Promise(resolve => setTimeout(resolve, 300));
      }
      results[size] = { median: median(times), times, rows: median(rows) };
      await page.close();
    }
    console.log(`busy room: ${MESSAGES} messages, ${CPU}x CPU slowdown, ${LATENCY} ms latency, ${runs} runs each`);
    for (const size of sizes) console.log(`first page ${String(size).padStart(3)} messages: median ${results[size].median} ms to the newest message painted  (${results[size].times.join(', ')}; ${results[size].rows} rows drawn)`);
  } finally { await server.stop(); fs.rmSync(root, { recursive: true, force: true }); }
}
main().catch(error => { console.error(error); process.exit(1); });

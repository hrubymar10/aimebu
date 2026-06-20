#!/usr/bin/env node
// smoke-render.js — headless render-lock for the aimebu frontend.
//
// Launches a minimal stub HTTP server that serves the frontend files
// and mocks the bus API, then loads the page in Chromium via Puppeteer.
// Fails if:
//   - any unhandled JavaScript errors are thrown on page load
//   - the root app element is missing after load
//   - visual_plan block renderers produce unexpected fallback output or
//     are missing expected DOM landmarks for known block types
//
// Run: node scripts/smoke-render.js
// Or:  make smoke-render
//
// Requirements: chromium + puppeteer-core installed globally.
//   which chromium && npm ls -g puppeteer-core
'use strict';

const http = require('http');
const fs = require('fs');
const path = require('path');

const FRONTEND_DIR = path.join(__dirname, '..', 'frontend');
const PORT = 19997;

// Minimal stub responses for every API path the frontend calls on load.
const STUBS = {
  'GET /rooms/_system/messages': { messages: [] },
  'GET /buildinfo': { version: 'smoke-test', go_version: 'go0.0.0' },
  'GET /settings': {},
  'GET /sounds': { sounds: [] },
  'GET /rooms': { rooms: [] },
  'GET /agents': { agents: [] },
  'GET /macros': { macros: {} },
  'GET /fleets': { fleets: {} },
  'GET /roles': { roles: [] },
  'GET /settings/prompts': { prompts: [] },
};

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js':   'application/javascript; charset=utf-8',
  '.css':  'text/css; charset=utf-8',
  '.json': 'application/json',
  '.webmanifest': 'application/manifest+json',
};

function serveStatic(req, res) {
  const urlPath = req.url.split('?')[0];
  const filePath = urlPath === '/' ? path.join(FRONTEND_DIR, 'index.html')
                                   : path.join(FRONTEND_DIR, urlPath.replace(/^\//, ''));
  if (!filePath.startsWith(FRONTEND_DIR)) {
    res.writeHead(403);
    return res.end('forbidden');
  }
  const ext = path.extname(filePath);
  try {
    const data = fs.readFileSync(filePath);
    res.writeHead(200, { 'Content-Type': MIME[ext] || 'application/octet-stream' });
    res.end(data);
  } catch (_) {
    res.writeHead(404);
    res.end('not found');
  }
}

const server = http.createServer(function (req, res) {
  const key = req.method + ' ' + req.url.split('?')[0];
  // API stubs
  for (const stub of Object.keys(STUBS)) {
    if (key === stub || key.startsWith(stub + '?')) {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      return res.end(JSON.stringify(STUBS[stub]));
    }
  }
  // SSE endpoints — keep the connection open and silent (no events needed for smoke test)
  if (req.headers['accept'] === 'text/event-stream' || req.url.includes('/events') || req.url.includes('/sse')) {
    res.writeHead(200, {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache',
      'Connection': 'keep-alive',
    });
    res.write(': ok\n\n');
    req.on('close', function () { res.end(); });
    return;
  }
  // Anything under /api or /rooms or /agents that isn't stubbed
  if (req.url.startsWith('/api/') || req.url.startsWith('/rooms') ||
      req.url.startsWith('/agents') || req.url.startsWith('/memory') ||
      req.url.startsWith('/sounds') || req.url.startsWith('/macros') ||
      req.url.startsWith('/fleets') || req.url.startsWith('/roles') ||
      req.url.startsWith('/settings') || req.url.startsWith('/buildinfo') ||
      req.url.startsWith('/ws') || req.url.startsWith('/events')) {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    return res.end('{}');
  }
  serveStatic(req, res);
});

// Fixture blocks covering each known visual_plan block type.
// Each entry specifies: the block object, and assertions to run on the
// rendered HTML (as CSS selectors that must be present / absent).
const FIXTURE_BLOCKS = [
  {
    block: { type: 'markdown', title: 'MD block', data: { markdown: '**hello world**' } },
    mustHave: ['.visual-plan-block'],
    mustNotHave: ['.visual-plan-raw-fallback'],
  },
  {
    block: { type: 'file-tree', title: 'Files', data: { nodes: [{ name: 'src', type: 'dir' }, { name: 'main.go', type: 'file', parent: 'src' }] } },
    mustHave: ['.visual-plan-block', '.visual-plan-file-tree'],
    mustNotHave: ['.visual-plan-raw-fallback'],
  },
  {
    block: { type: 'diagram', title: 'Diagram', data: { mermaid: 'graph TD\n  A --> B' } },
    mustHave: ['.visual-plan-block', '.visual-plan-mermaid'],
    mustNotHave: ['.visual-plan-raw-fallback'],
  },
  {
    block: { type: 'checklist', title: 'Steps', data: { items: [{ checked: false, text: 'Step 1' }, { checked: true, text: 'Done' }] } },
    mustHave: ['.visual-plan-block', '.visual-plan-checklist'],
    mustNotHave: ['.visual-plan-raw-fallback'],
  },
  {
    block: { type: 'diff', title: 'Patch', data: { diff: '-old line\n+new line' } },
    mustHave: ['.visual-plan-block', '.visual-plan-diff'],
    mustNotHave: ['.visual-plan-raw-fallback'],
  },
  {
    block: { type: 'annotated-code', title: 'Code', data: { code: 'func main() {}', language: 'go', annotations: [{ line: 1, note: 'entry' }] } },
    mustHave: ['.visual-plan-block', '.visual-plan-code-block'],
    mustNotHave: ['.visual-plan-raw-fallback'],
  },
  // Unknown future block type must degrade to raw fallback — not a crash.
  {
    block: { type: 'future-block-xyzzy', title: 'Unknown', data: { value: 42 } },
    mustHave: ['.visual-plan-block', '.visual-plan-raw-fallback'],
    mustNotHave: [],
  },
];

server.listen(PORT, '127.0.0.1', async function () {
  const url = 'http://127.0.0.1:' + PORT + '/';
  let puppeteer;
  try {
    puppeteer = require('puppeteer-core');
  } catch (_) {
    try {
      puppeteer = require('/usr/local/lib/node_modules/puppeteer-core');
    } catch (e) {
      console.error('puppeteer-core not found:', e.message);
      server.close();
      process.exit(2);
    }
  }

  const jsErrors = [];
  let browser;
  try {
    browser = await puppeteer.launch({
      executablePath: '/usr/bin/chromium',
      headless: true,
      args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'],
    });
    const page = await browser.newPage();
    page.on('pageerror', function (err) {
      jsErrors.push(err.message);
    });
    page.on('console', function (msg) {
      if (msg.type() === 'error') {
        // Ignore network errors from un-stubbed routes (expected in smoke mode)
        if (!msg.text().includes('Failed to load resource') &&
            !msg.text().includes('ERR_') &&
            !msg.text().includes('net::')) {
          jsErrors.push('console.error: ' + msg.text());
        }
      }
    });

    await page.goto(url, { waitUntil: 'networkidle2', timeout: 15000 });

    // Wait briefly for any deferred JS to execute
    await new Promise(function (r) { setTimeout(r, 500); });

    // Verify the root app element is present (not a blank page)
    const appEl = await page.$('#app, .app, #room-list, .room-list');
    if (!appEl) {
      jsErrors.push('Root app element (#app / #room-list / .app) not found after load');
    }

    // --- visual_plan renderer fixture tests ---
    // visualPlanHTML is a global from render-visual-plan.js (loaded before app.js).
    // We call it directly, inject the HTML, and assert DOM landmarks.
    for (let i = 0; i < FIXTURE_BLOCKS.length; i++) {
      const fixture = FIXTURE_BLOCKS[i];
      const blockType = fixture.block.type;

      const result = await page.evaluate(function (block, idx) {
        // Build a minimal message object with one visual_plan block.
        var message = { id: idx + 1000, visual_plan: [block] };
        // visualPlanHTML is defined globally in render-visual-plan.js.
        if (typeof visualPlanHTML !== 'function') {
          return { error: 'visualPlanHTML is not defined as a global function' };
        }
        var html = visualPlanHTML(message);
        // Parse into a detached element for querying.
        var container = document.createElement('div');
        container.innerHTML = html;
        return { html: container.innerHTML, outerHTML: container.outerHTML };
      }, fixture.block, i);

      if (result.error) {
        jsErrors.push('visual_plan[' + blockType + ']: ' + result.error);
        continue;
      }

      // Run selector assertions in-page using the rendered HTML.
      const assertResult = await page.evaluate(function (renderedHTML, mustHave, mustNotHave) {
        var container = document.createElement('div');
        container.innerHTML = renderedHTML;
        var failures = [];
        for (var j = 0; j < mustHave.length; j++) {
          if (!container.querySelector(mustHave[j])) {
            failures.push('missing ' + mustHave[j]);
          }
        }
        for (var k = 0; k < mustNotHave.length; k++) {
          if (container.querySelector(mustNotHave[k])) {
            failures.push('unexpected ' + mustNotHave[k]);
          }
        }
        return failures;
      }, result.html, fixture.mustHave, fixture.mustNotHave);

      for (const failure of assertResult) {
        jsErrors.push('visual_plan block type=' + blockType + ': ' + failure);
      }
    }
  } finally {
    if (browser) await browser.close();
    server.close();
  }

  if (jsErrors.length) {
    console.error('FAIL — errors during render:');
    jsErrors.forEach(function (e) { console.error('  ' + e); });
    process.exit(1);
  }
  console.log('PASS — page rendered without errors; ' + FIXTURE_BLOCKS.length + ' visual_plan block types verified');
  process.exit(0);
});

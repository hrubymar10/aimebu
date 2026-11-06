// Settings → Macros in Chromium, through its page slot, with every request held until the test answers it, so the order of answers is
// chosen. web/ is served as is with its import map; no Go server. Part of `make test-ui`.
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
<body><dialog id="settings-dialog" open><nav></nav></dialog></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const START = { greet: 'Hello', bye: 'Goodbye' };
const READ = 'GET /api/macros';

// fetch is held: each request waits for __answer(index, status, body) unless an `auto` rule answers it.
function installFetch(auto) {
  window.__requests = []; window.__auto = auto;
  window.fetch = (input, options = {}) => new Promise(resolve => {
    const request = { path: String(input), method: options.method || 'GET', body: options.body ? JSON.parse(options.body) : undefined, answered: false };
    request.answer = (status, body) => { request.answered = true; resolve(new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })); };
    window.__requests.push(request);
    const rule = window.__auto[`${request.method} ${request.path}`];
    if (rule) request.answer(200, rule);
  });
  window.__answer = (index, status, body) => window.__requests[index].answer(status, body);
}

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  const mount = async ({ admin = true, auto = { [READ]: START } } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, auto);
    await page.evaluate(async admin => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_1', owner: admin, admin });
      const { MacroSettings } = await import('/js/pages/macros/page.js');
      window.__page = new MacroSettings(document.querySelector('#settings-dialog'));
      window.__page.section.hidden = false; window.__page.open();
    }, admin);
    await page.waitForFunction(() => document.querySelector('#macros-list'));
    if (auto[READ]) await page.waitForFunction(() => document.querySelector('#macros-list .snippet-row, #macros-list p'));
    return page;
  };
  const pending = async (page, method, requestPath = '/api/macros') => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && request.method === method && request.path === requestPath), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findIndex(request => !request.answered && request.method === method && request.path === requestPath), method, requestPath);
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const hold = page => page.evaluate(() => { delete window.__auto['GET /api/macros']; });
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50)));
  const type = (page, selector, value) => page.$eval(selector, (el, value) => { el.value = value; el.dispatchEvent(new Event('input', { bubbles: true })); }, value);
  const click = (page, selector) => page.$eval(selector, el => el.click());
  const count = (page, method) => page.evaluate(method => window.__requests.filter(request => request.method === method).length, method);
  const body = (page, index) => page.evaluate(index => window.__requests[index].body, index);
  const shown = page => page.evaluate(() => ({
    keys: [...document.querySelectorAll('#macros-list .snippet-row strong')].map(el => el.textContent),
    list: document.querySelector('#macros-list').textContent, error: document.querySelector('#settings-macros .error').textContent,
    key: document.querySelector('#macro-editor [name=key]').value, body: document.querySelector('#macro-editor [name=body]').value,
  }));
  const locks = page => page.evaluate(() => {
    const off = selector => [...document.querySelectorAll(selector)].every(el => el.disabled);
    return {
      form: off('#macro-editor input, #macro-editor textarea'), save: off('#macro-editor [type=submit]'), rows: off('#macros-list button'),
      importText: off('#macros-import textarea'), preview: off('#macros-import-preview'), apply: off('#macros-import-apply'),
    };
  });
  const OPEN = { form: false, save: false, rows: false, importText: false, preview: false, apply: true };
  const LOCKED = { form: true, save: true, rows: true, importText: true, preview: true, apply: true };
  const saveForm = async (page, key, text) => { await type(page, '#macro-editor [name=key]', key); await type(page, '#macro-editor [name=body]', text); await click(page, '#macro-editor [type=submit]'); };
  const openImport = async (page, text) => { await click(page, '#macros-import-open'); await type(page, '#macros-import-text', text); await click(page, '#macros-import-preview'); };
  try {
    // Two reads answered out of order: only the newer one draws, and the older one's failure says nothing.
    {
      const page = await mount();
      await hold(page);
      await page.evaluate(() => window.__page.open()); const first = await pending(page, 'GET');
      await page.evaluate(() => window.__page.open());
      await page.waitForFunction(() => window.__requests.filter(request => !request.answered).length === 2);
      const second = await page.evaluate(() => window.__requests.findLastIndex(request => !request.answered));
      assert.deepEqual(await locks(page), { ...OPEN, save: true, rows: true, preview: true }, 'nothing writes while a read is held, though the form can be typed in');
      await answer(page, second, 200, { newer: 'N' });
      await page.waitForFunction(() => document.querySelector('#macros-list').textContent.includes('<NEWER>'));
      await answer(page, first, 500, { message: 'stale failure' });
      await settle(page);
      assert.deepEqual([(await shown(page)).keys, (await shown(page)).error], [['<NEWER>'], ''], 'only the newest read draws');
      await page.close();
    }
    // A failed read locks every write. The legacy page shows "No macros yet." and its next Save replaces the stored map with one macro.
    {
      const page = await mount({ auto: {} });
      await answer(page, await pending(page, 'GET'), 500, { message: 'read failed' });
      await page.waitForFunction(() => document.querySelector('#settings-macros .error').textContent === 'read failed');
      await saveForm(page, 'lonely', 'x');
      await page.$eval('#macro-editor', form => form.requestSubmit());
      await click(page, '#macros-import-open'); await type(page, '#macros-import-text', '{"a":"b"}'); await click(page, '#macros-import-preview');
      await settle(page);
      assert.deepEqual([await count(page, 'PUT'), (await shown(page)).list, await locks(page)], [0, '', { ...OPEN, save: true, rows: true, preview: true }], 'nothing is sent after a failed read');
      await page.evaluate(start => { window.__auto['GET /api/macros'] = start; window.__page.open(); }, START);
      await page.waitForFunction(() => !document.querySelector('#macro-editor [type=submit]').disabled);
      assert.deepEqual([(await shown(page)).keys, (await shown(page)).key], [['<BYE>', '<GREET>'], ''], 'a reopen reads again, and drops the form as every open does');
      await page.close();
    }
    // One write at a time: while a delete is held every write control is locked and a second delete sends nothing; the map drawn after
    // it is the server's answer.
    {
      const page = await mount();
      await openImport(page, '{"alpha":"A"}');
      await click(page, '#macros-list .snippet-row:first-child button.btn-danger');
      const put = await pending(page, 'PUT');
      assert.deepEqual(await body(page, put), { greet: 'Hello' });
      assert.deepEqual(await locks(page), LOCKED, 'everything that writes or feeds a write is locked');
      await page.$$eval('#macros-list button.btn-danger', buttons => buttons.forEach(button => button.click()));
      await page.$eval('#macro-editor', form => form.requestSubmit());
      await settle(page);
      assert.equal(await count(page, 'PUT'), 1, 'a second write is not sent while one is held');
      await answer(page, put, 200, { greet: 'Hello', fromserver: 'S' });
      await page.waitForFunction(() => !document.querySelector('#macro-editor [type=submit]').disabled);
      assert.deepEqual((await shown(page)).keys, ['<FROMSERVER>', '<GREET>']);
      assert.deepEqual(await page.$eval('#macros-import-summary', el => el.textContent), '', 'a preview computed against the old map is dropped');
      assert.deepEqual(await locks(page), OPEN);
      await page.close();
    }
    // A failed save keeps the form, the same input node and the message; edit and rename send one map without the old key.
    {
      const page = await mount();
      const box = await page.$('#macro-editor [name=body]');
      await saveForm(page, 'new', 'KEEP ME');
      await answer(page, await pending(page, 'PUT'), 500, { message: 'nope' });
      await page.waitForFunction(() => document.querySelector('#settings-macros .error').textContent === 'nope');
      assert.deepEqual([await page.evaluate(el => el === document.querySelector('#macro-editor [name=body]'), box), (await shown(page)).key, (await shown(page)).body, await page.evaluate(() => window.__page.hasUnsavedChanges())], [true, 'new', 'KEEP ME', true]);
      await click(page, '#macros-list .snippet-row:last-child .btn-icon:not(.btn-danger)');
      assert.deepEqual(await page.evaluate(() => [document.activeElement.name, document.querySelector('#macro-editor [type=button]').hidden]), ['key', false], 'Edit fills the form, focuses the key and shows Cancel');
      await type(page, '#macro-editor [name=key]', 'hi');
      await click(page, '#macro-editor [type=submit]');
      const put = await pending(page, 'PUT');
      assert.deepEqual(await body(page, put), { bye: 'Goodbye', hi: 'Hello' });
      await answer(page, put, 200, { bye: 'Goodbye', hi: 'Hello' });
      await page.waitForFunction(() => document.querySelector('#macro-editor [name=key]').value === '');
      assert.deepEqual([(await shown(page)).keys, await page.$eval('#macro-editor [type=button]', el => el.hidden), await page.evaluate(() => window.__page.hasUnsavedChanges())], [['<BYE>', '<HI>'], true, false]);
      await saveForm(page, 'bye', 'dup');
      assert.deepEqual([(await shown(page)).error, await count(page, 'PUT')], ['A macro with that key already exists.', 2], 'a local refusal sends nothing');
      await page.close();
    }
    // Import: a failed apply keeps the panel, the text and the preview, so Apply can be tried again; typing drops the preview; a success
    // closes the panel.
    {
      const page = await mount();
      await openImport(page, '{"alpha":"A","greet":"Changed"}');
      assert.match(await page.$eval('#macros-import-summary', el => el.textContent), /Will add 1 new and overwrite 1 existing/);
      await click(page, '#macros-import-apply');
      const put = await pending(page, 'PUT');
      assert.deepEqual(await body(page, put), { greet: 'Changed', bye: 'Goodbye', alpha: 'A' });
      await answer(page, put, 400, { message: 'too big' });
      await page.waitForFunction(() => document.querySelector('#macros-import-summary').textContent === 'too big');
      assert.deepEqual([await page.$eval('#macros-import', el => el.hidden), await page.$eval('#macros-import-summary', el => el.className), await page.$eval('#macros-import-text', el => el.value), (await locks(page)).apply], [false, 'macro-import-summary error', '{"alpha":"A","greet":"Changed"}', false]);
      await type(page, '#macros-import-text', '{"alpha":"A2"}');
      assert.deepEqual([await page.$eval('#macros-import-summary', el => el.textContent), (await locks(page)).apply], ['', true], 'editing the text drops the preview');
      await click(page, '#macros-import-preview');
      await click(page, '#macros-import-apply');
      const second = await pending(page, 'PUT');
      await answer(page, second, 200, await body(page, second));
      await page.waitForFunction(() => document.querySelector('#macros-import').hidden);
      assert.deepEqual([(await shown(page)).keys, await page.$eval('#macros-import-text', el => el.value), await page.$eval('#macros-import-open', el => el.hidden)], [['<ALPHA>', '<BYE>', '<GREET>'], '', false]);
      await page.close();
    }
    // A save between Preview and Apply drops the preview, so Apply cannot send a merge computed against the older map.
    {
      const page = await mount();
      await openImport(page, '{"alpha":"A"}');
      await saveForm(page, 'zed', 'Z');
      const put = await pending(page, 'PUT');
      await answer(page, put, 200, await body(page, put));
      await page.waitForFunction(() => document.querySelector('#macros-list').textContent.includes('<ZED>'));
      assert.deepEqual([(await locks(page)).apply, await page.$eval('#macros-import-summary', el => el.textContent)], [true, '']);
      await page.close();
    }
    // A reopen while a write is held: the write keeps the page locked until it settles, its answer reports nothing, and the map is read
    // again after it, because the reopen's read may have been answered with the map from before the write. The same after a failure.
    for (const outcome of ['success', 'failure']) {
      const page = await mount();
      await saveForm(page, 'held', 'H');
      const put = await pending(page, 'PUT');
      await hold(page);
      await page.evaluate(() => window.__page.open());
      const reopenRead = await pending(page, 'GET');
      await answer(page, reopenRead, 200, START);
      await settle(page);
      assert.deepEqual(await locks(page), LOCKED, `the reopen does not release the held write's lock (${outcome})`);
      if (outcome === 'success') await answer(page, put, 200, { ...START, held: 'H' }); else await answer(page, put, 500, { message: 'late failure' });
      const after = await pending(page, 'GET');
      assert.deepEqual(await locks(page), LOCKED, `still locked until the read after the write answers (${outcome})`);
      await answer(page, after, 200, outcome === 'success' ? { ...START, held: 'H' } : START);
      await page.waitForFunction(() => !document.querySelector('#macro-editor [type=submit]').disabled);
      assert.deepEqual([(await shown(page)).keys.includes('<HELD>'), (await shown(page)).error, await locks(page)], [outcome === 'success', '', OPEN], `the fresh map draws, the obsolete write reports nothing, and the page unlocks (${outcome})`);
      await page.close();
    }
    // Hide does nothing to a write in flight (its answer draws into the hidden page) nor to drafts. An open drops the form and the import
    // panel when its read is asked for, not when it answers: text typed while the read is held stays.
    {
      const page = await mount();
      await saveForm(page, 'hidden', 'H');
      const put = await pending(page, 'PUT');
      await page.evaluate(() => { window.__page.section.hidden = true; });
      await answer(page, put, 200, { ...START, hidden: 'H' });
      await page.waitForFunction(() => document.querySelector('#macros-list').textContent.includes('<HIDDEN>') && document.querySelector('#macro-editor [name=key]').value === '');
      await type(page, '#macro-editor [name=key]', 'before'); await openImport(page, '{"x":"y"}');
      await page.evaluate(() => { window.__page.section.hidden = true; });
      await page.evaluate(() => { window.__page.section.hidden = false; });
      assert.deepEqual([(await shown(page)).key, await page.$eval('#macros-import-text', el => el.value)], ['before', '{"x":"y"}'], 'hide keeps the drafts');
      await hold(page);
      await page.evaluate(() => window.__page.open());
      const held = await pending(page, 'GET');
      assert.deepEqual([(await shown(page)).key, await page.$eval('#macros-import', el => el.hidden), await page.$eval('#macros-import-text', el => el.value), await page.evaluate(() => window.__page.hasUnsavedChanges())], ['', true, '', false], 'an open drops the drafts when it asks for the read');
      await type(page, '#macro-editor [name=key]', 'typed while loading');
      await answer(page, held, 200, START);
      await page.waitForFunction(() => !document.querySelector('#macro-editor [type=submit]').disabled);
      assert.deepEqual([(await shown(page)).key, await page.evaluate(() => window.__page.hasUnsavedChanges())], ['typed while loading', true], 'text typed during the read is kept');
      await page.close();
    }
    // Clear during a held write and destroy during a held read: their answers draw and start nothing; a cleared page opens fresh.
    {
      const page = await mount();
      await saveForm(page, 'cleared', 'C');
      const put = await pending(page, 'PUT');
      await page.evaluate(() => window.__page.clear());
      const made = await page.evaluate(() => window.__requests.length);
      await answer(page, put, 200, { cleared: 'C' });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#settings-macros').childElementCount, window.__requests.length]), [0, made], 'a cleared page stays empty');
      await page.evaluate(() => window.__page.open());
      await page.waitForFunction(() => document.querySelectorAll('#macros-list .snippet-row').length === 2);
      assert.deepEqual([(await shown(page)).key, await locks(page)], ['', OPEN], 'a cleared page opens fresh and unlocked');
      await hold(page);
      await page.evaluate(() => window.__page.open());
      const held = await pending(page, 'GET');
      await page.evaluate(() => window.__page.destroy());
      const after = await page.evaluate(() => window.__requests.length);
      await answer(page, held, 200, START);
      await settle(page);
      assert.equal(await page.evaluate(() => window.__requests.length), after, 'a destroyed page starts nothing');
      await page.close();
    }
    // Copy all: the JSON of the map; feedback that arrives after a reopen or after the page was swapped out shows nothing, and the timer
    // of a cleared page touches nothing.
    {
      const page = await mount();
      await page.evaluate(() => {
        window.__copied = [];
        Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: value => new Promise(resolve => { window.__copied.push({ value, resolve }); }) } });
      });
      await click(page, '#macros-copy');
      await page.waitForFunction(() => window.__copied.length === 1);
      await page.evaluate(() => window.__copied[0].resolve());
      await page.waitForFunction(() => document.querySelector('#macros-copy').textContent === 'Copied');
      assert.deepEqual(JSON.parse(await page.evaluate(() => window.__copied[0].value)), START);
      await click(page, '#macros-copy');
      await page.waitForFunction(() => window.__copied.length === 2);
      await page.evaluate(() => window.__page.open());
      await page.evaluate(() => window.__copied[1].resolve());
      await settle(page);
      assert.equal(await page.$eval('#macros-copy', el => el.textContent), 'Copy all', 'an open resets the label, and a copy started before it shows nothing');
      await click(page, '#macros-copy');
      await page.waitForFunction(() => window.__copied.length === 3);
      await page.evaluate(() => window.__copied[2].resolve());
      await page.waitForFunction(() => document.querySelector('#macros-copy').textContent === 'Copied');
      await page.evaluate(() => window.__page.clear());
      await page.evaluate(() => window.__page.open());
      await page.waitForFunction(() => document.querySelectorAll('#macros-list .snippet-row').length === 2);
      await click(page, '#macros-copy');
      await page.waitForFunction(() => window.__copied.length === 4);
      await page.evaluate(() => window.__page.destroy());
      await page.evaluate(() => window.__copied[3].resolve());
      await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 2200)));
      assert.equal(await page.evaluate(() => document.querySelector('#settings-macros').childElementCount), 0, 'a copy answered after the page is gone draws nothing');
      await page.close();
    }
    // Two copies answered out of order: the older one, finishing after the newer one's label has gone back, shows nothing.
    {
      const page = await mount();
      await page.evaluate(() => {
        window.__copied = [];
        Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: value => new Promise(resolve => { window.__copied.push({ value, resolve }); }) } });
      });
      await click(page, '#macros-copy'); await click(page, '#macros-copy');
      await page.waitForFunction(() => window.__copied.length === 2);
      await page.evaluate(() => window.__copied[1].resolve());
      await page.waitForFunction(() => document.querySelector('#macros-copy').textContent === 'Copied');
      await page.waitForFunction(() => document.querySelector('#macros-copy').textContent === 'Copy all', { timeout: 5000 });
      await page.evaluate(() => window.__copied[0].resolve());
      await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 2200)));
      assert.equal(await page.$eval('#macros-copy', el => el.textContent), 'Copy all', 'an older copy cannot leave "Copied" stuck');
      await page.close();
    }
    // Someone who is not an admin sees the list but no form, no Import and no row actions.
    {
      const page = await mount({ admin: false });
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#macro-editor').hidden, document.querySelector('#macros-import-open').hidden, document.querySelectorAll('#macros-list button').length, document.querySelectorAll('#macros-list .snippet-row').length]), [true, true, 0, 2]);
      await page.close();
    }
    // A form or import draft counts as unsaved while it is there and is gone with the page; a write held when the page is destroyed is not
    // sent again and its answer draws nothing.
    for (const draft of ['form', 'import']) {
      const page = await mount();
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), false);
      if (draft === 'form') await type(page, '#macro-editor [name=body]', 'UNSAVED'); else { await click(page, '#macros-import-open'); await type(page, '#macros-import-text', '{"u":"v"}'); }
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), true, `${draft} counts as unsaved`);
      assert.equal(draft === 'form' ? (await shown(page)).body : await page.$eval('#macros-import-text', el => el.value), draft === 'form' ? 'UNSAVED' : '{"u":"v"}');
      await page.evaluate(() => window.__page.destroy());
      assert.deepEqual(await page.evaluate(() => [window.__page.hasUnsavedChanges(), document.querySelector('#settings-macros').childElementCount]), [false, 0], `destroying the page drops the ${draft} draft`);
      await page.close();
    }
    {
      const page = await mount();
      await click(page, '#macros-list .snippet-row:first-child button.btn-danger');
      const put = await pending(page, 'PUT');
      const reads = await count(page, 'GET');
      await page.evaluate(() => window.__page.destroy());
      await answer(page, put, 200, { greet: 'Hello' });
      await settle(page);
      assert.deepEqual([await count(page, 'PUT'), await count(page, 'GET'), await page.evaluate(() => document.querySelector('#settings-macros').childElementCount)], [1, reads, 0], 'the held write is not sent again, its answer reads nothing and draws nothing');
      await page.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('macros page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

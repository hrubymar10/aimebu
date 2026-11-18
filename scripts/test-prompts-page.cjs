// Settings → Prompts in Chromium, through its page slot, with every request held until the test answers it, so the order of answers is
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
<body><dialog id="settings-dialog" class="settings-modal" aria-label="Settings" open><div class="settings-pane"><nav class="settings-nav" aria-label="Settings sections"></nav><div class="settings-body"><header class="settings-body-header"></header></div></div></dialog></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const KEYS = ['identity', 'retry_notice', 'inbox_ask', 'mentions', 'human_reply', 'tail'];
const projects = [{ id: 'prj_a', name: 'A' }, { id: 'prj_b', name: 'B' }];
// A scope's answer: each slot's text names the scope it was read for, so the test can tell which answer drew. `slots` overrides by key.
const view = (scope, { slots = {}, ...rest } = {}) => ({
  slots: KEYS.map(key => ({ key, title: key.toUpperCase(), desc: `${key} desc`, default: `${key} shipped`, effective: `${key} in ${scope || 'global'}`, source: 'default', global: null, project: null, ...slots[key] })),
  max: 2000, inline_plan_appendix: 'optional', can_edit: true, can_edit_global: true, ...rest,
});
const read = scope => `GET /api/prompts?project_id=${scope}`;

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
  const mount = async (auto = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, { [read('')]: view(''), ...auto });
    await page.evaluate(async projects => {
      const { PromptSettings } = await import('/js/pages/prompts/page.js');
      window.__page = new PromptSettings(document.querySelector('#settings-dialog'), () => ({ projects, selectedProjectID: '' }));
      window.__page.section.hidden = false; window.__page.section.classList.add('active'); window.__page.open();
    }, projects);
    await page.waitForFunction(() => document.querySelectorAll('#prompt-list .prompt-slot').length === 6);
    return page;
  };
  const pending = async (page, method, requestPath) => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && request.method === method && request.path === requestPath), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findIndex(request => !request.answered && request.method === method && request.path === requestPath), method, requestPath);
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50)));
  const type = (page, key, value) => page.$eval(`#prompt-${key}`, (el, value) => { el.value = value; el.dispatchEvent(new Event('input', { bubbles: true })); }, value);
  const chooseScope = (page, scope) => page.$eval('#prompt-scope', (el, scope) => { el.value = scope; el.dispatchEvent(new Event('change', { bubbles: true })); }, scope);
  const click = (page, selector) => page.$eval(selector, el => el.click());
  const shown = page => page.evaluate(() => ({
    scope: document.querySelector('#prompt-list').dataset.scope, selected: document.querySelector('#prompt-scope').value,
    tail: document.querySelector('#prompt-tail').value, error: document.querySelector('#settings-prompts [role=alert]').textContent,
    status: document.querySelector('#settings-prompts [role=status]').textContent,
  }));
  const locks = page => page.evaluate(() => ({
    scope: document.querySelector('#prompt-scope').disabled, inline: document.querySelector('#prompt-inline-plan').disabled,
    boxes: [...document.querySelectorAll('#prompt-list textarea')].every(box => box.disabled), buttons: document.querySelector('#prompt-list').dataset.locked === 'true' && [...document.querySelectorAll('#prompt-list button')].every(button => button.disabled),
    busy: document.querySelector('#settings-prompts').hasAttribute('aria-busy'),
  }));
  const count = (page, method) => page.evaluate(method => window.__requests.filter(request => request.method === method).length, method);
  try {
    // Global → A → B → A with the answers reordered: only the last scope's answer draws, and an earlier one's failure says nothing.
    // While another scope's read is pending or has failed, the rows drawn for the previous scope cannot write anywhere.
    {
      const page = await mount();
      await chooseScope(page, 'prj_a'); const first = await pending(page, 'GET', '/api/prompts?project_id=prj_a');
      await chooseScope(page, 'prj_b'); const second = await pending(page, 'GET', '/api/prompts?project_id=prj_b');
      await chooseScope(page, 'prj_a');
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/prompts?project_id=prj_a').length === 2);
      const third = await page.evaluate(() => window.__requests.findLastIndex(request => request.path === '/api/prompts?project_id=prj_a'));
      assert.deepEqual(await locks(page), { scope: false, inline: false, boxes: true, buttons: true, busy: false }, 'rows of the previous scope are locked while another one loads');
      await answer(page, third, 200, view('prj_a'));
      await page.waitForFunction(() => document.querySelector('#prompt-list').dataset.scope === 'prj_a');
      await answer(page, second, 200, view('prj_b'));
      await answer(page, first, 500, { message: 'stale failure' });
      await settle(page);
      assert.deepEqual(await shown(page), { scope: 'prj_a', selected: 'prj_a', tail: 'tail in prj_a', error: '', status: '' }, 'only the latest read draws; a stale failure is not shown');
      await chooseScope(page, 'prj_b');
      await answer(page, await pending(page, 'GET', '/api/prompts?project_id=prj_b'), 500, { message: 'b failed' });
      await page.waitForFunction(() => document.querySelector('#settings-prompts [role=alert]').textContent === 'b failed');
      await type(page, 'tail', 'typed under B');
      await click(page, '.prompt-slot[data-key=tail] .prompt-save');
      await settle(page);
      assert.deepEqual([await shown(page).then(s => [s.scope, s.selected]), (await locks(page)).buttons, await count(page, 'PUT')], [['prj_a', 'prj_b'], true, 0], "A's rows under a failed B read send nothing");
      await page.close();
    }
    // A conflict reloads the list and keeps every draft, the saved slot's too; an untouched box shows the newer text. The page is locked
    // while the write is held.
    {
      const page = await mount();
      await type(page, 'tail', 'TAIL DRAFT'); await type(page, 'identity', 'ID DRAFT');
      await click(page, '.prompt-slot[data-key=tail] .prompt-save');
      const put = await pending(page, 'PUT', '/api/prompts/tail');
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body, put), { project_id: '', value: 'TAIL DRAFT', rev: 0 });
      assert.deepEqual(await locks(page), { scope: true, inline: true, boxes: true, buttons: true, busy: true }, 'everything is locked while a write is held');
      await page.evaluate(newer => { window.__auto['GET /api/prompts?project_id='] = newer; }, view('', { slots: { mentions: { effective: 'mentions newer' } } }));
      await answer(page, put, 409, { error: 'stale_rev', message: 'prompt changed; reload it' });
      await page.waitForFunction(() => document.querySelector('#settings-prompts [role=alert]').textContent.includes('your text is still in the box'));
      await settle(page);
      assert.deepEqual(await page.evaluate(() => ['tail', 'identity', 'mentions'].map(key => document.querySelector(`#prompt-${key}`).value)), ['TAIL DRAFT', 'ID DRAFT', 'mentions newer'], 'drafts survive a conflict; untouched boxes take the newer text');
      assert.deepEqual(await locks(page), { scope: false, inline: false, boxes: false, buttons: false, busy: false }, 'the page unlocks after the reload');
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), true);
      await page.close();
    }
    // Any other failure leaves the page as it is: the same box, the draft, the message, and no read.
    {
      const page = await mount();
      // Save waits for an edit (and goes back off when the text is put back), and Inline plans is a group of its own, outside the scope card.
      const saveOff = () => page.$eval('.prompt-slot[data-key=tail] .prompt-save', el => el.disabled);
      assert.equal(await saveOff(), true, 'nothing to save yet');
      const original = await page.$eval('#prompt-tail', el => el.value);
      await type(page, 'tail', 'KEEP ME');
      assert.equal(await saveOff(), false, 'an edit can be saved');
      await type(page, 'tail', original);
      assert.equal(await saveOff(), true, 'the original text is nothing to save');
      await type(page, 'tail', 'tail shipped');
      assert.equal(await saveOff(), true, 'the shipped text with no override anywhere is nothing to save');
      assert.equal(await page.$eval('#prompt-inline-plan', el => !el.closest('.settings-group, section, fieldset')?.querySelector('#prompt-scope')), true, 'Inline plans is not in the Scope card');
      await type(page, 'tail', 'KEEP ME');
      const box = await page.$('#prompt-tail');
      const reads = await count(page, 'GET');
      await click(page, '.prompt-slot[data-key=tail] .prompt-save');
      await answer(page, await pending(page, 'PUT', '/api/prompts/tail'), 500, { message: 'nope' });
      await page.waitForFunction(() => document.querySelector('#settings-prompts [role=alert]').textContent === 'nope');
      assert.deepEqual([await page.evaluate(el => el === document.querySelector('#prompt-tail') && el.value, box), await count(page, 'GET'), (await locks(page)).busy], ['KEEP ME', reads, false]);
      await page.close();
    }
    // Reset in a project scope sends that scope and the layer's revision, then reads the list again.
    // A project can save the shipped text over a global override (to say "the default, whatever the global says"): Save is on for it, though the text equals the shipped one.
    {
      const page = await mount({ [read('prj_a')]: view('prj_a', { slots: { tail: { global: 'tail global override', effective: 'tail global override', source: 'global' } } }) });
      await chooseScope(page, 'prj_a');
      await page.waitForFunction(() => document.querySelector('#prompt-tail').value === 'tail global override');
      await type(page, 'tail', 'tail shipped');
      assert.equal(await page.$eval('.prompt-slot[data-key=tail] .prompt-save', el => el.disabled), false, 'the shipped text over a global override can be saved by a project');
      await page.close();
    }
    {
      const overridden = view('prj_a', { slots: { tail: { source: 'project', effective: 'project tail', project: { value: 'project tail', rev: 4 } } } });
      const page = await mount({ [read('prj_a')]: overridden });
      await chooseScope(page, 'prj_a');
      await page.waitForFunction(() => document.querySelector('#prompt-list').dataset.scope === 'prj_a');
      assert.deepEqual(await page.$eval('.prompt-slot[data-key=tail]', el => [el.querySelector('.prompt-source').textContent, el.querySelector('.prompt-reset').textContent, el.querySelector('.prompt-reset').disabled]), ['Project override (this project)', 'Reset to global/shipped', false]);
      await page.evaluate(next => { window.__auto['GET /api/prompts?project_id=prj_a'] = next; }, view('prj_a'));
      await click(page, '.prompt-slot[data-key=tail] .prompt-reset');
      await answer(page, await pending(page, 'DELETE', '/api/prompts/tail?project_id=prj_a&rev=4'), 204);
      await page.waitForFunction(() => document.querySelector('#settings-prompts [role=status]').textContent === '“TAIL” is back to the next level up.');
      assert.deepEqual(await page.$eval('.prompt-slot[data-key=tail]', el => [el.querySelector('.prompt-source').textContent, el.querySelector('.prompt-reset').disabled, el.querySelector('textarea').value]), ['Shipped text', true, 'tail in prj_a']);
      await page.close();
    }
    // A save that succeeds and whose reload fails keeps the text in the box and says the reload failed; the next save conflicts, reloads
    // and settles.
    {
      const page = await mount();
      await page.evaluate(() => { delete window.__auto['GET /api/prompts?project_id=']; });
      await type(page, 'tail', 'SAVED TEXT');
      await click(page, '.prompt-slot[data-key=tail] .prompt-save');
      await answer(page, await pending(page, 'PUT', '/api/prompts/tail'), 204);
      await answer(page, await pending(page, 'GET', '/api/prompts?project_id='), 500, { message: 'read failed' });
      await page.waitForFunction(() => !document.querySelector('#settings-prompts').hasAttribute('aria-busy'));
      assert.deepEqual(await shown(page), { scope: '', selected: '', tail: 'SAVED TEXT', error: 'read failed', status: 'Saved “TAIL”.' });
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), true, 'the text is kept until a read shows it saved');
      await click(page, '.prompt-slot[data-key=tail] .prompt-save');
      await answer(page, await pending(page, 'PUT', '/api/prompts/tail'), 409, { message: 'prompt changed; reload it' });
      await answer(page, await pending(page, 'GET', '/api/prompts?project_id='), 200, view('', { slots: { tail: { source: 'global', effective: 'SAVED TEXT', global: { value: 'SAVED TEXT', rev: 1 } } } }));
      await page.waitForFunction(() => document.querySelector('.prompt-slot[data-key=tail] .prompt-source').textContent === 'Global override (saved here)');
      assert.deepEqual([await page.$eval('#prompt-tail', el => el.value), await page.evaluate(() => window.__page.hasUnsavedChanges())], ['SAVED TEXT', false]);
      await page.close();
    }
    // Hide does nothing to a write in flight; a reopen makes it obsolete: its answer reads nothing and reports nothing, but unlocks.
    {
      const page = await mount();
      await type(page, 'tail', 'HIDDEN SAVE');
      await click(page, '.prompt-slot[data-key=tail] .prompt-save');
      const put = await pending(page, 'PUT', '/api/prompts/tail');
      await page.evaluate(() => { window.__page.section.hidden = true; });
      await answer(page, put, 204);
      await page.waitForFunction(() => document.querySelector('#settings-prompts [role=status]').textContent === 'Saved “TAIL”.');
      await page.evaluate(() => { window.__page.section.hidden = false; window.__page.section.classList.add('active'); });
      await type(page, 'tail', 'REOPENED SAVE');
      await click(page, '.prompt-slot[data-key=tail] .prompt-save');
      const second = await pending(page, 'PUT', '/api/prompts/tail');
      await page.evaluate(() => window.__page.open());
      await page.waitForFunction(() => !document.querySelector('#prompt-tail').value.includes('REOPENED'));
      const reads = await count(page, 'GET');
      await answer(page, second, 204);
      await settle(page);
      assert.deepEqual([await count(page, 'GET'), (await shown(page)).status, await locks(page)], [reads, '', { scope: false, inline: false, boxes: false, buttons: false, busy: false }], 'an obsolete write reads and reports nothing');
      await page.close();
    }
    // Clear during a held write and destroy during a held read: their answers draw nothing and start nothing; a cleared page opens again.
    {
      const page = await mount();
      await type(page, 'tail', 'CLEARED');
      await click(page, '.prompt-slot[data-key=tail] .prompt-save');
      const put = await pending(page, 'PUT', '/api/prompts/tail');
      await page.evaluate(() => window.__page.clear());
      const made = await page.evaluate(() => window.__requests.length);
      await answer(page, put, 204);
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#settings-prompts').childElementCount, window.__requests.length, document.querySelector('#settings-prompts').hasAttribute('aria-busy')]), [0, made, false], 'a cleared page stays empty');
      await page.evaluate(() => window.__page.open());
      await page.waitForFunction(() => document.querySelectorAll('#prompt-list .prompt-slot').length === 6);
      assert.deepEqual([(await shown(page)).tail, (await locks(page)).busy], ['tail in global', false], 'a cleared page opens fresh');
      await page.evaluate(() => { delete window.__auto['GET /api/prompts?project_id=']; window.__page.open(); });
      const held = await pending(page, 'GET', '/api/prompts?project_id=');
      await page.evaluate(() => window.__page.destroy());
      const after = await page.evaluate(() => window.__requests.length);
      await answer(page, held, 200, view(''));
      await settle(page);
      assert.equal(await page.evaluate(() => window.__requests.length), after, 'a destroyed page starts nothing');
      await page.close();
    }
    // Inline plans: one save at a time; a failure goes back to the last saved value, not to the one first read; a save answered after
    // a reopen reports nothing.
    {
      const page = await mount();
      const pick = value => page.$eval('#prompt-inline-plan', (el, value) => { el.value = value; el.dispatchEvent(new Event('change', { bubbles: true })); }, value);
      await pick('always');
      const put = await pending(page, 'PUT', '/api/inline-plans');
      assert.deepEqual([await page.evaluate(index => window.__requests[index].body, put), (await locks(page)).inline], [{ value: 'always' }, true], 'the select is locked while its save is held');
      await answer(page, put, 204);
      await page.waitForFunction(() => document.querySelector('#settings-prompts [role=status]').textContent === 'Saved. Inline plans: always.');
      await pick('optional');
      await answer(page, await pending(page, 'PUT', '/api/inline-plans'), 500, { message: 'no' });
      await page.waitForFunction(() => document.querySelector('#settings-prompts [role=alert]').textContent === 'no');
      assert.equal(await page.$eval('#prompt-inline-plan', el => el.value), 'always', 'a failure restores the value last saved');
      await page.evaluate(next => { window.__auto['GET /api/prompts?project_id='] = next; }, view('', { inline_plan_appendix: 'always' }));
      await pick('optional');
      const late = await pending(page, 'PUT', '/api/inline-plans');
      await page.evaluate(() => window.__page.open());
      await settle(page);
      await answer(page, late, 500, { message: 'late no' });
      await settle(page);
      assert.deepEqual([(await shown(page)).error, await page.$eval('#prompt-inline-plan', el => [el.value, el.disabled])], ['', ['always', false]], 'a save answered after a reopen reports nothing');
      await page.close();
    }
    // Drafts go when a read is asked for, not when it answers: text typed while a reopen's read is held stays, an older draft does not.
    {
      const page = await mount();
      await type(page, 'identity', 'BEFORE REOPEN');
      await page.evaluate(() => { delete window.__auto['GET /api/prompts?project_id=']; window.__page.open(); });
      const held = await pending(page, 'GET', '/api/prompts?project_id=');
      await page.waitForFunction(() => document.querySelector('#prompt-identity').value === 'identity in global');
      await type(page, 'tail', 'TYPED WHILE LOADING');
      await answer(page, held, 200, view('', { slots: { identity: { effective: 'identity newer' }, tail: { effective: 'tail newer' } } }));
      await page.waitForFunction(() => document.querySelector('#prompt-identity').value === 'identity newer');
      assert.deepEqual([(await shown(page)).tail, await page.evaluate(() => window.__page.hasUnsavedChanges())], ['TYPED WHILE LOADING', true], 'text typed during the read is kept');
      await page.close();
    }
    // A read that started before an inline-plans save succeeded does not put the older value back, in either order of answers.
    for (const order of ['save first', 'read first']) {
      const page = await mount();
      const pick = value => page.$eval('#prompt-inline-plan', (el, value) => { el.value = value; el.dispatchEvent(new Event('change', { bubbles: true })); }, value);
      await page.evaluate(() => { delete window.__auto['GET /api/prompts?project_id=']; window.__page.open(); });
      const held = await pending(page, 'GET', '/api/prompts?project_id=');
      await pick('always');
      const put = await pending(page, 'PUT', '/api/inline-plans');
      if (order === 'save first') { await answer(page, put, 204); await settle(page); await answer(page, held, 200, view('')); } else { await answer(page, held, 200, view('')); await settle(page); await answer(page, put, 204); }
      await page.waitForFunction(() => !document.querySelector('#prompt-inline-plan').disabled);
      await settle(page);
      assert.equal(await page.$eval('#prompt-inline-plan', el => el.value), 'always', `an older read keeps the saved value (${order})`);
      await pick('optional');
      await answer(page, await pending(page, 'PUT', '/api/inline-plans'), 500, { message: 'no' });
      await page.waitForFunction(() => document.querySelector('#settings-prompts [role=alert]').textContent === 'no');
      assert.equal(await page.$eval('#prompt-inline-plan', el => el.value), 'always', `a later failure goes back to the saved value (${order})`);
      await page.close();
    }
    // The count is in characters, not UTF-16 units; the preview shows unsaved text and the locked parts.
    {
      const page = await mount();
      await type(page, 'tail', '👍é');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('.prompt-slot[data-key=tail] .prompt-count').textContent, document.querySelector('#prompt-preview').textContent.includes('👍é'), document.querySelector('#prompt-preview').textContent.startsWith('[Opening line (set by the agent adapter)]')]), ['2 / 2000', true, true]);
      await page.close();
    }
    // A viewer who cannot edit sees every control locked and the reason.
    {
      const page = await mount({ [read('')]: view('', { can_edit: false, can_edit_global: false }) });
      await page.waitForFunction(() => document.querySelector('#settings-prompts [role=status]').textContent !== '');
      assert.deepEqual([(await shown(page)).status, await locks(page)], ['Only the owner or an admin can change the global prompts.', { scope: false, inline: true, boxes: true, buttons: true, busy: false }]);
      await page.close();
    }
    // Unfinished text counts as unsaved while it is there, and is gone with the page; the scope is kept across reopening.
    {
      const page = await mount({ [read('prj_a')]: view('prj_a') });
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), false);
      await chooseScope(page, 'prj_a');
      await page.waitForFunction(() => document.querySelector('#prompt-list').dataset.scope === 'prj_a');
      await type(page, 'tail', 'UNSAVED');
      await settle(page);
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), true);
      await page.evaluate(() => { window.__page.open(); });
      await page.waitForFunction(() => document.querySelector('#prompt-list').dataset.scope === 'prj_a');
      assert.equal((await shown(page)).selected, 'prj_a', 'opening again keeps the scope');
      await page.waitForFunction(() => !window.__page.hasUnsavedChanges(), { timeout: 5000 }).catch(() => {});
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), false, 'opening again drops the draft');
      await page.evaluate(() => window.__page.destroy());
      assert.equal(await page.evaluate(() => document.querySelector('#settings-prompts').childElementCount), 0, 'destroying the page leaves no content');
      await page.close();
    }
    // The preview shows its prompt: the section is a flex column that scrolls, and an item with overflow:auto has no automatic minimum
    // size, so the column used to crush the preview to a single line under the slot cards. Text wraps, so nothing scrolls sideways.
    {
      const page = await mount();
      const seen = await page.evaluate(() => {
        const pre = document.querySelector('#prompt-preview');
        return { height: pre.getBoundingClientRect().height, content: pre.scrollHeight, sideways: pre.scrollWidth - pre.clientWidth };
      });
      assert.ok(seen.height >= Math.min(seen.content, 320) - 2, `the preview is as tall as its content up to its cap, not a crushed line (height ${seen.height}, content ${seen.content})`);
      assert.ok(seen.height >= 3 * 13, 'even a three-line prompt gets its three lines');
      assert.ok(seen.sideways <= 0, 'the wrapped text has no horizontal scrollbar');
      await page.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('prompts page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

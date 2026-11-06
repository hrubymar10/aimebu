// Settings → Roles in Chromium, with every request held until the test answers it, so the order of answers is chosen. web/ is served as is
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
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css">${importMap}</head>
<body><dialog id="settings-dialog" open><nav></nav><section id="settings-roles" class="settings-section active" hidden></section></dialog></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const role = (key, extra = {}) => ({ id: `role_${key}`, rev: 1, role_key: key, label: key[0].toUpperCase() + key.slice(1), emoji: '', body: `${key} instructions`, extends: '', cardinality: 'multi', builtin: false, default_agents: [], pick_mode: 'in_turn', ...extra });
const ROLES = [role('lead', { builtin: true, cardinality: 'singleton', cap_is_lead: true, emoji: '👑' }), role('reviewer', { builtin: true }), role('sec', { extends: 'reviewer', body: 'Additional focus.', resolved_body: 'reviewer instructions\n\nAdditional focus.', builtin: true }), role('worker', { builtin: true, rev: 4, default_agents: [{ launcher_id: 'l1', model: 'a' }] }), role('analyst')];
const READ = 'GET /api/roles';
const AUTO = {
  [READ]: ROLES, 'GET /api/machines': { machines: [{ id: 'm1', name: 'Host' }] },
  'GET /api/spawn-catalog?machine_id=m1': { tools: [{ launcher_id: 'l1', label: 'Codex', models: [{ id: 'a', label: 'A' }, { id: 'b', label: 'B' }] }] },
};

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
  const mount = async ({ auto = AUTO, open = true } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, auto);
    await page.evaluate(async open => {
      const { RoleSettings } = await import('/js/pages/roles/page.js');
      window.__events = [];
      document.addEventListener('role-defaults-change', event => window.__events.push(event.detail?.source ? 'own' : 'foreign'));
      window.__page = new RoleSettings(document.querySelector('#settings-roles'));
      window.__page.section.hidden = false; window.__page.section.classList.add('active');
      if (open) window.__page.open();
    }, open);
    if (open && auto[READ]) await page.waitForFunction(() => document.querySelector('#roles-list')?.dataset.loaded === '1');
    return page;
  };
  const pending = async (page, method, requestPath) => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && request.method === method && request.path.startsWith(requestPath)), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findIndex(request => !request.answered && request.method === method && request.path.startsWith(requestPath)), method, requestPath);
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 60)));
  const type = (page, selector, value) => page.$eval(selector, (el, value) => { el.value = value; el.dispatchEvent(new Event('input', { bubbles: true })); }, value);
  const click = (page, selector) => page.$eval(selector, el => el.click());
  const count = (page, method, requestPath = '') => page.evaluate((method, requestPath) => window.__requests.filter(request => request.method === method && request.path.startsWith(requestPath)).length, method, requestPath);
  const shown = page => page.evaluate(() => {
    const form = document.querySelector('#role-editor');
    return { open: !form.hidden, heading: document.querySelector('#role-editor-title').textContent, key: form.elements.role_key.value, keyReadOnly: form.elements.role_key.readOnly, label: form.elements.label.value,
      selected: [...document.querySelectorAll('#roles-cards .role-card-selected')].map(card => card.dataset.roleKey), error: document.querySelector('#roles-error').textContent,
      focus: document.activeElement?.name || document.activeElement?.className || document.activeElement?.id };
  });
  const dialog = page => page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
  try {
    // The cards, the hidden select behind them, and what opening a role or a new one shows.
    {
      const page = await mount();
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('#roles-cards .role-card')].map(card => [card.dataset.roleKey, [...card.querySelectorAll('.role-chip')].map(chip => chip.textContent).join('|'), card.querySelector('.role-card-edit').getAttribute('aria-label')])),
        [['lead', 'one holder|lead|built in', 'Edit Lead'], ['reviewer', 'many holders|built in', 'Edit Reviewer'], ['sec', 'many holders|extends Reviewer|built in', 'Edit Sec'], ['worker', 'many holders|built in', 'Edit Worker'], ['analyst', 'many holders', 'Edit Analyst']]);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#roles-list').closest('.settings-row').hidden, [...document.querySelectorAll('#roles-list option')].map(option => option.textContent).join('|')]), [true, 'New role|Lead (lead)|Reviewer (reviewer)|Sec (sec)|Worker (worker)|Analyst (analyst)']);
      assert.equal((await shown(page)).open, false, 'the editor stays closed until a role is opened');
      await click(page, '#role-new');
      assert.deepEqual(await shown(page).then(({ open, heading, key, keyReadOnly, selected, focus }) => ({ open, heading, key, keyReadOnly, selected, focus })), { open: true, heading: 'New role', key: '', keyReadOnly: false, selected: [], focus: 'role_key' }, 'New role opens a blank form on its key');
      await click(page, '#roles-cards [data-role-key=sec] .role-card-edit');
      assert.deepEqual(await page.evaluate(() => {
        const form = document.querySelector('#role-editor');
        return { heading: document.querySelector('#role-editor-title').textContent, key: form.elements.role_key.value, readOnly: form.elements.role_key.readOnly, extends: form.elements.extends.value, parents: [...form.elements.extends.options].map(option => option.value).join('|'),
          resolved: document.querySelector('#role-resolved').value, resolvedRow: document.querySelector('#role-resolved-row').hidden, reset: document.querySelector('#role-reset').disabled, remove: document.querySelector('#role-delete').disabled,
          button: document.querySelector('#roles-cards [data-role-key=sec] .role-card-edit').getAttribute('aria-label') };
      }), { heading: 'Editing Sec', key: 'sec', readOnly: true, extends: 'reviewer', parents: '|lead|reviewer|worker|analyst'.replace('|lead|reviewer|worker|analyst', '|analyst|lead|reviewer|worker'), resolved: 'reviewer instructions\n\nAdditional focus.', resolvedRow: false, reset: false, remove: true, button: 'Close Sec without saving' });
      await click(page, '#roles-cards [data-role-key=analyst] .role-card-edit');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#role-resolved-row').hidden, document.querySelector('#role-reset').disabled, document.querySelector('#role-delete').disabled]), [true, true, false], 'a role of one\'s own can be deleted, not reset');
      await page.close();
    }
    // Two reads answered out of order: only the newer one draws.
    {
      const page = await mount({ auto: { ...AUTO, [READ]: undefined } , open: false });
      await page.evaluate(() => { window.__page.open(); window.__page.open(); });
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/roles').length === 2);
      const [first, second] = await page.evaluate(() => window.__requests.flatMap((request, index) => request.path === '/api/roles' ? [index] : []));
      await answer(page, second, 200, [role('newer')]);
      await page.waitForFunction(() => document.querySelector('#roles-cards [data-role-key=newer]'));
      await answer(page, first, 200, [role('older')]);
      await settle(page);
      assert.deepEqual(await page.$$eval('#roles-cards .role-card', cards => cards.map(card => card.dataset.roleKey)), ['newer'], 'an older read answered late does not draw');
      await page.close();
    }
    // A save sends the draft with the stored revision, one write at a time; the form closes and focus goes to the role's card.
    {
      const page = await mount();
      await click(page, '#roles-cards [data-role-key=worker] .role-card-edit');
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), false);
      await type(page, '#role-editor [name=label]', ' Builder ');
      await click(page, '#role-editor [name=cap_can_spawn]');
      assert.equal(await page.evaluate(() => window.__page.hasUnsavedChanges()), true);
      await page.evaluate(() => { delete window.__auto['GET /api/roles']; document.querySelector('#role-editor').requestSubmit(); document.querySelector('#role-editor').requestSubmit(); });
      const put = await pending(page, 'PUT', '/api/roles/role_worker');
      await settle(page);
      assert.deepEqual([await count(page, 'PUT'), await page.evaluate(() => [document.querySelector('#role-editor [type=submit]').disabled, document.querySelector('#role-reset').disabled, document.querySelector('#role-delete').disabled])], [1, [true, true, true]], 'a second submit sends nothing and the write buttons wait');
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body, put), { role_key: 'worker', label: 'Builder', emoji: '', body: 'worker instructions', extends: '', cardinality: 'multi', cap_is_lead: false, cap_can_spawn: true, cap_singleton_writer: false, default_agents: [{ launcher_id: 'l1', model: 'a' }], pick_mode: 'in_turn', id: 'role_worker', rev: 4 });
      await answer(page, put, 200, { id: 'role_worker' });
      const read = await pending(page, 'GET', '/api/roles');
      await answer(page, read, 200, ROLES.map(item => (item.id === 'role_worker' ? { ...item, label: 'Builder', rev: 5 } : item)));
      await page.waitForFunction(() => document.querySelector('#role-editor').hidden && document.activeElement === document.querySelector('#roles-cards [data-role-key=worker] .role-card-edit'));
      assert.deepEqual(await page.evaluate(() => [window.__events.length, document.querySelector('#roles-cards [data-role-key=worker] .role-card-label').textContent]), [1, 'Builder'], 'a save closes the form, focuses the card and tells the Harnesses page');
      await page.close();
    }
    // A refused save says why and keeps the editor and what was typed; the next save can go.
    {
      const page = await mount();
      await click(page, '#roles-cards [data-role-key=analyst] .role-card-edit');
      await type(page, '#role-editor [name=label]', 'Changed');
      await page.evaluate(() => { document.querySelector('#role-editor').requestSubmit(); });
      await answer(page, await pending(page, 'PUT', '/api/roles/role_analyst'), 409, { message: 'This role changed elsewhere' });
      await page.waitForFunction(() => document.querySelector('#roles-error').textContent === 'This role changed elsewhere');
      assert.deepEqual(await shown(page).then(({ open, label, error }) => ({ open, label, error })), { open: true, label: 'Changed', error: 'This role changed elsewhere' });
      assert.equal(await page.$eval('#role-editor [type=submit]', el => el.disabled), false);
      assert.equal(await page.evaluate(() => window.__events.length), 0, 'a refused save tells no one');
      await page.close();
    }
    // Closing with edits asks first; Keep leaves the form, Discard closes it and gives focus back to the card; Escape only closes the editor.
    {
      const page = await mount();
      await click(page, '#roles-cards [data-role-key=reviewer] .role-card-edit');
      await click(page, '#role-cancel');
      assert.equal((await shown(page)).open, false, 'a form without edits closes at once');
      await click(page, '#roles-cards [data-role-key=reviewer] .role-card-edit');
      await type(page, '#role-editor [name=label]', 'Typed');
      await click(page, '#role-cancel');
      await dialog(page);
      assert.match(await page.$eval('#app-dialog', el => el.textContent), /Discard your changes to this role\?/);
      await click(page, '#app-dialog .app-dialog-cancel');
      await settle(page);
      assert.deepEqual(await shown(page).then(({ open, label }) => ({ open, label })), { open: true, label: 'Typed' }, 'Keep leaves the editor and the text');
      await page.focus('#role-editor [name=label]');
      await page.keyboard.press('Escape');
      await dialog(page);
      assert.equal(await page.evaluate(() => document.querySelector('#settings-dialog').open), true, 'Escape in the editor does not close Settings');
      await click(page, '#app-dialog .app-dialog-ok');
      await page.waitForFunction(() => document.querySelector('#role-editor').hidden);
      assert.deepEqual(await page.evaluate(() => [document.activeElement === document.querySelector('#roles-cards [data-role-key=reviewer] .role-card-edit'), document.querySelectorAll('#roles-cards .role-card-selected').length, window.__page.hasUnsavedChanges()]), [true, 0, false]);
      await click(page, '#roles-cards [data-role-key=reviewer] .role-card-edit');
      await click(page, '#roles-cards [data-role-key=reviewer] .role-card-edit');
      assert.equal((await shown(page)).open, false, 'the open card\'s button closes the editor');
      await page.close();
    }
    // Delete and reset ask first and send the stored revision; reset leaves the role open on its stored values.
    {
      const page = await mount();
      await click(page, '#roles-cards [data-role-key=analyst] .role-card-edit');
      await click(page, '#role-delete');
      await dialog(page);
      await click(page, '#app-dialog .app-dialog-cancel');
      await settle(page);
      assert.equal(await count(page, 'DELETE'), 0, 'a declined delete sends nothing');
      await click(page, '#role-delete');
      await dialog(page);
      await click(page, '#app-dialog .app-dialog-ok');
      const del = await pending(page, 'DELETE', '/api/roles/role_analyst?rev=1');
      await page.evaluate(() => { delete window.__auto['GET /api/roles']; });
      await answer(page, del, 204);
      await answer(page, await pending(page, 'GET', '/api/roles'), 200, ROLES.filter(item => item.role_key !== 'analyst'));
      await page.waitForFunction(() => !document.querySelector('#roles-cards [data-role-key=analyst]') && document.querySelector('#role-editor').hidden);
      await click(page, '#roles-cards [data-role-key=reviewer] .role-card-edit');
      await type(page, '#role-editor [name=body]', 'edited');
      await click(page, '#role-reset');
      await dialog(page);
      await click(page, '#app-dialog .app-dialog-ok');
      const reset = await pending(page, 'POST', '/api/roles/role_reviewer/reset');
      assert.deepEqual(await page.evaluate(index => window.__requests[index].body, reset), { rev: 1 });
      await answer(page, reset, 200, {});
      await answer(page, await pending(page, 'GET', '/api/roles'), 200, ROLES.filter(item => item.role_key !== 'analyst'));
      await page.waitForFunction(() => document.querySelector('#role-editor [name=body]')?.value === 'reviewer instructions');
      assert.deepEqual(await shown(page).then(({ open, selected }) => ({ open, selected })), { open: true, selected: ['reviewer'] }, 'after a reset the role stays open on its stored values');
      await page.close();
    }
    // The Harnesses page saves the same roles: its event is read here while no editor is open and ignored while one is; ours is not read back.
    {
      const page = await mount();
      const reads = () => count(page, 'GET', '/api/roles');
      const before = await reads();
      await page.evaluate(() => document.dispatchEvent(new CustomEvent('role-defaults-change', { detail: { source: {} } })));
      await page.waitForFunction(before => window.__requests.filter(request => request.path === '/api/roles').length === before + 1, {}, before);
      await click(page, '#roles-cards [data-role-key=worker] .role-card-edit');
      await type(page, '#role-editor [name=label]', 'Mid-edit');
      await page.evaluate(() => document.dispatchEvent(new CustomEvent('role-defaults-change', { detail: { source: {} } })));
      await settle(page);
      assert.deepEqual([await reads(), (await shown(page)).label], [before + 1, 'Mid-edit'], 'an open editor is left alone');
      await page.close();
    }
    // Destroying the page during a held save: the answer draws and reads nothing, and no listener is left.
    {
      const page = await mount();
      await click(page, '#roles-cards [data-role-key=analyst] .role-card-edit');
      await type(page, '#role-editor [name=label]', 'Gone');
      await page.evaluate(() => { document.querySelector('#role-editor').requestSubmit(); });
      const put = await pending(page, 'PUT', '/api/roles/role_analyst');
      await page.evaluate(() => window.__page.destroy());
      const made = await page.evaluate(() => window.__requests.length);
      await answer(page, put, 200, { id: 'role_analyst' });
      await page.evaluate(() => document.dispatchEvent(new CustomEvent('role-defaults-change', { detail: { source: {} } })));
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [window.__requests.length, document.querySelector('#settings-roles').childElementCount, window.__events.length]), [made, 0, 1], 'nothing is read, drawn or announced after destroy (the one event is the test\'s own)');
      await page.close();
    }
    // Default agents: add, order, pick order (only with several), remove, and a choice that is no longer offered.
    {
      const page = await mount();
      await click(page, '#roles-cards [data-role-key=lead] .role-card-edit');
      const rows = () => page.$$eval('#role-editor .spawn-default-row select', selects => selects.map(select => select.value));
      assert.deepEqual([await rows(), await page.$('#role-editor select[aria-label="Default agent selection mode"]')], [[], null]);
      await click(page, '#role-editor [aria-label="Add default agent"]');
      assert.deepEqual([await rows(), await page.evaluate(() => document.activeElement.getAttribute('aria-label'))], [['l1:a'], 'Default agent 1 harness and model'], 'Add picks the first choice and focuses it');
      await page.select('#role-editor .spawn-default-row select', 'l1:b');
      await click(page, '#role-editor [aria-label="Add default agent"]');
      assert.deepEqual(await rows(), ['l1:b', 'l1:a']);
      assert.notEqual(await page.$('#role-editor select[aria-label="Default agent selection mode"]'), null, 'with several defaults the form offers Pick order');
      await click(page, '#role-editor [aria-label="Move default agent 1 down"]');
      assert.deepEqual([await rows(), await page.$eval('#role-editor [aria-label="Move default agent 1 down"]', el => el.disabled)], [['l1:a', 'l1:b'], false]);
      await click(page, '#role-editor [aria-label="Remove default agent 1"]');
      assert.deepEqual([await rows(), await page.evaluate(() => document.activeElement.getAttribute('aria-label')), await page.$('#role-editor select[aria-label="Default agent selection mode"]')], [['l1:b'], 'Default agent 1 harness and model', null], 'removing the second one leaves one and hides Pick order');
      await click(page, '#roles-cards [data-role-key=worker] .role-card-edit');
      await page.waitForFunction(() => document.querySelector('#role-editor [name=label]').value === 'Worker');
      assert.deepEqual(await rows(), ['l1:a'], 'a choice that is offered shows as chosen');
      await page.evaluate(() => { window.__page.destroy(); });
      await page.close();
      const gone = await mount({ auto: { ...AUTO, 'GET /api/spawn-catalog?machine_id=m1': { tools: [] } } });
      await click(gone, '#roles-cards [data-role-key=worker] .role-card-edit');
      assert.deepEqual(await gone.$$eval('#role-editor .spawn-default-row select option', options => options.map(option => option.textContent)), ['l1 · a (unavailable)'], 'a default that is no longer offered stays visible, named as unavailable');
      assert.equal(await gone.$eval('#role-editor [aria-label="Add default agent"]', el => el.disabled), true, 'nothing to add when nothing is offered');
      await gone.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('roles page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

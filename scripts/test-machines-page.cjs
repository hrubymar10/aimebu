// Settings -> Machines and its pairing dialog in Chromium, with every request held until the test answers it (or answered by a rule).
// web/ is served as is with its import map; no Go server. Part of `make test-ui`.
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
<body><dialog id="settings-dialog" open><section id="settings-machines" class="settings-section active"></section></dialog>
<dialog id="machine-pair-dialog" class="dialog-card" aria-labelledby="machine-pair-title"></dialog></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const host = { id: 'mch_host', name: 'This computer', is_host: true, approved: true, connected: true, supervisor_version: '1.2' };
const pending = { id: 'mch_new', name: 'Laptop', approved: false };
const away = { id: 'mch_away', name: 'Box', approved: true, connected: false, last_seen: '2026-02-03T04:05:06Z' };
const old = { id: 'mch_old', name: 'Old', approved: true, revoked_at: '2026-01-01T00:00:00Z' };
const MACHINES = 'GET /api/machines';

// fetch is held unless a rule answers it: a rule is a body, or { status, body }.
function installFetch(auto) {
  window.__requests = []; window.__auto = auto;
  window.fetch = (input, options = {}) => new Promise(resolve => {
    const request = { path: String(input), method: options.method || 'GET', body: options.body ? JSON.parse(options.body) : undefined, answered: false };
    request.answer = (status, body) => { request.answered = true; resolve(new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })); };
    window.__requests.push(request);
    const rule = window.__auto[`${request.method} ${request.path}`];
    if (rule) request.answer(rule.status || 200, rule.status ? rule.body : rule);
  });
  window.__answer = (index, status, body) => window.__requests[index].answer(status, body);
}

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  const mount = async ({ auto = { [MACHINES]: { machines: [host, pending, away, old] } }, owner = true, open = true } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, auto);
    await page.evaluate(async ({ owner, open }) => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_1', owner, admin: true });
      const { MachinesSettings } = await import('/js/pages/machines/page.js');
      window.__page = new MachinesSettings(document.querySelector('#settings-machines'), document.querySelector('#machine-pair-dialog'));
      if (open) window.__page.open();
    }, { owner, open });
    return page;
  };
  const cards = page => page.waitForSelector('#machines-list .machine-card');
  const calls = (page, method) => page.evaluate(method => window.__requests.filter(request => request.method === method).map(request => [request.path, request.body]), method);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 60)));
  const waitRequest = async (page, method, requestPath) => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && request.method === method && request.path === requestPath), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findIndex(request => !request.answered && request.method === method && request.path === requestPath), method, requestPath);
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const confirmOk = async page => { await page.waitForFunction(() => document.querySelector('#app-dialog')?.open); await page.$eval('#app-dialog .app-dialog-ok', el => el.click()); };
  const copyWith = (page, writeText) => page.evaluate(async writeText => {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: new Function(`return ${writeText}`)() } });
    document.querySelector('#machine-pair-copy').click();
    await new Promise(resolve => setTimeout(resolve, 0));
  }, writeText);
  try {
    // The blocks in order; "Loading machines…" until the list answers; each card with its status and its icon actions.
    {
      const page = await mount({ auto: {} });
      assert.deepEqual(await page.evaluate(() => [...document.querySelector('#settings-machines').children].map(node => `${node.tagName.toLowerCase()}${node.id ? `#${node.id}` : ''}`)),
        ['p', 'div', 'p#machines-error', 'p#machines-note', 'div#machines-list'], 'the page starts with its own text: the dialog header already names the section');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#machines-list').textContent, document.querySelector('#machine-add').hidden, document.querySelector('#machine-add').disabled, document.querySelector('#machines-error').getAttribute('role'), document.querySelector('#machines-note').getAttribute('role')]),
        ['Loading machines…', false, false, 'alert', 'status']);
      await answer(page, await waitRequest(page, 'GET', '/api/machines'), 200, { machines: [host, pending, away, old] });
      await cards(page);
      const read = page.$$eval('#machines-list .machine-card', cards => cards.map(card => ({ id: card.dataset.machineId, title: card.querySelector('strong').textContent, status: card.querySelector('.machine-status').textContent, detail: card.querySelector('small').textContent.replace(/Last seen .+/, 'Last seen X'),
        actions: [...card.querySelectorAll('.machine-card-actions button')].map(b => [b.getAttribute('aria-label'), b.title, b.disabled, b.className, b.querySelector('.icon-mask')?.className.replace('icon-mask ', ''), b.textContent]) })));
      const icon = (label, glyph, cls = 'btn-icon') => [label, label, false, cls, `icon-mask-${glyph}`, ''];
      // The one labelled button: its name is its words, its title says the scope.
      const stopAll = [null, 'Stop all agents on this machine', false, 'btn-icon btn-danger machine-stop-all', 'icon-mask-stop', 'Stop all agents'];
      assert.deepEqual(await read, [
        { id: 'mch_host', title: 'This computer · Host', status: 'Connected', detail: 'Supervisor 1.2', actions: [stopAll] },
        { id: 'mch_new', title: 'Laptop', status: 'Pending approval', detail: '', actions: [icon('Approve', 'check'), icon('Revoke', 'key-off')] },
        { id: 'mch_away', title: 'Box', status: 'Offline', detail: 'Last seen X', actions: [icon('Revoke', 'key-off'), stopAll] },
        { id: 'mch_old', title: 'Old', status: 'Revoked', detail: '', actions: [] }]);
      assert.deepEqual(await page.$eval('#machine-refresh', el => [el.getAttribute('aria-label'), el.title, !!el.querySelector('.icon-mask-refresh'), el.textContent]), ['Refresh', 'Refresh', true, '']);
      await page.close();
      const none = await mount({ auto: { [MACHINES]: { machines: [] } } });
      await none.waitForFunction(() => document.querySelector('#machines-list').textContent === 'No machines yet.');
      await none.close();
    }
    // A pending local replacement is visible and only its owner can interrupt the drain.
    {
      const draining = { ...host, restart_pending: true };
      const page = await mount({ auto: { [MACHINES]: { machines: [draining] }, 'POST /api/machines/mch_host/supervisor/restart': { status: 202, body: { restart_pending: true } } } });
      await cards(page);
      assert.equal(await page.$eval('.machine-status', el => el.textContent), 'Supervisor restart pending');
      await page.$eval('.machine-card-actions button:last-child', el => el.click());
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      assert.match(await page.$eval('#app-dialog', el => el.textContent), /Running turns will be interrupted/);
      await page.$eval('#app-dialog .app-dialog-cancel', el => el.click());
      assert.equal((await calls(page, 'POST')).length, 0, 'cancelled restart sends no request');
      await page.$eval('.machine-card-actions button:last-child', el => el.click());
      await confirmOk(page);
      await page.waitForFunction(() => window.__requests.some(request => request.path === '/api/machines/mch_host/supervisor/restart'));
      await page.close();
      const admin = await mount({ auto: { [MACHINES]: { machines: [draining] } }, owner: false });
      await cards(admin);
      assert.equal(await admin.$eval('.machine-card-actions', el => el.textContent.includes('Restart now')), false);
      await admin.close();
    }
    // A remote machine lists its supervisor instances; only one that is not connected and not gone has the write-off icon, and its dialog says what is lost.
    {
      const lost = { journal_id: 'journal-lost-0123456789', build: '1.0', state: 'active', connected: false, last_seen_at: '2026-02-03T04:05:06Z', turns: 2, commands: 1, agents: [{ id: 'agt_1', slug: 'vince' }, { id: 'agt_2', slug: 'nora' }] };
      const live = { journal_id: 'journal-live-0123456789', build: '2.0', state: 'active', connected: true, last_seen_at: '2026-02-03T04:05:06Z', turns: 1, commands: 0, agents: [{ id: 'agt_3', slug: 'ida' }] };
      const gone = { journal_id: 'journal-done-0123456789', build: '0.9', state: 'gone', connected: false, last_seen_at: '2026-01-03T04:05:06Z', gone_at: '2026-01-04T00:00:00Z', turns: 0, commands: 0, agents: [] };
      const gonePath = 'POST /api/machines/mch_away/instances/journal-lost-0123456789/gone';
      const auto = { [MACHINES]: { machines: [host, away] }, 'GET /api/machines/mch_away/instances': { instances: [lost, live, gone] }, [gonePath]: { journal_id: lost.journal_id, already_gone: false, turns_lost: 2, commands_ended: 1 } };
      const page = await mount({ auto });
      await page.waitForSelector('.machine-instance');
      assert.deepEqual(await page.$$eval('.machine-card[data-machine-id="mch_away"] .machine-instance', rows => rows.map(row => [row.querySelector('.machine-instance-title').textContent, row.querySelector('small').textContent.replace(/last seen .+? ·/, 'last seen X ·').replace(/last seen [^·]+$/, 'last seen X'), [...row.querySelectorAll('button')].map(b => [b.title, b.getAttribute('aria-label'), b.className, b.querySelector('.icon-mask')?.className.replace('icon-mask ', ''), b.textContent])])),
        [['Instance journal-lost…', 'Build 1.0 · Not connected, last seen X · owns 2 turns and 1 command', [['Mark instance as gone', 'Mark instance as gone', 'btn-icon btn-danger machine-instance-gone', 'icon-mask-trash', '']]],
          ['Instance journal-live…', 'Build 2.0 · Connected · owns 1 turn and 0 commands', []],
          ['Instance journal-done…', 'Build 0.9 · Gone', []]], 'one icon, on the instance that is neither connected nor gone');
      assert.equal(await page.$eval('.machine-card[data-machine-id="mch_host"]', card => card.querySelectorAll('.machine-instance').length), 0, 'the host lists none');
      // Cancel sends nothing; the dialog names what is lost, whose it is and that it is irreversible.
      await page.$eval('.machine-instance-gone', el => el.click());
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      const text = await page.$eval('#app-dialog', el => el.textContent);
      for (const part of ['Mark instance as gone', 'journal-lost-0123456789', 'Box', '2 open turns', '1 running command', 'vince, nora', 'held for recovery', 'irreversible']) assert.ok(text.includes(part), `the dialog says ${part}: ${text}`);
      assert.equal(await page.$eval('#app-dialog .app-dialog-ok', el => [el.textContent, el.classList.contains('btn-danger')].join()), 'Mark as gone,true');
      await page.$eval('#app-dialog .app-dialog-cancel', el => el.click());
      await settle(page);
      assert.equal((await calls(page, 'POST')).length, 0, 'cancel sends no request');
      // Confirm writes it off and says what happened.
      await page.$eval('.machine-instance-gone', el => el.click());
      await confirmOk(page);
      await page.waitForFunction(() => document.querySelector('#machines-note').textContent.startsWith('Instance marked as gone'));
      assert.equal(await page.$eval('#machines-note', el => el.textContent), 'Instance marked as gone: 2 turns lost, 1 command ended.');
      assert.deepEqual(await calls(page, 'POST'), [['/api/machines/mch_away/instances/journal-lost-0123456789/gone', null]]);
      await page.close();
      // A refusal (the instance connected meanwhile) shows its reason.
      const refused = await mount({ auto: { ...auto, [gonePath]: { status: 409, body: { error: 'instance_connected', message: 'this instance is connected, so it is not lost' } } } });
      await refused.waitForSelector('.machine-instance-gone');
      await refused.$eval('.machine-instance-gone', el => el.click());
      await confirmOk(refused);
      await refused.waitForFunction(() => document.querySelector('#machines-error').textContent.includes('is connected'));
      await refused.close();
    }
    // A revoked machine keeps its instance list and the write-off control (F007): nobody will reconnect it, so its open work is what a
    // write-off exists for.
    {
      const revoked = { id: 'mch_revoked', name: 'Gone box', approved: true, revoked_at: '2026-01-01T00:00:00Z' };
      const lost = { journal_id: 'journal-lost-0123456789', build: '1.0', state: 'active', connected: false, last_seen_at: '2026-02-03T04:05:06Z', turns: 1, commands: 0, agents: [{ id: 'agt_1', slug: 'vince' }] };
      const page = await mount({ auto: { [MACHINES]: { machines: [revoked] }, 'GET /api/machines/mch_revoked/instances': { instances: [lost] } } });
      await page.waitForSelector('.machine-instance');
      assert.equal(await page.$eval('.machine-card[data-machine-id="mch_revoked"] .machine-instance-gone', el => el.title), 'Mark instance as gone', 'a revoked machine still offers the write-off');
      await page.close();
    }
    // An admin who is not the owner sees the machines with pairing, approving and revoking off and the reason on each; stopping stays on.
    {
      const page = await mount({ owner: false });
      await cards(page);
      assert.deepEqual(await page.evaluate(() => ({ add: [document.querySelector('#machine-add').hidden, document.querySelector('#machine-add').disabled, document.querySelector('#machine-add').title],
        locked: [...document.querySelectorAll('.machine-card-actions button:not(.machine-stop-all)')].map(b => [b.disabled, b.title]), stop: [...document.querySelectorAll('.machine-stop-all')].every(b => !b.disabled) })),
        { add: [false, true, 'Only the owner can pair machines'], locked: [[true, 'Only the owner can approve machines'], [true, 'Only the owner can revoke machines'], [true, 'Only the owner can revoke machines']], stop: true });
      assert.equal(await page.$eval('#machines-add-note', el => el.textContent), 'Only the owner can add machines, approve them or revoke them.', 'an admin is told why Add machine is off');
      // "Stop all agents" is a labelled danger button: its words are in it, it is wider than an icon, and it is the danger colour (not a bare square).
      assert.deepEqual(await page.$$eval('.machine-stop-all', nodes => nodes.map(node => [node.textContent.trim(), node.classList.contains('btn-danger'), node.getBoundingClientRect().width > 80, node.title])).then(list => list[0]), ['Stop all agents', true, true, 'Stop all agents on this machine'], 'Stop all agents says what it does inside a danger button');
      await page.close();
      const forbidden = await mount({ auto: { [MACHINES]: { status: 403, body: { message: 'forbidden' } } }, owner: false });
      await forbidden.waitForFunction(() => document.querySelector('#machines-error').textContent === 'Machines are for the owner and the admins.');
      assert.deepEqual(await forbidden.evaluate(() => [document.querySelector('#machine-add').hidden, document.querySelector('#machines-list').textContent]), [true, 'Loading machines…']);
      await forbidden.close();
      const failed = await mount({ auto: { [MACHINES]: { status: 500, body: { message: 'The disk is full' } } } });
      await failed.waitForFunction(() => document.querySelector('#machines-error').textContent === 'The disk is full');
      assert.equal(await failed.$eval('#machine-add', el => el.hidden), false);
      await failed.close();
    }
    // Approve writes and reads again; revoke asks first and a cancel sends nothing; stopping says how it went.
    {
      const page = await mount({ auto: { [MACHINES]: { machines: [host, pending, away] }, 'POST /api/machines/mch_new/approve': {}, 'POST /api/machines/mch_away/revoke': {},
        'POST /api/machines/mch_host/agents/stop': { stopped: ['a'], already_stopped: 2, forbidden: 1, failed: [{ error: 'busy' }] } } });
      await cards(page);
      const reads = async () => (await calls(page, 'GET')).length;
      let before = await reads();
      await page.$eval('[data-machine-id=mch_new] [aria-label=Approve]', el => el.click());
      await page.waitForFunction(() => window.__requests.some(request => request.path === '/api/machines/mch_new/approve'));
      await page.waitForFunction(before => window.__requests.filter(request => request.method === 'GET').length > before, {}, before);
      await page.$eval('[data-machine-id=mch_away] [aria-label=Revoke]', el => el.click());
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      await page.$eval('#app-dialog .app-dialog-cancel', el => el.click());
      await settle(page);
      assert.equal((await calls(page, 'POST')).filter(([path]) => path.endsWith('/revoke')).length, 0, 'a cancelled revoke sends nothing');
      await page.$eval('[data-machine-id=mch_away] [aria-label=Revoke]', el => el.click());
      assert.ok((await page.$eval('#app-dialog', el => el.textContent)).includes('Revoke Box?'));
      await confirmOk(page);
      await page.waitForFunction(() => window.__requests.some(request => request.path === '/api/machines/mch_away/revoke'));
      await page.$eval('.machine-card:first-child .machine-stop-all', el => el.click());
      await confirmOk(page);
      await page.waitForFunction(() => document.querySelector('#machines-note').textContent.length > 0);
      assert.equal(await page.$eval('#machines-note', el => el.textContent), 'Stopped 1 agent, 2 already stopped, 1 left running (not yours to stop), 1 could not be stopped: busy.');
      await page.close();
      const refused = await mount({ auto: { [MACHINES]: { machines: [host, pending] }, 'POST /api/machines/mch_new/approve': { status: 403, body: { message: 'Only the owner' } } } });
      await cards(refused);
      await refused.$eval('[data-machine-id=mch_new] [aria-label=Approve]', el => el.click());
      await refused.waitForFunction(() => document.querySelector('#machines-error').textContent === 'Only the owner');
      await refused.close();
    }
    // A server reached over plain http on another machine says so in the dialog: the warning is shown and the command carries --insecure.
    {
      const warning = 'This server is reached over plain http, so the pairing token would cross the network unencrypted.';
      const page = await mount({ auto: { [MACHINES]: { machines: [host] }, 'POST /api/machine-pairings': { command: "aimebu supervisor --insecure --pair 'http://10.0.0.5:9897' 'amp_x'", warning, insecure_transport: true, expires_at: '2026-02-03T04:05:06Z' } } });
      await cards(page);
      await page.$eval('#machine-add', el => el.click());
      await page.waitForFunction(() => document.querySelector('#machine-pair-dialog').open);
      await page.type('[name=name]', 'Remote');
      await page.$eval('#machine-pair-form button[type=submit]', el => el.click());
      await page.waitForFunction(() => !document.querySelector('#machine-pair-result').hidden);
      assert.deepEqual(await page.evaluate(() => { const note = document.querySelector('#machine-pair-warning'); return [note.hidden, note.textContent, note.getAttribute('role'), document.querySelector('#machine-pair-command').textContent.startsWith('aimebu supervisor --insecure --pair')]; }),
        [false, warning, 'alert', true], 'a plain-http origin is warned about where the command is shown');
      await page.close();
    }
    // Pairing: the dialog opens empty and focused, makes the command, copies it, and a copy that settles last decides what is shown.
    {
      const page = await mount({ auto: { [MACHINES]: { machines: [host] }, 'POST /api/machine-pairings': { command: "aimebu supervisor --pair 'amp_x'", expires_at: '2026-02-03T04:05:06Z' } } });
      await cards(page);
      await page.$eval('#machine-add', el => el.click());
      await page.waitForFunction(() => document.querySelector('#machine-pair-dialog').open);
      assert.deepEqual(await page.evaluate(() => [document.activeElement?.name, document.querySelector('#machine-pair-form').hidden, document.querySelector('#machine-pair-result').hidden, document.querySelector('[name=name]').value, document.querySelector('#machine-pair-error').textContent]), ['name', false, true, '', '']);
      await page.type('[name=name]', ' Remote ');
      await page.$eval('#machine-pair-form button[type=submit]', el => el.click());
      await page.waitForFunction(() => !document.querySelector('#machine-pair-result').hidden);
      assert.deepEqual((await calls(page, 'POST'))[0], ['/api/machine-pairings', { name: 'Remote' }], 'the name is trimmed');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#machine-pair-form').hidden, document.querySelector('#machine-pair-command').textContent, document.querySelector('#machine-pair-expiry').textContent.replace(/Expires .+?\. /, 'Expires X. '),
        document.querySelector('#machine-pair-copy').title, !!document.querySelector('#machine-pair-copy .icon-mask-copy')]), [true, "aimebu supervisor --pair 'amp_x'", 'Expires X. The token can be used once.', 'Copy command', true]);
      await copyWith(page, 'async () => {}');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#machine-pair-copy').title, !!document.querySelector('#machine-pair-copy .icon-mask-check')]), ['Copied', true]);
      await page.waitForFunction(() => document.querySelector('#machine-pair-copy').title === 'Copy command', { timeout: 8000 });
      await copyWith(page, 'async () => {}');
      await copyWith(page, "async () => { throw new Error('denied') }");
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#machine-pair-copy').title, document.querySelector('#machine-pair-error').textContent]), ['Copy command', 'Copy failed. Select the command and copy it manually.'], 'a failed copy resets the button');
      await copyWith(page, 'async () => {}');
      assert.equal(await page.$eval('#machine-pair-error', el => el.textContent), '', 'a copy that works clears the failure');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#machine-pair-warning').hidden, document.querySelector('#machine-pair-warning').textContent]), [true, ''], 'an https or local server gets no warning');
      await page.$eval('#machine-pair-done', el => el.click());
      await page.waitForFunction(() => !document.querySelector('#machine-pair-dialog').open);
      await page.$eval('#machine-add', el => el.click());
      await page.waitForFunction(() => document.querySelector('#machine-pair-dialog').open);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#machine-pair-form').hidden, document.querySelector('#machine-pair-result').hidden, document.querySelector('[name=name]').value]), [false, true, ''], 'it opens fresh each time');
      await page.keyboard.press('Escape');
      await page.waitForFunction(() => !document.querySelector('#machine-pair-dialog').open && !window.__page.store.pairOpen.value);
      await page.$eval('#machine-add', el => el.click());
      await page.waitForFunction(() => document.querySelector('#machine-pair-dialog').open);
      await page.$eval('#machine-pair-cancel', el => el.click());
      await page.waitForFunction(() => !document.querySelector('#machine-pair-dialog').open);
      await page.close();
      const refused = await mount({ auto: { [MACHINES]: { machines: [host] }, 'POST /api/machine-pairings': { status: 403, body: { message: 'Only the owner can pair' } } } });
      await cards(refused);
      await refused.$eval('#machine-add', el => el.click());
      await refused.type('[name=name]', 'Box');
      await refused.$eval('#machine-pair-form button[type=submit]', el => el.click());
      await refused.waitForFunction(() => document.querySelector('#machine-pair-error').textContent === 'Only the owner can pair');
      assert.deepEqual(await refused.evaluate(() => [document.querySelector('#machine-pair-form').hidden, document.querySelector('#machine-pair-form button[type=submit]').disabled]), [false, false]);
      await refused.close();
    }
    // Two reads answered out of order: only the newer one draws.
    {
      const page = await mount({ auto: {}, open: false });
      await page.evaluate(() => { window.__page.open(); window.__page.open(); });
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/machines').length === 2);
      await answer(page, 1, 200, { machines: [host] });
      await answer(page, 0, 200, { machines: [host, away] });
      await settle(page);
      assert.equal(await page.$$eval('#machines-list .machine-card', cards => cards.length), 1);
      await page.close();
    }
    // A page destroyed while a read, a pairing or a stop is held sends nothing more and draws nothing.
    {
      const page = await mount({ auto: {} });
      const read = await waitRequest(page, 'GET', '/api/machines');
      await page.evaluate(() => window.__page.destroy());
      await answer(page, read, 200, { machines: [host] });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [window.__requests.length, document.querySelectorAll('#machines-list *').length, document.querySelector('#settings-machines').children.length]), [1, 0, 0], 'a read answered after destroy draws nothing');
      await page.close();
      const pairing = await mount({ auto: { [MACHINES]: { machines: [host] } } });
      await cards(pairing);
      await pairing.$eval('#machine-add', el => el.click());
      await pairing.type('[name=name]', 'Box');
      await pairing.$eval('#machine-pair-form button[type=submit]', el => el.click());
      const post = await waitRequest(pairing, 'POST', '/api/machine-pairings');
      await pairing.evaluate(() => window.__page.destroy());
      const total = await pairing.evaluate(() => window.__requests.length);
      await answer(pairing, post, 200, { command: 'late', expires_at: '2026-02-03T04:05:06Z' });
      await settle(pairing);
      assert.deepEqual(await pairing.evaluate(() => [window.__requests.length, window.__page.store.pairing.value]), [total, null], 'a pairing answered after destroy sets nothing and reads nothing');
      await pairing.close();
      const stopping = await mount({ auto: { [MACHINES]: { machines: [host] } } });
      await cards(stopping);
      await stopping.$eval('.machine-stop-all', el => el.click());
      await confirmOk(stopping);
      const stop = await waitRequest(stopping, 'POST', '/api/machines/mch_host/agents/stop');
      await stopping.evaluate(() => window.__page.destroy());
      await answer(stopping, stop, 200, { stopped: ['a'] });
      await settle(stopping);
      assert.equal(await stopping.evaluate(() => window.__page.store.note.value), '', 'a stop answered after destroy says nothing');
      await stopping.close();
      const gone = await mount({ auto: { [MACHINES]: { machines: [host] }, 'POST /api/machine-pairings': { command: 'late', expires_at: '2026-02-03T04:05:06Z' }, 'POST /api/machines/mch_host/approve': {} } });
      await cards(gone);
      const clipboard = await gone.evaluate(async () => {
        let writes = 0;
        Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { writes++; } } });
        const { store } = window.__page;
        store.pairing.value = { command: 'x', expires: '' };
        const sent = window.__requests.length;
        window.__page.destroy();
        await store.createPairing(); await store.copyCommand(); await store.change({ id: 'mch_host', name: 'This computer' }, 'approve'); await store.load();
        return { writes, extra: window.__requests.length - sent };
      });
      assert.deepEqual(clipboard, { writes: 0, extra: 0 }, 'a destroyed store sends no pairing, approve or read and copies nothing');
      await gone.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('machines page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

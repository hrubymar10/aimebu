// Settings -> Users in Chromium, with every request held until the test answers it (or answered by a rule), so the order of answers is
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
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css"><link rel="stylesheet" href="/v2-only.css">${importMap}</head>
<body><dialog id="settings-dialog" open><nav></nav></dialog></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const projects = [{ id: 'p1', name: 'Alpha', role: 'owner' }, { id: 'p2', name: 'Beta', role: 'viewer' }, { id: 'p3', name: 'Gamma', role: 'owner' }];
const me = { id: 'usr_me', login: 'owner', owner: true, admin: false };
const bob = { id: 'usr_bob', login: 'bob', admin: false }, amy = { id: 'usr_amy', login: 'amy', admin: true }, off = { id: 'usr_off', login: 'off', disabled: true };
const accounts = [me, bob, amy, off, bob];
const members = [{ id: 'usr_me', login: 'owner', role: 'owner' }, { id: 'usr_bob', login: 'bob', role: 'viewer' }, { id: 'usr_off', login: 'off', role: 'operator', disabled: true }];
const everything = { 'GET /api/users': { global_owner: true, global_admin: true, users: accounts, projects }, 'GET /api/users?project_id=p1': { users: members } };

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
  const mount = async ({ auto = everything, admin = true, open = true, selected = 'p1' } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, auto);
    await page.evaluate(async ({ admin, open, selected, projects, me }) => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: me.id, login: me.login, owner: admin, admin });
      const { UsersSettings } = await import('/js/pages/users/page.js');
      window.__page = new UsersSettings(document.querySelector('#settings-dialog'), () => ({ projects, selectedProjectID: selected }));
      window.__page.section.hidden = false; window.__page.section.classList.add('active');
      if (open) window.__opened = window.__page.open();
    }, { admin, open, selected, projects, me });
    return page;
  };
  const ready = page => page.waitForSelector('#settings-users .user-accounts .user-row[data-user-id]');
  const calls = (page, method) => page.evaluate(method => window.__requests.filter(request => request.method === method).map(request => [request.path, request.body]), method);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 60)));
  const pending = async (page, method, requestPath) => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && request.method === method && request.path === requestPath), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findIndex(request => !request.answered && request.method === method && request.path === requestPath), method, requestPath);
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const confirmOk = async page => { await page.waitForFunction(() => document.querySelector('#app-dialog')?.open); await page.$eval('#app-dialog .app-dialog-ok', el => el.click()); };
  const rowOf = (page, scope, login) => page.evaluateHandle((scope, login) => [...document.querySelectorAll(`#settings-users ${scope} .user-row`)].find(row => row.querySelector('strong').textContent === login), scope, login);
  try {
    // The tab holds its place until the access check answers, then settles visible; the section is made of its blocks in order.
    {
      const page = await mount({ open: false });
      assert.equal(await page.evaluate(() => window.__page.store.tab.value), 'pending', 'the tab waits for the access check');
      await page.evaluate(() => { window.__page.open(); });
      await ready(page);
      assert.equal(await page.evaluate(() => window.__page.store.tab.value), 'shown');
      assert.deepEqual(await page.evaluate(() => [...document.querySelector('#settings-users').children].map(node => `${node.tagName.toLowerCase()}${node.className ? `.${node.className.split(' ')[0]}` : ''}`)),
        ['h3', 'p.error', 'div.settings-group', 'form.user-invite-form', 'input.settings-text-input', 'p.settings-intro', 'div.user-members', 'div.user-accounts']);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#settings-users p.error').getAttribute('role'), [...document.querySelectorAll('#users-project option')].map(o => [o.value, o.textContent]), document.querySelector('#users-project').value,
        document.querySelector('label[for=users-project]').textContent, document.querySelector('#users-project').getAttribute('aria-describedby'), document.querySelector('#users-invite-role').value, document.querySelector('input[aria-label="Invitation link"]').hidden, document.querySelector('.invite-link-hint').hidden]),
        ['alert', [['p1', 'Alpha'], ['p2', 'Beta'], ['p3', 'Gamma']], 'p1', 'Project', 'users-project-desc', 'viewer', true, true]);
      const read = scope => page.$$eval(`#settings-users ${scope} .user-row`, rows => rows.map(row => ({ login: row.querySelector('strong').textContent, off: row.querySelector('span').textContent, badge: row.querySelector('.user-badge')?.textContent || null, toggle: row.querySelector('.user-admin-toggle input')?.checked ?? null,
        buttons: [...row.querySelectorAll('button')].map(b => b.textContent || b.getAttribute('aria-label')), select: row.querySelector('select')?.value ?? null })));
      assert.deepEqual(await read('.user-members'), [
        { login: 'owner', off: '', badge: null, toggle: null, buttons: ['You cannot remove yourself from the project'], select: 'owner' },
        { login: 'bob', off: '', badge: null, toggle: null, buttons: ['Remove bob from this project'], select: 'viewer' },
        { login: 'off', off: 'Deactivated', badge: null, toggle: null, buttons: ['Remove off from this project'], select: 'operator' }]);
      assert.deepEqual(await read('.user-accounts'), [
        { login: 'owner', off: '', badge: 'Owner', toggle: null, buttons: ['Password reset link'], select: null },
        { login: 'bob', off: '', badge: null, toggle: false, buttons: ['Password reset link', 'Deactivate'], select: null },
        { login: 'amy', off: '', badge: null, toggle: true, buttons: ['Password reset link', 'Deactivate'], select: null },
        { login: 'off', off: 'Deactivated', badge: null, toggle: null, buttons: [], select: null }], 'an account is listed once; the owner is never deactivated and a deactivated account has no actions');
      assert.deepEqual(await page.$$eval('#settings-users h4', nodes => nodes.map(node => node.textContent)), ['Project members', 'Accounts']);
      assert.deepEqual(await page.$eval('#settings-users .user-members .btn-icon', el => [el.title, el.querySelector('.icon-mask-trash') !== null, el.classList.contains('btn-danger')]), ['You cannot remove yourself from the project', true, true]);
      assert.deepEqual(await page.$$eval('#settings-users .user-members .btn-icon', els => els.map(el => [getComputedStyle(el).opacity < 0.6, getComputedStyle(el).cursor])), [[true, 'not-allowed'], [false, 'pointer'], [false, 'pointer']], 'a remove button that cannot be used looks off, and one that can does not');
      assert.deepEqual(await page.$$eval('#settings-users .user-members .btn-icon', els => els.map(el => el.disabled)), [true, false, false], 'your own row (and the last owner) cannot be removed; the others can');
      await page.close();
    }
    // Choosing another project reads its members; a role change and a removal write, then read the lists again.
    {
      const page = await mount({ auto: { ...everything, 'GET /api/users?project_id=p3': { users: [members[0]] }, 'PUT /api/projects/p1/members/usr_bob': {}, 'DELETE /api/projects/p1/members/usr_off': {} } });
      await ready(page);
      await page.select('#users-project', 'p3');
      await page.waitForFunction(() => document.querySelectorAll('#settings-users .user-members .user-row').length === 1);
      await page.select('#users-project', 'p1');
      await page.waitForFunction(() => document.querySelectorAll('#settings-users .user-members .user-row').length === 3);
      const bobRow = await rowOf(page, '.user-members', 'bob');
      const readsBefore = (await calls(page, 'GET')).length;
      await bobRow.$eval('select', el => { el.value = 'operator'; el.dispatchEvent(new Event('change', { bubbles: true })); });
      await page.waitForFunction(before => window.__requests.some(request => request.method === 'PUT') && window.__requests.filter(request => request.method === 'GET').length >= before + 2, {}, readsBefore);
      assert.deepEqual((await calls(page, 'PUT'))[0], ['/api/projects/p1/members/usr_bob', { role: 'operator' }]);
      const offRow = await rowOf(page, '.user-members', 'off');
      await offRow.$eval('.btn-icon', el => el.click());
      assert.ok((await page.$eval('#app-dialog', el => el.textContent)).includes('Remove off from this project?'));
      await confirmOk(page);
      await page.waitForFunction(() => window.__requests.some(request => request.method === 'DELETE'));
      assert.deepEqual((await calls(page, 'DELETE'))[0], ['/api/projects/p1/members/usr_off', null]);
      await page.close();
    }
    // A refused change says why and puts the control back; a refused admin switch flips back.
    {
      const page = await mount({ auto: { ...everything, 'PUT /api/projects/p1/members/usr_bob': { status: 403, body: { message: 'Only an owner may change roles' } }, 'PUT /api/users/usr_bob/admin': { status: 500, body: { message: 'Nope' } } } });
      await ready(page);
      const bobRow = await rowOf(page, '.user-members', 'bob');
      await bobRow.$eval('select', el => { el.value = 'owner'; el.dispatchEvent(new Event('change', { bubbles: true })); });
      await page.waitForFunction(() => document.querySelector('#settings-users p.error').textContent === 'Only an owner may change roles');
      assert.equal(await bobRow.$eval('select', el => el.value), 'viewer', 'the refused role is put back');
      const accountRow = await rowOf(page, '.user-accounts', 'bob');
      await accountRow.$eval('.user-admin-toggle input', el => el.click());
      await confirmOk(page);
      await page.waitForFunction(() => document.querySelector('#settings-users p.error').textContent === 'Nope');
      assert.equal(await accountRow.$eval('.user-admin-toggle input', el => el.checked), false, 'the refused admin switch flips back');
      assert.deepEqual((await calls(page, 'PUT')).at(-1), ['/api/users/usr_bob/admin', { admin: true }]);
      await page.close();
    }
    // Granting and taking away admin are confirmed first, naming the person and what an admin can do; cancelling puts the box back and sends nothing.
    {
      const page = await mount({ auto: { ...everything, 'PUT /api/users/usr_bob/admin': {}, 'PUT /api/users/usr_amy/admin': {} } });
      await ready(page);
      const box = async (who) => (await rowOf(page, '.user-accounts', who)).$('.user-admin-toggle input');
      const text = () => page.$eval('#app-dialog', el => el.textContent);
      const bobBox = await box('bob');
      await bobBox.click();
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      const grant = await text();
      assert.ok(grant.includes('Make bob an admin?') && /roles, prompts, macros and memory/.test(grant) && /create projects/.test(grant) && /cannot reach your own account/.test(grant), `names the person and what an admin can do: ${grant}`);
      assert.equal(await page.$eval('#app-dialog .app-dialog-ok', el => el.textContent), 'Make admin');
      assert.equal(await bobBox.evaluate(el => el.checked), true, 'the box shows the choice while it is asked');
      await page.$eval('#app-dialog .app-dialog-cancel', el => el.click());
      await page.waitForFunction(() => !document.querySelector('#app-dialog')?.open);
      assert.equal(await bobBox.evaluate(el => el.checked), false, 'cancel puts the box back');
      assert.equal((await calls(page, 'PUT')).length, 0, 'a cancelled grant sends nothing');
      await bobBox.click(); await confirmOk(page);
      await page.waitForFunction(() => window.__requests.some(request => request.method === 'PUT' && request.path === '/api/users/usr_bob/admin'));
      assert.deepEqual((await calls(page, 'PUT')).at(-1), ['/api/users/usr_bob/admin', { admin: true }], 'confirming sends it');
      const amyBox = await box('amy');
      assert.equal(await amyBox.evaluate(el => el.checked), true);
      await amyBox.click();
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      const revoke = await text();
      assert.ok(revoke.includes('Remove admin from amy?') && /ordinary account/.test(revoke) && /memberships stay/.test(revoke), `revoking says what is lost: ${revoke}`);
      await page.$eval('#app-dialog .app-dialog-cancel', el => el.click());
      await page.waitForFunction(() => !document.querySelector('#app-dialog')?.open);
      assert.equal(await amyBox.evaluate(el => el.checked), true, 'cancel keeps the admin');
      const before = (await calls(page, 'PUT')).length;
      await amyBox.click(); await confirmOk(page);
      await page.waitForFunction(count => window.__requests.filter(request => request.method === 'PUT').length > count, {}, before);
      assert.deepEqual((await calls(page, 'PUT')).at(-1), ['/api/users/usr_amy/admin', { admin: false }], 'confirming a revoke sends it');
      await page.close();
    }
    // Accounts: reset shows the link (selected), a deactivation is confirmed first.
    {
      const page = await mount({ auto: { ...everything, 'POST /api/invites': { url: 'https://aimebu.example/#invite=aui_reset' }, 'POST /api/users/usr_bob/deactivate': {} } });
      await ready(page);
      const bobRow = await rowOf(page, '.user-accounts', 'bob');
      await bobRow.$$eval('button', buttons => buttons.find(b => b.textContent === 'Password reset link').click());
      await page.waitForFunction(() => !document.querySelector('input[aria-label="Invitation link"]').hidden);
      assert.deepEqual(await page.evaluate(() => { const input = document.querySelector('input[aria-label="Invitation link"]'); return [input.value, input.readOnly, document.activeElement === input || input.selectionEnd - input.selectionStart === input.value.length, document.querySelector('.invite-link-hint').hidden]; }),
        ['https://aimebu.example/#invite=aui_reset', true, true, true]);
      assert.deepEqual((await calls(page, 'POST'))[0], ['/api/invites', { kind: 'reset', target_user_id: 'usr_bob' }]);
      assert.equal(await bobRow.$$eval('button', buttons => buttons.find(b => b.textContent === 'Deactivate').classList.contains('btn-danger')), true, 'Deactivate is a danger action');
      await bobRow.$$eval('button', buttons => buttons.find(b => b.textContent === 'Deactivate').click());
      assert.ok((await page.$eval('#app-dialog', el => el.textContent)).includes('Deactivate bob across all projects?'));
      await page.$eval('#app-dialog .app-dialog-cancel', el => el.click());
      await settle(page);
      assert.equal((await calls(page, 'POST')).length, 1, 'a cancelled confirmation sends nothing');
      await bobRow.$$eval('button', buttons => buttons.find(b => b.textContent === 'Deactivate').click());
      await confirmOk(page);
      await page.waitForFunction(() => window.__requests.some(request => request.path === '/api/users/usr_bob/deactivate'));
      await page.close();
    }
    // An invitation is made for the chosen project and role; a link for this computer says it only works here.
    {
      const page = await mount({ auto: { ...everything, 'POST /api/invites': { url: 'http://localhost:8080/#invite=aui_join' } } });
      await ready(page);
      await page.select('#users-invite-role', 'operator');
      await page.$eval('.user-invite-form button', el => el.click());
      await page.waitForFunction(() => !document.querySelector('input[aria-label="Invitation link"]').hidden);
      assert.deepEqual((await calls(page, 'POST'))[0], ['/api/invites', { kind: 'join', project_id: 'p1', role: 'operator' }]);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('input[aria-label="Invitation link"]').value, document.querySelector('.invite-link-hint').hidden, document.querySelector('.invite-link-hint').textContent]),
        ['http://localhost:8080/#invite=aui_join', false, 'This address (localhost) only works on this computer. For someone else, replace it with an address they can reach: keep everything from "/#invite=" on.']);
      await page.close();
      const refused = await mount({ auto: { ...everything, 'POST /api/invites': { status: 403, body: { message: 'Not yours' } } } });
      await ready(refused);
      await refused.$eval('.user-invite-form button', el => el.click());
      await refused.waitForFunction(() => document.querySelector('#settings-users p.error').textContent === 'Not yours');
      await refused.close();
    }
    // Not an admin: only the projects the person owns are asked about, the tab is theirs only if one answers, and no accounts show.
    {
      const own = { 'GET /api/users': { status: 403, body: { message: 'forbidden' } }, 'GET /api/users?project_id=p1': { users: members }, 'GET /api/users?project_id=p3': { status: 403, body: { message: 'forbidden' } } };
      const page = await mount({ auto: own, admin: false });
      await page.waitForSelector('#settings-users .user-members .user-row');
      assert.deepEqual(await page.evaluate(() => [[...document.querySelectorAll('#users-project option')].map(o => o.value), window.__page.store.tab.value, document.querySelectorAll('#settings-users .user-accounts *').length]), [['p1'], 'shown', 0]);
      assert.deepEqual((await page.evaluate(() => window.__requests.map(request => request.path))).filter(item => item.includes('p2')), [], 'a project the person does not own is not asked about');
      await page.close();
      const nobody = await mount({ auto: { 'GET /api/users': { status: 403, body: { message: 'forbidden' } }, 'GET /api/users?project_id=p1': { status: 403, body: { message: 'forbidden' } }, 'GET /api/users?project_id=p3': { status: 403, body: { message: 'forbidden' } } }, admin: false });
      await nobody.waitForFunction(() => window.__page.store.tab.value !== 'pending');
      assert.equal(await nobody.evaluate(() => window.__page.store.tab.value), 'hidden');
      await nobody.close();
    }
    // The open project of the app is the picker's choice when the person manages it; an answer that fails to arrive hides the tab.
    {
      const page = await mount({ selected: 'p3', auto: { ...everything, 'GET /api/users?project_id=p3': { users: [members[0]] } } });
      await page.waitForFunction(() => document.querySelectorAll('#settings-users .user-members .user-row').length === 1);
      assert.equal(await page.$eval('#users-project', el => el.value), 'p3');
      await page.close();
    }
    // Two access checks answered out of order: the older caller gets the newer answer, and the lists are read once for this open.
    {
      const page = await mount({ auto: {}, open: false });
      await page.evaluate(() => { window.__both = Promise.all([window.__page.prepare(), window.__page.prepare()]); });
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/users').length === 2);
      await answer(page, 1, 200, { global_owner: true, global_admin: true, users: accounts, projects: [projects[0]] });
      await answer(page, 0, 200, { global_owner: true, global_admin: true, users: accounts, projects });
      assert.deepEqual(await page.evaluate(async () => (await window.__both).map(access => access.hidden)), [false, false]);
      assert.deepEqual(await page.$$eval('#users-project option', options => options.map(option => option.value)), ['p1'], 'the newer answer is the one drawn');
      await page.close();
    }
    // A page destroyed while its reads are held sends no further read or write and draws nothing.
    {
      const page = await mount({ auto: {} });
      const access = await pending(page, 'GET', '/api/users');
      await page.evaluate(() => window.__page.destroy());
      await answer(page, access, 200, { global_owner: true, global_admin: true, users: accounts, projects });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [window.__requests.length, document.querySelectorAll('#settings-users *').length, window.__page.store.tab.value]), [1, 0, 'pending'], 'no read after destroy and nothing drawn');
      await page.close();
      const loading = await mount({ auto: { 'GET /api/users': { global_owner: true, global_admin: true, users: accounts, projects } } });
      const read = await pending(loading, 'GET', '/api/users?project_id=p1');
      await loading.evaluate(() => window.__page.destroy());
      await answer(loading, read, 200, { users: members });
      await settle(loading);
      assert.equal(await loading.evaluate(() => window.__requests.filter(request => request.path === '/api/users').length), 1, 'the accounts are not read once the page is gone');
      await loading.close();
      const writing = await mount({ auto: everything });
      await ready(writing);
      await writing.evaluate(() => { document.querySelector('.user-invite-form button').click(); });
      const post = await pending(writing, 'POST', '/api/invites');
      await writing.evaluate(() => window.__page.destroy());
      const total = await writing.evaluate(() => window.__requests.length);
      await answer(writing, post, 200, { url: 'http://localhost:8080/#invite=aui_late' });
      await settle(writing);
      assert.deepEqual(await writing.evaluate(() => [window.__requests.length, window.__page.store.link.value, document.querySelectorAll('#settings-users *').length]), [total, null, 0], 'an invitation answered after destroy sets no link and sends nothing');
      await writing.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('users page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

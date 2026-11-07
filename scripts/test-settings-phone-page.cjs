// The settings dialog's phone layout in Chromium, on the real markup and styles: a member is one row, a role card keeps its edit button on the title's line and
// its key whole, the list of harnesses is a column, and a select cut by its column says so with an ellipsis. The sections' own markup is what their pages draw
// (their tests check the class names); this checks what the styles do with it. web/ is served as is; no Go server. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const fixture = fs.readFileSync(path.join(root, 'index.html'), 'utf8').replace(/<script type="module" src="\/js\/app\.js[^"]*"><\/script>/, '');
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const sections = {
  users: `<div class="user-row user-member-row"><strong>a-rather-long-login-name-that-has-to-give-way</strong><span></span><select class="settings-select" aria-label="Role"><option>owner</option></select><button type="button" class="btn-icon btn-danger" aria-label="Remove"><span class="icon-mask icon-mask-trash"></span></button></div>`,
  roles: `<div class="role-cards"><div class="role-card"><div class="role-card-head"><span class="role-card-emoji">🛡</span><strong>Security reviewer</strong><span class="role-card-key">sec-reviewer</span></div>
    <div class="role-card-chips"><span class="chip">many holders</span><span class="chip">extends Reviewer</span><span class="chip">built in</span></div><p class="role-card-summary">Additional focus: authorization boundaries, injection, unsafe file or network access.</p>
    <button type="button" class="btn-icon role-card-edit" aria-label="Edit"><span class="icon-mask icon-mask-edit"></span></button></div></div>`,
  harnesses: `<div class="harness-split"><div class="harness-master"><button type="button" class="harness-master-item"><span class="harness-master-name">Claude Code (Docker)</span></button><button type="button" class="harness-master-item"><span class="harness-master-name">Codex (Docker)</span></button><button type="button" class="harness-master-item"><span class="harness-master-name">Pi (Docker)</span></button></div><div class="machine-harness">detail</div></div>
    <select id="long-select"><option>Studio-Mac-with-a-very-long-machine-name-that-is-cut.local</option></select>`,
};

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  try {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    await page.setViewport({ width: 390, height: 844 });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(sections => {
      const dialog = document.querySelector('#settings-dialog');
      dialog.setAttribute('open', ''); dialog.dataset.view = 'section';
      for (const [key, markup] of Object.entries(sections)) { const section = document.querySelector(`#settings-${key}`) || Object.assign(document.createElement('section'), { id: `settings-${key}`, className: 'settings-section' }); if (!section.parentElement) document.querySelector('.settings-body').append(section); section.innerHTML = markup; }
    }, sections);
    const show = key => page.evaluate(key => { for (const section of document.querySelectorAll('.settings-body > section')) { section.hidden = section.id !== `settings-${key}`; section.classList.toggle('active', section.id === `settings-${key}`); } document.querySelector('#settings-dialog').dataset.section = key; }, key);
    const tops = (selector) => page.$$eval(selector, nodes => nodes.map(node => { const rect = node.getBoundingClientRect(); return { top: Math.round(rect.top), bottom: Math.round(rect.bottom), left: Math.round(rect.left), right: Math.round(rect.right), height: Math.round(rect.height) }; }));

    // Users: the name gives way (with an ellipsis); the role and the remove button stay, on the same row, inside the card.
    await show('users');
    const [name] = await tops('.user-member-row strong'), [role] = await tops('.user-member-row select'), [remove] = await tops('.user-member-row button'), [row] = await tops('.user-member-row');
    assert.ok(Math.abs(name.top + name.height / 2 - (role.top + role.height / 2)) <= 4 && Math.abs(role.top + role.height / 2 - (remove.top + remove.height / 2)) <= 4, `name, role and remove share one row: ${JSON.stringify({ name, role, remove })}`);
    assert.ok(remove.right <= row.right && role.right <= row.right && name.right <= role.left, `and none leaves the card: ${JSON.stringify({ row, name, role, remove })}`);
    assert.deepEqual(await page.$eval('.user-member-row strong', node => { const style = getComputedStyle(node); return [style.textOverflow, style.whiteSpace, node.scrollWidth > node.clientWidth]; }), ['ellipsis', 'nowrap', true], 'a long login is cut with an ellipsis');

    // Roles: the edit button is on the title's line, the chips and the summary are below it across the whole card, and the key is one word.
    await show('roles');
    const [edit] = await tops('.role-card-edit'), [head] = await tops('.role-card-head'), [chips] = await tops('.role-card-chips'), [summary] = await tops('.role-card-summary'), [card] = await tops('.role-card'), [key] = await tops('.role-card-key');
    assert.ok(edit.top < chips.top && edit.bottom <= chips.top + 2, `the edit button sits on the title's line, above the chips: ${JSON.stringify({ edit, chips })}`);
    assert.ok(chips.right > edit.left && summary.right > edit.left, `the chips and the summary run under it, across the card: ${JSON.stringify({ edit, chips, summary })}`);
    assert.ok(key.height < 24 && await page.$eval('.role-card-key', node => getComputedStyle(node).whiteSpace) === 'nowrap', `a role key does not break at its hyphen: ${JSON.stringify(key)}`);
    assert.ok(head.right <= edit.left + 1, 'the title does not run under the edit button');

    // Harnesses: the list is a column of full-width buttons; a select that is cut says so with an ellipsis and stays inside the dialog.
    await show('harnesses');
    const items = await tops('.harness-master-item');
    assert.ok(items.length === 3 && items.every((item, index) => item.left === items[0].left && item.right === items[0].right && (index === 0 || item.top >= items[index - 1].bottom)), `the harnesses are a column: ${JSON.stringify(items)}`);
    assert.deepEqual(await page.$eval('#long-select', node => { const style = getComputedStyle(node), rect = node.getBoundingClientRect(); return [style.textOverflow, rect.right <= innerWidth]; }), ['ellipsis', true]);
    assert.deepEqual(errors, [], 'no page errors');
    console.log('settings phone page: ok');
  } finally { await browser.close(); server.close(); }
}
main().catch(error => { console.error(error); process.exit(1); });

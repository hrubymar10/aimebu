// The settings dialog's header, navigation and category head in Chromium: the sections and categories, what the account may see, the selection, the Users
// category's wait, the pages' visibility and the General page's blocks. web/ is served as is with its import map; no Go server. Part of `make test-ui`.
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
const sections = ['general', 'usages', 'notifications', 'telemetry', 'roles', 'users', 'prompts', 'macros', 'machines', 'harnesses'];
const blocks = ['settings-appearance', 'chat-preferences', 'message-retention', 'agent-isolation', 'prompt-cache', 'settings-backup'];
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css"><link rel="stylesheet" href="/v2-only.css">${importMap}</head>
<body><dialog id="settings-dialog" class="settings-modal settings-shell" aria-label="Settings" open><div class="settings-pane"><nav class="settings-nav" aria-label="Settings sections"></nav><div class="settings-body"><header class="settings-body-header"></header>${sections.map(key => `<section id="settings-${key}" class="settings-section">${key === 'general' ? blocks.map(id => `<div class="settings-subsection" id="${id}">${id}</div>`).join('') : ''}</section>`).join('')}</div></div></dialog></body></html>`;
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
  const mount = async () => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async sections => {
      const { signal } = await import('/js/ui.js');
      const { SettingsShell } = await import('/js/pages/shell/page.js');
      const dialog = document.querySelector('#settings-dialog');
      window.__selected = []; window.__closed = 0; window.__backs = 0;
      const users = window.__users = signal('pending');
      window.__shell = new SettingsShell(dialog, Object.fromEntries(sections.map(key => [key, document.querySelector(`#settings-${key}`)])),
        { users, onSelect: path => { window.__selected.push(path); window.__shell.store.apply(path); }, onClose: () => { window.__closed++; }, onBack: () => { window.__backs++; window.__shell.store.showList(); } });
    }, sections);
    return page;
  };
  // The nav's captions and categories in order: a caption as `<section>-title`, a category by its id, the selected one starred.
  const nav = page => page.evaluate(() => [...document.querySelectorAll('#settings-dialog nav .settings-nav-title[id], #settings-dialog nav .settings-nav-item')].map(node => `${node.id.replace(/^settings-/, '').replace(/-tab$/, '')}${node.classList.contains('active') ? '*' : ''}`));
  const shown = page => page.evaluate(() => [...document.querySelectorAll('#settings-dialog .settings-body > section')].filter(node => !node.hidden).map(node => node.id.replace('settings-', '')));
  const blocksShown = page => page.evaluate(() => [...document.querySelectorAll('#settings-general > .settings-subsection')].filter(node => getComputedStyle(node).display !== 'none').map(node => node.id));
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 30)));
  const asOwner = async page => { await page.evaluate(() => { window.__shell.store.setUser('usr'); window.__shell.store.setRights({ owner: true, admin: false }); window.__users.value = 'shown'; }); await settle(page); };
  const rights = async (page, value) => { await page.evaluate(value => window.__shell.store.setRights(value), value); await settle(page); };
  const everything = ['personal-title', 'appearance*', 'chat', 'notifications', 'server-title', 'users', 'usages', 'telemetry', 'storage', 'backup', 'agents-title', 'roles', 'prompts', 'macros', 'harnesses', 'machines', 'isolation'];
  const without = (list, ...ids) => list.filter(item => !ids.includes(item.replace('*', '')));
  try {
    // The sections in order with their categories, Appearance selected; the title, the category's head and the close button.
    {
      const page = await mount();
      await asOwner(page);
      assert.deepEqual(await nav(page), everything, 'Personal, Server and Agents with their categories');
      assert.deepEqual(await page.evaluate(() => {
        const dialog = document.querySelector('#settings-dialog'), navigation = dialog.querySelector('nav'), close = document.querySelector('#settings-close'), head = document.querySelector('.settings-cat-head');
        return { captions: [...navigation.querySelectorAll('.settings-nav-title[id]')].map(node => node.textContent), title: document.querySelector('#settings-title').textContent, current: [...navigation.querySelectorAll('[aria-current]')].map(node => node.id),
          icons: [...navigation.querySelectorAll('.settings-nav-item')].every(node => node.querySelector('svg.settings-icon')), tooltip: navigation.querySelector('#settings-chat-tab').title,
          head: [head.querySelector('.settings-cat-name').textContent, head.querySelector('.settings-cat-desc').textContent, !!head.querySelector('svg')], subs: document.querySelectorAll('.settings-sub-row').length,
          close: [close.className, close.title, !!close.querySelector('.icon-mask-close')], dataset: [dialog.dataset.section, dialog.dataset.category, dialog.dataset.view] };
      }), { captions: ['Personal', 'Server', 'Agents'], title: 'Appearance', current: ['settings-appearance-tab'], icons: true, tooltip: 'Chat', head: ['Appearance', 'Theme and the room list preview', true], subs: 0,
        close: ['btn-icon settings-close-btn', 'Close settings', true], dataset: ['general', 'appearance', 'section'] });
      assert.deepEqual(await shown(page), ['general'], 'only the category\'s page shows');
      assert.deepEqual(await blocksShown(page), ['settings-appearance'], 'and of the General page only the Appearance block');
      await page.$eval('#settings-close', el => el.click());
      assert.equal(await page.evaluate(() => window.__closed), 1);
      await page.close();
    }
    // What the account may see: the owner everything; an admin everything too (Usages read-only); anyone else their own settings, Telemetry and the General blocks that
    // were always theirs, none of the instance's; a refused usage read takes Accounts & usage; the Users category holds its place while it waits, then shows or goes.
    {
      const page = await mount();
      assert.equal(await page.$eval('#settings-users-tab', el => [el.classList.contains('settings-tab-pending'), el.getAttribute('aria-hidden'), el.tabIndex].join()), 'true,true,-1', 'Users waits for the access check');
      await asOwner(page);
      assert.equal(await page.$eval('#settings-users-tab', el => el.hasAttribute('aria-hidden') || el.classList.contains('settings-tab-pending')), false);
      await rights(page, { owner: false, admin: true });
      assert.deepEqual(await nav(page), everything, 'an admin has every category');
      await rights(page, { owner: false, admin: false });
      assert.deepEqual(await nav(page), ['personal-title', 'appearance*', 'chat', 'notifications', 'server-title', 'users', 'telemetry', 'storage', 'agents-title', 'isolation'], 'anyone else loses the instance categories (Agent isolation, which General always showed them read-only, stays)');
      await rights(page, { owner: true, admin: false });
      await page.evaluate(() => window.__shell.store.hideUsages());
      await settle(page);
      assert.deepEqual(await nav(page), without(everything, 'usages'), 'a refused usage read takes Accounts & usage away');
      await page.evaluate(() => window.__shell.store.setUser());
      await settle(page);
      assert.deepEqual(await nav(page), everything, 'signing in shows it again');
      await page.evaluate(() => { window.__users.value = 'hidden'; });
      await settle(page);
      assert.equal((await nav(page)).includes('users'), false, 'a refused access check takes Users away');
      await page.close();
    }
    // Choosing a category: its page shows alone (and of General only its blocks), the title, the head and the dialog follow, and the shell tells the page what was chosen.
    {
      const page = await mount();
      await asOwner(page);
      await page.$eval('#settings-machines-tab', el => el.click());
      await settle(page);
      assert.deepEqual(await page.evaluate(() => ({ asked: window.__selected, title: document.querySelector('#settings-title').textContent, head: document.querySelector('.settings-cat-name').textContent,
        current: [...document.querySelectorAll('#settings-dialog nav [aria-current]')].map(n => n.id), dataset: document.querySelector('#settings-dialog').dataset.section })),
      { asked: ['settings/agents/machines'], title: 'Machines', head: 'Machines', current: ['settings-machines-tab'], dataset: 'machines' });
      assert.deepEqual(await shown(page), ['machines']);
      for (const [id, blocks] of [['chat', ['chat-preferences']], ['storage', ['message-retention', 'prompt-cache']], ['isolation', ['agent-isolation']], ['backup', ['settings-backup']], ['appearance', ['settings-appearance']]]) {
        await page.$eval(`#settings-${id}-tab`, el => el.click());
        await settle(page);
        assert.deepEqual([await shown(page), await blocksShown(page)], [['general'], blocks], `${id} shows only its blocks of the General page`);
      }
      await page.close();
    }
    // A category with subcategories lists them as rows with a chevron; a row asks for its own path.
    {
      const page = await mount();
      await asOwner(page);
      await page.$eval('#settings-storage-tab', el => el.click());
      await settle(page);
      assert.deepEqual(await page.$$eval('.settings-sub-row', rows => rows.map(row => [row.textContent, !!row.querySelector('svg'), row.title])), [['Message expiry', true, 'Message expiry'], ['Prompt cache', true, 'Prompt cache']]);
      await page.$eval('.settings-sub-row:last-child', el => el.click());
      assert.equal(await page.evaluate(() => window.__selected.at(-1)), 'settings/server/storage/prompt-cache');
      await page.close();
    }
    // A category whose entry is not shown is never the selection: Accounts & usage for someone who is neither owner nor admin opens the first category, not a blank
    // page; rights that take the selected category away put the selection back on Appearance; one the account may have is kept.
    {
      const page = await mount();
      const state = () => page.evaluate(() => ({ category: window.__shell.store.category.value, title: document.querySelector('#settings-title').textContent, shown: [...document.querySelectorAll('#settings-dialog .settings-body > section')].filter(node => !node.hidden).map(node => node.id) }));
      await rights(page, { owner: false, admin: true });
      await page.evaluate(() => window.__shell.store.select('usages'));
      assert.equal((await state()).category, 'usages', 'an admin has Accounts & usage (read-only)');
      await rights(page, { owner: false, admin: false });
      assert.deepEqual(await state(), { category: 'appearance', title: 'Appearance', shown: ['settings-general'] }, 'rights that take the selected category away put the selection on Appearance');
      await page.evaluate(() => window.__shell.store.select('usages'));
      await settle(page);
      assert.deepEqual(await state(), { category: 'users', title: 'Settings', shown: ['settings-users'] }, 'someone who is neither asks for it and gets the list (a wide screen shows the first category of its section)');
      await rights(page, { owner: false, admin: true });
      await page.evaluate(() => window.__shell.store.select('roles'));
      assert.equal((await state()).category, 'roles', 'a category the admin may have is kept');
      await page.evaluate(() => { window.__shell.store.setRights({ owner: true, admin: true }); window.__shell.store.select('usages'); window.__shell.store.hideUsages(); });
      await settle(page);
      assert.equal((await state()).category, 'appearance', 'a refused usage read moves the selection off Accounts & usage');
      await page.close();
    }
    // One width for every category on a desktop: the pane never grows over the top bar.
    {
      const page = await mount();
      await rights(page, { owner: true, admin: true });
      const widths = [];
      for (const key of ['appearance', 'harnesses', 'roles', 'machines']) {
        await page.evaluate(key => window.__shell.store.select(key), key); await settle(page);
        widths.push(await page.$eval('#settings-dialog .settings-pane', pane => Math.round(pane.getBoundingClientRect().width)));
      }
      assert.deepEqual([...new Set(widths)].length, 1, `one width across the categories: ${widths}`);
      await page.close();
    }
    // A phone shows the list of categories or one category: the list first (full screen, with its own close button), a category with a back arrow and a centred title (and
    // no list); a wider screen shows both and neither of those two buttons.
    {
      const page = await mount();
      await page.setViewport({ width: 390, height: 844 });
      await asOwner(page);
      await page.evaluate(() => window.__shell.store.showList());
      await settle(page);
      const phone = () => page.evaluate(() => {
        const visible = node => !!node && getComputedStyle(node).display !== 'none' && node.getBoundingClientRect().width > 0;
        const navigation = document.querySelector('#settings-dialog nav'), body = document.querySelector('#settings-dialog .settings-body'), pane = document.querySelector('.settings-pane').getBoundingClientRect();
        const title = document.querySelector('#settings-title').getBoundingClientRect(), back = document.querySelector('#settings-back').getBoundingClientRect();
        return { view: document.querySelector('#settings-dialog').dataset.view, nav: visible(navigation), navFull: navigation.getBoundingClientRect().width >= pane.width - 2, body: visible(body), back: visible(document.querySelector('#settings-back')),
          navClose: visible(document.querySelector('#settings-nav-close')), close: visible(document.querySelector('#settings-close')), pane: Math.round(pane.width), centred: visible(body) ? Math.abs(title.left + title.width / 2 - (pane.left + pane.width / 2)) <= 2 : null, backLeft: visible(body) ? back.left < title.left : null,
          chevrons: [...document.querySelectorAll('.settings-nav-chevron')].every(node => getComputedStyle(node).display !== 'none') };
      });
      assert.deepEqual(await phone(), { view: 'list', nav: true, navFull: true, body: false, back: false, navClose: true, close: false, pane: 390, centred: null, backLeft: null, chevrons: true }, 'the list fills the screen, with its own close button and a chevron on each row');
      await page.$eval('#settings-roles-tab', node => node.click()); await settle(page);
      assert.deepEqual(await phone(), { view: 'section', nav: false, navFull: false, body: true, back: true, navClose: false, close: true, pane: 390, centred: true, backLeft: true, chevrons: true }, 'a category fills the screen, with a way back and a centred title');
      assert.equal(await page.$eval('#settings-title', node => node.textContent), 'Roles');
      await page.$eval('#settings-back', node => node.click()); await settle(page);
      assert.equal((await phone()).view, 'list', 'back returns to the list');
      assert.equal(await page.evaluate(() => [window.__closed, window.__backs]).then(String), '0,1');
      await page.$eval('#settings-nav-close', node => node.click());
      assert.equal(await page.evaluate(() => window.__closed), 1, 'the list closes the dialog');
      await page.setViewport({ width: 1280, height: 800 });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => ({ nav: getComputedStyle(document.querySelector('#settings-dialog nav')).display !== 'none', body: getComputedStyle(document.querySelector('.settings-body')).display !== 'none',
        back: getComputedStyle(document.querySelector('#settings-back')).display !== 'none', navClose: getComputedStyle(document.querySelector('#settings-nav-close')).display !== 'none', navWidth: Math.round(document.querySelector('#settings-dialog nav').getBoundingClientRect().width) })),
      { nav: true, body: true, back: false, navClose: false, navWidth: 240 }, 'a wide screen shows both columns');
      await page.close();
    }
    // Search: the sections give way to results, each a name over its path; a word that matches nothing says so; what the account may not open is not offered;
    // Enter opens the first result and a click opens its own; Escape clears the box.
    {
      const page = await mount();
      await asOwner(page);
      const results = () => page.$$eval('.settings-result', rows => rows.map(row => [row.querySelector('.settings-result-name').textContent, row.querySelector('.settings-result-path').textContent]));
      const type = async text => { await page.$eval('#settings-search', (input, text) => { input.value = text; input.dispatchEvent(new Event('input', { bubbles: true })); }, text); await settle(page); };
      await type('mentions');
      assert.deepEqual(await results(), [['Mentions', 'Agents \u203a Prompts \u203a Mentions']], 'a setting of a category nobody opened, with its path');
      assert.equal(await page.$('.settings-nav-section'), null, 'the sections give way to the results');
      await type('qqqzzz');
      assert.equal(await page.$eval('#settings-results-empty', el => el.textContent), 'No settings match \u201cqqqzzz\u201d.');
      await rights(page, { owner: false, admin: false });
      await type('mentions');
      assert.deepEqual(await results(), [], 'hidden by rights: not found');
      assert.equal(await page.$('#settings-results-empty') !== null, true);
      await rights(page, { owner: true, admin: false });
      await type('mentions');
      await page.$eval('.settings-result', el => el.click());
      assert.equal(await page.evaluate(() => window.__selected.at(-1)), 'settings/agents/prompts', 'a click opens its category');
      await type('theme');
      await page.focus('#settings-search'); await page.keyboard.press('Enter');
      assert.equal(await page.evaluate(() => window.__selected.at(-1)), 'settings/personal/appearance', 'Enter opens the first result');
      await page.focus('#settings-search'); await page.keyboard.press('Escape'); await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#settings-search').value, !!document.querySelector('.settings-nav-section')]), ['', true], 'Escape clears the box and brings the sections back');
      await page.close();
    }
    // A destroyed shell draws nothing more and stops following the selection.
    {
      const page = await mount();
      await page.evaluate(() => window.__shell.destroy());
      await page.evaluate(() => { window.__shell.store.select('usages'); window.__shell.store.setRights({ owner: true, admin: true }); });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#settings-dialog nav').children.length, document.querySelector('#settings-dialog header').children.length, document.querySelectorAll('.settings-cat-host').length, document.querySelector('#settings-general').dataset.cat]), [0, 0, 0, 'appearance']);
      await page.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('shell page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

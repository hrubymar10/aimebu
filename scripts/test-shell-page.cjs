// The settings dialog's header and nav in Chromium: the tabs, what the account may see, the selection, the Users tab's wait and the
// sections' visibility. web/ is served as is with its import map; no Go server. Part of `make test-ui`.
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
const sections = ['general', 'usages', 'notifications', 'roles', 'users', 'prompts', 'macros', 'machines', 'harnesses'];
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css">${importMap}</head>
<body><dialog id="settings-dialog" aria-label="Settings" open><header></header><nav aria-label="Settings sections"></nav>${sections.map(key => `<section id="settings-${key}" class="settings-content"></section>`).join('')}</dialog></body></html>`;
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
    await page.evaluate(async () => {
      const { signal } = await import('/js/ui.js');
      const { SettingsShell } = await import('/js/pages/shell/page.js');
      const dialog = document.querySelector('#settings-dialog');
      window.__selected = []; window.__closed = 0;
      const users = window.__users = signal('pending');
      window.__shell = new SettingsShell(dialog, Object.fromEntries(['general', 'usages', 'notifications', 'roles', 'users', 'prompts', 'macros', 'machines', 'harnesses'].map(key => [key, document.querySelector(`#settings-${key}`)])),
        { users, onSelect: key => { window.__selected.push(key); window.__shell.store.select(key); }, onClose: () => { window.__closed++; } });
    });
    return page;
  };
  const nav = page => page.evaluate(() => [...document.querySelector('#settings-dialog nav').children].map(node => `${node.id || 'caption'}${node.hidden ? '!' : ''}${node.classList.contains('settings-selected') ? '*' : ''}`));
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 30)));
  try {
    // The tabs in order under their two captions, General selected, the Users tab holding its place; the title and the close button.
    {
      const page = await mount();
      assert.deepEqual(await nav(page), ['caption', 'settings-general-tab*', 'settings-usages-tab', 'settings-notifications-tab', 'settings-roles-tab!', 'settings-users-tab', 'settings-prompts-tab!', 'settings-macros-tab!', 'settings-agents-title!', 'settings-machines-tab!', 'settings-harnesses-tab!']);
      assert.deepEqual(await page.evaluate(() => {
        const nav = document.querySelector('#settings-dialog nav'), users = nav.querySelector('#settings-users-tab'), close = document.querySelector('#settings-close');
        return { caption: [nav.firstElementChild.textContent, nav.firstElementChild.getAttribute('aria-hidden')], title: document.querySelector('#settings-title').textContent, current: [...nav.querySelectorAll('[aria-current]')].map(node => node.id),
          users: [users.classList.contains('settings-tab-pending'), users.getAttribute('aria-hidden'), users.tabIndex], group: [nav.querySelector('#settings-agents-title').textContent, nav.querySelector('#settings-agents-title').className],
          close: [close.className, close.title, close.getAttribute('aria-label'), !!close.querySelector('.icon-mask-close')], sections: sections().map(([key, hidden]) => `${key}${hidden ? '!' : ''}`), dataset: document.querySelector('#settings-dialog').dataset.section };
        function sections() { return [...document.querySelectorAll('#settings-dialog > section')].map(node => [node.id.replace('settings-', ''), node.hidden]); }
      }), { caption: ['Settings', 'true'], title: 'General', current: ['settings-general-tab'], users: [true, 'true', -1], group: ['Agents', 'settings-nav-title settings-nav-group-title'], close: ['icon-btn', 'Close settings', 'Close settings', true],
        sections: ['general', 'usages!', 'notifications!', 'roles!', 'users!', 'prompts!', 'macros!', 'machines!', 'harnesses!'], dataset: 'general' });
      await page.$eval('#settings-close', el => el.click());
      assert.equal(await page.evaluate(() => window.__closed), 1);
      await page.close();
    }
    // What the account may see: the owner everything, an admin the instance tabs but not Usages, anyone else neither.
    {
      const page = await mount();
      await page.evaluate(() => { window.__shell.store.setUser('usr'); window.__shell.store.setRights({ owner: true, admin: false }); });
      await settle(page);
      assert.deepEqual((await nav(page)).filter(item => item.endsWith('!')), []);
      await page.evaluate(() => window.__shell.store.setRights({ owner: false, admin: true }));
      await settle(page);
      assert.deepEqual((await nav(page)).filter(item => item.endsWith('!')), ['settings-usages-tab!']);
      await page.evaluate(() => window.__shell.store.setRights({ owner: false, admin: false }));
      await settle(page);
      assert.deepEqual((await nav(page)).filter(item => item.endsWith('!')), ['settings-usages-tab!', 'settings-roles-tab!', 'settings-prompts-tab!', 'settings-macros-tab!', 'settings-agents-title!', 'settings-machines-tab!', 'settings-harnesses-tab!']);
      await page.evaluate(() => { window.__shell.store.setRights({ owner: true, admin: false }); window.__shell.store.hideUsages(); });
      await settle(page);
      assert.deepEqual((await nav(page)).filter(item => item.endsWith('!')), ['settings-usages-tab!'], 'a refused usage read takes the Usages tab away');
      await page.evaluate(() => window.__shell.store.setUser());
      await settle(page);
      assert.deepEqual((await nav(page)).filter(item => item.endsWith('!')), [], 'signing in shows it again');
      await page.close();
    }
    // The Users tab: pending, then shown or gone.
    {
      const page = await mount();
      await page.evaluate(() => { window.__users.value = 'shown'; });
      await settle(page);
      assert.deepEqual(await page.$eval('#settings-users-tab', el => [el.hidden, el.classList.contains('settings-tab-pending'), el.hasAttribute('aria-hidden'), el.hasAttribute('tabindex')]), [false, false, false, false]);
      await page.evaluate(() => { window.__users.value = 'hidden'; });
      await settle(page);
      assert.equal(await page.$eval('#settings-users-tab', el => el.hidden), true);
      await page.close();
    }
    // Choosing a tab: its section shows alone, the title and the dialog's section follow, and the shell tells the page to open it.
    {
      const page = await mount();
      await page.evaluate(() => window.__shell.store.setRights({ owner: true, admin: false }));
      await page.$eval('#settings-machines-tab', el => el.click());
      await settle(page);
      assert.deepEqual(await page.evaluate(() => ({ asked: window.__selected, title: document.querySelector('#settings-title').textContent, selected: [...document.querySelectorAll('#settings-dialog nav .settings-selected')].map(n => n.id),
        current: [...document.querySelectorAll('#settings-dialog nav [aria-current]')].map(n => n.id), shown: [...document.querySelectorAll('#settings-dialog > section')].filter(n => !n.hidden).map(n => n.id), dataset: document.querySelector('#settings-dialog').dataset.section })),
        { asked: ['machines'], title: 'Machines', selected: ['settings-machines-tab'], current: ['settings-machines-tab'], shown: ['settings-machines'], dataset: 'machines' });
      await page.evaluate(() => window.__shell.store.select('users'));
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#settings-title').textContent, [...document.querySelectorAll('#settings-dialog > section')].filter(n => !n.hidden).map(n => n.id)]), ['Users', ['settings-users']]);
      await page.close();
    }
    // A destroyed shell draws nothing more and stops following the selection.
    {
      const page = await mount();
      await page.evaluate(() => window.__shell.destroy());
      await page.evaluate(() => { window.__shell.store.select('usages'); window.__shell.store.setRights({ owner: true, admin: true }); });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#settings-dialog nav').children.length, document.querySelector('#settings-dialog header').children.length, document.querySelector('#settings-general').hidden]), [0, 0, false]);
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

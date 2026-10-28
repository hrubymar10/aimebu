// The phone's bottom tab bar in Chromium: the tab for the open view is marked, a tab drives the drawers through their own buttons, and the bar follows
// the page when something else opens a drawer. web/ is served as is with its import map and markup, without app.js; no Go server. It runs unchanged
// against the page that replaced the hand-written bar. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const REPLACED = fs.existsSync(path.join(root, 'js', 'pages', 'mobile-tabs', 'page.js'));
const MODULE = REPLACED ? '/js/pages/mobile-tabs/page.js' : '/js/mobile-tabs.js';
const fixture = fs.readFileSync(path.join(root, 'index.html'), 'utf8').replace(/<script type="module" src="\/js\/app\.js[^"]*"><\/script>/, '');
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
  // The drawer buttons do what app.js makes them do to the page's classes; the test records which of them a tab pressed.
  const mount = async (width = 390) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.setViewport({ width, height: 800 });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async module => {
      document.querySelector('#app-view').hidden = false;
      window.__clicks = [];
      const app = document.querySelector('#app-view'), panel = document.querySelector('#agents-panel');
      const on = (id, action) => document.querySelector(id).addEventListener('click', () => { window.__clicks.push(id.slice(1)); action(); });
      on('#rooms-toggle', () => app.classList.add('rooms-open'));
      on('#back', () => app.classList.remove('rooms-open'));
      on('#agents-toggle', () => panel.classList.add('mobile-open'));
      on('#agents-close', () => panel.classList.remove('mobile-open'));
      on('#inbox-toggle', () => { const view = document.querySelector('#inbox-view'); view.hidden = !view.hidden; app.classList.toggle('inbox-open', !view.hidden); });
      const { initMobileTabs } = await import(module);
      window.__tabs = initMobileTabs();
    }, MODULE);
    return page;
  };
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 60)));
  const marks = page => page.evaluate(() => [...document.querySelectorAll('#mobile-tabs .mobile-tab')].map(tab => [tab.dataset.tab, tab.textContent, tab.className, tab.getAttribute('aria-current')]));
  const active = async page => (await marks(page)).filter(tab => tab[3] === 'page' || / active\b/.test(tab[2])).map(tab => tab[0]);
  const press = async (page, name) => { await page.evaluate(() => { window.__clicks.length = 0; }); await page.$eval(`.mobile-tab[data-tab=${name}]`, node => node.click()); await settle(page); return page.evaluate(() => window.__clicks.slice()); };
  const set = (page, selector, name, on) => page.evaluate((selector, name, on) => document.querySelector(selector).classList.toggle(name, on), selector, name, on);

  // The bar has the three tabs in order; the chat tab is marked at first, with aria-current on it alone.
  {
    const page = await mount();
    assert.deepEqual(await marks(page), [['rooms', 'Rooms', 'row-btn mobile-tab', null], ['chat', 'Chat', 'row-btn mobile-tab active', 'page'], ['agents', 'Agents', 'row-btn mobile-tab', null]]);
    assert.equal(await page.$eval('#mobile-tabs', node => [node.tagName, node.getAttribute('aria-label')].join()), 'NAV,Sections');
    await page.close();
  }

  // A tab opens its view through the drawer's own buttons, closing the one that is open first; the marked tab follows.
  {
    const page = await mount();
    assert.deepEqual([await press(page, 'rooms'), await active(page)], [['rooms-toggle'], ['rooms']]);
    assert.deepEqual([await press(page, 'rooms'), await active(page)], [[], ['rooms']], 'the open view\'s tab does nothing');
    assert.deepEqual([await press(page, 'agents'), await active(page)], [['back', 'agents-toggle'], ['agents']]);
    assert.deepEqual([await press(page, 'rooms'), await active(page)], [['agents-close', 'rooms-toggle'], ['rooms']]);
    assert.deepEqual([await press(page, 'chat'), await active(page)], [['back'], ['chat']]);
    assert.deepEqual([await press(page, 'chat'), await active(page)], [[], ['chat']]);
    assert.deepEqual([await press(page, 'agents'), await active(page)], [['agents-toggle'], ['agents']]);
    assert.deepEqual([await press(page, 'agents'), await active(page)], [[], ['agents']]);
    assert.deepEqual([await press(page, 'chat'), await active(page)], [['agents-close'], ['chat']]);
    assert.equal((await marks(page)).filter(tab => tab[3] === 'page').length, 1);
    await page.close();
  }

  // The bar follows the page when something else opens or closes a drawer, and no tab is marked while the Inbox is open.
  {
    const page = await mount();
    await set(page, '#app-view', 'rooms-open', true); await settle(page);
    assert.deepEqual(await active(page), ['rooms']);
    await set(page, '#agents-panel', 'mobile-open', true); await settle(page);
    assert.deepEqual(await active(page), ['rooms'], 'the rooms drawer wins over the agents panel');
    await set(page, '#app-view', 'rooms-open', false); await settle(page);
    assert.deepEqual(await active(page), ['agents']);
    await set(page, '#app-view', 'inbox-open', true); await settle(page);
    assert.deepEqual([await active(page), (await marks(page)).map(tab => tab[3])], [[], [null, null, null]]);
    await set(page, '#app-view', 'inbox-open', false); await set(page, '#agents-panel', 'mobile-open', false); await settle(page);
    assert.deepEqual(await active(page), ['chat']);
    await page.close();
  }

  // With the Inbox open a tab closes it first (the Inbox button), then opens its view.
  {
    const page = await mount();
    await page.$eval('#inbox-toggle', node => node.click());
    assert.deepEqual(await active(page), []);
    assert.deepEqual([await press(page, 'rooms'), await active(page)], [['inbox-toggle', 'rooms-toggle'], ['rooms']]);
    await page.$eval('#inbox-toggle', node => node.click());
    assert.deepEqual([await press(page, 'chat'), await active(page)], [['inbox-toggle', 'back'], ['chat']], 'the chat tab closes the Inbox, then the drawer that was open');
    await page.close();
  }

  // The bar belongs to the phone: on a wide window a tab press drives nothing (the bar is not shown there), yet the marks still follow the page.
  {
    const page = await mount(1280);
    assert.equal(await page.$eval('#mobile-tabs', node => getComputedStyle(node).display), 'none');
    assert.deepEqual(await press(page, 'rooms'), []);
    await set(page, '#app-view', 'rooms-open', true); await settle(page);
    assert.deepEqual(await active(page), ['rooms']);
    await page.close();
  }

  // A destroyed bar is gone and no longer follows the page.
  if (REPLACED) {
    const page = await mount();
    await page.evaluate(() => window.__tabs.destroy());
    assert.equal(await page.$$eval('#mobile-tabs .mobile-tab', nodes => nodes.length), 0);
    await set(page, '#app-view', 'rooms-open', true); await settle(page);
    assert.equal(await page.$$eval('#mobile-tabs .mobile-tab', nodes => nodes.length), 0, 'a drawer opening redraws nothing');
    assert.equal(await page.evaluate(() => window.__tabs.store.current.value), 'chat', 'nor does it follow the page any more');
    await page.close();
  }

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('mobile tabs page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });

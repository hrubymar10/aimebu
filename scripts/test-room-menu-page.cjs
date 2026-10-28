// The phone overflow menu of the room header in Chromium: the controls that do not fit next to the title collapse into one menu whose items click the
// real header buttons. web/ is served as is with its import map and markup, without app.js; no Go server. It runs unchanged against the page that
// replaced the hand-written menu. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const MODULE = fs.existsSync(path.join(root, 'js', 'pages', 'room-menu', 'page.js')) ? '/js/pages/room-menu/page.js' : '/js/room-menu.js';
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
  const mount = async (width = 390) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.setViewport({ width, height: 800 });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(() => { document.querySelector('#app-view').hidden = false; document.querySelector('.chat').style.display = 'flex'; });
    // The header buttons the menu mirrors: three shown (one disabled), one hidden, and the room settings button beside the title.
    await page.evaluate(() => {
      window.__clicks = [];
      const buttons = [...document.querySelectorAll('.room-controls button')];
      buttons.forEach((button, index) => { button.hidden = index > 2; button.disabled = index === 1; button.addEventListener('click', () => window.__clicks.push(button.id)); });
      const info = document.querySelector('#room-info'); info.hidden = false; info.disabled = false; info.addEventListener('click', () => window.__clicks.push('room-info'));
      window.__ids = buttons.map(button => button.id);
    });
    await page.evaluate(async module => { const { RoomMenu } = await import(module); window.__menu = new RoomMenu(); }, MODULE);
    return page;
  };
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 60)));
  const state = page => page.evaluate(() => { const toggle = document.querySelector('#room-menu-toggle'), menu = document.querySelector('#room-menu');
    return { toggleHidden: toggle.hidden, expanded: toggle.getAttribute('aria-expanded'), menuHidden: menu.hidden, focus: document.activeElement.textContent || document.activeElement.id,
      items: [...menu.querySelectorAll('.room-menu-item')].map(item => [item.textContent, item.disabled]) }; });

  // On a phone the overflow button shows while at least one control can be used; the menu lists every shown control by its accessible name, the room
  // settings first, with the disabled ones disabled, and opens on the first usable item.
  {
    const page = await mount();
    const names = await page.evaluate(() => [document.querySelector('#room-info'), ...[...document.querySelectorAll('.room-controls button')].filter(button => !button.hidden)].map(button => [button.getAttribute('aria-label') || button.title, button.disabled]));
    let now = await state(page);
    assert.deepEqual([now.toggleHidden, now.expanded, now.menuHidden], [false, 'false', true]);
    assert.deepEqual(await page.$eval('#room-menu-toggle', node => [node.getAttribute('aria-haspopup'), node.getAttribute('aria-label'), node.title]), ['menu', 'Room actions', 'Room actions']);
    assert.deepEqual(await page.$eval('#room-menu', node => [node.getAttribute('role'), node.getAttribute('aria-label'), node.parentElement === document.body]), ['menu', 'Room actions', true]);
    await page.$eval('#room-menu-toggle', node => node.click());
    now = await state(page);
    assert.deepEqual([now.menuHidden, now.expanded], [false, 'true']);
    assert.deepEqual(now.items, names, 'one item per shown control, the room settings first, disabled as they are');
    assert.equal(now.focus, now.items.find(item => !item[1])[0], 'it opens on the first usable item');
    assert.equal(await page.$eval('#room-menu', node => node.style.top !== '' && node.style.right !== ''), true, 'it sits under the button');
    await page.close();
  }

  // The arrows, Home and End move between the usable items and wrap; Escape closes the menu and gives the focus back to the button; a click outside
  // closes it; ArrowDown on the closed button opens it.
  {
    const page = await mount();
    await page.focus('#room-menu-toggle');
    await page.keyboard.press('ArrowDown');
    assert.equal((await state(page)).menuHidden, false, 'ArrowDown opens it');
    const usable = (await state(page)).items.filter(item => !item[1]).map(item => item[0]);
    const focus = async () => (await state(page)).focus;
    assert.equal(await focus(), usable[0]);
    await page.keyboard.press('ArrowDown'); assert.equal(await focus(), usable[1]);
    for (let index = 2; index < usable.length; index++) await page.keyboard.press('ArrowDown');
    assert.equal(await focus(), usable.at(-1));
    await page.keyboard.press('ArrowDown'); assert.equal(await focus(), usable[0], 'wraps after the last');
    await page.keyboard.press('ArrowUp'); assert.equal(await focus(), usable.at(-1), 'wraps before the first');
    await page.keyboard.press('Home'); assert.equal(await focus(), usable[0]);
    await page.keyboard.press('End'); assert.equal(await focus(), usable.at(-1));
    await page.keyboard.press('Escape');
    assert.deepEqual([(await state(page)).menuHidden, await page.evaluate(() => document.activeElement.id)], [true, 'room-menu-toggle']);
    await page.$eval('#room-menu-toggle', node => node.click());
    await page.mouse.click(20, 700);
    assert.deepEqual([(await state(page)).menuHidden, (await state(page)).expanded], [true, 'false']);
    await page.$eval('#room-menu-toggle', node => node.click());
    await page.$eval('#room-menu-toggle', node => node.click());
    assert.equal((await state(page)).menuHidden, true, 'the button toggles it');
    await page.close();
  }

  // An item closes the menu and clicks the real header button; the menu follows the buttons it mirrors: a button that is disabled or hidden later is
  // gone or disabled in the next menu, and with nothing usable the overflow button goes and the open menu closes.
  {
    const page = await mount();
    await page.$eval('#room-menu-toggle', node => node.click());
    const first = (await state(page)).items.find(item => !item[1])[0];
    await page.evaluate(first => [...document.querySelectorAll('.room-menu-item')].find(item => item.textContent === first).click(), first);
    assert.equal((await state(page)).menuHidden, true);
    assert.equal(await page.evaluate(() => window.__clicks.length), 1, 'the real button was clicked');
    await page.evaluate(() => { document.querySelector('#room-info').disabled = true; document.querySelectorAll('.room-controls button')[0].hidden = true; });
    await settle(page);
    await page.$eval('#room-menu-toggle', node => node.click());
    assert.equal((await state(page)).items.some(item => item[0] === 'Room settings'), true);
    assert.equal((await state(page)).items.find(item => item[0] === 'Room settings')[1], true, 'a disabled control is a disabled item');
    await page.evaluate(() => { document.querySelector('#room-info').disabled = true; for (const button of document.querySelectorAll('.room-controls button')) button.disabled = true; });
    await settle(page);
    assert.deepEqual([(await state(page)).toggleHidden, (await state(page)).menuHidden], [true, true], 'with nothing usable the button goes and the menu closes');
    await page.evaluate(() => { document.querySelector('#room-info').disabled = false; });
    await settle(page);
    assert.equal((await state(page)).toggleHidden, false, 'it returns when a control can be used');
    await page.close();
  }

  // The menu exists on phones only: on a wide screen the button stays hidden, and leaving the phone layout closes an open menu.
  {
    const page = await mount(1280);
    assert.equal((await state(page)).toggleHidden, true);
    await page.close();
    const phone = await mount();
    await phone.$eval('#room-menu-toggle', node => node.click());
    assert.equal((await state(phone)).menuHidden, false);
    await phone.setViewport({ width: 1280, height: 800 }); await settle(phone);
    assert.deepEqual([(await state(phone)).menuHidden, (await state(phone)).toggleHidden], [true, true]);
    await phone.setViewport({ width: 390, height: 800 }); await settle(phone);
    assert.equal((await state(phone)).toggleHidden, false);
    await phone.close();
  }

  // Details of the menu: a click inside it (not on an item) keeps it open; Escape with the menu closed leaves the focus alone; an item closes it even
  // when its button's click does not reach the page; its keys do not scroll the page; it is placed again when the window changes and never closer than
  // 8px to the edge; the button follows the header controls alone, and a control's label falls back to its title.
  {
    const page = await mount();
    await page.$eval('#room-menu-toggle', node => node.click());
    await page.$eval('#room-menu', node => node.click());
    assert.equal((await state(page)).menuHidden, false, 'a click on the menu itself keeps it open');
    assert.equal(await page.evaluate(() => { const item = document.querySelector('.room-menu-item:not(:disabled)'); const event = new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true }); item.dispatchEvent(event); return event.defaultPrevented; }), true, 'its keys do not scroll the page');
    await page.keyboard.press('Escape');
    assert.equal(await page.evaluate(() => { const event = new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true }); document.querySelector('#room-menu-toggle').dispatchEvent(event); return event.defaultPrevented; }), true, 'ArrowDown on the button does not scroll the page');
    await page.keyboard.press('Escape');
    assert.equal(await page.evaluate(() => { const event = new KeyboardEvent('keydown', { key: 'a', bubbles: true, cancelable: true }); document.activeElement.dispatchEvent(event); return event.defaultPrevented; }), false, 'other keys are left alone');
    await page.keyboard.press('Escape');
    await page.evaluate(() => { const input = document.createElement('input'); input.id = 'elsewhere'; document.body.append(input); input.focus(); });
    await page.keyboard.press('Escape');
    assert.equal(await page.evaluate(() => document.activeElement.id), 'elsewhere', 'Escape with the menu closed does not take the focus');
    await page.evaluate(() => { document.querySelectorAll('.room-controls button')[0].addEventListener('click', event => event.stopPropagation()); });
    await page.$eval('#room-menu-toggle', node => node.click());
    const name = await page.evaluate(() => { const control = document.querySelectorAll('.room-controls button')[0]; return control.getAttribute('aria-label') || control.title; });
    await page.evaluate(name => [...document.querySelectorAll('.room-menu-item')].find(item => item.textContent === name).click(), name);
    assert.equal((await state(page)).menuHidden, true, 'an item closes the menu even if its button stops the click');
    await page.$eval('#room-menu-toggle', node => node.click());
    const right = () => page.$eval('#room-menu', node => parseInt(node.style.right, 10));
    const before = await right();
    await page.$eval('#room-menu-toggle', node => { node.style.marginRight = '40px'; });
    await page.evaluate(() => window.dispatchEvent(new Event('resize')));
    assert.equal(await right(), before + 40, 'the menu is placed again when the window changes');
    await page.$eval('#room-menu-toggle', node => { node.style.marginRight = '-30px'; });
    await page.evaluate(() => window.dispatchEvent(new Event('resize')));
    assert.equal(await right(), 8, 'never closer than 8px to the edge');
    await page.close();
  }
  {
    const page = await mount();
    await page.evaluate(() => { document.querySelector('#room-info').hidden = true; for (const button of document.querySelectorAll('.room-controls button')) button.disabled = true; });
    await settle(page);
    assert.equal((await state(page)).toggleHidden, true);
    await page.evaluate(() => { document.querySelectorAll('.room-controls button')[2].disabled = false; });
    await settle(page);
    assert.equal((await state(page)).toggleHidden, false, 'one control becoming usable brings the button back');
    await page.evaluate(() => { const control = document.querySelectorAll('.room-controls button')[2]; control.removeAttribute('aria-label'); control.title = 'Only a title'; });
    await page.$eval('#room-menu-toggle', node => node.click());
    assert.equal((await state(page)).items.some(item => item[0] === 'Only a title'), true, 'a control without an accessible name is labelled by its title');
    await page.close();
  }

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('room menu page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });

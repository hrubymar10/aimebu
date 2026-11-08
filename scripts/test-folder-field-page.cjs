// The Workspace folder field (web/js/folder-field.js) in Chromium against the real New project dialog markup and styles of web/ (no app.js, no Go server): it is
// empty until the person asks for their home folder, and the three ways to ask (A a button at the right edge, B a button at the left edge, C a ghost suggestion
// that Tab or → accepts) each fill the same way. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const indexHtml = fs.readFileSync(path.join(root, 'index.html'), 'utf8');
const importMap = /<script type="importmap">.*?<\/script>/s.exec(indexHtml)[0];
const dialog = /<dialog id="project-dialog".*?<\/dialog>/s.exec(indexHtml)[0];
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css"><link rel="stylesheet" href="/v2-only.css">${importMap}</head><body>${dialog}</body></html>`;
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
  const mount = async ({ home = '/home/ana', variant = 'a' } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.setViewport({ width: 900, height: 700 });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async (home, variant) => {
      document.documentElement.setAttribute('data-ui-homefolder', variant);
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_1', owner: true, admin: true, host_home: home });
      const { attachFolderField } = await import('/js/folder-field.js');
      attachFolderField(document.querySelector('#project-form').elements.folder);
      document.querySelector('#project-dialog').showModal();
    }, home, variant);
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 100)));
    return page;
  };
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
  const state = page => page.evaluate(() => {
    const input = document.querySelector('#project-form [name=folder]'), button = document.querySelector('.folder-home-btn'), ghost = document.querySelector('.folder-ghost');
    const box = el => { const r = el.getBoundingClientRect(); return { left: Math.round(r.left), right: Math.round(r.right), width: Math.round(r.width) }; };
    return { value: input.value, focused: document.activeElement === input, caret: [input.selectionStart, input.selectionEnd], placeholder: input.getAttribute('placeholder'), padding: [parseFloat(getComputedStyle(input).paddingLeft), parseFloat(getComputedStyle(input).paddingRight)],
      button: button.hidden ? null : box(button), ghost: ghost.hidden ? null : { text: ghost.textContent, ...box(ghost) }, input: box(input), label: button.getAttribute('aria-label'), title: button.title, side: document.querySelector('.folder-field').dataset.homeButton || '' };
  });

  // A: nothing fills by itself; the button is at the right edge inside the field while it is empty, and a click fills the home folder and keeps the focus.
  {
    const page = await mount({ variant: 'a' });
    await page.$eval('#project-form [name=folder]', el => el.focus()); await settle(page);
    const empty = await state(page);
    assert.equal(empty.value, '', 'focusing the field does not fill it');
    assert.ok(empty.button && empty.ghost === null, 'variant A shows the button and no ghost');
    assert.ok(empty.button.right <= empty.input.right && empty.button.left > empty.input.left + empty.input.width / 2, `the button is inside the field at its right edge: ${JSON.stringify(empty)}`);
    assert.deepEqual([empty.label, empty.title, empty.placeholder], ['Use my home folder', 'Use my home folder', 'Folder path']);
    assert.ok(empty.padding[1] >= 38, 'the text leaves room for the button');
    await page.$eval('.folder-home-btn', el => el.click()); await settle(page);
    const filled = await state(page);
    assert.deepEqual([filled.value, filled.focused, filled.caret, filled.button], ['/home/ana/', true, [10, 10], null], 'a click fills the home folder, keeps the focus, puts the caret after it and hides the button');
    // Typing a whole path over the start steps it aside, as before.
    await page.keyboard.type('/srv/work');
    assert.equal((await state(page)).value, '/srv/work');
    await page.$eval('#project-form [name=folder]', el => { el.value = ''; el.dispatchEvent(new Event('input', { bubbles: true })); });
    await settle(page);
    assert.ok((await state(page)).button, 'the button is back once the field is empty again');
    await page.close();
  }
  // B: the same at the left edge, with the text inset only while the button shows.
  {
    const page = await mount({ variant: 'b' });
    const empty = await state(page);
    assert.ok(empty.button && empty.button.left >= empty.input.left && empty.button.right < empty.input.left + empty.input.width / 2, `the button is at the left edge: ${JSON.stringify(empty)}`);
    assert.ok(empty.padding[0] >= 38 && empty.side === 'b', 'the text is inset for it');
    await page.$eval('.folder-home-btn', el => el.click()); await settle(page);
    const filled = await state(page);
    assert.deepEqual([filled.value, filled.button, filled.focused], ['/home/ana/', null, true]);
    assert.ok(filled.padding[0] < 20, 'the inset goes with the button');
    await page.close();
  }
  // C: no button; the home folder is a ghost in the field and the placeholder steps aside; Tab or the right arrow accepts it, other keys do nothing.
  {
    const page = await mount({ variant: 'c' });
    await page.$eval('#project-form [name=folder]', el => el.focus()); await settle(page);
    const empty = await state(page);
    assert.ok(empty.button === null && empty.ghost && empty.ghost.text === '/home/ana/', `the ghost shows the home folder: ${JSON.stringify(empty)}`);
    assert.equal(empty.placeholder, '', 'the placeholder steps aside for the ghost');
    assert.ok(empty.ghost.left >= empty.input.left && empty.ghost.right <= empty.input.right + 1, 'the ghost is inside the field');
    await page.keyboard.down('Shift'); await page.keyboard.press('Tab'); await page.keyboard.up('Shift');
    await page.$eval('#project-form [name=folder]', el => el.focus());
    assert.equal((await state(page)).value, '', 'Shift+Tab does not accept it');
    await page.keyboard.press('Tab'); await settle(page);
    const accepted = await state(page);
    assert.deepEqual([accepted.value, accepted.focused, accepted.caret, accepted.ghost, accepted.placeholder], ['/home/ana/', true, [10, 10], null, 'Folder path'], 'Tab accepts it and keeps the focus in the field');
    await page.$eval('#project-form [name=folder]', el => { el.value = ''; el.dispatchEvent(new Event('input', { bubbles: true })); el.focus(); });
    await settle(page);
    await page.keyboard.press('ArrowRight'); await settle(page);
    assert.equal((await state(page)).value, '/home/ana/', 'the right arrow accepts it too');
    await page.$eval('#project-form [name=folder]', el => { el.value = 'srv'; el.dispatchEvent(new Event('input', { bubbles: true })); });
    await settle(page);
    assert.equal((await state(page)).ghost, null, 'no ghost over typed text');
    await page.close();
  }
  // No known home folder: nothing to offer in any variant. And the choice can change while the page is open.
  for (const variant of ['a', 'b', 'c']) {
    const page = await mount({ home: '', variant });
    const none = await state(page);
    assert.deepEqual([none.button, none.ghost, none.value], [null, null, ''], `variant ${variant} offers nothing without a home folder`);
    await page.close();
  }
  {
    const page = await mount({ variant: 'a' });
    assert.ok((await state(page)).button);
    await page.evaluate(() => { document.documentElement.setAttribute('data-ui-homefolder', 'c'); document.dispatchEvent(new CustomEvent('ui-variant-change')); });
    await settle(page);
    const live = await state(page);
    assert.ok(live.button === null && live.ghost, 'switching to C swaps the button for the ghost at once');
    await page.close();
  }
  assert.deepEqual(errors, [], `the page reported errors: ${errors.join(' | ')}`);
  await browser.close();
  server.close();
  console.log('folder field page tests passed');
}

main().catch(error => { console.error(error); process.exit(1); });

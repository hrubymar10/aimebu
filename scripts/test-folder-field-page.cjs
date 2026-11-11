// The Workspace folder field (web/js/folder-field.js) in Chromium against the real New project dialog markup and styles of web/ (no app.js, no Go server): it is
// empty until the person asks for their home folder with the house button at its left edge. Part of `make test-ui`.
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
  if (url.pathname === '/api/machines/directories') {
    const parent = url.searchParams.get('path'), prefix = url.searchParams.get('prefix') || '';
    const entries = { '/home/ana/': ['odd', 'other', '.private'], '/home/ana/odd/': ['deeper'], '/home/ana/many/': Array.from({ length: 20 }, (_, i) => `folder-${i}`) }[parent] || [];
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ directories: entries.filter(name => name.startsWith(prefix) && (!name.startsWith('.') || prefix.startsWith('.'))) })); return;
  }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  const mount = async ({ home = '/home/ana' } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.setViewport({ width: 900, height: 700 });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async home => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_1', owner: true, admin: true, host_home: home });
      const { attachFolderField } = await import('/js/folder-field.js');
      attachFolderField(document.querySelector('#project-form').elements.folder);
      document.querySelector('#project-dialog').showModal();
    }, home);
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 100)));
    return page;
  };
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
  const state = page => page.evaluate(() => {
    const input = document.querySelector('#project-form [name=folder]'), button = document.querySelector('.folder-home-btn');
    const box = el => { const r = el.getBoundingClientRect(); return { left: Math.round(r.left), right: Math.round(r.right), width: Math.round(r.width) }; };
    return { value: input.value, focused: document.activeElement === input, caret: [input.selectionStart, input.selectionEnd], placeholder: input.getAttribute('placeholder'), padding: [parseFloat(getComputedStyle(input).paddingLeft), parseFloat(getComputedStyle(input).paddingRight)],
      button: button.hidden ? null : box(button), input: box(input), label: button.getAttribute('aria-label'), title: button.title, inset: 'homeButton' in document.querySelector('.folder-field').dataset };
  });

  // Nothing fills by itself; the button is at the left edge inside the field while it is empty (the text inset for it), and a click fills the home folder and keeps the focus.
  {
    const page = await mount();
    await page.$eval('#project-form [name=folder]', el => el.focus()); await settle(page);
    const empty = await state(page);
    assert.equal(empty.value, '', 'focusing the field does not fill it');
    assert.ok(empty.button && empty.button.left >= empty.input.left && empty.button.right < empty.input.left + empty.input.width / 2, `the button is inside the field at its left edge: ${JSON.stringify(empty)}`);
    assert.deepEqual([empty.label, empty.title, empty.placeholder], ['Use my home folder', 'Use my home folder', 'Folder path']);
    assert.ok(empty.padding[0] >= 38 && empty.inset, 'the text is inset for the button');
    await page.$eval('.folder-home-btn', el => el.click()); await settle(page);
    const filled = await state(page);
    assert.deepEqual([filled.value, filled.focused, filled.caret, filled.button], ['/home/ana/', true, [10, 10], null], 'a click fills the home folder, keeps the focus, puts the caret after it and hides the button');
    assert.ok(filled.padding[0] < 20 && !filled.inset, 'the inset goes with the button');
    // Typing a whole path over the start steps it aside, as before.
    await page.keyboard.type('/srv/work');
    assert.equal((await state(page)).value, '/srv/work');
    await page.$eval('#project-form [name=folder]', el => { el.value = ''; el.dispatchEvent(new Event('input', { bubbles: true })); });
    await settle(page);
    assert.ok((await state(page)).button, 'the button is back once the field is empty again');
    await page.close();
  }
  // No known home folder: nothing to offer, and no inset.
  {
    const page = await mount({ home: '' });
    const none = await state(page);
    assert.deepEqual([none.button, none.value, none.inset], [null, '', false], 'nothing is offered without a home folder');
    await page.close();
  }
  // Path completion uses the machine's directory list and leaves files/hidden names out.
  {
    const page = await mount();
    const input = '#project-form [name=folder]';
    await page.focus(input); await page.keyboard.type('/home/ana/od');
    await page.waitForSelector('.folder-suggestions [role=option]');
    assert.deepEqual(await page.$$eval('.folder-suggestions [role=option]', items => items.map(item => item.textContent)), ['odd/']);
    await page.keyboard.press('ArrowDown'); await page.keyboard.press('Enter');
    assert.equal((await state(page)).value, '/home/ana/odd/');
    await page.waitForSelector('.folder-suggestions [role=option]');
    await page.keyboard.press('/');
    assert.equal((await state(page)).value, '/home/ana/odd/deeper/');
    await page.$eval(input, node => { node.value = '/home/ana/'; node.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.waitForSelector('.folder-suggestions [role=option]');
    await page.keyboard.press('Escape');
    assert.equal(await page.$eval('.folder-suggestions', node => node.hidden), true);
    await page.$eval(input, node => { node.value = '/home/ana/.'; node.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.waitForSelector('.folder-suggestions [role=option]');
    assert.deepEqual(await page.$$eval('.folder-suggestions [role=option]', items => items.map(item => item.textContent)), ['.private/']);
    await page.keyboard.press('Tab');
    assert.equal((await state(page)).value, '/home/ana/.private/');
    await page.close();
  }
  // A long list escapes the dialog's scrolling box and is still hit-testable outside its border.
  {
    const page = await mount();
    await page.evaluate(() => {
      const dialog = document.querySelector('#project-dialog');
      dialog.style.position = 'fixed'; dialog.style.top = '40px'; dialog.style.left = '200px'; dialog.style.margin = '0';
      dialog.style.height = '180px'; dialog.style.maxHeight = '180px'; dialog.style.overflow = 'auto';
      const input = document.querySelector('#project-form [name=folder]');
      input.scrollIntoView({ block: 'end' }); input.focus();
      input.value = '/home/ana/many/'; input.dispatchEvent(new Event('input', { bubbles: true }));
    });
    await page.waitForSelector('.folder-suggestions [role=option]');
    const geometry = await page.evaluate(() => {
      const dialog = document.querySelector('#project-dialog').getBoundingClientRect();
      const list = document.querySelector('.folder-suggestions').getBoundingClientRect();
      const x = list.left + 20, y = Math.min(list.bottom - 10, dialog.bottom + 20);
      return { dialogBottom: dialog.bottom, listBottom: list.bottom, maxHeight: getComputedStyle(document.querySelector('.folder-suggestions')).maxHeight,
        hit: document.elementFromPoint(x, y)?.closest('.folder-suggestions') !== null };
    });
    assert.ok(geometry.listBottom > geometry.dialogBottom + 20 && geometry.hit, `the list escapes the dialog and accepts a click: ${JSON.stringify(geometry)}`);
    assert.ok(parseFloat(geometry.maxHeight) > 220, `the list can show roughly twelve rows: ${JSON.stringify(geometry)}`);
    await page.close();
  }
  // Near the bottom of a short viewport the list flips above its field.
  {
    const page = await mount();
    await page.setViewport({ width: 900, height: 350 });
    await page.evaluate(() => {
      const dialog = document.querySelector('#project-dialog');
      dialog.style.height = '200px'; dialog.style.maxHeight = '200px'; dialog.style.overflow = 'auto';
      const input = document.querySelector('#project-form [name=folder]');
      input.scrollIntoView({ block: 'end' }); input.focus();
      input.value = '/home/ana/many/'; input.dispatchEvent(new Event('input', { bubbles: true }));
    });
    await page.waitForSelector('.folder-suggestions [role=option]');
    const geometry = await page.evaluate(() => {
      const field = document.querySelector('#project-form [name=folder]').getBoundingClientRect();
      const list = document.querySelector('.folder-suggestions').getBoundingClientRect();
      return { fieldTop: field.top, listTop: list.top, listBottom: list.bottom };
    });
    assert.ok(geometry.listBottom <= geometry.fieldTop - 3 && geometry.listTop >= 8, `the list flips above and stays in view: ${JSON.stringify(geometry)}`);
    await page.close();
  }
  assert.deepEqual(errors, [], `the page reported errors: ${errors.join(' | ')}`);
  await browser.close();
  server.close();
  console.log('folder field page tests passed');
}

main().catch(error => { console.error(error); process.exit(1); });

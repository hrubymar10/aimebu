// Settings -> Notifications in Chromium, with every request held until the test answers it, so the order of answers is chosen. The inbox's
// notification store is replaced by a fake that records what the page asks of it. web/ is served as is with its import map; no Go server.
// Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const importMap = /<script type="importmap">.*?<\/script>/s.exec(fs.readFileSync(path.join(root, 'index.html'), 'utf8'))[0];
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css"><link rel="stylesheet" href="/v2-only.css">${importMap}</head>
<body><dialog id="settings-dialog" open><section id="settings-notifications" class="settings-section active"></section></dialog></body></html>`;
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

const chime = { id: 'builtin:chime', kind: 'builtin', name: 'Chime', size: 0 }, ding = { id: 'builtin:ding', kind: 'builtin', name: 'Ding', size: 0 };
const ping = { id: 'user:p1', kind: 'user', name: 'ping.wav', size: 4096 };
const SOUNDS = 'GET /api/sounds';

// fetch is held: each request waits for __answer(index, status, body) unless an `auto` rule answers it.
function installFetch(auto) {
  window.__requests = []; window.__auto = auto;
  window.fetch = (input, options = {}) => new Promise(resolve => {
    const request = { path: String(input), method: options.method || 'GET', answered: false, upload: options.body instanceof FormData };
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
  // `permission` is what the browser reports until it is asked; `asks` decides what asking does.
  const mount = async ({ auto = { [SOUNDS]: { sounds: [chime, ding, ping] } }, admin = true, permission = 'default', asks = 'granted', sound = 'builtin:chime', open = true } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(installFetch, auto);
    await page.evaluate(async ({ admin, permission, asks, sound, open }) => {
      (await import('/js/api.js')).setSession({ csrf_token: 'token', id: 'usr_1', owner: admin, admin });
      let current = permission;
      window.Notification = { get permission() { return current; }, requestPermission: async () => { if (asks === 'throws') throw new Error('The browser refused to ask'); current = asks; return asks; } };
      const store = window.__store = { muted: false, showText: false, soundID: sound, volume: 70, plays: 0, available: true, async requestPermission() { await Notification.requestPermission(); }, play() { this.plays++; },
        get status() { return `Browser permission: ${Notification.permission}.`; } };
      const { NotificationSettings } = await import('/js/pages/notifications/page.js');
      window.__page = new NotificationSettings(document.querySelector('#settings-notifications'), store);
      window.__page.section.hidden = false; window.__page.section.classList.add('active');
      if (open) window.__page.open();
    }, { admin, permission, asks, sound, open });
    if (open && auto[SOUNDS]) await page.waitForFunction(() => document.querySelectorAll('#notification-sound option').length > 0);
    return page;
  };
  const pending = async (page, method, requestPath) => {
    await page.waitForFunction((method, requestPath) => window.__requests.some(request => !request.answered && request.method === method && request.path.startsWith(requestPath)), {}, method, requestPath);
    return page.evaluate((method, requestPath) => window.__requests.findIndex(request => !request.answered && request.method === method && request.path.startsWith(requestPath)), method, requestPath);
  };
  const answer = (page, index, status, body) => page.evaluate((index, status, body) => window.__answer(index, status, body), index, status, body);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 60)));
  const hold = page => page.evaluate(() => { delete window.__auto['GET /api/sounds']; });
  const statuses = page => page.$$eval('#settings-notifications > p[role=status]', nodes => nodes.map(node => node.textContent));
  try {
    // What the page is made of, in order, with the settings rows named and described.
    {
      const page = await mount();
      assert.deepEqual(await page.evaluate(() => [...document.querySelector('#settings-notifications').children].map(node => `${node.tagName.toLowerCase()}${node.id ? `#${node.id}` : ''}${node.getAttribute('aria-label') ? `[${node.getAttribute('aria-label')}]` : ''}`)),
        ['p', 'div[Inbox alerts]', 'div[Inbox sound]', 'p', 'p#notification-status']);
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('#settings-notifications .settings-row')].map(row => { const field = row.querySelector('input, select, button'); return [field.id, row.querySelector('label').htmlFor === field.id, field.getAttribute('aria-describedby') === `${field.id}-desc`, row.hidden]; })),
        [['notification-muted', true, true, false], ['notification-permission', true, true, false], ['notification-text', true, true, false], ['notification-sound', true, true, false], ['notification-volume', true, true, false], ['notification-sound-upload', true, true, false]]);
      assert.deepEqual(await page.evaluate(() => ({ options: [...document.querySelectorAll('#notification-sound option')].map(option => option.textContent), value: document.querySelector('#notification-sound').value, volume: [document.querySelector('#notification-volume').value, document.querySelector('#notification-volume-value').textContent,
        document.querySelector('#notification-volume').getAttribute('aria-valuetext'), document.querySelector('#notification-volume').style.getPropertyValue('--range-value')], list: [...document.querySelectorAll('.notification-sound-list li')].map(li => li.textContent) })),
        { options: ['Chime', 'Ding', 'ping.wav (uploaded)'], value: 'builtin:chime', volume: ['70', '70%', '70%', '0.7'], list: ['ping.wav · 4 KB '] });
      await page.close();
    }
    // The switches and the slider write the inbox's store and read back from it; the slider's text and fill follow.
    {
      const page = await mount();
      await page.$eval('#notification-muted', el => el.click());
      await page.$eval('#notification-text', el => el.click());
      await page.select('#notification-sound', 'builtin:ding');
      await page.$eval('#notification-volume', el => { el.value = '35'; el.dispatchEvent(new Event('input', { bubbles: true })); });
      await page.$eval('#notification-sound-preview', el => el.click());
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [window.__store.muted, window.__store.showText, window.__store.soundID, window.__store.volume, window.__store.plays, document.querySelector('#notification-volume-value').textContent, document.querySelector('#notification-volume').style.getPropertyValue('--range-value')]),
        [true, true, 'builtin:ding', '35', 1, '35%', '0.35']);
      await page.evaluate(() => { window.__store.muted = false; window.__page.open(); });
      await page.waitForFunction(() => !document.querySelector('#notification-muted').checked);
      await page.close();
    }
    // A chosen sound that is gone falls back to the chime in the store; non-admins do not see the upload row.
    {
      const page = await mount({ sound: 'user:gone', admin: false });
      assert.deepEqual(await page.evaluate(() => [window.__store.soundID, document.querySelector('#notification-sound').value, document.querySelector('#notification-sound-upload').closest('.settings-row').hidden, document.querySelector('#notification-sound-upload').disabled]), ['builtin:chime', 'builtin:chime', true, true]);
      await page.close();
    }
    // The browser-notification button follows what the browser says, and asking changes it.
    {
      const page = await mount({ permission: 'default', asks: 'granted' });
      const gap = await page.evaluate(() => document.querySelector('#notification-sound-preview').getBoundingClientRect().left - document.querySelector('#notification-sound').getBoundingClientRect().right);
      assert.ok(gap >= 6, `the sound select and Preview are apart: ${gap}px`);
      const button = () => page.$eval('#notification-permission', el => [el.disabled, el.textContent]);
      assert.deepEqual(await button(), [false, 'Enable browser notifications']);
      await page.$eval('#notification-permission', el => el.click());
      await page.waitForFunction(() => document.querySelector('#notification-permission').textContent === 'Enabled');
      assert.deepEqual([await button(), (await statuses(page)).at(-1)], [[true, 'Enabled'], 'Browser permission: granted.']);
      await page.close();
      const denied = await mount({ permission: 'denied' });
      assert.deepEqual([await denied.$eval('#notification-permission', el => [el.disabled, el.textContent]), (await statuses(denied)).at(-1)], [[true, 'Blocked in browser settings'], 'Browser permission: denied.']);
      await denied.close();
      const refused = await mount({ permission: 'default', asks: 'throws' });
      await refused.$eval('#notification-permission', el => el.click());
      await refused.waitForFunction(() => document.querySelector('#notification-status').textContent === 'The browser refused to ask');
      assert.equal(await refused.$eval('#notification-permission', el => el.disabled), false, 'a refused ask leaves the button as it was');
      await refused.evaluate(() => window.__page.open());
      await refused.waitForFunction(() => document.querySelector('#notification-status').textContent.startsWith('Browser permission'));
      await refused.close();
      const without = await mount();
      await without.evaluate(() => { window.__store.available = false; window.__page.open(); });
      await without.waitForFunction(() => document.querySelector('#notification-permission').disabled);
      await without.close();
    }
    // Two reads of the sound list answered out of order: only the newer one draws. A failed read says why and keeps what was drawn.
    {
      const page = await mount({ auto: {}, open: false });
      await page.evaluate(() => { window.__page.open(); window.__page.open(); });
      await page.waitForFunction(() => window.__requests.filter(request => request.path === '/api/sounds').length === 2);
      await answer(page, 1, 200, { sounds: [chime] });
      await page.waitForFunction(() => document.querySelectorAll('#notification-sound option').length === 1);
      await answer(page, 0, 200, { sounds: [chime, ding, ping] });
      await settle(page);
      assert.equal(await page.$$eval('#notification-sound option', options => options.length), 1, 'an older answer drew nothing');
      await page.evaluate(() => window.__page.open());
      await answer(page, await pending(page, 'GET', '/api/sounds'), 500, { message: 'Sounds are unavailable' });
      await page.waitForFunction(() => document.querySelector('#settings-notifications > p[role=status]').textContent === 'Sounds are unavailable');
      assert.equal(await page.$$eval('#notification-sound option', options => options.length), 1);
      await page.evaluate(() => window.__page.open());
      assert.equal((await statuses(page))[0], '', 'opening again clears the message');
      await page.close();
    }
    // The owner uploads and deletes one at a time; each is followed by a read of the list, and the new sound is chosen.
    {
      const page = await mount();
      const wav = path.join(os.tmpdir(), `${process.pid}-ping.wav`);
      fs.writeFileSync(wav, Buffer.concat([Buffer.from('RIFF\x24\x00\x00\x00WAVEfmt '), Buffer.alloc(64)]));
      await hold(page);
      await (await page.$('#notification-sound-upload')).uploadFile(wav);
      const post = await pending(page, 'POST', '/api/sounds');
      assert.deepEqual(await page.evaluate(index => [window.__requests[index].upload], post), [true]);
      assert.deepEqual([(await statuses(page))[0], await page.$eval('#notification-sound-upload', el => el.disabled)], ['Uploading…', true], 'the picker waits while an upload runs');
      await answer(page, post, 200, { id: 'user:p2', kind: 'user', name: 'new.wav', size: 100 });
      await answer(page, await pending(page, 'GET', '/api/sounds'), 200, { sounds: [chime, ding, ping, { id: 'user:p2', kind: 'user', name: 'new.wav', size: 100 }] });
      await page.waitForFunction(() => document.querySelectorAll('.notification-sound-list li').length === 2);
      assert.deepEqual([(await statuses(page))[0], await page.evaluate(() => window.__store.soundID), await page.$eval('#notification-sound-upload', el => [el.value, el.disabled])], ['Added new.wav.', 'user:p2', ['', false]]);
      await page.$eval('.notification-sound-list li:first-child button', el => el.click());
      const del = await pending(page, 'DELETE', '/api/sounds/p1');
      assert.equal(await page.$$eval('.notification-sound-list button', buttons => buttons.every(button => button.disabled)), true, 'delete buttons wait for the answer');
      await answer(page, del, 204);
      await answer(page, await pending(page, 'GET', '/api/sounds'), 200, { sounds: [chime, ding, { id: 'user:p2', kind: 'user', name: 'new.wav', size: 100 }] });
      await page.waitForFunction(() => document.querySelectorAll('.notification-sound-list li').length === 1);
      assert.equal((await statuses(page))[0], 'Deleted ping.wav.');
      await page.close();
    }
    // A write or read answered after the page is destroyed draws nothing and starts nothing.
    {
      const page = await mount();
      await hold(page);
      await page.evaluate(() => window.__page.open());
      const read = await pending(page, 'GET', '/api/sounds');
      await page.evaluate(() => window.__page.destroy());
      const made = await page.evaluate(() => window.__requests.length);
      await answer(page, read, 200, { sounds: [chime] });
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [window.__requests.length, document.querySelector('#settings-notifications').childElementCount]), [made, 0]);
      await page.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('notifications page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

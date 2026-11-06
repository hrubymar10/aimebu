// The message list's scroll in the real app, on a scratch server: opening a room lands on its newest message, a live message follows the bottom
// while the reader is there, and leaves a reader who scrolled up where they are. The rows are tall (markdown, a code block, a picture that
// loads late), so the list is long and its height settles after the rows are drawn; the distance from the bottom is read once the list has been
// still for several frames. Part of `make test-ui`.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { scratch, OWNER } = require('./scratch-server.cjs');

const body = i => `## Scroll ${i}\n\nA paragraph of **markdown** with \`code\`. ${'Words that wrap onto several lines. '.repeat(6)}\n\n\`\`\`js\nfunction f${i}(a, b) {\n  return a + b + ${i};\n}\n\`\`\`\n\n![late ${i}](/icons/logo-light.png?scroll=${i})\n`;

async function main() {
  const server = await scratch();
  try {
    const page = await server.open({ width: 1280, height: 800, context: await server.context() });
    await page.goto(server.origin);
    await page.waitForSelector('#login-view:not([hidden])');
    await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
    await page.$eval('#login-form', form => form.requestSubmit());
    await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
    if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
    await page.waitForSelector('#app-view:not([hidden])');
    // Pictures arrive late, as they do from a real server.
    await page.setRequestInterception(true);
    page.on('request', request => { if (request.url().includes('/icons/logo-light.png?')) setTimeout(() => request.continue().catch(() => {}), 300); else request.continue().catch(() => {}); });
    const folder = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'aimebu-scroll-'));
    execFileSync('git', ['init', '-q'], { cwd: folder });
    const call = (url, method, payload) => page.evaluate(async (url, method, payload) => {
      const csrf = (await (await fetch('/api/me')).json()).csrf_token;
      const response = await fetch(url, { method, headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: payload ? JSON.stringify(payload) : undefined });
      const text = await response.text(); return { status: response.status, body: text ? JSON.parse(text) : null };
    }, url, method, payload);
    const project = (await call('/api/projects', 'POST', { name: 'Scroll', folder })).body;
    const home = (await call(`/api/projects/${project.id}/rooms`, 'GET')).body.find(room => room.kind === 'home');
    const second = (await call(`/api/projects/${project.id}/rooms`, 'POST', { title: 'Scroll second' })).body;
    for (const room of [home, second]) for (let i = 0; i < 45; i++) assert.equal((await call(`/api/rooms/${room.id}/messages`, 'POST', { body: body(i) })).status, 201);
    await page.reload(); await page.waitForSelector('#app-view:not([hidden])');
    const list = '#messages';
    const quiet = () => page.evaluate(() => new Promise(resolve => {
      const el = document.querySelector('#messages'); let last = '', stable = 0;
      const tick = () => { const now = [el.scrollTop, el.scrollHeight, el.querySelectorAll('.chat-msg').length].join(); stable = now === last ? stable + 1 : 0; last = now; if (stable >= 20) resolve(true); else requestAnimationFrame(tick); };
      requestAnimationFrame(tick);
    }));
    const distance = () => page.$eval(list, el => Math.round(el.scrollHeight - el.scrollTop - el.clientHeight));
    const open = async (id, title) => {
      await page.waitForSelector(`[data-room-id="${id}"] .room-item-name`);
      await page.evaluate(id => document.querySelector(`[data-room-id="${id}"] .room-item-name`).click(), id);
      await page.waitForFunction(title => document.querySelector('#room-title')?.textContent === title, {}, title);
      await page.waitForFunction(() => document.querySelectorAll('#messages .chat-msg').length >= 40);
      await quiet();
    };
    for (let round = 0; round < 3; round++) {
      await open(second.id, 'Scroll second');
      assert.ok(await distance() < 5, `opening a long room lands on its newest message, ${await distance()}px short of it (round ${round})`);
      await open(home.id, 'Scroll');
      assert.ok(await distance() < 5, `going back to a room lands on its newest message, ${await distance()}px short of it (round ${round})`);
    }
    await open(second.id, 'Scroll second');
    const append = async text => {
      assert.equal((await call(`/api/rooms/${second.id}/messages`, 'POST', { body: `${text}\n\n\`\`\`js\nlet a = 1;\nlet b = 2;\n\`\`\`\n\n![late](/icons/logo-light.png?live=${encodeURIComponent(text)})\n` })).status, 201);
      await page.waitForFunction(text => [...document.querySelectorAll('.chat-msg-body')].some(el => el.textContent.startsWith(text)), {}, text);
      await quiet();
    };
    await append('Pinned live message');
    assert.ok(await distance() < 5, `a live message follows the bottom while the reader is there, ${await distance()}px short of it`);
    await page.$eval(list, el => { el.dispatchEvent(new WheelEvent('wheel', { bubbles: true })); el.scrollTop = 300; });
    await quiet();
    const placed = await page.$eval(list, el => el.scrollTop);
    await append('Scrolled up live message');
    assert.ok(Math.abs(await page.$eval(list, el => el.scrollTop) - placed) < 2, 'a live message leaves a reader who scrolled up where they are');
    assert.deepEqual(server.errors, [], `the page reported errors: ${server.errors.join(' | ')}`);
    console.log('scroll page: ok');
  } finally {
    await server.stop();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

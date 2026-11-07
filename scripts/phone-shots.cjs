// Phone screenshots of every screen, on a scratch server with seeded content: 390x844 and 360x740, dark and light. Not part of
// `make test-ui` (it reports, it asserts nothing).
//   node scripts/phone-shots.cjs <out-dir> [only-substring]
'use strict';
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { scratch } = require('./scratch-server.cjs');

const out = path.resolve(process.argv[2] || 'phone-shots');
const only = process.argv[3] || '';
const SIZES = [[390, 844], [360, 740]];
const THEMES = ['dark', 'light'];
const findings = [];
const settle = (page, ms = 350) => new Promise(resolve => setTimeout(resolve, ms));

async function main() {
  fs.mkdirSync(out, { recursive: true });
  const server = await scratch();
  const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'aimebu-shots-'));
  try {
    const call = await server.ownerClient();
    const folder = path.join(root, 'demo'); fs.mkdirSync(folder); execFileSync('git', ['init', '-q'], { cwd: folder });
    const project = (await call('/api/projects', { method: 'POST', body: { name: 'Demo project', folder } })).body;
    const other = path.join(root, 'second'); fs.mkdirSync(other); execFileSync('git', ['init', '-q'], { cwd: other });
    await call('/api/projects', { method: 'POST', body: { name: 'Second project', folder: other } });
    const room = (await call(`/api/projects/${project.id}/rooms`, { method: 'POST', body: { title: 'Phone review' } })).body;
    await call(`/api/projects/${project.id}/rooms`, { method: 'POST', body: { title: 'A room with a rather long name that has to truncate' } });
    const say = async (body, extra = {}) => (await call(`/api/rooms/${room.id}/messages`, { method: 'POST', body: { body, ...extra } })).body;
    await say('Welcome to the room. This is a **bold** claim with `inline code` and a [link](https://example.com).');
    const root1 = await say('A message with a table and a code block:\n\n| name | value |\n|---|---:|\n| alpha | 1 |\n| a much longer cell that forces the table to scroll | 22 |\n\n```js\nconst answer = 42; // a long line that goes past the edge of a phone screen for sure\n```');
    await say('A reply in the thread.', { thread_root: root1.id });
    await say('Another message with a very long unbroken token: ' + 'x'.repeat(120));
    await say('- a list\n- of items\n\n> and a quote');
    const shots = [];
    const frame = async (page, name, size, theme) => { if (only && !name.includes(only)) return; const file = path.join(out, `${name}-${size[0]}x${size[1]}-${theme}.png`); await settle(page); await page.screenshot({ path: file }); shots.push(file);
      const problems = await page.evaluate(() => {
        const vw = window.innerWidth, vh = window.innerHeight, found = [];
        const label = el => `${el.tagName.toLowerCase()}${el.id ? '#' + el.id : ''}${typeof el.className === 'string' && el.className ? '.' + el.className.trim().split(/\s+/).slice(0, 2).join('.') : ''}`;
        const scrolls = el => { for (let n = el.parentElement; n; n = n.parentElement) { const o = getComputedStyle(n); if (/(auto|scroll|hidden|clip)/.test(o.overflowX) && n.scrollWidth > n.clientWidth) return true; } return false; };
        for (const el of document.querySelectorAll('body *')) {
          const r = el.getBoundingClientRect(); if (!r.width || !r.height) continue; const cs = getComputedStyle(el); if (cs.visibility === 'hidden' || cs.display === 'none' || cs.opacity === '0' || el.closest('[hidden], [inert], dialog:not([open])')) continue;
          if ((r.right > vw + 1 || r.left < -1) && !scrolls(el)) found.push(`outside viewport: ${label(el)} ${Math.round(r.left)}..${Math.round(r.right)}`);
          if (/^(BUTTON|A|INPUT|SELECT)$/.test(el.tagName) && el.type !== 'hidden' && el.type !== 'checkbox' && el.type !== 'radio' && Math.min(r.width, r.height) < 32 && r.bottom > 0 && r.top < vh && !el.closest('[inert]')) found.push(`small target: ${label(el)} ${Math.round(r.width)}x${Math.round(r.height)}`);
          if (el.children.length === 0 && el.scrollWidth > el.clientWidth + 1 && /(hidden|clip)/.test(cs.overflowX) && cs.textOverflow !== 'ellipsis' && el.textContent.trim()) found.push(`clipped text: ${label(el)} "${el.textContent.trim().slice(0, 30)}"`);
        }
        for (const el of document.querySelectorAll('body *')) {
          if (el.scrollWidth > el.clientWidth + 1 && el.clientWidth > 0 && /(auto|scroll)/.test(getComputedStyle(el).overflowX) && !el.closest('[hidden], dialog:not([open])') && el.getBoundingClientRect().width) found.push(`sideways scroll: ${label(el)} content ${el.scrollWidth} > ${el.clientWidth}`);
        }
        if (document.documentElement.scrollWidth > vw + 1) found.push(`page scrolls sideways: ${document.documentElement.scrollWidth} > ${vw}`);
        return [...new Set(found)].slice(0, 12);
      });
      for (const problem of problems) findings.push(`${path.basename(file, '.png')}: ${problem}`); };

    for (const theme of THEMES) for (const size of SIZES) {
      const context = await server.context(); // a fresh browser context each time: signed out
      const page = await server.open({ width: size[0], height: size[1], context, reduced: true });
      await page.setViewport({ width: size[0], height: size[1], isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
      // sign-in
      await page.goto(server.origin);
      await page.waitForSelector('#login-view:not([hidden])');
      await frame(page, '01-signin', size, theme);
      const session = (await server.signIn(page)).body;
      await server.api(page, '/api/me/preferences', { method: 'PUT', body: { preferences: { theme } } }); // the theme is a saved preference of the account
      await page.evaluate((id, theme) => { localStorage.setItem('aimebu2.ui.last-user', id); localStorage.setItem('aimebu2.ui.theme.' + id, theme); }, session.user_id, theme);
      await page.goto(server.origin);
      await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden, { timeout: 30000 });
      if (await page.$('#welcome-continue')) { await frame(page, '02-welcome', size, theme); await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
      // The first-run steps in front of the app (set up a harness, ...) are screens too; each is skipped in turn.
      await settle(page, 1500);
      for (let step = 1; step <= 4; step++) {
        const shown = await page.evaluate(() => [...document.querySelectorAll('button')].some(b => /^skip for now$/i.test(b.textContent.trim()) && b.offsetParent !== null));
        if (!shown) break;
        await frame(page, `02b-onboarding-${step}`, size, theme);
        await page.evaluate(() => { const skip = [...document.querySelectorAll('button')].find(b => /^skip for now$/i.test(b.textContent.trim()) && b.offsetParent !== null); skip?.click(); });
        await settle(page, 600);
        if (await page.$('#app-dialog[open]')) { await frame(page, `02c-skip-confirm-${step}`, size, theme); await page.evaluate(() => document.querySelector('#app-dialog .app-dialog-ok')?.click()); }
        await settle(page, 900);
      }
      await page.waitForFunction(() => !document.querySelector('#app-view').hidden, { timeout: 30000 });
      await frame(page, '03-rooms-home', size, theme);
      // sidebar drawer
      await page.evaluate(() => document.querySelector('#rooms-toggle')?.click()); await frame(page, '04-sidebar-drawer', size, theme);
      // open the room
      await page.evaluate(title => { const row = [...document.querySelectorAll('#projects a, #projects button, #projects [role=button]')].find(el => el.textContent.includes(title)); row?.click(); }, 'Phone review');
      await settle(page, 700);
      await frame(page, '05-chat', size, theme);
      await page.evaluate(() => document.querySelector('#room-info')?.click()); await frame(page, '06-room-settings', size, theme);
      await page.evaluate(() => { const d = document.querySelector('#room-info-dialog'); d?.open && d.close(); });
      await page.evaluate(() => document.querySelector('#agents-toggle')?.click()); await frame(page, '07-members', size, theme);
      await page.evaluate(() => document.querySelector('#agents-close')?.click());
      // thread
      await page.evaluate(() => { const reply = [...document.querySelectorAll('#messages [data-thread], #messages .thread-link, #messages .reply-count, #messages button')].find(el => /repl/i.test(el.textContent) && /\d/.test(el.textContent)); reply?.click(); });
      await settle(page, 600); await frame(page, '08-thread', size, theme);
      await page.evaluate(() => document.querySelector('#close-thread')?.click());
      // top bar panels
      for (const [id, name] of [['inbox-toggle', '09-inbox'], ['health-toggle', '10-health'], ['knowledge-toggle', '11-memory'], ['usages-toggle', '12-usages'], ['more-toggle', '13-more']]) {
        await page.evaluate(id => document.querySelector('#' + id)?.click(), id); await frame(page, name, size, theme);
        await page.keyboard.press('Escape'); await page.evaluate(() => { document.querySelectorAll('dialog[open]').forEach(d => d.close()); });
        await page.evaluate(() => document.querySelector('#inbox-view') && !document.querySelector('#inbox-view').hidden && document.querySelector('#inbox-toggle')?.click());
      }
      // spawn dialog
      await page.evaluate(() => document.querySelector('#spawn-open')?.click()); await settle(page, 500); await frame(page, '14-spawn', size, theme);
      await page.evaluate(() => { document.querySelectorAll('dialog[open]').forEach(d => d.close()); });
      // new project dialog
      await page.evaluate(() => document.querySelector('#project-open')?.click()); await frame(page, '15-new-project', size, theme);
      await page.evaluate(() => { document.querySelectorAll('dialog[open]').forEach(d => d.close()); });
      // settings and each section
      await page.evaluate(() => document.querySelector('#settings-toggle')?.click()); await settle(page, 500);
      const tabs = await page.evaluate(() => [...document.querySelectorAll('#settings-dialog .settings-nav-item')].filter(el => el.offsetParent).map(el => el.id));
      await frame(page, '16-settings-nav', size, theme);
      for (const id of tabs) { await page.evaluate(id => document.getElementById(id)?.click(), id); await settle(page, 500); await frame(page, `17-settings-${id.replace(/^settings-|-tab$/g, '')}`, size, theme); }
      await page.close();
    }
    fs.writeFileSync(path.join(out, 'findings.txt'), findings.join('\n') + '\n');
    console.log(`${shots.length} shots in ${out}, ${findings.length} automatic findings (findings.txt)`);
    if (server.errors.length) console.log('page errors:', [...new Set(server.errors)].slice(0, 10));
  } finally { await server.stop(); fs.rmSync(root, { recursive: true, force: true }); }
}
main().catch(error => { console.error(error); process.exit(1); });

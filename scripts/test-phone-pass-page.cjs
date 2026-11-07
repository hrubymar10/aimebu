// What the phone pass fixed, in Chromium at 390px with touch: the Settings -> Users invite form stays inside the pane, a Markdown table fills the
// message instead of hiding its columns, touch controls are 40px tall, the composer's corner "Aa" does not show through a panel above it, and the
// code-block copy icon is visible in the light themes. web/ is served as is; no Go server. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const fixture = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css"><link rel="stylesheet" href="/v2-only.css"></head>
<body>
<section class="settings-section active" style="width:270px"><form class="user-invite-form"><div class="settings-group"><div class="settings-row"><div class="settings-row-info"><label class="settings-label">Invite as</label><span class="settings-desc">Access the invited person gets: viewer reads, operator can also run agents, owner manages the project and its people. Creates a one-time invitation link.</span></div><div class="settings-control"><select class="settings-select"><option>viewer</option></select></div></div></div></form></section>
<div class="chat-msg-body md-rendered" style="width:300px"><div class="md-table-wrap"><table class="md-table"><thead><tr><th class="md-th">name</th><th class="md-th md-right">value</th></tr></thead><tbody><tr><td class="md-td">a much longer cell that forces the table to scroll on a phone</td><td class="md-td md-right">22</td></tr></tbody></table></div>
<div class="md-codeblock"><button type="button" class="md-copy-btn"><span class="icon-mask icon-mask-copy"></span></button><pre><code>x</code></pre></div></div>
<div id="settings-dialog" style="width:300px"><button class="btn" id="plain-btn">Save</button><select id="plain-select"><option>a</option></select><input id="plain-input" type="number"></div>
<button class="row-btn thread-chip" id="chip">1 reply</button>
<div class="room-item" id="row"><span class="room-project" id="proj-plain">P</span><span class="room-project room-project-needed" id="proj-needed">P</span><button class="row-btn add-room" id="add"></button></div>
<div class="composer-field" id="field"><button class="btn-icon composer-aa" id="aa"></button></div>
</body></html>`;
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
  try {
    const page = await browser.newPage();
    await page.setViewport({ width: 390, height: 844, isMobile: true, hasTouch: true });
    await page.goto(`${origin}/fixture.html`);
    assert.equal(await page.evaluate(() => matchMedia('(pointer: coarse)').matches), true, 'the page is a touch page');
    const measure = await page.evaluate(() => {
      const box = selector => document.querySelector(selector).getBoundingClientRect();
      const wrap = document.querySelector('.md-table-wrap');
      return {
        inviteRight: box('.user-invite-form > .settings-group').right, formRight: box('.user-invite-form').right,
        tableScrolls: wrap.scrollWidth > wrap.clientWidth + 1, headerHeight: box('.md-th').height, headerLineHeight: parseFloat(getComputedStyle(document.querySelector('.md-th')).lineHeight) || 0,
        btn: box('#plain-btn').height, select: box('#plain-select').height, input: box('#plain-input').height,
        isolation: getComputedStyle(document.querySelector('#field')).isolation, projPlain: getComputedStyle(document.querySelector('#proj-plain')).display, projNeeded: getComputedStyle(document.querySelector('#proj-needed')).display, addAfter: getComputedStyle(document.querySelector('#add'), '::after').content, chipAfter: getComputedStyle(document.querySelector('#chip'), '::after').content, chipPosition: getComputedStyle(document.querySelector('#chip')).position,
      };
    });
    assert.ok(measure.inviteRight <= measure.formRight + 1, `the invite group stays inside its form (${measure.inviteRight} <= ${measure.formRight})`);
    assert.equal(measure.tableScrolls, false, 'a two-column table fits the message and does not scroll sideways');
    assert.ok(measure.headerHeight < 60, `the header "value" is not broken into letters (${measure.headerHeight}px tall)`);
    assert.ok(measure.btn >= 40 && measure.select >= 40 && measure.input >= 40, `buttons and settings fields are at least 40px tall on touch (${measure.btn}, ${measure.select}, ${measure.input})`);
    assert.equal(measure.isolation, 'isolate', 'the composer field has its own stacking context');
    assert.equal(measure.projPlain, 'none', 'on a phone the project chip that only repeats the heading above is hidden'); assert.notEqual(measure.projNeeded, 'none', 'and stays where it tells rooms apart (filter results)');
    assert.notEqual(measure.addAfter, 'none', 'the new-room button has a larger hit area');
    assert.equal(measure.chipPosition, 'relative'); assert.notEqual(measure.chipAfter, 'none', 'a chip has an invisible larger hit area');
    // The copy icon is not white in the light themes (v1 draws #fff on a pale grey there).
    for (const theme of ['light', 'blue-light']) {
      await page.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
      await new Promise(resolve => setTimeout(resolve, 500)); // the button's colour is transitioned
      const color = await page.evaluate(() => getComputedStyle(document.querySelector('.md-copy-btn')).color);
      assert.notEqual(color, 'rgb(255, 255, 255)', `the copy button's icon is not white in ${theme}`);
    }
    await page.evaluate(() => { delete document.documentElement.dataset.theme; });
    await new Promise(resolve => setTimeout(resolve, 500));
    assert.equal(await page.evaluate(() => getComputedStyle(document.querySelector('.md-copy-btn')).color), 'rgb(255, 255, 255)', 'dark keeps v1\'s white icon');
    console.log('phone pass page tests passed');
  } finally { await browser.close(); server.close(); }
}
main().catch(error => { console.error(error); process.exit(1); });

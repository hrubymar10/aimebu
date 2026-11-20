// Usages variant C (web/js/pages/usages/variant-c.js) in Chromium, against the app's own mock of the usages API: the headline that names the tightest
// limit, one row per account with a bar per main window, details on demand, icon-only actions, the admin's read-only view, the four themes and a phone.
// web/ is served as is with its import map; no Go server. Part of `make test-ui`. SCREENSHOT_DIR=<dir> also saves a picture of each theme and the phone.
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
const fixture = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css"><link rel="stylesheet" href="/v2-only.css">${importMap}</head>
<body><dialog id="settings-dialog" class="settings-modal" aria-label="Settings" data-view="section" open><div class="settings-pane"><nav class="settings-nav" aria-label="Settings sections"></nav><div class="settings-body"><header class="settings-body-header"></header><section id="settings-usages" class="settings-section active"><div id="usages-list"></div></section></div></div></dialog></body></html>`;
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
  const shots = process.env.SCREENSHOT_DIR;
  // `tight` makes Codex main's weekly window critical (8% left) and its Spark window a warning (15% left), so the headline and the colours have work to do.
  const mount = async ({ owner = true, tight = false, theme = '', width = 1000 } = {}) => {
    const page = await browser.newPage();
    await page.setViewport({ width, height: 900 });
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async (owner, tight, theme) => {
      if (theme) document.documentElement.dataset.theme = theme;
      const { MockUsagesSource } = await import('/js/usages-source.js');
      const { UsagesStore } = await import('/js/pages/usages/store.js');
      const { UsagesVariantC } = await import('/js/pages/usages/variant-c.js');
      const { html, render } = await import('/js/ui.js');
      const source = new MockUsagesSource();
      if (tight) {
        const list = source.usageList.bind(source);
        source.usageList = () => {
          const answer = list();
          const windows = answer.providers[0].profiles[0].windows;
          windows[1].percent_used = 92;
          windows[2].percent_used = 85;
          return answer;
        };
      }
      if (!owner) { const switcher = source.switcher.bind(source); source.switcher = async () => ({ ...(await switcher()), owner: false }); }
      window.__calls = [];
      for (const name of ['use', 'refresh', 'provider', 'settings', 'rename', 'remove']) {
        if (!source[name]) continue; // the mock leaves out the writes a test does not need
        const original = source[name].bind(source);
        source[name] = (...args) => { window.__calls.push([name, ...args]); return original(...args); };
      }
      const section = document.querySelector('#settings-usages'), dialog = document.querySelector('#settings-dialog');
      const store = new UsagesStore({ source, section, isOpen: () => true, isShown: () => true });
      store.owner.value = owner;
      await store.load();
      render(html`<${UsagesVariantC} store=${store} />`, document.querySelector('#usages-list'));
    }, owner, tight, theme);
    await page.waitForSelector('.usages-c');
    return page;
  };
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
  const shoot = async (page, name) => { if (shots) { fs.mkdirSync(shots, { recursive: true }); await page.screenshot({ path: path.join(shots, `${name}.png`), fullPage: true }); } };
  const row = (provider, profile) => `.usages-c .uc-provider[data-provider="${provider}"] .uc-row[data-profile="${profile}"]`;
  try {
    // The owner's page: the headline names the tightest window, the providers in order, an account per row with the active one first.
    {
      const page = await mount();
      assert.deepEqual(await page.evaluate(() => ({
        headline: document.querySelector('.uc-headline').className, text: document.querySelector('.uc-headline-text').textContent.replace(/\d+d \d+h|\d+h \d+m|\d+m/g, 'T'), // a countdown at a whole hour may read "3d 0h" or "2d 23h"
        providers: [...document.querySelectorAll('.uc-provider')].map(group => [group.dataset.provider, group.querySelector('.uc-provider-name').textContent, group.querySelector('.uc-provider-status').textContent, group.classList.contains('is-idle'), group.querySelectorAll('.uc-row').length]),
        codexRows: [...document.querySelectorAll('.uc-provider[data-provider=codex] .uc-row')].map(item => [item.dataset.profile, item.classList.contains('is-active'), item.querySelector('.uc-dot').getAttribute('aria-label')]),
        affected: document.querySelector('.uc-provider[data-provider=codex] .uc-affected').getAttribute('title'),
      })), {
        headline: 'uc-headline uc-level-ok', text: 'All accounts have headroom. Tightest: Codex · main, Weekly 36% left, runs out in T, resets in T.',
        providers: [['codex', 'Codex', 'Enabled', false, 2], ['claude-code', 'Claude Code', 'Enabled', false, 1], ['github-copilot', 'GitHub Copilot', 'Not configured', true, 0], ['mistral', 'Mistral', 'Not configured', true, 0], ['ollama-cloud', 'Ollama Cloud', 'Not configured', true, 0]],
        codexRows: [['main', true, 'Active account'], ['backup', false, 'Inactive account']],
        affected: 'milo@mock',
      });
      // A collapsed row shows the main windows only (Spark, at 93% left, waits in the details), each with its figure, pace tick and reset.
      assert.deepEqual(await page.evaluate(selector => {
        const lanes = [...document.querySelectorAll(`${selector} .uc-lanes .uc-lane`)];
        return { lanes: lanes.map(lane => [lane.dataset.window, lane.querySelector('.uc-bar').getAttribute('aria-label'), lane.querySelector('.uc-bar').getAttribute('aria-valuenow'), lane.querySelector('.uc-lane-value').textContent, !!lane.querySelector('.uc-pace'), /^\d+[dhm]/.test(lane.querySelector('.uc-lane-reset').textContent)]),
          detail: !!document.querySelector(`${selector} .uc-detail`), plan: document.querySelector(`${selector} .uc-plan`).textContent };
      }, row('codex', 'main')), { lanes: [['session', 'Session left', '58', '58%', true, true], ['weekly', 'Weekly left', '36', '36%', true, true]], detail: false, plan: 'Pro' });
      // Every action is an icon with a tooltip and a name, never text; only the row and headline disclosures are row buttons with words.
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('.usages-c button:not(.row-btn)')].filter(button => button.textContent.trim() || !button.title || button.title !== button.getAttribute('aria-label') || !button.querySelector('.icon-mask')).map(button => button.outerHTML)), []);
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('.usages-c .uc-switch')].map(button => [button.closest('.uc-row').dataset.profile, button.title, button.disabled])), [['backup', 'Switch to backup', false]], 'Switch is on the inactive switcher account only');
      // Opening a row shows every window with its 48-hour line and pace sentence, then the account's facts and its rename and delete.
      await page.click(`${row('codex', 'main')} .uc-row-toggle`);
      assert.deepEqual(await page.evaluate(selector => {
        const item = document.querySelector(selector), detail = item.querySelector('.uc-detail');
        return { expanded: item.querySelector('.uc-row-toggle').getAttribute('aria-expanded'), controls: item.querySelector('.uc-row-toggle').getAttribute('aria-controls') === detail.id,
          windows: [...detail.querySelectorAll('.uc-window .uc-lane')].map(lane => lane.dataset.window), spark: !!detail.querySelector('.uc-spark svg polyline'),
          pace: [...detail.querySelectorAll('.uc-pace-text')].map(node => node.textContent.replace(/\d+d \d+h|\d+h \d+m|\d+m/g, 'T')), paceHelp: /even spend/.test(detail.querySelector('.uc-pace-text').title),
          facts: [...detail.querySelectorAll('.uc-facts dt')].map(node => `${node.textContent}: ${node.nextElementSibling.textContent}`.replace(/Updated .*/, 'Updated <time>')),
          actions: [...detail.querySelectorAll('.uc-detail-actions button')].map(button => button.title) };
      }, row('codex', 'main')), { expanded: 'true', controls: true, windows: ['session', 'weekly', 'codex_spark'], spark: true, pace: ['13% in reserve · lasts until reset', '14% in deficit · runs out in T'], paceHelp: true,
        facts: ['Account: main@example.test', 'Credits: Credits: 12.50', 'Status: Updated <time>'], actions: ['Rename main', 'Delete main'] });
      await shoot(page, 'dark');
      // Switching asks first, naming who is affected.
      await page.click(`${row('codex', 'backup')} .uc-switch`);
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      assert.match(await page.$eval('#app-dialog .app-dialog-message', el => el.textContent), /Affected agents: milo@mock\. Switch to backup\?/);
      await page.$eval('#app-dialog .app-dialog-ok', el => el.click());
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'use'));
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'use')), ['use', 'codex', 'backup']);
      // A provider moves in the order, and the credentials of one the server cannot read open behind its ⋯ button.
      await page.$eval('.uc-provider[data-provider=claude-code] .uc-move-up', el => el.click());
      await page.waitForFunction(() => window.__calls.some(call => call[0] === 'settings'));
      assert.deepEqual(await page.evaluate(() => window.__calls.find(call => call[0] === 'settings')[1]), { provider_order: ['claude-code', 'codex', 'github-copilot', 'mistral', 'ollama-cloud'] });
      await page.$eval('.uc-provider[data-provider=mistral] .uc-setup-toggle', el => el.click());
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [!!document.querySelector('.uc-provider[data-provider=mistral] .uc-setup textarea'), document.querySelector('.uc-provider[data-provider=mistral] .uc-setup-toggle').getAttribute('aria-expanded')]), [true, 'true']);
      await page.close();
    }
    // Near the limit: the headline says so and names when it runs out and resets, the row edge and the bar turn critical, and a non-main window that crossed
    // a threshold (Spark) joins the collapsed row. The headline opens that account's row.
    {
      const page = await mount({ tight: true });
      assert.deepEqual(await page.evaluate(selector => ({
        headline: document.querySelector('.uc-headline').className, text: document.querySelector('.uc-headline-text').textContent.replace(/\d+d \d+h|\d+h \d+m|\d+m/g, 'T'), // a countdown at a whole hour may read "3d 0h" or "2d 23h"
        row: [...document.querySelector(selector).classList].filter(name => name.startsWith('uc-level-')),
        lanes: [...document.querySelectorAll(`${selector} .uc-lanes .uc-lane`)].map(lane => [lane.dataset.window, [...lane.classList].find(name => name.startsWith('uc-level-'))]),
      }), row('codex', 'main')), {
        headline: 'uc-headline uc-level-critical', text: 'Almost out: Codex · main, Weekly 8% left, runs out in T, resets in T.',
        row: ['uc-level-critical'], lanes: [['session', 'uc-level-ok'], ['weekly', 'uc-level-critical'], ['codex_spark', 'uc-level-warning']],
      });
      await page.click('.uc-headline-text');
      await page.waitForSelector(`${row('codex', 'main')} .uc-detail`);
      // Each theme keeps the three levels apart, and the bar's frame is visible against the page.
      for (const theme of ['', 'light', 'high-contrast-dark', 'high-contrast-light']) {
        const colours = await page.evaluate(async (theme, selector) => {
          if (theme) document.documentElement.dataset.theme = theme; else delete document.documentElement.dataset.theme;
          await new Promise(resolve => requestAnimationFrame(resolve));
          const fill = name => getComputedStyle(document.querySelector(`${selector} .uc-lanes .uc-lane[data-window="${name}"] .uc-fill`)).backgroundColor;
          const bar = getComputedStyle(document.querySelector(`${selector} .uc-bar`));
          return { ok: fill('session'), warning: fill('codex_spark'), critical: fill('weekly'), frame: bar.borderTopColor, page: getComputedStyle(document.querySelector(selector)).backgroundColor };
        }, theme, row('codex', 'main'));
        assert.equal(new Set([colours.ok, colours.warning, colours.critical]).size, 3, `${theme || 'dark'}: the three levels have three colours (${JSON.stringify(colours)})`);
        assert.notEqual(colours.frame, colours.page, `${theme || 'dark'}: the bar has a visible frame`);
        await shoot(page, `tight-${theme || 'dark'}`);
      }
      await page.close();
    }
    // An admin reads the limits: no provider actions, no switch, no rename or delete.
    {
      const page = await mount({ owner: false });
      await page.click(`${row('codex', 'main')} .uc-row-toggle`);
      assert.deepEqual(await page.evaluate(() => ({ actions: document.querySelectorAll('.uc-provider-actions').length, switches: document.querySelectorAll('.uc-switch').length, manage: document.querySelectorAll('.uc-detail-actions button').length, rows: document.querySelectorAll('.uc-row').length })),
        { actions: 0, switches: 0, manage: 0, rows: 3 });
      await page.close();
    }
    // A phone: nothing runs off the side, the bars take the full width under the name, and touch targets are at least 36px.
    {
      const page = await mount({ width: 390, tight: true });
      await page.click(`${row('codex', 'main')} .uc-row-toggle`);
      assert.deepEqual(await page.evaluate(selector => {
        const root = document.querySelector('.usages-c'), item = document.querySelector(selector);
        const name = item.querySelector('.uc-row-toggle').getBoundingClientRect(), lanes = item.querySelector('.uc-lanes').getBoundingClientRect(), main = item.querySelector('.uc-row-main').getBoundingClientRect();
        const overflow = [...root.querySelectorAll('*')].filter(node => node.getBoundingClientRect().right > document.documentElement.clientWidth + 0.5).map(node => node.className);
        return { overflow, below: lanes.top >= name.bottom - 1, wide: lanes.width > main.width * 0.85, touch: Math.min(...[...root.querySelectorAll('.uc-provider-actions .btn-icon, .uc-row-actions .btn-icon')].map(button => button.getBoundingClientRect().height)) >= 36 };
      }, row('codex', 'main')), { overflow: [], below: true, wide: true, touch: true });
      await shoot(page, 'phone');
      await page.close();
    }
    assert.deepEqual(errors, []);
    console.log('test-usages-c-page: ok');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

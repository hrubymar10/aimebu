// The Usages column opened from the top bar (UsagesPopover) in Chromium, on the real markup, with the app's mock of the usages API: the owner can refresh, an admin
// reads the same limits and the refresh button is off for them with the reason as its tooltip, and an admin's read never asks for the switcher or the credential tokens
// as a requirement (they are the owner's). web/ is served as is; no Go server. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
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
  const mount = async owner => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async owner => {
      const { MockUsagesSource } = await import('/js/usages-source.js');
      const { UsagesPopover } = await import('/js/usages-popover.js');
      const source = new MockUsagesSource();
      window.__calls = [];
      for (const name of ['list', 'switcher', 'refresh', 'credentialTargets']) {
        const original = source[name].bind(source);
        source[name] = (...args) => { window.__calls.push(name); return name !== 'list' && !owner ? Promise.reject(Object.assign(new Error('owner required'), { status: 403 })) : original(...args); };
      }
      document.querySelector('#usages-toggle').hidden = false;
      window.__popover = new UsagesPopover(source, () => {}, () => owner);
    }, owner);
    return page;
  };
  try {
    for (const owner of [true, false]) {
      const page = await mount(owner);
      await page.$eval('#usages-toggle', node => node.click());
      await page.waitForFunction(() => document.querySelector('.usages-popover-body .usages-provider-tile'));
      assert.deepEqual(await page.evaluate(() => {
        const view = document.querySelector('.usages-popover-alt .usages-b');
        const plan = view.querySelector('.usages-b-profile[data-profile=main] .switcher-tile-plan');
        return { visible: view.textContent.includes('main@example.test'), tooltip: plan.title,
          nameHasEmail: view.querySelector('.usages-b-profile[data-profile=main] .switcher-tile-name').title.includes('@') };
      }), { visible: false, tooltip: 'Pro · main@example.test', nameHasEmail: false }, 'the panel shows email only on the plan label tooltip');
      const view = await page.evaluate(() => ({ refresh: [document.querySelector('.usages-popover-refresh').disabled, document.querySelector('.usages-popover-refresh').title], tiles: document.querySelectorAll('.usages-popover-body .usages-provider-tile').length > 0,
        note: document.querySelector('.usages-popover-body')?.textContent.includes('limited to the instance owner'), switches: document.querySelectorAll('.usages-b-switch').length }));
      assert.deepEqual([view.refresh, view.tiles, view.note, view.switches], owner ? [[false, 'Refresh usages'], true, false, view.switches] : [[true, 'Only the owner can refresh usages'], true, false, 0], `${owner ? 'owner' : 'admin'}: ${JSON.stringify(view)}`);
      // A profile whose data is older than twice the refresh interval keeps its numbers but is drawn grey (the bar and the figures), claims no pace and
      // shows no live countdown, while a fresh profile beside it is unchanged.
      await page.evaluate(async () => {
        const list = window.__popover.source.list.bind(window.__popover.source);
        window.__popover.source.list = async () => { const out = await list(); out.providers[0].profiles[0].last_refresh_at = new Date(Date.now() - 2 * 86400000).toISOString(); return out; };
        await window.__popover.load();
      });
      const stale = await page.evaluate(() => {
        const color = value => { const probe = document.createElement('i'); probe.style.color = value; document.body.append(probe); const out = getComputedStyle(probe).color; probe.remove(); return out; };
        const card = document.querySelector('.usages-b-profile.is-stale'), fresh = document.querySelector('.usages-b-profile:not(.is-stale)');
        const fill = node => node && getComputedStyle(node.querySelector('.usages-progress span')).backgroundColor;
        return { stale: !!card, fill: card && fill(card), muted: color(getComputedStyle(document.documentElement).getPropertyValue('--text-muted').trim()), freshFill: fresh && fill(fresh),
          pace: card && card.querySelectorAll('.usages-pace-text, .usages-pace-tick').length, countdown: card && /Resets in/.test(card.textContent), reset: card && [...card.querySelectorAll('.usages-window-meta span')].map(node => node.textContent) };
      });
      assert.ok(stale.stale, 'the old profile is stale');
      assert.equal(stale.fill, stale.muted, `a stale profile's bar is the muted grey: ${JSON.stringify(stale)}`);
      assert.notEqual(stale.freshFill, stale.muted, 'a fresh profile keeps its colour');
      assert.deepEqual([stale.pace, stale.countdown], [0, false], `no pace claim and no live countdown on a stale profile: ${JSON.stringify(stale)}`);
      assert.ok(stale.reset.every(text => /^(Reset passed|Resets .+)$/.test(text) && !/ in /.test(text)), `its reset is a fact, not a countdown: ${JSON.stringify(stale.reset)}`);
      await page.$eval('#usages-popover', () => window.__popover.close());
      await page.evaluate(() => window.__popover.reset());
      assert.deepEqual(await page.evaluate(() => [document.querySelector('.usages-popover-body').children.length, document.querySelector('.usages-popover-body').textContent]), [0, ''], 'a sign-out leaves no tile in the column');
      if (!owner) {
        await page.$eval('.usages-popover-refresh', node => node.click());
        assert.equal(await page.evaluate(() => window.__calls.filter(name => name === 'refresh').length), 0, 'a refresh the admin may not make is not asked for');
      }
      await page.close();
    }
    // The panel draws the Usages view, read-only: the actions are the header's own and it opens on the first click.
    {
      const page = await mount(true);
      await page.$eval('#usages-toggle', node => node.click());
      await page.waitForFunction(() => document.querySelector('.usages-popover-body .usages-provider-tile'));
      await page.waitForSelector('.usages-popover-alt .usages-b .usages-b-tile');
      {
        // The actions are the panel header's own (close, refresh, group, side by side), once: B draws no row of its own, and the bar and active name carry no border or underline.
        assert.deepEqual(await page.evaluate(() => { const head = document.querySelector('#usages-popover > header'), alt = document.querySelector('.usages-popover-alt'), btn = sel => head.querySelector(sel), bar = alt.querySelector('.usages-progress'), name = alt.querySelector('.usages-b-profile.is-active .switcher-tile-name');
          return { refresh: head.querySelectorAll('.usages-popover-refresh').length, group: !btn('.usages-popover-group').hidden, close: !!btn('.usages-popover-close'), row: !!alt.querySelector('.usages-b-bar, .usages-b-refresh-all, .usages-b-group, .usages-b-actions'), refreshes: document.querySelectorAll('#usages-popover [aria-label="Refresh usages"]').length,
            underline: name ? getComputedStyle(name).textDecorationLine : 'none', border: getComputedStyle(bar).borderTopWidth }; }),
          { refresh: 1, group: true, close: true, row: false, refreshes: 1, underline: 'none', border: '0px' }, 'B uses the panel header toolbar; no second row; no underline; no bar border');
        // The panel is view-only, as v1's was: no gear or management buttons, only Switch on an inactive profile.
        assert.deepEqual(await page.evaluate(() => { const alt = document.querySelector('.usages-popover-alt'); return { manage: alt.querySelectorAll('.usages-b-manage, .usages-b-manage-panel, .usages-b-rename, .usages-b-remove, .usages-b-toggle, .usages-b-add, .usages-b-import, .usages-b-refresh').length, switches: alt.querySelectorAll('.usages-b-profile:not(.is-active) .usages-b-switch').length > 0 }; }), { manage: 0, switches: true }, 'B in the panel is read-only apart from Switch');
        await page.$eval('#usages-popover .usages-popover-group', node => node.click());
        assert.equal(await page.evaluate(() => [document.querySelector('#usages-popover .usages-popover-group').getAttribute('aria-pressed'), document.querySelectorAll('.usages-popover-alt .usages-b-grid').length].join()), 'true,2', 'the header group button groups B');
        await page.$eval('#usages-popover .usages-popover-group', node => node.click());
        // An error from an owner action shows above the view; a push is held while a field has focus; a switcher that cannot be read leaves the figures.
        await page.evaluate(() => window.__popover.store.setError('Account switch failed'));
        await page.waitForFunction(() => document.querySelector('.usages-popover-alt .usages-popover-error')?.textContent === 'Account switch failed');
        const lists = () => page.evaluate(() => window.__calls.filter(name => name === 'list').length);
        await page.evaluate(() => { const input = document.createElement('input'); input.id = 'typing'; document.querySelector('.usages-popover-alt').append(input); input.focus(); });
        const before = await lists();
        await page.evaluate(() => { document.querySelector('#boot-view')?.setAttribute('hidden', ''); document.querySelector('#app-view').hidden = false; }); await page.focus('#typing'); await page.evaluate(() => window.__popover.changed()); await new Promise(resolve => setTimeout(resolve, 150));
        assert.equal(await lists(), before, 'a push is held while a field in the panel has focus');
        await page.$eval('#typing', node => node.blur()); await page.waitForFunction(n => window.__calls.filter(name => name === 'list').length > n, {}, before);
        await page.$eval('#typing', node => node.remove());
      }
      await page.close();
    }
    {
      const page = await mount(true);
      await page.evaluate(() => { window.__popover.source.switcher = () => Promise.reject(Object.assign(new Error('unavailable'), { status: 503 })); });
      await page.$eval('#usages-toggle', node => node.click());
      await page.waitForSelector('.usages-popover-alt .usages-b .usages-b-tile');
      await page.close();
    }
    // Strictly read-only: a reauth_required profile gets no Install anyway in the panel; with every provider off the panel offers Open Settings.
    {
      const page = await mount(true);
      await page.evaluate(() => {
        const list = window.__popover.source.list.bind(window.__popover.source);
        window.__popover.source.list = async () => ({ ...(await list()), credential_alerts: [{ kind: 'reauth_required', tool: 'codex', profile: 'backup', role: 'store', message: 'Sign in again' }] });
        window.__opened = 0; window.__popover.openSettings = () => { window.__opened++; };
      });
      await page.$eval('#usages-toggle', node => node.click());
      await page.waitForSelector('.usages-popover-alt .usages-b-profile[data-profile=backup]');
      assert.equal(await page.evaluate(() => document.querySelectorAll('.usages-popover-alt .usages-b-install-anyway').length), 0, 'no Install anyway in the panel');
      await page.evaluate(async () => {
        const list = window.__popover.source.list.bind(window.__popover.source);
        window.__popover.source.list = async () => { const out = await list(); out.providers.forEach(provider => { provider.enabled = false; }); return out; };
        await window.__popover.load();
      });
      await page.waitForSelector('.usages-popover-alt .usages-popover-settings');
      await page.$eval('.usages-popover-alt .usages-popover-settings', node => node.click());
      assert.deepEqual(await page.evaluate(() => [window.__opened, document.querySelector('#usages-popover').hidden]), [1, true], 'Open Settings closes the panel and opens Usages');
      await page.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('usages popover page: ok');
  } finally { await browser.close(); server.close(); }
}
main().catch(error => { console.error(error); process.exit(1); });

// The Usages page's automatic-switching block, its limits-before-100% block and its default daily limits, in Chromium, over sources whose
// calls are recorded and, where a test asks, held until the test lets them go, so the order of answers is chosen. web/ is served as is with
// its import map; no Go server. Part of `make test-ui`.
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
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/v1.css"><link rel="stylesheet" href="/style.css"><link rel="stylesheet" href="/theme-v1.css">${importMap}</head>
<body><dialog id="settings-dialog" open><section id="settings-usages" class="settings-content"></section></dialog></body></html>`;
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
  // The three blocks are mounted straight into the section over in-page sources: `held` lists the source methods to hold (a held call waits for
  // window.__release(name), the oldest first); `fail` makes load answer with that status.
  const mount = async ({ held = [], fail = 0 } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async (held, fail) => {
      const config = (names, extra = {}) => ({ enabled: false, dry_run: true, profiles: names.map(name => ({ name, session_max: 95, weekly_max: 95 })), protected: [], after_reset: null, warmup_seconds: 60, min_dwell_minutes: 5, ...extra });
      const tool = (names, extra = {}, top = {}) => ({ config: config(names, extra), saved: false, state: { evaluated_at: '2026-01-01T10:00:00Z' }, last_decision: null, decisions: [], ...top });
      window.__quotaData = { profiles: { claude: ['local', 'spare', 'parked'], codex: ['main', 'backup'] }, tools: {
        claude: tool(['local', 'spare'], { protected: ['parked'] }, { state: { evaluated_at: '2026-01-01T10:00:00Z', reset_latched_at: '2026-01-01T09:00:00Z', weekly_max_seen: 91, hold: 'warming up' }, decisions: [{ at: '2026-01-01T10:00:00Z', action: 'hold', from: 'local', reason: 'warming up' }] }),
        codex: tool(['main', 'backup']) } };
      const guard = (soft, hard, state = 'ok') => ({ config: { enabled: true, soft, hard, profiles: {} }, saved: false, result: { state, profile: 'local', percent: 40, window: 'session' }, held: false });
      window.__guardData = { profiles: { claude: ['local', 'spare'], codex: ['main'] }, tools: { claude: guard(95, 99), codex: guard(98, 99) } };
      window.__guardState = { can_edit: true, daily_cost_cap: null, daily_turn_cap: null };
      window.__calls = []; window.__waiting = {};
      const wrap = (target, names, base) => { for (const name of names) target[name] = (...args) => { window.__calls.push([name, ...args]); const run = () => base[name](...args);
        if (!held.includes(name)) return Promise.resolve().then(run); return new Promise((resolve, reject) => { (window.__waiting[name] ||= []).push(() => Promise.resolve().then(run).then(resolve, reject)); }); }; };
      const failure = () => { const error = new Error('Not here'); error.status = fail; return error; };
      const copy = value => JSON.parse(JSON.stringify(value));
      const quota = { async load() { if (fail) throw failure(); return copy(window.__quotaData); },
        async save(name, body) { window.__quotaData.tools[name] = { ...window.__quotaData.tools[name], config: copy(body), saved: true }; return copy(window.__quotaData.tools[name]); },
        async rearm(name) { const next = window.__quotaData.tools[name]; delete next.state.reset_latched_at; return copy(next); }, async reenable(name) { return copy(window.__quotaData.tools[name]); } };
      const guards = { async load() { if (fail) throw failure(); return copy(window.__guardData); },
        async save(name, body) { if (window.__guardRefuse) throw new Error(window.__guardRefuse); window.__guardData.tools[name] = { ...window.__guardData.tools[name], config: copy(body), saved: true }; return copy(window.__guardData.tools[name]); } };
      const sources = { quota: {}, guard: {} };
      wrap(sources.quota, ['load', 'save', 'rearm', 'reenable'], quota); wrap(sources.guard, ['load', 'save'], guards);
      window.__release = name => { (window.__waiting[name] || []).shift()?.(); };
      const send = async (path, options = {}) => { window.__calls.push(['default', path, options.method || 'GET', options.body]); if (fail) throw failure();
        if (options.method === 'PUT') { if (window.__defaultRefuse) throw new Error(window.__defaultRefuse); window.__guardState = { ...window.__guardState, ...options.body }; } return copy(window.__guardState); };
      const v = new URL(document.querySelector('script[type=module]')?.src || location.href).search;
      const { QuotaStore } = await import(`/js/pages/usages/quota-store.js${v}`), { QuotaBlock } = await import(`/js/pages/usages/quota-block.js${v}`);
      const { GuardStore } = await import(`/js/pages/usages/guard-store.js${v}`), { GuardBlock } = await import(`/js/pages/usages/guard-block.js${v}`);
      const { LimitDefaultStore, LimitDefault } = await import(`/js/pages/usages/limit-default.js${v}`), { html, render } = await import(`/js/ui.js${v}`);
      const section = document.querySelector('#settings-usages');
      const focused = (store, selector) => !!store.node && store.node.contains(document.activeElement) && document.activeElement.matches(selector);
      const stores = window.__stores = { quota: new QuotaStore({ source: sources.quota, isEditing: () => focused(stores.quota, 'input, select, textarea') }), guard: new GuardStore({ source: sources.guard, isEditing: () => focused(stores.guard, 'input, select') }), limit: new LimitDefaultStore({ send }) };
      render(html`<${QuotaBlock} store=${stores.quota} /><${GuardBlock} store=${stores.guard} /><${LimitDefault} store=${stores.limit} />`, section);
      window.__render = () => new Promise(resolve => setTimeout(resolve, 80));
    }, held, fail);
    return page;
  };
  const sync = async (page, what, options) => { await page.evaluate(async (what, options) => { await window.__stores[what].sync(options); await window.__render(); }, what, options); };
  const calls = (page, name) => page.evaluate(name => window.__calls.filter(call => call[0] === name).length, name);
  const settle = page => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 100)));
  const click = (page, selector) => page.$eval(selector, el => el.click());
  const type = (page, selector, value) => page.$eval(selector, (el, value) => { el.value = value; el.dispatchEvent(new Event('input', { bubbles: true })); }, value);
  const claude = '.quota-tool[data-tool="claude"]', codex = '.quota-tool[data-tool="codex"]';
  const names = (page, tool) => page.$$eval(`${tool} [aria-label$="session limit (%)"]`, els => els.map(el => el.getAttribute('aria-label').replace(' session limit (%)', '')));
  try {
    // Hidden until the owner of an enabled switcher asks; then each tool, its notes and its order.
    {
      const page = await mount();
      assert.deepEqual(await page.evaluate(() => [document.querySelector('.quota-settings').hidden, document.querySelector('.quota-settings').childElementCount, document.querySelector('.limit-guard-settings').hidden, document.querySelector('.usage-limit-default').hidden]), [true, 0, true, true]);
      await sync(page, 'quota', { owner: false, enabled: true });
      assert.equal(await calls(page, 'load'), 0, 'a non-owner is not asked');
      await sync(page, 'quota', { owner: true, enabled: false });
      assert.equal(await calls(page, 'load'), 0, 'a switcher that is off is not asked');
      await sync(page, 'quota', { owner: true, enabled: true });
      assert.deepEqual(await page.evaluate(() => ({ titles: [...document.querySelectorAll('.quota-tool-title')].map(el => el.textContent), defaults: document.querySelectorAll('.quota-defaults').length, hold: document.querySelector('.quota-tool[data-tool="claude"] .quota-hold').textContent,
        reset: document.querySelectorAll('.quota-tool[data-tool="claude"] .quota-reset button').length, noReset: document.querySelector('.quota-tool[data-tool="codex"] .quota-reset'), parked: document.querySelector('[data-quota-focus="protect:parked"]').checked,
        add: document.querySelector('#quota-claude-order-add').disabled && document.querySelector('#quota-claude-order-add').options[0].textContent, save: document.querySelector('.quota-tool[data-tool="claude"] .quota-save').disabled, discard: document.querySelector('.quota-tool[data-tool="claude"] .quota-discard').disabled,
        decisions: [document.querySelector('.quota-tool[data-tool="claude"] .quota-decisions').open, document.querySelector('.quota-tool[data-tool="claude"] .quota-decisions summary').textContent] })),
      { titles: ['Claude', 'Codex'], defaults: 2, hold: 'Current hold: warming up', reset: 1, noReset: null, parked: true, add: 'No other profiles', save: false, discard: true, decisions: [false, 'Recent decisions (1)'] });
      assert.deepEqual(await names(page, claude), ['local', 'spare']);
      await sync(page, 'quota', { owner: true, enabled: false });
      assert.deepEqual(await page.evaluate(() => [document.querySelector('.quota-settings').hidden, document.querySelector('.quota-settings').childElementCount]), [true, 0], 'turning the switcher off hides and empties the block');
      await page.close();
    }
    // Editing: a move keeps focus on the row, a bad limit is refused before anything is sent, a save says it waits and goes once.
    {
      const page = await mount({ held: ['save'] });
      await sync(page, 'quota', { owner: true, enabled: true });
      await click(page, `${claude} [aria-label="Move local down"]`);
      await settle(page);
      assert.deepEqual([await names(page, claude), await page.evaluate(() => document.activeElement?.getAttribute('aria-label'))], [['spare', 'local'], 'Move local up'], 'focus stayed with the moved row');
      assert.equal(await page.$eval(`${claude} .quota-unsaved`, el => !el.hidden), true);
      await type(page, `${claude} [aria-label="spare weekly limit (%)"]`, '0');
      await click(page, `${claude} .quota-save`);
      await settle(page);
      assert.match(await page.$eval(`${claude} .quota-error`, el => el.textContent), /limits for spare must be whole numbers from 1 to 100/);
      assert.equal(await calls(page, 'save'), 0, 'a refused draft is not sent');
      await type(page, `${claude} [aria-label="spare weekly limit (%)"]`, '80');
      await settle(page);
      assert.equal(await page.$eval(`${claude} .quota-error`, el => el.textContent), 'The limits for spare must be whole numbers from 1 to 100.', 'typing alone leaves the message');
      await click(page, '#quota-claude-enabled');
      await settle(page);
      assert.equal(await page.$eval(`${claude} .quota-error`, el => el.textContent), '', 'a change to the draft clears it');
      await page.evaluate(() => { document.querySelector('.quota-tool[data-tool="claude"] .quota-save').click(); document.querySelector('.quota-tool[data-tool="claude"] .quota-save').click(); });
      await page.waitForFunction(() => window.__waiting.save?.length === 1);
      await settle(page);
      assert.deepEqual(await page.evaluate(() => [window.__calls.filter(call => call[0] === 'save').length, document.querySelector('.quota-tool[data-tool="claude"] .quota-note').textContent, document.querySelector('.quota-tool[data-tool="claude"] .quota-save').disabled]), [1, 'Saving… (waits for a running switch)', true], 'the save waits behind a switch, once');
      await page.evaluate(() => window.__release('save'));
      await page.waitForFunction(() => document.querySelector('.quota-tool[data-tool="claude"] .quota-note')?.textContent === 'Saved.');
      assert.deepEqual(await page.evaluate(() => [document.activeElement === document.querySelector('.quota-tool[data-tool="claude"] .quota-note'), document.querySelectorAll('.quota-tool[data-tool="claude"] .quota-defaults').length, document.querySelector('.quota-tool[data-tool="claude"] .quota-save').disabled, window.__calls.find(call => call[0] === 'save')[2].enabled]), [true, 0, true, true], 'saved: focus on the note, no defaults note, Save disabled again');
      assert.match(await page.$eval(`${claude} .quota-dry-run`, el => el.textContent), /Dry run: decisions are recorded/);
      await page.close();
    }
    // Protecting leaves the order, discard restores, after-reset reselects its watched profile, and a draft below two profiles is refused.
    {
      const page = await mount();
      await sync(page, 'quota', { owner: true, enabled: true });
      await click(page, `${claude} [data-quota-focus="protect:local"]`);
      await settle(page);
      assert.deepEqual(await names(page, claude), ['spare'], 'a protected profile leaves the order');
      await click(page, `${claude} .quota-discard`);
      await settle(page);
      assert.deepEqual(await names(page, claude), ['local', 'spare']);
      await click(page, '#quota-claude-after-reset');
      await settle(page);
      assert.equal(await page.$eval('#quota-claude-watched', el => el.value), 'local');
      await page.click(`${claude} [data-quota-focus="protect:local"]`);
      await settle(page);
      assert.equal(await page.$eval('#quota-claude-watched', el => el.value), 'spare', 'the watched profile moved to one that is still listed');
      await click(page, '#quota-claude-enabled');
      await click(page, `${claude} .quota-save`);
      await settle(page);
      assert.match(await page.$eval(`${claude} .quota-error`, el => el.textContent), /after the reset needs at least two profiles/);
      assert.equal(await calls(page, 'save'), 0);
      await click(page, `${claude} .quota-discard`);
      await settle(page);
      assert.equal(await page.$('#quota-claude-watched'), null);
      await page.close();
    }
    // Re-arm: the latch goes, the draft in progress stays; a server that refuses says why.
    {
      const page = await mount();
      await sync(page, 'quota', { owner: true, enabled: true });
      await type(page, `${claude} [aria-label="local weekly limit (%)"]`, '70');
      await click(page, `${claude} .quota-reset button`);
      await page.waitForFunction(() => !document.querySelector('.quota-tool[data-tool="claude"] .quota-reset') && document.querySelector('.quota-tool[data-tool="claude"] .quota-note')?.textContent === 'Re-armed.');
      assert.equal(await page.$eval(`${claude} [aria-label="local weekly limit (%)"]`, el => el.value), '70', 'a re-arm keeps the draft being edited');
      await page.evaluate(() => { window.__stores.quota.source.reenable = async () => { throw new Error('Not allowed'); }; window.__quotaData.tools.claude.state.tripped = true; });
      await sync(page, 'quota', { owner: true, enabled: true });
      await click(page, `${claude} .quota-banner button`);
      await page.waitForFunction(() => document.querySelector('.quota-tool[data-tool="claude"] .quota-error')?.textContent === 'Not allowed');
      await page.close();
    }
    // Reads answered out of order: only the later request draws; a failing read says why but keeps what is shown; 403 and 404 hide the block.
    {
      const page = await mount({ held: ['load'] });
      await page.evaluate(() => { window.__stores.quota.sync({ owner: true, enabled: true }); window.__stores.quota.sync({ owner: true, enabled: true }); });
      await page.waitForFunction(() => window.__waiting.load?.length === 2);
      await page.evaluate(() => { window.__quotaData.tools.claude.state.hold = 'older'; window.__release('load'); });
      await settle(page);
      assert.equal(await page.evaluate(() => document.querySelector('.quota-settings').hidden), true, 'a read superseded by a later one draws nothing, even answered first');
      await page.evaluate(() => { window.__quotaData.tools.claude.state.hold = 'newer'; window.__release('load'); });
      await page.waitForFunction(() => document.querySelector('.quota-tool[data-tool="claude"] .quota-hold')?.textContent === 'Current hold: warming up' || document.querySelector('.quota-tool[data-tool="claude"] .quota-hold'));
      await page.close();
      const forbidden = await mount({ fail: 403 });
      await sync(forbidden, 'quota', { owner: true, enabled: true });
      assert.equal(await forbidden.evaluate(() => document.querySelector('.quota-settings').hidden), true, 'a 403 means this person is not the owner');
      await forbidden.close();
      const broken = await mount();
      await sync(broken, 'quota', { owner: true, enabled: true });
      await broken.evaluate(() => { window.__stores.quota.source.load = async () => { throw new Error('The server is busy'); }; });
      await sync(broken, 'quota', { owner: true, enabled: true });
      assert.deepEqual(await broken.evaluate(() => [document.querySelector('.quota-settings > .error').textContent, document.querySelectorAll('.quota-tool').length]), ['The server is busy', 2], 'a failed read says why and keeps the tools');
      await broken.close();
    }
    // A push while the owner types: the field and its draft survive, the status moves, another tool's server copy waits until focus leaves, and
    // a focused field of a tool with no edits is brought up to date at once.
    {
      const page = await mount();
      await sync(page, 'quota', { owner: true, enabled: true });
      const input = `${claude} [aria-label="local weekly limit (%)"]`;
      await page.focus(input);
      await type(page, input, '9');
      await page.evaluate(() => { window.__before = document.querySelector('.quota-tool[data-tool="claude"] .quota-evaluated').textContent; window.__field = document.activeElement; });
      await page.evaluate(() => { const data = window.__quotaData.tools; data.claude.state.evaluated_at = '2026-01-01T11:00:00Z'; data.claude.decisions.unshift({ at: '2026-01-01T11:00:00Z', action: 'stay', from: 'local' }); data.codex.config.enabled = true; data.codex.saved = true; });
      await sync(page, 'quota', { owner: true, enabled: true });
      assert.deepEqual(await page.evaluate(() => ({ same: document.activeElement === window.__field, value: window.__field.value, moved: document.querySelector('.quota-tool[data-tool="claude"] .quota-evaluated').textContent !== window.__before,
        decisions: document.querySelectorAll('.quota-tool[data-tool="claude"] .quota-decision').length, codexWaits: document.querySelector('#quota-codex-enabled').checked })), { same: true, value: '9', moved: true, decisions: 2, codexWaits: false });
      await page.evaluate(() => document.activeElement.blur());
      await page.waitForFunction(() => document.querySelector('#quota-codex-enabled').checked);
      assert.equal(await page.$eval(input, el => el.value), '9', 'the draft survives the catch-up');
      await page.focus('#quota-codex-dry-run');
      await page.evaluate(() => { window.__quotaData.tools.codex.config.dry_run = false; });
      await sync(page, 'quota', { owner: true, enabled: true });
      assert.deepEqual(await page.evaluate(() => [document.querySelector('#quota-codex-dry-run').checked, document.activeElement?.id]), [false, 'quota-codex-dry-run'], 'a clean focused field is updated at once and keeps focus');
      await page.close();
    }
    // A save answered after the page is gone draws nothing.
    {
      const page = await mount({ held: ['save'] });
      await sync(page, 'quota', { owner: true, enabled: true });
      await click(page, `${claude} .quota-save`);
      await page.waitForFunction(() => window.__waiting.save?.length === 1);
      await page.evaluate(async () => { const { render } = await import(`/js/ui.js${new URL(document.querySelector('script[type=module]')?.src || location.href).search}`); window.__stores.quota.destroy(); render(null, document.querySelector('#settings-usages')); window.__release('save'); });
      await settle(page);
      assert.equal(await page.evaluate(() => document.querySelector('#settings-usages').childElementCount), 0);
      await page.close();
    }
    // Limits before 100%: defaults, soft below hard, save, a per-profile override, and a server that refuses.
    {
      const page = await mount();
      await sync(page, 'guard', { owner: true });
      assert.deepEqual(await page.evaluate(() => ({ titles: [...document.querySelectorAll('.guard-tool-title')].map(el => el.textContent), live: document.querySelector('.guard-tool[data-tool="claude"] .guard-live').textContent, values: [document.querySelector('#guard-claude-soft').value, document.querySelector('#guard-claude-hard').value, document.querySelector('#guard-claude-enabled').checked], save: document.querySelector('.guard-tool[data-tool="claude"] .guard-save').disabled })),
        { titles: ['Claude', 'Codex'], live: 'Claude (local) is at 40% of its session window.', values: ['95', '99', true], save: false });
      const guard = '.guard-tool[data-tool="claude"]';
      await type(page, '#guard-claude-soft', '100');
      await click(page, `${guard} .guard-save`);
      await settle(page);
      assert.match(await page.$eval(`${guard} .guard-error`, el => el.textContent), /soft threshold must be below the hard one/);
      assert.equal(await calls(page, 'save'), 0);
      await type(page, '#guard-claude-soft', '80'); await type(page, '#guard-claude-hard', '90');
      await click(page, `${guard} .guard-save`);
      await page.waitForFunction(() => document.querySelector('.guard-tool[data-tool="claude"] .guard-note')?.textContent === 'Saved.');
      assert.deepEqual(await page.evaluate(() => [window.__guardData.tools.claude.config.soft, window.__guardData.tools.claude.config.hard, document.querySelector('.guard-tool[data-tool="claude"] .guard-save').disabled]), [80, 90, true]);
      await page.select('#guard-claude-add', 'spare');
      await click(page, `${guard} [aria-label="Give the chosen Claude profile its own thresholds"]`);
      await settle(page);
      assert.deepEqual(await page.$eval(guard, el => [el.querySelector('#guard-claude-o0-soft').value, el.querySelector('#guard-claude-o0-hard').value]), ['80', '90'], 'an override starts from the tool thresholds');
      await click(page, `${guard} [aria-label="Remove the own thresholds of spare"]`);
      await click(page, `${guard} .guard-discard`);
      await settle(page);
      assert.equal(await page.$('#guard-claude-o0-soft'), null);
      await page.evaluate(() => { window.__guardRefuse = 'Refused here'; });
      await type(page, '#guard-claude-soft', '70');
      await click(page, `${guard} .guard-save`);
      await page.waitForFunction(() => document.querySelector('.guard-tool[data-tool="claude"] .guard-error')?.textContent === 'Refused here');
      assert.equal(await page.$eval('#guard-claude-soft', el => el.value), '70', 'a refused save keeps what was typed');
      await page.close();
      const gone = await mount({ fail: 503 });
      await sync(gone, 'guard', { owner: true });
      assert.equal(await gone.evaluate(() => document.querySelector('.limit-guard-settings').hidden), true, 'a server without the guard hides the block');
      await sync(gone, 'guard', { owner: false });
      await gone.close();
    }
    // Default daily limits: only for someone who may change them; blank is no limit; a bad value is refused; the saved note and a refusal.
    {
      const page = await mount();
      await page.evaluate(async () => { window.__guardState = { ...window.__guardState, can_edit: false }; await window.__stores.limit.sync(); await window.__render(); });
      assert.equal(await page.evaluate(() => document.querySelector('.usage-limit-default').hidden), true, 'someone who may not edit sees nothing');
      await page.evaluate(async () => { window.__guardState = { ...window.__guardState, can_edit: true }; await window.__stores.limit.sync(); await window.__render(); });
      assert.deepEqual(await page.evaluate(() => [document.querySelector('.usage-limit-default').hidden, document.querySelector('#default-cost-cap').value, document.querySelector('#default-cost-cap').placeholder]), [false, '', 'No limit']);
      await type(page, '#default-cost-cap', '-3');
      await click(page, '.usage-limit-default .btn-primary');
      await settle(page);
      assert.equal(await page.$eval('.usage-limit-default .error', el => el.textContent), 'Enter positive limits, or leave a field blank for no limit.');
      assert.equal(await page.evaluate(() => window.__calls.filter(call => call[0] === 'default' && call[2] === 'PUT').length), 0);
      await type(page, '#default-cost-cap', '12.5'); await type(page, '#default-turn-cap', '40');
      await click(page, '.usage-limit-default .btn-primary');
      await page.waitForFunction(() => document.querySelector('.usage-limit-default .settings-saved')?.textContent === 'Saved.');
      assert.deepEqual(await page.evaluate(() => [window.__calls.find(call => call[2] === 'PUT')[3], document.querySelector('#default-cost-cap').value, document.querySelector('#default-turn-cap').value]), [{ daily_cost_cap: 12.5, daily_turn_cap: 40 }, '12.5', '40']);
      await page.evaluate(() => { window.__defaultRefuse = 'The owner only'; });
      await type(page, '#default-turn-cap', '');
      await click(page, '.usage-limit-default .btn-primary');
      await page.waitForFunction(() => document.querySelector('.usage-limit-default .error')?.textContent === 'The owner only');
      assert.equal(await page.$eval('.usage-limit-default .btn-primary', el => el.disabled), false, 'a refusal leaves Save usable');
      await page.close();
      const failing = await mount({ fail: 403 });
      await failing.evaluate(async () => { await window.__stores.limit.sync(); await window.__render(); });
      assert.equal(await failing.evaluate(() => document.querySelector('.usage-limit-default').hidden), true, 'a failed read leaves the card out');
      await failing.close();
    }
    assert.deepEqual(errors, [], 'no page errors');
    console.log('quota page tests passed');
  } finally {
    await browser.close();
    server.close();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

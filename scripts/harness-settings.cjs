// Settings → Harnesses (a list of harnesses beside the chosen one), for both pages of the preact-harnesses experiment (the legacy page, A, and
// the Preact page, B, opened with ?ui=preact-harnesses:b), each against a scratch server: the smoke for model editing, the per-model
// role defaults, the role order and pick mode, the sync with Settings → Roles, the stale-save refusal and the phone width; and, with
// SHOTS_DIR set, a screenshot on a desktop and a phone viewport. `make test-ui` runs the smoke; this file runs on its own too:
//   SHOTS_DIR=/some/dir node scripts/harness-settings.cjs      (HARNESS_VARIANT=a or b runs one page only)
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const { spawn, spawnSync } = require('node:child_process');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const shots = process.env.SHOTS_DIR || '';
const freePort = () => new Promise((resolve, reject) => { const server = net.createServer(); server.once('error', reject); server.listen(0, '127.0.0.1', () => { const port = server.address().port; server.close(() => resolve(port)); }); });

async function main(variant) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'aimebu-harnesses-'));
  const home = path.join(tmp, 'home'); fs.mkdirSync(home, { recursive: true });
  const env = { ...process.env, HOME: home, CODEX_HOME: path.join(home, '.codex'), CLAUDE_CONFIG_DIR: path.join(home, '.claude'), AIMEBU_CONFIG_DIR: path.join(tmp, 'v1'), AIMEBU2_CONFIG_DIR: tmp, TMPDIR: tmp };
  // `make test-ui` passes the binary it already built, so the smoke does not build a second one.
  let binary = process.env.HARNESS_BINARY;
  if (!binary) {
    binary = path.join(tmp, 'aimebu');
    const build = spawnSync('go', ['build', '-o', binary, './cmd/aimebu'], { stdio: 'inherit', env: { ...env, HOME: process.env.HOME } });
    if (build.status !== 0) throw new Error('go build failed');
  }
  const port = await freePort(), origin = `http://127.0.0.1:${port}`;
  const server = spawn(binary, ['server'], { env: { ...env, AIMEBU2_USAGES: '0', AIMEBU2_EMBEDDED_SUPERVISOR: '0', AIMEBU2_PORT: String(port), AIMEBU2_OWNER_LOGIN: 'owner', AIMEBU2_OWNER_PASSWORD: 'test-password-123' }, stdio: ['ignore', 'pipe', 'pipe'] });
  let logs = ''; server.stderr.on('data', data => { logs += data; });
  let supervisor, browser, page;
  const roleSaves = [], pageErrors = [];
  const cleanup = async () => {
    try { await browser?.close(); } catch { /* gone */ }
    for (const child of [supervisor, server]) { try { child?.kill('SIGTERM'); } catch { /* gone */ } }
    await new Promise(resolve => setTimeout(resolve, 500));
    fs.rmSync(tmp, { recursive: true, force: true });
  };
  try {
    for (let i = 0; i < 300; i++) { try { if ((await fetch(origin)).ok) break; } catch { /* not up yet */ } await new Promise(resolve => setTimeout(resolve, 100)); if (i === 299) throw new Error(`server did not start: ${logs}`); }
    supervisor = spawn(binary, ['supervisor'], { env: { ...env, AIMEBU2_URL: origin }, stdio: ['ignore', 'ignore', 'ignore'] });
    browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
    page = await browser.newPage(); page.setDefaultTimeout(60000);
    await page.evaluateOnNewDocument(() => { try { Object.defineProperty(Notification, 'permission', { configurable: true, get: () => 'denied' }); localStorage.setItem('aimebu2.composer.formatting', 'off'); } catch { /* not available */ } });
    await page.setViewport({ width: 1280, height: 900 });
    page.on('response', async response => { const request = response.request(); if (request.method() === 'PUT' && /\/api\/roles\//.test(request.url())) roleSaves.push(`${response.status()} rev=${JSON.parse(request.postData() || '{}').rev} ${request.url().split('/').pop()}`); });
    page.on('pageerror', error => pageErrors.push(error.message));
    page.on('console', message => { if (message.type() === 'error') roleSaves.push(`console: ${message.text()}`); });
    await page.goto(origin);
    // Seed through the API as the owner: three harnesses with models, and role defaults on two roles.
    const seed = await page.evaluate(async () => {
      const json = { 'Content-Type': 'application/json' };
      const login = await (await fetch('/api/login', { method: 'POST', headers: json, body: JSON.stringify({ login: 'owner', password: 'test-password-123' }) })).json();
      const headers = { ...json, 'X-CSRF-Token': login.csrf_token };
      await fetch('/api/me/preferences', { method: 'PUT', headers, body: JSON.stringify({ preferences: { harness_setup_skipped: '1' } }) });
      let machines = [];
      for (let i = 0; i < 100 && !machines.length; i++) { const body = await (await fetch('/api/machines')).json(); machines = Array.isArray(body) ? body : body.machines || []; if (!machines.length) await new Promise(resolve => setTimeout(resolve, 100)); }
      const machine = machines[0].id;
      const make = async (tool, label, command, models) => {
        // The supervisor connects a moment after the machine is listed: a harness is validated by it, so ask again until it answers.
        let response, created;
        for (let i = 0; i < 150; i++) {
          response = await fetch(`/api/machines/${machine}/launchers`, { method: 'POST', headers, body: JSON.stringify({ tool, label, command, args: [], env_refs: {}, enabled: true, containerized: false, path_maps: [], visible_roots: [], supports_toolbox: true }) });
          created = await response.json();
          if (response.status !== 503) break;
          await new Promise(resolve => setTimeout(resolve, 200));
        }
        const id = created.config?.id || created.id;
        if (!id) throw new Error(`launcher ${tool} not created: ${response.status} ${JSON.stringify(created)}`);
        const saved = await fetch(`/api/machines/${machine}/launchers/${id}/models`, { method: 'PUT', headers, body: JSON.stringify({ models: models.map(([slug, name, note], index) => ({ slug, label: name, note: note || '', default: index === 0 })) }) });
        return { id, status: saved.status };
      };
      const claude = await make('claude', 'Claude Code', 'claude', [['opus', 'Opus', 'Deep work'], ['sonnet', 'Sonnet', 'Everyday'], ['haiku', 'Haiku', 'Fast and cheap']]);
      const codex = await make('codex', 'Codex', 'codex', [['gpt-6-sol', 'GPT-6 Sol', ''], ['gpt-6-mini', 'GPT-6 Mini', 'Quick checks']]);
      const pi = await make('pi', 'Pi', 'pi', [['local/llama', 'Llama (local)', '']]);
      const roles = await (await fetch('/api/roles')).json();
      return { machine, claude, codex, pi, roles: roles.map(role => ({ id: role.id, key: role.role_key, rev: role.rev })), token: login.csrf_token };
    });
    for (const harness of [seed.claude, seed.codex, seed.pi]) assert.equal(harness.status, 200, 'the seeded harness has models');
    const putRole = (key, defaults, mode = 'in_turn') => page.evaluate(async (key, defaults, mode) => {
      const headers = { 'Content-Type': 'application/json', 'X-CSRF-Token': (await (await fetch('/api/me')).json()).csrf_token };
      const roles = await (await fetch('/api/roles')).json(); const role = roles.find(item => item.role_key === key);
      const response = await fetch(`/api/roles/${role.id}`, { method: 'PUT', headers, body: JSON.stringify({ id: role.id, rev: role.rev, role_key: role.role_key, label: role.label, emoji: role.emoji || '', body: role.body || '', extends: role.extends || '', cardinality: role.cardinality, cap_is_lead: !!role.cap_is_lead, cap_can_spawn: !!role.cap_can_spawn, cap_singleton_writer: !!role.cap_singleton_writer, default_agents: defaults, pick_mode: mode }) });
      return response.status;
    }, key, defaults, mode);
    assert.equal(await putRole('worker', [{ launcher_id: seed.codex.id, model: 'gpt-6-sol' }, { launcher_id: seed.claude.id, model: 'sonnet' }]), 200);
    assert.equal(await putRole('reviewer', [{ launcher_id: seed.claude.id, model: 'opus' }], 'randomly'), 200);
    // B is opened once through its address; the browser keeps the choice for the reloads below.
    await page.goto(variant === 'b' ? `${origin}/?ui=preact-harnesses:b` : origin);
    await page.waitForSelector('#app-view:not([hidden]):not([aria-busy]), #welcome-view', { timeout: 90000 });
    if (await page.$('#welcome-view')) { await page.$eval('#welcome-view', view => view.click()); await page.waitForSelector('#welcome-continue:not([hidden])'); await page.click('#welcome-continue'); }
    await page.waitForSelector('#app-view:not([hidden]):not([aria-busy])', { timeout: 90000 });
    const domClick = selector => page.$eval(selector, element => element.click());
    const openHarnesses = async () => {
      await domClick('#settings-toggle'); await page.waitForFunction(() => document.querySelector('#settings-dialog').open);
      await domClick('#settings-harnesses-tab');
      await page.waitForFunction(() => document.querySelector('#harnesses-list .machine-harness') && document.querySelector('#harness-roles-host .harness-role-card'), { timeout: 30000 });
    };
    const tabOf = id => `.harness-master [role=tab][data-harness-id="${id}"]`;
    const selectTab = async id => { await domClick(tabOf(id)); await page.waitForFunction(selector => document.querySelector(selector).getAttribute('aria-selected') === 'true', {}, tabOf(id)); };
    const state = () => page.evaluate(() => ({
      defaults: Object.fromEntries([...document.querySelectorAll('#harness-roles-host .harness-role-card')].map(card => [card.dataset.roleKey, [...card.querySelectorAll('.harness-role-entry')].map(entry => entry.dataset.model)])),
      modes: Object.fromEntries([...document.querySelectorAll('#harness-roles-host .harness-role-card')].map(card => [card.dataset.roleKey, card.querySelector('select').value])),
    }));
    const shot = async (name, viewport) => {
      if (!shots) return;
      fs.mkdirSync(shots, { recursive: true });
      await page.setViewport(viewport); await new Promise(resolve => setTimeout(resolve, 300));
      await page.screenshot({ path: path.join(shots, `${name}-${variant}.png`) });
    };
    const DESKTOP = { width: 1280, height: 1500 }, PHONE = { width: 390, height: 2400, deviceScaleFactor: 2 }; // no isMobile: puppeteer reloads the page when it changes

    await openHarnesses();
    assert.equal(await page.$eval('#settings-harnesses', section => section.dataset.implementation || 'legacy'), variant === 'b' ? 'preact' : 'legacy', `the ${variant === 'b' ? 'Preact' : 'legacy'} page is the one tested`);
    assert.equal(await page.$('.harness-layout-switch'), null, 'there is one layout, so no switch');

    // The list beside the chosen harness: the first is selected and shown alone; the arrow keys and a click move through them.
    assert.deepEqual(await page.$$eval('.harness-master [role=tab]', tabs => tabs.map(tab => [tab.getAttribute('aria-selected'), tab.querySelector('.harness-master-name').textContent])), [['true', 'Claude Code'], ['false', 'Codex'], ['false', 'Pi']], 'the list has every harness and selects the first');
    assert.deepEqual(await page.$$eval('#harnesses-list .machine-harness', cards => cards.map(card => card.hidden)), [false, true, true], 'only the chosen harness is shown');
    assert.equal(await page.$('#harnesses-list .machine-harness-models-toggle'), null, 'the models are always shown: no Models toggle');
    assert.equal(await page.$eval('#harnesses-list .machine-harness:not([hidden]) .harness-models', panel => panel.hidden), false, 'the chosen harness shows its models');
    await shot('desktop', DESKTOP);
    await page.$eval('.harness-master [role=tab][aria-selected=false]', tab => tab.click());
    assert.deepEqual(await page.$$eval('#harnesses-list .machine-harness', cards => cards.map(card => card.hidden)), [true, false, true], 'choosing a harness shows its detail');
    await page.focus('.harness-master [role=tab][aria-selected=true]'); await page.keyboard.press('ArrowDown');
    assert.deepEqual(await page.$$eval('#harnesses-list .machine-harness', cards => cards.map(card => card.hidden)), [true, true, false], 'the arrow keys move through the harnesses');
    await selectTab(seed.claude.id);

    // Role defaults: a labelled switch per role on every model.
    const row = slug => `#harnesses-list .harness-detail-row[data-slug="${slug}"]`;
    const box = (slug, role, model) => `${row(slug)} input[aria-label="${role} default: ${model}"]`;
    assert.equal(await page.$eval(box('sonnet', 'Worker', 'Sonnet'), el => el.checked), true, 'a model a role defaults to is switched on');
    assert.equal(await page.$eval(box('opus', 'Reviewer', 'Opus'), el => el.checked), true);
    assert.equal(await page.$eval(box('haiku', 'Worker', 'Haiku'), el => el.checked), false);
    // Switch Reviewer on for Sonnet: it saves on its own and shows in the role order panel at once.
    await page.$eval(box('sonnet', 'Reviewer', 'Sonnet'), el => el.click());
    await page.waitForFunction(() => [...document.querySelectorAll('#harness-roles-host .harness-role-card[data-role-key="reviewer"] .harness-role-entry')].length === 2);
    assert.deepEqual((await state()).defaults.reviewer, ['opus', 'sonnet']);
    assert.match(await page.$eval(`${row('sonnet')} .harness-role-switch.is-on .harness-role-switch-position`, el => el.textContent), /#2 of 2/, 'a switched-on role shows its place in the role order');
    // Reorder in the role panel: Sonnet moves above Opus; the model's own switch stays.
    await page.$eval('#harness-roles-host .harness-role-card[data-role-key="reviewer"] .harness-role-entry:nth-child(2) button[aria-label^="Move"][aria-label*="up"]', button => button.click());
    await page.waitForFunction(() => document.querySelector('#harness-roles-host .harness-role-card[data-role-key="reviewer"] .harness-role-entry')?.dataset.model === 'sonnet');
    // The pick mode.
    await page.select('#harness-roles-host .harness-role-card[data-role-key="reviewer"] select', 'in_turn');
    await page.waitForFunction(async () => (await (await fetch('/api/roles')).json()).find(role => role.role_key === 'reviewer').pick_mode === 'in_turn', { timeout: 20000 });
    // Switch off again: removed from the panel.
    await page.$eval(box('sonnet', 'Reviewer', 'Sonnet'), el => el.click());
    await page.waitForFunction(() => [...document.querySelectorAll('#harness-roles-host .harness-role-card[data-role-key="reviewer"] .harness-role-entry')].length === 1);
    assert.deepEqual((await state()).defaults.reviewer, ['opus']);
    // Quick successive changes of one role all land: an on and an off sent back to back leave the list as it was, with no stale-revision error.
    const reviewerRev = () => page.evaluate(async () => (await (await fetch('/api/roles')).json()).find(role => role.role_key === 'reviewer').rev);
    const before = await reviewerRev();
    await page.evaluate(selector => { const el = document.querySelector(selector); el.click(); el.click(); }, box('haiku', 'Reviewer', 'Haiku'));
    await page.waitForFunction(async rev => (await (await fetch('/api/roles')).json()).find(role => role.role_key === 'reviewer').rev >= rev + 2, { timeout: 20000 }, before);
    await new Promise(resolve => setTimeout(resolve, 500));
    assert.deepEqual((await state()).defaults.reviewer, ['opus'], 'two quick clicks cancel out');
    assert.equal(await page.$eval(box('haiku', 'Reviewer', 'Haiku'), el => el.checked), false);
    assert.deepEqual(await page.$$eval('#settings-harnesses .error', errors => errors.map(error => error.textContent).filter(Boolean)), [], 'no error after quick changes');
    // One button system: every button on the page is a .btn, an .icon-btn or a .row-btn.
    assert.deepEqual(await page.$$eval('#settings-harnesses button', buttons => buttons.filter(button => !button.matches('.btn, .icon-btn, .row-btn')).map(button => button.className)), [], 'every button belongs to the button system');

    // Model editing: the label, the note and the default are edited in place and saved as a whole.
    const claudeRow = () => page.evaluate(async (machine, id) => (await (await fetch(`/api/machines/${machine}/launchers`)).json()).launchers.find(item => item.config.id === id), seed.machine, seed.claude.id);
    const claudeModels = async () => (await claudeRow()).models.map(model => [model.slug, model.label, model.note || '', !!model.default]);
    const editField = (slug, name, value) => page.$eval(`${row(slug)} [name=${name}]`, (input, text) => { input.value = text; input.dispatchEvent(new Event('input', { bubbles: true })); }, value);
    const claudeSave = `#harnesses-list .machine-harness[data-harness-id="${seed.claude.id}"] .harness-models .btn-primary`;
    const saved = async label => { await page.waitForFunction(async (machine, id, text) => (await (await fetch(`/api/machines/${machine}/launchers`)).json()).launchers.find(item => item.config.id === id).models.some(model => model.label === text), { timeout: 20000 }, seed.machine, seed.claude.id, label); await page.waitForFunction(selector => document.querySelector(selector)?.disabled === true, {}, claudeSave); };
    assert.equal(await page.$eval(claudeSave, save => save.disabled), true, 'nothing to save before an edit');
    await editField('haiku', 'label', 'Haiku edited'); await editField('haiku', 'note', 'edited note');
    assert.equal(await page.$eval(claudeSave, save => save.disabled), false, 'an edit asks to be saved');
    await page.$eval(`${row('sonnet')} input[type=radio]`, el => el.click());
    await page.$eval(claudeSave, save => save.click());
    await saved('Haiku edited');
    assert.deepEqual(await claudeModels(), [['opus', 'Opus', 'Deep work', false], ['sonnet', 'Sonnet', 'Everyday', true], ['haiku', 'Haiku edited', 'edited note', false]], 'the edited labels, notes and default are saved');
    await page.$eval(`${row('sonnet')} .harness-model-remove`, button => button.click());
    assert.deepEqual(await page.$$eval(`#harnesses-list .machine-harness[data-harness-id="${seed.claude.id}"] .harness-detail-row`, rows => rows.map(item => item.dataset.slug)), ['opus', 'haiku'], 'removing a model drops its row');
    assert.equal(await page.$eval(`${row('opus')} input[type=radio]`, el => el.checked), true, 'removing the default makes the first model the default');
    // An unsaved removal is not stored: after a reload the model is back. Then put the labels back as they were.
    await page.reload(); await page.waitForSelector('#app-view:not([hidden]):not([aria-busy])', { timeout: 90000 });
    await openHarnesses();
    assert.deepEqual(await claudeModels(), [['opus', 'Opus', 'Deep work', false], ['sonnet', 'Sonnet', 'Everyday', true], ['haiku', 'Haiku edited', 'edited note', false]], 'an unsaved removal is not stored');
    await editField('haiku', 'label', 'Haiku'); await editField('haiku', 'note', 'Fast and cheap');
    await page.$eval(claudeSave, save => save.click());
    await saved('Haiku');

    // Removals in the role panel name their entry: removing the first two of three, both clicked at once, leaves the third.
    await page.$eval(box('haiku', 'Worker', 'Haiku'), el => el.click());
    await page.waitForFunction(() => document.querySelector('#harness-roles-host .harness-role-card[data-role-key="worker"]').querySelectorAll('.harness-role-entry').length === 3);
    await page.evaluate(() => { const buttons = [...document.querySelectorAll('#harness-roles-host .harness-role-card[data-role-key="worker"] .harness-role-entry button[aria-label^="Remove"]')]; buttons[0].click(); buttons[1].click(); });
    await page.waitForFunction(async () => (await (await fetch('/api/roles')).json()).find(role => role.role_key === 'worker').default_agents.map(item => item.model).join() === 'haiku', { timeout: 20000 });
    assert.deepEqual(await page.$$eval('#settings-harnesses .error', errors => errors.map(error => error.textContent).filter(Boolean)), [], 'no error shown after quick removals');
    // Back to the three worker defaults the Roles page check below starts from.
    assert.equal(await putRole('worker', [{ launcher_id: seed.codex.id, model: 'gpt-6-sol' }, { launcher_id: seed.claude.id, model: 'sonnet' }, { launcher_id: seed.claude.id, model: 'haiku' }]), 200);
    await page.reload(); await page.waitForSelector('#app-view:not([hidden]):not([aria-busy])', { timeout: 90000 });
    await openHarnesses();
    assert.deepEqual((await state()).defaults.worker, ['gpt-6-sol', 'sonnet', 'haiku']);
    assert.deepEqual(pageErrors, [], 'no script errors so far');

    // A phone width: the harness list becomes a row of chips above the detail, and nothing overflows sideways.
    await page.setViewport(PHONE);
    const fits = () => page.evaluate(() => {
      const dialog = document.querySelector('#settings-dialog'), split = document.querySelector('.harness-split'), master = document.querySelector('.harness-master');
      return { dialog: dialog.scrollWidth <= dialog.clientWidth + 1, split: split.scrollWidth <= split.clientWidth + 1, row: getComputedStyle(master).display, masterTop: master.getBoundingClientRect().bottom <= document.querySelector('.harness-detail').getBoundingClientRect().top + 1 };
    });
    assert.deepEqual(await fits(), { dialog: true, split: true, row: 'flex', masterTop: true }, 'the harness list sits above the detail and fits at 390 px');
    await shot('phone', PHONE);
    await page.setViewport(DESKTOP);

    // The same list is edited under Settings → Roles: the defaults saved here are there, and a save there shows here.
    await domClick('#settings-roles-tab');
    await page.waitForFunction(() => document.querySelector('#roles-cards .role-card'));
    await page.$eval('#roles-cards .role-card[data-role-key="worker"] .role-card-edit', button => button.click());
    await page.waitForFunction(() => document.querySelectorAll('#role-editor .spawn-default-row').length === 3);
    await page.$eval('#role-editor .spawn-default-row:last-of-type button[aria-label^="Remove default agent"]', button => button.click());
    await page.$eval('#role-editor button[type=submit]', button => button.click());
    await page.waitForFunction(() => document.querySelector('#roles-error').textContent === '' && document.querySelectorAll('#role-editor .spawn-default-row').length === 2);
    await domClick('#settings-harnesses-tab');
    await page.waitForFunction(() => document.querySelector('#harness-roles-host .harness-role-card[data-role-key="worker"]')?.querySelectorAll('.harness-role-entry').length === 2, { timeout: 20000 });
    assert.deepEqual((await state()).defaults.worker, ['gpt-6-sol', 'sonnet'], 'a save in Roles shows on the Harnesses page');

    // The Enabled switch saves on its own and the list beside the detail keeps in step with it.
    const claudeEnabled = () => page.evaluate(async (machine, id) => (await (await fetch(`/api/machines/${machine}/launchers`)).json()).launchers.find(row => row.config.id === id).config.enabled, seed.machine, seed.claude.id);
    const enabledBox = `#harnesses-list .machine-harness[data-harness-id="${seed.claude.id}"] input[name=enabled]`;
    await page.$eval(enabledBox, box => box.click());
    await page.waitForFunction(async (machine, id) => !(await (await fetch(`/api/machines/${machine}/launchers`)).json()).launchers.find(row => row.config.id === id).config.enabled, { timeout: 20000 }, seed.machine, seed.claude.id);
    // The switch is disabled while its save is in flight; click again once it is back.
    await page.waitForFunction(selector => !document.querySelector(selector).disabled, {}, enabledBox);
    assert.equal(await page.$eval(enabledBox, box => box.checked), false, 'the Enabled switch follows its save');
    await page.$eval(enabledBox, box => box.click());
    await page.waitForFunction(async (machine, id) => (await (await fetch(`/api/machines/${machine}/launchers`)).json()).launchers.find(row => row.config.id === id).config.enabled, { timeout: 20000 }, seed.machine, seed.claude.id);
    assert.equal(await claudeEnabled(), true);
    await page.waitForFunction(selector => !document.querySelector(selector).disabled && document.querySelector(selector).checked, {}, enabledBox);
    assert.deepEqual(pageErrors, [], 'no script errors so far');

    // Unsaved model edits survive choosing another harness and coming back.
    const edited = async value => page.$eval('#harnesses-list .harness-detail-row[data-slug="haiku"] [name=label]', (input, text) => { input.value = text; input.dispatchEvent(new Event('input', { bubbles: true })); }, value);
    await edited('Haiku edited');
    await selectTab(seed.codex.id); await selectTab(seed.claude.id);
    assert.equal(await page.$eval('#harnesses-list .harness-detail-row[data-slug="haiku"] [name=label]', input => input.value), 'Haiku edited', 'the edit is still there');
    assert.equal(await page.$eval(claudeSave, save => save.disabled), false, 'the draft still asks to be saved');
    await edited('Haiku');

    // Two tabs. Another tab saves a harness's models; a harness nobody edited here shows them after the page redraws, and a draft started
    // from the old list cannot overwrite the other tab's change: the save is refused, the stored list is shown and the page says why.
    const otherTab = (id, change) => page.evaluate(async (machine, launcher, wanted) => {
      const headers = { 'Content-Type': 'application/json', 'X-CSRF-Token': (await (await fetch('/api/me')).json()).csrf_token };
      const row = (await (await fetch(`/api/machines/${machine}/launchers`)).json()).launchers.find(item => item.config.id === launcher);
      const models = row.models.map(model => wanted[model.slug] ? { ...model, label: wanted[model.slug] } : model);
      const response = await fetch(`/api/machines/${machine}/launchers/${launcher}/models`, { method: 'PUT', headers, body: JSON.stringify({ models, expected_rev: row.models_rev }) });
      return response.status;
    }, seed.machine, id, change);
    const claudeCard = `#harnesses-list .machine-harness[data-harness-id="${seed.claude.id}"]`, codexCard = `#harnesses-list .machine-harness[data-harness-id="${seed.codex.id}"]`;
    await page.waitForFunction(selector => document.querySelector(selector + ' .harness-model-row[data-slug="haiku"] [name=label]'), {}, claudeCard);
    await edited('Haiku mine');
    assert.equal(await otherTab(seed.codex.id, { 'gpt-6-mini': 'Mini changed' }), 200, 'the other tab saves the Codex models');
    // Saving Claude's settings redraws the list from the server: Claude keeps its edit, Codex (never edited here) shows the other tab's.
    await page.$eval(`${claudeCard} .machine-harness-edit`, button => button.click());
    // The legacy page redraws from the server and replaces the card, so a card without this mark is the new one; the Preact page patches the
    // card in place, so there the other tab's label showing on Codex is what says the list was read again.
    await page.$eval(claudeCard, card => { card.dataset.old = '1'; });
    await page.$eval(`${claudeCard} .machine-harness-form button[type=submit]`, button => button.click());
    if (variant === 'a') await page.waitForFunction(selector => document.querySelector(selector) && !document.querySelector(selector).dataset.old, { timeout: 20000 }, claudeCard);
    await page.waitForFunction(selector => document.querySelector(selector + ' .harness-model-row[data-slug="gpt-6-mini"] [name=label]')?.value === 'Mini changed', { timeout: 20000 }, codexCard);
    assert.equal(await page.$eval(`${claudeCard} .harness-model-row[data-slug="haiku"] [name=label]`, input => input.value), 'Haiku mine', 'a draft with edits is kept across the redraw');
    // The other tab then saves Claude's models: the draft is based on a list that is gone.
    assert.equal(await otherTab(seed.claude.id, { haiku: 'Haiku theirs' }), 200, 'the other tab saves the Claude models');
    await page.$eval(`${claudeCard} .harness-models .btn-primary`, button => button.click());
    await page.waitForFunction(selector => document.querySelector(selector + ' .harness-model-row[data-slug="haiku"] [name=label]')?.value === 'Haiku theirs', { timeout: 20000 }, claudeCard);
    assert.match(await page.$eval('#settings-harnesses > .error', error => error.textContent), /Someone changed the models of Claude Code in the meantime/, 'the page says the save was refused');
    assert.equal(await page.$eval(`${claudeCard} .harness-models .btn-primary`, button => button.disabled), true, 'the refused draft is gone');
    assert.deepEqual(pageErrors, [], 'no script errors at the end');
    console.log(`harness settings ok (${variant})`);
  } catch (error) {
    if (shots) { try { fs.mkdirSync(shots, { recursive: true }); await page?.screenshot({ path: path.join(shots, `failure-${variant}.png`) }); } catch { /* best effort */ } }
    console.error(`role saves: ${JSON.stringify(roleSaves)}`);
    console.error(`server log tail:\n${logs.slice(-600)}`);
    throw error;
  } finally { await cleanup(); }
}
(async () => { for (const variant of process.env.HARNESS_VARIANT ? [process.env.HARNESS_VARIANT] : ['a', 'b']) await main(variant); })().catch(error => { console.error(error); process.exit(1); });

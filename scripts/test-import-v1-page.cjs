// The v1 import wizard against a scratch server whose config dir holds a small v1 backup (scripts/v1-fixture.cjs): sign in, walk all seven steps,
// reload on the summary (it must run the dry run again, not sit on "Checking…"), import, and check what came across. `AUTH_BINARY` shares a binary.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { scratch, OWNER, MODULES } = require('./scratch-server.cjs');
const { writeV1Backup, FIXTURE } = require('./v1-fixture.cjs');

const DIALOG = '.import-v1-dialog';
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

async function signIn(page, origin) {
  await page.goto(origin);
  await page.waitForFunction(() => document.querySelector('#welcome-continue') || document.querySelector('#login-view:not([hidden])') || document.querySelector(".import-v1-dialog[open]"));
  if (await page.$('#login-view:not([hidden])')) {
    await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
    await page.$eval('#login-form', form => form.requestSubmit());
    await page.waitForFunction(() => document.querySelector('#welcome-continue') || document.querySelector('.import-v1-dialog[open]') || !document.querySelector('#app-view').hidden);
  }
  if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
}
const heading = page => page.$eval(`${DIALOG} .import-progress-label`, node => node.textContent).catch(() => '');
const clickButton = (page, text) => page.evaluate((selector, text) => {
  const button = [...document.querySelector(selector).querySelectorAll('button')].find(item => item.textContent.trim() === text);
  if (!button) throw new Error(`no "${text}" button: ${[...document.querySelectorAll(`${selector} button`)].map(item => item.textContent.trim()).join(' | ')}`);
  button.click();
}, DIALOG, text);
const stepIs = (page, label) => page.waitForFunction((selector, label) => document.querySelector(`${selector} .import-progress-label`)?.textContent === label, {}, DIALOG, label);
const next = async (page, label) => { await clickButton(page, 'Next'); await stepIs(page, label); };
const footer = page => page.$eval(`${DIALOG} .import-totals`, node => node.hidden ? '' : node.textContent).catch(() => '');
const height = page => page.$eval(DIALOG, node => Math.round(node.getBoundingClientRect().height));

async function main() {
  const server = await scratch({ env: MODULES, prepare: writeV1Backup });
  const { origin } = server;
  const calls = [];
  try {
    const page = await server.open({ context: await server.context() });
    page.on('request', request => { if (request.url().includes('/api/import-v1/') && request.method() !== 'GET') calls.push(`${request.method()} ${new URL(request.url()).pathname}`); });
    await signIn(page, origin);
    await page.waitForSelector(`${DIALOG}[open]`);
    // The wizard presents like the other first-run screens: a modal dialog over an opaque full-page backdrop, with the app behind it hidden and
    // out of reach (no room header, sidebar or top bar around it).
    assert.deepEqual(await page.evaluate(selector => {
      const dialog = document.querySelector(selector), app = document.querySelector('#app-view');
      const backdrop = getComputedStyle(dialog, '::backdrop').backgroundColor, filter = document.querySelector('#app-view .room-filter input');
      filter.focus();
      const top = document.elementFromPoint(10, 10), header = document.querySelector('#room-title');
      return { modal: dialog.matches(':modal'), opaque: /^rgb\(/.test(backdrop), appHidden: getComputedStyle(app).visibility === 'hidden', headerHidden: !header || getComputedStyle(header).visibility === 'hidden',
        sidebarFocusable: document.activeElement === filter, behind: !!top?.closest(selector) };
    }, DIALOG), { modal: true, opaque: true, appHidden: true, headerHidden: true, sidebarFocusable: false, behind: true }, 'the wizard is modal over an opaque backdrop and the app behind it is out of reach');
    // The first step names what the backup holds, and will not go on without a workspace folder for the project.
    await stepIs(page, 'Step 1 of 7 · Projects');
    assert.match(await page.$eval(DIALOG, dialog => dialog.textContent), new RegExp(`The backup holds ${FIXTURE.rooms} rooms, ${FIXTURE.messages} messages, ${FIXTURE.attachments} attachments and ${FIXTURE.memory} memory records`));
    assert.match(await page.$eval(`${DIALOG} .import-only-chance`, node => node.textContent), /only chance to import v1 history\. Skipping closes the import permanently/, 'the warning is its own callout');
    const heights = [await height(page)];
    assert.equal(await footer(page), '1 project selected', 'the first page adds up its choices beside the buttons');
    await clickButton(page, 'Next');
    await page.waitForFunction(selector => /needs its workspace folder/.test(document.querySelector(`${selector} .error`).textContent), {}, DIALOG);
    assert.equal(await heading(page), 'Step 1 of 7 · Projects', 'a missing folder does not move on');
    const folder = path.join(server.tmp, 'workspace'); fs.mkdirSync(folder);
    // the second text field is the folder; the name stays "repo"
    await page.evaluate((selector, folder) => { const input = [...document.querySelectorAll(`${selector} .import-v1-content input[type=text]`)][1]; input.value = folder; input.dispatchEvent(new Event('input', { bubbles: true })); }, DIALOG, folder);
    await next(page, 'Step 2 of 7 · People');
    // Start over asks first, in the app's own dialog (not window.confirm); only Discard clears the draft.
    const hasDraft = () => page.evaluate(() => Object.keys(localStorage).some(key => key.includes('import-draft')));
    assert.equal(await hasDraft(), true, 'the wizard has saved a draft by now');
    const nativeDialogs = [];
    page.on('dialog', native => { nativeDialogs.push(`${native.type()}: ${native.message()}`); native.dismiss(); });
    const askedFor = () => page.evaluate(() => {
      const node = document.querySelector('#app-dialog');
      return !node?.open ? null : { title: node.querySelector('#app-dialog-title').textContent, text: node.querySelector('.app-dialog-message').textContent,
        buttons: [...node.querySelectorAll('.dialog-actions button')].map(button => ({ text: button.textContent, danger: button.classList.contains('btn-danger'), focused: document.activeElement === button })),
        modal: node.matches(':modal') };
    });
    await clickButton(page, 'Start over');
    await page.waitForSelector('#app-dialog[open]');
    assert.deepEqual(await askedFor(), { title: 'Start over?', text: 'Your choices on every page will be discarded.', modal: true,
      buttons: [{ text: 'Keep editing', danger: false, focused: true }, { text: 'Discard', danger: true, focused: false }] }, 'Start over asks, with Keep editing focused and Discard as the danger button');
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => !document.querySelector('#app-dialog')?.open);
    assert.equal(await heading(page), 'Step 2 of 7 · People', 'Escape is Keep editing: the wizard stays where it was');
    assert.equal(await hasDraft(), true, 'Escape keeps the draft');
    await clickButton(page, 'Start over');
    await page.waitForSelector('#app-dialog[open]');
    await page.evaluate(() => [...document.querySelectorAll('#app-dialog .dialog-actions button')].find(button => button.textContent === 'Keep editing').click());
    await page.waitForFunction(() => !document.querySelector('#app-dialog')?.open);
    assert.deepEqual([await heading(page), await hasDraft()], ['Step 2 of 7 · People', true], 'Keep editing keeps the page and the draft');
    await clickButton(page, 'Start over');
    await page.waitForSelector('#app-dialog[open]');
    await page.evaluate(() => [...document.querySelectorAll('#app-dialog .dialog-actions button')].find(button => button.textContent === 'Discard').click());
    await stepIs(page, 'Step 1 of 7 · Projects');
    assert.deepEqual(nativeDialogs, [], 'the confirmation is the app\'s own dialog, not a browser one');
    assert.equal(await page.$eval(`${DIALOG} [data-project-folder="repo"]`, input => input.value), '', 'starting over clears the project choice');
    assert.equal(await page.evaluate(() => Object.keys(localStorage).some(key => key.includes('import-draft'))), false, 'starting over clears the saved draft');
    await page.evaluate((selector, folder) => { const input = document.querySelector(`${selector} [data-project-folder="repo"]`); input.value = folder; input.dispatchEvent(new Event('input', { bubbles: true })); }, DIALOG, folder);
    await next(page, 'Step 2 of 7 · People');
    heights.push(await height(page));
    assert.match(await page.$eval(DIALOG, dialog => dialog.textContent), /messages show under the account you map them to/, 'the copy says what the rendering does');
    assert.doesNotMatch(await page.$eval(DIALOG, dialog => dialog.textContent), /original names remain/);
    // alice is the owner; bob gets a new account (the suggestion).
    await page.evaluate(selector => {
      const select = document.querySelector(selector).querySelector('select');
      select.value = [...select.options].find(option => /^owner/.test(option.textContent)).value;
      select.dispatchEvent(new Event('change', { bubbles: true }));
    }, DIALOG);
    await next(page, 'Step 3 of 7 · Rooms');
    heights.push(await height(page));
    assert.equal(await footer(page), '2 of 2 rooms selected · 5 messages go in', 'the rooms page says how much is selected');
    // Both rooms start open, even though `repo` has had no message for 60 days.
    const rooms = await page.evaluate(selector => [...document.querySelectorAll(`${selector} .import-room-row`)].map(row => ({ name: row.querySelector('strong, b, h3, .import-room-name')?.textContent || row.textContent.split('\n')[0], archived: row.querySelector('input[type=checkbox]')?.checked ?? null })), DIALOG);
    assert.equal(rooms.length, 2, `two rooms are offered (the event room and the chat are not): ${JSON.stringify(rooms)}`);
    assert.equal(rooms.find(room => room.name === 'repo')?.archived, false, 'an older room starts open');
    assert.match(await page.$eval(DIALOG, dialog => dialog.textContent), /A project's home room is never archived\./, 'a row with no archive choice says why');
    await page.$eval(`${DIALOG} [data-archived="repo"]`, box => box.click());
    await next(page, 'Step 4 of 7 · Direct messages');
    assert.match(await page.$eval(`${DIALOG} .settings-intro`, node => node.textContent), /^The chat is not going to be imported\./);
    assert.doesNotMatch(await page.$eval(DIALOG, node => node.textContent), /dm:alice/, 'the raw v1 id is not shown when the title is unambiguous');
    heights.push(await height(page));
    await next(page, 'Step 5 of 7 · Memory');
    assert.equal(await footer(page), '2 of 2 memory records selected');
    // The total follows the checkboxes: leaving the profiles out takes their records off it, and ticking them again puts them back.
    const toggleProfiles = () => page.evaluate(selector => { const box = [...document.querySelectorAll(`${selector} label`)].find(label => /global user profiles/.test(label.textContent)).querySelector('input'); box.click(); }, DIALOG);
    await toggleProfiles();
    assert.equal(await footer(page), '1 of 2 memory records selected · 1 record not imported');
    await toggleProfiles();
    assert.equal(await footer(page), '2 of 2 memory records selected');
    heights.push(await height(page));
    // Each choice says how many records it covers.
    assert.deepEqual(await page.evaluate(selector => [...document.querySelectorAll(`${selector} .import-v1-content label`)].map(label => (label.firstChild?.textContent || label.textContent).trim()), DIALOG),
      ['Project facts: repo (1 record)', 'Import global user profiles (1 record)', 'Import shared agent notes (0 records)']);
    await next(page, 'Step 6 of 7 · Macros');
    // A macro's card says what will happen to it.
    assert.deepEqual(await page.evaluate(selector => [...document.querySelectorAll(`${selector} .import-badge`)].map(badge => badge.textContent), DIALOG), ['New', 'New']);
    assert.equal(await page.$eval(`${DIALOG} .import-macros-left-out`, node => node.textContent),
      "Left out: <example_macro> (v1's built-in example, unchanged; v2 has its own), <old_default> (deleted in v1).", 'the built-ins that carry nothing of yours are named, not offered');
    assert.deepEqual(await page.evaluate(selector => [...document.querySelectorAll(`${selector} legend`)].map(legend => legend.firstChild.textContent.trim()), DIALOG), ['<greet>', '<name>'], 'macro keys are shown as stored');
    await next(page, 'Step 7 of 7 · Summary');
    heights.push(await height(page));
    assert.equal(new Set(heights).size, 1, `the dialog keeps one height from step to step: ${heights}`);
    const summary = async () => { await page.waitForFunction(selector => document.querySelector(`${selector} .import-headline`), {}, DIALOG); return page.evaluate(selector => ({
      headline: document.querySelector(`${selector} .import-headline`).textContent,
      parts: Object.fromEntries([...document.querySelectorAll(`${selector} .import-accounting-group`)].map(group => [group.dataset.key, Object.fromEntries([...group.querySelectorAll('tr[data-part]')].map(row => [row.dataset.part, Number(row.lastChild.textContent.replace(/,/g, ''))]))])),
      keys: [...document.querySelectorAll(`${selector} > .import-v1-content > table td[data-key]`)].map(cell => cell.dataset.key) }), DIALOG); };
    const first = await summary();
    assert.equal(await page.evaluate(selector => [...document.querySelector(`${selector} .dialog-actions`).querySelectorAll('button')].some(button => button.textContent === 'Start over'), DIALOG), true, 'start over remains available through review');
    // Every count of the backup is accounted for, with a reason for each part left out.
    assert.equal(first.headline, `Bringing across ${FIXTURE.imported} of ${FIXTURE.messages} messages, 2 of ${FIXTURE.rooms} rooms, ${FIXTURE.attachments} of ${FIXTURE.attachments} attachments.`);
    assert.deepEqual(first.parts.messages, { imported: FIXTURE.imported, presence_lines: FIXTURE.presence, system_room: 1, dm_not_imported: 1 });
    assert.deepEqual(first.parts.rooms, { imported: 2, system_room: 1, dm_not_imported: 1 });
    assert.deepEqual(first.parts.macros, { imported: FIXTURE.macrosOffered, v1_builtin_example: 1, deleted_in_v1: 1 }, 'the macros add up to the backup\'s rows, with a reason for each one left out');
    assert.ok(first.keys.indexOf('projects') < first.keys.indexOf('messages') && first.keys.indexOf('messages') < first.keys.indexOf('room_prefs'), `the table leads with projects and messages and ends with settings: ${first.keys}`);
    // A reload on the summary comes back to it and runs the dry run again; it does not sit on "Checking…".
    // A draft saved before a macro was left out may hold choices for things the backup no longer offers; the import must not fail on them, and the first page says so once.
    const stale = await page.evaluate(() => {
      const key = Object.keys(localStorage).find(name => /import/i.test(name) && name.includes('draft')) || Object.keys(localStorage).find(name => { try { return JSON.parse(localStorage.getItem(name))?.mapping; } catch { return false; } });
      const draft = JSON.parse(localStorage.getItem(key));
      draft.mapping.skip_macros = [...(draft.mapping.skip_macros || []), 'gone_macro'];
      draft.mapping.macro_conflicts = { ...(draft.mapping.macro_conflicts || {}), gone_macro: 'use_v1' };
      draft.mapping.rooms = { ...draft.mapping.rooms, gone_room: { name: 'gone', project: 'x' } };
      draft.mapping.humans = { ...draft.mapping.humans, gone_person: '-' };
      localStorage.setItem(key, JSON.stringify(draft));
      return key;
    });
    assert.ok(stale, 'the draft is in the browser');
    const before = calls.filter(call => call.endsWith('/dry-run')).length;
    await page.reload();
    await signIn(page, origin);
    await page.waitForSelector(`${DIALOG}[open]`);
    await stepIs(page, 'Step 7 of 7 · Summary');
    const again = await summary();
    assert.deepEqual(again.parts, first.parts);
    assert.equal(await page.$eval(`${DIALOG} .import-stale-note`, node => node.textContent), 'Some earlier choices no longer apply.', 'the dropped choices are mentioned once');
    assert.ok(calls.filter(call => call.endsWith('/dry-run')).length > before, 'the reload asked for a new dry run');
    // Import: one commit, then the finished page.
    await clickButton(page, 'Import now');
    await page.waitForFunction(selector => /v1 import complete/.test(document.querySelector(selector).textContent), { timeout: 60000 }, DIALOG);
    assert.equal(calls.filter(call => call.endsWith('/commit')).length, 1);
    assert.equal(await page.$eval(`${DIALOG} h2`, node => node.textContent), 'Import finished', 'the finished page has a heading');
    assert.equal(await page.$eval(`${DIALOG} .import-finished-summary`, node => node.textContent), `2 rooms · ${FIXTURE.imported} messages · Historical agents: 1`);
    assert.deepEqual(await page.$$eval(`${DIALOG} .import-destination`, nodes => nodes.map(node => node.textContent)), [`${FIXTURE.projectName} · ${folder}`]);
    assert.equal(await page.evaluate(selector => [...document.querySelector(`${selector} .dialog-actions`).querySelectorAll('button')].some(button => button.textContent === 'Start over'), DIALOG), false, 'start over is absent after commit');
    const owner = await server.ownerClient();
    const projects = await owner('/api/projects');
    assert.equal(projects.status, 200);
    const project = projects.body.find(item => item.name === FIXTURE.projectName);
    assert.ok(project, `the imported project exists: ${JSON.stringify(projects.body)}`);
    assert.equal(project.folder, folder, 'with the folder typed in step 1');
    // `repo-leader` became the project's home room; the owner chose to archive `repo` on the Rooms page.
    const placed = (await owner(`/api/projects/${project.id}/rooms`)).body;
    const list = Array.isArray(placed) ? placed : placed.rooms;
    assert.deepEqual(list.map(room => [room.kind, room.title, room.lifecycle]).sort(), [['home', 'repo', 'open'], ['task', 'general', 'closed']], JSON.stringify(list));
    assert.deepEqual(server.errors.filter(error => !/503/.test(error)), [], 'no page errors');
  } finally { await server.stop(); }
  console.log('import v1 page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });

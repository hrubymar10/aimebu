// A room's and a project's settings in the real app on a scratch server: the dialog is the shared settings shell, so every level has an address
// (#room/<id>/settings/<category>[/<sub>], and #project/<id>/settings/... for the project's categories of a home room), Back and Forward walk the levels, a link
// opens the dialog where it points, search finds a setting in a category nobody opened, a phone stacks the levels with a back arrow, and closing and reopening at
// once never loses the dialog. A task room has no project categories; a home room has no Leave & remove. Part of `make test-ui`.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { scratch, OWNER } = require('./scratch-server.cjs');

async function main() {
  const server = await scratch({ env: { AIMEBU_EMBEDDED_SUPERVISOR: '1' } });
  try {
    const context = await server.context();
    const page = await server.open({ width: 1280, height: 900, context });
    await page.goto(server.origin);
    await page.waitForSelector('#login-view:not([hidden])');
    await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
    await page.$eval('#login-form', form => form.requestSubmit());
    await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
    if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
    await page.waitForSelector('#app-view:not([hidden])');
    const call = (url, method, payload) => page.evaluate(async (url, method, payload) => {
      const csrf = (await (await fetch('/api/me')).json()).csrf_token;
      const response = await fetch(url, { method, headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: payload ? JSON.stringify(payload) : undefined });
      const text = await response.text(); return { status: response.status, body: text ? JSON.parse(text) : null };
    }, url, method, payload);
    const folder = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'room-settings-')); execFileSync('git', ['init', '-q'], { cwd: folder });
    const project = (await call('/api/projects', 'POST', { name: 'Settings nav', folder })).body;
    const rooms = (await call(`/api/projects/${project.id}/rooms`, 'GET')).body;
    const home = rooms.find(room => room.kind === 'home');
    const task = (await call(`/api/projects/${project.id}/rooms`, 'POST', { kind: 'task', title: 'A task', parent_id: home.id })).body;
    assert.equal((await call('/api/me/preferences', 'PUT', { preferences: { harness_setup_skipped: '1' } })).status, 200);
    await page.reload(); await page.waitForSelector('#app-view:not([hidden])');

    const settle = (target = page) => target.evaluate(() => new Promise(resolve => setTimeout(resolve, 350)));
    const openRoom = async (id, target = page) => {
      await target.waitForSelector(`[data-room-id="${id}"] .room-item-name`);
      await target.evaluate(id => document.querySelector(`[data-room-id="${id}"] .room-item-name`).click(), id);
      await target.waitForFunction(id => location.hash === `#${id}`, {}, id); await settle(target);
    };
    const state = (target = page) => target.evaluate(() => {
      const dialog = document.querySelector('#room-info-dialog');
      return { hash: location.hash, open: dialog.open, category: dialog.dataset.category, view: dialog.dataset.view, title: document.querySelector('#room-info-title').textContent, history: history.length };
    });
    const shown = (selector, target = page) => target.evaluate(selector => { const node = document.querySelector(selector); return !!node && node.getClientRects().length > 0; }, selector);
    const tabs = (target = page) => target.$$eval('#room-info-dialog .settings-nav-item', nodes => nodes.map(node => node.id.replace(/^room-info-|-tab$/g, '')));
    const click = async (selector, target = page) => { await target.$eval(selector, el => el.click()); await settle(target); };
    const back = async (target = page) => { await target.evaluate(() => history.back()); await settle(target); };
    const project_ = `#project/${project.id}/settings`, room_ = id => `#room/${id}/settings`;

    // A home room: the project's categories above the room's. Opening gives the dialog an address, and the title says it is the project's.
    await openRoom(home.id);
    const before = (await state()).history;
    await click('#room-info');
    assert.deepEqual(await state(), { hash: room_(home.id), open: true, category: 'details', view: 'list', title: 'Project settings', history: before + 1 }, 'opening: its own address, the project General shown on a wide screen');
    await page.waitForFunction(() => document.querySelector('#room-info-danger-tab')); // the project's lifecycle controls arrive with the project
    assert.deepEqual(await tabs(), ['details', 'project-agents', 'team-flow', 'recovery', 'limits', 'sessions', 'danger', 'general', 'members', 'notifications', 'agents', 'memory', 'workspace'],
      'a home room: the project categories, then the room\'s without Leave & remove (a home room cannot be archived, left or deleted as a room)');
    assert.deepEqual([await shown('#project-details'), await shown('#room-rename-form'), await shown('#room-member-list')], [true, false, false], 'only the category\'s blocks are shown');
    await click('#room-info-members-tab');
    assert.equal((await state()).hash, room_(home.id) + '/members');
    assert.deepEqual([await shown('#room-member-list'), await shown('#project-details')], [true, false]);
    await click('#room-info-limits-tab');
    assert.equal((await state()).hash, `${project_}/limits`, 'a project category is under the project');
    assert.deepEqual([await shown('#project-usage-limit'), await shown('#project-usage')], [true, true]);
    await click('#room-info-agents-tab');
    await click('.settings-sub-row:last-child');
    assert.equal((await state()).hash, room_(home.id) + '/agents/schedules');
    assert.equal(await page.$eval('#room-schedules', el => el.classList.contains('settings-flash') || !!el.closest('.settings-flash')), true, 'the subcategory scrolls to its group and marks it');
    // Back walks the levels in reverse; Forward walks them again.
    await back();
    assert.deepEqual([(await state()).hash, (await state()).category], [room_(home.id) + '/agents', 'agents']);
    await back();
    assert.deepEqual([(await state()).hash, (await state()).category], [`${project_}/limits`, 'limits']);
    await page.evaluate(() => history.forward()); await settle();
    assert.equal((await state()).hash, room_(home.id) + '/agents', 'Forward goes down again');
    await back(); await back(); await back();
    assert.deepEqual([(await state()).hash, (await state()).open], [room_(home.id), true]);
    await back();
    assert.deepEqual([(await state()).hash, (await state()).open], [`#${home.id}`, false], 'Back from the first level closes it and the room is in the address again');
    // The close button from a deep level walks the entries back the same way; Escape closes too.
    await click('#room-info'); await click('#room-info-members-tab'); await click('#room-info-notifications-tab');
    await click('#room-info-close');
    await page.waitForFunction(id => location.hash === `#${id}`, {}, home.id);
    assert.equal((await state()).open, false);
    await click('#room-info');
    await page.keyboard.press('Escape'); await settle();
    assert.deepEqual([(await state()).hash, (await state()).open], [`#${home.id}`, false]);

    // The project's danger zone is the owner's, and the usage link goes from the room's usage to the project's.
    await click('#room-info'); await click('#room-info-danger-tab');
    assert.deepEqual([await shown('#project-lifecycle'), await shown('#project-archive')], [true, true]);
    await click('#room-info-workspace-tab');
    assert.equal(await shown('#room-usage-project'), true, 'a home room links its usage to the project\'s');
    assert.equal(await page.$eval('#room-usage-project', el => [el.getAttribute('aria-label'), el.title, el.textContent.trim()]).then(list => list.join('|')), 'Project usage|Project usage|', 'an icon with a tooltip, no text');
    await click('#room-usage-project');
    assert.deepEqual([(await state()).hash, (await state()).category], [`${project_}/limits/usage`, 'limits']);
    await click('#room-info-close');

    // A task room has the room's categories only, and the room's own lifecycle.
    await openRoom(task.id);
    await click('#room-info');
    assert.deepEqual([(await state()).title, (await state()).category, (await state()).hash], ['Room settings', 'general', room_(task.id)]);
    assert.deepEqual(await tabs(), ['general', 'members', 'notifications', 'agents', 'memory', 'workspace', 'leave']);
    assert.equal(await shown('#room-usage-project'), false, 'a task room has no project usage to link');
    await click('#room-info-leave-tab');
    assert.deepEqual([await shown('#room-lifecycle'), await shown('#room-remove')], [true, true]);
    await click('#room-info-close');

    // Search finds a setting in a category nobody opened, and a click opens it with its row marked; a project setting is found only in a home room.
    {
      await click('#room-info');
      const type = async text => { await page.$eval('#room-info-search', (input, text) => { input.value = text; input.dispatchEvent(new Event('input', { bubbles: true })); }, text); await settle(); };
      await type('schedule');
      assert.deepEqual(await page.$$eval('.settings-result-path', rows => rows.map(row => row.textContent)), ['Room › Agents › Schedules', 'Room › Agents']);
      await page.$eval('.settings-result', el => el.click()); await settle();
      assert.equal((await state()).hash, room_(task.id) + '/agents');
      assert.equal(await page.$eval('#room-schedules', el => !!el.closest('.settings-flash') || el.classList.contains('settings-flash')), true, 'the opened setting is marked for a moment');
      assert.equal(await page.$eval('#room-info-search', el => el.value), '', 'the box is empty again');
      await type('recoveries');
      assert.equal(await page.$eval('#room-info-results-empty', el => el.textContent.includes('No settings match')), true, 'a project setting is not found in a task room');
      await type('');
      await click('#room-info-close');
      await openRoom(home.id); await click('#room-info');
      await page.$eval('#room-info-search', input => { input.value = 'recoveries'; input.dispatchEvent(new Event('input', { bubbles: true })); }); await settle();
      assert.ok((await page.$$eval('.settings-result-path', rows => rows.map(row => row.textContent))).includes('Project › Recovery & worktrees › Send worker recoveries to the project lead'));
      await page.$eval('.settings-result', el => el.click()); await settle();
      assert.equal((await state()).hash, `${project_}/recovery`);
      assert.equal(await shown('#worker-recovery-to-lead'), true);
      await click('#room-info-close');
    }

    // A link opens the dialog where it points, once the room is open, and closing it does not leave the app: the entry becomes the room's.
    for (const [hash, category, open] of [[`${room_(task.id)}/notifications`, 'notifications', '#room-notify-level'], [`${project_}/limits`, 'limits', '#project-usage-limit'], [`${project_}`, 'details', '#project-details']]) {
      const linked = await server.open({ width: 1280, height: 900, context });
      await linked.goto(`${server.origin}/${hash}`);
      await linked.waitForSelector('#app-view:not([hidden])');
      await linked.waitForFunction(() => document.querySelector('#room-info-dialog').open);
      await settle(linked);
      const opened = await state(linked);
      assert.deepEqual([opened.hash, opened.category], [hash, category], `a link opens the category it names and keeps its address: ${hash}`);
      assert.equal(await shown(open, linked), true);
      await click('#room-info-close', linked);
      await linked.waitForFunction(() => !location.hash.includes('settings'));
      assert.deepEqual([(await state(linked)).open, (await state(linked)).hash.startsWith('#rm_') || (await state(linked)).hash.length > 1], [false, true], 'closing it stays in the app, in a room');
      await linked.close();
    }
    // A plain room link is still a room, and a link for a project category in a task room opens the room's dialog at what exists.
    {
      const plain = await server.open({ width: 1280, height: 900, context });
      await plain.goto(`${server.origin}/#${task.id}`);
      await plain.waitForSelector('#app-view:not([hidden])'); await settle(plain);
      assert.deepEqual([await plain.evaluate(() => document.querySelector('#room-info-dialog').open), await plain.evaluate(() => document.querySelector('#room-title').textContent)], [false, 'A task'], 'a room address opens the room, not its settings');
      await plain.close();
    }

    // A Forward entry for another room's settings must not leave that stale address over the room now on screen.
    {
      const routed = await server.open({ width: 1280, height: 900, context });
      await routed.goto(`${server.origin}/#${task.id}`);
      await routed.waitForSelector('#app-view:not([hidden])');
      await routed.waitForFunction(id => document.querySelector('#room-title').textContent === 'A task' && location.hash === `#${id}`, {}, task.id);
      await click('#room-info', routed);
      await click('#room-info-members-tab', routed);
      await click('#room-info-close', routed);
      await routed.waitForFunction(id => location.hash === `#${id}`, {}, task.id);
      await openRoom(home.id, routed);
      await routed.evaluate(() => history.forward()); await settle(routed);
      assert.deepEqual([(await state(routed)).hash, (await state(routed)).open], [`#${home.id}`, false], 'Forward to the previous room settings keeps the selected room address');
      await routed.close();
    }

    // Close and reopen at once: the history walk of the close is asynchronous, and its popstate must not close the dialog opened right after it.
    {
      const stays = await page.evaluate(async () => {
        const dialog = document.querySelector('#room-info-dialog'), toggle = document.querySelector('#room-info');
        const wait = ms => new Promise(resolve => setTimeout(resolve, ms));
        const failures = [];
        if (!dialog.open) toggle.click();
        for (let round = 0; round < 20; round += 1) {
          if (!dialog.open) { toggle.click(); await wait(50); }
          document.querySelector('#room-info-notifications-tab').click(); await wait(20);
          document.querySelector('#room-info-close').click();
          if (round % 2 === 0) toggle.click(); else { await Promise.resolve(); toggle.click(); }
          await wait(600);
          if (!dialog.open || !location.hash.includes('/settings')) failures.push({ round, open: dialog.open, hash: location.hash });
        }
        if (dialog.open) document.querySelector('#room-info-close').click();
        return failures;
      });
      assert.deepEqual(stays, [], 'a close followed at once by an open leaves the dialog open, 20 times');
      await page.waitForFunction(() => !document.querySelector('#room-info-dialog').open);
      await settle();
    }

    // A phone: the list first, a category a level down with a back arrow and a centred title; Back and the arrow agree.
    await page.setViewport({ width: 390, height: 844 });
    await click('#room-info');
    assert.deepEqual([(await state()).hash, (await state()).view], [room_(home.id), 'list'], 'on a phone the dialog opens on the list');
    assert.equal(await shown('#room-info-content'), false, 'and the categories fill the screen, not their content');
    await click('#room-info-members-tab');
    assert.deepEqual([(await state()).hash, (await state()).view, (await state()).title], [room_(home.id) + '/members', 'section', 'Members']);
    await click('#room-info-back');
    assert.deepEqual([(await state()).hash, (await state()).view], [room_(home.id), 'list'], 'the arrow goes up a level (a step back in the history)');
    await click('#room-info-notifications-tab');
    await back();
    assert.deepEqual([(await state()).hash, (await state()).view], [room_(home.id), 'list'], 'Back goes up a level too');
    await click('#room-info-nav-close');
    await page.waitForFunction(id => location.hash === `#${id}`, {}, home.id);
    assert.equal((await state()).open, false);

    assert.deepEqual(server.errors, [], `the page reported errors: ${server.errors.join(' | ')}`);
    console.log('room settings nav page: ok');
  } finally {
    await server.stop();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

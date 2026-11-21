// Settings navigation in the real app on a scratch server: every level has its own address (#settings/<section>/<category>[/<sub>]), the browser's Back and
// Forward walk the levels, a link opens Settings where it points (and closing it never leaves the app), a phone stacks the levels with a back arrow, and the room
// the person was in comes back in the address when Settings closes (a fresh server has no room, so that address is empty). Part of `make test-ui`.
const assert = require('node:assert/strict');
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
    await page.evaluate(async () => {
      const csrf = (await (await fetch('/api/me')).json()).csrf_token;
      await fetch('/api/me/preferences', { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ preferences: { harness_setup_skipped: '1' } }) });
    });
    await page.reload(); await page.waitForSelector('#app-view:not([hidden])');
    const settle = (target = page) => target.evaluate(() => new Promise(resolve => setTimeout(resolve, 350)));
    await settle();
    const state = (target = page) => target.evaluate(() => ({ hash: location.hash, open: document.querySelector('#settings-dialog').open, category: document.querySelector('#settings-dialog').dataset.category, view: document.querySelector('#settings-dialog').dataset.view,
      title: document.querySelector('#settings-title')?.textContent ?? null, history: history.length }));
    const click = async (selector, target = page) => { await target.$eval(selector, el => el.click()); await settle(target); };
    const back = async (target = page) => { await target.evaluate(() => history.back()); await settle(target); };
    const room = await page.evaluate(() => location.hash);
    const before = (await state()).history;

    // Opening Settings gives it an address of its own; each category, and each subcategory, another one.
    await click('#settings-toggle');
    assert.deepEqual(await state(), { hash: '#settings', open: true, category: 'appearance', view: 'list', title: 'Settings', history: before + 1 }, 'opening Settings: its own address, Appearance shown on a wide screen');
    await click('#settings-chat-tab');
    assert.equal((await state()).hash, '#settings/personal/chat');
    await click('#settings-storage-tab');
    assert.equal((await state()).hash, '#settings/server/storage');
    await click('.settings-sub-row:last-child');
    assert.equal((await state()).hash, '#settings/server/storage/prompt-cache');
    assert.equal(await page.$eval('#agent-keepalive-enabled', el => !!el.closest('.settings-flash') || !!el.closest('.settings-row')?.classList.contains('settings-flash')), true, 'the subcategory scrolls to its group and marks it');
    // Back walks the levels in reverse; Forward walks them again.
    await back();
    assert.deepEqual([(await state()).hash, (await state()).category], ['#settings/server/storage', 'storage']);
    await back();
    assert.deepEqual([(await state()).hash, (await state()).category], ['#settings/personal/chat', 'chat']);
    await page.evaluate(() => history.forward()); await settle();
    assert.equal((await state()).hash, '#settings/server/storage', 'Forward goes down again');
    await back(); await back();
    assert.deepEqual([(await state()).hash, (await state()).open], ['#settings', true]);
    // Back from the first level closes Settings and the room is in the address again.
    await back();
    assert.deepEqual([(await state()).hash, (await state()).open], [room, false]);

    // Closing with the button from a deep level walks the entries back the same way.
    await click('#settings-toggle');
    await click('#settings-roles-tab'); await click('#settings-machines-tab');
    assert.equal((await state()).hash, '#settings/agents/machines');
    await click('#settings-close');
    await page.waitForFunction(room => location.hash === room, {}, room);
    assert.equal((await state()).open, false);
    // Opening it again, and Escape, behave the same.
    await click('#settings-toggle');
    await page.keyboard.press('Escape'); await settle();
    assert.deepEqual([(await state()).hash, (await state()).open], [room, false]);

    // The old calls with a page name land on the category that holds the page.
    await page.evaluate(() => document.dispatchEvent(new CustomEvent('aimebu:open-settings', { detail: { section: 'usages' } }))); await settle();
    assert.deepEqual([(await state()).hash, (await state()).category], ['#settings/server/usages', 'usages']);
    await click('#settings-close');

    // A link opens Settings where it points, with no room entry behind it, and closing it does not leave the app: the entry becomes the room's.
    {
      const linked = await server.open({ width: 1280, height: 900, context });
      await linked.goto(`${server.origin}/#settings/agents/prompts`);
      await linked.waitForSelector('#app-view:not([hidden])');
      await linked.waitForFunction(() => document.querySelector('#settings-dialog').open);
      await settle(linked);
      const opened = await state(linked);
      assert.deepEqual([opened.hash, opened.category, opened.title], ['#settings/agents/prompts', 'prompts', 'Prompts'], 'a link opens the category it names and keeps its address (the room the app opens does not replace it)');
      const length = opened.history;
      await click('#settings-roles-tab', linked);
      assert.equal((await state(linked)).hash, '#settings/agents/roles');
      await click('#settings-close', linked);
      await linked.waitForFunction(() => !location.hash.startsWith('#settings'));
      assert.deepEqual([(await state(linked)).open, new URL(linked.url()).origin === new URL(server.origin).origin], [false, true], 'closing a link-opened Settings stays in the app');
      assert.ok((await state(linked)).history >= length, 'and drops no history');
      await linked.close();
    }
    // A link that names nothing real opens at the nearest level that exists; one for a room is not Settings.
    {
      const odd = await server.open({ width: 1280, height: 900, context });
      await odd.goto(`${server.origin}/#settings/server/roles`);
      await odd.waitForFunction(() => document.querySelector('#settings-dialog').open);
      await settle(odd);
      assert.deepEqual([(await state(odd)).category, (await state(odd)).view], ['users', 'list'], 'a category under the wrong section opens the section (its first category on a wide screen)');
      await odd.close();
    }

    // Search finds a setting in a category nobody opened, and a click opens it with its row marked.
    {
      await click('#settings-toggle');
      const type = async text => { await page.$eval('#settings-search', (input, text) => { input.value = text; input.dispatchEvent(new Event('input', { bubbles: true })); }, text); await settle(); };
      await type('mentions');
      assert.deepEqual(await page.$$eval('.settings-result-path', rows => rows.map(row => row.textContent)), ['Agents \u203a Prompts \u203a Mentions']);
      await page.$eval('.settings-result', el => el.click());
      await page.waitForSelector('#prompt-mentions'); await settle();
      assert.equal((await state()).hash, '#settings/agents/prompts');
      assert.equal(await page.$eval('#prompt-mentions', el => el.closest('.prompt-slot').classList.contains('settings-flash')), true, 'the opened setting is marked for a moment');
      await page.waitForFunction(() => !document.querySelector('.settings-flash'), { timeout: 5000 });
      assert.equal(await page.$eval('#settings-search', el => el.value), '', 'the box is empty again');
      await type('zzzqq');
      assert.equal(await page.$eval('#settings-results-empty', el => el.textContent.includes('No settings match')), true);
      await type('');
      await click('#settings-close');
    }

    // Close and reopen at once: the history walk of the close is asynchronous, and its popstate must not close the dialog opened right after it.
    {
      const stays = await page.evaluate(async () => {
        const dialog = document.querySelector('#settings-dialog'), toggle = document.querySelector('#settings-toggle');
        const wait = ms => new Promise(resolve => setTimeout(resolve, ms));
        const failures = [];
        if (!dialog.open) toggle.click();
        for (let round = 0; round < 20; round += 1) {
          if (!dialog.open) { toggle.click(); await wait(50); }
          // a deeper level, so the close walks more than one entry back
          document.querySelector('#settings-chat-tab').click(); await wait(20);
          document.querySelector('#settings-close').click();
          if (round % 2 === 0) toggle.click(); else { await Promise.resolve(); toggle.click(); }
          await wait(600);
          if (!dialog.open || !location.hash.startsWith('#settings')) failures.push({ round, open: dialog.open, hash: location.hash });
        }
        if (dialog.open) document.querySelector('#settings-close').click();
        return failures;
      });
      assert.deepEqual(stays, [], 'a close followed at once by an open leaves Settings open, 20 times');
      await page.waitForFunction(() => !document.querySelector('#settings-dialog').open);
      await settle();
      assert.equal((await state()).open, false);
    }

    // A phone: the list first, a category a level down with a back arrow and a centred title; Back and the arrow agree.
    await page.setViewport({ width: 390, height: 844 });
    await click('#settings-toggle');
    assert.deepEqual([(await state()).hash, (await state()).view], ['#settings', 'list']);
    await click('#settings-roles-tab');
    assert.deepEqual([(await state()).hash, (await state()).view, (await state()).title], ['#settings/agents/roles', 'section', 'Roles']);
    await click('#settings-back');
    assert.deepEqual([(await state()).hash, (await state()).view], ['#settings', 'list'], 'the arrow goes up a level (a step back in the history)');
    await click('#settings-chat-tab');
    await back();
    assert.deepEqual([(await state()).hash, (await state()).view], ['#settings', 'list'], 'Back goes up a level too');
    await click('#settings-nav-close');
    await page.waitForFunction(room => location.hash === room, {}, room);
    assert.equal((await state()).open, false);

    assert.deepEqual(server.errors, [], `the page reported errors: ${server.errors.join(' | ')}`);
    console.log('settings nav page: ok');
  } finally {
    await server.stop();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

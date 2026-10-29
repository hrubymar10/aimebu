// First-project onboarding against a scratch server: the owner's first sign-in lands on "Create your first project", then the harness step
// (no harness yet), skipping it, the last step, and the harness step offered alone when a project exists and no harness does. `AUTH_BINARY`
// shares a built binary.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { scratch, OWNER } = require('./scratch-server.cjs');

const APP = '#app-view:not([hidden])';
const MISSING = 'No harness with a model yet. Tick a detected harness, or add one by hand and give it a model.';
const shown = page => page.evaluate(() => ({ root: !document.querySelector('#onboarding-view').hidden, steps: Object.fromEntries([...document.querySelectorAll('#onboarding-view [data-step]')].map(step => [step.dataset.step, !step.hidden])) }));
const prefs = async owner => ((await owner('/api/me/preferences')).body || {}).preferences || (await owner('/api/me/preferences')).body || {};

async function signInAndEnter(page, origin) {
  await page.goto(origin);
  await page.waitForSelector('#login-view:not([hidden])');
  await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
  await page.$eval('#login-form', form => form.requestSubmit());
  await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
  if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
  await page.waitForSelector(APP);
}

async function main() {
  const server = await scratch();
  const { origin } = server;
  try {
    const owner = await server.ownerClient();
    const page = await server.open({ context: await server.context() });
    await signInAndEnter(page, origin);
    await page.waitForSelector('#onboarding-view:not([hidden])');
    // the project step comes first, with the name field focused
    assert.deepEqual(await shown(page), { root: true, steps: { project: true, harnesses: false, how: false } }, 'a first sign-in with no project shows the project step');
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#onboarding-title').textContent, document.activeElement?.name]), ['Create your first project', 'name']);
    // an empty form says what is missing and does not send
    const creates = []; page.on('request', request => { if (request.method() === 'POST' && /\/api\/projects$/.test(request.url())) creates.push(request.postData()); });
    await page.$eval('#onboarding-form', form => { form.noValidate = true; form.requestSubmit(); });
    assert.deepEqual([await page.evaluate(() => document.querySelector('#onboarding-error').textContent), creates.length], ['Enter a project name and a workspace folder.', 0]);
    // the folder field offers the server's home folder when it is focused empty
    await page.focus('#onboarding-form [name=folder]');
    assert.ok((await page.evaluate(() => document.querySelector('#onboarding-form [name=folder]').value)).startsWith('/'), 'an empty folder field is filled with a path on focus');
    // creating the project goes on to the harness step, with Continue off and the reason said
    const folder = path.join(server.tmp, 'first-repo'); fs.mkdirSync(folder);
    await page.type('#onboarding-form [name=name]', 'First Project');
    await page.$eval('#onboarding-form [name=folder]', input => { input.value = ''; });
    await page.type('#onboarding-form [name=folder]', folder);
    await page.$eval('#onboarding-form', form => form.requestSubmit());
    await page.waitForSelector('[data-step="harnesses"]:not([hidden])');
    assert.equal(creates.length, 1, 'one submit creates one project');
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#onboarding-harness-status').textContent, document.querySelector('#onboarding-harnesses-continue').disabled]), [MISSING, true], 'the harness step says what is missing');
    const projects = await owner('/api/projects'); assert.ok(JSON.stringify(projects.body).includes('First Project'), 'the project exists on the server');
    // Skip asks first; cancelling stays; confirming goes to the last step, which says there is no harness
    await page.click('#onboarding-harnesses-skip');
    await page.waitForSelector('#app-dialog[open]');
    assert.match(await page.evaluate(() => document.querySelector('#app-dialog .app-dialog-message').textContent), /^Skip harness setup\? Agents need a harness with a model to run on\./);
    await page.click('#app-dialog .app-dialog-cancel');
    await page.waitForFunction(() => !document.querySelector('#app-dialog').open);
    assert.equal((await shown(page)).steps.harnesses, true, 'cancelling stays on the harness step');
    await page.click('#onboarding-harnesses-skip');
    await page.waitForSelector('#app-dialog[open]'); await page.click('#app-dialog .app-dialog-ok');
    await page.waitForSelector('[data-step="how"]:not([hidden])');
    const how = await page.evaluate(() => ({ note: document.querySelector('#onboarding-harness-note').textContent, noteShown: !document.querySelector('#onboarding-harness-note').hidden, spawn: !document.querySelector('#onboarding-spawn').hidden, setup: !document.querySelector('#onboarding-setup-harness').hidden, principles: document.querySelectorAll('.onboarding-principles li').length, focus: document.activeElement?.id }));
    assert.deepEqual(how, { note: 'No harness with a model is set up yet, so there is nothing for an agent to run on. Set one up, then spawn your first agent.', noteShown: true, spawn: false, setup: true, principles: 7, focus: 'onboarding-setup-harness' }, 'the last step offers to set a harness up, not to spawn');
    assert.equal(String((await prefs(owner)).harness_setup_skipped || ''), '1', 'the skip is kept on the account');
    // "Set up a harness first" goes back to the harness step; Escape closes the onboarding
    await page.click('#onboarding-setup-harness');
    await page.waitForSelector('[data-step="harnesses"]:not([hidden])');
    await page.keyboard.press('Escape');
    assert.equal((await shown(page)).root, false, 'Escape closes it');
    // a reload after the skip is an ordinary visit
    await page.reload(); await page.waitForSelector(APP);
    await new Promise(resolve => setTimeout(resolve, 1500));
    assert.equal((await shown(page)).root, false, 'a reload after the skip does not ask again');
    await page.close();

    // with a project and no harness, and the skip undone, the harness step is offered on its own; Escape closes it, Skip is remembered
    await owner('/api/me/preferences', { method: 'PUT', body: { preferences: { harness_setup_skipped: '' } } });
    const again = await server.open({ context: await server.context() });
    await signInAndEnter(again, origin);
    await again.waitForSelector('#onboarding-view:not([hidden])');
    assert.deepEqual(await shown(again), { root: true, steps: { project: false, harnesses: true, how: false } }, 'the harness step is offered alone');
    await again.focus('#onboarding-harnesses-skip'); await again.keyboard.press('Escape');
    assert.equal((await shown(again)).root, false, 'Escape closes it from inside');
    await again.reload(); await again.waitForSelector(APP); await new Promise(resolve => setTimeout(resolve, 1500));
    assert.equal((await shown(again)).root, false, 'a reload in the same session does not ask again');
    await again.close();
    const third = await server.open({ context: await server.context() });
    await signInAndEnter(third, origin);
    await third.waitForSelector('#onboarding-view:not([hidden])');
    assert.equal((await shown(third)).steps.harnesses, true, 'a new session is offered it again, since it was only closed');
    await third.click('#onboarding-harnesses-skip');
    await third.waitForSelector('#app-dialog[open]'); await third.click('#app-dialog .app-dialog-ok');
    await third.waitForFunction(() => document.querySelector('#onboarding-view').hidden);
    assert.equal(String((await prefs(owner)).harness_setup_skipped || ''), '1', 'skipping it alone is remembered');
    await third.close();
    const fourth = await server.open({ context: await server.context() });
    await signInAndEnter(fourth, origin); await new Promise(resolve => setTimeout(resolve, 1500));
    assert.equal((await shown(fourth)).root, false, 'after Skip a new session is not asked');
    await fourth.close();

    assert.deepEqual(server.errors, [], `the pages reported errors: ${server.errors.join(' | ')}`);
      // a double submit creates one project
    {
      const second = await scratch();
      try {
        const ownerTwo = await second.ownerClient();
        const page = await second.open({ context: await second.context() });
        await signInAndEnter(page, second.origin);
        await page.waitForSelector('#onboarding-view:not([hidden])');
        const creates = []; page.on('request', request => { if (request.method() === 'POST' && /\/api\/projects$/.test(request.url())) creates.push(request.postData()); });
        const folder = path.join(second.tmp, 'twice-repo'); fs.mkdirSync(folder);
        await page.type('#onboarding-form [name=name]', 'Twice Project'); await page.$eval('#onboarding-form [name=folder]', input => { input.value = ''; });
        await page.type('#onboarding-form [name=folder]', folder);
        await page.$eval('#onboarding-form', form => { form.requestSubmit(); form.requestSubmit(); });
        await page.waitForSelector('[data-step="harnesses"]:not([hidden])');
        const listed = JSON.stringify((await ownerTwo('/api/projects')).body).split('Twice Project').length - 1;
        assert.deepEqual([creates.length, listed], [1, 1], 'a double submit creates one project');
        await page.close();
      } finally { await second.stop(); }
    }

    // late answers: a second onboarding on its own root, every call it makes held by the test and answered after the page moved on
    {
      const page = await server.open({ context: await server.context() });
      await signInAndEnter(page, origin);
      await page.evaluate(() => { document.querySelector('#onboarding-view')?.remove(); });
      const settle = () => new Promise(resolve => setTimeout(resolve, 120));
      const make = options => page.evaluate(async options => {
        const { Onboarding } = await import('/js/pages/onboarding/page.js');
        document.querySelectorAll('.late-root').forEach(node => node.remove());
        const root = document.createElement('main'); root.className = 'late-root login onboarding'; root.hidden = true; document.body.append(root);
        const calls = { create: 0, ready: [], skip: [], onReady: 0, adds: 0, opens: 0, confirm: 0 }, gates = { create: [], ready: [], add: [], confirm: [], open: [], skip: [] };
        const gate = list => () => new Promise((resolve, reject) => list.push({ resolve, reject }));
        const step = new Onboarding({
          root, pollMs: options.pollMs || 100000, canSetUp: () => options.canSetUp ?? true,
          create: () => { calls.create++; return gate(gates.create)(); }, spawn: () => {},
          harnessReady: () => { calls.ready.push(1); return gate(gates.ready)(); },
          onSkip: mode => { calls.skip.push(mode); return options.slowSkip ? gate(gates.skip)() : undefined; }, onReady: () => { calls.onReady++; },
          confirm: () => { calls.confirm++; return gate(gates.confirm)(); },
        });
        let ticked = 0;
        step.harnesses.open = () => { calls.opens++; return gate(gates.open)(); };
        step.harnesses.clear = () => {};
        step.harnesses.tickedCount = () => ticked;
        step.harnesses.addTicked = () => { calls.adds++; return gate(gates.add)(); };
        window.__late = { step, calls, gates, root, setTicked: value => { ticked = value; } };
        return true;
      }, options);
      const view = () => page.evaluate(() => {
        const { root, calls } = window.__late, at = selector => root.querySelector(selector);
        return {
          open: !root.hidden, steps: Object.fromEntries([...root.querySelectorAll('[data-step]')].map(node => [node.dataset.step, !node.hidden])),
          error: at('#onboarding-error')?.textContent ?? '', status: at('#onboarding-harness-status')?.textContent ?? '', continueDisabled: at('#onboarding-harnesses-continue')?.disabled ?? null,
          spawnHidden: at('#onboarding-spawn')?.hidden ?? null, focus: document.activeElement?.id || document.activeElement?.name || '',
          calls: { ...calls, ready: calls.ready.length },
        };
      });
      const run = code => page.evaluate(new Function(`const late = window.__late; ${code}`));
      const answer = (list, index, how, value) => page.evaluate((list, index, how, value) => { window.__late.gates[list][index][how](how === 'reject' ? new Error(value) : value); }, list, index, how, value);
      const fill = () => run(`late.step.show(); const form = late.root.querySelector('#onboarding-form'); form.elements.name.value = 'Late'; form.elements.folder.value = '/tmp/late'; form.requestSubmit();`);
      const supersede = { close: () => run('late.step.close()'), destroy: () => run('late.step.destroy()'), 'a new step': () => run(`late.step.showHarnesses('harness-only')`) };

      // creation answered after the page moved on: no readiness question, no step, no error
      for (const [name, change] of Object.entries(supersede)) {
        await make({}); await fill(); await settle();
        assert.equal((await view()).calls.create, 1);
        await change();
        await answer('create', 0, 'resolve');
        await settle();
        const after = await view();
        assert.deepEqual([after.calls.ready, name === 'a new step' ? after.steps.harnesses : true, after.steps.how ?? false], [0, true, false], `a creation answered after ${name} asks nothing and changes no step`);
        await make({}); await fill(); await settle();
        await change();
        await answer('create', 0, 'reject', 'could not create');
        await settle();
        assert.equal((await view()).error, '', `a failed creation answered after ${name} writes nothing`);
      }
      // a double submit while the creation is out sends once
      await make({}); await fill(); await settle();
      await run(`late.root.querySelector('#onboarding-form').requestSubmit(); late.root.querySelector('#onboarding-form').requestSubmit();`); await settle();
      assert.equal((await view()).calls.create, 1, 'a submit while the creation is out sends nothing');
      // a failed creation says why, and the form can be sent again
      await answer('create', 0, 'reject', 'could not create'); await settle();
      assert.deepEqual([(await view()).error, await run(`return late.root.querySelector('#onboarding-form button[type=submit]').disabled`)], ['could not create', false]);

      // the readiness asked after the creation, answered after the page moved on: the Harnesses step does not open
      for (const [name, change] of Object.entries({ close: supersede.close, destroy: supersede.destroy })) {
        await make({}); await fill(); await settle();
        await answer('create', 0, 'resolve'); await settle();
        assert.equal((await view()).calls.ready, 1);
        await change(); await answer('ready', 0, 'resolve', false); await settle();
        assert.deepEqual([(await view()).calls.opens, (await view()).steps.harnesses ?? false], [0, false], `a readiness answered after ${name} does not open the Harnesses step`);
      }
      // the last step's own question, answered after destroy: nothing is drawn or focused
      await make({ canSetUp: false }); await fill(); await settle();
      await answer('create', 0, 'resolve'); await settle(); await answer('ready', 0, 'resolve', false); await settle();
      assert.equal((await view()).calls.ready, 2, 'with nothing ready the last step asks again');
      await run('late.step.destroy()'); await answer('ready', 1, 'resolve', false); await settle();
      assert.deepEqual(await run('return [late.step.store.how.value.noteHidden, late.step.store.focus.value.target !== \'done\']'), [true, true], 'the last step answered after destroy draws and focuses nothing');

      // the readiness poll answered after the step was left: no ready report, no status
      for (const [name, change] of Object.entries({ close: supersede.close, destroy: supersede.destroy })) {
        await make({ pollMs: 40 });
        await run(`late.step.showHarnesses('first')`); await answer('open', 0, 'resolve'); await settle();
        await answer('ready', 0, 'resolve', false); await settle();
        const before = await view();
        await page.waitForFunction(() => window.__late.calls.ready.length >= 2); // a poll is out
        await change(); await answer('ready', 1, 'resolve', true); await settle();
        const after = await view();
        assert.deepEqual([after.calls.onReady, name === 'close' ? after.status : before.status], [0, before.status], `a poll answered after ${name} reports nothing and rewrites no status`);
      }
      // leaving the step starts no poll afterwards, and a tick after leaving does not rewrite the status
      await make({ pollMs: 40 });
      await run(`late.step.showHarnesses('first')`); await answer('open', 0, 'resolve'); await settle(); await answer('ready', 0, 'resolve', true); await settle();
      await run(`late.step.close()`); await settle();
      assert.equal(await run('return late.step.watch'), null, 'a closed step is not polled');

      // leaving the step while it is still loading starts no poll afterwards, and the first readiness is not asked
      for (const [name, change] of Object.entries({ close: supersede.close, destroy: supersede.destroy })) {
        await make({ pollMs: 40 });
        await run(`late.step.showHarnesses('first')`); await change();
        await answer('open', 0, 'resolve'); await settle(); await settle();
        const early = await view();
        assert.deepEqual([early.calls.ready, await run('return late.step.watch')], [0, null], `a step left while loading (${name}) asks nothing and starts no poll`);
      }
      // a tick after the step was left does not rewrite the status the person last read
      await make({});
      await run(`late.step.showHarnesses('first')`); await answer('open', 0, 'resolve'); await settle(); await answer('ready', 0, 'resolve', false); await settle();
      await run(`late.root.querySelector('#onboarding-harnesses-skip').click()`); await settle(); await answer('confirm', 0, 'resolve', true); await settle(); await answer('ready', 1, 'resolve', false); await settle();
      const left = await view();
      await run('late.setTicked(2); late.step.harnesses.onTick(2)'); await settle();
      assert.deepEqual([left.steps.how, (await view()).status], [true, left.status], 'ticking after the step was left does not rewrite its status');

      // a creation answered after a newer one began does not free the form for a third
      await make({}); await fill(); await settle();
      await run('late.step.show()');
      await run(`const form = late.root.querySelector('#onboarding-form'); form.elements.name.value = 'Again'; form.elements.folder.value = '/tmp/again'; form.requestSubmit();`); await settle();
      assert.equal((await view()).calls.create, 2, 'showing the form again frees it for a new creation');
      await answer('create', 0, 'resolve'); await settle();
      await run(`late.root.querySelector('#onboarding-form').requestSubmit()`); await settle();
      assert.equal((await view()).calls.create, 2, 'the older creation answering does not free the form while the newer one is out');
      // the poll stops when the step is closed, not only when its answers are ignored
      await make({ pollMs: 40 });
      await run(`late.step.showHarnesses('first')`); await answer('open', 0, 'resolve'); await settle(); await answer('ready', 0, 'resolve', true); await settle();
      await run('late.step.close()');
      const polled = (await view()).calls.ready;
      await settle(); await settle();
      assert.equal((await view()).calls.ready, polled, 'a closed step asks nothing more');
      // the first readiness answered after the step was closed does not start a poll
      for (const [name, change] of Object.entries({ close: supersede.close, destroy: supersede.destroy })) {
        await make({ pollMs: 40 });
        await run(`late.step.showHarnesses('first')`); await answer('open', 0, 'resolve'); await settle();
        await change(); await answer('ready', 0, 'resolve', true); await settle(); await settle();
        assert.deepEqual([await run('return late.step.watch'), (await view()).calls.ready], [null, 1], `a first readiness answered after ${name} starts no poll`);
      }
      // the readiness asked after an addition, answered after the page moved on: the step does not go on
      for (const [name, change] of Object.entries({ close: supersede.close, destroy: supersede.destroy })) {
        await make({});
        await run(`late.step.showHarnesses('first')`); await answer('open', 0, 'resolve'); await settle(); await answer('ready', 0, 'resolve', false); await settle();
        await run('late.setTicked(1); late.step.harnesses.onTick(1)'); await settle();
        await run(`late.root.querySelector('#onboarding-harnesses-continue').click()`); await settle();
        await answer('add', 0, 'resolve', true); await settle();
        assert.equal((await view()).calls.ready, 2, 'after an addition the readiness is asked');
        await change(); await answer('ready', 1, 'resolve', true); await settle();
        assert.deepEqual([(await view()).steps.how ?? false, await run('return late.step.store.step.value')], [false, 'harnesses'], `a readiness answered after ${name} goes no further`);
      }
      // a skip recorded slowly, the page destroyed meanwhile: the last step is not asked for
      await make({ slowSkip: true });
      await run(`late.step.showHarnesses('first')`); await answer('open', 0, 'resolve'); await settle(); await answer('ready', 0, 'resolve', false); await settle();
      await run(`late.root.querySelector('#onboarding-harnesses-skip').click()`); await settle(); await answer('confirm', 0, 'resolve', true); await settle(); await answer('ready', 1, 'resolve', false); await settle();
      assert.equal((await view()).calls.skip.length, 1);
      const beforeSkipEnds = (await view()).calls.ready;
      await run('late.step.destroy()'); await answer('skip', 0, 'resolve'); await settle();
      assert.deepEqual([(await view()).calls.ready, await run('return late.step.store.step.value')], [beforeSkipEnds, 'harnesses'], 'a skip recorded after destroy does not go on to the last step');

      // Continue is one run at a time, and its answer after the page moved on goes no further
      for (const [name, change] of Object.entries({ close: supersede.close, destroy: supersede.destroy })) {
        await make({});
        await run(`late.step.showHarnesses('first')`); await answer('open', 0, 'resolve'); await settle(); await answer('ready', 0, 'resolve', false); await settle();
        await run('late.setTicked(1); late.step.harnesses.onTick(1)'); await settle();
        await run(`late.root.querySelector('#onboarding-harnesses-continue').click(); late.root.querySelector('#onboarding-harnesses-continue').disabled = false; late.root.querySelector('#onboarding-harnesses-continue').click();`); await settle();
        assert.equal((await view()).calls.adds, 1, 'a second Continue while the harnesses are being added adds nothing');
        const asked = (await view()).calls.ready;
        await change(); await answer('add', 0, 'resolve', true); await settle();
        const after = await view();
        assert.deepEqual([after.calls.ready, after.calls.skip.length, after.steps.how ?? false], [asked, 0, false], `an addition answered after ${name} goes no further`);
      }
      // an addition left behind by a closed step: the reopened step is independent of it, and its answer neither unlocks nor advances the newer one
      await make({});
      const continueOf = `late.root.querySelector('#onboarding-harnesses-continue')`;
      await run(`late.step.showHarnesses('first')`); await answer('open', 0, 'resolve'); await settle(); await answer('ready', 0, 'resolve', false); await settle();
      await run(`late.setTicked(1); late.step.harnesses.onTick(1)`); await settle();
      await run(`${continueOf}.click()`); await settle();
      assert.equal((await view()).calls.adds, 1, 'the first addition is out');
      await run('late.step.close()');
      await run(`late.step.showHarnesses('first')`); await answer('open', 1, 'resolve'); await settle(); await answer('ready', 1, 'resolve', false); await settle();
      assert.equal((await view()).continueDisabled, false, 'a reopened step is not locked by the older addition');
      await run(`${continueOf}.click()`); await settle();
      assert.equal((await view()).calls.adds, 2, 'the reopened step adds on its own');
      const askedBefore = (await view()).calls.ready;
      await answer('add', 0, 'resolve', true); await settle();
      await run('late.step.harnesses.onTick(1)'); await settle();
      await run(`${continueOf}.click()`); await settle();
      const older = await view();
      assert.deepEqual([older.calls.adds, older.calls.ready, older.steps.how ?? false, older.continueDisabled], [2, askedBefore, false, true], 'the older addition answering unlocks nothing and advances nothing; the newer stays locked');
      await answer('add', 1, 'resolve', true); await settle(); await answer('ready', 2, 'resolve', true); await settle();
      assert.equal((await view()).steps.how, true, 'the newer addition goes on when it answers');

      // a skip confirmed after the page moved on records nothing
      for (const [name, change] of Object.entries({ close: supersede.close, destroy: supersede.destroy })) {
        await make({});
        await run(`late.step.showHarnesses('first')`); await answer('open', 0, 'resolve'); await settle(); await answer('ready', 0, 'resolve', false); await settle();
        await run(`late.root.querySelector('#onboarding-harnesses-skip').click()`); await settle();
        const asked = (await view()).calls.ready;
        await change(); await answer('confirm', 0, 'resolve', true); await settle();
        assert.deepEqual([(await view()).calls.skip.length, (await view()).calls.ready, (await view()).steps.how ?? false], [0, asked, false], `a skip confirmed after ${name} records nothing and asks nothing`);
      }
      // and one whose readiness check is out when the page is destroyed
      await make({});
      await run(`late.step.showHarnesses('first')`); await answer('open', 0, 'resolve'); await settle(); await answer('ready', 0, 'resolve', false); await settle();
      await run(`late.root.querySelector('#onboarding-harnesses-skip').click()`); await settle(); await answer('confirm', 0, 'resolve', true); await settle();
      assert.equal((await view()).calls.ready, 2, 'leaving looks at the readiness again');
      await run('late.step.destroy()'); await answer('ready', 1, 'resolve', false); await settle();
      assert.equal((await view()).calls.skip.length, 0, 'a skip is not recorded for a page that is gone');
      await page.close();
    }

  console.log('onboarding page: ok');
  } finally {
    await server.stop();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

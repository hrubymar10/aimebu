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

  console.log('onboarding page: ok');
  } finally {
    await server.stop();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

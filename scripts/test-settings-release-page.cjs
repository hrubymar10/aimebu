// Settings in the real app on a scratch server: the sections are drawn when they are selected and taken out of the page when the dialog closes, so
// a closed Settings leaves the page as it was before the first open, however often and through however many sections it was opened. A section
// holding unsaved edits (typed text in General's message window or in Usages, or a form that reports it) keeps its page. Part of `make test-ui`.
const assert = require('node:assert/strict');
const { scratch, OWNER } = require('./scratch-server.cjs');

const SECTIONS = ['general', 'usages', 'notifications', 'roles', 'users', 'prompts', 'macros', 'telemetry', 'machines', 'harnesses'];

async function main() {
  const server = await scratch({ env: { AIMEBU2_EMBEDDED_SUPERVISOR: '1' } });
  try {
    const page = await server.open({ width: 1280, height: 900, context: await server.context() });
    await page.goto(server.origin);
    await page.waitForSelector('#login-view:not([hidden])');
    await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
    await page.$eval('#login-form', form => form.requestSubmit());
    await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
    if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
    await page.waitForSelector('#app-view:not([hidden])');
    // A server with no harness offers the first-run harness card over the app: skipped, as the person's own stored choice.
    await page.evaluate(async () => {
      const csrf = (await (await fetch('/api/me')).json()).csrf_token;
      await fetch('/api/me/preferences', { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ preferences: { harness_setup_skipped: '1' } }) });
    });
    await page.reload(); await page.waitForSelector('#app-view:not([hidden])');
    const settle = () => page.evaluate(() => new Promise(resolve => setTimeout(resolve, 300)));
    const nodes = () => page.evaluate(() => document.getElementsByTagName('*').length);
    const inSections = () => page.evaluate(() => [...document.querySelectorAll('#settings-dialog .settings-body > section')].reduce((sum, section) => sum + section.getElementsByTagName('*').length, 0));
    const open = async section => { await page.evaluate(name => document.dispatchEvent(new CustomEvent('aimebu:open-settings', { detail: { section: name } })), section); await settle(); };
    const close = async () => { await page.evaluate(() => document.querySelector('#settings-dialog').close()); await settle(); };

    assert.equal(await inSections(), 0, 'before Settings is first opened no section is in the page');
    // The first open leaves a few nodes of its own (the shell's header and nav, the kept build version): the count to return to is the one after it.
    await open('general'); await close();
    const base = await nodes();
    for (let round = 0; round < 3; round++) {
      for (const section of SECTIONS) {
        await open(section);
        assert.ok(await inSections() > 0, `${section} is drawn when selected`);
      }
      await close();
      assert.equal(await inSections(), 0, `round ${round}: closing Settings takes every section out of the page`);
      assert.equal(await nodes(), base, `round ${round}: the page is back to ${base} nodes`);
    }
    // Closing Settings while a Harnesses form is dirty asks before discarding it.
    await open('harnesses');
    await page.waitForSelector('#harness-add:not([disabled])');
    await page.click('#harness-add');
    await page.waitForSelector('#harness-add-form [name=command]');
    await page.type('#harness-add-form [name=command]', 'example-tool');
    await settle();
    await page.click('#settings-close');
    await page.waitForSelector('#app-dialog[open]');
    assert.deepEqual(await page.$eval('#app-dialog', dialog => [dialog.querySelector('h2').textContent, dialog.querySelector('.app-dialog-ok').textContent, dialog.querySelector('.app-dialog-cancel').textContent]), ['Discard unsaved changes?', 'Discard', 'Keep editing']);
    await page.click('#app-dialog .app-dialog-cancel');
    assert.equal(await page.$eval('#settings-dialog', dialog => dialog.open), true, 'Keep editing leaves Settings open');
    assert.equal(await page.$eval('#harness-add-form [name=command]', input => input.value), 'example-tool');
    await page.$eval('#settings-dialog', dialog => dialog.dispatchEvent(new MouseEvent('click', { bubbles: true })));
    await page.waitForSelector('#app-dialog[open]');
    await page.click('#app-dialog .app-dialog-cancel');
    assert.equal(await page.$eval('#settings-dialog', dialog => dialog.open), true, 'backdrop close also guards the form');
    await page.keyboard.press('Escape');
    await page.waitForSelector('#app-dialog[open]');
    await page.click('#app-dialog .app-dialog-ok');
    await page.waitForFunction(() => !document.querySelector('#settings-dialog').open);
    await open('harnesses');
    assert.equal(await page.$('#harness-add-form'), null, 'Discard clears the unsaved form');
    await close();
    // The selected section is drawn again by the next open, with its content.
    await open('general');
    assert.ok(await page.$('#settings-general *'), 'General is drawn again on the next open');
    await close();
    // A section holding typed, unsaved text keeps its page through a close, and the text with it; one that holds none is released as before.
    const typeInto = async (selector, text) => { await page.$eval(selector, (input, text) => { input.value = text; input.dispatchEvent(new Event('input', { bubbles: true })); }, text); await settle(); };
    const sectionNodes = name => page.evaluate(id => document.querySelector(id)?.getElementsByTagName('*').length || 0, `#settings-${name}`);
    await open('general');
    const savedWindow = await page.$eval('#message-expiry-amount', input => input.value);
    await typeInto('#message-expiry-amount', '4321');
    await close();
    assert.ok(await sectionNodes('general') > 0, 'General keeps its page while a message-expiry window is typed and not saved');
    await open('general');
    assert.equal(await page.$eval('#message-expiry-amount', input => input.value), '4321', 'the typed window is still there on the next open');
    await typeInto('#message-expiry-amount', savedWindow); // back to what is saved: nothing is unsaved any more
    await close();
    assert.equal(await sectionNodes('general'), 0, 'General is released again once the typed window equals the saved one');
    await open('usages');
    await page.waitForSelector('#default-cost-cap');
    await typeInto('#default-cost-cap', '12.5');
    await close();
    assert.ok(await sectionNodes('usages') > 0, 'Usages keeps its page while a default limit is typed and not saved');
    assert.equal(await page.$eval('#default-cost-cap', input => input.value), '12.5', 'the typed default limit is still in the closed dialog (opening Usages reads the server again, as it always did)');
    await open('usages'); await close(); // the open read the server: nothing is unsaved now, so this close releases the section
    assert.equal(await sectionNodes('usages'), 0, 'Usages is released once its fields start over from the server');
    assert.equal(await sectionNodes('roles'), 0, 'a section with nothing typed is released next to the ones kept');
    assert.deepEqual(server.errors, [], `the page reported errors: ${server.errors.join(' | ')}`);
    console.log('settings release page: ok');
  } finally {
    await server.stop();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

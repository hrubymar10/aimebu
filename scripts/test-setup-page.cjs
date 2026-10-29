// First-run owner setup against an owner-less scratch server: the welcome, the setup token (typed, or from a /setup?token= link; right,
// wrong and throttled), the Create owner form and its checks, the theme step, and what the server holds afterwards. Each flow that creates the
// owner gets a server of its own, since there is only one first run. `AUTH_BINARY` shares a built binary.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { scratch } = require('./scratch-server.cjs');

const APP = '#app-view:not([hidden])';
const PASSWORD = 'owner-password-12345';
const WRONG_TOKEN = 'that is not the setup token; copy it from the server\'s output or from the token file it names';

const status = page => page.evaluate(async () => (await fetch('/api/setup/status')).json());
const tokenOf = async page => fs.readFileSync((await status(page)).token_path, 'utf8').trim();
const requests = (page, pattern) => { const seen = []; page.on('request', request => { if (pattern.test(request.url())) seen.push({ method: request.method(), body: request.postData() }); }); return seen; };
const views = page => page.evaluate(() => Object.fromEntries(['#welcome-view', '#setup-token-view', '#setup-view', '#theme-view', '#app-view', '#login-view'].map(id => [id.slice(1), !!document.querySelector(id) && !document.querySelector(id).hidden])));
const toWelcome = async server => { const page = await server.open({ context: await server.context() }); await page.goto(`${server.origin}/setup`); await page.waitForSelector('#welcome-continue:not([inert])'); return page; };
const pass = async page => { await page.click('#welcome-continue'); await page.waitForSelector('#setup-token-view:not([hidden])'); };

async function withServer(run) {
  const server = await scratch({ owner: null });
  try { await run(server); assert.deepEqual(server.errors, [], `the pages reported errors: ${server.errors.join(' | ')}`); } finally { await server.stop(); }
}

async function main() {
  // the welcome, then the token step and its hint
  await withServer(async server => {
    const page = await toWelcome(server);
    assert.deepEqual(await views(page), { 'welcome-view': true, 'setup-token-view': false, 'setup-view': false, 'theme-view': false, 'app-view': false, 'login-view': false }, 'the setup starts with the welcome alone');
    assert.equal(await page.evaluate(() => document.querySelector('#welcome-view').dataset.stage), 'done', 'reduced motion shows the finished welcome at once');
    await pass(page);
    const hint = await page.evaluate(() => ({ text: document.querySelector('#setup-token-hint').textContent, code: document.querySelector('#setup-token-hint code')?.textContent, heading: document.querySelector('#setup-token-view h1').textContent, focus: document.activeElement?.name }));
    assert.deepEqual([hint.heading, hint.code, hint.text, hint.focus], ['Enter the setup token to continue', (await status(page)).token_path, `Find it in the output of the server, or in the file ${(await status(page)).token_path}.`, 'token'], 'the token step says where the token is');
    // an empty token is not sent
    const checks = requests(page, /\/api\/setup\/token$/);
    await page.$eval('#setup-token-form', form => form.requestSubmit());
    assert.deepEqual([checks.length, await page.evaluate(() => document.querySelector('#setup-token-error').textContent)], [0, 'Enter the setup token.']);
    // a wrong token says so, once, and the form takes another try
    await page.type('#setup-token-form [name=token]', 'wrong-token');
    await page.$eval('#setup-token-form', form => form.requestSubmit());
    await page.waitForFunction(() => document.querySelector('#setup-token-error').textContent !== 'Enter the setup token.' && !document.querySelector('#setup-token-form button[type=submit]').disabled);
    const wrong = await page.evaluate(() => [document.querySelector('#setup-token-error').textContent, document.querySelector('#setup-token-form button[type=submit]').disabled, document.activeElement?.name]);
    assert.deepEqual([checks.length, wrong[1]], [1, false], 'a wrong token is checked once, and the form is open again');
    assert.equal(wrong[0], WRONG_TOKEN, 'a wrong token shows the server\'s sentence');
    // the right one goes to the owner form
    await page.$eval('#setup-token-form [name=token]', input => { input.value = ''; });
    await page.type('#setup-token-form [name=token]', await tokenOf(page));
    await page.$eval('#setup-token-form', form => form.requestSubmit());
    await page.waitForSelector('#setup-view:not([hidden])');
    assert.deepEqual(await views(page), { 'welcome-view': false, 'setup-token-view': false, 'setup-view': true, 'theme-view': false, 'app-view': false, 'login-view': false }, 'the right token moves to the owner form');
    assert.equal(await page.evaluate(() => document.activeElement?.name), 'login');
    await page.close();
  });

  // a link with the token skips the step; a wrong one lands on the step with the value in and the reason
  await withServer(async server => {
    const probe = await server.open({ context: await server.context() }); await probe.goto(`${server.origin}/setup`); const token = await tokenOf(probe); await probe.close();
    const wrongLink = await server.open({ context: await server.context() });
    await wrongLink.goto(`${server.origin}/setup?token=nope`); await wrongLink.waitForSelector('#welcome-continue:not([inert])'); await wrongLink.click('#welcome-continue');
    await wrongLink.waitForSelector('#setup-token-view:not([hidden])');
    const wrong = await wrongLink.evaluate(() => [document.querySelector('#setup-token-form [name=token]').value, document.querySelector('#setup-token-error').textContent]);
    assert.equal(wrong[0], 'nope'); assert.equal(wrong[1], WRONG_TOKEN, 'a wrong linked token shows why');
    await wrongLink.close();
    const page = await server.open({ context: await server.context() });
    const checks = requests(page, /\/api\/setup\/token$/);
    await page.goto(`${server.origin}/setup?token=${encodeURIComponent(token)}`); await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue');
    await page.waitForSelector('#setup-view:not([hidden])');
    assert.deepEqual([checks.length, (await views(page))['setup-token-view']], [1, false], 'a right linked token is checked once and the token step is skipped');
    await page.close();
  });

  // the throttle: after enough wrong tokens the server says how long to wait
  await withServer(async server => {
    const page = await toWelcome(server); await pass(page);
    let message = '';
    for (let attempt = 0; attempt < 30 && !/^Too many attempts/.test(message); attempt++) {
      await page.$eval('#setup-token-form [name=token]', input => { input.value = ''; });
      await page.type('#setup-token-form [name=token]', `wrong-${attempt}`);
      await page.$eval('#setup-token-form', form => form.requestSubmit());
      await page.waitForFunction(() => !document.querySelector('#setup-token-form button[type=submit]').disabled);
      message = await page.evaluate(() => document.querySelector('#setup-token-error').textContent);
    }
    assert.match(message, /^Too many attempts\. Try again in \d+ seconds\.$/, 'the throttle says how long to wait');
    await page.close();
  });

  // the owner form: each check, then one create, the theme step and what the server holds afterwards
  await withServer(async server => {
    const page = await toWelcome(server); await pass(page);
    await page.type('#setup-token-form [name=token]', await tokenOf(page));
    await page.$eval('#setup-token-form', form => form.requestSubmit());
    await page.waitForSelector('#setup-view:not([hidden])');
    const creates = requests(page, /\/api\/setup$/), welcomeSeen = requests(page, /\/api\/welcome\/seen$/);
    const error = () => page.evaluate(() => document.querySelector('#setup-error').textContent);
    const fill = async (login, password, confirm) => { for (const [name, value] of [['login', login], ['password', password], ['confirm', confirm]]) { await page.$eval(`#setup-form [name=${name}]`, input => { input.value = ''; }); if (value) await page.type(`#setup-form [name=${name}]`, value); } };
    const submit = () => page.evaluate(() => { const form = document.querySelector('#setup-form'); form.noValidate = true; form.requestSubmit(); });
    await fill('Ab', PASSWORD, PASSWORD); await submit();
    assert.equal(await error(), 'Choose a login of 3 to 32 lowercase letters, digits, - or _, starting with a letter.');
    await fill('1abc', PASSWORD, PASSWORD); await submit();
    assert.equal(await error(), 'Choose a login of 3 to 32 lowercase letters, digits, - or _, starting with a letter.');
    await fill('boss', 'short', 'short'); await submit();
    assert.equal(await error(), 'Use a password of at least 12 characters.');
    await fill('boss', PASSWORD, `${PASSWORD}x`); await submit();
    assert.equal(await error(), 'The two passwords do not match.');
    assert.equal(creates.length, 0, 'none of those was sent');
    // one create, then the theme step
    await fill('Boss', PASSWORD, PASSWORD);
    await page.evaluate(() => document.querySelector('#setup-form').requestSubmit());
    await page.waitForSelector('#theme-view:not([hidden])');
    assert.equal(creates.length, 1, 'one submit creates the owner once');
    assert.deepEqual(await views(page), { 'welcome-view': false, 'setup-token-view': false, 'setup-view': false, 'theme-view': true, 'app-view': false, 'login-view': false }, 'the theme step follows');
    assert.equal((await page.evaluate(async () => (await fetch('/api/session')).json())).login, 'boss', 'the owner is signed in, with the login lowercased');
    // the theme tiles: a pick applies at once and is saved; arrows move it
    const tiles = await page.evaluate(() => [...document.querySelectorAll('.theme-tile')].map(tile => [tile.dataset.choice, tile.getAttribute('aria-checked'), tile.tabIndex]));
    assert.ok(tiles.length >= 2 && tiles.filter(tile => tile[1] === 'true').length === 1, `one tile is chosen: ${JSON.stringify(tiles)}`);
    const choice = tiles.find(tile => tile[1] !== 'true')[0];
    const saves = requests(page, /\/api\/me\/preferences$/);
    await page.click(`.theme-tile[data-choice="${choice}"]`);
    await page.waitForFunction(choice => document.documentElement.dataset.theme === choice || (choice === 'dark' && !document.documentElement.dataset.theme), {}, choice);
    await page.waitForFunction(() => true);
    await new Promise(resolve => setTimeout(resolve, 400));
    const saved = await page.evaluate(async () => (await (await fetch('/api/me/preferences')).json()));
    assert.equal(JSON.stringify(saved).includes(choice), true, `the pick is saved on the profile: ${JSON.stringify(saved)}`);
    assert.ok(saves.length >= 1, 'saved with a request');
    await page.keyboard.press('ArrowRight');
    assert.equal(await page.evaluate(() => document.querySelectorAll('.theme-tile[aria-checked="true"]').length), 1, 'arrow keys move the choice');
    await page.click('.theme-continue');
    await page.waitForSelector(APP);
    assert.deepEqual([await page.evaluate(() => [document.querySelector('#theme-view').hidden, document.querySelector('#welcome-view') === null]), welcomeSeen.length], [[true, true], 1], 'Continue enters the app and the welcome is recorded once');
    assert.equal(await page.evaluate(() => location.pathname), '/', 'the address leaves /setup');
    // a reload now is an ordinary visit: no welcome, no setup
    await page.reload(); await page.waitForSelector(APP);
    assert.equal(await page.evaluate(() => !document.querySelector('#welcome-view') && !document.querySelector('#setup-view') || document.querySelector('#setup-view').hidden), true, 'a reload goes straight to the app');
    assert.equal((await status(page)).open, false, 'the server has an owner now');
    await page.close();
  });
  console.log('setup page: ok');
}

main().catch(error => { console.error(error); process.exit(1); });

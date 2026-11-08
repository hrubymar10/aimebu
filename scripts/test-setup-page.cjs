// First-run owner setup against an owner-less scratch server: the welcome, the setup token (typed, or from a /setup?token= link; right,
// wrong and throttled), the Create owner form and its checks, the theme step, and what the server holds afterwards. Each flow that creates the
// owner gets a server of its own, since there is only one first run. `AUTH_BINARY` shares a built binary.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { scratch, MODULES } = require('./scratch-server.cjs');

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
  const server = await scratch({ owner: null, env: MODULES });
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
    // the form says what a valid login and password look like, before anything is typed wrong
    assert.deepEqual(await page.evaluate(() => ['setup-login-hint', 'setup-password-hint'].map(id => document.getElementById(id).textContent)), ['3 to 32 lowercase letters, digits, - or _, starting with a letter.', 'At least 12 characters.']);
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
    // exactly one tile is marked: every other tile has the same quiet frame, whatever theme it previews
    const frames = await page.evaluate(() => [...document.querySelectorAll('.theme-tile')].map(tile => [tile.getAttribute('aria-checked'), getComputedStyle(tile).borderTopColor, getComputedStyle(tile, '::after').content]));
    assert.equal(new Set(frames.filter(frame => frame[0] !== 'true').map(frame => frame[1])).size, 1, `unselected tiles share one frame colour: ${JSON.stringify(frames)}`);
    assert.deepEqual(frames.filter(frame => frame[2] !== 'none' && frame[2] !== 'normal').map(frame => frame[0]), ['true'], 'only the selected tile carries the tick');
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
  // a double submit sends one request: the token check and the owner create
  await withServer(async server => {
    const page = await toWelcome(server); await pass(page);
    const checks = requests(page, /\/api\/setup\/token$/);
    await page.type('#setup-token-form [name=token]', 'wrong-token');
    await page.$eval('#setup-token-form', form => { form.requestSubmit(); form.requestSubmit(); });
    await page.waitForFunction(() => document.querySelector('#setup-token-error').textContent !== '' && !document.querySelector('#setup-token-form button[type=submit]').disabled);
    assert.equal(checks.length, 1, 'a double submit checks the token once');
    await page.$eval('#setup-token-form [name=token]', input => { input.value = ''; });
    await page.type('#setup-token-form [name=token]', await tokenOf(page));
    await page.$eval('#setup-token-form', form => { form.requestSubmit(); form.requestSubmit(); });
    await page.waitForSelector('#setup-view:not([hidden])');
    assert.equal(checks.length, 2, 'and the right one once');
    const creates = requests(page, /\/api\/setup$/);
    await page.type('#setup-form [name=login]', 'boss'); await page.type('#setup-form [name=password]', PASSWORD); await page.type('#setup-form [name=confirm]', PASSWORD);
    await page.evaluate(() => { const form = document.querySelector('#setup-form'); form.requestSubmit(); form.requestSubmit(); });
    await page.waitForSelector('#theme-view:not([hidden])');
    assert.deepEqual([creates.length, await page.evaluate(() => document.querySelector('#setup-error').textContent)], [1, ''], 'a double submit creates the owner once and says nothing');
    await page.close();
  });
  // late answers: a second setup on a page without the app, its requests held and answered by the test in the order it chooses
  await withServer(async server => {
    const page = await server.open({ context: await server.context() });
    await page.setRequestInterception(true);
    const held = [], seen = [];
    page.on('request', request => {
      const url = new URL(request.url());
      if (/\/js\/app\.js$/.test(url.pathname)) { request.abort(); return; }
      if (/^\/api\/(setup|welcome|me\/preferences)/.test(url.pathname) && !(url.pathname === '/api/setup/status' && request.method() === 'GET')) { seen.push(`${request.method()} ${url.pathname}`); held.push({ url: url.pathname, method: request.method(), request }); } else request.continue();
    });
    await page.goto(server.origin);
    const answer = (entry, status, body) => entry.request.respond({ status, contentType: 'application/json', body: JSON.stringify(body) });
    const settle = () => new Promise(resolve => setTimeout(resolve, 150));
    const pending = async count => { for (let i = 0; i < 200 && held.length < count; i++) await new Promise(resolve => setTimeout(resolve, 25)); assert.equal(held.length, count, `expected ${count} held requests, saw ${held.length}: ${seen.join(', ')}`); };
    const make = async (search = '', options = {}) => {
      held.length = 0; seen.length = 0;
      await page.evaluate(async (search, options) => {
        const { Setup } = await import('/js/pages/setup/page.js');
        document.querySelectorAll('.late-view').forEach(node => node.remove());
        const views = {};
        for (const name of ['token', 'owner', 'theme']) { const view = document.createElement('main'); view.className = `late-view login`; view.id = `late-${name}`; view.hidden = true; document.body.append(view); views[name] = view; }
        window.__setup = new Setup({ views, search, welcome: async () => {} });
        window.__result = 'pending'; window.__setup.run().then(session => { window.__result = session; });
        window.__started = true;
      }, search, options);
    };
    const shown = () => page.evaluate(() => Object.fromEntries(['token', 'owner', 'theme'].map(name => [name, !!document.querySelector(`#late-${name}`) && !document.querySelector(`#late-${name}`).hidden && document.querySelector(`#late-${name}`).childElementCount > 0])));
    const text = selector => page.evaluate(selector => document.querySelector(selector)?.textContent ?? '', selector);
    const call = body => page.evaluate(new Function(`window.__setup.store.${body}`));
    const toOwner = async () => {
      await make();
      await page.waitForFunction(() => window.__setup.store.step.value === 'token');
      call("submitToken('typed')"); await pending(1);
      answer(held[0], 200, {}); await page.waitForFunction(() => window.__setup.store.step.value === 'owner');
      held.length = 0; seen.length = 0;
    };
    const submitOwner = () => page.evaluate(() => { window.__setup.store.submitOwner('late', 'late-password-12', 'late-password-12'); });
    const session = { user_id: 'u1', id: 'u1', login: 'late', csrf_token: 'c' };

    // the token from the link and a token typed meanwhile: the typed one is the later action, whichever answers first
    for (const [name, typed, linked, order] of [['typed rejected, link accepted, link answers first', 401, 200, 'link first'], ['typed rejected, link accepted, typed answers first', 401, 200, 'typed first'], ['typed accepted, link rejected, link answers first', 200, 401, 'link first'], ['typed accepted, link rejected, typed answers first', 200, 401, 'typed first']]) {
      await make('?token=from-link');
      await pending(1);
      const link = held[0];
      call("submitToken('typed')"); await pending(2);
      const manual = held[1];
      const reply = { link: () => answer(link, linked, linked === 200 ? {} : { message: 'link rejected' }), typed: () => answer(manual, typed, typed === 200 ? {} : { message: 'typed rejected' }) };
      if (order === 'link first') { reply.link(); await settle(); reply.typed(); } else { reply.typed(); await settle(); reply.link(); }
      await settle();
      const expected = typed === 200 ? { token: false, owner: true, theme: false } : { token: true, owner: false, theme: false };
      assert.deepEqual([await shown(), await text('#late-token #setup-token-error')], [expected, typed === 200 ? '' : 'typed rejected'], `${name}: the typed token decides, its error stays and the link's answer changes nothing`);
    }

    // a link that is right skips the token step; a wrong one is put in the field with the server's sentence
    await make('?token=from-link'); await pending(1); answer(held[0], 200, {}); await settle();
    assert.deepEqual(await shown(), { token: false, owner: true, theme: false }, 'a right link goes to the owner form');
    await make('?token=from-link'); await pending(1); answer(held[0], 401, { message: 'link rejected' }); await settle();
    assert.deepEqual([await shown(), await text('#late-token #setup-token-error'), await page.evaluate(() => document.querySelector('#late-token input').value)], [{ token: true, owner: false, theme: false }, 'link rejected', 'from-link'], 'a wrong link shows its sentence and the token in the field');

    // the link answered after destroy: nothing is shown
    await make('?token=from-link'); await pending(1);
    await call('destroy()');
    answer(held[0], 200, {}); await settle();
    assert.deepEqual(await shown(), { token: false, owner: false, theme: false }, 'a link checked and answered after destroy shows nothing');

    // the owner made, then destroyed at each boundary: nothing after that point happens
    await toOwner(); await submitOwner(); await pending(1);
    await call('destroy()');
    answer(held[0], 200, session); await settle();
    assert.deepEqual([seen.length, await page.evaluate(() => window.__result)], [1, 'pending'], 'an owner created and answered after destroy sends nothing more and finishes nothing');

    await toOwner(); await submitOwner(); await pending(1);
    answer(held[0], 200, session); await pending(2);
    assert.equal(held[1].url, '/api/welcome/seen', 'the welcome is recorded once the owner exists');
    await call('destroy()');
    answer(held[1], 200, {}); await settle();
    assert.deepEqual([seen, await page.evaluate(() => window.__result)], [['POST /api/setup', 'POST /api/welcome/seen'], 'pending'], 'the welcome answered after destroy loads no preferences');

    await toOwner(); await submitOwner(); await pending(1);
    answer(held[0], 200, session); await pending(2); answer(held[1], 200, {}); await pending(3);
    assert.equal(held[2].url, '/api/me/preferences', 'the preferences are read next');
    await call('destroy()');
    answer(held[2], 200, { preferences: {} }); await pending(4); answer(held[3], 200, {}); await settle();
    // reading the preferences uploads what only this browser holds (its own business); a theme is not among it
    const themeSaves = () => held.filter(entry => entry.method === 'PUT' && /"theme"/.test(entry.request.postData() || '')).length;
    assert.deepEqual([themeSaves(), await shown(), await page.evaluate(() => window.__result)], [0, { token: false, owner: false, theme: false }, 'pending'], 'the preferences answered after destroy show no theme step and save no theme');
    await call("select('red-dark')"); await settle();
    assert.deepEqual([themeSaves(), await page.evaluate(() => [window.__setup.store.step.value, window.__setup.store.theme.value !== 'red-dark', localStorage.getItem('aimebu2.ui.theme.u1')])], [0, ['idle', true, null]], 'the step does not open, and a theme picked after destroy is neither kept nor saved');

    // an owner refused keeps the server's sentence; a second submit while one is out sends nothing
    await toOwner();
    await page.evaluate(() => { window.__setup.store.submitOwner('late', 'late-password-12', 'late-password-12'); window.__setup.store.submitOwner('late', 'late-password-12', 'late-password-12'); }); await pending(1);
    await settle(); assert.equal(held.length, 1, 'a second submit while one is out sends nothing');
    answer(held[0], 409, { message: 'owner already exists' }); await settle();
    assert.deepEqual([await text('#late-owner #setup-error'), await page.evaluate(() => window.__setup.store.busy.owner.value)], ['owner already exists', false], 'a refused owner shows the server\'s sentence and can be tried again');

    // the theme step: pick, save, and Continue ends the run with the session and the theme
    await toOwner(); await submitOwner(); await pending(1);
    answer(held[0], 200, session); await pending(2); answer(held[1], 200, {}); await pending(3); answer(held[2], 200, { preferences: {} });
    await pending(4); answer(held[3], 200, {}); // what only this browser held is uploaded before the theme step opens
    await page.waitForFunction(() => window.__setup.store.step.value === 'theme');
    await pending(5);
    assert.deepEqual([held[4].method, held[4].url, /"theme"/.test(held[4].request.postData())], ['PUT', '/api/me/preferences', true], 'entering the theme step saves the current theme');
    answer(held[4], 200, {});
    await page.evaluate(() => { window.__setup.store.continueTheme(); });
    await page.waitForFunction(() => window.__result !== 'pending');
    assert.deepEqual(await page.evaluate(() => ({ login: window.__result.login, first: window.__result.first_run })), { login: 'late', first: false }, 'Continue ends the setup with the session');

    // saves queued behind a held one: dropped when the setup is destroyed unfinished, sent when it ended with Continue
    const toTheme = async () => {
      await toOwner(); await submitOwner(); await pending(1);
      answer(held[0], 200, session); await pending(2); answer(held[1], 200, {}); await pending(3); answer(held[2], 200, { preferences: {} });
      await pending(4); answer(held[3], 200, {});
      await page.waitForFunction(() => window.__setup.store.step.value === 'theme'); await pending(5);
      await call("select('red-dark')"); await settle();
      assert.equal(held.length, 5, 'the second theme waits behind the first save');
    };
    const themeBodies = () => held.slice(4).filter(entry => entry.method === 'PUT').map(entry => JSON.parse(entry.request.postData()).preferences.theme);
    await toTheme();
    await call('destroy()');
    answer(held[4], 200, {}); await settle();
    assert.deepEqual(themeBodies(), [themeBodies()[0]], 'a save queued behind a held one is dropped when the setup is destroyed unfinished');
    assert.equal(held.length, 5, 'and nothing more is sent');
    await toTheme();
    await page.evaluate(() => { window.__setup.store.continueTheme(); window.__setup.destroy(); });
    answer(held[4], 200, {}); await pending(6);
    assert.equal(JSON.parse(held[5].request.postData()).preferences.theme, 'red-dark', 'a save queued before Continue is still sent after the setup is torn down');
    answer(held[5], 200, {});

    // the welcome: its timer and its request after destroy
    const timers = await page.evaluate(async () => {
      const { openWelcome } = await import('/js/pages/setup/welcome.js');
      const out = {};
      const slow = openWelcome(null, { reduced: false, finishMs: 150 }); slow.destroy();
      await new Promise(resolve => setTimeout(resolve, 400));
      out.afterTimer = [!!document.querySelector('#welcome-view'), slow.store.stage.value];
      let resolveRequest, ended = false;
      const open = openWelcome(() => new Promise(resolve => { resolveRequest = resolve; }), { reduced: true });
      open.done.then(() => { ended = true; });
      document.querySelector('#welcome-continue').click();
      open.destroy(); resolveRequest();
      await new Promise(resolve => setTimeout(resolve, 200));
      out.afterRequest = [ended, !!document.querySelector('#welcome-view')];
      return out;
    });
    assert.deepEqual(timers, { afterTimer: [false, 'logo'], afterRequest: [false, false] }, 'a welcome destroyed before its timer or its request ends changes nothing');
    await page.close();
  });
  console.log('setup page: ok');
}

main().catch(error => { console.error(error); process.exit(1); });

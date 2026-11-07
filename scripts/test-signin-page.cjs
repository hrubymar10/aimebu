// Sign-in and the invitation card against a scratch server: the sign-in form, the card a link opens (a new account, a password reset, a link
// that is no good), the way between the two, the dialog a signed-in person gets for a link, and sign-out. `AUTH_BINARY` shares a built binary.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { scratch, OWNER, MODULES } = require('./scratch-server.cjs');

const APP = '#app-view:not([hidden])';

// The owner's first sign-in on an install shows the welcome before the app; the other accounts go straight in.
async function enterApp(page) {
  await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden, { timeout: 30000 });
  if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
  await page.waitForSelector(APP, { timeout: 30000 });
}

async function main() {
  const server = await scratch({ env: MODULES });
  const { origin } = server;
  try {
    const owner = await server.ownerClient();
    const folder = path.join(server.tmp, 'repo'); fs.mkdirSync(folder);
    const project = await owner('/api/projects', { method: 'POST', body: { name: 'Signin Project', folder } });
    assert.equal(project.status, 201, `project: ${JSON.stringify(project)}`);
    const projectID = project.body.id || project.body.project?.id;
    const signedOut = async () => { const page = await server.open({ context: await server.context() }); await page.goto(origin); return page; };
    const loginPosts = page => { const posts = []; page.on('request', request => { if (request.method() === 'POST' && /\/api\/login$/.test(request.url())) posts.push(request.postData()); }); return posts; };
    const state = page => page.evaluate(() => ({
      login: !document.querySelector('#login-view').hidden, app: !document.querySelector('#app-view').hidden, boot: !document.querySelector('#boot-view').hidden,
      form: !document.querySelector('#login-form').hidden, card: !document.querySelector('.invite-entry').hidden, error: document.querySelector('#login-error').textContent,
      cardError: document.querySelector('.invite-entry .error').textContent, hash: location.hash,
    }));
    const session = page => page.evaluate(async () => (await fetch('/api/session')).json());

    // signed out: the sign-in form and nothing else
    {
      const page = await signedOut();
      await page.waitForSelector('#login-view:not([hidden])');
      assert.deepEqual(await state(page), { login: true, app: false, boot: false, form: true, card: false, error: '', cardError: '', hash: '' }, 'a visit without a session shows the sign-in form');
      assert.deepEqual(await page.evaluate(() => [...document.querySelectorAll('#login-form input')].map(input => [input.name, input.type, input.required, input.autocomplete])), [['login', 'text', true, 'username'], ['password', 'password', true, 'current-password']]);
      const posts = loginPosts(page);
      // the right password signs in
      await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
      await page.$eval('#login-form', form => form.requestSubmit());
      await enterApp(page);
      assert.equal(posts.length, 1, 'one submit sends one sign-in');
      assert.deepEqual([await state(page).then(value => [value.login, value.app]), (await session(page)).login], [[false, true], OWNER.login], 'and signs in');
      // Signing out leaves nothing of the session on screen, and nothing the old session asked is put in front of the next one: on a phone, with the rooms
      // drawer, the Usages popover and Settings open (with an edit not yet saved) and a question from the shared dialog on top of it, then signed in again.
      await page.setViewport({ width: 390, height: 844 });
      await page.$eval('#rooms-toggle', button => button.click());
      await page.$eval('#usages-toggle', button => button.click());
      await page.$eval('#settings-toggle', button => button.click());
      await page.waitForSelector('#settings-dialog[open]');
      await page.$eval('#settings-telemetry-tab', button => button.click());
      await page.waitForSelector('#telemetry-dsn');
      await page.$eval('#telemetry-dsn', input => { input.value = 'https://half-typed@example.invalid/9'; input.dispatchEvent(new Event('input', { bubbles: true })); });
      await page.evaluate(async () => {
        const { confirmDialog } = await import(`/js/dialogs.js${new URL(document.querySelector('script[type=module]').src).search}`);
        window.__asked = { first: undefined, queued: undefined }; // answered when the session ends, not left pending
        confirmDialog('Remove something?').then(answer => { window.__asked.first = answer; });
        confirmDialog('A second question that waits behind it').then(answer => { window.__asked.queued = answer; });
      });
      await page.waitForSelector('#app-dialog[open]');
      assert.deepEqual(await page.evaluate(() => ({ rooms: document.querySelector('#app-view').classList.contains('rooms-open'), usages: !document.querySelector('#usages-popover').hidden })), { rooms: true, usages: true }, 'the drawer and the popover are open before signing out');
      await page.$eval('#logout', button => button.click());
      await page.waitForSelector('#login-view:not([hidden])');
      assert.deepEqual(await page.evaluate(() => ({ dialogs: document.querySelectorAll('dialog[open]').length, modal: document.querySelectorAll('dialog:modal').length, appHidden: document.querySelector('#app-view').hidden, roomsOpen: document.querySelector('#app-view').classList.contains('rooms-open'), usagesOpen: !document.querySelector('#usages-popover').hidden, asked: window.__asked })), { dialogs: 0, modal: 0, appHidden: true, roomsOpen: false, usagesOpen: false, asked: { first: false, queued: false } }, 'signing out leaves no dialog, popover or drawer, and answers the open question and the one behind it "no"');
      await page.waitForSelector('#login-form:not([hidden])');
      await page.$$eval('#login-form input', inputs => inputs.forEach(input => { input.value = ''; }));
      await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
      await page.$eval('#login-form', form => form.requestSubmit());
      await enterApp(page);
      // The next session's dialogs work (the queue was not left waiting on the one that was closed), and its Settings holds nothing half-typed.
      const next = page.evaluate(async () => {
        const { confirmDialog } = await import(`/js/dialogs.js${new URL(document.querySelector('script[type=module]').src).search}`);
        return confirmDialog('A question of the next session');
      });
      await page.waitForSelector('#app-dialog[open]');
      await page.$eval('#app-dialog .app-dialog-ok', button => button.click());
      assert.equal(await next, true, 'a dialog of the next session opens and is answered');
      await page.$eval('#settings-toggle', button => button.click());
      await page.waitForSelector('#settings-dialog[open]');
      await page.$eval('#settings-telemetry-tab', button => button.click());
      await page.waitForSelector('#telemetry-dsn');
      assert.notEqual(await page.$eval('#telemetry-dsn', input => input.value), 'https://half-typed@example.invalid/9', 'the previous session\'s unsaved edit is not in the next session\'s Settings');
      await page.keyboard.press('Escape');
      await page.setViewport({ width: 1280, height: 800 });
      await enterApp(page);
      await page.$eval('#logout', button => button.click());
      await page.waitForSelector('#login-view:not([hidden])');
      assert.deepEqual(await state(page).then(value => [value.login, value.app, value.form]), [true, false, true], 'signing out shows the sign-in form again');
      assert.equal((await session(page)).signed_in, false);
      // a wrong password says so and stays; one request
      posts.length = 0;
      await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', 'wrong-password-123');
      await page.$eval('#login-form', form => form.requestSubmit());
      await page.waitForFunction(() => document.querySelector('#login-error').textContent !== '');
      const failed = await state(page);
      assert.deepEqual([failed.login, failed.app, failed.error, posts.length], [true, false, 'invalid credentials', 1], 'a wrong password shows the server\'s sentence and stays on sign-in');
      await page.close();
    }

    // A link pasted into the address bar of a tab that already shows the sign-in only changes the hash: the card opens at once, as after a reload; then a
    // second link replaces the first; a hash with no invitation, or the same link again, changes nothing.
    {
      const first = await owner('/api/invites', { method: 'POST', body: { project_id: projectID, role: 'viewer' } });
      const second = await owner('/api/invites', { method: 'POST', body: { project_id: projectID, role: 'operator' } });
      assert.equal(first.status + second.status, 402, 'both links are made');
      const page = await signedOut();
      await page.waitForSelector('#login-view:not([hidden])');
      assert.equal(await page.evaluate(() => !document.querySelector('#login-form').hidden && document.querySelector('.invite-entry').hidden), true, 'a plain visit shows the sign-in form');
      const requests = []; page.on('request', request => { if (/\/api\/invites\/inspect$/.test(request.url())) requests.push(request.postData()); });
      await page.evaluate(() => { window.__sameDocument = true; }); // a reload would drop it (a hash change keeps the document)
      const lead = () => page.evaluate(() => document.querySelector('.invite-lead').textContent);
      await page.evaluate(token => { location.hash = `invite=${encodeURIComponent(token)}`; }, first.body.token);
      await page.waitForSelector('.invite-entry:not([hidden])', { timeout: 10000 });
      assert.match(await lead(), /invited to Signin Project as a viewer/, 'the pasted link opens its card without a reload');
      assert.equal(await page.evaluate(() => document.querySelector('#login-form').hidden), true, 'in place of the sign-in form');
      await page.evaluate(token => { location.hash = `invite=${encodeURIComponent(token)}`; }, second.body.token);
      await page.waitForFunction(() => /as an operator/.test(document.querySelector('.invite-lead').textContent), { timeout: 10000 });
      assert.equal(requests.length, 2, 'each link is looked at once');
      await page.evaluate(() => { location.hash = 'something-else'; });
      await page.evaluate(token => { location.hash = `invite=${encodeURIComponent(token)}`; }, second.body.token);
      await new Promise(resolve => setTimeout(resolve, 300));
      assert.equal(requests.length, 2, 'a hash with no invitation, and the same link again, ask nothing');
      assert.match(await lead(), /as an operator/);
      assert.equal(await page.evaluate(() => window.__sameDocument === true), true, 'the page was never reloaded');
      await page.close();
      // Signed in, a changed hash does nothing to the app.
      const inApp = await signedOut();
      await inApp.waitForSelector('#login-view:not([hidden])');
      await inApp.type('#login-form [name=login]', OWNER.login); await inApp.type('#login-form [name=password]', OWNER.password);
      await inApp.$eval('#login-form', form => form.requestSubmit());
      await enterApp(inApp);
      await inApp.evaluate(() => { location.hash = 'invite=nothing-real'; });
      await new Promise(resolve => setTimeout(resolve, 300));
      assert.deepEqual(await inApp.evaluate(() => ({ app: !document.querySelector('#app-view').hidden, login: !document.querySelector('#login-view').hidden })), { app: true, login: false }, 'a signed-in tab is left alone');
      await inApp.close();
    }

    // an invitation to join: the card says what it is for, creates the account and signs it in
    {
      const invite = await owner('/api/invites', { method: 'POST', body: { project_id: projectID, role: 'operator' } });
      assert.equal(invite.status, 201, JSON.stringify(invite));
      const page = await server.open({ context: await server.context() });
      await page.goto(`${origin}/#invite=${encodeURIComponent(invite.body.token)}`);
      await page.waitForSelector('.invite-entry:not([hidden])');
      const card = await page.evaluate(() => ({ lead: document.querySelector('.invite-lead').textContent, submit: document.querySelector('.invite-entry button[type=submit]').textContent, switch: document.querySelector('.invite-switch').textContent, switchHidden: document.querySelector('.invite-switch').hidden, form: document.querySelector('#login-form').hidden, label: document.querySelector('.invite-login').firstChild.textContent, readOnly: document.querySelector('.invite-entry [name=login]').readOnly }));
      assert.deepEqual(card, { lead: 'You are invited to Signin Project as an operator. Choose a login and a password to create your account.', submit: 'Create account and sign in', switch: 'I already have an account', switchHidden: false, form: true, label: 'Login', readOnly: false });
      // a password too short is refused by the form, not sent
      await page.type('.invite-entry [name=login]', 'newcomer'); await page.type('.invite-entry [name=password]', 'short');
      const redeems = []; page.on('request', request => { if (/\/api\/invites\/redeem$/.test(request.url())) redeems.push(request.postData()); });
      await page.$eval('.invite-entry', form => form.requestSubmit());
      assert.equal(redeems.length, 0, 'a short password is not sent');
      await page.$eval('.invite-entry [name=password]', input => { input.value = ''; });
      await page.type('.invite-entry [name=password]', 'a-long-password-1');
      await page.$eval('.invite-entry', form => form.requestSubmit());
      await enterApp(page);
      assert.deepEqual([redeems.length, (await session(page)).login, await page.evaluate(() => location.hash.includes('invite'))], [1, 'newcomer', false], 'the card creates the account once, signs it in and clears the link');
      assert.equal((await owner('/api/invites/inspect', { method: 'POST', body: { token: invite.body.token } })).status >= 400, true, 'the link is spent');
      await page.close();
      // a password reset for that account
      const users = await owner('/api/users');
      const target = (users.body.users || users.body).find(user => user.login === 'newcomer');
      const reset = await owner('/api/invites', { method: 'POST', body: { kind: 'reset', target_user_id: target.id } });
      assert.equal(reset.status, 201, JSON.stringify(reset));
      const again = await server.open({ context: await server.context() });
      await again.goto(`${origin}/#invite=${encodeURIComponent(reset.body.token)}`);
      await again.waitForSelector('.invite-entry:not([hidden])');
      const resetCard = await again.evaluate(() => ({ lead: document.querySelector('.invite-lead').textContent, submit: document.querySelector('.invite-entry button[type=submit]').textContent, switchHidden: document.querySelector('.invite-switch').hidden, label: document.querySelector('.invite-login').firstChild.textContent, login: document.querySelector('.invite-entry [name=login]').value, readOnly: document.querySelector('.invite-entry [name=login]').readOnly }));
      assert.deepEqual(resetCard, { lead: 'Choose a new password for newcomer.', submit: 'Set password and sign in', switchHidden: true, label: 'Account', login: 'newcomer', readOnly: true });
      await again.type('.invite-entry [name=password]', 'another-long-pass-2');
      await again.$eval('.invite-entry', form => form.requestSubmit());
      await enterApp(again);
      assert.equal((await session(again)).login, 'newcomer', 'a reset signs the account in with its new password');
      await again.close();
    }

    // a link that is no good, and the way back to sign-in
    {
      const page = await server.open({ context: await server.context() });
      await page.goto(`${origin}/#invite=not-a-real-token`);
      await page.waitForSelector('.invite-entry:not([hidden])');
      const bad = await page.evaluate(() => ({ lead: document.querySelector('.invite-lead').textContent, labels: [...document.querySelectorAll('.invite-entry label')].every(label => label.hidden), submit: document.querySelector('.invite-entry button[type=submit]').hidden, switch: document.querySelector('.invite-switch').textContent }));
      assert.deepEqual(bad, { lead: 'This invitation is not valid: it was already used, it expired, or the person who made it can no longer invite.', labels: true, submit: true, switch: 'Sign in' });
      await page.click('.invite-switch');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('.invite-entry').hidden, document.querySelector('#login-form').hidden, document.querySelector('.invite-back').hidden]), [true, false, true], 'Sign in leaves the dead link behind');
      await page.close();
    }

    // "I already have an account" shows sign-in with the way back to the card
    {
      const other = path.join(server.tmp, 'other'); fs.mkdirSync(other);
      const second = await owner('/api/projects', { method: 'POST', body: { name: 'Other Project', folder: other } });
      const invite = await owner('/api/invites', { method: 'POST', body: { project_id: second.body.id || second.body.project?.id, role: 'viewer' } });
      const page = await server.open({ context: await server.context() });
      await page.goto(`${origin}/#invite=${encodeURIComponent(invite.body.token)}`);
      await page.waitForSelector('.invite-entry:not([hidden])');
      await page.click('.invite-switch');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('.invite-entry').hidden, document.querySelector('#login-form').hidden, document.querySelector('.invite-back').hidden]), [true, false, false], 'the card gives way to sign-in, with a way back');
      await page.click('.invite-back');
      await page.waitForSelector('.invite-entry:not([hidden])');
      assert.deepEqual(await page.evaluate(() => [document.querySelector('.invite-entry').hidden, document.querySelector('#login-form').hidden, document.querySelector('.invite-lead').textContent.startsWith('You are invited to Other Project as a viewer')]), [false, true, true], 'and back to the card');
      // signing in with the link still pending offers it in a dialog; using it closes the dialog
      await page.click('.invite-switch');
      await page.type('#login-form [name=login]', 'newcomer'); await page.type('#login-form [name=password]', 'another-long-pass-2');
      await page.$eval('#login-form', form => form.requestSubmit());
      await enterApp(page);
      await page.waitForSelector('.invite-dialog[open]');
      assert.equal(await page.$eval('.invite-dialog [name=password]', el => el.closest('label').hidden), true, 'a plain invitation asks for no password');
      await page.$eval('.invite-dialog form', form => form.requestSubmit());
      await page.waitForFunction(() => !document.querySelector('.invite-dialog').open);
      assert.equal(await page.evaluate(() => location.hash.includes('invite')), false, 'using the link in the dialog clears it');
      await page.close();
    }

    // a double submit sends one request, in the form, on the card and in the dialog
    {
      const redeemsOf = page => { const posts = []; page.on('request', request => { if (/\/api\/invites\/redeem$/.test(request.url())) posts.push(request.postData()); }); return posts; };
      const page = await signedOut();
      await page.waitForSelector('#login-view:not([hidden])');
      const posts = loginPosts(page);
      await page.type('#login-form [name=login]', 'newcomer'); await page.type('#login-form [name=password]', 'another-long-pass-2');
      await page.$eval('#login-form', form => { form.requestSubmit(); form.requestSubmit(); });
      await enterApp(page);
      assert.deepEqual([posts.length, await page.evaluate(() => document.querySelector('#login-error').textContent)], [1, ''], 'a double submit signs in once and says nothing');
      await page.close();
      const invite = await owner('/api/invites', { method: 'POST', body: { project_id: projectID, role: 'viewer' } });
      const card = await server.open({ context: await server.context() });
      await card.goto(`${origin}/#invite=${encodeURIComponent(invite.body.token)}`);
      await card.waitForSelector('.invite-entry:not([hidden])');
      const redeems = redeemsOf(card);
      await card.type('.invite-entry [name=login]', 'twice'); await card.type('.invite-entry [name=password]', 'a-long-password-3');
      await card.$eval('.invite-entry', form => { form.requestSubmit(); form.requestSubmit(); });
      await enterApp(card);
      assert.deepEqual([redeems.length, await card.evaluate(() => document.querySelector('.invite-entry .error').textContent)], [1, ''], 'a double submit on the card redeems once');
      await card.close();
      const third = await owner('/api/projects', { method: 'POST', body: { name: 'Third Project', folder: (() => { const dir = path.join(server.tmp, 'third'); fs.mkdirSync(dir); return dir; })() } });
      const join = await owner('/api/invites', { method: 'POST', body: { project_id: third.body.id || third.body.project?.id, role: 'viewer' } });
      const dialog = await signedOut();
      await dialog.waitForSelector('#login-view:not([hidden])');
      await dialog.evaluate(token => { location.hash = `invite=${encodeURIComponent(token)}`; location.reload(); }, join.body.token);
      await dialog.waitForSelector('.invite-entry:not([hidden])');
      await dialog.click('.invite-switch');
      await dialog.type('#login-form [name=login]', 'twice'); await dialog.type('#login-form [name=password]', 'a-long-password-3');
      await dialog.$eval('#login-form', form => form.requestSubmit());
      await enterApp(dialog);
      await dialog.waitForSelector('.invite-dialog[open]');
      const used = redeemsOf(dialog);
      await dialog.$eval('.invite-dialog form', form => { form.requestSubmit(); form.requestSubmit(); });
      await dialog.waitForFunction(() => !document.querySelector('.invite-dialog').open);
      assert.deepEqual([used.length, await dialog.evaluate(() => document.querySelector('.invite-dialog .error').textContent)], [1, ''], 'a double submit in the dialog uses the link once');
      await dialog.close();
    }


    // late answers: a second sign-in page on a signed-out page, its requests held and answered by the test in the order it chooses
    {
      const stale = await signedOut();
      await stale.waitForSelector('#login-view:not([hidden])');
      await stale.setRequestInterception(true);
      const held = [];
      stale.on('request', request => {
        if (request.method() === 'POST' && /\/api\/(login|invites\/(inspect|redeem))$/.test(request.url())) held.push({ url: request.url(), request });
        else request.continue();
      });
      const answer = (entry, status, body) => entry.request.respond({ status, contentType: 'application/json', body: JSON.stringify(body) });
      const pending = async count => { await stale.waitForFunction(() => true); for (let i = 0; i < 200 && held.length < count; i++) await new Promise(resolve => setTimeout(resolve, 25)); assert.equal(held.length, count, `expected ${count} held requests, saw ${held.length}`); };
      const settle = () => new Promise(resolve => setTimeout(resolve, 150));
      const make = async (hash = '') => stale.evaluate(async hash => {
        const { Signin } = await import('/js/pages/signin/page.js');
        const view = document.createElement('main'); view.id = 'late-view'; document.body.append(view);
        window.__calls = { signedIn: 0, afterReset: 0, resync: 0 };
        window.__signin = new Signin({ view, hash, signedIn: () => { window.__calls.signedIn++; }, afterReset: () => { window.__calls.afterReset++; }, resync: () => { window.__calls.resync++; }, onError: error => { throw error; } });
        return true;
      }, hash);
      const read = () => stale.evaluate(() => {
        const at = selector => document.querySelector(`#late-view ${selector}`);
        return {
          calls: { ...window.__calls }, error: at('#login-error')?.textContent ?? '', cardError: at('.invite-entry .error')?.textContent ?? '',
          form: !!at('#login-form') && !at('#login-form').hidden, card: !!at('.invite-entry') && !at('.invite-entry').hidden,
          lead: at('.invite-lead')?.textContent ?? '', submit: at('.invite-entry button[type=submit]')?.textContent ?? '',
        };
      });
      const submitLogin = () => stale.evaluate(() => { const form = document.querySelector('#late-view #login-form'); form.elements.login.value = 'late'; form.elements.password.value = 'late-password-1'; form.requestSubmit(); });
      const teardown = async () => { await stale.evaluate(() => { window.__signin.destroy(); document.querySelector('#late-view')?.remove(); }); held.length = 0; };
      const sentence = { message: 'invalid credentials' };

      // sign-in superseded by hide, by show and by destroy: the late session signs nobody in and a late error shows nothing
      for (const [name, supersede] of [['hide', () => { window.__signin.hide(); }], ['show', () => { window.__signin.show(); }], ['destroy', () => { window.__signin.destroy(); }]]) {
        await make(); held.length = 0;
        await stale.evaluate(() => { window.__signin.show(); }); await settle();
        await submitLogin(); await pending(1);
        await stale.evaluate(supersede);
        answer(held[0], 200, { id: 'x', login: 'late', csrf_token: 'c' }); await settle();
        assert.equal((await read()).calls.signedIn, 0, `a sign-in answered after ${name} signs nobody in`);
        await teardown();
        await make(); held.length = 0;
        await stale.evaluate(() => { window.__signin.show(); }); await settle();
        await submitLogin(); await pending(1);
        await stale.evaluate(supersede);
        answer(held[0], 401, { message: 'a late error' }); await settle();
        const after = (await read()).error;
        assert.equal(after, '', `a failure answered after ${name} writes nothing`);
        await teardown();
      }

      // a replaced sign-in: the first answer comes last and does not replace the current error text, in either completion order
      for (const order of ['current first', 'stale first']) {
        await make(); held.length = 0;
        await stale.evaluate(() => { window.__signin.show(); }); await settle();
        await submitLogin(); await pending(1);
        await stale.evaluate(() => { window.__signin.show(); }); await settle();
        await submitLogin(); await pending(2);
        const [first, second] = held;
        if (order === 'current first') { answer(second, 401, { message: 'current' }); await settle(); answer(first, 401, { message: 'stale' }); }
        else { answer(first, 401, { message: 'stale' }); await settle(); answer(second, 401, { message: 'current' }); }
        await settle();
        const result = await read();
        assert.deepEqual([result.error, result.calls.signedIn], ['current', 0], `${order}: the current error stays and the stale answer changes nothing`);
        await teardown();
      }

      // a successful sign-in still reaches the host once
      await make(); held.length = 0;
      await stale.evaluate(() => { window.__signin.show(); }); await settle();
      await submitLogin(); await pending(1);
      answer(held[0], 200, { id: 'x', login: 'late', csrf_token: 'c' }); await settle();
      assert.deepEqual([(await read()).calls.signedIn, (await read()).error], [1, ''], 'the current answer signs in');
      await teardown();

      // a sign-in answered after the person switched to the invitation card changes nothing
      const token = (await owner('/api/invites', { method: 'POST', body: { project_id: projectID, role: 'viewer' } })).body.token;
      const info = { kind: 'join', project_name: 'Signin Project', role: 'viewer' };
      await make(`#invite=${encodeURIComponent(token)}`); held.length = 0;
      await stale.evaluate(() => { window.__signin.show(); }); await pending(1);
      answer(held[0], 200, info); await settle();
      assert.deepEqual([(await read()).card, (await read()).lead], [true, 'You are invited to Signin Project as a viewer. Choose a login and a password to create your account.'], 'the link opens the card');
      await stale.evaluate(() => document.querySelector('#late-view .invite-switch').click());
      await submitLogin(); await pending(2);
      await stale.evaluate(() => document.querySelector('#late-view .invite-back').click()); await pending(3);
      answer(held[2], 200, info); await settle();
      answer(held[1], 200, { id: 'x', login: 'late', csrf_token: 'c' }); await settle();
      const back = await read();
      assert.deepEqual([back.calls.signedIn, back.card, back.form], [0, true, false], 'a sign-in answered after returning to the invitation signs nobody in');

      // an inspection superseded by a newer one: whichever answers last, the newer link decides the card
      held.length = 0;
      await stale.evaluate(() => { window.__signin.show(); window.__signin.show(); }); await pending(2);
      answer(held[1], 200, { ...info, project_name: 'Newer' }); await settle();
      answer(held[0], 200, { error: 1, kind: 'x', project_name: 'Older' }); await settle();
      assert.match((await read()).lead, /invited to Newer/, 'an older inspection answered later does not replace the card');
      held.length = 0;
      await stale.evaluate(() => { window.__signin.show(); window.__signin.show(); }); await pending(2);
      answer(held[0], 200, { ...info, project_name: 'Older' }); await settle();
      answer(held[1], 200, { ...info, project_name: 'Newer' }); await settle();
      assert.match((await read()).lead, /invited to Newer/, 'and the newer one wins when it answers last');

      // a redemption superseded by switching to the ordinary sign-in: the link is not redeemed into the card, and nobody is signed in
      held.length = 0;
      await stale.evaluate(() => { const card = document.querySelector('#late-view .invite-entry'); card.elements.login.value = 'late-user'; card.elements.password.value = 'late-password-1'; card.requestSubmit(); }); await pending(1);
      await stale.evaluate(() => document.querySelector('#late-view .invite-switch').click());
      answer(held[0], 200, { login: 'late-user' }); await settle();
      const switched = await read();
      assert.deepEqual([switched.calls.signedIn, switched.form, switched.card, switched.submit], [0, true, false, 'Create account and sign in'], 'a redemption answered after switching to sign-in changes nothing');
      assert.equal(held.length, 1, 'and does not go on to sign in');

      // the latest action decides: a sign-in and an inspection of the link started one after the other, answered in either order
      for (const [first, second, order] of [['inspect', 'login', 'inspection answers first'], ['inspect', 'login', 'sign-in answers first'], ['login', 'inspect', 'sign-in answers first'], ['login', 'inspect', 'inspection answers first']]) {
        await teardown();
        await make(`#invite=${encodeURIComponent(token)}`); held.length = 0;
        const start = { inspect: () => stale.evaluate(() => { window.__signin.show(); }), login: submitLogin };
        await start[first](); await pending(1);
        await start[second](); await pending(2);
        const [one, two] = held, byKind = { inspect: /inspect$/.test(one.url) ? one : two, login: /login$/.test(one.url) ? one : two };
        const reply = { inspect: () => answer(byKind.inspect, 200, info), login: () => answer(byKind.login, 200, { id: 'x', login: 'late', csrf_token: 'c' }) };
        if (order.startsWith('inspection')) { reply.inspect(); await settle(); reply.login(); } else { reply.login(); await settle(); reply.inspect(); }
        await settle();
        const result = await read();
        assert.deepEqual([result.calls.signedIn, result.card], second === 'login' ? [1, false] : [0, true], `${first} then ${second}, ${order}: only the later action changes the view or the host`);
      }

      // the dialog closed with Cancel or Escape while its redemption is out: the answer changes nothing
      for (const closer of ['Cancel', 'Escape']) {
        await teardown();
        await make(`#invite=${encodeURIComponent(token)}`); held.length = 0;
        await stale.evaluate(() => { window.__signin.openDialog(); }); await stale.waitForSelector('.invite-dialog[open]');
        await stale.evaluate(() => { document.querySelector('.invite-dialog[open] form').requestSubmit(); }); await pending(1);
        if (closer === 'Cancel') await stale.evaluate(() => { document.querySelector('.invite-dialog[open] .invite-cancel').click(); });
        else await stale.keyboard.press('Escape');
        await stale.waitForFunction(() => !document.querySelector('.invite-dialog[open]'));
        answer(held[0], 200, { kind: 'reset' }); await settle();
        const after = await stale.evaluate(() => ({ calls: { ...window.__calls }, pending: window.__signin.pendingInvite, error: [...document.querySelectorAll('.invite-dialog .error')].map(node => node.textContent).join('') }));
        assert.deepEqual([after.calls, after.pending === token, after.error], [{ signedIn: 0, afterReset: 0, resync: 0 }, true, ''], `${closer}: a redemption answered after the dialog closed changes nothing`);
        // the same for a failure
        await stale.evaluate(() => { window.__signin.openDialog(); }); await stale.waitForSelector('.invite-dialog[open]');
        held.length = 0;
        await stale.evaluate(() => { document.querySelector('.invite-dialog[open] form').requestSubmit(); }); await pending(1);
        if (closer === 'Cancel') await stale.evaluate(() => { document.querySelector('.invite-dialog[open] .invite-cancel').click(); });
        else await stale.keyboard.press('Escape');
        await stale.waitForFunction(() => !document.querySelector('.invite-dialog[open]'));
        answer(held[0], 410, { message: 'too late' }); await settle();
        assert.equal(await stale.evaluate(() => [...document.querySelectorAll('.invite-dialog .error')].map(node => node.textContent).join('')), '', `${closer}: a failure answered after the dialog closed shows nothing`);
      }

      // the dialog: a use of the link answered after destroy reaches nobody
      await teardown();
      await make(`#invite=${encodeURIComponent(token)}`); held.length = 0;
      await stale.evaluate(() => { window.__signin.openDialog(); }); await stale.waitForSelector('.invite-dialog[open]');
      await stale.evaluate(() => { document.querySelectorAll('.invite-dialog form')[document.querySelectorAll('.invite-dialog form').length - 1].requestSubmit(); }); await pending(1);
      await stale.evaluate(() => window.__signin.destroy());
      answer(held[0], 200, { kind: 'reset' }); await settle();
      assert.deepEqual(await stale.evaluate(() => ({ ...window.__calls })), { signedIn: 0, afterReset: 0, resync: 0 }, 'a link used in the dialog and answered after destroy reaches no one');
      await stale.close();
    }

    assert.deepEqual(server.errors, [], `the pages reported errors: ${server.errors.join(' | ')}`);
    console.log('sign-in page: ok');
  } finally {
    await server.stop();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

// Sign-in and the invitation card against a scratch server: the sign-in form, the card a link opens (a new account, a password reset, a link
// that is no good), the way between the two, the dialog a signed-in person gets for a link, and sign-out. `AUTH_BINARY` shares a built binary.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { scratch, OWNER } = require('./scratch-server.cjs');

const APP = '#app-view:not([hidden])';

// The owner's first sign-in on an install shows the welcome before the app; the other accounts go straight in.
async function enterApp(page) {
  await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden, { timeout: 30000 });
  if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
  await page.waitForSelector(APP, { timeout: 30000 });
}

async function main() {
  const server = await scratch();
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
      // sign out returns to the form, which signs in again
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

    assert.deepEqual(server.errors, [], `the pages reported errors: ${server.errors.join(' | ')}`);
    console.log('sign-in page: ok');
  } finally {
    await server.stop();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

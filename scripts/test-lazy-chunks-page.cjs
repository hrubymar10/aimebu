// The bundle's lazily fetched chunks, in the real app on a scratch server (bundled): a chunk a newer build replaced (404) is told to the person, never
// swallowed (a toast when the build is the same, a reload or the "new version" bar when the server is another build); sign-in is in the entry and works
// with every lazy chunk gone; what was queued for a chunk that arrives after the session ended does not show in the next one; and Back to a settings
// address before the Settings chunk is loaded opens it; and the Harnesses offer of a sign-in (a card that arrives after the page is ready, once the
// Onboarding chunk is fetched) is announced as settled on #app-view, so a script that clears the card does not race it.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { scratch, OWNER } = require('./scratch-server.cjs');

const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

async function signIn(page, origin) {
  await page.goto(origin);
  await page.waitForSelector('#login-view:not([hidden])');
  await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
  await page.$eval('#login-form', form => form.requestSubmit());
  await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
  if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
  await page.waitForSelector('#app-view:not([hidden])');
}

async function main() {
  const server = await scratch();
  try {
    // The chunks the page imports at load are hinted in the page; every other /bundle/ script is a lazily fetched one.
    const html = await (await fetch(server.origin + '/')).text();
    const shared = new Set([...html.matchAll(/href="\/bundle\/([A-Za-z0-9_-]+\.js)"/g)].map(match => match[1]));
    const lazyChunk = url => { const match = /^\/bundle\/([A-Za-z0-9_-]+\.js)$/.exec(new URL(url).pathname); return !!match && match[1] !== 'app.js' && !shared.has(match[1]); };

    // 1. Every lazy chunk is gone (the old build's names after an upgrade): the signed-out page still draws and signs in, and Settings says so.
    const page = await server.open({ width: 1280, height: 900, context: await server.context() });
    const reported = []; page.on('console', message => { const match = /could not load ([a-z0-9-]+):/.exec(message.text()); if (match) reported.push(match[1]); });
    let buildVersion = null; // when set, /api/buildinfo answers this version (another build)
    await page.setRequestInterception(true);
    page.on('request', request => {
      const url = request.url();
      if (lazyChunk(url)) { request.respond({ status: 404, contentType: 'text/plain', body: 'not found' }); return; }
      if (buildVersion && new URL(url).pathname === '/api/buildinfo') { request.respond({ status: 200, contentType: 'application/json', body: JSON.stringify({ version: buildVersion, go_version: 'go' }) }); return; }
      request.continue();
    });
    await signIn(page, server.origin);
    await page.waitForFunction(() => document.querySelector('#build-version')?.textContent.length > 1);
    await page.$eval('#settings-toggle', button => button.click());
    await page.waitForFunction(() => !document.querySelector('#app-toast').hidden && /Could not load that part of the page/.test(document.querySelector('#app-toast').textContent), { timeout: 8000 });
    assert.equal(await page.evaluate(() => document.querySelector('#settings-dialog').open), false, 'Settings did not open without its chunk');
    assert.equal(await page.$('#update-bar'), null, 'the same build is no reason to ask for a reload');
    assert.ok(reported.includes('settings'), 'the person'+"'s own open of Settings reports");
    assert.ok(!reported.includes('health'), `the app's own health refresh reports nothing (${reported})`);

    // 2. The server is another build: with something typed a bar stays up (and Settings is retried by the next click); with nothing typed the page reloads.
    buildVersion = 'aimebu v9.9.9-new';
    await page.evaluate(() => { const box = document.querySelector('#composer textarea[name=body]'); if (box) { box.value = 'a draft'; box.dispatchEvent(new Event('input', { bubbles: true })); } });
    await page.evaluate(() => document.querySelector('#settings-toggle').click());
    await page.waitForSelector('#update-bar', { timeout: 8000 });
    assert.match(await page.$eval('#update-bar', bar => bar.textContent), /A new version is ready, reload to continue/);
    assert.equal(await page.$eval('#update-bar button', button => button.getAttribute('aria-label')), 'Reload', 'the bar has a reload icon button');
    await page.evaluate(() => { const box = document.querySelector('#composer textarea[name=body]'); box.value = ''; box.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.evaluate(() => document.querySelector('#update-bar')?.remove());
    const reloaded = page.waitForNavigation({ timeout: 8000 });
    await page.evaluate(() => document.querySelector('#settings-toggle').click());
    await reloaded;
    await page.close();

    // 3. Back to a settings address before the dialog is loaded opens it (a fresh page, nothing blocked).
    const history = await server.open({ width: 1280, height: 900, context: await server.context() });
    await signIn(history, server.origin);
    await history.evaluate(() => { history.pushState(null, '', '#settings/server/storage'); history.pushState(null, '', '#nothing'); });
    await history.evaluate(() => history.back());
    await history.waitForFunction(() => document.querySelector('#settings-dialog').open, { timeout: 8000 });
    await history.close();

    // 5. A Settings asked for while the chunk is on its way, and a session that ends before it arrives: the dialog does not open over sign-in.
    const slow = await server.open({ width: 1280, height: 900, context: await server.context() });
    await slow.setRequestInterception(true);
    let delayed = false;
    slow.on('request', request => {
      if (delayed && lazyChunk(request.url())) setTimeout(() => request.continue().catch(() => {}), 3000); else request.continue();
    });
    await signIn(slow, server.origin);
    delayed = true;
    await slow.evaluate(() => { document.querySelector('#settings-toggle').click(); document.querySelector('#logout').click(); });
    await slow.waitForSelector('#login-view:not([hidden])', { timeout: 8000 });
    await sleep(4500);
    assert.equal(await slow.evaluate(() => document.querySelector('#settings-dialog')?.open ?? false), false, 'Settings of the ended session did not open over sign-in');
    await slow.close();

    // 6. The first-run setup page is a chunk: when it cannot be fetched the boot view says so with Retry, not a blank page.
    const setup = await server.open({ width: 1280, height: 900, context: await server.context() });
    await setup.setRequestInterception(true);
    setup.on('request', request => { // (this server has an owner, so its own /setup is a 404: the page is served there as the setup of a new server would be)
      if (new URL(request.url()).pathname === '/setup') request.respond({ status: 200, contentType: 'text/html', body: html });
      else if (lazyChunk(request.url())) request.respond({ status: 404, contentType: 'text/plain', body: 'not found' }); else request.continue(); });
    await setup.goto(server.origin + '/setup');
    await setup.waitForFunction(() => !document.querySelector('#boot-view').hidden && !document.querySelector('#boot-retry').hidden, { timeout: 8000 });
    assert.match(await setup.$eval('#boot-status', node => node.textContent), /Could not load/);
    await setup.close();

    // 4. An onboarding that was queued for a chunk still on its way when the session ends does not show in the next one.
    const late = await server.open({ width: 1280, height: 900, context: await server.context() });
    await late.setRequestInterception(true);
    late.on('request', request => {
      if (lazyChunk(request.url())) setTimeout(() => request.continue().catch(() => {}), 3000); else request.continue();
    });
    late.on('dialog', dialog => dialog.accept().catch(() => {}));
    await signIn(late, server.origin);
    await late.$eval('#logout', button => button.click());
    await late.waitForSelector('#login-view:not([hidden])', { timeout: 8000 });
    await sleep(4500); // the chunk arrives
    assert.equal(await late.evaluate(() => document.querySelector('#onboarding-view').hidden), true, 'the onboarding of the ended session did not show over sign-in');
    assert.equal(await late.evaluate(() => document.querySelector('#login-view').hidden), false, 'sign-in stays visible');
    await late.close();

    // 7. The Harnesses offer is made after the page is ready, and its card waits for the Onboarding chunk (here 3 s on its way): the app says when the offer
    // is settled (data-harness-offer), and the card is on screen by then. A script that waited for the page and looked for the card found none, then it came.
    const owner = await server.ownerClient();
    const folder = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'aimebu-lazy-'));
    execFileSync('git', ['init', '-q'], { cwd: folder });
    assert.equal((await owner('/api/projects', { method: 'POST', body: { name: 'Offer', folder } })).status, 201, 'a project exists, so the first-run card is not the one shown');
    const offer = await server.open({ width: 1280, height: 900, context: await server.context() });
    await offer.setRequestInterception(true);
    const ownerChunks = new Set();
    offer.on('request', request => { if (lazyChunk(request.url())) ownerChunks.add(request.url()); });
    offer.on('request', request => { if (lazyChunk(request.url())) setTimeout(() => request.continue().catch(() => {}), 3000); else request.continue(); });
    await offer.goto(server.origin);
    await offer.waitForSelector('#login-view:not([hidden])');
    await offer.type('#login-form [name=login]', OWNER.login); await offer.type('#login-form [name=password]', OWNER.password);
    await offer.$eval('#login-form', form => form.requestSubmit());
    await offer.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
    if (await offer.$('#welcome-continue')) { await offer.waitForSelector('#welcome-continue:not([inert])'); await offer.click('#welcome-continue'); }
    await offer.waitForSelector('#app-view:not([hidden]):not([aria-busy])', { timeout: 20000 });
    assert.equal(await offer.evaluate(() => [document.querySelector('#onboarding-view').hidden, document.querySelector('#app-view').dataset.harnessOffer ?? null]).then(String), 'true,', 'the page is ready before the Harnesses card (its chunk is on its way): the window a script used to miss');
    await offer.waitForSelector('#app-view[data-harness-offer=settled]', { timeout: 20000 });
    assert.equal(await offer.evaluate(() => !document.querySelector('#onboarding-view').hidden), true, 'the Harnesses card is on screen when the offer is settled');
    await offer.$eval('#logout', button => button.click());
    await offer.waitForSelector('#login-view:not([hidden])', { timeout: 8000 });
    assert.equal(await offer.evaluate(() => document.querySelector('#app-view').dataset.harnessOffer ?? null), null, 'a session that ended is not settled');
    await offer.close();

    // 8. Only the owner is offered the Harnesses card, so only the owner's sign-in fetches the Onboarding chunk (the one that holds that card).
    let onboardingChunk = null;
    for (const url of ownerChunks) if ((await (await fetch(url)).text()).includes('onboarding-harnesses-skip')) onboardingChunk = url;
    assert.ok(onboardingChunk, 'the owner\'s sign-in fetched the Onboarding chunk');
    const project = (await owner('/api/projects')).body;
    const projectId = (project.projects || project)[0].id;
    const invite = await owner('/api/invites', { method: 'POST', body: { kind: 'join', project_id: projectId, role: 'viewer' } });
    assert.equal(invite.status, 201, `an invitation to the project (${JSON.stringify(invite.body)})`);
    const member = { login: 'member', password: 'member-password-123' };
    const redeemed = await fetch(server.origin + '/api/invites/redeem', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ token: invite.body.token, ...member }) });
    assert.ok(redeemed.status < 300, `the member joined (${redeemed.status})`);
    const other = await server.open({ width: 1280, height: 900, context: await server.context() });
    const memberChunks = [];
    other.on('request', request => { if (lazyChunk(request.url())) memberChunks.push(request.url()); });
    await other.goto(server.origin);
    assert.equal((await server.signIn(other, member)).status, 200, 'the member signs in');
    await other.goto(server.origin);
    await other.waitForSelector('#app-view:not([hidden]):not([aria-busy])', { timeout: 20000 });
    await other.waitForSelector('#app-view[data-harness-offer=settled]', { timeout: 20000 });
    await sleep(500);
    assert.ok(!memberChunks.includes(onboardingChunk), `a member's sign-in does not fetch the Onboarding chunk (${memberChunks})`);
    assert.equal(await other.evaluate(() => document.querySelector('#onboarding-view').hidden), true, 'and is offered no card');
    await other.close();
    assert.deepEqual(server.errors.filter(text => !/Failed to load resource|Could not load|not found|dynamically imported|Importing a module script failed/i.test(text)), [], 'no other page error');
  } finally { await server.stop(); }
  console.log('lazy chunks page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });

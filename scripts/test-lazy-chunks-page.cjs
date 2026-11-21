// The bundle's lazily fetched chunks, in the real app on a scratch server (bundled): a chunk a newer build replaced (404) is told to the person, never
// swallowed (a toast when the build is the same, a reload or the "new version" bar when the server is another build); sign-in is in the entry and works
// with every lazy chunk gone; what was queued for a chunk that arrives after the session ended does not show in the next one; and Back to a settings
// address before the Settings chunk is loaded opens it.
const assert = require('node:assert/strict');
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
    assert.deepEqual(server.errors.filter(text => !/Failed to load resource|Could not load|not found|dynamically imported|Importing a module script failed/i.test(text)), [], 'no other page error');
  } finally { await server.stop(); }
  console.log('lazy chunks page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });

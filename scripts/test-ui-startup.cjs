// The smoke's start-up and v1-import-offer checks: a dropped event stream and a replaced server, a held or stalled start, a refused or late import status, a closed import. Each one drives a browser context of its own against the server, so it needs only a signed-in owner.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const { spawn, spawnSync } = require('node:child_process');
const { start, isolatedEnv, binary } = require('./ui-smoke-lib.cjs');

async function main() {
  const ctx = await start();
  const { tempRoot, tmp, gitRoot, testHome, seedBinary, APP_READY_TIMEOUT, appReadyFolded, appReady, freePort, waitDump, waitRoomRow, installFixtureChat, child, browser, supervisor, createBrowserContext, a, b, first, second, origin, port, domClick, reachRow, projectMenuAct, iconOnlyReport, disableRandomUUID, answerDialogs, login, setChecked, settingsNav, openSettings, closeRoomInfo, openProjectSettings, settingsTab, welcomeState, settled, iconState, assertIcon, assertFinished, skipHarnessOffer } = ctx;
  try {
    // A database nobody has signed in to shows the owner the welcome first, before the checks below start the app; the smoke's own first sign-in consumes it.
    await login(first);
    const replayContext = await browser.createBrowserContext();
    const replayPage = await replayContext.newPage();
    await replayPage.goto(origin);
    const replayRetry = await replayPage.evaluate(async () => {
      const { EventStream } = await import('/js/events.js');
      const original = window.WebSocket;
      const sockets = [];
      class FakeSocket {
        constructor(url) { this.url = url; sockets.push(this); }
        close() { this.onclose?.(); }
        emit(frame) { this.onmessage?.({ data: JSON.stringify(frame) }); }
      }
      window.WebSocket = FakeSocket;
      let snapshots = 0, live = 0;
      const stream = new EventStream(() => { live++; }, async () => {
        if (++snapshots === 1) throw new Error('temporary snapshot failure');
        return 12;
      }, () => {}, 5);
      const until = async predicate => {
        for (let i = 0; i < 200; i++) {
          if (predicate()) return;
          await new Promise(resolve => setTimeout(resolve, 10));
        }
        throw new Error('event replay did not finish');
      };
      try {
        stream.start();
        // The first connection comes up with nothing to replay, then drops: what the next one replays can be anything, so it takes a snapshot.
        sockets[0].emit({ type: 'ready', seq: 5 });
        await until(() => stream.ready);
        sockets[0].close();
        await until(() => sockets.length === 2);
        sockets[1].emit({ type: 'event', event: { seq: 8 } });
        sockets[1].emit({ type: 'ready', seq: 8 });
        await until(() => sockets.length === 3);
        const retryAfter = new URL(sockets[2].url).searchParams.get('after');
        sockets[2].emit({ type: 'event', event: { seq: 8 } });
        sockets[2].emit({ type: 'ready', seq: 8 });
        await until(() => stream.ready && stream.seq === 12);
        sockets[2].emit({ type: 'event', event: { seq: 13 } });
        await until(() => live === 1);
        return { retryAfter, snapshots, seq: stream.seq };
      } finally { stream.stop(); window.WebSocket = original; }
    });
    assert.deepEqual(replayRetry, { retryAfter: '5', snapshots: 2, seq: 13 }, 'failed replay snapshot skipped unprocessed events');
    await replayContext.close();
    const startupContext = await browser.createBrowserContext();
    const startupPage = await startupContext.newPage();
    await startupPage.evaluateOnNewDocument(() => {
      const originalFetch = window.fetch.bind(window);
      window.fetch = (input, options) => String(input) === '/api/import-v1/status'
        ? new Promise(resolve => { window.__releaseImportCheck = () => originalFetch(input, options).then(resolve); })
        : originalFetch(input, options);
    });
    await startupPage.goto(origin);
    await startupPage.locator('#login-form [name=login]').fill('owner');
    await startupPage.locator('#login-form [name=password]').fill('test-password-123');
    await startupPage.click('#login-form button');
    await startupPage.waitForFunction(() => typeof window.__releaseImportCheck === 'function');
    // The checks while the import check is held run in one go in the page, and the check is released in the same task: the app gives startup
    // ten seconds of its own, and every round trip to a page on a loaded machine eats into them (a slow release was reported as an abort).
    // A click cannot reach the inert Settings button, which is what a person's click would hit: the element under its centre is not the button.
    const held = await startupPage.evaluate(() => {
      const toggle = document.querySelector('#settings-toggle'), box = toggle.getBoundingClientRect();
      const under = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2);
      const state = { busy: document.querySelector('#app-view').getAttribute('aria-busy') === 'true', settingsInert: toggle.inert, logoutInert: document.querySelector('#logout').inert,
        loading: !document.querySelector('#startup-status').hidden, reachable: under === toggle || toggle.contains(under), settingsOpen: document.querySelector('#settings-dialog').open };
      void window.__releaseImportCheck();
      return state;
    });
    assert.deepEqual(held, { busy: true, settingsInert: true, logoutInert: false, loading: true, reachable: false, settingsOpen: false }, 'workspace controls or loading state were wrong before the import check finished, or Settings could be reached');
    await appReady(startupPage);
    await startupPage.click('#settings-toggle');
    assert.equal(await startupPage.$eval('#settings-dialog', node => node.open), true, 'Settings did not open after the import check');
    await startupContext.close();
    const stalledContext = await browser.createBrowserContext();
    const stalledPage = await stalledContext.newPage();
    await stalledPage.evaluateOnNewDocument(() => {
      const originalFetch = window.fetch.bind(window);
      window.fetch = (input, options) => String(input) === '/api/import-v1/status'
        ? new Promise(() => {}) : originalFetch(input, options);
    });
    await stalledPage.goto(origin);
    await stalledPage.locator('#login-form [name=login]').fill('owner');
    await stalledPage.locator('#login-form [name=password]').fill('test-password-123');
    await domClick(stalledPage, '#login-form button');
    await stalledPage.waitForSelector('#app-view[aria-busy="true"]');
    await stalledPage.waitForFunction(() => !document.querySelector('#startup-status').hidden && document.querySelector('#startup-status').textContent.includes("Some checks didn't finish") && !document.querySelector('#settings-toggle').inert, { timeout: 45000 });
    assert.equal(await stalledPage.$eval('#startup-status button', button => !button.hidden && button.textContent === 'Retry'), true, 'stalled startup did not offer Retry');
    // The v1 import is offered once, in the first-run wizard: Settings has no import tab or section, not even after a timeout.
    await domClick(stalledPage, '#settings-toggle');
    assert.equal(await stalledPage.evaluate(() => [document.querySelector('#settings-import-tab'), document.querySelector('#settings-import')].every(node => node === null)), true, 'Settings still has an import entry');
    await domClick(stalledPage, '#settings-close');
    await domClick(stalledPage, '#logout');
    await stalledPage.waitForSelector('#login-view:not([hidden])');
    await stalledContext.close();
    const slowContext = await browser.createBrowserContext();
    const slowPage = await slowContext.newPage();
    await slowPage.evaluateOnNewDocument(() => {
      const originalFetch = window.fetch.bind(window);
      let held = false;
      window.fetch = (input, options) => {
        if (!held && String(input) === '/api/events/cursor') {
          held = true;
          return new Promise(resolve => { window.__releaseSlowSnapshot = () => originalFetch(input, options).then(resolve); });
        }
        return originalFetch(input, options);
      };
    });
    await slowPage.goto(origin);
    await slowPage.locator('#login-form [name=login]').fill('owner');
    await slowPage.locator('#login-form [name=password]').fill('test-password-123');
    await domClick(slowPage, '#login-form button');
    await slowPage.waitForFunction(() => typeof window.__releaseSlowSnapshot === 'function');
    await new Promise(resolve => setTimeout(resolve, 10500));
    assert.equal(await slowPage.$eval('#app-view', node => node.getAttribute('aria-busy')), 'true', 'slow snapshot ended the startup gate before stream setup');
    await slowPage.evaluate(() => window.__releaseSlowSnapshot());
    await appReady(slowPage);
    await slowPage.waitForFunction(() => document.querySelector('#connection')?.getAttribute('aria-label') === 'Connected');
    await slowContext.close();
    const deniedContext = await browser.createBrowserContext();
    const deniedPage = await deniedContext.newPage();
    await deniedPage.evaluateOnNewDocument(() => {
      const originalFetch = window.fetch.bind(window);
      window.fetch = (input, options) => String(input) === '/api/events/cursor'
        ? Promise.resolve(new Response(JSON.stringify({ message: 'Session expired' }), { status: 401, headers: { 'Content-Type': 'application/json' } }))
        : originalFetch(input, options);
    });
    await deniedPage.goto(origin);
    await deniedPage.locator('#login-form [name=login]').fill('owner');
    await deniedPage.locator('#login-form [name=password]').fill('test-password-123');
    await deniedPage.click('#login-form button');
    await deniedPage.waitForFunction(() => !document.querySelector('#login-view').hidden && document.querySelector('#app-view').hidden);
    assert.equal(await deniedPage.$eval('#startup-status button', button => button.hidden), true, 'expired session showed Retry instead of sign-in');
    await deniedContext.close();
    const preStreamContext = await browser.createBrowserContext();
    const preStreamPage = await preStreamContext.newPage();
    await preStreamPage.evaluateOnNewDocument(() => {
      const originalFetch = window.fetch.bind(window);
      const originalTimeout = window.setTimeout.bind(window);
      window.setTimeout = (callback, delay, ...args) => originalTimeout(callback, delay === 30000 ? 150 : delay, ...args);
      window.fetch = (input, options) => String(input) === '/api/events/cursor'
        ? new Promise(resolve => { window.__releaseStuckSnapshot = () => originalFetch(input, options).then(resolve); })
        : originalFetch(input, options);
    });
    await preStreamPage.goto(origin);
    await preStreamPage.locator('#login-form [name=login]').fill('owner');
    await preStreamPage.locator('#login-form [name=password]').fill('test-password-123');
    await domClick(preStreamPage, '#login-form button');
    await preStreamPage.waitForFunction(() => typeof window.__releaseStuckSnapshot === 'function');
    await preStreamPage.waitForFunction(() => !document.querySelector('#startup-status').hidden && document.querySelector('#startup-status button').textContent === 'Retry' && !document.querySelector('#startup-status button').hidden);
    assert.equal(await preStreamPage.$eval('#app-view', node => node.getAttribute('aria-busy')), null, 'stuck pre-stream snapshot kept workspace busy');
    await preStreamPage.evaluate(() => window.__releaseStuckSnapshot());
    await preStreamPage.waitForFunction(() => document.querySelector('#connection')?.getAttribute('aria-label') === 'Connected' && document.querySelector('#startup-status').hidden);
    assert.equal(await preStreamPage.$eval('#app-view', node => node.getAttribute('aria-busy')), null, 'late pre-stream snapshot left workspace busy');
    await preStreamContext.close();
    const failedPreStreamContext = await browser.createBrowserContext();
    const failedPreStreamPage = await failedPreStreamContext.newPage();
    await failedPreStreamPage.evaluateOnNewDocument(() => {
      const originalFetch = window.fetch.bind(window);
      const originalTimeout = window.setTimeout.bind(window);
      window.setTimeout = (callback, delay, ...args) => originalTimeout(callback, delay === 30000 ? 150 : delay, ...args);
      window.fetch = (input, options) => String(input) === '/api/events/cursor'
        ? new Promise((resolve, reject) => { window.__failStuckSnapshot = () => reject(new Error('held snapshot failed')); })
        : originalFetch(input, options);
    });
    await failedPreStreamPage.goto(origin);
    await failedPreStreamPage.locator('#login-form [name=login]').fill('owner');
    await failedPreStreamPage.locator('#login-form [name=password]').fill('test-password-123');
    await domClick(failedPreStreamPage, '#login-form button');
    await failedPreStreamPage.waitForFunction(() => typeof window.__failStuckSnapshot === 'function');
    await failedPreStreamPage.waitForFunction(() => !document.querySelector('#startup-status button').hidden);
    await failedPreStreamPage.evaluate(async () => { window.__failStuckSnapshot(); await new Promise(resolve => setTimeout(resolve, 0)); });
    assert.equal(await failedPreStreamPage.$eval('#startup-status button', button => !button.hidden), true, 'failed late snapshot hid Retry');
    assert.equal(await failedPreStreamPage.$eval('#app-view', node => node.getAttribute('aria-busy')), null, 'failed late snapshot kept workspace busy');
    await failedPreStreamContext.close();
    const lateContext = await browser.createBrowserContext();
    const latePage = await lateContext.newPage();
    await latePage.evaluateOnNewDocument(() => {
      const originalFetch = window.fetch.bind(window);
      let firstStatus = true;
      window.fetch = (input, options) => {
        if (firstStatus && String(input) === '/api/import-v1/status') {
          firstStatus = false;
          return Promise.resolve(new Response(JSON.stringify({ state: 'ready' }), { headers: { 'Content-Type': 'application/json' } }));
        }
        if (String(input) === '/api/import-v1/preview') return new Promise(resolve => { window.__releaseLatePreview = () => resolve(new Response(JSON.stringify({ suggested: {} }), { headers: { 'Content-Type': 'application/json' } })); });
        return originalFetch(input, options);
      };
    });
    await latePage.goto(origin);
    await latePage.locator('#login-form [name=login]').fill('owner');
    await latePage.locator('#login-form [name=password]').fill('test-password-123');
    await domClick(latePage, '#login-form button');
    await latePage.waitForFunction(() => typeof window.__releaseLatePreview === 'function');
    await domClick(latePage, '#logout');
    await latePage.waitForSelector('#login-view:not([hidden])');
    await latePage.evaluate(async () => { window.__releaseLatePreview(); await new Promise(resolve => setTimeout(resolve, 0)); });
    assert.equal(await latePage.$eval('.import-v1-dialog', dialog => dialog.open), false, 'late preview opened wizard after sign-out');
    await latePage.locator('#login-form [name=login]').fill('owner');
    await latePage.locator('#login-form [name=password]').fill('test-password-123');
    await latePage.click('#login-form button');
    await appReady(latePage);
    assert.equal(await latePage.$eval('.import-v1-dialog', dialog => dialog.open), false, 'old wizard crossed into a new sign-in');
    await lateContext.close();
    // A usages request that fails with 403 after the reader picked another Settings tab must not switch them back.
    const lateForbiddenContext = await browser.createBrowserContext();
    const lateForbiddenPage = await lateForbiddenContext.newPage();
    await lateForbiddenPage.evaluateOnNewDocument(() => {
      const originalFetch = window.fetch.bind(window);
      window.fetch = async (input, options) => {
        if (String(input) === '/api/usages') { await new Promise(resolve => setTimeout(resolve, 1200)); return new Response(JSON.stringify({ message: 'forbidden' }), { status: 403, headers: { 'Content-Type': 'application/json' } }); }
        return originalFetch(input, options);
      };
    });
    await login(lateForbiddenPage);
    await domClick(lateForbiddenPage, '#settings-toggle');
    await lateForbiddenPage.waitForFunction(() => document.querySelector('#settings-dialog')?.open);
    await lateForbiddenPage.click('#settings-roles-tab');
    await lateForbiddenPage.waitForFunction(() => document.querySelector('#usages-toggle')?.hidden === true, { timeout: 10000 });
    assert.deepEqual(await lateForbiddenPage.evaluate(() => ({ roles: !document.querySelector('#settings-roles').hidden, general: !document.querySelector('#settings-general').hidden })), { roles: true, general: false }, 'a late 403 for usages switched the reader away from the tab they chose');
    await lateForbiddenContext.close();
    // A finished (or skipped) import is never offered again: with status `done` the wizard stays closed, and no
    // Settings control or API call can bring it back.
    const closedContext = await browser.createBrowserContext();
    const closedPage = await closedContext.newPage();
    await closedPage.evaluateOnNewDocument(() => {
      const originalFetch = window.fetch.bind(window);
      window.__importCalls = [];
      window.fetch = (input, options) => {
        if (String(input).startsWith('/api/import-v1/')) window.__importCalls.push(String(input));
        if (String(input) === '/api/import-v1/status') return Promise.resolve(new Response(JSON.stringify({ state: 'done' }), { headers: { 'Content-Type': 'application/json' } }));
        return originalFetch(input, options);
      };
    });
    await login(closedPage);
    await closedPage.click('#settings-toggle');
    assert.deepEqual(await closedPage.evaluate(() => ({ dialog: document.querySelector('.import-v1-dialog')?.open ?? false, tab: Boolean(document.querySelector('#settings-import-tab')), section: Boolean(document.querySelector('#settings-import')), calls: window.__importCalls })),
      { dialog: false, tab: false, section: false, calls: ['/api/import-v1/status'] }, 'a closed import was offered again or reachable from Settings');
    await closedContext.close();
    console.log('UI smoke part startup passed');
  } finally {
    await ctx.close();
  }
}
main().catch(error => { console.error(error); process.exitCode = 1; });

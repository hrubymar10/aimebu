// The set-up the UI smoke (scripts/test-ui.cjs) and its parts (scripts/test-ui-*.cjs) share: the Go binaries, an isolated home and a scratch git
// project, a server and its supervisor, a Chromium with two signed-out browser contexts (`first`, `second`) and the helpers that drive them. A script
// calls start() and gets everything it needs in one object, and close() when it is done. Each process has its own ports and its own temporary directory,
// so any number of parts can run side by side.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const { spawn, spawnSync } = require('node:child_process');
const { QuotaMock } = require('./quota-mock.cjs');
const { LimitGuardMock } = require('./limit-guard-mock.cjs');
const { chromiumPath } = require('./chromium-path.cjs');
const { freePort, startServer } = require('./server-start.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const tempRoot = fs.realpathSync(os.tmpdir());
process.env.TMPDIR = tempRoot;
const tmp = fs.mkdtempSync(path.join(tempRoot, 'aimebu-ui-'));
assert.equal(fs.realpathSync(tmp), tmp, 'the UI smoke root is canonical');
const gitRoot = path.join(tmp, 'project-repo');
fs.mkdirSync(gitRoot);
for (const args of [['init', '-q'], ['config', 'user.name', 'UI Test'], ['config', 'user.email', 'ui@example.invalid']]) {
  const result = spawnSync('git', args, { cwd: gitRoot }); if (result.status !== 0) throw new Error(`git ${args[0]} failed`);
}
fs.writeFileSync(path.join(gitRoot, 'README.md'), 'UI worktree fixture\n');
for (const args of [['add', 'README.md'], ['commit', '-qm', 'base']]) {
  const result = spawnSync('git', args, { cwd: gitRoot }); if (result.status !== 0) throw new Error(`git ${args[0]} failed`);
}
const testHome = path.join(tmp, 'home');
fs.mkdirSync(testHome, { recursive: true });
const isolatedEnv = { ...process.env, HOME: testHome, CODEX_HOME: path.join(testHome, '.codex'), CLAUDE_CONFIG_DIR: path.join(testHome, '.claude'), AIMEBU_CONFIG_DIR: path.join(tmp, 'v1'), AIMEBU2_CONFIG_DIR: tmp, AIMEBU2_SENTRY_DSN: 'off' };
const binary = path.join(tmp, 'aimebu');
const build = spawnSync('go', ['build', '-o', binary, './cmd/aimebu'], { stdio: 'inherit' });
if (build.status !== 0) process.exit(build.status || 1);
const seedBinary = path.join(tmp, 'seed-inbox');
const seedBuild = spawnSync('go', ['build', '-o', seedBinary, './scripts/seed-inbox'], { stdio: 'inherit' });
if (seedBuild.status !== 0) process.exit(seedBuild.status || 1);
// The app leaves its loading state when startup finishes or hits its own 30 s deadline, so a 30 s wait here races
// that deadline under load. Wait on the state itself with room to spare: a stuck startup still fails, a slow one passes.
const APP_READY_TIMEOUT = 90000;
// A project starts folded, so the page is handed over with its projects open: most checks reach rooms inside them. Where the default is the point
// the page is waited for with appReadyFolded.
const appReadyFolded = page => page.waitForSelector('#app-view:not([hidden]):not([aria-busy])', { timeout: APP_READY_TIMEOUT });
// With no project yet the first-run card (a modal over the whole viewport) opens once the room list is known; it is skipped here so the toolbar
// behind it can be reached. The tests of the card itself (the setup flow) do not use it: they wait for the card.
const appReady = async page => {
  await appReadyFolded(page);
  await page.waitForFunction(() => !document.querySelector('#onboarding-view').hidden || document.querySelector('#projects .room-group'), { timeout: 8000 }).catch(() => {}); // the list is drawn after the page is shown; a page with no project has none
  if (await page.$('#onboarding-view:not([hidden])')) { await page.click('#onboarding-view .onboarding-skip'); await page.waitForSelector('#onboarding-view', { hidden: true }); }
  await page.evaluate(() => { for (const toggle of document.querySelectorAll('.room-group.collapsed > .room-item > .room-collapse')) toggle.click(); });
};
// waitForFunction that names what the page looked like when it timed out: a 30 s timeout alone cannot tell a slow
// transition from a lost one. `dump` runs in the page and its result is appended to the error.
async function waitDump(page, label, predicate, dump, ...args) {
  try { await page.waitForFunction(predicate, {}, ...args); } catch (error) {
    const state = await page.evaluate(dump, ...args).catch(failure => `dump failed: ${failure.message}`);
    error.message += `\n${label} page state at timeout: ${JSON.stringify(state)}`;
    throw error;
  }
}
// Waits for a room row to be listed after a sign-in. On timeout it names what the page showed (rows, connection, hash, the
// project list), so a stale room list can be told apart from a slow load.
const waitRoomRow = (page, label, roomID) => waitDump(page, label, id => !!document.querySelector(`[data-room-id="${id}"] .room-item-name`), id => ({
  hash: location.hash, appBusy: document.querySelector('#app-view')?.getAttribute('aria-busy'), connection: document.querySelector('#connection')?.getAttribute('aria-label'),
  rows: [...document.querySelectorAll('[data-room-id]')].map(row => `${row.dataset.roomId}:${(row.querySelector('.room-item-name')?.textContent || '').slice(0, 20)}`),
  projects: document.querySelector('#projects')?.innerText.replace(/\s+/g, ' ').slice(0, 300), wanted: id,
}), roomID);
const installFixtureChat = page => page.evaluate(async () => {
  const [{ Chat }, { ChatStore }, { mountPreactPage }, { MessageList, ThreadList }, { html }] = await Promise.all([import('/js/chat.js'), import('/js/pages/chat/store.js'), import('/js/pages/mount-page.js'), import('/js/pages/chat/rows.js'), import('/js/ui.js')]);
  // A chat built without its constructor, with the keyed lists mounted on the containers it is given.
  window.__fixtureChat = (list, threadList) => {
    const chat = Object.create(Chat.prototype); chat.store = new ChatStore(chat);
    chat.list = list; chat.nodes = new Map(); chat.agents = new Map(); chat.raw = false; chat.messages = new Map(); chat.threadCounts = new Map(); chat.serverThreadCounts = new Map(); chat.answerItems = new Map();
    chat.rows = []; chat.quotes = new Map(); chat.rowBatching = false; chat.rowPending = []; chat.expiry = { expired: new Set(), divider: null };
    chat.listView = mountPreactPage(list, props => html`<${MessageList} ...${props} />`);
    chat.threadEntries = []; chat.threadEntryIDs = new Set(); chat.threadBatching = false;
    if (threadList) { chat.threadList = threadList; chat.threadView = mountPreactPage(threadList, props => html`<${ThreadList} ...${props} />`); }
    chat.refreshReactions = () => {};
    chat.treeNewIDs = new Set(); chat.threadTreeButton = { set() {} }; // the marker of a reply inside the tree, which a fixture has no panel for
    return chat;
  };
});


async function start() {
  // A start whose port was taken between picking it and binding it is tried again on another (see server-start.cjs).
  const { child, port, origin, logs: serverLogs } = await startServer(binary, port => ({ ...isolatedEnv, AIMEBU2_USAGES: '0', AIMEBU2_SENTRY_DSN: 'off', AIMEBU2_EMBEDDED_SUPERVISOR: '0', AIMEBU2_WEB_BUNDLE: 'off', AIMEBU2_PORT: String(port), AIMEBU2_OWNER_LOGIN: 'owner', AIMEBU2_OWNER_PASSWORD: 'test-password-123' }), { label: 'smoke server' });
  let browser, supervisor, preserveFailure = false, browserClosing = false;
  const close = async failed => {
    browserClosing = true;
    await browser?.close();
    for (const proc of [supervisor, child]) { if (proc && proc.exitCode === null && proc.signalCode === null) { proc.kill('SIGTERM'); await new Promise(resolve => proc.once('exit', resolve)); } }
    if (!preserveFailure) fs.rmSync(tmp, { recursive: true, force: true });
  };
  try {
    supervisor = spawn(binary, ['supervisor'], { env: { ...isolatedEnv, AIMEBU2_URL: origin }, stdio: ['ignore', 'pipe', 'pipe'] });
    let supervisorLogs = ''; supervisor.stderr.on('data', data => supervisorLogs += data);
    browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
    // A page wait that dies with "frame got detached" and no step in its trace means Chromium itself went away; say so.
    browser.process()?.once('exit', (code, signal) => { if (!browserClosing) console.error(`Chromium exited before the smoke finished (code ${code}, signal ${signal}); a pending wait fails as "frame got detached"`); });
    // The smoke exercises threads, copy and links through the compact toolbar (style C); style A, the product default, has
    // its own steps. Every page starts with that choice stored, before any navigation.
    const prepare = async page => { await page.evaluateOnNewDocument(() => { window.__openPageSize = 100; // the smoke's rooms were written for a first page of 100 messages (test-chat-page checks the default of 25)
      try { if (!localStorage.getItem('aimebu2.ui.variant')) localStorage.setItem('aimebu2.ui.variant', 'c'); } catch {}
      // Browser notifications are not asked for: every smoke page reports the permission as refused, so the bar that offers them never shows
      // over unrelated flows. The page that tests the bar replaces this with a permission that has not been asked yet.
      try { if ('Notification' in window) Object.defineProperty(Notification, 'permission', { configurable: true, get: () => 'denied' }); } catch {}
      // Most of the smoke drives the composer's textarea directly, so it starts with formatting off; the composer block turns it on (and checks the default by clearing this, which a page marks with a session flag).
      try { if (!sessionStorage.getItem('smoke-default-formatting') && !localStorage.getItem('aimebu2.composer.formatting')) localStorage.setItem('aimebu2.composer.formatting', 'off'); } catch {} }); return page; };
    const createBrowserContext = browser.createBrowserContext.bind(browser);
    browser.createBrowserContext = async (...args) => { const context = await createBrowserContext(...args); const newPage = context.newPage.bind(context); context.newPage = async (...pageArgs) => prepare(await newPage(...pageArgs)); return context; };
    // Puppeteer's 30 s default is tight when other runs share the machine; a real failure still fails, just later.
    browser.on('targetcreated', async target => { try { (await target.page())?.setDefaultTimeout(60000); } catch {} });
    const a = await browser.createBrowserContext(), b = await browser.createBrowserContext();
    const first = await a.newPage(), second = await b.newPage();
    // Room settings and the agent card move while their content loads, so a pointer click can land where the button was a
    // moment ago. These buttons are clicked through the DOM once they are shown and enabled.
    // Every click on a control that can re-layout under the pointer goes through domClick. The raw page.click calls that remain are on
    // purpose: the composer, its toolbar, inputs and the pickers (focus and mousedown matter there), the experiment pill, the edge toggles
    // and the room menu (hit-test and covering checks), clicks that focus a message, and steps that check a control that is inert,
    // disabled or hidden does nothing (a DOM click would fire where a pointer cannot).
    // A click that cannot be lost: wait until the control is shown, enabled and its box has not moved for two frames (an agent
    // card re-renders or expands under the pointer, and a pointer click then lands on the old position), then click it in the DOM.
    const domClick = async (page, selector) => {
      // A message toolbar is made when its row is reached: a click on one of its buttons reaches the row first, as the pointer would.
      const rowOfControl = /^(.*?)\s*\.(?:message-actions|message-react|message-reply|message-find|message-copy|thread-action|quick-reaction|message-reaction-row)\b/.exec(selector)?.[1];
      if (rowOfControl) await reachRow(page, rowOfControl);
      // A project starts folded: a click on a room inside a folded project opens the project first, as a person would.
      if (selector.includes('data-room-id')) await page.evaluate(selector => { for (let group = document.querySelector(selector)?.closest('.room-group'); group; group = group.parentElement?.closest('.room-group')) if (group.classList.contains('collapsed')) group.querySelector(':scope > .room-item > .room-collapse')?.click(); }, selector);
      await page.waitForFunction(selector => new Promise(resolve => {
        const button = document.querySelector(selector);
        if (!button || button.disabled || button.hidden || !button.getClientRects().length) { resolve(false); return; }
        const box = () => { const rect = button.getBoundingClientRect(); return [rect.x, rect.y, rect.width, rect.height].join(); };
        const before = box();
        requestAnimationFrame(() => requestAnimationFrame(() => resolve(button.isConnected && box() === before && !button.disabled)));
      }), {}, selector);
      await page.$eval(selector, button => button.click());
    };
    // A row's toolbar is made when the row is reached (hovered, focused or touched). `reach` does what the pointer does, for a row named by a selector.
    const reachRow = (page, row) => page.evaluate(row => { const node = document.querySelector(row); (node?.querySelector('.chat-msg-bubble') || node)?.dispatchEvent(new PointerEvent('pointerover', { bubbles: true })); }, row);
    // A project's actions are the items of the menu behind the ☰ button of its home row: reach the button by the keyboard focus that shows it, open the
    // menu, press the item. `row` selects the home room's group.
    const projectMenuAct = async (page, row, item) => {
      await page.focus(`${row} > .room-item .room-item-name`);
      await domClick(page, `${row} > .room-item .project-menu-btn`);
      await domClick(page, `${row} > .room-item .project-menu-${item}`);
    };
    // The owner's rule: a control is an icon with a tooltip and an accessible name, never text. Returns what breaks it for the
    // elements matching `selectors` (a list), and how many matched.
    const iconOnlyReport = (page, selectors) => page.evaluate(selectors => {
      const bad = []; let found = 0;
      for (const element of document.querySelectorAll(selectors.join(','))) {
        found++;
        const text = [...element.childNodes].filter(node => node.nodeType === Node.TEXT_NODE && node.textContent.trim()).map(node => node.textContent.trim()).join('');
        const label = element.getAttribute('aria-label'), title = element.title;
        if (!element.querySelector('.icon-mask, svg, img') || (text && !element.matches('.hidden-room-show')) || !label || !title) bad.push(`${element.id || element.className}: glyph=${!!element.querySelector('.icon-mask, svg, img')} text=${JSON.stringify(text)} label=${JSON.stringify(label)} title=${JSON.stringify(title)}`);
      }
      return { found, bad };
    }, selectors);
    const disableRandomUUID = () => Object.defineProperty(Crypto.prototype, 'randomUUID', { configurable: true, value: undefined });
    await first.evaluateOnNewDocument(disableRandomUUID);
    await first.setViewport({ width: 1100, height: 720 }); await second.setViewport({ width: 1100, height: 720 });
    let jumpSeq = '';
    // The page's questions, confirmations and notices are in-app dialogs (web/js/dialogs.js); a flow that is not about the dialog itself has
    // them answered here, the way the browser's native ones used to be: `answer(text)` gets the dialog's title, message and field labels and
    // returns { accept, values } (values by field name). The dialog's own behaviour has its own checks further down.
    async function answerDialogs(page, answer) {
      page.__dialogAnswer = answer;
      if (page.__dialogInstalled) return;
      page.__dialogInstalled = true;
      await page.exposeFunction('__answerDialog', text => page.__dialogAnswer(text));
      const install = () => {
        if (window.__dialogWatch) return; window.__dialogWatch = true;
        new MutationObserver(() => {
          const dialog = document.querySelector('#app-dialog');
          if (!dialog || !dialog.open) { if (dialog) delete dialog.dataset.answering; return; }
          if (dialog.dataset.answering) return;
          dialog.dataset.answering = '1';
          const text = [dialog.querySelector('h2').textContent, dialog.querySelector('.app-dialog-message').textContent, ...[...dialog.querySelectorAll('.app-dialog-fields label')].map(label => label.textContent)].join(' | ');
          window.__answerDialog(text).then(({ accept, values = {} }) => {
            if (accept) for (const input of dialog.querySelectorAll('.app-dialog-fields input')) {
              if (input.name in values) { if (input.type === 'checkbox') input.checked = !!values[input.name]; else input.value = values[input.name]; input.dispatchEvent(new Event('input', { bubbles: true })); }
            }
            dialog.querySelector(accept ? '.app-dialog-ok' : '.app-dialog-cancel').click();
          });
        }).observe(document, { subtree: true, attributes: true, attributeFilter: ['open'], childList: true });
      };
      await page.evaluateOnNewDocument(install);
      await page.evaluate(install).catch(() => {});
    }
    await answerDialogs(first, text => (text.includes('New task room under') ? { accept: true, values: { title: 'Nested task' } } : text.includes('Go to message') ? { accept: true, values: { value: jumpSeq } } : text.includes('Base branch or ref') ? { accept: true, values: { value: '' } } : { accept: true, values: { title: 'Nested task', value: 'Nested task' } }));
    async function login(page, url = origin) { await page.goto(url); await page.locator('#login-form [name=login]').fill('owner'); await page.locator('#login-form [name=password]').fill('test-password-123'); await page.locator('#login-form button').click();
      // A database nobody has signed in to yet shows the owner the welcome first (the servers other than the main one start that way): skip it.
      await page.waitForSelector('#welcome-view, #app-view:not([hidden]):not([aria-busy])', { timeout: APP_READY_TIMEOUT });
      if (await page.$('#welcome-view')) { await page.$eval('#welcome-view', view => view.click()); await page.waitForSelector('#welcome-continue:not([hidden])'); await page.click('#welcome-continue'); }
      await appReady(page); await page.waitForFunction(() => document.querySelector('#connection')?.getAttribute('aria-label') === 'Connected');
      }
    // puppeteer has no check()/uncheck(); toggle through the element's own click so change handlers run.
    async function setChecked(page, selector, checked) { await page.$eval(selector, (el, want) => { if (el.checked !== want) el.click(); }, checked); }
    // The Settings nav in two groups: SETTINGS (General ... Macros), then a gap and AGENTS (Machines, Harnesses). `items` lists the shown
    // entries in DOM order (a caption as its upper-case text, a tab as its section); `gap` is the space between the last tab above the AGENTS
    // caption and the first one below it, `tabGap` the space between two neighbouring tabs. The AGENTS caption is styled like the SETTINGS
    // one, and neither caption can take focus.
    const settingsNav = page => page.evaluate(() => {
      const nav = document.querySelector('#settings-dialog nav'), [settings, agents] = nav.querySelectorAll('.settings-nav-title');
      const items = [...nav.children].filter(el => !el.hidden).map(el => el.classList.contains('settings-nav-head') ? el.querySelector('.settings-nav-title').textContent.toUpperCase() : el.classList.contains('settings-nav-title') ? el.textContent.toUpperCase() : el.id.replace(/^settings-|-tab$/g, ''));
      const style = el => { const s = getComputedStyle(el); return [s.color, s.fontSize, s.fontWeight, s.letterSpacing, s.textTransform, s.paddingTop, s.paddingRight, s.paddingBottom, s.paddingLeft, s.display]; };
      const shownSibling = (el, step) => { do el = el[step]; while (el && el.hidden); return el; };
      const above = shownSibling(agents, 'previousElementSibling'), below = shownSibling(agents, 'nextElementSibling');
      const gap = agents.hidden || !above || !below ? null : Math.round(below.getBoundingClientRect().top - above.getBoundingClientRect().bottom);
      const general = document.querySelector('#settings-general-tab'), usages = document.querySelector('#settings-usages-tab');
      return { items, agentsHidden: agents.hidden, sameStyle: JSON.stringify(style(settings)) === JSON.stringify(style(agents)), gap, tabGap: Math.round(usages.getBoundingClientRect().top - general.getBoundingClientRect().bottom),
        focusable: [settings, agents].some(el => el.tabIndex >= 0 || el.getAttribute('aria-hidden') !== 'true') };
    });
    async function openSettings(page) {
      await domClick(page, '#settings-toggle');
      const state = await page.evaluate(() => ({
        open: document.querySelector('#settings-dialog').open,
        dialogs: [...document.querySelectorAll('dialog[open]')].map(dialog => dialog.id),
      }));
      assert.equal(state.open, true, `Settings toggle did not open dialog; open dialogs: ${state.dialogs.join(', ') || 'none'}`);
      assert.equal(await page.$eval('#settings-general', el => !el.hidden), true, 'Settings opens on General');
      // v1's layout: a SETTINGS caption over plain nav items, the section name as the content title, a quiet close button.
      assert.deepEqual(await page.evaluate(async () => { await new Promise(resolve => setTimeout(resolve, 400)); const nav = document.querySelector('#settings-dialog nav'), caption = nav.querySelector('.settings-nav-title'), selected = nav.querySelector('.active'), close = document.querySelector('#settings-close'), box = document.querySelector('#settings-dialog').getBoundingClientRect();
        return { caption: caption.textContent, captionFirst: nav.firstElementChild.contains(caption), title: document.querySelector('#settings-title').textContent, selected: selected.textContent, closeGlyph: close.querySelector('.icon-mask-close') ? 'icon' : close.textContent, closeQuiet: getComputedStyle(close).borderTopWidth === '0px', navWidth: Math.round(nav.getBoundingClientRect().width), fullHeight: Math.abs(box.height - innerHeight) < 1, selectedTinted: (() => { const probe = document.createElement('i'); probe.style.background = 'var(--accent-dim)'; document.body.append(probe); const tint = getComputedStyle(probe).backgroundColor; probe.remove(); const highContrast = document.documentElement.dataset.theme === 'high-contrast-dark'; return getComputedStyle(selected).backgroundColor === (highContrast ? 'rgba(0, 0, 0, 0)' : tint); })() }; }),
        { caption: 'Settings', captionFirst: true, title: 'General', selected: 'General', closeGlyph: 'icon', closeQuiet: true, navWidth: 180, fullHeight: true, selectedTinted: true }, 'the Settings dialog follows v1: the selected tab is tinted with --accent-dim (outlined in high contrast)');
      const nav = await settingsNav(page);
      if (nav.items.includes('AGENTS')) { // the owner and admins: both groups, the AGENTS caption a group gap below Macros
        assert.deepEqual(nav.items.filter(item => item !== 'usages' && item !== 'users'), ['SETTINGS', 'general', 'notifications', 'telemetry', 'roles', 'prompts', 'macros', 'AGENTS', 'machines', 'harnesses'], 'Settings nav groups');
        assert.ok(nav.sameStyle && !nav.focusable && nav.gap >= nav.tabGap + 14, `the AGENTS caption looks like SETTINGS, takes no focus and opens a gap: ${JSON.stringify(nav)}`);
      }
    }
    // Closes the room settings if they are open (a deleted project's home room takes its settings with it).
    const closeRoomInfo = page => page.evaluate(() => { const dialog = document.querySelector('#room-info-dialog'); if (dialog.open) dialog.querySelector('#room-info-close').click(); });
    // The project's own settings are the home room's (Room settings → Project; there is no Settings → Project page). Selects a project's home
    // room (archived projects open too) and opens its settings, closing Settings first if it is open.
    async function openProjectSettings(page, projectID) {
      if (await page.evaluate(() => document.querySelector('#settings-dialog').open)) await domClick(page, '#settings-close');
      if (await page.evaluate(() => document.querySelector('#room-info-dialog').open)) await domClick(page, '#room-info-close');
      const home = await page.evaluate(async id => (await (await fetch(`/api/projects/${id}/rooms`)).json()).find(room => room.kind === 'home').id, projectID);
      await page.evaluate(() => document.querySelectorAll('.archived-projects, .archived-project').forEach(details => { details.open = true; }));
      await domClick(page, `[data-room-id="${home}"] .room-item-name`);
      await page.waitForFunction(id => decodeURIComponent(location.hash.slice(1)) === id, {}, home);
      await domClick(page, '#room-info');
      await page.waitForSelector('#room-info-dialog[open] #room-project:not([hidden]) #project-details');
      return home;
    }
    // Back to a Settings tab from a home room's settings (or open Settings on it): leaves the room settings first.
    async function settingsTab(page, tab) {
      if (await page.evaluate(() => document.querySelector('#room-info-dialog').open)) await domClick(page, '#room-info-close');
      if (!await page.evaluate(() => document.querySelector('#settings-dialog').open)) await domClick(page, '#settings-toggle');
      await domClick(page, tab);
    }
    // The owner's first sign-in on a new install shows the welcome before the app: the art, then the name, then Continue, in bands
    // from top to bottom. The rest of the smoke signs in many times; the welcome is gone for good once Continue is pressed.
    const welcomeState = () => first.evaluate(() => {
      const box = selector => { const r = document.querySelector(selector).getBoundingClientRect(); return { top: r.top, bottom: r.bottom, height: r.height }; };
      const style = selector => getComputedStyle(document.querySelector(selector));
      return { stage: document.querySelector('#welcome-view').dataset.stage, logo: box('.welcome-logo'), title: box('.welcome-title'), button: box('#welcome-continue'), view: box('#welcome-view'), text: document.querySelector('.welcome-title').textContent,
        focused: document.activeElement?.id, logoOpacity: style('.welcome-logo').opacity, versionOpacity: style('.welcome-version').opacity, buttonOpacity: style('#welcome-continue').opacity, inert: document.querySelector('#welcome-continue').inert };
    });
    // The parts fade in over a fraction of a second once the screen is finished; measure after that.
    const settled = () => first.waitForFunction(() => ['.welcome-name', '.welcome-version', '.welcome-logo', '#welcome-continue'].every(selector => getComputedStyle(document.querySelector(selector)).opacity === '1'), { timeout: 10000 });
    // The logo is the robot app icon on its rounded tile (never the bare mark), in the artwork that matches the theme: exactly one of the
    // light and the dark image shows, it is the 512px icon, and it has loaded.
    const iconState = () => first.evaluate(() => { const shown = [...document.querySelectorAll('.welcome-logo .welcome-icon')].filter(image => getComputedStyle(image).display !== 'none'); const light = /light/.test(document.documentElement.dataset.theme || ''); return { shown: shown.map(image => new URL(image.src).pathname), loaded: shown.every(image => image.complete && image.naturalWidth === 512), light }; });
    const assertIcon = (state, label) => assert.deepEqual([state.shown, state.loaded], [[state.light ? '/icons/icon-512.png' : '/icons/icon-512-dark.png'], true], `${label}: the logo is the tiled robot icon for this theme: ${JSON.stringify(state)}`);
    const assertFinished = (state, label) => {
      assert.ok(state.logo.top < state.title.top && state.title.top < state.button.top && state.logo.bottom <= state.title.top + 1, `${label}: logo, name and Continue sit in bands from top to bottom: ${JSON.stringify(state)}`);
      assert.ok(state.logo.height > 100 && state.logo.top >= state.view.top && state.logo.bottom <= state.view.top + state.view.height / 2 + 1, `${label}: the logo is full size within the top half: ${JSON.stringify(state)}`);
      assert.deepEqual([state.stage, state.text, state.focused, state.logoOpacity, state.versionOpacity, state.buttonOpacity, state.inert], ['done', 'aimebu v2', 'welcome-continue', '1', '1', '1', false], `${label}: the finished welcome is the logo, "aimebu v2" and a focused Continue`);
    };
    // The owner of the main server has skipped the offer to set up a harness before anything else: the smoke sets one up itself, later, through
    // the UI, and the offer would otherwise cover the page of every flow that signs in while there is none. The offer has its own checks (the
    // first-run section). The skip is the account's, a preference on the server, so every page of this owner starts as if it was dealt with.
    const skipHarnessOffer = async (base, loginName = 'owner', password = 'test-password-123') => {
      const context = await browser.createBrowserContext(); const page = await context.newPage();
      try {
        await page.goto(base);
        assert.deepEqual(await page.evaluate(async (login, secret) => {
          const signedIn = await fetch('/api/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ login, password: secret }) });
          const { csrf_token } = await signedIn.json();
          const saved = await fetch('/api/me/preferences', { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ preferences: { harness_setup_skipped: '1' } }) });
          return [signedIn.status, saved.status];
        }, loginName, password), [200, 200], 'the owner could not skip the harness offer');
      } finally { await context.close(); }
    };
    await skipHarnessOffer(origin);
  return { tempRoot, tmp, gitRoot, testHome, isolatedEnv, binary, seedBinary, APP_READY_TIMEOUT, appReadyFolded, appReady, freePort, waitDump, waitRoomRow, installFixtureChat, child, browser, supervisor, createBrowserContext, a, b, first, second, origin, port, domClick, reachRow, projectMenuAct, iconOnlyReport, disableRandomUUID, answerDialogs, login, setChecked, settingsNav, openSettings, closeRoomInfo, openProjectSettings, settingsTab, welcomeState, settled, iconState, assertIcon, assertFinished, skipHarnessOffer,
      logs: serverLogs, supervisorLogs: () => supervisorLogs, preserve: () => { preserveFailure = true; }, setJumpSeq: value => { jumpSeq = value; }, jumpSeq: () => jumpSeq, close, QuotaMock, LimitGuardMock };
  } catch (error) { await close(); throw error; }
}

module.exports = { start, isolatedEnv, binary, tmp, gitRoot };

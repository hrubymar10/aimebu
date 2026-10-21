const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const { spawn, spawnSync } = require('node:child_process');
const { QuotaMock } = require('./quota-mock.cjs');
const { LimitGuardMock } = require('./limit-guard-mock.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'aimebu-ui-'));
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
const isolatedEnv = { ...process.env, HOME: testHome, CODEX_HOME: path.join(testHome, '.codex'), CLAUDE_CONFIG_DIR: path.join(testHome, '.claude'), AIMEBU2_CONFIG_DIR: tmp };
const binary = path.join(tmp, 'aimebu');
const build = spawnSync('go', ['build', '-o', binary, './cmd/aimebu'], { stdio: 'inherit' });
if (build.status !== 0) process.exit(build.status || 1);
const seedBinary = path.join(tmp, 'seed-inbox');
const seedBuild = spawnSync('go', ['build', '-o', seedBinary, './scripts/seed-inbox'], { stdio: 'inherit' });
if (seedBuild.status !== 0) process.exit(seedBuild.status || 1);
// The app leaves its loading state when startup finishes or hits its own 30 s deadline, so a 30 s wait here races
// that deadline under load. Wait on the state itself with room to spare: a stuck startup still fails, a slow one passes.
const APP_READY_TIMEOUT = 90000;
const appReady = page => page.waitForSelector('#app-view:not([hidden]):not([aria-busy])', { timeout: APP_READY_TIMEOUT });
function freePort() { return new Promise((resolve, reject) => { const server = net.createServer(); server.once('error', reject); server.listen(0, '127.0.0.1', () => { const port = server.address().port; server.close(() => resolve(port)); }); }); }
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
const waitRoomRow = (page, label, roomID) => waitDump(page, label, id => !!document.querySelector(`[data-room-id="${id}"] .room-name`), id => ({
  hash: location.hash, appBusy: document.querySelector('#app-view')?.getAttribute('aria-busy'), connection: document.querySelector('#connection')?.getAttribute('aria-label'),
  rows: [...document.querySelectorAll('[data-room-id]')].map(row => `${row.dataset.roomId}:${(row.querySelector('.room-name')?.textContent || '').slice(0, 20)}`),
  projects: document.querySelector('#projects')?.innerText.replace(/\s+/g, ' ').slice(0, 300), wanted: id,
}), roomID);
async function main() {
  {
    const audit = spawnSync(process.execPath, [path.join(__dirname, 'a11y-audit.cjs'), '--strict'], { env: isolatedEnv, encoding: 'utf8' });
    assert.equal(audit.status, 0, `accessibility audit failed:\n${audit.stdout}${audit.stderr}`);
    const phone = spawnSync(process.execPath, [path.join(__dirname, 'phone-audit.cjs')], { env: isolatedEnv, encoding: 'utf8' });
    assert.equal(phone.status, 0, `phone layout audit failed:\n${phone.stdout}${phone.stderr}`);
  }
  const port = await freePort(); const origin = `http://127.0.0.1:${port}`;
  const child = spawn(binary, ['server'], { env: { ...isolatedEnv, AIMEBU2_USAGES: '0', AIMEBU2_EMBEDDED_SUPERVISOR: '0', AIMEBU2_PORT: String(port), AIMEBU2_OWNER_LOGIN: 'owner', AIMEBU2_OWNER_PASSWORD: 'test-password-123' }, stdio: ['ignore', 'pipe', 'pipe'] });
  let logs = ''; child.stderr.on('data', data => logs += data);
  let browser, supervisor, preserveFailure = false, browserClosing = false;
  try {
    for (let i = 0; i < 300; i++) { try { const response = await fetch(origin); if (response.ok) break; } catch {} await new Promise(r => setTimeout(r, 100)); if (i === 299) throw new Error(`server did not start: ${logs}`); }
    supervisor = spawn(binary, ['supervisor'], { env: { ...isolatedEnv, AIMEBU2_URL: origin }, stdio: ['ignore', 'pipe', 'pipe'] });
    let supervisorLogs = ''; supervisor.stderr.on('data', data => supervisorLogs += data);
    browser = await puppeteer.launch({ executablePath: '/usr/bin/chromium', headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
    // A page wait that dies with "frame got detached" and no step in its trace means Chromium itself went away; say so.
    browser.process()?.once('exit', (code, signal) => { if (!browserClosing) console.error(`Chromium exited before the smoke finished (code ${code}, signal ${signal}); a pending wait fails as "frame got detached"`); });
    // The smoke exercises threads, copy and links through the compact toolbar (style C); style A, the product default, has
    // its own steps. Every page starts with that choice stored, before any navigation.
    const prepare = async page => { await page.evaluateOnNewDocument(() => { try { if (!localStorage.getItem('aimebu2.ui.variant')) localStorage.setItem('aimebu2.ui.variant', 'c'); } catch {}
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
      await page.waitForFunction(selector => new Promise(resolve => {
        const button = document.querySelector(selector);
        if (!button || button.disabled || button.hidden || !button.getClientRects().length) { resolve(false); return; }
        const box = () => { const rect = button.getBoundingClientRect(); return [rect.x, rect.y, rect.width, rect.height].join(); };
        const before = box();
        requestAnimationFrame(() => requestAnimationFrame(() => resolve(button.isConnected && box() === before && !button.disabled)));
      }), {}, selector);
      await page.$eval(selector, button => button.click());
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
      await appReady(page); await page.waitForFunction(() => document.querySelector('#connection')?.getAttribute('aria-label') === 'Connected'); }
    // puppeteer has no check()/uncheck(); toggle through the element's own click so change handlers run.
    async function setChecked(page, selector, checked) { await page.$eval(selector, (el, want) => { if (el.checked !== want) el.click(); }, checked); }
    // The Settings nav in two groups: SETTINGS (General ... Macros), then a gap and AGENTS (Machines, Harnesses). `items` lists the shown
    // entries in DOM order (a caption as its upper-case text, a tab as its section); `gap` is the space between the last tab above the AGENTS
    // caption and the first one below it, `tabGap` the space between two neighbouring tabs. The AGENTS caption is styled like the SETTINGS
    // one, and neither caption can take focus.
    const settingsNav = page => page.evaluate(() => {
      const nav = document.querySelector('#settings-dialog nav'), [settings, agents] = nav.querySelectorAll('.settings-nav-title');
      const items = [...nav.children].filter(el => !el.hidden).map(el => el.classList.contains('settings-nav-title') ? el.textContent.toUpperCase() : el.id.replace(/^settings-|-tab$/g, ''));
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
      assert.deepEqual(await page.evaluate(() => { const nav = document.querySelector('#settings-dialog nav'), caption = nav.querySelector('.settings-nav-title'), selected = nav.querySelector('.settings-selected'), close = document.querySelector('#settings-close'), box = document.querySelector('#settings-dialog').getBoundingClientRect();
        return { caption: caption.textContent, captionFirst: nav.firstElementChild === caption, title: document.querySelector('#settings-title').textContent, selected: selected.textContent, closeGlyph: close.querySelector('.icon-mask-close') ? 'icon' : close.textContent, closeQuiet: getComputedStyle(close).borderTopWidth === '0px', navWidth: Math.round(nav.getBoundingClientRect().width), fullHeight: Math.abs(box.height - innerHeight) < 1, selectedStyle: [getComputedStyle(selected).backgroundColor, getComputedStyle(selected).outlineStyle, getComputedStyle(selected).outlineWidth] }; }),
        { caption: 'Settings', captionFirst: true, title: 'General', selected: 'General', closeGlyph: 'icon', closeQuiet: true, navWidth: 180, fullHeight: true, selectedStyle: ['rgba(0, 0, 0, 0)', 'solid', '1px'] }, 'the Settings dialog follows v1');
      const nav = await settingsNav(page);
      if (nav.items.includes('AGENTS')) { // the owner and admins: both groups, the AGENTS caption a group gap below Macros
        assert.deepEqual(nav.items.filter(item => item !== 'usages' && item !== 'users'), ['SETTINGS', 'general', 'project', 'notifications', 'roles', 'prompts', 'macros', 'AGENTS', 'machines', 'harnesses'], 'Settings nav groups');
        assert.ok(nav.sameStyle && !nav.focusable && nav.gap >= nav.tabGap + 14, `the AGENTS caption looks like SETTINGS, takes no focus and opens a gap: ${JSON.stringify(nav)}`);
      }
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
    {
      await first.goto(origin);
      await first.locator('#login-form [name=login]').fill('owner'); await first.locator('#login-form [name=password]').fill('test-password-123'); await first.locator('#login-form button').click();
      await first.waitForSelector('#welcome-view', { timeout: 30000 });
      assert.deepEqual(await first.evaluate(() => ({ stage: document.querySelector('#welcome-view').dataset.stage, inert: document.querySelector('#welcome-continue').inert, app: document.querySelector('#app-view').hidden })), { stage: 'logo', inert: true, app: true }, 'the welcome starts with the logo alone and the app behind it');
      await first.waitForFunction(() => document.querySelector('#welcome-view').dataset.stage === 'done', { timeout: 30000 });
      await settled();
      assertFinished(await welcomeState(), 'first sign-in');
      assertIcon(await iconState(), 'first sign-in');
      await first.click('#welcome-continue');
      await appReady(first);
      await first.waitForFunction(() => document.querySelector('#connection')?.getAttribute('aria-label') === 'Connected');
      assert.equal(await first.$('#welcome-view'), null, 'the welcome is gone once Continue is pressed');
      assert.equal(await first.evaluate(async () => (await (await fetch('/api/me')).json()).first_run), false, 'the server remembers it was seen');
      // Skipping, and reduced motion, go straight to the finished screen.
      const skips = await first.evaluate(async () => {
        const { showWelcome } = await import('/js/welcome.js' + new URL(document.querySelector('script[type=module]').src).search);
        const out = {};
        for (const how of ['click', 'Enter', 'Space', 'reduced']) {
          const done = showWelcome(null, { reduced: how === 'reduced' });
          const view = document.querySelector('#welcome-view');
          if (how === 'click') view.click(); else if (how === 'Enter') document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true })); else if (how === 'Space') document.dispatchEvent(new KeyboardEvent('keydown', { key: ' ', bubbles: true }));
          out[how] = { stage: view.dataset.stage, inert: document.querySelector('#welcome-continue').inert, focused: document.activeElement?.id };
          document.querySelector('#welcome-continue').click(); await done;
          out[how].removed = !document.querySelector('#welcome-view');
        }
        return out;
      });
      for (const [how, state] of Object.entries(skips)) assert.deepEqual(state, { stage: 'done', inert: false, focused: 'welcome-continue', removed: true }, `${how} goes straight to the finished welcome`);
      // Continue is the green fill alone: no border, and no ring while focus was only moved there by the screen. A person who uses Tab
      // gets the normal keyboard ring (also in a high-contrast theme).
      const ringOf = () => first.evaluate(() => { const button = document.querySelector('#welcome-continue'), style = getComputedStyle(button); return { focused: document.activeElement === button, auto: 'autoFocus' in button.dataset, border: style.borderTopWidth, ring: style.outlineStyle !== 'none' && parseFloat(style.outlineWidth) > 0, shadow: style.boxShadow, fill: style.backgroundColor }; });
      for (const theme of ['', 'high-contrast-dark']) {
        const saved = await first.evaluate(value => { const before = document.documentElement.dataset.theme || ''; if (value) document.documentElement.dataset.theme = value; return before; }, theme);
        await first.evaluate(async () => { const { showWelcome } = await import('/js/welcome.js' + new URL(document.querySelector('script[type=module]').src).search); window.__welcomeDone = showWelcome(null, { reduced: true }); });
        await first.waitForSelector('#welcome-continue:not([inert])');
        const idle = await ringOf();
        assert.deepEqual([idle.focused, idle.auto, idle.border, idle.ring, idle.shadow], [true, true, '0px', false, 'none'], `${theme || 'default'} theme: Continue is focused with no border and no ring: ${JSON.stringify(idle)}`);
        assert.notEqual(idle.fill, 'rgba(0, 0, 0, 0)', 'Continue has its green fill');
        let keyboard = idle;
        for (let i = 0; i < 4 && !(keyboard.focused && keyboard.ring); i++) { await first.keyboard.press('Tab'); keyboard = await ringOf(); }
        assert.deepEqual([keyboard.focused, keyboard.auto, keyboard.ring], [true, false, true], `${theme || 'default'} theme: after Tab the keyboard ring shows: ${JSON.stringify(keyboard)}`);
        await first.keyboard.press('Enter');
        await first.evaluate(() => window.__welcomeDone);
        assert.equal(await first.$('#welcome-view'), null, 'Enter on Continue proceeds');
        await first.evaluate(value => { if (value) document.documentElement.dataset.theme = value; else delete document.documentElement.dataset.theme; }, saved);
      }
      // The artwork follows the theme: the dark icon on a dark theme, the light icon on a light one.
      await first.evaluate(async () => { const { showWelcome } = await import('/js/welcome.js' + new URL(document.querySelector('script[type=module]').src).search); window.__welcomeDone = showWelcome(null, { reduced: true }); });
      await first.waitForSelector('#welcome-view');
      const themed = [], savedTheme = await first.evaluate(() => document.documentElement.dataset.theme || '');
      for (const theme of ['light', 'blue-light', 'dark', 'high-contrast-dark']) { await first.evaluate(value => { document.documentElement.dataset.theme = value; }, theme); themed.push([theme, (await iconState()).shown[0]]); }
      await first.evaluate(value => { if (value) document.documentElement.dataset.theme = value; else delete document.documentElement.dataset.theme; }, savedTheme);
      assert.deepEqual(themed, [['light', '/icons/icon-512.png'], ['blue-light', '/icons/icon-512.png'], ['dark', '/icons/icon-512-dark.png'], ['high-contrast-dark', '/icons/icon-512-dark.png']], 'the welcome icon follows the theme');
      await first.click('#welcome-continue'); await first.evaluate(() => window.__welcomeDone);
      // The welcome is first-run only: Settings -> General has no section to play it again.
      await openSettings(first);
      assert.deepEqual(await first.evaluate(() => ({ section: !!document.querySelector('#settings-welcome, #settings-welcome-play'), headings: [...document.querySelectorAll('#settings-general h3')].map(el => el.textContent.trim().toLowerCase()) })), { section: false, headings: ['appearance', 'chat', 'messages', 'settings backup'] }, 'Settings -> General has no Welcome section');
      await first.evaluate(() => document.querySelector('#settings-dialog').close());
    }
    // v1's global shortcuts: Ctrl/Cmd+, opens Settings on General; Ctrl/Cmd+W closes it again; Shift in the chord does nothing.
    {
      const chord = async (key, ...held) => { for (const name of held) await first.keyboard.down(name); await first.keyboard.press(key); for (const name of held.reverse()) await first.keyboard.up(name); };
      const open = () => first.evaluate(() => document.querySelector('#settings-dialog').open);
      await chord(',', 'Control', 'Shift'); await new Promise(resolve => setTimeout(resolve, 150));
      assert.equal(await open(), false, 'Ctrl+Shift+, is not the shortcut');
      await chord(',', 'Control');
      await first.waitForFunction(() => document.querySelector('#settings-dialog').open && !document.querySelector('#settings-general').hidden);
      await chord('w', 'Control');
      await first.waitForFunction(() => !document.querySelector('#settings-dialog').open);
      await chord(',', 'Meta');
      await first.waitForFunction(() => document.querySelector('#settings-dialog').open);
      await first.keyboard.press('Escape');
      await first.waitForFunction(() => !document.querySelector('#settings-dialog').open);
    }
    // The app icon: the header logo follows the theme, and the favicon links, apple-touch icon and web app manifest all load.
    {
      const icons = await first.evaluate(async () => {
        const visibleLogo = () => [...document.querySelectorAll('.brand-logo')].filter(img => getComputedStyle(img).display !== 'none').map(img => ({ cls: img.className, loaded: img.complete && img.naturalWidth > 0 }));
        const root = document.documentElement, saved = root.dataset.theme;
        const out = { dark: visibleLogo() };
        root.dataset.theme = 'light'; out.light = visibleLogo();
        root.dataset.theme = 'blue-light'; out.blueLight = visibleLogo();
        root.dataset.theme = 'high-contrast-dark'; out.hcDark = visibleLogo();
        if (saved === undefined) delete root.dataset.theme; else root.dataset.theme = saved;
        const get = async url => { const response = await fetch(url); return { url, status: response.status, type: response.headers.get('content-type') || '' }; };
        out.links = await Promise.all([...document.querySelectorAll('link[rel~="icon"], link[rel="apple-touch-icon"], link[rel="manifest"]')].map(link => get(link.getAttribute('href')).then(result => ({ rel: link.rel, ...result }))));
        const manifest = await (await fetch('/manifest.webmanifest')).json();
        out.manifest = { name: manifest.name, display: manifest.display, icons: await Promise.all(manifest.icons.map(async icon => {
          const image = new Image(); image.src = icon.src; await image.decode();
          return { src: icon.src, status: (await fetch(icon.src)).status, size: icon.sizes === 'any' ? 'any' : `${image.naturalWidth}x${image.naturalHeight}`, declared: icon.sizes };
        })) };
        return out;
      });
      assert.deepEqual(icons.dark, [{ cls: 'brand-logo brand-logo-dark', loaded: true }], 'the default (dark) theme shows the dark logo');
      assert.deepEqual(icons.light, [{ cls: 'brand-logo brand-logo-light', loaded: true }]);
      assert.deepEqual(icons.blueLight, [{ cls: 'brand-logo brand-logo-light', loaded: true }]);
      assert.deepEqual(icons.hcDark, [{ cls: 'brand-logo brand-logo-dark', loaded: true }]);
      assert.deepEqual(icons.links.map(link => [link.rel, link.status, link.type.split(';')[0]]), [['icon', 200, 'image/svg+xml'], ['icon', 200, 'image/vnd.microsoft.icon'], ['apple-touch-icon', 200, 'image/png'], ['manifest', 200, 'application/manifest+json']], JSON.stringify(icons.links));
      assert.equal(icons.manifest.name, 'aimebu'); assert.equal(icons.manifest.display, 'standalone');
      assert.ok(icons.manifest.icons.length >= 2 && icons.manifest.icons.every(icon => icon.status === 200 && (icon.declared === 'any' || icon.size === icon.declared)), JSON.stringify(icons.manifest));
    }
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
        sockets[0].emit({ type: 'event', event: { seq: 8 } });
        sockets[0].emit({ type: 'ready', seq: 8 });
        await until(() => sockets.length === 2);
        const retryAfter = new URL(sockets[1].url).searchParams.get('after');
        sockets[1].emit({ type: 'event', event: { seq: 8 } });
        sockets[1].emit({ type: 'ready', seq: 8 });
        await until(() => stream.ready);
        sockets[1].emit({ type: 'event', event: { seq: 13 } });
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
    assert.equal(await startupPage.evaluate(() => document.querySelector('#app-view').getAttribute('aria-busy') === 'true' && document.querySelector('#settings-toggle').inert && !document.querySelector('#logout').inert && !document.querySelector('#startup-status').hidden), true, 'workspace controls or loading state were wrong before the import check finished');
    await startupPage.click('#settings-toggle');
    assert.equal(await startupPage.$eval('#settings-dialog', node => node.open), false, 'Settings opened during the import check');
    await startupPage.evaluate(() => window.__releaseImportCheck());
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
    const macroUI = await first.evaluate(async () => {
      const { expandMacros, MacroAutocomplete, MAX_MACRO_EXPANSION } = await import('/js/macros.js');
      const macros = { greet: 'Hello <NAME>', name: 'world', loop: '<loop>' };
      const { csrf_token } = await (await fetch('/api/me')).json();
      const putMacros = async body => {
        const response = await fetch('/api/macros', { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify(body) });
        if (!response.ok) throw new Error((await response.json()).message || `macro save ${response.status}`);
      };
      await putMacros(macros);
      const host = document.createElement('div'), draft = document.createElement('textarea'); host.append(draft); document.body.append(host);
      const picker = new MacroAutocomplete(draft);
      draft.value = 'Before <GR'; draft.setSelectionRange(draft.value.length, draft.value.length);
      draft.dispatchEvent(new Event('input'));
      await picker.refresh();
      const offered = [...picker.popup.querySelectorAll('[role=option]')].map(button => button.textContent);
      picker.keydown(new KeyboardEvent('keydown', { key: 'Enter', cancelable: true }));
      const burst = expandMacros('<a>', { a: '<a>'.repeat(20) });
      const overLimit = expandMacros('\\<'.repeat(MAX_MACRO_EXPANSION + 1), {});
      const result = { offered, inserted: draft.value, escaped: expandMacros('\\<NAME> <missing>', macros), nestedEscape: expandMacros('<outer>', { outer: '\\<secret>', secret: 'expanded' }), cycle: expandMacros('<loop>', macros), bounded: burst.length <= MAX_MACRO_EXPANSION && burst.includes('<a>'), prototypeKey: expandMacros('<constructor>', macros), escapedPopup: false, overLimitResolved: overLimit === '<'.repeat(MAX_MACRO_EXPANSION + 1) };
      draft.value = '\\<gr'; draft.setSelectionRange(draft.value.length, draft.value.length); draft.dispatchEvent(new Event('input'));
      await picker.refresh();
      result.escapedPopup = !picker.popup.hidden;
      host.remove(); await putMacros({});
      return result;
    });
    assert.deepEqual(macroUI, { offered: ['<GREET>Hello <NAME>'], inserted: 'Before Hello world ', escaped: '<NAME> <missing>', nestedEscape: '<secret>', cycle: '<loop>', bounded: true, prototypeKey: '<constructor>', escapedPopup: false, overLimitResolved: true });
    await openSettings(first);
    // Opening Settings puts focus on the dialog, not on a close button with a ring.
    assert.deepEqual(await first.evaluate(() => ({ dialogFocused: document.activeElement === document.querySelector('#settings-dialog'), closeFocused: document.activeElement === document.querySelector('#settings-close'), ring: getComputedStyle(document.querySelector('#settings-dialog')).outlineStyle })), { dialogFocused: true, closeFocused: false, ring: 'none' }, 'a dialog opens without a focus ring on its close button');
    // General reads evenly on desktop and on a phone: the expiry Save beside its input, the same
    // gap before every section, and on a phone a "Settings" title.
    for (const [width, height] of [[1100, 720], [390, 740]]) {
      await first.setViewport({ width, height });
      await first.waitForFunction(() => !document.querySelector('#settings-general').hidden && document.querySelector('#message-expiry-save').getBoundingClientRect().width > 0);
      const general = await first.evaluate(() => {
        const box = selector => { const r = document.querySelector(selector).getBoundingClientRect(); return { top: r.top, bottom: r.bottom, left: r.left, right: r.right, height: r.height }; };
        const input = box('#message-expiry-seconds'), save = box('#message-expiry-save');
        const root = document.querySelector('#settings-general');
        const groups = [...root.querySelectorAll('.settings-group')].filter(group => group.getClientRects().length), gaps = [];
        for (const group of groups) { const heading = group.parentElement.querySelector(':scope > h3') || group.previousElementSibling; const next = groups[groups.indexOf(group) + 1]; if (!next) continue; const nextHeading = next.parentElement.querySelector(':scope > h3'); if (nextHeading) gaps.push(Math.round(nextHeading.getBoundingClientRect().top - group.getBoundingClientRect().bottom)); }
        return { saveBesideInput: Math.abs((save.top + save.bottom) / 2 - (input.top + input.bottom) / 2) <= 6 && save.left >= input.right - 1, gaps, title: getComputedStyle(document.querySelector('#settings-title'), '::before').content };
      });
      assert.ok(general.saveBesideInput, `${width}px: the expiry Save sits beside its input: ${JSON.stringify(general)}`);
      assert.ok(general.gaps.length >= 3 && Math.max(...general.gaps) - Math.min(...general.gaps) <= 2, `${width}px: the gap before each section is the same: ${JSON.stringify(general.gaps)}`);
      assert.equal(/Settings/.test(general.title), width <= 700, `${width}px: the phone title says Settings: ${general.title}`);
    }
    await first.setViewport({ width: 1100, height: 720 });
    // Settings backup (owner): Export downloads valid JSON of the instance's settings, and Import previews before it saves anything.
    {
      await first.waitForFunction(() => !document.querySelector('#settings-backup').hidden);
      const exported = await first.evaluate(async () => {
        let blob = null; const make = URL.createObjectURL; URL.createObjectURL = value => { blob = value; return make.call(URL, value); };
        document.querySelector('#settings-backup-export').click();
        for (let i = 0; i < 100 && !blob; i++) await new Promise(resolve => setTimeout(resolve, 50));
        URL.createObjectURL = make; return blob ? await blob.text() : null;
      });
      const backup = JSON.parse(exported);
      assert.deepEqual([backup.schema, backup.version, Array.isArray(backup.roles), typeof backup.retention], ['aimebu-settings-backup', 1, true, 'object'], 'the export is a settings backup');
      assert.equal(/token|secret|password/i.test(Object.keys(backup).join(',')), false, 'no credential-shaped section');
      const file = path.join(tmp, 'settings-backup.json');
      fs.writeFileSync(file, JSON.stringify({ schema: 'aimebu-settings-backup', version: 1, macros: { 'from-backup': 'Imported macro' }, ui_prefs: { usages_group: 'active' }, future_thing: {} }));
      await (await first.$('#settings-backup-file')).uploadFile(file);
      await first.waitForFunction(() => !document.querySelector('#settings-backup-plan').hidden);
      const planText = await first.$eval('#settings-backup-plan', node => node.textContent);
      assert.match(planText, /Macros: 1 new/); assert.match(planText, /Interface preferences/); assert.match(planText, /future_thing/);
      assert.equal(await first.evaluate(async () => Boolean((await (await fetch('/api/macros')).json())['from-backup'])), false, 'previewing saved nothing');
      await domClick(first, '#settings-backup-apply');
      await first.waitForFunction(() => document.querySelector('#settings-backup-plan').hidden && /Imported/.test(document.querySelector('#settings-backup-status').textContent));
      assert.equal(await first.evaluate(async () => (await (await fetch('/api/macros')).json())['from-backup']), 'Imported macro', 'apply saved the ticked macro');
      assert.equal(await first.evaluate(() => localStorage.getItem('aimebu2.usages.group')), 'active', 'the interface preference was applied to this browser');
      // Only the browser's interface preferences ticked: the server is not asked, so nothing else is overwritten.
      fs.writeFileSync(file, JSON.stringify({ schema: 'aimebu-settings-backup', version: 1, macros: { 'must-not-save': 'x' }, retention: { messages_considered_expired_after_seconds: 7200 }, ui_prefs: { usages_group: 'providers' } }));
      await (await first.$('#settings-backup-file')).uploadFile(file);
      await first.waitForFunction(() => !document.querySelector('#settings-backup-plan').hidden && document.querySelector('#settings-backup-plan input[value="ui_prefs"]'));
      for (const section of ['macros', 'retention']) await first.$eval(`#settings-backup-plan input[value="${section}"]`, box => { if (box.checked) box.click(); });
      await domClick(first, '#settings-backup-apply');
      await first.waitForFunction(() => document.querySelector('#settings-backup-plan').hidden && /Imported/.test(document.querySelector('#settings-backup-status').textContent));
      assert.equal(await first.evaluate(() => localStorage.getItem('aimebu2.usages.group')), 'providers', 'the ticked interface preference was applied');
      assert.deepEqual(await first.evaluate(async () => ({ macro: Boolean((await (await fetch('/api/macros')).json())['must-not-save']), seconds: (await (await fetch('/api/retention')).json()).messages_considered_expired_after_seconds })), { macro: false, seconds: 21600 }, 'sections left unticked were not applied (an empty server list must never mean all)');
      await first.evaluate(async () => { const { csrf_token } = await (await fetch('/api/me')).json(); await fetch('/api/macros', { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: '{}' }); localStorage.removeItem('aimebu2.usages.group'); });
    }
    // This block has flaked once under load without a captured cause. Each step waits for the
    // real condition it needs, and a timeout dumps the page and API state so a recurrence names itself.
    const macroStep = async (label, step) => {
      try { return await step(); }
      catch (error) {
        const state = await first.evaluate(async () => {
          const rect = selector => { const el = document.querySelector(selector); if (!el) return null; const box = el.getBoundingClientRect(); return { x: box.x, y: box.y, w: box.width, h: box.height, hidden: el.hidden, disabled: el.disabled === true }; };
          let api = null; try { api = await (await fetch('/api/macros')).json(); } catch (failure) { api = String(failure); }
          return { dialogOpen: document.querySelector('#settings-dialog')?.open, section: rect('#settings-macros'), editor: rect('#macro-editor'), key: rect('#macro-editor [name=key]'), list: document.querySelector('#macros-list')?.textContent, error: document.querySelector('#settings-macros .error')?.textContent, api, viewport: [innerWidth, innerHeight], active: document.activeElement?.tagName };
        }).catch(failure => `state unavailable: ${failure.message}`);
        error.message = `macros step "${label}": ${error.message}\n${JSON.stringify(state)}`;
        throw error;
      }
    };
    await domClick(first, '#settings-macros-tab');
    assert.equal(await first.evaluate(() => { const size = selector => getComputedStyle(document.querySelector(selector)).fontSize; return size('#settings-macros .settings-intro') === size('#settings-general .settings-intro, #settings-usages .settings-intro'); }), true, 'the Macros intro is set like the other intros');
    await macroStep('empty list rendered', () => first.waitForFunction(() => document.querySelector('#macros-list')?.textContent.includes('No macros yet.'), { timeout: 20000 }));
    await macroStep('editor visible', () => first.waitForSelector('#macro-editor:not([hidden]) [name=key]', { visible: true, timeout: 20000 }));
    assert.equal(await first.$eval('#macro-editor', form => form.hidden), false, 'instance owner should be able to edit macros');
    await macroStep('fill key', () => first.locator('#macro-editor [name=key]').setTimeout(20000).fill('review'));
    await macroStep('fill body', () => first.locator('#macro-editor [name=body]').setTimeout(20000).fill('Review this draft'));
    await domClick(first, '#macro-editor button[type=submit]');
    await macroStep('macro saved', () => first.waitForSelector('#macros-list .snippet-row button.btn-danger', { timeout: 20000 }));
    // Search, Copy all and Import: preview says what will happen before anything is saved, then apply merges by key.
    await macroStep('import opens', async () => { await domClick(first, '#macros-import-open'); await first.waitForSelector('#macros-import:not([hidden]) textarea', { visible: true, timeout: 20000 }); });
    await first.locator('#macros-import-text').fill('not json');
    await first.click('#macros-import-preview');
    assert.match(await first.$eval('#macros-import-summary', node => node.textContent), /not valid JSON/);
    assert.equal(await first.$eval('#macros-import-apply', node => node.disabled), true, 'apply stays off for bad JSON');
    await first.locator('#macros-import-text').fill(JSON.stringify({ alpha: 'Alpha text', review: 'Changed body', '9bad': 'x' }));
    await first.click('#macros-import-preview');
    assert.match(await first.$eval('#macros-import-summary', node => node.textContent), /Will add 1 new and overwrite 1 existing.*Skipped 1: 9bad/);
    assert.equal(await first.$eval('#macros-list', node => node.textContent.includes('Alpha text')), false, 'preview saves nothing');
    await domClick(first, '#macros-import-apply');
    await macroStep('import applied', () => first.waitForFunction(() => { const text = document.querySelector('#macros-list')?.textContent || ''; return text.includes('<ALPHA>') && text.includes('Changed body') && document.querySelector('#macros-import').hidden; }, { timeout: 20000 }));
    await first.locator('#macros-search').fill('alph');
    assert.deepEqual(await first.$$eval('#macros-list .snippet-row strong', nodes => nodes.map(node => node.textContent)), ['<ALPHA>'], 'search narrows the list');
    await first.locator('#macros-search').fill('zzz');
    assert.match(await first.$eval('#macros-list', node => node.textContent), /No macros match the search/);
    await first.evaluate(() => { const box = document.querySelector('#macros-search'); box.value = ''; box.dispatchEvent(new Event('input')); });
    assert.equal(await first.$$eval('#macros-list .snippet-row', nodes => nodes.length), 2, 'clearing the search shows every macro again');
    const copied = await first.evaluate(async () => {
      let text = null; const original = navigator.clipboard?.writeText;
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async value => { text = value; } } });
      document.querySelector('#macros-copy').click(); await new Promise(resolve => setTimeout(resolve, 100));
      return { text, label: document.querySelector('#macros-copy').textContent, original: Boolean(original) };
    });
    assert.deepEqual(JSON.parse(copied.text), { alpha: 'Alpha text', review: 'Changed body' }, 'Copy all copies the macros as JSON');
    assert.equal(copied.label, 'Copied');
    for (const key of ['alpha', 'review']) {
      await domClick(first, '#macros-list button.btn-danger');
      await macroStep(`macro ${key} deleted`, () => first.waitForFunction(count => document.querySelectorAll('#macros-list .snippet-row').length === count, { timeout: 20000 }, key === 'alpha' ? 1 : 0));
    }
    await macroStep('macro deleted', () => first.waitForFunction(() => !document.querySelector('#macros-list')?.textContent.includes('<REVIEW>'), { timeout: 20000 }));
    // Harnesses: nothing is seeded, so add Codex by hand, give it models in the UI, and see them reach the spawn catalog.
    {
      const machineID = await first.evaluate(async () => (await (await fetch('/api/machines')).json()).machines?.[0]?.id || (await (await fetch('/api/machines')).json())[0]?.id);
      const made = await first.evaluate(async id => {
        const { csrf_token } = await (await fetch('/api/me')).json();
        const response = await fetch(`/api/machines/${id}/launchers`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ tool: 'codex', command: 'codex', args: [], env_refs: {}, enabled: true, containerized: false, path_maps: [], visible_roots: [], supports_toolbox: true }) });
        return response.status;
      }, machineID);
      assert.equal(made, 200, 'a harness is added through the API');
      await domClick(first, '#settings-harnesses-tab');
      await first.waitForFunction(() => document.querySelector('#harnesses-list .machine-harness'));
      assert.equal(await first.$eval('#harnesses-list .machine-harness .settings-label', el => el.textContent), 'Codex', 'the label is derived from the family');
      await domClick(first, '#harnesses-list .machine-harness .machine-harness-models-toggle');
      await first.waitForFunction(() => !document.querySelector('#harnesses-list .harness-models').hidden);
      assert.ok((await first.$eval('#harnesses-list .harness-models', el => el.textContent)).includes('No models yet'), 'a new harness has no models');
      await first.locator('#harnesses-list .harness-model-add [name=slug]').fill('gpt-ui-test');
      await first.locator('#harnesses-list .harness-model-add [name=new-label]').fill('UI Test');
      await first.locator('#harnesses-list .harness-model-add [name=new-note]').fill('added by the smoke');
      await domClick(first, '#harnesses-list .harness-model-add button[type=submit]');
      await first.locator('#harnesses-list .harness-model-add [name=slug]').fill('gpt-ui-two');
      await domClick(first, '#harnesses-list .harness-model-add button[type=submit]');
      assert.deepEqual(await first.$$eval('#harnesses-list .harness-model-row', rows => rows.map(row => [row.dataset.slug, row.querySelector('input[type=radio]').checked])), [['gpt-ui-test', true], ['gpt-ui-two', false]], 'the first model is the default');
      await first.$eval('#harnesses-list .harness-model-row[data-slug="gpt-ui-two"] input[type=radio]', el => el.click());
      await domClick(first, '#harnesses-list .harness-models .btn-primary');
      // The save is asynchronous: wait until the server has the two models, not just until the box shows them.
      await first.waitForFunction(async () => ((await (await fetch('/api/spawn-catalog')).json()).tools.find(tool => tool.tool === 'codex')?.models || []).length === 2);
      await first.waitForFunction(() => document.querySelector('#harnesses-list .harness-model-row[data-slug="gpt-ui-two"] input[type=radio]')?.checked === true && !document.querySelector('#harnesses-list .harness-models').hidden);
      const catalogModels = await first.evaluate(async () => (await (await fetch('/api/spawn-catalog')).json()).tools.find(tool => tool.tool === 'codex').models.map(model => [model.id, model.label, model.note || '', !!model.default]));
      assert.deepEqual(catalogModels, [['gpt-ui-test', 'UI Test', 'added by the smoke', false], ['gpt-ui-two', 'gpt-ui-two', '', true]], 'the saved models and the default reach the spawn catalog');
      // A refused list says why and keeps the draft.
      await first.locator('#harnesses-list .harness-model-add [name=slug]').fill('bad id');
      await domClick(first, '#harnesses-list .harness-model-add button[type=submit]');
      assert.ok((await first.$eval('#harnesses-list .harness-models .error', el => el.textContent)).includes('not a valid model id'));
      await first.locator('#harnesses-list .harness-model-add [name=slug]').fill('');
      // Remove the harness again so later steps start clean.
      await domClick(first, '#harnesses-list .machine-harness .machine-harness-edit');
      await domClick(first, '#harnesses-list .machine-harness .btn-danger');
      await first.waitForFunction(() => !document.querySelector('#harnesses-list .machine-harness'));
    }
    await domClick(first, '#settings-close');
    // The import wizard is a stepper: one page per category, a progress bar, Back and Next, the draft kept across a reload, and a
    // summary that is the dry run in words. This walks the whole flow on a canned backup.
    const importWizardFlow = await first.evaluate(async () => {
      const { ImportV1Wizard } = await import('/js/import-v1.js');
      const original = window.fetch;
      const json = (body, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
      localStorage.removeItem('aimebu2.import-draft.v1-flow');
      let statusState = 'none';
      const calls = { dry: [], commit: [], skip: 0 };
      const room = (id, extra = {}) => ({ id, messages: 3, members: [], suggested_project: '', ...extra });
      const source = {
        source_key: 'v1-flow',
        rooms: [room('a-leader', { messages: 5 }), room('a', { messages: 7, memory_disabled: true }), room('solo', { messages: 2 }), room('scratch', { messages: 1 }),
          room('_system', { messages: 9 }),
          room('dm:alice:nina@repo', { messages: 4, dm: true, title: 'nina', participants: [{ id: 'alice', name: 'alice', kind: 'user' }, { id: 'nina@repo', name: 'nina', kind: 'agent' }] })],
        humans: ['alice', 'bob'], memory_projects: ['a', 'kcd2'], counts: { rooms: 5, messages: 19, attachments: 0, memory: 3, macros: 2 },
        macros: { greet: 'v1 hello', other: 'v1 other' }, macro_defaults: [], destination_users: ['owner', 'other'], v1_memory_enabled: true, v1_debug_button: true,
        roles: [{ key: 'reviewer', label: 'Reviewer', emoji: '🔎', body: 'Parent\n\nFocus', current_body: 'V2 reviewer', cardinality: 'multi', conflict: true }],
        not_carried: { plans: 31 },
        suggested: { source_key: 'v1-flow', owner_login: 'owner', projects: { a: { name: 'a', folder: '' }, kcd2: { name: 'kcd2', folder: '' } },
          rooms: { 'a-leader': { project: 'a', target: 'root', lifecycle: 'open' }, a: { project: 'a', target: 'sub', title: 'general', lifecycle: 'open' }, solo: { project: '', skip: true, lifecycle: 'closed' },
            scratch: { project: '', skip: true }, _system: { project: '', skip: true }, 'dm:alice:nina@repo': { project: 'a', skip: true, lifecycle: 'open' } },
          humans: { alice: 'owner', bob: 'owner' }, memory_projects: { a: 'a', kcd2: 'kcd2' }, enable_project_memory: { a: true, kcd2: true }, skip_profiles: false, skip_shared_notes: false },
      };
      window.fetch = async (path, options) => {
        const url = String(path);
        if (url === '/api/import-v1/status') return json({ state: statusState });
        if (url === '/api/import-v1/preview') return json(source);
        if (url === '/api/macros') return json({ greet: 'v2 hello' });
        if (url === '/api/import-v1/dry-run') {
          calls.dry.push(JSON.parse(options.body));
          return json({ digest: 'digest-1', report: { imported: { rooms: 3, messages: 12, macros: 1 }, already_present: { rooms: 2, people: 1 }, skipped: { rooms: 2 }, skipped_no_project: ['solo'], memory_off_targets: ['memory is off for general under ⌂ a because v1 room a had it off'], target_warnings: ['2 v1 rooms are merged into ⌂ a: a, a-leader'], new_accounts: [{ v1_name: 'bob', login: 'bobby', role: 'operator', user_id: 'usr_1' }], not_carried: { plans: 31 },
            role_conflicts: { reviewer: 'use_v1' }, project_readers: { a: 1 }, project_access_gaps: {}, unreadable_dm_participants: {} } });
        }
        if (url === '/api/import-v1/commit') { calls.commit.push(JSON.parse(options.body)); return json({ job_id: 'flow-job' }, 202); }
        if (url === '/api/import-v1/jobs/flow-job') return json({ phase: 'done', report: { imported: { rooms: 3 }, already_present: {}, skipped: {} }, invites: [{ login: 'bobby', url: 'http://example.test/#invite=aui_one', expires_at: '2026-10-19T12:00:00Z' }] });
        if (url === '/api/import-v1/skip') { calls.skip++; return new Response(null, { status: 204 }); }
        return original(path, options);
      };
      const out = {};
      let wizard = new ImportV1Wizard();
      const buttons = () => [...wizard.actions.querySelectorAll('button')].map(button => button.textContent);
      const press = label => [...wizard.actions.querySelectorAll('button')].find(button => button.textContent === label).click();
      const settle = () => new Promise(resolve => setTimeout(resolve, 30));
      const count = text => text.match(/^\d+ of \d+ \w+/)[0];
      const find = (selector, within = wizard.content) => within.querySelector(selector);
      const choose = (select, value) => { select.value = value; select.dispatchEvent(new Event('change')); };
      const label = text => [...wizard.content.querySelectorAll('label')].find(item => item.firstChild?.textContent === text);
      try {
        await wizard.maybeOpen(); out.closedWithoutBackup = !wizard.dialog.open;
        statusState = 'skipped'; await wizard.maybeOpen(); out.closedWhenSkipped = !wizard.dialog.open;
        statusState = 'done'; await wizard.maybeOpen(); out.closedWhenDone = !wizard.dialog.open;
        await wizard.open();
        out.cannotEscape = (() => { const cancel = new Event('cancel', { cancelable: true }); wizard.dialog.dispatchEvent(cancel); return cancel.defaultPrevented; })();
        // Step 1: Projects. Progress, no Back, the skip link only here, and a blank folder blocks Next with a link to the field.
        out.first = { label: wizard.progressLabel.textContent, now: wizard.progressBar.getAttribute('aria-valuenow'), max: wizard.progressBar.getAttribute('aria-valuemax'), buttons: buttons(), skipOffered: !!find('.import-skip-open', wizard.skipZone), steps: wizard.steps.map(step => step.id) };
        press('Next'); await settle();
        const folder = find('input[data-project-folder="a"]');
        out.blocked = { still: wizard.progressLabel.textContent, error: wizard.error.textContent.includes('needs its workspace folder'), link: !!wizard.error.querySelector('button'), focused: document.activeElement === folder, invalid: folder.getAttribute('aria-invalid') };
        for (const alias of ['a', 'kcd2']) { const input = find(`input[data-project-folder="${alias}"]`); input.value = `/tmp/${alias}`; input.dispatchEvent(new Event('change')); }
        press('Next'); await settle();
        // Step 2: People. Back works, the skip link is gone.
        out.second = { label: wizard.progressLabel.textContent, buttons: buttons(), skipOffered: !!find('.import-skip-open', wizard.skipZone) };
        press('Back'); await settle(); out.backToProjects = wizard.progressLabel.textContent.includes('Projects') && find('input[data-project-folder="a"]').value === '/tmp/a';
        // v1's system room is never imported: a draft that put it in a project is ignored when it is restored.
        {
          const keep = wizard.mapping;
          wizard.mapping = JSON.parse(JSON.stringify(source.suggested)); wizard.mapping.rooms._system = { project: 'a', target: 'sub', title: 'general', lifecycle: 'open' };
          wizard.save(); wizard.mapping = JSON.parse(JSON.stringify(source.suggested)); wizard.restoreDraft();
          out.systemDraft = wizard.mapping.rooms._system;
          wizard.mapping = keep; wizard.save();
        }
        // Add destination project: the form opens just above the button, at the end of the list, and the new project lands at the end with its name focused.
        {
          const addButton = () => [...wizard.content.querySelectorAll('button')].find(button => button.textContent === 'Add destination project');
          addButton().click(); await settle();
          const form = find('.import-new-project');
          const formAtEnd = form.nextElementSibling === addButton() && addButton() === wizard.content.lastElementChild && form.previousElementSibling.tagName === 'FIELDSET';
          const formFocus = document.activeElement === form.querySelector('[data-new-project-name]');
          form.querySelector('[data-new-project-name]').value = 'Last One'; form.querySelector('[data-new-project-folder]').value = '/tmp/last';
          // Enter in the form makes the project; it does not skip to the next page.
          form.querySelector('[data-new-project-folder]').dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true })); await settle();
          const stayed = wizard.progressLabel.textContent.includes('Projects') && !!wizard.mapping.projects['last-one'];
          const boxes = [...wizard.content.querySelectorAll('fieldset')], lastBox = boxes[boxes.length - 1];
          out.addProject = { formAtEnd, formFocus, stayed, last: lastBox.querySelector('legend').textContent, nameFocused: document.activeElement === lastBox.querySelector('[data-project-name]'), buttonAfter: lastBox.nextElementSibling === addButton() };
          delete wizard.mapping.projects['last-one']; delete wizard.mapping.enable_project_memory['last-one']; wizard.refresh(); await settle();
        }
        // People: a v1 person can get a new account; a login that is taken blocks Next; the role and login are kept.
        press('Next'); await settle();
        choose(find('select[data-person="bob"]'), '__new_account__'); await settle();
        const account = { login: find('input[data-new-login="bob"]').value, role: find('select[data-new-role="bob"]').value, mapped: wizard.mapping.humans.bob };
        const typed = (selector, value) => { const input = find(selector); input.value = value; input.dispatchEvent(new Event('change')); };
        typed('input[data-new-login="bob"]', 'owner'); press('Next'); await settle();
        account.takenBlocked = wizard.error.textContent.includes('already an account here') && wizard.progressLabel.textContent.includes('People');
        typed('input[data-new-login="bob"]', 'x'); press('Next'); await settle();
        account.shortBlocked = wizard.error.textContent.includes('3 to 32 characters');
        typed('input[data-new-login="bob"]', 'bobby'); typed('select[data-new-role="bob"]', 'operator');
        account.kept = { users: JSON.stringify(wizard.mapping.new_users), mapped: wizard.mapping.humans.bob };
        out.account = account;
        press('Next'); await settle();
        // Step 3: Rooms. "Import into" per room, a live tree, a counter, merge warnings, "+ New project…".
        const rooms = { label: wizard.progressLabel.textContent, status: count(wizard.status.textContent) };
        rooms.system = { listed: !!wizard.content.querySelector('[data-import-into="_system"]'), inTree: wizard.content.querySelector('.import-room-tree').textContent.includes('_system'), note: wizard.content.textContent.includes('v1 system events are not imported.') };
        const into = id => find(`select[data-import-into="${id}"]`);
        rooms.options = [...into('solo').options].map(option => option.textContent);
        rooms.groups = [...into('solo').querySelectorAll('optgroup')].map(group => group.label);
        rooms.initial = { solo: into('solo').value, a: into('a').value, leader: into('a-leader').value };
        { const tree = find('.import-room-tree').textContent; rooms.tree = { root: tree.includes('⌂ a · 5 messages'), general: tree.includes('general · 7 messages'), dropped: tree.includes('Not imported (2)') && tree.includes('solo') }; }
        rooms.memory = { tree: find('.import-room-tree').textContent.includes('memory off (v1 room a had it off)'), row: find('[data-room="a"] .settings-intro')?.textContent.includes('memory off for its room "general"') };
        rooms.archivedForRoot = !!find('[data-room="a-leader"] input[type=checkbox]'); rooms.archivedForGeneral = find('[data-room="a"] input[type=checkbox]').checked;
        choose(into('solo'), 'general:kcd2'); await settle();
        rooms.afterSolo = { status: count(wizard.status.textContent), tree: find('.import-room-tree').textContent.includes('⌂ kcd2') && find('.import-room-tree').textContent.includes('general · 2 messages') };
        choose(into('a'), 'root:a'); await settle();
        rooms.merged = [...wizard.content.querySelectorAll('.import-warning')].map(item => item.textContent);
        choose(into('a'), 'general:a'); await settle();
        rooms.unmerged = wizard.content.querySelectorAll('.import-warning').length;
        // + New project… opens a form in place, makes the project and puts the room in its general room.
        choose(into('scratch'), '__new_project__'); await settle();
        const panel = find('.import-new-project');
        rooms.panel = !!panel;
        panel.querySelector('[data-new-project-name]').value = 'Fresh Start'; panel.querySelector('[data-new-project-folder]').value = '/tmp/fresh';
        [...panel.querySelectorAll('button')].find(button => button.textContent === 'Create project').click(); await settle();
        rooms.created = { alias: Object.keys(wizard.mapping.projects).includes('fresh-start'), project: wizard.mapping.projects['fresh-start'], memory: wizard.mapping.enable_project_memory['fresh-start'], placed: into('scratch').value, panelGone: !find('.import-new-project') };
        out.rooms = rooms;
        press('Next'); await settle();
        // Step 4: Direct messages. Off by default; turning one on shows its suggested project, and clearing it asks for one.
        const dms = { label: wizard.progressLabel.textContent, status: count(wizard.status.textContent), checked: find('input[type=checkbox]').checked, retitle: find('legend[data-dm-room]').textContent };
        find('input[type=checkbox]').click(); await settle();
        dms.after = { status: count(wizard.status.textContent), hasProject: !!label('Project'), prefilled: find('select', label('Project'))?.value };
        choose(find('select', label('Project')), ''); await settle(); press('Next'); await settle();
        dms.blocked = wizard.error.textContent.includes('Choose a project for the chat') && wizard.progressLabel.textContent.includes('Direct messages');
        choose(find('select', label('Project')), 'a'); await settle(); press('Next'); await settle();
        out.dms = dms;
        // Step 5: Memory.
        const memory = { label: wizard.progressLabel.textContent, scopes: [...wizard.content.querySelectorAll('label')].filter(item => item.firstChild?.textContent.startsWith('Project facts')).length, enableBoxes: [...wizard.content.querySelectorAll('label')].filter(item => item.textContent.includes('Enable project memory after import')).map(item => item.querySelector('input').checked) };
        out.memory = memory;
        press('Next'); await settle();
        // Step 6: Macros. Per-macro import, and all / none.
        const macros = { label: wizard.progressLabel.textContent, status: count(wizard.status.textContent) };
        const bulk = text => [...wizard.content.querySelectorAll('.import-bulk button')].find(button => button.textContent === text);
        bulk('Import none').click(); await settle(); macros.none = [...wizard.mapping.skip_macros].sort(); macros.noneStatus = count(wizard.status.textContent);
        bulk('Import all').click(); await settle(); macros.all = wizard.mapping.skip_macros.length;
        [...wizard.content.querySelectorAll('fieldset')].find(box => box.querySelector('legend').textContent.startsWith('<OTHER>')).querySelector('input[type=checkbox]').click(); await settle();
        macros.one = wizard.mapping.skip_macros; macros.conflict = !!label('On conflict');
        out.macros = macros;
        press('Next'); await settle();
        // Step 7: Roles.
        const role = label('On conflict').querySelector('select'); choose(role, 'use_v1');
        out.roles = { label: wizard.progressLabel.textContent, choice: wizard.mapping.role_conflicts.reviewer };
        press('Next'); await settle(); await settle();
        // Step 8: Summary, the dry run in words; zero columns hidden; Import now.
        const sent = calls.dry.at(-1);
        out.summary = {
          label: wizard.progressLabel.textContent, buttons: buttons(), skipOffered: !!find('.import-skip-open', wizard.skipZone),
          headers: [...wizard.content.querySelectorAll('th')].map(item => item.textContent), rowKeys: [...wizard.content.querySelectorAll('td[data-key]')].map(item => item.dataset.key),
          where: [...[...wizard.content.querySelectorAll('.import-left-out details')].find(box => box.querySelector('h3').textContent === 'Where each room goes').querySelectorAll('p')].map(item => item.textContent).filter(text => text.includes(' → ')), lists: [...wizard.content.querySelectorAll('.import-left-out summary h3')].map(item => item.textContent), accounts: wizard.content.textContent.includes('bobby (operator) for bob'), noProject: wizard.content.textContent.includes('solo'),
          plans: wizard.content.textContent.includes('plans: 31 rows'),
          sent: { accounts: JSON.stringify(sent.new_users), skipMacros: sent.skip_macros, rooms: [sent.rooms['a-leader'].target, sent.rooms.a.title, sent.rooms['dm:alice:nina@repo'].skip, sent.rooms['dm:alice:nina@repo'].project], projects: Object.keys(sent.projects), roles: sent.role_conflicts.reviewer },
        };
        // Back keeps every choice; the draft is in the browser and survives a new wizard (a reload).
        press('Back'); await settle(); press('Back'); await settle(); press('Back'); await settle();
        out.backKept = { label: wizard.progressLabel.textContent, macro: wizard.mapping.skip_macros.join() };
        wizard.dialog.close(); wizard.dialog.remove();
        const draft = JSON.parse(localStorage.getItem('aimebu2.import-draft.v1-flow'));
        out.draft = { step: draft.step, project: draft.mapping.projects['fresh-start']?.name, role: draft.mapping.role_conflicts.reviewer };
        wizard = new ImportV1Wizard(); statusState = 'ready'; await wizard.open();
        out.restored = { label: wizard.progressLabel.textContent, note: !!find('.import-restored'), mapping: wizard.mapping.projects['fresh-start']?.name, startOver: [...wizard.skipZone.querySelectorAll('button')].map(button => button.textContent) };
        // From the summary, Import now commits, clears the draft and carries the debug button.
        for (const key of Object.keys(localStorage)) if (key.startsWith('aimebu2.ui.debug-inspector.')) localStorage.removeItem(key);
        await wizard.go('summary'); await settle(); await settle();
        press('Import now'); await new Promise(resolve => setTimeout(resolve, 900));
        const invite = find('input[data-invite-for="bobby"]');
        out.invites = { value: invite?.value, copy: !!invite?.parentElement.querySelector('button'), heading: wizard.content.textContent.includes('Invite links for the new accounts'), first: wizard.content.firstElementChild?.classList.contains('import-invites'), hint: wizard.content.textContent.includes('Reset link') };
        out.committed = { digest: calls.commit.at(-1)?.digest, status: wizard.status.textContent, draftCleared: localStorage.getItem('aimebu2.import-draft.v1-flow') === null };
        // Skip is confirmed in a second dialog and offered on step 1 only.
        wizard.dialog.close(); wizard.dialog.remove();
        wizard = new ImportV1Wizard(); await wizard.open();
        out.skipKept = (() => { find('.import-skip-open', wizard.skipZone).click(); const confirm = wizard.dialog.querySelector('.import-skip-confirm'); const shown = confirm.open && confirm.textContent.includes('will not be offered again') && document.activeElement.textContent === 'Keep reviewing' && calls.skip === 0; [...confirm.querySelectorAll('button')].find(button => button.textContent === 'Keep reviewing').click(); return shown && !wizard.dialog.querySelector('.import-skip-confirm'); })();
        find('.import-skip-open', wizard.skipZone).click();
        [...wizard.dialog.querySelector('.import-skip-confirm').querySelectorAll('button')].find(button => button.textContent === 'Skip import forever').click(); await settle();
        out.skipped = { posts: calls.skip, closed: !wizard.dialog.open, draftCleared: localStorage.getItem('aimebu2.import-draft.v1-flow') === null };
        // A backup whose numbers were taken blocks every page and only offers the skip link.
        wizard.dialog.remove();
        wizard = new ImportV1Wizard(); await wizard.open();
        wizard.preview = { ...wizard.preview, reservation_missed: true }; wizard.render();
        out.reserved = { text: wizard.status.textContent.includes('can no longer import v1 history'), empty: wizard.content.children.length === 0 && wizard.actions.children.length === 0, skip: !!find('.import-skip-open', wizard.skipZone) };
        return out;
      } finally {
        window.fetch = original; localStorage.removeItem('aimebu2.import-draft.v1-flow');
        wizard.dialog.close(); wizard.dialog.remove();
        // The inspector choice is stored on the user now, so what the wizard turned on must not leak into the checks after this one.
        const { csrf_token } = await (await fetch('/api/me')).json();
        await fetch('/api/me/preferences', { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ preferences: { debug_inspector: '0' } }) });
        for (const key of Object.keys(localStorage)) if (key.startsWith('aimebu2.ui.debug-inspector.')) localStorage.removeItem(key);
        document.documentElement.dataset.debugInspector = 'off';
      }
    });
    assert.deepEqual(importWizardFlow, {
      closedWithoutBackup: true, closedWhenSkipped: true, closedWhenDone: true, cannotEscape: true,
      first: { label: 'Step 1 of 8 · Projects', now: '1', max: '8', buttons: ['Next'], skipOffered: true, steps: ['projects', 'people', 'rooms', 'dms', 'memory', 'macros', 'roles', 'summary'] },
      blocked: { still: 'Step 1 of 8 · Projects', error: true, link: true, focused: true, invalid: 'true' },
      second: { label: 'Step 2 of 8 · People', buttons: ['Back', 'Next'], skipOffered: false }, backToProjects: true, systemDraft: { project: '', skip: true }, addProject: { formAtEnd: true, formFocus: true, stayed: true, last: 'last-one', nameFocused: true, buttonAfter: true },
      account: { login: 'bob', role: 'viewer', mapped: 'bob', takenBlocked: true, shortBlocked: true, kept: { users: '{"bob":{"login":"bobby","role":"operator"}}', mapped: 'bobby' } },
      rooms: {
        label: 'Step 3 of 8 · Rooms', status: '2 of 4 rooms',
        system: { listed: false, inTree: false, note: true },
        options: ["Don't import", '⌂ a', 'a › general', 'a › new sub-room "solo"', '⌂ kcd2', 'kcd2 › general', 'kcd2 › new sub-room "solo"', '+ New project…'],
        groups: ['a', 'kcd2'], initial: { solo: 'skip', a: 'general:a', leader: 'root:a' }, tree: { root: true, general: true, dropped: true }, memory: { tree: true, row: true }, archivedForRoot: false, archivedForGeneral: false,
        afterSolo: { status: '3 of 4 rooms', tree: true },
        merged: ['2 v1 rooms will be merged into the root of a: a-leader, a.'], unmerged: 0, panel: true,
        created: { alias: true, project: { name: 'Fresh Start', folder: '/tmp/fresh' }, memory: true, placed: 'general:fresh-start', panelGone: true },
      },
      dms: { label: 'Step 4 of 8 · Direct messages', status: '0 of 1 chats', checked: false, retitle: 'nina', after: { status: '1 of 1 chats', hasProject: true, prefilled: 'a' }, blocked: true },
      memory: { label: 'Step 5 of 8 · Memory', scopes: 2, enableBoxes: [true, true, true] },
      macros: { label: 'Step 6 of 8 · Macros', status: '2 of 2 macros', none: ['greet', 'other'], noneStatus: '0 of 2 macros', all: 0, one: ['other'], conflict: true },
      roles: { label: 'Step 7 of 8 · Roles', choice: 'use_v1' },
      summary: {
        label: 'Step 8 of 8 · Summary', buttons: ['Back', 'Import now'], skipOffered: false,
        where: ['a-leader → ⌂ a', 'a → a › general', 'solo → kcd2 › general (archived)', 'scratch → Fresh Start › general (archived)', 'nina → direct message in a · dm:alice:nina@repo'], headers: ['Record', 'New', 'Skipped'], rowKeys: ['macros', 'messages', 'rooms'],
        lists: ['Where each room goes', 'Rooms not imported: no project', 'Memory switched off', 'Merged rooms', 'Accounts created', 'Role conflicts', 'Not carried over'], accounts: true, noProject: true, plans: true,
        sent: { accounts: '{"bob":{"login":"bobby","role":"operator"}}', skipMacros: ['other'], rooms: ['root', 'general', false, 'a'], projects: ['a', 'kcd2', 'fresh-start'], roles: 'use_v1' },
      },
      backKept: { label: 'Step 5 of 8 · Memory', macro: 'other' }, draft: { step: 'memory', project: 'Fresh Start', role: 'use_v1' },
      restored: { label: 'Step 5 of 8 · Memory', note: false, mapping: 'Fresh Start', startOver: [] },
      invites: { value: 'http://example.test/#invite=aui_one', copy: true, heading: true, first: true, hint: true },
      committed: { digest: 'digest-1', status: 'v1 import complete', draftCleared: true },
      skipKept: true, skipped: { posts: 1, closed: true, draftCleared: true }, reserved: { text: true, empty: true, skip: true },
    }, 'import wizard flow');
    // On a phone the Rooms page is one column: the tree sits under the list and nothing runs off the dialog.
    await first.setViewport({ width: 390, height: 800 });
    const phoneWizard = await first.evaluate(async () => {
      const { ImportV1Wizard } = await import('/js/import-v1.js');
      const original = window.fetch;
      const json = body => new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
      const source = { source_key: 'v1-phone', rooms: [{ id: 'a-leader', messages: 5, members: [] }, { id: 'a', messages: 7, members: [] }, { id: 'a-very-long-room-name-that-should-wrap-instead-of-pushing-the-page-sideways', messages: 1, members: [] }], humans: [], memory_projects: [], macros: {}, counts: { rooms: 3, messages: 13, memory: 0 }, destination_users: ['owner'],
        suggested: { source_key: 'v1-phone', owner_login: 'owner', projects: { a: { name: 'a', folder: '/tmp/a' } }, rooms: { 'a-leader': { project: 'a', target: 'root', lifecycle: 'open' }, a: { project: 'a', target: 'sub', title: 'general', lifecycle: 'open' }, 'a-very-long-room-name-that-should-wrap-instead-of-pushing-the-page-sideways': { project: 'a', target: 'sub', title: 'general', lifecycle: 'closed' } }, humans: {}, memory_projects: {} } };
      window.fetch = async (path, options) => String(path) === '/api/import-v1/preview' ? json(source) : String(path) === '/api/macros' ? json({}) : original(path, options);
      const wizard = new ImportV1Wizard();
      try {
        localStorage.removeItem('aimebu2.import-draft.v1-phone');
        await wizard.open(); wizard.stepID = 'rooms'; wizard.render();
        const dialog = wizard.dialog, layout = dialog.querySelector('.import-rooms');
        return { columns: getComputedStyle(layout).gridTemplateColumns.trim().split(/\s+/).length, dialogFits: dialog.scrollWidth <= dialog.clientWidth + 1, contentFits: wizard.content.scrollWidth <= wizard.content.clientWidth + 1, pageFits: document.documentElement.scrollWidth <= window.innerWidth,
          selectsFit: [...dialog.querySelectorAll('select')].every(select => select.getBoundingClientRect().right <= window.innerWidth), treeBelow: dialog.querySelector('.import-room-tree').getBoundingClientRect().top >= dialog.querySelector('.import-room-list').getBoundingClientRect().bottom - 1 };
      } finally { window.fetch = original; localStorage.removeItem('aimebu2.import-draft.v1-phone'); wizard.dialog.close(); wizard.dialog.remove(); }
    });
    // People: the owner (by name) maps to the owner's account; anyone else is offered a new account by default, never merged into the
    // owner. Skipping someone says what that means.
    const peopleDefault = await first.evaluate(async () => {
      const { ImportV1Wizard } = await import('/js/import-v1.js');
      const original = window.fetch;
      const json = body => new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
      const source = { source_key: 'v1-people', rooms: [], humans: ['sam', 'tomas', 'rae'], memory_projects: [], macros: {}, counts: { rooms: 0, messages: 0, attachments: 0, memory: 0, macros: 0 }, destination_users: ['sam'], roles: [], fleets: [],
        suggested: { source_key: 'v1-people', owner_login: 'sam', projects: {}, rooms: {}, humans: { sam: 'sam', tomas: '-', rae: '-' }, memory_projects: {}, enable_project_memory: {}, skip_profiles: false, skip_shared_notes: false } };
      window.fetch = async (path, options) => String(path) === '/api/import-v1/preview' ? json(source) : String(path) === '/api/macros' ? json({}) : original(path, options);
      const wizard = new ImportV1Wizard();
      try {
        localStorage.removeItem('aimebu2.import-draft.v1-people');
        await wizard.open(); wizard.stepID = 'people'; wizard.render();
        const select = person => wizard.content.querySelector(`select[data-person="${person}"]`);
        const out = { owner: select('sam').value, tomas: select('tomas').value, login: wizard.content.querySelector('input[data-new-login="tomas"]')?.value, role: wizard.content.querySelector('select[data-new-role="tomas"]')?.value, mapped: wizard.mapping.humans.tomas, warnedBefore: !!wizard.content.querySelector('.import-warning') };
        // A typed login counts at once (no blur needed), is selected on focus so typing replaces it, and Enter goes on to the next page.
        const loginInput = wizard.content.querySelector('input[data-new-login="tomas"]');
        loginInput.focus(); out.selectedOnFocus = loginInput.selectionStart === 0 && loginInput.selectionEnd === loginInput.value.length;
        loginInput.value = 'tomas2'; loginInput.dispatchEvent(new Event('input'));
        out.typed = wizard.mapping.new_users.tomas.login;
        let advanced = 0; wizard.next = () => { advanced++; };
        loginInput.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }));
        out.enter = advanced;
        loginInput.value = 'tomas'; loginInput.dispatchEvent(new Event('input'));
        select('tomas').value = '-'; select('tomas').dispatchEvent(new Event('change'));
        out.skipped = { select: select('tomas').value, warned: [...wizard.content.querySelectorAll('.import-warning')].map(item => item.textContent.startsWith('No account')) };
        // A saved skip is a decision and stays; a person the saved draft has no choice for gets the default; Start over defaults everyone again.
        wizard.save();
        const draftKey = 'aimebu2.import-draft.v1-people', saved = JSON.parse(localStorage.getItem(draftKey));
        delete saved.mapping.humans.rae; delete saved.mapping.new_users.rae; localStorage.setItem(draftKey, JSON.stringify(saved));
        wizard.dialog.close(); wizard.dialog.remove();
        const again = new ImportV1Wizard();
        await again.open();
        out.reopened = { tomas: again.mapping.humans.tomas, tomasAccount: !!again.mapping.new_users.tomas, rae: again.mapping.humans.rae, raeAccount: !!again.mapping.new_users.rae, owner: again.mapping.humans.sam };
        again.startOver();
        out.startedOver = { tomas: again.mapping.humans.tomas, rae: again.mapping.humans.rae };
        again.dialog.close(); again.dialog.remove();
        return out;
      } finally { window.fetch = original; localStorage.removeItem('aimebu2.import-draft.v1-people'); wizard.dialog.close(); wizard.dialog.remove(); }
    });
    assert.deepEqual(peopleDefault, { owner: 'sam', tomas: '__new_account__', login: 'tomas', role: 'viewer', mapped: 'tomas', warnedBefore: false, selectedOnFocus: true, typed: 'tomas2', enter: 1, skipped: { select: '-', warned: [true] }, reopened: { tomas: '-', tomasAccount: false, rae: 'rae', raeAccount: true, owner: 'sam' }, startedOver: { tomas: 'tomas', rae: 'rae' } }, 'a person who is not the owner is offered a new account, not merged into the owner');
    // The wizard fits the window at any size: the title and progress stay on top, Next/Back and Skip stay at the bottom, and only the content
    // between them scrolls (no scrollbar on the dialog itself).
    const wizardFit = () => first.evaluate(async () => {
      const { ImportV1Wizard } = await import('/js/import-v1.js');
      const original = window.fetch;
      const json = body => new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
      const projects = Object.fromEntries(Array.from({ length: 30 }, (_, i) => [`p${i}`, { name: `Project ${i}`, folder: `/tmp/p${i}` }]));
      const source = { source_key: 'v1-fit', rooms: [], humans: [], memory_projects: [], macros: {}, counts: { rooms: 0, messages: 0, attachments: 0, memory: 0, macros: 0 }, destination_users: ['owner'], roles: [], fleets: [],
        suggested: { source_key: 'v1-fit', owner_login: 'owner', projects, rooms: {}, humans: {}, memory_projects: {}, enable_project_memory: {}, skip_profiles: false, skip_shared_notes: false } };
      window.fetch = async (path, options) => String(path) === '/api/import-v1/preview' ? json(source) : String(path) === '/api/macros' ? json({}) : original(path, options);
      const wizard = new ImportV1Wizard();
      try {
        localStorage.removeItem('aimebu2.import-draft.v1-fit');
        await wizard.open();
        await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
        const dialog = wizard.dialog, box = dialog.getBoundingClientRect(), inside = element => { const rect = element.getBoundingClientRect(); return rect.top >= box.top - 1 && rect.bottom <= box.bottom + 1 && rect.bottom <= innerHeight + 1 && rect.top >= -1; };
        const next = [...wizard.actions.querySelectorAll('button')].find(button => button.textContent === 'Next');
        return { dialogInWindow: box.top >= 0 && box.bottom <= innerHeight + 1, dialogNotScrolling: dialog.scrollHeight <= dialog.clientHeight + 1, titleVisible: inside(wizard.heading), progressVisible: inside(wizard.progress),
          contentScrolls: wizard.content.scrollHeight > wizard.content.clientHeight, nextVisible: inside(next), skipVisible: inside(wizard.skipZone) && wizard.skipZone.children.length > 0 };
      } finally { window.fetch = original; localStorage.removeItem('aimebu2.import-draft.v1-fit'); wizard.dialog.close(); wizard.dialog.remove(); }
    });
    const fitting = { dialogInWindow: true, dialogNotScrolling: true, titleVisible: true, progressVisible: true, contentScrolls: true, nextVisible: true, skipVisible: true };
    for (const [width, height] of [[390, 640], [1100, 720], [1280, 480]]) {
      await first.setViewport({ width, height });
      assert.deepEqual(await wizardFit(), fitting, `the import wizard fits a ${width}x${height} window`);
    }
    await first.setViewport({ width: 1100, height: 720 });
    assert.deepEqual(phoneWizard, { columns: 1, dialogFits: true, contentFits: true, pageFits: true, selectsFit: true, treeBelow: true }, 'import wizard Rooms page at 390px');
    // A failed status poll during the import leaves the modal with Retry and Reload, not stuck.
    const pollRecovery = await first.evaluate(async () => {
      const { ImportV1Wizard } = await import('/js/import-v1.js');
      const wizard = new ImportV1Wizard();
      const original = window.fetch; let attempts = 0;
      window.fetch = async (path, options) => {
        if (String(path) === '/api/import-v1/jobs/gone') {
          attempts++;
          if (attempts === 1) return new Response(JSON.stringify({ error: 'not_found', message: 'import job not found' }), { status: 404, headers: { 'Content-Type': 'application/json' } });
          return new Response(JSON.stringify({ phase: 'done', report: { imported: { rooms: 1 }, already_present: {}, skipped: {} } }), { status: 200, headers: { 'Content-Type': 'application/json' } });
        }
        return original(path, options);
      };
      try {
        wizard.dialog.showModal(); wizard.phase = 'import'; wizard.jobID = 'gone'; wizard.render();
        await wizard.poll();
        const failed = { status: wizard.status.textContent, error: wizard.error.textContent, buttons: [...wizard.actions.querySelectorAll('button')].map(button => button.textContent) };
        [...wizard.actions.querySelectorAll('button')].find(button => button.textContent === 'Retry').click();
        await new Promise(resolve => { const watch = setInterval(() => { if (/complete/.test(wizard.status.textContent)) { clearInterval(watch); resolve(); } }, 20); setTimeout(resolve, 5000); });
        return { failed, recovered: wizard.status.textContent, attempts };
      } finally { window.fetch = original; wizard.dialog.close(); }
    });
    assert.deepEqual(pollRecovery, { failed: { status: 'Import status unavailable', error: 'import job not found. The import may still be running or may have finished.', buttons: ['Retry', 'Reload'] }, recovered: 'v1 import complete', attempts: 2 }, `poll recovery: ${JSON.stringify(pollRecovery)}`);
    // A modified backup makes the status route answer 409 backup_changed: the wizard still opens, says why, offers Skip, and
    // "Decide later" leaves the choice open.
    const changedBackup = await first.evaluate(async () => {
      const { ImportV1Wizard } = await import('/js/import-v1.js');
      const wizard = new ImportV1Wizard();
      const original = window.fetch;
      const posts = [];
      window.fetch = async (path, options) => {
        if (String(path) === '/api/import-v1/status') return new Response(JSON.stringify({ error: 'backup_changed', message: 'the v1 backup was modified after it was made' }), { status: 409, headers: { 'Content-Type': 'application/json' } });
        if (String(path) === '/api/import-v1/skip') { posts.push(options?.method); return new Response(null, { status: 204 }); }
        return original(path, options);
      };
      try {
        const opened = await wizard.maybeOpen(undefined, () => true);
        const first = { opened, open: wizard.dialog.open, text: wizard.status.textContent, buttons: [...wizard.actions.querySelectorAll('button')].map(button => button.textContent) };
        [...wizard.actions.querySelectorAll('button')].find(button => button.textContent === 'Decide later').click();
        const later = { closed: !wizard.dialog.open, posts: posts.length };
        await wizard.maybeOpen(undefined, () => true);
        wizard.skipZone.querySelector('.import-skip-open').click();
        [...wizard.dialog.querySelector('.import-skip-confirm').querySelectorAll('button')].find(button => button.textContent === 'Skip import forever').click();
        await new Promise(resolve => setTimeout(resolve, 0));
        return { first, later, skipped: { posts: posts.join(','), closed: !wizard.dialog.open } };
      } finally { window.fetch = original; wizard.dialog.close(); wizard.dialog.remove(); }
    });
    assert.deepEqual(changedBackup, { first: { opened: true, open: true, text: 'the v1 backup was modified after it was made', buttons: ['Decide later'] }, later: { closed: true, posts: 0 }, skipped: { posts: 'POST', closed: true } }, 'a modified v1 backup is explained in the wizard with Skip and Decide later');
    const answerControlsResult = await first.evaluate(async () => {
      const { answerControls } = await import('/js/answer-controls.js');
      const sent = [];
      const buttons = answerControls({ proposed_answers: ['Proceed', 'Hold'], free_text: true }, async value => { sent.push(value); });
      document.body.append(buttons);
      buttons.querySelector('textarea').value = 'Another path';
      buttons.querySelector('form').requestSubmit();
      await new Promise(resolve => setTimeout(resolve, 0));
      const questionItem = { id: 'oq-draft', open_questions: [
        { question: 'Scope?', options: ['Small', 'Large'] },
        { question: 'When?', options: ['Now', 'Later'] },
      ] };
      const questions = answerControls(questionItem, async value => { sent.push(value); });
      document.body.append(questions);
      const trigger = questions.querySelector('button');
      const countAtStart = trigger.querySelector('.open-questions-count').textContent;
      trigger.click();
      let dialog = document.querySelector('.open-questions-dialog');
      const visibleQuestions = () => [...dialog.querySelectorAll('.open-question-sheet')].filter(sheet => !sheet.hidden).length;
      const firstView = { visible: visibleQuestions(), subtitle: dialog.querySelector('.open-questions-subtitle').textContent, sendHidden: dialog.querySelector('.open-questions-send').hidden, reviewDisabled: dialog.querySelector('.open-questions-review-step').disabled };
      dialog.querySelector('input[name="question-0"]').click();
      const afterFirst = { progress: dialog.querySelector('.open-questions-progress').textContent, triggerCount: trigger.querySelector('.open-questions-count').textContent, answeredStep: dialog.querySelector('.open-questions-step').classList.contains('answered') };
      // Submitting with an unanswered question must not send; it jumps there.
      dialog.querySelector('form').requestSubmit();
      const requiredAll = sent.length === 1;
      const jumped = dialog.querySelector('.open-questions-subtitle').textContent;
      // Closing keeps the draft: reopen and resume at the first unanswered one.
      await new Promise(resolve => { dialog.addEventListener('close', () => setTimeout(resolve, 0), { once: true }); dialog.close(); });
      const closedRemoved = !dialog.isConnected;
      trigger.click();
      dialog = document.querySelector('.open-questions-dialog');
      const resumed = { subtitle: dialog.querySelector('.open-questions-subtitle').textContent, kept: dialog.querySelector('input[name="question-0"]').checked };
      const other = dialog.querySelector('input[name="question-1"][value="other"]');
      other.click();
      const emptyOther = dialog.querySelector('.open-questions-progress').textContent;
      other.closest('label').querySelector('input[type=text]').value = 'Next week';
      other.closest('label').querySelector('input[type=text]').dispatchEvent(new Event('input', { bubbles: true }));
      const allAnswered = dialog.querySelector('.open-questions-progress').textContent;
      dialog.querySelector('.open-questions-next').click();
      const review = { subtitle: dialog.querySelector('.open-questions-subtitle').textContent, sendShown: !dialog.querySelector('.open-questions-send').hidden, items: dialog.querySelectorAll('.open-questions-review li').length };
      dialog.querySelector('form').requestSubmit();
      await new Promise(resolve => setTimeout(resolve, 0));
      buttons.remove(); questions.remove();
      return { sent, requiredAll, dialogClosed: !dialog.open, countAtStart, firstView, afterFirst, jumped, closedRemoved, resumed, emptyOther, allAnswered, review };
    });
    assert.deepEqual(answerControlsResult, {
      sent: ['Another path', 'Q1) a) Small\nQ2) other) Next week'], requiredAll: true, dialogClosed: true, countAtStart: '0/2',
      firstView: { visible: 1, subtitle: 'Question 1 of 2', sendHidden: true, reviewDisabled: true },
      afterFirst: { progress: '1/2 answered', triggerCount: '1/2', answeredStep: true },
      jumped: 'Question 2 of 2', closedRemoved: true,
      resumed: { subtitle: 'Question 2 of 2', kept: true },
      emptyOther: '1/2 answered', allAnswered: '2/2 answered',
      review: { subtitle: 'Review 2/2 answers', sendShown: true, items: 2 },
    });
    // Open Questions on a phone: full screen, heading inside the border, 40px targets, the action bar
    // stays on screen with long content, and tapping the Other box chooses Other.
    await first.setViewport({ width: 390, height: 780 });
    const phoneQuestions = await first.evaluate(async () => {
      const { answerControls } = await import('/js/answer-controls.js');
      const options = Array.from({ length: 8 }, (_, index) => `Option ${index + 1}: a deliberately long label that wraps across more than one line on a narrow phone screen`);
      const host = answerControls({ id: 'oq-phone', open_questions: [
        { question: 'Which rollout approach should we use for the production database migration this quarter?', description: 'A long description. '.repeat(30), options },
        { question: 'When?', options: ['Now', 'Later'] },
      ] }, async () => {});
      document.body.append(host);
      host.querySelector('.open-questions-open').click();
      const dialog = [...document.querySelectorAll('.open-questions-dialog')].pop();
      if (!dialog) throw new Error('Open Questions dialog did not open');
      const rect = node => node.getBoundingClientRect();
      const sheet = dialog.querySelector('.open-question-sheet:not([hidden])');
      const legend = sheet.querySelector('legend');
      const otherText = sheet.querySelector('input[type=text]');
      const otherRadio = sheet.querySelector('input[value="other"]');
      const targets = [...dialog.querySelectorAll('.open-questions-step, .open-questions-actions button:not([hidden])')].map(node => ({ name: node.className, w: rect(node).width, h: rect(node).height }));
      const rows = [...sheet.querySelectorAll('label')].map(label => rect(label).height);
      const legendBox = [rect(legend).top, rect(legend).bottom, rect(sheet).top, rect(sheet).bottom];
      const before = { disabled: otherText.disabled, checked: otherRadio.checked };
      otherText.focus();
      const afterFocus = { checked: otherRadio.checked, answered: dialog.querySelector('.open-questions-progress').textContent };
      otherText.value = 'typed'; otherText.dispatchEvent(new Event('input', { bubbles: true }));
      const afterTyping = dialog.querySelector('.open-questions-progress').textContent;
      const box = rect(dialog), actions = rect(dialog.querySelector('.open-questions-actions'));
      const result = {
        full: { left: box.left, top: box.top, width: box.width, height: box.height, vw: innerWidth, vh: innerHeight },
        legendInside: legendBox[0] > legendBox[2] && legendBox[1] < legendBox[3], legendBox,
        actionsBottom: actions.bottom, actionsTop: actions.top, sheetScrolls: sheet.scrollHeight > sheet.clientHeight,
        targets, rows, before, afterFocus, afterTyping,
        closeQuiet: dialog.querySelector('.open-questions-close').classList.contains('btn-primary') === false, backQuiet: dialog.querySelector('.open-questions-back').classList.contains('btn-primary') === false, nextQuiet: dialog.querySelector('.open-questions-next').classList.contains('btn-primary') === false,
      };
      dialog.close(); host.remove();
      sessionStorage.removeItem('aimebu2.oq.oq-phone');
      return result;
    });
    await first.setViewport({ width: 1100, height: 720 });
    assert.ok(Math.abs(phoneQuestions.full.width - phoneQuestions.full.vw) < 1 && Math.abs(phoneQuestions.full.height - phoneQuestions.full.vh) < 1 && Math.abs(phoneQuestions.full.left) < 1 && Math.abs(phoneQuestions.full.top) < 1, `Open Questions is not full screen on a phone: ${JSON.stringify(phoneQuestions.full)}`);
    assert.equal(phoneQuestions.legendInside, true, `question heading straddles the fieldset border: ${JSON.stringify(phoneQuestions.legendBox)}`);
    assert.ok(phoneQuestions.actionsBottom <= phoneQuestions.full.vh + 0.5 && phoneQuestions.sheetScrolls, `action bar left the screen or long content did not scroll: ${JSON.stringify(phoneQuestions)}`);
    assert.ok(phoneQuestions.targets.every(target => target.h >= 39.5) && phoneQuestions.rows.every(height => height >= 39.5), `Open Questions targets under 40px: ${JSON.stringify([phoneQuestions.targets, phoneQuestions.rows])}`);
    assert.deepEqual([phoneQuestions.before.disabled, phoneQuestions.before.checked, phoneQuestions.afterFocus.checked, phoneQuestions.afterFocus.answered, phoneQuestions.afterTyping], [false, false, true, '0/2 answered', '1/2 answered'], 'tapping Other did not choose it');
    assert.deepEqual([phoneQuestions.closeQuiet, phoneQuestions.backQuiet, phoneQuestions.nextQuiet], [true, true, false], 'Close and Back must be secondary, Next primary');
    const stableAnswerControls = await first.evaluate(async () => {
      const { Chat } = await import('/js/chat.js');
      const chat = Object.create(Chat.prototype);
      chat.nodes = new Map(); chat.threadList = document.createElement('div'); chat.inbox = { answer: async () => true };
      chat.authorMeta = () => document.createElement('span');
      chat.renderBody = (node, message) => { node.textContent = message.body; };
      chat.legacyDetails = () => null;
      const item = { id: 'inb-draft', message_id: 'msg-draft', kind: 'question', rev: 0,
        proposed_answers: ['Proceed', 'Hold'], free_text: true, can_decide: true };
      chat.answerItems = new Map([[item.message_id, item]]);
      const roomNode = document.createElement('article');
      const bubble = document.createElement('div'); bubble.className = 'message-bubble'; roomNode.append(bubble);
      chat.nodes.set(item.message_id, roomNode);
      const threadNode = chat.threadEntry({ id: item.message_id, seq: 1, no: 1, body: 'Choose' });
      chat.threadList.append(threadNode);
      chat.updateAnswerControls([item]);
      const roomControls = bubble.querySelector('.answer-controls');
      const threadControls = threadNode.querySelector('.answer-controls');
      roomControls.querySelector('textarea').value = 'Room draft';
      threadControls.querySelector('textarea').value = 'Thread draft';
      chat.updateAnswerControls([{ ...item }]);
      const kept = bubble.querySelector('.answer-controls') === roomControls && threadNode.querySelector('.answer-controls') === threadControls
        && roomControls.querySelector('textarea').value === 'Room draft' && threadControls.querySelector('textarea').value === 'Thread draft';
      chat.updateAnswerControls([{ ...item, rev: 1 }]);
      const replaced = bubble.querySelector('.answer-controls') !== roomControls && threadNode.querySelector('.answer-controls') !== threadControls;
      chat.updateAnswerControls([]);
      const removed = !bubble.querySelector('.answer-controls') && !threadNode.querySelector('.answer-controls');
      // The answered question keeps a quiet "Answered" (once, in the room and the thread), including after the message is drawn again.
      const marker = node => [...node.querySelectorAll(':scope > .answer-done button')].map(button => [button.textContent, button.disabled]);
      const answered = { room: marker(bubble), thread: marker(threadNode) };
      chat.syncAnswerControl(bubble, undefined, item.message_id); chat.updateAnswerControls([]);
      const stillOnce = marker(bubble).length;
      const fresh = document.createElement('div'); fresh.className = 'message-bubble'; chat.syncAnswerControl(fresh, undefined, item.message_id);
      const redrawn = marker(fresh).length;
      chat.updateAnswerControls([{ ...item, rev: 2 }]);
      const reopened = { controls: !!bubble.querySelector('.answer-controls'), marker: marker(bubble).length };
      chat.answeredMessages = new Set(); const other = document.createElement('div'); chat.syncAnswerControl(other, undefined, 'msg-never-open');
      return { kept, replaced, removed, answered, stillOnce, redrawn, reopened, neverOpen: marker(other).length };
    });
    assert.deepEqual(stableAnswerControls, { kept: true, replaced: true, removed: true, answered: { room: [['Answered', true]], thread: [['Answered', true]] }, stillOnce: 1, redrawn: 1, reopened: { controls: true, marker: 0 }, neverOpen: 0 },
      'an answered question keeps a disabled "Answered", once; a reopened one swaps it for its buttons; a message that was never open gets none');
    // Proposed answers and open questions stay visible under their message once nothing is open for them (imported or old asks): disabled,
    // "Answered" when a person replied to it or it was answered here, otherwise "No longer pending"; nothing for a message with none.
    const askHistory = await first.evaluate(async () => {
      const { Chat } = await import('/js/chat.js');
      const chat = Object.create(Chat.prototype);
      chat.nodes = new Map(); chat.threadList = document.createElement('div'); chat.inbox = { answer: async () => true }; chat.answerItems = new Map();
      chat.answeredMessages = new Set(); chat.repliedTo = new Set();
      const asked = { id: 'ask-1', seq: 1, no: 1, proposed_answers: ['Proceed', 'Hold'], open_questions: [{ question: 'Which?', options: ['a', 'b'] }] };
      chat.messages = new Map([[asked.id, asked], ['plain', { id: 'plain', seq: 2, no: 2, body: 'hi' }]]);
      const read = node => ({ buttons: [...node.querySelectorAll('.answer-history button')].map(button => [button.textContent.trim(), button.disabled, button.title]), status: node.querySelector('.answer-history-status')?.textContent || '', blocks: node.querySelectorAll('.answer-history').length });
      const bubble = document.createElement('div'); document.body.append(bubble);
      chat.syncAnswerControl(bubble, undefined, 'ask-1');
      const pending = read(bubble); const block = bubble.querySelector('.answer-history');
      chat.syncAnswerControl(bubble, undefined, 'ask-1');
      const same = bubble.querySelector('.answer-history') === block;
      // A person replies to it: the next sync says Answered.
      chat.repliedTo.add('ask-1'); chat.syncAnswerControl(bubble, undefined, 'ask-1');
      const answered = read(bubble);
      // It becomes open again: the live buttons replace the history.
      chat.syncAnswerControl(bubble, { id: 'inb', message_id: 'ask-1', kind: 'question', rev: 0, proposed_answers: ['Proceed'], free_text: true, can_decide: true }, 'ask-1');
      const reopened = { history: bubble.querySelectorAll('.answer-history').length, live: bubble.querySelectorAll('.answer-controls').length };
      const none = document.createElement('div'); chat.syncAnswerControl(none, undefined, 'plain');
      bubble.remove();
      return { pending, same, answered, reopened, plain: none.querySelectorAll('.answer-history, .answer-done').length };
    });
    assert.deepEqual(askHistory.pending, { buttons: [['Open Questions 1', true, 'No longer pending'], ['Proceed', true, 'No longer pending'], ['Hold', true, 'No longer pending']], status: 'No longer pending', blocks: 1 }, 'an ask with nothing open shows its buttons, disabled and marked no longer pending');
    assert.equal(askHistory.same, true, 'a sync that changes nothing keeps the same block');
    assert.deepEqual([askHistory.answered.status, askHistory.answered.buttons.every(([, disabled, title]) => disabled && title === 'Answered'), askHistory.answered.blocks], ['Answered', true, 1], 'a replied-to ask says Answered');
    assert.deepEqual([askHistory.reopened, askHistory.plain], [{ history: 0, live: 1 }, 0], 'an open ask swaps the history for live buttons; a message without an ask gets nothing');
    // A thumbnail with a known size keeps its shape: a panorama and a portrait are scaled into the box, not letterboxed in it.
    const thumbs = await first.evaluate(async () => {
      const picture = (width, height) => 'data:image/svg+xml,' + encodeURIComponent(`<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}"><rect width="${width}" height="${height}" fill="gray"/></svg>`);
      const host = document.createElement('div'); host.className = 'message-images'; document.body.append(host);
      const make = (width, height) => { const button = document.createElement('button'); button.className = 'row-btn image-thumb'; const img = document.createElement('img'); img.width = width; img.height = height; img.src = picture(width, height); button.append(img); host.append(button); return img; };
      const wide = make(2000, 400), tall = make(300, 1200);
      await Promise.all([wide.decode(), tall.decode()]);
      const out = { wide: [wide.getBoundingClientRect().width, wide.getBoundingClientRect().height], tall: [tall.getBoundingClientRect().width, tall.getBoundingClientRect().height] };
      host.remove(); return out;
    });
    assert.ok(Math.abs(thumbs.wide[0] / thumbs.wide[1] - 5) < 0.2 && thumbs.wide[0] <= 220.5, `a panorama thumbnail keeps its 5:1 shape: ${JSON.stringify(thumbs)}`);
    assert.ok(Math.abs(thumbs.tall[0] / thumbs.tall[1] - 0.25) < 0.02 && thumbs.tall[1] <= 180.5, `a portrait thumbnail keeps its 1:4 shape: ${JSON.stringify(thumbs)}`);
    const nativeHistoryLayout = await first.evaluate(async () => {
      const { Chat } = await import('/js/chat.js');
      const chat = Object.create(Chat.prototype);
      chat.list = document.createElement('div'); document.body.append(chat.list);
      chat.messages = new Map(); chat.nodes = new Map(); chat.threadCounts = new Map(); chat.agents = new Map(); chat.raw = false;
      chat.refreshReactions = () => {};
      const visual_plan = [{ type: 'future', data: { text: '<img src=x onerror=alert(1)>' } }];
      chat.add({ id: '123', seq: 123, no: 123, room_id: 'room_test', author_kind: 'agent', author_id: 'bot@repo',
        author_harness: 'codex', author_model: 'gpt-6-sol', body: 'Historical body', created_at: '2026-10-14T00:00:00Z', visual_plan });
      const article = chat.list.firstElementChild;
      const result = { label: article.querySelector('.message-author')?.textContent,
        planInBubble: Boolean(article.querySelector('.message-bubble > .visual-plan')),
        escaped: article.querySelector('.visual-plan-raw-fallback')?.textContent.includes('<img src=x onerror=alert(1)>'),
        injected: Boolean(article.querySelector('.visual-plan img')) };
      result.departed = chat.authorName({ author_kind: 'agent', author_id: 'agt_01ABCDEFGH123456' });
      chat.list.remove(); return result;
    });
    assert.deepEqual(nativeHistoryLayout, { label: 'bot@repo', planInBubble: true, escaped: true, injected: false, departed: 'Agent 123456' });
    const identityHeader = await first.evaluate(async () => {
      const { Chat } = await import('/js/chat.js');
      const chat = Object.create(Chat.prototype);
      chat.list = document.createElement('div'); document.body.append(chat.list);
      chat.messages = new Map(); chat.nodes = new Map(); chat.threadCounts = new Map(); chat.serverThreadCounts = new Map(); chat.raw = false;
      chat.agents = new Map([['live', { id: 'live', name: 'Live', tool: 'codex', model: 'gpt-6-sol', observed: 'in_turn' }], ['gone', { id: 'gone', name: 'Gone', tool: 'claude', model: 'opus', observed: 'stopped' }], ['wait', { id: 'wait', name: 'Wait', tool: 'pi', model: 'gemma', observed: 'idle', recovery_block: true }]]);
      chat.refreshReactions = () => {};
      const base = { room_id: 'room_test', body: 'hi', created_at: '2026-10-14T00:00:00Z' };
      chat.add({ ...base, id: 'i1', seq: 901, no: 901, author_kind: 'agent', author_id: 'live' });
      chat.add({ ...base, id: 'i2', seq: 902, no: 902, author_kind: 'agent', author_id: 'gone' });
      chat.add({ ...base, id: 'i3', seq: 903, no: 903, author_kind: 'agent', author_id: 'wait' });
      chat.add({ ...base, id: 'i4', seq: 904, no: 904, author_kind: 'agent', author_id: 'missing' });
      chat.add({ ...base, id: 'i5', seq: 905, no: 905, author_kind: 'user', author_id: 'someone', needs_attention: true });
      chat.add({ ...base, id: 'i6', seq: 906, no: 906, author_kind: 'user', author_id: 'someone' });
      chat.add({ ...base, id: 'i7', seq: 907, no: 907, author_kind: 'agent', author_id: 'agt_01GONE0000', author_model: 'opus4.8', author_harness: 'claude-code' });
      chat.add({ ...base, id: 'i8', seq: 908, no: 908, author_kind: 'agent', author_id: 'imported-bot', author_model: 'sonnet4.6', author_harness: 'codex' });
      const read = id => { const el = chat.list.querySelector(`[data-message-id="${id}"]`); const dot = el.querySelector('.msg-sender-dot'); const icon = el.querySelector('.msg-sender-icon');
        return { icon: icon.getAttribute('src'), title: icon.title, dot: dot.hidden ? null : [dot.className.replace('msg-sender-dot ', ''), dot.title], bell: Boolean(el.querySelector('.message-attention')) }; };
      const out = { live: read('i1'), stopped: read('i2'), wait: read('i3'), historical: read('i4'), attention: read('i5'), plain: read('i6'), departed: read('i7'), importedKnown: read('i8') };
      chat.agents.get('live').observed = 'lost'; chat.updateAuthor(chat.list.querySelector('[data-message-id="i1"]'), chat.messages.get('i1'));
      out.liveAfter = read('i1').dot;
      const number = chat.list.querySelector('[data-message-id="i5"] .message-number');
      out.numberText = number.textContent; number.click(); await new Promise(resolve => setTimeout(resolve, 200));
      out.copied = number.classList.contains('copied');
      // The name of a sender with no live record opens a read-only profile; a live sender's name does nothing.
      const author = id => chat.list.querySelector(`[data-message-id="${id}"] .message-author`);
      out.links = ['i1', 'i4', 'i7'].map(id => [author(id).classList.contains('message-author-link'), author(id).getAttribute('role')]);
      author('i8').click(); await new Promise(resolve => setTimeout(resolve, 100));
      const dialog = document.querySelector('#sender-profile-dialog');
      out.profile = dialog && { open: dialog.open, title: dialog.querySelector('h2').textContent, badge: dialog.querySelector('.sender-profile-badge').textContent, facts: [...dialog.querySelectorAll('.profile-fact')].map(row => row.textContent.replace(/\s+/g, ' ').trim()), note: dialog.querySelector('.sender-profile-note').textContent.slice(0, 22) };
      dialog?.querySelector('.sender-profile-close').click(); await new Promise(resolve => setTimeout(resolve, 50));
      out.closed = !document.querySelector('#sender-profile-dialog');
      author('i1').click(); await new Promise(resolve => setTimeout(resolve, 50)); out.liveOpens = Boolean(document.querySelector('#sender-profile-dialog'));
      chat.list.remove(); return out;
    });
    assert.deepEqual(identityHeader.links, [[false, null], [true, 'button'], [true, 'button']], 'only senders with no live record have a clickable name');
    assert.deepEqual([identityHeader.profile.open, identityHeader.profile.title, identityHeader.profile.badge, identityHeader.profile.note, identityHeader.closed, identityHeader.liveOpens], [true, 'imported-bot', 'Historical (imported)', 'This sender comes from', true, false]);
    assert.ok(identityHeader.profile.facts.some(fact => /^Runtime\s*sonnet4\.6 · Codex$/.test(fact)) && identityHeader.profile.facts.some(fact => /^Messages loaded\s*1$/.test(fact)) && identityHeader.profile.facts.some(fact => /^Last heard\s*\S/.test(fact)), `the profile shows what is known: ${JSON.stringify(identityHeader.profile.facts)}`);
    assert.deepEqual(identityHeader.live, { icon: '/icons/codex.svg', title: 'gpt-6-sol · Codex', dot: ['dot-active', 'working'], bell: false });
    assert.deepEqual(identityHeader.stopped, { icon: '/icons/claude-code.svg', title: 'opus · Claude Code', dot: ['dot-offline', 'stopped'], bell: false });
    assert.deepEqual(identityHeader.wait, { icon: '/icons/pi.svg', title: 'gemma · Pi', dot: ['dot-stale', 'needs recovery'], bell: false });
    assert.deepEqual(identityHeader.historical, { icon: '/icons/unknown.svg', title: 'unknown (historical)', dot: ['dot-historical', 'Imported from v1 (no live agent)'], bell: false }, 'a sender known only from imported history has the departed dot, and its tooltip says why');
    assert.deepEqual(identityHeader.departed, { icon: '/icons/claude-code.svg', title: 'opus4.8 · Claude Code (departed)', dot: ['dot-departed', 'departed'], bell: false }, 'an agent that was in a v2 room and left keeps the red departed dot');
    assert.deepEqual(identityHeader.importedKnown.dot, ['dot-historical', 'Imported from v1 (no live agent)']);
    assert.deepEqual(identityHeader.attention, { icon: '/icons/human.svg', title: 'human', dot: null, bell: true });
    assert.equal(identityHeader.plain.bell, false);
    assert.deepEqual(identityHeader.liveAfter, ['dot-offline', 'lost']);
    assert.equal(identityHeader.numberText, '#905');
    // A message's actions: one compact toolbar (react, reply, thread, copy text, copy link).
    const messageActions = await first.evaluate(async () => {
      const { Chat } = await import('/js/chat.js');
      const out = {};
      for (const variant of ['c']) {
        const chat = Object.create(Chat.prototype);
        chat.list = document.createElement('div'); document.body.append(chat.list);
        chat.messages = new Map(); chat.nodes = new Map(); chat.threadCounts = new Map(); chat.serverThreadCounts = new Map(); chat.agents = new Map(); chat.raw = false;
        chat.refreshReactions = () => {}; chat.toggleReaction = (id, emoji) => { (chat.reacted ||= []).push([id, emoji]); };
        chat.list.addEventListener('keydown', event => chat.moveActionFocus(event));
        const base = { room_id: 'room_test', created_at: '2026-10-14T00:00:00Z', author_kind: 'user', author_id: 'someone' };
        chat.add({ ...base, id: 'a1', seq: 801, no: 801, body: '**raw** body' });
        const one = chat.list.querySelector('.message');
        const bar = one.querySelector('.message-actions');
        const writes = []; const original = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
        Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async value => { writes.push(value); } } });
        const buttons = [...bar.querySelectorAll(':scope > .message-action')];
        const result = { labels: buttons.map(button => button.getAttribute('aria-label')), role: bar.getAttribute('role'), title: buttons.every(button => button.title && button.matches('.icon-btn')), text: buttons.map(button => button.textContent), menu: [...bar.querySelectorAll('.message-menu-item')].map(item => item.textContent), emoji: [...bar.querySelectorAll('.message-reaction-row .quick-reaction')].map(button => button.textContent), threadChipHidden: one.querySelector(':scope > .thread-chip').hidden, oldTextButtons: one.querySelectorAll('.message-actions button.quiet:not(.icon-btn):not(.quick-reaction):not(.message-menu-item)').length, debugText: one.querySelector('.chat-msg-debug-toggle')?.textContent, debugInHeader: !!one.querySelector('.message-meta .chat-msg-debug-toggle') };
        // React opens the inline emoji row, a pick reacts and closes it, and Escape closes it too.
        const react = bar.querySelector('.message-react'); react.click();
        result.rowOpen = !bar.querySelector('.message-reaction-row').hidden && react.getAttribute('aria-expanded') === 'true';
        bar.querySelector('.quick-reaction').click(); result.reacted = chat.reacted; result.rowClosed = bar.querySelector('.message-reaction-row').hidden;
        react.click(); document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true })); result.escapeCloses = bar.querySelector('.message-reaction-row').hidden && react.getAttribute('aria-expanded') === 'false';
        // Copy text and link are toolbar buttons.
        { for (const button of bar.querySelectorAll('.message-copy')) { button.click(); await new Promise(resolve => setTimeout(resolve, 30)); } }
        // Arrow keys move along the toolbar.
        buttons[0].focus(); buttons[0].dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true })); result.arrowRight = document.activeElement === buttons[1];
        buttons[0].focus(); buttons[0].dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft', bubbles: true })); result.arrowWrapLastLabel = document.activeElement.getAttribute('aria-label'); result.arrowWrap = document.activeElement === buttons.at(-1);
        buttons[0].focus(); buttons[0].dispatchEvent(new KeyboardEvent('keydown', { key: 'End', bubbles: true })); result.endLabel = document.activeElement.getAttribute('aria-label'); buttons[1].focus(); buttons[1].dispatchEvent(new KeyboardEvent('keydown', { key: 'Home', bubbles: true })); result.homeLabel = document.activeElement.getAttribute('aria-label');
        if (original) Object.defineProperty(navigator, 'clipboard', original); else delete navigator.clipboard;
        result.writes = writes;
        out[variant] = result;
        chat.list.remove();
      }
      return out;
    });
    assert.deepEqual(messageActions.c.labels, ['Add reaction', 'Find another reaction', 'Reply to #801', 'Reply in thread', 'Copy message text', 'Copy link to #801'], 'one icon toolbar');
    for (const variant of ['c']) {
      const result = messageActions[variant];
      assert.equal(result.role, 'toolbar'); assert.equal(result.title, true, `${variant}: every action is a framed icon button with a tooltip`);
      assert.deepEqual(result.text.every(text => text === ''), true, `${variant}: the action buttons carry icons, not text`);
      assert.deepEqual(result.emoji, ['👍', '🆗', '✅', '👀', '🙏', '🎉']);
      assert.deepEqual([result.rowOpen, result.rowClosed, result.escapeCloses], [true, true, true], `${variant}: the emoji row opens, closes on a pick and on Escape`);
      assert.deepEqual(result.reacted, [['a1', '👍']]);
      assert.deepEqual([result.oldTextButtons, result.debugText, result.debugInHeader, result.threadChipHidden], [0, '', true, true], `${variant}: no text Debug or inline emoji strip; the bug icon is in the header line`);
      assert.equal(result.arrowRight, true);
      assert.deepEqual([result.arrowWrapLastLabel, result.endLabel, result.homeLabel, result.arrowWrap], ['Copy link to #801', 'Copy link to #801', 'Add reaction', true], 'with the emoji row closed, the arrow keys, End and Home move over the six toolbar buttons only');
    }
    assert.deepEqual(messageActions.c.writes, ['**raw** body', '#801']);
    const prependedThreadOrder = await first.evaluate(async () => {
      const { Chat } = await import('/js/chat.js');
      const chat = Object.create(Chat.prototype);
      chat.threadList = document.createElement('div'); chat.threadCounts = new Map(); chat.serverThreadCounts = new Map(); chat.nodes = new Map(); chat.messages = new Map(); chat.agents = new Map(); chat.raw = false;
      const root = { id: 'thread-root', seq: 1, no: 1, body: 'root', author_kind: 'user', author_id: 'owner' };
      chat.threadRoot = root; chat.threadEntryIDs = new Set([root.id]); chat.threadList.append(chat.threadEntry(root));
      for (const seq of [4, 3, 2]) chat.add({ id: `reply-${seq}`, seq, thread_root: root.id, body: `reply ${seq}`, author_kind: 'user', author_id: 'owner' }, true);
      return [...chat.threadList.children].map(entry => Number(entry.dataset.seq));
    });
    assert.deepEqual(prependedThreadOrder, [1, 2, 3, 4], 'older replies arrived out of order in an open thread');
    assert.equal(await first.evaluate(() => Boolean(document.querySelector('script[src="/vendor/mermaid.min.js"]'))), false, 'Mermaid stays unloaded without diagrams');
    const planSafety = await first.evaluate(async () => {
      const { renderVisualPlan } = await import('/js/visual-plan.js');
      const attack = '<img src=x onerror="window.__planXss=1"><script>window.__planXss=1</script>';
      const samples = [
        ['markdown', { markdown: attack }], ['file-tree', { root: { name: attack, note: attack } }],
        ['data-model', { entities: [{ name: attack, fields: [{ name: attack, type: attack, notes: attack }] }] }],
        ['api-endpoint', { method: attack, path: attack }], ['annotated-code', { code: attack, annotations: [{ text: attack }] }],
        ['diff', { diff: attack }], ['checklist', { items: [{ text: attack }] }],
        ['question-form', { questions: [{ question: attack, description: attack, options: [attack] }] }],
        ['diagram', { mermaid: attack }], ['canvas', { nodes: [{ label: attack, x: 1 }] }],
        ['prototype', { screens: [{ title: attack, elements: [{ text: attack, x: 1 }] }] }],
      ];
      const blocks = samples.map(([type, data]) => ({ type, title: attack, data }));
      blocks.push({ type: 'future', data: { payload: attack } }, { type: 'markdown', data: { unsupported: attack } },
        { type: 'canvas', data: { nodes: [{ label: attack, x: attack }] } },
        { type: 'prototype', data: { screens: [{ elements: [{ text: attack, x: attack }] }] } });
      const node = renderVisualPlan({ visual_plan: blocks, appendix_pages: [{ title: attack, body: attack }] });
      document.body.append(node);
      const result = { blocks: node.querySelectorAll('.visual-plan-block').length,
        raw: node.querySelectorAll('.visual-plan-raw-fallback').length, scripts: node.querySelectorAll('script,img,iframe,[onerror],[onclick]').length,
        text: node.textContent.includes(attack), appendixClosed: !node.querySelector('.visual-plan-appendix').open, executed: Boolean(window.__planXss) };
      node.remove(); return result;
    });
    assert.deepEqual(planSafety, { blocks: 16, raw: 4, scripts: 0, text: true, appendixClosed: true, executed: false });
    // [group a] Nested lists, task items and rules in a message, with their styles.
    const listParity = await first.evaluate(async () => {
      const { renderMarkdown } = await import('/js/markdown.js');
      const message = document.createElement('div'); message.className = 'message-body';
      message.append(renderMarkdown('- a\n  - b\n- [x] done\n- [ ] todo\n\n---\n\n1. one\n   - sub'));
      document.body.append(message);
      const result = { nested: message.querySelectorAll('ul > li > ul > li').length, ordered: message.querySelectorAll('ol > li > ul > li').length,
        tasks: message.querySelectorAll('li.md-task input[type=checkbox][disabled]').length, checked: message.querySelectorAll('li.md-task input:checked').length,
        rules: message.querySelectorAll('hr').length, rule: getComputedStyle(message.querySelector('hr')).borderTopWidth, nestedMargin: getComputedStyle(message.querySelector('li > ul')).marginTop };
      message.remove(); return result;
    });
    assert.deepEqual(listParity, { nested: 1, ordered: 1, tasks: 2, checked: 1, rules: 1, rule: '1px', nestedMargin: '2px' }, 'nested lists, task items and a rule render, with their styles');
    // [end group a]
    // [group b] A fence that never closes is its opening line as typed, then the rest as Markdown.
    const fenceParity = await first.evaluate(async () => {
      const { renderMarkdown } = await import('/js/markdown.js');
      const message = document.createElement('div'); message.append(renderMarkdown('```js\nnever closed\n\n- after'));
      document.body.append(message);
      const result = { literal: [...message.querySelectorAll('p')].some(p => p.textContent === '```js'), blocks: message.querySelectorAll('.md-codeblock').length, list: message.querySelectorAll('ul > li').length };
      message.remove(); return result;
    });
    assert.deepEqual(fenceParity, { literal: true, blocks: 0, list: 1 }, 'an unterminated fence is literal and the rest is Markdown');
    // [end group b]
    // [group c] Plan blocks: quote line breaks, the compact code block styling, diffs, string data, and a diagram retried once.
    const planParity = await first.evaluate(async () => {
      const { renderVisualPlan } = await import('/js/visual-plan.js');
      const plan = renderVisualPlan({ visual_plan: [
        { type: 'markdown', data: { markdown: '> one\n> two\n\n```\ncode here\n```' } }, { type: 'markdown', data: 'string **data**' },
        { type: 'diff', data: { diff: '--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n keep\n-old\n+new' } } ] });
      const bubble = document.createElement('div'); bubble.className = 'message-bubble'; bubble.append(plan); document.body.append(bubble);
      const copy = plan.querySelector('.md-codeblock .md-copy');
      const result = { plan: { quote: getComputedStyle(plan.querySelector('blockquote')).whiteSpace, copyWidth: getComputedStyle(copy).width, codePad: getComputedStyle(plan.querySelector('.md-codeblock pre')).paddingRight,
        stringStrong: plan.querySelector('strong')?.textContent, added: plan.querySelectorAll('.visual-plan-diff .hl-add').length, removed: plan.querySelectorAll('.visual-plan-diff .hl-del').length, diffMarked: plan.querySelector('.visual-plan-diff code')?.classList.contains('hl-diff'),
        diffText: plan.querySelector('.visual-plan-diff code')?.textContent } };
      bubble.remove();
      // A diagram whose first render ends in mermaid's error graphic is rendered again from its source, once.
      const realMermaid = window.mermaid;
      const attempt = async mode => {
        let runs = 0;
        window.mermaid = { initialize() {}, run: async ({ nodes }) => { runs++; nodes[0].innerHTML = mode === 'recovers' && runs === 2 ? '<svg id="ok"></svg>' : '<div class="error-text">boom</div>'; } };
        const diagram = renderVisualPlan({ visual_plan: [{ type: 'diagram', data: { mermaid: 'flowchart LR\n A --> B' } }] }); document.body.append(diagram);
        for (let i = 0; i < 100 && !diagram.querySelector('svg,.visual-plan-render-error'); i++) await new Promise(resolve => setTimeout(resolve, 50));
        const out = { runs, svg: Boolean(diagram.querySelector('svg#ok')), failed: Boolean(diagram.querySelector('.visual-plan-render-error')) };
        diagram.remove(); return out;
      };
      result.recovers = await attempt('recovers'); result.fails = await attempt('fails');
      window.mermaid = realMermaid;
      return result;
    });
    assert.deepEqual([planParity.plan.quote, planParity.plan.copyWidth, planParity.plan.codePad, planParity.plan.stringStrong], ['pre-wrap', '22px', '34px', 'data'], 'plan blocks keep quote line breaks, get the compact code block styling, and take string data as Markdown');
    assert.deepEqual([planParity.plan.added, planParity.plan.removed, planParity.plan.diffMarked, planParity.plan.diffText], [1, 1, true, '--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n keep\n-old\n+new'], 'a plan diff block uses the diff renderer and keeps its text');
    assert.deepEqual([planParity.recovers, planParity.fails], [{ runs: 2, svg: true, failed: false }, { runs: 2, svg: false, failed: true }], 'a diagram is retried once: it recovers on the second run, and gives up after it');
    // A wide line in a plan diff wraps inside its block, and every line's tint spans the block's full width, short lines included.
    const wideDiff = await first.evaluate(async () => {
      const { renderVisualPlan } = await import('/js/visual-plan.js');
      const plan = renderVisualPlan({ visual_plan: [{ type: 'diff', data: { diff: `--- a/f\n+++ b/f\n@@ -1 +1 @@\n-short\n+${'long '.repeat(120)}` } }] });
      const bubble = document.createElement('div'); bubble.className = 'message-bubble'; bubble.style.width = '360px'; bubble.append(plan); document.body.append(bubble);
      const scroller = [plan.querySelector('.visual-plan-diff'), plan.querySelector('.visual-plan-diff code')].find(node => node && node.scrollWidth > node.clientWidth + 1) || plan.querySelector('.visual-plan-diff');
      const scrolled = scroller.scrollWidth;
      const lines = [...plan.querySelectorAll('.visual-plan-diff .hl-line')].map(line => Math.round(line.getBoundingClientRect().width));
      bubble.remove(); return { scrolled: Math.round(scrolled), lines };
    });
    assert.ok(wideDiff.scrolled <= 361, `the wide diff stays inside its 360px bubble: ${JSON.stringify(wideDiff)}`);
    assert.ok(wideDiff.lines.every(width => Math.abs(width - Math.max(...wideDiff.lines)) <= 1), `every diff line spans the same, full width: ${JSON.stringify(wideDiff)}`);
    // [end group c]
    // [group d] Autolinks in angle brackets, link titles, and spaces in an address.
    const linkParity = await first.evaluate(async () => {
      const { renderMarkdown } = await import('/js/markdown.js');
      const message = document.createElement('div'); message.append(renderMarkdown('<https://example.test/p> and [t](https://example.test/ "Title") and [s](https://example.test/a b) and <javascript:alert(1)>'));
      document.body.append(message);
      const result = { auto: message.querySelector('a[href="https://example.test/p"]')?.textContent, title: message.querySelector('a[title="Title"]')?.getAttribute('href'), space: message.querySelector('a[href="https://example.test/a%20b"]')?.textContent,
        links: message.querySelectorAll('a').length, unsafe: message.textContent.includes('<javascript:alert(1)>') };
      message.remove(); return result;
    });
    assert.deepEqual(linkParity, { auto: 'https://example.test/p', title: 'https://example.test/', space: 's', links: 3, unsafe: true }, 'angle autolinks, titles and spaced addresses link; an unsafe angle address stays text');
    // [end group d]
    const diagramSafety = await first.evaluate(async () => {
      const { renderVisualPlan } = await import('/js/visual-plan.js');
      const source = 'flowchart LR\n A["<img src=x onerror=window.__planXss=1>"] --> B["safe"]';
      const plan = renderVisualPlan({ visual_plan: [{ type: 'diagram', data: { mermaid: source } }] });
      const safe = renderVisualPlan({ visual_plan: [{ type: 'diagram', data: { mermaid: 'flowchart LR\n A["Start"] --> B["Done"]' } }] });
      document.body.append(plan, safe);
      for (let i = 0; i < 150 && (!plan.querySelector('svg,.visual-plan-render-error') || !safe.querySelector('svg,.visual-plan-render-error')); i++) await new Promise(resolve => setTimeout(resolve, 100));
      const result = { injected: Boolean(window.__planXss), source: plan.textContent.includes(source), svg: Boolean(plan.querySelector('svg')),
        completed: Boolean(plan.querySelector('svg,.visual-plan-render-error')), safeSvg: Boolean(safe.querySelector('svg')) };
      plan.remove(); safe.remove(); return result;
    });
    assert.equal(await first.evaluate(() => Boolean(document.querySelector('script[src="/vendor/mermaid.min.js"]'))), true, 'Mermaid loads for a diagram');
    assert.equal(diagramSafety.injected, false, 'Mermaid source must not execute HTML');
    assert.equal(diagramSafety.completed, true, 'diagram resolves to SVG or a source fallback');
    assert.equal(diagramSafety.safeSvg, true, 'valid Mermaid diagram renders as SVG');
    assert.ok(diagramSafety.svg || diagramSafety.source, 'diagram remains visible as SVG or source');
    assert.equal(await first.$eval('#usages-toggle', el => el.title), 'Usages');
    assert.equal(await first.$eval('#settings-toggle', el => el.title), 'Settings');
    await openSettings(first);
    await domClick(first, '#settings-general-tab');
    await first.select('#theme-select', 'high-contrast-dark');
    assert.equal(await first.evaluate(() => document.documentElement.dataset.theme), 'high-contrast-dark');
    await domClick(first, '#settings-general-tab');
    await first.waitForFunction(() => document.querySelector('#settings-general-tab')?.classList.contains('settings-selected'));
    await first.waitForFunction(() => getComputedStyle(document.querySelector('#settings-general-tab')).color === 'rgb(255, 255, 255)');
    const hcNavContrast = await first.$eval('#settings-general-tab', el => {
      const rgb = value => value.match(/[\d.]+/g).slice(0, 3).map(Number);
      const lum = value => rgb(value).map(v => { v /= 255; return v <= .04045 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4; }).reduce((sum, v, i) => sum + v * [.2126, .7152, .0722][i], 0);
      const style = getComputedStyle(el), a = lum(style.color), b = lum(style.backgroundColor);
      return { ratio: (Math.max(a, b) + .05) / (Math.min(a, b) + .05), color: style.color, background: style.backgroundColor, classes: el.className, theme: document.documentElement.dataset.theme, dialog: el.closest('dialog')?.id };
    });
    assert.ok(hcNavContrast.ratio >= 7, `selected HC settings tab contrast ${JSON.stringify(hcNavContrast)}`);
    const settingsSize = await first.$eval('#settings-dialog', el => ({ width: el.getBoundingClientRect().width, height: el.getBoundingClientRect().height, nav: el.querySelector('nav').getBoundingClientRect().width }));
    assert.equal(Math.round(settingsSize.width), 760);
    assert.equal(Math.round(settingsSize.height), 720);
    assert.equal(Math.round(settingsSize.nav), 180);
    for (const section of ['general', 'usages', 'project', 'notifications', 'roles', 'machines']) {
      await first.click(`#settings-${section}-tab`);
      assert.equal(await first.$eval(`#settings-${section}`, el => !el.hidden), true, `${section} settings section visible`);
      assert.equal(await first.$eval(`#settings-${section}`, el => el.scrollWidth <= el.clientWidth + 2), true, `${section} settings fits pane`);
    }
    await domClick(first, '#settings-general-tab');
    assert.equal(await first.$eval('.sidebar', el => getComputedStyle(el).backgroundColor), 'rgb(0, 0, 0)');
    await domClick(first, '#settings-close');
    await first.$eval('#spawn-dialog', el => el.showModal());
    const spawnTab = await first.$eval('#spawn-cancel', el => ({ color: getComputedStyle(el).color, background: getComputedStyle(el).backgroundColor, border: getComputedStyle(el).borderColor }));
    assert.notEqual(spawnTab.color, spawnTab.background);
    assert.notEqual(spawnTab.border, spawnTab.background);
    await first.$eval('#spawn-dialog', el => el.close());
    await first.evaluateOnNewDocument(() => document.addEventListener('DOMContentLoaded', () => {
      window.__themeAtDOMContentLoaded = document.documentElement.dataset.theme;
    }));
    await first.reload();
    await appReady(first);
    assert.equal(await first.evaluate(() => document.documentElement.dataset.theme), 'high-contrast-dark', 'theme was not restored before rendering');
    assert.equal(await first.evaluate(() => window.__themeAtDOMContentLoaded), 'high-contrast-dark', 'theme flashed before DOM ready');
    await openSettings(first);
    await domClick(first, '#settings-general-tab');
    assert.equal(await first.$eval('#theme-select', el => el.value), 'high-contrast-dark');
    await first.select('#theme-select', 'dark');
    await domClick(first, '#settings-close');
    assert.equal(await first.$eval('#build-version', el => el.textContent), 'v2 development');
    await first.waitForFunction(() => document.querySelector('#connection')?.getAttribute('aria-label') === 'Connected');
    const returning = await a.newPage();
    await returning.evaluateOnNewDocument(() => {
      window.__loginWasVisible = false;
      const check = () => { const login = document.querySelector('#login-view'); if (login && !login.hidden) window.__loginWasVisible = true; };
      new MutationObserver(check).observe(document, { childList: true, subtree: true, attributes: true, attributeFilter: ['hidden'] });
      document.addEventListener('DOMContentLoaded', check);
    });
    await returning.goto(origin);
    await appReady(returning);
    assert.equal(await returning.evaluate(() => window.__loginWasVisible), false, 'login form flashed during authenticated load');
    await returning.close();
    const usageStatus = await first.evaluate(async () => (await fetch('/api/usages')).status);
    assert.equal(usageStatus, 503, 'usage reader should be disabled in UI smoke');
    assert.equal(await first.evaluate(() => typeof crypto.randomUUID), 'undefined');
    assert.match(await first.evaluate(async () => (await import('/js/uuid.js')).uuid()), /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    await first.click('#project-open');
    assert.equal(await first.$eval('#project-dialog', el => el.open && document.activeElement === el.querySelector('[name=name]')), true, 'new project dialog did not focus the name');
    await domClick(first, '#project-form button[type=submit]');
    assert.match(await first.$eval('#project-error', el => el.textContent), /project name and folder path/);
    await first.keyboard.press('Escape');
    await first.waitForFunction(() => !document.querySelector('#project-dialog').open);
    await domClick(first, '#project-open');
    assert.equal(await first.$eval('#project-error', el => el.textContent), '');
    // A relative folder is refused with the reason, and the dialog stays open for the correction.
    await first.locator('#project-form [name=name]').fill('Relative'); await first.locator('#project-form [name=folder]').fill('work/repo');
    await domClick(first, '#project-form button[type=submit]');
    await first.waitForFunction(() => /absolute path/.test(document.querySelector('#project-error').textContent) && document.querySelector('#project-dialog').open);
    await first.evaluate(() => document.querySelector('#project-form').reset());
    await first.locator('#project-form [name=name]').fill('UI smoke'); await first.locator('#project-form [name=folder]').fill(gitRoot);
    await domClick(first, '#project-form [name=folder]'); await first.keyboard.press('Enter');
    await first.waitForFunction(() => !document.querySelector('#project-dialog').open);
    await first.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'UI smoke' && document.querySelector('.room-row.active .room-name')?.textContent.includes('UI smoke'));
    await openSettings(first);
    await first.waitForSelector('#settings-users-tab:not([hidden]):not(.settings-tab-pending)');
    await domClick(first, '#settings-users-tab');
    await first.waitForFunction(() => !document.querySelector('#settings-users')?.hidden && document.querySelector('#settings-users .user-row'));
    assert.equal(await first.$eval('#settings-users', el => el.textContent.includes('owner')), true);
    await domClick(first, '#settings-users .user-invite-form button');
    await first.waitForFunction(() => !document.querySelector('#settings-users input[aria-label="Invitation link"]')?.hidden);
    assert.match(await first.$eval('#settings-users input[aria-label="Invitation link"]', el => el.value), /#invite=aui_/);
    const inviteLink = await first.$eval('#settings-users input[aria-label="Invitation link"]', el => el.value);
    assert.match(await first.$eval('.invite-link-hint', el => (el.hidden ? '' : el.textContent)), /only works on this computer/, 'a link with a local address says it is only good on this computer');
    await domClick(first, '#settings-close');
    await first.locator('#composer textarea').fill('/');
    await first.waitForFunction(() => !document.querySelector('.quick-commands')?.hidden);
    assert.deepEqual(await first.$$eval('.quick-command strong', els => els.map(el => el.textContent)), ['/spawn'], 'home room has no archive command');
    await first.locator('#composer textarea').fill('');
    await first.locator('#composer textarea').fill('/sp');
    await first.waitForFunction(() => document.querySelector('.quick-commands')?.textContent.includes('/spawn'));
    await first.keyboard.press('Escape');
    await first.waitForFunction(() => document.querySelector('.quick-commands')?.hidden);
    assert.equal(await first.$eval('#composer textarea', el => el.value), '/sp', 'Escape keeps the typed command');
    await first.locator('#composer textarea').fill('');
    // The macro picker (typing <) looks like v1's autocomplete: one outlined list over the composer, one line per macro.
    {
      const putMacros = body => first.evaluate(async macros => { const { csrf_token } = await (await fetch('/api/me')).json(); const response = await fetch('/api/macros', { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify(macros) }); if (!response.ok) throw new Error(`macros ${response.status}`); }, body);
      await putMacros({ brainstorm: 'b'.repeat(60), review: 'Review this' });
      await first.locator('#composer textarea').fill('');
      await first.click('#composer textarea');
      await first.keyboard.type('<br');
      await first.waitForFunction(() => document.querySelector('.macro-commands:not([hidden]) .quick-command'));
      const look = await first.evaluate(() => {
        const popup = document.querySelector('.macro-commands'), row = popup.querySelector('.quick-command'), key = row.querySelector('strong'), preview = row.querySelector('span'), form = document.querySelector('#composer');
        const box = popup.getBoundingClientRect(), formBox = form.getBoundingClientRect();
        return { rows: popup.querySelectorAll('.quick-command').length, direction: getComputedStyle(row).flexDirection, mono: /mono|courier|menlo|consolas/i.test(getComputedStyle(key).fontFamily), key: key.textContent, preview: preview.textContent, active: row.getAttribute('aria-selected'),
          fullWidth: Math.abs(box.width - formBox.width) < 2 && Math.abs(box.left - formBox.left) < 2, above: box.bottom <= formBox.top + 1, border: getComputedStyle(popup).borderTopWidth };
      });
      assert.deepEqual(look, { rows: 1, direction: 'row', mono: true, key: '<BRAINSTORM>', preview: `${'b'.repeat(40)}...`, active: 'true', fullWidth: true, above: true, border: '1px' }, `macro picker look: ${JSON.stringify(look)}`);
      await first.keyboard.press('Tab');
      assert.equal(await first.$eval('#composer textarea', el => el.value), `${'b'.repeat(60)} `, 'Tab expands the macro in place');
      await first.locator('#composer textarea').fill('');
      // One shared list box: a single active row (hover moves the selection), aria-activedescendant, the active row scrolled into view.
      const many = Object.fromEntries(Array.from({ length: 24 }, (_, i) => [`m${String(i).padStart(2, '0')}`, `Macro number ${i}`]));
      await putMacros(many);
      await first.click('#composer textarea');
      await first.keyboard.type('<m');
      await first.waitForFunction(() => document.querySelectorAll('.macro-commands:not([hidden]) .quick-command').length === 24);
      const state = () => first.evaluate(() => {
        const rows = [...document.querySelectorAll('.macro-commands .quick-command')], draft = document.querySelector('#composer textarea'), popup = document.querySelector('.macro-commands');
        const selected = rows.filter(row => row.getAttribute('aria-selected') === 'true'), tinted = rows.filter(row => getComputedStyle(row).backgroundColor !== 'rgba(0, 0, 0, 0)');
        const active = selected[0], box = popup.getBoundingClientRect(), rect = active?.getBoundingClientRect();
        return { selected: selected.length, tinted: tinted.length, index: rows.indexOf(active), descendant: draft.getAttribute('aria-activedescendant') === active?.id, inView: !!rect && rect.top >= box.top - 1 && rect.bottom <= box.bottom + 1 };
      });
      assert.deepEqual(await state(), { selected: 1, tinted: 1, index: 0, descendant: true, inView: true }, 'one active row to start');
      const second = await first.$eval('.macro-commands .quick-command:nth-child(3)', el => { const r = el.getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; });
      await first.mouse.move(second.x, second.y);
      assert.deepEqual(await state(), { selected: 1, tinted: 1, index: 2, descendant: true, inView: true }, 'hovering moves the selection');
      await first.keyboard.press('ArrowDown');
      assert.deepEqual(await state(), { selected: 1, tinted: 1, index: 3, descendant: true, inView: true }, 'with the pointer resting on a row, the keyboard selection is still the only highlight');
      await first.keyboard.press('ArrowUp'); await first.keyboard.press('ArrowUp'); await first.keyboard.press('ArrowUp'); await first.keyboard.press('ArrowUp');
      assert.deepEqual(await state(), { selected: 1, tinted: 1, index: 23, descendant: true, inView: true }, 'wrapping to the last row scrolls it into view');
      await first.keyboard.press('Escape');
      assert.equal(await first.$eval('#composer textarea', el => el.hasAttribute('aria-activedescendant')), false, 'closing the list clears aria-activedescendant');
      await first.mouse.move(5, 5);
      await first.locator('#composer textarea').fill('');
      // The @ list is the same component: one selected row and aria-activedescendant.
      await first.click('#composer textarea');
      await first.keyboard.type('@');
      await first.waitForFunction(() => document.querySelector('.mention-commands:not([hidden]) .quick-command'));
      assert.deepEqual(await first.evaluate(() => { const rows = [...document.querySelectorAll('.mention-commands .quick-command')], draft = document.querySelector('#composer textarea'); const selected = rows.filter(row => row.getAttribute('aria-selected') === 'true'); return { selected: selected.length, descendant: draft.getAttribute('aria-activedescendant') === selected[0]?.id, many: rows.length > 1 }; }), { selected: 1, descendant: true, many: true }, 'the @ list has one active row');
      await first.keyboard.press('Escape');
      await first.locator('#composer textarea').fill('');
      await putMacros({});
    }
    await domClick(first, '.add-room');
    await first.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Nested task');
    await first.waitForFunction(() => document.querySelector('#messages')?.dataset.empty === 'true');
    assert.equal(await first.$eval('#messages', el => getComputedStyle(el, '::before').content), '"No messages in this room yet."', 'an empty room says so');
    assert.deepEqual(await first.$eval('#composer .composer-aa', el => ({ icon: el.querySelectorAll('svg').length, text: el.textContent.trim() })), { icon: 1, text: '' }, 'the formatting switch is an icon, not the letters Aa');
    await domClick(first, '#room-info');
    assert.equal(await first.$eval('#room-info-dialog', el => el.open), true);
    assert.equal(await first.$eval('#room-rename-title', el => el.value), 'Nested task', 'the room title is the field at the top of its settings');
    // Room settings: one control size, the debugging facts hidden unless the inspector is on, Pause / Stop as secondary / danger.
    {
      const visibleFacts = () => first.$$eval('#room-info-facts dt', els => els.filter(el => getComputedStyle(el).display !== 'none').map(el => el.textContent));
      assert.deepEqual(await visibleFacts(), ['Status', 'Members'], 'Kind and Room ID stay behind the inspector, and the title is the field above');
      await first.evaluate(() => { document.documentElement.dataset.debugInspector = 'on'; });
      assert.deepEqual(await visibleFacts(), ['Kind', 'Status', 'Members', 'Room ID'], 'the inspector shows Kind and Room ID');
      await first.evaluate(() => { document.documentElement.dataset.debugInspector = 'off'; });
      const sizes = await first.evaluate(() => {
        const dialog = document.querySelector('#room-info-dialog');
        const controls = [...dialog.querySelectorAll('button, input:not([type=checkbox]), select')].filter(el => el.offsetParent !== null && !el.closest('[hidden]') && !el.matches('#room-info-close, .room-members button'));
        return { count: controls.length, heights: [...new Set(controls.map(el => Math.round(el.getBoundingClientRect().height)))], pause: document.querySelector('[data-scope-action="pause"]').className, stop: document.querySelector('[data-scope-action="stop"]').className };
      });
      assert.ok(sizes.count >= 6 && sizes.heights.length === 1 && sizes.heights[0] >= 32, `Room settings controls share one height: ${JSON.stringify(sizes)}`);
      assert.equal(/\bbtn-primary\b/.test(sizes.pause) || /\bbtn-primary\b/.test(sizes.stop), false, 'Pause and Stop are not primary');
      // Pause, Stop and Resume are icons: a glyph, a tooltip and an accessible name, and no text of their own.
      const scopeIcons = await first.evaluate(() => [...document.querySelectorAll('#room-scope-control [data-scope-action], #project-scope-control [data-scope-action]')].map(button => ({ action: button.dataset.scopeAction, glyph: Boolean(button.querySelector('.icon-mask')), title: button.title, name: button.getAttribute('aria-label'), desired: button.closest('#room-scope-control, #project-scope-control').dataset.desired, text: [...button.childNodes].filter(node => node.nodeType === Node.TEXT_NODE && node.textContent.trim()).length })));
      assert.equal(scopeIcons.length >= 3, true, JSON.stringify(scopeIcons));
      assert.deepEqual(scopeIcons.filter(icon => !icon.glyph || !icon.title || icon.title !== icon.name || icon.text).length, 0, `scope controls are icon buttons: ${JSON.stringify(scopeIcons)}`);
      // Each scope's Resume is named from its own state: "Retry resume" when that scope is already active, "Resume" otherwise.
      const expectedName = icon => ({ pause: 'Pause', stop: 'Stop', resume: icon.desired === 'active' ? 'Retry resume' : 'Resume' })[icon.action];
      assert.deepEqual(scopeIcons.filter(icon => icon.name !== expectedName(icon)).map(icon => `${icon.action}:${icon.name} (scope ${icon.desired || 'unloaded'})`), [], 'each scope control is named from its own state');
      assert.deepEqual([...new Set(scopeIcons.map(icon => icon.action))].sort(), ['pause', 'resume', 'stop'], 'every scope control is present');
      assert.equal(/\bbtn-danger\b/.test(sizes.stop) && !/\bbtn-danger\b/.test(sizes.pause), true, 'Stop is the danger button, Pause is secondary');
    }
    await first.waitForFunction(() => !document.querySelector('.room-memory-setting select')?.disabled);
    await first.select('.room-memory-setting select', 'off');
    await first.waitForFunction(() => document.querySelector('.room-memory-setting p')?.textContent === 'Room message memory is off.');
    await first.select('.room-memory-setting select', 'inherit');
    await first.waitForFunction(() => document.querySelector('.room-memory-setting p')?.textContent === 'Room follows the project setting.');
    await domClick(first, '.room-worktree button:first-of-type');
    await first.waitForFunction(() => document.querySelector('.room-worktree p')?.textContent.includes('.aimebu/worktrees/'));
    assert.match(await first.$eval('.room-worktree p', el => el.textContent), /ready:/);
    await domClick(first, '#room-info-close');
    const roomID = await first.evaluate(() => location.hash.slice(1));
    await first.waitForFunction(() => !document.querySelector('#task-notes-toggle')?.hidden);
    await first.click('#knowledge-toggle');
    await first.waitForFunction(() => document.querySelector('#knowledge-dialog')?.open && document.querySelector('#knowledge-status')?.textContent.includes('Memory is off'));
    // The viewer opens on "All visible"; its project picker then only scopes the project facts listed with the global records.
    assert.deepEqual(await first.evaluate(() => ({ scope: document.querySelector('#knowledge-scope').value, title: document.querySelector('#knowledge-title').textContent, project: !document.querySelector('#knowledge-project-label').hidden, projectLabel: document.querySelector('#knowledge-project-label').firstChild.textContent.trim(), enable: document.querySelector('#knowledge-enabled-label').hidden, add: document.querySelector('#knowledge-add').hidden })),
      { scope: 'all', title: 'All visible memory', project: true, projectLabel: 'Project facts from', enable: true, add: true }, 'the Memory viewer opens on All visible');
    await first.select('#knowledge-scope', 'project');
    await first.waitForFunction(() => document.querySelector('#knowledge-title')?.textContent === 'Project facts' && !document.querySelector('#knowledge-enabled-label').hidden && document.querySelector('#knowledge-project-label').firstChild.textContent.trim() === 'Project' && document.querySelector('#knowledge-status')?.textContent.includes('bytes') && document.querySelector('#knowledge-status').textContent.includes('Memory is off'));
    await first.click('#knowledge-enabled');
    await first.waitForFunction(() => document.querySelector('#knowledge-enabled')?.checked && !document.querySelector('#knowledge-add')?.hidden);
    await first.locator('#knowledge-add textarea').fill('Project fact from smoke');
    await first.click('#knowledge-add button');
    await first.waitForFunction(() => document.querySelector('#knowledge-records textarea')?.value === 'Project fact from smoke');
    assert.deepEqual(await first.$eval('#knowledge-records .knowledge-actions button.btn-danger', el => [el.getAttribute('aria-label'), el.title, !!el.querySelector('.icon-mask-trash'), el.textContent]), ['Delete', 'Delete', true, '']);
    // v1's memory cards use small record text.
    assert.deepEqual(await first.evaluate(() => { const card = document.querySelector('#knowledge-records .knowledge-record'); return { textPx: parseFloat(getComputedStyle(card.querySelector('textarea')).fontSize), metaPx: parseFloat(getComputedStyle(card.querySelector('small')).fontSize) }; }), { textPx: 12, metaPx: 11 }, 'knowledge cards use v1\'s small record text');
    await first.select('#knowledge-scope', 'agent_shared_notes');
    await first.waitForFunction(() => document.querySelector('#knowledge-title')?.textContent === 'Shared agent notes');
    await first.waitForFunction(() => document.querySelector('#knowledge-status')?.textContent.includes('records') && !document.querySelector('#knowledge-add')?.hidden);
    await first.locator('#knowledge-add textarea').fill('Shared note from smoke');
    await first.click('#knowledge-add button');
    await first.waitForFunction(() => document.querySelector('#knowledge-records textarea')?.value === 'Shared note from smoke');
    await first.waitForFunction(() => {
      const button = document.querySelector('#knowledge-records .knowledge-actions button:last-child');
      if (button?.getAttribute('aria-label') !== 'Deactivate' || button.title !== 'Deactivate' || !button.querySelector('.icon-mask-stop')) return false;
      button.click(); // Find and click in one browser task so refresh cannot detach it.
      return true;
    });
    await waitDump(first, 'knowledge Deactivate', () => document.querySelector('#knowledge-records article small')?.textContent.includes('Inactive'), () => ({
      error: document.querySelector('#knowledge-error')?.textContent, status: document.querySelector('#knowledge-status')?.textContent,
      records: [...document.querySelectorAll('#knowledge-records article')].map(article => ({ meta: article.querySelector('small')?.textContent, buttons: [...article.querySelectorAll('.knowledge-actions button')].map(button => button.getAttribute('aria-label') || button.textContent) })),
    }));
    await first.waitForFunction(() => {
      const button = document.querySelector('#knowledge-records .knowledge-actions button:last-child');
      if (button?.getAttribute('aria-label') !== 'Activate for agents' || button.title !== 'Activate for agents' || !button.querySelector('.icon-mask-play')) return false;
      button.click();
      return true;
    });
    await first.waitForFunction(() => !document.querySelector('#knowledge-records article small')?.textContent.includes('Inactive'));
    await first.select('#knowledge-scope', 'user_profile');
    await first.waitForFunction(() => document.querySelector('#knowledge-title')?.textContent === 'User profiles');
    await first.waitForFunction(() => document.querySelector('#knowledge-status')?.textContent.includes('records') && !document.querySelector('#knowledge-add')?.hidden);
    await first.locator('#knowledge-add textarea').fill('Profile from smoke');
    await first.click('#knowledge-add button');
    await first.waitForFunction(() => document.querySelector('#knowledge-records textarea')?.value === 'Profile from smoke');
    await first.select('#knowledge-scope', 'all');
    await first.waitForFunction(() => document.querySelector('#knowledge-title')?.textContent === 'All visible memory' && document.querySelectorAll('#knowledge-records article').length === 3);
    assert.equal(await first.$eval('#knowledge-add', el => el.hidden), true, 'adding needs a concrete scope');
    const allMeta = await first.$$eval('#knowledge-records article small', els => els.map(el => el.textContent));
    assert.equal(allMeta.some(text => text.startsWith('Project facts · ')), true);
    assert.equal(allMeta.some(text => text.startsWith('User profile ')), true);
    assert.equal(allMeta.some(text => text.startsWith('Shared agent notes · ')), true);
    const usageRows = await first.$$eval('.knowledge-usage-row .knowledge-usage-meta', els => els.map(el => el.textContent));
    assert.equal(usageRows.length, 3, 'one usage row per scope');
    assert.equal(usageRows.every(text => /\d+ \/ \d+ bytes · 1 record$/.test(text)), true, usageRows.join('|'));
    assert.equal(await first.$$eval('.knowledge-usage-bar[role="progressbar"]', els => els.length), 3);
    await first.click('#knowledge-clean');
    await first.waitForFunction(() => document.querySelectorAll('#knowledge-records article').length === 0 && document.querySelector('#knowledge-records')?.textContent.includes('No visible memory saved.'));
    assert.equal(await first.$eval('.knowledge-usage', el => el.hidden), true);
    await domClick(first, '#knowledge-close');
    await first.waitForFunction(() => !document.querySelector('#knowledge-dialog')?.open && !document.querySelector('#task-notes-toggle')?.hidden);
    await domClick(first, '#task-notes-toggle');
    await first.waitForFunction(() => document.querySelector('#knowledge-dialog')?.open && document.querySelector('#knowledge-title')?.textContent.startsWith('Notes'));
    await first.waitForFunction(() => !document.querySelector('#knowledge-add')?.hidden && document.querySelector('#knowledge-status')?.textContent.includes('records'));
    await first.locator('#knowledge-add textarea').fill('Task note from smoke');
    await first.click('#knowledge-add button');
    await first.waitForFunction(() => document.querySelector('#knowledge-records textarea')?.value === 'Task note from smoke');
    await domClick(first, '#knowledge-close');
    await first.waitForFunction(() => !document.querySelector('#knowledge-dialog')?.open);
    await login(second);
    await second.waitForFunction(() => [...document.querySelectorAll('.room-name')].some(el => el.textContent.includes('Nested task')));
    await second.evaluate(() => [...document.querySelectorAll('.room-name')].find(el => el.textContent.includes('Nested task')).click());
    await second.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Nested task');
    await first.locator('#composer textarea').fill('Draft');
    await first.click('#composer textarea');
    await first.evaluate(() => {
      window.__composerEnterTrace = [];
      document.querySelector('#composer textarea').addEventListener('keydown', event => {
        if (event.key === 'Enter') window.__composerEnterTrace.push({ shift: event.shiftKey, composing: event.isComposing, value: event.target.value });
      }, { capture: true });
    });
    await first.keyboard.down('Shift'); await first.keyboard.press('Enter'); await first.keyboard.up('Shift');
    const shiftedDraft = await first.$eval('#composer textarea', el => el.value);
    if (shiftedDraft !== 'Draft\n') {
      preserveFailure = true;
      const trace = await first.evaluate(() => ({ keys: window.__composerEnterTrace, active: document.activeElement?.outerHTML.slice(0, 200), room: location.hash,
        messages: [...document.querySelectorAll('.message-body')].map(el => el.textContent).slice(-5), error: document.querySelector('#chat-error')?.textContent }));
      throw new Error(`Shift+Enter changed draft to ${JSON.stringify(shiftedDraft)}; temp ${tmp}; trace ${JSON.stringify(trace)}; server ${logs.slice(-4000)}`);
    }
    await first.locator('#composer textarea').fill('First message');
    await first.keyboard.press('Enter');
    await first.waitForSelector('.message[data-message-id]');
    await second.waitForFunction(() => [...document.querySelectorAll('.message-body')].some(el => el.textContent === 'First message'));
    await first.waitForFunction(() => [...document.querySelectorAll('.room-preview')].some(el => el.textContent === 'First message'));
    assert.deepEqual(await first.$eval('#room-members', el => [el.querySelector('.room-members-count')?.textContent, el.getAttribute('aria-label'), el.title, !!el.querySelector('svg'), el.hidden]), ['1', '1 member', '1 member', true, false]);
    assert.equal(await first.$eval('#room-members', el => !!el.querySelector('.room-header-status')), false, 'the idle member chip has no empty status gap');
    assert.equal(await first.$eval('#room-icon', el => el.textContent), '#', 'a task room shows its kind glyph');
    const panelParity = await first.evaluate(() => {
      const css = selector => getComputedStyle(document.querySelector(selector));
      const box = selector => document.querySelector(selector).getBoundingClientRect();
      const sample = document.createElement('div'); sample.className = 'room-row active';
      sample.innerHTML = '<button class="row-btn room-name"># sample</button><span class="unread-count">2</span>';
      document.querySelector('#projects').append(sample);
      const result = {
        label: [css('.sidebar > header strong').fontSize, css('.sidebar > header strong').lineHeight],
        room: [Math.round(box('.room-row:not(.active)').height), css('.room-row:not(.active)').padding],
        unread: [getComputedStyle(sample.querySelector('.room-name')).fontWeight, getComputedStyle(sample.querySelector('.unread-count')).fontSize, getComputedStyle(sample.querySelector('.unread-count')).padding, getComputedStyle(sample.querySelector('.unread-count')).backgroundColor, getComputedStyle(sample.querySelector('.unread-count')).color],
        preview: [css('.room-preview').fontSize, css('.room-preview').lineHeight],
        header: [Math.round(box('.chat > header').height), css('.chat > header').gap, css('.chat > header').padding],
        count: [Math.round(box('#room-members').height), css('#room-members').backgroundColor, css('#room-members').fontSize],
        bar: [Math.round(box('.workspace-bar').height), Math.round(box('#settings-toggle').width), css('#settings-toggle').backgroundColor, css('#build-version').fontSize, css('#build-version').opacity],
      };
      sample.remove();
      return result;
    });
    assert.deepEqual(panelParity, {
      label: ['9.8px', '14.7px'], room: [38, '4px 8px'],
      unread: ['700', '9.1px', '1px 7px', 'rgb(31, 111, 235)', 'rgb(255, 255, 255)'],
      preview: ['10.5px', '15.75px'], header: [43, '12px', '0px 16px'],
      count: [22, 'rgb(28, 33, 40)', '9.1px'],
      bar: [53, 30, 'rgba(0, 0, 0, 0)', '8.4px', '1'],
    }, 'desktop panels retain the v1 measurements');
    await second.evaluate(() => [...document.querySelectorAll('.room-name')].find(el => el.textContent.startsWith('⌂'))?.click());
    await second.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'UI smoke');
    assert.equal(await second.$eval('#room-icon', el => el.textContent), '⌂', 'the home room shows its glyph');
    await second.locator('#composer textarea').fill('Unread preview'); await second.locator('#composer button[type=submit]').click();
    const homeID = await second.evaluate(() => location.hash.slice(1));
    // Read cursors are per account, so a post from another tab of the same account is not unread here.
    // The list previews the last message only under the room that is open: the home room is not open in this tab.
    await first.waitForFunction(id => document.querySelector(`[data-room-id="${id}"] .room-name`) && !document.querySelector(`[data-room-id="${id}"] > .room-preview`), {}, homeID);
    assert.deepEqual(await first.evaluate(id => ({ count: document.querySelectorAll('.room-count').length, tip: document.querySelector(`[data-room-id="${id}"] .room-name`).title }), homeID), { count: 0, tip: await first.$eval(`[data-room-id="${homeID}"] .room-name`, el => el.title) }, 'the grey member count is gone from the room rows');
    assert.match(await first.$eval(`[data-room-id="${homeID}"] .room-name`, el => el.title), / · \d+ members?$/, 'the member count is in the name tooltip');
    assert.equal(await first.$$eval('.room-preview', nodes => nodes.length), 1, 'only the active room shows a preview');
    assert.equal(await first.$eval(`[data-room-id="${homeID}"]`, (el, id) => { const badge = el.querySelector(':scope > .room-row .unread-count'); return badge && !badge.hidden ? badge.textContent : ''; }, homeID), '', 'a same-account post raised unread');
    assert.equal((await first.evaluate(async id => (await (await fetch(`/api/rooms/${id}`)).json()).unread_count, homeID)), 0, 'server unread_count counted a same-account post');
    await domClick(first, `[data-room-id="${homeID}"] .room-name`);
    await first.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'UI smoke');
    await domClick(first, `[data-room-id="${roomID}"] .room-name`);
    await first.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Nested task' && document.querySelectorAll('.message').length === 1 && document.querySelector('.message-body')?.textContent === 'First message');
    await domClick(second, `[data-room-id="${roomID}"] .room-name`);
    await second.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Nested task' && document.querySelector('.message-body')?.textContent === 'First message');
    const id = await first.$eval('.message', el => el.dataset.messageId);
    await first.mouse.move(2, 2);
    assert.equal(await first.$eval('.message:first-child .message-actions', el => getComputedStyle(el).opacity), '0', 'the message actions stay out of the way until the message is hovered or focused');
    await first.hover('.message:first-child');
    await first.waitForFunction(() => getComputedStyle(document.querySelector('.message:first-child .message-actions')).opacity === '1');
    assert.deepEqual(await first.evaluate(() => { const bubble = document.querySelector('.message:first-child .message-bubble').getBoundingClientRect(), bar = document.querySelector('.message:first-child .message-actions').getBoundingClientRect(); const items = [...document.querySelector('.message:first-child .message-meta').children].filter(node => node.getClientRects().length).map(node => node.getBoundingClientRect()); const clear = items.every(meta => bar.right <= meta.left || bar.left >= meta.right || bar.bottom <= meta.top || bar.top >= meta.bottom); const text = document.querySelector('.message:first-child .message-body').getBoundingClientRect(); const clearOfText = bar.right <= text.left || bar.left >= text.right || bar.bottom <= text.top || bar.top >= text.bottom; const beside = bar.right <= bubble.left + 1, corner = bar.right <= bubble.right && bubble.right - bar.right < 12 && bar.top < bubble.top + 4 && bubble.top - bar.top < 24; return { onTheBubble: beside || corner, clearOfTheText: clearOfText, clearOfTheSenderLine: clear, buttons: document.querySelectorAll('.message:first-child .message-actions > .message-action').length }; }), { onTheBubble: true, clearOfTheText: true, clearOfTheSenderLine: true, buttons: 6 }, 'the toolbar (react, more reactions, reply, thread, copy, link) sits at the right end of the sender line over the bubble corner (or beside the bubble when it is the viewer\'s own) and covers neither the message text nor the sender, the number or the time');
    // v1 parity of the message frame: the bubble sits right under the sender line (the hover toolbar reserves no space), and the sender's
    // name is plain text until it is hovered or focused.
    assert.deepEqual(await first.evaluate(() => { const meta = document.querySelector('.message:first-child .message-meta').getBoundingClientRect(), bubble = document.querySelector('.message:first-child .message-bubble').getBoundingClientRect(), author = document.querySelector('.message:first-child .message-author'); return { gap: Math.round(bubble.top - meta.bottom), underline: getComputedStyle(author).textDecorationLine }; }), { gap: 2, underline: 'none' }, 'the bubble is 2px under the sender line and the sender name has no permanent underline');
    // The shared in-app dialog (the page's own flows have it answered for them above): focus, Escape, Enter, validation, one at a time.
    {
      const page = second;
      await page.evaluate(async () => { const v = new URL([...document.scripts].find(script => script.src.includes('/js/app.js')).src).search; window.__dialogs = await import('/js/dialogs.js' + v); });
      const state = () => page.evaluate(() => { const dialog = document.querySelector('#app-dialog'); return dialog?.open ? { title: dialog.querySelector('h2').textContent, ok: dialog.querySelector('.app-dialog-ok').textContent, cancelShown: !dialog.querySelector('.app-dialog-cancel').hidden, focus: document.activeElement.className || document.activeElement.name, error: dialog.querySelector('.error').textContent } : null; });
      const result = key => page.evaluate(key => window[key], key);
      await page.focus('#settings-toggle');
      await page.evaluate(() => { window.__r1 = undefined; window.__dialogs.confirmDialog('Really remove it?', { title: 'Remove thing', confirm: 'Remove', danger: true }).then(value => { window.__r1 = value; }); });
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      assert.deepEqual(await state(), { title: 'Remove thing', ok: 'Remove', cancelShown: true, focus: 'btn app-dialog-cancel', error: '' }, 'a destructive question starts on Cancel');
      await page.keyboard.press('Escape');
      await page.waitForFunction(() => window.__r1 !== undefined);
      assert.equal(await result('__r1'), false, 'Escape answers no');
      assert.equal(await page.evaluate(() => document.activeElement.id), 'settings-toggle', 'focus goes back to where it was');
      await page.evaluate(() => { window.__r2 = undefined; window.__dialogs.promptDialog('Name', { title: 'Name it', label: 'Name', required: true, validate: text => (text.length < 3 ? 'At least three characters.' : '') }).then(value => { window.__r2 = value; }); });
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      assert.equal((await state()).focus, 'value', 'a prompt starts on its field');
      await page.keyboard.press('Enter');
      assert.equal((await state()).error, 'Name is required.', 'an empty required field is refused where it is');
      await page.keyboard.type('ab'); await page.keyboard.press('Enter');
      assert.equal((await state()).error, 'At least three characters.', 'a validator speaks inline and keeps the dialog open');
      await page.keyboard.type('c'); await page.keyboard.press('Enter');
      await page.waitForFunction(() => window.__r2 !== undefined);
      assert.equal(await result('__r2'), 'abc', 'Enter submits the typed value');
      await page.evaluate(() => { window.__order = []; window.__dialogs.noticeDialog('First notice', { title: 'One' }).then(() => window.__order.push('one')); window.__dialogs.noticeDialog('Second notice', { title: 'Two' }).then(() => window.__order.push('two')); });
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open);
      assert.deepEqual(await state(), { title: 'One', ok: 'OK', cancelShown: false, focus: 'btn btn-primary app-dialog-ok', error: '' }, 'a notice has a single OK');
      await page.keyboard.press('Enter');
      await page.waitForFunction(() => document.querySelector('#app-dialog')?.open && document.querySelector('#app-dialog h2').textContent === 'Two');
      await page.keyboard.press('Enter');
      await page.waitForFunction(() => window.__order.length === 2);
      assert.deepEqual(await result('__order'), ['one', 'two'], 'a second request waits for the first');
      assert.equal(await page.evaluate(() => !!document.querySelector('#app-dialog')?.open), false);
    }
    // A known :shortcode: in message text becomes its emoji; code, links, unknown names and clock-like text stay as they are.
    {
      const html = await second.evaluate(async () => {
        const v = new URL([...document.scripts].find(script => script.src.includes('/js/app.js')).src).search;
        const { expandShortcodes } = await import('/js/emoji-data.js' + v);
        const box = document.createElement('div'); box.innerHTML = 'thanks :pray: and :nope_not_real: at 10:30:15 <code>:pray:</code> <a href="#">:pray:</a>';
        document.body.append(box); expandShortcodes(box);
        await new Promise(resolve => { const check = () => (box.firstChild.nodeValue.includes(':pray:') ? setTimeout(check, 20) : resolve()); check(); });
        const out = box.innerHTML; box.remove(); return out;
      });
      assert.equal(html, 'thanks 🙏 and :nope_not_real: at 10:30:15 <code>:pray:</code> <a href="#">:pray:</a>', 'shortcodes in message text become emoji outside code and links');
    }
    // No experiment is running, so the pill is hidden. The message width is final: text keeps to about 80 characters, and code,
    // tables and plans use the whole bubble.
    {
      assert.deepEqual(await first.$$eval('.workspace-bar .ui-variant-toggle', nodes => nodes.map(el => el.hidden)), [true], 'with no experiment running the pill is hidden');
      const widths = await first.evaluate(async () => {
        const { renderMarkdown } = await import('/js/markdown.js');
        const host = document.createElement('div'); host.className = 'message-body'; host.style.width = '1200px';
        host.append(renderMarkdown('A paragraph of text.\n\n- a list item\n\n> a quote\n\n```\ncode\n```\n\n| a | b |\n| - | - |\n| 1 | 2 |'));
        document.querySelector('#messages').append(host);
        const cs = selector => { const el = host.querySelector(selector); return el ? getComputedStyle(el).maxWidth : null; };
        const out = { p: cs('p'), ul: cs('ul'), quote: cs('blockquote'), code: cs('.md-codeblock'), table: cs('.md-table-wrap, table'), wide: matchMedia('(min-width: 701px)').matches };
        host.remove(); return out;
      });
      assert.ok(widths.wide && parseFloat(widths.p) > 300 && widths.p === widths.ul && widths.p === widths.quote, `text is held to a measure: ${JSON.stringify(widths)}`);
      assert.ok(widths.code !== widths.p && widths.table !== widths.p, `code and tables are not held to it: ${JSON.stringify(widths)}`);
    }
    // The A/B/C pill is the experiment framework: hidden with no experiment, otherwise it names the test and cycles the options.
    const pill = await first.evaluate(async () => {
      const { defineExperiment, initUIVariant, activeVariant } = await import('/js/ui-variant.js');
      localStorage.removeItem('aimebu2.ui.variant.smoke'); localStorage.removeItem('aimebu2.ui.variant.other');
      defineExperiment('smoke', 'smoke question', ['first', 'second', 'third']);
      const make = () => { const old = document.querySelector('#ui-variant-toggle'); const button = old.cloneNode(true); button.id = 'ui-variant-smoke'; button.hidden = true; document.body.append(button); return button; };
      const events = []; document.addEventListener('ui-variant-change', event => events.push(event.detail.variant + ':' + event.detail.experiment));
      localStorage.setItem('aimebu2.ui.variant', 'c');
      const hidden = make(); initUIVariant(hidden, null);
      const legacyGone = localStorage.getItem('aimebu2.ui.variant') === null;
      const none = { legacyGone, hidden: hidden.hidden, ui: document.documentElement.dataset.ui ?? null, variant: activeVariant('smoke') };
      const button = make(); initUIVariant(button, 'smoke');
      const read = () => ({ hidden: button.hidden, active: button.dataset.active, testing: button.querySelector('.ui-variant-testing').textContent, label: button.getAttribute('aria-label'), variant: activeVariant('smoke'), other: activeVariant('other'), url: new URL(location.href).searchParams.get('ui') });
      const first = read(); button.click(); const second = read(); button.click(); button.click(); const wrapped = read();
      button.click(); const staleUrl = new URL(location.href).search; defineExperiment('other', 'another question', ['x', 'y', 'z']); const fresh = make(); initUIVariant(fresh, 'other'); const separate = { staleUrl, strippedUrl: new URL(location.href).search, active: fresh.dataset.active, saved: localStorage.getItem('aimebu2.ui.variant.smoke'), otherSaved: localStorage.getItem('aimebu2.ui.variant.other') }; fresh.remove(); localStorage.removeItem('aimebu2.ui.variant.other');
      hidden.remove(); button.remove(); delete document.documentElement.dataset.ui; delete document.documentElement.dataset.uiExperiment; localStorage.removeItem('aimebu2.ui.variant.smoke');
      { const bare = new URL(location.href); bare.searchParams.set('ui', 'b'); history.replaceState(history.state, '', bare); }
      localStorage.removeItem('aimebu2.ui.variant.smoke');
      const bareButton = make(); initUIVariant(bareButton, 'smoke'); const bareState = { active: bareButton.dataset.active, url: new URL(location.href).search }; bareButton.remove();
      { const forced = new URL(location.href); forced.searchParams.set('ui', 'smoke:c'); history.replaceState(history.state, '', forced); }
      const forcedButton = make(); initUIVariant(forcedButton, 'smoke'); const bare = { ...bareState, forced: forcedButton.dataset.active }; forcedButton.remove();
      localStorage.removeItem('aimebu2.ui.variant.smoke'); delete document.documentElement.dataset.ui; delete document.documentElement.dataset.uiExperiment;
      const url = new URL(location.href); url.searchParams.delete('ui'); history.replaceState(history.state, '', url);
      // More than three options: the pill follows the number of labels, up to E, and an option the experiment does not have is refused.
      defineExperiment('five', 'five-way question', ['one', 'two', 'three', 'four', 'five']); localStorage.removeItem('aimebu2.ui.variant.five'); delete document.documentElement.dataset.ui; delete document.documentElement.dataset.uiExperiment;
      const fiveButton = make(); initUIVariant(fiveButton, 'five');
      const letters = [...fiveButton.querySelectorAll('.ui-variant-opt')].map(item => item.textContent).join('');
      const seen = [fiveButton.dataset.active]; for (let i = 0; i < 5; i++) { fiveButton.click(); seen.push(fiveButton.dataset.active); }
      const fourth = (() => { fiveButton.click(); fiveButton.click(); fiveButton.click(); fiveButton.click(); return { active: fiveButton.dataset.active, label: fiveButton.getAttribute('aria-label'), url: new URL(location.href).searchParams.get('ui') }; })();
      fiveButton.remove(); delete document.documentElement.dataset.ui; delete document.documentElement.dataset.uiExperiment;
      { const forced = new URL(location.href); forced.searchParams.set('ui', 'five:e'); history.replaceState(history.state, '', forced); }
      const forcedFive = make(); initUIVariant(forcedFive, 'five'); const five = { letters, seen: seen.join(''), fourth, forced: forcedFive.dataset.active, variant: activeVariant('five') }; forcedFive.remove();
      delete document.documentElement.dataset.ui; delete document.documentElement.dataset.uiExperiment; localStorage.removeItem('aimebu2.ui.variant.five'); localStorage.removeItem('aimebu2.ui.variant.smoke');
      { const refused = new URL(location.href); refused.searchParams.set('ui', 'smoke:e'); history.replaceState(history.state, '', refused); }
      const threeButton = make(); initUIVariant(threeButton, 'smoke'); five.refused = { active: threeButton.dataset.active, url: new URL(location.href).search, letters: [...threeButton.querySelectorAll('.ui-variant-opt')].map(item => item.textContent).join('') }; threeButton.remove();
      localStorage.removeItem('aimebu2.ui.variant.smoke'); delete document.documentElement.dataset.ui; delete document.documentElement.dataset.uiExperiment;
      { const clean = new URL(location.href); clean.searchParams.delete('ui'); history.replaceState(history.state, '', clean); }
      return { none, first, second, wrapped, separate, bare, events, five };
    });
    assert.deepEqual(pill.five, { letters: 'ABCDE', seen: 'abcdea', fourth: { active: 'e', label: 'Testing five-way question: option E (five). Switch to A', url: 'five:e' }, forced: 'e', variant: 'e', refused: { active: 'a', url: '', letters: 'ABC' } }, 'the pill follows the number of options (up to E), and an option the experiment lacks is refused');
    assert.deepEqual(pill.none, { legacyGone: true, hidden: true, ui: null, variant: 'a' }, 'with no experiment the pill stays hidden and the old single saved choice is dropped');
    assert.deepEqual(pill.first, { hidden: false, active: 'a', testing: ' \u2014 TESTING: smoke question', label: 'Testing smoke question: option A (first). Switch to B', variant: 'a', other: 'a', url: null });
    assert.deepEqual([pill.second.active, pill.second.variant, pill.second.url, pill.second.label], ['b', 'b', 'smoke:b', 'Testing smoke question: option B (second). Switch to C'], 'a click moves to the next option and records it in the URL');
    assert.deepEqual([pill.wrapped.active, pill.wrapped.variant], ['a', 'a'], 'after C comes A again');
    assert.match(pill.separate.staleUrl, /ui=smoke%3Ab/);
    assert.deepEqual({ active: pill.separate.active, saved: pill.separate.saved, otherSaved: pill.separate.otherSaved, strippedUrl: pill.separate.strippedUrl }, { active: 'a', saved: 'b', otherSaved: 'a', strippedUrl: '' }, 'a saved choice and a stale ?ui= link belong to their own experiment: the next one starts at A and the stale parameter is stripped');
    assert.deepEqual(pill.bare, { active: 'a', url: '', forced: 'c' }, 'a bare ?ui=b is ignored and stripped, while ?ui=<experiment>:c forces that experiment');
    assert.deepEqual(pill.events.slice(0, 4), ['a:smoke', 'b:smoke', 'c:smoke', 'a:smoke'], 'each choice announces itself');
    // A fresh message can start a thread with the debug inspector off.
    assert.equal(await first.evaluate(() => document.documentElement.dataset.debugInspector), 'off', 'the inspector is off for this check');
    await domClick(first, '.message:first-child .message-actions .thread-action');
    await first.waitForFunction(() => !document.querySelector('#thread-panel').hidden);
    await domClick(first, '#close-thread');
    await first.hover('.message:first-child');
    await domClick(first, '.message:first-child .message-react');
    await first.waitForSelector('.message:first-child .message-reaction-row:not([hidden])');
    await domClick(first, '.message:first-child .message-reaction-row .quick-reaction');
    await first.waitForFunction(() => document.querySelector('.reaction-pill')?.textContent === '👍 1');
    assert.equal(await first.$eval('.reaction-pill', el => el.title), 'owner', 'reaction tooltip shows an internal user ID');
    await first.locator('.reaction-pill').click();
    await first.waitForFunction(() => !document.querySelector('.reaction-pill'));
    await first.hover('.message:first-child');
    await domClick(first, '.message-actions .message-reply');
    await first.locator('#composer textarea').fill('Reply body'); await first.locator('#composer button[type=submit]').click();
    await first.waitForFunction(() => document.querySelectorAll('.message').length === 2);
    assert.equal(await first.$eval('.message:nth-child(2) .quote', el => /^↪ #\d+ (You|owner)$/.test(el.textContent)), true);
    await first.hover('.message:first-child');
    await domClick(first, '.message:first-child .thread-action');
    await second.hover('.message:first-child');
    await domClick(second, '.message:first-child .thread-action');
    await first.locator('#thread-composer textarea').fill('Thread reply');
    // The thread's send button is the same green paper-plane square as the main composer's.
    const sendLook = selector => first.evaluate(selector => {
      const button = document.querySelector(selector), svg = button.querySelector('svg'), rect = button.getBoundingClientRect(), icon = svg.getBoundingClientRect(), cs = getComputedStyle(button);
      const probe = document.createElement('i'); probe.style.background = 'var(--green)'; document.body.append(probe); const green = getComputedStyle(probe).backgroundColor; probe.remove();
      return { square: Math.abs(rect.width - rect.height) <= 1, dx: Math.abs((icon.x + icon.width / 2) - (rect.x + rect.width / 2)), dy: Math.abs((icon.y + icon.height / 2) - (rect.y + rect.height / 2)), green: cs.backgroundColor === green && cs.borderTopColor === green, text: button.textContent.trim() };
    }, selector);
    await first.mouse.move(2, 2); // not hovered: the hover colour is the lighter green
    for (const theme of ['', 'high-contrast-dark', 'high-contrast-light']) {
      await first.evaluate(theme => { if (theme) document.documentElement.dataset.theme = theme; else delete document.documentElement.dataset.theme; }, theme);
      await new Promise(resolve => setTimeout(resolve, 400)); // past the colour transition
      for (const selector of ['#thread-composer button.btn-primary', '#composer button[type=submit]']) {
        const look = await sendLook(selector);
        assert.ok(look.square && look.dx <= 1 && look.dy <= 1 && look.green && look.text === '', `${selector} is a green square with a centred icon in ${theme || 'the default theme'}: ${JSON.stringify(look)}`);
      }
      // The members panel's Spawn button wears the same green, as text plus a glyph (the owner's exception to the icon-only rule).
      const spawn = await first.evaluate(() => {
        const button = document.querySelector('#spawn-open'), probe = document.createElement('i'); probe.style.background = 'var(--green)'; document.body.append(probe); const green = getComputedStyle(probe).backgroundColor; probe.remove();
        // Read with the transition off and the pointer state recorded: under load a colour that is still on its way, or a hover left
        // over from an earlier step, reads as the wrong green.
        button.style.transition = 'none'; void button.offsetWidth;
        const cs = getComputedStyle(button), text = [...button.querySelectorAll(':scope > span:not(.icon-mask)')].map(node => node.textContent).join('');
        const look = { green: cs.backgroundColor === green && cs.borderTopColor === green, text, glyph: !!button.querySelector('.icon-mask-spawn'), name: button.getAttribute('aria-label'), title: button.title };
        const detail = { hover: button.matches(':hover'), background: cs.backgroundColor, border: cs.borderTopColor, wanted: green };
        button.style.transition = '';
        return { look, detail };
      });
      assert.deepEqual(spawn.look, { green: true, text: 'Spawn', glyph: true, name: 'Spawn an agent', title: 'Spawn an agent…' }, `Spawn is green text plus a glyph in ${theme || 'the default theme'}: ${JSON.stringify(spawn.detail)}`);
    }
    await first.evaluate(() => { delete document.documentElement.dataset.theme; });
    await first.locator('#thread-composer button.btn-primary').click();
    await first.waitForFunction(() => document.querySelectorAll('.thread-entry').length === 2);
    await second.waitForFunction(() => [...document.querySelectorAll('.thread-entry .message-body')].some(el => el.textContent === 'Thread reply'));
    assert.equal(await first.$eval('.message:first-child .thread-action', el => el.title), 'Thread · 1');
    assert.equal(await second.$eval('.message:first-child .thread-action', el => el.title), 'Thread · 1');
    await domClick(first, '#close-thread'); await second.click('#close-thread');
    await first.locator('#composer textarea').fill('Unsent draft');
    const before = await first.$('.message[data-message-id="' + id + '"]');
    const csrf = await second.evaluate(async () => (await (await fetch('/api/me')).json()).csrf_token);
    async function post(body) { await second.evaluate(async ({ roomID, body, csrf }) => { const r = await fetch(`/api/rooms/${roomID}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ body }) }); if (!r.ok) throw new Error(await r.text()); }, { roomID, body, csrf }); }
    assert.equal(await first.$eval('#messages', el => el.hasAttribute('aria-live')), false, 'the message list must not be a live region');
    assert.equal(await first.$eval('#live-announcer', el => el.textContent), '', 'opening a room announces nothing');
    const announcerRules = await first.evaluate(async () => {
      const { LiveAnnouncer } = await import('/js/live-region.js');
      const log = [], announcer = new LiveAnnouncer(document.createElement('div'));
      announcer.queue = item => log.push(item.id);
      const mine = id => announcer.arrival({ id, mine: true, from: 'You', body: id, room: 'r' }), theirs = id => announcer.arrival({ id, mine: false, from: 'bob', body: id, room: 'r' });
      const steps = {};
      theirs('t1'); steps.others = [...log];
      log.length = 0; announcer.sendStarted(); mine('m1'); theirs('t2'); steps.heldWhileSending = [...log]; announcer.sendFinished('m1'); steps.ownDroppedOnResponse = [...log];
      log.length = 0; mine('m1'); steps.ownEventAfterResponse = [...log];
      log.length = 0; mine('x1'); steps.otherTabNoSend = [...log];
      log.length = 0; announcer.sendStarted(); mine('x2'); announcer.sendFinished('m2'); steps.otherTabReleased = [...log];
      log.length = 0; announcer.sendStarted(); mine('x3'); announcer.sendFinished(null); steps.failedSendReleases = [...log];
      log.length = 0; announcer.sendStarted(); announcer.sendStarted(); mine('x4'); announcer.sendFinished('m3'); steps.heldUntilAllSendsFinish = [...log]; announcer.sendFinished('m4'); steps.releasedAtEnd = [...log];
      // A batch that was just flushed has its text set 50 ms later; a reset in between (the room changed) must cancel it.
      const region = document.createElement('div'), reset = new LiveAnnouncer(region);
      reset.pending.push({ from: 'bob', body: 'old room' }); reset.flush(); reset.reset();
      await new Promise(resolve => setTimeout(resolve, 150));
      steps.spokenAfterReset = region.textContent;
      return steps;
    });
    assert.deepEqual(announcerRules, { others: ['t1'], heldWhileSending: ['t2'], ownDroppedOnResponse: ['t2'], ownEventAfterResponse: [], otherTabNoSend: ['x1'], otherTabReleased: ['x2'], failedSendReleases: ['x3'], heldUntilAllSendsFinish: [], releasedAtEnd: ['x4'], spokenAfterReset: '' }, 'own messages are silenced by id; same-account messages from another tab are announced');
    await post('Live update'); await first.waitForFunction(() => [...document.querySelectorAll('.message-body')].some(el => el.textContent === 'Live update'));
    await first.waitForFunction(() => document.querySelector('#live-announcer').textContent === 'New message from You: Live update', {}).catch(async () => { throw new Error(`live announcement was ${JSON.stringify(await first.$eval('#live-announcer', el => el.textContent))}`); });
    assert.equal(await first.$eval('#live-announcer', el => el.getAttribute('aria-live') + el.getAttribute('role')), 'politestatus');
    const after = await first.$('.message[data-message-id="' + id + '"]');
    assert.equal(await first.evaluate((x, y) => x === y, before, after), true, 'message node identity changed');
    assert.equal(await first.$eval('#composer textarea', el => el.value), 'Unsent draft');
    // Another tab of the same account posts right after this tab sent: only the other tab's message is announced.
    await first.locator('#composer textarea').fill('Own message'); await first.keyboard.press('Enter');
    await first.waitForFunction(() => [...document.querySelectorAll('.message-body')].some(el => el.textContent === 'Own message'));
    await post('Other tab');
    await first.waitForFunction(() => [...document.querySelectorAll('.message-body')].some(el => el.textContent === 'Other tab'));
    await first.waitForFunction(() => document.querySelector('#live-announcer').textContent === 'New message from You: Other tab', {}).catch(async () => { throw new Error(`same-account announcement was ${JSON.stringify(await first.$eval('#live-announcer', el => el.textContent))}`); });
    for (let i = 0; i < 22; i++) await post(`Filler ${i}`);
    await first.waitForFunction(() => document.querySelectorAll('.message').length >= 25);
    await first.$eval('#messages', el => { el.scrollTop = 100; });
    const scrollBefore = await first.$eval('#messages', el => el.scrollTop);
    await post('While scrolled up'); await first.waitForFunction(() => [...document.querySelectorAll('.message-body')].some(el => el.textContent === 'While scrolled up'));
    const scrollAfter = await first.$eval('#messages', el => el.scrollTop);
    assert.equal(scrollAfter, scrollBefore, 'scroll jumped on live append');
    await first.waitForFunction(() => /^\d+ new messages in /.test(document.querySelector('#live-announcer').textContent), { timeout: 15000 }); // the 22-message burst is coalesced
    for (let i = 0; i < 120; i++) await post(`History ${i}`);
    await first.waitForFunction(() => [...document.querySelectorAll('.message-body')].some(el => el.textContent === 'History 119'));
    await first.evaluate(async () => { await document.querySelector('.room-row.active .room-name').onclick(); });
    await first.waitForFunction(() => document.querySelector('#messages').dataset.hasOlder === 'true');
    assert.equal(await first.$('button.load-older'), null, 'there is no Load older messages button: older pages load on scroll');
    assert.equal(await first.$eval('#live-announcer', el => el.textContent), '', 're-opening a room announces nothing');
    const beforeOlder = await first.$$eval('.message', els => els.length);
    // Keyboard only: put the reader in the middle of the list, focus a message, press Home. That scrolls to the top and the older page loads.
    // The re-opened room is still settling when this runs (late layout from the 100 rows, live arrivals of the burst above): a resize
    // callback that lands between the key's scroll and the scroll event re-pins the list to the bottom and the older page is never
    // asked for. So the list is first waited until its height and position are unchanged for several frames, both before and after
    // the click, and the key is pressed on a quiet list. On a failure the page names its state and the scroll anchor's decision log.
    const quiet = () => first.evaluate(() => new Promise(resolve => {
      const list = document.querySelector('#messages'); let last = '', stable = 0;
      const tick = () => { const now = [list.scrollTop, list.scrollHeight, document.querySelectorAll('.message').length].join(); stable = now === last ? stable + 1 : 0; last = now; if (stable >= 5) resolve(true); else requestAnimationFrame(tick); };
      requestAnimationFrame(tick);
    }));
    await first.evaluate(() => { window.__anchorLog = []; });
    await quiet();
    await first.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = Math.floor((list.scrollHeight - list.clientHeight) / 2); });
    await quiet();
    await first.click('#messages .message:nth-child(40) .message-body');
    await quiet();
    await first.keyboard.press('Home');
    await waitDump(first, 'keyboard Home', count => document.querySelectorAll('.message').length > count,
      () => ({ messages: document.querySelectorAll('.message').length, scrollTop: document.querySelector('#messages').scrollTop, scrollHeight: document.querySelector('#messages').scrollHeight, hasOlder: document.querySelector('#messages').dataset.hasOlder, status: document.querySelector('.older-status')?.hidden, anchorLog: (window.__anchorLog || []).slice(-30) }), beforeOlder);
    await first.evaluate(() => { delete window.__anchorLog; });
    // A reader's scroll that lands in a frame after its scroll-event step, followed by a resize callback in the same frame, must not be undone:
    // the pinned list is not dragged back to the bottom (that swallowed the older-page request above). A detached list of its own, so no
    // message of the room moves.
    assert.deepEqual(await first.evaluate(async () => {
      const { ScrollAnchor } = await import('/js/scroll-anchor.js');
      const list = document.createElement('div'); list.style.cssText = 'position:fixed;left:0;top:0;width:200px;height:300px;overflow:auto;overflow-anchor:none;visibility:hidden';
      for (let i = 0; i < 30; i++) { const item = document.createElement('article'); item.className = 'message'; item.style.height = '60px'; list.append(item); }
      document.body.append(list);
      const frames = count => new Promise(resolve => { const tick = () => --count <= 0 ? resolve() : requestAnimationFrame(tick); requestAnimationFrame(tick); });
      const anchor = new ScrollAnchor(list); list.scrollTop = list.scrollHeight; anchor.update(); await frames(5);
      await new Promise(resolve => requestAnimationFrame(() => { list.scrollTop = 0; list.children[10].style.height = '80px'; resolve(); }));
      await frames(6); const stayed = list.scrollTop;
      list.remove();
      return { stayedAtTop: stayed === 0 };
    }), { stayedAtTop: true }, 'a pinned list was dragged back to the bottom over a reader scroll that had not been delivered yet');
    await new Promise(resolve => setTimeout(resolve, 1500));
    assert.equal(await first.$eval('#live-announcer', el => el.textContent), '', 'loading older history must not announce');
    // Narrow columns: the viewer's own long message stays readable (it is not squeezed to leave room for the toolbar beside it), and the
    // hover toolbar of an own message and of someone else's stays inside the list and covers neither the text nor any item of the sender
    // line (name, #number, time), at a window with both panels open (a column under 420px) and at the usual one.
    {
      await post('Narrow column check. ' + 'This is a long message of my own that wraps across lines. '.repeat(8));
      await first.waitForFunction(() => [...document.querySelectorAll('.message.self .message-body')].some(el => el.textContent.startsWith('Narrow column check.')));
      await first.evaluate(() => {
        const own = [...document.querySelectorAll('.message.self')].find(item => item.querySelector('.message-body').textContent.startsWith('Narrow column check.'));
        own.dataset.narrowCheck = 'own';
        const other = own.cloneNode(true); other.classList.remove('self'); other.dataset.narrowCheck = 'other'; // someone else's message, laid out by the same rules
        own.after(other);
      });
      for (const width of [800, 1100]) {
        await first.setViewport({ width, height: 720 });
        for (const kind of ['own', 'other']) {
          const message = `[data-narrow-check=${kind}]`;
          await first.evaluate(selector => document.querySelector(selector).scrollIntoView({ block: 'center' }), message);
          await first.hover(`${message} .message-bubble`);
          await first.waitForFunction(selector => getComputedStyle(document.querySelector(`${selector} .message-actions`)).opacity === '1', {}, message);
          const geometry = await first.evaluate(selector => {
            const box = node => node.getBoundingClientRect(), hit = (a, b) => !(a.right <= b.left || a.left >= b.right || a.bottom <= b.top || a.top >= b.bottom);
            const list = box(document.querySelector('#messages')), root = document.querySelector(selector), bubble = box(root.querySelector('.message-bubble')), bar = box(root.querySelector('.message-actions')), text = box(root.querySelector('.message-body'));
            const items = [...root.querySelector('.message-meta').children].filter(node => node.getClientRects().length).map(box);
            return { readable: bubble.width >= Math.min(0.6 * list.width, 200), insideList: bubble.right <= list.right + 1 && bar.left >= list.left - 1 && bar.right <= list.right + 1, clearOfText: !hit(bar, text), clearOfSenderLine: !items.some(item => hit(bar, item)), list: Math.round(list.width), bubble: Math.round(bubble.width) };
          }, message);
          assert.deepEqual([geometry.readable, geometry.insideList, geometry.clearOfText, geometry.clearOfSenderLine], [true, true, true, true], `${kind} message at ${width}px: ${JSON.stringify(geometry)}`);
        }
      }
      await first.setViewport({ width: 1100, height: 720 });
      await first.evaluate(() => { document.querySelector('[data-narrow-check=other]').remove(); delete document.querySelector('[data-narrow-check=own]').dataset.narrowCheck; });
    }
    const markdown = '## Heading\n\n**bold** *italic* `inline`\n\n- one\n- two\n\n> quoted\n\n| Left | Right |\n| :--- | ---: |\n| cell | **value** |\n\n[Safe](https://example.com/path) [Bad](javascript:alert(1))\n\n```js\nconst n = 1;\n```';
    await post(markdown);
    await first.waitForFunction(() => [...document.querySelectorAll('.message-body h2')].some(el => el.textContent === 'Heading'));
    const md = await first.$('.message:last-child .message-body');
    assert.equal(await md.$eval('strong', el => el.textContent), 'bold');
    assert.equal(await md.$eval('.md-table-wrap table tbody tr td:last-child strong', el => el.textContent), 'value');
    assert.deepEqual(await md.$$eval('.md-table-wrap th', cells => cells.map(el => getComputedStyle(el).textAlign)), ['left', 'right']);
    assert.equal(await md.$eval('pre code', el => getComputedStyle(el).display), 'block');
    const tricky = '| L | C | R |\n| :--- | :---: | ---: |\n| **safe** | `a|b` | <img src=x onerror=alert(1)> |\n| short |\n| extra | middle | one | more |';
    const tableCheck = await first.evaluate(async source => {
      const { renderMarkdown } = await import('/js/markdown.js');
      const host = document.createElement('div'); host.className = 'message-body'; host.append(renderMarkdown(source)); document.body.append(host);
      const result = { rows: host.querySelectorAll('tbody tr').length, cells: [...host.querySelectorAll('tbody tr')].map(row => row.cells.length), align: [...host.querySelectorAll('thead th')].map(el => getComputedStyle(el).textAlign), html: host.innerHTML, last: host.querySelector('tbody tr:last-child td:last-child').textContent };
      host.remove(); return result;
    }, tricky);
    assert.deepEqual(tableCheck.cells, [3, 3, 3]);
    assert.deepEqual(tableCheck.align, ['left', 'center', 'right']);
    assert.equal(tableCheck.rows, 3);
    assert.equal(tableCheck.html.includes('<img'), false, 'table cell HTML must stay escaped');
    assert.equal(tableCheck.last, 'one | more');
    // Underline is <u>…</u>: it renders as underline (with other marks inside), and nothing else in angle brackets becomes markup.
    const underlineCheck = await first.evaluate(async () => {
      const { renderMarkdown } = await import('/js/markdown.js');
      const html = source => { const host = document.createElement('div'); host.append(renderMarkdown(source)); return host.innerHTML; };
      return { plain: html('a <u>under</u> b'), nested: html('<u>**bold** in</u>'), open: html('<u>never closed'), empty: html('<u></u>'), other: html('<b>x</b> <script>1</script> <u><img src=x onerror=1></u>') };
    });
    assert.equal(underlineCheck.plain, '<p>a <u>under</u> b</p>');
    assert.equal(underlineCheck.nested, '<p><u><strong>bold</strong> in</u></p>');
    assert.equal(underlineCheck.open, '<p>&lt;u&gt;never closed</p>');
    assert.equal(underlineCheck.empty, '<p>&lt;u&gt;&lt;/u&gt;</p>');
    assert.equal(/<(b|script|img)[ >]/.test(underlineCheck.other), false, 'angle brackets other than <u> must stay text');
    const shortRules = await first.evaluate(async () => {
      const { renderMarkdown } = await import('/js/markdown.js');
      const host = document.createElement('div'); host.append(renderMarkdown('| Center | Right | Both | Plain |\n| :-: | -: | :- | - |\n| c | r | b | p |'));
      return { table: !!host.querySelector('table'), align: [...host.querySelectorAll('th')].map(cell => cell.className) };
    });
    assert.equal(shortRules.table, true, 'one-dash GFM table delimiters render a table');
    assert.deepEqual(shortRules.align, ['md-th md-align-center', 'md-th md-align-right', 'md-th md-align-left', 'md-th md-align-left']);
    const rules = await first.evaluate(async () => {
      const { renderMarkdown, renderRaw, copyText } = await import('/js/markdown.js');
      const render = source => { const host = document.createElement('div'); host.append(renderMarkdown(source)); return host; };
      const upper = render('See HTTPS://Example.com/Path now');
      const auto = render('see https://example.com/a_b_c?x=1#frag, and (https://example.org/p) or <https://bad.test/"x> end. https://example.net/.');
      const ref = render('see #42 and #0 and a#7 and https://x.test/p#42 and `#9` and [t](https://y.test/#5)');
      const mail = render('[me](mailto:a@b.test) [js](javascript:x) [data](data:text/html,x)');
      const strike = render('~~gone~~ ~one~ **~~both~~**');
      const code = render('para\n\n    indented <b>x</b>\n    second\n\nafter\n    not code');
      const raw = document.createElement('div'); raw.append(renderRaw('text\n```js\nlet a;\n```\nmid\n\n    four\n    spaces\n\n~~~\ntilde\n~~~'));
      const writes = []; const original = document.execCommand;
      Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
      document.execCommand = command => { writes.push([command, document.activeElement.value]); return true; };
      await copyText('fallback payload'); document.execCommand = original; delete navigator.clipboard;
      return {
        upper: [...upper.querySelectorAll('a')].map(a => a.textContent),
        autoLinks: [...auto.querySelectorAll('a')].map(a => [a.textContent, a.getAttribute('href'), a.rel]),
        refs: [...ref.querySelectorAll('a.msg-ref')].map(a => [a.textContent, a.dataset.no]), refLinks: [...ref.querySelectorAll('a:not(.msg-ref)')].map(a => a.getAttribute('href')),
        mail: [...mail.querySelectorAll('a')].map(a => a.getAttribute('href')), mailText: mail.textContent,
        strike: [...strike.querySelectorAll('del')].map(el => el.textContent), strikeText: strike.textContent,
        indented: [...code.querySelectorAll('pre code')].map(el => el.textContent), codeHTML: code.innerHTML.includes('<b>'), paragraphs: [...code.querySelectorAll('p')].map(el => el.textContent),
        rawBlocks: [...raw.querySelectorAll('.raw-codeblock code')].map(el => el.textContent), rawButtons: raw.querySelectorAll('.raw-codeblock button.md-copy').length, rawText: raw.textContent.startsWith('text'),
        writes,
      };
    });
    assert.deepEqual(rules.autoLinks.map(item => item[0]), ['https://example.com/a_b_c?x=1#frag', 'https://example.org/p', 'https://example.net/'], 'bare URLs autolink without trailing punctuation or angle-bracket forms');
    assert.deepEqual(rules.upper, ['HTTPS://Example.com/Path'], 'uppercase schemes autolink');
    assert.equal(rules.autoLinks.every(item => item[1] === item[0] && item[2] === 'noopener noreferrer'), true);
    assert.deepEqual(rules.refs, [['#42', '42']], '#seq links skip #0, word-attached, URL fragments, code, and link text');
    assert.deepEqual(rules.refLinks, ['https://x.test/p#42', 'https://y.test/#5']);
    assert.deepEqual(rules.mail, ['mailto:a@b.test']); assert.equal(rules.mailText, 'me js data');
    assert.deepEqual(rules.strike, ['gone', 'both']); assert.equal(rules.strikeText, 'gone ~one~ both');
    assert.deepEqual(rules.indented, ['indented <b>x</b>\nsecond']); assert.equal(rules.codeHTML, false); assert.deepEqual(rules.paragraphs, ['para', 'after\n    not code'.replace('\n', '')]);
    assert.deepEqual(rules.rawBlocks, ['```js\nlet a;\n```', '    four\n    spaces', '~~~\ntilde\n~~~']); assert.equal(rules.rawButtons, 3); assert.equal(rules.rawText, true);
    assert.deepEqual(rules.writes, [['copy', 'fallback payload']], 'copy falls back to execCommand without a clipboard API');
    // Header and top-bar buttons carry v1's icons (viewBox fingerprints of v1's own SVGs), never a typed glyph.
    assert.deepEqual(await first.evaluate(() => Object.fromEntries(['#knowledge-toggle', '#settings-toggle', '#usages-toggle', '#room-info', '#room-jump', '#room-export', '#room-raw', '#composer button[type=submit]', '#system-events-toggle'].map(id => { const el = document.querySelector(id); return [id, { text: [...el.childNodes].filter(node => node.nodeType === 3).map(node => node.textContent).join('').trim(), boxes: [...el.querySelectorAll('svg')].map(svg => svg.getAttribute('viewBox')) }]; }))), {
      '#knowledge-toggle': { text: '', boxes: ['0 0 512 512'] }, '#settings-toggle': { text: '', boxes: ['0 0 16 16'] }, '#usages-toggle': { text: '', boxes: ['0 0 24 24'] },
      '#room-info': { text: '', boxes: ['0 0 16 16'] }, '#room-jump': { text: '', boxes: ['0 0 24 24'] }, '#room-export': { text: '', boxes: ['0 0 490.009 490.009'] },
      '#room-raw': { text: '', boxes: ['0 0 24 24', '0 0 24 24'] }, '#composer button[type=submit]': { text: '', boxes: ['0 0 24 24'] }, '#system-events-toggle': { text: '', boxes: ['0 0 400 512'] },
    }, 'header and top-bar buttons use v1 icons');
    assert.deepEqual(await first.evaluate(() => [...document.querySelectorAll('#room-raw svg')].map(svg => svg.getAttribute('class') + ':' + (getComputedStyle(svg).display !== 'none'))), ['icon-rendered:false', 'icon-raw:true'], 'while rendered the toggle shows the raw-code icon (v1)');
    await domClick(first, '#room-raw');
    assert.equal(await first.$eval('#room-raw', el => el.getAttribute('aria-pressed')), 'true');
    assert.equal(await first.$eval('.message:last-child .message-body', el => el.textContent.startsWith('## Heading')), true);
    assert.deepEqual(await first.evaluate(() => [...document.querySelectorAll('#room-raw svg')].map(svg => svg.getAttribute('class') + ':' + (getComputedStyle(svg).display !== 'none'))), ['icon-rendered:true', 'icon-raw:false'], 'while raw the toggle shows the eye icon (v1)');
    await domClick(first, '#room-raw');
    assert.equal(await first.$eval('.message:last-child .message-body h2', el => el.textContent), 'Heading');
    jumpSeq = String(await first.evaluate(async id => { const messages = await (await fetch(`/api/rooms/${id}/messages`)).json(); return messages.find(item => item.body === 'Thread reply').no; }, roomID));
    await domClick(first, '#room-jump');
    await first.waitForFunction(() => !document.querySelector('#thread-panel').hidden && [...document.querySelectorAll('.thread-entry .message-body')].some(el => el.textContent === 'Thread reply'));
    // A long unbroken word and a long sentence must wrap inside the panel, not run past it.
    await first.evaluate(async id => {
      const csrf = (await (await fetch('/api/me')).json()).csrf_token;
      const messages = await (await fetch(`/api/rooms/${id}/messages`)).json();
      const reply = messages.find(message => message.body === 'Thread reply');
      const response = await fetch(`/api/rooms/${id}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ body: `Long word: ${'x'.repeat(240)} and a long sentence ${'that keeps going '.repeat(14)}`, thread_root: reply.thread_root || reply.id }) });
      if (!response.ok) throw new Error(`long reply: ${response.status}`);
    }, roomID);
    await first.waitForFunction(() => [...document.querySelectorAll('#thread-messages .message-body')].some(el => el.textContent.startsWith('Long word: ')));
    // Thread entries use the main list's header (sender, #number, bug, time, spaced alike) and their text wraps inside the panel.
    const threadLayoutViewport = first.viewport();
    for (const width of [1440, 1100, 390]) {
      await first.setViewport({ ...threadLayoutViewport, width });
      const layout = await first.evaluate(() => {
        document.documentElement.dataset.debugInspector = 'on';
        const panel = document.querySelector('#thread-panel').getBoundingClientRect();
        const columnEnd = Math.max(...[...document.querySelectorAll('#messages .message-bubble')].map(bubble => bubble.getBoundingClientRect().right));
        window.__columnClear = innerWidth <= 700 || !(columnEnd > panel.left + 1);
        return [...document.querySelectorAll('#thread-messages .thread-entry')].map(entry => {
          const meta = entry.querySelector(':scope > .message-meta');
          const items = meta ? [...meta.children].filter(node => node.getClientRects().length).map(node => ({ kind: node.classList.contains('message-number') ? 'number' : node.classList.contains('chat-msg-debug-toggle') ? 'bug' : node.classList.contains('message-author-wrap') ? 'author' : node.tagName.toLowerCase(), left: node.getBoundingClientRect().left, right: node.getBoundingClientRect().right })) : [];
          const body = entry.querySelector('.message-body');
          return { meta: !!meta, kinds: items.map(item => item.kind), gaps: items.slice(1).map((item, index) => Math.round(item.left - items[index].right)), bodyFits: body.scrollWidth <= body.clientWidth, inside: entry.getBoundingClientRect().right <= panel.right + 1 && Math.max(...items.map(item => item.right)) <= panel.right + 1 };
        });
      });
      assert.equal(await first.evaluate(() => window.__columnClear), true, `messages in the main list run under the open thread panel at ${width}px`);
      assert.equal(await first.evaluate(() => { const panel = document.querySelector('#thread-panel').getBoundingClientRect(), members = document.querySelector('#agents-panel'); if (innerWidth <= 700 || getComputedStyle(members).display === 'none') return true; const box = members.getBoundingClientRect(); return panel.right <= box.left + 1 || panel.left >= box.right - 1; }), true, `the thread panel lies over the members column at ${width}px`);
      await first.evaluate(() => { document.documentElement.dataset.debugInspector = 'off'; });
      assert.ok(layout.length > 0, 'no thread entries');
      for (const entry of layout) {
        assert.equal(entry.meta, true, `a thread entry has no message header at ${width}px`);
        assert.deepEqual(entry.kinds.slice(0, 3), ['author', 'number', 'bug'], `thread header order at ${width}px: ${entry.kinds}`);
        assert.ok(entry.gaps.every(gap => gap >= 4), `thread header items touch at ${width}px: gaps ${entry.gaps}`);
        assert.equal(entry.bodyFits && entry.inside, true, `a thread entry overflows the panel at ${width}px: ${JSON.stringify(entry)}`);
      }
    }
    await first.setViewport(threadLayoutViewport);
    await domClick(first, '#close-thread');
    await first.evaluate(() => {
      window.__originalCreateObjectURL = URL.createObjectURL;
      window.__originalRevokeObjectURL = URL.revokeObjectURL;
      window.__originalAnchorClick = HTMLAnchorElement.prototype.click;
      URL.createObjectURL = blob => { window.__exportBlob = blob; return 'blob:test'; };
      URL.revokeObjectURL = () => {};
      HTMLAnchorElement.prototype.click = function() { window.__downloadName = this.download; };
    });
    await domClick(first, '#room-export'); await first.click('#room-export-dialog [data-format="markdown"]');
    await first.waitForFunction(() => !!window.__downloadName);
    assert.equal(await first.evaluate(() => window.__downloadName), 'Nested-task.md');
    assert.equal(await first.evaluate(async () => (await window.__exportBlob.text()).includes('Thread reply')), true);
    await domClick(first, '#room-export'); await first.click('#room-export-dialog [data-format="json"]');
    await first.waitForFunction(() => window.__downloadName?.endsWith('.json'));
    assert.equal(await first.evaluate(async () => JSON.parse(await window.__exportBlob.text()).messages.some(item => item.body === 'Thread reply')), true);
    await first.evaluate(() => { URL.createObjectURL = window.__originalCreateObjectURL; URL.revokeObjectURL = window.__originalRevokeObjectURL; HTMLAnchorElement.prototype.click = window.__originalAnchorClick; });
    assert.equal(await md.$eval('em', el => el.textContent), 'italic');
    assert.equal(await md.$eval('table tbody td:last-child strong', el => el.textContent), 'value');
    assert.equal(await md.$eval('a', el => el.getAttribute('rel').includes('noopener') && el.protocol === 'https:'), true);
    assert.equal(await md.$$eval('a', els => els.length), 1, 'unsafe link created');
    assert.equal(await md.$eval('.md-codeblock code', el => el.textContent), 'const n = 1;');
    assert.equal(await md.$eval('.md-copy', el => el.getAttribute('aria-label')), 'Copy code block');
    const xss = '<script>alert(1)</script> <img src=x onerror=alert(2)> [click](javascript:evil) ``nested ` ticks``';
    await post(xss);
    await first.waitForFunction(() => [...document.querySelectorAll('.message-body')].some(el => el.textContent.includes('<script>alert(1)</script>')));
    assert.equal(await first.$eval('.message:last-child .message-body', el => el.querySelectorAll('script,img,a').length), 0, 'XSS markup became DOM');
    assert.equal(await first.$eval('.message:last-child .message-body', el => el.textContent.includes('onerror=alert(2)')), true);
    assert.equal(await first.$eval('.message:last-child .message-body code', el => el.textContent), 'nested ` ticks');
    assert.deepEqual(await first.$$eval('.message:last-child .quick-reaction', els => els.map(el => el.textContent)), ['👍', '🆗', '✅', '👀', '🙏', '🎉']);
    const actualProjectID = await first.evaluate(async () => (await (await fetch('/api/projects')).json()).find(p => p.name === 'UI smoke').id);
    // Message numbers: "#N" is one sequence across every room, shown and written by people. A #N in a message jumps to that message, in
    // another room too (it opens that room), and an unknown number says so.
    {
      const originalTitle = await first.$eval('#room-title', el => el.textContent);
      const other = await first.evaluate(async projectID => {
        const { csrf_token } = await (await fetch('/api/me')).json();
        const call = async (path, body) => { const response = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify(body) }); if (!response.ok) throw new Error(`${path}: ${response.status}`); return response.json(); };
        const rooms = await (await fetch(`/api/projects/${projectID}/rooms`)).json();
        const room = rooms.find(item => item.kind === 'task' && item.lifecycle === 'open' && item.id !== location.hash.slice(1));
        const message = await call(`/api/rooms/${room.id}/messages`, { body: 'a message in another room' });
        return { roomID: room.id, title: room.title, no: message.no };
      }, actualProjectID);
      assert.ok(other.no > 0, 'a posted message has a number');
      await post(`see #${other.no} and #999999`);
      await first.waitForFunction(() => document.querySelectorAll('#messages a.msg-ref').length >= 2);
      const refs = await first.$$eval('#messages a.msg-ref', links => links.slice(-2).map(link => [link.textContent, link.dataset.no]));
      assert.deepEqual(refs, [[`#${other.no}`, String(other.no)], ['#999999', '999999']], 'both numbers are links');
      await first.evaluate(() => [...document.querySelectorAll('#messages a.msg-ref')].at(-1).click());
      await first.waitForFunction(() => /does not exist or is not visible/.test(document.querySelector('#chat-error').textContent));
      await first.evaluate(() => { document.querySelector('#chat-error').textContent = ''; [...document.querySelectorAll('#messages a.msg-ref')].at(-2).click(); });
      await first.waitForFunction(title => document.querySelector('#room-title').textContent === title && [...document.querySelectorAll('#messages .message-body')].some(el => el.textContent === 'a message in another room'), {}, other.title);
      assert.equal(await first.evaluate(() => [...document.querySelectorAll('#messages .message')].find(item => item.querySelector('.message-body')?.textContent === 'a message in another room').querySelector('.message-number').textContent), `#${other.no}`, 'the message shows its number, not its internal order');
      await first.evaluate(title => [...document.querySelectorAll('.room-name')].find(node => node.textContent.includes(title)).click(), originalTitle);
      await first.waitForFunction(title => document.querySelector('#room-title').textContent === title, {}, originalTitle);
    }
    assert.ok(actualProjectID);
    assert.equal(await first.title(), 'aimebu');
    // The chat never scrolls sideways: wide content (code, tables, URLs, plans, diffs) scrolls inside its own block or wraps, at desktop
    // and phone widths. The messages are rendered by the real Chat into the real list and removed again.
    {
      const before = first.viewport();
      for (const width of [1440, 390]) {
        await first.setViewport({ width, height: 900 });
        const fit = await first.evaluate(async () => {
          const { Chat } = await import('/js/chat.js');
          const chat = Object.create(Chat.prototype);
          const list = document.querySelector('#messages'), kept = new Set(list.children);
          chat.list = list; chat.messages = new Map(); chat.nodes = new Map(); chat.threadCounts = new Map(); chat.agents = new Map(); chat.raw = false; chat.refreshReactions = () => {};
          const long = 'verylongidentifier'.repeat(20), bodies = [
            '```\n' + 'x'.repeat(400) + '\n```',
            '| a | b | c | d | e |\n|---|---|---|---|---|\n| ' + Array(5).fill('cell '.repeat(30)).join(' | ') + ' |',
            'https://example.test/' + 'a'.repeat(400), 'a'.repeat(500), '`' + 'inline'.repeat(80) + '`', '> ' + 'q'.repeat(400),
            '# ' + 'H'.repeat(300) + '\n\n1. item\n   ```\n   ' + 'n'.repeat(300) + '\n   ```\n2. [' + 'link'.repeat(80) + '](https://e.test/x)',
            '```diff\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-short\n+' + 'long '.repeat(120) + '\n```',
          ];
          bodies.forEach((body, index) => chat.add({ id: `9${index}`, seq: 9000, no: 9000 + index, room_id: 'x', author_kind: 'agent', author_id: 'bot@repo', author_harness: 'codex', author_model: 'm', body, created_at: '2026-10-14T00:00:00Z' }));
          chat.add({ id: '99', seq: 9099, no: 9099, room_id: 'x', author_kind: 'agent', author_id: 'bot@repo', author_harness: 'codex', author_model: 'm', body: 'plan', created_at: '2026-10-14T00:00:00Z', visual_plan: [
            { type: 'markdown', data: { markdown: '| a | b |\n|---|---|\n| ' + 'cell '.repeat(60) + ' | z |\n\n```\n' + 'x'.repeat(300) + '\n```\n\nhttps://example.test/' + 'a'.repeat(300) } },
            { type: 'file-tree', data: { root: { name: long, type: 'dir', note: 'n'.repeat(300), children: [{ name: long + '.go', note: 'm'.repeat(300) }] } } },
            { type: 'data-model', data: { entities: [{ name: long, fields: [{ name: long, type: 'string'.repeat(30), note: 'q'.repeat(200) }] }] } },
            { type: 'api-endpoint', data: { method: 'POST', path: '/api/' + long, notes: 'z'.repeat(400) } },
            { type: 'annotated-code', data: { code: 'a'.repeat(400), annotations: [{ line: 1, note: 'note '.repeat(60) }] } },
            { type: 'canvas', data: { nodes: [{ id: 'a', x: 0, y: 0, label: long }, { id: 'b', x: 2000, y: 100, label: 'far' }], edges: [] } },
            { type: 'diff', data: { diff: '--- a/f\n+++ b/f\n@@ -1 +1 @@\n-short\n+' + 'long '.repeat(120) } },
          ], appendix_pages: [{ title: 'T', body: '| a | b |\n|---|---|\n| ' + 'x'.repeat(300) + ' | y |\n\n```\n' + 'c'.repeat(400) + '\n```' }] });
          await new Promise(resolve => setTimeout(resolve, 300));
          document.querySelectorAll('#messages details').forEach(item => { item.open = true; });
          await new Promise(resolve => setTimeout(resolve, 200));
          const box = list.getBoundingClientRect();
          const out = { overflowX: getComputedStyle(list).overflowX, scrollWidth: list.scrollWidth, clientWidth: list.clientWidth,
            outside: [...list.children].filter(node => !kept.has(node)).filter(node => node.getBoundingClientRect().right > box.right + 1).length };
          for (const node of [...list.children]) if (!kept.has(node)) node.remove();
          return out;
        });
        assert.deepEqual([fit.overflowX, fit.scrollWidth <= fit.clientWidth, fit.outside], ['hidden', true, 0], `the chat scrolls sideways at ${width}px: ${JSON.stringify(fit)}`);
      }
      await first.setViewport(before);
    }
    // Hovering a thumbnail never moves the image out of its frame: the frame (the button, whose border shows on hover) hugs the image, the
    // image stays inside it before and while the pointer is over it, and a long file name never widens the figure past the image.
    {
      const added = await first.evaluate(async () => {
        const { Chat } = await import('/js/chat.js');
        const chat = Object.create(Chat.prototype);
        const list = document.querySelector('#messages');
        chat.list = list; chat.messages = new Map(); chat.nodes = new Map(); chat.threadCounts = new Map(); chat.agents = new Map(); chat.raw = false; chat.refreshReactions = () => {};
        const pic = (w, h) => 'data:image/svg+xml,' + encodeURIComponent(`<svg xmlns="http://www.w3.org/2000/svg" width="${w}" height="${h}"><rect width="${w}" height="${h}" fill="steelblue"/></svg>`);
        const kept = new Set(list.children);
        chat.add({ id: '9801', seq: 9801, no: 9801, room_id: 'x', author_kind: 'agent', author_id: 'bot@repo', author_harness: 'codex', author_model: 'm', body: 'pictures', created_at: '2026-10-14T00:00:00Z', attachments: [
          { mime: 'image/png', url: pic(900, 450), name: 'a-very-long-file-name-that-must-not-widen-its-figure-' + 'x'.repeat(80) + '.png', width: 900, height: 450 },
          { mime: 'image/png', url: pic(200, 100), name: 'small.png', width: 200, height: 100 },
          { mime: 'image/png', url: pic(300, 900), name: 'tall.png', width: 300, height: 900 }] });
        list.lastElementChild.dataset.thumbProbe = '1';
        list.lastElementChild.scrollIntoView();
        await new Promise(resolve => setTimeout(resolve, 400));
        return [...list.children].filter(node => !kept.has(node)).length;
      });
      assert.equal(added, 1);
      const frames = () => first.evaluate(() => [...document.querySelectorAll('[data-thumb-probe] .image-item')].map(item => {
        const figure = item.getBoundingClientRect(), frame = item.querySelector('.image-thumb').getBoundingClientRect(), img = item.querySelector('img').getBoundingClientRect(), caption = item.querySelector('figcaption').getBoundingClientRect();
        const style = getComputedStyle(item.querySelector('.image-thumb')), shot = getComputedStyle(item.querySelector('img'));
        return { inside: img.left >= frame.left - 0.5 && img.right <= frame.right + 0.5 && img.top >= frame.top - 0.5 && img.bottom <= frame.bottom + 0.5,
          hugs: Math.round(frame.width - img.width) <= 2 && Math.round(frame.height - img.height) <= 2, captionFits: caption.width <= frame.width + 0.5 && figure.width <= frame.width + 0.5,
          padding: style.padding, clipped: style.overflow, transform: shot.transform, widthPx: Math.round(img.width), heightPx: Math.round(img.height) };
      }));
      const expectFrames = async when => {
        const list = await frames();
        assert.equal(list.length, 3, `${when}: three thumbnails`);
        for (const [index, frame] of list.entries()) {
          assert.deepEqual([frame.inside, frame.hugs, frame.captionFits, frame.padding, frame.clipped, frame.transform], [true, true, true, '0px', 'hidden', 'none'], `${when}: thumbnail ${index} leaves its frame: ${JSON.stringify(frame)}`);
          assert.ok(frame.widthPx <= 220 && frame.heightPx <= 180, `${when}: thumbnail ${index} is within the size limit: ${JSON.stringify(frame)}`);
        }
      };
      await expectFrames('at rest');
      for (const index of [0, 1, 2]) {
        const handle = (await first.$$('[data-thumb-probe] .image-thumb'))[index];
        await handle.hover();
        await new Promise(resolve => setTimeout(resolve, 250));
        await expectFrames(`while hovering thumbnail ${index}`);
      }
      await first.mouse.move(2, 2);
      await first.evaluate(() => document.querySelector('[data-thumb-probe]')?.remove());
    }
    await first.evaluate(() => {
      window.__soundPlays = 0;
      HTMLMediaElement.prototype.play = function() { window.__soundPlays++; return Promise.resolve(); };
    });
    const seed = spawnSync(seedBinary, [path.join(tmp, 'server', 'aimebu.sqlite'), actualProjectID, roomID, id]);
    if (seed.status !== 0) throw new Error(`inbox seed failed: ${seed.stderr}`);
    await first.waitForFunction(() => document.querySelector('#inbox-count')?.textContent === '2');
    // Start as v2 agent: a historical sender's profile offers it, never automatically, with what will happen written out; the spawn dialog
    // opens prefilled with the old name and role, without the team tab, and closing it puts the ordinary dialog back.
    {
      const legacySeed = spawnSync(seedBinary, [path.join(tmp, 'server', 'aimebu.sqlite'), actualProjectID, roomID, id, 'legacy']);
      if (legacySeed.status !== 0) throw new Error(`legacy seed failed: ${legacySeed.stderr}`);
      await first.waitForSelector('.message-author[data-sender-kind="historical"]');
      await first.evaluate(() => { const author = [...document.querySelectorAll('.message-author[data-sender-kind="historical"]')].find(node => node.textContent.includes('veteran')); author.click(); });
      await first.waitForSelector('#sender-profile-dialog #sender-profile-start');
      assert.equal(await first.$eval('#sender-profile-start', button => button.title), 'Start as v2 agent', 'the start button carries its tooltip');
      assert.match(await first.$eval('.sender-profile-start .sender-profile-note', note => note.textContent), /Continues the v1 session in \/definitely\/not\/in\/the\/project\. That folder is outside the project folder/);
      await domClick(first, '#sender-profile-start');
      await first.waitForSelector('#spawn-dialog[open] #spawn-resume:not([hidden])');
      assert.deepEqual(await first.evaluate(() => ({
        title: document.querySelector('#spawn-form h2').textContent, worktree: document.querySelector('.spawn-worktree-option').hidden,
        text: document.querySelector('#spawn-resume-text').textContent, confirm: document.querySelector('#spawn-resume-confirm-label').hidden,
      })), { title: 'Start veteran as a v2 agent', worktree: true, text: 'This continues the v1 session of veteran@old, working in /definitely/not/in/the/project.', confirm: true });
      await first.waitForFunction(() => document.querySelector('#spawn-form [name=role]').value === 'reviewer', {}, {});
      // Which entry the dialog picks: a continued claude session takes the native claude entry even when a Docker one sorts first (and leaves
      // other tools and containers unselectable), and a fresh start only preselects the v1 tool without locking the others.
      assert.deepEqual(await first.evaluate(async () => {
        const { AgentsPanel } = await import('/js/agents.js');
        const pick = (resume, catalog) => {
          const select = document.createElement('select');
          catalog.forEach((tool, index) => { const option = document.createElement('option'); option.value = String(index); option.disabled = !tool.available; select.append(option); });
          select.selectedIndex = catalog.findIndex(tool => tool.available);
          AgentsPanel.prototype.applyResumeChoice.call({ pendingResume: resume, catalog }, select);
          return { selected: select.selectedIndex, disabled: [...select.options].map(option => option.disabled) };
        };
        const catalog = [{ tool: 'claude', containerized: true, available: true }, { tool: 'codex', containerized: false, available: true }, { tool: 'claude', containerized: false, available: true }];
        return [pick({ tool: 'claude', resumable: true }, catalog), pick({ tool: 'codex', resumable: false }, catalog), pick({ tool: 'claude', resumable: true }, catalog.slice(0, 2))];
      }), [{ selected: 2, disabled: [true, true, false] }, { selected: 1, disabled: [false, false, false] }, { selected: -1, disabled: [true, true] }], 'the start dialog picks the native claude entry, just the v1 tool for a fresh start, and nothing when no native entry fits');
      await domClick(first, '#spawn-cancel');
      await first.waitForFunction(() => document.querySelector('#spawn-resume').hidden);
      assert.deepEqual(await first.evaluate(() => ({ title: document.querySelector('#spawn-form h2').textContent, banner: document.querySelector('#spawn-resume').hidden })), { title: 'Spawn', banner: true }, 'closing the start dialog restores the ordinary one');
    }
    assert.equal(await first.title(), '(2) aimebu');
    assert.ok(await first.evaluate(() => window.__soundPlays > 0), 'new inbox items did not play the chime');
    assert.equal(await first.evaluate(async () => {
      const { InboxNotifications } = await import('/js/inbox-notifications.js');
      const original = window.Notification;
      let note, opened;
      class FakeNotification { static permission = 'granted'; constructor(title, options) { note = this; this.title = title; this.options = options; } close() {} }
      Object.defineProperty(window, 'Notification', { configurable: true, value: FakeNotification });
      Object.defineProperty(document, 'hidden', { configurable: true, value: true });
      try {
        const alerts = new InboxNotifications(item => { opened = item.id; });
        alerts.muted = true;
        Object.defineProperty(alerts, 'available', { value: true });
        alerts.arrived([{ id: 'new_item', room_id: 'room', project_id: 'project', question: 'Review this item' }], new Map([['project', { name: 'Project' }]]), new Map([['room', { title: 'Room' }]]));
        note.onclick();
        return note.title === 'aimebu · Room' && note.options.body.includes('Review this item') && note.options.icon === '/icons/icon-192.png' && opened === 'new_item';
      } finally {
        Object.defineProperty(window, 'Notification', { configurable: true, value: original });
        delete document.hidden;
      }
    }), true, 'notification click did not open the new item');
    // Muting is stored on the user now, so what this check turned on must be turned off before the pages that follow.
    await first.evaluate(async () => { const { csrf_token } = await (await fetch('/api/me')).json(); await fetch('/api/me/preferences', { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ preferences: { inbox_muted: '0' } }) }); for (const key of Object.keys(localStorage)) if (key.startsWith('aimebu2:inbox-muted:')) localStorage.removeItem(key); });
    await second.waitForFunction(() => document.querySelector('#inbox-count')?.textContent === '2');
    await domClick(first, '#inbox-toggle'); await second.click('#inbox-toggle');
    await domClick(first, '.inbox-card .inbox-choices button');
    await second.waitForFunction(() => document.querySelector('#inbox-count')?.textContent === '1' && !document.querySelector('[data-inbox-id="inb_smoke_a"]'));
    await second.locator('.inbox-card .inbox-answer textarea').fill('A written answer');
    await domClick(second, '.inbox-card .inbox-answer button');
    await first.waitForFunction(() => document.querySelector('.inbox-empty')?.textContent === 'All caught up');
    assert.equal(await first.title(), 'aimebu');
    const recoverySeed = spawnSync(seedBinary, [path.join(tmp, 'server', 'aimebu.sqlite'), actualProjectID, roomID, id, 'recovery']);
    if (recoverySeed.status !== 0) throw new Error(`recovery seed failed: ${recoverySeed.stderr}`);
    await first.waitForFunction(() => document.querySelector('#inbox-count')?.textContent === '2');
    const recoveryCard = '[data-inbox-id="inb_smoke_recovery"]';
    const deliveryCard = '[data-inbox-id="inb_smoke_delivery"]';
    assert.deepEqual(await first.$$eval(`${recoveryCard} .inbox-decisions button`, els => els.map(el => [el.textContent, el.disabled])), [['Continue', false], ['Abandon', false]]);
    assert.equal(await first.$eval(`${deliveryCard} .inbox-evidence`, el => el.textContent.includes('reply was held')), true);
    assert.equal(await first.$$eval(`${deliveryCard} .inbox-decisions button`, els => els.length), 0);
    const heldPage = await a.newPage();
    await heldPage.goto(`${origin}/?mock_delivery=1`);
    await appReady(heldPage);
    await heldPage.waitForFunction(() => document.querySelector('[data-inbox-id="mock_delivery"] .inbox-evidence')?.textContent === 'The target room was closed.');
    assert.equal(await heldPage.$eval('[data-inbox-id="mock_delivery"] .inbox-preview', el => el.textContent), 'The requested review is ready.');
    assert.equal(await heldPage.$eval('[data-inbox-id="mock_delivery"]', el => el.textContent.includes('{"messages"')), false);
    await domClick(heldPage, '#inbox-toggle');
    // Dismissing a held reply is an icon button with a tooltip and an accessible name, and no text of its own.
    assert.deepEqual(await heldPage.$eval('[data-inbox-id="mock_delivery"] button:last-child', el => ({ icon: el.classList.contains('icon-btn'), title: el.title, name: el.getAttribute('aria-label'), glyph: !!el.querySelector('.icon-mask-close'), text: el.textContent.trim() })), { icon: true, title: 'Dismiss', name: 'Dismiss', glyph: true, text: '' }, 'Dismiss is an icon button');
    await domClick(heldPage, '[data-inbox-id="mock_delivery"] button:last-child');
    await heldPage.waitForFunction(() => document.querySelector('.inbox-empty')?.textContent === 'All caught up');
    const quotaPage = await a.newPage();
    await quotaPage.goto(`${origin}/?mock_provider_limit=1`);
    await appReady(quotaPage);
    await quotaPage.waitForFunction(() => document.querySelector('[data-inbox-id="mock_provider_limit"] .btn-primary')?.textContent === 'Retry now');
    await domClick(quotaPage, '#inbox-toggle');
    await domClick(quotaPage, '[data-inbox-id="mock_provider_limit"] button:last-child');
    await quotaPage.waitForFunction(() => document.querySelector('.inbox-empty')?.textContent === 'All caught up');
    await quotaPage.close();
    await heldPage.close();
    await domClick(first, `${recoveryCard} .inbox-jump`);
    await first.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Nested task' && !document.querySelector('.chat').hidden);
    // Delivery failures: plain text only (the raw record is behind the debug switch), archived rooms' failures dismissed in bulk,
    // and no empty gutter when the room list is hidden.
    {
      const resolvedContext = await browser.createBrowserContext(), resolvedPage = await resolvedContext.newPage();
      await resolvedPage.setViewport({ width: 1100, height: 720 });
      await login(resolvedPage, `${origin}/?mock_resolved=1`);
      await resolvedPage.waitForFunction(() => document.querySelector('#inbox-count')?.textContent === '3');
      await domClick(resolvedPage, '#inbox-toggle');
      await resolvedPage.waitForSelector('[data-inbox-id="mock_resolved_a"]');
      const cards = await resolvedPage.$$eval('.inbox-card', els => els.map(el => ({ id: el.dataset.inboxId, text: el.innerText, archived: !!el.querySelector('.inbox-archived'), rawHidden: el.querySelector('.inbox-raw')?.hidden, rawVisible: el.querySelector('.inbox-raw-toggle') && getComputedStyle(el.querySelector('.inbox-raw-toggle')).display !== 'none' })));
      for (const card of cards) {
        const visible = card.text.replace(/\s+/g, ' ');
        assert.ok(!/[{}\[\]]|"\w+":/.test(visible.replace(/\(\d+\)/g, '')), `raw JSON shows in the Inbox card ${card.id}: ${visible}`);
        assert.equal(card.rawHidden, true, 'the raw record starts hidden');
        assert.equal(card.rawVisible, false, 'the raw toggle is only there with the debug inspector on');
      }
      assert.ok(cards.find(card => card.id === 'mock_resolved_a').text.includes('The summary you asked for is ready.'), 'the draft text is shown, not its JSON');
      assert.deepEqual(cards.map(card => [card.id, card.archived]), [['mock_resolved_a', true], ['mock_resolved_b', true], ['mock_open_failure', false]]);
      await resolvedPage.evaluate(() => { document.documentElement.dataset.debugInspector = 'on'; });
      await resolvedPage.click('[data-inbox-id="mock_resolved_a"] .inbox-raw-toggle');
      assert.equal(await resolvedPage.$eval('[data-inbox-id="mock_resolved_a"] .inbox-raw', el => !el.hidden && el.textContent.includes('validation_failed') && el.textContent.includes('mock_resolved_a')), true, 'the raw record opens behind the bug icon');
      // Resume / Retry are .btn, Abandon is .btn-danger (checked on the recovery fixture elsewhere); the bulk dismiss is a .btn.
      assert.equal(await resolvedPage.$eval('.inbox-dismiss-resolved', el => [el.hidden, el.textContent, el.classList.contains('btn')]).then(JSON.stringify), JSON.stringify([false, 'Dismiss all resolved (2)', true]));
      // A hidden room list leaves no empty column.
      const gutter = await resolvedPage.evaluate(() => { document.querySelector('.sidebar').hidden = true; const r = document.querySelector('#inbox-view').getBoundingClientRect(); const out = { left: Math.round(r.left), width: Math.round(r.width), viewport: innerWidth }; document.querySelector('.sidebar').hidden = false; return out; });
      assert.deepEqual([gutter.left, gutter.width], [0, gutter.viewport], `an empty gutter is left when the room list is hidden: ${JSON.stringify(gutter)}`);
      // A room that is archived while the card is on screen updates the card and the bulk count (open -> closing -> closed).
      await resolvedPage.evaluate(() => { const item = window.__mockInbox.items.find(entry => entry.id === 'mock_open_failure'); item.room_lifecycle = 'closing'; window.__mockInbox.notify(); });
      await resolvedPage.waitForFunction(() => document.querySelector('[data-inbox-id="mock_open_failure"] .inbox-archived')?.textContent === 'Room is being archived' && document.querySelector('.inbox-dismiss-resolved').textContent === 'Dismiss all resolved (3)');
      await resolvedPage.evaluate(() => { const item = window.__mockInbox.items.find(entry => entry.id === 'mock_open_failure'); item.room_lifecycle = 'closed'; window.__mockInbox.notify(); });
      await resolvedPage.waitForFunction(() => document.querySelector('[data-inbox-id="mock_open_failure"] .inbox-archived')?.textContent === 'In archived room');
      await resolvedPage.evaluate(() => { const item = window.__mockInbox.items.find(entry => entry.id === 'mock_open_failure'); item.room_lifecycle = 'open'; window.__mockInbox.notify(); });
      await resolvedPage.waitForFunction(() => !document.querySelector('[data-inbox-id="mock_open_failure"] .inbox-archived') && document.querySelector('.inbox-dismiss-resolved').textContent === 'Dismiss all resolved (2)');
      // The held reason reads as a sentence (a known reason needs no technical error after it).
      assert.ok((await resolvedPage.$eval('[data-inbox-id="mock_resolved_a"]', el => el.innerText)).includes('The revised reply still did not pass validation.'), 'a known held reason is a sentence');
      await domClick(resolvedPage, '.inbox-dismiss-resolved');
      await resolvedPage.waitForFunction(() => document.querySelectorAll('.inbox-card').length === 1 && document.querySelector('.inbox-dismiss-resolved').hidden);
      assert.equal(await resolvedPage.$eval('.inbox-card', el => el.dataset.inboxId), 'mock_open_failure', 'only the archived rooms\' failures are dismissed');
      await resolvedContext.close();
    }
    const mockContext = await browser.createBrowserContext(), mock = await mockContext.newPage();
    await mock.setViewport({ width: 390, height: 780 });
    await login(mock, `${origin}/?mock_inbox=1`);
    await mock.waitForFunction(() => document.querySelector('#inbox-count')?.textContent === '2');
    // A phone login lands on a room, not the Inbox (the badge still says there is something waiting).
    assert.deepEqual(await mock.evaluate(() => ({ inbox: !document.querySelector('#inbox-view').hidden, chat: !document.querySelector('.chat').hidden })), { inbox: false, chat: true }, 'a phone login must land on a room');
    assert.notEqual(await mock.$eval('#room-title', el => el.textContent), 'Choose a room', 'the phone landed on no room');
    assert.equal(await mock.evaluate(() => localStorage.getItem('aimebu2.lastRoom') === decodeURIComponent(location.hash.slice(1)) && !!localStorage.getItem('aimebu2.lastRoom')), true, 'the room being shown is remembered for the next login');
    await mock.click('#inbox-toggle');
    assert.equal(await mock.$eval('#inbox-view', el => !el.hidden), true, 'the Inbox button did not open the inbox');
    assert.equal(await mock.$eval('.inbox-group h2', el => el.textContent), 'UI smoke / Nested task');
    await domClick(mock, '.inbox-jump');
    await mock.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Nested task' && !document.querySelector('.chat').hidden);
    await domClick(mock, '#inbox-toggle');
    await mock.locator('.inbox-card:last-child .inbox-answer textarea').fill('A custom answer');
    await domClick(mock, '.inbox-card .inbox-choices button');
    await mock.waitForFunction(() => document.querySelector('#inbox-count')?.textContent === '1');
    assert.equal(await mock.$eval('.inbox-card .inbox-answer textarea', el => el.value), 'A custom answer');
    await mock.click('.inbox-card .inbox-answer button');
    await mock.waitForFunction(() => document.querySelector('.inbox-empty')?.textContent === 'All caught up');
    assert.equal(await mock.$eval('#inbox-count', el => el.hidden), true);
    {
      // usages_updated is a transient frame: ignored before `ready`, then handed to onUsagesUpdated.
      const probeContext = await browser.createBrowserContext(), probe = await probeContext.newPage();
      await probe.goto(origin);
      const pushFrames = await probe.evaluate(async () => {
        const { EventStream } = await import('/js/events.js');
        const original = window.WebSocket; const sockets = [];
        class FakeSocket { constructor() { sockets.push(this); } close() { this.onclose?.(); } emit(frame) { this.onmessage?.({ data: JSON.stringify(frame) }); } }
        window.WebSocket = FakeSocket;
        let pushes = 0;
        const stream = new EventStream(() => {}, async () => 0, () => {}, 0);
        stream.onUsagesUpdated = () => { pushes++; };
        stream.start();
        const socket = sockets[0];
        socket.emit({ type: 'usages_updated' }); await stream.pending;
        const beforeReady = pushes;
        socket.emit({ type: 'ready', seq: 0 }); await stream.pending;
        socket.emit({ type: 'usages_updated' }); await stream.pending;
        stream.stop(); window.WebSocket = original;
        return { beforeReady, afterReady: pushes, seq: stream.seq };
      });
      assert.deepEqual(pushFrames, { beforeReady: 0, afterReady: 1, seq: 0 }, 'usages_updated must reach the handler only once the stream is live, without moving seq');
      await probeContext.close();
      // First-run owner setup: a server with no owner and no owner environment prints a one-time link; /setup shows the welcome, then
      // the Create owner form, signs the new owner in without a second welcome, and is gone afterwards.
      {
        const setupTmp = fs.mkdtempSync(path.join(os.tmpdir(), 'aimebu-ui-setup-'));
        const setupPort = await freePort(), setupOrigin = `http://127.0.0.1:${setupPort}`;
        const setupEnv = { ...isolatedEnv, AIMEBU2_CONFIG_DIR: setupTmp, AIMEBU2_EMBEDDED_SUPERVISOR: '0', AIMEBU2_USAGES: '0', AIMEBU2_PORT: String(setupPort) };
        delete setupEnv.AIMEBU2_OWNER_LOGIN; delete setupEnv.AIMEBU2_OWNER_PASSWORD;
        const setupServer = spawn(binary, ['server'], { env: setupEnv, stdio: ['ignore', 'pipe', 'pipe'] });
        let setupLogs = ''; setupServer.stderr.on('data', data => setupLogs += data);
        const setupContext = await browser.createBrowserContext();
        try {
          for (let i = 0; i < 300; i++) { try { if ((await fetch(setupOrigin)).ok) break; } catch {} await new Promise(r => setTimeout(r, 100)); if (i === 299) throw new Error(`setup server did not start: ${setupLogs}`); }
          const tokenFile = path.join(setupTmp, '.setup_token');
          const setupToken = fs.readFileSync(tokenFile, 'utf8').trim();
          assert.equal(fs.statSync(tokenFile).mode & 0o777, 0o600, 'the token file is private');
          assert.ok(setupLogs.includes(`${setupOrigin}/setup?token=${setupToken}`), `the startup message does not carry the link: ${setupLogs}`);
          assert.ok(setupLogs.includes(`\n  ${setupToken}\n`) && setupLogs.includes(`The token is also in ${tokenFile}`), `the startup message must print the token and name its file: ${setupLogs}`);
          // Opening the plain address of a server with no owner asks for the setup token (there is no login that could work), tells where to
          // find it, refuses a wrong one with a clear error, and accepts the one in the file.
          {
            const plain = await setupContext.newPage();
            await plain.goto(setupOrigin);
            await plain.waitForSelector('#welcome-view');
            await plain.$eval('#welcome-view', view => view.click()); await plain.waitForSelector('#welcome-continue:not([hidden])'); await plain.click('#welcome-continue');
            await plain.waitForSelector('#setup-token-view:not([hidden])');
            assert.equal(await plain.$eval('#login-view', view => view.hidden), true, 'a server with no owner shows no login');
            assert.equal(await plain.$eval('#setup-token-view h1', el => el.textContent), 'Enter the setup token to continue');
            assert.match(await plain.$eval('#setup-token-hint', el => el.textContent), /output of the server/);
            assert.equal(await plain.$eval('#setup-token-hint code', el => el.textContent), tokenFile, 'the screen names the full path of the token file');
            assert.equal(await plain.$eval('#setup-view', view => view.hidden), true, 'the owner form waits for the token');
            await plain.type('#setup-token-form [name=token]', 'setup_wrong'); await plain.click('#setup-token-form button[type=submit]');
            await plain.waitForFunction(() => document.querySelector('#setup-token-error').textContent.includes('not the setup token'));
            assert.equal(await plain.$eval('#setup-view', view => view.hidden), true, 'a wrong token does not open the form');
            await plain.$eval('#setup-token-form [name=token]', input => { input.value = ''; });
            await plain.type('#setup-token-form [name=token]', `  ${setupToken}  `); await plain.click('#setup-token-form button[type=submit]');
            await plain.waitForSelector('#setup-view:not([hidden])');
            assert.equal(await plain.$eval('#setup-token-view', view => view.hidden), true, 'the token screen is gone once the token is accepted');
            assert.equal(fs.existsSync(tokenFile), true, 'checking the token does not spend it');
            await plain.close();
          }
          const setupPage = await setupContext.newPage();
          await setupPage.goto(`${setupOrigin}/setup?token=${setupToken}`);
          await setupPage.waitForSelector('#welcome-view');
          assert.equal(await setupPage.$eval('#setup-view', view => view.hidden), true, 'the form must wait behind the welcome');
          await setupPage.$eval('#welcome-view', view => view.click()); await setupPage.waitForSelector('#welcome-continue:not([hidden])'); await setupPage.click('#welcome-continue');
          // The link is the shortcut: it fills the token in and the token screen never shows.
          await setupPage.waitForSelector('#setup-view:not([hidden])');
          assert.equal(await setupPage.$eval('#setup-token-view', view => view.hidden), true, 'a link with the token skips the token screen');
          await setupPage.type('#setup-form [name=login]', 'Boss'); await setupPage.type('#setup-form [name=password]', 'a-long-enough-password'); await setupPage.type('#setup-form [name=confirm]', 'a-different-password');
          await setupPage.click('#setup-form button[type=submit]');
          await setupPage.waitForFunction(() => document.querySelector('#setup-error').textContent.includes('do not match'));
          await setupPage.$eval('#setup-form [name=confirm]', input => { input.value = ''; });
          await setupPage.type('#setup-form [name=confirm]', 'a-long-enough-password');
          await setupPage.click('#setup-form button[type=submit]');
          await setupPage.waitForSelector('#theme-view:not([hidden])');
          assert.equal(await setupPage.$$eval('#theme-view .theme-tile', tiles => tiles.length), 10, 'one tile per theme');
          await setupPage.click('#theme-view .theme-tile[data-choice="red-light"]');
          assert.equal(await setupPage.evaluate(() => document.documentElement.dataset.theme), 'red-light', 'a picked theme applies at once');
          await setupPage.waitForFunction(async () => (await (await fetch('/api/me/preferences')).json()).preferences.theme === 'red-light');
          await setupPage.click('#theme-view .theme-continue');
          await setupPage.waitForSelector('#app-view:not([hidden])', { timeout: APP_READY_TIMEOUT });
          assert.equal(await setupPage.$('#welcome-view'), null, 'the welcome was shown a second time after setup');
          // The last step of the first run: no project yet, so onboarding offers to make one. Skipping it leaves the empty state.
          await setupPage.waitForSelector('#onboarding-view:not([hidden])');
          await setupPage.click('#onboarding-view .onboarding-skip');
          await setupPage.waitForSelector('#app-view.no-room #chat-empty');
          assert.deepEqual(await setupPage.evaluate(() => { const shown = selector => { const node = document.querySelector(selector); return !!node && getComputedStyle(node).display !== 'none'; }; return { card: document.querySelector('#chat-empty-action').textContent, composer: shown('#composer'), members: shown('#agents-panel'), controls: shown('.room-controls'), heading: shown('.room-heading'), empty: shown('#chat-empty') }; }),
            { card: 'Create your first project', composer: false, members: false, controls: false, heading: false, empty: true }, 'the empty state replaces the composer, members and room controls');
          await setupPage.click('#chat-empty-action');
          await setupPage.waitForSelector('#onboarding-view:not([hidden]) #onboarding-form');
          assert.deepEqual(await setupPage.evaluate(() => { const form = document.querySelector('#onboarding-form'), style = getComputedStyle(form); return { border: style.borderTopWidth, padding: style.paddingTop, tabs: getComputedStyle(document.querySelector('#mobile-tabs')).display }; }),
            { border: '0px', padding: '0px', tabs: 'none' }, 'the first project form is one card, not a card in a card, with no tab bar under it');
          await setupPage.type('#onboarding-form [name=name]', 'demo'); fs.mkdirSync(path.join(tmp, 'demo-folder'), { recursive: true });
          await setupPage.type('#onboarding-form [name=folder]', path.join(tmp, 'demo-folder'));
          await setupPage.click('#onboarding-form button[type=submit]');
          await setupPage.waitForSelector('#onboarding-view [data-step="how"]:not([hidden])');
          assert.equal(await setupPage.$$eval('.onboarding-principles li', items => items.length), 7, 'seven principles');
          await setupPage.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'general');
          assert.equal(await setupPage.$eval('#app-view', view => view.classList.contains('no-room')), false, 'the empty state is gone once the general room opens');
          assert.deepEqual(await setupPage.evaluate(async () => (await (await fetch('/api/projects')).json()).map(project => project.name)), ['demo']);
          await setupPage.click('#onboarding-spawn');
          await setupPage.waitForFunction(() => document.querySelector('#spawn-dialog')?.open);
          await setupPage.keyboard.press('Escape');
          assert.equal(new URL(setupPage.url()).pathname, '/', 'the setup address stays in the address bar after setup');
          assert.equal((await fetch(`${setupOrigin}/setup?token=${setupToken}`)).status, 404, '/setup must be gone once an owner exists');
          assert.equal(fs.existsSync(tokenFile), false, 'the token file must be removed after use');
          assert.deepEqual(await (await fetch(`${setupOrigin}/api/setup/status`)).json(), { open: false }, 'the token path is not offered once an owner exists');
          await setupPage.reload(); await setupPage.waitForSelector('#app-view:not([hidden])', { timeout: APP_READY_TIMEOUT });
          assert.equal(await setupPage.$('#welcome-view'), null, 'the welcome came back after the setup marked it seen');
          assert.equal(await setupPage.evaluate(() => document.documentElement.dataset.theme), 'red-light', 'the chosen theme did not survive a reload');
          // The theme belongs to the user, not the browser: with this browser's cache gone, a new sign-in still gets it.
          await setupPage.evaluate(() => localStorage.clear()); await setupPage.reload(); await setupPage.waitForSelector('#app-view:not([hidden])', { timeout: APP_READY_TIMEOUT });
          assert.equal(await setupPage.evaluate(() => document.documentElement.dataset.theme), 'red-light', 'the theme was not restored from the server');
        } finally {
          await setupContext.close();
          setupServer.kill();
          fs.rmSync(setupTmp, { recursive: true, force: true });
        }
      }
      // End to end against a second server with usage polling on (the main smoke server disables it):
      // a change made from one browser reaches an open Settings -> Usages panel in another with no
      // reload, and a second forced refresh inside the cooldown tells the owner to wait.
      const liveTmp = fs.mkdtempSync(path.join(os.tmpdir(), 'aimebu-ui-usages-'));
      const livePort = await freePort(), liveOrigin = `http://127.0.0.1:${livePort}`;
      const liveEnv = { ...isolatedEnv, AIMEBU2_CONFIG_DIR: liveTmp, AIMEBU2_EMBEDDED_SUPERVISOR: '0', AIMEBU2_PORT: String(livePort), AIMEBU2_OWNER_LOGIN: 'owner', AIMEBU2_OWNER_PASSWORD: 'test-password-123' };
      delete liveEnv.AIMEBU2_USAGES;
      const liveServer = spawn(binary, ['server'], { env: liveEnv, stdio: ['ignore', 'pipe', 'pipe'] });
      let liveLogs = ''; liveServer.stderr.on('data', data => liveLogs += data);
      const liveContexts = [];
      try {
        for (let i = 0; i < 300; i++) { try { if ((await fetch(liveOrigin)).ok) break; } catch {} await new Promise(r => setTimeout(r, 100)); if (i === 299) throw new Error(`usages server did not start: ${liveLogs}`); }
        const [watcher, actor] = await Promise.all([browser.createBrowserContext(), browser.createBrowserContext()]);
        liveContexts.push(watcher, actor);
        const watchPage = await watcher.newPage(), actPage = await actor.newPage();
        await login(watchPage, liveOrigin); await login(actPage, liveOrigin);
        // The Users tab appears once its access check answers; the tabs below it must not move while
        // Settings is first opened, or a click aimed at one lands on its neighbour.
        const tabMoves = await watchPage.evaluate(() => new Promise(resolve => {
          const ys = new Set(); const started = performance.now();
          document.querySelector('#settings-toggle').click();
          const sample = () => {
            ys.add(Math.round(document.querySelector('#settings-macros-tab').getBoundingClientRect().y));
            if (performance.now() - started < 1200) requestAnimationFrame(sample);
            else resolve({ ys: [...ys], pending: document.querySelector('#settings-users-tab').classList.contains('settings-tab-pending'), usersHidden: document.querySelector('#settings-users-tab').hidden });
          };
          sample();
        }));
        assert.equal(tabMoves.ys.length, 1, `settings tabs moved while opening: ${JSON.stringify(tabMoves)}`);
        assert.deepEqual([tabMoves.pending, tabMoves.usersHidden], [false, false], 'the Users tab must settle visible for the owner');
        await domClick(watchPage, '#settings-usages-tab');
        const codexStatus = '.usage-provider[data-provider="codex"] .usage-provider-status';
        await watchPage.waitForFunction(selector => document.querySelector(selector)?.textContent === 'Enabled', {}, codexStatus);
        await watchPage.evaluate(() => { window.__usagesNoReload = true; });
        const toggleProvider = (provider, enabled = false) => actPage.evaluate(async (name, on) => { const { csrf_token } = await (await fetch('/api/me')).json(); const response = await fetch('/api/usages/providers', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ provider: name, enabled: on }) }); if (!response.ok) throw new Error(`provider toggle: ${response.status}`); }, provider, enabled);
        await toggleProvider('codex');
        await watchPage.waitForFunction(selector => document.querySelector(selector)?.textContent === 'Not configured', { timeout: 10000 }, codexStatus);
        assert.equal(await watchPage.evaluate(() => window.__usagesNoReload === true), true, 'the usages panel was reloaded instead of updated in place');
        // A push that arrives while a field in the panel has focus is held, then applied once focus leaves.
        const claudeStatus = '.usage-provider[data-provider="claude-code"] .usage-provider-status';
        assert.equal(await watchPage.$eval(claudeStatus, el => el.textContent), 'Enabled');
        await watchPage.focus('#settings-usages input[type="number"]');
        await toggleProvider('claude-code');
        await new Promise(resolve => setTimeout(resolve, 800));
        assert.equal(await watchPage.$eval(claudeStatus, el => el.textContent), 'Enabled', 'a focused field must not be re-rendered by a push');
        await watchPage.$eval('#settings-usages input[type="number"]', el => el.blur());
        await watchPage.waitForFunction(selector => document.querySelector(selector)?.textContent === 'Not configured', { timeout: 10000 }, claudeStatus);
        // The top-bar popover follows the same push, and its Refresh is the rate-limited forced refresh.
        await domClick(watchPage, '#settings-close');
        await domClick(watchPage, '#usages-toggle'); await watchPage.waitForSelector('#usages-popover:not([hidden]) .usages-popover-note');
        assert.equal(await watchPage.$('#usages-popover [data-provider="codex"]'), null);
        await toggleProvider('codex', true);
        await watchPage.waitForSelector('#usages-popover [data-provider="codex"]', { timeout: 10000 });
        const popoverRefresh = async () => {
          const response = watchPage.waitForResponse(r => r.url().endsWith('/api/usages/refresh') && r.request().method() === 'POST');
          await domClick(watchPage, '#usages-popover .usages-popover-refresh');
          return (await response).status();
        };
        assert.equal(await popoverRefresh(), 200, 'the popover Refresh must call the forced refresh endpoint');
        await watchPage.waitForFunction(() => !document.querySelector('#usages-popover .usages-popover-refresh').disabled);
        assert.equal(await popoverRefresh(), 429, 'a second popover refresh inside the cooldown should be refused');
        await watchPage.waitForFunction(() => /try again in \d+s/.test(document.querySelector('#usages-popover .usages-popover-status')?.textContent || ''), { timeout: 10000 });
        assert.equal(await watchPage.$('#usages-popover [data-provider="codex"]') !== null, true, 'a refused refresh must keep the overview');
        // The cooldown is shared: Settings -> Refresh now is refused too.
        await domClick(watchPage, '#settings-toggle');
        await watchPage.waitForSelector('#settings-dialog[open]');
        await domClick(watchPage, '#settings-usages-tab');
        const settingsRefresh = watchPage.waitForResponse(r => r.url().endsWith('/api/usages/refresh') && r.request().method() === 'POST');
        await watchPage.evaluate(() => [...document.querySelectorAll('#settings-usages button')].find(el => el.textContent === 'Refresh now').click());
        assert.equal((await settingsRefresh).status(), 429, 'the cooldown is shared by Settings and the popover');
        await watchPage.waitForFunction(() => /try again in \d+s/.test(document.querySelector('#usages-error')?.textContent || ''), { timeout: 10000 });
      } finally {
        for (const context of liveContexts) await context.close().catch(() => {});
        if (liveServer.exitCode === null && liveServer.signalCode === null) { liveServer.kill('SIGTERM'); await new Promise(resolve => liveServer.once('exit', resolve)); }
        fs.rmSync(liveTmp, { recursive: true, force: true });
      }
    }
    const settingsContext = await browser.createBrowserContext(), settingsPage = await settingsContext.newPage();
    await settingsPage.setViewport({ width: 1100, height: 720 });
    // The quota switcher API is answered by a Node-side mock (scripts/quota-mock.cjs) through request interception, so the
    // production Settings code runs over plain HTTP with no test hooks. Everything else goes to the real server.
    const quotaMock = new QuotaMock(), guardMock = new LimitGuardMock();
    await settingsPage.setRequestInterception(true);
    settingsPage.on('request', async request => {
      if (request.isInterceptResolutionHandled()) return;
      const url = new URL(request.url());
      if (url.pathname.startsWith('/api/limit-guard')) {
        const raw = request.postData(), answer = guardMock.handle(request.method(), url.pathname, raw ? JSON.parse(raw) : undefined);
        request.respond({ status: answer.status, contentType: 'application/json', body: JSON.stringify(answer.body) });
        return;
      }
      if (!url.pathname.startsWith('/api/quota-switcher')) { request.continue(); return; }
      if (request.method() === 'PUT' && quotaMock.gate) await quotaMock.gate; // a save that waits for a running switch
      const raw = request.postData(), answer = quotaMock.handle(request.method(), url.pathname, raw ? JSON.parse(raw) : undefined);
      request.respond({ status: answer.status, contentType: 'application/json', body: JSON.stringify(answer.body) });
    });
    await login(settingsPage, `${origin}/?mock_usages=1&mock_project_settings=1`);
    await settingsPage.waitForSelector('.room-filter');
    await domClick(settingsPage, '#usages-toggle');
    await settingsPage.waitForSelector('#usages-popover:not([hidden]) .usages-popover-provider[data-provider="codex"]');
    assert.equal(await settingsPage.$eval('#usages-toggle', el => el.getAttribute('aria-expanded')), 'true');
    assert.equal(await settingsPage.$eval('#settings-dialog', el => el.open), false, 'the usage indicator opens a popover, not Settings');
    assert.deepEqual(await settingsPage.$$eval('#usages-popover .usages-popover-provider-name', els => els.map(el => el.textContent)), ['Codex', 'Claude Code'], 'only configured providers are listed');
    assert.deepEqual(await settingsPage.$$eval('#usages-popover [data-provider="codex"] .usages-popover-profile-name', els => els.map(el => el.textContent)), ['main', 'backup'], 'the active profile comes first');
    assert.equal(await settingsPage.$eval('#usages-popover [data-provider="codex"] .usages-popover-profile', el => el.classList.contains('is-active') && !!el.querySelector('.usage-profile-active')), true);
    assert.equal(await settingsPage.$$eval('#usages-popover [data-provider="codex"] .usages-popover-profile.is-active .usage-window .usage-track', els => els.length), 3);
    assert.equal(await settingsPage.$eval('#usages-popover [data-provider="codex"] .usage-profile-credits', el => el.textContent), 'Credits: 12.50');
    {
      const spark = await settingsPage.$eval('#usages-popover [data-provider="codex"] .usages-popover-profile.is-active .usage-window:first-child .usage-spark', el => {
        const box = el.getBoundingClientRect();
        return { tag: el.tagName.toLowerCase(), role: el.getAttribute('role'), label: el.getAttribute('aria-label'), points: el.querySelector('polyline').getAttribute('points').trim().split(/\s+/).length, width: box.width, height: box.height, inside: !!el.closest('.usage-window') };
      });
      assert.equal(spark.tag, 'svg'); assert.equal(spark.role, 'img'); assert.equal(spark.points, 5, 'the sparkline plots every sample');
      assert.match(spark.label, /Session over the last 48 hours: \d+% to \d+% (used|left)/);
      assert.equal(spark.inside && spark.width > 40 && spark.height >= 16, true, `sparkline geometry ${JSON.stringify(spark)}`);
      assert.equal(await settingsPage.$$eval('#usages-popover [data-provider="codex"] .usages-popover-profile.is-active .usage-window:nth-child(2) .usage-spark', els => els.length), 0, 'a window without history draws no sparkline');
      // The sparkline names its period and what it plots; the pace line explains itself (hover title, focusable, accessible name).
      const labelled = await settingsPage.$eval('#usages-popover [data-provider="codex"] .usages-popover-profile.is-active .usage-window:first-child', row => {
        const caption = [...row.querySelectorAll('.usage-spark-caption span')].map(el => el.textContent), pace = row.querySelector('.usages-pace-text');
        return { caption, figure: !!row.querySelector('.usage-spark-figure .usage-spark'), title: pace?.title || '', focusable: pace?.tabIndex === 0, named: /^.+\. Pace compares/.test(pace?.getAttribute('aria-label') || '') };
      });
      assert.deepEqual(labelled.caption.length === 3 && labelled.caption[0] === '48 h ago' && labelled.caption[2] === 'now' && /^% (used|left), last 48 h$/.test(labelled.caption[1]), true, `sparkline caption ${JSON.stringify(labelled.caption)}`);
      assert.equal(labelled.figure && labelled.focusable && labelled.named && /In reserve|In deficit|On pace/.test(labelled.title) && /even spend/.test(labelled.title), true, `pace explanation ${JSON.stringify(labelled)}`);
    }
    // v1's usages column: toolbar on the left (close, refresh, group), the title on the right; provider cards with icon, age, active profile, plan, bars and pace.
    {
      const head = await settingsPage.$eval('#usages-popover > header', el => ({ labels: [...el.querySelectorAll('button')].map(button => button.getAttribute('aria-label')), titleAtRight: el.querySelector('strong').getBoundingClientRect().left > el.querySelector('button:last-of-type').getBoundingClientRect().right, title: el.querySelector('strong').textContent, size: [...el.querySelectorAll('button')].map(button => `${Math.round(button.getBoundingClientRect().width)}x${Math.round(button.getBoundingClientRect().height)}`) }));
      assert.deepEqual(head.labels, ['Close usages', 'Refresh usages', 'Show active profiles first']); assert.equal(head.titleAtRight, true); assert.equal(head.title, 'Usages'); assert.deepEqual(head.size, ['28x28', '28x28', '28x28'], 'the usages toolbar uses the shared framed icon button');
      const card = await settingsPage.$eval('#usages-popover [data-provider="codex"]', el => ({ mask: !!el.querySelector('.usages-provider-mask-codex'), updated: el.querySelector('.usages-provider-updated')?.textContent, active: el.querySelector('.usage-profile-active')?.textContent, plan: el.querySelector('.usages-popover-profile.is-active .usage-profile-plan')?.textContent, meta: el.querySelector('.usages-window-meta strong')?.textContent, reset: el.querySelector('.usages-window-meta span')?.textContent, pace: el.querySelector('.usages-pace-text')?.textContent, tick: !!el.querySelector('.usages-progress .usages-pace-tick') }));
      assert.match(card.updated, /^Updated (\d+[smhd]|just now) ago$|^just now$/); assert.deepEqual([card.mask, card.active, card.plan, card.tick], [true, 'Active', 'Pro', true]);
      assert.match(card.meta, /^\d+(\.\d)?% left$/); assert.match(card.reset, /^Resets in /); assert.match(card.pace, /in reserve|Lasts until reset/);
      // v1's token-expiry icon (healthy, expiring, expired) and SWITCH pill (inactive profiles only, after a confirmation).
      assert.deepEqual(await settingsPage.$$eval('#usages-popover [data-provider="codex"] .usages-popover-profile', nodes => nodes.map(node => [node.querySelector('.usages-popover-profile-name').textContent, node.querySelector('.switcher-expiry-icon')?.dataset.level, !!node.querySelector('.switcher-switch-pill')])), [['main', 'ok', false], ['backup', 'expiring', true]]);
      assert.equal(await settingsPage.$eval('#usages-popover [data-provider="claude-code"] .switcher-expiry-icon', el => el.dataset.level + ':' + el.classList.contains('switcher-expiry-icon--expired') + ':' + /^Token expires: .* \(expired \d+m ago\)$/.test(el.title)), 'expired:true:true');
      assert.equal(await settingsPage.$eval('#usages-popover [data-provider="codex"] .switcher-switch-pill', el => el.textContent), 'Switch');
      const confirmed = []; await answerDialogs(settingsPage, text => { confirmed.push(text); return { accept: true }; });
      await domClick(settingsPage, '#usages-popover [data-provider="codex"] .switcher-switch-pill');
      await settingsPage.waitForFunction(() => document.querySelector('#usages-popover [data-provider="codex"] .usages-popover-profile.is-active .usages-popover-profile-name')?.textContent === 'backup');
      assert.match(confirmed[0], /Switch to backup\?$/);
      assert.deepEqual(await settingsPage.$$eval('#usages-popover [data-provider="codex"] .usages-popover-profile', nodes => nodes.map(node => [node.querySelector('.usages-popover-profile-name').textContent, !!node.querySelector('.switcher-switch-pill')])), [['backup', false], ['main', true]], 'after the switch the other profile gets the pill');
      // The group button shows each provider's active profile first, then a divider and the other profiles.
      await domClick(settingsPage, '#usages-popover .usages-popover-group');
      assert.equal(await settingsPage.$eval('#usages-popover .usages-popover-group', el => el.getAttribute('aria-pressed')), 'true');
      assert.deepEqual(await settingsPage.$$eval('#usages-popover .usages-sidebar-list > *', nodes => nodes.map(node => node.classList.contains('usages-group-divider') ? '—' : `${node.dataset.provider}:${[...node.querySelectorAll('.usages-popover-profile-name')].map(name => name.textContent).join(',')}`)), ['codex:backup', 'claude-code:local', '—', 'codex:main'], 'active profiles first (backup is active after the switch), then a divider and the rest');
      await settingsPage.click('#usages-popover .usages-popover-group');
      assert.equal(await settingsPage.$eval('#usages-popover .usages-popover-group', el => el.getAttribute('aria-pressed')), 'false');
    }
    assert.equal(await settingsPage.$('#usages-popover [data-provider="mistral"]'), null, 'unconfigured providers stay out of the popover');
    await settingsPage.setViewport({ width: 390, height: 780 });
    const popoverFits = await settingsPage.$eval('#usages-popover', el => { const box = el.getBoundingClientRect(); return box.left >= 0 && box.right <= innerWidth && box.top >= 0 && box.bottom <= innerHeight; });
    assert.equal(popoverFits, true, 'the popover fits a phone viewport');
    await settingsPage.setViewport({ width: 1100, height: 720 });
    await settingsPage.keyboard.press('Escape');
    assert.equal(await settingsPage.$eval('#usages-popover', el => el.hidden), true, 'Escape closes the popover');
    await domClick(settingsPage, '#usages-toggle'); await settingsPage.waitForSelector('#usages-popover:not([hidden])');
    // Usages are a docked column (v1): they take the members column's place, and a click elsewhere does not dismiss them.
    assert.equal(await settingsPage.evaluate(() => document.activeElement?.id), 'usages-popover', 'opening the usages takes keyboard focus');
    // The other top-bar popovers do the same, and Escape hands focus back to the button that opened them.
    for (const [toggle, popover] of [['#health-toggle', '#health-popover'], ['#system-events-toggle', '#system-events-popover']]) {
      await settingsPage.evaluate(() => document.querySelector('#usages-popover .usages-popover-close').click());
      if (await settingsPage.$eval(toggle, el => el.hidden)) continue;
      await settingsPage.evaluate(selector => document.querySelector(selector).click(), toggle);
      await settingsPage.waitForFunction(selector => !document.querySelector(selector).hidden, {}, popover);
      assert.equal(await settingsPage.evaluate(selector => document.activeElement === document.querySelector(selector), popover), true, `${popover} takes keyboard focus when it opens`);
      await settingsPage.keyboard.press('Escape');
      assert.equal(await settingsPage.evaluate(selector => document.activeElement === document.querySelector(selector), toggle), true, `Escape returns focus to ${toggle}`);
    }
    await domClick(settingsPage, '#usages-toggle'); await settingsPage.waitForSelector('#usages-popover:not([hidden])');
    const docked = await settingsPage.evaluate(() => { const box = document.querySelector('#usages-popover').getBoundingClientRect(); return { right: Math.round(innerWidth - box.right), width: Math.round(box.width), chat: document.querySelector('.chat').classList.contains('usages-open'), members: getComputedStyle(document.querySelector('#agents-panel')).display }; });
    assert.deepEqual(docked, { right: 0, width: 260, chat: true, members: 'none' }, 'usages replace the right column');
    // The usages column runs from the header row to the bottom of the window at any height and scrolls its own content.
    for (const height of [720, 1000, 420]) {
      await settingsPage.setViewport({ width: 1100, height });
      const fit = await settingsPage.evaluate(() => { const dock = document.querySelector('#usages-popover'), box = dock.getBoundingClientRect(), chat = document.querySelector('.chat').getBoundingClientRect(); return { bottomGap: Math.round(innerHeight - box.bottom), chatGap: Math.round(chat.bottom - box.bottom), overflowY: getComputedStyle(dock).overflowY }; });
      assert.deepEqual(fit, { bottomGap: 0, chatGap: 0, overflowY: 'auto' }, `the usages column must reach the bottom of the window at ${height}px: ${JSON.stringify(fit)}`);
    }
    {
      const scrolls = await settingsPage.evaluate(() => { const dock = document.querySelector('#usages-popover'); const overflowing = dock.scrollHeight > dock.clientHeight; dock.scrollTop = 80; return { overflowing, moved: dock.scrollTop > 0 }; });
      assert.deepEqual(scrolls, { overflowing: true, moved: true }, 'a short window makes the usages column scroll its own content');
    }
    await settingsPage.setViewport({ width: 1100, height: 720 });
    // The usages column starts on the chat header's row with a header of the same 43px height, like the members column.
    assert.deepEqual(await settingsPage.evaluate(() => { const dock = document.querySelector('#usages-popover'), header = document.querySelector('.chat > header'); return { top: Math.round(dock.getBoundingClientRect().top - header.getBoundingClientRect().top), height: Math.round(dock.querySelector('header').getBoundingClientRect().height), chatHeader: Math.round(header.getBoundingClientRect().height) }; }), { top: 0, height: 43, chatHeader: 43 }, 'the usages column sits on the chat header row');
    await settingsPage.click('body', { offset: { x: 5, y: 400 } });
    assert.equal(await settingsPage.$eval('#usages-popover', el => el.hidden), false, 'an outside click leaves the docked usages open');
    await settingsPage.click('#usages-popover .usages-popover-close');
    assert.equal(await settingsPage.$eval('#usages-popover', el => el.hidden), true, 'the close button closes the usages column');
    assert.equal(await settingsPage.$eval('.chat', el => el.classList.contains('usages-open')), false);
    // On a phone the usages sheet starts below the room header, so the header (back, room menu) stays reachable.
    await settingsPage.setViewport({ width: 390, height: 844 });
    await domClick(settingsPage, '#usages-toggle'); await settingsPage.waitForSelector('#usages-popover:not([hidden])');
    assert.equal(await settingsPage.evaluate(() => { const dock = document.querySelector('#usages-popover').getBoundingClientRect(), header = document.querySelector('.chat > header').getBoundingClientRect(); return dock.top >= header.bottom - 1; }), true, 'the usages sheet must not cover the room header on a phone');
    await settingsPage.click('#usages-popover .usages-popover-close');
    await settingsPage.setViewport({ width: 1100, height: 720 });
    await settingsPage.click('#usages-toggle'); await settingsPage.waitForSelector('#usages-popover:not([hidden])');
    await settingsPage.click('#usages-toggle');
    assert.equal(await settingsPage.$eval('#usages-popover', el => el.hidden), true, 'the toggle closes the usages column');
    await domClick(settingsPage, '#usages-toggle'); await settingsPage.waitForSelector('#usages-popover:not([hidden])');
    await domClick(settingsPage, '#settings-toggle');
    await domClick(settingsPage, '#settings-usages-tab');
    await settingsPage.waitForSelector('#settings-dialog[open] .usage-provider[data-provider="codex"]');
    await settingsPage.click('#settings-notifications-tab');
    const secure = await settingsPage.evaluate(() => window.isSecureContext);
    assert.equal(await settingsPage.$eval('#notification-permission', el => el.disabled), !secure, 'notification permission button does not follow isSecureContext');
    assert.equal(await settingsPage.$eval('#notification-status', el => el.textContent.includes(window.isSecureContext ? 'Browser permission' : 'secure context')), true);
    // t2-custom-sounds: choose a bundled or uploaded inbox sound, owner uploads/deletes, deleted choices fall back to the chime.
    {
      const stored = prefix => settingsPage.evaluate(wanted => { const key = Object.keys(localStorage).find(item => item.startsWith(wanted)); return key ? localStorage.getItem(key) : null; }, prefix);
      const wav = path.join(tmp, 'custom-ping.wav');
      fs.writeFileSync(wav, Buffer.concat([Buffer.from('RIFF\x24\x00\x00\x00WAVEfmt '), Buffer.alloc(64)]));
      await settingsPage.waitForFunction(() => document.querySelectorAll('#notification-sound option').length === 4);
      assert.deepEqual(await settingsPage.$$eval('#notification-sound option', els => els.map(el => el.value)), ['builtin:chime', 'builtin:ding', 'builtin:beep', 'builtin:knock']);
      assert.equal(await settingsPage.$eval('#notification-volume', el => el.value), '70');
      // The slider shows its value beside it and reads it out as a percentage; its round thumb reaches both track ends.
      const volume = () => settingsPage.$eval('#notification-volume', el => ({ shown: document.querySelector('#notification-volume-value').textContent, spoken: el.getAttribute('aria-valuetext'), fill: el.style.getPropertyValue('--range-value') }));
      assert.deepEqual(await volume(), { shown: '70%', spoken: '70%', fill: '0.7' });
      // Pseudo-element styles are not computed for a range thumb, so the stylesheet is read instead: a round WebKit/Blink thumb over a
      // track drawn by the page rather than the browser (Chromium drops the ::-moz- twin rules, so they cannot be read here).
      const thumb = await settingsPage.$eval('#notification-volume', el => {
        const rules = [...document.styleSheets].flatMap(sheet => { try { return [...sheet.cssRules]; } catch { return []; } }).filter(rule => rule.selectorText);
        const pick = selector => { const rule = rules.find(item => item.selectorText === selector); return rule && [rule.style.width, rule.style.height, rule.style.borderRadius].join(' '); };
        return { webkit: pick('.range-input::-webkit-slider-thumb'), track: !!rules.find(item => item.selectorText === '.range-input::-webkit-slider-runnable-track'), appearance: getComputedStyle(el).appearance };
      });
      assert.deepEqual(thumb, { webkit: 'var(--range-thumb) var(--range-thumb) 50%', track: true, appearance: 'none' }, `the volume slider has a round thumb on its own track: ${JSON.stringify(thumb)}`);
      assert.equal(await settingsPage.$eval('#notification-sound-upload', el => !el.closest('[hidden]') && !el.disabled), true, 'owner cannot upload sounds');
      await settingsPage.select('#notification-sound', 'builtin:knock');
      assert.equal(await stored('aimebu2:inbox-sound:'), 'builtin:knock');
      await settingsPage.$eval('#notification-volume', el => { el.value = '35'; el.dispatchEvent(new Event('input', { bubbles: true })); });
      assert.equal(await stored('aimebu2:inbox-volume:'), '35');
      assert.deepEqual(await volume(), { shown: '35%', spoken: '35%', fill: '0.35' }, 'the volume label follows the slider');
      await settingsPage.focus('#notification-volume'); await settingsPage.keyboard.press('ArrowRight');
      assert.deepEqual(await volume(), { shown: '40%', spoken: '40%', fill: '0.4' }, 'the arrow keys move the slider and its label');
      await (await settingsPage.$('#notification-sound-upload')).uploadFile(wav);
      await settingsPage.waitForFunction(() => document.querySelectorAll('#notification-sound option').length === 5 && document.querySelector('#notification-sound').value.startsWith('user:snd_'));
      assert.match(await settingsPage.$eval('.notification-sound-list li', el => el.textContent), /custom-ping\.wav/);
      await domClick(settingsPage, '#notification-sound-preview');
      const uploaded = await settingsPage.$eval('#notification-sound', el => el.value);
      assert.equal((await settingsPage.evaluate(async id => { const response = await fetch(`/api/sounds/${id.slice(5)}`); return [response.status, response.headers.get('content-type')]; }, uploaded)).join(), '200,audio/wav');
      assert.equal(await settingsPage.evaluate(async () => {
        const { InboxNotifications } = await import('/js/inbox-notifications.js');
        const alerts = new InboxNotifications(() => {});
        alerts.soundID = 'user:snd_gone';
        alerts.play();
        await new Promise(resolve => alerts.audio.addEventListener('error', resolve, { once: true }));
        await new Promise(resolve => setTimeout(resolve, 100));
        const during = alerts.audio.getAttribute('src'), kept = alerts.soundID;
        alerts.play();
        const repointed = alerts.audio.getAttribute('src');
        alerts.soundID = 'builtin:chime';
        return [during, kept, repointed].join(' ');
      }), '/sounds/chime.wav user:snd_gone /api/sounds/snd_gone', 'a missing uploaded sound did not fall back to the chime for one play while keeping the choice');
      await domClick(settingsPage, '.notification-sound-list li button');
      await settingsPage.waitForFunction(() => document.querySelectorAll('#notification-sound option').length === 4 && document.querySelector('#notification-sound').value === 'builtin:chime' && !document.querySelector('.notification-sound-list li'));
      await settingsPage.evaluate(() => { for (const key of Object.keys(localStorage)) if (/^aimebu2:inbox-(sound|volume):/.test(key)) localStorage.removeItem(key); });
    }
    await domClick(settingsPage, '#notification-muted');
    await settingsPage.click('#settings-project-tab');
    await settingsPage.click('#settings-notifications-tab');
    assert.equal(await settingsPage.$eval('#notification-muted', el => el.checked), true, 'mute choice did not persist');
    const rowAudit = [];
    for (const tab of await settingsPage.$$eval('#settings-dialog nav button', els => els.filter(el => !el.hidden).map(el => el.id))) {
      if (tab === 'settings-import-tab') continue;
      await domClick(settingsPage, `#${tab}`);
      const section = tab.replace(/-tab$/, '');
      rowAudit.push(await settingsPage.evaluate(id => {
        const root = document.getElementById(id);
        const skip = '.usage-provider, .machine-harness, .user-members, .user-accounts, #machines-list, #harnesses-list, #usages-list, [aria-label="Invitation link"]';
        const fields = [...root.querySelectorAll('input:not([type=hidden]), select, textarea')].filter(el => !el.closest(skip));
        const rows = [...root.querySelectorAll('.settings-row')];
        return {
          id, fields: fields.length,
          unrowed: fields.filter(el => !el.closest('.settings-row')).map(el => el.id || el.name || el.tagName),
          noDesc: rows.filter(row => { const field = row.querySelector('input,select,textarea,button'); const desc = field && document.getElementById((field.getAttribute('aria-describedby') || '').split(' ')[0]); return !desc || !desc.textContent.trim() || !row.querySelector('.settings-desc'); }).map(row => row.querySelector('.settings-label')?.textContent),
          noLabel: rows.filter(row => { const label = row.querySelector('label.settings-label'); const field = label && document.getElementById(label.htmlFor); return !field || !row.contains(field); }).map(row => row.querySelector('.settings-label')?.textContent),
          rows: rows.length, rowIds: rows.map(row => row.querySelector('input,select,textarea,button')?.id), grouped: rows.every(row => row.parentElement.classList.contains('settings-group')),
        };
      }, section));
    }
    for (const audit of rowAudit) { assert.deepEqual(audit.unrowed, [], `${audit.id}: settings fields outside a settings row`); assert.deepEqual(audit.noDesc, [], `${audit.id}: settings rows without a description`); assert.deepEqual(audit.noLabel, [], `${audit.id}: settings rows whose label does not point at their control`); assert.equal(audit.grouped, true, `${audit.id}: rows must sit in a settings group`); }
    for (const id of ['settings-general', 'settings-usages', 'settings-project', 'settings-notifications', 'settings-roles', 'settings-macros']) assert.equal(rowAudit.some(item => item.id === id && item.rows > 0), true, `${id} was not audited or has no settings rows`);
    assert.deepEqual(rowAudit.find(item => item.id === 'settings-general').rowIds, ['theme-select', 'pref-system-events', 'pref-debug-inspector', 'message-expiry-seconds', 'settings-backup-export', 'settings-backup-file'], 'General holds the theme, the two chat switches, the message-expiry window, and the settings backup, all as rows');
    // One spacing system on every settings page, at desktop and phone width: each section label, card and action row ends before
    // the next one starts with a gap between them (no overlap, no touching), text inside a card keeps its padding from the card
    // edge, and row actions such as move, remove and delete are icon buttons with a tooltip and an accessible name.
    {
      const layoutAudit = id => {
        const root = document.getElementById(id);
        const shown = el => el.getClientRects().length > 0 && getComputedStyle(el).visibility !== 'hidden' && !el.closest('[hidden]') && !el.closest('details:not([open]) > :not(summary)');
        const name = el => `${el.tagName.toLowerCase()}${el.id ? `#${el.id}` : ''}.${[...el.classList].join('.')}[${el.textContent.trim().slice(0, 24)}]`;
        const blocks = [...root.querySelectorAll('h3, h4, h5, .settings-group, .settings-card, .settings-actions, .dialog-actions, .usage-provider, .usage-profile, .machine-card, .quota-banner, .quota-reset')].filter(shown);
        const touching = [];
        for (let i = 1; i < blocks.length; i++) {
          const a = blocks[i - 1], b = blocks[i];
          if (a.contains(b) || b.contains(a)) continue;
          const ra = a.getBoundingClientRect(), rb = b.getBoundingClientRect();
          if (ra.right <= rb.left || rb.right <= ra.left) continue;
          if (rb.top - ra.bottom < 4) touching.push(`${name(a)} -> ${name(b)}: gap ${(rb.top - ra.bottom).toFixed(1)}px`);
        }
        const cramped = [];
        for (const card of [...root.querySelectorAll('.settings-group, .settings-card, .usage-provider, .machine-card')].filter(shown)) {
          const box = card.getBoundingClientRect(), walker = document.createTreeWalker(card, NodeFilter.SHOW_TEXT);
          for (let node; (node = walker.nextNode());) {
            if (!node.textContent.trim() || !shown(node.parentElement)) continue;
            const host = node.parentElement.closest('button, input, select, textarea, .usage-profile, .settings-group');
            if (host && host !== card && card.contains(host)) continue;
            const range = document.createRange(); range.selectNodeContents(node);
            const rect = [...range.getClientRects()].find(item => item.width > 0);
            if (rect && (rect.left - box.left < 8 || box.right - rect.right < 8 || rect.top - box.top < 4)) cramped.push(`${name(card)}: "${node.textContent.trim().slice(0, 30)}" is ${Math.round(rect.left - box.left)}px from the left edge, ${Math.round(rect.top - box.top)}px from the top`);
          }
        }
        const textActions = [...root.querySelectorAll('button')].filter(shown).filter(button => !button.closest('.dialog-actions, .settings-actions') && /^(Up|Down|Move up|Move down|Remove|Delete)$/.test(button.textContent.trim())).map(name);
        const badIcons = [...root.querySelectorAll('button.icon-btn')].filter(shown).filter(button => !button.querySelector('.icon-mask, svg') || button.textContent.trim() || !button.title || !button.getAttribute('aria-label')).map(name);
        return { touching, cramped, textActions, badIcons };
      };
      // Machines and Models are being replaced by a page of their own and keep their current layout until then.
      const tabs = await settingsPage.$$eval('#settings-dialog nav button', els => els.filter(el => !el.hidden && !['settings-import-tab', 'settings-machines-tab', 'settings-models-tab'].includes(el.id)).map(el => el.id));
      for (const [width, height] of [[1100, 720], [390, 844]]) {
        await settingsPage.setViewport({ width, height });
        // The owner's full nav in both groups; on a phone the captions are gone but the gap before Machines stays.
        const nav = await settingsNav(settingsPage);
        assert.deepEqual(nav.items, ['SETTINGS', 'general', 'usages', 'project', 'notifications', 'roles', 'users', 'prompts', 'macros', 'AGENTS', 'machines', 'harnesses'], `Settings nav order at ${width}px`);
        assert.ok(!nav.focusable && nav.gap >= nav.tabGap + 14, `the AGENTS group is set apart at ${width}px: ${JSON.stringify(nav)}`);
        for (const tab of tabs) {
          await domClick(settingsPage, `#${tab}`);
          const section = tab.replace(/-tab$/, '');
          // Sections fill in as their requests answer; the layout is judged once it has settled.
          let report;
          for (let attempt = 0; attempt < 20; attempt++) {
            await new Promise(resolve => setTimeout(resolve, 250));
            report = await settingsPage.evaluate(layoutAudit, section);
            if (!Object.values(report).some(list => list.length)) break;
          }
          assert.deepEqual(report, { touching: [], cramped: [], textActions: [], badIcons: [] }, `${section} at ${width}px: settings blocks must not touch or overlap, card text keeps its padding, row actions are icons`);
        }
      }
      await settingsPage.setViewport({ width: 1100, height: 720 });
      assert.equal(await settingsPage.$('#settings-welcome, #settings-welcome-play'), null, 'Settings -> General has no Welcome section');
    }
    const geometry = await settingsPage.evaluate(() => { document.querySelector('#settings-general-tab').click(); const row = document.querySelector('#settings-general .settings-row'), info = row.querySelector('.settings-row-info').getBoundingClientRect(), control = row.querySelector('.settings-control').getBoundingClientRect(); const label = getComputedStyle(row.querySelector('.settings-label')).fontSize, desc = getComputedStyle(row.querySelector('.settings-desc')).fontSize; return { left: info.left < control.left, label, desc, tall: control.height >= 32 }; });
    assert.deepEqual(geometry, { left: true, label: '11.48px', desc: '10.5px', tall: true }, 'label and description sit left of the control, in the v1 type scale');
    await domClick(settingsPage, '#settings-usages-tab');
    assert.equal(await settingsPage.$$eval('.usage-provider[data-provider="codex"] .usage-profile', els => els.length), 2);
    assert.equal(await settingsPage.$eval('.usage-provider[data-provider="codex"] .usage-window-foot', el => el.textContent.includes('reserve')), true);
    assert.equal(await settingsPage.$eval('.usage-provider[data-provider="codex"] .usage-profile-credits', el => el.textContent), 'Credits: 12.50', 'credits line should render');
    assert.equal(await settingsPage.$$eval('.usage-provider[data-provider="codex"] .usage-window strong:first-child', els => els.map(el => el.textContent).includes('Codex Spark')), true, 'spark window label should render');
    assert.equal(await settingsPage.$eval('.usage-provider[data-provider="codex"] .usage-switch:not([hidden])', el => el.disabled), false, 'configured profile switch should be available');
    await answerDialogs(settingsPage, () => ({ accept: true, values: { value: 'reserve' } }));
    await settingsPage.evaluate(() => [...document.querySelectorAll('.usage-provider[data-provider="codex"] .usage-profile')].find(el => el.querySelector('.usage-profile-name').textContent === 'backup').querySelector('.usage-rename').click());
    await settingsPage.waitForFunction(() => [...document.querySelectorAll('.usage-provider[data-provider="codex"] .usage-profile-name')].some(el => el.textContent === 'reserve'));
    assert.equal(await settingsPage.$$eval('.usage-provider[data-provider="codex"] .usage-profile-name', els => els.some(el => el.textContent === 'backup')), false, 'renamed profile kept its old name');
    // Automatic account switching (mock source): defaults, order editing, limits, protected profiles, validation,
    // save/reopen, dry run, re-arm, keyboard focus, escaping, the paused banner, and a phone-width fit.
    {
      const claude = '.quota-tool[data-tool="claude"]', codex = '.quota-tool[data-tool="codex"]';
      await settingsPage.waitForSelector(`${claude} .quota-last`);
      assert.deepEqual(await settingsPage.$$eval('.quota-tool-title', els => els.map(el => el.textContent)), ['Claude', 'Codex']);
      assert.equal(await settingsPage.$$eval('.quota-defaults', els => els.length), 2, 'unsaved defaults are called out for each tool');
      assert.match(await settingsPage.$eval(`${claude} .quota-hold`, el => el.textContent), /Current hold: warming up/);
      assert.equal(await settingsPage.$$eval(`${claude} .quota-reset button`, els => els.length), 1, 'a latched reset offers Re-arm');
      assert.equal(await settingsPage.$(`${codex} .quota-reset`), null);
      const names = tool => settingsPage.$$eval(`${tool} [aria-label$="session limit (%)"]`, els => els.map(el => el.getAttribute('aria-label').replace(' session limit (%)', '')));
      assert.deepEqual(await names(claude), ['local', 'spare']);
      assert.equal(await settingsPage.$eval(`${claude} [data-quota-focus="protect:parked"]`, el => el.checked), true, 'a protected profile is checked');
      assert.equal(await settingsPage.$eval(`${claude} #quota-claude-order-add`, el => el.disabled && el.options[0].textContent), 'No other profiles', 'nothing unlisted and unprotected is left to add');
      assert.equal(await settingsPage.$eval(`${claude} .quota-save`, el => el.disabled), false, 'unsaved defaults can be saved as they are');
      assert.equal(await settingsPage.$eval(`${claude} .quota-discard`, el => el.disabled), true, 'nothing to discard before an edit');
      // Reorder by keyboard-operable buttons; focus follows the moved row.
      await settingsPage.click(`${claude} [aria-label="Move local down"]`);
      assert.deepEqual(await names(claude), ['spare', 'local']);
      assert.equal(await settingsPage.evaluate(() => document.activeElement?.getAttribute('aria-label')), 'Move local up', 'focus stayed with the moved row');
      assert.equal(await settingsPage.$eval(`${claude} .quota-save`, el => !el.disabled) && await settingsPage.$eval(`${claude} .quota-unsaved`, el => !el.hidden), true);
      // A bad limit is refused before anything is sent.
      await settingsPage.$eval(`${claude} [aria-label="spare weekly limit (%)"]`, el => { el.value = '0'; el.dispatchEvent(new Event('input', { bubbles: true })); });
      await domClick(settingsPage, `${claude} .quota-save`);
      assert.match(await settingsPage.$eval(`${claude} .quota-error`, el => el.textContent), /limits for spare must be whole numbers from 1 to 100/);
      await settingsPage.$eval(`${claude} [aria-label="spare weekly limit (%)"]`, el => { el.value = '80'; el.dispatchEvent(new Event('input', { bubbles: true })); });
      await domClick(settingsPage, `${claude} #quota-claude-enabled`);
      // A save that waits behind a running switch says so and cannot be sent twice.
      let release; quotaMock.gate = new Promise(resolve => { release = resolve; });
      await domClick(settingsPage, `${claude} .quota-save`);
      await settingsPage.waitForFunction(selector => document.querySelector(`${selector} .quota-note`)?.textContent === 'Saving… (waits for a running switch)' && document.querySelector(`${selector} .quota-save`).disabled, {}, claude);
      release(); quotaMock.gate = null;
      await settingsPage.waitForFunction(selector => document.querySelector(`${selector} .quota-note`)?.textContent === 'Saved.', {}, claude);
      assert.match(await settingsPage.$eval(`${claude} .quota-dry-run`, el => el.textContent), /Dry run: decisions are recorded/, 'an enabled tool in dry run says nothing is switched');
      assert.equal(await settingsPage.$(`${claude} .quota-defaults`) === null && await settingsPage.$eval(`${claude} .quota-save`, el => el.disabled), true, 'saved: no defaults note, Save disabled again');
      // Discarding restores the saved copy; protecting a listed profile takes it out of the order.
      await domClick(settingsPage, `${claude} [data-quota-focus="protect:local"]`);
      assert.deepEqual(await names(claude), ['spare'], 'a protected profile leaves the order');
      await domClick(settingsPage, `${claude} .quota-discard`);
      assert.deepEqual(await names(claude), ['spare', 'local']);
      // Protecting the watched profile of the after-reset order reselects one that is still listed.
      await domClick(settingsPage, '#quota-claude-after-reset');
      assert.equal(await settingsPage.$eval('#quota-claude-watched', el => el.value), 'spare');
      await settingsPage.click(`${claude} [data-quota-focus="protect:spare"]`);
      assert.equal(await settingsPage.$eval('#quota-claude-watched', el => el.value), 'local', 'the watched profile was left on a protected one');
      // An enabled tool whose after-reset order shrank below two profiles cannot be saved, and nothing is sent.
      await domClick(settingsPage, `${claude} .quota-save`);
      assert.match(await settingsPage.$eval(`${claude} .quota-error`, el => el.textContent), /after the reset needs at least two profiles/);
      await domClick(settingsPage, `${claude} .quota-discard`);
      assert.equal(await settingsPage.$('#quota-claude-watched'), null);
      // Reopening Settings shows what the server kept.
      await domClick(settingsPage, '#settings-close'); await settingsPage.click('#settings-toggle');
      await settingsPage.waitForSelector(`${claude} .quota-last`);
      await domClick(settingsPage, '#settings-usages-tab');
      assert.deepEqual(await names(claude), ['spare', 'local'], 'the saved order survived a reopen');
      assert.equal(await settingsPage.$eval(`${claude} [aria-label="spare weekly limit (%)"]`, el => el.value), '80');
      assert.equal(await settingsPage.$eval('#quota-claude-enabled', el => el.checked), true);
      // Re-arm clears the latch.
      await domClick(settingsPage, `${claude} .quota-reset button`);
      await settingsPage.waitForFunction(selector => !document.querySelector(`${selector} .quota-reset`) && document.querySelector(`${selector} .quota-note`)?.textContent === 'Re-armed.', {}, claude);
      // Recent decisions: closed by default, then the list.
      assert.equal(await settingsPage.$eval(`${claude} .quota-decisions`, el => el.open), false);
      await settingsPage.click(`${claude} .quota-decisions > summary`);
      assert.deepEqual(await settingsPage.$$eval(`${claude} .quota-decision`, els => els.map(el => el.dataset.action)), ['hold', 'switch']);
      // A live push while the owner is typing replaces only the server-driven blocks: the same field element, with its value, survives.
      // A second Settings section over the same intercepted API plays the open tab; the server's state moves between the page calls.
      await settingsPage.evaluate(async () => {
        const { QuotaSettings } = await import('/js/quota-settings.js');
        const { HttpQuotaSource } = await import('/js/quota-source.js');
        const quota = window.__quota = new QuotaSettings(new HttpQuotaSource());
        document.querySelector('#settings-usages').append(quota.node); // inside the modal Settings dialog: anything outside it is inert and cannot take focus
        await quota.sync({ owner: true, enabled: true });
        const input = window.__typingInput = quota.node.querySelector('[aria-label="local weekly limit (%)"]');
        input.focus(); input.value = '9'; input.dispatchEvent(new Event('input', { bubbles: true }));
        window.__evaluatedBefore = quota.node.querySelector('.quota-evaluated').textContent;
      });
      quotaMock.tools.claude.state.evaluated_at = new Date(Date.now() + 90000).toISOString();
      quotaMock.tools.claude.decisions.unshift(quotaMock.decision('claude', 'stay', 'local', null, 'x'));
      quotaMock.tools.claude.last_decision = quotaMock.tools.claude.decisions[0];
      quotaMock.tools.codex.config.enabled = true; quotaMock.tools.codex.saved = true; // another tab saved codex meanwhile
      const typing = await settingsPage.evaluate(async () => {
        const quota = window.__quota, input = window.__typingInput;
        await quota.sync({ owner: true, enabled: true });
        const out = { same: document.activeElement === input && quota.node.contains(input), value: input.value, statusMoved: quota.node.querySelector('.quota-evaluated').textContent !== window.__evaluatedBefore, decisions: quota.node.querySelectorAll('.quota-tool[data-tool="claude"] .quota-decision').length, codexDuringEdit: quota.node.querySelector('#quota-codex-enabled').checked };
        input.blur();
        await new Promise(resolve => setTimeout(resolve, 50));
        out.codexAfterBlur = quota.node.querySelector('#quota-codex-enabled').checked; out.draftKept = quota.node.querySelector('[aria-label="local weekly limit (%)"]').value;
        // A focused field whose tool has no typed edits is rebuilt at once when the server's configuration changed.
        quota.node.querySelector('#quota-codex-dry-run').focus();
        return out;
      });
      quotaMock.tools.codex.config.dry_run = !quotaMock.tools.codex.config.dry_run;
      typing.cleanFocusRefreshed = await settingsPage.evaluate(async dryRun => {
        const quota = window.__quota;
        await quota.sync({ owner: true, enabled: true });
        const refreshed = quota.node.querySelector('#quota-codex-dry-run').checked === dryRun && document.activeElement?.id === 'quota-codex-dry-run';
        quota.node.remove(); delete window.__quota;
        return refreshed;
      }, quotaMock.tools.codex.config.dry_run);
      assert.deepEqual(typing, { same: true, value: '9', statusMoved: true, decisions: 3, codexDuringEdit: false, codexAfterBlur: true, draftKept: '9', cleanFocusRefreshed: true }, 'a live push rebuilt the field being typed in, or the server copy never arrived after focus left (the field element and its value must survive the push; the rest catches up on blur)');
      // Server text is plain text, and a tripped breaker is a visible alert with a re-enable button.
      const hostile = await settingsPage.evaluate(async () => {
        const { QuotaSettings } = await import('/js/quota-settings.js');
        const decision = { at: new Date().toISOString(), tool: 'claude', action: 'hold', from: '<img src=x onerror=window.__quotaXss=1>', to: null, reason: '<b>bold</b>', dry_run: false, profiles: { '<i>p</i>': { status: '<u>ok</u>', session: 1, weekly: 2, ok_for_seconds: 3 } } };
        let reenabled = 0;
        const tool = { config: { enabled: true, dry_run: false, profiles: [{ name: '<i>p</i>', session_max: 95, weekly_max: 95 }, { name: 'q', session_max: 95, weekly_max: 95 }], protected: [], after_reset: null, warmup_seconds: 60, min_dwell_minutes: 5 }, saved: true, state: { tripped: true, tripped_at: new Date().toISOString(), hold: '<b>hold</b>', evaluated_at: null }, last_decision: decision, decisions: [decision] };
        const source = { load: async () => ({ profiles: { claude: ['<i>p</i>', 'q'], codex: [] }, tools: { claude: tool, codex: { ...tool, state: {}, decisions: [], last_decision: null } } }), reenable: async () => { reenabled++; tool.state.tripped = false; return tool; } };
        const quota = new QuotaSettings(source);
        document.querySelector('#settings-usages').append(quota.node); // inside the modal Settings dialog: anything outside it is inert and cannot take focus
        await quota.sync({ owner: true, enabled: true });
        const box = quota.node.querySelector('.quota-tool[data-tool="claude"]');
        const out = { markup: quota.node.querySelectorAll('img, b, i, u').length, xss: window.__quotaXss || 0, hold: box.querySelector('.quota-hold').textContent, last: box.querySelector('.quota-last').textContent, alert: box.querySelector('.quota-banner')?.getAttribute('role') };
        box.querySelector('.quota-banner button').click();
        await new Promise(resolve => setTimeout(resolve, 50));
        out.reenabled = reenabled; out.bannerGone = !quota.node.querySelector('.quota-banner');
        await quota.sync({ owner: false, enabled: true });
        out.hiddenForNonOwner = quota.node.hidden && !quota.node.childElementCount;
        quota.node.remove();
        return out;
      });
      assert.deepEqual(hostile, { markup: 0, xss: 0, hold: 'Current hold: <b>hold</b>', last: hostile.last, alert: 'alert', reenabled: 1, bannerGone: true, hiddenForNonOwner: true }, 'server text rendered as markup, or the breaker banner failed');
      assert.match(hostile.last, /hold: <b>bold<\/b>/);
      await settingsPage.setViewport({ width: 390, height: 780 });
      assert.equal(await settingsPage.$eval('.quota-settings', el => el.scrollWidth <= el.clientWidth + 1), true, 'the switching section overflows a phone viewport');
      await settingsPage.setViewport({ width: 1100, height: 720 });
    }
    // Limits before 100% (mock source): the thresholds, validation, save, a per-profile override, and the rows in the Health list.
    {
      const claude = '.guard-tool[data-tool="claude"]', codex = '.guard-tool[data-tool="codex"]';
      const type = (selector, value) => settingsPage.$eval(selector, (el, v) => { el.value = v; el.dispatchEvent(new Event('input', { bubbles: true })); }, value);
      await settingsPage.waitForSelector(`${claude} .guard-live`);
      assert.deepEqual(await settingsPage.$$eval('.guard-tool .guard-tool-title', els => els.map(el => el.textContent)), ['Claude', 'Codex']);
      assert.match(await settingsPage.$eval(`${claude} .guard-live`, el => el.textContent), /^Claude \(local\) is at 96% of its weekly window: new turns are held, running ones finish\.$/);
      assert.match(await settingsPage.$eval(`${codex} .guard-live`, el => el.textContent), /^Codex \(main\) is at 40% of its session window\.$/);
      assert.deepEqual(await settingsPage.$eval(claude, el => [el.querySelector('#guard-claude-soft').value, el.querySelector('#guard-claude-hard').value, el.querySelector('#guard-claude-enabled').checked]), ['95', '99', true], 'the Claude defaults are 95 and 99, on');
      assert.deepEqual(await settingsPage.$eval(codex, el => [el.querySelector('#guard-codex-soft').value, el.querySelector('#guard-codex-hard').value]), ['98', '99'], 'the Codex defaults are its own switcher limit and 99');
      assert.equal(await settingsPage.$eval(`${claude} .guard-save`, el => el.disabled), false, 'unsaved defaults can be saved as they are');
      // Soft must stay below hard: refused before anything is sent.
      await type('#guard-claude-soft', '100');
      await domClick(settingsPage, `${claude} .guard-save`);
      assert.match(await settingsPage.$eval(`${claude} .guard-error`, el => el.textContent), /soft threshold must be below the hard one/);
      assert.equal(guardMock.tools.claude.saved, false, 'a refused draft is not sent');
      await type('#guard-claude-soft', '80'); await type('#guard-claude-hard', '90');
      await domClick(settingsPage, `${claude} .guard-save`);
      await settingsPage.waitForFunction(selector => document.querySelector(`${selector} .guard-note`)?.textContent === 'Saved.', {}, claude);
      assert.deepEqual([guardMock.tools.claude.config.soft, guardMock.tools.claude.config.hard], [80, 90]);
      assert.equal(await settingsPage.$eval(`${claude} .guard-save`, el => el.disabled), true, 'saved: Save is disabled again');
      // A profile with its own thresholds.
      await settingsPage.select('#guard-claude-add', 'spare');
      await domClick(settingsPage, `${claude} [aria-label="Give the chosen Claude profile its own thresholds"]`);
      assert.deepEqual(await settingsPage.$eval(claude, el => [el.querySelector('#guard-claude-o0-soft').value, el.querySelector('#guard-claude-o0-hard').value]), ['80', '90'], 'an override starts from the tool thresholds');
      await type('#guard-claude-o0-soft', '50'); await type('#guard-claude-o0-hard', '60');
      await domClick(settingsPage, `${claude} .guard-save`);
      await settingsPage.waitForFunction(selector => document.querySelector(`${selector} .guard-note`)?.textContent === 'Saved.', {}, claude);
      assert.deepEqual(guardMock.tools.claude.config.profiles, { spare: { soft: 50, hard: 60 } });
      await domClick(settingsPage, `${claude} [aria-label="Remove the own thresholds of spare"]`);
      await domClick(settingsPage, `${claude} .guard-save`);
      await settingsPage.waitForFunction(selector => document.querySelector(`${selector} .guard-note`)?.textContent === 'Saved.', {}, claude);
      assert.deepEqual(guardMock.tools.claude.config.profiles, {}, 'removing an override saves without it');
      await settingsPage.setViewport({ width: 390, height: 780 });
      assert.equal(await settingsPage.$eval('.limit-guard-settings', el => el.scrollWidth <= el.clientWidth + 1), true, 'the limits section overflows a phone viewport');
      await settingsPage.setViewport({ width: 1100, height: 720 });
      // Health: the held tool is counted and drawn with its thresholds; a tool under the soft line is listed but not counted.
      await settingsPage.evaluate(() => document.querySelector('#health-toggle').click());
      await settingsPage.waitForSelector('#health-popover .health-limit[data-tool="claude"][data-state="soft"]');
      assert.deepEqual(await settingsPage.$$eval('#health-popover .health-limit', els => els.map(el => `${el.dataset.tool}:${el.dataset.state}:${el.querySelectorAll('.health-limit-tick').length}`)), ['claude:soft:2', 'codex:ok:2']);
      assert.equal(await settingsPage.$eval('#health-count', el => !el.hidden && el.textContent), '1', 'a tool holding new turns counts toward the badge');
      assert.match(await settingsPage.$eval('#health-popover .health-limit[data-tool="claude"] .health-limit-text', el => el.textContent), /new turns are held/);
      await settingsPage.evaluate(() => document.querySelector('#health-toggle').click());
      // Rebuilding the agent list does not ask for the limit guard again: it is read once per sign-in and then on the timer, and a
      // person who is not the owner is not asked again at all.
      const fresh = () => settingsPage.evaluate(async () => {
        const version = new URL(document.querySelector('script[type=module][src]').src).search;
        const { Health } = await import(`/js/health.js${version}`);
        const health = new Health({ setHealth() {}, rooms: new Map() }, { cards: new Map() }, () => {}, () => {});
        clearInterval(health.limitTimer);
        // Count this instance's own reads: the signed-in page's Health also polls the same mock on its timer, so the mock's global
        // counter can include a read that is not this instance's.
        let reads = 0; const load = health.limitGuard.load.bind(health.limitGuard);
        health.limitGuard.load = (...args) => { reads++; return load(...args); };
        for (let i = 0; i < 3; i++) await health.refresh();
        return { gone: health.guardGone, reads };
      });
      const rebuilt = await fresh();
      assert.equal(rebuilt.gone, false);
      assert.equal(rebuilt.reads, 1, 'three rebuilds of the health list read the limit guard once');
      guardMock.forbidden = true;
      const refused = await fresh();
      assert.equal(refused.gone, true, 'a 403 marks the guard as not for this person');
      assert.equal(refused.reads, 1, 'a non-owner is asked once and then left alone');
      guardMock.forbidden = false;
    }
    await settingsPage.$eval('.usage-provider[data-provider="mistral"] textarea', el => { el.value = 'Cookie: csrftoken=abc; ory_session_test=secret'; el.dispatchEvent(new Event('input', { bubbles: true })); });
    await settingsPage.evaluate(() => [...document.querySelectorAll('.usage-provider[data-provider="mistral"] button')].find(el => el.textContent === 'Save cookie').click());
    await settingsPage.waitForFunction(() => [...document.querySelectorAll('.usage-provider[data-provider="mistral"] button')].some(el => el.textContent === 'Clear cookie'));
    assert.equal(await settingsPage.$eval('.usage-provider[data-provider="mistral"] textarea', el => el.value), '', 'saved cookie leaked into Settings');
    await settingsPage.$eval('.usage-provider[data-provider="ollama-cloud"] input[type="password"]', el => { el.value = 'ollama-secret'; el.dispatchEvent(new Event('input', { bubbles: true })); });
    await settingsPage.select('.usage-provider[data-provider="ollama-cloud"] select', 'api_key');
    await settingsPage.evaluate(() => [...document.querySelectorAll('.usage-provider[data-provider="ollama-cloud"] button')].find(el => el.textContent === 'Save credentials').click());
    await settingsPage.waitForFunction(() => [...document.querySelectorAll('.usage-provider[data-provider="ollama-cloud"] button')].some(el => el.textContent === 'Clear credentials'));
    assert.equal(await settingsPage.$eval('.usage-provider[data-provider="ollama-cloud"] input[type="password"]', el => el.value), '', 'saved API key leaked into Settings');
    await settingsPage.evaluate(() => [...document.querySelectorAll('.usage-provider[data-provider="mistral"] button')].find(el => el.textContent === 'Clear cookie').click());
    await settingsPage.waitForFunction(() => ![...document.querySelectorAll('.usage-provider[data-provider="mistral"] button')].some(el => el.textContent === 'Clear cookie'));
    await settingsPage.evaluate(() => [...document.querySelectorAll('.usage-provider[data-provider="ollama-cloud"] button')].find(el => el.textContent === 'Clear credentials').click());
    await settingsPage.waitForFunction(() => ![...document.querySelectorAll('.usage-provider[data-provider="ollama-cloud"] button')].some(el => el.textContent === 'Clear credentials'));
    await settingsPage.evaluate(() => [...document.querySelectorAll('.usage-provider[data-provider="github-copilot"] button')].find(el => el.textContent === 'Sign in with GitHub').click());
    await settingsPage.waitForFunction(() => [...document.querySelectorAll('.usage-provider[data-provider="github-copilot"] button')].some(el => el.textContent === 'Sign out'));
    assert.equal(await settingsPage.$eval('.usage-provider[data-provider="codex"] .usage-affected', el => el.textContent.includes('milo')), true, 'switch impact should name affected agents');
    assert.deepEqual(await settingsPage.$$eval('#settings-usages [aria-label="Account maintenance"] .usage-token-state', els => els.map(el => el.textContent)), ['Claude: off', 'Codex: off'], 'Account maintenance shows each tool as off by default');
    await domClick(settingsPage, '#usage-token-auto-refresh');
    await settingsPage.waitForFunction(() => document.querySelector('#usage-token-auto-refresh')?.checked && [...document.querySelectorAll('.usage-token-state')].some(el => el.textContent === 'Codex: waiting: codex is running'));
    assert.equal(await settingsPage.$eval('.usage-credential-alerts', el => el.getAttribute('role') + ':' + el.textContent), 'alert:claude live: the access token expires in 4 min.', 'credential alerts render as plain text');
    assert.equal(await settingsPage.$eval('#usage-token-auto-refresh', el => el.getAttribute('aria-describedby') && document.getElementById(el.getAttribute('aria-describedby')).textContent.includes('must stay running')), true, 'the server-uptime dependency is described on the toggle');
    await domClick(settingsPage, '#usage-token-auto-refresh');
    await settingsPage.waitForFunction(() => !document.querySelector('#usage-token-auto-refresh').checked && [...document.querySelectorAll('.usage-token-state')].every(el => el.textContent.endsWith(': off')));
    assert.equal(await settingsPage.$('.usage-credential-alerts'), null, 'no credential alerts once the refresher is off');
    await settingsPage.click('#settings-project-tab');
    await settingsPage.waitForFunction(() => !document.querySelector('#project-auto-resume').disabled);
    assert.equal(await settingsPage.$eval('#project-auto-resume', el => el.checked), false, 'auto-resume must default off');
    assert.equal(await settingsPage.$eval('.settings-risk', el => el.textContent.includes('may repeat an action')), true);
    await domClick(settingsPage, '#project-auto-resume');
    await settingsPage.waitForFunction(() => !document.querySelector('#project-auto-resume').disabled);
    await settingsPage.click('#settings-usages-tab');
    await domClick(settingsPage, '#settings-project-tab');
    await settingsPage.waitForFunction(() => !document.querySelector('#project-auto-resume').disabled);
    assert.equal(await settingsPage.$eval('#project-auto-resume', el => el.checked), true, 'saved project toggle was lost');
    await settingsPage.evaluate(() => {
      window.__harnessFetch = window.fetch;
      window.__harnessWrites = [];
      window.__harnessRows = [
        { config: { id: 'mln_claude', machine_id: 'mch_mock', tool: 'claude', name: 'native', command: '/usr/bin/claude', args: [], env_refs: {}, enabled: false, containerized: false, agent_server_url: '', path_maps: [], visible_roots: [], supports_toolbox: true, preset: true }, version: '2.1', available: false, unavailable_reason: 'disabled by owner', active_agents: 0 },
        { config: { id: 'mln_codex', machine_id: 'mch_mock', tool: 'codex', name: 'harness-docker', command: '/usr/bin/docker', args: [], env_refs: {}, enabled: true, containerized: true, agent_server_url: '', path_maps: [], visible_roots: [], supports_toolbox: true, preset: true }, version: '29.1', available: true, active_agents: 0 },
      ];
      const json = value => new Response(JSON.stringify(value), { status: 200, headers: { 'Content-Type': 'application/json' } });
      window.fetch = async (input, options = {}) => {
        const path = String(input);
        if (path === '/api/machines') return json({ machines: [{ id: 'mch_mock', name: 'Host', approved: true, last_seen: new Date().toISOString() }] });
        if (path === '/api/machines/mch_mock/launchers' && options.method !== 'POST') return json({ machine_id: 'mch_mock', launchers: window.__harnessRows, activity: window.__harnessWrites.map(change => ({ ...change, created_at: new Date().toISOString(), actor: 'owner' })) });
        // Auto-detect harnesses -> Add selected also detects and saves the new harness's models.
        if (path === '/api/machines/mch_mock/launchers/detect') return json({ candidates: [{ family: 'pi', label: 'Pi', command: '/usr/bin/pi', containerized: false, already_added: false }, { family: 'pi', label: 'Pi (Docker)', command: '/usr/bin/pi-docker', containerized: true, already_added: false }] });
        if (path === '/api/machines/mch_mock/launchers' && options.method === 'POST') {
          const body = JSON.parse(options.body);
          const config = { ...body, id: body.containerized ? 'mln_auto_docker' : 'mln_auto', machine_id: 'mch_mock', name: body.containerized ? 'pi-docker' : 'native', agent_server_url: '' };
          window.__harnessRows.push({ config, available: true, unavailable_reason: '' });
          return json({ config, observed: window.__harnessRows.at(-1) });
        }
        if (path === '/api/machines/mch_mock/launchers/mln_auto/models/detect') return json({ source: 'harness', candidates: Array.from({ length: 66 }, (_, index) => ({ slug: `pi-${index}`, label: `Pi ${index}`, note: '', already_listed: false })) });
        // The Docker one cannot answer from the default folder; asked in a chosen folder, it lists one model.
        if (path === '/api/machines/mch_mock/launchers/mln_auto_docker/models/detect') {
          const body = JSON.parse(options.body || '{}'); (window.__dockerDetect ||= []).push(body);
          if (!body.dir) return new Response(JSON.stringify({ error: 'detect_failed', message: "Couldn't detect the models: the harness could not list its models (exit status 1): the harness container is not running. Add the model ID manually." }), { status: 502, headers: { 'Content-Type': 'application/json' } });
          return json({ source: 'harness', candidates: [{ slug: 'ollama-cloud/gemma4:31b', label: 'gemma4:31b', note: '', already_listed: false }] });
        }
        if (path === '/api/machines/mch_mock/launchers/mln_auto/models' && options.method === 'PUT') { window.__autoModels = JSON.parse(options.body).models; return json({ models: window.__autoModels }); }
        if (path.startsWith('/api/machines/mch_mock/launchers/') && options.method === 'PUT') {
          const id = path.split('/').pop();
          const body = JSON.parse(options.body);
          const row = window.__harnessRows.find(item => item.config.id === id);
          window.__harnessWrites.push({ id, old_command: row.config.command, new_command: body.command, old_enabled: row.config.enabled, new_enabled: body.enabled });
          Object.assign(row.config, body);
          row.available = body.enabled;
          row.unavailable_reason = body.enabled ? '' : 'disabled by owner';
          return json({ config: row.config, observed: row });
        }
        return window.__harnessFetch(input, options);
      };
    });
    await settingsPage.click('#settings-harnesses-tab');
    await settingsPage.waitForFunction(() => document.querySelectorAll('.machine-harness').length === 2);
    assert.equal(await settingsPage.$eval('.machine-harness[data-variant="native"] [name=enabled]', el => el.disabled), false, 'owner must be able to enable native launcher');
    assert.equal(await settingsPage.$eval('.machine-harness[data-variant="native"] .machine-harness-status', el => el.textContent), 'Disabled');
    // Adding a detected harness also detects its models and saves them, the first as the default.
    await domClick(settingsPage, '#harness-detect');
    await settingsPage.waitForSelector('#harness-detect-panel input[type=checkbox]');
    await settingsPage.$$eval('#harness-detect-panel input[type=checkbox]', boxes => boxes.forEach(el => el.click()));
    await domClick(settingsPage, '#harness-detect-panel button[type=submit]');
    await settingsPage.waitForFunction(() => window.__autoModels);
    // 66 were offered, a list holds 64: the first 64 are saved, the first is the only default, and the page says it cut the list.
    assert.deepEqual(await settingsPage.evaluate(() => ({ count: window.__autoModels.length, first: window.__autoModels[0].slug, last: window.__autoModels.at(-1).slug, defaults: window.__autoModels.filter(model => model.default).map(model => model.slug) })), { count: 64, first: 'pi-0', last: 'pi-63', defaults: ['pi-0'] }, 'detected models are capped at 64 with the first as the default');
    await settingsPage.waitForFunction(() => document.querySelector('#settings-harnesses > .error')?.textContent.includes('only the first 64'));
    // The Docker one could not list its models: the page says why, and its Models panel opens on that reason with a folder to retry in.
    assert.ok((await settingsPage.$eval('#settings-harnesses > .error', el => el.textContent)).includes('A harness has no models yet. Couldn\'t detect the models: the harness could not list its models (exit status 1): the harness container is not running.'), 'the auto-fill failure says why');
    await domClick(settingsPage, '[aria-controls="harness-models-mln_auto_docker"]');
    await settingsPage.waitForFunction(() => !document.querySelector('#harness-models-mln_auto_docker').hidden && !document.querySelector('#harness-models-mln_auto_docker .harness-detect-retry').hidden);
    assert.ok((await settingsPage.$eval('#harness-models-mln_auto_docker .error', el => el.textContent)).includes('the harness container is not running'), 'the Models panel opens on the failure');
    assert.deepEqual(await settingsPage.$eval('#harness-models-mln_auto_docker .harness-detect-retry-button', el => [el.title, el.getAttribute('aria-label'), !!el.querySelector('.icon-mask-refresh'), el.textContent.trim()]), ['Retry auto-detect in this folder', 'Retry auto-detect in this folder', true, ''], 'Retry is an icon button with a tooltip and an accessible name');
    await domClick(settingsPage, '#harness-models-mln_auto_docker .harness-detect-retry-button');
    assert.ok((await settingsPage.$eval('#harness-models-mln_auto_docker .error', el => el.textContent)).includes('absolute folder path'), 'a retry needs an absolute folder');
    await settingsPage.locator('#harness-models-mln_auto_docker [name=detect-dir]').fill('/work/project');
    await domClick(settingsPage, '#harness-models-mln_auto_docker .harness-detect-retry-button');
    await settingsPage.waitForSelector('#harness-models-mln_auto_docker .harness-model-candidates [data-slug="ollama-cloud/gemma4:31b"]');
    assert.deepEqual(await settingsPage.evaluate(() => window.__dockerDetect), [{}, { dir: '/work/project' }], 'the retry sends the chosen folder; the first try sent none');
    assert.deepEqual(await settingsPage.$eval('#harness-models-mln_auto_docker', panel => [panel.querySelector('.harness-detect-retry').hidden, panel.querySelector('.error').textContent]), [true, ''], 'a successful retry clears the failure');
    await settingsPage.evaluate(() => { window.__harnessRows = window.__harnessRows.filter(row => row.config.id !== 'mln_auto' && row.config.id !== 'mln_auto_docker'); });
    await domClick(settingsPage, '#settings-usages-tab');
    await settingsPage.click('#settings-harnesses-tab');
    await settingsPage.waitForFunction(() => document.querySelectorAll('.machine-harness').length === 2);
    // A launcher is a summary row: name, status, summary line and the Enabled switch. The executable, arguments and the rest sit behind Edit.
    assert.deepEqual(await settingsPage.$eval('.machine-harness[data-variant="harness-docker"]', card => { const form = card.querySelector('form'), edit = card.querySelector('.machine-harness-edit'); return { row: !!card.querySelector(':scope > .settings-row .settings-label'), formHidden: form.hidden || getComputedStyle(form).display === 'none', edit: [edit.textContent, edit.getAttribute('aria-expanded'), edit.getAttribute('aria-controls') === form.id], switchInRow: !!card.querySelector(':scope > .settings-row [name=enabled]'), commandVisible: !!card.querySelector('[name=command]')?.offsetParent }; }), { row: true, formHidden: true, edit: ['Edit', 'false', true], switchInRow: true, commandVisible: false }, 'the launcher is a summary row; its executable fields are behind Edit');
    await domClick(settingsPage, '.machine-harness[data-variant="harness-docker"] .machine-harness-edit');
    assert.deepEqual(await settingsPage.$eval('.machine-harness[data-variant="harness-docker"]', card => [card.querySelector('.machine-harness-edit').textContent, card.querySelector('.machine-harness-edit').getAttribute('aria-expanded'), !!card.querySelector('[name=command]').offsetParent]), ['Done', 'true', true]);
    await settingsPage.locator('.machine-harness[data-variant="harness-docker"] [name=command]').fill('/opt/custom/docker');
    await domClick(settingsPage, '.machine-harness[data-variant="harness-docker"] button[type=submit]');
    await settingsPage.waitForFunction(() => document.querySelector('.machine-harness[data-variant="harness-docker"] [name=command]')?.value === '/opt/custom/docker' && document.querySelectorAll('.machine-activity-item').length === 1);
    assert.deepEqual(await settingsPage.evaluate(() => window.__harnessWrites[0]), { id: 'mln_codex', old_command: '/usr/bin/docker', new_command: '/opt/custom/docker', old_enabled: true, new_enabled: true });
    // The card stays open across a save, and the Enabled switch saves on its own and only itself: edits still open in the form stay unsaved.
    assert.equal(await settingsPage.$eval('.machine-harness[data-variant="harness-docker"] [name=command]', el => !!el.offsetParent), true, 'the Edit form stays open after Save');
    await settingsPage.$eval('.machine-harness[data-variant="harness-docker"] [name=command]', el => { el.value = '/opt/unsaved/docker'; el.dispatchEvent(new Event('input', { bubbles: true })); });
    await domClick(settingsPage, '.machine-harness[data-variant="harness-docker"] [name=enabled]');
    await settingsPage.waitForFunction(() => document.querySelectorAll('.machine-activity-item').length === 2);
    assert.equal(await settingsPage.$eval('.machine-harness[data-variant="harness-docker"] [name=command]', el => el.value), '/opt/unsaved/docker', 'toggling Enabled must keep the unsaved command text in the form');
    assert.deepEqual(await settingsPage.evaluate(() => window.__harnessWrites[1]), { id: 'mln_codex', old_command: '/opt/custom/docker', new_command: '/opt/custom/docker', old_enabled: true, new_enabled: false });
    // Saving the form afterwards does send the edited command.
    await settingsPage.click('.machine-harness[data-variant="harness-docker"] button[type=submit]');
    await settingsPage.waitForFunction(() => document.querySelectorAll('.machine-activity-item').length === 3);
    assert.deepEqual(await settingsPage.evaluate(() => window.__harnessWrites[2]), { id: 'mln_codex', old_command: '/opt/custom/docker', new_command: '/opt/unsaved/docker', old_enabled: false, new_enabled: false });
    assert.equal(await settingsPage.$eval('.machine-harness[data-variant="harness-docker"] .machine-harness-status', el => el.textContent), 'Disabled');
    await settingsPage.evaluate(() => { window.fetch = window.__harnessFetch; });
    await settingsPage.setViewport({ width: 390, height: 780 });
    assert.equal(await settingsPage.$eval('#settings-dialog', el => Math.abs(el.getBoundingClientRect().height - innerHeight) < 1), true);
    await settingsPage.click('#settings-close');
    assert.equal(await settingsPage.$eval('#settings-dialog', el => el.open), false);
    const recoveryContext = await browser.createBrowserContext(), recoveryPage = await recoveryContext.newPage();
    await login(recoveryPage, `${origin}/?mock_recovery=1`);
    await recoveryPage.waitForFunction(() => document.querySelector('#inbox-count')?.textContent === '2');
    await domClick(recoveryPage, '#inbox-toggle');
    assert.deepEqual(await recoveryPage.$$eval('[data-inbox-id="mock_recovery"] .inbox-decisions button', els => els.map(el => el.textContent)), ['Resume', 'Retry', 'Abandon']);
    assert.deepEqual(await recoveryPage.$$eval('[data-inbox-id="mock_budget"] .inbox-decisions button', els => els.map(el => el.textContent)), ['Continue', 'Abandon']);
    assert.equal(await recoveryPage.$eval('[data-inbox-id="mock_recovery"] .inbox-diagnostic pre', el => el.textContent.includes('files inspected')), true);
    assert.equal(await recoveryPage.$eval('[data-inbox-id="mock_recovery"] .inbox-retry-warning', el => el.textContent.includes('may repeat an action')), true);
    assert.equal(await recoveryPage.$eval('[data-inbox-id="mock_recovery"] .inbox-archived', el => el.textContent), 'In archived room');
    await recoveryPage.click('[data-inbox-id="mock_recovery"] .inbox-decisions button:nth-child(2)');
    await recoveryPage.waitForFunction(() => document.querySelector('#inbox-count')?.textContent === '1');
    await domClick(recoveryPage, '[data-inbox-id="mock_budget"] .inbox-decisions button:first-child');
    await recoveryPage.waitForFunction(() => document.querySelector('.inbox-empty')?.textContent === 'All caught up');
    const boundaryPage = await browser.newPage();
    await login(boundaryPage, `${origin}/?mock_boundary=1`);
    await domClick(boundaryPage, '#inbox-toggle');
    assert.deepEqual(await boundaryPage.$$eval('[data-inbox-id="mock_boundary"] .inbox-decisions button', els => els.map(el => el.textContent)), ['Approve', 'Deny']);
    assert.equal(await boundaryPage.$eval('[data-inbox-id="mock_boundary"] .inbox-evidence', el => el.textContent.includes('/work/project') && el.textContent.includes('lead')), true);
    await domClick(boundaryPage, '[data-inbox-id="mock_boundary"] .inbox-decisions button:first-child');
    await boundaryPage.waitForFunction(() => document.querySelector('.inbox-empty')?.textContent === 'All caught up');
    await boundaryPage.close();
    const readonlyContext = await browser.createBrowserContext(), readonlyPage = await readonlyContext.newPage();
    await login(readonlyPage, `${origin}/?mock_readonly_recovery=1`);
    await domClick(readonlyPage, '#inbox-toggle');
    assert.equal(await readonlyPage.$$eval('[data-inbox-id="mock_recovery"] .inbox-decisions button', els => els.length), 0);
    assert.equal(await readonlyPage.$eval('[data-inbox-id="mock_recovery"]', el => el.textContent.includes('owner or operator must decide')), true);
    const staleContext = await browser.createBrowserContext(), stalePage = await staleContext.newPage();
    await login(stalePage, `${origin}/?mock_recovery=1&mock_stale=1`);
    await stalePage.waitForFunction(() => document.querySelector('#inbox-count')?.textContent === '2');
    await domClick(stalePage, '#inbox-toggle');
    await domClick(stalePage, '[data-inbox-id="mock_recovery"] .inbox-decisions button:first-child');
    await stalePage.waitForFunction(() => document.querySelector('#inbox-error')?.textContent === 'This item changed, reload item.');
    await stalePage.waitForFunction(() => document.querySelector('[data-inbox-id="mock_recovery"]')?.dataset.rev === '2');
    await domClick(stalePage, '[data-inbox-id="mock_recovery"] .inbox-decisions button:first-child');
    await stalePage.waitForFunction(() => document.querySelector('#inbox-count')?.textContent === '1');
    const waitingContext = await browser.createBrowserContext(), waitingPage = await waitingContext.newPage();
    await login(waitingPage, `${origin}/?mock_quiescence=1`);
    await waitingPage.waitForFunction(() => document.querySelector('#inbox-count')?.textContent === '1');
    await domClick(waitingPage, '#inbox-toggle');
    assert.deepEqual(await waitingPage.$$eval('.inbox-decisions button', els => els.map(el => el.textContent)), ['Override', 'Abandon']);
    assert.equal(await waitingPage.$eval('.inbox-retry-warning', el => el.textContent.includes('may still be running')), true);
    await domClick(waitingPage, '.inbox-decisions button:first-child');
    await waitingPage.waitForFunction(() => document.querySelector('.inbox-empty')?.textContent === 'All caught up');
    const agentContext = await browser.createBrowserContext(), agentPage = await agentContext.newPage();
    await agentPage.evaluateOnNewDocument(disableRandomUUID);
    await agentPage.setViewport({ width: 1100, height: 720 });
    await login(agentPage, `${origin}/?mock_agents=1&mock_inbox=1&mock_held=1`);
    // Agent profile (v1): opening a member replaces the list with that agent; to act on another one, close it first.
    const agentAct = async (id, cls) => {
      const open = await agentPage.$eval('#agents-panel', (panel, agent) => panel.classList.contains('profile-open') && !panel.querySelector(`[data-agent-id="${agent}"].expanded`), id);
      if (open) await domClick(agentPage, '#agent-profile-close');
      if (!(await agentPage.$(`[data-agent-id="${id}"].expanded`))) await domClick(agentPage, `[data-agent-id="${id}"] .agent-name`);
      await domClick(agentPage, `[data-agent-id="${id}"] .${{ 'agent-stop': 'agent-act-stop', 'agent-pause': 'agent-act-pause' }[cls] || cls}`);
    };
    await agentPage.waitForFunction(() => document.querySelectorAll('.agent-card[data-agent-id]').length === 2);
    await agentPage.waitForFunction(() => document.querySelector('#health-count')?.textContent === '2' && !document.querySelector('#health-count')?.hidden);
    const popupItems = () => agentPage.$$eval('.mention-commands:not([hidden]) .quick-command strong', els => els.map(el => el.textContent));
    await agentPage.focus('#composer textarea'); await agentPage.keyboard.type('hi @');
    await agentPage.waitForFunction(() => document.querySelectorAll('.mention-commands:not([hidden]) .quick-command').length > 0);
    assert.deepEqual(await popupItems(), ['@everyone', '@all', '@channel', '@here', '@humans', '@ais', '@reviewer', '@worker', '@ada', '@bert', '@owner'], 'mention popup lists groups, roles, then members, project humans included');
    await agentPage.keyboard.type('a');
    assert.deepEqual(await popupItems(), ['@all', '@ais', '@ada']);
    await agentPage.keyboard.press('ArrowDown'); await agentPage.keyboard.press('ArrowUp'); await agentPage.keyboard.press('ArrowUp');
    assert.equal(await agentPage.$eval('.mention-commands .quick-command[aria-selected=true] strong', el => el.textContent), '@ada', 'arrow keys wrap the selection');
    await agentPage.keyboard.press('Escape');
    assert.equal(await agentPage.$eval('.mention-commands', el => el.hidden), true, 'Escape closes the mention popup');
    await agentPage.keyboard.type('d');
    await agentPage.keyboard.press('Tab');
    assert.equal(await agentPage.$eval('#composer textarea', el => el.value), 'hi @ada ', 'Tab inserts the mention and a space');
    await agentPage.keyboard.type('and @wor'); await agentPage.waitForFunction(() => !document.querySelector('.mention-commands').hidden);
    await agentPage.keyboard.press('Enter');
    assert.equal(await agentPage.$eval('#composer textarea', el => el.value), 'hi @ada and @worker ', 'Enter picks instead of sending while the popup is open');
    await agentPage.keyboard.type('\\@here `@bert` @nobody ');
    await agentPage.keyboard.press('Enter');
    await agentPage.waitForFunction(() => [...document.querySelectorAll('.message-body')].some(el => el.textContent.includes('@nobody')));
    assert.deepEqual(await agentPage.$$eval('.message:last-child .message-body .mention', els => els.map(el => el.textContent)), ['@ada', '@worker'], 'only live in-room mentions are highlighted');
    await agentPage.keyboard.type('x @ad there'); for (let i = 0; i < 6; i++) await agentPage.keyboard.press('ArrowLeft');
    await agentPage.waitForFunction(() => !document.querySelector('.mention-commands').hidden);
    await agentPage.keyboard.press('Enter');
    assert.equal(await agentPage.$eval('#composer textarea', el => el.value), 'x @ada there');
    assert.equal(await agentPage.$eval('.mention-commands', el => el.hidden), true, 'picking before a space must not reopen the popup');
    // Emoji: the ":" suggester, ":name:" auto-convert, the picker (search, react, composer button) and the remembered skin tone.
    {
      const clearDraft = () => agentPage.$eval('#composer textarea', el => { el.value = ''; el.dispatchEvent(new Event('input', { bubbles: true })); });
      const draftValue = () => agentPage.$eval('#composer textarea', el => el.value);
      await agentPage.evaluate(() => localStorage.removeItem('aimebu-emoji-skin-tone'));
      await agentPage.focus('#composer textarea'); await clearDraft();
      await agentPage.keyboard.type(':shru');
      await agentPage.waitForFunction(() => document.querySelector('.emoji-commands:not([hidden]) .quick-command'));
      assert.deepEqual(await agentPage.$$eval('.emoji-commands .quick-command', rows => rows.slice(0, 1).map(row => [row.querySelector('.emoji-glyph').textContent, row.querySelector('.emoji-code').textContent, row.querySelector('mark').textContent])), [['🤷', ':shrug:', 'shru']], 'the suggester shows the glyph, the :shortcode: and the matched part');
      await agentPage.keyboard.press('Enter');
      assert.equal(await draftValue(), '🤷 ', 'Enter inserts the glyph');
      assert.equal(await agentPage.$eval('.emoji-commands', el => el.hidden), true, 'picking closes the suggester');
      await clearDraft(); await agentPage.keyboard.type(':ro'); assert.equal(await agentPage.$eval('.emoji-commands', el => el.hidden), false, 'two letters open the list');
      await agentPage.keyboard.press('Escape'); assert.equal(await agentPage.$eval('.emoji-commands', el => el.hidden), true, 'Escape closes the list'); assert.equal(await draftValue(), ':ro', 'Escape leaves the text alone');
      await clearDraft(); await agentPage.keyboard.type(':r'); assert.equal(await agentPage.$eval('.emoji-commands', el => el.hidden), true, 'one letter does not open the list');
      await clearDraft(); await agentPage.keyboard.type('ok :shrug:');
      assert.equal(await draftValue(), 'ok 🤷', 'a full :shrug: converts in place');
      await clearDraft(); await agentPage.keyboard.type('at 10:30 and 10:30:15 http://x.test:shrug: a:shrug: ');
      assert.equal(await draftValue(), 'at 10:30 and 10:30:15 http://x.test:shrug: a:shrug: ', 'times, URLs and colons inside a word are left alone');
      await clearDraft(); await agentPage.keyboard.type('`:shrug:'); assert.equal(await draftValue(), '`:shrug:', 'an inline code span is left alone');
      assert.equal(await agentPage.$eval('.emoji-commands', el => el.hidden), true, 'no list in a code span');
      await clearDraft(); await agentPage.keyboard.type('```'); await agentPage.keyboard.down('Shift'); await agentPage.keyboard.press('Enter'); await agentPage.keyboard.up('Shift'); await agentPage.keyboard.type(':shrug:');
      assert.equal(await draftValue(), '```\n:shrug:', 'a fenced block is left alone');
      await clearDraft(); await agentPage.keyboard.type('`a` :shrug:'); assert.equal(await draftValue(), '`a` 🤷', 'after a closed code span it converts again');
      await clearDraft();
      // Picker from the toolbar: search, react.
      await agentPage.hover('.message:last-child');
      await domClick(agentPage, '.message:last-child .message-find');
      await agentPage.waitForSelector('.emoji-picker .emoji-cell');
      assert.equal(await agentPage.evaluate(() => document.activeElement?.classList.contains('emoji-search')), true, 'the search box is focused on open');
      assert.deepEqual(await agentPage.$$eval('.emoji-tab', tabs => tabs.map(tab => tab.getAttribute('aria-label'))), ['People', 'Nature', 'Food', 'Travel', 'Activities', 'Objects', 'Symbols', 'Flags']);
      await agentPage.keyboard.type('rocket');
      await agentPage.waitForFunction(() => document.querySelector('.emoji-picker .emoji-cell')?.getAttribute('aria-label') === 'rocket');
      await agentPage.hover('.emoji-picker .emoji-cell');
      assert.equal(await agentPage.$eval('.emoji-preview-code', el => el.textContent), ':rocket:', 'the footer previews the hovered emoji');
      await agentPage.click('.emoji-picker .emoji-cell');
      await agentPage.waitForFunction(() => !document.querySelector('.emoji-picker'));
      await agentPage.waitForFunction(() => [...document.querySelectorAll('.message:last-child .reaction-pill')].some(pill => pill.textContent === '🚀 1'));
      // Picker from the composer button: Escape, keyboard navigation, tone persistence.
      await agentPage.click('#emoji-button'); await agentPage.waitForSelector('.emoji-picker .emoji-cell');
      assert.equal(await agentPage.$$eval('.emoji-cell', cells => cells.length > 1800), true, 'the whole database is in the grid');
      await agentPage.keyboard.press('Escape');
      assert.equal(await agentPage.$('.emoji-picker'), null, 'Escape closes the picker'); assert.equal(await agentPage.evaluate(() => document.activeElement?.id), 'emoji-button', 'focus returns to the button');
      await agentPage.click('#emoji-button'); await agentPage.waitForSelector('.emoji-picker .emoji-cell');
      await agentPage.keyboard.press('ArrowDown');
      assert.equal(await agentPage.evaluate(() => document.activeElement?.classList.contains('emoji-cell')), true, 'ArrowDown from the search box enters the grid');
      const firstLabel = await agentPage.evaluate(() => document.activeElement.getAttribute('aria-label'));
      await agentPage.keyboard.press('ArrowRight'); assert.notEqual(await agentPage.evaluate(() => document.activeElement.getAttribute('aria-label')), firstLabel, 'ArrowRight moves along the row');
      await agentPage.keyboard.press('ArrowDown');
      assert.equal(await agentPage.evaluate(() => document.activeElement.getBoundingClientRect().top > document.querySelector('.emoji-cell').getBoundingClientRect().top), true, 'ArrowDown moves to the next row');
      await agentPage.click('.emoji-tab[aria-label=Flags]');
      assert.equal(await agentPage.$eval('.emoji-tab[aria-label=Flags]', el => el.getAttribute('aria-current')), 'true', 'a category tab scrolls its section into view');
      await agentPage.click('.emoji-tone[aria-label="Medium skin tone"]');
      assert.equal(await agentPage.evaluate(() => localStorage.getItem('aimebu-emoji-skin-tone')), '\u{1F3FD}', 'the skin tone is saved');
      await agentPage.focus('.emoji-search'); await agentPage.keyboard.type('thumbsup');
      await agentPage.waitForFunction(() => document.querySelector('.emoji-picker .emoji-cell')?.firstChild.textContent === '👍🏽');
      await agentPage.keyboard.press('Escape'); await agentPage.click('#emoji-button'); await agentPage.waitForSelector('.emoji-picker .emoji-cell');
      assert.equal(await agentPage.$eval('.emoji-tone[aria-label="Medium skin tone"]', el => el.getAttribute('aria-pressed')), 'true', 'a reopened picker remembers the tone');
      await agentPage.keyboard.type('thumbsup'); await agentPage.waitForFunction(() => document.querySelector('.emoji-picker .emoji-cell')?.firstChild.textContent === '👍🏽');
      await agentPage.keyboard.press('Enter');
      await agentPage.waitForFunction(() => !document.querySelector('.emoji-picker'));
      assert.equal(await draftValue(), '👍🏽', 'a pick from the composer button lands in the draft'); assert.equal(await agentPage.evaluate(() => document.activeElement?.tagName), 'TEXTAREA', 'and focus returns to the draft');
      await clearDraft(); await agentPage.keyboard.type('good :+1: '); assert.equal(await draftValue(), 'good 👍🏽 ', 'the saved tone also applies to :name: conversion');
      await agentPage.evaluate(() => localStorage.removeItem('aimebu-emoji-skin-tone')); await clearDraft();
      // Cold cache: ":shrug:" typed while the database is still loading converts when it arrives, unless the draft moved on.
      const roomHash = await agentPage.evaluate(() => location.hash);
      for (const [typed, after, expected] of [['ok :shrug:', '', 'ok 🤷'], ['ok :shrug:', 'x', 'ok :shrug:x']]) {
        const cold = await agentContext.newPage(); await cold.setViewport({ width: 1100, height: 720 });
        let release; const held = new Promise(resolve => { release = resolve; }); let requested = 0;
        await cold.setRequestInterception(true);
        cold.on('request', request => { if (request.url().endsWith('/vendor/gemoji/emoji.json')) { requested++; held.then(() => request.continue()); } else request.continue(); });
        await cold.goto(`${origin}/?mock_agents=1&mock_inbox=1&mock_held=1${roomHash}`); await appReady(cold);
        await cold.waitForSelector('#composer textarea:not([disabled])');
        await cold.focus('#composer textarea'); await cold.keyboard.type(typed);
        assert.equal(await cold.$eval('#composer textarea', el => el.value), typed, 'nothing converts while the data is loading'); assert.ok(requested >= 1, 'the data was requested');
        if (after) await cold.keyboard.type(after);
        release();
        if (expected !== typed + after) await cold.waitForFunction(value => document.querySelector('#composer textarea').value === value, {}, expected);
        else { await new Promise(resolve => setTimeout(resolve, 600)); assert.equal(await cold.$eval('#composer textarea', el => el.value), expected, 'a draft that moved on is not rewritten'); }
        await cold.close(); await agentPage.bringToFront();
      }
    }
    // @mentions of an agent that wrote in the room but is no longer a member (departed, or imported) are highlighted too.
    {
      const marked = await agentPage.evaluate(async () => {
        const { MentionIndex, highlightMentions } = await import('/js/mentions.js');
        const host = document.createElement('div'); host.innerHTML = '<p>hi @nina and @nina@other-proj and @ghost and @ada</p>'; document.body.append(host);
        highlightMentions(host, new MentionIndex([{ slug: 'ada', role_key: 'worker' }], 'proj-1', [], ['nina@other-proj', 'nina']));
        const out = [...host.querySelectorAll('.mention')].map(node => node.textContent); host.remove(); return out;
      });
      assert.deepEqual(marked, ['@nina', '@nina@other-proj', '@ada'], 'a historical agent slug is highlighted, an unknown name is not');
    }
    // Small parity fixes: timestamps, the owner icon in Light, a thread code block, the attachment caption.
    {
      const report = await agentPage.evaluate(async () => {
        const { renderMarkdown } = await import('/js/markdown.js');
        const out = {};
        // Raw ISO timestamps with nanoseconds never reach the page: the formatter gives relative text, the tooltip the full date.
        const { relativeTime } = await import('/js/time.js'); out.relative = relativeTime('2026-10-18T22:25:41.123456789Z');
        // The human icon is dark in the light themes (it is white, so it would vanish on a light card).
        document.documentElement.dataset.theme = 'light';
        const probe = document.createElement('img'); probe.className = 'agent-harness-icon'; probe.src = '/icons/human.svg'; document.body.append(probe);
        await new Promise(resolve => setTimeout(resolve, 100)); out.humanFilter = getComputedStyle(probe).filter; probe.remove(); delete document.documentElement.dataset.theme;
        // A long code line in a thread entry scrolls inside the block; the block stays inside the panel.
        const panel = document.querySelector('#thread-panel'), wasHidden = panel.hidden; panel.hidden = false;
        const entry = document.createElement('article'); entry.className = 'thread-entry'; const body = document.createElement('div'); body.className = 'message-body'; body.append(renderMarkdown('```\n' + 'x'.repeat(400) + '\n```')); entry.append(body); document.querySelector('#thread-messages').append(entry);
        await new Promise(resolve => setTimeout(resolve, 150));
        out.threadInside = entry.querySelector('.md-codeblock').getBoundingClientRect().right <= panel.getBoundingClientRect().right + 0.5 && entry.querySelector('pre').scrollWidth > entry.querySelector('pre').clientWidth;
        entry.remove(); panel.hidden = wasHidden;
        return out;
      });
      assert.ok(!/\d{2}:\d{2}:\d{2}\.\d/.test(report.relative) && /ago|\d/.test(report.relative), `relative time: ${report.relative}`);
      assert.match(report.humanFilter, /invert/, 'the human icon is inverted (dark) in a light theme');
      assert.equal(report.threadInside, true, 'a code block in the thread panel scrolls inside it');
    }
    // The mention list also offers authors of the room's history that are no longer in it, last, marked as not in the room.
    {
      const offered = await agentPage.evaluate(async () => {
        const { MentionIndex } = await import('/js/mentions.js');
        const index = new MentionIndex([{ slug: 'ada', role_key: 'worker', tool: 'codex', model: 'm' }], 'proj-1', ['bert'], ['nina@other-proj', 'nina', 'ada']);
        return { all: index.candidates('').slice(-3).map(item => `${item.insertText}|${item.preview}`), filtered: index.candidates('ni').map(item => item.insertText) };
      });
      assert.deepEqual(offered.all.slice(-2), ['@bert|human', '@nina|not in room'], 'a historical author is offered last, once, as not in the room (a current member is not repeated)');
      assert.deepEqual(offered.filtered, ['@nina'], 'the offer follows the prefix');
      // An agent is listed with its project, dim, after its name; a person is not; the inserted text is the name alone.
      const projectRow = await agentPage.evaluate(async () => {
        // The server versions every module URL, so import the same instances the app runs (an unversioned URL is a second copy).
        const version = new URL(document.querySelector('script[type=module][src]').src).search;
        const { MentionIndex } = await import(`/js/mentions.js${version}`), { setProjectNames } = await import(`/js/sender-identity.js${version}`);
        setProjectNames(id => id === 'p1' ? 'demo' : '');
        const index = new MentionIndex([{ slug: 'ada', project_id: 'p1', role_key: 'worker' }, { slug: 'zed', role_key: 'worker' }], 'p1', ['bert'], []);
        const items = index.candidates('').filter(item => ['@ada', '@zed', '@bert'].includes(item.insertText)).map(item => `${item.insertText}|${item.project}`);
        setProjectNames(() => ''); return items;
      });
      assert.deepEqual(projectRow, ['@ada|demo', '@bert|undefined', '@zed|'], 'an agent carries its project, a person has none, an agent with an unknown project has an empty one');
    }
    // On a phone the bottom tab bar replaces the header's side-panel expand buttons.
    {
      const phone = await agentContext.newPage(); await phone.emulate({ viewport: { width: 390, height: 780, isMobile: true, hasTouch: true, deviceScaleFactor: 2 }, userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148' });
      await phone.goto(`${origin}/?mock_agents=1&mock_inbox=1&mock_held=1${await agentPage.evaluate(() => location.hash)}`); await appReady(phone);
      await phone.evaluate(() => { document.querySelector('#app-view').classList.add('chat-open', 'rooms-collapsed'); document.querySelector('.chat').classList.add('agents-collapsed'); });
      assert.deepEqual(await phone.evaluate(() => ({ tabs: getComputedStyle(document.querySelector('.mobile-tabs')).display, expand: [...document.querySelectorAll('.chat > header > .edge-expand')].map(el => getComputedStyle(el).display) })), { tabs: 'flex', expand: ['none', 'none'] }, 'the header repeats the bottom bar on a phone');
      await phone.close(); await agentPage.bringToFront();
    }
    // The room list previews the last message as plain text: a fenced diff reads "[diff]", never raw Markdown.
    {
      const { previewText } = await agentPage.evaluate(async () => ({ previewText: (await import('/js/preview.js')).previewText('```diff\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n```') }));
      assert.equal(previewText, '[diff]', 'a diff previews as [diff]');
    }
    // Code in messages: highlight.js tokens, diff lines, raw text on Copy, inert markup, and a lazy load.
    {
      const report = await agentPage.evaluate(async () => {
        const { renderMarkdown } = await import('/js/markdown.js');
        const out = {}, copies = [];
        const original = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
        Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async value => { copies.push(value); } } });
        const render = source => { const host = document.createElement('div'); host.className = 'message-body'; host.append(renderMarkdown(source)); document.body.append(host); return host; };
        const settle = async (host, selector) => { for (let i = 0; i < 100 && !host.querySelector(selector); i++) await new Promise(resolve => setTimeout(resolve, 50)); };
        const copy = async host => { host.querySelector('.md-copy').click(); await new Promise(resolve => setTimeout(resolve, 30)); return copies.at(-1); };
        const goSource = 'package main\n\n// greet says hello\nfunc greet(name string) string { return "hi " + name }';
        const go = render('```golang\n' + goSource + '\n```'); await settle(go, '.hljs-keyword');
        out.go = { keyword: [...go.querySelectorAll('.hljs-keyword')].map(node => node.textContent).includes('func'), string: !!go.querySelector('.hljs-string'), comment: !!go.querySelector('.hljs-comment'), lang: go.querySelector('code').dataset.highlighted, text: go.querySelector('code').textContent === goSource, copied: (await copy(go)) === goSource };
        const diffSource = 'diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,3 @@\n package main\n-var x = 1\n+var x = 2\n';
        const diff = render('```diff\n' + diffSource + '```');
        const lines = [...diff.querySelectorAll('.hl-line')];
        out.diff = { kinds: lines.map(node => node.className.replace('hl-line hl-', '')).join(), text: diff.querySelector('code').textContent === diffSource.replace(/\n$/, ''), copied: (await copy(diff)) === diffSource.replace(/\n$/, ''), noLib: !diff.querySelector('.hljs-keyword') };
        const patch = render('```patch\n@@ -1 +1 @@\n-a\n+b\n```'); out.patch = [...patch.querySelectorAll('.hl-line')].map(node => node.className.replace('hl-line hl-', '')).join();
        const bare = render('```\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n```'); out.bareDiff = [...bare.querySelectorAll('.hl-line')].map(node => node.className.replace('hl-line hl-', '')).join();
        const list = render('```\n- one\n+ two\n```'); out.plainList = list.querySelectorAll('.hl-line, [class*=hljs]').length;
        const unknown = render('```nonsense\nfunc x() {}\n```'); const mermaid = render('```mermaid\ngraph TD; A-->B\n```'); await new Promise(resolve => setTimeout(resolve, 200));
        out.plain = [unknown, mermaid].map(host => host.querySelectorAll('code span').length);
        const huge = render('```go\n' + 'x := 1\n'.repeat(8000) + '```'); await new Promise(resolve => setTimeout(resolve, 300)); out.huge = huge.querySelectorAll('code span').length;
        window.__xss = undefined;
        const hostile = render('```html\n<script>window.__xss = 1</script><img src=x onerror="window.__xss = 2">\n```\n\n```diff\n@@ -1 +1 @@\n+<script>window.__xss = 3</script>\n```'); await settle(hostile, '.hljs-tag, .hljs-name'); await new Promise(resolve => setTimeout(resolve, 200));
        out.hostile = { elements: hostile.querySelectorAll('code script, code img, code iframe').length, flag: window.__xss === undefined, texts: [...hostile.querySelectorAll('code')].map(node => node.textContent), highlighted: !!hostile.querySelector('code .hljs-tag, code .hljs-name'), attributes: [...hostile.querySelectorAll('code *')].every(node => [...node.attributes].every(attribute => attribute.name === 'class')) };
        for (const host of document.querySelectorAll('body > .message-body')) host.remove();
        if (original) Object.defineProperty(navigator, 'clipboard', original); else delete navigator.clipboard;
        return out;
      });
      assert.deepEqual(report.go, { keyword: true, string: true, comment: true, lang: 'go', text: true, copied: true }, 'a golang block gets token spans, keeps its text and copies the raw source');
      assert.deepEqual(report.diff, { kinds: 'meta,file,file,hunk,ctx,del,add', text: true, copied: true, noLib: true }, 'a diff block gets per-line classes from our own renderer');
      assert.equal(report.patch, 'hunk,del,add'); assert.equal(report.bareDiff, 'file,file,hunk,del,add', 'an unlabelled unified diff is rendered as a diff'); assert.equal(report.plainList, 0, 'an unlabelled list of - and + lines is not a diff');
      assert.deepEqual(report.plain, [0, 0], 'unknown languages and mermaid blocks stay plain'); assert.equal(report.huge, 0, 'a huge block is not highlighted');
      assert.equal(report.hostile.elements, 0, 'markup in a code block stays inert'); assert.equal(report.hostile.flag, true, 'no script in a code block ran'); assert.equal(report.hostile.highlighted, true); assert.equal(report.hostile.attributes, true, 'only class attributes survive on highlighted spans');
      assert.deepEqual(report.hostile.texts, ['<script>window.__xss = 1</script><img src=x onerror="window.__xss = 2">', '@@ -1 +1 @@\n+<script>window.__xss = 3</script>']);
      // highlight.js is fetched only for a block that names a language, and then only that language's grammar.
      const lazy = await agentContext.newPage(); await lazy.setViewport({ width: 1100, height: 720 });
      const fetched = []; lazy.on('request', request => { if (request.url().includes('/vendor/highlight/')) fetched.push(new URL(request.url()).pathname.replace('/vendor/highlight/', '')); });
      await lazy.goto(`${origin}/?mock_agents=1&mock_inbox=1&mock_held=1`); await appReady(lazy);
      await lazy.evaluate(async () => { const { renderMarkdown } = await import('/js/markdown.js'); const host = document.createElement('div'); host.append(renderMarkdown('```\nplain\n```\n\n```diff\n-a\n+b\n```\n\n```nonsense\nx\n```')); document.body.append(host); await new Promise(resolve => setTimeout(resolve, 400)); });
      assert.deepEqual(fetched, [], 'plain, diff and unknown blocks load no highlight.js');
      await lazy.evaluate(async () => { const { renderMarkdown } = await import('/js/markdown.js'); const host = document.createElement('div'); host.append(renderMarkdown('```py\nprint(1)\n```')); document.body.append(host); for (let i = 0; i < 100 && !host.querySelector('.hljs-built_in, .hljs-title'); i++) await new Promise(resolve => setTimeout(resolve, 50)); });
      assert.deepEqual([...fetched].sort(), ['core.js', 'languages/python.js'], 'a python block loads the core and the python grammar only');
      await lazy.close(); await agentPage.bringToFront();
    }
    // Touch (an iPad mini upright): the message actions are hidden until a tap selects a message, as in v1.
    {
      const roomHash = await agentPage.evaluate(() => location.hash);
      const touch = await agentContext.newPage();
      await touch.emulate({ viewport: { width: 744, height: 1000, isMobile: true, hasTouch: true, deviceScaleFactor: 2 }, userAgent: 'Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148' });
      await touch.evaluateOnNewDocument(() => { try { localStorage.setItem('aimebu2.composer.formatting', 'on'); } catch {} });
      await touch.goto(`${origin}/?mock_agents=1&mock_inbox=1&mock_held=1${roomHash}`); await appReady(touch);
      await touch.waitForFunction(() => document.querySelectorAll('#messages .message:not(.system-notice)').length >= 2);
      assert.equal(await touch.evaluate(() => matchMedia('(hover: none) and (pointer: coarse)').matches), true, 'the page emulates a touch screen');
      const state = () => touch.evaluate(() => { const rows = [...document.querySelectorAll('#messages .message:not(.system-notice)')]; return rows.map(row => [row.classList.contains('actions-open'), getComputedStyle(row.querySelector('.message-actions')).opacity]); });
      const settle = () => new Promise(resolve => setTimeout(resolve, 350)); // past the opacity transition
      // Taps land on the empty right end of a line, never on a mention or a link.
      const tapMessage = async index => { const point = await touch.evaluate(index => { const row = [...document.querySelectorAll('#messages .message:not(.system-notice)')][index]; row.scrollIntoView({ block: 'center' }); const box = (row.querySelector('.message-body') || row).getBoundingClientRect(); return { x: box.right - 6, y: box.y + box.height / 2 }; }, index); await touch.touchscreen.tap(point.x, point.y); await settle(); };
      await settle();
      assert.deepEqual((await state()).slice(0, 2), [[false, '0'], [false, '0']], 'on touch the actions are hidden by default');
      await tapMessage(0);
      assert.deepEqual((await state()).slice(0, 2), [[true, '1'], [false, '0']], 'a tap on a message shows its actions');
      await tapMessage(1);
      assert.deepEqual((await state()).slice(0, 2), [[false, '0'], [true, '1']], 'a tap on another message moves them');
      await touch.evaluate(() => document.querySelector('#messages .message.actions-open .message-react')?.click());
      assert.equal(await touch.evaluate(() => document.querySelectorAll('#messages .message.actions-open').length), 1, 'using an action does not hide it'); await touch.keyboard.press('Escape');
      await touch.touchscreen.tap(...await touch.evaluate(() => { const box = document.querySelector('#room-title').getBoundingClientRect(); return [box.x + 5, box.y + box.height / 2]; })); await settle();
      assert.deepEqual((await state()).slice(0, 2), [[false, '0'], [false, '0']], 'a tap on empty space hides them');
      await tapMessage(0); assert.equal((await state())[0][0], true);
      await touch.keyboard.press('Escape'); await settle();
      assert.deepEqual((await state()).slice(0, 2), [[false, '0'], [false, '0']], 'Escape hides them');
      // The idle composer is one row; the toolbar appears with focus and wraps instead of being cut off; the room title keeps a width.
      const bar = () => touch.evaluate(() => { const t = document.querySelector('#composer .composer-toolbar'), tools = document.querySelector('#composer .composer-tools'), title = document.querySelector('#room-title').getBoundingClientRect(); return { shown: !!t && getComputedStyle(t).display !== 'none' && t.getClientRects().length > 0, oneRow: tools ? tools.getBoundingClientRect().height <= 44 : null, scrolls: tools ? getComputedStyle(tools).overflowX === 'auto' : null, titleWidth: Math.round(title.width), controls: getComputedStyle(document.querySelector('.room-controls')).display }; });
      await touch.evaluate(() => document.activeElement?.blur()); await settle();
      assert.equal((await bar()).shown, false, 'the idle composer on a touch tablet is one row, without the toolbar');
      await touch.focus('#composer .composer-editor .ProseMirror, #composer textarea:not([style*="none"])').catch(() => {});
      await touch.evaluate(() => (document.querySelector('#composer .ProseMirror') || document.querySelector('#composer textarea')).focus());
      await settle();
      const focused = await bar();
      assert.deepEqual([focused.shown, focused.oneRow, focused.scrolls], [true, true, true], `the toolbar appears with focus as one row that scrolls: ${JSON.stringify(focused)}`);
      assert.ok(focused.titleWidth >= 40 && focused.controls === 'none', `the room title is squeezed or the controls are still crowding it: ${JSON.stringify(focused)}`);
      await touch.evaluate(() => document.activeElement?.blur());
      // A tablet has room for one side panel next to the chat: the chat column keeps at least 400px, the members panel starts closed and
      // opening one panel closes the other; the toolbar stays one row.
      for (const [width, height] of [[744, 1133], [820, 1180]]) {
        await touch.setViewport({ width, height, isMobile: true, hasTouch: true, deviceScaleFactor: 2 }); await touch.reload(); await appReady(touch);
        await touch.waitForFunction(() => document.querySelectorAll('#messages .message').length > 0); await settle();
        const column = () => touch.evaluate(() => ({ chat: Math.round(document.querySelector('#messages').getBoundingClientRect().width), rooms: !document.querySelector('#app-view').classList.contains('rooms-collapsed'), members: !document.querySelector('.chat').classList.contains('agents-collapsed') }));
        const start = await column();
        assert.ok(start.chat >= 400 && start.rooms && !start.members, `${width}: the chat column keeps its room and the members panel starts closed: ${JSON.stringify(start)}`);
        await touch.evaluate(() => document.querySelector('#edge-agents-expand').click()); await settle();
        const opened = await column(); assert.ok(opened.members && !opened.rooms, `${width}: opening the members panel closes the rooms: ${JSON.stringify(opened)}`);
        await touch.evaluate(() => document.querySelector('#edge-rooms-expand').click()); await settle();
        const back = await column(); assert.ok(back.rooms && !back.members, `${width}: opening the rooms closes the members panel: ${JSON.stringify(back)}`);
        await touch.evaluate(() => (document.querySelector('#composer .ProseMirror') || document.querySelector('#composer textarea')).focus()); await settle();
        const rows = await bar(); assert.deepEqual([rows.shown, rows.oneRow], [true, true], `${width}: the toolbar is one row: ${JSON.stringify(rows)}`);
        await touch.evaluate(() => document.activeElement?.blur());
      }
      await touch.close(); await agentPage.bringToFront();
    }
    await agentPage.$eval('#composer textarea', el => { el.value = ''; el.dispatchEvent(new Event('input', { bubbles: true })); });
    await domClick(agentPage, '.message:last-child .thread-action');
    await agentPage.waitForFunction(() => !document.querySelector('#thread-panel').hidden);
    await agentPage.focus('#thread-composer textarea'); await agentPage.keyboard.type('thanks @a');
    await agentPage.waitForFunction(() => { const popup = document.querySelector('#thread-composer .mention-commands'); return popup && !popup.hidden; });
    const threadPopup = await agentPage.evaluate(() => {
      const popup = document.querySelector('#thread-composer .mention-commands').getBoundingClientRect(), field = document.querySelector('#thread-composer').getBoundingClientRect(), panel = document.querySelector('#thread-panel').getBoundingClientRect();
      return { aboveField: popup.bottom <= field.top + 1 && field.top - popup.bottom < 40, inPanel: popup.top >= panel.top && popup.left >= panel.left - 1 && popup.right <= panel.right + 1 };
    });
    assert.deepEqual(threadPopup, { aboveField: true, inPanel: true }, 'thread mention popup anchors above the thread composer');
    // The reply can show up in the thread from the live stream before the POST that created it answers, and the
    // composer is only cleared when that answer arrives. Hold the answer back so the test sees that order on every run.
    await agentPage.evaluate(() => {
      window.__realFetch = window.fetch;
      window.fetch = (input, options) => {
        const response = window.__realFetch(input, options);
        return options?.method === 'POST' && /\/messages$/.test(String(input)) && /thread_root/.test(String(options.body || '')) ? response.then(answer => new Promise(resolve => setTimeout(() => resolve(answer), 700))) : response;
      };
    });
    await agentPage.keyboard.press('Escape');
    await agentPage.keyboard.press('Enter');
    await agentPage.waitForFunction(() => [...document.querySelectorAll('#thread-messages .message-body')].some(el => el.textContent.includes('thanks @a')));
    await agentPage.waitForFunction(() => document.querySelector('#thread-composer textarea').value === '', { timeout: 10000 }).catch(async error => { throw new Error(`Enter did not send the thread reply once the popup was closed (composer still holds ${JSON.stringify(await agentPage.$eval('#thread-composer textarea', el => el.value))}): ${error.message}`); });
    await agentPage.evaluate(() => { window.fetch = window.__realFetch; });
    await domClick(agentPage, '#close-thread');
    const mentionRules = await agentPage.evaluate(async () => {
      const { MentionIndex, highlightMentions } = await import('/js/mentions.js');
      const { renderMarkdown } = await import('/js/markdown.js');
      const cards = [{ slug: 'sam', role_key: 'worker', model: 'm', tool: 'codex' }, { slug: 'sam', role_key: 'reviewer', model: 'm', tool: 'pi' }, { slug: 'zed', role_key: 'sam', tool: 'x' }, { slug: 'kim', role_key: 'leader' }];
      const index = new MentionIndex(cards, 'proj-1');
      const host = document.createElement('div');
      host.append(renderMarkdown('@sam @sam@proj-1 @kim @leader @reviewer @here @HERE, @zed-\n\n```\n@kim\n```\n\n[@kim](https://x.test) mail a@kim\n\n    @kim'));
      highlightMentions(host, index); highlightMentions(host, index);
      return { marked: [...host.querySelectorAll('.mention')].map(el => el.textContent), groups: host.querySelectorAll('.mention-group').length, code: host.querySelectorAll('pre .mention, a .mention').length, candidates: index.candidates('s').map(item => item.insertText), roles: index.candidates('').map(item => item.insertText).filter(text => ['@sam', '@leader', '@reviewer'].includes(text)) };
    });
    assert.deepEqual(mentionRules.marked, ['@sam@proj-1', '@kim', '@leader', '@reviewer', '@here', '@HERE', '@zed', '@kim'], 'collisions and slug-vs-role precedence follow server addressing');
    assert.equal(mentionRules.groups, 2); assert.equal(mentionRules.code, 0);
    assert.deepEqual(mentionRules.candidates, ['@sam@proj-1', '@sam@proj-1'], 'colliding slugs insert the full form');
    assert.deepEqual(mentionRules.roles, ['@leader', '@reviewer'], 'a role key that is also a slug is not offered as a role');
    const humans = await agentPage.evaluate(async () => {
      const { MentionIndex, highlightMentions } = await import('/js/mentions.js');
      const index = new MentionIndex([{ slug: 'ada', role_key: 'reviewer', model: 'm', tool: 'pi' }, { slug: 'kim', role_key: 'leader', role_deleted: true }], 'proj-1', ['reviewer', 'kim', 'casey']);
      const host = document.createElement('div'); host.textContent = '@reviewer @kim @casey @ada @leader';
      highlightMentions(host, index);
      return { marked: [...host.querySelectorAll('.mention')].map(el => el.textContent), offered: index.candidates('').map(item => item.insertText).slice(6) };
    });
    assert.deepEqual(humans.marked, ['@reviewer', '@casey', '@ada', '@leader'], 'human logins share the slug namespace; deleted roles still resolve');
    assert.deepEqual(humans.offered, ['@leader', '@ada', '@casey', '@kim@proj-1', '@reviewer'], 'a login equal to a role key is offered as the human; a colliding login is not offered');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-health', el => el.textContent.includes('Not responding')), true);
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_reviewer"] .agent-health', el => el.textContent.includes('Needs your decision')), true);
    assert.equal(await agentPage.$eval('.room-row.active .room-health', el => el.textContent), '⚠ 2');
    await domClick(agentPage, '#health-toggle');
    assert.deepEqual(await agentPage.$$eval('#health-popover .health-item .health-item-title', els => els.map(el => el.textContent)), ['Ada looks stuck', 'Bert needs a recovery decision', 'An answer to Bert could not be delivered'], 'health items are plain sentences, one per problem');
    // Each entry says what the problem means, in a line of its own, and none of them shows an internal state name.
    assert.deepEqual(await agentPage.$$eval('#health-popover .health-item .health-item-hint', els => els.map(el => el.textContent.length > 20 && /[a-z]/.test(el.textContent))), [true, true, true], 'each health entry has a one-line explanation');
    assert.equal(await agentPage.$eval('#health-popover', el => !/held_answer|recovery_waiting|Held answer|Recovery waiting/.test(el.textContent)), true, 'the health list shows internal state names');
    await domClick(agentPage, '#health-toggle');
    await domClick(agentPage, '#health-toggle');
    assert.equal(await agentPage.$eval('#health-toggle', el => el.getAttribute('aria-expanded')), 'true');
    await agentPage.keyboard.press('Escape');
    assert.equal(await agentPage.$eval('#health-popover', el => el.hidden), true, 'Escape closes the health popover');
    assert.equal(await agentPage.evaluate(() => document.activeElement?.id), 'health-toggle', 'focus returns to the health toggle');
    assert.equal(await agentPage.$eval('#health-toggle', el => el.getAttribute('aria-expanded')), 'false');
    // Disk usage heads the health panel; Clean now shows what it would remove first and removes only on the second click.
    {
      const blobDir = path.join(tmp, 'server', 'attachments'); fs.mkdirSync(blobDir, { recursive: true });
      const orphanFile = path.join(blobDir, 'smoke-orphan.bin'); fs.writeFileSync(orphanFile, 'x'.repeat(2048));
      const longAgo = new Date(Date.now() - 6 * 3600 * 1000); fs.utimesSync(orphanFile, longAgo, longAgo);
      await domClick(agentPage, '#health-toggle');
      await agentPage.waitForSelector('#health-disk:not([hidden]) .health-disk-total');
      assert.equal(await agentPage.$eval('#health-popover', el => el.firstElementChild.id), 'health-disk', 'disk usage comes first in the health panel');
      assert.match(await agentPage.$eval('#health-disk .health-disk-total', el => el.textContent), /\d/, 'the total is a size');
      await domClick(agentPage, '#health-disk .health-disk-clean');
      await agentPage.waitForSelector('#health-disk .health-disk-plan:not([hidden]) li[data-action="would_remove"]');
      assert.equal(fs.existsSync(orphanFile), true, 'previewing the cleanup must not remove anything');
      assert.match(await agentPage.$eval('#health-disk .health-disk-summary', el => el.textContent), /Nothing has been removed yet/);
      await domClick(agentPage, '#health-disk .health-disk-plan .btn-primary');
      await agentPage.waitForFunction(() => /Removed 1 item/.test(document.querySelector('#health-disk .health-disk-plan')?.textContent || ''));
      assert.equal(fs.existsSync(orphanFile), false, 'the confirmed cleanup removes the orphan upload');
      // Attachment retention: 90 days by default, saved when changed; a message whose attachment was removed says so in its place.
      assert.equal(await agentPage.$eval('#cleanup-attachment-days', input => input.value), '90', 'attachments are kept 90 days by default');
      await agentPage.$eval('#cleanup-attachment-days', input => { input.value = '45'; input.dispatchEvent(new Event('change', { bubbles: true })); });
      await agentPage.waitForFunction(async () => (await (await fetch('/api/cleanup/settings')).json()).attachment_days === 45);
      await agentPage.$eval('#cleanup-attachment-days', input => { input.value = '90'; input.dispatchEvent(new Event('change', { bubbles: true })); });
      await agentPage.waitForFunction(async () => (await (await fetch('/api/cleanup/settings')).json()).attachment_days === 90);
      assert.deepEqual(await agentPage.evaluate(async () => {
        const { Chat } = await import('/js/chat.js'); const holder = document.createElement('div');
        Chat.prototype.renderAttachments.call({}, holder, [{ id: 'att_x', name: 'shot.png', mime: 'image/png', url: '/api/attachments/att_x', removed_at: '2030-01-01T00:00:00Z', removed_after_days: 90 }]);
        return [holder.querySelector('.attachment-removed')?.textContent, !!holder.querySelector('img')];
      }), ['shot.png: attachment removed after 90 days', false], 'a removed attachment shows a note instead of the image');
      await agentPage.keyboard.press('Escape');
    }
    // An entry with no Inbox item behind it opens the agent: its room, then its profile.
    await domClick(agentPage, '#health-toggle');
    await agentPage.click('#health-popover .health-item[data-code="stuck"]');
    await agentPage.waitForFunction(() => document.querySelector('#agents-panel').classList.contains('profile-open') && document.querySelector('#agents-panel header h2').textContent === 'Agent profile');
    assert.equal(await agentPage.$eval('#health-popover', el => el.hidden), true, 'choosing a health entry closes the list');
    await agentPage.keyboard.press('Escape');
    await agentPage.waitForFunction(() => !document.querySelector('#agents-panel').classList.contains('profile-open'));
    await agentPage.evaluate(() => document.activeElement?.blur()); // closing returns focus to the row; the member-row steps below start from rest
    const staleRead = await agentPage.evaluate(async () => {
      const { HttpAgentSource } = await import('/js/agent-source.js');
      const source = new HttpAgentSource();
      source.agents.set('agt_stale', { id: 'agt_stale', room_id: 'room_stale', desired: 'active', observed: 'idle', desired_rev: 2, generation: 1 });
      const original = window.fetch;
      window.fetch = () => Promise.resolve(new Response(JSON.stringify([{ id: 'agt_stale', room_id: 'room_stale', desired: 'stopped', observed: 'stopped', desired_rev: 1, generation: 0 }]), { status: 200, headers: { 'Content-Type': 'application/json' } }));
      try { return (await source.list('room_stale'))[0]; } finally { window.fetch = original; }
    });
    assert.equal(staleRead.desired, 'active', 'an older room read regressed a control response');
    assert.equal(staleRead.generation, 1);
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-state-label', el => el.textContent), 'working');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-harness-icon', el => el.getAttribute('src')), '/icons/codex.svg');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-harness-icon', el => el.title), 'fake-model · Codex');
    // Every member card carries its harness icon, whatever the tool, between the presence dot and the name, centred with it.
    for (const [id, src] of [['agt_mock_worker', '/icons/codex.svg'], ['agt_mock_reviewer', '/icons/unknown.svg']]) {
      const geometry = await agentPage.$eval(`[data-agent-id="${id}"]`, card => {
        const box = selector => { const node = card.querySelector(selector); const rect = node.getBoundingClientRect(); return { left: rect.left, mid: rect.top + rect.height / 2, width: rect.width, height: rect.height, shown: getComputedStyle(node).display !== 'none' && getComputedStyle(node).visibility !== 'hidden' }; };
        return { src: card.querySelector('.agent-harness-icon').getAttribute('src'), dot: box('.agent-dot'), icon: box('.agent-harness-icon'), name: box('.agent-name') };
      });
      assert.equal(geometry.src, src, `${id} icon`);
      assert.ok(geometry.icon.shown && geometry.icon.width >= 12 && geometry.icon.height >= 12, `${id} has a visible harness icon: ${JSON.stringify(geometry.icon)}`);
      assert.ok(geometry.dot.left < geometry.icon.left && geometry.icon.left < geometry.name.left, `${id}: dot, icon, name in that order`);
      assert.ok(Math.abs(geometry.icon.mid - geometry.name.mid) <= 1, `${id}: icon and name share a centre line: ${geometry.icon.mid} vs ${geometry.name.mid}`);
    }
    // vibe and cline are white glyphs; on a light theme they must still read against the card, and stay white on a dark one.
    const whiteIcons = await agentPage.evaluate(async () => {
      const root = document.documentElement, saved = root.dataset.theme, out = {};
      const luminance = async img => {
        await img.decode(); const canvas = document.createElement('canvas'); canvas.width = canvas.height = 32; const context = canvas.getContext('2d');
        context.filter = getComputedStyle(img).filter; context.drawImage(img, 0, 0, 32, 32);
        const data = context.getImageData(0, 0, 32, 32).data; let sum = 0, count = 0;
        for (let i = 0; i < data.length; i += 4) if (data[i + 3] > 128) { sum += (data[i] + data[i + 1] + data[i + 2]) / 765; count++; }
        return count ? sum / count : -1;
      };
      const host = document.querySelector('.agent-card');
      for (const name of ['vibe', 'cline']) {
        const img = document.createElement('img'); img.className = 'agent-harness-icon'; img.src = `/icons/${name}.svg`; host.append(img);
        root.dataset.theme = 'light'; out[`${name}Light`] = await luminance(img);
        root.dataset.theme = 'blue-dark'; out[`${name}Dark`] = await luminance(img);
        img.remove();
      }
      if (saved === undefined) delete root.dataset.theme; else root.dataset.theme = saved;
      return out;
    });
    for (const name of ['vibe', 'cline']) {
      assert.ok(whiteIcons[`${name}Light`] >= 0 && whiteIcons[`${name}Light`] < 0.3, `${name} is dark enough to read on a light theme: ${JSON.stringify(whiteIcons)}`);
      assert.ok(whiteIcons[`${name}Dark`] > 0.8, `${name} stays white on a dark theme: ${JSON.stringify(whiteIcons)}`);
    }
    // A turn that began with another agent's reply says so on the card; any other turn does not.
    const wokenTag = await agentPage.evaluate(async () => {
      const { AgentsPanel } = await import('/js/agents.js');
      const panel = Object.create(AgentsPanel.prototype); panel.source = { available: true }; panel.room = { lifecycle: 'open' }; panel.control = () => {}; panel.openSwitch = () => {}; panel.onPrivate = () => {};
      const agent = { id: 'agt_tag', name: 'tag', slug: 'tag', role: 'worker', role_key: 'worker', tool: 'codex', model: 'm', desired: 'active', observed: 'in_turn', activity: 'tool inspect' };
      const card = panel.card(agent); document.body.append(card);
      const read = value => { panel.update(card, { ...agent, woken_by: value }); return card.querySelector('.agent-activity').textContent; };
      const out = { reply: read('reply'), none: read(''), other: read('mention') };
      card.remove(); return out;
    });
    assert.deepEqual(wokenTag, { reply: 'Woken by reply · tool inspect', none: 'tool inspect', other: 'tool inspect' }, 'a card tags a turn woken by a reply');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-live-chip', el => /^tool 0:\d\d$/.test(el.textContent)), true);
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-context-value', el => el.textContent), '75%');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-context', el => el.title.includes('150,000 / 200,000 tokens') && el.title.includes('Cost so far $0.35')), true);
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_reviewer"] .agent-context-value', el => el.textContent), '—');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-switch', el => el.disabled), false, 'active turn should allow a switch');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_reviewer"] .agent-state', el => el.title), 'Idle; waiting for new work.\nProcess exited with code 17.\nCheckout is not mounted.');
    // Room members (v1 style): a compact row (dot, emoji, name, DM / Pause / Stop); Switch… and Reactivate open with the row.
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-switch', el => el.offsetParent === null), true, 'the row is compact until opened');
    assert.deepEqual(await agentPage.$$eval('[data-agent-id="agt_mock_worker"] .agent-quick button', els => els.map(el => el.getAttribute('aria-label'))), ['Message Ada privately', 'Pause', 'Stop', 'Open agent profile'], 'DM, Pause, Stop (v1 order) and the detail toggle are icon buttons with accessible names');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-name', el => el.title === el.textContent && el.textContent.length > 0), true, 'the member name carries its full text as a title');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-dot', el => el.dataset.state), 'working');
    const cardParity = await agentPage.$eval('[data-agent-id="agt_mock_worker"]', card => {
      const name = getComputedStyle(card.querySelector('.agent-name'));
      const colors = {};
      for (const state of ['idle', 'working', 'paused', 'error', 'stopped']) {
        const dot = document.createElement('span'); dot.className = 'presence-dot'; dot.dataset.state = state; card.append(dot);
        colors[state] = getComputedStyle(dot).backgroundColor; dot.remove();
      }
      return { padding: getComputedStyle(card).padding, titleHeight: getComputedStyle(card.querySelector('.agent-title')).minHeight,
        name: [name.fontSize, name.lineHeight, name.fontWeight], colors };
    });
    assert.deepEqual(cardParity, { padding: '7px 8px', titleHeight: '17px', name: ['11.2px', '16.8px', '600'], colors: {
      idle: 'rgb(66, 186, 83)', working: 'rgb(66, 186, 83)', paused: 'rgb(210, 154, 35)', error: 'rgb(248, 81, 73)', stopped: 'rgb(248, 81, 73)',
    } }, 'compact member rows match v1 sizing and state colours');
    // One line per member: the whole name is readable, the state sits on the same line, and the buttons only show on hover.
    const rowShape = await agentPage.$eval('[data-agent-id="agt_mock_worker"]', card => { const name = card.querySelector('.agent-name'), state = card.querySelector('.agent-state'), quick = card.querySelector('.agent-quick'); return { whole: name.scrollWidth <= name.clientWidth, wide: name.clientWidth >= 12 * 6, sameLine: Math.abs(name.getBoundingClientRect().top - state.getBoundingClientRect().top) < 8, quickRest: getComputedStyle(quick).opacity }; });
    assert.deepEqual(rowShape, { whole: true, wide: true, sameLine: true, quickRest: '0' }, `member row shape: ${JSON.stringify(rowShape)}`);
    await agentPage.hover('[data-agent-id="agt_mock_worker"] .agent-name');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-quick', el => getComputedStyle(el).opacity), '1', 'hovering a member shows its buttons');
    await domClick(agentPage, '[data-agent-id="agt_mock_worker"] .agent-name');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-switch', el => el.offsetParent !== null), true, 'clicking the row opens its profile');
    assert.deepEqual(await agentPage.evaluate(() => ({ title: document.querySelector('#agents-panel header h2').textContent, closeShown: !document.querySelector('#agent-profile-close').hidden, others: [...document.querySelectorAll('#agents-list .agent-card:not(.expanded)')].every(card => getComputedStyle(card).display === 'none'), humans: getComputedStyle(document.querySelector('#agents-humans')).display })), { title: 'Agent profile', closeShown: true, others: true, humans: 'none' }, 'the profile replaces the members list');
    // The state reads like v1, "working 0:12": the state and how long it has lasted, in a readable pill (not tiny mono text).
    assert.deepEqual(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-state-pill', el => { const cs = getComputedStyle(el); return { text: el.textContent.replace(/\s+/g, ' ').trim().replace(/\d+:\d\d$/, 'M:SS'), size: parseFloat(cs.fontSize) >= 12, sans: !/mono/i.test(cs.fontFamily), pill: parseFloat(cs.borderTopWidth) === 1 && parseFloat(cs.borderTopLeftRadius) > 10 }; }), { text: 'working M:SS', size: true, sans: true, pill: true }, 'the profile state is "state age" in a readable pill');
    const ageBefore = await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-state-age', el => el.textContent);
    await agentPage.waitForFunction(before => document.querySelector('[data-agent-id="agt_mock_worker"] .agent-state-age').textContent !== before, { timeout: 5000 }, ageBefore);
    // A long-lived state reads "4d", never a clock that runs past the card: 104 h 40 min ago, whole and untruncated at both widths.
    for (const width of [1440, 390]) {
      await agentPage.setViewport({ width, height: 900 });
      await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-state', (state, since) => { state.dataset.since = since; }, new Date(Date.now() - (104 * 3600 + 40 * 60) * 1000).toISOString());
      await agentPage.waitForFunction(() => document.querySelector('[data-agent-id="agt_mock_worker"] .agent-state-age')?.textContent === '4d', { timeout: 5000 });
      assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-state-pill', el => el.scrollWidth <= el.clientWidth + 1 && el.getBoundingClientRect().right <= innerWidth), true, `the state age is cut off at ${width}px`);
    }
    await agentPage.setViewport({ width: 1100, height: 720 });
    // The profile's actions are labelled buttons; Stop is the danger one, Pause a plain one, and the row's hover icons are not repeated.
    assert.deepEqual(await agentPage.$$eval('[data-agent-id="agt_mock_worker"] .agent-actions button', els => els.map(el => [el.getAttribute('aria-label'), el.textContent, el.classList.contains('icon-btn'), el.classList.contains('icon-btn-danger')])), [['Message', '', true, false], ['Pause', '', true, false], ['Stop', '', true, true], ['Switch', '', true, false], ['Reactivate', '', true, false]], 'the agent actions are icon buttons named by aria-label, with no text');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-quick', el => [...el.children].filter(child => getComputedStyle(child).display !== 'none').length), 1, 'the open profile keeps only its close button in the row');
    // A disabled action says why: in its tooltip and in a line under the buttons.
    assert.deepEqual(await agentPage.$eval('[data-agent-id="agt_mock_worker"]', card => { const button = card.querySelector('.agent-reactivate'), hint = card.querySelector(`#${button.getAttribute('aria-describedby')}`); return { disabled: button.disabled, title: button.title, hint: hint?.textContent, shown: !!hint?.offsetParent, pauseTitle: card.querySelector('.agent-act-pause').title, pauseDescribed: card.querySelector('.agent-act-pause').hasAttribute('aria-describedby') }; }), { disabled: true, title: 'Reactivate is unavailable: it is already active', hint: 'Reactivate is unavailable: it is already active.', shown: true, pauseTitle: 'Pause: finish the current turn, then accept no new work', pauseDescribed: false });
    const factLabels = await agentPage.$$eval('[data-agent-id="agt_mock_worker"] .profile-fact-label', els => els.map(el => el.textContent));
    for (const label of ['Status', 'Presence', 'Runtime', 'Role', 'Agent ID']) assert.ok(factLabels.includes(label), `the profile lacks ${label}: ${factLabels}`);
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .profile-fact-value code, [data-agent-id="agt_mock_worker"] code.profile-fact-value', el => el.textContent), 'agt_mock_worker', 'ids are monospace');
    await domClick(agentPage, '#agent-profile-close');
    assert.deepEqual(await agentPage.evaluate(() => ({ title: document.querySelector('#agents-panel header h2').textContent, list: getComputedStyle(document.querySelector('[data-agent-id="agt_mock_worker"]')).display !== 'none', closeShown: !document.querySelector('#agent-profile-close').hidden })), { title: 'Room members', list: true, closeShown: false }, 'the close button brings the members back');
    await domClick(agentPage, '[data-agent-id="agt_mock_worker"] .agent-name');
    await agentPage.keyboard.press('Escape');
    assert.equal(await agentPage.$eval('#agents-panel', el => el.classList.contains('profile-open')), false, 'Escape closes the profile');
    await domClick(agentPage, '[data-agent-id="agt_mock_worker"] .agent-name');
    const agentBefore = await agentPage.$('[data-agent-id="agt_mock_worker"]');
    await agentAct('agt_mock_worker', 'agent-stop');
    await agentPage.waitForFunction(() => document.querySelector('[data-agent-id="agt_mock_worker"] .agent-state-label')?.textContent === 'stopping');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-switch', el => el.disabled && el.title === 'Switch is unavailable: reactivate it first'), true);
    const agentAfter = await agentPage.$('[data-agent-id="agt_mock_worker"]');
    assert.equal(await agentPage.evaluate((a, b) => a === b, agentBefore, agentAfter), true, 'agent card was replaced');
    await agentAct('agt_mock_reviewer', 'agent-pause');
    await agentPage.waitForFunction(() => document.querySelector('[data-agent-id="agt_mock_reviewer"] .agent-state-label')?.textContent === 'paused');
    await agentAct('agt_mock_reviewer', 'agent-reactivate');
    await agentPage.waitForFunction(() => document.querySelector('[data-agent-id="agt_mock_reviewer"] .agent-state-label')?.textContent === 'idle');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_reviewer"] .agent-switch', el => el.disabled && el.title === 'Switch is unavailable: resolve the recovery item first'), true);
    await agentAct('agt_mock_worker', 'agent-reactivate');
    await agentPage.waitForFunction(() => document.querySelector('[data-agent-id="agt_mock_worker"] .agent-state-label')?.textContent === 'idle');
    await agentAct('agt_mock_reviewer', 'agent-stop');
    await agentPage.waitForFunction(() => document.querySelector('[data-agent-id="agt_mock_reviewer"] .agent-state-label')?.textContent === 'stopped');
    await agentAct('agt_mock_reviewer', 'agent-reactivate');
    await agentPage.waitForFunction(() => document.querySelector('[data-agent-id="agt_mock_reviewer"] .agent-state-label')?.textContent === 'idle');
    const switchingCard = await agentPage.$('[data-agent-id="agt_mock_worker"]');
    await agentAct('agt_mock_worker', 'agent-switch');
    await agentPage.waitForFunction(() => document.querySelectorAll('#switch-form [name=tool] option').length === 3);
    await agentPage.select('#switch-form [name=tool]', '0');
    await agentPage.select('#switch-form [name=model]', 'test');
    await agentPage.click('#switch-form button[type=submit]');
    await agentPage.waitForFunction(() => document.querySelector('[data-agent-id="agt_mock_worker"] .agent-switch-status')?.textContent === 'Starting new session: fake · test');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-switch', el => el.disabled), true, 'switch should be disabled during handoff');
    await agentPage.waitForFunction(() => !document.querySelector('#switch-dialog').open && document.querySelector('[data-agent-id="agt_mock_worker"] .agent-meta')?.textContent === 'fake · test');
    assert.equal(await agentPage.$eval('[data-agent-id="agt_mock_worker"] .agent-context-value', el => el.textContent), '—', 'new generation reused old context');
    assert.equal(await agentPage.evaluate((before, after) => before === after, switchingCard, await agentPage.$('[data-agent-id="agt_mock_worker"]')), true, 'tool switch replaced agent card');
    await agentAct('agt_mock_worker', 'agent-switch');
    await agentPage.waitForFunction(() => document.querySelector('#switch-form [name=model]')?.value === 'test');
    assert.equal(await agentPage.$eval('#switch-form button[type=submit]', el => el.disabled), true, 'unchanged model should not submit');
    await agentPage.select('#switch-form [name=model]', 'fake-reviewer');
    await domClick(agentPage, '#switch-form button[type=submit]');
    await agentPage.waitForFunction(() => !document.querySelector('#switch-dialog').open && document.querySelector('[data-agent-id="agt_mock_worker"] .agent-meta')?.textContent === 'fake · fake-reviewer');
    await agentPage.evaluate(() => document.querySelector('#agent-profile-close').click()); // Spawn is part of the members list, not of a profile
    await agentPage.click('#spawn-open');
    await agentPage.waitForFunction(() => document.querySelectorAll('#spawn-form [name=tool] option').length === 3);
    assert.equal(await agentPage.$eval('#spawn-form [name=tool] option:nth-child(2)', el => el.disabled && el.textContent.includes('not offered by a connected machine')), true);
    assert.equal(await agentPage.$eval('#spawn-version', el => el.textContent), 'CLI version fixture-1');
    await agentPage.select('#spawn-form [name=role]', 'reviewer');
    await agentPage.select('#spawn-form [name=model]', 'fake-reviewer');
    await domClick(agentPage, '#spawn-form .dialog-actions button:last-child');
    await agentPage.waitForFunction(() => document.querySelectorAll('.agent-card[data-agent-id]').length === 3 && !document.querySelector('#spawn-dialog').open);
    await agentPage.waitForFunction(() => getComputedStyle(document.querySelector('.chat')).display === 'flex' && document.querySelectorAll('.agent-card[data-agent-id]').length === 3);
    assert.equal(await agentPage.$eval('#rooms-toggle', el => getComputedStyle(el).display), 'none', 'the top-bar Rooms button is gone on desktop (the edge toggle replaces it)');
    // v1-style column toggles: the shared 28px outlined square with a thin chevron inside each column's header. A collapsed column is 0px
    // wide (nothing is left over) and its expand button sits in the chat header: left of the room name for rooms, at the right end for members.
    const edge = (id, container) => agentPage.evaluate((id, container) => { const el = document.querySelector(id), box = document.querySelector(container), r = el.getBoundingClientRect(), c = box.getBoundingClientRect(), cs = getComputedStyle(el);
      return { direction: el.dataset.direction, chevron: el.querySelectorAll('svg polyline').length, text: el.textContent.trim(), label: el.getAttribute('aria-label'), visible: cs.display !== 'none' && r.width > 0, size: [Math.round(r.width), Math.round(r.height)], outlined: parseFloat(cs.borderTopWidth) === 1,
        inside: r.left >= c.left - 0.5 && r.right <= c.right + 0.5 && r.top >= c.top - 0.5 && r.bottom <= c.bottom + 0.5, left: r.left, right: r.right }; }, id, container);
    let left = await edge('#edge-rooms', '.sidebar > header'), right = await edge('#edge-agents', '#agents-panel > header');
    assert.deepEqual([left.direction, left.chevron, left.text, left.label, left.visible, left.size, left.outlined, left.inside], ['left', 1, '', 'Hide rooms', true, [28, 28], true, true], `rooms toggle: ${JSON.stringify(left)}`);
    assert.deepEqual([right.direction, right.chevron, right.text, right.label, right.visible, right.size, right.outlined, right.inside], ['right', 1, '', 'Hide agents', true, [28, 28], true, true], `agents toggle: ${JSON.stringify(right)}`);
    assert.deepEqual(await agentPage.evaluate(() => ['#edge-rooms-expand', '#edge-agents-expand'].map(id => getComputedStyle(document.querySelector(id)).display)), ['none', 'none'], 'the expand buttons show only while their column is collapsed');
    // The ⋯ overflow button exists only where the controls overflow into it (phones): never a dead button on desktop.
    for (const width of [1100, 1440]) {
      await agentPage.setViewport({ width, height: 800 });
      assert.equal(await agentPage.$eval('#room-menu-toggle', el => el.getClientRects().length === 0), true, `the overflow button is hidden at ${width}px`);
    }
    await agentPage.setViewport({ width: 1100, height: 720 });
    const cards = await iconOnlyReport(agentPage, ['.agent-expand', '.agent-act', '.profile-copy', '.agent-card .agent-quick .icon-btn', '.add-room']);
    assert.deepEqual(cards.bad, [], 'the agent card buttons are icon buttons with a tooltip and a name'); assert.ok(cards.found >= 10, `agent card icon buttons found: ${cards.found}`);
    // Rooms header: the + and ‹ sit together at the right with a 6px gap, and the filter box starts right under the header.
    assert.deepEqual(await agentPage.evaluate(() => { const header = document.querySelector('.sidebar > header').getBoundingClientRect(), plus = document.querySelector('#project-open').getBoundingClientRect(), collapse = document.querySelector('#edge-rooms').getBoundingClientRect(), filter = document.querySelector('.sidebar .room-filter').getBoundingClientRect(); return { gap: Math.round(collapse.left - plus.right), rightPad: Math.round(header.right - collapse.right), band: Math.round(filter.top - header.bottom) <= 14 }; }), { gap: 6, rightPad: 12, band: true }, 'the rooms header groups its buttons and leaves no empty band');
    // One button system: every visible button in the app shell is a .btn, an .icon-btn or a .row-btn, and a hover lights the
    // top bar's buttons up (the border and the text go to the primary text colour).
    const strays = await agentPage.evaluate(() => [...document.querySelectorAll('#app-view button, .workspace-bar button')].filter(el => el.getClientRects().length && !el.matches('.btn, .icon-btn, .row-btn')).map(el => `${el.id || el.className || el.tagName}:${el.textContent.trim().slice(0, 16)}`));
    assert.deepEqual(strays, [], `buttons outside the button system: ${strays.join(', ')}`);
    const topBefore = await agentPage.$eval('#inbox-toggle', el => { const cs = getComputedStyle(el); return { border: cs.borderTopColor, colour: cs.color }; });
    await agentPage.hover('#inbox-toggle'); await new Promise(resolve => setTimeout(resolve, 400)); // past the colour transition
    const topAfter = await agentPage.evaluate(() => { const cs = getComputedStyle(document.querySelector('#inbox-toggle')); return { border: cs.borderTopColor, colour: cs.color, primary: getComputedStyle(document.documentElement).getPropertyValue('--text-primary').trim() }; });
    assert.ok(topAfter.border !== topBefore.border && topAfter.border === topAfter.colour, `a top-bar hover must light the border up to the text colour: ${JSON.stringify([topBefore, topAfter])}`);
    await agentPage.mouse.move(5, 5); await new Promise(resolve => setTimeout(resolve, 400)); // the Inbox button just lost its hover; let the colour transition finish
    // Room header icon buttons share one style; the workspace bar uses its own v1-sized transparent buttons.
    const iconButtons = await agentPage.evaluate(() => [...document.querySelectorAll('.sidebar header button.icon-btn, .chat header button.icon-btn, .room-controls button.icon-btn')].filter(el => el.getClientRects().length && !el.matches('.btn-primary, .btn-danger')).map(el => { const cs = getComputedStyle(el), r = el.getBoundingClientRect(), svg = el.querySelector('svg'); return { id: el.id, size: [Math.round(r.width), Math.round(r.height)], border: cs.borderTopWidth + ' ' + cs.borderTopStyle + ' ' + cs.borderTopColor, radius: cs.borderTopLeftRadius, background: cs.backgroundColor, icon: svg ? [Math.round(svg.getBoundingClientRect().width), Math.round(svg.getBoundingClientRect().height)].join('x') : 'text' }; }));
    assert.ok(iconButtons.length >= 6, `header icon buttons found: ${JSON.stringify(iconButtons.map(item => item.id))}`);
    for (const key of ['size', 'border', 'radius', 'background']) assert.equal(new Set(iconButtons.map(item => JSON.stringify(item[key]))).size, 1, `header icon buttons differ in ${key}: ${JSON.stringify(iconButtons)}`);
    const iconBefore = await agentPage.$eval('#room-info', el => ({ fill: getComputedStyle(el).backgroundColor, colour: getComputedStyle(el).color }));
    await agentPage.hover('#room-info'); await new Promise(resolve => setTimeout(resolve, 400)); // past the colour transition
    assert.deepEqual(await agentPage.$eval('#room-info', (el, was) => { const cs = getComputedStyle(el); return { fillKept: cs.backgroundColor === was.fill, iconBrightens: cs.color !== was.colour }; }, iconBefore), { fillKept: true, iconBrightens: true }, 'hover brightens the icon and keeps the fill');
    await agentPage.mouse.move(5, 5);
    // The members header sits on the same row as the chat header: no empty band above it.
    assert.deepEqual(await agentPage.evaluate(() => { const a = document.querySelector('.chat > header').getBoundingClientRect(), b = document.querySelector('#agents-panel > header').getBoundingClientRect(); return [Math.abs(a.top - b.top) < 1, Math.abs(a.height - b.height) < 1]; }), [true, true], 'the members header must align with the chat header');
    await agentPage.click('#edge-rooms');
    left = await edge('#edge-rooms-expand', '.chat > header');
    const roomsGone = await agentPage.evaluate(() => ({ column: getComputedStyle(document.querySelector('.app')).gridTemplateColumns.split(' ')[0], sidebar: document.querySelector('.sidebar').getBoundingClientRect().width, titleLeft: document.querySelector('#room-title').getBoundingClientRect().left }));
    assert.deepEqual([left.direction, left.label, left.visible, left.inside, roomsGone.column, roomsGone.sidebar], ['right', 'Show rooms', true, true, '0px', 0], `a collapsed rooms column is 0px and its expand button is in the chat header: ${JSON.stringify([left, roomsGone])}`);
    assert.ok(left.right <= roomsGone.titleLeft + 0.5, 'the rooms expand button sits left of the room name');
    await agentPage.click('#edge-rooms-expand');
    assert.equal(await agentPage.$eval('.sidebar #projects', el => getComputedStyle(el).display) !== 'none', true);
    assert.equal(await agentPage.$('#rooms-collapse'), null, 'the old in-sidebar collapse button is gone');
    await agentPage.click('#edge-agents');
    right = await edge('#edge-agents-expand', '.chat > header');
    assert.deepEqual([right.direction, right.label, right.visible, right.inside], ['left', 'Show agents', true, true], `the agents expand button is in the chat header: ${JSON.stringify(right)}`);
    assert.equal(await agentPage.$eval('#agents-panel', el => getComputedStyle(el).display), 'none');
    const viewportWidth = await agentPage.evaluate(() => innerWidth);
    assert.ok(viewportWidth - right.right < 40, 'the agents expand button is not at the right end of the header');
    // Messages use the whole column (no 75% cap, no shrink-wrap) in both states.
    const fill = await agentPage.$eval('#messages', el => { const cs = getComputedStyle(el), column = el.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight); return [...el.querySelectorAll('.message:not(.system-notice)')].map(m => ({ id: m.dataset.messageId, cls: m.className, width: Math.round(m.getBoundingClientRect().width), column: Math.round(column) })); });
    assert.ok(fill.every(m => m.cls.split(' ').includes('self') || Math.abs(m.width - m.column) < 2), `someone else's message does not fill the column: ${JSON.stringify(fill)}`);
    assert.equal(await agentPage.$eval('#messages', el => getComputedStyle(el).marginRight), '0px', 'a collapsed members column frees all of its space');
    assert.notEqual(await agentPage.$eval('.chat > header', el => getComputedStyle(el).paddingRight), '84px', 'the chat header no longer reserves room for the removed Agents button');
    await agentPage.click('#edge-agents-expand');
    assert.notEqual(await agentPage.$eval('#agents-panel', el => getComputedStyle(el).display), 'none');
    await agentPage.$eval('[data-agent-id="agt_mock_reviewer"] .agent-recovery', el => el.scrollIntoView({ block: 'center' }));
    assert.deepEqual(await agentPage.$eval('[data-agent-id="agt_mock_reviewer"] .agent-recovery', el => [el.getAttribute('aria-label'), el.title, !!el.querySelector('.icon-mask-health'), el.textContent]), ['Recovery needed', 'Recovery needed', true, '']);
    await agentPage.waitForFunction(() => {
      const button = document.querySelector('[data-agent-id="agt_mock_reviewer"] .agent-recovery');
      if (!button || button.hidden || getComputedStyle(button).visibility !== 'visible') return false;
      const rect = button.getBoundingClientRect();
      if (rect.width <= 0 || rect.height <= 0) return false;
      const top = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2);
      return top === button || button.contains(top);
    });
    await domClick(agentPage, '[data-agent-id="agt_mock_reviewer"] .agent-recovery');
    assert.equal(await agentPage.$eval('#inbox-view', el => !el.hidden), true, 'recovery badge did not open inbox');
    // On desktop the room list stays beside the Inbox (only the chat column is replaced); a click on a room returns to the chat.
    assert.deepEqual(await agentPage.evaluate(() => { const side = document.querySelector('.sidebar'), view = document.querySelector('#inbox-view'), s = side.getBoundingClientRect(), v = view.getBoundingClientRect(); return { sidebarShown: !side.hidden && s.width > 100, chatHidden: document.querySelector('.chat').hidden, sideBySide: s.right <= v.left + 1 }; }), { sidebarShown: true, chatHidden: true, sideBySide: true }, 'the Inbox view replaces only the chat column');
    await domClick(agentPage, '.room-row.active .room-name');
    await agentPage.waitForFunction(() => document.querySelector('#inbox-view').hidden && !document.querySelector('.chat').hidden);
    await domClick(agentPage, '#inbox-toggle');
    await agentPage.waitForFunction(() => !document.querySelector('#inbox-view').hidden);
    // A health item with an Inbox item behind it opens that item; one without goes to the room.
    await agentPage.evaluate(async () => { await document.querySelector('.room-row.active .room-name').onclick(); });
    await agentPage.waitForFunction(() => document.querySelector('#inbox-view').hidden);
    await domClick(agentPage, '#health-toggle');
    await agentPage.click('#health-popover .health-item[data-code="recovery_waiting"]');
    await agentPage.waitForFunction(() => !document.querySelector('#inbox-view').hidden && document.querySelector('#inbox-view [data-inbox-id="mock_approval"]'));
    assert.equal(await agentPage.$eval('#health-popover', el => el.hidden), true, 'choosing a health item closes the list');
    // Leave the Inbox, then the held answer opens its own item.
    await agentPage.evaluate(async () => { await document.querySelector('.room-row.active .room-name').onclick(); });
    await agentPage.waitForFunction(() => document.querySelector('#inbox-view').hidden);
    await domClick(agentPage, '#health-toggle');
    await domClick(agentPage, '#health-popover .health-item[data-code="held_answer"]');
    await agentPage.waitForFunction(() => !document.querySelector('#inbox-view').hidden && document.querySelector('#inbox-view [data-inbox-id="mock_held"]'));
    await agentPage.evaluate(async () => { await document.querySelector('.room-row.active .room-name').onclick(); });
    await agentPage.waitForFunction(() => document.querySelector('#inbox-view').hidden);
    await agentPage.setViewport({ width: 390, height: 780 });
    await domClick(agentPage, '.mobile-tab[data-tab="rooms"]');
    await agentPage.waitForFunction(() => document.querySelector('#app-view').classList.contains('rooms-open') && !document.querySelector('#app-view').classList.contains('inbox-open') && document.querySelector('#inbox-view').hidden);
    await agentPage.evaluate(async () => { const room = document.querySelector('.room-row.active .room-name'); if (!room) throw new Error('room missing'); await room.onclick(); });
    await agentPage.waitForFunction(() => document.querySelector('#app-view')?.classList.contains('chat-open') && !document.querySelector('#app-view').classList.contains('rooms-open') && document.querySelectorAll('.agent-card[data-agent-id]').length === 3 && getComputedStyle(document.querySelector('.chat')).display === 'flex');
    // Phone: Health sits in the ⋯ menu; choosing a recovery item there opens the Inbox on its card with the drawers closed.
    await domClick(agentPage, '#more-toggle');
    await agentPage.evaluate(() => [...document.querySelectorAll('#more-menu .more-item')].find(item => item.textContent.startsWith('Health')).click());
    await agentPage.waitForSelector('#health-popover:not([hidden]) .health-item[data-code="recovery_waiting"]');
    await domClick(agentPage, '#health-popover .health-item[data-code="recovery_waiting"]');
    await agentPage.waitForFunction(() => !document.querySelector('#inbox-view').hidden && document.querySelector('#inbox-view [data-inbox-id="mock_approval"]') && !document.querySelector('#agents-panel').classList.contains('mobile-open') && !document.querySelector('#app-view').classList.contains('rooms-open'));
    await domClick(agentPage, '.mobile-tab[data-tab="rooms"]');
    await agentPage.waitForFunction(() => document.querySelector('#app-view').classList.contains('rooms-open') && document.querySelector('#inbox-view').hidden);
    await agentPage.evaluate(async () => { await document.querySelector('.room-row.active .room-name').onclick(); });
    await agentPage.waitForFunction(() => document.querySelector('#app-view')?.classList.contains('chat-open') && !document.querySelector('#app-view').classList.contains('rooms-open'));
    await agentPage.click('.mobile-tab[data-tab="agents"]');
    assert.equal(await agentPage.$eval('#agents-panel', el => getComputedStyle(el).display), 'flex', 'agents drawer did not open on phone');
    await agentPage.waitForFunction(() => Math.abs(document.querySelector('#agents-panel').getBoundingClientRect().right - innerWidth) < 1).catch(async error => { throw new Error(`${error.message}: ${JSON.stringify(await agentPage.evaluate(() => ({ rect: document.querySelector('#agents-panel').getBoundingClientRect().toJSON(), viewport: innerWidth, classes: document.querySelector('#agents-panel').className, chat: document.querySelector('.chat').className })))}`); });
    assert.equal(await agentPage.$eval('#agents-scrim', el => getComputedStyle(el).display), 'block');
    await domClick(agentPage, '#agents-close');
    await agentPage.waitForFunction(() => !document.querySelector('#agents-panel').classList.contains('mobile-open'));
    await domClick(agentPage, '.mobile-tab[data-tab="rooms"]');
    await agentPage.waitForFunction(() => document.querySelector('#app-view').classList.contains('rooms-open') && Math.abs(document.querySelector('.sidebar').getBoundingClientRect().left) < 1);
    await agentPage.mouse.click(380, 400);
    await agentPage.waitForFunction(() => !document.querySelector('#app-view').classList.contains('rooms-open'));
    const phone = await agentPage.evaluate(() => ({ tabs: document.querySelector('#mobile-tabs').getBoundingClientRect().toJSON(), composer: document.querySelector('#composer').getBoundingClientRect().toJSON(), chat: document.querySelector('.chat').getBoundingClientRect().toJSON(), bar: document.querySelector('.workspace-bar').getBoundingClientRect().toJSON(), viewport: { width: innerWidth, height: innerHeight } }));
    assert.equal(Math.abs(phone.composer.bottom - phone.tabs.top) < 1 && Math.abs(phone.tabs.bottom - phone.viewport.height) < 1, true, `phone composer is not pinned just above the bottom tab bar: ${JSON.stringify(phone)}`);
    // v1's phone navigation: the bottom tabs drive the drawers and show where you are; one logo; v1's names on the top-bar icons.
    {
      const tab = name => agentPage.click(`.mobile-tab[data-tab="${name}"]`);
      const where = () => agentPage.evaluate(() => ({ active: [...document.querySelectorAll('.mobile-tab.active')].map(el => el.dataset.tab), current: document.querySelector('.mobile-tab[aria-current]')?.dataset.tab, rooms: document.querySelector('#app-view').classList.contains('rooms-open'), agents: document.querySelector('#agents-panel').classList.contains('mobile-open') }));
      assert.deepEqual(await where(), { active: ['chat'], current: 'chat', rooms: false, agents: false });
      await tab('agents');
      assert.deepEqual(await where(), { active: ['agents'], current: 'agents', rooms: false, agents: true });
      await tab('rooms');
      assert.deepEqual(await where(), { active: ['rooms'], current: 'rooms', rooms: true, agents: false });
      // The open rooms drawer ends above the bar, and its last row can be scrolled fully into view above it.
      const drawer = await agentPage.evaluate(async () => { const side = document.querySelector('.sidebar'); const last = [...side.querySelectorAll('.room-row, .dm-row, [data-room-id]')].pop(); last?.scrollIntoView({ block: 'end' }); await new Promise(r => setTimeout(r, 50)); const tabs = document.querySelector('#mobile-tabs').getBoundingClientRect(), box = side.getBoundingClientRect(), row = last?.getBoundingClientRect(); return { aboveBar: box.bottom <= tabs.top + 0.5, lastVisible: !row || row.bottom <= tabs.top + 0.5 }; });
      assert.deepEqual(drawer, { aboveBar: true, lastVisible: true }, 'the rooms drawer is not covered by the bottom bar');
      await tab('chat');
      assert.deepEqual(await where(), { active: ['chat'], current: 'chat', rooms: false, agents: false });
      // A notch phone: with a 34px bottom inset the bar grows by the inset, its tabs keep their full height inside it, and the
      // chat ends exactly at the bar's top (no gap, no overflow into the safe area).
      const notch = await agentPage.evaluate(async () => {
        document.documentElement.style.setProperty('--sab', '34px'); await new Promise(r => setTimeout(r, 50));
        const bar = document.querySelector('#mobile-tabs').getBoundingClientRect(), tab = document.querySelector('.mobile-tab').getBoundingClientRect(), chat = document.querySelector('.chat').getBoundingClientRect();
        const result = { barHeight: Math.round(bar.height), bottomAtViewport: Math.abs(bar.bottom - innerHeight) < 1, tabInside: tab.bottom <= bar.bottom - 34 + 0.5 && tab.height >= 40, chatMeetsBar: Math.abs(chat.bottom - bar.top) < 1 };
        document.documentElement.style.removeProperty('--sab'); return result;
      });
      assert.deepEqual(notch, { barHeight: 78, bottomAtViewport: true, tabInside: true, chatMeetsBar: true }, 'the phone tab bar mishandles a bottom safe-area inset');
      assert.equal(await agentPage.$$eval('.workspace-brand img', imgs => imgs.filter(img => getComputedStyle(img).display !== 'none').length), 1, 'one logo is shown on a phone');
      assert.deepEqual(await agentPage.evaluate(() => ['#usages-toggle', '#knowledge-toggle', '#system-events-toggle', '#settings-toggle'].map(id => { const el = document.querySelector(id); return el && [el.title, el.getAttribute('aria-label')]; })), [['Usages', 'Usages'], ['Memory', 'Memory'], ['System events', 'System events'], ['Settings', 'Settings']], "the top-bar icons carry v1's names");
    }
    await agentPage.setViewport({ width: 320, height: 640 });
    const narrow = await agentPage.evaluate(() => ({ logout: document.querySelector('#logout').getBoundingClientRect().right, send: document.querySelector('#composer button[type=submit]').getBoundingClientRect().right, controls: document.querySelector('.room-controls').getBoundingClientRect().right, agents: document.querySelector('#mobile-tabs').getBoundingClientRect().right, bottom: document.querySelector('#composer').getBoundingClientRect().bottom, topbar: document.querySelector('.workspace-bar').getBoundingClientRect().height, chat: document.querySelector('.chat').getBoundingClientRect().toJSON(), width: innerWidth, height: innerHeight }));
    assert.equal(narrow.logout <= narrow.width && narrow.send <= narrow.width && narrow.controls <= narrow.width && narrow.agents <= narrow.width && Math.abs(narrow.bottom - (narrow.height - 44)) < 1, true, `narrow phone layout overflows (the composer sits just above the 44px tab bar): ${JSON.stringify(narrow)}`);
    // Phone room header: the room controls collapse into one overflow menu, nothing overlaps,
    // and every header target is at least 40px.
    for (const width of [390, 360, 320]) {
      await agentPage.setViewport({ width, height: 780 });
      const header = await agentPage.evaluate(() => {
        const box = selector => { const el = document.querySelector(selector); if (!el || el.hidden) return null; const r = el.getBoundingClientRect(); return r.width ? r.toJSON() : null; };
        return { vw: innerWidth, back: box('#back'), title: box('#room-title'), menu: box('#room-menu-toggle'), agents: box('#agents-toggle'), controls: box('.room-controls'), tabs: ['rooms', 'chat', 'agents'].map(name => box(`.mobile-tab[data-tab="${name}"]`)) };
      });
      assert.equal(header.controls, null, `room controls still occupy the phone header at ${width}`);
      // v1's phone layout: the drawer buttons are replaced by the bottom Rooms | Chat | Agents bar.
      assert.ok(!header.back && !header.agents, `the drawer buttons are not shown on a phone at ${width}`);
      assert.ok(header.title && header.menu && header.tabs.every(Boolean), `phone header lost a control at ${width}: ${JSON.stringify(header)}`);
      for (const [name, box] of [['menu', header.menu], ...header.tabs.map((box, index) => [`tab ${index}`, box])]) assert.ok(box.height >= 39.5 && box.width >= 39.5, `${name} tap target under 40px at ${width}: ${JSON.stringify(box)}`);
      assert.ok(header.title.width >= 50, `room title squeezed to ${header.title.width}px at ${width}`);
      const parts = [header.title, header.menu];
      assert.ok(parts.every((part, index) => part.right <= header.vw && (index === 0 || parts[index - 1].right <= part.left + 0.5)), `phone header overlaps or overflows at ${width}: ${JSON.stringify(header)}`);
    }
    await agentPage.setViewport({ width: 390, height: 780 });
    // A rooms drawer that is still sliding out must not swallow a tap: closing it makes it non-interactive at once, so the real
    // mouse click below reaches the toggle that sits inside the drawer's width. (It used to hit the drawer for ~180ms.)
    await agentPage.evaluate(() => document.querySelector('#app-view').classList.add('chat-open', 'rooms-open'));
    await agentPage.waitForFunction(() => getComputedStyle(document.querySelector('.sidebar')).visibility === 'visible' && Math.abs(document.querySelector('.sidebar').getBoundingClientRect().left) < 1);
    assert.equal(await agentPage.evaluate(() => {
      const app = document.querySelector('#app-view'), toggle = document.querySelector('#room-menu-toggle');
      app.classList.remove('rooms-open'); // the drawer is now sliding out, still over the toggle
      const box = toggle.getBoundingClientRect(), hit = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2);
      return hit === toggle;
    }), true, 'a closing rooms drawer covers the room menu toggle');
    await agentPage.waitForFunction(() => getComputedStyle(document.querySelector('.sidebar')).visibility === 'hidden');
    await agentPage.click('#room-menu-toggle');
    await agentPage.waitForFunction(() => !document.querySelector('#room-menu').hidden, { timeout: 15000 }).catch(async error => {
      const state = await agentPage.evaluate(() => {
        const toggle = document.querySelector('#room-menu-toggle'), box = toggle.getBoundingClientRect();
        const top = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2);
        return { toggle: [box.left, box.top, box.width, box.height], covering: top ? `${top.tagName.toLowerCase()}#${top.id}.${top.className}` : null, viewport: [innerWidth, innerHeight], menuHidden: document.querySelector('#room-menu').hidden, expanded: toggle.getAttribute('aria-expanded'), openDialogs: [...document.querySelectorAll('dialog[open]')].map(d => d.id), popovers: [...document.querySelectorAll('[id$="-popover"]:not([hidden])')].map(p => p.id) };
      }).catch(failure => `state unavailable: ${failure.message}`);
      error.message = `room menu did not open: ${error.message}\n${JSON.stringify(state)}`;
      throw error;
    });
    const menuState = await agentPage.evaluate(() => {
      const menu = document.querySelector('#room-menu').getBoundingClientRect();
      return { labels: [...document.querySelectorAll('#room-menu .room-menu-item')].map(item => item.textContent), heights: [...document.querySelectorAll('#room-menu .room-menu-item')].map(item => item.getBoundingClientRect().height), left: menu.left, right: menu.right, vw: innerWidth, expanded: document.querySelector('#room-menu-toggle').getAttribute('aria-expanded'), focus: document.activeElement.className };
    });
    for (const label of ['Room settings', 'Go to message number', 'Show raw Markdown', 'Export room']) assert.ok(menuState.labels.includes(label), `room menu lacks "${label}": ${JSON.stringify(menuState.labels)}`);
    assert.ok(menuState.heights.every(height => height >= 43.5) && menuState.left >= 0 && menuState.right <= menuState.vw && menuState.expanded === 'true' && menuState.focus.includes('room-menu-item'), `room menu geometry or focus is off: ${JSON.stringify(menuState)}`);
    await agentPage.keyboard.press('Escape');
    await agentPage.waitForFunction(() => document.querySelector('#room-menu').hidden && document.activeElement.id === 'room-menu-toggle' && document.querySelector('#room-menu-toggle').getAttribute('aria-expanded') === 'false');
    await agentPage.click('#room-menu-toggle');
    await agentPage.waitForFunction(() => [...document.querySelectorAll('#room-menu .room-menu-item')].some(item => item.textContent === 'Show raw Markdown'), { timeout: 15000 }).catch(async error => { throw new Error(`room menu lacks "Show raw Markdown": ${JSON.stringify(await agentPage.evaluate(() => ({ hidden: document.querySelector('#room-menu').hidden, labels: [...document.querySelectorAll('#room-menu .room-menu-item')].map(item => item.textContent), raw: document.querySelector('#room-raw').getAttribute('aria-label') })))}`, { cause: error }); });
    await agentPage.evaluate(() => [...document.querySelectorAll('#room-menu .room-menu-item')].find(item => item.textContent === 'Show raw Markdown').click());
    await agentPage.waitForFunction(() => document.querySelector('#room-menu').hidden && document.querySelector('#room-raw').getAttribute('aria-pressed') === 'true');
    await agentPage.click('#room-menu-toggle');
    await agentPage.waitForFunction(() => [...document.querySelectorAll('#room-menu .room-menu-item')].some(item => item.textContent === 'Show rendered Markdown'), { timeout: 15000 }).catch(async error => { throw new Error(`room menu lacks "Show rendered Markdown": ${JSON.stringify(await agentPage.evaluate(() => ({ hidden: document.querySelector('#room-menu').hidden, labels: [...document.querySelectorAll('#room-menu .room-menu-item')].map(item => item.textContent), raw: document.querySelector('#room-raw').getAttribute('aria-label') })))}`, { cause: error }); });
    await agentPage.evaluate(() => [...document.querySelectorAll('#room-menu .room-menu-item')].find(item => item.textContent === 'Show rendered Markdown').click());
    await agentPage.waitForFunction(() => document.querySelector('#room-raw').getAttribute('aria-pressed') === 'false');
    await agentPage.click('#room-menu-toggle');
    await agentPage.evaluate(() => [...document.querySelectorAll('#room-menu .room-menu-item')].find(item => item.textContent === 'Room settings').click());
    await agentPage.waitForFunction(() => document.querySelector('#room-info-dialog').open && document.querySelector('#room-menu').hidden);
    // The dialog fills in its worktree, memory and scope sections asynchronously, which moves the Close
    // button; Escape does not depend on where it is.
    await agentPage.keyboard.press('Escape');
    await agentPage.waitForFunction(() => !document.querySelector('#room-info-dialog').open);
    await agentPage.click('#room-menu-toggle');
    await agentPage.mouse.click(40, 400);
    await agentPage.waitForFunction(() => document.querySelector('#room-menu').hidden);
    await agentPage.setViewport({ width: 320, height: 640 });
    if (supervisor.exitCode !== null) throw new Error(`supervisor exited: ${supervisorLogs}`);
    // The pages may be sitting in the Inbox view; its toggle is the way back to the chat now that the top-bar Rooms button is gone on desktop.
    for (const page of [first, second]) if (await page.$eval('#inbox-view', el => !el.hidden)) await domClick(page, '#inbox-toggle');
    await first.waitForFunction(() => document.querySelector('#inbox-view').hidden && !document.querySelector('.chat').hidden); await second.waitForFunction(() => document.querySelector('#inbox-view').hidden && !document.querySelector('.chat').hidden);
    await first.evaluate(async () => { const room = [...document.querySelectorAll('.room-name')].find(el => el.textContent.includes('Nested task')); await room.onclick(); });
    await second.evaluate(async () => { const room = [...document.querySelectorAll('.room-name')].find(el => el.textContent.includes('Nested task')); await room.onclick(); });
    await first.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Nested task');
    await second.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Nested task');
    const liveCatalog = await first.evaluate(async () => (await (await fetch('/api/spawn-catalog')).json()).tools);
    const choices = liveCatalog.map(item => [item.tool, item.variant]);
    assert.deepEqual(choices, [], 'a new machine has no harnesses: nothing is seeded');

    await first.click('#spawn-open');
    await first.waitForFunction(count => document.querySelectorAll('#spawn-form [name=tool] option').length === count, {}, liveCatalog.length);
    assert.deepEqual(await first.$$eval('#spawn-form [name=tool] option', options => options.map(option => option.disabled)), liveCatalog.map(item => !item.available));
    const firstAvailable = liveCatalog.find(item => item.available);
    if (firstAvailable) {
      assert.deepEqual(await first.$$eval('#spawn-form [name=model] option', options => options.map(option => option.value)), firstAvailable.models.map(model => model.id));
      assert.equal(await first.$eval('#spawn-version', el => el.textContent), firstAvailable.cli_version ? `CLI version ${firstAvailable.cli_version}` : '');
    } else assert.equal(await first.$eval('#spawn-form button[type=submit]', el => el.disabled), true);
    await first.waitForFunction(() => document.querySelector('#spawn-tool-reason').textContent.includes('Settings → Harnesses'), { timeout: 10000 }); // with no harness the dialog points to Settings → Harnesses
    // The hint is a link for the owner, and plain words for anyone else (who could only be refused): checked on the real class.
    const hints = await first.evaluate(async () => {
      const { AgentsPanel } = await import('/js/agents.js');
      const panel = Object.create(AgentsPanel.prototype);
      panel.dialog = { close() {} }; panel.switchDialog = { open: false, close() {} }; panel.spawnMachine = { id: 'mch_here' };
      const events = []; const listen = event => { events.push(event.detail); event.stopPropagation(); }; // the app's own listener must not open Settings here
      window.addEventListener('aimebu:open-settings', listen, true);
      const text = (owner, message) => { panel.canManageHarnesses = owner; const node = document.createElement('p'); panel.setHint(node, message); return [node.textContent, !!node.querySelector('button.inline-link')]; };
      const out = { ownerLink: text(true, 'No harness yet. Add a harness in Settings → Harnesses.'), other: text(false, 'No harness yet. Add a harness in Settings → Harnesses.'),
        otherCheck: text(false, 'not found; check Settings → Harnesses'), otherEnable: text(false, 'disabled by owner; enable it in Settings → Harnesses'), none: text(false, 'No models') };
      panel.canManageHarnesses = true; const node = document.createElement('p'); panel.setHint(node, 'x Settings → Harnesses'); node.querySelector('button').click();
      window.removeEventListener('aimebu:open-settings', listen, true);
      out.event = events;
      return out;
    });
    assert.deepEqual(hints, { ownerLink: ['No harness yet. Add a harness in Settings → Harnesses.', true], other: ['No harness yet. Ask an admin to add a harness.', false], otherCheck: ['not found; ask an admin to check the harnesses', false],
      otherEnable: ['disabled by owner; ask an admin to enable it', false], none: ['No models', false], event: [{ section: 'harnesses', machineID: 'mch_here' }] });
    // ... as a link: it closes the dialog and opens Settings → Harnesses.
    await first.waitForSelector('#spawn-tool-reason button.inline-link');
    await first.click('#spawn-tool-reason button.inline-link');
    await first.waitForFunction(() => !document.querySelector('#spawn-dialog').open && !document.querySelector('#settings-harnesses').hidden && document.querySelector('#settings-dialog').open);
    await first.click('#settings-close');
    await first.click('#spawn-open');
    await first.waitForFunction(() => document.querySelector('#spawn-dialog').open && document.querySelector('#spawn-tool-reason button.inline-link'));
    await first.click('#spawn-cancel');
    await first.evaluate(() => {
      window.__originalFetch = window.fetch;
      window.fetch = (input, options) => String(input).startsWith('/api/spawn-catalog')
        ? Promise.resolve(new Response(JSON.stringify({ message: 'catalog offline' }), { status: 503, headers: { 'Content-Type': 'application/json' } }))
        : window.__originalFetch(input, options);
    });
    await first.click('#spawn-open');
    await first.waitForFunction(() => document.querySelector('#spawn-tool-reason')?.textContent.includes('Tool catalog unavailable'));
    assert.equal(await first.$eval('#spawn-form button[type=submit]', el => el.disabled), true);
    await first.click('#spawn-cancel');
    await first.evaluate(() => { window.fetch = window.__originalFetch; });
    await first.evaluate(() => {
      window.__originalFetch = window.fetch;
      window.fetch = (input, options) => String(input).startsWith('/api/spawn-catalog')
        ? Promise.resolve(new Response(JSON.stringify({ tools: [{ tool: 'fake', variant: 'test', label: 'Fake', cli_version: 'fixture-1', models: [{ id: 'test', label: 'Test' }], available: true }, { tool: 'pi', variant: 'docker', label: 'Pi (Docker)', models: [{ id: 'ollama-cloud/gemma4:31b', label: 'Gemma 4 31B' }], available: true, toolbox: false, toolbox_reason: 'pi-docker has no private credential channel' }] }), { status: 200, headers: { 'Content-Type': 'application/json' } }))
        : window.__originalFetch(input, options);
    });
    // The dialog's worktree choice reaches the request. The request is captured and refused, so no agent is made by this check.
    await domClick(first, '#spawn-open');
    await first.waitForFunction(() => document.querySelector('#spawn-form [name=model]')?.value === 'test' && !document.querySelector('#spawn-form button[type=submit]').disabled);
    await first.evaluate(() => {
      window.__spawnBodies = []; window.__spawnInner = window.fetch; const inner = window.fetch;
      window.fetch = (input, options) => {
        if (String(input).endsWith('/agents') && options?.method === 'POST') { window.__spawnBodies.push(JSON.parse(options.body)); return Promise.resolve(new Response(JSON.stringify({ error: 'refused_for_test', message: 'refused for the test' }), { status: 409, headers: { 'Content-Type': 'application/json' } })); }
        return inner(input, options);
      };
    });
    await first.evaluate(() => { const box = document.querySelector('#spawn-form [name=worktree]'); box.checked = true; box.dispatchEvent(new Event('change', { bubbles: true })); });
    await first.type('#spawn-form [name=base_ref]', 'release/1.0');
    await domClick(first, '#spawn-form .dialog-actions button:last-child');
    await first.waitForFunction(() => window.__spawnBodies.length === 1 && !document.querySelector('#spawn-dialog').open);
    assert.deepEqual(await first.evaluate(() => [window.__spawnBodies[0].worktree, window.__spawnBodies[0].base_ref]), [true, 'release/1.0'], 'the worktree choice was dropped from the spawn request');
    // The refused submit left the box ticked. In a room that is not a task room the box is hidden and cannot be unticked, so nothing about a
    // worktree may be sent from there, and the dialog must open with the box clear.
    const homeRoomForSpawn = await first.evaluate(async id => (await (await fetch(`/api/projects/${id}/rooms`)).json()).find(room => room.kind === 'home').id, actualProjectID);
    await domClick(first, `[data-room-id="${homeRoomForSpawn}"] .room-name`);
    await first.waitForFunction(id => location.hash.slice(1) === id, {}, homeRoomForSpawn);
    await domClick(first, '#spawn-open');
    await first.waitForFunction(() => document.querySelector('#spawn-form [name=model]')?.value === 'test' && !document.querySelector('#spawn-form button[type=submit]').disabled);
    assert.deepEqual(await first.evaluate(() => ({ ticked: document.querySelector('#spawn-form [name=worktree]').checked, hidden: document.querySelector('.spawn-worktree-option').hidden, base: document.querySelector('#spawn-form [name=base_ref]').value })), { ticked: false, hidden: true, base: '' }, 'the worktree box must start clear, and be hidden outside task rooms');
    // Home room: lead only is on for a new project, so the home room offers the lead roles alone, with the reason.
    assert.deepEqual(await first.evaluate(() => ({ roles: [...document.querySelectorAll('#spawn-form [name=role] option')].map(option => option.value), hint: !document.querySelector('#spawn-home-hint').hidden })),
      { roles: ['lead'], hint: true }, 'the home room spawn dialog offers only lead roles while the project keeps it for the lead');
    // Start stays off until the roles are read. The tool catalog arrives on its own and used to enable Start while the role list of the
    // last opening (here the lead alone) was still in the form, so a stale role could be started. The roles are held back here to see it.
    {
      await domClick(first, '#spawn-cancel');
      await first.evaluate(() => {
        const inner = window.fetch; window.__rolesHeld = true;
        window.fetch = (input, options) => String(input).endsWith('/api/roles') && window.__rolesHeld ? new Promise(resolve => { window.__releaseRoles = () => { window.__rolesHeld = false; window.fetch = inner; resolve(inner(input, options)); }; }) : inner(input, options);
      });
      await domClick(first, '#spawn-open');
      await first.waitForFunction(() => document.querySelector('#spawn-form [name=model]')?.value === 'test');
      assert.deepEqual(await first.evaluate(() => ({ roles: document.querySelectorAll('#spawn-form [name=role] option').length, submit: document.querySelector('#spawn-form button[type=submit]').disabled })), { roles: 0, submit: true }, 'Start is off, and no role from the last opening is left, until the roles are read');
      await first.evaluate(() => window.__releaseRoles());
      await first.waitForFunction(() => document.querySelectorAll('#spawn-form [name=role] option').length === 1 && !document.querySelector('#spawn-form button[type=submit]').disabled);
    }
    await domClick(first, '#spawn-form .dialog-actions button:last-child');
    await first.waitForFunction(() => window.__spawnBodies.length === 2 && !document.querySelector('#spawn-dialog').open);
    assert.deepEqual(await first.evaluate(() => [Object.hasOwn(window.__spawnBodies[1], 'worktree'), Object.hasOwn(window.__spawnBodies[1], 'base_ref'), window.__spawnBodies[1].role_key || window.__spawnBodies[1].role]), [false, false, 'lead'], 'a spawn from a room that is not a task room must not carry a worktree');
    // Turned off (from the home room's Project section), the home room offers every role again; turned back on, only the lead.
    await domClick(first, '#room-info');
    await first.waitForFunction(() => !!document.querySelector('#room-project #project-home-lead-only') && !document.querySelector('#project-home-lead-only').disabled);
    // Renaming: the title is a field of the room's settings and nowhere else. In the home room it is the project's name, so the project form's
    // own Name row is not shown a second time, and a rename reaches the header, the room list and the project.
    {
      const homeTitle = await first.$eval('#room-title', el => el.textContent);
      const form = await first.evaluate(() => ({ shown: !document.querySelector('#room-rename-form').hidden, value: document.querySelector('#room-rename-title').value, nameFact: [...document.querySelectorAll('#room-info-facts dt')].some(dt => dt.textContent === 'Name'),
        projectNameRow: getComputedStyle(document.querySelector('#project-details-name').closest('.settings-row')).display, button: (button => ({ title: button.title, name: button.getAttribute('aria-label'), text: button.textContent.trim(), glyph: !!button.querySelector('.icon-mask-check') }))(document.querySelector('#room-rename-save')) }));
      assert.deepEqual(form, { shown: true, value: homeTitle, nameFact: false, projectNameRow: 'none', button: { title: 'Save title', name: 'Save title', text: '', glyph: true } }, `the home room's settings carry its title as a field: ${JSON.stringify(form)}`);
      await first.$eval('#room-rename-title', el => { el.value = 'Renamed project'; el.dispatchEvent(new Event('input')); });
      await domClick(first, '#room-rename-save');
      await first.waitForFunction(() => document.querySelector('#room-rename-status').textContent === 'Saved.');
      await first.waitForFunction(id => document.querySelector(`[data-room-id="${id}"] .room-name`)?.textContent.includes('Renamed project') && document.querySelector('#room-title').textContent === 'Renamed project', {}, homeRoomForSpawn);
      assert.equal(await first.evaluate(async id => (await (await fetch('/api/projects')).json()).find(project => project.id === id).name, actualProjectID), 'Renamed project', 'renaming the home room renames the project');
      await first.$eval('#room-rename-title', (el, value) => { el.value = value; el.dispatchEvent(new Event('input')); }, homeTitle);
      await domClick(first, '#room-rename-save');
      await first.waitForFunction(title => document.querySelector('#room-title').textContent === title && document.querySelector('#room-rename-status').textContent === 'Saved.', {}, homeTitle);
    }
    assert.equal(await first.$eval('#project-home-lead-only', el => el.checked), true, 'Home room: lead only starts on');
    await domClick(first, '#project-home-lead-only');
    await first.waitForFunction(() => !document.querySelector('#project-home-lead-only').disabled);
    assert.equal(await first.evaluate(async id => (await (await fetch(`/api/projects/${id}/settings`)).json()).home_lead_only, actualProjectID), false, 'the toggle saves the project setting');
    await domClick(first, '#room-info-close');
    await domClick(first, '#spawn-open');
    await first.waitForFunction(() => document.querySelector('#spawn-form [name=model]')?.value === 'test' && document.querySelectorAll('#spawn-form [name=role] option').length > 1);
    assert.deepEqual(await first.evaluate(() => ({ worker: [...document.querySelectorAll('#spawn-form [name=role] option')].some(option => option.value === 'worker'), hint: document.querySelector('#spawn-home-hint').hidden })), { worker: true, hint: true }, 'with the setting off the home room offers every role');
    await domClick(first, '#spawn-cancel');
    await first.evaluate(async id => { const { csrf_token } = await (await fetch('/api/me')).json(); await fetch(`/api/projects/${id}/settings`, { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ home_lead_only: true }) }); }, actualProjectID);
    await domClick(first, `[data-room-id="${roomID}"] .room-name`);
    await first.waitForFunction(id => location.hash.slice(1) === id, {}, roomID);
    // A task room is renamed the same way: the title field, the header and the room list; a refused title is answered under the field.
    {
      await domClick(first, '#room-info');
      await first.waitForFunction(() => !document.querySelector('#room-rename-form').hidden);
      const before = await first.$eval('#room-rename-title', el => el.value);
      await first.$eval('#room-rename-title', el => { el.value = 'Renamed task'; el.dispatchEvent(new Event('input')); });
      await domClick(first, '#room-rename-save');
      await first.waitForFunction(() => document.querySelector('#room-rename-status').textContent === 'Saved.');
      await first.waitForFunction((id, title) => document.querySelector(`[data-room-id="${id}"] .room-name`)?.textContent.includes(title) && document.querySelector('#room-title').textContent === title, {}, roomID, 'Renamed task');
      await first.$eval('#room-rename-title', el => { el.value = '   '; el.dispatchEvent(new Event('input')); });
      await domClick(first, '#room-rename-save');
      await first.waitForFunction(() => document.querySelector('#room-rename-status').textContent === 'A room needs a title.');
      await first.$eval('#room-rename-title', (el, value) => { el.value = value; el.dispatchEvent(new Event('input')); }, before);
      await domClick(first, '#room-rename-save');
      await first.waitForFunction((id, title) => document.querySelector(`[data-room-id="${id}"] .room-name`)?.textContent.includes(title) && document.querySelector('#room-title').textContent === title, {}, roomID, before);
      await domClick(first, '#room-info-close');
    }
    await first.evaluate(() => {
      // Put the page back as it was: the real fetch, and an unticked worktree box (a refused submit leaves the form as it was).
      window.fetch = window.__spawnInner; delete window.__spawnInner; delete window.__spawnBodies;
      const box = document.querySelector('#spawn-form [name=worktree]'); box.checked = false; box.dispatchEvent(new Event('change', { bubbles: true })); document.querySelector('#spawn-form [name=base_ref]').value = '';
    });
    await domClick(first, '#spawn-open');
    await first.waitForFunction(() => document.querySelector('#spawn-form [name=model]')?.value === 'test' && !document.querySelector('#spawn-form button[type=submit]').disabled);
    // A worktree is offered where the project folder is a Git repository (this one is); where the machine says it is not, the box is off
    // with the reason, and a machine that does not say leaves it available.
    assert.deepEqual(await first.evaluate(async () => {
      const { AgentsPanel } = await import('/js/agents.js');
      const form = document.querySelector('#spawn-form'), option = form.querySelector('.spawn-worktree-option');
      const state = () => ({ hidden: option.hidden, disabled: form.elements.worktree.disabled, note: option.querySelector('.spawn-worktree-note').hidden, display: getComputedStyle(option).display });
      const sync = (git, kind = 'task') => { AgentsPanel.prototype.syncWorktreeOption.call({ form, room: { kind }, spawnMachine: { machine: git === undefined ? {} : { project_worktree: git ? { ok: true } : { ok: false, code: 'git_unusable', reason: 'Git cannot use the project folder: detected dubious ownership' } } }, pendingResume: null }); return state(); };
      const live = state(), notRepo = sync(false), reason = option.querySelector('.spawn-worktree-note').textContent, unknown = sync(undefined), home = sync(false, 'home'); sync(undefined);
      return { live, notRepo, reason, unknown, home };
    }), {
      live: { hidden: false, disabled: false, note: true, display: 'flex' }, notRepo: { hidden: false, disabled: true, note: false, display: 'flex' },
      reason: 'No worktree here: Git cannot use the project folder: detected dubious ownership',
      unknown: { hidden: false, disabled: false, note: true, display: 'flex' }, home: { hidden: true, disabled: true, note: false, display: 'none' },
    }, 'the worktree box follows whether the project folder is a Git repository');
    await first.select('#spawn-form [name=tool]', '1');
    assert.match(await first.$eval('#spawn-tool-reason', el => el.textContent), /No agent toolbox: pi-docker has no private credential channel/);
    await first.select('#spawn-form [name=tool]', '0');
    await domClick(first, '#spawn-form .dialog-actions button:last-child');
    await first.waitForFunction(() => !document.querySelector('#spawn-dialog').open);
    await first.waitForFunction(() => document.querySelectorAll('.agent-card[data-agent-id]').length >= 1);
    await first.waitForFunction(() => document.querySelector('#agents-humans .human-card.is-you'));
    assert.equal(await first.$eval('#agents-humans .human-card.is-you', el => el.querySelectorAll('.human-dm').length), 0, 'you cannot DM yourself from the members list');
    {
      const human = await first.$eval('#agents-humans .human-card.is-you', card => {
        const icon = card.querySelector('img[src$="human.svg"]'), name = card.querySelector('.human-name');
        const centre = node => { const rect = node.getBoundingClientRect(); return rect.top + rect.height / 2; };
        return { hasIcon: Boolean(icon), glyph: card.textContent.includes('\u{1F464}'), width: icon?.getBoundingClientRect().width, off: icon && Math.abs(centre(icon) - centre(name)) };
      });
      assert.ok(human.hasIcon && human.width >= 12, `a human card shows the person icon: ${JSON.stringify(human)}`);
      assert.equal(human.glyph, false, 'a human card no longer uses the person emoji');
      assert.ok(human.off <= 1, `the person icon and the name share a centre line: ${human.off}`);
    }
    assert.equal(await first.$eval('#agents-panel h2', el => el.textContent), 'Room members');
    await first.evaluate(() => { window.fetch = window.__originalFetch; });
    const realAgentID = await first.$eval('.agent-card[data-agent-id]', el => el.dataset.agentId);
    const realAgentName = await first.$eval('.agent-card[data-agent-id] .agent-name', el => el.textContent);
    // An AI agent is always written "slug@project" (cards, message headers, the @-list); a person is just the login.
    const realAgentSlug = realAgentName.split('@')[0];
    {
      const projectName = await first.evaluate(async () => (await (await fetch('/api/projects')).json())[0].name);
      assert.equal(realAgentName, `${realAgentSlug}@${projectName}`, 'the member card names the agent with its project');
      assert.equal(await first.$eval('#agents-humans .agent-name, .human-name', el => el.textContent.includes('@')), false, 'a person has no project suffix');
    }
    // The profile of a real agent carries what the server knows beyond the card.
    await domClick(first, `[data-agent-id="${realAgentID}"] .agent-name`);
    await first.waitForFunction(id => [...document.querySelectorAll(`[data-agent-id="${id}"] .profile-fact-label`)].some(el => el.textContent === 'CWD'), {}, realAgentID);
    const realFacts = await first.$$eval(`[data-agent-id="${realAgentID}"] .profile-fact`, rows => Object.fromEntries(rows.map(row => [row.querySelector('.profile-fact-label').textContent, row.querySelector('.profile-fact-value').textContent])));
    for (const label of ['Machine', 'Project', 'Spawned', 'CWD', 'Agent ID']) assert.ok(realFacts[label], `the real profile lacks ${label}: ${JSON.stringify(realFacts)}`);
    assert.equal(realFacts['Agent ID'], realAgentID);
    await domClick(first, '#agent-profile-close');
    // Room settings lists who is in the room, read-only: the people with their project role, the spawned agent with its role
    // emoji and label, and each singleton role with its holder. Server text stays inert.
    {
      await domClick(first, '#room-info');
      await first.waitForFunction(() => document.querySelector('#room-info-dialog').open && document.querySelector('#room-member-list .room-members-agents li'));
      const listed = await first.evaluate(() => {
        const root = document.querySelector('#room-member-list');
        const texts = selector => [...root.querySelectorAll(selector)].map(item => item.textContent.replace(/\s+/g, ' ').trim());
        return { people: texts('.room-members-people li'), agents: texts('.room-members-agents li'), singletons: texts('.room-members-singletons li'),
          headings: texts('.room-members-heading'), controls: root.querySelectorAll('input, select, textarea, button, a').length,
          emojiHidden: [...root.querySelectorAll('.room-member-emoji')].every(item => item.getAttribute('aria-hidden') === 'true'), status: root.querySelector('.room-members-status').textContent,
          lists: [...root.querySelectorAll('ul')].map(list => list.getAttribute('aria-label')) };
      });
      assert.ok(listed.people.some(text => text.startsWith('owner') && text.includes('owner')), `people: ${JSON.stringify(listed.people)}`);
      assert.ok(listed.agents.some(text => text.startsWith(realAgentSlug) || text.includes(` ${realAgentSlug} `) || text.includes(realAgentSlug)), `agents: ${JSON.stringify(listed.agents)} (agent ${realAgentSlug})`);
      assert.ok(listed.agents.every(text => /Active|Stopped|Not accepting|Interrupt|Revoked/.test(text)), `agents carry their state: ${JSON.stringify(listed.agents)}`);
      assert.ok(listed.singletons.some(text => /Lead/.test(text) && /held by|vacant/.test(text)), `singleton roles: ${JSON.stringify(listed.singletons)}`);
      assert.equal(listed.controls, 0, 'the member list is read-only: no controls');
      assert.equal(listed.emojiHidden, true);
      assert.equal(listed.status, '');
      assert.deepEqual(listed.lists, ['People in this room', 'Agents in this room', 'Singleton roles']);
      // Hostile names stay text, and a slow answer for a room that was reset is dropped.
      const inert = await first.evaluate(async () => {
        const { RoomMembers } = await import('/js/room-members.js');
        const root = document.createElement('section'); root.innerHTML = '<p class="room-members-status"></p><div class="room-members-body"></div>'; document.body.append(root);
        const view = new RoomMembers(root);
        view.render({ humans: [{ id: 'u', login: '<img src=x onerror=window.__memberXss=1>', role: '<b>owner</b>' }], agents: [{ id: 'a', slug: '<i>slug</i>', role_key: 'k', role_label: '<u>Role</u>', role_emoji: '<s>x</s>', singleton: true, desired: 'active', observed: 'idle' }], singletons: [{ role_key: 'k', label: '<em>Lead</em>', emoji: '', held_by: { agent_id: 'a', slug: '<i>slug</i>', in_this_room: false } }] });
        const out = { markup: root.querySelectorAll('img, b, i, u, s, em').length, xss: window.__memberXss || 0, held: root.querySelector('.room-members-singletons .room-member-state').textContent };
        const original = window.fetch; let release;
        window.fetch = (input, options) => String(input).endsWith('/members') ? new Promise(resolve => { release = () => resolve(new Response(JSON.stringify({ humans: [{ id: 'late', login: 'late', role: 'viewer' }], agents: [], singletons: [] }), { headers: { 'Content-Type': 'application/json' } })); }) : original(input, options);
        const pending = view.load('room_x'); view.reset(); release(); await pending;
        window.fetch = original; out.lateRendered = root.textContent.includes('late'); out.statusAfterReset = root.querySelector('.room-members-status').textContent;
        root.remove(); return out;
      });
      assert.deepEqual(inert, { markup: 0, xss: 0, held: 'held by <i>slug</i> (another room)', lateRendered: false, statusAfterReset: '' }, 'member list rendered server text as markup, or a stale answer rendered');
      await domClick(first, '#room-info-close');
      await first.waitForFunction(() => !document.querySelector('#room-info-dialog').open);
    }
    await first.waitForSelector('.system-notice .system-row time');
    assert.equal(await first.$eval('.system-notice .system-row', el => !!el.querySelector('.message-body') && !!el.querySelector('time')), true);
    assert.equal(await first.$$eval('.system-notice .message-actions', els => els.length), 0, 'system notice exposes message actions');
    // t2-system-events: the bell feed and the message debug inspector.
    assert.equal(await first.$eval('#system-events-toggle', el => !el.hidden), true, 'system events bell is on by default');
    await domClick(first, '#system-events-toggle');
    await first.waitForFunction(() => !document.querySelector('#system-events-popover').hidden && document.querySelectorAll('#system-events-popover .system-event-row').length > 0);
    assert.equal(await first.$eval('#system-events-popover .system-event-row .system-event-text', el => el.textContent.length > 0), true);
    assert.deepEqual(await first.$$eval('#system-events-popover > header button', els => els.map(el => el.textContent.trim())), [], 'the feed has no button of its own to hide the bell; Settings -> General is the only switch');
    await first.keyboard.press('Escape');
    await first.waitForFunction(() => document.querySelector('#system-events-popover').hidden);
    assert.equal(await first.$eval('.message:not(.system-notice) .chat-msg-debug-toggle', el => getComputedStyle(el).display), 'none', 'debug button is off by default');
    await openSettings(first);
    await domClick(first, '#settings-general-tab');
    await setChecked(first, '#pref-debug-inspector', true);
    await domClick(first, '#settings-close');
    assert.notEqual(await first.$eval('.message:not(.system-notice) .chat-msg-debug-toggle', el => getComputedStyle(el).display), 'none', 'debug button follows the preference');
    const debugMessageID = await first.$eval('.message:not(.system-notice)', el => el.dataset.messageId);
    // v1's header line: sender, #id, then the bug icon, then the time; the bug opens the inspector.
    assert.deepEqual(await first.$eval('.message:not(.system-notice) .message-meta', el => { const bug = el.querySelector('.chat-msg-debug-toggle'); return { order: [...el.children].map(child => child.classList.contains('message-number') ? 'id' : child.classList.contains('chat-msg-debug-toggle') ? 'bug' : child.tagName === 'TIME' ? 'time' : 'other').filter(item => item !== 'other'), label: bug.getAttribute('aria-label'), title: bug.title, text: bug.textContent, mask: getComputedStyle(bug.querySelector('.icon-mask-bug')).maskImage.includes('/icons/bug.svg') || getComputedStyle(bug.querySelector('.icon-mask-bug')).webkitMaskImage.includes('/icons/bug.svg') }; }), { order: ['id', 'bug', 'time'], label: 'Open debug inspector', title: 'Open debug inspector', text: '', mask: true });
    // The #number and the bug are quiet in the header: no box or outline at rest or on hover, only a colour change; another
    // row button elsewhere still gets its hover border.
    // The hover state is forced through the DevTools protocol rather than by moving the mouse: a headless window's :hover depends
    // on window focus and pointer state, which is not something a style check should wait on.
    const quietLook = async (selector, rest) => {
      await first.$eval(selector, el => { el.scrollIntoView({ block: 'center', behavior: 'instant' }); });
      await new Promise(resolve => setTimeout(resolve, 150));
      const client = await first.createCDPSession();
      try {
        await client.send('DOM.enable'); await client.send('CSS.enable');
        const { root } = await client.send('DOM.getDocument', { depth: 0 });
        const { nodeId } = await client.send('DOM.querySelector', { nodeId: root.nodeId, selector });
        await client.send('CSS.forcePseudoState', { nodeId, forcedPseudoClasses: ['hover'] });
        await first.waitForFunction((target, color) => getComputedStyle(document.querySelector(target)).color !== color, { timeout: 5000 }, selector, rest).catch(() => {});
        await new Promise(resolve => setTimeout(resolve, 300));
        const look = await first.$eval(selector, el => { const cs = getComputedStyle(el); return { borderColor: cs.borderTopColor, background: cs.backgroundColor, outline: cs.outlineStyle, color: cs.color, hovered: el.matches(':hover') }; });
        await client.send('CSS.forcePseudoState', { nodeId, forcedPseudoClasses: [] });
        return look;
      } finally { await client.detach().catch(() => {}); }
    };
    const restColor = selector => first.$eval(selector, el => getComputedStyle(el).color);
    // The newest message sits at the bottom of the list, which stays put under the pointer.
    await first.evaluate(() => { const rows = [...document.querySelectorAll('#messages .message:not(.system-notice)')]; document.querySelectorAll('[data-quiet-probe]').forEach(node => delete node.dataset.quietProbe); rows.at(-1).dataset.quietProbe = '1'; });
    // A system notice carries the same bug icon in its own row; it must be as quiet as the header's.
    await first.evaluate(() => { document.querySelectorAll('[data-quiet-notice]').forEach(node => delete node.dataset.quietNotice); const notice = [...document.querySelectorAll('#messages .system-notice')].at(-1); if (notice) notice.dataset.quietNotice = '1'; });
    assert.equal(await first.$('[data-quiet-notice] .system-row .chat-msg-debug-toggle') !== null, true, 'a system notice has a bug icon to check');
    for (const theme of ['', 'high-contrast-dark', 'high-contrast-light']) {
      await first.evaluate(theme => { if (theme) document.documentElement.dataset.theme = theme; else delete document.documentElement.dataset.theme; }, theme);
      await new Promise(resolve => setTimeout(resolve, 400)); // past the colour transition
      for (const selector of ['[data-quiet-probe] .message-meta .message-number', '[data-quiet-probe] .message-meta .chat-msg-debug-toggle', '[data-quiet-notice] .system-row .chat-msg-debug-toggle']) {
        await first.mouse.move(2, 2);
        // At rest the #id and the bug have no border at all and no fill, in every theme.
        const resting = await first.$eval(selector, el => { const cs = getComputedStyle(el); return { width: cs.borderTopWidth, background: cs.backgroundColor, shadow: cs.boxShadow }; });
        assert.deepEqual(resting, { width: '0px', background: 'rgba(0, 0, 0, 0)', shadow: 'none' }, `${selector} has a border or fill at rest in ${theme || 'the default theme'}: ${JSON.stringify(resting)}`);
        const rest = await restColor(selector);
        const hovered = await quietLook(selector, rest);
        assert.deepEqual([hovered.borderColor, hovered.background, hovered.outline], ['rgba(0, 0, 0, 0)', 'rgba(0, 0, 0, 0)', 'none'], `${selector} shows a box or outline on hover in ${theme || 'the default theme'}: ${JSON.stringify(hovered)}`);
        assert.notEqual(hovered.color, rest, `${selector} gives no hover tint in ${theme || 'the default theme'}: ${JSON.stringify(hovered)}`);
      }
    }
    await first.evaluate(() => { delete document.documentElement.dataset.theme; });
    await first.mouse.move(2, 2);
    const otherRowButton = await first.$('#projects .room-collapse');
    if (otherRowButton) {
      await otherRowButton.hover(); await new Promise(resolve => setTimeout(resolve, 350));
      assert.notEqual(await first.$eval('#projects .room-collapse', el => getComputedStyle(el).borderTopColor), 'rgba(0, 0, 0, 0)', 'other row buttons lost their hover border');
      await first.mouse.move(2, 2);
    }
    await domClick(first, '.message:not(.system-notice) .chat-msg-debug-toggle');
    await first.waitForFunction(() => document.querySelector('#message-debug-dialog')?.open && document.querySelector('#message-debug-dialog .message-debug-raw pre')?.textContent.length > 0);
    const debugText = await first.$eval('#message-debug-dialog', el => el.textContent);
    assert.ok(debugText.includes(debugMessageID) && debugText.includes('Wake and delivery') && debugText.includes('Addressed to'), 'inspector shows the record and delivery sections');
    assert.ok(debugText.includes('Addressing trace'), 'inspector explains addressing');
    // v1's previous/next walk the room's messages; the actions the bubble leaves out in style A live in the inspector.
    const inspectorNav = await first.evaluate(() => { const dialog = document.querySelector('#message-debug-dialog'); return { position: dialog.querySelector('.message-debug-position').textContent, labels: [...dialog.querySelectorAll('header button:not([hidden])')].map(button => button.getAttribute('aria-label') || button.textContent) }; });
    assert.match(inspectorNav.position, /^\d+ \/ \d+$/); assert.deepEqual(inspectorNav.labels, ['Previous message', 'Next message', 'Reply in thread', 'Copy text', 'Copy link', 'Copy JSON', 'Close message inspector']);
    if (!/^1 \/ 1$/.test(inspectorNav.position)) {
      const before = await first.$eval('#message-debug-title', el => el.textContent);
      await first.evaluate(() => { const dialog = document.querySelector('#message-debug-dialog'); (dialog.querySelector('button[aria-label="Next message"]:not(:disabled)') || dialog.querySelector('button[aria-label="Previous message"]:not(:disabled)')).click(); });
      await first.waitForFunction(was => document.querySelector('#message-debug-title').textContent !== was, {}, before);
    }
    await domClick(first, '#message-debug-close');
    await first.waitForFunction(() => !document.querySelector('#message-debug-dialog').open);
    // t2-debug-addressing-trace: the inspector explains each @token. The server paths are covered by Go
    // tests; this pins how the dialog renders ignored, ambiguous, unmatched, and drifted mentions, and that
    // hostile names stay inert.
    const traceUI = await first.evaluate(async () => {
      const { MessageDebug } = await import('/js/message-debug.js');
      const original = window.fetch;
      const payload = {
        message: { id: 'msg_trace', seq: 7, no: 7, room_id: 'room_x', author_kind: 'user', author_id: 'usr_x', body: '`@ghost` @bob @nobody @ada', created_at: '2026-10-16T12:00:00Z' },
        addressed_to: ['agt_ada'],
        addressing: {
          mentions: [
            { token: 'ghost', kind: 'ignored', outcome: 'ignored', reason: 'written inside backticks or a code block, or escaped with a backslash, so it is shown literally and addresses nobody', reached: [], would_reach: [{ id: 'agt_ada', name: 'ada', kind: 'agent' }] },
            { token: 'bob', kind: 'slug', outcome: 'ambiguous', reason: '2 room members share the slug "bob", so nobody is addressed', reached: [{ id: 'agt_1', name: 'bob', kind: 'agent' }, { id: 'agt_2', name: '<img src=x onerror=window.__debugXss=1>', kind: 'agent' }] },
            { token: 'nobody', kind: 'none', outcome: 'no_match', reason: 'no room member, role key, or group tag has this name', reached: [] },
            { token: 'ada', kind: 'slug', outcome: 'addressed', reason: 'exact slug of a room member', reached: [{ id: 'agt_ada', name: 'ada', kind: 'agent' }] },
          ],
          sources: ['reply to a message by casey: a reply addresses its parent’s author'],
          recorded_targets: ['agt_ada'], resolved_now: ['agt_1', 'agt_ada'], drift: true,
        },
        notes: [], events: [],
      };
      window.fetch = async (url, init) => String(url).includes('/debug') ? new Response(JSON.stringify(payload), { status: 200, headers: { 'Content-Type': 'application/json' } }) : original(url, init);
      const inspector = new MessageDebug();
      try {
        await inspector.open({ id: 'msg_trace', seq: 7, no: 7 });
        const dialog = inspector.dialog;
        const row = token => dialog.querySelector(`.message-debug-mention[data-token="${token}"]`);
        return {
          outcomes: Object.fromEntries(['ghost', 'bob', 'nobody', 'ada'].map(token => [token, row(token)?.dataset.outcome])),
          ghostReason: row('ghost')?.querySelector('.message-debug-reason')?.textContent || '',
          ghostWould: row('ghost')?.querySelector('.message-debug-reached')?.textContent || '',
          bobReached: row('bob')?.querySelector('.message-debug-reached')?.textContent || '',
          adaReached: row('ada')?.querySelector('.message-debug-reached')?.textContent || '',
          source: dialog.querySelector('.message-debug-source')?.textContent || '',
          drift: [...dialog.querySelectorAll('.message-debug-skip')].some(el => el.textContent.includes('membership has changed')),
          injectedImages: dialog.querySelectorAll('img').length, xss: window.__debugXss === 1,
        };
      } finally {
        window.fetch = original; inspector.dialog.close(); inspector.dialog.remove();
      }
    });
    assert.deepEqual(traceUI.outcomes, { ghost: 'ignored', bob: 'ambiguous', nobody: 'no_match', ada: 'addressed' }, 'each token shows its outcome');
    assert.ok(traceUI.ghostReason.includes('backticks'), 'a backticked mention says why it addressed nobody');
    assert.equal(traceUI.ghostWould, 'Would have reached: ada', 'an ignored mention says who it would have reached');
    assert.ok(traceUI.bobReached.startsWith('Candidates: bob, '), 'an ambiguous slug lists its candidates');
    assert.ok(traceUI.adaReached.startsWith('Reaches: ada'), 'an addressed token lists who it reached');
    assert.ok(traceUI.source.includes('reply to a message by casey'), 'reply sources are listed');
    assert.equal(traceUI.drift, true, 'drift is called out');
    assert.equal(traceUI.injectedImages, 0, 'a hostile name must not become markup');
    assert.equal(traceUI.xss, false, 'a hostile name must not run');
    await openSettings(first);
    await domClick(first, '#settings-general-tab');
    await setChecked(first, '#pref-system-events', false);
    await setChecked(first, '#pref-debug-inspector', false);
    await domClick(first, '#settings-close');
    assert.equal(await first.$eval('#system-events-toggle', el => el.hidden), true, 'bell hides when system events are turned off');
    await openSettings(first);
    await domClick(first, '#settings-general-tab');
    await setChecked(first, '#pref-system-events', true);
    await domClick(first, '#settings-close');
    // t0-web-small: relative timestamps, notification gate, panel persistence, reactions, drag class.
    await first.waitForSelector('.message time[data-relative]');
    const stamp = await first.$eval('.message time[data-relative]', el => ({ text: el.textContent, title: el.title, iso: el.dateTime }));
    assert.match(stamp.text, /^(just now|\d+[smhd] ago|\d{1,2}[./]\d{1,2}[./]\d{2,4})$/, `message time is not relative: ${stamp.text}`);
    assert.ok(stamp.title.length > 8 && stamp.iso, 'message time lacks a full-date tooltip');
    assert.equal(await first.evaluate(async () => {
      const { relativeTime } = await import('/js/time.js');
      const ago = ms => relativeTime(new Date(Date.now() - ms).toISOString());
      return [ago(2000), ago(30000), ago(5 * 60000), ago(3 * 3600000), ago(2 * 86400000)].join('|');
    }), 'just now|30s ago|5m ago|3h ago|2d ago');
    assert.equal(await first.evaluate(async () => {
      const { InboxNotifications } = await import('/js/inbox-notifications.js');
      return new InboxNotifications(() => {}).available === (window.isSecureContext && 'Notification' in window);
    }), true, 'notification availability is not gated on isSecureContext');
    assert.equal(await first.$$eval('.quick-reaction', els => [...new Set(els.map(el => el.textContent))].join('')), '👍🆗✅👀🙏🎉');
    await first.evaluate(() => { const dt = new DataTransfer(); dt.items.add(new File(['x'], 'x.txt')); document.querySelector('#composer').dispatchEvent(new DragEvent('dragover', { bubbles: true, cancelable: true, dataTransfer: dt })); });
    assert.equal(await first.$eval('#composer', el => el.classList.contains('drag-attachment')), true, 'composer did not get the drag-over class');
    await first.evaluate(() => document.querySelector('#composer').dispatchEvent(new DragEvent('dragleave', { bubbles: true })));
    assert.equal(await first.$eval('#composer', el => el.classList.contains('drag-attachment')), false, 'drag-over class stuck after dragleave');
    if (!(await first.$eval('#app-view', el => el.classList.contains('rooms-collapsed')))) await first.click('#edge-rooms');
    if (!(await first.$eval('.chat', el => el.classList.contains('agents-collapsed')))) await first.click('#edge-agents');
    assert.deepEqual(await first.evaluate(() => [localStorage.getItem('aimebu_left_collapsed'), localStorage.getItem('aimebu_right_collapsed')]), ['true', 'true']);
    const restored = await a.newPage(); await restored.setViewport({ width: 1100, height: 720 });
    await restored.goto(origin);
    await appReady(restored);
    assert.equal(await restored.evaluate(() => document.querySelector('#app-view').classList.contains('rooms-collapsed') && document.querySelector('.chat').classList.contains('agents-collapsed')), true, 'panel collapse was not restored on a fresh page');
    await restored.close();
    await first.click('#edge-agents-expand');
    assert.deepEqual(await first.evaluate(() => [localStorage.getItem('aimebu_left_collapsed'), localStorage.getItem('aimebu_right_collapsed')]), ['true', 'false']);
    // t2-scroll-anchor: late-growing content (image + Mermaid) must keep a pinned list pinned and must not move an unpinned one.
    const anchorResult = await first.evaluate(async () => {
      const { renderVisualPlan } = await import('/js/visual-plan.js');
      const list = document.querySelector('#messages');
      const wait = ms => new Promise(resolve => setTimeout(resolve, ms));
      // Wait for a condition instead of a fixed delay: late content takes arbitrarily long on a busy machine.
      const until = async (condition, ms = 10000) => { for (const start = Date.now(); Date.now() - start < ms && !condition(); ) await wait(50); };
      const tall = 'data:image/svg+xml,' + encodeURIComponent('<svg xmlns="http://www.w3.org/2000/svg" width="300" height="400"><rect width="300" height="400" fill="#888"/></svg>');
      const made = [];
      const make = (html = '') => { const el = document.createElement('article'); el.className = 'message'; el.dataset.anchorTest = '1'; el.innerHTML = html; made.push(el); return el; };
      const filler = () => make('<div style="height:120px">filler</div>');
      const out = {};
      // Decision log for a failing run (flaky under load): ScrollAnchor appends to window.__anchorLog, and every write to the
      // list's scrollTop is recorded with its caller, so a failure shows who moved the list and what the anchor decided.
      const log = window.__anchorLog = [];
      const note = (ev, info = {}) => log.push({ t: Math.round(performance.now()), ev, st: Math.round(list.scrollTop * 10) / 10, sh: list.scrollHeight, ch: list.clientHeight, ...info });
      const proto = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollTop');
      Object.defineProperty(list, 'scrollTop', { configurable: true, get() { return proto.get.call(this); }, set(value) {
        note('write', { to: Math.round(value * 10) / 10, by: (new Error().stack.split('\n')[2] || '').trim().replace(/^at /, '').slice(-60) }); proto.set.call(this, value);
      } });
      try {
        // Pinned: a message whose image and diagram load late must leave the list at the bottom.
        list.scrollTop = list.scrollHeight; await wait(100);
        const late = make('<img alt="late" style="display:block">');
        const plan = renderVisualPlan({ visual_plan: [{ type: 'diagram', data: { mermaid: 'flowchart TD\n A["Start"] --> B["Middle"] --> C["Done"]' } }] });
        if (plan) late.append(plan);
        list.append(late); list.scrollTop = list.scrollHeight; await wait(100);
        late.querySelector('img').src = tall;
        await until(() => late.querySelector('img').complete && late.querySelector('svg')); await wait(200);
        out.pinnedGap = list.scrollHeight - list.scrollTop - list.clientHeight;
        out.lateHeight = late.getBoundingClientRect().height;
        // Not pinned: growth above the viewport must not shift what the reader sees; growth below must not move it.
        const above = make('<img alt="above" style="display:block">');
        list.prepend(above); for (let i = 0; i < 12; i++) list.prepend(filler());
        const mid = filler(); list.insertBefore(mid, late);
        // Buffer right above mid: the viewport edge, 40px above mid, must fall inside it and not in `above`. With few or no
        // real messages in the list (it varies with the suite's history) `above` itself would be the first visible
        // message, and its own growth then correctly pushes everything below it down.
        for (let i = 0; i < 3; i++) list.insertBefore(filler(), mid);
        // Not scrollTop 0: within 80px of the top the list asks for the older page (the suite's room has more history), and when
        // that page arrives it is compensated for by moving scrollTop by the height it inserted. That is correct, but this test
        // measures scrollTop and element positions, so a page that lands mid-measurement (a busy machine) reads as a jump.
        list.scrollTop = 120; await wait(100);
        list.scrollTop = mid.offsetTop - list.offsetTop - 40; await wait(150);
        const before = mid.getBoundingClientRect().top;
        note('grow-above', { before: Math.round(before), items: list.querySelectorAll('.message').length, aboveH: above.offsetHeight });
        above.querySelector('img').src = tall; await until(() => above.querySelector('img').complete); await wait(200);
        out.shift = Math.abs(mid.getBoundingClientRect().top - before);
        note('grown-above', { after: Math.round(mid.getBoundingClientRect().top), aboveH: above.offsetHeight, shift: Math.round(out.shift) });
        if (out.shift >= 2) out.log = log.filter(entry => !(entry.ev === 'scroll' && entry.echo)).slice(-120);
        const top = list.scrollTop; late.querySelector('img').style.height = '900px'; await wait(300);
        out.belowMoved = Math.abs(list.scrollTop - top);
      } finally { delete list.scrollTop; delete window.__anchorLog; for (const el of made) el.remove(); list.scrollTop = list.scrollHeight; }
      return out;
    });
    assert.equal(await first.$eval('#messages', el => getComputedStyle(el).overflowAnchor), 'none', 'the message list must not use the browser\'s scroll anchoring: ScrollAnchor compensates itself and the two would fight');
    assert.ok(anchorResult.lateHeight > 400, `late content did not grow: ${JSON.stringify(anchorResult)}`);
    assert.ok(anchorResult.pinnedGap < 50, `pinned list drifted off the bottom: ${JSON.stringify(anchorResult)}`);
    assert.ok(anchorResult.shift < 2, `unpinned list jumped when content above grew: ${JSON.stringify({ ...anchorResult, log: undefined })}${anchorResult.log ? '\nScrollAnchor decision log (last 120, scroll echoes left out; reading the layout to log can itself shift timing, so a 379 that vanishes with this on is a clue too):\n' + anchorResult.log.map(entry => JSON.stringify(entry)).join('\n') : ''}`);
    assert.ok(anchorResult.belowMoved < 2, `unpinned list moved when content below grew: ${JSON.stringify(anchorResult)}`);
    // The scroll event for a write arrives a frame later. If content grows in between, that event must not
    // re-derive the state from the new layout (a pinned list would be read as "scrolled up" and stop following).
    const lateScrollEvent = await first.evaluate(async () => {
      const { ScrollAnchor } = await import('/js/scroll-anchor.js');
      const box = document.createElement('div'); box.style.cssText = 'position:fixed;left:0;top:0;width:200px;height:200px;overflow-y:auto;visibility:hidden;overflow-anchor:none'; // like .messages: the browser's own scroll anchoring must not move scrollTop too
      const items = [0, 1, 2].map(() => { const item = document.createElement('article'); item.className = 'message'; item.style.height = '100px'; box.append(item); return item; });
      document.body.append(box);
      const frames = n => new Promise(resolve => { const step = () => n-- > 0 ? requestAnimationFrame(step) : resolve(); step(); });
      try {
        const anchor = new ScrollAnchor(box);
        await frames(2);
        box.scrollTop = box.scrollHeight; anchor.update();
        items[2].style.height = '600px';
        box.dispatchEvent(new Event('scroll')); // the late event for the write above, seen against the grown layout
        await frames(4);
        return { pinned: anchor.pinned, gap: box.scrollHeight - box.scrollTop - box.clientHeight };
      } finally { box.remove(); }
    });
    assert.deepEqual(lateScrollEvent, { pinned: true, gap: 0 }, 'a late scroll event re-derived the pinned state from the grown layout');
    // An explicit update() (the chat calls it after every render) that lands after late content grew but before
    // the resize callback must neither swallow the growth above a scrolled-up reader nor unpin a pinned list.
    const updateBeforeResize = await first.evaluate(async () => {
      const { ScrollAnchor } = await import('/js/scroll-anchor.js');
      const make = () => {
        const box = document.createElement('div'); box.style.cssText = 'position:fixed;left:0;top:0;width:200px;height:200px;overflow-y:auto;visibility:hidden;overflow-anchor:none'; // like .messages: the browser's own scroll anchoring must not move scrollTop too
        const items = [0, 1, 2, 3, 4, 5].map(() => { const item = document.createElement('article'); item.className = 'message'; item.style.height = '100px'; box.append(item); return item; });
        document.body.append(box);
        return { box, items };
      };
      const frames = n => new Promise(resolve => { const step = () => n-- > 0 ? requestAnimationFrame(step) : resolve(); step(); });
      const out = {};
      let { box, items } = make();
      try {
        const anchor = new ScrollAnchor(box);
        await frames(2);
        box.scrollTop = 250; anchor.update(true);
        const before = items[2].getBoundingClientRect().top - box.getBoundingClientRect().top;
        items[0].style.height = '400px';
        anchor.update(); // lands before the resize callback
        await frames(4);
        out.shift = Math.round(items[2].getBoundingClientRect().top - box.getBoundingClientRect().top - before);
      } finally { box.remove(); }
      ({ box, items } = make());
      try {
        const anchor = new ScrollAnchor(box);
        await frames(2);
        box.scrollTop = box.scrollHeight; anchor.update(true);
        items[5].style.height = '400px';
        anchor.update();
        await frames(4);
        out.pinned = anchor.pinned; out.gap = box.scrollHeight - box.scrollTop - box.clientHeight;
      } finally { box.remove(); }
      // A programmatic jump away from a pinned list whose last message just grew, followed by update(), must stay put.
      ({ box, items } = make());
      try {
        const anchor = new ScrollAnchor(box);
        await frames(2);
        box.scrollTop = box.scrollHeight; anchor.update(true);
        items[5].style.height = '400px';
        box.scrollTop = 100; // the jump
        anchor.update();
        await frames(4);
        out.jumpTop = box.scrollTop; out.jumpPinned = anchor.pinned;
      } finally { box.remove(); }
      return out;
    });
    assert.deepEqual(updateBeforeResize, { jumpTop: 100, jumpPinned: false, shift: 0, pinned: true, gap: 0 }, 'an update() between late growth and the resize callback swallowed the growth');
    // A message inserted above the reader whose image finishes loading before the first frame after its insertion
    // (a busy machine delays that frame by hundreds of ms), with the reader's late scroll event landing in that gap,
    // must still have its growth compensated: its starting height is recorded at insertion, not at the first resize callback.
    const growthBeforeFirstFrame = await first.evaluate(async () => {
      const { ScrollAnchor } = await import('/js/scroll-anchor.js');
      const box = document.createElement('div'); box.style.cssText = 'position:fixed;left:0;top:0;width:200px;height:200px;overflow-y:auto;overflow-anchor:none;visibility:hidden';
      const make = () => { const item = document.createElement('article'); item.className = 'message'; item.style.height = '100px'; return item; };
      const items = [0, 1, 2, 3, 4, 5].map(() => { const item = make(); box.append(item); return item; });
      document.body.append(box);
      const frames = n => new Promise(resolve => { const step = () => n-- > 0 ? requestAnimationFrame(step) : resolve(); step(); });
      try {
        const anchor = new ScrollAnchor(box);
        await frames(3);
        box.scrollTop = 250; anchor.update(true);
        const before = items[3].getBoundingClientRect().top - box.getBoundingClientRect().top;
        const arrival = make(); box.insertBefore(arrival, items[0]); // above the reader
        const scrollTopAfterInsert = box.scrollTop;
        await Promise.resolve(); await Promise.resolve(); // mutation observers run; no frame has been rendered yet
        arrival.style.height = '500px'; // the late image
        anchor.update(true); // the scroll event for the reader's last write, delivered late: still no frame since the insertion
        await frames(4);
        return { grew: arrival.offsetHeight, shift: Math.round(items[3].getBoundingClientRect().top - box.getBoundingClientRect().top - before - 100), movedByInsert: box.scrollTop - scrollTopAfterInsert };
      } finally { box.remove(); }
    });
    assert.deepEqual(growthBeforeFirstFrame, { grew: 500, shift: 0, movedByInsert: 400 }, 'growth of a message before its first resize callback was not compensated');
    // Several messages above an unpinned viewport grow, one after a late update(), then growth below and inside the view:
    // only growth above the reader may move the list.
    const lateUpdate = await first.evaluate(async () => {
      const { ScrollAnchor } = await import('/js/scroll-anchor.js');
      const box = document.createElement('div'); box.style.cssText = 'position:fixed;left:0;top:0;width:200px;height:200px;overflow-y:auto;overflow-anchor:none;visibility:hidden';
      const items = Array.from({ length: 8 }, () => { const item = document.createElement('article'); item.className = 'message'; item.style.height = '100px'; box.append(item); return item; });
      document.body.append(box);
      const frames = n => new Promise(resolve => { const step = () => n-- > 0 ? requestAnimationFrame(step) : resolve(); step(); });
      const topOf = item => Math.round(item.getBoundingClientRect().top - box.getBoundingClientRect().top);
      const result = {};
      try {
        const anchor = new ScrollAnchor(box);
        await frames(2);
        box.scrollTop = 300; await frames(3); anchor.update(); // items 3 and 4 are in view, items 0-2 are above
        const view = topOf(items[3]);
        items[0].style.height = '500px'; // grows above the viewport
        anchor.update();                  // a late update, after the growth and before the observer runs
        await frames(4);
        result.lateUpdateShift = topOf(items[3]) - view;
        items[1].style.height = '300px'; items[2].style.height = '250px'; // two more above, in one batch
        await frames(4);
        result.batchShift = topOf(items[3]) - view;
        items[6].style.height = '700px'; // below the viewport: the view must not move
        await frames(4);
        result.belowShift = topOf(items[3]) - view;
        items[3].style.height = '180px'; // the message in view itself grows: its top stays, nothing is compensated
        await frames(4);
        result.ownShift = topOf(items[3]) - view;
        return result;
      } finally { box.remove(); }
    });
    assert.deepEqual(lateUpdate, { lateUpdateShift: 0, batchShift: 0, belowShift: 0, ownShift: 0 }, `unpinned view moved when messages above it grew: ${JSON.stringify(lateUpdate)}`);
    await second.waitForFunction(id => !!document.querySelector(`[data-agent-id="${id}"]`), {}, realAgentID);
    await first.waitForFunction(id => document.querySelector(`[data-agent-id="${id}"] .agent-state-label`)?.textContent === 'idle', {}, realAgentID);
    await openSettings(first);
    await domClick(first, '#settings-general-tab');
    await first.select('#theme-select', 'high-contrast-dark');
    await domClick(first, '#settings-close');
    const hc = await first.evaluate(id => {
      const css = selector => getComputedStyle(document.querySelector(selector));
      const rgb = value => value.match(/[\d.]+/g).slice(0, 3).map(Number);
      const luminance = value => rgb(value).map(n => n / 255).map(n => n <= .04045 ? n / 12.92 : ((n + .055) / 1.055) ** 2.4)
        .reduce((sum, n, i) => sum + n * [.2126, .7152, .0722][i], 0);
      const contrast = (foreground, background) => { const a = luminance(foreground), b = luminance(background); return (Math.max(a, b) + .05) / (Math.min(a, b) + .05); };
      return {
        roomBorder: css('.room-row.active .room-name').borderTopWidth,
        roomNameBackground: css('.room-row.active .room-name').backgroundColor,
        roomNameColor: css('.room-row.active .room-name').color,
        roomNameSize: css('.room-row.active .room-name').fontSize,
        roomNamePadding: css('.room-row.active .room-name').padding,
        roomBackground: css('.room-row.active').backgroundColor,
        selfBorder: css('.message.self .message-bubble').borderTopColor,
        stateBorder: css(`[data-agent-id="${id}"] .agent-state`).borderTopWidth,
        stateRadius: css(`[data-agent-id="${id}"] .agent-state`).borderRadius,
        messageContrast: contrast(css('.message.self .message-body').color, css('.message.self .message-bubble').backgroundColor),
        mutedContrast: contrast(css(`[data-agent-id="${id}"] .agent-meta`).color, css(`[data-agent-id="${id}"]`).backgroundColor),
      };
    }, realAgentID);
    assert.equal(hc.roomBorder, '0px', 'high contrast room name is a framed button');
    assert.equal(hc.roomNameBackground, 'rgba(0, 0, 0, 0)', 'room name should be plain text');
    assert.equal(hc.roomNameColor, 'rgb(255, 255, 255)', 'high contrast room name should use white text');
    assert.equal(hc.roomNameSize, '11.9px'); assert.equal(hc.roomNamePadding, '0px');
    assert.equal(hc.selfBorder, 'rgb(111, 195, 223)', 'own message border is too dark');
    assert.equal(hc.stateBorder, '0px', 'the member state is small plain text, not a badge');
    assert.ok(hc.messageContrast >= 7 && hc.mutedContrast >= 7, `high contrast text misses AAA: ${JSON.stringify(hc)}`);
    console.log(`High Contrast Dark contrast: message ${hc.messageContrast.toFixed(1)}:1, muted ${hc.mutedContrast.toFixed(1)}:1`);
    const originalViewport = first.viewport();
    const checkHeaderGeometry = async theme => {
      for (const width of [1440, 1100, 800]) {
        await first.setViewport({ width, height: 800 });
        const layout = await first.evaluate(() => {
          const box = selector => document.querySelector(selector).getBoundingClientRect().toJSON();
          const visible = element => !element.hidden && getComputedStyle(element).display !== 'none';
          const buttons = [...document.querySelectorAll('.room-controls button')].filter(visible).map(el => el.getBoundingClientRect().toJSON());
          const topButtons = [...document.querySelectorAll('.workspace-bar nav button')].filter(visible).map(el => ({ ...el.getBoundingClientRect().toJSON(), icon: el.classList.contains('icon-btn') }));
          return { viewport: innerWidth, bar: box('.workspace-bar'), header: box('.chat > header'), panel: box('#agents-panel'), toolbar: box('.room-controls'), buttons, topButtons, connection: box('#connection') };
        });
        const noOverlap = boxes => boxes.every((a, i) => boxes.slice(i + 1).every(b => a.right <= b.left || b.right <= a.left));
        assert.ok(layout.topButtons.every(box => box.top >= 0 && box.bottom <= layout.bar.bottom && box.right <= width && Math.abs(box.height - (box.icon ? 30 : 28)) < 1) && layout.connection.right <= width && noOverlap(layout.topButtons), `${theme} top bar clips, overlaps, or has uneven button heights at ${width}: ${JSON.stringify(layout)}`);
        assert.ok(layout.header.right <= layout.panel.left + 1 && layout.toolbar.right <= layout.header.right + 1 && noOverlap(layout.buttons), `${theme} room toolbar overlaps at ${width}: ${JSON.stringify(layout)}`);
      }
      await first.setViewport(originalViewport);
    };
    await checkHeaderGeometry('High Contrast Dark');
    await openSettings(first);
    await domClick(first, '#settings-general-tab');
    await first.select('#theme-select', 'dark');
    await domClick(first, '#settings-close');
    const darkLegacyBackground = await first.evaluate(() => {
      const sample = document.createElement('div');
      sample.className = 'knowledge-record';
      document.body.append(sample);
      const background = getComputedStyle(sample).backgroundColor;
      sample.remove();
      return { background, hasThemeAttribute: document.documentElement.hasAttribute('data-theme') };
    });
    assert.equal(darkLegacyBackground.hasThemeAttribute, false, 'default Dark has no theme attribute');
    assert.equal(darkLegacyBackground.background, 'rgb(28, 33, 40)', 'legacy card uses the Dark tertiary surface');
    assert.notEqual(darkLegacyBackground.background, 'rgb(247, 249, 247)', 'legacy card must not keep the light surface in Dark');
    await checkHeaderGeometry('Dark');
    assert.equal(await first.$eval('.room-row.active', el => getComputedStyle(el).backgroundColor), 'rgba(88, 166, 255, 0.1)');
    if (await first.$eval(`[data-agent-id="${realAgentID}"]`, el => !el.classList.contains('expanded'))) await domClick(first, `[data-agent-id="${realAgentID}"] .agent-name`);
    await domClick(first, `[data-agent-id="${realAgentID}"] .agent-act-stop`);
    await first.waitForFunction(id => document.querySelector(`[data-agent-id="${id}"] .agent-state-label`)?.textContent === 'stopped', {}, realAgentID);
    if (await first.$eval(`[data-agent-id="${realAgentID}"]`, el => !el.classList.contains('expanded'))) await domClick(first, `[data-agent-id="${realAgentID}"] .agent-name`);
    await domClick(first, `[data-agent-id="${realAgentID}"] .agent-reactivate`);
    await first.waitForFunction(id => document.querySelector(`[data-agent-id="${id}"] .agent-state-label`)?.textContent === 'idle', {}, realAgentID).catch(async error => {
      const ui = await first.evaluate(id => ({ state: document.querySelector(`[data-agent-id="${id}"] .agent-state`)?.textContent,
        error: document.querySelector('#agents-error')?.textContent, connection: document.querySelector('#connection')?.getAttribute('aria-label') }), realAgentID);
      const response = await first.evaluate(async room => (await fetch(`/api/rooms/${room}/agents`)).json(), roomID);
      throw new Error(`reactivate did not render: ${error.message}; UI ${JSON.stringify(ui)}; API ${JSON.stringify(response)}; server ${logs}; supervisor ${supervisorLogs}`);
    });
    const budgetSeed = spawnSync(seedBinary, [path.join(tmp, 'server', 'aimebu.sqlite'), actualProjectID, roomID, id, 'budget', realAgentID]);
    if (budgetSeed.status !== 0) throw new Error(`budget seed failed: ${budgetSeed.stderr}`);
    await first.waitForFunction(() => document.querySelector('#inbox-count')?.textContent === '3');
    await domClick(first, '#inbox-toggle');
    await first.waitForFunction(() => document.querySelectorAll('[data-inbox-id="inb_smoke_budget"] .inbox-decisions button').length === 2);
    assert.deepEqual(await first.$$eval('[data-inbox-id="inb_smoke_budget"] .inbox-decisions button', els => els.map(el => el.textContent)), ['Continue', 'Abandon']);
    await domClick(first, '[data-inbox-id="inb_smoke_budget"] .inbox-decisions button:first-child');
    await first.waitForFunction(() => !document.querySelector('[data-inbox-id="inb_smoke_budget"]') && document.querySelector('#inbox-count')?.textContent === '2');
    // The Inbox toggle is also the way back to the chat (the top-bar Rooms button is gone on desktop).
    await domClick(first, '#inbox-toggle');
    await first.waitForFunction(id => !document.querySelector('.chat').hidden && document.querySelector('#inbox-view').hidden && document.querySelector(`[data-agent-id="${id}"] .agent-state-label`)?.textContent === 'idle', {}, realAgentID);
    if (await first.$eval('#app-view', el => el.classList.contains('rooms-collapsed'))) await first.click('#edge-rooms-expand');
    await first.locator('#composer textarea').fill('Hello agent');
    await first.click('#composer button[type=submit]');
    await first.waitForFunction(() => [...document.querySelectorAll('.message-body')].some(el => el.textContent.includes('I heard: Hello agent'))).catch(async error => {
      preserveFailure = true;
      const diagnostics = await first.evaluate(async (room, agent) => {
        const read = async path => { const response = await fetch(path); return { status: response.status, body: await response.text() }; };
        return { connection: document.querySelector('#connection')?.getAttribute('aria-label'), room: location.hash,
          draft: document.querySelector('#composer textarea')?.value, chatError: document.querySelector('#chat-error')?.textContent,
          agentState: document.querySelector(`[data-agent-id="${agent}"] .agent-state`)?.textContent,
          agentActivity: document.querySelector(`[data-agent-id="${agent}"] .agent-activity`)?.textContent,
          messages: await read(`/api/rooms/${room}/messages`), agents: await read(`/api/rooms/${room}/agents`) };
      }, roomID, realAgentID);
      throw new Error(`fake agent echo timed out: ${error.message}; temp ${tmp}; diagnostics ${JSON.stringify(diagnostics)}; server ${logs.slice(-8000)}; supervisor ${supervisorLogs.slice(-8000)}`);
    });
    await first.waitForFunction(id => document.querySelector(`[data-agent-id="${id}"] .agent-activity`)?.textContent.startsWith('Replied '), {}, realAgentID);
    await second.waitForFunction(id => document.querySelector(`[data-agent-id="${id}"] .agent-activity`)?.textContent.startsWith('Replied '), {}, realAgentID);
    // t2-composer-recall: Up/Down walk the viewer's own messages, Esc restores the draft.
    {
      const own = (await first.evaluate(() => [...document.querySelectorAll('.message.self .message-body')].map(el => el.textContent.trim()))).filter((body, i, all) => body !== all[i - 1]);
      assert.ok(own.length >= 2, `need two own messages for recall, saw ${own.length}`);
      const value = () => first.$eval('#composer textarea', el => el.value);
      await first.focus('#composer textarea'); await first.$eval('#composer textarea', el => { el.value = ''; });
      await first.keyboard.press('ArrowUp'); assert.equal(await value(), own.at(-1), 'Up on an empty composer did not recall the newest own message');
      await first.keyboard.press('ArrowUp'); assert.equal(await value(), own.at(-2), 'second Up did not go back');
      await first.keyboard.press('ArrowDown'); assert.equal(await value(), own.at(-1), 'Down did not go forward');
      await first.keyboard.press('ArrowDown'); assert.equal(await value(), '', 'Down past the newest did not restore the empty draft');
      await first.keyboard.type('draft in progress'); await first.keyboard.press('ArrowUp');
      assert.equal(await value(), 'draft in progress', 'Up with the caret at the end must not recall');
      await first.keyboard.press('Home'); await first.keyboard.press('ArrowUp');
      assert.equal(await value(), own.at(-1), 'Up at the caret start did not recall');
      await first.keyboard.press('Escape'); assert.equal(await value(), 'draft in progress', 'Esc did not restore the draft');
      await first.keyboard.press('Home'); await first.keyboard.press('ArrowUp'); await first.keyboard.press('ArrowDown');
      assert.equal(await value(), 'draft in progress', 'Down past the newest did not restore the draft');
      await first.$eval('#composer textarea', el => { el.value = ''; el.dispatchEvent(new Event('input')); });
    }
    assert.equal(await first.$eval('.message:last-child .message-author', el => el.textContent), realAgentName, 'agent author shows raw ID');
    assert.deepEqual(await first.$eval('.message:last-child .message-role', el => [el.textContent, el.title.toLowerCase()]), ['🛠️', 'worker']);
    assert.deepEqual(await first.$eval('.message:last-child .msg-sender-presence', el => [el.querySelector('img').title.endsWith('· fake'), el.querySelector('.msg-sender-dot').className]), [true, 'msg-sender-dot dot-active'], 'agent message lacks identity icon/dot');
    assert.equal(await first.$eval('.message.self .msg-sender-icon', el => el.getAttribute('src')), '/icons/human.svg');
    if (await first.$eval(`[data-agent-id="${realAgentID}"]`, el => !el.classList.contains('expanded'))) await domClick(first, `[data-agent-id="${realAgentID}"] .agent-name`);
    await domClick(first, `[data-agent-id="${realAgentID}"] .agent-message`);
    await first.waitForFunction(name => document.querySelector('#room-title')?.textContent === name && !!document.querySelector('.direct-rooms .room-row.active'), {}, realAgentSlug);
    assert.equal(await first.$eval('#room-icon', el => el.textContent), '@', 'a private room shows its kind glyph');
    await first.waitForFunction(() => document.querySelector('#spawn-open')?.disabled);
    assert.equal(await first.$eval('#spawn-open', el => el.disabled), true, 'private chat allows spawning another agent');
    await domClick(first, '#room-info');
    assert.equal(await first.$eval('#room-scope-control', el => el.hidden), true, 'private chat shows controls for agents homed elsewhere');
    await domClick(first, '#room-info-close');
    await second.waitForFunction(() => !!document.querySelector('.direct-rooms .room-row'));
    await first.locator('#composer textarea').fill('Private hello');
    await first.click('#composer button[type=submit]');
    await first.waitForFunction(() => [...document.querySelectorAll('.message-body')].some(el => el.textContent.includes('I heard: Private hello')));
    // The read cursor is server-side, so the sender reading its own open chat clears the badge on the other session.
    await second.waitForFunction(() => { const badge = document.querySelector('.direct-rooms .unread-count'); return !badge || badge.hidden; });
    await domClick(first, `[data-room-id="${roomID}"] .room-name`);
    await first.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Nested task');
    await openSettings(first);
    await first.click('#settings-project-tab');
    await first.waitForFunction(() => !document.querySelector('#project-auto-resume').disabled);
    assert.equal(await first.$eval('#project-auto-resume', el => el.checked), false, 'real project auto-resume default is on');
    // The owner edits the project's name and folder here: a bad folder is refused with its reason, a rename reaches the sidebar.
    await first.waitForFunction(() => document.querySelector('#project-details-folder').value && !document.querySelector('#project-details-save').disabled);
    assert.deepEqual(await first.evaluate(() => ({ name: document.querySelector('#project-details-name').value, folder: document.querySelector('#project-details-folder').value, locked: document.querySelector('#project-details-save').disabled })), { name: 'UI smoke', folder: gitRoot, locked: false }, 'the project details are filled in for the owner');
    await first.evaluate(() => { document.querySelector('#project-details-folder').value = 'not/absolute'; });
    await domClick(first, '#project-details-save');
    await first.waitForFunction(() => /absolute path/.test(document.querySelector('#project-details-status').textContent));
    await first.evaluate(path => { document.querySelector('#project-details-folder').value = path; document.querySelector('#project-details-name').value = 'UI smoke renamed'; }, gitRoot);
    await domClick(first, '#project-details-save');
    await first.waitForFunction(() => document.querySelector('#project-details-status').textContent === 'Saved.');
    assert.equal(await first.evaluate(async () => (await (await fetch('/api/projects')).json()).some(project => project.name === 'UI smoke renamed')), true, 'the rename was stored');
    await first.evaluate(() => { document.querySelector('#project-details-name').value = 'UI smoke'; });
    await domClick(first, '#project-details-save');
    await first.waitForFunction(() => document.querySelector('#project-details-status').textContent === 'Saved.');
    await domClick(first, '#project-auto-resume');
    await first.waitForFunction(() => !document.querySelector('#project-auto-resume').disabled);
    const recoverySetting = await first.evaluate(async projectID => (await (await fetch(`/api/projects/${projectID}/settings`)).json()).auto_resume_after_clean_diagnostic, actualProjectID);
    assert.equal(recoverySetting, true, 'project auto-resume setting did not persist');
    // Settings -> Prompts: the six shared sentences with their shipped text, a global and a project override, a live preview, inline plans.
    {
      await domClick(first, '#settings-prompts-tab');
      await first.waitForFunction(() => document.querySelectorAll('#prompt-list .prompt-slot').length === 6 && !document.querySelector('#prompt-tail').disabled);
      const initial = await first.evaluate(() => ({ keys: [...document.querySelectorAll('#prompt-list .prompt-slot')].map(card => card.dataset.key), sources: [...document.querySelectorAll('.prompt-source')].map(el => el.textContent), resetDisabled: [...document.querySelectorAll('.prompt-reset')].every(button => button.disabled), preview: document.querySelector('#prompt-preview').textContent, inline: document.querySelector('#prompt-inline-plan').value, scopes: [...document.querySelector('#prompt-scope').options].map(option => option.textContent) }));
      assert.deepEqual(initial.keys, ['identity', 'retry_notice', 'inbox_ask', 'mentions', 'human_reply', 'tail']);
      assert.ok(initial.sources.every(text => text === 'Shipped text') && initial.resetDisabled && initial.inline === 'optional' && initial.scopes[0].startsWith('Global') && initial.scopes.length >= 2, JSON.stringify(initial));
      assert.ok(initial.preview.includes('[Response contract and schema (fixed)]') && initial.preview.includes('Use context.you_are'), 'the preview shows the locked parts as placeholders');
      // A live edit reaches the preview before anything is saved; Save makes it a global override.
      await first.$eval('#prompt-tail', el => { el.value = 'GLOBAL CLOSING WORDS'; el.dispatchEvent(new Event('input')); });
      assert.ok((await first.$eval('#prompt-preview', el => el.textContent)).includes('GLOBAL CLOSING WORDS'), 'unsaved edit in the preview');
      await domClick(first, '.prompt-slot[data-key=tail] .prompt-save');
      await first.waitForFunction(() => document.querySelector('.prompt-slot[data-key=tail] .prompt-source')?.textContent.startsWith('Global override') && !document.querySelector('.prompt-slot[data-key=tail] .prompt-reset').disabled);
      // A project override wins in the project's view and leaves the global scope alone.
      await first.select('#prompt-scope', actualProjectID);
      await first.waitForFunction(projectID => document.querySelector('#prompt-list').dataset.scope === projectID && document.querySelector('.prompt-slot[data-key=tail] .prompt-source')?.textContent.startsWith('Global override') && document.querySelector('#prompt-tail').value === 'GLOBAL CLOSING WORDS', {}, actualProjectID);
      await first.$eval('#prompt-tail', el => { el.value = 'PROJECT CLOSING WORDS'; el.dispatchEvent(new Event('input')); });
      await domClick(first, '.prompt-slot[data-key=tail] .prompt-save');
      await first.waitForFunction(() => document.querySelector('.prompt-slot[data-key=tail] .prompt-source')?.textContent === 'Project override (this project)');
      const effective = await first.evaluate(async projectID => ({ project: (await (await fetch(`/api/prompts?project_id=${projectID}`)).json()).slots.find(slot => slot.key === 'tail').effective, global: (await (await fetch('/api/prompts')).json()).slots.find(slot => slot.key === 'tail').effective }), actualProjectID);
      assert.deepEqual(effective, { project: 'PROJECT CLOSING WORDS', global: 'GLOBAL CLOSING WORDS' });
      // Reset peels the project layer, then the global one.
      await domClick(first, '.prompt-slot[data-key=tail] .prompt-reset');
      await first.waitForFunction(() => document.querySelector('.prompt-slot[data-key=tail] .prompt-source')?.textContent.startsWith('Global override') && document.querySelector('#prompt-tail').value === 'GLOBAL CLOSING WORDS');
      await first.select('#prompt-scope', '');
      await first.waitForFunction(() => document.querySelector('#prompt-list').dataset.scope === '' && document.querySelector('.prompt-slot[data-key=tail] .prompt-source')?.textContent.startsWith('Global override'));
      await domClick(first, '.prompt-slot[data-key=tail] .prompt-reset');
      await first.waitForFunction(() => document.querySelector('.prompt-slot[data-key=tail] .prompt-source')?.textContent === 'Shipped text' && document.querySelector('#prompt-tail').value.startsWith('A lead with a completed review'));
      // Too-long text is stopped by the field itself; an unknown slot is refused by the server.
      assert.equal(await first.$eval('#prompt-tail', el => el.maxLength), 2000);
      // A failed save keeps what was typed (and does not redraw the boxes); a conflict reloads but keeps the text and warns.
      {
        const draft = await first.evaluate(async () => {
          const original = window.fetch, box = document.querySelector('#prompt-tail');
          const failWith = status => { window.fetch = async (path, options) => String(path).startsWith('/api/prompts/') && options?.method === 'PUT' ? new Response(JSON.stringify({ error: 'x', message: 'nope' }), { status, headers: { 'Content-Type': 'application/json' } }) : original(path, options); };
          const wait = () => new Promise(resolve => setTimeout(resolve, 400));
          const out = {};
          try {
            box.value = 'DRAFT NOT SAVED'; box.dispatchEvent(new Event('input'));
            failWith(500); document.querySelector('.prompt-slot[data-key=tail] .prompt-save').click(); await wait();
            out.failed = { same: document.querySelector('#prompt-tail') === box, value: box.value, error: document.querySelector('#settings-prompts [role=alert]').textContent };
            failWith(409); document.querySelector('.prompt-slot[data-key=tail] .prompt-save').click(); await wait();
            out.conflict = { value: document.querySelector('#prompt-tail').value, error: document.querySelector('#settings-prompts [role=alert]').textContent.includes('your text is still in the box') };
            // While a save is pending the scope select and every box and button are locked (and aria-busy), and they unlock afterwards.
            window.fetch = async (path, options) => { if (String(path).startsWith('/api/prompts/') && options?.method === 'PUT') { await new Promise(resolve => setTimeout(resolve, 500)); return new Response(JSON.stringify({ error: 'x', message: 'slow nope' }), { status: 500, headers: { 'Content-Type': 'application/json' } }); } return original(path, options); };
            document.querySelector('.prompt-slot[data-key=tail] .prompt-save').click(); await new Promise(resolve => setTimeout(resolve, 150));
            const locked = () => ({ scope: document.querySelector('#prompt-scope').disabled, boxes: [...document.querySelectorAll('#prompt-list textarea')].every(box => box.disabled), buttons: [...document.querySelectorAll('#prompt-list button')].every(button => button.disabled), busy: document.querySelector('#settings-prompts').hasAttribute('aria-busy') });
            out.pending = locked(); await wait(); await wait(); out.after = locked();
          } finally { window.fetch = original; }
          return out;
        });
        assert.deepEqual(draft, { failed: { same: true, value: 'DRAFT NOT SAVED', error: 'nope' }, conflict: { value: 'DRAFT NOT SAVED', error: true },
          pending: { scope: true, boxes: true, buttons: true, busy: true }, after: { scope: false, boxes: false, buttons: false, busy: false } }, 'prompt drafts survive a failed save, and the page is locked while a save is pending');
        // Only real edits come back after a reload: an untouched box shows the newer text another editor saved, a typed one is kept.
        const restored = await first.evaluate(async () => {
          const { csrf_token } = await (await fetch('/api/me')).json();
          const call = async (path, method, body) => { const response = await fetch(path, { method, headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: body ? JSON.stringify(body) : undefined }); return response.json().catch(() => ({})); };
          const field = key => document.querySelector(`#prompt-${key}`);
          const settle = () => new Promise(resolve => setTimeout(resolve, 600));
          const view = await (await fetch('/api/prompts?project_id=')).json();
          // Another editor saves "identity" while this page still shows the old text.
          await call('/api/prompts/identity', 'PUT', { project_id: '', value: 'NEWER FROM ELSEWHERE', rev: view.slots.find(slot => slot.key === 'identity').global?.rev || 0 });
          field('inbox_ask').value = 'INBOX EDIT'; field('inbox_ask').dispatchEvent(new Event('input'));
          field('tail').value = 'TAIL SAVED'; field('tail').dispatchEvent(new Event('input'));
          document.querySelector('.prompt-slot[data-key=tail] .prompt-save').click(); await settle();
          const out = { identity: field('identity').value, inbox: field('inbox_ask').value, tail: field('tail').value };
          for (const key of ['identity', 'tail']) { const now = await (await fetch('/api/prompts?project_id=')).json(); await call(`/api/prompts/${key}?project_id=&rev=${now.slots.find(slot => slot.key === key).global.rev}`, 'DELETE'); }
          return out;
        });
        assert.deepEqual(restored, { identity: 'NEWER FROM ELSEWHERE', inbox: 'INBOX EDIT', tail: 'TAIL SAVED' }, 'only real edits are put back after a reload');
        await first.$eval('#prompt-tail', el => { el.value = ''; el.dispatchEvent(new Event('input')); });
      }
      // Inline plans.
      await first.select('#prompt-inline-plan', 'always');
      await first.waitForFunction(() => document.querySelector('#settings-prompts [role=status]').textContent.includes('Inline plans: always'));
      assert.equal(await first.evaluate(async () => (await (await fetch('/api/prompts')).json()).inline_plan_appendix), 'always');
      await first.select('#prompt-inline-plan', 'optional');
      await first.waitForFunction(() => document.querySelector('#settings-prompts [role=status]').textContent.includes('Inline plans: optional'));
    }
    await domClick(first, '#settings-roles-tab');
    await first.waitForFunction(() => document.querySelector('#roles-list').dataset.loaded === '1');
    // Every role is a card with its emoji, name, key and a line of instructions; Edit opens it in the editor, New role blanks it.
    {
      const cards = await first.evaluate(() => [...document.querySelectorAll('#roles-cards .role-card')].map(card => ({ key: card.dataset.roleKey, label: card.querySelector('.role-card-label').textContent, summary: card.querySelector('.role-card-summary').textContent.length > 0, edit: card.querySelector('.role-card-edit').getAttribute('aria-label') })));
      assert.ok(cards.length >= 0 && cards.every(card => card.key && card.label && card.summary && /^Edit /.test(card.edit)), JSON.stringify(cards));
      assert.equal(await first.evaluate(() => document.querySelector('#roles-list').closest('.settings-row').hidden), true, 'the role list replaces the dropdown');
      assert.equal(await first.$eval('#role-editor', el => el.hidden), true, 'the role editor stays closed until a role is opened');
    }
    await domClick(first, '#role-new');
    await first.locator('#role-editor [name=role_key]').fill('analyst');
    await first.locator('#role-editor [name=label]').fill('Analyst');
    await first.locator('#role-editor [name=emoji]').fill('📊');
    await first.locator('#role-editor [name=body]').fill('Analyze the task.');
    await domClick(first, '#role-editor button[type=submit]');
    await first.waitForFunction(() => [...document.querySelectorAll('#roles-list option')].some(el => el.textContent.includes('Analyst')) || !!document.querySelector('#roles-error')?.textContent);
    assert.equal(await first.$eval('#roles-error', el => el.textContent), '');
    // Roles can extend other roles, and a built-in role can be reset: the built-in specialists extend reviewer.
    {
      await first.waitForFunction(() => [...document.querySelectorAll('#roles-list option')].some(option => option.textContent.includes('(sec-reviewer)')));
      const pick = async key => { const value = await first.evaluate(text => [...document.querySelectorAll('#roles-list option')].find(option => option.textContent.includes(text))?.value, `(${key})`); assert.ok(value, `no role ${key}`); await first.select('#roles-list', value); };
      const form = () => first.evaluate(() => { const f = document.querySelector('#role-editor'), resolved = document.querySelector('#role-resolved'); return {
        extends: f.elements.extends.value, options: [...f.elements.extends.options].map(option => option.value), resolvedShown: !document.querySelector('#role-resolved-row').hidden, resolved: resolved.value,
        readonly: resolved.readOnly, body: f.elements.body.value, label: f.elements.label.value, reset: !document.querySelector('#role-reset').disabled, remove: !document.querySelector('#role-delete').disabled }; });
      const saveRole = () => first.click('#role-editor button[type=submit]');
      const secCard = await first.evaluate(() => { const card = document.querySelector('#roles-cards [data-role-key="sec-reviewer"]'); return card && { chips: [...card.querySelectorAll('.role-chip')].map(chip => chip.textContent), summary: card.querySelector('.role-card-summary').textContent }; });
      assert.ok(secCard && secCard.chips.some(chip => /^extends /.test(chip)) && secCard.chips.includes('built in') && secCard.summary.startsWith('Additional focus'), `a specialist's card shows what it extends: ${JSON.stringify(secCard)}`);
      await domClick(first, '#roles-cards [data-role-key="sec-reviewer"] .role-card-edit');
      assert.deepEqual(await first.evaluate(() => ({ key: document.querySelector('#role-editor').elements.role_key.value, selected: document.querySelector('#roles-cards .role-card-selected')?.dataset.roleKey })), { key: 'sec-reviewer', selected: 'sec-reviewer' }, 'Edit opens that role in the editor and marks its card');
      // Cancel, Esc and the open card's own button close the editor without saving, asking first when something was changed, and
      // give focus back to the card's Edit button. While open, that button is a close icon with a tooltip.
      {
        const secEdit = '#roles-cards [data-role-key="sec-reviewer"] .role-card-edit';
        const editor = () => first.evaluate(selector => { const edit = document.querySelector(selector); return { open: !document.querySelector('#role-editor').hidden, focused: document.activeElement === edit, label: edit.getAttribute('aria-label'), title: edit.title, glyph: !!edit.querySelector('.icon-mask-close'), expanded: edit.getAttribute('aria-expanded'), settings: document.querySelector('#settings-dialog').open }; }, secEdit);
        const savedLabel = () => first.evaluate(async () => (await (await fetch('/api/roles')).json()).find(role => role.role_key === 'sec-reviewer')?.label);
        const original = await savedLabel();
        const asked = [], previousAnswer = first.__dialogAnswer; await answerDialogs(first, text => { asked.push(text.split(' | ')[1]); return { accept: true }; });
        assert.deepEqual(await editor(), { open: true, focused: false, label: 'Close Security reviewer without saving', title: 'Close Security reviewer without saving', glyph: true, expanded: 'true', settings: true }, 'the open card shows a close button');
        await first.locator('#role-editor [name=label]').fill('Cancelled name');
        await domClick(first, '#role-cancel');
        await first.waitForFunction(() => document.querySelector('#role-editor').hidden);
        assert.deepEqual(await editor(), { open: false, focused: true, label: 'Edit Security reviewer', title: '', glyph: false, expanded: 'false', settings: true }, 'Cancel closes the editor and focuses Edit');
        assert.deepEqual(asked.splice(0), ['Discard your changes to this role?'], 'Cancel asks before throwing an edit away');
        assert.equal(await savedLabel(), original, 'Cancel saves nothing');
        await domClick(first, secEdit);
        await first.locator('#role-editor [name=label]').fill('Escaped name');
        await first.focus('#role-editor [name=label]'); await first.keyboard.press('Escape');
        await first.waitForFunction(() => document.querySelector('#role-editor').hidden);
        assert.deepEqual(await editor(), { open: false, focused: true, label: 'Edit Security reviewer', title: '', glyph: false, expanded: 'false', settings: true }, 'Esc closes only the editor and focuses Edit');
        assert.deepEqual(asked.splice(0), ['Discard your changes to this role?'], 'Esc asks before throwing an edit away');
        assert.equal(await savedLabel(), original, 'Esc saves nothing');
        await domClick(first, secEdit); await domClick(first, secEdit);
        assert.deepEqual([(await editor()).open, (await editor()).focused, asked.length], [false, true, 0], 'the close button closes an unchanged editor without asking');
        await answerDialogs(first, previousAnswer);
        await domClick(first, secEdit);
      }
      await domClick(first, '#role-new');
      assert.deepEqual(await first.evaluate(() => ({ key: document.querySelector('#role-editor').elements.role_key.value, selected: document.querySelectorAll('#roles-cards .role-card-selected').length })), { key: '', selected: 0 }, 'New role blanks the editor');
      await pick('sec-reviewer');
      const sec = await form();
      assert.equal(sec.extends, 'reviewer', 'a built-in specialist shows that it extends reviewer');
      assert.deepEqual([sec.options.includes(''), sec.options.includes('reviewer'), sec.options.includes('worker'), sec.options.includes('sec-reviewer')], [true, true, true, false], 'a role is offered every role but itself');
      assert.equal(sec.resolvedShown && sec.readonly && sec.resolved.includes('ordered by severity') && sec.resolved.includes('Additional focus:'), true, 'the effective instructions show the parent text and the focus');
      assert.equal(sec.body.startsWith('Additional focus:') && !sec.body.includes('ordered by severity'), true, 'the specialist holds only its own focus');
      assert.deepEqual([sec.reset, sec.remove], [true, false], 'a built-in can be reset and not deleted');
      assert.deepEqual(await first.$eval('#role-reset', el => [el.getAttribute('aria-label'), el.title.startsWith('Reset to default'), !!el.querySelector('.icon-mask-refresh'), el.textContent]), ['Reset to default', true, true, '']);
      // Edit it, then reset it to what shipped.
      await first.locator('#role-editor [name=body]').fill('my own words'); await saveRole();
      // The editor shows what was typed at once; the saved role (with its new revision and effective text) is back when the effective text has it.
      await first.waitForFunction(() => document.querySelector('#role-resolved').value.includes('my own words'));
      assert.equal(await first.$eval('#roles-error', el => el.textContent), '');
      await domClick(first, '#role-reset');
      await first.waitForFunction(() => document.querySelector('#role-editor').elements.body.value.startsWith('Additional focus:'));
      assert.equal((await form()).extends, 'reviewer', 'reset restores the parent');
      // A role of our own that extends reviewer, a child of it, and the refusals.
      const create = async (key, label, body, extendsKey) => {
        await first.select('#roles-list', '');
        await first.locator('#role-editor [name=role_key]').fill(key); await first.locator('#role-editor [name=label]').fill(label); await first.locator('#role-editor [name=body]').fill(body);
        await first.select('#role-extends', extendsKey); await saveRole();
        await first.waitForFunction(text => [...document.querySelectorAll('#roles-list option')].some(option => option.textContent.includes(text)) || !!document.querySelector('#roles-error')?.textContent, {}, `(${key})`);
      };
      await create('qasmoke', 'QA smoke', 'qa words', 'reviewer');
      const qa = await form();
      assert.equal(qa.resolvedShown && qa.resolved.includes('ordered by severity') && qa.resolved.trimEnd().endsWith('qa words'), true, 'a new role that extends reviewer shows the combined text'); assert.equal(qa.options.includes('qasmoke'), false);
      await create('kidsmoke', 'Kid smoke', 'kid words', 'qasmoke');
      await pick('qasmoke'); await first.select('#role-extends', 'kidsmoke'); await saveRole();
      await first.waitForFunction(() => /extend/i.test(document.querySelector('#roles-error').textContent));
      await pick('qasmoke'); await domClick(first, '#role-delete');
      await first.waitForFunction(() => /extend/i.test(document.querySelector('#roles-error').textContent));
      assert.equal(await first.evaluate(() => [...document.querySelectorAll('#roles-list option')].some(option => option.textContent.includes('(qasmoke)'))), true, 'a role that others extend is not deleted');
      await pick('kidsmoke'); await domClick(first, '#role-delete');
      await first.waitForFunction(() => ![...document.querySelectorAll('#roles-list option')].some(option => option.textContent.includes('(kidsmoke)')));
      await pick('qasmoke'); await domClick(first, '#role-delete');
      await first.waitForFunction(() => ![...document.querySelectorAll('#roles-list option')].some(option => option.textContent.includes('(qasmoke)')));
    }
    await domClick(first, '#settings-close');
    const imagePath = path.join(tmp, 'tiny.png');
    fs.writeFileSync(imagePath, Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL/nwAAAABJRU5ErkJggg==', 'base64'));
    await first.evaluate(() => {
      const original = window.fetch;
      window.fetch = async (input, options) => {
        if (String(input).includes('/attachments') && options?.method === 'POST') await new Promise(resolve => setTimeout(resolve, 350));
        return original(input, options);
      };
    });
    const imageBase64 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL/nwAAAABJRU5ErkJggg==';
    const transferImage = async type => first.evaluate(({ type, imageBase64 }) => {
      const bytes = Uint8Array.from(atob(imageBase64), char => char.charCodeAt(0));
      const data = new DataTransfer(); data.items.add(new File([bytes], 'tiny.png', { type: 'image/png' }));
      const target = document.querySelector(type === 'paste' ? '#composer textarea' : '#composer');
      target.dispatchEvent(type === 'paste' ? new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData: data }) : new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: data }));
    }, { type, imageBase64 });
    for (const kind of ['paste', 'drop']) {
      await transferImage(kind);
      await first.waitForSelector('#pending-attachments .pending-image');
      await first.click('#pending-attachments .pending-image button');
      await first.waitForFunction(() => !document.querySelector('#pending-attachments .pending-image'));
    }
    await first.locator('#composer textarea').fill('');
    const [chooser] = await Promise.all([first.waitForFileChooser(), first.click('.attachment-slot')]);
    await chooser.accept([imagePath]);
    await first.waitForFunction(() => document.querySelector('#pending-attachments')?.textContent.includes('Uploading'));
    assert.equal(await first.$eval('#composer button[type=submit]', el => el.disabled), true, 'send enabled during upload');
    await first.waitForSelector('#pending-attachments .pending-image img');
    await first.click('#composer button[type=submit]');
    // This room holds a live fake agent that echoes every human message about a
    // second after it is sent, so the new message is only the last child until
    // that reply lands. Find attachments by what they are, never by position.
    await first.waitForSelector('#messages .image-thumb img');
    await domClick(first, '#messages .image-thumb');
    assert.equal(await first.$eval('#image-lightbox', el => el.open), true);
    await domClick(first, '#image-lightbox-close');
    await first.setViewport({ width: 390, height: 780 });
    await first.evaluate(() => {
      document.querySelector('#app-view').classList.add('chat-open');
      document.querySelector('#app-view').classList.remove('rooms-open');
      document.querySelector('#agents-panel').classList.remove('mobile-open');
    });
    await first.waitForFunction(() => document.elementFromPoint(195, 635)?.closest('.chat'));
    await first.waitForFunction(() => document.querySelector('.sidebar').getBoundingClientRect().right <= 0);
    await domClick(first, '#messages .image-thumb');
    assert.equal(await first.$eval('#image-lightbox', el => el.open), true, 'mobile lightbox did not open');
    await domClick(first, '#image-lightbox-close');
    await first.setViewport({ width: 1100, height: 720 });
    const textPath = path.join(tmp, 'notes.txt');
    fs.writeFileSync(textPath, 'Arbitrary file attachment');
    const [fileChooser] = await Promise.all([first.waitForFileChooser(), first.click('.attachment-slot')]);
    await fileChooser.accept([textPath]);
    await first.waitForFunction(() => document.querySelector('#pending-attachments .pending-image span')?.textContent === 'notes.txt');
    await first.click('#composer button[type=submit]');
    // A timeout dumps what the list, the pending tray and the composer looked like, so a failure names itself.
    await first.waitForFunction(() => document.querySelector('#messages .attachment-file')?.textContent === 'notes.txt').catch(async error => {
      const state = await first.evaluate(() => ({
        lastChildren: [...document.querySelector('#messages').children].slice(-3).map(el => `${el.tagName.toLowerCase()}.${el.className}: ${el.textContent.trim().slice(0, 80)}`),
        attachmentFiles: [...document.querySelectorAll('.attachment-file')].map(el => el.textContent).slice(-3),
        pending: document.querySelector('#pending-attachments')?.textContent, chatError: document.querySelector('#chat-error')?.textContent,
        composerDisabled: document.querySelector('#composer textarea')?.disabled, sendDisabled: document.querySelector('#composer button[type=submit]')?.disabled, room: location.hash,
      })).catch(failure => `state unavailable: ${failure.message}`);
      error.message = `attachment message did not appear: ${error.message}\n${JSON.stringify(state)}`;
      throw error;
    });
    const uploadRoomA = await first.evaluate(() => decodeURIComponent(location.hash.slice(1)));
    const uploadRoomB = await first.evaluate(roomID => [...document.querySelectorAll('.room-name')].map(node => node.closest('[data-room-id]')?.dataset.roomId).find(id => id && id !== roomID), uploadRoomA);
    assert.ok(uploadRoomB, 'room-switch upload probe needs a second room');
    await first.evaluate(roomID => {
      const previousFetch = window.fetch;
      window.__releaseRoomSwitchUpload = null;
      window.__roomSwitchUploadDeleted = false;
      window.fetch = (input, options) => {
        const target = `/api/rooms/${roomID}/attachments`;
        if (String(input) === target && options?.method === 'POST') return new Promise(resolve => {
          window.__releaseRoomSwitchUpload = () => resolve(new Response(JSON.stringify({ id: 'att-room-switch', room_id: roomID, name: 'Old room upload', mime: 'image/png', url: `${target}/att-room-switch` }), { headers: { 'Content-Type': 'application/json' } }));
        });
        if (String(input) === `${target}/att-room-switch` && options?.method === 'DELETE') { window.__roomSwitchUploadDeleted = true; return Promise.resolve(new Response(null, { status: 204 })); }
        return previousFetch(input, options);
      };
    }, uploadRoomA);
    await (await first.$('#attachment-input')).uploadFile(imagePath);
    await first.waitForFunction(() => Boolean(window.__releaseRoomSwitchUpload) && document.querySelector('#pending-attachments')?.textContent.includes('Uploading 1'));
    const uploadRoomATitle = await first.$eval('#room-title', node => node.textContent);
    await domClick(first, `[data-room-id="${uploadRoomB}"] .room-name`);
    await first.waitForFunction(({ id, title }) => decodeURIComponent(location.hash.slice(1)) === id && document.querySelector('#room-title')?.textContent !== title, {}, { id: uploadRoomB, title: uploadRoomATitle });
    await first.evaluate(() => window.__releaseRoomSwitchUpload());
    await first.waitForFunction(() => window.__roomSwitchUploadDeleted && !document.querySelector('#pending-attachments')?.textContent.includes('Uploading'));
    assert.equal(await first.$eval('#composer button[type=submit]', node => node.disabled), false, 'room switch left upload count blocking send');
    await first.locator('#composer textarea').fill('Room switch upload check');
    await first.keyboard.press('Enter');
    await first.waitForFunction(() => [...document.querySelectorAll('#messages .message-body')].some(node => node.textContent.includes('Room switch upload check')));
    await domClick(first, `[data-room-id="${uploadRoomA}"] .room-name`);
    await first.waitForFunction(({ id, title }) => decodeURIComponent(location.hash.slice(1)) === id && document.querySelector('#room-title')?.textContent === title, {}, { id: uploadRoomA, title: uploadRoomATitle });
    // The composer's notices (a file that is too big, an upload or send that failed) never stay: they clear on dismiss, on a timer, on
    // the next pick, on send and on a room switch, and they name the file.
    {
      const pick = specs => first.evaluate(list => { const data = new DataTransfer(); for (const [name, size] of list) data.items.add(new File([new Uint8Array(size)], name, { type: 'application/octet-stream' })); const input = document.querySelector('#attachment-input'); input.files = data.files; input.dispatchEvent(new Event('change')); }, specs);
      const notice = () => first.evaluate(() => { const node = document.querySelector('#composer-notice'); return { hidden: node.hidden, text: node.querySelector('.composer-notice-text').textContent }; });
      const BIG = 11 * 1024 * 1024 + 1; // the app's modules carry ?v=<build>, so the shared settings object is reached through that URL
      await first.evaluate(async () => { (await import('/js/notice.js' + new URL(document.querySelector('script[type=module]').src).search)).noticeSettings.timeout = 600; });
      await pick([['huge.bin', BIG]]);
      await first.waitForFunction(() => !document.querySelector('#composer-notice').hidden);
      assert.deepEqual(await notice(), { hidden: false, text: 'huge.bin is too big (11.1 MB). Use files up to 10 MB.' }, 'the notice names the file and its size');
      assert.deepEqual(await first.$eval('#composer-notice .composer-notice-close', el => [el.getAttribute('aria-label'), el.title, !!el.querySelector('.icon-mask-close'), el.textContent]), ['Dismiss message', 'Dismiss message', true, '']);
      assert.deepEqual(await first.evaluate(async limit => { const { megabytes } = await import('/js/notice.js' + new URL(document.querySelector('script[type=module]').src).search); return [megabytes(limit + 1), megabytes(limit), megabytes(1)]; }, 10 * 1024 * 1024), ['10.1 MB', '10.0 MB', '0.1 MB'], 'a file just over the limit never reads as the limit');
      // Dismiss.
      await first.evaluate(() => document.querySelector('#composer-notice .composer-notice-close').click());
      assert.equal((await notice()).hidden, true, 'the dismiss button clears the notice');
      // Timeout.
      await pick([['again.bin', BIG]]);
      await first.waitForFunction(() => !document.querySelector('#composer-notice').hidden);
      await first.waitForFunction(() => document.querySelector('#composer-notice').hidden, { timeout: 5000 });
      // Several files rejected in one go are all named.
      await first.evaluate(async () => { (await import('/js/notice.js' + new URL(document.querySelector('script[type=module]').src).search)).noticeSettings.timeout = 60000; });
      await pick([['one.bin', BIG], ['two.bin', BIG]]);
      await first.waitForFunction(() => document.querySelector('#composer-notice .composer-notice-text').textContent.includes('two.bin'));
      assert.match((await notice()).text, /one\.bin is too big.*two\.bin is too big/);
      // The next pick clears it, whether or not that file is accepted; an accepted one is removed again afterwards.
      await (await first.$('#attachment-input')).uploadFile(imagePath);
      await first.waitForFunction(() => document.querySelector('#composer-notice').hidden);
      // The room-switch check above left its upload stub in place; it holds each POST until released.
      await first.waitForFunction(() => document.querySelector('#pending-attachments')?.textContent.includes('Uploading 1'));
      await first.evaluate(() => window.__releaseRoomSwitchUpload());
      await first.waitForFunction(() => document.querySelector('#pending-attachments .pending-image'));
      await first.evaluate(() => document.querySelector('#pending-attachments .pending-image button').click());
      await first.waitForFunction(() => !document.querySelector('#pending-attachments .pending-image'));
      // Send clears it.
      await pick([['huge.bin', BIG]]);
      await first.waitForFunction(() => !document.querySelector('#composer-notice').hidden);
      await first.locator('#composer textarea').fill('Notice cleared by send');
      await first.keyboard.press('Enter');
      await first.waitForFunction(() => [...document.querySelectorAll('#messages .message-body')].some(node => node.textContent.includes('Notice cleared by send')));
      assert.equal((await notice()).hidden, true, 'sending clears the notice');
      // A room switch clears it.
      await pick([['huge.bin', BIG]]);
      await first.waitForFunction(() => !document.querySelector('#composer-notice').hidden);
      await first.click(`[data-room-id="${uploadRoomB}"] .room-name`);
      await first.waitForFunction(({ id, title }) => decodeURIComponent(location.hash.slice(1)) === id && document.querySelector('#room-title')?.textContent !== title, {}, { id: uploadRoomB, title: uploadRoomATitle });
      assert.equal((await notice()).hidden, true, 'a room switch clears the notice');
      await domClick(first, `[data-room-id="${uploadRoomA}"] .room-name`);
      await first.waitForFunction(({ id, title }) => decodeURIComponent(location.hash.slice(1)) === id && document.querySelector('#room-title')?.textContent === title, {}, { id: uploadRoomA, title: uploadRoomATitle });
      // A failed upload says which file, and goes away like the rest.
      await first.evaluate(() => { window.__realFetch = window.fetch; window.fetch = (input, options) => (String(input).endsWith('/attachments') && options?.method === 'POST' ? Promise.resolve(new Response(JSON.stringify({ error: 'bad_type', message: 'This file type is not allowed' }), { status: 400, headers: { 'Content-Type': 'application/json' } })) : window.__realFetch(input, options)); });
      await pick([['script.exe', 10]]);
      await first.waitForFunction(() => document.querySelector('#composer-notice .composer-notice-text').textContent === 'script.exe: This file type is not allowed');
      await first.evaluate(() => { window.fetch = window.__realFetch; });
      await first.evaluate(() => document.querySelector('#composer-notice .composer-notice-close').click());
      // A pending image and a pending file are one chip each: same top and height, the remove button inside, all fully visible.
      await first.evaluate(() => {
        window.__realFetch = window.fetch; let n = 0;
        window.fetch = (input, options) => {
          const target = String(input);
          if (target.endsWith('/attachments') && options?.method === 'POST') { n++; const body = n === 1 ? { id: 'chip-img', name: 'chip.png', mime: 'image/png', url: '/icons/favicon.svg' } : { id: 'chip-file', name: 'a-rather-long-document-name-for-a-chip.txt', mime: 'text/plain', url: '/x' }; return Promise.resolve(new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } })); }
          if (/\/attachments\/chip-/.test(target) && options?.method === 'DELETE') return Promise.resolve(new Response(null, { status: 204 }));
          return window.__realFetch(input, options);
        };
      });
      await pick([['chip.png', 10], ['doc.txt', 10]]);
      await first.waitForFunction(() => document.querySelectorAll('#pending-attachments .pending-image').length === 2);
      const chips = await first.evaluate(() => [...document.querySelectorAll('#pending-attachments .pending-image')].map(chip => { const box = chip.getBoundingClientRect(), x = chip.querySelector('button').getBoundingClientRect(), host = document.querySelector('#pending-attachments').getBoundingClientRect(); return { top: box.top, height: box.height, within: x.left >= box.left && x.right <= box.right && x.top >= box.top && x.bottom <= box.bottom, visible: box.left >= host.left && box.right <= host.right + 1 }; }));
      assert.equal(chips.length, 2);
      assert.ok(Math.abs(chips[0].top - chips[1].top) < 1 && Math.abs(chips[0].height - chips[1].height) < 1, `chips share top and height: ${JSON.stringify(chips)}`);
      assert.ok(chips.every(chip => chip.within && chip.visible), `the remove button sits inside each chip and every chip is visible: ${JSON.stringify(chips)}`);
      await first.evaluate(() => { for (const button of document.querySelectorAll('#pending-attachments .pending-image button')) button.click(); });
      await first.waitForFunction(() => !document.querySelector('#pending-attachments .pending-image'));
      await first.evaluate(() => { window.fetch = window.__realFetch; });
      // The thread composer's send failure uses the same notice.
      await first.waitForSelector('#messages .message:last-of-type');
      await first.hover('#messages .message:last-of-type');
      await domClick(first, '#messages .message:last-of-type .message-actions .thread-action');
      await first.waitForFunction(() => !document.querySelector('#thread-panel').hidden);
      await first.evaluate(() => { window.__realFetch = window.fetch; window.fetch = (input, options) => (/\/messages$/.test(String(input)) && options?.method === 'POST' ? Promise.resolve(new Response(JSON.stringify({ error: 'nope', message: 'The thread could not take that' }), { status: 400, headers: { 'Content-Type': 'application/json' } })) : window.__realFetch(input, options)); });
      await first.locator('#thread-composer textarea').fill('thread notice check');
      await first.keyboard.press('Enter');
      await first.waitForFunction(() => document.querySelector('#thread-notice .composer-notice-text').textContent === 'The thread could not take that' && !document.querySelector('#thread-notice').hidden);
      await first.evaluate(() => { document.querySelector('#thread-notice .composer-notice-close').click(); });
      assert.equal(await first.$eval('#thread-notice', node => node.hidden), true, 'the thread notice can be dismissed');
      // Closing the thread takes a notice with it, so it cannot reappear on the next thread opened.
      await first.locator('#thread-composer textarea').fill('thread notice check again');
      await first.keyboard.press('Enter');
      await first.waitForFunction(() => !document.querySelector('#thread-notice').hidden);
      await first.evaluate(() => { window.fetch = window.__realFetch; });
      await domClick(first, '#close-thread');
      assert.equal(await first.$eval('#thread-notice', node => node.hidden), true, 'closing the thread clears its notice');
      await first.evaluate(async () => { (await import('/js/notice.js' + new URL(document.querySelector('script[type=module]').src).search)).noticeSettings.timeout = 8000; });
    }
    await openSettings(first); await domClick(first, '#settings-machines-tab');
    await first.waitForSelector('#machines-list .machine-card');
    assert.ok(await first.$eval('#machines-list .machine-card strong', el => el.textContent.includes('Host')));
    await domClick(first, '#machine-add');
    await first.locator('#machine-pair-form [name=name]').fill('Remote UI smoke');
    await domClick(first, '#machine-pair-form button[type=submit]');
    await first.waitForFunction(() => !document.querySelector('#machine-pair-result').hidden);
    assert.deepEqual(await first.$eval('#machine-pair-copy', el => [el.getAttribute('aria-label'), el.title, !!el.querySelector('.icon-mask-copy'), el.textContent]), ['Copy command', 'Copy command', true, '']);
    assert.deepEqual(await first.$eval('#machine-refresh', el => [el.getAttribute('aria-label'), el.title, !!el.querySelector('.icon-mask-refresh'), el.textContent]), ['Refresh', 'Refresh', true, '']);
    const pairCommand = await first.$eval('#machine-pair-command', el => el.textContent);
    assert.ok(pairCommand.includes('aimebu supervisor --pair'));
    const copiedPair = await first.evaluate(async () => {
      const original = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
      let copied = '';
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async text => { copied = text; } } });
      try {
        document.querySelector('#machine-pair-copy').click();
        await new Promise(resolve => setTimeout(resolve, 0));
        const button = document.querySelector('#machine-pair-copy');
        return { copied, label: button.getAttribute('aria-label'), title: button.title, check: !!button.querySelector('.icon-mask-check'), text: button.textContent };
      } finally {
        if (original) Object.defineProperty(navigator, 'clipboard', original); else delete navigator.clipboard;
      }
    });
    assert.deepEqual(copiedPair, { copied: pairCommand, label: 'Copied', title: 'Copied', check: true, text: '' });
    // The confirmation does not stay: after a moment the button is "Copy command" again.
    await first.waitForFunction(() => { const button = document.querySelector('#machine-pair-copy'); return button.title === 'Copy command' && button.getAttribute('aria-label') === 'Copy command' && !!button.querySelector('.icon-mask-copy') && !button.querySelector('.icon-mask-check'); }, { timeout: 8000 });
    // A copy that fails right after one that worked does not leave "Copied" standing beside "Copy failed", and the earlier timer does not
    // fire into the next state.
    const failedAfterCopy = await first.evaluate(async () => {
      const original = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
      const press = async writeText => {
        Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
        document.querySelector('#machine-pair-copy').click();
        await new Promise(resolve => setTimeout(resolve, 0));
      };
      try {
        await press(async () => {});
        const first = document.querySelector('#machine-pair-copy').title;
        await press(async () => { throw new Error('denied'); });
        const button = document.querySelector('#machine-pair-copy');
        return { first, title: button.title, check: !!button.querySelector('.icon-mask-check'), error: document.querySelector('#machine-pair-error').textContent };
      } finally {
        if (original) Object.defineProperty(navigator, 'clipboard', original); else delete navigator.clipboard;
      }
    });
    assert.deepEqual(failedAfterCopy, { first: 'Copied', title: 'Copy command', check: false, error: 'Copy failed. Select the command and copy it manually.' }, 'a failed copy resets the button');
    // Two copies in flight: whichever settles last decides what the button and the error say, so a failure that settles first leaves no
    // error under a "Copied", and a success that settles first leaves "Copy command" beside the failure.
    const overlapping = await first.evaluate(async () => {
      const original = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
      const deferred = () => { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; };
      const tick = () => new Promise(resolve => setTimeout(resolve, 0));
      const read = () => { const button = document.querySelector('#machine-pair-copy'); return { title: button.title, error: document.querySelector('#machine-pair-error').textContent }; };
      const run = async (settle) => {
        const one = deferred(), two = deferred(), writes = [one, two];
        Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: () => writes.shift().promise } });
        document.querySelector('#machine-pair-copy').click(); document.querySelector('#machine-pair-copy').click();
        await settle(one, two);
        return read();
      };
      try {
        const failFirst = await run(async (one, two) => { one.reject(new Error('denied')); await tick(); two.resolve(); await tick(); });
        const successFirst = await run(async (one, two) => { two.resolve(); await tick(); one.reject(new Error('denied')); await tick(); });
        return { failFirst, successFirst };
      } finally {
        if (original) Object.defineProperty(navigator, 'clipboard', original); else delete navigator.clipboard;
      }
    });
    assert.deepEqual(overlapping, { failFirst: { title: 'Copied', error: '' }, successFirst: { title: 'Copy command', error: 'Copy failed. Select the command and copy it manually.' } }, 'the copy that settles last decides');
    await first.evaluate(() => { document.querySelector('#machine-pair-error').textContent = ''; });
    const token = pairCommand.match(/'(amp_[^']+)'$/)?.[1]; assert.ok(token);
    const paired = await first.evaluate(async token => {
      const response = await fetch('/api/machine-pairings/redeem', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ token }) });
      if (!response.ok) throw new Error(await response.text()); return response.json();
    }, token);
    await domClick(first, '#machine-pair-done'); await first.click('#machine-refresh');
    await first.waitForFunction(id => {
      const card = document.querySelector(`[data-machine-id="${id}"]`);
      if (card?.querySelector('.machine-status')?.textContent !== 'Pending approval') return false;
      const approve = card.querySelector('.machine-card-actions button:first-child');
      if (approve?.getAttribute('aria-label') !== 'Approve' || approve.title !== 'Approve' || !approve.querySelector('.icon-mask-check')) return false;
      approve.click(); // Find and click in one browser task so refresh cannot detach it.
      return true;
    }, {}, paired.machine_id);
    await first.waitForFunction(id => document.querySelector(`[data-machine-id="${id}"] .machine-status`)?.textContent === 'Offline', {}, paired.machine_id);
    await domClick(first, '#settings-close');
    await domClick(first, '#spawn-open');
    await first.waitForFunction(id => !!document.querySelector(`#spawn-machine option[value="${id}"]`), {}, paired.machine_id);
    await first.select('#spawn-machine', paired.machine_id);
    await first.waitForFunction(() => !document.querySelector('#spawn-machine-folder').hidden);
    await first.locator('#spawn-machine-folder input').fill('/tmp/remote-ui-smoke');
    await domClick(first, '#spawn-save-folder');
    await first.waitForFunction(() => document.querySelector('#spawn-folder-status')?.textContent === 'Folder saved.');
    await domClick(first, '#spawn-cancel');
    await domClick(first, `[data-room-id="${roomID}"] .room-name`);
    await first.waitForFunction(id => {
      const button = document.querySelector(`[data-agent-id="${id}"] .agent-switch`);
      if (!button) return false;
      button.click();
      return true;
    }, {}, realAgentID);
    await first.waitForFunction(id => !!document.querySelector(`#switch-machine option[value="${id}"]`), {}, paired.machine_id);
    await first.select('#switch-machine', paired.machine_id);
    assert.equal(await first.$eval('#switch-machine-folder input', el => el.value), '/tmp/remote-ui-smoke');
    assert.equal(await first.$eval('#switch-form button[type=submit]', el => el.disabled), true, 'offline remote machine permits switch');
    assert.equal(await first.$eval('#switch-dialog', dialog => dialog.open), true, 'same-room refresh closed switch dialog');
    await domClick(first, '#switch-cancel');
    const roomRaceContext = await browser.createBrowserContext();
    const roomRacePage = await roomRaceContext.newPage();
    await roomRacePage.evaluateOnNewDocument(() => {
      const originalFetch = window.fetch.bind(window);
      window.__holdAgentRoom = null;
      window.fetch = (input, options) => {
        const url = new URL(String(input), location.href);
        if (window.__holdAgentRoom && url.pathname === `/api/rooms/${window.__holdAgentRoom}/agents` && (!options?.method || options.method === 'GET') && !window.__releaseHeldAgentList) {
          return new Promise(resolve => { window.__releaseHeldAgentList = () => originalFetch(input, options).then(resolve); });
        }
        return originalFetch(input, options);
      };
    });
    await roomRacePage.setViewport({ width: 1100, height: 720 });
    await login(roomRacePage, `${origin}/#${encodeURIComponent(roomID)}`);
    await roomRacePage.waitForSelector(`[data-agent-id="${realAgentID}"]`);
    await roomRacePage.evaluate(id => { window.__holdAgentRoom = id; }, uploadRoomB);
    await domClick(roomRacePage, `[data-agent-id="${realAgentID}"] .agent-name`);
    await domClick(roomRacePage, `[data-agent-id="${realAgentID}"] .agent-switch`);
    await roomRacePage.waitForFunction(() => document.querySelector('#switch-dialog').open);
    await roomRacePage.evaluate(id => document.querySelector(`[data-room-id="${id}"] .room-name`).click(), uploadRoomB);
    await roomRacePage.waitForFunction(() => typeof window.__releaseHeldAgentList === 'function');
    assert.equal(await roomRacePage.$eval('#switch-dialog', dialog => dialog.open), false, 'room change left old Switch dialog open');
    assert.equal(await roomRacePage.$eval('#agents-list', list => list.querySelectorAll('.agent-card[data-agent-id]').length), 0, 'room change kept old agent controls while the new list was held');
    await roomRacePage.evaluate(() => window.__releaseHeldAgentList());
    await roomRacePage.waitForFunction(id => decodeURIComponent(location.hash.slice(1)) === id && !document.querySelector('#agents-list .agent-card'), {}, uploadRoomB);
    await roomRaceContext.close();
    await domClick(first, `[data-room-id="${uploadRoomB}"] .room-name`);
    await waitDump(first, 'room B after the held agent list', id => decodeURIComponent(location.hash.slice(1)) === id && !document.querySelector('#agents-list .agent-card'), id => ({
      hash: decodeURIComponent(location.hash), wanted: id, title: document.querySelector('#room-title')?.textContent, active: [...document.querySelectorAll('.room-row.active')].map(row => row.textContent.trim()),
      agentCards: document.querySelectorAll('#agents-list .agent-card').length, agentsList: document.querySelector('#agents-list')?.textContent.slice(0, 200), connection: document.querySelector('#connection')?.getAttribute('aria-label'),
    }), uploadRoomB);
    await first.evaluate(room => {
      const originalFetch = window.fetch.bind(window);
      let held = false;
      window.fetch = (input, options) => {
        if (!held && String(input) === `/api/rooms/${room}/messages?limit=100`) {
          held = true;
          return new Promise(resolve => { window.__releaseHeldRoomMessages = () => originalFetch(input, options).then(resolve); });
        }
        return originalFetch(input, options);
      };
    }, roomID);
    await domClick(first, `[data-room-id="${roomID}"] .room-name`);
    await first.waitForFunction(() => typeof window.__releaseHeldRoomMessages === 'function');
    await domClick(first, `[data-room-id="${uploadRoomB}"] .room-name`);
    await first.waitForFunction(id => decodeURIComponent(location.hash.slice(1)) === id && !document.querySelector('#agents-list .agent-card'), {}, uploadRoomB);
    const selectedTitle = await first.$eval('#room-title', node => node.textContent);
    await first.evaluate(() => window.__releaseHeldRoomMessages());
    await new Promise(resolve => setTimeout(resolve, 100));
    assert.equal(await first.$eval('#room-title', node => node.textContent), selectedTitle, 'late room load replaced the selected chat');
    assert.equal(await first.$eval('#agents-list', list => list.querySelectorAll('.agent-card[data-agent-id]').length), 0, 'late room load restored old agent controls');
    await domClick(first, `[data-room-id="${roomID}"] .room-name`);
    await first.waitForSelector(`[data-agent-id="${realAgentID}"]`);
    await openSettings(first); await domClick(first, '#settings-machines-tab');
    await first.waitForSelector(`[data-machine-id="${paired.machine_id}"]`);
    await domClick(first, `[data-machine-id="${paired.machine_id}"] .machine-card-actions button`);
    await first.waitForFunction(id => document.querySelector(`[data-machine-id="${id}"] .machine-status`)?.textContent === 'Revoked', {}, paired.machine_id);
    await domClick(first, '#settings-close');
    await domClick(first, '#spawn-open');
    await first.click('#spawn-cancel');
    const archiveRoomID = await first.evaluate(async projectID => {
      const csrf = (await (await fetch('/api/me')).json()).csrf_token;
      const response = await fetch(`/api/projects/${projectID}/rooms`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ title: 'Archive smoke' }) });
      if (!response.ok) throw new Error(await response.text());
      return (await response.json()).id;
    }, actualProjectID);
    const archiveChildID = await first.evaluate(async ({ projectID, parentID }) => {
      const csrf = (await (await fetch('/api/me')).json()).csrf_token;
      const response = await fetch(`/api/projects/${projectID}/rooms`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ title: 'Archive child', parent_id: parentID }) });
      if (!response.ok) throw new Error(await response.text());
      return (await response.json()).id;
    }, { projectID: actualProjectID, parentID: archiveRoomID });
    const archiveMessageID = await first.evaluate(async roomID => {
      const csrf = (await (await fetch('/api/me')).json()).csrf_token;
      const send = async body => {
        const response = await fetch(`/api/rooms/${roomID}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ body }) });
        if (!response.ok) throw new Error(await response.text());
        return (await response.json()).id;
      };
      const found = await send('archive needle in a message');
      for (let i = 0; i < 5; i++) await send(`filler ${i} ` + 'more text '.repeat(200));
      return found;
    }, archiveRoomID);
    await first.waitForSelector(`[data-room-id="${archiveRoomID}"] .room-name`);
    await domClick(first, `[data-room-id="${archiveRoomID}"] .room-name`);
    await first.waitForFunction(id => location.hash.slice(1) === id && document.querySelector('#room-title')?.textContent === 'Archive smoke', {}, archiveRoomID);
    // Archive lives in Room settings (the header has no Archive button); a room that is not archived cannot be removed.
    assert.equal(await first.$('#room-archive'), null, 'the header has no Archive button');
    // The viewer's own messages are on the right (header line included), shrink-wrapped and at most 75% wide; others fill the column on the left.
    await first.waitForSelector('#messages .message.self', { timeout: 20000 });
    const alignment = await first.evaluate(() => {
      const list = document.querySelector('#messages'), cs = getComputedStyle(list), left = list.getBoundingClientRect().left + list.clientLeft + parseFloat(cs.paddingLeft), right = list.getBoundingClientRect().left + list.clientLeft + list.clientWidth - parseFloat(cs.paddingRight), column = right - left; // clientWidth excludes a scrollbar
      const own = list.querySelector('.message.self'), other = list.querySelector('.message:not(.self):not(.system-notice)');
      const box = element => element.getBoundingClientRect();
      const ownMeta = own?.querySelector('.message-meta'), ownTime = ownMeta?.lastElementChild; // the header's last item (the #id) is at the right edge
      return { own: !!own, ownRight: own ? Math.abs(box(own).right - right) < 2 : null, ownNarrow: own ? box(own).width <= column * 0.75 + 1 : null, ownHeaderRight: ownTime ? Math.abs(box(ownTime).right - (box(own).right - 2)) < 6 : null,
        ownBorder: own ? (() => { const probe = document.createElement('i'); probe.style.color = 'var(--green)'; document.body.append(probe); const green = getComputedStyle(probe).color; probe.remove(); return getComputedStyle(own.querySelector('.message-bubble')).borderTopColor === green; })() : null, other: other ? Math.abs(box(other).left - left) < 2 && Math.abs(box(other).width - column) < 2 : null };
    });
    assert.equal(alignment.own, true, 'the archive room has a message of the viewer');
    assert.deepEqual(alignment, { own: true, ownRight: true, ownNarrow: true, ownHeaderRight: true, ownBorder: true, other: alignment.other === null ? null : true }, `own messages right, others left: ${JSON.stringify(alignment)}`);
    const openCommands = await first.evaluate(() => {
      const draft = document.querySelector('#composer textarea');
      draft.value = '/'; draft.dispatchEvent(new Event('input', { bubbles: true }));
      const offered = [...document.querySelectorAll('#composer .quick-commands:not([hidden]) [role="option"]')].map(item => item.querySelector('strong')?.textContent);
      draft.value = ''; draft.dispatchEvent(new Event('input', { bubbles: true }));
      return offered;
    });
    assert.equal(openCommands.includes('/archive'), true, 'slash picker offers archive in an open task room');
    await domClick(first, '#room-info');
    await first.waitForSelector('#room-info-dialog[open]');
    assert.equal(await first.$eval('#room-lifecycle', el => el.textContent), 'Archive room');
    assert.deepEqual(await first.$eval('#room-remove', el => [el.hidden, el.disabled, el.classList.contains('btn-danger')]), [false, true, true], 'Remove is shown, destructive and disabled until the room is archived');
    await domClick(first, '#room-info-close');
    const archiveConfirmations = [];
    await answerDialogs(first, text => { archiveConfirmations.push(text); return { accept: true }; });
    await first.locator('#composer textarea').fill('/archive');
    await first.waitForFunction(() => document.querySelector('#composer .quick-commands:not([hidden]) [role="option"] strong')?.textContent === '/archive');
    await first.keyboard.press('Enter');
    await first.waitForFunction(id => document.querySelector(`[data-room-id="${id}"] > .room-row`)?.classList.contains('archived') && document.querySelector('#composer textarea').disabled, {}, archiveRoomID);
    assert.equal(archiveConfirmations.some(text => text.includes('Archive room |')), true, 'slash archive kept the confirmation');
    await answerDialogs(first, text => (text.includes('New task room under') ? { accept: true, values: { title: 'Nested task' } } : text.includes('Go to message') ? { accept: true, values: { value: jumpSeq } } : text.includes('Base branch or ref') ? { accept: true, values: { value: '' } } : { accept: true, values: { title: 'Nested task', value: 'Nested task' } }));
    assert.equal(await first.$eval(`[data-room-id="${archiveChildID}"] > .room-row`, el => el.classList.contains('archived')), true, 'cascade did not archive the child room');
    await first.waitForFunction(() => document.querySelector('#room-members')?.textContent.includes('Archived'));
    assert.deepEqual(await first.$eval('#room-members', el => { const status = el.querySelector('.room-header-status'), css = getComputedStyle(status); return [status.textContent, css.fontSize, css.margin, Math.round(el.getBoundingClientRect().height), /^\d+ members? · Archived$/.test(el.title)]; }), ['Archived', '9.1px', '0px', 22, true], 'the archived member chip stays compact');
    // The app closes the settings dialog once the archive has been applied (after the room list refresh), which can come after the
    // row and the member line already look archived: reopen only once it has closed, or the wait below sees the old dialog.
    await first.waitForFunction(() => !document.querySelector('#room-info-dialog').open);
    const closedCommands = await first.evaluate(() => {
      const draft = document.querySelector('#composer textarea');
      draft.value = '/archive'; draft.dispatchEvent(new Event('input', { bubbles: true }));
      const offered = [...document.querySelectorAll('#composer .quick-commands:not([hidden]) [role="option"]')].map(item => item.querySelector('strong')?.textContent);
      draft.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }));
      return offered;
    });
    assert.equal(closedCommands.includes('/archive'), false, 'slash picker hides archive in a closed room');
    assert.equal(await first.$eval('#room-members', el => el.textContent.includes('Archived')), true, 'slash command did not reopen a closed room');
    await domClick(first, '#room-info');
    await first.waitForSelector('#room-info-dialog[open]');
    assert.equal(await first.$eval('#room-lifecycle', el => el.textContent), 'Reopen room');
    // The parent has an archived child, so it cannot be removed yet.
    assert.equal(await first.$eval('#room-remove', el => el.disabled), false, 'an archived room can be removed');
    await domClick(first, '#room-info-close');
    assert.equal(await first.$eval(`[data-room-id="${archiveRoomID}"]`, el => el.closest('.archived-rooms')?.open), false, 'archived rooms should start collapsed');
    await first.click('.archived-rooms summary');
    await first.waitForFunction(id => document.querySelector(`[data-room-id="${id}"]`)?.closest('.archived-rooms')?.open, {}, archiveRoomID);
    // Set the archive search the way a person's typing does: one input event, so the sidebar's remembered filter
    // follows. Locator.fill clears the box without an event, and a sidebar render landing between that clear and
    // the typing restores the old remembered text, which the typing then appends to ("Archive smoneedle").
    const setArchiveSearch = text => first.evaluate(value => { const box = document.querySelector('.archive-search'); box.focus(); box.value = value; box.dispatchEvent(new Event('input', { bubbles: true })); }, text);
    await setArchiveSearch('Archive smo');
    assert.equal(await first.$eval(`[data-room-id="${archiveRoomID}"]`, el => !!el.closest('.archived-rooms')), true);
    assert.equal(await first.evaluate(id => !!document.querySelector(`[data-room-id="${id}"]`), archiveChildID), false, 'archive name filter kept a nonmatching room');
    await setArchiveSearch('needle');
    try {
      await first.waitForFunction(id => document.querySelector('.archive-hit')?.textContent.includes('archive needle') && !document.querySelector(`[data-room-id="${id}"]`), {}, archiveRoomID);
    } catch (error) {
      console.error('Archive search state:', JSON.stringify(await first.evaluate(id => ({
        search: document.querySelector('.archive-search')?.value,
        hits: [...document.querySelectorAll('.archive-hit')].map(node => node.textContent),
        error: document.querySelector('.archive-search-error')?.textContent,
        archiveOpen: document.querySelector('.archived-rooms')?.open,
        roomVisible: Boolean(document.querySelector(`[data-room-id="${id}"]`)),
        connection: document.querySelector('#connection')?.getAttribute('aria-label'),
      }), archiveRoomID)));
      throw error;
    }
    await domClick(first, '.archive-hit');
    await first.waitForFunction(({ roomID, messageID }) => location.hash.slice(1) === roomID && document.querySelector(`[data-message-id="${messageID}"]`), {}, { roomID: archiveRoomID, messageID: archiveMessageID });
    await first.waitForFunction(messageID => {
      const el = document.querySelector(`[data-message-id="${messageID}"]`);
      if (!el) return false;
      const rect = el.getBoundingClientRect(), box = document.querySelector('#messages').getBoundingClientRect();
      return rect.top >= box.top && rect.bottom <= box.bottom;
    }, {}, archiveMessageID);
    await first.evaluate(() => { const search = document.querySelector('.archive-search'); search.value = ''; search.dispatchEvent(new Event('input', { bubbles: true })); });
    await first.waitForSelector(`[data-room-id="${archiveRoomID}"] > .room-row`);
    await first.evaluate(id => {
      window.__reopenNode = document.querySelector(`[data-room-id="${id}"] .reopen-room`);
      window.__archivedRow = document.querySelector(`[data-room-id="${id}"] > .room-row`);
      window.__archivedFold = window.__archivedRow.closest('.archived-rooms');
      window.__projectNode = window.__archivedFold.parentElement;
    }, archiveRoomID);
    await domClick(first, `[data-room-id="${roomID}"] .room-name`);
    await first.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Nested task');
    assert.equal(await first.evaluate(id => window.__reopenNode?.isConnected
      && window.__reopenNode === document.querySelector(`[data-room-id="${id}"] .reopen-room`)
      && window.__archivedRow === document.querySelector(`[data-room-id="${id}"] > .room-row`)
      && window.__archivedFold === window.__archivedRow.closest('.archived-rooms')
      && window.__projectNode === window.__archivedFold.parentElement, archiveRoomID), true, 'Archived row or Reopen button was replaced during a sidebar refresh');
    await domClick(first, `[data-room-id="${archiveRoomID}"] .reopen-room`);
    await first.waitForFunction(id => !document.querySelector(`[data-room-id="${id}"] > .room-row`)?.classList.contains('archived') && !document.querySelector('#composer textarea').disabled, {}, archiveRoomID);
    assert.equal(await first.$eval(`[data-room-id="${archiveChildID}"]`, el => el.closest('.archived-rooms')?.open), true, 'Archived section should stay expanded after a room refresh');
    // Removing an archived room: its own settings, destructive confirm, then it is gone for everyone and the view moves to the home room.
    {
      const returnTo = await first.evaluate(() => location.hash.slice(1));
      const removeID = await first.evaluate(async projectID => {
        const csrf = (await (await fetch('/api/me')).json()).csrf_token;
        const response = await fetch(`/api/projects/${projectID}/rooms`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ title: 'Remove smoke' }) });
        if (!response.ok) throw new Error(await response.text());
        return (await response.json()).id;
      }, actualProjectID);
      await first.waitForSelector(`[data-room-id="${removeID}"] .room-name`);
      await domClick(first, `[data-room-id="${removeID}"] .room-name`);
      await first.waitForFunction(id => location.hash.slice(1) === id, {}, removeID);
      await domClick(first, '#room-info'); await first.waitForSelector('#room-info-dialog[open]');
      await domClick(first, '#room-lifecycle');
      await first.waitForFunction(id => document.querySelector(`[data-room-id="${id}"] > .room-row`)?.classList.contains('archived') && document.querySelector('#room-members')?.textContent.includes('Archived'), {}, removeID);
      // The dialog closes itself after the archive is applied, which can be after the row looks archived (see above).
      await first.waitForFunction(() => !document.querySelector('#room-info-dialog').open);
      await domClick(first, '#room-info'); await first.waitForSelector('#room-info-dialog[open]');
      await first.waitForFunction(() => { const button = document.querySelector('#room-remove'); return !button.hidden && !button.disabled && button.getClientRects().length > 0; });
      await first.click('#room-remove');
      await first.waitForFunction(id => !document.querySelector(`[data-room-id="${id}"]`) && !document.querySelector('#room-info-dialog').open, {}, removeID);
      assert.equal(await first.evaluate(id => fetch(`/api/rooms/${id}`).then(response => response.status), removeID), 404, 'the removed room is gone from the server');
      await first.waitForFunction(id => location.hash.slice(1) !== id && document.querySelector('#room-title').textContent !== 'Remove smoke', {}, removeID);
      await domClick(first, `[data-room-id="${returnTo}"] .room-name`);
      await first.waitForFunction(id => location.hash.slice(1) === id, {}, returnTo);
    }
    // Composer formatting: the rich-text editor with its toolbar in the frame, and the Aa switch to a plain textarea. A room of its own
    // keeps the rest of the smoke undisturbed.
    {
      const returnTo = await first.evaluate(() => location.hash.slice(1));
      const composeID = await first.evaluate(async projectID => {
        const csrf = (await (await fetch('/api/me')).json()).csrf_token;
        const response = await fetch(`/api/projects/${projectID}/rooms`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify({ title: 'Composer smoke' }) });
        if (!response.ok) throw new Error(await response.text());
        return (await response.json()).id;
      }, actualProjectID);
      await first.waitForSelector(`[data-room-id="${composeID}"] .room-name`);
      await domClick(first, `[data-room-id="${composeID}"] .room-name`);
      await first.waitForFunction(id => location.hash.slice(1) === id && !document.querySelector('#composer textarea').disabled, {}, composeID);
      const editor = '#composer .composer-editing .ProseMirror';
      const draft = () => first.$eval('#composer textarea', el => el.value);
      const chord = async (keys, code) => { for (const key of keys) await first.keyboard.down(key); await first.keyboard.press(code); for (const key of [...keys].reverse()) await first.keyboard.up(key); };
      const selectAll = () => chord(['Control'], 'KeyA');
      const wipe = async () => { await first.focus(editor); await selectAll(); await first.keyboard.press('Backspace'); };
      const wipeRaw = async () => { await first.focus('#composer textarea'); await selectAll(); await first.keyboard.press('Backspace'); };
      const pressed = () => first.$$eval('#composer .composer-format', buttons => Object.fromEntries(buttons.map(button => [button.dataset.format, button.getAttribute('aria-pressed')])));
      const setFormatting = async on => {
        if ((await first.$eval('#composer .composer-aa', el => el.getAttribute('aria-pressed'))) !== String(on)) await first.click('#composer .composer-aa');
        if (on) await first.waitForSelector(editor); else await first.waitForFunction(() => !document.querySelector('#composer .composer-editor'));
      };
      // The editor is the default (nothing stored means on) and there is no option layer.
      await first.evaluate(() => { localStorage.removeItem('aimebu2.composer.formatting'); sessionStorage.setItem('smoke-default-formatting', '1'); });
      await first.reload();
      await first.waitForSelector('#app-view:not([hidden]):not([aria-busy])', { timeout: 90000 });
      await first.waitForFunction(id => location.hash.slice(1) === id && !document.querySelector('#composer textarea').disabled, {}, composeID);
      await first.waitForSelector(editor);
      assert.equal(await first.$('#composer .composer-layer'), null, 'the live layer is gone');
      assert.deepEqual(await first.$$eval('#composer .composer-toolbar .composer-format', buttons => buttons.map(button => button.getAttribute('aria-label'))), ['Bold', 'Italic', 'Underline', 'Strikethrough', 'Link', 'Numbered list', 'Bulleted list', 'Quote', 'Code', 'Code block']);
      assert.equal(await first.$eval('#composer .composer-toolbar', el => el.getAttribute('role') + ':' + el.getAttribute('aria-label')), 'toolbar:Formatting');
      const layoutGeometry = (scope, firstItem) => first.evaluate((scope, firstItem) => {
        const rect = selector => document.querySelector(`${scope} ${selector}`).getBoundingClientRect();
        const tools = rect('.composer-toolbar'), start = rect(firstItem), field = rect('.composer-field'), aa = rect('.composer-aa'), last = rect('.composer-tools .composer-format:last-child');
        return { leftEdgeAligned: Math.abs(tools.left - start.left) <= 1, aboveInputRow: tools.bottom <= Math.min(start.top, field.top) + 1, fullRow: tools.right >= field.right - 1, aaLast: aa.left >= last.right - 0.5 && aa.right <= tools.right + 0.5 && aa.top >= tools.top - 0.5 && aa.bottom <= tools.bottom + 0.5, noFrame: getComputedStyle(document.querySelector(`${scope} .composer-field`)).borderTopWidth };
      }, scope, firstItem);
      // The layout: the toolbar is a full-width row over the input, with attach and emoji as its first items. Checked by geometry.
      const boxes = scope => first.evaluate(scope => {
        const box = selector => { const el = document.querySelector(`${scope} ${selector}`); if (!el) return null; const r = el.getBoundingClientRect(); return { left: r.left, right: r.right, top: r.top, bottom: r.bottom, cx: (r.left + r.right) / 2, cy: (r.top + r.bottom) / 2 }; };
        return { tools: box('.composer-toolbar'), field: box('.composer-field'), editor: box('.composer-editor .ProseMirror'), attach: box('.attachment-slot'), emoji: box('#emoji-button'), send: box('button[type=submit]') || box('button.btn-primary'), aa: box('.composer-aa'), first: box('.composer-tools .composer-format'), last: box('.composer-tools .composer-format:last-child'), framed: getComputedStyle(document.querySelector(`${scope} .composer-field`)).borderTopWidth, toolbarInField: document.querySelector(`${scope} .composer-field`).contains(document.querySelector(`${scope} .composer-toolbar`)), attachInToolbar: !!document.querySelector(`${scope} .composer-toolbar > .attachment-slot`) };
      }, scope);
      const near = (x, y, tolerance = 1.5) => Math.abs(x - y) <= tolerance;
      const layouts = {
        // A: attach and emoji lead the toolbar row; the bottom row is the input and send only.
        a: g => g.attachInToolbar && near(g.tools.left, g.attach.left) && g.emoji.right <= g.first.left + 2 && g.tools.bottom <= g.field.top + 1 && near(g.field.left, g.tools.left) && g.send.left >= g.field.right - 1 && near(g.send.cy, g.field.cy, 6),
      };
      for (const layout of ['a']) {
        const g = await boxes('#composer');
        assert.equal(layouts[layout](g), true, `layout ${layout.toUpperCase()}: geometry ${JSON.stringify(g)}`);
        // Every layout keeps Aa as the toolbar's last item and the ten format buttons, and the attach and emoji buttons.
        assert.equal(g.aa.left >= g.last.right - 0.5 && g.aa.right <= g.tools.right + 0.5 && g.aa.top >= g.tools.top - 0.5 && g.aa.bottom <= g.tools.bottom + 0.5, true, `layout ${layout.toUpperCase()}: Aa ends the toolbar`);
        assert.equal(await first.evaluate(() => document.querySelector('#composer .composer-aa').parentNode === document.querySelector('#composer .composer-toolbar')), true, `layout ${layout.toUpperCase()}: Aa is the toolbar's last item`);
        assert.equal(await first.$$eval('#composer .composer-tools .composer-format', buttons => buttons.length), 10);
        assert.equal(await first.evaluate(() => !!document.querySelector('#composer .attachment-slot input[type=file]') && !!document.querySelector('#composer #emoji-button')), true);
      }
      // A phone: nothing overflows and the toolbar and the send button stay on screen in every layout (attach and emoji go back to the bottom row in A).
      await first.setViewport({ width: 390, height: 740 });
      await first.waitForFunction(() => !document.querySelector('#composer .composer-toolbar > .attachment-slot'));
      // On a phone the idle composer is one row; the toolbar shows while the composer has focus.
      await first.evaluate(() => document.activeElement?.blur());
      assert.equal(await first.evaluate(() => getComputedStyle(document.querySelector('#composer .composer-toolbar')).display), 'none', 'the idle phone composer has no toolbar row');
      await first.evaluate(() => (document.querySelector('#composer .ProseMirror') || document.querySelector('#composer textarea')).focus());
      await first.waitForFunction(() => getComputedStyle(document.querySelector('#composer .composer-toolbar')).display !== 'none');
      for (const layout of ['a']) {
        const fit = await first.evaluate(() => { const form = document.querySelector('#composer'), r = selector => document.querySelector(selector).getBoundingClientRect(); return { overflow: form.scrollWidth - form.clientWidth, toolbarLeft: r('#composer .composer-toolbar').left, sendRight: r('#composer button[type=submit]').right, view: innerWidth, toolbarShown: getComputedStyle(document.querySelector('#composer .composer-toolbar')).display !== 'none' }; });
        assert.equal(fit.overflow <= 1 && fit.toolbarLeft >= 0 && fit.sendRight <= fit.view + 0.5 && fit.toolbarShown, true, `layout ${layout.toUpperCase()} on a phone: ${JSON.stringify(fit)}`);
      }
      // A phone held sideways has no toolbar: Aa must still be reachable and work in every layout, from the field's corner.
      await first.setViewport({ width: 740, height: 390 });
      await first.waitForFunction(() => document.querySelector('#composer .composer-field').contains(document.querySelector('#composer .composer-aa')));
      for (const layout of ['a']) {
        const reach = await first.evaluate(() => { const aa = document.querySelector('#composer .composer-aa'), r = aa.getBoundingClientRect(), hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2); return { visible: r.width > 0 && r.height > 0 && r.top >= 0 && r.bottom <= innerHeight && r.left >= 0 && r.right <= innerWidth, hit: aa.contains(hit), toolbarShown: getComputedStyle(document.querySelector('#composer .composer-toolbar')).display !== 'none', inField: document.querySelector('#composer .composer-field').contains(aa) }; });
        assert.deepEqual(reach, { visible: true, hit: true, toolbarShown: false, inField: true }, `layout ${layout.toUpperCase()} on a landscape phone: Aa is not reachable ${JSON.stringify(reach)}`);
        await first.click('#composer .composer-aa'); await first.waitForFunction(() => !document.querySelector('#composer .composer-editor') && document.querySelector('#composer .composer-aa').getAttribute('aria-pressed') === 'false');
        await first.click('#composer .composer-aa'); await first.waitForSelector(editor);
      }
      await first.setViewport({ width: 1100, height: 720 });
      await first.waitForFunction(() => document.querySelector('#composer .composer-toolbar > .attachment-slot') && document.querySelector('#composer .composer-toolbar').contains(document.querySelector('#composer .composer-aa')));
      assert.equal((await layoutGeometry('#composer', '.attachment-slot')).leftEdgeAligned, true);
      // Formatting from the keyboard and the toolbar, and the toolbar showing what is on at the selection.
      await wipe();
      assert.ok(Object.values(await pressed()).every(value => value === 'false'), 'nothing is on in an empty editor');
      await first.keyboard.type('say hello'); await selectAll(); await chord(['Control'], 'KeyB');
      assert.equal(await draft(), '**say hello**'); assert.equal(await first.$eval(`${editor} strong`, el => el.textContent), 'say hello');
      assert.deepEqual([(await pressed()).bold, (await pressed()).italic], ['true', 'false'], 'bold shows as on while the selection is bold');
      await chord(['Control'], 'KeyB'); assert.equal(await draft(), 'say hello', 'a second Ctrl+B takes the bold off'); assert.equal((await pressed()).bold, 'false');
      await first.click('#composer .composer-format[data-format="italic"]'); assert.equal(await draft(), '_say hello_', 'the toolbar formats the editor too'); assert.equal((await pressed()).italic, 'true');
      await first.click('#composer .composer-format[data-format="italic"]'); await chord(['Control'], 'KeyU'); assert.equal(await draft(), '<u>say hello</u>'); assert.equal((await pressed()).underline, 'true');
      await chord(['Control'], 'KeyU'); await chord(['Control', 'Shift'], 'KeyX'); assert.equal(await draft(), '~~say hello~~'); assert.equal((await pressed()).strike, 'true');
      await chord(['Control', 'Shift'], 'KeyX'); assert.equal(await draft(), 'say hello');
      await first.click('#composer .composer-format[data-format="code"]'); assert.equal(await draft(), '`say hello`'); assert.equal((await pressed()).code, 'true');
      await first.click('#composer .composer-format[data-format="code"]'); assert.equal(await draft(), 'say hello');
      // A link: the button opens a small address row in the frame (no browser prompt): Enter applies, Escape cancels, a bad address shows an error.
      const linkRow = () => first.evaluate(() => { const row = document.querySelector('#composer .composer-link'), input = row.querySelector('input'); return { open: !row.hidden, focused: document.activeElement === input, value: input.value, error: row.querySelector('.composer-link-error').textContent, aboveInput: row.getBoundingClientRect().top >= document.querySelector('#composer .composer-toolbar').getBoundingClientRect().bottom - 1 && row.getBoundingClientRect().bottom <= document.querySelector('#composer .composer-field').getBoundingClientRect().top + 1 }; });
      await first.click('#composer .composer-format[data-format="link"]');
      assert.deepEqual(await linkRow(), { open: true, focused: true, value: 'https://', error: '', aboveInput: true }, 'the link button opens the address row under the toolbar');
      await first.keyboard.press('Escape'); assert.equal((await linkRow()).open, false, 'Escape cancels the link row'); assert.equal(await first.$(`${editor} a`), null);
      assert.equal(await first.evaluate(() => document.querySelector('#composer .ProseMirror').contains(document.activeElement)), true, 'focus goes back to the editor');
      await first.click('#composer .composer-format[data-format="link"]');
      await first.keyboard.down('Control'); await first.keyboard.press('KeyA'); await first.keyboard.up('Control'); await first.keyboard.type('javascript:alert(1)'); await first.keyboard.press('Enter');
      assert.deepEqual([(await linkRow()).open, (await linkRow()).error], [true, 'Use an http, https or mailto address.'], 'an unsafe address is refused with an inline error');
      assert.equal(await first.$(`${editor} a`), null); assert.equal(await draft(), 'say hello', 'Enter in the row does not send the message');
      await first.keyboard.down('Control'); await first.keyboard.press('KeyA'); await first.keyboard.up('Control'); await first.keyboard.type('https://example.test/a'); await first.keyboard.press('Enter');
      await first.waitForSelector(`${editor} a`); assert.equal((await linkRow()).open, false);
      assert.equal(await first.$eval(`${editor} a`, el => el.getAttribute('href')), 'https://example.test/a');
      assert.equal((await pressed()).link, 'true', 'link shows as on inside a link');
      await first.click('#composer .composer-format[data-format="link"]'); assert.equal(await first.$(`${editor} a`), null, 'the link button takes the link off'); assert.equal((await pressed()).link, 'false');
      await chord(['Control'], 'KeyK'); assert.equal((await linkRow()).open, true, 'Ctrl+K opens the address row'); await first.keyboard.press('Escape');
      // With nothing selected, the address itself becomes the link text.
      await wipe(); await first.keyboard.type('see '); await first.click('#composer .composer-format[data-format="link"]');
      await first.keyboard.down('Control'); await first.keyboard.press('KeyA'); await first.keyboard.up('Control'); await first.keyboard.type('https://example.test/b'); await first.keyboard.press('Enter');
      await first.waitForSelector(`${editor} a`);
      assert.deepEqual(await first.$eval(`${editor} a`, el => [el.textContent, el.getAttribute('href')]), ['https://example.test/b', 'https://example.test/b'], 'a link with no selection uses the address as its text');
      assert.equal(await draft(), 'see [https://example.test/b](https://example.test/b)');
      // Blocks: a list, a quote, a code block, each showing as on while the caret is inside and off outside.
      await wipe(); await first.keyboard.type('- one'); assert.deepEqual([(await pressed()).ul, (await pressed()).ol, (await pressed()).quote], ['true', 'false', 'false']);
      await chord(['Shift'], 'Enter'); await chord(['Shift'], 'Enter'); await first.keyboard.type('after'); assert.equal((await pressed()).ul, 'false', 'a list is off once the caret has left it');
      await wipe(); await first.keyboard.type('1. first'); assert.deepEqual([(await pressed()).ol, (await pressed()).ul], ['true', 'false']);
      await wipe(); await first.keyboard.type('> said'); assert.equal((await pressed()).quote, 'true');
      await first.click('#composer .composer-format[data-format="quote"]'); assert.equal((await pressed()).quote, 'false'); assert.equal(await draft(), 'said', 'the quote button takes the quote off');
      await wipe(); await first.keyboard.type('```'); await first.keyboard.type('x'); assert.equal((await pressed()).codeblock, 'true');
      // Leaving a block: Backspace at the start of its text, or Enter on an empty line, takes the caret out (and Enter does not send);
      // a code block also leaves on a second Enter at its end. What would be sent is checked after typing on.
      const blockExit = async (label, open, backspaceDraft, enterDraft, pressedId) => {
        // Backspace on the empty line of a quote or list item
        await wipe(); await first.keyboard.type(open); await chord(['Shift'], 'Enter');
        assert.equal((await pressed())[pressedId], 'true', `${label}: still inside after Shift+Enter`);
        await first.keyboard.press('Backspace');
        assert.equal((await pressed())[pressedId], 'false', `${label}: Backspace on the empty line leaves the block`);
        await first.keyboard.type('x'); assert.equal(await draft(), backspaceDraft, `${label}: the Markdown after Backspace`);
        // Enter on the empty line
        await wipe(); await first.keyboard.type(open); await chord(['Shift'], 'Enter'); await first.keyboard.press('Enter');
        assert.equal((await pressed())[pressedId], 'false', `${label}: Enter on the empty line leaves the block`);
        await first.keyboard.type('x'); assert.equal(await draft(), enterDraft, `${label}: the Markdown after Enter`);
        assert.equal(await first.$$eval('#messages .message-body', bodies => bodies.filter(body => body.textContent.trim() === 'x').length), 0, `${label}: Enter on an empty line did not send the message`);
      };
      await blockExit('quote', '> a', '> a\n\nx', '> a\n\nx', 'quote');
      await blockExit('bulleted list', '- a', '- a\n\nx', '- a\n\nx', 'ul');
      await blockExit('numbered list', '1. a', '1. a\n\nx', '1. a\n\nx', 'ol');
      // Backspace at the start of a non-empty first line lifts it too, without joining anything.
      await wipe(); await first.keyboard.type('> abc'); for (let i = 0; i < 3; i++) await first.keyboard.press('ArrowLeft'); await first.keyboard.press('Backspace');
      assert.deepEqual([(await pressed()).quote, await draft()], ['false', 'abc'], 'Backspace at the start of a quoted line lifts it out of the quote');
      // A code block: Backspace in an empty one makes a paragraph; two Enters at the end of the text leave it.
      await wipe(); await first.keyboard.type('```'); assert.equal((await pressed()).codeblock, 'true');
      await first.keyboard.press('Backspace'); assert.equal((await pressed()).codeblock, 'false', 'Backspace in an empty code block makes a paragraph');
      await first.keyboard.type('x'); assert.equal(await draft(), 'x');
      await wipe(); await first.keyboard.type('```'); await first.keyboard.type('a'); await first.keyboard.press('Enter');
      assert.equal((await pressed()).codeblock, 'true', 'one Enter in a code block adds a line'); await first.keyboard.press('Enter');
      assert.equal((await pressed()).codeblock, 'false', 'a second Enter at the end leaves the code block');
      await first.keyboard.type('x'); assert.equal(await draft(), '```\na\n```\n\nx', 'the code block keeps its text and loses the trailing newline');
      // Backspace on the empty last line leaves the block too, the mirror of Enter there; on a line with text it only deletes a character.
      await wipe(); await first.keyboard.type('```'); await first.keyboard.type('a'); await first.keyboard.press('Enter');
      assert.equal((await pressed()).codeblock, 'true', 'Enter in a code block adds a line'); await first.keyboard.press('Backspace');
      assert.equal((await pressed()).codeblock, 'false', 'Backspace on the empty last line leaves the code block');
      await first.keyboard.type('x'); assert.equal(await draft(), '```\na\n```\n\nx', 'the code block keeps its text and loses the empty line');
      await wipe(); await first.keyboard.type('```'); await first.keyboard.type('ab'); await first.keyboard.press('Backspace');
      assert.deepEqual([(await pressed()).codeblock, await draft()], ['true', '```\na\n```'], 'Backspace after text only deletes a character');
      await wipe();
      // The empty editor shows its placeholder (the textarea's own text), and drops it once there is anything in it.
      const placeholder = () => first.evaluate(() => { const pm = document.querySelector('#composer .ProseMirror'), after = getComputedStyle(pm, '::before'); return { text: after.content, empty: pm.classList.contains('composer-empty') }; });
      const shown = await placeholder();
      assert.deepEqual([shown.empty, shown.text], [true, '"Message"'], `the rich editor shows the Message placeholder: ${JSON.stringify(shown)}`);
      await first.keyboard.type('x');
      assert.deepEqual([(await placeholder()).empty, (await placeholder()).text], [false, 'none'], 'the placeholder goes away once there is text');
      await wipe();
      await wipe();
      // Typing converts as in a chat editor: **x** and _x_ become marks, "- " a list, "> " a quote, ``` a code block.
      await wipe(); await first.keyboard.type('a **bold** and _soft_ word'); assert.equal(await draft(), 'a **bold** and _soft_ word'); assert.deepEqual(await first.$$eval(`${editor} strong, ${editor} em`, nodes => nodes.map(node => node.tagName + ':' + node.textContent)), ['STRONG:bold', 'EM:soft']);
      await wipe(); await first.keyboard.type('- one'); await chord(['Shift'], 'Enter'); await first.keyboard.type('two'); assert.equal(await draft(), '- one\n- two'); assert.equal(await first.$$eval(`${editor} ul > li`, items => items.length), 2);
      await chord(['Shift'], 'Enter'); await chord(['Shift'], 'Enter'); await first.keyboard.type('after'); assert.equal(await draft(), '- one\n- two\n\nafter', 'an empty item leaves the list');
      await wipe(); await first.keyboard.type('1. first'); await chord(['Shift'], 'Enter'); await first.keyboard.type('second'); assert.equal(await draft(), '1. first\n2. second');
      await wipe();
      // Code is sent byte for byte: blank lines kept, and a fence longer than any backtick run inside.
      await first.keyboard.type('```'); await first.keyboard.type('a'); for (let i = 0; i < 3; i++) await chord(['Shift'], 'Enter'); await first.keyboard.type('use ``` here'); // Shift+Enter adds lines; two plain Enters at the end would leave the block
      assert.equal(await draft(), '````\na\n\n\nuse ``` here\n````', 'a code block with blank lines and backticks is kept exactly');
      await wipe();
      // Pasting an image file into the editor attaches it (the textarea's paste handler is not reached there); an unsafe pasted link is plain text.
      const pasteImage = target => first.evaluate(target => {
        const bytes = Uint8Array.from(atob('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL/nwAAAABJRU5ErkJggg=='), char => char.charCodeAt(0));
        const data = new DataTransfer(); data.items.add(new File([bytes], 'tiny.png', { type: 'image/png' }));
        document.querySelector(target).dispatchEvent(new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData: data }));
      }, target);
      await pasteImage(editor);
      await first.waitForSelector('#pending-attachments .pending-image');
      await first.click('#pending-attachments .pending-image button');
      await first.waitForFunction(() => !document.querySelector('#pending-attachments .pending-image'));
      await first.evaluate(() => {
        const data = new DataTransfer(); data.setData('text/html', '<p><a href="javascript:alert(1)">bad</a> <a href="data:text/html,x">worse</a> <a href="https://example.test/ok">good</a></p>'); data.setData('text/plain', 'bad worse good');
        document.querySelector('#composer .composer-editing .ProseMirror').dispatchEvent(new ClipboardEvent('paste', { bubbles: true, cancelable: true, clipboardData: data }));
      });
      await first.waitForFunction(() => document.querySelector('#composer .composer-editing .ProseMirror').textContent.includes('good'));
      assert.deepEqual(await first.$$eval(`${editor} a`, links => links.map(link => link.getAttribute('href'))), ['https://example.test/ok'], 'only a safe pasted link stays a link');
      await wipe();
      // The shared pickers work inside the editor: "/" commands, ":" emoji (a pick, and a typed :shortcode:), "<" macros and "@" mentions.
      await first.focus(editor); await first.keyboard.type('/ar'); await first.waitForFunction(() => document.querySelector('#composer .quick-commands:not([hidden])')?.textContent.includes('/archive'));
      await wipe(); await first.waitForFunction(() => !document.querySelector('#composer .quick-commands:not([hidden])'));
      // The editor cancels the browser's input event for edits it handles itself (select-all + Backspace); the picker must still close, every time.
      for (let round = 0; round < 20; round++) {
        await first.keyboard.type('/ar'); await first.waitForFunction(() => document.querySelector('#composer .quick-commands:not([hidden])')?.textContent.includes('/archive'), { timeout: 5000 });
        await wipe(); await first.waitForFunction(() => !document.querySelector('#composer .quick-commands:not([hidden])'), { timeout: 5000 });
      }
      await first.keyboard.type('hi :shru'); await first.waitForSelector('#composer .emoji-commands:not([hidden])');
      await first.keyboard.press('Enter'); await first.waitForFunction(() => document.querySelector('#composer textarea').value.startsWith('hi \u{1F937}'));
      assert.equal(await first.$eval('#composer .emoji-commands', el => el.hidden), true, 'the emoji list closes after a pick');
      await wipe(); await first.keyboard.type('and :shrug:'); await first.waitForFunction(() => document.querySelector('#composer textarea').value.includes('\u{1F937}') && !document.querySelector('#composer textarea').value.includes(':shrug:'));
      await first.evaluate(async () => { const { csrf_token } = await (await fetch('/api/me')).json(); await fetch('/api/macros', { method: 'PUT', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ greet: 'hello there' }) }); });
      await wipe(); await first.keyboard.type('say <'); await first.waitForSelector('#composer .macro-commands:not([hidden])'); await first.keyboard.press('Escape'); await wipe();
      await first.keyboard.type('ping @'); await first.waitForSelector('#composer .mention-commands:not([hidden])'); await first.keyboard.press('Escape'); await wipe();
      // Enter sends the Markdown, the editor clears, and the message renders.
      await first.keyboard.type('plain **sent from the editor**'); await first.keyboard.press('Enter');
      await first.waitForFunction(() => [...document.querySelectorAll('#messages .message-body')].some(body => body.textContent.includes('sent from the editor') && body.querySelector('strong')));
      assert.equal(await first.evaluate(async id => (await (await fetch(`/api/rooms/${id}/messages`)).json()).at(-1).body, composeID), 'plain **sent from the editor**');
      assert.deepEqual(await first.evaluate(() => ({ draft: document.querySelector('#composer textarea').value, editorText: document.querySelector('#composer .composer-editing .ProseMirror').textContent })), { draft: '', editorText: '' }, 'sending clears the editor');
      await first.keyboard.type('**strong** and '); await chord(['Control'], 'KeyU'); await first.keyboard.type('under'); await first.keyboard.press('Enter');
      await first.waitForFunction(() => [...document.querySelectorAll('#messages .message-body')].some(body => body.querySelector('strong') && body.querySelector('u')));
      assert.equal(await first.evaluate(async id => (await (await fetch(`/api/rooms/${id}/messages`)).json()).at(-1).body, composeID), '**strong** and <u>under</u>', 'underline is sent as <u>');
      // "Aa": formatting off is a plain textarea, with no toolbar, frame or editor; the Markdown text is exactly what it was.
      await first.keyboard.type('keep **this**');
      await setFormatting(false);
      assert.deepEqual(await first.evaluate(() => ({ editor: !!document.querySelector('#composer .composer-editor'), toolbar: getComputedStyle(document.querySelector('#composer .composer-toolbar')).display, rich: document.querySelector('#composer .composer-field').classList.contains('composer-rich'), draft: document.querySelector('#composer textarea').value, shown: getComputedStyle(document.querySelector('#composer textarea')).display, aa: document.querySelector('#composer .composer-aa').getAttribute('aria-pressed'), stored: localStorage.getItem('aimebu2.composer.formatting') })), { editor: false, toolbar: 'none', rich: false, draft: 'keep **this**', shown: 'block', aa: 'false', stored: 'off' });
      assert.equal(await first.evaluate(() => { const area = document.querySelector('#composer textarea').getBoundingClientRect(), aa = document.querySelector('#composer .composer-aa').getBoundingClientRect(); return aa.left >= area.left && aa.right <= area.right && aa.top >= area.top && aa.bottom <= area.bottom; }), true, 'Aa stays inside the plain textarea');
      // Raw input: typing is literal, Enter sends the text as it is, and the pickers and paste work as before.
      await wipeRaw(); await first.keyboard.type('/ar'); await first.waitForFunction(() => document.querySelector('#composer .quick-commands:not([hidden])')?.textContent.includes('/archive'));
      await wipeRaw(); await first.waitForFunction(() => !document.querySelector('#composer .quick-commands:not([hidden])'));
      await first.keyboard.type('hi :shru'); await first.waitForSelector('#composer .emoji-commands:not([hidden])'); await first.keyboard.press('Enter'); await first.waitForFunction(() => document.querySelector('#composer textarea').value.startsWith('hi \u{1F937}'));
      await wipeRaw(); await first.keyboard.type('ping @'); await first.waitForSelector('#composer .mention-commands:not([hidden])'); await first.keyboard.press('Escape'); await wipeRaw();
      await pasteImage('#composer textarea'); await first.waitForSelector('#pending-attachments .pending-image');
      await first.click('#pending-attachments .pending-image button'); await first.waitForFunction(() => !document.querySelector('#pending-attachments .pending-image'));
      await first.focus('#composer textarea'); await first.keyboard.type('raw **text** and <u>x</u>'); await first.keyboard.press('Enter');
      await first.waitForFunction(() => [...document.querySelectorAll('#messages .message-body')].some(body => body.textContent.includes('raw text and x') && body.querySelector('strong') && body.querySelector('u')));
      assert.equal(await first.evaluate(async id => (await (await fetch(`/api/rooms/${id}/messages`)).json()).at(-1).body, composeID), 'raw **text** and <u>x</u>');
      // Switching is lossless: awkward whitespace and blank lines survive off -> on -> off, and so does Markdown the editor shows literally.
      const awkward = '  leading\n\n\n```\ncode\n\n\n```\ntrailing **x** _y_  \n\n';
      await first.$eval('#composer textarea', (el, value) => { el.value = value; el.dispatchEvent(new Event('input', { bubbles: true })); }, awkward);
      await setFormatting(true);
      assert.equal(await first.$eval(editor, el => el.textContent.includes('trailing **x** _y_')), true, 'the editor shows the draft as text');
      await setFormatting(false);
      assert.equal(await draft(), awkward, 'a draft changed just by visiting the editor');
      await wipeRaw(); await first.keyboard.type('keep me'); await setFormatting(true); assert.equal(await first.$eval(editor, el => el.textContent), 'keep me');
      await first.keyboard.type(' **and more**'); await setFormatting(false); assert.equal(await draft(), 'keep me **and more**', 'what was written in the editor stays as Markdown');
      await setFormatting(true); await first.focus(editor); await selectAll(); await first.keyboard.press('Backspace');
      // The choice is kept in this browser: a reload comes back with formatting off, and on again after switching it on.
      await setFormatting(false);
      await first.reload();
      await first.waitForSelector('#app-view:not([hidden]):not([aria-busy])', { timeout: 90000 });
      await first.waitForFunction(id => location.hash.slice(1) === id && !document.querySelector('#composer textarea').disabled, {}, composeID);
      assert.deepEqual(await first.evaluate(() => ({ editor: !!document.querySelector('#composer .composer-editor'), aa: document.querySelector('#composer .composer-aa').getAttribute('aria-pressed') })), { editor: false, aa: 'false' }, 'formatting stays off after a reload');
      await setFormatting(true);
      await first.reload();
      await first.waitForSelector('#app-view:not([hidden]):not([aria-busy])', { timeout: 90000 });
      await first.waitForFunction(id => location.hash.slice(1) === id && !document.querySelector('#composer textarea').disabled, {}, composeID);
      await first.waitForSelector(editor);
      assert.equal(await first.$eval('#composer .composer-aa', el => el.getAttribute('aria-pressed')), 'true', 'formatting stays on after a reload');
      // The thread composer is the same: toolbar row, pressed states, the shared Aa choice, Enter sends.
      await first.keyboard.press('Escape');
      await first.hover('#messages .message:last-of-type');
      await domClick(first, '#messages .message:last-of-type .message-actions .thread-action');
      await first.waitForFunction(() => !document.querySelector('#thread-panel').hidden);
      const threadDraft = () => first.$eval('#thread-panel textarea[name=body]', el => el.value);
      await first.waitForSelector('#thread-panel .composer-editing .ProseMirror');
      assert.deepEqual(await first.$$eval('#thread-panel .composer-toolbar .composer-format', buttons => buttons.length), 10);
      assert.equal(await first.evaluate(() => getComputedStyle(document.querySelector('#thread-panel .ProseMirror'), '::before').content), '"Reply in thread"', 'the thread editor shows its own placeholder');
      assert.deepEqual(await layoutGeometry('#thread-panel', '.composer-field'), { leftEdgeAligned: true, aboveInputRow: true, fullRow: true, aaLast: true, noFrame: '0px' }, 'the thread toolbar is a row above its input row too');
      const threadA = await boxes('#thread-panel');
      assert.equal(near(threadA.tools.left, threadA.field.left) && threadA.tools.bottom <= threadA.field.top + 1, true, `layout A in the thread composer (no attach or emoji there): ${JSON.stringify(threadA)}`);
      await first.focus('#thread-panel .ProseMirror'); await first.keyboard.type('threaded '); await chord(['Control'], 'KeyB'); await first.keyboard.type('reply');
      assert.equal(await threadDraft(), 'threaded **reply**'); assert.equal(await first.$eval('#thread-panel .composer-format[data-format="bold"]', el => el.getAttribute('aria-pressed')), 'true');
      await first.keyboard.press('Enter');
      await first.waitForFunction(() => [...document.querySelectorAll('#thread-messages .message-body')].some(body => body.textContent.includes('threaded reply') && body.querySelector('strong')));
      assert.deepEqual(await first.evaluate(() => ({ draft: document.querySelector('#thread-panel textarea[name=body]').value, editor: document.querySelector('#thread-panel .ProseMirror').textContent })), { draft: '', editor: '' }, 'sending a thread reply clears the thread editor');
      await first.click('#composer .composer-aa');
      await first.waitForFunction(() => !document.querySelector('#thread-panel .composer-editor') && !document.querySelector('#composer .composer-editor'));
      assert.equal(await first.$eval('#thread-panel .composer-toolbar', el => getComputedStyle(el).display), 'none', 'Aa turns formatting off in the thread composer as well');
      await first.focus('#thread-panel textarea[name=body]'); await first.keyboard.type('raw **reply**'); await first.keyboard.press('Enter');
      await first.waitForFunction(() => [...document.querySelectorAll('#thread-messages .message-body')].some(body => body.textContent.includes('raw reply') && body.querySelector('strong')));
      await domClick(first, '#close-thread');
      // The rest of the smoke types into the plain textarea.
      await first.evaluate(() => sessionStorage.removeItem('smoke-default-formatting'));
      await domClick(first, `[data-room-id="${returnTo}"] .room-name`);
      await first.waitForFunction(id => location.hash.slice(1) === id, {}, returnTo);
    }
    const scopeDump = scope => { const root = document.querySelector(`#${scope}-scope-control`), pause = root?.querySelector('[data-scope-action="pause"]'), box = pause?.getBoundingClientRect(); return { status: root?.querySelector('.scope-control-status')?.textContent, error: root?.querySelector('.scope-control-error')?.textContent, results: root?.querySelector('.scope-control-results')?.textContent, desired: root?.dataset.desired, pauseDisabled: pause?.disabled, box: box && [box.x, box.y, box.width, box.height].map(Math.round), hit: box && document.elementFromPoint(box.x + box.width / 2, box.y + box.height / 2)?.className }; };
    await first.click(`#room-info`);
    await first.waitForFunction(() => document.querySelector('#room-scope-control .scope-control-status')?.textContent === 'State: Active');
    await domClick(first, '#room-scope-control [data-scope-action="pause"]');
    await waitDump(first, 'room scope paused', () => document.querySelector('#room-scope-control .scope-control-status')?.textContent === 'State: Paused', scopeDump, 'room');
    assert.equal(await first.$eval('#room-scope-control [data-scope-action="pause"]', el => el.disabled), true);
    await domClick(first, '#room-scope-control [data-scope-action="resume"]');
    await first.waitForFunction(() => document.querySelector('#room-scope-control .scope-control-status')?.textContent === 'State: Active');
    // Daily limits are per project: the room settings have no limit control, and the room route is gone.
    assert.equal(await first.$('#room-usage-limit'), null, 'room settings must not have a daily limit control');
    assert.equal(await first.evaluate(async id => (await fetch(`/api/rooms/${id}/usage-limit`)).status, roomID), 404, 'the room usage-limit route is gone');
    await first.waitForFunction(() => document.querySelector('#room-schedules .schedule-list')?.textContent.includes('No schedules'));
    await first.locator('#room-schedules [name=body]').fill('UI scheduled note');
    await first.select('#room-schedules [name=kind]', 'interval');
    await first.locator('#room-schedules [name=minutes]').fill('5');
    await domClick(first, '#room-schedules .schedule-form button');
    await first.waitForFunction(() => document.querySelector('#room-schedules .schedule-row')?.textContent.includes('UI scheduled note'));
    assert.equal(await first.$eval('#room-schedules .schedule-row', el => el.textContent.includes('next') && el.textContent.includes(document.querySelector('#room-schedules [name=timezone]').value)), true);
    assert.deepEqual(await first.$eval('#room-schedules [data-action=toggle]', el => [el.getAttribute('aria-label'), el.title, !!el.querySelector('.icon-mask-pause'), el.textContent]), ['Pause', 'Pause', true, '']);
    assert.deepEqual(await first.$eval('#room-schedules [data-action=delete]', el => [el.getAttribute('aria-label'), el.title, !!el.querySelector('.icon-mask-trash'), el.textContent]), ['Delete', 'Delete', true, '']);
    await domClick(first, '#room-schedules [data-action=toggle]');
    await first.waitForFunction(() => document.querySelector('#room-schedules .schedule-row')?.textContent.includes('paused'));
    assert.deepEqual(await first.$eval('#room-schedules [data-action=toggle]', el => [el.getAttribute('aria-label'), el.title, !!el.querySelector('.icon-mask-play'), el.textContent]), ['Resume', 'Resume', true, '']);
    await domClick(first, '#room-info-close');
    await openSettings(first);
    await domClick(first, '#settings-project-tab');
    await first.waitForFunction(() => document.querySelector('#project-scope-control .scope-control-status')?.textContent === 'State: Active');
    await domClick(first, '#project-scope-control [data-scope-action="pause"]');
    await waitDump(first, 'project scope paused', () => document.querySelector('#project-scope-control .scope-control-status')?.textContent === 'State: Paused', scopeDump, 'project');
    await domClick(first, '#project-scope-control [data-scope-action="resume"]');
    await first.waitForFunction(() => document.querySelector('#project-scope-control .scope-control-status')?.textContent === 'State: Active');
    await first.waitForFunction(() => document.querySelector('#project-usage-limit .usage-limit-summary')?.textContent.includes('turns /'));
    // A project inherits the default (Settings, Usages), sets its own value, or has no limit; each for cost and for turns.
    assert.equal(await first.$eval('#project-turn-mode', el => el.value), 'inherit', 'a project inherits by default');
    assert.equal(await first.$eval('#project-turn-cap', el => el.hidden), true, 'the value box shows only for Custom');
    await first.select('#project-turn-mode', 'custom');
    assert.equal(await first.$eval('#project-turn-cap', el => el.hidden), false);
    await first.locator('#project-turn-cap').fill('100');
    await first.select('#project-cost-mode', 'none');
    await domClick(first, '#project-usage-limit button');
    await first.waitForFunction(() => document.querySelector('#project-usage-limit .usage-limit-summary')?.textContent.includes('100 cap'));
    assert.deepEqual(await first.evaluate(async id => { const x = await (await fetch(`/api/projects/${id}/usage-limit`)).json(); return [x.turn_mode, x.daily_turn_cap, x.cost_mode, x.daily_cost_cap]; }, actualProjectID), ['custom', 100, 'none', null]);
    // The default every project inherits is set by the owner in Settings, Usages, and the project's Inherit option names it.
    await domClick(first, '#settings-usages-tab');
    await first.waitForSelector('.usage-limit-default:not([hidden]) #default-cost-cap');
    await first.locator('#default-cost-cap').fill('12.5');
    await first.locator('#default-turn-cap').fill('40');
    await domClick(first, '.usage-limit-default .btn-primary');
    await first.waitForFunction(() => document.querySelector('.usage-limit-default .settings-saved')?.textContent === 'Saved.');
    assert.deepEqual(await first.evaluate(async () => { const x = await (await fetch('/api/usage-limit-default')).json(); return [x.daily_cost_cap, x.daily_turn_cap]; }), [12.5, 40]);
    await domClick(first, '#settings-project-tab');
    await first.waitForFunction(() => document.querySelector('#project-cost-mode option[value=inherit]')?.textContent.includes('$12.50'));
    await first.select('#project-turn-mode', 'inherit');
    await domClick(first, '#project-usage-limit button');
    await first.waitForFunction(() => document.querySelector('#project-usage-limit .usage-limit-summary')?.textContent.includes('40 cap'));
    assert.equal(await first.$eval('#project-turn-mode option[value=inherit]', el => el.textContent), 'Inherit (default: 40 turns)', 'Inherit names the default');
    // Put the install back as it was for the checks that follow: no default, no project limit.
    await first.evaluate(async id => {
      const { csrf_token } = await (await fetch('/api/me')).json(); const headers = { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token };
      await fetch('/api/usage-limit-default', { method: 'PUT', headers, body: JSON.stringify({ daily_cost_cap: null, daily_turn_cap: null }) });
      const current = await (await fetch(`/api/projects/${id}/usage-limit`)).json();
      await fetch(`/api/projects/${id}/usage-limit`, { method: 'PUT', headers, body: JSON.stringify({ cost_mode: 'none', turn_mode: 'none', daily_cost_cap: null, daily_turn_cap: null, rev: current.rev }) });
    }, actualProjectID);
    await domClick(first, '#settings-close');
    // Every room has the same Room settings. The home room's adds the Project section (the very form of Settings → Project), its schedules,
    // the project's memory where a task room has task notes, and a line instead of the worktree controls.
    {
      const homeID = await first.evaluate(async id => (await (await fetch(`/api/projects/${id}/rooms`)).json()).find(room => room.kind === 'home').id, actualProjectID);
      await domClick(first, `[data-room-id="${homeID}"] .room-name`);
      await first.waitForFunction(id => decodeURIComponent(location.hash.slice(1)) === id, {}, homeID);
      await first.waitForFunction(() => !document.querySelector('#task-notes-toggle').hidden && document.querySelector('#task-notes-toggle').getAttribute('aria-label') === 'Project memory').catch(() => {});
      assert.deepEqual(await first.$eval('#task-notes-toggle', el => ({ hidden: el.hidden, label: el.getAttribute('aria-label'), title: el.title })), { hidden: false, label: 'Project memory', title: 'Project memory' }, 'the home room\'s notes button opens the project memory');
      await domClick(first, '#room-info');
      await first.waitForSelector('#room-info-dialog[open]');
      await first.waitForFunction(() => document.querySelector('#room-schedules .schedule-list')?.textContent.includes('No schedules') && !document.querySelector('#project-auto-resume').disabled);
      const home = await first.evaluate(() => {
        const shown = selector => { const el = document.querySelector(selector); return !!el && !el.closest('[hidden]') && el.getClientRects().length > 0; };
        return {
          schedules: shown('#room-schedules'), lead: document.querySelector('#room-schedules [name=target] option[value=lead]')?.textContent,
          project: shown('#room-project'), form: !!document.querySelector('#room-project #project-details') && !!document.querySelector('#room-project #project-usage-limit') && !!document.querySelector('#room-project #project-scope-control'),
          settingsEmpty: !document.querySelector('#settings-project #project-details'), name: document.querySelector('#project-details-name').value,
          notes: document.querySelector('.room-notes h3')?.textContent, notesButton: document.querySelector('.room-notes button')?.textContent, notesShown: shown('.room-notes'),
          worktree: document.querySelector('.room-worktree p')?.textContent, worktreeButtons: [...document.querySelectorAll('.room-worktree button')].filter(button => !button.hidden).length,
          wide: document.querySelector('#room-info-dialog').classList.contains('room-info-home'), archive: document.querySelector('#room-lifecycle').hidden,
        };
      });
      assert.deepEqual(home, { schedules: true, lead: 'Current room lead', project: true, form: true, settingsEmpty: true, name: 'UI smoke', notes: 'Project memory', notesButton: 'Open project memory', notesShown: true,
        worktree: 'The home room works in the project folder. Task rooms can have their own Git worktree.', worktreeButtons: 0, wide: true, archive: true }, 'the home room settings carry every room section plus the project');
      // A home-room schedule is created like a task room's.
      await first.locator('#room-schedules [name=body]').fill('UI home note');
      await first.select('#room-schedules [name=kind]', 'interval');
      await first.locator('#room-schedules [name=minutes]').fill('5');
      await domClick(first, '#room-schedules .schedule-form button');
      await first.waitForFunction(() => document.querySelector('#room-schedules .schedule-row')?.textContent.includes('UI home note'));
      await domClick(first, '#room-schedules [data-action=delete]');
      await first.waitForFunction(() => document.querySelector('#room-schedules .schedule-list')?.textContent.includes('No schedules'));
      // The project memory entry opens the Memory viewer on this project's facts.
      await domClick(first, '.room-notes button');
      await first.waitForFunction(() => document.querySelector('#knowledge-dialog').open && !document.querySelector('#room-info-dialog').open);
      assert.deepEqual(await first.evaluate(id => ({ scope: document.querySelector('#knowledge-scope').value, project: document.querySelector('#knowledge-project').value === id, title: document.querySelector('#knowledge-title').textContent }), actualProjectID),
        { scope: 'project', project: true, title: 'Project facts' }, 'the home room links to the project facts');
      await domClick(first, '#knowledge-close');
      // Settings → Project is a shortcut to the same form: it moves there, and back while the home room settings are still open underneath.
      await domClick(first, '#room-info');
      await first.waitForSelector('#room-project #project-details');
      await openSettings(first);
      await domClick(first, '#settings-project-tab');
      await first.waitForFunction(() => !!document.querySelector('#settings-project #project-details') && !document.querySelector('#project-auto-resume').disabled);
      await domClick(first, '#settings-close');
      await first.waitForFunction(() => !!document.querySelector('#room-project #project-details'));
      await domClick(first, '#room-info-close');
      // A task room keeps its task notes and worktree controls and has no Project section.
      await domClick(first, `[data-room-id="${roomID}"] .room-name`);
      await first.waitForFunction(id => decodeURIComponent(location.hash.slice(1)) === id && document.querySelector('#task-notes-toggle').getAttribute('aria-label') === 'Task notes', {}, roomID);
      await domClick(first, '#room-info');
      await first.waitForSelector('#room-info-dialog[open]');
      assert.deepEqual(await first.evaluate(() => ({ project: document.querySelector('#room-project').hidden, notes: document.querySelector('.room-notes h3').textContent, wide: document.querySelector('#room-info-dialog').classList.contains('room-info-home'), toggle: document.querySelector('#task-notes-toggle').getAttribute('aria-label') })),
        { project: true, notes: 'Task notes', wide: false, toggle: 'Task notes' }, 'a task room has no Project section');
      await domClick(first, '#room-info-close');
    }
    // Hide is set from the room settings, not from the rows; nothing has a pin.
    await domClick(first, `[data-room-id="${roomID}"] .room-name`);
    await first.waitForFunction(id => decodeURIComponent(location.hash.slice(1)) === id, {}, roomID);
    await domClick(first, '#room-info');
    assert.equal(await first.$('#room-pin'), null, 'room settings have no Pin room button');
    assert.equal(await first.$$eval(`[data-room-id="${roomID}"] > .room-row .room-pin, [data-room-id="${roomID}"] > .room-row .room-hide, [data-room-id="${roomID}"] > .room-row .room-pinned`, nodes => nodes.length), 0, 'the rows carry no pin or hide buttons and no star');
    await domClick(first, '#room-hide');
    await first.waitForFunction(id => !document.querySelector(`[data-room-id="${id}"]`) && !!document.querySelector('.hidden-rooms') && !document.querySelector('#room-info-dialog').open, {}, roomID);
    await first.click('.hidden-rooms summary');
    await domClick(first, `.hidden-room-show[data-hidden-room-id="${roomID}"]`);
    await first.waitForFunction(id => !!document.querySelector(`[data-room-id="${id}"] > .room-row`), {}, roomID);
    const invitedContext = await browser.createBrowserContext();
    const invitedPage = await invitedContext.newPage();
    if (process.env.UI_THROTTLE_INVITED) { const session = await invitedPage.createCDPSession(); await session.send('Emulation.setCPUThrottlingRate', { rate: Number(process.env.UI_THROTTLE_INVITED) }); }
    await invitedPage.evaluateOnNewDocument(() => {
      const originalFetch = window.fetch.bind(window);
      let failFirstLogin = true;
      window.fetch = (input, options) => {
        if (failFirstLogin && String(input) === '/api/login') {
          failFirstLogin = false;
          return Promise.resolve(new Response('{"message":"retry sign-in"}', { status: 503, headers: { 'Content-Type': 'application/json' } }));
        }
        return originalFetch(input, options);
      };
    });
    const pageErrors = []; invitedPage.on('pageerror', error => pageErrors.push(error.message));
    const unauthorised = []; invitedPage.on('response', response => { if (response.status() === 401) unauthorised.push(response.url()); });
    await invitedPage.goto(inviteLink);
    await invitedPage.waitForSelector('.invite-entry:not([hidden])');
    assert.deepEqual(unauthorised, [], 'a signed-out page load causes no 401 (each is a console error)');
    // The invitation is one card that says what it is for; the sign-in card is not stacked under it.
    {
      const card = await invitedPage.evaluate(() => ({ lead: document.querySelector('.invite-lead').textContent, signInHidden: document.querySelector('#login-form').hidden,
        visibleCards: [...document.querySelectorAll('#login-view form')].filter(form => !form.hidden).length, submit: document.querySelector('.invite-entry button[type=submit]').textContent }));
      assert.match(card.lead, /^You are invited to .+ as an? (owner|operator|viewer)\./);
      assert.deepEqual([card.signInHidden, card.visibleCards, card.submit], [true, 1, 'Create account and sign in']);
    }
    await invitedPage.locator('.invite-entry [name=login]').fill('invitee');
    await invitedPage.locator('.invite-entry [name=password]').fill('a secure password');
    await domClick(invitedPage, '.invite-entry button[type=submit]');
    await invitedPage.waitForFunction(() => document.querySelector('.invite-entry .error')?.textContent === 'retry sign-in');
    assert.equal(await invitedPage.$eval('.invite-entry button[type=submit]', el => el.textContent), 'Sign in');
    await domClick(invitedPage, '.invite-entry button[type=submit]');
    await appReady(invitedPage);
    // A viewer's sign-in reaches the ready state: no "Some checks didn't finish", no script error.
    assert.deepEqual(await invitedPage.evaluate(() => ({ status: document.querySelector('#startup-status').hidden, text: document.querySelector('#startup-status span').textContent })), { status: true, text: '' }, 'a non-owner sign-in finishes its startup checks');
    assert.deepEqual(pageErrors, [], 'a non-owner sign-in raises no script error');
    await domClick(invitedPage, '#settings-toggle');
    await invitedPage.waitForFunction(() => document.querySelector('#usages-toggle')?.hidden && document.querySelector('#settings-usages-tab')?.hidden);
    await domClick(invitedPage, '#settings-close');
    // A reset link is one card too: it names the account, asks for a password and signs in with it.
    {
      // The target is a throwaway account: a reset ends its sessions, and the invitee's page below is still in use.
      const origin = new URL(inviteLink).origin;
      const mint = body => first.evaluate(async payload => {
        const csrf = (await (await fetch('/api/me')).json()).csrf_token;
        return (await (await fetch('/api/invites', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify(payload) })).json()).token;
      }, body);
      const projectID = await first.evaluate(async () => (await (await fetch('/api/projects')).json())[0].id);
      const joinToken = await mint({ kind: 'join', project_id: projectID, role: 'viewer' });
      const made = await fetch(`${origin}/api/invites/redeem`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ token: joinToken, login: 'resetme', password: 'a secure password' }) });
      assert.equal(made.status, 201);
      const resetToken = await mint({ kind: 'reset', target_user_id: (await made.json()).user_id });
      const resetContext = await browser.createBrowserContext(), resetPage = await resetContext.newPage();
      await resetPage.goto(`${origin}/#invite=${resetToken}`);
      await resetPage.waitForSelector('.invite-entry:not([hidden])');
      assert.deepEqual(await resetPage.evaluate(() => ({ lead: document.querySelector('.invite-lead').textContent, login: document.querySelector('.invite-entry [name=login]').value, readOnly: document.querySelector('.invite-entry [name=login]').readOnly,
        submit: document.querySelector('.invite-entry button[type=submit]').textContent, signIn: document.querySelector('#login-form').hidden })),
        { lead: 'Choose a new password for resetme.', login: 'resetme', readOnly: true, submit: 'Set password and sign in', signIn: true }, 'a reset link names the account and offers one action');
      await resetPage.locator('.invite-entry [name=password]').fill('another secure password');
      await domClick(resetPage, '.invite-entry button[type=submit]');
      await appReady(resetPage);
      await resetContext.close();
    }
    // A private chat is started from a person's card in the members panel (the Direct messages section only lists the chats).
    await invitedPage.waitForSelector('#projects .room-group .room-name', { visible: true });
    assert.equal(await invitedPage.$('.new-direct-chat'), null, 'the sidebar has no New private chat button');
    await domClick(invitedPage, '#projects .room-group .room-name');
    await invitedPage.waitForSelector('.human-dm', { visible: true });
    // A viewer is not offered what would answer 403: no writing or spawning in an ordinary room, no new project, no owner-only settings.
    assert.deepEqual(await invitedPage.evaluate(() => ({ send: document.querySelector('#composer button[type=submit]').disabled, note: !document.querySelector('#composer-viewer-note').hidden,
      spawn: document.querySelector('#spawn-open').disabled, spawnTitle: document.querySelector('#spawn-open').title, project: document.querySelector('#project-open').disabled, projectTitle: document.querySelector('#project-open').title })),
      { send: true, note: true, spawn: true, spawnTitle: 'Viewers cannot spawn agents', project: true, projectTitle: 'Only the owner or an admin can create projects' }, 'a viewer sees the disabled controls with the reason');
    await domClick(invitedPage, '#settings-toggle');
    assert.deepEqual(await invitedPage.evaluate(() => ['#settings-roles-tab', '#settings-machines-tab', '#settings-harnesses-tab', '#settings-prompts-tab', '#settings-macros-tab'].map(id => document.querySelector(id)?.hidden ?? 'missing')), [true, true, true, true, true], 'the owner-only settings tabs are gone for a viewer');
    // With Machines and Harnesses both gone, so is the AGENTS caption and its gap: the viewer's nav is one SETTINGS group.
    const viewerNav = await settingsNav(invitedPage);
    assert.deepEqual([viewerNav.agentsHidden, viewerNav.gap, viewerNav.items.filter(item => item === item.toUpperCase())], [true, null, ['SETTINGS']], `a viewer's Settings nav has no AGENTS group: ${JSON.stringify(viewerNav)}`);
    await domClick(invitedPage, '#settings-close');
    // A viewer sees the home room's Project section with the same locks Settings → Project has.
    {
      const viewerHome = await invitedPage.evaluate(async () => { const [project] = await (await fetch('/api/projects')).json(); return (await (await fetch(`/api/projects/${project.id}/rooms`)).json()).find(room => room.kind === 'home').id; });
      await domClick(invitedPage, `[data-room-id="${viewerHome}"] .room-name`);
      await invitedPage.waitForFunction(id => decodeURIComponent(location.hash.slice(1)) === id, {}, viewerHome);
      await domClick(invitedPage, '#room-info');
      await invitedPage.waitForFunction(() => document.querySelector('#project-details-status')?.textContent === 'Only the project owner can change these.' && document.querySelector('#room-project #project-details'));
      assert.deepEqual(await invitedPage.evaluate(() => ({ project: document.querySelector('#room-project').hidden, name: document.querySelector('#project-details-name').disabled, folder: document.querySelector('#project-details-folder').disabled, save: document.querySelector('#project-details-save').disabled, schedules: document.querySelector('#room-schedules').hidden })),
        { project: false, name: true, folder: true, save: true, schedules: false }, 'a viewer cannot edit the project from the home room settings');
      await domClick(invitedPage, '#room-info-close');
    }
    const dmResponses = [];
    invitedPage.on('response', response => { if (/\/dms$/.test(response.url())) dmResponses.push(`${response.request().method()} ${response.status()}`); });
    assert.equal(await invitedPage.$eval('.human-dm', el => el.getAttribute('aria-label')), 'Message owner privately', 'the person card offers the private chat');
    assert.equal(await invitedPage.$eval('.human-dm', el => { const role = el.closest('.human-card').querySelector('.human-role').getBoundingClientRect(), box = el.getBoundingClientRect(); return box.right <= role.left + 0.5 || box.left >= role.right - 0.5; }), true, 'the private-chat button does not lie over the role');
    await domClick(invitedPage, '.human-dm');
    await waitDump(invitedPage, 'new DM', () => document.querySelector('.room-row.active .room-name')?.textContent.trim().endsWith(' owner'), () => ({
      hash: decodeURIComponent(location.hash),
      title: document.querySelector('#room-title')?.textContent, active: [...document.querySelectorAll('.room-row.active')].map(row => row.textContent.trim()),
      rows: [...document.querySelectorAll('.room-row')].map(row => row.textContent.trim()).slice(0, 12),
    })).catch(error => { error.message += `\nnew DM POST /dms responses: ${JSON.stringify(dmResponses)}`; throw error; });
    const peopleDMID = await invitedPage.evaluate(() => decodeURIComponent(location.hash.slice(1)));
    await invitedPage.locator('#composer textarea').fill('Private hello from invitee');
    await invitedPage.keyboard.press('Enter');
    await invitedPage.waitForFunction(() => [...document.querySelectorAll('#messages .message-body')].some(node => node.textContent.includes('Private hello from invitee')));
    await first.waitForFunction(id => document.querySelector(`[data-room-id="${id}"] .unread-count`)?.textContent === '1', {}, peopleDMID);
    // Opening the room marks it read on the server, so the count is gone for every session of this account.
    const roomBeforePeopleDM = await first.evaluate(() => decodeURIComponent(location.hash.slice(1)));
    await domClick(first, `[data-room-id="${peopleDMID}"] .room-name`);
    await first.waitForFunction(id => { const badge = document.querySelector(`[data-room-id="${id}"] .unread-count`); return !badge || badge.hidden; }, {}, peopleDMID);
    await first.waitForFunction(async id => (await (await fetch(`/api/rooms/${id}`)).json()).unread_count === 0, {}, peopleDMID);
    if (roomBeforePeopleDM && roomBeforePeopleDM !== peopleDMID) await domClick(first, `[data-room-id="${roomBeforePeopleDM}"] .room-name`);
    // Global admins: the owner flags an account in Settings -> Users -> Accounts (the toggle is the owner's alone), the row says Admin, the
    // account then has the admin's reach and no more, and an admin's own list offers no way to act on the owner.
    {
      const flagOf = page => page.evaluate(async () => { const body = await (await fetch('/api/users')).json().catch(() => ({})); return [body.global_owner === true, body.global_admin === true]; });
      assert.deepEqual(await flagOf(invitedPage), [false, false], 'a person invited as a viewer is no admin');
      await openSettings(first);
      await first.waitForSelector('#settings-users-tab:not([hidden]):not(.settings-tab-pending)');
      await domClick(first, '#settings-users-tab');
      await first.waitForSelector('#settings-users .user-accounts .user-row[data-user-id]');
      const accounts = () => first.$$eval('#settings-users .user-accounts .user-row', rows => rows.map(row => ({ login: row.querySelector('strong').textContent, badge: row.querySelector('.user-badge')?.textContent || '', toggle: row.querySelector('.user-admin-toggle input')?.checked ?? null, buttons: [...row.querySelectorAll('button')].map(button => button.textContent) })));
      let rows = await accounts();
      const ownerRow = rows.find(row => row.login === 'owner'), inviteeRow = rows.find(row => row.login === 'invitee');
      assert.deepEqual([ownerRow.badge, ownerRow.toggle, ownerRow.buttons.includes('Deactivate')], ['Owner', null, false], `the owner's row: ${JSON.stringify(ownerRow)}`);
      assert.deepEqual([inviteeRow.badge, inviteeRow.toggle], ['', false], `the invitee's row: ${JSON.stringify(inviteeRow)}`);
      await first.evaluate(() => [...document.querySelectorAll('#settings-users .user-accounts .user-row')].find(row => row.querySelector('strong').textContent === 'invitee').querySelector('.user-admin-toggle input').click());
      await first.waitForFunction(() => [...document.querySelectorAll('#settings-users .user-accounts .user-row')].some(row => row.querySelector('strong').textContent === 'invitee' && row.querySelector('.user-badge')?.textContent === 'Admin'));
      assert.deepEqual(await flagOf(invitedPage), [false, true], 'the flagged account is now an admin');
      // The admin sees the accounts, may act on ordinary ones, and is offered nothing to do to the owner or to the toggle.
      await invitedPage.evaluate(() => document.querySelector('#settings-toggle').click());
      await invitedPage.waitForSelector('#settings-users-tab:not([hidden]):not(.settings-tab-pending)');
      await invitedPage.evaluate(() => document.querySelector('#settings-users-tab').click());
      await invitedPage.waitForSelector('#settings-users .user-accounts .user-row[data-user-id]');
      const adminView = await invitedPage.$$eval('#settings-users .user-accounts .user-row', rows => rows.map(row => ({ login: row.querySelector('strong').textContent, badge: row.querySelector('.user-badge')?.textContent || '', toggle: !!row.querySelector('.user-admin-toggle'), buttons: [...row.querySelectorAll('button')].map(button => button.textContent) })));
      assert.ok(adminView.length >= 2 && adminView.every(row => !row.toggle), 'only the owner sees the Admin toggle');
      const adminsOwnerRow = adminView.find(row => row.login === 'owner');
      assert.deepEqual([adminsOwnerRow.badge, adminsOwnerRow.buttons], ['Owner', []], "an admin is offered no reset or deactivate for the owner");
      assert.equal(adminView.find(row => row.login === 'invitee').badge, 'Admin');
      await invitedPage.evaluate(() => document.querySelector('#settings-close').click());
      // Taking the flag away ends the reach at once.
      await first.evaluate(() => [...document.querySelectorAll('#settings-users .user-accounts .user-row')].find(row => row.querySelector('strong').textContent === 'invitee').querySelector('.user-admin-toggle input').click());
      await first.waitForFunction(() => ![...document.querySelectorAll('#settings-users .user-accounts .user-row')].some(row => row.querySelector('strong').textContent === 'invitee' && row.querySelector('.user-badge')));
      assert.deepEqual(await flagOf(invitedPage), [false, false], 'with the flag gone the account is ordinary again');
      // An admin who is in no project still sees the accounts (they do not wait for a project to be chosen).
      const asOwner = (method, url, body) => first.evaluate(async (m, u, b) => {
        const { csrf_token } = await (await fetch('/api/me')).json();
        const response = await fetch(u, { method: m, headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: b === undefined ? undefined : JSON.stringify(b) });
        return { status: response.status, body: await response.json().catch(() => ({})) };
      }, method, url, body);
      const link = await asOwner('POST', '/api/invites', { kind: 'join', project_id: actualProjectID, role: 'viewer' });
      const joined = await fetch(`${origin}/api/invites/redeem`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ token: link.body.token, login: 'nobody', password: 'a secure password' }) });
      assert.equal(joined.status, 201);
      const nobodyID = (await joined.json()).user_id;
      assert.equal((await asOwner('DELETE', `/api/projects/${actualProjectID}/members/${nobodyID}`)).status, 204);
      assert.equal((await asOwner('PUT', `/api/users/${nobodyID}/admin`, { admin: true })).status, 204);
      const nobodyContext = await browser.createBrowserContext(), nobodyPage = await nobodyContext.newPage();
      await nobodyPage.goto(origin);
      await nobodyPage.locator('#login-form [name=login]').fill('nobody'); await nobodyPage.locator('#login-form [name=password]').fill('a secure password'); await nobodyPage.locator('#login-form button').click();
      await appReady(nobodyPage);
      assert.equal(await nobodyPage.$eval('#startup-status', el => el.hidden), true, 'an admin sign-in finishes its startup checks');
      await nobodyPage.evaluate(() => document.querySelector('#settings-toggle').click());
      await nobodyPage.waitForSelector('#settings-users-tab:not([hidden]):not(.settings-tab-pending)');
      await nobodyPage.evaluate(() => document.querySelector('#settings-users-tab').click());
      await nobodyPage.waitForSelector('#settings-users .user-accounts .user-row[data-user-id]');
      assert.ok(await nobodyPage.$$eval('#settings-users .user-accounts .user-row', rows => rows.length) >= 3, 'an admin without a project sees every account');
      assert.ok((await nobodyPage.$$eval('#users-project option', options => options.map(option => option.value))).includes(actualProjectID), 'and is offered every project, though a member of none');
      assert.ok(await nobodyPage.$$eval('#settings-users .user-members .user-row', rows => rows.length) > 0, 'and sees the members of the chosen project');
      assert.equal(await nobodyPage.$eval('#settings-users .user-error, #settings-users .error', el => el.textContent), '', 'without an error');
      // The machines are readable to an admin, who cannot pair, approve or revoke them: those controls are off, with the reason.
      await nobodyPage.evaluate(() => document.querySelector('#settings-machines-tab').click());
      await nobodyPage.waitForFunction(() => document.querySelector('#machines-list .machine-card') && !document.querySelector('#machines-error').textContent);
      assert.deepEqual(await nobodyPage.evaluate(() => ({ pair: [document.querySelector('#machine-add').disabled, document.querySelector('#machine-add').title],
        cardButtons: [...document.querySelectorAll('#machines-list .machine-card-actions button')].every(button => button.disabled && /^Only the owner can /.test(button.title)) })),
        { pair: [true, 'Only the owner can pair machines'], cardButtons: true }, 'an admin sees the machines with pairing, approving and revoking off');
      await nobodyContext.close();
      await domClick(first, '#settings-close');
    }
    // A resync that lands while the reader opens another room must not cancel that navigation. This is the new-DM
    // flake: the stream's catch-up snapshot re-selected the room it had loaded from, the DM selection saw a newer
    // selection generation and gave up, and the page stayed on the old room. The race is forced in both orders by
    // holding the room-list requests, handing the stream a `snapshot_required` frame, and releasing them by hand:
    //   snapshot-first: the snapshot's room list is held, the reader opens another room, the snapshot is let through
    //                   and finishes (the stream reconnects), and only then does the reader's refresh return.
    //   reader-first:   the reader's click is in flight (its refresh is held) when the resync starts; the snapshot's
    //                   list returns first and the snapshot finishes before the reader's refresh does.
    for (const order of ['snapshot-first', 'reader-first']) {
      const raceContext = await browser.createBrowserContext(), racePage = await raceContext.newPage();
      await racePage.setViewport({ width: 1100, height: 720 });
      await racePage.evaluateOnNewDocument(() => {
        const Native = window.WebSocket; window.__sockets = [];
        window.WebSocket = class extends Native { constructor(...args) { super(...args); window.__sockets.push(this); } };
      });
      await login(racePage);
      const rooms = await racePage.$$eval('.room-group', groups => groups.map(group => ({ id: group.dataset.roomId, name: group.querySelector('.room-name')?.textContent || '' })));
      const current = await racePage.evaluate(() => decodeURIComponent(location.hash.slice(1)));
      const other = rooms.find(room => room.id && room.id !== current && /Nested task|UI smoke/.test(room.name));
      assert.ok(other, `no second room to open: ${JSON.stringify(rooms)}`);
      await racePage.evaluate(() => {
        const originalFetch = window.fetch; window.__held = []; window.__holding = true;
        window.fetch = (input, options) => {
          if (window.__holding && /^\/api\/projects\/[^/]+\/rooms$/.test(String(input)) && (!options?.method || options.method === 'GET')) return new Promise((resolve, reject) => window.__held.push(() => originalFetch(input, options).then(resolve, reject)));
          return originalFetch(input, options);
        };
      });
      const resync = () => racePage.evaluate(() => window.__sockets.at(-1).onmessage({ data: JSON.stringify({ type: 'snapshot_required' }) }));
      const click = () => racePage.click(`[data-room-id="${other.id}"] .room-name`);
      const snapshotList = order === 'snapshot-first' ? 0 : 1, readerRefresh = order === 'snapshot-first' ? 1 : 0;
      if (order === 'snapshot-first') { await resync(); await racePage.waitForFunction(() => window.__held.length === 1); await click(); } else { await click(); await racePage.waitForFunction(() => window.__held.length === 1); await resync(); }
      await racePage.waitForFunction(() => window.__held.length === 2);
      await racePage.evaluate(index => { window.__holding = false; window.__held[index](); }, snapshotList);
      await racePage.waitForFunction(() => window.__sockets.length === 2);
      await racePage.evaluate(index => window.__held[index](), readerRefresh);
      await waitDump(racePage, `resync during navigation (${order})`, id => decodeURIComponent(location.hash.slice(1)) === id && document.querySelector('.room-row.active .room-name')?.textContent === document.querySelector(`[data-room-id="${id}"] .room-name`)?.textContent, () => ({
        hash: decodeURIComponent(location.hash), title: document.querySelector('#room-title')?.textContent, active: [...document.querySelectorAll('.room-row.active')].map(row => row.textContent.trim()),
      }), other.id);
      await raceContext.close();
    }
    // Reading while an older page loads. The page that arrives late must be compensated for what it inserts above the reader,
    // not for how far the reader scrolled meanwhile. This is the scroll-anchor flake: loadOlder measured its anchor before the
    // request, so a reader who scrolled while a slow request was in flight was yanked back by that distance when the page
    // arrived (the unpinned-list asserts saw shifts of 379 and ~20000 px under load). The request is held to force it.
    {
      const olderContext = await browser.createBrowserContext(), olderPage = await olderContext.newPage();
      await olderPage.setViewport({ width: 1100, height: 720 });
      await login(olderPage);
      const olderRoom = await olderPage.evaluate(async () => {
        const { csrf_token } = await (await fetch('/api/me')).json();
        const call = async (url, body) => { const response = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify(body) }); if (!response.ok) throw new Error(`${url}: ${response.status}`); return response.json(); };
        const project = (await (await fetch('/api/projects')).json())[0];
        const home = (await (await fetch(`/api/projects/${project.id}/rooms`)).json()).find(room => room.kind === 'home');
        const room = await call(`/api/projects/${project.id}/rooms`, { title: 'Older history', parent_id: home.id, worktree: { enabled: false, base_ref: '' } });
        for (let start = 0; start < 130; start += 10) await Promise.all(Array.from({ length: 10 }, (_, i) => call(`/api/rooms/${room.id}/messages`, { body: `History line ${start + i}` })));
        return room.id;
      });
      await olderPage.reload();
      await appReady(olderPage);
      await olderPage.waitForFunction(() => document.querySelector('#connection')?.getAttribute('aria-label') === 'Connected');
      await olderPage.$eval(`[data-room-id="${olderRoom}"] .room-name`, el => el.click());
      await olderPage.waitForFunction(() => document.querySelectorAll('#messages .message').length === 100 && document.querySelector('#messages').dataset.hasOlder === 'true');
      await olderPage.evaluate(() => {
        const originalFetch = window.fetch;
        window.fetch = (input, options) => /\/messages\?before=/.test(String(input)) ? new Promise((resolve, reject) => { window.__releaseOlder = () => originalFetch(input, options).then(resolve, reject); }) : originalFetch(input, options);
        const list = document.querySelector('#messages');
        list.scrollTop = 0; // near the top: the list asks for the older page, and the request is held
      });
      await olderPage.waitForFunction(() => Boolean(window.__releaseOlder));
      const probe = await olderPage.evaluate(() => {
        const list = document.querySelector('#messages');
        list.scrollTop = 1500; // the reader moves on while the page is in flight
        return new Promise(resolve => setTimeout(() => {
          const top = list.getBoundingClientRect().top;
          window.__probe = [...list.querySelectorAll('.message')].find(el => el.getBoundingClientRect().top >= top + 20);
          resolve({ top: window.__probe.getBoundingClientRect().top, scrollTop: list.scrollTop });
        }, 150));
      });
      assert.ok(probe.scrollTop > 1000, `the reader did not move away: ${JSON.stringify(probe)}`);
      await olderPage.evaluate(() => window.__releaseOlder());
      await olderPage.waitForFunction(() => document.querySelectorAll('#messages .message').length > 100);
      await new Promise(resolve => setTimeout(resolve, 300));
      const moved = await olderPage.evaluate(top => ({ connected: window.__probe.isConnected, shift: Math.abs(window.__probe.getBoundingClientRect().top - top), count: document.querySelectorAll('#messages .message').length }), probe.top);
      assert.ok(moved.connected && moved.shift < 2, `an older page that arrived while the reader scrolled moved them: ${JSON.stringify({ probe, moved })}`);
      // The thread panel has the same load-older-on-scroll path and the same hazard.
      const threadRoom = await olderPage.evaluate(async () => {
        const { csrf_token } = await (await fetch('/api/me')).json();
        const call = async (url, body) => { const response = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify(body) }); if (!response.ok) throw new Error(`${url}: ${response.status}`); return response.json(); };
        const project = (await (await fetch('/api/projects')).json())[0];
        const home = (await (await fetch(`/api/projects/${project.id}/rooms`)).json()).find(room => room.kind === 'home');
        const room = await call(`/api/projects/${project.id}/rooms`, { title: 'Older thread', parent_id: home.id, worktree: { enabled: false, base_ref: '' } });
        const root = await call(`/api/rooms/${room.id}/messages`, { body: 'Thread root for older replies' });
        for (let start = 0; start < 130; start += 10) await Promise.all(Array.from({ length: 10 }, (_, i) => call(`/api/rooms/${room.id}/messages`, { body: `Reply ${start + i}`, thread_root: root.id, reply_to: root.id })));
        return room.id;
      });
      await olderPage.reload();
      await appReady(olderPage);
      await olderPage.waitForFunction(() => document.querySelector('#connection')?.getAttribute('aria-label') === 'Connected');
      await olderPage.$eval(`[data-room-id="${threadRoom}"] .room-name`, el => el.click());
      // The room list shows the latest 100 messages, all of them replies (not rendered there); the root is on the older page.
      await waitDump(olderPage, 'Older thread room opens', () => document.querySelector('#messages').dataset.hasOlder === 'true' && document.querySelector('#room-title')?.textContent === 'Older thread', () => ({
        hash: decodeURIComponent(location.hash), title: document.querySelector('#room-title')?.textContent, hasOlder: document.querySelector('#messages').dataset.hasOlder, messages: document.querySelectorAll('#messages .message').length,
        active: [...document.querySelectorAll('.room-row.active')].map(row => row.textContent.trim().slice(0, 40)), rows: [...document.querySelectorAll('[data-room-id] .room-name')].map(row => row.textContent.trim().slice(0, 30)),
        connection: document.querySelector('#connection')?.getAttribute('aria-label'), error: document.querySelector('#chat-error')?.textContent, busy: document.querySelector('#app-view')?.getAttribute('aria-busy'),
      }));
      await olderPage.$eval('#messages', el => { el.scrollTop = 0; });
      await olderPage.waitForFunction(() => [...document.querySelectorAll('#messages .message')].some(el => el.textContent.includes('Thread root for older replies')));
      await olderPage.evaluate(() => [...document.querySelectorAll('#messages .message')].find(el => el.textContent.includes('Thread root for older replies')).querySelector('.thread-action').click());
      await waitDump(olderPage, 'older thread opens with 101 entries', () => document.querySelectorAll('#thread-messages .thread-entry').length === 101 && document.querySelector('#thread-messages').dataset.hasOlder === 'true', () => {
        const list = document.querySelector('#thread-messages');
        return { entries: list.querySelectorAll('.thread-entry').length, hasOlder: list.dataset.hasOlder, scrollTop: Math.round(list.scrollTop), scrollHeight: list.scrollHeight, clientHeight: list.clientHeight, olderStatusHidden: document.querySelector('.thread-older-status, #thread-older-status')?.hidden, error: document.querySelector('#thread-error')?.textContent, panelHidden: document.querySelector('#thread-panel').hidden, title: document.querySelector('#thread-panel h2')?.textContent, rootsInRoom: document.querySelectorAll('#messages .message').length };
      });
      if (process.env.UI_STOP_AFTER_OLDER_THREAD) { console.log('older thread reached; stopping early (UI_STOP_AFTER_OLDER_THREAD)'); throw Object.assign(new Error('stop-early'), { stopEarly: true }); }
      await olderPage.evaluate(() => {
        const originalFetch = window.fetch; window.__releaseOlder = null;
        window.fetch = (input, options) => /thread_root=[^&]+&before=/.test(String(input)) ? new Promise((resolve, reject) => { window.__releaseOlder = () => originalFetch(input, options).then(resolve, reject); }) : originalFetch(input, options);
        document.querySelector('#thread-messages').scrollTop = 0;
      });
      await olderPage.waitForFunction(() => Boolean(window.__releaseOlder));
      const threadProbe = await olderPage.evaluate(() => {
        const list = document.querySelector('#thread-messages');
        list.scrollTop = 1500;
        return new Promise(resolve => setTimeout(() => {
          const top = list.getBoundingClientRect().top;
          window.__probe = [...list.querySelectorAll('.thread-entry')].find(el => el.getBoundingClientRect().top >= top + 20);
          resolve({ top: window.__probe.getBoundingClientRect().top, scrollTop: list.scrollTop });
        }, 150));
      });
      assert.ok(threadProbe.scrollTop > 1000, `the reader did not move away in the thread: ${JSON.stringify(threadProbe)}`);
      await olderPage.evaluate(() => window.__releaseOlder());
      await olderPage.waitForFunction(() => document.querySelectorAll('#thread-messages .thread-entry').length > 101);
      await new Promise(resolve => setTimeout(resolve, 300));
      const threadMoved = await olderPage.evaluate(top => ({ connected: window.__probe.isConnected, shift: Math.abs(window.__probe.getBoundingClientRect().top - top), count: document.querySelectorAll('#thread-messages .thread-entry').length }), threadProbe.top);
      assert.ok(threadMoved.connected && threadMoved.shift < 2, `older replies that arrived while the reader scrolled the thread moved them: ${JSON.stringify({ threadProbe, threadMoved })}`);
      await olderContext.close();
    }
    await invitedPage.evaluate(() => {
      const originalFetch = window.fetch;
      let held = false;
      window.__emptyNextAccount = false;
      window.__oldRoomSignal = null;
      window.__releaseOldRooms = null;
      window.fetch = (input, options) => {
        const path = String(input);
        if (window.__emptyNextAccount && path === '/api/projects') return Promise.resolve(new Response('[]', { headers: { 'Content-Type': 'application/json' } }));
        if (!held && /^\/api\/projects\/[^/]+\/rooms$/.test(path) && (!options?.method || options.method === 'GET')) {
          held = true;
          window.__oldRoomSignal = options.signal;
          const projectID = path.split('/')[3];
          return new Promise(resolve => { window.__releaseOldRooms = () => resolve(new Response(JSON.stringify([{
            id: 'room-private-before-logout', project_id: projectID, kind: 'dm', title: 'Private invitee room', message_count: 1,
          }]), { headers: { 'Content-Type': 'application/json' } })); });
        }
        return originalFetch(input, options);
      };
    });
    await domClick(invitedPage, `[data-room-id="${peopleDMID}"] .room-name`);
    await invitedPage.waitForFunction(() => Boolean(window.__releaseOldRooms));
    await domClick(invitedPage, '#logout');
    await invitedPage.waitForSelector('#login-view:not([hidden])');
    assert.equal(await invitedPage.$eval('#room-title', el => el.textContent), 'Choose a room', 'logout left the prior chat header visible');
    await invitedPage.evaluate(() => { window.__emptyNextAccount = true; });
    await invitedPage.locator('#login-form [name=login]').fill('owner');
    await invitedPage.locator('#login-form [name=password]').fill('test-password-123');
    await invitedPage.click('#login-form button');
    await appReady(invitedPage);
    assert.equal(await invitedPage.$eval('#usages-toggle', el => el.hidden), false, 'owner sign-in left Usages hidden');
    assert.equal(await invitedPage.$eval('#room-title', el => el.textContent), 'Choose a room', 'empty next account inherited prior chat header');
    const oldRoomLeak = await invitedPage.evaluate(async () => {
      window.__releaseOldRooms();
      await new Promise(resolve => setTimeout(resolve, 0));
      return { aborted: window.__oldRoomSignal.aborted,
        rendered: document.querySelector('#projects').textContent.includes('Private invitee room'),
        saved: Object.keys(localStorage).some(key => key.endsWith(':room-private-before-logout')),
        header: document.querySelector('#room-title').textContent };
    });
    assert.deepEqual(oldRoomLeak, { aborted: true, rendered: false, saved: false, header: 'Choose a room' }, 'old room list crossed a different-user sign-in');
    await domClick(invitedPage, '#logout');
    await invitedPage.waitForSelector('#login-view:not([hidden])');
    await invitedPage.evaluate(() => { window.__emptyNextAccount = false; });
    await invitedPage.locator('#login-form [name=login]').fill('invitee');
    await invitedPage.locator('#login-form [name=password]').fill('a secure password');
    await domClick(invitedPage, '#login-form button');
    await appReady(invitedPage);
    await waitRoomRow(invitedPage, 'invitee after sign-in', peopleDMID);
    await domClick(invitedPage, `[data-room-id="${peopleDMID}"] .room-name`);
    await invitedPage.waitForFunction(id => decodeURIComponent(location.hash.slice(1)) === id && document.querySelector('#room-title')?.textContent !== 'Choose a room', {}, peopleDMID);
    await invitedPage.evaluate(roomID => {
      const previousFetch = window.fetch;
      window.__releaseOldUpload = null;
      window.__oldUploadDeleted = false;
      window.__oldUploadSignal = null;
      window.fetch = (input, options) => {
        const target = `/api/rooms/${roomID}/attachments`;
        if (String(input) === target && options?.method === 'POST') return new Promise(resolve => {
          window.__oldUploadSignal = options.signal;
          window.__releaseOldUpload = () => resolve(new Response(JSON.stringify({ id: 'att-old-session', room_id: roomID, name: 'Private old upload', mime: 'image/png', url: `${target}/att-old-session` }), { headers: { 'Content-Type': 'application/json' } }));
        });
        if (String(input) === `${target}/att-old-session` && options?.method === 'DELETE') { window.__oldUploadDeleted = true; return Promise.resolve(new Response(null, { status: 204 })); }
        return previousFetch(input, options);
      };
    }, peopleDMID);
    await (await invitedPage.$('#attachment-input')).uploadFile(imagePath);
    await invitedPage.waitForFunction(() => Boolean(window.__releaseOldUpload) && document.querySelector('#pending-attachments')?.textContent.includes('Uploading 1'));
    await invitedPage.click('#logout');
    await invitedPage.waitForSelector('#login-view:not([hidden])');
    assert.equal(await invitedPage.evaluate(() => window.__oldUploadSignal?.aborted), true, 'logout did not abort the old upload');
    await invitedPage.locator('#login-form [name=login]').fill('owner');
    await invitedPage.locator('#login-form [name=password]').fill('test-password-123');
    await domClick(invitedPage, '#login-form button');
    await appReady(invitedPage);
    await waitRoomRow(invitedPage, 'invitee after sign-in', peopleDMID);
    await domClick(invitedPage, `[data-room-id="${peopleDMID}"] .room-name`);
    await invitedPage.waitForFunction(id => decodeURIComponent(location.hash.slice(1)) === id && document.querySelector('#room-title')?.textContent !== 'Choose a room', {}, peopleDMID);
    await invitedPage.evaluate(() => window.__releaseOldUpload());
    await invitedPage.waitForFunction(() => window.__oldUploadDeleted);
    assert.deepEqual(await invitedPage.evaluate(() => ({ pending: document.querySelector('#pending-attachments').textContent, files: document.querySelectorAll('#pending-attachments .pending-image').length, sendDisabled: document.querySelector('#composer button[type=submit]').disabled })),
      { pending: '', files: 0, sendDisabled: false }, 'old upload changed the new account composer');
    await invitedPage.locator('#composer textarea').fill('Post-reset upload check');
    await invitedPage.keyboard.press('Enter');
    await invitedPage.waitForFunction(() => [...document.querySelectorAll('#messages .message-body')].some(node => node.textContent.includes('Post-reset upload check')));
    await domClick(invitedPage, '#logout');
    await invitedPage.waitForSelector('#login-view:not([hidden])');
    await invitedPage.locator('#login-form [name=login]').fill('invitee');
    await invitedPage.locator('#login-form [name=password]').fill('a secure password');
    await domClick(invitedPage, '#login-form button');
    await appReady(invitedPage);
    await waitRoomRow(invitedPage, 'invitee after sign-in', peopleDMID);
    await domClick(invitedPage, `[data-room-id="${peopleDMID}"] .room-name`);
    await invitedPage.waitForFunction(id => decodeURIComponent(location.hash.slice(1)) === id && document.querySelector('#room-title')?.textContent !== 'Choose a room', {}, peopleDMID);
    await invitedPage.evaluate(roomID => {
      const previousFetch = window.fetch;
      window.__releaseHeldSend = null;
      window.fetch = (input, options) => {
        if (String(input) === `/api/rooms/${roomID}/messages` && options?.method === 'POST' && options.body?.includes('Held invitee send')) {
          return new Promise(resolve => { window.__releaseHeldSend = () => resolve(new Response(JSON.stringify({ id: 'msg-old-held-send', room_id: roomID, seq: 999, body: 'Held invitee send', author_kind: 'user', author_id: 'invitee', created_at: new Date().toISOString() }), { headers: { 'Content-Type': 'application/json' } })); });
        }
        return previousFetch(input, options);
      };
    }, peopleDMID);
    await invitedPage.locator('#composer textarea').fill('Held invitee send');
    await invitedPage.keyboard.press('Enter');
    await invitedPage.waitForFunction(() => Boolean(window.__releaseHeldSend));
    await domClick(invitedPage, '#logout');
    await invitedPage.waitForSelector('#login-view:not([hidden])');
    await invitedPage.locator('#login-form [name=login]').fill('owner');
    await invitedPage.locator('#login-form [name=password]').fill('test-password-123');
    await domClick(invitedPage, '#login-form button');
    await appReady(invitedPage);
    await waitRoomRow(invitedPage, 'invitee after sign-in', peopleDMID);
    // Sign-in can restore this room from the URL. Clicking it again starts a
    // second asynchronous load and can clear its messages between assertions.
    if (await invitedPage.evaluate(id => decodeURIComponent(location.hash.slice(1)) !== id, peopleDMID)) {
      await domClick(invitedPage, `[data-room-id="${peopleDMID}"] .room-name`);
    }
    await invitedPage.waitForFunction(() => [...document.querySelectorAll('#messages .message-body')].some(node => node.textContent === 'Post-reset upload check'));
    await invitedPage.locator('#composer textarea').fill('Owner draft stays');
    await invitedPage.evaluate(() => {
      const body = [...document.querySelectorAll('#messages .message-body')].find(node => node.textContent === 'Post-reset upload check');
      body.closest('.message').querySelector('.message-actions .message-reply').click();
    });
    const keptDraft = await invitedPage.evaluate(async () => {
      window.__releaseHeldSend();
      await new Promise(resolve => setTimeout(resolve, 0));
      return { draft: document.querySelector('#composer textarea').value, reply: !document.querySelector('#reply-chip').hidden };
    });
    assert.deepEqual(keptDraft, { draft: 'Owner draft stays', reply: true }, 'old send cleared the new account draft or reply');
    assert.equal(await invitedPage.$eval('#messages', el => el.textContent.includes('Held invitee send')), false, 'old send response appeared in new account chat');
    await invitedPage.keyboard.press('Enter');
    await invitedPage.waitForFunction(() => [...document.querySelectorAll('#messages .message-body')].some(node => node.textContent.includes('Owner draft stays')));
    await invitedContext.close();
    if (process.env.AIMEBU_UI_SCREENSHOT_PREFIX) {
      await first.screenshot({ path: `${process.env.AIMEBU_UI_SCREENSHOT_PREFIX}-desktop.png` });
      await first.setViewport({ width: 390, height: 780 });
      await first.waitForFunction(() => document.querySelector('.sidebar').getBoundingClientRect().right < 0 && document.querySelector('#agents-panel').getBoundingClientRect().left > innerWidth);
      await first.screenshot({ path: `${process.env.AIMEBU_UI_SCREENSHOT_PREFIX}-phone.png` });
      await domClick(first, '.mobile-tab[data-tab="agents"]');
      await first.waitForFunction(() => Math.abs(document.querySelector('#agents-panel').getBoundingClientRect().right - innerWidth) < 1);
      await first.screenshot({ path: `${process.env.AIMEBU_UI_SCREENSHOT_PREFIX}-agents-drawer.png` });
      await domClick(first, '#agents-close');
      await domClick(first, '.mobile-tab[data-tab="rooms"]');
      await first.waitForFunction(() => Math.abs(document.querySelector('.sidebar').getBoundingClientRect().left) < 1);
      await first.screenshot({ path: `${process.env.AIMEBU_UI_SCREENSHOT_PREFIX}-rooms-drawer.png` });
      await first.setViewport({ width: 1100, height: 720 });
    }
    await first.evaluateOnNewDocument(() => {
      window.__agentReadsAfterReload = 0;
      window.__soundPlaysAfterReload = 0;
      HTMLMediaElement.prototype.play = function() { window.__soundPlaysAfterReload++; return Promise.resolve(); };
      const originalFetch = window.fetch.bind(window);
      window.fetch = (input, options) => {
        if (/^\/api\/rooms\/[^/]+\/agents$/.test(String(input))) window.__agentReadsAfterReload++;
        return originalFetch(input, options);
      };
      window.WebSocket = new Proxy(window.WebSocket, { construct(target, args) {
        if (String(args[0]).includes('/api/events')) window.__eventSocketURL = String(args[0]);
        return Reflect.construct(target, args);
      } });
    });
    await first.reload();
    // A scheduled note can change the agent's state while the page reloads.
    // Wait for a hydrated card and live connection before checking replay.
    await first.waitForFunction(id => !!document.querySelector(`[data-agent-id="${id}"] .agent-state`)?.textContent
      && document.querySelector('#connection')?.getAttribute('aria-label') === 'Connected', {}, realAgentID);
    await new Promise(resolve => setTimeout(resolve, 300));
    const replay = await first.evaluate(() => ({ reads: window.__agentReadsAfterReload, sounds: window.__soundPlaysAfterReload, after: new URL(window.__eventSocketURL).searchParams.get('after') }));
    assert.ok(Number(replay.after) > 0, `event stream restarted from history: ${JSON.stringify(replay)}`);
    assert.ok(replay.reads <= 2, `agent history replayed after reload: ${JSON.stringify(replay)}`);
    assert.equal(replay.sounds, 0, `historical inbox items chimed after reload: ${JSON.stringify(replay)}`);
    const reviewContext = await browser.createBrowserContext();
    const reviewPage = await reviewContext.newPage();
    await login(reviewPage);
    const memoryView = await reviewPage.evaluate(async () => {
      const { KnowledgePanel } = await import('/js/knowledge.js');
      const panel = new KnowledgePanel(() => ({ projects: [], selectedProjectID: '' }), () => null);
      panel.mode = 'project';
      panel.data = { enabled: true, can_edit: true, cap_bytes: 4096, records: [
        { id: 'knw_imported', scope_id: 'prj_test', body: 'brought over from v1', rev: 3, author_kind: 'user', author_id: 'alice', updated_at: '2026-10-01', active: true, _edit: true },
      ] };
      panel.render();
      const record = panel.records.querySelector('article');
      const buttons = [...record.querySelectorAll('button')].map(button => button.getAttribute('aria-label') || button.textContent);
      panel.data.can_edit = false;
      panel.data.records[0]._edit = false;
      panel.render();
      const viewer = panel.records.querySelector('article');
      return { buttons, readOnly: record.querySelector('textarea').readOnly, status: panel.status.textContent, review: !!panel.dialog.querySelector('input[data-review-id]') || /review/i.test(panel.dialog.textContent),
        viewerReadOnly: viewer.querySelector('textarea').readOnly, viewerButtons: viewer.querySelectorAll('button').length };
    });
    assert.deepEqual(memoryView.buttons, ['Save', 'Delete'], `an imported record is edited like any other: ${JSON.stringify(memoryView)}`);
    assert.equal(memoryView.readOnly, false);
    assert.equal(memoryView.review, false, 'Knowledge has no imported-review controls');
    assert.ok(!/pending/i.test(memoryView.status), memoryView.status);
    assert.deepEqual([memoryView.viewerReadOnly, memoryView.viewerButtons], [true, 0]);
    await reviewContext.close();
    // The navbar is v1's height and every control in it is one icon button: an svg or mask glyph, a tooltip and an accessible name,
    // no text of its own, one box size, and a count that is a corner pill that never widens its button.
    const navbar = await first.evaluate(() => {
      const bar = document.querySelector('.workspace-bar'), buttons = [...bar.querySelectorAll('nav button')].filter(button => !button.classList.contains('ui-variant-toggle') && button.getClientRects().length);
      const widthOf = id => document.querySelector(id).getBoundingClientRect().width;
      const badge = document.querySelector('#inbox-count'), wasHidden = badge.hidden, text = badge.textContent, before = widthOf('#inbox-toggle');
      badge.hidden = false; badge.textContent = '99'; const withBadge = widthOf('#inbox-toggle'), badgeBox = badge.getBoundingClientRect(), buttonBox = document.querySelector('#inbox-toggle').getBoundingClientRect();
      badge.hidden = wasHidden; badge.textContent = text;
      return {
        height: bar.getBoundingClientRect().height,
        buttons: buttons.map(button => button.id), glyph: buttons.every(button => button.querySelector('svg, img, .icon-mask')), named: buttons.every(button => button.getAttribute('aria-label') && button.title),
        bareText: buttons.map(button => [...button.childNodes].filter(node => node.nodeType === Node.TEXT_NODE && node.textContent.trim()).length).reduce((a, b) => a + b, 0),
        sizes: [...new Set(buttons.map(button => `${Math.round(button.getBoundingClientRect().width)}x${Math.round(button.getBoundingClientRect().height)}`))], badgeKeepsWidth: before === withBadge,
        badgeInside: badgeBox.right <= buttonBox.right + 1 && badgeBox.top >= buttonBox.top - 1,
      };
    });
    assert.equal(navbar.height, 53, "the navbar is v1's 53px"); assert.ok(navbar.buttons.includes('inbox-toggle') && navbar.buttons.includes('health-toggle') && navbar.buttons.includes('logout'), JSON.stringify(navbar.buttons));
    assert.deepEqual([navbar.glyph, navbar.named, navbar.bareText, navbar.sizes.length, navbar.badgeKeepsWidth, navbar.badgeInside], [true, true, 0, 1, true, true], `navbar buttons are uniform icon buttons: ${JSON.stringify(navbar)}`);
    // Close, plus, chevron and scroll helpers that used to be a typed character or a word are icon buttons now.
    const chrome = await iconOnlyReport(first, ['#project-open', '#project-cancel', '#back', '#cancel-reply', '#image-lightbox-close', '#close-thread', '#agents-close', '#agent-profile-close', '#settings-close', '#knowledge-close', '#room-menu-toggle', '#message-debug-close', '.message-debug-nav', '.add-room', '.back-to-latest', '#agents-toggle', '#rooms-toggle']);
    assert.deepEqual(chrome.bad, [], 'every close, plus, chevron and scroll button is an icon with a tooltip and a name'); assert.ok(chrome.found >= 15, `icon buttons found: ${chrome.found}`);
    // Sign out is an icon-only button in the top bar, sized like its neighbours, and still signs out.
    assert.deepEqual(await first.evaluate(() => { const button = document.querySelector('#logout'), neighbour = document.querySelector('#knowledge-toggle'); const rect = button.getBoundingClientRect(), other = neighbour.getBoundingClientRect(); return { icon: button.matches('button.icon-btn'), text: button.textContent.trim(), name: button.getAttribute('aria-label'), title: button.title, svg: !!button.querySelector('svg[aria-hidden=true]'), sameSize: Math.abs(rect.width - other.width) <= 1 && Math.abs(rect.height - other.height) <= 1 }; }), { icon: true, text: '', name: 'Sign out', title: 'Sign out', svg: true, sameSize: true }, 'Sign out is an icon-only top-bar button');
    await domClick(first, '#logout');
    await first.waitForSelector('#login-view:not([hidden])');
    // Create the away room from a page with no app running in it: a second sign-in inside `second` raced that tab's
    // own session handling and the first write failed with "invalid CSRF token".
    const awayContext = await browser.createBrowserContext();
    const awayPage = await awayContext.newPage();
    await awayPage.goto(origin);
    const awayRoomID = await awayPage.evaluate(async projectID => {
      const login = await fetch('/api/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ login: 'owner', password: 'test-password-123' }) });
      if (!login.ok) throw new Error(`second owner sign-in failed: ${login.status}`);
      const { csrf_token: csrf } = await login.json();
      const post = async (path, body) => {
        const response = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify(body) });
        if (!response.ok) throw new Error(`${path}: ${response.status} ${await response.text()}`);
        return response.json();
      };
      const room = await post(`/api/projects/${projectID}/rooms`, { title: 'Created while signed out' });
      await post(`/api/rooms/${room.id}/messages`, { body: 'History while away' });
      return room.id;
    }, actualProjectID);
    await awayContext.close();
    await first.locator('#login-form [name=login]').fill('owner');
    await first.locator('#login-form [name=password]').fill('test-password-123');
    await domClick(first, '#login-form button');
    await appReady(first);
    await first.waitForFunction(id => !!document.querySelector(`[data-room-id="${id}"]`), {}, awayRoomID);
    const awayRoom = await first.evaluate(async id => {
      const { currentUser } = await import('/js/api.js');
      const badge = document.querySelector(`[data-room-id="${id}"] .unread-count`);
      return { unread: badge && !badge.hidden ? badge.textContent : '',
        user: currentUser, room: document.querySelector(`[data-room-id="${id}"]`)?.textContent };
    }, awayRoomID);
    assert.equal(awayRoom.unread, '', `same-user sign-in left room unread: ${JSON.stringify(awayRoom)}`);
    const probeContext = await browser.createBrowserContext();
    const probePage = await probeContext.newPage();
    await probePage.goto(origin);
    await probePage.waitForSelector('#login-view:not([hidden])');
    const resyncUnread = await probePage.evaluate(async () => {
      const { setSession } = await import('/js/api.js'); setSession({ id: 'snapshot-probe-user' });
      const { Sidebar } = await import('/js/sidebar.js');
      const project = { id: 'snapshot-unread-probe', name: 'Snapshot unread probe' };
      const oldRoom = { id: 'snapshot-old-room', project_id: project.id, kind: 'home', title: 'Old room', message_count: 2, unread_count: 0 };
      const newRoom = { id: 'snapshot-new-room', project_id: project.id, kind: 'dm', title: 'New chat', message_count: 1, unread_count: 1 };
      let rooms = [oldRoom];
      const originalFetch = window.fetch;
      window.fetch = (input, options) => {
        if (String(input) === '/api/projects') return Promise.resolve(new Response(JSON.stringify([project]), { headers: { 'Content-Type': 'application/json' } }));
        if (String(input) === `/api/projects/${project.id}/rooms`) return Promise.resolve(new Response(JSON.stringify(rooms), { headers: { 'Content-Type': 'application/json' } }));
        return originalFetch(input, options);
      };
      const sidebar = new Sidebar(document.createElement('div'), () => {});
      try {
        await sidebar.load();
        const oldUnread = sidebar.unreadCount(oldRoom);
        rooms = [oldRoom, newRoom];
        await sidebar.load();
        return { oldUnread, newUnread: sidebar.unreadCount(newRoom) };
      } finally {
        window.fetch = originalFetch;
      }
    });
    await probeContext.close();
    assert.deepEqual(resyncUnread, { oldUnread: 0, newUnread: 1 }, 'live snapshot keeps a newly discovered DM unread');
    const receiptsContext = await browser.createBrowserContext();
    const receiptsPage = await receiptsContext.newPage();
    await receiptsPage.goto(origin);
    // The app's own session check clears the session when it fails, so wait for it before faking a user.
    await receiptsPage.waitForSelector('#login-view:not([hidden])');
    const receiptStrips = await receiptsPage.evaluate(async () => {
      // The server versions module URLs (?v=…); the probe must share the app's api.js instance for currentUser.
      const version = new URL(document.querySelector('script[type=module]').src).search;
      const { setSession } = await import(`/js/api.js${version}`); setSession({ id: 'usr_me' });
      const { ReadReceipts } = await import(`/js/read-receipts.js${version}`);
      const list = document.createElement('div'); document.body.append(list);
      const chat = { list, room: { id: 'receipt-room' }, messages: new Map(), nodes: new Map() };
      for (const [id, seq, author] of [['m1', 1, 'usr_me'], ['m2', 2, 'agt_a'], ['m3', 3, 'usr_me']]) {
        const node = document.createElement('article'); node.className = 'message'; list.append(node);
        chat.nodes.set(id, node); chat.messages.set(id, { id, seq, author_id: author });
      }
      const originalFetch = window.fetch;
      window.fetch = (input, options) => {
        if (String(input) === '/api/rooms/receipt-room/read-receipts') {
          return Promise.resolve(new Response(JSON.stringify([
            { member_kind: 'user', member_id: 'usr_you', label: 'you', seq: 2 },
            { member_kind: 'agent', member_id: 'agt_a', label: 'ada', seq: 2 },
            { member_kind: 'user', member_id: 'usr_me', label: 'me', seq: 3 },
          ]), { headers: { 'Content-Type': 'application/json' } }));
        }
        return originalFetch(input, options);
      };
      try {
        const receipts = new ReadReceipts(chat);
        await receipts.refresh();
        const strips = id => [...chat.nodes.get(id).querySelectorAll(':scope > .message-receipts .receipt-avatar')].map(el => el.title);
        // The markers sit at the message's right edge, whoever wrote it, 14px each.
        const edge = id => { const node = chat.nodes.get(id), avatars = node.querySelectorAll(':scope > .message-receipts .receipt-avatar'), last = avatars[avatars.length - 1].getBoundingClientRect(), box = node.getBoundingClientRect(), cs = getComputedStyle(node); return { gap: Math.round(box.right - parseFloat(cs.paddingRight) - parseFloat(cs.borderRightWidth) - last.right), size: Math.round(last.width) }; };
        return { m1: strips('m1'), m2: strips('m2'), m3: strips('m3'), label: chat.nodes.get('m3').querySelector('.message-receipts')?.getAttribute('aria-label'), edge2: edge('m2'), edge3: edge('m3') };
      } finally { window.fetch = originalFetch; list.remove(); }
    });
    for (const width of [1440, 390]) {
      await receiptsPage.setViewport({ width, height: 800 });
      const fits = await receiptsPage.evaluate(() => {
        const strip = document.createElement('div'); strip.className = 'message-receipts'; document.body.append(strip);
        const out = [];
        for (const text of ['W', 'MA', 'VL', 'WW', 'MM', 'OW']) {
          const avatar = document.createElement('span'); avatar.className = 'receipt-avatar'; avatar.textContent = text; strip.append(avatar);
          const box = avatar.getBoundingClientRect(), style = getComputedStyle(avatar);
          out.push({ text, size: [Math.round(box.width), Math.round(box.height)], fits: (() => { const range = document.createRange(); range.selectNodeContents(avatar); const text = range.getBoundingClientRect(); return text.width <= avatar.clientWidth && text.height <= avatar.clientHeight; })(), noWrap: style.whiteSpace === 'nowrap', clipped: style.overflow === 'hidden' });
        }
        strip.remove(); return out;
      });
      for (const item of fits) assert.deepEqual([item.size, item.fits, item.noWrap, item.clipped], [[14, 14], true, true, true], `seen-by initials "${item.text}" do not fit their circle at ${width}px: ${JSON.stringify(item)}`);
    }
    await receiptsContext.close();
    assert.deepEqual(receiptStrips, { m1: [], m2: ['Seen by you', 'Seen by ada'], m3: ['Seen by me'], label: 'Seen by me', edge2: { gap: 2, size: 14 }, edge3: { gap: 2, size: 14 } }, 'read receipts mark every member (humans, agents and the viewer) on the newest message at or before their position, their own messages included');
    const archiveContext = await browser.createBrowserContext();
    const archivePage = await archiveContext.newPage();
    await archivePage.goto(origin);
    const archiveReset = await archivePage.evaluate(async () => {
      const { Sidebar } = await import('/js/sidebar.js');
      const project = { id: 'archive-reset-probe', name: 'Archive reset probe' };
      const room = { id: 'archive-reset-room', project_id: project.id, kind: 'task', title: 'Archived', lifecycle: 'closed', message_count: 1 };
      const sidebar = new Sidebar(document.createElement('div'), () => {});
      sidebar.projects.set(project.id, project); sidebar.rooms.set(room.id, room); sidebar.render();
      let releaseOld, oldSignal;
      const originalFetch = window.fetch;
      window.fetch = (input, options) => {
        if (String(input).includes('/archive/search?')) {
          oldSignal = options.signal;
          return new Promise(resolve => { releaseOld = () => resolve(new Response(JSON.stringify([{ room_id: room.id, room_name: 'Private chat', snippet: 'Private message snippet' }]), { headers: { 'Content-Type': 'application/json' } })); });
        }
        return originalFetch(input, options);
      };
      try {
        const search = sidebar.archiveSearchNode(project.id);
        search.value = 'same query'; search.dispatchEvent(new Event('input'));
        await new Promise(resolve => setTimeout(resolve, 300));
        if (!releaseOld) throw new Error('archive search did not start');
        sidebar.reset();
        sidebar.projects.set(project.id, project); sidebar.rooms.set(room.id, room); sidebar.render();
        sidebar.archiveFilter.set(project.id, 'same query');
        releaseOld();
        await new Promise(resolve => setTimeout(resolve, 0));
        return { aborted: oldSignal.aborted, leaked: sidebar.archiveHitsNode(project.id).textContent.includes('Private message snippet') };
      } finally { window.fetch = originalFetch; }
    });
    await archiveContext.close();
    assert.deepEqual(archiveReset, { aborted: true, leaked: false }, 'old archive search crossed a sign-in reset');
    // Perf smoke: a 20k-message room must open quickly, and scrolling back must stay bounded in DOM size and layout passes.
    const perfRoomID = await first.evaluate(async projectID => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const response = await fetch(`/api/projects/${projectID}/rooms`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ title: 'Perf history' }) });
      if (!response.ok) throw new Error(`perf room creation: ${response.status}`);
      return (await response.json()).id;
    }, actualProjectID);
    const seedPerf = spawnSync('python3', ['-c', `
import sqlite3, sys
db, room = sys.argv[1:]
con = sqlite3.connect(db)
owner = con.execute("SELECT id FROM user WHERE login='owner'").fetchone()[0]
seq = con.execute("SELECT value FROM seq_counter WHERE id=1").fetchone()[0]
rows = [('msg_perf_%05d' % i, seq+i+1, room, 'user', owner, 'Perf message %05d' % i, '[]', '2026-01-01T00:00:00.000000000Z') for i in range(20000)]
with con:
    con.executemany('INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES (?,?,?,?,?,?,?,?)', rows)
    con.execute('UPDATE seq_counter SET value=? WHERE id=1', (seq+len(rows),))
`, path.join(tmp, 'server', 'aimebu.sqlite'), perfRoomID], { encoding: 'utf8' });
    if (seedPerf.status !== 0) throw new Error(`perf seed failed: ${seedPerf.stderr}`);
    const perfContext = await browser.createBrowserContext();
    const perfPage = await perfContext.newPage();
    await perfPage.evaluateOnNewDocument(() => {
      window.__renderCap = 300; window.__readPosts = [];
      const fetch = window.fetch;
      window.fetch = (input, init) => { if (init?.method === 'POST' && /\/read$/.test(String(input))) window.__readPosts.push(String(input)); return fetch(input, init); };
    });
    await perfPage.goto(`${origin}/#${perfRoomID}`);
    await perfPage.locator('#login-form [name=login]').fill('owner');
    await perfPage.locator('#login-form [name=password]').fill('test-password-123');
    const perfStart = Date.now();
    await perfPage.locator('#login-form button').click();
    await perfPage.waitForFunction(() => document.querySelector('#messages')?.textContent.includes('Perf message 19999'), { timeout: 20000 });
    const perfOpenMs = Date.now() - perfStart;
    assert(perfOpenMs < 15000, `20k-message room took ${perfOpenMs}ms to open`);
    await perfPage.waitForFunction(() => document.querySelector('#connection')?.getAttribute('aria-label') === 'Connected');
    await new Promise(resolve => setTimeout(resolve, 500));
    const perfCdp = await perfPage.createCDPSession();
    await perfCdp.send('Performance.enable');
    const layoutCount = async () => (await perfCdp.send('Performance.getMetrics')).metrics.find(metric => metric.name === 'LayoutCount').value;
    const layoutsBefore = await layoutCount();
    let perfPages = 0;
    for (; perfPages < 8; perfPages++) {
      const before = await perfPage.evaluate(() => document.querySelector('#messages .message')?.dataset.messageId);
      await perfPage.evaluate(() => { document.querySelector('#messages').scrollTop = 0; });
      await perfPage.waitForFunction(id => document.querySelector('#messages .message')?.dataset.messageId !== id, { timeout: 15000 }, before);
    }
    const perfLayouts = (await layoutCount()) - layoutsBefore;
    const perfDom = await perfPage.evaluate(() => ({ messages: document.querySelectorAll('#messages .message').length, button: [...document.querySelectorAll('.back-to-latest')].map(button => button.className + ':' + button.hidden).join() }));
    assert(perfDom.messages <= 300, `scrolling back kept ${perfDom.messages} messages in the DOM`);
    assert(perfDom.button.startsWith('icon-btn back-to-latest:false'), `a capped history window did not offer Back to latest: ${JSON.stringify(perfDom)} after ${perfPages} pages`);
    assert(perfLayouts < perfPages * 40, `scrolling back ran ${perfLayouts} layouts for ${perfPages} pages`);
    // While the window hides the live end, an arrival is counted on Back to latest and announced, and the open room stays unread.
    const seedPerfRows = (...ids) => {
      const result = spawnSync('python3', ['-c', `
import sqlite3, sys
db, room, ids = sys.argv[1], sys.argv[2], sys.argv[3:]
con = sqlite3.connect(db)
owner = con.execute("SELECT id FROM user WHERE login='owner'").fetchone()[0]
with con:
    for id in ids:
        seq = con.execute("SELECT value FROM seq_counter WHERE id=1").fetchone()[0] + 1
        con.execute('INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES (?,?,?,?,?,?,?,?)', (id, seq, room, 'user', owner, 'Perf late ' + id, '[]', '2026-01-01T00:00:00.000000000Z'))
        con.execute('UPDATE seq_counter SET value=? WHERE id=1', (seq,))
`, path.join(tmp, 'server', 'aimebu.sqlite'), perfRoomID, ...ids], { encoding: 'utf8' });
      if (result.status !== 0) throw new Error(`perf rows seed failed: ${result.stderr}`);
    };
    const postAsOwner = (page, roomID, body) => page.evaluate(async (roomID, body) => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const response = await fetch(`/api/rooms/${roomID}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ body }) });
      if (!response.ok) throw new Error(`post: ${response.status}`);
    }, roomID, body);
    await postAsOwner(first, perfRoomID, 'Perf live arrival');
    await waitDump(perfPage, 'missed arrival count', () => document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)')?.getAttribute('aria-label') === 'Back to latest · 1 new', () => ({ button: document.querySelector('.back-to-latest')?.getAttribute('aria-label') }));
    await perfPage.waitForFunction(() => document.querySelector('#live-announcer')?.textContent.includes('Perf live arrival'));
    assert.equal(await perfPage.evaluate(() => [...document.querySelectorAll('#messages .message-body')].some(body => body.textContent.includes('Perf live arrival'))), false, 'a live arrival was appended to the history window');
    // Unread rows behind the window, then a project refresh: the open room must not be marked read while it is a window.
    seedPerfRows('msg_perf_late_1', 'msg_perf_late_2');
    await perfPage.evaluate(() => { window.__readPosts = []; });
    await first.evaluate(async projectID => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const response = await fetch(`/api/projects/${projectID}/rooms`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ title: 'Perf refresh' }) });
      if (!response.ok) throw new Error(`refresh room creation: ${response.status}`);
    }, actualProjectID);
    await perfPage.waitForFunction(() => [...document.querySelectorAll('.room-name')].some(el => el.textContent.includes('Perf refresh')));
    await new Promise(resolve => setTimeout(resolve, 300));
    assert.deepEqual(await perfPage.evaluate(id => window.__readPosts.filter(url => url.includes(id)), perfRoomID), [], 'the room was marked read while its newest messages were hidden');
    await perfPage.click('.back-to-latest:not(.thread-latest):not(.thread-new)');
    await waitDump(perfPage, 'return to latest', id => window.__readPosts.some(url => url.includes(id)) && document.querySelector('#messages .message:last-child')?.dataset.messageId === 'msg_perf_late_2', () => ({ reads: window.__readPosts, last: document.querySelector('#messages .message:last-child')?.dataset.messageId }), perfRoomID);
    assert.equal(await perfPage.$eval('.back-to-latest:not(.thread-latest):not(.thread-new)', el => el.hidden && el.getAttribute('aria-label') === 'Back to latest'), true, 'Back to latest kept its count after returning');
    // A trimmed window pages forward when its bottom is reached, trimming its oldest rows, and is the latest view again at the end.
    const perfRows = () => perfPage.evaluate(() => { const rows = [...document.querySelectorAll('#messages > article.message')]; return { count: rows.length, first: rows[0]?.dataset.messageId, last: rows.at(-1)?.dataset.messageId, lastSeq: Number(rows.at(-1)?.dataset.seq), latestHidden: document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden, hasOlder: document.querySelector('#messages').dataset.hasOlder }; });
    const scrollBack = async pages => {
      for (let page = 0; page < pages; page++) {
        const before = (await perfRows()).first;
        await perfPage.evaluate(() => { document.querySelector('#messages').scrollTop = 0; });
        await perfPage.waitForFunction(id => document.querySelector('#messages .message')?.dataset.messageId !== id, { timeout: 15000 }, before);
      }
      await perfPage.waitForFunction(() => !document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden);
    };
    await scrollBack(8);
    let forward = await perfRows();
    await perfPage.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = list.scrollHeight; });
    await waitDump(perfPage, 'page forward', last => document.querySelector('#messages > article.message:last-child')?.dataset.messageId !== last, () => ({ rows: document.querySelectorAll('#messages > article.message').length, last: document.querySelector('#messages > article.message:last-child')?.dataset.messageId }), forward.last);
    await new Promise(resolve => setTimeout(resolve, 500));
    let paged = await perfRows();
    assert(paged.lastSeq - forward.lastSeq <= 50 && !paged.latestHidden, `one scroll to the bottom paged ${paged.lastSeq - forward.lastSeq} messages forward: ${JSON.stringify(paged)}`);
    assert(paged.count <= 300 && paged.first !== forward.first && paged.hasOlder === 'true', `paging forward did not trim the oldest rows: ${JSON.stringify({ forward, paged })}`);
    for (let step = 0; step < 20 && !paged.latestHidden; step++) {
      await perfPage.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = list.scrollHeight; });
      await perfPage.waitForFunction(last => document.querySelector('#messages > article.message:last-child')?.dataset.messageId !== last || document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden, { timeout: 15000 }, paged.last);
      paged = await perfRows();
      assert(paged.count <= 300, `paging forward kept ${paged.count} messages in the DOM`);
    }
    assert.deepEqual([paged.latestHidden, paged.last], [true, 'msg_perf_late_2'], 'paging forward did not return to the latest view');
    await postAsOwner(first, perfRoomID, 'Perf after paging');
    await perfPage.waitForFunction(() => document.querySelector('#messages > article.message:last-child')?.textContent.includes('Perf after paging'));
    // Sending from a history window shows the latest view with the sent message at its end, not the message among old rows.
    await scrollBack(8);
    await perfPage.locator('#composer textarea').fill('Perf sent from the window');
    await perfPage.keyboard.press('Enter');
    await waitDump(perfPage, 'send from window', () => document.querySelector('#messages > article.message:last-child')?.textContent.includes('Perf sent from the window') && document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden, () => ({ last: document.querySelector('#messages > article.message:last-child')?.textContent, button: document.querySelector('.back-to-latest').hidden }));
    assert.equal(await perfPage.evaluate(() => document.querySelector('#messages > article.message:last-child').previousElementSibling?.textContent.includes('Perf after paging')), true, 'the sent message does not follow the room\'s previous newest message');
    // Trimming a thread root also forgets its replies: the inspector's previous/next must end at the newest rendered row.
    const threadPerf = spawnSync('python3', ['-c', `
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
with con:
    con.execute("UPDATE message SET thread_root='msg_perf_19900', reply_to='msg_perf_19900' WHERE id BETWEEN 'msg_perf_19990' AND 'msg_perf_19998'")
`, path.join(tmp, 'server', 'aimebu.sqlite')], { encoding: 'utf8' });
    if (threadPerf.status !== 0) throw new Error(`perf thread seed failed: ${threadPerf.stderr}`);
    await perfPage.reload();
    await perfPage.waitForFunction(() => document.querySelector('#messages > article.message:last-child')?.textContent.includes('Perf sent from the window') && document.querySelector('#connection')?.getAttribute('aria-label') === 'Connected');
    await scrollBack(8);
    await perfPage.evaluate(() => document.querySelector('#messages > article.message:last-child .chat-msg-debug-toggle').click());
    await perfPage.waitForFunction(() => document.querySelector('#message-debug-dialog')?.open && document.querySelector('#message-debug-dialog .message-debug-position').textContent);
    const trimmedPosition = await perfPage.$eval('#message-debug-dialog .message-debug-position', el => el.textContent);
    await domClick(perfPage, '#message-debug-close');
    assert.match(trimmedPosition, /^(\d+) \/ \1$/, `replies of trimmed thread roots stayed after the newest rendered row: ${trimmedPosition}`);
    // Paging forward brings the root and its replies back, counted once.
    for (let step = 0; step < 20 && !(await perfRows()).latestHidden; step++) {
      const last = (await perfRows()).last;
      await perfPage.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = list.scrollHeight; });
      await perfPage.waitForFunction(last => document.querySelector('#messages > article.message:last-child')?.dataset.messageId !== last || document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden, { timeout: 15000 }, last);
    }
    assert.equal(await perfPage.$eval('#messages > article[data-message-id="msg_perf_19900"] > .thread-chip', el => el.textContent), '9 replies', 'a trimmed thread root counted its replies twice after paging forward');
    // The open thread's root still follows the raw/rendered switch after its row was trimmed away.
    await perfPage.evaluate(() => document.querySelector('#messages > article[data-message-id="msg_perf_19900"] > .thread-chip').click());
    await perfPage.waitForFunction(() => document.querySelectorAll('#thread-messages .thread-entry').length === 10);
    await scrollBack(8);
    assert.equal(await perfPage.evaluate(() => !!document.querySelector('#messages > article[data-message-id="msg_perf_19900"]')), false, 'the thread root row was not trimmed');
    await perfPage.evaluate(() => document.querySelector('#room-raw').click());
    const rawEntries = await perfPage.$$eval('#thread-messages .thread-entry', entries => entries.map(entry => !!entry.querySelector('.message-body .raw-text')));
    await perfPage.evaluate(() => document.querySelector('#room-raw').click());
    await perfPage.evaluate(() => document.querySelector('#close-thread').click());
    assert.deepEqual(rawEntries, Array(10).fill(true), 'the open thread did not switch to raw Markdown after its root row was trimmed');
    // Closing that thread lets go of the replies kept for it: the inspector again ends at the newest rendered row.
    await perfPage.evaluate(() => document.querySelector('#messages > article.message:last-child .chat-msg-debug-toggle').click());
    await perfPage.waitForFunction(() => document.querySelector('#message-debug-dialog')?.open && document.querySelector('#message-debug-dialog .message-debug-position').textContent);
    const closedThreadPosition = await perfPage.$eval('#message-debug-dialog .message-debug-position', el => el.textContent);
    await domClick(perfPage, '#message-debug-close');
    assert.match(closedThreadPosition, /^(\d+) \/ \1$/, `a closed thread's replies stayed after its root row was trimmed: ${closedThreadPosition}`);
    // A cap smaller than what an older page pushes below the reader must not trim the rows on screen: the trim waits.
    await domClick(perfPage, '.back-to-latest:not(.thread-latest):not(.thread-new)');
    await perfPage.waitForFunction(() => document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden && document.querySelector('#messages > article.message:last-child')?.textContent.includes('Perf sent from the window'));
    // Watched for removal: a row dropped and paged back in later would look kept at the end.
    const visibleBefore = await perfPage.evaluate(() => {
      window.__renderCap = 20; const id = document.querySelector('#messages > article.message').dataset.messageId;
      window.__visibleRemoved = false;
      window.__visibleWatch = new MutationObserver(records => { if (records.some(record => [...record.removedNodes].some(node => node.dataset?.messageId === id))) window.__visibleRemoved = true; });
      window.__visibleWatch.observe(document.querySelector('#messages'), { childList: true });
      return id;
    });
    await perfPage.evaluate(() => { document.querySelector('#messages').scrollTop = 0; });
    await perfPage.waitForFunction(id => document.querySelector('#messages > article.message')?.dataset.messageId !== id, { timeout: 15000 }, visibleBefore);
    await new Promise(resolve => setTimeout(resolve, 300));
    const visibleAfter = await perfPage.evaluate(id => { window.__visibleWatch.disconnect(); const row = document.querySelector(`#messages > article[data-message-id="${id}"]`), list = document.querySelector('#messages').getBoundingClientRect(); const rect = row?.getBoundingClientRect(); return { removed: window.__visibleRemoved, onScreen: !!rect && rect.bottom > list.top && rect.top < list.bottom, rows: document.querySelectorAll('#messages > article.message').length }; }, visibleBefore);
    await perfPage.evaluate(() => { window.__renderCap = 300; });
    assert.deepEqual([visibleAfter.removed, visibleAfter.onScreen], [false, true], `an older page trimmed the rows on screen: ${JSON.stringify(visibleAfter)}`);
    // A forward page's response can be held back (the request goes out, its answer waits for __releaseNewer()), so a live
    // arrival can be made to land while the page is in flight.
    const holdNextNewer = () => perfPage.evaluate(() => {
      window.__releaseNewer = null;
      if (!window.__newerHook) {
        window.__newerHook = true; const previous = window.fetch;
        window.fetch = (input, init) => {
          if (!window.__holdNewer || !/\/messages\?after=\d+&limit=50$/.test(String(input))) return previous(input, init);
          window.__holdNewer = false; const response = previous(input, init);
          return new Promise(resolve => { window.__releaseNewer = () => resolve(response); });
        };
      }
      window.__holdNewer = true;
    });
    const jumpPerf = async no => {
      await answerDialogs(perfPage, () => ({ accept: true, values: { value: String(no) } }));
      await perfPage.evaluate(() => document.querySelector('#room-jump').click());
      await perfPage.waitForFunction(no => document.querySelector(`#messages > article.message[data-no="${no}"]`) && !document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden, {}, no);
    };
    const nearEndSeq = await perfPage.evaluate(() => Number([...document.querySelectorAll('#messages > article.message')].at(-60).dataset.no));
    // The window's last forward page is in flight when a live message arrives: it is counted, and must still be shown when
    // the window reaches the live end, with the room marked read only after that.
    await scrollBack(8);
    await jumpPerf(nearEndSeq);
    await holdNextNewer();
    await perfPage.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = list.scrollHeight; });
    await perfPage.waitForFunction(() => !!window.__releaseNewer);
    await postAsOwner(first, perfRoomID, 'Perf live during the last page');
    await perfPage.waitForFunction(() => /· 1 new$/.test(document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').getAttribute('aria-label')));
    seedPerfRows('msg_perf_late_3');
    await first.evaluate(async projectID => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const response = await fetch(`/api/projects/${projectID}/rooms`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ title: 'Perf refresh two' }) });
      if (!response.ok) throw new Error(`refresh room creation: ${response.status}`);
    }, actualProjectID);
    await perfPage.waitForFunction(() => [...document.querySelectorAll('.room-name')].some(el => el.textContent.includes('Perf refresh two')));
    await perfPage.evaluate(id => {
      window.__readShown = null; const previous = window.fetch;
      window.fetch = (input, init) => { if (init?.method === 'POST' && String(input).endsWith(`/rooms/${id}/read`) && window.__readShown === null) window.__readShown = { live: [...document.querySelectorAll('#messages .message-body')].some(body => body.textContent.includes('Perf live during the last page')), seeded: !!document.querySelector('#messages > article[data-message-id="msg_perf_late_3"]') }; return previous(input, init); };
      window.__releaseNewer();
    }, perfRoomID);
    await waitDump(perfPage, 'arrival during the last page', () => document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden && [...document.querySelectorAll('#messages .message-body')].some(body => body.textContent.includes('Perf live during the last page')) && window.__readShown !== null, () => ({ button: document.querySelector('.back-to-latest').textContent, hidden: document.querySelector('.back-to-latest').hidden, last: [...document.querySelectorAll('#messages > article.message')].slice(-3).map(row => row.dataset.messageId), read: window.__readShown }));
    assert.deepEqual(await perfPage.evaluate(() => window.__readShown), { live: true, seeded: true }, 'the room was marked read before the arrivals were shown');
    // A jump while a forward page is in flight starts a new window, and that window still pages forward.
    await scrollBack(8);
    await holdNextNewer();
    await perfPage.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = list.scrollHeight; });
    await perfPage.waitForFunction(() => !!window.__releaseNewer);
    const jumpWindowSeq = nearEndSeq - 3000;
    await jumpPerf(jumpWindowSeq);
    await perfPage.evaluate(() => window.__releaseNewer());
    const jumpedLast = (await perfRows()).last;
    await perfPage.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = list.scrollHeight; });
    await waitDump(perfPage, 'paging after a jump', last => document.querySelector('#messages > article.message:last-child')?.dataset.messageId !== last, () => ({ last: document.querySelector('#messages > article.message:last-child')?.dataset.messageId }), jumpedLast);
    // Sending from a window with a thread open keeps the thread panel and its unsent reply across the jump to the latest.
    const windowRoot = await perfPage.evaluate(() => { const row = document.querySelector('#messages > article.message:last-child'); row.querySelector('.thread-action').click(); return row.dataset.messageId; });
    await perfPage.waitForFunction(id => !document.querySelector('#thread-panel').hidden && document.querySelector('#thread-messages .thread-entry')?.dataset.messageId === id, {}, windowRoot);
    await perfPage.evaluate(() => { document.querySelector('#thread-composer').elements.body.value = 'Perf unsent thread reply'; });
    await perfPage.locator('#composer textarea').fill('Perf sent with a thread open');
    await perfPage.keyboard.press('Enter');
    await perfPage.waitForFunction(() => document.querySelector('#messages > article.message:last-child')?.textContent.includes('Perf sent with a thread open') && document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden);
    // The thread is reopened once the room has reopened; a lost thread fails the assertion below after this wait.
    await perfPage.waitForFunction(() => !document.querySelector('#thread-panel').hidden, { timeout: 5000 }).catch(() => {});
    const threadAfterSend = await perfPage.evaluate(() => ({ open: !document.querySelector('#thread-panel').hidden, root: document.querySelector('#thread-messages .thread-entry')?.dataset.messageId, draft: document.querySelector('#thread-composer').elements.body.value }));
    await perfPage.evaluate(() => document.querySelector('#close-thread').click());
    assert.deepEqual(threadAfterSend, { open: true, root: windowRoot, draft: 'Perf unsent thread reply' }, 'sending from a window lost the open thread or its unsent reply');
    // If the catch-up refresh at the live end fails, the room is not marked read and the arrival stays counted; Back to
    // latest then completes it, marking read only up to the newest message shown.
    await scrollBack(8);
    await jumpPerf(nearEndSeq);
    await holdNextNewer();
    await perfPage.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = list.scrollHeight; });
    await perfPage.waitForFunction(() => !!window.__releaseNewer);
    await postAsOwner(first, perfRoomID, 'Perf live before a failed catch-up');
    await perfPage.waitForFunction(() => /· 1 new$/.test(document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').getAttribute('aria-label')));
    seedPerfRows('msg_perf_late_4');
    await first.evaluate(async projectID => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const response = await fetch(`/api/projects/${projectID}/rooms`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ title: 'Perf refresh three' }) });
      if (!response.ok) throw new Error(`refresh room creation: ${response.status}`);
    }, actualProjectID);
    await perfPage.waitForFunction(() => [...document.querySelectorAll('.room-name')].some(el => el.textContent.includes('Perf refresh three')));
    await perfPage.evaluate(() => {
      window.__failCatchUp = true; window.__readBodies = []; const previous = window.fetch;
      setTimeout(() => window.__releaseNewer());
      window.fetch = (input, init) => {
        if (init?.method === 'POST' && /\/read$/.test(String(input))) window.__readBodies.push(init.body || '');
        if (window.__failCatchUp && /\/messages\?after=\d+&limit=100$/.test(String(input))) { window.__failCatchUp = false; return Promise.reject(new TypeError('Failed to fetch')); }
        return previous(input, init);
      };
    });
    await perfPage.waitForFunction(() => !window.__failCatchUp);
    await new Promise(resolve => setTimeout(resolve, 500));
    const afterFailure = await perfPage.evaluate(() => ({ button: document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden ? 'hidden' : document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').getAttribute('aria-label'), reads: window.__readBodies.length }));
    assert.deepEqual(afterFailure, { button: 'Back to latest · 1 new', reads: 0 }, 'a failed catch-up marked the room read or lost the arrival count');
    await domClick(perfPage, '.back-to-latest:not(.thread-latest):not(.thread-new)');
    await perfPage.waitForFunction(() => window.__readBodies.length === 1 && document.querySelector('#messages > article.message:last-child')?.dataset.messageId === 'msg_perf_late_4');
    const shownSeq = await perfPage.$eval('#messages > article.message:last-child', el => Number(el.dataset.seq));
    assert.deepEqual(await perfPage.evaluate(() => JSON.parse(window.__readBodies[0])), { seq: shownSeq }, 'Back to latest did not mark read up to the newest message shown');
    // A project refresh while the catch-up is in flight must not mark read past what is shown, and a failed catch-up then
    // leaves the unseen row unread on the server.
    const perfUnread = () => first.evaluate(async (projectID, roomID) => (await (await fetch(`/api/projects/${projectID}/rooms`)).json()).find(room => room.id === roomID).unread_count, actualProjectID, perfRoomID);
    await scrollBack(8);
    await jumpPerf(nearEndSeq);
    await perfPage.evaluate(() => {
      window.__readBodies = []; window.__failHeldCatchUp = null; let hold = true; const previous = window.fetch;
      window.fetch = (input, init) => {
        if (!hold || !/\/messages\?after=\d+&limit=100$/.test(String(input))) return previous(input, init);
        hold = false; return new Promise((resolve, reject) => { window.__failHeldCatchUp = () => reject(new TypeError('Failed to fetch')); });
      };
    });
    await perfPage.evaluate(() => { const list = document.querySelector('#messages'); list.scrollTop = list.scrollHeight; });
    await perfPage.waitForFunction(() => !!window.__failHeldCatchUp);
    seedPerfRows('msg_perf_late_5');
    await first.evaluate(async projectID => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const response = await fetch(`/api/projects/${projectID}/rooms`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ title: 'Perf refresh four' }) });
      if (!response.ok) throw new Error(`refresh room creation: ${response.status}`);
    }, actualProjectID);
    await perfPage.waitForFunction(() => [...document.querySelectorAll('.room-name')].some(el => el.textContent.includes('Perf refresh four')));
    await new Promise(resolve => setTimeout(resolve, 300));
    await perfPage.evaluate(() => window.__failHeldCatchUp());
    await perfPage.waitForFunction(() => !document.querySelector('.back-to-latest:not(.thread-latest):not(.thread-new)').hidden);
    const duringCatchUp = { reads: await perfPage.evaluate(() => window.__readBodies.length), unread: await perfUnread(), shown: await perfPage.evaluate(() => !!document.querySelector('#messages > article[data-message-id="msg_perf_late_5"]')) };
    assert.deepEqual(duringCatchUp, { reads: 0, unread: 1, shown: false }, 'a project refresh during the catch-up marked the room read past what was shown');
    await domClick(perfPage, '.back-to-latest:not(.thread-latest):not(.thread-new)');
    await perfPage.waitForFunction(() => document.querySelector('#messages > article.message:last-child')?.dataset.messageId === 'msg_perf_late_5');
    for (let i = 0; i < 50 && await perfUnread() !== 0; i++) await new Promise(resolve => setTimeout(resolve, 100));
    assert.equal(await perfUnread(), 0, 'Back to latest did not mark the shown row read');
    // A thread open beside a window can hold a reply newer than the top-level rows the window hides. Reselecting the room
    // (its latest page failing to load) must not mark read past the window's newest top-level row.
    await scrollBack(8);
    const windowEnd = await perfPage.evaluate(() => { const row = document.querySelector('#messages > article.message:last-child'); return { id: row.dataset.messageId, seq: Number(row.dataset.seq) }; });
    seedPerfRows('msg_perf_late_6', 'msg_perf_late_7', 'msg_perf_late_8');
    const replySeed = spawnSync('python3', ['-c', `
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
with con:
    con.execute("UPDATE message SET thread_root=?, reply_to=? WHERE id='msg_perf_late_8'", (sys.argv[2], sys.argv[2]))
`, path.join(tmp, 'server', 'aimebu.sqlite'), windowEnd.id], { encoding: 'utf8' });
    if (replySeed.status !== 0) throw new Error(`perf reply seed failed: ${replySeed.stderr}`);
    await perfPage.evaluate(() => document.querySelector('#messages > article.message:last-child .thread-action').click());
    await perfPage.waitForFunction(() => [...document.querySelectorAll('#thread-messages .thread-entry')].some(entry => entry.dataset.messageId === 'msg_perf_late_8'));
    await first.evaluate(async projectID => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const response = await fetch(`/api/projects/${projectID}/rooms`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ title: 'Perf refresh five' }) });
      if (!response.ok) throw new Error(`refresh room creation: ${response.status}`);
    }, actualProjectID);
    await perfPage.waitForFunction(() => [...document.querySelectorAll('.room-name')].some(el => el.textContent.includes('Perf refresh five')));
    assert.equal(await perfUnread(), 3, 'the seeded rows were not unread before reselecting');
    await perfPage.evaluate(() => {
      window.__latestFailed = false; const previous = window.fetch;
      window.fetch = (input, init) => { if (!window.__latestFailed && /\/messages\?limit=100$/.test(String(input))) { window.__latestFailed = true; return Promise.reject(new TypeError('Failed to fetch')); } return previous(input, init); };
      [...document.querySelectorAll('.room-name')].find(el => el.textContent.includes('Perf history')).click();
    });
    await perfPage.waitForFunction(() => window.__latestFailed);
    await new Promise(resolve => setTimeout(resolve, 500));
    assert.equal(await perfUnread(), 3, `reselecting the room marked read past the window's newest top-level row (#${windowEnd.seq})`);
    await perfPage.evaluate(() => [...document.querySelectorAll('.room-name')].find(el => el.textContent.includes('Perf history')).click());
    await perfPage.waitForFunction(() => document.querySelector('#messages > article.message:last-child')?.dataset.messageId === 'msg_perf_late_7');
    for (let i = 0; i < 50 && await perfUnread() !== 0; i++) await new Promise(resolve => setTimeout(resolve, 100));
    assert.equal(await perfUnread(), 0, 'reopening the room did not mark it read');
    await perfContext.close();
    // Live cap: a room left open keeps receiving messages. Past the live cap the oldest rows go (with their records,
    // reactions and answer links), rows carry no listeners of their own, and scrolling up brings the trimmed rows back.
    const liveRoomID = await first.evaluate(async projectID => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const response = await fetch(`/api/projects/${projectID}/rooms`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ title: 'Live cap' }) });
      if (!response.ok) throw new Error(`live cap room creation: ${response.status}`);
      return (await response.json()).id;
    }, actualProjectID);
    const liveContext = await browser.createBrowserContext();
    const livePage = await liveContext.newPage();
    await livePage.evaluateOnNewDocument(() => { window.__liveCap = 40; window.__exposeChat = true; });
    await livePage.goto(`${origin}/#${liveRoomID}`);
    await livePage.locator('#login-form [name=login]').fill('owner');
    await livePage.locator('#login-form [name=password]').fill('test-password-123');
    await livePage.locator('#login-form button').click();
    await livePage.waitForFunction(id => window.__chat?.room?.id === id && document.querySelector('#connection')?.getAttribute('aria-label') === 'Connected', {}, liveRoomID);
    // Each burst posts top-level messages, a person's reply to the previous one, a thread reply and a reaction, so every
    // per-message map has something to keep. (repliedTo is not a cache: it keeps a question answered after its reply is
    // trimmed, one id per reply, and is reset when the room is opened again.)
    let livePosted = 0;
    const liveBurst = count => first.evaluate(async (roomID, from, count) => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const headers = { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token };
      let previous = null;
      for (let i = from; i < from + count; i++) {
        const body = { body: `Live cap ${String(i).padStart(3, '0')}`, ...(previous && i % 5 === 0 ? { reply_to: previous } : {}) };
        const response = await fetch(`/api/rooms/${roomID}/messages`, { method: 'POST', headers, body: JSON.stringify(body) });
        if (!response.ok) throw new Error(`live cap post: ${response.status}`);
        const message = await response.json();
        if (i % 7 === 0) await fetch(`/api/rooms/${roomID}/messages`, { method: 'POST', headers, body: JSON.stringify({ body: `Live cap thread ${i}`, thread_root: message.id, reply_to: message.id }) });
        if (i % 3 === 0) await fetch(`/api/messages/${message.id}/reactions/${encodeURIComponent('👍')}`, { method: 'PUT', headers });
        previous = message.id;
      }
    }, liveRoomID, livePosted, count).then(() => { livePosted += count; });
    const liveCdp = await livePage.createCDPSession();
    const liveStats = async () => {
      await liveCdp.send('HeapProfiler.collectGarbage');
      const { jsEventListeners, nodes } = await liveCdp.send('Memory.getDOMCounters');
      return { jsEventListeners, nodes, ...await livePage.evaluate(() => {
        const chat = window.__chat, rows = [...document.querySelectorAll('#messages > article.message')];
        return { rows: rows.length, first: rows[0]?.querySelector('.message-body')?.textContent, last: rows.at(-1)?.querySelector('.message-body')?.textContent, hasOlder: document.querySelector('#messages').dataset.hasOlder, maps: { nodes: chat.nodes.size, messages: chat.messages.size, reactions: chat.reactions.size, threadCounts: chat.threadCounts.size, serverThreadCounts: chat.serverThreadCounts.size } };
      }) };
    };
    const waitLiveLast = () => waitDump(livePage, 'live cap arrival', last => [...document.querySelectorAll('#messages > article.message .message-body')].at(-1)?.textContent === last && window.__chat.newestSeq === Math.max(...[...window.__chat.messages.values()].map(m => m.seq)), () => ({ last: [...document.querySelectorAll('#messages > article.message .message-body')].at(-1)?.textContent, rows: document.querySelectorAll('#messages > article.message').length }), `Live cap ${String(livePosted - 1).padStart(3, '0')}`);
    await liveBurst(60); await waitLiveLast();
    await new Promise(resolve => setTimeout(resolve, 800)); // read receipts and reactions settle
    const liveBaseline = await liveStats();
    await liveBurst(140); await waitLiveLast();
    await new Promise(resolve => setTimeout(resolve, 800));
    const liveAfter = await liveStats();
    for (const stats of [liveBaseline, liveAfter]) {
      assert(stats.rows <= 40, `the live end kept ${stats.rows} rows past its cap of 40: ${JSON.stringify(stats)}`);
      assert.equal(stats.hasOlder, 'true', `trimmed rows are not offered again by scrolling up: ${JSON.stringify(stats)}`);
      for (const [name, size] of Object.entries(stats.maps)) assert(size <= 60, `the chat's ${name} map kept ${size} entries for ${stats.rows} rows: ${JSON.stringify(stats)}`);
    }
    assert.equal(liveAfter.last, 'Live cap 199', `the newest arrival is not the last row: ${JSON.stringify(liveAfter)}`);
    // 140 more arrivals at about 17 handler closures each would add some 2400 listeners; delegated rows add none.
    assert(liveAfter.jsEventListeners - liveBaseline.jsEventListeners < 100, `listeners grew with arrivals: ${liveBaseline.jsEventListeners} -> ${liveAfter.jsEventListeners}`);
    assert(liveAfter.nodes - liveBaseline.nodes < 2000, `DOM nodes grew with arrivals: ${liveBaseline.nodes} -> ${liveAfter.nodes}`);
    // Replies to a root the cap already trimmed have no row to count them on: they are not kept, and the maps stay bounded.
    const trimmedRoot = await first.evaluate(async roomID => {
      const [root] = await (await fetch(`/api/rooms/${roomID}/messages?after=0&limit=1`)).json();
      const { csrf_token } = await (await fetch('/api/me')).json();
      const headers = { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token };
      for (let i = 0; i < 30; i++) {
        const response = await fetch(`/api/rooms/${roomID}/messages`, { method: 'POST', headers, body: JSON.stringify({ body: `Live cap late reply ${i}`, thread_root: root.id, reply_to: root.id }) });
        if (!response.ok) throw new Error(`live cap late reply: ${response.status}`);
      }
      const response = await fetch(`/api/rooms/${roomID}/messages`, { method: 'POST', headers, body: JSON.stringify({ body: 'Live cap 200' }) });
      if (!response.ok) throw new Error(`live cap sentinel: ${response.status}`);
      return root;
    }, liveRoomID);
    assert.equal(trimmedRoot.body, 'Live cap 000', 'the oldest live-cap message is not the trimmed root');
    livePosted = 201; await waitLiveLast();
    await new Promise(resolve => setTimeout(resolve, 500));
    const liveLateReplies = await liveStats();
    const orphanReplies = await livePage.evaluate(id => ({ rendered: !!document.querySelector(`#messages > article[data-message-id="${id}"]`), kept: [...window.__chat.messages.values()].filter(message => message.thread_root === id).length, counted: window.__chat.threadCounts.has(id) || window.__chat.serverThreadCounts.has(id) }), trimmedRoot.id);
    assert.deepEqual(orphanReplies, { rendered: false, kept: 0, counted: false }, 'replies to a trimmed root were kept with no row owning them');
    assert(liveLateReplies.rows <= 40, `the live end kept ${liveLateReplies.rows} rows past its cap of 40`);
    for (const [name, size] of Object.entries(liveLateReplies.maps)) assert(size <= 60, `the chat's ${name} map kept ${size} entries after replies to a trimmed root: ${JSON.stringify(liveLateReplies)}`);
    // Scrolling up re-fetches what the cap dropped, in order and without a gap.
    await livePage.evaluate(() => { document.querySelector('#messages').scrollTop = 0; });
    await waitDump(livePage, 'live cap older page', first => document.querySelector('#messages > article.message .message-body')?.textContent !== first, () => ({ first: document.querySelector('#messages > article.message .message-body')?.textContent }), liveLateReplies.first);
    const liveOlder = await livePage.evaluate(() => [...document.querySelectorAll('#messages > article.message .message-body')].map(body => Number(body.textContent.replace('Live cap ', ''))));
    assert(liveOlder[0] < Number(liveLateReplies.first.replace('Live cap ', '')) && liveOlder.every((n, i) => i === 0 || n === liveOlder[i - 1] + 1), `scrolling up after a live trim did not restore the dropped rows in order: ${liveOlder.join(',')}`);
    // Delegated row controls still work on a row that arrived live: copying the number and opening the thread from its chip.
    await livePage.evaluate(() => [...document.querySelectorAll('#messages > article.message')].find(row => !row.querySelector('.thread-chip').hidden).querySelector('.thread-chip').click());
    await livePage.waitForFunction(() => !document.querySelector('#thread-panel').hidden && document.querySelectorAll('#thread-messages .thread-entry').length === 2);
    await livePage.evaluate(() => document.querySelector('#close-thread').click());
    await liveContext.close();
    const largeRoomID = await first.evaluate(async projectID => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const response = await fetch(`/api/projects/${projectID}/rooms`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ title: 'Large imported history' }) });
      if (!response.ok) throw new Error(`large room creation: ${response.status}`);
      const room = await response.json();
      return room.id;
    }, actualProjectID);
    const seedLarge = spawnSync('python3', ['-c', `
import sqlite3, sys
db, room = sys.argv[1:]
con = sqlite3.connect(db)
con.execute('BEGIN IMMEDIATE')
owner = con.execute("SELECT id FROM user WHERE login='owner'").fetchone()[0]
seq = con.execute("SELECT value FROM seq_counter WHERE id=1").fetchone()[0]
rows = [('msg_large_%05d' % i, seq+i+1, room, 'user', owner,
         'Imported message %05d' % i, '[]', '2026-01-01T00:00:00.000000000Z') for i in range(5602)]
with con:
    con.executemany('INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES (?,?,?,?,?,?,?,?)', rows)
    con.execute("UPDATE message SET thread_root='msg_large_00000',reply_to='msg_large_00000' WHERE id BETWEEN 'msg_large_01000' AND 'msg_large_01159' OR id IN ('msg_large_03000','msg_large_04000')")
    con.execute('UPDATE seq_counter SET value=? WHERE id=1', (seq+len(rows),))
print(con.execute("SELECT no FROM message WHERE id='msg_large_00000'").fetchone()[0])  # the trigger numbered the rows
`, path.join(tmp, 'server', 'aimebu.sqlite'), largeRoomID], { encoding: 'utf8' });
    if (seedLarge.status !== 0) throw new Error(`large-room seed failed: ${seedLarge.stderr}`);
    const firstLargeSeq = Number(seedLarge.stdout.trim()); // the message number of the first seeded message (consecutive)
    const largeContext = await browser.createBrowserContext();
    const largePage = await largeContext.newPage();
    await largePage.evaluateOnNewDocument(() => {
      window.__messageRequests = [];
      const originalFetch = window.fetch;
      window.fetch = (input, options) => {
        const url = String(input);
        if (url.includes('/messages')) window.__messageRequests.push(url);
        if (window.__holdThreadProbe && url.includes('thread_root=msg_large_00000') && url.includes('&after=') && url.includes('&limit=1')) {
          window.__holdThreadProbe = false;
          return originalFetch(input, options).then(async response => {
            window.__threadProbeResult = (await response.clone().json()).length;
            return new Promise(resolve => { window.__releaseThreadProbe = () => resolve(response); });
          });
        }
        return originalFetch(input, options);
      };
    });
    await largePage.goto(`${origin}/#${largeRoomID}`);
    await largePage.locator('#login-form [name=login]').fill('owner');
    await largePage.locator('#login-form [name=password]').fill('test-password-123');
    const largeStart = Date.now();
    await largePage.locator('#login-form button').click();
    await largePage.waitForFunction(() => document.querySelector('#messages')?.textContent.includes('Imported message 05601'), { timeout: 15000 });
    const largeOpenMs = Date.now() - largeStart;
    assert(largeOpenMs < 10000, `large room took ${largeOpenMs}ms to open`);
    const initialLarge = await largePage.evaluate(() => ({ count: document.querySelectorAll('#messages .message').length, requests: window.__messageRequests }));
    assert.equal(initialLarge.count, 100, `initial history rendered ${initialLarge.count} messages`);
    assert(initialLarge.requests.some(url => url.includes('limit=100')), 'initial history did not request a bounded page');
    assert.equal(initialLarge.requests.filter(url => url.includes('/reactions')).length, 0, 'initial page fetched reactions one message at a time');
    const anchor = await largePage.$eval('#messages', element => {
      element.scrollTop = 0;
      const message = element.querySelector('.message');
      return { id: message.dataset.messageId, top: message.getBoundingClientRect().top };
    });
    await largePage.waitForFunction(() => document.querySelectorAll('#messages .message').length >= 150);
    const anchorTop = await largePage.evaluate(id => document.querySelector(`[data-message-id="${id}"]`)?.getBoundingClientRect().top, anchor.id);
    assert(Math.abs(anchorTop - anchor.top) < 8, `older history moved the visible message by ${anchorTop - anchor.top}px`);
    const olderRequests = await largePage.evaluate(() => window.__messageRequests.filter(url => url.includes('before=')));
    assert(olderRequests.length >= 1, 'scrolling up did not load older history');
    await largePage.waitForFunction(() => document.querySelector('#connection')?.getAttribute('aria-label') === 'Connected');
    await new Promise(resolve => setTimeout(resolve, 500));
    const requestsBeforeLive = await largePage.evaluate(() => window.__messageRequests.length);
    await first.evaluate(async roomID => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const response = await fetch(`/api/rooms/${roomID}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ body: 'Live after large import' }) });
      if (!response.ok) throw new Error(`large room live send: ${response.status}`);
    }, largeRoomID);
    await largePage.waitForFunction(() => document.querySelector('#messages')?.textContent.includes('Live after large import'));
    const liveRequests = await largePage.evaluate(start => window.__messageRequests.slice(start), requestsBeforeLive);
    assert(liveRequests.some(url => url.includes('after=')), `live refresh did not use a cursor: ${liveRequests}`);
    assert(liveRequests.every(url => url.includes('limit=')), `live refresh made an unbounded request: ${liveRequests}`);
    let jumpTarget = firstLargeSeq + 5601;
    await answerDialogs(largePage, () => ({ accept: true, values: { value: String(jumpTarget) } }));
    await largePage.evaluate(roomID => {
      const previousFetch = window.fetch;
      window.fetch = (input, options) => {
        if (!window.__heldLiveOnce && String(input).includes(`/api/rooms/${roomID}/messages?after=`)) {
          window.__heldLiveOnce = true;
          return new Promise(resolve => { window.__releaseHeldLive = () => resolve(previousFetch(input, options)); });
        }
        return previousFetch(input, options);
      };
    }, largeRoomID);
    await first.evaluate(async roomID => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const response = await fetch(`/api/rooms/${roomID}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ body: 'Held live after jump' }) });
      if (!response.ok) throw new Error(`held live send: ${response.status}`);
    }, largeRoomID);
    await largePage.waitForFunction(() => !!window.__releaseHeldLive);
    await domClick(largePage, '#room-jump');
    await largePage.evaluate(() => window.__releaseHeldLive());
    await largePage.waitForFunction(() => document.querySelector('#messages')?.textContent.includes('Held live after jump'));
    await largePage.evaluate(() => {
      URL.createObjectURL = blob => { window.__largeExportBlob = blob; return 'blob:test'; };
      URL.revokeObjectURL = () => {};
      HTMLAnchorElement.prototype.click = () => {};
    });
    await domClick(largePage, '#room-export');
    await domClick(largePage, '#room-export-dialog [data-format="json"]');
    await largePage.waitForFunction(() => !!window.__largeExportBlob, { timeout: 30000 });
    const exported = await largePage.evaluate(async () => JSON.parse(await window.__largeExportBlob.text()).messages.map(item => item.body));
    assert.equal(exported.length, 5604, 'room export omitted unloaded history');
    assert.equal(exported[0], 'Imported message 00000');
    assert.equal(exported.at(-1), 'Held live after jump');
    jumpTarget = firstLargeSeq + 1000;
    await domClick(largePage, '#room-jump');
    await largePage.waitForFunction(() => [...document.querySelectorAll('#thread-messages .thread-entry')].some(item => item.textContent.includes('Imported message 01000')));
    const contextThread = await largePage.$$eval('#thread-messages .thread-entry .message-body', nodes => nodes.map(node => node.textContent));
    assert.equal(contextThread.length, 101, 'old-reply jump did not keep the thread bounded');
    assert(contextThread.includes('Imported message 01000') && !contextThread.includes('Imported message 04000'), 'old-reply jump lost its context');
    assert.equal(await largePage.$eval('.thread-latest', node => node.hidden), false, 'old thread context cannot return to latest');
    await domClick(largePage, '.thread-latest');
    await largePage.waitForFunction(() => document.querySelector('#thread-messages')?.textContent.includes('Imported message 04000'));
    assert.equal(await largePage.$$eval('#thread-messages .thread-entry', nodes => nodes.length), 101, 'latest thread page was unbounded');
    assert.equal(await largePage.$eval('#thread-messages', node => node.dataset.hasOlder), 'true', 'a large thread still has older replies behind the loaded page');
    const threadAnchor = await largePage.$eval('#thread-messages', element => {
      element.scrollTop = 0;
      const reply = element.children[1];
      return { id: reply.dataset.messageId, top: reply.getBoundingClientRect().top };
    });
    await largePage.waitForFunction(() => document.querySelectorAll('#thread-messages .thread-entry').length >= 151);
    const threadAnchorTop = await largePage.evaluate(id => document.querySelector(`#thread-messages [data-message-id="${id}"]`)?.getBoundingClientRect().top, threadAnchor.id);
    assert(Math.abs(threadAnchorTop - threadAnchor.top) < 8, `older replies moved the visible reply by ${threadAnchorTop - threadAnchor.top}px`);
    const replySeqs = await largePage.$$eval('#thread-messages .thread-entry', nodes => nodes.map(node => Number(node.dataset.seq)));
    assert(replySeqs.every((seq, index) => index === 0 || seq > replySeqs[index - 1]), 'older thread page arrived out of sequence');
    jumpTarget = firstLargeSeq;
    await domClick(largePage, '#room-jump');
    await largePage.waitForFunction(() => document.querySelector('#messages')?.textContent.includes('Imported message 00000'));
    assert.equal(await largePage.$eval('[data-message-id="msg_large_00000"] .thread-action', node => node.title), 'Thread · 162', 'root count missed replies outside the context page');
    jumpTarget = firstLargeSeq + 1000;
    await domClick(largePage, '#room-jump');
    await largePage.waitForFunction(() => !document.querySelector('.thread-latest')?.hidden && document.querySelector('#thread-messages')?.textContent.includes('Imported message 01000'));
    await largePage.evaluate(roomID => {
      const previousFetch = window.fetch;
      window.fetch = (input, options) => {
        if (String(input).includes(`/api/rooms/${roomID}/messages`) && options?.method === 'POST' && options.body?.includes('Local old-context reply')) {
          return previousFetch(input, options).then(response => new Promise(resolve => { window.__releaseLocalReply = () => resolve(response); }));
        }
        return previousFetch(input, options);
      };
    }, largeRoomID);
    await largePage.locator('#thread-composer textarea').fill('Local old-context reply');
    await largePage.click('#thread-composer button.btn-primary');
    await largePage.waitForFunction(() => !!window.__releaseLocalReply && document.querySelector('.thread-new')?.getAttribute('aria-label') === '1 new reply');
    assert.equal(await largePage.$eval('#thread-panel h2', node => node.textContent), 'Thread · 163', 'live GET did not update the old-thread count');
    await largePage.evaluate(() => window.__releaseLocalReply());
    await largePage.waitForFunction(() => document.querySelector('#thread-composer textarea')?.value === '');
    assert.equal(await largePage.$eval('#thread-panel h2', node => node.textContent), 'Thread · 163', 'local POST double-counted a reply already seen by live GET');
    for (const body of ['First live reply in old thread', 'Second live reply in old thread']) {
      await first.evaluate(async ({ roomID, body }) => {
        const { csrf_token } = await (await fetch('/api/me')).json();
        const response = await fetch(`/api/rooms/${roomID}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ body, thread_root: 'msg_large_00000', reply_to: 'msg_large_00000' }) });
        if (!response.ok) throw new Error(`context thread send: ${response.status}`);
      }, { roomID: largeRoomID, body });
    }
    await largePage.waitForFunction(() => document.querySelector('.thread-new')?.getAttribute('aria-label') === '3 new replies');
    assert.equal(await largePage.$eval('#thread-panel h2', node => node.textContent), 'Thread · 165', 'remote live replies did not update the old-thread count');
    assert.equal(await largePage.$eval('#thread-messages', node => node.textContent.includes('First live reply in old thread')), false, 'new reply appeared after a gap in old thread context');
    await largePage.click('.thread-new');
    await largePage.waitForFunction(() => document.querySelector('#thread-messages')?.textContent.includes('Second live reply in old thread'));
    assert.equal(await largePage.$eval('.thread-new', node => node.hidden), true, 'new reply marker remained after opening latest replies');
    jumpTarget = firstLargeSeq;
    await largePage.click('#room-jump');
    await largePage.waitForFunction(() => document.querySelector('#messages')?.textContent.includes('Imported message 00000'));
    assert.equal(await largePage.$eval('[data-message-id="msg_large_00000"] .thread-action', node => node.title), 'Thread · 165', 'live replies did not update the full thread count');
    assert.equal(await largePage.$eval('.back-to-latest', element => element.hidden), false, 'old-message jump did not expose latest navigation');
    await domClick(largePage, '.back-to-latest');
    await largePage.waitForFunction(() => document.querySelector('#messages')?.textContent.includes('Held live after jump'));
    const newestThreadReply = await largePage.evaluate(async roomID => {
      const response = await fetch(`/api/rooms/${roomID}/messages?thread_root=msg_large_00000&limit=1`);
      if (!response.ok) throw new Error(`latest thread reply: ${response.status}`);
      return (await response.json())[0];
    }, largeRoomID);
    assert(newestThreadReply?.no && newestThreadReply.body === 'Second live reply in old thread', 'latest thread reply was not selected deterministically');
    jumpTarget = newestThreadReply.no;
    await largePage.click('#room-jump');
    await largePage.waitForFunction(body => document.querySelector('#thread-messages')?.textContent.includes(body) && document.querySelector('.thread-latest')?.hidden, {}, newestThreadReply.body);
    assert.equal(await largePage.$eval('.thread-latest', node => node.hidden), true, 'jump to newest reply showed Latest replies');
    await largePage.evaluate(() => { window.__holdThreadProbe = true; window.__threadProbeResult = null; window.__releaseThreadProbe = null; });
    await largePage.click('#room-jump');
    await largePage.waitForFunction(() => typeof window.__releaseThreadProbe === 'function');
    assert.equal(await largePage.evaluate(() => window.__threadProbeResult), 0, 'held thread probe did not capture the latest window');
    assert.equal(await largePage.$eval('.thread-latest', node => node.hidden), false, 'Latest replies closed before probe completed');
    await first.evaluate(async roomID => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const response = await fetch(`/api/rooms/${roomID}/messages`, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ body: 'Live reply during latest probe', thread_root: 'msg_large_00000', reply_to: 'msg_large_00000' }) });
      if (!response.ok) throw new Error(`latest probe reply: ${response.status}`);
    }, largeRoomID);
    await largePage.waitForFunction(() => document.querySelector('.thread-new')?.getAttribute('aria-label') === '1 new reply' && !document.querySelector('.thread-new')?.hidden);
    await largePage.evaluate(() => window.__releaseThreadProbe());
    await largePage.waitForFunction(() => document.querySelector('#thread-messages')?.textContent.includes('Live reply during latest probe') && document.querySelector('.thread-latest')?.hidden && document.querySelector('.thread-new')?.hidden);
    assert.equal(await largePage.$eval('.thread-new', node => node.hidden), true, 'probe race left a new-reply marker for an inline reply');
    await largeContext.close();
    // Room list. One list across projects: each project's home room is a parent row (no section labels) with its task rooms
    // nested one level, one Direct messages section last. Layouts a and b close a project's tree with an
    // "Archived (N)" row that lists them inline; layout c has no archived rooms in the list and the filter box finds them.
    {
      const second = path.join(tmp, 'second-project-repo'); fs.mkdirSync(second);
      for (const args of [['init', '-q'], ['config', 'user.name', 'UI Test'], ['config', 'user.email', 'ui@example.invalid']]) { const result = spawnSync('git', args, { cwd: second }); if (result.status !== 0) throw new Error(`git ${args[0]} failed`); }
      fs.writeFileSync(path.join(second, 'README.md'), 'second\n');
      for (const args of [['add', 'README.md'], ['commit', '-qm', 'base']]) { const result = spawnSync('git', args, { cwd: second }); if (result.status !== 0) throw new Error(`git ${args[0]} failed`); }
      const listContext = await browser.createBrowserContext(), listPage = await listContext.newPage();
      await listPage.setViewport({ width: 1100, height: 900 });
      await login(listPage);
      const api = (method, url, body) => listPage.evaluate(async (m, u, b) => {
        const { csrf_token } = await (await fetch('/api/me')).json();
        const response = await fetch(u, { method: m, headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: b === undefined ? undefined : JSON.stringify(b) });
        if (!response.ok) throw new Error(`${m} ${u}: ${response.status}`);
        return response.json();
      }, method, url, body);
      const project2 = await api('POST', '/api/projects', { name: 'Second project', folder: second });
      const home2 = (await api('GET', `/api/projects/${project2.id}/rooms`)).find(room => room.kind === 'home');
      const make = title => api('POST', `/api/projects/${project2.id}/rooms`, { title, parent_id: home2.id, worktree: { enabled: false, base_ref: '' } });
      const alpha = await make('Alpha list'), beta = await make('Beta list');
      for (const room of [alpha, beta, alpha]) await api('POST', `/api/rooms/${room.id}/messages`, { body: `Activity in ${room.title}` });
      const delta = await make('Delta old');
      await api('POST', `/api/rooms/${delta.id}/messages`, { body: 'Old news' });
      await api('POST', `/api/rooms/${delta.id}/close`, { lifecycle_rev: delta.lifecycle_rev || 0 });
      await listPage.waitForFunction(async id => { const rooms = await (await fetch(`/api/rooms/${id}`)).json(); return rooms.lifecycle === 'closed'; }, { timeout: 30000 }, delta.id);
      await listPage.reload();
      await appReady(listPage);
      await listPage.waitForFunction(id => !!document.querySelector(`[data-room-id="${id}"] .room-name`), {}, alpha.id);
      const structure = () => listPage.evaluate(([home, alphaID, betaID, deltaID]) => {
        const top = [...document.querySelector('#projects').children];
        const homeGroup = document.querySelector(`[data-room-id="${home}"]`);
        const tail = homeGroup?.querySelector(':scope > .room-children')?.lastElementChild;
        return {
          first: top[0].className, last: top.at(-1).className, labels: document.querySelectorAll('#projects h2, #projects .project').length,
          homeIsTop: homeGroup?.parentElement === document.querySelector('#projects'),
          nested: !!document.querySelector(`[data-room-id="${home}"] > .room-children [data-room-id="${alphaID}"]`) && !!document.querySelector(`[data-room-id="${home}"] > .room-children [data-room-id="${betaID}"]`),
          flatTasks: top.some(node => [alphaID, betaID].includes(node.dataset?.roomId)),
          oneLevel: document.querySelectorAll('#projects .room-children .room-children').length,
          branches: document.querySelectorAll('#projects .room-pin, #projects .room-hide').length,
          homeChip: !!homeGroup?.querySelector(':scope > .room-row .room-project'), taskChip: !!document.querySelector(`[data-room-id="${alphaID}"] .room-project`),
          tail: tail ? { cls: tail.className, summary: tail.querySelector('summary')?.textContent, open: tail.open, has: !!tail.querySelector(`[data-room-id="${deltaID}"]`) } : null,
          archivedFolds: document.querySelectorAll('#projects details.archived-rooms').length, deltaVisible: !!document.querySelector(`[data-room-id="${deltaID}"]`)?.checkVisibility(),
          directSections: document.querySelectorAll('#projects .direct-rooms').length, directHeading: document.querySelector('.direct-rooms h3')?.textContent,
          previews: document.querySelectorAll('.room-preview').length,
        };
      }, [home2.id, alpha.id, beta.id, delta.id]);
      {
        const variant = 'list';
        await listPage.waitForFunction(id => !!document.querySelector(`[data-room-id="${id}"] .room-name`), {}, alpha.id);
        const state = await structure();
        assert.equal(state.first, 'room-filter', `${variant}: the filter box is on top`); assert.equal(state.last, 'direct-rooms', `${variant}: nothing follows the Direct messages section`);
        assert.equal(state.labels, 0, `${variant}: no project section labels`); assert.equal(state.homeIsTop, true, `${variant}: the home room is the parent row`);
        assert.equal(state.nested, true, `${variant}: tasks are nested under their home`); assert.equal(state.flatTasks, false, `${variant}: tasks are not repeated at the top level`);
        assert.equal(state.oneLevel, 0, `${variant}: one level of nesting`); assert.equal(state.branches, 0, `${variant}: no pin or hide controls on rows`);
        assert.equal(state.homeChip, false, `${variant}: a home room named after its project has no chip`); assert.equal(state.taskChip, false, `${variant}: nested tasks carry no chip`);
        assert.deepEqual(state.tail && { cls: state.tail.cls, summary: state.tail.summary, open: state.tail.open, has: state.tail.has }, { cls: 'archived-rooms', summary: 'Archived (1)', open: false, has: true }, `${variant}: Archived (N) is the last row under the home room`);
        assert.equal(state.deltaVisible, false, `${variant}: archived rooms stay out of sight until the row is opened`);
        assert.equal(state.directSections, 1); assert.equal(state.directHeading, 'Direct messages'); assert.ok(state.previews <= 1, 'a preview only on the open room');
        await listPage.click(`[data-room-id="${home2.id}"] > .room-children > .archived-rooms > summary`);
        await listPage.waitForFunction(id => !!document.querySelector(`[data-room-id="${id}"]`)?.checkVisibility(), {}, delta.id);
        assert.equal((await structure()).deltaVisible, true, `${variant}: the Archived row lists its rooms inline`);
        await listPage.click(`[data-room-id="${home2.id}"] > .room-children > .archived-rooms > summary`);
      }
      // A root room's chevron hides or shows its children (and its Archived row); the choice survives a reload.
      const homeToggle = `[data-room-id="${home2.id}"] > .room-row > .room-collapse`;
      const visibleIn = id => listPage.evaluate(room => !!document.querySelector(`[data-room-id="${room}"]`)?.checkVisibility(), id);
      assert.equal(await listPage.$eval(homeToggle, el => el.getAttribute('aria-expanded')), 'true');
      assert.equal(await visibleIn(alpha.id), true);
      await listPage.click(homeToggle);
      await listPage.waitForFunction(selector => document.querySelector(selector)?.getAttribute('aria-expanded') === 'false', {}, homeToggle);
      assert.deepEqual([await visibleIn(alpha.id), await visibleIn(beta.id), await listPage.evaluate(home => !!document.querySelector(`[data-room-id="${home}"] > .room-children > .archived-rooms`)?.checkVisibility(), home2.id)], [false, false, false], 'a collapsed root hides its children and its Archived row');
      assert.equal(await listPage.$eval(homeToggle, el => el.getAttribute('aria-label')), `Expand ${home2.title}`);
      await listPage.reload(); await appReady(listPage);
      await listPage.waitForSelector(homeToggle);
      assert.equal(await listPage.$eval(homeToggle, el => el.getAttribute('aria-expanded')), 'false', 'the collapsed state is remembered');
      assert.equal(await visibleIn(alpha.id), false);
      await listPage.click(homeToggle);
      await listPage.waitForFunction(id => !!document.querySelector(`[data-room-id="${id}"]`)?.checkVisibility(), {}, alpha.id);
      // Nothing is pinned. The sort button orders the projects (and the rooms inside them); in the manual order a project is dragged or
      // moved with its buttons, and the order is a stored personal preference.
      const projectOrder = () => listPage.$$eval('#projects > .room-group', nodes => nodes.map(node => node.querySelector(':scope > .room-row .room-name')?.textContent || '').filter(text => text.startsWith('⌂')).map(text => text.replace(/^⌂\s*/, '')));
      const homeIDs = () => listPage.$$eval('#projects > .room-group', nodes => nodes.filter(node => (node.querySelector(':scope > .room-row .room-name')?.textContent || '').startsWith('⌂')).map(node => node.dataset.roomId));
      const roomOrder = () => listPage.$$eval(`[data-room-id="${home2.id}"] > .room-children > .room-group`, nodes => nodes.map(node => node.querySelector('.room-name').textContent.replace(/^#\s*/, '')).filter(title => title !== 'general')); // general: every project's own task room, not under test here
      const setSort = async key => { await domClick(listPage, '#rooms-sort'); await domClick(listPage, `#rooms-sort-menu .sort-item[data-sort="${key}"]`); await listPage.waitForFunction(() => document.querySelector('#rooms-sort-menu').hidden); };
      assert.deepEqual(await listPage.$$eval('#room-pin, .project-pin, .room-pin, .room-pinned', nodes => nodes.length), 0, 'there is no pin anywhere: not on a room, not on a project');
      assert.equal(await listPage.evaluate(async id => (await fetch(`/api/rooms/${id}/preferences`, { method: 'PATCH', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': (await (await fetch('/api/me')).json()).csrf_token }, body: JSON.stringify({ pinned: true }) })).status, beta.id), 400, 'a room cannot be pinned through the API either');
      assert.equal(await listPage.evaluate(async id => (await fetch(`/api/projects/${id}/preferences`, { method: 'PATCH', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': (await (await fetch('/api/me')).json()).csrf_token }, body: JSON.stringify({ pinned: true }) })).status, project2.id) >= 404, true, 'there is no project pin route');
      assert.equal((await api('GET', '/api/projects')).some(item => 'pinned' in item), false, 'the project list carries no pin');
      assert.deepEqual(await projectOrder(), ['UI smoke', 'Second project'], 'manual with nothing saved is creation order');
      // Every project row carries the chevron or an invisible spacer, so the names of all roots start at the same x.
      const rootX = await listPage.$$eval('#projects > .room-group', nodes => nodes.filter(node => (node.querySelector(':scope > .room-row .room-name')?.textContent || '').startsWith('⌂')).map(node => Math.round(node.querySelector(':scope > .room-row .room-name').getBoundingClientRect().left)));
      assert.equal(new Set(rootX).size, 1, `project names do not start at the same x: ${rootX}`);
      // The sort button: by name, by latest activity, manual. Rooms inside a project follow the sort.
      const sortState = () => listPage.evaluate(() => ({ expanded: document.querySelector('#rooms-sort').getAttribute('aria-expanded'), hidden: document.querySelector('#rooms-sort-menu').hidden, checked: [...document.querySelectorAll('#rooms-sort-menu .sort-item')].filter(item => item.getAttribute('aria-checked') === 'true').map(item => item.dataset.sort), roles: [...document.querySelectorAll('#rooms-sort-menu .sort-item')].map(item => item.getAttribute('role')) }));
      assert.deepEqual(await sortState(), { expanded: 'false', hidden: true, checked: ['manual'], roles: ['menuitemradio', 'menuitemradio', 'menuitemradio'] });
      assert.equal(await listPage.$eval('#rooms-sort', el => !!el.title && !!el.getAttribute('aria-label') && !!el.querySelector('.icon-mask')), true, 'the sort button is an icon with a tooltip and a name');
      await api('POST', `/api/rooms/${beta.id}/messages`, { body: 'Beta is newest of the second project' });
      await new Promise(resolve => setTimeout(resolve, 1100)); // activity times have one-second resolution
      await api('POST', `/api/rooms/${roomID}/messages`, { body: 'Newest activity in the first project' });
      await listPage.reload(); await appReady(listPage); // the list reads each room's newest activity when it loads
      await listPage.waitForSelector('#rooms-sort');
      await setSort('name');
      assert.deepEqual(await projectOrder(), ['Second project', 'UI smoke'], 'by name');
      assert.deepEqual(await roomOrder(), ['Alpha list', 'Beta list'], 'rooms by name');
      assert.equal(await listPage.$$eval('.project-move', nodes => nodes.length), 0, 'the move buttons exist only in the manual order');
      await setSort('activity');
      await listPage.waitForFunction(() => document.querySelector('#projects > .room-group .room-name')?.textContent.includes('UI smoke'));
      assert.deepEqual(await projectOrder(), ['UI smoke', 'Second project'], 'by latest activity the project with the newest message leads');
      assert.deepEqual(await roomOrder(), ['Beta list', 'Alpha list'], 'rooms by latest activity');
      await listPage.waitForFunction(async () => (await (await fetch('/api/me/preferences')).json()).preferences.sidebar_sort === 'activity'); // the save is asynchronous
      await listPage.reload(); await appReady(listPage);
      await listPage.waitForSelector('#rooms-sort');
      assert.deepEqual((await sortState()).checked, ['activity'], 'the sort survives a reload');
      assert.equal((await api('GET', '/api/me/preferences')).preferences.sidebar_sort, 'activity', 'the sort is a stored personal preference');
      assert.deepEqual(await roomOrder(), ['Beta list', 'Alpha list']);
      // Choosing manual starts from the order on screen; the rooms are left as the server lists them.
      await setSort('manual');
      assert.deepEqual(await projectOrder(), ['UI smoke', 'Second project'], 'manual starts from what the list showed');
      const served = (await api('GET', `/api/projects/${project2.id}/rooms`)).filter(room => room.parent_id === home2.id && room.lifecycle !== 'closed' && room.title !== 'general').map(room => room.title);
      assert.deepEqual(await roomOrder(), served, 'manual leaves rooms in the order the server lists them');
      // Manual: the move buttons and the drag handle reorder projects; the end buttons are disabled.
      const moveState = () => listPage.$$eval('#projects > .room-group > .room-row', rows => rows.filter(row => (row.querySelector('.room-name')?.textContent || '').startsWith('⌂')).map(row => `${row.querySelector('.room-name').textContent.replace(/^⌂\s*/, '')}:${row.querySelector('.project-move-up')?.disabled}:${row.querySelector('.project-move-down')?.disabled}:${row.draggable}`));
      const order0 = await projectOrder();
      assert.deepEqual(await moveState(), order0.map((name, index) => `${name}:${index === 0}:${index === order0.length - 1}:true`), 'the first project cannot move up, the last cannot move down, and rows can be dragged');
      const firstHome = (await homeIDs())[0];
      await domClick(listPage, `[data-room-id="${firstHome}"] > .room-row .project-move-down`);
      await listPage.waitForFunction(first => document.querySelector('#projects > .room-group').dataset.roomId !== first, {}, firstHome);
      assert.deepEqual(await projectOrder(), [...order0].reverse(), 'move down swaps the two projects');
      const projectsNow = await api('GET', '/api/projects');
      const shownIDs = (await projectOrder()).map(name => Object.fromEntries(projectsNow.map(item => [item.name, item.id]))[name]);
      await listPage.waitForFunction(async ids => { const stored = (await (await fetch('/api/me/preferences')).json()).preferences.sidebar_project_order; return !!stored && JSON.stringify(JSON.parse(stored).slice(0, ids.length)) === JSON.stringify(ids); }, {}, shownIDs); // the save is asynchronous
      await listPage.reload(); await appReady(listPage);
      await listPage.waitForSelector('#rooms-sort');
      assert.deepEqual(await projectOrder(), [...order0].reverse(), 'the manual order survives a reload');
      const idByName = Object.fromEntries((await api('GET', '/api/projects')).map(item => [item.name, item.id]));
      assert.deepEqual(JSON.parse((await api('GET', '/api/me/preferences')).preferences.sidebar_project_order).slice(0, 2), (await projectOrder()).map(name => idByName[name]), 'the manual order is a stored personal preference');
      const drag = (from, to, edge) => listPage.evaluate(([fromID, toID, atEdge]) => {
        const source = document.querySelector(`[data-room-id="${fromID}"] > .room-row`), target = document.querySelector(`[data-room-id="${toID}"] > .room-row`), box = target.getBoundingClientRect();
        const transfer = new DataTransfer(), fire = (node, type, y) => node.dispatchEvent(new DragEvent(type, { bubbles: true, cancelable: true, dataTransfer: transfer, clientX: box.left + 5, clientY: y }));
        fire(source, 'dragstart', box.top); fire(target, 'dragover', atEdge === 'after' ? box.bottom - 1 : box.top + 1); fire(target, 'drop', atEdge === 'after' ? box.bottom - 1 : box.top + 1); fire(source, 'dragend', box.top);
      }, [from, to, edge]);
      const [top, other] = await homeIDs();
      await drag(other, top, 'before');
      await listPage.waitForFunction(first => document.querySelector('#projects > .room-group').dataset.roomId !== first, {}, top);
      assert.deepEqual(await projectOrder(), order0, 'dragging a project before another moves it');
      await drag(other, other, 'before');
      assert.deepEqual(await projectOrder(), order0, 'dropping a project onto itself leaves the order');
      await drag(other, top, 'after');
      await listPage.waitForFunction(first => document.querySelector('#projects > .room-group').dataset.roomId !== first, {}, other);
      assert.deepEqual(await projectOrder(), [...order0].reverse(), 'dragging a project after another puts it second');
      // The menu is keyboard-operable: Escape closes it and returns focus to the button.
      await listPage.focus('#rooms-sort'); await listPage.keyboard.press('Enter');
      await listPage.waitForFunction(() => !document.querySelector('#rooms-sort-menu').hidden && document.activeElement?.classList.contains('sort-item'));
      await listPage.keyboard.press('ArrowDown');
      assert.equal(await listPage.evaluate(() => document.activeElement.dataset.sort), 'name', 'ArrowDown from the last order (manual, the selected one) wraps to the first');
      await listPage.keyboard.press('Escape');
      await listPage.waitForFunction(() => document.querySelector('#rooms-sort-menu').hidden && document.activeElement?.id === 'rooms-sort');
      await setSort('manual');
      // The box is a plain filter: it narrows the list, and Enter creates or opens nothing (rooms are made from a row's +).
      await listPage.reload(); await appReady(listPage);
      await listPage.waitForFunction(id => !!document.querySelector(`[data-room-id="${id}"] .room-name`), {}, alpha.id);
      assert.equal(await listPage.$$eval('.room-filter select, .room-filter button, .room-filter .error', nodes => nodes.length), 0, 'no project choice and no Create button in the filter box');
      assert.deepEqual(await listPage.$eval('.room-filter-input', el => [el.placeholder, el.getAttribute('aria-label')]), ['Filter rooms…', 'Filter rooms']);
      await listPage.type('.room-filter-input', 'beta');
      assert.deepEqual(await listPage.$$eval('#projects > .room-group .room-name', els => els.map(el => el.textContent.toLocaleLowerCase().includes('beta'))).then(list => [...new Set(list)]), [true], 'the filter keeps only matching rooms');
      assert.equal(await listPage.$$eval('#projects details.archived-rooms', nodes => nodes.length), 0, 'no Archived row while filtering');
      await listPage.$eval('.room-filter-input', el => { el.value = ''; el.dispatchEvent(new Event('input', { bubbles: true })); });
      const countRooms = async () => (await api('GET', `/api/projects/${project2.id}/rooms`)).length;
      const roomsBefore = await countRooms(), hashBefore = await listPage.evaluate(() => location.hash);
      await listPage.type('.room-filter-input', 'Brand new list room'); await listPage.keyboard.press('Enter');
      await new Promise(resolve => setTimeout(resolve, 400));
      assert.equal(await countRooms(), roomsBefore, 'Enter in the filter box creates no room');
      assert.equal(await listPage.evaluate(() => location.hash), hashBefore, 'Enter in the filter box opens nothing');
      await listPage.type('.room-filter-input', ' x'); await listPage.keyboard.press('Enter');
      assert.equal(await listPage.$eval('.rooms-empty', el => el.textContent), 'No rooms match “Brand new list room x”.', 'a filter that matches nothing says so');
      await listPage.$eval('.room-filter-input', el => { el.value = ''; el.dispatchEvent(new Event('input', { bubbles: true })); });
      await listPage.waitForFunction(id => !!document.querySelector(`[data-room-id="${id}"] .room-name`), {}, alpha.id);
      // Collapse all and expand all: one button in the header, an icon with a tooltip, that folds every project and opens them again.
      const collapseState = () => listPage.evaluate(() => ({ label: document.querySelector('#rooms-fold').getAttribute('aria-label'), title: document.querySelector('#rooms-fold').title, disabled: document.querySelector('#rooms-fold').disabled, icon: !!document.querySelector('#rooms-fold .icon-mask'), folded: [...document.querySelectorAll('#projects > .room-group > .room-row > .room-collapse')].map(button => button.getAttribute('aria-expanded')) }));
      let collapse = await collapseState();
      assert.deepEqual([collapse.label, collapse.title, collapse.disabled, collapse.icon], ['Collapse all projects', 'Collapse all projects', false, true]);
      assert.ok(collapse.folded.length >= 1 && collapse.folded.every(value => value === 'true'), `every project starts open: ${collapse.folded}`);
      await domClick(listPage, '#rooms-fold');
      await listPage.waitForFunction(() => document.querySelector('#rooms-fold').getAttribute('aria-label') === 'Expand all projects');
      collapse = await collapseState();
      assert.ok(collapse.folded.every(value => value === 'false'), `collapse all folds every project: ${collapse.folded}`);
      assert.equal(await listPage.$$eval('#projects .room-children', nodes => nodes.filter(node => node.checkVisibility()).length), 0, 'a folded project hides its rooms');
      await listPage.reload(); await appReady(listPage);
      await listPage.waitForSelector('#rooms-fold');
      assert.equal((await collapseState()).label, 'Expand all projects', 'the folded state is remembered');
      await domClick(listPage, '#rooms-fold');
      await listPage.waitForFunction(() => document.querySelector('#rooms-fold').getAttribute('aria-label') === 'Collapse all projects');
      assert.ok((await collapseState()).folded.every(value => value === 'true'), 'expand all opens every project');
      // With one project folded by hand the button still offers to fold the rest.
      await domClick(listPage, '#projects > .room-group > .room-row > .room-collapse');
      assert.equal((await collapseState()).label, 'Collapse all projects', 'with something still open the button folds');
      await domClick(listPage, '#rooms-fold'); await domClick(listPage, '#rooms-fold');
      assert.equal(await listPage.$('.new-direct-chat'), null, 'the Direct messages section has no New private chat button');
      await listContext.close();
    }
    // Message expiry. Messages older than the window stay in the chat (expiry only decides what an AI is given) but are
    // marked: an hourglass on each and one divider before the first live message. The room's first three top-level messages are made
    // ten hours old in the page's own fetch, so the marks are deterministic; the setting row turns expiry off and on.
    {
      const expiryContext = await browser.createBrowserContext(), expiryPage = await expiryContext.newPage();
      await expiryPage.setViewport({ width: 1100, height: 760 });
      await expiryPage.evaluateOnNewDocument(() => {
        const originalFetch = window.fetch;
        window.fetch = async (input, options) => {
          const response = await originalFetch(input, options);
          if ((!options?.method || options.method === 'GET') && /^\/api\/rooms\/[^/]+\/messages\?limit=\d+$/.test(String(input))) {
            const messages = await response.clone().json();
            const old = new Date(Date.now() - 10 * 3600 * 1000).toISOString();
            const roots = messages.filter(message => !message.thread_root); // thread replies are not in the main list
            // Three old AI messages (expired) and one old human message (always delivered, so never marked).
            roots.slice(0, 3).forEach(message => { message.created_at = old; message.author_kind = 'agent'; message.expirable = true; });
            if (roots[3]) { roots[3].created_at = old; roots[3].author_kind = 'user'; roots[3].expirable = false; }
            window.__oldIDs = roots.slice(0, 3).map(message => message.id);
            return new Response(JSON.stringify(messages), { status: response.status, headers: response.headers });
          }
          return response;
        };
      });
      await login(expiryPage);
      const expiryState = () => expiryPage.evaluate(() => {
        const order = [...document.querySelectorAll('#messages > article.message')].map(item => item.dataset.messageId);
        return { old: window.__oldIDs, order, expired: [...document.querySelectorAll('#messages > article.message-expired')].map(item => item.dataset.messageId),
          marks: document.querySelectorAll('#messages .message-expired-mark').length, dividers: document.querySelectorAll('#messages > article.expiry-boundary').length,
          after: document.querySelector('#messages > article.expiry-boundary')?.dataset.messageId || null,
          tops: [...document.querySelectorAll('#messages > article.message')].map(item => item.offsetTop),
          boxes: [...document.querySelectorAll('#messages > article.message')].map(item => [item.getBoundingClientRect().top, item.querySelector('.message-meta, .system-row')?.getBoundingClientRect().height, item.querySelector('.message-bubble')?.getBoundingClientRect().height, getComputedStyle(item).borderTopWidth].join(' ')) };
      });
      await waitDump(expiryPage, 'expired marks', () => window.__oldIDs?.length === 3 && document.querySelectorAll('#messages > article.message-expired').length === 3, () => ({ old: window.__oldIDs, expired: document.querySelectorAll('#messages > article.message-expired').length, messages: document.querySelectorAll('#messages > article').length }));
      let state = await expiryState();
      assert.deepEqual(state.expired, state.old, 'only the three old messages are expired');
      assert.equal(state.marks, 3, 'each expired message carries one hourglass');
      assert.equal(state.dividers, 1, 'exactly one boundary');
      const topsExpired = state.tops, boxesExpired = state.boxes;
      assert.equal(state.after, state.order[4], 'the boundary marks the first message newer than the window (the old human message sits above it, unmarked)');
      assert.equal(await expiryPage.$eval('#messages .message-expired-mark', el => el.getAttribute('aria-label')), 'Expired from AI context');
      await openSettings(expiryPage);
      await domClick(expiryPage, '#settings-general-tab');
      await expiryPage.waitForSelector('#message-expiry-seconds');
      assert.equal(await expiryPage.$eval('#message-expiry-seconds', el => el.value), '21600', 'the default window is six hours');
      assert.equal(await expiryPage.$eval('#message-expiry-seconds', el => el.disabled), false, 'the owner can edit it');
      const setExpiry = async value => { await expiryPage.$eval('#message-expiry-seconds', el => { el.focus(); el.select(); }); await expiryPage.keyboard.type(String(value)); await expiryPage.click('#message-expiry-save'); };
      await setExpiry(5);
      await expiryPage.waitForFunction(() => /60/.test(document.querySelector('#message-expiry-error')?.textContent || ''));
      assert.equal((await expiryState()).expired.length, 3, 'a rejected value changes nothing');
      await setExpiry(0);
      await expiryPage.waitForFunction(() => /no longer expire/.test(document.querySelector('#message-retention [role=status]')?.textContent || ''));
      await expiryPage.waitForFunction(() => document.querySelectorAll('#messages > article.message-expired').length === 0 && document.querySelectorAll('#messages > article.expiry-boundary').length === 0);
      state = await expiryState();
      assert.equal(state.marks, 0, 'with expiry off no message is marked');
      assert.deepEqual(state.tops, topsExpired, `the boundary label is out of flow: removing it moves no message (expired ${JSON.stringify(boxesExpired)}, then ${JSON.stringify(state.boxes)})`);
      await setExpiry(21600);
      await expiryPage.waitForFunction(() => document.querySelectorAll('#messages > article.message-expired').length === 3 && document.querySelectorAll('#messages > article.expiry-boundary').length === 1);
      assert.deepEqual((await expiryState()).tops, topsExpired, 'adding the boundary label moves no message');
      assert.equal(await expiryPage.evaluate(async () => (await (await fetch('/api/retention')).json()).messages_considered_expired_after_seconds), 21600, 'the setting is back at its default');
      await expiryContext.close();
    }
    // Last, because the v1 agent seeded into the home room is one more historical sender there for every check that lists them.
    {
      // In a lead-only home room a v1 agent keeps its role: every role stays listed, the reviewer v1 gave it is preselected, and the dialog
      // says the home room does not take it and keeps Start off, instead of quietly offering the lead role alone. A lead role clears it.
      const homeRoomID = await first.evaluate(async project => (await (await fetch(`/api/projects/${project}/rooms`)).json()).find(room => room.kind === 'home').id, actualProjectID);
      const homeSeed = spawnSync(seedBinary, [path.join(tmp, 'server', 'aimebu.sqlite'), actualProjectID, homeRoomID, id, 'legacy']);
      if (homeSeed.status !== 0) throw new Error(`legacy home seed failed: ${homeSeed.stderr}`);
      await first.goto(`${origin}/#${homeRoomID}`); await first.reload();
      await first.waitForSelector('#app-view:not([hidden])', { timeout: APP_READY_TIMEOUT });
      await first.waitForFunction(room => location.hash.slice(1) === room, {}, homeRoomID);
      await first.waitForSelector('.message-author[data-sender-kind="historical"]');
      // An answer meant for an earlier opening must not fill a later one. The ordinary spawn dialog of this home room is opened with its roles
      // held back (it would offer the lead alone) and closed again; the start dialog for the v1 agent is opened and filled; then the held
      // answer is let through and must change nothing.
      await first.evaluate(() => {
        const inner = window.fetch; window.__holdRoles = true;
        window.fetch = (input, options) => String(input).endsWith('/api/roles') && window.__holdRoles
          ? (window.__holdRoles = false, new Promise(resolve => { window.__releaseStaleRoles = () => { window.fetch = inner; resolve(inner(input, options)); }; }))
          : inner(input, options);
      });
      await first.waitForFunction(() => !document.querySelector('#spawn-open').disabled); // the page was just reloaded: the member list is still loading
      await domClick(first, '#spawn-open');
      await waitDump(first, 'ordinary spawn with the roles held', () => typeof window.__releaseStaleRoles === 'function' && document.querySelector('#spawn-dialog').open, () => ({ held: typeof window.__releaseStaleRoles, open: document.querySelector('#spawn-dialog').open, disabled: document.querySelector('#spawn-open').disabled, model: document.querySelector('#spawn-form [name=model]')?.value, reason: document.querySelector('#spawn-tool-reason').textContent }));
      await domClick(first, '#spawn-cancel');
      await first.evaluate(() => { const author = [...document.querySelectorAll('.message-author[data-sender-kind="historical"]')].find(node => node.textContent.includes('veteran')); author.click(); });
      await first.waitForSelector('#sender-profile-dialog #sender-profile-start');
      await domClick(first, '#sender-profile-start');
      await first.waitForSelector('#spawn-dialog[open] #spawn-resume:not([hidden])');
      await first.waitForFunction(() => !document.querySelector('#spawn-home-hint').hidden);
      await first.evaluate(() => window.__releaseStaleRoles());
      await new Promise(resolve => setTimeout(resolve, 500));
      assert.deepEqual(await first.evaluate(() => ({ roles: document.querySelectorAll('#spawn-form [name=role] option').length > 1, role: document.querySelector('#spawn-form [name=role]').value, resume: !document.querySelector('#spawn-resume').hidden })), { roles: true, role: 'reviewer', resume: true }, 'the roles of an earlier opening arrived late and changed the start dialog');
      const blocked = await first.evaluate(() => ({ roles: [...document.querySelectorAll('#spawn-form [name=role] option')].map(option => option.value), role: document.querySelector('#spawn-form [name=role]').value, hint: document.querySelector('#spawn-home-hint').textContent, submit: document.querySelector('#spawn-form button[type=submit]').disabled }));
      assert.ok(blocked.roles.length > 1 && blocked.roles.includes('worker') && blocked.role === 'reviewer' && /home room is for the lead.*veteran would start as .*Reviewer/i.test(blocked.hint) && blocked.submit === true, `the home room refuses the v1 role and says so: ${JSON.stringify(blocked)}`);
      await first.select('#spawn-form [name=role]', 'lead');
      assert.equal(await first.$eval('#spawn-home-hint', el => el.hidden), true, 'a lead role is taken by the home room, so the explanation goes');
      await first.select('#spawn-form [name=role]', 'worker');
      assert.equal(await first.$eval('#spawn-home-hint', el => el.hidden), false, 'another role the home room does not take brings it back');
      await domClick(first, '#spawn-cancel');

    }
    console.log('UI smoke passed: login, rooms, chat, threads, reactions, stable DOM, Markdown/XSS, inbox recovery, mock controls, scope controls, usage limits, real fake-agent spawn/echo/control');
  } finally {
    browserClosing = true;
    await browser?.close();
    for (const proc of [supervisor, child]) { if (proc && proc.exitCode === null && proc.signalCode === null) { proc.kill('SIGTERM'); await new Promise(resolve => proc.once('exit', resolve)); } }
    if (!preserveFailure) fs.rmSync(tmp, { recursive: true, force: true });
  }
}
main().catch(error => { if (error?.stopEarly) return; console.error(error); process.exitCode = 1; });

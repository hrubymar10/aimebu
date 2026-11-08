// The top bar's overflow menu against a scratch server: on a narrow screen the Events, Health, Memory and Sign out buttons are offered in a
// "more" menu whose items forward to the real buttons. Opening, focus, the arrow keys, Escape, a click outside, forwarding, the health mark,
// hidden and badge mirroring, and signing out through it. `AUTH_BINARY` shares a built binary.
const assert = require('node:assert/strict');
const { scratch, OWNER, MODULES } = require('./scratch-server.cjs');

const APP = '#app-view:not([hidden])';

async function main() {
  const server = await scratch({ env: MODULES });
  try {
    const page = await server.open({ width: 390, height: 844, context: await server.context() });
    await page.goto(server.origin);
    await page.waitForSelector('#login-view:not([hidden])');
    await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
    await page.$eval('#login-form', form => form.requestSubmit());
    await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
    if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
    await page.waitForSelector(APP);
    await page.waitForSelector('#onboarding-view:not([hidden])'); await page.keyboard.press('Escape');
    await page.$eval('#onboarding-view', view => { view.hidden = true; });
    const menu = () => page.evaluate(() => ({ open: !document.querySelector('#more-menu').hidden, expanded: document.querySelector('#more-toggle').getAttribute('aria-expanded'), focus: document.activeElement?.classList.contains('more-item') ? document.activeElement.firstElementChild.textContent : document.activeElement?.id || '', items: [...document.querySelectorAll('#more-menu .more-item')].map(button => [button.firstElementChild.textContent, button.hidden]) }));
    // on a phone the bar tucks controls only when they do not fit, in the order Sign out, System events, Memory, Health
    const bar = () => page.evaluate(() => ({
      tucked: [...document.querySelectorAll('.topbar nav .topbar-tucked')].map(button => button.id),
      more: getComputedStyle(document.querySelector('#more-toggle')).display !== 'none',
      offered: [...document.querySelectorAll('#more-menu .more-item')].filter(button => !button.hidden).map(button => button.firstElementChild.textContent),
      mark: document.querySelector('#more-toggle').classList.contains('has-issues'),
      overflow: document.querySelector('.topbar nav').scrollWidth > document.querySelector('.topbar nav').clientWidth + 1 || document.documentElement.scrollWidth > innerWidth,
    }));
    const settle = () => page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(() => setTimeout(resolve, 50)))));
    await page.setViewport({ width: 600, height: 900 }); await settle();
    assert.deepEqual(await bar(), { tucked: [], more: false, offered: [], mark: false, overflow: false }, 'with room for every control nothing is tucked and the overflow button is gone');
    const aligned = await page.evaluate(() => { const nav = document.querySelector('.topbar nav'), brand = document.querySelector('.topbar-left').getBoundingClientRect(); return [getComputedStyle(nav).justifyContent, [...nav.querySelectorAll('button')].find(button => button.getBoundingClientRect().width).getBoundingClientRect().left > brand.right + 20, innerWidth - document.querySelector('#connection').getBoundingClientRect().right <= 16]; });
    assert.deepEqual(aligned, ['flex-end', true, true], 'the controls sit at the right edge, away from the logo');
    await page.$eval('#health-toggle', button => button.classList.add('has-issues')); await settle();
    assert.equal((await bar()).mark, false, 'a Health warning does not colour the overflow button while Health is in the bar');
    // a wide Sign out does not fit: it alone goes into the menu, and Health, still in the bar, keeps the warning off the button
    await page.$eval('#logout', button => { button.style.cssText = 'width:400px;min-width:400px;flex-basis:400px'; }); await settle();
    assert.deepEqual(await bar(), { tucked: ['logout'], more: true, offered: ['Sign out'], mark: false, overflow: false }, 'only Sign out is tucked, and the menu offers only it');
    // a wide Settings leaves no room for the rest: all four go, Health last, and the warning moves to the overflow button
    await page.$eval('#settings-toggle', button => { button.style.cssText = 'width:600px;min-width:600px;flex-basis:600px'; }); await settle();
    const crowded = await bar();
    assert.ok(['logout', 'knowledge-toggle', 'health-toggle'].every(id => crowded.tucked.includes(id)), `Sign out, Memory and Health are all tucked: ${crowded.tucked}`);
    assert.deepEqual([crowded.more, crowded.mark], [true, true], 'with Health tucked the overflow button carries its warning');
    // an open menu closes when every control fits again, and the focus inside it goes to a control that came back to the bar
    await page.$eval('#more-toggle', button => button.click());
    assert.ok(await page.evaluate(() => document.querySelector('#more-menu').contains(document.activeElement)), 'opening the menu focuses an item ');
    const where = () => page.evaluate(() => ({ open: !document.querySelector('#more-menu').hidden, offered: [...document.querySelectorAll('#more-menu .more-item')].filter(button => !button.hidden).map(button => button.firstElementChild.textContent),
      focus: (element => (element ? `${element.tagName.toLowerCase()}${element.id ? `#${element.id}` : ''}${element.closest?.('#more-menu') ? ' (in the menu)' : ''}` : 'nothing'))(document.activeElement) }));
    const focusedBefore = (await where()).focus;
    // Settings shrinks first, while Sign out is still wide: Health and Memory come back to the bar, so the item that has focus goes, but the menu
    // stays open for Sign out. Focus follows its control into the bar instead of dropping on the body. (This used to depend on timing: the
    // intermediate layout only showed under load.)
    await page.$eval('#settings-toggle', button => { button.style.cssText = ''; }); await settle();
    const partly = await where();
    assert.deepEqual([partly.open, partly.offered], [true, ['Sign out']], `the menu stays open for the control that is still tucked: ${JSON.stringify(partly)}`);
    assert.ok(/^button#(system-events-toggle|health-toggle|knowledge-toggle)$/.test(partly.focus), `the focused item's control came back to the bar and took the focus (it was ${focusedBefore}): focus is on ${partly.focus}`);
    await page.$eval('#logout', button => { button.style.cssText = ''; }); await settle();
    const closed = await page.evaluate(() => ({ open: !document.querySelector('#more-menu').hidden, focus: document.activeElement?.id, shown: !!document.activeElement && getComputedStyle(document.activeElement).display !== 'none',
      which: (element => (element ? `${element.tagName.toLowerCase()}${element.id ? `#${element.id}` : ''}${element.className ? `.${String(element.className).trim().split(/\s+/).join('.')}` : ''}${element.closest?.('#more-menu') ? ' (in the menu)' : ''}` : 'nothing'))(document.activeElement) }));
    assert.deepEqual([closed.open, closed.shown, ['logout', 'health-toggle', 'knowledge-toggle', 'system-events-toggle'].includes(closed.focus)], [false, true, true], `the menu closed itself and its focus moved to a control back in the bar, not to the body or the hidden overflow button: focus is on ${closed.which}`);
    // what fits again comes back: the bar is measured again as the controls change
    await page.$eval('#settings-toggle', button => { button.style.cssText = ''; }); await page.$eval('#logout', button => { button.style.cssText = ''; }); await settle();
    assert.deepEqual(await bar(), { tucked: [], more: false, offered: [], mark: false, overflow: false }, 'when the controls fit again nothing is tucked and the warning stays off');
    await page.$eval('#health-toggle', button => button.classList.remove('has-issues'));
    // from here on every control is crowded out, so the menu has all four
    await page.setViewport({ width: 390, height: 844 });
    await page.$eval('#settings-toggle', button => { button.style.cssText = 'width:600px;min-width:600px;flex-basis:600px'; }); await settle();
    assert.ok((await bar()).more, 'the overflow button is there');
    // closed at first, with its toggle described; the menu has an item for each button
    assert.deepEqual(await page.evaluate(() => [document.querySelector('#more-toggle').getAttribute('aria-haspopup'), getComputedStyle(document.querySelector('#more-toggle')).display !== 'none', document.querySelector('#more-menu').hidden, [...document.querySelectorAll('#more-menu .more-item')].map(button => [button.getAttribute('role'), button.firstElementChild.textContent])]),
      ['menu', true, true, [['menuitem', 'System events'], ['menuitem', 'Health'], ['menuitem', 'Memory'], ['menuitem', 'Sign out']]], 'on a phone the overflow toggle is there and its menu is closed');
    // opening moves focus to the first item that is shown
    await page.$eval('#more-toggle', button => button.click());
    const opened = await menu();
    assert.deepEqual([opened.open, opened.expanded], [true, 'true']);
    const visible = opened.items.filter(item => !item[1]).map(item => item[0]);
    assert.equal(opened.focus, visible[0], 'focus goes to the first item that is shown');
    // arrows wrap, Home and End jump, other keys do nothing
    const key = async name => { await page.keyboard.press(name); return (await menu()).focus; };
    assert.equal(await key('ArrowDown'), visible[1]);
    assert.equal(await key('End'), visible.at(-1));
    assert.equal(await key('ArrowDown'), visible[0], 'ArrowDown wraps from the last');
    assert.equal(await key('ArrowUp'), visible.at(-1), 'ArrowUp wraps from the first');
    assert.equal(await key('Home'), visible[0]);
    // Escape closes it and gives the focus back to the toggle
    await page.keyboard.press('Escape');
    assert.deepEqual([await menu().then(state => [state.open, state.expanded, state.focus])], [[false, 'false', 'more-toggle']]);
    // a click outside closes it; a click on the toggle while it is open does too
    await page.$eval('#more-toggle', button => button.click());
    assert.equal((await menu()).open, true);
    await page.mouse.click(5, 400);
    assert.equal((await menu()).open, false, 'a click outside closes it');
    await page.$eval('#more-toggle', button => button.click()); await page.$eval('#more-toggle', button => button.click());
    assert.equal((await menu()).open, false, 'the toggle closes it again');
    // the health mark of the real button shows on the toggle
    await page.$eval('#health-toggle', button => button.classList.add('has-issues'));
    await page.waitForFunction(() => document.querySelector('#more-toggle').classList.contains('has-issues'));
    await page.$eval('#health-toggle', button => button.classList.remove('has-issues'));
    await page.waitForFunction(() => !document.querySelector('#more-toggle').classList.contains('has-issues'));
    // an item follows its button's hidden state, and the arrows skip it
    await page.$eval('#knowledge-toggle', button => { button.hidden = true; });
    await page.waitForFunction(() => [...document.querySelectorAll('#more-menu .more-item')].find(button => button.firstElementChild.textContent === 'Memory').hidden);
    await page.$eval('#more-toggle', button => button.click());
    const skip = (await menu()).items.filter(item => !item[1]).map(item => item[0]);
    assert.ok(!skip.includes('Memory'), 'a hidden button\'s item is hidden');
    await page.keyboard.press('Escape');
    await page.$eval('#knowledge-toggle', button => { button.hidden = false; });
    // an item forwards its click to the real button and closes the menu
    await page.evaluate(() => { window.__forwarded = []; for (const id of ['system-events-toggle', 'health-toggle', 'knowledge-toggle']) document.querySelector(`#${id}`).addEventListener('click', () => window.__forwarded.push(id)); });
    for (const [label, id] of [['System events', 'system-events-toggle'], ['Health', 'health-toggle'], ['Memory', 'knowledge-toggle']]) {
      await page.$eval('#more-toggle', button => button.click());
      await page.evaluate(label => [...document.querySelectorAll('#more-menu .more-item')].find(button => button.firstElementChild.textContent === label).click(), label);
      assert.equal(await page.evaluate(() => window.__forwarded.at(-1)), id, `${label} clicks its button`);
      assert.equal((await menu()).open, false, 'and closes the menu');
      await page.keyboard.press('Escape');
    }
    // a badge on the real button is mirrored on the item
    await page.evaluate(() => { const badge = document.querySelector('#system-events-toggle .badge'); if (badge) { badge.textContent = '7'; badge.hidden = false; } });
    const badge = await page.evaluate(() => { const item = [...document.querySelectorAll('#more-menu .more-item')][0]; const mark = item.querySelector('.badge'); return mark ? [mark.textContent, mark.hidden] : null; });
    if (badge) assert.deepEqual(badge, ['7', false], 'the badge of the real button shows on its item');
    // teardown: a menu destroyed while open stops answering the page, and stops following the buttons it mirrored
    {
      const torn = await page.evaluate(async () => {
        const { TopbarMenu } = await import('/js/pages/topbar/page.js');
        const host = document.createElement('div'); document.body.append(host);
        const button = (id, label) => { const node = document.createElement('button'); node.id = id; node.textContent = label; host.append(node); return node; };
        const toggle = button('late-toggle', 'more'), menu = document.createElement('div'); menu.id = 'late-menu'; menu.hidden = true; host.append(menu);
        const targets = [button('late-a', 'a'), button('late-health', 'health'), button('late-c', 'c')];
        const badge = document.createElement('span'); badge.className = 'badge'; badge.textContent = '2'; targets[1].append(badge);
        const clicks = []; for (const target of targets) target.addEventListener('click', () => clicks.push(target.id));
        const menuObject = new TopbarMenu({ toggle, menu, health: targets[1], sources: targets.map((target, index) => ({ target, label: ['A', 'Health', 'C'][index] })) });
        const settle = () => new Promise(resolve => setTimeout(resolve, 50));
        await settle();
        const out = { items: [...menu.querySelectorAll('[role=menuitem]')].map(item => item.textContent), expanded: toggle.getAttribute('aria-expanded') };
        toggle.click();
        out.opened = [!menu.hidden, toggle.getAttribute('aria-expanded'), document.activeElement === menu.querySelector('[role=menuitem]')];
        menuObject.destroy();
        out.afterDestroy = { menuKept: !menu.hidden, children: menu.children.length };
        // the page no longer reaches it: Escape, a click outside and the toggle change nothing
        document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
        document.body.click();
        out.escapeAndOutside = [!menu.hidden, menuObject.store.open.value];
        toggle.click();
        out.toggled = [!menu.hidden, menuObject.store.open.value];
        // and the buttons it mirrored no longer reach it
        targets[0].hidden = true; badge.textContent = '9'; targets[1].classList.add('has-issues');
        await settle();
        out.mirrored = { hidden: menuObject.store.items.value[0].hidden, badge: menuObject.store.items.value[1].badge.text, issues: menuObject.store.issues.value, mark: toggle.classList.contains('has-issues') };
        out.clicks = clicks.length;
        host.remove();
        return out;
      });
      // a second menu on the same toggle and menu replaces the first: one click opens it once
      const again = await page.evaluate(async () => {
        const { TopbarMenu } = await import('/js/pages/topbar/page.js');
        const host = document.createElement('div'); document.body.append(host);
        const toggle = document.createElement('button'), menu = document.createElement('div'), target = document.createElement('button');
        menu.hidden = true; host.append(toggle, menu, target);
        const sources = [{ target, label: 'One' }];
        const first = new TopbarMenu({ toggle, menu, sources, health: target }), second = new TopbarMenu({ toggle, menu, sources, health: target });
        toggle.click();
        const out = { open: !menu.hidden, first: first.store.open.value, second: second.store.open.value, items: menu.querySelectorAll('[role=menuitem]').length };
        // destroying the replaced menu again takes nothing from the one that replaced it, nor its claim on the menu
        toggle.click(); first.destroy(); first.destroy();
        out.afterStale = { items: menu.querySelectorAll('[role=menuitem]').length, open: !menu.hidden };
        toggle.click();
        out.afterToggle = !menu.hidden;
        const third = new TopbarMenu({ toggle, menu, sources, health: target });
        toggle.click();
        out.third = { open: !menu.hidden, second: second.store.open.value, third: third.store.open.value };
        second.destroy(); third.destroy(); host.remove();
        return out;
      });
      assert.deepEqual(again, { open: true, first: false, second: true, items: 1, afterStale: { items: 1, open: false }, afterToggle: true, third: { open: true, second: true, third: true } }, 'a second menu on the same elements replaces the first, and destroying the first again takes nothing from the second');
      assert.deepEqual(torn.items, ['A', 'Health2', 'C'], 'the menu draws an item per button, with the badge');
      assert.deepEqual([torn.expanded, torn.opened], ['false', [true, 'true', true]], 'it opens and focuses the first item');
      assert.deepEqual([torn.escapeAndOutside, torn.toggled], [[true, true], [true, true]], 'a destroyed menu is not closed by Escape or a click outside, nor toggled');
      assert.deepEqual(torn.mirrored, { hidden: false, badge: '2', issues: false, mark: false }, 'a destroyed menu no longer mirrors its buttons');
      assert.equal(torn.clicks, 0, 'and nothing was forwarded');
    }

    await page.$eval('#settings-toggle', button => { button.style.cssText = ''; }); await page.keyboard.press('Escape');
    // The bar folds only what does not fit, at every width: a tablet upright (a touch screen, 744px) has room for every control, so there is no ⋯ at all
    // (it used to fold four of them at any touch width up to 900px); a narrow phone folds some; resizing between them re-flows.
    const crowd = () => page.evaluate(() => ({ tucked: document.querySelectorAll('.topbar nav .topbar-tucked').length, more: getComputedStyle(document.querySelector('#more-toggle')).display !== 'none', visible: ['#system-events-toggle', '#health-toggle', '#knowledge-toggle', '#logout', '#inbox-toggle', '#settings-toggle'].filter(id => { const node = document.querySelector(id); return node && !node.hidden && getComputedStyle(node).display !== 'none'; }).length, spills: document.documentElement.scrollWidth > innerWidth }));
    for (const [width, height] of [[744, 1133], [1133, 744], [1024, 768], [1440, 900]]) {
      await page.setViewport({ width, height, isMobile: width < 1100, hasTouch: width < 1100 }); await settle();
      const state = await crowd();
      assert.deepEqual([state.tucked, state.more, state.spills], [0, false, false], `all the controls are in the bar at ${width}px: ${JSON.stringify(state)}`);
      // A desktop bar (over 700px, not a touch screen) is 43px with the usual 30px buttons; a touch screen keeps the 53px bar and its 40px targets.
      const touch = width < 1100, size = await page.evaluate(() => ({ bar: Math.round(document.querySelector('.topbar').getBoundingClientRect().height), button: Math.round(document.querySelector('#settings-toggle').getBoundingClientRect().width) }));
      assert.deepEqual(size, touch ? { bar: 53, button: 40 } : { bar: 43, button: 30 }, `the bar and its buttons at ${width}px${touch ? ' (touch)' : ''}: ${JSON.stringify(size)}`);
    }
    await page.setViewport({ width: 320, height: 568, isMobile: true, hasTouch: true }); await settle();
    const phone = await crowd();
    assert.ok(phone.tucked > 0 && phone.more && !phone.spills, `a narrow phone (320px) folds some controls into ⋯: ${JSON.stringify(phone)}`);
    await page.setViewport({ width: 744, height: 1133, isMobile: true, hasTouch: true }); await settle();
    assert.deepEqual([(await crowd()).tucked, (await crowd()).more], [0, false], 'and widening the window brings them back');
    await page.setViewport({ width: 390, height: 844 });
    await page.$eval('#settings-toggle', button => { button.style.cssText = 'width:600px;min-width:600px;flex-basis:600px'; }); await settle();

    // Sign out through the menu signs out
    await page.$eval('#more-toggle', button => button.click());
    await page.evaluate(() => [...document.querySelectorAll('#more-menu .more-item')].find(button => button.firstElementChild.textContent === 'Sign out').click());
    await page.waitForSelector('#login-view:not([hidden])');
    assert.equal((await page.evaluate(async () => (await fetch('/api/session')).json())).signed_in, false, 'Sign out in the menu signs out');
    await page.close();
    assert.deepEqual(server.errors, [], `the page reported errors: ${server.errors.join(' | ')}`);
    console.log('top bar page: ok');
  } finally {
    await server.stop();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

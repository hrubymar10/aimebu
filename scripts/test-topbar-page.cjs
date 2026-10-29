// The top bar's overflow menu against a scratch server: on a narrow screen the Events, Health, Memory and Sign out buttons are offered in a
// "more" menu whose items forward to the real buttons. Opening, focus, the arrow keys, Escape, a click outside, forwarding, the health mark,
// hidden and badge mirroring, and signing out through it. `AUTH_BINARY` shares a built binary.
const assert = require('node:assert/strict');
const { scratch, OWNER } = require('./scratch-server.cjs');

const APP = '#app-view:not([hidden])';

async function main() {
  const server = await scratch();
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

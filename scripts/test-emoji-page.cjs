// The emoji picker in the real app, on a scratch server: it is one shared, virtual grid. Opening it draws a screenful of cells (not the 1,900 of the
// database), every emoji is still reachable by scrolling, the tabs and the search, the keyboard walks across rows that are not drawn yet, it opens
// beside its button inside the viewport, and opening and closing it again and again leaves no cells behind. Part of `make test-ui`.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { scratch, OWNER } = require('./scratch-server.cjs');

async function main() {
  const server = await scratch();
  try {
    const page = await server.open({ width: 1280, height: 900, context: await server.context() });
    await page.goto(server.origin);
    await page.waitForSelector('#login-view:not([hidden])');
    await page.type('#login-form [name=login]', OWNER.login); await page.type('#login-form [name=password]', OWNER.password);
    await page.$eval('#login-form', form => form.requestSubmit());
    await page.waitForFunction(() => document.querySelector('#welcome-continue') || !document.querySelector('#app-view').hidden);
    if (await page.$('#welcome-continue')) { await page.waitForSelector('#welcome-continue:not([inert])'); await page.click('#welcome-continue'); }
    await page.waitForSelector('#app-view:not([hidden])');
    const folder = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'aimebu-emoji-'));
    execFileSync('git', ['init', '-q'], { cwd: folder });
    const call = (url, method, payload) => page.evaluate(async (url, method, payload) => {
      const csrf = (await (await fetch('/api/me')).json()).csrf_token;
      const response = await fetch(url, { method, headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: payload ? JSON.stringify(payload) : undefined });
      const text = await response.text(); return { status: response.status, body: text ? JSON.parse(text) : null };
    }, url, method, payload);
    const project = (await call('/api/projects', 'POST', { name: 'Emoji', folder })).body;
    const home = (await call(`/api/projects/${project.id}/rooms`, 'GET')).body.find(room => room.kind === 'home');
    assert.equal((await call(`/api/rooms/${home.id}/messages`, 'POST', { body: 'a message to react to' })).status, 201);
    // A server with no harness offers the first-run harness card as a modal over the app: that is skipped, as the person's own stored choice.
    assert.equal((await call('/api/me/preferences', 'PUT', { preferences: { harness_setup_skipped: '1' } })).status, 200);
    await page.reload(); await page.waitForSelector('#app-view:not([hidden])');
    await page.waitForSelector(`[data-room-id="${home.id}"] .room-item-name`);
    await page.evaluate(id => document.querySelector(`[data-room-id="${id}"] .room-item-name`).click(), home.id);
    await page.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'Emoji' && document.querySelector('#emoji-button'));

    const cells = () => page.$$eval('.emoji-picker .emoji-cell', nodes => nodes.length);
    const buttons = () => page.$$eval('button', nodes => nodes.length);
    const nodes = () => page.evaluate(() => document.getElementsByTagName('*').length);
    const total = await page.evaluate(async () => (await (await fetch('/vendor/gemoji/emoji.json')).json()).length);
    const openPicker = async () => { await page.click('#emoji-button'); await page.waitForSelector('.emoji-picker .emoji-cell'); };
    const closePicker = async () => { await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('.emoji-picker')); };

    // Opening draws a screenful, not the database, and the picker sits beside its button, inside the viewport.
    const base = { buttons: await buttons(), nodes: await nodes() };
    await openPicker();
    const opened = await cells();
    assert.ok(opened > 20 && opened < 250, `opening the picker drew ${opened} cells of ${total}`);
    assert.ok(await nodes() - base.nodes < 500, `the open picker adds ${await nodes() - base.nodes} nodes`);
    const place = await page.evaluate(() => { const picker = document.querySelector('.emoji-picker').getBoundingClientRect(), anchor = document.querySelector('#emoji-button').getBoundingClientRect(); return { pickerLeft: Math.round(picker.left), anchorLeft: Math.round(anchor.left), inside: picker.left >= 0 && picker.top >= 0 && picker.right <= innerWidth && picker.bottom <= innerHeight, above: picker.bottom <= anchor.top + 1 }; });
    assert.deepEqual([place.inside, place.pickerLeft >= place.anchorLeft - 1, place.above], [true, true, true], `the picker opens to the right of its button, above it, inside the window: ${JSON.stringify(place)}`);
    assert.equal(await page.evaluate(() => document.activeElement?.classList.contains('emoji-search')), true, 'the search box has the focus');
    // One border on the focused search field (its own, no outline ring beside it), and the idle "Pick an emoji" is a hint, not a bold name.
    assert.deepEqual(await page.evaluate(() => { const search = getComputedStyle(document.querySelector('.emoji-search')), name = document.querySelector('.emoji-preview-name'); return { outline: search.outlineStyle, border: search.borderTopWidth, ring: search.boxShadow.includes('0px 0px 0px 1px'), hint: [name.textContent, getComputedStyle(name).fontWeight] }; }),
      { outline: 'none', border: '1px', ring: true, hint: ['Pick an emoji', '400'] }, 'the search field has one border and the idle name is not bold');

    // Every emoji can be reached: scrolling the grid through draws them all, never many at once.
    const seen = new Set(); let most = 0;
    for (let at = 0; ; at += 200) {
      const state = await page.evaluate(top => { const grid = document.querySelector('.emoji-grid'); grid.scrollTop = top; return { done: top > grid.scrollHeight, top: grid.scrollTop }; }, at);
      await page.waitForFunction(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve(true)))));
      const here = await page.$$eval('.emoji-picker .emoji-cell', list => list.map(node => Number(node.dataset.i)));
      for (const index of here) seen.add(index); most = Math.max(most, here.length);
      if (state.done) break;
    }
    assert.equal(seen.size, total, `scrolling reached ${seen.size} of ${total} emoji`);
    assert.ok(most < 250, `at most ${most} cells at a time`);
    // Tabs, search, tone.
    await page.click('.emoji-tab[aria-label=Flags]');
    assert.equal(await page.$eval('.emoji-tab[aria-label=Flags]', node => node.getAttribute('aria-current')), 'true', 'a tab scrolls its category to the top');
    assert.ok((await page.$$eval('.emoji-picker .emoji-heading', list => list.map(node => node.textContent))).includes('Flags'), 'and its heading is drawn');
    await page.focus('.emoji-search'); await page.keyboard.type('rocket');
    await page.waitForFunction(() => document.querySelector('.emoji-picker .emoji-cell')?.getAttribute('aria-label') === 'rocket');
    assert.ok(await cells() < 30, 'a search draws its few results only');
    await page.hover('.emoji-picker .emoji-cell');
    assert.equal(await page.$eval('.emoji-preview-code', node => node.textContent), ':rocket:', 'the footer previews the hovered emoji (a cell made after the picker opened)');
    await page.$eval('.emoji-search', node => { node.value = ''; node.dispatchEvent(new Event('input', { bubbles: true })); });
    // The keyboard walks over rows that are not in the page yet.
    await page.keyboard.press('ArrowDown');
    assert.equal(await page.evaluate(() => document.activeElement?.classList.contains('emoji-cell')), true, 'ArrowDown from the search box enters the grid');
    const first = await page.evaluate(() => document.activeElement.dataset.i);
    for (let step = 0; step < 40; step++) await page.keyboard.press('ArrowDown');
    const moved = await page.evaluate(() => ({ index: Number(document.activeElement.dataset.i), inside: (() => { const cell = document.activeElement.getBoundingClientRect(), grid = document.querySelector('.emoji-grid').getBoundingClientRect(); return cell.top >= grid.top - 1 && cell.bottom <= grid.bottom + 1; })() }));
    assert.ok(moved.index > Number(first) + 100 && moved.inside, `forty ArrowDowns moved the focus to a cell that is in view: ${JSON.stringify(moved)}`);
    await page.keyboard.press('End');
    assert.equal(await page.evaluate(() => Number(document.activeElement.dataset.i)), total - 1, 'End goes to the last emoji, drawn on the way');
    await page.keyboard.press('Home');
    assert.equal(await page.evaluate(() => Number(document.activeElement.dataset.i)), 0, 'Home goes to the first');
    // A wheel or a scrollbar can carry the grid far from the focused cell: its row stays drawn, so the focus (and Escape, Tab, the arrows) stays in the picker.
    await page.evaluate(() => { document.querySelector('.emoji-grid').scrollTop = 20000; });
    await page.waitForFunction(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve(true)))));
    assert.equal(await page.evaluate(() => document.activeElement?.classList.contains('emoji-cell')), true, 'scrolling far away from the focused cell keeps the focus on it');
    await page.keyboard.press('ArrowRight');
    assert.equal(await page.evaluate(() => Number(document.activeElement.dataset.i)), 1, 'and the arrows still work from there');
    // The last row is short, but its cells are as wide as the others and in the same columns.
    await page.keyboard.press('End');
    const widths = await page.evaluate(() => { const cells = [...document.querySelectorAll('.emoji-picker .emoji-cell')]; const last = cells.at(-1).getBoundingClientRect(), first = cells[0].getBoundingClientRect(); return { first: Math.round(first.width), last: Math.round(last.width) }; });
    assert.equal(widths.last, widths.first, `a cell of a short last row is as wide as the others: ${JSON.stringify(widths)}`);
    // A tone change with nothing found keeps the message instead of wiping it.
    await page.focus('.emoji-search'); await page.keyboard.type('zzzzqqqq');
    await page.waitForSelector('.emoji-picker .emoji-empty');
    await page.click('.emoji-tone:nth-child(2)');
    assert.ok(await page.$('.emoji-picker .emoji-empty'), 'choosing a skin tone with no results keeps the message');
    await page.$eval('.emoji-search', node => { node.value = ''; node.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.waitForSelector('.emoji-picker .emoji-cell');
    // Across the phone breakpoint the cells get bigger: the rows are laid out again, so none overlaps the next.
    await page.setViewport({ width: 600, height: 900 });
    await page.waitForFunction(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve(true)))));
    const overlap = await page.evaluate(() => { const rows = [...document.querySelectorAll('.emoji-picker .emoji-row')].map(node => node.getBoundingClientRect()).sort((a, b) => a.top - b.top); let worst = 0; for (let at = 1; at < rows.length; at++) worst = Math.max(worst, rows[at - 1].bottom - rows[at].top); return worst; });
    assert.ok(overlap <= 1, `rows do not overlap after the picker is resized across the phone breakpoint (${overlap}px)`);
    await page.setViewport({ width: 1280, height: 900 });
    await closePicker();
    assert.equal(await page.evaluate(() => document.activeElement?.id), 'emoji-button', 'Escape closes it and the focus goes back to the button');
    // The composer moves the attach and emoji buttons between its toolbar and its bottom row when the window crosses the compact breakpoint; moving a
    // node drops its focus, which made the line above fail now and then (a layout change landing just after the picker closed). Focus must survive it.
    const frames = () => page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(() => setTimeout(resolve, 50)))));
    for (const [width, height] of [[600, 900], [1280, 900], [600, 900], [1280, 900]]) {
      await page.focus('#emoji-button');
      await page.setViewport({ width, height });
      await frames();
      assert.equal(await page.evaluate(() => document.activeElement?.id), 'emoji-button', `the emoji button keeps the focus when the window becomes ${width}px wide`);
    }
    await page.focus('.attachment-picker-btn');
    await page.setViewport({ width: 600, height: 900 }); await frames();
    assert.equal(await page.evaluate(() => document.activeElement?.classList.contains('attachment-picker-btn')), true, 'and so does the attach button');
    await page.setViewport({ width: 1280, height: 900 }); await frames();

    // Opening and closing it again and again leaves nothing behind (the picker itself is the same few dozen nodes, kept for the next open).
    await openPicker(); await closePicker();
    const settled = { buttons: await buttons(), nodes: await nodes() };
    for (let round = 0; round < 30; round++) { await openPicker(); await closePicker(); }
    assert.deepEqual({ buttons: await buttons(), nodes: await nodes() }, settled, 'thirty more opens and closes leave the same buttons and nodes');
    assert.ok(settled.buttons - base.buttons <= 20 && settled.nodes - base.nodes <= 80, `a closed picker is out of the page (${settled.buttons - base.buttons} buttons and ${settled.nodes - base.nodes} nodes more than before the first open)`);
    // And a pick still lands.
    await openPicker(); await page.keyboard.type('rocket'); await page.waitForFunction(() => document.querySelector('.emoji-picker .emoji-cell')?.getAttribute('aria-label') === 'rocket');
    await page.keyboard.press('Enter');
    await page.waitForFunction(() => !document.querySelector('.emoji-picker'));
    assert.ok((await page.$eval('#composer textarea', node => node.value)).includes('🚀'), 'a pick lands in the draft');
    assert.deepEqual(server.errors, [], `the page reported errors: ${server.errors.join(' | ')}`);
    console.log('emoji page: ok');
  } finally {
    await server.stop();
  }
}

main().catch(error => { console.error(error); process.exit(1); });

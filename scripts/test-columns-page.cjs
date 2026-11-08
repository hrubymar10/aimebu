// The drag handles of the two side columns (web/js/column-resize.js) in Chromium against the real markup and styles of web/ (no app.js, no Go server):
// dragging, the clamps (each column's range and the room the chat keeps), the keyboard, persistence across a reload, a double click that resets, the
// handles' absence on a phone and beside a collapsed column, and a window that shrinks under a wide column. Part of `make test-ui`.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const root = path.join(__dirname, '..', 'web');
const fixture = fs.readFileSync(path.join(root, 'index.html'), 'utf8').replace(/<script type="module" src="\/js\/app\.js[^"]*"><\/script>/, '');
const types = { '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/fixture.html') { res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(fixture); return; }
  const file = path.join(root, url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});

async function main() {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  const mount = async ({ width = 1280, height = 800, fresh = true, hold } = {}) => {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => { if (message.type() === 'error' && !/Failed to load resource/.test(message.text())) errors.push(message.text()); });
    await page.setViewport({ width, height });
    if (fresh) await page.evaluateOnNewDocument(() => { if (!sessionStorage.getItem('kept')) { try { localStorage.clear(); } catch {} sessionStorage.setItem('kept', '1'); } });
    await page.goto(`${origin}/fixture.html`);
    await page.evaluate(async hold => {
      document.querySelector('#app-view').hidden = false;
      for (const view of ['#boot-view', '#login-view', '#onboarding-view', '#welcome-view']) { const node = document.querySelector(view); if (node) node.hidden = true; }
      const { ColumnResize } = await import('/js/column-resize.js');
      window.__columns = new ColumnResize(hold ? { gripHold: hold } : {});
    }, hold);
    return page;
  };
  const widths = page => page.evaluate(() => ({ rooms: Math.round(document.querySelector('.sidebar').getBoundingClientRect().width), agents: Math.round(document.querySelector('#agents-panel').getBoundingClientRect().width), chat: Math.round(document.querySelector('#messages').getBoundingClientRect().width) }));
  const handle = (page, name) => page.$(`.col-resizer-${name}`);
  const centre = async node => { const box = await node.boundingBox(); return { x: box.x + box.width / 2, y: box.y + Math.min(box.height / 2, 300) }; };
  const drag = async (page, name, dx) => {
    const node = await handle(page, name), from = await centre(node);
    const under = await page.evaluate((x, y) => { const el = document.elementFromPoint(x, y); return el ? `${el.tagName}.${el.className}` : null; }, from.x, from.y);
    const rects = await page.evaluate(() => Object.fromEntries(['#app-view', '.sidebar', '.col-resizer-rooms', '.chat', '#agents-panel'].map(selector => { const r = document.querySelector(selector)?.getBoundingClientRect(); return [selector, r && [Math.round(r.x), Math.round(r.y), Math.round(r.width), Math.round(r.height)]]; })));
    assert.ok(under?.includes('col-resizer'), `the ${name} handle is what is under the pointer at ${JSON.stringify(from)}, not ${under}; boxes ${JSON.stringify(rects)}`);
    await page.mouse.move(from.x, from.y); await page.mouse.down(); await page.mouse.move(from.x + dx / 2, from.y, { steps: 3 }); await page.mouse.move(from.x + dx, from.y, { steps: 3 }); await page.mouse.up();
  };
  const doubleClick = async (page, name) => { const from = await centre(await handle(page, name)); await page.mouse.click(from.x, from.y, { count: 2 }); await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 50))); };
  const hiddenHandles = page => page.evaluate(() => ({ rooms: document.querySelector('.col-resizer-rooms').hidden, agents: document.querySelector('.col-resizer-agents').hidden }));
  const aria = (page, name) => page.$eval(`.col-resizer-${name}`, node => ({ role: node.getAttribute('role'), orientation: node.getAttribute('aria-orientation'), min: Number(node.getAttribute('aria-valuemin')), max: Number(node.getAttribute('aria-valuemax')), now: Number(node.getAttribute('aria-valuenow')), focusable: node.tabIndex === 0, label: node.getAttribute('aria-label') }));
  const stored = page => page.evaluate(() => localStorage.getItem('aimebu2.ui.column-widths'));

  // Defaults, and the handles as separators.
  {
    const page = await mount();
    assert.deepEqual(await widths(page), { rooms: 240, agents: 260, chat: 756 }, 'the columns start at their defaults');
    assert.deepEqual(await aria(page, 'rooms'), { role: 'separator', orientation: 'vertical', min: 180, max: 480, now: 240, focusable: true, label: 'Resize the rooms column' });
    assert.deepEqual(await aria(page, 'agents'), { role: 'separator', orientation: 'vertical', min: 200, max: 520, now: 260, focusable: true, label: 'Resize the members column' });
    // Dragging grows and shrinks each column from its inner edge; nothing is written until the pointer is released.
    await drag(page, 'rooms', 60);
    assert.deepEqual([(await widths(page)).rooms, (await aria(page, 'rooms')).now], [300, 300], 'the rooms column follows the pointer to the right');
    await drag(page, 'agents', -60);
    assert.deepEqual([(await widths(page)).agents, (await aria(page, 'agents')).now], [320, 320], 'the members column grows to the left');
    assert.equal(await stored(page), JSON.stringify({ rooms: 300, agents: 320 }), 'the widths are kept in the browser');
    // The clamps: each column's own range ...
    await drag(page, 'rooms', -600);
    assert.equal((await widths(page)).rooms, 180, 'the rooms column stops at its minimum');
    await drag(page, 'rooms', 900);
    assert.equal((await widths(page)).rooms, 480, 'and at its maximum');
    // ... and the room the chat keeps: with the rooms column at 480, the members column may have 1280 - 480 - 360 - 24 (the gaps) = 416 at most, not its own 520.
    await drag(page, 'agents', -900);
    assert.deepEqual([(await widths(page)).agents, (await widths(page)).chat], [416, 360], 'the chat keeps its minimum width, and the members column stops short of its maximum');
    await drag(page, 'agents', 900);
    assert.equal((await widths(page)).agents, 200, 'the members column stops at its minimum');
    await drag(page, 'rooms', -900);
    await drag(page, 'agents', -900);
    assert.deepEqual([(await widths(page)).rooms, (await widths(page)).agents], [180, 520], 'with room to spare the members column reaches its own maximum');
    await page.close();
  }

  // On a narrow desktop window the chat's minimum limits the other column, and a window that shrinks pulls a wide column in (without forgetting it).
  {
    const page = await mount({ width: 900 });
    await drag(page, 'rooms', 900);
    assert.equal((await widths(page)).rooms, 256, '900 - 260 - 360 - 24 is what the rooms column may have');
    assert.equal((await aria(page, 'rooms')).max, 256, 'and the separator says so');
    await page.close();
    const wide = await mount({ width: 1280 });
    await drag(wide, 'rooms', 400); // 480
    await wide.setViewport({ width: 1000, height: 800 });
    await wide.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
    assert.deepEqual(await widths(wide), { rooms: 377, agents: 239, chat: 360 }, 'a shrinking window pulls both columns in, the wide one more, so the chat keeps its width');
    await wide.setViewport({ width: 1280, height: 800 });
    await wide.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
    assert.deepEqual(await widths(wide), { rooms: 480, agents: 260, chat: 516 }, 'and the widths the person chose come back when there is room again');
    await wide.close();
  }

  // The keyboard: arrows move 16px (the right column grows to the left), Home and End go to the ends, and every change is kept.
  {
    const page = await mount();
    await page.focus('.col-resizer-rooms');
    await page.keyboard.press('ArrowRight'); assert.equal((await widths(page)).rooms, 256);
    await page.keyboard.press('ArrowLeft'); await page.keyboard.press('ArrowLeft'); assert.equal((await widths(page)).rooms, 224);
    await page.keyboard.press('Home'); assert.equal((await widths(page)).rooms, 180);
    await page.keyboard.press('End'); assert.equal((await widths(page)).rooms, 480);
    await page.focus('.col-resizer-agents');
    await page.keyboard.press('ArrowLeft'); assert.equal((await widths(page)).agents, 276, 'left grows the members column');
    await page.keyboard.press('ArrowRight'); await page.keyboard.press('ArrowRight'); assert.equal((await widths(page)).agents, 244);
    await page.keyboard.press('Home'); assert.equal((await widths(page)).agents, 200);
    await page.keyboard.press('End'); assert.equal((await aria(page, 'agents')).now, (await aria(page, 'agents')).max);
    assert.equal(JSON.parse(await stored(page)).rooms, 480, 'a key press is kept at once');
    await page.close();
  }

  // Persistence and the reset.
  {
    const page = await mount();
    await drag(page, 'rooms', 40); await drag(page, 'agents', -30);
    assert.deepEqual(await widths(page), { rooms: 280, agents: 290, chat: 686 });
    await page.close();
    const again = await mount({ fresh: false });
    assert.deepEqual([(await widths(again)).rooms, (await widths(again)).agents], [280, 290], 'the widths survive a reload');
    await doubleClick(again, 'rooms');
    assert.deepEqual([(await widths(again)).rooms, (await widths(again)).agents], [240, 290], 'a double click puts one column back to its default');
    assert.equal(await stored(again), JSON.stringify({ agents: 290 }), 'and forgets only that column');
    await doubleClick(again, 'agents');
    assert.deepEqual([(await widths(again)).rooms, (await widths(again)).agents, await stored(again)], [240, 260, null], 'both defaults leave nothing stored');
    await again.close();
  }

  // An open thread: at 1280px and wider it sits beside the members column, and the chat keeps its minimum beside both; narrower it takes the members
  // column's place (and the handle goes with the column). The members column's width moves the chat's margins and the thread's offset together.
  {
    const page = await mount();
    await drag(page, 'agents', -140); // 400
    await page.evaluate(() => { document.querySelector('#thread-panel').hidden = false; });
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
    const open = await page.evaluate(() => { const px = node => Math.round(parseFloat(getComputedStyle(document.querySelector(node)).marginRight)); const thread = getComputedStyle(document.querySelector('#thread-panel')); return { margin: px('#messages'), right: Math.round(parseFloat(thread.right)), messages: Math.round(document.querySelector('#messages').getBoundingClientRect().width) }; });
    const sizes = await widths(page);
    assert.deepEqual([open.margin, open.right, open.messages], [sizes.agents + 6 + 420, sizes.agents + 6, 360], `the thread sits beside the members column and the chat keeps its minimum: ${JSON.stringify({ open, sizes })}`);
    assert.ok(sizes.rooms >= 180 && sizes.agents >= 200 && sizes.rooms + sizes.agents <= 1280 - 360 - 420 - 24, 'both columns were pulled in to make room for the thread');
    await page.evaluate(() => { document.querySelector('#thread-panel').hidden = true; });
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
    assert.deepEqual([(await widths(page)).rooms, (await widths(page)).agents], [240, 400], 'closing the thread gives the chosen widths back');
    // narrower than 1280px the thread takes the column's place: no members handle
    await page.setViewport({ width: 1000, height: 800 });
    await page.evaluate(() => { document.querySelector('#thread-panel').hidden = false; });
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
    assert.deepEqual(await hiddenHandles(page), { rooms: false, agents: true }, 'the members handle is gone while a thread has taken its column');
    await page.evaluate(() => { document.querySelector('#thread-panel').hidden = true; });
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
    assert.deepEqual(await hiddenHandles(page), { rooms: false, agents: false }, 'and is back with the column');
    // no room selected: the members column is not shown, and neither is its handle
    await page.evaluate(() => { document.querySelector('#app-view').classList.add('no-room'); });
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
    assert.deepEqual(await hiddenHandles(page), { rooms: false, agents: true }, 'no members handle without a room');
    await page.evaluate(() => { document.querySelector('#app-view').classList.remove('no-room'); });
    await page.close();
  }

  // The Usages overview stands in for the members column: the handle goes but the column's width stays on the variable, so the overview keeps it.
  {
    const page = await mount();
    await drag(page, 'agents', -60); // 320
    await page.evaluate(() => { const popover = document.querySelector('#usages-popover'); popover.hidden = false; document.querySelector('.chat').append(popover); document.querySelector('.chat').classList.add('usages-open'); }); // (the app moves the overview into the chat while it is open)
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80)));
    assert.deepEqual(await hiddenHandles(page), { rooms: false, agents: true }, 'no members handle while Usages is open');
    const overview = await page.evaluate(() => ({ popover: Math.round(parseFloat(getComputedStyle(document.querySelector('#usages-popover')).width)), variable: document.querySelector('#app-view').style.getPropertyValue('--agents-w'), box: getComputedStyle(document.querySelector('#usages-popover')).boxSizing, display: getComputedStyle(document.querySelector('#usages-popover')).display }));
    assert.deepEqual([overview.popover, overview.variable], [320, '320px'], `the overview has the width of the members column: ${JSON.stringify(overview)}`);
    await page.evaluate(() => { document.querySelector('.chat').classList.remove('usages-open'); });
    await page.close();
  }

  // A drag that loses its pointer ends: the window losing focus leaves neither the dragging state on the page nor an unwritten width.
  {
    const page = await mount();
    const node = await handle(page, 'rooms'), from = await centre(node);
    await page.mouse.move(from.x, from.y); await page.mouse.down(); await page.mouse.move(from.x + 40, from.y, { steps: 3 });
    assert.equal(await page.evaluate(() => document.documentElement.classList.contains('col-resizing')), true, 'a drag is under way');
    await page.evaluate(() => window.dispatchEvent(new Event('blur')));
    assert.deepEqual([await page.evaluate(() => document.documentElement.classList.contains('col-resizing')), await stored(page)], [false, JSON.stringify({ rooms: 280 })], 'the blur ended it and wrote the width');
    await page.mouse.up();
    await page.close();
  }

  // The gaps are the handles: three rounded panels with a gap between them, a grip of dots that shows while the pointer moves over the window and fades
  // a moment after it stops, and the accent when a gap is hovered.
  {
    const page = await mount({ hold: 700 });
    const geometry = await page.evaluate(() => {
      const box = selector => { const r = document.querySelector(selector).getBoundingClientRect(); return { left: Math.round(r.left), right: Math.round(r.right), width: Math.round(r.width) }; };
      const radius = (selector, pseudo) => getComputedStyle(document.querySelector(selector), pseudo).borderTopLeftRadius;
      return { sidebar: box('.sidebar'), chat: box('.chat'), agents: box('#agents-panel'), rooms: box('.col-resizer-rooms'), members: box('.col-resizer-agents'),
        radii: [radius('.sidebar'), radius('.chat', '::before'), radius('#agents-panel')], page: getComputedStyle(document.querySelector('#app-view')).backgroundColor, sidebarFill: getComputedStyle(document.querySelector('.sidebar')).backgroundColor };
    });
    assert.deepEqual(geometry.radii, ['6px', '6px', '6px'], 'the three columns are rounded panels, with tighter corners');
    assert.equal(geometry.rooms.width, 6, 'the gap is 6px wide'); assert.equal(geometry.members.width, 6);
    assert.deepEqual([geometry.rooms.left, geometry.rooms.right], [geometry.sidebar.right, geometry.chat.left], 'the rooms handle fills the gap between the rooms panel and the chat');
    assert.equal(geometry.members.right, geometry.agents.left, 'the members handle fills the gap before the members panel');
    assert.notEqual(geometry.page, geometry.sidebarFill, 'the panels sit on the page background');
    const grip = () => page.evaluate(() => ({ shown: document.documentElement.classList.contains('col-grips'), opacity: getComputedStyle(document.querySelector('.col-resizer-rooms'), '::after').opacity, fade: getComputedStyle(document.querySelector('.col-resizer-rooms'), '::after').transitionDuration }));
    const wait = ms => page.evaluate(ms => new Promise(resolve => setTimeout(resolve, ms)), ms);
    assert.deepEqual([(await grip()).shown, (await grip()).opacity], [false, '0'], 'no grip until the pointer moves');
    await page.mouse.move(600, 400); await page.mouse.move(640, 420); await wait(60);
    assert.equal((await grip()).shown, true, 'the grip shows while the pointer moves over the window');
    await wait(300); assert.ok(Number((await grip()).opacity) > 0.99, 'fully visible once it has faded in');
    assert.equal((await grip()).fade, '0.25s', 'and fades out over a quarter of a second');
    await wait(1000);
    assert.deepEqual([(await grip()).shown, (await grip()).opacity], [false, '0'], 'the grip is gone a moment after the pointer stops');
    assert.equal(await page.evaluate(async () => (await import('/js/column-resize.js')).GRIP_HOLD_MS), 3000, 'it stays for three seconds by default');
    // Hovered, a gap is filled with the accent and its grip stays (the pointer is no longer moving, so it is the hover that holds it).
    const from = await centre(await handle(page, 'rooms'));
    await page.mouse.move(from.x, from.y); await wait(500);
    const hover = await page.evaluate(() => { const probe = document.createElement('i'); probe.style.background = 'var(--accent)'; document.body.append(probe); const accent = getComputedStyle(probe).backgroundColor; probe.remove(); const handle = document.querySelector('.col-resizer-rooms'); return { accent, fill: getComputedStyle(handle).backgroundColor, opacity: getComputedStyle(handle, '::after').opacity }; });
    assert.deepEqual([hover.fill, hover.opacity], [hover.accent, '1'], 'a hovered gap is accent-coloured and keeps its grip');
    await page.emulateMediaFeatures([{ name: 'prefers-reduced-motion', value: 'reduce' }]);
    assert.equal((await grip()).fade, '0s', 'with reduced motion the grip does not fade');
    await page.close();
  }

  // Not on a phone, and not beside a collapsed column.
  {
    const phone = await mount({ width: 390, height: 800 });
    assert.deepEqual(await phone.$$eval('.col-resizer', nodes => nodes.map(node => getComputedStyle(node).display)), ['none', 'none', 'none'], 'a phone has no handles: its columns are drawers');
    await phone.close();
    const page = await mount();
    await page.evaluate(() => { document.querySelector('#app-view').classList.add('rooms-collapsed'); });
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 60)));
    assert.deepEqual(await hiddenHandles(page), { rooms: true, agents: false }, 'a collapsed rooms column has no handle');
    await page.evaluate(() => { document.querySelector('#app-view').classList.remove('rooms-collapsed'); document.querySelector('.chat').classList.add('agents-collapsed'); });
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 60)));
    assert.deepEqual(await hiddenHandles(page), { rooms: false, agents: true }, 'nor does a collapsed members column');
    await page.close();
  }

  // The thread column: its own handle on the panel's left edge, a drag and the keys move --thread-w (the chat's margins and the panel follow), the width is
  // kept across a reload, a double click resets it, and the handle is there only while a thread is open.
  {
    const open = async (page, shut = false) => { await page.evaluate(shut => { document.querySelector('#thread-panel').hidden = shut; }, shut); await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 80))); };
    const thread = page => page.evaluate(() => ({ panel: Math.round(document.querySelector('#thread-panel').getBoundingClientRect().width), variable: document.querySelector('#app-view').style.getPropertyValue('--thread-w'), margin: Math.round(parseFloat(getComputedStyle(document.querySelector('#messages')).marginRight)), headerMargin: Math.round(parseFloat(getComputedStyle(document.querySelector('.chat > header')).marginRight)), agents: Math.round(document.querySelector('#agents-panel').getBoundingClientRect().width) }));
    const page = await mount({ width: 1600 });
    assert.equal(await page.$eval('.col-resizer-thread', node => node.hidden), true, 'no thread handle while no thread is open');
    await open(page);
    assert.equal(await page.$eval('.col-resizer-thread', node => node.hidden), false, 'the handle shows with an open thread');
    let state = await thread(page);
    assert.deepEqual([state.panel, state.variable], [420, '420px'], 'the thread opens at its default width');
    await drag(page, 'thread', -100);
    state = await thread(page);
    assert.equal(state.panel, 520, 'dragging the handle left widens the thread');
    assert.equal(state.margin, state.agents + 6 + 520, 'the chat gives up the new width');
    assert.equal(state.headerMargin, state.margin, 'and so does the room header, so its icons end before the thread');
    assert.equal((await aria(page, 'thread')).now, 520);
    await handle(page, 'thread').then(node => node.focus());
    await page.keyboard.press('ArrowRight');
    assert.equal((await thread(page)).panel, 504, 'ArrowRight narrows by one step');
    await page.keyboard.press('Home');
    assert.equal((await thread(page)).panel, 320, 'Home goes to the minimum');
    await page.keyboard.press('End');
    assert.ok((await thread(page)).panel >= 700, 'End goes to the maximum the window leaves');
    await drag(page, 'thread', 200);
    const kept = (await thread(page)).panel;
    await page.reload(); await page.evaluate(async () => { document.querySelector('#app-view').hidden = false; for (const view of ['#boot-view', '#login-view', '#onboarding-view', '#welcome-view']) { const node = document.querySelector(view); if (node) node.hidden = true; } const { ColumnResize } = await import('/js/column-resize.js'); window.__columns = new ColumnResize(); });
    await open(page);
    assert.equal((await thread(page)).panel, kept, 'the width is kept across a reload');
    await doubleClick(page, 'thread');
    assert.equal((await thread(page)).panel, 420, 'a double click resets it');
    await open(page, true);
    assert.equal(await page.evaluate(() => document.querySelector('#app-view').style.getPropertyValue('--thread-w')), '', 'closing the thread drops the variable');
    await page.close();
  }

  await browser.close(); server.close();
  assert.deepEqual(errors, []);
  console.log('columns page: ok');
}
main().catch(error => { console.error(error); process.exit(1); });

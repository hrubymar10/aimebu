// The high-contrast themes (dark and light) in the real app: they look as v1's do. Links, mentions, message refs and sender names are the theme's accent
// (v1's orange in the dark theme, its blue in the light one), a primary button and Send are v1's green, a plain button is v1's light one, the read-receipt avatars take the cyan outline, and what
// v2 added (the top-bar badge, the composer's emoji and format buttons, the health chip) is set in that palette and stays readable (4.5:1).
// `AUTH_BINARY` shares a built binary.
const assert = require('node:assert/strict');
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
    for (const theme of ['high-contrast-dark', 'high-contrast-light']) {
      const seen = await page.evaluate(theme => {
        document.documentElement.dataset.theme = theme;
        const parse = text => text.match(/[\d.]+/g).slice(0, 3).map(Number);
        const lum = ([r, g, b]) => { const f = v => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b); };
        const ratio = (a, b) => { const [x, y] = [lum(parse(a)), lum(parse(b))].sort((p, q) => q - p); return (x + 0.05) / (y + 0.05); };
        const style = el => getComputedStyle(el);
        const badge = document.querySelector('#inbox-count'); badge.hidden = false; badge.textContent = '2';
        const make = cls => { const el = document.createElement('button'); el.className = cls; el.textContent = 'x'; document.body.append(el); return el; };
        const primary = make('btn btn-primary'), secondary = make('btn');
        const fill = el => style(el).backgroundColor, edge = el => `${style(el).borderTopWidth} ${style(el).borderTopStyle} ${style(el).borderTopColor}`;
        const tools = ['.attachment-picker-btn', '#emoji-button', '.composer-format', '.composer-aa'].map(sel => document.querySelector(`#composer ${sel}`));
        const send = document.querySelector('#composer .btn-send');
        const link = document.createElement('a'); link.href = '#'; const body = document.createElement('div'); body.className = 'chat-msg-body md-rendered'; body.append(link); document.body.append(body);
        const mention = document.createElement('span'); mention.className = 'mention'; document.body.append(mention);
        const ref = document.createElement('a'); ref.className = 'msg-ref'; document.body.append(ref);
        const from = document.createElement('span'); from.className = 'chat-msg-from'; document.body.append(from);
        const health = document.createElement('span'); health.className = 'room-health'; health.textContent = '1'; document.body.append(health);
        const receipt = document.createElement('span'); receipt.className = 'receipt-avatar'; document.body.append(receipt);
        const pill = document.createElement('button'); pill.className = 'row-btn reaction-pill'; pill.setAttribute('aria-pressed', 'true'); document.body.append(pill);
        const healthBg = style(health).backgroundColor;
        const resolve = (prop, name) => { const el = document.createElement('i'); el.style[prop] = `var(${name})`; document.body.append(el); return style(el)[prop]; };
        const hues = { accentRgb: resolve('color', '--accent'), borderRgb: resolve('color', '--border'), link: style(link).color, mention: style(mention).color, ref: style(ref).color, from: style(from).color, text: style(document.body).color };
        return {
          hues,
          health: ratio(style(health).color, /rgba/.test(healthBg) ? style(document.body).backgroundColor : healthBg), receiptEdge: style(receipt).borderTopColor, green: resolve('backgroundColor', '--green'), tertiary: resolve('backgroundColor', '--bg-tertiary'), pageFill: style(document.body).backgroundColor, sendFill: fill(send), sendEdgeColor: style(send).borderTopColor, pillFill: style(pill).backgroundColor,
          badge: ratio(style(badge).color, style(badge).backgroundColor),
          secondaryLook: [fill(secondary), style(secondary).color], primaryDiffers: fill(primary) !== fill(secondary), primaryFill: fill(primary), primaryText: ratio(style(primary).color, fill(primary)),
          tools: tools.map(el => el && edge(el)), toolFill: tools.map(el => el && fill(el)),
          sendMuted: (() => { const copy = send.cloneNode(true); copy.disabled = true; send.after(copy); const r = ratio(style(copy).color, fill(copy)); copy.remove(); return r; })(), send: edge(send), sendIsPrimary: send.classList.contains('btn-primary'), sendText: ratio(style(send).color, fill(send)), secondaryEdge: edge(secondary),
        };
      }, theme);
      assert.deepEqual([seen.hues.link, seen.hues.mention, seen.hues.ref, seen.hues.from], Array(4).fill(seen.hues.accentRgb), `links, mentions, message refs and sender names are the accent in ${theme}`);
      assert.ok(seen.health >= 4.5, `the health chip text is ${seen.health.toFixed(1)}:1 in ${theme}`);
      assert.equal(seen.receiptEdge, seen.hues.borderRgb, `a read-receipt avatar has the theme's outline in ${theme}`);
      assert.deepEqual(seen.secondaryLook, ['rgb(239, 239, 239)', 'rgb(0, 0, 0)'], `a plain button is v1's: the browser's light fill and black text in ${theme}`);
      assert.ok(seen.badge >= 4.5, `badge text on its fill is ${seen.badge.toFixed(1)}:1 in ${theme}`);
      assert.ok(seen.primaryDiffers && seen.primaryText >= 4.5 && seen.primaryFill === seen.green, `a primary button is filled differently from a secondary one, with readable text (${seen.primaryText.toFixed(1)}:1) in ${theme}`);
      assert.ok(seen.tools.every(Boolean) && new Set(seen.tools).size === 1 && !seen.tools[0].startsWith('0px'), `the composer buttons share one border: ${JSON.stringify(seen.tools)}`);
      assert.deepEqual([...new Set(seen.toolFill)], [seen.tertiary], `and one fill, v1's attach button: ${JSON.stringify(seen.toolFill)}`);
      assert.equal(seen.sendFill, seen.green, `Send is v1's green in ${theme}`);
      assert.equal(seen.sendEdgeColor, seen.green, 'and is edged in that green, as in v1');
      assert.notEqual(seen.pillFill, seen.pageFill, `a reaction you made is tinted (as in v1), not the page colour: ${seen.pillFill}`);
      assert.ok(seen.sendText >= 4.5, `Send is readable (${seen.sendText.toFixed(1)}:1)`);
      assert.ok(seen.sendMuted >= 4.5, `and so is a Send that is off (${seen.sendMuted.toFixed(1)}:1)`);
      // The thread composer's buttons are boxed like the main composer's, a reply's quote is the accent (v1's link), a message id is the muted text colour,
      // the memory usage bar is a black track with an outline, and an emoji cell is outlined only when hovered or focused.
      const more = await page.evaluate(() => {
        const parse = text => text.match(/[\d.]+/g).slice(0, 3).map(Number);
        const lum = ([r, g, b]) => { const f = v => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }; return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b); };
        const ratio = (a, b) => { const [x, y] = [lum(parse(a)), lum(parse(b))].sort((p, q) => q - p); return (x + 0.05) / (y + 0.05); };
        const style = el => getComputedStyle(el), make = (tag, cls, parent = document.body) => { const el = document.createElement(tag); el.className = cls; parent.append(el); return el; };
        const resolve = (prop, name) => { const el = document.createElement('i'); el.style[prop] = `var(${name})`; document.body.append(el); return style(el)[prop]; };
        const edge = el => `${style(el).borderTopWidth} ${style(el).borderTopStyle} ${style(el).borderTopColor}`;
        const thread = ['#thread-composer .attachment-picker-btn', '#thread-emoji-button', '#thread-composer .composer-format', '#thread-composer .composer-aa'].map(sel => document.querySelector(sel));
        const history = make('div', 'answer-history'), chip = make('button', 'btn proposed-answer-btn', history), asked = make('button', 'btn open-questions-open', history), live = make('button', 'btn proposed-answer-btn'), liveAsk = make('button', 'btn open-questions-open'); chip.disabled = true; asked.disabled = true;
        const card = make('article', 'agent-card'), review = make('button', 'btn agent-recovery', card); review.textContent = 'Review in Inbox';
        const quote = make('button', 'row-btn quote'), id = make('button', 'row-btn quiet-glyph chat-msg-id'), bar = make('div', 'memory-usage-bar'), cell = make('button', 'row-btn emoji-cell');
        return { thread: thread.map(el => el && edge(el)), main: edge(document.querySelector('#composer .attachment-picker-btn')), quote: style(quote).color, accent: resolve('color', '--accent'), id: style(id).color, muted: resolve('color', '--text-muted'),
          bar: [style(bar).borderTopWidth, style(bar).backgroundColor, resolve('backgroundColor', '--bg-primary')], review: [style(review).backgroundColor, style(review).borderTopStyle, style(review).borderTopColor, style(review).textDecorationLine, ratio(style(review).color, style(review).backgroundColor)], live: [live, liveAsk].map(el => [style(el).backgroundColor, style(el).color, style(el).borderTopStyle, style(el).borderTopColor]), chips: [chip, asked].map(el => [style(el).backgroundColor, style(el).borderTopStyle, style(el).borderTopColor, style(el).opacity]), border: resolve('color', '--border'), page: resolve('backgroundColor', '--bg-primary'), text: resolve('color', '--text-primary'), cell: style(cell).borderTopColor, header: style(document.querySelector('#room-info')).backgroundColor, tertiary: resolve('backgroundColor', '--bg-tertiary') };
      });
      assert.ok(more.thread.every(Boolean) && new Set([...more.thread, more.main]).size === 1, `the thread composer's buttons are boxed like the main one's in ${theme}: ${JSON.stringify(more)}`);
      assert.equal(more.quote, more.accent, `a reply's quote is the accent in ${theme}`);
      assert.equal(more.id, more.muted, `a message id is the muted text colour in ${theme}`);
      assert.deepEqual([more.bar[0], more.bar[1]], ['1px', more.bar[2]], `the memory usage bar is a track with an outline in ${theme}`);
      assert.deepEqual(more.chips, [[more.tertiary, 'solid', more.border, '0.52'], [more.tertiary, 'solid', more.border, '0.58']], `a closed proposed answer and its question button are v1's dimmed chip, solid and not grey, in ${theme}`);
      assert.deepEqual(more.live, Array(2).fill([more.tertiary, more.text, 'solid', more.border]), `an open proposed answer and the question button are v1's chip, not the plain light button, in ${theme}`);
      assert.deepEqual(more.review.slice(0, 4), [more.page, 'solid', more.border, 'none'], `"Review in Inbox" on an agent card is an outlined control, not the plain button, in ${theme}`);
      assert.ok(more.review[4] >= 4.5, `and its text reads (${more.review[4].toFixed(1)}:1) in ${theme}`);
      assert.equal(more.header, more.tertiary, `the room header's icon buttons have v1's tertiary fill in ${theme}`);
      assert.equal(more.cell, 'rgba(0, 0, 0, 0)', `an emoji cell is not outlined at rest in ${theme}`);
    }
    assert.deepEqual(server.errors, []);
    console.log('high-contrast page tests passed');
  } finally { await server.stop(); }
}
main().catch(error => { console.error(error); process.exit(1); });

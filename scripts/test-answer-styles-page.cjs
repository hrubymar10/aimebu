// Render the four question states in Chromium, exercise each answered style, and keep comparison screenshots.
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
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  const errors = [];
  try {
    const page = await browser.newPage();
    page.on('pageerror', error => errors.push(error.message));
    await page.setViewport({ width: 1000, height: 700 });
    await page.goto(`http://127.0.0.1:${server.address().port}/fixture.html`);
    await page.evaluate(async () => {
      document.querySelector('#boot-view').style.display = 'none';
      document.querySelector('#app-view').style.display = 'none';
      const host = document.createElement('main');
      host.id = 'answer-style-fixture';
      host.style.cssText = 'box-sizing:border-box;width:780px;min-height:280px;padding:20px;background:var(--bg-primary);color:var(--text-primary);font:13px var(--font-sans);';
      document.body.append(host);
      const { html, render } = await import('/js/ui.js');
      const { AnswerControls } = await import('/js/components/answer-controls.js');
      const proposed = { proposed_answers: ['Ship now', 'Wait for review'] };
      const questions = { open_questions: [{ question: 'Route?', options: ['North', 'South'] }, { question: 'When?', options: ['Now', 'Later'] }] };
      window.drawAnswers = () => render(html`<div>
        <section><h3>Proposed answer · answered</h3><${AnswerControls} item=${proposed} answered="@agent Ship now" answerAuthor="owner" answerReplyNo=${42} /></section>
        <section><h3>Open Questions · answered</h3><${AnswerControls} item=${questions} answered=${'Q1) a) North\nQ2) b) Later'} answerAuthor="owner" answerReplyNo=${43} /></section>
        <section><h3>Proposed answer · open</h3><${AnswerControls} item=${proposed} submit=${() => true} /></section>
        <section><h3>Open Questions · open</h3><${AnswerControls} item=${questions} submit=${() => true} /></section>
        <section><h3>Open Questions · answered, body not loaded</h3><${AnswerControls} item=${questions} answered="" answerAuthor="owner" /></section>
        <section><h3>Proposed answer · answered, body not loaded</h3><${AnswerControls} item=${proposed} answered="" answerAuthor="owner" /></section>
      </div>`, host);
      window.drawAnswers();
    });
    const shots = path.join(__dirname, '..', '.aimebu', 'answer-style-shots');
    fs.mkdirSync(shots, { recursive: true });
    for (const theme of ['dark', 'high-contrast-dark', 'high-contrast-light']) for (const variant of ['a', 'b', 'c']) {
      await page.evaluate(({ theme, variant }) => {
        document.documentElement.dataset.theme = theme;
        document.documentElement.setAttribute('data-ui-answered-choices', variant);
        document.dispatchEvent(new CustomEvent('ui-variant-change', { detail: { experiment: 'answered-choices', variant } }));
      }, { theme, variant });
      await page.waitForFunction(variant => document.querySelectorAll(`#answer-style-fixture .answer-variant-${variant}`).length === 4, {}, variant);
      await page.waitForFunction(() => !document.getAnimations().some(animation => animation.playState === 'running'));
      const states = await page.$$eval('#answer-style-fixture section', sections => sections.map(section => ({
        text: section.querySelector('.answer-controls')?.textContent || '',
        resolved: !!section.querySelector('.answer-controls-resolved'),
        check: !!section.querySelector('.icon-mask-check'),
        buttons: [...section.querySelectorAll('.answer-controls button')].map(button => button.disabled),
      })));
      assert.deepEqual(states.map(state => state.resolved), [true, true, false, false, true, true]);
      // A set of questions shows its count; an answer that is not loaded yet names who answered, never "Answer recorded".
      assert.ok(states[1].text.includes('(2/2)') && !states[0].text.includes('('), 'only a set of questions carries a count');
      assert.ok(states[4].text.includes('Answered by owner') && states[4].text.includes('(2/2)') && states[5].text.includes('Answered by owner') && !/Answer recorded/.test(states[4].text + states[5].text));
      assert.ok(states[0].text.includes('Ship now') && states[1].text.includes('North') && states[1].text.includes('Later'));
      assert.ok(states[0].text.includes('owner') && states[1].text.includes('owner') && states[0].check && states[1].check);
      assert.ok(states[2].buttons.some(disabled => !disabled) && states[3].buttons.some(disabled => !disabled));
      if (variant === 'b') {
        const colors = await page.$$eval('.answer-choice-pill.chosen', nodes => nodes.map(node => [getComputedStyle(node).opacity, getComputedStyle(node).color, getComputedStyle(node).backgroundColor]));
        assert.deepEqual(colors.map(color => color[0]), ['1', '1', '1', '1']);
        assert.ok(colors.every(color => color.join() === colors[0].join()), 'every chosen pill retains the same contrast');
      }
      if (theme === 'dark' || theme === 'high-contrast-dark') await (await page.$('#answer-style-fixture')).screenshot({ path: path.join(shots, `${variant}-${theme}.png`) });
    }
    await page.evaluate(() => { document.documentElement.dataset.theme = 'dark'; document.documentElement.setAttribute('data-ui-answered-choices', 'a'); document.dispatchEvent(new CustomEvent('ui-variant-change', { detail: { experiment: 'answered-choices', variant: 'a' } })); });
    await page.waitForSelector('.answer-choice-chip');
    // The two receipts A12-4b changed, in the dark theme.
    const changed = path.join(__dirname, '..', '.aimebu', 'a124b-shots');
    fs.mkdirSync(changed, { recursive: true });
    const sections = await page.$$('#answer-style-fixture section');
    await sections[1].screenshot({ path: path.join(changed, 'multi-question-answered-dark.png') });
    await sections[4].screenshot({ path: path.join(changed, 'body-not-loaded-dark.png') });
    await page.evaluate(() => document.querySelector('.answer-choice-chip').click());
    assert.deepEqual(await page.$$eval('.answer-choice-options button', buttons => buttons.map(button => button.disabled)), [true, true]);
    await page.evaluate(() => { document.documentElement.setAttribute('data-ui-answered-choices', 'c'); document.dispatchEvent(new CustomEvent('ui-variant-change', { detail: { experiment: 'answered-choices', variant: 'c' } })); });
    await page.waitForSelector('.answer-choice-inline');
    await page.focus('.answer-choice-inline');
    assert.equal(await page.$eval('.answer-choice-tooltip', node => getComputedStyle(node).display), 'flex');
    assert.deepEqual(errors, []);
    await page.close();
    console.log('answer styles page: ok');
  } finally { await browser.close(); server.close(); }
}
main().catch(error => { console.error(error); process.exitCode = 1; server.close(); });

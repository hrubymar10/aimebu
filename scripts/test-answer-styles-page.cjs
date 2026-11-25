// Render the four question states in Chromium, check the answered receipt's shape against the unanswered buttons in four themes (ANSWER_SHOTS_DIR keeps screenshots).
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
    const shots = process.env.ANSWER_SHOTS_DIR || '';
    if (shots) fs.mkdirSync(shots, { recursive: true });
    for (const theme of ['dark', 'light', 'high-contrast-dark', 'high-contrast-light']) {
      await page.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
      await page.waitForFunction(() => document.querySelectorAll('#answer-style-fixture .answer-choice-row').length === 4);
      await page.waitForFunction(() => !document.getAnimations().some(animation => animation.playState === 'running'));
      const states = await page.$$eval('#answer-style-fixture section', sections => sections.map(section => ({
        text: [...section.querySelectorAll('.answer-choice-pill.chosen, .answer-choice-count, .answer-choice-by')].map(node => node.textContent.trim()).join(' '),
        resolved: !!section.querySelector('.answer-controls-resolved'),
        check: !!section.querySelector('.icon-mask-check'),
        pills: [...section.querySelectorAll('.answer-choice-pill')].map(pill => [pill.textContent.trim(), pill.disabled, pill.classList.contains('chosen')]),
        buttons: [...section.querySelectorAll('.answer-controls button')].map(button => button.disabled),
      })));
      assert.deepEqual(states.map(state => state.resolved), [true, true, false, false, true, true]);
      // A set of questions shows its count; an answer that is not loaded yet names who answered, never "Answer recorded".
      assert.equal(states[0].text, 'Ship now by owner');
      assert.equal(states[1].text, 'Q1) a) North · Q2) b) Later (2/2) by owner', 'only a set of questions carries a count');
      assert.equal(states[4].text, 'Answered by owner (2/2)');
      assert.equal(states[5].text, 'Answered by owner');
      assert.deepEqual(states[0].pills, [['Ship now', true, true], ['Wait for review', true, false]], 'the offered options stay, disabled, the chosen one marked');
      assert.equal(states[1].pills.length, 1, 'a set of questions is one chosen row');
      assert.ok(states[0].check && states[1].check);
      assert.ok(states[2].buttons.some(disabled => !disabled) && states[3].buttons.some(disabled => !disabled));
      // The receipt's options have the shape of the unanswered buttons: the same radius, height, padding and font.
      const shape = await page.evaluate(() => {
        const read = node => { const style = getComputedStyle(node); return { radius: style.borderRadius, minHeight: style.minHeight, padding: style.padding, font: `${style.fontSize} ${style.fontWeight} ${style.fontFamily}`, lineHeight: style.lineHeight }; };
        return { open: read(document.querySelector('.proposed-answer-btn:not(:disabled)')), answered: [...document.querySelectorAll('.answer-choice-pill')].map(read) };
      });
      assert.equal(shape.open.radius, '5px');
      for (const answered of shape.answered) assert.deepEqual(answered, shape.open, `${theme}: an answered option has the shape of an unanswered one`);
      const colors = await page.$$eval('.answer-choice-pill.chosen', nodes => nodes.map(node => [getComputedStyle(node).opacity, getComputedStyle(node).color, getComputedStyle(node).backgroundColor]));
      assert.deepEqual(colors.map(color => color[0]), ['1', '1', '1', '1']);
      assert.ok(colors.every(color => color.join() === colors[0].join()), 'every chosen option retains the same contrast');
      assert.notEqual(colors[0][1], colors[0][2], 'the chosen option is readable: text differs from its background');
      if (shots) await (await page.$('#answer-style-fixture')).screenshot({ path: path.join(shots, `${theme}.png`) });
    }
    assert.deepEqual(errors, []);
    await page.close();
    console.log('answer styles page: ok');
  } finally { await browser.close(); server.close(); }
}
main().catch(error => { console.error(error); process.exitCode = 1; server.close(); });

// Phone layout audit (no server): tap-target size, overlap and reachability on the top bar, the message
// action bar, the settings navigation and code-block copy buttons, with touch emulation at common phone
// widths, plus the chat frame, thread panel, health popover and static dialogs also in landscape and at
// tablet sizes. Usage: node scripts/phone-audit.cjs [--json]   (exit status 1 on any finding)
const { withPages } = require('./ui-fixture.cjs');

const MIN = 40;
// Landscape phones and tablets run the same checks as portrait phones; all are touch devices.
const EXTRA = [['844x390', { width: 844, height: 390 }], ['740x360', { width: 740, height: 360 }], ['744x1000', { width: 744, height: 1000 }], ['768x1024', { width: 768, height: 1024 }], ['820x1180', { width: 820, height: 1180 }], ['1024x768', { width: 1024, height: 768 }]];
const PHONES = [['320x640', { width: 320, height: 640 }], ['360x740', { width: 360, height: 740 }], ['390x844', { width: 390, height: 844 }], ['480x800', { width: 480, height: 800 }], ['600x900', { width: 600, height: 900 }]];

function audit(MIN) {
  const findings = [];
  document.querySelector('#app-view').classList.add('chat-open');
  const box = element => element.getBoundingClientRect();
  const name = element => `${element.tagName.toLowerCase()}${element.id ? '#' + element.id : ''}${element.className && typeof element.className === 'string' ? '.' + element.className.trim().split(/\s+/)[0] : ''}${element.textContent.trim() ? ` "${element.textContent.trim().slice(0, 14)}"` : ''}`;
  const vw = window.innerWidth;
  const small = (scope, label, selector = 'button, a[href], input:not([type=hidden]), select, textarea, summary') => {
    for (const element of scope.querySelectorAll(selector)) {
      const rect = box(element); if (!rect.width || !rect.height) continue;
      if (Math.min(rect.width, rect.height) < MIN) findings.push(`${label}: ${name(element)} is ${Math.round(rect.width)}x${Math.round(rect.height)}`);
    }
  };
  const overlaps = (elements, label) => {
    const rects = elements.map(element => [element, box(element)]).filter(([, rect]) => rect.width && rect.height);
    for (let i = 0; i < rects.length; i++) for (let j = i + 1; j < rects.length; j++) {
      const [a, ra] = rects[i], [b, rb] = rects[j];
      const w = Math.min(ra.right, rb.right) - Math.max(ra.left, rb.left), h = Math.min(ra.bottom, rb.bottom) - Math.max(ra.top, rb.top);
      if (w > 1 && h > 1) findings.push(`${label}: ${name(a)} overlaps ${name(b)}`);
    }
  };

  // Top bar
  const bar = document.querySelector('.workspace-bar'), nav = bar.querySelector('nav');
  const barButtons = [...nav.querySelectorAll('button')].filter(button => !button.hidden && box(button).width);
  small(nav, 'top bar', 'button');
  overlaps(barButtons, 'top bar');
  for (const label of ['#logout', '#inbox-toggle']) { const element = document.querySelector(label); if (element && box(element).height > 48) findings.push(`top bar: ${name(element)} wraps onto two lines (${Math.round(box(element).height)}px tall)`); }
  const badge = document.querySelector('#inbox-count'); if (badge) { badge.hidden = false; badge.textContent = '12'; if (box(badge).height > 24) findings.push(`top bar: inbox badge wraps (${Math.round(box(badge).height)}px tall)`); badge.hidden = true; }
  // v1's top bar is 53px tall, 49px up to 480px wide.
  const wantBar = vw <= 480 ? 49 : 53;
  if (Math.abs(box(bar).height - wantBar) > 1) findings.push(`top bar: ${Math.round(box(bar).height)}px tall at ${vw}px wide, v1's is ${wantBar}px`);
  const scroller = nav.scrollWidth > nav.clientWidth + 1;
  if (vw <= 900 && scroller) findings.push(`top bar: the bar scrolls sideways at ${vw}px (${nav.scrollWidth} > ${nav.clientWidth}); controls must fit`);
  // The experiment pill (a testing aid, shown here with its long label) must not be squashed or clipped, and every icon button keeps its size.
  { const pill = document.querySelector('#ui-variant-toggle'), was = pill.hidden; pill.hidden = false; pill.querySelector('.ui-variant-testing').textContent = 'TESTING: composer layout';
    if (pill.scrollWidth > pill.clientWidth + 1) findings.push(`top bar: the experiment pill is clipped at ${vw}px (${pill.scrollWidth} > ${pill.clientWidth})`);
    const sizes = new Set([...nav.querySelectorAll('.icon-btn')].filter(button => box(button).width).map(button => `${Math.round(box(button).width)}x${Math.round(box(button).height)}`)); if (sizes.size > 1) findings.push(`top bar: the icon buttons differ in size at ${vw}px: ${[...sizes].join(' ')}`);
    pill.hidden = was; }
  if (box(bar).right > vw + 1 || document.documentElement.scrollWidth > vw + 1) findings.push(`top bar: page is ${document.documentElement.scrollWidth}px wide in a ${vw}px viewport`);
  // Every control must be reachable: inside the viewport, or reachable by scrolling the bar.
  const navOverflow = getComputedStyle(nav).overflowX;
  for (const button of barButtons) {
    const rect = box(button); if (rect.left >= -1 && rect.right <= vw + 1) continue;
    if (!['auto', 'scroll'].includes(navOverflow)) { findings.push(`top bar: ${name(button)} sits outside the viewport (${Math.round(rect.left)}..${Math.round(rect.right)}) and the bar does not scroll`); continue; }
    nav.scrollLeft = nav.scrollWidth;
    const after = box(button); if (after.left < -1 || after.right > vw + 1) findings.push(`top bar: ${name(button)} stays outside the viewport even when the bar is scrolled to its end`);
    nav.scrollLeft = 0;
  }

  // Message actions (opened state)
  const article = document.querySelector('.message[data-fixture=plain]');
  article.classList.add('actions-open');
  const actions = article.querySelector('.message-actions');
  small(actions, 'message actions', 'button');
  article.classList.remove('actions-open');

  // Settings navigation
  const dialog = document.querySelector('#settings-dialog'); dialog.setAttribute('open', '');
  const tabs = dialog.querySelector('nav'); tabs.style.height = ''; 
  const tabButtons = [...tabs.querySelectorAll('button')].filter(button => !button.hidden);
  const sample = document.createElement('button'); sample.textContent = 'Sample extra tab'; tabs.append(sample);
  const tabsRect = box(tabs), contentRect = box(dialog.querySelector('.settings-content:not([hidden])') || dialog.querySelector('.settings-content'));
  const vertical = new Set([...tabs.querySelectorAll('button')].filter(button => box(button).width).map(button => Math.round(box(button).left))).size === 1;
  if (!vertical) findings.push(`settings: the tab list is not a vertical column (${tabButtons.length + 1} tabs, nav ${Math.round(tabsRect.width)}x${Math.round(tabsRect.height)})`);
  for (const button of tabs.querySelectorAll('button')) { const rect = box(button); if (rect.width && Math.min(rect.width, rect.height) < MIN) findings.push(`settings: tab ${name(button)} is ${Math.round(rect.width)}x${Math.round(rect.height)}`); if (rect.right > tabsRect.right + 1) findings.push(`settings: tab ${name(button)} is clipped by the nav`); }
  if (contentRect.width < 200) findings.push(`settings: content column is only ${Math.round(contentRect.width)}px wide`);
  sample.remove(); dialog.removeAttribute('open');

  // Code block copy button must not sit on the code text, even when the line scrolls
  const pre = document.querySelector('.message[data-fixture=plain] .md-codeblock pre'), copy = pre.closest('.md-codeblock').querySelector('.md-copy'), code = pre.querySelector('code');
  code.textContent = 'x'.repeat(400); pre.scrollLeft = 0;
  const copyRect = box(copy), preRect = box(pre);
  const style = getComputedStyle(pre);
  const lineTop = preRect.top + parseFloat(style.paddingTop);
  if (copyRect.bottom > lineTop + 1 && copyRect.right > preRect.left + parseFloat(style.paddingLeft)) findings.push(`code block: the copy button (${Math.round(copyRect.top - preRect.top)}..${Math.round(copyRect.bottom - preRect.top)}px) overlaps the first code line (starts at ${Math.round(lineTop - preRect.top)}px)`);
  small(pre.closest('.md-codeblock'), 'code block', 'button');
  void scroller;
  return findings;
}

// Surfaces outside the top bar: the chat frame, the thread panel, the health popover and every static dialog.
// Geometry only: inside the viewport, scrollable instead of clipped, 40px targets, a usable message list.
function surfaceAudit(MIN) {
  const out = [];
  const vw = innerWidth, vh = innerHeight;
  const box = element => element.getBoundingClientRect();
  const name = element => `${element.tagName.toLowerCase()}${element.id ? '#' + element.id : ''}${typeof element.className === 'string' && element.className ? '.' + element.className.trim().split(/\s+/)[0] : ''}${element.textContent.trim() ? ` "${element.textContent.trim().slice(0, 14)}"` : ''}`;
  document.querySelector('#app-view').classList.add('chat-open');
  const small = (scope, label) => {
    for (const element of scope.querySelectorAll('button, a[href], input:not([type=hidden]), select, textarea, summary')) {
      const rect = box(element); if (!rect.width || !rect.height) continue;
      let width = rect.width, height = rect.height;
      if (element.matches('input[type=checkbox],input[type=radio]')) { const label = element.closest('label'); if (label) { const labelRect = box(label); width = labelRect.width; height = labelRect.height; } }
      if (Math.min(width, height) < MIN) out.push(`${label}: ${name(element)} is ${Math.round(width)}x${Math.round(height)}`);
    }
  };
  const fits = (element, label) => {
    const rect = box(element);
    if (rect.left < -1 || rect.right > vw + 1 || rect.top < -1 || rect.bottom > vh + 1) out.push(`${label}: outside the viewport (${Math.round(rect.left)},${Math.round(rect.top)} to ${Math.round(rect.right)},${Math.round(rect.bottom)} in ${vw}x${vh})`);
    if (element.scrollWidth > element.clientWidth + 1) out.push(`${label}: scrolls sideways (${element.scrollWidth} > ${element.clientWidth})`);
    if (element.scrollHeight > element.clientHeight + 1 && !['auto', 'scroll'].includes(getComputedStyle(element).overflowY)) out.push(`${label}: taller than its box and does not scroll`);
  };
  // Chat frame: the composer must be fully visible and the message list usable, also in landscape.
  const chat = document.querySelector('.chat'), list = document.querySelector('#messages'), composer = document.querySelector('#composer');
  if (box(chat).bottom > vh + 0.5) out.push(`chat: its bottom edge (${Math.round(box(chat).bottom)}px) is below the viewport (${vh}px), so the composer is cut off`);
  if (box(composer).bottom > vh + 0.5) out.push(`composer: bottom edge ${Math.round(box(composer).bottom)}px is below the viewport (${vh}px)`);
  if (box(list).height < 120) out.push(`messages: the list is only ${Math.round(box(list).height)}px tall`);
  // The "/" command list (fixture: /archive and /spawn) opens above the composer and stays on screen, with rows a thumb can hit.
  const slash = document.querySelector('.quick-commands[data-fixture=quick]');
  if (slash) {
    slash.hidden = false;
    const at = box(slash);
    if (at.width && (at.left < -0.5 || at.right > vw + 0.5)) out.push(`slash list: ${Math.round(at.left)}..${Math.round(at.right)}px leaves the ${vw}px viewport`);
    if (at.width && at.top < -0.5) out.push(`slash list: its top edge (${Math.round(at.top)}px) is above the viewport`);
    for (const row of slash.querySelectorAll('.quick-command')) if (box(row).height && box(row).height < MIN) out.push(`slash list: ${row.textContent} is only ${Math.round(box(row).height)}px tall`);
  }
  // Thread panel
  const thread = document.querySelector('#thread-panel'); thread.hidden = false;
  fits(thread, 'thread panel'); small(thread, 'thread panel');
  const threadList = thread.querySelector('#thread-messages'); if (box(threadList).height < 90) out.push(`thread panel: the reply list is only ${Math.round(box(threadList).height)}px tall`);
  thread.hidden = true;
  // Emoji picker (open in the fixture): a bottom sheet on a phone, anchored and inside the viewport elsewhere; 40px targets.
  const picker = document.querySelector('.emoji-picker');
  if (picker) {
    fits(picker, 'emoji picker'); small(picker, 'emoji picker');
    const rect = box(picker);
    if (vw <= 700 && (Math.abs(rect.bottom - vh) > 1 || rect.left > 1 || rect.right < vw - 1)) out.push(`emoji picker: not a bottom sheet (${Math.round(rect.left)},${Math.round(rect.top)} to ${Math.round(rect.right)},${Math.round(rect.bottom)} in ${vw}x${vh})`);
  }
  // Health popover
  const health = document.querySelector('#health-popover'); health.hidden = false; fits(health, 'health popover'); small(health, 'health popover'); health.hidden = true;
  // Dialogs
  for (const id of ['project-dialog', 'spawn-dialog', 'switch-dialog', 'machine-pair-dialog', 'room-export-dialog', 'room-info-dialog', 'knowledge-dialog', 'import-v1-blocked']) {
    const dialog = document.querySelector('#' + id); dialog.showModal();
    fits(dialog, id); small(dialog, id);
    dialog.close();
  }
  return out;
}

async function menuAudit(page, vw) {
  return page.evaluate(async width => {
    const findings = [];
    const { TopbarMenu } = await import('/js/topbar-menu.js');
    const toggle = document.querySelector('#more-toggle'), menu = document.querySelector('#more-menu');
    if (width > 900) { if (toggle && getComputedStyle(toggle).display !== 'none') findings.push('menu: the overflow button shows above 900px'); return findings; }
    new TopbarMenu();
    let clicked = 0; for (const id of ['#system-events-toggle', '#health-toggle', '#knowledge-toggle', '#logout']) document.querySelector(id).addEventListener('click', () => clicked++);
    for (const id of ['#system-events-toggle', '#health-toggle', '#knowledge-toggle', '#logout']) if (getComputedStyle(document.querySelector(id)).display !== 'none') findings.push(`menu: ${id} is still in the bar at ${width}px`);
    toggle.click();
    if (menu.hidden || toggle.getAttribute('aria-expanded') !== 'true') findings.push('menu: the button does not open it');
    const items = [...menu.querySelectorAll('[role=menuitem]')];
    if (items.map(item => item.textContent.replace(/\d+$/, '')).join() !== 'System events,Health,Memory,Sign out') findings.push(`menu: unexpected items ${items.map(item => item.textContent)}`);
    for (const item of items) { const rect = item.getBoundingClientRect(); if (rect.height < 40) findings.push(`menu: ${item.textContent} is ${Math.round(rect.height)}px tall`); if (rect.left < 0 || rect.right > width) findings.push(`menu: ${item.textContent} is outside the viewport`); }
    if (document.activeElement !== items[0]) findings.push('menu: opening does not focus the first item');
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown' }));
    if (document.activeElement !== items[1]) findings.push('menu: ArrowDown does not move focus');
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    if (!menu.hidden || document.activeElement !== toggle) findings.push('menu: Escape does not close it and return focus to the button');
    toggle.click(); items[1].click();
    if (clicked !== 1 || !menu.hidden) findings.push('menu: an item does not forward its click and close');
    // The first item opens the System events panel and leaves it open (the same click must not close what it opened).
    const panel = document.querySelector('#system-events-popover');
    if (panel) {
      if (!panel.hidden) document.querySelector('#system-events-toggle').click();
      toggle.click(); items[0].click();
      await new Promise(resolve => setTimeout(resolve, 0));
      if (panel.hidden) findings.push('menu: System events does nothing');
      else document.querySelector('#system-events-toggle').click();
    }
    document.querySelector('#health-toggle').classList.add('has-issues');
    await new Promise(resolve => setTimeout(resolve, 0));
    if (!toggle.classList.contains('has-issues')) findings.push('menu: the health warning is not mirrored on the button');
    return findings;
  }, vw);
}

(async () => {
  const report = {};
  await withPages([...PHONES, ...EXTRA], async (name, page) => {
    await page.addStyleTag({ content: '*,*::before,*::after{transition:none!important;animation:none!important}' });
    const phone = PHONES.some(([label]) => label === name), narrow = phone || page.viewport().width <= 900 && page.viewport().height >= 700;
    report[name] = narrow ? await page.evaluate(audit, MIN) : [];
    if (narrow) report[name].push(...await menuAudit(page, page.viewport().width));
    report[name].push(...await page.evaluate(surfaceAudit, MIN));
  }, { touch: true });
  if (process.argv.includes('--json')) { console.log(JSON.stringify(report, null, 1)); return; }
  let total = 0;
  for (const [name, findings] of Object.entries(report)) { console.log(`== ${name}: ${findings.length} findings`); for (const finding of [...new Set(findings)]) console.log('  ' + finding); total += findings.length; }
  if (total) process.exitCode = 1;
})().catch(error => { console.error(error); process.exit(1); });

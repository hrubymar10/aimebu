// Computed-style snapshot of representative elements, for proving that a CSS refactor changes nothing.
// Usage: node scripts/css-snapshot.cjs > before.json; ...refactor...; node scripts/css-snapshot.cjs > after.json; diff before.json after.json
// It serves web/ statically (no Go server, no login), builds fixture markup that mirrors what the
// UI modules create, and records computed styles across themes and viewports.

const { withPages, VIEWPORTS } = require('./ui-fixture.cjs');

const PROPS = ['display', 'position', 'color', 'backgroundColor', 'fontSize', 'fontWeight', 'fontFamily', 'lineHeight', 'fontStyle', 'textAlign', 'textDecorationLine',
  'paddingTop', 'paddingRight', 'paddingBottom', 'paddingLeft', 'marginTop', 'marginRight', 'marginBottom', 'marginLeft',
  'borderTopWidth', 'borderTopStyle', 'borderTopColor', 'borderRightWidth', 'borderRightColor', 'borderBottomWidth', 'borderBottomColor', 'borderLeftWidth', 'borderLeftColor',
  'borderTopLeftRadius', 'boxShadow', 'opacity', 'zIndex', 'overflowX', 'overflowY', 'gap', 'cursor', 'gridTemplateColumns', 'flexDirection', 'alignItems', 'justifyContent', 'whiteSpace', 'outlineStyle'];

// Elements are forced visible while measured so hidden popovers and dialogs report real styles.
const FIXTURE = async () => {
  const [{ renderMarkdown }, { renderVisualPlan }, { SettingsRow, SettingsGroup }, { MentionIndex, highlightMentions }] = await Promise.all([
    import('/js/markdown.js'), import('/js/visual-plan.js'), import('/js/components/settings-row.js'), import('/js/mentions.js')]);
  const freeze = document.createElement('style'); freeze.textContent = '*,*::before,*::after{transition:none!important;animation:none!important}'; document.head.append(freeze);
  document.querySelector('#login-view').hidden = true;
  document.querySelector('#app-view').hidden = false;
  const make = (tag, className, text, parent) => { const node = document.createElement(tag); if (className) node.className = className; if (text) node.textContent = text; if (parent) parent.append(node); return node; };
  const list = document.querySelector('#messages');
  const message = (extra = '', body = '') => {
    const article = make('article', `chat-msg ${extra}`.trim(), '', list); article.dataset.fixture = extra || 'plain';
    const meta = make('div', 'chat-msg-header', '', article); make('strong', 'chat-msg-from-name', 'ada', meta); make('time', '', '12:01', meta); make('button', 'chat-msg-id quiet', '#7', meta);
    const bubble = make('div', 'chat-msg-bubble', '', article);
    make('button', 'row-btn quote', '↪ #3 bert', bubble);
    const content = make('div', 'chat-msg-body', '', bubble);
    content.append(renderMarkdown(body || '## Title\n\nSome **bold** and *italic* text with `code` and https://example.test and #7 and @ada\n\n> quote\n\n- one\n- two\n\n| A | B |\n| :- | -: |\n| 1 | 2 |\n\n```js\nconst x = 1;\n```\n\n~~gone~~'));
    highlightMentions(content, new MentionIndex([{ slug: 'ada', role_key: 'worker', tool: 'codex', model: 'm' }], 'p'));
    const plan = renderVisualPlan({ visual_plan: [{ type: 'markdown', title: 'Plan', data: { markdown: 'Plan **text**' } }, { type: 'checklist', data: { items: [{ text: 'a', checked: true }, { text: 'b' }] } }, { type: 'file-tree', data: { root: { name: 'src', type: 'dir', children: [{ name: 'a.js', type: 'file', note: 'entry' }] } } }] });
    if (plan) bubble.append(plan);
    const actions = make('div', 'message-actions', '', bubble);
    make('button', 'row-btn', 'Reply', actions); make('button', 'row-btn quick-reaction', '👍', actions); make('button', 'row-btn', 'Copy', actions);
    const pills = make('span', 'reaction-pills', '', bubble); const pill = make('button', 'reaction-pill', '👍 2', pills); pill.setAttribute('aria-pressed', 'true');
    return article;
  };
  message(); message('self'); message('needs-attention');
  const system = make('article', 'chat-msg chat-msg-system', '', list); const row = make('div', 'chat-msg-system-row', '', system); make('span', 'chat-msg-body', 'ada joined', row); make('time', '', '12:00', row);
  const agents = document.querySelector('#agents-panel') || document.querySelector('.agents-panel');
  if (agents) {
    const card = make('article', 'agent-card agent-card-compact', '', agents); const title = make('div', 'agent-id agent-title', '', card); make('span', 'agent-online-dot active agent-dot', '', title); const identity = make('span', 'agent-identity', '', title);
    make('span', 'agent-id-text agent-name', 'Ada', identity); make('span', 'agent-role', 'worker', title); make('small', 'agent-meta', 'codex · m', card); const stateBox = make('div', 'agent-state', '', card); make('span', 'agent-state-pill agent-state-badge agent-state-idle', 'idle', stateBox);
    const actions = make('div', 'agent-actions', '', card); make('button', 'btn-icon btn-danger agent-stop', 'Stop', actions);
  }
  const sidebar = document.querySelector('.sidebar');
  const group = make('div', 'room-group', '', sidebar); const roomRow = make('div', 'room-item active', '', group); make('button', 'room-item-name', 'general', roomRow); make('span', 'room-health', '⚠ 2', roomRow);
  const quick = make('div', 'quick-commands', '', document.querySelector('#composer')); quick.dataset.fixture = 'quick'; const option = make('button', 'quick-command', '', quick); make('strong', '', '/archive', option); make('span', '', 'Archive room', option); option.setAttribute('aria-selected', 'true');
  make('button', 'quick-command', '', quick).append(make('strong', '', '/spawn'), make('span', '', 'Open spawn dialog'));
  const section = document.querySelector('#settings-general');
  const { html: fixtureHtml, render: fixtureRender } = await import('/js/ui.js');
  const fixtureRows = make('div', '', '', section);
  fixtureRender(fixtureHtml`<${SettingsGroup}>
    <${SettingsRow} label="Fixture select" desc="A description for the select" id="fx-select"><select><option>Dark</option></select><//>
    <${SettingsRow} label="Fixture toggle" desc="A description for the toggle" id="fx-check" className="settings-row-toggle"><input type="checkbox" /><//>
    <${SettingsRow} label="Fixture area" desc="A stacked row" id="fx-area" className="settings-row-stack"><textarea></textarea><//>
  <//>`, fixtureRows);
};

const TARGETS = [
  'body', '.topbar', '.topbar-left .logo', '#rooms-toggle', '#usages-toggle', '#settings-toggle', '#health-toggle', '.badge',
  '.sidebar', '.room-item', '.room-item .room-item-name', '.room-health',
  '.chat header', '#messages', '#composer', '#composer textarea', '#composer button[type=submit]', '.quick-commands[data-fixture=quick]', '.quick-command', '.quick-command[aria-selected=true]',
  '.chat-msg[data-fixture=plain]', '.chat-msg[data-fixture=self]', '.chat-msg.needs-attention', '.chat-msg-system', '.chat-msg-system-row', '.chat-msg-header', '.chat-msg-from-name', '.chat-msg-header time', 'button.chat-msg-id',
  '.chat-msg-bubble', '.quote', '.chat-msg-body', '.chat-msg-body h2', '.chat-msg-body p', '.chat-msg-body code', '.chat-msg-body a', '.chat-msg-body a.msg-ref', '.chat-msg-body .mention', '.chat-msg-body blockquote', '.chat-msg-body ul', '.chat-msg-body del',
  '.chat-msg-body table', '.chat-msg-body th', '.chat-msg-body td', '.md-codeblock', '.md-codeblock pre', '.md-copy-btn', '.message-actions', '.message-actions button', '.quick-reaction', '.reaction-pill',
  '.visual-plan', '.visual-plan-block', '.visual-plan-block-header', '.visual-plan-block-type', '.visual-plan-block-title', '.visual-plan-block-body', '.visual-plan-check-row', '.visual-plan-file-tree',
  '.agents-panel', '.agent-card', '.agent-name', '.agent-id', '.agent-online-dot', '.agent-role', '.agent-state', '.agent-state-badge', '.agent-actions button',
  '#health-popover', '#usages-popover', '#usages-popover > header', '#usages-popover button',
  '#system-events-popover', '.system-events-popover', '#system-events-toggle',
  '#settings-dialog', '#settings-dialog .settings-body-header', '#settings-dialog nav', '#settings-dialog nav button', '#settings-dialog nav .settings-nav-item.active', '#settings-dialog .settings-section.active', '#settings-dialog h3',
  '#settings-general .settings-group', '#settings-general .settings-row', '#settings-general .settings-row-info', '#settings-general .settings-label', '#settings-general .settings-desc', '#settings-general .settings-control', '#settings-general .settings-control select', '#settings-general .settings-row-toggle input', '#settings-general .settings-row-stack', '#settings-general textarea',
  '#settings-roles .settings-row', '#role-editor .settings-row', '#role-editor textarea', '#room-project .settings-row', '#project-scope-control', '#project-usage-limit', '#settings-notifications .settings-row',
  '#knowledge-dialog', '#knowledge-dialog header', '#room-info-dialog', '#room-export-dialog', '#project-dialog', '.invite-dialog', '.answer-dialog', '#spawn-dialog', '#switch-dialog', '#machine-pair-dialog', '.message-debug-dialog', '#thread-panel', '#thread-composer',
  '#login-form', '#login-view h1',
];

async function snapshot(page, theme) {
  await page.evaluate(theme => { if (theme === 'dark') delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = theme; }, theme);
  return page.evaluate((targets, props) => {
    const out = {};
    for (const selector of targets) {
      const element = document.querySelector(selector);
      if (!element) { out[selector] = null; continue; }
      const wasHidden = element.hidden, hiddenChain = []; for (let node = element; node && node !== document.body; node = node.parentElement) if (node.hidden) { hiddenChain.push(node); node.hidden = false; }
      const isDialog = element.tagName === 'DIALOG' && !element.open; if (isDialog) element.setAttribute('open', '');
      const style = getComputedStyle(element), values = {};
      for (const prop of props) values[prop] = style[prop];
      const box = element.getBoundingClientRect(); values.width = Math.round(box.width); values.height = Math.round(box.height);
      out[selector] = values;
      if (isDialog) element.removeAttribute('open');
      for (const node of hiddenChain) node.hidden = true;
      void wasHidden;
    }
    return out;
  }, TARGETS, PROPS);
}

(async () => {
  const result = {};
  await withPages(VIEWPORTS, async (name, page) => {
    for (const theme of ['dark', 'light', 'high-contrast-dark', 'blue-light']) result[`${name}/${theme}`] = await snapshot(page, theme);
  });
  process.stdout.write(JSON.stringify(result, null, 1) + '\n');
})().catch(error => { console.error(error); process.exit(1); });

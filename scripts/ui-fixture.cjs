// Shared by css-snapshot.cjs and a11y-audit.cjs: serves web/ statically (no Go server, no login).
// launches headless Chromium and builds fixture markup with the real renderers.
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromiumPath } = require('./chromium-path.cjs');
let puppeteer;
try { puppeteer = require('puppeteer-core'); } catch { puppeteer = require('/usr/local/lib/node_modules/puppeteer-core'); }

const { QuotaMock } = require('./quota-mock.cjs');
const quotaMock = new QuotaMock();
const { LimitGuardMock } = require('./limit-guard-mock.cjs');
const guardMock = new LimitGuardMock();
const root = process.env.CSS_WEB_ROOT || path.join(__dirname, '..', 'web');
const types = { '.html': 'text/html', '.js': 'text/javascript', '.mjs': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png', '.json': 'application/json' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname.startsWith('/api/quota-switcher')) {
    let raw = ''; req.on('data', chunk => raw += chunk);
    req.on('end', () => { const answer = quotaMock.handle(req.method, url.pathname, raw ? JSON.parse(raw) : undefined); res.writeHead(answer.status, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(answer.body)); });
    return;
  }
  if (url.pathname.startsWith('/api/limit-guard')) {
    let raw = ''; req.on('data', chunk => raw += chunk);
    req.on('end', () => { const answer = guardMock.handle(req.method, url.pathname, raw ? JSON.parse(raw) : undefined); res.writeHead(answer.status, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(answer.body)); });
    return;
  }
  if (url.pathname.startsWith('/api/')) { res.writeHead(401, { 'Content-Type': 'application/json' }); res.end('{"error":"unauthorized"}'); return; }
  let file = path.join(root, url.pathname === '/' ? 'index.html' : url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});


// Elements are forced visible while measured so hidden popovers and dialogs report real styles.
const FIXTURE = async () => {
  const [{ renderMarkdown }, { renderVisualPlan }, { settingsRow, settingsGroup }, { usageWindowRow }, { MentionIndex, highlightMentions }, { QuotaSettings }, { HttpQuotaSource }, { buildMessageActions }] = await Promise.all([
    import('/js/markdown.js'), import('/js/visual-plan.js'), import('/js/settings-row.js'), import('/js/usage-format.js'), import('/js/mentions.js'), import('/js/quota-settings.js'), import('/js/quota-source.js'), import('/js/message-actions.js')]);
  const freeze = document.createElement('style'); freeze.textContent = '*,*::before,*::after{transition:none!important;animation:none!important}'; document.head.append(freeze);
  document.querySelector('#login-view').hidden = true;
  document.querySelector('#app-view').hidden = false;
  const make = (tag, className, text, parent) => { const node = document.createElement(tag); if (className) node.className = className; if (text) node.textContent = text; if (parent) parent.append(node); return node; };
  const list = document.querySelector('#messages');
  const message = (extra = '', body = '') => {
    const article = make('article', `message ${extra}`.trim(), '', list); article.dataset.fixture = extra || 'plain';
    const meta = make('div', 'message-meta', '', article); make('strong', 'message-author', 'ada', meta); make('time', '', '12:01', meta); make('button', 'row-btn quiet-glyph message-number', '#7', meta);
    const bubble = make('div', 'message-bubble', '', article);
    make('button', 'row-btn quote', '↪ #3 bert', bubble);
    const content = make('div', 'message-body', '', bubble);
    content.append(renderMarkdown(body || '## Title\n\nSome **bold** and *italic* text with `code` and https://example.test and #7 and @ada\n\n> quote\n\n- one\n- two\n\n| A | B |\n| :- | -: |\n| 1 | 2 |\n\n```js\nconst x = 1;\n```\n\n~~gone~~'));
    highlightMentions(content, new MentionIndex([{ slug: 'ada', role_key: 'worker', tool: 'codex', model: 'm' }], 'p'));
    const plan = renderVisualPlan({ visual_plan: [{ type: 'markdown', title: 'Plan', data: { markdown: 'Plan **text**' } }, { type: 'checklist', data: { items: [{ text: 'a', checked: true }, { text: 'b' }] } }, { type: 'file-tree', data: { root: { name: 'src', type: 'dir', children: [{ name: 'a.js', type: 'file', note: 'entry' }] } } }] });
    if (plan) bubble.append(plan);
    bubble.append(buildMessageActions({ setReply() {}, toggleReaction() {}, openThread() {}, threadLabel: () => 'Reply in thread' }, { id: 'fixture', seq: 7, no: 7, body: 'text' }));
    make('button', 'row-btn quiet-glyph chat-msg-debug-toggle', '', meta); meta.lastChild.append(Object.assign(document.createElement('span'), { className: 'icon-mask icon-mask-bug' })); meta.lastChild.setAttribute('aria-label', 'Open debug inspector');
    const pills = make('span', 'reaction-pills', '', bubble); const pill = make('button', 'row-btn reaction-pill', '👍 2', pills); pill.setAttribute('aria-pressed', 'true');
    return article;
  };
  const expiredFixture = message(); const boundaryFixture = message('self'); message('needs-attention');
  // Proposed answers (the real renderer), for the accessibility and contrast audits.
  {
    const { answerControls } = await import('/js/answer-controls.js');
    const asked = message('answer-fixture', 'Should I go ahead with the migration?');
    const controls = answerControls({ proposed_answers: ['Proceed', 'Hold', 'Revise'], free_text: true }, async () => {}, { mention: '@tess' });
    asked.querySelector('.message-bubble').append(controls);
  }
  // Highlighted code and a diff, from the real renderer; the audits wait for the lazy highlight.js load.
  const codeFixture = message('code-fixture', '```go\npackage main\n\n// greet says hello\nfunc greet(name string) string { return \"hi \" + name + \" 42\" }\n```\n\n```diff\ndiff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,3 @@\n package main\n-var x = 1\n+var x = 2\n```\n\n```html\n<div class=\"a\">x</div>\n```');
  for (let i = 0; i < 100 && !codeFixture.querySelector('.hljs-keyword'); i++) await new Promise(resolve => setTimeout(resolve, 50));
  expiredFixture.classList.add('message-expired');
  { const { EXPIRED_HOURGLASS } = await import('/js/usage-expiry.js'); const mark = make('span', 'message-expired-mark', '', expiredFixture.querySelector('.message-meta') || expiredFixture); mark.innerHTML = EXPIRED_HOURGLASS; mark.setAttribute('role', 'img'); mark.setAttribute('aria-label', 'Expired from AI context'); }
  boundaryFixture.classList.add('expiry-boundary'); boundaryFixture.prepend(make('span', 'expiry-label', "Older AI chatter won't reach agents automatically"));
  const system = make('article', 'message system-notice', '', list); const row = make('div', 'system-row', '', system); make('span', 'message-body', 'ada joined', row); make('time', '', '12:00', row);
  const agents = document.querySelector('#agents-panel') || document.querySelector('.agents-panel');
  if (agents) {
    const humans = document.querySelector('#agents-humans') || agents;
    const person = make('article', 'agent-card human-card is-you', '', humans); const personTitle = make('div', 'agent-title', '', person);
    make('span', 'presence-dot', '', personTitle); { const icon = make('img', 'agent-harness-icon', '', personTitle); icon.src = '/icons/human.svg'; icon.alt = ''; } make('strong', 'agent-name human-name', 'ada', personTitle); make('small', 'human-role', 'owner', personTitle);
    // The real member card, opened as a profile (a working agent, so Reactivate is unavailable and says why), plus the facts the server adds.
    const { AgentsPanel } = await import('/js/agents.js');
    const panel = Object.create(AgentsPanel.prototype); panel.source = { available: true }; panel.room = { lifecycle: 'open' }; panel.control = () => {}; panel.openSwitch = () => {}; panel.onPrivate = () => {};
    const agent = { id: 'agt_fixture', name: 'claude-reviewer', slug: 'claude-reviewer', role: 'worker', role_key: 'worker', role_emoji: '🛠', tool: 'codex', model: 'm', desired: 'active', observed: 'in_turn', activity: 'tool inspect', activity_at: new Date(Date.now() - 74000).toISOString(), health_flags: [], recovery_block: false };
    const card = panel.card(agent); card.classList.add('expanded'); agents.append(card); panel.update(card, agent);
    const factsList = make('div', 'profile-facts-list', '', card.querySelector('.agent-profile-facts'));
    for (const [label, value, mono, copy] of [['Status', 'Working'], ['Runtime', 'fixture-model · codex'], ['Session', '3b241101-e2bb-4255-8caf-4136c566a962', true], ['Resume', 'cd /tmp/work && claude --resume 3b241101', true, true], ['CWD', '/tmp/work', true], ['Machine', 'host']]) {
      const row = make('div', 'profile-fact', '', factsList); make('span', 'profile-fact-label', label, row); make(mono ? 'code' : 'strong', 'profile-fact-value', value, row);
      if (copy) { const button = make('button', 'icon-btn profile-copy', '', row); make('span', 'icon-mask icon-mask-copy', '', button).setAttribute('aria-hidden', 'true'); button.title = 'Copy resume'; button.setAttribute('aria-label', 'Copy resume'); }
    }
  }
  // Settings -> Harnesses, from the real renderer: the list beside one harness, once on a harness with its Edit form open and once on a disabled one.
  {
    const { HarnessSettings } = await import('/js/harness-settings.js');
    const { RoleDefaults } = await import('/js/role-defaults.js');
    const { rolePanel } = await import('/js/harness-views.js');
    const settings = Object.create(HarnessSettings.prototype); settings.openLaunchers = new Set(['mln_docker']);
    // The state the constructor sets up (the fixture skips it): the roles with their defaults and the role views.
    settings.prefix = ''; settings.roleViews = new Set(); settings.otherHarnesses = new Map(); settings.drafts = new Map(); settings.selectedHarness = ''; settings.sequence = 0; settings.modelFailures = new Map();
    settings.roleDefaults = new RoleDefaults(async () => { throw new Error('the fixture has no server'); });
    settings.roleDefaults.roles = [
      { id: 'role_lead_default', role_key: 'lead', label: 'Lead', emoji: '👑', default_agents: [], pick_mode: 'in_turn', rev: 0, cardinality: 'singleton' },
      { id: 'role_worker_default', role_key: 'worker', label: 'Worker', emoji: '🛠️', default_agents: [{ launcher_id: 'mln_native', model: 'gpt-6-sol' }, { launcher_id: 'mln_native', model: 'gpt-6-luna' }], pick_mode: 'randomly', rev: 2, cardinality: 'multi' },
      { id: 'role_reviewer_default', role_key: 'reviewer', label: 'Reviewer', emoji: '🔎', default_agents: [{ launcher_id: 'mln_native', model: 'gpt-6-sol' }], pick_mode: 'in_turn', rev: 1, cardinality: 'multi' },
    ];
    const list = document.querySelector('#harnesses-list');
    const base = { tool: 'codex', label: 'Codex', models: [{ slug: 'gpt-6-sol', label: 'GPT-6 Sol', note: 'The frontier model', default: true }, { slug: 'gpt-6-luna', label: 'GPT-6 Luna', note: '', default: false }], enabled: true, available: true, preset: true, version: '1.2.3', active_agents: 2, args: ['--flag'], env_refs: { TOKEN: 'HOME' }, containerized: false, path_maps: [], visible_roots: [], supports_toolbox: true };
    const native = { ...base, id: 'mln_native', variant: 'native', name: 'native', command: 'codex', launcher_path: '/usr/bin/codex' };
    const docker = { ...base, id: 'mln_docker', variant: 'harness-docker', name: 'harness-docker', preset: false, enabled: false, available: false, unavailable_reason: 'docker is not installed', command: 'docker', launcher_path: '/usr/bin/docker' };
    settings.rows = [native, docker];
    settings.selectedHarness = 'mln_native'; list.append(settings.splitView(settings.rows.map(row => settings.harnessCard('mch_fixture', row))));
    settings.selectedHarness = 'mln_docker'; list.append(settings.splitView(settings.rows.map(row => settings.harnessCard('mch_fixture', row))));
    settings.rolesHost = document.createElement('div');
    list.append(rolePanel({ roles: settings.roleDefaults.roles, labelOf: item => `${item.launcher_id} · ${item.model}`, store: settings.roleDefaults, report: () => {}, refresh: () => {} }));
  }
  const sidebar = document.querySelector('.sidebar');
  // The real room list over fake data (two projects, a direct message, a closed and a hidden room), in the layout
  // so the audits see the box, the rows, the chips and the folds.
  {
    const { Sidebar } = await import('/js/sidebar.js');
    const list = new Sidebar(document.querySelector('#projects'), () => {});
    for (const project of [{ id: 'p1', name: 'Alpha project' }, { id: 'p2', name: 'Beta project' }]) list.projects.set(project.id, project);
    const room = (id, project, kind, title, extra = {}) => ({ id, project_id: project, kind, title, lifecycle: 'open', member_count: 3, unread_count: 0, last_activity_at: `2026-10-17T12:0${id.length}:00Z`, last_message_body: 'Last words', ...extra });
    for (const item of [room('r1', 'p1', 'home', 'Alpha home'), room('r2', 'p1', 'task', 'Alpha task one', { parent_id: 'r1', unread_count: 4 }), room('r3', 'p1', 'task', 'Alpha task two', { parent_id: 'r1', attention_count: 1 }),
      room('r4', 'p2', 'home', 'Beta home'), room('r5', 'p2', 'dm', 'ada · bert', { unread_count: 2 }), room('r6', 'p2', 'task', 'Old task', { lifecycle: 'closed', parent_id: 'r4' }), room('r7', 'p2', 'task', 'Quiet task', { hidden: true, parent_id: 'r4' })]) list.rooms.set(item.id, item);
    // The sort menu is open, so the audits see it too.
    list.bindSort(document.querySelector('#rooms-sort')); list.openSortMenu();
    list.active = 'r1'; list.health.set('r3', [{ name: 'xan', flags: [{ code: 'held_answer' }] }]); list.render();
  }
  // The bar that offers browser notifications, shown through the real module (the room header's neighbour).
  { const { NotificationBanner } = await import('/js/notification-banner.js'); new NotificationBanner({ notifications: { available: true, requestPermission: async () => {} }, saveDismissed: () => {} }).show(); }
  // The first-run theme picker, from the real module: each tile paints its own theme, and its label has to read on it in every page theme.
  { const { chooseTheme } = await import('/js/setup.js'); void chooseTheme(document.querySelector('#theme-view'), '', async () => ({})); document.querySelector('#theme-view').hidden = true; }
  const quick = make('div', 'quick-commands', '', document.querySelector('#composer')); quick.dataset.fixture = 'quick'; const option = make('div', 'quick-command', '', quick); make('strong', '', '/archive', option); make('span', '', 'Archive room', option); option.setAttribute('aria-selected', 'true');
  make('div', 'quick-command', '', quick).append(make('strong', '', '/spawn'), make('span', '', 'Open spawn dialog'));
  const macroPopup = make('div', 'quick-commands macro-commands', '', document.querySelector('#composer')); macroPopup.dataset.fixture = 'macros';
  for (const [key, body, selected] of [['<BRAINSTORM-IDEAS>', 'Brainstorm ten ideas for the next...', true], ['<DO-CR>', 'Run the code review on this pull request', false]]) { const row = make('div', 'quick-command', '', macroPopup); row.setAttribute('role', 'option'); row.setAttribute('aria-selected', String(selected)); make('strong', '', key, row); make('span', '', body, row); }
  const section = document.querySelector('#settings-general');
  const select = make('select'); select.append(make('option', '', 'Dark'));
  const check = make('input'); check.type = 'checkbox';
  const area = make('textarea');
  section.append(settingsGroup([settingsRow({ label: 'Fixture select', desc: 'A description for the select', control: select, id: 'fx-select' }), settingsRow({ label: 'Fixture toggle', desc: 'A description for the toggle', control: check, id: 'fx-check', className: 'settings-row-toggle' }), settingsRow({ label: 'Fixture area', desc: 'A stacked row', control: area, id: 'fx-area', className: 'settings-row-stack' })]));
  // The real automatic-switching section over the mock quota API, with its decision list opened, so the audits see every control.
  const quota = new QuotaSettings(new HttpQuotaSource());
  await quota.sync({ owner: true, enabled: true });
  quota.node.querySelectorAll('details').forEach(item => { item.open = true; });
  document.querySelector('#settings-usages').append(quota.node);
  // The limit guard's settings over its mock API, and its rows in the Health list, both rendered by the real code.
  const { LimitGuardSettings } = await import('/js/limit-guard-settings.js'), { HttpLimitGuardSource } = await import('/js/limit-guard-source.js'), { Health } = await import('/js/health.js');
  const guard = new LimitGuardSettings(new HttpLimitGuardSource());
  await guard.sync({ owner: true });
  document.querySelector('#settings-usages').append(guard.node);
  const guardData = await new HttpLimitGuardSource().load(), healthLimits = Health.prototype.limitRows(guardData);
  if (healthLimits) document.querySelector('#health-popover').prepend(healthLimits);
  // The room dialog's member list with every row kind, so the audits see people, agents, badges and singleton roles.
  const { RoomMembers } = await import('/js/room-members.js');
  new RoomMembers(document.querySelector('#room-member-list')).render({
    humans: [{ id: 'u1', login: 'ada', role: 'owner' }, { id: 'u2', login: 'bert', role: 'viewer' }],
    agents: [{ id: 'a1', slug: 'tessa', role_key: 'lead', role_label: 'Lead', role_emoji: '👑', singleton: true, desired: 'active', observed: 'in_turn' }, { id: 'a2', slug: 'drew', role_key: 'worker', role_label: 'Worker', role_emoji: '🛠', desired: 'stopped', observed: 'stopped', role_deleted: true }],
    singletons: [{ role_key: 'lead', label: 'Lead', emoji: '👑', held_by: { agent_id: 'a1', slug: 'tessa', in_this_room: true } }, { role_key: 'scribe', label: 'Scribe', emoji: '', held_by: null }],
  });
  // The usages column, rendered by the real popover over the mock snapshots (open, so the audits see it).
  const { UsagesPopover } = await import('/js/usages-popover.js'), { MockUsagesSource } = await import('/js/usages-source.js');
  const mockUsages = new MockUsagesSource(), usagesColumn = new UsagesPopover(mockUsages, () => {});
  usagesColumn.state = await mockUsages.switcher(); usagesColumn.tokens = (await mockUsages.credentialTargets()).targets;
  usagesColumn.popover.hidden = false; usagesColumn.chat.classList.add('usages-open'); usagesColumn.render(await mockUsages.list());
  // The import wizard's blocking notice (a missed number reservation: only the skip is offered), rendered by the real wizard.
  const { ImportV1Wizard } = await import('/js/import-v1.js');
  const blocked = new ImportV1Wizard(); blocked.dialog.id = 'import-v1-blocked';
  blocked.preview = { reservation_missed: true, counts: {} }; blocked.mapping = {}; blocked.phase = 'review'; blocked.render();
  // The ":" suggester (a list of rows, one selected) and the emoji picker, open and loaded, from the real modules.
  {
    const popup = make('div', 'quick-commands emoji-commands', '', document.querySelector('#composer')); popup.dataset.fixture = 'emoji-suggest'; popup.setAttribute('role', 'listbox');
    for (const [glyph, before, hit, after, name, selected] of [['🤷', ':', 'shru', 'g:', 'shrug', true], ['🫨', ':', 'shru', 'gging_face:', 'shaking face', false]]) {
      const row = make('div', 'quick-command', '', popup); row.setAttribute('role', 'option'); row.setAttribute('aria-selected', String(selected));
      const key = make('strong', '', '', row); make('i', 'emoji-glyph', glyph, key).setAttribute('aria-hidden', 'true'); const code = make('i', 'emoji-code', before, key); make('mark', '', hit, code); code.append(after); make('span', '', name, row);
    }
    const { openEmojiPicker } = await import('/js/emoji-picker.js');
    openEmojiPicker({ anchor: document.querySelector('#emoji-button'), onPick() {} });
    for (let i = 0; i < 100 && !document.querySelector('.emoji-picker .emoji-cell'); i++) await new Promise(resolve => setTimeout(resolve, 50));
    document.querySelector('.emoji-picker .emoji-cell').dispatchEvent(new Event('mouseenter'));
  }
};


async function withPages(viewports, run, options = {}) {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: chromiumPath(), headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'] });
  try {
    for (const [name, viewport] of viewports) {
      const page = await browser.newPage();
      if (options.touch) await page.emulate({ viewport: { ...viewport, isMobile: true, hasTouch: true, deviceScaleFactor: 2 }, userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148' });
      else await page.setViewport(viewport);
      page.on('pageerror', () => {});
      await page.goto(origin, { waitUntil: 'load' });
      await page.evaluate(FIXTURE);
      await run(name, page);
      await page.close();
    }
  } finally { await browser.close(); server.close(); }
}

const THEMES = ['dark', 'light', 'red-dark', 'red-light', 'blue-dark', 'blue-light', 'green-dark', 'green-light', 'high-contrast-dark', 'high-contrast-light'];
const VIEWPORTS = [['desktop', { width: 1100, height: 720 }], ['phone', { width: 390, height: 780 }]];
module.exports = { withPages, THEMES, VIEWPORTS };

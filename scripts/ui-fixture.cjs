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
  if (req.method === 'GET' && url.pathname === '/api/machines') { res.writeHead(200, { 'Content-Type': 'application/json' }); res.end(JSON.stringify({ machines: [{ id: 'mch_fixture', name: 'Fixture host', approved: true, last_seen: new Date().toISOString() }] })); return; }
  if (req.method === 'GET' && url.pathname === '/api/machines/mch_fixture/launchers') {
    const base = { tool: 'codex', label: 'Codex', enabled: true, preset: true, args: ['--flag'], env_refs: { TOKEN: 'HOME' }, containerized: false, path_maps: [], visible_roots: [], supports_toolbox: true, machine_id: 'mch_fixture', agent_server_url: '' };
    const models = [{ slug: 'gpt-6-sol', label: 'GPT-6 Sol', note: 'The frontier model', default: true }, { slug: 'gpt-6-luna', label: 'GPT-6 Luna', note: '', default: false }];
    const rows = [
      { config: { ...base, id: 'mln_native', name: 'native', command: 'codex' }, resolved_command: '/usr/bin/codex', version: '1.2.3', available: true, active_agents: 2, models, models_rev: 'r1' },
      { config: { ...base, id: 'mln_docker', name: 'harness-docker', command: 'docker', preset: false, enabled: false }, resolved_command: '/usr/bin/docker', version: '1.2.3', available: false, unavailable_reason: 'docker is not installed', active_agents: 2, models, models_rev: 'r1' },
    ];
    res.writeHead(200, { 'Content-Type': 'application/json' }); res.end(JSON.stringify({ machine_id: 'mch_fixture', launchers: rows, activity: [] })); return;
  }
  if (req.method === 'GET' && url.pathname === '/api/roles') {
    const roles = [
      { id: 'role_leader_default', role_key: 'leader', label: 'Leader', emoji: '👑', default_agents: [], pick_mode: 'in_turn', rev: 0, cardinality: 'singleton' },
      { id: 'role_worker_default', role_key: 'worker', label: 'Worker', emoji: '🛠️', default_agents: [{ launcher_id: 'mln_native', model: 'gpt-6-sol' }, { launcher_id: 'mln_native', model: 'gpt-6-luna' }], pick_mode: 'randomly', rev: 2, cardinality: 'multi' },
      { id: 'role_reviewer_default', role_key: 'reviewer', label: 'Reviewer', emoji: '🔎', default_agents: [{ launcher_id: 'mln_native', model: 'gpt-6-sol' }], pick_mode: 'in_turn', rev: 1, cardinality: 'multi' },
    ];
    res.writeHead(200, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(roles)); return;
  }
  if (url.pathname.startsWith('/api/')) { res.writeHead(401, { 'Content-Type': 'application/json' }); res.end('{"error":"unauthorized"}'); return; }
  let file = path.join(root, url.pathname === '/' ? 'index.html' : url.pathname);
  if (!file.startsWith(root) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { 'Content-Type': types[path.extname(file)] || 'application/octet-stream' }); res.end(fs.readFileSync(file));
});


// Elements are forced visible while measured so hidden popovers and dialogs report real styles.
const FIXTURE = async () => {
  const [{ renderMarkdown }, { renderVisualPlan }, { SettingsRow, SettingsGroup }, { MentionIndex, highlightMentions }, { HttpQuotaSource }, { drawMessageActions, wakeMessageActions, messageActionsProps }] = await Promise.all([
    import('/js/markdown.js'), import('/js/visual-plan.js'), import('/js/components/settings-row.js'), import('/js/mentions.js'), import('/js/quota-source.js'), import('/js/message-actions.js')]);
  const freeze = document.createElement('style'); freeze.textContent = '*,*::before,*::after{transition:none!important;animation:none!important}'; document.head.append(freeze);
  document.querySelector('#login-view').hidden = true;
  document.querySelector('#app-view').hidden = false;
  const make = (tag, className, text, parent) => { const node = document.createElement(tag); if (className) node.className = className; if (text) node.textContent = text; if (parent) parent.append(node); return node; };
  const list = document.querySelector('#messages');
  const message = (extra = '', body = '') => {
    const article = make('article', `chat-msg ${extra}`.trim(), '', list); article.dataset.fixture = extra || 'plain';
    const meta = make('div', 'chat-msg-header', '', article); make('strong', 'chat-msg-from-name', 'ada', meta); make('time', '', '12:01', meta); make('button', 'row-btn quiet-glyph chat-msg-id', '#7', meta);
    const bubble = make('div', 'chat-msg-bubble', '', article);
    make('button', 'row-btn quote', '↪ #3 bert', bubble);
    const content = make('div', 'chat-msg-body', '', bubble);
    content.append(renderMarkdown(body || '## Title\n\nSome **bold** and *italic* text with `code` and https://example.test and #7 and @ada\n\n> quote\n\n- one\n- two\n\n| A | B |\n| :- | -: |\n| 1 | 2 |\n\n```js\nconst x = 1;\n```\n\n~~gone~~'));
    highlightMentions(content, new MentionIndex([{ slug: 'ada', role_key: 'worker', tool: 'codex', model: 'm' }], 'p'));
    const plan = renderVisualPlan({ visual_plan: [{ type: 'markdown', title: 'Plan', data: { markdown: 'Plan **text**' } }, { type: 'checklist', data: { items: [{ text: 'a', checked: true }, { text: 'b' }] } }, { type: 'file-tree', data: { root: { name: 'src', type: 'dir', children: [{ name: 'a.js', type: 'file', note: 'entry' }] } } }] });
    if (plan) bubble.append(plan);
    {
      const chat = { setReply() {}, toggleReaction() {}, openThread() {}, threadLabel: () => 'Reply in thread' }, message = { id: 'fixture', seq: 7, no: 7, body: 'text' }, props = messageActionsProps(message);
      const bar = make(props.tag, props.class, '', bubble); bar.setAttribute('role', props.role); bar.setAttribute('aria-label', props['aria-label']);
      drawMessageActions(bar, { chat, message }); wakeMessageActions(chat, bar, message);
    }
    make('button', 'row-btn quiet-glyph chat-msg-debug-toggle', '', meta); meta.lastChild.append(Object.assign(document.createElement('span'), { className: 'icon-mask icon-mask-bug' })); meta.lastChild.setAttribute('aria-label', 'Open debug inspector');
    const pills = make('span', 'reaction-pills', '', bubble); const pill = make('button', 'row-btn reaction-pill', '👍 2', pills); pill.setAttribute('aria-pressed', 'true');
    return article;
  };
  const expiredFixture = message(); const boundaryFixture = message('self'); message('needs-attention');
  // The three handoff outcome badges, as the chat row draws them, for the contrast audit in every theme.
  { const meta = message('handoff-outcome-fixture').querySelector('.chat-msg-header'); for (const [outcome, glyph, word] of [['done', '✓', 'Done'], ['partial', '◐', 'Partial'], ['failed', '✕', 'Failed']]) { const badge = make('span', `handoff-outcome handoff-outcome-${outcome}`, '', meta); make('span', '', glyph, badge).setAttribute('aria-hidden', 'true'); badge.append(` ${word}`); } }
  // Proposed answers (the real renderer), for the accessibility and contrast audits.
  {
    const answerControls = await (async () => { const { html, render } = await import('/js/ui.js'), { AnswerControls } = await import('/js/components/answer-controls.js'); return (item, submit, options = {}) => { const host = document.createElement('div'); render(html`<${AnswerControls} item=${item} submit=${submit} ...${options} />`, host); return host.firstElementChild; }; })();
    const asked = message('answer-fixture', 'Should I go ahead with the migration?');
    const controls = answerControls({ proposed_answers: ['Proceed', 'Hold', 'Revise'], free_text: true }, async () => {}, { mention: '@tess' });
    controls.classList.add('answer-row'); asked.after(controls); // a row of the list under the message's card, as in the chat
  }
  // Highlighted code and a diff, from the real renderer; the audits wait for the lazy highlight.js load.
  const codeFixture = message('code-fixture', '```go\npackage main\n\n// greet says hello\nfunc greet(name string) string { return \"hi \" + name + \" 42\" }\n```\n\n```diff\ndiff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,3 @@\n package main\n-var x = 1\n+var x = 2\n```\n\n```html\n<div class=\"a\">x</div>\n```');
  for (let i = 0; i < 100 && !codeFixture.querySelector('.hljs-keyword'); i++) await new Promise(resolve => setTimeout(resolve, 50));
  expiredFixture.classList.add('chat-msg-expired');
  { const { EXPIRED_HOURGLASS } = await import('/js/usage-expiry.js'); const mark = make('span', 'chat-msg-expired-icon', '', expiredFixture.querySelector('.chat-msg-header') || expiredFixture); mark.innerHTML = EXPIRED_HOURGLASS; mark.setAttribute('role', 'img'); mark.setAttribute('aria-label', 'Expired from AI context'); }
  boundaryFixture.classList.add('expiry-boundary'); boundaryFixture.prepend(make('span', 'expiry-label', "Older AI chatter won't reach agents automatically"));
  const system = make('article', 'chat-msg chat-msg-system', '', list); const row = make('div', 'chat-msg-system-row', '', system); make('span', 'chat-msg-body', 'ada joined', row); make('time', '', '12:00', row);
  const agents = document.querySelector('#agents-panel') || document.querySelector('.agents-panel');
  if (agents) {
    const humans = document.querySelector('#agents-humans') || agents;
    const person = make('article', 'agent-card agent-card-compact human-card is-you', '', humans); const personTitle = make('div', 'agent-id agent-title', '', person);
    make('span', 'agent-online-dot active', '', personTitle); { const icon = make('img', 'harness-icon agent-harness-icon', '', personTitle); icon.src = '/icons/human.svg'; icon.alt = ''; } make('span', 'agent-id-text agent-name human-name', 'ada', personTitle); make('small', 'human-role', 'owner', personTitle);
    // The real member card, opened as a profile (a working agent, so Reactivate is unavailable and says why), plus the facts the server adds.
    const { html, render } = await import('/js/ui.js');
    const { AgentCard } = await import('/js/pages/agents/agents-list.js');
    const api = { store: { now: { value: Date.now() }, toggleProfile() {}, control() {}, setOutputOpen() {}, loadOutput() {} }, onPrivate() {}, onRecovery() {}, openSwitch() {}, openLogs() {} };
    const agent = { id: 'agt_fixture', name: 'claude-reviewer', slug: 'claude-reviewer', role: 'worker', role_key: 'worker', role_emoji: '🛠', tool: 'codex', model: 'm', desired: 'active', observed: 'in_turn', activity: 'tool inspect', activity_at: new Date(Date.now() - 74000).toISOString(), health_flags: [], recovery_block: false };
    const profile = { native_session_id: '3b241101-e2bb-4255-8caf-4136c566a962', resume_command: 'cd /tmp/work && claude --resume 3b241101', cwd: '/tmp/work', machine_name: 'host' };
    const slot = document.createElement('div'); slot.style.display = 'contents'; agents.append(slot);
    render(html`<${AgentCard} agent=${agent} expanded=${true} profile=${profile} output=${null} readOutput=${true} down="" lifecycle="open" api=${api} />`, slot);
    // The agent log popup with a read drawn and a highlight, from the real dialog over a store that holds one answer.
    const { LogDialog } = await import('/js/pages/agents/log-dialog.js'), { signal } = await import('/js/ui.js');
    const logs = { state: signal({ agentID: agent.id, lines: 200, follow: true, text: 'Error: first\nworking\nerror again\n', status: 'Earlier output is not shown.', loading: false }), setLines() {}, setFollow() {}, read() {} };
    render(html`<${LogDialog} store=${logs} host=${{ agentName: () => 'claude-reviewer', agentSlug: () => 'claude-reviewer', closeLogs() {}, store: { now: signal(Date.now()), agents: signal(new Map([[agent.id, { ...agent, last_activity_at: new Date(Date.now() - 192000).toISOString(), turn_started_at: new Date(Date.now() - 2460000).toISOString(), health_flags: [{ code: 'stuck' }], last_exit_evidence: 'Process exited with code 17.' }]])) } }} />`, document.querySelector('#agent-log-dialog'));
  }
  // The Settings dialog is a chunk the app fetches on first use, and nothing opens it here: make it, so its navigation and sections are in the page for the audits.
  {
    const { SettingsDialog } = await import('/js/settings-dialog.js');
    new SettingsDialog({ source: {}, projectContext: () => ({ projects: [], rooms: [], selectedProjectID: '' }), notifications: { available: false } });
  }
  // Settings -> Harnesses & models, from the real page over the fixture server's answers: the list beside one harness, with the Edit form open on a disabled one.
  {
    const { HarnessSettings } = await import('/js/pages/harnesses/page.js');
    document.querySelector('#settings-harnesses')?.remove(); document.querySelector('#settings-harnesses-tab')?.remove();
    const settings = new HarnessSettings(document.querySelector('#settings-dialog'));
    await settings.open();
    settings.section.querySelector('.harness-master-item:last-child')?.click();
    await new Promise(resolve => setTimeout(resolve, 100));
    document.querySelector('#harnesses-list .machine-harness:not([hidden]) .machine-harness-edit')?.click();
  }
  // Settings -> Roles from the real page over the fixture server's roles, with the editor open on the Worker role.
  {
    const { RoleSettings } = await import('/js/pages/roles/page.js');
    const { render } = await import('/js/ui.js');
    render(null, document.querySelector('#settings-roles')); // the app's own copy, already mounted there
    const roles = new RoleSettings(document.querySelector('#settings-roles'));
    roles.open();
    await new Promise(resolve => setTimeout(resolve, 300));
    document.querySelector('#roles-cards [data-role-key="worker"] .role-card-edit')?.click();
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  const sidebar = document.querySelector('.sidebar');
  // The real room list over fake data (two projects, a direct message, a closed and a hidden room), in the layout
  // so the audits see the box, the rows, the chips and the folds.
  {
    const { Sidebar } = await import('/js/pages/sidebar/page.js');
    const list = new Sidebar(document.querySelector('#projects'), () => {});
    for (const project of [{ id: 'p1', name: 'Alpha project' }, { id: 'p2', name: 'Beta project' }]) list.projects.set(project.id, project);
    const room = (id, project, kind, title, extra = {}) => ({ id, project_id: project, kind, title, lifecycle: 'open', member_count: 3, unread_count: 0, last_activity_at: `2026-10-17T12:0${id.length}:00Z`, last_message_body: 'Last words', ...extra });
    for (const item of [room('r1', 'p1', 'home', 'Alpha home'), room('r2', 'p1', 'task', 'Alpha task one', { parent_id: 'r1', unread_count: 4 }), room('r3', 'p1', 'task', 'Alpha task two', { parent_id: 'r1', attention_count: 1 }),
      room('r4', 'p2', 'home', 'Beta home'), room('r5', 'p2', 'dm', 'ada · bert', { unread_count: 2 }), room('r6', 'p2', 'task', 'Old task', { lifecycle: 'closed', parent_id: 'r4' }), room('r7', 'p2', 'task', 'Quiet task', { hidden: true, parent_id: 'r4' })]) list.rooms.set(item.id, item);
    // The sort menu is open, so the audits see it too.
    list.bindSort(document.querySelector('#rooms-sort')); list.openSortMenu();
    list.setActive('r1'); list.health.set('r3', [{ name: 'xan', flags: [{ code: 'held_answer' }] }]); list.render();
    await new Promise(resolve => setTimeout(resolve, 50)); // Preact draws the list on the next tick
  }
  // The bar that offers browser notifications, shown through the real module (the room header's neighbour).
  { const { NotificationBanner } = await import('/js/pages/banners/page.js'); new NotificationBanner({ notifications: { available: true, requestPermission: async () => {} }, saveDismissed: () => {} }).show(); }
  // The first-run theme picker, from the real module: each tile paints its own theme, and its label has to read on it in every page theme.
  { const { chooseTheme } = await import('/js/pages/setup/page.js'); void chooseTheme(document.querySelector('#theme-view'), '', async () => ({})); document.querySelector('#theme-view').hidden = true; }
  const quick = make('div', 'quick-commands', '', document.querySelector('#composer')); quick.dataset.fixture = 'quick'; const option = make('div', 'quick-command', '', quick); make('strong', '', '/archive', option); make('span', '', 'Archive room', option); option.setAttribute('aria-selected', 'true');
  make('div', 'quick-command', '', quick).append(make('strong', '', '/spawn'), make('span', '', 'Open spawn dialog'));
  const macroPopup = make('div', 'quick-commands macro-commands', '', document.querySelector('#composer')); macroPopup.dataset.fixture = 'macros';
  for (const [key, body, selected] of [['<BRAINSTORM-IDEAS>', 'Brainstorm ten ideas for the next...', true], ['<DO-CR>', 'Run the code review on this pull request', false]]) { const row = make('div', 'quick-command', '', macroPopup); row.setAttribute('role', 'option'); row.setAttribute('aria-selected', String(selected)); make('strong', '', key, row); make('span', '', body, row); }
  const section = document.querySelector('#settings-general');
  const { html: fixtureHtml, render: fixtureRender } = await import('/js/ui.js');
  const fixtureRows = make('div', '', '', section);
  fixtureRender(fixtureHtml`<${SettingsGroup}>
    <${SettingsRow} label="Fixture select" desc="A description for the select" id="fx-select"><select><option>Dark</option></select><//>
    <${SettingsRow} label="Fixture toggle" desc="A description for the toggle" id="fx-check" className="settings-row-toggle"><input type="checkbox" /><//>
    <${SettingsRow} label="Fixture area" desc="A stacked row" id="fx-area" className="settings-row-stack"><textarea></textarea><//>
  <//>`, fixtureRows);
  // The real automatic-switching and limit-guard blocks over the mock APIs, with the decision list opened, so the audits see every control.
  const { QuotaStore } = await import('/js/pages/usages/quota-store.js'), { QuotaBlock } = await import('/js/pages/usages/quota-block.js');
  const { GuardStore } = await import('/js/pages/usages/guard-store.js'), { GuardBlock } = await import('/js/pages/usages/guard-block.js');
  const { HttpLimitGuardSource } = await import('/js/limit-guard-source.js'), { LimitRows } = await import('/js/pages/health/health-popover.js'), { html, render } = await import('/js/ui.js');
  const quotaHost = make('div', '', '', document.querySelector('#settings-usages')), guardHost = make('div', '', '', document.querySelector('#settings-usages'));
  const quota = new QuotaStore({ source: new HttpQuotaSource(), isEditing: () => false }), guard = new GuardStore({ source: new HttpLimitGuardSource(), isEditing: () => false });
  render(html`<${QuotaBlock} store=${quota} />`, quotaHost); render(html`<${GuardBlock} store=${guard} />`, guardHost);
  await quota.sync({ owner: true, enabled: true }); await guard.sync({ owner: true });
  await new Promise(resolve => setTimeout(resolve, 150));
  quotaHost.querySelectorAll('details').forEach(item => { item.open = true; });
  const guardData = await new HttpLimitGuardSource().load();
  render(html`<${LimitRows} guard=${guardData} />`, document.querySelector('#health-popover'));
  // The room dialog's member list with every row kind, so the audits see people, agents, badges and singleton roles.
  const { RoomMembers } = await import('/js/pages/members/page.js');
  new RoomMembers(document.querySelector('#room-member-list')).render({
    humans: [{ id: 'u1', login: 'ada', role: 'owner' }, { id: 'u2', login: 'bert', role: 'viewer' }],
    agents: [{ id: 'a1', slug: 'tessa', role_key: 'leader', role_label: 'Leader', role_emoji: '👑', singleton: true, desired: 'active', observed: 'in_turn' }, { id: 'a2', slug: 'drew', role_key: 'worker', role_label: 'Worker', role_emoji: '🛠', desired: 'stopped', observed: 'stopped', role_deleted: true }],
    singletons: [{ role_key: 'leader', label: 'Leader', emoji: '👑', held_by: { agent_id: 'a1', slug: 'tessa', in_this_room: true } }, { role_key: 'scribe', label: 'Scribe', emoji: '', held_by: null }],
  });
  await new Promise(resolve => setTimeout(resolve, 50)); // Preact draws the list on the next tick
  // The usages column, rendered by the real popover over the mock snapshots (open, so the audits see it).
  const { UsagesPopover } = await import('/js/usages-popover.js'), { MockUsagesSource } = await import('/js/usages-source.js');
  const mockUsages = new MockUsagesSource(), usagesColumn = new UsagesPopover(mockUsages, () => {});
  usagesColumn.popover.hidden = false; usagesColumn.chat.classList.add('usages-open'); usagesColumn.toggle.setAttribute('aria-expanded', 'true');
  await usagesColumn.load();
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
      // The app boots on its own after the load event: it asks for the session, gets a 401 from this server, and shows the sign-in view. The fixture hides that view and shows
      // the app's, so it must come after, or the boot (late on a busy machine) puts the sign-in back above the app and moves everything measured down by its height.
      await page.waitForFunction(() => document.querySelector('#boot-view').hidden && !document.querySelector('#login-view').hidden, { timeout: 60000 });
      await page.evaluate(FIXTURE);
      await run(name, page);
      await page.close();
    }
  } finally { await browser.close(); server.close(); }
}

const THEMES = ['dark', 'light', 'red-dark', 'red-light', 'blue-dark', 'blue-light', 'green-dark', 'green-light', 'high-contrast-dark', 'high-contrast-light'];
const VIEWPORTS = [['desktop', { width: 1100, height: 720 }], ['phone', { width: 390, height: 780 }]];
module.exports = { withPages, THEMES, VIEWPORTS };

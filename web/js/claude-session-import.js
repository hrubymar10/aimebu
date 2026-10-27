// Bulk import of Claude Code sessions (the home room's settings → Project): pick any number of the sessions a machine has for the project's folder, give each
// a role, and each becomes an agent of its own in a room of its own. Every agent works on a copy of its session (the originals stay as they
// are) and starts idle, waiting for a message. The agents are started one by one through the ordinary spawn request, so the project's
// live-agent and spawn-rate limits, the one-writer-per-folder rule and the owner's approval for folders outside the project all apply as
// they do for a single spawn; what a limit leaves unstarted is said, and can be imported again.
import { request } from './api.js';
import { setGlyph } from './icon-button.js';
import { sessionFacts, sessionFlags, listingStatus } from './claude-sessions.js';

function node(tag, className = '', text = '') { const item = document.createElement(tag); if (className) item.className = className; if (text) item.textContent = text; return item; }

// The role each listed session starts with: the first session of a folder works there (the one writer a folder has), the others review, so
// the first import does not run into the writer rule. `roles` are role keys in the order offered.
export function presetRoles(sessions, roles) {
  const writer = roles.includes('worker') ? 'worker' : roles[0] || '';
  const other = roles.includes('reviewer') ? 'reviewer' : writer;
  const seen = new Set();
  return sessions.map(session => {
    const folder = session.cwd || '';
    if (seen.has(folder)) return other;
    seen.add(folder);
    return writer;
  });
}

export class ClaudeSessionImport {
  constructor(source) {
    this.source = source;
    this.dialog = document.querySelector('#cc-import-dialog');
    this.machine = document.querySelector('#cc-import-machine');
    this.harness = document.querySelector('#cc-import-harness');
    this.model = document.querySelector('#cc-import-model');
    this.list = document.querySelector('#cc-import-list');
    this.status = document.querySelector('#cc-import-status');
    this.error = document.querySelector('#cc-import-error');
    this.run = setGlyph(document.querySelector('#cc-import-run'), 'play', 'Import the selected sessions as agents');
    this.all = setGlyph(document.querySelector('#cc-import-all'), 'select-all', 'Select all or none');
    this.closeButton = document.querySelector('#cc-import-close');
    this.closeButton.onclick = () => this.dialog.close();
    // The running import writes into this dialog and uses its project, room and machine; closing it half way (and opening it for another
    // project) could mix the two, so it stays until the import is done.
    this.dialog.addEventListener('cancel', event => { if (this.running) event.preventDefault(); });
    this.dialog.addEventListener('close', () => { if (this.running) this.dialog.showModal(); });
    this.machine.onchange = () => this.loadCatalog();
    this.harness.onchange = () => { this.fillModels(); void this.loadSessions(); };
    this.run.onclick = () => this.start();
    this.all.onclick = () => { const boxes = [...this.list.querySelectorAll('input[type=checkbox]:not(:disabled)')]; const on = boxes.some(box => !box.checked); for (const box of boxes) box.checked = on; this.updateRun(); };
    this.generation = 0;
    this.running = false;
  }
  // open({ projectID, homeRoomID }): the room only anchors the spawn requests; each agent gets a room of its own.
  async open({ projectID, homeRoomID }) {
    if (this.running) return; // an import in progress keeps this dialog, with its project, room and machine, until it is done
    this.projectID = projectID; this.homeRoomID = homeRoomID;
    this.error.textContent = ''; this.status.textContent = 'Loading…'; this.list.replaceChildren(); this.updateRun();
    for (const select of [this.machine, this.harness, this.model]) select.replaceChildren();
    this.dialog.showModal();
    const generation = ++this.generation;
    try {
      const [machines, roles] = await Promise.all([request(`/api/machines?project_id=${encodeURIComponent(projectID)}`), this.source.roles()]);
      if (generation !== this.generation || !this.dialog.open) return;
      this.roles = (roles || []).filter(role => !role.cap_is_lead);
      const usable = (machines.machines || []).filter(machine => machine.approved && !machine.revoked_at);
      for (const machine of usable) {
        const option = node('option', '', `${machine.name}${machine.is_host ? ' · Host' : ''}${machine.connected ? '' : ' · Offline'}`);
        option.value = machine.id; option.disabled = !machine.connected; this.machine.append(option);
      }
      const first = usable.find(machine => machine.connected && machine.is_host) || usable.find(machine => machine.connected);
      if (!first) { this.status.textContent = 'No connected machine to import from.'; return; }
      this.machine.value = first.id;
      await this.loadCatalog();
    } catch (error) {
      if (generation !== this.generation || !this.dialog.open) return;
      this.status.textContent = ''; this.error.textContent = error.message || 'Could not load the machines.';
    }
  }
  // Only a native claude harness, or a Docker one that shares the host's Claude home, can continue (or copy) a session; any other container
  // keeps a Claude home of its own.
  async loadCatalog() {
    const generation = ++this.generation;
    this.harness.replaceChildren(); this.model.replaceChildren(); this.list.replaceChildren(); this.updateRun();
    this.status.textContent = 'Loading…'; this.error.textContent = '';
    try {
      const catalog = await this.source.catalog(this.machine.value);
      if (generation !== this.generation || !this.dialog.open) return;
      this.tools = (catalog.tools || []).filter(tool => tool.tool === 'claude' && tool.available && (!tool.containerized || tool.shares_claude_home));
      this.tools.forEach((tool, index) => { const option = node('option', '', tool.label || 'Claude Code'); option.value = String(index); this.harness.append(option); });
      if (!this.tools.length) { this.status.textContent = 'This machine has no Claude Code harness that can continue a session (a native one, or a Docker one that shares the host Claude home). Add one under Settings → Harnesses.'; return; }
      this.fillModels();
      await this.loadSessions();
    } catch (error) {
      if (generation !== this.generation || !this.dialog.open) return;
      this.status.textContent = ''; this.error.textContent = error.message || 'Could not load the harnesses.';
    }
  }
  get tool() { return this.tools?.[Number(this.harness.value)]; }
  fillModels() {
    this.model.replaceChildren();
    for (const model of this.tool?.models || []) { const option = node('option', '', model.label || model.id); option.value = model.id; option.selected = !!model.default; this.model.append(option); }
  }
  async loadSessions() {
    const generation = ++this.generation;
    this.list.replaceChildren(); this.updateRun();
    this.status.textContent = 'Looking for sessions…'; this.error.textContent = '';
    try {
      const result = await this.source.claudeSessions(this.projectID, { machineID: this.machine.value, launcherID: this.tool?.launcher_id || '' });
      if (generation !== this.generation || !this.dialog.open) return;
      const sessions = result.sessions || [];
      this.sessions = sessions;
      this.status.textContent = listingStatus(sessions.length, 'There are no Claude Code sessions for this project\'s folder on this machine.', result);
      const roles = presetRoles(sessions, this.roles.map(role => role.role_key));
      sessions.forEach((session, index) => this.list.append(this.row(session, roles[index])));
      this.updateRun();
    } catch (error) {
      if (generation !== this.generation || !this.dialog.open) return;
      this.status.textContent = ''; this.error.textContent = error.message || 'Could not list the sessions.';
    }
  }
  row(session, role) {
    const row = node('div', 'cc-session cc-import-row'); row.dataset.sessionId = session.id;
    const box = node('input'); box.type = 'checkbox'; box.setAttribute('aria-label', `Import ${session.title || 'untitled session'}`); box.onchange = () => this.updateRun();
    const main = node('div', 'cc-session-main');
    main.append(node('span', 'cc-session-title', session.title || 'Untitled session'), node('span', 'cc-session-facts', sessionFacts(session).join(' · ')));
    if (session.outside_folder) main.append(node('span', 'cc-session-cwd', session.cwd));
    const flags = node('span', 'cc-session-flags');
    for (const [text, kind] of sessionFlags(session)) flags.append(node('span', `cc-session-flag cc-session-flag-${kind}`, text));
    if (session.outside_folder) flags.append(node('span', 'cc-session-flag cc-session-flag-warn', 'needs approval'));
    const choice = node('select', 'cc-import-role'); choice.setAttribute('aria-label', 'Role');
    for (const item of this.roles) { const option = node('option', '', item.label || item.role_key); option.value = item.role_key; choice.append(option); }
    choice.value = role;
    const state = node('span', 'cc-import-state'); state.setAttribute('role', 'status');
    row.append(box, main, flags, choice, state);
    return row;
  }
  updateRun() { this.run.disabled = this.running || !this.list.querySelector('input[type=checkbox]:checked:not(:disabled)'); }
  // One agent per ticked session, in the order listed. A limit stops the rest (they can be imported again later); any other refusal is
  // shown on its row and the next one goes on.
  async start() {
    // Everything the loop uses is taken once, here, not read from the dialog while it runs.
    const tool = this.tool, model = this.model.value, projectRoom = this.homeRoomID, machineID = this.machine.value;
    const picked = [...this.list.querySelectorAll('.cc-import-row')].filter(row => row.querySelector('input').checked && !row.querySelector('input').disabled)
      .map(row => ({ row, session: row.dataset.sessionId, role: row.querySelector('select').value }));
    if (!tool || !model || !picked.length) return;
    const rows = picked.map(item => item.row);
    this.running = true; this.updateRun(); this.error.textContent = '';
    const locked = [...this.list.querySelectorAll('select, input')].filter(control => !control.disabled);
    for (const control of [...locked, this.machine, this.harness, this.model, this.closeButton]) control.disabled = true;
    let started = 0, stopped = false;
    for (const { row, session, role } of picked) {
      const state = row.querySelector('.cc-import-state');
      if (stopped) { state.textContent = 'Not started: limit reached'; continue; }
      state.textContent = 'Starting…';
      try {
        const agent = await this.source.spawn(projectRoom, { role, tool: tool.tool, variant: tool.variant, launcher_id: tool.launcher_id, model,
          provider: tool.provider, machine_id: machineID, resume_session: session, resume_mode: 'fork', new_room: true });
        started += 1; state.textContent = `Started as ${agent.slug || agent.name}`; state.classList.remove('error');
        row.querySelector('input').checked = false; row.dataset.started = '1';
      } catch (error) {
        state.classList.add('error');
        if (error.code === 'spawn_limit') { stopped = true; state.textContent = 'Not started: limit reached'; } else state.textContent = error.message || 'Could not start';
      }
    }
    for (const control of [...locked, this.machine, this.harness, this.model, this.closeButton]) control.disabled = false;
    for (const row of rows) if (row.dataset.started) row.querySelector('input').disabled = true; // a started session is not offered again
    this.running = false; this.updateRun();
    this.status.textContent = `${started} of ${rows.length} ${rows.length === 1 ? 'session' : 'sessions'} started${stopped ? '; the project\'s agent or spawn-rate limit was reached, import the rest later' : ''}.`;
  }
}

import { batch, signal } from '../../ui.js';
import { request } from '../../api.js';
import { listingStatus } from '../../claude-sessions.js';
import { defaultModel, finalStatus, machineLabel, presetRoles, usableTool } from './model.js';

export class ImportStore {
  constructor(source, dialog) {
    this.source = source;
    this.dialog = dialog;
    this.machines = signal([]);
    this.machine = signal('');
    this.tools = signal([]);
    this.harness = signal('');
    this.model = signal('');
    this.sessions = signal([]);
    this.roles = signal([]);
    this.presets = signal([]);
    this.ticked = signal(new Set());
    this.started = signal(new Set());
    this.states = signal(new Map());
    this.status = signal('');
    this.error = signal('');
    this.running = signal(false);
    this.alive = true;
    this.generation = 0;
  }

  get tool() { return this.tools.peek()[Number(this.harness.peek())]; }

  fresh(generation) { return this.alive && generation === this.generation && this.dialog.open; }

  // The room only anchors the spawn requests; each agent gets a room of its own.
  async open({ projectID, homeRoomID }) {
    if (!this.alive || this.running.peek()) return;
    this.projectID = projectID; this.homeRoomID = homeRoomID;
    batch(() => {
      this.error.value = ''; this.status.value = 'Loading…'; this.machines.value = []; this.machine.value = ''; this.tools.value = []; this.harness.value = ''; this.model.value = ''; this.sessions.value = [];
      this.ticked.value = new Set(); this.started.value = new Set(); this.states.value = new Map();
    });
    this.dialog.showModal();
    const generation = ++this.generation;
    try {
      const [machines, roles] = await Promise.all([request(`/api/machines?project_id=${encodeURIComponent(projectID)}`), this.source.roles()]);
      if (!this.fresh(generation)) return;
      this.roles.value = (roles || []).filter(role => !role.cap_is_lead);
      const usable = (machines.machines || []).filter(machine => machine.approved && !machine.revoked_at);
      this.machines.value = usable.map(machine => ({ id: machine.id, label: machineLabel(machine), disabled: !machine.connected }));
      const first = usable.find(machine => machine.connected && machine.is_host) || usable.find(machine => machine.connected);
      if (!first) { this.status.value = 'No connected machine to import from.'; return; }
      this.machine.value = first.id;
      await this.loadCatalog();
    } catch (failure) {
      if (!this.fresh(generation)) return;
      batch(() => { this.status.value = ''; this.error.value = failure.message || 'Could not load the machines.'; });
    }
  }

  selectMachine(id) { this.machine.value = id; return this.loadCatalog(); }

  selectHarness(index) { batch(() => { this.harness.value = index; this.model.value = defaultModel(this.tool); }); return this.loadSessions(); }

  async loadCatalog() {
    if (!this.alive) return;
    const generation = ++this.generation;
    batch(() => { this.tools.value = []; this.harness.value = ''; this.model.value = ''; this.sessions.value = []; this.ticked.value = new Set(); this.status.value = 'Loading…'; this.error.value = ''; });
    try {
      const catalog = await this.source.catalog(this.machine.peek());
      if (!this.fresh(generation)) return;
      const tools = (catalog.tools || []).filter(usableTool);
      batch(() => { this.tools.value = tools; this.harness.value = tools.length ? '0' : ''; this.model.value = defaultModel(tools[0]); });
      if (!tools.length) { this.status.value = 'This machine has no Claude Code harness that can continue a session (a native one, or a Docker one that shares the host Claude home). Add one under Settings → Harnesses & models.'; return; }
      await this.loadSessions();
    } catch (failure) {
      if (!this.fresh(generation)) return;
      batch(() => { this.status.value = ''; this.error.value = failure.message || 'Could not load the harnesses.'; });
    }
  }

  async loadSessions() {
    if (!this.alive) return;
    const generation = ++this.generation;
    batch(() => { this.sessions.value = []; this.ticked.value = new Set(); this.status.value = 'Looking for sessions…'; this.error.value = ''; });
    try {
      const result = await this.source.claudeSessions(this.projectID, { machineID: this.machine.peek(), launcherID: this.tool?.launcher_id || '' });
      if (!this.fresh(generation)) return;
      const sessions = result.sessions || [];
      batch(() => {
        this.sessions.value = sessions;
        this.presets.value = presetRoles(sessions, this.roles.peek().map(role => role.role_key));
        this.status.value = listingStatus(sessions.length, 'There are no Claude Code sessions for this project\'s folder on this machine.', result);
      });
    } catch (failure) {
      if (!this.fresh(generation)) return;
      batch(() => { this.status.value = ''; this.error.value = failure.message || 'Could not list the sessions.'; });
    }
  }

  tick(id, on) { const next = new Set(this.ticked.peek()); if (on) next.add(id); else next.delete(id); this.ticked.value = next; }

  startable() { return this.sessions.peek().filter(session => !this.started.peek().has(session.id)); }

  selectAll() {
    if (this.running.peek()) return;
    const rows = this.startable(), on = rows.some(session => !this.ticked.peek().has(session.id));
    this.ticked.value = on ? new Set([...this.ticked.peek(), ...rows.map(session => session.id)]) : new Set([...this.ticked.peek()].filter(id => !rows.some(session => session.id === id)));
  }

  canRun() { return !this.running.value && this.startable().some(session => this.ticked.value.has(session.id)); }

  // A limit stops the rest; any other refusal is shown on its row and the next one goes on. `roles` are what the role boxes hold at the start.
  async start(roles) {
    const tool = this.tool, model = this.model.peek(), projectRoom = this.homeRoomID, machineID = this.machine.peek();
    const picked = this.startable().filter(session => this.ticked.peek().has(session.id)).map(session => ({ session: session.id, role: roles[session.id] }));
    if (!this.alive || !tool || !model || !picked.length || this.running.peek()) return;
    batch(() => { this.running.value = true; this.error.value = ''; });
    const say = (id, text, error = false) => { if (this.alive) this.states.value = new Map(this.states.peek()).set(id, { text, error }); };
    let started = 0, stopped = false;
    for (const { session, role } of picked) {
      if (stopped) { say(session, 'Not started: limit reached', true); continue; }
      if (!this.alive) break;
      say(session, 'Starting…');
      try {
        const agent = await this.source.spawn(projectRoom, { role, tool: tool.tool, variant: tool.variant, launcher_id: tool.launcher_id, model,
          provider: tool.provider, machine_id: machineID, resume_session: session, resume_mode: 'fork', new_room: true });
        started += 1; say(session, `Started as ${agent.slug || agent.name}`);
        if (this.alive) batch(() => { this.tick(session, false); this.started.value = new Set(this.started.peek()).add(session); });
      } catch (failure) {
        if (failure.code === 'spawn_limit') { stopped = true; say(session, 'Not started: limit reached', true); } else say(session, failure.message || 'Could not start', true);
      }
    }
    if (!this.alive) return;
    batch(() => { this.running.value = false; this.status.value = finalStatus(started, picked.length, stopped); });
  }

  destroy() { this.alive = false; this.generation++; }
}

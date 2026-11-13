import { request } from './api.js';
import { uuid } from './uuid.js';

export class MockAgentSource {
  constructor(seed = false) { this.seed = seed; this.available = seed; this.agents = new Map(); this.listeners = new Set(); this.seeded = false; this.switchTimers = new Map(); }
  async list(roomID) {
    if (this.seed && !this.seeded) {
      this.seeded = true;
      this.agents.set('agt_mock_worker', { id: 'agt_mock_worker', room_id: roomID, name: 'Ada', slug: 'ada', role: 'worker', role_key: 'worker', tool: 'codex', model: 'fake-model', desired: 'active', observed: 'in_turn', activity: 'tool inspect', activity_at: new Date(Date.now() - 12000).toISOString(), recovery_block: false, health_flags: [{ code: 'stuck' }],
        latest_usage: { tokens_in: 5000, tokens_out: 1200, context_used: 150000, context_window: 200000, cost_so_far: 0.35 } });
      this.agents.set('agt_mock_reviewer', { id: 'agt_mock_reviewer', room_id: roomID, name: 'Bert', slug: 'bert', role: 'reviewer', role_key: 'reviewer', tool: 'fake', model: 'fake-model', desired: 'active', observed: 'idle', activity: 'Waiting', recovery_block: true, recovery_inbox_id: 'mock_approval', health_flags: [{ code: 'recovery_waiting' }, ...(new URLSearchParams(location.search).has('mock_held') ? [{ code: 'held_answer' }] : [])], last_exit_evidence: 'Process exited with code 17.\nCheckout is not mounted.' });
    }
    return [...this.agents.values()].filter(agent => agent.room_id === roomID).map(agent => ({ ...agent }));
  }
  async output() { throw Object.assign(new Error('no output has been captured for this agent'), { status: 404, code: 'no_output' }); }
  async catalog() { return { tools: [
    { tool: 'fake', variant: 'test', label: 'Fake', cli_version: 'fixture-1', models: [{ id: 'test', label: 'Test' }, { id: 'fake-reviewer', label: 'Fake reviewer' }], available: true },
    { tool: 'claude', variant: 'native', label: 'Claude Code', cli_version: '', models: [{ id: 'haiku', label: 'Haiku' }], available: false, unavailable_reason: 'not offered by a connected machine' },
    { tool: 'codex', variant: 'native', label: 'Codex', cli_version: 'mock-1', models: [{ id: 'gpt-6-luna', label: 'GPT-6 Luna' }], available: true },
  ] }; }
  async roles() { return [{ id: 'role_worker', role_key: 'worker', label: 'Worker' }, { id: 'role_reviewer', role_key: 'reviewer', label: 'Reviewer' }, { id: 'role_leader', role_key: 'leader', label: 'Leader', cap_is_lead: true }]; }
  // The mock projects keep today's behaviour: every role may be spawned into the home room.
  async homeLeadOnly() { return false; }
  async teamFlow() { return null; }
  // The mock's pool of names, like the server's: the next free one, in a fixed order so a test can tell two asks apart.
  takenNames() { return new Set([...this.agents.values()].flatMap(agent => [agent.slug, agent.name].filter(Boolean).map(name => name.toLowerCase()))); }
  async suggestName(roomID, exclude = '') {
    const taken = this.takenNames(); if (exclude) taken.add(exclude.toLowerCase());
    const free = ['nova-one', 'mira', 'orbit', 'quill', 'tango', 'ember'].filter(name => !taken.has(name));
    this.nameAsks = (this.nameAsks || 0) + 1;
    return free.length ? free[(this.nameAsks - 1) % free.length] : `agent${taken.size + 1}`;
  }
  async spawn(roomID, params) {
    if (!this.available) throw new Error('Agent controls are not available yet');
    if (params.name && this.takenNames().has(params.name)) {
      throw Object.assign(new Error('this project already has an agent, person or role with that name'), { status: 409, code: 'name_in_use' });
    }
    const id = `agt_mock_${uuid()}`;
    const agent = { id, room_id: roomID, name: params.name || `Agent ${this.agents.size + 1}`, ...(params.name ? { slug: params.name } : {}), role: params.role, tool: params.tool, model: params.model, desired: 'active', observed: 'idle', activity: 'Waiting', recovery_block: false };
    this.agents.set(id, agent); this.notify(); return { ...agent };
  }
  async switch(id, params) {
    const agent = this.agents.get(id); if (!agent) throw new Error('Agent not found');
    agent.switch = { state: agent.observed === 'in_turn' ? 'draining' : 'starting', handoff_status: agent.observed === 'in_turn' ? 'waiting' : 'ready', ...params };
    agent.desired_rev = (agent.desired_rev || 0) + 1;
    this.notify();
    this.switchTimers.set(id, setTimeout(() => {
      this.switchTimers.delete(id);
      Object.assign(agent, { tool: params.tool, variant: params.variant, model: params.model, generation: (agent.generation || 0) + 1, observed: 'idle' });
      agent.latest_usage = agent.latest_usage?.cost_so_far == null ? undefined : { cost_so_far: agent.latest_usage.cost_so_far };
      delete agent.switch;
      this.notify();
    }, 1000));
    return { ...agent };
  }
  async switchPreview(id, params) { return { room_id: params.room_id, handoff_status: 'available', history_window: 40, handbook_included: this.agents.get(id)?.role_key === 'leader', can_cancel: true, can_retry: true, can_continue: true, target: params }; }
  async switchProgress(id) { const agent = this.agents.get(id); return { switch: agent?.switch || null, desired_rev: agent?.desired_rev || 0 }; }
  async switchAction(id, action) { const agent = this.agents.get(id); if (!agent) throw new Error('Agent not found'); if (action === 'cancel') { clearTimeout(this.switchTimers.get(id)); this.switchTimers.delete(id); delete agent.switch; } else if (agent.switch) agent.switch.handoff_status = action === 'retry-handoff' ? 'pending' : 'automatic'; this.notify(); return { ...agent }; }
  async remove(id) {
    if (!this.agents.has(id)) throw new Error('Agent not found');
    this.agents.delete(id); this.notify();
  }
  async control(id, action) {
    if (!this.available) throw new Error('Agent controls are not available yet');
    const agent = this.agents.get(id); if (!agent) throw new Error('Agent not found');
    if (action === 'stop') { agent.desired = 'stopped'; agent.observed = agent.observed === 'in_turn' ? 'interrupting' : 'stopped'; agent.activity = agent.observed === 'interrupting' ? 'Waiting for current work to stop' : 'Stopped'; }
    else if (action === 'pause') { agent.desired = 'not_accepting'; }
    else if (action === 'reactivate') { agent.desired = 'active'; if (agent.observed === 'stopped' || agent.observed === 'interrupting') agent.observed = 'idle'; agent.activity = 'Waiting'; }
    this.notify(); return { ...agent };
  }
  onEvent(event) {
    const agent = this.agents.get(event.agent_id); if (!agent) return;
    if (event.kind === 'agent_activity') { let payload = {}; try { payload = JSON.parse(event.payload_json); } catch {} agent.activity = payload.summary || agent.activity; if (payload.observed) agent.observed = payload.observed; this.notify(); }
  }
  subscribe(listener) { this.listeners.add(listener); return () => this.listeners.delete(listener); }
  notify() { for (const listener of this.listeners) listener(); }
}

export class HttpAgentSource {
  constructor() { this.available = true; this.agents = new Map(); this.activity = new Map(); this.listeners = new Set(); }
  normalize(agent) { const activity = this.activity.get(agent.id); return { ...agent, name: agent.slug, role: agent.role_key, activity: activity?.summary || '', activity_at: activity?.at || agent.last_activity_at || '' }; }
  catalog(machineID) { return request('/api/spawn-catalog' + (machineID ? `?machine_id=${encodeURIComponent(machineID)}` : '')); }
  roles() { return request('/api/roles'); }
  teamFlow(projectID) { return request(`/api/projects/${encodeURIComponent(projectID)}/team-flow`); }
  // Whether the project's home room takes only lead roles (Home room: lead only, on unless the project turned it off).
  async homeLeadOnly(projectID) { return (await request(`/api/projects/${encodeURIComponent(projectID)}/settings`)).home_lead_only !== false; }
  // The end of what an agent's harness wrote to stderr, for a project's owner or an admin (the server refuses anyone else).
  async output(roomID, agentID, lines = 200) {
    return request(`/api/rooms/${encodeURIComponent(roomID)}/agents/${encodeURIComponent(agentID)}/output?lines=${encodeURIComponent(lines)}`);
  }
  async list(roomID) {
    try {
      const cards = await request(`/api/rooms/${roomID}/agents`);
      this.available = true;
      const current = cards.map(card => {
        const prior = this.agents.get(card.id);
        // An older room read can finish after a control response. Keep the
        // newer revision so Stop/Reactivate cannot visually run backward.
        const older = prior && ((card.generation ?? 0) < (prior.generation ?? 0)
          || (card.desired_rev ?? 0) < (prior.desired_rev ?? 0));
        const accepted = older ? { ...prior, room_id: card.room_id } : card;
        this.agents.set(card.id, accepted);
        return accepted;
      });
      return current.map(card => this.normalize(card));
    } catch (error) {
      if (error.status === 404) { this.available = false; return []; }
      throw error;
    }
  }
  // The Claude Code sessions of the project's folder on a machine, or one session by id wherever it worked.
  claudeSessions(projectID, { machineID = '', launcherID = '', sessionID = '' } = {}) {
    const query = new URLSearchParams();
    if (machineID) query.set('machine_id', machineID);
    if (launcherID) query.set('launcher_id', launcherID);
    if (sessionID) query.set('session_id', sessionID);
    const text = query.toString();
    return request(`/api/projects/${encodeURIComponent(projectID)}/claude-sessions${text ? `?${text}` : ''}`);
  }
  // A free name for the next agent of the room's project (nothing is reserved; the spawn decides if it is still free).
  async suggestName(roomID, exclude = '') { return (await request(`/api/rooms/${roomID}/agent-name${exclude ? `?exclude=${encodeURIComponent(exclude)}` : ''}`)).name; }
  async spawn(roomID, params) {
    const card = await request(`/api/rooms/${roomID}/agents`, {
      method: 'POST', headers: { 'Idempotency-Key': uuid() },
      body: { role_key: params.role, tool: params.tool, variant: params.variant, model: params.model, ...(params.name ? { name: params.name } : {}), ...(params.launcher_id ? { launcher_id: params.launcher_id } : {}), ...(params.provider ? { provider: params.provider } : {}), ...(params.machine_id ? { machine_id: params.machine_id } : {}), ...(params.resume_legacy ? { resume_legacy: params.resume_legacy, ...(params.resume_mode ? { resume_mode: params.resume_mode } : {}), confirm_v1_idle: !!params.confirm_v1_idle } : {}), ...(params.resume_session ? { resume_session: params.resume_session, ...(params.resume_mode ? { resume_mode: params.resume_mode } : {}), confirm_session_closed: !!params.confirm_session_closed, ...(params.new_room ? { new_room: true } : {}) } : {}),
        ...(params.work_on_copy ? { work_on_copy: true } : {}),
        // The dialog's worktree choice reaches the server; it used to be dropped here, so the checkbox did nothing.
        ...(params.worktree ? { worktree: true, ...(params.base_ref ? { base_ref: params.base_ref } : {}) } : {}) },
    });
    this.agents.set(card.id, card); await this.changed(); return this.normalize(card);
  }
  async switch(id, params) {
    const current = this.agents.get(id);
    if (!current) throw new Error('Agent not found');
    const card = await request(`/api/agents/${id}/switch`, {
      method: 'POST', headers: { 'Idempotency-Key': uuid() },
      body: { room_id: params.room_id, tool: params.tool, variant: params.variant, ...(params.launcher_id ? { launcher_id: params.launcher_id } : {}), model: params.model, machine_id: params.machine_id, rev: current.desired_rev },
    });
    this.agents.set(card.id, card); await this.changed(); return this.normalize(card);
  }
  async switchPreview(id, params) {
    const current = this.agents.get(id);
    if (!current) throw new Error('Agent not found');
    return request(`/api/agents/${encodeURIComponent(id)}/switch/preview`, { method: 'POST', body: { ...params, rev: current.desired_rev } });
  }
  async switchProgress(id) {
    const progress = await request(`/api/agents/${encodeURIComponent(id)}/switch`);
    const current = this.agents.get(id);
    if (current && progress.desired_rev >= current.desired_rev && JSON.stringify(current.switch || null) !== JSON.stringify(progress.switch || null)) {
      this.agents.set(id, { ...current, switch: progress.switch, desired_rev: progress.desired_rev });
      await this.changed();
    }
    return progress;
  }
  async switchAction(id, action) {
    const current = this.agents.get(id);
    if (!current) throw new Error('Agent not found');
    const card = await request(`/api/agents/${encodeURIComponent(id)}/switch/${action}`, { method: 'POST', headers: { 'Idempotency-Key': uuid() }, body: { rev: current.desired_rev } });
    this.agents.set(card.id, card); await this.changed(); return this.normalize(card);
  }
  // The server answers for the whole removal (the agent leaves its rooms, its worktree goes); the room's list is read again afterwards.
  async remove(id) {
    await request(`/api/agents/${id}`, { method: 'DELETE', body: {} });
    this.agents.delete(id); await this.changed();
  }
  async control(id, action) {
    const current = this.agents.get(id);
    if (!current) throw new Error('Agent not found');
    const desired = { stop: 'stopped', pause: 'not_accepting', reactivate: 'active', 'start-fresh': 'active' }[action];
    const card = await request(`/api/agents/${id}/control`, { method: 'POST', body: { desired, rev: current.desired_rev, ...(action === 'start-fresh' ? { start_fresh: true } : {}) } });
    this.agents.set(id, card); await this.changed(); return this.normalize(card);
  }
  async onEvent(event) {
    if (event.agent_id) {
      let payload = {}; try { payload = JSON.parse(event.payload_json); } catch {}
      const labels = { turn_recorded: 'Queued', turn_dispatch_requested: 'Starting', answer_committed: 'Replied', answer_held: 'Reply held', turn_process_exit: 'Finished', turn_finished: 'Finished', turn_lost: 'Needs recovery', turn_never_started: 'Never started' };
      const label = labels[event.kind] || (event.kind.startsWith('turn_ack_') ? 'Working' : '');
      const previous = this.activity.get(event.agent_id);
      if (payload.summary) this.activity.set(event.agent_id, { summary: payload.summary, turnID: event.turn_id, kind: event.kind, at: event.created_at });
      else if (label && !((event.kind === 'turn_process_exit' || event.kind === 'turn_finished') && previous?.turnID === event.turn_id && previous.kind === 'answer_committed')) {
        const when = new Date(event.created_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
        this.activity.set(event.agent_id, { summary: `${label} ${when}`, turnID: event.turn_id, kind: event.kind, at: event.created_at });
      }
      if (event.kind.startsWith('agent_') || event.kind.startsWith('turn_') || event.kind.startsWith('answer_') || event.kind.startsWith('supervisor_') || event.kind.startsWith('provider_') || event.kind === 'activity' || event.kind === 'warm_process_ended') await this.changed(event);
    }
  }
  subscribe(listener) { this.listeners.add(listener); return () => this.listeners.delete(listener); }
  // `event` is the stream event that changed an agent (the listeners decide whether it concerns what they show); none for a change made here.
  changed(event) { return Promise.all([...this.listeners].map(listener => listener(event))); }
}

export function createAgentSource() { return new URLSearchParams(location.search).has('mock_agents') ? new MockAgentSource(true) : new HttpAgentSource(); }

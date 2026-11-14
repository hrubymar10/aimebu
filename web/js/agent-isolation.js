import { request } from './api.js';

// "Only run agents in docker": an instance setting. It is read from the server for any signed-in person and written by an admin.
export const agentIsolation = {
  dockerOnly: false,
  canEdit: false,
  warmIdleMinutes: 5, // how long agents are kept warm after a turn (5 until the owner saves another, 0 is off); only the owner changes it
  canEditWarm: false,
  codexWarmEnabled: false, // experimental opt-in; absent on an older server means off
  keepAlive: { enabled: true, claude_minutes: 50, codex_minutes: 10, stop_after_hours: 24 }, // the cache keep-alive: pings that keep an idle agent's prompt cache
  listeners: new Set(),
  async load() {
    try { this.apply(await request('/api/agent-isolation')); } catch {}
    return this;
  },
  apply(body) {
    this.dockerOnly = !!body?.docker_only;
    this.canEdit = !!body?.can_edit;
    if (typeof body?.warm_idle_minutes === 'number' && Number.isFinite(body.warm_idle_minutes)) this.warmIdleMinutes = body.warm_idle_minutes; // a server that says none leaves the default
    this.canEditWarm = !!body?.can_edit_warm;
    this.codexWarmEnabled = !!body?.codex_warm_enabled;
    if (body?.keep_alive) this.keepAlive = { ...this.keepAlive, ...body.keep_alive };
    for (const listener of this.listeners) listener(this);
  },
  async save(dockerOnly) {
    const body = await request('/api/agent-isolation', { method: 'PUT', body: { docker_only: !!dockerOnly } });
    this.apply(body);
    return body;
  },
  async saveKeepAlive(change) {
    const body = await request('/api/agent-isolation', { method: 'PUT', body: { keep_alive: change } });
    this.apply(body);
    return body;
  },
  async saveWarm(minutes) {
    const body = await request('/api/agent-isolation', { method: 'PUT', body: { warm_idle_minutes: Number(minutes) } });
    this.apply(body);
    return body;
  },
  async saveCodexWarm(enabled) {
    const body = await request('/api/agent-isolation', { method: 'PUT', body: { codex_warm_enabled: !!enabled } });
    this.apply(body);
    return body;
  },
  onChange(listener) { this.listeners.add(listener); return () => this.listeners.delete(listener); },
};

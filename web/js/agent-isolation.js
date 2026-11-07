import { request } from './api.js';

// "Only run agents in docker": an instance setting. It is read from the server for any signed-in person and written by an admin.
export const agentIsolation = {
  dockerOnly: false,
  canEdit: false,
  listeners: new Set(),
  async load() {
    try { this.apply(await request('/api/agent-isolation')); } catch {}
    return this;
  },
  apply(body) {
    this.dockerOnly = !!body?.docker_only;
    this.canEdit = !!body?.can_edit;
    for (const listener of this.listeners) listener(this);
  },
  async save(dockerOnly) {
    const body = await request('/api/agent-isolation', { method: 'PUT', body: { docker_only: !!dockerOnly } });
    this.apply(body);
    return body;
  },
  onChange(listener) { this.listeners.add(listener); return () => this.listeners.delete(listener); },
};

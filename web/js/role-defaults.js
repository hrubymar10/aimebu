import { request } from './api.js';
import { roleBody, withModel, movedByKey } from './role-defaults-logic.js';

// Fired on `document` whenever a role's default agents were saved, from the Harnesses page or from Settings → Roles, so the other view reloads.
export const ROLE_DEFAULTS_CHANGED = 'role-defaults-change';

// The global roles and their default agents, read and saved through the same /api/roles the Roles settings use: two views of one list.
export class RoleDefaults {
  constructor(send = request) { this.send = send; this.roles = []; this.queues = new Map(); }
  async load() { this.roles = await this.send('/api/roles'); return this.roles; }
  // Changes one role: `change(current)` answers the fields to change given the role as it is when this save's turn comes. The saves of one
  // role go out one after another, each on the revision and the list the one before left, so quick successive changes (a tick, a move, a
  // pick mode) all land instead of the later ones failing on a stale revision.
  update(id, change) {
    const run = async () => {
      const current = this.roles.find(role => role.id === id);
      if (!current) throw new Error('That role no longer exists');
      try {
        const saved = await this.send(`/api/roles/${encodeURIComponent(id)}`, { method: 'PUT', body: roleBody(current, change(current)) });
        this.roles = this.roles.map(role => role.id === saved.id ? { ...role, ...saved } : role);
        document.dispatchEvent(new CustomEvent(ROLE_DEFAULTS_CHANGED, { detail: { source: this, roleID: saved.id } }));
        return saved;
      } catch (error) { await this.load().catch(() => {}); throw error; } // a revision changed elsewhere: show what is stored now
    };
    const next = (this.queues.get(id) || Promise.resolve()).catch(() => {}).then(run);
    this.queues.set(id, next);
    next.catch(() => {}).then(() => { if (this.queues.get(id) === next) this.queues.delete(id); });
    return next;
  }
  setModel(role, launcherID, model, on) { return this.update(role.id, current => ({ default_agents: withModel(current.default_agents, launcherID, model, on) })); }
  // An entry is named by its launcher and model (see movedByKey), so a queued save acts on the entry the person meant.
  moveEntry(role, launcherID, model, by) { return this.update(role.id, current => ({ default_agents: movedByKey(current.default_agents, launcherID, model, by) })); }
  removeEntry(role, launcherID, model) { return this.setModel(role, launcherID, model, false); }
  setMode(role, mode) { return this.update(role.id, () => ({ pick_mode: mode })); }
}

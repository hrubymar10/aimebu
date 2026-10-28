import { batch, signal } from '../../ui.js';
import { request, rights } from '../../api.js';
import { chosenProject, inviteHint } from './model.js';

export class UsersStore {
  constructor(projectContext) {
    this.projectContext = projectContext;
    // The nav's Users tab holds its place invisibly until the access check answers, so the tabs below it never move under a pointer.
    this.tab = signal('pending');
    this.error = signal('');
    this.allowed = signal([]);
    this.project = signal('');
    this.role = signal('viewer');
    this.members = signal(null);
    this.accounts = signal(null);
    this.link = signal(null);
    this.globalOwner = false;
    this.globalAdmin = false;
    this.alive = true;
    this.accessGeneration = 0;
    this.loadGeneration = 0;
    this.access = null;
  }

  // Settings runs this on open and the section runs it again: the older caller adopts the newer answer, so the lists are read once.
  prepare() {
    if (!this.alive) return Promise.resolve(null);
    const generation = ++this.accessGeneration;
    return this.access = this.#prepare(generation);
  }

  async #prepare(generation) {
    const projects = this.projectContext().projects, current = () => this.alive && generation === this.accessGeneration;
    let allowed = [], globalOwner = false, globalAdmin = false;
    try {
      if (!rights.admin) throw Object.assign(new Error('not an admin'), { status: 403 });
      const result = await request('/api/users');
      if (!current()) return this.#latest();
      globalOwner = result.global_owner; globalAdmin = result.global_admin;
      allowed = result.projects || projects;
    } catch {
      if (!current()) return this.#latest();
      globalOwner = false; globalAdmin = false;
      // Only a project's owner may list its people: ask for those projects alone, not a 403 for each of the others.
      for (const project of projects.filter(item => item.role === 'owner')) {
        if (!current()) return this.#latest();
        try { await request(`/api/users?project_id=${encodeURIComponent(project.id)}`); if (!current()) return this.#latest(); allowed.push(project); } catch { if (!current()) return this.#latest(); }
      }
    }
    if (!current()) return this.#latest();
    this.globalOwner = globalOwner; this.globalAdmin = globalAdmin;
    batch(() => { this.allowed.value = allowed; this.project.value = chosenProject(allowed, this.projectContext().selectedProjectID); });
    return { hidden: !allowed.length && !globalAdmin };
  }

  #latest() { return this.alive ? this.access : Promise.resolve(null); }

  async load() {
    if (!this.alive) return;
    const generation = ++this.loadGeneration, project = this.project.peek();
    this.error.value = '';
    try {
      let members = null, accounts = this.accounts.peek();
      if (project) members = (await request(`/api/users?project_id=${encodeURIComponent(project)}`)).users;
      if (!this.alive || generation !== this.loadGeneration) return;
      if (this.globalAdmin) accounts = (await request('/api/users')).users;
      if (!this.alive || generation !== this.loadGeneration) return;
      batch(() => { this.members.value = members ?? []; this.accounts.value = accounts; });
    } catch (failure) { if (this.alive && generation === this.loadGeneration) this.error.value = failure.message; }
  }

  select(project) { this.project.value = project; return this.load(); }

  async write(path, options, { undo = () => {}, then = () => this.load() } = {}) {
    if (!this.alive) return;
    try { const result = await request(path, options); if (!this.alive) return; await then(result); }
    catch (failure) { if (!this.alive) return; undo(); this.error.value = failure.message; }
  }

  setRole(user, role, undo) { return this.write(`/api/projects/${this.project.peek()}/members/${user.id}`, { method: 'PUT', body: { role } }, { undo }); }
  removeMember(user) { return this.write(`/api/projects/${this.project.peek()}/members/${user.id}`, { method: 'DELETE' }); }
  setAdmin(user, admin, undo) { return this.write(`/api/users/${user.id}/admin`, { method: 'PUT', body: { admin } }, { undo }); }
  deactivate(user) { return this.write(`/api/users/${user.id}/deactivate`, { method: 'POST' }); }
  resetLink(user) { return this.write('/api/invites', { method: 'POST', body: { kind: 'reset', target_user_id: user.id } }, { then: result => this.showLink(result.url) }); }

  createInvite() {
    this.error.value = '';
    return this.write('/api/invites', { method: 'POST', body: { kind: 'join', project_id: this.project.peek(), role: this.role.peek() } }, { then: result => this.showLink(result.url) });
  }

  showLink(url) { this.link.value = { url, hint: inviteHint(url) }; }

  destroy() { this.alive = false; this.accessGeneration++; this.loadGeneration++; }
}

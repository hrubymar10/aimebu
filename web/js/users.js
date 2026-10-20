import { request, currentUser, rights } from './api.js';
import { iconButton } from './icon-button.js';
import { settingsRow, settingsGroup } from './settings-row.js';
import { confirmDialog } from './dialogs.js';

function node(tag, text = '') {
  const element = document.createElement(tag);
  element.textContent = text;
  return element;
}

export class UsersSettings {
  constructor(dialog, projectContext) {
    this.projectContext = projectContext;
    this.tab = node('button', 'Users'); this.tab.type = 'button'; this.tab.id = 'settings-users-tab'; this.tab.className = 'row-btn';
    // Whether this tab exists depends on an access check that returns after Settings opens.
    // Until it does the tab holds its place invisibly, so the tabs below it never move under
    // a pointer that is already on its way to them.
    this.tab.classList.add('settings-tab-pending'); this.tab.setAttribute('aria-hidden', 'true'); this.tab.tabIndex = -1;
    dialog.querySelector('nav').append(this.tab);
    this.section = node('section'); this.section.id = 'settings-users'; this.section.className = 'settings-content'; this.section.hidden = true;
    this.section.append(node('h3', 'People and project access'));
    this.error = node('p'); this.error.className = 'error'; this.error.role = 'alert'; this.section.append(this.error);
    this.projectSelect = node('select');
    this.section.append(settingsGroup([settingsRow({ label: 'Project', desc: 'The project whose members and invitations are shown below.', control: this.projectSelect, id: 'users-project' })], 'Project selection'));
    this.projectSelect.onchange = () => this.load();
    this.inviteForm = node('form'); this.inviteForm.className = 'user-invite-form';
    this.roleSelect = node('select');
    for (const role of ['viewer', 'operator', 'owner']) { const option = node('option', role); option.value = role; this.roleSelect.append(option); }
    const create = node('button', 'Create invite'); create.type = 'submit'; create.className = 'btn btn-primary';
    const actions = node('div'); actions.className = 'settings-actions'; actions.append(create);
    this.inviteForm.append(settingsGroup([settingsRow({ label: 'Invite as', desc: 'Access the invited person gets: viewer reads, operator can also run agents, owner manages the project and its people. Creates a one-time invitation link.', control: this.roleSelect, id: 'users-invite-role' })], 'Invitation'), actions); this.section.append(this.inviteForm);
    this.inviteForm.onsubmit = event => { event.preventDefault(); this.createInvite(); };
    this.link = node('input'); this.link.readOnly = true; this.link.hidden = true;
    this.link.setAttribute('aria-label', 'Invitation link'); this.section.append(this.link);
    this.linkHint = node('p', ''); this.linkHint.className = 'settings-intro invite-link-hint'; this.linkHint.hidden = true; this.section.append(this.linkHint);
    this.members = node('div'); this.members.className = 'user-members'; this.section.append(this.members);
    this.accounts = node('div'); this.accounts.className = 'user-accounts'; this.section.append(this.accounts);
    dialog.append(this.section);
  }
  async prepare() {
    const projects = this.projectContext().projects;
    this.allowed = [];
    try {
      if (!rights.admin) throw Object.assign(new Error('not an admin'), { status: 403 });
      const result = await request('/api/users');
      this.globalOwner = result.global_owner; this.globalAdmin = result.global_admin;
      this.allUsers = result.users;
      // The owner and an admin manage every project, whether or not they belong to it.
      this.allowed = result.projects || projects;
    } catch {
      this.globalOwner = false; this.globalAdmin = false;
      // Only a project's owner may list its people: ask for those projects alone, instead of getting a 403 for each of the others.
      for (const project of projects.filter(item => item.role === 'owner')) {
        try { await request(`/api/users?project_id=${encodeURIComponent(project.id)}`); this.allowed.push(project); }
        catch {}
      }
    }
    this.tab.hidden = !this.allowed.length && !this.globalAdmin;
    this.settle();
    this.projectSelect.replaceChildren();
    for (const project of this.allowed) { const option = node('option', project.name); option.value = project.id; this.projectSelect.append(option); }
    const selected = this.projectContext().selectedProjectID;
    if (this.allowed.some(project => project.id === selected)) this.projectSelect.value = selected;
  }
  // The access check has answered (or failed): show the tab for real, or take it away.
  settle(hide = this.tab.hidden) {
    this.tab.hidden = hide;
    this.tab.classList.remove('settings-tab-pending'); this.tab.removeAttribute('aria-hidden'); this.tab.removeAttribute('tabindex');
  }
  async open() { await this.prepare(); if (!this.tab.hidden) await this.load(); }
  async load() {
    this.error.textContent = '';
    const projectID = this.projectSelect.value;
    try {
      // The accounts belong to the owner and the admins, whether or not they are in any project; the members need a project.
      if (projectID) this.renderMembers((await request(`/api/users?project_id=${encodeURIComponent(projectID)}`)).users, projectID);
      else this.members.replaceChildren();
      if (this.globalAdmin) { this.allUsers = (await request('/api/users')).users; this.renderAccounts(); }
    } catch (error) { this.error.textContent = error.message; }
  }
  renderMembers(users, projectID) {
    this.members.replaceChildren(node('h4', 'Project members'));
    for (const user of users) {
      const row = node('div'); row.className = 'user-row';
      row.append(node('strong', user.login), node('span', user.disabled ? 'Deactivated' : ''));
      const role = node('select'); role.setAttribute('aria-label', `Role for ${user.login}`);
      for (const value of ['viewer', 'operator', 'owner']) { const option = node('option', value); option.value = value; role.append(option); }
      role.value = user.role;
      role.onchange = async () => {
        try { await request(`/api/projects/${projectID}/members/${user.id}`, { method: 'PUT', body: { role: role.value } }); await this.load(); }
        catch (error) { role.value = user.role; this.error.textContent = error.message; }
      };
      const remove = iconButton('close', `Remove ${user.login} from this project`, { danger: true });
      remove.onclick = async () => {
        if (!await confirmDialog(`Remove ${user.login} from this project?`, { title: 'Remove member', confirm: 'Remove', danger: true })) return;
        try { await request(`/api/projects/${projectID}/members/${user.id}`, { method: 'DELETE' }); await this.load(); }
        catch (error) { this.error.textContent = error.message; }
      };
      row.append(role, remove); this.members.append(row);
    }
  }
  // Every account, with who it is (Owner, Admin) and what the signed-in person may do to it: the owner can reset or deactivate anyone and
  // appoint admins; an admin only acts on ordinary accounts (the server enforces the same rule, the buttons just do not offer the rest).
  renderAccounts() {
    this.accounts.replaceChildren(node('h4', 'Accounts'));
    const seen = new Set();
    for (const user of this.allUsers) {
      if (seen.has(user.id)) continue;
      seen.add(user.id);
      const row = node('div'); row.className = 'user-row'; row.dataset.userId = user.id;
      row.append(node('strong', user.login), node('span', user.disabled ? 'Deactivated' : ''));
      if (user.owner) { const badge = node('span', 'Owner'); badge.className = 'user-badge user-badge-owner'; row.append(badge); }
      else if (user.admin) { const badge = node('span', 'Admin'); badge.className = 'user-badge user-badge-admin'; row.append(badge); }
      const privileged = user.owner || user.admin, mayAct = this.globalOwner || !privileged;
      if (this.globalOwner && !user.owner && !user.disabled) {
        const label = node('label'); label.className = 'user-admin-toggle';
        const box = node('input'); box.type = 'checkbox'; box.checked = !!user.admin; box.setAttribute('aria-label', `${user.login} is an admin`);
        box.onchange = async () => {
          try { await request(`/api/users/${user.id}/admin`, { method: 'PUT', body: { admin: box.checked } }); await this.load(); }
          catch (error) { box.checked = !box.checked; this.error.textContent = error.message; }
        };
        label.append(box, document.createTextNode(' Admin')); row.append(label);
      }
      if (!user.disabled && mayAct) {
        const reset = node('button', 'Reset link'); reset.type = 'button'; reset.className = 'btn';
        reset.onclick = async () => {
          try { const result = await request('/api/invites', { method: 'POST', body: { kind: 'reset', target_user_id: user.id } }); this.showLink(result.url); }
          catch (error) { this.error.textContent = error.message; }
        };
        row.append(reset);
        if (user.id !== currentUser && !user.owner) {
          const disable = node('button', 'Deactivate'); disable.type = 'button'; disable.className = 'btn';
          disable.onclick = async () => {
            if (!await confirmDialog(`Deactivate ${user.login} across all projects?`, { title: 'Deactivate account', confirm: 'Deactivate', danger: true })) return;
            try { await request(`/api/users/${user.id}/deactivate`, { method: 'POST' }); await this.load(); }
            catch (error) { this.error.textContent = error.message; }
          };
          row.append(disable);
        }
      }
      this.accounts.append(row);
    }
  }
  async createInvite() {
    this.error.textContent = '';
    try {
      const result = await request('/api/invites', { method: 'POST', body: { kind: 'join', project_id: this.projectSelect.value, role: this.roleSelect.value } });
      this.showLink(result.url);
    } catch (error) { this.error.textContent = error.message; }
  }
  // A link made on this computer carries an address that only works here: say so, since the person it is for is usually somewhere else.
  showLink(url) {
    this.link.value = url; this.link.hidden = false; this.link.select();
    let host = '';
    try { host = new URL(url).hostname; } catch { /* a link that does not parse gets no hint */ }
    const local = host === 'localhost' || host === '127.0.0.1' || host === '::1' || host === '[::1]';
    this.linkHint.hidden = !local;
    this.linkHint.textContent = local ? `This address (${host}) only works on this computer. For someone else, replace it with an address they can reach: keep everything from "/#invite=" on.` : '';
  }
}

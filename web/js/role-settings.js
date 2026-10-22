import { request } from './api.js';
import { confirmDialog } from './dialogs.js';
import { setGlyph } from './icon-button.js';

const $ = selector => document.querySelector(selector);
const option = (value, label) => { const node = document.createElement('option'); node.value = value; node.textContent = label; return node; };
const field = (name, value = '') => { const input = document.createElement('input'); input.name = name; input.value = value; return input; };

export class RoleSettings {
  constructor() {
    this.rolesList = $('#roles-list');
    this.roleForm = $('#role-editor');
    this.rolesList.onchange = () => { this.showRole(); this.setRoleEditorOpen(true); };
    this.roleForm.onsubmit = event => { event.preventDefault(); this.saveRole(); };
    // Every role is shown as a card above the editor (v1's list); a card opens that role in the editor below.
    this.roleCards = document.createElement('div'); this.roleCards.id = 'roles-cards'; this.roleCards.className = 'role-cards';
    const newRole = document.createElement('button'); newRole.type = 'button'; newRole.id = 'role-new'; newRole.className = 'btn'; newRole.textContent = 'New role';
    newRole.onclick = () => this.editRole('');
    // The editor is shown only while a role is being edited or created; Cancel, Esc or the open card's button close it.
    this.roleForm.hidden = true;
    $('#role-cancel').onclick = () => this.cancelRole();
    this.roleForm.addEventListener('keydown', event => {
      if (event.key !== 'Escape' || event.isComposing) return;
      // Esc would otherwise close the whole Settings dialog; inside the editor it only cancels the edit.
      event.preventDefault(); event.stopPropagation();
      this.cancelRole();
    });
    const toolbar = document.createElement('div'); toolbar.className = 'role-cards-toolbar'; toolbar.append(newRole);
    this.rolesList.closest('.settings-row').hidden = true;
    this.roleForm.before(toolbar, this.roleCards);
    $('#role-delete').onclick = () => this.deleteRole();
    $('#role-reset').onclick = () => this.resetRole();
  }
  async open() {
    try { await this.loadRoles(); } catch (error) { this.error(error.message); }
  }
  error(value = '') { $('#roles-error').textContent = value; }
  async loadRoles(selected = '') {
    this.error();
    this.rolesList.dataset.loaded = '';
    const sequence = this.rolesLoadSequence = (this.rolesLoadSequence || 0) + 1;
    try {
      const roles = await request('/api/roles');
      if (sequence !== this.rolesLoadSequence) return;
      this.roles = roles;
      this.rolesList.replaceChildren(option('', 'New role'), ...roles.map(role => option(role.id, role.label + ' (' + role.role_key + ')')));
      this.rolesList.value = selected;
      this.renderRoleCards(roles);
      this.showRole();
      this.setRoleEditorOpen(!!selected);
      this.rolesList.dataset.loaded = '1';
    } catch (error) { if (sequence === this.rolesLoadSequence) this.error(error.message); }
  }
  // Opens a role (or a blank one, with '') in the editor and brings the editor into view.
  editRole(id) {
    this.rolesList.value = id; this.showRole(); this.setRoleEditorOpen(true);
    this.roleForm.scrollIntoView?.({ block: 'nearest' });
    this.roleForm.elements[id ? 'label' : 'role_key'].focus({ preventScroll: true });
  }
  renderRoleCards(visible) {
    const parents = new Map((this.roles || []).map(role => [role.role_key, role]));
    this.roleCards.replaceChildren();
    if (!visible.length) { const empty = document.createElement('p'); empty.className = 'settings-intro'; empty.textContent = 'No roles yet.'; this.roleCards.append(empty); return; }
    for (const role of visible) {
      const card = document.createElement('article'); card.className = 'role-card'; card.dataset.roleId = role.id; card.dataset.roleKey = role.role_key;
      const head = document.createElement('div'); head.className = 'role-card-head';
      const emoji = document.createElement('span'); emoji.className = 'role-card-emoji'; emoji.textContent = role.emoji || ''; emoji.setAttribute('aria-hidden', 'true');
      const label = document.createElement('strong'); label.className = 'role-card-label'; label.textContent = role.label;
      const key = document.createElement('code'); key.className = 'role-card-key'; key.textContent = role.role_key;
      head.append(emoji, label, key);
      const chips = document.createElement('div'); chips.className = 'role-card-chips';
      const chip = (text, title) => { const item = document.createElement('span'); item.className = 'role-chip'; item.textContent = text; if (title) item.title = title; chips.append(item); };
      chip(role.cardinality === 'singleton' ? 'one holder' : 'many holders');
      if (role.extends) { const parent = parents.get(role.extends); chip(`extends ${parent ? parent.label : role.extends}`, 'Adds to the instructions of ' + role.extends); }
      if (role.cap_is_lead) chip('lead'); if (role.cap_can_spawn) chip('can spawn'); if (role.cap_singleton_writer) chip('writer');
      if (role.builtin) chip('built in');
      const summary = document.createElement('p'); summary.className = 'role-card-summary';
      const text = (role.body || role.resolved_body || '').trim().replace(/\s+/g, ' ');
      summary.textContent = text.length > 160 ? text.slice(0, 157) + '…' : text || 'No instructions of its own.';
      const edit = document.createElement('button'); edit.type = 'button'; edit.className = 'icon-btn role-card-edit'; setGlyph(edit, 'edit', `Edit ${role.label}`);
      edit.dataset.roleLabel = role.label; edit.setAttribute('aria-controls', 'role-editor');
      edit.onclick = () => { if (this.roleEditorOpen && this.rolesList.value === role.id) this.cancelRole(); else this.editRole(role.id); };
      card.append(head, chips, summary, edit); this.roleCards.append(card);
    }
    this.syncRoleEditButtons();
  }
  setRoleEditorOpen(open) {
    this.roleEditorOpen = open; this.roleForm.hidden = !open;
    this.syncRoleEditButtons();
  }
  // The card whose role is open in the editor turns its Edit button into a close button.
  syncRoleEditButtons() {
    for (const edit of this.roleCards.querySelectorAll('.role-card-edit')) {
      const open = !!this.roleEditorOpen && edit.closest('.role-card').dataset.roleId === this.rolesList.value, label = edit.dataset.roleLabel;
      edit.setAttribute('aria-expanded', String(open));
      if (open) { setGlyph(edit, 'close', `Close ${label} without saving`); edit.classList.add('role-card-close'); continue; }
      edit.classList.remove('role-card-close'); setGlyph(edit, 'edit', `Edit ${label}`);
    }
  }
  // What the editor holds now, to tell whether closing it would throw an edit away.
  roleFormState() {
    const elements = this.roleForm.elements;
    return JSON.stringify([...['role_key', 'label', 'emoji', 'body', 'extends', 'cardinality'].map(key => elements[key].value), ...['cap_is_lead', 'cap_can_spawn', 'cap_singleton_writer'].map(key => elements[key].checked)]);
  }
  // Closes the editor without saving (after a confirmation when something was changed) and gives focus back to what opened it.
  async cancelRole() {
    if (!this.roleEditorOpen) return;
    if (this.roleFormState() !== this.roleFormSaved && !await confirmDialog('Discard your changes to this role?', { title: 'Discard changes', confirm: 'Discard', danger: true })) return;
    if (!this.roleEditorOpen) return; // closed by another route while the question was open
    const id = this.rolesList.value;
    this.rolesList.value = ''; this.showRole(); this.setRoleEditorOpen(false);
    const opener = id && [...this.roleCards.querySelectorAll('.role-card')].find(card => card.dataset.roleId === id)?.querySelector('.role-card-edit');
    (opener || $('#role-new'))?.focus();
  }
  showRole() {
    const role = this.roles?.find(item => item.id === this.rolesList.value);
    for (const card of this.roleCards.children) card.classList.toggle('role-card-selected', !!role && card.dataset.roleId === role.id);
    for (const key of ['role_key', 'label', 'emoji', 'body', 'cardinality']) this.roleForm.elements[key].value = role?.[key] || (key === 'cardinality' ? 'multi' : '');
    for (const key of ['cap_is_lead', 'cap_can_spawn', 'cap_singleton_writer']) this.roleForm.elements[key].checked = !!role?.[key];
    this.roleForm.elements.role_key.readOnly = !!role;
    this.showExtends(role);
    // The text an agent gets once the parent's is added in front (only a saved role has one to show).
    const resolved = $('#role-resolved'), row = $('#role-resolved-row');
    resolved.value = role?.resolved_body ?? ''; row.hidden = !role || !role.extends;
    $('#role-delete').disabled = !role || role.builtin;
    // A built-in global role can go back to what shipped.
    $('#role-reset').disabled = !role || !role.builtin;
    this.roleFormSaved = this.roleFormState();
    this.syncRoleEditButtons();
  }
  // The roles a role may extend: every other role by key.
  showExtends(role) {
    const select = this.roleForm.elements.extends;
    const keys = new Map();
    for (const item of this.roles || []) if (!role || item.id !== role.id) keys.set(item.role_key, item);
    const current = role?.extends || '';
    const options = [option('', 'None'), ...[...keys.values()].sort((a, b) => a.role_key.localeCompare(b.role_key)).map(item => option(item.role_key, `${item.emoji ? item.emoji + ' ' : ''}${item.label} (${item.role_key})`))];
    // A parent that no longer exists is still shown, so saving does not drop it silently.
    if (current && !keys.has(current)) options.push(option(current, `${current} (missing)`));
    select.replaceChildren(...options); select.value = current;
  }
  roleData() {
    const selected = this.roles?.find(role => role.id === this.rolesList.value);
    const data = { role_key: this.roleForm.elements.role_key.value.trim(),
      label: this.roleForm.elements.label.value.trim(), emoji: this.roleForm.elements.emoji.value.trim(),
      body: this.roleForm.elements.body.value, extends: this.roleForm.elements.extends.value, cardinality: this.roleForm.elements.cardinality.value };
    for (const key of ['cap_is_lead', 'cap_can_spawn', 'cap_singleton_writer']) data[key] = this.roleForm.elements[key].checked;
    if (selected) { data.id = selected.id; data.rev = selected.rev; }
    return data;
  }
  async saveRole() {
    this.error();
    const data = this.roleData();
    try {
      const saved = await request(data.id ? '/api/roles/' + data.id : '/api/roles',
        { method: data.id ? 'PUT' : 'POST', body: data });
      await this.loadRoles(saved.id);
    } catch (error) { this.error(error.message); }
  }
  async resetRole() {
    const role = this.roles?.find(item => item.id === this.rolesList.value);
    if (!role || !role.builtin || !await confirmDialog(`Reset ${role.label} to its default name, instructions and settings? Your edits to it are lost.`, { title: 'Reset role', confirm: 'Reset', danger: true })) return;
    this.error();
    try {
      await request('/api/roles/' + role.id + '/reset', { method: 'POST', body: { rev: role.rev } });
      await this.loadRoles(role.id);
    } catch (error) { this.error(error.message); }
  }
  async deleteRole() {
    const role = this.roles?.find(item => item.id === this.rolesList.value);
    if (!role || !await confirmDialog('Delete role ' + role.label + '?', { title: 'Delete role', confirm: 'Delete', danger: true })) return;
    try {
      await request('/api/roles/' + role.id + '?rev=' + role.rev, { method: 'DELETE' });
      await this.loadRoles();
    } catch (error) { this.error(error.message); }
  }
}

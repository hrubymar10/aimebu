import { request, rights } from './api.js';
import { confirmDialog } from './dialogs.js';
import { setGlyph } from './icon-button.js';

export class MachinesSettings {
  constructor() {
    this.section = document.querySelector('#settings-machines');
    this.list = document.querySelector('#machines-list');
    this.error = document.querySelector('#machines-error');
    this.dialog = document.querySelector('#machine-pair-dialog');
    this.form = document.querySelector('#machine-pair-form');
    this.result = document.querySelector('#machine-pair-result');
    this.command = document.querySelector('#machine-pair-command');
    document.querySelector('#machine-add').onclick = () => this.openPairing();
    document.querySelector('#machine-refresh').onclick = () => this.load();
    document.querySelector('#machine-pair-cancel').onclick = () => this.dialog.close();
    document.querySelector('#machine-pair-done').onclick = () => this.dialog.close();
    document.querySelector('#machine-pair-copy').onclick = () => this.copyCommand();
    this.form.onsubmit = event => { event.preventDefault(); this.createPairing(); };
  }
  async load() {
    this.list.textContent = 'Loading machines…';
    try {
      const result = await request('/api/machines');
      this.error.textContent = '';
      // Pairing, approving and revoking are the owner's; an admin sees the machines and gets those controls off, with the reason.
      const add = document.querySelector('#machine-add'); add.hidden = false; add.disabled = !rights.owner; add.title = rights.owner ? '' : 'Only the owner can pair machines';
      this.render(result.machines || []);
    } catch (error) {
      this.error.textContent = error.status === 403 ? 'Machines are for the owner and the admins.' : error.message;
      document.querySelector('#machine-add').hidden = error.status === 403;
    }
  }
  render(machines) {
    this.list.replaceChildren();
    for (const machine of machines) {
      const card = document.createElement('article'); card.className = 'machine-card'; card.dataset.machineId = machine.id;
      const title = document.createElement('strong'); title.textContent = machine.name + (machine.is_host ? ' · Host' : '');
      const status = document.createElement('span'); status.className = 'machine-status';
      status.textContent = machine.revoked_at ? 'Revoked' : !machine.approved ? 'Pending approval' : machine.connected ? 'Connected' : 'Offline';
      const detail = document.createElement('small'); detail.textContent = [machine.supervisor_version && `Supervisor ${machine.supervisor_version}`, machine.last_seen && `Last seen ${new Date(machine.last_seen).toLocaleString()}`].filter(Boolean).join(' · ');
      const actions = document.createElement('div'); actions.className = 'machine-card-actions';
      if (!machine.approved && !machine.revoked_at) actions.append(this.action('Approve', () => this.change(machine, 'approve'), 'check'));
      if (!machine.is_host && !machine.revoked_at) actions.append(this.action('Revoke', () => this.change(machine, 'revoke')));
      if (!rights.owner) for (const button of actions.children) { button.disabled = true; button.title = `Only the owner can ${button.getAttribute('aria-label').toLowerCase()} machines`; }
      card.append(title, status, detail, actions); this.list.append(card);
    }
    if (!machines.length) { const empty = document.createElement('p'); empty.className = 'settings-intro'; empty.textContent = 'No machines yet.'; this.list.append(empty); }
  }
  action(label, callback, glyph) { const button = document.createElement('button'); button.type = 'button'; button.className = glyph ? 'icon-btn' : 'btn'; if (glyph) setGlyph(button, glyph, label); else { button.textContent = label; button.setAttribute('aria-label', label); } button.onclick = callback; return button; }
  async change(machine, action) {
    if (action === 'revoke' && !await confirmDialog(`Revoke ${machine.name}? Its supervisor will disconnect and cannot reconnect with this key.`, { title: 'Revoke machine', confirm: 'Revoke', danger: true })) return;
    this.error.textContent = '';
    try { await request(`/api/machines/${encodeURIComponent(machine.id)}/${action}`, { method: 'POST' }); await this.load(); }
    catch (error) { this.error.textContent = error.message; }
  }
  openPairing() {
    this.form.reset(); this.form.hidden = false; this.result.hidden = true;
    setGlyph(document.querySelector('#machine-pair-copy'), 'copy', 'Copy command');
    document.querySelector('#machine-pair-error').textContent = '';
    this.dialog.showModal(); this.form.elements.name.focus();
  }
  async createPairing() {
    const errorNode = document.querySelector('#machine-pair-error'); errorNode.textContent = '';
    const button = this.form.querySelector('button[type=submit]'); button.disabled = true;
    try {
      const result = await request('/api/machine-pairings', { method: 'POST', body: { name: this.form.elements.name.value.trim() } });
      this.command.textContent = result.command;
      document.querySelector('#machine-pair-expiry').textContent = `Expires ${new Date(result.expires_at).toLocaleString()}. The token can be used once.`;
      this.form.hidden = true; this.result.hidden = false;
      await this.load();
    } catch (error) { errorNode.textContent = error.message; }
    finally { button.disabled = false; }
  }
  async copyCommand() {
    const errorNode = document.querySelector('#machine-pair-error'); errorNode.textContent = '';
    try { await navigator.clipboard.writeText(this.command.textContent); setGlyph(document.querySelector('#machine-pair-copy'), 'check', 'Copied'); }
    catch { errorNode.textContent = 'Copy failed. Select the command and copy it manually.'; }
  }
}

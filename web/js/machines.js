import { request, rights } from './api.js';
import { confirmDialog } from './dialogs.js';
import { setGlyph } from './icon-button.js';

export class MachinesSettings {
  constructor() {
    this.section = document.querySelector('#settings-machines');
    this.list = document.querySelector('#machines-list');
    this.error = document.querySelector('#machines-error');
    this.note = document.querySelector('#machines-note');
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
      if (!machine.is_host && !machine.revoked_at) actions.append(this.action('Revoke', () => this.change(machine, 'revoke'), 'key-off'));
      // Stopping is not the owner's alone: the server stops only the agents the person may manage, as a single stop does.
      const stopAll = machine.approved && !machine.revoked_at ? this.action('Stop all agents on this machine', () => this.stopAgents(machine), 'stop') : null;
      if (stopAll) { stopAll.classList.add('machine-stop-all'); actions.append(stopAll); }
      if (!rights.owner) for (const button of [...actions.children].filter(button => button !== stopAll)) { button.disabled = true; button.title = `Only the owner can ${button.getAttribute('aria-label').toLowerCase()} machines`; }
      card.append(title, status, detail, actions); this.list.append(card);
    }
    if (!machines.length) { const empty = document.createElement('p'); empty.className = 'settings-intro'; empty.textContent = 'No machines yet.'; this.list.append(empty); }
  }
  action(label, callback, glyph) { const button = document.createElement('button'); button.type = 'button'; button.className = glyph ? 'icon-btn' : 'btn'; if (glyph) setGlyph(button, glyph, label); else { button.textContent = label; button.setAttribute('aria-label', label); } button.onclick = callback; return button; }
  // Stops every agent that runs on the machine, after the person confirms; says how many stopped and why any did not.
  async stopAgents(machine) {
    if (!await confirmDialog(`Stop every agent on ${machine.name}? Their current work will be stopped. Each agent can be started again from its card.`, { title: 'Stop all agents on this machine', confirm: 'Stop agents', danger: true })) return;
    this.error.textContent = ''; this.note.textContent = '';
    try {
      const result = await request(`/api/machines/${encodeURIComponent(machine.id)}/agents/stop`, { method: 'POST' });
      const parts = [`Stopped ${result.stopped.length} agent${result.stopped.length === 1 ? '' : 's'}`];
      if (result.already_stopped) parts.push(`${result.already_stopped} already stopped`);
      if (result.forbidden) parts.push(`${result.forbidden} left running (not yours to stop)`);
      if (result.failed?.length) parts.push(`${result.failed.length} could not be stopped: ${result.failed.map(item => item.error).join('; ')}`);
      this.note.textContent = `${parts.join(', ')}.`;
    } catch (error) { this.error.textContent = error.message; }
  }
  async change(machine, action) {
    if (action === 'revoke' && !await confirmDialog(`Revoke ${machine.name}? Its supervisor will disconnect and cannot reconnect with this key.`, { title: 'Revoke machine', confirm: 'Revoke', danger: true })) return;
    this.error.textContent = '';
    try { await request(`/api/machines/${encodeURIComponent(machine.id)}/${action}`, { method: 'POST' }); await this.load(); }
    catch (error) { this.error.textContent = error.message; }
  }
  openPairing() {
    this.form.reset(); this.form.hidden = false; this.result.hidden = true;
    clearTimeout(this.copiedTimer); setGlyph(document.querySelector('#machine-pair-copy'), 'copy', 'Copy command');
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
  // "Copied" is a confirmation, not a state: the button goes back to "Copy command" after a moment, like the other copy buttons, so the
  // command can be copied again without wondering whether the first copy is still what the clipboard holds.
  async copyCommand() {
    const errorNode = document.querySelector('#machine-pair-error'), button = document.querySelector('#machine-pair-copy'); errorNode.textContent = '';
    // One timer for the button: every outcome clears it first and a success sets it afresh, so a copy that finishes after another one (or
    // fails after a success) can neither restore the label early nor leave "Copied" next to "Copy failed".
    try {
      await navigator.clipboard.writeText(this.command.textContent);
      errorNode.textContent = ''; // an earlier copy that failed may settle first
      clearTimeout(this.copiedTimer); setGlyph(button, 'check', 'Copied');
      this.copiedTimer = setTimeout(() => setGlyph(button, 'copy', 'Copy command'), 2000);
    }
    catch { clearTimeout(this.copiedTimer); setGlyph(button, 'copy', 'Copy command'); errorNode.textContent = 'Copy failed. Select the command and copy it manually.'; }
  }
}

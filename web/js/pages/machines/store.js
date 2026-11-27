import { batch, signal } from '../../ui.js';
import { request, rights } from '../../api.js';
import { confirmDialog } from '../../dialogs.js';
import { writeOffWarning } from './model.js';

const COPIED_MS = 2000;

export class MachinesStore {
  constructor() {
    this.machines = signal(undefined);
    this.instances = signal({});
    this.owner = signal(false);
    this.addHidden = signal(false);
    this.addDisabled = signal(null);
    this.error = signal('');
    this.note = signal('');
    this.pairOpen = signal(false);
    this.name = signal('');
    this.pairing = signal(null);
    this.pairError = signal('');
    this.pairBusy = signal(false);
    this.copied = signal(false);
    this.scopeMachine = signal('');
    this.scopeProjects = signal([]);
    this.scopeAll = signal(true);
    this.scopeSelected = signal([]);
    this.scopeDisabled = signal(false);
    this.scopeBusy = signal(false);
    this.alive = true;
    this.generation = 0;
    this.copiedTimer = 0;
  }

  async load() {
    if (!this.alive) return;
    const generation = ++this.generation;
    this.machines.value = null;
    try {
      const result = await request('/api/machines');
      if (!this.alive || generation !== this.generation) return;
      batch(() => { this.error.value = ''; this.owner.value = rights.owner; this.addDisabled.value = !rights.owner; this.addHidden.value = false; this.machines.value = result.machines || []; });
      await this.loadInstances(result.machines || [], generation);
    } catch (failure) {
      if (!this.alive || generation !== this.generation) return;
      batch(() => { this.error.value = failure.status === 403 ? 'Machines are for the owner and the admins.' : failure.message; this.addHidden.value = failure.status === 403; });
    }
  }

  async openScope(machine) {
    if (this.scopeMachine.value === machine.id) { this.scopeMachine.value = ''; return; }
    this.scopeMachine.value = machine.id;
    this.scopeAll.value = machine.scope_mode !== 'listed';
    this.scopeSelected.value = machine.allowed_project_ids || [];
    this.scopeDisabled.value = !!machine.disabled;
    this.error.value = '';
    try {
      const projects = await request('/api/projects');
      if (this.alive && this.scopeMachine.value === machine.id) this.scopeProjects.value = projects.filter(project => !project.system);
    } catch (failure) { if (this.alive) this.error.value = failure.message; }
  }

  toggleProject(id, checked) {
    const selected = new Set(this.scopeSelected.value);
    if (checked) selected.add(id); else selected.delete(id);
    this.scopeSelected.value = [...selected];
  }

  async saveScope(machine) {
    this.scopeBusy.value = true;
    this.error.value = '';
    try {
      await request(`/api/machines/${encodeURIComponent(machine.id)}/scope`, { method: 'PUT', body: { scope_mode: this.scopeAll.value ? 'all' : 'listed', allowed_project_ids: this.scopeAll.value ? [] : this.scopeSelected.value, disabled: machine.is_host && this.scopeDisabled.value } });
      if (this.alive) { this.scopeMachine.value = ''; await this.load(); this.note.value = 'Machine settings saved.'; }
    } catch (failure) { if (this.alive) this.error.value = failure.message; }
    finally { if (this.alive) this.scopeBusy.value = false; }
  }

  // A machine's supervisor instances sit next to its card; a machine that cannot answer simply shows none. A revoked machine keeps
  // its list: its instances may still own open work, and a write-off is the one action nobody will ever preempt by reconnecting.
  async loadInstances(machines, generation) {
    const approved = machines.filter(machine => machine.approved);
    const results = await Promise.all(approved.map(machine => request(`/api/machines/${encodeURIComponent(machine.id)}/instances`).then(result => [machine.id, result.instances || []], () => [machine.id, []])));
    if (this.alive && generation === this.generation) this.instances.value = Object.fromEntries(results);
  }

  async writeOff(machine, instance) {
    if (!await confirmDialog(writeOffWarning(machine, instance), { title: 'Mark instance as gone', confirm: 'Mark as gone', danger: true })) return;
    if (!this.alive) return;
    batch(() => { this.error.value = ''; this.note.value = ''; });
    try {
      const result = await request(`/api/machines/${encodeURIComponent(machine.id)}/instances/${encodeURIComponent(instance.journal_id)}/gone`, { method: 'POST' });
      if (!this.alive) return;
      this.note.value = result.already_gone ? 'That instance was already gone.' : `Instance marked as gone: ${result.turns_lost} turn${result.turns_lost === 1 ? '' : 's'} lost, ${result.commands_ended} command${result.commands_ended === 1 ? '' : 's'} ended.`;
      await this.load();
    } catch (failure) { if (this.alive) this.error.value = failure.message; }
  }

  async change(machine, action) {
    if (action === 'revoke' && !await confirmDialog(`Revoke ${machine.name}? Its supervisor will disconnect and cannot reconnect with this key.`, { title: 'Revoke machine', confirm: 'Revoke', danger: true })) return;
    if (!this.alive) return;
    this.error.value = '';
    try { await request(`/api/machines/${encodeURIComponent(machine.id)}/${action}`, { method: 'POST' }); await this.load(); }
    catch (failure) { if (this.alive) this.error.value = failure.message; }
  }

  async stopAgents(machine) {
    if (!await confirmDialog(`Stop every agent on ${machine.name}? Their current work will be stopped. Each agent can be started again from its card.`, { title: 'Stop all agents on this machine', confirm: 'Stop agents', danger: true })) return;
    if (!this.alive) return;
    batch(() => { this.error.value = ''; this.note.value = ''; });
    try {
      const result = await request(`/api/machines/${encodeURIComponent(machine.id)}/agents/stop`, { method: 'POST' });
      if (!this.alive) return;
      const parts = [`Stopped ${result.stopped.length} agent${result.stopped.length === 1 ? '' : 's'}`];
      if (result.already_stopped) parts.push(`${result.already_stopped} already stopped`);
      if (result.forbidden) parts.push(`${result.forbidden} left running (not yours to stop)`);
      if (result.failed?.length) parts.push(`${result.failed.length} could not be stopped: ${result.failed.map(item => item.error).join('; ')}`);
      this.note.value = `${parts.join(', ')}.`;
    } catch (failure) { if (this.alive) this.error.value = failure.message; }
  }

  async restartSupervisor(machine) {
    if (!await confirmDialog('Restart the local supervisor now? Running turns will be interrupted and continued once.', { title: 'Restart supervisor', confirm: 'Restart now', danger: true })) return;
    if (!this.alive) return;
    this.error.value = '';
    try { await request(`/api/machines/${encodeURIComponent(machine.id)}/supervisor/restart`, { method: 'POST' }); await this.load(); }
    catch (failure) { if (this.alive) this.error.value = failure.message; }
  }

  openPairing() {
    clearTimeout(this.copiedTimer);
    batch(() => { this.name.value = ''; this.pairing.value = null; this.pairError.value = ''; this.copied.value = false; this.pairOpen.value = true; });
  }

  closePairing() { this.pairOpen.value = false; }

  async createPairing() {
    if (!this.alive) return;
    this.pairError.value = '';
    this.pairBusy.value = true;
    try {
      const result = await request('/api/machine-pairings', { method: 'POST', body: { name: this.name.value.trim() } });
      if (!this.alive) return;
      this.pairing.value = { command: result.command, warning: result.warning || '', expires: `Expires ${new Date(result.expires_at).toLocaleString()}. The token can be used once.` };
      await this.load();
    } catch (failure) { if (this.alive) this.pairError.value = failure.message; }
    finally { if (this.alive) this.pairBusy.value = false; }
  }

  // One timer for the button: every outcome clears it first, so a copy that settles after another can neither restore the label early
  // nor leave "Copied" next to "Copy failed".
  async copyCommand() {
    if (!this.alive) return;
    this.pairError.value = '';
    try {
      await navigator.clipboard.writeText(this.pairing.value.command);
      if (!this.alive) return;
      clearTimeout(this.copiedTimer);
      batch(() => { this.pairError.value = ''; this.copied.value = true; });
      this.copiedTimer = setTimeout(() => { this.copied.value = false; }, COPIED_MS);
    } catch {
      if (!this.alive) return;
      clearTimeout(this.copiedTimer);
      batch(() => { this.copied.value = false; this.pairError.value = 'Copy failed. Select the command and copy it manually.'; });
    }
  }

  destroy() { this.alive = false; this.generation++; clearTimeout(this.copiedTimer); }
}

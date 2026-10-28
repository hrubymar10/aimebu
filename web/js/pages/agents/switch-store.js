import { batch, computed, signal } from '../../ui.js';
import { agentLabel } from '../../sender-identity.js';
import { harnessName } from '../../harness-version.js';
import { hintParts, switchModel, switchReason, switchStart, switchUnchanged } from '../../spawn-model.js';
import { MachineStore } from './machine-store.js';

// The Switch dialog: one opening, for one agent, at a time. Each opening, catalog read and call carries a number and says nothing once a newer
// one started, the dialog was closed or the panel is gone.
export class SwitchStore {
  constructor(host) {
    this.host = host;
    this.machine = new MachineStore({ onChange: () => this.loadCatalog(), onFolderEdit: () => {} });
    this.agentName = signal('');
    this.catalog = signal([]);
    this.toolIndex = signal(-1);
    this.model = signal('');
    this.override = signal('');
    this.error = signal('');
    this.sending = signal(false);
    this.agentID = null; this.roomID = ''; this.needsUpdate = false;
    this.opening = 0; this.catalogRequestID = 0;
    this.alive = true;
    this.tool = computed(() => this.toolIndex.value < 0 ? null : this.catalog.value[this.toolIndex.value] || null);
    this.models = computed(() => (this.tool.value?.models || []).map(model => ({ value: model.id, label: model.label || model.id, note: model.note || '' })));
    this.unchanged = computed(() => !!this.tool.value && switchUnchanged({ tool: this.tool.value, model: this.model.value, agent: this.agent(), machineID: this.machine.id.value }));
    this.version = computed(() => this.tool.value?.cli_version ? `CLI version ${this.tool.value.cli_version}` : '');
    this.reason = computed(() => this.override.value || switchReason({ needsUpdate: this.needsUpdate, tool: this.tool.value, modelCount: this.models.value.length, unchanged: this.unchanged.value }));
    this.submitDisabled = computed(() => this.sending.value || !this.tool.value?.available || !this.models.value.length || this.unchanged.value || !this.machine.ready);
  }

  get dialog() { return this.host.switchDialog; }
  agent() { return this.host.cards.get(this.agentID); }

  async open(id) {
    const agent = this.host.cards.get(id);
    if (!this.alive || !agent) return;
    this.agentID = id;
    this.roomID = this.host.room.id;
    const opening = this.opening = this.opening + 1;
    this.catalogRequestID++; // a catalog asked for by an earlier opening is no answer to this one
    batch(() => {
      this.sending.value = false; this.agentName.value = `${agentLabel(agent) || id} · ${harnessName(agent.tool)} · ${agent.model}`;
      this.catalog.value = []; this.toolIndex.value = -1; this.model.value = ''; this.override.value = 'Loading available tools…'; this.error.value = '';
    });
    this.dialog.showModal();
    try {
      await this.machine.load(this.host.room.project_id, agent.machine_id);
    } catch (error) {
      if (this.alive && opening === this.opening && this.dialog.open) this.override.value = `Machines unavailable: ${error.message}`;
    }
  }

  async loadCatalog() {
    const id = this.agentID, machineID = this.machine.id.peek();
    const requestID = this.catalogRequestID = this.catalogRequestID + 1;
    batch(() => { this.catalog.value = []; this.toolIndex.value = -1; this.model.value = ''; this.override.value = 'Loading available tools…'; });
    if (!machineID) return;
    try {
      const result = await this.host.source.catalog(machineID);
      if (!this.alive || !this.dialog.open || this.agentID !== id || requestID !== this.catalogRequestID) return;
      const catalog = Array.isArray(result?.tools) ? result.tools : [];
      this.needsUpdate = !!result?.supervisor_update_needed;
      batch(() => { this.catalog.value = catalog; this.override.value = ''; this.pickTool(String(switchStart(catalog, this.agent()))); });
    } catch (error) {
      if (this.alive && this.dialog.open && requestID === this.catalogRequestID) this.override.value = `Tool catalog unavailable: ${error.message}`;
    }
  }

  pickTool(value) {
    const index = value === '' ? -1 : Number(value);
    batch(() => { this.toolIndex.value = index; this.model.value = switchModel(this.catalog.peek()[index], this.agent()); });
  }

  pickModel(value) { this.model.value = value; }

  async submit() {
    const id = this.agentID, selected = this.tool.peek(), model = this.model.peek();
    if (!id || !selected?.available || !model) return;
    const opening = this.opening, generation = this.host.generation, stale = () => !this.alive || !this.dialog.open || opening !== this.opening || generation !== this.host.generation;
    batch(() => { this.sending.value = true; this.error.value = ''; });
    try {
      await this.host.source.switch(id, { room_id: this.roomID, tool: selected.tool, variant: selected.variant, launcher_id: selected.launcher_id, model, machine_id: this.machine.id.peek() });
      if (stale()) return;
      await this.host.refresh();
      this.dialog.close();
    } catch (error) { if (stale()) return; this.error.value = error.message; }
    finally { if (!stale()) this.sending.value = false; }
  }

  // The room changed or the panel was reset: the opening is over, the dialog closes, and what it was waiting for starts nothing.
  invalidate() {
    this.dialog.close(); this.agentID = null;
    this.opening++; this.catalogRequestID++; this.machine.cancel();
  }

  hint(text) { return hintParts(text, this.host.canManageHarnesses()); }

  destroy() { this.alive = false; this.invalidate(); }
}

import { batch, computed, signal } from '../../ui.js';
import { rights } from '../../api.js';
import { agentLabel } from '../../sender-identity.js';
import { harnessName } from '../../harness-version.js';
import { hintParts, sortModelOptions, switchModel, switchReason, switchStart, switchUnchanged } from '../../spawn-model.js';
import { MachineStore } from './machine-store.js';

// The Switch dialog: one opening, for one agent, at a time. Each opening, catalog read and call carries a number and says nothing once a newer
// one started, the dialog was closed or the panel is gone.
export class SwitchStore {
  constructor(host) {
    this.host = host;
    this.machine = new MachineStore({ onChange: () => this.loadCatalog(), onFolderEdit: () => {}, mayAdmit: () => rights.admin || this.host.projectRole?.(this.host.room?.project_id) === 'owner' });
    this.agentName = signal('');
    this.catalog = signal([]);
    this.toolIndex = signal(-1);
    this.model = signal('');
    this.override = signal('');
    this.error = signal('');
    this.sending = signal(false);
    this.preview = signal(null);
    this.agentID = null; this.roomID = ''; this.needsUpdate = false;
    this.opening = 0; this.catalogRequestID = 0; this.selection = 0;
    this.alive = true;
    this.tool = computed(() => this.toolIndex.value < 0 ? null : this.catalog.value[this.toolIndex.value] || null);
    this.models = computed(() => {
      const tool = this.tool.value, agent = this.agent();
      const models = (tool?.models || []).map(model => ({ value: model.id, label: model.label || model.id, note: model.note || '', default: !!model.default }));
      if (agent?.model && tool?.launcher_id && tool.launcher_id === agent.launcher_id && this.machine.id.value === agent.machine_id && !models.some(model => model.value === agent.model)) {
        models.push({ value: agent.model, label: `${agent.model} (unavailable)`, note: 'No longer offered by this harness', unavailable: true });
      }
      return sortModelOptions(models);
    });
    this.unchanged = computed(() => !!this.tool.value && switchUnchanged({ tool: this.tool.value, model: this.model.value, agent: this.agent(), machineID: this.machine.id.value }));
    this.version = computed(() => this.tool.value?.cli_version ? `CLI version ${this.tool.value.cli_version}` : '');
    this.reason = computed(() => this.override.value || (this.models.value.find(model => model.value === this.model.value)?.unavailable ? 'This model is unavailable; choose another.' : switchReason({ needsUpdate: this.needsUpdate, tool: this.tool.value, modelCount: this.models.value.length, unchanged: this.unchanged.value })));
    this.submitDisabled = computed(() => this.sending.value || !this.tool.value?.available || !this.models.value.length || this.models.value.find(model => model.value === this.model.value)?.unavailable || this.unchanged.value || !this.machine.ready);
  }

  get dialog() { return this.host.switchDialog; }
  agent() { return this.host.cards.get(this.agentID); }

  async open(id) {
    const agent = this.host.cards.get(id);
    if (!this.alive || !agent) return;
    this.agentID = id;
    this.roomID = this.host.room.id;
    const opening = this.opening = this.opening + 1;
    this.selection++;
    this.catalogRequestID++; // a catalog asked for by an earlier opening is no answer to this one
    batch(() => {
      this.sending.value = false; this.agentName.value = `${agentLabel(agent) || id} · ${harnessName(agent.tool)} · ${agent.model}`;
      this.catalog.value = []; this.toolIndex.value = -1; this.model.value = ''; this.override.value = 'Loading available tools…'; this.error.value = ''; this.preview.value = null;
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
    this.selection++;
    batch(() => { this.catalog.value = []; this.toolIndex.value = -1; this.model.value = ''; this.override.value = 'Loading available tools…'; this.preview.value = null; });
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
    this.selection++;
    batch(() => { this.toolIndex.value = index; this.model.value = switchModel(this.catalog.peek()[index], this.agent(), this.machine.id.peek()); this.preview.value = null; });
  }

  pickModel(value) { this.selection++; batch(() => { this.model.value = value; this.preview.value = null; }); }

  params() {
    const selected = this.tool.peek();
    return { room_id: this.roomID, tool: selected.tool, variant: selected.variant, launcher_id: selected.launcher_id, model: this.model.peek(), machine_id: this.machine.id.peek() };
  }

  async submit() {
    const id = this.agentID, selected = this.tool.peek(), model = this.model.peek();
    if (!id || !selected?.available || !model || this.models.peek().find(item => item.value === model)?.unavailable) return;
    const opening = this.opening, generation = this.host.generation, stale = () => !this.alive || !this.dialog.open || opening !== this.opening || generation !== this.host.generation;
    const selection = this.selection;
    batch(() => { this.sending.value = true; this.error.value = ''; });
    try {
      if (!this.preview.peek()) {
        const preview = await this.host.source.switchPreview(id, this.params());
        if (!stale() && selection === this.selection) this.preview.value = preview;
        return;
      }
      await this.host.source.switch(id, this.params());
      if (stale()) return;
      await this.host.refresh();
      this.dialog.close();
    } catch (error) { if (stale()) return; this.error.value = error.message; }
    finally { if (!stale()) this.sending.value = false; }
  }

  // The room changed or the panel was reset: the opening is over, the dialog closes, and what it was waiting for starts nothing.
  invalidate() {
    this.dialog.close(); this.agentID = null;
    this.opening++; this.catalogRequestID++; this.selection++; this.machine.cancel();
  }

  hint(text) { return hintParts(text, this.host.canManageHarnesses()); }

  destroy() { this.alive = false; this.invalidate(); }
}

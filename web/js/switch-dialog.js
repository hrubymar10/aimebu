import { MachineSelection } from './machine-selection.js';
import { rights } from './api.js';
import { agentLabel } from './sender-identity.js';
import { harnessName } from './harness-version.js';

// The Switch dialog, moved out of AgentsPanel unchanged while the panel and the Spawn dialog were replaced; it reads the panel through `host`.
export class SwitchDialog {
  constructor(host) {
    this.host = host;
    this.switchDialog = document.querySelector('#switch-dialog'); this.switchForm = document.querySelector('#switch-form');
    this.switchMachine = new MachineSelection('switch', () => this.loadSwitchCatalog(), () => this.renderSwitchModels(false));
    document.querySelector('#switch-cancel').onclick = () => this.switchDialog.close();
    this.switchForm.elements.tool.onchange = () => this.renderSwitchModels();
    this.switchForm.elements.model.onchange = () => this.renderSwitchModels(false);
    this.switchForm.onsubmit = event => { event.preventDefault(); this.switchAgent(); };
  }
  get room() { return this.host.room; }
  get cards() { return this.host.cards; }
  get source() { return this.host.source; }
  get refreshGeneration() { return this.host.refreshGeneration; }
  refresh() { return this.host.refresh(); }
  roomChanged() { this.switchDialog.close(); this.switchAgentID = null; }
  close() { this.switchDialog.close(); }
  async openSwitch(id) {
    const agent = this.cards.get(id); if (!agent) return;
    this.switchAgentID = id;
    this.switchRoomID = this.room.id;
    this.switchOpening = (this.switchOpening || 0) + 1;
    this.switchCatalogRequestID = (this.switchCatalogRequestID || 0) + 1; // a catalog asked for by an earlier opening is no answer to this one
    document.querySelector('#switch-cancel').disabled = false;
    this.switchDialog.showModal();
    document.querySelector('#switch-agent-name').textContent = `${agentLabel(agent) || id} · ${harnessName(agent.tool)} · ${agent.model}`;
    const tools = this.switchForm.elements.tool, models = this.switchForm.elements.model;
    const reason = document.querySelector('#switch-tool-reason');
    tools.replaceChildren(); models.replaceChildren();
    reason.textContent = 'Loading available tools…'; document.querySelector('#switch-version').textContent = '';
    document.querySelector('#switch-error').textContent = '';
    this.switchForm.querySelector('button[type=submit]').disabled = true;
    try {
      await this.loadOwner(); // before the catalog, whose hints depend on it
      await this.switchMachine.load(this.room.project_id, agent.machine_id);
    } catch (error) {
      if (this.switchDialog.open) reason.textContent = `Machines unavailable: ${error.message}`;
    }
  }
  async loadSwitchCatalog() {
    const id = this.switchAgentID, machineID = this.switchMachine.id;
    const requestID = (this.switchCatalogRequestID || 0) + 1; this.switchCatalogRequestID = requestID;
    const tools = this.switchForm.elements.tool, models = this.switchForm.elements.model;
    tools.replaceChildren(); models.replaceChildren(); this.switchForm.querySelector('button[type=submit]').disabled = true;
    document.querySelector('#switch-tool-reason').textContent = 'Loading available tools…';
    if (!machineID) return;
    try {
      const result = await this.source.catalog(machineID);
      if (!this.switchDialog.open || this.switchAgentID !== id || requestID !== this.switchCatalogRequestID) return;
      this.switchCatalog = Array.isArray(result?.tools) ? result.tools : [];
      this.switchNeedsUpdate = !!result?.supervisor_update_needed;
      this.switchCatalog.forEach((tool, index) => {
        const option = document.createElement('option'); option.value = String(index);
        option.textContent = `${tool.label || tool.tool}${tool.available ? '' : ` — ${tool.unavailable_reason || 'unavailable'}`}`;
        option.disabled = !tool.available; tools.append(option);
      });
      const agent = this.cards.get(id);
      const current = this.switchCatalog.findIndex(tool => tool.available && tool.tool === agent?.tool && tool.variant === agent?.variant);
      tools.selectedIndex = current >= 0 ? current : this.switchCatalog.findIndex(tool => tool.available);
      this.renderSwitchModels();
    } catch (error) {
      if (this.switchDialog.open && requestID === this.switchCatalogRequestID) document.querySelector('#switch-tool-reason').textContent = `Tool catalog unavailable: ${error.message}`;
    }
  }
  renderSwitchModels(resetModels = true) {
    const agent = this.cards.get(this.switchAgentID);
    const tools = this.switchForm.elements.tool, models = this.switchForm.elements.model;
    const tool = tools.selectedIndex < 0 ? null : this.switchCatalog?.[Number(tools.value)];
    if (resetModels) {
      models.replaceChildren();
      for (const model of tool?.models || []) {
        const option = document.createElement('option'); option.value = model.id; option.textContent = model.label || model.id; if (model.note) option.title = model.note; models.append(option);
      }
      const preferred = (tool?.models || []).find(model => model.default);
      if (preferred) models.value = preferred.id;
      if (tool?.tool === agent?.tool && tool?.variant === agent?.variant && [...models.options].some(option => option.value === agent.model)) models.value = agent.model;
    }
    document.querySelector('#switch-version').textContent = tool?.cli_version ? `CLI version ${tool.cli_version}` : '';
    const unchanged = tool?.tool === agent?.tool && tool?.variant === agent?.variant && models.value === agent?.model && (!agent?.machine_id || this.switchMachine.id === agent.machine_id);
    this.setHint(document.querySelector('#switch-tool-reason'), this.switchNeedsUpdate && !tool?.available ? 'This machine\'s supervisor predates harnesses. Update the supervisor to add harnesses.'
      : !tool ? 'No harness is available on a connected machine. Add one in Settings → Harnesses.'
      : !models.options.length ? 'This harness has no models yet. Add one under Settings → Harnesses.'
        : unchanged ? 'Choose a different model or tool.'
          : tool.toolbox === false ? `No agent toolbox: ${tool.toolbox_reason || 'unavailable for this launcher'}.` : '');
    this.switchForm.querySelector('button[type=submit]').disabled = !tool?.available || !models.options.length || unchanged || !this.switchMachine.folderReady;
  }
  async switchAgent() {
    const id = this.switchAgentID;
    const tools = this.switchForm.elements.tool;
    const selected = tools.selectedIndex < 0 ? null : this.switchCatalog?.[Number(tools.value)];
    const model = this.switchForm.elements.model.value;
    if (!id || !selected?.available || !model) return;
    const opening = this.switchOpening, generation = this.refreshGeneration, stale = () => !this.switchDialog.open || opening !== this.switchOpening || generation !== this.refreshGeneration;
    const submit = this.switchForm.querySelector('button[type=submit]');
    const cancel = document.querySelector('#switch-cancel');
    submit.disabled = true; cancel.disabled = true;
    document.querySelector('#switch-error').textContent = '';
    try {
      await this.source.switch(id, { room_id: this.switchRoomID, tool: selected.tool, variant: selected.variant, launcher_id: selected.launcher_id, model, machine_id: this.switchMachine.id });
      if (stale()) return;
      await this.refresh();
      this.switchDialog.close();
    } catch (error) { if (stale()) return; document.querySelector('#switch-error').textContent = error.message; this.renderSwitchModels(false); }
    finally { if (!stale()) cancel.disabled = false; }
  }
  // A hint that names Settings → Harnesses makes that name a button that opens it, so an empty machine says where to go.
  // Only an owner or admin can add or change a harness, so anyone else gets plain words (the link would only end in a refusal).
  setHint(node, text) {
    const marker = 'Settings → Harnesses', at = text.indexOf(marker);
    node.replaceChildren();
    if (at < 0) { node.textContent = text; return; }
    if (!this.canManageHarnesses) {
      node.textContent = text.replace(/Add a harness in Settings → Harnesses/, 'Ask an admin to add a harness').replace(/Add one (?:in|under) Settings → Harnesses/, 'Ask an admin to add one').replace(/add one (?:in|under) Settings → Harnesses/, 'ask an admin to add one')
        .replace(/; [Cc]heck Settings → Harnesses/, '; ask an admin to check the harnesses').replace(/[Cc]heck Settings → Harnesses/, 'Ask an admin to check the harnesses').replace(/enable it in Settings → Harnesses/, 'ask an admin to enable it');
      return;
    }
    const link = document.createElement('button'); link.type = 'button'; link.className = 'inline-link'; link.textContent = marker;
    // Settings opens on the machine the dialog is showing.
    const machineID = this.switchMachine.id;
    link.onclick = () => { this.switchDialog.close(); document.dispatchEvent(new CustomEvent('aimebu:open-settings', { detail: { section: 'harnesses', machineID } })); };
    node.append(text.slice(0, at), link, text.slice(at + marker.length));
  }
  // Whether this person is the owner or an admin (who are trusted alike), read when a dialog opens; it decides what the hints can offer.
  async loadOwner() {
    this.canManageHarnesses = rights.admin;
  }
}

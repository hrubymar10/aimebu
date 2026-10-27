import { request } from './api.js';
import { iconButton } from './icon-button.js';

let nextEditorID = 0;

// The same ordered picker is used by global roles and project overrides.
export class SpawnDefaultsEditor {
  constructor(container) {
    this.container = container;
    this.id = `spawn-defaults-${++nextEditorID}`;
    this.items = [];
    this.options = [];
    this.mode = 'in_turn';
    this.render();
  }
  async loadOptions(projectID = '') {
    const query = projectID ? `?project_id=${encodeURIComponent(projectID)}` : '';
    const machines = (await request('/api/machines' + query)).machines || [];
    const rows = await Promise.all(machines.map(async machine => {
      try {
        const catalog = await request(`/api/spawn-catalog?machine_id=${encodeURIComponent(machine.id)}`);
        return (catalog.tools || []).flatMap(tool => (tool.models || []).filter(model => tool.launcher_id && model.id).map(model => ({
          launcher_id: tool.launcher_id, model: model.id,
          label: `${machine.name} · ${tool.label} · ${model.label || model.id}`,
        })));
      } catch { return []; }
    }));
    this.options = rows.flat();
    this.render();
  }
  set(items, mode) {
    this.items = (items || []).map(item => ({ launcher_id: item.launcher_id, model: item.model }));
    this.mode = mode || 'in_turn';
    this.render();
  }
  get() { return { default_agents: this.items.map(item => ({ ...item })), pick_mode: this.mode }; }
  render() {
    const heading = document.createElement('h4'); heading.textContent = 'Default agents';
    const help = document.createElement('p'); help.className = 'settings-desc';
    help.textContent = 'Used when spawning this role. An empty list leaves the harness and model to the person or lead.';
    const group = document.createElement('div'); group.className = 'settings-group spawn-default-list';
    const settingRow = (name, description, control, index) => {
      const row = document.createElement('div'); row.className = 'settings-row';
      const info = document.createElement('div'); info.className = 'settings-row-info';
      const label = document.createElement('label'); label.className = 'settings-label';
      label.htmlFor = control.id = `${this.id}-${index}`; label.textContent = name;
      const desc = document.createElement('span'); desc.className = 'settings-desc';
      desc.id = `${control.id}-desc`; desc.textContent = description;
      control.setAttribute('aria-describedby', desc.id);
      info.append(label, desc); row.append(info, control);
      return row;
    };
    const mode = document.createElement('select'); mode.setAttribute('aria-label', 'Default agent selection mode');
    for (const [value, label] of [['in_turn', 'In turn'], ['randomly', 'Randomly']]) {
      const option = document.createElement('option'); option.value = value; option.textContent = label; mode.append(option);
    }
    mode.value = this.mode; mode.onchange = () => { this.mode = mode.value; };
    // The order matters only when there is more than one default to choose between.
    if (this.items.length > 1) group.append(settingRow('Pick order', 'Choose in turn or randomly from available agents.', mode, 'mode'));
    this.items.forEach((item, index) => {
      const select = document.createElement('select'); select.setAttribute('aria-label', `Default agent ${index + 1} harness and model`);
      const selected = `${item.launcher_id}:${item.model}`;
      const available = this.options.some(option => `${option.launcher_id}:${option.model}` === selected);
      if (!available) {
        const missing = document.createElement('option'); missing.value = selected;
        missing.textContent = `${item.launcher_id} · ${item.model} (unavailable)`; select.append(missing);
      }
      for (const choice of this.options) {
        const option = document.createElement('option'); option.value = `${choice.launcher_id}:${choice.model}`;
        option.textContent = choice.label; select.append(option);
      }
      select.value = selected;
      select.onchange = () => {
        const choice = this.options.find(value => `${value.launcher_id}:${value.model}` === select.value);
        if (choice) { this.items[index] = { launcher_id: choice.launcher_id, model: choice.model }; this.render(); this.container.querySelectorAll('.spawn-default-row select')[index]?.focus(); }
      };
      const up = iconButton('arrow-up', `Move default agent ${index + 1} up`);
      up.disabled = index === 0; up.onclick = () => this.move(index, -1);
      const down = iconButton('arrow-down', `Move default agent ${index + 1} down`);
      down.disabled = index === this.items.length - 1; down.onclick = () => this.move(index, 1);
      const remove = iconButton('trash', `Remove default agent ${index + 1}`, { danger: true });
      remove.onclick = () => { this.items.splice(index, 1); this.render(); (this.container.querySelectorAll('.spawn-default-row select')[Math.min(index, this.items.length - 1)] || this.container.querySelector('[aria-label="Add default agent"]'))?.focus(); };
      const row = settingRow(`Default agent ${index + 1}`, 'Harness and model for this choice.', select, index);
      row.classList.add('spawn-default-row'); row.append(up, down, remove); group.append(row);
    });
    const add = iconButton('plus', 'Add default agent'); add.disabled = !this.options.length || this.items.length >= 32;
    add.onclick = () => { const choice = this.options[0]; this.items.push({ launcher_id: choice.launcher_id, model: choice.model }); this.render(); this.container.querySelectorAll('.spawn-default-row select')[this.items.length - 1]?.focus(); };
    this.container.replaceChildren(heading, help, group, add);
  }
  move(index, by) {
    const [item] = this.items.splice(index, 1); this.items.splice(index + by, 0, item); this.render();
    this.container.querySelectorAll('.spawn-default-row')[index + by]?.querySelector('select')?.focus();
  }
}

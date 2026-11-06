import { html, useState, useRef, useLayoutEffect } from '../ui.js';
import { IconButton } from './icon-button.js';

let counter = 0;
const keyOf = item => `${item.launcher_id}:${item.model}`;

// The ordered default agents of a role: pick order (when there is more than one), one harness and model per entry, and add.
export function SpawnDefaults({ items, mode, options, onChange }) {
  const [id] = useState(() => `spawn-defaults-${++counter}`);
  const root = useRef(null);
  const focusAt = useRef(null);
  useLayoutEffect(() => {
    const target = focusAt.current;
    focusAt.current = null;
    if (target === null) return;
    const selects = root.current.querySelectorAll('.spawn-default-row select');
    (target === 'add' ? root.current.querySelector('[aria-label="Add default agent"]') : selects[target])?.focus();
  });
  const change = (next, focus) => { focusAt.current = focus; onChange(next); };
  const replace = (index, item) => change({ items: items.map((old, at) => (at === index ? item : old)), mode }, index);
  const move = (index, by) => {
    const next = [...items];
    const [item] = next.splice(index, 1);
    next.splice(index + by, 0, item);
    change({ items: next, mode }, index + by);
  };
  const row = (name, description, control, index, extra = []) => html`
    <div class=${`settings-row${extra.length ? ' spawn-default-row' : ''}`} key=${index}>
      <div class="settings-row-info">
        <label class="settings-label" for=${`${id}-${index}`}>${name}</label>
        <span class="settings-desc" id=${`${id}-${index}-desc`}>${description}</span>
      </div>
      ${control}${extra}
    </div>`;
  return html`<section class="settings-subsection spawn-defaults" ref=${root}>
    <h4>Default agents</h4>
    <p class="settings-desc">Used when spawning this role. An empty list leaves the harness and model to the person or lead.</p>
    <div class="settings-group spawn-default-list">
      ${items.length > 1 && row('Pick order', 'Choose in turn or randomly from available agents.', html`
        <select class="settings-select" id=${`${id}-mode`} aria-describedby=${`${id}-mode-desc`} aria-label="Default agent selection mode" value=${mode} onChange=${event => onChange({ items, mode: event.currentTarget.value })}>
          <option value="in_turn">In turn</option><option value="randomly">Randomly</option>
        </select>`, 'mode')}
      ${items.map((item, index) => row(`Default agent ${index + 1}`, 'Harness and model for this choice.', html`
        <select class="settings-select" id=${`${id}-${index}`} aria-describedby=${`${id}-${index}-desc`} aria-label=${`Default agent ${index + 1} harness and model`} value=${keyOf(item)} onChange=${event => {
          const choice = options.find(value => keyOf(value) === event.currentTarget.value);
          if (choice) replace(index, { launcher_id: choice.launcher_id, model: choice.model });
        }}>
          ${!options.some(choice => keyOf(choice) === keyOf(item)) && html`<option value=${keyOf(item)}>${`${item.launcher_id} · ${item.model} (unavailable)`}</option>`}
          ${options.map(choice => html`<option value=${keyOf(choice)} key=${keyOf(choice)}>${choice.label}</option>`)}
        </select>`, index, [
        html`<${IconButton} name="arrow-up" label=${`Move default agent ${index + 1} up`} disabled=${index === 0} onClick=${() => move(index, -1)} />`,
        html`<${IconButton} name="arrow-down" label=${`Move default agent ${index + 1} down`} disabled=${index === items.length - 1} onClick=${() => move(index, 1)} />`,
        html`<${IconButton} name="trash" label=${`Remove default agent ${index + 1}`} danger onClick=${() => {
          const next = items.filter((_, at) => at !== index);
          change({ items: next, mode }, next.length ? Math.min(index, next.length - 1) : 'add');
        }} />`]))}
    </div>
    <${IconButton} name="plus" label="Add default agent" disabled=${!options.length || items.length >= 32} onClick=${() => change({ items: [...items, { launcher_id: options[0].launcher_id, model: options[0].model }], mode }, items.length)} />
  </section>`;
}

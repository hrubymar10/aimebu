import { iconButton } from './icon-button.js';
import { PICK_MODES, canAddDefault } from './role-defaults-logic.js';

const node = (tag, className = '', value = '') => {
  const element = document.createElement(tag);
  if (className) element.className = className;
  if (value) element.textContent = value;
  return element;
};

export const roleMark = role => role.emoji || (role.label || role.role_key).slice(0, 2);

// One role's "default for this model": a labelled switch row. `update({ on, position, total, disabled, reason })` redraws it from the
// current defaults.
export function roleToggle(role, modelLabel, onToggle) {
  const name = `${role.label} default: ${modelLabel}`;
  const row = node('label', 'harness-role-switch'), box = document.createElement('input'); box.type = 'checkbox'; box.setAttribute('aria-label', name);
  const text = node('span', 'harness-role-switch-text'), badge = node('span', 'harness-role-switch-position');
  text.append(node('span', 'harness-role-switch-mark', roleMark(role)), document.createTextNode(` ${role.label}`));
  box.onchange = () => onToggle(box.checked);
  row.append(box, text, badge);
  return { element: row, update: state => {
    box.checked = state.on; box.disabled = state.disabled; badge.textContent = state.on ? `#${state.position} of ${state.total}` : '';
    row.title = state.reason || ''; row.classList.toggle('is-on', state.on);
  } };
}

// The ordered defaults of every role and each role's pick order, edited in place: every change is one save. `labelOf(item)` names an entry.
export function rolePanel({ roles, labelOf, store, report, refresh }) {
  const panel = node('section', 'harness-roles'); panel.setAttribute('aria-label', 'Role defaults');
  panel.append(node('h4', '', 'Role defaults'), node('p', 'settings-desc', 'The models a role is spawned with, in order. Tick a role on a model above to add it; order and pick mode are set here. A role with no defaults leaves the harness and model to the person or lead.'));
  const list = node('div', 'harness-role-list');
  const run = async action => { report(''); try { await action(); } catch (error) { report(error.message || 'Could not save the role'); } refresh(); };
  for (const role of roles) {
    const card = node('article', 'harness-role-card'); card.dataset.roleKey = role.role_key;
    const title = node('div', 'harness-role-head-line');
    title.append(node('span', 'harness-role-mark', roleMark(role)), node('strong', '', role.label), node('code', 'harness-role-key', role.role_key));
    const mode = document.createElement('select'); mode.setAttribute('aria-label', `Pick order for ${role.label}`); mode.title = 'How the next default is picked';
    for (const [value, label] of PICK_MODES) { const option = node('option', '', label); option.value = value; mode.append(option); }
    mode.value = role.pick_mode || 'in_turn'; mode.onchange = () => run(() => store.setMode(role, mode.value));
    title.append(mode);
    const order = node('ol', 'harness-role-order');
    const items = role.default_agents || [];
    if (!items.length) order.append(node('li', 'settings-desc harness-role-empty', 'No defaults'));
    items.forEach((item, index) => {
      const entry = node('li', 'harness-role-entry'); entry.dataset.model = item.model;
      entry.append(node('span', 'harness-role-entry-label', labelOf(item)));
      const up = iconButton('arrow-up', `Move ${labelOf(item)} up for ${role.label}`); up.disabled = index === 0; up.onclick = () => run(() => store.moveEntry(role, item.launcher_id, item.model, -1));
      const down = iconButton('arrow-down', `Move ${labelOf(item)} down for ${role.label}`); down.disabled = index === items.length - 1; down.onclick = () => run(() => store.moveEntry(role, item.launcher_id, item.model, 1));
      const remove = iconButton('trash', `Remove ${labelOf(item)} from ${role.label}`, { danger: true }); remove.onclick = () => run(() => store.removeEntry(role, item.launcher_id, item.model));
      entry.append(up, down, remove); order.append(entry);
    });
    if (!canAddDefault(role)) order.append(node('li', 'settings-desc', 'The list is full.'));
    card.append(title, order); list.append(card);
  }
  if (!roles.length) list.append(node('p', 'settings-intro', 'No roles yet.'));
  panel.append(list);
  return panel;
}

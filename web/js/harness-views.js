import { iconButton } from './icon-button.js';
import { PICK_MODES, canAddDefault } from './role-defaults-logic.js';

const node = (tag, className = '', value = '') => {
  const element = document.createElement(tag);
  if (className) element.className = className;
  if (value) element.textContent = value;
  return element;
};

// The three layouts of Settings → Harnesses, all over the same data: A a table (a matrix of models by role), B cards with their models
// as tiles, C a list of harnesses beside the detail of the chosen one.
export const LAYOUTS = [
  ['a', 'A', 'Table: one row per harness, models in a role matrix'],
  ['b', 'B', 'Cards: one card per harness, models as tiles'],
  ['c', 'C', 'Split: harness list beside the chosen harness'],
];
export const LAYOUT_KEYS = LAYOUTS.map(([key]) => key);
export const normalizeLayout = value => LAYOUT_KEYS.includes(value) ? value : 'a';

// A segmented radio group: Left/Right (and Up/Down) move the choice and select it, Home/End jump to the ends, like a native radio group.
export function layoutSwitch(current, onChange) {
  const group = node('div', 'harness-layout-switch'); group.setAttribute('role', 'radiogroup'); group.setAttribute('aria-label', 'Harness layout');
  const buttons = LAYOUTS.map(([key, letter, title]) => {
    const button = node('button', 'btn harness-layout-option', letter); button.type = 'button'; button.dataset.layout = key;
    button.setAttribute('role', 'radio'); button.title = title; button.setAttribute('aria-label', `Layout ${letter}: ${title}`);
    return button;
  });
  const show = key => {
    for (const button of buttons) {
      const on = button.dataset.layout === key;
      button.setAttribute('aria-checked', String(on)); button.tabIndex = on ? 0 : -1; button.classList.toggle('is-selected', on);
    }
  };
  buttons.forEach(button => {
    button.onclick = () => { show(button.dataset.layout); onChange(button.dataset.layout); };
    button.onkeydown = event => {
      const at = buttons.indexOf(button), last = buttons.length - 1;
      const to = { ArrowRight: at === last ? 0 : at + 1, ArrowDown: at === last ? 0 : at + 1, ArrowLeft: at ? at - 1 : last, ArrowUp: at ? at - 1 : last, Home: 0, End: last }[event.key];
      if (to === undefined) return;
      event.preventDefault(); buttons[to].focus(); buttons[to].click();
    };
  });
  group.append(...buttons); show(current);
  return { element: group, show };
}

export const roleMark = role => role.emoji || (role.label || role.role_key).slice(0, 2);

// One role's "default for this model" control in the shape of a layout: a checkbox cell of the matrix (A), a pressed-state chip (B) or a
// labelled switch row (C). `update({ on, position, total, disabled, reason })` redraws it from the current defaults.
export function roleToggle(kind, role, modelLabel, onToggle) {
  const name = `${role.label} default: ${modelLabel}`;
  if (kind === 'a') {
    const cell = node('td', 'harness-role-cell'), box = document.createElement('input'); box.type = 'checkbox'; box.setAttribute('aria-label', name);
    box.onchange = () => onToggle(box.checked);
    cell.append(box);
    return { element: cell, update: state => { box.checked = state.on; box.disabled = state.disabled; cell.title = state.reason || (state.on ? `${role.label}: default (${state.position} of ${state.total})` : `Make a default for ${role.label}`); } };
  }
  if (kind === 'b') {
    const chip = node('button', 'btn harness-role-chip'); chip.type = 'button';
    chip.append(node('span', 'harness-role-chip-mark', roleMark(role)), node('span', 'harness-role-chip-position'));
    // The pressed state flips at once and the target is taken from it, so a quick second click asks for the opposite of the first; the saved
    // state is drawn again when the save is done.
    chip.onclick = () => { const next = chip.getAttribute('aria-pressed') !== 'true'; chip.setAttribute('aria-pressed', String(next)); onToggle(next); };
    return { element: chip, update: state => {
      chip.setAttribute('aria-pressed', String(state.on)); chip.disabled = state.disabled; chip.setAttribute('aria-label', name);
      chip.querySelector('.harness-role-chip-position').textContent = state.on ? String(state.position) : '';
      chip.title = state.reason || (state.on ? `${role.label}: default, number ${state.position} of ${state.total}. Click to remove` : `Make a default for ${role.label}`);
    } };
  }
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

// The header row of the A matrix: one column per role, headed by its mark with the role's name as tooltip and accessible name.
export function matrixHead(roles, hasRemove = true) {
  const head = node('thead'), row = node('tr');
  const cell = (text, className = '') => { const th = node('th', className, text); th.scope = 'col'; return th; };
  row.append(cell('Default'), cell('Model'), cell('Note'));
  for (const role of roles) {
    const th = cell('', 'harness-role-head'); th.title = `Default for ${role.label}`;
    th.append(node('span', '', roleMark(role)), node('span', 'sr-only', `Default for ${role.label}`));
    row.append(th);
  }
  if (hasRemove) { const th = cell('', 'harness-actions-head'); th.append(node('span', 'sr-only', 'Actions')); row.append(th); }
  head.append(row);
  return head;
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

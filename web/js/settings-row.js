// The shared settings row: label and description on the left, the control on the right. Static
// markup in index.html follows the same structure (.settings-group > .settings-row), so this helper
// and that markup stay interchangeable and styled by one block of CSS.
let counter = 0;

// `control` is a form field or a wrapper element; the first field inside it is labelled and described.
export function settingsRow({ label, desc, control, id = '', className = '' }) {
  const row = document.createElement('div');
  row.className = `settings-row${className ? ` ${className}` : ''}`;
  const info = document.createElement('div'); info.className = 'settings-row-info';
  const name = document.createElement('label'); name.className = 'settings-label'; name.textContent = label;
  const hint = document.createElement('span'); hint.className = 'settings-desc'; hint.textContent = desc;
  info.append(name, hint);
  const slot = document.createElement('div'); slot.className = 'settings-control'; slot.append(control);
  const field = control.matches('input,select,textarea,button') ? control : control.querySelector('input,select,textarea,button');
  if (field) {
    field.id ||= id || `settings-row-${++counter}`;
    name.htmlFor = field.id; hint.id = `${field.id}-desc`; field.setAttribute('aria-describedby', hint.id);
  }
  row.append(info, slot);
  return row;
}

export function settingsGroup(rows, label = '') {
  const group = document.createElement('div'); group.className = 'settings-group';
  if (label) { group.setAttribute('role', 'group'); group.setAttribute('aria-label', label); }
  group.append(...rows);
  return group;
}

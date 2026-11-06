// Buttons that are an icon, never text: the glyph is a file in /icons drawn through a mask (see .icon-mask-* in theme-v1.css),
// and the button carries its tooltip and accessible name.
export function iconGlyph(name) {
  const glyph = document.createElement('span'); glyph.className = `icon-mask icon-mask-${name}`; glyph.setAttribute('aria-hidden', 'true');
  return glyph;
}
// Shows `name` in `button`; with a label it is also the tooltip and the accessible name.
export function setGlyph(button, name, label) {
  button.replaceChildren(iconGlyph(name));
  if (label) { button.title = label; button.setAttribute('aria-label', label); }
  return button;
}
// A row action (move up or down, remove, delete) as an icon button: the label is its tooltip and accessible name.
export function iconButton(name, label, { danger = false, className = '' } = {}) {
  const button = document.createElement('button'); button.type = 'button';
  button.className = ['btn-icon', danger ? 'btn-danger' : '', className].filter(Boolean).join(' ');
  return setGlyph(button, name, label);
}

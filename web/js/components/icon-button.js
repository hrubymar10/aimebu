import { html } from '../ui.js';

export function IconButton({ name, label, danger = false, className = '', ...rest }) {
  const classes = ['icon-btn', danger ? 'btn-danger' : '', className].filter(Boolean).join(' ');
  return html`<button type="button" class=${classes} title=${label} aria-label=${label} ...${rest}><span class=${`icon-mask icon-mask-${name}`} aria-hidden="true"></span></button>`;
}

import { html, cloneElement, useState } from '../ui.js';

let counter = 0;

export function SettingsRow({ label, desc, id = '', className = '', children }) {
  const [fallback] = useState(() => `settings-row-p${++counter}`);
  const field = id || fallback;
  const control = children && typeof children === 'object' && !Array.isArray(children) ? cloneElement(children, { id: field, 'aria-describedby': `${field}-desc` }) : children;
  return html`<div class=${`settings-row${className ? ` ${className}` : ''}`}>
    <div class="settings-row-info"><label class="settings-label" for=${field}>${label}</label><span class="settings-desc" id=${`${field}-desc`}>${desc}</span></div>
    <div class="settings-control">${control}</div>
  </div>`;
}

export function SettingsGroup({ label = '', children }) {
  return html`<div class="settings-group" role=${label ? 'group' : undefined} aria-label=${label || undefined}>${children}</div>`;
}

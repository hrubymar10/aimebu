import { html } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { TABS, sectionTitle, tabHidden } from './model.js';

export function SettingsHeader({ store, onClose }) {
  return html`<h2 id="settings-title">${sectionTitle(store.section.value)}</h2><${IconButton} id="settings-close" name="close" label="Close settings" onClick=${onClose} />`;
}

export function SettingsNav({ store, onSelect }) {
  const section = store.section.value, rights = { owner: store.owner.value, admin: store.admin.value, usagesHidden: store.usagesHidden.value, users: store.users.value };
  return html`<div class="settings-nav-title" aria-hidden="true">Settings</div>${TABS.map(([key, label]) => {
    const hidden = tabHidden(key, rights);
    if (key === 'agents') return html`<div id="settings-agents-title" class="settings-nav-title settings-nav-group-title" aria-hidden="true" hidden=${hidden}>Agents</div>`;
    const pending = key === 'users' && rights.users === 'pending', selected = section === key;
    return html`<button id=${`settings-${key}-tab`} type="button" class=${`row-btn${selected ? ' settings-selected' : ''}${pending ? ' settings-tab-pending' : ''}`} hidden=${hidden}
      aria-current=${selected ? 'page' : undefined} aria-hidden=${pending ? 'true' : undefined} tabindex=${pending ? -1 : undefined} onClick=${() => onSelect(key)}>${label}</button>`;
  })}`;
}

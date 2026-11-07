import { html } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { TABS, sectionTitle, tabHidden } from './model.js';

// The back arrow is for a phone, where a section is a screen of its own (CSS hides it elsewhere).
export function SettingsHeader({ store, onClose, onBack }) {
  return html`<${IconButton} id="settings-back" className="settings-back-btn" name="left" label="Back to the sections" onClick=${onBack} /><h2 id="settings-title" class="settings-section-title">${sectionTitle(store.section.value)}</h2><${IconButton} id="settings-close" className="settings-close-btn" name="close" label="Close settings" onClick=${onClose} />`;
}

export function SettingsNav({ store, onSelect, onClose }) {
  const section = store.section.value, rights = { owner: store.owner.value, admin: store.admin.value, usagesHidden: store.usagesHidden.value, users: store.users.value };
  return html`<div class="settings-nav-head"><div class="settings-nav-title" aria-hidden="true">Settings</div><${IconButton} id="settings-nav-close" className="settings-nav-close" name="close" label="Close settings" onClick=${onClose} /></div>${TABS.map(([key, label]) => {
    const hidden = tabHidden(key, rights);
    if (key === 'agents') return html`<div id="settings-agents-title" class="settings-nav-title settings-nav-group-title" aria-hidden="true" hidden=${hidden}>Agents</div>`;
    const pending = key === 'users' && rights.users === 'pending', selected = section === key;
    return html`<button id=${`settings-${key}-tab`} type="button" class=${`settings-nav-item${selected ? ' active' : ''}${pending ? ' settings-tab-pending' : ''}`} hidden=${hidden}
      aria-current=${selected ? 'page' : undefined} aria-hidden=${pending ? 'true' : undefined} tabindex=${pending ? -1 : undefined} onClick=${() => onSelect(key)}>${label}</button>`;
  })}`;
}

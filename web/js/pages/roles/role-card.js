import { html } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { chipsOf, summaryOf } from './model.js';

export function RoleCard({ role, roles, selected, open, onEdit, onClose }) {
  return html`<article class=${`role-card${selected ? ' role-card-selected' : ''}`} data-role-id=${role.id} data-role-key=${role.role_key}>
    <div class="role-card-head">
      <span class="role-card-emoji" aria-hidden="true">${role.emoji || ''}</span>
      <strong class="role-card-label">${role.label}</strong>
      <code class="role-card-key">${role.role_key}</code>
      ${role.default_body_changed && html`<span class="role-default-marker" role="img" aria-label="The default prompt has changed since you edited this role" title="The default prompt has changed since you edited this role"><svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M12 3 2 21h20L12 3Z"/><path d="M12 9v5m0 3v1"/></svg></span>`}
    </div>
    <div class="role-card-chips">${chipsOf(role, roles).map(([text, title]) => html`<span class="role-chip" title=${title} key=${text}>${text}</span>`)}</div>
    <p class="role-card-summary">${summaryOf(role)}</p>
    <${IconButton} name=${open ? 'close' : 'edit'} label=${open ? `Close ${role.label} without saving` : `Edit ${role.label}`} className=${`role-card-edit${open ? ' role-card-close' : ''}`}
      data-role-label=${role.label} aria-controls="role-editor" aria-expanded=${String(open)} onClick=${open ? onClose : onEdit} />
  </article>`;
}

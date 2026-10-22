import { html } from '../../ui.js';

export function Activity({ activity }) {
  if (!activity) return null;
  if (!activity.length) return html`<p class="settings-intro">No harness changes recorded yet.</p>`;
  return activity.map((change, index) => html`
    <article class="machine-activity-item" key=${change.id || index}>
      <strong>Harness ${change.action || 'updated'}</strong>
      <span>${`${change.created_at ? new Date(change.created_at).toLocaleString() : ''}${change.actor ? ` · ${change.actor}` : ''}`}</span>
    </article>
  `);
}

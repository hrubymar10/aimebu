import { html } from '../../ui.js';

export function PendingAttachments({ attachments = [], uploads = 0, onRemove }) {
  return [
    attachments.map(attachment => html`<div class="attachment-pending-item ready" key=${attachment.id}>${attachment.mime.startsWith('image/')
      ? html`<img class="attachment-pending-thumb" src=${attachment.url} alt="Pending image" />` : html`<span class="attachment-pending-thumb" aria-hidden="true"></span>`}<div class="attachment-pending-meta"><div class="attachment-pending-name">${attachment.name || 'Attachment'}</div></div><button type="button" class="attachment-remove-btn" title="Remove file" aria-label="Remove file" onClick=${() => onRemove(attachment)}>×</button></div>`),
    uploads ? html`<div class="attachment-pending-item uploading"><span class="attachment-pending-thumb" aria-hidden="true"></span><div class="attachment-pending-meta"><div class="attachment-pending-status">${`Uploading ${uploads}…`}</div></div></div>` : null,
  ];
}

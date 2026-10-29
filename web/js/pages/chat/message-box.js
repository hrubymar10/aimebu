import { html } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';

export function PendingAttachments({ attachments = [], uploads = 0, onRemove }) {
  return [
    attachments.map(attachment => html`<span class="pending-image" key=${attachment.id}>${attachment.mime.startsWith('image/')
      ? html`<img src=${attachment.url} alt="Pending image" />` : html`<span>${attachment.name || 'Attachment'}</span>`}<${IconButton} name="close" label="Remove file" onClick=${() => onRemove(attachment)} /></span>`),
    uploads ? html`<span>${`Uploading ${uploads}…`}</span>` : null,
  ];
}

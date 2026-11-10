import { html, render } from '../ui.js';

// The small controls of the chat frame, as Preact components: the "Loading older …" status over a list and the icon buttons that bring a list back to its latest
// messages (a count of missed ones is a pill on the icon; the tooltip and accessible name carry the whole wording, "Back to latest · 2 new"), and the notes above a
// composer that say why it cannot be used, and the strip of a message's attachments. `Control` hosts one of them where chat.js wants it and keeps the few calls chat.js made on the element it replaces: `hidden`, `onclick`, `set`.

export function OlderStatus({ text, className, hidden }) {
  return html`<div class=${['older-status', className].filter(Boolean).join(' ')} role="status" hidden=${hidden}>${text}</div>`;
}

export function LatestButton({ label, count, className, hidden, onClick }) {
  return html`<button type="button" class=${['btn-icon', 'back-to-latest', className].filter(Boolean).join(' ')} title=${label} aria-label=${label} hidden=${hidden} onClick=${onClick}><span class="icon-mask icon-mask-arrow-down" aria-hidden="true"></span>${count ? html`<span class="badge">${String(count)}</span>` : null}</button>`;
}

export function RoomNote({ id, text, hidden }) {
  return html`<p class="system-room-notice" id=${id} role="note" hidden=${hidden}>${text}</p>`;
}

// What a message carries: an image as a thumbnail with its file name under it (its size, when known, keeps its place so the list does not jump as images load; a click
// opens it, see Chat.onMessageClick), another file as a download link, and a file that retention removed as a note where it was.
function Attachment({ attachment }) {
  const name = attachment.name;
  if (attachment.removed_at) {
    const days = attachment.removed_after_days;
    return html`<span class="attachment-removed" data-attachment-id=${attachment.id}>${`${name || (attachment.mime.startsWith('image/') ? 'Image' : 'Attachment')}: attachment removed${days > 0 ? ` after ${days} days` : ''}`}</span>`;
  }
  if (!attachment.mime.startsWith('image/')) return html`<a href=${attachment.url} class="attachment-file" download=${name || 'attachment'}>${name || 'Attachment'}</a>`;
  const sized = attachment.width > 0 && attachment.height > 0;
  return html`<figure class="image-item"><button type="button" class="row-btn image-thumb" aria-label="Open attached image" data-url=${attachment.url}><img src=${attachment.url} alt="Attached image" loading="lazy" width=${sized ? attachment.width : undefined} height=${sized ? attachment.height : undefined} /></button><figcaption title=${name || 'Image'}>${name || 'Image'}</figcaption></figure>`;
}

export function AttachmentStrip({ attachments }) {
  return html`<div class="message-images">${attachments.map(attachment => html`<${Attachment} key=${attachment.id} attachment=${attachment} />`)}</div>`;
}

// One control: a host element (it takes no part in the layout, so the control's own box is what the page's styles see) and the props its view draws, drawn at once on every change.
export class Control {
  constructor(View, props) {
    this.View = View; this.props = { hidden: false, ...props };
    this.host = document.createElement('div'); this.host.style.display = 'contents';
    this.paint();
  }
  paint() { render(html`<${this.View} ...${this.props} />`, this.host); }
  set(patch) { Object.assign(this.props, patch); this.paint(); }
  // The control's own element (the page's tests and styles find it by its class or id).
  get node() { return this.host.firstElementChild; }
  get hidden() { return this.props.hidden; }
  set hidden(value) { if (this.props.hidden !== !!value) this.set({ hidden: !!value }); }
  set onclick(handler) { this.set({ onClick: handler }); }
}

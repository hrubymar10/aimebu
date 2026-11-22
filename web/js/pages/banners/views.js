import { html, useLayoutEffect } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';

// The bars' contents. Each bar's element (its class, role, label and hidden state) is the adapter's (page.js); these draw only what is inside it.
// The listeners a bar needs are added by a layout effect, which runs inside the adapter's synchronous render, and removed when it unmounts.

const Glyph = ({ name }) => html`<span class=${`icon-mask icon-mask-${name}`} aria-hidden="true"></span>`;

export function UpdateBarBody({ model }) {
  useLayoutEffect(() => model.watch(), [model]);
  if (model.kind === 'stale') {
    return html`<span>A new version is ready, reload to continue.</span><${IconButton} name="refresh" label="Reload" className="update-bar-reload" onClick=${() => model.reload()} />`;
  }
  return html`<span>aimebu was updated. Reload to use the new version.</span><button type="button" class="btn btn-primary" onClick=${() => model.reload()}>Reload</button>`;
}

const part = item => (typeof item === 'string' ? item : item[0] === 'code' ? html`<code>${item[1]}</code>` : html`<strong>${item[1]}</strong>`);

export function UpdateBannerBody({ model }) {
  useLayoutEffect(() => () => model.stop(), [model]);
  return html`<${Glyph} name="health" /><span class="update-banner-text">${model.text.map(part)}</span>
    <${IconButton} name="copy" label="Copy the update command" className="update-banner-copy" onClick=${() => void model.copy()} />
    <${IconButton} name="bell" label="Remind me again in 24 hours" className="update-banner-remind" onClick=${() => void model.answer('remind')} />
    <${IconButton} name="close" label="Skip this version and state" className="update-banner-skip" onClick=${() => void model.answer('skip')} />`;
}

export function NotificationBannerBody({ model, doc, permissions }) {
  useLayoutEffect(() => model.watch(doc, permissions), [model]);
  return html`<${Glyph} name="bell" /><span class="notification-banner-text">Get a browser notification when something needs you.</span>
    <${IconButton} name="bell" label="Enable browser notifications" className="notification-banner-enable" disabled=${model.enabling} onClick=${() => void model.ask()} />
    <${IconButton} name="close" label="Dismiss this and do not ask again" className="notification-banner-dismiss" onClick=${() => model.close(true)} />`;
}

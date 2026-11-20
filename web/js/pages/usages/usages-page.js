import { html, useState, useLayoutEffect } from '../../ui.js';
import { useDirty } from '../../hooks/use-dirty.js';
import { SwitcherControls } from './switcher-controls.js';
import { QuotaBlock } from './quota-block.js';
import { GuardBlock } from './guard-block.js';
import { LimitDefault } from './limit-default.js';
import { UsagesView, UsagesActions } from './usages-view.js';

const TEXT_TYPES = new Set(['', 'text', 'number', 'url', 'password', 'search', 'email', 'tel']);
const isTyping = event => (event.target instanceof HTMLTextAreaElement) || (event.target instanceof HTMLInputElement && TEXT_TYPES.has(event.target.type));

// The whole Usages section, in the order it has always had. Its fields are uncontrolled (read when a Save is pressed), so what was typed and not
// saved is told from typing itself: any text typed in the section marks it, and a write that draws the fields again (a new revision of the usages,
// a default read or saved) clears the mark, since those fields start over from what the server holds. A save that leaves the fields as they are (the
// quota and guard blocks) leaves the mark: the section then stays in the page when Settings closes, which costs nodes and loses nothing.
export function UsagesPage({ page, onDirty = () => {} }) {
  const [typed, setTyped] = useState(false);
  const revision = page.store.revision.value, defaults = page.limitDefault.state.value, owner = page.store.owner.value;
  useLayoutEffect(() => { setTyped(false); }, [revision, defaults]);
  useDirty(onDirty, 'usages-typed', typed);
  return html`<div class="usages-page" style="display: contents" onInput=${event => { if (isTyping(event)) setTyped(true); }}>
    <div class="usages-head"><p class="settings-intro">Provider limits and account profiles</p><${UsagesActions} store=${page.store} /></div>
    ${!owner && html`<p id="usages-readonly" class="settings-intro usages-readonly" role="note">Only the owner can change Usages. You can see the limits here.</p>`}
    <p id="usages-error" class="error" role="alert">${page.store.error.value}</p>
    ${owner && html`<div class="usage-switcher-controls" id="usages-switcher"><${SwitcherControls} store=${page.store} /></div>
    <${QuotaBlock} store=${page.quota} />
    <${GuardBlock} store=${page.guard} />`}
    <${LimitDefault} store=${page.limitDefault} />
    <div id="usages-list"><${UsagesView} store=${page.store} /></div>
  </div>`;
}

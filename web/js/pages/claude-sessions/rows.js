import { html } from '../../ui.js';
import { sessionFacts, sessionFlags } from '../../claude-sessions.js';

// The pieces of a session row, shared by the picker and the import dialog (pages/claude-import/): its title with the facts under it, and its flags.
export function SessionMain({ session, title = session.title || 'Untitled session' }) {
  return html`<div class="cc-session-main"><span class="cc-session-title">${title}</span><span class="cc-session-facts">${sessionFacts(session).join(' · ')}</span>${session.outside_folder && html`<span class="cc-session-cwd">${session.cwd}</span>`}</div>`;
}

export function SessionFlags({ session, children }) {
  return html`<span class="cc-session-flags">${sessionFlags(session).map(([text, kind]) => html`<span class=${`cc-session-flag cc-session-flag-${kind}`}>${text}</span>`)}${children}</span>`;
}

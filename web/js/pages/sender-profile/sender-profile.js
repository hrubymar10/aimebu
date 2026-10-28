import { html } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { relativeTime } from '../../time.js';
import { messageRows, profileNote, profileRows, senderFacts, startNotes } from './model.js';

const date = value => (value ? `${relativeTime(value)} · ${new Date(value).toLocaleString()}` : '');

function Rows({ rows }) {
  return html`<div class="profile-facts-list">${rows.map(([label, value, mono]) => html`<div class="profile-fact" key=${label}><span class="profile-fact-label">${label}</span>${mono ? html`<code class="profile-fact-value">${value}</code>` : html`<span class="profile-fact-value">${value}</span>`}</div>`)}</div>`;
}

// The offer to start a historical agent as a v2 agent: a button, with what will happen written beside it, never automatic. Once started, the
// profile says so and points at the v2 agent instead.
function Start({ start, store, dialog }) {
  const { authorID, onStartAgent, onOpenAgent } = store.options;
  if (start.started_agent_id) {
    return html`<div class="sender-profile-start"><p class="sender-profile-note">${`Running as v2 agent ${start.started_agent_slug || ''}.`}</p>${onOpenAgent && html`<button type="button" class="btn sender-profile-open-agent" onClick=${() => { dialog.close(); onOpenAgent(start.started_agent_id); }}>${`Open ${start.started_agent_slug || 'agent'}`}</button>`}</div>`;
  }
  return html`<div class="sender-profile-start"><button type="button" class="btn sender-profile-start-button" id="sender-profile-start" title="Start as v2 agent" aria-label="Start as v2 agent" disabled=${!onStartAgent} onClick=${() => { dialog.close(); onStartAgent({ ...start, legacy_id: authorID }); }}><span class="icon-mask icon-mask-play" aria-hidden="true"></span>${' Start as v2 agent'}</button><p class="sender-profile-note">${startNotes(start)}</p></div>`;
}

export function SenderProfile({ store, dialog }) {
  const { name, authorID, kind, messages, identity } = store.options, phase = store.phase.value, profile = store.profile.value;
  const facts = senderFacts(messages, authorID);
  const extra = phase === 'done' ? profileRows(profile, date, relativeTime) : [];
  return html`<header class="sender-profile-header"><img class="sender-profile-icon" src=${identity.icon} alt="" width="28" height="28" /><h2 id="sender-profile-title">${name}</h2>
    <span class=${`sender-profile-badge sender-profile-${kind}`}>${kind === 'historical' ? 'Historical (imported)' : 'Departed'}</span><${IconButton} name="close" label="Close" className="sender-profile-close" onClick=${() => dialog.close()} /></header>
    <${Rows} rows=${messageRows(facts, identity.title, date)} />
    <div class="sender-profile-more" role="status">${phase === 'done' && html`<${Rows} rows=${extra} />${kind === 'historical' && profile.start && html`<${Start} start=${profile.start} store=${store} dialog=${dialog} />`}`}</div>
    <p class="sender-profile-note">${profileNote(kind, phase, profile)}</p>`;
}

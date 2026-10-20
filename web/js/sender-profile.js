// The read-only profile a message's sender name opens when that sender is not a live member any more: an agent from imported v1 history
// (a "historical" sender, known only by what it wrote) or one that left a v2 room ("departed"). It shows what is known: the runtime,
// when it was first and last heard from in this room, how many of its messages are loaded and, for a v2 agent the server still
// remembers, its session, resume command, working directory and branch. A historical sender's session is whatever the import kept from
// v1; when none was kept (or the viewer may not see host details) a note says that only what it wrote is shown.
import { request } from './api.js';
import { relativeTime } from './time.js';
import { setGlyph } from './icon-button.js';

function node(tag, className = '', text = '') { const item = document.createElement(tag); if (className) item.className = className; if (text) item.textContent = text; return item; }

// What the loaded messages say about one sender.
export function senderFacts(messages, authorID) {
  const mine = [...messages].filter(message => message.author_id === authorID && message.author_kind === 'agent').sort((a, b) => a.seq - b.seq);
  const withModel = [...mine].reverse().find(message => message.author_model || message.author_harness);
  return { count: mine.length, first: mine[0]?.created_at || '', last: mine.at(-1)?.created_at || '', model: withModel?.author_model || '', harness: withModel?.author_harness || '' };
}

// The offer to start a historical agent as a v2 agent. Never automatic: a button, with what will happen written beside it. Once started, the
// profile says so and points at the v2 agent instead.
function startSection(start, authorID, dialog, onStartAgent, onOpenAgent) {
  const box = node('div', 'sender-profile-start');
  if (start.started_agent_id) {
    box.append(node('p', 'sender-profile-note', `Running as v2 agent ${start.started_agent_slug || ''}.`));
    if (onOpenAgent) { const open = node('button', 'btn sender-profile-open-agent', `Open ${start.started_agent_slug || 'agent'}`); open.type = 'button'; open.onclick = () => { dialog.close(); onOpenAgent(start.started_agent_id); }; box.append(open); }
    return box;
  }
  const button = node('button', 'btn sender-profile-start-button'); button.type = 'button'; button.id = 'sender-profile-start';
  setGlyph(button, 'play', 'Start as v2 agent'); button.append(document.createTextNode(' Start as v2 agent'));
  button.disabled = !onStartAgent;
  button.onclick = () => { dialog.close(); onStartAgent({ ...start, legacy_id: authorID }); };
  const notes = [start.resumable ? `Continues the v1 session in ${start.cwd}.` : start.reason];
  if (start.resumable && start.outside_folder) notes.push('That folder is outside the project folder; the owner approves it first.');
  if (start.v1_active) notes.push('v1 saw it a moment ago and may still be using the session.');
  box.append(button, node('p', 'sender-profile-note', notes.filter(Boolean).join(' ')));
  return box;
}

// `kind` is "historical" (imported from v1) or "departed" (a v2 agent that left).
// onStartAgent(start) opens the spawn dialog for a historical sender; onOpenAgent(id) shows a v2 agent that was already started from it.
export async function openSenderProfile({ name, authorID, kind, roomID, messages, identity, onStartAgent = null, onOpenAgent = null }) {
  document.querySelector('#sender-profile-dialog')?.remove();
  const dialog = node('dialog', 'dialog-card sender-profile'); dialog.id = 'sender-profile-dialog'; dialog.setAttribute('aria-labelledby', 'sender-profile-title');
  const header = node('header', 'sender-profile-header');
  const icon = node('img', 'sender-profile-icon'); icon.src = identity.icon; icon.alt = ''; icon.width = 28; icon.height = 28;
  const title = node('h2', '', name); title.id = 'sender-profile-title';
  const badge = node('span', `sender-profile-badge sender-profile-${kind}`, kind === 'historical' ? 'Historical (imported)' : 'Departed');
  const close = node('button', 'icon-btn sender-profile-close'); close.type = 'button'; setGlyph(close, 'close', 'Close'); close.onclick = () => dialog.close();
  header.append(icon, title, badge, close);
  const list = node('div', 'profile-facts-list');
  const fact = (label, value, mono) => { if (!value) return; const row = node('div', 'profile-fact'); row.append(node('span', 'profile-fact-label', label)); const text = node(mono ? 'code' : 'span', 'profile-fact-value', value); row.append(text); list.append(row); return row; };
  const facts = senderFacts(messages, authorID);
  const date = value => (value ? `${relativeTime(value)} · ${new Date(value).toLocaleString()}` : '');
  fact('Runtime', identity.title.replace(/ \((departed|historical)\)$/, ''));
  fact('Model', facts.model); fact('Harness', facts.harness);
  fact('First heard', date(facts.first)); fact('Last heard', date(facts.last));
  fact('Messages loaded', facts.count ? String(facts.count) : '');
  // A departed agent's note is plain; a historical one is filled in once the profile is fetched, because what it says depends on it.
  const note = node('p', 'sender-profile-note', kind === 'historical' ? '' : 'This agent is no longer in the room.');
  const noSession = () => { if (kind === 'historical') note.textContent = 'This sender comes from imported v1 history, and no session was kept for it, so only what it wrote is shown.'; };
  const more = node('div', 'sender-profile-more'); more.setAttribute('role', 'status');
  dialog.append(header, list, more, note);
  dialog.addEventListener('close', () => dialog.remove());
  document.body.append(dialog); dialog.showModal();
  // A v2 agent the server still knows, or a v1 agent whose session the import kept, has a session, a resume command and a working directory.
  if (roomID) {
    try {
      const profile = await request(`/api/rooms/${roomID}/agents/${authorID}/profile`);
      if (!dialog.isConnected) return;
      const extra = node('div', 'profile-facts-list');
      const add = (label, value, mono) => {
        if (!value) return; const row = node('div', 'profile-fact'); row.append(node('span', 'profile-fact-label', label), node(mono ? 'code' : 'span', 'profile-fact-value', value));
        extra.append(row);
      };
      add('Session', profile.native_session_id, true); add('Resume', profile.resume_command, true); add('CWD', profile.cwd, true); add('Branch', profile.branch, true);
      add('Machine', profile.machine_name); add('Project', profile.project_name); if (profile.imported) add('Last active in v1', date(profile.last_seen)); add('Spawned', profile.spawned_by ? `${profile.spawned_by}${profile.spawned_at ? ` · ${relativeTime(profile.spawned_at)}` : ''}` : '');
      more.replaceChildren(extra);
      if (kind === 'historical' && profile.start) more.append(startSection(profile.start, authorID, dialog, onStartAgent, onOpenAgent));
      if (!profile.native_session_id && !profile.resume_command && !profile.cwd) noSession(); else if (kind === 'historical') note.textContent = 'This sender comes from imported v1 history.';
    } catch { noSession(); /* the server has no session for it (or the viewer may not see host details): the facts above stand */ }
  } else noSession();
  return dialog;
}

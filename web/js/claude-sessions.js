// What the Claude Code session lists show of a session: pure helpers for the picker (pages/claude-sessions/) and the import dialog
// (pages/claude-import/). Only the title Claude gave a session is shown, never what was said in it.
import { relativeTime } from './time.js';

export function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(bytes < 10 * 1024 * 1024 ? 1 : 0)} MB`;
}

// The facts shown under a session's title, in order.
export function sessionFacts(session) {
  const facts = [];
  if (session.git_branch && session.git_branch !== 'HEAD') facts.push(session.git_branch); // "HEAD" is no branch: a detached HEAD, or a folder that is not a repository
  if (session.modified_at) facts.push(`used ${relativeTime(session.modified_at)}`);
  facts.push(formatSize(session.size || 0));
  facts.push(session.id.slice(0, 8));
  return facts;
}

// The flags a session carries, as [text, kind]. An agent that continues a session only rules out continuing it a second time: a copy of it can
// still be worked on, so it warns and does not stop.
export function sessionFlags(session) {
  const flags = [];
  if (session.agent) flags.push([`continued by ${session.agent.slug}`, 'note']); // a fact, not a danger: amber is kept for what can go wrong (a terminal has it open)
  if (session.active) flags.push(['open in a terminal', 'warn']);
  if (session.outside_folder) flags.push(['outside the project folder', 'note']);
  if (session.entrypoint === 'sdk-cli') flags.push(['started by an agent', 'note']);
  return flags;
}

// What a row is called. Claude gives a session its own title, and the sessions an agent starts are all titled alike ("aimebu turn"): a list of such rows cannot be told
// apart, so a title that more than one row has says when the session was last used, to the minute ("aimebu turn · Nov 16, 22:41").
export function rowTitles(sessions) {
  const seen = new Map();
  for (const session of sessions) { const title = session.title || 'Untitled session'; seen.set(title, (seen.get(title) || 0) + 1); }
  return sessions.map(session => {
    const title = session.title || 'Untitled session', at = new Date(session.modified_at || '');
    if (seen.get(title) < 2 || Number.isNaN(at.getTime())) return title;
    return `${title} · ${at.toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}`;
  });
}

// The line above the list: how many sessions, and what the scan could not show (transcripts it could not read, a list cut at its limits).
export function listingStatus(count, empty, result = {}) {
  const notes = [];
  if (result.truncated) notes.push('only the newest are shown');
  if (result.unreadable) notes.push(`${result.unreadable} ${result.unreadable === 1 ? 'session' : 'sessions'} could not be read`);
  const base = count ? `${count} ${count === 1 ? 'session' : 'sessions'}` : empty;
  return notes.length ? `${base} (${notes.join('; ')})` : base;
}

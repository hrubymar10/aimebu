// The picker of the Claude Code sessions a machine already has: the ones that worked in the project's folder (found by the machine's
// supervisor from Claude's own files), or one pasted by id. Picking one hands it to the Spawn dialog, which starts an agent that continues
// it. Only the title Claude gave the session is shown, never what was said in it.
import { setGlyph } from './icon-button.js';
import { relativeTime } from './time.js';

function node(tag, className = '', text = '') { const item = document.createElement(tag); if (className) item.className = className; if (text) item.textContent = text; return item; }

export function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(bytes < 10 * 1024 * 1024 ? 1 : 0)} MB`;
}

// The facts shown under a session's title, in order.
export function sessionFacts(session) {
  const facts = [];
  if (session.git_branch) facts.push(session.git_branch);
  if (session.modified_at) facts.push(`used ${relativeTime(session.modified_at)}`);
  facts.push(formatSize(session.size || 0));
  facts.push(session.id.slice(0, 8));
  return facts;
}

// The flags a session carries, as [text, kind]. An agent that continues a session only rules out continuing it a second time: a copy of it can
// still be worked on, so it warns and does not stop.
export function sessionFlags(session) {
  const flags = [];
  if (session.agent) flags.push([`continued by ${session.agent.slug}`, 'warn']);
  if (session.active) flags.push(['open in a terminal', 'warn']);
  if (session.outside_folder) flags.push(['outside the project folder', 'note']);
  if (session.entrypoint === 'sdk-cli') flags.push(['started by an agent', 'note']);
  return flags;
}

// The line above the list: how many sessions, and what the scan could not show (transcripts it could not read, a list cut at its limits).
export function listingStatus(count, empty, result = {}) {
  const notes = [];
  if (result.truncated) notes.push('only the newest are shown');
  if (result.unreadable) notes.push(`${result.unreadable} ${result.unreadable === 1 ? 'session' : 'sessions'} could not be read`);
  const base = count ? `${count} ${count === 1 ? 'session' : 'sessions'}` : empty;
  return notes.length ? `${base} (${notes.join('; ')})` : base;
}

export class ClaudeSessionPicker {
  constructor(source) {
    this.source = source;
    this.dialog = document.querySelector('#cc-sessions-dialog');
    this.list = document.querySelector('#cc-sessions-list');
    this.status = document.querySelector('#cc-sessions-status');
    this.error = document.querySelector('#cc-sessions-error');
    this.find = document.querySelector('#cc-sessions-find');
    this.input = document.querySelector('#cc-session-id');
    setGlyph(document.querySelector('#cc-session-find'), 'select-all', 'Find a session by its id');
    document.querySelector('#cc-sessions-close').onclick = () => this.dialog.close();
    this.find.onsubmit = event => { event.preventDefault(); void this.byID(); };
    this.generation = 0;
  }
  // open({ projectID, machineID, machineName, onPick }): lists the project's sessions on that machine; onPick(session) is called with the
  // chosen one (a row of the server's list, plus machine_id).
  async open({ projectID, machineID, machineName, launcherID = '', onPick }) {
    this.projectID = projectID; this.machineID = machineID; this.launcherID = launcherID; this.onPick = onPick;
    this.input.value = ''; this.error.textContent = ''; this.list.replaceChildren();
    document.querySelector('#cc-sessions-intro').textContent = `Sessions on ${machineName || 'this machine'} that worked in this project's folder. The agent continues the session where it was made; nothing of what was said in it is copied here.`;
    this.dialog.showModal();
    const generation = ++this.generation;
    this.status.textContent = 'Looking for sessions…';
    try {
      const result = await this.source.claudeSessions(projectID, { machineID, launcherID });
      if (generation !== this.generation || !this.dialog.open) return;
      this.launcherID = result.launcher_id || '';
      this.render(result.sessions || [], 'There are no Claude Code sessions for this project\'s folder on this machine.', result);
    } catch (error) {
      if (generation !== this.generation || !this.dialog.open) return;
      this.status.textContent = ''; this.error.textContent = error.message || 'Could not list the sessions.';
    }
  }
  async byID() {
    const id = this.input.value.trim();
    if (!id) return;
    const generation = ++this.generation;
    this.error.textContent = ''; this.status.textContent = 'Looking for that session…';
    try {
      const result = await this.source.claudeSessions(this.projectID, { machineID: this.machineID, launcherID: this.launcherID, sessionID: id });
      if (generation !== this.generation || !this.dialog.open) return;
      this.launcherID = result.launcher_id || this.launcherID;
      this.render(result.sessions || [], 'No session with that id.');
    } catch (error) {
      if (generation !== this.generation || !this.dialog.open) return;
      this.list.replaceChildren(); this.status.textContent = ''; this.error.textContent = error.message || 'Could not find that session.';
    }
  }
  // `result` carries what a folder listing could not show: transcripts it could not read, and a listing cut at its limits.
  render(sessions, empty, result = {}) {
    this.error.textContent = '';
    this.status.textContent = listingStatus(sessions.length, empty, result);
    this.list.replaceChildren(...sessions.map(session => this.row(session)));
  }
  row(session) {
    const row = node('div', 'cc-session'); row.dataset.sessionId = session.id;
    const main = node('div', 'cc-session-main');
    main.append(node('span', 'cc-session-title', session.title || 'Untitled session'), node('span', 'cc-session-facts', sessionFacts(session).join(' · ')));
    if (session.outside_folder) main.append(node('span', 'cc-session-cwd', session.cwd));
    const flags = node('span', 'cc-session-flags');
    const sessionFlagList = sessionFlags(session);
    for (const [text, kind] of sessionFlagList) flags.append(node('span', `cc-session-flag cc-session-flag-${kind}`, text));
    const pick = node('button', 'icon-btn cc-session-pick'); pick.type = 'button';
    setGlyph(pick, 'play', session.agent ? `Continued by ${session.agent.slug}: work on a copy of this session` : 'Continue this session or work on a copy');
    pick.onclick = () => { this.dialog.close(); this.onPick?.({ ...session, machine_id: this.machineID, launcher_id: this.launcherID }); };
    row.append(main, flags, pick);
    return row;
  }
}

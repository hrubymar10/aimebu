import { html } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { rowTitles } from '../../claude-sessions.js';
import { SessionFlags, SessionMain } from './rows.js';

// The body of #cc-sessions-dialog. The id box is left to the browser and read when the form is sent.
export function PickerDialog({ store, inputRef }) {
  const sessions = store.sessions.value, titles = rowTitles(sessions), machineName = store.machineName.value;
  const find = event => { event.preventDefault(); void store.byID(inputRef.current?.value); };
  return html`<h2 id="cc-sessions-title">Continue a Claude Code session</h2>
    <p id="cc-sessions-intro" class="cc-sessions-intro">${`Sessions on ${machineName || 'this machine'} that worked in this project's folder. The agent continues the session where it was made; nothing of what was said in it is copied here.`}</p>
    <p id="cc-sessions-status" class="cc-sessions-status" role="status">${store.status.value}</p>
    <div id="cc-sessions-list" class="cc-sessions-list">${sessions.map((session, index) => html`<div key=${session.id} class="cc-session" data-session-id=${session.id}>
      <${SessionMain} session=${session} title=${titles[index]} /><${SessionFlags} session=${session} />
      <${IconButton} name="play" label=${session.agent ? `Continued by ${session.agent.slug}: work on a copy of this session` : 'Continue this session or work on a copy'} className="cc-session-pick" onClick=${() => store.pick(session)} /></div>`)}</div>
    <form id="cc-sessions-find" class="cc-sessions-find" onSubmit=${find}><input id="cc-session-id" name="session_id" placeholder="Or paste a session id" autocomplete="off" autocapitalize="none" spellcheck="false" maxlength="64" aria-label="Claude Code session id" ref=${inputRef} /><${IconButton} type="submit" id="cc-session-find" name="select-all" label="Find a session by its id" /></form>
    <p id="cc-sessions-error" class="error" role="alert">${store.error.value}</p>
    <div class="cc-sessions-actions"><button id="cc-sessions-close" type="button" class="btn" onClick=${() => store.dialog.close()}>Close</button></div>`;
}

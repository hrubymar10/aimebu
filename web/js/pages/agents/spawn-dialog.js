import { html, useState, useEffect, useRef } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { toolRows } from '../../spawn-model.js';
import { MachineFields } from './machine-fields.js';

// The reason line under the tool: plain words, or words with Settings → Harnesses as a button that opens it.
function Reason({ id, className, text, store, host }) {
  const parts = store.hint(text);
  return html`<p id=${id} class=${className}>${parts.link ? html`${parts.before}<button type="button" class="inline-link" onClick=${() => host.openSettings(store.machine.id.peek())}>${parts.link}</button>${parts.after}` : parts.text}</p>`;
}

// The "Continue a session" entry of the Spawn dialog: a ⋯ button in the title row that opens a menu of one item. It is a menu, not a button, so that
// more entries can join it. (No other entry point to the flow exists: the agent cards and the session history reopen an agent that was already started,
// and Import Claude Code sessions makes agents from many sessions at once.) Arrow keys have nowhere to go with one item; Escape closes the menu (not
// the dialog) and gives focus back to the button, Tab or a click outside closes it.
function ContinueMenu({ enabled, onContinue }) {
  const [open, setOpen] = useState(false);
  const root = useRef(null), item = useRef(null), toggle = useRef(null);
  useEffect(() => {
    if (!open) return undefined;
    item.current?.focus();
    const outside = event => { if (!root.current?.contains(event.target)) setOpen(false); };
    document.addEventListener('pointerdown', outside, true);
    return () => document.removeEventListener('pointerdown', outside, true);
  }, [open]);
  const keys = event => {
    if (event.key === 'Escape' && open) { event.preventDefault(); event.stopPropagation(); setOpen(false); toggle.current?.focus(); }
    else if (event.key === 'Tab' && open) setOpen(false);
  };
  return html`<span class="spawn-continue-menu" ref=${root} onKeyDown=${keys}>
    <button type="button" id="spawn-sessions-menu" ref=${toggle} class="btn-icon" title="More" aria-label="More" aria-haspopup="menu" aria-expanded=${String(open)} onClick=${() => setOpen(!open)}><span class="icon-mask icon-mask-more" aria-hidden="true"></span></button>
    <div class="spawn-continue-list" role="menu" hidden=${!open}><button type="button" id="spawn-sessions" ref=${item} role="menuitem" class="row-btn" disabled=${!enabled} onClick=${() => { setOpen(false); onContinue(); }}>Continue a Claude Code session…</button></div>
  </span>`;
}

// The Spawn dialog's form. The dialog element and its static frame stay in index.html; the adapter shows and closes it.
export function SpawnForm({ store, host }) {
  const resume = store.resume.value, continuing = resume?.kind === 'claude_session', banner = store.banner.value, machine = store.machine, view = store.worktreeView.value;
  const room = host.room, disabled = store.toolsDisabled.value, tools = toolRows(store.catalog.value, disabled), sessions = store.sessions.value;
  const sessionsHidden = !!resume || room?.kind === 'dm' || !!sessions.hidden;
  const swap = sessions.switchTo >= 0 ? store.catalog.value[sessions.switchTo] : null;
  const fieldsOf = form => ({ name: form.elements.name.value, baseRef: form.elements.base_ref.value, mode: form.elements.resume_mode.value, confirmed: form.querySelector('#spawn-resume-confirm').checked });
  const submit = event => { event.preventDefault(); store.submit(fieldsOf(event.currentTarget)); };
  const entry = sessionsHidden ? null : html`<${ContinueMenu} enabled=${sessions.enabled} onContinue=${() => store.openSessionPicker()} />`;
  return html`<form id="spawn-form" onSubmit=${submit}><div class="spawn-title-row"><h2>${banner.title}</h2>${entry}</div>
    <p id="spawn-sessions-note" class="spawn-sessions-note" role="status" hidden=${sessionsHidden || !sessions.note}><span id="spawn-sessions-note-text">${(() => { const parts = store.hint(sessions.note); return parts.link ? html`${parts.before}<button type="button" class="inline-link" onClick=${() => host.openSettings(store.machine.id.peek())}>${parts.link}</button>${parts.after}` : parts.text; })()}</span>${' '}<button type="button" id="spawn-sessions-switch" class="btn" hidden=${!swap} onClick=${() => store.useTool(sessions.switchTo)}>${swap ? `Use ${swap.label || 'the native launcher'}` : ''}</button></p>
    <${MachineFields} machine=${machine} prefix="spawn" />
    <div id="spawn-resume" class="spawn-resume" hidden=${banner.hidden}><p id="spawn-resume-text">${banner.text}</p>
      <dl id="spawn-resume-card" class="spawn-resume-card" hidden=${!banner.card}>${(banner.card || []).map(([name, value, mono]) => html`<dt key=${`t${name}`}>${name}</dt><dd key=${`d${name}`} class=${mono ? 'mono' : null}>${value}</dd>`)}</dl>
      <fieldset id="spawn-resume-mode" class="spawn-resume-mode" hidden=${!banner.choosing}><legend>How</legend><label title="Continue the original session"><input type="radio" name="resume_mode" value="continue" checked=${store.mode.value === 'continue'} disabled=${banner.held} onChange=${() => store.pickMode('continue')} /> Continue</label><label title="Work on a copy (fork): the original stays as it is"><input type="radio" name="resume_mode" value="fork" checked=${store.mode.value === 'fork'} onChange=${() => store.pickMode('fork')} /> Work on a copy</label></fieldset>
      <p id="spawn-resume-notes" class="spawn-resume-notes">${store.notesOverride.value || banner.notes}</p>
      <label id="spawn-resume-confirm-label" hidden=${banner.confirmHidden}><input type="checkbox" id="spawn-resume-confirm" checked=${store.confirmed.value} onChange=${event => store.pickConfirmed(event.currentTarget.checked)} />${banner.confirmText}</label></div>
    <label id="spawn-start-in" hidden=${!resume}>Start in<select name="start_room" id="spawn-start-room" value=${store.startRoom.value} onChange=${event => store.pickStartRoom(event.currentTarget.value)}>${store.startOptions.value.map(option => html`<option key=${option.value} value=${option.value}>${option.label}</option>`)}</select></label>
    <label class="spawn-worktree-option" hidden=${view.hidden}><input type="checkbox" name="worktree" checked=${store.worktree.value} disabled=${view.disabled} onChange=${event => { store.worktree.value = event.currentTarget.checked; }} /><span>Use a Git worktree for this task room</span><small class="spawn-worktree-note" hidden=${!view.refused}>${view.note}</small></label>
    <label class="spawn-worktree-base" hidden=${!store.worktree.value || view.refused}>Base branch or ref (optional)<input name="base_ref" placeholder="Current HEAD" value=${store.baseRef.value} onInput=${event => { store.baseRef.value = event.currentTarget.value; }} /></label>
    <div id="spawn-agent-fields"><div class="spawn-name" hidden=${!!resume && !continuing}><label for="spawn-name">Name</label><div class="spawn-name-row"><input id="spawn-name" name="name" autocomplete="off" autocapitalize="none" spellcheck="false" maxlength="21" placeholder="Automatic" aria-describedby="spawn-name-error" value=${store.name.value} onInput=${event => store.typeName(event.currentTarget.value)} /><${IconButton} name="dice" label="Another name" id="spawn-name-dice" disabled=${store.nameBusy.value} onClick=${() => store.fillName()} /></div><p id="spawn-name-error" class="error spawn-name-error" role="alert">${store.nameError.value}</p></div>
      <label>Role<select name="role" value=${store.role.value} onChange=${event => store.pickRole(event.currentTarget.value)}>${store.roles.value.map(role => html`<option key=${role.value} value=${role.value}>${role.label}</option>`)}</select></label>
      <p id="spawn-home-hint" class="spawn-tool-reason" hidden=${!store.leadHint.value}>The home room is for the lead. Spawn workers into a task room such as general.</p>
      <label>Tool<select name="tool" required value=${store.toolIndex.value < 0 ? '' : String(store.toolIndex.value)} onChange=${event => store.pickTool(event.currentTarget.value)}>${tools.map((row, index) => html`<option key=${row.value} value=${row.value} disabled=${row.disabled || !!disabled?.[index]}>${row.label}</option>`)}</select></label>
      <${Reason} id="spawn-tool-reason" className="spawn-tool-reason" text=${store.reasonText.value} store=${store} host=${host} /><p id="spawn-version" class="spawn-version">${store.version.value}</p>
      <label>Model<select name="model" required value=${store.model.value} onChange=${event => store.pickModel(event.currentTarget.value)}>${store.models.value.map(model => html`<option key=${model.value} value=${model.value} ...${model.note ? { title: model.note } : {}}>${model.label}</option>`)}</select></label></div>
    <div class="dialog-actions"><button type="button" id="spawn-cancel" class="btn" onClick=${() => host.spawnDialog.close()}>Cancel</button><button type="submit" class="btn btn-primary" disabled=${store.submitDisabled.value}>Spawn</button></div></form>`;
}

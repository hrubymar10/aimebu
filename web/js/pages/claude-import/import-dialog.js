import { html, useLayoutEffect } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { sessionFacts, sessionFlags } from '../../claude-sessions.js';

function Row({ store, session, preset, roles, locked }) {
  const state = store.states.value.get(session.id), done = store.started.value.has(session.id);
  return html`<div class="cc-session cc-import-row" data-session-id=${session.id} data-started=${done ? '1' : undefined}>
    <input type="checkbox" aria-label=${`Import ${session.title || 'untitled session'}`} checked=${store.ticked.value.has(session.id)} disabled=${locked || done} onChange=${event => store.tick(session.id, event.currentTarget.checked)} />
    <div class="cc-session-main"><span class="cc-session-title">${session.title || 'Untitled session'}</span><span class="cc-session-facts">${sessionFacts(session).join(' · ')}</span>${session.outside_folder && html`<span class="cc-session-cwd">${session.cwd}</span>`}</div>
    <span class="cc-session-flags">${sessionFlags(session).map(([text, kind]) => html`<span class=${`cc-session-flag cc-session-flag-${kind}`}>${text}</span>`)}${session.outside_folder && html`<span class="cc-session-flag cc-session-flag-warn">needs approval</span>`}</span>
    <select class="cc-import-role" aria-label="Role" disabled=${locked}>${roles.map(role => html`<option value=${role.role_key} selected=${role.role_key === preset}>${role.label || role.role_key}</option>`)}</select>
    <span class=${`cc-import-state${state?.error ? ' error' : ''}`} role="status">${state?.text || ''}</span></div>`;
}

// The role boxes are left to the browser and read when the import starts.
export function ImportDialog({ store }) {
  const locked = store.running.value, tools = store.tools.value, tool = tools[Number(store.harness.value)];
  useLayoutEffect(() => {
    const dialog = store.dialog;
    const cancel = event => { if (store.running.peek()) event.preventDefault(); };
    const closed = () => { if (store.running.peek()) dialog.showModal(); };
    dialog.addEventListener('cancel', cancel); dialog.addEventListener('close', closed);
    return () => { dialog.removeEventListener('cancel', cancel); dialog.removeEventListener('close', closed); };
  }, [store]);
  const start = () => {
    const roles = {};
    for (const row of store.dialog.querySelectorAll('.cc-import-row')) roles[row.dataset.sessionId] = row.querySelector('select').value;
    store.start(roles);
  };
  return html`<h2 id="cc-import-title">Import Claude Code sessions</h2>
    <p class="cc-sessions-intro">Pick the sessions a machine has for this project's folder. Each becomes an agent in a room of its own, working on a copy of its session (the originals stay as they are), idle until it is sent a message.</p>
    <div class="cc-import-pickers">
      <label>Machine<select id="cc-import-machine" value=${store.machine.value} disabled=${locked} onChange=${event => store.selectMachine(event.currentTarget.value)}>${store.machines.value.map(machine => html`<option value=${machine.id} disabled=${machine.disabled}>${machine.label}</option>`)}</select></label>
      <label>Harness<select id="cc-import-harness" value=${store.harness.value} disabled=${locked} onChange=${event => store.selectHarness(event.currentTarget.value)}>${tools.map((item, index) => html`<option value=${String(index)}>${item.label || 'Claude Code'}</option>`)}</select></label>
      <label>Model<select id="cc-import-model" value=${store.model.value} disabled=${locked} onChange=${event => { store.model.value = event.currentTarget.value; }}>${(tool?.models || []).map(model => html`<option value=${model.id}>${model.label || model.id}</option>`)}</select></label></div>
    <div class="cc-import-bar"><${IconButton} id="cc-import-all" name="select-all" label="Select all or none" onClick=${() => store.selectAll()} /><p id="cc-import-status" class="cc-sessions-status" role="status">${store.status.value}</p></div>
    <div id="cc-import-list" class="cc-sessions-list">${store.sessions.value.map((session, index) => html`<${Row} key=${session.id} store=${store} session=${session} preset=${store.presets.value[index]} roles=${store.roles.value} locked=${locked} />`)}</div>
    <p id="cc-import-error" class="error" role="alert">${store.error.value}</p>
    <div class="cc-sessions-actions"><${IconButton} id="cc-import-run" name="play" label="Import the selected sessions as agents" disabled=${!store.canRun()} onClick=${start} /><button id="cc-import-close" type="button" class="btn" disabled=${locked} onClick=${() => store.dialog.close()}>Close</button></div>`;
}

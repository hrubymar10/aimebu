import { html } from '../../ui.js';

// The machine a dialog works on and the project's folder on it; `prefix` gives the ids the Spawn and Switch dialogs keep.
export function MachineFields({ machine, prefix }) {
  return html`<label>Machine<select name="machine" id=${`${prefix}-machine`} required disabled=${machine.disabled.value} value=${machine.id.value} onChange=${event => machine.pick(event.currentTarget.value)}>${machine.machines.value.map(item => html`<option key=${item.id} value=${item.id} disabled=${!!item.revoked_at || !item.approved}>${`${item.name}${item.is_host ? ' · Host' : ''}${item.revoked_at ? ' · Revoked' : !item.approved ? ' · Pending approval' : !item.connected ? ' · Offline' : !item.project_allowed ? ' · Project approval needed' : ''}`}</option>`)}</select></label>
    <div id=${`${prefix}-machine-folder`} class="machine-folder" hidden=${!machine.folderShown}><label>Project folder on this machine<input name="machine_folder" placeholder="/absolute/path/to/project" value=${machine.folder.value} onInput=${event => machine.edit(event.currentTarget.value)} /></label><button type="button" id=${`${prefix}-save-folder`} class="btn" disabled=${machine.saving.value} onClick=${() => machine.saveFolder()}>Save folder</button><p id=${`${prefix}-folder-status`} role="status">${machine.status.value}</p></div>`;
}

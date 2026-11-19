import { html } from '../../ui.js';
import { switchRows } from '../../spawn-model.js';
import { MachineFields } from './machine-fields.js';
import { ModelPicker } from './model-picker.js';

// The reason line under the tool: plain words, or words with Settings → Harnesses & models as a button that opens it.
function Reason({ text, store, host }) {
  const parts = store.hint(text);
  return html`<p id="switch-tool-reason" class="spawn-tool-reason">${parts.link ? html`${parts.before}<button type="button" class="inline-link" onClick=${() => host.openSettings(store.machine.id.peek())}>${parts.link}</button>${parts.after}` : parts.text}</p>`;
}

// The Switch dialog's form. The dialog element and its frame stay in index.html; the adapter shows and closes it.
export function SwitchForm({ store, host }) {
  const tools = switchRows(store.catalog.value);
  const preview = store.preview.value;
  return html`<form id="switch-form" onSubmit=${event => { event.preventDefault(); store.submit(); }}><h2 id="switch-title">Change agent tool or model</h2><p id="switch-agent-name">${store.agentName.value}</p>
    <${MachineFields} machine=${store.machine} prefix="switch" />
    <label>Tool<select name="tool" required value=${store.toolIndex.value < 0 ? '' : String(store.toolIndex.value)} onChange=${event => store.pickTool(event.currentTarget.value)}>${tools.map(row => html`<option key=${row.value} value=${row.value} disabled=${row.disabled}>${row.label}</option>`)}</select></label>
    <${Reason} text=${store.reason.value} store=${store} host=${host} /><p id="switch-version" class="spawn-version">${store.version.value}</p>
    <${ModelPicker} key=${store.toolIndex.value} prefix="switch" models=${store.models.value} value=${store.model.value} onChange=${value => store.pickModel(value)} />
    <p class="switch-note">The agent keeps its identity, role, and rooms. The tool must be available on the selected machine. A remote machine needs a project folder mapping.</p>
    ${preview && html`<section class="switch-preview" aria-label="Switch preview"><h3>What the new agent will receive</h3><p>${preview.handoff_status === 'automatic' ? 'Automatic handoff summary' : 'Handoff from the current agent'} · Last ${preview.history_window} room messages · ${preview.handbook_included ? 'Project handbook' : 'No handbook for this role'}</p><p>${preview.handoff_status === 'automatic' ? 'The current agent cannot prepare a handoff. The server will summarize recent room history.' : 'The current agent can prepare a handoff after finishing its turn. If it cannot, the server will summarize recent room history.'}</p></section>`}
    <p id="switch-error" class="error" role="alert">${store.error.value}</p>
    <div class="dialog-actions"><button type="button" id="switch-cancel" class="btn" disabled=${store.sending.value} onClick=${() => host.switchDialog.close()}>Cancel</button><button type="submit" class="btn btn-primary" disabled=${store.submitDisabled.value}>${store.sending.value ? 'Working…' : preview ? 'Confirm switch' : 'Preview switch'}</button></div></form>`;
}

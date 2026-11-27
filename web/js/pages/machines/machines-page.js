import { html } from '../../ui.js';
import { confirmDialog } from '../../dialogs.js';
import { IconButton } from '../../components/icon-button.js';
import { cardView, instanceView } from './model.js';
import { absoluteTime } from '../../time.js';

function MachineCard({ store, machine }) {
  const view = cardView(machine, store.owner.value);
  const instances = store.instances.value[machine.id] || [];
  const lock = view.locked ? label => ({ disabled: true, title: `Only the owner can ${label.toLowerCase()} machines` }) : () => ({});
  return html`<article class="machine-card" data-machine-id=${machine.id}>
    <strong>${view.title}</strong><span class="machine-status">${view.status}</span><small data-time-tip=${absoluteTime(machine.last_seen) || null}>${view.detail}</small>
    <div class="machine-card-actions">
      ${view.approve && html`<${IconButton} name="check" label="Approve" ...${lock('Approve')} onClick=${() => store.change(machine, 'approve')} /><span class="machine-action-label">Approve</span>`}
      ${view.revoke && html`<${IconButton} name="key-off" label="Revoke" ...${lock('Revoke')} onClick=${() => store.change(machine, 'revoke')} /><span class="machine-action-label">Revoke</span>`}
      ${view.stop && html`<button type="button" class="btn-icon btn-danger machine-stop-all" title="Stop all agents on this machine" onClick=${() => store.stopAgents(machine)}><span class="icon-mask icon-mask-stop" aria-hidden="true"></span><span>Stop all agents</span></button>`}
      ${view.restart && html`<button type="button" class="btn btn-danger" onClick=${() => store.restartSupervisor(machine)}>Restart now</button>`}
    </div>
    <p class="settings-desc">${machine.scope_mode === 'listed' ? `Allowed projects: ${machine.allowed_project_ids?.length || 0}` : 'Allowed projects: All'}</p>
    ${!machine.revoked_at && html`<button type="button" class="btn machine-scope-edit" onClick=${() => store.openScope(machine)}>Projects & availability</button>`}
    ${store.scopeMachine.value === machine.id && html`<div class="settings-group machine-scope-editor" role="group" aria-label=${`Projects allowed on ${machine.name}`}>
      <label><input type="checkbox" class="machine-scope-all" checked=${store.scopeAll.value} onChange=${event => store.scopeAll.value = event.currentTarget.checked} />All projects</label>
      <div hidden=${store.scopeAll.value}>${store.scopeProjects.value.map(project => html`<label key=${project.id}><input type="checkbox" class="machine-scope-project" value=${project.id} checked=${store.scopeSelected.value.includes(project.id)} onChange=${event => store.toggleProject(project.id, event.currentTarget.checked)} />${project.name}</label>`)}</div>
      ${machine.is_host && html`<label><input type="checkbox" class="machine-scope-disabled" checked=${store.scopeDisabled.value} onChange=${event => store.scopeDisabled.value = event.currentTarget.checked} />Disabled</label>`}
      <p class="settings-desc">Disabled stops the local supervisor and prevents agents from running here. Stop or move active agents before removing their project or disabling this machine.</p>
      <button type="button" class="btn btn-primary machine-scope-save" disabled=${store.scopeBusy.value} onClick=${() => store.saveScope(machine)}>Save machine</button>
    </div>`}
    ${instances.length > 0 && html`<ul class="machine-instances" aria-label="Supervisor instances">${instances.map(instance => {
      const row = instanceView(instance);
      return html`<li class="machine-instance" data-journal-id=${instance.journal_id}><span class="machine-instance-title">${row.title}</span><small data-time-tip=${absoluteTime(instance.connected || instance.state === 'gone' ? '' : instance.last_seen_at) || null}>${row.detail}</small>
        ${row.canWriteOff && html`<${IconButton} name="trash" label="Mark instance as gone" danger className="machine-instance-gone" onClick=${() => store.writeOff(machine, instance)} />`}</li>`;
    })}</ul>`}
    </article>`;
}

export function MachinesPage({ store }) {
  const machines = store.machines.value;
  return html`
    <p class="settings-intro">Pair a supervisor, approve it, and map project folders before spawning there. The coding tools on a machine are set up under Harnesses & models.</p>
    <div class="machine-actions">
      <button id="machine-add" type="button" class="btn btn-primary" hidden=${store.addHidden.value} disabled=${!!store.addDisabled.value} title=${store.addDisabled.value === null ? undefined : store.addDisabled.value ? 'Only the owner can pair machines' : ''} onClick=${() => store.openPairing()}>Add machine</button>
      <${IconButton} id="machine-refresh" name="refresh" label="Refresh" onClick=${() => store.load()} />
    </div>
    ${store.addDisabled.value && html`<p id="machines-add-note" class="settings-intro">Only the owner can add machines, approve them or revoke them.</p>`}
    <p id="machines-error" class="error" role="alert">${store.error.value}</p>
    <p id="machines-note" class="settings-intro" role="status">${store.note.value}</p>
    <div id="machines-list">${machines === undefined ? '' : machines === null ? 'Loading machines…' : machines.length ? machines.map(machine => html`<${MachineCard} key=${machine.id} store=${store} machine=${machine} />`) : html`<p class="settings-intro">No machines yet.</p>`}</div>`;
}

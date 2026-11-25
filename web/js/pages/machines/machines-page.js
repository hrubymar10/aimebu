import { html } from '../../ui.js';
import { confirmDialog } from '../../dialogs.js';
import { IconButton } from '../../components/icon-button.js';
import { cardView, instanceView } from './model.js';

function MachineCard({ store, machine }) {
  const view = cardView(machine, store.owner.value);
  const instances = store.instances.value[machine.id] || [];
  const lock = view.locked ? label => ({ disabled: true, title: `Only the owner can ${label.toLowerCase()} machines` }) : () => ({});
  return html`<article class="machine-card" data-machine-id=${machine.id}>
    <strong>${view.title}</strong><span class="machine-status">${view.status}</span><small>${view.detail}</small>
    <div class="machine-card-actions">
      ${view.approve && html`<${IconButton} name="check" label="Approve" ...${lock('Approve')} onClick=${() => store.change(machine, 'approve')} /><span class="machine-action-label">Approve</span>`}
      ${view.revoke && html`<${IconButton} name="key-off" label="Revoke" ...${lock('Revoke')} onClick=${() => store.change(machine, 'revoke')} /><span class="machine-action-label">Revoke</span>`}
      ${view.stop && html`<button type="button" class="btn-icon btn-danger machine-stop-all" title="Stop all agents on this machine" onClick=${() => store.stopAgents(machine)}><span class="icon-mask icon-mask-stop" aria-hidden="true"></span><span>Stop all agents</span></button>`}
      ${view.restart && html`<button type="button" class="btn btn-danger" onClick=${() => store.restartSupervisor(machine)}>Restart now</button>`}
    </div>
    ${instances.length > 0 && html`<ul class="machine-instances" aria-label="Supervisor instances">${instances.map(instance => {
      const row = instanceView(instance);
      return html`<li class="machine-instance" data-journal-id=${instance.journal_id}><span class="machine-instance-title">${row.title}</span><small>${row.detail}</small>
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

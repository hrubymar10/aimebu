import { html, Fragment, useState, useEffect, useLayoutEffect, useRef } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { HarnessCard } from './card.js';
import { Activity } from './activity.js';
import { AddHarnessForm } from './add-harness-form.js';
import { DetectPanel } from './detect-panel.js';
import { useHarnesses } from './use-harnesses.js';
import { DetectionDetails } from './detection-details.js';

export function HarnessesPage({ openToken, wanted, onDirty = () => {}, onView = () => {}, onReady = () => {} }) {
  const [panel, setPanel] = useState(null);
  const panelRef = useRef(null);
  const data = useHarnesses({ openToken, wanted, closePanel: () => setPanel(null), onReady });
  const { state, load } = data;
  const machineID = state.machineID.value, rows = state.rows.value, openForms = state.openForms.value, openModels = state.openModels.value;
  useEffect(() => { if (panel) panelRef.current?.querySelector('input,select,button')?.focus(); }, [panel]);
  useLayoutEffect(() => { onView(machineID); }, [machineID]);

  const detect = async () => {
    const found = await data.detect();
    if (found) setPanel({ kind: 'detect', ...found });
  };

  const cardProps = harness => ({
    machineID, harness, key: harness.id,
    formOpen: openForms.has(harness.id), modelsOpen: openModels.has(harness.id),
    onFormOpen: data.setFormOpen,
    onModelsOpen: data.setModelsOpen,
    draft: state.drafts.value.get(harness.id),
    onDraft: (kind, update) => data.setDraft(harness.id, kind, update),
    modelFailure: state.modelFailures.value.get(harness.id) || '',
    onModelFailure: data.setModelFailure,
    onReload: () => load(machineID),
    onRefresh: () => load(machineID, { keepError: true }),
    onModelsSaved: id => {
      data.setModelsOpen(id, true);
      return load(machineID);
    },
    onSnapshot: data.patchRow,
    onDirty,
  });

  return html`
    <${Fragment}>
      <h3>Harnesses <span class="room-member-badge" title="The Preact version of this page (option B of the preact-harnesses experiment)">Preact</span></h3>
      <p class="settings-intro">A harness is a coding tool installed on a machine, with the models it can start agents with. Nothing is listed until you add it. Pick a machine, add its harnesses (or auto-detect them), then give each harness its models: exactly one is the default, which an agent falls back to when its own model is removed.</p>
      <${SettingsGroup} label="Harness machine">
        <${SettingsRow} label="Machine" desc="The supervisor whose harnesses and recent changes are shown below." id="harness-machine">
          <select value=${machineID} onChange=${event => data.chooseMachine(event.currentTarget.value)}>
            ${state.machines.value.map(machine => html`<option value=${machine.id} key=${machine.id}>${machine.name || machine.id}</option>`)}
          </select>
        <//>
      <//>
      <p class="settings-intro" role="status">${state.summary.value}</p>
      <p class="error" role="alert">${state.error.value}</p>
      <${DetectionDetails} detail=${state.errorDetail.value} />
      <div class="machine-actions" hidden=${state.toolbarHidden.value}>
        <button type="button" class="btn btn-primary" id="harness-add" onClick=${() => setPanel({ kind: 'add', machineID })}>Add harness</button>
        <button type="button" class="btn" id="harness-detect" disabled=${state.detecting.value} onClick=${detect}>
          ${state.detecting.value ? 'Detecting…' : 'Auto-detect harnesses'}
        </button>
      </div>
      <div id="harness-panel" ref=${panelRef}>
        ${panel?.kind === 'add' && html`
          <${AddHarnessForm}
            key="add"
            onCancel=${() => setPanel(null)}
            onCreate=${fields => data.addByHand(panel.machineID, fields)}
            onDirty=${onDirty}
          />
        `}
        ${panel?.kind === 'detect' && html`
          <${DetectPanel}
            key="detect"
            candidates=${panel.candidates}
            onClose=${() => setPanel(null)}
            onAdd=${chosen => data.addDetected(panel.machineID, chosen)}
            onFailed=${() => data.alive() && load(panel.machineID)}
            onDirty=${onDirty}
          />
        `}
      </div>
      <div id="harnesses-list">
        ${rows && !rows.length && html`<p class="settings-intro">No harnesses on this machine yet. Add one, or auto-detect what is installed.</p>`}
        ${rows?.map(harness => html`<${HarnessCard} ...${cardProps(harness)} />`)}
      </div>
      <h4 class="machine-activity-heading">Activity</h4>
      <div id="machine-activity"><${Activity} activity=${state.activity.value} /></div>
    <//>
  `;
}

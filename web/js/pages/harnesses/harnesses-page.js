import { html, Fragment, useState, useEffect, useLayoutEffect, useRef } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { HarnessCard } from './card.js';
import { Activity } from './activity.js';
import { AddHarnessForm } from './add-harness-form.js';
import { DetectPanel } from './detect-panel.js';
import { useHarnesses } from './use-harnesses.js';
import { DetectionDetails } from './detection-details.js';

const toggled = (set, id, open) => {
  const next = new Set(set);
  if (open) next.add(id);
  else next.delete(id);
  return next;
};

export function HarnessesPage({ openToken, wanted, onDirty = () => {}, onView = () => {}, onReady = () => {} }) {
  const [panel, setPanel] = useState(null);
  const panelRef = useRef(null);
  const [openForms, setOpenForms] = useState(() => new Set());
  const [openModels, setOpenModels] = useState(() => new Set());
  const data = useHarnesses({ openToken, wanted, closePanel: () => setPanel(null), onReady });
  const { machines, machineID, summary, error, toolbarHidden, rows, activity, detecting, modelFailures, load } = data;
  useEffect(() => { if (panel) panelRef.current?.querySelector('input,select,button')?.focus(); }, [panel]);
  useLayoutEffect(() => { onView(machineID); }, [machineID]);

  const detect = async () => {
    const found = await data.detect();
    if (found) setPanel({ kind: 'detect', ...found });
  };

  const cardProps = harness => ({
    machineID, harness, key: harness.id,
    formOpen: openForms.has(harness.id), modelsOpen: openModels.has(harness.id),
    onFormOpen: (id, open) => setOpenForms(current => toggled(current, id, open)),
    onModelsOpen: (id, open) => setOpenModels(current => toggled(current, id, open)),
    modelFailure: modelFailures.get(harness.id) || '',
    onModelFailure: data.setModelFailure,
    onReload: () => load(machineID),
    onRefresh: () => load(machineID, { keepError: true }),
    onModelsSaved: id => {
      setOpenModels(current => toggled(current, id, true));
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
            ${machines.map(machine => html`<option value=${machine.id} key=${machine.id}>${machine.name || machine.id}</option>`)}
          </select>
        <//>
      <//>
      <p class="settings-intro" role="status">${summary}</p>
      <p class="error" role="alert">${error}</p>
      <${DetectionDetails} detail=${data.errorDetail} />
      <div class="machine-actions" hidden=${toolbarHidden}>
        <button type="button" class="btn btn-primary" id="harness-add" onClick=${() => setPanel({ kind: 'add', machineID })}>Add harness</button>
        <button type="button" class="btn" id="harness-detect" disabled=${detecting} onClick=${detect}>
          ${detecting ? 'Detecting…' : 'Auto-detect harnesses'}
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
      <div id="machine-activity"><${Activity} activity=${activity} /></div>
    <//>
  `;
}

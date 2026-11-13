import { html, useState, useLayoutEffect, useRef } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { HarnessCard } from './card.js';
import { Activity } from './activity.js';
import { AddHarnessForm } from './add-harness-form.js';
import { DetectPanel } from './detect-panel.js';
import { rights } from '../../api.js';
import { NOT_CONNECTED, OWNER_ONLY } from './model.js';
import { useHarnesses } from './use-harnesses.js';
import { DetectionDetails } from './detection-details.js';
import { MasterList } from './master-list.js';
import { RolePanel } from './role-panel.js';
import { IdPrefix } from './id-prefix.js';

const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;
// What the result step says was added: the harnesses and how many models they offer.
const resultLine = rows => `${plural(rows.length, 'harness', 'harnesses')}, ${plural(rows.reduce((sum, row) => sum + (row.models?.length || 0), 0), 'model', 'models')} found.`;

// `wanted` is the machine to open on. Embedded (the onboarding step) the detect panel has no Add button: `controls` lets the step add what is ticked, `onTick` says how many are.
export function HarnessesPage({ openToken, wanted, idPrefix = '', embedded = false, controls = null, result = false, showRoles = !embedded, onTick, onOpened, onDirty = () => {}, onView = () => {}, onReady = () => {}, isHidden = () => false }) {
  const [panel, setPanel] = useState(null);
  const panelRef = useRef(null);
  const data = useHarnesses({ openToken, wanted, closePanel: () => setPanel(null), onReady, onOpened, isHidden });
  const { state, load } = data;
  const readOnly = !rights.owner;
  const machineID = state.machineID.value, rows = state.rows.value, openForms = state.openForms.value, selected = state.selected.value, roles = state.roles.value;
  // Focused in the same commit as the panel: after paint it would take the focus from a field already chosen.
  useLayoutEffect(() => { if (panel) panelRef.current?.querySelector('input,select,button')?.focus(); }, [panel]);
  useLayoutEffect(() => { onView(machineID); }, [machineID]);

  const detect = async () => {
    const found = await data.detect();
    if (found) setPanel({ kind: 'detect', ...found });
  };

  const cardProps = harness => ({
    machineID, harness, key: harness.id, selected: harness.id === selected, readOnly,
    formOpen: openForms.has(harness.id),
    onFormOpen: data.setFormOpen,
    roles, defaults: data.defaults, onRoleChanged: data.roleChanged, onStale: data.staleModels, onStaleProviders: data.staleProviders,
    draft: state.drafts.value.get(harness.id),
    onDraft: (kind, update) => data.setDraft(harness.id, kind, update),
    modelFailure: state.modelFailures.value.get(harness.id) || '',
    onModelFailure: data.setModelFailure,
    onReload: () => load(machineID),
    onRefresh: () => load(machineID, { keepError: true }),
    onModelsSaved: () => load(machineID),
    onSnapshot: data.patchRow,
    onDirty,
  });

  // A role save that fails shows why; either way the views redraw from what the store holds.
  const runRole = async action => {
    state.roleError.value = '';
    try {
      await action();
    } catch (problem) {
      if (data.alive()) state.roleError.value = problem.message || 'Could not save the role';
    }
    data.roleChanged();
  };

  // A machine whose supervisor is not connected cannot detect or take a harness: say so, and why the buttons are off.
  const offline = state.machines.value.find(entry => entry.id === machineID)?.connected === false;
  const blocked = readOnly ? OWNER_ONLY : offline ? NOT_CONNECTED : undefined;

  return html`
    <${IdPrefix.Provider} value=${idPrefix}>
      <p class="settings-intro">A harness is a coding tool installed on a machine, with the models it can start agents with. Nothing is listed until you add it. Pick a machine, add its harnesses (or auto-detect them), then give each harness its models: exactly one is the default, which an agent falls back to when its own model is removed.</p>
      <${SettingsGroup} label="Harness machine">
        <${SettingsRow} label="Machine" desc="The supervisor whose harnesses and recent changes are shown below." id=${`${idPrefix}harness-machine`}>
          <select value=${machineID} onChange=${event => data.chooseMachine(event.currentTarget.value)}>
            ${state.machines.value.map(machine => html`<option value=${machine.id} key=${machine.id}>${machine.name || machine.id}</option>`)}
          </select>
        <//>
      <//>
      <p class="settings-intro" role="status">${state.summary.value}</p>
      <p class="settings-intro" role="status" hidden=${!state.info.value}>${state.info.value}</p>
      <p class="error" role="alert">${state.error.value}</p>
      <${DetectionDetails} detail=${state.errorDetail.value} />
      <p class="settings-intro" id=${`${idPrefix}harness-readonly`} role="note" hidden=${!readOnly}>${OWNER_ONLY} You can look at the harnesses here; the controls are off.</p>
      <p class="settings-intro" id=${`${idPrefix}harness-offline`} role="note" hidden=${!offline || state.toolbarHidden.value}>${NOT_CONNECTED}</p>
      <div class="machine-actions" hidden=${state.toolbarHidden.value}>
        <button type="button" class="btn btn-primary" id=${`${idPrefix}harness-add`} disabled=${readOnly || offline} title=${blocked} onClick=${() => setPanel({ kind: 'add', machineID })}>Add harness</button>
        <button type="button" class="btn" id=${`${idPrefix}harness-detect`} disabled=${state.detecting.value || readOnly || offline} title=${blocked} onClick=${detect}>
          ${state.detecting.value ? 'Detecting…' : 'Auto-detect harnesses'}
        </button>
      </div>
      <div id=${`${idPrefix}harness-panel`} ref=${panelRef}>
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
            embedded=${embedded}
            controls=${controls}
            onTick=${onTick}
            onClose=${() => setPanel(null)}
            onAdd=${chosen => data.addDetected(panel.machineID, chosen)}
            onFailed=${() => data.alive() && load(panel.machineID)}
            onDirty=${onDirty}
          />
        `}
      </div>
      ${result && rows && html`<p class="harness-result" role="status">${resultLine(rows)}</p>`}
      <div class="harness-list" id=${`${idPrefix}harnesses-list`}>
        ${/* In the first-run step the status line under the list already says what is missing; a second, near-identical line is hidden (kept in the DOM: the page test waits for the list to have a child). */''}
        ${rows && !rows.length && !(panel?.kind === 'detect' && panel.candidates.length) && html`<p class="settings-intro" hidden=${embedded}>No harnesses on this machine yet. Add one, or auto-detect what is installed.</p>`}
        ${rows?.length > 0 && html`
          <div class="harness-split">
            <${MasterList} rows=${rows} selected=${selected} onSelect=${data.selectHarness} />
            <div class="harness-detail">${rows.map(harness => html`<${HarnessCard} ...${cardProps(harness)} />`)}</div>
          </div>
        `}
      </div>
      <div class="harness-roles-host" id=${`${idPrefix}harness-roles-host`}>
        ${rows && showRoles && html`<${RolePanel} roles=${roles} labelOf=${data.labelOfDefault} store=${data.defaults} run=${runRole} />`}
      </div>
      <p class="error" role="alert">${state.roleError.value}</p>
      <h4 class="machine-activity-heading">Activity</h4>
      <div id=${`${idPrefix}machine-activity`}><${Activity} activity=${state.activity.value} /></div>
    <//>
  `;
}

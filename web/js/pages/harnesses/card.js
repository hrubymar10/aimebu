import { html, useState, useRef } from '../../ui.js';
import { request } from '../../api.js';
import { confirmDialog } from '../../dialogs.js';
import { useAlive } from '../../hooks/use-alive.js';
import { useDirty } from '../../hooks/use-dirty.js';
import { IconButton } from '../../components/icon-button.js';
import { ModelsPanel } from './models.js';
import { versionLabel } from '../../harness-version.js';
import { familyName, harnessStatus, harnessTitle, harnessLabel, formFromHarness, launcherBody, enabledBody } from './model.js';

const launcherURL = (machineID, harnessID) => `/api/machines/${encodeURIComponent(machineID)}/launchers/${encodeURIComponent(harnessID)}`;

const TextArea = ({ label, value, onInput }) => html`
  <label class="machine-harness-path-label">${label}<textarea
    spellcheck=${false}
    value=${value}
    onInput=${event => onInput(event.currentTarget.value)}
  ></textarea></label>
`;
const Switch = ({ label, checked, onChange }) => html`
  <label class="machine-harness-enabled"><input
    type="checkbox"
    checked=${checked}
    onChange=${event => onChange(event.currentTarget.checked)}
  />${label}</label>
`;

// Drafts survive other cards' refreshes; a save clears its draft only if nothing was typed meanwhile.
export function HarnessCard({ machineID, harness, selected, formOpen, onFormOpen, draft, onDraft, roles, defaults, onRoleChanged, modelFailure, onModelFailure, onReload, onRefresh, onModelsSaved, onStale, onSnapshot, onDirty }) {
  const alive = useAlive();
  const edits = draft?.form ?? null;
  const setEdits = update => onDraft('form', update);
  const revision = useRef(0);
  const form = edits || formFromHarness(harness);
  // A function of the latest draft, so two events before the next render both count.
  const set = (key, value) => {
    revision.current++;
    setEdits(current => ({ ...(current || formFromHarness(harness)), [key]: value }));
  };
  useDirty(onDirty, `form:${harness.id}`, edits !== null);
  const [error, setError] = useState('');
  // A ref as well as disabled controls: a form can still be submitted with its button disabled.
  const writeLock = useRef(false);
  const [writing, setWriting] = useState(false);
  const lock = () => {
    if (writeLock.current) return false;
    writeLock.current = true;
    setWriting(true);
    return true;
  };
  const unlock = () => {
    writeLock.current = false;
    if (alive()) setWriting(false);
  };
  const [pendingEnabled, setPendingEnabled] = useState(null);
  const enabled = pendingEnabled ?? !!harness.enabled;
  const status = harnessStatus(harness);
  const formID = `launcher-form-${harness.id}`;

  const save = async event => {
    event.preventDefault();
    if (!lock()) return;
    setError('');
    const started = revision.current;
    try {
      const body = launcherBody(harness, form, enabled);
      await request(launcherURL(machineID, harness.id), { method: 'PUT', body });
      if (!alive()) return;
      const refreshed = await onReload();
      if (!alive()) return;
      if (!refreshed) onSnapshot(harness.id, body);
      if (revision.current === started) setEdits(null);
    } catch (problem) {
      if (!alive()) return;
      setError(problem.message || 'Could not save harness settings');
      onFormOpen(harness.id, true);
    } finally {
      unlock();
    }
  };
  // Sends only the new Enabled value, so edits still open in the form stay unsaved.
  const toggle = async next => {
    if (!lock()) return;
    setError('');
    setPendingEnabled(next);
    try {
      await request(launcherURL(machineID, harness.id), { method: 'PUT', body: enabledBody(harness, next) });
      if (!alive()) return;
      if (!await onRefresh() && alive()) onSnapshot(harness.id, { enabled: next });
    } catch (problem) {
      if (!alive()) return;
      setError(problem.message || 'Could not save harness settings');
      onFormOpen(harness.id, true);
    } finally {
      if (alive()) setPendingEnabled(null);
      unlock();
    }
  };
  const remove = async () => {
    if (!await confirmDialog(`Remove ${harness.label || harness.name}? Its models go with it.`, { title: 'Remove harness', confirm: 'Remove', danger: true })) return;
    if (!lock()) return;
    try {
      await request(launcherURL(machineID, harness.id), { method: 'DELETE' });
      if (alive()) await onReload();
    } catch (problem) {
      if (alive()) setError(problem.message || 'Could not remove launcher');
    } finally {
      unlock();
    }
  };

  return html`
    <article class="machine-harness settings-group" id=${`harness-card-${harness.id}`} data-harness-id=${harness.id} data-tool=${harness.tool} data-variant=${harness.variant} role="tabpanel" aria-labelledby=${`harness-tab-${harness.id}`} hidden=${!selected}>
      <div class="settings-row machine-harness-summary">
        <div class="settings-row-info">
          <div class="machine-harness-title">
            <strong class="settings-label">${harnessTitle(harness)}</strong>
            <span class=${`machine-harness-status${status === 'Available' ? ' is-available' : ''}`}>${status}</span>
          </div>
          <span class="settings-desc machine-harness-meta">
            ${`${versionLabel(harness)} · ${familyName(harness.tool)} · ${harness.active_agents || 0} active agents`}
          </span>
          ${harness.unavailable_reason && html`<span class="settings-desc machine-harness-reason">${harness.unavailable_reason}</span>`}
          <span class="settings-desc machine-harness-path">${`Command: ${harness.launcher_path || harness.command}`}</span>
        </div>
        <div class="settings-control machine-harness-controls">
          <label class="machine-harness-enabled"><input
            type="checkbox"
            name="enabled"
            checked=${enabled}
            disabled=${writing}
            aria-label=${`Enable ${harness.label || harness.tool}`}
            onChange=${event => toggle(event.currentTarget.checked)}
          />Enabled</label>
          <${IconButton}
            name=${formOpen ? 'close' : 'edit'}
            label=${`${formOpen ? 'Close settings for' : 'Edit'} ${harnessLabel(harness)}`}
            className="machine-harness-edit"
            aria-expanded=${String(formOpen)}
            aria-controls=${formID}
            onClick=${() => onFormOpen(harness.id, !formOpen)}
          />
        </div>
      </div>
      <form class="machine-harness-form" id=${formID} hidden=${!formOpen} onSubmit=${save}>
        <label class="machine-harness-path-label">Label<input
          type="text"
          name="label"
          maxLength="80"
          autocomplete="off"
          value=${form.label}
          onInput=${event => set('label', event.currentTarget.value)}
        /></label>
        <label class="machine-harness-path-label">Executable name or absolute path<input
          type="text"
          name="command"
          autocomplete="off"
          spellcheck=${false}
          placeholder=${harness.launcher_path || 'claude'}
          value=${form.command}
          onInput=${event => set('command', event.currentTarget.value)}
        /></label>
        <p class="machine-harness-help">The supervisor finds executable names on this machine. Arguments run without a shell.</p>
        <${TextArea} label="Extra arguments, one per line" value=${form.args} onInput=${value => set('args', value)} />
        <${TextArea} label="Environment references, TARGET=SOURCE per line" value=${form.env_refs} onInput=${value => set('env_refs', value)} />
        <${Switch} label="Containerized" checked=${form.containerized} onChange=${value => set('containerized', value)} />
        <label class="machine-harness-path-label">Agent-reachable server URL (optional)<input
          type="text"
          value=${form.agent_server_url}
          onInput=${event => set('agent_server_url', event.currentTarget.value)}
        /></label>
        <${TextArea} label="Path mappings, host → visible per line" value=${form.path_maps} onInput=${value => set('path_maps', value)} />
        <${TextArea} label="Visible host roots, one per line" value=${form.visible_roots} onInput=${value => set('visible_roots', value)} />
        <${Switch} label="Supports agent toolbox" checked=${form.supports_toolbox} onChange=${value => set('supports_toolbox', value)} />
        <div class="machine-harness-actions">
          <button type="submit" class="btn btn-primary" disabled=${writing}>Save</button>
          <${IconButton} name="trash" label=${`Remove ${harnessLabel(harness)}`} danger className="machine-harness-remove" disabled=${writing} onClick=${remove} />
        </div>
        <p class="error" role="alert">${error}</p>
      </form>
      <${ModelsPanel}
        machineID=${machineID}
        harness=${harness}
        draft=${draft?.models ?? null}
        setDraft=${update => onDraft('models', update)}
        roles=${roles}
        defaults=${defaults}
        onRoleChanged=${onRoleChanged}
        failure=${modelFailure}
        onFailure=${onModelFailure}
        onSaved=${onModelsSaved}
        onStale=${onStale}
        onSnapshot=${onSnapshot}
        onDirty=${onDirty}
      />
    </article>
  `;
}

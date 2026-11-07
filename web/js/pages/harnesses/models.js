import { html, useState, useEffect, useRef } from '../../ui.js';
import { request } from '../../api.js';
import { IconButton } from '../../components/icon-button.js';
import { useAlive } from '../../hooks/use-alive.js';
import { useDirty } from '../../hooks/use-dirty.js';
import { positionOf, canAddDefault } from '../../role-defaults-logic.js';
import { DetectionDetails } from './detection-details.js';
import { RoleSwitch } from './role-switch.js';
import { useIdPrefix } from './id-prefix.js';
import { addModel, addFoundModels, removeModel, setDefaultModel, updateModel, modelsError, detectionFailure } from './model.js';

const modelsURL = (machineID, harnessID) => `/api/machines/${encodeURIComponent(machineID)}/launchers/${encodeURIComponent(harnessID)}/models`;

// `draft` is { models, rev } while the list has unsaved edits: `rev` is the revision of the stored list the draft was started from, which the save sends back so a list changed meanwhile is refused.
export function ModelsPanel({ machineID, harness, draft, setDraft, roles, defaults, onRoleChanged, failure, onFailure, onSaved, onStale, onSnapshot, onDirty }) {
  const alive = useAlive();
  const prefix = useIdPrefix();
  const revision = useRef(0);
  const models = draft?.models || harness.models || [];
  const dirty = draft !== null;
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [failureDetail, setFailureDetail] = useState('');
  const [failureProblems, setFailureProblems] = useState({});
  const [status, setStatus] = useState('');
  const [found, setFound] = useState(null);
  const [retryShown, setRetryShown] = useState(false);
  const [detecting, setDetecting] = useState(false);
  const [entry, setEntry] = useState({ slug: '', label: '', note: '' });
  const [dir, setDir] = useState('');
  const slugInput = useRef(null), dirInput = useRef(null);
  const edit = change => {
    revision.current++;
    setDraft(current => ({ models: change(current?.models || harness.models || []), rev: current?.rev ?? harness.models_rev ?? '' }));
    setStatus('');
  };
  useDirty(onDirty, `models:${harness.id}`, dirty);
  useDirty(onDirty, `model-entry:${harness.id}`, !!(entry.slug || entry.label || entry.note));
  const setField = (key, value) => setEntry(current => ({ ...current, [key]: value }));
  const failed = problem => {
    const failure = problem?.summary ? problem : detectionFailure(problem, !!harness.containerized);
    setError(failure.summary);
    setFailureDetail(failure.detail);
    setFailureProblems(problem?.body?.endpoint_problems || {}); // a listing that failed still says which custom endpoints could not be asked
    if (harness.tool !== 'claude') setRetryShown(true);
  };
  useEffect(() => { if (failure) failed(failure); }, [failure]);

  const addByHand = event => {
    event.preventDefault();
    setError('');
    let next;
    try {
      next = addModel(models, harness.tool, entry.slug, entry.label, entry.note);
    } catch (problem) {
      setError(problem.message);
      return;
    }
    edit(() => next);
    setEntry({ slug: '', label: '', note: '' });
    slugInput.current?.focus();
  };
  // The folder is sent with that one request and never saved.
  // One detection at a time, and only the latest run writes its answer: Enter can fire while the button is disabled.
  const detectBusy = useRef(false), detectRun = useRef(0);
  const detect = async folder => {
    if (detectBusy.current) return;
    detectBusy.current = true;
    const run = ++detectRun.current;
    const latest = () => alive() && run === detectRun.current;
    setError('');
    setFailureDetail('');
    setFailureProblems({});
    setDetecting(true);
    try {
      const result = await request(`${modelsURL(machineID, harness.id)}/detect`, { method: 'POST', body: folder ? { dir: folder } : {} });
      if (!latest()) return;
      onFailure(harness.id, '');
      setRetryShown(false);
      setFound(result);
    } catch (problem) {
      if (latest()) failed(problem);
    } finally {
      if (run === detectRun.current) {
        detectBusy.current = false;
        if (alive()) setDetecting(false);
      }
    }
  };
  const retry = () => {
    const folder = dir.trim();
    if (!folder.startsWith('/')) {
      setError('Enter an absolute folder path on that machine.');
      dirInput.current?.focus();
      return;
    }
    detect(folder);
  };
  const addFound = picked => {
    const result = addFoundModels(models, harness.tool, picked);
    if (result.added) edit(() => result.models);
    setStatus(result.status);
    setFound(null);
  };
  const save = async () => {
    setError('');
    setStatus('');
    const problem = modelsError(models);
    if (problem) {
      setError(problem);
      return;
    }
    setSaving(true);
    const started = revision.current, sent = models;
    try {
      const saved = await request(modelsURL(machineID, harness.id), { method: 'PUT', body: { models: sent, expected_rev: draft?.rev ?? harness.models_rev ?? '' } });
      if (!alive()) return;
      // What is stored now has this revision: edits made while the save was in flight, and the next save, start from it, not from the
      // revision the draft began with (which the server has just moved on from).
      const rev = saved?.models_rev;
      if (rev !== undefined) setDraft(current => current && { ...current, rev });
      const refreshed = await onSaved(harness.id);
      if (!alive()) return;
      if (!refreshed) onSnapshot(harness.id, rev === undefined ? { models: sent } : { models: sent, models_rev: rev });
      if (revision.current === started) setDraft(null);
    } catch (failure) {
      if (!alive()) return;
      if (failure.status === 409) onStale(harness);
      else setError(failure.message || 'Could not save the models');
    } finally {
      if (alive()) setSaving(false);
    }
  };

  // The role defaults of one model, as labelled switches. They save on their own (a role's defaults are the role's, not the harness's),
  // so a model that is not saved yet cannot be ticked.
  const toggleRole = async (role, model, on) => {
    setError('');
    const live = defaults.roles.find(item => item.id === role.id) || role;
    try {
      await defaults.setModel(live, harness.id, model.slug, on);
    } catch (problem) {
      if (alive()) setError(problem.message || 'Could not save the role');
    }
    onRoleChanged();
  };
  const roleState = (role, model) => {
    const at = positionOf(role, harness.id, model.slug), stored = (harness.models || []).some(item => item.slug === model.slug), full = at < 0 && !canAddDefault(role);
    return { on: at >= 0, position: at + 1, total: (role.default_agents || []).length, disabled: !stored || full, reason: stored ? (full ? `${role.label} has the most defaults it can hold` : '') : 'Save the models first' };
  };

  const onDirKey = event => {
    if (event.key === 'Enter') {
      event.preventDefault();
      retry();
    }
  };

  return html`
    <div class="harness-models" id=${`${prefix}harness-models-${harness.id}`}>
      <h4>Models of ${harness.label || harness.name}</h4>
      <div class="harness-model-rows">
        ${!models.length && html`<p class="settings-intro">No models yet: this harness cannot start an agent until one is added.</p>`}
        ${models.map(model => html`
          <div class="harness-model-row harness-detail-row" data-slug=${model.slug} key=${model.slug}>
            <div class="harness-detail-top">
              <label class=${`harness-model-default${model.default ? ' is-default' : ''}`} title=${model.default ? 'Default model' : 'Make default'}>
                <input
                  type="radio"
                  name=${`default-${harness.id}`}
                  checked=${!!model.default}
                  aria-label=${model.default ? `${model.label || model.slug} is the default model` : `Make ${model.label || model.slug} the default`}
                  onChange=${() => edit(list => setDefaultModel(list, model.slug))}
                />
                <svg class="harness-model-star" viewBox="0 0 24 24" aria-hidden="true"><use href="/icons/star.svg#star" /></svg>
                <span class="harness-model-default-text">${model.default ? 'Default' : ''}</span>
              </label>
              <code class="harness-model-slug">${model.slug}</code>
              <${IconButton} name="trash" label=${`Remove ${model.slug}`} danger className="harness-model-remove" onClick=${() => edit(list => removeModel(list, model.slug))} />
            </div>
            <div class="harness-detail-fields">
              <input
                type="text"
                name="label"
                maxLength="80"
                value=${model.label}
                aria-label=${`Label of ${model.slug}`}
                onInput=${event => {
                  const label = event.currentTarget.value;
                  edit(list => updateModel(list, model.slug, { label }));
                }}
              />
              <input
                type="text"
                name="note"
                maxLength="280"
                placeholder="Note (optional)"
                title=${model.note || ''}
                value=${model.note || ''}
                aria-label=${`Note of ${model.slug}`}
                onInput=${event => {
                  const note = event.currentTarget.value;
                  edit(list => updateModel(list, model.slug, { note }));
                }}
              />
            </div>
            <p class="harness-detail-roles-hint settings-desc">Default for: tick a role to make this model one of its spawn defaults (the number is its place in that role's list). With none ticked it is never picked automatically, but you can still choose it when you spawn by hand.</p>
            <div class="harness-detail-roles" role="group" aria-label=${`Default for roles: ${model.label || model.slug}`}>
              ${roles.map(role => html`
                <${RoleSwitch} key=${role.id} role=${role} modelLabel=${model.label || model.slug} state=${roleState(role, model)} onToggle=${on => toggleRole(role, model, on)} />
              `)}
            </div>
          </div>
        `)}
      </div>
      <form class="harness-model-add" onSubmit=${addByHand}>
        <input
          ref=${slugInput}
          type="text"
          name="slug"
          required
          autocomplete="off"
          spellcheck=${false}
          placeholder=${harness.tool === 'opencode' ? 'provider/model' : 'model id'}
          aria-label="Model id"
          value=${entry.slug}
          onInput=${event => setField('slug', event.currentTarget.value)}
        />
        <input
          type="text"
          name="new-label"
          maxLength="80"
          placeholder="Label (optional)"
          aria-label="New model label"
          value=${entry.label}
          onInput=${event => setField('label', event.currentTarget.value)}
        />
        <input
          type="text"
          name="new-note"
          maxLength="280"
          placeholder="Note (optional)"
          aria-label="New model note"
          value=${entry.note}
          onInput=${event => setField('note', event.currentTarget.value)}
        />
        <button type="submit" class="btn">Add model</button>
      </form>
      <div class="machine-harness-actions">
        <${IconButton}
          name="refresh"
          label=${detecting ? 'Detecting models…' : 'Auto-detect models'}
          className="harness-model-detect"
          disabled=${detecting}
          onClick=${() => detect('')}
        />
        <button type="button" class="btn btn-primary" disabled=${!dirty || saving} onClick=${save}>Save models</button>
      </div>
      <p class="error" role="alert">${error}</p>
      <div class="harness-detect-retry" hidden=${!retryShown}>
        <label class="harness-detect-dir">Run the command in this folder${harness.containerized && html`<span class="harness-detect-dir-hint">or pick a folder that is mounted in the container</span>`}<input
          ref=${dirInput}
          type="text"
          name="detect-dir"
          autocomplete="off"
          spellcheck=${false}
          placeholder="Absolute path on that machine"
          value=${dir}
          onInput=${event => setDir(event.currentTarget.value)}
          onKeyDown=${onDirKey}
        /></label>
        <${IconButton} name="refresh" label="Retry auto-detect in this folder" className="harness-detect-retry-button" disabled=${detecting} onClick=${retry} />
      </div>
      <${DetectionDetails} detail=${failureDetail} />
      <${EndpointProblems} problems=${failureProblems} />
      <div class="harness-detect-models">
        ${found && html`
          <${ModelCandidates}
            found=${found}
            onAdd=${addFound}
            onClose=${() => setFound(null)}
            onDirty=${onDirty}
            dirtyKey=${`model-candidates:${harness.id}`}
          />
        `}
      </div>
      <p class="settings-desc" role="status">${status}</p>
    </div>
  `;
}

// A custom provider whose endpoint could not be asked for its models (the reason, never the endpoint's own words), per provider id.
function EndpointProblems({ problems }) {
  const entries = Object.entries(problems || {});
  return entries.length > 0 && html`<ul class="harness-endpoint-problems" role="status">${entries.map(([id, reason]) => html`<li data-provider=${id} key=${id}>${`${id}: its endpoint could not be asked (${reason})`}</li>`)}</ul>`;
}

function ModelCandidates({ found, onAdd, onClose, onDirty, dirtyKey }) {
  const candidates = found.candidates || [];
  // A model its harness cannot reach (a container and a provider on the machine's loopback) is shown with why, and cannot be ticked.
  const available = candidates.filter(candidate => !candidate.already_listed && !candidate.unreachable);
  const [picked, setPicked] = useState(() => new Set());
  useDirty(onDirty, dirtyKey, picked.size > 0);
  const toggle = slug => setPicked(current => {
    const next = new Set(current);
    if (next.has(slug)) next.delete(slug);
    else next.add(slug);
    return next;
  });
  const selected = available.filter(candidate => picked.has(candidate.slug)).length;
  const all = available.length > 0 && selected === available.length;
  const selectAll = () => setPicked(available.some(candidate => !picked.has(candidate.slug)) ? new Set(available.map(candidate => candidate.slug)) : new Set());
  const submit = event => {
    event.preventDefault();
    onAdd(candidates.filter(candidate => picked.has(candidate.slug)));
  };
  return html`
    <form class="machine-harness-form harness-model-candidates" onSubmit=${submit}>
      <div class="harness-candidate-header">
        <strong>${candidates.length} found${found.source === 'snapshot' ? ' (built-in list: the published source could not be reached)' : ''} — tick the ones to add</strong>
        <div class="harness-candidate-controls">
          <span class="harness-candidate-count">${selected}/${available.length} selected</span>
          <${IconButton}
            name="select-all"
            label=${all ? 'Clear selection' : 'Select all available models'}
            className="harness-candidate-select-all"
            aria-pressed=${all ? 'true' : selected ? 'mixed' : 'false'}
            disabled=${!available.length}
            onClick=${selectAll}
          />
        </div>
      </div>
      <${EndpointProblems} problems=${found.endpoint_problems} />
      <div class="harness-candidate-list">
        ${candidates.map(candidate => html`
          <label class="machine-harness-enabled harness-detect-row" data-slug=${candidate.slug} key=${candidate.slug}>
            <input
              type="checkbox"
              disabled=${!!candidate.already_listed || !!candidate.unreachable}
              checked=${picked.has(candidate.slug)}
              aria-label=${`Add ${candidate.slug}`}
              onChange=${() => toggle(candidate.slug)}
            />
            <span>${`${candidate.label} · ${candidate.slug}${candidate.note ? ` · ${candidate.note}` : ''}${candidate.already_listed ? ' · already listed' : ''}${candidate.unreachable ? ` · not offered: ${candidate.unreachable}` : ''}`}</span>
          </label>
        `)}
      </div>
      <div class="machine-harness-actions">
        <button type="submit" class="btn btn-primary">Add selected</button>
        <button type="button" class="btn" onClick=${onClose}>Close</button>
      </div>
    </form>
  `;
}

import { html, useState, useEffect, useRef } from '../../ui.js';
import { request } from '../../api.js';
import { IconButton } from '../../components/icon-button.js';
import { useAlive } from '../../hooks/use-alive.js';
import { useDirty } from '../../hooks/use-dirty.js';
import { useIdPrefix } from './id-prefix.js';
import { PROVIDERS_MAX, addProvider, addSuggestions, editableProvider, inUseMessage, keyState, providerInUse, providersBody, providersError, removeProvider, removeWithModelsLabel, suggestionState, updateProvider } from './model.js';

const providersURL = (machineID, harnessID) => `/api/machines/${encodeURIComponent(machineID)}/launchers/${encodeURIComponent(harnessID)}/providers`;

// The custom providers of an OpenCode harness: OpenAI-compatible endpoints whose models are listed as `<id>/<model>`. Like the models list, edits are a
// draft ({ providers, rev }) until Save providers, which sends the whole list with the revision it started from. Only the NAME of the variable the key
// is read from is entered or shown, never a key; whether that variable has a value on the machine comes from the key-status answer.
export function ProvidersPanel({ machineID, harness, draft, setDraft, onSaved, onStale, onSnapshot, onDirty }) {
  const alive = useAlive();
  const prefix = useIdPrefix();
  const revision = useRef(0);
  const stored = (harness.providers || []).map(editableProvider);
  const providers = draft?.providers || stored;
  const dirty = draft !== null;
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [status, setStatus] = useState('');
  const [use, setUse] = useState(null);
  const [keys, setKeys] = useState(null);
  const [found, setFound] = useState(null);
  const [importing, setImporting] = useState(false);
  const importRun = useRef(0);
  useDirty(onDirty, `providers:${harness.id}`, dirty);

  const edit = change => {
    revision.current++;
    setDraft(current => ({ providers: change(current?.providers || stored), rev: current?.rev ?? harness.providers_rev ?? '' }));
    setStatus('');
    setUse(null); // an offer to remove models belongs to the list it was made for
  };

  // The key status of the saved list, read again whenever the stored list changes (a save, a reload).
  useEffect(() => {
    let live = true;
    setKeys(null);
    if (!(harness.providers || []).some(provider => provider.key_env)) return undefined;
    request(`${providersURL(machineID, harness.id)}/key-status`).then(answer => { if (live && alive()) setKeys(answer); }).catch(() => { if (live && alive()) setKeys(null); });
    return () => { live = false; };
  }, [machineID, harness.id, harness.providers_rev]);

  const importConfig = async () => {
    if (importing) return;
    const run = ++importRun.current;
    setError('');
    setStatus('');
    setImporting(true);
    try {
      const answer = await request(`${providersURL(machineID, harness.id)}/import`, { method: 'POST', body: {} });
      if (alive() && run === importRun.current) setFound(answer);
    } catch (problem) {
      // 503: the machine is offline or its supervisor predates providers; 502: the OpenCode config could not be read.
      if (alive() && run === importRun.current) setError(problem.status === 503 ? `Can't read the OpenCode config now: ${problem.message}` : problem.status === 502 ? `The OpenCode config could not be read: ${problem.message}` : problem.message || 'Could not read the OpenCode config');
    } finally {
      if (alive() && run === importRun.current) setImporting(false);
    }
  };
  const takeSuggestions = picked => {
    const result = addSuggestions(providers, picked);
    if (result.taken) edit(() => result.providers);
    setStatus(result.status);
    setFound(null);
  };

  const save = async removeModels => {
    setError('');
    setStatus('');
    setUse(null);
    const problem = providersError(providers);
    if (problem) {
      setError(problem);
      return;
    }
    setSaving(true);
    const started = revision.current, sent = providers, rev = draft?.rev ?? harness.providers_rev ?? '';
    try {
      const saved = await request(providersURL(machineID, harness.id), { method: 'PUT', body: providersBody(sent, rev, removeModels === true) });
      if (!alive()) return;
      const next = saved?.providers_rev;
      if (next !== undefined) setDraft(current => current && { ...current, rev: next });
      const refreshed = await onSaved(harness.id);
      if (!alive()) return;
      if (!refreshed) onSnapshot(harness.id, next === undefined ? { providers: saved?.providers || [] } : { providers: saved?.providers || [], providers_rev: next, ...(saved?.models ? { models: saved.models, models_rev: saved.models_rev } : {}) });
      if (revision.current === started) setDraft(null);
      setStatus(removeModels === true ? 'Providers saved; their models were removed.' : 'Providers saved.');
    } catch (failure) {
      if (!alive()) return;
      if (failure.status === 409 && failure.code === 'stale_providers') onStale(harness);
      else if (failure.status === 409 && providerInUse(failure)) {
        // The offer is for the list that was sent: one edited while the save was in flight would be offered the removal of models it never asked about.
        if (revision.current === started) setUse(providerInUse(failure));
        else setError('The list changed while it was being saved, so nothing was checked. Save again.');
      }
      else setError(failure.message || 'Could not save the providers');
    } finally {
      if (alive()) setSaving(false);
    }
  };

  return html`
    <div class="harness-providers" id=${`${prefix}harness-providers-${harness.id}`}>
      <h4>Providers of ${harness.label || harness.name}</h4>
      <p class="settings-desc">OpenAI-compatible endpoints this harness can use. Their models are listed as <code>provider-id/model</code> below. Only the name of the environment variable that holds the key is stored; set the variable itself on the machine that runs the agents.</p>
      <div class="harness-provider-rows">
        ${!providers.length && html`<p class="settings-intro">No custom providers.</p>`}
        ${providers.map((provider, index) => {
          const saved = (harness.providers || []).find(item => item.id === provider.id);
          const key = keyState(provider, saved, keys);
          const change = field => event => { const value = event.currentTarget.value; edit(list => updateProvider(list, index, { [field]: value })); };
          const name = provider.id || `provider ${index + 1}`;
          return html`
            <div class="harness-provider-row harness-detail-row" data-provider=${provider.id} key=${index}>
              <div class="harness-detail-top">
                <code class="harness-model-slug">${provider.id || 'new provider'}</code>
                <span class=${`harness-provider-key is-${key.kind}`} role="status">${key.text}</span>
                <${IconButton} name="trash" label=${`Remove ${name}`} danger className="harness-provider-remove" onClick=${() => edit(list => removeProvider(list, index))} />
              </div>
              <div class="harness-detail-fields">
                <input type="text" name="id" maxLength="40" autocomplete="off" spellcheck=${false} placeholder="id (model prefix)" aria-label=${`Id of ${name}`} value=${provider.id} onInput=${change('id')} />
                <input type="text" name="name" maxLength="80" autocomplete="off" placeholder="Name" aria-label=${`Name of ${name}`} value=${provider.name} onInput=${change('name')} />
                <input type="text" name="base_url" autocomplete="off" spellcheck=${false} placeholder="https://host/v1" aria-label=${`Endpoint URL of ${name}`} value=${provider.base_url} onInput=${change('base_url')} />
                <input type="text" name="key_env" autocomplete="off" spellcheck=${false} placeholder="KEY_VARIABLE (a name, not the key)" aria-label=${`Key variable of ${name}`} value=${provider.key_env} onInput=${change('key_env')} />
                <input type="text" name="context_tokens" inputmode="numeric" autocomplete="off" placeholder="Context tokens (optional)" aria-label=${`Context tokens of ${name}`} value=${provider.context_tokens} onInput=${change('context_tokens')} />
                <input type="text" name="output_tokens" inputmode="numeric" autocomplete="off" placeholder="Output tokens (optional)" aria-label=${`Output tokens of ${name}`} value=${provider.output_tokens} onInput=${change('output_tokens')} />
              </div>
            </div>
          `;
        })}
      </div>
      <div class="machine-harness-actions">
        <${IconButton} name="plus" label="Add a provider" className="harness-provider-add" disabled=${providers.length >= PROVIDERS_MAX} onClick=${() => edit(list => addProvider(list))} />
        <${IconButton} name="arrow-down" label=${importing ? 'Reading the OpenCode config…' : 'Import providers from the OpenCode config'} className="harness-provider-import" disabled=${importing} onClick=${importConfig} />
        <button type="button" class="btn btn-primary" disabled=${!dirty || saving} onClick=${() => save(false)}>Save providers</button>
      </div>
      <p class="error" role="alert">${error}</p>
      ${use && html`
        <div class="harness-provider-use" role="alert">
          <span>${inUseMessage(use)}</span>
          <${IconButton} name="trash" label=${removeWithModelsLabel(use)} danger className="harness-provider-remove-models" disabled=${saving} onClick=${() => save(true)} />
          <${IconButton} name="close" label="Keep the provider" className="harness-provider-keep" onClick=${() => setUse(null)} />
        </div>
      `}
      ${found && html`<${SuggestionList} found=${found} providers=${providers} onAdd=${takeSuggestions} onClose=${() => setFound(null)} onDirty=${onDirty} dirtyKey=${`provider-suggestions:${harness.id}`} />`}
      <p class="settings-desc" role="status">${status}</p>
    </div>
  `;
}

function SuggestionList({ found, providers, onAdd, onClose, onDirty, dirtyKey }) {
  const suggestions = found.suggestions || [], skipped = found.skipped || [];
  const [picked, setPicked] = useState(() => new Set());
  useDirty(onDirty, dirtyKey, picked.size > 0);
  const toggle = id => setPicked(current => {
    const next = new Set(current);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    return next;
  });
  const submit = event => {
    event.preventDefault();
    onAdd(suggestions.filter(item => picked.has(item.provider.id)));
  };
  return html`
    <form class="machine-harness-form harness-provider-suggestions" onSubmit=${submit}>
      <strong>${suggestions.length ? `${suggestions.length} in your OpenCode config${found.source ? ` (${found.source})` : ''} — tick the ones to add` : 'No custom providers in your OpenCode config.'}</strong>
      <div class="harness-candidate-list">
        ${suggestions.map(item => {
          const state = suggestionState(item, providers), id = item.provider.id;
          const notes = [item.provider.base_url, (item.models || []).length ? `models: ${item.models.map(model => model.slug).join(', ')}` : '', state === 'already' ? 'already in the list' : '', item.problem ? `can't be taken: ${item.problem}` : '', item.inline_key ? 'its key is written into the config: set an environment variable and enter its name after adding' : ''].filter(Boolean).join(' · ');
          return html`
            <label class="machine-harness-enabled harness-detect-row" data-provider=${id} key=${id}>
              <input type="checkbox" disabled=${state !== 'ok'} checked=${picked.has(id)} aria-label=${`Add ${id}`} onChange=${() => toggle(id)} />
              <span>${`${item.provider.name || id} · ${id} · ${notes}`}</span>
            </label>
          `;
        })}
      </div>
      ${skipped.length > 0 && html`<p class="settings-desc harness-provider-skipped">${`Not offered: ${skipped.map(item => `${item.id} (${item.reason})`).join(', ')}`}</p>`}
      <div class="machine-harness-actions">
        <button type="submit" class="btn btn-primary" disabled=${!picked.size}>Add selected</button>
        <button type="button" class="btn" onClick=${onClose}>Close</button>
      </div>
    </form>
  `;
}

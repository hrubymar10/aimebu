// Import-free: scripts/test-harness-model.cjs runs this module in a vm.

// The server refuses a list longer than this, so a longer detected list is cut to it rather than failing the save.
export const MODELS_MAX = 64;
export const FAMILIES = [['claude', 'Claude Code'], ['codex', 'Codex'], ['pi', 'Pi'], ['opencode', 'OpenCode'], ['vibe', 'Mistral Vibe']];
export const MODEL_ID = /^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$/;
export const familyName = family => FAMILIES.find(([key]) => key === family)?.[1] || family;

export const lines = value => value.split('\n').map(row => row.trim()).filter(Boolean);
export function pairs(value, separator) {
  const result = [];
  for (const row of lines(value)) {
    const at = row.indexOf(separator);
    if (at <= 0 || at === row.length - separator.length) throw new Error(`Use one ${separator} pair per line`);
    result.push([row.slice(0, at).trim(), row.slice(at + separator.length).trim()]);
  }
  return result;
}

export const harnessFromLauncher = row => ({ ...row.config, variant: row.config.name, launcher_path: row.resolved_command, version: row.version, version_problem: row.version_problem,
  available: row.available, unavailable_reason: row.unavailable_reason, active_agents: row.active_agents || 0, models: row.models || [], models_rev: row.models_rev || '', providers: row.providers || [], providers_rev: row.providers_rev || '' });

export const harnessStatus = harness => !harness.enabled ? 'Disabled' : harness.available ? 'Available' : 'Unavailable';
export const harnessTitle = harness => harness.label || `${familyName(harness.tool)} · ${harness.variant}`;
export const harnessLabel = harness => harness.label || `${familyName(harness.tool)}${harness.containerized ? ' (Docker)' : ''}`;

export function persistedDefinition(harness) {
  const { variant, launcher_path, version, version_problem, available, unavailable_reason, active_agents, models, models_rev, providers, providers_rev, ...definition } = harness;
  return definition;
}

export const formFromHarness = harness => ({
  label: harness.label || '', command: harness.command || '', args: (harness.args || []).join('\n'),
  env_refs: Object.entries(harness.env_refs || {}).map(([target, source]) => `${target}=${source}`).join('\n'),
  containerized: !!harness.containerized, agent_server_url: harness.agent_server_url || '',
  path_maps: (harness.path_maps || []).map(map => `${map.host} → ${map.visible}`).join('\n'),
  visible_roots: (harness.visible_roots || []).join('\n'), supports_toolbox: !!harness.supports_toolbox,
});

export const launcherBody = (harness, form, enabled) => ({
  ...persistedDefinition(harness), label: form.label.trim(), enabled, command: form.command.trim(),
  args: lines(form.args), env_refs: Object.fromEntries(pairs(form.env_refs, '=')),
  containerized: form.containerized, agent_server_url: form.agent_server_url.trim(),
  path_maps: pairs(form.path_maps, '→').map(([host, visible]) => ({ host, visible })),
  visible_roots: lines(form.visible_roots), supports_toolbox: form.supports_toolbox,
});

export const enabledBody = (harness, enabled) => ({ ...persistedDefinition(harness), enabled });

export const createBody = ({ tool, label, command, containerized }) => ({
  tool, label, command, args: [], env_refs: {}, enabled: true, containerized, path_maps: [], visible_roots: [], supports_toolbox: !(tool === 'pi' && containerized),
});

export function addModel(models, tool, slug, label = '', note = '') {
  slug = slug.trim();
  if (!MODEL_ID.test(slug)) throw new Error(`"${slug}" is not a valid model id.`);
  if (tool === 'opencode' && !slug.includes('/')) throw new Error('An OpenCode model id is provider/model.');
  if (models.some(model => model.slug === slug)) return models;
  return [...models, { slug, label: label.trim() || slug, note: note.trim(), default: models.length === 0 }];
}

export function removeModel(models, slug) {
  const removed = models.find(model => model.slug === slug);
  const rest = models.filter(model => model.slug !== slug);
  return removed?.default && rest.length ? rest.map((model, index) => ({ ...model, default: index === 0 })) : rest;
}

export const setDefaultModel = (models, slug) => models.map(model => ({ ...model, default: model.slug === slug }));
export const updateModel = (models, slug, change) => models.map(model => model.slug === slug ? { ...model, ...change } : model);
export const modelsError = models => models.length && models.filter(model => model.default).length !== 1 ? 'Choose exactly one default model.' : '';

export function addFoundModels(models, tool, picked) {
  let next = models, added = 0;
  for (const model of picked) {
    if (next.length >= MODELS_MAX) break;
    try {
      const grown = addModel(next, tool, model.slug, model.label, model.note);
      if (grown !== next) {
        next = grown;
        added++;
      }
    } catch { /* refused by the list */ }
  }
  const full = next.length >= MODELS_MAX;
  const status = added ? `${added} added to the list${added < picked.length && full ? ` (limit ${MODELS_MAX})` : ''}. Save models to keep them.`
    : full ? `The list is full (limit ${MODELS_MAX}).` : 'Nothing new to add.';
  return { models: next, added, status };
}

export function detectionFailure(problem, containerized) {
  const detail = problem?.message || "Couldn't detect the models.";
  const noModels = detail.match(/(?:pi|opencode|codex) reports no models — is it logged in\? Run \/login in it/);
  const folderMissing = /(?:not mounted|not visible|does not exist or is not visible)/i.test(detail);
  const summary = containerized && problem?.code === 'detect_failed' && problem.detectDir && folderMissing
    ? `Detection blocked — please mount ${problem.detectDir} to your harness Docker, or pick a mounted folder below.`
    : noModels ? noModels[0] : containerized ? detail : problem?.code === 'detect_failed' ? 'Model detection failed. Check the harness or try another folder.' : detail;
  return { summary, detail: detail === summary ? '' : detail };
}

export function initialModels(candidates) {
  const all = (candidates || []).filter(candidate => !candidate.already_listed);
  const models = all.slice(0, MODELS_MAX).map((candidate, index) => ({ slug: candidate.slug, label: candidate.label || candidate.slug, note: candidate.note || '', default: index === 0 }));
  return { models, cut: all.length - models.length };
}

export function detectNotes(filled) {
  const notes = [];
  const empty = filled.filter(done => !done.saved).length;
  const cut = filled.find(done => done.cut > 0);
  const reason = filled.find(done => !done.saved && done.reason)?.reason;
  if (empty) notes.push(`${empty === 1 ? 'A harness has' : `${empty} harnesses have`} no models yet.${reason ? ` ${reason}` : ''} Open Models to retry Auto-detect, in a folder you choose if the harness needs one, or add a model by hand.`);
  if (cut) notes.push(`A harness offered more models than a list can hold, so only the first ${cut.kept} were added.`);
  return notes.join(' ');
}

export const machineSummary = machine => machine?.approved === false ? 'Approval pending' :
  machine?.last_seen ? `Last seen ${new Date(machine.last_seen).toLocaleString()}` : 'Never connected';

// The status word of a harness and the title of its tab in the list beside the detail.
export const tabTitle = harness => `${harness.label || harness.name}: ${harnessStatus(harness)}, ${(harness.models || []).length} models`;

export const staleModelsMessage = harness => `Someone changed the models of ${harness.label || harness.name} in the meantime. The list was reloaded; make your changes again.`;

// The tab to select after a list is (re)loaded: the chosen one while it is still there, else the first.
export const chosenHarness = (rows, selected) => rows.some(row => row.id === selected) ? selected : rows[0]?.id || '';

// The tab a key moves to from `index` among `count` tabs, or -1 when the key is not a navigation key.
export function tabKeyTarget(key, index, count) {
  const to = { ArrowDown: (index + 1) % count, ArrowUp: (index + count - 1) % count, Home: 0, End: count - 1 }[key];
  return to === undefined ? -1 : to;
}

// A label for a role default: the machine's name, the harness label and the model label, or the ids when the harness is not known.
export function defaultLabel(item, here, other) {
  const harness = here?.harness || other, owner = here ? here.machine : other?.machine;
  if (!harness) return `${item.launcher_id} · ${item.model}`;
  const model = (harness.models || []).find(entry => entry.slug === item.model);
  return [owner, harness.label || harness.name, model?.label || item.model].filter(Boolean).join(' · ');
}

// Custom providers of an OpenCode harness (OpenAI-compatible endpoints). The server decides what a list may hold (docs/api.md); these keep the obvious
// mistakes out of a save and shape the request. Only the NAME of the key variable is ever held, never a key.
export const PROVIDERS_MAX = 16;
export const PROVIDER_NPM = '@ai-sdk/openai-compatible';
export const PROVIDER_ID = /^[a-z][a-z0-9-]{0,39}$/;
export const KEY_ENV = /^[A-Z][A-Z0-9_]*$/;

export const emptyProvider = () => ({ id: '', name: '', npm: PROVIDER_NPM, base_url: '', key_env: '', context_tokens: '', output_tokens: '' });
const limit = value => (value === undefined || value === null ? '' : String(value === 0 ? '' : value));
// What is held while editing: the stored provider with its limits as text.
export const editableProvider = provider => ({ ...emptyProvider(), ...provider, name: provider.name || '', key_env: provider.key_env || '', context_tokens: limit(provider.context_tokens), output_tokens: limit(provider.output_tokens) });
export const addProvider = (providers, provider = emptyProvider()) => (providers.length >= PROVIDERS_MAX ? providers : [...providers, editableProvider(provider)]);
export const removeProvider = (providers, index) => providers.filter((_, at) => at !== index);
export const updateProvider = (providers, index, change) => providers.map((provider, at) => (at === index ? { ...provider, ...change } : provider));

// The first thing wrong with a list, or '' (the server checks the rest, and says why in the same words).
export function providersError(providers) {
  const seen = new Set();
  for (const [index, provider] of providers.entries()) {
    const which = provider.id ? `Provider ${provider.id}` : `Provider ${index + 1}`;
    if (!PROVIDER_ID.test(provider.id)) return `${which}: the id is lowercase letters, digits and hyphens, starting with a letter (at most 40).`;
    if (seen.has(provider.id)) return `Two providers are called ${provider.id}.`;
    seen.add(provider.id);
    if (!provider.base_url.trim()) return `${which}: enter the endpoint URL (https://…).`;
    if (provider.key_env.trim() && !KEY_ENV.test(provider.key_env.trim())) return `${which}: the key variable is upper case letters, digits and underscores (a name, not the key).`;
    const [context, output] = [provider.context_tokens, provider.output_tokens].map(value => String(value).trim());
    if (!context !== !output) return `${which}: give both token limits or neither.`;
    if (context && !(/^[1-9][0-9]*$/.test(context) && /^[1-9][0-9]*$/.test(output))) return `${which}: token limits are whole numbers above zero.`;
  }
  return '';
}

// The request body of a save; the limits go only as a pair.
export function providersBody(providers, rev, removeModels = false) {
  const body = { providers: providers.map(provider => {
    const out = { id: provider.id.trim(), name: provider.name.trim() || provider.id.trim(), npm: provider.npm || PROVIDER_NPM, base_url: provider.base_url.trim() };
    if (provider.key_env.trim()) out.key_env = provider.key_env.trim();
    const [context, output] = [provider.context_tokens, provider.output_tokens].map(value => Number(String(value).trim()));
    if (context > 0 && output > 0) { out.context_tokens = context; out.output_tokens = output; }
    return out;
  }), expected_rev: rev };
  if (removeModels) body.remove_models = true;
  return body;
}

// The offer a refused save makes when providers it would drop still have models: every such provider with its model count (the server lists all of
// them in in_use, since remove_models deletes the models of every one), and the total.
export function providerInUse(problem) {
  if (problem?.code !== 'provider_in_use') return null;
  const listed = Array.isArray(problem.body?.in_use) && problem.body.in_use.length ? problem.body.in_use : [{ provider_id: problem.body?.provider_id || '', model_count: problem.body?.model_count || 0 }];
  const providers = listed.map(item => ({ id: item.provider_id || '', count: item.model_count || 0 }));
  return { providers, count: providers.reduce((sum, item) => sum + item.count, 0) };
}
const idsOf = use => use.providers.map(item => item.id).filter(Boolean).join(', ');
export const removeWithModelsLabel = use => `Remove ${idsOf(use) ? `${idsOf(use)} ` : ''}with ${use.providers.length > 1 ? 'their' : 'its'} ${use.count} model${use.count === 1 ? '' : 's'}`;
export const inUseMessage = use => `${use.providers.map(item => `${item.id || 'a provider'} (${item.count})`).join(', ')} ${use.providers.length > 1 ? 'still have' : 'still has'} models listed.`;

export const staleProvidersMessage = harness => `Someone changed the providers of ${harness.label || harness.name} in the meantime. The list was reloaded; make your changes again.`;

// What the key of one provider looks like on the machine that runs the turns: a word and a kind (set / missing / unknown), from the key-status answer
// (null while it cannot be known) and the saved key variable, never a value. A key variable that is edited and not saved yet has no status to show.
export function keyState(provider, saved, status) {
  const name = provider.key_env.trim();
  if (!name) return { kind: 'none', text: 'No key variable' };
  if (!saved || saved.key_env !== name) return { kind: 'unsaved', text: 'Save to check the key' };
  if (!status) return { kind: 'unknown', text: 'Key not checked' };
  if (status.machine_online === false) return { kind: 'unknown', text: 'Machine offline: key not checked' };
  if (status.supervisor_supported === false) return { kind: 'unknown', text: 'Supervisor update needed to check the key' };
  const entry = (status.keys || []).find(item => item.provider_id === provider.id);
  if (!entry || entry.set === null || entry.set === undefined) return { kind: 'unknown', text: status.error ? `Key not checked: ${status.error}` : 'Key not checked' };
  return entry.set ? { kind: 'set', text: `${name} is set` } : { kind: 'missing', text: `${name} is not set on that machine` };
}

// A suggestion from the import cannot be taken when it is already in the list or the server found a problem with it.
export const suggestionState = (suggestion, providers) => suggestion.already_added || providers.some(item => item.id === suggestion.provider.id) ? 'already' : suggestion.problem ? 'problem' : 'ok';

// Picked suggestions join the unsaved list (up to its limit); `taken` counts the ones that went in.
export function addSuggestions(providers, picked) {
  let next = providers, taken = 0, keyless = 0;
  for (const suggestion of picked) {
    if (next.length >= PROVIDERS_MAX || suggestionState(suggestion, next) !== 'ok') continue;
    next = addProvider(next, suggestion.provider);
    taken++;
    if (suggestion.inline_key) keyless++;
  }
  const status = taken ? `${taken} added to the list${keyless ? `; ${keyless === 1 ? 'its' : 'their'} key was written into the OpenCode config, so set an environment variable and enter its name` : ''}. Save providers to keep ${taken === 1 ? 'it' : 'them'}.` : next.length >= PROVIDERS_MAX ? `The list is full (limit ${PROVIDERS_MAX}).` : 'Nothing new to add.';
  return { providers: next, taken, status };
}

// What a machine runs (a harness's command, models and providers) is the owner's alone: an admin sees it, and the controls that are off say why.
export const OWNER_ONLY = 'Only the owner can change what a machine runs.';

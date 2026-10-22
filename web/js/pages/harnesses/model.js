// Import-free: scripts/test-harness-model.cjs runs this module in a vm.

// The server refuses a list longer than this, so a longer detected list is cut to it rather than failing the save.
export const MODELS_MAX = 64;
export const FAMILIES = [['claude', 'Claude Code'], ['codex', 'Codex'], ['pi', 'Pi'], ['opencode', 'OpenCode']];
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

export const harnessFromLauncher = row => ({ ...row.config, variant: row.config.name, launcher_path: row.resolved_command, version: row.version,
  available: row.available, unavailable_reason: row.unavailable_reason, active_agents: row.active_agents || 0, models: row.models || [] });

export const harnessStatus = harness => !harness.enabled ? 'Disabled' : harness.available ? 'Available' : 'Unavailable';
export const harnessTitle = harness => harness.label || `${familyName(harness.tool)} · ${harness.variant}`;
export const harnessLabel = harness => harness.label || `${familyName(harness.tool)}${harness.containerized ? ' (Docker)' : ''}`;

export function persistedDefinition(harness) {
  const { variant, launcher_path, version, available, unavailable_reason, active_agents, models, ...definition } = harness;
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

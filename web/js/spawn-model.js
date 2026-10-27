// Picking a model in the Spawn dialog. The model list is rebuilt whenever the catalog arrives or the tool changes, and the person's own
// choice must survive that; a model that only a name is known for (a v1 agent's "opus5.5", a Claude session's "claude-opus-4-5-20251101")
// is matched to the list by its family when it is not listed verbatim. No DOM here, so the rules can be tested.

const FAMILIES = ['opus', 'sonnet', 'haiku'];

// The Claude family a model name belongs to ("opus", "sonnet", "haiku"), or ''.
export const modelFamily = name => FAMILIES.find(family => String(name || '').toLowerCase().includes(family)) || '';

// matchModel picks the entry of `models` ([{ value, label }]) that `wanted` names: the same id, the same id ignoring case, or else the first
// entry of the same Claude family. '' when nothing fits, so the caller keeps the list's own default.
export function matchModel(models, wanted) {
  if (!wanted) return '';
  const exact = models.find(model => model.value === wanted);
  if (exact) return exact.value;
  const lower = String(wanted).toLowerCase();
  const loose = models.find(model => model.value.toLowerCase() === lower);
  if (loose) return loose.value;
  const family = modelFamily(wanted);
  if (!family) return '';
  const same = models.find(model => modelFamily(model.value) === family || modelFamily(model.label) === family);
  return same ? same.value : '';
}

// The model to select after the list was rebuilt: what the person picked for this tool when it is still offered, else the one named by the
// start (the v1 agent's or the session's), else the list's own default, else the first.
export function chooseModel(models, { picked = null, toolKey = '', named = '', preferred = '' } = {}) {
  if (picked && picked.toolKey === toolKey && models.some(model => model.value === picked.model)) return picked.model;
  return matchModel(models, named) || (models.some(model => model.value === preferred) ? preferred : '') || models[0]?.value || '';
}

// The key of a tool entry of the catalog, which says whether the tool is still the same after a reload.
export const toolKey = tool => (tool ? tool.launcher_id || `${tool.tool || ''}/${tool.variant || ''}` : '');

// The session entry of the Spawn dialog. A Claude Code session can only be continued through a launcher that sees the Claude home it sits in:
// the native one, or a container launcher that says it shares the host's (harness-docker's claude_home_shared). With another one selected the
// dialog says so up front, and offers the launcher that can, instead of listing sessions that fail once picked.
// Returns { enabled, note, switchTo } (switchTo is the catalog index of the launcher to use, -1 when there is none).
export function sessionEntryState(selected, catalog) {
  const canContinue = tool => tool.tool === 'claude' && tool.available && (!tool.containerized || tool.shares_claude_home);
  if (!selected || selected.tool !== 'claude' || !selected.containerized || selected.shares_claude_home) return { enabled: true, note: '', switchTo: -1 };
  const label = selected.label || 'This launcher';
  const switchTo = catalog.findIndex(canContinue);
  if (switchTo >= 0) return { enabled: false, note: `${label} keeps its own Claude home, so a session cannot be continued through it.`, switchTo };
  return { enabled: false, note: `${label} keeps its own Claude home, so a session cannot be continued through it. Sessions need the native Claude Code harness, or a Docker launcher that shares Claude's home (harness-docker's claude_home_shared); add one under Settings → Harnesses.`, switchTo: -1 };
}

// A warning when the model chosen for the agent is not the one the session ran on, so a session is not continued on another model unnoticed.
// `sessionModel` is what Claude recorded ("claude-opus-4-5-20251101") or a v1 slug ("opus5.5"); `chosen` is { value, label }.
export function modelChangeWarning(sessionModel, chosen) {
  const was = modelFamily(sessionModel);
  if (!was || !chosen) return '';
  if ((modelFamily(chosen.value) || modelFamily(chosen.label)) === was) return '';
  return `This session ran on ${sessionModel}; the agent will continue it on ${chosen.label || chosen.value}.`;
}

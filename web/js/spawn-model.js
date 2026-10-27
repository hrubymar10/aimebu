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

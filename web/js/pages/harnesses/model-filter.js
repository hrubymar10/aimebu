// The model list's filter: pure functions, so the page and its test share one answer.

// The provider of a model id is what comes before the first `/`; an id without one belongs to the harness's own provider.
export const providerOf = (slug, own) => {
  const at = String(slug).indexOf('/');
  return at > 0 ? slug.slice(0, at) : own;
};

// Each provider once, with how many models it has, by name.
export function providerCounts(models, own) {
  const counts = new Map();
  for (const model of models) {
    const name = providerOf(model.slug, own);
    counts.set(name, (counts.get(name) || 0) + 1);
  }
  return [...counts].map(([name, count]) => ({ name, count })).sort((a, b) => a.name.localeCompare(b.name));
}

const words = query => String(query || '').toLowerCase().split(/\s+/).filter(Boolean);

// A model matches when every word of the query is in its id, label or note (case-insensitive) and it belongs to the chosen provider.
export function matches(model, { query, provider }, own) {
  if (provider && providerOf(model.slug, own) !== provider) return false;
  const text = `${model.slug}\n${model.label || ''}\n${model.note || ''}`.toLowerCase();
  return words(query).every(word => text.includes(word));
}

const sameModel = (a, b) => (a.label || '') === (b.label || '') && (a.note || '') === (b.note || '') && !!a.default === !!b.default;

// What the list shows: the matching models, plus the default and any model with unsaved edits even when the filter hides them (with the reason).
// `matched` counts the real matches alone, so an empty result is told even while a pinned model stays on screen.
export function filterModels(models, stored, filter, own) {
  const saved = new Map(stored.map(model => [model.slug, model]));
  const rows = [], reasons = {};
  let matched = 0;
  for (const model of models) {
    const hit = matches(model, filter, own);
    if (hit) matched++;
    if (hit) { rows.push({ model }); continue; }
    const before = saved.get(model.slug);
    if (model.default) reasons[model.slug] = 'default';
    else if (!before || !sameModel(before, model)) reasons[model.slug] = 'edited';
    else continue;
    rows.push({ model });
  }
  return { rows, reasons, matched };
}

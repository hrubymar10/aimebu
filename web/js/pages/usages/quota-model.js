// Import-free: scripts/test-quota-model.cjs runs this module in a vm.

export const TOOLS = [['claude', 'Claude'], ['codex', 'Codex']];
export const RANGES = { session_max: [1, 100], weekly_max: [1, 100], drop_points: [1, 100], warmup_seconds: [0, 600], min_dwell_minutes: [0, 120] };
export const DEFAULT_LIMIT = 95;

export const clone = value => JSON.parse(JSON.stringify(value));
const canon = value => (Array.isArray(value) ? value.map(canon) : value && typeof value === 'object' ? Object.fromEntries(Object.keys(value).sort().map(key => [key, canon(value[key])])) : value);
export const same = (a, b) => JSON.stringify(canon(a)) === JSON.stringify(canon(b));
export const when = value => { const date = value ? new Date(value) : null; return !date ? 'never' : Number.isNaN(date.getTime()) ? 'unknown' : date.toLocaleString(); };
export const percent = value => (typeof value === 'number' ? `${Math.round(value)}%` : '-');

export function describeDecision(decision) {
  const route = decision.from && decision.to ? `${decision.from} → ${decision.to}` : decision.from || decision.to || '';
  const text = ({
    // An entry of the journal written while dry run existed says it was never made.
    switch: decision.dry_run ? `would switch ${route}` : `switched ${route}`,
    stay: `stay on ${decision.from || 'the active profile'}`,
    hold: 'hold', idle: 'idle', error: 'error',
  })[decision.action] || String(decision.action || '');
  return decision.reason && ['hold', 'error', 'switch', 'stay'].includes(decision.action) ? `${text}: ${decision.reason}` : text;
}

// Whether a tool's draft differs from the stored configuration; a configuration the server has not saved yet (suggested defaults) can be
// saved even unchanged, so the owner can accept it as it is.
export const isDirty = (data, drafts, tool) => !!data?.tools?.[tool] && !!drafts[tool] && !same(drafts[tool], data.tools[tool].config);
export const isSavable = (data, drafts, tool) => isDirty(data, drafts, tool) || data?.tools?.[tool]?.saved === false;

// What a number box's text means: an empty box is not a number.
export const numberOf = text => (text === '' ? NaN : Number(text));
export const boxText = value => (Number.isFinite(value) ? String(value) : '');

// A draft the owner can save, or the sentence that says why not.
export function numbersProblem(draft) {
  const bad = (key, value) => !Number.isInteger(value) || value < RANGES[key][0] || value > RANGES[key][1];
  const range = key => `${RANGES[key][0]} to ${RANGES[key][1]}`;
  for (const entry of [...draft.profiles, ...(draft.after_reset?.profiles || [])]) {
    if (bad('session_max', entry.session_max) || bad('weekly_max', entry.weekly_max)) return `The limits for ${entry.name} must be whole numbers from ${range('session_max')}.`;
  }
  if (draft.enabled && draft.after_reset && draft.after_reset.profiles.length < 2) return 'The order after the reset needs at least two profiles to turn switching on.';
  if (draft.after_reset && bad('drop_points', draft.after_reset.drop_points)) return `The reset drop must be a whole number from ${range('drop_points')}.`;
  if (bad('warmup_seconds', draft.warmup_seconds)) return `Warm-up must be a whole number of seconds from ${range('warmup_seconds')}.`;
  if (bad('min_dwell_minutes', draft.min_dwell_minutes)) return `The minimum time between switches must be a whole number of minutes from ${range('min_dwell_minutes')}.`;
  return '';
}

// Protecting a profile takes it out of both orders, and out of the watched slot of the after-reset order.
export function protectProfile(draft, name, on) {
  draft.protected = draft.protected.filter(item => item !== name);
  if (!on) return;
  draft.protected.push(name);
  draft.profiles = draft.profiles.filter(entry => entry.name !== name);
  if (draft.after_reset) {
    draft.after_reset.profiles = draft.after_reset.profiles.filter(entry => entry.name !== name);
    if (draft.after_reset.profile === name) draft.after_reset.profile = draft.profiles[0]?.name || '';
  }
}

export const afterResetFor = draft => ({ profile: draft.profiles[0]?.name || '', drop_points: 20, profiles: clone(draft.profiles) });

// Profiles of the store that can still be added to an order: not listed in it, not protected.
export const addable = (stored, entries, protectedNames) => stored.filter(name => !entries.some(entry => entry.name === name) && !protectedNames.includes(name));
export const protectNames = (stored, draft) => [...new Set([...stored, ...draft.protected])];
export const watchChoices = after => [...new Set([...after.profiles.map(entry => entry.name), after.profile].filter(Boolean))];
export const orderEntry = name => ({ name, session_max: DEFAULT_LIMIT, weekly_max: DEFAULT_LIMIT });

export function moveEntry(entries, index, delta) {
  const target = index + delta;
  [entries[index], entries[target]] = [entries[target], entries[index]];
}

export const decisionText = decision => `${when(decision.at)}: ${describeDecision(decision)}`;
export const decisionProfiles = decision => Object.entries(decision.profiles || {}).map(([name, value]) => `${name}: ${value?.status || '?'}, session ${percent(value?.session)}, weekly ${percent(value?.weekly)}`);
export const decisionAction = decision => String(decision.action || '').replace(/[^a-z]/g, '');
export const resetText = state => `A weekly reset was seen at ${when(state.reset_latched_at)}${typeof state.weekly_max_seen === 'number' ? ` (the weekly peak was ${percent(state.weekly_max_seen)})` : ''}; the order after the reset is in effect. Re-arm when the next week starts.`;
export const trippedText = state => `Automatic switching is paused: it switched too often in one hour (${when(state.tripped_at)}). Check the order and the limits, then re-enable it.`;

// The key of the control that should keep focus after a change, or its nearest sibling when that control is now disabled.
export function focusFallbacks(key) {
  const parts = key.split(':');
  return parts.length === 3 ? [`${parts[0]}:${parts[1]}:${parts[2] === 'up' ? 'down' : 'up'}`, `${parts[0]}:${parts[1]}:remove`] : [];
}

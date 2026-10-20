// Plain words for the limit guard's verdicts, shared by Settings and the Health list. Pure: no DOM.
export const GUARD_TOOLS = [['claude', 'Claude'], ['codex', 'Codex']];

const percent = value => typeof value === 'number' ? `${Math.round(value)}%` : '-';

// One sentence on where a tool stands: which profile, how full, and what the guard is doing about it.
export function guardSentence(label, view) {
  const result = view?.result || {};
  const full = typeof result.percent === 'number' ? `${percent(result.percent)} of its ${result.window || ''} window`.replace('  ', ' ') : '';
  switch (result.state) {
    case 'soft': return `${label} (${result.profile}) is at ${full}: new turns are held, running ones finish.`;
    case 'hard': return `${label} (${result.profile}) is at ${full}: new turns are held until it resets or the account is switched.`;
    case 'ok': return `${label} (${result.profile}) is at ${full}.`;
    case 'off': return `The limit guard is off for ${label}.`;
    default: return `${label}: no clear usage reading yet, nothing is held.`;
  }
}

// What the Health list shows for a tool, or null when there is nothing to say (guard off, or no clear reading).
export function guardHealthRow(tool, label, view) {
  const result = view?.result || {};
  if (!['ok', 'soft', 'hard'].includes(result.state)) return null;
  return { tool, state: result.state, held: !!view.held, percent: result.percent, soft: result.soft, hard: result.hard, text: guardSentence(label, view) };
}

// How many tools are holding new turns: they count toward the Health badge.
export function heldTools(data) { return GUARD_TOOLS.filter(([tool]) => data?.tools?.[tool]?.held).length; }

// A draft the owner can save: thresholds are whole numbers, soft below hard, within 1 to 100.
export function guardProblem(draft) {
  const bad = value => !Number.isInteger(value) || value < 1 || value > 100;
  const check = (name, soft, hard) => bad(soft) || bad(hard) ? `${name}: the thresholds must be whole numbers from 1 to 100.` : soft >= hard ? `${name}: the soft threshold must be below the hard one.` : '';
  return check('Thresholds', draft.soft, draft.hard) || Object.entries(draft.profiles || {}).map(([name, limit]) => check(name, limit.soft, limit.hard)).find(Boolean) || '';
}

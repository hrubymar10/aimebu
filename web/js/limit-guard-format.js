// Plain words for the limit guard's verdicts, shared by Settings and the Health list. Pure: no DOM.
export const GUARD_TOOLS = [['claude', 'Claude'], ['codex', 'Codex']];

const percent = value => typeof value === 'number' ? `${Math.round(value)}%` : '-';

const at = (display, full) => (display === 'left' ? `has ${full}` : `is at ${full}`);

// One sentence on where a tool stands: which profile, how full, and what the guard is doing about it.
// `display` is the owner's Usages setting ("left" or "used"): the sentence says it the way the Usages cards do.
export function guardSentence(label, view, display = 'used') {
  const result = view?.result || {};
  const window = `its ${result.window || ''} window`.replace('  ', ' ');
  const full = typeof result.percent !== 'number' ? '' : display === 'left' ? `${percent(100 - result.percent)} left of ${window}` : `${percent(result.percent)} of ${window}`;
  switch (result.state) {
    case 'soft': return `${label} (${result.profile}) ${at(display, full)}: new turns are held, running ones finish.`;
    case 'hard': return `${label} (${result.profile}) ${at(display, full)}: new turns are held until it resets or the account is switched.`;
    case 'ok': return `${label} (${result.profile}) ${at(display, full)}.`;
    case 'off': return `The limit guard is off for ${label}.`;
    default: return `${label}: no clear usage reading yet, nothing is held.`;
  }
}

// What the Health list shows for a tool, or null when there is nothing to say there: an `ok` reading is not a
// problem and does not belong in Health (it stays in Settings -> Usages), nor does a guard off or no clear reading.
export function guardHealthRow(tool, label, view, display = 'used') {
  const result = view?.result || {};
  if (!['soft', 'hard'].includes(result.state)) return null;
  return { tool, state: result.state, held: !!view.held, percent: result.percent, soft: result.soft, hard: result.hard, display, text: guardSentence(label, view, display) };
}

// How many tools are holding new turns: they count toward the Health badge.
export function heldTools(data) { return GUARD_TOOLS.filter(([tool]) => data?.tools?.[tool]?.held).length; }

// A draft the owner can save: thresholds are whole numbers, soft below hard, within 1 to 100.
export function guardProblem(draft) {
  const bad = value => !Number.isInteger(value) || value < 1 || value > 100;
  const check = (name, soft, hard) => bad(soft) || bad(hard) ? `${name}: the thresholds must be whole numbers from 1 to 100.` : soft >= hard ? `${name}: the soft threshold must be below the hard one.` : '';
  return check('Thresholds', draft.soft, draft.hard) || Object.entries(draft.profiles || {}).map(([name, limit]) => check(name, limit.soft, limit.hard)).find(Boolean) || '';
}

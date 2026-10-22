// What a message's usage popover and tooltip say. No DOM, no network: a test runs it in a vm.
//
// `usage` is what the server sends with a message an agent wrote: the figures of the turn that wrote it ({turn_id, index, count,
// tokens_in?, tokens_out?, tokens_cache_read?, tokens_cache_write?, cost?, model?, partial?}). A figure the harness did not report is
// absent, and is shown as an em dash, never as zero.

export const DASH = '—';

export function formatTokens(value) {
  return Number.isFinite(value) && value >= 0 ? new Intl.NumberFormat('en-US').format(value) : DASH;
}

// Cost to two decimals, four when under a cent (the agent card's rule); it is an estimate, so the callers say so.
export function formatCost(value) {
  return Number.isFinite(value) && value >= 0 ? `$${value.toFixed(value > 0 && value < 0.01 ? 4 : 2)}` : DASH;
}

// The rows of the popover, as [label, value].
export function usageRows(usage) {
  const rows = [
    ['Model', usage?.model || DASH],
    ['Input', formatTokens(usage?.tokens_in)],
    ['Output', formatTokens(usage?.tokens_out)],
    ['Cache read', formatTokens(usage?.tokens_cache_read)],
    ['Cache written', formatTokens(usage?.tokens_cache_write)],
    ['Cost (estimate)', formatCost(usage?.cost)],
  ];
  return rows;
}

// Said under the rows: where this message sits among the turn's, and when the figures are only part of the turn.
export function usageNotes(usage) {
  const notes = [];
  const count = Number(usage?.count) || 1, index = Number(usage?.index) || 1;
  notes.push(count > 1
    ? `These are the figures of the whole turn, which wrote ${count} messages; this is ${index} of ${count}. They are not added up per message.`
    : 'These are the figures of the turn that wrote this message.');
  if (usage?.partial) notes.push('The turn ended before it reported its totals, so these figures are a floor.');
  return notes;
}

// One line for the button's tooltip.
export function usageTooltip(usage) {
  const parts = [];
  if (Number.isFinite(usage?.tokens_in)) parts.push(`${formatTokens(usage.tokens_in)} in`);
  if (Number.isFinite(usage?.tokens_out)) parts.push(`${formatTokens(usage.tokens_out)} out`);
  if (Number.isFinite(usage?.cost)) parts.push(formatCost(usage.cost));
  const count = Number(usage?.count) || 1;
  const turn = count > 1 ? ` (turn of ${count} messages)` : '';
  return `Usage${parts.length ? `: ${parts.join(' · ')}` : ''}${usage?.partial ? ' (partial)' : ''}${turn}`;
}

// The rows of a room's or project's totals, as [label, value]. The figures are sums over turns, each turn once.
export function totalsRows(totals) {
  return [
    ['Turns', String(Number(totals?.turns) || 0)],
    ['Input', formatTokens(totals?.tokens_in)],
    ['Output', formatTokens(totals?.tokens_out)],
    ['Cache read', formatTokens(totals?.tokens_cache_read)],
    ['Cache written', formatTokens(totals?.tokens_cache_write)],
    ['Cost (estimate)', formatCost(totals?.cost)],
  ];
}

// Said under the totals: how much of the cost is known, and when part of the spend was not seen.
export function totalsNotes(totals) {
  const notes = [];
  const turns = Number(totals?.turns) || 0, priced = Number(totals?.turns_with_cost) || 0;
  if (!turns) notes.push('Nothing was spent in this range.');
  else if (totals?.cost_coverage === 'partial') notes.push(`The cost is known for ${priced} of ${turns} turns; the harness of the others does not report one, so the total is a floor.`);
  else if (totals?.cost_coverage === 'unknown') notes.push('No turn in this range reported a cost.');
  if (totals?.partial) notes.push('Some turns ended before they reported their totals, so those figures are a floor.');
  return notes;
}

// One line of a breakdown by agent or model.
export function groupLine(group) {
  const parts = [`${Number(group?.turns) || 0} ${Number(group?.turns) === 1 ? 'turn' : 'turns'}`];
  if (Number.isFinite(group?.tokens_in) || Number.isFinite(group?.tokens_out)) parts.push(`${formatTokens(group?.tokens_in)} in / ${formatTokens(group?.tokens_out)} out`);
  parts.push(formatCost(group?.cost));
  return parts.join(' \u00b7 ');
}

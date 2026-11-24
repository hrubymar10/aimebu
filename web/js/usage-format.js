export function percent(value) {
  const number = Number(value);
  return Number.isFinite(number) ? Math.max(0, Math.min(100, number)) : 0;
}

export function windowLabel(key) {
  if (String(key || '').startsWith('weekly_scoped:')) return `Weekly (${key.slice('weekly_scoped:'.length).replaceAll('-', ' ')})`;
  return ({ session: 'Session', weekly: 'Weekly', monthly: 'Monthly', monthly_spend: 'Monthly spend', weekly_opus: 'Weekly (Opus)', weekly_sonnet: 'Weekly (Sonnet)', codex_spark: 'Codex Spark', codex_spark_weekly: 'Codex Spark Weekly' })[key] || key.replaceAll('_', ' ');
}

// A monthly window spans the calendar month it resets in (28 to 31 days; some providers report
// a 30-day sentinel). Other durations are not monthly.
export function isMonthlyWindow(window) {
  const days = Number(window.window_duration_seconds) / 86400;
  return Number.isFinite(days) && days >= 28 && days <= 31;
}

// A monthly window's daily target: the remaining share of the allowance divided by the local
// calendar days left before the reset, counting today; a reset exactly at local midnight starts
// the next allowance without adding a day. Unknown usage, a missing or expired reset, and
// non-monthly windows show no target. The usage API carries percentages only, so the target is
// a share per day, not an amount in the allowance's own unit.
export function dailyTarget(window, now = new Date()) {
  if (!isMonthlyWindow(window)) return null;
  const used = Number(window.percent_used);
  const reset = new Date(window.reset_at);
  if (!Number.isFinite(used) || Number.isNaN(reset.getTime()) || reset <= now) return null;
  const startOfDay = value => { const day = new Date(value); day.setHours(0, 0, 0, 0); return day; };
  const resetDay = startOfDay(reset);
  const days = Math.round((resetDay - startOfDay(now)) / 86400000);
  const dayCount = Math.max(1, days + (reset > resetDay ? 1 : 0));
  return (100 - Math.max(0, Math.min(100, used))) / dayCount;
}

export function dailyTargetText(window, now = new Date()) {
  const target = dailyTarget(window, now);
  if (target == null) return '';
  return `≈ ${Number(target.toFixed(2))}% / day to last until reset`;
}

export function creditsText(credits) {
  const balance = Number(credits.balance), limit = Number(credits.spend_limit);
  let text = Number.isFinite(balance) ? balance.toFixed(2) : '-';
  if (Number.isFinite(limit) && limit > 0) text += ` / ${limit.toFixed(2)}`;
  return `${credits.label || 'Credits'}: ${text}`;
}

// v1's relative age: seconds, minutes, hours, days.
export function age(value) {
  const ms = Date.parse(value);
  if (!Number.isFinite(ms)) return 'just now';
  const sec = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (sec < 60) return `${sec}s`;
  const mins = Math.floor(sec / 60);
  if (mins < 60) return `${mins}m`;
  const hours = Math.floor(mins / 60);
  return hours < 24 ? `${hours}h` : `${Math.floor(hours / 24)}d`;
}
export function countdown(value) {
  const ms = Date.parse(value);
  if (!Number.isFinite(ms)) return '-';
  const sec = Math.max(0, Math.round((ms - Date.now()) / 1000)), days = Math.floor(sec / 86400), hours = Math.floor((sec % 86400) / 3600), mins = Math.floor((sec % 3600) / 60);
  return days > 0 ? `${days}d ${hours}h` : hours > 0 ? `${hours}h ${mins}m` : `${mins}m`;
}
export function shown(value) { const rounded = Math.max(0, Math.min(100, value)); return `${rounded.toFixed(rounded % 1 ? 1 : 0)}%`; }

// A stale profile's reset, said as a fact about its last update instead of a countdown that looks live: the time it was due, or "Reset passed".
export function staleResetText(value, now = Date.now()) {
  const ms = Date.parse(value);
  if (!Number.isFinite(ms)) return '';
  if (ms <= now) return 'Reset passed';
  return `Resets ${new Date(ms).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' })}`;
}

const SPARK_SPAN_MS = 48 * 3600000;

// The geometry of a window's recent used-percentage samples, or null when there are fewer than two. The x axis is the last 48
// hours, so a gap in the data reads as a gap; the y axis follows the percent display (used or left). A window whose samples
// stopped because its login was rejected (`rejected_at`, unix seconds, the episode's first rejection) also carries a marker
// at that time, so the gap says why it is there; an ordinary gap (the server being down) carries nothing. The marker is only
// drawn when it falls on a visible gap: after the series' first sample, so there is a sample on its left.
export function sparkGeometry(window, usedDisplay, now = Date.now()) {
  const points = (window.history || []).filter(point => Number.isFinite(Number(point.t)) && Number.isFinite(Number(point.p)) && point.t * 1000 >= now - SPARK_SPAN_MS);
  if (points.length < 2) return null;
  const width = 100, height = 20, pad = 2;
  const shown = point => usedDisplay ? percent(point.p) : 100 - percent(point.p);
  const first = shown(points[0]), last = shown(points[points.length - 1]);
  const coords = points.map(point => {
    const x = Math.max(0, Math.min(width, ((point.t * 1000 - (now - SPARK_SPAN_MS)) / SPARK_SPAN_MS) * width));
    const y = height - pad - (shown(point) / 100) * (height - 2 * pad);
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  });
  const rejected = Number(window.rejected_at);
  let marker = null;
  if (Number.isFinite(rejected) && rejected * 1000 >= now - SPARK_SPAN_MS && rejected >= points[0].t) {
    const x = Math.max(0, Math.min(width, ((rejected * 1000 - (now - SPARK_SPAN_MS)) / SPARK_SPAN_MS) * width));
    marker = { x: x.toFixed(1), label: `Login rejected ${new Date(rejected * 1000).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' })}` };
  }
  return { viewBox: `0 0 ${width} ${height}`, label: `${windowLabel(window.key)} over the last 48 hours: ${Math.round(first)}% to ${Math.round(last)}% ${usedDisplay ? 'used' : 'left'}`, points: coords.join(' '), end: coords[coords.length - 1].split(','), marker };
}

// What the pace line means, in a sentence: pace compares the share used with the share an even spend would have used by
// now. expected_percent is the even-spend share of the window that has passed.
export function paceExplanation(pace, usedDisplay) {
  const expected = Number(pace?.expected_percent);
  const evenShare = Number.isFinite(expected) ? ` An even spend would have used about ${Math.round(expected)}% by now${usedDisplay ? '' : ` and left ${Math.round(100 - expected)}%`}.` : '';
  const how = 'Pace compares what you have used with an even spend across the window.';
  if (pace?.state === 'reserve') return `${how}${evenShare} In reserve: you are behind that pace, so the limit should last until it resets.`;
  if (pace?.state === 'deficit') return `${how}${evenShare} In deficit: you are ahead of that pace, so at this rate the limit runs out before it resets.`;
  return `${how}${evenShare} On pace: you are using it about as fast as an even spend.`;
}

// What the marks on a usage card mean, said once under the cards (the Usages page and the popover): hovering a mark shows the same words, but a tooltip
// is not something a phone or a keyboard finds.
export const USAGE_LEGEND = 'On each bar the small tick is the expected pace for the time that has passed in the window. "In reserve" means you are using less than that pace, "in deficit" more. The thin line is the last 48 hours of use; a short dashed tick on it marks where the samples stopped because a login was rejected.';

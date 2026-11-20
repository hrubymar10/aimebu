import { html } from '../../ui.js';
import { percent, windowLabel, paceExplanation, sparkGeometry, countdown, shown, staleResetText, dailyTargetText } from '../../usage-format.js';

// One limit window, laid out as v1's row (the same classes and rules as the popover): the label, the bar with the expected-pace tick, the 48-hour
// sparkline (v2 only), "N% left" with the reset countdown, then the pace line.
// A stale profile (data older than twice the refresh interval) keeps its numbers but shows no pace claim and no live countdown.
export function UsageWindow({ window, usedDisplay, fetchedAt, stale = false }) {
  const used = percent(window.percent_used), value = usedDisplay ? used : 100 - used;
  const label = windowLabel(window.key), side = usedDisplay ? 'used' : 'left';
  const pace = stale ? null : window.pace, expected = Number(pace?.expected_percent);
  const state = pace?.state === 'reserve' ? 'reserve' : pace?.state === 'deficit' ? 'deficit' : 'on-track';
  const spark = sparkGeometry(window, usedDisplay);
  let paceLabel = '', eta = '';
  if (pace) {
    const delta = Math.abs(Number(pace.delta_percent) || 0);
    paceLabel = pace.state === 'reserve' ? `${shown(delta)} in reserve` : pace.state === 'deficit' ? `${shown(delta)} in deficit` : '';
    if (pace.lasts_to_reset) eta = 'Lasts until reset';
    else if (pace.eta_seconds != null) eta = `Runs out in ${countdown(new Date((Number.isFinite(Date.parse(fetchedAt)) ? Date.parse(fetchedAt) : Date.now()) + pace.eta_seconds * 1000).toISOString())}`;
  }
  const explanation = pace && paceExplanation(pace, usedDisplay), paceText = [paceLabel, eta].filter(Boolean).join(' · ');
  // The daily target follows the pace: it appears with the same data the pace line needs, and a
  // stale profile claims no pace and no target.
  const target = pace ? dailyTargetText(window) : '';
  return html`<div class="usage-window usages-window-row">
    <div class="usages-window-top"><span>${label}</span></div>
    <div class="usage-track usages-progress" role="progressbar" aria-label=${`${label} ${side}`} aria-valuenow=${String(Math.round(value))} aria-valuemin="0" aria-valuemax="100">
      <span class="usage-fill" style=${{ width: `${value}%` }}></span>
      ${pace && Number.isFinite(expected) && html`<i class=${`usage-pace-marker usages-pace-tick usages-pace-tick--${state}`} style=${{ left: `${usedDisplay ? percent(expected) : 100 - percent(expected)}%` }} title="Expected pace"></i>`}
    </div>
    ${spark && html`<div class="usage-spark-figure">
      <svg class="usage-spark" viewBox=${spark.viewBox} preserveAspectRatio="none" role="img" aria-label=${spark.label}>
        <polyline class="usage-spark-line" points=${spark.points} fill="none" vector-effect="non-scaling-stroke" />
        <circle class="usage-spark-dot" cx=${spark.end[0]} cy=${spark.end[1]} r="1.6" />
      </svg>
      <div class="usage-spark-caption"><span>48 h ago</span><span>${`${usedDisplay ? '% used' : '% left'}, last 48 h`}</span><span>now</span></div>
    </div>`}
    <div class="usages-window-meta usage-window-foot"><strong>${`${shown(value)} ${side}`}</strong><span>${window.reset_at ? (stale ? staleResetText(window.reset_at) : `Resets in ${countdown(window.reset_at)}`) : ''}</span></div>
    ${paceText && html`<div class=${`usages-pace-text usages-pace-text--${state}`} title=${explanation} tabindex="0" aria-label=${`${paceText}. ${explanation}`}>
      ${paceLabel && html`<span>${paceLabel}</span>`}${paceLabel && eta && html`<span class="usages-pace-sep">·</span>`}${eta && html`<span>${eta}</span>`}</div>`}
    ${target && html`<div class="usages-daily-target">${target}</div>`}
  </div>`;
}

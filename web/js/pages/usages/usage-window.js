import { html } from '../../ui.js';
import { percent, windowLabel, resetLabel, paceExplanation, sparkGeometry } from '../../usage-format.js';

// One limit window: its label and reset, the bar with the expected-pace marker, the 48-hour sparkline and the pace line.
export function UsageWindow({ window, usedDisplay }) {
  const used = percent(window.percent_used), value = usedDisplay ? used : 100 - used;
  const label = windowLabel(window.key), side = usedDisplay ? 'used' : 'left';
  const pace = window.pace, expected = Number(pace?.expected_percent);
  const delta = Math.abs(Number(pace?.delta_percent) || 0);
  const paceText = pace && (pace.state === 'reserve' ? `${Math.round(delta)}% in reserve` : pace.state === 'deficit' ? `${Math.round(delta)}% in deficit` : 'On pace');
  const explanation = pace && paceExplanation(pace, usedDisplay);
  const spark = sparkGeometry(window, usedDisplay);
  return html`<div class="usage-window">
    <div class="usage-window-top"><strong>${label}</strong><span>${window.reset_at ? resetLabel(window.reset_at) : ''}</span></div>
    <div class="usage-track" role="progressbar" aria-label=${`${label} ${side}`} aria-valuenow=${String(Math.round(value))} aria-valuemin="0" aria-valuemax="100">
      <span class="usage-fill" style=${{ width: `${value}%` }}></span>
      ${pace && Number.isFinite(expected) && html`<i class=${`usage-pace-marker usage-pace-${pace.state || 'on_track'}`} style=${{ left: `${usedDisplay ? percent(expected) : 100 - percent(expected)}%` }} title="Expected pace"></i>`}
    </div>
    ${spark && html`<div class="usage-spark-figure">
      <svg class="usage-spark" viewBox=${spark.viewBox} preserveAspectRatio="none" role="img" aria-label=${spark.label}>
        <polyline class="usage-spark-line" points=${spark.points} fill="none" vector-effect="non-scaling-stroke" />
        <circle class="usage-spark-dot" cx=${spark.end[0]} cy=${spark.end[1]} r="1.6" />
      </svg>
      <div class="usage-spark-caption"><span>48 h ago</span><span>${`${usedDisplay ? '% used' : '% left'}, last 48 h`}</span><span>now</span></div>
    </div>`}
    <div class="usage-window-foot">
      <strong>${`${Math.round(value)}% ${side}`}</strong>
      ${pace && html`<span class=${`usage-pace-text usage-pace-${pace.state || 'on_track'}`} title=${explanation} tabindex="0" aria-label=${`${paceText}. ${explanation}`}>${paceText}</span>`}
    </div>
  </div>`;
}

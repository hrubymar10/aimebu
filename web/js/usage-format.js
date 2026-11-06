export function element(tag, className, value = '') {
  const node = document.createElement(tag);
  node.className = className;
  node.textContent = value;
  return node;
}

export function percent(value) {
  const number = Number(value);
  return Number.isFinite(number) ? Math.max(0, Math.min(100, number)) : 0;
}

export function windowLabel(key) {
  if (String(key || '').startsWith('weekly_scoped:')) return `Weekly (${key.slice('weekly_scoped:'.length).replaceAll('-', ' ')})`;
  return ({ session: 'Session', weekly: 'Weekly', monthly: 'Monthly', weekly_opus: 'Weekly (Opus)', weekly_sonnet: 'Weekly (Sonnet)', codex_spark: 'Codex Spark', codex_spark_weekly: 'Codex Spark Weekly' })[key] || key.replaceAll('_', ' ');
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

export function resetLabel(value) {
  const remaining = new Date(value).getTime() - Date.now();
  if (!Number.isFinite(remaining)) return '';
  if (remaining <= 0) return 'Reset due';
  const minutes = Math.ceil(remaining / 60000);
  if (minutes < 60) return `Resets in ${minutes}m`;
  const hours = Math.floor(minutes / 60);
  return hours < 48 ? `Resets in ${hours}h ${minutes % 60}m` : `Resets in ${Math.floor(hours / 24)}d ${hours % 24}h`;
}

const SPARK_SPAN_MS = 48 * 3600000, SVG_NS = 'http://www.w3.org/2000/svg';

// The geometry of a window's recent used-percentage samples, or null when there are fewer than two. The x axis is the last 48
// hours, so a gap in the data reads as a gap; the y axis follows the percent display (used or left).
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
  return { viewBox: `0 0 ${width} ${height}`, label: `${windowLabel(window.key)} over the last 48 hours: ${Math.round(first)}% to ${Math.round(last)}% ${usedDisplay ? 'used' : 'left'}`, points: coords.join(' '), end: coords[coords.length - 1].split(',') };
}

// usageSparkline draws that geometry as a tiny inline SVG (built with DOM calls only), or returns null when there is none.
export function usageSparkline(window, usedDisplay, now = Date.now()) {
  const geometry = sparkGeometry(window, usedDisplay, now);
  if (!geometry) return null;
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('class', 'usage-spark');
  svg.setAttribute('viewBox', geometry.viewBox);
  svg.setAttribute('preserveAspectRatio', 'none');
  svg.setAttribute('role', 'img');
  svg.setAttribute('aria-label', geometry.label);
  const line = document.createElementNS(SVG_NS, 'polyline');
  line.setAttribute('class', 'usage-spark-line');
  line.setAttribute('points', geometry.points);
  line.setAttribute('fill', 'none');
  line.setAttribute('vector-effect', 'non-scaling-stroke');
  svg.append(line);
  const dot = document.createElementNS(SVG_NS, 'circle');
  dot.setAttribute('class', 'usage-spark-dot');
  dot.setAttribute('cx', geometry.end[0]); dot.setAttribute('cy', geometry.end[1]); dot.setAttribute('r', '1.6');
  svg.append(dot);
  return svg;
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

// The sparkline with its axes named: the period it covers under it, and what the line is (the share used or left).
export function sparkFigure(spark, usedDisplay) {
  const figure = element('div', 'usage-spark-figure');
  const caption = element('div', 'usage-spark-caption');
  caption.append(element('span', '', '48 h ago'), element('span', '', `${usedDisplay ? '% used' : '% left'}, last 48 h`), element('span', '', 'now'));
  figure.append(spark, caption);
  return figure;
}

export function usageWindowRow(window, usedDisplay) {
    const row = element('div', 'usage-window');
    const used = percent(window.percent_used), value = usedDisplay ? used : 100 - used;
    const top = element('div', 'usage-window-top');
    top.append(element('strong', '', windowLabel(window.key)), element('span', '', window.reset_at ? resetLabel(window.reset_at) : ''));
    const track = element('div', 'usage-track');
    track.setAttribute('role', 'progressbar');
    track.setAttribute('aria-label', `${windowLabel(window.key)} ${usedDisplay ? 'used' : 'left'}`);
    track.setAttribute('aria-valuenow', String(Math.round(value)));
    track.setAttribute('aria-valuemin', '0'); track.setAttribute('aria-valuemax', '100');
    const fill = element('span', 'usage-fill'); fill.style.width = `${value}%`; track.append(fill);
    if (window.pace && Number.isFinite(Number(window.pace.expected_percent))) {
      const marker = element('i', `usage-pace-marker usage-pace-${window.pace.state || 'on_track'}`);
      marker.style.left = `${usedDisplay ? percent(window.pace.expected_percent) : 100 - percent(window.pace.expected_percent)}%`;
      marker.title = 'Expected pace'; track.append(marker);
    }
    const foot = element('div', 'usage-window-foot');
    foot.append(element('strong', '', `${Math.round(value)}% ${usedDisplay ? 'used' : 'left'}`));
    if (window.pace) {
      const delta = Math.abs(Number(window.pace.delta_percent) || 0);
      const state = window.pace.state === 'reserve' ? `${Math.round(delta)}% in reserve` : window.pace.state === 'deficit' ? `${Math.round(delta)}% in deficit` : 'On pace';
      const pace = element('span', `usage-pace-text usage-pace-${window.pace.state || 'on_track'}`, state);
      const explanation = paceExplanation(window.pace, usedDisplay);
      pace.title = explanation; pace.tabIndex = 0; pace.setAttribute('aria-label', `${state}. ${explanation}`);
      foot.append(pace);
    }
    const spark = usageSparkline(window, usedDisplay);
    row.append(top, track, ...(spark ? [sparkFigure(spark, usedDisplay)] : []), foot);
    return row;
}

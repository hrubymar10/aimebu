// Import-free: scripts/test-usage-alerts.cjs runs this module in a vm.

// The server says one sentence per tool when another token refresher owns its live login ("claude: <reason>; this tool will not refresh"), which comes out as
// the same long sentence once per tool. This turns the alerts into what a person reads: one alert for all the tools whose refresh is paused, in plain words
// with what to do (usually nothing), and the reasons and paths as details. Every other alert is kept as it is.
const REFRESHER = /^([a-z0-9_-]+): (.*); this tool will not refresh$/i;

export function mergeAlerts(alerts, mask) {
  const out = [], tools = [], details = [];
  const say = text => (typeof mask === 'function' ? mask(String(text || '')) : String(text || ''));
  for (const alert of Array.isArray(alerts) ? alerts : []) {
    const match = alert?.kind === 'refresher_conflict' ? REFRESHER.exec(String(alert.message || '')) : null;
    if (!match) { out.push({ kind: String(alert?.kind || ''), text: say(alert?.message), details: [] }); continue; }
    if (!tools.includes(match[1])) tools.push(match[1]);
    details.push(say(`${match[1]}: ${match[2]}`));
  }
  if (tools.length) {
    const these = tools.length > 1 ? 'these logins' : 'this login';
    out.unshift({ kind: 'refresher_conflict', text: `Token refresh is paused for ${tools.join(', ')}: another aimebu (v1) or token refresher manages ${these}. Nothing to do, unless you want this aimebu to refresh ${these}: then stop the other one.`, details });
  }
  return out;
}

// How old a snapshot is allowed to be before it is shown as stale: twice the refresh interval, and never under five minutes.
export function staleAfterSeconds(refreshIntervalSec) { return Math.max(300, 2 * (Number(refreshIntervalSec) || 120)); }

// The age of a profile's data when it is stale ("2d", "3h", "12 min"), else ''.
export function staleAge(lastRefreshAt, refreshIntervalSec, now = Date.now()) {
  const at = Date.parse(lastRefreshAt || '');
  if (!Number.isFinite(at)) return '';
  const seconds = Math.floor((now - at) / 1000);
  if (seconds <= staleAfterSeconds(refreshIntervalSec)) return '';
  if (seconds >= 172800) return `${Math.floor(seconds / 86400)}d`;
  if (seconds >= 3600) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.max(1, Math.floor(seconds / 60))} min`;
}

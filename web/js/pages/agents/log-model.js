// The agent log popup's pure rules: the lines asked for, ANSI codes taken out, the highlight's matches and when the next read may run.
// Import-free, so scripts/test-agent-log-model.cjs runs it in a vm.

export const LINES_MIN = 50;
export const LINES_MAX = 2000;
export const LINES_DEFAULT = 200;
// The highlight marks at most this many matches; past it the counter says "2000+ matches".
export const MARK_CAP = 2000;
// While the person types, the highlight runs again at most this often.
export const HIGHLIGHT_MS = 150;
// Follow reads at most once in this long (15 a minute: half the server's 30 a person), and every this long while the agent is in a turn
// with no event to wake it.
export const FOLLOW_GAP_MS = 4000;
export const FOLLOW_FALLBACK_MS = 15000;
// A 429 without a usable Retry-After waits this long, which is the window the server counts in.
export const RETRY_DEFAULT_MS = 60000;

// What the lines field holds as a number the server takes: whole, between LINES_MIN and LINES_MAX; anything that is not a number is the default.
export function clampLines(value) {
  const number = Math.floor(Number(String(value ?? '').trim()));
  if (!Number.isFinite(number) || String(value ?? '').trim() === '') return LINES_DEFAULT;
  return Math.min(LINES_MAX, Math.max(LINES_MIN, number));
}

// CSI sequences (colours, cursor moves), OSC sequences (titles, links; ended by BEL or ESC \) and the two-byte escapes. Codex writes colour codes into stderr.
const ANSI = /\u001b\[[0-?]*[ -/]*[@-~]|\u001b\][^\u0007\u001b]*(?:\u0007|\u001b\\)|\u001b[@-Z\\-_]|\u009b[0-?]*[ -/]*[@-~]/g;
export function stripAnsi(text) { return String(text ?? '').replace(ANSI, ''); }

// The last `lines` lines of a text (a trailing newline does not count as a line of its own), for a read that asked for more than is shown.
export function lastLines(text, lines) {
  const value = String(text ?? '');
  const body = value.endsWith('\n') ? value.slice(0, -1) : value;
  const parts = body.split('\n');
  if (parts.length <= lines) return value;
  return `${parts.slice(-lines).join('\n')}${value.endsWith('\n') ? '\n' : ''}`;
}

export function escapeRegExp(text) { return String(text).replace(/[.*+?^${}()|[\]\\/-]/g, '\\$&'); }

// The text cut into plain parts and matches of `query`: literal and case-insensitive (the query is never a regular expression), at most `cap`
// marked matches. `count` is how many were marked and `capped` says there are more. The caller builds text nodes and <mark> elements from the parts.
export function highlightParts(text, query, cap = MARK_CAP) {
  const value = String(text ?? ''), needle = String(query ?? '');
  if (!needle || !value) return { parts: value ? [{ text: value, mark: false }] : [], count: 0, capped: false };
  const pattern = new RegExp(escapeRegExp(needle), 'gi');
  const parts = [];
  let at = 0, count = 0, capped = false, match;
  while ((match = pattern.exec(value))) {
    if (count >= cap) { capped = true; break; }
    if (match.index > at) parts.push({ text: value.slice(at, match.index), mark: false });
    parts.push({ text: match[0], mark: true });
    at = match.index + match[0].length; count++;
  }
  if (at < value.length) parts.push({ text: value.slice(at), mark: false });
  return { parts, count, capped };
}

// The counter beside the highlight field.
export function matchLabel({ count, capped }, query, current = -1) {
  if (!query) return '';
  if (!count) return 'No matches';
  if (capped) return `${count}+ matches`;
  return current >= 0 ? `${current + 1} of ${count}` : `${count} ${count === 1 ? 'match' : 'matches'}`;
}

// The match Enter (step 1) or Shift+Enter (step -1) moves to, wrapping at both ends; from no match, Enter goes to the first and Shift+Enter to the last.
export function stepMatch(current, count, step) {
  if (!count) return -1;
  if (current < 0 || current >= count) return step < 0 ? count - 1 : 0;
  return (current + step + count) % count;
}

// When a follow read may start: not before `gap` after the last one began, and not before a 429's wait is over.
export function nextReadAt({ now, lastReadAt = 0, blockedUntil = 0, gap = FOLLOW_GAP_MS }) {
  return Math.max(now, lastReadAt ? lastReadAt + gap : 0, blockedUntil);
}

// How long a 429 asks to wait: Retry-After in seconds (the server sends 60), or an HTTP date; a missing or broken value waits a full window.
export function retryAfterMs(value, now = Date.now()) {
  const raw = String(value ?? '').trim();
  if (/^\d+$/.test(raw)) return Number(raw) * 1000;
  const date = Date.parse(raw);
  if (raw && Number.isFinite(date)) return Math.max(0, date - now);
  return RETRY_DEFAULT_MS;
}

// Whether an event from the stream should make a following popup read again: an agent's activity, a turn or an answer of the agent it shows.
export function followEvent(event, agentID) {
  if (!event || !agentID || event.agent_id !== agentID) return false;
  const kind = String(event.kind || '');
  return kind === 'supervisor_activity' || kind.startsWith('turn_') || kind.startsWith('answer_');
}

// Whether a view scrolled to (nearly) its end, so a follow read keeps it there; a person who scrolled up is left where they are.
export function atBottom({ scrollTop, scrollHeight, clientHeight }, slack = 8) { return scrollHeight - scrollTop - clientHeight <= slack; }

// What the popup says under its text for an answer, and for a refusal (the server's own words where it has them).
export function logStatus(view) {
  if (!view?.text) return 'No output has been captured for this agent yet.';
  return view.truncated ? 'Earlier output is not shown.' : '';
}

export function logFailure(error, now = Date.now(), blockedUntil = 0) {
  if (error?.status === 403) return 'Only the project owner or an admin can read an agent\'s logs.';
  if (error?.code === 'no_output') return 'No output has been captured for this agent yet.';
  if (error?.status === 429) {
    const seconds = Math.max(1, Math.ceil((blockedUntil - now) / 1000));
    return `Logs were read too often; trying again in ${seconds}s.`;
  }
  return error?.message || 'The logs could not be read.';
}

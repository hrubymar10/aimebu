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
  const withheld = Number(view.withheld) || 0;
  return [view.truncated ? 'Earlier output is not shown.' : '',
    withheld ? `${withheld} ${withheld === 1 ? 'turn is' : 'turns are'} withheld: a private chat you are not in, or a turn the server cannot place in a room.` : ''].filter(Boolean).join(' ');
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

// The two logs an agent has. The popup opens on the conversation (what the agent was asked, said and ran); stderr is the harness's own output.
export const STREAMS = [{ id: 'conversation', label: 'Conversation' }, { id: 'stderr', label: 'Stderr' }];
export const STREAM_DEFAULT = 'conversation';
export function streamOf(value) { return STREAMS.some(stream => stream.id === value) ? value : STREAM_DEFAULT; }

// What stands in front of a conversation record for its kind. `withheld` is the server's line for a turn the reader may not see (a private chat
// they are not in, or a turn it cannot place in a room), `event` an event of the harness the adapter did not know, and `cut` the end of a record
// too long for one read (the log's "... earlier part cut" line).
export const KIND_LABELS = { turn_start: 'Turn', prompt: 'Prompt', assistant: 'Assistant', thinking: 'Thinking', tool_call: 'Tool call', tool_result: 'Tool result',
  error: 'Error', event: 'Event', withheld: 'Withheld', turn_end: 'Turn end', cut: 'Cut' };
export const CUT_MARKER = '... earlier part cut';
const RECORD_HEAD = /^\d{4}-\d\d-\d\dT(\d\d:\d\d:\d\d)Z ([a-z_]+)(?: (.*))?$/;

// A conversation log as records. A record is a line "<UTC time> <kind> <text>" and the lines after it that start with a tab, which are the rest
// of its text (the tab is not part of it). A line that is neither (an older format, a cut the server made) is kept as text of the record before
// it, or as a record of its own with no kind, so nothing the server sent is dropped. A kind this page does not know keeps its own name as its label.
export function conversationRecords(text) {
  const value = String(text ?? '');
  if (!value) return [];
  const records = [];
  let open = null;
  for (const line of (value.endsWith('\n') ? value.slice(0, -1) : value).split('\n')) {
    if (line.startsWith('\t') && open) { open.text += `\n${line.slice(1)}`; continue; }
    const head = RECORD_HEAD.exec(line);
    if (head) open = { kind: head[2], label: KIND_LABELS[head[2]] || head[2].replace(/_/g, ' '), clock: head[1], text: head[3] || '' };
    else if (line === CUT_MARKER) open = { kind: 'cut', label: KIND_LABELS.cut, clock: '', text: 'the beginning of this record is not shown' };
    else if (open && !open.kind) { open.text += `\n${line}`; continue; }
    else open = { kind: '', label: '', clock: '', text: line.startsWith('\t') ? line.slice(1) : line };
    records.push(open);
  }
  return records;
}

// The last `lines` lines of a conversation, begun at a record: a cut that falls inside a record goes back to that record's first line, as the
// server's own tail does, so a record never loses the line that says what it is.
export function conversationTail(text, lines) {
  const value = String(text ?? '');
  const body = value.endsWith('\n') ? value.slice(0, -1) : value;
  const parts = body.split('\n');
  if (parts.length <= lines) return value;
  let start = parts.length - lines;
  while (start > 0 && parts[start].startsWith('\t')) start--;
  return `${parts.slice(start).join('\n')}${value.endsWith('\n') ? '\n' : ''}`;
}

// The blocks the popup draws: one block of plain text for stderr, one per record for a conversation.
export function logBlocks(text, stream) {
  const value = String(text ?? '');
  if (stream !== 'conversation') return value ? [{ kind: '', label: '', clock: '', text: value, plain: true }] : [];
  return conversationRecords(value);
}

// highlightParts over every block's text, with one count and one cap for all of them, so the counter and Enter step through the whole log.
export function highlightBlocks(blocks, query, cap = MARK_CAP) {
  let count = 0, capped = false;
  const out = blocks.map(block => {
    const found = highlightParts(block.text, capped ? '' : query, cap - count);
    count += found.count; capped = capped || found.capped;
    return { ...block, parts: found.parts };
  });
  return { blocks: out, count, capped };
}

// What the popup says above its text about the stream it shows.
export function streamNote(stream) {
  return stream === 'conversation'
    ? 'What this agent was asked, what it said and the tools it ran, kept on its machine for 14 days. Known secret shapes are masked and long tool output is cut, but it can still contain secrets.'
    : 'The end of what this agent\'s harness printed to stderr, kept on its machine for 14 days. It can contain secrets from tool output.';
}

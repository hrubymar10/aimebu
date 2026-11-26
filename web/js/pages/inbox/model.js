// Import-free: scripts/test-inbox-model.cjs runs this module in a vm.

// The server cuts a long draft off with an ellipsis, which leaves invalid JSON; the text of its "body" fields is still there.
function salvageBodies(text) {
  const bodies = [];
  for (const match of text.matchAll(/"body"\s*:\s*"((?:[^"\\]|\\.)*)/g)) {
    let body = match[1];
    try { body = JSON.parse(`"${body}"`); } catch { body = body.replace(/\\n/g, '\n').replace(/\\"/g, '"').replace(/\\\\/g, '\\'); }
    if (body.trim()) bodies.push(body.trim());
  }
  return bodies;
}

// Nothing the human reads in the Inbox may be raw JSON. A string that is JSON is turned into a short plain summary
// (the message text when it has one, else a few "label: value" lines); the raw record is behind the debug inspector.
export function humanize(value) {
  if (typeof value !== 'string') return value == null ? '' : String(value);
  const text = value.trim();
  if (!/^[{[]/.test(text)) return value;
  let data;
  try { data = JSON.parse(text); } catch {
    const salvaged = salvageBodies(text);
    return salvaged.length ? salvaged.join('\n\n') : 'The details are incomplete. Open the debug inspector for the raw record.';
  }
  const messages = Array.isArray(data) ? data : data?.messages || [data];
  const bodies = messages.map(entry => entry?.body || entry?.text || entry?.content || entry?.message).filter(entry => typeof entry === 'string' && entry.trim());
  if (bodies.length) return bodies.join('\n\n');
  const lines = [];
  const visit = (node, label, depth) => {
    if (depth > 3 || lines.length >= 4 || node == null) return;
    if (Array.isArray(node)) { for (const entry of node) visit(entry, label, depth + 1); return; }
    if (typeof node === 'object') { for (const [key, entry] of Object.entries(node)) visit(entry, key.replaceAll('_', ' '), depth + 1); return; }
    lines.push(`${label || 'Detail'}: ${typeof node === 'boolean' ? (node ? 'yes' : 'no') : node}`);
  };
  visit(data, '', 0);
  return lines.join(' · ') || 'No readable details. Open the debug inspector for the raw record.';
}

// A card's heading is one short line; an agent's question is often a whole message (paragraphs, a list, code). The heading is its
// first line, or that line's first sentence when the line is long; whatever follows is the body, which the card renders as Markdown.
// A long first line with no sentence break is cut at a word and the body repeats the whole text, so nothing is ever dropped.
const TITLE_MAX = 120;
export function questionParts(value) {
  const text = humanize(value).replace(/\r\n?/g, '\n').trim();
  const plain = line => line.replace(/^#{1,6}\s+/, '').replace(/\*\*|__/g, '').trim();
  const newline = text.indexOf('\n');
  const first = newline < 0 ? text : text.slice(0, newline).trim(), rest = newline < 0 ? '' : text.slice(newline + 1).trim();
  if (first.length <= TITLE_MAX) return { title: plain(first), body: rest };
  const sentence = first.slice(0, TITLE_MAX + 1).match(/^(.{20,}?[.!?:])(?=\s)/);
  if (sentence) return { title: plain(sentence[1]), body: [first.slice(sentence[1].length).trim(), rest].filter(Boolean).join('\n') };
  const cut = first.lastIndexOf(' ', TITLE_MAX);
  return { title: `${plain(first.slice(0, cut > 40 ? cut : TITLE_MAX))}…`, body: text };
}

export function readablePreview(raw) {
  if (!raw) return '';
  const value = raw.trim();
  if (!/^[{[]/.test(value)) return value;
  try {
    const data = JSON.parse(value);
    const messages = Array.isArray(data) ? data : data.messages || [data];
    const bodies = messages.map(message => message?.body || message?.text || message?.content).filter(text => typeof text === 'string' && text.trim());
    return bodies.length ? bodies.join('\n\n') : 'The reply draft contains no readable text. View the room for context.';
  } catch {
    const salvaged = salvageBodies(value);
    return salvaged.length ? salvaged.join('\n\n') : 'The reply draft is incomplete. View the room for context.';
  }
}

export function readableReport(report) {
  if (typeof report === 'string') {
    try { report = JSON.parse(report); } catch { return 'The diagnostic details could not be displayed.'; }
  }
  const lines = [];
  function visit(value, label, depth = 0) {
    if (depth > 3 || lines.length >= 30) return;
    if (Array.isArray(value)) { for (const entry of value) visit(entry, label, depth + 1); return; }
    if (value && typeof value === 'object') { for (const [key, entry] of Object.entries(value)) visit(entry, key.replaceAll('_', ' '), depth + 1); return; }
    if (value == null) return;
    lines.push(`${label || 'Detail'}: ${typeof value === 'boolean' ? value ? 'Yes' : 'No' : value}`);
  }
  visit(report, 'Detail');
  return lines.join('\n') || 'No diagnostic details available.';
}

// Why a reply was held, in a sentence. A reason we know needs no technical error after it (that is in the raw record).
export function deliveryEvidence(item) {
  const known = {
    room_closed: 'The room was archived before this reply could be posted.',
    correction_unavailable: 'The agent could not repair its reply, which did not pass validation.',
    invalid_after_correction: 'The revised reply still did not pass validation.',
  };
  const error = item.delivery_error && /^[{[]/.test(item.delivery_error.trim())
    ? 'The reply did not pass validation.' : item.delivery_error;
  // A validation failure carries the actual reason, so the person can see what to fix; a closed room needs none.
  if (item.held_reason === 'correction_unavailable' || item.held_reason === 'invalid_after_correction') {
    return [known[item.held_reason], error && error !== 'The reply did not pass validation.' ? `Reason: ${error}` : ''].filter(Boolean).join(' ');
  }
  if (known[item.held_reason]) return known[item.held_reason];
  const reason = item.held_reason?.replaceAll('_', ' ');
  return [reason, error].filter(Boolean).join(' ') || 'The reply was held; view the room for context.';
}

export const KIND_LABELS = { approval: 'Approval', question: 'Question', recovery: 'Recovery', delivery_failed: 'Delivery failed', usage_info: 'Usage notice', usage_limit: 'Usage limit', usage_alert: 'Usage alert', pool_change: 'Pool change' };

const ARCHIVED = ['closed', 'closing'];

export const resolvedItems = items => (items || []).filter(item => item.kind === 'delivery_failed' && item.can_decide && ARCHIVED.includes(item.room_lifecycle));

export const bulkLabel = count => `Dismiss all resolved (${count})`;

export function countView(count) {
  return { text: String(count), hidden: !count, name: count ? `Inbox, ${count} waiting` : 'Inbox', title: count ? `(${count}) aimebu` : 'aimebu' };
}

export function groupItems(items) {
  const groups = new Map();
  for (const item of items) { const key = `${item.project_id}/${item.room_id}`; if (!groups.has(key)) groups.set(key, []); groups.get(key).push(item); }
  return [...groups].map(([key, list]) => ({ key, items: list }));
}

export const groupTitle = (item, projects, rooms) => (item.kind === 'usage_alert' ? 'Usages' : `${projects.get(item.project_id)?.name || 'Project'} / ${rooms.get(item.room_id)?.title || 'Room'}`);

// A card is drawn again, not updated in place, when what it shows changes under the same id.
export function cardKey(item) {
  if (item.kind === 'usage_alert') return `${item.id}|${item.question}`;
  if (item.kind === 'recovery' || item.kind === 'delivery_failed') return `${item.id}|${item.rev}|${item.room_lifecycle || ''}`;
  return item.id;
}

// A pool change's list: each affected agent counted once under its own room, "<room>: leader, 2 workers"; never an agent id.
export function poolGroups(agents) {
  const rooms = new Map();
  for (const agent of agents || []) {
    const key = agent.room_id || '';
    if (!rooms.has(key)) rooms.set(key, { room: agent.room_title || 'Room', roles: new Map() });
    const roles = rooms.get(key).roles, label = (agent.role_label || agent.role_key || 'agent').toLowerCase();
    roles.set(label, (roles.get(label) || 0) + 1);
  }
  return [...rooms.values()].map(({ room, roles }) => ({ room, text: [...roles].map(([label, count]) => (count === 1 ? label : `${count} ${label}s`)).join(', ') }));
}

export const POOL_CHOICES = [['Respawn on the pool model', 'respawn', 'refresh', 'btn btn-primary'], ['Keep the current model', 'keep', 'check', 'btn']];

export function decisionOptions(item) {
  if (item.kind === 'usage_limit') return (item.options || []).map(label => [label, label.startsWith('Continue:') ? 'continue' : 'abandon']);
  if (item.recovery_phase === 'quiescence_wait') return [['Override', 'override'], ['Abandon', 'abandon']];
  if (item.recovery_phase === 'broken_session') return [['Restart', 'restart'], ['Abandon', 'abandon']];
  if (item.recovery_phase === 'answer_held') return [['Retry', 'retry'], ['Abandon', 'abandon']];
  // A turn that failed before it did anything has nothing to resume: Retry is the one way on.
  if (item.turn_id && item.definite_failure) return [['Retry', 'retry'], ['Abandon', 'abandon']];
  return item.turn_id ? [['Resume', 'resume'], ['Retry', 'retry'], ['Abandon', 'abandon']] : [['Continue', 'continue'], ['Abandon', 'abandon']];
}

export const retryWarning = item => (item.recovery_phase === 'quiescence_wait' ? 'The old process may still be running. Override can overlap it and repeat an action.'
  : item.turn_id && item.start_failure ? 'The harness did not run. Retry is safe once the start problem is fixed.'
  : item.turn_id && item.definite_failure ? 'Retry is safe: the turn failed before it did anything. It fails the same way until the cause is fixed.'
  : item.turn_id ? 'Resume may repeat an action if the diagnostic missed an effect. Retry repeats the task.' : '');

// What the harness said went wrong, as a sentence, with the raw output kept apart: a recovery card shows `text` and folds `raw` behind a
// details toggle. Null when the harness gave no sentence of its own.
export function recoveryResult(item) {
  if (item.kind !== 'recovery' || !item.result_text) return null;
  return { text: humanize(item.result_text), raw: humanize(item.evidence || '') };
}

// One line of what went wrong, for the top of a recovery card, taken from the raw text a harness left when it gave no sentence of its own: the exit
// code the process ended with, and the harness's own words from the JSON result inside the output when there are some; else the first line of the
// text, cut short. The whole text goes behind the card's Details.
export function evidenceSummary(text) {
  const clean = String(text || '').trim();
  if (!clean) return '';
  const exit = /^Process exited with code (-?\d+)/i.exec(clean);
  let cause = '';
  const result = /"result"\s*:\s*"((?:[^"\\]|\\.)*)"/.exec(clean);
  if (result) { try { cause = JSON.parse(`"${result[1]}"`); } catch { cause = result[1]; } cause = cause.replace(/\s+/g, ' ').trim(); }
  const cut = (value, length) => (value.length > length ? `${value.slice(0, length - 1).trimEnd()}…` : value);
  if (exit) return cause ? `The agent's process exited with code ${exit[1]}: ${cut(cause, 160)}` : `The agent's process exited with code ${exit[1]}.`;
  if (cause) return cut(cause, 200);
  return cut(clean.split('\n').find(line => line.trim()).trim().replace(/\s+/g, ' '), 200);
}

// What a recovery card says: `summary` is the line shown first (the harness's sentence when it gave one, else a line made from its output, else what
// is known about the turn), and `details` are the labelled texts folded behind the card's Details (the raw output when it says more than the summary,
// and the diagnostic report), so the decision is on screen without scrolling past a wall of output.
export function recoveryView(item) {
  const result = recoveryResult(item);
  const evidence = humanize(item.evidence || '');
  const details = [];
  let summary;
  if (result) {
    summary = result.text;
    if (result.raw) details.push({ label: 'Raw output', text: result.raw });
  } else {
    summary = evidenceSummary(evidence) || evidenceText(item);
    if (evidence && evidence.trim() !== summary) details.push({ label: 'Output', text: evidence });
  }
  if (item.diagnostic_report) details.push({ label: 'Diagnostic report', text: readableReport(item.diagnostic_report) });
  return { summary, details };
}

export function evidenceText(item) {
  const result = recoveryResult(item);
  if (result) return result.text;
  return humanize((item.kind === 'delivery_failed' && deliveryEvidence(item)) || item.evidence || (item.kind === 'delivery_failed' ? 'The reply was held; view the room for context.'
    : item.diagnostic_report ? 'A diagnostic report is available below.' : item.turn_exec === 'lost' ? 'The previous process may still be running; no diagnostic was run.' : 'No diagnostic report is available yet.'));
}

export const boundaryText = item => humanize(`Requested by ${item.boundary_requester} · Machine ${item.boundary_machine} · Folder ${item.boundary_path}`);

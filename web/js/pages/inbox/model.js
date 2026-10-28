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
  if (known[item.held_reason]) return known[item.held_reason];
  const reason = item.held_reason?.replaceAll('_', ' ');
  const error = item.delivery_error && /^[{[]/.test(item.delivery_error.trim())
    ? 'The reply did not pass validation.' : item.delivery_error;
  return [reason, error].filter(Boolean).join(' ') || 'The reply was held; view the room for context.';
}

export const KIND_LABELS = { approval: 'Approval', question: 'Question', recovery: 'Recovery', delivery_failed: 'Delivery failed', usage_info: 'Usage notice', usage_limit: 'Usage limit', usage_alert: 'Usage alert' };

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

export function decisionOptions(item) {
  if (item.kind === 'usage_limit') return (item.options || []).map(label => [label, label.startsWith('Continue:') ? 'continue' : 'abandon']);
  if (item.recovery_phase === 'quiescence_wait') return [['Override', 'override'], ['Abandon', 'abandon']];
  return item.turn_id ? [['Resume', 'resume'], ['Retry', 'retry'], ['Abandon', 'abandon']] : [['Continue', 'continue'], ['Abandon', 'abandon']];
}

export const retryWarning = item => (item.recovery_phase === 'quiescence_wait' ? 'The old process may still be running. Override can overlap it and repeat an action.'
  : item.turn_id ? 'Resume may repeat an action if the diagnostic missed an effect. Retry repeats the task.' : '');

export function evidenceText(item) {
  return humanize((item.kind === 'delivery_failed' && deliveryEvidence(item)) || item.evidence || (item.kind === 'delivery_failed' ? 'The reply was held; view the room for context.'
    : item.diagnostic_report ? 'A diagnostic report is available below.' : item.turn_exec === 'lost' ? 'The previous process may still be running; no diagnostic was run.' : 'No diagnostic report is available yet.'));
}

export const boundaryText = item => humanize(`Requested by ${item.boundary_requester} · Machine ${item.boundary_machine} · Folder ${item.boundary_path}`);

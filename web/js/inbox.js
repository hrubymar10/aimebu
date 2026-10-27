import { setGlyph } from './icon-button.js';
import { uuid } from './uuid.js';
import { answerControls } from './answer-controls.js';
import { rights } from './api.js';

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

function readablePreview(raw) {
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

function readableReport(report) {
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

export class Inbox {
  constructor(source, projects, rooms, onJump, notifications, onItems) {
    this.source = source; this.projects = projects; this.rooms = rooms; this.onJump = onJump; this.notifications = notifications; this.onItems = onItems;
    this.view = document.querySelector('#inbox-view'); this.list = document.querySelector('#inbox-list'); this.badge = document.querySelector('#inbox-count'); this.error = document.querySelector('#inbox-error');
    this.nodes = new Map(); this.groups = new Map(); this.keys = new Map();
    // "Dismiss all resolved": delivery failures in archived rooms need nothing more from anyone.
    this.bulk = document.createElement('button'); this.bulk.type = 'button'; this.bulk.className = 'icon-btn inbox-dismiss-resolved'; this.bulk.hidden = true;
    this.bulk.onclick = () => this.dismissResolved();
    this.view.querySelector('header').append(this.bulk);
    this.source.subscribe(() => this.refresh());
  }
  async refresh({ notify = true } = {}) {
    const ticket = (this.requestSeq || 0) + 1; this.requestSeq = ticket;
    const items = await this.source.list(); if (ticket !== this.requestSeq) return;
    const previous = this.items && new Set(this.items.map(item => item.id));
    this.items = items; this.badge.textContent = String(items.length); this.badge.hidden = !items.length; this.labelToggle(items.length);
    this.syncBulk();
    document.title = items.length ? `(${items.length}) aimebu` : 'aimebu';
    this.render();
    this.onItems?.(items);
    if (notify && previous) this.notifications?.arrived(items.filter(item => !previous.has(item.id)), this.projects, this.rooms);
  }
  // The count is a corner pill, so the accessible name and the tooltip carry it for everyone else.
  labelToggle(count) {
    const toggle = document.querySelector('#inbox-toggle'); if (!toggle) return;
    const name = count ? `Inbox, ${count} waiting` : 'Inbox'; toggle.title = name; toggle.setAttribute('aria-label', name);
  }
  reset() {
    this.requestSeq = (this.requestSeq || 0) + 1;
    this.items = undefined; this.nodes.clear(); this.groups.clear(); this.keys.clear(); this.list.replaceChildren();
    this.activeRoom = undefined;
    this.badge.textContent = '0'; this.badge.hidden = true; this.labelToggle(0); this.error.textContent = '';
    this.hide(); this.onItems?.([]);
  }
  show() {
    // On desktop the room list stays beside the Inbox (the view replaces only the chat column), so any room is one click away;
    // on a phone the list is a drawer and stays closed.
    this.view.hidden = false; document.querySelector('.chat').hidden = true; document.querySelector('.sidebar').hidden = matchMedia('(max-width: 700px)').matches; document.querySelector('#app-view').classList.add('inbox-open');
  }
  hide() { this.view.hidden = true; document.querySelector('.chat').hidden = false; document.querySelector('.sidebar').hidden = false; document.querySelector('#app-view').classList.remove('inbox-open'); }
  setRoom(room) { this.activeRoom = room?.id; this.render(); }
  setError(message) { this.error.textContent = message; }
  render() {
    if (!this.items) return;
    const active = new Set(this.items.map(item => item.id));
    for (const [id, node] of this.nodes) if (!active.has(id)) { node.remove(); this.nodes.delete(id); this.keys.delete(id); }
    if (!this.items.length) {
      for (const node of this.groups.values()) node.remove(); this.groups.clear();
      if (!this.list.querySelector('.inbox-empty')) { const empty = document.createElement('p'); empty.className = 'inbox-empty'; empty.textContent = 'All caught up'; this.list.append(empty); }
      return;
    }
    this.list.querySelector('.inbox-empty')?.remove();
    const groups = new Map();
    for (const item of this.items) { const key = `${item.project_id}/${item.room_id}`; if (!groups.has(key)) groups.set(key, []); groups.get(key).push(item); }
    for (const [key, items] of groups) {
      let group = this.groups.get(key);
      if (!group) { group = document.createElement('section'); group.className = 'inbox-group'; group.append(document.createElement('h2')); this.groups.set(key, group); this.list.append(group); }
      const first = items[0]; group.querySelector('h2').textContent = first.kind === 'usage_alert' ? 'Usages' : `${this.projects.get(first.project_id)?.name || 'Project'} / ${this.rooms.get(first.room_id)?.title || 'Room'}`;
      for (const item of items) {
        let card = this.nodes.get(item.id);
        if (!card || (item.kind === 'usage_alert' && card.dataset.question !== item.question) || ((item.kind === 'recovery' || item.kind === 'delivery_failed') && (card.dataset.rev !== String(item.rev) || card.dataset.roomLifecycle !== (item.room_lifecycle || '')))) {
          const next = this.card(item);
          if (card) card.replaceWith(next);
          card = next; this.nodes.set(item.id, card);
        }
        if (card.parentElement !== group) group.append(card);
      }
    }
    for (const [key, node] of this.groups) if (!groups.has(key)) { node.remove(); this.groups.delete(key); }
  }
  resolvedItems() { return (this.items || []).filter(item => item.kind === 'delivery_failed' && item.can_decide && ['closed', 'closing'].includes(item.room_lifecycle)); }
  syncBulk() {
    const count = this.resolvedItems().length;
    this.bulk.hidden = count === 0;
    const label = `Dismiss all resolved (${count})`;
    setGlyph(this.bulk, 'archive', label); this.bulk.title = `${label}: the delivery failures of archived rooms`;
    const total = document.createElement('span'); total.className = 'inbox-dismiss-count'; total.textContent = String(count); this.bulk.append(total);
  }
  async dismissResolved() {
    const items = this.resolvedItems();
    if (!items.length || this.bulk.disabled) return;
    this.setError(''); this.bulk.disabled = true;
    try { for (const item of items) await this.source.dismiss(item.id, item.rev); }
    catch (error) { this.setError(error.message); }
    finally { this.bulk.disabled = false; await this.refresh({ notify: false }); }
  }
  card(item) {
    const card = document.createElement('article'); card.className = 'inbox-card'; card.dataset.inboxId = item.id;
    card.dataset.rev = String(item.rev);
    card.dataset.question = item.question || '';
    card.dataset.roomLifecycle = item.room_lifecycle || '';
    const kind = document.createElement('small'); kind.textContent = ({ approval: 'Approval', question: 'Question', recovery: 'Recovery', delivery_failed: 'Delivery failed', usage_info: 'Usage notice', usage_limit: 'Usage limit', usage_alert: 'Usage alert' })[item.kind] || item.kind; if (item.kind === 'delivery_failed') kind.className = 'inbox-kind-error'; card.append(kind);
    if ((item.kind === 'recovery' || item.kind === 'delivery_failed') && ['closed', 'closing'].includes(item.room_lifecycle)) {
      const archived = document.createElement('small'); archived.className = 'inbox-archived'; archived.textContent = item.room_lifecycle === 'closing' ? 'Room is being archived' : 'In archived room'; card.append(archived);
    }
    card.append(this.rawToggle(item));
    const question = document.createElement('h3'); question.textContent = humanize(item.question); card.append(question);
    if (item.kind === 'usage_alert') {
      if (rights.owner && this.openUsages) {
        const open = document.createElement('button'); open.type = 'button'; open.className = 'btn btn-primary'; open.textContent = 'Open Usages';
        open.onclick = () => this.openUsages(); card.append(open);
      }
      const dismiss = document.createElement('button'); dismiss.type = 'button'; dismiss.className = 'icon-btn'; setGlyph(dismiss, 'close', 'Dismiss usage alert');
      dismiss.onclick = () => this.dismiss(item); card.append(dismiss);
      return card;
    }
    if (item.boundary_pending) {
      const detail = document.createElement('p'); detail.className = 'inbox-evidence';
      detail.textContent = humanize(`Requested by ${item.boundary_requester} · Machine ${item.boundary_machine} · Folder ${item.boundary_path}`);
      card.append(detail);
      if (item.can_decide) {
        const decisions = document.createElement('div'); decisions.className = 'inbox-decisions';
        for (const [label, choice] of [['Approve', 'approve'], ['Deny', 'deny']]) {
          const button = document.createElement('button'); button.type = 'button'; button.className = 'btn'; button.textContent = label;
          button.onclick = () => this.decide(item, choice); decisions.append(button);
        }
        card.append(decisions);
      } else {
        const notice = document.createElement('p'); notice.className = 'inbox-evidence'; notice.textContent = 'The project owner must decide this request.'; card.append(notice);
      }
      return card;
    }
    if (this.rooms.has(item.room_id)) { const jump = document.createElement('button'); jump.type = 'button'; jump.className = 'icon-btn inbox-jump'; setGlyph(jump, 'external-link', 'View in room'); jump.onclick = () => this.onJump(item); card.append(jump); }
    if (item.kind === 'recovery' || item.kind === 'delivery_failed') {
      const evidence = document.createElement('p'); evidence.className = 'inbox-evidence';
      evidence.textContent = humanize((item.kind === 'delivery_failed' && deliveryEvidence(item)) || item.evidence || (item.kind === 'delivery_failed' ? 'The reply was held; view the room for context.'
        : item.diagnostic_report ? 'A diagnostic report is available below.'
        : item.turn_exec === 'lost' ? 'The previous process may still be running; no diagnostic was run.'
        : 'No diagnostic report is available yet.')); card.append(evidence);
      if (item.kind === 'delivery_failed' && item.candidate_preview) {
        const preview = document.createElement('p'); preview.className = 'inbox-evidence inbox-preview';
        preview.textContent = readablePreview(item.candidate_preview); card.append(preview);
      }
      if (item.kind === 'recovery' && item.diagnostic_report) {
        const details = document.createElement('details'); details.className = 'inbox-diagnostic';
        const summary = document.createElement('summary'); summary.textContent = 'Diagnostic report';
        const report = document.createElement('pre');
        report.textContent = readableReport(item.diagnostic_report);
        details.append(summary, report); card.append(details);
      }
    }
    if (item.kind === 'recovery' || item.kind === 'usage_limit') {
      if (item.kind === 'recovery' && item.can_decide === false) {
        const notice = document.createElement('p'); notice.className = 'inbox-evidence';
        notice.textContent = 'A project owner or operator must decide this recovery item.';
        card.append(notice);
        return card;
      }
      const decisions = document.createElement('div'); decisions.className = 'inbox-decisions';
      const options = item.kind === 'usage_limit'
        ? (item.options || []).map(label => [label, label.startsWith('Continue:') ? 'continue' : 'abandon'])
        : item.recovery_phase === 'quiescence_wait'
        ? [['Override', 'override'], ['Abandon', 'abandon']]
        : item.turn_id ? [['Resume', 'resume'], ['Retry', 'retry'], ['Abandon', 'abandon']]
          : [['Continue', 'continue'], ['Abandon', 'abandon']];
      for (const [label, choice] of options) {
        const button = document.createElement('button'); button.type = 'button'; button.className = 'btn'; button.textContent = label; if (choice === 'abandon') button.className = 'btn btn-danger';
        button.onclick = () => this.decide(item, choice); decisions.append(button);
      }
      card.append(decisions);
      if (item.recovery_phase === 'quiescence_wait' || item.turn_id) {
        const warning = document.createElement('small'); warning.className = 'inbox-retry-warning';
        warning.textContent = item.recovery_phase === 'quiescence_wait'
          ? 'The old process may still be running. Override can overlap it and repeat an action.'
          : 'Resume may repeat an action if the diagnostic missed an effect. Retry repeats the task.';
        card.append(warning);
      }
      return card;
    }
    if (item.kind === 'usage_info' && (item.options || []).includes('Retry now')) {
      const retry = document.createElement('button'); retry.type = 'button'; retry.className = 'btn btn-primary'; retry.textContent = 'Retry now';
      retry.onclick = () => this.decide(item, 'retry'); card.append(retry);
    }
    if (item.kind === 'delivery_failed') {
      if (item.can_decide) {
        const dismiss = document.createElement('button'); dismiss.type = 'button'; dismiss.className = 'icon-btn'; setGlyph(dismiss, 'close', 'Dismiss');
        dismiss.onclick = () => this.dismiss(item); card.append(dismiss);
      }
      return card;
    }
    if (item.kind === 'usage_info') return card;
    if (item.can_decide === false) { card.append(document.createTextNode('Read only')); return card; }
    if (item.proposed_answers?.length || item.open_questions?.length) {
      card.append(answerControls(item, value => this.answer(item, value), { freeAnswer: true }));
      return card;
    }
    const choices = document.createElement('div'); choices.className = 'inbox-choices';
    for (const option of item.options || []) { const button = document.createElement('button'); button.type = 'button'; button.className = 'btn'; button.textContent = humanize(option); button.onclick = () => this.answer(item, option); choices.append(button); }
    card.append(choices);
    if (item.free_text) {
      const form = document.createElement('form'); form.className = 'inbox-answer';
      const input = document.createElement('textarea'); input.name = 'answer'; input.rows = 2; input.placeholder = 'Your answer'; input.required = true; input.setAttribute('aria-label', 'Your answer');
      const submit = document.createElement('button'); submit.className = 'btn btn-primary'; submit.textContent = 'Send answer'; form.append(input, submit);
      form.onsubmit = event => { event.preventDefault(); this.answer(item, input.value.trim()); }; card.append(form);
    }
    return card;
  }
  // The raw record of an item, behind the same switch as the message debug inspector (Settings > General).
  rawToggle(item) {
    const holder = document.createElement('div'); holder.className = 'inbox-raw-holder';
    const button = document.createElement('button'); button.type = 'button'; button.className = 'icon-btn quiet-glyph chat-msg-debug-toggle inbox-raw-toggle';
    button.title = 'Open debug inspector'; button.setAttribute('aria-label', 'Show the raw record of this item'); button.setAttribute('aria-expanded', 'false');
    const glyph = document.createElement('span'); glyph.className = 'icon-mask icon-mask-bug'; glyph.setAttribute('aria-hidden', 'true'); button.append(glyph);
    const raw = document.createElement('pre'); raw.className = 'inbox-raw'; raw.hidden = true; raw.textContent = JSON.stringify(item, null, 2);
    button.onclick = () => { raw.hidden = !raw.hidden; button.setAttribute('aria-expanded', String(!raw.hidden)); };
    holder.append(button, raw);
    return holder;
  }
  async answer(item, value) {
    if (!value) return;
    this.setError('');
    const card = this.nodes.get(item.id);
    if (card?.dataset.pending === 'true') return;
    if (card) { card.dataset.pending = 'true'; for (const control of card.querySelectorAll('button,textarea')) control.disabled = true; }
    let attempt = this.keys.get(item.id);
    if (!attempt || attempt.value !== value) { attempt = { value, key: uuid() }; this.keys.set(item.id, attempt); }
    try { await this.source.answer(item.id, value, attempt.key, item.rev); this.keys.delete(item.id); await this.refresh(); return true; }
    catch (error) { this.setError(error.message); if (card) { delete card.dataset.pending; for (const control of card.querySelectorAll('button,textarea')) control.disabled = false; } return false; }
  }
  async decide(item, choice) {
    this.setError('');
    const card = this.nodes.get(item.id);
    if (card?.dataset.pending === 'true') return;
    if (card) { card.dataset.pending = 'true'; for (const button of card.querySelectorAll('button')) button.disabled = true; }
    let attempt = this.keys.get(item.id);
    if (!attempt || attempt.value !== choice) { attempt = { value: choice, key: uuid() }; this.keys.set(item.id, attempt); }
    try { await this.source.decision(item.id, choice, attempt.key, item.rev); this.keys.delete(item.id); await this.refresh(); }
    catch (error) {
      this.setError(error.code === 'stale_rev' ? 'This item changed, reload item.' : error.message);
      if (card) { delete card.dataset.pending; for (const button of card.querySelectorAll('button')) button.disabled = false; }
      if (error.code === 'stale_rev') { this.keys.delete(item.id); await this.refresh(); }
    }
  }
  async dismiss(item) {
    this.setError('');
    const card = this.nodes.get(item.id);
    if (card?.dataset.pending === 'true') return;
    if (card) { card.dataset.pending = 'true'; for (const button of card.querySelectorAll('button')) button.disabled = true; }
    try { await this.source.dismiss(item.id, item.rev); await this.refresh(); }
    catch (error) {
      this.setError(error.message);
      if (card) { delete card.dataset.pending; for (const button of card.querySelectorAll('button')) button.disabled = false; }
    }
  }
}

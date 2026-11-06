import { request } from './api.js';
import { setGlyph } from './icon-button.js';

function node(tag, className = '', text = '') { const item = document.createElement(tag); if (className) item.className = className; if (text) item.textContent = text; return item; }

function pretty(value) { return JSON.stringify(value, null, 2); }

function section(title, ...children) {
  const wrap = node('section', 'message-debug-section'); wrap.append(node('h3', '', title), ...children); return wrap;
}

function rows(fields) {
  const grid = node('dl', 'message-debug-fields');
  for (const [key, value] of fields) {
    grid.append(node('dt', '', key));
    const item = node('dd');
    if (value !== null && typeof value === 'object') item.append(node('code', '', JSON.stringify(value)));
    else item.append(node('code', '', value === null || value === undefined || value === '' ? '—' : String(value)));
    grid.append(item);
  }
  return grid;
}

// A read-only inspector for one message: its stored record, who it addressed,
// the wake note each agent got (or why none was created), the turns and
// receipts that carried it, and the related events. Everything is rendered
// with textContent, since message bodies and agent output are untrusted.
export class MessageDebug {
  constructor() {
    this.dialog = node('dialog', 'message-debug-dialog'); this.dialog.id = 'message-debug-dialog'; this.dialog.setAttribute('aria-labelledby', 'message-debug-title');
    const head = node('header'); this.title = node('h2', '', 'Message'); this.title.id = 'message-debug-title';
    this.copy = node('button', 'btn', 'Copy JSON'); this.copy.type = 'button';
    const close = node('button', 'btn-icon'); close.type = 'button'; setGlyph(close, 'close', 'Close message inspector'); close.id = 'message-debug-close';
    // v1's inspector walks the room with previous and next; the message actions the bubble leaves out in style A live here too.
    this.prev = node('button', 'row-btn message-debug-nav'); this.prev.type = 'button'; setGlyph(this.prev, 'left', 'Previous message');
    this.next = node('button', 'row-btn message-debug-nav'); this.next.type = 'button'; setGlyph(this.next, 'right', 'Next message');
    this.position = node('span', 'message-debug-position'); this.position.setAttribute('aria-live', 'polite');
    this.threadButton = node('button', 'btn message-debug-action', 'Reply in thread'); this.threadButton.type = 'button';
    this.copyText = node('button', 'btn message-debug-action', 'Copy text'); this.copyText.type = 'button';
    this.copyLink = node('button', 'btn message-debug-action', 'Copy link'); this.copyLink.type = 'button';
    head.append(this.title, this.position, this.prev, this.next, this.threadButton, this.copyText, this.copyLink, this.copy, close);
    this.prev.onclick = () => this.step(-1); this.next.onclick = () => this.step(1);
    this.threadButton.onclick = () => { if (this.current) { this.dialog.close(); this.context?.thread?.(this.current); } };
    for (const [button, label, text] of [[this.copyText, 'Copy text', () => this.current?.body || ''], [this.copyLink, 'Copy link', () => this.current ? `#${this.current.no}` : '']]) {
      button.onclick = async () => { try { await this.context?.copy?.(text()); button.textContent = 'Copied'; } catch { button.textContent = 'Copy failed'; } setTimeout(() => { button.textContent = label; }, 1500); };
    }
    this.status = node('p', 'message-debug-status'); this.status.setAttribute('role', 'status');
    this.body = node('div', 'message-debug-body');
    this.dialog.append(head, this.status, this.body);
    document.body.append(this.dialog);
    close.onclick = () => this.dialog.close();
    this.dialog.addEventListener('click', event => { if (event.target === this.dialog) this.dialog.close(); });
    this.copy.onclick = async () => { try { await navigator.clipboard.writeText(this.json || ''); this.copy.textContent = 'Copied'; } catch { this.copy.textContent = 'Copy failed'; } setTimeout(() => { this.copy.textContent = 'Copy JSON'; }, 1500); };
    this.ticket = 0;
  }

  reset() { this.ticket++; this.current = null; if (this.dialog.open) this.dialog.close(); }

  // What the inspector may ask of the chat: the room's messages (previous/next), opening a thread, copying text.
  setContext(context) { this.context = context; }

  // Moves to the neighbouring message of the room, as v1's previous and next do.
  step(direction) {
    const list = this.context?.messages?.() || [], index = list.findIndex(item => item.id === this.current?.id), next = list[index + direction];
    if (next) this.open(next);
  }

  syncNavigation() {
    const list = this.context?.messages?.() || [], index = list.findIndex(item => item.id === this.current?.id);
    this.position.textContent = index >= 0 ? `${index + 1} / ${list.length}` : '';
    this.prev.disabled = index <= 0; this.next.disabled = index < 0 || index >= list.length - 1;
    this.threadButton.hidden = !this.context?.thread || !!this.current?.thread_root;
  }

  async open(message) {
    const ticket = ++this.ticket;
    this.current = message; this.syncNavigation();
    this.title.textContent = `Message #${message.no ?? ''}`.trim();
    this.status.textContent = 'Loading…'; this.body.replaceChildren(); this.json = '';
    if (!this.dialog.open) this.dialog.showModal();
    try {
      const debug = await request(`/api/messages/${encodeURIComponent(message.id)}/debug`);
      if (ticket !== this.ticket) return;
      this.status.textContent = ''; this.json = pretty(debug); this.render(debug);
    } catch (error) {
      if (ticket === this.ticket) this.status.textContent = error.message || 'Could not load the message record';
    }
  }

  // Why each @token did or did not reach someone. The trace re-reads the body
  // against the room's current members, so it can differ from what was
  // recorded if people joined or left since; the drift note says so.
  addressingSection(trace) {
    const wrap = node('div', 'message-debug-trace');
    if (!trace) return section('Addressing trace', node('p', 'message-debug-empty', 'No trace available.'));
    const mentions = trace.mentions || [];
    if (!mentions.length) wrap.append(node('p', 'message-debug-empty', 'The message has no @mentions.'));
    for (const entry of mentions) {
      const row = node('div', `message-debug-mention outcome-${entry.outcome}`); row.dataset.token = entry.token; row.dataset.outcome = entry.outcome;
      const head = node('div', 'message-debug-mention-head');
      head.append(node('code', '', `@${entry.token}`), node('span', 'message-debug-outcome', entry.outcome.replace('_', ' ')), node('span', 'message-debug-kind', entry.kind.replace('_', ' ')));
      row.append(head, node('p', 'message-debug-reason', entry.reason));
      if (entry.would_reach?.length) row.append(node('p', 'message-debug-reached', `Would have reached: ${entry.would_reach.map(person => person.name || person.id).join(', ')}`));
      if (entry.reached?.length) row.append(node('p', 'message-debug-reached', `${entry.outcome === 'ambiguous' ? 'Candidates' : 'Reaches'}: ${entry.reached.map(person => person.name || person.id).join(', ')}`));
      wrap.append(row);
    }
    for (const source of trace.sources || []) wrap.append(node('p', 'message-debug-source', source));
    if (trace.drift) wrap.append(node('p', 'message-debug-skip', 'Heads up: re-reading the body against the room as it is now addresses someone the recorded targets do not include, so membership has changed since this was sent.'));
    return section('Addressing trace', wrap);
  }

  render(debug) {
    const m = debug.message || {};
    const parts = [];
    parts.push(section('Record', rows([['id', m.id], ['number', m.no], ['seq', m.seq], ['room', m.room_id], ['from', `${m.author_kind}: ${m.author_id}`], ['created', m.created_at], ['reply_to', m.reply_to], ['thread_root', m.thread_root], ['needs_attention', String(!!m.needs_attention)], ['ask', m.ask], ['proposed_answers', m.proposed_answers], ['open_questions', m.open_questions]])));
    const addressed = debug.addressed_to || [];
    parts.push(section('Addressed to', addressed.length ? rows(addressed.map((target, index) => [String(index + 1), target])) : node('p', 'message-debug-empty', 'Nobody in particular (a room-wide message).')));
    parts.push(this.addressingSection(debug.addressing));
    const notWoken = (debug.events || []).filter(event => event.kind === 'message_not_woken');
    const delivery = [];
    for (const note of debug.notes || []) {
      const card = node('div', 'message-debug-note'); card.dataset.state = note.state;
      card.append(rows([['agent', note.agent_name ? `${note.agent_name} (${note.agent_id})` : note.agent_id], ['note state', note.state], ['wake reason', note.wake_reason], ['queued', note.created_at]]));
      for (const turn of note.turns || []) {
        card.append(node('h4', '', `Turn ${turn.id}`), rows([['mode', turn.mode], ['attempt', turn.attempt], ['exec', turn.exec], ['outcome', turn.outcome], ['settle', turn.settle]]));
        for (const receipt of turn.receipts || []) card.append(node('pre', 'message-debug-receipt', pretty(receipt)));
      }
      delivery.push(card);
    }
    if (!delivery.length) delivery.push(node('p', 'message-debug-empty', 'No agent was woken by this message.'));
    for (const event of notWoken) {
      let payload = {}; try { payload = JSON.parse(event.payload_json || '{}'); } catch {}
      delivery.push(node('p', 'message-debug-skip', `Not woken: ${event.agent_id || 'agent'} (${payload.reason || 'unknown reason'})`));
    }
    parts.push(section('Wake and delivery', ...delivery));
    const events = (debug.events || []).map(event => node('div', 'message-debug-event', `${event.seq} · ${event.kind}${event.agent_id ? ` · ${event.agent_id}` : ''} · ${event.payload_json}`));
    parts.push(section('Events', ...(events.length ? events : [node('p', 'message-debug-empty', 'No related events.')])));
    const raw = node('details', 'message-debug-raw'); raw.append(node('summary', '', 'Raw JSON'), node('pre', '', this.json));
    parts.push(raw);
    this.body.replaceChildren(...parts);
  }
}

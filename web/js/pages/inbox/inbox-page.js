import { html, useLayoutEffect, useRef, useState } from '../../ui.js';
import { rights } from '../../api.js';
import { IconButton } from '../../components/icon-button.js';
import { answerControls } from '../../answer-controls.js';
import { KIND_LABELS, boundaryText, bulkLabel, cardKey, decisionOptions, evidenceText, groupItems, groupTitle, humanize, readablePreview, readableReport, recoveryResult, resolvedItems, retryWarning } from './model.js';

// answerControls is shared with the chat, so its DOM is built by that function and held in this island; the wrapper takes no space of its own.
function AnswerIsland({ item, store, pending }) {
  const host = useRef(null), latest = useRef(item);
  latest.current = item;
  useLayoutEffect(() => { host.current.replaceChildren(answerControls(item, value => store.answer(latest.current, value), { freeAnswer: true })); }, [item.id]);
  useLayoutEffect(() => { for (const control of host.current.querySelectorAll('button,textarea,input')) control.disabled = pending; }, [pending]);
  return html`<div class="inbox-answer-host" ref=${host}></div>`;
}

function Raw({ item, pending }) {
  const [open, setOpen] = useState(false);
  return html`<div class="inbox-raw-holder"><button type="button" class="btn-icon quiet-glyph chat-msg-debug-toggle inbox-raw-toggle" title="Open debug inspector" aria-label="Show the raw record of this item" aria-expanded=${String(open)} disabled=${pending} onClick=${() => setOpen(!open)}><span class="icon-mask icon-mask-bug" aria-hidden="true"></span></button>
    <pre class="inbox-raw" hidden=${!open}>${JSON.stringify(item, null, 2)}</pre></div>`;
}

function Freeform({ item, store, pending }) {
  return html`<form class="inbox-answer" onSubmit=${event => { event.preventDefault(); store.answer(item, event.currentTarget.elements.answer.value.trim()); }}>
    <textarea name="answer" rows="2" placeholder="Your answer" required aria-label="Your answer" disabled=${pending}></textarea><button class="btn btn-primary" disabled=${pending}>Send answer</button></form>`;
}

function Card({ item, store, rooms, onJump, openUsages }) {
  const pending = store.pending.value.has(item.id), kind = item.kind, closed = ['closed', 'closing'].includes(item.room_lifecycle);
  const button = (label, onClick, classes = 'btn', extra = {}) => html`<button type="button" class=${classes} disabled=${pending} onClick=${onClick} ...${extra}>${label}</button>`;
  const head = html`${kind === 'delivery_failed' ? html`<small class="inbox-kind-error">${KIND_LABELS[kind]}</small>` : html`<small>${KIND_LABELS[kind] || kind}</small>`}
    ${(kind === 'recovery' || kind === 'delivery_failed') && closed && html`<small class="inbox-archived">${item.room_lifecycle === 'closing' ? 'Room is being archived' : 'In archived room'}</small>`}
    <${Raw} item=${item} pending=${pending} /><h3>${humanize(item.question)}</h3>`;
  const wrap = body => html`<article class="inbox-card" data-inbox-id=${item.id} data-rev=${String(item.rev)} data-question=${item.question || ''} data-room-lifecycle=${item.room_lifecycle || ''} data-pending=${pending ? 'true' : undefined}>${head}${body}</article>`;
  if (kind === 'usage_alert') {
    return wrap(html`${rights.owner && openUsages && button('Open Usages', () => openUsages(), 'btn btn-primary')}<${IconButton} name="close" label="Dismiss usage alert" disabled=${pending} onClick=${() => store.dismiss(item)} />`);
  }
  if (item.boundary_pending) {
    return wrap(html`<p class="inbox-evidence">${boundaryText(item)}</p>${item.can_decide ? html`<div class="inbox-decisions">${[['Approve', 'approve'], ['Deny', 'deny']].map(([label, choice]) => button(label, () => store.decide(item, choice)))}</div>` : html`<p class="inbox-evidence">The project owner must decide this request.</p>`}`);
  }
  const jump = rooms.has(item.room_id) && html`<${IconButton} name="external-link" label="View in room" className="inbox-jump" disabled=${pending} onClick=${() => onJump(item)} />`;
  const evidence = (kind === 'recovery' || kind === 'delivery_failed') && html`<p class="inbox-evidence">${evidenceText(item)}</p>
    ${kind === 'delivery_failed' && item.candidate_preview && html`<p class="inbox-evidence inbox-preview">${readablePreview(item.candidate_preview)}</p>`}
    ${recoveryResult(item)?.raw && html`<details class="inbox-diagnostic inbox-raw"><summary>Raw output</summary><pre>${recoveryResult(item).raw}</pre></details>`}
    ${kind === 'recovery' && item.diagnostic_report && html`<details class="inbox-diagnostic"><summary>Diagnostic report</summary><pre>${readableReport(item.diagnostic_report)}</pre></details>`}`;
  if (kind === 'recovery' || kind === 'usage_limit') {
    if (kind === 'recovery' && item.can_decide === false) return wrap(html`${jump}${evidence}<p class="inbox-evidence">A project owner or operator must decide this recovery item.</p>`);
    const warning = retryWarning(item);
    return wrap(html`${jump}${evidence}<div class="inbox-decisions">${decisionOptions(item).map(([label, choice]) => button(label, () => store.decide(item, choice), choice === 'abandon' ? 'btn btn-danger' : 'btn'))}</div>${warning && html`<small class="inbox-retry-warning">${warning}</small>`}`);
  }
  if (kind === 'usage_info') return wrap(html`${jump}${(item.options || []).includes('Retry now') && button('Retry now', () => store.decide(item, 'retry'), 'btn btn-primary')}`);
  if (kind === 'delivery_failed') return wrap(html`${jump}${evidence}${item.can_decide && html`<${IconButton} name="close" label="Dismiss" disabled=${pending} onClick=${() => store.dismiss(item)} />`}`);
  if (item.can_decide === false) return wrap(html`${jump}Read only`);
  if (item.proposed_answers?.length || item.open_questions?.length) return wrap(html`${jump}<${AnswerIsland} item=${item} store=${store} pending=${pending} />`);
  return wrap(html`${jump}<div class="inbox-choices">${(item.options || []).map(option => button(humanize(option), () => store.answer(item, option)))}</div>${item.free_text && html`<${Freeform} item=${item} store=${store} pending=${pending} />`}`);
}

export function InboxPage({ store, onJump }) {
  const openUsages = store.openUsages.value, items = store.items.value, resolved = resolvedItems(items).length;
  return html`<header><div><h1>Inbox</h1><p>Questions, approvals, and agent recovery</p></div>
    <button type="button" class="btn-icon inbox-dismiss-resolved" hidden=${!resolved} disabled=${store.bulkBusy.value} aria-label=${bulkLabel(resolved)} title=${`${bulkLabel(resolved)}: the delivery failures of archived rooms`} onClick=${() => store.dismissResolved()}><span class="icon-mask icon-mask-archive" aria-hidden="true"></span><span class="inbox-dismiss-count">${String(resolved)}</span></button></header>
    <div id="inbox-list" class="inbox-list">${items && (items.length ? groupItems(items).map(group => html`<section class="inbox-group" key=${group.key}><h2>${groupTitle(group.items[0], store.projects, store.rooms)}</h2>
      ${group.items.map(item => html`<${Card} key=${cardKey(item)} item=${item} store=${store} rooms=${store.rooms} onJump=${onJump} openUsages=${openUsages} />`)}</section>`) : html`<p class="inbox-empty">All caught up</p>`)}</div>
    <p id="inbox-error" class="error" role="alert">${store.error.value}</p>`;
}

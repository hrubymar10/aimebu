import { html, useLayoutEffect, useRef, useState } from '../../ui.js';
import { rights } from '../../api.js';
import { IconButton } from '../../components/icon-button.js';
import { AnswerControls } from '../../components/answer-controls.js';
import { renderMarkdown } from '../../markdown.js';
import { WindowRow } from '../usages/usages-view.js';
import { staleAge } from '../../usage-alerts.js';
import { KIND_LABELS, POOL_CHOICES, boundaryText, bulkLabel, cardKey, decisionOptions, evidenceText, groupItems, groupTitle, humanize, poolGroups, questionParts, readablePreview, recoveryView, resolvedItems, retryWarning } from './model.js';

// The answer controls are the chat's component too; the wrapper takes no space of its own, and every control is off while the item is being answered.
function AnswerIsland({ item, store, pending }) {
  const latest = useRef(item);
  latest.current = item;
  return html`<div class="inbox-answer-host"><${AnswerControls} key=${item.id} item=${item} submit=${value => store.answer(latest.current, value)} freeAnswer disabled=${pending} /></div>`;
}

// The rest of a long question, rendered like a chat message's Markdown (lists, code, links) under the one-line heading.
function QuestionBody({ source }) {
  const host = useRef(null);
  useLayoutEffect(() => { host.current.replaceChildren(renderMarkdown(source)); }, [source]);
  return html`<div class="inbox-question-body md-rendered" ref=${host}></div>`;
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

// The alerted window's chart: the Usages panel's own block, from the usage snapshot the page already reads. No snapshot, or none for this
// provider, profile and window, draws nothing.
function UsageChart({ item, store }) {
  const usage = store.usages.value;
  const profile = (usage?.providers || []).find(provider => provider.provider_name === item.provider)?.profiles?.find(entry => entry.profile_name === item.profile);
  const window = profile?.windows?.find(entry => entry.key === item.window);
  if (!window) return null;
  const refreshSec = usage?.settings?.refresh_interval_sec;
  return html`<div class="inbox-usage-chart usages-b"><${WindowRow} window=${window} usedDisplay=${usage?.settings?.percent_display === 'used'} fetchedAt=${profile.last_refresh_at} stale=${!!staleAge(profile.last_refresh_at, refreshSec)} /></div>`;
}

function Card({ item, store, rooms, onJump, openUsages }) {
  const pending = store.pending.value.has(item.id), kind = item.kind, closed = ['closed', 'closing'].includes(item.room_lifecycle), question = questionParts(item.question);
  const button = (label, onClick, classes = 'btn', extra = {}) => html`<button type="button" class=${classes} disabled=${pending} onClick=${onClick} ...${extra}>${label}</button>`;
  const head = html`${kind === 'delivery_failed' ? html`<small class="inbox-kind-error">${KIND_LABELS[kind]}</small>` : html`<small>${KIND_LABELS[kind] || kind}</small>`}
    ${(kind === 'recovery' || kind === 'delivery_failed') && closed && html`<small class="inbox-archived">${item.room_lifecycle === 'closing' ? 'Room is being archived' : 'In archived room'}</small>`}
    <${Raw} item=${item} pending=${pending} /><h3>${question.title}</h3>${question.body && html`<${QuestionBody} source=${question.body} />`}`;
  const wrap = body => html`<article class="inbox-card" data-inbox-id=${item.id} data-rev=${String(item.rev)} data-question=${item.question || ''} data-room-lifecycle=${item.room_lifecycle || ''} data-pending=${pending ? 'true' : undefined}>${head}${body}</article>`;
  if (kind === 'usage_alert') {
    return wrap(html`${item.window && html`<${UsageChart} item=${item} store=${store} />`}${rights.owner && openUsages && button('Open Usages', () => openUsages(item), 'btn btn-primary')}<${IconButton} name="close" label="Dismiss usage alert" disabled=${pending} onClick=${() => store.dismiss(item)} />`);
  }
  // A shared agent note waiting for approval: approving switches it on for every project's agents, declining deletes it.
  if (item.note_id) {
    return wrap(html`${item.can_decide ? html`<div class="inbox-decisions">${[['Approve', 'approve', 'btn btn-primary'], ['Decline', 'deny', 'btn btn-danger']].map(([label, choice, classes]) => button(label, () => store.decide(item, choice), classes))}</div>` : html`<p class="inbox-evidence">An owner or admin must decide this note.</p>`}`);
  }
  if (item.boundary_pending) {
    return wrap(html`<p class="inbox-evidence">${boundaryText(item)}</p>${item.can_decide ? html`<div class="inbox-decisions">${[['Approve', 'approve'], ['Deny', 'deny']].map(([label, choice]) => button(label, () => store.decide(item, choice)))}</div>` : html`<p class="inbox-evidence">The project owner must decide this request.</p>`}`);
  }
  const jump = rooms.has(item.room_id) && html`<${IconButton} name="external-link" label="View in room" className="inbox-jump" disabled=${pending} onClick=${() => onJump(item)} />`;
  const evidence = kind === 'delivery_failed' && html`<p class="inbox-evidence">${evidenceText(item)}</p>
    ${item.candidate_preview && html`<p class="inbox-evidence inbox-preview">${readablePreview(item.candidate_preview)}</p>`}`;
  if (kind === 'recovery') {
    // The summary and the decision come first and fit on a screen; whatever the harness left behind is folded under Details.
    const view = recoveryView(item), warning = item.can_decide === false ? '' : retryWarning(item);
    const details = view.details.length > 0 && html`<details class="inbox-details"><summary>Details</summary>${view.details.map(detail => html`<div class="inbox-detail inbox-diagnostic" key=${detail.label}><small>${detail.label}</small><pre>${detail.text}</pre></div>`)}</details>`;
    const summary = html`<p class="inbox-evidence inbox-summary">${view.summary}</p>`;
    if (item.can_decide === false) return wrap(html`${jump}${summary}<p class="inbox-evidence">A project owner or operator must decide this recovery item.</p>${details}`);
    return wrap(html`${jump}${summary}<div class="inbox-decisions">${decisionOptions(item).map(([label, choice]) => button(label, () => store.decide(item, choice), choice === 'abandon' ? 'btn btn-danger' : 'btn'))}</div>${warning && html`<small class="inbox-retry-warning">${warning}</small>`}${details}`);
  }
  if (kind === 'pool_change') {
    const groups = poolGroups(item.pool_agents);
    return wrap(html`${jump}<div class="inbox-pool-list"><small>Switching will affect:</small><ul>${groups.map(group => html`<li key=${group.room}>${group.room}: ${group.text}</li>`)}</ul>${(item.pool_agents || []).filter(agent => agent.respawn_error).map(agent => html`<p class="inbox-evidence pool-failed" key=${agent.slug}>${agent.slug}: ${agent.respawn_error}</p>`)}</div>
      ${item.can_decide ? html`<div class="inbox-decisions">${POOL_CHOICES.map(([label, choice, icon, classes]) => html`<button type="button" class=${classes} data-choice=${choice} title=${label} disabled=${pending} onClick=${() => store.decide(item, choice)}><span class=${`icon-mask icon-mask-${icon}`} aria-hidden="true"></span> ${label}</button>`)}</div>` : html`<p class="inbox-evidence">The project owner or an admin must decide this.</p>`}`);
  }
  if (kind === 'usage_limit') {
    const warning = retryWarning(item);
    return wrap(html`${jump}<div class="inbox-decisions">${decisionOptions(item).map(([label, choice]) => button(label, () => store.decide(item, choice), choice === 'abandon' ? 'btn btn-danger' : 'btn'))}</div>${warning && html`<small class="inbox-retry-warning">${warning}</small>`}`);
  }
  if (kind === 'usage_info') return wrap(html`${jump}${(item.options || []).includes('Retry now') && button('Retry now', () => store.decide(item, 'retry'), 'btn btn-primary')}`);
  if (kind === 'delivery_failed' && item.recovery_phase === 'answer_held' && item.can_decide) return wrap(html`${jump}${evidence}<div class="inbox-decisions">${decisionOptions(item).map(([label, choice]) => button(label, () => store.decide(item, choice), choice === 'abandon' ? 'btn btn-danger' : 'btn'))}</div>`);
  if (kind === 'delivery_failed') return wrap(html`${jump}${evidence}${item.can_decide && html`<${IconButton} name="close" label="Dismiss" disabled=${pending} onClick=${() => store.dismiss(item)} />`}`);
  if (item.can_decide === false) return wrap(html`${jump}Read only`);
  if (item.proposed_answers?.length || item.open_questions?.length) return wrap(html`${jump}<${AnswerIsland} item=${item} store=${store} pending=${pending} />${button('Dismiss', () => store.dismiss(item), 'btn inbox-dismiss')}`);
  return wrap(html`${jump}<div class="inbox-choices">${(item.options || []).map(option => button(humanize(option), () => store.answer(item, option)))}</div>${item.free_text && html`<${Freeform} item=${item} store=${store} pending=${pending} />`}${(kind === 'question' || kind === 'approval') && button('Dismiss', () => store.dismiss(item), 'btn inbox-dismiss')}`);
}

export function InboxPage({ store, onJump }) {
  const openUsages = store.openUsages.value, items = store.items.value, resolved = resolvedItems(items).length;
  return html`<header><div><h1>Inbox</h1><p>Questions, approvals, and agent recovery</p></div>
    <button type="button" class="btn-icon inbox-dismiss-resolved" hidden=${!resolved} disabled=${store.bulkBusy.value} aria-label=${bulkLabel(resolved)} title=${`${bulkLabel(resolved)}: the delivery failures of archived rooms`} onClick=${() => store.dismissResolved()}><span class="icon-mask icon-mask-archive" aria-hidden="true"></span><span class="inbox-dismiss-count">${String(resolved)}</span></button></header>
    <div id="inbox-list" class="inbox-list">${items && (items.length ? groupItems(items).map(group => html`<section class="inbox-group" key=${group.key}><h2>${groupTitle(group.items[0], store.projects, store.rooms)}</h2>
      ${group.items.map(item => html`<${Card} key=${cardKey(item)} item=${item} store=${store} rooms=${store.rooms} onJump=${onJump} openUsages=${openUsages} />`)}</section>`) : html`<p class="inbox-empty">All caught up</p>`)}</div>
    <p id="inbox-error" class="error" role="alert">${store.error.value}</p>`;
}

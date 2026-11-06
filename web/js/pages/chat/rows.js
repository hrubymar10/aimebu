import { html, Component, Fragment, useCallback, useLayoutEffect, useRef } from '../../ui.js';
import { EXPIRED_HOURGLASS } from '../../usage-expiry.js';
import { fillMessageActions, messageActionsProps, placeholderMessageActions } from '../../message-actions.js';
import { stampTime } from '../../time.js';
import { applySenderIdentity, fillSenderPresence, SENDER_PRESENCE_ROOT } from '../../sender-identity.js';
import { fillVisualPlan, hasVisualPlan, VISUAL_PLAN_ROOT } from '../../visual-plan.js';
import { ANSWER_CONTROLS_ROOT, ANSWER_HISTORY_ROOT, fillAnswerControls, fillAnswerHistory } from '../../answer-controls.js';

export const rowRenders = { count: 0, thread: 0, main: 0 };

// A row re-renders only when its record or one of the keys the page computes for it changes.
export class Row extends Component {
  shouldComponentUpdate(next) {
    const now = this.props;
    return next.message !== now.message || next.view.key !== now.view.key;
  }
  render({ message, view, host, kind, children }) {
    rowRenders.count++; rowRenders[kind]++;
    return children();
  }
}

const usageButton = (view, message) => message.usage && html`<button type="button" class="row-btn quiet-glyph chat-msg-usage-toggle" title=${view.usageTitle} aria-label=${view.usageTitle} aria-haspopup="dialog" aria-expanded="false"><span class="icon-mask icon-mask-chart-column" aria-hidden="true"></span></button>`;

function Presence({ identity }) {
  const root = useRef(null);
  useLayoutEffect(() => { if (!root.current.firstChild) fillSenderPresence(root.current); applySenderIdentity(root.current, identity); }, [identity.icon, identity.alt, identity.title, identity.dot?.cls, identity.dot?.title]);
  return html`<${SENDER_PRESENCE_ROOT.tag} class=${SENDER_PRESENCE_ROOT.class} ref=${root} />`;
}

function Stamp({ createdAt }) {
  const root = useRef(null);
  useLayoutEffect(() => { stampTime(root.current, createdAt); }, [createdAt]);
  return html`<time ref=${root} />`;
}

const ExpiredMark = () => html`<span class="message-expired-mark" title="Older than the expiry window: AI room chatter this old is not delivered to agents. Still shown here." role="img" aria-label="Expired from AI context" dangerouslySetInnerHTML=${{ __html: EXPIRED_HOURGLASS }} />`;

// The header line of a message, in the list and in the thread panel alike: sender, (bell), #number, bug, time.
export function MessageMeta({ message, view }) {
  const author = view.author;
  return html`<div class="message-meta"><span class="message-author-wrap">
    ${message.author_kind !== 'system' && html`<${Presence} identity=${author.identity} />`}
    <strong class=${author.link ? 'message-author message-author-link' : 'message-author'} data-sender-kind=${author.kind} tabindex=${author.link ? 0 : undefined} role=${author.link ? 'button' : undefined} title=${author.link ? author.title : undefined}>${author.name}</strong>
    <span class="message-role" title=${author.roleTitle} hidden=${!author.roleEmoji}>${author.roleEmoji}</span>
  </span>
  ${message.needs_attention && html`<span class="message-attention" title="Needs human attention" aria-label="Needs human attention">🔔</span>`}
  <button type="button" class="row-btn quiet-glyph message-number" title="Click to copy">${`#${message.no}`}</button>
  ${usageButton(view, message)}
  <button type="button" class="row-btn quiet-glyph chat-msg-debug-toggle" title="Open debug inspector" aria-label="Open debug inspector"><span class="icon-mask icon-mask-bug" aria-hidden="true"></span></button>
  ${view.expired && html`<${ExpiredMark} />`}<${Stamp} createdAt=${message.created_at} /></div>`;
}

export function Body({ message, view, host }) {
  const root = useRef(null);
  useLayoutEffect(() => { host.renderBody(root.current, message); }, [message, view.bodyKey]);
  return html`<div class="message-body" ref=${root} />`;
}

export function Plan({ message }) {
  const root = useRef(null);
  useLayoutEffect(() => { if (root.current) fillVisualPlan(root.current, message); }, [message]);
  return hasVisualPlan(message) && html`<${VISUAL_PLAN_ROOT.tag} class=${VISUAL_PLAN_ROOT.class} ref=${root} />`;
}

// The controls, the history or the "Answered" marker under a question; which one follows from the page's answer view.
// `listed` is the slot as a row of the message list, under its message's card and aligned with it (`self`: the viewer's own); in the thread panel it is part of the entry.
export function AnswerSlot({ message, view, host, listed = '' }) {
  const root = useRef(null), answer = view.answer;
  useLayoutEffect(() => {
    if (!root.current) return;
    if (answer.kind === 'controls') fillAnswerControls(root.current, answer.item, value => host.answer(answer.item, value), host.answerOptions(answer.parent));
    else if (answer.kind === 'history') fillAnswerHistory(root.current, message, answer.answered);
  }, [message, answer.key]);
  if (answer.kind === 'controls') return html`<${ANSWER_CONTROLS_ROOT.tag} key=${answer.key} class=${`${ANSWER_CONTROLS_ROOT.class}${listed}`} data-inbox-id=${answer.item.id} data-rev=${String(answer.item.rev)} ref=${root} />`;
  if (answer.kind === 'history') return html`<${ANSWER_HISTORY_ROOT.tag} key=${answer.key} class=${`${ANSWER_HISTORY_ROOT.class}${listed}`} data-answered=${String(answer.answered)} ref=${root} />`;
  if (answer.kind === 'done') return html`<div key=${answer.key} class=${`answer-done${listed}`}><button type="button" class="btn" disabled>Answered</button></div>`;
  return null;
}

export function ThreadEntry({ message, view, host }) {
  return html`<${Row} kind="thread" message=${message} view=${view} host=${host}>${() => html`<article class=${view.needsAttention ? 'thread-entry needs-attention' : 'thread-entry'} data-message-id=${message.id} data-seq=${String(message.seq)} data-no=${String(message.no)}>
    <${MessageMeta} message=${message} view=${view} /><${Body} message=${message} view=${view} host=${host} /><${Plan} message=${message} /><${AnswerSlot} message=${message} view=${view} host=${host} />
  </article>`}<//>`;
}

export function ThreadList({ entries = [], host, viewOf, restore }) {
  useLayoutEffect(() => { restore?.(); });
  return entries.map(message => html`<${ThreadEntry} key=${message.id} message=${message} view=${viewOf(message)} host=${host} />`);
}

function Actions({ message, host, label }) {
  const root = useRef(null), props = messageActionsProps(message);
  // The toolbar's buttons are made when the row is reached (Chat.wakeActions); one that was already made follows its message.
  useLayoutEffect(() => { if (root.current.dataset.built) fillMessageActions(root.current, host, message); else placeholderMessageActions(root.current); }, [message]);
  useLayoutEffect(() => { for (const button of root.current.querySelectorAll('.thread-action')) { button.title = label; button.setAttribute('aria-label', label); } }, [message, label]);
  return html`<${props.tag} class=${props.class} role=${props.role} aria-label=${props['aria-label']} ref=${root} />`;
}

const Pills = ({ pills }) => pills.length > 0 && html`<span class="reaction-pills">${pills.map(pill => html`<button type="button" class="row-btn reaction-pill" title=${pill.title} aria-pressed=${String(pill.pressed)} data-emoji=${pill.emoji}>${`${pill.emoji} ${pill.count}`}</button>`)}</span>`;

const Quote = ({ quote }) => html`<button type="button" class="row-btn quote" title=${quote.title}>${quote.text}</button>`;

// A notice shows its text as it is; a change of the Markdown mode draws it like any body, and a new mention index only highlights it.
function NoticeBody({ message, view, host }) {
  const root = useRef(null), drawn = useRef(false), marked = useRef(false);
  useLayoutEffect(() => {
    if (drawn.current) host.renderBody(root.current, message); else root.current.textContent = message.body;
    drawn.current = true;
  }, [message, view.raw]);
  useLayoutEffect(() => {
    if (marked.current) host.highlight(root.current);
    marked.current = true;
  }, [view.mentionKey]);
  return html`<span class="message-body" ref=${root} />`;
}

function SystemRow({ message, view, host, classes, register }) {
  return html`<article class=${classes} data-message-id=${message.id} data-seq=${String(message.seq)} data-no=${String(message.no)} ref=${register}>
    <div class="system-row"><${NoticeBody} message=${message} view=${view} host=${host} />${view.expired && html`<${ExpiredMark} />`}<${Stamp} createdAt=${message.created_at} /><button type="button" class="row-btn quiet-glyph chat-msg-debug-toggle" title="Open debug inspector" aria-label="Open debug inspector"><span class="icon-mask icon-mask-bug" aria-hidden="true"></span></button></div>
    <${Plan} message=${message} />
  </article>`;
}

export function MessageRow({ message, view, host }) {
  const register = useCallback(element => host.registerNode(message.id, element), [message.id]);
  const classes = [view.articleClass, view.needsAttention ? 'needs-attention' : '', view.expired ? 'message-expired' : '', view.boundary ? 'expiry-boundary' : ''].filter(Boolean).join(' ');
  return html`<${Row} kind="main" message=${message} view=${view} host=${host}>${() => message.author_kind === 'system' ? html`<${SystemRow} message=${message} view=${view} host=${host} classes=${classes} register=${register} />` : html`<${Fragment}><article class=${classes} data-message-id=${message.id} data-seq=${String(message.seq)} data-no=${String(message.no)} ref=${register}>
    ${view.boundary && html`<span class="expiry-label">Older AI chatter won't reach agents automatically</span>`}
    <${MessageMeta} message=${message} view=${view} />
    <div class="message-bubble">${view.quote && html`<${Quote} quote=${view.quote} />`}<${Body} message=${message} view=${view} host=${host} /><${Plan} message=${message} /><${Actions} message=${message} host=${host} label=${view.threadLabel} /><${Pills} pills=${view.pills} /></div>
    ${!view.chip.hidden && html`<button type="button" class="row-btn thread-chip" title="Open thread" aria-label=${view.chip.label}>${view.chip.text}</button>`}
  </article><${AnswerSlot} message=${message} view=${view} host=${host} listed=${/\bself\b/.test(view.articleClass || '') ? ' answer-row answer-row-self' : ' answer-row'} /><//>`}<//>`;
}

export function MessageList({ entries = [], host, viewOf, restore }) {
  useLayoutEffect(() => { restore?.(); });
  return entries.map(message => html`<${MessageRow} key=${message.id} message=${message} view=${viewOf(message)} host=${host} />`);
}

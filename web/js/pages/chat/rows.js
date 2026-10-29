import { html, Component, useLayoutEffect, useRef } from '../../ui.js';
import { stampTime } from '../../time.js';
import { applySenderIdentity, fillSenderPresence, SENDER_PRESENCE_ROOT } from '../../sender-identity.js';
import { fillVisualPlan, hasVisualPlan, VISUAL_PLAN_ROOT } from '../../visual-plan.js';
import { ANSWER_CONTROLS_ROOT, ANSWER_HISTORY_ROOT, fillAnswerControls, fillAnswerHistory } from '../../answer-controls.js';

export const rowRenders = { count: 0 };

// A row re-renders only when its record or one of the keys the page computes for it changes.
export class Row extends Component {
  shouldComponentUpdate(next) {
    const now = this.props;
    return next.message !== now.message || next.view.key !== now.view.key;
  }
  render({ message, view, host, className, children }) {
    rowRenders.count++;
    return children({ message, view, host, className });
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
  <${Stamp} createdAt=${message.created_at} /></div>`;
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
export function AnswerSlot({ message, view, host }) {
  const root = useRef(null), answer = view.answer;
  useLayoutEffect(() => {
    if (!root.current) return;
    if (answer.kind === 'controls') fillAnswerControls(root.current, answer.item, value => host.answer(answer.item, value), host.answerOptions(answer.parent));
    else if (answer.kind === 'history') fillAnswerHistory(root.current, message, answer.answered);
  }, [message, answer.key]);
  if (answer.kind === 'controls') return html`<${ANSWER_CONTROLS_ROOT.tag} key=${answer.key} class=${ANSWER_CONTROLS_ROOT.class} data-inbox-id=${answer.item.id} data-rev=${String(answer.item.rev)} ref=${root} />`;
  if (answer.kind === 'history') return html`<${ANSWER_HISTORY_ROOT.tag} key=${answer.key} class=${ANSWER_HISTORY_ROOT.class} data-answered=${String(answer.answered)} ref=${root} />`;
  if (answer.kind === 'done') return html`<div key=${answer.key} class="answer-done"><button type="button" class="btn" disabled>Answered</button></div>`;
  return null;
}

export function ThreadEntry({ message, view, host }) {
  return html`<${Row} message=${message} view=${view} host=${host}>${() => html`<article class=${view.needsAttention ? 'thread-entry needs-attention' : 'thread-entry'} data-message-id=${message.id} data-seq=${String(message.seq)} data-no=${String(message.no)}>
    <${MessageMeta} message=${message} view=${view} /><${Body} message=${message} view=${view} host=${host} /><${Plan} message=${message} /><${AnswerSlot} message=${message} view=${view} host=${host} />
  </article>`}<//>`;
}

export function ThreadList({ entries = [], host, viewOf, restore }) {
  useLayoutEffect(() => { restore?.(); });
  return entries.map(message => html`<${ThreadEntry} key=${message.id} message=${message} view=${viewOf(message)} host=${host} />`);
}

import { html, Component, Fragment, useCallback, useEffect, useLayoutEffect, useRef, useState } from '../../ui.js';
import { teamFlowSource } from '../../team-flow-source.js';
import { EXPIRED_HOURGLASS } from '../../usage-expiry.js';
import { clearMessageActions, drawMessageActions, messageActionsProps } from '../../message-actions.js';
import { stampTime } from '../../time.js';
import { treeLayout } from './model.js';
import { applySenderIdentity, fillSenderPresence, SENDER_PRESENCE_ROOT } from '../../sender-identity.js';
import { fillVisualPlan, hasVisualPlan, VISUAL_PLAN_ROOT } from '../../visual-plan.js';
import { AnswerControls, AnswerHistory } from '../../components/answer-controls.js';

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
  return html`<time class="chat-msg-time" ref=${root} />`;
}

const ExpiredMark = () => html`<span class="chat-msg-expired-icon" title="Older than the expiry window: AI room chatter this old is not delivered to agents. Still shown here." role="img" aria-label="Expired from AI context" dangerouslySetInnerHTML=${{ __html: EXPIRED_HOURGLASS }} />`;

// The header line of a message, in the list and in the thread panel alike: sender, (bell), #number, bug, time.
export function MessageMeta({ message, view }) {
  const author = view.author;
  return html`<div class="chat-msg-header"><span class="chat-msg-from">
    ${message.author_kind !== 'system' && html`<${Presence} identity=${author.identity} />`}
    <strong class=${author.link ? 'chat-msg-from-name agent-profile-link' : 'chat-msg-from-name'} data-sender-kind=${author.kind} tabindex=${author.link ? 0 : undefined} role=${author.link ? 'button' : undefined} title=${author.link ? author.title : undefined}>${author.name}</strong>
    <span class="message-role" title=${author.roleTitle} hidden=${!author.roleEmoji}>${author.roleEmoji}</span>
  </span>
  ${message.needs_attention && html`<span class="chat-msg-attention-icon" title="Needs human attention" aria-label="Needs human attention">🔔</span>`}
  <button type="button" class="row-btn quiet-glyph chat-msg-id" title="Click to copy">${`#${message.no}`}</button>
  ${usageButton(view, message)}
  <button type="button" class="row-btn quiet-glyph chat-msg-debug-toggle" title="Open debug inspector" aria-label="Open debug inspector"><span class="icon-mask icon-mask-bug" aria-hidden="true"></span></button>
  ${view.expired && html`<${ExpiredMark} />`}<${Stamp} createdAt=${message.created_at} /></div>`;
}

export function Body({ message, view, host }) {
  const root = useRef(null);
  useLayoutEffect(() => { host.renderBody(root.current, message); }, [message, view.bodyKey]);
  return html`<div class=${view.raw ? 'chat-msg-body' : 'chat-msg-body md-rendered'} ref=${root} />`;
}

// A reference grant reveals one message, never its room's history. The card
// contains plain text, so references inside the borrowed body do not become
// navigation links into a restricted room.
export function ReferencedMessages({ message }) {
  const numbers = [...new Set([...String(message.body || '').matchAll(/(?:^|[^\w])#([1-9]\d*)\b/g)].map(match => match[1]))].slice(0, 10);
  const [found, setFound] = useState([]);
  useEffect(() => {
    let current = true;
    setFound([]);
    if (numbers.length) Promise.all(numbers.map(no => teamFlowSource.referenced(no, message.id).catch(() => null))).then(items => {
      if (current) setFound(items.filter(item => item?.available));
    });
    return () => { current = false; };
  }, [message.id, message.body]);
  return found.length > 0 && html`<div class="chat-references" aria-label="Referenced messages">${found.map(item => html`<aside class="chat-reference" key=${item.no}>
    <header><strong>${`#${item.no}`}</strong><span>${item.room || ''}</span><span>${item.from || ''}</span></header>
    <pre>${item.body || ''}</pre>
  </aside>`)}</div>`;
}

export function Plan({ message }) {
  const root = useRef(null);
  useLayoutEffect(() => { if (root.current) fillVisualPlan(root.current, message); }, [message]);
  return hasVisualPlan(message) && html`<${VISUAL_PLAN_ROOT.tag} class=${VISUAL_PLAN_ROOT.class} ref=${root} />`;
}

// The controls, the history or the "Answered" marker under a question; which one follows from the page's answer view.
// `listed` keeps the slot under the message bubble and above its seen-by strip (`self`: the viewer's own).
export function AnswerSlot({ message, view, host, listed = '' }) {
  const answer = view.answer;
  if (answer.kind === 'controls') return html`<${AnswerControls} key=${answer.key} className=${listed} data-inbox-id=${answer.item.id} data-rev=${String(answer.item.rev)} item=${answer.item} submit=${value => host.answer(answer.item, value)} ...${host.answerOptions(answer.parent)} />`;
  if (answer.kind === 'history') return html`<${AnswerHistory} key=${answer.key} className=${listed} message=${message} answered=${answer.answered} />`;
  if (answer.kind === 'done') return html`<div key=${answer.key} class=${`answer-done${listed}`}><button type="button" class="btn" disabled>Answered</button></div>`;
  return null;
}

export function ThreadEntry({ message, view, host, tree }) {
  return html`<${Row} kind="thread" message=${message} view=${view} host=${host}>${() => html`<article class=${[view.articleClass, 'thread-entry', view.needsAttention ? 'needs-attention' : ''].filter(Boolean).join(' ')} data-message-id=${message.id} data-seq=${String(message.seq)} data-no=${String(message.no)} data-tree=${String(tree.shown)} style=${`--tree-levels:${tree.shown}`}>
    <${TreeGutter} tree=${tree} host=${host} message=${message} />
    <${MessageMeta} message=${message} view=${view} /><div class="chat-msg-bubble">${view.quote && html`<${Quote} quote=${view.quote} />`}<${Body} message=${message} view=${view} host=${host} /><${ReferencedMessages} message=${message} /><${Plan} message=${message} /><${Actions} message=${message} host=${host} label=${view.threadLabel} /><${Pills} pills=${view.pills} /></div><${AnswerSlot} message=${message} view=${view} host=${host} listed="" />
  </article>`}<//>`;
}

// The guide lines of a row of the tree: one cell per level, a vertical line where the ancestor has replies still to come and nothing where it was the last, and the
// row's own connector (a tee when more replies to its parent follow, an elbow when it is the last). The lines are drawn by the stylesheet; the words are for a screen reader
// (the structure is otherwise only in the picture) and, for a row too deep to show another step or one whose parent is not loaded, for everybody.
function TreeGutter({ tree, host, message }) {
  const parent = tree.parent || host.entryMessage?.(message.reply_to), number = parent?.no;
  const words = tree.orphan ? (number ? `from #${number}` : 'from an earlier message') : tree.branch && number ? `to #${number}` : '';
  return html`<${Fragment}>
    <div class="tree-gutter" aria-hidden="true">${tree.guides.map((on, index) => html`<span key=${index} class=${on ? 'tree-pipe' : 'tree-blank'} style=${`--i:${index}`}></span>`)}${tree.branch && html`<span class=${tree.last ? 'tree-elbow' : 'tree-tee'} style=${`--i:${tree.guides.length}`}></span>`}</div>
    ${words && html`<div class=${tree.deep || tree.orphan ? 'tree-label' : 'tree-label sr-only'}>${`\u21b3 ${words}`}</div>`}
  <//>`;
}

export function ThreadList({ entries = [], host, viewOf, restore }) {
  useLayoutEffect(() => { restore?.(); });
  // The conversation as `tree` prints it: the rows depth first, each with its guide lines.
  const { order, info } = treeLayout(entries);
  return order.map(message => {
    const shape = info.get(message.id), view = viewOf(message);
    // The place in the tree is part of what the row draws, so a row whose place changed (an older reply arriving) is drawn again.
    const placed = { ...view, key: `${view.key}|tree.${shape.shown}.${shape.guides.map(Number).join('')}.${shape.last}.${shape.deep}.${shape.orphan}.${shape.parent?.no || ''}` };
    return html`<${ThreadEntry} key=${message.id} message=${message} view=${placed} host=${host} tree=${shape} />`;
  });
}

function Actions({ message, host, label, thread }) {
  const root = useRef(null), props = messageActionsProps(message);
  // The toolbar is its own Preact root (message-actions.js): the placeholder until the row is reached (Chat.wakeActions), then the buttons, which follow the message and label.
  useLayoutEffect(() => { drawMessageActions(root.current, { chat: host, message, label, thread }); }, [message, label]);
  useLayoutEffect(() => () => clearMessageActions(root.current), []);
  return html`<${props.tag} class=${props.class} role=${props.role} aria-label=${props['aria-label']} ref=${root} />`;
}

const Pills = ({ pills }) => pills.length > 0 && html`<span class="reaction-pills">${pills.map(pill => html`<button type="button" class="row-btn reaction-pill" title=${pill.title} aria-pressed=${String(pill.pressed)} data-emoji=${pill.emoji}>${`${pill.emoji} ${pill.count}`}</button>`)}</span>`;

const Quote = ({ quote }) => html`<button type="button" class="row-btn quote" title=${quote.title}>${quote.text}</button>`;

// A notice shows its text as it is; a change of the Markdown mode draws it like any body, and a new mention index only highlights it.
function NoticeBody({ message, view, host }) {
  const root = useRef(null), drawn = useRef(false), marked = useRef(false);
  useLayoutEffect(() => {
    // Plain text first, as it is (its line breaks are kept by the body's own pre-wrap); the class that gives v1's Markdown rules, which collapse them, only after it is drawn.
    if (drawn.current) { host.renderBody(root.current, message); root.current.classList.toggle('md-rendered', !view.raw); } else { root.current.textContent = message.body; root.current.classList.remove('md-rendered'); }
    drawn.current = true;
  }, [message, view.raw]);
  useLayoutEffect(() => {
    if (marked.current) host.highlight(root.current);
    marked.current = true;
  }, [view.mentionKey]);
  return html`<span class="chat-msg-body" ref=${root} />`;
}

function SystemRow({ message, view, host, classes, register }) {
  return html`<article class=${classes} data-message-id=${message.id} data-seq=${String(message.seq)} data-no=${String(message.no)} ref=${register}>
    <div class="chat-msg-system-row"><${NoticeBody} message=${message} view=${view} host=${host} />${view.expired && html`<${ExpiredMark} />`}<${Stamp} createdAt=${message.created_at} /><button type="button" class="row-btn quiet-glyph chat-msg-debug-toggle" title="Open debug inspector" aria-label="Open debug inspector"><span class="icon-mask icon-mask-bug" aria-hidden="true"></span></button></div>
    <${Plan} message=${message} />
  </article>`;
}

export function MessageRow({ message, view, host }) {
  const register = useCallback(element => host.registerNode(message.id, element), [message.id]);
  const classes = [view.articleClass, view.needsAttention ? 'needs-attention' : '', view.expired ? 'chat-msg-expired' : '', view.boundary ? 'expiry-boundary' : ''].filter(Boolean).join(' ');
  return html`<${Row} kind="main" message=${message} view=${view} host=${host}>${() => message.author_kind === 'system' ? html`<${SystemRow} message=${message} view=${view} host=${host} classes=${classes} register=${register} />` : html`<${Fragment}><article class=${classes} data-message-id=${message.id} data-seq=${String(message.seq)} data-no=${String(message.no)} ref=${register}>
    ${view.boundary && html`<span class="expiry-label">Older AI chatter won't reach agents automatically</span>`}
    <${MessageMeta} message=${message} view=${view} />
    <div class="chat-msg-bubble">${view.quote && html`<${Quote} quote=${view.quote} />`}<${Body} message=${message} view=${view} host=${host} /><${ReferencedMessages} message=${message} /><${Plan} message=${message} /><${Actions} message=${message} host=${host} label=${view.threadLabel} /><${Pills} pills=${view.pills} />${!view.chip.hidden && html`<button type="button" class="row-btn thread-chip" title="Open the replies" aria-label=${view.chip.label}>${view.chip.text}</button>`}</div>
    <${AnswerSlot} message=${message} view=${view} host=${host} listed=${/\bself\b/.test(view.articleClass || '') ? ' answer-row answer-row-self' : ' answer-row'} />
  </article><//>`}<//>`;
}

export function MessageList({ entries = [], host, viewOf, restore }) {
  useLayoutEffect(() => { restore?.(); });
  return entries.map(message => html`<${MessageRow} key=${message.id} message=${message} view=${viewOf(message)} host=${host} />`);
}

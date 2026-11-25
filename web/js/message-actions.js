import { html, h, render, useState, useLayoutEffect, useRef } from './ui.js';
import { openEmojiPicker } from './emoji-picker-lazy.js';
import { openContextMenu } from './components/context-menu.js';
import { messageMenu } from './pages/chat/message-menu.js';

// What a message offers: one compact toolbar of icon buttons (react, reply, reply in a thread, and a last ⋮ with the less
// common actions), chosen over two other looks in the A/B/C trial. Every control is a framed .btn-icon with a tooltip and an
// accessible name; hovering or focusing the message reveals them (always visible on touch screens).
export const QUICK_REACTIONS = ['👍', '🆗', '✅', '👀', '🙏', '🎉'];

const ICONS = {
  react: [['path', { d: 'M12 5v14M5 12h14' }]],
  find: [['path', { d: 'M21 12a9 9 0 1 1-9-9' }], ['path', { d: 'M8 14s1.5 2 4 2 4-2 4-2' }], ['path', { d: 'M9 9h.01M15 9h.01' }], ['path', { d: 'M19 2v6M16 5h6' }]],
  reply: [['path', { d: 'M9 14L4 9l5-5' }], ['path', { d: 'M20 20v-7a4 4 0 0 0-4-4H4' }]],
  thread: [['path', { d: 'M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z' }]],
  link: [['path', { d: 'M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71' }], ['path', { d: 'M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71' }]],
  // The room header's "more" glyph (web/icons/more.svg) turned upright: the toolbar's last button, the message's shared menu.
  more: [['circle', { cx: 12, cy: 5, r: 1 }], ['circle', { cx: 12, cy: 12, r: 1 }], ['circle', { cx: 12, cy: 19, r: 1 }]],
};

// The toolbar's glyphs, also drawn by a message's context menu (pages/chat/message-menu.js).
export const Icon = ({ name }) => html`<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name].map(([tag, attrs]) => h(tag, attrs))}</svg>`;

function IconButton({ name, label, className = '', title, buttonRef, ...rest }) {
  return html`<button type="button" class=${`btn-icon message-action ${className}`.trim()} title=${title || label} aria-label=${label} ref=${buttonRef} ...${rest}><${Icon} name=${name} /></button>`;
}

// One pop-up (the quick emoji row) is open at a time; it closes on Escape or a click elsewhere.
let openPopup = null;
function closePopup() { if (!openPopup) return; const { close } = openPopup; openPopup = null; close(); }
document.addEventListener('click', event => { if (openPopup && !openPopup.popup.contains(event.target) && !openPopup.trigger.contains(event.target)) closePopup(); });
document.addEventListener('keydown', event => { if (event.key === 'Escape' && openPopup) { const { trigger } = openPopup; closePopup(); trigger.focus(); } });

// The full picker, for a reaction that is not one of the quick ones. `anchor` is the visible control it opens from.
function findReaction(chat, message, anchor) { closePopup(); openEmojiPicker({ anchor, onPick: glyph => chat.toggleReaction(message.id, glyph) }); }

// The toolbar's last button: it opens the message's shared menu (pages/chat/message-menu.js), the one a right-click opens, so
// there is one list of items. It sits anchored below the button (the shared placement rules keep it on the screen), Escape and a
// pick give the focus back to the button, and while it is open the toolbar keeps its aria-expanded (the bar stays visible with it).
function MoreButton({ chat, message, thread }) {
  const [open, setOpen] = useState(false), button = useRef(null);
  const show = () => {
    if (open) { closeContextMenu(false); return; }
    closePopup();
    const box = button.current.getBoundingClientRect(), { label, items } = messageMenu(chat, message, { thread, anchor: button.current });
    setOpen(true);
    openContextMenu({ point: { x: box.right, y: box.bottom }, label, items, returnFocus: button.current, onClose: () => setOpen(false) });
  };
  return html`<${IconButton} name="more" label="More actions" className="message-more" aria-haspopup="menu" aria-expanded=${String(open)} buttonRef=${button} onClick=${show} />`;
}

// Touch: a tap on a message selects it and shows its actions (the .actions-open class, see theme-v1.css); a tap on another message, on
// empty space or Escape hides them. Taps on links, mentions, buttons (reactions, the actions themselves, attachments, copy) and
// while text is selected do nothing here, and a tap on a button in another message only hides the open one.
const TAPPABLE = 'button, a, summary, details, input, textarea, select, label, .mention, [role=button], [role=menu]';
let selectedMessage = null;
function clearSelectedMessage() { selectedMessage?.classList.remove('actions-open'); selectedMessage = null; }
document.addEventListener('click', event => {
  if (!matchMedia('(hover: none)').matches) return;
  if (selectedMessage && !selectedMessage.isConnected) selectedMessage = null;
  const message = event.target.closest?.('.chat-msg:not(.chat-msg-system), .thread-entry');
  if (!message) { clearSelectedMessage(); return; }
  if (event.target.closest(TAPPABLE)) { if (message !== selectedMessage) clearSelectedMessage(); return; }
  if (getSelection()?.toString()) return;
  if (message === selectedMessage) { clearSelectedMessage(); return; }
  clearSelectedMessage(); selectedMessage = message; message.classList.add('actions-open');
});
document.addEventListener('keydown', event => { if (event.key === 'Escape') clearSelectedMessage(); });


export function messageActionsProps(message) {
  return { tag: 'div', class: 'message-actions', role: 'toolbar', 'aria-label': `Actions for message #${message.no}` };
}

function Toolbar({ chat, message, label, thread }) {
  const [open, setOpen] = useState(false), trigger = useRef(null), popup = useRef(null);
  useLayoutEffect(() => {
    if (!open) return undefined;
    const mine = { trigger: trigger.current, popup: popup.current, close: () => setOpen(false) };
    openPopup = mine; popup.current.querySelector('button')?.focus();
    return () => { if (openPopup === mine) openPopup = null; };
  }, [open]);
  const toggle = () => { if (open) { closePopup(); return; } closePopup(); setOpen(true); };
  const react = emoji => { closePopup(); chat.toggleReaction(message.id, emoji); };
  // Inside the panel a send answers the conversation's last message, so neither a reply nor a thread button is offered there (reply to an older message from the room).
  return html`<${IconButton} name="react" label="Add reaction" className="message-react" aria-haspopup="true" aria-expanded=${String(open)} onClick=${toggle} buttonRef=${trigger} />
    <${IconButton} name="find" label="Find another reaction" className="message-find" aria-haspopup="dialog" aria-expanded="false" onClick=${event => findReaction(chat, message, event.currentTarget)} />
    ${!thread && html`<${IconButton} name="reply" label=${`Reply to #${message.no}`} className="message-reply" onClick=${() => chat.setReply(message)} />
    <${IconButton} name="thread" label=${label ?? chat.threadLabel(message.id)} className="thread-action" onClick=${() => chat.openConversation(message)} />`}
    <${MoreButton} chat=${chat} message=${message} thread=${thread} />
    <div class="message-reaction-row" role="group" aria-label="Add reaction" hidden=${!open} ref=${popup}>${QUICK_REACTIONS.map(emoji => html`<button type="button" class="row-btn quick-reaction" title=${`React ${emoji}`} data-emoji=${emoji} onClick=${() => react(emoji)}>${emoji}</button>`)}</div>`;
}

// An unmade toolbar holds one button, visually hidden, so the keyboard and screen readers can still find it (Tab stop, browse mode); the row's
// pointer, focus and touch handlers replace it with the real buttons when the row is reached (Chat.wakeActions).
const Placeholder = () => html`<button type="button" class="row-btn message-actions-wake">Message actions</button>`;

// The toolbar of a row is built when the row is first reached (hovered, focused, touched) and released again once the reader has moved on: it is
// about 30 DOM nodes a row, which on a room of 100 rows was more than half of the page. `bar` is the row's empty .message-actions element, owned by
// the row; its contents are a Preact root of their own, drawn here at once (so a caller reads the buttons back) and cleared when the row goes.
const toolbars = new WeakMap();
function draw(bar) {
  const state = toolbars.get(bar); if (!state) return;
  if (state.built) bar.dataset.built = '1'; else delete bar.dataset.built;
  render(state.built ? html`<${Toolbar} chat=${state.chat} message=${state.message} label=${state.label} thread=${state.thread} />` : html`<${Placeholder} />`, bar);
}
// Draws a row's toolbar (the placeholder, or the made toolbar when the row's was already made); called again when the row's message or label changes.
export function drawMessageActions(bar, { chat, message, label, thread = !!bar.closest('.thread-panel') }) {
  const state = toolbars.get(bar) || {}; toolbars.set(bar, Object.assign(state, { chat, message, label, thread })); draw(bar);
}
export function clearMessageActions(bar) { toolbars.delete(bar); render(null, bar); delete bar.dataset.built; }
export function wakeMessageActions(chat, bar, message) {
  const state = toolbars.get(bar); if (!state || state.built) return false;
  state.built = true; state.chat = chat; state.message = message; draw(bar);
  return true;
}

// Returns whether the toolbar was released: one that holds the focus, the pointer, a tapped selection or an open pop-up is kept.
export function releaseMessageActions(bar) {
  const state = toolbars.get(bar); if (!state || !state.built) return true;
  const row = bar.closest('.chat-msg, .thread-entry');
  if (row && (row.matches(':hover, :focus-within, .actions-open') || bar.querySelector('[aria-expanded="true"]'))) return false;
  state.built = false; draw(bar); // a released toolbar is an unmade one again, so the keyboard can still find it
  return true;
}

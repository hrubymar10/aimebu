import { copyText } from './markdown.js';
import { openEmojiPicker } from './emoji-picker.js';

// What a message offers: one compact toolbar of icon buttons (react, reply, reply in a thread, copy the text, copy the link),
// chosen over two other looks in the A/B/C trial. Every control is a framed .btn-icon with a tooltip and an accessible name;
// hovering or focusing the message reveals them (always visible on touch screens).
export const QUICK_REACTIONS = ['👍', '🆗', '✅', '👀', '🙏', '🎉'];

const SVG = 'http://www.w3.org/2000/svg';
const ICONS = {
  react: '<path d="M12 5v14M5 12h14"/>',
  find: '<path d="M21 12a9 9 0 1 1-9-9"/><path d="M8 14s1.5 2 4 2 4-2 4-2"/><path d="M9 9h.01M15 9h.01"/><path d="M19 2v6M16 5h6"/>',
  reply: '<path d="M9 14L4 9l5-5"/><path d="M20 20v-7a4 4 0 0 0-4-4H4"/>',
  thread: '<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>',
  copy: '<rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/>',
  link: '<path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/>',
};

function icon(name) {
  const svg = document.createElementNS(SVG, 'svg');
  svg.setAttribute('width', '14'); svg.setAttribute('height', '14'); svg.setAttribute('viewBox', '0 0 24 24'); svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor'); svg.setAttribute('stroke-width', '2'); svg.setAttribute('stroke-linecap', 'round'); svg.setAttribute('stroke-linejoin', 'round'); svg.setAttribute('aria-hidden', 'true');
  svg.innerHTML = ICONS[name];
  return svg;
}

function iconButton(name, label, className = '') {
  const button = document.createElement('button'); button.type = 'button'; button.className = `btn-icon message-action ${className}`.trim();
  button.title = label; button.setAttribute('aria-label', label); button.append(icon(name));
  return button;
}

// Closes any open pop-up (the emoji row) on Escape or a click elsewhere.
let openPopup = null;
function closePopup() { if (!openPopup) return; const { trigger, popup } = openPopup; popup.hidden = true; trigger.setAttribute('aria-expanded', 'false'); openPopup = null; }
document.addEventListener('click', event => { if (openPopup && !openPopup.popup.contains(event.target) && !openPopup.trigger.contains(event.target)) closePopup(); });
document.addEventListener('keydown', event => { if (event.key === 'Escape' && openPopup) { const { trigger } = openPopup; closePopup(); trigger.focus(); } });

function attachPopup(trigger, popup) {
  trigger.setAttribute('aria-haspopup', 'true'); trigger.setAttribute('aria-expanded', 'false'); popup.hidden = true;
}
function togglePopup(trigger, popup) {
  const wasOpen = openPopup?.popup === popup; closePopup();
  if (!wasOpen) { popup.hidden = false; trigger.setAttribute('aria-expanded', 'true'); openPopup = { trigger, popup }; popup.querySelector('button')?.focus(); }
}

// The full picker, for a reaction that is not one of the quick ones. `anchor` is the visible control it opens from.
function findReaction(chat, message, anchor) { closePopup(); openEmojiPicker({ anchor, onPick: glyph => chat.toggleReaction(message.id, glyph) }); }

function emojiRow(message, chat) {
  const row = document.createElement('div'); row.className = 'message-reaction-row'; row.setAttribute('role', 'group'); row.setAttribute('aria-label', 'Add reaction');
  for (const emoji of QUICK_REACTIONS) {
    const button = document.createElement('button'); button.type = 'button'; button.className = 'row-btn quick-reaction'; button.textContent = emoji; button.title = `React ${emoji}`; button.dataset.emoji = emoji;
    row.append(button);
  }
  return row;
}

// Copies and reports the outcome on the button's tooltip, which is all a framed icon button has room for.
async function copyWithFeedback(button, text) {
  const label = button.dataset.label ||= button.title;
  try { await copyText(text); button.title = 'Copied'; } catch { button.title = 'Copy failed'; }
  setTimeout(() => { button.title = label; }, 1500);
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
  const message = event.target.closest?.('.message:not(.system-notice)');
  if (!message) { clearSelectedMessage(); return; }
  if (event.target.closest(TAPPABLE)) { if (message !== selectedMessage) clearSelectedMessage(); return; }
  if (getSelection()?.toString()) return;
  if (message === selectedMessage) { clearSelectedMessage(); return; }
  clearSelectedMessage(); selectedMessage = message; message.classList.add('actions-open');
});
document.addEventListener('keydown', event => { if (event.key === 'Escape') clearSelectedMessage(); });

// The toolbar's buttons carry no handlers of their own: the message list delegates one click listener for every row (see
// Chat.onMessageClick), which resolves the row's message and calls this. Returns whether `button` was an action.
export function messageAction(chat, button, message) {
  const is = name => button.classList.contains(name);
  if (is('quick-reaction')) { closePopup(); chat.toggleReaction(message.id, button.dataset.emoji); }
  else if (is('message-react')) { const popup = button.closest('.message-actions')?.querySelector(':scope > .message-reaction-row'); if (popup) togglePopup(button, popup); }
  else if (is('message-find')) findReaction(chat, message, button);
  else if (is('message-reply')) chat.setReply(message);
  else if (is('message-link')) copyWithFeedback(button, `#${message.no}`);
  else if (is('message-copy')) copyWithFeedback(button, message.body);
  else if (is('thread-action')) chat.openThread(message);
  else return false;
  return true;
}

export function messageActionsProps(message) {
  return { tag: 'div', class: 'message-actions', role: 'toolbar', 'aria-label': `Actions for message #${message.no}` };
}

// The toolbar of a row is built when the row is first reached (hovered, focused, touched) and released again once the reader has moved on: it is
// about 30 DOM nodes a row, which on a room of 100 rows was more than half of the page. `bar` is the row's empty .message-actions element.
// An unmade toolbar holds one button, visually hidden, so the keyboard and screen readers can still find it (Tab stop, browse mode); it is
// replaced by the real buttons when reached.
export function placeholderMessageActions(bar) {
  if (!bar || bar.dataset.built || bar.firstChild) return;
  const wake = document.createElement('button'); wake.type = 'button'; wake.className = 'row-btn message-actions-wake'; wake.textContent = 'Message actions';
  bar.append(wake);
}
export function wakeMessageActions(chat, bar, message) {
  if (!bar || bar.dataset.built) return false;
  fillMessageActions(bar, chat, message); bar.dataset.built = '1';
  return true;
}

// Returns whether the toolbar was released: one that holds the focus, the pointer, a tapped selection or an open pop-up is kept.
export function releaseMessageActions(bar) {
  if (!bar || !bar.dataset.built) return true;
  const row = bar.closest('.message');
  if (row && (row.matches(':hover, :focus-within, .actions-open') || bar.querySelector('[aria-expanded="true"]'))) return false;
  bar.replaceChildren(); delete bar.dataset.built;
  return true;
}

export function buildMessageActions(chat, message) {
  const props = messageActionsProps(message), bar = document.createElement(props.tag); bar.className = props.class;
  bar.setAttribute('role', props.role); bar.setAttribute('aria-label', props['aria-label']);
  fillMessageActions(bar, chat, message);
  return bar;
}

export function fillMessageActions(bar, chat, message) {
  bar.replaceChildren();
  const react = iconButton('react', 'Add reaction', 'message-react');
  const reply = iconButton('reply', `Reply to #${message.no}`, 'message-reply');
  const reactionRow = emojiRow(message, chat); attachPopup(react, reactionRow);
  const find = iconButton('find', 'Find another reaction', 'message-find'); find.setAttribute('aria-haspopup', 'dialog'); find.setAttribute('aria-expanded', 'false');
  const copyText_ = iconButton('copy', 'Copy message text', 'message-copy');
  const copyLink = iconButton('link', `Copy link to #${message.no}`, 'message-copy message-link');
  const thread = iconButton('thread', chat.threadLabel(message.id), 'thread-action');
  bar.append(react, find, reply, thread, copyText_, copyLink, reactionRow);
}

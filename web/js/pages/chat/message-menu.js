import { html } from '../../ui.js';
import { Icon } from '../../message-actions.js';
import { copyText } from '../../markdown.js';
import { openEmojiPicker } from '../../emoji-picker-lazy.js';

// The context menu of a message, in the list and in the thread panel: Mark unread from here, then what the message's toolbar and header already
// do (react, reply, the conversation, copy the text and the link, and the debug inspector while it is switched on). A system notice has no
// toolbar, so it offers only the read mark, the copies and the inspector. Inside the thread panel a send answers the conversation, so neither
// reply nor the conversation is offered there, as on the toolbar.
export function messageMenu(chat, message, { thread = false, anchor = null } = {}) {
  const system = message.author_kind === 'system';
  const copy = text => copyText(text).catch(() => chat.say('Could not copy to the clipboard.'));
  return {
    label: `Actions for message #${message.no}`,
    items: [
      { id: 'mark-unread', label: 'Mark unread from here', icon: 'message', hidden: !chat.markUnreadFrom || !(message.seq > 0) || !chat.room, onSelect: () => chat.markUnreadFrom(chat.room, message.seq) },
      { separator: true },
      { id: 'react', label: 'Add reaction…', icon: html`<${Icon} name="react" />`, hidden: system, onSelect: () => openEmojiPicker({ anchor: anchor?.querySelector('.message-actions') || anchor, onPick: glyph => chat.toggleReaction(message.id, glyph) }) },
      { id: 'reply', label: `Reply to #${message.no}`, icon: html`<${Icon} name="reply" />`, hidden: system || thread, onSelect: () => chat.setReply(message) },
      { id: 'thread', label: chat.threadLabel(message.id), icon: html`<${Icon} name="thread" />`, hidden: system || thread, onSelect: () => chat.openConversation(message) },
      { separator: true },
      { id: 'copy-text', label: 'Copy message text', icon: 'copy', onSelect: () => copy(message.body || '') },
      { id: 'copy-link', label: `Copy link to #${message.no}`, icon: 'link', onSelect: () => copy(`#${message.no}`) },
      { separator: true },
      { id: 'debug', label: 'Open debug inspector', icon: 'bug', hidden: !chat.debugCallback || document.documentElement.dataset.debugInspector !== 'on', onSelect: () => chat.debugCallback(message) },
    ],
  };
}

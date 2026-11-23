import { html } from '../../ui.js';
import { NOTIFY_LEVELS, MUTE_CHOICES, muteUntil, mutedNow } from '../../room-notify.js';
import { rowMenu } from './model.js';
import { newRoomDialog } from './sidebar-list.js';

// The gear of the header's Room settings button (#room-info in index.html).
const GEAR = html`<svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M8 4.754a3.246 3.246 0 100 6.492 3.246 3.246 0 000-6.492zM5.754 8a2.246 2.246 0 114.492 0 2.246 2.246 0 01-4.492 0z"/><path d="M9.796 1.343c-.527-1.79-3.065-1.79-3.592 0l-.094.319a.873.873 0 01-1.255.52l-.292-.16c-1.64-.892-3.433.902-2.54 2.541l.159.292a.873.873 0 01-.52 1.255l-.319.094c-1.79.527-1.79 3.065 0 3.592l.319.094a.873.873 0 01.52 1.255l-.16.292c-.892 1.64.902 3.434 2.541 2.54l.292-.159a.873.873 0 011.255.52l.094.319c.527 1.79 3.065 1.79 3.592 0l.094-.319a.873.873 0 011.255-.52l.292.16c1.64.892 3.434-.902 2.54-2.541l-.159-.292a.873.873 0 01.52-1.255l.319-.094c1.79-.527 1.79-3.065 0-3.592l-.319-.094a.873.873 0 01-.52-1.255l.16-.292c.892-1.64-.902-3.433-2.541-2.54l-.292.159a.873.873 0 01-1.255-.52l-.094-.319zm-2.633.283c.246-.835 1.428-.835 1.674 0l.094.319a1.873 1.873 0 002.693 1.115l.291-.16c.764-.415 1.6.42 1.184 1.185l-.159.292a1.873 1.873 0 001.116 2.692l.318.094c.835.246.835 1.428 0 1.674l-.319.094a1.873 1.873 0 00-1.115 2.693l.16.291c.415.764-.42 1.6-1.185 1.184l-.291-.159a1.873 1.873 0 00-2.693 1.116l-.094.318c-.246.835-1.428.835-1.674 0l-.094-.319a1.873 1.873 0 00-2.692-1.115l-.292.16c-.764.415-1.6-.42-1.184-1.185l.159-.291a1.873 1.873 0 00-1.116-2.693l-.318-.094c-.835-.246-.835-1.428 0-1.674l.319-.094a1.873 1.873 0 001.115-2.692l-.16-.292c-.415-.764.42-1.6 1.185-1.184l.292.159a1.873 1.873 0 002.692-1.116l.094-.318z"/></svg>`;

// The context menu of a room row: what model.js's rowMenu offers, each item calling a handler the list or the room settings already have. The
// settings, archiving and leaving belong to the open room, so `store.roomActions` (set by app.js) opens the room first and then runs the room
// settings' own action.
export function roomRowMenu(store, room) {
  const project = store.projects.get(room.project_id), plan = rowMenu(room, { role: project?.role || '', archivedProject: !!project?.archived_at });
  const actions = store.roomActions || {};
  const level = NOTIFY_LEVELS.some(item => item.value === room.notify) ? room.notify : 'default';
  const muted = mutedNow(room);
  return {
    label: `${room.title} actions`,
    items: [
      plan.read === 'read'
        ? { id: 'mark-read', label: 'Mark as read', icon: 'check', onSelect: () => store.markRead(room) }
        : { id: 'mark-unread', label: 'Mark unread', icon: 'message', onSelect: () => store.markUnread(room) },
      { separator: true },
      { id: 'settings', label: plan.settings, icon: GEAR, hidden: !actions.settings, onSelect: () => actions.settings(room) },
      { id: 'new-room', label: 'New task room…', icon: 'plus', hidden: !plan.newRoom, onSelect: () => newRoomDialog(store, room) },
      { separator: true },
      { group: 'Notify me about', items: NOTIFY_LEVELS.map(item => ({ id: `notify-${item.value}`, label: item.label, title: item.help, checked: item.value === level, onSelect: () => store.setPreference(room, 'notify', item.value) })) },
      { separator: true },
      ...(muted
        ? [{ id: 'unmute', label: `Unmute (muted until ${new Date(room.muted_until).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' })})`, icon: 'bell', onSelect: () => store.setPreference(room, 'muted_until', '') }]
        : MUTE_CHOICES.map(choice => ({ id: `mute-${choice.value}`, label: choice.label, icon: 'bell-disabled', onSelect: () => store.setPreference(room, 'muted_until', muteUntil(choice.value).toISOString()) }))),
      { separator: true },
      { id: 'hide', label: 'Hide room', icon: 'eye-off', hidden: !plan.hide, onSelect: () => store.setPreference(room, 'hidden', true) },
      { id: 'reopen', label: 'Reopen room', icon: 'reopen', hidden: !plan.reopen, onSelect: () => store.reopen(room) },
      { id: 'archive', label: 'Archive room…', icon: 'archive', hidden: !plan.archive || !actions.archive, onSelect: () => actions.archive(room) },
      { id: 'leave', label: 'Leave chat', icon: 'left', hidden: !plan.leave || !actions.leave, onSelect: () => actions.leave(room) },
    ],
  };
}

// Per-room notification settings of one person: the level of the room and an optional mute that ends by itself. The server stores them
// on the room (`notify`, `muted_until`); these helpers are what the sidebar, the room settings and the notifier all ask.

// The levels, in the order the room settings list them.
export const NOTIFY_LEVELS = [
  { value: 'default', label: 'Default', help: 'The Inbox, and messages that address you (a mention, a reply, a group you are in).' },
  { value: 'all', label: 'All messages', help: 'Every new message in this room, as well as the Inbox.' },
  { value: 'mentions', label: 'Only mentions and replies to me', help: 'Only messages that address you; the unread count in the room list counts only those too.' },
  { value: 'none', label: 'Nothing', help: 'No sound or notification from this room, its Inbox items included. They still arrive in the Inbox.' },
];

// The temporary mutes, by the hours they last; "tomorrow" ends at the next local 08:00.
export const MUTE_CHOICES = [
  { value: '1h', label: 'Mute for 1 hour' },
  { value: '8h', label: 'Mute for 8 hours' },
  { value: 'tomorrow', label: 'Mute until tomorrow' },
];

export function muteUntil(choice, now = new Date()) {
  if (choice === '1h') return new Date(now.getTime() + 3600 * 1000);
  if (choice === '8h') return new Date(now.getTime() + 8 * 3600 * 1000);
  if (choice === 'tomorrow') { const end = new Date(now); end.setDate(end.getDate() + 1); end.setHours(8, 0, 0, 0); return end; }
  return null;
}

// Whether a mute is still running.
export function mutedNow(room, now = Date.now()) { return !!room?.muted_until && Date.parse(room.muted_until) > now; }

// A room that is quiet by its person's own choice: the level "none", or a mute that has not run out yet.
export function roomSilenced(room, now = Date.now()) { return !!room && (room.notify === 'none' || mutedNow(room, now)); }

// Whether a chat message is worth a sound or a notification, by the level the person set for its room: "all" for every message from
// somebody else, "default" and "mentions" for a message that addresses them (a mention, a reply, a group they are in), "none" never.
// The message is the event's payload: the author and the members it addresses.
export function messageNotifies(room, message, me, now = Date.now()) {
  if (!room || !message || roomSilenced(room, now) || message.author_kind === 'system' || message.author_id === me) return false;
  if (room.notify === 'all') return true;
  return Array.isArray(message.targets) && message.targets.includes(me);
}

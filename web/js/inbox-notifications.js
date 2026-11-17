import { savePreference } from './preferences.js';
import { currentUser, request } from './api.js';
import { roomSilenced, messageNotifies } from './room-notify.js';

const DEFAULT_SOUND = 'builtin:chime';
const BUILTIN_SOUNDS = ['chime', 'ding', 'beep', 'knock'];
// An inbox item and the chat message it came from reach the browser as two events. Whichever is first speaks; the other, in the same room
// within this window, stays quiet. A burst of messages in one room is likewise one sound.
const COALESCE_MS = 3000;

// The most of a message a desktop notification shows when the person asked for its text: the first lines, flattened to one.
export function notificationSnippet(body, max = 140) {
  const flat = String(body || '').replace(/\s+/g, ' ').trim();
  return flat.length > max ? `${flat.slice(0, max - 1).trimEnd()}…` : flat;
}

export class InboxNotifications {
  constructor(onOpen, onOpenRoom) {
    this.onOpen = onOpen; this.onOpenRoom = onOpenRoom;
    // Called whenever something needs the person's attention (an Inbox item or a message that addresses them), whatever the room's sound level.
    this.onAttention = null; this.onPermission = null;
    this.messageClaimed = new Map(); this.inboxClaimed = new Map();
    this.audio = new Audio('/sounds/chime.wav');
    this.audio.preload = 'auto';
    this.audio.volume = 0.7;
    // A chosen sound that fails to load (deleted by the owner, possibly from another tab) plays the
    // chime for that one play only. The saved choice is kept; Settings drops it once the server list
    // confirms the sound is gone, and the next play re-points the element at the choice.
    this.audio.addEventListener('error', () => {
      if (!this.pending || this.audio.getAttribute('src') === this.soundURL(DEFAULT_SOUND)) return;
      this.pending = false;
      this.audio.src = this.soundURL(DEFAULT_SOUND);
      this.audio.play().catch(() => {});
    });
    this.audio.addEventListener('playing', () => { this.pending = false; });
    const prime = () => {
      if (this.primed) return;
      this.primed = true;
      this.audio.muted = true;
      this.audio.play().then(() => {
        this.audio.pause(); this.audio.currentTime = 0; this.audio.muted = false;
      }).catch(() => { this.audio.muted = false; });
    };
    document.addEventListener('pointerdown', prime, { once: true });
    document.addEventListener('keydown', prime, { once: true });
  }
  get soundID() { return localStorage.getItem(`aimebu2:inbox-sound:${currentUser}`) || DEFAULT_SOUND; }
  set soundID(value) { if (value && value !== DEFAULT_SOUND) localStorage.setItem(`aimebu2:inbox-sound:${currentUser}`, value); else localStorage.removeItem(`aimebu2:inbox-sound:${currentUser}`); savePreference('inbox_sound', value || DEFAULT_SOUND); }
  get volume() { const stored = localStorage.getItem(`aimebu2:inbox-volume:${currentUser}`); return stored === null || !Number.isFinite(Number(stored)) ? 70 : Math.min(100, Math.max(0, Number(stored))); }
  set volume(value) { const level = String(Math.min(100, Math.max(0, Math.round(Number(value) || 0)))); localStorage.setItem(`aimebu2:inbox-volume:${currentUser}`, level); savePreference('inbox_volume', level); }
  // Bundled sounds live under /sounds; uploads are served by the API.
  soundURL(id) {
    if (id.startsWith('user:')) return `/api/sounds/${encodeURIComponent(id.slice(5))}`;
    const name = id.startsWith('builtin:') ? id.slice(8) : '';
    return BUILTIN_SOUNDS.includes(name) ? `/sounds/${name}.wav` : `/sounds/chime.wav`;
  }
  // Point the shared audio element at the chosen sound and volume.
  sync() {
    const url = this.soundURL(this.soundID);
    if (this.audio.getAttribute('src') !== url) this.audio.src = url;
    this.audio.volume = this.volume / 100;
  }
  play() {
    this.sync();
    this.pending = true;
    this.audio.pause(); this.audio.currentTime = 0;
    this.audio.play().catch(() => {});
  }
  get muted() { return localStorage.getItem(`aimebu2:inbox-muted:${currentUser}`) === '1'; }
  set muted(value) { localStorage.setItem(`aimebu2:inbox-muted:${currentUser}`, value ? '1' : '0'); savePreference('inbox_muted', value ? '1' : '0'); }
  // Whether a desktop notification for a chat message shows the start of its text (off by default: the text of a room can be private to
  // whoever sits at the screen). The inbox items' own notifications already show their question.
  get showText() { return localStorage.getItem(`aimebu2:notification-text:${currentUser}`) === '1'; }
  set showText(value) { localStorage.setItem(`aimebu2:notification-text:${currentUser}`, value ? '1' : '0'); savePreference('notification_text', value ? '1' : '0'); }
  // The text of one message, read through the room's own message list so the room's access rules apply; '' when it cannot be had.
  async snippetOf(roomID, messageID, send = request) {
    try {
      // The room's message list answers with a bare array.
      const list = await send(`/api/rooms/${encodeURIComponent(roomID)}/messages?around=${encodeURIComponent(messageID)}&limit=3`);
      return notificationSnippet((Array.isArray(list) ? list : []).find(item => item.id === messageID)?.body);
    } catch { return ''; }
  }
  get available() { return window.isSecureContext && 'Notification' in window; }
  get status() {
    if (!window.isSecureContext) return 'Browser notifications require a secure context (HTTPS or localhost); this page cannot show them.';
    if (!('Notification' in window)) return 'Browser notifications are unavailable in this browser.';
    const words = { default: 'not asked yet', granted: 'allowed', denied: 'blocked' }[Notification.permission] || Notification.permission; // the browser's own words are "default", "granted" and "denied"
    return `Browser permission: ${words}. Notifications appear only while this tab is hidden.`;
  }
  async requestPermission() {
    if (!this.available || Notification.permission !== 'default') return;
    await Notification.requestPermission();
    this.onPermission?.(); // whoever shows an offer of notifications (the bar) re-reads the answer
  }
  arrived(items, projects, rooms) {
    // A room set to nothing, or muted for now, stays quiet; the item is in the Inbox all the same.
    items = items.filter(item => !roomSilenced(rooms.get(item.room_id)) && !this.recent(this.messageClaimed, item.room_id));
    if (!items.length) return;
    this.onAttention?.();
    const now = Date.now();
    for (const item of items) this.inboxClaimed.set(item.room_id, now);
    if (!this.muted) {
      this.play();
    }
    if (!this.available || Notification.permission !== 'granted' || !document.hidden) return;
    for (const item of items) {
      const room = item.kind === 'usage_alert' ? 'Usages' : rooms.get(item.room_id)?.title || 'a room';
      const project = projects.get(item.project_id)?.name;
      try {
        const note = new Notification(`aimebu · ${room}`, {
          body: `${project ? `${project} · ` : ''}${item.question || 'New inbox item'}`.slice(0, 180),
          icon: '/icons/icon-192.png',
          tag: `aimebu-inbox-${item.id}`,
          silent: true,
        });
        note.onclick = () => { window.focus(); this.onOpen(item); note.close(); };
      } catch { /* Permission may have changed between the check and display. */ }
    }
  }
  recent(claims, roomID) { return Date.now() - (claims.get(roomID) || 0) < COALESCE_MS; }
  // A chat message from the event stream. `watching` is true when the person is looking at that room right now, which needs no alert.
  // The global mute is the master switch for the sound; the desktop note only shows while the tab is hidden, as for the inbox.
  message(message, room, projects, me, watching) {
    if (!messageNotifies(room, message, me)) return;
    this.onAttention?.();
    if (this.recent(this.inboxClaimed, room.id) || this.recent(this.messageClaimed, room.id)) return;
    // The room being read is claimed too, without a sound: the Inbox item the same message opens must not chime for what is on screen.
    this.messageClaimed.set(room.id, Date.now());
    if (watching) return;
    if (!this.muted) this.play();
    if (!this.available || Notification.permission !== 'granted' || !document.hidden) return;
    const project = projects.get(room.project_id)?.name;
    const addressed = Array.isArray(message.targets) && message.targets.includes(me);
    const show = text => {
      // The text took a moment to arrive: the person may have come back to the tab, or taken the permission away, meanwhile.
      if (!document.hidden || !this.available || Notification.permission !== 'granted') return;
      try {
        const note = new Notification(`aimebu · ${room.title || 'a room'}`, {
          body: `${project ? `${project} · ` : ''}${this.showText && text || (addressed ? 'A message for you' : 'New message')}`.slice(0, 180),
          icon: '/icons/icon-192.png',
          tag: `aimebu-room-${room.id}`,
          silent: true,
        });
        note.onclick = () => { window.focus(); this.onOpenRoom?.(room, message.message_id); note.close(); };
      } catch { /* Permission may have changed between the check and display. */ }
    };
    // The text is asked for only now, when a note is really about to show, and only with the setting on; any failure shows the plain note.
    if (this.showText) this.snippetOf(room.id, message.message_id).then(show); else show('');
  }
}

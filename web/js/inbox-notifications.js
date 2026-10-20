import { savePreference } from './preferences.js';
import { currentUser } from './api.js';

const DEFAULT_SOUND = 'builtin:chime';
const BUILTIN_SOUNDS = ['chime', 'ding', 'beep', 'knock'];

export class InboxNotifications {
  constructor(onOpen) {
    this.onOpen = onOpen;
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
  get available() { return window.isSecureContext && 'Notification' in window; }
  get status() {
    if (!window.isSecureContext) return 'Browser notifications require a secure context (HTTPS or localhost); this page cannot show them.';
    if (!('Notification' in window)) return 'Browser notifications are unavailable in this browser.';
    return `Browser permission: ${Notification.permission}. Notifications appear only while this tab is hidden.`;
  }
  async requestPermission() {
    if (!this.available || Notification.permission !== 'default') return;
    await Notification.requestPermission();
  }
  arrived(items, projects, rooms) {
    if (!items.length) return;
    if (!this.muted) {
      this.play();
    }
    if (!this.available || Notification.permission !== 'granted' || !document.hidden) return;
    for (const item of items) {
      const room = rooms.get(item.room_id)?.title || 'a room';
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
}

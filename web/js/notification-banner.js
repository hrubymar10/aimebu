// A quiet bar that offers browser notifications. It shows once, the first time something needs the person's attention (an Inbox item, or a
// chat message that addresses them) while the browser has not been asked yet: the permission prompt is only ever raised by a click on it.
// Dismissing it is remembered on the account (a preference), so no browser shows it again; granting or refusing the permission, from here or
// from Settings, ends it too, since the browser then no longer says "default". Nothing about the message is put in it.
import { iconButton, iconGlyph } from './icon-button.js';

export const BANNER_DISMISSED_PREFERENCE = 'notification_banner_dismissed';

// Whether the bar may be shown now. `shown` is true once it has been shown in this page (it is not shown twice, so closing nothing and
// looking away does not bring it back on every message).
export function bannerWanted({ available, permission, dismissed, shown }) {
  return !!available && permission === 'default' && !dismissed && !shown;
}

export class NotificationBanner {
  // `notifications` answers `available` and `requestPermission()`; `saveDismissed()` stores the dismissal on the account.
  constructor({ notifications, saveDismissed, anchor = document.querySelector('.chat > header'), permission = () => ('Notification' in window ? Notification.permission : 'denied') }) {
    this.notifications = notifications; this.saveDismissed = saveDismissed; this.permission = permission;
    this.dismissed = false; this.shown = false; this.waiting = false;
    this.bar = document.createElement('div'); this.bar.className = 'notification-banner'; this.bar.hidden = true;
    this.bar.setAttribute('role', 'region'); this.bar.setAttribute('aria-label', 'Browser notifications');
    this.text = document.createElement('span'); this.text.className = 'notification-banner-text';
    this.text.textContent = 'Get a browser notification when something needs you.';
    this.enable = iconButton('bell', 'Enable browser notifications', { className: 'notification-banner-enable' });
    this.dismiss = iconButton('close', 'Dismiss this and do not ask again', { className: 'notification-banner-dismiss' });
    this.bar.append(iconGlyph('bell'), this.text, this.enable, this.dismiss);
    anchor.after(this.bar);
    this.enable.onclick = () => void this.ask();
    this.dismiss.onclick = () => this.close(true);
    // A message that arrives while the tab is in the background is offered when the person comes back to it.
    document.addEventListener('visibilitychange', () => { this.sync(); if (!document.hidden && this.waiting) { this.waiting = false; this.attention(); } });
    // The permission can change behind the bar's back (from Settings, or in the browser's own site settings): the bar goes with it.
    try { navigator.permissions?.query({ name: 'notifications' }).then(status => { status.onchange = () => this.sync(); }, () => {}); } catch { /* not every browser can be asked */ }
  }
  // Drops the bar, shown or waiting, once the browser no longer says "default".
  sync() { if (this.permission() !== 'default') { this.bar.hidden = true; this.waiting = false; } }
  // The account's saved dismissal, read at sign-in; a new sign-in starts from a closed bar.
  reset(dismissed) { this.dismissed = !!dismissed; this.shown = false; this.waiting = false; this.bar.hidden = true; }
  // Something needs the person's attention.
  attention() {
    if (!bannerWanted({ available: this.notifications.available, permission: this.permission(), dismissed: this.dismissed, shown: this.shown })) return;
    if (document.hidden) { this.waiting = true; return; }
    this.show();
  }
  show() { this.shown = true; this.bar.hidden = false; }
  async ask() {
    this.enable.disabled = true;
    try { await this.notifications.requestPermission(); } catch { /* the browser refused to ask; the bar is done either way */ }
    this.close(false);
    this.enable.disabled = false;
  }
  close(forGood) {
    this.bar.hidden = true;
    if (forGood) { this.dismissed = true; void Promise.resolve(this.saveDismissed?.()).catch(() => {}); }
  }
}

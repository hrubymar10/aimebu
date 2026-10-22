// Room settings → Notifications: how this room speaks to the signed-in person (the level), and a mute that ends by itself. Both are the
// person's own and live on the server with the rest of their view of the room, so they follow them to other devices.
import { iconGlyph } from './icon-button.js';
import { NOTIFY_LEVELS, MUTE_CHOICES, muteUntil, mutedNow } from './room-notify.js';

const node = (tag, className = '', text = '') => { const el = document.createElement(tag); if (className) el.className = className; if (text) el.textContent = text; return el; };

export class RoomNotifySettings {
  // `save(field, value)` stores one preference of the room and answers whether it worked; `current()` is the room as the list knows it.
  constructor(before, save, current) {
    this.save = save; this.current = current; this.requestID = 0;
    this.section = node('section', 'room-notify-setting'); this.section.setAttribute('aria-labelledby', 'room-notify-title');
    const heading = node('h3'); heading.id = 'room-notify-title'; heading.append(iconGlyph('bell'), ' Notifications');
    this.level = node('select'); this.level.id = 'room-notify-level';
    for (const { value, label } of NOTIFY_LEVELS) { const option = node('option', '', label); option.value = value; this.level.append(option); }
    this.mute = node('select'); this.mute.id = 'room-notify-mute';
    this.help = node('p', 'settings-intro'); this.help.id = 'room-notify-help';
    this.status = node('p', 'room-notify-status'); this.status.setAttribute('role', 'status');
    const levelLabel = node('label', '', 'Notify me about '); levelLabel.append(this.level);
    const muteLabel = node('label', '', 'Mute '); muteLabel.append(this.mute);
    this.level.setAttribute('aria-describedby', 'room-notify-help');
    this.section.append(heading, levelLabel, this.help, muteLabel, this.status);
    before.before(this.section);
    this.level.onchange = () => this.change('notify', this.level.value);
    this.mute.onchange = () => {
      if (this.mute.value === 'keep') return;
      const until = this.mute.value === 'off' ? null : muteUntil(this.mute.value);
      this.change('muted_until', until ? until.toISOString() : '');
    };
  }
  // Shows the room's current settings; a mute that is running is the first choice of the mute list, so it reads as what is in force.
  render(room = this.current()) {
    if (!room) return;
    const level = NOTIFY_LEVELS.find(item => item.value === room.notify) || NOTIFY_LEVELS[0];
    this.level.value = level.value; this.help.textContent = level.help;
    this.mute.replaceChildren();
    const muted = mutedNow(room);
    if (muted) this.mute.append(Object.assign(node('option', '', `Muted until ${new Date(room.muted_until).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' })}`), { value: 'keep' }));
    this.mute.append(Object.assign(node('option', '', muted ? 'Unmute' : 'Not muted'), { value: 'off' }));
    for (const { value, label } of MUTE_CHOICES) this.mute.append(Object.assign(node('option', '', label), { value }));
    this.mute.value = muted ? 'keep' : 'off';
  }
  async change(field, value) {
    const requestID = ++this.requestID;
    this.level.disabled = this.mute.disabled = true; this.status.textContent = '';
    const done = await this.save(field, value);
    if (requestID !== this.requestID) return;
    this.level.disabled = this.mute.disabled = false;
    this.render();
    this.status.textContent = done ? 'Saved.' : '';
  }
}

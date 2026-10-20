// Settings → Notifications: pick the inbox chime and its volume (both browser preferences), and
// let the owner manage the small pool of uploaded sounds everyone can choose from.
import { request, upload, rights } from './api.js';
import { iconButton } from './icon-button.js';
import { settingsRow, settingsGroup } from './settings-row.js';

const node = (tag, className = '', text = '') => { const el = document.createElement(tag); if (className) el.className = className; if (text) el.textContent = text; return el; };

export class NotificationSounds {
  constructor(section, notifications) {
    this.notifications = notifications; this.sounds = []; this.owner = false;
    this.select = node('select'); this.select.id = 'notification-sound';
    this.preview = node('button', 'btn', 'Preview'); this.preview.type = 'button'; this.preview.id = 'notification-sound-preview';
    const choice = node('div', 'notification-sound-choice'); choice.append(this.select, this.preview);
    this.volume = node('input'); this.volume.type = 'range'; this.volume.min = '0'; this.volume.max = '100'; this.volume.step = '5'; this.volume.id = 'notification-volume'; this.volume.className = 'range-input';
    // The value is shown next to the slider and read out as a percentage; the track fills up to the thumb's centre.
    this.volumeValue = node('output', 'range-value'); this.volumeValue.id = 'notification-volume-value'; this.volumeValue.htmlFor = 'notification-volume'; this.volumeValue.setAttribute('aria-hidden', 'true');
    const volume = node('div', 'settings-range'); volume.append(this.volume, this.volumeValue);
    this.file = node('input'); this.file.type = 'file'; this.file.accept = '.mp3,.wav,audio/mpeg,audio/wav'; this.file.id = 'notification-sound-upload';
    this.list = node('ul', 'notification-sound-list');
    const manage = node('div', 'notification-sound-manage'); manage.append(this.file, this.list);
    this.group = settingsGroup([
      settingsRow({ label: 'Inbox sound', desc: 'The sound that plays for new inbox items. Only this browser uses your choice.', control: choice }),
      settingsRow({ label: 'Volume', desc: 'How loud the inbox sound plays, from silent to full volume.', control: volume }),
    ], 'Inbox sound');
    this.manageRow = settingsRow({ label: 'Custom sounds', desc: 'Upload .mp3 or .wav files up to 1 MB each, ten at most. Everyone signed in can choose them; only the owner or an admin can add or delete.', control: manage });
    this.manageRow.hidden = true; this.group.append(this.manageRow);
    this.status = node('p', 'settings-intro'); this.status.setAttribute('role', 'status');
    section.querySelector('#notification-status').before(this.group, this.status);
    this.select.onchange = () => { this.notifications.soundID = this.select.value; };
    this.volume.oninput = () => { this.notifications.volume = this.volume.value; this.renderVolume(); };
    this.preview.onclick = () => this.notifications.play();
    this.file.onchange = () => this.add();
    this.renderVolume();
  }
  async open() {
    this.status.textContent = '';
    this.volume.value = String(this.notifications.volume);
    this.renderVolume();
    this.owner = rights.admin; // known from sign-in; asking /api/users would only answer 403 for everyone else
    await this.load();
  }
  renderVolume() {
    const value = Number(this.volume.value), min = Number(this.volume.min), max = Number(this.volume.max);
    const text = `${value}%`;
    this.volumeValue.textContent = text; this.volume.setAttribute('aria-valuetext', text);
    this.volume.style.setProperty('--range-value', String((value - min) / (max - min || 1)));
  }
  async load() {
    try { this.sounds = (await request('/api/sounds')).sounds; } catch (error) { this.status.textContent = error.message; return; }
    this.render();
  }
  render() {
    const chosen = this.sounds.some(sound => sound.id === this.notifications.soundID) ? this.notifications.soundID : 'builtin:chime';
    if (chosen !== this.notifications.soundID) this.notifications.soundID = chosen;
    this.select.replaceChildren(...this.sounds.map(sound => { const option = node('option', '', sound.kind === 'user' ? `${sound.name} (uploaded)` : sound.name); option.value = sound.id; return option; }));
    this.select.value = chosen;
    this.manageRow.hidden = !this.owner; this.file.disabled = !this.owner;
    this.list.replaceChildren(...this.sounds.filter(sound => sound.kind === 'user').map(sound => {
      const item = node('li'); item.dataset.soundId = sound.id;
      const remove = iconButton('close', `Delete ${sound.name}`, { danger: true });
      remove.onclick = () => this.remove(sound);
      item.append(node('span', '', `${sound.name} · ${Math.max(1, Math.round(sound.size / 1024))} KB `), remove);
      return item;
    }));
  }
  async add() {
    const file = this.file.files[0]; if (!file) return;
    this.status.textContent = 'Uploading…';
    try { const sound = await upload('/api/sounds', file); this.notifications.soundID = sound.id; this.status.textContent = `Added ${sound.name}.`; await this.load(); }
    catch (error) { this.status.textContent = error.message; }
    finally { this.file.value = ''; }
  }
  async remove(sound) {
    try { await request(`/api/sounds/${encodeURIComponent(sound.id.slice(5))}`, { method: 'DELETE' }); this.status.textContent = `Deleted ${sound.name}.`; await this.load(); }
    catch (error) { this.status.textContent = error.message; }
  }
}

import { html, useState, useRef, useLayoutEffect } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { IconButton } from '../../components/icon-button.js';
import { useAlive } from '../../hooks/use-alive.js';
import { request, upload, rights } from '../../api.js';
import { volumeText, volumeFill, soundLabel, soundSize, uploadedSounds, chosenSound, soundPath, permissionView } from './model.js';

// `notifications` is the shared store the inbox reads (in this browser, per person); the page reads it as it renders and writes through it,
// so the chime that plays is always the one shown here.
export function NotificationsPage({ openToken, notifications }) {
  const alive = useAlive();
  const generation = useRef(0);
  const writing = useRef(false);
  const [, changed] = useState(0);
  const [sounds, setSounds] = useState([]);
  const [manage, setManage] = useState(false);
  const [status, setStatus] = useState('');
  const [permissionError, setPermissionError] = useState('');
  const [busy, setBusy] = useState(false);
  const refresh = () => changed(count => count + 1);
  useLayoutEffect(() => () => { generation.current++; }, []);

  const load = async () => {
    const ticket = ++generation.current;
    let list;
    try { list = (await request('/api/sounds')).sounds; } catch (failure) { if (alive() && ticket === generation.current) setStatus(failure.message); return; }
    if (!alive() || ticket !== generation.current) return;
    const chosen = chosenSound(list, notifications.soundID);
    if (chosen !== notifications.soundID) notifications.soundID = chosen;
    setSounds(list);
  };
  useLayoutEffect(() => {
    if (!openToken) return;
    setStatus(''); setPermissionError('');
    setManage(rights.admin); // known from sign-in; asking /api/users would only answer 403 for everyone else
    refresh();
    load();
  }, [openToken]);

  // One upload or delete at a time: each is followed by a read of the list.
  const write = async (run, done, failed) => {
    if (writing.current) return;
    writing.current = true; setBusy(true);
    try { await run(); if (alive()) { setStatus(done()); await load(); } } catch (failure) { if (alive()) setStatus(failure.message || failed); } finally { writing.current = false; if (alive()) setBusy(false); }
  };
  const add = async event => {
    const input = event.currentTarget, file = input.files[0];
    if (!file) return;
    setStatus('Uploading…');
    let sound;
    await write(async () => { sound = await upload('/api/sounds', file); notifications.soundID = sound.id; }, () => `Added ${sound.name}.`, 'Upload failed');
    input.value = '';
  };
  const remove = sound => write(() => request(soundPath(sound), { method: 'DELETE' }), () => `Deleted ${sound.name}.`, 'Delete failed');
  const requestPermission = async () => {
    try { await notifications.requestPermission(); setPermissionError(''); } catch (failure) { setPermissionError(failure.message); return; }
    refresh();
  };

  const volume = notifications.volume;
  const permission = permissionView(notifications.available, notifications.available ? Notification.permission : 'default');
  return html`
    <p class="settings-intro">New items in your inbox</p>
    <${SettingsGroup} label="Inbox alerts">
      <${SettingsRow} label="Mute inbox sound" desc="A short chime plays for new inbox items, and for chat messages that address you, after you interact with this tab. Turn this on to keep it silent everywhere; each room can also be quieter on its own (Room settings → Notifications)." id="notification-muted" className="settings-row-toggle">
        <input type="checkbox" checked=${notifications.muted} onChange=${event => { notifications.muted = event.currentTarget.checked; refresh(); }} />
      <//>
      <${SettingsRow} label="Browser notifications" desc="Show an operating-system notification for new inbox items while this tab is in the background. Needs a secure context (HTTPS or localhost) and your browser's permission." id="notification-permission">
        <button type="button" class="btn" disabled=${permission.disabled} onClick=${requestPermission}>${permission.label}</button>
      <//>
      <${SettingsRow} label="Show message text in notifications" desc="Put the start of a chat message in the operating-system notification, so it can be read from the lock screen or by anyone near the screen. Off, a notification only says that a message arrived. Inbox items always show their question." id="notification-text" className="settings-row-toggle">
        <input type="checkbox" checked=${notifications.showText} onChange=${event => { notifications.showText = event.currentTarget.checked; refresh(); }} />
      <//>
    <//>
    <${SettingsGroup} label="Inbox sound">
      <div class="settings-row">
        <div class="settings-row-info"><label class="settings-label" for="notification-sound">Inbox sound</label><span class="settings-desc" id="notification-sound-desc">The sound that plays for new inbox items. Your choice is saved to your account and follows you to other devices.</span></div>
        <div class="settings-control"><div class="notification-sound-choice">
          <select class="settings-select" id="notification-sound" aria-describedby="notification-sound-desc" value=${notifications.soundID} onChange=${event => { notifications.soundID = event.currentTarget.value; refresh(); }}>
            ${sounds.map(sound => html`<option value=${sound.id} key=${sound.id}>${soundLabel(sound)}</option>`)}
          </select>
          <button type="button" id="notification-sound-preview" class="btn" onClick=${() => notifications.play()}>Preview</button>
        </div></div>
      </div>
      <div class="settings-row">
        <div class="settings-row-info"><label class="settings-label" for="notification-volume">Volume</label><span class="settings-desc" id="notification-volume-desc">How loud the inbox sound plays, from silent to full volume.</span></div>
        <div class="settings-control"><div class="settings-range">
          <input type="range" id="notification-volume" class="range-input" aria-describedby="notification-volume-desc" min="0" max="100" step="5" value=${String(volume)}
            aria-valuetext=${volumeText(volume)} style=${{ '--range-value': volumeFill(volume) }} onInput=${event => { notifications.volume = event.currentTarget.value; refresh(); }} />
          <output id="notification-volume-value" class="range-value" for="notification-volume" aria-hidden="true">${volumeText(volume)}</output>
        </div></div>
      </div>
      <div class="settings-row" hidden=${!manage}>
        <div class="settings-row-info"><label class="settings-label" for="notification-sound-upload">Custom sounds</label><span class="settings-desc" id="notification-sound-upload-desc">Upload .mp3 or .wav files up to 1 MB each, ten at most. Everyone signed in can choose them; only the owner or an admin can add or delete.</span></div>
        <div class="settings-control"><div class="notification-sound-manage">
          <input type="file" id="notification-sound-upload" aria-describedby="notification-sound-upload-desc" accept=".mp3,.wav,audio/mpeg,audio/wav" disabled=${!manage || busy} onChange=${add} />
          <ul class="notification-sound-list">
            ${uploadedSounds(sounds).map(sound => html`<li data-sound-id=${sound.id} key=${sound.id}><span>${soundSize(sound)}</span><${IconButton} name="trash" label=${`Delete ${sound.name}`} danger disabled=${busy} onClick=${() => remove(sound)} /></li>`)}
          </ul>
        </div></div>
      </div>
    <//>
    <p class="settings-intro" role="status">${status}</p>
    <p id="notification-status" class="settings-intro" role="status">${permissionError || notifications.status}</p>
  `;
}

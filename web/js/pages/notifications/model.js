// Import-free: scripts/test-notifications-model.cjs runs this module in a vm.

export const DEFAULT_SOUND = 'builtin:chime';

export const volumeText = value => `${value}%`;
export const volumeFill = value => String(value / 100);

export const soundLabel = sound => (sound.kind === 'user' ? `${sound.name} (uploaded)` : sound.name);
export const soundSize = sound => `${sound.name} · ${Math.max(1, Math.round(sound.size / 1024))} KB `;
export const uploadedSounds = sounds => sounds.filter(sound => sound.kind === 'user');

// A chosen sound that is no longer on offer (deleted, or from another account's list) falls back to the chime.
export const chosenSound = (sounds, id) => (sounds.some(sound => sound.id === id) ? id : DEFAULT_SOUND);

// The server's id of an uploaded sound is `user:<id>`; its URL takes the part after the colon.
export const soundPath = sound => `/api/sounds/${encodeURIComponent(sound.id.slice(5))}`;

// What the browser-notification button shows and whether it can be pressed, from what the browser offers.
export function permissionView(available, permission) {
  return { disabled: !available || permission !== 'default', label: available && permission === 'granted' ? 'Enabled' : 'Enable browser notifications' };
}

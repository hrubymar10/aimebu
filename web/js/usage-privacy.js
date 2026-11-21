// The privacy toggle of the usages-ports experiment's B: it masks account
// emails and profile names in the Usages view, for screen sharing. The choice
// is kept in this browser, like the group-by one. The masks apply only in B;
// A keeps the plain view whatever the toggle says.
import { signal } from './ui.js';
import { usagesPorts } from './usages-ports.js';

const KEY = 'aimebu2.usages.privacy';
const read = () => { try { return localStorage.getItem(KEY) === 'on'; } catch { return false; } };

export const usagePrivacy = signal(read());

export function toggleUsagePrivacy() {
  const next = !usagePrivacy.value;
  try { localStorage.setItem(KEY, next ? 'on' : 'off'); } catch { /* storage may be denied */ }
  usagePrivacy.value = next;
}

// Masking is on only while the experiment shows its B; the toggle's own choice
// is remembered for when B returns.
export const maskingOn = () => usagesPorts.value === 'b' && usagePrivacy.value;

// A masked name keeps its first character, so rows stay tellable apart.
export function maskName(name) {
  const value = String(name || '');
  return maskingOn() && value ? `${value.slice(0, 1)}•••` : value;
}

// A masked email keeps its first character, the @ and the domain's first
// character; enough to tell two accounts apart, not enough to read.
export function maskEmail(email) {
  const value = String(email || '');
  if (!maskingOn() || !value) return value;
  const at = value.indexOf('@');
  if (at < 0) return `${value.slice(0, 1)}•••`;
  return `${value.slice(0, 1)}•••@${value.slice(at + 1, at + 2)}•••`;
}


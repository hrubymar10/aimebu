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

// maskNames builds a one-pass text mask over the given profile names: a
// single regex alternation of the escaped names, longest first, so an
// overlapping name (m inside main) cannot leave a readable suffix behind
// ("m•••ain"). Every occurrence is replaced with the name's masked form.
// Returns undefined when masking is off or there is nothing to hide, so
// callers can leave the text as it is.
export function maskNames(names) {
  if (!maskingOn()) return undefined;
  const sorted = [...new Set((names || []).map(name => String(name || '')).filter(name => name))].sort((a, b) => b.length - a.length);
  if (!sorted.length) return undefined;
  const pattern = new RegExp(sorted.map(name => name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|'), 'g');
  const masked = new Map(sorted.map(name => [name, maskName(name)]));
  return text => String(text || '').replace(pattern, name => masked.get(name));
}


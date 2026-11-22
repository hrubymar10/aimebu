// The privacy toggle of the Usages view: it masks account emails and profile
// names, for screen sharing. The choice is kept in this browser, like the
// group-by one.
import { signal } from './ui.js';

const KEY = 'aimebu2.usages.privacy';
const read = () => { try { return localStorage.getItem(KEY) === 'on'; } catch { return false; } };

export const usagePrivacy = signal(read());

export function toggleUsagePrivacy() {
  const next = !usagePrivacy.value;
  try { localStorage.setItem(KEY, next ? 'on' : 'off'); } catch { /* storage may be denied */ }
  usagePrivacy.value = next;
}

export const maskingOn = () => usagePrivacy.value;

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
// Only whole names are masked: a name needs a token boundary on both sides
// (the text's edge or a character outside [A-Za-z0-9._-]), so a profile named
// a leaves the words around it alone. Returns undefined when masking is off
// or there is nothing to hide, so callers can leave the text as it is.
export function maskNames(names) {
  if (!maskingOn()) return undefined;
  const sorted = [...new Set((names || []).map(name => String(name || '')).filter(name => name))].sort((a, b) => b.length - a.length);
  if (!sorted.length) return undefined;
  const pattern = new RegExp(`(?<![A-Za-z0-9._-])(?:${sorted.map(name => name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|')})(?![A-Za-z0-9._-])`, 'g');
  const masked = new Map(sorted.map(name => [name, maskName(name)]));
  return text => String(text || '').replace(pattern, name => masked.get(name));
}


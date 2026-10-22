// Whether to offer the Harnesses step on sign-in. A person who can manage harnesses (an owner or an admin) is offered it when no harness
// with a model exists, whatever brought them here (a v1 import, a first sign-in from another browser, every harness removed). The answer
// comes from the server: whether a harness exists, and whether this person skipped the offer, a preference stored with their account.
// Skip is kept until a harness with a model exists; the offer then resets itself, so it can come back if they are all removed later.
// The offer is made once per browser session, so closing it without choosing does not bring it back on every reload.
export const HARNESS_SKIP_PREFERENCE = 'harness_setup_skipped';

// Whether a harness can start an agent: it is enabled and has a model. One test for the sign-in check and for the Settings list, so they
// agree on what "set up" means.
export const launcherReady = (enabled, models) => enabled !== false && (models || []).length > 0;
const OFFERED_KEY = 'aimebu2.harness-offered';

// `configured()` says whether a harness with a model exists; `show()` opens the step; `setSkipped(bool)` stores the preference. Answers
// what it did, for the tests: 'not-allowed', 'unknown', 'configured', 'skipped', 'session' or 'offered'.
export async function offerHarnessSetup({ user, canSetUp, skipped, configured, show, setSkipped, storage = sessionStorage }) {
  if (!canSetUp) return 'not-allowed';
  let ready;
  try { ready = await configured(); } catch { return 'unknown'; }
  if (ready) { if (skipped) await setSkipped(false); return 'configured'; }
  if (skipped) return 'skipped';
  try { if (storage.getItem(OFFERED_KEY) === user) return 'session'; storage.setItem(OFFERED_KEY, user); } catch { /* private browsing: it may be asked again on a reload */ }
  await show();
  return 'offered';
}

// Whether a harness with a model is set up on some approved machine, connected or not: a supervisor that is briefly away does not make the
// person set up what they already have. Only owners and admins can read the launchers. `request` is the app's JSON request function.
export async function anyHarnessConfigured(request) {
  const machines = (await request('/api/machines')).machines || [];
  let unreadable = null;
  for (const machine of machines.filter(item => item.approved && !item.revoked_at)) {
    try {
      const response = await request(`/api/machines/${encodeURIComponent(machine.id)}/launchers`);
      if ((response?.launchers || []).some(row => launcherReady(row.config?.enabled, row.models))) return true;
    } catch (error) { unreadable = error; } // one machine that cannot be read does not hide a harness on another
  }
  if (unreadable) throw unreadable; // nothing found and something unread: not known, so nobody is asked
  return false;
}

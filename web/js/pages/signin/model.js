export const ROLE_WORDS = { owner: 'an owner', operator: 'an operator', viewer: 'a viewer' };
export const INVALID_LEAD = 'This invitation is not valid: it was already used, it expired, or the person who made it can no longer invite.';
export const THROTTLED_LEAD = 'Too many tries. Wait a moment and reload this link.';

export function inviteToken(hash) { return new URLSearchParams(String(hash).replace(/^#/, '')).get('invite') || ''; }

// What the invitation card shows: for a link that cannot be used, only a way back to the sign-in; for a reset, the account it belongs to;
// once redeemed, only the sign-in with the account that was made.
export function cardView(info, redeemedLogin) {
  if (info?.error) return { lead: info.error.status === 429 ? THROTTLED_LEAD : INVALID_LEAD, fields: false, loginLabel: 'Login', loginRequired: true, readOnly: false, loginValue: undefined, submitText: '', switchText: 'Sign in', switchHidden: false };
  const reset = info?.kind === 'reset';
  const view = {
    lead: !info ? '' : reset ? `Choose a new password for ${info.login}.` : `You are invited to ${info.project_name} as ${ROLE_WORDS[info.role] || info.role}. Choose a login and a password to create your account.`,
    fields: true, loginLabel: reset ? 'Account' : 'Login', loginRequired: !reset, readOnly: reset, loginValue: reset ? info.login : undefined,
    submitText: !info ? '' : reset ? 'Set password and sign in' : 'Create account and sign in', switchText: 'I already have an account', switchHidden: reset,
  };
  if (redeemedLogin) return { ...view, loginRequired: false, readOnly: true, loginValue: redeemedLogin, submitText: 'Sign in', switchHidden: true };
  return view;
}

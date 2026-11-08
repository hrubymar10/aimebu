// The telemetry consent popup: shown once to the owner, after the welcome on a new install and on the first sign-in after an update that
// brought error reports. Nothing is reported until it has been answered (the server holds reports back until then). Import-free apart from
// what is passed in, so scripts/test-telemetry-consent.cjs runs it in a vm.

export const CONSENT_TEXT = 'Send telemetry to help fix bugs: errors and performance traces. ' +
  'Never sent: messages, prompts, tokens, file contents. You can change this later in Settings → Telemetry.';

// The per-person identity switch, worded once for this popup and for Settings -> Telemetry.
export const SEND_NAME_LABEL = 'Send my user name instead of a fingerprint';
export const SEND_NAME_DESC = 'Off by default. Your reports then carry your login name (nothing else) in place of the fingerprint, never both.';

export const RESET_DSN_LABEL = 'Reset to default DSN';

export const DSN_PLACEHOLDER = 'https://<key>@<host>/<project id>';

// A DSN is https://<key>@<host>/<project id>; http only for a Sentry on this machine. The server checks it too.
export function dsnProblem(value) {
  if (!value) return '';
  const match = /^(https?):\/\/[^@/\s]+@([^/\s:]+|\[[^\]]+\])(?::\d+)?\/\d+$/.exec(value);
  if (!match) return 'That is not a Sentry DSN (https://<key>@<host>/<project id>).';
  if (match[1] === 'http' && !/^(localhost|127(\.\d+){3}|\[::1\])$/i.test(match[2])) return 'A Sentry DSN must use https (http only for localhost).';
  return '';
}

// The fields of the popup: the toggle starts as the server says (on exactly when a default DSN ships) and the field is always filled with the
// effective DSN (the default unless one was chosen), editable while reports are on, with a button that puts the default back.
export function consentFields(view) {
  return [
    { name: 'enabled', type: 'checkbox', label: 'Send telemetry', value: !!view.enabled },
    // Off greys the field and its reset button out; it does not hide them.
    { name: 'dsn', label: 'Sentry DSN', value: view.dsn || '', placeholder: DSN_PLACEHOLDER, disabledIf: values => !values.enabled,
      reset: { label: RESET_DSN_LABEL, value: view.default_dsn || '' },
      validate: (value, values) => (!values.enabled ? '' : !value ? 'Enter a DSN, or turn telemetry off.' : dsnProblem(value)) },
    // The owner's own account: off by default, greyed out with the rest while reports are off.
    { name: 'send_name', type: 'checkbox', label: SEND_NAME_LABEL, hint: SEND_NAME_DESC, value: !!view.send_name, disabledIf: values => !values.enabled },
  ];
}

// What to save for the dialog's result: Save keeps what was chosen; Not now (or Escape) turns reports off, and either way it is not asked again.
export function consentBody(result) {
  if (!result) return { prompted: true, enabled: false };
  return { prompted: true, enabled: !!result.enabled, ...(result.enabled ? { dsn: result.dsn } : {}) };
}

// Resolves once answered; never throws (a failure means the popup comes back on the next sign-in).
export async function askTelemetryConsent(request, formDialog) {
  try {
    let view = await request('/api/telemetry/settings');
    if (view.prompted) return;
    let problem = '';
    // Save with the name switch ticked (and reports on) also makes this person's reports carry their user name. If that one call fails the popup stays: it
    // is asked again with what was chosen and the reason, so the choice is not lost silently; Not now changes nothing of the kind.
    for (let attempt = 0; attempt < 3; attempt++) {
      const result = await formDialog({ title: 'Telemetry', message: problem ? `${problem} ${CONSENT_TEXT}` : CONSENT_TEXT, fields: consentFields(view), confirm: 'Save', cancel: 'Not now' });
      await request('/api/telemetry/settings', { method: 'PUT', body: consentBody(result) });
      if (!(result && result.enabled && result.send_name)) return;
      try { await request('/api/me/telemetry', { method: 'PUT', body: { send_name: true } }); return; } catch (failure) {
        if (failure && failure.unauthorized) return; // the session is gone: the page is on its way to sign-in, and the popup must not come back over it
        problem = `Could not save your user-name choice (${failure.message || 'the server did not answer'}); try Save again.`;
        view = { ...view, enabled: true, dsn: result.dsn, send_name: true };
      }
    }
    // Three failures: the settings are saved, the user-name choice is not, and the person is told where to make it.
    await formDialog({ title: 'Telemetry', message: 'Your user-name choice could not be saved. You can turn it on in Settings → Telemetry.', confirm: 'OK', notice: true });
  } catch { /* asked again next time */ }
}

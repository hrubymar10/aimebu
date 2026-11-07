// The error-reports consent popup: shown once to the owner, after the welcome on a new install and on the first sign-in after an update that
// brought error reports. Nothing is reported until it has been answered (the server holds reports back until then). Import-free apart from
// what is passed in, so scripts/test-telemetry-consent.cjs runs it in a vm.

export const CONSENT_TEXT = 'Send error reports to help fix bugs: errors and performance traces. ' +
  'Never sent: messages, prompts, tokens, file contents. You can change this later in Settings → General.';

export const DSN_PLACEHOLDER = 'https://<key>@<host>/<project id>';

// A DSN is https://<key>@<host>/<project id>; http only for a Sentry on this machine. The server checks it too.
export function dsnProblem(value) {
  if (!value) return '';
  const match = /^(https?):\/\/[^@/\s]+@([^/\s:]+|\[[^\]]+\])(?::\d+)?\/\d+$/.exec(value);
  if (!match) return 'That is not a Sentry DSN (https://<key>@<host>/<project id>).';
  if (match[1] === 'http' && !/^(localhost|127(\.\d+){3}|\[::1\])$/i.test(match[2])) return 'A Sentry DSN must use https (http only for localhost).';
  return '';
}

// The fields of the popup and of Settings: the toggle starts as the server says (on exactly when a default DSN ships) and the field holds the
// default, editable.
export function consentFields(view) {
  return [
    { name: 'enabled', type: 'checkbox', label: 'Error reports', value: !!view.enabled },
    { name: 'dsn', label: 'Sentry DSN', value: view.dsn || '', placeholder: DSN_PLACEHOLDER, showIf: values => values.enabled,
      validate: (value, values) => (values.enabled && !value ? 'Enter a DSN, or turn error reports off.' : dsnProblem(value)) },
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
    const view = await request('/api/telemetry/settings');
    if (view.prompted) return;
    const result = await formDialog({ title: 'Error reports', message: CONSENT_TEXT, fields: consentFields(view), confirm: 'Save', cancel: 'Not now' });
    await request('/api/telemetry/settings', { method: 'PUT', body: consentBody(result) });
  } catch { /* asked again next time */ }
}

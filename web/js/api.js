let csrf = '';
export let currentUser = '';
// The viewer's login: what @mentions of them are written with (currentUser is the account id).
export let currentLogin = '';
// What the account may do instance-wide: the owner and the admins create projects and edit the global settings; everyone else does not.
// hostHome is the home folder on the machine projects are created on (only the owner and admins are told), where a folder field starts.
export let rights = { owner: false, admin: false, hostHome: '' };
export function setSession(data) { csrf = data.csrf_token; currentUser = data.id || data.user_id; currentLogin = data.login || ''; rights = { owner: !!data.owner, admin: !!data.admin, hostHome: data.host_home || '' }; }
export function clearSession() { csrf = ''; currentUser = ''; currentLogin = ''; rights = { owner: false, admin: false, hostHome: '' }; }
// The flags are read at sign-in; the owner can flag or unflag an admin afterwards, so a place that is about to show or hide something by
// them (opening Settings) reads them again.
export async function refreshRights() {
  const me = await request('/api/me');
  rights = { owner: !!me.owner, admin: !!me.admin, hostHome: me.host_home || '' };
  return rights;
}
export async function request(path, options = {}) {
  const headers = { ...options.headers };
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';
  if (options.method && !['GET', 'HEAD'].includes(options.method)) headers['X-CSRF-Token'] = csrf;
  const response = await fetch(path, { credentials: 'same-origin', ...options, headers, body: options.body === undefined ? undefined : JSON.stringify(options.body) });
  if (!response.ok) {
    let problem = {};
    try { problem = await response.json(); } catch {}
    const error = new Error(problem.message || `Request failed (${response.status})`);
    error.status = response.status;
    error.code = problem.error;
    error.detectDir = problem.detect_dir;
    error.retryAfter = problem.retry_after_sec;
    throw error;
  }
  return response.status === 204 ? null : response.json();
}

// Fetches a file the server builds (a room export): the bytes and the name the server gave it in Content-Disposition.
export async function download(path) {
  const response = await fetch(path, { credentials: 'same-origin' });
  if (!response.ok) {
    let problem = {}; try { problem = await response.json(); } catch {}
    const error = new Error(problem.message || `Download failed (${response.status})`); error.status = response.status; error.code = problem.error; throw error;
  }
  const named = /filename="([^"]+)"/.exec(response.headers.get('Content-Disposition') || '');
  return { blob: await response.blob(), filename: named ? named[1] : '' };
}

export async function upload(path, file, { signal } = {}) {
  const body = new FormData(); body.append('file', file);
  const response = await fetch(path, { method: 'POST', credentials: 'same-origin', headers: { 'X-CSRF-Token': csrf }, body, signal });
  if (!response.ok) {
    let problem = {}; try { problem = await response.json(); } catch {}
    const error = new Error(problem.message || `Upload failed (${response.status})`); error.status = response.status; error.code = problem.error; throw error;
  }
  return response.json();
}

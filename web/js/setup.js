// First-run owner setup: the welcome, then "Enter the setup token", then a "Create owner" form. The token comes from the server's
// output or from the file it names (the screen says where); a /setup?token=… link, which the server also prints, fills it in and
// skips the token screen. The server checks the token, creates the owner and signs them in.
import { request, setSession } from './api.js';
import { loadPreferences, savePreference } from './preferences.js';
import { THEMES } from './pages/general/model.js';
import { showWelcome } from './welcome.js';

const MIN_PASSWORD = 12;

// Before anyone has signed in there is no stored theme: the setup pages follow the operating system's light or dark setting.
export function systemTheme() { return matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'; }
function applyTheme(theme) { if (theme === 'dark') delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = theme; }

// What a failed token or setup call says: the server's sentence, and for the rate limit how long to wait.
const problemText = problem => problem.status === 429 && problem.retryAfter ? `Too many attempts. Try again in ${problem.retryAfter} seconds.` : problem.message;

// The token step. A token from the link is checked at once and, when it is right, the step is skipped; otherwise (no link, or a wrong
// one) the person types or pastes it. Returns the token the server accepted.
async function askToken({ tokenView, tokenForm, linked, send }) {
  const input = tokenForm.elements.token, error = tokenView.querySelector('.error'), hint = tokenView.querySelector('#setup-token-hint');
  let path = '';
  try { path = (await send('/api/setup/status')).token_path || ''; } catch { /* the hint then names only the server output */ }
  hint.replaceChildren('Find it in the output of the server');
  if (path) { const code = document.createElement('code'); code.textContent = path; hint.append(', or in the file ', code); }
  hint.append('.');
  const check = value => send('/api/setup/token', { method: 'POST', body: { token: value } });
  if (linked) {
    try { await check(linked); return linked; } catch (problem) { input.value = linked; error.textContent = problemText(problem); }
  }
  tokenView.hidden = false;
  input.focus();
  return new Promise(resolve => {
    tokenForm.onsubmit = async event => {
      event.preventDefault();
      const submit = tokenForm.querySelector('button[type=submit]');
      if (submit.disabled) return;
      error.textContent = '';
      const value = input.value.trim();
      if (!value) { error.textContent = 'Enter the setup token.'; return; }
      submit.disabled = true;
      try { await check(value); tokenView.hidden = true; resolve(value); }
      catch (problem) { error.textContent = problemText(problem); submit.disabled = false; input.select(); }
    };
  });
}

export async function ownerSetup({ view, form, error, tokenView, tokenForm, themeView, welcome = showWelcome, send = request } = {}) {
  applyTheme(systemTheme());
  await welcome();
  const token = await askToken({ tokenView, tokenForm, linked: new URLSearchParams(location.search).get('token') || '', send });
  view.hidden = false;
  const login = form.elements.login;
  login.focus();
  return new Promise(resolve => {
    form.onsubmit = async event => {
      event.preventDefault();
      if (form.querySelector('button[type=submit]').disabled) return;
      error.textContent = '';
      const name = login.value.trim().toLowerCase(), password = form.elements.password.value;
      if (!/^[a-z][a-z0-9_-]{2,31}$/.test(name)) { error.textContent = 'Choose a login of 3 to 32 lowercase letters, digits, - or _, starting with a letter.'; return; }
      if (password.length < MIN_PASSWORD) { error.textContent = `Use a password of at least ${MIN_PASSWORD} characters.`; return; }
      if (password !== form.elements.confirm.value) { error.textContent = 'The two passwords do not match.'; return; }
      const submit = form.querySelector('button[type=submit]'); submit.disabled = true;
      try {
        const session = await send('/api/setup', { method: 'POST', body: { token, login: name, password } });
        // The welcome was already shown above, so the app must not show it a second time.
        setSession(session);
        await send('/api/welcome/seen', { method: 'POST', body: {} }).catch(() => {});
        history.replaceState(null, '', '/');
        view.hidden = true;
        await loadPreferences(session.user_id);
        await chooseTheme(themeView, session.user_id, send);
        resolve({ ...session, first_run: false });
      } catch (problem) {
        error.textContent = problemText(problem);
        submit.disabled = false;
      }
    };
  });
}

// The theme step: a tile per theme drawn in its own colours, applied as soon as it is picked and saved to the profile.
export function chooseTheme(view, userID, send = request) {
  const grid = view.querySelector('.theme-grid');
  const themes = THEMES.map(([value, label]) => ({ value, label }));
  let current = document.documentElement.dataset.theme || 'dark';
  const tiles = new Map();
  const select = value => {
    current = value; applyTheme(value);
    try { localStorage.setItem(`aimebu2.ui.theme.${userID}`, value); } catch { /* the saved copy is what counts */ }
    savePreference('theme', value, { send });
    for (const [name, tile] of tiles) { tile.setAttribute('aria-checked', String(name === value)); tile.tabIndex = name === value ? 0 : -1; }
  };
  grid.replaceChildren();
  for (const theme of themes) {
    const tile = document.createElement('button'); tile.type = 'button'; tile.className = 'theme-tile'; tile.dataset.theme = theme.value; tile.dataset.choice = theme.value;
    tile.setAttribute('role', 'radio');
    tile.innerHTML = '<span class="theme-preview" aria-hidden="true"><span class="theme-preview-bar"></span><span class="theme-preview-line"></span><span class="theme-preview-line short"></span><span class="theme-preview-accent"></span></span>';
    const label = document.createElement('span'); label.className = 'theme-label'; label.textContent = theme.label; tile.append(label);
    tile.onclick = () => select(theme.value);
    tile.onkeydown = event => {
      const index = themes.findIndex(item => item.value === current);
      const step = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 }[event.key];
      if (!step) return;
      event.preventDefault(); const next = themes[(index + step + themes.length) % themes.length].value; select(next); tiles.get(next).focus();
    };
    tiles.set(theme.value, tile); grid.append(tile);
  }
  select(current);
  view.hidden = false; tiles.get(current).focus();
  return new Promise(resolve => { view.querySelector('.theme-continue').onclick = () => { view.hidden = true; resolve(current); }; });
}

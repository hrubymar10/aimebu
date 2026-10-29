export const MIN_PASSWORD = 12;
const LOGIN = /^[a-z][a-z0-9_-]{2,31}$/;

// What a failed token or setup call says: the server's sentence, and for the rate limit how long to wait.
export const problemText = problem => problem.status === 429 && problem.retryAfter ? `Too many attempts. Try again in ${problem.retryAfter} seconds.` : problem.message;

// The owner form's checks, in the order the person meets them. The login is lowercased; the sentences are the ones the form shows.
export function checkOwner(login, password, confirm) {
  const name = String(login).trim().toLowerCase();
  if (!LOGIN.test(name)) return { error: 'Choose a login of 3 to 32 lowercase letters, digits, - or _, starting with a letter.' };
  if (password.length < MIN_PASSWORD) return { error: `Use a password of at least ${MIN_PASSWORD} characters.` };
  if (password !== confirm) return { error: 'The two passwords do not match.' };
  return { name, error: '' };
}

// The theme an arrow key moves to from `current`, wrapping round the list; null for any other key.
export function nextTheme(themes, current, key) {
  const step = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 }[key];
  if (!step) return null;
  const index = themes.findIndex(([value]) => value === current);
  return themes[(index + step + themes.length) % themes.length][0];
}

// A "Workspace folder (absolute path)" field can start at the home folder of the machine the project is created on, so the person types the rest
// of the path instead of the whole of it. It never fills by itself: the person asks (the home button, or accepting the ghost suggestion, in folder-field.js),
// and a fill only goes into an empty field, never over what was typed. `home` is what the server said (empty for someone who may not create projects, or
// when it does not know: the field then has nothing to offer).
export function folderStart(home) {
  if (typeof home !== 'string' || !home.startsWith('/')) return '';
  return home.endsWith('/') ? home : `${home}/`;
}

// What the field says after the person typed over the start: a path that begins with "/" is a whole path, so the start steps aside
// ("/home/ana/" + "/srv/work" is "/srv/work", not "/home/ana//srv/work"); anything else is the rest of the path and stays after it.
// null when the text is not the start followed by something that begins with "/".
export function pathTypedOverStart(value, start) {
  return start && typeof value === 'string' && value.startsWith(`${start}/`) ? value.slice(start.length) : null;
}

// Puts `start` in an empty field with the caret after it, and says so to the page. Answers whether it filled (a field with text in it is left alone).
// `arm(start)` is told first, so the field's own input event does not take the start for typed text.
export function fillFolderStart(input, start, arm) {
  if (!start || input.value !== '') return false;
  arm?.(start);
  input.value = start;
  input.setSelectionRange?.(start.length, start.length);
  input.dispatchEvent(new Event('input', { bubbles: true }));
  return true;
}

// Watches a field for a whole path typed over a start that was put there: the start steps aside, and a relative rest stays after it. Returns arm(start),
// which says what start is in the field now ('' for none).
export function watchTypedOverStart(input) {
  let start = '';
  input.addEventListener('input', () => {
    if (!start) return;
    const whole = pathTypedOverStart(input.value, start);
    if (whole !== null) { start = ''; input.value = whole; input.setSelectionRange?.(whole.length, whole.length); input.dispatchEvent(new Event('input', { bubbles: true })); }
    else if (!input.value.startsWith(start)) start = ''; // the start was edited: it is the person's text now
  });
  return value => { start = value; };
}

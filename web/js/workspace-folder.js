// A "Workspace folder (absolute path)" field starts at the home folder of the machine the project is created on, so the person types the rest
// of the path instead of the whole of it. It only fills an empty field when it gets focus, and never replaces what was typed. `home` is
// what the server said (empty for someone who may not create projects, or when it does not know: the field then stays empty).
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

export function prefillFolderOnFocus(input, getHome) {
  let prefill = '';
  input.addEventListener('focus', () => {
    if (input.value !== '') return;
    const start = folderStart(typeof getHome === 'function' ? getHome() : getHome);
    if (!start) return;
    prefill = start;
    input.value = start;
    input.setSelectionRange?.(start.length, start.length);
    input.dispatchEvent(new Event('input', { bubbles: true }));
  });
  // Typing a whole path after the start replaces the start; the check runs while the start is still what the field begins with.
  input.addEventListener('input', () => {
    if (!prefill) return;
    const whole = pathTypedOverStart(input.value, prefill);
    if (whole !== null) { prefill = ''; input.value = whole; input.setSelectionRange?.(whole.length, whole.length); input.dispatchEvent(new Event('input', { bubbles: true })); }
    else if (!input.value.startsWith(prefill)) prefill = ''; // the start was edited: it is the person's text now
  });
}

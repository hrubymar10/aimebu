// A "Workspace folder (absolute path)" field starts at the home folder of the machine the project is created on, so the person types the rest
// of the path instead of the whole of it. It only fills an empty field when it gets focus, and never replaces what was typed. `home` is
// what the server said (empty for someone who may not create projects, or when it does not know: the field then stays empty).
export function folderStart(home) {
  if (typeof home !== 'string' || !home.startsWith('/')) return '';
  return home.endsWith('/') ? home : `${home}/`;
}

export function prefillFolderOnFocus(input, getHome) {
  input.addEventListener('focus', () => {
    if (input.value !== '') return;
    const start = folderStart(typeof getHome === 'function' ? getHome() : getHome);
    if (!start) return;
    input.value = start;
    input.setSelectionRange?.(start.length, start.length);
    input.dispatchEvent(new Event('input', { bubbles: true }));
  });
}

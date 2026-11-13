// The Workspace folder field, one component for every place that has one (New project, project settings, the first-run project step, the import wizard's Projects page). The field
// is empty until the person asks for their home folder: a house button sits at the left edge of the field (the text is inset to make room, only while it shows),
// and a click fills the home folder (a trailing slash, the caret after it, the focus kept). The home button stays hidden when no home is known.
// While the suggestion list is open, Enter, Tab and `/` complete a suggestion only when one is highlighted (arrow keys or the pointer over it); with none highlighted
// they do what they always do (a `/` is typed, Tab moves the focus, Enter submits), and Escape closes the list and leaves the text as it is.
import { request, rights } from './api.js';
import { fillFolderStart, folderStart, watchTypedOverStart } from './workspace-folder.js';

const SIDE = 6; // the button's distance from the field's edge, in px

export function attachFolderField(input, getHome = () => rights.hostHome, getMachine = () => '') {
  if (!input || input.dataset.folderField) return;
  input.dataset.folderField = 'true';
  const host = input.parentElement;
  const arm = watchTypedOverStart(input);
  host.classList.add('folder-field');
  const button = document.createElement('button');
  button.type = 'button'; button.className = 'folder-home-btn'; button.title = 'Use my home folder'; button.setAttribute('aria-label', 'Use my home folder'); button.hidden = true;
  const icon = document.createElement('span'); icon.className = 'icon-mask icon-mask-house'; icon.setAttribute('aria-hidden', 'true'); button.append(icon);
  input.after(button);
  input.setAttribute('autocomplete', 'off');
  input.setAttribute('aria-autocomplete', 'list');
  input.setAttribute('aria-expanded', 'false');
  const suggestions = document.createElement('div');
  suggestions.className = 'folder-suggestions';
  suggestions.setAttribute('role', 'listbox');
  suggestions.id = `folder-suggestions-${Math.random().toString(36).slice(2)}`;
  suggestions.hidden = true;
  input.setAttribute('aria-controls', suggestions.id);
  input.after(suggestions);
  let names = [], selected = -1, serial = 0, timer, hovered = false; // hovered: the highlight is the pointer's, so it goes when the pointer leaves the list
  const positionSuggestions = () => {
    if (suggestions.hidden) return;
    const field = input.getBoundingClientRect(), edge = 8, gap = 4;
    const below = Math.max(0, window.innerHeight - edge - field.bottom - gap);
    const above = Math.max(0, field.top - edge - gap);
    const flip = below < Math.min(420, suggestions.scrollHeight) && above > below;
    suggestions.style.maxHeight = `${Math.min(420, flip ? above : below)}px`;
    suggestions.style.width = `${Math.min(field.width, window.innerWidth - edge * 2)}px`;
    suggestions.style.left = `${Math.max(edge, Math.min(field.left, window.innerWidth - edge - suggestions.offsetWidth))}px`;
    suggestions.style.top = `${flip ? field.top - gap - suggestions.offsetHeight : field.bottom + gap}px`;
  };
  const close = () => {
    suggestions.hidden = true;
    document.removeEventListener('scroll', positionSuggestions, true);
    window.removeEventListener('resize', positionSuggestions);
    input.setAttribute('aria-expanded', 'false'); suggestions.replaceChildren(); names = []; selected = -1; hovered = false; input.removeAttribute('aria-activedescendant');
  };
  const choose = index => {
    if (index < 0 || index >= names.length) return false;
    const slash = input.value.lastIndexOf('/');
    input.value = input.value.slice(0, slash + 1) + names[index] + '/';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.setSelectionRange(input.value.length, input.value.length);
    input.focus();
    return true;
  };
  const highlight = index => {
    selected = index; hovered = false;
    for (const [i, option] of [...suggestions.children].entries()) option.setAttribute('aria-selected', String(i === index));
    if (index >= 0) {
      input.setAttribute('aria-activedescendant', `${suggestions.id}-${index}`);
      suggestions.children[index].scrollIntoView({ block: 'nearest' });
    } else input.removeAttribute('aria-activedescendant');
  };
  const search = async () => {
    const value = input.value, slash = value.lastIndexOf('/'), current = ++serial;
    close();
    if (!value.startsWith('/') || slash < 0 || !input.isConnected || input.disabled) return;
    const parent = value.slice(0, slash + 1), prefix = value.slice(slash + 1);
    const machine = typeof getMachine === 'function' ? getMachine() : getMachine;
    const params = new URLSearchParams({ path: parent, prefix });
    if (machine) params.set('machine_id', machine);
    try {
      const result = await request(`/api/machines/directories?${params}`);
      if (current !== serial || document.activeElement !== input || input.value !== value) return;
      names = result.directories || [];
      for (const [index, name] of names.entries()) {
        const option = document.createElement('div');
        option.id = `${suggestions.id}-${index}`;
        option.setAttribute('role', 'option');
        option.setAttribute('aria-selected', 'false');
        option.textContent = name + '/';
        option.addEventListener('pointerdown', event => { event.preventDefault(); choose(index); });
        option.addEventListener('pointerover', () => { if (selected !== index) { highlight(index); hovered = true; } });
        suggestions.append(option);
      }
      suggestions.hidden = names.length === 0;
      input.setAttribute('aria-expanded', String(names.length > 0));
      if (names.length) {
        positionSuggestions();
        document.addEventListener('scroll', positionSuggestions, true);
        window.addEventListener('resize', positionSuggestions);
      }
    } catch { close(); }
  };
  suggestions.addEventListener('pointerleave', () => { if (hovered) highlight(-1); });
  const schedule = () => { ++serial; clearTimeout(timer); close(); timer = setTimeout(search, 100); };
  input.addEventListener('input', schedule);
  input.addEventListener('focus', schedule);
  input.addEventListener('blur', () => { ++serial; clearTimeout(timer); close(); });
  input.addEventListener('keydown', event => {
    if (suggestions.hidden) return;
    if (event.key === 'Escape') { event.preventDefault(); ++serial; clearTimeout(timer); close(); return; }
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      highlight(selected < 0 ? (event.key === 'ArrowDown' ? 0 : names.length - 1) :
        (selected + (event.key === 'ArrowDown' ? 1 : -1) + names.length) % names.length);
    } else if ((event.key === 'Enter' || event.key === 'Tab' || event.key === '/') && selected >= 0 && !event.shiftKey && !event.ctrlKey && !event.metaKey && !event.altKey) {
      event.preventDefault(); choose(selected);
    }
  });
  const start = () => folderStart(typeof getHome === 'function' ? getHome() : getHome);
  const fill = () => { fillFolderStart(input, start(), arm); input.focus(); sync(); };
  const place = () => {
    button.style.top = `${input.offsetTop + (input.offsetHeight - button.offsetHeight) / 2}px`;
    button.style.left = `${input.offsetLeft + SIDE}px`;
  };
  const sync = () => {
    const offer = start() !== '' && input.value === '';
    button.hidden = !offer;
    if (offer) host.dataset.homeButton = 'true'; else delete host.dataset.homeButton;
    place();
    positionSuggestions();
  };
  button.addEventListener('click', fill);
  input.addEventListener('input', sync);
  input.addEventListener('focus', sync);
  input.form?.addEventListener('reset', () => setTimeout(sync));
  // The field may be drawn before the person is known, and a dialog's field has no size until it opens: both are read again when its size changes.
  if (typeof ResizeObserver === 'function') new ResizeObserver(sync).observe(input);
  sync();
}

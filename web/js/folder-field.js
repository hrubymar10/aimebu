// The Workspace folder field, one component for every place that has one (New project, the first-run project step, the import wizard's Projects page). The field
// is empty until the person asks for their home folder: a house button sits at the left edge of the field (the text is inset to make room, only while it shows),
// and a click fills the home folder (a trailing slash, the caret after it, the focus kept). Nothing shows when the home folder is not known.
import { rights } from './api.js';
import { fillFolderStart, folderStart, watchTypedOverStart } from './workspace-folder.js';

const SIDE = 6; // the button's distance from the field's edge, in px

export function attachFolderField(input, getHome = () => rights.hostHome) {
  if (!input || input.dataset.folderField) return;
  input.dataset.folderField = 'true';
  const host = input.parentElement;
  const arm = watchTypedOverStart(input);
  host.classList.add('folder-field');
  const button = document.createElement('button');
  button.type = 'button'; button.className = 'folder-home-btn'; button.title = 'Use my home folder'; button.setAttribute('aria-label', 'Use my home folder'); button.hidden = true;
  const icon = document.createElement('span'); icon.className = 'icon-mask icon-mask-house'; icon.setAttribute('aria-hidden', 'true'); button.append(icon);
  input.after(button);
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
  };
  button.addEventListener('click', fill);
  input.addEventListener('input', sync);
  input.addEventListener('focus', sync);
  input.form?.addEventListener('reset', () => setTimeout(sync));
  // The field may be drawn before the person is known, and a dialog's field has no size until it opens: both are read again when its size changes.
  if (typeof ResizeObserver === 'function') new ResizeObserver(sync).observe(input);
  sync();
}

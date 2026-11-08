// The Workspace folder field, one component for every place that has one (New project, the first-run project step, the import wizard's Projects page). The field
// is empty until the person asks for their home folder; three ways to ask are being tried on the testing pill ("homefolder"):
//   A  a house button at the right edge of the field,
//   B  the same button at the left edge (the text is inset to make room),
//   C  no button: the home folder shows in the field as a ghost, and Tab or → accepts it.
// A and B show only while the field is empty, and a click fills the home folder (a trailing slash, the caret after it, the focus kept). Nothing shows when the
// home folder is not known. Which way is on is activeVariant('homefolder'); the choice can change while the page is open.
import { rights } from './api.js';
import { activeVariant, defineExperiment } from './ui-variant.js';
import { fillFolderStart, folderStart, watchTypedOverStart } from './workspace-folder.js';

defineExperiment('homefolder', 'Home folder button', ['Icon at the right', 'Icon at the left', 'Ghost suggestion']);

const SIDE = 6; // the button's distance from the field's edge, in px

export function attachFolderField(input, getHome = () => rights.hostHome) {
  if (!input || input.dataset.folderField) return;
  input.dataset.folderField = 'true';
  const host = input.parentElement, placeholder = input.getAttribute('placeholder');
  const arm = watchTypedOverStart(input);
  host.classList.add('folder-field');
  const button = document.createElement('button');
  button.type = 'button'; button.className = 'folder-home-btn'; button.title = 'Use my home folder'; button.setAttribute('aria-label', 'Use my home folder'); button.hidden = true;
  const icon = document.createElement('span'); icon.className = 'icon-mask icon-mask-house'; icon.setAttribute('aria-hidden', 'true'); button.append(icon);
  const ghost = document.createElement('span'); ghost.className = 'folder-ghost'; ghost.setAttribute('aria-hidden', 'true'); ghost.hidden = true;
  input.after(button, ghost);
  const start = () => folderStart(typeof getHome === 'function' ? getHome() : getHome);
  const fill = () => { fillFolderStart(input, start(), arm); input.focus(); sync(); };
  const place = () => {
    const top = input.offsetTop, height = input.offsetHeight, left = input.offsetLeft, width = input.offsetWidth;
    button.style.top = `${top + (height - button.offsetHeight) / 2}px`;
    if (host.dataset.homeButton === 'b') { button.style.left = `${left + SIDE}px`; button.style.right = ''; }
    else { button.style.right = `${host.clientWidth - left - width + SIDE}px`; button.style.left = ''; }
    const cs = getComputedStyle(input);
    ghost.style.cssText = `top:${top}px;left:${left}px;height:${height}px;max-width:${width}px;padding:0 ${cs.paddingRight} 0 ${cs.paddingLeft};font:${cs.font};line-height:${height}px`;
  };
  const sync = () => {
    const variant = activeVariant('homefolder'), text = start(), offer = text !== '' && input.value === '';
    const showButton = offer && variant !== 'c', showGhost = offer && variant === 'c';
    button.hidden = !showButton; ghost.hidden = !showGhost; ghost.textContent = text;
    if (showButton) host.dataset.homeButton = variant; else delete host.dataset.homeButton;
    // A ghost and the placeholder would overlap, so while the ghost is offered the placeholder steps aside.
    if (placeholder !== null) input.setAttribute('placeholder', showGhost ? '' : placeholder);
    place();
  };
  button.addEventListener('click', fill);
  input.addEventListener('input', sync);
  input.addEventListener('focus', sync);
  input.addEventListener('keydown', event => {
    if (event.defaultPrevented || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    if (activeVariant('homefolder') !== 'c' || input.value !== '' || start() === '') return;
    if (event.key === 'Tab' || event.key === 'ArrowRight') { event.preventDefault(); fill(); }
  });
  input.form?.addEventListener('reset', () => setTimeout(sync));
  document.addEventListener('ui-variant-change', sync);
  // The field may be drawn before the person is known, and a dialog's field has no size until it opens: both are read again when its size changes.
  if (typeof ResizeObserver === 'function') new ResizeObserver(sync).observe(input);
  sync();
}

// The usage popover of one message: opened from the chart icon next to the message number, closed by Escape or a click elsewhere, and
// it hands focus back to the icon. The wording is in message-usage-format.js. Everything is written with textContent.
import { usageRows, usageNotes, inputTooltip } from './message-usage-format.js';
import { focusPopover, returnFocus } from './popover-focus.js';

let open = null; // { popover, button, off }

export function closeMessageUsage() {
  if (!open) return;
  const { popover, button, off } = open;
  open = null;
  off();
  returnFocus(popover, button);
  popover.remove();
  button.setAttribute('aria-expanded', 'false');
}

function node(tag, className = '', text = '') { const item = document.createElement(tag); if (className) item.className = className; if (text) item.textContent = text; return item; }

export function toggleMessageUsage(button, usage) {
  const same = open?.button === button;
  closeMessageUsage();
  if (same) return;
  const popover = node('div', 'usage-popover'); popover.setAttribute('role', 'dialog'); popover.setAttribute('aria-label', 'Usage of this message');
  const grid = node('dl', 'usage-popover-rows');
  for (const [label, value] of usageRows(usage)) {
    const detail = node('dd', '', value);
    if (label === 'Input') detail.title = inputTooltip(usage);
    grid.append(node('dt', '', label), detail);
  }
  popover.append(grid);
  for (const note of usageNotes(usage)) popover.append(node('p', 'usage-popover-note', note));
  document.body.append(popover);
  button.setAttribute('aria-expanded', 'true');
  // Below the icon, kept inside the window.
  const at = button.getBoundingClientRect(), width = popover.offsetWidth, height = popover.offsetHeight;
  const left = Math.max(8, Math.min(at.left, window.innerWidth - width - 8));
  const below = at.bottom + 6 + height <= window.innerHeight - 8;
  popover.style.left = `${left + window.scrollX}px`;
  popover.style.top = `${(below ? at.bottom + 6 : Math.max(8, at.top - height - 6)) + window.scrollY}px`;
  const onKey = event => { if (event.key === 'Escape') { event.preventDefault(); closeMessageUsage(); } };
  const onPointer = event => { if (!popover.contains(event.target) && !button.contains(event.target)) closeMessageUsage(); };
  document.addEventListener('keydown', onKey, true);
  document.addEventListener('pointerdown', onPointer, true);
  open = { popover, button, off: () => { document.removeEventListener('keydown', onKey, true); document.removeEventListener('pointerdown', onPointer, true); } };
  focusPopover(popover);
}

// A top-bar popover that opens must take keyboard focus, or a keyboard user has to tab through the whole page to reach it. Focus goes to the
// panel itself (no focus ring, like a modal dialog in dialog-focus.js); Tab then reaches its first control. Closing hands focus back to the
// button that opened it, but only when focus was still inside the panel, so a click elsewhere keeps the focus where the user put it.
export function focusPopover(popover) {
  if (!popover.hasAttribute('tabindex')) popover.tabIndex = -1;
  popover.focus({ preventScroll: true });
}

export function returnFocus(popover, toggle) {
  if (popover.contains(document.activeElement)) toggle.focus({ preventScroll: true });
}

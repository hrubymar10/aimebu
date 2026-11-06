// A click on the backdrop of a modal dialog closes it, the way Escape does. One document-level listener serves every <dialog>.
//
// Only a real backdrop click counts: the pointer went down and the click came up on the <dialog> element itself, with both points outside the
// dialog's content (its box, or its `.settings-pane` / `.open-questions-modal-pane` child when the dialog element is a full-screen overlay). A
// selection dragged out of a field ends outside the dialog too, but it began inside, so it does not close anything.
//
// "Closes it the way Escape does": the dialog gets the same cancelable `cancel` event Escape sends, and is closed only if nothing stops it, so a
// dialog that guards its unsaved changes or asks before closing (the import dialogs, the confirm dialogs) does so for a backdrop click too.
const PANES = ':scope > .settings-pane, :scope > .open-questions-modal-pane';

function content(dialog) { return (dialog.querySelector(PANES) || dialog).getBoundingClientRect(); }

function outside(dialog, event) {
  const box = content(dialog);
  return event.clientX < box.left || event.clientX > box.right || event.clientY < box.top || event.clientY > box.bottom;
}

let pressed = null;

document.addEventListener('pointerdown', event => {
  const dialog = event.target;
  pressed = event.isTrusted && dialog instanceof HTMLDialogElement && dialog.open && event.button === 0 && outside(dialog, event) ? dialog : null;
}, true);

// A press that became a scroll or a pinch is no press on the backdrop: nothing is remembered of it.
document.addEventListener('pointercancel', () => { pressed = null; }, true);

document.addEventListener('click', event => {
  const dialog = event.target, began = pressed;
  pressed = null;
  if (!event.isTrusted || !(dialog instanceof HTMLDialogElement) || dialog !== began || !dialog.open || !outside(dialog, event)) return;
  // A dialog that is not modal has no backdrop to click.
  if (!dialog.matches(':modal')) return;
  const cancel = new Event('cancel', { cancelable: true });
  dialog.dispatchEvent(cancel);
  if (!cancel.defaultPrevented && dialog.open) dialog.close();
});

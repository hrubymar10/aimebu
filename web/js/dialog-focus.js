// A modal dialog opens with focus on its first focusable control, which is usually a close button, and that shows a focus ring the moment
// the dialog appears (in Settings, the first tab). When focus lands on an icon button or a Settings tab this way, put it on the dialog itself instead (it has no ring); Tab still
// goes to the first control, and a dialog that focuses a field of its own (an autofocus input) is left alone.
const showModal = HTMLDialogElement.prototype.showModal;
HTMLDialogElement.prototype.showModal = function showModalWithoutRing(...args) {
  const result = showModal.apply(this, args);
  const active = document.activeElement;
  if (active && active !== this && this.contains(active) && active.matches('button.btn-icon, button.settings-nav-item')) {
    if (!this.hasAttribute('tabindex')) this.tabIndex = -1;
    this.focus({ preventScroll: true });
  }
  return result;
};

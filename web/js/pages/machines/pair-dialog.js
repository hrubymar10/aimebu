import { html, useLayoutEffect, useRef } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';

export function PairDialog({ store, dialog }) {
  const open = store.pairOpen.value, name = useRef(null);
  useLayoutEffect(() => {
    // The event comes a task after the close: a reopen in between has already shown the dialog again.
    const closed = () => { if (!dialog.open) store.pairOpen.value = false; };
    dialog.addEventListener('close', closed);
    return () => dialog.removeEventListener('close', closed);
  }, [dialog, store]);
  useLayoutEffect(() => {
    if (open && !dialog.open) { dialog.showModal(); name.current?.focus(); }
    else if (!open && dialog.open) dialog.close();
  }, [open, dialog]);
  const pairing = store.pairing.value;
  return html`<h2 id="machine-pair-title">Add machine</h2>
    <form id="machine-pair-form" hidden=${!!pairing} onSubmit=${event => { event.preventDefault(); store.createPairing(); }}>
      <label>Machine name<input class="settings-text-input" name="name" maxlength="100" required placeholder="Workstation" ref=${name} value=${store.name.value} onInput=${event => { store.name.value = event.currentTarget.value; }} /></label>
      <div class="dialog-actions"><button id="machine-pair-cancel" type="button" class="btn" onClick=${() => store.closePairing()}>Cancel</button><button type="submit" class="btn btn-primary" disabled=${store.pairBusy.value}>Create pairing command</button></div>
    </form>
    <div id="machine-pair-result" hidden=${!pairing}>
      <p id="machine-pair-warning" class="error" role="alert" hidden=${!pairing?.warning}>${pairing?.warning || ''}</p>
      <p>Run this command on the other machine before the token expires:</p>
      <code id="machine-pair-command">${pairing?.command || ''}</code>
      <p id="machine-pair-expiry">${pairing?.expires || ''}</p>
      <${IconButton} id="machine-pair-copy" name=${store.copied.value ? 'check' : 'copy'} label=${store.copied.value ? 'Copied' : 'Copy command'} onClick=${() => store.copyCommand()} />
      <button id="machine-pair-done" type="button" class="btn" onClick=${() => store.closePairing()}>Close</button>
    </div>
    <p id="machine-pair-error" class="error" role="alert">${store.pairError.value}</p>`;
}

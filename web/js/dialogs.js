// In-app dialogs for questions, confirmations and notices, one shared component in place of the browser's prompt(), confirm() and
// alert(). Each function returns a promise: confirm -> true/false, prompt -> the text or null, form -> the values or null, notice -> void.
// One dialog is open at a time; a second request waits for the first. Escape or Cancel answers "no".
// This module keeps the queue, the session's generation and the <dialog> element (made on the first call, so a page without app.js can use
// it); the dialog's body is the Preact AppDialog (pages/app-dialog/), drawn with a synchronous render() so a call's DOM is there at once.
import { html, render } from './ui.js';
import { AppDialog } from './pages/app-dialog/app-dialog.js';
import { accepted, declined, fieldStates, firstProblem, focusTarget, readValues, startValues } from './pages/app-dialog/model.js';

let dialog = null, queue = Promise.resolve(), generation = 0, cancelActive = null, calls = 0;

function host() {
  if (dialog) return dialog;
  dialog = document.createElement('dialog'); dialog.id = 'app-dialog'; dialog.className = 'app-dialog dialog-card dialog-card-primary';
  dialog.setAttribute('aria-labelledby', 'app-dialog-title');
  document.body.append(dialog);
  return dialog;
}

function run({ title, message, fields = [], confirm = 'OK', cancel = 'Cancel', danger = false, notice = false }) {
  const node = host();
  const call = { seq: ++calls, title, message, fields, confirm, cancel, danger, notice };
  const input = index => node.querySelector(`#app-dialog-field-${index}`);
  const values = () => readValues(fields, input);
  let error = '', finish = () => {};
  const draw = (now = values()) => render(html`<${AppDialog} call=${call} states=${fieldStates(fields, now)} error=${error} onInput=${() => draw()} onReset=${reset}
    onSubmit=${submit} onCancel=${() => finish(declined(fields))} />`, node);
  // A field that depends on another (showIf, disabledIf) follows it as it changes; the reset button puts its value back.
  function reset(index) { const field = input(index); field.value = fields[index].reset.value; field.dispatchEvent(new Event('input')); field.focus(); }
  function submit(event) {
    event.preventDefault();
    const now = values(), problem = firstProblem(fields, now);
    if (problem) { error = problem.message; draw(now); input(problem.index)?.focus(); return; }
    finish(accepted(fields, now));
  }
  draw(startValues(fields)); // the first draw has no inputs to read yet
  const opener = document.activeElement;
  return new Promise(resolve => {
    finish = result => { finish = () => {}; node.oncancel = null; cancelActive = null; if (node.open) node.close(); opener?.focus?.(); resolve(result); };
    cancelActive = () => finish(declined(fields));
    node.oncancel = event => { event.preventDefault(); finish(declined(fields)); };
    node.showModal();
    const target = focusTarget(call);
    (target === 'cancel' ? node.querySelector('.app-dialog-cancel') : target === 'field' ? input(0) : node.querySelector('.app-dialog-ok')).focus();
  });
}

// The session ended (sign-out, or a 401 on the way to the sign-in page): the open dialog is answered "no" the way Escape would, and what waits behind it is
// answered "no" without being shown, so nothing the old session asked is put in front of the next one and no promise is left pending (a dialog closed from
// outside would leave the queue waiting on it for good).
export function cancelAppDialogs() {
  generation++;
  cancelActive?.();
}

function enqueue(options) {
  const asked = generation;
  const next = queue.then(() => (asked === generation ? run(options) : declined(options.fields)));
  queue = next.catch(() => {});
  return next;
}

export const confirmDialog = (message, { title = 'Please confirm', confirm = 'OK', cancel = 'Cancel', danger = false } = {}) => enqueue({ title, message, confirm, cancel, danger });
export const noticeDialog = (message, { title = 'Notice' } = {}) => enqueue({ title, message, notice: true }).then(() => undefined);
export const promptDialog = (message, { title = 'Enter a value', label = message, value = '', placeholder = '', confirm = 'OK', required = false, validate, inputMode } = {}) =>
  enqueue({ title, message: '', confirm, fields: [{ name: 'value', label, value, placeholder, required, validate, inputMode }] }).then(result => (result ? result.value : null));
export const formDialog = (options) => enqueue(options);

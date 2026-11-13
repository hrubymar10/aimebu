// In-app dialogs for questions, confirmations and notices, one shared component in place of the browser's prompt(), confirm() and
// alert(). Each function returns a promise: confirm -> true/false, prompt -> the text or null, form -> the values or null, notice -> void.
// One dialog is open at a time; a second request waits for the first. Escape or Cancel answers "no".
import { iconButton } from './icon-button.js';
let dialog = null, queue = Promise.resolve(), generation = 0, cancelActive = null;

function build() {
  const node = document.createElement('dialog'); node.id = 'app-dialog'; node.className = 'app-dialog dialog-card dialog-card-primary';
  node.setAttribute('aria-labelledby', 'app-dialog-title');
  node.innerHTML = '<form novalidate><h2 id="app-dialog-title"></h2><p class="app-dialog-message"></p><div class="app-dialog-fields"></div><p class="error" role="alert"></p>' +
    '<div class="dialog-actions"><button type="button" class="btn app-dialog-cancel">Cancel</button><button type="submit" class="btn btn-primary app-dialog-ok">OK</button></div></form>';
  document.body.append(node);
  return node;
}

function field(spec, index) {
  if (spec.reset) return resetField(spec, index);
  const label = document.createElement('label'); label.className = spec.type === 'checkbox' ? 'app-dialog-check' : 'app-dialog-field';
  const input = document.createElement('input'); input.type = spec.type === 'checkbox' ? 'checkbox' : 'text'; input.name = spec.name; input.id = `app-dialog-field-${index}`;
  if (spec.type === 'checkbox') input.checked = !!spec.value; else { input.value = spec.value || ''; input.placeholder = spec.placeholder || ''; input.autocomplete = 'off'; if (spec.inputMode) input.inputMode = spec.inputMode; }
  const text = document.createElement('span'); text.textContent = spec.label;
  if (spec.type === 'checkbox') label.append(input, text); else label.append(text, input);
  if (spec.hint) { const hint = document.createElement('small'); hint.textContent = spec.hint; hint.id = `${input.id}-hint`; input.setAttribute('aria-describedby', hint.id); label.append(hint); }
  return { spec, label, input };
}

// A text field with a button to its left that puts spec.reset.value back (disabled while the field already holds it). The wrapper is a div, not a
// label: a button inside a label would become what the label points at.
function resetField(spec, index) {
  const wrap = document.createElement('div'); wrap.className = 'app-dialog-field';
  const input = document.createElement('input'); input.type = 'text'; input.name = spec.name; input.id = `app-dialog-field-${index}`;
  input.value = spec.value || ''; input.placeholder = spec.placeholder || ''; input.autocomplete = 'off';
  const text = document.createElement('label'); text.htmlFor = input.id; text.textContent = spec.label;
  const reset = iconButton('reopen', spec.reset.label, { className: 'app-dialog-reset' }); reset.id = `${input.id}-reset`;
  reset.onclick = () => { input.value = spec.reset.value; input.dispatchEvent(new Event('input')); input.focus(); };
  const row = document.createElement('div'); row.className = 'app-dialog-input-row'; row.append(reset, input);
  wrap.append(text, row);
  return { spec, label: wrap, input, reset };
}

function run({ title, message, fields = [], confirm = 'OK', cancel = 'Cancel', danger = false, notice = false }) {
  dialog ||= build();
  const node = dialog, form = node.querySelector('form'), errorNode = node.querySelector('.error');
  const ok = node.querySelector('.app-dialog-ok'), no = node.querySelector('.app-dialog-cancel');
  node.querySelector('#app-dialog-title').textContent = title;
  node.querySelector('.app-dialog-message').textContent = message || ''; node.querySelector('.app-dialog-message').hidden = !message;
  const built = fields.map(field);
  node.querySelector('.app-dialog-fields').replaceChildren(...built.map(item => item.label));
  ok.textContent = confirm; ok.className = `btn ${danger ? 'btn-danger' : 'btn-primary'} app-dialog-ok`; no.textContent = cancel; no.hidden = notice; errorNode.textContent = '';
  // A field that depends on another (showIf) follows it as it changes.
  const values = () => Object.fromEntries(built.map(({ spec, input }) => [spec.name, spec.type === 'checkbox' ? input.checked : input.value.trim()]));
  // disabledIf greys a field out instead of hiding it; its reset button follows it, and is also off while the field holds the reset value.
  const sync = () => {
    const now = values();
    for (const item of built) {
      item.label.hidden = item.spec.showIf ? !item.spec.showIf(now) : false;
      if (item.spec.disabledIf) item.input.disabled = !!item.spec.disabledIf(now);
      if (item.reset) { item.reset.hidden = !item.spec.reset.value; item.reset.disabled = item.input.disabled || item.input.value.trim() === item.spec.reset.value; }
    }
  };
  for (const item of built) item.input.oninput = item.input.onchange = sync;
  sync();
  const opener = document.activeElement;
  return new Promise(resolve => {
    const finish = result => { node.onclose = null; node.oncancel = null; form.onsubmit = null; no.onclick = null; cancelActive = null; if (node.open) node.close(); opener?.focus?.(); resolve(result); };
    cancelActive = () => finish(fields.length ? null : false);
    form.onsubmit = event => {
      event.preventDefault();
      const now = values();
      for (const { spec, label } of built) if (spec.required && !label.hidden && !now[spec.name]) { errorNode.textContent = spec.requiredMessage || `${spec.label} is required.`; label.querySelector('input').focus(); return; }
      for (const { spec, label } of built) { const problem = !label.hidden && spec.validate?.(now[spec.name], now); if (problem) { errorNode.textContent = problem; label.querySelector('input').focus(); return; } }
      finish(fields.length ? now : true);
    };
    no.onclick = () => finish(fields.length ? null : false);
    node.oncancel = event => { event.preventDefault(); finish(fields.length ? null : false); };
    node.showModal();
    // A destructive question starts on Cancel; a form on its first field; anything else on the confirm button.
    (danger ? no : built[0]?.input || ok).focus();
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
  const next = queue.then(() => (asked === generation ? run(options) : (options.fields?.length ? null : false)));
  queue = next.catch(() => {});
  return next;
}

export const confirmDialog = (message, { title = 'Please confirm', confirm = 'OK', cancel = 'Cancel', danger = false } = {}) => enqueue({ title, message, confirm, cancel, danger });
export const noticeDialog = (message, { title = 'Notice' } = {}) => enqueue({ title, message, notice: true }).then(() => undefined);
export const promptDialog = (message, { title = 'Enter a value', label = message, value = '', placeholder = '', confirm = 'OK', required = false, validate, inputMode } = {}) =>
  enqueue({ title, message: '', confirm, fields: [{ name: 'value', label, value, placeholder, required, validate, inputMode }] }).then(result => (result ? result.value : null));
export const formDialog = (options) => enqueue(options);

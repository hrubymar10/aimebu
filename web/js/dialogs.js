// In-app dialogs for questions, confirmations and notices, one shared component in place of the browser's prompt(), confirm() and
// alert(). Each function returns a promise: confirm -> true/false, prompt -> the text or null, form -> the values or null, notice -> void.
// One dialog is open at a time; a second request waits for the first. Escape or Cancel answers "no".
let dialog = null, queue = Promise.resolve();

function build() {
  const node = document.createElement('dialog'); node.id = 'app-dialog'; node.className = 'app-dialog dialog-card dialog-card-primary';
  node.setAttribute('aria-labelledby', 'app-dialog-title');
  node.innerHTML = '<form novalidate><h2 id="app-dialog-title"></h2><p class="app-dialog-message"></p><div class="app-dialog-fields"></div><p class="error" role="alert"></p>' +
    '<div class="dialog-actions"><button type="button" class="btn app-dialog-cancel">Cancel</button><button type="submit" class="btn btn-primary app-dialog-ok">OK</button></div></form>';
  document.body.append(node);
  return node;
}

function field(spec, index) {
  const label = document.createElement('label'); label.className = spec.type === 'checkbox' ? 'app-dialog-check' : 'app-dialog-field';
  const input = document.createElement('input'); input.type = spec.type === 'checkbox' ? 'checkbox' : 'text'; input.name = spec.name; input.id = `app-dialog-field-${index}`;
  if (spec.type === 'checkbox') input.checked = !!spec.value; else { input.value = spec.value || ''; input.placeholder = spec.placeholder || ''; input.autocomplete = 'off'; if (spec.inputMode) input.inputMode = spec.inputMode; }
  const text = document.createElement('span'); text.textContent = spec.label;
  if (spec.type === 'checkbox') label.append(input, text); else label.append(text, input);
  if (spec.hint) { const hint = document.createElement('small'); hint.textContent = spec.hint; hint.id = `${input.id}-hint`; input.setAttribute('aria-describedby', hint.id); label.append(hint); }
  return { spec, label, input };
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
  const sync = () => { const now = values(); for (const item of built) item.label.hidden = item.spec.showIf ? !item.spec.showIf(now) : false; };
  for (const item of built) item.input.oninput = item.input.onchange = sync;
  sync();
  const opener = document.activeElement;
  return new Promise(resolve => {
    const finish = result => { node.onclose = null; node.oncancel = null; form.onsubmit = null; no.onclick = null; if (node.open) node.close(); opener?.focus?.(); resolve(result); };
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

function enqueue(options) {
  const next = queue.then(() => run(options));
  queue = next.catch(() => {});
  return next;
}

export const confirmDialog = (message, { title = 'Please confirm', confirm = 'OK', danger = false } = {}) => enqueue({ title, message, confirm, danger });
export const noticeDialog = (message, { title = 'Notice' } = {}) => enqueue({ title, message, notice: true }).then(() => undefined);
export const promptDialog = (message, { title = 'Enter a value', label = message, value = '', placeholder = '', confirm = 'OK', required = false, validate, inputMode } = {}) =>
  enqueue({ title, message: '', confirm, fields: [{ name: 'value', label, value, placeholder, required, validate, inputMode }] }).then(result => (result ? result.value : null));
export const formDialog = (options) => enqueue(options);

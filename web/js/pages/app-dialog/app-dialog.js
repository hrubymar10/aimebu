import { html } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';

// The inputs are uncontrolled (defaultValue, defaultChecked) and read when the form is sent: a redraw for showIf or disabledIf never touches what
// was typed. Each call is keyed by its number, so a new call starts from fresh inputs. The ids go by position, as they always have.
function Field({ spec, index, state, onInput, onReset }) {
  const id = `app-dialog-field-${index}`;
  if (spec.reset) {
    // A div, not a label: a button inside a label would become what the label points at.
    return html`<div class="app-dialog-field" hidden=${state.hidden}><label for=${id}>${spec.label}</label><div class="app-dialog-input-row">
      <${IconButton} name="reopen" label=${spec.reset.label} className="app-dialog-reset" id=${`${id}-reset`} hidden=${state.reset.hidden} disabled=${state.reset.disabled} onClick=${() => onReset(index)} />
      <input type="text" name=${spec.name} id=${id} defaultValue=${spec.value || ''} placeholder=${spec.placeholder || ''} autocomplete="off" disabled=${state.disabled} onInput=${onInput} onChange=${onInput} /></div></div>`;
  }
  const hint = spec.hint && html`<small id=${`${id}-hint`}>${spec.hint}</small>`;
  if (spec.type === 'checkbox') {
    return html`<label class="app-dialog-check" hidden=${state.hidden}><input type="checkbox" name=${spec.name} id=${id} defaultChecked=${!!spec.value} aria-describedby=${spec.hint ? `${id}-hint` : undefined} disabled=${state.disabled} onInput=${onInput} onChange=${onInput} /><span>${spec.label}</span>${hint}</label>`;
  }
  return html`<label class="app-dialog-field" hidden=${state.hidden}><span>${spec.label}</span><input type="text" name=${spec.name} id=${id} defaultValue=${spec.value || ''} placeholder=${spec.placeholder || ''} autocomplete="off" inputMode=${spec.inputMode || undefined} aria-describedby=${spec.hint ? `${id}-hint` : undefined} disabled=${state.disabled} onInput=${onInput} onChange=${onInput} />${hint}</label>`;
}

// The body of #app-dialog. The <dialog> itself, its showModal, close and cancel, belong to dialogs.js.
export function AppDialog({ call, states, error, onInput, onReset, onSubmit, onCancel }) {
  if (!call) return null;
  const { title, message, fields, confirm, cancel, danger, notice } = call;
  return html`<form key=${call.seq} novalidate onSubmit=${onSubmit}><h2 id="app-dialog-title">${title}</h2><p class="app-dialog-message" hidden=${!message}>${message || ''}</p>
    <div class="app-dialog-fields">${fields.map((spec, index) => html`<${Field} key=${spec.name} spec=${spec} index=${index} state=${states[index]} onInput=${onInput} onReset=${onReset} />`)}</div>
    <p class="error" role="alert">${error}</p>
    <div class="dialog-actions"><button type="button" class="btn app-dialog-cancel" hidden=${notice} onClick=${onCancel}>${cancel}</button><button type="submit" class=${`btn ${danger ? 'btn-danger' : 'btn-primary'} app-dialog-ok`}>${confirm}</button></div></form>`;
}

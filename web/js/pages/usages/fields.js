import { html, useState } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { numberOf, boxText } from './quota-model.js';

// A settings row whose control is several fields: the label points at the first one (`fieldId`), which also carries the description.
export function FieldsRow({ label, desc, fieldId, className = '', children }) {
  return html`<div class=${`settings-row${className ? ` ${className}` : ''}`}>
    <div class="settings-row-info"><label class="settings-label" for=${fieldId}>${label}</label><span class="settings-desc" id=${`${fieldId}-desc`}>${desc}</span></div>
    <div class="settings-control">${children}</div>
  </div>`;
}

// `rest` carries what a settings row hands its control (the id and the description it is described by).
export const NumberField = ({ value, label, range, onInput, describedBy, ...rest }) => html`<input class="settings-text-input" type="number" aria-label=${label} aria-describedby=${describedBy}
  min=${String(range[0])} max=${String(range[1])} step="1" value=${boxText(value)} onInput=${event => onInput(numberOf(event.currentTarget.value))} ...${rest} />`;

export const EntryField = ({ caption, children }) => html`<span class="quota-entry-field"><span class="quota-entry-caption" aria-hidden="true">${caption}</span>${children}</span>`;

// A text button, or with `glyph` an icon button whose label is also its tooltip. `focus` names it for the focus that follows a change.
export function ActionButton({ text, label, onClick, disabled = false, primary = false, focus = '', glyph = '', danger = false, className = '', ...rest }) {
  if (glyph) return html`<${IconButton} name=${glyph} label=${label} danger=${danger} className=${className} disabled=${disabled} data-quota-focus=${focus || undefined} onClick=${onClick} ...${rest} />`;
  return html`<button type="button" class=${`${primary ? 'btn btn-primary' : 'btn'}${className ? ` ${className}` : ''}`} aria-label=${label} disabled=${disabled} data-quota-focus=${focus || undefined} onClick=${onClick} ...${rest}>${text}</button>`;
}

// The picker of profiles that can still be added, with its Add button (disabled until one is chosen).
export function AddRow({ id, focus, available, label, desc, addLabel, onAdd }) {
  const [chosen, setChosen] = useState('');
  return html`<${FieldsRow} label=${label} desc=${desc} fieldId=${id}>
    <div class="quota-add-controls">
      <select class="settings-select" id=${id} aria-describedby=${`${id}-desc`} data-quota-focus=${focus} disabled=${!available.length} value=${chosen} onChange=${event => setChosen(event.currentTarget.value)}>
        <option value="">${available.length ? 'Choose a profile' : 'No other profiles'}</option>
        ${available.map(name => html`<option value=${name} key=${name}>${name}</option>`)}
      </select>
      <${ActionButton} text="Add" label=${addLabel} disabled=${!chosen} onClick=${() => { if (!chosen) return; onAdd(chosen); setChosen(''); }} />
    </div>
  <//>`;
}

import { html } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { useDirty } from '../../hooks/use-dirty.js';
import { formDirty } from './model.js';

const KEY_DESC = 'Typed between angle brackets, as <KEY>, to insert the macro. Lowercase letters, digits, _ or -, starting with a letter.';
const BODY_DESC = 'Text inserted into the draft when you pick the macro. It may contain other <KEY> references, which expand up to 8 levels. Up to 16 KiB.';

export function MacroForm({ state, keyRef, onSave, onCancel, onDirty }) {
  const form = state.form.value, editing = state.editing.value, busy = state.busy.value;
  useDirty(onDirty, 'macro-form', formDirty(form, editing, state.macros.value));
  const set = (name, value) => { state.form.value = { ...state.form.peek(), [name]: value }; };
  return html`<form id="macro-editor" hidden=${!state.canEdit.value} onSubmit=${event => { event.preventDefault(); onSave(); }}>
    <${SettingsGroup} label="Macro">
      <${SettingsRow} label="Key" desc=${KEY_DESC} id="macro-key">
        <input class="settings-text-input" name="key" required placeholder="greeting" ref=${keyRef} disabled=${busy} value=${form.key} onInput=${event => set('key', event.currentTarget.value)} />
      <//>
      <${SettingsRow} label="Body" desc=${BODY_DESC} id="macro-body" className="settings-row-stack">
        <textarea class="settings-text-input settings-textarea" name="body" rows="3" disabled=${busy} value=${form.body} onInput=${event => set('body', event.currentTarget.value)}></textarea>
      <//>
    <//>
    <div class="dialog-actions">
      <button type="submit" class="btn btn-primary" disabled=${busy || !state.loaded.value}>Save macro</button>
      <button type="button" class="btn" hidden=${!editing} disabled=${busy} onClick=${onCancel}>Cancel</button>
    </div>
  </form>`;
}

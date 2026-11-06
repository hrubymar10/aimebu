import { html } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { useDirty } from '../../hooks/use-dirty.js';

export function ImportPanel({ state, textRef, onText, onPreview, onApply, onCancel, onDirty }) {
  const open = state.importOpen.value, text = state.importText.value, busy = state.busy.value, summary = state.summary.value;
  useDirty(onDirty, 'macro-import', open && text.trim() !== '');
  return html`<div id="macros-import" class="macro-import" hidden=${!open}>
    <${SettingsGroup} label="Import macros">
      <${SettingsRow} label="Macro JSON" desc="An object of macro keys and bodies, such as the output of Copy all. A matching key is overwritten." id="macros-import-text" className="settings-row-stack">
        <textarea class="settings-text-input settings-textarea" rows="6" placeholder=${'{"greeting": "Hello"}'} aria-label="Macro JSON to import" spellcheck="false" ref=${textRef} disabled=${busy} value=${text} onInput=${event => onText(event.currentTarget.value)}></textarea>
      <//>
    <//>
    <p id="macros-import-summary" class=${summary.error ? 'macro-import-summary error' : 'macro-import-summary'} role="status">${summary.text}</p>
    <div class="dialog-actions">
      <button type="button" id="macros-import-preview" class="btn" disabled=${busy || !state.loaded.value} onClick=${onPreview}>Preview</button>
      <button type="button" id="macros-import-apply" class="btn btn-primary" disabled=${busy || !state.loaded.value || !state.pending.value} onClick=${onApply}>Apply import</button>
      <button type="button" class="btn" disabled=${busy} onClick=${onCancel}>Cancel</button>
    </div>
  </div>`;
}

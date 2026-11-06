import { html, Fragment, useRef } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { IconButton } from '../../components/icon-button.js';
import { filterMacroKeys } from '../../macro-import.js';
import { MacroForm } from './macro-form.js';
import { ImportPanel } from './import-panel.js';
import { useMacros } from './use-macros.js';
import { label } from './model.js';

const INTRO = 'Type <KEY> anywhere in a draft, then select a suggestion to insert its editable text.';

export function MacrosPage({ openToken, onDirty = () => {} }) {
  const { state, save, remove, edit, resetForm, setImportText, preview, applyImport, toggleImport, copyAll } = useMacros({ openToken });
  const keyRef = useRef(null), importRef = useRef(null);
  const macros = state.macros.value, canEdit = state.canEdit.value, loaded = state.loaded.value;
  const locked = state.busy.value || !loaded;
  const keys = filterMacroKeys(macros, state.filter.value);
  const empty = !Object.keys(macros).length;
  const openImport = () => { toggleImport(true); queueMicrotask(() => importRef.current?.focus()); };
  const startEdit = key => { edit(key); keyRef.current?.focus(); };

  return html`
    <${Fragment}>
      <h3>Macros</h3>
      <p class="settings-intro">${INTRO}</p>
      <div class="macro-toolbar">
        <${SettingsGroup} label="Find macros">
          <${SettingsRow} label="Search" desc="Show only the macros whose key or text contains this." id="macros-search">
            <input class="settings-text-input" type="search" placeholder="Search macros" aria-label="Search macros" value=${state.filter.value} onInput=${event => { state.filter.value = event.currentTarget.value; }} />
          <//>
        <//>
        <div class="dialog-actions">
          <button type="button" id="macros-copy" class="btn" title="Copy every macro as JSON" disabled=${empty} onClick=${copyAll}>${state.copyLabel.value}</button>
          <button type="button" id="macros-import-open" class="btn" title="Paste macro JSON to add or update macros" hidden=${state.importOpen.value || !canEdit} onClick=${openImport}>Import…</button>
        </div>
      </div>
      <${ImportPanel} state=${state} textRef=${importRef} onText=${setImportText} onPreview=${preview} onApply=${applyImport} onCancel=${() => toggleImport(false)} onDirty=${onDirty} />
      <${MacroForm} state=${state} keyRef=${keyRef} onSave=${save} onCancel=${resetForm} onDirty=${onDirty} />
      <p class="error" role="alert">${state.error.value}</p>
      <div id="macros-list">
        ${loaded && empty ? html`<p>No macros yet.</p>` : loaded && !keys.length ? html`<p>No macros match the search.</p>` : null}
        ${keys.map(key => html`<div class="snippet-row" key=${key}>
          <div class="snippet-text"><strong>${label(key)}</strong><p>${macros[key]}</p></div>
          ${canEdit && html`<div class="snippet-actions-row">
            <${IconButton} name="edit" label=${`Edit ${label(key)}`} disabled=${locked} onClick=${() => startEdit(key)} />
            <${IconButton} name="trash" label=${`Delete ${label(key)}`} danger disabled=${locked} onClick=${() => remove(key)} />
          </div>`}
        </div>`)}
      </div>
    <//>
  `;
}

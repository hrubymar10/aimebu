import { html } from '../../ui.js';
import { useDirty } from '../../hooks/use-dirty.js';
import { sourceLabel, layerOf, resetLabel, countText } from './model.js';

export function SlotCard({ slot, scope, max, draft, locked, onDraft, onSave, onReset, onDirty }) {
  const value = draft ?? slot.effective;
  useDirty(onDirty, `prompt-${slot.key}`, draft !== undefined);
  return html`<div class="prompt-slot" data-key=${slot.key}>
    <div class="prompt-slot-head"><strong>${slot.title}</strong><span class="prompt-source">${sourceLabel(slot, scope)}</span></div>
    <p class="settings-intro">${slot.desc}</p>
    <textarea class="settings-text-input settings-textarea" rows="4" maxLength=${max} id=${`prompt-${slot.key}`} aria-label=${slot.title} disabled=${locked} value=${value} onInput=${event => onDraft(slot, event.currentTarget.value)}></textarea>
    <div class="dialog-actions">
      <button type="button" class="btn btn-primary prompt-save" disabled=${locked || draft === undefined || draft === slot.effective || (draft === slot.default && !layerOf(slot, scope))} onClick=${() => onSave(slot)}>Save</button>
      <button type="button" class="btn prompt-reset" disabled=${locked || !layerOf(slot, scope)} onClick=${() => onReset(slot)}>${resetLabel(scope)}</button>
      <span class="prompt-count">${countText(value, max)}</span>
    </div>
    <details class="prompt-shipped"><summary>Show the shipped default</summary><p>${slot.default}</p></details>
  </div>`;
}

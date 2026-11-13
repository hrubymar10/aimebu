import { html, useState } from '../../ui.js';

// Keep the chosen model visible while typing, so filtering never silently changes a spawn or switch.
export function ModelPicker({ models, value, onChange, prefix }) {
  const [query, setQuery] = useState('');
  const needle = query.trim().toLocaleLowerCase();
  const matches = needle ? models.filter(model => [model.label, model.value, model.note].some(text => String(text || '').toLocaleLowerCase().includes(needle))) : models;
  const selected = models.find(model => model.value === value);
  const shown = selected && !matches.includes(selected) ? [selected, ...matches] : matches;
  return html`<div class="model-picker">
    <label for=${`${prefix}-model`}>Model</label>
    <input id=${`${prefix}-model-filter`} type="search" aria-label="Filter models" placeholder="Filter models…" autocomplete="off" value=${query} onInput=${event => setQuery(event.currentTarget.value)} />
    <select id=${`${prefix}-model`} name="model" required value=${value} onChange=${event => onChange(event.currentTarget.value)}>${shown.map(model => html`<option key=${model.value} value=${model.value} disabled=${!!model.unavailable} ...${model.note ? { title: model.note } : {}}>${model.label}</option>`)}</select>
    <span class="model-picker-count" role="status" hidden=${!needle}>${matches.length ? `${matches.length} matching model${matches.length === 1 ? '' : 's'}` : 'No matching models'}</span>
  </div>`;
}

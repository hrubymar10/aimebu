import { html, useState } from '../../ui.js';
import { useAlive } from '../../hooks/use-alive.js';
import { useDirty } from '../../hooks/use-dirty.js';
import { FAMILIES } from './model.js';
import { useIdPrefix } from './id-prefix.js';

export function AddHarnessForm({ onCancel, onCreate, onDirty }) {
  const alive = useAlive();
  const prefix = useIdPrefix();
  const [fields, setFields] = useState({ tool: FAMILIES[0][0], label: '', command: '', containerized: false });
  useDirty(onDirty, 'add-harness', fields.tool !== FAMILIES[0][0] || !!fields.label || !!fields.command || fields.containerized);
  const [error, setError] = useState('');
  const [saving, setSaving] = useState(false);
  const set = (key, value) => setFields(current => ({ ...current, [key]: value }));
  const submit = async event => {
    event.preventDefault();
    setError('');
    setSaving(true);
    try {
      await onCreate({ ...fields, label: fields.label.trim(), command: fields.command.trim() });
    } catch (problem) {
      if (alive()) {
        setError(problem.message || 'Could not add the harness');
        setSaving(false);
      }
    }
  };
  return html`
    <form class="machine-harness-form harness-add-form" id=${`${prefix}harness-add-form`} onSubmit=${submit}>
      <label class="machine-harness-path-label">Family<select
        name="family"
        value=${fields.tool}
        onChange=${event => set('tool', event.currentTarget.value)}
      >
        ${FAMILIES.map(([value, name]) => html`<option value=${value} key=${value}>${name}</option>`)}
      </select></label>
      <label class="machine-harness-path-label">Label (optional)<input
        type="text"
        name="label"
        maxLength="80"
        autocomplete="off"
        value=${fields.label}
        onInput=${event => set('label', event.currentTarget.value)}
      /></label>
      <label class="machine-harness-path-label">Executable name or absolute path<input
        type="text"
        name="command"
        autocomplete="off"
        spellcheck=${false}
        required
        value=${fields.command}
        onInput=${event => set('command', event.currentTarget.value)}
      /></label>
      <label class="machine-harness-enabled"><input
        type="checkbox"
        name="containerized"
        checked=${fields.containerized}
        onChange=${event => set('containerized', event.currentTarget.checked)}
      />A Docker wrapper</label>
      <div class="machine-harness-actions">
        <button type="button" class="btn" onClick=${onCancel}>Cancel</button>
        <button type="submit" class="btn btn-primary" disabled=${saving}>Add harness</button>
      </div>
      <p class="error" role="alert">${error}</p>
    </form>
  `;
}

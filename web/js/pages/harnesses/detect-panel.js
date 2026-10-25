import { html, useState } from '../../ui.js';
import { useAlive } from '../../hooks/use-alive.js';
import { useDirty } from '../../hooks/use-dirty.js';

export function DetectPanel({ candidates, onClose, onAdd, onFailed, onDirty }) {
  const alive = useAlive();
  const [picked, setPicked] = useState(() => new Set());
  useDirty(onDirty, 'detect-harnesses', picked.size > 0);
  const [error, setError] = useState('');
  const [adding, setAdding] = useState(false);
  const toggle = index => setPicked(current => {
    const next = new Set(current);
    if (next.has(index)) next.delete(index);
    else next.add(index);
    return next;
  });
  const submit = async event => {
    event.preventDefault();
    setError('');
    const chosen = candidates.filter((_, index) => picked.has(index));
    if (!chosen.length) {
      setError('Tick at least one harness to add.');
      return;
    }
    setAdding(true);
    try {
      await onAdd(chosen);
    } catch (problem) {
      if (alive()) {
        setError(problem.message || 'Could not add the harnesses');
        setAdding(false);
      }
      await onFailed();
    }
  };
  return html`
    <form class="machine-harness-form harness-detect-panel" id="harness-detect-panel" onSubmit=${submit}>
      <strong>${candidates.length ? 'Found on this machine — tick the ones to add' : 'Nothing found on this machine'}</strong>
      ${!candidates.length && html`
        <p class="machine-harness-help">No Claude Code, Codex, Pi, OpenCode or Mistral Vibe executable was found. Add one by hand with its path.</p>
      `}
      ${candidates.map((candidate, index) => html`
        <label class="machine-harness-enabled harness-detect-row" data-command=${candidate.command} key=${`${candidate.command}:${index}`}>
          <input
            type="checkbox"
            checked=${picked.has(index)}
            disabled=${!!candidate.already_added}
            aria-label=${`Add ${candidate.label} at ${candidate.command}`}
            onChange=${() => toggle(index)}
          />
          <span>${`${candidate.label} · ${candidate.command}${candidate.version ? ` · ${candidate.version}` : ''}${candidate.already_added ? ' · already added' : ''}`}</span>
        </label>
      `)}
      <div class="machine-harness-actions">
        ${candidates.length > 0 && html`<button type="submit" class="btn btn-primary" disabled=${adding}>Add selected</button>`}
        <button type="button" class="btn" onClick=${onClose}>Close</button>
      </div>
      <p class="error" role="alert">${error}</p>
    </form>
  `;
}

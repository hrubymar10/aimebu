import { html, useState, useLayoutEffect, useRef } from '../../ui.js';
import { useAlive } from '../../hooks/use-alive.js';
import { useDirty } from '../../hooks/use-dirty.js';
import { useIdPrefix } from './id-prefix.js';
import { candidateVersionLabel } from '../../harness-version.js';

// Embedded in the onboarding there is no Add or Close button: its Continue adds what is ticked (through `controls`), so ticking only reports how many.
export function DetectPanel({ candidates, embedded = false, controls = null, onTick = () => {}, onClose, onAdd, onFailed, onDirty }) {
  const alive = useAlive();
  const prefix = useIdPrefix();
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
  const pickedRef = useRef(picked);
  pickedRef.current = picked;
  // Resolves to whether the harnesses were added, so the onboarding's Continue knows whether to go on.
  const add = async () => {
    setError('');
    const chosen = candidates.filter((_, index) => pickedRef.current.has(index));
    if (!chosen.length) {
      setError('Tick at least one harness to add.');
      return false;
    }
    setAdding(true);
    try {
      await onAdd(chosen);
      return true;
    } catch (problem) {
      if (alive()) {
        setError(problem.message || 'Could not add the harnesses');
        setAdding(false);
      }
      await onFailed();
      return false;
    }
  };
  const submit = event => {
    event.preventDefault();
    add();
  };
  useLayoutEffect(() => { onTick(picked.size); }, [picked.size]);
  useLayoutEffect(() => {
    if (!controls) return undefined;
    controls.current = { count: () => pickedRef.current.size, add };
    return () => { controls.current = null; onTick(0); };
  }, [controls]);
  return html`
    <form class="machine-harness-form harness-detect-panel" id=${`${prefix}harness-detect-panel`} onSubmit=${submit}>
      <strong>${candidates.length ? `Found on this machine — tick the ones to add${embedded ? ', then press Continue' : ''}` : 'Nothing found on this machine'}</strong>
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
          <span>${`${candidate.label} · ${candidate.command}${candidateVersionLabel(candidate)}${candidate.already_added ? ' · already added' : ''}`}</span>
        </label>
      `)}
      <div class="machine-harness-actions">
        ${candidates.length > 0 && !embedded && html`<button type="submit" class="btn btn-primary" disabled=${adding}>Add selected</button>`}
        ${!embedded && html`<button type="button" class="btn" onClick=${onClose}>Close</button>`}
      </div>
      <p class="error" role="alert">${error}</p>
    </form>
  `;
}

import { html, useState, useRef, useLayoutEffect } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { useAlive } from '../../hooks/use-alive.js';
import { agentIsolation } from '../../agent-isolation.js';

const READ_ONLY = 'Only the owner or an admin can change this.';
const DESC = 'Agents on a native launcher run as the person who runs the supervisor, with that person’s files within reach, including other agents’ working folders. With this on, a native launcher is refused: no new agent is started on one, and an agent already on one gets no turns until it is moved to a Docker launcher. Off by default.';

// One switch, saved when it is changed: the instance either refuses native launchers or it does not. A refused save puts the switch back.
export function AgentIsolation() {
  const alive = useAlive();
  const [, changed] = useState(0);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const writing = useRef(false);
  useLayoutEffect(() => agentIsolation.onChange(() => changed(count => count + 1)), []);
  const canEdit = agentIsolation.canEdit;
  const change = async checked => {
    if (writing.current) return;
    writing.current = true;
    setError('');
    setSaving(true);
    try { await agentIsolation.save(checked); } catch (failure) { if (alive()) setError(failure.message || 'Could not save'); } finally { writing.current = false; if (alive()) { setSaving(false); changed(count => count + 1); } }
  };
  return html`
    <h3>Agents</h3>
    <${SettingsGroup} label="Agents">
      <${SettingsRow} label="Only run agents in docker" desc=${DESC} id="agent-docker-only" className="settings-row-toggle">
        <input type="checkbox" checked=${agentIsolation.dockerOnly} disabled=${!canEdit || saving} onChange=${event => change(event.currentTarget.checked)} />
      <//>
    <//>
    <p class="settings-intro" role="status">${canEdit ? '' : READ_ONLY}</p>
    <p id="agent-docker-only-error" class="error" role="alert">${error}</p>
  `;
}

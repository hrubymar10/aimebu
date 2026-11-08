import { html, useState, useRef, useLayoutEffect } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { useAlive } from '../../hooks/use-alive.js';
import { agentIsolation } from '../../agent-isolation.js';

const READ_ONLY = 'Only the owner or an admin can change this.';
const WARM_DESC = 'After a turn, an agent\'s harness stays running for this many minutes, so a message that arrives in the window skips the start (a few seconds) and the agent answers faster. After the window it goes back to sleep and the next turn starts it again. Each warm agent holds memory (roughly 0.3 GB for Claude Code), and when the machine runs low the longest-idle one is put to sleep first. 0 turns it off, which is the default. Only Claude Code agents are kept warm so far.';
const WARM_OWNER_ONLY = 'Only the owner can change this.';
const DESC = 'Agents on a native launcher run as the person who runs the supervisor, with that person’s files within reach, including other agents’ working folders. With this on, a native launcher is refused: no new agent is started on one, and an agent already on one gets no turns until it is moved to a Docker launcher. Off by default.';

// One switch, saved when it is changed: the instance either refuses native launchers or it does not. A refused save puts the switch back.
export function AgentIsolation() {
  const alive = useAlive();
  const [, changed] = useState(0);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [warmError, setWarmError] = useState('');
  const [warmSaving, setWarmSaving] = useState(false);
  const [warmDraft, setWarmDraft] = useState(null);
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
  const saveWarm = async () => {
    if (warmDraft === null || writing.current) return;
    const minutes = Number(warmDraft);
    if (!Number.isInteger(minutes) || minutes < 0 || minutes > 1440) { setWarmError('Enter a whole number of minutes from 0 to 1440.'); return; }
    if (minutes === agentIsolation.warmIdleMinutes) { setWarmDraft(null); return; }
    writing.current = true;
    setWarmError('');
    setWarmSaving(true);
    try { await agentIsolation.saveWarm(minutes); if (alive()) setWarmDraft(null); } catch (failure) { if (alive()) setWarmError(failure.message || 'Could not save'); } finally { writing.current = false; if (alive()) { setWarmSaving(false); changed(count => count + 1); } }
  };
  return html`
    <h3>Agents</h3>
    <${SettingsGroup} label="Agents">
      <${SettingsRow} label="Only run agents in docker" desc=${DESC} id="agent-docker-only" className="settings-row-toggle">
        <input type="checkbox" checked=${agentIsolation.dockerOnly} disabled=${!canEdit || saving} onChange=${event => change(event.currentTarget.checked)} />
      <//>
      <${SettingsRow} label="Keep agents warm (minutes)" desc=${WARM_DESC} id="agent-warm-idle">
        <input type="number" min="0" max="1440" step="1" value=${warmDraft ?? agentIsolation.warmIdleMinutes} disabled=${!agentIsolation.canEditWarm || warmSaving}
          title=${agentIsolation.canEditWarm ? undefined : WARM_OWNER_ONLY}
          onInput=${event => setWarmDraft(event.currentTarget.value)} onBlur=${saveWarm} onKeyDown=${event => { if (event.key === 'Enter') { event.preventDefault(); saveWarm(); } }} />
      <//>
    <//>
    <p class="settings-intro" role="status">${canEdit ? '' : READ_ONLY}</p>
    <p id="agent-docker-only-error" class="error" role="alert">${error}</p>
    <p class="settings-intro" role="note" hidden=${agentIsolation.canEditWarm}>${WARM_OWNER_ONLY}</p>
    <p id="agent-warm-idle-error" class="error" role="alert">${warmError}</p>
  `;
}

import { html, useState, useRef, useLayoutEffect } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { useAlive } from '../../hooks/use-alive.js';
import { agentIsolation } from '../../agent-isolation.js';

const WARM_OWNER_ONLY = 'Only the owner can change this.';
const KEEP_DESC = 'A harness remembers a long conversation cheaply only while its prompt cache lives (Claude Code keeps it for an hour). For an agent that is idle, aimebu sends it a tiny ping shortly before the cache would expire, so the next real message does not have to pay to write the whole context again. A ping posts nothing, creates nothing to review and does not touch what you have read; it costs the cache-read price of the agent\'s context (the agent\'s card shows what a stretch of pings costs), and it adds about 150 tokens of context each. On by default.';
const KEEP_INTERVAL_DESC = 'Minutes between pings after an agent\'s last turn. Claude Code keeps its cache for an hour, so 50, and nothing over 55 is accepted (a ping later than the hour always finds the cache gone). 0 turns pinging off for that harness.';
const KEEP_CODEX_DESC = 'Minutes between pings for Codex agents. Codex does not say how long it keeps its cache, so 10 is a starting guess: each ping is logged with how much it read from the cache, to tune it from.';
const KEEP_STOP_DESC = 'Pings stop when an agent has had no real work for this many hours (24 by default), and when it is stopped.';

// The prompt-cache keep-alive: one switch and the three numbers that shape it, each saved when it is changed. Its own section of General, with its own heading.
export function PromptCache() {
  const alive = useAlive();
  const [, changed] = useState(0);
  const [keepError, setKeepError] = useState('');
  const [keepSaving, setKeepSaving] = useState(false);
  const [keepDraft, setKeepDraft] = useState({});
  const writing = useRef(false);
  useLayoutEffect(() => agentIsolation.onChange(() => changed(count => count + 1)), []);
  const keep = agentIsolation.keepAlive;
  const saveKeep = async change => {
    if (writing.current) return;
    writing.current = true;
    setKeepError('');
    setKeepSaving(true);
    try { await agentIsolation.saveKeepAlive(change); if (alive()) setKeepDraft({}); } catch (failure) { if (alive()) setKeepError(failure.message || 'Could not save'); } finally { writing.current = false; if (alive()) { setKeepSaving(false); changed(count => count + 1); } }
  };
  const keepNumber = (field, label, desc, max) => {
    const commit = () => {
      if (keepDraft[field] === undefined) return;
      const value = Number(keepDraft[field]);
      if (!Number.isInteger(value) || value < 0 || value > max) { setKeepError(`Enter a whole number from 0 to ${max}.`); return; }
      if (value === keep[field]) { setKeepDraft(draft => { const next = { ...draft }; delete next[field]; return next; }); return; }
      saveKeep({ [field]: value });
    };
    return html`<${SettingsRow} label=${label} desc=${desc} id=${`agent-keepalive-${field.split('_').join('-')}`}>
      <input type="number" min="0" max=${max} step="1" value=${keepDraft[field] ?? keep[field]} disabled=${!agentIsolation.canEditWarm || keepSaving} title=${agentIsolation.canEditWarm ? undefined : WARM_OWNER_ONLY}
        onInput=${event => { const value = event.currentTarget.value; setKeepDraft(draft => ({ ...draft, [field]: value })); }} onBlur=${commit} onKeyDown=${event => { if (event.key === 'Enter') { event.preventDefault(); commit(); } }} />
    <//>`;
  };
  return html`
    <h3>Prompt cache</h3>
    <${SettingsGroup} label="Keep the prompt cache warm">
      <${SettingsRow} label="Keep the prompt cache warm" desc=${KEEP_DESC} id="agent-keepalive-enabled" className="settings-row-toggle">
        <input type="checkbox" checked=${!!keep.enabled} disabled=${!agentIsolation.canEditWarm || keepSaving} title=${agentIsolation.canEditWarm ? undefined : WARM_OWNER_ONLY} onChange=${event => saveKeep({ enabled: event.currentTarget.checked })} />
      <//>
      ${keepNumber('claude_minutes', 'Claude Code: ping every (minutes)', KEEP_INTERVAL_DESC, 55)}
      ${keepNumber('codex_minutes', 'Codex: ping every (minutes)', KEEP_CODEX_DESC, 1440)}
      ${keepNumber('stop_after_hours', 'Stop pinging after (hours without work)', KEEP_STOP_DESC, 720)}
    <//>
    <p id="agent-keepalive-error" class="error" role="alert">${keepError}</p>
    <p class="settings-intro" role="note" hidden=${agentIsolation.canEditWarm}>${WARM_OWNER_ONLY}</p>
  `;
}

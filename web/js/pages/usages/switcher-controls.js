import { html, useRef } from '../../ui.js';
import { SettingsGroup, SettingsRow } from '../../components/settings-row.js';
import { alertKind, usageSettingsBody, refreshMessage, WARMUP_MODES, warmupBody, warmupDisabled, tokenAutoRefreshOn, tokenAutoRefreshAvailable, tokenStateLine } from './model.js';

const AUTO_REFRESH = 'aimebu becomes the only thing that refreshes your Claude and Codex logins. While on, aimebu must stay running whenever you use Claude or Codex, also outside aimebu: if it is stopped for more than about an hour around a token\'s expiry, sessions get auth errors until it runs again. Turn this off before stopping aimebu for long or uninstalling it.';

// "Auto-refresh tokens": the server becomes the only refresher of the Claude and Codex logins. The setting migrates or un-migrates each tool on
// the server; the per-tool line shows how that went (migrated, off, pending, n/a, or waiting: <reason>).
function AccountMaintenance({ store, settings }) {
  const on = tokenAutoRefreshOn(settings), available = tokenAutoRefreshAvailable(settings);
  const disabled = !available && !on;
  const mode = settings?.warmup_mode || 'off';
  const warmupOff = warmupDisabled(settings);
  const schedule = useRef(null);
  return html`<${SettingsGroup} label="Account maintenance">
    <div class=${`settings-row${disabled ? ' usages-provider-row-disabled' : ''}`}>
      <div class="settings-row-info">
        <label class="settings-label" for="usage-token-auto-refresh">Auto-refresh tokens</label>
        <span class="settings-desc" id="usage-token-auto-refresh-desc">${available ? AUTO_REFRESH : `${AUTO_REFRESH} Unavailable: harness-docker-ctrl is not installed.`}</span>
        ${['claude', 'codex'].map(tool => html`<span class="settings-desc usage-token-state" data-tool=${tool} key=${tool}>${tokenStateLine(tool, settings?.token_auto_refresh_state?.[tool] || { status: 'off' })}</span>`)}
      </div>
      <div class="settings-control"><input type="checkbox" id="usage-token-auto-refresh" aria-describedby="usage-token-auto-refresh-desc" checked=${on} disabled=${disabled} onChange=${event => store.saveTokenAutoRefresh(event.currentTarget.checked)} /></div>
    </div>
    <${SettingsRow} label="Warm idle Claude and Codex accounts" desc="Start a five-hour session window with a short request for eligible inactive profiles. Active logins and profiles used by another credential target are skipped." id="usage-warmup-mode" className=${warmupOff ? 'usages-provider-row-disabled' : ''}>
      <select class="settings-select" value=${mode} disabled=${warmupOff} onChange=${event => store.mutateUsage(() => store.source.settings(warmupBody(event.currentTarget.value, settings)))}>
        ${WARMUP_MODES.map(([value, label]) => html`<option value=${value} key=${value}>${label}</option>`)}
      </select>
    <//>
    ${mode === 'scheduled' && html`<div class="settings-row">
      <div class="settings-row-info"><label class="settings-label" for="usage-warmup-schedule">Warmup schedule</label><span class="settings-desc" id="usage-warmup-schedule-desc">One five-field cron line per row, in the server’s local time. Each field accepts * or one number.</span></div>
      <div class="settings-control"><div class="usage-warmup-schedule-control">
        <textarea class="settings-text-input settings-textarea" id="usage-warmup-schedule" aria-describedby="usage-warmup-schedule-desc" rows="3" placeholder="30 9 * * 1" ref=${schedule} value=${settings?.warmup_schedule || ''}></textarea>
        <button type="button" class="settings-button" onClick=${() => store.mutateUsage(() => store.source.settings({ warmup_schedule: schedule.current.value }))}>Save schedule</button>
      </div></div>
    </div>`}
  <//>`;
}

function UsageConfig({ store, settings }) {
  const interval = useRef(null), display = useRef(null), warning = useRef(null), critical = useRef(null);
  const save = () => store.mutateUsage(() => store.source.settings(usageSettingsBody({ interval: interval.current.value, display: display.current.value, warning: warning.current.value, critical: critical.current.value })));
  const refresh = () => store.mutateUsage(async () => {
    try { await store.source.refresh(); } catch (failure) { failure.message = refreshMessage(failure); throw failure; }
  });
  return html`<div class="usage-config-controls">
    <${SettingsGroup} label="Usage refresh">
      <${SettingsRow} label="Refresh interval (seconds)" desc="How often the server re-reads provider limits in the background. Minimum 15 seconds." id="usage-refresh-interval">
        <input class="settings-text-input" type="number" min="15" ref=${interval} value=${String(settings?.refresh_interval_sec || 120)} />
      <//>
      <${SettingsRow} label="Percentages" desc="Show usage bars as the share still left or the share already used, here and in the usages popover." id="usage-percent-display">
        <select class="settings-select" ref=${display} value=${settings?.percent_display || 'left'}><option value="left">Percent left</option><option value="used">Percent used</option></select>
      <//>
      <${SettingsRow} label="Usage warning (% remaining)" desc="Send one global inbox alert per provider window and reset when this much quota remains." id="usage-alert-warning">
        <input class="settings-text-input" type="number" min="1" max="99" ref=${warning} value=${String(settings?.alert_warning_remaining ?? 20)} />
      <//>
      <${SettingsRow} label="Usage critical (% remaining)" desc="Send a stronger alert at this lower remaining percentage. It must be below the warning threshold." id="usage-alert-critical">
        <input class="settings-text-input" type="number" min="1" max="99" ref=${critical} value=${String(settings?.alert_critical_remaining ?? 10)} />
      <//>
    <//>
    <div class="settings-actions"><button type="button" class="btn btn-primary" onClick=${save}>Save usage settings</button><button type="button" class="btn" onClick=${refresh}>Refresh now</button></div>
    <${AccountMaintenance} store=${store} settings=${settings} />
  </div>`;
}

// Credentials of the token-copy model that need a human (a rejected refresh token, one about to expire), one sentence each. Plain text only:
// the server never sends CLI output here.
function CredentialAlerts({ alerts }) {
  return html`<div class="usage-credential-alerts" role="alert">${alerts.map((alert, index) => html`<p class=${`usage-credential-alert usage-credential-alert-${alertKind(alert.kind)}`} key=${index}>${String(alert.message || '')}</p>`)}</div>`;
}

export function SwitcherControls({ store }) {
  const state = store.switcher.value, usage = store.usage.value;
  if (!state) return null;
  if (!state.owner) return null;
  const alerts = usage?.credential_alerts;
  return html`
    <button type="button" class=${state.enabled ? 'btn' : 'btn btn-primary'} onClick=${() => store.mutateSwitcher(() => store.source.enable(!state.enabled))}>${state.enabled ? 'Disable account switcher' : 'Enable account switcher'}</button>
    ${Array.isArray(alerts) && alerts.length > 0 && html`<${CredentialAlerts} alerts=${alerts} />`}
    <${UsageConfig} key=${store.revision.value} store=${store} settings=${usage?.settings} />
  `;
}

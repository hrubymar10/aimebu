// Import-free: scripts/test-usages-model.cjs runs this module in a vm.

export const toolOf = key => (key === 'claude-code' ? 'claude' : key);
export const isSwitchable = tool => tool === 'claude' || tool === 'codex';
export const sortProviders = list => [...(list || [])].sort((a, b) => (a.order_number || 0) - (b.order_number || 0));
export const providerStatus = provider => (!provider.available ? 'Unavailable' : provider.enabled ? 'Enabled' : 'Not configured');
export const profileKey = (provider, profile) => `${provider}/${profile.profile_name || 'local'}`;

export const affectedText = affected => (affected.length ? `Switch affects: ${affected.map(agent => `${agent.id}${agent.running ? ' (running)' : ''}`).join(', ')}` : 'No agents use this tool');
export function impactGroups(affected) {
  if (!affected.length) return [];
  const groups = new Map();
  for (const agent of affected) {
    const room = agent.room || { id: '', name: 'Agents without a room' };
    const key = room.id || 'unassigned';
    const name = room.name?.trim() && !/^(room_|agt_)/.test(room.name) ? room.name.trim() : room.id ? 'Unnamed room' : 'Agents without a room';
    if (!groups.has(key)) groups.set(key, { name, roles: new Map() });
    const roles = groups.get(key).roles;
    const role = agent.role === 'lead' ? 'leader' : (agent.role || 'agent').replaceAll('_', ' ').toLowerCase();
    roles.set(role, (roles.get(role) || 0) + 1);
  }
  const rank = role => role === 'leader' ? 0 : role === 'dispatcher' ? 1 : 2;
  return [...groups.values()].sort((a, b) => a.name.localeCompare(b.name)).map(({ name, roles }) => {
    const counts = [...roles].sort(([a], [b]) => rank(a) - rank(b) || a.localeCompare(b)).map(([role, count]) => {
      if (role === 'leader' && count === 1) return 'leader';
      return `${count} ${role}${count === 1 ? '' : 's'}`;
    });
    return `${name}: ${counts.join(', ')}`;
  });
}
export const impactText = affected => (affected.length ? `Switching will affect ${impactGroups(affected).join('; ')}.` : 'No affected agents.');

// A provider can be switched on once it has credentials, or if it is one of the two the server can read itself.
export const canEnable = (provider, configured) => provider.enabled || configured || ['codex', 'claude-code'].includes(provider.provider_name);
export const providerConfigured = provider => !!(provider.token_configured || provider.cookie_configured || provider.api_key_configured);

// Moves a provider one place in the order: the whole new order, or null when it cannot move.
export function movedOrder(providers, index, delta) {
  if (index + delta < 0 || index + delta >= providers.length) return null;
  const keys = providers.map(item => item.provider_name);
  [keys[index], keys[index + delta]] = [keys[index + delta], keys[index]];
  return keys;
}

export const usageSettingsBody = form => ({ refresh_interval_sec: Number(form.interval), percent_display: form.display, alert_warning_remaining: Number(form.warning), alert_critical_remaining: Number(form.critical) });
export const refreshMessage = error => (error.status === 429 && error.retryAfter ? `Refreshed moments ago; try again in ${error.retryAfter}s` : error.message);

export const WARMUP_MODES = [['off', 'Off'], ['force', 'Warm every idle account'], ['scheduled', 'Scheduled'], ['smart', 'Spread across five hours']];
export const warmupBody = (mode, settings) => ({ warmup_mode: mode, warmup_schedule: mode === 'scheduled' ? (settings?.warmup_schedule || '0 9 * * *') : (settings?.warmup_schedule || '') });
export const warmupDisabled = settings => !settings?.warmup_available && (settings?.warmup_mode || 'off') === 'off';

export const tokenAutoRefreshOn = settings => !!settings?.token_auto_refresh;
export const tokenAutoRefreshAvailable = settings => !!settings?.token_auto_refresh_available;
export const tokenStatesPending = usage => Object.values(usage?.settings?.token_auto_refresh_state || {}).some(item => item?.status === 'pending');
export function tokenStateLine(tool, state) {
  const status = ['migrated', 'waiting', 'pending', 'n/a'].includes(state?.status) ? state.status : 'off';
  return `${tool[0].toUpperCase()}${tool.slice(1)}: ${status}${status === 'waiting' && state?.reason ? `: ${state.reason}` : ''}`;
}

export const alertKind = kind => String(kind || '').replace(/[^a-z_]/g, '');

export const ollamaBody = (mode, cookie, apiKey) => {
  const body = { auth_mode: mode };
  if (cookie.trim()) body.cookie = cookie;
  if (apiKey.trim()) body.api_key = apiKey;
  return body;
};

export const flowText = flow => `Code: ${flow.user_code} · ${flow.status || 'Waiting for GitHub'}`;
// Seconds to wait before asking GitHub again.
export const pollDelay = (result, flow) => Math.max(1, result.retry_after || result.interval || flow.interval || 5);
export const flowExpired = (flow, now = Date.now()) => now >= new Date(flow.expires_at).getTime();

// Why a profile is not refreshing, and when it tries again when that is known, from the server's error_kind and retry_at (an admin's response has no error text).
export function staleReason(profile, time) {
  const kind = profile.error_kind || (profile.status === 'token_expired' ? 'token_expired' : '');
  if (kind !== 'rate_limited' && kind !== 'fetch_failed') return '';
  const at = profile.retry_at ? new Date(profile.retry_at) : null;
  const retry = at && !Number.isNaN(at.getTime()) ? `, retrying at ${time(at)}` : '';
  return `${kind === 'rate_limited' ? 'Rate limited by the provider' : 'Last fetch failed'}${retry}`;
}

export function profileStatusLine(profile, time) {
  const refreshed = profile.last_refresh_at ? `Updated ${time(new Date(profile.last_refresh_at))}` : '';
  const detail = profile.status === 'token_expired' ? 'Stale – token expired' : profile.stale ? 'Stale snapshot' : profile.status === 'ok' ? '' : (profile.status || 'No snapshot').replaceAll('_', ' ');
  return detail && refreshed ? `${detail} · ${refreshed}` : detail + refreshed;
}

// How a profile's Switch button looks and whether it can be pressed.
export function switchView({ switchable, enabled, owner, configured, profile, hasActive, eligible }) {
  const hidden = !switchable || !enabled || !owner || !configured || !!profile.active;
  const disabled = !hasActive || (!eligible?.eligible && !eligible?.absent);
  return { hidden, disabled, title: !hasActive ? 'Import the current login first' : disabled ? (eligible?.reason || 'Tool is not eligible') : '' };
}

import { request } from './api.js';

export class HttpUsagesSource {
  list() { return request('/api/usages'); }
  refresh(provider = '') { return request('/api/usages/refresh', { method: 'POST', body: { provider } }); }
  settings(body) { return request('/api/usages/settings', { method: 'POST', body }); }
  provider(provider, enabled) { return request('/api/usages/providers', { method: 'POST', body: { provider, enabled } }); }
  mistral(cookie) { return request('/api/usages/mistral/config', { method: 'POST', body: { cookie } }); }
  ollama(body) { return request('/api/usages/ollama/config', { method: 'POST', body }); }
  copilotStart(enterprise_host) { return request('/api/usages/copilot/login/start', { method: 'POST', body: { enterprise_host } }); }
  copilotPoll(flow_id) { return request('/api/usages/copilot/login/poll', { method: 'POST', body: { flow_id } }); }
  copilotLogout() { return request('/api/usages/copilot/login/logout', { method: 'POST', body: {} }); }
  switcher() { return request('/api/switcher'); }
  enable(enabled) { return request('/api/switcher', { method: 'PUT', body: { enabled } }); }
  add(tool, name) { return request(`/api/switcher/${tool}/profiles`, { method: 'POST', body: { name } }); }
  import(tool, name) { return request(`/api/switcher/${tool}/import`, { method: 'POST', body: { name } }); }
  use(tool, name) { return request(`/api/switcher/${tool}/use`, { method: 'POST', body: { name } }); }
  credentialTargets() { return request('/api/credentials/targets'); }
  remove(tool, name) { return request(`/api/switcher/${tool}/profiles/${encodeURIComponent(name)}`, { method: 'DELETE' }); }
  rename(tool, name, newName) { return request(`/api/switcher/${tool}/profiles/${encodeURIComponent(name)}/rename`, { method: 'POST', body: { name: newName } }); }
}

export class MockUsagesSource {
  constructor() { this.renamed = {}; this.flags = { 'github-copilot': {}, mistral: {}, 'ollama-cloud': {} }; this.preferences = { percent_display: 'left', refresh_interval_sec: 120 }; }
  async refresh() { return this.list(); }
  async use(tool, name) { (this.activeProfile ||= {})[tool] = name; this.used = { tool, name }; return {}; }
  // Token state of the stored profiles, as the server reports it: one healthy, one expiring, one expired.
  async credentialTargets() {
    const at = seconds => new Date(Date.now() + seconds * 1000).toISOString();
    return { enabled: true, alerts: [], targets: [
      { tool: 'codex', profile: 'main', role: 'store', active: true, expires_at: at(172800), seconds_left: 172800, level: 'ok' },
      { tool: 'codex', profile: 'backup', role: 'store', expires_at: at(3000), seconds_left: 3000, level: 'expiring' },
      { tool: 'claude', profile: 'local', role: 'store', active: true, expires_at: at(-600), seconds_left: -600, level: 'expired' },
    ] };
  }
  async settings(body) { Object.assign(this.preferences, body); return this.list(); }
  async provider(provider, enabled) { this.flags[provider] ||= {}; this.flags[provider].enabled = enabled; return this.list(); }
  async mistral(cookie) { this.flags.mistral = { enabled: !!cookie, cookie_configured: !!cookie }; return this.list(); }
  async ollama(body) {
    const current = this.flags['ollama-cloud'];
    if (body.cookie !== undefined) current.cookie_configured = !!body.cookie;
    if (body.api_key !== undefined) current.api_key_configured = !!body.api_key;
    if (body.auth_mode !== undefined) current.auth_mode = body.auth_mode;
    current.enabled = !!(current.cookie_configured || current.api_key_configured);
    return this.list();
  }
  async copilotStart() { return { flow_id: 'mock-flow', user_code: 'ABCD-1234', verification_uri: 'https://github.com/login/device', interval: 1, expires_at: new Date(Date.now() + 60000).toISOString() }; }
  async copilotPoll() { this.flags['github-copilot'] = { enabled: true, token_configured: true }; return { status: 'success' }; }
  async copilotLogout() { this.flags['github-copilot'] = {}; return this.list(); }
  async rename(tool, name, newName) { this.renamed[`${tool}/${name}`] = newName; return { name: newName }; }
  profileName(tool, name) { return this.renamed[`${tool}/${name}`] || name; }
  async switcher() {
    const state = this.switcherState();
    for (const profile of state.profiles) profile.name = this.profileName(profile.tool, profile.name);
    return state;
  }
  switcherState() {
    return { enabled: true, owner: true, profiles: [
      { tool: 'codex', name: 'main', active: true, has_credentials: true },
      { tool: 'codex', name: 'backup', active: false, has_credentials: true },
      { tool: 'claude', name: 'local', active: true, has_credentials: true },
    ], eligibility: [{ tool: 'codex', eligible: true }, { tool: 'claude', eligible: true }],
      affected_agents: { codex: [{ id: 'milo@mock', slug: 'milo', tool: 'codex', desired: 'active', observed: 'idle', running: false }], claude: [] } };
  }
  async list() {
    const response = this.usageList();
    for (const provider of response.providers) {
      const tool = provider.provider_name === 'claude-code' ? 'claude' : provider.provider_name;
      const chosen = this.activeProfile?.[tool];
      if (chosen) for (const profile of provider.profiles) profile.active = profile.profile_name === chosen;
    }
    for (const provider of response.providers) {
      const tool = provider.provider_name === 'claude-code' ? 'claude' : provider.provider_name;
      for (const profile of provider.profiles) profile.profile_name = this.profileName(tool, profile.profile_name);
    }
    return response;
  }
  // Evenly spread samples over the last 40 hours, newest last, shaped like the
  // server's window history ({t: unix seconds, p: percent used}).
  mockHistory(values) {
    const step = (40 * 3600) / (values.length - 1), start = Math.floor(Date.now() / 1000) - 40 * 3600;
    return values.map((p, i) => ({ t: Math.round(start + i * step), p }));
  }
  usageList() {
    const reset = hours => new Date(Date.now() + hours * 3600000).toISOString();
    return {
      switcher_enabled: true,
      credential_alerts: this.preferences.token_auto_refresh ? [{ kind: 'expiring', tool: 'claude', role: 'live', message: 'claude live: the access token expires in 4 min.' }] : undefined,
      settings: {
        ...this.preferences,
        token_auto_refresh_available: true,
        token_auto_refresh_state: { claude: { status: this.preferences.token_auto_refresh ? 'migrated' : 'off' }, codex: { status: this.preferences.token_auto_refresh ? 'waiting' : 'off', reason: this.preferences.token_auto_refresh ? 'codex is running' : undefined } },
      },
      providers: [
        { provider_name: 'codex', label: 'Codex', order_number: 0, enabled: true, available: true, profiles: [
          { profile_name: 'main', active: true, has_credentials: true, email: 'main@example.test', plan: 'Pro', status: 'ok', last_refresh_at: new Date().toISOString(), credits: { label: 'Credits', balance: 12.5 }, windows: [
            { key: 'session', percent_used: 42, reset_at: reset(2), history: this.mockHistory([60, 20, 25, 33, 42]), pace: { expected_percent: 55, delta_percent: -13, state: 'reserve', lasts_to_reset: true } },
            { key: 'weekly', percent_used: 64, reset_at: reset(72), pace: { expected_percent: 50, delta_percent: 14, state: 'deficit', eta_seconds: 86400 } },
            { key: 'codex_spark', percent_used: 7, reset_at: reset(3) },
          ] },
          { profile_name: 'backup', active: false, has_credentials: true, email: 'backup@example.test', plan: 'Plus', status: 'ok', windows: [
            { key: 'session', percent_used: 18, reset_at: reset(4) },
          ] },
        ] },
        { provider_name: 'claude-code', label: 'Claude Code', order_number: 1, enabled: true, available: true, profiles: [
          { profile_name: 'local', active: true, has_credentials: true, plan: 'Max', status: 'ok', windows: [
            { key: 'session', percent_used: 31, reset_at: reset(3) }, { key: 'weekly', percent_used: 53, reset_at: reset(100) },
          ] },
        ] },
        { provider_name: 'github-copilot', label: 'GitHub Copilot', order_number: 2, enabled: false, available: true, ...this.flags['github-copilot'], profiles: [
          { profile_name: 'local', active: true, has_credentials: false, status: 'not_configured' },
        ] },
        { provider_name: 'mistral', label: 'Mistral', order_number: 3, enabled: false, available: true, ...this.flags.mistral, profiles: [
          { profile_name: 'local', active: true, has_credentials: !!this.flags.mistral.cookie_configured, status: 'not_configured' },
        ] },
        { provider_name: 'ollama-cloud', label: 'Ollama Cloud', order_number: 4, enabled: false, available: true, ...this.flags['ollama-cloud'], profiles: [
          { profile_name: 'local', active: true, has_credentials: !!(this.flags['ollama-cloud'].cookie_configured || this.flags['ollama-cloud'].api_key_configured), status: 'not_configured' },
        ] },
      ],
    };
  }
}

export function createUsagesSource() { return new URLSearchParams(location.search).has('mock_usages') ? new MockUsagesSource() : new HttpUsagesSource(); }

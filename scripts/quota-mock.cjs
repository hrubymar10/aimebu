// A Node-side stand-in for the quota switcher API (docs/quota-switcher.md), used only by the browser tests: the UI smoke
// answers /api/quota-switcher* with it through request interception, and the static fixture server does the same. It follows
// the server's JSON shape and validation, so the production Settings code is exercised over plain HTTP with no test hooks.
'use strict';

const LIMITS = { session_max: [1, 100], weekly_max: [1, 100], drop_points: [1, 100], warmup_seconds: [0, 600], min_dwell_minutes: [0, 120] };

class QuotaMock {
  constructor() {
    this.names = { claude: ['local', 'spare', 'parked'], codex: ['main', 'backup'] };
    this.tools = {};
    for (const tool of ['claude', 'codex']) this.tools[tool] = { config: this.defaults(tool), saved: false, state: this.freshState(), last_decision: null, decisions: [] };
    this.tools.claude.state.reset_latched_at = new Date(Date.now() - 3600000).toISOString();
    this.tools.claude.state.weekly_max_seen = 91;
    this.tools.claude.state.hold = 'warming up';
    this.tools.claude.last_decision = this.decision('claude', 'hold', 'local', null, 'warming up');
    this.tools.claude.decisions = [this.tools.claude.last_decision, this.decision('claude', 'switch', 'local', 'spare', 'active over weekly limit', true)];
  }
  freshState() { return { reset_latched_at: null, weekly_max_seen: null, last_switch_at: null, tripped: false, tripped_at: null, hold: null, evaluated_at: new Date().toISOString() }; }
  defaults(tool) {
    return { enabled: false, profiles: this.names[tool].filter(name => name !== 'parked').map(name => ({ name, session_max: 95, weekly_max: 95 })), protected: tool === 'claude' ? ['parked'] : [], after_reset: null, warmup_seconds: 60, min_dwell_minutes: 5 };
  }
  decision(tool, action, from, to, reason, dry_run = false) {
    return { at: new Date().toISOString(), tool, action, from, to, reason, dry_run, profiles: { [from]: { status: 'ok', session: 42, weekly: 96, ok_for_seconds: 300 }, ...(to ? { [to]: { status: 'ok', session: 10, weekly: 12, ok_for_seconds: 300 } } : {}) } };
  }
  copy(tool) { return JSON.parse(JSON.stringify(this.tools[tool])); }

  validate(tool, config) {
    const fail = message => { const error = new Error(message); error.status = 400; throw error; };
    const known = new Set(this.names[tool]);
    const check = (key, value) => { const [low, high] = LIMITS[key]; if (!Number.isInteger(value) || value < low || value > high) fail(`${key} must be a whole number from ${low} to ${high}`); };
    for (const entry of [...config.profiles, ...(config.after_reset?.profiles || [])]) {
      if (!known.has(entry.name)) fail(`unknown profile ${entry.name}`);
      check('session_max', entry.session_max); check('weekly_max', entry.weekly_max);
    }
    if (new Set(config.profiles.map(entry => entry.name)).size !== config.profiles.length) fail('a profile is listed twice');
    if (config.profiles.some(entry => config.protected.includes(entry.name))) fail('a profile cannot be both listed and protected');
    if (config.enabled && config.profiles.length < 2) fail('at least two profiles are needed to enable switching');
    if (config.enabled && config.after_reset && config.after_reset.profiles.length < 2) fail('after_reset needs at least two profiles to enable automatic switching');
    if (config.after_reset) check('drop_points', config.after_reset.drop_points);
    check('warmup_seconds', config.warmup_seconds); check('min_dwell_minutes', config.min_dwell_minutes);
  }

  // handle(method, pathname, body) -> { status, body }, like the server's routes.
  handle(method, pathname, body) {
    const parts = pathname.replace(/^\/api\/quota-switcher\/?/, '').split('/').filter(Boolean);
    if (!parts.length) {
      if (method !== 'GET') return { status: 405, body: { message: 'method not allowed' } };
      return { status: 200, body: { profiles: JSON.parse(JSON.stringify(this.names)), tools: { claude: this.copy('claude'), codex: this.copy('codex') } } };
    }
    const tool = parts[0];
    if (!this.tools[tool]) return { status: 404, body: { message: 'unknown tool' } };
    const current = this.tools[tool];
    if (parts.length === 1 && method === 'PUT') {
      try { this.validate(tool, body); } catch (error) { return { status: error.status || 500, body: { message: error.message } }; }
      current.config = JSON.parse(JSON.stringify(body)); current.saved = true;
      return { status: 200, body: this.copy(tool) };
    }
    if (parts[1] === 'rearm' && method === 'POST') { current.state.reset_latched_at = null; current.state.weekly_max_seen = null; return { status: 200, body: this.copy(tool) }; }
    if (parts[1] === 'reenable' && method === 'POST') { current.state.tripped = false; current.state.tripped_at = null; return { status: 200, body: this.copy(tool) }; }
    return { status: 404, body: { message: 'not found' } };
  }
}

module.exports = { QuotaMock };

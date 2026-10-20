// A Node-side stand-in for the limit guard API (docs/quota-switcher.md#limit-guard-acting-before-100), used only by the
// browser tests: the UI smoke answers /api/limit-guard* with it through request interception, and the static fixture
// server does the same. It follows the server's JSON shape and validation.
'use strict';

class LimitGuardMock {
  constructor() {
    this.calls = 0; this.forbidden = false; // GET /api/limit-guard requests so far, and whether to answer them as a non-owner would
    this.names = { claude: ['local', 'spare', 'parked'], codex: ['main', 'backup'] };
    this.tools = {
      claude: { config: this.defaults('claude'), saved: false, result: { state: 'soft', profile: 'local', percent: 96, window: 'weekly', soft: 95, hard: 99 }, held: true, since: new Date(Date.now() - 600000).toISOString() },
      codex: { config: this.defaults('codex'), saved: false, result: { state: 'ok', profile: 'main', percent: 40, window: 'session', soft: 98, hard: 99 }, held: false, since: null },
    };
  }
  defaults(tool) { return { enabled: true, soft: tool === 'codex' ? 98 : 95, hard: 99, profiles: {} }; }
  copy(tool) { return JSON.parse(JSON.stringify(this.tools[tool])); }
  validate(tool, config) {
    const fail = message => { const error = new Error(message); error.status = 400; throw error; };
    const check = (name, limit) => {
      if (![limit.soft, limit.hard].every(value => Number.isInteger(value) && value >= 1 && value <= 100)) fail(`${name}: thresholds must be between 1 and 100`);
      if (!(limit.soft < limit.hard)) fail(`${name}: the soft threshold must be below the hard one`);
    };
    check('limit guard', config);
    for (const [name, limit] of Object.entries(config.profiles || {})) { if (!this.names[tool].includes(name)) fail(`limit guard overrides "${name}", which is not a profile`); check(name, limit); }
  }
  // handle(method, pathname, body) -> { status, body }, like the server's routes.
  handle(method, pathname, body) {
    const parts = pathname.replace(/^\/api\/limit-guard\/?/, '').split('/').filter(Boolean);
    if (!parts.length) {
      if (method !== 'GET') return { status: 405, body: { message: 'method not allowed' } };
      this.calls++;
      if (this.forbidden) return { status: 403, body: { message: 'owner only' } };
      return { status: 200, body: { profiles: JSON.parse(JSON.stringify(this.names)), tools: { claude: this.copy('claude'), codex: this.copy('codex') } } };
    }
    const tool = parts[0];
    if (!this.tools[tool]) return { status: 404, body: { message: 'unknown tool' } };
    if (parts.length === 1 && method === 'PUT') {
      try { this.validate(tool, body); } catch (error) { return { status: error.status || 500, body: { message: error.message } }; }
      this.tools[tool].config = JSON.parse(JSON.stringify(body)); this.tools[tool].saved = true;
      return { status: 200, body: this.copy(tool) };
    }
    return { status: 404, body: { message: 'not found' } };
  }
}

module.exports = { LimitGuardMock };

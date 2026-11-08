// Error and performance reporting to Sentry. Nothing here runs unless the server says it is on: GET /api/buildinfo carries a `sentry`
// object only while reporting is on (a DSN from AIMEBU2_SENTRY_DSN or Settings, after the owner answered the popup), and only then is the
// vendored SDK (web/vendor/sentry/) fetched. Otherwise the page never requests the file and never opens a connection to Sentry. docs/telemetry.md lists what is sent.
//
// What may leave the browser is decided here, in the scrub functions below, and they run on every event, transaction and breadcrumb
// (beforeSend, beforeSendTransaction, beforeBreadcrumb). The server side has the same rules (internal/telemetry/scrub.go).

export const SDK_URL = '/vendor/sentry/bundle.tracing.min.js';

const MAX_TEXT = 300;
const BEARER = /\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+/gi;
const SECRET = /\b([A-Za-z0-9_-]*(?:token|secret|password|passwd|cookie|authorization|api[_-]?key|dsn)[A-Za-z0-9_-]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)/gi;
const HOME = /(?:\/Users|\/home)\/[^/\s]+/g;
// The bare tokens aimebu and its providers issue (agent keys, machine keys, sessions, API keys).
const BARE_TOKEN = /\b(?:aak_|amk_|as_|sk-ant-|sk-)[A-Za-z0-9_-]{8,}/g;

// The path of a URL: no origin, user info, query or fragment. Not a URL gives ''.
export function scrubUrl(raw) {
  if (typeof raw !== 'string' || !raw) return '';
  try { return new URL(raw, 'http://placeholder.invalid').pathname; } catch { return ''; }
}

// One free-text value: home directories as "~", secrets redacted, length bounded.
export function scrubText(text) {
  if (typeof text !== 'string') return '';
  let out = text.replace(HOME, '~').replace(BARE_TOKEN, '[redacted]').replace(BEARER, '$1 [redacted]').replace(SECRET, '$1$2[redacted]');
  if (out.length > MAX_TEXT) out = `${out.slice(0, MAX_TEXT)}…`;
  return out;
}

// A URL as a route: no host, query or fragment, and no name a person chose. Under /api only the resource is kept ("/api/rooms/…"), elsewhere only
// the first segment; the ids and names after it never leave the browser.
export function routeOf(raw) {
  const path = scrubUrl(raw);
  const segments = path.split('/').filter(Boolean);
  if (!segments.length) return '/';
  const keep = segments[0] === 'api' ? 2 : 1;
  return `/${segments.slice(0, keep).join('/')}${segments.length > keep ? '/…' : ''}`;
}

// The operations of the spans the browser makes for every file a page loads.
export const RESOURCE_OPS = ['resource.script', 'resource.css', 'resource.img', 'resource.link', 'resource.iframe', 'resource.other', 'resource.audio', 'resource.video', 'resource.track', 'resource.fetch', 'resource.xmlhttprequest'];
// The page load's own timing spans are kept only where they tell the server's wait from the download (browser.request, browser.response): the rest
// (cache, dns, connect, the unload / DOM / load events, paint) are ten more spans a load, and the paints and the vitals are measurements of the
// transaction anyway.
const KEPT_BROWSER_SPANS = new Set(['browser.request', 'browser.response']);
export const isNoiseSpan = span => typeof span?.op === 'string' && (span.op.startsWith('resource.') || (span.op.startsWith('browser.') && !KEPT_BROWSER_SPANS.has(span.op)));

// The span data that may leave: the method, the status and a route. No host (server.address), no query, no fragment.
const SPAN_DATA = new Set(['http.request.method', 'http.response.status_code', 'http.route', 'http.method', 'http.status_code', 'url', 'url.path', 'type', 'size', 'duration', 'sentry.op', 'sentry.origin', 'sentry.source']);

function scrubFrames(frames) {
  for (const frame of frames || []) {
    if (typeof frame.filename === 'string') frame.filename = scrubUrl(frame.filename) || scrubText(frame.filename);
    if (typeof frame.abs_path === 'string') frame.abs_path = scrubUrl(frame.abs_path) || scrubText(frame.abs_path);
    delete frame.vars; delete frame.pre_context; delete frame.context_line; delete frame.post_context;
  }
}

// BeforeSend and BeforeSendTransaction: the event with everything but the allowed fields taken out. Returns the same object.
export function scrubEvent(event) {
  if (!event || typeof event !== 'object') return null;
  // Nobody is identified and no address is inferred: the SDK says infer_ip "never" on every event, and this makes it so whatever the SDK did.
  // The id is this person's fingerprint (or their login, when they chose that): see docs/telemetry.md. Nothing else of a user goes out.
  const userID = typeof event.user?.id === 'string' && event.user.id ? event.user.id.slice(0, 64) : undefined;
  event.user = { ...(userID ? { id: userID } : {}), ip_address: null };
  event.sdk = { ...event.sdk, settings: { ...event.sdk?.settings, infer_ip: 'never' } };
  delete event.server_name; delete event.extra; delete event.modules;
  if (event.request) event.request = { url: routeOf(event.request.url), ...(event.request.method ? { method: event.request.method } : {}) };
  if (event.contexts) for (const name of Object.keys(event.contexts)) if (!['trace', 'os', 'browser'].includes(name)) delete event.contexts[name];
  // The root span's data sits in the trace context, not in event.spans: the same allow-list (this is where url.full and the device's memory and cores were).
  const root = event.contexts?.trace?.data;
  if (root && typeof root === 'object') for (const key of Object.keys(root)) {
    if (!SPAN_DATA.has(key)) delete root[key];
    else if (key === 'url' || key === 'url.path') root[key] = routeOf(String(root[key]));
  }
  if (typeof event.message === 'string') event.message = scrubText(event.message);
  if (typeof event.transaction === 'string') event.transaction = event.transaction.startsWith('/') || /^https?:/.test(event.transaction) ? routeOf(event.transaction) : scrubText(event.transaction);
  for (const exception of event.exception?.values || []) {
    if (typeof exception.value === 'string') exception.value = scrubText(exception.value);
    scrubFrames(exception.stacktrace?.frames);
  }
  scrubFrames(event.stacktrace?.frames);
  if (Array.isArray(event.breadcrumbs)) event.breadcrumbs = event.breadcrumbs.map(scrubBreadcrumb).filter(Boolean);
  if (event.tags) delete event.tags.server_name;
  if (Array.isArray(event.spans)) event.spans = event.spans.filter(span => !isNoiseSpan(span));
  for (const span of event.spans || []) {
    if (typeof span.description === 'string') { // "GET https://host/a?q=1" keeps only the path
      const request = /^([A-Z]+) (\S+)$/.exec(span.description);
      span.description = request ? `${request[1]} ${routeOf(request[2])}` : /^(https?:\/\/|\/)\S*$/.test(span.description) ? routeOf(span.description) : scrubText(span.description);
    }
    if (span.data) for (const key of Object.keys(span.data)) {
      if (!SPAN_DATA.has(key)) delete span.data[key];
      else if (key === 'url' || key === 'url.path') span.data[key] = routeOf(String(span.data[key]));
    }
    delete span.tags;
  }
  return event;
}

// BeforeBreadcrumb: the browser's console, clicks and typing (their text is page content) are dropped; a request keeps its method, path and
// status; everything else keeps a scrubbed message and no data.
export function scrubBreadcrumb(crumb) {
  if (!crumb || typeof crumb !== 'object') return null;
  if (crumb.category === 'console' || String(crumb.category || '').startsWith('ui.')) return null;
  const data = crumb.data || {};
  const kept = {};
  if (crumb.category === 'fetch' || crumb.category === 'xhr') {
    if (data.method) kept.method = String(data.method);
    if (data.status_code !== undefined) kept.status_code = data.status_code;
    if (data.url) kept.url = routeOf(String(data.url));
  } else if (crumb.category === 'navigation') {
    if (data.from) kept.from = routeOf(String(data.from));
    if (data.to) kept.to = routeOf(String(data.to));
  }
  crumb.data = Object.keys(kept).length ? kept : undefined;
  if (typeof crumb.message === 'string') crumb.message = scrubText(crumb.message);
  return crumb;
}

// The options Sentry.init takes for a server-provided config ({dsn, environment, release, *_sample_rate}).
export function sentryOptions(config, Sentry) {
  return {
    dsn: config.dsn, environment: config.environment, release: config.release,
    sendDefaultPii: false,
    // sendDefaultPii does nothing in this SDK version: what it collects is `dataCollection`, whose defaults are all on (IP address, user agent,
    // headers, cookies, query strings, bodies). Everything is switched off.
    dataCollection: { userInfo: false, cookies: false, httpHeaders: false, httpBodies: [], urlQueryParams: false, graphQL: { document: false, variables: false },
      genAI: { inputs: false, outputs: false }, mcp: { inputs: false, outputs: false }, databaseQueryData: false, queues: false, stackFrameVariables: false, frameContextLines: 0 },
    // The default "stream" lifecycle sends spans one by one, past beforeSendTransaction; "static" sends a transaction, which the scrubber sees.
    traceLifecycle: 'static',
    tracesSampleRate: Number(config.traces_sample_rate) || 0,
    // Never attach trace headers to a request: they would add a header to calls the server does not read.
    tracePropagationTargets: [],
    // No release-health sessions (the SDK's session envelope asks Sentry to infer the IP address with "{{auto}}"), no client reports.
    sendClientReports: false,
    integrations: defaults => [
      ...defaults.filter(integration => integration.name !== 'BrowserSession'),
      // Page load and navigation, the API requests and the Web Vitals. Not one span per loaded file (script, stylesheet, image, ...): they were most of
      // a page load's spans (about 240) and say nothing about this app; scrubEvent drops any that get through anyway.
      Sentry.browserTracingIntegration({ ignoreResourceSpans: RESOURCE_OPS }),
    ],
    // A call that found the session gone (marked `unauthorized` by api.js) is the page going to sign-in, not an error to report, whatever listener saw it first.
    beforeSend: (event, hint) => (hint?.originalException?.unauthorized ? null : scrubEvent(event)),
    beforeSendTransaction: scrubEvent,
    beforeBreadcrumb: scrubBreadcrumb,
  };
}

function loadScript(url) {
  return new Promise((resolve, reject) => {
    const script = document.createElement('script');
    script.src = url; script.async = true;
    script.onload = resolve; script.onerror = () => reject(new Error(`could not load ${url}`));
    document.head.append(script);
  });
}

let started = false;

// Starts reporting when `config` (the `sentry` object of /api/buildinfo) has a DSN; with none, it does nothing at all (the SDK file is never
// requested). Resolves true once the SDK is running. It never throws: reporting being broken must not break the page. `deps` is for tests.
export async function startTelemetry(config, deps = {}) {
  if (started || !config || typeof config.dsn !== 'string' || !config.dsn) return false;
  started = true;
  try {
    await (deps.load || loadScript)(SDK_URL);
    const Sentry = deps.Sentry ? deps.Sentry() : globalThis.Sentry;
    if (!Sentry) return false;
    Sentry.init(sentryOptions(config, Sentry));
    if (typeof config.user_id === 'string' && config.user_id) Sentry.setUser({ id: config.user_id });
    return true;
  } catch { return false; }
}

// Changes who the reports name (after the person re-rolled their fingerprint or chose to send their login) without a reload. Nothing happens while
// reporting is not running.
export function setTelemetryUser(id, Sentry = globalThis.Sentry) {
  if (!started || typeof Sentry?.setUser !== 'function') return;
  Sentry.setUser(typeof id === 'string' && id ? { id } : null);
}

// Test seam: lets a test start again.
export function resetTelemetryForTest() { started = false; }

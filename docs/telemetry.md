# Telemetry (Sentry)

aimebu can report crashes, errors and performance traces to [Sentry](https://sentry.io) (or a self-hosted Sentry), from the server and
from the web UI. It reports only while a DSN is in effect (from the environment or Settings, and not before the owner has answered a one-time
popup, below). With no DSN in effect nothing is initialized, nothing is sent, and the browser never even requests the
Sentry SDK file.

## Turning it on, off, and where it is decided

Reporting is decided in this order:

1. **`AIMEBU2_SENTRY_DSN` in the server's environment wins.** A DSN turns reporting on with that project; `off` (or an empty value)
   turns it off. When it is set, Settings cannot change it and no popup is shown.
2. Otherwise **Settings → Telemetry** decides: a toggle and a DSN field (the same two fields as the popup below). The
   field is always filled with the effective DSN: the one you chose, else the default this build ships (if any). A "Reset to default DSN" icon
   button to the left of the field puts the default back (it is off while the field already holds it, and absent when the build ships no
   default). Turning telemetry off disables the field and the button; it does not hide them, and an off save stores only the switch. A save applies to
   the server at once, with no restart; a page already open starts or stops its own reporting after a reload. Only the owner and
   admins can change it.
3. **Nothing is reported until the owner has answered the popup.** On a new install the owner sees a one-time "Telemetry" popup
   after the welcome, and on the first sign-in after an update that brought telemetry. It holds the toggle (on when this build
   ships a default DSN, otherwise off), the DSN field (filled in, with the same reset button; greyed out while the toggle is off), and one sentence on what is and is not sent. **Save** keeps what is shown; **Not
   now** (or Escape) turns reports off. Either way it is not asked again.

This build ships the maintainers' DSN as the default (`internal/telemetry/default_dsn.go`, EU ingest host `ingest.de.sentry.io`), so the popup's toggle starts on with it filled in. Deleting that one file gives a build with no default, which is off until a DSN comes from the environment or Settings. Set `AIMEBU2_SENTRY_DSN=off` to opt out for a whole server.

Other variables (all optional):

| Variable | Meaning | Default |
|---|---|---|
| `AIMEBU2_SENTRY_DSN` | A DSN, or `off`/empty to disable; overrides Settings. | unset |
| `AIMEBU2_SENTRY_ENV` | The Sentry environment name. | `dev` |
| `AIMEBU2_SENTRY_TRACES_SAMPLE_RATE` | Share of requests traced (server) and of page loads traced (browser), 0 to 1. Settings → Telemetry shows the value in effect. | `0.5` |

A DSN must be https (http only for a Sentry on this machine: localhost, 127.x, ::1); anything else is refused. A rate outside 0 to 1 (or not a number) is reported on stderr and the process runs with telemetry **off**; it never refuses to start
over it. The server says `telemetry on: ...` at startup when it is on. The release is the build's version (`aimebu version`, the same
string the page header shows, with spaces and slashes replaced by `_`).

`aimebu supervisor`, `aimebu agent-tools` and the other subcommands read the environment variables, so a crash in them is reported too
when `AIMEBU2_SENTRY_DSN` is set (they do not read Settings, which live in the server's database).

**Tests never send.** The test environment (`internal/testenv`, `scripts/scratch-server.cjs`, `scripts/test-ui.cjs`) sets
`AIMEBU2_SENTRY_DSN=off`, `testenv.Check` refuses an environment where it is not, and `telemetry.Init` refuses the real network
transport inside a test binary.

## What is sent

**Server (Go)**

- Panics and 5xx answers in the HTTP handlers (each linked to its request's trace): the error text, the stack (function names, file paths with your home directory
  shown as `~`), the request **method and route pattern** (for example `GET /api/projects/{id}`), the status code. A panic is reported
  and then raised again, so the process behaves exactly as it would without telemetry.
- Panics at the entry points of the subcommands (server, supervisor, CLI).
- Traces: one transaction per HTTP request (method, route, status, duration), with one span per SQLite statement made while serving it:
  the SQL text **with its `?` placeholders**, the duration and the number of rows. Never an argument value. Event streams and
  WebSocket connections are not traced (their span would last as long as the connection). At most 50 statements of one request get a span
  (`telemetry.MaxDBSpans`); the transaction then carries `db.calls` (how many ran) and `db.calls_dropped` (how many were left out), so a request that
  asks per row shows up without every iteration costing a span.

**Web UI (browser)**

- Uncaught errors and unhandled promise rejections, with stack frames reduced to the script path.
- Web Vitals and page-load / navigation traces, and the page's own requests as spans (method, route, status). Spans go out inside their
  transaction (`traceLifecycle: 'static'`), so the same scrubber sees them. **No span per loaded file** (script, stylesheet, image: about
  240 per page load), and of the page load's own timing spans only `browser.request` and `browser.response` stay; the vitals are measurements of the
  transaction, not spans.

## Who a report is about

Every account has a **random fingerprint** (16 hex characters from `crypto/rand`, generated the first time it is needed, stored on the account row
as `user.telemetry_fingerprint`, **not** derived from the account id). While telemetry is on, the server's reports and the page's reports carry it
as Sentry `user.id`, and nothing else about the person (no name, e-mail, address or place), so Sentry can count affected people and an
owner can tell their own reports from a tester's.

**Settings → Telemetry** shows every signed-in person their own fingerprint (read-only, with a copy button) and a 🎲 button, "Generate a new
fingerprint", that replaces it with a fresh random one. The switch **"Send my user name instead of a fingerprint"** (per person, off by default)
makes `user.id` the login name instead: never both. Rolling while the switch is on asks first ("Rolling a new fingerprint turns off sending your
user name and gives you a new anonymous id."); confirming turns the switch off and rolls, cancelling changes nothing. A change reaches the server
at once and the page's own SDK without a reload. The DSN and the on/off switch above stay with the owner and the admins.

API: `GET`/`PUT /api/me/telemetry` (`{send_name}`) and `POST /api/me/telemetry/fingerprint`, each answering
`{fingerprint, send_name, login, report_id}`; `GET /api/buildinfo` adds `sentry.user_id` (the `report_id`) for the page.

## Volume

The free Sentry plan counts spans (5 million a month), so the number of spans per use is bounded: half of the requests and page loads are traced by
default (`AIMEBU2_SENTRY_TRACES_SAMPLE_RATE`), a request carries at most 50 database spans, and a sampled page load is a handful of spans. Settings → Telemetry
shows these numbers read-only (`GET /api/telemetry/settings` carries `traces_sample_rate` and `max_db_spans`); the rate is the server's environment's to change.
- **No Session Replay.** The vendored browser bundle is the tracing-only one and contains no Replay code, so it cannot be turned on by a setting. A
  recording of the screen would carry link targets, names in attributes and page URLs that the SDK offers no hook to scrub, so it is left out until
  that can be done (see `web/vendor/README.md`).
- The page learns the DSN, environment and release from `GET /api/buildinfo` after sign-in, so errors before sign-in are not reported.
  The SDK (`web/vendor/sentry/`) is requested only if that answer carries a `sentry` object, which it does only while reporting is on.

## What is never sent

Both sides scrub every event before it is sent, in code that does not depend on the SDK's defaults (`internal/telemetry/scrub.go`,
`web/js/telemetry.js`; both have tests, and the page test checks a real error end to end):

- no message bodies, prompts, attachments, or any request or response body;
- no tokens, passwords, cookies, `Authorization` or any other header, no CSRF token; text that looks like `token=...`, `password: ...`,
  `Bearer ...` is redacted in error messages, and so are aimebu's and the providers' bare tokens (`aak_`, `amk_`, `as_`, `sk-ant-`, `sk-`);
- no environment variable values, no host name, no name, e-mail or IP address and no user agent (the one thing that names a person is the id below): the browser SDK is given an explicit
  `dataCollection` with everything off (its `sendDefaultPii` option does nothing in the vendored version, so it is not relied on), and the
  page test checks the envelopes it actually sends. No address is inferred by Sentry either: every browser event, error or transaction, says
  `sdk.settings.infer_ip: "never"` with an explicitly empty `user.ip_address`, the browser sends **no session** (the SDK's session envelope carries
  `ip_address: "{{auto}}"`, which asks Sentry to infer it) and no client reports, and the page test fails on any `{{auto}}` or on any other item type
  that could carry one. This does not depend on Sentry's own "Prevent Storing of IP Addresses" project setting;
- no URL in the form a person typed it: the server names a request by its **route pattern** (`GET /api/rooms/{room}/messages`), and the
  browser reduces a URL to its resource (`/api/rooms/…`); no host, no query string, no fragment, and no ids or names chosen by a person
  (a profile name is part of some paths). 
- no machine paths: in every stack trace (an exception's, a thread's) the program's own files are shown relative to its source root
  (`internal/server/server.go`), the Go toolchain as `$GOROOT`, and any home directory (this process's `$HOME`, or any `/Users/<name>`,
  `/home/<name>`, `C:\Users\<name>`; the browser too) as `~`, so it does not matter where the binary was built or who runs it;
- no local variables or source lines in stack frames, no console output, no click or typing breadcrumbs;
- free text is cut to 300 characters; span and context data outside an allow-list is dropped.


## Supervisors

A standalone supervisor reports the way the server's settings say. When it connects (`hello_ack`), and again whenever the settings change, the server sends
the effective configuration: whether reporting is on, the DSN, the environment name and the trace sample rate, with no identity of a person (see
[the contract](contract.md)). The supervisor starts, replaces or stops its own reporting by it; its events carry no user, no host name and no IP address,
like the server's. The `aimebu agent-tools` bridge that a harness starts for an agent gets the same through the environment of its MCP config entry
(`AIMEBU2_SENTRY_DSN`, `AIMEBU2_SENTRY_ENV`, `AIMEBU2_SENTRY_TRACES_SAMPLE_RATE`; `AIMEBU2_SENTRY_DSN=off` when reporting is off), never through its
arguments; the files are rewritten when the settings change, so the next bridge an agent starts has them, and one that is already running keeps what it
started with.

A DSN in the supervisor machine's own environment (`AIMEBU2_SENTRY_DSN`, a DSN or `off`) wins over the server's settings, for the supervisor and for its
bridges. A supervisor that predates this ignores the settings (and is not sent the frame); the server's own embedded supervisor is the server's process and
reports through it.

## Self-hosting

The DSN is just a URL, so any Sentry-compatible server works: run [self-hosted Sentry](https://develop.sentry.dev/self-hosted/), create a
project, and use its DSN. Events then never leave your network. Note the page sends its events to the DSN's host **from the browser**,
so that host must be reachable from the browsers that use aimebu, and (for a browser on another origin) allow CORS as Sentry does.
The app page is served with a Content-Security-Policy ([HTTPS](tls.md#content-security-policy)): while reporting is on its `connect-src`
names that DSN's origin (scheme and host), and with reporting off it names no other origin, so a page of a server that does not report may
talk to nobody but the server.

## The vendored SDK

`web/vendor/sentry/bundle.tracing.min.js` is the unmodified Sentry browser SDK bundle with Tracing only, no Replay (MIT); its version,
source and SHA-256 are in `web/vendor/README.md`. It is a plain script, loaded like the other vendored files, with no build step. The Go
side uses `github.com/getsentry/sentry-go` and `.../otel` (MIT) with the OpenTelemetry API and SDK (Apache-2.0); a small span processor in
`internal/telemetry` converts finished OpenTelemetry spans into Sentry transactions.

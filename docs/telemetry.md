# Telemetry (Sentry)

aimebu can report crashes, errors and performance traces to [Sentry](https://sentry.io) (or a self-hosted Sentry), from the server and
from the web UI. It reports only while a DSN is in effect (from the environment or Settings, and not before the owner has answered a one-time
popup, below). With no DSN in effect nothing is initialized, nothing is sent, and the browser never even requests the
Sentry SDK file.

## Turning it on, off, and where it is decided

Reporting is decided in this order:

1. **`AIMEBU_SENTRY_DSN` in the server's environment wins.** A DSN turns reporting on with that project; `off` (or an empty value)
   turns it off. When it is set, Settings cannot change it and no popup is shown.
2. Otherwise **Settings → Server → Telemetry** decides. It is one card: the master switch **"Send telemetry"** first (it applies at once, with no Save),
   then the Sentry DSN field, then the person's own identity (see below). The DSN field is always filled with the effective DSN: the one you chose,
   else the default this build ships (if any); a "Reset to default DSN" icon button to its left puts the default back (off while the field already
   holds it, absent when the build ships no default), and its own **Save** applies a new DSN. **With the switch off, everything under it is disabled
   and muted** (the DSN, reset, Save, the identity field, copy, dice, the name switch and the volume facts), in every theme: dimmed, and in
   high contrast (which keeps disabled controls at full opacity) by the muted colour and dashed borders. A change applies to the server at once,
   with no restart; a page already open starts or stops its own reporting after a reload. Only the owner and admins see the switch, the DSN and the
   volume facts; everyone else sees their identity rows only.
3. **Nothing is reported until the owner has answered the popup.** On a new install the owner sees a one-time "Telemetry" popup
   after the welcome, and on the first sign-in after an update that brought telemetry. It holds the toggle (on when this build
   ships a default DSN, otherwise off), the DSN field (filled in, with the same reset button; greyed out while the toggle is off), the owner's own switch "Send my user name instead of a
   fingerprint" (off by default, greyed out with the DSN while the toggle is off, worded as in Settings → Server → Telemetry), and one sentence on what is and is not sent. **Save** keeps what is shown
   (and, with the switch ticked and reports on, also sets it for the owner's account, as `PUT /api/me/telemetry` with `send_name`; if that call fails the popup comes back with the reason and what was chosen, up to three times); **Not
   now** (or Escape) turns reports off and changes nothing else. Either way it is not asked again.

This build ships the maintainers' DSN as the default (`internal/telemetry/default_dsn.go`, EU ingest host `ingest.de.sentry.io`), so the popup's toggle starts on with it filled in. Deleting that one file gives a build with no default, which is off until a DSN comes from the environment or Settings. Set `AIMEBU_SENTRY_DSN=off` to opt out for a whole server.

Other variables (all optional):

| Variable | Meaning | Default |
|---|---|---|
| `AIMEBU_SENTRY_DSN` | A DSN, or `off`/empty to disable; overrides Settings. | unset |
| `AIMEBU_SENTRY_ENV` | The Sentry environment name. A normal server run (including `bin/aimebu server`) reports `production`; `make dev` sets `development` itself, and also sets the DSN to `off` (a sandbox run never reports unless `AIMEBU_DEV_SENTRY_DSN` is set explicitly). | `production` |
| `AIMEBU_SENTRY_TRACES_SAMPLE_RATE` | Share of requests traced (server) and of page loads traced (browser), 0 to 1. Settings → Server → Telemetry shows the value in effect. | `1` |

A DSN must be https (http only for a Sentry on this machine: localhost, 127.x, ::1); anything else is refused. A rate outside 0 to 1 (or not a number) is reported on stderr and the process runs with telemetry **off**; it never refuses to start
over it. The server says `telemetry on: ...` at startup when it is on. The release is the build's version (`aimebu version`, the same
string the page header shows, with spaces and slashes replaced by `_`).

`aimebu supervisor`, `aimebu agent-tools` and the other subcommands read the environment variables, so a crash in them is reported too
when `AIMEBU_SENTRY_DSN` is set (they do not read Settings, which live in the server's database).

**Tests never send.** The test environment (`internal/testenv`, `scripts/scratch-server.cjs`, `scripts/test-ui.cjs`) sets
`AIMEBU_SENTRY_DSN=off`, `testenv.Check` refuses an environment where it is not, and `telemetry.Init` refuses the real network
transport inside a test binary.

## What is sent

**Server (Go)**

- Panics and 5xx answers in the HTTP handlers (each linked to its request's trace): the underlying error and its wrapped causes for a handled internal failure (the scrubbed error chain is also written to the server log), or a generic status report when no error is available; the stack (function names, file paths with your home directory
  shown as `~`), the request **method and route pattern** (for example `GET /api/projects/{id}`), the status code. A panic is reported
  and then raised again, so the process behaves exactly as it would without telemetry.
- Panics at the entry points of the subcommands (server, supervisor, CLI).
- Panics in the long-lived background goroutines (the dispatcher, the supervisor's connection loops and warm/keep-alive/launcher workers, the
  credential refreshers, the quota runner, the machine and event-stream writers): reported as `fatal` and then raised again, so the process
  still dies as it always did (`telemetry.Go` / `defer telemetry.Recover`). Every goroutine the server or a supervisor starts for its whole
  life (and the work it puts off to one, an import job, a usage refresh, a machine report) runs under this; the spawn sites that predate it
  were moved onto it when the crash file below was added.
- A classified warm-process refusal and an unrecognized Docker Codex host
  config key are warnings. `CaptureWarning` and `CaptureOnce` send each stable
  reason key once a day (per process) with a key-only fingerprint; values, paths, and
  agent IDs are excluded from the event and fingerprint.
- **Fatal errors, through the crash file.** A Go runtime throw (`fatal error: fault` after a SIGSEGV or SIGBUS in
  native code, an out-of-memory) is unrecoverable: no panic hook sees it, and a server that hits one used to die
  with nothing in Sentry. At startup the server and the standalone supervisor call `debug.SetCrashOutput` with
  `<config root>/server/crash.log` (0600), so the runtime writes its crash report there as well as to standard
  error. On the next start, if the file is not empty, it is renamed to `crash-<timestamp>.log` (the newest five
  kept) and, **while reporting is on**, sent to Sentry: the report's first line as the event's title, with the
  address in it normalised (`unexpected fault address <addr>`), and the trace as a `crash.log` text attachment,
  trimmed to 256 KiB and scrubbed of the home directory and source roots like every stack trace. With reporting
  off (`AIMEBU_SENTRY_DSN=off`, or the Settings switch off) nothing is sent and the rotated file just keeps the
  trace on disk. The report is made once the start's reporting decision is final — for the server after the stored
  Settings are applied, for the supervisor after the server's settings arrive (or at once when its own
  `AIMEBU_SENTRY_DSN` decides) — so a Settings-only consent is honoured too. The recording itself needs no
  consent: it never leaves the machine.
- Caught failures, by place. One API (`telemetry.ReportError`, `ReportWarning`, `ReportCause`, `WarnCause`): an error the program logs and carries on from
  goes to Sentry as an `error` (a degradation it recovers from by itself as a `warning`), tagged `area`, **once per cause per day** (a set of at most 2000 keys;
  a failure that goes on for days is one report a day, not one per pass). The key and the message are the error text with ids, request ids, timestamps,
  e-mail addresses, hex digests, paths (Unix and Windows) and counts replaced (`telemetry.Classify`), its first line, bounded. Where an error can carry what has no
  recognisable shape (an account or profile name, a person's file, a harness's own output) only a **fixed phrase** is sent (`ReportCause` / `WarnCause`); the
  text stays in the local log. A cancelled context (a client that went away) is not a failure and is not reported. The local log lines are unchanged.
  - `supervisor`: the supervisor's own failures (a harness that would not start, a failed journal write, a stalled turn, an error frame from the server that is
    not one of the classified refusals (a stale generation or desired revision, a conflict, a forbidden or invalid transition: those are the
    protocol working and are only logged; nor is an `unauthorized` for a late frame, an ack, of an agent the supervisor already stopped), a warm process not confirmed gone
    (a warning when it is a containerized process whose host side ended and whose container end the launcher never confirmed, the known Docker path that the quarantine
    releases by itself; an error when the process is still alive), keep-alive and detection problems,
    a server connection that failed five times in a row and stayed unreachable for ten minutes: a shorter outage, and what the journal or the next
    connection delivers again (a journaled turn frame, a turn's exit, the warm state, a warm process that could not be kept for lack of a server) is only logged; an activity frame, which
    nothing replays, is only logged while the connection is down and reported when a send on a live connection fails;
    a journal that cannot be read to replay after reconnecting is reported). Each call site names its cause with a **fixed phrase** and only that goes out; the error behind it (a
    harness's own words, a path, a name) stays in the local log. Two of them are degraded states the supervisor recovers from by itself, so they go out as warnings: a turn killed by a turn
    limit (`AIMEBU_TURN_IDLE` reports `a turn stalled; killing harness`; `AIMEBU_TURN_MAX` reports `turn exceeded the <duration> turn limit` under a separate fingerprint; the watchdog did its job, and the server continues the turn once on its own), and an idle warm process that ended by itself — the system recovers by starting a new one on the next turn. When the same agent's idle warm processes have ended by themselves three or more times within an hour (a harness that keeps falling over while idle is a failure to triage, not noise), the ends are reported as an error instead, under its own phrase `an idle warm process ended by itself repeatedly within an hour` (its own once-a-day report, like every cause); once an hour has passed without one, the ends are warnings again.
  - `machine`, `event-stream`: a failure while a supervisor connects (machine state, reconciliation, harness and launcher state, agent keys), an agent frame
    that fails for a reason that is not a classified refusal, a refused frame (a warning, with its code; not one for an agent that is already stopped or removed on that machine, a late ack, or a stale generation from a retiring instance of the machine — its older frames are the swap racing itself), a write the server used to ignore (that the machine is
    alive, its harnesses, capabilities and last warm report), a failed event replay or poll. A normal restart closes WebSockets with 1001 before the database;
    event-stream cancellation during shutdown does not report an error.
  - `dispatcher`: any failure of a pass (it runs when work wakes it, and at a two-second fallback tick), a room that failed to close, a team-flow migration or agent switch whose spawn could not be sent,
    the checks that hold an agent's turn, the scheduled cleanup.
  - `credentials` (the credential alerts, the refresher, the reconcile pass and the corrupt state files; fixed phrases): a credential alert goes by its kind
    alone: a refresh, fan-out, state-save, login-import, login-strip or commit that failed is an error, a login to redo (`reauth_required`, `expired`) a warning,
    and the information (expiring soon, a login imported, another refresher present) nothing. Also a refresh or warm-up pass that fails as a whole, the start-up
    reconcile, a refresh that could not journal that the token may be spent, rotated credentials not saved first, the aborted refresh whose rescue file holds the
    new login, a switch refused because the maintenance lock could not be taken, and a corrupt `removed.json`, refresh state, usage-alert state or
    `switched_away.json` that was set aside.
  - `switcher`, `usages`, `quota` (fixed phrases): the account switcher's degradations (a link that fell back to a plain copy, MCP logins not carried over or
    replaced, a login imported but not yet rewritten token-only), the usage alerts, and the automatic switcher and limit guard (a switch that failed, a decision
    or state that could not be saved, an unreadable guard configuration). The quota package may import nothing of the program: it reports through
    `quota.Report`, which the server wires to `reportQuota`.
  - `startup`: a server that cannot start (the database file, `store.Open`, the host key, the owner, the listeners), reported and flushed before the process
    exits. The reporter is the one configured by `AIMEBU_SENTRY_DSN`: telemetry turned on only in Settings is applied once the store is open, so a failure before
    that is reported only when the variable is set. `store`: a pre-migration backup that could not be rotated (a warning).
- Traces: one transaction per HTTP request (method, route, status, duration), with one span per SQLite statement made while serving it:
  the SQL text **with its `?` placeholders**, the duration and the number of rows. Never an argument value. The span's description is that statement
  normalized (white space collapsed, any literal written into the text replaced by `?`, cut at 1000 characters), so one query can be told from another in Sentry. The files of the app (the ~240 scripts, styles and icons one page load fetches) are not
  traced either: only the API and the pages the app is opened by (`/`, `/index.html`, `/setup`) are, because each file used to be a transaction named
  `GET /` and together they were most of the server's transactions (a panic or a 5xx in a file is still reported). Event streams and
  WebSocket connections are not traced (their span would last as long as the connection). At most 50 statements of one request get a span
  (`telemetry.MaxDBSpans`); the transaction then carries `db.calls` (how many ran) and `db.calls_dropped` (how many were left out), so a request that
  asks per row shows up without every iteration costing a span.

**Web UI (browser)**

- Uncaught errors and unhandled promise rejections, with stack frames reduced to the script path. The page's own code is one bundled entry script
  (`/bundle/app.js`) and a few chunks beside it (`/bundle/<name>-<hash>.js`), so the path says no more than that; the bundle keeps function and class names (esbuild's `keepNames`), so a frame still names
  the function, and its source map is served beside it but is not uploaded anywhere.
- Errors the page catches and carries on from (`captureError` in `web/js/telemetry.js`): a page's render error (its `ErrorBoundary` shows nothing and
  reports it, tag `source: render`), a failed workspace startup or a read the page tolerates (`startup`, `usages.load`, `preferences.upload`, ... one
  fixed place name each, never an id or a name). Each cause goes out once per page load, fingerprinted by place, error name and HTTP status. Not
  reported, because none of them is a defect: a lost session (401), a request that got no answer (the server restarting), a cancelled request and
  any 4xx answer (the server refusing, which the page shows). The message goes through the same scrubber as every other event.
- Web Vitals and page-load / navigation traces, and the page's own requests as spans (method, route, status). Spans go out inside their
  transaction (`traceLifecycle: 'static'`), so the same scrubber sees them. **No span per loaded file** (script, stylesheet, image: about
  240 per page load), and of the page load's own timing spans only `browser.request` and `browser.response` stay; the vitals are measurements of the
  transaction, not spans.

## Who a report is about

Every account has a **random fingerprint** (16 hex characters from `crypto/rand`, generated the first time it is needed, stored on the account row
as `user.telemetry_fingerprint`, **not** derived from the account id). While telemetry is on, the server's reports and the page's reports carry it
as Sentry `user.id`, and nothing else about the person (no name, e-mail, address or place), so Sentry can count affected people and an
owner can tell their own reports from a tester's.

**Settings → Server → Telemetry** shows every signed-in person their own fingerprint, or their login name while the switch below is on (read-only, labelled accordingly, with a copy button) and a 🎲 button, "Generate a new
fingerprint", that replaces it with a fresh random one. The switch **"Send my user name instead of a fingerprint"** (per person, off by default)
makes `user.id` the login name instead, and the field shows that name instead of the hash until the switch is turned off or the fingerprint is rolled: never both. Rolling while the switch is on asks first ("Rolling a new fingerprint turns off sending your
user name and gives you a new anonymous id."); confirming turns the switch off and rolls, cancelling changes nothing. A change reaches the server
at once and the page's own SDK without a reload. The DSN and the on/off switch stay with the owner and the admins.

API: `GET`/`PUT /api/me/telemetry` (`{send_name}`) and `POST /api/me/telemetry/fingerprint`, each answering
`{fingerprint, send_name, login, report_id}`; `GET /api/buildinfo` adds `sentry.user_id` (the `report_id`) for the page.

## Volume

The free Sentry plan counts spans (5 million a month), so the number of spans per use is bounded by what is traced, not by how many requests: every request and page load is traced by
default (`AIMEBU_SENTRY_TRACES_SAMPLE_RATE` lowers the share), a request carries at most 50 database spans, a sampled page load is a handful of spans, and the files of the app are not traced at all. Settings → Server → Telemetry
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
  the Go server cannot say `infer_ip` (sentry-go has no such setting, and leaves an empty address out of the payload, so Sentry would use this
  server's connecting IP and derive a location), so every server event and transaction carries the unspecified address `0.0.0.0`, which resolves to no
  location; a test pins it;
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
(`AIMEBU_SENTRY_DSN`, `AIMEBU_SENTRY_ENV`, `AIMEBU_SENTRY_TRACES_SAMPLE_RATE`; `AIMEBU_SENTRY_DSN=off` when reporting is off), never through its
arguments; the next turn of any agent builds its entry with the current settings, so the next bridge an agent starts has them, and one that is already running keeps what it
started with.

A DSN in the supervisor machine's own environment (`AIMEBU_SENTRY_DSN`, a DSN or `off`) wins over the server's settings, for the supervisor and for its
bridges. A supervisor that predates this ignores the settings (and is not sent the frame).

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

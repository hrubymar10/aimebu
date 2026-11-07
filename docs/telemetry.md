# Telemetry (Sentry)

aimebu can report crashes, errors and performance traces to [Sentry](https://sentry.io) (or a self-hosted Sentry), from the server and
from the web UI. It reports only while a DSN is in effect (from the environment or Settings, and not before the owner has answered a one-time
popup, below). With no DSN in effect nothing is initialized, nothing is sent, and the browser never even requests the
Sentry SDK file.

## Turning it on, off, and where it is decided

Reporting is decided in this order:

1. **`AIMEBU2_SENTRY_DSN` in the server's environment wins.** A DSN turns reporting on with that project; `off` (or an empty value)
   turns it off. When it is set, Settings cannot change it and no popup is shown.
2. Otherwise **Settings → General → Error reports** decides: a toggle and a DSN field (the same two fields as the popup below). The
   field holds the DSN you chose, else the default this build ships (if any); clearing it goes back to the default. A save applies to
   the server at once, with no restart; a page already open starts or stops its own reporting after a reload. Only the owner and
   admins can change it.
3. **Nothing is reported until the owner has answered the popup.** On a new install the owner sees a one-time "Error reports" popup
   after the welcome, and on the first sign-in after an update that brought error reports. It holds the toggle (on when this build
   ships a default DSN, otherwise off), the DSN field, and one sentence on what is and is not sent. **Save** keeps what is shown; **Not
   now** (or Escape) turns reports off. Either way it is not asked again.

A build that ships no default DSN is off until a DSN comes from the environment or Settings.

Other variables (all optional):

| Variable | Meaning | Default |
|---|---|---|
| `AIMEBU2_SENTRY_DSN` | A DSN, or `off`/empty to disable; overrides Settings. | unset |
| `AIMEBU2_SENTRY_ENV` | The Sentry environment name. | `dev` |
| `AIMEBU2_SENTRY_TRACES_SAMPLE_RATE` | Share of requests traced (server and browser), 0 to 1. | `0.1` |

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
  WebSocket connections are not traced (their span would last as long as the connection).

**Web UI (browser)**

- Uncaught errors and unhandled promise rejections, with stack frames reduced to the script path.
- Web Vitals and page-load / navigation traces, and the page's own requests as spans (method, route, status). Spans go out inside their
  transaction (`traceLifecycle: 'static'`), so the same scrubber sees them.
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
- no environment variable values, no host name, no user, no IP address and no user agent: the browser SDK is given an explicit
  `dataCollection` with everything off (its `sendDefaultPii` option does nothing in the vendored version, so it is not relied on), and the
  page test checks the envelopes it actually sends;
- no URL in the form a person typed it: the server names a request by its **route pattern** (`GET /api/rooms/{room}/messages`), and the
  browser reduces a URL to its resource (`/api/rooms/…`); no host, no query string, no fragment, and no ids or names chosen by a person
  (a profile name is part of some paths). 
- no machine paths: in every stack trace (an exception's, a thread's) the program's own files are shown relative to its source root
  (`internal/server/server.go`), the Go toolchain as `$GOROOT`, and any home directory (this process's `$HOME`, or any `/Users/<name>`,
  `/home/<name>`, `C:\Users\<name>`; the browser too) as `~`, so it does not matter where the binary was built or who runs it;
- no local variables or source lines in stack frames, no console output, no click or typing breadcrumbs;
- free text is cut to 300 characters; span and context data outside an allow-list is dropped.


## Self-hosting

The DSN is just a URL, so any Sentry-compatible server works: run [self-hosted Sentry](https://develop.sentry.dev/self-hosted/), create a
project, and use its DSN. Events then never leave your network. Note the page sends its events to the DSN's host **from the browser**,
so that host must be reachable from the browsers that use aimebu, and (for a browser on another origin) allow CORS as Sentry does.

## The vendored SDK

`web/vendor/sentry/bundle.tracing.min.js` is the unmodified Sentry browser SDK bundle with Tracing only, no Replay (MIT); its version,
source and SHA-256 are in `web/vendor/README.md`. It is a plain script, loaded like the other vendored files, with no build step. The Go
side uses `github.com/getsentry/sentry-go` and `.../otel` (MIT) with the OpenTelemetry API and SDK (Apache-2.0); a small span processor in
`internal/telemetry` converts finished OpenTelemetry spans into Sentry transactions.

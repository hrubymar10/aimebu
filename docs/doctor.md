# Doctor

`aimebu doctor` runs read-only health checks on the server installation and
prints a table; `aimebu doctor --json` prints the same results as JSON. The
exit code is 0 when no check fails, 1 when at least one fails. Warnings never
fail the run. Doctor is safe to run while the server is running: it never
writes — the database is opened with a read-only DSN (no journal mode
change), the TLS files are only parsed, and the server is probed with a single
unauthenticated `GET /api/me`.

## Checks

- **config dir** — ok when it exists and is not world-writable; warn when
  world-writable; fail when missing or unreadable.
- **sqlite database** — opened read-only, then `PRAGMA integrity_check` and
  the stored schema version compared against the newest migration this binary
  applies. A missing or empty database warns (the server may not have started
  yet); a schema older than the binary warns (it forward-migrates on next
  start); a failed integrity check or a schema newer than the binary fails.
  It also fails when the v2 database holds v1's `schema_meta` table, which
  means a v1 server was started on the folder after v2 moved v1's files into
  `.old-v1-bkp` (see [migrating from v1](migrating-from-v1.md)).
- **server** — ok when `GET /api/me` answers 401 at the configured
  `AIMEBU2_BIND`:`AIMEBU2_PORT`; any other HTTP answer warns; a refused
  connection or timeout fails. The plain-HTTP listener stays up when TLS is
  enabled, so HTTP is always the right probe.
- **tls** — uses the same resolution as `aimebu server`: both
  `AIMEBU2_TLS_CERT` and `AIMEBU2_TLS_KEY` must be set, readable, and form a
  parseable pair, and `AIMEBU2_TLS_PORT` must differ from `AIMEBU2_PORT`.
  Not configured is ok; a certificate expiring in less than 14 days warns;
  an expired or broken pair fails.
- **bind/allow** — reports the effective bind address and allowlist (already
  validated while resolving the config) and warns when the bind is
  non-loopback without TLS.
- **proxy** — shown only when `AIMEBU2_PUBLIC_URL` is set. A value the
  server would refuse fails. It warns when `AIMEBU2_TRUSTED_PROXIES` is
  empty: every client behind the proxy then shares the proxy's IP as one
  login, invitation and pairing throttle identity, so one client's failures
  can delay everyone's sign-in.
- **switcher** — shown only when `switcher/state.json` exists under the config
  root. A state file that does not parse fails; a disabled switcher is ok.
  When enabled, each tool (`switcher/claude`, `switcher/codex`) is ok with no
  active profile or a healthy one, and warns when the active profile's
  directory is missing (stale pointer), it has neither stored nor live
  credentials, or the live credential file is missing while only a stored
  copy remains (login required; the newest backup of that profile is named
  when one exists).
  The check only reads files; the live credential path honours
  `CLAUDE_CONFIG_DIR` and `CODEX_HOME`.
- **credential \<tool\>/\<profile or live\>**, **credential targets**,
  **credential refresher**, **claude account files** — shown only on a
  token-copy install (`switcher/token-copies.json` lists a migrated tool, see
  [switcher](switcher.md#token-only-credential-model-migrate-copies)) and only
  for the migrated tools. They read files only, so they work with the server
  stopped, which is when they matter: with no refresh token in the live files
  nothing renews them. Each stored profile and live file is judged by its
  expiry: expired or a parked account (`reauth_required`, `needs_relogin_soon`)
  fails; under 15 minutes left or `refresh_failed` warns; healthy ones fold into
  one `credential targets` ok line with the next live expiry. `credential
  refresher` warns when `harness-docker-ctrl` is not on this command's `PATH` or
  when the `docker` CLI is missing or its daemon does not answer (`docker info`
  fails as seen by this command); the refresher runs its CLI in a container, so
  it needs both, and the server needs them on its own `PATH`. `claude account files` warns when claude-code's
  `.claude.json` files name different accounts, which could file a login under
  the wrong profile.
- **v1 credential targets** — warns if the v1 backup contains an external
  `server/credential-targets.json` but the new server directory does not. V2
  copies this registry on startup before loading the switcher.
- **credential refresher ownership** — fails when the switcher root cannot be
  opened or the other default roots cannot be checked for token-copy claims
  (the message carries the error).
- **credential refresher \<tool\>** — fails when another default switcher root
  marks the same tool as token-only and resolves to the same live credential
  file. This doctor check only looks at `~/.aimebu` and `~/.aimebu2`; the server
  does not depend on it. The server holds a lock file next to each live
  credential file it refreshes (`<live file>.aimebu-refresher.lock`, held for the
  process lifetime), takes it at once when a tool is migrated, and re-checks it every 30 seconds; a tool refreshes only while its claim is actually held, so a second refresher
  anywhere, in any config directory, is refused for that tool and reported as a
  `refresher_conflict` alert in `GET /api/credentials/targets` and as a system
  event. When the other refresher exits the next check takes the tool over. If
  token auto-refresh is off, Settings still shows that tool as `off`; the alert
  signals the ownership conflict.

## Configuration

Doctor reads the same environment as `aimebu server`: `AIMEBU2_CONFIG_DIR`
(default `~/.aimebu2`), `AIMEBU2_BIND`, `AIMEBU2_PORT`, `AIMEBU2_ALLOW`,
`AIMEBU2_TRUSTED_PROXIES`, `AIMEBU2_PUBLIC_URL`,
`AIMEBU2_TLS_CERT`/`AIMEBU2_TLS_KEY`, and `AIMEBU2_TLS_PORT`. An invalid
value is reported on stderr and exits 1.

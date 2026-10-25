# Doctor

`aimebu doctor` runs read-only health checks on the server installation and
prints a table; `aimebu doctor --json` prints the same results as JSON. A
warning or failure carries a one-line hint on fixing it (printed under the row,
and as `hint` in JSON). The exit code is 0 when no check fails, 1 when at least
one fails, 2 on a usage error. Warnings do not fail the run unless
`--strict` is given, which makes them exit 1 too (for scripts that gate on a
clean report). Doctor is safe to run while the server is running: it never
writes — the database is opened with a read-only DSN (no journal mode
change), the TLS files are only parsed, and the server is probed with a single
unauthenticated `GET /api/me`. The one thing SQLite itself does on a read-only
open of a WAL-mode database is create its empty `-wal`/`-shm` sidecar files
when the server is stopped; the database file is never touched.

The JSON is `{"version", "generated_at", "checks": [{"name", "status",
"description", "hint", "scope"}], "exit_code"}`. `scope` says where a fact was
read: `server` (this host's config root and database), `live` (the running
server) or `machine` (a supervisor host).

## Modes

- **default** — the server installation: this host's config root and database,
  plus a probe of the server (the checks below).
- **`--machine`** — a host that runs only the supervisor, with no server or
  database of its own. It runs the config
  dir check and the [machine checks](#machine-checks) and skips every
  server-owned one.

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
- **disk space** — free space on the filesystem holding the config root, with
  the database and WAL sizes. Warns under 2 GiB, fails under 200 MiB: SQLite
  writes start failing on a full disk.
- **backups** — the server writes a verified copy to `server/backups/` before
  every schema migration. No backups yet is ok (nothing has migrated). Doctor
  counts them, shows the age of the newest, and runs `PRAGMA quick_check` on it
  through a read-only open; the check is skipped (warn, "not verified") for a
  file over 512 MiB or one that takes over 10 seconds. A corrupt newest backup
  fails; leftover `.partial` files from an interrupted backup, or a backups
  directory readable by others, warn.
- **foreign keys** — `PRAGMA foreign_key_check`; fails when rows point at rows
  that no longer exist, naming the child→parent relations.
- **machines** — approved, awaiting-approval and revoked counts. While the
  server answers, an approved machine not seen for 10 minutes (or never
  connected) warns. With the server down liveness is not judged. **revoked
  machines** warns when a revoked machine still owns live agents. No machines is
  ok.
- **pending stops** — stop controls no machine has applied yet. They make
  deleting a project return 409 until the machine applies them. Warns for a
  revoked machine, or an approved one that is not connected while the server is
  up; a connected machine with a few in flight is ok.
- **launchers** — warns for an enabled launcher with no models, or a model list
  with no default. No launchers yet is ok.
- **provider limits** — warns while agents are held by a provider usage limit
  that has not reset (with the latest reset time per tool).

  All of the above read the database, so they run only when the sqlite check
  passed its integrity check; an empty or new install answers ok or warn, never
  fail.
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

## Machine checks

`aimebu doctor --machine` reads what lives on the supervisor's host. Findings
carry the scope `machine`. It never writes: the journal is opened read-only and
its lock is only tested, not taken.

- **machine credential** — the pairing credential, `supervisor/machine.key` (or
  `server/host-machine.key` on the server's own host). Fails when the file is
  not mode 0600 (the supervisor refuses it too) or does not parse; warns when
  there is none (this host is not paired). The key itself is never printed.
- **supervisor journal** — `supervisor/journal.sqlite`: size, WAL size, whether
  a supervisor holds its lock right now (with its pid), and a read-only
  `quick_check` bounded like the backup one (512 MiB, 10 seconds). A damaged
  journal fails.
- **agent tools config** — the per-agent toolbox configs in
  `supervisor/agent-tools/` hold an agent key, so the folder must be 0700 and
  each file 0600; looser modes warn.
- **agent logs** — the size of the captured agent output in
  `supervisor/agent-logs/` against the daily sweep's 256 MiB cap; warns at 80%.
- **harnesses** — the native `claude`, `codex`, `pi`, `opencode` and `vibe`
  executables the supervisor finds (the same search as Settings → Harnesses
  auto-detect), with versions. None warns.
- **docker** — the Docker CLI and daemon (`docker info`). A missing daemon only
  warns when a `*-docker` launcher is installed.
- **launcher \<name\>-docker** — for each Docker launcher found,
  `HARNESS_DOCKER_PRINT_FEATURES=1 <launcher> --version`, which exits before it
  touches Docker. The features are listed; a launcher without `forward_env`
  warns "too old" for opencode and vibe, whose Docker turns need it (update
  harness-docker).
- **paired server** — asks the paired server (`AIMEBU2_URL`, else the URL in
  the credential) whether it knows this machine and has approved it, with the
  machine's own key as the bearer token, exactly as the supervisor does at
  start. Fails when the server is unreachable or rejects the key (revoked, or
  paired with another server); warns while approval is pending.

## Configuration

Doctor reads the same environment as `aimebu server`: `AIMEBU2_CONFIG_DIR`
(default `~/.aimebu2`), `AIMEBU2_BIND`, `AIMEBU2_PORT`, `AIMEBU2_ALLOW`,
`AIMEBU2_TRUSTED_PROXIES`, `AIMEBU2_PUBLIC_URL`,
`AIMEBU2_TLS_CERT`/`AIMEBU2_TLS_KEY`, and `AIMEBU2_TLS_PORT`. An invalid
value is reported on stderr and exits 1.

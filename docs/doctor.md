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
  database of its own (`--live` does not combine with it). It runs the config
  dir check and the [machine checks](#machine-checks) and skips every
  server-owned one.
- **`--live`** — the default checks plus what only the running server knows
  (the [live checks](#live-checks)).

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
- **pending stops** — stop controls no machine has applied yet, with exactly
  the condition project deletion uses: an agent that is not revoked, on a
  machine that is not revoked, wanted stopped, whose stop the machine has not
  confirmed. They make deleting a project return 409 until the machine applies
  them. Warns for an approved machine that is not connected while the server is
  up; a connected machine with a few in flight is ok. A revoked machine's stops
  do not block deletion and are not listed.
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
- **launcher \<name\>-docker** — for each Docker launcher found whose tool
  depends on a launcher feature (opencode and vibe need `forward_env`),
  `HARNESS_DOCKER_PRINT_FEATURES=1 <launcher> --version`, which a current
  launcher answers before it touches Docker. A launcher that predates the probe
  ignores it and would start a container running `<harness> --version`, so only
  those tools are probed (the supervisor refuses an old launcher for them
  anyway), from a fresh empty temporary folder rather than the caller's
  directory, with a 10-second limit. The features are listed; a launcher
  without `forward_env` warns "too old" (update harness-docker). Docker
  launchers of other tools are not run.
- **paired server** — asks the paired server (`AIMEBU2_URL`, else the URL in
  the credential) whether it knows this machine and has approved it, with the
  machine's own key as the bearer token, exactly as the supervisor does at
  start. Fails when the server is unreachable or rejects the key (revoked, or
  paired with another server); warns while approval is pending.

## Live checks

`aimebu doctor --live` adds findings with the scope `live`, read from the
running server's memory through `GET /api/doctor`: the capability flags each
connected supervisor announced in its hello, and what each machine's launchers
last reported. None of it is in the database, so the offline checks cannot see
it.

The endpoint needs no login. At start the server writes a fresh random key to
`server/doctor.key` (mode 0600, replaced at every start, removed at shutdown)
and accepts it as `Authorization: Bearer <key>` only from a same-host connection
that carries no forwarding header (`X-Forwarded-For`, `Forwarded`, `Via` and
the like), so a reverse proxy on the same host cannot relay it. Same-host means
the peer address is loopback, or is the very address the connection was
accepted on: with `AIMEBU2_BIND` set to a VPN or LAN address the CLI dials that
address and the kernel uses it as the source too, and a remote host cannot fake
it (the handshake's reply goes to the source address). Anything else gets 403. Doctor reads the file only when it is a regular file, owned by the
running user, with no group or other permission bits; being able to read the
config directory is the credential. The answer holds no secrets: no keys,
environment or launcher arguments.

- **doctor key** — whether the key file exists and is usable. Missing means the
  server is not running from this build or could not write it; `--live` then
  reports it as a warning and nothing more.
- **live server** — warns when the server does not take the key: 403 means the
  key file belongs to another start of the server (it is replaced at every
  start) or the request did not reach it from this host directly; 404 means an
  older build without `/api/doctor`.
- **machine \<name\>** — a connected machine whose supervisor lacks a
  capability flag warns, naming each flag and what degrades without it (for
  example `supports_session_fork`: a Claude Code session can only be continued
  in place, not as a copy; `supports_worktree`: room worktrees cannot be created
  on that machine; `supports_stop_replay`: a stop sent while the machine was
  offline is applied by a fallback that can kill a live process early). A
  machine that is not connected right now is ok here; the offline `machines`
  check judges how long it has been gone.
- **connected machines** — warns when no approved machine is connected.
- **launcher \<label\> on \<machine\>** — for each *enabled* launcher of a
  connected machine: not yet reported by the supervisor, unavailable (with the
  supervisor's reason, such as "command not found on this machine"), or a Docker
  launcher whose feature probe failed or that is too old for its tool all warn.
  The supervisor asks a Docker launcher for its features
  (`HARNESS_DOCKER_PRINT_FEATURES=1 <launcher> --version`) once when it inspects
  the launcher in a project folder of the machine (like the launcher's own
  `--version` check, never in an arbitrary directory), only for tools whose Docker turns depend on one (opencode and
  vibe need `forward_env`), and reports them with the launcher. The row stays
  available: the turn's own preflight still refuses a too-old launcher with its
  own message.

## Configuration

`--live` talks to `AIMEBU2_BIND`:`AIMEBU2_PORT` over plain HTTP, never through
a proxy from the environment and without following redirects, and sends the key
only after the server check got the 401 an aimebu server answers (any other
answer from that port, such as another program's 200, ends the live checks
before the key leaves the process).
Doctor reads the same environment as `aimebu server`: `AIMEBU2_CONFIG_DIR`
(default `~/.aimebu2`), `AIMEBU2_BIND`, `AIMEBU2_PORT`, `AIMEBU2_ALLOW`,
`AIMEBU2_TRUSTED_PROXIES`, `AIMEBU2_PUBLIC_URL`,
`AIMEBU2_TLS_CERT`/`AIMEBU2_TLS_KEY`, and `AIMEBU2_TLS_PORT`. An invalid
value is reported on stderr and exits 1.

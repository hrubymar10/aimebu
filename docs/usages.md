# Usage Snapshots

The Usages feature shows provider quota snapshots in the right sidebar,
Settings -> Usages, and the `aimebu usages` CLI command. The top-bar Usages
button opens the right-sidebar Usages view, where enabled providers are
rendered as a vertical list. Disabled providers remain configurable from
Settings -> Usages; when none are enabled, the sidebar shows a shortcut to
that settings section.

Supported providers:

| Provider | Auth source | Setup surface |
|---|---|---|
| `codex` | Codex OAuth file at `$CODEX_HOME/auth.json`, or `~/.codex/auth.json` | Enable in Settings -> Usages |
| `claude-code` | Claude Code OAuth file at `~/.claude/.credentials.json` | Enable in Settings -> Usages |
| `github-copilot` | GitHub device flow token stored locally by aimebu | Settings -> Usages -> Sign in with GitHub |
| `mistral` | Browser `Cookie` header from `https://console.mistral.ai` | Settings -> Usages -> Mistral credentials |
| `ollama-cloud` | Browser `Cookie` header from `https://ollama.com/settings`, or Ollama API key | Settings -> Usages -> Ollama Cloud credentials |

Provider secrets are stored in `~/.aimebu/usages/config.json` with file mode
`0600`. `~/.aimebu/usages/cache.json` stores only redacted last successful
snapshots and has file mode `0644`.

## CLI

```bash
aimebu usages
aimebu usages codex
aimebu usages claude-code --json
aimebu usages github-copilot --plain
aimebu usages mistral --json
aimebu usages ollama-cloud --json
```

Plain text is the default. `--json` returns the normalized response used by
the web UI:

```
{
  "providers": [ { "provider_name": ..., "order_number": ..., "enabled": ...,
                   "available": ..., "profiles": [ ... ] } ],
  "settings": { "refresh_interval_sec": ..., "percent_display": ... },
  "switcher_enabled": true
}
```

`providers` is an **ordered array**, not a map — `order_number` is the display
order, so nothing has to be zipped against a separate ordering setting. Each
provider carries a non-empty `profiles` array with **one entry per switcher
profile**, or a single active entry named `"local"` when no switcher profiles
exist. A profile name is never empty. Switcher initialization failure uses the
same `local` fallback, so it cannot change the usages response shape.

A `Profile` carries **both** the usage data and the switcher facts for that
account: `profile_name`, `active`, `has_credentials`, `expires_at`, `email`,
plus `status`, `plan`, `windows`, `credits` and `last_refresh_at`. That is
deliberate — there is no join between usage data and account data, so there is
nothing for the two to disagree about. Switch eligibility is per tool and lives
on the provider (`switch_eligible`, `switch_absent`,
`switch_ineligible_reason`); only the global on/off flag sits at the top level.
`switch_absent=true` means the active profile has no live credentials to
capture, so switching away remains allowed even though the tool is not
otherwise eligible.

Provider rows also carry the non-secret settings needed by their credential
controls: `enterprise_host` for GitHub Copilot, `auth_mode` and
`api_key_configured` for Ollama Cloud, and `cookie_configured` for Ollama Cloud
and Mistral. `token_configured` reports whether a Copilot token is stored.
Fields with empty or false values are omitted. The API never returns the
stored API key, cookie, or token.

Settings -> Usages has an on/off toggle for every provider. Turning off GitHub
Copilot, Mistral, or Ollama Cloud stops usage fetching while keeping saved
credentials, so the provider can be turned on again without signing in or
pasting secrets. The toggle is disabled until usable credentials are saved:
Copilot needs a token, Mistral needs a cookie, and Ollama Cloud needs the
credential selected by its `auth_mode` (`auto` accepts either). Clearing
credentials or signing out still removes them and disables the provider.

## Refresh Behavior

The stored refresh interval defaults to `120` seconds and has a minimum of
`15` seconds. `AIMEBU_USAGES_REFRESH` overrides the stored value for both the
server and CLI.

`GET /api/usages` is a **pure cache read** — it never triggers a network
fetch. All per-profile fetching is owned by the background poller (5-second
tick, fetches when the interval elapses) and the force-refresh button. On a
cold cache (server restart), the response already carries resolved profile
rows with no usage snapshot; the first poller tick (~5 seconds) warms them.

**Per-profile refresh:** every active and inactive profile refreshes on the
configured provider interval. The active profile uses the provider's normal
live-credential fetch; inactive stored profiles use a read-only fetch. Neither
fetch rotates tokens: the server never refreshes the active account's token
(see [Never the active profile](#never-the-active-profile)).
Email-only lookups retain a separate one-hour floor. After a switch,
`InvalidateProfileSnapshot` drops the affected named entries, so the next
poller tick (~5 seconds) refetches immediately regardless of the interval.
Until it lands, the API keeps the active named profile row and the web UI shows
`Refreshing…`; a failed fetch fills that same row with its failure status
instead of changing the response shape.

The cache has only profile-named entries. Older empty/`"default"` entries are
migrated to `local` when no switcher profiles exist. When named profiles do
exist, legacy default and orphan entries are discarded because their account
ownership cannot be proven; valid named history is retained and deduplicated.

Claude and Codex fetches are bound to the active profile name and a fingerprint
of the credentials that started the request. If either changes while the
network request is running, aimebu discards the late result instead of caching
one account's usage under another profile. The switcher lock is not held while
waiting on the provider. A credential-file change also bypasses an otherwise
fresh cache entry so CLI token rotation is picked up on the next poller tick;
an in-flight rotation leaves the previous cache entry intact until a fetch
owned by the new credentials completes.

## Token-only refresher

The normal path is **Settings → Usages → Account maintenance → Auto-refresh
tokens**. Turning it on starts migration of each eligible Claude and Codex
tool to token-only live credentials. A tool that cannot yet
migrate shows `waiting` and the reason in Settings; it is retried on later
ticks. The existing safety refusals apply: a token within 15 minutes of
expiry, a running Codex process, or no active profile stops that tool's
migration. For running Codex sessions, the per-tool reason gives only their
count and says that migration retries automatically; process details stay in
the server log. The manual `migrate-copies` refusal still includes those details.
The other tool may migrate independently. Turning the setting off
restores a full credential to each live file, preserves unrelated live fields
such as Claude `mcpOAuth`, relinks the active stored profile, and removes the
tool's migration marker. A pending `/login` is imported before restore.

The stored `usages/config.json` key is `token_auto_refresh` (default false).
An internal `token_auto_refresh_managed` flag keeps an interrupted OFF cleanup
retryable after restart; a manual `migrate-copies` while the setting is off is
left alone. A legacy ON value carried over from `claude_auto_refresh` also
sets this flag so a later OFF restores full live credentials.
Old `claude_auto_refresh` config values are carried over when the new key is
absent; saves mirror both keys for one release so a downgrade keeps the
setting. Settings GET responses include both names for one release, with the
old name read-only. Settings POST accepts
either name, preferring `token_auto_refresh` when both are present.
`token_auto_refresh_state` reports each tool as `migrated`, `waiting` with a
reason, `pending` while a setting change is applied, `n/a` when no active
profile exists, or `off`; `token_auto_refresh_available` reports the runtime
gate. A setting change starts reconciliation immediately; later scans retry
safety refusals without blocking usage refreshes. A waiting tool retries at
most once a minute, while a changed toggle bypasses the backoff.

While on, aimebu must keep running whenever Claude or Codex is used, even
outside aimebu: a token-only live file cannot refresh itself. Turn the setting
off before a long server shutdown or uninstalling aimebu.

On a **migrated** tool (listed in `switcher/token-copies.json`, see
[switcher.md](switcher.md)) the server is the only party that spends refresh
tokens. The profile store holds the only full credentials; the live
`~/.claude` and `~/.codex` files are token-only copies the server rewrites. The
refresher replaces the legacy auto-refresh below for as long as it is enabled
(the legacy refresh triggers and warmups stand down for that tool).

The refresher is **off by default** and runs only for migrated tools. At server
start it first settles the journal of a killed process, then runs the
reconciler's pass, then the due pass, in that order, so a login that
landed in a live file while the server was down is imported before any refresh
can race it.

**Knobs.** `AIMEBU_TOKEN_LEAD_CLAUDE` (default `60m`) and
`AIMEBU_TOKEN_LEAD_CODEX` (default `24h`) set how long before expiry a profile is
due, as Go durations between 10 minutes and 14 days; anything else is ignored.
A lead longer than a token's life makes every profile due at once, which is how
an acceptance run forces a refresh.

**Rotation-loop guard.** A lead at or above the token's life would make a freshly
rotated token due again on the next tick. So the refresher never rotates a
profile again within **30 minutes** of its last successful rotation, and once it
knows the token life it caps the lead at that life minus 30 minutes. It keeps
the last rotation time, token life and a hash of the refresh token the rotation
produced per profile in `switcher/refresh-last.json` (0600, no credentials). The
floor and the cap apply only while the profile still stores that token, so a
re-login or import is never held back by an earlier rotation. Losing the file
only loses the cap for the first rotation after a restart.

**What it refreshes.** Every profile in the store, the active one included,
once its access token is expired or within its lead of expiry: **60 minutes**
for Claude, **24 hours** for Codex. It runs at server start and then every 30
seconds, one profile after another, one account at a time. A profile whose
stored file is a symlink (an install that has not been migrated) is never
touched, and neither is a profile switched away from in the last two minutes (an
incoming profile in a switch is the exception: it is refreshed if due however
recently it was last active). Codex profiles are also left alone for ten minutes
after migration: a Codex session started before the migration caches its
`auth.json` and may still refresh the old token itself (a switch's incoming
profile is exempt).

**One run.** Under the per-account lock (the same `switcher/.maintenance` flock
a switch holds, so a switch and a refresh never overlap):

1. re-read the profile's refresh token and compare it with the one the tick
   decided on; if a login was imported meanwhile the tick is abandoned and the
   older token is never spent;
2. journal `spending` with the hash of the refresh token about to be used;
3. run the CLI (`claude -p` / `codex exec`, in the `harness-docker` container)
   against a throwaway copy of the credential;
4. classify what happened from the files and the CLI output (the journal phase
   moves from `spending` to `started` the moment the CLI process exists; a crash
   before that never reached the CLI and is not treated as a spend);
5. on success write the new credentials to a rescue file first, journal
   `rotated`, commit them with the compare-and-swap on the starting refresh
   token, delete the rescue file, and fan the token-only form out to the live
   files. A commit that loses the compare-and-swap keeps the rescue file. If a
   live file still holds a refresh token (a `/login` waiting for the reconciler)
   the fan-out neither retries nor forces: it raises `login_pending` and leaves
   the file to the reconciler.

A commit that fails while the store still holds the spent token (disk full, a
lock error, a listing error) is **not** a reason to spend again: the journal
stays at `rotated` with its rescue file, the account backs off, and the next
passes retry the commit from the rescue copy until it lands. The commit reaches
**every** profile that holds the spent token (the same account stored twice),
and it judges a rotation by the access token or expiry that moved, never by the
refresh token alone, because a provider may rotate the access token without
rotating the refresh token. A server shutdown lets a CLI that is already
running finish (bounded by its own timeout) and commits its result; it starts
no further run. The startup pass (settle the journal, reconcile, first due pass)
runs the first time the refresher is enabled, which on a live server is also
when the toggle migrates a tool. While the refresher owns a tool's refresh,
that tool's legacy auto-refresh and warmup stand down.

**Outcomes (spend once).**

| outcome | meaning | what happens |
|---|---|---|
| rotated | new credentials on disk | committed and fanned out |
| not sent | the CLI never started, or its output proves the request never left (DNS, connection refused), or it exited cleanly without rotating | nothing was spent; retried after 5 min, doubling to 30 min per account; three clean no-ops in a row report `refresh_failed` |
| `invalid_grant` | Claude overwrote its own refresh token with `""` in the copy; Codex printed that the token expired, was already used, was revoked or rejected | terminal `reauth_required`; the stored credential is never rewritten or wiped |
| unknown | the CLI started, failed, and rotated nothing, with no proof the request was not sent | the token is **never spent again**: terminal `needs_relogin_soon` until a human logs in |

Terminal states and the journal live in `switcher/refresh-state.json` (mode
0600, no credentials in it), keyed by the hash of the refresh token they apply
to. A new login is a new refresh token, so it clears the state by itself, and a
restart cannot forget one. On start, before its first pass, the refresher settles
a journal left by a killed process: a rotated rescue file, or a temp copy that
rotated before the crash, is committed against the spent token and fanned out;
a spend with nothing to recover is marked `needs_relogin_soon` and not repeated.

**Seeing it.** `GET /api/credentials/targets` returns, for every store profile
and the two live files, the expiry, time left, level (`ok`, `expiring` under 15
minutes, `expired`), the refresher state and the next attempt; it never carries a
token, and never the free-text `detail` of a failure (CLI output and error text,
which stays in `refresh-state.json`, the server log and `aimebu doctor`, redacted
of anything token-shaped and with the home directory shown as `~`). The same
alerts are in the usages response (`credential_alerts`) and
appear as a red banner above the tiles of the Usages panel. `aimebu doctor`
reads the files directly, so it works with the server stopped: a credential
under 15 minutes is a warning, an expired one, `reauth_required` or
`needs_relogin_soon` is a failure. Edge alerts (`reauth_required`,
`needs_relogin_soon`, `refresh_failed`, `expiring`, `expired`, `fanout_failed`,
`login_pending`, `recovery`) go to the alert hook once per credential and expiry,
and an account's alerts are re-armed when it is healthy again. The human-facing
kinds (`reauth_required`, `needs_relogin_soon`, `expired`) are also posted as a
system message to the human members of every room, which every member of those
rooms can read, so the message is a fixed sentence plus the credential
(`claude/main`); the websocket `credential_alert` frame carries the kind only.

With no refresh token in the live files a CLI cannot renew itself, so the server
being down for longer than the remaining life of a live token (at least the lead,
typically hours for Claude) means requests fail until it is back; the doctor
check and the banner exist to say so in time.

## Legacy link-model maintenance

The following refresh and warmup rules apply only while a tool is still on
the original linked-credential model. The `token_auto_refresh` setting first
tries to migrate it; a safety refusal leaves it waiting and retrying on later
ticks. Once any tool is migrated, the token-only refresher owns refresh and
that tool's legacy warmup stands down.

Claude OAuth access tokens live only about 8–12 hours. On macOS the live
harness refreshes them lazily through the Keychain, but a stored switcher
profile with no live process keeping it fresh — an inactive profile — sees its
token silently lapse, and switching to it forces a browser re-login. (The
active profile is kept fresh by the harnesses running on it; see
[Never the active profile](#never-the-active-profile).)

Claude auto-refresh keeps near-expiry `claude` switcher profiles ready to use.
When enabled, the background poller renews any **inactive** profile
whose access token is already expired or within 10 minutes of expiry by running
the real `claude` binary inside a throwaway `harness-docker` container pointed
at a copy of that profile's credentials. Inside the container there is no
Keychain, so claude performs its file-based refresh-on-use and writes rotated
tokens straight back, which aimebu then copies into the stored profile (see
[Never the active profile](#never-the-active-profile) for what happens when the
profile is switched to mid-run).

Design notes:

- **Inactive profiles only.** See
  [Never the active profile](#never-the-active-profile).
- **Temp-dir isolation.** claude pollutes its config dir with `.claude.json`,
  `sessions/`, `projects/`, and `policy-limits.json`. aimebu copies only
  `.credentials.json` (the resolved bytes, never a symlink — a host link would
  dangle inside the container) into a temp dir, marks only that throwaway
  copy's access token expiry as past, runs the container there, and copies
  only the genuinely rotated `.credentials.json` back — the stored profile dir
  stays clean and is never force-expired.
- **Success is the rotated file, not the exit code.** A refresh counts only when
  the new `.credentials.json` parses and its `expiresAt` advanced beyond the old
  value. If the container rotates the token and then exits non-zero (for example,
  because the follow-up model turn is usage-capped), aimebu still validates and
  safely copies back the rotated credentials; a non-zero exit remains fatal when
  no valid rotation occurred. Codex account maintenance follows the same rule.
- **Non-rotation safety net.** Pre-emptive refresh force-expires only the temp
  copy so claude normally rotates even when the stored access token still has up
  to 10 minutes left. If the executor nevertheless returns credentials whose
  `expiresAt` did not advance, aimebu still treats that as a no-op rather than a
  failure: it logs `outcome=noop reason=token-still-valid`, avoids failure
  backoff, and defers the next attempt to the stored token's real expiry.
- **Copy-back safety.** The slow container run happens outside the switcher
  lock. The final copy-back holds `switcher/.lock`, re-resolves the named
  profile, and compares the stored refresh token with the one that started the
  run. A switch changes the credential path, and a rename changes the name;
  rotated tokens follow the profile holding the starting refresh token. A
  profile-listing error or ambiguous match saves the rotated file for recovery.
  With no named switcher profiles, copy-back retains the implicit `local`
  profile. Claude copy-back replaces only `claudeAiOauth`, retaining unrelated
  keys such as `mcpOAuth`. If the profile was re-logged into another account,
  copy-back leaves it untouched and saves the rotated file under
  `switcher/backups/claude/<profile>-rotated-<UTC timestamp>.json` with mode
  `0600`; the abort log names that rescue path. Removing a profile records a
  hashed refresh-token tombstone in `switcher/removed.json`; copy-back checks
  it before lookup and discards that in-flight rotation without a rescue file
  when the removal happened during the run and no current profile holds the
  starting token. Every other unresolved lookup, including an older tombstone,
  a re-login after rename, or a corrupt tombstone file, leads to a rescue.
- **Rate limiting.** A profile is never refreshed twice concurrently, and after
  a genuine failure it is not retried for 5 minutes.

**Gate.** Claude and Codex legacy maintenance run only when all of these hold: the
`token_auto_refresh` setting is enabled (default **off**), the
`harness-docker-ctrl` binary is on `PATH`, and the docker daemon is reachable.
When `harness-docker-ctrl` is absent the feature is reported unavailable and the
Settings toggle is disabled.

**Cost.** Enabling it incurs a small per-account cost — occasional `claude -p`
calls against each near-expiry inactive profile — so it is opt-in.

Configure it in **Settings → Usages → Account maintenance → Auto-refresh tokens**.
The GET/POST `/api/usages/settings` responses expose `token_auto_refresh` and
the runtime `token_auto_refresh_available` availability flag. POST accepts
optional `token_auto_refresh`, `warmup_mode`, and `warmup_schedule`
values alongside `refresh_interval_sec`, `percent_display`, and
`claude_model_flag`.

The shared `claude_model_flag` setting controls the model used by both the
auto-refresh and warmup container prompts. It stores the complete flag string
and defaults to `--model haiku`. Set it to an empty string to omit the flag and
use the Claude CLI's default model. Non-empty values must match
`--model [A-Za-z0-9-]+`; extra arguments and whitespace are rejected.

Codex account maintenance uses the same container gate and stores its model in
`codex_model_flag`, defaulting to `--model gpt-5.6-luna`. Codex model flags
accept dots via `--model [A-Za-z0-9.-]+`; both settings may be empty to use the
CLI default. Codex OAuth rotation is performed only by the isolated
harness-docker maintenance run;
usage reads never exchange refresh tokens or write `auth.json`.

### Never the active profile

Auto-refresh, warmup and Codex maintenance only ever touch **inactive**
snapshots. The active profile's token is in use by every harness running
against the live credential file, and they refresh it themselves. A server-side
refresh of the same account spends the same refresh token a second time, which
makes the provider revoke the whole token family and logs every session out.
The server therefore:

- never selects the active profile (the `active` flag, the implicit `local`
  profile of an install with no switcher profiles, or a stored credential file
  that is a link to the live one) for refresh, warmup or maintenance, and
  refuses it again when a run starts;
- leaves a profile alone for **8 hours after it stops being active** (recorded
  in `switcher/switched_away.json` by a switch or an import that moves the
  active pointer; a rename carries it, a removal drops it). Harness sessions
  started while the profile was active can still be running and refreshing the
  token they hold, so the stored snapshot is not yet safe to rotate;
- runs **one maintenance job at a time per account**: warmup, auto-refresh
  and Codex maintenance share an exclusion keyed by the profile's stored
  refresh token (the account), not its name, so a rename mid-run does not free
  a second slot. It is an advisory file lock under `switcher/.maintenance/`
  (one transient file per account, removed on release), so it also holds across
  processes (a second server, the `aimebu switcher` CLI);
- makes a **switch hold the target account's lock for the whole switch**,
  waiting up to 45 s for a run to finish and otherwise refusing with HTTP 409 /
  "a background token refresh of that account is still running; try again in a
  minute". The account lock is taken before the switcher lock (a run's
  copy-back takes the switcher lock while holding its account lock), so the two
  cannot deadlock. The lock is keyed by the stored refresh token and a run's
  copy-back replaces it, so the key is re-derived after every acquisition (a
  lock taken under a token that has since changed guards nothing). A switch
  therefore never lands mid-rotation and no run can start between the check
  and the swap. If the lock cannot be taken at all (the `.maintenance`
  directory is unusable) the switch is refused, and a background job is not
  started either: nothing falls back to unlocked operation;
- if a profile still became active during a run, the copy-back writes the
  rotated token through the link **only when the live file still holds the
  refresh token the run spent** (it is then the only usable token). If a
  harness already rotated the live token, nothing is overwritten and the
  rotated copy is kept as a
  `backups/<tool>/<profile>-rotated-<UTC timestamp>.json` rescue, logged at
  `ERROR` with its path.

### Claude warmup (idle 5-hour window)

Claude subscription usage is bucketed into rolling **5-hour session windows**.
A window only **starts on first use**, so an account that has been idle has no
active window and its capacity is not counting down. Warmup proactively kicks
one off: it runs a trivial `claude -p` as an idle account inside the same
throwaway `harness-docker` container that auto-refresh uses (the shared
executor), which opens the account's rolling 5-hour window server-side.

Warmup is an **automatic, mode-gated** poller feature — there is no button or
endpoint that triggers it on demand. Enable it in **Settings → Usages → Account
maintenance → Warm idle Claude and Codex accounts**, alongside the auto-refresh
toggle in the same bordered section. The stored `warmup_mode` is one of `off`,
`force`, `scheduled`, or `smart`. The GET/POST `/api/usages/settings` responses
expose it plus the runtime `warmup_available` flag; POST accepts optional
`warmup_mode` and `warmup_schedule` values.

- **Trigger — the background poller.** On each eligible poller tick (the same
  tick that drives auto-refresh), aimebu scans accounts whose 5h window
  is uninitialized. `force` warms every candidate, `scheduled` warms every
  candidate only when a cron line matches, and `smart` starts at most one
  candidate with spacing. `off` does nothing. Exactly one mode is active.
- **Chained to auto-refresh.** A non-off mode runs only when
  `token_auto_refresh` is enabled, the feature is available, and no tool has
  entered the token-only model. The mode selector is disabled when those
  conditions are not met. Legacy warmup reuses the same container path and
  must persist rotated tokens back the way legacy auto-refresh does.
- **Eligibility — out of the 5h window.** Any `claude` switcher profile with
  stored credentials is a candidate, **except the active profile**. An
  account is treated as “out of window” when its usages snapshot
  has **no active session (`five_hour`) window** — that is, no session window at
  all, or a session window whose `resets_at` is nil or already in the past. A
  session window with a **future** `resets_at` is active (even at 0%
  utilization) and is left alone. Snapshots that are not `ok` are skipped. This
  is judged from the last **persisted** snapshot (the on-disk usages cache),
  which may be up to one refresh interval stale — an account that just opened a
  window can still be selected until the next poll refreshes its snapshot; the
  1h rate-limit bounds the cost of acting on a slightly-stale read.
  > **Unconfirmed:** the exact shape the Anthropic usage API returns for a
  > long-idle account (omitted `five_hour` vs. present-but-stale `resets_at`)
  > has not been verified against real accounts. The reading above is the safe
  > superset; confirm it against a running server with real idle accounts.
- **Copy-back is mandatory.** Running `claude -p` performs OAuth
  refresh-on-use: for an idle/aged account (warmup's target set) the access
  token is typically expired, so Anthropic rotates the refresh token into the
  container's credential copy. Warmup therefore persists any rotated
  credentials back to the stored profile — discarding them would leave a dead,
  unrecoverable login. It reuses auto-refresh's locked, refresh-token-aware
  copy-back, so a switch during the run follows the named account. A re-login
  leaves the current profile untouched and saves the rotated file under
  `switcher/backups/claude/`; removal discards it. Warmup
  never targets the active profile (see
  [Never the active profile](#never-the-active-profile)).
  Unlike auto-refresh, warmup does **not** require `expiresAt` to advance: it
  copies back only when the credentials actually changed, and “no rotation” is
  still a success (the 5h window started regardless).
- **Rate limit — 1 hour.** Warmup success is only observable on the *next*
  usages snapshot (a warmed account then shows an active window and drops out of
  the eligible set), so it cannot be classified inline. Each account is
  therefore warmed at most **once per hour** regardless of outcome — distinct
  from auto-refresh's 5-minute post-failure backoff — plus the shared in-flight
  account lock so an account never has a warmup concurrent with another warmup
  or a refresh.
- **Scheduled mode.** `warmup_schedule` contains one five-field cron entry per
  line; blank lines are ignored. Scheduled mode scans when **any** line matches,
  using the server's local time, and deduplicates across all lines so a matching
  minute fires once. The built-in parser deliberately accepts `*` or one integer
  per field (minute, hour, day-of-month, month, day-of-week); lists, ranges, and
  steps are rejected. `0` and `7` both mean Sunday. When both day fields are
  restricted, either one matching follows standard cron behavior.
- **Smart mode.** One scan starts at most one account
  and different accounts are separated by a gap of **5 hours divided by the
  number of warmable credential-bearing inactive profiles**.
  Profiles whose active weekly limit is at 100% are excluded from both warmup
  attempts and this spacing count until that weekly window resets. Claude uses
  its `weekly` window for this gate; Codex uses either `weekly` or
  `codex_spark_weekly`. This spreads the remaining rolling windows roughly
  evenly without another setting. The
  longest-idle candidate is selected first: an account with no timestamped
  session window is treated as oldest, otherwise the earliest expired
  `resets_at` wins, with the profile name breaking ties.
- **Migration.** Configs written by the earlier independent controls migrate on
  load: disabled becomes `off`; enabled smart spacing becomes `smart`; otherwise
  a non-empty schedule becomes `scheduled`, and enabled with no schedule becomes
  `force`. The next config save removes the legacy fields.
- **Gate.** Warmup shares auto-refresh's availability gate: `harness-docker-ctrl`
  must be on `PATH` and docker must be reachable; additionally auto-refresh must
  be enabled, no tool may be migrated, and the mode must not be `off`. The multiline schedule editor
  appears only for Scheduled mode and identifies unsupported or out-of-range
  entries by line. Warmup is inert otherwise.

The right-sidebar force-refresh button calls `POST /api/usages/refresh`. It
bypasses the normal interval but has a separate server-side 15 second
cooldown. During cooldown the endpoint returns HTTP `429` with:

```json
{"retry_after_sec": 15}
```

The web sidebar uses the same provider-card and profile-row renderer for one
or many profiles. `switcher_enabled` controls only whether Switch buttons and
other credential-mutation affordances appear; it does not select another data
path or response representation.

The sidebar header's group button switches between provider grouping (the
default) and an active-first view. Active-first shows each provider's active
profile above a divider, then groups the remaining profiles by provider below
it. The browser stores this display-only preference locally; it does not alter
the shared usage settings or API response.

## Transient-Failure Handling

Provider usage fetches retry once for idempotent HTTP methods (`GET`, `HEAD`,
and `OPTIONS`) when a transient transport failure occurs or the provider
returns HTTP `408`, `429`, `500`, `502`, `503`, or `504`. The retry uses
exponential backoff starting at one second, caps delay at ten seconds, honors
`Retry-After` when present, and stops immediately if the request context is
canceled. Non-idempotent auth and device-flow `POST` requests are not retried
by default; Claude Code OAuth refresh is the narrow exception and retries one
`429` response when the request body can be replayed.

Codex OAuth refreshes are staged in memory and validated before the live
credential file is replaced atomically. A malformed success response or one
without a new access token is rejected without advancing `last_refresh` or
changing the existing login.

## Provider Ordering

Settings -> Usages includes up/down controls for the provider rows. The saved
order controls the vertical order in the web Usages sidebar for enabled
providers and the provider metadata order in `aimebu usages --json`. Empty or
older configs use the canonical order (`codex`, `claude-code`,
`github-copilot`, `mistral`, `ollama-cloud`); unknown entries are ignored and
missing known providers are appended.

## Stale Cache

When a provider fetch fails from a transient transport or server-side problem
after a previous successful snapshot exists, aimebu returns the previous plan,
windows, and credits with:

- `status: "stale_cache"`
- `stale: true`
- sanitized `error` / `error_detail` for the current failure

The CLI marks these rows with `(stale)`. The web sidebar shows the stale state
and error instead of presenting cached values as fresh.

Stale-cache preservation covers timeouts, canceled refreshes, DNS failures,
connection failures or resets, and HTTP 429 (rate-limit) and 5xx responses. A
transient `429` keeps the last good numbers marked stale rather than blanking
them. Credential and permission failures such as `auth_missing` and
`scope_missing` replace the cached snapshot so expired or unauthorized auth
does not look like live quota data.

Credit snapshots can include both current spend and a spend limit. The CLI
prints those as `used/limit`; the web sidebar shows the same pair in the
provider's credits row. Codex can return credits without recognized
rate-limit windows; aimebu keeps that as an OK credits-only snapshot instead
of treating the refresh as a failed response. GitHub Copilot token-billed
seats expose their raw `credits_used` count in the same row with the label
`Premium requests used`; aimebu does not invent a percentage without an
entitlement.

## Pace Marker

For windows with a known fixed duration — `session` (5 h) and `weekly` (7 d)
on codex, claude-code, and ollama-cloud — aimebu computes a linear-spend pace
marker and renders it alongside the usage bar.

**How it works**: given the window's full duration and time remaining until
reset, the expected used percentage at this moment is:

```
expected% = elapsed / duration × 100
delta% = actual used% − expected%
```

The sign of `delta%` determines the state:

| State | Meaning | Colour |
|---|---|---|
| **reserve** | under pace (delta < −2%) | green |
| **on_track** | within ±2% of pace | muted |
| **deficit** | over pace (delta > +2%) | red |

The web bar shows a small vertical **tick** at the linear-pace position.
Below the bar, a pace text line shows:
- **"X% in reserve · Lasts until reset"** — under pace, projected to not run
  out before reset.
- **"X% in deficit · Runs out in Nd Mh"** — over pace, projected to hit 100%
  before reset. The ETA counts down live in the UI.
- **"X% in deficit · Lasts until reset"** — over pace but the burn rate still
  projects finishing after reset.

The CLI appends pace text inline in the window cell:

```
session=43% (7% reserve · lasts to reset)
weekly=82% (12% deficit · runs out in 1d 4h)
```

Flat windows (`weekly_sonnet`, `codex_spark`, `codex_spark_weekly`) carry no
duration and show no pace marker.

GitHub Copilot monthly windows infer their duration from the quota reset date.
If the reset date is absent or invalid, the window remains available without
a pace marker.

Codex rate-limit lanes are classified by their reported duration: up to one
day is session, more than one day through 14 days is weekly, and more than 14
days through 31 days is monthly. Reset timestamps and durations remain in the
normalized snapshot.

Ollama Cloud cookie snapshots recognize session, weekly, and monthly usage
sections. Cookie setup accepts raw pairs, copied `Cookie:` request headers,
and common `curl` cookie forms; cookie values stay opaque even when they
contain text resembling a header name. Authentication messages distinguish
missing setup, unrecognized session cookies, and rejected cookie or API-key
credentials without echoing secret values.

The pace model is **purely linear**: it assumes a constant burn rate from
window start to now. No historical samples or probabilistic run-out estimates
are used. The computed `pace` object is included in the `Window` fields in
`aimebu usages --json` output. Depleted windows (`percent_used >= 100`) omit
the `pace` object because there is no remaining quota to project.

## Troubleshooting

- `auth_missing`: authenticate in the service's own CLI or complete setup in
  Settings -> Usages. Claude forbidden responses that explicitly identify an
  invalid or expired login use this status too.
- `scope_missing`: the token was accepted but lacks access to the usage
  endpoint. Re-authenticate or check the account's plan/enterprise policy.
- `timeout`: a provider request exceeded aimebu's per-request timeout. The
  provider may be slow or unreachable; retry later.
- `fetch_error`: the service returned an unexpected status or shape. Claude
  Cloudflare browser challenges use this status with a redacted challenge
  marker. The `error_detail.fields` map records field names and types only,
  never values.
- `stale_cache`: the latest fetch failed but cached values are still shown.
- `expired`: the Claude/Codex access token (stored profile or live account) is
  past its `expires_at`, so aimebu sends **no request** with it (Anthropic
  answers an expired bearer with HTTP 429, which used to look like a timeout).
  The last good windows are kept and marked `stale: true`; the profile row
  always carries `expires_at`. It clears as soon as the credentials are
  refreshed (by the opt-in maintenance, a harness, or a re-login). Clients such
  as the quota watchdog should treat it as "not ok, quota unknown" and hold.

Provider-specific setup and failure notes live in:

- [Codex](codex.md#usage-snapshots)
- [Claude Code](claude-code.md#usage-snapshots)
- [GitHub Copilot](github-copilot.md)
- [Mistral](mistral.md)
- [Ollama Cloud](ollama-cloud.md)

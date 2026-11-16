# Usage snapshots

Settings → Usages shows five provider cards: Codex, Claude Code, GitHub
Copilot, Mistral, and Ollama Cloud. The owner can enable providers, change
their order, set the refresh interval (default 120 seconds, minimum 15),
choose percent left or used, and refresh snapshots on demand. The cards are visible to the
owner and, redacted, to global admins; account details are visible only to the owner. Disabled providers keep their credentials until the
owner explicitly clears them. `AIMEBU2_USAGES=0` disables local credential
reads and usage requests, and `/api/usages` returns 503.

## Remaining-quota alerts

Settings → Usages sets global warning and critical thresholds (20% and 10%
remaining by default; each must be between 1 and 99, with warning above
critical). Each fresh, successful provider window that reaches a threshold
opens a global Inbox alert for the owner and global admins. The alert shows
the provider, profile name, window, remaining percentage, and reset time
inline. The owner alone gets an **Open Usages** action; the Usages cards and
`GET /api/usages` is the owner's whole snapshot; a global admin reads a redacted one (the limits and the profile names, nothing that names an account or how it signs in) and sees the cards read-only, and every change stays owner-only. Inbox alerts omit email and credentials.
The five-second background scan also notices switcher profiles added, renamed
or removed by a separate CLI process. It drops removed profile snapshots and
pushes a transient update so an open Settings → Usages view refetches its
switcher list and usage cards.
They trigger the normal Inbox sound and hidden-tab browser notification.

The server records one alert per threshold for each provider, account,
window, and reset cycle. A critical alert replaces that window's warning.
Stale or failed snapshots do not create alerts. Alerts survive a server
restart or account switch and close when a new reset cycle is observed, their
reset time passes, their account disappears, or their provider is disabled. An
admin can dismiss an alert without rearming it in the same cycle; then
the thresholds can fire again. An active limit guard holds new turns as
described in [automatic account switching](quota-switcher.md#limit-guard-acting-before-100)
and hides both warning and critical quota Inbox alerts for that tool. Alert
state is stored in private `usages/alerts.json` with mode 0600 (the `usages/` folder itself is 0700,
including one an earlier version made open); account
identity is hashed there. Stored profiles receive an opaque ID in their directory,
so renaming a profile keeps its alert identity and reusing its name does not.
The account switcher's profile overlay is applied before alert tracking, so
an active account and its stored profile share one identity and the alert
shows the profile name. A configured token, cookie, or key replacement resets
that local provider's alert threshold state.
Codex local alerts use a stable account or subject claim from the login file's
ID token, falling back to its plain `account_id` field. Claude Code does not
expose a stable account ID in the usage data
read here, so its local alerts cannot detect an account swap within one reset
cycle. Rotating OAuth tokens never rearm an alert. The first fresh observation
with a newly available stable identity after upgrading re-keys any open alert
once; it also re-arms a dismissed alert once in that cycle.
Reset timestamps within two minutes of the previous reset are one cycle despite
provider clock jitter. If the alert state is corrupt, the server logs the error,
moves the file aside with a `.corrupt-*` suffix, and starts with empty alert
state while continuing to serve.
Invalid saved alert thresholds fall back to 20/10 on load without discarding
other usage settings or credentials; settings writes still reject invalid values.

Codex and Claude Code read their local OAuth access tokens. Usage polling
never rotates or writes those files; a login that expired or was rejected is
renewed by running `claude` or `codex` yourself, unless **Auto-refresh tokens**
is on (below). The optional account switcher
still manages their named profiles; when it is enabled, every stored profile
gets its own read-only snapshot as well (see [switcher.md](switcher.md)).

Both fetchers live in `internal/usages/providers` beside the other providers
and send one bounded GET per refresh (a second attempt on a transient
failure, or when Claude answers with an empty body). An access token that has
already expired is never sent: the previous snapshot stays, marked stale, as
`token_expired`. Failures are classified rather than collapsed into one
message:

- `auth_missing` — no login file, an API-key-only Codex file, a `401`, or a
  Claude `403` whose body says the login is gone. After a `401` or a lost-login
  `403` no stale snapshot is kept, since the old numbers belong to a login that
  no longer works; a missing or unreadable file keeps the previous snapshot,
  marked stale. The message says which CLI to run.
- `scope_missing` — a `403` that is not a lost login.
- `fetch_error` / `timeout` — transport failures, `5xx`, undecodable bodies,
  and Claude's Cloudflare browser challenge (recorded in the error detail as
  `usage.challenge=cloudflare`). The previous snapshot (windows, plan and credits) stays, marked stale.

If Codex rejects the token, the fetcher re-reads `auth.json` once in case the
CLI rotated it meanwhile. Error text is redacted of the access token, and the
HTTP client drops credential headers on cross-origin redirects.

Claude Code windows: `session` (5 h), `weekly`, `weekly_opus`, `weekly_sonnet`,
and one `weekly_scoped:<model>` window for each per-model weekly limit the
account reports. Pace is computed for the session and weekly windows. When
extra usage is enabled the monthly cap shows as credits (`Extra usage monthly
cap`, used / limit). The plan is the subscription type, or the rate-limit tier.
Codex windows: `session`, `weekly` and `monthly` by their reported duration,
plus `codex_spark` and `codex_spark_weekly` from the additional Spark limits,
the credit balance when the account has one, and a plan name derived from
`plan_type` or the id token (`pro` shows as "Pro 20x", `prolite` as "Pro 5x"). GitHub Copilot, Mistral, and Ollama Cloud use a
single `local` profile. Their credentials are stored under the v2 config root
in `usages/config.json` with mode 0600. This uses the v1 JSON shape, including
`providers.<name>.token`, `cookie`, `api_key`, `auth_mode`, and
`enterprise_host`, so a v1 credential directory can be carried over without
rewriting the config. Cached snapshots and HTTP responses include configured
flags, not secret values.

Owner-only `GET /api/usages` only reads the cache. Background polling refreshes enabled
providers every configured interval. A credential, host, auth-mode, or enabled
change refreshes only the affected provider on the next five-second poller
tick; display-only settings edits keep all snapshots. Disabled providers show
`disabled`, including after their credentials are cleared, rather than waiting
for a refresh. A transient provider failure keeps a previous
successful snapshot marked `stale_cache`; rejected credentials report
`auth_missing` instead. Provider fetches use bounded HTTP requests and strip
credential headers from cross-origin redirects. Settings mutations require an
owner session and CSRF token. If `usages/config.json` is malformed, polling
continues with the last known provider enablement. A truncated or otherwise
invalid JSON file can be repaired from Settings. If the JSON is valid but a
field has the wrong type, settings saves are refused so saved credentials are
not lost; a literal `null` is also refused. Fix the field in the file first.

## Auto-refresh tokens (Account maintenance)

Settings → Usages → Account maintenance has an **Auto-refresh tokens** toggle
(`token_auto_refresh` in `usages/config.json`, `POST /api/usages/settings`). It is
the normal way to move an install to the token-copy model of
[switcher.md](switcher.md): on, aimebu becomes the only thing that refreshes the
Claude and Codex logins, and the server must stay running whenever you use
Claude or Codex, also outside aimebu (stopped for more than about an hour around a
token's expiry, sessions get auth errors until it runs again). Turn it off before
stopping aimebu for long or uninstalling it. The toggle is unavailable (off stays
possible) while `harness-docker-ctrl` is not on `PATH`, because the refresh runs
the real CLI in a throwaway container.

- **On** converts each tool on the server, in the background: the same as
  `aimebu switcher migrate-copies --tool <tool>`, whose safety refusals (a live token
  with under 15 minutes left, a running codex) become a per-tool
  `waiting: <reason>` that is retried about once a minute. **Off** returns each tool
  to the link model: the full credential goes back into the live file with every
  unrelated key kept (MCP logins included) and the profile is re-linked.
  For running Codex sessions, the per-tool reason gives only their count and says
  retries are automatic; process details stay in the server log. The manual
  `migrate-copies` refusal still includes those details. Before the automatic
Codex migration scans for running processes, the server asks the host
supervisor to retire idle Codex app-server processes. If their end cannot be
confirmed, migration waits and retries.
  Codex warm mode works with a ChatGPT subscription once token auto-refresh is
  on (Settings → Usages), or with an API key; otherwise the agent runs cold.
  Token-copy migration makes the subscription login eligible by writing a
  token-only target with no refresh token. A full Codex login runs cold even
  when the owner enables warm mode.
- The POST answers at once with every tool `pending`; the worker then answers per
  tool, and the UI re-reads until nothing is pending. `token_auto_refresh_state`
  in the settings maps `claude` and `codex` to `{status, reason?}`:
  `off`, `migrated`, `pending`, `waiting` (with `reason`), or `n/a` (no active
  profile, nothing to convert). Before the first pass the state is seeded from the
  migration marker, so a migrated tool never shows `off` next to a checked toggle.
  `token_auto_refresh_available` says whether the executor can run.
- One worker applies the setting (at start, on every change, every 5 seconds), so a
  tick and a toggle never run at once, and a slow account lock or codex scan cannot
  delay usage polling. `token_auto_refresh_managed` records that the setting owns
  what it migrated: a manual `migrate-copies` done while the setting was never on is
  left alone, and the flag clears once an off has returned every tool.
- `claude_auto_refresh` is the key the setting had in v1. It is read when the new
  key is absent (a legacy ON carries over and owns cleanup), accepted on the POST
  (the new key wins), reported read-only in the settings, and written beside the
  new key on every save for one release so a downgrade keeps the toggle.
- While a full login waits in a tool's live file (a `/login`, or an interrupted
  turn-off), the refresher does not spend the active profile's stored token.

## Scheduled idle-account warmup (Account maintenance)

Settings → Usages → Account maintenance also offers **Warm idle Claude and Codex
accounts** (`warmup_mode` and `warmup_schedule` in `usages/config.json`). Warmup
runs only while Auto-refresh tokens is on and at least one tool has completed its
token-copy migration. `off` is the default. `force` warms every eligible
profile on a poller scan; `scheduled` warms on a matching local-server-time
minute; `smart` starts at most one account per scan and spaces starts by five
hours divided by the count of eligible accounts. The selected mode is preserved
if the executor or migration becomes unavailable, but does no work then.

An eligible account is an inactive stored Claude or Codex profile with complete
credentials and a persisted `ok` usage snapshot whose session window is absent
or has no future reset. A current weekly window at 100% excludes it. Active
profiles, profiles pinned to an external credential target, linked credential
files (including a link to another tool's live file), and profiles sharing a
refresh token with any of those are excluded. Idle profiles sharing a token
count as one account and make at most one request. A profile just switched away
keeps the two-minute grace, and Codex stays quiet for ten minutes after token
copy migration. The executor makes one short CLI
request without expiring or blanking the copied access token. Each account is
attempted at most once per hour, including failures; the last attempt lives in
`switcher/warmup-last.json` (0600, no credentials). A scheduled matching minute
fires once even when multiple cron lines match. If a busy poll misses the
minute, the next scan catches it up within five minutes. Invalid warmup fields
in an existing config are reset to `off` and an empty schedule on load; the
repaired values are persisted on the next save so other modes remain usable.

The schedule is one five-field cron entry per line: minute (0–59), hour (0–23),
day (1–31), month (1–12), weekday (0–7, Sunday is 0 or 7). A field accepts
`*` or one integer; lists, ranges, and steps are rejected. When both day
fields are restricted, either matching day fires. The schedule editor shows
only for Scheduled mode and saving validates every line.

A CLI can still rotate its OAuth grant on use. Warmup runs under the same
per-account maintenance lock and spend-once journal as token refresh; a rotated
credential is rescued and copied back to every matching stored profile under
the switcher lock, then fanned out to token-only targets. A started request
with an unknown failed outcome is held from being tried again with that grant.
An unsent request raises a refresh-failed alert without changing the account's
terminal state. A started CLI keeps the one-hour attempt interval; a CLI that
never started records no attempt.

## Automatic account switching (Settings)

Settings → Usages → **Automatic account switching** is the owner's control panel for
the quota switcher (the rules and the runner are described in
[quota-switcher.md](quota-switcher.md)). It appears only for an owner while the account
switcher is enabled, and is hidden if the server does not serve it. Per tool (Claude,
Codex) it shows and edits:

- **Switch automatically** and **Dry run**. Both are saved with the rest of the
  configuration. In dry run the server records what it would do and switches
  nothing; the section says so while an enabled tool is in dry run.
- **Profile order**: one row per profile with its session and weekly limits (whole
  percentages, 1 to 100), Up, Down and Remove buttons, and an Add select of profiles
  that are not yet listed or protected. The first usable profile in the order wins.
- **Protect <name>** toggles: a protected profile is never switched to or away from
  while active and is taken out of the orders when protected.
- **Use a different order after a reset**: the watched profile, the drop in points
  that counts as a reset, and the order that applies afterwards. Once a reset is
  seen the section says when, and **Re-arm** clears it by hand (it never re-arms
  itself).
- **Advanced**: warm-up seconds (0 to 600) and the minimum minutes between switches
  (0 to 120). The minimum time also delays a switch away from an exhausted account
  that comes right after a switch.
- Status: the last decision, the current hold, when the server last evaluated, and
  the **Recent decisions** list (the last 50, each with the per-profile status and
  usage it was based on). When the circuit breaker has paused a tool, a banner with
  **Re-enable switching** appears; check the order and limits before re-enabling.

Defaults come pre-filled from the store's profile names and are labelled as such
(`saved: false`): the server never acts on them until the owner presses **Save**
once, and Save is open for them even unchanged. Edits are a local draft: **Save**
sends the whole configuration (`PUT /api/quota-switcher/{tool}`; it waits for a
running switch, so the note reads "Saving… (waits for a running switch)" and Save is
disabled until the answer) and **Discard changes** drops the draft; a live push never overwrites a draft being edited. All
reasons and names the server sends are shown as plain text. The calls are
`GET /api/quota-switcher`, `PUT /api/quota-switcher/{tool}`, and
`POST /api/quota-switcher/{tool}/rearm` and `/reenable`, all owner-only with the CSRF
token.

Live updates: whenever a snapshot is written (a poll, a forced refresh, a
credential or enablement change, an account switch), the server sends the
owner's and global admins' open event sockets a transient
`{"type":"usages_updated","alert_revision":N}` frame. It carries no usage
snapshot and no `seq`, and is never logged or replayed. The revision advances
when a synthetic Inbox usage alert changes or is dismissed, or a limit guard
hold changes. Every global admin receives this
hint, including a hold release caused by a stale or failed reading that sends
no system event. The browser reads Inbox when the revision advances and on the
first frame after each connection; the owner also refreshes the Health hold
badge. Stored Inbox items follow their own event stream updates;
the owner also re-reads `GET /api/usages`, so an
open Settings → Usages panel updates without a reload. While a field in the
panel has focus the reload is held so typing is not lost, and it runs once focus
leaves; a push that arrives during a reload triggers one more reload afterwards.
The top-bar Usages icon opens the same data (every configured provider and
profile, active profile first) in the right-hand column of the chat, where the
room-members column normally sits, as in v1. The icon, the ✕ in the panel header
or Escape closes it and the members column comes back; clicking elsewhere leaves
it open. It re-reads when opened or refreshed and never changes anything. The panel header holds v1's
toolbar (✕ close, ↻ refresh, group by) with the title on the right; the group-by button puts
each provider's active profile first, then a divider and the other profiles, and is remembered
in this browser. Each profile shows v1's token-expiry icon (healthy, expiring, expired; from
`GET /api/credentials/targets`, owner only) and, when the account switcher is on, a SWITCH pill on
the inactive ones that confirms and calls `POST /api/switcher/{tool}/use`.

The top-bar usages column (the owner and global admins; Refresh and Switch are the owner's) follows the same push while it is
open, and its Refresh button calls the same forced refresh as Settings → Usages
(`POST /api/usages/refresh`), then re-reads the overview. Inside the cooldown it
keeps what it shows and says how long to wait; the cooldown is shared with
Settings.

Forced refresh cooldown: `POST /api/usages/refresh` ("Refresh now") may run
once per 15 seconds, counted across all providers. A second call inside the
window answers `429` with a `Retry-After` header and
`{"error":"cooldown","retry_after_sec":N}` and starts no fetch. An unknown
provider is rejected with `400` before the cooldown is consulted. Background
polling is not affected.

`aimebu usages [provider] [--json|--plain]` prints the table the server last
cached (provider, profile, status, plan, windows with pace, credits); with
`--json` it prints the same shape as `GET /api/usages`. It reads
`usages/cache.json`, the saved `usages/config.json` (enablement, order) and the
switcher profiles straight from `AIMEBU2_CONFIG_DIR`, the way `aimebu sessions`
reads SQLite, rather than calling the API (which needs a login session and CSRF
token). It makes no network requests and writes nothing: the switcher is
opened read-only, so even the account-email note that the server records beside
a Claude profile is not saved. The only credential files it reads are the
switcher profiles' own (the live login for the active profile), to report
whether a login exists, when it expires and which account it is; tokens are
never printed. It works with the server stopped; it only shows what the server
cached, and
says so on stderr when no snapshot exists or the cache is older than twice the
refresh interval. Cached values are shown as written (`stale` as the server
marked it). `GET /api/usages` stays the source of live data.

In Settings → Usages and in the header popover, a profile whose data is older
than twice the refresh interval (never less than five minutes) is shown as
stale: its bars and figures are grey, it claims no pace, its reset is the time
it was due or "Reset passed" instead of a ticking countdown, and a line says
how old the data is ("Stale, last update 2d ago") with a Refresh button for the
owner. Fresh profiles beside it are unchanged. A provider's "Updated ... ago" is
the age of its oldest profile, so it never claims to be fresher than a card
below it.

Each window also carries a short history, drawn as a sparkline under its bar
in Settings → Usages and in the header popover. The server keeps a bounded
ring per provider, profile and window in `usages/history.json` (mode 0600, not
in SQLite): one sample of the used percentage at most every 15 minutes, only
from a fresh successful snapshot (a stale or failed read leaves a gap, not a
flat line), the last 48 hours and at most 193 samples per series. It survives a
restart, a removed profile takes its series with it, and a credential swap on
the live account starts that account's series over, since the old samples
belong to another login. The samples are added to `GET /api/usages` as
`windows[].history` (`{t: unix seconds, p: percent used}`) and are not part of
`cache.json`; the line plots used or left to match the percent display.

The sparkline is labelled: "48 h ago" under its left end, "now" under the right end and, between
them, what the line plots ("% used" or "% left", last 48 h). The pace line ("N% in reserve", "N% in
deficit", "Lasts until reset", "Runs out in …") explains itself in a tooltip on hover and for keyboard
focus: pace compares what has been used with an even spend across the window; in reserve means behind
that pace, so the limit should last until it resets, and in deficit means ahead of it, so at this rate
the limit runs out first. The tooltip also gives the share an even spend would have used by now.

- [GitHub Copilot setup and quota shapes](github-copilot.md)
- [Mistral Vibe quota and spend](mistral.md)
- [Ollama Cloud cookie and API-key modes](ollama-cloud.md)

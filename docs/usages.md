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
live-credential fetch so Codex token rotation is persisted safely; inactive
stored profiles use a read-only fetch that never rotates their credentials.
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

## Claude Auto-Refresh

Claude OAuth access tokens live only about 8–12 hours. On macOS the live
harness refreshes them lazily through the Keychain, but a stored **inactive**
switcher profile has no process keeping it fresh, so its token silently lapses
and switching to it forces a browser re-login.

Claude auto-refresh keeps near-expiry inactive `claude` switcher profiles ready
to switch to. When enabled, the background poller renews any inactive profile
whose access token is already expired or within 10 minutes of expiry by running
the real `claude` binary inside a throwaway `harness-docker` container pointed
at a copy of that profile's credentials. Inside the container there is no
Keychain, so claude performs its file-based refresh-on-use and writes rotated
tokens straight back, which aimebu then copies into the stored profile.

Design notes:

- **Inactive-only.** The active profile is never touched — the live harness
  owns it. Codex profiles are unaffected; this is claude-only.
- **Temp-dir isolation.** claude pollutes its config dir with `.claude.json`,
  `sessions/`, `projects/`, and `policy-limits.json`. aimebu copies only
  `.credentials.json` into a temp dir, runs the container there, and copies only
  the rotated `.credentials.json` back — the stored profile dir stays clean.
- **Success is the rotated file, not the exit code.** A refresh counts only when
  the new `.credentials.json` parses and its `expiresAt` advanced beyond the old
  value.
- **Copy-back safety.** The slow container run happens outside the switcher
  lock. The final copy-back holds `switcher/.lock` and re-checks, compare-and-swap
  style, that the profile still exists, is still inactive, and its stored
  credentials are byte-identical to what the refresh started from. A switch or
  removal that landed mid-refresh aborts the copy-back rather than clobbering it.
- **Rate limiting.** A profile is never refreshed twice concurrently, and after
  a failed attempt it is not retried for 5 minutes.

**Gate.** The feature runs only when all of these hold: the
`claude_auto_refresh` setting is enabled (default **off**), the
`harness-docker-ctrl` binary is on `PATH`, and the docker daemon is reachable.
When `harness-docker-ctrl` is absent the feature is reported unavailable and the
Settings toggle is disabled.

**Cost.** Enabling it incurs a small per-account cost — occasional `claude -p`
calls against each near-expiry inactive profile — so it is opt-in.

Configure it in **Settings → Usages → Claude auto-refresh**. The stored flag is
`claude_auto_refresh` in `~/.aimebu/usages/config.json`; the GET/POST
`/api/usages/settings` responses expose `claude_auto_refresh` and the runtime
`claude_auto_refresh_available` availability flag. `POST /api/usages/settings`
accepts optional `claude_auto_refresh` and `claude_auto_warmup` booleans
alongside `refresh_interval_sec` and `percent_display`.

### Claude warmup (idle 5-hour window)

Claude subscription usage is bucketed into rolling **5-hour session windows**.
A window only **starts on first use**, so an account that has been idle has no
active window and its capacity is not counting down. Warmup proactively kicks
one off: it runs a trivial `claude -p` as an inactive account inside the same
throwaway `harness-docker` container that auto-refresh uses (the shared
executor), which opens the account's rolling 5-hour window server-side.

Warmup is an **automatic, toggle-gated** poller feature — there is no button or
endpoint that triggers it on demand. Enable it in **Settings → Usages → Claude
account maintenance → Warm idle Claude accounts**, alongside the auto-refresh
toggle in the same bordered section. The stored flag is `claude_auto_warmup` in
`~/.aimebu/usages/config.json`; the GET/POST `/api/usages/settings` responses
expose `claude_auto_warmup` and the runtime `claude_auto_warmup_available`
availability flag, and `POST /api/usages/settings` accepts an optional
`claude_auto_warmup` boolean.

- **Trigger — the background poller.** On each poller tick (the same tick that
  drives auto-refresh), aimebu warms every eligible inactive account whose 5h
  window is uninitialized. “Force” only in the sense that the rule is simply
  *uninitialized → initialize now* — there is no smart pacing or schedule beyond
  the per-account 1h rate-limit.
- **Chained to auto-refresh.** Warmup runs only when **both** `claude_auto_refresh`
  **and** `claude_auto_warmup` are enabled (and the feature is available). The
  warmup toggle in the UI is disabled while auto-refresh is off. This is
  deliberate: warmup reuses the same container path and must persist rotated
  tokens back the way auto-refresh does.
- **Eligibility — out of the 5h window.** Only **inactive** `claude` switcher
  profiles with stored credentials are candidates (the active one is warmed by
  normal use). An account is treated as “out of window” when its usages snapshot
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
  unrecoverable login. It reuses auto-refresh's compare-and-swap copy-back (same
  `switcher/.lock` + re-resolve guard), so a switch/removal that lands mid-run
  is never clobbered and the active profile is never touched. Unlike
  auto-refresh, warmup does **not** require `expiresAt` to advance: it copies
  back only when the credentials actually changed, and “no rotation” is still a
  success (the 5h window started regardless).
- **Rate limit — 1 hour.** Warmup success is only observable on the *next*
  usages snapshot (a warmed account then shows an active window and drops out of
  the eligible set), so it cannot be classified inline. Each account is
  therefore warmed at most **once per hour** regardless of outcome — distinct
  from auto-refresh's 5-minute post-failure backoff — plus an in-flight guard so
  an account never has two concurrent warmups.
- **Gate.** Warmup shares auto-refresh's availability gate: `harness-docker-ctrl`
  must be on `PATH` and docker must be reachable; additionally both the
  `claude_auto_refresh` and `claude_auto_warmup` settings must be enabled. It is
  inert otherwise.

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

Provider-specific setup and failure notes live in:

- [Codex](codex.md#usage-snapshots)
- [Claude Code](claude-code.md#usage-snapshots)
- [GitHub Copilot](github-copilot.md)
- [Mistral](mistral.md)
- [Ollama Cloud](ollama-cloud.md)

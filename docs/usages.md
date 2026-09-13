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
provider carries a `profiles` array with **one entry per switcher profile**, or
a single entry named `"default"` when the provider has no switcher concept.
A profile name is never empty.

A `Profile` carries **both** the usage data and the switcher facts for that
account: `profile_name`, `active`, `has_credentials`, `expires_at`, `email`,
plus `status`, `plan`, `windows`, `credits` and `last_refresh_at`. That is
deliberate — there is no join between usage data and account data, so there is
nothing for the two to disagree about. Switch eligibility is per tool and lives
on the provider (`switch_eligible`, `switch_ineligible_reason`); only the global
on/off flag sits at the top level.

## Refresh Behavior

The stored refresh interval defaults to `120` seconds and has a minimum of
`15` seconds. `AIMEBU_USAGES_REFRESH` overrides the stored value for both the
server and CLI.

`GET /api/usages` is a **pure cache read** — it never triggers a network
fetch. All provider and per-profile fetching is owned by the background
poller (5-second tick, fetches when the interval elapses) and the
force-refresh button. On a cold cache (server restart), the first poller tick
(~5 seconds) warms the cache; until then the response carries empty arrays.

**Per-profile refresh:** inactive switcher-profile snapshots refresh on a
floor of **1 hour** (`max(provider interval, 1h)`), so they never poll more
often than their provider. The **active** profile is exempt — it's derived
from the provider fetch and refreshes on the provider's interval for free.
After a switch, `InvalidateProfileSnapshot` drops the old entry, so the next
poller tick (~5 seconds) refetches immediately regardless of the interval —
the `!ok` short-circuit means absent entries always fetch now. If that
post-switch fetch fails, aimebu still publishes a named active-profile row
with the failure status (`timeout`, `fetch_error`, `auth_missing`, and so on)
instead of leaving the provider's `profiles` array empty. That distinguishes
"this configured profile is refreshing or errored" from a genuinely
unconfigured provider in the web sidebar.

Claude and Codex fetches are bound to the active profile name and a fingerprint
of the credentials that started the request. If either changes while the
network request is running, aimebu discards the late result instead of caching
one account's usage under another profile. The switcher lock is not held while
waiting on the provider. A credential-file change also bypasses an otherwise
fresh cache entry so CLI token rotation is picked up on the next poller tick;
an in-flight rotation leaves the previous cache entry intact until a fetch
owned by the new credentials completes.

The right-sidebar force-refresh button calls `POST /api/usages/refresh`. It
bypasses the normal interval but has a separate server-side 15 second
cooldown. During cooldown the endpoint returns HTTP `429` with:

```json
{"retry_after_sec": 15}
```

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
connection failures or resets, and HTTP 5xx responses. Credential and
permission failures such as `auth_missing` and `scope_missing` replace the
cached snapshot so expired or unauthorized auth does not look like live quota
data.

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

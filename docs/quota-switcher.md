# Automatic account switching (quota switcher)

For each of Claude and Codex the owner defines an **ordered list of profiles**.
On every usage snapshot the server picks the first profile in the list that is
*usable* and, if it is not the active one and no safety rule says wait, switches
to it with the same `Switcher.Switch` the web UI and the CLI use. If nothing is
usable it stays where it is. It replaces an out-of-tree watchdog script; the
design and its reasoning are in
[design/quota-switcher.md](design/quota-switcher.md).

It needs the account switcher enabled and the usage reader running
(`AIMEBU2_USAGES` not `0`). It is **off by default**; enabling it starts in
**dry run**, which computes and records every decision and switches nothing.

## What "usable" means

A profile is usable when all of these hold:

- its usage snapshot is `ok` and not stale, and has been continuously ok for the
  warm-up (60 s by default). Time is the usage reader's own: re-reading one old
  snapshot does not warm a profile up, and a reading older than three refresh
  intervals (at least ten minutes) is stale, so a stalled usage poller makes the tool
  hold instead of acting on old percentages;
- its session usage is below its session limit (a profile that reports no session
  window is not limited by it), and its weekly usage is below its weekly limit (a
  profile with no weekly figure is not usable). Limits are percent used and the
  comparison is strict: a profile at exactly its limit is exhausted;
- it has stored credentials and the token refresher has not parked it
  (`reauth_required`, `needs_relogin_soon`).

## The decision, in order

1. Off, or fewer than two listed profiles exist: idle. (Enabling a tool with fewer than two
   profiles, or with an after-reset order of fewer than two, is refused when saving: once the reset
   latches the after-reset order replaces the main one, and a single entry could never switch.)
2. The tool tripped the circuit breaker: hold until the owner re-enables it.
3. No active profile, or a **protected** profile is active: hold. Protected
   profiles are never switched away from or to.
4. The active snapshot is not ok: hold (never switch on unclear data).
5. Any ok profile of the tool has been ok for less than the warm-up: hold.
6. Take the first usable entry of the order. None usable: stay put.
7. It is the active profile: stay.
8. On a tool that is **not** on the token-copy model, the active or target stored
   token expires within 15 minutes (or its expiry is unknown): hold. A migrated
   tool instead refreshes the incoming profile under its account lock. Both the
   outgoing and incoming account locks are held through the switch, so an
   in-progress token refresh or warm-up on either side cannot race the swap.
9. The last automatic switch of the tool was less than the minimum dwell ago
   (5 minutes by default): hold. This applies even when the active profile is
   exhausted; the dwell is the guard against two profiles flapping.
10. Switch. The reason says why: "a higher-priority profile is usable again", "the
    active profile is over its limit or unusable", or "the active profile is not in
    the order". The last one is the rule for an active profile the owner never listed
    (and, once the reset has latched, one that is not in the after-reset order): the
    switcher reverts to the order, it does not leave an unlisted profile active. Protect
    a profile you want left alone.

Every outcome is recorded as a decision with a fixed reason sentence; only changes
are recorded, not every evaluation. The newest 50 per tool are kept. Each change
to a hold reason also appears as one `quota_hold` System event, including across
server restarts; repeated evaluations of the same hold stay quiet. The decision
and its System event commit together, so one cannot be stored without the other.

## What happens around the switch

- **Running turns wait.** If an agent of the tool is in a turn, the switch is held
  (`agents running`) and retried; it never interrupts. This is the same check the
  web switch makes.
- **Account maintenance waits.** Refresh or warm-up of either the active or the
  target account holds the switch. The switch acquires distinct account locks in
  a fixed order before the switcher lock; if the active profile changed while
  waiting, the automatic switcher refuses the stale decision and evaluates
  again. A manual switch retries against the newly active profile; if its
  target is active already, it returns a no-op.
- Other refusals from the switcher are retryable holds with a fixed reason
  (`account busy`, `incoming refresh failed`, `login pending`); a real failure is
  an `error` decision (its text goes to the server log only). A held or failed
  switch is retried after 30 s, 1 min, 2 min, then every 5 min. A failure is
  announced (`quota_switch_failed`) the first time and again only when it changes,
  not on every retry; the failed attempts count toward the circuit breaker below.
- **Circuit breaker.** A fifth automatic switch attempt of a tool within an hour
  (a switch or a failed attempt) is refused and trips the breaker (`quota_switcher_tripped`,
  reason "too many automatic switch attempts this hour"); the tool holds until the owner
  re-enables it.
- While a switch is in flight (it can wait on the account lock or an incoming refresh,
  up to a few minutes) reading the settings keeps working, but saving them waits for
  the switch to end, so the settings page can take that long to say "Saved".
- A successful switch announces `quota_switch` in the owner's system events and
  refreshes the usage snapshot like a web switch.

## The after-reset order

A tool can carry a second order that applies once a watched profile's weekly window
has reset: the profile's weekly usage dropping by `drop_points` (default 20) below
the highest ok value seen. Only an ok snapshot moves that maximum. The latch is
persisted, announces `quota_reset_seen` once, and is **re-armed by hand only**
(`POST /api/quota-switcher/{tool}/rearm`).

## Configuration

Per tool (stored in the server database, so plain `aimebu prune` keeps it and
`aimebu prune -a` removes it with the rest):

| Field | Meaning |
| --- | --- |
| `enabled`, `dry_run` | off by default; dry run records but never switches |
| `profiles` | the order; each `{name, session_max, weekly_max}` (1 to 100) |
| `protected` | profiles never switched from or to |
| `after_reset` | `{profile, drop_points, profiles}` or `null`; enabling with fewer than two `after_reset.profiles` is refused |
| `warmup_seconds` | continuous ok time before a profile counts (default 60) |
| `min_dwell_minutes` | minimum between automatic switches (default 5) |

With nothing saved, a tool shows a default pre-filled from the profiles that exist
(in listing order) with the thresholds of the original rules: Claude 95/95 for every
entry; Codex 93/98 for the first entry and 98/98 for the rest. The names always come
from your own profiles. Saving validates every name against the store.

## API

Owner session; writes need the CSRF token; `503` until the quota switcher is running.

- `GET /api/quota-switcher`: `{profiles: {claude, codex}, tools: {claude, codex}}`
  where each tool is `{config, saved, state, last_decision, decisions}`.
- `PUT /api/quota-switcher/{tool}`: replace a tool's config (400 on validation).
- `POST /api/quota-switcher/{tool}/rearm`: clear the reset latch.
- `POST /api/quota-switcher/{tool}/reenable`: clear the circuit breaker.

`state` is `{reset_latched_at, weekly_max_seen, last_switch_at, tripped,
tripped_at, hold, evaluated_at}`; a decision is `{at, tool, action, from, to,
reason, dry_run, profiles}` with `action` one of `idle`, `stay`, `hold`, `switch`,
`error`. The warm-up clock is kept in memory: after a restart the server waits one
warm-up before it acts.

## Limit guard: acting before 100%

The limit guard works on the **active** profile of each tool, ahead of the switcher's own limits. It
reads the fuller of the session and weekly percentages and compares with `>=`:

- **soft** (default: the tool's own switcher limit, Claude 95 and Codex 98): the server dispatches no *new* turn to agents of the tool
  **on the host machine** (the account the usage reader and the switcher watch; an agent on a remote machine has its own
  login and is not held); queued notes wait.
  A turn already running finishes, which also lets a switch happen (a switch waits for running turns).
  The switcher's limits are **clamped to the soft threshold** (each profile's own limit or the soft
  one, whichever is lower), so the soft threshold is where it leaves a profile: with another profile
  usable it moves at once and the hold lifts with it. With no usable profile it stays, and the hold
  lasts until usage falls back under the threshold (a window reset).
- **hard** (default 99): the profile is at its limit. New turns stay held, and the owner gets one
  `limit_guard_hard` system event. Both notices say what happens next: `next` is `switching` when the
  switcher could move off the profile now (it is enabled, not a dry run, not tripped, the active
  profile is not protected, and a listed profile is usable under the soft threshold), else `held`.
  Nothing is paused or stopped: the hold clears itself, and agents keep their sessions.

Only a healthy, fresh reading is acted on, with the same ageing as the switcher (a reading older than three
refresh intervals, at least ten minutes, is stale): a stale or failed snapshot releases the hold without an
owner system notice, while the socket still hints that the visible hold changed.
A flapping snapshot does not repeat a notice. The hold is in memory and re-derived within
seconds of a restart. Events are `limit_guard_soft`, `limit_guard_hard` and `limit_guard_clear`, once
per change of state.

Settings -> Usages -> **Limits before 100%** (owner only) edits the thresholds per tool and gives a profile
its own, for one that should be held earlier or later than the rest. The **Health** list starts with a bar
per tool whose active profile has a clear reading, with the soft and hard thresholds ticked on it; a tool
that is holding new turns also counts toward the Health badge. The rows are re-read every 30 seconds while
the list is open and when it opens. Hold changes refresh the badge even when the list is closed,
including a release after a stale or failed reading.

It is on by default (soft Claude 95 / Codex 98, hard 99, so by default nothing the switcher does changes) and independent of the switcher's `enabled`. A tool with no stored
profile is protected too, through its one implicit `local` profile. Settings are
per tool, with optional per-profile `{soft, hard}` overrides (soft below hard, both 1 to 100), stored as
the global setting `limit_guard.<tool>`; no migration was needed. `GET /api/limit-guard` returns
`{profiles, tools}`: `profiles` lists the profile names that exist per tool, and each tool in `tools` is
`{config, saved, result, held, since}` (`result` is `{state, profile, percent, window, soft, hard}`,
`state` one of `off`, `unknown`, `ok`, `soft`, `hard`),
and `PUT /api/limit-guard/{tool}` replaces a tool's config (400 on validation, 404 unknown tool, 503
until the quota switcher is running). Both travel in the settings backup (`quota_switcher` and `limit_guard`). The guard is separate from the per-day cost and turn caps
(`usage_limit`), which are about spend per room or project, not provider quota.

## Rollback

The quota switcher adds a database migration (three tables for its configuration,
state and decision history; nothing existing is altered). A binary that predates it
refuses the newer schema (`database version N is newer than supported version M`),
so going back to an older build means restoring the pre-migration backup of
`aimebu.sqlite` with the server stopped (startup writes it to
`server/backups/aimebu-pre-migrate-<from>-to-<to>-<UTC>.sqlite` before it
migrates; see the [migration guide](migrating-from-v1.md)). The quota switcher's own settings and history are lost
with that restore, since they were written after the backup. Nothing outside the
database changes: the switcher's profiles and credentials are untouched either way.

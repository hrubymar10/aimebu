# Quota switcher: automatic account switching as a v2 server feature

Status: implemented (see [../quota-switcher.md](../quota-switcher.md) for the user-facing description). Author: sage. Replaces the out-of-tree watchdog
`usage-policy.py` (a Python loop that polls `/api/usages` every 12 s and POSTs a
switch). Behaviour reference: that script, rules approved by the owner in the
bus history; this document restates every rule it implements and says where v2
should differ and why.

## 1. What it does, in one paragraph

For each of Claude and Codex, an owner-defined **ordered list of profiles**. On
every usage snapshot the server picks the first profile in the list that is
*usable* (session and weekly usage under that profile's limits) and, if it is not
the active one and nothing says "hold", switches to it through the same
`Switcher.Switch` the web UI and CLI use. If nothing is usable it stays where it
is. Everything that makes the watchdog safe today (the holds) becomes an explicit,
visible, tested part of the policy instead of folklore in a script.

## 2. Where it lives

```
internal/quota/
  config.go     types + validation of the per-tool settings (no I/O)
  policy.go     Decide(): a PURE function, snapshot + config + state + now -> Decision
  runner.go     the loop: reads snapshots, calls Decide, acts, records, backs off
  store.go      persistence (settings + latch state + recent decisions)
internal/server/quota.go     owner-only routes, wiring in StartTokenCopies' sibling
web/js/quota-settings.js     the Settings UI section
docs/quota-switcher.md       the user-facing doc (written with the code)
```

The pure `Decide` is the heart. It takes no clock, no network, no files: every
input is a parameter, so every rule below is a table-test row. The runner is thin.

It runs **in process** and reads the usage reader's snapshot directly
(`usages.Reader.Snapshot()` after `Switcher.OverlayUsages`, the same merge the web
shows), not over HTTP, so there is no auth, no 12 s polling and no second copy of
"what is the active profile". It is started next to the token refresher
(`Server.StartTokenCopies`'s sibling `StartQuotaSwitcher`), idempotent like it, and
driven by the reader's update hook plus a slow ticker.

## 3. Configuration (per tool)

```
quota_switcher: {
  claude: {
    enabled: false,               // default off; dry_run first (see 8)
    dry_run: true,
    profiles: [                    // ORDER MATTERS: first usable wins
      { name: "main",      session_max: 95, weekly_max: 95 },
      { name: "bkp",       session_max: 95, weekly_max: 95 },
      { name: "bkp-tomas", session_max: 95, weekly_max: 95 },
    ],
    protected: ["do-not-touch-tomas"],   // never switched away from or to
    after_reset: {                        // optional second order, see 5
      profile: "main", drop_points: 20,
      profiles: [ bkp, main, bkp-tomas ... ],
    },
    warmup_seconds: 60,            // continuous ok before a profile counts, see 4
    min_dwell_minutes: 5,          // anti-flap, see 7
  },
  codex: { enabled, profiles: [ {main, session_max: 93, weekly_max: 98},
                                 {bkp,  session_max: 98, weekly_max: 98} ], ... },
}
```

- **Thresholds are per list entry**, because the script's Codex rule is exactly
  that (`main` while session < 93 and weekly < 98, else `bkp` while both < 98).
  Claude's are 95/95 for all three. All comparisons are strict `<` on
  `percent_used`.
- A **session** value of `null` (the provider did not report a session window, as
  Codex `bkp` does) counts as *not limiting*; a **weekly** value of `null` makes
  the profile *unusable* (the script requires `weekly is not None`). Both are kept.
- **Protected** profiles are not in any order list and are never a target. While
  one is the active profile the tool is on **hold** ("protected profile is
  active"): the owner parked it there on purpose (`do-not-touch-tomas`).
- `profiles` must name existing profiles; unknown names are a validation error at
  save and a visible warning if the profile disappears later (the entry is skipped,
  never silently dropped from the saved config).
- Validation: at least two usable entries to enable; thresholds 1..100; no
  duplicates; a profile cannot be both listed and protected.
- **Defaults are pre-filled from the owner's own profiles, never from names in
  code.** With no saved config a tool's form is pre-filled from the profiles that
  exist in the store (in listing order) with today's thresholds by position: Claude
  95/95 for every entry; Codex 93/98 for the first entry and 98/98 for the rest. No
  profile name appears in code, fixtures or docs; tests use synthetic names.

**Storage.** SQLite, a new migration (`quota_switcher_config`, one JSON document per
tool, plus `quota_switcher_state` and a bounded `quota_switcher_decisions` ring).
Not `usages/config.json`: that file is shared with v1 until cutover and v1 rewrites
only the keys it knows, so a v2-only key there could be erased by a v1 save. The
decision history belongs with the other server-owned data (and `prune` rules): plain
`aimebu prune` keeps the config and state, `prune -a` removes them.

## 4. The decision (what `Decide` does, in order)

Inputs per tool: the profiles' snapshots (status, session %, weekly %, how long it has been continuously ok), which profile is active, the config, the
persisted latch state, the refresher's per-profile account state, the token expiry of
the active and target profile, now.

1. **Disabled or fewer than 2 candidates** → `idle`.
2. **Protected active** → `hold: protected profile is active`.
3. **Unclear active snapshot** (the active profile's status is not `ok`: refreshing,
   stale, error, `backing_off`, `token_expired`) → `hold: active snapshot not ok`.
   The 2026-10-14 flap (`bkp-tomas -> bkp -> bkp-tomas` on a transient non-ok status)
   is the reason this exists; it stays unconditional.
4. **Warm-up** (decided: 60 s of continuous ok, counted in time, not snapshots): the
   runner records, per profile, the instant since which every observation was `ok`
   (`ok_since`; any non-ok observation clears it). A profile that is currently `ok`
   but has been ok for less than `warmup_seconds` → `hold: warming up`. As in the
   script this is applied across *all* ok profiles of the tool, not only the
   candidate, so a profile that just came back cannot be switched to or away from on
   a single reading. `ok_since` is in memory only: a restart waits one warm-up.
5. **Choose**: the first entry of the effective order (section 5) that is *usable*:
   status `ok`, ok for at least `warmup_seconds`, `session < session_max` (or
   null) and `weekly < weekly_max` (and not null), and not parked by the refresher
   (`reauth_required`, `needs_relogin_soon`) and has credentials. If none is usable,
   **stay put** (`want = active`; the script's 2026-10-16 rule: switching to another
   exhausted account only adds churn).
6. `want == active` → `stay`.
7. **Near-refresh** (link-model installs only, section 6): the active or target
   stored token expires within 15 minutes → `hold: near token refresh`.
8. **Dwell**: the last automatic switch of this tool was less than `min_dwell`
   ago → `hold: switched recently` (section 7).
9. Otherwise `switch(active -> want)` with the reason (`active over weekly limit`,
   `higher-priority profile usable again`, ...) for the audit trail.

Every outcome returns a structured `Decision{Tool, Action, From, To, Reason,
Details}`; the runner stores it, shows it, and logs it. Nothing is decided by log
text.

## 5. Back-after-reset detection (the order change)

The Claude rule is time-dependent: `main -> bkp -> bkp-tomas` until `main`'s
weekly window resets, then `bkp -> main -> bkp-tomas`. The script detects the reset
as "main's weekly dropped 20+ points below the highest value seen" and latches it
(`main_reset_done`, `main_weekly_max` in a state file so a restart keeps it).

v2 generalises it without hard-coding names:

- `after_reset = {profile, drop_points, profiles: [...]}` names the profile whose
  weekly window is watched, the drop that counts as a reset (default 20), and the
  order that applies **after** the latch.
- State per tool: `weekly_max_seen` and `reset_latched_at`, updated on every snapshot
  where the watched profile's weekly is known and the snapshot is `ok` (a non-ok
  snapshot never moves the maximum; a stale 0 must not look like a reset).
- Once latched the order stays switched; it is **persisted** (SQLite) so a restart
  keeps it. **Re-arming is manual only** (decided): the owner presses "re-arm" in
  the UI when the next week starts; it is a decision with a reason in the history.
  Nothing re-arms it automatically.
- The script also writes a one-off `bkp-reset.flag` when `bkp`'s weekly first reads
  below 100, used outside the script to tell another team that the profile is back.
  In v2 that becomes a **system event** (`quota_reset_seen` with the profile and
  tool, no numbers beyond the percent) in the owner's feed. No flag files.

## 6. Interaction with the token refresher and the switch locks

The quota switcher does not take any lock or touch any credential file itself. It
calls `Switcher.Switch(tool, name)` and inherits everything the web switch has:

- **Account lock first, switcher flock second** (`HoldMaintenance` on the incoming
  profile, then the flock), the same ordering as the refresher, un-migrate and the
  reconciler, so no new lock-order edge exists.
- **Token-copy model**: `Switch` refreshes the incoming profile through the
  refresher's `RefreshIncoming` if it is inside its lead (and refuses with
  `ErrIncomingRefresh` when that fails), then rewrites the live targets token-only.
  The script's `near_refresh` hold exists because on the link model a switch landing
  mid-refresh discarded the rotated token (profile `main` revoked on 2026-10-15
  19:58Z). On the token-copy model that hazard is gone by construction (the refresh
  runs under the account lock Switch holds), so step 7 of the decision is **only**
  active for a tool that is not migrated. An `ErrIncomingRefresh` or
  `ErrMaintenanceBusy` on a migrated tool is a *retryable hold*, not a failure.
- **Quiescence.** The web switch refuses while an affected agent is running a turn
  (`409 agents_running`, `switcherQuiescent`). The quota switcher applies the same
  rule: a running turn is `hold: agents running`, retried on the next evaluation.
  This is the one deliberate difference in *effect* from the script, which switched
  under running sessions (Claude sessions follow a token-copy rewrite on their next
  request; Codex sessions do not, see below). Decided: **wait for running turns**,
  never interrupt. The check is extracted from the HTTP handler into
  `Server.switchBlockers(ctx, tool)` so the handler and the runner share one
  implementation.
- **Codex** keeps its old account for the life of a running session (a rewritten
  file of another account is refused by its account-id guard), so a Codex switch is
  only meaningful when no Codex agent is running; the quiescence hold covers it and
  the UI says so.
- **Pending login / parked accounts.** `ErrLoginPending` (a login waiting for the
  reconciler) is a retryable hold. A profile whose refresh state is `reauth_required`
  or `needs_relogin_soon` is never a target (it would hand sessions a dying
  account); a switch *away* from such an active profile is allowed and preferred.
- **Usage invalidation.** After a successful switch the runner calls the same
  `refreshSwitchedUsage(tool)` the web path does (invalidate the live snapshot,
  refresh), then the normal "active snapshot not ok" hold absorbs the settling
  window.
- **Refresher grace.** The refresher's two-minute switch-away grace and 30-minute
  rotation floor are unaffected; a quota switch is an ordinary switch to them.

## 7. Churn protection (new, not in the script)

The script relies on the 3-snapshot warm-up and the 12 s poll to avoid flapping.
With event-driven evaluation and thresholds that can straddle a boundary, v2 adds
two cheap guards, both visible in the UI and both off the critical path:

- **Minimum dwell** (default 5 min) between two automatic switches of one tool.
- **Circuit breaker**: more than 4 automatic switches of a tool in one hour disables
  that tool's automation, raises a `quota_switcher_tripped` system event and requires
  the owner to re-enable it. Two profiles oscillating around a threshold must not burn
  a session on every flip.

Retries after an action error (`409`, busy, refresh failed) back off 30 s, 1 min,
2 min, 5 min (cap), reset on success; the reason is the visible hold.

## 8. Rollout and safety defaults

- Off by default; enabling starts in **dry run**: the full decision is computed,
  recorded and shown ("would switch claude main -> bkp: main weekly 96%"), and nothing
  is switched. A second explicit step turns dry run off.
- **Shadow comparison** before the script is retired: while `usage-policy.py` is still
  running, v2 in dry run logs its decision next to what the script did; they should
  agree except for the documented differences (quiescence, dwell). Retire the script
  only after a week of agreement.
- Disabled when the switcher is disabled, when the usage reader is off
  (`AIMEBU2_USAGES=0`), and while the server is in maintenance mode.
- Only the owner can read or change any of it; there is no per-project scope.

## 9. Settings UI

Settings → Usages gets an **Automatic account switching** section per tool, owner
only, built from the existing `SettingsRow`/`SettingsGroup` components (`web/js/components/settings-row.js`):

- Toggle (enabled), toggle (dry run), a status line: *last decision* (action, reason,
  time), *next evaluation*, and the current hold if any. Plain text only.
- The ordered list: rows with up/down buttons (keyboard-operable, same pattern as the
  provider ordering), per-row session and weekly limits (number inputs, 1..100), a
  remove button, an "add profile" select of the unlisted profiles, and a "protected"
  checkbox list.
- The after-reset block: watched profile, drop points, its order, the latch state
  ("main's weekly reset seen at ...") and a "re-arm" button.
- A "recent decisions" disclosure (last 50, each with its reason), the same data the
  API serves; and the circuit-breaker state with a "re-enable" button.
- Warm-up/dwell numbers under an "Advanced" disclosure.

API (owner session + CSRF, like the other settings). The usage snapshot and the
settings response do not change shape. `{tool}` is `claude` or `codex`.

```
GET  /api/quota-switcher
  -> { "profiles": { "claude": ["<names in the store>"], "codex": [...] },
       "tools": { "claude": <Tool>, "codex": <Tool> } }

PUT  /api/quota-switcher/{tool}      body: <Config>      -> <Tool>   (400 on validation)
POST /api/quota-switcher/{tool}/rearm      {}            -> <Tool>   (clears the reset latch)
POST /api/quota-switcher/{tool}/reenable   {}            -> <Tool>   (clears the circuit breaker)

<Tool>  = { "config": <Config>, "saved": bool,       // saved=false: config is the pre-filled default
            "state": <State>, "last_decision": <Decision|null>,
            "decisions": [<Decision>]  /* newest first, at most 50 */ }
<Config>= { "enabled": bool, "dry_run": bool,
            "profiles": [ {"name": s, "session_max": 1..100, "weekly_max": 1..100} ],
            "protected": [s],
            "after_reset": null | { "profile": s, "drop_points": 1..100,
                                    "profiles": [ {"name","session_max","weekly_max"} ] },
            "warmup_seconds": 0..600, "min_dwell_minutes": 0..120 }
<State> = { "reset_latched_at": ts|null, "weekly_max_seen": number|null,
            "last_switch_at": ts|null, "tripped": bool, "tripped_at": ts|null,
            "hold": s|null /* the current hold's reason */, "evaluated_at": ts|null }
<Decision> = { "at": ts, "tool": s, "action": "idle"|"stay"|"hold"|"switch"|"error",
               "from": s|null, "to": s|null, "reason": s, "dry_run": bool,
               "profiles": { "<name>": { "status": s, "session": number|null,
                                          "weekly": number|null, "ok_for_seconds": number } } }
```

All `reason` strings are fixed sentences chosen by the server (never CLI output or
error text with paths); errors from the switch are mapped to a short fixed reason
(`agents running`, `incoming refresh failed`, `account busy`, `login pending`,
`switch failed`) with the detail in the server log only.

## 10. Tests

All fake data, no host credentials, no network, a fake clock.

1. **Policy table** (`internal/quota/policy_test.go`): one row per rule above, with the
   script's numbers as fixtures: Claude order before and after the latch; every
   threshold boundary (94/95/96, `null` session, `null` weekly); Codex `main` 92/93/94
   session and 97/98 weekly, fallback to `bkp`, and "else main"; protected active;
   unclear active; warm-up held by a *different* profile; nothing usable stays put;
   parked target skipped; near-refresh only on an unmigrated tool.
2. **Latch**: weekly rising then dropping 19/20 points (no reset / reset); a non-ok
   snapshot never moves the maximum; persistence across a runner restart; manual
   re-arm.
3. **Runner** with a fake switcher and snapshot source: acts exactly once per
   decision; `agents_running`, `ErrIncomingRefresh`, `ErrMaintenanceBusy`,
   `ErrLoginPending` become holds with backoff and never an error storm; dwell; the
   circuit breaker; dry run never calls `Switch`; disabled/maintenance never evaluates.
4. **Replay**: sanitised sequences from the live watchdog log (profile names and
   percentages only; the log already has `claude active=.. want=.. bkp=0/88 ..` lines)
   fed through `Decide`; the decisions must equal the script's `SWITCH`/`HOLD` lines
   except where a documented difference applies. This is the acceptance test for
   "same rules".
5. **Integration** over the real switcher, usage reader and token refresher with fake
   credentials: a due incoming profile is refreshed once by the switch it triggers
   (spend-once), a running agent blocks the switch, and the live file ends token-only
   with the target account's access token.
6. **API/auth**: non-owner 403, CSRF required, validation errors, config round trip.
7. **UI smoke** step: the section renders from the mock source, reorder and threshold
   edits post the expected body, and decision text is escaped.
8. **Migration/prune** test for the new tables (kept by `prune`, removed by `prune -a`).

## 11. Phasing

1. `internal/quota` pure policy + config + table/replay tests (no wiring).
2. SQLite migration and store; runner with fake adapters; dry run only.
3. Server wiring, API, `switchBlockers` extraction, system events.
4. Settings UI section and docs.
5. Shadow week, then enable per tool; retire the script.

Each phase is a reviewable commit with its tests; phase 1 alone already pins the
rules the script implements today.

## 12. Decisions (owner)

1. **Running turns:** wait for them, never interrupt.
2. **Warm-up:** 60 s of continuous ok, counted in time.
3. **After-reset re-arm:** manual only.
4. **Defaults:** pre-filled from the owner's existing profile names with today's
   thresholds; no names in code, fixtures or tests.
5. **Dwell and breaker:** 5 minutes minimum between automatic switches of a tool;
   more than 4 in an hour trips the breaker.

This feature is post-RC: it is not part of the RC push.

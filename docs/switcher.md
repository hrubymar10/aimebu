# Account switcher

The v2 account switcher stores named Claude Code and Codex credential profiles
under `<AIMEBU_CONFIG_DIR>/switcher/` (default `~/.aimebu/switcher/`). It
starts disabled in a fresh root and keeps the existing enabled flag at
cutover. It uses credential files only; it never accesses the macOS
Keychain. A development instance can use a separate root with
`AIMEBU_CONFIG_DIR`. Both versions swap the same live credential files, so
when v1's `~/.aimebu/switcher/.lock` exists and is a different file from v2's
own lock, v2 takes the v1 lock first and then its own `switcher/.lock`. With a
separate root it never creates the v1 lock or changes v1 profiles or state.
With the default root, v2 uses the existing v1 switcher tree in place after
the server has finished cutover. The v2 switcher CLI refuses to operate on an
unmigrated v1 root or an interrupted move. Both
versions share the version-1 `state.json`, profile layout, active symlinks, backups, and
single `switcher/.lock` flock (the same file, so only one lock is taken). An
unsupported state version fails loudly;
there is no automatic profile conversion or merge between roots.

For a fresh root, enable the switcher in Settings → Server → Accounts & usage, then **Import
current login** before switching. Import adopts the live login as a profile.
Existing v1 profiles at cutover need no re-import. **Add profile** creates an
empty destination. Switching to an empty profile clears the live credential
file so the harness asks you to log in. You can also use the CLI with the
server stopped:

```bash
aimebu switcher enable
aimebu switcher import codex main
aimebu switcher add codex other
aimebu switcher use codex other
aimebu switcher use codex other --force
aimebu switcher rename codex other spare
aimebu switcher list
aimebu switcher status
aimebu switcher migrate-copies [--tool claude|codex] [--allow-running-codex]
```

`list` and `status` print aligned tables (`TOOL PROFILE ACTIVE CREDS EMAIL`
and `TOOL ACTIVE ELIGIBLE REASON`, `status` preceded by a `Switcher: enabled`
or `disabled` line); add `--json` for machine-readable output
(`{"profiles": [...]}` and `{"enabled": true, "eligibility": [...], "active": {...}}`).
`enable` and `disable` take no arguments; extra ones exit 2. `migrate-copies`
is the one-time move to the token-only credential model described below.
`aimebu switcher help` prints the command list.

Switching to an empty profile and removing the active profile both delete the
live login, so the CLI asks `Proceed? [y/N]` first. Pass `-y` / `--yes` to
skip the prompt; without a terminal on stdin the command refuses and asks for
`-y`. `aimebu switcher remove <tool> <profile>` deletes an inactive profile
without a prompt. In the UI, removing an active profile asks for confirmation.
The open Settings → Server → Accounts & usage view refetches after a web removal. The server also
detects profile names changed by the CLI on its five-second usage scan and
notifies open views; a removed profile disappears without reloading the page.
Only the owner can change profiles through the web API; switcher status and
account profiles are owner-only too.

`switcher use <tool> <profile> --force` requires stored credentials and installs
them without refreshing the incoming profile. It still uses the normal safe
swap or token-only fan-out, never clears a credential because it is expired,
and may leave the selected account needing a fresh login. The CLI asks for
confirmation; `-y` skips the prompt. The owner can choose **Install anyway**
on a profile with a `reauth_required` alert in Settings → Server → Accounts & usage. The switch
API accepts `{"name":"<profile>","force":true}`. Ordinary switches still
refresh due incoming profiles before installing them.

Errors carry hints: a disabled switcher points at `aimebu switcher enable`,
and switching with no active profile points at `aimebu switcher import` first.
Usage mistakes exit 2; failed operations exit 1.

Renaming a profile (`POST /api/switcher/<tool>/profiles/<name>/rename` with
`{"name": "<new>"}`, or **Rename** on the profile card in Settings → Server → Accounts & usage) is
allowed while the profile is active and while agents run: only the stored
profile moves, never the live credential file. The profile's backups and
rotated-credential rescue files move with it, and a destination name that
already exists is refused with 409. The profile directory also carries a
private opaque `usage-alert-id` file when needed for usage alerts; it moves on
rename and is deleted with the profile, so a reused name gets a new identity.

Removing a profile first records a tombstone in `switcher/removed.json`: the
tool, profile name, removal time and the SHA-256 hash of its refresh token,
never the token. The last 50 are kept, the file is mode 0600 and
byte-compatible with v1, which shares this tree: a v1 maintenance run that
rotated the removed account's token checks `removed.json` under the switcher
lock and discards the rotation instead of resurrecting the account. v2 rotates
tokens only through the token-only refresher described below. A profile whose credentials are missing, unreadable or
have no refresh token is still removed; no tombstone can be written for it and
the reason is logged. A corrupt `removed.json` is logged and rewritten by the
next removal. Removal also deletes the profile's backups and rescue files.

`switcher/switched_away.json` records, per profile, when it last stopped being
the active one (a switch, or an import that moves the active pointer; rename
carries the entry, removal drops it). It is the v1 file, byte for byte: v1
background token maintenance leaves a profile alone for eight hours after that
time, because sessions started while it was active may still be refreshing the
token they hold. v2 refreshes tokens only on a token-only (migrated) install,
where it applies its own two-minute grace instead; it keeps the file current so a
v1 server sharing the tree sees switches made here. It is separate from
`state.json` and advisory: unreadable or missing means no grace. A file that
exists but cannot be parsed also holds other profiles' records, so a writer
moves it to `switched_away.json.corrupt` (a numbered suffix if one is already
there) and logs an error instead of replacing it with a one-entry file; listing
never touches it. Renaming the active profile drops stale entries under either
name. The profile
list reports it as `switched_away_at`.

The safe swap validates the live and stored credentials, captures the outgoing
login, backs it up, restores the incoming login, and links the active profile
to the live file. The link follows token rotation while the profile is active;
inactive profiles hold regular files. Backups remain under
`switcher/backups/<tool>/`. Rescue files
(`<name>-rotated-<UTC>.json`, written by v1 when a rotated token could not be
copied back safely) live there too; Add, Import and rename-to leave them
intact, Rename moves them to the new name, and only Remove deletes them. Credential files use mode 0600 and profile
directories 0700.

A missing live file or a parseable logged-out placeholder such as `{}` can be
switched away from without capturing it. Malformed JSON, partial tokens, and
other nonempty credential material block the switch. Expired but structurally
complete credentials remain valid to capture because the refresh token may
still work. A switch with no active profile is refused until you import the
current login.

**MCP logins follow the user, not the profile.** Claude keeps the OAuth logins of
the user's MCP servers in the `mcpOAuth` object of `.credentials.json`, next to
the account login. A switch carries the *live* `mcpOAuth` to the profile being
activated: on the link model the incoming credential is merged with it before the
live file is replaced (the union of both maps; the account login of the incoming
profile is untouched, so no refresh token of the outgoing account crosses over).
Where the same server key holds different content on both sides the **active (live)
entry wins** and the profile's own copy is written to its 0600 backups first; the
log names only the profile and the server keys, never a value. Freshness is not
guessed from expiry. A live `mcpOAuth` that cannot be read as an object is not
carried (logged; the live file is backed up as always), and a profile whose own
section is unreadable gets the live one in its place, backed up first. On a
migrated install the fan-out already rewrites only the token fields of the live
file, so the live `mcpOAuth` stays where it is. Switching to an **empty** profile
removes the account login but keeps `mcpOAuth`: the live file is reduced to that
section (deleted only when there is none), which reads as logged out, not corrupt,
and rides along to the next account. On the link model Claude's
`<configdir>/.storage-write.lock` is held from the first read of the live file to
the end of the swap, so an MCP refresh cannot land between the read and the
write; a switch that cannot take it within 3 seconds is refused with nothing
changed. A Claude process that rewrites a stale copy after the lock is released
is the same limitation as for the account login: switch with the sessions
stopped. A profile's own stored MCP logins come back with it, and a switch
carries them forward like any other: logging out of an MCP server while
another account is active does not remove it from a profile that still holds
it (there are no tombstones).

The UI lists host agents on the selected tool before a switch, for information.
**A switch never waits for agents**: a running turn keeps the token it holds and
finishes on it (a turn that then fails on the limit goes through the ordinary
failed/limit path, and the queue continues on the new login: each host turn
records the profile it started on, and a limit it meets counts only against that
profile, so it neither pauses the agent nor marks the new login limited). The swap keeps the
account lock, the incoming refresh and, for Claude, Claude's own
`.oauth_refresh.lock` and `.storage-write.lock` held across the file swap
(Claude re-reads the file before every refresh and saves compare-and-swap, so a
running session never writes the old account into the new one). After the swap
the warm processes and cache pings of the tool are retired in the background,
best effort; the next turn starts a fresh process on the new login. A running
codex may write the old account's tokens back into `auth.json` (an in-flight
write takes no lock), so after a Codex switch the live account id is checked at
about +5 s, +30 s and +2 min, and after that on every usage refresh and before
every Codex turn on the host, until the next Codex switch. The account expected
is the one the switch installed (taken from the stored profile it wrote, never
from re-reading the live file, which may already hold the old account). Each
check compares the account id only; when the file names another stored
profile's account instead, the active profile is installed again (with the
newest tokens a check saw for it) and one `switch_overwritten` owner event is
raised per switch. A login of an account no profile holds, or a logged-out
file, is left alone. The stored profile copy is never overwritten by the old one. A standalone CLI switch should be made while the
server and any harness sessions using that login are stopped. Remote-machine
switching is not available in this slice.

Usage polling also covers inactive profiles, read-only: each refresh reads a
stored profile's credential copy (under the switcher lock, so a switch, rename
or removal cannot replace it mid-read) and asks the usage endpoint with its
access token. Nothing is refreshed or written. A stored login whose token has
expired shows `token_expired` with its last snapshot kept stale, until you
switch to it and run the CLI to renew it. A stored copy identical to the live
login is not polled again (`not_selected`, "Same login as the active
profile"). A stored profile whose request fails transiently (rate limit, 5xx, timeout)
backs off on its own: its snapshot keeps the last numbers, marked stale with
status `backing_off` and the next try time, the delay doubles per consecutive
failure from 2 minutes up to 30, and any answer the endpoint gives on purpose
resets it. The live account is never throttled this way. A result is discarded
when the stored copy changed, the profile
became active, or the owner switched while the request was in flight. After a
web switch, the previous active snapshot is cleared and the new login is
refreshed; the live login is also re-polled as soon as its file changes (a
re-login or the harness rotating its token) instead of waiting for the interval.

Claude's live file is `$CLAUDE_CONFIG_DIR/.credentials.json` when that variable
is set, otherwise `~/.claude/.credentials.json`. Its account-state file is
read only for the email label: `$CLAUDE_CONFIG_DIR/.claude.json` or
`~/.claude.json` respectively. Codex uses `$CODEX_HOME/auth.json` when set,
otherwise `~/.codex/auth.json`.

## Token-only credential model (`migrate-copies`)

The normal way to turn this on and off is the **Auto-refresh tokens** setting
(see [usages.md](usages.md#auto-refresh-tokens-account-maintenance)); the
`migrate-copies` command below is the advanced, manual path for an install whose
server is not running or that wants one tool at a time.

Links share one refresh token between every running session and the server,
and a refresh-token reuse revokes the whole family. The token-only model ends
that: **the profile store holds the only full credentials** (regular 0600
files, the active profile included) and the server is the only party that
spends refresh tokens. The live `~/.claude` and `~/.codex` files become
*targets*: copies with the access token (Codex: plus the account id) and **no
refresh token** (Claude: the `refreshToken` key is deleted; Codex:
`refresh_token` is `""` with `auth_mode: chatgptAuthTokens`). Sessions use the
access token until it expires and re-read the file when it is rewritten.

- `aimebu switcher migrate-copies` converts an install, per tool, all or
  nothing. It **refuses with exit code 2** when a live access token has less
  than 15 minutes left, because a session may be refreshing it right then;
  rerun once the token is fresh. Logins still sitting in a registered
  external target are handled in a fixed order, all under the switcher lock.
  First, with nothing touched, the logins are grouped by refresh token (targets
  holding one login are one family and are filed once) and each group's
  destination is resolved: the profile its pinned member is pinned to, else
  the profile that already holds the token (every profile holding it counts;
  the pin is accepted if it is one of them), else a new `login-<time>`
  profile. One invariant covers the refusals: a destination profile is touched
  by at most one group, whether it is replaced by a pinned filing or is where
  another group's login is already stored. The whole run is refused (a login
  is waiting; nothing changed) for an unusable credential, an uncreatable
  rescue directory, members pinned to two different profiles, a token held
  only by profiles other than the pin, a pin on a profile that is still a link,
  or two groups resolving to one profile. Then the live file is converted. Then
  each group is filed after a rescue copy, resolved again against the store as
  it is by then (a profile planned for reuse must still hold the token). If a
  filing replaced the active profile's credential the live file is rewritten
  from it. Then the marker is written. Last, each target of a filed group is
  rewritten token-only; that write refuses a credential of another refresh-token
  family and checks, under the writer's own lock, that the target still holds
  exactly the filed login (a removed target stays removed, its directory is not
  recreated, and a newer pair is left for the reconciler).
  It backs up the live file, writes the full
  credential into the profile as a regular file, strips the live file and
  writes `token-copies.json` last (version 2: a `migrated` map of tool to
  migration time; a version-1 file means both tools). A second run is a no-op
  for tools already migrated. Start or restart
  the server afterwards. It also **refuses while a `codex` process is
  running** (it lists them; `--allow-running-codex` overrides). The check covers
  host processes and, best effort, the processes inside running docker
  containers (`docker ps` then `docker top`; the fleet's codex workers run in
  `harness-docker` containers that mount `~/.codex`). When docker is missing
  or unreachable it says so and the containers must be stopped by hand;
  the success output always states what was checked. Codex
  caches `auth.json`, refreshes shortly before expiry with the refresh token it
  cached and writes a full credential back, which would spend the token the
  server is refreshing. Warm Codex requires token-only credentials (or an API
  key without refresh credentials); a full login stays on cold exec. An idle
  Codex app-server is a running Codex process for
  this check. The server's automatic migration first asks its supervisor to
  retire those warm processes; the manual command still refuses them. The refresher leaves codex profiles alone for 10
  minutes after the migration. Restarting running claude sessions is optional
  (claude re-reads the file before it refreshes and finds no refresh token).
- A **switch** no longer captures, restores or links anything. It refreshes
  the incoming profile if it is due (a failed refresh refuses the switch with
  409), rewrites the live targets from the profile, and moves the active
  pointer. While a login is waiting in the live file it refuses (retry in a
  minute; see Login import below). A switch holds maintenance locks for both
  the outgoing and incoming accounts until it finishes. It takes distinct
  account locks in a fixed order before the switcher lock, so a background
  refresh or warm-up on either account must finish before the switch proceeds.
  If another switch changes the active profile during that wait, a manual
  switch retries with the new active profile; an automatic switch refuses its
  stale decision and evaluates again.
  An explicit forced switch skips only the incoming refresh; it still requires
  stored credentials and runs the same locks, validation, fan-out and state
  update. An account in `reauth_required` does not receive retry advice because
  that state requires a new login.
- The **server** keeps targets current. Every live-target write takes the
  switcher lock and re-reads the active profile under it, so a switch landing
  meanwhile is never overwritten. `server/credential-targets.json` lists extra
  targets (a profile-pinned or follow-the-active file elsewhere); the two live
  files are implicit. Every manager built by `switcher.New` (the server and
  the CLI alike) carries the same fan-out, so `aimebu switcher use` works on a
  migrated install. At a same-root v1 cutover, startup copies the backed-up
  external-target registry into the new `server/` directory before loading it.
- v2 reports credential alerts as `credential_alert` system events (owner
  only; they appear in the system-events feed), not as room messages. The
  alert text can carry CLI output or file errors, so the server redacts it
  (`usages.RedactDetail`: JWTs, `sk-` keys, bearer values, token and secret
  fields, long opaque runs, the home directory shown as `~`) before it is
  logged or stored.
- Before the migration, a token-only live file is refused (`switch`,
  `import`, `rename`, `remove`: "token-only copy", HTTP 409): another install
  owns it, and capturing it would overwrite the only full credential with a
  copy that cannot refresh. `import` on a migrated install stores the live
  login as the profile's own regular file and strips the live file.
- On a migrated install the v2 server runs the refresher (next section), so it
  can be the only refresher of the tree. A v1 server must not share a migrated
  tree with a running v2 server: two refreshers would spend the same token. A
  process with no refresher (the `aimebu switcher use` CLI) still refuses a
  switch to a profile inside its refresh lead.

## Returning a tool to the link model (un-migrate)

`Manager.UnmigrateTokenCopies(tool)` undoes `migrate-copies` for one tool; the
server calls it when the token auto-refresh setting is turned off (see Usages).
Under the active profile's account lock it: refuses while a login is waiting in
the live file that is not the stored credential (`ErrLoginPending`, retry once the
reconciler imported it); refreshes the active profile first if it is due; merges
the stored full credential with the live file (the refresh token restored **and
every unrelated live key kept**: Claude's `mcpOAuth` logins accumulated while
migrated, Codex's other fields) and writes that merged document into the store
*first*, then into the live file, so a crash or a failed live write leaves the
live file token-only with the marker still set and the retry starts clean (and a
login in the live file that carries a stored profile's own refresh token is that
profile to the reconciler, never a new one); persists the live document into
the store once more in case a worker added a key in between; removes the tool from
`token-copies.json` (the file itself when no tool is left); and re-links the
active profile's stored file to the live one. A tool that is not migrated only
gets the active link repaired.

## Token refresher

On a migrated tool the server spends refresh tokens: one `TargetRefresher`
(`internal/usages/target_refresher.go`), started once by `Server.StartTokenCopies`
and idle until `token-copies.json` marks a tool as migrated. Every 30 seconds it
refreshes each stored profile of a migrated tool that is expired or within its
lead of expiring (**60 minutes for Claude, 24 hours for Codex**, overridable with
`AIMEBU_TOKEN_LEAD_CLAUDE` / `AIMEBU_TOKEN_LEAD_CODEX`, 10 minutes to 14 days; a
bad value is ignored with a log line). The active profile is a stored file like
any other, so it is refreshed too. At startup, before the first pass, it settles
the journal a previous process left and runs one reconciler pass, so a login that
landed while the server was down is imported before anything is fanned out.
At startup, it checks the other default switcher root for a token-copy marker.
If both roots claim a tool and resolve the same live file, this server does not
refresh or reconcile that tool; it records a `credential_alert`, and doctor
fails until only one root owns it. The other migrated tool can still refresh.
This check compares the configured root with `~/.aimebu/switcher` and
`~/.aimebu2/switcher` once at startup; it does not discover another custom root
or a marker created after startup.

A refresh is spend-once. Under the account's flock it (1) journals `spending`
with the hash of the refresh token about to be used, (2) runs the real CLI in an
ephemeral `harness-docker` container against a throwaway copy of the credential
under `switcher/refresh-tmp/` (`harness-docker-ctrl` and a reachable docker are
required; without them the refresh is skipped and an alert is raised),
(3) journals `started` once the process exists, (4) classifies the result from the
files the CLI left and its output, (5) on success writes the rotated credential to
`backups/<tool>/<profile>-rotated-<UTC>.json` first, commits it into every profile
that still holds the spent token (compare-and-swap under the switcher lock; a
removed, renamed or re-logged-in profile leaves the rescue file instead), deletes
the rescue file and fans the full credential out to the targets as token-only
copies.

Outcomes: a request that never left the machine backs the account off 5 minutes,
doubling to 30, and is retried. A CLI that ran and failed with no proof the request
was not sent is **never retried**: the account is parked as `needs_relogin_soon`,
the tokens run to expiry and a human logs in. A provider rejection
(`invalid_grant`) parks it as `reauth_required`. Nothing is ever wiped. A crash
between the spend and the commit is finished on the next start from the journal
and the rescue file, never by spending again. `refresh-state.json` holds the
journal and the parked accounts, `refresh-last.json` the last rotation per
profile: a profile is not refreshed again within 30 minutes of its last rotation,
and the lead is capped below the token's life, so an oversized lead cannot loop.
`warmup-last.json` records recent idle-account warmup attempts (no credentials)
so a restart keeps their one-hour rate limit; the same journal protects a
warmup if its short CLI request rotates a grant.
A profile switched away from within two minutes, a Codex profile within 10 minutes
of its migration, and a profile whose store file is still a link are left alone.

`GET /api/credentials/targets` (owner only) returns `{enabled, targets, alerts}`:
one row per stored profile and live file of a migrated tool with `expires_at`,
`seconds_left`, `level` (`ok`, `expiring` under 15 minutes, `expired`,
`unknown`) and, for a parked account, `state` (`reauth_required`,
`needs_relogin_soon`, `refresh_failed`) and `next_attempt`; `alerts` holds one
sentence per credential that needs attention. It never carries a token or the
CLI's error text (that stays in the state file, the log and the owner's doctor
run). Edge alerts for these states, and for a credential about to expire, are
`credential_alert` system events like the reconciler's.

## Login import (the credential reconciler)

In the token-only model the live credential files hold no refresh token, so a
`/login` in any session writes a *full* credential into a file that must never
hold one. Every 60 seconds, and once at server start, the reconciler
(`internal/credtarget`) looks for exactly that and files the login into the
profile store. A login in a target that was written less than 15 seconds ago
waits for a later pass: Claude writes the credentials first and its account
file once it has fetched the profile, and the account file is only trusted when
it is no older than the credentials, so a login is never filed under the
account that was logged in before it.

1. The full credential is first saved as `backups/<tool>/_login-<UTC>.json`
   (at most five are kept; an identical one is reused), so nothing is lost if a
   later step fails.
2. The account is identified: Claude by `oauthAccount.emailAddress` in the
   account-state file, Codex by the `id_token` email **and** the account id
   together (two workspaces under one email are different accounts), else
   whichever of the two exists. The
   Claude file is the most recently written of `<credentials dir>/.claude.json`
   and claude-code's default for the server's environment (`~/.claude.json`, or
   `$CLAUDE_CONFIG_DIR/.claude.json`): which one is real depends on how the
   sessions were started, not the server, and `aimebu doctor` should flag two
   files that name different accounts. An account that cannot be identified
   never matches anything.
3. The login goes to the first rule that applies:
   - a profile already holds this very refresh token (the same grant; with two such
     profiles the active one wins, else the first by name: a failed
     turn-off leaves the live file with the stored credential's token plus keys the
     store lacks, such as MCP logins): that profile again, with no alert;
   - the login landed in a target **pinned** to a profile that exists: into that
     profile, and the host's active profile does not change, whichever rule files
     it (this one, a same-token match, an already-imported login or a rotation); the
     heal pass would otherwise rewrite the pinned target back from the profile the
     login just moved away from. A pinned target whose profile is gone follows the
     rules below. A login of a different account replaces the pinned profile's
     credential: the target *is* that profile; the replaced credential is kept
     as a backup;
   - the active profile is empty (`switcher add main`, `switcher use main`,
     then log in): into the active profile;
   - the same account as the active profile: replaces it;
   - another known account (exactly one profile): replaces that profile and
     raises a `login_imported` alert;
   - otherwise a new profile `login-<UTC>` and the same alert. Another
     account's profile is never overwritten without a match, and a profile that
     cannot be read is never taken for an empty one.
4. The profile that received the login becomes the active profile, because the
   live file now holds its account. A replaced credential is backed up first,
   the write happens under that profile's account lock and the switcher lock,
   and the file is always a regular file, never written through a link.
5. The live file is rewritten token-only and fanned out to the other targets.
   The strip uses the writer's per-call login-replacing method; the ordinary
   write refuses a target that still holds a refresh token, so no fan-out can
   destroy a login nobody has rescued. The strip re-checks the target under the
   writer's lock (Claude's storage-write lock), so a pair its owner wrote after
   the reconciler's own re-read is never overwritten.

Every step can be repeated, so a failure part-way is finished by the next pass:
a login that was stored but not yet stripped is recognised by its bytes, is
stored again (which re-asserts the active pointer if only that step failed) and
is not imported twice. The target is read again before the strip: a login whose
token pair changed meanwhile is imported again into the same profile, a target
that was removed meanwhile (a logout) stays removed, and a worker adding an MCP
login to the same file is not mistaken for a rotation.

A token-only target whose access token is not its profile's (a missed fan-out,
or another account's after a switch) is rewritten from the profile. Identity
decides, not expiry order, and the rewrite re-reads the active profile, the
stored credential and the target under the switcher lock, the lock a switch
holds, so it can never put the previous account's token back over a switch. A
credential file with open permissions is set back to `0600`. A missing file is
left missing: that is a logout.

The reconciler does nothing until the credential migration has run, and it
refuses to import while any profile is still a link to a live file, because
stripping that file would remove the profile's only refresh token. It says so
once per tool, not every pass, and saves no rescue copy in that state.

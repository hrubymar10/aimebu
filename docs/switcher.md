# aimebu switcher

Switch which account `claude` and `codex` use, from the CLI or the web UI.

The switcher stores a credential set per named profile and swaps the active one
into the tool's own credential location. It is **off by default** and does
nothing until you enable it.

After creating an active profile, use **Settings → Usages → Account maintenance
→ Auto-refresh tokens** to enable the token-only model. The setting migrates
each eligible tool as soon as the setting is saved and shows `waiting` with a reason
when a safety guard refuses it. Turning the setting off restores full live
credentials and the active-profile symlink. The link-based behavior described
below applies while the setting is off or a tool is waiting to migrate.

## Why it exists

Swapping credential files is easy to get wrong in a way that destroys logins.
The failure mode is subtle: OAuth refresh tokens rotate, so a stored copy of a
profile's credentials goes stale — and often becomes *invalid* — the moment the
live credentials are refreshed. A switcher that restores a stored snapshot
without first re-capturing the live one will hand you a dead login every time
you switch back.

aimebu's switcher captures the live credentials into the outgoing profile
**before** restoring the incoming one, so a profile's stored copy is always the
most recently live one.

While a profile is active its stored credential file is not a copy at all: it is
a **symlink to the tool's live credential file**. The harness rotates tokens
only in the live file, so a copy would go stale the moment it refreshed; the
link means anything reading `profiles/<tool>/<active>/<file>` sees the current
tokens. Inactive profiles always hold regular files.

Installs from before this hold a plain copy for the active profile. While the
token setting is off, the server converts it on a poller tick without a switch:
when the switcher is enabled and the live file validates, the copy is saved
under `backups/` first, then replaced by the link. Running it again is a
no-op — with everything linked it takes no lock and writes nothing — and
inactive profiles are never touched. The CLI does not do this; with the server
stopped, the plain copy stays until the server next runs or you switch.

## Requirements

- **File credentials only.** If a tool keeps its credentials in the macOS
  Keychain rather than a file, switching is disabled for that tool and the UI
  says so. aimebu never reads or writes the Keychain.
- **claude-code and codex only** — the two tools aimebu reports usage for.

### Where claude's files are read from

claude-code has two layouts and aimebu follows both. `CLAUDE_CONFIG_DIR`
decides which:

| | credentials | account state (`oauthAccount`) |
|---|---|---|
| `CLAUDE_CONFIG_DIR` **unset** | `~/.claude/.credentials.json` | `~/.claude.json` |
| `CLAUDE_CONFIG_DIR` **set** | `$CLAUDE_CONFIG_DIR/.credentials.json` | `$CLAUDE_CONFIG_DIR/.claude.json` |

The account file is the awkward one: with the variable set it sits beside the
credentials, but with it unset it lives at the **home root**, not inside
`~/.claude/`. That asymmetry is claude-code's own.

aimebu only ever **reads** the account file, and only one key
(`oauthAccount.emailAddress`) to label a profile. It never writes it.

Check what aimebu can see:

```bash
aimebu switcher status
```

```
TOOL    ACTIVE  ELIGIBLE  REASON
claude  main    yes
codex   -       no        not logged in, or using the macOS Keychain
```

## First run

The order matters, and the first step is not optional:

```bash
aimebu switcher enable

aimebu switcher import claude bkp    # adopt the login you already have
aimebu switcher add    claude main   # create an empty profile
aimebu switcher use    claude main   # clears the live credentials
claude login                          # log in as the second account
```

**`import` must come first.** Until a profile is active there is nowhere to
capture the live credentials to, so switching would silently discard the login
you currently have. The switcher refuses that rather than doing it:

```
$ aimebu switcher use claude main
no active profile: run 'aimebu switcher import <tool> <name>' first — without a
profile to capture into, switching would discard the current login
```

After `claude login`, the new credentials are live and belong to `main`.
`main` was empty, so at first it has no stored file and no link. Once the login
writes the live file, the running server links `main`'s stored path to it
on a poller tick — with no backup, since there was nothing stored to
keep — and from then on the stored path follows live through the link. A
regular snapshot is written into `main`'s stored copy on the next switch
**away** from it.

## Commands

| command | what it does |
|---|---|
| `aimebu switcher list [--json]` | profiles across both tools, with active marker, whether credentials are stored, and login email |
| `aimebu switcher status [--json]` | active profile and eligibility per tool |
| `aimebu switcher use <tool> <profile>` | switch |
| `aimebu switcher add <tool> <profile>` | create an **empty** profile |
| `aimebu switcher import <tool> <profile>` | adopt the **current live login** as a profile |
| `aimebu switcher rename <tool> <old> <new>` | rename |
| `aimebu switcher remove <tool> <profile>` | delete a profile |
| `aimebu switcher enable` / `disable` | turn the feature on or off |
| `aimebu switcher migrate-copies [--tool claude\|codex] [--allow-running-codex]` | advanced manual move to the token-only credential model (normally use the Settings toggle) |

`add` and `import` are deliberately different verbs: `add` creates an empty
profile you will log into later, `import` captures the login you already have.

`-y` / `--yes` skips the confirmation on destructive operations. When stdin is
not a terminal, a required confirmation without `-y` is an error rather than a
silent yes.

**The CLI works with the aimebu server stopped.** It talks to the filesystem
directly — no HTTP, no daemon.

## What a switch actually does

1. Refuse if the switcher is disabled, the tool is ineligible, the target
   profile does not exist, or **no profile is currently active**.
   An absent live credentials file is the exception: it means an empty active
   profile has nothing to capture, so switching away is allowed. Malformed
   live credentials still block the switch.
2. **Validate** the live credentials. If they are malformed, abort before
   writing anything — storing them would overwrite a known-good profile with
   garbage.
3. **Capture** the live credentials into the outgoing profile. This replaces
   the outgoing profile's symlink with a regular-file snapshot; the live file
   is not written. If the live file is gone (for example after a harness
   logout), there is nothing to capture and the outgoing profile's now-dangling
   link is removed, leaving that profile with no stored credentials. Its
   last-known credentials stay under `backups/`: one is written whenever a
   profile is imported, linked in by a switch, or switched away from.
4. **Back up** the live credentials under `backups/`.
5. **Restore** the target profile's credentials, or, for an empty profile,
   delete the live credentials so the tool prompts for a login. A target whose
   stored file is a dangling symlink counts as empty.
6. Update the active pointer.
7. **Link** the now-active profile's stored file to the live file, after
   backing up the stored bytes it replaces (skipped for an empty target, which
   has nothing to link). If the backup or link fails, the switch still stands
   and the profile keeps a plain copy; the failure is logged.

Failures during 5–6 roll back from the step-4 backup, and the outgoing
profile — still active — is linked again. Every write goes to a temp file in
the same directory followed by a rename, so an interrupted switch can never
leave a half-written credential file. `import` links the adopted profile the
same way, and backs up the adopted login.

`import` while another profile is active means the live login was replaced
outside the switcher (for example `claude login` as a different account), so
the old profile's link now points at the new login. Before the import makes the
new profile active, the old profile is detached: its stored file is restored
from its newest valid backup — its last-known credentials, the same bytes it
would have kept before active profiles were linked — or, with no backup, the
link is dropped and the profile becomes empty. It never ends up holding the new
login.

Expired credentials are **valid** and are captured normally — that is what the
refresh token is for. Only malformed ones are rejected.

## Switch when idle

A tool session running across a switch can refresh its token afterwards and
write it over the newly active credentials, putting you back on the previous
account. aimebu holds a lock around its own writes and verifies the live file
hasn't changed underneath a refresh, so aimebu will not do this to you — but it
cannot control a `claude` or `codex` process you started yourself.

Switch when you don't have a session running against the outgoing account.

## Storage

```
~/.aimebu/switcher/
  state.json                              # enabled flag + active profile per tool
  profiles/claude/<name>/.credentials.json  # symlink to live while active
  profiles/codex/<name>/auth.json           # symlink to live while active
  backups/<tool>/<name>-<unix>.json        # newest 10 per profile
  backups/<tool>/<name>-rotated-<UTC>.json # newest 3 rescues per profile
  removed.json                           # last 50 explicit removals (token hashes only)
  token-copies.json                      # marker: which tools run the token-only model (and since when)
  .lock
~/.aimebu/server/credential-targets.json # extra (non-live) token-only targets
```

Each profile keeps its newest 10 backups; older ones are pruned whenever a new
backup is written. The newest backup that still validates is always kept, even
when it is older than those 10, because restoring a detached profile relies on
it. A backup identical to the profile's newest one is not written again, and
renaming a profile moves its backups to the new name. Removing a profile deletes
its backups too, and adding, importing or renaming a profile first clears any
backups an earlier profile left under that name, so a new profile never
inherits another account's credentials.
Rotated rescue files have a separate cap of three newest files per profile;
older rescues are pruned when a new one is saved. Add, Import, rollback, and
rename-to leave existing rescues intact. Rename moves rescues from the old
name, stepping a timestamp forward when the destination name already exists.
Only Remove deletes rescues with the profile's backups.

Credential files are `0600`, directories `0700` (an active profile's symlink
has no mode of its own; its target, the live file, is `0600`). This tree is **not**
server-owned — the CLI writes it with the server stopped. `.lock` also
serializes the server-side writers that touch a live or stored credential path:
the copy-back of tokens rotated by the opt-in legacy maintenance jobs (Claude
auto-refresh and warmup, Codex maintenance; see
[usages.md](usages.md#legacy-link-model-maintenance)). These
maintenance jobs never run on the active profile. Each holds a per-account
advisory lock, `switcher/.maintenance/<hash of the refresh token>.lock`
(created for the job, removed on release), and a switch **holds the same lock
of its target account for the whole switch** (waiting up to 45 s for a run to
finish, then refusing with a "try again" message), so a run can neither be
mid-rotation when a profile goes live nor start in between. The order is
always account lock, then `.lock`; a run's copy-back takes `.lock` while it
holds its account lock, so the two cannot deadlock. The key is re-derived
after the lock is taken (a finished run replaces the token it was keyed by),
and a lock that cannot be taken at all refuses the switch. A profile that just
stopped being active is also left to its own sessions for 8 hours
(`switcher/switched_away.json`, advisory, not part of `state.json`). A file that exists
but cannot be parsed also holds other profiles' records, so a writer moves it
to `switched_away.json.corrupt` (a numbered suffix if one is already there) and
logs an error rather than replacing it with a one-entry file; listing never
touches it. Renaming the active profile drops stale entries under either name. If a profile became active
during a run anyway, copy-back writes through its link to the live file only
while the live file still holds the refresh token the run spent; otherwise the
rotated copy is rescued (see
[usages.md](usages.md#never-the-active-profile)). A rename during the
run also follows the profile when exactly one stored refresh token matches.
If the profile has a different refresh token, its current credentials are not
changed; the rotated file is rescued as a mode-`0600`
`backups/<tool>/<profile>-rotated-<UTC timestamp>.json` file and named in the
abort log. Removing a profile records a `0600` tombstone containing the tool,
profile name, removal time, and SHA-256 hash of its refresh token, without the
token itself. The last 50 removals are kept. A profile with no usable refresh
token is still removed; no tombstone can be written for it. Copy-back checks
these under the switcher lock and discards an in-flight rotation only when the
tombstone is newer than the run and no current profile holds that token. A
shared token in another readable profile can still receive its own rotation.
Every other unresolved copy-back saves the rotated file for recovery, including
no match, an unreadable sibling, or a tombstone from before the run. A corrupt
`removed.json` is logged as an error and treated as empty; the next Remove with
a usable refresh token rewrites it cleanly. If the live credential path cannot
be resolved, the credential file cannot be read, or the tombstone file cannot
be read or written, Remove logs the error and still deletes the profile; an
unresolved in-flight rotation is saved as a rescue.

The active profile's link is absolute, so renaming a profile keeps it valid, and
removing a profile deletes the link, never the file it points at (removing the
*active* profile still clears the live login on purpose, as described above).
The link is a host path: refresh and warmup containers never mount a profile
dir, only a temp dir seeded with the resolved credential bytes.

**Neither `aimebu prune` nor `aimebu prune -a` ever deletes anything here.**
These are your logins, not conversation state, and no prune flag advertises
destroying credentials. Removal is explicit:

```bash
aimebu switcher remove <tool> <name>
rm -rf ~/.aimebu/switcher          # all of it
```

## Web UI

**Settings → Usages** has the on/off toggle, plus a note naming any tool that
is enabled but not eligible.

The **usages panel** has the switching itself: the profile list per tool with
an active marker, Switch buttons, "Add profile", "Import current login", and
per-row delete. The active profile sorts first. Login email shows as a hover
tooltip on the plan badge (see the note on claude emails below), so a profile
with no usage snapshot has no plan badge and no email tooltip. Empty profiles
render as "needs login" rather than as zero usage, and a profile that has
credentials but no snapshot yet — the two seconds after a switch, or a cold
cache — renders as "Refreshing…" instead, since those mean different things.

The card header carries one **"Updated X ago"** value based on the oldest
profile timestamp, so it never claims the whole card is fresher than a row
beneath it. All profiles refresh at the provider interval, though their
requests may finish a few seconds apart.

Profile usage arrives with everything else on `GET /api/usages`; there is no
separate switcher fetch. The panel never triggers a network call of its own —
the background poller owns all fetching.

The usages API and sidebar always use profile rows. With no configured
switcher profiles (or when the switcher cannot initialize), each provider has
one active implicit profile named `local`; configured profiles replace that
implicit row. On a cold cache, those rows exist immediately and show
`Refreshing…` until the background poller fills them. Turning the switcher off
hides switching controls only — it does not change the usage data shape.

An icon right of the profile name shows token-expiry state: a checkmark while
the token has more than the near-expiry threshold left, an hourglass at or under
it, and a red hourglass once it has expired. The icon and the profile name both
carry the same hover tooltip with the absolute expiry timestamp and a relative
"in 3h 20m" / "expired 5h 2m ago" reading. The default threshold is 3 hours,
short enough that a checkmark means the token is genuinely healthy — claude
tokens live only about 8 to 12 hours, so a wider window would leave every claude
profile showing an hourglass permanently — and long enough to switch profiles by
hand before one lapses. When Auto-refresh tokens (see
[usages.md](usages.md#token-only-refresher)) is enabled the threshold tightens to
1 hour: the container refresh gives the long heads-up automatically, so only a
genuinely imminent lapse still warrants the hourglass. With it disabled the
3-hour heads-up stays, so manual users keep the wider warning.

For codex the expiry shown is the access token's. A codex `id_token` is a
short-lived identity assertion minted at login and never refreshed, so it is
normally long expired on a perfectly healthy install; only the account email is
read from it.

Two actions confirm first, because both end a working login: switching to an
empty profile, and deleting the active profile.

## A note on claude profile emails

Codex stores its account email inside each profile's own token, so every codex
profile is labelled correctly and immediately.

**Claude has no email in its credentials file**, and none in its usage API
response either — the address exists only in `.claude.json`, a single global
file that stays behind when credentials move. So a claude profile's email can
only be learned while that profile is *actually in use* and claude-code has
authenticated under it.

aimebu captures the address into a `profile.json` beside each profile's stored
credentials, on `import` and on every switch away from it. Resolution order:

1. the profile's own captured address
2. the live account file — only for the active profile, only if nothing was
   captured yet
3. otherwise **nothing**

A freshly added profile therefore shows no email until you have used it once.
That is deliberate: showing whichever account happens to be live would label
the profile with someone else's address. For the same reason, switching to a
profile and straight back — without running claude in between — records
nothing, because the account file has not caught up yet.

## Recovery

```bash
aimebu doctor
```

reports a stale active pointer, a profile with no stored credentials while the
live file is also missing, and any backups available to restore from. Backups
are plain JSON files under `~/.aimebu/switcher/backups/<tool>/` — copy one back
over the tool's credential file by hand if you need to.
Rotated rescue files use the `-rotated-` suffix and retain the tokens produced
by a maintenance run that could not safely update an existing profile.

## Token-only credential model

Links share one refresh token between every running session and the server,
and a refresh-token reuse revokes the whole family. The token-only model ends
that: **the profile store holds the only full credentials** (regular 0600
files, the active profile included) and the server is the only party that
spends refresh tokens. The live `~/.claude` and `~/.codex` files become
*targets*: copies with the access token (Codex: plus the account id) and **no
refresh token** (Claude: the `refreshToken` key is deleted; Codex:
`refresh_token` is `""` with `auth_mode: chatgptAuthTokens`). Sessions use the
access token until it expires and re-read the file when it is rewritten.

- The **Auto-refresh tokens** setting starts converting each eligible tool
  immediately. A tool that fails a safety check shows `waiting: <reason>` and
  is retried on later ticks; the other tool can migrate independently. It
  refuses a live token with under 15 minutes left (a session may be refreshing
  it), a running Codex process that could spend the same refresh token, and a
  tool with no active profile. This 15-minute migration guard differs from the
  15-minute warning in doctor and Usages for a stored token near expiry.
- `aimebu switcher migrate-copies` is an **advanced** manual command for the
  same conversion. It does not change the saved toggle, and startup does not
  undo a manual migration while the toggle is off. `--tool claude` leaves
  Codex linked; `--allow-running-codex` overrides the Codex-process guard.
  The CLI refusal for a token inside the 15-minute window exits with code 2.
  Start or restart the server after manual migration.
- Migration backs up the live file, saves the full credential as a regular
  stored file, rewrites the live file token-only, then writes
  `token-copies.json` last (version 2 has a `migrated` map of tool to time; a
  version-1 marker means both tools). Codex process checks cover the host and,
  best effort, running Docker containers (`docker ps`, then `docker top`).
  If Docker is unreachable, the CLI says containers were not checked.
  The refresher leaves Codex profiles alone for ten minutes after migration;
  restarting running Claude sessions is optional because Claude re-reads its
  live file before it refreshes. A second migration of a migrated tool is a
  no-op.
- Turning the setting **off** refreshes a near-expiry active profile first,
  restores its full credential to the live file without losing unrelated live
  fields such as `mcpOAuth` (the merged document goes into the profile store
  first, so a failure at any step leaves the store and the live file
  consistent), returns the active stored path to a symlink, and removes that
  tool from the marker. A new `/login` in the live file is
  imported before the restore. If restore cannot finish, Settings shows
  `waiting` with the reason and retries on the next tick.
- `aimebu switcher use` on a migrated tool hands the switch to the running
  server when one answers on this machine (`AIMEBU_URL` must be a loopback
  address; the server owns the refresher, so a due incoming profile is refreshed
  first) and prints the server's answer, including its refusals. With no server
  it switches locally with the same live-file fan-out; there is no refresher in
  that process, so a switch to a profile that is due for a refresh is refused
  with a pointer to start the server (the server refreshes profiles ahead of
  expiry, so that only happens after it was down).
- A **switch** no longer captures, restores or links anything. It refreshes
  the incoming profile if it is due (a failed refresh refuses the switch with
  409), rewrites the live targets from the profile, and moves the active
  pointer. While a login is waiting in the live file it refuses (retry in a
  minute; see Login import below).
  Over HTTP these refusals are 409 (login pending, incoming refresh failed,
  profile still linked, codex running); login pending and incoming
  refresh carry a `Retry-After` header; a migration request while the feature is off is 400.
  The web UI shows the server's message as-is.
- **`import`** adopts a login that has just been made (the live file holds a
  full credential the reconciler has not filed yet) under the name you choose,
  without creating the link described above. It saves a rescue copy and a
  backup, stores the login as a regular `0600` file in the profile store, moves
  the active pointer, and only then rewrites the live file token-only, so the
  live file and the state never disagree about whose login it holds. A failure
  before the pointer moves undoes the profile and leaves the live file as it
  was; if only the final rewrite fails, the profile is stored and active and
  the reconciler strips the file within a minute. A live file that is already
  token-only is still refused (there is no login to adopt), and so is an
  install whose fan-out is not wired.
- **`remove`** records the removal tombstone from the profile store for every
  profile, the active one included: the live file is token-only and holds no
  refresh token, so the store is the only place the removed account's token
  can be found. It also deletes any rescued login (`backups/<tool>/_login-*`)
  that holds one of the removed profile's refresh tokens, so a removed profile's
  credentials do not survive in the copy that created it; rescued logins older
  than a day are pruned whenever a new one is saved.
- **`rename`** of the active profile makes no backup of the live file (it is
  token-only); the profile's moved backups and its store file are the recovery
  source.
- The **server** keeps targets current: the refresher
  ([usages.md](usages.md#token-only-refresher)) refreshes profiles ahead of
  expiry and fans each new credential out to the targets bound to that
  profile. Every live-target write takes the switcher lock and re-reads the
  active profile under it, so a switch landing meanwhile is never overwritten.
  `server/credential-targets.json` lists extra targets (a profile-pinned
  or follow-the-active file elsewhere); the two live files are implicit.
  This is **advanced: hand-edited, no CLI yet**. The file is read at server
  start; a missing file means no extra targets:

  ```json
  {"version": 1, "targets": [
    {"id": "ci-claude", "tool": "claude", "path": "/abs/path/.credentials.json", "profile": "main"}
  ]}
  ```

  `id`, an absolute `path` and `tool` (`claude` or `codex`) are required (an
  entry missing any, or with another tool, is skipped silently); `profile`
  pins the target to one profile, and omitting it follows the tool's active
  profile. Field names match case-insensitively.
- After a switch to a different **Codex** account, a Codex session that was
  already open keeps refusing the new credentials (it guards its account id)
  until it is restarted. Claude sessions adopt the rewritten file by themselves.
- Startup order: the reconciler pass runs first, then the refresher.
- `aimebu doctor` reports the model's health for each tool listed in `token-copies.json`
  (including a warning when `harness-docker-ctrl`, which the refresher needs on
  the server's PATH, cannot be found),
  and the Usages panel shows credential alerts. An alert that ends in "log in
  again" (`reauth_required`, `needs_relogin_soon`, `expired`) is also posted
  once per edge as an attention-raising system message to the human members of
  every room that has one.

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
   account-state file, Codex by the `id_token` email, else the account id. The
   Claude file is the most recently written of `<credentials dir>/.claude.json`
   and claude-code's default for the server's environment (`~/.claude.json`, or
   `$CLAUDE_CONFIG_DIR/.claude.json`): which one is real depends on how the
   sessions were started, not the server, and `aimebu doctor` should flag two
   files that name different accounts. An account that cannot be identified
   never matches anything.
3. The login goes to the first rule that applies:
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
   destroy a login nobody has rescued.

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

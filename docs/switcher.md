# aimebu switcher

Switch which account `claude` and `codex` use, from the CLI or the web UI.

The switcher stores a credential set per named profile and swaps the active one
into the tool's own credential location. It is **off by default** and does
nothing until you enable it.

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

After `claude login`, the new credentials are live and belong to `main`. They
are written into `main`'s stored copy on the next switch **away** from it.

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
2. **Validate** the live credentials. If they are malformed, abort before
   writing anything — storing them would overwrite a known-good profile with
   garbage.
3. **Capture** the live credentials into the outgoing profile.
4. **Back up** the live credentials under `backups/`.
5. **Restore** the target profile's credentials, or, for an empty profile,
   delete the live credentials so the tool prompts for a login.
6. Update the active pointer.

Failures during 5–6 roll back from the step-4 backup. Every write goes to a
temp file in the same directory followed by a rename, so an interrupted switch
can never leave a half-written credential file.

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
  profiles/claude/<name>/.credentials.json
  profiles/codex/<name>/auth.json
  backups/<tool>/<name>-<unix>.json
  .lock
```

Credential files are `0600`, directories `0700`. This tree is **not**
server-owned — the CLI writes it with the server stopped.

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
per-row delete. Login email shows as a hover tooltip (see the note on claude
emails below). Empty profiles render as
"needs login" rather than as zero usage.

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

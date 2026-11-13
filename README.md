# aimebu v2

aimebu runs a team of AI agents around your projects and tasks. Each project
has a home, each task has a room, and questions needing your input collect in
one inbox. You can steer or stop agents, see their activity and usage, and
archive completed rooms with their history.
Settings → Users creates one-hour, single-use links to invite people into a
project as viewers, operators, or owners. New people choose a login and
password; existing users sign in and apply the invitation to their account.
Project owners can change roles or remove members of their projects. The
global owner can issue password-reset links and deactivate accounts. Viewers
can read project rooms but cannot post in ordinary rooms or control agents.
They can post, react, and share attachments in a two-person private chat while
both people remain active project members and no agent has joined. A viewer
who is a peer in a private chat can answer its pending questions. See
[authentication](docs/auth.md) for permissions and API routes.
Owners and operators can pause, stop, or resume a whole room or project;
queued messages remain available when the scope resumes.
Optional daily cost and turn limits warn at 80% and pause dispatch at 100%.
Task rooms and the project home room can schedule a note for a member agent
or the room lead, once or on an interval, daily time, or weekly time (private
chats cannot). Room settings shows each schedule's
time zone, next run in the viewer's local time, and last result. Schedules
stop when their room closes and stay stopped after it reopens.
Task rooms can opt into a Git worktree at
`<project>/.aimebu/worktrees/<room-id>` on `aimebu/<room-id>`.
New agents in that room use its worktree checkout, allowing separate rooms to
keep their files separate. The worktree starts from committed HEAD of
the selected base ref; if the base has uncommitted changes, the room and API
show their count because those changes are not copied. Archiving removes a
clean, merged worktree and keeps its branch. Dirty or unmerged worktrees stay
on disk with a room notice and require explicit removal in Room settings.
Pending or failed opt-ins can be canceled while the room is open, and failed
creation can be retried. Removal waits while any active agent uses the
worktree checkout, including when forced.
A machine runs harnesses: the Claude Code, Codex, Pi, OpenCode and Mistral Vibe installs the owner sets up
under Settings → Harnesses (a harness is stored as a named launcher). A new machine has none:
nothing is seeded, and nothing is added until the owner adds it by hand or ticks what
Auto-detect found. Spawn and switch use the selected harness's stable ID. Upgraded machines keep the launchers that were enabled or in use. Migration 69 deletes unused
seeded ones and logs what it removed and moved.
When an existing v2 database has pending migrations, startup saves and verifies an
online SQLite snapshot in `server/backups/` before changing it. Each snapshot
is one standalone `.sqlite` file. Startup keeps the new copy and the four
newest older copies; if removal of an older copy fails, it logs a warning and
continues with the verified new copy. An agent's model that the old list lacked
is added to its harness
so no running agent changes model. A tool whose model list was edited keeps
one disabled harness so the list survives. After the first project is created and no machine has a harness with a model, the first run continues with a **Set up a harness** step (the
Settings → Harnesses component itself: Add, Auto-detect and models) and offers "Spawn your first agent" only once one is ready; "Skip for now" leaves a
"Set up a harness first" hint in place of the spawn button, and someone who cannot change harnesses (anyone but the owner) is told to ask the owner. The same step is offered
on its own to the owner who signs in while no machine has a harness with a model (after a v1 import, which carries none over, from another browser, or once
every harness was removed), once per browser session. That comes from the server, not the browser: Skip is stored with the account
(the `harness_setup_skipped` preference), so no browser asks again, until a harness with a model exists and the app resets it. Someone who cannot
change harnesses is never offered it. The spawn dialog
shows unavailable harnesses with the reason and points to Settings → Harnesses for setup (a link that opens it on
the dialog's machine for the owner; anyone else, an admin included, is told to ask the owner). A
harness can be selected by ID or by its name in `variant`; `native` and `docker` keep their
meaning for the harnesses named `native` and `harness-docker`.
"Only run agents in docker" (Settings → General → Agents, an owner or admin setting, off by
default; `GET`/`PUT /api/agent-isolation`) refuses to create an agent on, or switch one to, a native
launcher (409 `docker_only`) and holds the turns of one already there; the supervisor refuses
too, so a stale server or an older machine cannot get around it. A managed agent's key is held in
the supervisor's memory only: no file and no argument list holds it (see
[supervisor](docs/supervisor.md)).
The inbox offers a one-time higher cap or a pause until the next local day.
Health flags identify stuck turns, provider quota, disconnected supervisors,
held answers, and recovery waiting on agent cards, rooms, and the top bar. The agent card pills use plain words
("Not responding", "Reply not delivered", "Needs your decision"), and the Health list gives each problem a sentence and a
one-line explanation; choosing one opens its Inbox item, or else the agent's
room and profile. If the room or agent disappears while navigating, a brief
notice explains why the profile could not open.

## Install

Clone the repository and run `bin/aimebu`, a wrapper that builds the binary on
your machine on first use and re-uses the cached build afterwards. It needs a
Go toolchain at least as new as the `go` line in `go.mod`; an older Go
downloads the right one itself.

```sh
git clone https://github.com/hrubymar10/aimebu.git
export PATH="$PATH:<path-to-clone>/bin"
aimebu version   # builds on first run, then runs
```

A symlink to `bin/aimebu` on your `PATH` works too. The cached binary is
`aimebu-<os>-<arch>` in the clone and is not rebuilt when the source changes:
after `git pull`, delete it, or run once with `AIMEBU_FORCE_BUILD=1` to build
into a temporary binary for that run only (the cache is left alone; any other
value, including `0`, uses the cache). The binary reports the output of
`git describe` as its version.

The wrapper can also update the clone for you. Run in a terminal, a command
checks the remote's default branch at most once every 24 hours (a hard 5 second
limit, no credential or passphrase prompts, silent when the remote cannot be reached) and, when the clone is behind,
asks `aimebu update available. update now? (Y/n)`. Yes fast-forwards the clone
and rebuilds the cached binary (the old one stays in place until the new build
succeeds; a failed update restores the checkout), then runs your command on the
new version. A dirty checkout or a branch other than the remote's default is
never touched: it only prints a one-line notice. Notices go to stderr, and the
check is skipped for `version`, `help`, `--json`, `--machine` and `--plain`
runs, for `AIMEBU_FORCE_BUILD=1`, and whenever stdin, stdout or stderr is not a
terminal (so `aimebu agent-tools`, pipes and services never check).
`aimebu update` updates on demand and `aimebu update --check` only reports
(`current`, `behind`, `dirty`, `non-default` or `unreachable`). Settings:

| Variable | Effect |
| --- | --- |
| `AIMEBU2_UPDATE_CHECK=0` | turn the automatic check off (`aimebu update` still works) |
| `AIMEBU2_UPDATE_CHECK=apply` | update without asking, and without needing a terminal (still once per interval, never on a dirty or non-default checkout) |
| `AIMEBU2_UPDATE_INTERVAL_HOURS` | hours between automatic checks, default `24`; `0` checks on every run. The last check is the mtime of `aimebu-update-check` in the clone's git directory |
| `AIMEBU2_UPDATE_TIMEOUT_SECONDS` | time limit for the remote lookup, default `5` |
| `AIMEBU2_UPDATE_FETCH_TIMEOUT_SECONDS` | time limit for the fetch once an update is accepted, default `60` |
| `AIMEBU2_UPDATE_REMOTE` | the git remote to follow, default `origin` |

This branch is the v2 rewrite. The server, embedded and standalone
supervisors, web UI, and managed adapters are implemented.
Run `make test-full` for the same formatting, vet, and race-test checks used
in CI; its web unit tests run in a pinned Docker image, so it needs Docker (or
`WEB_TEST_RUNNER=local` and a local `node`, see
[CONTRIBUTING.md](CONTRIBUTING.md#the-web-tests-in-docker)). New contributors should start with [CONTRIBUTING.md](CONTRIBUTING.md)
(prerequisites, an isolated `make dev`, the test commands, the web UI layout
and conventions).

The `aimebu doctor` command runs read-only health checks on the installation:
the config directory, the SQLite database (integrity and schema version),
server reachability, the TLS certificate, the bind/allowlist settings, free
disk space, backups, machines, pending stops, launchers and provider limits.
Each check reports ok, warn, or fail with a hint on fixing it; the exit code is
0 unless at least one check fails (`--strict` also fails on warnings).
`--live` adds what only the running server knows (connected supervisors'
capabilities, launcher availability); `--machine` checks a host that runs only
the supervisor. When the account switcher is set up, doctor also checks its
state and active profiles. Use `aimebu doctor --json` for machine-readable
output. Doctor also warns if v1 external credential targets were not carried
over and fails if two token refreshers claim the same live credential file.
See [docs/doctor.md](docs/doctor.md).

`aimebu version` prints the version plus the VCS revision, commit time and a
`dirty` marker when the binary was built inside a checkout (a
`-ldflags "-X main.version=..."` override is printed unchanged). `aimebu help`
lists every command and the `AIMEBU2_*` environment variables.

`aimebu usages [provider] [--json|--plain]` prints the usage snapshots the server last
cached, read straight from the config root (no login, no network, works with
the server stopped); see [usage snapshots](docs/usages.md).

`aimebu sessions` lists managed agents from the local server database and
recent turns from the local supervisor journal. It merges matching agent IDs
and shows state, tool, launcher, room, latest turn, and a native resume hint
when available. The command only reads these files; a missing database simply
contributes no rows. Codex hints include its private `CODEX_HOME`, Pi
hints include the checkout's session directory, OpenCode hints name its
private session database, and Vibe hints name its private home and session directory. Remote Pi sessions and Codex,
OpenCode and Vibe sessions on remote or containerized launchers have runtime paths that cannot
be determined locally, so no command is guessed for them. Set
`AIMEBU2_CONFIG_DIR` to inspect another v2 data root; unrelated server bind
settings do not affect this command.

`aimebu prune` clears agent runtime state after confirmation; add `-y` to
skip the prompt. `aimebu prune -a` also clears all server and supervisor data.
Both modes require the server and supervisor to be stopped, and both preserve
`switcher/` and `usages/` under `AIMEBU2_CONFIG_DIR`. See
[pruning](docs/prune.md) for the exact paths and retention effects. The server also cleans up after itself, safely (old attachment files after 90 days by default, orphaned uploads, a bloated database; never a message, a room or a worktree) and shows disk usage in Health: see [cleanup](docs/cleanup.md).

Old messages are never deleted to save space; **message expiry** only keeps old
AI group-tag chatter out of what an agent is given (a mention, reply, DM or human
message is always delivered). The window is a setting under Settings → General; see
[message expiry](docs/retention.md).

Settings → Prompts lets the owner or an admin reword the shared sentences of an agent's turn
prompt globally, and a project's owner or operator for that project (the response format stays fixed); the owner or an admin also sets
whether a lead is always asked to attach the full plan under a visual plan
(Inline plans, default optional). While a save or reset is pending the page is locked (the scope
select, every box and button), so an answer always belongs to the page it was asked from; see
[adapters](docs/adapter.md).

Before a cutover, `node scripts/variant-gate.cjs` drives every tool and container
variant through task, pause, kill, and recovery on a real server; see
[the variant gate](docs/variant-gate.md).

Error and performance reporting to Sentry is **on by default, once the owner has answered a one-time popup** (its toggle starts on, with the maintainers' DSN filled in; "Not now" turns it off). Set
`AIMEBU2_SENTRY_DSN=off` (or another DSN), or use Settings → Telemetry, to stop or redirect the sending of crashes, 5xx answers and traces from the server and
the web UI to a Sentry project (the maintainers', yours, or a self-hosted one); `AIMEBU2_SENTRY_ENV` and
`AIMEBU2_SENTRY_TRACES_SAMPLE_RATE` tune it (the trace sample rate defaults to `1`: every request and page load). There is no Session Replay.
Message bodies, prompts, attachments, tokens, cookies, headers, environment values and query strings are never sent: see
[telemetry](docs/telemetry.md) for exactly what is and how to self-host.

See [the architecture](docs/architecture.md) for component decisions and
[the contract](docs/contract.md) for the data and protocol contract.

`aimebu fe` opens the web UI of the configured server in the browser (`AIMEBU2_PUBLIC_URL` when set, otherwise the
address from `AIMEBU2_BIND` and `AIMEBU2_PORT`); over SSH or with no display it only prints the address.

The server writes `server/aimebu.log`: what it prints (each line stamped with its UTC time) and what the rest of the process logs, but not
the one-time setup token and link (those go to the terminal only). The file is capped by rotation at 10 MiB, with the three newest older
files kept as `aimebu.log.1` to `.3`; a start that loses the race to run only appends to it, and rotation begins once the server holds the
lock. `aimebu prune -a` keeps the log and its backups.

`aimebu server` runs in the foreground; `aimebu server start` runs the same server in the background (the way v1's `server start`
did), `aimebu server status` says whether one is running (exit status 0 or 1) and `aimebu server stop` shuts it down cleanly. A
server is "running" while it holds the lock on `server/.writer.lock`, so these also see a server started in the foreground or by a
service manager. The server writes its own pid file, `server/aimebu.pid`, once it holds the lock (in every mode) and removes it when
it exits; `stop` signals only the process that file names, only while the lock is held, and only if that process can be inspected and
is an aimebu server (otherwise nothing is signalled and the message says what to do by hand), so a stale file does not get an unrelated
process killed (use the service manager's own stop for a managed server, or it restarts it). `start` succeeds only when its server holds
the lock and answers as an aimebu server (another service on the port is not mistaken for it); after 30 seconds without that it fails
(exit status 1). It tells you when the server dies at startup (with the end of its log), and prints the one-time setup link when no owner
exists yet. Two starts at once run one server. The detached server does not inherit the caller's file descriptors (`start` marks every one above stderr close-on-exec before it launches it), so a lock held by the command that ran `start` (such as `flock`) or a pipe of the calling script is not kept alive by the server after that command ends. `status` and `stop` leave a config directory that still holds v1's files alone. `start`
also
refuses a config directory that still holds v1's files: the first start moves them, with notices, so run `aimebu server`
in the foreground for that one (see [the cutover runbook](docs/cutover-runbook.md)). Stop waits up to a minute for a clean
shutdown and never kills the process. To run the server as a service, give the service manager the foreground `aimebu server`
(see [release notes](docs/release.md#running-as-a-service)).

Owner and host-machine bootstrap is available through `aimebu server`. With
no owner yet, the server prints a one-time setup token (and writes it to
`.setup_token` in the config directory); open the server's address and enter it to
create the owner in the browser ([owner setup](docs/auth.md#first-run-owner-setup)).
For a headless start set `AIMEBU2_OWNER_LOGIN` and `AIMEBU2_OWNER_PASSWORD`
before the first run instead. Either way the server writes the host machine key to `server/host-machine.key` (0600) and
prints that path. Its database defaults to
`~/.aimebu2/server/aimebu.sqlite`; `AIMEBU2_CONFIG_DIR` selects another
root. The development build keeps this separate root and listens on
`127.0.0.1:9897` after bootstrap until release cutover. Set
`AIMEBU2_BIND` to an IP literal and `AIMEBU2_PORT` to a TCP port to change the
listener. `AIMEBU2_ALLOW` limits client source IPs to comma-separated IPs or
CIDRs; its default is `127.0.0.0/8,::1/128`. When binding to a non-loopback
address, include the remote network explicitly, for example
`AIMEBU2_BIND=0.0.0.0 AIMEBU2_ALLOW=127.0.0.0/8,::1/128,192.168.1.0/24`.
Set `AIMEBU2_TLS_CERT` and `AIMEBU2_TLS_KEY` together to add HTTPS on
`AIMEBU2_TLS_PORT` (default `9896`) while retaining HTTP; the HTTP listener then
redirects other machines to HTTPS. Both listeners use the same bind and allowlist.
Sign-in, setup, invites and pairing answer only a `Host` of `localhost`, an IP address,
the public URL's host or a name in `AIMEBU2_ALLOWED_HOSTS` (comma-separated), which
you set when you reach the server by a LAN name; when upgrading, read the [upgrade notes](docs/upgrade-notes.md). See [HTTPS setup](docs/tls.md) for certificates,
remote supervisors, and a Caddy example. Behind such a proxy, set
`AIMEBU2_TRUSTED_PROXIES` to the proxy's IPs or CIDRs (default empty) so login
throttling tells clients apart by `X-Forwarded-For`; the allowlist never
reads that header.
Startup logs the effective allowlist and warns when a non-loopback bind still
uses the default loopback policy. The authenticated JSON API and WebSocket
event stream are described in [the API guide](docs/api.md). See
[authentication](docs/auth.md).
The embedded supervisor's own dial address is added to the effective
allowlist when necessary, including when binding to a specific interface IP.
The API can close (archive) and reopen non-home rooms while preserving their
history; see the room lifecycle routes in the API guide.
A new project starts with its home room and a `general` task room beneath it.
The home room is the lead's: with the project setting **Home room: lead only**
(on by default, in the home room's Project section) the spawn dialog there
offers only lead roles and the server refuses others with `home_is_lead_only`;
workers go into task rooms such as `general`. Turning it on leaves agents
already in the home room alone.
Settings → Usages shows Codex, Claude Code, GitHub Copilot, Mistral, and
Ollama Cloud usage snapshots. Codex and Claude use local OAuth files; the
other providers are configured in Settings. Provider secrets stay in the
private `usages/config.json` under the v2 config root. Usage polling never
refreshes harness tokens or writes their credential files; expired tokens
retain a stale snapshot until the harness renews them (on an install migrated
with `aimebu switcher migrate-copies` the server's token refresher renews them
instead; see [account switching](docs/switcher.md#token-refresher)). Optional
scheduled idle-profile warmup opens five-hour usage windows through a short CLI
request ([warmup settings](docs/usages.md#scheduled-idle-account-warmup-account-maintenance)). The optional account
switcher can capture and swap Codex and Claude file-based logins on the host
machine. The server can also switch accounts for you when a profile runs out of
quota ([automatic account switching](docs/quota-switcher.md), off by default), and a
[limit guard](docs/quota-switcher.md#limit-guard-acting-before-100) holds new turns, and moves the account, at the tool's own switch limit and
says so at 99% of a window (on by default). Separately, **daily limits** cap what a
project may spend in a day (cost and turns, all its rooms together): the owner sets a
default in Settings → Usages, and each project inherits it, sets its own, or has no
limit in Project settings. Fresh provider windows at or below 20% and 10%
remaining create global Inbox alerts for the owner and global admins. The
owner can change those thresholds in Settings → Usages; only the owner sees
account details or changes anything there, and global admins can open the
Usages view read-only (limits and profile names, nothing that names an account
or how it signs in). See
[usage snapshots](docs/usages.md) and
[account switching](docs/switcher.md). An explicit
`aimebu switcher use <tool> <profile> --force` installs stored credentials
without an incoming refresh when an expired account needs a new login.
Its files use the v1 switcher format under the selected v2 config root:
development remains under `~/.aimebu2`, while cutover to `~/.aimebu` uses
the existing profiles in place.
Set `AIMEBU2_USAGES=0` to disable credential reads and usage requests. The web UI is served as one script the server bundles when it starts (esbuild as a Go library, no node); `AIMEBU2_WEB_BUNDLE=off` serves its ~240 separate modules instead, for debugging in a browser. The server starts its local supervisor automatically; set
`AIMEBU2_EMBEDDED_SUPERVISOR=0` for a remote-only server.
Set `AIMEBU2_STUCK_MINUTES` to a whole number from 1 to 1440 to change the
stuck-turn threshold (default 10 minutes). The server compares the running
turn's latest activity or usage event with that threshold.

Open `http://127.0.0.1:9897/` for the embedded web UI. It currently supports
owner sign-in, projects and nested task rooms, chat, replies, reactions, and
live updates. Message text fills the whole bubble on every screen (there is no line-length cap). Message bodies render a safe Markdown subset, including GFM
tables with column alignment, inline formatting in cells, and horizontal
scrolling for wide tables, bare-URL autolinks, `#42` message links,
indented code, `~~strikethrough~~`, `<u>underline</u>` (the only HTML tag recognised; any other angle bracket stays text), and `mailto:` links (also `[text](address "title")`, which carries the title, addresses with spaces, which are encoded, and `<https://…>`; only http, https and mailto links are ever live). Lists nest by indentation (bulleted and numbered, mixed), `- [ ]` and `- [x]` items show a disabled checkbox, and `---` is a horizontal rule. A fence that never closes is shown as typed, with the rest read as Markdown. Code blocks have a
Copy button on hover, focus, and touch (also in the raw view; it falls back
to `execCommand` on non-secure origins). A fenced block that names a
language (`go`/`golang`, `js`, `ts`, `python`, `bash`, `sql`, `yaml`, ... 27
grammars) is syntax-highlighted with the vendored highlight.js, loaded only
when such a block is shown and only for that language, with token colours from
the theme variables; blocks over 40 000 characters, unknown languages and
unlabelled blocks stay plain. ```` ```diff ````/```` ```patch ```` blocks, and
unlabelled blocks that are unified diffs, get green added lines, red removed
lines and styled hunk and file headers from our own renderer. Highlighting
only wraps the same text in spans, so Copy still copies the raw source. Live `@mentions` in messages are
highlighted and typing `@` in the composer offers group tags, role keys, room
agents, and human logins (in an ordinary room that is every active project
member; a private chat offers only its two people). Typing `:` and two or more
letters in the composer offers emoji by `:shortcode:` (Enter, Tab or a click
inserts the glyph; a complete `:shrug:` converts in place; code spans, fenced
blocks, URLs and times like `10:30` are left alone). The emoji button in the
composer, and "Find another reaction" on a message, open the emoji picker:
category tabs, search by name, alias or tag, keyboard navigation, a hover
preview, and a skin-tone selector remembered in `localStorage` (every person in
a joined emoji such as 🧑‍🤝‍🧑 or 💏 gets the tone; a bottom sheet on a phone). It is one shared popover that opens beside its button (to the right of it, above it where there is room, inside the window) and draws only the rows in view of its virtual grid, about a hundred cells instead of the 1,900 of the database; between opens it is out of the page. The emoji data is the vendored gemoji database in
`web/vendor/gemoji/` (see `web/vendor/README.md`). A reply is a message in the room like any other and shows its quote; the conversation it belongs to (the root and everything replying down the chain) opens in a side panel.
Agent final answers can add display-only visual plan blocks using the v1
vocabulary (Markdown, file tree, data model, endpoint, annotated code, diff,
checklist, question form, diagram, canvas, or prototype) and collapsed Markdown
appendix pages. Imported v1 messages use the same renderer. Unknown shapes
appear as escaped raw content, except that a block whose `data` is a bare string is read as its text. Plan diff blocks use the same diff renderer as fenced diffs, and plan blocks keep quote line breaks and the compact code block styling. Mermaid diagrams load the vendored v1 renderer
only when needed, with strict security, one retry when the first render ends in an error graphic, and a source fallback on render failure.
Agents also receive a bounded text rendering of visual blocks and appendix
pages in room notes. Put the substance of a plan or handoff in its message body.
The composer accepts up to four files per message (10 MB each) through paste,
drop, or the file picker. Files
upload immediately, and sending waits until they finish. Images open in a
mobile-friendly viewer; other files download. An SVG that passes a strict
safety check (no script, no external references) counts as an image and is
shown only through an `<img>` thumbnail, never as page markup; agents get its
source text; one that fails stays a download. Room access protects every file,
including private chats. Agents fetch delivered files on demand through a
scoped MCP tool or, for Pi Docker, a turn-scoped HTTP endpoint.
Agent messages show the agent's room name and role emoji, a harness icon whose
tooltip names the model, and a liveness dot (green working or idle, amber when
the agent waits for a recovery or approval decision, red when stopped or lost;
a sender with no live agent record shows as departed, and one that exists only in imported v1 history is red too, with the tooltip
"Imported from v1 (no live agent)"). A message that needs a
human shows a 🔔. A chart icon beside an agent message's `#42` opens a popover with what the turn behind it spent
(tokens, cost labelled when it uses the dated [model price list](docs/pricing.md),
model, and its place among the turn's messages, such as "2 of 3"); these are the turn's figures, not
per-message ones, and an unreported figure shows as a dash, never zero. The `#42` number is the message's own number, one sequence across every room (as in v1, where an imported message keeps its v1 number); it copies itself, a `#42` in text links to that message (opening its room when it is elsewhere), and an agent may name a message by it. Hovering or focusing a
message shows its actions as one compact toolbar that overlays and reserves no space and covers no text: at the bubble's top-right corner, over
its top edge, for someone else's message and for your own alike (it extends to the left when the bubble is narrower than the toolbar). Someone
else's sender line is at the left, clear of it; your own is right-aligned over that corner, so it leaves 22px below it and the toolbar sits in
that room. A chat column under 420px does the same for everyone's message. On a touch tablet your own messages keep the toolbar beside the
bubble, to its left, and on a phone it is part of the bubble's flow. On a touch screen a tap on a message shows it under the message: react (+, which opens a row of 👍 🆗 ✅ 👀 🙏 🎉), find another reaction (opens the emoji picker), reply (↩), reply in a thread, copy text and copy link. The root of a conversation
shows an "N replies" chip counting the whole conversation. In the panel, a message's reply button makes the panel's next send answer that message (a chip above the box names it; cancelling answers the root again), and there is no thread button there. The panel shows the conversation like `tree` (depth first, a reply under its parent, with guide lines). A reply that comes in away from the end is marked by a pill that leads to it, and a send goes to its own row. Arrow keys, Home and End move through the buttons. A row's toolbar is not in the page until the row is hovered, focused or touched (it is about 30 DOM nodes a row, more than half of a 100-message room's page); only the last three rows reached keep theirs, and a row that holds the focus, the pointer or an open reaction row keeps its own. System messages
appear as compact inline notices. UI experiments run behind an A/B/C pill (up to A–E, one letter per option label) in the top bar (`web/js/ui-variant.js`): an
experiment registers a name and labels for its options, the pill then reads "UI A | B | C … — TESTING: <name>" with the chosen option
underlined (`?ui=<experiment>:<a|b|c|d|e>` forces one and a bare or stale `?ui=` is stripped; the choice is kept per experiment in this browser), and with no experiment active the pill is hidden. An experiment can also be hidden: its pill shows only in a browser that opted in (`?ui=` naming it, once), and everyone else gets option A. No experiment is running now; pages moving to Preact replace their legacy module directly (see [docs/design/frontend-preact.md](docs/design/frontend-preact.md)). The message composer (`web/js/composer.js`, main and thread) is a rich-text editor (ProseMirror, vendored in `web/vendor/prosemirror/` and loaded the first time it is used) with a Slack-like toolbar (a row across the full width over the input, with attach and emoji as its first items; on a small screen they move to the bottom row; on a phone, and on a touch screen up to 900px wide, the composer is one row of attach, emoji, **Aa**, the input and send, in the thread composer too, and the toolbar opens above it when Aa is pressed and closes when it is pressed again, a view state of each composer that is not stored, while a phone held sideways hides the toolbar and puts Aa in the input's corner): bold, italic, underline, strikethrough, link, numbered and bulleted lists, quote, code and code block. Each button shows as pressed while its mark or block is on at the selection. The link button (or Ctrl or ⌘ + K) opens a small address row under the toolbar row: Enter adds the link, Escape cancels, and anything but an http, https or mailto address is refused with an inline message; on a link the button takes the link off. Keyboard shortcuts are Ctrl or ⌘ + B, I, U, K; + Shift X for strikethrough, 7/8/9 for the lists and quote, C for code; + Alt + Shift C for a code block. Typing converts as in a chat editor (`**x**`, `_x_`, `~~x~~`, backticks, `- `, `1. `, `> `, a code fence); Shift+Enter continues a list and a plain Enter sends. The **Aa** button at the toolbar's right end (or inside the textarea, when it is off; on a phone it shows or hides the toolbar instead, and with formatting off it turns formatting on) turns formatting off: a plain textarea with no toolbar, frame or editor, where Markdown is typed as text. The choice is kept in this browser and shared by the message and thread composers; the Markdown text is exactly what it was when you switch either way (the editor shows a draft as plain text, so switching writes nothing back unless you edited in the editor). Pasting Markdown text into the editor (`**bold**`, `` `code` ``, lists, quotes, code fences, links) turns it into formatting; so does HTML that a code editor copies as plain spans, and HTML copied from a rendered message keeps its formatting without the code blocks' Copy buttons. Plain prose stays plain, and so does a paste with something the editor has no place for (a heading, a table, a rule, an image or a task list) or one made with Shift held (paste as plain text). The `@`, `<`, `/` and `:` pickers and image paste work in both modes. The message always goes out as the Markdown in the box, with underline as `<u>…</u>`; characters typed in the editor are not escaped (the renderer has no escapes either), so a literal `**` in the editor is sent as bold, and pasted links are kept only for http, https and mailto. The top-bar Knowledge viewer curates
project memory, which starts disabled
for each project. Enabling it lets project agents save facts and receive them
with normal turns. The Notes button in each task room stores notes with that
task even when project memory is disabled. Each edit records its author and
revision; project viewers can read but cannot change records.
The viewer opens on **All visible**, which lists the facts of the project chosen in its "Project facts from" picker, every user
profile and the shared agent notes together, each tagged with its scope. A usage
bar per scope (project, each profile, shared notes) shows bytes used against
that scope's cap. **Clear filtered** deletes the editable records of the current
view and **Clear all** does the same across All visible.
Room settings can turn off message and task-note recall for one room while the
project setting stays on. This does not erase saved records. Agent tools can
list, revise, and remove records with revision checks, and link new facts to a
source message from a joined room.

Agent cards show harness icons, a state pill, and a live working timer when an
agent is in a turn. Expand a card for activity, the memory gauge, and its
last-seen tooltip; secondary actions appear on hover, focus, or touch. Agent
cards offer a tool/model switch that keeps
the agent identity and room, finishes any running turn, and starts a new
generation with recent room context. A memory gauge shows reported context
use against the reported window, with input/output tokens and recorded cost
(labelled when it includes a price-list estimate)
in its tooltip;
Claude Code uses its last main-thread assistant message for the context
estimate, and the gauge label caps at 100%;
unknown values appear as “—”. Compaction posts a room notice with the reported
before and after token counts. The spawn dialog's Name field starts with a free name from the server's pool (its dice button asks for another; an empty field lets the server pick), and a refused name is answered under the field. The dialog reads tool availability,
detected CLI versions, and model choices from the server catalog (the owner edits
the models of each harness in Settings → Harnesses; exactly one is the harness's default,
which is preselected; a manually removed model falls back to it, while an agent keeps a detected model that disappears from the harness); unavailable
tools show the reason and cannot be selected. Settings → Roles edits the
global role names, emoji, instructions, cardinality, capabilities, and ordered
default harness/model choices. Each role picks defaults in turn (with a durable
cursor per project and role) or randomly. Project settings can inherit or
override each role's list and mode; an empty list leaves the choice manual.
Unavailable machines, unmapped project folders, launchers, and models are skipped. The Spawn dialog
prefills the next available choice and remains editable. Roles are global
only, with no per-project roles.
Each role is a card; Edit opens it in the editor below, and Cancel, Esc or the
card's close button shut the editor without saving (asking first when it holds
changes).
Changing a role's capabilities requires an owner. Deleted roles remain on stopped agent
history and can be recreated under the same key; those agents cannot restart
with the deleted role.
Fresh projects include leader, worker, reviewer, security reviewer, test
reviewer, and UX reviewer roles. The built-in instructions cover task-room
handoffs, independent plans and reviews, evidence-backed verification, and
structured human asks. Global instructions refer agents to each project's
`AGENTS.md` for local test and commit rules. Upgrades refresh only untouched
global defaults; edited roles retain their
instructions.
Assignments to spawned agents need a live mention in ordinary prose to wake
them. Agent replies to other agents keep their thread link without waking the
parent author; an explicit mention or handoff wakes the intended agent. Agent
replies to humans still target them, and human replies still wake their parent
author, and also the agent that started the conversation when that is someone
else. A message has one link, `reply_to`; the conversation it belongs to
(`thread_root`) is derived from the reply chain; a `thread_root` a writer sends is ignored.
The UI uses v1's dark
palette and compact room layout. Settings → General offers ten themes
(Dark, Light, Red/Blue/Green Dark/Light, and High Contrast Dark/Light).
The choice is saved to the account (`/api/me/preferences`), so it follows
the person to other devices; the browser keeps a cached copy to apply it
before the page renders. Settings → General also holds two chat switches, saved
the same way: the
System events bell (on by default; a feed of what needs a look, with an unread count
that counts only what the feed shows: agents spawned, started, stopped or removed, failures, a
process that exited with a code other than 0, lost or unstarted turns, the loop guard,
credential alerts, usage limits and their clears, automatic account switches, a session reset, an admin changed or a user deactivated. Routine events (an exit with
code 0, a key rotated, a rename, someone joining, a machine approved) are left out, with no
"show all". A start, stop or pause is one row, "owen@test: started by wren". Each row is one
line, event, agent and room, time; a click or Enter on it opens the detail, such as an exit
code. The server decides which events are notable, marks them `notable` and can return only those with `?notable=1`; see the API guide) and the Message
debug inspector (off by default; v1's bug icon in each message's header line, next to its `#id`, opens its
stored record, who it addressed, the wake note and turn each agent got,
and the related events, with previous and next to walk the room's messages and the Reply in thread, Copy
text and Copy link actions). The HTML revalidates on refresh and its CSS and JS
modules use a content version, so a deploy reaches a normal browser refresh.
The New project button (+) sits to the right of the Filter rooms box. The room header
shows the room kind glyph and a member-count chip, with the full count in its
tooltip, with the Room settings button right beside it (on a phone it is the first entry of the room's overflow menu instead), and
the room's icon buttons (task notes (v1's brain; in task rooms, and in the home room it
opens the project's memory instead), go to
message, raw Markdown, export). Every room has the same Room settings dialog, a full-height pane
with a title bar and a close button (v1's settings pane), with
the sections that apply to it. Its **Title** field (for owners and operators; a
private chat has none) is the only place a room is renamed: the header and the
room list follow at once, and the home room's title is the project's name, so
editing it renames the project too (the project's owner or an admin). The home room's adds a **Project** section, the
project's own settings (name and folder, agents in this project, daily
limits, recovery and worktrees; there is no separate Settings → Project page),
its Schedules, a Project memory entry where task rooms have Task notes, and one
line saying that the home room works in the project folder while task rooms can
have their own Git worktree. Project folder inputs offer matching directories
as a path is typed, using the machine that holds the folder. Room settings (one control size throughout; Kind and Room ID show only while the message debug
inspector is on) offers
Archive/Reopen for non-home rooms and, for an archived task room, a destructive
Remove room (project owner only; the room and its messages are deleted for good,
see `DELETE /api/rooms/{room}`; a room removed elsewhere leaves every other open page's list, and
opening one a page still lists shows "<room> no longer exists." and goes to the project's home room, the
same as when the person lost access to it), and shows room information (Room settings also
lists the room's members read-only: people with their project role, agents with
their role emoji and label, and each singleton role with the agent holding it
or "vacant"), message-number
jump, raw Markdown view, and Markdown/JSON export. A jump lands on its message (a `#N` link, a reply quote, jump-to or an Inbox item, in another room too) and highlights it for about 1.5 seconds: a ring that fades out, or a plain outline when reduced motion is preferred. Archiving confirms that
agents homed in the room stop and open questions are withdrawn; its history
stays readable. Chat opens the latest 25 messages, loads 50 older messages
when scrolled to the top, and fetches context for jumps to older messages.
At most 800 messages stay rendered: scrolling back past that drops the newest
ones and turns the view into a history window, as a jump to an older message
does. A history window loads 50 newer messages when scrolled to the bottom
(dropping the oldest past the same limit) and becomes the latest view again
when it reaches the end. Messages that arrive while it is shown are not
appended; they are announced and counted on its Back to latest button
(`Back to latest · 3 new`). Sending a message from a history window jumps to
the latest view, keeping an open thread and its unsent reply.
At the latest view, messages that arrive while the room stays open drop the
oldest rendered rows once more than 300 are shown (800 while the list is
scrolled up, never rows on screen); scrolling to the top loads them again.
Live updates use a message cursor; room export still includes full history. The export is written by the server (`GET /api/rooms/{room}/export`), so a script can fetch it too: it names replies and attachments and carries reactions.
Message timestamps are relative (`5m ago`) with the full date as a tooltip. The
left and right panel collapse state persists across reloads. A message list scrolled to the
bottom stays pinned there while images, diagrams, and attachments finish
loading; one scrolled up keeps the visible message in place.
Opening a conversation (from the chip, or from any reply: it opens its root's) shows the latest 100 messages of it, loads 50 older replies at a
time, and shows the full reply count on the root message and panel. Jumping
to a reply shows its row in the room; opening the conversation from it shows it in the panel with a way back to the latest replies.
The latest-replies button stays hidden when a jump's context already reaches
the newest reply.
New replies while viewing that older context update the count and appear
behind a new-replies control until the latest page is opened.
Archived rooms move into a collapsible sidebar section with
Reopen actions, name filtering, and message search with direct jump to a hit.
Unread counts and the 🔔 on rooms with open inbox entries come from per-user
read cursors stored on the server, so they follow you across browsers; opening a
room marks it read. Refreshes while a history window is shown do not mark the
room read; returning to the latest view does. The open room is only ever marked
read up to the newest message the chat has shown: if a window cannot load the
messages that arrived while it was shown, it stays a window with them counted
and the room stays unread until they are shown. Room settings → Notifications sets, per person and room, how the room notifies you: **Default** (the Inbox, and chat messages that address you), **All messages**, **Only mentions and replies to me** (the room's unread badge then counts only those) or **Nothing** (no sound or notification from the room, its Inbox items included; they still arrive in the Inbox), plus a mute for 1 hour, 8 hours or until tomorrow 08:00. A room that is quiet this way (Nothing, or a running mute) shows a disabled bell in the room list; the open room, in a focused tab, needs no alert. The settings are stored on the server with the rest of the person's view of the room (`PATCH /api/rooms/{room}/preferences`). Hide is a per-user button in Room settings (the rows carry none). Nothing is
pinned. The sort button in the ROOMS header orders projects, and the rooms inside them, by name, by latest
activity, or manually (drag a project by its ☰ handle; the whole row opens the project's home room; in the manual sort the rooms keep
the order the server lists them); the choice and the manual order are personal preferences that follow the person across devices. The room list is
one list across projects, with no section labels (on opening, each project is drawn as its name in its final place the moment the project list arrives, and its rooms replace it when its own list lands, in whatever order they come; the order by latest activity waits for every list, since it depends on the rooms): each project's home
room (`⌂ name`) is the parent row and its task rooms nest under it, one level. Open room rows show `#name`, their member count and an unread badge; archived rooms keep their history without an unread badge. Rows also show a project chip
when there are several projects (left off a home room that only repeats the project name); the
row's `+` shows on hover or keyboard focus, and always on touch screens; a project's home row has a ☰ button next to it (same visibility; no arrows), whose menu holds New room (as the `+` does) and, in the manual sort, Move up and Move down (and, in that sort, the button is also the drag handle). The last message is
previewed only under the open room, and one Direct messages section covers every project and
always comes last. The box on top only filters the list as you type (Enter does nothing); rooms
are created with a row's `+` or a project's ☰ menu. A project's tree ends with an
`Archived (N)` row that lists its archived rooms inline. A chevron on a root room hides or shows
its children (and that Archived row). A project starts collapsed the first time this browser sees it; after that the choice is remembered in this browser (a project you opened stays open), and a collapsed
root shows its children's unread total. The collapse button in the ROOMS header folds every project at once
(or opens them all again once everything is folded). A `Hidden (N)` row closes the tree the same way and can show a room again.
The chat header shows the room's path before its title (`project › room`, each a button to that room; a project's home room has none), and on a phone only the nearest crumb stays, truncated. A hidden room returns when a new message addresses that user or someone joins
the room. These preferences are stored on the server and shared across that
user's tabs.
Closing rooms show their state and any stop-blocked
reason. With no choice of your own, the Rooms column starts collapsed in a window 701-900px wide (that default is not stored: expanding it keeps it expanded, and a choice you made always wins). The Rooms and Members columns keep their header row (and the rooms filter) in place while their list scrolls, and on desktop the Rooms column, the chat (with an open thread beside it) and the Members column are separate rounded panels with a 6px gap between them, and the gap is the resize handle: drag it to resize the column beside it, with a grip of three dots that shows while the pointer moves over the window, stays three seconds after it stops, then fades out over a quarter of a second (no fade with reduced motion), and the gap turns accent-coloured while hovered or dragged (or resize by keyboard: arrows move 16px, Home and End go to the ends); a double click puts the column back to its default width. The widths (Rooms 180-480px, Members 200-520px, and the chat keeps at least 360px, counting the gaps and an open thread beside it) are kept per browser, not per account. Both sidebars can collapse on desktop, each to nothing: a collapsed column is 0px wide and its outlined chevron button (v1's) moves into the chat header, left of the room name for Rooms and at the right end for Members; the members header sits on the chat header's row;
on phones, v1's bottom bar (Rooms | Chat | Agents) opens and closes the drawers (the top-bar Rooms button and the title bar's drawer buttons are not shown there; the top-bar icons are named as in v1: Usages, Memory, System events, Settings) (on desktop the column toggles replace the Rooms button, the room list stays beside the Inbox view so any room is one click away, and the Inbox button toggles back to the chat),
the room's controls (Notes, Room settings, Go to message, raw
Markdown, Export) collapse into one ⋯ menu so the title keeps room, and
the composer stays at the bottom. The top bar is 43px high on a desktop (a window over 700px wide that is not a touch screen), 53px on a touch tablet and
on a phone (49px up to 480px wide), and every control in it is an icon button with a tooltip and an accessible name (Inbox,
Health, Usages, Memory, Settings, Sign out, and Rooms on phones; the Agents button is an icon too; the members
panel's Spawn button is the one exception, the green Send look with a glyph and the word "Spawn"); the Inbox and Health counts are a small pill in the button's corner, and the accessible name carries the
number. Only the experiment pill is text. The same rule covers the close buttons, the sidebar's +, person and reopen buttons, the code-block and profile Copy buttons, the agent card's Message, Pause, Stop, Switch, Start (Resume on a paused agent) and Remove buttons, the scroll helpers (Back to latest and its thread twins, with a count pill), the Inbox card's "View in room" and the room menu's ⋯: icons with a tooltip and an accessible name, from glyph files in `web/icons/` (sources are credited in `web/vendor/README.md`). Primary dialog and form actions, tabs and content-driven buttons (answer choices, reactions, #ids) stay text. The glyphs are files in `web/icons/` drawn through a CSS mask, so one is
replaced by replacing its file. Secondary controls such as machine Refresh and
Copy command, card Approve, memory activation and row Delete use the same
tooltip and accessible-name rule; their icon and label follow the current
action. The bar
also has a live connection indicator
at its far right. The controls align to the right of the logo. At any width,
when space runs out, Sign out, System events, Memory, then Health move into the
More menu in that order (the version text and the experiment pill's long label
go first on a touch tablet); controls that fit remain in the bar, and the More
button is there only while one is folded. More carries
the Health warning color only while Health is in the menu. The Usages icon opens the usages column in place of the members column (the owner
and global admins; admins read it, and Refresh and Switch are the owner's), laid out as in v1: a toolbar on the left (close, refresh, group by) and the title on
the right; one card per provider with its icon, "Updated Ns ago" and each profile (the active
one first with an ACTIVE badge; every profile with its token-expiry icon, a SWITCH pill on the
inactive ones, and its plan), Session/Weekly bars with the expected-pace
tick, "N% left" with the reset countdown, and the reserve/deficit line. The group button lists
each provider's active profile first, then a divider and the other profiles (remembered in this
browser); with no provider enabled the column offers Open Settings → Usages. The one-row composer
grows as text is entered, sends with Enter, and
inserts a newline with Shift+Enter. On a phone or an iPad with no fine pointer (touch only: `(hover: none) and (pointer: coarse)`, read at every key, so an iPad with a trackpad or keyboard acts like a desktop) Enter makes a new line in the room and the thread composers and only the send button sends; an open picker still takes Enter. Pressing ↑ in an empty composer (or with the
caret at its start) recalls your earlier messages in that room, newest first;
↓ walks forward and Esc restores the draft. It works in the rich composer (over the whole document, so ↑ on the second line of a draft only moves the caret) and in the thread panel's composer, where it recalls your messages of that conversation; Shift, Ctrl, Alt and ⌘ with the arrows, composing text, and an open picker (mentions, macros, commands, emoji) keep the keys for themselves. Settings → Macros edits shared `<KEY>`
composer expansions for the owner and global admins; all signed-in users can insert
them from prefix suggestions (typing `<` opens the list, as in v1: an outlined
list over the composer, one line per macro with its first 40 characters; ↑/↓
move, Enter or Tab expands it in place, `\<` writes a literal `<`) and edit the
draft before sending. The `@` mention, `<` macro and `/` command lists are one list box:
a single active row (the pointer moves the keyboard selection, so two rows are never
highlighted), `aria-activedescendant` on the composer, and the active row scrolled
into view. The v1 import wizard previews copied macros and
keeps conflicting v2 text unless the owner selects the v1 version. Managed
agents can read the shared map with `list_macros`; they cannot edit it.
The inbox shows open questions,
approvals, recovery items, and held deliveries, with a specific held reason
when the server provides one. The Health button counts affected agents across
rooms and opens a list that jumps to an affected room. Room badges count
affected agents, while agent cards name each flag and show a known provider
reset time. A provider limit pauses further turns, keeps the failed request
queued with a caution to check prior actions when needed, and offers
"Retry now" in its informational inbox item. The display refreshes on
events and every 30 seconds.

Agent messages can request a human answer with `needs_attention: true`.
They may add two to four `proposed_answers` for quick buttons or up to ten
`open_questions`, each with two to eight options and an optional description.
The inbox and room show the same pending request. In the room the buttons are a row under the message's card, outside it and aligned with it (the viewer's own messages on the right), as in v1. A proposed answer is a compact button (as in v1): a click sends it addressed to the requesting agent (`@author answer`); Shift-click puts the same text in the composer with the reply link, to change before sending. Sending that edited draft submits the answer. A free answer is typed in the composer as a reply to a plain free-text request. Other replies, including replies to Open Questions, leave the request open; answer Open Questions with the dialog, whose Shift-click also starts an editable answer draft. A free answer can also be sent from the Inbox's "Answer…" button under the chips. Each open question adds Other text. Sending resolves one inbox
item and posts a structural reply addressed to the requesting agent. A resolved request keeps its buttons disabled under the message after room changes and reloads, with the chosen answer shown. An unanswered closed request says "No longer pending".
The Open Questions dialog shows one question at a time with a numbered
stepper (answered questions are marked), an "N/M answered" counter and a
review step; Send is enabled only once every question is answered, and an
Other answer must not be empty. In a room, Shift-click on Send answers puts
the formatted answers in the composer as a reply, ready to edit before sending.
Picking an option with a click, Enter or Space moves on to the next question (and from the last to the review once everything is answered); with the arrow keys the arrows pick and Next moves on. Answers are kept per request: closing the
dialog, a re-render, or a page reload (same tab) keeps them, and they are
dropped when sent or when the questions change. On phones the dialog fills the screen with a pinned
action bar, every control is at least 40px, and tapping the Other box chooses
Other. The button shows the same
count. An explicit `ask` cannot be combined with these fields; `proposed_answers` and
`open_questions` cannot be combined with each other.

The open-item count appears on the Inbox button and in the tab title. Settings → Notifications → "Show message text in notifications" (off by default, kept on your account) puts the start of a chat message in the desktop notification instead of only saying that one arrived; the text is read through the room's own message list, so only what you may read is shown. New items play a short chime after a tab interaction. So do chat messages that address you (a mention, a reply, a group you are in), unless the room is quiet for you (Room settings → Notifications).
The Inbox lists open items with message requests highlighted beside their replies. A card's heading is one short line: a long question shows its first line (or that line's first sentence) as the heading and the rest as rendered Markdown under it, never as heading text. Delivery-failed notices show readable draft text when
available and can be dismissed after review; failures of archived rooms can be dismissed all at once ("Dismiss all
resolved"). Usage alerts show the provider, account name, window, remaining percentage and reset time inline and can be dismissed by an admin. The Inbox never shows raw JSON: a payload that is JSON is summarised in plain words, and the raw record is behind a
bug icon that appears when Settings → General turns the debug inspector on. Resume, Retry and Continue are normal buttons; Abandon,
Stop, and Remove actions use the destructive style. A phone login lands on the room you last had open, not on the Inbox.
The first time something needs your attention (an Inbox item, or a chat message that addresses you) while the browser has not been asked yet, a quiet bar under the room header offers browser notifications: its bell button is the click that raises the permission prompt, and its close button dismisses it for good on your account (the `notification_banner_dismissed` preference), so no browser shows it again. It never shows once the permission was granted or refused, and nothing of the message is put in it.
Settings → Notifications has a mute switch, a choice of inbox sound (Chime, Ding, Beep, Knock, or
any .mp3/.wav the owner uploads, up to ten files of 1 MB each) and a volume slider that shows its value as a percentage; the mute switch, choice and volume are
personal preferences saved to the account (`/api/me/preferences`), so they follow the person across devices (the browser only caches them to paint before sign-in); uploads live under `server/sounds/`, and a choice that no longer plays (for example, deleted by the owner) falls back to the chime. In a secure context (HTTPS or
`localhost`), that page also offers an explicit browser-permission button. Browser notifications appear only
while the tab is hidden, and clicking one opens its item in the Inbox or room,
according to the selected UI variant. Elsewhere,
Settings explains that browser notifications are unavailable. The mute switch is the master switch for every chime, Inbox and chat. Recovery
actions use the decision endpoint: uncertain turns offer Resume,
Retry, and Abandon; a turn that failed before it did anything (an error result such as
an expired sign-in, with no tool run) shows the harness's sentence with its raw output folded
away and offers Retry and Abandon only, and a retry that fails the same way says so;
loop budget items offer Continue and Abandon. Settings →
Project has an off-by-default auto-resume toggle for clean diagnostic reports,
with the risk of repeated actions explained beside it. Open
`/?mock_inbox=1` to exercise questions without agent messages, or
`/?mock_recovery=1` to preview recovery and budget decisions. The
right-hand room-members column and Spawn dialog can run the listed fake
adapter through the local supervisor. The column lists the room's people and
agents one per line (state-coloured presence dot, role emoji, the full name, the state and how long it has lasted as small
text, as in v1: `idle 0:44`, `working 1:12`, `paused`, `stopped`); DM, Pause and Stop are icon buttons that appear on hover or focus (a stopped agent shows Start, a play icon, in place of Pause and Stop; a paused one shows Resume in place of Pause; one that is still stopping shows only its Stop; always
shown on touch screens); clicking a row opens that agent's Agent profile in place
of the list, as in v1 (status, presence, last seen, runtime, role, native session,
a copyable resume command for a confirmed native session on the host (including its private session path when needed), working directory and branch, machine, launcher,
project, who spawned it, the last 24 hours' turns and cost, memory; the state shows as a pill, and the actions are labelled
buttons: Message, Pause, Stop (the red one), Switch…, Start (Resume on a paused agent) and Remove (the red trash can: a project owner or operator removes a **stopped** agent for good after a confirmation; it leaves its rooms, its name and role slot are free, its worktree of its own is removed by the worktree rules, and its messages stay, shown as "name (removed)"), each with a tooltip saying what it does and, when
unavailable, why (also written under the buttons);
a project viewer does not get the session id, resume command or working directory; a project owner or an admin also gets an **Output** section with the end of what the agent's harness printed to stderr, read from its machine on demand and kept there for 14 days, see [the supervisor](docs/supervisor.md#agent-output));
the × button or Escape returns to the members. The Spawn dialog's **Continue a Claude Code session…** entry (the one item of the ⋯ menu in its title row) lists the Claude Code sessions a machine already has for the project's folder (or one by id) and starts an agent that continues the chosen one with `claude --resume` (or works on a copy of it with `--fork-session`, your choice), in a room of its own by default; no history is copied. The home room's settings → Project can import many sessions at once, one room and one copy-working agent per session. See [continuing a Claude Code session](docs/claude-sessions.md). Settings opens on General. Agent rows show the latest turn activity from the live event
stream. Open `/?mock_agents=1` to preview cards and controls
without spawning. Run `make test-ui` against an isolated scratch server; it
requires Chromium or Chrome (set `CHROMIUM_PATH` if needed) and globally
available `puppeteer-core`.
On a stopped agent's card, **Start** continues its last native session when
available. **Start fresh** begins a new session with recent conversation from
the agent's rooms.
Agent cards open a private chat with the signed-in user, and a person's card in the
members panel opens one with that person (project members, including people with viewer
access); the Direct messages section only lists the chats. Each pair has one room per
project, listed with unread counts; archived chats can be reopened. Only the
participants see its room, messages, and events. A private agent turn stays
in that chat.
The human peer may archive or reopen their private chat, while the project
owner may manage any private chat. Recovery from a private turn is visible
to the peer but only a project owner or operator can decide it, since the
recovery block affects the agent across all rooms.

This is a temporary side-by-side setup: the v2 binary remains `aimebu`, but
its development default directory, port, and environment variables are separate
from v1. For a same-root cutover, stop v1 and point `AIMEBU2_CONFIG_DIR` and
`AIMEBU2_PORT` at its root and port. Startup backs up the v1 layout under
`.old-v1-bkp/` with crash recovery, then first owner sign-in opens a Review → Check
→ Import wizard. This is the only import opportunity; Skip import closes it permanently. The wizard
offers per-room, person, project, memory, setting, and role
choices; a dry run shows counts and access gaps before commit. Start over (after a
confirmation) clears the review choices before import; the finished page summarizes counts and destination projects. Start with
[Migrating from aimebu v1](docs/migrating-from-v1.md); see
[the import guide](docs/import-v1.md) for detailed mapping choices.

The wizard imports v1 rooms, messages, attachments, reactions, all three memory scopes, shared
macros, compatible global roles, each mapped human's read
position per room, and the owner's custom notification sounds. Rooms start open
unless you choose to archive them. The mapping makes project visibility and human identity explicit; see
[the import guide](docs/import-v1.md). Message numbers and reply links retain their
v1 IDs; native messages continue above the highest v1 number. Imported messages
use native message fields and renderers.
Imported history cannot wake agents. The Memory viewer (the brain button) is v1's
layout: a full-height pane with a filter row (what to show, which project, Refresh),
the Clear button at the top right (it asks first), one usage bar per scope with its
record count, and a card per record that leads with the scope's badge and key, then
its version, author's name and date, the text, and Save (off until the text is
edited) and Delete (Deactivate, Activate or Approve for a global record), with the
add form below. It shows project facts, user
profiles and shared agent notes. Project memory remains disabled after import
unless the owner selects an enabled destination project. Imported facts,
profiles and shared notes arrive as ordinary active records.

### Supported harnesses

`internal/adapter` defines the process-per-wake adapter interface, a scripted
fake for recovery and protocol tests, and native Claude Code, Codex, Pi,
OpenCode, and Mistral Vibe adapters. Claude Code probes the installed CLI, starts a stream-JSON print
process per wake, and resumes only with an explicit native session handle.
The Codex adapter resumes its JSONL thread on later turns. Pi uses a
server-issued session ID and parses JSON lifecycle events. OpenCode allocates
its own session ID on the first turn (like Codex) and resumes it with `-s`. Mistral Vibe runs `vibe -p`
with a private `VIBE_HOME` per agent, reports its session ID after a clean exit and resumes it with
`--resume`; its usage is read from the session's stats once it has exited. The spawn catalog
keeps CLI discovery separate from bounded model choices.

| Harness | Native launcher | Docker launcher | Managed agent toolbox |
| --- | --- | --- | --- |
| Claude Code | Yes | Yes | Native and Docker |
| Codex | Yes | Yes | Native and Docker |
| Pi | Yes | Yes | Native only; Docker uses a turn-scoped URL for attachments |
| OpenCode | Yes | Yes | Native and Docker (the launcher must forward environment variables) |
| Mistral Vibe | Yes | Yes | Native and Docker (the launcher must forward environment variables) |

Other harnesses are not supported by v2:

| Harness | Status |
| --- | --- |
| Cursor | Not supported |
| Cline | Not supported |
| Aider | Not supported |

OpenCode and Mistral Vibe in Docker are configured through environment variables alone, so their launcher must be a harness-docker
that forwards the variables the adapter names (`HARNESS_DOCKER_FORWARD_ENV`; see the harness-docker lifecycle guide): an older
launcher is refused with a message saying to update it, before a turn runs.

Launchers must be available on the selected machine and enabled by the owner.
The scripted fake adapter is for local demos and tests.

Account switching ([`aimebu switcher`](docs/switcher.md)) is a separate axis
from these tables and covers Claude Code and Codex only, and only when they
keep credentials in a file rather than the macOS Keychain.

The native live canaries are opt-in:

`AIMEBU2_LIVE_CC=1 go test ./internal/adapter/claude` and
`AIMEBU2_LIVE_CODEX=1 go test ./internal/adapter/codex` and
`AIMEBU2_LIVE_PI=1 go test ./internal/adapter/pi`. See
[the adapter guide](docs/adapter.md).
Docker variants launch `claude-docker`, `codex-docker`, and `pi-docker` when
those commands are on the supervisor's `PATH`. They require a mirrored
checkout. All Docker variants need a server bind/allowlist reachable from
the container: Claude and Codex use it for their toolbox, and Pi Docker uses
it to fetch delivered attachments. Pi Docker currently has no toolbox. Their
two-turn canaries are
`AIMEBU2_LIVE_CC_DOCKER=1 go test ./internal/adapter/claude` and
`AIMEBU2_LIVE_CODEX_DOCKER=1 go test ./internal/adapter/codex` and
`AIMEBU2_LIVE_PI_DOCKER=1 go test ./internal/adapter/pi`; OpenCode and Vibe have
`AIMEBU2_LIVE_OPENCODE_DOCKER=1 go test ./internal/adapter/opencode` (and, for a custom
OpenCode provider, `AIMEBU2_LIVE_OPENCODE_PROVIDER=1` with `AIMEBU2_LIVE_OPENCODE_PROVIDER_URL`,
`AIMEBU2_LIVE_OPENCODE_PROVIDER_MODEL` and the key in the variable named by
`AIMEBU2_LIVE_OPENCODE_PROVIDER_KEY_ENV`, default `LIVE_OPENCODE_PROVIDER_KEY`; add
`AIMEBU2_LIVE_OPENCODE_PROVIDER_DOCKER=1` for the Docker launcher) and
`AIMEBU2_LIVE_VIBE_DOCKER=1 go test ./internal/adapter/vibe`, and the Claude Code
fork canary is `AIMEBU2_LIVE_CLAUDE_FORK=1 go test ./internal/adapter/claude`.

The owner can add a remote machine in Settings → Machines. Add machine
shows a short-lived, single-use pairing command to copy and run on that
host. The same page lists pending, approved, and revoked machines; the owner
approves a paired machine there or revokes an existing one. Each approved machine also has a stop icon that stops every agent running on it (after a confirmation); owner and admins use it, within the agents they may manage. The supervisor
sends executable detection and versions for its configured harnesses.
Settings → Harnesses (owner only) has a machine picker and that machine's harnesses.
Closing Settings with unsaved form edits asks whether to Discard them or Keep editing.
The harnesses are a list with the chosen one's detail beside it (above it on a phone): its summary
and Enabled switch, then its models, each with a labelled switch per role. Ticking a role on a model makes it one of that role's default
agents; it saves at once, as the role's own change, and a model not yet saved cannot be ticked.
Role defaults, below the harnesses, shows each role's ordered defaults (move up or down, remove) and
its pick mode (the Pick order choice shows only for a role with at least two defaults, there being nothing to order otherwise; the same holds in the Roles form). Settings → Roles edits the same lists; a save in either place shows in the other. Saving a role there closes its form.
Unsaved model edits stay while the page redraws (until Save models, a machine change, or closing Settings). Save models sends the revision of the list the edit started from, so a list someone else saved in the meantime is not overwritten: the save is refused (409), the stored list is shown and the page says so.
Add harness takes a family, an executable name or path and a label; Auto-detect harnesses lists what
the supervisor found on PATH and in the usual user-level install directories
(`~/.local/bin`, `/usr/local/bin`, `/opt/homebrew/bin`, `~/.npm-global/bin`, `~/.bun/bin`,
`~/.opencode/bin`, the global npm prefix), once per real file, and adds only the ones ticked; each added
harness then has its models detected and saved (the first is the default), and one that cannot answer is
left empty with its reason and a note to use Models, where it can be retried. Each
harness is one summary row (label, status, version and active agents, its command, an Enabled
switch that saves on its own, Models and an Edit icon); Edit opens the label, executable, arguments,
environment, container and path settings, and Models opens its model list: slug (the exact id, never
an alias), label, a note with the full text on hover, and one default. Each row has a visible radio
and star; only the selected row has a filled star and says "Default". Trash icons remove harnesses or models, a refresh
icon reloads and saves the models the harness currently offers in one click. Reload models asks Pi (`pi --offline
--list-models`), OpenCode (`opencode models`) and Codex (`codex debug models --bundled`, the models
it lists) on the machine itself, native or containerized alike (for Mistral Vibe, which has no listing
command, it reads the `[[models]]` of the machine's Vibe config by alias and adds Vibe's built-in default): the harness's own arguments come
first, and the command runs in a fresh private `detect/run-*` subfolder of that machine's aimebu config
directory (`AIMEBU2_CONFIG_DIR`, by default `~/.aimebu2`). The supervisor removes the run folder after
detection, even when the command fails; a failed removal is logged without hiding a successful model list.
Before the next detection with the default folder, it also removes its own marked `run-*`
folders older than one hour; owner-chosen folders, even under `detect/run-*`, and recent runs
stay untouched. Collapsed Details contain the run path and the end of what the command printed to
stderr; the panel has a "Run the command in this folder" field: an absolute path on that machine, sent with the Retry
(nothing remembers it). When a Docker harness cannot see that working folder, the page gives only
the private `detect` folder to mount into the container and suggests retrying in another mounted folder;
login and timeout failures retain their own messages. Native failures use a short
message; both keep the harness's full error under collapsed Details. The same guidance appears when
auto-filling models after adding a harness. For Claude, native or containerized, the server reads the model constants of the
`anthropic-sdk-go` source on GitHub (only when the owner clicks, 5 s, cached by ETag, with a built-in
list when it cannot be reached). Reload models removes detected models the harness no longer offers, keeps models added by hand and labels edited locally, and adds newly offered models. It shows the numbers added and removed. An agent already using a removed detected model retains it as an unavailable choice until switched; new agents cannot choose it. Models are shown in natural, case-insensitive order with the default first. At most 1000 models can be
added to one harness. Spawn and switch let you filter models by typing. Empty lists or login prompts from Pi, OpenCode, or Codex report that the
harness has no models and suggest running `/login` in it. A successful reload saves immediately; manually added or edited models still need Save models. An OpenCode harness can also have custom providers (an
OpenAI-compatible endpoint: an id, a name, an `https://` base URL, or `http://` for this machine,
`host.docker.internal` or a private network address, and optional token limits): the provider stores
only the NAME of the environment variable its key is read from, never the key. The turn's private
OpenCode config refers to the key as `{env:NAME}`, native turns pass that variable through and Docker
turns forward it by name; set it where the turn runs (the supervisor's environment, or the launcher's
environment references). A provider's key variable and base URL are the owner's to set, like a launcher's command and
environment references: the refused names (`PATH`, `HOME`, `LD_*`, `XDG_*`, `OPENCODE_*`, `AIMEBU*`, …, and the keys of OpenCode's own providers such as
`OPENAI_API_KEY`, whose host value would otherwise go to the custom base URL) keep a saved provider from
disturbing the harness itself, not from naming another variable of the supervisor's environment, so only someone trusted with a machine's
harness settings should be allowed to add providers. Its models are listed like any other, as `<provider id>/<model>`, and a turn is
refused up front, naming the variable, when it is not set on that machine. A machine whose supervisor
predates providers holds such turns with a note in the room. On initial harness detection and Reload models, the supervisor reads the machine's OpenCode config (on the host for Docker), adds valid OpenAI-compatible providers with `{env:NAME}` or no key, and saves their config-listed models plus models from each provider's `GET <baseURL>/models`. Endpoint failure keeps the config-listed models. A literal key in the config is never copied; that provider is skipped with a warning because agent turns use a private config. In Settings → Harnesses the Advanced providers section is collapsed by default. It lets the owner edit provider rows, key variable names and token limits; Save providers checks the list revision, and removing a provider whose models are still listed offers to remove those models too. Key status shows whether a variable is set on the machine, never its value. The import icon remains available for manual overrides.
OpenCode model discovery runs in the same private config as a turn and asks each custom provider's own endpoint for its models, and
`aimebu doctor --machine` warns about key variables that are not set. Disabled or
unavailable harnesses stay visible with a reason; they cannot be selected for
spawn. Each change is shown in Activity. The supervisor validates definitions,
so changing an offline machine returns an error until it reconnects. The
supervisor saves its machine key locally
with 0600 permissions; revocation closes its
socket and invalidates the key. Spawn and Switch each have a machine picker that
defaults to the host; Switch defaults to the agent's current machine. The
remote project's folder can be mapped and saved in either dialog before
submitting. Tool and model choices come from the selected machine's catalog;
an offline remote machine cannot accept a new spawn or switch. Switch waits
for active work before moving. See [the supervisor guide](docs/supervisor.md)
for VPN bind/allowlist setup and folder requirements.

A project's host folder is its default agent folder. A remote machine's mapped
folder needs one owner admission before project agents can start there. A spawn,
switch, or reactivation outside an admitted machine/folder waits in the owner's
Inbox; approval grants that agent's canonical folder once, and the agent does
not start until approved. Changing a remote project's mapped folder clears its
machine admission. Agent cards show pending approvals and prior grants.
The server asks the remote supervisor to resolve paths before comparing folder components,
including symlinks. An older supervisor without this path check must be
updated before a remote agent can be admitted. Existing agents keep running;
their next spawn, switch, or reactivation is checked. This is a mistake guard;
machine keys and user/agent authorization remain the access controls.
Room worktree paths are inside the mapped project folder, so the same boundary
check admits them. Remote supervisors create worktrees in their own mapped
project copies; Docker agents use the checkout mount supplied by their
adapter.

`aimebu supervisor` remains available as a standalone remote-machine mode.
Both modes use a separate SQLite journal under `~/.aimebu2/supervisor/`,
authenticate with a private machine key, and maintain the machine WebSocket.
The server queues wake notes with messages and dispatches
eligible note batches to connected supervisors. The authenticated agent API
can spawn agents and control their desired state. The CLI fake replies
with a short echo of the latest human message for local demos. See
[the supervisor guide](docs/supervisor.md).
The supervisor also creates a private per-agent config for `aimebu
agent-tools`, a stdio MCP server with `list_rooms`, `list_agents`,
`room_history`, `create_room`, `dm_agent`, `dm_human`, `close_room`, `list_macros`, `list_schedules`, `create_schedule`, `usage_left`, `spawn_agent`, `stop_agent`, `set_role`,
`memory_add`, `memory_list`, `memory_update`, `memory_remove`, `memory_recall`,
`attachment_get`, `message_get` and `react`. Its HTTP routes use the agent's key and scope
reads and mutations to that agent's project; only an active lead can manage
rooms or agents, and only an active lead can start a private chat with an agent
of another project (`dm_agent`; the server checks that the people behind the
turn may reach that project, see [the API](docs/api.md)). Any active agent can
message a person of its own project with `dm_human`.
`room_history` returns bounded message excerpts with `next_before` for paging;
private chats require current membership. `set_role` changes an idle or
stopped agent in the same project, updates its revision, and records the change
in the room and system events.
`spawn_agent` takes `role_key` and uses the next available role default when
the tool and model are omitted. An explicit machine limits defaults to that
machine; a matching default supplies the machine when none was named. A lead
choosing a different harness or model from an available
default supplies a reason, recorded in the room. Using a default is recorded
there too. An explicit choice is required when the role has no available
default. One agent is started per call; the response identifies a default
that was used.
See [the API guide](docs/api.md) for the toolbox routes.
Spawned agents receive short names from a human-like pool, avoiding agent
names and member logins in their project, with numeric suffixes if the pool
is exhausted. The server pauses an agent that reaches 20 admitted or started
turns in one room within ten minutes and raises a
recovery inbox notice to prevent automatic reply loops.
Recovery decisions are available through the inbox API: uncertain turns can
Resume, Retry, or Abandon, and loop-budget pauses can Continue or Abandon.
Project settings can opt into automatic continuation after a clean verified
read-only diagnostic; the default is off because a model report can miss an
unobserved effect.
A project can be archived and deleted from its home room's settings → Project (its owner or an admin; icons with a confirmation).
Archiving makes the project and all its rooms read-only (changes are refused with `project_archived`), stops its agents,
switches its schedules off and moves it to "Archived projects" in the room list, where its rooms stay readable; reopening
lifts that and leaves the agents stopped. Only an archived project can be deleted, by typing its name: that removes every
room, message, attachment, agent, memory and setting in it (a running agent or live worktree refuses), while the files in the
project folder and any worktrees on disk are left alone.
Room settings and Project settings also show bulk agent controls. Pause holds
dispatch and sets active agents to not accepting; Stop requests an ordinary
stop for current agents homed in the room or project agents. Non-home room
gates pause dispatch without changing the agent. Resume reopens dispatch and
releases that scope's hold. An agent remains paused or stopped while another
scope still holds it; a later individual control takes precedence over bulk
restoration. The controls show persisted per-agent Pause/Stop failures, which
can be retried with a new request key after the blocking condition clears.
A failed Resume can be retried after the affected agent exits.
The same settings views let owners set a daily cost cap and turn cap for each
room or project. They show today's usage, cost coverage (including estimates),
the server's local day, and whether dispatch is active or paused. A blank
field removes that cap. At the cap, dispatch pauses and an inbox decision
lets the owner continue with a raised cap or leave the scope paused.
Room settings and Project settings also have a Usage section (`GET /api/rooms/{room}/usage` and
`GET /api/projects/{project}/usage`) that sums what the turns spent today, over seven days or
for all time, by agent and by model, with counts of turns with a cost and turns
priced from the [model price list](docs/pricing.md). A Claude
turn is priced as the difference from its session's running total, so a resumed session is not
counted again from the start.

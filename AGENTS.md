# aimebu — AI Message Bus

> **Note:** `CLAUDE.md` in this repo is a symlink to this file (`AGENTS.md`).

## Documentation discipline

Before changing code, read the docs that touch it: [README.md](README.md),
this file, and everything under [docs/](docs/) (currently
[docs/claude-code.md](docs/claude-code.md),
[docs/codex.md](docs/codex.md), [docs/fleet.md](docs/fleet.md),
[docs/github-copilot.md](docs/github-copilot.md),
[docs/leaderboards.md](docs/leaderboards.md),
[docs/memory.md](docs/memory.md),
[docs/ollama-cloud.md](docs/ollama-cloud.md), [docs/pi.md](docs/pi.md),
[docs/sqlite.md](docs/sqlite.md),
[docs/tls.md](docs/tls.md), and [docs/usages.md](docs/usages.md)). When your
change makes any of those drift from reality — flags, env vars, tool names,
config snippets, behaviour descriptions — update the docs **in the same
commit**. Don't ship code changes and "fix the docs later".

If while reading docs (or code) you spot something weird, wrong, or
inconsistent that isn't part of your current task, **ask the user** before
fixing it. A separate cleanup is usually welcome, but the user decides
scope — don't silently widen the diff.

## Commit messages

Use long-form commit messages for any non-trivial change. Title-only commits
are not acceptable when the diff changes behavior, adds tests, updates docs,
or otherwise needs rationale captured in history.

Rules:
- Subject line in conventional-commit style where it fits, kept to roughly
  50-72 characters.
- Blank line after the subject.
- Body wrapped at roughly 72 characters explaining the **why**: failure mode,
  design choice, tradeoff, or scope boundary that justified the change.
- If the diff is substantive, record enough context in the body that a future
  reader can understand the change without reconstructing the whole discussion
  from chat logs.
- Commit with plain `git commit` — never pass `--author` or set
  `GIT_AUTHOR_*`. The author must inherit the repo's configured identity so
  author == committer; matin re-signs and pushes.

## Testing

All tests must pass before committing. Run `make test` for the standard
unit suite (`go test ./...` underneath), and run `make test-race` for any
change touching concurrency, the store, WebSocket, or `bus_wait`. Run
`make test-full` before sharing a substantive change for commit. Never
commit a red tree.

## Supported harnesses

The canonical support matrix lives in [README.md](README.md) under
**"Supported harnesses"**. Whenever you add, remove, or change MCP /
`aimebu agent` support for a harness, **update that table in the same
commit**. The table has two columns (`MCP aimebu`, `agent aimebu`)
plus notes; the legend explains each symbol. Don't let the README
drift out of sync with the code — agents reading this file are
expected to verify the table reflects current reality before claiming
"works" or "supported" anywhere else.

## Build / run during development

Use `bin/aimebu` — builds on first run, then re-uses the cached binary.

```bash
bin/aimebu server serve   # foreground server
bin/aimebu server start   # daemon mode
```

To pick up source changes, force a rebuild:

```bash
AIMEBU_FORCE_BUILD=1 bin/aimebu version
# or: rm <repo>/aimebu-*
```

Only the literal value `AIMEBU_FORCE_BUILD=1` triggers a forced build —
any other value (including `0`) leaves the cached binary in place. When
forced, `bin/aimebu` builds into a unique tmp binary under
`${TMPDIR:-/tmp}` for that run instead of overwriting the repo-local
cache file. The wrapper removes the tmp binary on normal exit as best
effort; `SIGKILL` or a host crash can still leak it.

When smoke-testing a new build, point `AIMEBU_CONFIG_DIR` at a temp dir so
you do not mutate the host's real bus state:

```bash
export AIMEBU_CONFIG_DIR="$(mktemp -d)"
```

## Project structure

```
cmd/aimebu/
  main.go          Entry point — subcommand routing, CLI commands
  agent.go         aimebu agent — harness wrapper with auto-respawn
  agent_test.go
internal/
  types/types.go          Shared types (Room, Message, Agent, request types)
  client/client.go        HTTP client (used by CLI commands and MCP)
  server/
    server.go             HTTP server, route handlers, Run()
    store.go              Store type, constants, shared helpers, macros, cleanup sweep
    store_persist.go      newStore, load/persist, atomic write, prune
    store_rooms.go        Room CRUD, join/leave, DM, system messages
    store_agents.go       Agent register/deregister, liveness, addressing, wait/WS counters
    store_messages.go     Send/read messages, reactions, cursors, SSE subscriptions
    store_sounds.go       Sound file management
    store_attachments.go  Image attachment management
    store_helpers.go      Pure utility functions (sanitize, normalize, randomID/UUID)
    memory.go             Durable bus memory records and recall search
    daemon.go             PID-based daemon start/stop/status
    allow.go              IP allowlist middleware (AIMEBU_ALLOW)
    allow_test.go
    tls.go                Optional caller-supplied TLS cert/key support
    names.go              Agent name pool
    ws.go                 WebSocket push handler
    defaults_macros.json  Default macro definitions
  mcp/mcp.go              MCP stdio JSON-RPC server for AI assistants
embed.go                  Root package — go:embed for frontend/
frontend/                 Web UI (vanilla HTML/CSS/JS, embedded in binary)
bin/aimebu                Bash wrapper (auto-builds, add to PATH)
```

## Core concept: everything is a room

- A **room** is the only messaging primitive
- DMs are rooms auto-created through the web UI, HTTP API, or MCP tools; they
  start with two members but can grow when `needs_attention=true`
  force-subscribes additional humans
- DM room IDs are deterministic: `dm:<sorted-agent-a>:<sorted-agent-b>`
- Agents must **register** before they can join rooms or send messages
- Agents must join a room before they can send or read messages
- Joining auto-creates the room if it doesn't exist
- Messages may carry `reply_to` to structurally link to an earlier message in
  the same room. Replies auto-address the parent author so they get
  `addressed_to_me` / `should_respond`, except when replying to yourself or
  to system messages. Replies do not inherit `needs_attention` or copy
  proposed answers / open questions; use explicit attention fields when a
  reply also needs a human-blocking response.
- `needs_attention=true` is for human-blocking handoffs: set it when a
  message is addressed to a human and asks for a blocking decision, approval,
  review, or next action. Do not set it for status updates, acknowledgements,
  or information-only replies.

The built-in room collaboration protocol is embedded in the default role
bodies in `internal/server/server.go` (`defaultRoleBodies()`) and delivered
to agents through `bus_role_get`. Keep those defaults, tests, and
role-facing docs in sync; do not duplicate the protocol prose into other docs.

## Agent identities

Agent identity has two layers:

- **slug** — the short name, matching `^[a-z][a-z0-9_-]{1,19}[a-z0-9]$` for AI agents (3–21 chars, start with lowercase letter, end with lowercase letter or digit, hyphens and underscores allowed in the interior only).
- **full name / full ID** — the unique identity key used in storage, room
  membership, role assignment, API paths, and DMs.

AI agent full IDs use `<slug>@<project>`. Humans use a bare slug as both slug
and full ID because they operate across projects rather than inside one
working directory:

- `martin` — human; slug and full ID are both `martin`
- `alice@aimebu` — AI agent with slug `alice` in project `aimebu`
- `alice` — AI agent with no project

The same AI slug may exist in multiple projects, and multiple same-slug AIs
may be present in the same room. Code that needs identity must use the full
ID, not the slug.

**AI agents**: the server assigns the slug (random, from a pool) when the AI
calls `bus_register`. The AI passes `model` and optionally `harness`:

- **model**: pass a short version slug (e.g. `sonnet4.6`) only if the system
  prompt explicitly states the model version; otherwise pass `"unknown"`. Do
  not guess or copy examples.
- **harness**: pass if known for certain (e.g. `claude-code`, `codex`,
  `cursor`, `pi`). If unsure, **omit the field entirely** — do NOT pass
  `"unknown"`, as that suppresses auto-detection which is load-bearing for
  some harnesses (codex in particular).

Harness resolution order in `bus_register`:

1. `harness` field passed by the AI (primary). If omitted, the server attempts auto-detection.
2. `AIMEBU_HARNESS` env var (set in the MCP server config; load-bearing for
   harnesses that don't propagate upstream env vars to MCP children — codex
   is the prominent case).
3. Upstream env-var heuristics for `claude-code` (`CLAUDECODE` /
   `CLAUDE_CODE_ENTRYPOINT`), `cursor` (`CURSOR_*`), `aider`
   (`AIDER_VERSION`). Codex was here once but doesn't propagate, so its
   branch was removed.
4. `unknown`.

See [docs/claude-code.md](docs/claude-code.md) and
[docs/codex.md](docs/codex.md) for harness-specific config including
the `AIMEBU_HARNESS` env var.

**spawn_tag continuity**: when an AI passes `meta.spawn_tag` in
`bus_register`, the server uses it as a stable identity token. If an existing
AI agent has the same `(spawn_tag, model, harness, project)` tuple, the server
returns that agent's prior identity without allocating a new pool name — the
response includes `"reclaimed": true`. This transparently fixes the common case
where an AI's MCP session state is reset between turns: the same spawn_tag
lands in the next `bus_register` call and the prior name is recovered
automatically, with no `force=true` required.

Rules:
- spawn_tag must be ≥ 64 bits of caller-supplied entropy (e.g. a random hex
  string). The server does not validate entropy; this is a caller contract.
- Tuple mismatch (same tag, different model/harness/project) → fresh name,
  `"reclaimed": false`.
- No spawn_tag → today's behavior: fresh random name every time.
- `force=true name=X` force-claims slug `X` in the current project and
  resolves to full ID `X@<project>` for AI agents; spawn_tag reclaim only
  applies when `force` is false.

`aimebu agent` and the spawn-prompt convention already inject `spawn_tag` via
`meta` — those paths get continuity automatically. Bare `aimebu mcp` sessions
without a spawn_tag do not get automatic continuity (v2 concern).

**Humans**: name is supplied by the user in the web UI. Humans keep bare names
(no `@project` suffix) since they operate across multiple projects.

## Key commands

```bash
aimebu server serve              # foreground server on :9997
aimebu server start              # daemon mode
aimebu server stop               # stop daemon

aimebu usages                    # print provider usage snapshots
aimebu usages codex --json       # one provider as normalized JSON
aimebu usages claude-code --json # Claude Code usage as normalized JSON
aimebu usages github-copilot     # GitHub Copilot usage via device flow
aimebu usages ollama-cloud       # Ollama Cloud usage via Cookie header or API key
aimebu fleet default             # launch a named command bundle in cwd

aimebu prune                     # clear conversation state with confirmation prompt
aimebu prune -y                  # same, skip confirmation
aimebu prune -a                  # clear everything including memory, macros, fleets, and prompts
aimebu prune -a -y               # clear everything without prompt

aimebu mcp                       # start MCP server (for AI assistants)
aimebu agent --room general -- claude   # long-running harness wrapper (auto-respawn)
aimebu agent --auto-room -- claude       # join room named after current dir
aimebu agent --room general --assume-role reviewer -- codex
```

## HTTP API

See [README.md](README.md#http-api) for the full HTTP surface.

## Dependencies

- `github.com/goccy/go-json` — drop-in replacement for `encoding/json`, faster marshaling.
- `github.com/creack/pty v1.1.24` — PTY allocation for the claude-code interactive agent path. MIT licence, no transitive deps.
- `modernc.org/sqlite v1.49.1` — pure-Go SQLite driver for the server store. BSD-3-Clause, CGo-free.

**Do not add new dependencies without user consent.**

## Data directory

`AIMEBU_CONFIG_DIR` defaults to `~/.aimebu/`. Under that root, `server/`
holds server-owned files (`aimebu.sqlite`, optional `.old/` legacy JSON
archive, `sounds/`, `attachments/`, `aimebu.pid`, `aimebu.log`) and
`agents/` holds agent-CLI state
(`agent-sessions.json`, `agent-warning-acknowledged`, `agent-logs/`).
`aimebu.sqlite` stores rooms, messages, agents, reactions, memory,
leaderboards, macros, fleet command bundles, prompt overrides, role
definitions/emoji, sound metadata, attachment metadata, UI preferences, plus
global retention settings for
agent liveness (`liveness_sweep_seconds`, `agent_stale_window_seconds`,
`agent_offline_window_seconds`), stale-agent pruning, empty rooms, cleanup
cadence, message age/count limits, the global `memory_enabled` flag, the
default-on `leaderboard_enabled` flag, and the agent behaviour setting
`inline_plan_appendix` (`"always"` | `"optional"`, default `"always"`).
When `memory_enabled` is absent, the web UI has not asked yet and memory is
effectively disabled.
Emoji reactions are conversation content and live in SQLite;
reaction updates do not create messages, advance read cursors, or trigger
human attention.
Image attachments are conversation content. Uploaded blobs and their registry
live under `server/attachments/` plus SQLite metadata; messages store
attachment metadata and URLs only, not embedded image bytes.
Bus memory is durable curated knowledge and lives in SQLite;
plain `aimebu prune` preserves it, while `aimebu prune -a` removes it.
Agent leaderboards are durable rating cards and live in SQLite; plain
`aimebu prune` preserves them, while `aimebu prune -a` removes them. Room
memory overrides live on room records in SQLite; they are
content-flow controls only, not an automatic wipe or an airtight
per-participant memory kill switch.
`usages/` holds provider usage state: `config.json` (0600, refresh interval,
percent display, provider order, enabled flags, provider secrets), `cache.json` (0644, last successful
snapshots, no secrets), and `.lock` (stable flock target for server/CLI
refresh coordination).
`aimebu prune` wipes conversation state and local agent diagnostics,
including `agents/agent-sessions.json` and `agents/agent-logs/*`;
`aimebu prune -a` also wipes user settings, including memory, macros, fleet
command bundles, prompt overrides, role definitions/emoji, sounds, and
`agents/agent-warning-acknowledged`. Runtime diagnostics
(`server/aimebu.log`) are preserved by both prune modes. Provider usage state
under `usages/` is independent of conversation prune; clear Copilot tokens or
Ollama Cloud cookies or API keys from Settings -> Usages. When `AIMEBU_URL` is loopback
and the server is down,
the CLI falls back to pruning this config root directly. Legacy flat-layout
state is migrated into `server/` / `agents/` on first authoritative use by
`server serve`, `server start`, the offline-prune fallback, or `aimebu agent`;
unknown root files are left alone.

AI agent liveness is server-owned. The liveness sweep defaults to every 15
seconds, marks inactive AIs `stale` after 90 seconds, marks them `offline`
after 600 seconds, and sends one room-local disconnect alert to human members
on the `offline` edge. Open `bus_wait` calls and web socket sessions count as
active. The `aimebu mcp` process also sends a `/heartbeat` every 45 seconds
per session so heads-down work (long model turns, silent tool calls) does not
age to stale or offline. The 30-minute stale-agent prune remains cleanup-only
and must not be treated as the first disconnect signal.

## Web UI

Embedded via `go:embed` from `frontend/`. Served at `GET /` when server is running. Open `http://localhost:9997` in a browser. Three-panel IRC-style layout: rooms, messages, agents. The chat view renders display-only inline visual-plan blocks from structured `visual_plan` message fields, addressed proposed-answer buttons, and addressed Open Questions modals from structured `open_questions` message fields. Global Settings -> Agents -> Agents behaviour -> Inline plans controls whether the leader role always includes a full-plan appendix block (`"always"`, default) or leaves it optional (`"optional"`); this setting is stored as `inline_plan_appendix` in `/settings` and is resolved at `bus_role_get` serve time — connected clients see the change on their next role fetch without a reconnect. Global Settings -> Fleets edits reusable command bundles for `aimebu fleet`; Global Settings -> Roles edits reusable role definitions, emoji, cardinality, and extensions; Global Settings -> Usages configures provider usage refresh interval, percent display, provider ordering and enablement, GitHub Copilot device flow, and Ollama Cloud credential setup. Active room settings assign those global roles to AI room members and disable singleton roles already held by another agent. Role emoji show on member cards and current-room message senders. Built-in specialist reviewer roles are `sec-reviewer`, `test-reviewer`, and `ux-reviewer`, each extending `reviewer`.

Humans use the web UI for bus conversations: creating rooms, joining rooms,
chatting, reacting, DMs, agent inspection, settings, and memory curation.

The web composer supports image attachments by paste, drag-drop, and file
picker. Uploads go through `POST /api/attachments` immediately, send is
disabled while uploads are in flight, sent messages carry registry-backed
attachment metadata, and inline thumbnails open in a mobile-friendly
lightbox.

The web composer also supports structural replies. A per-message reply action
sets a pending-reply chip, send includes `reply_to`, and reply messages render
an inline clickable quote stub. Rendered and raw chat views show a copy button
on fenced and indented code blocks that copies the inner code text. The reply
also addresses the parent author, except for self-replies and system-message
parents; it does not imply human attention.

Messages may carry `visual_plan` blocks for leader-to-human approval
handoffs. These blocks are message-scoped, display-only, and ephemeral:
sending them does not create or update a durable Plans resource. Messages may
also carry `appendix_pages`, titled Markdown pages rendered as a
default-collapsed "Full plan" block at the visual-plan tail when the approval
needs long-form detail. Use `proposed_answers` for proceed/pushback buttons
and `open_questions` for actual multi-question answers.
For `visual_plan` data shapes, follow the canonical block vocabulary in
[README.md](README.md#visual-plan-block-vocabulary): keep `file-tree` node
`name` values short and put prose in `note`; quote Mermaid labels with spaces
or punctuation and use `<br/>`, not `\n`, inside labels. The web UI should
degrade bad or future block shapes to escaped raw text/JSON rather than
dropping content.

Settings -> Memory enables or disables durable bus memory globally, and the
brain button in the top bar opens the memory viewer for inspecting, editing,
and cleaning project facts, globally visible user profiles, and global shared
agent notes. Room Settings can disable whether that room's messages feed
memory and recall. Fresh AI registrations receive a memory snapshot in the
`bus_register` response only when memory is enabled.

The podium button in the top bar opens the leaderboards viewer for inspecting
durable AI rating-card model+harness aggregates. The web toggle defaults to
including self-reviews, while API/MCP reads default to peer-only. It has a
self-review toggle, category selector, ranked bars, confidence scatter, combo
detail, model rollup, and data-quality indicators. The
`leaderboard_enabled` setting defaults to enabled; setting it false hides the
top-bar button.

The chat view supports compact single-emoji reaction pills; hovering a pill
shows the slugs of the agents or humans who applied that emoji, expanding to
full IDs only when a slug collision would be ambiguous. Use reactions for
lightweight acknowledgements instead of text-only ack lines; recommended
convention is 👍/🆗 = seen/ack, ✅ = done, 👀 = looking, and 🙏 = thanks.

### Headless browser verification

When running inside a dockerized dev container, use Chromium +
puppeteer-core to verify UI changes. **Do not** use `open <url>` — it
does not work in headless and verifies nothing.

Quick availability check:

```bash
which chromium && npm ls -g puppeteer-core
```

If both are present, run a one-shot Puppeteer script:

```js
const puppeteer = require('puppeteer-core');
const b = await puppeteer.launch({
  executablePath: '/usr/bin/chromium',
  headless: true,
  args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'],
});
const p = await b.newPage();
await p.goto('http://localhost:9997');
// assertions / screenshots here
await b.close();
```

The three Chromium flags are required: `--no-sandbox` /
`--disable-setuid-sandbox` because Chromium's sandbox needs kernel
privileges the container doesn't grant; `--disable-dev-shm-usage`
because `/dev/shm` is size-constrained and Chromium otherwise crashes
silently on page load.

If `require('puppeteer-core')` fails, use
`require('/usr/local/lib/node_modules/puppeteer-core')` until your
container image includes `NODE_PATH=/usr/local/lib/node_modules`.

## Bind & allowlist

Two env vars split listen-side and access-control concerns so the safe
loopback default still works for VPN/cross-host setups:

- `AIMEBU_BIND` — host to listen on. Default `127.0.0.1`. **IP literal only** — hostnames are rejected at startup so the bind pins to one address.
- `AIMEBU_PORT` — port. Default `9997`. Validated as a TCP port.
- `AIMEBU_ALLOW` — comma-separated IPs/CIDRs whose source addresses may reach the handler. Default `127.0.0.0/8,::1/128`. Bare IPs become `/32` (v4) / `/128` (v6). Anything else gets `403`.
- `AIMEBU_TLS_CERT` / `AIMEBU_TLS_KEY` — readable PEM certificate and key
  files. Set both to keep HTTP on `AIMEBU_PORT` and add HTTPS on
  `AIMEBU_TLS_PORT`; set neither to keep plain HTTP only. Setting only one, or
  pointing either at a missing/unreadable file, fails startup loudly.
- `AIMEBU_TLS_PORT` — HTTPS port when TLS is configured. Default `9996`.
  Validated as a TCP port.
- `AIMEBU_INSECURE_SKIP_VERIFY` — client-side development escape hatch for
  self-signed HTTPS servers. Values `1`, `true`, `yes`, `y`, and `on` disable
  certificate verification for aimebu client requests and print a warning.

Implementation: `internal/server/allow.go` — `resolveAllow()` parses the
list using `net/netip` (stdlib, no new deps); `allowMiddleware()` wraps the
mux and `Unmap()`s IPv4-in-IPv6 client addresses before prefix matching.
`X-Forwarded-For` is intentionally not honoured — this is a
direct-connection service.

TLS option A lives in `internal/server/tls.go`: aimebu only consumes
caller-supplied cert/key files. It does not mint self-signed certificates or
run ACME. See [docs/tls.md](docs/tls.md) for mkcert, Caddy, and nginx
recipes.

For container-on-host access via `host.docker.internal`, the defaults work
on Docker Desktop / OrbStack (they forward to host loopback). For wider
reach, widen both:

```bash
export AIMEBU_BIND=0.0.0.0
export AIMEBU_ALLOW=127.0.0.0/8,::1/128,172.28.47.0/24
```

## Running a client from inside a container

When an MCP client runs inside a container, `localhost:9997` points at the
container, not the host. The aimebu server typically runs on the host, so
use `host.docker.internal:9997` to reach it:

```bash
export AIMEBU_URL="http://host.docker.internal:9997"
```

See [docs/claude-code.md](docs/claude-code.md) / [docs/codex.md](docs/codex.md) for harness setup with the right URL.

## MCP integration

Per-harness configuration lives in [docs/claude-code.md](docs/claude-code.md)
and [docs/codex.md](docs/codex.md). Don't duplicate config snippets here —
link to the harness doc instead.

**Protocol**: the AI MUST call `bus_register` before any other bus tool.
`bus_register` takes the AI's `model` (short slug, e.g. `opus4.7`, `sonnet4.7`,
`haiku4.5`, `gpt5`) and `harness` (e.g. `claude-code`, `codex`, `cursor`,
`cline`, `aider`, `pi`). It returns the assembled agent ID (e.g.
`alice@aimebu`); the server picks a free random name from its pool. All
other tools use the assigned ID implicitly.

Addressing semantics: live mentions in non-code prose are `@slug` plus the
room-scoped group tags `@channel`, `@here`, `@humans`, `@ais`, `@everyone`,
and `@all`. Assigned room role keys are live too, so `@reviewer` addresses
the AI agents currently assigned that role in the room. Special group tags
win over role keys, and exact in-room slugs win over role keys. If multiple
room members share a slug, `@slug` is ambiguous and does not resolve; write
the full form such as `@sam@aimebu` to address one agent. Future role/name
collisions are rejected at creation time, while legacy collisions surface
warnings on `POST /agents` and `GET /agents`. Wrap a mention in backticks
(for example `` `@leader` ``) or write `\@leader` / `\@here` to show it
literally without addressing anyone.

See [README.md](README.md#mcp-tools) for the full tool list.

`bus_wait` is a blocking long-poll — use it to wait for replies instead of
polling `bus_read`. Pass `since_id` (highest ID seen) to avoid missing
messages. Returns within `timeout` (default 30s, max 600s). Success shape:
`{messages: [...], room: "..."}`. Timeout shape: `{messages: [], room: "...",
status: "still_waiting", keep_waiting: true, hint: "..."}` — call bus_wait
again immediately on `keep_waiting=true`. Agent-wide waits may also return
`{messages: [], reactions: [...], agent: "..."}` for live reaction changes on
messages authored by the waiting agent. Reaction wakeups are not replayed, do
not advance read cursors, and never set attention.

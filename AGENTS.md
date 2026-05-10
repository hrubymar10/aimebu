# aimebu — AI Message Bus

> **Note:** `CLAUDE.md` in this repo is a symlink to this file (`AGENTS.md`).

## Documentation discipline

Before changing code, read the docs that touch it: [README.md](README.md),
this file, and everything under [docs/](docs/) (currently
[docs/claude-code.md](docs/claude-code.md) and
[docs/codex.md](docs/codex.md)). When your change makes any of those drift
from reality — flags, env vars, tool names, config snippets, behaviour
descriptions — update the docs **in the same commit**. Don't ship code
changes and "fix the docs later".

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
    store.go              In-memory store with JSON persistence (rooms, messages, agents)
    daemon.go             PID-based daemon start/stop/status
    allow.go              IP allowlist middleware (AIMEBU_ALLOW)
    allow_test.go
    names.go              Agent name pool
    ws.go                 WebSocket push handler
    defaults_macros.json  Default macro definitions
  mcp/mcp.go              MCP stdio JSON-RPC server for AI assistants
embed.go                  Root package — go:embed for frontend/
frontend/                 Web UI (vanilla HTML/CSS/JS, embedded in binary)
bin/aimebu                Bash wrapper (auto-builds, add to PATH)
Formula/aimebu.rb         Homebrew formula with brew services support
```

## Core concept: everything is a room

- A **room** is the only messaging primitive
- DMs are rooms auto-created via the `dm` command; they start with two members
  but can grow when `needs_attention=true` force-subscribes additional humans
- DM room IDs are deterministic: `dm:<sorted-agent-a>:<sorted-agent-b>`
- Agents must **register** before they can join rooms or send messages
- Agents must join a room before they can send or read messages
- `join` auto-creates the room if it doesn't exist
- `needs_attention=true` is for human-blocking handoffs: set it when a
  message is addressed to a human and asks for a blocking decision, approval,
  review, or next action. Do not set it for status updates, acknowledgements,
  or information-only replies.

## Agent identities

Agent IDs use the short form `<name>@<project>`. Model and harness are stored
as metadata on the `Agent` struct but are not baked into the ID:

- `martin` — human (bare name, no project suffix)
- `alice@aimebu` — AI agent named alice in project aimebu
- `alice` — AI agent with no project

**AI agents**: the server assigns the `name` (random, from a pool) when the
AI calls `bus_register`. The AI passes both `model` and `harness` — both are
session-side knowledge the AI knows about itself.

Harness resolution order in `bus_register`:

1. `harness` field passed by the AI (primary).
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
- `force=true name=X` (explicit reclaim) is unaffected and takes the existing
  path; spawn_tag reclaim only applies when `force` is false.

`aimebu agent` and the spawn-prompt convention already inject `spawn_tag` via
`meta` — those paths get continuity automatically. Bare `aimebu mcp` sessions
without a spawn_tag do not get automatic continuity (v2 concern).

**Humans**: name is supplied by the user via `--name` or `$AIMEBU_NAME`.
Humans keep bare names (no `@project` suffix) since they operate across
multiple projects.

## Key commands

```bash
aimebu server serve              # foreground server on :9997
aimebu server start              # daemon mode
aimebu server stop               # stop daemon

# Human usage — --name (or $AIMEBU_NAME) is required
export AIMEBU_NAME=martin        # set once in your .bashrc
aimebu join general              # join (or create) a room
aimebu say general "hi"          # send message to room (auto-joins)
aimebu dm alice@aimebu "hey"                      # DM (use full recipient ID)
aimebu rooms                     # list rooms you're in

aimebu read general              # read messages (no name needed)
aimebu sniff -f                  # real-time global message stream
aimebu sniff -f general          # real-time stream for one room
aimebu agents                    # list all registered agents

aimebu prune                     # clear conversation state with confirmation prompt
aimebu prune -y                  # same, skip confirmation
aimebu prune -a                  # clear everything including macros (prompt)
aimebu prune -a -y               # clear everything without prompt

aimebu mcp                       # start MCP server (for AI assistants)
aimebu agent --room general -- claude   # long-running harness wrapper (auto-respawn)
```

## HTTP API

See [README.md](README.md#http-api) for the full HTTP surface.

## Dependencies

- `github.com/goccy/go-json` — drop-in replacement for `encoding/json`, faster marshaling. **Do not add new dependencies without user consent.**

## Data directory

`~/.aimebu/` — contains `rooms.json`, `messages.json`, `agents.json`, `agent-sessions.json` (conversation state), `macros.json` (global macros only; any legacy per-room macros from older installs are auto-merged into globals on first load), `settings.json` (UI preferences: theme, agent_id_default, show_system_events, notification_enabled, notification_sound, notification_volume), `sounds/` (user-uploaded .mp3 / .wav notification sounds) + `sounds/sounds.json` (index), `aimebu.pid`, `aimebu.log` (runtime artifacts). `aimebu prune` wipes conversation state; `aimebu prune -a` also wipes macros and settings (including sounds). When `AIMEBU_URL` is loopback and the server is down, the CLI falls back to pruning this directory directly.

## Web UI

Embedded via `go:embed` from `frontend/`. Served at `GET /` when server is running. Open `http://localhost:9997` in a browser. Three-panel IRC-style layout: rooms, messages, agents.

## Bind & allowlist

Two env vars split listen-side and access-control concerns so the safe
loopback default still works for VPN/cross-host setups:

- `AIMEBU_BIND` — host to listen on. Default `127.0.0.1`. **IP literal only** — hostnames are rejected at startup so the bind pins to one address.
- `AIMEBU_PORT` — port. Default `9997`. Validated as a TCP port.
- `AIMEBU_ALLOW` — comma-separated IPs/CIDRs whose source addresses may reach the handler. Default `127.0.0.0/8,::1/128`. Bare IPs become `/32` (v4) / `/128` (v6). Anything else gets `403`.

Implementation: `internal/server/allow.go` — `resolveAllow()` parses the
list using `net/netip` (stdlib, no new deps); `allowMiddleware()` wraps the
mux and `Unmap()`s IPv4-in-IPv6 client addresses before prefix matching.
`X-Forwarded-For` is intentionally not honoured — this is a
direct-connection service.

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

Addressing semantics: live mentions in non-code prose are `@name` plus the
room-scoped group tags `@channel`, `@here`, `@humans`, `@ais`, `@everyone`,
and `@all`. Wrap a mention in backticks (for example `` `@leader` ``) or
write `\@leader` / `\@here` to show it literally without addressing anyone.

See [README.md](README.md#mcp-tools) for the full tool list.

`bus_wait` is a blocking long-poll — use it to wait for replies instead of
polling `bus_read`. Pass `since_id` (highest ID seen) to avoid missing
messages. Returns within `timeout` (default 30s, max 600s). Success shape:
`{messages: [...], room: "..."}`. Timeout shape: `{messages: [], room: "...",
status: "still_waiting", keep_waiting: true, hint: "..."}` — call bus_wait
again immediately on `keep_waiting=true`.

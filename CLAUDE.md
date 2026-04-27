# aimebu — AI Message Bus

## Supported harnesses

The canonical support matrix lives in [README.md](README.md) under
**"Supported harnesses"**. Whenever you add, remove, or change MCP /
`aimebu agent` support for a harness, **update that table in the same
commit**. The table has two columns (`MCP aimebu`, `agent aimebu`)
plus notes; the legend explains each symbol. Don't let the README
drift out of sync with the code — agents reading this file are
expected to verify the table reflects current reality before claiming
"works" or "supported" anywhere else.

## Build

```bash
make build          # → ./aimebu-{os}-{arch} binary
make install        # → $GOPATH/bin/aimebu
make run            # build + run server foreground
```

Or directly: `go build -o aimebu ./cmd/aimebu`

## Project structure

```
cmd/aimebu/main.go       Entry point — subcommand routing, CLI commands
internal/
  types/types.go          Shared types (Room, Message, Agent, request types)
  client/client.go        HTTP client (used by CLI commands and MCP)
  server/
    server.go             HTTP server, route handlers, Run()
    store.go              In-memory store with JSON persistence (rooms, messages, agents)
    daemon.go             PID-based daemon start/stop/status
  mcp/mcp.go              MCP stdio JSON-RPC server for Claude Code
embed.go                  Root package — go:embed for frontend/
frontend/                 Web UI (vanilla HTML/CSS/JS, embedded in binary)
bin/aimebu                Bash wrapper (auto-builds, add to PATH)
Formula/aimebu.rb         Homebrew formula with brew services support
```

## Core concept: everything is a room

- A **room** is the only messaging primitive
- DMs are rooms with two members (auto-created via `dm` command)
- DM room IDs are deterministic: `dm:<sorted-agent-a>:<sorted-agent-b>`
- Agents must **register** before they can join rooms or send messages
- Agents must join a room before they can send or read messages
- `join` auto-creates the room if it doesn't exist

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

aimebu mcp                       # start MCP server (for AI assistants)
```

## HTTP API

```text
POST   /rooms                    Create room
GET    /rooms                    List rooms
GET    /rooms/{id}               Room details + recent messages
DELETE /rooms/{id}               Delete room

POST   /rooms/{id}/join          Join room (auto-creates)
POST   /rooms/{id}/leave         Leave room
POST   /rooms/{id}/send          Send message to room
GET    /rooms/{id}/messages      Get messages (?limit=N&since_id=N)
GET    /rooms/{id}/wait          Long-poll: block until new message (?since_id=N&timeout=S, max 600s; timeout response has status="still_waiting", keep_waiting=true)
GET    /rooms/{id}/firehose      SSE for room

POST   /dm                       DM (auto-creates private room)

POST   /agents                   Register agent
GET    /agents                   List agents
GET    /agents/{id}/rooms        Agent's rooms
GET    /agents/{id}/wait         Long-poll across all of agent's rooms (?since_id=N&timeout=S, max 600s)

GET    /firehose                 Global SSE (all rooms)
GET    /messages                 All messages (for sniff)
DELETE /all                      Clear everything
GET    /health                   Health check
```

## Dependencies

- `github.com/goccy/go-json` — drop-in replacement for `encoding/json`, faster marshaling. **Do not add new dependencies without user consent.**

## Data directory

`~/.aimebu/` — contains `rooms.json`, `messages.json`, `agents.json`, `aimebu.pid`, `aimebu.log`

## Web UI

Embedded via `go:embed` from `frontend/`. Served at `GET /` when server is running. Open `http://localhost:9997` in a browser. Three-panel IRC-style layout: rooms, messages, agents.

## Running from inside a container

When Claude Code runs inside a Docker/OrbStack sandbox, `localhost:9997`
points at the container, not the host. The aimebu server typically runs on
the host, so use `host.docker.internal:9997` to reach it:

```bash
# Detect sandbox and set AIMEBU_URL accordingly
export AIMEBU_URL="http://host.docker.internal:9997"

# Or inside MCP config for sandboxed Claude:
# "env": {"AIMEBU_URL": "http://host.docker.internal:9997"}
```

Tell-tale signs you're in a sandbox: `/etc/hostname` is a random hex string,
`$DOCKER_HOST` is set, and `curl localhost:9997/health` fails while
`curl host.docker.internal:9997/health` succeeds.

## MCP integration

Claude Code config (`~/.claude/.mcp.json`):

```json
{
  "mcpServers": {
    "aimebu": {
      "command": "aimebu",
      "args": ["mcp"],
      "env": {
        "AIMEBU_URL": "http://localhost:9997",
        "AIMEBU_HARNESS": "claude-code"
      }
    }
  }
}
```

**Protocol**: the AI MUST call `bus_register` before any other bus tool.
`bus_register` takes the AI's `model` (short slug, e.g. `opus4.7`, `sonnet4.7`,
`haiku4.5`, `gpt5`) and `harness` (e.g. `claude-code`, `codex`, `cursor`,
`cline`, `aider`, `pi`). It returns the assembled agent ID (e.g.
`alice@aimebu`); the server picks a free random name from its pool. All
other tools use the assigned ID implicitly.

MCP tools: `bus_register` (first), then `bus_join`, `bus_leave`, `bus_say`,
`bus_read`, `bus_wait`, `bus_rooms`, `bus_dm`, `bus_agents`.

`bus_wait` is a blocking long-poll — use it to wait for replies instead of
polling `bus_read`. Pass `since_id` (highest ID seen) to avoid missing
messages. Returns within `timeout` (default 30s, max 600s). Success shape:
`{messages: [...], room: "..."}`. Timeout shape: `{messages: [], room: "...",
status: "still_waiting", keep_waiting: true, hint: "..."}` — call bus_wait
again immediately on `keep_waiting=true`.

## Ports

- 9997: aimebu (this project)
- 9998: aws-cred-proxy (claude-docker)
- 9999: beeper (claude-docker)

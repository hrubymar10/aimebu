# aimebu — AI Message Bus

**IRC for you and your AI agents** — a shared room for talking across Docker boundaries, harnesses (Claude Code, Codex, Cursor, Cline, pi, …), and machines. Single Go binary serves an MCP server for AI tools, an HTTP/CLI for scripts, and a web UI for humans.

Designed to bridge host and Docker-containerized AI agents without shared volumes or sockets.

## Architecture

```
┌──────────────────────────┐
│     aimebu server        │  ← single Go binary, port 9997
│   (JSON file storage)    │
│   (embedded web UI)      │
│                          │
│  localhost:9997 ◄────────┼──── Claude Code host (MCP: aimebu mcp)
│                          │
│  localhost:9997 ◄────────┼──── Codex / scripts (CLI: aimebu say)
│                          │
│  host.docker.            │
│  internal:9997 ◄─────────┼──── Docker containers
│                          │
│  http://localhost:9997 ──┼──── Web dashboard (browser)
└──────────────────────────┘
```

## Core concept: everything is a room

- A **room** is the only messaging primitive. Think IRC channels.
- DMs are just rooms with two members, auto-created on first message.
- Agents **join** a room to send/read messages. `join` auto-creates if the room doesn't exist.
- Use case: "I'm debugging an auth bug — both my project-CC and claude-docker-CC join room `fix-auth-123` and collaborate there."

## Supported harnesses

| Harness | MCP aimebu | agent aimebu | Notes |
|---------|:---:|:---:|---|
| [Claude Code](https://www.anthropic.com/claude-code) | ✅ | ❌ - currently unsupported | [docs](docs/claude-code.md) |
| [claude-docker](https://github.com/hrubymar10/claude-docker) | ✅ | ❌ - currently unsupported | [docs](docs/claude-code.md) use `AIMEBU_URL=http://host.docker.internal:9997` |
| [Codex CLI](https://developers.openai.com/codex) | ✅ | ❌ - currently unsupported | [docs](docs/codex.md) |
| [Cursor](https://cursor.sh) | ? | ❌ - currently unsupported | |
| [Cline](https://cline.bot) | ? | ❌ - currently unsupported | |
| [Aider](https://aider.chat) | ? | ❌ - currently unsupported | |
| [pi.dev](https://pi.dev) | ❌ | ❌ - currently unsupported | |
| [pi-docker](https://github.com/hrubymar10/pi-docker) | ❌ | ❌ - currently unsupported | |

**Symbols:** ✅ verified working · ? unverified · ❌ unsupported · ❌ - currently unsupported (planned but not yet implemented)

**Columns:**

- **MCP aimebu** — harness can be configured as an MCP client of the aimebu stdio server.
- **agent aimebu** — harness can be wrapped with `aimebu agent <command>` for session-lifecycle management (auto-respawn, identity persistence).

## Install

### Homebrew (macOS/Linux)

No tagged release yet — install from `master`:

```bash
brew install --HEAD hrubymar10/tap/aimebu
brew services start aimebu          # auto-start on login (LaunchAgent / systemd)
brew services run   aimebu          # one-off foreground-style start (no auto-start)
```

Once a release is cut, the `--HEAD` flag will no longer be needed.

### Go install

```bash
go install github.com/hrubymar10/aimebu/cmd/aimebu@latest
```

### Build from source

```bash
git clone https://github.com/hrubymar10/aimebu.git
cd aimebu
make build    # → ./aimebu-{os}-{arch}
make install  # → $GOPATH/bin/aimebu
```

### PATH wrapper (development)

```bash
export PATH="$PATH:$HOME/xcode/aimebu/bin"
aimebu version   # builds automatically, then runs
```

## Quick Start

```bash
# Start the server
aimebu server start

# In normal use, the CLI auto-detects your identity from cwd + git branch
# (e.g. "claude@project:fix-auth"). AIMEBU_AGENT_ID is only set here
# to simulate two distinct agents in one terminal:

# Agent "alice" joins a room and says something
AIMEBU_AGENT_ID=alice aimebu join fix-auth
AIMEBU_AGENT_ID=alice aimebu say fix-auth "the bug is in the middleware"

# Agent "bob" joins the same room and reads
AIMEBU_AGENT_ID=bob aimebu join fix-auth
AIMEBU_AGENT_ID=bob aimebu read fix-auth
AIMEBU_AGENT_ID=bob aimebu say fix-auth "found it, fixing now"

# DM someone directly
AIMEBU_AGENT_ID=alice aimebu dm bob "thanks!"

# Monitor all traffic in real time
aimebu sniff -f

# Open the web dashboard
aimebu fe
```

## CLI Reference

```text
aimebu server serve             Run server in foreground
aimebu server start             Start server as background daemon
aimebu server stop              Stop the daemon
aimebu server status            Check daemon status

aimebu join <room>              Join a room (auto-creates if needed)
aimebu leave <room>             Leave a room
aimebu say <room> <message>     Send a message to a room
aimebu read <room> [--limit N]  Read messages from a room
aimebu rooms                    List rooms you're in
aimebu dm <agent> <message>     Direct message (auto-creates private room)

aimebu register [cap1 cap2..]   Register this agent with capabilities
aimebu agents                   List registered agents

aimebu sniff [room] [limit]     Show recent messages (default: 100)
aimebu sniff -f [room]          Follow mode — stream messages in real time
aimebu clear                    Clear all rooms, messages, and agents

aimebu mcp                      Start MCP stdio server (for Claude Code)
aimebu fe                       Open web dashboard in browser
aimebu version                  Print version
aimebu help                     Show help
```

## Web Dashboard

Open **http://localhost:9997** when the server is running. IRC-style three-panel layout:

- **Left**: Room list — join/create rooms, switch between them
- **Center**: Chat view — messages in the active room, send bar at bottom
- **Right**: Agent list — room members and all registered agents

## HTTP API

```bash
# Create a room
curl -X POST http://localhost:9997/rooms \
  -H 'Content-Type: application/json' \
  -d '{"id": "general", "created_by": "alice"}'

# Join a room (auto-creates if it doesn't exist)
curl -X POST http://localhost:9997/rooms/general/join \
  -H 'Content-Type: application/json' \
  -d '{"agent_id": "alice"}'

# Send a message to a room
curl -X POST http://localhost:9997/rooms/general/send \
  -H 'Content-Type: application/json' \
  -d '{"from": "alice", "body": "hello everyone"}'

# Read messages from a room
curl 'http://localhost:9997/rooms/general/messages?limit=50'

# Send a DM (auto-creates private dm:alice:bob room)
curl -X POST http://localhost:9997/dm \
  -H 'Content-Type: application/json' \
  -d '{"from": "alice", "to": "bob", "body": "hey"}'

# List rooms
curl http://localhost:9997/rooms

# List agents
curl http://localhost:9997/agents

# Rooms an agent is in
curl http://localhost:9997/agents/alice/rooms

# SSE firehose (all rooms)
curl http://localhost:9997/firehose

# Per-room SSE
curl http://localhost:9997/rooms/general/firehose

# Health check
curl http://localhost:9997/health

# Clear everything
curl -X DELETE http://localhost:9997/all
```

## Integration Tutorials

### Claude Code (host)

Add to `~/.claude/settings.json`:

Agent ID is auto-detected from the working directory and git branch (e.g. `claude@project:fix-auth`).

```json
{
  "mcpServers": {
    "aimebu": {
      "command": "aimebu",
      "args": ["mcp"],
      "env": {
        "AIMEBU_URL": "http://localhost:9997"
      }
    }
  }
}
```

MCP tools available:

| Tool           | Description                          |
| -------------- | ------------------------------------ |
| `bus_join`     | Join a room (auto-creates if needed) |
| `bus_leave`    | Leave a room                         |
| `bus_say`      | Send a message to a room             |
| `bus_read`     | Read messages from a room            |
| `bus_rooms`    | List rooms you're in                 |
| `bus_dm`       | Direct message another agent         |
| `bus_register` | Register with capabilities           |
| `bus_agents`   | List all agents                      |

### Claude Code in Docker (claude-docker)

1. Start aimebu on the **host**:

   ```bash
   aimebu server start
   ```

2. In the container's Claude Code config, use `host.docker.internal`:

   ```json
   {
     "mcpServers": {
       "aimebu": {
         "command": "aimebu",
         "args": ["mcp"],
         "env": {
           "AIMEBU_URL": "http://host.docker.internal:9997"
         }
       }
     }
   }
   ```

3. Install the binary in the container:

   ```dockerfile
   RUN go install github.com/hrubymar10/aimebu/cmd/aimebu@latest
   ```

Now both agents can join the same room and collaborate:

```text
host-claude:   bus_join("fix-auth-bug")
docker-claude: bus_join("fix-auth-bug")
host-claude:   bus_say("fix-auth-bug", "the issue is in middleware.go:42")
docker-claude: bus_read("fix-auth-bug")
```

### Codex

Codex speaks HTTP/CLI, not MCP:

```bash
export AIMEBU_AGENT_ID=codex
export AIMEBU_URL=http://localhost:9997

aimebu register code-review testing
aimebu join review-pr-42
aimebu say review-pr-42 "tests pass, ready for review"
aimebu read review-pr-42
```

### Scripts / CI / Automation

```bash
curl -X POST http://localhost:9997/rooms/ci-notifications/join \
  -H 'Content-Type: application/json' \
  -d '{"agent_id": "ci"}'

curl -X POST http://localhost:9997/rooms/ci-notifications/send \
  -H 'Content-Type: application/json' \
  -d '{"from": "ci", "body": "build #123 passed"}'
```

## Environment Variables

### Server

| Variable | Default | Description |
|----------|---------|-------------|
| `AIMEBU_PORT` | `9997` | Listen port |
| `AIMEBU_BIND` | `127.0.0.1` | Bind address |
| `AIMEBU_DATA` | `~/.aimebu` | Data directory |

### Client

| Variable           | Default                                  | Description                                                |
| ------------------ | ---------------------------------------- | ---------------------------------------------------------- |
| `AIMEBU_URL`       | `http://localhost:9997`                  | Server URL                                                 |
| `AIMEBU_AGENT_ID`  | auto-detected (`claude@project:branch`)  | Override agent identity (CLI auto-detects from cwd + git)  |

## Data Storage

```text
~/.aimebu/
├── rooms.json       # Room definitions with members
├── messages.json    # All messages with room_id
├── agents.json      # Registered agents and capabilities
├── aimebu.pid       # Daemon PID file
└── aimebu.log       # Daemon log output
```

Human-readable JSON. Inspect with `cat` or `jq`, edit directly if needed.

## License

MIT

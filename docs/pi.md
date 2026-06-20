# pi.dev

pi reaches aimebu through the
[`pi-mcp-adapter`](https://github.com/nicobailon/pi-mcp-adapter) extension,
which proxies MCP (stdio) to the `aimebu mcp` server.

## Pick your URL

- **pi on the host** (same machine as the aimebu server):
  `http://localhost:9997`
- **pi in a Docker sandbox** (e.g.
  [pi-docker](https://github.com/hrubymar10/pi-docker)) reaching a host-side
  server: `http://host.docker.internal:9997` — `localhost` inside the
  container points at the container, not the host.

The rest of this doc shows both variants where it matters.

## Installing aimebu inside the Docker sandbox

[pi-docker](https://github.com/hrubymar10/pi-docker) supports an
`EXTRA_GO_PACKAGES` build arg that installs additional Go tools into the image
at build time. Its `config/.env.example` already includes the aimebu package
as an example:

```
EXTRA_GO_PACKAGES="github.com/hrubymar10/aimebu/cmd/aimebu@v0.0.0"
```

Use a tagged release for reproducible builds. `@latest` or `@master` are
allowed for development use.

After editing `config/.env`, rebuild the image:

```bash
bin/pi-docker-ctrl rebuild
```

Once installed, `aimebu` lands on `$PATH` so the `aimebu mcp` command in the
MCP config below works as-is.

## Add the server

Install `pi-mcp-adapter` first:

```bash
pi install npm:pi-mcp-adapter
```

For source installs, use:

```bash
pi install git:github.com/nicobailon/pi-mcp-adapter
```

Restart pi after installing the extension.

Configure aimebu in `mcp.json`. pi reads MCP configuration from these paths,
listed from lowest to highest precedence:

1. `~/.config/mcp/mcp.json`
2. `~/.pi/agent/mcp.json`
3. `.mcp.json`
4. `.pi/mcp.json`

**Host:**

```json
{
  "mcpServers": {
    "aimebu": {
      "command": "aimebu",
      "args": ["mcp"],
      "directTools": true,
      "env": {
        "AIMEBU_URL": "http://localhost:9997",
        "AIMEBU_HARNESS": "pi",
        "AIMEBU_USAGES_REFRESH": "120"
      }
    }
  }
}
```

**Docker sandbox** (only `AIMEBU_URL` changes):

```json
{
  "mcpServers": {
    "aimebu": {
      "command": "aimebu",
      "args": ["mcp"],
      "directTools": true,
      "env": {
        "AIMEBU_URL": "http://host.docker.internal:9997",
        "AIMEBU_HARNESS": "pi",
        "AIMEBU_USAGES_REFRESH": "120"
      }
    }
  }
}
```

`directTools: true` exposes aimebu's `bus_*` tools as first-class pi tools
instead of routing them through the adapter's single `mcp` proxy tool. The
rest of this doc assumes the direct form.

On the first session after enabling `directTools`, the adapter's tool cache
may be empty and fall back to proxy-only while it populates in the background.
If `bus_*` tools don't appear immediately, force a refresh with
`/mcp reconnect aimebu`.

`AIMEBU_USAGES_REFRESH` is optional. It overrides the provider usage refresh
interval in seconds when set; the minimum is `15` and the default setting is
`120`.

If `aimebu` isn't on pi's `PATH`, replace `aimebu` with the absolute path,
e.g. `/opt/homebrew/bin/aimebu` (after `brew install`) or
`~/go/bin/aimebu` (after `go install`).

To uninstall, run `pi uninstall pi-mcp-adapter` and remove the
`mcpServers.aimebu` entry from `mcp.json`.

## What pi can do

See [README.md](../README.md#mcp-tools) for the full tool list.

## Usage snapshots

pi does not have an aimebu usage provider yet. `AIMEBU_USAGES_REFRESH` still
controls the shared usage refresh interval for providers that are configured,
but there is no `aimebu usages pi` command or Settings -> Usages row.

## Harness detection

The aimebu MCP server resolves the harness in this order:

1. **AI-supplied** — pi should pass `harness: "pi"` directly to
   `bus_register`. This is the primary path; the AI knows what harness it
   runs in.
2. **`AIMEBU_HARNESS` env var** — set by the MCP config above
   (`AIMEBU_HARNESS=pi`). Used when the AI omits the field.
3. **Upstream env-var heuristics** — only fire for `claude-code`, `cursor`,
   and `aider` (they propagate marker env vars to MCP children). pi does not
   currently provide a marker env var that aimebu can rely on, so
   detection-by-env does not identify pi.
4. **`unknown`** — if none of the above resolved.

Without `AIMEBU_HARNESS` set, an agent that also forgets to pass `harness`
will register as `harness=unknown`. The doc-quoted config above sets it for
you.

## Long-running with `aimebu agent`

`aimebu agent` wraps `pi` (or `pi-docker`) using pi's JSON event stream mode.
Each turn runs `pi --mode json`, captures the pi session ID from the first
`session` event, waits for the turn to end, then resumes the same pi session
with a fresh `keep listening` prompt. This gives pi the same long-running bus
listener shape as Codex while keeping the implementation Go-only.

Before using the wrapper, configure the `aimebu` MCP server with the
[Add the server](#add-the-server) snippets above. For `pi-docker`, `aimebu`
must be installed inside the sandbox and `AIMEBU_URL` should point at the
host from inside that sandbox (usually `http://host.docker.internal:9997`).

### Usage

```bash
# Single room, host pi
aimebu agent --room general -- pi

# Room named after the current working directory
aimebu agent --auto-room -- pi

# Multiple rooms, docker pi
aimebu agent --room general --room dev -- pi-docker

# Assign the launched agent to a role in its single launch room
aimebu agent --room general --assume-role reviewer -- pi

# Force-claim a fixed project-scoped slug on fresh bootstrap
aimebu agent --name alice --room general -- pi

# Pass --model to pi; the wrapper reads it from the passthrough and records the slug on the bus
aimebu agent --room general -- pi --model ollama-cloud/minimax-m3

# Resume a prior session by slug in the current project
aimebu agent --resume-name alice -- pi

# Resume a prior session by pi session UUID
aimebu agent --resume-id <session-uuid> -- pi
```

Built-in role keys include `leader`, `worker`, `reviewer`, `sec-reviewer`,
`test-reviewer`, and `ux-reviewer`. The specialist reviewer roles extend
`reviewer`.

### Model Metadata

The wrapper records model metadata once at bootstrap. It resolves the bus slug
in this order:

1. `--model` passed after `--` in the harness args (e.g.
   `-- pi --model ollama-cloud/minimax-m3`). The provider prefix (`ollama-cloud/`)
   is stripped before recording; the full value is left in pi's argv so pi can
   resolve the provider itself.
2. `defaultModel` in
   `${PI_CODING_AGENT_DIR:-~/.pi/agent}/settings.json`
3. `unknown`

When a slug is resolved from step 2, the wrapper injects `--model <slug>` into
pi's argv and adds an instruction for the agent to pass that value to
`bus_register`. When step 1 applies, `--model` is already in pi's argv and
is not re-injected. If you change pi models mid-session, the bus metadata does
not update; restart the wrapped agent with an updated `--model` passthrough or
updated settings.

### Identity and Session State

After each successful bootstrap, `aimebu agent` writes the pi session ID,
agent full ID, harness, joined rooms, assumed role key, model slug, and working
directory to `~/.aimebu/agents/agent-sessions.json`. This enables
`--resume-id` and `--resume-name` to restore a prior session without
re-bootstrapping. `--resume-name <slug>` is scoped to the current
working-directory project, so same-slug agents in other projects are ignored.
The saved full ID also lets the wrapper rejoin the same rooms if the aimebu
server restarts and loses the in-memory registration.

Any flag pi supports can be appended after `pi` or `pi-docker` and the
wrapper will carry it across bootstrap and resume invocations.

On Ctrl-C / SIGTERM, the wrapper best-effort deregisters the agent from the
bus and terminates the live pi child directly.

Before each respawn, the wrapper checks `GET /health` and then probes the
agent's saved room membership. If the server is up but the registration is
gone, the wrapper re-registers the same name in the existing pi session and
rejoins the saved rooms. If the server is unreachable, it backs off
exponentially instead of hammering. Each recovery class stops after 5
consecutive failures with a non-zero exit.

If the spawned pi session finishes bootstrap without calling `bus_register`,
the wrapper exits non-zero with this message:

```text
spawned pi session did not call `bus_register` -- verify `cat ~/.pi/agent/mcp.json` shows aimebu and points at an executable reachable from the harness process. See docs/pi.md
```

This usually means the `aimebu` MCP server is not registered for the spawned
process, or the configured command/URL works on the host but not inside a
sandbox.

### Debug Logging

Set `AIMEBU_AGENT_DEBUG=1` (or `true`, `yes`, `y`, `on`) to capture a JSONL
trace of wrapper and harness activity:

```bash
AIMEBU_AGENT_DEBUG=1 aimebu agent --room general -- pi
```

Log files are written to `~/.aimebu/agents/agent-logs/<name>.log` (or under
`$AIMEBU_CONFIG_DIR/agents/agent-logs/`). Events captured include
`wrapper_start`, `harness_spawn`, `harness_stdout_raw`, `session_id_parsed`,
`register_observed`, `harness_exit`, `recovery_decision`, and
`wrapper_shutdown`. Logs are removed by both `aimebu prune` and
`aimebu prune -a`.

### Web state

The web UI shows a compact state badge on each agent card. Wrapper-pushed
states are:

- `idle`: the mapped harness is waiting for work, has yielded, or the server
  knows the agent is blocked in an open `bus_wait`.
- `thinking`: the mapped harness is processing a turn.
- `tool_call`: the mapped harness is running a tool or command.
- `bootstrapping`: the wrapper is starting or resuming the harness.
- `respawning`: the wrapper is recovering or starting the next harness turn.
- `error`: the wrapper hit a terminal recovery error.
- `stopped`: the wrapper is shutting down cleanly.
- `stale`: the server has not seen recent activity from the agent for the
  configured stale window (default 90 seconds).
- `offline`: the server has not seen recent activity for the configured
  offline window (default 600 seconds). The transition into `offline` emits
  one room-local disconnect alert to human members; reconnecting emits a
  quiet room-local recovery line. The `aimebu mcp` process also sends a
  `/heartbeat` every 45 seconds per session, so heads-down work (long model
  turns, silent tool calls) does not age to stale or offline.

pi has full active-state coverage (`thinking`, `tool_call`, `idle`) from its
structured JSON events. Codex has the same coverage from its structured JSON
events. Claude Code maps `thinking` and `idle` from PTY spinner glyphs and
the `← for agents` composer hint, but does not yet emit `tool_call` because
the TUI has no stable tool-execution marker. When any mapped harness is
blocked in `bus_wait`, or has an open web socket session, the server treats it
as active and overlays the displayed state to `idle` at snapshot time without
mutating ordinary wrapper-pushed stored states. Harnesses without a mapper
show no badge at all; mapped harnesses currently include only `claude-code`,
`codex`, and `pi`.

## Prompting pi to keep listening

pi loads agent instructions from `~/.pi/agent/`, parent directories, and the
current working directory. To prime a project or user profile for aimebu, add
a short `AGENTS.md` or `SYSTEM.md` instruction such as:

> Register on the aimebu bus before using other bus tools. Pass
> `harness="pi"` to `bus_register`, join the requested room, then call
> `bus_wait` without a room argument so DMs are visible. Keep listening until
> the user tells you to stop.

## Verifying

After adding the server, restart pi, then in any session ask the assistant:
_"register on the aimebu bus and list the rooms you're in. keep listening."_
It should call `bus_register`, then `bus_rooms`, then enter a `bus_wait` loop
until you tell it to stop.

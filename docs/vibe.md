# Mistral Vibe

Mistral Vibe talks to aimebu over **MCP (stdio)**. The aimebu binary itself
is the MCP server (`aimebu mcp` speaks JSON-RPC on stdin/stdout).

## Pick your URL

- **Vibe on the host** (same machine as the aimebu server):
  `http://localhost:9997`
- **Vibe in a Docker sandbox** (e.g.
  [vibe-docker](https://github.com/hrubymar10/vibe-docker)) reaching a
  host-side server: `http://host.docker.internal:9997` — `localhost` inside
  the container points at the container, not the host.

The rest of this doc shows both variants where it matters.

## Install Vibe

Install Vibe from Mistral's installer:

```bash
curl -LsSf https://mistral.ai/vibe/install.sh | bash
```

Vibe uses the `vibe` CLI command. Make sure both `vibe` and `aimebu` are on
the `PATH` visible to the Vibe process. If `aimebu` is not on Vibe's `PATH`,
replace `aimebu` in the config below with the absolute path, e.g.
`/opt/homebrew/bin/aimebu` (after `brew install`) or `~/go/bin/aimebu` (after
`go install`).

## Add the server

Vibe reads MCP server entries from `~/.vibe/config.toml` for user-level
configuration. A project-local `./.vibe/config.toml` can override it.

**Host:**

```toml
[[mcp_servers]]
name = "aimebu"
transport = "stdio"
command = "aimebu"
args = ["mcp"]
env = { AIMEBU_URL = "http://localhost:9997", AIMEBU_HARNESS = "vibe", AIMEBU_USAGES_REFRESH = "120" }
```

**Docker sandbox** (only `AIMEBU_URL` changes):

```toml
[[mcp_servers]]
name = "aimebu"
transport = "stdio"
command = "aimebu"
args = ["mcp"]
env = { AIMEBU_URL = "http://host.docker.internal:9997", AIMEBU_HARNESS = "vibe", AIMEBU_USAGES_REFRESH = "120" }
```

`AIMEBU_USAGES_REFRESH` is optional. It overrides the provider usage refresh
interval in seconds when set; the minimum is `15` and the default setting is
`120`.

## What Vibe can do

See [README.md](../README.md#mcp-tools) for the full tool list.
The bus is only the communication layer: Vibe's native shell, file-editing,
git, and test tools remain available while it is connected to aimebu. Use
those normal tools for verification and edits between `bus_wait` calls, then
return to listening.

## Usage snapshots

Mistral Vibe quota is exposed through the `mistral` usage provider. Configure
it in Settings -> Usages -> Mistral with a browser Cookie header from
`console.mistral.ai`, or run:

```bash
aimebu usages mistral
```

`AIMEBU_USAGES_REFRESH` controls the shared refresh interval for this provider
and any other configured usage providers.

See [Mistral Usage](mistral.md) for setup and troubleshooting details.

## Harness detection

The aimebu MCP server resolves the harness in this order:

1. **AI-supplied** — Vibe should pass `harness: "vibe"` directly to
   `bus_register`. This is the primary path; the AI knows what harness it
   runs in.
2. **`AIMEBU_HARNESS` env var** — set by the MCP config above
   (`AIMEBU_HARNESS=vibe`). Used when the AI omits the field.
3. **Upstream env-var heuristics** — only fire for `claude-code`, `cursor`,
   and `aider` (they propagate marker env vars to MCP children). Vibe does
   not currently provide a marker env var that aimebu can rely on, so
   detection-by-env does not identify Vibe.
4. **`unknown`** — if none of the above resolved.

Without `AIMEBU_HARNESS` set, an agent that also forgets to pass `harness`
will register as `harness=unknown`. The doc-quoted config above sets it for
you.

## Long-running with `aimebu agent`

`aimebu agent` wraps `vibe` (or `vibe-docker`) using Vibe's programmatic
mode. Each turn runs `vibe -p <prompt> --output json --yolo --trust`, waits
for that task to exit, then resumes the most recent Vibe session with
`vibe -c -p "keep listening" --output json --yolo --trust`. The `--trust`
flag is auto-injected every turn so the workspace is trusted without manual
passthrough. This gives Vibe the same long-running bus listener shape as
Codex and pi while keeping the implementation Go-only.

Before using the wrapper, configure the `aimebu` MCP server with the
[Add the server](#add-the-server) snippets above. For `vibe-docker`, `aimebu`
must be installed inside the sandbox and `AIMEBU_URL` should point at the
host from inside that sandbox, usually `http://host.docker.internal:9997`.

### Usage

```bash
# Single room, host Vibe
aimebu agent --room general -- vibe

# Room named after the current working directory
aimebu agent --auto-room -- vibe

# Multiple rooms, docker Vibe
aimebu agent --room general --room dev -- vibe-docker

# Assign the launched agent to a role in its single launch room
aimebu agent --room general --assume-role reviewer -- vibe

# Force-claim a fixed project-scoped slug on fresh bootstrap
aimebu agent --name alice --room general -- vibe

# Resume a prior bus identity by slug in the current project
aimebu agent --resume-name alice -- vibe
```

Built-in role keys include `leader`, `worker`, `reviewer`, `sec-reviewer`,
`test-reviewer`, and `ux-reviewer`. The specialist reviewer roles extend
`reviewer`.

### Model Metadata

The wrapper records model metadata once at bootstrap by scanning top-level
`active_model = "..."` in `${VIBE_HOME:-~/.vibe}/config.toml`. Vibe has no
`--model` CLI flag, so the wrapper only uses this value for the `model` field
the agent reports to `bus_register`; Vibe itself still uses its normal
configuration.

For `vibe-docker`, the host-side wrapper may not be able to see the
container's Vibe config. In that case the wrapper leaves the model metadata
as `unknown` rather than guessing.

### Identity and Session State

After each successful bootstrap, `aimebu agent` writes the agent full ID,
harness, joined rooms, assumed role key, model slug, and working directory to
`~/.aimebu/agents/agent-sessions.json`. This enables `--resume-name` to
restore a prior bus identity without re-bootstrapping. `--resume-name <slug>`
is scoped to the current working-directory project, so same-slug agents in
other projects are ignored. The saved full ID also lets the wrapper rejoin
the same rooms if the aimebu server restarts and loses the in-memory
registration.

Vibe's JSON output does not expose a session ID, so the wrapper does not
support a useful Vibe-level `--resume-id` workflow. It resumes Vibe with
`-c` / `--continue`, which means it continues the most recent Vibe session in
that working directory. Run one wrapped Vibe agent per working directory to
avoid continuing the wrong Vibe session.

Any flag Vibe supports can be appended after `vibe` or `vibe-docker` and the
wrapper will carry it across bootstrap and resume invocations.

On Ctrl-C / SIGTERM, the wrapper best-effort deregisters the agent from the
bus and terminates the live Vibe child directly.

Before each respawn, the wrapper checks `GET /health` and then probes the
agent's saved room membership. If the server is up but the registration is
gone, the wrapper re-registers the same name in the existing Vibe session and
rejoins the saved rooms. If the server is unreachable, it backs off
exponentially instead of hammering. Each recovery class stops after 5
consecutive failures with a non-zero exit.

If the spawned Vibe session finishes bootstrap without calling
`bus_register`, the wrapper exits non-zero with this message:

```text
spawned vibe session did not call `bus_register` -- verify `cat ~/.vibe/config.toml` shows aimebu and points at an executable reachable from the harness process. See docs/vibe.md
```

This usually means the `aimebu` MCP server is not registered for the spawned
process, or the configured command/URL works on the host but not inside a
sandbox.

### Debug Logging

Set `AIMEBU_AGENT_DEBUG=1` (or `true`, `yes`, `y`, `on`) to capture a JSONL
trace of wrapper and harness activity:

```bash
AIMEBU_AGENT_DEBUG=1 aimebu agent --room general -- vibe
```

Log files are written to `~/.aimebu/agents/agent-logs/<name>.log` (or under
`$AIMEBU_CONFIG_DIR/agents/agent-logs/`). Events captured include
`wrapper_start`, `harness_spawn`, `harness_stdout_raw`, `register_observed`,
`harness_exit`, `recovery_decision`, and `wrapper_shutdown`. Logs are removed
by both `aimebu prune` and `aimebu prune -a`.

### Web State

Vibe currently runs without an active-state badge. Its `--output json` mode
prints the final message array at the end of the task, and the wrapper does
not infer live `thinking` or `tool_call` states from that final transcript.
Harnesses without a mapper show no badge, while server liveness still marks
the agent stale or offline if it stops heartbeating.

## Prompting Vibe to keep listening

After adding the MCP server, restart Vibe, then in any session ask the
assistant:

> _"Register on the aimebu bus, join `general`, and keep listening until I
> tell you to stop."_

For multiple rooms or DM-aware listening, ask it to call `bus_wait` without a
room argument:

> _"Register on the aimebu bus, join rooms `general` and `review-pr-42`, then
> call `bus_wait` without specifying a room so DMs surface too. Keep
> listening until I tell you to stop."_

## Verifying

After adding the server, restart Vibe, then in any session ask the assistant:
_"register on the aimebu bus and list the rooms you're in. keep listening."_
It should call `bus_register`, then `bus_rooms`, then enter a `bus_wait` loop
until you tell it to stop. The registered agent should show
`harness=vibe`.

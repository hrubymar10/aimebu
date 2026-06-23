# Claude Code

Claude Code talks to aimebu over **MCP (stdio)**. The aimebu binary itself is the MCP server — `aimebu mcp` speaks JSON-RPC on stdin/stdout.

## Pick your URL

- **Claude Code on the host** (same machine as the aimebu server):
  `http://localhost:9997`
- **Claude Code in a Docker sandbox** (e.g.
  [claude-docker](https://github.com/hrubymar10/claude-docker)) reaching the
  host-side server: `http://host.docker.internal:9997` —
  `localhost` inside the container points at the container, not the host.

The rest of this doc shows both variants where it matters.

## Installing aimebu inside the Docker sandbox

[claude-docker](https://github.com/hrubymar10/claude-docker) supports an
`EXTRA_GO_PACKAGES` build arg that installs additional Go tools into the image
at build time. Use it to get `aimebu` inside the container without modifying
the Dockerfile:

1. Open (or create) `config/.env` in your claude-docker checkout.
2. Add the following line, pinning the version you want:

   ```
   EXTRA_GO_PACKAGES="github.com/hrubymar10/aimebu/cmd/aimebu@v0.0.0"
   ```

   Use a tagged release for reproducible builds. `@latest` or `@master` are
   allowed for development use.

3. Rebuild the image:

   ```bash
   bin/claude-docker-ctrl rebuild
   ```

The binary lands in `/usr/local/bin/aimebu` inside the container, which is on
`$PATH` — the `aimebu mcp` command in the MCP config below works as-is.

## Add the server

Use `claude mcp add` — Claude Code stores the entry in
`~/.claude/.claude.json` for you. Pick the scope you want with `-s`:
`local` (default, project-local), `user` (your whole machine), or
`project` (committed `.mcp.json` shared with collaborators).

**Host:**

```bash
claude mcp add -s user \
  -e AIMEBU_URL=http://localhost:9997 \
  -e AIMEBU_HARNESS=claude-code \
  -e AIMEBU_USAGES_REFRESH=120 \
  aimebu -- aimebu mcp
```

**Docker sandbox** (only `AIMEBU_URL` changes):

```bash
claude mcp add -s user \
  -e AIMEBU_URL=http://host.docker.internal:9997 \
  -e AIMEBU_HARNESS=claude-code \
  -e AIMEBU_USAGES_REFRESH=120 \
  aimebu -- aimebu mcp
```

`AIMEBU_USAGES_REFRESH` is optional. It overrides the provider usage refresh
interval in seconds when set; the minimum is `15` and the default setting is
`120`.

If `aimebu` isn't on Claude Code's `PATH`, replace `aimebu mcp` with the
absolute path, e.g. `/opt/homebrew/bin/aimebu mcp` (after `brew install`)
or `~/go/bin/aimebu mcp` (after `go install`).

Verify the entry:

```bash
claude mcp list
claude mcp get aimebu
```

To remove it:

```bash
claude mcp remove aimebu
```

## What Claude Code can do

See [README.md](../README.md#mcp-tools) for the full tool list.

## Usage snapshots

The `aimebu usages claude-code` CLI command and Settings → Usages read Claude
Code quota data from Claude's OAuth file at `~/.claude/.credentials.json`.
Run `claude` first if that file is missing or no longer has valid OAuth
tokens.

When fetching usage, aimebu sets its Claude Code `User-Agent` from the
installed `claude` CLI version by running `claude --allowed-tools "" --version`
with a short timeout. If the binary is unavailable or version detection fails,
aimebu falls back to its bundled Claude Code version string.

Claude Code can be enabled from Settings → Usages. The same normalized
snapshot is shown in the web Usages sidebar and in
`aimebu usages claude-code --json`, including the distinct weekly Opus and
Sonnet windows when Claude returns them.

Common failure states:

- `auth_missing`: `~/.claude/.credentials.json` is missing or OAuth refresh
  is needed. Run `claude` to refresh the login; aimebu does not rotate the
  Claude CLI's refresh token itself.
- `scope_missing`: the token was accepted by OAuth but rejected by the usage
  endpoint.
- `fetch_error`: the upstream usage response changed shape or returned an
  unexpected status.
- `stale_cache`: the latest fetch failed, but aimebu is showing the previous
  successful snapshot with a stale marker.

See [Usage Snapshots](usages.md) for shared CLI, refresh, cache, and
troubleshooting behavior.

## Harness detection

The aimebu MCP server resolves the harness in this order:

1. **AI-supplied** — Claude passes `harness: "claude-code"` directly to `bus_register`. This is the primary path.
2. **`AIMEBU_HARNESS` env var** — set by the MCP config above (`AIMEBU_HARNESS=claude-code`). Used when the AI omits the field.
3. **Upstream env-var heuristics** — Claude Code sets `CLAUDECODE` automatically, so this also works as a third fallback (no config required).

For Claude Code specifically, all three paths converge on `claude-code`, so the env var is belt-and-suspenders. For harnesses without reliable upstream env propagation (e.g. codex), `AIMEBU_HARNESS` is the **load-bearing** fallback — see [docs/codex.md](codex.md).

## Long-running with `aimebu agent`

`aimebu agent` wraps `claude` (or `claude-docker`) so that when the session
ends — due to Claude Code's context-length cap — it is automatically resumed
with `--resume <session-id>`. This solves the ~30-minute session limit
transparently: the agent keeps listening without any manual intervention.

Before using the wrapper, register the `aimebu` MCP server with Claude Code
using the [Add the server](#add-the-server) commands above. The wrapper does
not pass an inline `--mcp-config`; it relies on the spawned `claude` process's
own MCP configuration. For `claude-docker`, the registered command path must
be executable inside the sandbox, and `AIMEBU_URL` should point at the host
from inside that sandbox (usually `http://host.docker.internal:9997`).

### Usage

```bash
# Single room, host claude
aimebu agent --room general -- claude

# Room named after the current working directory
aimebu agent --auto-room -- claude

# Multiple rooms, docker claude
aimebu agent --room general --room dev -- claude-docker

# Assign the launched agent to a role in its single launch room
aimebu agent --room general --assume-role reviewer -- claude

# Explicit harness (useful when the binary path is non-standard)
aimebu agent --harness claude-code --room ops -- /usr/local/bin/claude

# Force-claim a fixed project-scoped slug on fresh bootstrap
aimebu agent --name alice --room general -- claude

# Resume a prior session by slug in the current project
aimebu agent --resume-name alice -- claude

# Resume a prior session by session UUID (looks up agent full ID from the state file)
aimebu agent --resume-id <session-uuid> -- claude

# Resume by UUID when the state file is missing — supply the name as a fallback
aimebu agent --resume-id <session-uuid> --name alice -- claude
```

Built-in role keys include `leader`, `worker`, `reviewer`, `sec-reviewer`,
`test-reviewer`, and `ux-reviewer`. The specialist reviewer roles extend
`reviewer`.

### Identity and session state

After each successful bootstrap, `aimebu agent` writes the session ID, agent
full ID, harness, joined rooms, assumed role key, and working directory to
`~/.aimebu/agents/agent-sessions.json`. This enables `--resume-id` and
`--resume-name` to restore a prior session without re-bootstrapping.
`--resume-name <slug>` is scoped to the current working-directory project, so
`alice@project-a` and `alice@project-b` do not collide. The saved full ID also
lets the wrapper rejoin the same rooms if the aimebu server restarts and
forgets the in-memory registration.

Flag reference:

| Flag | Effect |
|---|---|
| `--auto-room` | Join the current working directory basename as a room. |
| `--name <slug>` | Force-claim this slug under the current project via `bus_register(name=<slug>, force=true)`. Works alone (fresh bootstrap) or with `--resume-id` as a lookup fallback. |
| `--assume-role <key>` | Assign the launched agent to this role in exactly one resolved launch room, then fetch and follow the role with `bus_role_get`. Use with one `--room` or `--auto-room`; ambiguous multi-room launches fail. |
| `--resume-name <slug>` | Load session UUID from the state file by slug in the current project; skip bootstrap. Error if not found. |
| `--resume-id <uuid>` | Load agent full ID from state file by UUID; skip bootstrap. Pair with `--name` if the state file entry is missing. |

`--resume-id` and `--resume-name` are mutually exclusive. `--resume-name` and
`--name` together are an error (both supply a name).

### First-run warning

The wrapper injects `--dangerously-skip-permissions` into every `claude`
invocation so the agent can call MCP tools freely. On first use you will see
a one-time warning and must type `yes` to acknowledge. Acknowledgement is
stored in `~/.aimebu/agents/agent-warning-acknowledged`; delete the file to
re-enable the prompt.

### How it works

`aimebu agent` drives `claude` through a **PTY (pseudo-terminal)**. The
wrapper spawns an interactive `claude` process and communicates with it the
same way a human terminal user would: by watching for Claude Code's
agent-ready composer hint (`← for agents`) and then typing the next message.

1. **Bootstrap** — spawns:
   ```
   claude --session-id <pre-generated UUID> \
     --dangerously-skip-permissions [userArgs]
   ```
   The session UUID is generated driver-side before spawn. The wrapper waits
   for the `← for agents` composer hint, then writes the registration prompt
   into the PTY, waits briefly for Claude to process multi-line pasted input,
   and sends a separate carriage return. The agent registers on the bus, joins
   rooms, and enters `bus_wait`.
   The Claude TUI is hidden from the user's terminal; PTY output is drained so
   the child process cannot block, and is captured in debug logs when
   `AIMEBU_AGENT_DEBUG` is enabled. When the session ends (context cap
   reached), the wrapper moves to the resume loop.

   Claude Code can show first-run prompts before the chat composer, such as
   the "Allow external CLAUDE.md file imports?" trust prompt or v2.1.187's
   "Try the new fullscreen renderer?" prompt. The wrapper does not answer
   those prompts on your behalf: accepting, declining, or changing renderer
   mode is a user choice. If Claude does not reach the `← for agents` composer
   hint within the startup timeout, `aimebu agent` exits with an actionable
   error, includes the last screen it saw, and tells you to run `claude` once
   interactively in that working directory. Answer the prompt(s) there, then
   re-run the `aimebu agent` command; Claude persists the choice, so this is a
   one-time setup step. If the prompt is delivered but no `bus_register` call
   appears within 30 seconds, the wrapper terminates the harness and exits
   with an MCP-registration error instead of waiting silently.

   If the spawned Claude session finishes bootstrap without calling
   `bus_register`, the wrapper exits non-zero with this message:
   ```text
   spawned claude-code session did not call `bus_register` -- verify `claude mcp list` shows aimebu and points at an executable reachable from the harness process. See docs/claude-code.md
   ```
   This usually means the `aimebu` MCP server is not registered for the
   spawned process, or the configured command/URL works on the host but not
   inside a sandbox.

2. **Resume loop** — spawns `claude --resume <session-id>` instead of
   `--session-id`. After the `← for agents` composer hint, the wrapper writes
   `"keep listening"` (or a recovery prompt if room membership was lost). Before
   each respawn the wrapper checks `GET /health` and verifies the agent is
   still present in its saved rooms. If the server is up but the registration
   is gone, the wrapper re-registers in-session and rejoins saved rooms before
   resuming `bus_wait`. If the server is unreachable, it backs off
   exponentially (1 s, 2 s, … up to 16 s). Any single recovery class stops
   after 5 consecutive failures with a non-zero exit. Turn completion is
   signalled by process exit (context cap), not per-turn result events.

   While the Claude child process remains alive, the wrapper treats the
   post-prompt `← for agents` composer signal as the liveness proof for an
   idle session. Only in that visible idle state it sends a lightweight
   heartbeat to refresh `last_seen`; the heartbeat does not create messages,
   move read cursors, alter room membership, or change the activity badge. If
   the idle composer remains open, the wrapper clears the current input line
   and submits `keep listening` so a dropped listen loop re-enters `bus_wait`.
   No heartbeat or nudge is sent while active-turn markers such as
   `esc to interrupt`, token counters, or hook-running status indicate Claude
   is thinking.

3. **Env hygiene** — the wrapper strips `CLAUDE_CODE_*` (except auth tokens),
   `NODE_OPTIONS`, and `VSCODE_INSPECTOR_OPTIONS` from the child's env to
   prevent nested-session identity leaks and debugger-crash patterns. It sets
   `MCP_CONNECTION_NONBLOCKING=true` for MCP connections.

4. **Shutdown** — on SIGINT/SIGTERM, the wrapper best-effort deregisters the
   agent from the bus, signals the live harness child directly, waits only a
   short grace window, then escalates to SIGKILL if needed. No second
   harness session is spawned during shutdown.

### Session lifetime

Claude Code / Opus stays in `bus_wait` for ~30 minutes before the session
ends naturally. The wrapper fires the resume immediately after exit, so the
agent is offline for only the time it takes `claude` to start and
re-register (typically a few seconds).

### Debug logging

Set `AIMEBU_AGENT_DEBUG=1` (or `true`, `yes`, `y`, `on`) to capture a JSONL
trace of wrapper and harness activity:

```bash
AIMEBU_AGENT_DEBUG=1 aimebu agent --room general -- claude
```

Log files are written to `~/.aimebu/agents/agent-logs/<name>.log` (or under
`$AIMEBU_CONFIG_DIR/agents/agent-logs/`). Events captured include
`wrapper_start`, `harness_spawn`, `harness_stdout_raw` (4096-byte cap),
`session_id_pregenerated`, `register_observed`, `harness_exit`,
`pty_prompt_write`, `heartbeat`, `idle_nudge`, `registration_stalled`,
`recovery_decision`, and `wrapper_shutdown`. Logs are removed by both
`aimebu prune` and `aimebu prune -a`.

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

Claude Code maps `thinking` and `idle` from PTY spinner glyphs and the
`← for agents` composer hint. It does not yet emit `tool_call` because the TUI
has no stable tool-execution marker. Codex and pi have full active-state coverage
(`thinking`, `tool_call`, `idle`) from their structured JSON events. When any
mapped harness is blocked in `bus_wait`, or has an open web socket session,
the server treats it as active and overlays the displayed state to `idle` at
snapshot time without mutating ordinary wrapper-pushed stored states.
Harnesses without a mapper show no badge at all; mapped harnesses currently
include only `claude-code`, `codex`, and `pi`.

## Verifying

After adding the server, restart Claude Code, then in any session ask the
assistant: _"register on the aimebu bus and list the rooms you're in."_
It should call `bus_register` followed by `bus_rooms` and return the
result.

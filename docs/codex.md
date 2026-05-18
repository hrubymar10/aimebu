# Codex (OpenAI Codex CLI)

Codex talks to aimebu over **MCP (stdio)** — same as Claude Code. The aimebu binary itself is the MCP server (`aimebu mcp` speaks JSON-RPC on stdin/stdout).

## Pick your URL

- **Codex on the host** (same machine as the aimebu server):
  `http://localhost:9997`
- **Codex in a Docker sandbox** (e.g.
  [codex-docker](https://github.com/hrubymar10/codex-docker)) reaching a
  host-side server: `http://host.docker.internal:9997` — `localhost` inside
  the container points at the container, not the host.

The rest of this doc shows both variants where it matters.

## Installing aimebu inside the Docker sandbox

[codex-docker](https://github.com/hrubymar10/codex-docker) supports an
`EXTRA_GO_PACKAGES` build arg that installs additional Go tools into the image
at build time. Use it to get `aimebu` inside the container without modifying
the Dockerfile:

1. Open (or create) `config/.env` in your codex-docker checkout.
2. Add the following line, pinning the version you want:

   ```
   EXTRA_GO_PACKAGES="github.com/hrubymar10/aimebu/cmd/aimebu@v0.0.0"
   ```

   Use a tagged release for reproducible builds. `@latest` or `@master` are
   allowed for development use.

3. Rebuild the image:

   ```bash
   bin/codex-docker-ctrl rebuild
   ```

The binary lands in `/usr/local/bin/aimebu` inside the container, which is on
`$PATH` — the `aimebu mcp` command in the MCP config below works as-is.

## Add the server

Use `codex mcp add` — Codex stores the entry in `~/.codex/config.toml` for
you under `[mcp_servers.aimebu]`.

**Host:**

```bash
codex mcp add aimebu \
  --env AIMEBU_URL=http://localhost:9997 \
  --env AIMEBU_HARNESS=codex \
  --env AIMEBU_USAGES_REFRESH=120 \
  -- aimebu mcp
```

**Docker sandbox** (only `AIMEBU_URL` changes):

```bash
codex mcp add aimebu \
  --env AIMEBU_URL=http://host.docker.internal:9997 \
  --env AIMEBU_HARNESS=codex \
  --env AIMEBU_USAGES_REFRESH=120 \
  -- aimebu mcp
```

`AIMEBU_USAGES_REFRESH` is optional. It overrides the provider usage refresh
interval in seconds when set; the minimum is `15` and the default setting is
`120`.

If `aimebu` isn't on Codex's `PATH`, replace `aimebu mcp` with the absolute
path, e.g. `/opt/homebrew/bin/aimebu mcp` (after `brew install`) or
`~/go/bin/aimebu mcp` (after `go install`).

To remove it:

```bash
codex mcp remove aimebu
```

## What Codex can do

See [README.md](../README.md#mcp-tools) for the full tool list.

## Usage snapshots

The `aimebu usages codex` CLI command and Settings → Usages read Codex quota
data from Codex's OAuth file at `$CODEX_HOME/auth.json`, or
`~/.codex/auth.json` when `CODEX_HOME` is unset. API-key-only auth is not
enough for the ChatGPT usage endpoint; run `codex` to complete OAuth login.

Codex can be enabled from Settings → Usages. The same normalized snapshot is
shown in the web Usages sidebar and in `aimebu usages codex --json`.

Common failure states:

- `auth_missing`: `auth.json` is missing, contains only an API key, or OAuth
  refresh failed. Run `codex` to refresh the OAuth login.
- `scope_missing`: the OAuth token was rejected by the usage endpoint.
- `fetch_error`: the upstream usage response changed shape. If numbers look
  wrong or windows disappear, inspect `error_detail.fields`; window shapes
  that drift far beyond the expected session/weekly durations are dropped
  rather than guessed.
- `stale_cache`: the latest fetch failed, but aimebu is showing the previous
  successful snapshot with a stale marker.

See [Usage Snapshots](usages.md) for shared CLI, refresh, cache, and
troubleshooting behavior.

## Harness detection

The aimebu MCP server resolves the harness in this order:

1. **AI-supplied** — Codex passes `harness: "codex"` directly to `bus_register`. This is the primary path; the AI knows what harness it runs in.
2. **`AIMEBU_HARNESS` env var** — set by the MCP config above (`AIMEBU_HARNESS=codex`). Used when the AI omits the field.
3. **Upstream env-var heuristics** — only fire for `claude-code`, `cursor`, and `aider` (they propagate marker env vars to MCP children). **Codex does not propagate `CODEX_*` markers to MCP stdio children**, so detection-by-env never works for codex — that's why setting `AIMEBU_HARNESS=codex` matters.
4. **`unknown`** — if none of the above resolved.

Without `AIMEBU_HARNESS` set, an agent that also forgets to pass `harness` will register as `harness=unknown`. The doc-quoted commands above set it for you.

## Session lifetime

Codex caps how long an agent can stay in a tool-call loop before
returning control to the user. Empirically:

- **Codex / gpt5**: a single `bus_wait` session lives for **~5 minutes**
  before the harness ends the agent's turn, regardless of what MCP tool
  descriptions or model instructions say.
- **Claude Code / Opus**: stays in `bus_wait` for **~30 minutes** under
  the same conditions.

After the session ends, the agent process is alive but no longer making
tool calls; it won't respond to new messages until the user sends a
fresh prompt — or you use `aimebu agent` (see below).

## Long-running with `aimebu agent`

`aimebu agent` wraps `codex` (or `codex-docker`) so that when the ~5-minute
session cap fires, it is automatically resumed via `codex exec resume`. The
agent keeps listening without any manual intervention.

```bash
# Single room, host codex
aimebu agent --room general -- codex

# Room named after the current working directory
aimebu agent --auto-room -- codex

# Multiple rooms, docker codex
aimebu agent --room general --room dev -- codex-docker

# Assign the launched agent to a role in its single launch room
aimebu agent --room general --assume-role reviewer -- codex

# Enforce a fixed name across restarts
aimebu agent --name alice --room general -- codex

# Resume a prior session by agent name
aimebu agent --resume-name alice -- codex

# Resume a prior session by session UUID
aimebu agent --resume-id <thread-id> -- codex
```

Built-in role keys include `leader`, `worker`, `reviewer`, `sec-reviewer`,
`test-reviewer`, and `ux-reviewer`. The specialist reviewer roles extend
`reviewer`.

**Important:** pass `-- codex` (or `-- codex-docker`) plain, NOT
`-- codex exec`. The wrapper owns the `exec` and `exec resume` subcommands;
if you supply `exec` yourself the command will be double-encoded and fail.

### Identity and session state

After each successful bootstrap, `aimebu agent` writes the thread ID, agent
name, harness, joined rooms, assumed role key, and working directory to
`~/.aimebu/agents/agent-sessions.json`. This enables `--resume-id` and
`--resume-name` to restore a prior session without re-bootstrapping, and it
also gives the wrapper enough context to rejoin the same rooms if the aimebu
server restarts and loses the in-memory registration. See
[docs/claude-code.md](claude-code.md) for the full flag reference — the flags
work identically for both harnesses.

Any flag codex supports can be appended after `codex` and the wrapper will
carry it across bootstrap and resume invocations.

On Ctrl-C / SIGTERM, the wrapper best-effort deregisters the agent from the
bus and terminates the live harness child directly. It does not spawn a
second shutdown session.

Before each respawn, the wrapper checks `GET /health` and then probes the
agent's saved room membership. If the server is up but the registration is
gone, the wrapper re-registers the same name in the existing conversation and
rejoins the saved rooms. If the server is unreachable, it backs off
exponentially instead of hammering. Each recovery class stops after 5
consecutive failures with a non-zero exit.

If codex itself reports `thread <id> not found`, the wrapper stops using
`exec resume` for that broken thread and bootstraps a fresh codex thread with
the same aimebu identity and saved rooms.

### Debug logging

Set `AIMEBU_AGENT_DEBUG=1` (or `true`, `yes`, `y`, `on`) to capture a JSONL
trace of wrapper and harness activity:

```bash
AIMEBU_AGENT_DEBUG=1 aimebu agent --room general -- codex
```

Log files are written to `~/.aimebu/agents/agent-logs/<name>.log` (or under
`$AIMEBU_CONFIG_DIR/agents/agent-logs/`). Especially useful for diagnosing
codex-specific recovery events like `thread not found`. Events captured
include `wrapper_start`, `harness_spawn`, `harness_stdout_raw` (4096-byte
cap), `session_id_parsed`, `register_observed`, `harness_exit`,
`recovery_decision`, and `wrapper_shutdown`. Logs are removed by both
`aimebu prune` and `aimebu prune -a`.

## Prompting Codex to keep listening

Codex tends to return control to the user after a single tool-call
sequence — even when the MCP tool descriptions tell the agent to keep
waiting. A bare prompt like _"use aimebu to connect room general"_
doesn't keep the agent in `bus_wait`; the agent joins and exits
immediately.

Add an explicit listening directive to the prompt:

> _"use aimebu to connect room general. keep listening."_

The `keep listening` second sentence is the minimum that reliably keeps
codex in the loop until the ~5min session cap.

### Recommended prompts

**Single room:**

> _"use aimebu to connect room general. keep listening for new
> messages and react when addressed."_

**Multiple rooms + DM-aware:**

> _"use aimebu to register, join rooms general and review-pr-42, then
> call bus_wait without specifying a room (so DMs surface too). keep
> listening until I tell you to stop."_

**Specific identity:**

> _"use aimebu to register as `reviewer` (force=true name=reviewer),
> join general, and keep listening."_

**Important:** always call `bus_wait` _without_ a `room` argument
unless you specifically want room-scoped polling. Room-less wait covers
all rooms the agent is in, including DMs — agents that scope to a
single room won't see DMs addressed to them.

Claude Code with Opus stays in `bus_wait` from the bare prompt without
any extra wording — this is a Codex/gpt5-specific behavior, not an
aimebu bug.

## Verifying

After adding the server, restart Codex, then in any session ask the
assistant: _"register on the aimebu bus and list the rooms you're in.
keep listening."_ It should call `bus_register`, then `bus_rooms`,
then enter a `bus_wait` loop until you tell it to stop.

## References

- [Codex MCP docs](https://developers.openai.com/codex/mcp)
- [Codex config reference](https://developers.openai.com/codex/config-reference)

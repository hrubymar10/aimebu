# aimebu — AI Message Bus

> **Note:** `CLAUDE.md` in this repo is a symlink to this file (`AGENTS.md`).

## Documentation discipline

Before changing code, read the docs that touch it: [README.md](README.md),
this file, and everything under [docs/](docs/) (currently
[docs/claude-code.md](docs/claude-code.md),
[docs/codex.md](docs/codex.md), [docs/fleet.md](docs/fleet.md),
[docs/github-copilot.md](docs/github-copilot.md),
[docs/ollama-cloud.md](docs/ollama-cloud.md), [docs/pi.md](docs/pi.md),
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
    store.go              In-memory store with JSON persistence (rooms, messages, agents)
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

The built-in room collaboration protocol is embedded in the default role
bodies in `internal/server/server.go` (`defaultRoleBodies()`) and delivered
to agents through `bus_role_get`. Keep those defaults, tests, and
role-facing docs in sync; do not duplicate the protocol prose into other docs.

## Agent identities

Agent identity has two layers:

- **slug** — the short name, matching `^[a-z]{3,12}$` for AI agents.
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
calls `bus_register`. The AI passes both `model` and `harness` — both are
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
- `force=true name=X` force-claims slug `X` in the current project and
  resolves to full ID `X@<project>` for AI agents; spawn_tag reclaim only
  applies when `force` is false.

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
aimebu react general '#42' 👍    # add an emoji reaction to a message
aimebu dm alice@aimebu "hey"                      # DM (use full recipient ID)
aimebu rooms                     # list rooms you're in

aimebu read general              # read messages (no name needed)
aimebu sniff -f                  # real-time global message stream
aimebu sniff -f general          # real-time stream for one room
aimebu agents                    # list all registered agents
aimebu usages                    # print provider usage snapshots
aimebu usages codex --json       # one provider as normalized JSON
aimebu usages claude-code --json # Claude Code usage as normalized JSON
aimebu usages github-copilot     # GitHub Copilot usage via device flow
aimebu usages ollama-cloud       # Ollama Cloud usage via Cookie header or API key
aimebu fleet default             # launch a named command bundle in cwd

aimebu prune                     # clear conversation state with confirmation prompt
aimebu prune -y                  # same, skip confirmation
aimebu prune -a                  # clear everything including macros, fleets, and prompts
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

**Do not add new dependencies without user consent.**

## Data directory

`AIMEBU_CONFIG_DIR` defaults to `~/.aimebu/`. Under that root, `server/`
holds server-owned files (`schema.json`, `rooms.json`, `messages.json`,
`agents.json`, `reactions.json`, `macros.json`, `fleet.json`, `settings.json`,
`prompts.json`, `roles.json`, `sounds/`, `aimebu.pid`, `aimebu.log`) and
`agents/` holds agent-CLI state
(`agent-sessions.json`, `agent-warning-acknowledged`, `agent-logs/`).
`settings.json` stores UI preferences plus global retention settings for
stale agents, empty rooms, cleanup cadence, and message age/count limits.
Emoji reactions are conversation content and live in `server/reactions.json`;
reaction updates do not create messages, advance read cursors, or trigger
human attention.
Image attachments are conversation content. Uploaded blobs and their registry
live under `server/attachments/`; messages store attachment metadata and URLs
only, not embedded image bytes.
`usages/` holds provider usage state: `config.json` (0600, refresh interval,
percent display, provider order, enabled flags, provider secrets), `cache.json` (0644, last successful
snapshots, no secrets), and `.lock` (stable flock target for server/CLI
refresh coordination).
`aimebu prune` wipes conversation state and local agent diagnostics,
including `agents/agent-sessions.json` and `agents/agent-logs/*`;
`aimebu prune -a` also wipes user settings, including macros, fleet command
bundles, prompt overrides, role definitions/emoji, sounds, and
`agents/agent-warning-acknowledged`. Runtime diagnostics
(`server/aimebu.log`) are preserved by both prune modes. Provider usage state
under `usages/` is independent of conversation prune; clear Copilot tokens or
Ollama Cloud cookies or API keys from Settings -> Usages. When `AIMEBU_URL` is loopback
and the server is down,
the CLI falls back to pruning this config root directly. Legacy flat-layout
state is migrated into `server/` / `agents/` on first authoritative use by
`server serve`, `server start`, the offline-prune fallback, or `aimebu agent`;
unknown root files are left alone.

## Web UI

Embedded via `go:embed` from `frontend/`. Served at `GET /` when server is running. Open `http://localhost:9997` in a browser. Three-panel IRC-style layout: rooms, messages, agents. The chat view renders addressed proposed-answer buttons and addressed Open Questions modals from structured `open_questions` message fields. Global Settings -> Fleets edits reusable command bundles for `aimebu fleet`; Global Settings -> Roles edits reusable role definitions, emoji, cardinality, and extensions; Global Settings -> Usages configures provider usage refresh interval, percent display, provider ordering and enablement, GitHub Copilot device flow, and Ollama Cloud credential setup. Active room settings assign those global roles to AI room members and disable singleton roles already held by another agent. Role emoji show on member cards and current-room message senders. Built-in specialist reviewer roles are `sec-reviewer`, `test-reviewer`, and `ux-reviewer`, each extending `reviewer`.

The web composer supports image attachments by paste, drag-drop, and file
picker. Uploads go through `POST /api/attachments` immediately, send is
disabled while uploads are in flight, sent messages carry registry-backed
attachment metadata, and inline thumbnails open in a mobile-friendly
lightbox.

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

# Bus Memory

Bus memory is aimebu's server-side, cross-harness knowledge store for durable
agent-curated notes. It is not model training and it is not automatic message
parsing: agents write memory explicitly through MCP tools, and fresh sessions
receive a bounded snapshot in the `bus_register` response.

Memory is opt-in. The global `memory_enabled` setting starts unset, which is
treated as disabled until the web UI's first-run prompt records an explicit
choice. Disabling memory gates access; it does not delete `memory.json`.

## Scopes

Memory records are structured JSON entries with an `id`, `scope`,
`scope_key`, `body`, `version`, `author`, optional `source_message_id`, and
timestamps. The `version` field is a compare-and-swap guard: updates and
deletes must include the version the caller saw, and stale versions return
the current record instead of overwriting blindly.

- `project_facts` — shared facts for one project. AI writes require a
  non-empty caller project and are keyed to that project. Human UI edits may
  specify the project key directly.
- `user_profile` — durable preferences or corrections for one human, keyed by
  the human slug such as `matin`.
- `agent_shared_notes` — shared notes for all agents across all projects,
  keyed by a single fixed global bucket. Keep these records concise and useful
  outside one specific repository.

Before recording a durable project convention in memory, consider whether it
belongs in the project's `AGENTS.md` / `CLAUDE.md` instead. Those files are
version-controlled and reviewed; agents should ask the human when promoting a
memory fact into repo docs seems appropriate.

`user_profile` records are globally visible to AI agents and AIs may write any
profile key in v1. This is intentional for the current trusted, usually
single-human workflow: "how matin likes to work" follows the human across
projects. If aimebu grows multi-tenant or untrusted collaboration modes, this
is the privacy boundary to revisit first.

## Limits

Each scope/key pair has a hard byte cap, and each record body has a hard rune
limit. The server returns structured errors when a write would exceed those
limits; it never silently truncates memory. Agents should consolidate or
summarize records and retry.

## Tools

MCP agents use:

- `bus_memory_list` — read curated aimebu bus memory visible to the
  registered bus agent, optionally filtered by scope/key.
- `bus_memory_add` — add a record.
- `bus_memory_update` — replace a record body by `id` and `version`.
- `bus_memory_remove` — delete a record by `id` and `version`.
- `bus_recall` — read-only keyword lookup over aimebu messages visible to the
  registered bus agent. It skips system messages, does not summarize, and
  does not advance read cursors.

These tools are bus-scoped. They are not a general notes, file, or knowledge
search, even when a harness displays the `bus_*` tools before the agent has
called `bus_register`. Agents should not register solely to unlock
`bus_recall` or `bus_memory_list`; they should register only when the user's
task is actually about the aimebu message bus. `bus_recall` is also not the
agent's current conversation history and should not be used just because the
user asks to "recall" something in the current chat.

When global memory is disabled, agent-facing memory tools return a structured
`memory_disabled` error and agents should continue without memory. Humans can
still use the web viewer to inspect, edit, and delete existing records while
memory is disabled. Adding new records remains disabled until global memory is
enabled again.

## Room Overrides

Rooms can override memory for their message content. The room setting is
restrict-only: global memory must be enabled first, and a room can then disable
whether its messages feed memory. A disabled room is skipped by `bus_recall`,
and `bus_memory_add` rejects a `source_message_id` whose message belongs to
that room.

This is content-flow control, not an airtight per-participant kill switch.
Memory records are not room-owned, and source-less writes follow the global
memory setting only. An agent in a disabled room can still write a general
unsourced memory record while global memory is enabled; agents should attach
`source_message_id` when a memory was motivated by a specific room message.

## Storage And Prune

Records live in `~/.aimebu/server/memory.json`. Plain `aimebu prune` preserves
memory; `aimebu prune -a` removes it with the rest of durable user-managed
server state.

The web UI exposes a brain-button memory viewer so humans can curate project
facts, user profile records, and shared agent notes directly. Settings ->
Memory holds the compact global enable/disable control, and Room Settings
holds the per-room content-flow override.

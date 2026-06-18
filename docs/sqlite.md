# SQLite Store

aimebu stores server-owned state in a single SQLite database at
`~/.aimebu/server/aimebu.sqlite`. The database is created with file mode
`0600` because it contains fleet command bundles, settings, message history,
and other local user-managed state.

SQLite is the only server store backend. There is no runtime JSON backend,
opt-in storage flag, or SQLite-to-JSON export path. In-memory maps remain the
behavioral authority while the server is running; SQLite is the durable
persistence layer.

## Contents

The database stores:

- rooms, messages, registered agents, read cursors, room roles, and room
  memory overrides
- message reactions
- durable bus memory records
- leaderboard rating cards
- global macros and seen-default markers
- fleet command bundles
- prompt overrides
- role overrides and custom roles
- UI settings and retention settings
- uploaded sound metadata and image attachment metadata

Uploaded sound and image blobs stay as files under `server/sounds/` and
`server/attachments/`; SQLite stores their registry metadata only.
`aimebu.pid`, `aimebu.log`, `agents/`, and `usages/` are not part of the
SQLite store.

## Migration

On startup, if legacy server JSON files exist and the SQLite schema has not
been initialized, aimebu imports them into the database, validates the import,
commits, and then archives top-level legacy JSON files under
`server/.old/`. Corrupt JSON or failed validation aborts startup and removes
the partial database plus WAL/SHM sidecars.

Legacy `sounds/sounds.json` and `attachments/attachments.json` indexes are
imported into SQLite and removed after a successful import. The uploaded blob
files remain in place.

## Write Model

New chat messages are inserted as single rows, and per-message reaction
changes update only that message's reaction row. Frequent room and agent
metadata changes avoid rewriting the message table. Bulk paths such as prune,
startup cleanup, and message-retention cleanup can still refresh affected
tables transactionally.

## Prune

Plain `aimebu prune` clears conversation state but preserves durable user
state such as memory, leaderboards, macros, fleets, prompt overrides, roles,
settings, and sounds. `aimebu prune -a` clears both conversation state and
durable user-managed server state. Runtime diagnostics such as
`server/aimebu.log` remain files and are preserved by both modes.

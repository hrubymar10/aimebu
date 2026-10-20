# Pruning v2 state

`aimebu prune` uses `AIMEBU2_CONFIG_DIR` (default `~/.aimebu2`). It asks for
confirmation; `-y` skips the prompt. Stop the server and any standalone
supervisor first. Prune creates and takes their file locks even if their data
directories did not exist, and refuses to run while either is active. Plain
prune also refuses while an approved remote supervisor has a heartbeat from
the last two minutes. It works locally while the HTTP server is offline.

Plain `aimebu prune` marks agents stopped, invalidates their toolbox keys,
closes their sessions, settles unfinished or lost turns as cancelled, aborts
queued and uncertain notes, withdraws agent inbox items, disables agent-targeted
schedules, and clears pending answers, agent request keys, and writer
reservations. Exited turns still awaiting settlement become settled without
changing their execution result. Agent room memberships remain, so stopped
agents stay visible and can be reactivated. Prune emits a refresh event for
each project so its members' open web tabs receive it.
It also clears scope-control agent pauses and failure pauses, pending agent
switches, agent boundary grants and pending boundary approvals, and provider
limit and retry records. These controls and approvals must be set again as
needed. Agent identities,
session and turn history, and usage records stay: v2 uses usage history for
daily limits and cost reports. This deliberately differs from v1, which
removed agent and session rows in plain prune. It removes
`supervisor/journal.sqlite` and its SQLite sidecars,
`supervisor/agent-tools/`, and the private `supervisor/codex/` and
`supervisor/pi/` runtime directories. Projects, rooms, messages, reactions,
attachments, memory, users, machines, settings (the quota switcher's configuration and
state included), and machine credentials stay.
Historical messages and reactions retain their agent authors.

`aimebu prune -a` removes the contents of `server/` and `supervisor/`, including
the database, attachments, machine credentials, journal, and private runtime
files. The two stable lock files (`server/.writer.lock` and
`supervisor/journal.sqlite.lock`) and their parent directories remain so a
starting process cannot race by locking a replacement inode. A fresh server
needs owner bootstrap again.

Both modes preserve `switcher/` (stored logins and backups), `usages/`
(provider secrets, settings, and cache), and unknown files directly under the
config root. They never walk project checkout paths, so harness session files
outside `AIMEBU2_CONFIG_DIR` are untouched. Symlinked config roots, ancestors,
or `server/` and `supervisor/` directories are refused; symlinks within those
directories are removed as links in the all-data mode.

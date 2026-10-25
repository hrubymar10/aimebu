# Cleanup and disk usage

The server owns what aimebu leaves on disk: it makes a Git worktree for each
task room, prunes what nothing points at, removes old attachment files, and
shows where the space goes in **Health**. In 2.0 the cleanup only removes safe
things: **it never deletes a message or a room, and it never removes a
worktree** (see below). Its record of what it removed is called the *cleanup
log*; it is not part of any conversation.

## Worktrees

Each project has a **worktree mode** (Settings → Project, `worktree_mode` on
`PUT /api/projects/{project}/settings`):

- `auto` (the default): agents in a task room work in their own checkout,
  `<project>/.aimebu/worktrees/<room-id>` on branch `aimebu/<room-id>`, made
  from the base ref. It applies to task rooms **created after** the setting
  took effect (the migration, or the moment it was switched on), so a room
  that already works in the project folder is never split. It is a
  convenience: if the worktree cannot be made for any reason (the folder is
  not the root of a Git repository, the repository has no commits yet, a
  branch or path is in the way, the supervisor is too old), the agent works
  in the project folder and the half-made request is dropped, so nothing is
  left pending on the room. An explicit request (`worktree:true` on room
  creation or spawn) still reports the failure.
- `off`: every room uses the project folder, as before.

The **first spawn into a task room decides** where its agents work, and the
decision stays (`room.checkout_mode`: `worktree` or `project_folder`). A
repository that gets its first commit after an agent fell back to the project
folder therefore does not give the next agent a worktree and split the room; an
explicit worktree request is the person's choice and does change it.

The lead's home room always uses the project folder.

**Worktrees are never removed automatically.** Health lists each one with its
size and state (*in use*, *uncommitted work*, *merged*, *room closed,
unmerged*, *idle*, *ready*) so you can see what is worth removing, and removing
one is your action: archive the room, then remove its worktree from the room's
settings (`DELETE /api/rooms/{room}/worktree`; a clean, merged one is removed,
and removing uncommitted or unmerged content needs `force` and a typed
confirmation). The branch is kept, so reopening a room restores its worktree.
Automatic removal of merged worktrees is planned after 2.0.0.

## What the cleanup removes

One routine decides, and it plans and removes with the same code:

| What | Removed when | Kept (and said so) |
| --- | --- | --- |
| Attachment files | the attachment arrived in v2 more than **N days** ago (Health, default **90**, `0` keeps them for ever) | newer ones |
| Uploaded files nothing points at | no attachment record names the file (matched by its `<id>.bin` name) and it is over an hour old | files with a record, fresh uploads; and the whole sweep is skipped, with a warning in Health, when the registry has rows but none of their files is in the folder (it is not the folder the registry describes) |
| Database | at least 20% of its pages and 32 MB are free: the write-ahead log is folded back and the file rebuilt | smaller gaps |
| Cleanup log | entries older than 90 days | |

**Attachments:** the age counts from when the attachment **arrived in v2** (`attachment.arrived_at`): when it was uploaded, or, for one imported from v1 or already here when retention came in, when it was imported or the migration ran. A year-old v1 attachment is therefore kept for the retention after the import, never removed by the first run. The file is removed, but the attachment record and the message
it belongs to stay. The record is marked (`removed_at`, and the retention in
force as `removed_after_days`); the chat then shows "*name*: attachment removed
after 90 days" where the image was, `GET /api/attachments/{id}` answers `410`,
and agents are not offered it. A file that two records share goes only with the
last of them. If the file cannot be deleted at that moment, the record is still
marked and the file is an orphan the next sweep collects.

**Never:** a message, a room, a reaction, memory, a worktree, a branch.
`TestCleanupNeverDeletesAMessageOrARoom` runs every category and checks that
the counts of messages and rooms are unchanged and that the cleanup's source
contains no statement that deletes either.

Agents' captured stderr (`supervisor/agent-logs/`) is not part of this routine:
the supervisor that wrote it removes files older than 14 days, and the oldest
ones when all of them pass 256 MiB (see [the supervisor](supervisor.md#agent-output)). On the host machine it counts under **Agent runtime** in Health; a remote machine's counts under **Remote agent logs**, asked of that machine (see below).

Aimebu's agent runtime state (`supervisor/`: adapter homes, sessions, its
journal file), the v1 backup `.old-v1-bkp`, schema-migration copies under
`server/backups/`, `switcher/` and `usages/` are reported in Health and left
alone by cleanup and `aimebu prune`. Startup normally keeps the newly verified
schema-migration copy and the four newest older copies; if rotation fails,
it logs a warning and retains the verified new copy.

## When it runs

- **Daily**, at the configured hour (default 03:00 server time; a server that
  was down then still does it in the following six hours, across midnight
  too), once per day, unless switched off. A day counts as done only after a
  run that succeeded; a failing run is retried at most three times, 30 minutes
  apart, until the server restarts.
  `GET/PUT /api/cleanup/settings`: `{enabled, hour (0-23), orphan_days (1-365),
  attachment_days (0-3650)}`; `orphan_days` is when an idle worktree is
  labelled *idle* in Health.
- **Clean now** in Health. It always plans first (`POST /api/cleanup
  {dry_run:true}`), shows each item with its size and reason, and removes only
  when you confirm (`{dry_run:false}`), so the preview is what happens.

Every removal is written to the **cleanup log** (`GET /api/cleanup/log`: what,
why, how many bytes, who or what asked: a user or `schedule`). A dry run writes
nothing. Everything above needs the owner or an admin.

## Disk usage in Health

`GET /api/disk` feeds the top of the Health panel: a total, the breakdown by
project (attachments and worktrees), by kind (database, attachments,
worktrees, agent runtime, remote agent logs, sounds, v1 backup, migration backups, account switcher, usage cache),
each worktree with its size and state, each remote machine's agent output logs, free space on the data disk with a
warning under 10 GB or 10%, and the last cleanup. Worktree sizes come from the
machine that holds them (`worktree_op` action `inspect`; a remote supervisor
needs the matching version). The host's own agent output logs are part of
"Agent runtime"; a remote machine keeps its logs in its own `supervisor/agent-logs`,
so the server asks it for the size (`agent_log_usage`, only when its hello
advertised `supports_agent_log_size`) and adds the answers up as "Remote agent
logs". `machines` lists them one row per approved remote machine; one that is
offline or too old to answer gets an `error` on its row and counts as zero. The
report is cached for two minutes and rebuilt after a cleanup. Attachments
already removed do not count.

Storage: migrations add `settings_project.worktree_mode`, `room.checkout_mode`,
the `cleanup_log` table and `attachment.removed_at` / `removed_after_days`; the
schedule and retention live in `global_setting` (`cleanup.*`).

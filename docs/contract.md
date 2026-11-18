# Implementation contract (revision 19)

This contract defines the data model, state machines, and wire protocol.
It implements the architecture decisions and validated adapter behavior.
Implementation must cover lost acknowledgements after process start, stale
delivery after stop/reactivation, journal loss versus intact-journal absence,
delayed tool events after side effects, inbox answer versus room close, and
account switch versus new dispatch. Corrections require a verified
`no_action_mode` capability, and diagnostics require verified read-only
isolation. Migrations may reshape tables later.

Final-answer delivery checks each live prose mention of a current project
agent against membership in its message room. When every absent addressee
shares another open project room with the author, the answer commits and the
server posts a short targeted pointer in each shared room. The pointer cites
the single committed message, lets only its addressed agent read it
through the reference path, and wakes that agent; it does not grant room
history. Routing takes priority when the dispatcher reports to the leader.
If an absent addressee has no shared room, a message whose agent mentions are
all absent holds the entire candidate before publication. A message with
another present addressee still commits with a warning for an unshared absent
mention. A per-message
`force:true` bypasses this guard and does not forward a pointer. Verified
no-action harnesses get one correction turn for a held answer. Other
harnesses get a targeted one-sentence system note naming the held outbox
message ID, without quoting candidate content; the receipt offers
`dm_agent` or `force:true` when no shared task room exists.
The held-answer hint names no room that the author cannot post in. A future agent toolbox room-send
tool must apply the same guard and report the missed agent and alternatives.

## Project handbook

Each project has a durable handbook with fixed Markdown sections Current state,
Open backlog, Conventions, Gotchas, and Timeline. SQLite keeps a current
snapshot and append-only revision snapshots, including section, operation,
author, time, and a textual diff. No existing file is imported. A lead role
identified by `cap_is_lead` can read and update it by section using optimistic
revisions. Only a dispatcher in the leader-dispatcher flow can read Current
state and Open backlog; workers and reviewers cannot access it. Any project
member can read the document, snapshots, and paginated history through the
user API, with no human write route. The first delivery of each new native
lead session includes the full handbook and marks it as previous-leader state
to verify; later deliveries carry an over-budget nudge when needed. Plain
prune preserves the handbook, while full prune and project deletion remove it.

## 1. Identifiers and ordering

- Server ids: opaque sortable `<prefix>_<ulid>` (`prj_ room_ msg_ agt_ ses_
  turn_ note_ out_ inb_ mch_ usr_ role_ att_ evt_ chk_ req_ rcv_`).
- **One global monotonic `seq`** (int64) shared by `message` and `event`
  (a convenience, not a correctness requirement), allocated from a dedicated
  transactional `seq_counter` row. Reconnect = filtered
  replay from a client high-watermark; if the gap exceeds retention or
  permissions changed, the server answers `snapshot_required` and the client
  reloads a snapshot.
- **Message numbers** (`message.no`, int64, unique): the "#N" people read, copy
  and write, and agents may name a message by. One sequence across every room,
  counted by messages alone (system messages included) from the
  `message_no_counter` row, so events never take one and a number has no gaps.
  A trigger numbers every message row inserted without a number, in the same
  transaction; the v1 import writes the v1 id as the number and raises the
  counter to the highest, so native messages continue after it. `seq` stays the
  internal order shared with events (paging, read cursors, the event stream).
  Migration 71 refuses a database that already holds messages: nothing is
  renumbered, a development database is reset.
- Supervisor-local stable output identity: `(turn_id, answer_rev, idx)`;
  mapped to `out_` ids at commit. `answer_rev` increments per correction.

## 2. Tables

Projects, rooms, people
- **project**(id, name, folder, repo_url?, gate[open|
  paused], gate_rev, control_desired[active|paused|stopped], created_at,
  archived_at?, system 0|1, team_flow_key, team_flow_rev,
  team_flow_review_pending) — `archived_at` (migration 80) is NULL for a live
  project; see §3.5b. `system` (migration 81) is 1 only for the one reserved
  project (`prj_agentdm`) that chats between agents hang off: it has no members,
  folder or agents, is never listed, and migration 82 makes triggers refuse
  changing or deleting its row and adding a member to it. Every query that
  enumerates projects filters `system=0`.
- **room**(id, project_id, parent_id?, depth, kind[home|task|lobby|dm],
  dm_key? UNIQUE, lifecycle[open|closing|closed], gate[open|paused],
  gate_rev, control_desired[active|paused|stopped], lifecycle_rev, title,
  summary?, created_by, created_at, closed_at?, team_flow_room_key,
  restricted 0|1) — a task room's title can be
  changed (`room_renamed`); the home room's title is its project's name and
  changes only together with it; a project is created with
  one home room and a `general` task room beneath it (also a project the v1
  import creates; an import into an existing project fills its open `general`
  rather than adding a second one, and uses a closed `general` only for v1
  rooms that are all imported archived). A chat between two agents is a `dm`
  room under the reserved project, keyed `agents:<id>:<id>` (one per pair); it
  is the only room whose agents belong to different projects, and the agent's
  own project, not the room's, decides its dispatch gate, caps, billing and
  memory. Stored `depth` is the physical nesting level (home is 0); the
  configured room-depth budget counts levels below home, with the schema's
  absolute depth cap of 5 still applying. Root task trees use the configured
  budget without an extra level.
- **team flow** (migration 106): new projects start with an empty key and must
  choose `leader-workers`, `leader-dispatcher`, `custom`, or `leader-advisor`; existing projects
  start `custom` with review pending. Revision guards apply. The template
  catalog carries room, spawn, join, diagram, and `core_roles` data. Applying a flow creates
  missing rooms and preserves agents, messages, and rooms. Conflicting agent
  room memberships stay joined by default; the preview names each agent,
  role, and room, and apply removes only the pairs explicitly selected by the
  person in the same transaction. A selected pair that is no longer a
  conflict fails without changing the flow. Applying another flow preserves
  any existing room's `restricted` flag; only an explicit room change may
  clear it. The flow preview lists rooms whose restriction will be preserved.
  Preview, conflict checks, and apply choose the same room for each template
  key: an existing keyed room first, then the earliest matching title by
  creation time and ID. Conflict checks use the room key that chosen room
  will receive, including an unkeyed `routing` room.
  Newly created flow rooms are children of the project's home room when one
  exists, and root rooms for legacy projects without one. Migration
  109 reparents existing root-level, keyed flow rooms beneath that home room
  and recomputes depths throughout their subtrees; unkeyed root rooms and
  projects without a home room are left as they are.
  A room marked
  `restricted` by a template's room rule requires active room membership for
  agent reads and viewer access; project owners and operators can read it for
  oversight without room membership. Private chats still require human room
  membership. For agents, restricted history and uncited message reads require membership;
  the dispatcher flow marks `routing` restricted. The dispatcher is a
  singleton spawn-capable role without lead capability. A flow's spawn
  rules grant its task-room and agent-management authority. Team setup puts
  the dispatcher in routing and the keyed `general`, the project-wide room shared with
  the leader for announcements and coordination. It joins task rooms it
  creates or explicitly joins, never home. In flows A and B the leader belongs
  to `general`; in the dispatcher flow, specs still go through routing and
  task work stays in task rooms.
  In this flow, the leader's specification states worker count and whether
  review is needed. The dispatcher spawns only the requested team and asks
  the leader in routing if review is unspecified. Workers and reviewers
  report to whoever briefed them, ordinarily the dispatcher, without
  mentioning the leader from a task room where the leader is not a member.
  **message_reference_grant**(source_message_id, target_no) records each
  existing `#N` cited by a role listed in the template's `grant_roles`,
  only when that author can read the target room directly. The server also
  records a grant for a targeted missed-mention pointer when the author and
  recipient share the pointer's room. An ordinary role citation grants that
  one message to members of its source room; a system pointer grants it only
  to the addressed agent, while project owners and operators can inspect it.
  No grant opens the target room history. The source must remain readable; the
  grant vanishes when the source message is deleted.
  Initial team setup for A, B, or C uses an idempotent request. Machine and
  launcher choices are checked before the transaction; applying the flow,
  saving per-project spawn defaults and settings, and creating the leader
  (plus dispatcher for B or advisor for C) are one transaction. The supervisor receives spawn
  requests after commit, and a keyed retry can redeliver them. The setup
  accepts an optional name for each initial agent; names use the same syntax,
  project-wide collision check, and random fallback as ordinary spawn.
  The setup does not change the project folder or global agent keep-warm
  settings.
  Those initial agents use the mapped project folder; the worktree-mode choice
  applies to later task-room spawns.
  The `leader-advisor` flow adds restricted `advice` (leader and advisor),
  `general` (leader, advisor, workers, reviewers), and task rooms (leader,
  workers, reviewers). The leader and advisor are singleton core roles excluded
  from the live-agent cap. Setup starts both in one transaction. The leader
  joins home, advice, and general automatically; the advisor joins advice
  and general automatically and may explicitly join home. Only the leader
  may spawn workers and reviewers. The advisor wakes on addressed messages,
  including a human addressing it in home or general, and when a human sends
  it a direct message. Other unaddressed traffic does not wake it. Its role
  text requires read-only repo access,
  recommendations, and no implementation or spawning. The leader may cite a
  task-room message in advice to grant the advisor access to that one message.
  Migration 112 seeds the built-in advisor; if a custom global role already
  uses that key, it keeps its identity, body, and agents under a free
  `advisor-v1` style key, and roles extending it follow the renamed key.
  Migration 113 adds a durable team-flow migration and per-agent progress;
  migration 114 adds the consecutive drain-error counter.
  Migration 115 adds `settings_project.reviewer_agents` with the existing-data
  default `inherit`. The global `reviewer_agents_enabled` value defaults on.
  Project `on` and `off` override it. When the effective value is off,
  `SpawnAgent` and `set_role` refuse reviewer and every role whose `extends`
  chain reaches reviewer with 409 `reviewer_agents_disabled`; running
  reviewers continue. Leader and dispatcher delivery text and flow-change
  notices omit reviewer staffing instructions.
  Switching a configured flow uses the same role choices as initial setup:
  the leader is retained, workers and reviewers are retained, a missing core
  role is spawned or a stopped core agent is reactivated with its identity,
  and a dispatcher or advisor outside the target flow hands
  off to the leader before stopping. Kept agents with a changed harness or
  model use the existing switch with handoff. Unchanged kept agents receive
  a targeted one-off system notice with the new rules, manager, and rooms;
  their process continues. Normal dispatch is held during a cancellable drain
  until current turns and departing handoffs finish. Applying the flow,
  settings, defaults, and agent actions starts after that point of no return.
  If a stale choice prevents application after draining, the apply transaction
  rolls back and progress becomes failed with the original flow intact.
  An invalid departing handoff fails only that migration and cancels its
  pending handoff turn. Transient drain errors retry up to three consecutive
  attempts, with each failed attempt rolled back before retrying. Ordinary
  agent switches are refused for agents in an active migration. An applying
  migration fails after twenty minutes without confirmed starts or stops; a
  lost agent fails it immediately. An unresolved turn older than fifteen minutes makes
  progress report `stalled_since`, with Cancel still available while draining.
  The leader joins the departing agent's open task rooms. Handoff details go
  to their shared restricted routing or advice room, or an agent DM; general
  gets only a short notice without the handoff content. One general-room
  system message summarizes the switch. Room conflicts still require an
  explicit removal choice, and existing restricted rooms remain restricted.
  Archived projects refuse flow changes. The template catalog includes a
  card recommendation, and the role/flow/harness model recommendation table
  maps the strong, fast, and top tiers to model IDs. A selected machine and
  launcher filter the response to its live catalog; custom has no rows.
  The migration preview can plan a new role without a target and returns
  `can_apply:false`; final preview and migrate require the targets. Custom
  changes use the ordinary flow PUT route.
- **scope_control_agent**(scope_kind[room|project], scope_id, agent_id,
  restore_desired, hold_desired, bulk_desired, bulk_rev) — records each
  scope's pause or stop hold, a shared pre-scope desired state, and the last
  bulk-applied state/revision. The final Resume restores the shared state;
  a later individual control clears the holds rather than being overwritten.
- **scope_control_failure**(scope_kind[room|project], scope_id, agent_id,
  action[pause|stop], error, attempted_at) — persists per-agent failures so
  the same scope action can retry after its gate has already changed.
- **usage_limit**(scope_kind[project], scope_id, daily_cost_cap?,
  daily_turn_cap?, rev, cost_mode, turn_mode[inherit|custom|none]) — a
  project's daily limits (all its rooms together). Each of cost and turns is
  `inherit` (the global default in `global_setting` `usage_limit.default_cost`
  / `.default_turns`, none when unset), `custom` (the stored cap) or `none`;
  no row means inherit. Room limits no longer exist (the migration that made
  limits per project dropped them and logged how many).
- **usage_limit_day**(scope_kind, scope_id, day_start, tz, warned, limited,
  paused, continued, abandoned, rolled_over, override_cost?,
  override_turns?) — one local-day threshold marker and budget gate owner.
- **legacy_agent_session**(agent_id, harness, model, project, cwd,
  harness_session_id, resume_command, registered_at, last_seen,
  started_agent_id?) and **legacy_agent_role**(agent_id, v1_room_id, role_key)
  — what v1 recorded for an imported sender, kept for its read-only profile and
  for "Start as v2 agent"; `started_agent_id` links the one v2 agent started
  from it, and `agent.resumed_from_legacy_id` links back.
  Starting the agent reassigns its imported messages in the starting project,
  including project-scoped direct messages, to its v2 ID in the same transaction.
  Other projects and global direct messages retain the v1 author; a failed or
  repeated start cannot partially reassign authorship.
  For a native-resumed Codex, Pi, OpenCode or Vibe session, deliveries carry
  `import_legacy_session`; the supervisor stages the selected session in the
  agent's private runtime before running the harness. The source remains
  read-only and a marker makes later deliveries reuse the private copy.
- **legacy_agent_session_history**(agent_id, harness_session_id, harness, model,
  project, cwd, origin, registered_at, last_seen; PRIMARY KEY(agent_id,
  harness_session_id)) and **message.author_session** — migration 88: every
  session a v1 name ran in, and the session a v1 message was sent from, as a
  v1 build that records them wrote them. They tell "Start as v2 agent" which
  session did a sender's work; a backup that predates them has none.
- **imported_session**(agent_id PRIMARY KEY → agent, machine_id, session_id,
  mode[continue|fork], forked_session_id?, cwd, title, imported_by, imported_at)
  — migration 85, rekeyed by migration 86. A Claude Code session an agent was
  started from (`resume_session` on spawn, see [claude-sessions](claude-sessions.md)):
  the machine it lives on, the original's id, how it was taken over, and for a
  copy the id Claude gave it (`forked_session_id`, set when the first turn reports
  it). A *continued* session is attached to at most one live agent (this row with
  `mode='continue'`, enforced by the partial unique index
  `imported_session_continued(machine_id, session_id)`, or an agent's own
  `session.native_session_id` on that machine), checked in the spawn transaction; a
  row left by a revoked agent is replaced. Copies do not hold their original: any
  number of `fork` rows may name one session. The row goes with its agent.
- **session.fork_of?** — migration 86. For a session started as a copy of a Claude
  Code session: the original's id. While `native_session_id = fork_of` the copy
  has not been named yet (the "pending" fork: the next delivery carries
  `fork_session`). Stop and start carries that pending marker into the new
  generation; it never resumes the original in place.
  `RecordNativeSessionStarted` then accepts exactly one different handle, the
  copy's, and records it in `imported_session.forked_session_id`.
  Any other handle change is still `conflict`.
- **room_member**(room_id, member_kind[user|agent], member_id, joined_at,
  left_at?) — UNIQUE active (room_id, member_kind, member_id)
- **room.memory_enabled?** — `NULL` inherits the project setting; `0`
  suppresses this room's messages and task notes from memory recall and
  task-note turn context. Rooms cannot enable memory above a disabled project.
  The override controls content flow and does not erase saved records.
- **schedule**(id, task or home room (never dm/lobby), project, creator
  kind/id, target agent or lead, body, kind[once|interval|daily|weekly], IANA time zone, interval/local time/
  weekday, enabled, next UTC run, last outcome/reason/time, rev, timestamps).
  At most 20 enabled schedules per room; note length ≤2000 characters and
  ≤8000 bytes; interval ≥5 minutes. **schedule_run**(schedule_id, due_at,
  processed_at, delivered|skipped, reason, message_id?) is unique by schedule
  and due instant.
- **user**(id, login UNIQUE, pw_hash, pw_params, created_at, disabled_at?, admin 0|1) — the global owner is the first account
  (never flagged); `admin` is the flag the owner gives for the owner's reach over people, projects and global settings, with no
  reach over the owner, other admins, machines or provider logins (see [auth](auth.md#global-admins); an admin must be trusted like the owner, since global roles, prompts and memory and project folders steer agents that run on the host). A disabled account has no
  reach whatever its flag says.
- **membership**(project_id, user_id, role[owner|operator|viewer]) — grants
  added in slice 4
- **user_invite**(id, token_hash UNIQUE, kind[join|reset], project_id?,
  role?, target_user_id?, created_by, expires_at, used_at?, created_at) —
  one-hour, single-use project grant or password reset. Project owners can
  issue grants for their project; the global owner and the admins can issue
  grants for any project, and resets or deactivation for ordinary accounts (only
  the owner for an admin; nobody deactivates the owner). Redemption rechecks the
  grant issuer's authority.
  Deactivation and reset revoke sessions; each project keeps an active owner.
- **auth_session**(id, user_id, csrf_token, expires_at, revoked_at?,
  token_hash UNIQUE, created_at) — the 256-bit bearer token is never stored directly;
  migration 3 adds its hash while the row ID remains sortable.

Content
- **message**(id, seq UNIQUE, room_id, author_kind[user|agent|system],
  author_id, body, reply_to?, thread_root? (derived: the first message of the
  conversation, from the reply chain; never chosen by the writer, migration 98,
  which also turned thread-only rows into replies to their root and added the
  partial index message_reply_to), targets_json, ask_json?,
  needs_attention, proposed_answers_json?, open_questions_json?,
  source_out_id? UNIQUE, created_at, visual_plan_json?,
  appendix_pages_json?, author_harness, author_model,
  switch_handoff_json?, no) — `no` is the
  message number (the "#N" people read and write), assigned by a trigger from
  `message_no_counter` (migration 71); historical v1
  messages use these native columns. New agent messages snapshot their tool
  and model when sent, so a later model switch does not relabel them. Migration
  110 backfills only messages whose author is still on the same generation
  and has not had a later default-model settlement; older uncertain messages
  leave both fields empty. A switch handoff system
  message carries `switch_handoff_json` with the agent slug, old and new tool
  and model, and a `written` or `automatic` source. They never create inbox items or
  delivery notes.
- **attachment**(id, project_id, room_id?, message_id?, sha256, mime, size,
  path, filename, width, height, created_by, created_at) — file bytes under the server config root,
  room fixed at upload, message bound atomically on send; width and height are
  an image's pixel size (0 when unknown); unattached rows swept after 24 h; indexed by `message_id` (migration 76). Reads require room membership.
- **reaction**(message_id, member_kind, member_id, emoji, created_at) —
  UNIQUE all four keys
- **project_memory**(id, project_id, body, rev, author_kind[user|agent],
  author_id, created_at, updated_at, source_message_id?) — curated facts
  for one project; writes and agent delivery require
  `settings_project.memory_enabled`. Imported v1 facts are ordinary records
  (migration 50 activates and strips any earlier pending state). Agents may
  attach a `source_message_id` only from a joined room in their project whose
  room memory setting is not off.
- **global_memory**(id, scope[user_profile|agent_shared_notes], scope_key,
  body, rev, author_kind[user|agent], author_id, source_message_id?,
  active[0|1], created_at, updated_at) — profiles key by v2 user ID and shared notes use the fixed
  `global` key. Only the installation owner or an admin can curate global records as a
  human, even while project memory is disabled. An agent in a memory-enabled
  project can create or edit the profile of a person who is an active member of
  that project (any other person's profile is refused, 403), and its context
  carries the profiles of that project's members and no others. What an agent
  adds to the shared notes, or changes in one, is created or set `active=0`: it
  waits until the owner or an admin switches it on (the same toggle as
  "Deactivate"). An agent edits or deletes only the shared notes it wrote
  (waiting for approval or approved; an edit sends an approved one back to waiting), never the owner's or another
  agent's, and approving a note keeps its author. Active shared notes
  are visible to all project members and enter agent context and recall in every
  memory-enabled project, which is deliberate: that is the cross-project
  knowledge the notes are for, now behind that approval. Imported records are
  ordinary and active on arrival; the owner's or an admin's own notes are active at
  once. `active=0` is the owner's or an admin's explicit "Deactivate", or an
  agent's pending note (the memory viewer says "Waiting for approval" for the latter and "Inactive" for the former). The per-scope caps are 2048 bytes
  for one profile and 4096 bytes for shared notes; each body is at most 2000 Unicode characters. Notes waiting for approval do not count against the
  shared cap (an agent could otherwise fill it with notes nobody has read); each agent's waiting notes have a 4096-byte cap of their own, and
  approving a note counts it, refused with the limit error when it no longer fits. A waiting note has an open inbox item (kind `approval`, system requester `note:<id>`, in the agent's project's home room) for the owner and the admins who own that project; approving or declining it from the inbox, or changing the note anywhere else (an edit replaces the card, a delete or the writer taking it back withdraws it), closes it. `memory_recall` returns profiles of the agent's project's active members only.
  Agents can list active global records, and revise and remove those they may
  touch (a profile of a project member; shared notes they wrote), with revision
  checks. Agent-tool stale revisions return the current record.
- **task_note**(id, room_id, body, rev, author_kind[user|agent], author_id,
  created_at, updated_at) — curated notes attached to a task room; separate
  from delivery `note` and `turn_note`

Roles, agents
- The built-in coordinating role has ID `role_leader_default`, key `leader`,
  and label `Leader`. Migration 102 renames the former `lead` role and its
  agent and spawn references. A global custom `leader` role is first moved
  to the next available `leader-v1` key (or numbered variant), with its body
  and agents retained. Project-scoped `leader` rows remain unchanged and are
  logged. There is no `lead` alias for the built-in role.
- **role**(id, key UNIQUE (legacy internal key), project_id? (always NULL since migration 64), role_key,
  label, emoji, body (editable instructions), extends (role_key of a parent
  role, '' for none), cardinality[multi|singleton],
  cap_is_lead, cap_can_spawn, cap_singleton_writer, spawn_defaults_json,
  spawn_pick_mode[in_turn|randomly], rev, deleted_at?) — active
  public `role_key` is unique. `cap_singleton_writer` is a legacy field with no
  effect on checkout access and is hidden in the role editor. Roles are global only. Deletion preserves historical agent
  references and hides the role from new selection; stopped agents with a
  deleted role cannot reactivate. Existing agents retain their `role_id` until
  respawn or explicit role change. Editing a row's body affects its agents on their next
  delivery. The instructions delivered are the role's *resolved* body: the
  parent's resolved body, a blank line, then the role's own (trimmed); no cycles, at
  most five ancestors, and a role other roles extend cannot be deleted. A built-in
  global role (id `*_default`) can be reset to its shipped definition by the host
  owner or an admin.
  Capability and cardinality changes are rejected while live agents use the
  row; `cap_*` changes require the host owner or an admin (roles are
  global, and for a global scope the capability flag is not consulted: any
  global role edit already needs the owner or an admin).
  `spawn_defaults_json` stores at most 32 ordered `{launcher_id,model}` pairs
  listed by detected harness models when saved. Empty means manual choice. A
  removed model or unavailable launcher is skipped at spawn.
- **project_role_spawn_default**(project_id, role_id, items_json,
  pick_mode[in_turn|randomly]) — a row replaces the role's global list and mode
  for one project; no row inherits both.
- **role_spawn_cursor**(project_id, role_id, next_position) — the next in-turn
  position, advanced in the successful spawn transaction after skipped
  unavailable choices. A random pick does not change the cursor. A competing
  pick reports `role_default_changed` and may be retried with the same key.
- **role_spawn_warning**(room_id, role_id, last_warned_at) — throttles the
  room-local unavailable-default warning to once per role per hour without
  scanning message bodies. These role tables and columns arrive in
  migration 77.
- **prompt_override**(id, project_id?, key, value, rev, updated_at) — an
  owner-edited replacement for one shared prose sentence of an adapter's turn
  prompt (keys are the slots of `internal/adapter/prompt`: `identity`,
  `retry_notice`, `inbox_ask`, `mentions`, `human_reply`, `tail`), global
  (`project_id` NULL, host owner or an admin) or for one project (its owner or operators);
  unique per (scope, key); value 1–2000 characters. Resolution at dispatch:
  project row, else global row, else the shipped text, carried in the turn
  context as `prompt_overrides` (only the slots that have one). The output
  contract, the response schema and every adapter-specific part are not slots.
  The global setting `inline_plan_appendix` (`optional` default, or `always`)
  adds one sentence to a lead's delivered instructions asking it to attach the
  full plan as `appendix_pages` with a visual plan.
- **agent**(id, slug, project_id, role_id, recovery_block bool, tool, variant,
  launcher_id?, model, provider,
  machine_id, checkout_id, home_room_id?, key_hash, revoked_at?,
  desired[active|not_accepting|stopped], desired_rev,
  observed[idle|in_turn|interrupting|stopped|lost], generation,
  launch_path (default empty), removed_slug (default empty, migration 93),
  created_by, created_at) — **one role per agent**: one agent cannot hold
  different roles in different rooms; `@role` resolves to room members whose
  agent role matches; `home_room_id` is set on the first non-DM agent join and
  limits room bulk control to that agent's current home membership; `generation`
  is a **session** counter; `launch_path` (migration 87) is the spelling of the
  checkout's folder a Claude Code session was saved under (a link to it), when it
  differs from the canonical `checkout.canonical_path`: the harness is started
  there (`launch_path` on the delivery), and it is ignored once the agent is not a
  Claude agent and cleared when a switch moves it to another checkout
  (a removed agent — `DELETE /api/agents/{id}` — is revoked; its `slug`, unique within
  the project, is rewritten to a tombstone so the name is free, and `removed_slug`
  keeps the name its messages are shown under)
- **session**(id, agent_id, generation, native_session_id?,
  native_session_started_at?, start_mode[native_resume|fresh_with_history],
  started_at, ended_at?)
  Stopping and starting an agent increments the generation but carries the
  latest native session handle into a `native_resume` session when the harness
  has reported one. An explicit Start fresh creates `fresh_with_history`.
  A missing or broken native session also starts fresh. The first turn of a
  fresh session receives up to 40 recent messages from rooms the agent is in,
  newest selected by message sequence and shown in chronological order;
  imported messages and the agent's own messages are included. Each body is
  limited to 1,500 characters. These are prior context, not new requests.
  If starting needs folder or machine approval, the pending request retains
  the explicit fresh choice until it is approved.
- **agent_switch**(agent_id UNIQUE, room_id, tool, variant, launcher_id?, model,
  provider, machine_id?, checkout_path?, state[draining|starting|failed],
  error, requested_at) — durable target for an in-place tool, model, or
  machine change; an omitted machine target retains the agent's machine

Machines and workspace
- **machine**(id, name, key_hash, approved, is_host, revoked_at?, last_seen?,
  supervisor_version?, journal_id?, capabilities_json, harnesses_json) — the host machine is
  created approved at bootstrap; `journal_id` records the last accepted
  supervisor journal identity. The capability reports from the most recent
  `hello` are stored here and matched when admitting a turn. Only one machine
  is the host; remote machines start pending and require owner approval.
- **machine_harness_setting**(machine_id, tool, variant, enabled,
  override_path) and **machine_harness_activity**(machine_id, tool, variant,
  actor, old/new enabled and path, created_at) — owner settings and their
  append-only audit history. The supervisor validates executable paths before
  a setting is persisted.
- **machine_pairing**(token_hash, name, created_by, expires_at,
  redeemed_at?, machine_id?) — a ten-minute, single-use pairing token. Its
  redemption creates a pending machine and returns a private key once.
- **project_machine_folder**(project_id, machine_id, folder) — the remote
  machine's path for a project. The host uses `project.folder` directly.
- **project_machine_allowed**(project_id, machine_id, approved_by,
  approved_at) — owner admission of a remote machine for one project; the
  host is implicitly allowed. Migration backfills machines already hosting
  agents in their mapped folder. A new remote mapping alone does not admit
  agents; changing the mapped folder clears admission, withdraws pending
  approvals against the old mapping, and opens replacement requests.
- **agent_boundary_grant**(agent_id, machine_id, canonical_path, approved_by,
  approved_at) — one-time owner approval for that agent at that exact folder
  on that machine. It does not grant another agent or another machine.
- **boundary_pending**(inbox_id, agent_id, machine_id, canonical_path,
  operation[spawn|switch|reactivate], mapped_root, prior_desired, requester,
  state) — one open admission request per agent, linked to an ordinary
  approval inbox item. Approval checks that the machine's mapping still
  matches the requested mapping and preserves a prior pause on spawn.
- **room_worktree**(room_id, base_ref, base_commit, branch_name,
  relative_path, state[pending|ready|failed|preserved|removed], reason,
  dirty_base_count) and **room_worktree_instance**(room_id, machine_id,
  canonical_path, base_commit, dirty_base_count, state, reason) — a task
  room's opt-in checkout and each
  machine's independent Git worktree. No row means the project folder.
- **agent_worktree**(agent_id, room_id, machine_id, name, branch_name,
  canonical_path, base_ref, base_commit, dirty_base_count,
  state[ready|preserved|removed]) — an agent-specific Git worktree created by
  an earlier release. Agents may share a room checkout; no writer slot is
  required. Existing worktrees remain available until explicitly removed.
- **machine_launcher**(id, machine_id, tool, name, label, command, args_json,
  env_refs_json, enabled, containerized, agent_server_url?, path_maps_json,
  visible_roots_json, supports_toolbox, preset) — the **harness** a machine
  offers (the product calls it a harness; the table keeps its name because
  agents, switches and dispatch reference it): one unique name per tool and
  machine, an owner-facing `label`. Owner configuration and stable ID select
  execution; `variant` is a compatibility alias. Environment references store
  variable names only. **machine_launcher_activity** records owner changes
  (`save`, `delete`, `models`, `providers`, and `model_default` when an agent's removed model
  was replaced by the default). Nothing is seeded: a new machine has no
  harness. Migration 69 deleted the seeded launchers that were disabled and
  unused, cleared `preset`, set labels and copied the old per-tool model list
  into each kept harness; migration 28 had carried forward Docker choices and
  sampled `AIMEBU_ALLOW_NATIVE=1` once. `docker` is a reserved name (an alias
  for `harness-docker` in older API requests). Active or stopping agents
  (including revoked agents) and in-flight switch targets block edits and
  removal until their stop or switch is observed.
- **harness_provider**(launcher_id → machine_launcher ON DELETE CASCADE, id, name,
  npm, base_url, key_env, context_tokens, output_tokens, position) — PRIMARY KEY
  (launcher_id, id). A custom OpenCode provider: an OpenAI-compatible endpoint whose
  models are ordinary `harness_model` rows with the slug `<id>/<model>`. `npm` is
  always `@ai-sdk/openai-compatible` (OpenCode installs a provider's package at run
  time, so nothing else is accepted). `key_env` is the NAME of the environment
  variable the key is read from on the machine that runs the turn (empty: no key);
  **no key or key value is stored anywhere**. Plain `http://` is accepted only for
  loopback, `host.docker.internal` and private-network hosts. The list is replaced
  whole, with the same revision check as models (`stale_providers`); dropping a
  provider that models still use is refused (`provider_in_use`) unless its models
  are removed in the same transaction (the refusal lists every such provider, `in_use`). Changes are logged as `providers` in
  `machine_launcher_activity`. Migration 91.
- **harness_model**(launcher_id → machine_launcher ON DELETE CASCADE, slug,
  label, note, is_default, position, source, available) — PRIMARY KEY (launcher_id, slug); a
  partial unique index allows one default per harness. The slug is the exact model
  id the harness is started with (never an alias). A non-empty list has exactly one
  default. `source` is `manual` for old rows and hand-added models, or
  `detected` for models found by a harness listing. The one-click reload detects and saves, removing missing detected
  models while retaining manual ones. A detected model still used by an
  agent remains as `available=0`: it stays in that agent's configuration and
  is hidden from the harness list and new spawn choices. A manually removed
  model still falls back to the default on next dispatch, updating the agent's
  `model`. Spawn and switch accept only available models (an omitted model
  with a harness means its default). The old global
  `spawn_model` table is gone. Migration 69 copies the old per-tool list into every
  kept harness (its first entry the default) and adds, after them at position 1000
  and up and never as the default, each model an existing agent or pending switch
  uses that the list lacked, so that no live agent changes model; a harness that
  would only have those gets its first as the default. Migration 104 adds
  provenance and availability, treating all existing rows as manual. A tool whose launchers would
  all be deleted (disabled, unused, seeded) keeps one disabled launcher (the host's
  native one, else its first by name) when its list differs from the seeded one in an id, a label or its length,
  so an edited list survives; a list that is dropped is written in full to the log.
- **checkout**(id, project_id, machine_id, canonical_path, writer_agent_id?,
  writer_state[none|reserved|releasing]) — UNIQUE (project_id, machine_id,
  canonical_path); container mounts map to the host canonical path so
  aliases within a project resolve to the same checkout. Different projects
  may use the same physical folder and retain separate checkout records.

Execution
- **note**(id, agent_id, message_id, room_id, state (§3.1), wake_reason,
  created_at) — UNIQUE (agent_id, message_id)
- **turn**(id, agent_id, session_id, generation, room_id (originating),
  parent_turn_id?, attempt, mode[normal|correction|diagnostic],
  exec (§3.2a), outcome (§3.2b), settle[pending|settled], cancel_intent,
  terminal_event, report_json?, policy_json?, started_at?, ended_at?)
  — at most one turn per agent with exec in (pending, running)
- **turn_note**(turn_id, note_id)
- **outbox**(id, turn_id, answer_rev, idx, agent_id, room_id, body,
  visual_plan_json?, appendix_pages_json?,
  reply_to?, thread_root?, ask_json?, needs_attention,
  proposed_answers_json?, open_questions_json?, attachments_json?, digest,
  state[proposed|validated|invalid|committed|held|superseded],
  held_reason?[invalid_after_correction|room_closed|not_member|
  stale_generation|correction_unavailable], dedupe_key UNIQUE)
- **receipt**(id, turn_id, answer_rev, digest, result_json, created_at) —
  UNIQUE (turn_id, answer_rev); the server's durable answer acknowledgement
- **request_key**(actor_kind, actor_id, kind[spawn|team_spawn|control|
  scope_control|switch|inbox_answer|recovery_decision|room_create|room_close|
  schedule_create|leaderboard_start|dm_agent|dm_human], key,
  request_digest, result_json, created_at) —
  UNIQUE (actor_kind, actor_id, kind, key); same key + same digest → original
  result; same key + different digest → `conflict`. The `kind` column has a
  CHECK constraint (migration 52 rebuild, extended with `dm_agent` by migration
  81 and `dm_human` by migration 87) listing exactly the values above, so a new kind needs a migration. `team_spawn` is reused by keyed team-flow
  migration requests; `leaderboard_start` is retired (the leaderboard rows
  were deleted by migration 56). The kinds written today are spawn, team_spawn,
  switch, scope_control, control (agent lead tool), room_create, room_close,
  schedule_create, inbox_answer, recovery_decision, dm_agent (a lead's private
  message to an agent) and dm_human (an agent's private message to a person of
  its project).

Inbox, usage, events, settings
- **inbox_item**(id, project_id, room_id, message_id?, turn_id?,
  agent_id?, requester_kind, requester_id,
  kind[question|approval|recovery|delivery_failed|usage_info|usage_limit],
  options_json?, state[open|resolved|withdrawn|superseded], rev, answer_message_id?,
  resolved_by?, resolved_at?, created_at) — `assignee` returns in slice 4.
  `usage_alert` is not a stored kind: the inbox listing appends it as a
  virtual row for each open provider usage alert (built in
  `internal/server/inbox.go`, shown to admins), with no table row behind it.
  An open agent `ask` question or approval becomes `superseded` when a later
  message from a person or that asking agent commits in the same room; system
  messages and messages in other rooms do not supersede it. Supersession and
  the new message commit in one transaction, and a superseded item rejects an
  inbox answer like any other closed item. Migration 117 applies this rule
  once to existing open agent asks with a later qualifying room message.
- **usage_record**(id, source_event_id UNIQUE, turn_id, agent_id, room_id,
  project_id, tokens_in?, tokens_out?, tokens_cache_read?, tokens_cache_write?,
  context_used?, context_window?, cost_est?, cost_total?, model?, partial,
  source[measured|estimated|unavailable], created_at) — `cost_est` is what one
  turn cost and is the only cost anything sums (limits, cards, totals). Claude
  reports a resumed session's whole spend in every result, so the store keeps
  that running total in `cost_total` and prices a turn as the difference from
  the native session's previous total, including across a stopped/start
  generation boundary (a fresh session's first report is its own cost; a
  total that went down is an unknown cost, never zero). `partial` marks what a
  turn spent before it ended without a report (killed, crashed): Claude gives
  its input and cache tokens only (its output tokens and cost stay unknown),
  opencode its steps' totals, and vibe the session's totals less what they were
  before the turn, with Vibe's own cumulative cost as for Claude (a resumed
  session whose earlier totals cannot be read reports no tokens, only the
  cumulative cost; a killed turn that left the totals unchanged reports nothing).
  Codex uses the dated model price list when its model
  and token counts permit; those rows have `source=estimated`, while a cost
  reported by a harness has `source=measured`. `model` is the one the harness
  named, else the agent's.
  Indexed by
  (agent_id, created_at), (room_id, created_at), (project_id, created_at) and (turn_id) (migration 75),
  with turn(room_id, started_at) and room(project_id), so the limit checks, the
  agent card and the profile never scan the table; a test asks SQLite for
  each plan
- **event**(id, seq UNIQUE, kind, project_id?, room_id?, agent_id?,
  turn_id?, payload_json, created_at) — same transaction as the change
- **settings_project**(project_id, max_live_agents=10, spawn_rate_per_min=6,
  auto_resume_after_clean_diagnostic=false, memory_enabled=false,
  worktree_mode='auto', worktree_auto_since, home_lead_only=true,
  reviewer_agents='inherit'); **global_setting** holds reviewer_agents_enabled
  (absent means on); **cleanup_log**(id, seq, category, target, project_id,
  reason, bytes, action, actor, created_at) (the cleanup log: what each cleanup
  removed and why; also `attachment.removed_at`, `removed_after_days`)
- **seq_counter**(id=1, value) — internal sequence allocator, seeded in
  migration 1; `schema_version` records migrations only.
- **room_member.auto** (migration 72): 1 when the membership is only the lead
  being put into a project room (a new room, a newly spawned lead, the backfill):
  quiet (no join event, nothing un-hidden), addressable, and not woken by
  unaddressed human messages. Joining the room on purpose (spawn, the lead
  creating it) makes it 0. Private chats and closed rooms get no automatic
  lead. Reopening a room joins the
  project's leads then. An automatic member has full member rights (closing and
  reopening rooms, sub-rooms, recall scope): the lead manages its project's rooms.
- **Message references from agents** are project-wide on purpose: an agent may
  read, by number, any message of any room of its own project (rooms it is not
  in and archived ones included; a private chat only as a member), for
  `referenced_messages`, `message_get`, and paginated `room_history`. Another project's messages never,
  except a chat between agents the agent is a member of (it belongs to no project, so its text is readable by its two agents and its people only). `list_agents` lists only agents in the caller's project. Only an active lead may use `set_role`; it cannot assign the lead role or change a lead, requires the target to be idle or stopped and the current desired revision, preserves role cardinality, and refuses reviewer-lineage roles while reviewers are disabled.
- **message_no_counter**(id=1, value) — the message-number allocator
  (migration 71); `message.no` is unique where it is positive.
- **room_view_preference**(user_id, room_id, hidden 0|1, notify[default|all|
  mentions|none], muted_until?) — a person's per-room view and notification
  setting (primary key user_id + room_id); `notify` and `muted_until` (an
  instant in the future) arrive in migration 73; `pinned` was dropped.
- **room_read_cursor**(user_id, room_id, last_read_seq) — how far a person has
  read a room (migration 43).
- **agent_room_seen**(room_id, agent_id, seq) — the newest message of a room
  the router handed an agent with nothing for it to do (not addressed to it,
  system chatter, or an unaddressed message for the worker while it is the
  lead put in the room), kept as it is routed, and only for an agent that is
  not stopped (a stopped agent saw nothing after it stopped); one of the
  inputs of the agent's seen marker (`GET /api/rooms/{room}/read-receipts`),
  which reads it as at most the message before the agent's oldest queued or
  dispatched note in the room, so chatter after a waiting note does not carry
  the marker past it. Primary key room_id + agent_id; migration 96 creates it
  and fills it from the existing `message_not_woken` events.
- **user_preference**(user_id, key, value, updated_at) — a person's interface
  preferences (theme, notification sound, sidebar project order), following
  them across devices (migration 57).
- **macro**(key, body) and **macro_default_seen**(key) — the editable quick
  commands and the shipped defaults already offered (migrations 37 and 40).
- **sound**(id, name, ext[mp3|wav], size, created_by, created_at) — uploaded
  notification sounds, bytes under the server config root (migration 44).
- **quota_switcher_config**(tool[claude|codex], config_json, updated_at),
  **quota_switcher_state**(tool, state_json, updated_at) and
  **quota_switcher_decision**(id, tool, at, decision_json) — the automatic
  provider account switcher's per-tool settings, current state and decision
  log (migration 48).

Project memory starts disabled and saved records remain readable to humans
when disabled. Owners/operators and current project agents can write project
memory when enabled; owners/operators and current task room agents can write
task notes. Viewers may read but not write. Each record stores its last
author and revision; every edit emits an event with actor and revision.
Updates and deletes compare revisions atomically. Body storage is capped at
4096 UTF-8 bytes per scope and 2000 Unicode characters per record.
Agents can list, revise, and remove project facts and joined task notes.
`memory_recall` returns `memory_disabled` while project memory is off. A
restrictive room override excludes that room's messages and notes from recall;
source-linked writes from that room fail, while unsourced writes remain
available in an enabled project. `PUT /api/rooms/{room}/memory-enabled`
requires `enabled`: `false` sets the restrictive override and explicit `null`
clears it; an omitted field is invalid. The first ordinary agent turn carries
the current enabled project facts, active global knowledge, and joined task
notes.

### v1 history import (migrations 25, 31, 37, 38, 50, and 51)

On a same-root first start, the server backs up the v1 layout under
`.old-v1-bkp/` before creating the owner. It reads the copied v1 history
read-only and reserves the highest v1 message ID in the shared sequence
counter before owner or host setup can append events. The first owner sign-in
opens the Review → Check → Import wizard when a backup has rooms. The owner
imports during setup or chooses Skip import forever. A durable
`import_v1_choice` row closes preview, dry run, and commit after either choice;
there is no Settings or normal CLI entry. The old CLI path is available only
with `AIMEBU_DEV_IMPORT_V1=1` for development tests.

The mapping assigns rooms to projects, v1 humans to v2 users or historical
names, and memory scopes. The dry run is a rolled-back transaction and
returns a digest; commit requires that digest. Maintenance pauses writes and
dispatch while copying files and inserting rows. Imported messages keep their
v1 numeric IDs as string `message.id` values and the same integer `message.seq`
shown after `#` in the UI. Reply links, attachment ownership, reactions, and
memory source links use those IDs. An overlap with an existing v2 event number
fails clearly; native writes continue above the highest source message ID.

Imported messages use native author kind and ID, harness and model, targets,
attention, proposed answers, open questions, visual plans, and appendix pages.
System messages and DMs remain ordinary native history. Unknown v1 message
fields and nonempty source tables without an importer appear in the dry-run
`not_carried` report and stay only in the backup. Migration 50 removes the
old message payload, author-label, attachment raw-metadata, and memory payload,
legacy-source and pending-review columns from existing v2 databases,
including databases that already hold imported rows. Historical messages do
not wake agents or create inbox items. Imported memory is native: active records with `source_message_id` set to the
kept v1 message ID when that message was imported.
Migration 50 activates imported records except those the owner deactivated (the record of that choice is the
latest `global_knowledge_updated` event with `active:false`). Migration 51 repairs databases that ran an earlier
migration 50: it switches off any global record whose latest such event is a deactivation but which is active;
it changes nothing on a database whose records agree with their events.

A marker is synced after commit. If a crash leaves committed ledger rows but
no marker, the pending wizard can resume its saved mapping. Once the marker
and durable choice say done, import is closed. The backup remains on disk.

The planned `spawn_code` table is not implemented. Boundary admission uses an
`approval` inbox item; membership grants and usage caps are described above.

## 3. State machines (who writes: S = server, M = supervisor report)

### 3.1 note
`queued → dispatched → in_turn → done | aborted | dropped | unknown`
- Created `queued` atomically with the triggering message whenever the agent
  is eligible (§6), **regardless of pause/busy/recovery**; gates are checked
  at dispatch, not at creation.
- `dispatched` (S, on M `ack persisted`); `in_turn` (S, on M `ack
  accepted`); `done` (S, when the turn's outcome is final and settled);
  `aborted` (S, cancelled before acceptance or Abandon); `dropped` (S, agent
  left the room / revoked / retired, with event); `unknown` (S, recovery).
- Recovery transitions: `dispatched → queued` **only** when the `hello`
  journal exchange proves the turn never started (otherwise → `unknown`);
  `in_turn → unknown`; `unknown → queued` (Retry: new immutable turn,
  attempt+1, linked to the original note set); Resume: a new continuation
  turn, notes → `done` **only when that continuation completes**;
  `unknown → aborted` (Abandon).

### 3.2 turn — execution, outcome, settlement are separate
a) **exec** (M-reported, S-stored): `pending → running | killed`;
   `running → exited | killed | lost`. `pending → killed` records cancellation
   before supervisor acceptance and frees the active-turn slot.
   `lost` = no evidence the process ended. `lost` resolves only by
   a `hello` whose journal shows the process ended (then classify) or an
   owner override (→ `unknown` → recovery; the override UI states that an
   old process may still be running) — **no timeout**, so a second
   process is never started silently. `lost` is shown in the UI, never an
   invisible limbo.
b) **outcome** (S-decided): `none → completed | partial_failed | failed |
   cancelled | unknown`
   - `completed`: terminal event with a final answer (later committed
     **or** held — validity is the settlement axis, not the outcome).
   - `failed`: terminal error and no possible effect, proven mechanically
     (mode verifiably effect-free) — never inferred from absent events.
   - `partial_failed`: terminal error after effects were possible → treated
     like `unknown` for recovery, but with the terminal evidence attached.
   - `cancelled`: cancel_intent and no possible effect.
   - `unknown`: no terminal event while effects were possible.
c) **settle**: `pending → settled` when every outbox row of the latest
   answer_rev is `committed` or `held` (zero items still settles via the
   receipt). Note `done` requires `settled`.
d) A **correction** turn has its own revision-1 receipt and settles its parent
   turn's pending output. A **diagnostic** turn's validated JSON is stored
   without a surrounding Markdown fence in `turn.report_json` and shown in
   the recovery inbox item — it is never
   delivered into the room as ordinary task messages.

### 3.3 outbox (per answer revision)
`proposed → validated | invalid | held`, then all-or-none:
- all `validated` → re-check room lifecycle, membership and ask
  permissions **inside** the commit transaction → `committed` in that one
  transaction with the message rows,
  inbox items derived from `ask`, wake notes, events and the receipt;
- any `invalid` → if the adapter capability `no_action_mode` is true, one
  correction turn (`mode=correction`, tools disabled, parent_turn_id set,
  answer_rev+1; **all** previous revision rows, including already-validated
  rows, → `superseded`); else, or if the
  correction is invalid too → all rows `held` + `delivery_failed` inbox item
  with a bounded, redacted candidate preview. An open originating room also
  receives a system notice for answers held during validation. When the answer
  was held because no correction could be requested (`no_action_mode` false),
  the author also gets a system note "Your previous reply was held: <reason>"
  (`OnlyTargets` the author; parser errors that quote the agent's text are
  described, not echoed). The note is written once per held receipt (a replay
  of the same digest returns the stored receipt). It wakes the author, so a turn
  that was itself woken by such a note and is held again gets no second note:
  a repeating invalid reply ends in the delivery-failed card, not a loop. A
  committed reply clears the author's earlier open delivery failures only for
  its own parent turn and for the rooms it actually posted a message to.
- `room_closed` / `not_member` targets are authorisation/lifecycle
  failures, not formatting errors: never re-asked (the model must not
  reroute content); the whole batch is `held` with the reason and raises a
  delivery-failure inbox item; release or retry needs explicit human
  authorisation. Closing a room can move a still
  `proposed` row directly to `held(room_closed)`. Correction is only for
  repairable formatting/reference errors.
- Dedupe: same dedupe_key + same digest → return the original receipt;
  same key + different digest → `conflict` error, nothing written.

### 3.4 agent — desired (admin) vs observed (execution)
- **desired** (S, from UI/lead/API): `active→not_accepting`,
  `not_accepting→active`, `active→stopped` (direct Stop),
  `not_accepting→stopped`, and `stopped→active` (Reactivate). No other
  edges. Every change bumps `desired_rev`; supervisor applies only the
  newest rev.
- **observed** (M): `idle→in_turn`, `in_turn→idle`,
  `in_turn→interrupting`, `interrupting→stopped`, `idle→stopped` (immediate
  stop), `stopped→idle` (Reactivate), `in_turn→lost`,
  `interrupting→lost`, and `lost→idle|stopped` only after reconciliation.
  A hello journal that proves termination, or an owner override that first
  classifies the turn outcome as `unknown`, resolves `lost` according to the
  current desired state. There is no `idle→lost` edge.
  Process-per-wake: `idle` with no process is the normal resting state and
  **never** implies stop.
- `stop` with no running **or pending** turn, after any dispatch/start
  uncertainty is reconciled with the supervisor → desired=stopped,
  observed=stopped immediately. With a running turn → interrupt sent, observed=interrupting
  → stopped on M `process{exited}`; kill after timeout (default 30 s).
- Admission (supervisor-side, serialised with control application): a new
  turn is admitted only when `deliver.desired_rev` **equals** the latest
  desired_rev the supervisor has applied, desired=active, and no other turn
  is pending/running for the agent; admission is journaled (`turn_start`)
  before the process starts. A repeated turn_id returns its recorded state
  and never starts again. `not_accepting` is a **drain**: already-admitted
  work runs to completion; `stopped` interrupts it. Socket order alone is
  not relied on across reconnect.
- Human- or close-requested interrupt with recorded termination → outcome
  `cancelled` if no effect was possible; otherwise `partial_failed` with
  the observed effects posted as a system message ("interrupted after:
  edited X, ran Y") and **no immediate inbox item** — the human caused it
  and gives the next instruction. Pressing Stop is not consent to replay:
  the next delivery to that agent carries the unreconciled effects as
  `recovery_evidence`, and any continuation of that work goes through
  reconciliation (§5), never a blind re-run. A requested kill without recorded termination is
  `unknown`.
- **Reactivate** (admin; named so it never collides with the recovery
  "Resume" button): desired → active; a new session that resumes the last
  known native handle bumps `generation`. Start fresh explicitly drops that
  handle and supplies recent room history on the first turn.
- **Switch model/tool** (owner/operator): an active or not-accepting agent
  enters a durable handoff wait. An in-progress turn finishes without
  interruption; no new ordinary turn starts. A dedicated normal-adapter turn
  with a switch-handoff marker then asks the outgoing agent for structured
  task, work-state, next-step, question, and caution fields. Its answer is
  captured by the server, not published as an agent message. The handoff turn
  has three minutes from dispatch; the entire wait has a 15-minute cap.
  If it cannot answer, the server marks the source automatic and builds a
  bounded summary from the selected room only. The old execution must be
  quiescent before generation changes. Continue without handoff is available
  while waiting; Cancel before the new session starts restores the old
  desired state. A room-local system message records the handoff or fallback.
  The fresh successor session receives that handoff and up to 40 recent joined
  room messages; a lead also gets the project handbook. The same agent ID,
  role, room memberships, and queued notes survive, and the generation rises
  by one. A pre-existing `not_accepting` state is restored after spawn.
  A failed switch can retry the same target. The switch cannot start from a
  stopped or recovery-blocked agent or a closing/closed room.

### 3.5 room
lifecycle `open → closing → closed → open`; gate `open ⇄ paused`.
- Close: persist lifecycle=closing (no new dispatch) → children closed
  first or cascade (confirmed) → room agents desired=not_accepting, running
  turns get ≤ 30 s to settle, then interrupt, then desired=stopped (close
  releases agents) → pending outbox for the room `held(room_closed)` →
  lifecycle=closed. `closing` is distinct from a manual pause.
- Reopen: lifecycle=open, gate follows the room's bulk control state; no
  automatic respawn.
- Closing a room never removes its worktree, and neither does anything else
  automatically (the daily cleanup, [cleanup](cleanup.md), only lists it in
  Health with its state; automatic removal of merged worktrees is planned
  after 2.0.0). The owner may request worktree removal from a live room; the
  request starts its normal archive and proceeds when closure finishes
  immediately. A clean worktree
  whose branch is merged into its recorded base ref is removed and its branch
  remains. Dirty, ignored (except aimebu's own `.aimebu/` adapter files), or
  unmerged content is preserved with a room notice. Transient failures retain
  ready state.
  Reopen restores the worktree from its retained branch. A user may
  explicitly remove preserved content from an archived room with a separate
  destructive confirmation, provided no active agent uses that checkout.
- Closing a room disables all its schedules in the close transaction;
  reopening leaves them disabled. The server dispatcher processes each due
  schedule transactionally before dispatch, including once at startup. It
  catches up at most one missed occurrence and advances directly to the next
  future occurrence. A delivered run is a targeted system message and normal
  wake note. Room/project pause, usage-limit gate, stopped/absent/revoked
  target, or closed room skips the due run. Skips are stored individually;
  repeated skips with the same reason post one room notice until the result
  changes. Each delivered run posts a room message naming creator, due time,
  and processing time. For local daily/weekly times, a spring DST gap shifts
  to the next valid instant, and a fall overlap runs once at its first
  occurrence.

### 3.5b project — live → archived → deleted
- Archive (owner or admin, who need not be a member): `archived_at` is set, the
  project gate closes (`gate=paused`, `gate_rev` +1) so nothing dispatches,
  every agent that is not stopped goes `desired=stopped` (the actor's
  membership is not required per agent: the owner or an admin decides for the
  project), its schedules are switched off, and its open question, approval,
  `usage_info` and `usage_limit` inbox items are withdrawn (recovery and
  delivery-failure items stay, to be resolved). Rooms and messages are
  untouched. Each agent is stopped in a savepoint of its own: one that cannot
  be stopped (its execution is unknown until a recovery decision, `lost`) does
  not keep the project live; it is listed in the answer's `stop_failed` and
  stays as it is until its recovery item is decided (below). Archiving an archived project changes nothing. Event
  `project_archived`.
- Read-only: while `archived_at` is set the server refuses, with `409
  project_archived`, every change a person makes through a project, room,
  agent, inbox or message route (`POST|PUT|PATCH|DELETE`), except reopening,
  deleting, archiving again, marking a room read, a person's own room
  notification preference, resolving a recovery decision or a failed delivery
  (`POST /api/inbox/{item}/decision|dismiss`), stopping or pausing one agent
  (`POST /api/agents/{id}/control`, whose `active` is refused inside the store),
  and removing a room worktree (so the project can be deleted). In an archived
  project a recovery decision can only be Abandon (any other choice, which would
  start work, is `409 project_archived`), and it ends with the agent stopped
  and its machine told, which counts as the stop confirmation delete needs.
  Reading is unaffected. Whoever may write to the project at all (a member, the
  instance owner, an admin) is refused with `409 project_archived`, admins
  included; anyone else gets what the route answers for an object they cannot
  see. A start through `POST /api/agents/{id}/control` is refused before the
  launcher, Docker and remote-folder checks. The transactions that start work
  check it too, so an archive that commits between a route's check and its
  transaction still wins: spawning into an archived project, or restarting one
  of its agents, fails with `project_archived`. Agents are stopped and the gate
  is closed, so no turn runs either.
- Reopen: `archived_at` is cleared and the gate returns to what the project's
  own control and daily limits say (`open` only when `control_desired=active`
  and no usage limit has paused it today, else `paused`). Agents stay stopped
  and schedules stay off. Event `project_reopened`.
- Delete (owner or admin; the body must carry the project's exact name, else
  400 before any archive): archives a live project first, then removes it in
  the same request when possible. A busy project remains archived for a later
  retry. Deletion is refused with 409 `project_busy` unless every
  agent that is not revoked is confirmed stopped (`desired` and `observed` both
  `stopped`) or sits on a revoked machine, which can never come back (an agent
  on an approved machine that is merely offline may still be running its turn,
  and would run on, unknown to the server, when the machine returns: bring the
  machine online or revoke it), and while a room worktree is not `removed` (an
  instance still `ready` or `preserved`). It
  deletes the project's rows in every table that depends on it (rooms
  including the home room, messages, reactions, attachments and their files,
  agents and their turns, usage, memory, schedules, task notes, inbox items,
  worktree records, members, machine permissions and folders, role spawn
  defaults, its events), and the rows that name the project with no foreign
  key (cleanup log, usage limits, scope control). Rows that point at the
  project through an optional column (project-scoped prompt overrides, roles
  and invites, whose empty project means "global", and events, whose empty
  project means the owner's) are deleted, never turned into global ones. A
  schema test fails when a table that names a project is in neither group.
  Files in the project folder, worktrees on disk and output logs kept on
  machines are not touched. The one event `project_removed` has no project
  (its project is gone) and names `user_ids`, the members it concerned, who are
  the people it is visible to (plus the instance owner).

### 3.6 checkout sharing
Agents in the same room may use the same checkout. Role cardinality still
limits singleton roles. The legacy writer-reservation columns remain in the
schema for compatibility but do not gate spawn, reactivation, role changes,
switches, or dispatch. Checkout identity includes the project, so projects
that use one physical folder do not share a checkout record.

### 3.7 inbox item
`open → resolved | withdrawn`, compare-and-set on (state=open, rev):
- Boundary admission is a distinct use of `approval`: only a project owner
  may choose Approve or Deny. Approve records the project-machine admission
  and the agent's exact canonical path grant in the same transaction as
  releasing the pending spawn, switch, or reactivation. Deny leaves the
  agent stopped or cancels the pending switch. A paused legacy spawn stays paused on approval. Stop, room close, and machine
  revocation withdraw pending admission. Request-key replay returns the same
  decision; a stale revision cannot release the agent.
- Answer: in one transaction write a **message** (author = user, reply_to =
  the ask message), set resolved + answer_message_id + resolved_by, bump rev,
  create the requester's note, emit event; idempotent by request_key.
- A message with needs_attention or answer controls creates one ordinary
  question inbox item. Proposed answers always allow free text; open questions
  collect one answer per question and add a free-text Other choice to each.
  The room and inbox use the same item and answer endpoint. An explicit ask
  cannot be combined with attention controls.
- Withdraw (requester or system): open → withdrawn.
- Close vs answer: closing a room withdraws its open question/approval
  items in the close transaction (an answer committed first wins). A
  recovery decision after close is stored as a control/audit decision
  (event) without posting into the closed room or reopening it.
- Authorisation: question = project owner/operator (assignee if set),
  ordinary approval = project owner; a private-chat human peer may answer
  either ordinary kind regardless of project role. Boundary admission stays
  owner-only;
  withdraw = requester, owner, or the system. Same request_key → original
  result; a different answer to a resolved item → `already_resolved`.
- Errors: `already_resolved`, `already_withdrawn`, `stale_rev`.
- Recovery items: **Retry** and **Resume** are the same mechanics — new
  turn, attempt+1, same note set (`unknown → queued → in_turn`), notes
  `done` only when that turn settles. Retry delivers the notes raw (and the
  UI warns it repeats the task); Resume delivers notes + evidence + report
  with "continue, do not repeat"; **Abandon** = notes aborted,
  recovery_block cleared; `desired` is untouched (recovery never changes
  the admin state, so a manual stop or room/project pause survives). A
  decision is rejected (`stale_rev`) if newer
  evidence or a newer turn exists.

## 3b. Supervisor journal (per machine, durable local SQLite)

Records (all keyed by turn_id + generation):
- `turn_start{turn_id, generation, desired_rev, note_ids, mode, started_at}`
  — written and fsynced **before** the tool process is started; the
  `accepted` ack is sent only after it.
- `activity{source_event_id, kind, summary, ts}` — appended as parsed.
- `process_exit{exit_code, terminal_event, cancel_intent, stderr_tail?, error_event?, ts,
  acked}` — replayed until the server acknowledges its durable process
  update. On a non-terminal exit, `stderr_tail` contains at most 4 KiB of
  launcher stderr with agent keys and bearer tokens redacted before the
  journal write. `error_event` holds the last Codex stdout `error` or
  `turn.failed` JSON event, or for Pi the last provider error it reported
  (`Pi provider error: …`, from `auto_retry_start`, a failed `auto_retry_end`,
  or an assistant message with `stopReason` `error`), or for OpenCode the
  `error` event it reported (`OpenCode error: APIError (HTTP 429): …`), or for Vibe
  the last `Error: …` line it printed on stderr (`Vibe error: …`) or the text of a
  `<vibe_stop_event>` message that ended the turn (`Vibe stopped the turn: …`), subject
  to the same limit and redaction. Pi's "creating a new session with that id" notice is dropped
  from `stderr_tail` because every fresh turn prints it. The server
  shows the exit code and available diagnostics in recovery evidence and the
  agent-card state tooltip.
- `answer{answer_rev, digest, candidate}` — written before sending `answer`;
  `candidate` is the exact raw final-answer text and `digest` is SHA-256 of
  its UTF-8 bytes.
- `receipt{answer_rev, result}` — on terminal receipt; then the answer
  payload may be pruned.
The journal stores a random `journal_id` created once at initialization and
preserved on reopen. A replacement journal gets a different ID.
Write ordering per turn: turn_start → (spawn) → activity* → process_exit /
answer → receipt. On reconnect the supervisor reports running turns (even
after a terminal answer receipt), unreceipted starts, and unacknowledged exits
in `hello`; a turn with no `turn_start`
record in an intact journal was never started (the only proof that permits
requeue). What each agent's
harness writes to stderr (not stdout) is kept on the machine that ran it, beside
the journal, for debugging (retention 14 d): see "Agent output" in
`docs/supervisor.md`.

## 3c. Provider accounts

- The host switcher stores named Claude Code and Codex file profiles under
  `~/.aimebu/switcher/` by default, sharing the existing v1 switcher tree
  after cutover. Account scope
  is host-machine-wide per tool across projects. Remote machines have no
  switcher profile set in this slice.
- Host-machine switching requires owner and shows the agents using the tool.
  A switch returns 409 with the running agents while any matching turn is
  active; the owner waits or stops those agents and retries. Remote-machine
  switching and a persistent dispatch gate are later work.
  An explicit forced switch requires stored credentials and skips the incoming
  token refresh; it keeps the ordinary turn guard and credential swap.

## 3d. Deployment

`aimebu server` runs the local supervisor in-process by default. It uses the
same versioned machine WebSocket and `supervisor/journal.sqlite` as the
standalone `aimebu supervisor` process, so reconnect and crash recovery use
one path. The server provisions an approved local machine and private key on
first start; the key file is mode 0600. `AIMEBU_EMBEDDED_SUPERVISOR=0`
disables the local instance for tests and remote-only installations.
Shutdown stops new dispatch and HTTP requests, interrupts local turns, waits
up to 40 seconds for their journal writes, closes the journal, then closes
the server store. On timeout, the remaining turn IDs are logged before the
journal closes.
An ungraceful process death still reconciles the durable journal on restart;
it never silently reruns a turn with uncertain effects.

## 4. Wire protocol (versioned, `v:2`)

### 4.1 Machine-level frames (machine key)
- M→S `hello{v, machine_id, journal_id, supervisor_version,
  supports_path_probe?, supports_directory_list?, supports_session_scan?, supports_session_fork?, supports_session_copy?, supports_legacy_session_copy?, supports_session_attribution?, supports_launch_path?, supports_worktree?, supports_launchers?, supports_providers?, supports_detect?,
  supports_detect_dir?, supports_agent_logs?, supports_agent_log_size?, supports_telemetry_config?, supports_agent_keys?, supports_warm?, held_keys?[agent_id],
  capabilities[{tool, version, environment, flags}], harnesses[...], agents[{agent_id,
  generation, open_turns[{turn_id, exec, process_alive?, has_answer, answer_rev,
  digest?}]}]}`
- S→M `hello_ack{v, desired[{agent_id, generation, desired, desired_rev}],
  redeliver[...], resend_receipts[...], harness_settings[...],
  harness_probe_dir?, harness_probe_dirs?, launcher_definitions[...], telemetry?}` — the optional probe directories are the
  project folders mapped to that machine (`harness_probe_dir` the first, `harness_probe_dirs` all of them in project order,
  each once, at most 8), used only for launcher version discovery: a container launcher answers only from a folder its
  container mounts, which the server cannot know, so the supervisor asks from each in turn until one answers.
- S→M `telemetry_config{v, telemetry:{enabled, dsn?, environment?, traces_sample_rate?}}`, and the same object as `telemetry` in `hello_ack`: the
  server's effective error and performance reporting settings (its environment, else the stored Settings once the owner answered the consent popup), sent
  when a supervisor connects and again whenever they change. It carries no identity of a person. A supervisor announces `supports_telemetry_config=true`
  in `hello` when it takes them, and only then is it sent the frame (an older supervisor does not know the frame type; it ignores the `hello_ack`
  field); the server's own embedded supervisor does not announce it, being the server's process. The supervisor starts, replaces or stops its own reporting
  by it, and sets the environment of the `agent-tools` bridge it spawns (see [Telemetry](telemetry.md#supervisors)). A DSN in the supervisor machine's own
  environment (`AIMEBU_SENTRY_DSN`, a DSN or `off`) wins over it.
- `hello_ack` also carries `agent_keys[{agent_id, generation, key}]`: a new `aak_` key for each agent the supervisor should run and, per its
  `held_keys`, holds no key for (it keeps keys in memory only, so a restarted one holds none). Issuing one replaces the agent's stored key hash, so the
  key the old process held stops authenticating. Only a supervisor that announced `supports_agent_keys=true` is sent any. `spawn` and `deliver`
  carry `docker_only` when the instance runs agents only in docker (see [the supervisor](supervisor.md)); a supervisor refuses a native launcher then.
- S→M `harness_set{v, req_id, harness_setting:{tool,variant,enabled,override_path}}`;
  M→S `harness_result{v,req_id,harness?,code?,message?}` validates and applies
  the setting on that supervisor. M→S
  `harness_status{v,harnesses[...],capabilities[...]}` reports effective rows
  and version-matched capabilities after reconnect settings are applied.
  Docker discovery keeps a PATH-found launcher available and skips version
  probing without a known mirrored directory; its version remains unknown
  until probed successfully from one. Spawn performs the checkout-specific
  launcher preflight. A successful Docker spawn can supply a newly mapped
  checkout for a fresh version and capability report.
- S→M `launcher_validate` then `launcher_set`, each with
  `{v,req_id,launcher_definition:{id,tool,name,command,args,
  env_refs,enabled,containerized,agent_server_url?,path_maps,visible_roots,
  supports_toolbox}}`; M→S `launcher_result{v,req_id,launcher?,code?,message?}`;
  M→S `launcher_status{v,launchers[...],capabilities[...]}`. The definition
  carries only environment variable names. Validation has no runtime effect;
  the server saves only a validated definition, then applies it. The
  supervisor resolves its own values, executable path and version, and probes
  the mapped checkout before spawn. A launcher ID binds an agent to its selected definition. Older
  supervisors do not receive launcher update frames.
- S→M `detect{v,req_id,detect:{kind:"harnesses"|"models",family?,command?,args?,dir?}}`;
  M→S `detect_result{v,req_id,harness_candidates?[{family,command,containerized,
  version?}],model_candidates?[{slug,label,note?}],code?,message?}` (hello
  carries `supports_detect`). `harnesses` lists the Claude, Codex, Pi, OpenCode and Vibe
  executables on PATH and the usual user-level install directories, once per real
  file; `models` runs the family's own listing command (`pi --offline
  --list-models`, `opencode models`, `codex debug models --bundled`; Vibe has none, so
  the supervisor reads the `[[models]]` of its Vibe config instead) on the
  absolute `command` with a 20 s limit and an 8 MiB output limit, for native and
  containerized launchers alike. `args` are the launcher's own arguments, placed
  before the listing ones. The default working directory is a fresh private
  `detect/run-*` folder under the supervisor's aimebu config directory, removed
  after the command when possible; leftover run folders older than one hour are
  removed before the next default-folder detection. A removal error is logged
  and does not fail a successful listing. The request's `dir` overrides it
  when given: an absolute directory that must exist on the supervisor's machine, else
  `code:"invalid_dir"`. A failure message includes the run path and ends with
  the last lines the command wrote to stderr (from its last 2 KiB). The server sends
  `dir` only to a supervisor whose hello carries `supports_detect_dir`; older
  supervisors ignore both fields. Detection only reports; the server returns the
  candidates to the owner and saves nothing.
- S→M `path_probe{v,req_id,checkout_path,launcher_id?}`; M→S
  `path_result{v,req_id,canonical_path?,code?,message?}`. The supervisor
  verifies an existing directory and resolves symlinks on its own machine.
  Launcher checkout probes run without blocking the machine frame reader.
  The server sends this frame only to supervisors that advertised
  `supports_path_probe`. Older supervisors fail admission without receiving
  the unknown frame; the server does not fall back to lexical comparison.
- S→M `directory_list{v,req_id,checkout_path,directory_prefix?}`; M→S
  `directory_list_result{v,req_id,directory_names?}`. The path is absolute;
  the supervisor reads only its immediate entries and returns at most 200
  matching directory names, including links only when their target is a
  directory. Hidden names match only a prefix beginning with `.`. Missing or
  unreadable paths yield an empty list. The server waits at most three seconds.
  It sends the frame only when hello advertises `supports_directory_list`;
  without the flag the field gets no suggestions, without an unknown frame.
- S→M `deliver` carries `fork_session` for the first turn of a copy of a Claude Code
  session: `native_session` is the original and `native_session_started` is true; the
  Claude adapter resumes it with `--fork-session`, and the turn's first `system`/`init`
  `session_id` that differs from the original is reported once as
  `native_session_started` (the copy's id). The server sends `fork_session` only to
  supervisors whose hello has `supports_session_fork`, checked on every delivery
  (a machine can reconnect with an older supervisor between spawn and the first
  turn): the dispatcher holds such an agent before preparing a turn and posts one
  system note in the room where its note is queued, and `Dispatch` refuses the delivery with
  `supervisor_update_needed`.
- S→M `deliver` carries `import_legacy_session` for a native Codex, Pi,
  OpenCode or Vibe agent when its v1 session must be
  copied into private runtime before resume. The supervisor copies only
  the selected session from read-only source state and keeps a marker for
  redelivery. The requirement survives stop and start until a turn on that
  resumed session exits and settles successfully; a repeated request after
  staging uses the marker.
  The dispatcher holds the agent when hello lacks
  `supports_legacy_session_copy`, and `Dispatch` checks the capability on
  each delivery so a supervisor downgrade cannot resume without the copy.
- S→M `deliver` carries `launch_path` for an agent started from a Claude Code session
  that was saved under another spelling of its folder (a link): the folder the harness is
  started in (`cmd.Dir` and `PWD`), because Claude keys sessions by the folder as spelled.
  The checkout stays the canonical path; the supervisor ignores a `launch_path` that does
  not resolve to exactly it. Sent only to supervisors whose hello has `supports_launch_path`,
  with the same per-delivery check and hold as `fork_session`.
- S→M `deliver` carries `providers[{id,name,npm,base_url,key_env?,context_tokens?,output_tokens?,
  models[{id,name}]}]` for an OpenCode agent whose model id starts with `<id>/` of a custom
  provider of its harness: the one provider the turn's model names, with the harness's models of
  it (ids without the prefix), and nothing for any other model. The supervisor holds no server
  settings, so the document travels with the turn. `key_env` is the NAME of the variable the key
  is read from on the machine; **no key value is ever in a delivery**. The model counts as the one
  the turn starts with: an agent whose model the harness no longer lists starts with the harness
  default. Sent only to supervisors whose hello has `supports_providers`: the dispatcher holds an
  agent whose next turn needs a provider before preparing the turn and posts one system note in
  the room where its note is queued, and `Dispatch` refuses the delivery with
  `supervisor_update_needed`.
- S→M `detect{detect:{kind: provider_keys | provider_import | models, launcher_id?, key_names?, providers?}}`,
  answered by M→S `detect_result` (`details`), is how the server asks a supervisor that says
  `supports_providers` about custom providers. `provider_keys` answers `{set:{NAME:bool}}`: whether each
  name has a value where the supervisor starts the launcher's turns (its environment or the launcher's
  environment references); only whether, and a name the harness uses itself is not answered.
  `provider_import` answers the custom providers the person's own OpenCode config defines
  (`{source, suggestions:[{provider, inline_key?}], skipped?}`), never a key: a `{env:NAME}` key becomes
  the name, a key written as text is only flagged and is skipped by automatic
  provider discovery. `models` for an OpenCode harness carries `providers`
  (with their models): the listing runs in the same private config as a turn and each provider's own
  endpoint is asked for its models; the answer's `details` carry `endpoint_problems`.
- S→M `session_scan{v,req_id,launcher_id?,scan_folders?[1..4] | scan_session_id?}`; M→S
  `session_scan_result{v,req_id,sessions?[{id,cwd,dir?,title?,git_branch?,version?,
  entrypoint?,model?,modified_at,size,active?}],unreadable?,scan_truncated?,code?,message?}`.
  With `scan_attribution:true` (and `scan_folders`, optional `scan_since` and `scan_agent`) the
  answer is `attributions?[{id,cwd,model?,sender,message_nos?,modified_at}]` instead:
  for each session of those folders written since then, the agent id its `bus_register`
  returned and the numbers its `bus_say`/`bus_dm` returned, read from the transcripts' tool
  results. Only those ids leave the machine, never a line of text (advertised by
  `supports_session_attribution`).
  With `scan_session_id` and `copy_to` (an absolute folder) it instead copies that session, the
  transcript and its own directory, under the Claude project directory of `copy_to`, so a session
  forked there is found from that folder; the answer has no `sessions`, and `code` is
  `session_not_found`, `session_unreadable`, `invalid_request` or `copy_failed` on failure
  (advertised by `supports_session_copy`: an older supervisor would ignore `copy_to`).
  The supervisor lists the Claude Code sessions of its own machine, from the
  configuration folder of the launcher named by `launcher_id` (that launcher's
  `env_refs` apply: its `CLAUDE_CONFIG_DIR`, else its `HOME`, else the
  supervisor's own; `code:"launcher_unavailable"` for an unknown launcher; empty
  means the supervisor's own environment): either
  every session whose recorded working folder is one of `scan_folders` (spellings
  of one folder: as configured and resolved) or below it
  (newest first, at most 200 sessions; the folder is decided first, from each
  transcript's own `cwd`, and the limits count what is left: at most 5000
  transcripts are opened and at most 200 long-named directories that do not
  start like the folder's are looked into, because Claude hashes long paths;
  `scan_truncated` says a limit was reached (also when long-named
  directories were left unopened because the 200 probes ran out); `unreadable` counts transcripts
  of the folder's directories whose first line carrying a working folder is
  beyond the first 1 MiB, so they could not be assigned), or the one
  session `scan_session_id` wherever it worked (`code:"session_not_found"`
  when no transcript carries the id, `session_unreadable` when it exists but
  has no readable working folder; `invalid_request` when both or neither
  are named). Each transcript is read only at its two ends (the first and the
  last 64 KiB): the working folder, branch, Claude version and entrypoint come
  from the start (`dir` is the project directory the transcript sits in, which
  Claude derives from that folder), the title from the last `ai-title` or `last-prompt` line
  (else the first prompt), cut to 120 characters on one line; no transcript
  text beyond that title leaves the machine. Subagent and sidechain
  transcripts and sessions with nothing said in them are left out. `active`
  says a live process registered the session in `<config>/sessions/<pid>.json`;
  it is advisory. The server sends this frame only to supervisors that
  advertised `supports_session_scan`.
- S→M `worktree_op{v,req_id,checkout_path,room_id,base_ref,
  worktree_action,worktree_force}`; M→S `worktree_result{v,req_id,
  details?,code?,message?}`. The supervisor runs bounded Git operations in
  the mapped project root and reports the canonical worktree path. The server
  sends this frame only when the hello advertised `supports_worktree`.
  Actions are `validate`, `ensure`, `remove`, `retire` (renames the kept branch of a removed
  worktree to `<branch>-removed-<n>`, so the name is free; refused while a worktree stands
  there; a supervisor that does not know it answers an unknown-action error, and the removal of the agent that asked stops there until the machine is updated), and `inspect` (read-only: size,
  uncommitted file count, merge state, head commit and last activity, used by
  the cleanup); typed Git error codes are preserved in `worktree_result`.
- S→M `agent_log_read{v,req_id,agent_id,log_lines?,log_max_bytes?}`; M→S
  `agent_log_result{v,req_id,agent_id,details?,code?,message?}`. The server asks
  the machine that ran an agent for the end of that agent's captured stderr,
  only for a project's owner or an admin who asked (`GET
  /api/rooms/{room}/agents/{agent}/output`), and only when the hello advertised
  `supports_agent_logs`. `details` is `{text, bytes, files, truncated,
  updated_at}`: at most `log_lines` lines (default 200, never more than 2000)
  and `log_max_bytes` bytes (never more than 256 KiB), cut at a line start;
  `code` is `no_log` when nothing was captured for the agent and `log_failed`
  when it could not be read. The frame names an agent id, never a path.
- S→M `agent_log_usage{v,req_id}`; M→S `agent_log_usage_result{v,req_id,
  details?,code?,message?}`. The server asks a remote machine how much
  agent output it keeps, for the Health disk report, only when the hello
  advertised `supports_agent_log_size`. `details` is `{bytes, agents}`: the
  bytes in the machine's `supervisor/agent-logs` and how many agents have
  output there; `code` is `no_log` when the machine keeps none. Only the size
  crosses the wire.
- M⇄S `ping/pong` every 15 s → `machine.last_seen`; offline after 60 s.
- S→M `spawn{v, req_id, agent_id, generation, desired_rev,
  agent:{id, tool, variant, launcher_id?, model, provider}, agent_key,
  checkout_path}` —
  authorised for this machine; the new agent channel is bound here.
  `req_id` is stable as `agent_id/generation` and is reused until a matching
  `spawn_result` confirms the request. The raw `agent_key` is held only in
  server memory for retries. After a server restart, a new key is issued;
  only its verifier is stored durably. The supervisor never writes the raw
  key to its journal or logs.

### 4.2 Agent channel frames (bound to agent_id + generation)
- M→S `spawn_result{v, req_id, agent_id, generation, result:"ok"|"error",
  message?}`
- S→M `control{v, agent_id, generation, desired_state, desired_rev}` — acked
  by `ack{level:"control", ref:"<desired_rev>"}` so the server knows the
  newest rev is applied. A stop ack waits until any running process exits.
  A supervisor advertises `supports_stop_replay=true` in `hello` when it can
  apply a stop control at the same revision as the desired snapshot. On
  reconnect, after `hello_ack`, the server sends
  one stop control for each agent with `desired=stopped` whose stop is not
  confirmed by `agent_control_applied` at that revision, or whose observed
  state is not stopped. The snapshot alone does not confirm application or
  start the control frame's kill fallback. A stopped agent on a machine that
  can reconnect keeps its project from deletion until the stop is applied.
  Older supervisors receive the stop replay too, with the stopped revision
  one behind in the snapshot so their equal-revision filter accepts the
  control and starts its kill fallback. An older supervisor's replay ack is
  not proof of application. An older supervisor acks a control when it sees it,
  not when the process has exited, and sends no later ack, so its ordinary
  live-control ack counts only if the agent is already observed stopped at that
  moment (an agent with no running turn is). The ack for a stop that interrupts
  a running turn arrives while the agent is still interrupting and is dropped:
  that stop stays pending, and deleting the project stays refused (409) until
  the supervisor is upgraded or the machine is revoked. The same holds for a
  stop missed while the machine was offline.
- S→M `deliver{v, agent_id, generation, turn_id, mode, parent_turn_id?,
  notes[{note_id, room_id, from, message_id, no?, body, reply_to?, thread_root?,
  attachments[{id,name?,mime,size,sha256}], why?, fyi?, in_reply_to?:{no, from,
  body, yours?}, conversation?:{no, from, body, yours?}}], attachment_bearer?,
  context{role_body, rooms[{room_id, title, kind}],
  you_are:{id,name,role,harness,model},
  team_roster:[{id,name,role,harness,model}],
  team_roster_more?:string, referenced_messages?:[{no, available, room_id?, room?, from?, body?, created_at?}],
  knowledge?:{project_memory_enabled, project_memory[], user_profiles[],
  agent_shared_notes[], task_notes[]},
  control_changes?, recovery_evidence?, correction_error?}, desired_rev,
  allowed_tools?, agent:{id, tool, variant, model, provider},
  native_session?, native_session_started?, import_legacy_session?}` — rooms in context list every current open agent room membership, including restricted rooms, with its title and kind; they are information, not grants. Closed rooms are omitted. In the dispatcher flow, the leader's role body names the live routing room title and ID. For pi agents on gemma models `knowledge` is trimmed to a
  shared 2048-byte budget of record bodies; later records are dropped. The
  attachment bearer is used only by Pi Docker, never journaled, and authorizes
  reads only for a delivered file in a live turn while the agent remains a
  member of its room; MCP agents use `attachment_get` with the same checks.
- M→S `ack{v, agent_id, generation, ref, level[persisted|accepted]}`
- M→S `activity{…, turn_id, source_event_id, kind, summary}`,
  `usage{…, source_event_id, tokens_in?, tokens_out?, tokens_cache_read?,
  tokens_cache_write?, context_used?, context_window?, cost_est?,
  cost_source?, cost_cumulative?, model?, partial?}`, `compaction{…,
  pre_tokens, post_tokens}`. `cost_source=estimated` marks a price-list cost;
  an omitted source means the harness reported it.
- M→S `answer{v, agent_id, generation, turn_id, answer_rev, digest,
  candidate}`. `candidate` is the raw final-answer string, including malformed
  output or an empty string. The digest covers its exact UTF-8 bytes. The
  server parses it into items during validation; items are not a wire field.
  Machine frames are capped at 4 MiB and final answers at 2 MiB. The
  supervisor sends `answer_too_large:true` with the original digest and no
  candidate when either limit would be exceeded, so the server records a
  receipt with an `answer too large` error instead of dropping the socket.
- M→S `process{v, agent_id, generation, turn_id,
  exit:{exit_code, terminal_event, cancel_intent, stderr_tail?, error_event?}}`
- S→M `ack{v, turn_id, level=process}` after the process update commits;
  the supervisor marks that exit acknowledged and stops replaying it.
- Frames whose agent is revoked, not bound to this machine, or of a stale
  generation are rejected for control/state; a stale-generation `answer`
  whose turn is journaled goes through reconciliation (§5).

### 4.3 Answer receipt
S→M `receipt{v, turn_id, answer_rev, digest, result[committed|held|
correction_requested|superseded|conflict], details}` — written durably before sending;
re-sent on hello. `details` carries `held_reason`, `error`, `message_ids`, and
`warnings`: advisory strings such as `message 1: 'worker:' prefix detected ...`, one
per message that opens with an IRC-style `name:` prefix or an inline
`a, b —` / `a and b:` form naming known room members, which addresses nobody
(use `@name`). Warnings never change the result. The supervisor may
delete its journaled answer only after
a **terminal** receipt (`committed`, `held`, or `superseded`, whose content the
server has durably retained). Revision 0 moves from nonterminal
`correction_requested` to terminal `superseded` when its correction turn
settles. `correction_requested` and `conflict` never permit cleanup.
`no_action_mode` is taken from the matching machine capability report at
turn admission and fixed in `turn.policy_json`; missing or ambiguous reports
leave it false.

### 4.4 Agent I/O (one canonical schema)
Final answer (from the adapter's final-answer event; native schema flag on
CC/Codex, prompt-only on pi):
`{"messages":[{"room":"…","body":"…","reply_to":"msg_…?",
"attachments":["att_…"]?,"ask":{"kind":"question|approval",
"text":"…","options":["…"]?,"free_text":bool}?,
"needs_attention":bool?,"proposed_answers":["…"]?,
"open_questions":[{"question":"…","description":"…"?,"options":["…"]}]?,
"handoff":bool?,
"visual_plan":[{"id":"…"?,"type":"…","title":"…"?,"data":{},
"order":0?}]?,"appendix_pages":[{"title":"…"?,"body":"…"}]?}],
"reconciliation":{"status":"clean|uncertain","findings":["…"]}?}`
- A message may use `proposed_answers` or `open_questions`, but not both.
  An `ask` implies attention: `needs_attention`, `proposed_answers` and
  `open_questions` sent with it are dropped, with a receipt warning, instead of
  invalidating the reply.
- One concept, the reply: a message that answers a delivered note sets
  `reply_to` to it (its `no` or message ID) and so joins that message's
  conversation; `thread_root` is derived, not part of the answer (one still
  sent is ignored). `fyi: true` marks a note that was not addressed to the agent (an
  agent's broadcast, §6). Each note says why it reached the agent (`why`: "a reply to your
  message #N", "you are mentioned", "a person's message to the room", "a
  person's reply in a conversation you started"…), and carries the message it
  replies to (`in_reply_to`) and the first message of its conversation when
  that is another one (`conversation`): number, author, a 500-character
  excerpt, and `yours:true` when the agent wrote it, so a reply is never an
  opaque ID.
- A reply wakes the author of the message it answers (agents behind the reply
  hop guard). A person's reply anywhere in a conversation also wakes the agent
  that started it, when that is not the author replied to (wake reason
  `conversation`); an agent's reply never widens this way.
- Default `reply_to` (server-side, conservative): when a normal turn was woken
  by exactly one note, written by a person in the turn's room, and the answer
  holds exactly one message to that room with no `reply_to`, no `ask` and no
  `handoff`, the server sets `reply_to` to that note's message and records a
  `reply_defaulted` event (`message_id`, `reply_to`) on the turn. Any other
  shape (several notes, several messages, another room, an agent's note) keeps
  exactly the link the agent wrote, or none; the server never guesses there.
- `handoff:true` addressed to the lead = task-result handoff (wakes the
  lead). `reconciliation` only in diagnostic turns.
- `visual_plan` and `appendix_pages` are message-scoped display content. The
  v1 block vocabulary is supported; Codex and Claude structured-output
  schemas encode each block's `data` object as a JSON string. Unknown or
  mismatched shapes are kept in the message and outbox, exposed as escaped
  raw content in the web UI, and named in receipt warnings. The server assigns
  a missing block ID and replaces input `order` with its array index. An
  imported v1 message uses these native fields. Diagrams load the exact v1 vendored Mermaid bundle
  lazily with strict security; failed diagrams show their escaped source.

  The `data` object uses the v1 vocabulary:

  | Type | Data keys |
  |---|---|
  | `markdown` | `markdown` (Markdown text) |
  | `file-tree` | `root:{name,type,children?,note?}` |
  | `data-model` | `entities:[{name,fields:[{name,type,notes?}]}]` |
  | `api-endpoint` | `method`, `path`, `request?`, `response?`, `notes?` |
  | `annotated-code` | `code`, `annotations?:[{line,text}]` |
  | `diff` | `diff` (patch text) |
  | `checklist` | `items:[{text,checked}]` |
  | `question-form` | `questions:[{question,description?,options?}]` |
  | `diagram` | `mermaid` (source text) |
  | `canvas` | `nodes:[{label,x,y,w,h}]` (percent positions) |
  | `prototype` | `screens:[{id,title,elements:[{text,x,y,w,h,target?}]}]` |

  Blocks are display-only. `question-form` is not an inbox request; use `ask`
  for a blocking human question. `appendix_pages` trail the blocks in one
  default-collapsed Full plan panel. Keep substantive decisions, evidence, and
  next actions in `body` too: agent turn notes append a readable text form of
  the blocks and appendix Markdown, capped at 16 KiB beyond the original body.
  A malformed block remains visible as raw content and produces an answer
  receipt warning when the browser would use its raw fallback.

- Bounds: whole answer ≤ 2 MiB, ≤ 10 messages, body ≤ 32 KiB each,
  ≤ 80 visual blocks of ≤ 64 KiB each (≤ 64000 data bytes), ≤ 160 runes per
  block or page title, and ≤ 10 appendix pages of ≤ 32000 body bytes each
  and ≤ 128000 body bytes total.
  Unknown top-level and nested
  `ask` fields are rejected. Unknown message-level fields are ignored only
  when the remaining answer is valid, with their names recorded as receipt
  warnings.
  These bounds apply to the raw candidate, including an optional single
  surrounding Markdown fence (`json` or unlabelled). The server removes that
  fence for JSON validation on every adapter path; the wire and receipt retain
  the exact original text. Diagnostic `report_json` stores the validated JSON
  without the fence. Prose outside the fence or a second fence remains invalid.
  A model that reasons in its reply text puts the reasoning ahead of the JSON as
  an element such as `<think>…</think>`. The server also removes any number of
  leading complete `<name>…</name>` elements (no attributes, any name, since the
  JSON never starts with `<`) before the fence handling, with a receipt warning
  that says how many; an element that never closes, one with attributes, and
  text before it stay invalid, and the wire and receipt keep the original text.
  Malformed raw output remains available for a held receipt or correction
  request. The authorized delivery-failure Inbox shows the redacted parse
  error and bounded candidate excerpt; the room notice shows only the fixed
  held reason.

## 5. Recovery

1. On `hello`, merge server turns with the supervisor journal by turn_id +
   generation; `lost` stays visible until resolved (§3.2a). Journal
   start/completion tombstones are retained after answer-payload pruning.
   The supervisor replays unacknowledged process exits and unreceipted answers
   after `hello_ack`; a durable server receipt proves the turn started but
   does not prove that its process exited. If no live process or journaled
   exit can be shown, the turn becomes `lost` and requires recovery.
   The server compares `journal_id` with the last accepted ID for the machine.
   A mismatch, or the first hello when the machine already has open dispatched
   turns, marks every such turn `lost`/`unknown` with recovery blocked and a
   visible `supervisor_journal_changed` event. Journal loss classifies
   uncertainty, never invites replay. A first hello without open dispatched
   turns records the ID.
2. Classify:
   - **intact journal has no `turn_start`** → never started → note
     `queued` (redeliver); a missing ack alone proves nothing;
   - journaled answer → reconcile by digest via receipt;
   - outcome `cancelled|failed` (no possible effect) → final;
   - operation-level idempotency contract → retry (no current adapter provides
     one; HTTP `request_key` replay does not make a harness turn safe to repeat);
   - else `unknown` / `partial_failed`.
3. `unknown`/`partial_failed` → agent `recovery_block=true` (dispatch
   blocker separate from `desired`) → if capability
   `readonly_verified` for (tool, variant): diagnostic turn
   (`mode=diagnostic`, shell/network/MCP/write disabled) producing
   `reconciliation`; then `recovery` inbox item — unless
   `auto_resume_after_clean_diagnostic` is on **and** status=clean **and**
   readonly_verified (store policy + actor in `turn.policy_json`).
   If not readonly_verified → straight to the inbox item. A `lost` turn also
   goes straight to the inbox even if read-only diagnostics are supported:
   its previous process may still be running, so a diagnostic could overlap
   it. The item states that no diagnostic was run and that resuming may
   repeat an action. A Resume/Retry decision creates a linked continuation
   but dispatch waits for positive quiescence evidence: the supervisor reports
   the old process gone in hello, or its exit/kill is recorded. Journal
   absence after replacement is insufficient. The owner sees a waiting item
   and may explicitly override the overlap risk; that risk and actor are
   recorded. An intact journal proving a continuation never started replaces
   it with another linked continuation carrying the same recovery context if
   the agent remains active. If Stop arrived first, its notes are aborted and
   the stop is confirmed.
7. In an archived project (§3.5b) the only recovery decision is Abandon: it
   ends with the agent `desired=stopped`, its machine told to stop it, and the
   notes of the lost turn aborted. That counts as the stop confirmation a
   project delete needs; Resume, Retry, Continue and Override are refused with
   `409 project_archived`, since they would start work again.

## 6. Wake eligibility (note creation)

Computed in the message-insert transaction; every decision → note or
"not woken because …" event:
- human message → every AI member of the room, unless it addresses specific
  members (then only those). The exception is a lead whose membership is
  `auto` (below; never in the home room, where the project lead is a full member
  and migration 116 cleared the mark of existing leads): it is not woken by an unaddressed human message
  (`message_not_woken` reason `auto_member`), only when addressed by name,
  role, group tag, reply or handoff;
- AI message → resolved targets (mentions, addressed role), the human peer in
  a DM; `handoff` → the lead. An AI message that names nobody and answers
  nothing (no live `@mention` token of any kind, no `reply_to`, no `handoff`,
  not in a private chat with a person, not `OnlyTargets`) is a **broadcast**: it
  wakes every other AI agent in the room as an FYI (reason `broadcast`; the
  sender is never woken; a lead whose membership is `auto` is skipped with
  `message_not_woken` reason `auto_member`, as for a person's unaddressed
  message). A mention that reaches nobody (an unknown name, a person) is still
  a mention, so it is not a broadcast; a mention written inside code is not a
  mention, so it is. Nor is what an agent writes in a turn that has a note from a
  person written in the same room (found through the message's outbox row and
  the turn's notes): it is said to that person, however many notes or messages
  there are, and does not wake the room. Nor is a message that asks a person
  for something (an Inbox ask, `needs_attention`, proposed answers, open
  questions): it is addressed to that person. The delivered note says it was not addressed to the agent
  (`fyi: true`, and its `why`), and the agent answers only if it has to act,
  otherwise it returns an empty `messages` array. A broadcast is room-wide AI
  chatter like a group tag: message expiry drops it the same way
  (docs/retention.md). A broadcast is an automatic wake like a reply, and the
  hop guard below counts both together: after 4 such wakes in the room with no
  person's message between them, the next broadcast is stored and shown and
  wakes nobody (`loop_guard` event with `kind: broadcast`, one system notice
  per run). The prompt's etiquette (always `@mention` whoever should act next)
  keeps them rare. An AI reply to another AI wakes the parent author
  (reason `reply`) while the room has seen fewer than 4 such wakes (replies and
  broadcasts together) since the last person's message, within 30 minutes (the
  hop guard, counted per room, not per chain); the next reply keeps its thread link and wakes nobody, with one
  `loop_guard` event and one system notice per run, and a person's message
  resets the count. Mentions and handoffs are not limited and are never
  overwritten by the `reply` reason.
  An AI reply to a human still targets that human;
- inbox answer → the requester;
- in a chat between agents (`room.project_id` is the reserved project, `kind=dm`,
  `dm_key` `agents:<id>:<id>`): an agent's message → only the other agent of the
  pair, reason `agent_dm` (mentions, group tags, replies and handoffs are not
  read, and people are not addressed); at most 30 agent turns per chat in the
  last hour, after which an agent's message wakes nobody
  (`message_not_woken` reason `dm_turn_cap`, one system notice an hour); a
  person's message → the agents it names, or both when it names none, but only
  agents whose project that person reaches (owner or operator there, or the
  instance owner): the others get `message_not_woken` reason `no_reach`, and the
  message stays visible;
- never: own messages, duplicates, routine system chatter, agents that are
  revoked or have left the room. Role/control changes ride in the next
  `deliver` context.
Dispatch (separate): agent desired=active, room/project gate open, switch
gate open, room lifecycle=open, `recovery_block=false`, no pending **and**
no running turn → batch =
all `queued` notes of the agent's oldest-queued room, ≤ 20, rooms
round-robin.
- Special turns: a **diagnostic** turn may dispatch while
  `recovery_block=true`; a **correction** turn may dispatch while its parent
  awaits settlement. Neither bypasses desired=stopped, a room/project pause
  or the switch gate. Their own execution settles via their own receipt;
  the correction's answer settles the parent's output, the diagnostic's
  result settles the parent's report. No ordinary queued work dispatches
  for the agent until the original turn's settlement/recovery is resolved.

Addressing: `@login` resolves against the room's user members and, in a
non-DM room, every active human of the room's project (they can read the room
without a `room_member` row); a DM resolves only its peers. Group tags
(`@channel`, `@humans`, ...) still mean the room's explicit members. A login
that equals an agent slug is ambiguous, as for two agents with one slug.

## 7. Spawn / move / role change checks

Adapter-listed (tool, variant, model, provider) with provider pinned; actor
authorised (owner/operator, or an agent whose role has `cap_can_spawn`);
`max_live_agents` counts live workers and reviewers, excluding the template's
core roles (leader for A, leader and dispatcher for B). Custom projects
exclude singleton roles with lead capability; multi-instance lead roles use
worker slots. The spawn rate still counts every new
agent; depth cap and singleton role cardinality also apply.

When the role has a nonempty effective default list, a spawn that omits the harness
and model uses its next available `{launcher_id,model}`. In-turn selection
starts at the durable project/role cursor and skips unavailable machines,
unmapped project folders, launchers, and models;
random selection samples uniformly from available entries. The cursor
advances only in the successful spawn transaction. An agent's explicit
deviation from an available default requires a reason in the room notice.
When none is available, an explicit choice is accepted with a room warning;
an omitted choice is refused and also warns the room. The original request,
before default resolution, is the idempotency digest, so a retry replays the
same spawned agent even if the next default changed. While the
project's `home_lead_only` is on (the default, migration 70 turns it on for
existing projects), a spawn into its home room needs a role with
`cap_is_lead` (`home_is_lead_only` otherwise; agents already there are not
touched); idempotent via request_key; system message + event for each.
Folder admission runs before a new supervisor process starts. The host project
folder and an owner-admitted remote project's mapped folder are allowed
roots. Paths are canonicalized on the owning machine and compared by path
components. Outside roots and unadmitted remote machines create an owner
approval inbox item; the pending agent or switch does not dispatch. A grant
is one-time per `(agent_id,machine_id,canonical_path)`. Existing processes
continue at deployment and are checked at the next spawn, switch, or
reactivation. Boundary admission is a mistake guard, separate from machine
keys, user permissions, and per-agent key revocation.
A task room (opted in, or by default while the project's `worktree_mode` is
`auto`, the room was created after that took effect, and the worktree can be
made, else the project folder is used; see [cleanup](cleanup.md)) creates
`<project>/.aimebu/worktrees/<room-id>` on
`aimebu/<room-id>` from the committed HEAD of its base ref. The server adds
`.aimebu/` once to Git's `info/exclude` and creates
`.aimebu/.gitignore` with `*` when absent; neither is committed. Dirty base files stay in the base checkout, and their count is
reported in the room and API. New agents homed in the room use that path;
an existing lead that joins another room keeps its current checkout. Each
machine creates the equivalent worktree in its mapped project root, and the
folder boundary validates the resulting path. Creation is retriable after
failure; no agent starts in a pending or failed worktree.
Launcher admission then checks the selected machine's enabled launcher,
reported availability, container network bind, canonical checkout, visible
host roots, and longest-prefix host-to-visible path mapping. A containerized
launcher may specify an agent-reachable server origin; toolbox availability
is reported per launcher, subject to the adapter's actual credential channel.
The server passes launcher commands and argument arrays without a shell.
The Git root is validated before opt-in persists. An open room may cancel a
pending or failed opt-in if no machine created its checkout. A same-machine
model switch keeps its current checkout; only newly spawned agents select the
room worktree.

## 8. Adapter capability registry (code, per tool × variant × tool
version × environment; a capability verified for one version is re-verified
on upgrade)

`schema_flag`, `no_action_mode` (correction), `readonly_verified`
(diagnostic: requires negative tests that shell, network, MCP and writes are
blocked), `compaction_events`, `context_window`, `cost`, `cancel_confirmed`.
Initial values from the spikes: CC/Codex `schema_flag` true, pi false; Codex
`compaction_events` false; Pi 1.1.0 has verified no-action tool selection for
corrections and emits compaction events; **`readonly_verified` false for all**
until the negative tests in T5 pass (Codex additionally needs host sandbox
verification).

## 9. Error codes

`room_closed`, `room_closing`, `not_member`,
`stale_generation`, `stale_rev`, `snapshot_required`, `spawn_limit`,
`invalid_answer`, `conflict`, `unauthorized`,
`forbidden`, `depth_exceeded`, `already_resolved`, `already_withdrawn`,
`agent_not_active`, `invalid_transition`, `invalid_request`, `not_found`,
`invalid_csrf`, `invalid_credentials`, `rate_limited`, `project_archived`,
`docker_only`, `scope_mismatch`, `checkout_scope_mismatch`. docs/api.md lists
the codes of each route. Each documented as retryable or not.

## 10. Idle-profile warmup

`warmup_mode` and `warmup_schedule` are owner settings. The token-copy
refresher scans persisted usage snapshots for inactive, unpinned, unlinked
profiles with no active session window. It uses the same account lock,
spend-once journal, and rotated-credential commit as refresh, but never
forces an access token to expire. One-hour attempt times persist separately
from credentials.

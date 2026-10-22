# Slice-1 implementation contract (revision 19)

This contract defines the slice-1 data model, state machines, and wire protocol.
It implements the architecture decisions and validated adapter behavior.
Implementation must cover lost acknowledgements after process start, stale
delivery after stop/reactivation, journal loss versus intact-journal absence,
delayed tool events after side effects, inbox answer versus room close, and
account switch versus new dispatch. Corrections require a verified
`no_action_mode` capability, and diagnostics require verified read-only
isolation. Migrations may reshape tables later.

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

## 2. Tables (slice 1)

Projects, rooms, people
- **project**(id, name, folder, repo_url?, gate[open|
  paused], gate_rev, control_desired[active|paused|stopped], created_at)
- **room**(id, project_id, parent_id?, depth, kind[home|task|lobby|dm],
  dm_key? UNIQUE, lifecycle[open|closing|closed], gate[open|paused],
  gate_rev, control_desired[active|paused|stopped], lifecycle_rev, title,
  summary?, created_by, created_at, closed_at?) — a task room's title can be
  changed (`room_renamed`); the home room's title is its project's name and
  changes only together with it; a project is created with
  one home room and a `general` task room beneath it (also a project the v1
  import creates; an import into an existing project fills its open `general`
  rather than adding a second one, and uses a closed `general` only for v1
  rooms that are all imported archived)
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
  token_hash UNIQUE) — the 256-bit bearer token is never stored directly;
  migration 3 adds its hash while the row ID remains sortable.

Content
- **message**(id, seq UNIQUE, room_id, author_kind[user|agent|system],
  author_id, body, reply_to?, thread_root?, targets_json, ask_json?,
  needs_attention, proposed_answers_json?, open_questions_json?,
  source_out_id? UNIQUE, created_at, visual_plan_json?,
  appendix_pages_json?, author_harness, author_model) — historical v1
  messages use these native columns. They never create inbox items or
  delivery notes.
- **attachment**(id, project_id, room_id?, message_id?, sha256, mime, size,
  path, filename, width, height, created_by, created_at) — file bytes under the server config root,
  room fixed at upload, message bound atomically on send; width and height are
  an image's pixel size (0 when unknown); unattached rows swept after 24 h. Reads require room membership.
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
  `global` key. Only the installation owner can curate global records as a
  human, even while project memory is disabled. An agent in any
  memory-enabled project can create or edit profiles for any user and shared
  notes; active records are visible to all project members and enter agent
  context and recall in every memory-enabled project. This deliberately
  carries v1's trusted-workspace cross-project knowledge semantics. Imported
  records are ordinary and active on arrival. `active=0` is only the owner's
  explicit "Deactivate" on a record. The per-scope caps are 2048 bytes for one profile and
  4096 bytes for shared notes; each body is at most 2000 Unicode characters.
  Agents can list, revise, and remove active global records with revision
  checks. Agent-tool stale revisions return the current record.
- **task_note**(id, room_id, body, rev, author_kind[user|agent], author_id,
  created_at, updated_at) — curated notes attached to a task room; separate
  from delivery `note` and `turn_note`

Roles, agents
- **role**(id, key UNIQUE (legacy internal key), project_id? (always NULL since migration 64), role_key,
  label, emoji, body (editable instructions), extends (role_key of a parent
  role, '' for none), cardinality[multi|singleton],
  cap_is_lead, cap_can_spawn, cap_singleton_writer, rev, deleted_at?) — active
  public `role_key` is unique. Roles are global only. Deletion preserves historical agent
  references and hides the role from new selection; stopped agents with a
  deleted role cannot reactivate. Existing agents retain their `role_id` until
  respawn or explicit role change. Editing a row's body affects its agents on their next
  delivery. The instructions delivered are the role's *resolved* body: the
  parent's resolved body, a blank line, then the role's own (trimmed); no cycles, at
  most five ancestors, and a role other roles extend cannot be deleted. A built-in
  global role (id `*_default`) can be reset to its shipped definition by the host
  owner.
  Capability and cardinality changes are rejected while live agents use the
  row; `cap_*` changes require the host owner.
- **prompt_override**(id, project_id?, key, value, rev, updated_at) — an
  owner-edited replacement for one shared prose sentence of an adapter's turn
  prompt (keys are the slots of `internal/adapter/prompt`: `identity`,
  `retry_notice`, `inbox_ask`, `mentions`, `human_reply`, `tail`), global
  (`project_id` NULL, host owner) or for one project (its owner or operators);
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
  created_by, created_at) — **one role per agent** (no per-room roles in
  slice 1 — stated plainly: one agent cannot hold different roles in
  different rooms; `@role` resolves to room members whose agent role
  matches); `home_room_id` is set on the first non-DM agent join and limits
  room bulk control to that agent's current home membership; `generation`
  is a **session** counter
- **session**(id, agent_id, generation, native_session_id?,
  start_mode[native_resume|fresh_with_history], started_at, ended_at?)
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
- **machine_launcher**(id, machine_id, tool, name, label, command, args_json,
  env_refs_json, enabled, containerized, agent_server_url?, path_maps_json,
  visible_roots_json, supports_toolbox, preset) — the **harness** a machine
  offers (the product calls it a harness; the table keeps its name because
  agents, switches and dispatch reference it): one unique name per tool and
  machine, an owner-facing `label`. Owner configuration and stable ID select
  execution; `variant` is a compatibility alias. Environment references store
  variable names only. **machine_launcher_activity** records owner changes
  (`save`, `delete`, `models`, and `model_default` when an agent's removed model
  was replaced by the default). Nothing is seeded: a new machine has no
  harness. Migration 69 deleted the seeded launchers that were disabled and
  unused, cleared `preset`, set labels and copied the old per-tool model list
  into each kept harness; migration 28 had carried forward Docker choices and
  sampled `AIMEBU2_ALLOW_NATIVE=1` once. `docker` is a reserved name (an alias
  for `harness-docker` in older API requests). Active or stopping agents
  (including revoked agents) and in-flight switch targets block edits and
  removal until their stop or switch is observed.
- **harness_model**(launcher_id → machine_launcher ON DELETE CASCADE, slug,
  label, note, is_default, position) — PRIMARY KEY (launcher_id, slug); a
  partial unique index allows one default per harness. The slug is the exact model
  id the harness is started with (never an alias). A non-empty list has exactly one
  default, which is what an agent whose model was removed starts with: the agent's
  `model` is updated on its next dispatch or spawn. Spawn and switch accept only a
  listed model (an omitted model with a harness means its default). The old global
  `spawn_model` table is gone. Migration 69 copies the old per-tool list into every
  kept harness (its first entry the default) and adds, after them at position 1000
  and up and never as the default, each model an existing agent or pending switch
  uses that the list lacked, so that no live agent changes model; a harness that
  would only have those gets its first as the default. A tool whose launchers would
  all be deleted (disabled, unused, seeded) keeps one disabled launcher (the host's
  native one, else its first by name) when its list differs from the seeded one in an id, a label or its length,
  so an edited list survives; a list that is dropped is written in full to the log.
- **checkout**(id, project_id, machine_id, canonical_path, writer_agent_id?,
  writer_state[none|reserved|releasing]) — UNIQUE (machine_id,
  canonical_path); container mounts map to the host canonical path so
  aliases cannot evade the writer rule

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
- **request_key**(actor_kind, actor_id, kind[spawn|control|scope_control|switch|
  inbox_answer|recovery_decision|room_create|room_close], key,
  request_digest, result_json, created_at) —
  UNIQUE (actor_kind, actor_id, kind, key); same key + same digest → original
  result; same key + different digest → `conflict`

Inbox, usage, events, settings
- **inbox_item**(id, project_id, room_id, message_id?, turn_id?,
  agent_id?, requester_kind, requester_id,
  kind[question|approval|recovery|delivery_failed], options_json?,
  state[open|resolved|withdrawn], rev, answer_message_id?, resolved_by?,
  resolved_at?, created_at) — `assignee` returns in slice 4
- **usage_record**(id, source_event_id UNIQUE, turn_id, agent_id, room_id,
  project_id, tokens_in?, tokens_out?, context_used?, context_window?,
  cost_est?, source[measured|estimated|unavailable], created_at)
- **event**(id, seq UNIQUE, kind, project_id?, room_id?, agent_id?,
  turn_id?, payload_json, created_at) — same transaction as the change
- **settings_project**(project_id, max_live_agents=10, spawn_rate_per_min=6,
  auto_resume_after_clean_diagnostic=false, memory_enabled=false,
  worktree_mode='auto', worktree_auto_since, home_lead_only=true); **cleanup_log**(id, seq, category, target, project_id,
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
  `referenced_messages` and `message_get`. Another project's messages never.
- **message_no_counter**(id=1, value) — the message-number allocator
  (migration 71); `message.no` is unique where it is positive.

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
with `AIMEBU2_DEV_IMPORT_V1=1` for development tests.

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

Slice 3+ (not in the slice-1 migration): spawn_code,
admission inbox kind, membership grants, usage caps.

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
  receives a system notice for answers held during validation.
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
  **never** implies stop or writer release.
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
  "Resume" button): desired → active; a new session (native resume or
  fresh) bumps `generation`.
- **Switch model/tool** (owner/operator): an active or not-accepting agent
  enters `not_accepting` with a new desired revision. After its open work
  settles and no recovery block remains, the old session ends and a fresh
  session starts on the same agent ID with the selected adapter and a new
  generation. A room-local addressed handoff note supplies recent context;
  queued notes and room membership survive. A pre-existing
  `not_accepting` state is restored after the new spawn confirms. A failed
  switch can retry the same target, and a successful Stop or Reactivate
  clears its failure gate. The switch cannot start from a
  stopped or recovery-blocked agent or a closing/closed room. Stop cancels
  a pending switch.

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
  after 2.0.0). After the room is closed and its home agents have stopped, the
  owner may remove the worktree from the room's settings: a clean worktree
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

### 3.6 writer reservation (checkout)
`none → reserved` (S, on spawn/assign of a `cap_singleton_writer` agent) →
`releasing` (S, on stop/close/reassign/role-change/move request) → `none`
only when the old writer is **no longer dispatchable for writing**
(desired≠active, or its role lost `cap_singleton_writer`, or it moved off
the checkout) **and** observed ∈ {idle, stopped} with no running process (held/unsettled replies do not block —
they write no files), or on owner override (event + UI keep the
uncertainty). Every writing-turn
dispatch re-checks that the agent is the checkout's current writer.

### 3.7 inbox item
`open → resolved | withdrawn`, compare-and-set on (state=open, rev):
- Boundary admission is a distinct use of `approval`: only a project owner
  may choose Approve or Deny. Approve records the project-machine admission
  and the agent's exact canonical path grant in the same transaction as
  releasing the pending spawn, switch, or reactivation. Deny leaves the
  agent stopped and releases its writer reservation, or cancels the pending
  switch. A paused legacy spawn stays paused on approval. Stop, room close, and machine
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
  `error` event it reported (`OpenCode error: APIError (HTTP 429): …`), subject
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
requeue). Raw tool
stdout/stderr per agent are kept alongside for debugging (retention 14 d).

## 3c. Provider accounts (slice 1)

- The host switcher stores named Claude Code and Codex file profiles under
  `~/.aimebu2/switcher/`, separate from the v1 switcher state. Account scope
  is host-machine-wide per tool across projects. Remote machines have no
  switcher profile set in this slice.
- Host-machine switching requires owner and shows the agents using the tool.
  A switch returns 409 with the running agents while any matching turn is
  active; the owner waits or stops those agents and retries. Remote-machine
  switching and a persistent dispatch gate are later work.

## 3d. Deployment

`aimebu server` runs the local supervisor in-process by default. It uses the
same versioned machine WebSocket and `supervisor/journal.sqlite` as the
standalone `aimebu supervisor` process, so reconnect and crash recovery use
one path. The server provisions an approved local machine and private key on
first start; the key file is mode 0600. `AIMEBU2_EMBEDDED_SUPERVISOR=0`
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
  supports_path_probe?, supports_worktree?, supports_launchers?, supports_detect?,
  supports_detect_dir?,
  capabilities[{tool, version, environment, flags}], harnesses[...], agents[{agent_id,
  generation, open_turns[{turn_id, exec, process_alive?, has_answer, answer_rev,
  digest?}]}]}`
- S→M `hello_ack{v, desired[{agent_id, generation, desired, desired_rev}],
  redeliver[...], resend_receipts[...], harness_settings[...],
  harness_probe_dir?, launcher_definitions[...]}` — the optional probe directory is the first project
  folder mapped to that machine, used only for launcher version discovery.
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
  carries `supports_detect`). `harnesses` lists the Claude, Codex, Pi and OpenCode
  executables on PATH and the usual user-level install directories, once per real
  file; `models` runs the family's own listing command (`pi --offline
  --list-models`, `opencode models`, `codex debug models --bundled`) on the
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
- S→M `worktree_op{v,req_id,checkout_path,room_id,base_ref,
  worktree_action,worktree_force}`; M→S `worktree_result{v,req_id,
  details?,code?,message?}`. The supervisor runs bounded Git operations in
  the mapped project root and reports the canonical worktree path. The server
  sends this frame only when the hello advertised `supports_worktree`.
  Actions are `validate`, `ensure`, `remove`, and `inspect` (read-only: size,
  uncommitted file count, merge state, head commit and last activity, used by
  the cleanup); typed Git error codes are preserved in `worktree_result`.
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
  newest rev is applied
- S→M `deliver{v, agent_id, generation, turn_id, mode, parent_turn_id?,
  notes[{note_id, room_id, from, message_id, body, reply_to?, thread_root?,
  attachments[{id,name?,mime,size,sha256}]}], attachment_bearer?,
  context{role_body, rooms[{room_id, title, kind}],
  you_are:{id,name,role,harness,model},
  team_roster:[{id,name,role,harness,model}],
  team_roster_more?:string, referenced_messages?:[{no, available, room_id?, room?, from?, body?, created_at?}],
  knowledge?:{project_memory_enabled, project_memory[], user_profiles[],
  agent_shared_notes[], task_notes[]},
  control_changes?, recovery_evidence?, correction_error?}, desired_rev,
  allowed_tools?, agent:{id, tool, variant, model, provider},
  native_session?}` — rooms in context are information, not grants. For pi agents on gemma models `knowledge` is trimmed to a
  shared 2048-byte budget of record bodies; later records are dropped. The
  attachment bearer is used only by Pi Docker, never journaled, and authorizes
  reads only for a delivered file in a live turn while the agent remains a
  member of its room; MCP agents use `attachment_get` with the same checks.
- M→S `ack{v, agent_id, generation, ref, level[persisted|accepted]}`
- M→S `activity{…, turn_id, source_event_id, kind, summary}`,
  `usage{…, source_event_id, tokens_in?, tokens_out?, context_used?,
  context_window?, cost_est?}`, `compaction{…, pre_tokens, post_tokens}`
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
`{"messages":[{"room":"…","body":"…","reply_to":"msg_…?","thread_root":
"msg_…?","attachments":["att_…"]?,"ask":{"kind":"question|approval",
"text":"…","options":["…"]?,"free_text":bool}?,
"needs_attention":bool?,"proposed_answers":["…"]?,
"open_questions":[{"question":"…","description":"…"?,"options":["…"]}]?,
"handoff":bool?,
"visual_plan":[{"id":"…"?,"type":"…","title":"…"?,"data":{},
"order":0?}]?,"appendix_pages":[{"title":"…"?,"body":"…"}]?}],
"reconciliation":{"status":"clean|uncertain","findings":["…"]}?}`
- A message may use `proposed_answers` or `open_questions`, but not both.
  Neither may be combined with `ask`.
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
   - idempotency contract → retry (**slice 1: no operation has one**);
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

## 6. Wake eligibility (note creation)

Computed in the message-insert transaction; every decision → note or
"not woken because …" event:
- human message → every AI member of the room, unless it addresses specific
  members (then only those). The exception is a lead whose membership is
  `auto` (below): it is not woken by an unaddressed human message
  (`message_not_woken` reason `auto_member`), only when addressed by name,
  role, group tag, reply or handoff;
- AI message → resolved targets (mentions, addressed role), the human peer in
  a DM; `handoff` → the lead. An AI reply to another AI wakes the parent author
  (reason `reply`) while the room has seen fewer than 4 such wakes since the last
  person's message, within 30 minutes (the hop guard, counted per room, not per
  chain); the next reply keeps its thread link and wakes nobody, with one
  `loop_guard` event and one system notice per run, and a person's message
  resets the count. Mentions and handoffs are not limited and are never
  overwritten by the `reply` reason.
  An AI reply to a human still targets that human;
- inbox answer → the requester;
- never: own messages, duplicates, routine system chatter, agents that are
  revoked or have left the room. Role/control changes ride in the next
  `deliver` context.
Dispatch (separate): agent desired=active, room/project gate open, switch
gate open, room lifecycle=open, `recovery_block=false`, no pending **and**
no running turn, writer check for writing roles → batch =
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
`max_live_agents` and spawn rate; depth cap; writer rule (§3.6); while the
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
`.aimebu/` once to Git's `info/exclude`; it never edits `.gitignore` or
commits. Dirty base files stay in the base checkout, and their count is
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

`room_closed`, `room_closing`, `room_paused`, `not_member`,
`stale_generation`, `stale_rev`, `snapshot_required`, `writer_occupied`, `limit_agents`,
`limit_rate`, `invalid_answer`, `conflict`, `unknown_tool`, `unauthorized`,
`forbidden`, `depth_exceeded`, `already_resolved`, `already_withdrawn`,
`agent_not_active`, `machine_unapproved`, `invalid_transition`,
`readonly_unavailable`. Each documented as retryable or not.

# Architecture decisions

**Architecture**

1. The server runs its local supervisor in the same process by default. It
   connects through the same machine WebSocket and durable journal as a
   standalone supervisor. `aimebu supervisor` remains available for remote
   machines; `AIMEBU2_EMBEDDED_SUPERVISOR=0` disables the local instance.
2. Relational SQLite store + transactional append-only event/outbox table.
   Not event-sourced. Store startup retries temporary SQLite BUSY/LOCKED
   contention during connection setup or migrations for up to 15 seconds;
   other migration errors fail immediately. The supervisor journal has its
   own WAL database and process lock.
3. Native resume first where the spike validates it; fresh + history as the
   explicit fallback (also for swap/recovery); UI says which happened.
4. Common reply schema, server-side validation; native schema modes are
   adapter optimisations; the schema applies to the structured final answer,
   taken from the adapter's final-answer event (per-tool event recorded by the
   spike), never scraped stdout. Raw output kept privately on failure.
5. Transport: one multiplexed socket per machine; machine key = trusted
   supervisor for its registered agents only; each logical agent channel bound
   to that agent's credential + current generation, frames authorised against
   the binding; revocation cuts one agent without dropping peers; bounded
   queues per agent. Per-agent keys for toolbox calls.

**Data model additions** — RoomMembership; Message.targets; Turn (id,
originating room, input-note set, attempt); Session ≠ process generation;
Checkout + writer reservation; Role; Attachment; Reaction; InboxItem
(message_id, assignee); Machine (machine key); auth sessions/codes;
provider-account ownership; usage keyed by source-event id + originating
turn's room. Room = lifecycle (open/closed) + separate persistent dispatch
gate (paused) at room and project level, inherited by spawned/moved agents.

**Protocol and state**
- Ack vocabulary: persisted → accepted for execution → turn completed → reply
  committed. Server owns committed messages, control, permissions, wake
  decisions; supervisor owns a durable local execution journal; reconnect
  merges both by turn_id + generation.
- Completion = execution finished AND outputs settled (committed or held);
  zero-message answers send an explicit completion ack. Process state and
  delivery state are separate fields.
- Reply: reply_to, thread, attachment refs, `ask` (question/approval, options,
  free text) → server derives InboxItems. Multi-message answers all-or-none.
  "Room closed" is a validation error.
- Correction turn: formatting-only, action tools disabled.
- Fencing: stale-generation control/state frames rejected; an old
  generation's durably journaled reply is reconciled by turn/output id —
  committed once if still admissible, else held visibly, never dropped.

**Recovery**
- Unknown outcome → `recovery_needed`. Automatic action-enabled continuation
  only if (a) the run never started, (b) the execution mode was verifiably
  incapable of effects throughout, (c) completion was durably recorded
  (pending replies resend by dedupe), or (d) the operation has a verified
  idempotency/reconciliation contract with complete coverage of its effects.
- Otherwise: read-only diagnostic turn (enforced by the adapter, incl.
  shell/network/MCP) collects evidence → human inbox item (Resume / Retry /
  Abandon). "No action event observed" is evidence, not a safe condition.
- Routine crashes with a known outcome raise no inbox item.
- Docs promise deduped delivery, not exactly-once actions.
- Slice-1 gate test: fake side effect, tool event delayed until after it,
  crash → must end in `recovery_needed`, no duplicate execution.

**Wake rules** — human message wakes every AI member of the room unless
addressed elsewhere (a lead that is in the room only because leads are put into
every project room, see `room_member.auto` in the contract, is woken only when
addressed: by name, role, group tag, a reply to it, or a handoff); AI message wakes explicit mentions, DM peers, addressed
roles, and the lead on task-result handoffs. An AI reply to another AI alone
keeps the thread link without waking its parent author; replies to humans
still target them. Inbox answers
wake the requester; routine system chatter never wakes; role/control changes
are folded into the next delivery's context. Bounded batches with stable IDs,
per room with fair scheduling (cross-room context labelled); one turn per
agent; no mid-turn injection. Wake decision + queue entry persisted atomically
with the triggering message; every decision logged.

**Private chats** — a DM is keyed by one project member and one agent, or by
two people with active membership in the same project. Only active room
participants can list or access its room, messages, reactions, ordinary inbox
items, and room events; the global owner has no room access unless a
participant. Cross-room recovery decisions follow their separate owner rule.
An agent sees a DM through its active room membership. DM notes form their
own turn; its answers can target only that DM. Closing and reopening use the
ordinary room lifecycle, and the pair keeps the same room identity.
Human-to-human messages address the other participant for the same target
metadata and unread flow as an agent's private reply. Adding an agent to a
human-to-human DM remains unavailable through the current agent join routes.
Viewer write access applies only to these two-person chats while both people
remain active project members and no agent has joined; an agent DM still needs
owner or operator write permission.

**Control, spawning, rooms**
- Lead spawn/stop: same server path as the UI, idempotency keys, max live
  agents per project (default 10) + spawn-rate cap, system message + event for
  each, single-writer check on spawn, role change and move.
- Writer reservation releases only on confirmed termination/quiescence or an
  informed owner override; never on request, machine loss or revocation.
- Close: persist the no-new-work gate first; children closed first or cascade
  with confirmation; in-flight work interrupted/drained; late replies held
  with "delivery failed" visible; nothing discarded.

**Auth** — PBKDF2-HMAC-SHA256 via stdlib (policy stored with the hash,
transparent rehash on login; parameters verified at T3 time, e.g. 600k
iterations, 16-byte salt); CSRF; authenticated WS upgrade + origin check;
login throttling; private/held output redacted with defined access and
retention. Per-agent raw stdout/stderr capture alongside the journal.

**Frontend** — framework-free, modular from day one, patch live lists in
place (no innerHTML rebuilds); tests for stable message identity, draft
preservation, scroll under live updates. Preact + htm (vendored) is an option
for the owner only if a concrete case arises (dependency consent + licence).

**Tasks (order)** — T-contract (tables, states, ack vocabulary, versioned
envelope) → T1 skeleton → T2 core → T3 auth ∥ T4 API/events ∥ T13 usages/
switcher boundary review → T5a adapter interface + fake adapter → T6a delivery
log → T7 structured protocol → T8 wake rules → T9a spawn + writer reservation
→ T6b recovery on the fake adapter (+ gate test) → T5b CC / T5c Codex → T9b
lead toolbox → T10a shell+login / T10b sidebar / T10c chat / T10d agents panel
→ T11 inbox → T12 room lifecycle → T14 usage attribution → T5d containers →
T15 acceptance. Acceptance fixtures + fault injection grow from T5a. Plus:
membership/addressing, roles CRUD, machine identity + supervisor
bootstrap, attachments, reactions, DMs, renderer port, per-agent logs,
reconnect cursors + snapshot, auth/revocation tests, T-docs,
T-cutover-checklist.

**Product decisions**
- Harness output parser: integrate it as an aimebu package and use it for
  tool event streams.
- Add the per-project switch "auto-resume after a clean diagnostic report",
  **default off**, as an explicit product decision (outside the council
  consensus). Its settings text must state the risk: a model's report can miss
  unobserved effects (e.g. a network write), so a resumed task may repeat an
  action.

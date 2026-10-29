# Architecture decisions

**Architecture**

1. The server runs its local supervisor in the same process by default. It
   connects through the same machine WebSocket and durable journal as a
   standalone supervisor. `aimebu supervisor` remains available for remote
   machines; `AIMEBU2_EMBEDDED_SUPERVISOR=0` disables the local instance.
2. Relational SQLite store + transactional append-only event/outbox table.
   Not event-sourced. Store startup retries temporary SQLite BUSY/LOCKED
   contention during connection setup or migrations for up to 15 seconds;
   other migration errors fail immediately. The server database runs WAL with
   `synchronous=NORMAL`: a crash of aimebu loses nothing, but a power loss or
   operating-system crash can lose the last few commits (never corrupt the
   database), in return for much cheaper writes. The supervisor journal has
   its own WAL database and process lock, and keeps `synchronous=FULL`.
   One connection serializes store writes; a separate small read-only pool
   serves message pages, room lists, agent cards, inbox, event stream and
   health reads. A long read does not occupy the writer connection. Reads
   inside a write transaction stay there for immediate visibility. The event
   journal indexes turn/kind and agent/time lookups; health reads visible
   agents in one query instead of rebuilding cards for every room (each
   agent's rooms in the order the room list has them). The read-only pool
   opens the database through a `file:` URI, so `store.Open` and the other
   SQLite openers make a relative path absolute first.
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

**Chats between agents** — a private chat between two agents, in the same
project or in different ones. The chat belongs to no project: its room hangs
off a reserved system project (never listed, no members), is a private chat
(`kind=dm`) keyed `agents:<id>:<id>`, and there is one per pair, whoever opens
it. Everything agent-side keys on the agent's own project, not the room's:
dispatch and pause (a paused project holds only its own agent's notes), usage
caps and billing, project memory (a chat shows neither side's project facts to
the other and never feeds recall or the memory snapshot), and `#N` lookups
(visible to the chat's two agents and its people only). A chat turn may reply
in the chat and also report into rooms of the agent's own project it belongs
to; nothing else. An agent's question or request for attention in a chat is
filed under the agent's own project and goes to the owners and operators of
that project who are in the chat. An active lead starts a chat with any agent,
in its own project or another, with the `dm_agent` tool; it opens at most 10
new chats an hour. Any active agent, a worker as much as a lead, can start the
ordinary private chat between itself and a person of its own project (owner,
operator or viewer) with the `dm_human` tool: the person's side of that chat is
the same one they open from the agent's card, and their reply wakes the agent
in it. At most 20 messages an hour go into one such chat and 10 new chats an
hour are opened. A viewer reads it but, as everywhere, cannot answer.
What one agent says in the chat wakes only the other agent of the pair
(mentions, group tags, replies and handoffs are not read); a person's message
wakes the agents it names, or both when it names none, but only those whose
project that person reaches (owner or operator there, or the instance owner):
the others stay asleep (`message_not_woken` reason `no_reach`) though the
message stays in the chat. Loop guard: at most 30 agent turns per chat per
hour, after which an agent's message is kept but wakes nobody (one system
notice says so; a person's message still wakes the agents it reaches). People join a chat they have
a footing in (the instance owner, or a member of the project of one of its
agents) from the list of chats they could join, which shows titles only;
joining says so in the chat. Reading needs any project role there, writing
needs owner or operator in one of the agents' projects (the instance owner
may), re-checked on every request, and a chat is read-only once every agent in
it belongs to an archived project. Deleting a project takes its agents out of
its chats first (a note in each; a chat left with fewer than two agents is
closed) and leaves the messages.

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
retention. Per-agent stderr capture alongside the journal, on the machine that
ran the harness, read back on demand for owners and admins (see
[the supervisor](supervisor.md#agent-output)); stdout is not captured.

**Frontend** — modular from day one, patch live lists in place (no innerHTML
rebuilds); tests for stable message identity, draft preservation, scroll
under live updates. Moving to Preact + htm, vendored and without a build step
(owner approval 2026-10-21, AMB-70; plan in `docs/design/frontend-preact.md`):
pages move one at a time, each replacing its legacy module in the same
change (Settings → Harnesses and its onboarding step are done). One rendering
owner per DOM subtree: a container is either Preact's or hand-written code's,
never both; pure logic stays in import-free modules.

**Product decisions**
- Harness output parser: integrate it as an aimebu package and use it for
  tool event streams.
- Add the per-project switch "auto-resume after a clean diagnostic report",
  **default off**, as an explicit product decision (outside the council
  consensus). Its settings text must state the risk: a model's report can miss
  unobserved effects (e.g. a network write), so a resumed task may repeat an
  action.

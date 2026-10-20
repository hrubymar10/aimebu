# Supervisor and delivery log

`aimebu server` starts a local supervisor in the same process by default.
It auto-provisions the approved host machine and its private key, opens
`supervisor/journal.sqlite`, and connects through the same `/api/machine`
WebSocket used by standalone supervisors. It starts once the server is
listening (reading each tool's version takes seconds, and the web UI does not
wait for it), and in first-run setup right after the owner is created. Set
`AIMEBU2_EMBEDDED_SUPERVISOR=0` to disable it for remote-only setups or
tests. On shutdown, the server stops dispatch, closes its HTTP listener,
interrupts active local turns, waits up to 40 seconds for their journal
writes, then closes the journal and server store. A timeout logs the turn IDs
that remain active; a restart reconciles their journal state normally.
One supervisor owns a journal at a time. A sibling `journal.sqlite.lock`
file (0600) holds an exclusive process lock and its owner's PID. Opening the
same journal from a second supervisor fails with a message naming that PID.
Two live processes cannot safely share its turn records because each has a
different in-memory process-liveness map. When testing standalone mode on the same
host, set `AIMEBU2_EMBEDDED_SUPERVISOR=0` on the server first.

The owner pairs a remote machine from the browser's **Add machine** action.
It issues a one-use token valid for ten minutes and shows a command to paste
on the remote host:

```sh
aimebu supervisor --pair 'https://vpn-server.example:9897' 'amp_...'
```

The command's server URL is `AIMEBU2_PUBLIC_URL` when set (for a server
behind a reverse proxy; see [HTTPS](tls.md#reverse-proxy)), otherwise the
scheme and host the browser used to reach the server.

The remote supervisor redeems the token through the server allowlist and
saves its machine ID, private key, and server URL in
`supervisor/machine.key` under its local v2 config root (mode 0600). The
server stores only a hash of the token and machine key. The machine stays
pending until the owner approves it; the CLI waits and then connects.
Later `aimebu supervisor` runs reuse this credential and URL. On the server
host, standalone mode still reads `server/host-machine.key` when no remote
credential exists. Set `AIMEBU2_URL` to override the saved origin if needed.
The supervisor opens `supervisor/journal.sqlite`, uses one WebSocket per
machine, and reconnects after a disconnect. Revocation invalidates the key,
closes its current socket immediately, and rejects subsequent frames.

For remote supervisors over a VPN, bind the server to its VPN IP with
`AIMEBU2_BIND` and include the supervisor's VPN source IP or CIDR in
`AIMEBU2_ALLOW`. The allowlist checks the direct peer address, including
pairing, approval checks, and the machine socket. Use an HTTPS server URL
when crossing an untrusted network so the pairing token and machine key are
protected in transit. V2 can serve HTTPS directly from caller-supplied
certificate files; see [HTTPS setup](tls.md). A standalone supervisor may set
`AIMEBU2_INSECURE_SKIP_VERIFY=1` for a self-signed development server, which
disables certificate verification for pairing and subsequent connections.
Each project needs a folder mapping for the remote
machine before it can receive a spawn; that folder must exist on that host
and use its canonical path rather than a symlink alias.
The host machine uses the project's ordinary folder automatically.
The supervisor receives named launcher definitions from the server and reports
whether each executable exists, its version when available, and its current
availability. A new machine has no harnesses (nothing is seeded): the owner adds
each in Settings → Harnesses, by hand or from what Auto-detect found, with one
executable name or absolute path, a label, argument array, environment variable
references, path mappings, and visible host roots. Arguments execute without a shell, and environment
values are never stored in the server. The last report and owner activity stay
visible while a machine is offline, but changes require a connected supervisor.
The name `docker` is reserved: older API requests use it as an alias for the
harness named `harness-docker`. The supervisor also answers two read-only
questions on its own machine, each a `detect` frame (see the contract): which
harness executables are installed (PATH plus `~/.local/bin`, `/usr/local/bin`,
`/opt/homebrew/bin`, `~/.npm-global/bin`, `~/.bun/bin`, `~/.opencode/bin` and the
global npm prefix, once per real file, with `--version`), and which models a Pi,
OpenCode or Codex harness lists (run with a 20-second limit and an 8 MiB output
limit). It never saves what it finds.
Docker discovery uses `PATH` without probing from an unmounted supervisor cwd.
A version probe uses a known mirrored directory when available; spawn preflights
the selected checkout through the launcher before admitting the agent. A
containerized launcher has up to 60 seconds for its checkout version probe;
a failed probe refuses the spawn before a turn is recorded. Path probes run
alongside the supervisor's frame reader, so a slow version check does not
delay other machine messages. Closing that machine connection cancels its
unfinished probes. A probe caps its wait for child-inherited output pipes at
one second; a successful version command remains valid when a child leaves a
pipe open. Spawn also rejects a launcher that has become unavailable when
restarting or replaying a pending agent.
Existing machines keep the launchers that were enabled or in use. Migration 28
enabled host native presets only when `AIMEBU2_ALLOW_NATIVE=1` was set as it
ran, and afterwards the variable has no effect; migration 69 deleted the seeded
launchers that were disabled and unused, and logged them (back up `aimebu.sqlite` before
upgrading). It adds a model an agent already uses to its harness's list, never as the default, so
no agent changes model, and for a tool whose launchers would all go while its model list was
edited it keeps one disabled harness so the list survives; a list that is dropped is written in
full to the log.

Machine frames use `v:2`; the supervisor sends `hello`
with its open journaled turns, receives `hello_ack`, and exchanges
`ping`/`pong` every 15 seconds. Agent frames are checked against the
machine binding, revocation, and current generation before server state is
changed. The server rechecks machine revocation on every dispatch and ping;
revoking a machine closes its socket and prevents new deliveries. The server
retains only the authenticated machine key hash in the socket state and
compares it with the current stored hash on those checks.
The `hello` includes capability reports keyed by tool, version, and
environment. The server snapshots the matching flags into each admitted
turn's policy. A missing or unmatched report leaves all flags false.
The server sends `spawn` after an agent, session, room membership, and writer
reservation commit. The supervisor binds that agent's generation and replies
with `spawn_result`. A spawn carries a stable request ID per agent generation.
Before acknowledging a spawn, the supervisor checks that its checkout path
is an existing directory on this machine, then writes a private MCP config at
`supervisor/agent-tools/<agent-id-hash>.json` with mode 0600. It contains the
server URL and that agent's `aak_` key in the MCP child environment
(`AIMEBU2_URL`, `AIMEBU2_AGENT_ID`, `AIMEBU2_AGENT_KEY`), plus an
absolute path to `aimebu agent-tools`. Only the config path enters a normal
turn's adapter spec and journal; the raw key does not. Correction and
diagnostic turns have no toolbox config. The file persists across supervisor
restarts and is replaced atomically when the server sends a new key.
Claude and Codex Docker adapters read this private host-side file and use the
HTTP toolbox; they never copy the file into the shared checkout. The HTTP endpoint uses the
same agent-key authorization and scope as the stdio toolbox's API calls.
Docker's container must be able to reach `host.docker.internal` and the
server's allowlist must admit its source IP. A stale spawn is rejected before
this write; a write failure sends a failed
`spawn_result` for that agent while the machine connection stays open.

Before a remote spawn, switch, or reactivation reaches the store, the server
checks the supervisor's `supports_path_probe` hello flag, then sends a
`path_probe` machine frame. The supervisor resolves symlinks and
returns the canonical directory in `path_result`; missing or non-directory
paths fail. An older supervisor does not receive the unknown frame: the
server returns `folder_unverified` without disconnecting it. Missing host
project folders also return `folder_unverified` before agent creation. The path is checked
again when the supervisor receives `spawn`.
An opted-in task room sends `worktree_op` to each selected remote supervisor
that advertises `supports_worktree` in its hello.
It creates or restores the room branch beneath that machine's mapped project
root and returns `worktree_result`; it never invokes a harness-specific
Docker command. A spawn uses the resulting checkout path through the normal
adapter mount. An older supervisor receives no unsupported frame and cannot
host that room's worktree agents.
The server first sends a `validate` action to check the Git root before it
persists the room opt-in. The supervisor returns typed Git error codes so the
API can explain invalid roots and path conflicts.
The server resends an unconfirmed active spawn after reconnect and
periodically while the machine remains connected, at most 16 per pass; a
retry with the same HTTP idempotency key also resends it. Stopped agents are
excluded. An error `spawn_result` records a terminal failure for that
generation, so pings do not resend it indefinitely. A `control` frame updates
the desired revision. Stop
cancels a running turn and confirms the stop only after its process exit;
the supervisor attempts a force kill after 30 seconds when an adapter
supports it. A disconnected supervisor receives current desired states in
the next `hello_ack`.
A lost turn retains its writer reservation until recovery establishes
quiescence.

`POST /api/agents/{id}/switch` first sets the agent to `not_accepting`,
letting an active turn finish without admitting another. Once open work has
settled, the dispatcher ends the old native session, advances the generation
and desired revision, and sends a fresh `spawn` for the chosen adapter. The
agent keeps its ID, room memberships, role, and queued notes. An addressed
room-local handoff note starts its first new turn with recent context. The
switch remains durable across a server restart; supervisor confirmation
clears the pending state and permits dispatch, while a rejected spawn leaves
a visible failure and keeps queued work from dispatching. If the agent was
already paused, confirmation restores `not_accepting` and sends a control
frame for that new generation. A successful Stop or Reactivate clears a
failed switch record; the same target can also be retried.

The journal generates a random identity once and sends it in each `hello`.
The server remembers the last identity per machine. If the journal is replaced,
all open dispatched turns on that machine become uncertain and block recovery;
none is requeued from the replacement journal's empty inventory. The server
emits a visible `supervisor_journal_changed` event for each affected turn.

The journal uses SQLite WAL with `synchronous=FULL`. On `deliver`, the
supervisor sends `ack persisted`, commits `turn_start`, sends `ack accepted`,
then starts the adapter process. If the adapter cannot launch it (for
example a missing executable or an unusable Docker checkout), the supervisor
journals a failed exit whose evidence carries that error and reports it like
any other non-terminal exit; the machine connection stays open for the
machine's other agents. A CLI-reported native session handle is
journaled before forwarding it to the server. The server binds it to the
turn's session and rejects a conflicting handle. On reconnect, the supervisor
replays unreceipted native-session events, then the usage and compaction
events of every turn whose exit the server has not yet acknowledged, before
exits and answers. The server dedupes those events by `source_event_id`, so
usage sent live before a disconnect is counted once, and usage or compaction
reported while the socket was down still reaches the daily cost cap and the
room notice. The next
delivery includes the handle and started flag, while a first delivery has no
started flag. The journal also records activity, usage, compaction, process
exit, final answer and receipt evidence. The final-answer event
carries the adapter's raw candidate; process exit alone never signals
completion. For a non-terminal process exit, the journal retains a bounded,
redacted stderr tail with the exit code. The server exposes that evidence in
the recovery item and the agent-card state tooltip. A terminal receipt permits
pruning the local candidate bytes; turn-start, exit, and receipt tombstones
remain. A running turn stays in the hello inventory even after its answer is
receipted; the supervisor reports whether its process is still alive. An
exited turn stays there until the server acknowledges its durable process
update; then the journal stops replaying that exit. An answered turn also
stays until its terminal receipt. An acknowledged exit without an answer
leaves the inventory, because no receipt can ever arrive for it; this keeps
the hello small and lets a machine reconnect after one of its agents
switched to another machine. A reconnect with a
journaled turn never silently requeues work. An intact journal with no
`turn_start` allows the server to settle the never-started turn and requeue
its notes.

Claude's failed `result` event and Codex's failed turn event also contribute
bounded, redacted error evidence to the process exit. The server recognizes
provider quota only from the respective tool's `limit reached` or
`usage limit` wording. It records a known failed turn, requeues its notes,
raises an informational inbox item, and gates later turns until the reported
reset time. Notes from a turn with activity beyond startup or usage carry a
`retry_notice` to check the state before repeating actions. An unknown reset
remains gated until Reactivate, Switch, or the inbox `Retry now` action.
This path does not request recovery.

A resumed Codex turn that fails with `failed to record rollout items` and
`thread <id> not found` (the CLI lost the native thread) is not a recovery
case either. The server clears the session's native handle, marks the turn
failed, requeues its notes (with the same `retry_notice` caution when the turn
showed activity), and records a `native_session_reset` event; the next
delivery starts a fresh Codex thread. A resumed OpenCode turn that prints
`Error: Session not found` on stderr is handled the same way (the event's
reason is `opencode_session_missing`). It does not apply to a turn that opened
its own thread and then lost it, so a fresh start that fails the same way falls
through to ordinary recovery instead of looping.
The server stores reported usage for the agent card and posts a room system
notice for each deduped compaction event, including after journal replay.
A correction proven never started is abandoned: its parent receipt is held
and settled, and blocked parents raise a delivery-failure inbox item. That
item exposes the held reason from the parent or correction receipt.
An exited turn with an uncertain outcome starts a diagnostic only when its
admitted capability snapshot verifies read-only mode. The diagnostic report
stays private to the recovery inbox. A lost turn goes straight to the inbox
because its previous process may still be running. Owner/operator decisions
use an item revision and idempotency key: Resume creates a linked turn with
evidence and a continuation instruction, Retry creates one with raw notes,
and Abandon aborts the notes. The notes become done only after the linked
turn settles. A clean diagnostic can auto-resume only when the project's
default-off setting was enabled at diagnostic admission; the policy and
system actor are recorded. Linked Resume/Retry turns survive a server restart
or transient dispatch failure with their chosen recovery context intact.
Once admitted, they wait for journal reconciliation rather than another
delivery. New source-turn evidence increments the open item's revision and
refreshes its decision baseline; an older revision is rejected. A stopped
agent receives an owner recovery item without starting a diagnostic, and
Resume/Retry are unavailable until it is active again. A continuation from a
lost turn waits for positive process quiescence: a supervisor hello reporting
that process gone, or its recorded exit/kill. Journal absence after replacement
does not count. The owner sees a waiting item and may explicitly override the
overlap risk; the actor and risk are recorded. An intact journal that proves a
continuation never started replaces it with another linked continuation and
preserves the recovery evidence while the agent remains active. If the agent
was stopped before hello, its notes are aborted and the stop is confirmed.
Stopping an unsent diagnostic or correction runs its parent cleanup; stopping
an unsent continuation aborts its notes.
Live process goroutines send through the current machine socket after reconnect.
The supervisor replays each unacknowledged process exit and unreceipted answer
after `hello`, so a disconnect cannot strand those frames or rerun the tool.
Attachments remain under the server's private storage; the supervisor does
not fetch or stage them in a shared checkout. A normal agent turn can use the
`attachment_get` MCP tool, which checks the delivered note and current room
membership. Pi Docker has no MCP credential channel, so its private turn input
includes a turn-scoped HTTP bearer and a server URL reachable from the
container. That bearer is not journaled or included in receipts, and loses
access when the turn ends or the agent leaves the room.
The attachment delivery change requires wire protocol v2. Upgrade standalone
supervisors together with the server; older supervisors cannot connect.
If the supervisor restarts after an answer receipt but before recording process
exit, the receipt proves the answer was stored, but the missing exit makes the
turn `lost` and blocks ordinary dispatch for recovery.
After a process exit is journaled, the supervisor frees that agent's local
admission slot before reporting the exit, allowing an immediate follow-up
delivery after the server settles the turn.

The server retains the raw candidate in its durable receipt before sending
the receipt. A final answer over 2 MiB, or one whose encoded frame exceeds
the 4 MiB machine WebSocket limit, instead travels as a compact
`answer_too_large` marker with the original digest; the receipt records an
`answer too large` error while the supervisor keeps its journaled candidate.
The server parses the canonical answer schema, checks all room targets,
references, attachments, and ask permissions in one transaction, and commits
the entire batch with its messages, inbox items, wake notes, events, and
receipt. Invalid format or references get one correction turn only when the
turn's policy includes verified `no_action_mode`; otherwise the batch is
held. A correction answer uses revision 1. The parent revision's
`correction_requested` receipt becomes terminal `superseded` when the
correction settles. Closed rooms and lost membership hold immediately.
Diagnostic output is stored on the turn for recovery and never posted to a
room. Message insertion resolves room mentions, replies, roles, DM peers and
handoffs, then queues eligible agent notes or records why an agent was not
woken in the same transaction. Human messages without a specific target wake
all AI room members. Routine system chatter never wakes agents; explicitly
targeted system messages can. Leaving a room drops
that agent's queued notes there.

The server runs a dispatcher every 100 ms. It batches at most 20 queued notes
from one room per agent, rotating rooms between passes. Paused rooms/projects,
stopped agents, recovery blocks, provider quota, active or unsettled turns,
writer reservations,
and the per-machine/tool `dispatch_switch_gate` pause prevent ordinary dispatch
without discarding notes. A pending correction or verified read-only diagnostic
turn can bypass the ordinary recovery block while still respecting gates.

Repeated failures before admission back off per agent, doubling from 200 ms
up to 25.6 seconds. A correction or diagnostic that fails three such
attempts is abandoned; a pending special turn that has not been sent is also
abandoned when its room closes, or when it was created at least a minute ago
and its room or project gate is currently paused. Sent turns wait for journal
reconciliation instead: once `turn_dispatch_requested` exists, the dispatcher
does not resend a pending special or linked recovery turn, abandon it as
`dispatch_failed`, or cancel an admitted normal turn as unsent. A linked
recovery turn that fails before admission keeps its evidence through backoff,
including after a third failure.
It clears special-turn in-flight markers after an acknowledgment, socket
replacement, or five seconds; clearing that marker does not permit a resend.
At server startup, turns prepared but never admitted to a machine are
cancelled and settled so their queued notes can be sent again; linked recovery
continuations are retained with their evidence instead.

For normal turns, each agent has a budget of 20 admitted or started turns per
room in ten minutes, counted since its latest reactivation or continue.
Cancelled turns proven never started do not count.
When reached, the server changes its desired state to `not_accepting`, posts
a room notice, and creates a recovery inbox item. Agent-triggered wakes wait
two seconds after that agent's previous turn in the room; human messages do
not wait for that cooldown.

Role text, current room context and the latest active desired revision ride in
each delivery. The supervisor applies a newer revision before admission, so
queued work can resume after reactivation without restarting the socket.
A running turn is bounded by two stall deadlines, ported from v1's pi
watchdog. `AIMEBU2_TURN_IDLE` fires when the harness writes no output at all
for that long (any stdout line counts, including streamed partial output,
not only the events the adapter forwards); it is suspended while a tool execution is in flight, since a long
command is legitimately silent. The supervisor counts a tool as in flight
from pi's `tool_execution_start`/`_end`, Codex's `item.started`/
`item.completed` for commands, MCP calls, file changes and searches, and
Claude Code's `tool_use` / `tool_result` blocks. `AIMEBU2_TURN_MAX` is a
wall-clock cap for the whole turn, tools included. Defaults depend on the
harness: pi gets 600s idle and a 30m cap (as in v1); Claude Code and Codex,
which had no bound before and routinely run long turns, get 600s idle and
no cap. Setting either variable overrides it for every harness; both are Go
durations, `0` disables a deadline, and an invalid value fails startup. The
deadlines apply to the embedded and standalone supervisors alike and stop
counting once the final answer has been received. A tool that never reports
its end keeps the idle deadline suspended, so set `AIMEBU2_TURN_MAX` if a
hung tool on Claude Code or Codex must also be bounded. On expiry the
supervisor logs the stall, kills the harness process group, and reports the
exit as a failure (not an owner cancel) whose evidence names the deadline.
The server then treats it like any other non-terminal exit: the turn's notes
become `unknown`, the agent is recovery-blocked, and a recovery inbox item
carries the evidence, so the delivery is not lost and the owner can continue
it. A failed journal write for a turn's event or final answer is handled the
same way: the supervisor stops forwarding that turn's events, kills the
harness process group, waits for it to exit, and reports a non-terminal
failed exit whose evidence names the journal error. A harness therefore
never keeps running after the supervisor stopped tracking it.

The standalone and embedded supervisors route turns by tool: the fake
adapter provides a short echo for local demos, while installed Claude Code,
Codex, and Pi CLIs use their native adapters. An unavailable CLI does not
appear in the supervisor's catalog.

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

The supervisor refuses to pair, or to run, against a plain-`http` server at an address other than
this machine's (loopback): the pairing token, the machine key it returns and every frame
the supervisor then exchanges would cross the network in the clear. Use an `https` address, or accept the
risk with `aimebu supervisor --insecure --pair '<url>' '<token>'` (and `--insecure` on later runs). The
**Add machine** dialog says so, and its command carries `--insecure`, when the server was reached over plain http at such an address.

A remote supervisor that already runs against a plain-`http` server (paired before this check) stops with that message after the
upgrade. Either move the server to HTTPS and re-pair (or edit the saved URL, `supervisor/machine.key`, or set `AIMEBU2_URL`), or
start it with `--insecure`. The pairing request and the approval wait re-check the transport on every redirect they are sent along, so a
redirect from https to plain http on another host is refused too. The server's own embedded supervisor is exempt: it dials the address
the server itself listens on, so nothing it sends leaves the machine.

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
global npm prefix, once per real file, with `--version`; the test helpers set `AIMEBU2_TEST_RESTRICT_PATH=1`, which limits the search to `PATH` so a test never finds the developer's own CLIs), and which models a Pi,
OpenCode, Codex or Vibe harness lists, native or containerized (run with the harness's
own arguments first, a 20-second limit and an 8 MiB output limit). That command
runs in a fresh private `detect/run-*` subfolder of the supervisor's aimebu
config directory, removed after the command, unless the request names another
existing absolute folder on the machine. A removal error is logged without failing a successful
listing, and the next detection removes leftover `run-*` folders older than one hour. Other files
and recent run folders are left alone. The owner can enter another folder in the Models
panel when a harness refuses to answer from the default one. When the command
fails, the run path and the end of its stderr are in the error shown. It never saves what it finds.
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
The server sends `spawn` after an agent, session, room membership, and checkout
selection commit. The supervisor binds that agent's generation and replies
with `spawn_result`. A spawn carries a stable request ID per agent generation.
Before acknowledging a spawn, the supervisor checks that its checkout path
is an existing directory on this machine, then holds that agent's `aak_` key **in memory only**:
no file under the supervisor's or an adapter's directory has it, so an agent that reads those
directories finds no key, its own or another's. A turn is handed its own agent's key and no other's
(`adapter.AgentToolsAccess`, built from memory per turn; it is never journaled), and the adapter gives
the harness the key in the harness's environment (`AIMEBU2_AGENT_KEY`), never in its argument list
(`ps` shows that to every local user):

The bridge (`aimebu agent-tools`) reads `AIMEBU2_URL`, `AIMEBU2_AGENT_ID` and `AIMEBU2_AGENT_KEY` from the environment it is started with; the first two
are in its MCP entry, the key is the variable the harness fills from its own environment:

- Claude: the `aimebu` MCP entry is inline (`--mcp-config <json>`), a stdio entry for `aimebu agent-tools`
  whose environment says `AIMEBU2_AGENT_KEY=${AIMEBU2_AGENT_KEY}`, which Claude expands from its own environment.
- Codex: the private `config.toml` names the bridge with `env_vars = ["AIMEBU2_AGENT_KEY"]`; the key is only in Codex's environment.
- Pi: the private `mcp.json` has `Authorization: Bearer ${AIMEBU2_AGENT_KEY}`, which Pi expands.
- OpenCode and Vibe: as before, the bearer is in the harness's environment or inline configuration for the turn, never in a file.

**What this does and does not protect.** The key is in no file, but while a turn runs it is in the harness process's environment, so a native agent
(which runs as the same user as the supervisor and has that user's files within reach) can read the environment of a running sibling agent's process
(`ps eww`, `/proc/<pid>/environ` on Linux, `sysctl kern.procargs2` on macOS) and act as that agent while its turn lasts. Keeping keys out of files removes the
easy path (the old shared directory, left-over configs, a backup of the home folder) and shortens a key's life to a turn plus a restart; it is not a
sandbox. **Only run agents in docker (below) is the real boundary**: a Docker launcher does not mount the supervisor's directories and the container has its
own process table. Turn it on for an install where more than one person can start agents.

Correction and diagnostic turns have no toolbox. The supervisor drops the key, and deletes the
config an adapter generated for the agent (Codex's `config.toml`, Pi's `mcp.json` under the runtime root),
when the agent is stopped (once its running turn has ended), so nothing a stopped or removed agent leaves
behind can authenticate. A supervisor that starts removes the `supervisor/agent-tools/` folder an
older version wrote, and the generated adapter configs that held a key.

A supervisor that restarts has lost its keys, and a confirmed spawn is not sent again. It says
`supports_agent_keys=true` in `hello` and lists the agents whose key it holds (`held_keys`); the server
answers in `hello_ack` with `agent_keys` (agent id, generation, a **new** key) for every agent the
supervisor should run and holds no key for. Issuing the key replaces the stored hash, so the key
the old process held stops authenticating (keys rotate on a supervisor restart). A supervisor that does not
say so (an older one, which keeps keys on disk) is sent none.

Claude and Codex Docker adapters build the HTTP toolbox entry from the same in-memory access; nothing is copied into the shared checkout. The key is not in the harness's
argument list: the MCP entry names a variable
(`Authorization: Bearer ${AIMEBU2_AGENT_KEY}` for Claude, `bearer_token_env_var` for
Codex) and the launcher hands the variable over by name through `HARNESS_DOCKER_FORWARD_ENV`, so a Docker
launcher without that feature (`forward_env`) is refused with the way out. The HTTP endpoint uses the
same agent-key authorization and scope as the stdio toolbox's API calls.
Docker's container must be able to reach `host.docker.internal` and the
server's allowlist must admit its source IP. A stale spawn is rejected before
the key is kept; an invalid key sends a failed
`spawn_result` for that agent while the machine connection stays open.

**Only run agents in docker.** An instance setting (Settings → General → Agents, owner or admin, off by default;
`GET`/`PUT /api/agent-isolation`) that refuses native launchers. A native agent runs as the person who runs the
supervisor, with that person's files and other agents' folders within reach; a Docker launcher does not mount the
supervisor's directories. With it on, the server refuses to create or switch an agent onto a native launcher (409
`docker_only`, naming the setting), holds an agent already on one before any turn is made for it (the dispatcher says why once in its rooms, the card shows a
`docker_only` hold, queued messages wait and no turn row is created; `Dispatch` refuses it too, as a backstop), and sends `docker_only` on every spawn and delivery so the supervisor
refuses it too (a stale or older server cannot bypass it). The test agent never runs a harness and is not refused.

A container launcher's version is asked from the project folders of the machine, in turn, until
one answers, within one deadline per launcher (3 seconds when its row is made, 20 on the retry after
connection) however many folders there are, and the first timeout ends the asking (a launcher that hangs
hangs for every folder, and the launchers are asked one after another before any frame is read). The
legacy Docker harness rows are asked the same way (the launcher only runs in a folder its container mounts, and the server cannot tell
which of the machine's folders that is; when none is mounted the row says so once, not with one
folder's text). The harnesses found on the machine ("Found on this machine" in the wizard and in
Settings → Harnesses) get their container versions the same way, within a shared 20 second budget;
without a project folder the entry says why instead of showing nothing. A container launcher's
features (`claude_home_shared`, `forward_env`, and Codex's `codex_home_shared`)
are asked with `HARNESS_DOCKER_PRINT_FEATURES=1`. The Codex probe asks the
Docker daemon for the container home; the others exit before they touch Docker.
They need no project folder: without one the probe runs in
a fresh empty folder of the supervisor's (a launcher too old to know the probe checks the mount
first, fails there, and is reported as too old). When it cannot be answered the launcher row carries a
`version_problem` instead of a bare "unknown": the missing folder, what the launcher said on stderr,
or that it did not answer in time. A timeout gets a prompt 20-second retry after connection;
while a container launcher still has an unresolved version and a probe folder is available,
the supervisor retries about once a minute and reports the row when its answer changes.

A machine's Claude Code sessions are listed by the supervisor on request
(`session_scan`, advertised by `supports_session_scan`): it reads the
configuration folder of the launcher that would continue the session (the
launcher's own `CLAUDE_CONFIG_DIR`/`HOME` references apply), only the first and last 64 KiB
of each transcript, and never writes. The one exception is the attribution scan
(`scan_attribution`, advertised by `supports_session_attribution`): to trace a v1
message number to the session that sent it, it streams the whole transcripts of
the asked folders (bounded in size and time, long lines skipped) and returns only
ids, the agent id a `bus_register` result named and the message numbers the
`bus_say`/`bus_dm` results named, never any text. See [continuing a Claude Code
session](claude-sessions.md).

Before a remote spawn, switch, or reactivation reaches the store, the server
checks the supervisor's `supports_path_probe` hello flag, then sends a
`path_probe` machine frame. The supervisor resolves symlinks and
returns the canonical directory in `path_result`; missing or non-directory
paths fail. An older supervisor does not receive the unknown frame: the
server returns `folder_unverified` without disconnecting it. Missing host
project folders also return `folder_unverified` before agent creation. The path is checked
again when the supervisor receives `spawn`.
For project folder completion, owner and admin requests for a selected remote
machine use its `supports_directory_list` capability. The supervisor returns
up to 200 matching immediate directory names, including symlinks only when
they point to directories; unreadable paths return an empty list. A short
server timeout keeps the field responsive if the machine stops answering. An
older supervisor without the flag gets no listing frame and gives no suggestions.
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
the next `hello_ack`. The server also resends an unconfirmed stop as a
`control` frame, so the machine acknowledges that revision and starts the
force-kill fallback for a process still running after reconnect. The
supervisor advertises stop-replay support in `hello` and acknowledges a stop
only after any running process exits. A project
cannot be deleted while such a stop is unconfirmed on a machine that can
reconnect.
Older supervisors receive the replay to start their kill fallback, but replay
acks do not confirm the stop for project deletion. An older supervisor acks a
control when it sees it and sends no later ack, so its live-control ack counts
only if the agent is already observed stopped (one with no running turn is).
The ack of a stop that interrupts a running turn is dropped and that stop stays
pending, so deleting the project is refused (409) until the supervisor is
upgraded or the machine is revoked.
A lost turn remains unsettled until recovery establishes quiescence.
Dedicated handoff turns carry their request as a synthetic delivery note.
Admission checks queued source notes separately, so that request can reach
the supervisor even though it has no note row.

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
exit, final answer and receipt evidence. For a continued v1 Codex, Pi,
OpenCode or Vibe session, the first delivery instead carries the recorded
handle, `native_session_started`, and `import_legacy_session`. A supervisor
advertising `supports_legacy_session_copy` copies that native state from the
machine's user home into the agent's private runtime before launching the
resumed turn. A copy failure is reported through the same journaled start
failure path; an older supervisor is refused at spawn.
The final-answer event
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
provider quota only from the tool's own wording, and only for Claude, Codex, Pi,
OpenCode and Vibe: Codex needs `usage limit` in stderr or the error evidence; Claude
needs `limit reached` there, but not `rate limit reached`; Pi, OpenCode and Vibe count
only the provider error they reported (their stderr is ignored; Vibe's is its last `Error:` line) and need
`usage limit`, `usage_limit`, or a `weekly`, `hourly`, `monthly` or `daily`
limit, never text that mentions a `rate limit`. A bare HTTP 429 or a generic
rate limit is ordinary recovery, not a quota. A reset time is read from an
ISO timestamp after `reset`, `retry` or `try again`, or from `resets in` or
`retry in` N minutes or hours (up to 24 hours). It records a known failed turn, requeues its notes,
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
delivery starts a fresh Codex thread. A resumed Claude turn that reports
`No conversation found with session ID:` is handled the same way. A resumed
OpenCode or Vibe turn that prints `Error: Session not found` on stderr is
handled the same way (the event's
reason is `opencode_session_missing` or `vibe_session_missing`). It does not apply to a turn that opened
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
because its previous process may still be running. If the harness never
launched, the supervisor reports a definite start failure. Recovery shows the
start error and offers Retry or Abandon without a diagnostic or a warning that
work may have run. A missing tools access error tells the owner to start the
agent again before retrying.
Owner/operator decisions
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
references, attachments, ask permissions, and live agent mentions in one transaction, and commits
the entire batch with its messages, inbox items, wake notes, events, and
receipt. Invalid format or references get one correction turn only when the
turn's policy includes verified `no_action_mode`; otherwise the batch is
held. A correction answer uses revision 1. The parent revision's
`correction_requested` receipt becomes terminal `superseded` when the
correction settles. Closed rooms and lost membership hold immediately.
An answer mentioning active agents outside its target room commits when the
author shares another open project room with each absent addressee. The
server posts a targeted pointer there. Only the addressed agent can read
the cited message by number; project owners and operators can inspect it on
the web. The pointer does not open the rest of the target room.
Routing is preferred for dispatcher-to-leader reports. Without a shared
room, an answer whose only agent addressee is absent is held as a whole
unless the message sets `force:true`. The receipt names the missed agent
and offers `dm_agent` or `force:true`. A verified no-action mode gets one
correction turn. Other harnesses receive a targeted one-sentence system note
naming the held outbox message, without quoting the mention or suggesting an
inaccessible room; no candidate message is published.
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
### Cache keep-alive (on by default)

A harness's prompt cache is what makes a turn after a long pause cheap. Claude Code writes it with a **one-hour**
lifetime (measured: all cache creation is `ephemeral_1h`, and a turn after 450 s of silence still read it, 4 253
tokens read, 177 written; dev's own usage records show 155 k and 90 k cached tokens read after a 37-minute gap;
the 621 Codex session logs on one machine show 96 % cache hits at 5 to 10 minutes and 18 of 19 at 10 to 15). When the
cache expires the next turn pays to write the whole context again, so for an agent that is active and idle the
supervisor sends the harness a tiny **ping** shortly before the cache would expire.

- **What a ping is.** The agent's last real turn, run again as a cold resume (or into its live warm process, if
  it has one: a ping never starts a warm process) with *exactly* that turn's arguments, environment and folder,
  and a user turn that only says `keep_alive` and asks the model to reply with an empty `messages` array. The
  instruction is in the user turn, not the system prompt: the system prompt, the schema and the tool list are the
  cached prefix, and a ping that differed there would not read the cache (measured: the same ping without
  `--json-schema` re-created 3 755 of 3 954 tokens, at the price of a cold turn). The adapter test pins that the
  arguments, the environment and the warm fingerprint are the real turn's.
- **It cannot act.** The harness is asked in words not to use the toolbox, and that is not what keeps it from acting
  (the words are in the user turn so the cached prefix stays the real turn's, and the process arguments and
  environment stay byte-identical, so the key in them is the agent's own). The server enforces it: the supervisor
  sends `keep_alive_scope` (`scope_on` true, the agent id) before the ping's process exists and starts the ping only
  once the server answers `keep_alive_scope_ack`; while the scope is on, every agent-tools call made with that
  agent's key (REST and MCP alike, in `requireAgentTool`) is refused with **403 `keep_alive_scope`** before any
  handler runs, so a ping that calls a tool anyway posts nothing and changes nothing. The supervisor lifts the scope
  (`scope_on` false, answered the same way) only after the ping's process is confirmed gone and before the ping
  counts as ended, so a real turn that follows is never refused. Without an answer the ping does not run (it fails
  closed and counts as a failure). A scope nobody lifted (a supervisor that died) lapses on the server after three
  minutes (`protocol.KeepAliveScopeTTL`; a supervisor that holds an unconfirmed ping in quarantine renews it, at the
  start of the quarantine and then every minute, so an orphan is never free to use the toolbox while the agent is
  still blocked); it is not dropped when a connection comes or goes, so a supervisor that reconnects mid-ping leaves the
  ping scoped. **The limit:** this covers the aimebu toolbox only. The harness's own tools (a shell, file edits, and for Codex shell
  commands) are outside it, so there a ping relies on the instruction in its user turn ("no tools, no messages" for
  Claude, "no commands, no messages" for Codex), which a model could still ignore; the server cannot refuse those.
- **It is not a turn.** No turn exists for it on the server. Nothing is posted, no recovery item can come of it,
  no read marker or note moves, the agent card does not change state, and the model's reply is thrown away. The
  supervisor sends the server one `keep_alive_report` frame (what it read from and wrote to the cache, its cost, how
  long it took, or why it failed; never what the model said), writes one line to its log, and the card shows the last
  ping. A ping whose cache write exceeds its cache read **missed** (the cache had expired, or its arguments differed)
  and the card says so; the log line carries the numbers so the interval can be tuned from what happened. A ping adds
  about 150 tokens of context and about 10 KB of transcript each (measured), which is one reason the pings stop.
- **When.** One interval after the last real turn ended cleanly: Claude **50 minutes** (at most 55: the
  cache lives an hour, so a later ping always finds it gone, and a larger stored value is read as 55), Codex **10 minutes** (an
  assumption: Codex states no lifetime, so each ping's cache read and write are logged to tune it from). The
  supervisor skips a ping quietly until the real turn has reported a native
  session handle; there is no resumable session to ping before then. Both
  adapters implement `adapter.KeepAliver`: Codex runs `codex exec resume` with the real turn's arguments and the
  instruction in the turn JSON after the unchanged instruction text; its cost is the price list's, per ping,
  where Claude's is the difference of the session's running total. Settings -> General -> Agents holds a switch (on by default), the interval
  per harness (0 is off for that harness) and "stop pinging after N hours without work" (24 by default); only the
  owner changes them. The server puts the setting for the agent's tool on every delivery (`keep_alive`:
  `interval_seconds`, `stop_after_seconds`); a supervisor that does not know it never pings. A delivery only
  reaches an agent that has a turn to take, so the same settings (and the warm window) also go to every
  warm-capable supervisor in an `agent_settings` frame: when it connects, when the owner saves a change, and with
  the server's 15-second check. On receipt the supervisor applies the warm window (off ends every idle process),
  forgets the keep-alive of an agent whose interval is now 0 (ending a ping that runs), and re-arms the timer of
  the others from their last real turn at the new interval. Turning pings on starts them at an agent's next real
  turn (an idle agent has no turn to ping from).
- **Never in the way.** A real turn stops the pending ping and arms a new one when it ends (a turn that does not
  end cleanly arms none). A delivery that arrives while a ping runs ends the ping: a process on this machine is
  killed; one in a container is told to terminate (SIGTERM, then the launcher's own cleanup, up to about 12 s). A
  ping that finds a turn running looks again a minute later. The ping's own time limit is 90 s.
- **A ping's end in a container is confirmed like a warm process's.** Killing the wrapper confirms nothing about the
  process inside the container, so such a ping counts as ended only when it finished by itself (exit status 0, or any
  status from 1 to 124: `docker exec` passes the inner process's own status on, so an API error or a failed turn
  has ended inside the container; 125 and above are the docker CLI's own failures or a signal's 128+n, which say
  nothing about what runs in the container and are not confirmed), or was told to terminate and its wrapper exited with exactly 143 (the launcher advertises `stop_on_signal`), or a warm
  process was closed through its launcher. A container whose launcher does not advertise `stop_on_signal` is not
  pinged at all. When the end is not confirmed (the wrapper was killed after its grace) the ping is not over: the
  agent stays blocked (a delivery for it waits, in its own goroutine, for the quarantine to be over and then runs; a stop is not
  acknowledged and its key not dropped, an account switch is refused) and the server keeps refusing the agent's
  toolbox (the scope is not lifted) for a 150 s quarantine, after which both are lifted, since nothing can observe
  the orphaned process any more and a ping's tiny turn does not outlive that. The timeout is accepted rather than
  a confirmation because the ping is a tiny turn with a refused toolbox, and the harness container's own watchdog
  ends an orphan (a process whose wrapper has gone) within about 15 s: 150 s is ten times that. Where it did not
  work the expiry is not silent: it logs a warning and adds to the **unconfirmed ping ends** counter that the
  supervisor's `warm_state` report carries (`unconfirmed_ping_ends`; the machine's card in Settings → Machines
  shows it once it is above 0).
- **A lift that was not answered is owed.** If the scope lift after a ping gets no ack (the connection dropped),
  the supervisor remembers the lift per agent (with the ping it is for) and sends it again, waiting for the ack, on reconnect and before a
  real turn's process is started, so a real turn's toolbox is never refused for a ping. A ping whose scope-on gets no ack owes a lift too (the
  server may have applied it), so the next real turn is not refused for the scope's lease. A real turn whose owed
  lift still cannot be confirmed is not started (the failed start is retried like an unconfirmed ping end).
  The server also lapses a scope by itself after its limit. Every scope frame names its ping (`scope_gen`, the
  ping's turn id), one scope transition per agent is in flight at a time, and a ping settles the lift an earlier
  ping still owes before its own scope begins; the server ignores a lift whose `scope_gen` is not the ping scoped
  now (a late, owed lift of an older ping must not clear a newer ping's guard). A frame without `scope_gen`
  (an older supervisor) always applies.
- **Waits never hold up the supervisor's frame loop.** A delivery that has to wait for a ping runs in a goroutine
  of its own; a stop or spawn waits a moment for the ping it ends and leaves the rest to the stop's own retry.
- **Stops.** After the owner's hours without real work, when the agent is stopped, spawned again or loses its key,
  when the setting is off, at shutdown, and after three failed pings in a row (a success in between starts the
  count again). A fork, a correction, a diagnostic or a tool whose adapter cannot ping arms nothing.
- **Cost.** (The cost of the first ping after a supervisor restart is 0 in the supervisor's own report, which has
  no earlier total to take a difference from; the server records a ping's cost as the difference from the
  session's last total it holds, which survives a restart, so the usage figures and the card are not affected.)
  A ping reads the agent's whole context from the cache: at the cache-read price (a tenth of the input
  price) of that context. Measured on Haiku with a 4 k context it is $0.0001; for a 90 k-token Opus context about
  $0.045, so up to 28 pings after the last turn (24 h at 50 min) is about $1.3 per idle agent per day at most. The
  agent's card says "Cache kept warm every 50 min, up to 28 pings after its last turn ≈ $1.3": the measured cost
  of the last ping times the pings in a stretch, and before there is a ping the price list's estimate when it has
  the model. The last report per agent is kept in memory on the server and the supervisor logs each one. The spend is also
  written to `usage_record` (source `keep_alive`, no turn, the agent's room and project) so usage totals, limits and
  the cost views count it; a Claude ping's cost is the difference from the session's running total, which is kept,
  so the next real turn is not charged for the ping.

### Warm agents (5 minutes by default)

*Instance setting.* The window is set by the owner in Settings → General → Agents ("Keep agents warm
(minutes)", 5 until the owner saves another, 0 is off, at most 1440; `GET`/`PUT /api/agent-isolation`,
`warm_idle_minutes` with `warm_idle_saved`, owner only to change). The order is **the owner's saved value, then
`AIMEBU2_WARM_IDLE` on the supervisor, then 5 minutes**: the server puts a window on a delivery and on an
`agent_settings` frame (`warm_idle_seconds`) only when the owner has saved one (a saved 0 is sent and means
off); with none saved the field is absent and the supervisor works to its own (`AIMEBU2_WARM_IDLE`, a Go
duration, 0 is off, unset or unreadable is 5 minutes). A supervisor applies a window before the turn, and
turning it off ends every idle process at once. Codex app-server warm mode
also needs the owner's `codex_warm_enabled` opt-in (off by default); turning that
off retires idle Codex processes while cold Codex turns and keep-alive pings
continue. It says the window it works to in its `warm_state` report
(`idle_seconds`), and a card's warm or sleeping follows that and nothing else: a live idle process is "warm"; no
process is "sleeping" only when the supervisor says it works to a window above 0; before its first report the card stays cold, while a report
without `idle_seconds` is unknown (the card's `warm.state` is `unknown`) and the card says neither warm nor sleeping. So a supervisor started with
`AIMEBU2_WARM_IDLE=0` shows no "Sleeping".

*Mixed versions.* The server of the build before this one sent `warm_idle_seconds: 0` on every delivery when the
owner had saved nothing, so a **new supervisor under that older server stays cold** (it reads the 0 as a saved off)
until the server is updated or the owner saves a window. A **new server with an older supervisor** is shown as
unknown (no "Sleeping"): the older supervisor's own default is off and it does not say its window.

*What the record says.* A turn that ends cleanly in a process that is kept warm is recorded as `turn_finished`
(`{process: "kept"}`): nothing exited, so there is no `turn_process_exit` with exit code 0 for it (the supervisor's
process frame carries `process_alive: true`). A process that ended with its turn, a failed or cancelled turn, and
every turn without a warm process keep `turn_process_exit` as before. The real end of a kept process is recorded when
it comes, as `warm_process_ended` with the reason (the supervisor's own words: "the idle window ended", an account
switch or other retire, "it ended while idle" for a crash, memory pressure, the supervisor stopping), sent in a
`warm_process_ended` frame that the server accepts only from the machine that runs the agent. A process that ends
while it works again is reported by that turn's exit, not twice. The web treats `turn_finished` like the exit it
replaces (an agent's activity, the refresh of its card) and refreshes a card on `warm_process_ended`, so
the warm or sleeping state follows the process without polling.

*What the owner sees.* Every agent's card has one status dot, and for an idle Claude agent, or an opted-in Codex agent, it says whether the process is warm: green without a ring is online and cold (the
next answer starts the agent), green with a ring that pulses (as v1's presence dot did; a ring only under reduced motion) is online and warm, red is offline, and orange is a state
that is not plain online or offline: a paused or waiting agent (its tooltip says which), or a Claude agent whose warm state is `unknown`. The dot's tooltip says it in words
("Online · warm, the next answer skips the start (≈4 min left)", "Online · cold…", "Offline", "State unknown"), and the same sentence is there for a screen reader. A working
agent is green (its process is alive; whether it was warm is not told). An idle agent on a harness without a warm process (every harness except Claude and opted-in Codex) shows a neutral dot and “Warm n/a”, with a tooltip explaining that the harness does not keep its process warm. The members panel's people have the same 8px dot. It reads the card's `warm` field, refreshed when a
process starts or ends, with no polling of its own (the minutes follow the panel's clock).

A supervisor that keeps processes warm sends the server a `warm_state` frame (at
connect, and whenever the picture changes: a process goes idle, is taken for a turn, ends): the agents with a
live process (idle until when, or working), `refusals` with each agent's last classified reason a turn ran cold,
the counts started and reused since it started, the memory of
the warm processes it runs itself, and what every running container uses. An idle Claude or opted-in Codex agent's card says
"Warm · sleeps in N min" or "Sleeping" (`warm` on the card: `state` warm or sleeping, `sleeps_at`; absent when
the window is off or does not apply to the agent), and the machine's card in Settings → Machines says how many
agents are warm and what the processes use (`warm` on the machine row). The memory is **not per agent**: the
harness container is shared by all agents and Docker reports no finer, so it is labelled container memory,
all containers. A refusal clears on the next real turn and when the agent stops or disappears from the server's desired state. Its reason is limited to 240 Unicode characters; the server gives viewers a generic message instead of the specific configuration key.

A Codex warm process is stamped with the `auth.json` size, modification
time, and content hash. A different stamp retires it before another turn. Docker
Codex needs `codex_home_shared` and `stop_on_signal` from harness-docker, and
the launcher's cached feature probe must report the running container's
absolute `codex_home=` path. The Codex probe has its own 10-second deadline;
missing, failed, or timed-out answers keep warm mode off for that launcher.
When warm mode is on and the probe answers without `codex_home=`, Settings
and Doctor show that the container may be stopped or the launcher may be too
old; the feature output cannot tell which case applies. The warning clears when a
later probe reports the home.
When a normal Codex Docker turn wants warm mode but the cached home is missing,
the supervisor starts a background reprobe at most once every two minutes per
launcher. That turn runs cold; if the probe finds the home, the next turn can
run warm. Replacing the launcher also refreshes the cached answer. A stamp
changed during a turn is accepted only when the file still names the same
account. Codex app-server may request external
auth tokens on a 401; aimebu does not refresh a token there. It rereads the
live token-only target and responds only with a newer access token for the
process's original account, otherwise the turn fails and the process ends.
Warm Codex first requires token-only credentials without a refresh token, or
an API key without refresh credentials. Full credentials run cold, so a warm
process never keeps a refresh token while it is idle or in a turn.

*Account switch.* A warm process holds the login it started with. The guarantee: **a switch of account (by
hand, or the automatic one) and the removal of the active profile go on only when every idle warm process of the
tool on the host is confirmed gone, and none can have been started since.** In order:

1. The server holds new turns of the tool on the host (the agents wait in the queue; a hold of its own, apart from
   the limit guard's) and waits for the transactions that are open, so a turn that was being dispatched is
   recorded by then. Only then does it check for running turns (`409 agents_running`).
2. It sends the host's supervisor a `retire_warm` request for the tool (the login is on the host, so no other
   machine is asked) and goes on only when it answers `ok`, that is, every idle process of the tool is confirmed
   gone. `error`, no answer within 30 s, or a supervisor that goes away before it answers refuses the switch
   (`409 warm_agents`; the automatic switch counts it as agents running).
3. The swap itself checks for running turns once more under the switcher's lock. The hold of turns ends when the
   switch is over, whatever it came to.

The server's view of a host can only be ahead of reality, never behind. A supervisor therefore tells the server
before it keeps a process: when a turn ends and its process would go idle it sends `warm_claim` (the count of
processes it has running, warm and pings, including that one) and keeps the process only once the server answers
`warm_claim_ack`; with the socket down, no answer within 5 s, a stale connection, or a count the server could not
save (it answers only after the count is in the database, and memory is never ahead of what was saved), the process
is ended instead and the next turn runs cold. The agent's slot is not locked while the answer is waited for (a stop
or a spawn on the loop that reads the server's frames needs it): the process stays the slot's busy one meanwhile,
and whether to keep it is decided again after the answer. A ping does the same with its scope-on frame (which raises the server's count to at
least 1, saved before it is answered, and is answered before the ping's process exists; one that cannot be saved
is not answered, so the ping does not run). Decreases (a process ended) are not claims: they go in
the `warm_state` reports, and a stale higher count only makes the server refuse, which is safe. Everything the
supervisor says of its running processes (reports, claims, scope-on) is numbered from one counter (`seq` /
`warm_seq`, taken before the count it describes), and the server applies and saves counts under one lock in that
order: a number older than one it holds on that connection is ignored, and so is a count from a connection that
is no longer the machine's, so a late older report can never overwrite a newer count (a supervisor that restarts
begins its counter again, on a new connection). A report's count is its live warm processes of any state
(`processes`; the `agents` list is only the picture for the screen) plus `pings_active`.

A host whose supervisor is **not connected** is not taken to have no warm processes: a dropped connection or a
server restart leaves a supervisor's idle processes running. The switch is refused (`409 warm_agents`) until the
supervisor is back and can answer, unless the server knows the host has none: it never connected, its supervisor
cannot keep anything warm, or its last `warm_state` (a supervisor that stops cleanly sends one more, synchronously,
after it has ended everything and before the connection closes) listed none, **no warm slot and no ping** (`pings_active`
counts the cache pings that run or are held in quarantine, which are processes on the login though they hold no
slot), and the server holds no live ping scope for the host (a ping scopes its agent before its process exists, so
one that began just before the drop is seen even if its report did not arrive). The last report is kept in the
database (the global setting `machine_warm_seen:<machine id>`, count and time; no table of its own), so a
server that restarted still has it and switching is not wedged when the host's last word was none; revoking the
machine removes it. A host with no report at all that had connected before is assumed to have some.

A cache ping is a turn on the same login that the server's check for a running turn cannot see, so the request
also covers pings: the server first holds the tool's pings back (the tool joins `paused_tools` in a
`keep_alive_paused` frame sent before `retire_warm`, and stays in it until the switch is over, whatever it came to),
and the supervisor ends every ping of the tool that runs and waits for a confirmed end (a container ping whose
launcher cannot confirm refuses the switch) before it answers. A ping ended this way is not counted as a failure and
its timer is armed again, to find the hold and look again later; the pings go on when the server lifts the hold.
A supervisor announces `supports_warm` in `hello`; the server asks only one that does (a machine without
it has no warm process, and `aimebu doctor` says so).

With a window (5 minutes unless set otherwise, see above) a supervisor keeps an agent's
harness process up for that window after a turn, so the next message goes straight into the
live process instead of starting a new one and resuming the session (on dev that start was
4 to 5 of the 6.5 to 7.5 seconds a short reply took). Only an adapter that implements
`adapter.WarmAdapter` takes part: the Claude Code adapter and, when enabled,
the Codex adapter. When the window ends the
agent sleeps: the process is ended and the next turn runs cold, as before. A correction, a
diagnostic and a fork always run cold, and ending a live process comes first, so two
processes are never on one session.

- **One state machine per agent.** A warm process lives in the agent's slot whatever it is doing
  (starting, running a turn, idle), and everything that changes it holds the slot's lock for
  the whole change: a delivery taking it, a turn ending, the idle timer, a stop, a key change,
  the memory guard and the crash watcher. So a stop that arrives while a delivery is still handing
  the turn to the process waits for it and then ends it (a stop kills a process that is running a
  turn too, and the stop is acknowledged and the key dropped only after); a key change in that
  window leaves the running turn alone but the process does not go idle afterwards; and a turn
  that ends as a stop lands is settled against the desired state the stop already set, so no
  process is left idle after a stop. An idle process is not an active turn.
- **Fingerprint.** At every delivery the supervisor asks the adapter what a process for the turn
  would be started with (command, arguments including the system prompt and MCP configuration,
  environment with the agent key as a hash, directory) and adds the login (below); a live process
  whose fingerprint differs is ended and the turn gets a new one. So a role, prompt, model,
  launcher, folder, key or login change is seen without a trigger list.
- **Login.** The login is the Claude credentials file in the configuration directory the
  *launcher* gives the process (`CLAUDE_CONFIG_DIR` through the owner's variable references, else
  the home's `.claude`), by size, modification time and a hash of the content. A refresh by someone else or an account switch
  between two turns changes it, so **the next turn after one is cold** (the end is reported as "the login it works under changed",
  where a change of the command, arguments or environment says "what it was started with changed"). The stamp is taken again when a turn
  ends, because Claude renews an expired token itself and writes the file during the turn: when the file still names the process's account (the
  account-state file's `oauthAccount.accountUuid`, else the e-mail address: the one beside the credentials for a process started with
  `CLAUDE_CONFIG_DIR`, the home's `.claude.json` without it, the newest that names one) the new stamp is the process's own and is adopted. Another
  account (a `claude /login` in a terminal during the turn), or one that cannot be told, ends the process with the turn ("the login it works under
  changed"), so a login that was never checked is not kept running; an agent whose account file names nobody therefore runs cold after each renewal. When it cannot be told reliably the turn
  runs cold and no process is kept: no credentials file (a login in the macOS keychain, or none),
  or a Docker launcher that does not report `claude_home_shared`.
- **Ended at once** (and reaped before the stop is acknowledged and before the key is dropped):
  stop/pause/archive, a new spawn of the agent, a dropped key, a native launcher refused by
  docker-only, shutdown, and `RetireAllWarm` (for a switch of account). A turn that does not end
  cleanly (error result, crash, cancel) ends the process too; an interrupt and a malformed input
  line make the CLI exit, so any abnormal event simply means the agent is asleep. An idle process
  that ends by itself is noticed at once (the agent is asleep from then, and its last stderr goes
  to the supervisor's log, bounded).
- **Unconfirmed ends.** An end is confirmed (the process is reaped); a kill that is not confirmed
  is tried once more. If a process still cannot be confirmed gone, nothing goes on as if it were:
  a stop's acknowledgement and the key's drop wait and the stop is retried in the background (the
  ack goes out when the process is finally gone), a spawn is refused, and no other process, warm
  or cold, is started on the session (the turn fails to start and the next delivery tries again),
  so there are never two processes on one session.
- **Never fails a turn.** A process that cannot be started, noted in the journal or handed the
  turn is reported and the turn runs cold.
- **Limits.** `AIMEBU2_WARM_MAX_LIFETIME` (default 1h): a process that old is not reused and is
  not kept idle (nothing refreshes credentials inside one). The memory guard
  (`AIMEBU2_WARM_MIN_FREE_MB`, default 1536, and `AIMEBU2_WARM_MIN_FREE_PERCENT`, default 10)
  ends the longest-idle process, one per ten seconds, while free memory is under either; there
  is no cap on how many agents are warm. With a Docker warm process the guard also asks Docker
  (`docker info` for the Docker host's memory, `docker stats` for what the containers use), because
  the memory is the container's, not the supervisor's; Health reports the processes the
  supervisor runs itself and, separately and labelled container-wide, what every running
  container uses (Docker reports no finer, and the harness container is shared by all agents), not
  the warm agents' own. Measured: a Claude Code
  process is about 240 MB native and 250 to 330 MB inside the harness container.
- **Orphans.** The journal notes each warm process with its own start time and keeps the note until
  the process is known to be gone; a process that cannot be noted is not kept warm. A supervisor
  that starts ends the ones a dead predecessor left (a pid with another start time is never
  touched). With harness-docker the wrapper's own session watchdog (it watches the wrapper's
  pid and start time) ends the process inside the shared harness container when the host wrapper
  is gone, so there is nothing for the supervisor to label: the harness container is shared by
  every agent and a warm process is one `docker exec` in it.
- **Containers.** `docker exec` passes no signal on, so killing the host side leaves the process in the
  container until harness-docker's session heartbeat sweeps it (about fifteen seconds), and neither the
  wrapper's exit after a kill nor the passing of time proves it gone. A process in a container is
  therefore kept warm **only when its launcher advertises `stop_on_signal`** (listed by
  `HARNESS_DOCKER_PRINT_FEATURES=1 claude-docker --version`, so it is in the launcher's features), and
  its end is confirmed only two ways: the clean one (input closed, the CLI exits, `docker exec`
  returns, the wrapper's session cleanup runs and it exits 0), or, with that capability, a
  terminate: the wrapper traps TERM, ends the session in the container, waits for it and only then
  exits 128+signal, so exactly 143 after our SIGTERM. Any other status, a kill, or no capability
  is *unconfirmed*. An unconfirmed end follows the rules above (stop ack and key drop wait, no
  replacement on the session) and is never cleared by elapsed time; it stays until the supervisor
  restarts, whose start-up sweep and the harness's own heartbeat deal with the leftovers. A launcher
  without the capability runs its agents cold on every turn, as before. A process in a container is
  always closed first (input, then SIGTERM with about eight seconds for the wrapper's own cleanup, only
  then SIGKILL). A running container gets the faster in-container wrapper only after
  `harness-docker-ctrl rebuild`; until then the terminate takes about two seconds.
- An idle process is ended gracefully first (its input is closed and it gets a few seconds to go;
  with a Docker launcher that lets the wrapper clean its session up properly, which is also the
  roughly one second the wrapper's cleanup costs a one-process turn), and killed if it does not;
  a process that is being replaced for a delivery, or whose turn failed, is killed at once. The
  harness-docker wrapper (`docker exec -i` into the long-lived harness container) is what a
  Docker launcher runs, so a warm Docker agent is one `docker exec` kept open.
- Output the process writes to stderr goes to the log of the turn that is running.

A running turn is bounded by two stall deadlines, ported from v1's pi
watchdog. `AIMEBU2_TURN_IDLE` fires when the harness writes no output at all
for that long (any stdout line counts, including streamed partial output,
not only the events the adapter forwards); it is suspended while a tool execution is in flight, since a long
command is legitimately silent. The supervisor counts a tool as in flight
from pi's `tool_execution_start`/`_end`, Codex's `item.started`/
`item.completed` for commands, MCP calls, file changes and searches, and
Claude Code's `tool_use` / `tool_result` blocks. `AIMEBU2_TURN_MAX` is a
wall-clock cap for the whole turn, tools included. Defaults depend on the
harness: pi gets 600s idle and a 30m cap (as in v1); every other harness
(Claude Code, Codex, OpenCode and Vibe), which had no bound before and
routinely run long turns, gets 600s idle and no cap. Setting either variable overrides it for every harness; both are Go
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
Codex, Pi, OpenCode, and Mistral Vibe CLIs use their native adapters. An unavailable CLI does not
appear in the supervisor's catalog.

## Agent output

Each agent's harness writes to stderr while a turn runs (warnings, launcher
errors, what a tool printed). The supervisor keeps that in a per-agent log on
the machine that ran it: `supervisor/agent-logs/<agent-dir>/stderr.log`, with
rotated backups `stderr.log.1` to `.3`. It is the harness's stderr only: stdout
is its event stream (the whole conversation and every tool result) and is not
captured.

- **Capture.** The supervisor gives each turn a process-only sink
  (`adapter.TurnSpec.StderrSink`, never journaled) and every adapter (Claude
  Code, Codex, Pi, OpenCode and Vibe, native and Docker) tees the harness's stderr
  into it beside the 4 KiB tail it already keeps as exit evidence. A log that
  cannot be opened is reported and the turn runs without one.
- **Format.** One line per line of output, each prefixed with its UTC time,
  and a marker line `=== turn <id> gen <n> <mode> ===` before a turn's first
  line, so output of consecutive turns can be told apart. A turn that prints
  nothing adds nothing. A line longer than 256 KiB is written in pieces, cut only
  at a space or tab (so never inside a key or a multi-byte character); a line
  with no space in it is held until it ends, and one that reaches 1 MiB with no
  space at all is replaced by `[long token redacted; rest of line dropped]` and the rest of that line dropped, up to its end.
- **Redaction.** Agent keys and bearer tokens are removed before a line is
  written (the same rules as the exit-evidence tail), whichever way the stream
  was cut into chunks, including a `Bearer` at the end of one line and its token
  at the start of the next. Whatever else a tool printed is not recognisable,
  which is why the log is only ever shown to an owner or admin.
- **Where, and how big.** Files are 0600 in 0700 directories. An agent's
  directory is named from a slug of its id and a hash of the whole id, never the
  id itself, so nothing in an id can reach a path. A file rotates at 1 MiB with
  three backups (4 MiB per agent).
- **Retention.** A file whose last write is more than 14 days old is removed; if
  all agents' logs together exceed 256 MiB the oldest files go first, except
  anything written in the last five minutes or of an agent whose turn is
  running. The supervisor sweeps at start and daily.
- **Reading.** The output never leaves the machine on its own. The server asks
  for a bounded tail (`agent_log_read`, see [the contract](contract.md)) when a
  project's owner or an admin opens the Output section of an agent's profile, so
  a remote machine that is offline, or a supervisor without
  `supports_agent_logs`, simply has nothing to show (the web says which). An
  agent that moved to another machine keeps its earlier output on the first.
- **Size.** The server asks each remote machine only for the total size of its
  logs (`agent_log_usage`, advertised as `supports_agent_log_size`) to show in
  Health under "Remote agent logs"; the host's logs are counted with the rest of
  `supervisor/`.
- **Prune.** `aimebu prune` leaves the logs; `aimebu prune -a` removes them with
  the rest of `supervisor/` (see [pruning](prune.md)).

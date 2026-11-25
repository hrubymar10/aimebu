# Supervisor and delivery log

`aimebu server` starts or adopts a local supervisor, a process of its own.
It uses `server/host-machine.key` and dials the server over loopback. There
is no in-process mode any more: a server that still has `AIMEBU_SUPERVISOR`
set logs one line saying the setting is ignored and starts as usual. The
server can restart without interrupting active turns or commands: the
supervisor reconnects and replays its durable journal state. A normal
`aimebu server stop` leaves it running; `aimebu server stop --with-supervisor`
stops both.
The agent-tools bridge retries connection failures and maintenance responses
for up to a minute; keyed writes retain their request key. Other server
errors reach the agent immediately.
`aimebu supervisor status` shows its PID (one line per instance while several run side by side), and `aimebu supervisor restart`
asks the running server to replace it using the server's environment. If the
server is absent, the command starts a replacement from the CLI environment.
If a server holds the config directory but does not answer the request, the
command fails; retry it or restart the server. It never replaces that server's
supervisor from the CLI environment.
The server compares a local supervisor instance's build and turn environment
fingerprint with its own. On a remote machine the newest connected matching
build is active, or the newest connected hello when no build matches; remote
environment fingerprints are host-specific and are not compared. One machine may have several supervisor instances,
each with its own journal and WebSocket connection. The matching instance takes
new turns and commands. A mismatched or drain-only instance retires: it accepts
no new work, retires idle warm processes, and keeps its current turns and all
command jobs, including native jobs, until their results and acknowledgements
are complete. A new matching instance can serve other agents immediately.
During a swap, dispatch keeps using the connected old instance until the new
instance's `hello_ack` is queued; only then is the old one told to retire.
For each agent, the server waits for the retiring instance's open turn and warm
process to end before sending that agent's next turn to the new instance.
There is no replacement grace or automatic interruption of a turn during a
normal swap. Restart now remains an explicit interruption. A bounded handover
is reserved for a disconnected or empty instance that does not exit. An
instance that crashes with open work is recovered from its own journal.
See "Side by side" below for how the instances are started and finished.
If the executable on disk is newer than the running server, restart the
server to use that build; repeated supervisor replacements do not help.
After 24 hours without
a server connection and with no active turn or command, it exits by itself.
Its output is in `supervisor/aimebu.log`.

### Side by side

An upgrade starts a new supervisor instance next to the old one; nothing is
replaced. The server tells instances apart by `journal_id` and routes new work
to the one that matches its build and environment (see above). The local manager
(in the server process) keeps the instances running:

- **A mismatch starts the next instance.** When no instance is active and the
  server has the old one connected as retiring, the manager starts
  `aimebu supervisor --local --journal supervisor/journals/<name>.sqlite`. The
  old instance keeps its turns and jobs on its own journal and leaves by itself
  when it owes nothing: no turn, no job (native ones included), no warm process
  and nothing the server has not acknowledged. It then removes its journal and
  exits 0, so an old build lives as long as its longest job. The log shows
  `supervisor swap: no instance is active; starting a new one on <name> next to
  pid P` and `started supervisor instance pid P on journal <name>`. An instance
  that holds its lock without connecting is given 30 seconds (a healthy one
  connects within seconds, also after a server restart) before another is
  started next to it, and at most three unconnected instances are tolerated.
- **A crash is finished by a drain instance.** A journal whose lock is free and
  that still owes work (an open turn, a job, an end the server never
  acknowledged, a warm process of the dead holder) gets `aimebu supervisor
  --local --journal <path> --drain` of the current build. Its hello says
  `drain_only`, so the server sends it nothing new; it finishes the journal the
  way a replacement used to (the server replays journaled ends or loses the
  turns with recovery, native jobs are adopted and report their real exit code)
  and exits, removing the journal. With no instance active at all, the same
  journal is instead taken by a regular supervisor, which is what takes the
  machine's work again. A journal that owes nothing and whose lock is free is
  removed, except the legacy path. If a journal file is gone altogether and its
  instance has not connected for a minute, the manager tells the server the
  instance is gone, and the work the server still lists on it is lost, once.
- **Liveness is the only bound.** A retiring instance that holds its lock
  without a server connection for about 10 minutes, or one the server sees as
  empty for about a minute that does not exit, gets the bounded handover: an
  exit request (SIGUSR1), SIGTERM after 30 seconds, SIGKILL of its process tree
  15 seconds later, sparing verified detached jobs (below). A retiring instance
  that is connected and runs work is never cut, however long it takes. The
  owner's Restart now still interrupts: it asks every instance to stop.
- **Remote hosts.** `aimebu supervisor` started while another instance holds the
  default journal's lock picks a fresh `journals/<name>.sqlite` by itself. To
  upgrade a remote host, install the new build and start `aimebu supervisor`
  (or let its service start it): the server sees the newer matching build,
  retires the old instance, which finishes its work and exits. No manager runs
  there, so a crashed instance's journal is finished by starting `aimebu
  supervisor --journal <path> --drain` by hand.
  Nothing writes a remote instance off by itself: when its journal is lost for
  good (a replaced disk, a reinstall) its turns and commands stay owned by an
  instance that will never come back, and the server keeps refusing delivery to
  those agents. The owner or an admin ends that under Settings → Machines: each
  remote machine lists its instances, and an instance that is neither connected
  nor gone has a trash icon, "Mark instance as gone". Its dialog first says how
  many open turns and running commands the instance owns and which agents they
  belong to, and that the action is irreversible. Confirming loses the open
  turns (each agent gets the usual `supervisor_journal_changed` recovery notice
  and waits for recovery) and ends the commands, and records the decision with
  the person's id (`supervisor_instance_gone`). A connected instance, or one
  that said hello in the last minute, is refused (409; the minute is checked in
  the write-off's own transaction), a second call changes nothing, and the server's own instances are not
  offered: the local manager writes those off. If a written-off instance
  connects again, its hello makes it active (as it does for a local one); the
  work it lost stays lost.
- **A pre-feature supervisor** (one on `journal.sqlite` that does not write its
  journal ID in the lock file) is treated as retiring when the server holds it
  so, and is waited for by the same rules. The manager works out which server
  instance such a holder is without opening its journal: while it is connected,
  its is the one connection whose journal no lock file on the machine claims
  (a current build always writes its ID next to the pid); when it is not
  connected, it is the one retiring record of an older build that is left once
  every lock file's ID is accounted for. When the mapping is ambiguous (two
  holders without an ID, two such connections or records), the manager leaves
  the holder alone and says so once in the log, because cutting it on a guess
  could cut the wrong instance. Once matched, its own connection is the one
  that counts, and the liveness handover asks it to stop with SIGTERM, not the
  SIGUSR1 an older build knows as its restart signal. An old build leaves its
  journal behind when it exits, so what the journal still owes is finished by a
  drain instance of the current build. A retiring instance
  that removed its journal and is started again on that path has a new
  `journal_id` and is a new instance. After each `hello_ack` to an instance it
  holds as retiring, the server sends `retire_at_idle` again, so one that
  reconnects keeps retiring.

The text below this section describes the bounded handover and the adoption of
jobs by a replacement; those now apply to the liveness handover and to a drain
instance.

The server auto-provisions the approved host machine and its private key on
first start. The local supervisor opens `supervisor/journal.sqlite` and
connects through the same `/api/machine` WebSocket used by remote
supervisors. The server starts it once it is listening (the web UI does not
wait for it), and in first-run setup right after the owner is created. Set
`AIMEBU_EMBEDDED_SUPERVISOR=0` to run no local supervisor, for remote-only
setups or tests (the variable keeps its old name). On shutdown, the server
stops dispatch, closes its HTTP listener and its store; the local supervisor
and its turns keep running and reconnect to the next server.
One supervisor owns a journal at a time. A sibling `journal.sqlite.lock`
file (0600) holds an exclusive process lock, its owner's PID and, on a second
line, the journal ID. Opening the same journal from a second supervisor fails
with a message naming that PID. Instances that run next to each other each have
a journal of their own: the legacy `journal.sqlite` and `journals/<name>.sqlite`
(see "Side by side").
Two live processes cannot safely share its turn records because each has a
different in-memory process-liveness map. When testing a second standalone
supervisor on the same host, set `AIMEBU_EMBEDDED_SUPERVISOR=0` on the server
and stop any existing local supervisor first with
`aimebu server stop --with-supervisor`.

The owner pairs a remote machine from the browser's **Add machine** action.
It issues a one-use token valid for ten minutes and shows a command to paste
on the remote host:

```sh
aimebu supervisor --pair 'https://vpn-server.example:9997' 'amp_...'
```

The command's server URL is `AIMEBU_PUBLIC_URL` when set (for a server
behind a reverse proxy; see [HTTPS](tls.md#reverse-proxy)), otherwise the
scheme and host the browser used to reach the server.

The supervisor refuses to pair, or to run, against a plain-`http` server at an address other than
this machine's (loopback): the pairing token, the machine key it returns and every frame
the supervisor then exchanges would cross the network in the clear. Use an `https` address, or accept the
risk with `aimebu supervisor --insecure --pair '<url>' '<token>'` (and `--insecure` on later runs). The
**Add machine** dialog says so, and its command carries `--insecure`, when the server was reached over plain http at such an address.

A remote supervisor that already runs against a plain-`http` server (paired before this check) stops with that message after the
upgrade. Either move the server to HTTPS and re-pair (or edit the saved URL, `supervisor/machine.key`, or set `AIMEBU_URL`), or
start it with `--insecure`. The pairing request and the approval wait re-check the transport on every redirect they are sent along, so a
redirect from https to plain http on another host is refused too. The server's own local supervisor dials loopback, so the check never
stops it.

The remote supervisor redeems the token through the server allowlist and
saves its machine ID, private key, and server URL in
`supervisor/machine.key` under its local v2 config root (mode 0600). The
server stores only a hash of the token and machine key. The machine stays
pending until the owner approves it; the CLI waits and then connects.
Later `aimebu supervisor` runs reuse this credential and URL. On the server
host, standalone mode still reads `server/host-machine.key` when no remote
credential exists. Set `AIMEBU_URL` to override the saved origin if needed.
The supervisor opens `supervisor/journal.sqlite` (or, when another instance of
this machine holds it, a fresh `supervisor/journals/<name>.sqlite`: to upgrade a
remote host, start the new build and the old instance retires, see "Side by
side"), uses one WebSocket per instance, and reconnects after a disconnect. Revocation invalidates the key,
closes its current socket immediately, and rejects subsequent frames.

For remote supervisors over a VPN, bind the server to its VPN IP with
`AIMEBU_BIND` and include the supervisor's VPN source IP or CIDR in
`AIMEBU_ALLOW`. The allowlist checks the direct peer address, including
pairing, approval checks, and the machine socket. Use an HTTPS server URL
when crossing an untrusted network so the pairing token and machine key are
protected in transit. V2 can serve HTTPS directly from caller-supplied
certificate files; see [HTTPS setup](tls.md). A standalone supervisor may set
`AIMEBU_INSECURE_SKIP_VERIFY=1` for a self-signed development server, which
disables certificate verification for pairing and subsequent connections.
Each project needs a folder mapping for the remote
machine before it can receive a spawn; that folder must exist on that host
and use its canonical path rather than a symlink alias.
The host machine uses the project's ordinary folder automatically.
The supervisor receives named launcher definitions from the server and reports
whether each executable exists, its version when available, and its current
availability. A new machine has no harnesses (nothing is seeded): the owner adds
each in Settings → Agents → Harnesses & models, by hand or from what Auto-detect found, with one
executable name or absolute path, a label, argument array, environment variable
references, path mappings, and visible host roots. Arguments execute without a shell, and environment
values are never stored in the server. The last report and owner activity stay
visible while a machine is offline, but changes require a connected supervisor.
The name `docker` is reserved: older API requests use it as an alias for the
harness named `harness-docker`. The supervisor also answers two read-only
questions on its own machine, each a `detect` frame (see the contract): which
harness executables are installed (PATH plus `~/.local/bin`, `/usr/local/bin`,
`/opt/homebrew/bin`, `~/.npm-global/bin`, `~/.bun/bin`, `~/.opencode/bin` and the
global npm prefix, once per real file, with `--version`; the test helpers set `AIMEBU_TEST_RESTRICT_PATH=1`, which limits the search to `PATH` so a test never finds the developer's own CLIs), and which models a Pi,
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
enabled host native presets only when `AIMEBU_ALLOW_NATIVE=1` was set as it
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
the harness the key in the harness's environment (`AIMEBU_AGENT_KEY`), never in its argument list
(`ps` shows that to every local user):

The bridge (`aimebu agent-tools`) reads `AIMEBU_URL`, `AIMEBU_AGENT_ID` and `AIMEBU_AGENT_KEY` from the environment it is started with; the first two
are in its MCP entry, the key is the variable the harness fills from its own environment. The entry also carries `AIMEBU_AGENT_WORKDIR`, the
agent's checkout (Claude's launch folder when it has one), where `attachment_get` saves a file it cannot return inline:

- Claude: the `aimebu` MCP entry is inline (`--mcp-config <json>`), a stdio entry for `aimebu agent-tools`
  whose environment says `AIMEBU_AGENT_KEY=${AIMEBU_AGENT_KEY}`, which Claude expands from its own environment.
- Codex: the private `config.toml` names the bridge with `env_vars = ["AIMEBU_AGENT_KEY"]`; the key is only in Codex's environment.
- Pi: the private `mcp.json` has `Authorization: Bearer ${AIMEBU_AGENT_KEY}`, which Pi expands.
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
say so (an older one, which keeps keys on disk) is sent none. Before the first delivery to an agent a newer instance takes over from a
retiring one, the server sends the key in a standalone `agent_keys` frame, but only to an instance whose hello says
`supports_agent_keys_frame=true`: the previous build advertises `supports_agent_keys` without handling that frame (its read loop would
drop the connection). For that build, the server closes the connection with WebSocket code 1013 (`try again later`) and reason
`agent key requires reconnect`, then grants the key in its next `hello_ack` before delivering the turn.

Claude, Codex and Pi Docker adapters build the HTTP toolbox entry from the same in-memory access; nothing key-bearing is copied into the shared checkout. The key is not in the harness's
argument list: the MCP entry names a variable
(`Authorization: Bearer ${AIMEBU_AGENT_KEY}` for Claude and Pi, `bearer_token_env_var` for
Codex) and the launcher hands the variable over by name through `HARNESS_DOCKER_FORWARD_ENV`, so a Docker
launcher without that feature (`forward_env`) is refused with the way out. The HTTP endpoint uses the
same agent-key authorization and scope as the stdio toolbox's API calls.
Docker's container must be able to reach `host.docker.internal` and the
server's allowlist must admit its source IP. A stale spawn is rejected before
the key is kept; an invalid key sends a failed
`spawn_result` for that agent while the machine connection stays open.

**Only run agents in docker.** An instance setting (Settings → Agents → Agent isolation, owner or admin, off by default;
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
Settings → Agents → Harnesses & models) get their container versions the same way, within a shared 20 second budget;
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
Every Git command the supervisor runs on the host in a checkout an agent can
write to (the worktree operations, the work-tree description of a limit kill,
the Docker adapter's lookup of the repository's shared directory) goes through
one helper that keeps the repository's own config from running a program as
the host user: `core.fsmonitor` off, hooks pointed at the null device,
`log.showSignature` off (so a signed tip never calls `gpg.program`), and for
each filter driver the repository's own config (local or worktree scope,
includes followed) defines or changes, only the commands the system, global
and command-line config give it (none when they give none). These are passed
as command-line config in the environment (`GIT_CONFIG_COUNT`), which wins
over every config file. Optional locks and prompts are off. So repository
hooks no longer run on these host-side calls (a `worktree add` runs no
`post-checkout`), while filters from the global and system config (git-lfs
installed globally, say) still do. A filter driver added between the read of
the config and the command is not covered. A status made this way does not go
into submodules or embedded repositories (`diff.ignoreSubmodules=all`, and
`--ignore-submodules=all` on the command, which a `.gitmodules` `ignore`
setting cannot override): a nested repository has its own config, whose
filter drivers the helper never reads. The accepted tradeoff is that changes
inside a submodule or an embedded repository do not count as dirty files in
the worktree inspection or the description of a limit-killed turn's
checkout. Because of that, the worktree removal checks for them separately,
and its rule is: when it cannot prove the worktree clean, it keeps it as
`preserved`, as a dirty tree is, with the reason. It keeps it when a gitlink
in the index differs from the one in `HEAD` (`ls-files --stage` against
`ls-tree`, neither of which runs a filter); when a gitlink's folder is not
empty, whether a submodule is checked out there or it holds loose files (the
status does not look for untracked files inside a gitlink's folder, so
`git worktree remove` would delete them); when the worktree's git dir has a
`modules/` entry (a submodule was checked out, and git refuses to remove the
worktree even after `git submodule deinit` empties its folder); and when a
`.git` is anywhere below the top level, since a repository made inside a
folder the parent tracks is invisible to the status and its own branches
would be deleted. That walk does not use git or follow symlinks; a path it
cannot read (`could not read <path>`, a path inside the worktree; `could not read
the worktree's submodule data` for the `modules/` check), a tree of more than
200,000 entries (`too many files to check`), or a walk the request's deadline
interrupts (`check interrupted`) also keeps the worktree. The walk reads each
folder in chunks and checks the deadline at every entry, so it ends soon after
it and does not hold the repository's worktree lock longer. `git worktree add` never
populates a submodule and leaves its folder empty, so a clean worktree of a
repository that commits one is still removed. Repository-local filter drivers, such as the ones
`git lfs install --local` writes, do not run on these calls either: a
worktree the host creates gets LFS pointer files instead of their content,
the git-lfs `post-checkout` hook is skipped with the other hooks, and a file
such a driver cleans counts as modified once its timestamp changes. A global
`git lfs install` is unaffected.
The server resends an unconfirmed active spawn after reconnect and
periodically while the machine remains connected, at most 16 per pass; a
retry with the same HTTP idempotency key also resends it. Stopped agents are
excluded. An error `spawn_result` records a terminal failure for that
generation, so pings do not resend it indefinitely. A `control` frame updates
the desired revision. Stop
cancels a running turn and confirms the stop only after its process exit;
the server records such an exit as a cancelled turn and raises no recovery for
it (a deliberate stop is not a failure);
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
Each `hello` reconciles only the turns and jobs owned by that journal. A new
remote instance leaves an older one listed, even when it is disconnected;
its open work stays with it until it reports, finishes, or its journal is
marked gone. On the local machine, the manager marks a missing journal gone
after its instance has not connected for a minute. Marking an instance gone
makes only its open turns uncertain and ends only its open jobs; none is
requeued from another journal's empty inventory. The server emits a visible
`supervisor_journal_changed` event for each affected turn.
A dispatched turn with no recorded owner keeps the earlier rule: a hello with
a journal other than the last accepted one makes it uncertain.
The owner or an admin can also mark a remote machine's disconnected instance
gone by hand (see "Remote hosts" under Side by side); the effect on its turns
and jobs is the one above.

The journal uses SQLite WAL with `synchronous=FULL`. On `deliver`, the
supervisor sends `ack persisted`, commits `turn_start`, sends `ack accepted`,
then starts the adapter process. If the adapter cannot launch it (for
example a missing executable or an unusable Docker checkout), the supervisor
journals a failed exit whose evidence carries that error and reports it like
any other non-terminal exit; the machine connection stays open for the
machine's other agents. A CLI-reported native session handle is
journaled before forwarding it to the server. The server binds it to the
turn's session and rejects a conflicting handle. On reconnect, the supervisor
replays native-session events only for turns still in the hello inventory,
then the usage and compaction
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
A manual context clear waits for a running turn to settle. Its fresh generation
is spawned through the ordinary path, which ends a warm process before it can
accept another turn.
A correction proven never started is abandoned: its parent receipt is held
and settled, and blocked parents raise a delivery-failure inbox item. That
item exposes the held reason from the parent or correction receipt.
An exited turn with an uncertain outcome starts a diagnostic only when its
admitted capability snapshot verifies read-only mode. The diagnostic report
stays private to the recovery item. A lost turn goes straight to recovery routing
because its previous process may still be running. If the harness never
launched, the supervisor reports a definite start failure. Recovery shows the
start error and offers Retry or Abandon without a diagnostic or a warning that
work may have run. A missing tools access error tells the owner to start the
agent again before retrying.
A turn the agent's own deliberate stop ends is not a failure: when a person,
the lead or the dispatcher stops the agent (or closes its room, which stops its
agents) and the stop kills the running turn (the supervisor reports the exit as
the stop's cancel, such as exit code 143), the server records the turn as
cancelled (`outcome` `cancelled`, settled), aborts its delivered notes, and
raises no recovery item, no Inbox card and no lead note — the exit event carries
`cancel_reason: agent stopped`. A turn that carries a turn interrupt request
keeps today's path whatever the stop did (a request whose turn then ends another
way changes nothing). A genuine crash, OOM or unrequested kill (no
cancel intent, or a cancel while the agent is still desired active) takes the
ordinary recovery path below exactly as before.
Owner/operator and lead decisions
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
agent receives a recovery item without starting a diagnostic, and
Resume/Retry are unavailable until it is active again. A continuation from a
lost turn waits for positive process quiescence: a supervisor hello reporting
that process gone, or its recorded exit/kill. Journal absence after replacement
does not count. A hello records that evidence for every unsettled lost turn the
journal vouches for (a turn lost in the same hello, with an intact journal that
shows the process gone, counts too), whether or not a continuation exists yet:
the decision usually comes after that hello, and a restart used to leave it
waiting for a second hello that told it nothing new. When nothing proved the
process gone, the waiting item says why (the journal was replaced, or no report
has covered the turn since it was lost). The owner sees a waiting item and may
explicitly override the overlap risk; the actor and risk are recorded. The lead
never decides it: the overlap risk is a person's call. The supervisor does not
confirm the old process by pid: a supervisor's journal says on the next hello
whether its turns ended; a replaced journal has no
record at all, which is why that case needs a decision. An intact journal that proves a
continuation never started replaces it with another linked continuation and
preserves the recovery evidence while the agent remains active. If the agent
was stopped before hello, its notes are aborted and the stop is confirmed.
Stopping an unsent diagnostic or correction runs its parent cleanup; stopping
an unsent continuation aborts its notes. A stop of an agent whose turn the
stop's cancel then ends raises no recovery item at all (see above); only a
crash or an unrequested kill after the stop still raises one, which the
periodic settling then resolves as `agent stopped` if the agent stays stopped.
Live process goroutines send through the current machine socket after reconnect.
The supervisor replays each unacknowledged process exit and unreceipted answer
after `hello`, so a disconnect cannot strand those frames or rerun the tool.
Attachments remain under the server's private storage; the supervisor does
not fetch or stage them ahead of a turn. A normal agent turn can use the
`attachment_get` MCP tool, which checks the delivered note and current room
membership. For native Claude Code and Codex the tool runs in the stdio
bridge next to the agent's files, so a file it cannot return inline (a zip, a
PDF, text over 64 KB; at most 10 MB) is written, only when the agent asks for
it, to `<checkout>/.aimebu/attachments/<attachment_id>/<name>` with mode 0600 (as readable as the checkout when the supervisor saves it, below)
(see [the adapter guide](adapter.md)). The other channels run the tool in the
server, which asks this supervisor to save it with `attachment_save` (below). Pi Docker has no MCP credential channel, so its private turn input
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

The server runs a dispatcher that is woken by work: every committed change that
can create dispatch work (a message that queues a note, a turn ending, a
control or desired change, a recovery decision, a queue change) sends a
non-blocking wake, and the next pass starts at once, so a new message reaches an
idle agent in well under a second. The wake coalesces (several changes share
one signal), the pass's own writes do not wake it again, and a fallback tick of
two seconds catches everything no write covers — a schedule falling due, a
sweep's effect, a lost signal. An idle server therefore passes about once per
fallback tick, not ten times a second, and skips the team-flow step entirely
while no change is draining or applying (a cheap cached check, seeded at start,
set when a change begins or is cancelled or fails, and cleared by the pass that
finds nothing left). It batches at most 20 queued notes
from one room per agent, rotating rooms between passes. Paused rooms/projects,
stopped agents, recovery blocks, provider quota, active or unsettled turns,
and the per-machine/tool `dispatch_switch_gate` pause prevent ordinary dispatch
without discarding notes. A pending correction or verified read-only diagnostic
turn can bypass the ordinary recovery block while still respecting gates.
Stopping an agent is the exception: the stop itself drops its queued notes as
"undelivered: agent stopped" (one `notes_dropped` event per room; the messages
stay in their rooms' history), so a stopped agent never starts another turn for
them. Notes an admitted turn whose delivery may still be in flight carries keep
their state, an agent a switch was moving (draining, starting or failed: the
switch, its retry or the reactivate that follows intends to run the agent again)
keeps all of its notes, and so does an agent a team-flow change is draining to
start again.

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
  agent's key is refused with **403 `keep_alive_scope`** before any handler runs (`requireAgentTool`), so a ping that
  calls a tool anyway posts nothing and changes nothing. The one exception is the MCP endpoint itself
  (`POST /api/agent/mcp`, `requireAgentToolMCP`): it still answers `initialize` and `tools/list`, because Codex's MCP
  client treats a 403 there as fatal (exit 1, the ping fails and its cache lapses) and the tool list is part of the
  cached prompt prefix, which a ping must keep identical to the real turn's. A `tools/call` made through it reaches the
  scoped REST routes and comes back as a tool error (`HTTP 403: ... keep_alive_scope`). A ping whose error ends on the server's refusal body
  (`{"error":"keep_alive_scope","message":...}`, pinned by a server test; a failure that only quotes an earlier one is not) is logged and counted as a failure but not reported to Sentry. The supervisor lifts the scope
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
  instruction in the turn JSON after the unchanged instruction text; its cost is the price list's, per ping (a cold ping
  reports the thread's running token totals, so the server records the ping's own delta from the session's previous
  totals),
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
  supervisor's `warm_state` report carries (`unconfirmed_ping_ends`; the machine's card in Settings → Agents → Machines
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
  session's last total it holds, which survives a restart, so the usage figures and the card are not affected.
  A ping whose session holds no earlier total at all records no cost and no totals-shaped tokens: a ping is
  never the first spend of a session, so such figures are the session's lifetime, and only the reported totals
  are kept as the baseline the next real turn is priced from.)
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

*Instance setting.* The window is set by the owner in Settings → Agents → Agent isolation ("Keep agents warm
(minutes)", 5 until the owner saves another, 0 is off, at most 1440; `GET`/`PUT /api/agent-isolation`,
`warm_idle_minutes` with `warm_idle_saved`, owner only to change). The order is **the owner's saved value, then
`AIMEBU_WARM_IDLE` on the supervisor, then 5 minutes**: the server puts a window on a delivery and on an
`agent_settings` frame (`warm_idle_seconds`) only when the owner has saved one (a saved 0 is sent and means
off); with none saved the field is absent and the supervisor works to its own (`AIMEBU_WARM_IDLE`, a Go
duration, 0 is off, unset or unreadable is 5 minutes). A supervisor applies a window before the turn, and
turning it off ends every idle process at once. Codex app-server warm mode
also needs the owner's `codex_warm_enabled` opt-in (off by default); turning that
off retires idle Codex processes while cold Codex turns and keep-alive pings
continue. It says the window it works to in its `warm_state` report
(`idle_seconds`), and a card's warm or sleeping follows that and nothing else: a live idle process is "warm"; no
process is "sleeping" only when the supervisor says it works to a window above 0; before its first report the card stays cold, while a report
without `idle_seconds` is unknown (the card's `warm.state` is `unknown`) and the card says neither warm nor sleeping. So a supervisor started with
`AIMEBU_WARM_IDLE=0` shows no "Sleeping".

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
agent whose live warm process is working keeps the pulsing ring and says “Online · warm · working”; a working agent without a reported warm process is green. During `interrupting`, `lost`, and other observed states besides `idle` or `in_turn`, the card does not present a warm state. An idle agent on a harness without a warm process (every harness except Claude and opted-in Codex) shows a neutral dot and “Warm n/a”, with a tooltip explaining that the harness does not keep its process warm. The members panel's people have the same 8px dot. It reads the card's `warm` field, refreshed when a
process starts or ends, with no polling of its own (the minutes follow the panel's clock). The server emits `agent_warm_working` when a supervisor first reports the process busy, prompting the card to refresh during the turn.

A supervisor that keeps processes warm sends the server a `warm_state` frame (at
connect, and whenever the picture changes: a process goes idle, is taken for a turn, ends). A turn's settled `warm_state` is sent before its `process` frame, so the card refreshed by turn completion immediately sees the idle process. The report lists the agents with a
live process (idle until when, or working), `refusals` with each agent's last classified reason a turn ran cold,
the counts started and reused since it started, the memory of
the warm processes it runs itself, and what every running container uses. An idle Claude or opted-in Codex agent's card says
"Warm · sleeps in N min" or "Sleeping" (`warm` on the card: `state` warm or sleeping, `sleeps_at`; absent when
the window is off or does not apply to the agent), and the machine's card in Settings → Agents → Machines says how many
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

*Account switch.* A warm process holds the login it started with. **A switch of account (by hand, or the
automatic one) and the removal of the active profile never wait for agents or warm processes.** After the swap the
server retires the host's warm processes of the tool in the background:

1. It sends the host's supervisor a `retire_warm` request for the tool (the login is on the host, so no other
   machine is asked), holding the tool's cache pings back first (see below).
2. The supervisor ends every idle process of the tool and answers `ok`, or `error` when one is not confirmed gone.
   `error`, no answer within 30 s, or a supervisor that goes away is only logged: nothing is refused, and nothing
   waits for it. The supervisor's own quarantine of an unconfirmed process still keeps a new process off that
   session; the agent's next turn starts a fresh process on the new login.

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
server restart leaves a supervisor's idle processes running. The background retire cannot reach it until the
supervisor is back and can answer (it is logged, and the switch is not held), unless the server knows the host has none: it never connected, its supervisor
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
  or cold, is started on the session, so there are never two processes on one session. A delivery
  that finds the agent's last process not confirmed gone waits for it, in a goroutine of its own
  (the loop that reads the server's frames goes on), for up to 150 s: long enough for the two-minute
  quarantine of a process whose container end is unconfirmed (below), so the automatic continuation
  of a turn-limit kill starts once the quarantine is over instead of failing in under a second and
  sending the agent to recovery. It looks again every second, nudging the stuck watcher, and goes on
  as soon as the process is confirmed gone, which for a process whose container is gone is at once
  (see Containers below). A stop of
  the agent (or the supervisor stopping) during the wait wins: no process is started and the turn
  is reported as the stop's cancel (exit 130, `cancel_intent`). A process still provably alive
  after the 150 s is not released, and the turn fails to start as before.
- **Never fails a turn.** A process that cannot be started, noted in the journal or handed the
  turn is reported and the turn runs cold.
- **Limits.** `AIMEBU_WARM_MAX_LIFETIME` (default 1h): a process that old is not reused and is
  not kept idle (nothing refreshes credentials inside one). The memory guard
  (`AIMEBU_WARM_MIN_FREE_MB`, default 1536, and `AIMEBU_WARM_MIN_FREE_PERCENT`, default 10)
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
  is *unconfirmed*. The supervisor retries the end periodically, backing off as the retries fail, twice as long
  each time up to about five minutes; a stop or a delivery that finds the slot stuck wakes the retry at once (at most one such wake per stuck process every 30 seconds), so a process that has recovered is confirmed without waiting out the backoff; only the first failure reports the uncertainty (the release says so once more), so a slot
  nothing can settle neither spins nor fills the log. While the
  host wrapper is alive, no replacement starts on the session and stop ack and key drop wait. If the
  wrapper has been reaped but its container end remains unconfirmed, the session stays quarantined
  for two minutes while kill is retried; a timer at the quarantine's end wakes the watcher, so it is released at that
  deadline and not at the next backoff step. The supervisor then
  reports the uncertainty once and releases the session; the harness's session heartbeat cleans up a
  process left in the container. The release report is a warning, and there is none at all when the
  end that stuck the slot was a planned one (the agent was stopped, or the supervisor is stopping):
  that quarantine is the stop's own path.
  A launcher
  without the capability runs its agents cold on every turn, as before. A process in a container is
  always closed first (input, then SIGTERM with about eight seconds for the wrapper's own cleanup, only
  then SIGKILL). A running container gets the faster in-container wrapper only after
  `harness-docker-ctrl rebuild`; until then the terminate takes about two seconds.
- **A process is gone once its container is.** An end the launcher did not confirm is still confirmed, at
  once and with no quarantine and no report, when the container the process ran in is gone: whatever was
  in it died with it. The supervisor knows that container by its **ID, never by its name** (a container
  that is made again keeps its name and gets a new ID). A launcher with `turn_activity` is started with
  `HARNESS_DOCKER_REPORT_EXEC_ID=1` for a warm process too and prints
  `HARNESS_DOCKER_EXEC_ID=<container id>:<session>` on stderr before its `docker exec`; the adapter keeps
  the ID (12 to 64 hexadecimal digits, anything else is not an identity) and takes the line out of the
  agent's output log. The request is not part of the warm fingerprint. When the host wrapper has been
  reaped and its end is unconfirmed, the supervisor asks Docker about that ID
  (`docker ps --all --no-trunc --filter id=<id>`, 5-second limit), when the end is first tried, at every
  retry, and every 5 seconds while the slot is stuck:
  - Docker lists no container with that ID (it was removed, or made again under a new ID), or lists it as
    `removing`, `exited` or `dead`: the process is confirmed gone and the slot is released at once.
  - It is `running` (or paused, or restarting): nothing is known, and the quarantine above holds as before.
    A container restarted under the same ID is in this case.
  - Docker cannot be asked (the command fails or does not answer): nothing is known, the failure goes to
    the local log once, and the quarantine holds.

  An absence counts only for a container Docker has shown the supervisor before: it takes a first look
  when the launcher names the container (up to three tries), so a launcher that talks to another Docker
  than the supervisor's own `docker` does not make every container look gone. A launcher that was asked
  for its container and ended without naming one never reached its `docker exec`, so nothing of it ran in
  a container: that end is confirmed too, without asking Docker. A launcher without `turn_activity`, a
  line that names no ID, and a wrapper that is still alive all leave the quarantine as the only answer.
  Keep-alive pings and interrupted one-process turns in a container keep their own 150-second hold.
- **A container that is being replaced.** A Docker launcher that finds its harness container on the way
  out says `Harness container is removing` (or `does not exist`, when it vanished between the launcher's
  two looks at it), starts nothing and exits. That is a rebuild of the container, seconds before the new
  one is current, so a turn whose process ended that way, with a failure status and without having said
  anything else, is not reported: the supervisor starts it again after 2 seconds, and again, for up to
  60 seconds from the first such failure. The start that finds a container runs the turn as usual; when
  the minute is over the last failure is reported as it always was. A stop of the agent during the wait
  wins (the turn ends as the stop's cancel, exit 130), and a turn the server asked to be interrupted is
  not started again. Any other launcher failure (`Docker is not running`, `Harness is not started`, a
  folder that is not mounted) fails at once, as before.
- An idle process is ended gracefully first (its input is closed and it gets a few seconds to go;
  with a Docker launcher that lets the wrapper clean its session up properly, which is also the
  roughly one second the wrapper's cleanup costs a one-process turn), and killed if it does not;
  a process that is being replaced for a delivery, or whose turn failed, is killed at once. The
  harness-docker wrapper (`docker exec -i` into the long-lived harness container) is what a
  Docker launcher runs, so a warm Docker agent is one `docker exec` kept open.
- Output the process writes to stderr goes to the log of the turn that is running.

A running turn is bounded by two deadlines, ported from v1's pi
watchdog. `AIMEBU_TURN_IDLE` fires when the harness writes no output at all
for that long (any stdout line counts, including streamed partial output,
not only the events the adapter forwards); it is suspended while a tool execution is in flight, since a long
command is legitimately silent. The supervisor counts a tool as in flight
from pi's `tool_execution_start`/`_end`, Codex's `item.started`/
`item.completed` for commands, MCP calls, file changes and searches, and
Claude Code's `tool_use` / `tool_result` blocks. OpenCode and Vibe report a
tool call only once it has finished (OpenCode's `--format json` emits a `tool_use`
part when its state is completed or error, and Vibe's streaming output writes only
completed entries), so neither has an event for a tool's start. For these two the
supervisor looks at the process table instead, when the idle deadline falls due and
again every 30 seconds while it holds: a live process under the harness that began
after the harness last wrote anything (less two seconds, the resolution of a
process's age) is a tool call in flight, and suspends the idle deadline like an
announced tool. A helper the harness started earlier, such as a person's own MCP
server, began before the last output and does not count. When the process ends the
harness gets one fresh idle period. The process table is read with `ps`, on Linux
and macOS. A **Docker turn** runs its tools in a container that is shared by every
agent, out of reach of the harness's process tree on the host, so there the
launcher is asked instead, when it has harness-docker's `turn_activity` feature
(its `HARNESS_DOCKER_PRINT_FEATURES=1` answer, read once per turn with the
`forward_env` check): the turn's launcher is started with
`HARNESS_DOCKER_REPORT_EXEC_ID=1`, prints `HARNESS_DOCKER_EXEC_ID=<container>:<session>`
on stderr before the harness starts (the adapter takes that line out of the
agent's output log), and at the same moments as above (when the idle deadline
falls due, then every 30 seconds, or every idle limit if that is shorter, while
the answer is busy) the supervisor runs
`HARNESS_DOCKER_PROBE=<id> HARNESS_DOCKER_SINCE=<last output, unix seconds> <launcher>`
(5-second limit, under the turn's context so a cancelled or ended turn
interrupts it, no forwarded variable, no agent key). `busy`, and `starting`
(the session's `docker exec` is still pending), suspend the idle deadline;
`idle`, and any failure (no exec ID yet, an unknown or ended session, a timeout),
do not, so the deadline keeps its old meaning rather than a guess. A Docker
launcher without `turn_activity` is not asked, and there a long silent command
still waits for `AIMEBU_TURN_IDLE` (`AIMEBU_TURN_MAX` bounds a hung tool in
every case). Claude, Codex and Pi announce a tool's start, so they need neither.
`AIMEBU_TURN_MAX` is a
wall-clock cap for the whole turn, tools included. The defaults are the same
for every harness: 30 minutes idle and a 90-minute cap. Setting either
variable overrides it for every harness; both are Go
durations, `0` disables a deadline, and an invalid value fails startup. The
deadlines apply to the local and remote supervisors alike and stop
counting once the final answer has been received. A tool that never reports
its end keeps the idle deadline suspended, so only `AIMEBU_TURN_MAX` bounds
a hung tool. On expiry the supervisor logs `stalled` for an idle timeout or
`turn exceeded the <duration> turn limit` for a wall-clock cap. Both are
telemetry warnings with separate fingerprints. It kills the harness process
group, and reports the
exit as a failure (not an owner cancel) whose evidence names the deadline.
The exit also says what the killed turn left in its checkout:
`git log -1 --oneline` and the count of dirty files (`git status --porcelain`,
untracked files included, run with optional locks off, with the programs the
repository's config can name turned off (see `worktree_op`) and a
5-second limit),
as `tip ab31772 <subject>, tree clean` or `…, 3 dirty files`, in the
evidence (`work tree at the kill: …`) and in the exit's `work_tree`, which
the journal keeps for a replay. It is left out when the folder is not a git
checkout or git does not answer.
The server then continues the turn once on its own: the turn's notes are
re-queued on a continuation turn with a note that says the previous turn was
stopped by the turn limit (with the work tree at the kill, when there is one:
`your previous turn was stopped by the turn limit (work tree at the kill: tip
ab31772 …, tree clean); check state and continue; keep commands short`), and
the room gets a system event saying so (its payload carries `work_tree`), so a
long but legitimate turn does not wait for a person. Only when that
continuation turn is limit-killed too does the agent fall back to the
ordinary failure path: the turn's notes become `unknown`, the agent is
recovery-blocked, and a recovery item carries the evidence. A worker's item
goes to the active project lead first, with a `worker_recovery` wake note and a
room notice. The lead can choose Resume, Retry, or Abandon with
`list_recoveries` and `resolve_recovery`. After 30 minutes by default, or when
the lead stops or becomes recovery-blocked, the dispatcher raises the item to
the people's Inbox and sends its ordinary notification. Project Settings can
turn lead routing off or change that interval. The lead's own recovery goes
straight to people, and so does a worker's when the lead is not in the
worker's room and the project has no home room to wake it in. A turn that is **interrupted** is continued the same way. When the harness process ends
without a final answer, by a signal, a non-zero status or silently, and says nothing about why
(no error result, no stderr message with an ordinary non-zero status), it was cut off, not
answered badly: the harness was killed, the session restarted, or the container stopped. The
supervisor marks the exit `interrupted` (with the reason, such as `the harness process was
stopped by signal 9 (killed) (exit code 137)`, as its evidence), and an empty answer from such a
process is not sent, since the server would hold it as `invalid_answer: EOF`. The server then
continues the turn once with the note `your previous turn was interrupted (<reason>); check
state and continue` and a `turn_interrupted_continuation` system event that carries
`reason: turn_interrupted`, the detail and the exit code. A chain of turns has one automatic
continuation in all, of either kind; a repeat takes the ordinary failure path below.

**Handoff outcome.** A supervisor announces `supports_handoff_outcome` in `hello` because its
adapters ask every agent for the `outcome` of a task-result handoff (`done`, `partial` or
`failed`) and the Claude and Codex response schemas carry the field. The server records the flag
on each turn it dispatches to that supervisor (`turn.handoff_outcome_required`) and holds an
answer to such a turn whose handoff has no outcome as invalid, telling the agent the allowed
values. The turn decides, not the connection that delivers the answer: an answer an older
supervisor journaled and its replacement replays after a swap is committed without an outcome. An
older supervisor, still running until the swap replaces it or on a remote machine not yet
updated, gives its agents a schema without the field: their handoffs are committed without an
outcome and shown without one, and `aimebu doctor` lists the missing flag.

The server can also ask a supervisor to **save an attachment** into an agent's workspace. A
supervisor that announces `supports_attachment_save` in `hello` takes an `attachment_save`
frame (agent, turn, attachment) when `attachment_get` runs in the server, over the HTTP MCP
endpoint that Docker variants and native Pi, OpenCode and Vibe use: the server cannot reach
the agent's files, the supervisor can. It saves only for that agent's active ordinary turn
(`turn_not_running` otherwise), downloads the file from `/api/agent/attachments/{id}` with the
agent's own key (the same checks as the tool: a note in the live turn, room membership), writes
it with the bridge's helper to `<checkout>/.aimebu/attachments/<id>/<name>` (no linked folder,
atomic, at most 10 MB) and answers with the path the agent sees: the host path mapped through
the agent's launcher path map, the rule the Docker adapter uses for the checkout. Unlike the
bridge's 0600, the folders under `.aimebu/attachments` and the file take the checkout's owner and group (when the supervisor may give
them away) and the checkout's read access for its group and others, never more, so a container user other than the
supervisor's can read a file in a checkout it can read; `.aimebu/` itself gains only the checkout's search bits. The download runs off the read loop, and it is the only one: the server's tool
reads the type and size with a HEAD request and fetches the bytes itself only for a file that may
come back inline (an image, or text up to 64 KB). A supervisor from before the frame never gets it; the
tool then says the file cannot be saved, as it does when the machine is offline.

The server can also **interrupt one turn on purpose** without stopping its agent. A supervisor
that announces `supports_turn_interrupt` in `hello` takes a `turn_interrupt` frame for one
running ordinary turn and answers `ok`, `not_running` (it runs no such turn) or
`unsupported_mode` (a correction or a diagnostic, which is left alone; the
server itself never asks for a handoff turn to be cut). It ends the turn's
process the way a stop does (a cancel, a kill after 30 seconds) and reports the exit
`interrupted` with the reason `the turn was interrupted on request`, never as a cancel; the
agent stays active and its next delivery is admitted as usual. An answer that was in first
wins, and so does a stop of the agent that follows. A process in a container whose launcher
confirms the end of its session is told to terminate and given 8 seconds; when the end in the
container is not confirmed, the supervisor holds the agent's slot (it reports the exit, and
admits another turn, only after the same 150-second quarantine an unconfirmed keep-alive ping
gets, or at once when the agent is stopped or the supervisor shuts down), so that a new turn
never starts beside a process that may still be running. What becomes of the interrupted
turn is recorded by the server before it sends the frame, in a `turn_interrupt_requested`
event: `continue` continues it like any interrupted turn but outside the one automatic
continuation of its chain, and `requeue` puts its notes back in the queue (with the
"previous attempt was interrupted" caution when the turn had started working) and makes no
turn for them. A team flow change's **Interrupt now** uses `requeue`; an older supervisor is
stopped and started again instead. An
owner cancel, a start failure, an explained failure, a provider limit and a lost thread are
not interruptions and keep their own handling, and an answer that arrives but does not parse
stays an invalid answer. A failed journal write for a turn's event or final answer is handled the
same way (without the automatic continuation): the supervisor stops
forwarding that turn's events, kills the
harness process group, waits for it to exit, and reports a non-terminal
failed exit whose evidence names the journal error. A harness therefore
never keeps running after the supervisor stopped tracking it.

Every supervisor, local or remote, routes turns by tool: the fake
adapter provides a short echo for local demos, while installed Claude Code,
Codex, Pi, OpenCode, and Mistral Vibe CLIs use their native adapters. An unavailable CLI does not
appear in the supervisor's catalog.

## Commands outside a turn

An agent can start a long command (a test gate, 15–30 minutes) that runs
outside its turn, so it neither holds the turn nor dies with it: `run_command`
in the agent toolbox, which records a `command_job` and sends `command_run` to
the machine (see the contract for the frames). The request goes to the supervisor
instance the job names, chosen when the job is created: an instance that
replaces it a moment later never receives the command, and the job never runs on
one instance while another owns it. The supervisor runs a native
one in a session of its own (a container one in its own process group), in
the agent's own checkout (`cwd` may not leave it: it is
cleaned first, so `..`, `./..` or `a/../..` is refused on the server and again
on the machine), with
the same rights that agent's own Bash has there and nothing more: none of the
agent's keys is in its environment. It is not a turn: no turn row, no usage,
and the turn limits do not apply to it.

- **Where it runs.** On the machine, for an agent on a native launcher. A
  containerized launcher (or an agent on the `docker` variant without a
  launcher record, through that tool's Docker harness) runs commands in the
  agent's container through harness-docker's command mode: the feature probe
  (`HARNESS_DOCKER_PRINT_FEATURES=1 <launcher> --version`) must print
  `run_command`; its answer is cached per launcher file (path and arguments,
  device, inode, size and modification time) for at most 5 minutes, and a
  failed probe falls back to the features the launcher row reported. A
  launcher without it is refused with the update to make (409
  `command_refused`), and a command never falls back to the host (the
  container is shared and its paths differ); neither does one whose launcher
  record is not on the machine or whose Docker harness is unavailable. The
  hello reports `supports_container_commands`, and the server sends no
  container command to a supervisor that does not (an older one would run a
  `docker`-variant agent's command on the host). The
  launcher is run from the checkout with `HARNESS_DOCKER_RUN_COMMAND` (the
  same wrapper line as natively, quoted into one `sh -c` string, so the
  command writes its exit marker), `HARNESS_DOCKER_CWD` (relative to the
  checkout, empty for the checkout itself), `HARNESS_DOCKER_EXIT_MARKER` (the
  container's spelling of `<log>.exit`) and `HARNESS_DOCKER_TIMEOUT_SECONDS`;
  its environment is the launcher's base one and the owner's references,
  never a forwarded variable, the agent key, or a mode switch from a
  reference. Its output streams into the job log as for a native command; the
  launcher's exit is the command's end, and a host-side process it left behind
  holding that output (a `docker exec` client that outlived it) is cut off from
  the log after 5 seconds rather than holding the end. A Docker start is
  answered aside from the frame loop, since the probe may take seconds. **Ends:** a cancel, stop or timeout sends the launcher alone a TERM
  (the whole group gets SIGKILL after 8 seconds); the kill is confirmed only
  by exit 143 or the command's exit marker, and anything else (125, the
  SIGKILL, a launcher killed before it could trap the signal) is
  `cancelled_unconfirmed`, which outranks `timed_out`. An adopted Docker
  command (the journal records `container`) has no exit status to read, so
  its kill is unconfirmed without the marker. Exit 126 with no marker is the
  launcher refusing the command because its container runs an image from
  before command runs: the job ends `failed` with "rebuild the harness image"
  (`bin/harness-docker-ctrl rebuild`), not with an exit code of the command.
  A start whose journal record cannot be written is ended the same way (the
  launcher's TERM first, the group's SIGKILL only after the grace) before it
  is reported failed.
  The contract is `docs/design/harness-docker-run-command.md`.
- **Output.** The process's stdout and stderr are captured into the job's log
  at `<checkout>/.aimebu/commands/<job>.log` — beside the checkout's runtime
  state, so the path the wake note quotes is one the agent can open from its
  own Bash, native or in a container. Each line is redacted (agent keys,
  bearer and basic credentials, cookies, anything named like a key, token,
  secret or password) before it is written; the log is capped at 10 MiB with a
  truncation marker, and the note carries only its last 8 KiB. A native job's
  output goes through a log writer of its own (`aimebu command-log <log>`, an
  internal subcommand that is not part of the CLI: the supervisor's own
  executable by its absolute path, resolved at the job's start, in the job's
  session; it outlives a SIGTERM to finish the
  log and ends with the output), and the exit marker is put in place only once
  that writer is done, so the job needs nothing of the supervisor that started
  it. A container job's output passes through the supervisor, as before.
- **The wake note.** When the process ends (exit, timeout, cancel, a kill on
  stop), the supervisor journals the end and reports it; the server writes a
  system message in the room the command was started from and wakes the agent
  with it: what ran, the exit code and duration, the tail of the output and
  the log path. A stopped agent or a closed room gets no note (the event says
  so), and the result stays in `list_commands`.
- **Journal and restart.** `command_start` is written once the process
  exists, `command_exit` when it ends, replayed until the server acknowledges
  it. The wrapper writes its exit code to `<log>.exit` before it exits, so a
  supervisor that restarts (and cannot wait for a process it did not start)
  still learns the real exit code; a job whose process is gone and left no
  marker is reported `lost`, never guessed at. With every connection the
  supervisor adopts the journal's open commands it does not already hold (its
  own running commands are left alone, so nothing is adopted or reported
  twice). An adopted command is held like one it started, so a cancel, a stop
  of its agent and its timeout (counted from its journaled start) reach it;
  it is not this process's child, so it is watched for its marker rather than
  waited for. Its pid is checked against the start time journaled with it
  (`pid_start`): a pid that now belongs to another process is never
  signalled, and the job is reported `lost`.
- **Who may report an end.** A command outlives a context clear or a harness
  switch, which move the agent's generation on. Its `command_started` and
  `command_ended` frames are therefore authorized by the job they report on
  (this agent's job, run on this machine), not by the agent's current
  generation. An end for a job the server no longer has is acknowledged, so
  the journal stops replaying it.
- **An unconfirmed start.** The server records the machine's
  `command_started` as it arrives, whether or not the `run_command` call still
  waits for it. When no answer comes (a timeout, a disconnect), that does not
  prove the machine refused it: the job stays `queued`, the call answers 202
  with `start_unconfirmed`, and a late `command_started` or `command_ended`
  still settles it. A fast command's end may arrive before its start; the call
  then returns the finished job. Cancelling a job whose start is unconfirmed
  records it `cancelled` at once and also sends the machine a cancel; it is
  killed if the machine turns out to run it, and if it had already finished,
  its reported end replaces the cancel.
- **Inferred ends give way to evidence.** When the server ends a job on its
  own inference (cancelled before its start was confirmed, or `failed` as
  never started), the machine's own evidence still corrects it: a late
  `command_started` makes a job inferred never started `running`, and a
  reported `command_ended` replaces either inference with the real outcome
  (and wakes the agent). A real end is never replaced.
- **Reconciliation on hello.** A supervisor's hello lists the commands it
  still runs (`command_open`) and the ends it still owes (`command_exits`).
  The server makes a `queued` job the machine runs `running`, and ends the
  jobs the machine knows nothing of, since they can never be reported: one
  whose start was never confirmed becomes `failed` (it never started), a
  running one `lost`. Either wakes the agent, so a job never stays `running`
  forever or holds one of the agent's two slots. A `queued` job younger than
  60 seconds (the run request's 30-second timeout and a margin) is left alone:
  its request may still be in flight on the connection the hello replaces.
- **Unconfirmed-job sweep.** Every 10 seconds the dispatcher fails, and wakes
  the agent for, a job still `queued` after those 60 seconds whose run request
  went out before the machine's current connection was made (it can never be
  answered now), and any job still `queued` after 10 minutes. Both are
  inferred ends, so the machine's evidence still corrects them.
- **Cancellation and limits.** A cancel is SIGTERM to the process group and
  SIGKILL after 8 seconds; a stop of the agent kills its commands the same
  way, aside from the frame loop (the grace never holds up other frames). A Docker cancel is confirmed only as far as the wrapper confirms it, so
  an unconfirmed end is reported `cancelled_unconfirmed`, never as done. Each
  job has a timeout (default 1 hour, cap 6); at most two run per agent. The
  job's log and its `.exit` marker are swept with the checkout's runtime
  state.

## Agent output

Each agent's harness writes to stderr while a turn runs (warnings, launcher
errors, what a tool printed). The supervisor keeps that in a per-agent log on
the machine that ran it: `supervisor/agent-logs/<agent-dir>/stderr.log`, with
rotated backups `stderr.log.1` to `.3`. That file is the harness's stderr only.
Stdout is the harness's event stream; what the adapters parse from it is kept
beside it as the agent's conversation log, `conversation.log` (see
[Conversation log](#conversation-log) below).

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
  at the start of the next. A PEM private key is dropped across its lines: its
  `BEGIN` line becomes `[redacted private key]` and the lines up to and with
  its `END` line are left out (at most 400 of them when no `END` comes). Whatever else a tool printed is not recognisable,
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
  project's owner or an admin opens the Output section of an agent's profile or
  its log popup (Show agent's logs, which opens on the conversation log and has
  stderr as its second stream; with Follow on, the popup reads again on the
  agent's events at most every 4 s, and shares its reads between tabs), so
  a remote machine that is offline, or a supervisor without
  `supports_agent_logs`, simply has nothing to show (the web says which). An
  agent that moved to another machine keeps its earlier output on the first.
  The read names the stream (`stream=stderr|conversation` on the route,
  `log_stream` in the frame); stderr is the default.
- **Size.** The server asks each remote machine only for the total size of its
  logs (`agent_log_usage`, advertised as `supports_agent_log_size`) to show in
  Health under "Remote agent logs"; the host's logs are counted with the rest of
  `supervisor/`.
- **Prune.** `aimebu prune` leaves the logs; `aimebu prune -a` removes them with
  the rest of `supervisor/` (see [pruning](prune.md)).

### Conversation log

The conversation log is what an agent did, turn by turn: what it was asked, what
the model said, each tool it called with the input, and what each tool returned.
It is `supervisor/agent-logs/<agent-dir>/conversation.log`, in the same
directory as `stderr.log`, with rotated backups `conversation.log.1` to `.3`.

- **Source.** The supervisor does not read any harness's own transcript files.
  Each adapter already parses its harness's event stream for the answer and the
  usage, and writes the same events to a process-only sink
  (`adapter.TurnSpec.ConversationSink`, never journaled) as it reads them:
  Claude Code, Codex (`exec` and the warm app-server), Pi, OpenCode and Vibe,
  native and Docker alike. The supervisor frames the turn around that: it
  writes the turn's start and the notes the agent was given before the harness
  starts, and how the turn ended after its exit is known. The rest of the
  turn's input (the context: role, roster, room history) is the same from turn
  to turn and is not repeated in the log. A keep-alive ping is not work and
  writes nothing. A log that cannot be opened is reported and the turn runs
  without one.
- **Format.** One record per event: a line `<UTC time> <kind> <text>`, and the
  rest of the record's text on following lines that each start with a tab, so a
  line of a tool's output can never pass for a record of its own. Control
  characters other than a tab are removed. The kinds are `turn_start` (turn id,
  generation, mode, harness and model), `prompt` (the turn's notes, or input
  the harness gave the model on its own, such as the end of a background task),
  `assistant` (text the model wrote, every time it wrote it), `thinking` (reasoning the harness reported
  as text), `tool_call` (the tool's name and its input), `tool_result` (the
  tool's name, `(failed)` when it failed, and its output), `error` (a failure
  the harness or the supervisor reported), `event` (see below) and `turn_end`
  (`answered` or `no answer`, why when the supervisor ended it, and the exit
  status, or `process kept warm`). OpenCode and Vibe report a tool only once it has finished, so
  their call and result records are written together.
- **Unknown events.** A harness can change its stream. An event of a type the
  adapter does not know, a line that is not an event, and an event of a known
  type whose content is not what the adapter reads (`<type> (unreadable)`) each
  leave an `event` record that names the type and carries none of the content,
  so a change shows up in the log as a trace instead of a silent gap, and never
  stops the turn. The same note many times in a row is written once. Events
  that only frame or stream what a later event carries whole (message deltas,
  a running tool's progress, thread and account state) leave no record. A Codex
  item of an unknown type is written as a tool call and result under its own
  type name, with the rest of the item as its input.
- **Secret hygiene.** Every text passes the same redaction as stderr (agent,
  machine and session keys, provider and host API keys by prefix, PEM private
  keys from their `BEGIN` line through their `END` line, bearer and
  basic credentials, cookies, and the value of anything named like a key, token,
  secret or password) before it is written, in one place every adapter writes
  through. That is a list of known shapes, not a promise: a tool can print a
  secret no rule recognises, which is why this log, like stderr, is only ever
  shown to an owner or admin.
- **Size.** One record keeps at most 16 KiB of a tool call's input or of a tool
  result, and 64 KiB of a prompt, of the model's text or of an error; what is
  over is cut at a character and replaced by a last line `... N bytes cut`. A
  file rotates at 4 MiB with three backups (16 MiB per agent).
- **Retention.** The same as stderr and counted in the same total: a file whose
  last write is more than 14 days old is removed, and past 256 MiB for all
  agents the oldest files go first, except what was written in the last five
  minutes or belongs to an agent whose turn is running.
- **Reading.** As for stderr, through `agent_log_read` with `log_stream:
  "conversation"`, for a project's owner or an admin only. A supervisor
  advertises `supports_agent_conversation_log`; the server does not ask one
  that does not (it would answer with stderr), and the route answers 409
  `supervisor_outdated`. A tail that was cut starts at the first line of a
  record, going back past the lines asked for when the cut falls inside one;
  only a record longer than the read's byte limit comes as its end behind a
  line `... earlier part cut`. A read is never empty while the log has
  content. The page also names the turn its first records are in (`turn`) when
  that turn's start is before the page.
- **Private chats.** The log is one per agent, whichever room a turn served,
  and the route that reads it checks only the room it is asked through. The
  server therefore looks up the rooms of every turn on the page (the turn's own
  room, the room of each message it was given, and every room whose content
  the turn read: what a toolbox read such as `message_get`, `room_history` or
  `memory_recall` returned, recorded per turn by the server that answered it,
  and what its context quoted from other rooms, recent history or `#N`
  references) and replaces a turn that
  served a private chat by one line, `<time> withheld private chat turn:
  content withheld`, unless the reader is an active member of that chat. Being
  the project's owner or an admin does not open someone else's private chat,
  here as everywhere else. A turn that served a restricted room (routing,
  advice) is replaced by `<time> withheld restricted room turn: content
  withheld` unless the reader can read that room under the room-access rule of
  every other read: a member of its project who is its owner or operator, or
  who has a seat in it. A global admin who is only a plain member of the
  project reads the agent's log but not those turns. A turn that started before
  migration 129 ran (the migration's `schema_version.applied_at`) and has no
  `turn_room_read` row recorded nothing about what its tools read, so it counts
  as having served every room its agent was a member of when it started
  (`room_member` joined and left times), its private chats included, and is
  withheld from a reader who cannot read one of them; turns after the upgrade
  are judged on their records. The same rule judges such a turn for a lead
  (below). A turn the server cannot place (its room was
  removed, an older supervisor did not name the page's first turn, a turn from
  before migration 129 whose agent's membership of its own room then is not
  recorded) is withheld
  from everyone with `<time> withheld turn the server cannot place in a room:
  content withheld`: what cannot be shown to be open is not shown. The stderr
  log is not filtered; it holds what the harness printed, not the conversation.
- **A worker's output for its lead.** Two ways hand a worker's conversation to
  another agent, both under the room-read rule of an agent rather than a
  person's: a turn is shown only when the reader may read every room it served
  under the `room_history` rule (a room of its project that is neither a private
  chat nor restricted, or one it is an active member of; a chat between agents
  only to its members; a removed room to nobody), and the rooms of every turn
  shown are recorded against the reader's own turn, so the reader's log is
  withheld in turn from whoever may not read them. Keys and tokens are masked
  again on the server, and the text is always marked
  `untrusted`: it is the worker's output and may quote anything its tools read,
  so the reader weighs it as evidence and never follows it as instructions.
  - A notice about a worker (a silent-turn note, reason `silent_turn`, or a
    worker recovery handed to the lead, reason `worker_recovery`, or a person's
    stop of a worker handed to the lead, reason `worker_stopped`) carries
    `worker_output` on its delivered note: `{untrusted, agent_id, turn_id,
    text?, truncated?, withheld?}`, the end of that one turn's records, at most
    2 KiB, cut at a line. The server starts reading it from the worker's
    machine in the background when the notice is written (each read at most
    3 s, at most 8 at once, kept in memory for an hour per worker turn and
    recipient), so nothing of the log is stored on the server. The delivery
    waits at most about 250 ms for a read that has not finished, so a slow
    machine never holds up the dispatcher's pass for other agents; the lead
    can still call `agent_output`. The turn is judged against the
    recipient's room access when the read is made and again, from the
    membership at that moment, on the first delivery that uses a kept read,
    so a lead removed from a room meanwhile gets `room_not_readable`. A turn
    sent again carries the notes it carried first. Without text, `withheld` says why: `room_not_readable` (the turn served a room the recipient may not read), `not_found` (the turn is no longer in the end of the worker's log), `unreadable` (the machine is offline or too old, or the read failed) or `not_ready` (the read had not finished when the note was delivered). An older
    supervisor drops the field and the notice reads as before.
  - The `agent_output` tool (`GET /api/agent/agents/{id}/output`, see
    [the API](api.md)) reads a worker's latest turns (1–10, 32 KiB) for an
    active lead, or for the flow's coordinator for an agent it manages;
    a turn the reader may not read is replaced by one `withheld` line.
- **A new toolbox tool.** Every route under `/api/agent/` is classified in
  `toolRoomReads` (`internal/server/agent_tool_room_reads_test.go`), and the
  test there fails for a route that is not. A route whose answer carries
  content of a room (a message, a room's notes, what a schedule posts, a turn's
  output) is "records room reads": its handler calls `recordToolRoomReads`
  with those rooms before it answers, and answers an error when that fails.
  Anything else is "returns no room content", with the reason. Echoing what
  the agent itself just wrote is not room content; a room's title is not
  either.

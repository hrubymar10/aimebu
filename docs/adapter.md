# Adapter interface and fake

`internal/adapter.Adapter` starts one process per wake with a `TurnSpec`:
agent identity and tool selection, checkout path, optional native session
handle and started flag, mode (`normal`, `correction`, `diagnostic`), note and context JSON, and allowed
tools. `Turn.Events()` reports activity, usage, compaction, final-answer,
native-session-started, and process-exit events. Every event has a stable
`source_event_id`. The first native-session-started event records a native
session handle in the supervisor journal and server session. Later deliveries
carry that handle and `native_session_started=true` so the adapter can resume.
The final-answer event carries the tool's raw candidate text; the server later
validates its room targets and answer schema. A single surrounding Markdown
fence (`json` or unlabelled) is accepted around the whole JSON document on
every adapter path. Other prose outside the document remains invalid. The
wire retains the exact raw candidate, including any fence. A normal candidate has a
`messages` array. Each entry requires `room` and `body` and may add
`reply_to`, `thread_root`, `attachments`, `ask`, `needs_attention`,
`proposed_answers`, `open_questions`, `handoff`, `visual_plan`, and
`appendix_pages`. `needs_attention` creates a free-text human question;
`proposed_answers` adds two to four quick choices, or `open_questions` adds
up to ten questions with two to eight options each. The two answer formats
cannot appear together or with `ask`. `Turn.Wait()` returns exit code,
terminal-event evidence, and cancel intent separately. Exit code zero alone
does not mean the answer completed.
Each delivery context includes `you_are` (the current agent's ID, name, role,
harness, and model) and `team_roster` (those fields for up to 20 current agent
members of the turn's room). The recipient, leads, and agents who authored or
were mentioned in delivered notes come first. `team_roster_more` says `N more`
when members are omitted. A `#N` in a delivered note is a message number: the messages
it names (at most five, not ones the delivery already carries, code spans ignored) are
listed in the context as `referenced_messages` (`{no, available, room_id?, room?, from?,
body?, created_at?}`, the body cut to 1500 characters). Only rooms of the agent's own
project qualify (a private chat only for a member of it); anything else is `{no,
available:false}` with nothing more, so another project's messages and unknown numbers look
alike. The roster is read at dispatch time, so a later
turn reflects joins, leaves, and role changes. A recipient may be absent from
the roster after leaving a room but is still identified by `you_are` for a
recovery turn. The roster is a snapshot, not an access grant; room membership
is still checked when a candidate is committed.
Mention ranking uses the original message body and the same addressing rules
as wake delivery. Mentions only in visual blocks or appendix pages do not
affect roster order.
On an exit without a final answer, both native and Docker adapters attach the
last 4 KiB of launcher stderr to the exit evidence. Agent keys and bearer
tokens are redacted before the tail enters the supervisor journal, server
events, recovery inbox, or agent-card tooltip. Every adapter also tees the
whole of the harness's stderr into `TurnSpec.StderrSink` when the supervisor
sets one (the per-agent output log, see
[the supervisor](supervisor.md#agent-output)); a sink that fails never stops
the stderr pipe from being read.
Normal turns may carry `AgentToolsConfigPath`, a supervisor-owned 0600 MCP
config file path. A real adapter can pass that file to its CLI's MCP config
flag without placing the agent key in argv or the prompt. Docker variants read
it on the host and pass an authenticated HTTP MCP endpoint inline in the
CLI arguments; the key is visible to local process inspection during a turn.
No key-bearing file is written in the checkout: agents homed in the same task
room share a checkout, and a co-tenant Docker agent running as the same
container user could read another agent's bearer and act as it. Both Claude
Docker (`--mcp-config`) and Codex Docker (`-c`) therefore pass the bearer in
argv until the Docker launcher supports a private per-turn channel, such as
environment passthrough or a mount that is not shared between agents.
Correction and
diagnostic turns leave this path empty; no messaging tools live there.

The in-process `Fake` consumes a script keyed by turn ID. Scripts can emit
events, append a file or run an effect hook, wait on a gate, hang until
cancelled, exit with or without terminal evidence, or crash. An effect
followed by a crash before its activity event models the recovery gate case:
the effect happened, but the observer has no event proving it. The fake
assigns deterministic source IDs from the turn ID and event order when a
script does not specify one. A default script can cover turn IDs allocated by
the dispatcher during tests. It preserves malformed final-answer bytes.
The CLI enables the fake's default echo for local demos. Test scripts take
priority over the echo.

`adapter/adaptertest.Run` checks event order and unique IDs, cancellation
(including a repeated cancel, a cancel after the process has exited, and a cancel
in the gap between the process being reaped and the exit being reported, which
must all be no-ops that record no cancel intent), zero exit without terminal evidence, and crash after an
unreported effect. Every adapter signals its process group through
`adapter.SignalGroup` and records the reap with `adapter.Reaper`: a reaped process
is never signalled, and a group that is already gone (Linux answers `ESRCH`, macOS
may answer `EPERM`) is success once the process has been reaped, while a
permission error on a live process is still returned.
The native adapter fixtures run the same scenarios. Each adapter lists
its detected tool versions and environments through `Catalog`; the
supervisor derives its `hello` capability reports from that list. `Registry`
stores capabilities by exact tool, version, and environment; an upgraded version
or different environment starts unverified. The fake advertises all flags
except `readonly_verified`, which tests may explicitly enable. The supervisor
journals deliveries and answers before
sending them to the server. Its `hello` reports verified capabilities for an
exact tool, version, and environment; a missing or unmatched report grants no
capability.

## Claude Code native adapter

The native Claude Code adapter starts one `claude -p --output-format
stream-json` process for each wake. When a delivery carries a native session
handle and the started flag, it passes that exact value with `--resume`; a
handle without the started flag (the first turn) goes out as `--session-id`. A turn
with the fork flag (the first turn of an agent working on a copy of an existing
Claude Code session, see [claude-sessions](claude-sessions.md#working-on-a-copy-fork))
resumes the original with `--resume <id> --fork-session`, and reports the first
session id that differs from the original as the turn's `native_session_started`
(the copy's id); later turns resume that id. It never uses ambiguous
continue-last-session flags. The input prompt is one JSON object containing
the turn ID, notes, context, and allowed tools.
The Claude schema spells out `ask:{kind,text,options,free_text}` for inbox
questions. Its prompt tells a lead to continue after `create_room` and target
the returned room ID; `context.rooms` is only the turn-start snapshot. The
server still verifies current membership before committing any message.
Claude and Codex final-answer schemas also expose `needs_attention`,
`proposed_answers`, `open_questions`, `visual_plan` blocks, and
`appendix_pages`. A block's `data` is a JSON-encoded object string in those
native schemas; Pi may send an object directly. The server accepts either,
stores the raw block, and warns when its shape does not match a known renderer.
For an Inbox `ask`, the native prompts show `needs_attention:false`,
`proposed_answers:null`, and `open_questions:null` in the same message.
An agent's reply to another agent's message wakes that agent, but only up to four
times in a room with no human message between them (then it keeps the thread and
wakes nobody); replies to humans still target them. The native
prompts state this rule and direct agents to avoid
acknowledgement-only turns and to publish a completed lead summary promptly.
They also say that a human's message to the agent (addressed to it, or posted room-wide in its
room) always gets a reply, even a one-line one: silence and reaction-only rules are for
agent-to-agent traffic and for the agent's own unprompted progress reports. An agent that
answers a human with an empty `messages` list is following a stored rule, not failing; the
turn commits normally.
After spawning, they direct the lead to use a live mention in the assignment;
a name in code formatting does not wake the new agent.
On a failed Claude `result`, the adapter keeps a bounded, redacted copy of
the event in process-exit evidence. This lets the server identify a provider
limit and any reported reset time even when stderr is empty.
Claude, Codex, Pi, OpenCode, and Vibe prompts tell the agent to honor `retry_notice` on a
requeued note (an attempt interrupted by a provider limit or a lost session)
by checking the state before repeating actions.
The five native prompts (Claude Code, Codex, Pi, OpenCode, Vibe) are built by `internal/adapter/prompt` from sentences they
share plus each adapter's own deltas (where the turn JSON comes from, the role
line, how to fetch attachments, the output contract, the acknowledgement rule,
and the closing line); each adapter's golden tests pin the exact bytes, so a
wording change shows up as a golden diff. The six shared sentences can be
overridden (Settings → Prompts): a turn whose context carries
`prompt_overrides` builds its prompt with them, and every other part, the
output contract included, stays as shipped. The overrides reach the agent only
through that instruction text: every adapter forwards the context to the agent
without `prompt_overrides` (`prompt.WithoutOverrides`), so the agent never reads
them twice. A turn without overrides uses the shipped bytes, so the goldens are
unchanged.
Input attachments on notes carry an ID, safe display name, MIME type, size,
and digest. Claude Code, Codex, and native Pi fetch content through the
room-scoped `attachment_get` MCP tool. Pi Docker has no MCP channel; its prompt
receives a turn-scoped HTTP bearer and reachable URL for on-demand download to
its own temporary directory. The bearer is omitted from the supervisor journal
and stops working when the turn ends.

Its parser retains the result event's `result` string byte-for-byte as the
final candidate. Assistant, tool-progress, system, user, and rate-limit
records become activity; usage and context/cost fields are forwarded when
reported. Claude context use adds input, cache-read, and cache-creation tokens
from a reported result; its model costs are summed and flagged `cost_cumulative` (a resumed session's
result reports the whole session's spend, so the store subtracts the session's
previous total to get the turn's own cost), and a context window is
used only when all reported models agree. `system/compact_boundary` becomes a
compaction event. Fields Claude does not emit remain unknown. Cancellation
kills the per-wake process and
records cancel intent independently of its exit code. A stdout line longer
than the parser's 8 MiB limit stops parsing; the adapter then kills the
process group, as the Codex, Pi, OpenCode, and Vibe adapters do, and reports a
non-terminal exit instead of waiting on a child blocked on its unread pipe.

The catalog probes `claude --version` at supervisor startup and advertises it
only when the executable is available. CLI version is discovery data, not a
model choice: the server's spawn catalog separately bounds Claude models to
the owner-editable list (default `haiku`, `sonnet`, and `opus`). Normal turns receive the private MCP config
with strict config enforcement; diagnostic turns use the narrow read-only
tool flags. Its capability report advertises schema, compaction,
context-window, cost, and confirmed-cancel support; it does not claim
verified no-action or read-only modes. The normal test suite uses a scripted
`claude` binary.
Set `AIMEBU2_LIVE_CC=1` to opt into the separately gated live canary.

## Codex native adapter

The native Codex adapter uses `codex exec --json` and `codex exec resume`
with the journaled thread handle. It passes turn input on stdin, keeps the
exact final candidate bytes, and parses thread, activity, usage, and
completion events through `internal/harness`. `turn.completed.usage` counts
the cached share inside `input_tokens` (`cached_input_tokens` is part of it), so
the adapter reports Input as `input_tokens` less the reported cache-read and
cache-write shares (never below zero), with those shares shown separately.
When Codex omits either cache figure, that share stays unknown and is treated
as zero in the estimate. Codex 0.162.1 also prints
`reasoning_output_tokens`; output already includes it. For listed models,
the adapter estimates a dollar cost from the dated
[model price list](pricing.md) and marks its source `estimated`. An unlisted
model, missing input/output, or inconsistent token shares leave cost unknown.

Its private per-agent `CODEX_HOME` holds the structured-output schema and a
translated MCP config. Only the private config contains the agent key; the
inherited process environment excludes `AIMEBU2_*`. Normal turns use the
approved action-enabled CLI mode; correction and diagnostic turns request
read-only sandboxing and omit the toolbox. Container sandbox verification
failed during the spike, so Codex does not claim `readonly_verified` or
`no_action_mode`. Capabilities tested against Codex CLI 0.162.1 on Linux are
not carried to a new version or OS. Set `AIMEBU2_LIVE_CODEX=1` to run the
opt-in two-turn native resume canary.

A v2 turn continues an existing harness session when the agent's session row
has a native session id and `native_session_started_at`: `claude --resume <id>`,
`codex exec resume <id>`, `opencode run -s <id>`, `vibe -p --resume <id>`; Pi passes `--session-id <id>` on
every turn. Only Claude Code runs in the user's own home, so only it can
continue a session that v1 made (see [Start as v2 agent](import-v1.md#start-as-v2-agent));
codex, pi, opencode and vibe run with a private home or data directory per agent, which
a v1 session is not in.

Both standalone and embedded supervisors route `fake`, `claude`, `codex`, `pi`, `opencode`, and `vibe`
turns through `adapter.Multi` when their CLIs are available.

## Pi native and Docker adapters

Pi starts one `pi -p --mode json` process per wake. The server allocates a
session UUID at spawn or reactivation; the adapter passes it with
`--session-id` and uses a stable per-agent `--session-dir` below the mirrored
checkout. The turn prompt goes through piped stdin so a batch of large notes
cannot exceed the operating system's per-argument limit. The first `session`
lifecycle event must confirm that ID before a
candidate can be final. The parser takes the terminal assistant text from
`turn_end`, retains malformed JSON for the server's correction path, and
forwards usage and `compaction_end` token counts. The adapter's explicit
`Compact` operation uses Pi RPC `{"type":"compact"}`; sending `/compact` as a
print prompt does not compact a session.

Pi retries retryable provider failures itself (three retries, 2, 4 and 8
seconds apart) and can exit 0 when they run out, with no settled run. The
adapter treats that as a non-terminal exit and keeps the last provider error
Pi reported in the exit evidence, so recovery shows the real cause rather than
a clean exit code. Only that provider error counts, and only wording such as
`usage limit`, `usage_limit` or a weekly, hourly, monthly or daily limit (never a
plain `rate limit`) routes into the provider-limit gate.

Normal native turns allow the built-in action tools and aimebu MCP tools over
HTTP. Their MCP config is a private 0600 file under the supervisor's Pi
runtime root, isolated with `PI_CODING_AGENT_DIR`. The installed Ollama Cloud
provider extension is loaded explicitly for the bounded model without loading
other user packages. Correction and diagnostic turns
allow only `read,grep,find,ls`, disable MCP, and do not load project context
files or skills. Pi 1.1.0 has no response-schema flag; the server validates
the raw candidate. The verified no-action tool list enables one correction
turn, while read-only isolation remains unverified. Native execution is
controlled by the selected machine launcher.

Pi Docker runs through `pi-docker` from a mirrored checkout and uses the same
session directory. Its launcher has no private credential channel for HTTP
MCP, so the spawn catalog reports `toolbox:false` with a reason and Pi Docker
turns use `--no-mcp`. Pi Docker's turn attachment bearer is included in its
prompt and may remain in the checkout's Pi session transcript. The bearer
works only while that turn is live, for delivered files in rooms where the
agent remains a member; a saved transcript cannot fetch files after the turn.
No long-lived agent key is written in the checkout. The
owner lists the models of a Pi harness in Settings → Harnesses (Auto-detect models
runs `pi --offline --list-models` on the machine; ids are `provider/model`). Set
`AIMEBU2_LIVE_PI=1` or `AIMEBU2_LIVE_PI_DOCKER=1` to run the respective
two-turn nonce-recall canary.

## OpenCode native adapter

OpenCode starts one `opencode run --pure --format json -m <provider/model> --dir
<checkout>` process per wake. `--pure` keeps it from installing external
plugins, which otherwise blocks a cold start on the network. The prompt goes in
on stdin, which must reach EOF: OpenCode appends piped stdin to the message and
otherwise waits for it forever. OpenCode allocates its own session ID
(`ses_…`) on the first turn, so the handle comes from the first event and later
turns pass `-s <id>`; an unknown ID makes it print `Error: Session not found` on
stderr, which resets the session the way a missing Codex thread does.

The stream is JSONL (`step_start`, `text`, `tool_use`, `step_finish`, `error`).
A turn is terminal only when a step finishes with reason `stop` after text, no
error event, and the session ID stayed the same. The answer is all the text
parts of the last step, joined as they arrive; narration in earlier steps
(before a tool call) is ignored. Usage sums the turn's steps
(tokens, cost, and the last step's prompt as context used). A finished tool
call is reported once and is activity, so a long silent tool call can trip the
idle deadline: raise `AIMEBU2_TURN_IDLE` for such work. Provider failures are
retried by OpenCode for about 70 seconds without events and then end with one
`error` event; it goes into the exit evidence as `OpenCode error: APIError
(HTTP 429): …`, and wording such as `usage limit`, `usage_limit` or a
weekly, hourly, daily or monthly limit (never a plain `rate limit`) routes into
the provider-limit gate.

Normal turns run with `--auto` (permissions not explicitly denied are
approved, like the Claude and Codex bypass modes) and get the aimebu MCP
toolbox over HTTP through an inline `OPENCODE_CONFIG_CONTENT` with the agent's
private bearer. Correction and diagnostic turns drop `--auto` and the MCP and
deny `bash`, `edit`, `webfetch`, and `external_directory`, which removes those
tools from the model's tool list. Isolation: the user's data directory is kept
so OpenCode finds its own `auth.json` (aimebu never reads or copies it), but
the agent gets a private `XDG_CONFIG_HOME`, a private session database
(`OPENCODE_DB`) under the supervisor's `opencode` runtime root, and the inline
config with autoupdate and sharing disabled, project config and Claude Code
compatibility files ignored, and the title-generation call turned off. Only a
bounded list of provider API-key variables passes through.

The owner lists the models of an OpenCode harness in
Settings → Harnesses (Auto-detect models runs `opencode models`); ids must be
`provider/model`.
The adapter's capabilities are pinned to the verified release, 1.19.0 on
Linux.

### OpenCode in Docker

The Docker variant runs the same turn through the `opencode-docker` launcher. OpenCode is configured
by environment variables alone (the inline config document with the toolbox and its bearer key, the
private session database, the config folder), and the launcher starts the harness with
`docker exec`, which does not carry the supervisor's environment into the container. Putting those
values in the arguments would show the bearer key in every local user's process list. The adapter
therefore lists the variables in `HARNESS_DOCKER_FORWARD_ENV` and puts their values in the launcher's own
environment: the launcher passes them to the harness by name (`docker exec -e NAME`), so they never
appear in argv. The list is the config document (`OPENCODE_CONFIG_CONTENT`), `OPENCODE_DB`,
`XDG_CONFIG_HOME`, the three `OPENCODE_DISABLE_*` switches the native adapter sets, and the provider
API-key variables that are set on the host. Everything else of the supervisor's environment stays
behind, and an owner's environment reference to any of these names (a provider key that is set on the
host included, so it cannot be swapped for another variable's value), or to the launcher's flag and
forwarding variables, is ignored: such references configure the launcher, not the agent.

The agent's private state lives in `<checkout>/.aimebu/opencode/<agent hash>/` (its config folder
and `opencode.db`), seen from the container under the launcher's path mapping; OpenCode's own data
folder, with its `auth.json`, is the one the launcher's container mounts. The MCP endpoint is
the same `/api/agent/mcp` as for the other Docker harnesses (see below), with `localhost` mapped to
`host.docker.internal`. `OPENCODE_LAUNCH_FLAGS=` (empty) is set because the launcher's built-in
`--auto` comes before the subcommand and would start the interactive interface instead of `run`; the
adapter passes `--auto` itself, after `run`, for normal turns.

A launcher that predates the feature would start the harness with none of this. The checkout preflight
and every turn therefore run the launcher with `HARNESS_DOCKER_PRINT_FEATURES=1 … --version`: a launcher
that has the feature prints `forward_env` and exits before it touches Docker, and one that does not
prints a version. The latter is refused with a message to update harness-docker. The supervisor also runs the probe once
when it inspects such a launcher and reports the answer with the launcher row (`features`, and `feature_problem` when the
probe failed or the launcher is too old), which `GET /api/machines/{id}/launchers` passes on and `aimebu doctor --live`
reads; the row stays available, the turn's preflight is what refuses. The opt-in live canary
(`AIMEBU2_LIVE_OPENCODE_DOCKER=1 go test ./internal/adapter/opencode`) runs two real turns through the
launcher (a nonce, then a `-s` resume that must give it back) and checks the session id and the usage of
each. The provider key comes from the environment (`OLLAMA_API_KEY` for the default model
`ollama-cloud/gemma4:31b`; `AIMEBU2_LIVE_OPENCODE_MODEL` names another `provider/model` whose key is among
the forwarded variables), never from the person's home. The Docker variant still claims no capabilities:
read-only turns, cancel and cost inside a container are not verified by it.

An agent turn does not see the person's own OpenCode configuration (the config folder is private and project
config is off), so a provider defined only there, such as a custom OpenAI-compatible endpoint, is unavailable to
it: only the built-in providers whose key variables are in the forwarded list can be used.

A model that reasons in its reply text (GLM over an OpenAI-compatible provider) streams `<think>…</think>`
ahead of the JSON in the same text part. The adapter passes the text through unchanged; the server's answer
parser removes leading reasoning elements (see the contract).

## Mistral Vibe native adapter

Vibe starts one `vibe -p --output streaming --trust --workdir <checkout>` process per wake. With no
`TEXT` argument `-p` reads the prompt from stdin, which must reach EOF. Vibe allocates its own session
ID (a UUID) on the first turn; later turns pass `--resume <id>`, and an unknown ID makes it print
`Error: Session not found: <id>` on stderr, which resets the session the way a missing Codex thread
does. The model is `VIBE_ACTIVE_MODEL=<alias>`: there is no `--model` flag, and the alias is one the
agent's config knows (see below), because Vibe silently falls back to its default model for an alias it
cannot find.

The stream is one JSON entry per line, each a finished entry of the session's history (`message`,
`reasoning`, `effect`, ...; camelCase keys `sessionId`, `generationStatus`, `source`). A resumed
session replays its earlier history first, so only what follows the entry that carries this turn's own
prompt (`type` `message`, `role` `user`, `source` `turn_start`) counts. The answer is the last
assistant message after it; narration before a tool call is ignored. A turn is terminal only when the
process exited 0, that prompt was seen, an answer followed, no `<vibe_stop_event>` message ended the
turn, and the session ID stayed the same. A stop event (a turn, price or token limit) is evidence
(`Vibe stopped the turn: …`), never an answer, and aimebu sets none of those limits. An `effect` is
reported once it has finished, so a long silent tool call can trip the idle deadline: raise
`AIMEBU2_TURN_IDLE` for such work. API and configuration failures come on stderr as `Error: …` with exit
status 1; the last such line goes into the exit evidence as `Vibe error: …`, and wording such as
`usage limit` or a weekly, hourly, daily or monthly limit (never a plain `rate limit`) routes into the
provider-limit gate.

A session exists only after Vibe exits cleanly, and a cancelled or killed turn persists nothing. The
native session handle is therefore reported after a clean exit, never from the first entry, so a first
turn that is cancelled leaves nothing to resume. Cancel signals the process group (SIGTERM, then
SIGKILL after five seconds).

Vibe streams no usage. After the process has exited, the adapter reads the session's `meta.json`
(`<save dir>/session_<time>_<first 8 of the id>/meta.json`, its `stats`): the session totals are
cumulative across resumes, and cached tokens are counted inside the prompt tokens, so a turn's own
usage is the totals now less what they were before it, with the cached tokens split out of the input.
The cost is Vibe's own `session_cost`, cumulative (the store takes the difference), and unknown for a
model with no price. A turn that left the totals unchanged (killed before Vibe saved) reports nothing;
one that ended without an answer reports what it spent as partial. A resumed session whose totals
before the turn cannot be read has nothing to subtract from, so its tokens stay unknown (logged) and
only the cumulative cost is reported.

Isolation: every agent runs with its own `VIBE_HOME` and session directory
(`VIBE_SESSION_LOGGING__SAVE_DIR`) under the supervisor's `vibe` runtime root. Before every turn the
adapter writes that home's `config.toml` with the person's `[[models]]` and `[[providers]]` tables
copied from their own config (`$VIBE_HOME` or `~/.vibe`; nothing else of it, so their MCP servers,
hooks, skills and agents never reach an agent) under settings that turn off telemetry, update
checks, auto-update, notifications and connectors, and links the person's `.env` into it (it is never
opened by aimebu). Only a bounded environment passes: `MISTRAL_API_KEY` and the key variables the copied
providers name. Normal turns run with `--trust --yolo` and get the aimebu toolbox through `VIBE_MCP_SERVERS`,
whose `api_key_env` names a variable that carries the agent's private bearer: the bearer is in neither
argv nor a file. `--trust` (this invocation only; nothing is saved to `trusted_folders.toml`) makes Vibe
load the checkout's own configuration, a `.vibe` or `.agents` folder and `AGENTS.md`, the way the other
harnesses read theirs; Vibe has no switch that keeps `AGENTS.md` but ignores the rest, and the agent
already runs with every tool approved. Correction and diagnostic turns drop `--yolo`, `--trust` and the
toolbox and pass `--enabled-tools read_file --enabled-tools grep`, which with `-p` disables every other
tool; the untrusted checkout's hooks, MCP servers, skills and agents are never loaded into them (Vibe
prints a warning about it on stderr). The variables the adapter sets itself (`VIBE_HOME`,
`VIBE_ACTIVE_MODEL`, `VIBE_SESSION_LOGGING__SAVE_DIR`, `VIBE_MCP_SERVERS` and the bearer's variable)
come last and an owner's environment reference to one of them is ignored.

The owner lists the models of a Vibe harness in Settings → Harnesses; as Vibe
has no command that lists them, Auto-detect models reads the `[[models]]` of the machine's Vibe config
(by alias) and adds Vibe's built-in default, `mistral-medium-3.5`. The adapter's capabilities are
pinned to the verified release, 2.25.0 on Linux, and claim cost and cancel confirmation only: no-action
mode is not claimed until a live canary has shown that a read-only turn loads none of the person's MCP
servers. `AIMEBU2_LIVE_VIBE=1` is reserved for that opt-in canary, which has not been written or run.
The stream entries in the tests are shaped from the Vibe 2.25.0 sources, not recorded from a run.

### Mistral Vibe in Docker

The Docker variant runs the same turn through the `vibe-docker` launcher, on the same channel as
OpenCode in Docker (see above): the container does not inherit the supervisor's environment, so the
adapter lists the variables it sets (`VIBE_HOME`, `VIBE_ACTIVE_MODEL`, `VIBE_SESSION_LOGGING__SAVE_DIR`,
`VIBE_MCP_SERVERS` with the variable that carries the bearer key, and the provider key variables
that are set on the host: `MISTRAL_API_KEY` and the ones the copied provider tables name) in
`HARNESS_DOCKER_FORWARD_ENV` and puts their values in the launcher's own environment. A harness-docker
that has the matching feature passes them by name, so no value is ever in argv, and a launcher that
does not is refused before the turn runs. The launcher's own flags are cleared
(`VIBE_LAUNCH_FLAGS=`): its built-in `--yolo` would also apply to a read-only turn, so the adapter
passes its flags itself, as natively.

The agent's private home (with the `config.toml` seeded from the person's models and providers, read
on the host, and their `.env` linked in) and its session folder are `<checkout>/.aimebu/vibe/<agent hash>/home`
and `.../sessions`, seen from the container under the launcher's path mapping; the stats of a
finished turn are read from the host side of the same folder. The `.env` link points at the person's
own Vibe home, so it resolves in the container only if the harness container mounts that folder at the same
path (harness-docker mirrors `VIBE_HOME`, `~/.vibe` by default). The adapter cannot see a container's mounts:
when Vibe reports a missing key, the exit evidence says the key reaches the container from the host environment
(forwarded by name) or from that `.env` under the condition above, and naming the host folder. Setting
`MISTRAL_API_KEY` on the host avoids the question. `PYTHONUNBUFFERED=1` is forwarded as natively, so the
streamed entries arrive as they are written. The MCP endpoint is the same
`/api/agent/mcp` as for the other Docker harnesses, with `localhost` mapped to `host.docker.internal`.
The opt-in live canary (`AIMEBU2_LIVE_VIBE_DOCKER=1 go test ./internal/adapter/vibe`) runs two real turns
through the launcher (a nonce, then a `--resume` that must give it back) and checks the session id and the
usage of each. It reads `MISTRAL_API_KEY` from the environment only (the test home is a temporary one, so
`~/.vibe/.env` is not consulted) and takes the model alias from `AIMEBU2_LIVE_VIBE_MODEL` (default
`mistral-medium-3.5`). The Docker variant still claims no capabilities: read-only turns, cancel and cost inside
a container are not verified by it.

## Docker variants

The `docker` variants use `claude-docker`, `codex-docker`, `pi-docker`, `opencode-docker` and `vibe-docker` from the
supervisor's `PATH`. The launchers receive the same turn arguments and stdin
as the native CLIs. A configured path map translates Codex's `-C` checkout
and output schema paths to their container-visible locations; Pi translates
its session directory. The supervisor runs a bounded
launcher version probe from the requested checkout before recording the agent;
an unmounted path fails spawn with the launcher's error. The adapters create
`<checkout>/.aimebu/` for Codex's output schema and Pi sessions. The Docker
launcher version probe supplies the catalog version. The owner adds and enables
native or containerized harnesses per machine in Settings → Harnesses; a new
machine has none.

Claude and Codex Docker turns connect to the server's `/api/agent/mcp` HTTP endpoint using the
supervisor's private agent key. Localhost URLs are mapped to
`host.docker.internal` for the container, so the server must listen on a
non-loopback IP with `AIMEBU2_BIND`, and `AIMEBU2_ALLOW` must admit the Docker
network source. Pi Docker needs the same network access for turn attachments.
A loopback-only bind makes Docker catalog entries unavailable and blocks direct
spawn, switch, and reactivation requests.
The server checks the agent key and exposes the same managed toolbox as stdio
MCP.
Codex's response schema is written below the mirrored checkout, while its
credentials stay in the container's configured Codex home. Codex Docker turns
pass `exec --ignore-user-config`, retaining those credentials while skipping
the mounted user's `config.toml` and its MCP servers. The only MCP configured
for a normal turn is aimebu's HTTP endpoint. Claude Docker passes
`--strict-mcp-config` for the same MCP isolation. On a failed Codex turn, the
last stdout `error` or `turn.failed` event is included with the stderr tail in
recovery evidence after redaction. Docker capabilities
do not claim confirmed cancellation until container-session teardown can be
verified. Set `AIMEBU2_LIVE_CC_DOCKER=1` or
`AIMEBU2_LIVE_CODEX_DOCKER=1` to run the respective two-turn Docker canary;
the canary checkout is created below this repository's ignored `tmp/`.

`claude-docker` does not forward `CLAUDE_CONFIG_DIR` (it is not among the variables the Claude Docker turn lists in
`HARNESS_DOCKER_FORWARD_ENV`, and `docker exec` carries nothing else of the supervisor's environment): a Claude Docker turn
always uses the container's own Claude home, never a per-run or temporary one. That is why an existing Claude Code session
cannot be continued or copied through it. The server refuses `resume_session` on a containerized launcher for both
`resume_mode`s (409 `needs_native`), and the fork canary below runs native.

`AIMEBU2_LIVE_CLAUDE_FORK=1 go test ./internal/adapter/claude` runs the fork canary: a real first turn on a throwaway
session, then a second that resumes it with `--fork-session`. The copy must report a session id of its own and recall what
the first turn was told, and the original transcript must be byte for byte unchanged. Claude Code runs with the test's
temporary `CLAUDE_CONFIG_DIR`; its login is `ANTHROPIC_API_KEY`, or a copy of the file named by
`AIMEBU2_LIVE_CLAUDE_CREDENTIALS` (refused when it holds a refresh token, so that a copy cannot rotate the real login).

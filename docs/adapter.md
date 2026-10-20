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
when members are omitted. The roster is read at dispatch time, so a later
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
events, recovery inbox, or agent-card tooltip.
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

`adapter/adaptertest.Run` checks event order and unique IDs, cancellation,
zero exit without terminal evidence, and crash after an unreported effect.
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
stream-json` process for each wake. When a delivery contains a native session
handle, it passes that exact value with `--resume`; it never uses ambiguous
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
Claude, Codex, Pi, and OpenCode prompts tell the agent to honor `retry_notice` on a
requeued note (an attempt interrupted by a provider limit or a lost session)
by checking the state before repeating actions.
The three prompts are built by `internal/adapter/prompt` from sentences they
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
from a reported result; its model costs are summed, and a context window is
used only when all reported models agree. `system/compact_boundary` becomes a
compaction event. Fields Claude does not emit remain unknown. Cancellation
kills the per-wake process and
records cancel intent independently of its exit code. A stdout line longer
than the parser's 8 MiB limit stops parsing; the adapter then kills the
process group, as the Codex, Pi, and OpenCode adapters do, and reports a
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
completion events through `internal/harness`.

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
`codex exec resume <id>`, `opencode run -s <id>`; Pi passes `--session-id <id>` on
every turn. Only Claude Code runs in the user's own home, so only it can
continue a session that v1 made (see [Start as v2 agent](import-v1.md#start-as-v2-agent));
codex, pi and opencode run with a private home or data directory per agent, which
a v1 session is not in.

Both standalone and embedded supervisors route `fake`, `claude`, `codex`, `pi`, and `opencode`
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
a clean exit code. Wording that names a usage cap routes into the provider-limit
gate.

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
(HTTP 429): …`, and wording that names a usage, weekly, hourly, daily or
monthly cap routes into the provider-limit gate.

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

There is no Docker variant. The owner lists the models of an OpenCode harness in
Settings → Harnesses (Auto-detect models runs `opencode models`); ids must be
`provider/model`.
The adapter's capabilities are pinned to the verified release, 1.19.0 on
Linux.

## Docker variants

The `docker` variants use `claude-docker` and `codex-docker` from the
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

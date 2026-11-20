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
`reply_to`, `attachments`, `ask`, `needs_attention`,
`proposed_answers`, `open_questions`, `handoff`, `visual_plan`,
`appendix_pages`, and `force`. Set `force:true` on a message to bypass the
missed-agent-mention guard for that message. `needs_attention` creates a free-text human question;
`proposed_answers` adds two to four quick choices, or `open_questions` adds
up to ten questions with two to eight options each. The two answer formats
cannot appear together. An `ask` already puts the message in the person's
Inbox, so `needs_attention`, `proposed_answers` and `open_questions` sent with it are dropped
with a receipt warning instead of rejecting the reply. When a final answer is invalid and the
harness cannot be asked for a correction (no `no_action_mode`, as for Claude), the reply is held
and the agent gets a system note that wakes it: "Your previous reply was held: <error>" (quote-bearing parser errors are described, not echoed). A turn woken by such a note and held again gets no further note.
A message refused only for its target (a room in another project, a closed room, a room the agent is not in) never holds the whole reply: the other messages are delivered and the agent gets a note "Part of your previous reply was held: ..." with the held text and, for another project's room, the hint to use `dm_agent`.
`Turn.Wait()` returns exit code,
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
project qualify (a private chat only for a member of it), plus a message of another
project that a person who can read it cited (`#N`) in a message of a room the agent is in:
it carries `project`, the source project's name, and the person's access is re-checked on
each read; anything else is `{no, available:false}` with nothing more, so another project's
uncited messages and unknown numbers look alike. The roster is read at dispatch time, so a later
turn reflects joins, leaves, and role changes. A recipient may be absent from
the roster after leaving a room but is still identified by `you_are` for a
recovery turn. The roster is a snapshot, not an access grant; room membership
is still checked when a candidate is committed.
Mention ranking uses the original message body and the same addressing rules
as wake delivery. Mentions only in visual blocks or appendix pages do not
affect roster order.
On an exit without a final answer, both native and Docker adapters attach the
last 4 KiB of launcher stderr to the exit evidence. Agent, machine and session
keys (`aak_`, `amk_`, `as_`), provider API keys (`sk-ant-…`, `sk-…`), bearer tokens,
`x-api-key` values, cookie and set-cookie headers and `api_key`/`token`/`secret`
assignments are redacted before the tail enters the supervisor journal, server
events, recovery inbox, or agent-card tooltip (and before the per-agent output log is written). Every adapter also tees the
whole of the harness's stderr into `TurnSpec.StderrSink` when the supervisor
sets one (the per-agent output log, see
[the supervisor](supervisor.md#agent-output)); a sink that fails never stops
the stderr pipe from being read.
Normal turns carry `AgentTools` (`adapter.AgentToolsAccess`): the server address, the agent's own id and `aak_` key, the path of the `aimebu`
executable and the rest of the bridge's environment (telemetry). It is built per turn from the supervisor's memory, never journaled (`json:"-"`) and
never written to a file, and it holds no other agent's key. An adapter gives the key to the harness through the harness's **environment**
(`AIMEBU_AGENT_KEY`), never through its argument list (`ps` shows that to every local user): Claude gets an inline stdio MCP entry whose key is
`${AIMEBU_AGENT_KEY}`, Codex's private config names the variable (`env_vars`), Pi's header is `Bearer ${AIMEBU_AGENT_KEY}`, and the Docker variants build
their HTTP endpoint from the same access and forward the variable by name (`HARNESS_DOCKER_FORWARD_ENV`). `adapter.BridgeEntry`, `NativeMCP` and
`DockerMCPAt` validate the access (the agent id must match the turn's) and build those entries. No key-bearing file is written in the checkout:
agents homed in the same task room share a checkout, and a co-tenant Docker agent running as the same container user could read another agent's
bearer and act as it. A key in a process's environment is still readable by a same-user process that can read its environment (see
[the supervisor](supervisor.md)). Correction and diagnostic turns have no access; no messaging tools are given to them.

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
`appendix_pages`. Their strict native schemas list every object property in
`required`; optional message fields such as `force` use a nullable type and
the server treats `null` as unset. A block's `data` is a JSON-encoded object
string in those native schemas; Pi may send an object directly. The server accepts either,
stores the raw block, and warns when its shape does not match a known renderer.
For an Inbox `ask`, the native prompts show `needs_attention:false`,
`proposed_answers:null`, and `open_questions:null` in the same message.
An agent's reply to another agent's message wakes that agent, but only up to four
times in a room with no human message between them (then it keeps the thread and
wakes nobody); replies to humans still target them. The native
prompts state this rule and direct agents to avoid
acknowledgement-only turns and to publish a completed lead summary promptly.
They also carry the etiquette toward people (the `human_reply` sentence): don't narrate;
message a person only for a decision, an answer, an approval, or a result they asked for;
batch questions into one numbered message; acknowledge with a reaction (👍 seen, ✅ done)
instead of a text ack; and when a person addresses the agent directly (a mention, a reply to
its message, a DM), always respond, with an answer if there is anything to say and otherwise
a reaction, never with no sign at all. Every prompt asks the agent to set `reply_to` when its message answers a
delivered one (a reply joins that message's conversation; there is no separate thread link), and
explains the note's `why`, `in_reply_to` and `conversation`. An agent that
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
overridden (Settings → Agents → Prompts): a turn whose context carries
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
final candidate (the last result that answers the turn; see "Which result is the
turn's" under warm mode for the results the CLI's own queries produce). Assistant, tool-progress, system, user, and rate-limit
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
the list the owner sets on the harness in Settings → Agents → Harnesses & models (nothing is seeded; Auto-detect offers the current models). Normal turns receive the aimebu MCP entry
inline through `--mcp-config` and are not strict: the person's own MCP servers and connectors
(user, project and local scope, claude.ai) stay available to the agent, as in their
terminal. The aimebu entry takes precedence over a server of the same name in any
scope, so a v1 bus the person has configured under that name never reaches a v2
agent. Diagnostic turns pass `--strict-mcp-config` with the narrow read-only tool flags. Its capability report advertises schema, compaction,
context-window, cost, and confirmed-cancel support; it does not claim
verified no-action or read-only modes. The normal test suite uses a scripted
`claude` binary.
Set `AIMEBU_LIVE_CC=1` to opt into the separately gated live canary.

### Warm mode (adapter side)

The Claude and Codex adapters implement `adapter.WarmAdapter`. Claude: it can keep one `claude -p
--input-format stream-json --output-format stream-json` process up between turns, so a
turn that follows another soon after is written to the live process instead of starting
a process and resuming the session. The supervisor decides when a process starts and
ends; the adapter feeds the turns. The warm window defaults to five minutes;
Codex additionally requires the owner to enable `codex_warm_enabled`.

- `WarmFingerprint(spec)` hashes everything a warm process is started with (command,
  arguments, environment with the agent key only as a hash, directory) and leaves out which
  session is continued and the turn itself. A correction, a diagnostic or a fork returns
  `ErrNoWarm` and runs one process per turn as before.
- `StartWarm(ctx, spec)` starts the process with `--replay-user-messages` too;
  `WarmSession.Run(spec)` writes one stream-json user message (the same JSON a one-turn
  process reads on standard input). The returned `Turn` ends at that turn's own `result`
  event: a successful result with an answer is a terminal exit with code 0 and the process
  stays up. Any other ending (an error result, a crash, a cancel, input the CLI refuses)
  ends the process too, and the turn's exit is the process's own. A live session takes one
  turn at a time (`ErrWarmBusy`).
- **Which result is the turn's.** Claude Code runs queries of its own in the session: when
  a background task (a `run_in_background` command) ends, or a resumed session finds one
  that outlived its previous process (a deploy, the warm window's end), it reports it in a
  query that ends with its own `result`, marked `origin.kind: "task-notification"`. On a
  resume that happens before the CLI reads the turn's input (an empty result, no usage, no
  assistant event; taken as the answer, it was an `invalid_answer: EOF`), and a task that
  ends while the process is idle can run a full query (assistant events, a non-empty
  result) just before it. So a live turn accepts a result only after the CLI has repeated
  its input (the replayed user message whose text begins with `{"turn_id":"<this turn>"`).
  On either path, a result with `origin.kind` `task-notification`, or an empty one that
  spent nothing and followed no assistant event, is not an answer either (a result after
  the repeat with any other origin is not known to mean anything, so it is accepted and
  its origin journaled). Such a result is ignored: it does not end the turn or start the
  kill that follows a result without an answer. What its query spent is still reported,
  as a usage event of its own (its tokens, and the session's running cost, which the
  store turns into that query's cost as for any result); one that reports no spend at
  all gets no usage event. An error result always counts (it ends the process, and what it says, a provider
  limit or a failed sign-in, is the session's). Recorded with CLI 2.1.329 against a local
  stand-in for the API: `internal/adapter/claude/testdata/stream`.
- **What the journal keeps.** Every `result` is summarized in an event's `detail`
  (journal only; it is not in the frame sent to the server): `subtype`, `is_error`,
  `num_turns`, the length of `result`, whether `structured_output` is present,
  `session_id`, `origin`, and the count of assistant events before it. An answering
  result's summary is on its usage event, and so is an ignored one's (with why it was
  ignored); an ignored result that spent nothing gets an activity event `result` instead. Output a live process writes while no turn runs is not dropped
  silently: its event types (up to 32) and the first three lines (300 bytes each,
  redacted) are journaled with the next turn, as a `between_turns` activity event.
- Measured with a real run (haiku, CLI 2.1.318): `--json-schema` gives `structured_output`
  on every result of a multi-turn stream; `total_cost_usd` stays cumulative per session (the
  existing delta logic applies); an interrupt and a malformed input line each make the CLI
  exit 1, which is why any abnormal event means the process is gone; one idle process is
  about 240 MB resident.

Codex warm mode uses `codex app-server` over newline JSON-RPC. It initializes
against the private `CODEX_HOME`, resumes the journaled thread or starts one,
and sends the same turn text and output schema through `turn/start`. A completed
turn with a final answer keeps the process; an error, missing answer, crash, or
handshake/resume failure ends it and the supervisor falls back to `codex exec`.
The adapter declines app-server approval requests and never asks it to refresh
credentials. Docker app-server has no `--ignore-user-config` flag: the adapter
disables host MCP servers the cold Docker path would omit, and falls back cold
when a server name or config cannot be overridden safely. `model` and
`mcp_servers` are handled separately; `projects` (trust), `notice`, `tui`,
`history`, `file_opener`, and `desktop` are allowed. Both credential-store
keys (`cli_auth_credentials_store`, `mcp_oauth_credentials_store`) are allowed
only when set to `file`. App-server `model/list` supplies the model's default
reasoning effort; every `turn/start` sends it explicitly. If that default is
absent, both native and Docker warm mode fall back cold and name the missing
default in the refusal. Host `personality` can remain because Codex 0.169 no
longer uses its former styles. The known `js_repl`, `plugins`, and `remote_plugin` features are
reset to their cold defaults (`false`, `true`, and `true`). Host plugin entries
are disabled individually with `plugins.<id>.enabled=false` overrides,
preserving plugin loading and the cold path's account-installed plugins.
Codex's `-c` parser treats quoted path segments as literal key names, so IDs
containing dots or other unsafe characters force a cold turn. Marketplace
tables only define plugin sources and can remain in the host config. Malformed
plugin tables also force a cold turn.
Codex 0.169 deprecates `personality`: its former styles no longer select a
style, so the host key can remain during a warm turn. Docker warm is allowed
by default for other host settings, including unknown future top-level keys;
each unknown key is logged once per agent and reported once as a warning.
Provider/account (`model_provider`, `model_providers`, `profile`, `profiles`,
`chatgpt_base_url`, and non-file credential stores), permission
(`approval_policy`, `sandbox_mode`, `sandbox_workspace_write`,
`shell_environment_policy`), instruction and prompt settings
(`developer_instructions`, `instructions`, instruction files, compaction),
tool settings (`tools`, `web_search`, `notify`, `features.*`, `experimental_*`),
output settings, and telemetry settings (`analytics`, `otel`) are checked
explicitly. Additional known `config.toml` inputs for agents, skills, hooks,
environment context, permissions, model catalogs, and output limits also
force cold turns. A test classifies the Codex source's `ConfigToml` fields;
the app-server schema test rejects newly added, unclassified keys while
allowing older installed Codex versions to omit keys. MCP servers and
plugins are disabled or reset when safe; settings that cannot be overridden
force a cold turn with a reason naming the key. Each classified warm refusal
is reported once per tool and reason class as a warning. The
latest classified warm refusal appears in the agent card's cold tooltip.
The generated private config bytes (or Docker host config bytes) are part of the warm
fingerprint, so a change retires the old process before reuse. A token-only auth callback can receive a newer access token only
from the same account's live `auth.json`; otherwise it gets an error. Fake
app-server tests cover the protocol; live Codex compatibility still needs the
gated canary.
For the first turn of a resumed thread, token usage is the change in thread
totals from the first update's pre-turn baseline, so multiple model requests
in that turn are counted without charging prior history.
Codex warm mode works with a ChatGPT subscription once token auto-refresh is
on (Settings → Server → Accounts & usage), or with an API key; otherwise the agent runs cold. The
eligible `auth.json` is token-only (no refresh token), or holds an API key
without refresh credentials. A full Codex login runs cold and logs
"Codex warm needs token-only credentials" once per credential stamp and
reason; the same check runs before reuse.
The supervisor stamps the native adapter's linked auth file (following its
symlink), or `auth.json` under the Docker launcher's reported `codex_home=`
directory, before reusing a process; a change to that file starts a new process.
The launcher reports `codex_home=` from the running container in its cached
feature probe. A missing answer, failed probe, or timeout keeps that turn
cold with a reason. Docker warm never infers the home from host environment
variables. A changed launcher re-probes and a changed home changes the warm
fingerprint.

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
inherited process environment excludes `AIMEBU_*`. A native normal turn also
carries the person's own MCP servers: the `mcp_servers` of the `config.toml` next to their
`auth.json` are read with a TOML parser (so a server counts however TOML writes it: tables
and sub-tables, dotted keys, an inline table, spaces or quotes in a header) and written into the
private config, and the environment variables those servers name (`env_vars`,
`bearer_token_env_var`, `env_http_headers`) pass through. A server called `aimebu`
is not copied (nor any server that is aimebu's own bus of v1 under another name: one
whose command is the aimebu binary with an `mcp` argument, or that points at
the `/mcp` route on a loopback-style host; the v2 `/api/agent/mcp` route on
port 9997 stays available. A server that only mentions aimebu, such as a filesystem server in
an aimebu checkout, is kept), so a v1 bus
never reaches an agent: aimebu's own entry is the only one under that name (a merge
would mix the two, since Codex merges same-named tables). A config that is not valid TOML
(a table defined twice, an open string) is not read at all, with a line in the supervisor's log
that names the line and column and never the content, so a person's config cannot break a turn;
the rest of the person's config (model, profile, approvals) still does not apply.

The same rules hold for every harness that carries a person's own servers (Codex,
OpenCode, Pi and Vibe: normal turns only, a read-only turn gets none). A Docker turn
carries the person's remote (HTTP or SSE) servers only, because a command on the host is
not in the container (each one left out is logged once per supervisor process and config version with that reason): Codex passes each as
a `-c mcp_servers.<name>=…` override and keeps `--ignore-user-config`, with aimebu's own two entries
last. The Codex path fails closed: a server is carried only when its parsed table holds nothing but a
`url`, `enabled = true` and the two timeouts. Any other key (a header or tools table in any
writing, an unknown key, a command) leaves the whole server out with a log line, never carried in
a weaker form: that covers a server that is switched off (it would be switched on), tool lists,
`required`, and a token variable or headers (a literal header is a credential, and the override sits
on a command line that any process of the host can read; Codex also has no way to hand the container
a variable). For every harness, a url the container cannot use as written is left out: the host
itself in any spelling (`localhost`, loopback or unspecified addresses, numeric forms), user name and
password in the url, a query string or a fragment (likely tokens); and for Codex a server whose
name is not a plain word (Codex reads a quoted `-c` key with its quotes) or is `aimebu_docker`; OpenCode puts them
in the inline config and forwards the `{env:NAME}` variables by name like the provider keys;
Vibe writes them into the private home, with their `api_key_env` variables forwarded; Pi
Docker has no MCP channel at all (`--no-mcp`), so it carries none. A variable a server names is passed to the harness, except one that would
undo the isolation: the tool's own homes and config locations, `AIMEBU*`,
`CLAUDE*`, `XDG_*`, `HOME` and `PATH`, `SSH_AUTH_SOCK` and `GIT_*`, and what injects code
(`LD_*`, `DYLD_*`, `NODE_OPTIONS` and `NODE_PATH`, the interpreters' own option and path
variables, `JAVA_TOOL_OPTIONS`). The adapter's own values are written last. A config that cannot be
read is left out and logged by path, never by content. A variable or a token a
server names is readable by the agent itself, not only by the server: it reaches
the harness process, and so its shell tool, and an OpenCode header token sits in
`OPENCODE_CONFIG_CONTENT`. Carry only what an agent may see. Normal turns use the
approved action-enabled CLI mode; correction and diagnostic turns request
read-only sandboxing and omit the toolbox. Container sandbox verification
failed during the spike, so Codex does not claim `readonly_verified` or
`no_action_mode`. Capabilities tested against Codex CLI 0.162.1 on Linux are
not carried to a new version or OS. Set `AIMEBU_LIVE_CODEX=1` to run the
opt-in two-turn native resume canary.

A v2 turn continues an existing harness session when the agent's session row
has a native session id and `native_session_started_at`: `claude --resume <id>`,
`codex exec resume <id>`, `opencode run -s <id>`, `vibe -p --resume <id>`; Pi passes `--session-id <id>` on
every turn. Claude Code reads v1's transcript from the user's own home.
Codex, Pi, OpenCode and Vibe run with private state per agent, so the
supervisor first copies the selected v1 native session into that state (see
[Start as v2 agent](import-v1.md#start-as-v2-agent)).

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
runtime root, isolated with `PI_CODING_AGENT_DIR`. The `mcpServers` of the
person's own global `mcp.json` are copied into it next to aimebu's entry (a server
called `aimebu` is not copied, so a v1 bus never reaches an agent), the `${NAME}`
variables they name pass through, and the tool allowlist names `codemode` for them
and no `mcp__` entry, so Pi keeps every MCP tool. The project's `.pi/mcp.json` is not
carried over. The installed Ollama Cloud provider extension is loaded explicitly for
the bounded model without loading other user packages. Correction and diagnostic turns
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
owner lists the models of a Pi harness in Settings → Agents → Harnesses & models (Auto-detect models
runs `pi --offline --list-models` on the machine; ids are `provider/model`). Set
`AIMEBU_LIVE_PI=1` or `AIMEBU_LIVE_PI_DOCKER=1` to run the respective
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
call is reported once and is activity; the start of a tool call is not reported, so the supervisor takes a process under
the harness that began after its last output as a tool in flight (native turns only; see the turn limits in
[the supervisor](supervisor.md)). A long silent tool call in a Docker turn can still trip the idle deadline: raise `AIMEBU_TURN_IDLE` for such work. Provider failures are
retried by OpenCode for about 70 seconds without events and then end with one
`error` event; it goes into the exit evidence as `OpenCode error: APIError
(HTTP 429): …`, and wording such as `usage limit`, `usage_limit` or a
weekly, hourly, daily or monthly limit (never a plain `rate limit`) routes into
the provider-limit gate.

Normal turns run with `--auto` (permissions not explicitly denied are
approved, like the Claude and Codex bypass modes) and get the aimebu MCP
toolbox over HTTP through an inline `OPENCODE_CONFIG_CONTENT` with the agent's
private bearer. A native turn also carries the `mcp` entries of the person's own global
`opencode.jsonc` / `opencode.json` there (comments allowed; an entry that reads a
`{file:…}` is left out, and the `{env:NAME}` variables the others name pass through); an
entry called `aimebu` is not copied, since OpenCode deep-merges same-named entries and a
v1 bus would leave keys behind. Project config stays off. Correction and diagnostic turns drop `--auto` and the MCP and
deny `bash`, `edit`, `webfetch`, and `external_directory`, which removes those
tools from the model's tool list. Isolation: the user's data directory is kept
so OpenCode finds its own `auth.json` (aimebu never reads or copies it), but
the agent gets a private `XDG_CONFIG_HOME`, a private session database
(`OPENCODE_DB`) under the supervisor's `opencode` runtime root, and the inline
config with autoupdate and sharing disabled, project config and Claude Code
compatibility files ignored, and the title-generation call turned off. Only a
bounded list of provider API-key variables passes through.
When an imported v1 OpenCode session is continued natively, the supervisor
snapshots the user's database read-only and keeps only the selected session,
its project, and their related rows before placing the result at the agent's
private `OPENCODE_DB`. The source database and its account rows stay untouched.

The owner lists the models of an OpenCode harness in
Settings → Agents → Harnesses & models (Reload models runs `opencode models` and saves the result);
ids must be `provider/model`. During that action the supervisor reads the
machine's OpenCode config and imports usable OpenAI-compatible providers with
environment-referenced or absent keys. The models named in that config and
models returned by each provider's `GET <baseURL>/models` join the launcher
list. An endpoint failure leaves config-listed models available. A provider
with a literal key is skipped with a warning because agent turns use a private
config and cannot read that key. The same sync runs when a harness is first
added by auto-detection, including the Docker variant whose config is read on
the host.
The adapter's capabilities are pinned to the verified release, 1.19.0 on
Linux.

#### Custom providers

An OpenCode harness may have custom providers (Settings → Agents → Harnesses & models): OpenAI-compatible endpoints with a
base URL and the name of the environment variable their key is read from. A turn whose model id starts with
`<id>/` of one carries that provider (`TurnSpec.Providers`: id, name, npm package, base URL, key variable
name, token limits and the harness's models of it), and the adapter adds it to the same private inline
config as `provider.<id>` with `npm`, `name`, `options.baseURL` and the models (the token limits, when set
as a pair, as each model's `limit`). The key is written only as the reference `"apiKey": "{env:NAME}"`,
which OpenCode resolves from its own environment: the key's value is never in the config, the argument
list, a delivery, the database or a log. Natively the variable is passed through by name (with the other
provider keys); in Docker it is added to `HARNESS_DOCKER_FORWARD_ENV` and the launcher forwards it by name.
Where the owner's launcher references another variable for the key (`env_refs`), that variable's value
is what the provider's variable holds. A turn is refused before anything starts, naming the variable, when
the key variable is not set where the turn runs, and also for a package other than
`@ai-sdk/openai-compatible` (OpenCode installs a provider's package at run time), a key variable the
harness uses itself (`PATH`, `OPENCODE_*`, `XDG_*`, …) or the key of one of OpenCode's own providers
(`OPENAI_API_KEY`, …: the host's value of one would be sent to the custom base URL, in Docker and natively alike),
an id with a slash or a URL that is not http(s).
Trust boundary: the refused key-variable names are a denylist of what the harness or its launcher uses itself, which keeps a saved provider
from redirecting the harness; it does not stop whoever may save a provider from naming any other variable of the supervisor's environment
as the key and an endpoint of their choosing as its URL, so the key would be sent there. That is not more than the same person can do with
a launcher's command and environment references, which is why saving providers belongs to the owner, never to an admin or a project role.
A supervisor that does not say `supports_providers` in its hello is never given such a turn: it is held
with a note in the room (see `docs/contract.md`).
The opt-in canary `AIMEBU_LIVE_OPENCODE_PROVIDER=1 go test ./internal/adapter/opencode` runs the same two
real turns (a nonce, then a resumed turn that must return it) on a custom provider the person names with
`AIMEBU_LIVE_OPENCODE_PROVIDER_URL`, `AIMEBU_LIVE_OPENCODE_PROVIDER_MODEL` and the key in the variable named by
`AIMEBU_LIVE_OPENCODE_PROVIDER_KEY_ENV` (default `LIVE_OPENCODE_PROVIDER_KEY`; a name the harness uses itself is refused; unset: the endpoint takes
no key), natively or, with `AIMEBU_LIVE_OPENCODE_PROVIDER_DOCKER=1`, through `opencode-docker`. It spends a little on the
endpoint and is never part of the gate. The config shape and the `{env:NAME}` key were also checked offline
against opencode 1.19.5 and a local stand-in for an OpenAI-compatible endpoint: the request carried the key from
the environment, the provider's model id and a streamed answer, and the turn ended with its final answer.

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
when it inspects such a launcher in a project folder of the machine and reports the answer with the launcher row
(`features`, and `feature_problem` when the probe failed or the launcher is too old). For Docker Codex, a missing
`codex_home=` also appears as `feature_problem` while warm mode is on; it may mean a stopped container or an old
launcher and clears once a later probe finds the home. `GET /api/machines/{id}/launchers` passes the field on and
`aimebu doctor --live` reads it. The row stays available; preflight refuses a turn only when a required feature is
missing. The opt-in live canary
(`AIMEBU_LIVE_OPENCODE_DOCKER=1 go test ./internal/adapter/opencode`) runs two real turns through the
launcher (a nonce, then a `-s` resume that must give it back) and checks the session id and the usage of
each. The provider key comes from the environment (`OLLAMA_API_KEY` for the default model
`ollama-cloud/gemma4:31b`; `AIMEBU_LIVE_OPENCODE_MODEL` names another `provider/model` whose key is among
the forwarded variables), never from the person's home. The Docker variant still claims no capabilities:
read-only turns, cancel and cost inside a container are not verified by it.

An agent turn does not see the person's own OpenCode configuration (the config folder is private and project
config is off), so a provider defined only there, such as a custom OpenAI-compatible endpoint, is unavailable to
it unless the owner adds it to the harness as a custom provider (see above); otherwise only the built-in
providers whose key variables are in the forwarded list can be used.

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
reported once it has finished, so the supervisor takes a process under the harness that began after its last output as a tool in
flight (native turns only; see the turn limits in [the supervisor](supervisor.md)). A long silent tool call in a Docker turn can
still trip the idle deadline: raise `AIMEBU_TURN_IDLE` for such work. API and configuration failures come on stderr as `Error: …` with exit
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
and (for a native turn) `[[mcp_servers]]` tables copied from their own config (`$VIBE_HOME` or `~/.vibe`;
a container gets no server of the person's, only aimebu's endpoint; nothing else of it, so their hooks, skills and agents never reach an agent; a server called `aimebu` is dropped, and
aimebu's own entry comes through `VIBE_MCP_SERVERS`, which Vibe layers above the user and project
files and which replaces a same-named entry) under settings that turn off telemetry, update
checks, auto-update, notifications and connectors, and links the person's `.env` into it (it is never
opened by aimebu). Only a bounded environment passes: `MISTRAL_API_KEY` and the key variables the copied
providers and MCP servers (`api_key_env`) name. Normal turns run with `--trust --yolo` and get the aimebu toolbox through `VIBE_MCP_SERVERS`,
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

The owner lists the models of a Vibe harness in Settings → Agents → Harnesses & models; as Vibe
has no command that lists them, Auto-detect models reads the `[[models]]` of the machine's Vibe config
(by alias) and adds Vibe's built-in default, `mistral-medium-3.5`. A model with no
`display_name` is labelled by its model and provider (`devstral-small-latest (llamacpp)`), not only by the alias.
For a Docker launcher the detect request says so (`containerized`), and a model whose provider's
`api_base` is the machine's own loopback address (`127.0.0.1`, `localhost`, `::1`, `0.0.0.0`) is
listed with `unreachable`, the reason, and cannot be ticked: inside a container that address is the
container, so an agent on it could never reach its model. Add such a model by hand if the container
reaches the provider some other way. The adapter's capabilities are
pinned to the verified release, 2.25.0 on Linux, and claim cost and cancel confirmation only: no-action
mode is not claimed until a live canary has shown that a read-only turn loads none of the person's MCP
servers. `AIMEBU_LIVE_VIBE=1` is reserved for that opt-in canary, which has not been written or run.
The `recorded_*` fixtures in the tests follow the keys and values of real `vibe -p --output streaming` runs and their `meta.json` files (ids and prompt text trimmed); the other fixtures are shaped from the Vibe 2.25.0 sources. (The two-turn Docker canary below, `AIMEBU_LIVE_VIBE_DOCKER=1`, is a different test from the reserved no-action canary above.)

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
The opt-in live canary (`AIMEBU_LIVE_VIBE_DOCKER=1 go test ./internal/adapter/vibe`) runs two real turns
through the launcher (a nonce, then a `--resume` that must give it back) and checks the session id and the
usage of each. It reads `MISTRAL_API_KEY` from the environment only (the test home is a temporary one, so
`~/.vibe/.env` is not consulted) and takes the model alias from `AIMEBU_LIVE_VIBE_MODEL` (default
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
`<checkout>/.aimebu/` for generated state, write `.aimebu/.gitignore` containing `*` when missing, and add `.aimebu/` to the
repository's `info/exclude` (shared by its worktrees, carried by no commit) so an agent's `git add -A` cannot publish that
runtime state. Ordinary `git clean -fd` leaves ignored state alone; `git clean -fdx` can still remove it. The Docker
launcher version probe supplies the catalog version. The owner adds and enables
native or containerized harnesses per machine in Settings → Agents → Harnesses & models; a new
machine has none.

Claude and Codex Docker turns connect to the server's `/api/agent/mcp` HTTP endpoint using the
supervisor's private agent key. Localhost URLs are mapped to
`host.docker.internal` for the container, so the server must listen on a
non-loopback IP with `AIMEBU_BIND`, and `AIMEBU_ALLOW` must admit the Docker
network source. Pi Docker needs the same network access for turn attachments.
A loopback-only bind makes Docker catalog entries unavailable and blocks direct
spawn, switch, and reactivation requests.
The server checks the agent key and exposes the same managed toolbox as stdio
MCP.
Codex's response schema is written below the mirrored checkout, while its
credentials stay in the container's configured Codex home. The adapter checks
that the schema still exists immediately before launch and recreates it
if checkout cleanup removed it after preparation. A forced `git clean -fdx`
or another cleanup of the shared checkout can still remove it during a turn.
Codex Docker turns
pass `exec --ignore-user-config`, retaining those credentials while skipping
the mounted user's `config.toml` and its MCP servers. The only MCP configured
for a normal turn is aimebu's HTTP endpoint. Claude Docker is not strict
(see above): whatever MCP servers the container's Claude configuration has stay available next to aimebu's. On a failed Codex turn, the
last stdout `error` or `turn.failed` event is included with the stderr tail in
recovery evidence after redaction. Docker capabilities
do not claim confirmed cancellation until container-session teardown can be
verified. Set `AIMEBU_LIVE_CC_DOCKER=1` or
`AIMEBU_LIVE_CODEX_DOCKER=1` to run the respective two-turn Docker canary;
the canary checkout is created below this repository's ignored `tmp/`.

`claude-docker` does not forward `CLAUDE_CONFIG_DIR` (it is not among the variables the Claude Docker turn lists in
`HARNESS_DOCKER_FORWARD_ENV`, and `docker exec` carries nothing else of the supervisor's environment): a Claude Docker turn
always uses the container's Claude home, never a per-run or temporary one: the host's own `CLAUDE_CONFIG_DIR`, which harness-docker
mounts at the same path. A launcher that says so (the `claude_home_shared` feature of its probe, recorded on the launcher row)
can therefore continue or copy a session made on the host, and the server allows `resume_session` through it; any other
containerized launcher is refused for both `resume_mode`s (409 `needs_native`, with the reason). The fork canary below runs
native, because it needs a temporary Claude home that the container would not use.

`AIMEBU_LIVE_CLAUDE_FORK=1 go test ./internal/adapter/claude` runs the fork canary: a real first turn on a throwaway
session, then a second that resumes it with `--fork-session`. The copy must report a session id of its own and recall what
the first turn was told, and the original transcript must be byte for byte unchanged. Claude Code runs with the test's
temporary `CLAUDE_CONFIG_DIR`; its login is `ANTHROPIC_API_KEY`, or a copy of the file named by
`AIMEBU_LIVE_CLAUDE_CREDENTIALS` (refused when it holds a refresh token, so that a copy cannot rotate the real login).

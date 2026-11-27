# Answer-robustness eval

The eval measures how reliably each model in the worker pool, run through its real harness adapter, produces a correct turn result **on the
first try**, and what the repair path makes of the misses. A later change to the prompt or to how an answer is submitted is then measured, not
guessed. The code is `internal/eval`.

## Layers

| layer | what runs | cost | where |
|---|---|---|---|
| replay | a **recorded** harness stdout stream goes through the real adapter's parser and `answer.Parse` | free | `make eval-replay`, `make test`, the gate |
| mock | a **canned** model answer or fault (fenced JSON, prose around the JSON, a handoff without an outcome, a crash) is printed by a stand-in harness executable that the real adapter starts | free | same |
| live | the 22 scenarios in `internal/eval/testdata/live` on real models, through the Docker launchers agents run through | subscription quota | `make eval` (`internal/eval/cmd/evalrun`), never in the gate |

The replay and mock layers need no credential and no network. They are ordinary Go tests (no build tag), so `go test ./...` and the gate run
them; `make eval-replay` runs the package alone (`go test -race -count=1 ./internal/eval`), for a CI job or while working on a fixture.
The live runner never belongs in the gate (`make eval` is not part of `test-full` or `gate`). Nothing in this package reads `~/.claude`, `~/.codex`, `~/.pi` or `~/.aimebu*`: the tests run with
the temporary `HOME` of `internal/testenv`, and the stand-in harness is a shell script in a temporary directory.

### How a stream reaches the parser

`eval.Play(ctx, kind, stream, exitCode)` writes a shell script that drains its stdin, prints the stream and exits, and starts the real adapter
(`claude`, `codex` through its Docker variant, `pi`, `opencode`, `vibe`) with that script as the harness command. The adapter builds its
arguments, environment and prompt exactly as it does for the real CLI; what comes out (`Played`: the final-answer event, the exit evidence, the
usage) is what the adapter's parser made of the stream. `Scenario.Verdict` then compares it with the case's expectation, using
`answer.Parse` for the validity.

### What the expectation says

```json
{"id": "s05-pi-preamble", "title": "...", "layer": "replay",
 "output": {"kinds": ["pi"], "stream_file": "internal/eval/testdata/streams/pi_preamble_then_json.jsonl"},
 "expect": {"parse": "repairable", "error_contains": "text around the JSON", "class": "format", "answer_contains": "Let me answer."}}
```

- `parse`: `valid` (the parser accepts the candidate; `strict` says whether the answer must be strict (true) or must not be (false; unset is not checked), `warning_contains` names a warning it must carry),
  `repairable` (the parser rejects it with a reason the repair path sends back; `error_contains` and `class` are required) or `terminal` (the
  harness ended without any answer a repair could fix; class `harness`).
- `class`: `format` (not one JSON object: prose around it, truncated JSON, a bare array that is not an answer), `field` (JSON whose fields break
  the contract: an unknown field, a wrong type, a handoff without an outcome), `behaviour` (a valid answer that does the wrong thing; only a
  scenario check can see it) or `harness` (the CLI crashed, timed out or was limited).
- `answer_contains`: a text the candidate the adapter cut out of the stream must hold; it pins which part of a stream the adapter takes as the
  answer (for example, that the last of two agent messages wins).
- `checks`: predicates on the parsed answer (live scenarios): `messages_count`, `no_messages`, `room_is`, `reply_to_is`, `handoff_outcome`,
  `no_handoff`, `ask_kind`, `ask_options`, `no_attention_controls`, `needs_attention`, `proposed_answers`, `open_questions`, `body_contains`,
  `body_not_contains`, `mentions_id` (a live mention, not one in code formatting), `react_if_silent`, `tool_called` and `tool_not_called`.
  `{worker}`, `{dispatcher}`, `{other_agent}`, `{person}`, `{task_room}` and `{home_room}` stand for the fixed ids of the preset context.
  Every live answer also passes the universal checks: each room is one of the context's rooms, each `reply_to` names a delivered note, each
  `@agt_` mention names an agent of the roster.

## The scorer and the metrics

`eval.Score(raw, scenario, tools)` parses the candidate with `answer.Parse` and applies the checks. It reports: **strict** (valid, no tolerance
warning, and the text is the envelope itself, one JSON object with a `messages` field: no fence, no reasoning block, nothing dropped, and not a lone
message object or a bare array of messages, which the parser reads as the envelope without a warning), **accepted** (valid, which is what the server takes today), **correct**
(valid and every check passes) and the error class. The gap between strict and accepted is what the server forgives today and a stricter way to
submit an answer might not. A handoff without an outcome is judged invalid, as the server holds it for a supervisor that reports
`supports_handoff_outcome` (`internal/store/answers.go`, `handoffOutcomeRequired`).

A live case runs up to `max_turns_to_valid` attempts. `eval.Render` writes the report from `[]eval.Case` (each case is the attempts of one
repetition of one scenario on one model): per model, **first attempt against after repairs** (strict, accepted, correct, valid after repairs,
correct after repairs, each with its 95% Wilson interval), the first-attempt errors by class, time (the median, the mean of the two middle values for an even count) and tokens and cost (first attempts and all),
the model by scenario matrix, and the most common errors. A first-attempt miss that a repair fixes points at the prompt; one that survives the
repairs points at the model or the repair loop; a valid answer that fails a check is a behaviour fault no repair can see.

## Adding a replay fixture from a production stream

1. Take the harness output of the turn: the supervisor's per-agent output log, or the recovery inbox item's evidence. Only a stream the
   adapter saw on stdout is a replay fixture; stderr is not.
2. Redact it. Replace every id (`agt_`, `usr_`, `room_`, `msg_`), name, path, URL and any text of a note with the fixed ids and neutral text of
   `internal/eval` (`WorkerID`, `TaskRoom` ...), and delete every key or bearer token. The adapters redact `aak_`, `amk_`, `as_`, `sk-`, `ghp_`
   and similar tokens in logs, but a fixture is committed: read it line by line. Keep the keys, types and nesting of every event: the fixture is
   evidence of the real shape. Trim long tool results and thinking text to a few words.
3. Put the file in `internal/eval/testdata/streams/<kind>_<what>.jsonl`.
4. Add `internal/eval/testdata/cases/<id>.json` with `layer: replay`, the kind, the file, and what the parser should make of it. Run
   `make eval-replay`. A new odd stream that fails an expectation you believe right is a finding: file it before changing the parser.
5. Streams that already exist in an adapter's `testdata/` are referenced by path, not copied.

## Adding a mock case

A mock case is one JSON file in `internal/eval/testdata/cases/` with `layer: mock`, the harness kinds it applies to, and either a `candidate`
(the text the model answers with; `eval.Wrap` puts it in each harness's stream format) or a `fault` (`crash`: exit 17 after the start;
`silent`: nothing printed). Use it for every shape of near-miss answer: it costs one file and runs against all five adapters.

## Adding a live scenario

A live scenario is `internal/eval/testdata/live/<id>.json`: `input` (`notes` as the supervisor hands them over, an optional `preset` and
`context` keys over it, `allowed_tools`, and `filler_tokens` for a long knowledge block), `expect.checks` and `expect.max_turns_to_valid`.
The scenario must be true to its own words, because a model checks: a scenario about finished work gives the checkout that work
(`input.checkout`: a `commit` message and `files`, committed on top of the base project, a small Go module that builds and passes its test; every
case starts from that base again) and a scenario that quotes or relies on an earlier message (a brief, a thread) gives it in `input.history`
(`no`, `room_id`, `from`, `body`; a number must not repeat a note's). An empty checkout or an empty history makes the agent ask instead of
answer, which reads as a prompt failure and is a fixture one. `internal/eval/live/fixture_test.go` reads, through the stand-in's own tools, what
each corrected scenario shows a model. Add an answer that passes it, and a typical wrong one, to `internal/eval/score_test.go`: the test fails for a scenario with no good answer, so a
check that always passes or never does is found without a model. Some scenarios are contested on purpose and say so in their title (`s15`: an
agent waiting on its own `run_command` ends the turn with no message, though the prompt also says never to end a turn silent while work is owed).

## The live layer (`make eval`)

`internal/eval/cmd/evalrun` runs the live scenarios on real models and writes `tmp/eval/<run>/results.json` and `report.md`. It is the only part
of the eval that spends anything, and it spends **subscription quota**, not API money: it runs the harnesses the way agents run today, through
their Docker launchers, so the logins stay where they live. Nothing here reads or writes `~/.claude`, `~/.codex`, `~/.pi` or `~/.aimebu*`; the
launchers do what they do for an agent.

**Scenarios on some harnesses only.** A scenario with `only_kinds` runs on those harnesses' targets and shows `-` for the others in the report. `s22`
(the unchanged-context marker) is `["claude"]` on purpose: the server sends `{"unchanged":true,...}` instead of the role body and the knowledge
block only to a session whose harness reports compaction, which is Claude alone (`toolReportsCompaction`, `internal/store/context_delivery.go`);
Codex, Pi, OpenCode and Vibe always get the full text, so the marker never reaches them and there is nothing to measure.

**A harness that fails.** A turn that ends with no answer is a `harness` case whose error carries the exit code and, when they are not empty, the
adapter's `error_summary`, its `error_event` (OpenCode and Claude report a failure as an event on stdout and leave stderr empty) and the
stderr tail; a failure that said nothing anywhere says "no output on stdout or stderr".

**Switches.** It refuses to run unless `AIMEBU_EVAL=1` is exported. Without `--full` it is a **smoke**: scenarios `s01` and `s15`, the first target,
one repetition. Run the smoke first. Run `--full` only with the quota switcher on and outside peak hours.

**How a turn is launched.** The runner builds the same `adapter.TurnSpec` as the supervisor: the launcher fields come from
`supervisor.ApplyLauncher` (`internal/supervisor/launcher_spec.go`), which `handleDeliver` calls too, so there is one copy. It then starts the
Docker adapter of the tool (`adapter.DockerPreflight` first). Only Docker launchers are accepted. It does not call `adapter.NewMulti`, so no CLI
is asked for its version.

**The toolbox.** A turn reaches an aimebu toolbox at `/api/agent/mcp`. The runner starts a stand-in on `0.0.0.0` and an ephemeral port (never a fixed
one, never the real server): it lists the scenario's `allowed_tools` with production's own definitions (the description and input schema of each, taken from `agenttools.Definitions`, the list the server's endpoint serves, so the two cannot drift; `TestStandInCatalogIsProductions` compares them for every scenario), answers each call with a canned text, and records the calls each turn made
(the `tool_called` checks read them). `room_history` and `message_get` are the exception to "canned": they answer from the scenario's messages,
which are its input notes (each note is a message of its room) plus its `history`, so a model that looks for the brief, the thread or the
note it was woken with finds it, and a number the scenario does not have is "Message not available", as in the real toolbox. **`submit_answer`.** The stand-in offers it exactly where production does: when the connection's endpoint carries `answer_tool=1`
(`internal/server/agent_mcp.go`), which an adapter adds only for a turn that `adapter.OffersAnswerTool` (OpenCode and Pi at a supported harness version, in the
normal mode; `internal/adapter/answer_tool_version.go`). The version is the launcher row's `version` in the config file, so a row whose version is below the
minimum measures the text-only path, as the same agent would run it. A call is handled by production's own code (`agenttools.Server`, the same
`answer.Parse` check): an answer it refuses comes back as an error the model can correct, and one it accepts is kept. The answer the adapter reports
for the turn (it takes the tool's input when it is valid) is scored as always, and each attempt in `results.json` has `via`: `tool` when it is the
answer the toolbox accepted, `text` when it is the final text. The report counts the two per model.

Each turn has a key of its own: `aak_` and 128 random bits, never derived from the turn id. Every normal turn has a toolbox, since the Docker
adapters refuse one without agent tools access, so a scenario with no tools gets one that offers none, and the turn's `allowed_tools` is the
scenario's list, as the supervisor sets it. A container reaches the stand-in at `host.docker.internal`; set `visible_host` in the config when its
gateway is another address. The stand-in listens on every interface (`0.0.0.0`), because the Docker bridge gateway address is not the same on every
setup and a loopback listener is unreachable from a container; the random per-turn key is what keeps another machine on the network from using it,
and it only ever offers canned replies. Stop a run with nothing else listening on that network if that is not acceptable.

**Sessions and Claude transcripts.** Every case is a first turn in a session of its own (a new UUID for Claude and Pi; the id the harness
reports for the others), never an agent's session. Its repair turn, when there is one, resumes that session. The checkout of a target is
`<scratch_dir>/aimebu-eval-<run id>-<target>`, a fresh git repository, and Claude names the folder of a session's transcript after the working
directory, so every transcript of a run sits in a folder whose name contains `aimebu-eval-<run id>`. The `claude-docker` launcher shares the host's
Claude home (`claude_home_shared`), so those folders are in `<Claude home>/projects/`: delete the ones matching `*aimebu-eval-*` when you are done with
a run. The runner does not delete them, because it never touches that home itself. The Claude adapter the runner starts is marked `StandIn`, so it does not log the Docker variant's "no path map to the container's config dir" (it has no agent session to check a transcript for); in a real session the supervisor logs that line once per session and reason, not on every turn.
Every Git command used to prepare these scratch repositories ignores the host's global and system Git configuration. Each repository's local
`info/exclude` keeps its `.aimebu/` harness state out of scenario commits.

**Repairs.** A reply the answer parser rejects is sent back: the next turn resumes the session with the system note a held reply gets ("Your
previous reply was held: ..."). That is what the server does for Claude; for the other harnesses the server runs a correction-mode turn
instead, which the runner does not reproduce, so their "after repairs" numbers are for the held-reply path. Up to `max_turns_to_valid` turns per
case (2 in every scenario). A valid answer that fails a check is not repaired: the server cannot see it. The harness's own failures (crash,
timeout, a provider limit) end the case and are counted apart, outside every rate.

The report's token columns add total prompt input (uncached input, cache reads
and cache writes) and output for each attempt. A cumulative harness report is
reduced by its previous session total before it enters those columns. Cost
continues to use the harness's reported value or its separate price categories.
Hovering a token total in the Markdown report shows the input split and output.

**Quota and cost.** `usages_url` is a running server whose `GET /api/usages` the runner reads (the repo's own usage data, owner-only) with the cookie
in the environment variable named by `usages_cookie_env`. The run is refused without it. The quota is read before the run, every 10 cases and
once more on every way out of the run (the end, the budget cap, the guard, an unreadable quota, a cancel), with a short context of its own; when
that last reading fails, the last successful one stays in the after column; a weekly window (`weekly*`, `seven_day*`) above `--stop-above` (default 90) percent stops the run, but only a window of a tool one of the run's targets uses (matched by the provider name `/api/usages` reports, chosen by `providers.ProviderFor`: a target's explicit `provider` that `/api/usages` knows wins, then the harness (`claude` is `claude-code`, `codex` is `codex`, `vibe` is `mistral`), then, for `pi` and `opencode`, the `<provider>/` prefix of the target's `model` when that is a provider `/api/usages` knows (`ollama-cloud/glm-5.3` is `ollama-cloud`); any other prefix has no window to read), and of that tool only its **active** profile (a tool with no profile flagged active in `/api/usages` keeps all its profiles); a tool no target uses, and an inactive profile, never stop it (a Claude-only smoke ignores `codex` at 100%). The report carries the
before and after share of every window. `--max-usd` (default 30) stops the run when the cost the harnesses report passes it; for a subscription
that is an equivalent, not a charge. The report keeps the cases finished when a run stops early.

**Config file.** `tmp/eval/config.json` (not committed), for example:

```json
{
  "scratch_dir": "/Users/me/eval-scratch",
  "usages_url": "http://127.0.0.1:9996",
  "usages_cookie_env": "AIMEBU_EVAL_COOKIE",
  "targets": [
    {"name": "claude-sonnet", "tool": "claude", "launcher_id": "lch_claude", "model": "claude-sonnet-5-5"},
    {"name": "codex-sol", "tool": "codex", "launcher_id": "lch_codex", "model": "gpt-6-sol"},
    {"name": "glm-pi", "tool": "pi", "launcher_id": "lch_pi", "model": "my-provider/glm-5.3-flash"},
    {"name": "glm-opencode", "tool": "opencode", "launcher_id": "lch_opencode", "model": "my-provider/glm-5.3-flash",
     "providers": [{"id": "my-provider", "name": "My provider", "npm": "@ai-sdk/openai-compatible", "base_url": "https://llm.example/v1",
                    "key_env": "MY_PROVIDER_KEY", "models": [{"id": "glm-5.3-flash", "name": "GLM 5.3 flash"}]}]}
  ],
  "launchers": [
    {"id": "lch_claude", "tool": "claude", "command": "claude-docker", "args": [], "env_refs": {}, "enabled": true, "containerized": true,
     "path_maps": [], "visible_roots": ["/Users/me"], "supports_toolbox": true,
     "resolved_command": "/usr/local/bin/claude-docker", "version": "x.y.z", "available": true, "features": ["forward_env", "claude_home_shared"]}
  ]
}
```

A target may carry `providers`, the custom providers its model can need (OpenCode only). As the server does for an agent
(`providersForTurn`), only the provider whose `<id>/` prefix the model id has reaches the turn, and none for another tool or a model with no
prefix; OpenCode writes its provider section only when the turn carries one. `key_env` is the name of an environment variable of the machine that
runs the launcher, never a key. Pi reads its models from its own configuration in the container, so a Pi target needs no `providers`.

The scratch directory must be inside a root the launchers mount (their `visible_roots`). To fill a launcher row: the definition fields
(`id`, `tool`, `command`, `args`, `env_refs`, `containerized`, `agent_server_url`, `path_maps`, `visible_roots`, `supports_toolbox`) are in Settings →
Agents → Harnesses & models, or the owner's `GET /api/machines/{id}/launchers`; `resolved_command` is the absolute path of the command
(`command -v claude-docker`); `features` is what the launcher prints for `HARNESS_DOCKER_PRINT_FEATURES=1` (the doctor shows it too). The runner
reads these from the file only, never from the supervisor's own files. Run it from the repository root:

```sh
export AIMEBU_EVAL=1 AIMEBU_EVAL_COOKIE='<the owner session cookie>'
make eval                                          # the smoke on the first target
make eval EVAL_ARGS="--config tmp/eval/config.json --full --reps 3"
```

## What the free layers cannot reach

The repair loop itself, the correction turn and the held-reply note ("Your previous reply was held: ..."), lives in `internal/store`
(`answers.go`, `answers_settle.go`) on top of its database, and a turn's answer is settled there. The eval does not start a server or a store.
The mock layer therefore stops at the adapter and `answer.Parse`: it proves what each adapter makes of a stream and whether the parser would
hold it (`repairable`) or accept it, which is the input of the repair loop. The repair loop's own behaviour is covered by the store's tests.
Measuring how a model behaves when the failure is sent back to it is the live layer: it takes the reason the parser gives and sends it the
way the held-reply note does.

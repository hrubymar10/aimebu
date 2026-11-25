# Answer-robustness eval

The eval measures how reliably each model in the worker pool, run through its real harness adapter, produces a correct turn result **on the
first try**, and what the repair path makes of the misses. A later change to the prompt or to how an answer is submitted is then measured, not
guessed. The code is `internal/eval`.

## Layers

| layer | what runs | cost | where |
|---|---|---|---|
| replay | a **recorded** harness stdout stream goes through the real adapter's parser and `answer.Parse` | free | `make eval-replay`, `make test`, the gate |
| mock | a **canned** model answer or fault (fenced JSON, prose around the JSON, a handoff without an outcome, a crash) is printed by a stand-in harness executable that the real adapter starts | free | same |
| live | the 22 scenarios in `internal/eval/testdata/live` on real models, through the real harnesses | paid | not built yet (phase 2b) |

The replay and mock layers need no credential and no network. They are ordinary Go tests (no build tag), so `go test ./...` and the gate run
them; `make eval-replay` runs the package alone (`go test -race -count=1 ./internal/eval`), for a CI job or while working on a fixture.
The live runner never belongs in the gate. Nothing in this package reads `~/.claude`, `~/.codex`, `~/.pi` or `~/.aimebu*`: the tests run with
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

- `parse`: `valid` (the parser accepts the candidate; `strict` says with no tolerance warning, `warning_contains` names a warning it must carry),
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
warning: no fence, no reasoning block, no lone object, nothing dropped), **accepted** (valid, which is what the server takes today), **correct**
(valid and every check passes) and the error class. The gap between strict and accepted is what the server forgives today and a stricter way to
submit an answer might not. A handoff without an outcome is judged invalid, as the server holds it for a supervisor that reports
`supports_handoff_outcome` (`internal/store/answers.go`, `handoffOutcomeRequired`).

A live case runs up to `max_turns_to_valid` attempts. `eval.Render` writes the report from `[]eval.Case` (each case is the attempts of one
repetition of one scenario on one model): per model, **first attempt against after repairs** (strict, accepted, correct, valid after repairs,
correct after repairs, each with its 95% Wilson interval), the first-attempt errors by class, time and tokens and cost (first attempts and all),
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
Add an answer that passes it, and a typical wrong one, to `internal/eval/score_test.go`: the test fails for a scenario with no good answer, so a
check that always passes or never does is found without a model. Some scenarios are contested on purpose and say so in their title (`s15`: an
agent waiting on its own `run_command` ends the turn with no message, though the prompt also says never to end a turn silent while work is owed).

## What the free layers cannot reach

The repair loop itself, the correction turn and the held-reply note ("Your previous reply was held: ..."), lives in `internal/store`
(`answers.go`, `answers_settle.go`) on top of its database, and a turn's answer is settled there. The eval does not start a server or a store.
The mock layer therefore stops at the adapter and `answer.Parse`: it proves what each adapter makes of a stream and whether the parser would
hold it (`repairable`) or accept it, which is the input of the repair loop. The repair loop's own behaviour is covered by the store's tests.
Measuring how a model behaves when the failure is sent back to it is the live layer: it takes the reason the parser gives and sends it the
way the held-reply note does.

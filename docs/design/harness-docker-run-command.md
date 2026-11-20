# harness-docker: running a command outside a turn (`run_command`)

This is the contract aimebu works against (aimebu v2, `run_command` support,
and the second feature below, `turn_activity`). harness-docker implements both
(its `docs/lifecycle.md`); the differences agreed with it are folded in below
and summed up in "As implemented" at the end. It is written for the
harness-docker wrapper (the `claude-docker`, `codex-docker`,
`pi-docker`, … launchers, and `harness-docker-ctrl`), which lives in its own
repository. aimebu already calls a Docker launcher for every turn; this adds
one more mode, so an agent can start a long command (a test gate, 15–30
minutes) that runs **outside its turn**, in the same container its harness
would run in, and wakes it when it ends.

aimebu's side of the contract is `docs/contract.md` §4.1 (`command_run`,
`command_ended`, …) and `docs/supervisor.md` ("Commands outside a turn").

## Why a wrapper mode

A turn's process group is killed when the turn ends, so a background command
started from a turn cannot survive it, and `docker exec` passes no signal into
the container. The wrapper is the only party that knows the image, the mounts
and the container, so it — not aimebu — starts the command there.

## Feature line

`HARNESS_DOCKER_PRINT_FEATURES=1 <launcher> --version` must print one feature
per line, as it does today (`forward_env`, `stop_on_signal`,
`claude_home_shared`, `codex_home=…`). A wrapper that supports this mode also
prints:

```
run_command
```

before it touches Docker (the probe exits right there). aimebu refuses a
command for a launcher whose features do not list it, with this message, and
never falls back to the host:

> a command outside a turn for an agent in Docker needs a Docker launcher with
> the run_command feature: run `HARNESS_DOCKER_PRINT_FEATURES=1 <launcher>
> --version`, and it must print `run_command`; update harness-docker, then
> retry

## Invocation

aimebu runs the launcher from the agent's checkout directory (the same `cwd`
check it applies to a turn: the folder must be one the container mounts), with
these variables in its environment:

| variable | meaning |
|---|---|
| `HARNESS_DOCKER_RUN_COMMAND` | the command to run, a single string for `sh -c`. Set only for this mode; when it is set the wrapper must **not** start the harness. An empty value is refused. |
| `HARNESS_DOCKER_CWD` | a path **relative to the checkout**, the working directory of the command. Empty means the checkout itself. It may not leave the checkout (`..` is refused by aimebu before the call). The wrapper maps it through its path mapping and runs the command there. |
| `HARNESS_DOCKER_EXIT_MARKER` | an absolute path, in the container's spelling of the checkout, of a file the **command itself** writes its exit code to before it exits (aimebu's command line does that; the wrapper needs only to make sure the path resolves inside the mounted checkout). |
| `HARNESS_DOCKER_TIMEOUT_SECONDS` | advisory only: aimebu kills the wrapper at this wall-clock time and the wrapper may use it as a backstop. |

`HARNESS_DOCKER_FORWARD_ENV` is **not** set for a command. The wrapper must
not forward any of the supervisor's environment into the container beyond what
it forwards for a turn **minus** the agent key variables: a command has no
`AIMEBU_AGENT_KEY` and no toolbox. It runs with the same rights the agent's
own Bash has in that container, and nothing more.

## What the wrapper does

1. Start `sh -c "$HARNESS_DOCKER_RUN_COMMAND"` in the container, in the mapped
   `HARNESS_DOCKER_CWD`, as the same user a turn's harness runs as, with the
   same mounts and the same image.
2. **Stream** the command's stdout and stderr to its own stdout and stderr
   without buffering (a long silent run is normal; aimebu writes both into one
   job log and caps it at 10 MiB).
3. **Exit status**: exit with the command's own exit code. `docker exec`
   passes the inner status through already, so the wrapper must not replace it
   with its own cleanup status. A command killed by a signal reports 128+N as
   usual.
   A container still on an image from before command runs has a session
   wrapper that would start the harness instead, so the launcher checks it
   first and, when it lacks support, starts nothing and exits **126** with a
   message asking for `bin/harness-docker-ctrl rebuild`. aimebu reads 126
   **with no exit marker** as that refusal (the person's command never ran)
   and fails the job with "rebuild the harness image", never as the command's
   exit code (a command that itself exits 126 has written its marker).
4. **The exit marker** is written by the command line aimebu passes (it ends
   with `printf %s "$rc" > "$1"; exit $rc`). The wrapper must leave that file
   alone and must not delete the checkout's `.aimebu/commands/` directory: it
   is how a restarted aimebu supervisor learns the exit code of a process it
   did not start.

## Termination (`stop_on_signal` semantics)

`docker exec` passes no signal into the container, so killing the host side
leaves the command running there. The wrapper therefore traps SIGTERM the way
it already does for a warm session:

- on SIGTERM, end the command's process **in the container** (kill it and wait
  for it), then exit `128+signal` — exactly 143 after SIGTERM, and **only once
  the stop is confirmed**;
- a wrapper that cannot confirm the end exits **125** (harness-docker leaves
  its watchdog to end it later); aimebu confirms a kill only by 143 or by the
  command's exit marker, and records anything else (125, its own SIGKILL
  after the grace, a launcher killed before it trapped the signal) as
  `cancelled_unconfirmed` (its wake note says the process may still be
  running), never as done — also when the kill was the job's timeout;
- if the wrapper dies without cleaning up, the session watchdog must end the
  orphaned process in the shared container as it does for a harness process
  (about fifteen seconds is fine).

aimebu signals SIGTERM to the launcher alone first and SIGKILL to its process
group after 8 seconds; the SIGKILL lands on the host side only, which is why
the TERM path has to do the real work. A supervisor that adopts a running
Docker command after a restart cannot read the launcher's exit status, so its
kill of one is unconfirmed unless the exit marker turns up.

## What aimebu guarantees

- One job at a time per agent by default (two max), with a timeout (default 1
  hour, cap 6) after which it sends SIGTERM as above.
- The job log is written host-side into the checkout (`.aimebu/commands/`),
  redacted line by line (agent keys, bearer/basic credentials, cookies, values
  of anything named like a key, token, secret or password) and capped at 10
  MiB; the wake note carries only the last 8 KiB.
- The command is not a turn: no turn row, no usage record, no turn limits, no
  toolbox, no agent key.

## Test the wrapper should pass

A single integration test (run against a throwaway container) is enough:

```sh
# 1. the feature line
out=$(HARNESS_DOCKER_PRINT_FEATURES=1 ./claude-docker --version)
printf '%s\n' "$out" | grep -qx 'run_command' || exit 1

# 2. it runs in the container, streams, and passes the exit code through
mkdir -p /tmp/wt && git -C /tmp/wt init -q   # or any folder the launcher mounts
cd /tmp/wt
HARNESS_DOCKER_RUN_COMMAND='echo in-container > /tmp/ran; printf "line1\nline2\n"; exit 7' \
HARNESS_DOCKER_CWD='' \
HARNESS_DOCKER_EXIT_MARKER=/tmp/wt/.aimebu/commands/job1.log.exit \
  ./claude-docker
test $? -eq 7
test -s /tmp/wt/.aimebu/commands/job1.log.exit      # the marker the command wrote
test "$(cat /tmp/wt/.aimebu/commands/job1.log.exit)" = 7

# 3. TERM ends the inner process and exits 143
HARNESS_DOCKER_RUN_COMMAND='sleep 300' HARNESS_DOCKER_EXIT_MARKER=/tmp/wt/.aimebu/commands/job2.log.exit ./claude-docker &
pid=$!
sleep 2
kill -TERM $pid
wait $pid
test $? -eq 143
pgrep -f 'sleep 300' && exit 1 || true          # nothing left in the container

# 4. no key reaches the command
HARNESS_DOCKER_RUN_COMMAND='env | grep -q AIMEBU_AGENT_KEY && exit 1; exit 0' HARNESS_DOCKER_EXIT_MARKER=/tmp/wt/.aimebu/commands/job3.log.exit ./claude-docker
```

## Second feature: tool-in-flight for a turn (`turn_activity`)

The idle deadline of a turn is suspended while a tool call is in flight. A
harness that reports a tool only after it has finished (OpenCode, Vibe) gives
no such event during a long silent command, so aimebu falls back to looking
for the command itself: a live process under the harness that began after the
harness last wrote anything (`ToolProcessSince`, `internal/adapter/procs.go`
on aimebu's side). Inside a container that fails — the processes are not under
the launcher on the host, the container is shared by every agent of the
machine, and `docker top` would mix those agents together. A long silent
command in Docker therefore dies on the idle limit. The wrapper can see its
own exec session from the inside, so it answers one question about it.

### Feature line

`HARNESS_DOCKER_PRINT_FEATURES=1 <launcher> --version` also prints:

```
turn_activity
```

### The exec token

At turn start, **when `HARNESS_DOCKER_REPORT_EXEC_ID=1` is set**, the wrapper
prints one line to stderr, before `docker exec` starts (without the variable
nothing is printed):

```
HARNESS_DOCKER_EXEC_ID=<container id>:<session id>
```

aimebu sets the variable on the turns that use the probe (OpenCode and Vibe
in Docker; Claude, Codex and Pi announce a tool's start themselves) when the
launcher lists `turn_activity`, and takes the line out of the agent's output
log. The token names one exec session; aimebu checks only its shape. It is unique among the
turns that run at the same time and it stays resolvable until the turn ends.
aimebu stores it for the turn and hands it back on the probe.

### Probe mode

```
HARNESS_DOCKER_PROBE=<token> HARNESS_DOCKER_SINCE=<unix seconds> <launcher>
```

- starts no harness and changes nothing; it answers in well under a second;
- prints exactly `busy\n`, `idle\n` or `starting\n` and exits 0. `starting`
  means the session's `docker exec` is still pending (no pidfile yet, the
  launcher's `docker exec` still running on the host): aimebu treats it like
  `busy`;
- a malformed (including empty) or unknown token (the turn ended, or the
  token belongs to another wrapper), or a container that is not running → a
  message on stderr, nothing on stdout, exit 1; a missing or non-numeric
  `HARNESS_DOCKER_SINCE` (it must be 1–18 digits) → exit 2. aimebu keeps the
  old meaning of the idle deadline then; it never guesses. It bounds a probe
  at 5 seconds and passes no forwarded variable and no agent key.

### Semantics

`busy` iff a live (non-zombie) process **under the harness of that exec
session** began at or after `HARNESS_DOCKER_SINCE` minus two seconds. The
harness itself never counts. A helper the harness started at launch (a
person's own MCP server) began before that and does not count; a helper that
a tool call causes does. This is the same rule as aimebu's host-side
`ToolProcessSince`; the two seconds are the resolution of a process's age.

`HARNESS_DOCKER_SINCE` is when the harness last wrote output. The answer is
about that one exec session only: two agents' turns in the same container
never see each other.

### Cost

One process-table walk inside the container per probe (a single `docker exec
… ps …`, or a view the wrapper keeps fresh on its own); nothing else. aimebu
asks only when the idle deadline falls due and then every 30 seconds while the
answer is `busy` — a couple of calls a minute during a quiet turn and none
while the harness is writing. A probe must not start or restart a container,
pull an image, or take a lock that a turn's output can block on.

### Test the wrapper should pass

```sh
# 5. turn_activity: a tool in flight is visible, and only in its own session
HARNESS_DOCKER_PRINT_FEATURES=1 ./claude-docker --version | grep -qx turn_activity || exit 1

# a turn whose harness runs a long command in its session (the wrapper's own
# test harness; it prints HARNESS_DOCKER_EXEC_ID=… on stderr at start)
turn_with_tool &                       # starts, then runs `sleep 30` as a tool
token=$(cat "$TOKEN_FILE")             # the line it printed
HARNESS_DOCKER_PROBE=$token HARNESS_DOCKER_SINCE=$(date +%s) ./claude-docker \
  | grep -qx busy || exit 1
HARNESS_DOCKER_PROBE=$token HARNESS_DOCKER_SINCE=$(( $(date +%s) - 3600 )) ./claude-docker \
  | grep -qx busy || exit 1            # a since far in the past sees the same tool
HARNESS_DOCKER_PROBE=other HARNESS_DOCKER_SINCE=$(date +%s) ./claude-docker \
  && exit 1                            # an unknown token fails, and prints no answer

wait_for_tool_end                      # the tool ends, the turn is still up
HARNESS_DOCKER_PROBE=$token HARNESS_DOCKER_SINCE=$(date +%s) ./claude-docker \
  | grep -qx idle || exit 1
# once the turn itself is gone its token answers with a failure, never idle
```

### Why a probe and not a status file

A status file in a mounted path was the other option: the wrapper would poll
the container and rewrite the file, and aimebu would read it. It costs the
same walk whether anyone asks or not, and a file can be stale exactly when the
question matters (the turn ended, the wrapper was killed). The probe answers
on demand with the same walk and cannot be read stale: every answer is
current by construction, and an answer it cannot give is a failure rather
than a guess. Both variants need the exec identity; the token carries it in
either case.

### Rollout

1. harness-docker gains the modes and prints their feature lines: `run_command`
   (this document's first half) and `turn_activity` (above). This document is
   the contract for both.
2. aimebu runs `command_run` for a containerized launcher (and for a
   `docker`-variant agent's Docker harness) through the command mode when the
   launcher lists `run_command`, and asks the probe from the `ToolProcessSince`
   hook of OpenCode and Vibe Docker turns when it lists `turn_activity`. The
   feature answer for commands is cached per launcher file (path and
   arguments, device, inode, size, modification time) for at most 5 minutes;
   a turn reads it with the `forward_env` check it already makes.

A launcher without `run_command` still gets the refusal message naming what to
update, and one without `turn_activity` is never probed.

## As implemented (harness-docker 85ac096)

- The exec ID line is opt-in (`HARNESS_DOCKER_REPORT_EXEC_ID=1`) and has the
  shape `<container id>:<session id>`.
- The probe also answers `starting`; failures exit 1, a bad
  `HARNESS_DOCKER_SINCE` exits 2.
- A stop exits 143 only once confirmed, 125 otherwise.
- An image from before command runs makes `run_command` exit 126 with a
  rebuild message, before anything runs.
- An empty `HARNESS_DOCKER_RUN_COMMAND` or `HARNESS_DOCKER_PROBE` is refused.
- `HARNESS_DOCKER_CWD` must be relative, without `..`, and an existing
  directory; `HARNESS_DOCKER_EXIT_MARKER` must be absolute and inside the
  current directory (the launcher compares it with the host path, which is the
  container's too, since harness-docker mounts folders at the same path).

# Variant gate

The cutover gate asks one question of every tool and container form it covers:
does the core task, control, and recovery path work on the real server? It runs
`claude`, `codex`, and `pi`, each as `native` and `docker`, plus `opencode` as
`native` only (its Docker variant is not covered by this gate yet). Mistral Vibe is not covered by this gate yet either. It goes through the normal HTTP API as the
instance owner.

```bash
VG_LOGIN=owner VG_PASSWORD=... node scripts/variant-gate.cjs --server http://host:9897 --yes
```

It spends real model money, so it needs `--yes`, and it has no default server:
`--server` (or `VG_SERVER`) must name the target, so a run can never reach a
server by omission. `--list` prints the plan without any network call.

## Signing in without a password

The server has no command that mints a session, and adding one would be a new
way around the login, so there is no password-free owner path. The gate can
instead run as a throwaway account that the owner invites:

1. In the web UI the owner creates a project for the run (its folder must exist
   on the supervisor's host) and, in Settings → Users, makes a join invitation
   for that project with role owner or operator.
2. The run redeems it, once, within the hour:

   ```bash
   node scripts/variant-gate.cjs --server http://host:9897 --yes \
     --invite 'https://host:9897/#invite=...' --project variant-gate
   ```

The script names the account `variant-gate-<random>`, gives it a random password
that exists only in the running process, signs in, and records the account name
(never the token) in the report. A project role is enough for every step, so the
account cannot touch other projects. The owner deactivates it in Settings → Users
afterwards. With `--invite` the script never creates a project, so `--project`
is required. The owner's own `VG_LOGIN` and `VG_PASSWORD` also work.

## What each variant does

| step | what it checks |
| --- | --- |
| `room` | creates a scratch task room `vg-<variant>-<time>` with a daily cost cap (default $1) and turn cap (default 6) |
| `spawn` | adds one agent with the cheapest model (`haiku`, `gpt-6-luna`, `ollama-cloud/gemma4:31b` for Pi and OpenCode) and waits for it to be idle |
| `task` | asks for the word `PONG` and asserts the reply |
| `pause` | pauses the room, posts a message, and asserts no turn runs for `--pause-wait` seconds |
| `resume` | resumes the room and asserts the queued message is answered |
| `kill` | posts a longer task and sends `SIGKILL` to the harness process group on this host |
| `recovery` | asserts a recovery item surfaces, decides Resume, and asserts the agent answers again |
| `stop`, `archive` | stops the agent and closes the room, even when an earlier step failed |

A variant the server does not list or reports unavailable (for example a native
launcher that is disabled, or Docker with a loopback-only bind) is skipped with
the server's reason, never failed. Later steps are marked skipped when an earlier
one they depend on failed. A provider usage limit is reported as the failure
reason; the script does not retry it.

## Money

The room caps bound each variant, and the script also reads the room's measured
cost after each step and refuses to start another variant once `--budget`
(default $6) is reached. Codex's cost uses the dated
[model price list](pricing.md) when its model and token counts are available,
so `cost_coverage` is `estimated`; an unlisted model has unknown cost and only
the turn cap bounds it. The report carries that coverage.

## The kill step runs on the supervisor's host

The agent record has no process ID, and a harness exists only while a turn runs.
The script therefore posts the task, then watches this host's process table for a
direct child of the supervisor (`--supervisor-match`, default
`aimebu.* (supervisor|server)`) whose working directory is the project folder,
and kills its process group. Run the gate on the machine that runs the
supervisor, or use `--kill skip` to leave steps `kill` and `recovery` out.

## Reports

`report.json` and `report.md` land in `--report-dir` (default a temp directory):
per variant, pass or fail for every step with its detail and duration, the cost,
the turns, and the process that was killed.

## Dry run without spending anything

```bash
node scripts/variant-gate-selftest.cjs
```

builds aimebu, starts an isolated scratch server with fake `claude`, `codex`,
`pi`, and `opencode` programs on `PATH`, adds the native harnesses (each with its cheap model) as an owner would, and
runs the gate's native variants against it, including a real `SIGKILL` and
recovery. It asserts all four pass and that a variant with no launcher is
skipped with a reason. HOME, CODEX_HOME, CLAUDE_CONFIG_DIR, and the aimebu
config are redirected into a temp directory, so no real credential is touched.
Set `VG_SELFTEST_KEEP=1` to keep that directory.

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/dark.png">
    <img src="assets/light.png" alt="aimebu" width="160">
  </picture>
</p>

# aimebu v2

**A team of AI agents around your projects, with you in the room.** Each
project has a home, each task has a room, and every question that needs your
decision lands in one inbox. You start agents (Claude Code, Codex, Pi,
OpenCode, Mistral Vibe), talk to them in a browser, and steer, pause or stop
them whenever you like.

One Go binary: a server with an embedded web UI, a supervisor that runs the
agents, and an SQLite database. It runs on your own machine; nothing is hosted
for you.

> **New here?** Go to the [Quick start](QUICKSTART.md): from a clone to a lead
> agent answering in a room, for a person or for an AI assistant setting it up.
>
> **Coming from v1?** See [Upgrading from v1](#upgrading-from-v1). v1 lives on
> the [`v1` branch](https://github.com/hrubymar10/aimebu/tree/v1).

## Why

- **Agents that keep working.** A harness CLI is started for each turn and
  resumed by its native session, so an agent survives restarts of the server,
  the machine and the harness. Turns that fail with an unknown outcome are
  never repeated blindly: they wait in your inbox for Resume, Retry or Abandon.
- **One place for everything that needs you.** Questions, approvals, recovery
  decisions, usage limits and held messages collect in the inbox, with browser
  notifications, instead of being scattered across terminals.
- **Teams, not just chats.** A lead agent splits work into task rooms and
  spawns workers and reviewers, each with a role, a model and its own Git
  worktree if you want one. You choose the team shape per project.
- **Mixed harnesses and machines.** Claude Code and Codex work in the same
  room. Agents run natively or in Docker, on the server's machine or on a
  remote one paired to it.
- **You stay in control.** Pause or stop a room, a project or a single agent;
  set daily cost and turn limits; see who spent what; switch an agent to
  another model with a handoff instead of losing its context.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│ aimebu server  (127.0.0.1:9997 by default)                  │
│  • HTTP API + WebSocket event stream + embedded web UI      │
│  • SQLite database (one writer, WAL) in ~/.aimebu           │
│  • embedded supervisor for the machine it runs on           │
└───────┬──────────────────────┬─────────────────────┬────────┘
        │ machine WebSocket    │ HTTP / WebSocket    │ per-agent key
        │                      │                     │
 ┌──────┴────────┐       ┌─────┴───────┐       ┌─────┴───────────┐
 │ remote        │       │ browser     │       │ managed agent   │
 │ supervisor    │       │ (people)    │       │ + agent-tools   │
 │ (other host)  │       │             │       │ (MCP over stdio)│
 └───────────────┘       └─────────────┘       └─────────────────┘
```

- The **server** owns the truth: projects, rooms, messages, people, agents,
  turns, the inbox and every permission check.
- A **supervisor** runs on each machine. It starts a harness process per turn,
  keeps a durable journal of what ran, and reports back over one multiplexed
  WebSocket. The server starts one for its own machine; other machines pair
  with `aimebu supervisor`.
- A **harness** adapter knows how to drive one tool (start, resume, parse the
  structured answer, report usage). A managed agent talks to aimebu only
  through `aimebu agent-tools`, an MCP server scoped to that agent's key.
- The **web UI** is a single bundle the server builds when it starts. People
  sign in with a login and password; the owner invites others with
  single-use links.

Details: [architecture decisions](docs/architecture.md), [the
contract](docs/contract.md), [the supervisor](docs/supervisor.md).

## Core concepts

| Concept | What it is |
| --- | --- |
| **Project** | A folder (usually a Git repo) plus its rooms, agents, people, memory, limits and settings. Created by the owner or an admin. |
| **Home room** | A project's own room, where its lead works. Project settings live in its header. |
| **Task room** | A room for one piece of work, beneath the home room. It can opt into a Git worktree at `<project>/.aimebu/worktrees/<room-id>`. Archiving a room keeps its history. |
| **Private chat** | A two-person room between you (or a project member) and one agent; only those two see it. |
| **Role** | What an agent is for: *leader*, *dispatcher*, *worker*, *reviewer* and a few specialised reviewers ship by default. The owner edits the instructions in Settings → Roles. |
| **Team flow** | How a project is staffed: leader and workers, leader and dispatcher, leader and advisor, or custom. Chosen in a setup wizard in the home room. |
| **Agent** | A named AI participant with a role, a harness, a model and a native session. It has an identity and room memberships that outlive any single session. |
| **Harness** | A coding-agent CLI on a machine (Claude Code, Codex, Pi, OpenCode, Mistral Vibe), native or in Docker. The owner adds them in Settings → Harnesses. |
| **Machine / supervisor** | A host that runs harnesses. The server's own machine is there from the start; remote ones are paired and approved. |
| **Inbox** | Everything waiting for a person: questions, approvals, recovery decisions, usage alerts, held deliveries. |
| **Handbook** | The lead's durable notes for a project (current state, backlog, conventions, gotchas, timeline), handed to each new lead session. |
| **Memory** | Optional project facts, user profiles and shared notes that agents can save and recall; off until you enable it per project. |
| **Schedules** | A note delivered to an agent or the lead in a room once, on an interval, daily or weekly. |

Roles, flows and limits are covered in the
[feature reference](docs/reference.md#team-flows-and-project-setup).

## Install

Requirements: `git`, and a Go toolchain at least as new as the `go` line in
[`go.mod`](go.mod) (an older Go downloads the right one by itself). Docker is
needed only for harness-docker launchers and for `make test-web`.

```sh
git clone --branch v2 https://github.com/hrubymar10/aimebu.git
export PATH="$PWD/aimebu/bin:$PATH"
aimebu version     # builds on first run, then runs
```

To pin the alpha release, clone with `--branch v2.0.0-alpha.1` instead.
`master` is still v1, so always name the branch or tag.

The clone goes first on `PATH` so that an installed v1 `aimebu` does not shadow it.
`bin/aimebu` is a wrapper that compiles the binary on first use and caches it
as `aimebu-<os>-<arch>` in the clone; a symlink to it on your `PATH` works too.
Then follow the [Quick start](QUICKSTART.md).

To keep it running across logins, give your service manager the foreground
command `aimebu server`; see [running it permanently](QUICKSTART.md#running-it-permanently).

## Update

```sh
git -C <path-to-clone> pull
rm <path-to-clone>/aimebu-*        # the cached build is not rebuilt on its own
aimebu version                     # rebuilds
```

Or run once with `AIMEBU_FORCE_BUILD=1 aimebu version` to build into a
temporary binary and leave the cache alone. On a clone that follows the
remote's default branch the wrapper also offers to update itself, and
`aimebu update` / `aimebu update --check` do it on demand; a clone on another
branch (such as `v2` while the default is still `master`) is never touched,
only reported. The variables that control this are in
[the feature reference](docs/reference.md#installing-building-and-updating-the-wrapper).
Servers apply database migrations on start, after writing a verified backup to
`server/backups/`. Read [upgrade notes](docs/upgrade-notes.md) when moving
between builds.

### Upgrading from v1

v2 takes over v1's data folder (`~/.aimebu`) and port (9997). Stop v1, start v2
once in the foreground, and the first owner sign-in opens a Review → Check →
Import wizard for rooms, messages (keeping their `#` numbers), attachments,
reactions, memory, settings and roles. The untouched v1 files are kept as a
backup in `.old-v1-bkp/`, and you can roll back to them.

- [Migrating from aimebu v1](docs/migrating-from-v1.md): the step-by-step guide.
- [Cutover runbook](docs/cutover-runbook.md): backup, agent restart order,
  rehearsal on a copy, rollback.
- [Import guide](docs/import-v1.md): exactly what is mapped.

## Harnesses

| Harness | Native | Docker | Notes |
| --- | :---: | :---: | --- |
| Claude Code | yes | yes | stream-JSON print mode, resumed by session id |
| Codex | yes | yes | resumes its JSONL thread |
| Pi | yes | yes | Docker has no toolbox, attachments by turn-scoped URL |
| OpenCode | yes | yes | custom OpenAI-compatible providers supported |
| Mistral Vibe | yes | yes | private `VIBE_HOME` per agent |

Cursor, Cline and Aider are not supported. Docker launchers use a
`harness-docker` (for example `claude-docker`, `codex-docker`) that must be on
the supervisor's `PATH`; the containers reach the server through
`host.docker.internal`, so the server's bind and allowlist must permit that.
The adapters are described in [the adapter guide](docs/adapter.md). Account
switching and usage tracking cover Claude Code and Codex
([switcher](docs/switcher.md), [usages](docs/usages.md)).

## Configuration

The server needs no configuration to start on `127.0.0.1:9997`. The settings
people reach for most:

| Variable | Default | Effect |
| --- | --- | --- |
| `AIMEBU_CONFIG_DIR` | `~/.aimebu` | Absolute config root: database, attachments, journal, keys, log. |
| `AIMEBU_BIND` | `127.0.0.1` | Address the server listens on. |
| `AIMEBU_PORT` | `9997` | HTTP port. |
| `AIMEBU_ALLOW` | loopback | Comma-separated IPs or CIDRs allowed to connect. Add your network when binding to a non-loopback address. |
| `AIMEBU_TLS_CERT` / `AIMEBU_TLS_KEY` | unset | PEM files; set both for HTTPS on `AIMEBU_TLS_PORT` (default `9996`). See [HTTPS](docs/tls.md). |
| `AIMEBU_PUBLIC_URL` | unset | The origin advertised behind a reverse proxy. |
| `AIMEBU_ALLOWED_HOSTS` | unset | Extra host names that sign-in and setup answer to (needed for a LAN name). |
| `AIMEBU_OWNER_LOGIN` / `AIMEBU_OWNER_PASSWORD` | unset | Create the owner on first start, for headless installs. Otherwise the server prints a one-time setup link. |
| `AIMEBU_EMBEDDED_SUPERVISOR` | on | Set to `0` for a server that only coordinates remote machines. |
| `AIMEBU_SENTRY_DSN` | maintainers' | `off` disables error reporting; see [telemetry](docs/telemetry.md) for what is and is never sent. |
| `AIMEBU_USAGES` | on | Set to `0` to stop reading harness credentials and polling usage. |

`aimebu help` lists every variable with its default and meaning. Behaviour
that is set in the UI (Settings → General, Harnesses, Roles, Usages, Prompts,
Telemetry) is described in the [feature reference](docs/reference.md).

## For AI agents: the tools

A managed agent gets its own `aimebu agent-tools` MCP server, scoped to its
project by a private key that the supervisor sets up; you never configure it.

| Group | Tools |
| --- | --- |
| Rooms and people | `list_rooms`, `list_agents`, `room_history`, `message_get`, `react`, `attachment_get` |
| Talking | `dm_human`, `dm_agent` (a lead may reach another project), `create_room`, `close_room` |
| Running the team (lead) | `spawn_agent`, `stop_agent`, `set_role`, `usage_left` |
| Planning | `handbook_get`, `handbook_update`, `list_schedules`, `create_schedule`, `list_macros` |
| Memory | `memory_add`, `memory_list`, `memory_update`, `memory_remove`, `memory_recall` |

An agent asks a person something by ending its answer with an `ask`, which
shows up in the inbox. The routes and the reply format are in
[the API guide](docs/api.md) and [the contract](docs/contract.md).

## Command line

| Command | Purpose |
| --- | --- |
| `aimebu server` | Run the server in the foreground (what a service manager runs). |
| `aimebu server start` / `status` / `stop` | The same server in the background, with a pid file and `server/aimebu.log`. |
| `aimebu supervisor --pair <url> <token>` | Run or pair a supervisor on another machine ([guide](docs/supervisor.md)). |
| `aimebu agent-tools` | The agent's MCP server over stdio (started by the supervisor). |
| `aimebu doctor [--live] [--json] [--strict] [--machine]` | Read-only health checks ([doctor](docs/doctor.md)). |
| `aimebu fe` | Open the web UI (prints the address over SSH). |
| `aimebu sessions` | Agents and their native resume hints. |
| `aimebu usages [provider] [--json]` | Last cached usage snapshots ([usages](docs/usages.md)). |
| `aimebu switcher <command>` | Switch which account Claude Code or Codex uses ([switcher](docs/switcher.md)). |
| `aimebu prune [-a] [-y]` | Clear agent runtime state, or everything with `-a`, server stopped ([prune](docs/prune.md)). |
| `aimebu update [--check]` | Update the clone the wrapper runs from. |
| `aimebu version`, `aimebu help` | Version and the full command and variable list. |

## Documentation

| | |
| --- | --- |
| [QUICKSTART.md](QUICKSTART.md) | Zero to a lead agent replying, for people and AI agents |
| [docs/reference.md](docs/reference.md) | The long-form feature reference |
| [docs/api.md](docs/api.md) | HTTP and WebSocket API |
| [docs/contract.md](docs/contract.md) | The implementation contract: data, protocol, errors |
| [docs/auth.md](docs/auth.md) | Owner setup, roles, invitations, sessions |
| [docs/supervisor.md](docs/supervisor.md) | Machines, pairing, Docker, folders |
| [docs/adapter.md](docs/adapter.md) | The harness adapters |
| [docs/tls.md](docs/tls.md) | HTTPS, reverse proxies, remote supervisors |
| [docs/quota-switcher.md](docs/quota-switcher.md) | Automatic account switching and the limit guard |
| [docs/telemetry.md](docs/telemetry.md) | What error reporting sends |
| [docs/release.md](docs/release.md) | Versioning, builds, distribution |

## Development

```sh
make test-full   # fmt-check, vet, go test -race, and the web unit tests (Docker)
make gate        # test-full plus the Chromium smoke twice; run once on the tip
make dev         # live-reload server on ~/.aimebu-dev, port 9897
```

Use an isolated `HOME`, `CODEX_HOME`, `CLAUDE_CONFIG_DIR` and
`AIMEBU_CONFIG_DIR` for anything that starts a server. [CONTRIBUTING.md](CONTRIBUTING.md)
has the prerequisites, the test commands and the web UI layout;
[AGENTS.md](AGENTS.md) holds the rules for agents working in this repository.
The standard library is the default; the few approved dependencies are listed
there.

## License

MIT, see [LICENSE](LICENSE).

# Continuing a Claude Code session as an agent

A Claude Code session that already exists on a machine (made in a terminal, by
a script, or by an earlier agent) can be continued by an aimebu agent: the
agent's first turn is `claude --resume <session id>` in the folder the session
was made in. Nothing of the session is copied into aimebu: no history is
imported, only a pointer. The model carries on from the transcript Claude
already holds, and that transcript keeps growing in place.

This is the same mechanism as [Start as v2 agent](import-v1.md#start-as-v2-agent)
for imported v1 agents; the difference is where the session id comes from. There
it is read from v1's database. Here the machine's own Claude state is the source.

## Where the sessions come from

Claude Code keeps each session as a JSONL transcript under
`<config>/projects/<working folder, every non-alphanumeric character as "-">/<session id>.jsonl`,
where `<config>` is `CLAUDE_CONFIG_DIR` or `~/.claude`, and registers a running
interactive process in `<config>/sessions/<pid>.json`. A machine's supervisor
reads those files on request (the `session_scan` frame, see
[the contract](contract.md#41-machine-level-frames-machine-key)), in the
environment of the launcher that will continue the session: a launcher whose
`env_refs` point `CLAUDE_CONFIG_DIR` (or `HOME`) elsewhere has its own sessions,
and that is the folder scanned, so what is listed is what `claude --resume`
will find through that launcher. The Spawn dialog passes the launcher it has
selected (the machine's native `claude` launcher when another tool is selected).

A scan reads only the first and the last 64 KiB of a transcript. The start
holds the working folder, git branch, Claude version and entrypoint (`cli` for a
terminal session, `sdk-cli` for print mode, which is what agents use); the end
holds the title Claude made (`ai-title`) and the last prompt. The title shown is
the last `ai-title`, else the last prompt, else the first prompt, cut to 120
characters on one line. **Nothing else of a transcript leaves the machine.**
A session belongs to a project when the working folder it records is the
project's folder or below it; the folder's directory name is only a hint where
to look (Claude hashes long paths, and a sibling folder can share a prefix). A
directory with a long name that does not start like the folder's is looked into
too, by the working folder of its newest transcript.

Subagent transcripts (`agent-*.jsonl`), sidechains, files that are not a session
id and sessions with nothing said in them are left out. A scan returns at most
200 sessions, newest first. The folder is decided before any limit is applied, so
other folders' sessions never push the project's out; reading is bounded at 5000
transcripts, and the answer says `truncated` when a limit cut the list (a probe
limit of 200 long-named directories counts too). A
transcript whose first line holding a working folder is longer than the first
64 KiB is read once more up to 1 MiB; one still without a folder is counted in
`unreadable`, and the picker says "N sessions could not be read".

## Listing and picking

`GET /api/projects/{project}/claude-sessions?machine_id=&launcher_id=` (the
answer names the `launcher_id` it read; the Spawn dialog keeps it, so the start
selects that launcher and not another native one with a different Claude home) lists the
sessions of the project's folder on a machine (the host by default) in the Claude
home of that launcher (default: the machine's native `claude` launcher, or the
supervisor's own home when the machine has none yet; 409 `launcher_unavailable`
for a named launcher that is unknown, disabled or not claude, 409 `needs_native`
for a containerized one); `?session_id=` finds one
session wherever it worked, on a machine where the project has a folder (409
`machine_folder_missing` otherwise). Only the project's owners and operators may ask: a
title is what somebody typed to Claude. Each session carries its `id`, `title`,
`cwd`, `git_branch`, `version`, `entrypoint`, `modified_at`, `size`, and:

- `active`: a live process registered it. It is open in a terminal (or an agent
  is running it). Advisory, the registry is the only evidence there is.
- `outside_folder`: it worked outside the project folder, so continuing it needs
  the owner's approval first, like any agent outside its folder. On a machine
  other than the host the project's folder is the one mapped for that machine, for
  an id lookup as for a listing.
- `agent`: the live agent that already continues it (started from it, or whose
  own Claude session it is). Such a session cannot be *continued* again, but it
  can still be copied (see below).

In the web UI the Spawn dialog has a button, **Continue a Claude Code session**.
It opens a picker of those sessions (and a field to look one up by id), and a
pick reopens the Spawn dialog for it, with a room of its own selected by default
and a choice: **Continue the original session** (the default) or **Work on a copy
(fork)**. A session an agent already continues can only be copied, so the dialog
starts at the copy. A machine whose supervisor predates session scans answers 409
`supervisor_update_needed`.

## Starting the agent

`POST /api/rooms/{room}/agents` takes `resume_session` (a Claude Code session id),
`resume_mode`, `confirm_session_closed` and `new_room`; see [the API](api.md). The server looks
the session up itself on the chosen machine and never trusts a folder from the
client. The tool must be `claude` on a **native** launcher: a container keeps its
own Claude home and cannot see the session (409 `needs_native`).

The agent runs in the folder the session was made in, resolved on the machine,
so it cannot use a worktree. If that folder no longer exists the start is refused
(409 `session_folder_missing`). Claude looks a session up under a directory named
after the folder it is started in, so a session made through a link (saved under
the link's name) cannot be found from the resolved folder: the start is refused
(409 `session_folder_alias`) rather than failing on the first turn.

`resume_mode` is `continue` (append to the original transcript) or `fork` (work on
a copy, below). **When the request does not say, it is `fork`**: the safe choice
for a caller that cannot ask (an API client, the bulk import). The web Spawn
dialog always sends its choice, and offers `continue` first. An unknown value is
400 `invalid_request`, and so is a `resume_mode` without `resume_session`.

Refusals that protect the transcript (they apply to `continue`; a copy leaves the
original as it is, so none of them stands in its way):

- 409 `session_active`: a Claude process has the session open. Close it, then send
  `confirm_session_closed`. Two processes appending to one transcript corrupt it.
- 409 `already_started`: an agent already continues it (also when a second request
  races the first: a session is attached to one live agent, by a unique row in
  `imported_session`). Use that agent. A revoked agent no longer holds it.

With `new_room` the agent starts in a new task room under the project's home
room, titled with the session's title (or `Claude session <id8>`), made in the same
transaction as the agent so a failed start leaves no room. The room gets one
system note: `<agent> joined as <role>, continuing the Claude Code session "<title>" (<id8>) in <folder>`.

## Working on a copy (fork)

With `resume_mode: fork` the agent's **first turn** runs
`claude --resume <id> --fork-session`: Claude loads the whole conversation and saves
what follows under a **new session id**, leaving the original transcript untouched.
The new id comes from the turn's first `system`/`init` event (the adapter reports it
as `native_session_started`); from then on the agent is an ordinary Claude agent
whose session is the copy, and its later turns resume that id without the flag.

- Until the first turn names the copy, `session.native_session_id` still holds the
  original and `session.fork_of` says so (that is the "pending" state: the next
  delivery carries `fork_session`, so a lost turn forks again from the original
  instead of resuming it). The agent profile shows no session id and no resume
  command then, because the original is not the agent's session.
- `imported_session` keeps the original's id and the mode, and stores the copy's id
  as `forked_session_id` once known. A copy does not hold its original: any number
  of agents can work on copies of one session, and the original can still be
  continued once (a continued session is held by a partial unique index).
- A copy needs no `confirm_session_closed` and ignores `session_active` and
  `already_started`: it only reads the original.
- Everything else is as for a continued session: the agent works in the folder the
  session was made in (so the single-writer-per-folder rule and the approval for a
  folder outside the project apply), counts against the project's limits, and the
  room's note reads `<agent> joined as <role>, working on a copy of the Claude Code
  session "<title>" (<id8>) in <folder>`.
- The supervisor must advertise `supports_session_fork`; a fork through an older one
  is 409 `supervisor_update_needed` (an older supervisor would ignore the flag and
  append to the original, so the server never sends it one). Continuing needs no flag.
  The check is made **on every delivery**, not only at spawn: if the machine
  reconnects with an older supervisor before the copy's first turn, that turn is not
  sent. The dispatcher holds the agent without preparing a turn (its notes stay
  queued), says so once, in the room where its note is queued ("…is held: … the supervisor on
  <machine> predates that…"; the first room it joined when nothing is queued), and goes on when a supervisor that can fork is connected;
  `Dispatch` refuses the delivery too (`supervisor_update_needed`) as a last guard.

## Importing many sessions at once

Settings → Project → **Claude Code sessions → Import sessions** lists the sessions a
machine has for the project's folder (the same list as the picker, for a chosen
machine, native `claude` harness and model), each with a checkbox and a role. Every
ticked session becomes **one agent in one room of its own, working on a copy**, idle
until it is sent a message. The roles are preset the way the single-writer rule needs
them: the first session of a folder is a worker, the others of that folder reviewers.

The dialog starts the agents one by one through the ordinary spawn request, so
nothing is bypassed: the project's `max_live_agents` and spawn rate apply (429
`spawn_limit` stops the rest, which stay ticked and are said not to have started, to
import later), a folder's writer rule can refuse one row (409 `writer_occupied`, shown on
that row; the others go on), and sessions outside the project folder wait for the owner's
approval in the Inbox. Nothing of what was said in a session is copied anywhere.

## What continues, and what does not

The first turn resumes the transcript, so the agent has the whole earlier
conversation. It is **not** the same as the terminal: aimebu turns run with a
strict private MCP configuration, so MCP servers and hooks from the user's own
project or home configuration are not loaded, the same as for any aimebu Claude
agent. Continuing appends to the original transcript and cannot be undone. Do not
open the session in a terminal again while its agent is active; the agent's
profile shows the resume command, and `aimebu sessions` lists it. A copy keeps the
original as it was, at the cost of a second transcript.

The agent counts against the project's live-agent limit and the spawn rate like
any other. It stays idle until something wakes it, and costs nothing until then.

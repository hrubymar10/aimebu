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
  own Claude session it is). Such a session cannot be started again.

In the web UI the Spawn dialog has a button, **Continue a Claude Code session**.
It opens a picker of those sessions (and a field to look one up by id), and a
pick reopens the Spawn dialog for it, with a room of its own selected by default.
A machine whose supervisor predates session scans answers 409
`supervisor_update_needed`.

## Starting the agent

`POST /api/rooms/{room}/agents` takes `resume_session` (a Claude Code session id),
`confirm_session_closed` and `new_room`; see [the API](api.md). The server looks
the session up itself on the chosen machine and never trusts a folder from the
client. The tool must be `claude` on a **native** launcher: a container keeps its
own Claude home and cannot see the session (409 `needs_native`).

The agent runs in the folder the session was made in, resolved on the machine,
so it cannot use a worktree. If that folder no longer exists the start is refused
(409 `session_folder_missing`). Claude looks a session up under a directory named
after the folder it is started in, so a session made through a link (saved under
the link's name) cannot be found from the resolved folder: the start is refused
(409 `session_folder_alias`) rather than failing on the first turn.

Refusals that protect the transcript:

- 409 `session_active`: a Claude process has the session open. Close it, then send
  `confirm_session_closed`. Two processes appending to one transcript corrupt it.
- 409 `already_started`: an agent already continues it (also when a second request
  races the first: a session is attached to one live agent, by a unique row in
  `imported_session`). Use that agent. A revoked agent no longer holds it.

With `new_room` the agent starts in a new task room under the project's home
room, titled with the session's title (or `Claude session <id8>`), made in the same
transaction as the agent so a failed start leaves no room. The room gets one
system note: `<agent> joined as <role>, continuing the Claude Code session "<title>" (<id8>) in <folder>`.

## What continues, and what does not

The first turn resumes the transcript, so the agent has the whole earlier
conversation. It is **not** the same as the terminal: aimebu turns run with a
strict private MCP configuration, so MCP servers and hooks from the user's own
project or home configuration are not loaded, the same as for any aimebu Claude
agent. Resuming appends to the original transcript and cannot be undone. Do not
open the session in a terminal again while its agent is active; the agent's
profile shows the resume command, and `aimebu sessions` lists it.

The agent counts against the project's live-agent limit and the spawn rate like
any other. It stays idle until something wakes it, and costs nothing until then.

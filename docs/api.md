# HTTP API

Run `aimebu server`. On a first run it prints a one-time owner setup token
(also in `.setup_token`, with a `/setup?token=…` shortcut, see [auth](auth.md#first-run-owner-setup)), or bootstraps
the owner from `AIMEBU2_OWNER_LOGIN` and `AIMEBU2_OWNER_PASSWORD` when both are set. The listener defaults to
`127.0.0.1:9897`; `AIMEBU2_BIND` must be an IP literal and `AIMEBU2_PORT` a
TCP port. Use `AIMEBU2_CONFIG_DIR` to choose the data root. These v2 defaults
and overrides are a temporary side-by-side setup that ignores v1's
`AIMEBU_*` environment settings.
`AIMEBU2_ALLOW` accepts comma-separated IPs and CIDRs and defaults to
`127.0.0.0/8,::1/128`. Bare addresses become `/32` (IPv4) or `/128` (IPv6).
IPv4-mapped CIDRs are rejected; use their IPv4 form instead. The embedded
supervisor's own dial address is added to the effective list when necessary,
including for a listener bound to a specific interface IP.
The allowlist gates every route, including the `/api/machine` WebSocket;
unlisted sources receive HTTP 403 before authentication. It checks the direct
peer address (including IPv4-mapped IPv6 addresses) and ignores
`X-Forwarded-For`. Invalid entries fail startup. Startup logs the effective
allowlist and warns if `AIMEBU2_BIND` is non-loopback while the default
loopback-only allowlist is active. For remote access, set both the bind and
the allowed source network.
When `AIMEBU2_TLS_CERT` and `AIMEBU2_TLS_KEY` are both set, the server also
listens with HTTPS on `AIMEBU2_TLS_PORT` (default `9896`). HTTP remains on its
configured port; both listeners apply the same allowlist. A missing or invalid
certificate pair fails startup. See [HTTPS setup](tls.md).
The host machine credential is written to `server/host-machine.key` with
0600 permissions; startup logs the path only.

All endpoints use JSON. `POST /api/login` establishes an HttpOnly session
cookie and returns a CSRF token. User endpoints require the cookie;
mutations also require `X-CSRF-Token`. The agent toolbox routes below use
`Authorization: Bearer <aak_ agent key>` instead, without a user cookie or
CSRF token. The global owner and the admins (accounts the owner flagged, see [auth](auth.md#global-admins)) can create projects, and the creator receives a project
owner membership, a home room and a `general` task room beneath it in the same
transaction. Project reads
require membership; owners and operators can write in ordinary rooms, while
participants in a two-person private chat can write there regardless of
project role while both remain active project members and no agent has joined.
Project owners manage membership through single-use invitation links and the
Users settings page.
Errors from these routes are JSON objects with `error` and `message` fields.
Authentication uses `unauthorized`, `invalid_credentials`, `invalid_csrf`,
and `rate_limited`; rate limits include a `Retry-After` header in seconds.
Validation uses `invalid_request`, `invalid_kind`, or `invalid_answer`, missing resources
use `not_found`, and authorization uses `forbidden`. Domain-specific errors
use codes such as those in [the contract](contract.md#9-error-codes), which lists
the main ones; routes also return others, named in the rows below where they
apply to one route.

The embedded web UI is served at `/` with an SPA fallback for extensionless
non-API paths. It uses the endpoints below, keeps the CSRF token in memory,
and reconnects the event stream from its last seen `seq`.

| Method | Path | Body or query | Result |
| --- | --- | --- | --- |
| GET | `/api/setup/status` | — | while a setup token waits: `{open:true, token_path}`, the file that holds it (the full path for a loopback connection with no forwarding header, `<config dir>/.setup_token` for any other; never the token); `200 {open:false}` once an owner exists |
| POST | `/api/setup/token` | `{token}` | checks a setup token without spending it (204; 403 wrong; 429 throttled; 404 once an owner exists) |
| POST | `/api/setup` | `{token,login,password}` | first-run owner setup (no session needed; 404 once an owner exists, 403 wrong token, 400 bad login/password, 429 throttled); creates the owner and signs in, same body as `/api/login` |
| GET | `/api/me/preferences` | none | `{preferences:{name:value}}`: the signed-in user's personal interface preferences |
| PUT | `/api/me/preferences` | `{preferences:{name:value}}` | stores them and returns all; names: `theme`, `inbox_muted`, `inbox_sound`, `inbox_volume`, `system_events`, `debug_inspector`, `markdown_mode` (`raw` or `rendered`: how message bodies show), `composer_formatting`, `sidebar_sort` (`name`, `activity` or `manual`), `sidebar_project_order` (a JSON list of project ids, up to 4096 bytes), `harness_setup_skipped` (`1` once an owner or admin skipped the offer to set up a harness, `0` or absent otherwise; the web app sets it back to `0` when it finds a harness with a model), `notification_banner_dismissed` (`1` once the person dismissed the bar that offers browser notifications), `sidebar_preview` (`1` to show the last message under every room in the room list; otherwise only the open room shows it) and `notification_text` (`1` when desktop notifications for chat messages show the start of the message text; off by default) (400 for others or values over 256 bytes; the retired `harness_layout` is accepted and dropped, so an older page's save is not refused). The browser keeps only a cache for painting before sign-in; per-device layout (collapsed panels) is not stored |
| POST | `/api/login` | `{login,password}` | user ID, CSRF token, expiry, `first_run`, `owner`/`admin` and, for those two, `host_home` (see `/api/me`) |
| POST | `/api/logout` | — | 204; revokes session |
| GET | `/api/session` | — | no session needed: what `/api/me` returns for a signed-in account, and `{signed_in:false}` (status 200) otherwise; the page asks this on load so the sign-in screen costs no 401 |
| GET | `/api/me` | — | user ID, login, `telemetry_prompt` (true for the owner until the telemetry popup was answered; never when `AIMEBU2_SENTRY_DSN` is set), CSRF token, `owner` and `admin` (what the account may do instance-wide: the web turns off New project and the owner/admin settings tabs for anyone else), `host_home` (only for the owner and admins, who create projects: the absolute home folder of the user the server runs as, which an empty "Workspace folder" field starts at; nobody else is told a path of the host), and `first_run`: true only for the instance owner and only until they press Continue on the first-run welcome (a database that already had people when the welcome migration ran counts as seen, and one made later, including by the v1 import, does not), so the web app shows the welcome once, between sign-in and the app |
| GET | `/api/me/telemetry` | — | the account's own telemetry identity `{fingerprint, send_name, login, report_id}`: a random per-account fingerprint (generated on first read), whether reports carry the login instead of it, and the one id reports carry now (see [telemetry](telemetry.md#who-a-report-is-about)) |
| PUT | `/api/me/telemetry` | `{send_name}` | the account's own switch "send my user name instead of a fingerprint"; answers the same object |
| POST | `/api/me/telemetry/fingerprint` | `{}` | replaces the account's fingerprint with a new random one and turns `send_name` off; answers the same object |
| GET | `/api/telemetry/settings` | — | owner or admin: `{enabled, dsn, default_dsn, prompted, env_locked, active, traces_sample_rate, max_db_spans}`, the Telemetry settings (the last two are read-only facts) (see [telemetry](telemetry.md)) |
| PUT | `/api/telemetry/settings` | `{enabled?, dsn?, prompted?}` | owner or admin: saves any of them (400 `invalid_dsn` for a DSN that is not one, or is plain http to a host other than this machine; an empty `dsn`, or the build's default, is stored as empty and means the default) and applies at once; answers the same object |
| GET | `/api/buildinfo` | — | any signed-in account: `{version, go_version}` (plus `sentry: {dsn, environment, release, traces_sample_rate, user_id}` only while reporting is on; the page starts its Sentry SDK from it, see [telemetry](telemetry.md)), the version string the server was built as (`git describe` when built with `bin/aimebu` or `make dev`, otherwise the default plus the VCS revision) and the Go toolchain; the page header shows it |
| POST | `/api/welcome/seen` | `{}` | owner-only; records that the owner has been through the welcome (`global_setting` `welcome_seen`), after which `first_run` is false. 204, idempotent |
| GET | `/api/users` | `project_id?` | the owner and the admins list all accounts (each row says whether it is the `owner` or an `admin`; the answer also carries `global_owner` and `global_admin` for the caller); a project owner lists that project's members |
| POST | `/api/invites` | `{kind:"join",project_id,role}` or `{kind:"reset",target_user_id}` | one-hour link; a project owner can invite to their own project; the owner and the admins can invite into any project at any role and send password resets (an admin only to ordinary accounts, never to the owner or another admin) |
| POST | `/api/invites/inspect` | `{token}` | no session needed; what a usable link is for: `{kind:"join",project_name,role}` or `{kind:"reset",login}`; `401 invalid_invite` for a spent, expired, unknown or no-longer-authorised link (the same throttle as redeeming) |
| POST | `/api/invites/redeem` | `{token,login?,password?}` | create account, grant signed-in account project access, or reset password; returns stored `login`; signed-in callers send CSRF token |
| PUT/DELETE | `/api/projects/{project}/members/{user}` | PUT: `{role}` | project owner (or the owner / an admin, for any project) changes role or removes member; last active owner stays; an admin never changes the owner's or another admin's membership |
| PUT | `/api/users/{user}/admin` | `{admin:boolean}` | owner only: gives or takes away the admin flag (400 for the owner or for a deactivated account when appointing, 404 unknown account); emits `user_admin_changed` |
| POST | `/api/users/{user}/deactivate` | — | the owner, or an admin for an ordinary account, blocks sign-in and revokes sessions; never the owner, and an admin cannot deactivate another admin |
| GET | `/api/prompts` | `project_id?` | every overridable prompt slot as seen from the scope (`key`, `title`, `desc`, `default`, `effective`, `source` = `default`/`global`/`project`, and the saved `global` / `project` override with its `rev`), plus `max` (2000), `inline_plan_appendix`, `can_edit` (global: the owner or an admin; project: its owner or operators) and `can_edit_global` (the owner or an admin); a project view needs access to that project |
| PUT | `/api/prompts/{key}` | `{project_id?, value, rev}` | saves one override (`rev` 0 creates, otherwise the current revision); 204; `400` for an unknown slot or an empty or over-2000-character value, `403`, `409 stale_rev` |
| DELETE | `/api/prompts/{key}` | `?project_id=&rev=` | removes one override, so the next level down (or the shipped text) applies; 204, `409 stale_rev`, `404` when there is none |
| PUT | `/api/inline-plans` | `{value: "always"\|"optional"}` | owner or admin; whether a lead's instructions always ask for the full plan as appendix pages (default `optional`); 204 |
| GET/PUT | `/api/agent-isolation` | PUT: `{docker_only}` | the instance setting "Only run agents in docker" (default off): `{docker_only, can_edit}`; any signed-in user reads it, owner or admin writes it (400 without `docker_only`). With it on, creating or switching an agent onto a native launcher answers 409 `docker_only`, and an agent already on one gets no turns (see [the supervisor](supervisor.md)) |
| GET | `/api/retention` | — | the message-expiry window (`messages_considered_expired_after_seconds`, default 21600, `0` = never, otherwise 60 to 2592000) with its default and bounds; any signed-in user |
| PUT | `/api/retention` | `{messages_considered_expired_after_seconds}` | owner or admin; 400 outside the bounds. Expiry only hides old messages from what an AI is given; it never deletes one |
| GET | `/api/credentials/targets` | — | owner-only token state of the stored profiles and live files of a migrated install (`enabled`, `targets`, `alerts`); never a token or CLI output. An alert of kind `refresher_conflict` means another refresher owns that tool's live credential file (it holds the claim lock, or its switcher root's token-copy marker names the same file): this server does not refresh the tool until it is gone |
| GET | `/api/quota-switcher` | — | owner-only automatic account switching: per-tool config, state, last decision and the newest 50; 503 until it runs (see [quota-switcher](quota-switcher.md)) |
| PUT | `/api/quota-switcher/{tool}` | config | owner replaces a tool's configuration (400 on validation, 404 unknown tool) |
| POST | `/api/quota-switcher/{tool}/rearm`, `/reenable` | `{}` | owner clears the after-reset latch / the circuit breaker |
| GET | `/api/limit-guard` | — | owner-only limit guard: per tool `{config, saved, result, held, since}` under `tools`, plus `profiles` and `percent_display` (`left` or `used`, the owner's Usages setting, which Health and Settings use to word the percentages); 503 until the quota switcher runs (see [quota-switcher](quota-switcher.md#limit-guard-acting-before-100)) |
| PUT | `/api/limit-guard/{tool}` | config | owner replaces a tool's soft/hard thresholds and per-profile overrides (400 on validation, 404 unknown tool) |
| GET | `/api/usages` | — | provider usage snapshots for Settings → Usages. The owner reads all of it; a global admin reads the limits only (`usages.Response.ForAdmin`: windows, resets, plan, credits, status and the profile names, but no e-mail, Enterprise host, error text, credential alerts, token-refresh state or configured-secret flags); anyone else gets 403. Every write stays the owner's |
| POST | `/api/usages/refresh` | `{provider?}` | owner-only forced refresh; `429` + `Retry-After` and `retry_after_sec` inside the 15-second cooldown (error code `cooldown`); `400 invalid_provider` for an unknown provider |
| GET | `/api/switcher` | — | owner-only enabled state, profiles, eligibility, and host agents affected by a switch |
| PUT | `/api/switcher` | `{enabled}` | owner enables or disables profile switching |
| POST | `/api/switcher/{tool}/profiles` | `{name}` | owner adds an empty profile |
| POST | `/api/switcher/{tool}/import` | `{name}` | owner adopts the current live login |
| POST | `/api/switcher/{tool}/use` | `{name}` | owner switches profiles; 409 with `agents` if matching turns run |
| DELETE | `/api/switcher/{tool}/profiles/{name}` | — | owner removes a profile; active removal clears the live login |
| POST | `/api/switcher/{tool}/profiles/{name}/rename` | `{name}` | owner renames a profile, active or not; the live login is untouched; 404 unknown profile, 409 destination exists |
| GET | `/api/health` | — | visible agents with `room_ids` and current health flags, plus the configured stuck threshold |
| GET | `/api/import-v1/status` | — | owner-only first-run status `{state, source_key?, ledger_rows?, job, reservation_missed?}`: `state` is `none`, `backup_incomplete`, `ready`, `resume`, `skipped`, or `done`; skipping or finishing closes the import path, and an install that already has messages reports `skipped`; `409 backup_changed` when the v1 backup was modified after it was made (it can no longer be verified, so it is not imported; the web wizard shows the message with the Skip choice, and restoring a pristine copy of `.old-v1-bkp` makes the status answer normally again); `reservation_missed` is true when an event already took a number inside the v1 message-number range, which makes the import refuse (the preview carries the same flag) |
| GET | `/api/import-v1/preview` | — | owner-only source identifiers, counts, destination choices, and suggested mapping (`409 source_unavailable` when the v1 data cannot be read), including enabled host launchers, and `carried_over_files` when v1 external credential targets are present in v2. The suggested people mapping maps the v1 person with the owner's name, or the one person in the data when there is only one (including human agent records, read cursors, and room preferences), to the owner, and gives every other person a new viewer account (`suggested.new_users`); `human_stakes` counts per person what a skip would drop (`memory_profiles`, `reactions`, `read_positions`, `room_view`) |
| POST | `/api/import-v1/dry-run` | the import mapping itself (as returned in `suggested`, plus any edits) | owner-only validation and import report without a commit; `200 {digest, report}`. The report lists `carried_over_files` when present; the digest names these exact choices and is what `commit` needs; `400 invalid_mapping` for a mapping that does not validate, `409 import_running` while an import is in progress |
| POST | `/api/import-v1/commit` | `{mapping, digest}` with the digest from the latest dry run of the same mapping | owner-only; starts the import in the background and answers `202 {job_id, phase:"scan"}`. `409 dry_run_required` when the digest is missing, stale or for other choices, `409 import_running` while one is in progress, `400 invalid_mapping` for a mapping that does not validate |
| POST | `/api/import-v1/skip` | — | owner-only first-run choice; 204, permanently closes import (`409 import_running` while an import is in progress); preview, dry run, and commit return 410 `import_closed` afterwards, and so does a skip after a completed import (a repeated skip of a skipped import stays a 204) |
| GET | `/api/import-v1/jobs/{id}` | — | owner-only job progress for a `job_id`: `{id, phase, error?, report?, invites?, invite_error?}` (`invites` are the one-time reset links of the accounts the import made: `{login, url, expires_at}`, each valid an hour and shown only here; `invite_error` says why they are missing). `phase` runs `scan`, `validate`, `copy`, `commit`, then `done` or `error`; `report` appears once the import has written. 404 for an unknown id |
| GET/POST | `/api/projects` | POST (owner or admin): `{name,folder,repo_url?}`; `folder` must be an absolute path to an existing folder on the machine running the server (`invalid_folder`) | accessible projects, each with the caller's `role` (`owner`, `operator` or `viewer`) and `archived_at` when it is archived / created project: the creator becomes its owner member and picks its folder on the host, the same for an admin as for the owner |
| GET | `/api/disk` | — | owner or admin disk report for Health: total, per project, per kind, worktrees with state, remote machines' agent output logs, free space, last cleanup ([cleanup](cleanup.md)) |
| POST | `/api/cleanup` | `{dry_run:boolean}` (required) | owner or admin: plan (`true`) or run (`false`) the cleanup; returns `{dry_run,items:[{category,target,project_id,reason,bytes,action}],bytes}` with action `would_remove`, `removed` or `kept` |
| GET/PUT | `/api/cleanup/settings` | PUT: `{enabled?,hour?,orphan_days?,attachment_days?}` | owner or admin schedule of the daily cleanup and the attachment retention (`attachment_days`, default 90, 0 keeps them; a removed attachment keeps its record and its message, shows `removed_at`/`removed_after_days`, and its download answers `410`) |
| GET | `/api/cleanup/log` | — | owner or admin cleanup log (what each cleanup removed, and why), newest first |
| POST | `/api/projects/{project}/archive` | — | the project's owner or an admin (who need not be a member) makes the project read-only: agents stopped, gate closed, schedules off, open questions and usage notices withdrawn; returns the project with `archived_at` and `stop_failed` (agents that could not be stopped because their execution is unknown until a recovery decision). Idempotent. While archived, every change through a project, room, agent, inbox or message route is refused with 409 `project_archived` (reading, reopening, stopping or pausing an agent, resolving a recovery or failed delivery, and removing a worktree are not), for whoever may write to the project at all (members, the owner, admins; anyone else gets the route's normal answer); starting an agent in it fails the same way, and a recovery decision there can only be Abandon, which ends with the agent stopped |
| POST | `/api/projects/{project}/reopen` | — | owner or admin lifts the archive; returns the project. The gate returns to what the project's control and today's usage limit say; agents stay stopped and schedules off |
| DELETE | `/api/projects/{project}` | `{name}` | 204; owner or admin deletes an **archived** project and everything in it: rooms (including the home room), messages, attachments (rows and files), agents, usage, memory, schedules, task notes, inbox items, worktree records, members and spawn defaults. `name` must equal the project's name (400 otherwise). 409 `project_not_archived` for a live project and 409 `project_busy` unless every agent is confirmed stopped or on a revoked machine (bring an offline machine online or revoke it) and no worktree is live. The project folder and worktrees on disk are not touched |
| PATCH | `/api/projects/{project}` | `{name?,folder?}` (at least one) | the project's owner or an admin renames it (the home room is renamed with it when it still has the old name); only an admin moves the folder (same checks as create: `invalid_folder`), and only while the project has no agents and no live worktree (`folder_in_use`); a `folder` equal to the current one is ignored; emits `project_updated` |
| GET/PUT | `/api/projects/{project}/settings` | PUT: `{auto_resume_after_clean_diagnostic?:boolean, worktree_mode?:"auto"|"off", home_lead_only?:boolean}` (at least one) | project recovery, worktree and home-room settings; owner/operator may change them. `home_lead_only` (Home room: lead only) defaults to `true`: a spawn into the home room accepts only roles with `cap_is_lead` and answers 409 `home_is_lead_only` for others; agents already there are untouched. `worktree_mode` defaults to `auto`: task rooms get their own Git worktree where the folder is a Git root ([cleanup](cleanup.md)) |
| GET/POST | `/api/projects/{project}/rooms` | POST: `{title,kind?,parent_id?,worktree?:{enabled,base_ref?}}`; kind defaults to `task`, the only client-creatable kind | rooms visible to this user, including the per-user `hidden` flag / created room, with worktree state if requested |
| GET | `/api/projects/{project}/claude-sessions` | `?machine_id=` (default: the host) lists the Claude Code sessions of the project's folder on that machine, read from the Claude home of `launcher_id` (default: the machine's native `claude` launcher); `?session_id=` finds one session wherever it worked (409 `machine_folder_missing` when the project has no folder on that machine) | `{machine_id, folder, launcher_id, unreadable, truncated, sessions:[{id,cwd,dir,title,git_branch,version,entrypoint,modified_at,size,active?,outside_folder,agent?}]}`; project owner or operator (the owner and admins may look at a machine that is not admitted yet); a machine other than the host must be admitted for the project first, else 403 `machine_not_admitted` before anything is sent to it; 409 `supervisor_update_needed` for a supervisor that predates scans, 409 `launcher_unavailable` / `needs_native`, 404 `session_not_found` for an unknown id, 409 `session_unreadable` for one with no readable working folder; `unreadable` counts transcripts that could not be assigned to a folder, `truncated` says a scan limit cut the list ([sessions](claude-sessions.md)) |
| GET | `/api/projects/{project}/dm-peers` | — | active people sharing the caller's project, excluding the caller, as `{id,login}`; any project member |
| POST | `/api/projects/{project}/dms` | `{user_id}` | the one private room for this pair in the project; both must be active project members |
| GET | `/api/projects/{project}/archive/search` | `q` (nonempty text) | up to 50 newest matching messages in closed rooms as `{room_id,room_name,message_id,snippet,created_at}`; project membership required and private chats limited to their active human member |
| GET/POST | `/api/projects/{project}/memory` | POST: `{body}` | `{enabled,can_edit,records,cap_bytes}` (each record has `author_kind`, `author_id` and `author_name`: a person's login or an agent's slug, empty when the author is gone) / created project fact |
| PUT | `/api/projects/{project}/memory-enabled` | `{enabled:boolean}` | enable or disable project memory; owner/operator only |
| PUT | `/api/rooms/{room}/memory-enabled` | `{enabled:false|null}` | restrict room memory flow or inherit project setting; owner/operator only |
| PUT/DELETE | `/api/projects/{project}/memory/{id}` | PUT: `{body,rev}`; DELETE: `?rev=…` | revised or deleted fact; pending imported facts require the installation owner |
| GET/POST | `/api/global-memory/{scope}` | scope `user_profile` or `agent_shared_notes`; POST `{scope_key,body}` | scoped global records (`can_edit` is true for the owner and the admins) / created record (owner or admin) |
| PUT/DELETE | `/api/global-memory/{scope}/{id}` | PUT: `{scope_key,body,rev}`; DELETE: `?scope_key=…&rev=…` | revised or deleted global record |
| PUT | `/api/global-memory/{scope}/{id}/activation` | `{scope_key,rev,active:boolean}` | the owner or an admin activates or deactivates a global record for agent context and recall |
| POST | `/api/agents/{id}/dm` | — | the existing or new private room for this user and agent in the agent's project |
| GET | `/api/agent-dms` | — | `{project_id,rooms,joinable}`: the private chats between agents the caller has joined (with `agent_chat:true` and `can_write`, listed under the reserved system `project_id`), and the ones they could join (`{id,title,last_activity_at,message_count,lifecycle}`, never message text). A chat shows only to someone who is the instance owner or belongs to the project of an agent still in it |
| POST | `/api/rooms/{room}/join` | — | joins a chat between agents the caller has a footing in (an agent's project member at any role, or the instance owner) and posts a system line saying so; idempotent; 404 for anything else. Returns the room |
| POST | `/api/rooms/{room}/leave` | — | leaves a chat between agents (system line); allowed in an archived chat |
| PATCH | `/api/rooms/{room}` | `{title}` | renames the room and returns it; the title is trimmed and must be 1 to 80 characters on one line, no control characters (400 `invalid_title`). A task room is renamed by the project's owners and operators (403 for a viewer); the home room's title is the project's name, so renaming it renames the project through the same path as `PATCH /api/projects/{project}` and takes the same rights (the owner or an admin), and the home room's title is set to the new name whatever it was; a private chat cannot be renamed (409 `invalid_kind`). A task room that already has the title is left alone and records nothing; a home room is left alone only when the room and the project both already carry it (a home room whose title drifted from the project's name is brought back in step, or the project renamed to the title, whichever the title is). Emits `room_renamed` (`{room_id,title,previous_title,actor_id}`), and `project_updated` too for the home room |
| GET | `/api/rooms/{room}` | — | room with the same live counts and preview as the project room list, plus the caller's `hidden` preference |
| GET | `/api/rooms/{room}/mention-users` | — | `{logins}`: the human logins a `@login` mention addresses in this room, as server addressing reads them: its user room members plus, outside DMs, every active member of the project; same room access as the room itself |
| GET | `/api/rooms/{room}/members` | — | read-only member list `{humans, agents, singletons}`: `humans` are `{id,login,role}` (a private chat's two people, or every active project member of an ordinary room, with the project role); `agents` are the room's agents as `{id,slug,role_key,role_label,role_emoji,singleton,desired,observed,revoked?,role_deleted?}`; `singletons` (empty for a private chat) lists each singleton role of the project as `{role_key,label,emoji,held_by}`, where `held_by` is `null` when vacant or `{agent_id,slug,in_this_room}` for the live agent holding it (the room it works in is never named). Same access as the room itself; nothing is changed |
| PATCH | `/api/rooms/{room}/preferences` | `{hidden?:boolean, notify?:"default"\|"all"\|"mentions"\|"none", muted_until?:string}` (at least one) | change the caller's own view of the room and answer it as `{hidden,notify,muted_until?}` (`pinned` is not accepted: nothing is pinned). `notify` is how the room notifies this person: `default` (the Inbox, and messages that address them), `all` (every message from somebody else), `mentions` (only messages that address them, and the room's `unread_count` counts only those) and `none` (no sound or notification from the room, its Inbox items included). `muted_until` is an RFC 3339 time in the future, at most a year ahead, until which the room is muted for them (the client sends the end of "1 hour", "8 hours" or "until tomorrow"); an empty string ends the mute, and it ends by itself at the time. A level or time the server cannot keep is 400 `invalid_request`. A change emits `room_preference_changed` (payload `user_id`) |
| POST | `/api/rooms/{room}/read` | `{seq?:integer}` | advance the caller's server-side read cursor to `seq` (the room's newest message when omitted or above the newest); the cursor never moves backwards. Returns `{read_cursor,unread_count}` and emits `room_read_changed` (payload `user_id`) when it advances, so the user's other sessions refresh |
| GET | `/api/rooms/{room}/read-receipts` | — | each member's read position as `[{member_kind,member_id,label,seq}]`: humans report their stored read cursor; an agent reports the newest message it has dealt with, which is v1's rule (a waiting v1 agent was handed every room message and its cursor moved as it took them): a message a turn took (a note in `in_turn` or `done`), one it wrote itself (so its marker moves past its own reply), or one the router handed it with nothing for it to do (not addressed to it, system chatter, or an unaddressed message for the worker while it is the lead put in the room), which counts the moment it is posted. A queued note does not count (a message meant for a busy or stopped agent stays unread until a turn takes it), and neither does a message meant for it that did not get through (revoked, out of reach, a full agent chat). Members who have read nothing are omitted. The web chat marks every member, the viewer and each sender included as v1 does, on the newest loaded message at or before their position and refreshes on `room_read_changed`, `message_created` and agent turn events |
| POST | `/api/rooms/{room}/close` | `{lifecycle_rev,cascade?}` | 202 with `closing` room; 409 `open_children` unless cascade is confirmed, or `home_room` for the project home room |
| POST | `/api/rooms/{room}/reopen` | `{lifecycle_rev}` | reopened room; no automatic agent respawn |
| DELETE | `/api/rooms/{room}` | — | 204; the project owner removes an **archived task room** for good: its messages, reactions, notes, turns, inbox items, schedules, task notes, members and attachment rows and files are deleted. Its usage and cost records go with it. Events, memory records and an agent's home room that merely point at it keep existing with the reference cleared. An attachment file that cannot be removed afterwards is logged, not an error. 409 `room_not_closed` (archive first), `home_room`, `has_children` (remove child rooms first), `room_not_removable` (a private chat, or a room with a live worktree); 403 unless the caller is a project owner. Emits `room_removed` (payload `{room_id}`) |
| GET/POST/DELETE | `/api/rooms/{room}/worktree` | `machine_id?` (query; the host when omitted); task rooms only (409 `invalid_kind`); POST `{enabled:true,base_ref?}`; DELETE `{force?,confirm?}` | status (a viewer gets `relative_path` but not the absolute `path` on the machine's disk), opt in/retry, cancel a pending or failed opt-in while open, or remove from a closed room. Active agent checkouts block removal even with force (`409 worktree_in_use`); `400 confirmation_required` when `force` lacks the matching `confirm`; `409 room_open` when the room is not closed and its worktree already exists (only a pending or failed opt-in can be cancelled while open). Dirty or unmerged work is preserved unless `force:true` and `confirm:"REMOVE <room-id>"` are supplied |
| GET | `/api/rooms/{room}/agent-name` | `exclude?` (query) | anyone who can open the room: a free name for the next agent of the room's project, from the same pool as an unnamed spawn, that a spawn with `name` accepts, never the `exclude` name when one is given; nothing is reserved, so the spawn can still answer `name_in_use`; the Spawn dialog prefills it and its dice button asks again with the name it shows as `exclude`. Result `{name}` |
| GET/POST | `/api/rooms/{room}/agents` | POST: `{role_key,tool?,variant?,launcher_id?,model?,provider?,machine_id?,name?,reason?,worktree?,base_ref?,resume_legacy?,confirm_v1_idle?,resume_session?,confirm_session_closed?,new_room?,work_on_copy?}` and `Idempotency-Key`; `name` is the agent's name (3 to 21 characters: lowercase letters, digits, `-` and `_`, starting with a letter and ending with a letter or digit, and not a group tag such as `all` or `here`), empty means the server picks a free one from its pool; 400 `invalid_name` for a name that does not fit, 409 `name_in_use` when the project already has an agent, person or role key of that name (checked before anything is prepared, and again in the spawn transaction, so two requests for one name create one agent), and 400 `invalid_request` when combined with `resume_legacy` (a resumed v1 agent keeps its own name); `resume_legacy` (a person only) starts the agent from an imported v1 agent: its name, and its Claude Code session continued in v1's working folder, or, with `resume_mode: fork`, worked on as a copy there (`continue` when omitted, as it always was; a copy needs a supervisor with `supports_session_fork`, 409 `supervisor_update_needed` otherwise, needs no `confirm_v1_idle`, and 400 `invalid_request` when the agent has no session that can be continued; other harnesses start fresh under the name); the agent is found by project, so it may have written in any room of this room's project (404 `not_found` when it wrote only in another project), and the room in the path is where it starts; 409 `home_is_lead_only` for a role without `cap_is_lead` in a home room while the project keeps it for the lead (also for `resume_legacy`); refusals `not_found`, `already_started`, `invalid_name`, `name_in_use`, `tool_mismatch`, `v1_may_be_active` (needs `confirm_v1_idle`), `v1_folder_missing`, `session_not_found`, `needs_native` ([import](import-v1.md#start-as-v2-agent)); `resume_session` (a person only; the tool must be `claude` on a native launcher) starts the agent from a Claude Code session the machine already has: its id, found on the chosen machine by the server, never a client-supplied folder; the first turn is `claude --resume <id>` in the folder the session was made in (no worktree, 400 otherwise); `resume_mode` is `continue` (append to the original) or `fork` (work on a copy: `--fork-session`, the original stays as it is), **`fork` when omitted** (the server logs each time it applies that default; earlier builds had no mode and always continued, so a caller that relied on that must now send `continue`), 400 `invalid_request` for another value or without `resume_session`, and 409 `supervisor_update_needed` for a fork through a supervisor that does not advertise `supports_session_fork`; `confirm_session_closed` acknowledges that a live process has the original open when continuing (409 `session_active` without it; a copy needs none), `new_room` starts the agent in a new task room titled after the session instead of the room in the path (that room's id is the card's `room_id`); further refusals `session_not_found` (404), `session_folder_missing`, `session_folder_alias` (the session's folder is not the directory the project resolves to; a link to it is fine: turns start in the session's own spelling, which needs a supervisor with `supports_launch_path`, else `supervisor_update_needed`), `supervisor_update_needed`, `needs_native`, and `already_started` for a session a live agent already continues (not for a copy) ([sessions](claude-sessions.md)) | agent cards / created agent; omit the harness and model to use an available role default, or supply `launcher_id` (the harness) or a `variant` naming one (`native`/`docker` select the harnesses of those names); `launcher_id` takes precedence; omitted machine selects the host unless a matching role default supplies one. The model must be one the harness lists (400 `model_unlisted` otherwise); with a `launcher_id` it may be omitted, which means the harness's default model |
| GET | `/api/rooms/{room}/agents/{agent}/profile` | — | read-only detail of a member agent for the web's Agent profile panel: `native_session_id`, `session_started_at`, `start_mode`, `resume_command` (only for tools whose resume syntax is known, `claude` and `codex`, and only when the session id is known), `cwd` (always the agent's own checkout), `worktree` and `branch` (only when that checkout is the agent's own ready worktree, or else the room's ready worktree, on the agent's machine; an agent working elsewhere, such as a continued v1 session in its old folder, shows neither), `machine_name`, `launcher_name`, `project_name`, `repo_url`, `spawned_by`, `spawned_at`, `turns_today`, `cost_today`, and `cost_today_estimated` when any part of that last-24-hour cost uses the embedded model price list. Empty fields are omitted, never guessed. `resume_command` is omitted for a containerised launcher (it cannot be resumed from the host). A project **viewer** gets no `native_session_id`, `resume_command` or `cwd`. Same access as the room. It also answers for an agent that left the room but spoke in it, and for a sender that exists only in imported v1 history: the profile is then the session v1 recorded (`imported: true`, `native_session_id`, `resume_command`, `cwd`, `project_name`, `spawned_at` as v1's first-seen and `last_seen`), only for an id that wrote in this room. For an owner or operator it also carries `start` (what "Start as v2 agent" needs: `slug`, `tool`, `model`, `role_key`, `cwd`, `resumable`, `reason`, `v1_active`, `outside_folder`, and `started_agent_id`/`started_agent_slug` once started, and `history_room`: the room of the project that holds most of the agent's imported messages, `{id,title,kind,lifecycle}`, which the dialog starts it in when it is an open task room); a v2 agent started from one carries `resumed_from`. 404 otherwise |
| GET | `/api/rooms/{room}/agents/{agent}/output` | `lines?` (default 200, at most 2000) | the end of what the agent's harness wrote to stderr, read from the machine that ran it (see [the supervisor](supervisor.md#agent-output)); **project owner or admin only** (403 otherwise), `Cache-Control: no-store`, at most 30 reads a minute a person (429 `rate_limited`, `Retry-After`). `{agent_id, machine_id, text, bytes, files, truncated, updated_at}`. 404 `not_found` when the agent is not one of this room's project's (an agent that has left its room is still readable), 404 `no_output` when nothing was captured; 409 `machine_offline` when the machine is not connected, 409 `supervisor_outdated` when its supervisor predates this; 502 `output_unavailable` when the machine could not read it. Every read is logged with who asked |
| GET | `/api/spawn-catalog` | `machine_id?` | the machine's harnesses with `launcher_id`, portable `launcher_name`, owner label, availability, `containerized`, `toolbox` capability and that harness's `models:[{id,label,note?,default?}]` (exactly one is the default; empty when the owner has listed none); omitted machine selects the host. Only a machine one of the caller's projects is mapped to (the host belongs to every project) as an owner or operator of that project is shown, and the owner and admins see any; any other machine, one that does not exist included, is 404 `not_found`. A machine with no harness has an empty list; `supervisor_update_needed` is true when the connected supervisor predates harnesses |
| GET | `/api/sounds` | — | `{sounds:[{id,name,kind,size?,uploaded_at?,ext?}]}`: the four bundled sounds (`builtin:chime`, `builtin:ding`, `builtin:beep`, `builtin:knock`, served from `/sounds/<name>.wav`) followed by uploads (`user:<id>`) in upload order |
| POST | `/api/sounds` | multipart `file` (`.mp3` or `.wav`, at most 1 MB; the header must match the extension) | owner or admin; 201 with the new entry. 400 `invalid_sound` (bad extension, header or multipart), 413 `invalid_sound` beyond 1 MB, 409 `sound_limit` beyond 10 uploads |
| GET | `/api/sounds/{sound}` | — | the uploaded file (`audio/mpeg` or `audio/wav`, `nosniff`, `Cache-Control: private, no-store`); any signed-in user |
| DELETE | `/api/sounds/{sound}` | — | owner or admin; 204, removes the file |
| GET | `/api/machines` | `project_id?` | without a project ID, the owner and admins see all machines with approval/revocation state; with a project ID, project members (including the owner) see only approved, unrevoked picker choices; both include host marker, connection, last seen, and supervisor version; with a project ID the host row also carries `project_worktree` (`{ok, code?, reason?}`: the server's own worktree check on the project folder, so the Spawn dialog offers a worktree only where one can be made and says why when not, e.g. `not_git_repo`, `not_git_root`, or `git_unusable` with git's reason such as dubious ownership; absent when not known) |
| GET | `/api/machines/{id}/harnesses` | — | legacy compatibility view of the six supervisor-discovered harness rows and recent setting changes |
| PUT | `/api/machines/{id}/harnesses/{tool}/{variant}` | `{enabled,override_path}` | owner only (an `override_path` makes the supervisor run that file); legacy compatibility setting endpoint; it does not change a named launcher on updated supervisors |
| GET/POST | `/api/machines/{id}/launchers` | POST (owner only): `{tool,label?,name?,command,args,env_refs,enabled,containerized,agent_server_url?,path_maps,visible_roots,supports_toolbox}` | owner or admin sees the machine's harnesses (stored as launchers; nothing is seeded): config with `label`, observed availability/version (and, for a Docker launcher of a tool that depends on a launcher feature, the `features` the supervisor probed and a `feature_problem` when the probe failed or the launcher is too old; the launcher stays available; and, for a Docker launcher whose version is unknown, a `version_problem` saying why: no project folder to ask it from, what the launcher itself said on stderr (a folder its container does not mount), or that it did not answer in time; the supervisor promptly retries timeouts and retries unresolved container versions about once a minute when a probe folder exists), active agent count, each harness's `models:[{slug,label,note,default}]` with `models_rev` (a short digest of that list, to send back with an edit), and change activity; POST creates a harness after connected supervisor validation. An omitted `label` is the family's name ("Claude Code", plus " (Docker)"); an omitted `name` is `native` (or `harness-docker`) when the machine has no such harness for the tool, otherwise a unique slug of the label. `docker` is reserved |
| PUT/DELETE | `/api/machines/{id}/launchers/{launcher}` | PUT: full launcher definition; DELETE: — | owner only (a launcher is a command line a machine runs) edits or removes a harness (its models go with it); an active or stopping agent, including a revoked one, or an in-flight switch targeting it returns 409 |
| PUT | `/api/machines/{id}/launchers/{launcher}/models` | `{models:[{slug,label,note?,default?}],expected_rev?}` | owner only replaces a harness's whole model list, atomically (works while the machine is offline); the answer is `{models,models_rev}`. With `expected_rev` (the `models_rev` the edit was based on) an edit over a list that has changed since is refused with `409 stale_models` and changes nothing, so two tabs or two people cannot overwrite each other; without it the list is replaced unconditionally. The Save-models editor in the web app always sends it; the first-run fill of a new harness's models does not. Slugs match `^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$` (OpenCode: `provider/model`), labels are at most 80 characters, notes at most 280, at most 64 models, and a non-empty list has exactly one default; otherwise 400 `invalid_models` and nothing changes. A keyed spawn or switch retry still replays its stored result after its model is removed. An agent whose model is no longer listed starts with the harness default (its model is updated and the change is logged as `model_default` in the activity) |
| GET/PUT | `/api/machines/{id}/launchers/{launcher}/providers` | PUT: `{providers:[{id,name,npm,base_url,key_env?,context_tokens?,output_tokens?}],expected_rev?,remove_models?}` | owner only for PUT, owner or admin for GET reads or replaces an OpenCode harness's whole list of custom providers (OpenAI-compatible endpoints), atomically and while the machine is offline; both answer `{providers,providers_rev,models,models_rev}`, and the launchers listing carries `providers` and `providers_rev` for every harness (empty for the other families). `id` is the prefix of the provider's model ids (`<id>/<model>`: lowercase letters, digits and hyphens, starting with a letter, at most 40 characters, and not one OpenCode ships such as `openai`), `name` at most 80 characters, `npm` must be `@ai-sdk/openai-compatible`, at most 16 providers. `base_url` is `https://…`, or `http://…` only for this machine (`localhost`, loopback), `host.docker.internal` or a private-network address; no credentials, query or fragment. `key_env` names the environment variable the key is read from on the machine that runs the turn (upper case, may be empty for a server that takes no key; `PATH`, `HOME`, `LD_*`, `XDG_*`, `HARNESS_DOCKER_*`, `OPENCODE_*`, `AIMEBU*` and the like are refused, and so are the keys of OpenCode's own providers such as `OPENAI_API_KEY`: the host's value of one would be sent to the custom base URL): **only the name is stored, never a key**. The optional token limits come as a pair (`context_tokens` and `output_tokens`, both or neither; 0 or absent leaves them to OpenCode) and apply to all of the provider's models. A refused list is 400 `invalid_providers` and changes nothing; with `expected_rev` an edit over a list that has changed since is 409 `stale_providers`. Dropping a provider (or changing its `id`) while models of the harness start with `<id>/` is 409 `provider_in_use` with `provider_id` and `model_count` (the first such provider) and `in_use:[{provider_id,model_count}]` (all of them, since `remove_models` deletes the models of every dropped provider); `remove_models:true` deletes those models in the same transaction (another model becomes the default if the default went). Changes are logged as `providers` (and `models`) in the machine's launcher activity |
| GET | `/api/machines/{id}/launchers/{launcher}/providers/key-status` | — | owner or admin: for each provider of an OpenCode harness that names a key variable, whether that variable has a value on the machine that runs the turns: `{machine_online, supervisor_supported, keys:[{provider_id,key_env,set}], error?}` with `set` true, false, or null while it cannot be known (machine offline, supervisor without `supports_providers`, no answer). The supervisor answers only whether (through the harness's environment references too), never a value; a name the harness uses itself is never answered. Another family is 400, another machine's harness 404 |
| POST | `/api/machines/{id}/launchers/{launcher}/providers/import` | — | owner only asks the machine's supervisor which custom providers the person's own OpenCode config (`$XDG_CONFIG_HOME` or `~/.config/opencode`, `opencode.json` or `.jsonc`) defines, as suggestions for this harness: `{source, suggestions:[{provider:{id,name,npm,base_url,key_env,context_tokens?,output_tokens?}, models:[{slug,label}], inline_key?, problem?, already_added?}], skipped:[{id,reason}]}`. Only `@ai-sdk/openai-compatible` providers qualify (the others are `skipped` with the reason). **No key is ever copied**: a key written as `{env:NAME}` becomes `key_env: NAME`, a key written into the config as text only sets `inline_key` (the owner names a variable and sets it). Each suggestion is checked against the rules a saved provider must meet (`problem` says why one cannot be taken as it is). Nothing is saved: the owner saves the list through the PUT above. 503 when the machine is offline or its supervisor predates providers, 502 `import_failed` when the config cannot be read |
| POST | `/api/machines/{id}/launchers/detect` | — | owner only (setting harnesses up is the owner's) asks the machine's supervisor which harnesses are installed (PATH plus the usual user-level install directories, once per real file, with versions). Candidates only: `{candidates:[{family,command,containerized,version?,label,already_added}]}`; nothing is saved. 503 when the machine is offline or its supervisor predates detection; 502 `detect_failed` |
| POST | `/api/machines/{id}/launchers/{launcher}/models/detect` | — | owner only (it sends a provider key to the provider address) asks for the models a harness offers: Pi `pi --offline --list-models`, OpenCode `opencode models`, Codex `codex debug models --bundled` (the models it lists), Mistral Vibe (no command: the `[[models]]` of its config, by alias, and its built-in default), run by the supervisor on the machine with the harness's own arguments first, native or containerized alike, in a fresh private `detect/run-*` folder under the machine's aimebu config directory (removed after the command when possible; leftover `run-*` folders older than one hour are cleared before the next detection) unless the optional body `{dir}` names an absolute folder on that machine; Claude from the Anthropic Go SDK's published `message.go` (fetched by the server only on this call, 5 s, ETag-cached, with a built-in list when unreachable). Candidates only: `{candidates:[{slug,label,note?,already_listed}],source}` with `source` `harness`, `github`, `cache` or `snapshot`; nothing is saved. 400 `invalid_dir` when `dir` is not absolute or not an existing folder on that machine; 503 `machine_unavailable` when the supervisor is offline, or too old to honour `dir`; 502 `detect_failed` with the run path, the end of the command's stderr, "Add the model ID manually", and `detect_dir` (only the private `detect` folder, or the owner-chosen folder, when the supervisor answered), and for an OpenCode harness `endpoint_problems` when custom endpoints could not be asked (also on the 502 for an empty list, and a harness that reports no models still asks the custom endpoints before it fails) For an OpenCode harness the listing runs in the private config an agent turn gets (the person's own OpenCode config and the project's are ignored, the harness's custom providers are defined), so it shows what an agent can use, and each custom provider's own endpoint (`GET <base_url>/models`, with that provider's key if it is set on the machine; no redirect is followed) is asked which models it serves, all of them at once within a shared 8 second budget (the listing command's own 20 seconds come first, and the server waits 30 for the answer): those come back as candidates `<provider id>/<model>`, and an endpoint that could not be asked, or did not answer in time, is named in `endpoint_problems` (`{provider_id: reason}`, never an answer's text) |
| GET | `/api/doctor` | — | read-only; no session. For `aimebu doctor --live`: authenticated by `Authorization: Bearer <key>` with the key in `server/doctor.key` (0600; written at server start, replaced at every start, removed at shutdown), and only from a same-host connection (peer address loopback, or equal to the address the connection was accepted on) without a forwarding header (`X-Forwarded-For`, `Forwarded`, `Via`, …); anything else is `403`. Answer `{machines:[{id,name,approved,connected,supports:[{flag,supported,degrades}],launchers:[{id,tool,name,label?,enabled,containerized,reported,available,unavailable_reason?,version?,features?,feature_problem?}]}]}` for every non-revoked machine; `supports` (the hello's capability flags, with what degrades without each) is present only while the machine is connected. No keys, environment or launcher arguments |
| POST | `/api/machine-pairings` | `{name}` | owner creates a single-use, 10-minute token and copyable supervisor command. When the server is reached over plain http at an address other than this machine's, the answer adds `insecure_transport: true` and a `warning`, and the command carries `--insecure` (the supervisor refuses to pair over plain http to another machine without it) |
| POST | `/api/machine-pairings/redeem` | `{token}` | allowlisted token exchange, rate-limited per submitted token and client IP; returns pending machine ID and key once |
| GET | `/api/machines/{id}/pair-status` | machine bearer key | pending/approved state for the paired supervisor; revoked keys get 401. For an approved machine the answer also lists `provider_keys` (`[{launcher,provider_id,key_env,env_refs?}]`: the key variable names of the custom providers of the machine's enabled OpenCode harnesses, with the harness's environment references, names only), which `aimebu doctor --machine` checks against its own environment |
| POST | `/api/machines/{id}/approve` | — | owner approves a pending machine |
| POST | `/api/machines/{id}/agents/stop` | none | stops every agent that runs on the machine, as a single stop (`POST /api/agents/{id}/control`) does and by the same permission: `{stopped:[agent ids], already_stopped, forbidden, failed:[{agent_id,error}]}`; an agent in a project the caller may not manage is left running and counted as `forbidden` (403 when that is all of them); 404 for an unknown machine. A stopped agent starts again from its card |
| POST | `/api/machines/{id}/revoke` | — | owner revokes a remote machine and closes its socket immediately |
| GET | `/api/projects/{project}/machine-folders` | — | effective folder per unrevoked machine; host uses the project folder |
| PUT | `/api/projects/{project}/machine-folders/{machine}` | `{folder}` | owner/operator sets an absolute, clean remote folder path |
| POST | `/api/projects/{project}/machine-folders/{machine}/admit` | none | the project's owner (or an admin) admits a paired machine for the project without starting an agent to raise the approval: 204; 403 for anyone else, 404 unknown machine, 409 when the machine is not approved or has no folder mapped for the project. Until a machine is admitted its folder is not read and no worktree is made there (403 `machine_not_admitted`), except for the project's owner and the admins, whose own requests pass |
| GET/POST | `/api/roles` | POST: `{role_key,label,emoji,body,extends?,cardinality,cap_is_lead,cap_can_spawn,cap_singleton_writer,default_agents?,pick_mode?}` | global role definitions (each with `extends` and a read-only `resolved_body`) / created definition; `default_agents` is an ordered array of at most 32 `{launcher_id,model}` from the models a harness lists (400 `invalid_spawn_defaults` otherwise), and `pick_mode` is `in_turn` (default) or `randomly`; roles are global only, so a `project_id` on a write is `400 invalid_request` and on the list is ignored |
| GET | `/api/projects/{project}/role-spawn-defaults` | — | effective defaults for each role with `inherited`, global list/mode, and durable next position |
| PUT | `/api/projects/{project}/role-spawn-defaults/{role}` | `{inherit,default_agents?,pick_mode?}` | project owner or operator sets a complete list/mode override, or deletes it to inherit global settings |
| GET | `/api/projects/{project}/role-spawn-defaults/{role}/next` | — | next currently available default, with machine, launcher, tool, variant, and model for the human Spawn dialog; `available=false` when none can start |
| PUT/DELETE | `/api/roles/{id}` | PUT: full definition with `rev` (an omitted `extends` clears it); DELETE: `?rev=…` | revised definition / 204; `409 role_extended` while other roles extend it |
| POST | `/api/roles/{id}/reset` | `{rev}` | the owner or an admin restores a built-in global role (id ending `_default`) to its shipped name, emoji, instructions, parent, cardinality, capabilities, and empty default-agent list with in-turn mode; returns the definition (with `resolved_body`); `400` for a role that is not built in, `409 stale_rev`, or `409 role_in_use` when live agents hold it with other capabilities or cardinality |
| GET/PUT | `/api/macros` | PUT: complete `{key:body}` map | shared composer macros; any signed-in user reads, the owner or an admin replaces (CSRF required) |
| GET | `/api/settings-backup` | — | owner or admin download of the instance's settings (an admin's file leaves out `quota_switcher` and `limit_guard`: they decide over the owner's logins, and only the owner can apply them) as `{schema:"aimebu-settings-backup", version:1, exported_at, macros, roles, retention, quota_switcher, limit_guard}`; each section is present only when it has content, except `retention`, which is always present. `roles` are the **global** ones only; `quota_switcher` and `limit_guard` are the saved per-tool configurations (a tool on its defaults is left out) and appear only while the switcher runs. Built field by field, so it never contains credentials, tokens, provider secrets, machines, users or sessions. The web UI adds a `ui_prefs` section of its own (browser preferences, named by what they are) |
| POST | `/api/settings-backup/preview` | `{backup}` | owner-only; saves nothing. Per section `{section, new, overwrite, unchanged, invalid:[{key,reason}], unavailable?}`, `ignored` (top-level members this version does not know) and a `digest` of the plan. 400 `invalid_request` for a wrong `schema`, 400 `unsupported_version` for a `version` other than 1 |
| POST | `/api/settings-backup/apply` | `{backup, digest, sections?}` | owner-only; `409 preview_stale` unless `digest` is the one a preview of the same file answered against the current state: the digest covers every step and the current value of each entry it would overwrite (or that it is absent), so a different file, or an edit of any of those entries after the preview, is refused. `sections` chooses a subset: absent means every section that has changes, and a present but empty list means none. Sections apply in a fixed order (macros, retention, quota_switcher, limit_guard, roles, ui_prefs) and independently; within a section an entry that cannot be saved is reported in `failed` while the rest still apply, so a partial import is possible and is never silent. Returns `{sections:[{section,applied}], failed:[{section,key,error}], complete}`. Matching macros and roles (by key) are overwritten, nothing is deleted; model lists belong to the harnesses now, so a `spawn_models` section in an older file is not imported and the preview lists it under `ignored`; body limit 4 MiB |
| GET/POST | `/api/rooms/{room}/control` | POST: `{action:"pause"|"stop"|"resume",rev}` and `Idempotency-Key` | room bulk control state and per-agent results |
| GET/POST | `/api/projects/{project}/control` | POST: `{action:"pause"|"stop"|"resume",rev}` and `Idempotency-Key` | project bulk control state and per-agent results |
| GET/PUT | `/api/usage-limit-default` | PUT (owner): `{daily_cost_cap?,daily_turn_cap?}`, null or absent = no limit | the global default daily limits every project inherits (any signed-in user may read it; `can_edit` says whether the caller may change it); changing it re-checks the projects that inherit it: for each field (cost, turns) whose default actually changed, today's Continue raise on a project that inherits that field is cleared (a field the project sets itself, or one whose default is unchanged, keeps its raise), so a raised default lifts the pause the old one caused; saving the values it already has does nothing |
| GET/POST | `/api/rooms/{room}/schedules` | POST: `{target_agent_id? ,target_lead?,body,kind,timezone,at?,interval_seconds?,local_time?,weekday?}` | task-room or home-room schedules / created schedule; 409 `invalid_scope` for a private chat or the lobby, 400 `invalid_schedule` for a bad target, time or note, 409 `schedule_limit` beyond 20 active |
| PATCH/DELETE | `/api/rooms/{room}/schedules/{id}` | PATCH: `{enabled,rev}` | pause/resume or delete a schedule |
| DELETE | `/api/agents/{agent}/worktree` | `{force?,confirm?}` | removes the worktree a second writer works in (project operator): `{state:"removed"}`, or `{state:"preserved",reason}` when it holds uncommitted or unmerged work, which only `force` with `confirm: "REMOVE <name>"` removes anyway (400 `confirmation_required` otherwise); 409 `worktree_in_use` while its agent runs (a start of the agent waits for a removal under way, and a removal that finds the agent started refuses); the branch is kept, and the agent goes back to its room's checkout (its ready worktree, else the project folder; starting it takes that checkout's writer slot, or makes the worktree again when another writer holds it) |
| GET/PUT | `/api/projects/{project}/usage-limit` | PUT: `{cost_mode?,daily_cost_cap?,turn_mode?,daily_turn_cap?,rev}`; a mode is `inherit`, `custom` (needs its value) or `none`; without modes a value means custom and null means none | the project's daily limits (all its rooms together), the caps in force, `inherited_cost_cap`/`inherited_turn_cap` (the global default), measured totals, and effective state |
| POST | `/api/agents/{id}/control` | `{desired,rev}` | updated agent card; who may do it is decided first (project owner or operator), so a person who may not, and an id that is no agent, both get the same 403 before anything is loaded or sent to a machine |
| DELETE | `/api/agents/{id}` | `{}` | removes a **stopped** agent for good (project owner or operator; 409 `agent_not_stopped` unless it is `desired:stopped` and `observed:stopped`, 409 `project_archived` in an archived project, 404 for an agent that is gone or already removed): its worktree of its own, if it has one, is removed first by the rules of `DELETE /api/agents/{agent}/worktree` without `force`: a clean worktree is removed, and work in it refuses the removal (409 `worktree_preserved`, with the reason: the agent keeps its name until the person has removed or merged the worktree on purpose, so that a new agent of that name never finds this work in its folder); 409 `worktree_in_use`, or the worktree error, also leaves the agent as it was. The branch it made is kept, set aside as `aimebu/<room>-<slug>-removed-<n>` so that a worktree made later under the freed name starts from the room's base; then in one transaction the agent leaves every room (its queued notes are dropped; a chat between agents left with fewer than two agents is closed), its open Inbox items are withdrawn, and it is revoked, which stops its key and frees its role's slot, its checkout's writer slot and its name (the row keeps a tombstone in `slug` and the name it had in `removed_slug`). Schedules aimed at the agent are paused, each with a notice in its room. Answers `{agent_id,slug,worktree}` (`worktree` is `{name,path,branch,state,reason,kept_branch}`, `kept_branch` being the set-aside branch name when there was one, or `{}` when the agent had no worktree). Its messages, turns and history stay: a message carries `author_name` (the name the agent had) and `author_project_id`, and a reply's `reply_preview` the same, so a client writes `<slug>@<project> (removed)`. Events `room_member_left` per room and `agent_revoked` (with `slug` and `removed_by`) |
| POST | `/api/agents/{id}/switch` | `{room_id,tool,variant,launcher_id?,model,machine_id?,rev}` and `Idempotency-Key` | 202 agent card with pending switch; `launcher_id` takes precedence over `variant`; omitted machine keeps the current one; 200 on keyed replay |
| GET/POST | `/api/rooms/{room}/messages` | GET: `limit=1..500` (default 200), one of `after=<seq>`, `before=<seq-or-id>`, or `around=<seq-or-id>`; `thread_root=<id>` gives the conversation that message belongs to (its first message's replies, any depth) with the same cursor choices. POST: `{body,reply_to?,attachments?:[id]}` (a `thread_root` sent is ignored: the conversation follows `reply_to`) | GET returns ascending messages: latest page by default, exclusive newer or older page with `after` or `before`, a context page containing `around`, or matching thread replies. Each GET message includes reactions and the full `thread_count` for its root, including replies outside the page. Agent `needs_attention`, `proposed_answers`, `open_questions`, `visual_plan`, and `appendix_pages` appear when present. A message from a person carries `author_name`, the person's login, next to `author_id`, so a client shows a name rather than the account id. POST creates a user message with attachment metadata and includes `thread_count` for a thread reply when the count lookup succeeds; the saved reply still returns 201 if that lookup fails. A reply (and a created reply) also carries `reply_preview` `{no, author_kind, author_id, author_name?, snippet}` for the message it answers, read for the whole page in one query, so a client that has not loaded that message can still quote it; `snippet` is one line of its text (whitespace collapsed, cut to 96 characters, `[attachment]` for a message that is only files), and a parent that is not in the same room previews nothing. A message an agent wrote also carries `usage` `{turn_id, index, count, tokens_in?, tokens_out?, tokens_cache_read?, tokens_cache_write?, cost?, cost_source?, model?, partial?}`: what the turn that wrote it spent, for anyone who can read the room. Every message of a turn carries the same turn figures with its place (`index` of `count` messages the turn wrote), so a client must never add them up across messages; a figure the harness did not report is absent (never 0), `cost_source` is `estimated` for a dated model price-list value and `reported` for a harness value, and `partial` means the turn ended before it reported its totals, so the figures are a floor. The turn's several usage records are summed once; a message whose turn reported nothing has no `usage`. Read for the whole page in three queries (the turn behind each message, the turn's message count, the turn's totals). `tokens_in` is as the harness reports it: Claude and opencode report fresh input with the cache apart; Codex input excludes its reported cache-read and cache-write shares, which appear separately. Every message carries `no`, its number: the "#N" shown, copied and written, one sequence across every room counted by messages alone (see the contract), while `seq` is the internal order shared with events that paging cursors (`after`, `before`, `around`) use. A GET also returns `expirable` on every message: true only for AI group chatter that message expiry may leave out of an agent's context (see [message expiry](retention.md)) |
| GET/POST | `/api/rooms/{room}/task-notes` | POST: `{body}` | `{can_edit,records,cap_bytes}` / created task note |
| PUT/DELETE | `/api/rooms/{room}/task-notes/{id}` | PUT: `{body,rev}`; DELETE: `?rev=…` | revised or deleted task note |
| POST | `/api/rooms/{room}/attachments` | multipart `file`, any type ≤10 MB; a person may have at most 20 unsent uploads, 100 MB together (429 `attachment_quota`: send or remove some) | staged `{id,name,mime,size,url,width?,height?}` (`width` and `height` are the pixel size of a PNG, JPEG or GIF, omitted when unknown, and are carried on a message's `attachments` too, so a thumbnail can hold its place); room access and CSRF required |
| (SVG) | upload | — | Go does not sniff SVG, so an uploaded file is checked with the standard library's strict XML decoder: UTF-8 only, at most 2 MB, 50,000 tokens and 64 levels deep, no DOCTYPE or entity declaration, the root an `svg` element in the SVG namespace, and nothing active: no `script`, `foreignObject`, `iframe`, `embed`, `object`, `audio`, `video`, `handler` or `listener` element, no `<?xml-stylesheet?>` instruction, no `on*` attribute or `javascript:` value, no `href`/`src` that leaves the file (only `#fragment` and inline PNG, JPEG, GIF or WebP data are allowed), no CSS `@import`, `image-set()` or `src()`, no `url()` that is not one of those (CSS comments are removed before the check, and the text of a `<style>` is read as one piece), and no `set` or `animate` element that writes an event handler or a reference or CSS that leaves the file. A file that passes gets `mime: image/svg+xml` and its `width`/`height` from the `width`/`height` attributes or the `viewBox`; the web UI shows it as an `<img>` and never puts it into the page as markup. One that fails keeps its sniffed text type and stays download-only, as before. Nothing is rewritten. The `attachment_get` tool gives an agent an SVG as source text (up to 64 KB) |
| DELETE | `/api/rooms/{room}/attachments/{attachment}` | staged upload owned by caller | 204; discards unused file |
| GET | `/api/attachments/{attachment}` | — | file bytes; caller must have access to the file's room (project membership for normal rooms, DM peer membership for DMs); an upload no message has claimed yet is its uploader's alone, and a file the caller may not open answers the same 404 `not_found` as one that does not exist. Only PNG, JPEG, GIF, and WebP stay inline; every other MIME type downloads with an attachment filename (a fallback if the stored name is empty). An SVG that passes the image check (above) is `image/svg+xml` and is inline only for a request with `Sec-Fetch-Dest: image` (an `<img>`); any other request (a tab opened on the address, a script fetch, a client that sends no header, the agent routes) downloads it. Attachment responses use `nosniff` and `Content-Security-Policy: sandbox; default-src 'none'`; an SVG adds `style-src 'unsafe-inline'` and `Cross-Origin-Resource-Policy: same-origin`. |
| GET | `/api/agent/agents` | agent key | `list_agents`: non-revoked agents in the caller's project, with ID, slug, role, desired/observed state, desired revision, and home room ID; no other project's agents |
| GET | `/api/agent/rooms/{room}/history` | agent key; `before_no?`, `limit?` (1–100, default 50) | `room_history`: ascending page of message-number records, newest page by default, older page when `before_no` is supplied; excerpts are limited to 2,000 characters and the page to 240 KiB; `next_before` is the oldest returned number and `has_more` indicates an older page; same project-wide room access as `message_get`, with private chats requiring current membership; 403 if inaccessible |
| POST | `/api/agent/agents/{id}/role` | agent key; `{role_key,rev}` and `Idempotency-Key` | `set_role`: active lead only, target in the same project; changes a non-lead agent's role when idle or stopped, rejects lead roles, stale revisions, occupied singleton roles and writer conflicts; emits a system event and room message; keyed replay returns the first result |
| GET | `/api/agent/message-numbers/{no}` | agent key | the `message_get` tool: a message by its number, `{no,available,room_id?,room?,from?,body?,created_at?}`, from any room of the agent's own project (a private chat only for a member of it); another project's message and a number nothing carries both answer `{no,available:false}` with nothing else; 400 for a number that is not a positive integer |
| GET | `/api/agent/attachments/{attachment}` | agent key | file bytes only for a note delivered in the caller's live turn while still a room member; used by `attachment_get`; same download and security headers as the user attachment route |
| GET | `/api/agent/turns/{turn}/attachments/{attachment}` | turn-scoped attachment bearer | file bytes only for that live turn and current room membership; used by Pi Docker; same download and security headers as the user attachment route |
| POST | `/api/inbox/{item}/answer` | `{body,rev}` and `Idempotency-Key` header | answer message; 200 on keyed replay, 201 on first answer |
| POST | `/api/inbox/{item}/decision` | `{choice,rev}` and `Idempotency-Key` header | recovery or daily usage-limit decision `{item_id,choice,turn_id?,rev}`; 200 on keyed replay, 201 first time |
| POST | `/api/inbox/{item}/dismiss` | `{rev}` | dismiss an open delivery-failed notice (owner, operator, or DM peer), or a `usage_alert` notice (owner or global admin); 204 on success, 409 on stale revision or resolved delivery item, 404 for an absent usage alert |
| GET | `/api/system-events` | `limit?` (1–500, default 100), `notable=1`? | newest-first operational events the caller may see (`events`, same shape as the live stream) and the `kinds` list of the feed. Every event of the feed (here and on the live stream) carries `notable: true` when it is one the bell shows; `notable=1` returns only those, as the newest `limit` of them, so a burst of routine events cannot push them out of a page. What is notable is defined once in the store (`IsNotableSystemEvent`): a fixed list of kinds (spawns and their failures, an agent stopped or removed, lost or unstarted turns, held answers, the loop guard, credential alerts, usage limits and their clears, automatic account switches, a session reset, an admin changed, a user deactivated, a room that could not close, a machine revoked) plus two conditional ones, a `turn_process_exit` with a code other than 0 and an `agent_control_requested` that starts, stops or pauses (not the automatic pause of an account switch); the web keeps no list of its own; joins, spawns, recovery, quota, machine and access changes, never chat bookkeeping such as `message_created` or reactions. Each event carries `names` (id → display name: agents as `slug@project`, users by login) for the agent and user ids it mentions, so a client never has to show a raw id; ids that no longer resolve are omitted. Private chats only for their members; events with no project (user, machine and agent-key changes) are visible to the instance owner only, in this list and on the live stream (the per-login session and password-rehash bookkeeping is shown to nobody) |
| GET | `/api/rooms/{room}/export` | `format=json` or `format=markdown` (required; 400 for anything else) | the whole history of the room, oldest first, as a download named after the room (`<title>.json` or `<title>.md`, letters, digits, `-` and `_` only). Whoever may read the room may export it (same access as the messages route). JSON is `{room, exported_at, messages:[…]}` with each message as the messages route returns it (`reactions`, `thread_count`, `expirable` included), plus `author_label` (the login, `slug@project`, or `System`) and, for a reply, `reply_to_no`; an attachment's server path is never included, and neither is `reply_preview` (a reply has `reply_to_no` instead) nor `usage` (what a turn spent is read from the room's usage route, not copied into each message of an export). Markdown has a heading for the room and one per message (`## #no · author · time`), a quote naming the message a reply answers, a line for each attachment (name, type, size; the bytes are not included) and the reactions per emoji. Streamed in pages of 500, so only a small id-to-number map grows with the room. The first page is read before anything is sent (an error there is an ordinary 500); an error after that ends the connection abruptly, so a truncated file is a failed download and never one that ends cleanly |
| GET | `/api/rooms/{room}/usage` | `range=today` (default), `7d` or `all` (400 for anything else) | what the room's turns spent over the range: `{scope, scope_id, range, since?, turns, turns_with_cost, turns_with_estimated_cost, cost_coverage, tokens_in?, tokens_out?, tokens_cache_read?, tokens_cache_write?, cost?, partial?, by_agent:[{key,label,turns,cost_estimated?,…}], by_model:[…]}`. It sums `usage_record`, which holds each turn once, so a turn that wrote several messages is counted once. `today` is the server's local day, `7d` the last 168 hours. `cost_coverage` is `known` when every turn had a harness-reported cost, `estimated` when every turn had a cost and at least one used the dated model price list, `partial` when some had no cost, and `unknown` when none had one. `partial: true` means some turn ended before it reported its totals. A figure nobody reported is absent, never 0. Whoever may read the room may read it; the records go with the room when it is removed |
| GET | `/api/projects/{project}/usage` | same | the same for every room of the project; whoever belongs to the project may read it |
| GET | `/api/messages/{message}/debug` | — | debug inspector view of one message: the stored record (`message`, with `ask`, `proposed_answers`, `open_questions`, `visual_plan`, `appendix_pages` as parsed JSON), `addressed_to`, an `addressing` trace (for each @token: how it was read — group tag, full ID, slug, project-qualified, role, revoked, none — whether it was `addressed`, `ambiguous`, `no_match`, `ignored` (inside backticks, a code block, or escaped, and only when it would have reached someone, with `would_reach` naming them; code such as Java annotations is not reported), `self`, or `empty`, who it reached, and why; plus reply and private-chat sources, and a `drift` flag when re-reading the body against the room as it is now would address someone the recorded targets do not include; never set for an inbox answer, whose targets the answer fixes), per-agent wake `notes` (state, `wake_reason`, the turns that carried the note and their receipts), and related `events` (including `message_not_woken` reasons). Same room access as reading the message; long stored text, including the body, is truncated at 8 KiB on a character boundary; related events are those of the message's own room |
| GET | `/api/messages/{message}/reactions` | — | reaction entries |
| PUT/DELETE | `/api/messages/{message}/reactions/{emoji}` | — | 204; add/remove one Unicode emoji sequence for the user; DELETE accepts older stored reactions up to 128 bytes; unchanged reactions emit no event |
| PUT/DELETE | `/api/agent/messages/{message}/reactions/{emoji}` | agent key | `{message}` is a message id or its number (`#12` or `12`); 204; add/remove one Unicode emoji sequence in a joined room; DELETE accepts older stored reactions up to 128 bytes; unchanged reactions emit no event |
| GET | `/api/message-numbers/{no}` | — | where the message numbered `#no` lives: `{id,no,seq,room_id}`; the caller needs access to its room (404 when no message has the number, 400 for a number that is not a positive integer); the web UI uses it for a `#N` link into another room |
| GET | `/api/events?after=<seq>` | WebSocket upgrade | event stream: project events for project members (private chats for their members), events with no project for the instance owner only; owners and global admins also get transient `{type:"usages_updated"}` frames (no `seq`, never replayed) |
| GET | `/api/events/cursor` | — | `{seq}` current global event watermark |
| GET | `/api/inbox` | — | open questions, approvals, recovery, and delivery failures visible to authorized project members and private-chat peers; an `approval` entry with `note_id` is a shared agent note waiting for approval (only the owner and admins who own the agent's project see it; its card sits in the project's home room): `POST /api/inbox/{item}/decision` with choice `approve` switches the note on for every project's agents, `deny` deletes it, and either closes the card, as does any other change to the note |
| GET | `/api/agent/rooms` | active or not-accepting agent key | rooms where the caller is an active member, including archived rooms; no message previews |
| GET/POST | `/api/agent/rooms/{room}/schedules` | joined agent key; POST requires active lead, schedule fields above and `Idempotency-Key` | list or create schedules in a joined task room or the project home room |
| POST | `/api/agent/rooms` | active lead key; `{title,parent_id?,worktree?:{enabled,base_ref?}}` and `Idempotency-Key` | create task room beneath the home room by default or a joined parent, join the lead, and post one parent-room system notice |
| POST | `/api/agent/rooms/{room}/close` | active lead key; `{lifecycle_rev,cascade?}` and `Idempotency-Key` | begin the ordinary closing grace period for a joined task room; parent-room notice and agent actor audit |
| GET | `/api/agent/usage-left` | agent key | project live-agent and spawn-rate headroom plus provider session/weekly percent left and reset time; provider status is `unknown` when no snapshot is available |
| POST | `/api/agent/memory` | agent key; `{scope:"project"|"task"|"user_profile"|"agent_shared_notes",room_id?,scope_key?,body,source_message_id?}` | created fact, profile, shared note, or task note (a profile only for an active member of the agent's project, else 403; a shared note is created inactive and waits for the owner or an admin to switch it on, as does a change to one; an agent edits or deletes only the shared notes it wrote); source ID is accepted for project/global records only and must belong to a joined, memory-enabled room in the caller's project |
| GET | `/api/agent/memory` | agent key; `scope`, optional `room_id` or `scope_key` | list caller-visible records in one scope; project and global results include active records only |
| PUT/DELETE | `/api/agent/memory/{id}` | agent key; scope query as above; PUT `{body,rev}` or DELETE `?rev=…` | revised or removed record; stale revision returns `current` |
| GET | `/api/agent/recall` | agent key; `query`, optional `limit` (1–10) | ranked matching project facts, joined task notes, and joined room messages |
| GET | `/api/agent/macros` | agent key | read-only shared composer macro map (managed agent `list_macros`) |
| POST | `/api/agent/agents` | agent key; `{room_id,role_key,tool?,variant?,model?,provider?,machine_id?,reason?,worktree?,base_ref?}` and `Idempotency-Key` | lead-only individual agent spawn; omitted harness/model uses the next available role default, while an explicit deviation from an available default needs `reason` (`400 reason_required`) and appears in the room notice; a singleton role already held in the project answers `409 role_occupied`, and the agent or spawn rate limit `429 spawn_limit`. The response includes `default_used` and `default_selection` when applicable |
| POST | `/api/agent/agents/{id}/stop` | agent key; `{rev}` and `Idempotency-Key` | lead-only stop in the caller's project; system room notice and agent actor audit |

When a spawn specifies `machine_id`, role defaults on other machines are not
candidates. A matching choice with no `machine_id` inherits the default's
machine. If none on a requested machine can start, the caller must provide a
manual harness and model; the room receives at most one unavailable-default
warning per role per hour. A spawn using a default adds a short room notice.
For in-turn defaults, a competing cursor update returns `409
role_default_changed`; retry with the same idempotency key. An explicit
launcher with an omitted model is compared after resolving its harness default
model, so it counts as using the role default when they match.

Settings backup role entries include `default_agents` and `pick_mode`.
Restoring an older backup without those fields preserves the current defaults
of each existing role; an explicit empty `default_agents` list clears them.
Imported choices still require matching launcher IDs, listed models, and a
valid pick mode on the destination. The preview flags any invalid effective
defaults under `role_key.default_agents` while still counting the role for
import. Apply restores the role without invalid defaults, preserving valid
destination defaults when possible, and reports the skipped defaults as a
separate failed backup entry.

The import dry-run response has `digest` and `report`. A completed import job
also includes `report`. Both reports contain `imported`, `already_present`,
and `skipped` count maps. `skipped_memory` lists each excluded v1 memory record
as `{id,scope,scope_key,reason}`; it is empty when every selected record is
imported. If profiles mapped to one v2 user exceed the 2048-byte profile cap,
the wizard's Check error points to **Skip global user profiles**.

A role may **extend** another role by key. The instructions an agent receives are the parent's resolved body, a blank line, then the
role's own body, each trimmed; a role with no parent (or whose parent is gone) gives its own body untouched. A role cannot extend
itself or make a cycle, an `extends` that names no role is refused,
and a chain may hold at most five ancestors (all `400 invalid_extends`). A role that others extend cannot be deleted
(`409 role_extended`). The built-in specialist reviewers (`sec-reviewer`, `test-reviewer`, `ux-reviewer`) extend `reviewer` and hold only
their own focus; the dispatcher sends the resolved body, so their agents receive the same text as before. Migration 54 converts the
unedited ones and leaves an edited specialist as its owner made it. An unedited specialist follows the **current** `reviewer`, so editing `reviewer` changes what every unedited specialist's agents receive (the same as v1, where specialists were built on the reviewer text).

The web composer opens a quick-command popup only when the whole draft is
a single command typed from its first character (`/`, `/ar`, but not
` /ar`, `hi /ar`, `/ar now` or a command on a later line); it lists `/archive` only in an open task room where the user can
archive, and `/spawn` only when the Spawn button is available. `/archive`
confirms and explicitly archives through `POST /api/rooms/{room}/close`;
it never reopens a closed room. `/spawn` opens the Spawn dialog. The list
is navigable with ↑/↓/Enter/Esc and is a labelled listbox for assistive
technology. Macros are not slash commands: they stay on `<`.

Macros are a separate shared resource. A fresh install is seeded once with
the shipped defaults `do-cr` and `example_macro`; the seed is write-once per
key, so a default the owner edits or deletes is never re-created or
overwritten, an existing key keeps its body, and a key the v1 import ledger
already records is never re-seeded. Keys match `^[a-z][a-z0-9_-]*$`;
there are at most 256, and each body is at most 16 KiB in UTF-8 bytes.
Settings → Macros is read-only for other users and editable by the instance
owner or an admin. Typing `<KEY>` anywhere in the composer offers case-insensitive
prefix matches. Selecting one inserts expanded text into the editable draft;
nested references expand to depth 8, and `\<KEY>` inserts literal `<KEY>`.
Escapes also work inside macro bodies. Expansion stops before exceeding
16 Ki UTF-16 code units of inserted text and leaves remaining references literal. Unknown
references remain literal. Message send stores the draft verbatim.

Typing `@` after whitespace in the composer (main and thread) offers `@mention`
candidates for the open room, filtered by prefix and navigable with
↑/↓/Enter/Tab/Esc: group tags (`@everyone`, `@all`, `@channel`, `@here`,
`@humans`, `@ais`), then assigned role keys, then agent slugs from the room's
agent cards and human logins from `GET /api/rooms/{room}/mention-users`
(in an ordinary room that is every active project human, because they can read
it; a DM offers only its two peers). The login list is refetched when the room
changes, when the composer gains focus and the list is older than 30 s, and
immediately on a project `project_member_changed` / `user_invite_redeemed`
event. Agent
slugs and human logins share one namespace, as in server addressing: a role key
that is also a slug or login is not offered as a role, a colliding human login
is not offered, and when two agents share a slug the full `@slug@<project_id>`
form is inserted. Sent messages highlight a mention only when it resolves under
the same order (group tag, unique slug/login, `slug@<project id>`, else role
key). This is narrower than the server, which also accepts member-ID tokens and
`slug@<project name>`; those go unhighlighted. Fenced, inline and indented code,
link text, and `\@` escapes are never highlighted.

Project memory defaults off per project. Turning it off preserves saved facts
for the human viewer, but excludes them from agent turn context and recall and
blocks writes. Task notes stay available in task rooms regardless of that
setting. Owners/operators and active room agents may edit; viewers read only.
Each scope has a 4096-byte total body limit; one body is at most 2000 Unicode
characters. Updates and deletes require the current revision and return
`stale_rev` (409) if another edit won. A normal agent turn receives a
`knowledge` context object containing the project memory flag, enabled facts,
and notes for its task room. For a pi agent whose model name contains `gemma`
the object is trimmed to a shared 2048-byte budget of record bodies: the
record that crosses it is truncated and later records (profiles, shared notes,
task notes) are dropped. `memory_recall` returns at most ten matches,
returns `memory_disabled` while project memory is off, excludes rooms with a
restrictive memory override, and does not advance message cursors.
Global user profiles are keyed by v2 user ID and capped at 2048 bytes per
user; shared agent notes use scope key `global` and a 4096-byte cap. Both
retain the 2000-character body and revision rules. Humans with project access
can inspect them even while project memory is off; only the installation
owner or an admin can curate them as a human. Agents in memory-enabled projects can add
global records, and active records reach agents in every memory-enabled
project. Imported v1 records are ordinary and active on arrival. Profiles follow v1's trusted workspace model
and are visible across projects.

Roles are global only and defined by the instance owner or an admin; changing a role's capabilities needs the same authority.
Migration 64 retired project roles: a live project role became global when no global role had its key (the first by id when several
projects defined it) but without its lead, can-spawn or writer capabilities, which the owner sets again in Settings → Roles; any other was dropped with a log line and its agents moved to the global role of that key, and a deleted one stays
as history. A role used by a live agent cannot be deleted. Deleting a role preserves stopped agents' historical
cards, marks their role as deleted, and frees its key for a new definition;
those stopped agents cannot reactivate with the deleted role. A role key is
a live `@mention`, so creating a role whose key equals the slug of a live
agent or the login of an active human fails with
`409 role_name_collision`. Only live agents hold a
name: stopped and revoked agents do not, and a revoked agent that is still a
room member never hides `@role` from its live holders. New agent slugs skip
visible role keys for the same reason. 
An owner or operator can switch an active or paused agent to another model or tool
listed by the selected connected supervisor, from a room where it is an
active member and the lifecycle is open. The request uses the card's
`desired_rev` as `rev`; a stale revision fails with 409, an unlisted choice
with 400, and an unavailable adapter on that supervisor with 503. Omit
`machine_id` to stay on the current machine. To move an agent, set its target
machine and first map the project's folder there. A missing mapping returns
`machine_folder_missing` (409). The switch waits for the old turn to settle,
then moves the checkout and writer reservation, rotates the agent key, and
starts a fresh generation. The handoff warns that target files may differ.
The
response retains the same agent ID, role, and room membership, with a
`switch` object containing `state`, target `tool`, `variant`, and `model`.
The state is `draining` while the current turn finishes, then `starting`
while the fresh generation is spawned. An agent that was paused before the
request returns to `not_accepting` after the new spawn is confirmed. On
success, `switch` is omitted.
On spawn failure it has `state:"failed"` and a short `error`; the agent card
remains visible for another switch, including the same target, or manual
Stop/Reactivate. Either successful control clears the failed state so it
cannot block future dispatch. New messages queue
during the handoff. A system handoff message addressed to the agent includes
up to eight recent messages from one of its own joined rooms, never content
from another room. Replaying the same idempotency key returns the current
card and does not request a second switch. A manual Stop during a pending
switch cancels that switch.

Room responses include `lifecycle`, `gate`, `lifecycle_rev`, and `closed_at`
when closed. Owners and operators may close or reopen ordinary non-home rooms.
A private chat's human peer may close or reopen it regardless of project role;
the project owner may do so even when they are not the peer. Other members
cannot manage that private chat. Closing blocks dispatch immediately,
withdraws open questions and approvals, and
waits up to 30 seconds for running turns before requesting Stop for agents
spawned in that room. Agents merely joined to it keep running in their home
room. A dispatcher pass finishes the close, holds pending outbox messages
with `room_closed`, and preserves room history and recovery items. Reopening
restores the gate to the room's bulk control state without respawning agents
or restoring withdrawn inbox items. Both mutations reject a stale
`lifecycle_rev` with `stale_rev` (409).
Inbox entries include `room_lifecycle` so recovery items in closed rooms can
be labeled "in archived room".
Room and project control GETs return `desired`, `effective_state`, `rev`, and
persisted `failures` (`agent_id`, `action`, `error`) when a Pause or Stop has
not yet applied to an agent.
Only owners and operators may mutate them. Pause sets the existing dispatch
gate and transitions active agents to `not_accepting`; Stop requests ordinary
per-agent Stop. Room controls select current agents whose home room is that
room; non-home room gates pause dispatch without changing the agent. Private
DMs reject bulk mutations with `invalid_scope` (409). Each scope records
its own hold; Resume releases that hold and restores the agent only after all
overlapping room and project holds are gone. Agent revisions still distinguish
a later individual control, which takes precedence over bulk restoration.
Queued notes survive pauses. The project gate blocks dispatch in every room.
Each changed scope emits one audit event and one system notice (in the
project home room for project controls); replaying its idempotency key emits
neither. Per-agent Pause/Stop transition failures appear in ordered `results`
while the scope gate still changes. Failed Pause/Stop agents are recorded and retried by
repeating that action with a new key and the current scope revision, even when
the scope already has the requested state. A stopped process may need to
finish exiting before Resume can restore it. An early Resume returns 409 and
keeps the hold, failure records, and dispatch gate; retry with a new key and
current scope revision after that process exits.
Schedules are available in open task rooms and in the project home room (never
in a private chat or the lobby) and have at most 20 active entries per room. A
`target_lead` schedule goes to the room's lead, preferring an active one when
a stopped earlier lead is still a member; in the home room that is the project
lead. The note is limited to 2000 characters and 8000 bytes;
intervals are at least 300 seconds. `kind=once` uses `at` as an RFC3339
instant; `interval` uses `interval_seconds`; `daily` and `weekly` use
`local_time` (`HH:MM`) in the named IANA `timezone`, with weekly `weekday`
0=Sunday through 6=Saturday. Responses include UTC `next_run_at`, creator,
target, last outcome/reason, and revision. Pausing or resuming uses a matching
revision. Closing a room disables schedules, and reopening does not resume
them. A delayed server catches up one occurrence per schedule. While a gate
or target prevents delivery, runs are skipped, recorded, and surfaced by a
room notice on each changed skip reason. A spring DST gap advances to the
next valid local minute; a fall overlap runs at its first occurrence only.

Daily limits belong to a project and count the spend of all its rooms together;
there are no room limits. A project's cost and turn limits are each inherited
from the global default, its own value, or none. Usage-limit GETs return the
caps in force (`daily_cost_cap`, `daily_turn_cap`), `cost_mode`, `turn_mode`,
the inherited defaults, `daily_cost`, `daily_turns`,
`cost_coverage` (`known`, `estimated`, `partial`, or `unknown`),
`turns_with_estimated_cost`, `day_start`, `tz`,
`desired`, `effective_state`, and `rev`. The day begins at midnight in the
server's local time zone, including daylight-saving transitions. A turn
counts when it starts; cost is the sum of reported cost records dated in
that local day. A turn with unknown cost still counts toward the turn cap.
At 80% of either cap the server posts one notice and an informational inbox
item. At 100% it pauses dispatch for the project and opens a `usage_limit`
inbox item. Continue displays the raised cap, raises it to at least twice
the reached cap for that day, and clears only the budget-owned gate. It can
be chosen once per day. Abandon keeps the scope paused. At the next local
midnight the budget
gate clears, prior manual control remains in force, and the room receives a
reset notice. Project notices appear in its home room. Updating or removing
a cap above the day's measured usage also clears a budget pause. Saving a
project's limits clears that day's Continue raise; changing the global default
clears it only for the field a project inherits, and only when that field's
default changed.
An unresolved lost turn on a home agent can keep a room in `closing` until
reconciliation; the server emits one `room_close_blocked` event per blocked
agent with `reason:agent_stop_invalid_transition`.

The usage endpoint (the owner's whole snapshot, an admin's redacted one) reads the v2 cache under `AIMEBU2_CONFIG_DIR/usages/`.
Set `AIMEBU2_USAGES=0` to disable credential reads and background requests;
`/api/usages` then returns 503. Enabled providers refresh in the background
on the stored interval (120 seconds by default, minimum 15). The response
contains ordered, normalized profiles for Codex, Claude Code, GitHub Copilot,
Mistral, and Ollama Cloud. Copilot, Mistral, and Ollama each use one `local`
profile; Codex and Claude retain their account-switcher views. The endpoint
requires the global owner (the whole snapshot) or an admin (the redacted limits view) and never returns credential values.

Owner-only Settings routes use CSRF-protected POST requests:
`/api/usages/providers` changes a provider's enabled state;
`/api/usages/settings` changes refresh interval, percent display, provider order,
`alert_warning_remaining` and `alert_critical_remaining` (20 and 10 by default;
1–99, critical below warning), or the
`token_auto_refresh` setting (legacy alias `claude_auto_refresh`), `warmup_mode`
(`off`, `force`, `scheduled`, `smart`) and multiline `warmup_schedule`; see
[usages](usages.md#auto-refresh-tokens-account-maintenance) and
[warmup](usages.md#scheduled-idle-account-warmup-account-maintenance);
`/api/usages/refresh` fetches current snapshots;
`/api/usages/mistral/config` saves or clears a Cookie header;
`/api/usages/ollama/config` sets auth mode, API key, or Cookie header;
`/api/usages/copilot/login/start`, `/poll`, and `/logout` implement the
server-side GitHub device flow. The first poll response returns only status,
never the token. Secrets live in `usages/config.json` with mode 0600 and the
v1 JSON format. The cache and API responses contain no secrets. See
[usage snapshots](usages.md) for setup and provider behavior.
Fresh provider windows at either remaining-quota threshold add a virtual
`usage_alert` entry to `GET /api/inbox` for the owner and global admins. Its
question gives the provider, profile name, window, remaining percentage, and
reset time; it carries no email or credential. The owner alone can open the
Usages view from that entry. A critical alert supersedes a warning for the
same window, and each threshold fires once per reset cycle. The server stores
this dedupe state in private `usages/alerts.json` rather than SQLite. The
alert closes when its reset time passes, its account disappears, or the provider
is disabled. The Inbox card has a dismiss control; dismissing does not rearm
that threshold in the same cycle. A renamed stored profile keeps its opaque
account identity. Invalid saved thresholds fall back to 20/10 on load while
other saved usage settings and credentials remain intact.

Project responses set `repo_url` to `null` when absent. Message responses
expose `targets` as a JSON array and omit the raw `targets_json` column.
Project room lists and single-room details include read-only `member_count`
(active project humans, or a private chat's own people, plus room agents; a disabled
user counts nowhere), `message_count`, and an optional
`last_message_body` preview (at most 160 characters of the newest message that is not a system notice; absent when the room has only notices) and `last_activity_at` (the newest message's time, notices included, else the room's creation, which orders the room list). Both include the
signed-in user's `hidden`, `notify` (`default` until they choose) and `muted_until` (omitted unless a mute is set; it may be in the past) preferences, plus per-user read state:
`read_cursor` (highest read message `seq`), `unread_count` (messages after the
cursor; posting a message advances the author's own cursor to it; in a room whose `notify` is `mentions`, only the messages that address the user, which `POST /api/rooms/{room}/read` answers the same way), and `attention_count` (open inbox entries
the user can see in that room, the same set `GET /api/inbox` returns). Read
cursors are stored on the server (`room_read_cursor`), so unread state is
shared across browsers and devices; the web UI marks the active room read
through `POST /api/rooms/{room}/read` and shows a 🔔 on rooms
with open inbox entries. Migration 43 baselines existing rooms as read; a room or
user with no cursor row counts every message as unread.
The v1 import preview returns `rooms`, `humans`, `destination_users`, and
`memory_projects` as arrays, including `[]` when a category is empty, plus
the backed-up global `macros` map, `macro_defaults` keys,
`imported_macros` and `imported_roles` keys already in the
source ledger, source role previews, enabled host launcher choices,
`v1_memory_enabled`, and a skipped-settings list with reasons. Room previews
identify restrictive v1 memory overrides. Import maps may choose
`enable_project_memory` per destination project, and `role_conflicts` per key; the report lists role
conflicts plus skipped roles with reasons. v1 fleets are not imported and show
in the report's not-carried counts. Each room's `members` is likewise an array. The preview exposes
identifiers and counts rather than message or memory bodies; role and macro
text is shown for
owner conflict review.
Reaction entries include `member_name` for a readable tooltip while retaining
`member_id` for identity and toggling.
Room agent cards include optional `last_activity_at` (the newest event that is the
agent's own doing: its process starting or stopping, its turns, answers and
supervisor journal; not a message queued for it or a decision made about it, so
other people's posts do not move it) and `machine_last_seen` (latest supervisor
heartbeat). These are
different clocks; the latter does not prove that the agent itself was active.
Cards also include `health_flags`, each with `code` and optional `since` or
`reset_at`. Codes are `stuck`, `out_of_quota`, `machine_offline`,
`held_answer`, and `recovery_waiting`. `GET /api/health` returns the same
flags for every agent in a room visible to the caller, with `room_ids` for
room badges and one entry per agent for the overall count. Machine offline
uses the server's current supervisor connection, not its last heartbeat.
An agent is stuck when a running turn has no server-recorded activity or
usage for longer than `AIMEBU2_STUCK_MINUTES` (default 10). The UI refreshes
time-derived flags every 30 seconds. The top-bar health list shows one plain
sentence per flag ("Ada looks stuck", "An answer to Bert could not be delivered",
"Bert needs a recovery decision"); choosing one opens the matching Inbox item
(variant A) or the room with its inline callout (variant B) for `recovery_waiting`
and `held_answer`, and the agent's room for the other flags.
Listed room agent cards also carry an optional `hold` (`{code, text, detail}`) when an agent that is idle and accepting
work will not start its next turn because the limit guard is holding new turns of its tool: `code` is `limit_guard`, `text` the
short form ("claude session 95%": the tool, the fuller window of the active account and how full it is) and `detail` the sentence
behind it. The guard judges the host machine's account, so only agents on the host machine have it, and an agent that is working,
paused, stopped, lost or waiting on a recovery decision does not. A `hold` with `code` `docker_only` (text "native launcher refused") marks any agent that is not stopped on a native launcher while "Only run agents in docker" is on, whatever its machine or state. The card shows an agent that needs a recovery decision
(`recovery_block`) or has a `hold` as an orange dot with the state "needs decision" or "held: claude session 95%" and the reason on
the card, instead of the green "idle" of an agent that is free; a lost process stays red.
Listed room agent cards also carry an optional `warm` (`{state, sleeps_at}`) when the owner has set a warm window and the
agent is an active Claude agent on a machine that keeps processes warm: `state` is `warm` (with `sleeps_at`, RFC 3339, when its
process goes to sleep) while its process is kept up for the next message, `sleeping` when there is none, and the field is absent
while the agent is in a turn. The machine rows of `GET /api/machines` carry an optional `warm` (`{idle, busy, started, reused,
rss_bytes, container_wide_bytes, unconfirmed_ping_ends?}`; `unconfirmed_ping_ends` counts keep-alive pings in a container whose end was never confirmed
and whose quarantine ran out, present only when above 0) for a connected machine whose supervisor keeps processes warm; the memory is not per agent
(`rss_bytes` is the processes the supervisor runs itself, `container_wide_bytes` everything running in containers).
Listed room agent cards also carry an optional `keep_alive` for an active Claude agent on a machine that keeps processes warm, when the owner
has the cache keep-alive on: `{interval_minutes, pings_per_stretch, estimate_usd?, last_at?, last_ok?, last_error?, cache_read?, cache_write?,
missed?}` (`estimate_usd` is the cost of a stretch of pings, measured from the last ping when there is one, else the price list's; `missed` says the
last ping wrote to the cache what it should have read). `GET /api/agent-isolation` also returns `keep_alive: {enabled, claude_minutes,
codex_minutes, stop_after_hours}` (defaults on, 50, 10, 24), which the owner changes with `PUT {keep_alive: {...}}` (any of the fields; 0 minutes is
off for that harness; the bounds are 0 to 55 minutes for Claude, 0 to 1440 for Codex and 0 to 720 hours; another user gets 403).
Listed room agent cards also include optional `latest_usage` with nullable
`tokens_in`, `tokens_out`, `context_used`, `context_window`, and
`cost_so_far`, and `cost_estimated` when any of that cost used the price list.
Context values come from the latest reported usage in the agent's current
generation; cost is the sum of recorded turn costs for the agent.
For Claude Code, context used comes from the last assistant message's prompt
usage in the main thread when present; assistant events from sub-agents are
ignored. The result's usage can sum several model calls. The web
gauge caps its percentage label at 100% while retaining raw tokens in the
tooltip.
Absent values are unknown, not zero. Usage and compaction events refresh cards
through the live event stream. A compaction creates one system room message
with the reported before/after token counts; missing counts appear as `—`.
Inbox entries include `agent_id` when present. Recovery rows include
`turn_id` for uncertain turns and `diagnostic_report` when available. Rows
without a `turn_id` are loop budget items.
Posting an inbox answer resolves an open question or approval and queues the
requester in the same transaction. Project owners and operators may answer
questions; only project owners may answer ordinary approvals outside private
chats. In a private chat its human peer may answer either ordinary kind
regardless of project role. Boundary admission remains owner-only. A repeated
idempotency key returns the original message; a changed
answer with that key returns `conflict`. A stale revision returns `stale_rev`.
Uncertain-turn recovery items carry Resume, Retry, and Abandon; loop-budget
items carry Continue and Abandon. A turn that ended with an explicit error result
before it did anything (Claude Code `is_error` with no tool run and no output, such
as a failed sign-in) is a definite failure, not an uncertain outcome: no read-only
diagnostic is run for it, its item carries `definite_failure: true` and
`result_text` (the sentence the harness gave) next to the raw `evidence`, and its
options are Retry and Abandon only (Resume has nothing to find). The evidence line
for a harness's error output is labelled by that harness (`Claude Code error
result:`, `Codex error event:`, `Harness error event:`). A Retry or Resume that
fails the same way again opens an item whose question begins "Same failure again —
fix the cause first", instead of the cycle going round silently: the same failure is the
same `result_text` when both turns have one, else the same evidence when it says more than
an exit code (two unrelated failures that both exited with code 1 are not a repeat). The
decision endpoint refuses a choice the item was not stored with (`400 invalid_choice`),
so Resume of a definite failure is refused through the API as well as not offered. `choice` is the lowercase form of the
button label. Project owners and operators may decide recovery in every room.
The human peer can view a private-chat recovery item, but only a project owner
or operator may decide it. Owners and operators can see those recovery items
without joining the private chat because recovery blocks the agent across
rooms. The inbox row's `can_decide` field lets the UI show peer recovery as
read-only. Owners and operators receive recovery inbox events for those
private chats without receiving their ordinary room events or messages.
A folder-boundary approval uses the same decision endpoint with
`choice:"approve"|"deny"`. Only the project owner may decide it. The item
includes `boundary_pending`, `boundary_machine`, `boundary_path`, and
`boundary_requester`; agent cards carry `boundary_pending` until the request
is resolved and `boundary_grants` lists prior machine/path approvals. The
project machine picker reports `project_allowed`. The approval question says
that approval admits the machine for the whole project. Owners can see a
boundary approval opened from a private DM without joining that DM. Remapping
a machine folder withdraws pending approvals against its old mapping and
opens fresh requests for held transitions. An
approved remote machine is usable only after its project folder is mapped and
verified by a connected supervisor advertising path preflight support. An old
supervisor is rejected before a probe frame is sent. A missing host folder or
unverified remote folder returns `folder_unverified` before agent creation.

A stale `rev` or newer turn evidence returns `stale_rev` (409);
the same key replays the
original decision. New source-turn evidence increments the open item's `rev`,
so clients can reload and decide against the refreshed evidence. Recovery
rows include `agent_id`, `turn_id`, `turn_exec`,
`turn_outcome`, and `diagnostic_report` when available. A lost process item
warns that the old process may still be running and no diagnostic was run.
After Resume/Retry on a lost turn, another open item has
`recovery_phase:"quiescence_wait"` and shows the waiting state. Its choices
are Override (`choice:"override"`, explicitly accept the risk that the old
process can overlap and repeat an action) or `abandon`; it resolves
automatically only when the supervisor positively reports the process gone.
An absent journal entry after replacement is insufficient. The override actor
and risk are recorded.
Resume sends the original notes with evidence and a continuation instruction;
Retry sends the notes raw and may repeat an action. Abandon aborts those
notes without changing the agent's desired state. Continue resets the loop
budget and makes the agent active again. Resume/Retry while the agent is not
active return `invalid_transition` (409); Abandon remains available.
`auto_resume_after_clean_diagnostic` defaults to false. Enabling it can
repeat an action: a model's clean report may miss unobserved effects such as
a network write. The server uses the setting snapshotted at diagnostic
admission and records `system (auto-resume setting)` as the decision actor.
Delivery-failure items link to the room and include `held_reason` and a
redacted `delivery_error` from the parent or correction receipt, plus a
240-character candidate preview with credentials redacted. The originating
room receives a system notice with the fixed held reason when open;
the candidate preview remains visible only in the authorized Inbox. They
resolve only when an answer for the same originating turn commits. Neither kind
uses the inbox answer endpoint for questions and approvals.
Provider-limit failures recognized from Claude `limit reached`, Codex
`usage limit`, or a Pi, OpenCode or Vibe provider error naming a usage, weekly, hourly, daily or
monthly limit (a plain rate limit or 429 is not enough) process evidence are recorded as failed turns without starting
recovery. Their attempted notes return to the queue. If the turn reported
activity beyond startup or usage, those notes carry `retry_notice` telling
the agent to check the state before acting; a limit before the first tool
call can retry without the notice. Later queued work waits behind the same
provider-limit gate. An owner or operator receives one informational inbox
item with a `Retry now` action. A parsed reset time appears on the item and
health flag and clears the gate automatically when reached. Reactivate,
Switch, or Retry now clears a limit without a known reset. A general
rate-limit error does not use this path.

Agent cards contain `id`, `slug`, `room_id`, `project_id`, `role_key`,
`role_emoji`, `role_deleted` for a stopped agent whose role was deleted, and
`role_updated` when its role was deleted and the key now has a
different definition, plus `tool`,
`variant`, `model`, `provider`, `machine_id`, `desired`, `desired_rev`,
`observed`, `generation`, and `recovery_block`. Spawn and control require a
project owner or operator. Spawn requires a connected supervisor advertising
the chosen tool and variant. Model choices are bounded by the server, while
CLI version is reported separately by the spawn catalog. Spawn respects
project live-agent and per-minute spawn limits, and reserves the checkout
for a worker. A second worker (a role with the singleton-writer capability)
in a room whose checkout already has one is not refused: it is spawned in a Git
worktree of its own, `<project>/.aimebu/worktrees/<room-id>-<slug>` on branch
`aimebu/<room-id>-<slug>`, made from the room's base ref (the project's HEAD
when the room has none), and the first writer keeps the room's folder. When it
cannot be made (the project's `worktree_mode` is `off`, the folder is not the
root of a Git repository, or the supervisor refuses) the spawn answers
`writer_occupied` (409). When the holder is known, the body also carries
`writer_agent_id`, `writer_slug` and `path`, and the message names them
("alice@proj is the writer of /path — stop it, or give this agent a non-writer
role (it cannot get a worktree of its own: …)"); the Spawn dialog stays open on that answer, names the holder, and offers
stopping it or a role that does not write (a continued session also gets "Work on a copy"). When the dialog already sees a live writer of the room on that
machine and no worktree can be made, it says so before anything is sent, keeps Spawn disabled, and offers the same two ways on under the warning; a reactivate, switch or role
change refused for the same reason answers the same way (a room or project **resume**
starts such a writer in a worktree of its own like a single Reactivate does, the
worktrees being made before the resume, so one blocked writer does not leave the scope stopped). An agent card of a
singleton-writer role carries `writer_blocked_by` (`agent_id`, `slug`, `holder`
as slug@project, `path`, and `own_worktree` when the project's `worktree_mode`
is not `off`, so starting it moves it to a worktree of its own) while another
agent holds its checkout's writer slot.
A repeated idempotency key returns the same agent;
using it for a different request returns `idempotency_conflict` (409).
Agent toolbox requests require a current, unrevoked, not-stopped agent key.
Claude/Codex Docker and native Pi use `POST /api/agent/mcp` with the same
bearer key for streamable HTTP MCP. Pi Docker has no private credential
channel and runs without this toolbox. JSON-RPC `initialize`, `ping`,
`tools/list`, and `tools/call` map to the same narrow tools as the stdio
transport;
notifications return HTTP 202.
The key alone identifies the caller. Room and agent control mutations require
an active lead role; `spawn_agent` also requires the role's spawn capability.
`POST /api/agent/dms` (`{target_agent_id,body}`, `Idempotency-Key` required; the
`dm_agent` tool, active lead only) sends a private message to any agent in any
project and returns `{room_id,message_id,no,created}`. Who may is decided by the
server in the same transaction, never the model: every person whose message
caused the lead's current turn (and the turns it continues) must be an owner or
operator of the target's project, or the instance owner; when no person caused
it (an agent or a schedule woke the lead) an owner of the lead's own project
must be. A viewer never counts. Unknown or out-of-reach targets answer 403
alike; 409 `agent_unavailable` (revoked, stopped or not accepting), 409
`project_archived`, 429 `dm_turn_cap` (30 agent turns per chat per hour) and
429 `dm_fanout_cap` (10 new chats per lead per hour); see
[architecture](architecture.md#chats-between-agents).
`POST /api/agent/human-dms` (`{target_user,body}`, `Idempotency-Key` required; the
`dm_human` tool, **any active agent**) sends a private message to a person of the
agent's own project (owner, operator or viewer; `target_user` is their login or
user id) and returns `{room_id,message_id,no,created}`. It opens or finds the
ordinary private chat between that person and the agent (the one the person opens
from the agent's card), and the person's reply wakes the agent in it. The server
decides in the same transaction, never the model: a person who does not exist, is
disabled or is not in the agent's project is 403 alike, so a login cannot be probed;
409 `project_archived`; 429 `human_dm_cap` (20 messages from one agent into one chat
per hour) and 429 `human_dm_fanout_cap` (10 new chats per agent per hour). Agent to
agent stays `dm_agent`, lead only.
`create_room` records the agent in `created_by`, joins the lead atomically (a project's lead is in every open room of its project: a new room, made by anyone, gets the lead, and a lead spawned later joins the existing rooms; private chats are not included), and
posts one system notice in the parent room. `close_room` uses the same
`lifecycle_rev`, child-room check, cascade option, grace period and control
delivery as human archive. Repeated request keys replay the original result
without a second notice. Workers can list their rooms, check usage, and use
memory, but cannot manage rooms and agents.
The built-in lead role coordinates a home-room request through a new task room:
it frames the evidence and plan first, asks the human which agents to use
through a structured inbox question, spawns them one at a time, follows their
handoffs and independent reviews, summarizes in the home room, and archives
the task room. The lead never implements: its instructions forbid it to edit
files or make any change itself, however small, and say that every change goes
through a task room and `spawn_agent`; when no worker can be spawned it tells
the human what is blocked and asks how to proceed instead of doing the work
(this is guidance in the role body, not a permission the server enforces).
Built-in worker
and reviewer roles call for independent plans, concrete verification, and
evidence in the final room message. Their instructions are updated on upgrade
only while the global default body is untouched; project and edited global
role bodies remain under their owners' control.
After spawning, an assignment needs a live agent mention to wake its recipient;
code-formatted names do not count. An agent's `reply_to` another agent's message
wakes that agent (wake reason `reply`), but only while the room has seen fewer
than four such wakes (`ReplyWakeHops`) since the last message from a person,
within 30 minutes: a later reply is threaded and wakes nobody (a reply to an
older message does not start the count again), and one `loop_guard` event and
one system notice say so, once per run. A mention or handoff is not limited by
this, and a reply never replaces the mention, role or group reason that already
targets its parent's author (nor does a self-reply count). Agent replies to
humans still target them, and human replies still wake their parent author. An
agent card's `woken_by` is `reply` while its running turn began that way.
Acknowledgements belong in reactions,
and the lead should publish the home-room summary as soon as review passes.
The question is a room message with
`ask:{kind:"question",text:"Which agents should take this task?",options:["…"],free_text:true}`;
plain question text does not create an inbox item. A task room returned by
`create_room` can be targeted in the same turn after the tool succeeds.
`go test ./internal/server -run TestFakeLeadCoordinatesTaskFromHomeToArchive`
exercises that flow with scripted fake turns. The MCP `spawn_agent` tool takes `role_key`, defaults the harness and model where configured, and accepts optional `machine_id` and `reason` for a deliberate deviation;
it spawns one agent per call. A project
room may also request `worktree:true` and optional `base_ref` on
`create_room` or `spawn_agent`; a new agent then uses the room's worktree
checkout on its selected machine. A dirty base contributes only its changed
file count to the API response and a room notice; uncommitted content stays
in the base checkout.
The lead must currently be `active` to spawn or stop. An agent lead cannot
spawn another lead or stop a different lead; it may stop itself. Notices use
agent slugs and roles, for example `tessa (lead) stopped drew (worker)`.
Provider quota uses the active profile snapshot for the caller's tool. A
cached or failed-refresh snapshot with windows is marked `stale`; missing
windows return `unknown`. It does not return credential or token material.
The `claude` tool uses the `claude-code` provider snapshot.
If spawn delivery fails or its acknowledgment is lost, the same key retries
the original agent's spawn until the supervisor confirms it. The API returns
`machine_unavailable` (503) while that machine is disconnected.
Control accepts `active`, `not_accepting`, or `stopped` with the current
`desired_rev` as `rev`. Stopping a running agent reports `observed` as
`interrupting` until its process exit is confirmed; an idle agent stops
immediately. Reactivating a stopped agent starts a new generation.
Control with an outdated `rev` returns `stale_rev` (409).
Reactivating a paused agent, or resuming a held room or project, checks the
saved launcher's enabled setting and Docker bind without requiring a connected
supervisor. The desired state reaches the supervisor on reconnect. A stopped
remote agent still needs a connected supervisor for checkout path verification.
Stopping a turn that was prepared but never sent cancels and settles it,
leaving its notes queued for later reactivation. A dispatched pending turn
keeps the writer reservation until journal reconciliation proves it never
started or a process exit confirms it stopped. A lost process cannot be
stopped through this endpoint until recovery resolves it. Reactivation also
checks the project's live-agent limit.

One concept, the reply: a message links to the one it answers with
`reply_to`, any message in the same room (a reply to a reply is fine). Its
`thread_root` is derived, the first message of the conversation, and comes back
in every message view; a writer never chooses it. A `reply_to` that is missing
or in another room returns `invalid_answer` (400). An inbox answer replies to
the ask, so it lands in the ask's conversation.

An agent's single plain answer to the one person's message that woke its turn gets that message as `reply_to` when the agent named none; see the contract's agent I/O rules.

An agent is told what a message it receives answers: each delivered note says
why it reached the agent (`why`) and carries the message it replies to and the
first message of its conversation (`in_reply_to`, `conversation`: number,
author, an excerpt, `yours`), and the prompt asks the agent to set `reply_to`
when its message answers one (see docs/contract.md §4.4).

`/api/events` requires a same-origin `Origin` header (or the configured
`AIMEBU2_PUBLIC_URL` origin) and a valid session.
The web UI reads the authenticated `/api/events/cursor` before its HTTP
snapshots, then opens the stream after that `seq`. Events committed during
the snapshot are caught without scanning older history. What the client does
with the replay before `ready` depends on the connection. One opened straight
after a snapshot (the first of a page load, and the one after
`snapshot_required`) replays only what was committed since that snapshot's
cursor, up to 100 events, and the client applies those like live events, in
order. Any other connection (after a drop, when anything may have been missed)
and a replay of more than 100 events are coalesced into one silent snapshot
refresh on `ready`, so historical inbox items do not trigger a chime or
browser notification, and the open room is brought up to date where it is
rather than opened again.
`message_created` carries `message_id`, `author_kind`, `author_id` and `targets` (the ids of the members the message addresses, an empty list when none), so a client can decide whether the message is worth a sound or a notification, by the room's `notify` level and `muted_until`, without fetching it.
If that refresh fails, reconnect replays from the last processed `seq`
until a snapshot succeeds. After a disconnect it resumes from the last
processed `seq`.
The initial replay sends visible project events with `seq > after` in order,
using bounded batches and rechecking session validity periodically during a
long replay,
then `{type:"ready",seq:<high-watermark>,heartbeat:<seconds>,version:<build>}`. Live frames use
`{type:"event",event:{...}}`; the event's `seq` is the global cursor.
After `ready` the server also sends `{type:"heartbeat"}` every `heartbeat` seconds (20; no `seq`, nothing to
replay), whether or not anything happened. A page cannot tell a socket that died silently (a sleeping laptop,
a proxy that dropped an idle connection) from a quiet room, so the web UI replaces a socket that has said
nothing for three heartbeats, and checks it at once when the tab becomes visible or the browser comes back
online; a server that sends no `heartbeat` is never judged by silence. `version` is the build serving the
socket (what `GET /api/buildinfo` calls `version`): a page that finds a different one after a reconnect was
loaded from an older build, and shows a bar asking for a reload (it reloads by itself once the tab is hidden
and no dialog is open and no field holds text).
If the cursor predates retained events, the server sends
`{type:"snapshot_required",min_seq:<oldest>}` and closes the socket.
The client should reload its project and room snapshots, then reconnect
from the new high-watermark. A bounded queue disconnects slow clients;
database writes never wait for them. Poll failures close the socket so clients
can reconnect; transient SQLite busy errors are retried. Open sockets recheck
their sessions every 30 seconds without renewing expiry and close after
logout, expiry, or revocation. Database errors during a session check close
the socket with an internal-error code so clients reconnect.

The separate supervisor uses `GET /api/machine` as a WebSocket endpoint.
Attachment bytes stay in the server's storage; the supervisor never stages
them in a checkout. Uploads are bound to a room immediately and one message
on send; up to four files allow a file-only message. Unbound uploads expire
after 24 hours. The Pi Docker turn bearer is derived for one turn, is absent
from journals and receipts, and stops working when the turn ends or room
membership is removed.
It authenticates with `X-Machine-ID` and `Authorization: Bearer <machine-key>`
from the private host key file; browser sessions cannot use that endpoint.
See [the supervisor guide](supervisor.md).

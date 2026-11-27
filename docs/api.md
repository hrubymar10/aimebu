# HTTP API

Run `aimebu server`. On a first run it prints a one-time owner setup token
(also in `.setup_token`, with a `/setup#token=…` shortcut, see [auth](auth.md#first-run-owner-setup)), or bootstraps
the owner from `AIMEBU_OWNER_LOGIN` and `AIMEBU_OWNER_PASSWORD` when both are set. The listener defaults to
`127.0.0.1:9997`; `AIMEBU_BIND` must be an IP literal and `AIMEBU_PORT` a
TCP port. The default data root is `~/.aimebu`; use `AIMEBU_CONFIG_DIR` to
choose another. The v2 server ignores v1's `AIMEBU_*` environment settings.
`AIMEBU_ALLOW` accepts comma-separated IPs and CIDRs and defaults to
`127.0.0.0/8,::1/128`. Bare addresses become `/32` (IPv4) or `/128` (IPv6).
IPv4-mapped CIDRs are rejected; use their IPv4 form instead. The embedded
supervisor's own dial address is added to the effective list when necessary,
including for a listener bound to a specific interface IP.
The allowlist gates every route, including the `/api/machine` WebSocket;
unlisted sources receive HTTP 403 before authentication. It checks the direct
peer address (including IPv4-mapped IPv6 addresses) and ignores
`X-Forwarded-For`. Invalid entries fail startup. Startup logs the effective
allowlist and warns if `AIMEBU_BIND` is non-loopback while the default
loopback-only allowlist is active. For remote access, set both the bind and
the allowed source network.
When `AIMEBU_TLS_CERT` and `AIMEBU_TLS_KEY` are both set, the server also
listens with HTTPS on `AIMEBU_TLS_PORT` (default `9996`). HTTP remains on its
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
use `not_found`, and authorization uses `forbidden`. A JSON body over a route's
size limit is `413 request_too_large` with the limit in the message, never a
`400 invalid JSON`. Domain-specific errors
use codes such as those in [the contract](contract.md#9-error-codes), which lists
the main ones; routes also return others, named in the rows below where they
apply to one route.
Resources that belong to different projects or machines return
`409 scope_mismatch`; a spawn whose selected checkout has the wrong scope
returns `409 checkout_scope_mismatch`.

The embedded web UI is served at `/` with an SPA fallback for extensionless
non-API paths. It uses the endpoints below, keeps the CSRF token in memory,
and reconnects the event stream from its last seen `seq`.

| Method | Path | Body or query | Result |
| --- | --- | --- | --- |
| GET | `/api/setup/status` | — | while a setup token waits: `{open:true, token_path}`, the file that holds it (the full path for a loopback connection with no forwarding header, `<config dir>/.setup_token` for any other; never the token); `200 {open:false}` once an owner exists |
| POST | `/api/setup/token` | `{token}` | checks a setup token without spending it (204; 403 wrong; 429 throttled; 404 once an owner exists) |
| POST | `/api/setup` | `{token,login,password}` | first-run owner setup (no session needed; 404 once an owner exists, 403 wrong token, 400 bad login/password, 429 throttled); creates the owner and signs in, same body as `/api/login` |
| GET | `/api/me/preferences` | none | `{preferences:{name:value}}`: the signed-in user's personal interface preferences |
| PUT | `/api/me/preferences` | `{preferences:{name:value}}` | stores them and returns all; names: `theme`, `inbox_muted`, `inbox_sound`, `inbox_volume`, `system_events`, `debug_inspector`, `markdown_mode` (`raw` or `rendered`: how message bodies show), `composer_formatting`, `sidebar_sort` (`name`, `activity` or `manual`), `sidebar_project_order` (a JSON list of project ids, up to 4096 bytes), `harness_setup_skipped` (`1` once an owner or admin skipped the offer to set up a harness, `0` or absent otherwise; the web app sets it back to `0` when it finds a harness with a model), `notification_banner_dismissed` (`1` once the person dismissed the bar that offers browser notifications), `sidebar_preview` (`1` to show the last message under every room in the room list; otherwise only the open room shows it) `notification_text` (`1` when desktop notifications for chat messages show the start of the message text; off by default) (400 for others or values over 256 bytes; the retired `harness_layout`, `thread_order`, `message_layout` and `sidebar_status_variant` are accepted and dropped, so an older page's save is not refused, and a value stored under one is still read back). The browser keeps only a cache for painting before sign-in; per-device layout (collapsed panels) is not stored |
| POST | `/api/login` | `{login,password}` | user ID, CSRF token, expiry, `first_run`, `owner`/`admin` and, for those two, `host_home` (see `/api/me`) |
| POST | `/api/logout` | — | 204; revokes session |
| POST | `/api/presence` | `{}` | 204; the signed-in browser's presence heartbeat (CSRF token required, like any write). The web app sends it only while its page is visible and the person has interacted with it within the last minute, at most once a minute. It is how a person stays online while only reading: any request that changes something counts as activity by itself, but background reads do not, so an idle reader would otherwise go offline mid-page. See the presence rule at `/api/rooms/{room}/members` |
| GET | `/api/session` | — | no session needed: what `/api/me` returns for a signed-in account, and `{signed_in:false}` (status 200) otherwise; the page asks this on load so the sign-in screen costs no 401 |
| GET | `/api/me` | — | user ID, login, `telemetry_prompt` (true for the owner until the telemetry popup was answered; never when `AIMEBU_SENTRY_DSN` is set), CSRF token, `owner` and `admin` (what the account may do instance-wide: the web turns off New project and the owner/admin settings tabs for anyone else), `host_home` (only for the owner and admins, who create projects: the absolute home folder of the user the server runs as, which an empty "Workspace folder" field starts at; nobody else is told a path of the host), and `first_run`: true only for the instance owner and only until they press Continue on the first-run welcome (a database that already had people when the welcome migration ran counts as seen, and one made later, including by the v1 import, does not), so the web app shows the welcome once, between sign-in and the app |
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
| POST | `/api/invites/redeem` | `{token,login?,password?}` | create account, grant signed-in account project access, or reset password; returns stored `login`; signed-in callers send CSRF token; a new login equal (any case) to the slug of a **live** agent in any project is refused with 409 `login_in_use` and the generic message "that login is taken" (no agent slug, no project name: the caller may not be signed in), which counts toward the invite redeem throttle like a wrong token. A live agent is one that is not removed and not stopped for at least 24 hours with its home room closed, so a freed agent name can become a person's login. There is no endpoint that renames a person or an agent; if one is added it must make the same check |
| PUT/DELETE | `/api/projects/{project}/members/{user}` | PUT: `{role}` | project owner (or the owner / an admin, for any project) changes role or removes member; last active owner stays; an admin never changes the owner's or another admin's membership |
| PUT | `/api/users/{user}/admin` | `{admin:boolean}` | owner only: gives or takes away the admin flag (400 for the owner or for a deactivated account when appointing, 404 unknown account); emits `user_admin_changed` |
| POST | `/api/users/{user}/deactivate` | — | the owner, or an admin for an ordinary account, blocks sign-in and revokes sessions; never the owner, and an admin cannot deactivate another admin |
| GET | `/api/prompts` | `project_id?` | every overridable prompt slot as seen from the scope (`key`, `title`, `desc`, `default`, `effective`, `source` = `default`/`global`/`project`, and the saved `global` / `project` override with its `rev`), plus `max` (2000), `inline_plan_appendix`, `can_edit` (global: the owner or an admin; project: its owner or operators) and `can_edit_global` (the owner or an admin); a project view needs access to that project |
| PUT | `/api/prompts/{key}` | `{project_id?, value, rev}` | saves one override (`rev` 0 creates, otherwise the current revision); 204; `400` for an unknown slot or an empty or over-2000-character value, `403`, `409 stale_rev` |
| DELETE | `/api/prompts/{key}` | `?project_id=&rev=` | removes one override, so the next level down (or the shipped text) applies; 204, `409 stale_rev`, `404` when there is none |
| PUT | `/api/inline-plans` | `{value: "always"\|"optional"}` | owner or admin; whether a lead's instructions always ask for the full plan as appendix pages (default `optional`); 204 |
| GET/PUT | `/api/agent-isolation` | PUT: `{docker_only?,reviewer_agents_enabled?}` | instance agent settings: Docker-only defaults off and reviewer agents default on; `{docker_only,reviewer_agents_enabled,can_edit}`. Any signed-in user reads it; owner or admin writes it. With Docker-only on, native launches answer 409 `docker_only` (see [the supervisor](supervisor.md)) |
| GET | `/api/retention` | — | the message-expiry window (`messages_considered_expired_after_seconds`, default 21600, `0` = never, otherwise 60 to 2592000) with its default and bounds; any signed-in user |
| PUT | `/api/retention` | `{messages_considered_expired_after_seconds}` | owner or admin; 400 outside the bounds. Expiry only hides old messages from what an AI is given; it never deletes one |
| GET | `/api/credentials/targets` | — | owner-only token state of the stored profiles and live files of a migrated install (`enabled`, `targets`, `alerts`); never a token or CLI output. An alert of kind `refresher_conflict` means another refresher owns that tool's live credential file (it holds the claim lock, or its switcher root's token-copy marker names the same file): this server does not refresh the tool until it is gone |
| GET | `/api/quota-switcher` | — | owner-only automatic account switching: per-tool config, state, last decision and the newest 50; 503 until it runs (see [quota-switcher](quota-switcher.md)) |
| PUT | `/api/quota-switcher/{tool}` | config | owner replaces a tool's configuration (400 on validation, 404 unknown tool). A `dry_run` field (removed with dry run itself) is **ignored with a logged warning**, so an old client does not break; the view never carries it |
| POST | `/api/quota-switcher/{tool}/rearm`, `/reenable` | `{}` | owner clears the after-reset latch / the circuit breaker |
| GET | `/api/limit-guard` | — | owner-only limit guard: per tool `{config, saved, result, held, since}` under `tools`, plus `profiles` and `percent_display` (`left` or `used`, the owner's Usages setting, which Health and Settings use to word the percentages); 503 until the quota switcher runs (see [quota-switcher](quota-switcher.md#limit-guard-acting-before-100)) |
| PUT | `/api/limit-guard/{tool}` | config | owner replaces a tool's soft/hard thresholds and per-profile overrides (400 on validation, 404 unknown tool) |
| GET | `/api/usages` | — | provider usage snapshots for Settings → Server → Accounts & usage. The owner reads all of it; a global admin reads the limits only (`usages.Response.ForAdmin`: windows, resets, plan, credits, status, `error_kind`, `retry_at` and the profile names, but no e-mail, Enterprise host, error text, credential alerts, token-refresh state or configured-secret flags); anyone else gets 403. A failed profile carries `error_kind` (`rate_limited` for HTTP 429, `fetch_failed` for any other failed fetch, `token_expired`), which the web app reads instead of the error text. A profile that is backing off after a failed fetch carries `retry_at` (RFC 3339, when it is fetched next); the web app shows it in local time on the stale line. A window whose series has the gap a rejected login left carries `rejected_at` (unix seconds, the first rejection of the profile's current or last episode, while it is inside the 48-hour history span; omitted otherwise), which the chart marks on that series. Every write stays the owner's |
| POST | `/api/usages/refresh` | `{provider?}` | owner-only forced refresh; `429` + `Retry-After` and `retry_after_sec` inside the 15-second cooldown (error code `cooldown`); `400 invalid_provider` for an unknown provider |
| GET | `/api/switcher` | — | owner-only enabled state, profiles, eligibility, and host agents affected by a switch. `affected_agents` maps each tool to its host agents; each entry keeps `id`, `slug`, `tool`, `desired`, `observed`, and `running`, and adds `name`, `role` (role key), and `room: {id,name}` for the agent's own room (`null` if unset). The switch dialog counts each agent once under its own room and role. |
| PUT | `/api/switcher` | `{enabled}` | owner enables or disables profile switching |
| POST | `/api/switcher/{tool}/profiles` | `{name}` | owner adds an empty profile |
| POST | `/api/switcher/{tool}/import` | `{name}` | owner adopts the current live login |
| POST | `/api/switcher/{tool}/use` | `{name, force?}` | owner switches profiles; `force:true` requires stored credentials and skips incoming refresh while keeping the normal swap; a switch never waits for agents (running turns keep their token, warm processes are retired in the background) |
| DELETE | `/api/switcher/{tool}/profiles/{name}` | — | owner removes a profile; active removal clears the live login |
| POST | `/api/switcher/{tool}/profiles/{name}/rename` | `{name}` | owner renames a profile, active or not; the live login is untouched; 404 unknown profile, 409 destination exists |
| GET | `/api/health` | — | visible agents with `room_ids` and current health flags, plus the configured stuck threshold |
| GET | `/api/import-v1/status` | — | owner-only first-run status `{state, source_key?, ledger_rows?, job, reservation_missed?}`: `state` is `none`, `backup_incomplete`, `ready`, `resume`, `skipped`, or `done`; skipping or finishing closes the import path, and an install that already has messages reports `skipped`; `409 backup_changed` when the v1 backup was modified after it was made (it can no longer be verified, so it is not imported; the web wizard shows the message with the Skip choice, and restoring a pristine copy of `.old-v1-bkp` makes the status answer normally again); `reservation_missed` is true when an event already took a number inside the v1 message-number range, which makes the import refuse (the preview carries the same flag) |
| GET | `/api/import-v1/preview` | — | owner-only source identifiers, counts, destination choices, and suggested mapping (`409 source_unavailable` when the v1 data cannot be read), including enabled host launchers, and `carried_over_files` when v1 external credential targets are present in v2. The suggested people mapping maps the v1 person with the owner's name, or the one person in the data when there is only one (including human agent records, read cursors, and room preferences), to the owner, and gives every other person a new viewer account (`suggested.new_users`); `human_stakes` counts per person what a skip would drop (`memory_profiles`, `reactions`, `read_positions`, `room_view`) |
| POST | `/api/import-v1/dry-run` | the import mapping itself (as returned in `suggested`, plus any edits) | owner-only validation and import report without a commit; `200 {digest, report}`. The report lists `carried_over_files` when present; the digest names these exact choices and is what `commit` needs; `400 invalid_mapping` for a mapping that does not validate, `409 import_running` while an import is in progress |
| POST | `/api/import-v1/commit` | `{mapping, digest}` with the digest from the latest dry run of the same mapping | owner-only; starts the import in the background and answers `202 {job_id, phase:"scan"}`. `409 dry_run_required` when the digest is missing, stale or for other choices, `409 import_running` while one is in progress, `400 invalid_mapping` for a mapping that does not validate |
| POST | `/api/import-v1/skip` | — | owner-only first-run choice; 204, permanently closes import (`409 import_running` while an import is in progress); preview, dry run, and commit return 410 `import_closed` afterwards, and so does a skip after a completed import (a repeated skip of a skipped import stays a 204) |
| GET | `/api/import-v1/jobs/{id}` | — | owner-only job progress for a `job_id`: `{id, phase, error?, report?, invites?, invite_error?}` (`invites` are the one-time reset links of the accounts the import made: `{login, url, expires_at}`, each valid an hour and shown only here; `invite_error` says why they are missing). `phase` runs `scan`, `validate`, `copy`, `commit`, then `done` or `error`; `report` appears once the import has written. 404 for an unknown id |
| GET/POST | `/api/projects` | POST (owner or admin): `{name,folder,repo_url?}`; `folder` must be an absolute path to an existing folder on the machine running the server (`invalid_folder`) | accessible projects, each with the caller's `role` (`owner`, `operator` or `viewer`), `folder_in_use` (true while agents or live worktrees use its folder), and `archived_at` when it is archived / created project: the response includes `role:"owner"` because the creator becomes its owner member and picks its folder on the host, the same for an admin as for the owner |
| GET | `/api/team-flows` | — | `{templates:[{key,label,description,recommendation,diagram:{rooms:[{key,label,roles}],edges:[{from,to}]},rooms:[{key,title,kind,restricted}],join_rules:[{room_kind,allowed_roles}],spawn_rules:[{actor_role,allowed_roles}],auto_join_roles:[role_key],core_roles:[role_key],task_parent_key,grant_roles:[role_key],role_text_overrides:{role_key:text}}]}`; keys appear in order: `leader-workers`, `leader-dispatcher`, `leader-advisor`, then `custom` last. Core roles are leader for A/D, leader plus dispatcher for B, and leader plus advisor for C |
| GET | `/api/team-flows/recommended-models` | optional `machine_id` and `launcher_id` query parameters together | `{rows:[{role_key,flow_key,tool,models:[{model,why}]}]}`; the full table includes strong, fast, and top tier IDs (Custom has no rows). With a selected machine and launcher, only models in that launcher's live catalog are returned, and empty rows are omitted |
| GET | `/api/projects/{project}/team-flow` | — | `{key,rev,review_pending,template,conflicts}`; new projects have an empty key, migrated projects have `custom` with `review_pending:true`. Any project member may read it; viewers see only conflicts in rooms they can access |
| POST | `/api/projects/{project}/team-flow/preview` | `{key}` | Owner/operator only. Returns `{key,create_rooms,preserved_restricted_rooms:[{room_id,title}],role_changes,conflicts}` without mutation; each conflict includes `agent_id`, `slug`, `role_key`, `role_label`, `room_id`, `room_title`, and a reason. Preserved rooms stay restricted after the switch |
| PUT | `/api/projects/{project}/team-flow` | `{key,rev,remove_memberships?:[{agent_id,room_id}]}` | owner/operator applies the flow atomically and answers `{key,rev,review_pending:false,created_rooms,conflicts}`; `custom` also acknowledges a migrated setup. Membership removal is opt-in: unselected conflicts remain listed and joined. A selected pair must still be a current conflict, or the request returns 409 `stale_conflict` with no change. A stale revision returns 409 `stale_rev`. No rooms, messages, or agents are deleted; existing room restrictions persist |
| POST | `/api/projects/{project}/team-setup/preview` | `{flow_key,flow_rev,leader:{machine_id,name?,tool,variant,launcher_id?,model},dispatcher?:{...},advisor?:{...},role_defaults?:{leader?:{default_agents:[{launcher_id,model}],pick_mode},advisor?:{...},worker?:{...},reviewer?:{...}},settings?:{worktree_mode?,memory_enabled?,max_live_agents?,reviewer_agents?:"inherit"|"on"|"off"},remove_memberships?:[{agent_id,room_id}]}` | owner/operator validation without mutation; returns `{flow,created_rooms,conflicts,agents:[{role,room_key,target}],defaults_saved,settings,can_create}`. `settings` contains project values overlaid with supplied choices, including when omitted. Each initial agent accepts an optional name with the same syntax and collision rules as ordinary spawn; empty picks a free name. A leader, dispatcher or advisor target may instead carry `resume_legacy` (an imported v1 agent of the project) and `confirm_v1_idle`, with the meaning and refusals of the same fields on a spawn (a continued agent keeps its v1 name, so `name` is ignored; the tool must be the one v1 ran). The same fields are accepted on the targets of `team-flow/migration/preview` and `team-flow/migrate`, which is how a flow change takes over the v1 leader. Dispatcher is required only for `leader-dispatcher`; advisor and its role default are allowed only for `leader-advisor`; the `custom` choice uses the ordinary flow PUT route. The host project folder is selected at project creation and is not changed here |
| POST | `/api/projects/{project}/team-setup` | same JSON as preview plus `Idempotency-Key` | 201 `{flow,created_rooms,agents:[{role,agent_id,room_id,room_key}],defaults_saved,settings}`; a keyed replay returns 200. The flow, selected membership removals, project settings, role defaults, and fresh leader/optional dispatcher/advisor records commit together, then supervisor spawn delivery starts. Initial agents use the mapped project folder; worktree mode applies to later task-room spawns. A failed preflight or transaction leaves all of them unchanged; a delivery failure after commit is retried with the same key. A stale flow revision returns 409 `stale_rev`; a stale removal returns 409 `stale_conflict`. `max_live_agents` is 1–100, and `worktree_mode` is `auto` or `off` |
| POST | `/api/projects/{project}/team-flow/migration/preview` | `{flow_key,flow_rev,leader?,dispatcher?,advisor?,changes?:[{agent_id,target:{machine_id,tool,variant,launcher_id?,model}}],role_defaults?,settings?,remove_memberships?}` | owner/operator preview for a configured project switching to a different non-Custom flow; `leader`, `dispatcher`, and `advisor` targets are for a new or stopped core role. Returns `{from_key,to_key,roles:[{role_key,status,agents}],created_rooms,conflicts,settings,can_apply}`. Role status is `kept`, `new`, or `leaving`; already-stopped roles outside the target flow are omitted and leaving-agent membership conflicts are omitted. `agents` is an empty array when a role has none. Agent entries include ID, slug, machine, harness, model, launcher, and state; the wizard labels stopped kept agents as unchanged. The initial plan may omit new-role targets and returns `can_apply:false`; final preview and migrate require them. Kept agent model/harness changes go in `changes`. No mutation |
| POST | `/api/projects/{project}/team-flow/migrate` | same JSON as migration preview plus `Idempotency-Key` | owner/operator starts a cancellable drain and returns 202 `{migration:{id,state,error?,can_cancel,stalled_since?,agents:[{agent_id,slug,role_key,action,state,error?,blocking?,keeps?}]}}` (`blocking` is true while a draining migration waits for that agent to finish a turn; `keeps` is true when the new flow keeps it running); actions are `notice`, `switch`, `spawn`, `reactivate`, or `handoff_stop`. A keyed replay returns 200 with that migration's current progress. Current turns and departing-role handoffs finish before the flow, settings, defaults, and selected membership removals apply. An idle kept agent is marked done during the drain and receives a pending-change notice while other agents finish. A departing agent that is stopped, recovery-blocked, or on a revoked machine uses an automatic handoff; an unresolved turn still holds the drain until recovery ends. Transient drain errors retry up to three consecutive attempts; a permanent error cancels pending handoffs and fails the migration. After applying, unchanged kept agents receive the final rules notice, changed agents switch with handoff, new core roles spawn or stopped core roles reactivate, and departing roles stop after handoff. Handoff details go only to a shared restricted room or agent DM; `general` gets a short notice and migration summary. An unconfirmed apply fails after twenty minutes. A stale revision returns 409 `stale_rev`; archived projects return 409 `project_archived`; direct agent switching during migration returns 409 `migration_active` |
| GET | `/api/projects/{project}/team-flow/migration` | — | owner/operator reads the latest migration's progress, including per-agent state; 404 if none exists. A planned core-role spawn appears during the drain with `agent_id:"planned:<role>"`, the chosen name or `new <role>` as `slug`, `action:"spawn"`, and `state:"waiting"`; the real agent row replaces it after apply. `team_flow_migration` events carry the same `{migration}` object |
| POST | `/api/projects/{project}/team-flow/migration/cancel` | — | owner/operator cancels while `can_cancel` is true and returns 200 `{migration}`; after applying starts, returns 409 `past_point_of_no_return` |
| POST | `/api/projects/{project}/team-flow/migration/interrupt` | `{agent_id}` | owner/operator cuts the current turn of an agent a draining migration waits for (an agent with `blocking:true` in the progress), so the drain goes on without waiting for the turn. On a machine whose supervisor announces `supports_turn_interrupt`, a running ordinary turn is interrupted alone: the agent keeps running, the turn's notes are queued again, and `blocking` stays true until the turn has ended (a repeat sends the frame again). Otherwise (an older supervisor, a turn that cannot be cut that way, a frame that was not answered) the agent is stopped, as the Agents page's Stop does; a kept agent (`keeps:true` in the progress) is then started again by the migration once its turn is gone (also when the change is cancelled or fails), an agent the new flow stops anyway stays stopped. Returns 200 `{migration}`; a repeat while the turn is still open (interrupted, or the agent stopping or interrupting) is a 200; 400 `invalid_request` when the agent has no open turn in the migration, including one that is already stopped; 409 `past_point_of_no_return` once applying has started; 404 when no migration exists |
| GET | `/api/disk` | — | owner or admin disk report for Health: total, per project, per kind, worktrees with state, remote machines' agent output logs, free space, last cleanup ([cleanup](cleanup.md)) |
| GET | `/api/update-status` | — | owner or admin: what the server's own update check last concluded — `{checked:boolean, state?, version?, channel?, checked_at?, ack?}` with state `available` (the channel's newest release is newer than the running process) `installed` (the built binary on disk is newer than the running process, a restart picks it up) or `unbuilt` (the checkout is newer but the binary was not rebuilt, so rebuild first); `checked:false` when no check has run or run to a conclusion (a clone the wrapper did not make, an unreachable remote, or no release for the channel). A later check that finds nothing clears a stored status (`state` becomes empty, and the `update_status` event goes out so open banners close). `ack` is the signed-in person's last answer |
| POST | `/api/update-status/skip` | — | owner or admin only (403 for anyone else): hides the current version and state for good (their own ack; other people keep seeing it) |
| POST | `/api/update-status/remind` | — | owner or admin only (403 for anyone else): hides it until `until`, 24 hours after the ask |
| POST | `/api/cleanup` | `{dry_run:boolean}` (required) | owner or admin: plan (`true`) or run (`false`) the cleanup; returns `{dry_run,items:[{category,target,project_id,reason,bytes,action}],bytes}` with action `would_remove`, `removed` or `kept` |
| GET/PUT | `/api/cleanup/settings` | PUT: `{enabled?,hour?,orphan_days?,attachment_days?}` | owner or admin schedule of the daily cleanup and the attachment retention (`attachment_days`, default 90, 0 keeps them; a removed attachment keeps its record and its message, shows `removed_at`/`removed_after_days`, and its download answers `410`) |
| GET | `/api/cleanup/log` | — | owner or admin cleanup log (what each cleanup removed, and why), newest first |
| POST | `/api/projects/{project}/archive` | — | the project's owner or an admin (who need not be a member) makes the project read-only: agents stopped, gate closed, schedules off, open questions and usage notices withdrawn; returns the project with `archived_at` and `stop_failed` (agents that could not be stopped because their execution is unknown until a recovery decision). Idempotent. While archived, every change through a project, room, agent, inbox or message route is refused with 409 `project_archived` (reading, reopening, stopping or pausing an agent, resolving a recovery or failed delivery, and removing a worktree are not), for whoever may write to the project at all (members, the owner, admins; anyone else gets the route's normal answer); starting an agent in it fails the same way, and a recovery decision there can only be Abandon, which ends with the agent stopped |
| POST | `/api/projects/{project}/reopen` | — | owner or admin lifts the archive; returns the project. The gate returns to what the project's control and today's usage limit say; agents stay stopped and schedules off |
| DELETE | `/api/projects/{project}` | `{name}` | 204; owner or admin archives a live project and deletes it in the same request when possible, removing rooms (including home), messages, attachments (rows and files), agents, usage, memory, schedules, task notes, inbox items, worktree records, members and spawn defaults. `name` must equal the project's name (400 otherwise, before archiving). If an agent stop is pending or a worktree is live, 409 `project_busy` leaves the project archived for a later retry. The project folder and worktrees on disk are not touched |
| PATCH | `/api/projects/{project}` | `{name?,folder?}` (at least one) | the project's owner or an admin renames it (the home room is renamed with it when it still has the old name); only an admin moves the folder (same checks as create: `invalid_folder`), and only while the project has no agents and no live worktree (`folder_in_use`); a `folder` equal to the current one is ignored; emits `project_updated` |
| GET/PUT | `/api/projects/{project}/settings` | PUT: `{auto_resume_after_clean_diagnostic?:boolean, worktree_mode?:"auto"|"off", home_lead_only?:boolean, max_live_agents?:integer, reviewer_agents?:"inherit"|"on"|"off", worker_recovery_to_lead?:boolean, worker_recovery_escalate_minutes?:integer}` (at least one) | project recovery, worktree, home-room, agent limit, and reviewer settings; owner/operator may change them. Worker recovery routing defaults on and escalates to the human Inbox after 30 minutes; the interval accepts 1–1440 minutes. GET and PUT return `reviewer_agents`, `reviewer_agents_global_enabled`, and effective `reviewer_agents_enabled`. Inherit is the default. When effective off, spawning reviewer or a role extending reviewer answers 409 `reviewer_agents_disabled`; existing agents continue. `max_live_agents` is 1–100 and excludes the flow's core roles (for custom, singleton lead-capable roles only). `home_lead_only` defaults true: a spawn into the home room accepts only lead roles and answers 409 `home_is_lead_only` for others. `worktree_mode` defaults to `auto`: task rooms get their own Git worktree where the folder is a Git root ([cleanup](cleanup.md)) |
| GET/POST | `/api/projects/{project}/rooms` | POST: `{title,kind?,purpose?,parent_id?,worktree?:{enabled,base_ref?}}`; kind defaults to `task`, the only client-creatable kind; `purpose` is `task` (the default), `research` or `general` (400 `invalid_request` for another value) and is fixed at creation | rooms visible to this user, including the per-user `hidden` flag, `restricted:true` for restricted rooms, `team_flow_room_key` (`general` or `routing` when assigned) and `purpose` (`task`, `research` or `general`; see below) / created room, with worktree state if requested. Project owners and operators can list restricted rooms for oversight; viewers need active room membership |
| GET | `/api/projects/{project}/legacy-agents` | `?role=leader` (required) | owner/operator: `{agents:[...]}`, the imported v1 agents of the project whose most frequent v1 role is `role` and that no v2 agent continues yet, each with the start info of an imported sender's profile plus `legacy_id`; what the team flow wizard offers to continue instead of a new agent |
| GET | `/api/projects/{project}/claude-sessions` | `?machine_id=` (default: the host) lists the Claude Code sessions of the project's folder on that machine, read from the Claude home of `launcher_id` (default: the machine's native `claude` launcher); `?session_id=` finds one session wherever it worked (409 `machine_folder_missing` when the project has no folder on that machine) | `{machine_id, folder, launcher_id, unreadable, truncated, sessions:[{id,cwd,dir,title,git_branch,version,entrypoint,modified_at,size,active?,outside_folder,agent?}]}`; project owner or operator (the owner and admins may look at a machine that is not admitted yet); a machine other than the host must be admitted for the project first, else 403 `machine_not_admitted` before anything is sent to it; 409 `supervisor_update_needed` for a supervisor that predates scans, 409 `launcher_unavailable` / `needs_native`, 404 `session_not_found` for an unknown id, 409 `session_unreadable` for one with no readable working folder; `unreadable` counts transcripts that could not be assigned to a folder, `truncated` says a scan limit cut the list ([sessions](claude-sessions.md)) |
| GET | `/api/projects/{project}/dm-peers` | — | active people sharing the caller's project, excluding the caller, as `{id,login}`; any project member |
| POST | `/api/projects/{project}/dms` | `{user_id}` | the one private room for this pair in the project; both must be active project members |
| GET | `/api/projects/{project}/archive/search` | `q` (nonempty text) | up to 50 newest matching messages in closed rooms as `{room_id,room_name,message_id,snippet,created_at}`; project membership required, private chats limited to their active human member, and restricted rooms searchable only by an active room member or project owner/operator |
| GET | `/api/projects/{project}/handbook` | — | read-only project handbook for any project member: `{rev,sections,markdown,updated_at,updated_by,line_count,over_budget,nudge?}`. `sections` has `current_state`, `open_backlog`, `conventions`, `gotchas`, `timeline` Markdown strings in that display order; rev 0 is an empty handbook |
| GET | `/api/projects/{project}/handbook/history` | `?limit=20&before_rev=` (exclusive; limit 1–100) | newest-first `{revisions:[{rev,section,operation,updated_at,updated_by,diff}],next_before_rev}`; `next_before_rev` is null on the last page |
| GET | `/api/projects/{project}/handbook/history/{rev}` | — | read-only handbook snapshot at positive revision |
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
| GET | `/api/rooms/{room}` | — | room with the same live counts and preview as the project room list, plus the caller's `hidden` preference; a private chat also carries `can_delete` when the caller may delete it. Owners and operators may read a restricted project room, viewers need active room membership |
| GET | `/api/rooms/{room}/mention-users` | — | `{logins}`: the human logins a `@login` mention addresses in this room, as server addressing reads them: its user room members plus, outside DMs, every active member of the project; same room access as the room itself |
| GET | `/api/rooms/{room}/members` | — | read-only member list `{humans, agents, singletons}`: `humans` are `{id,login,role,online}` (a private chat's two people, or every active project member of an ordinary room, with the project role). **Human presence rule:** a person is `online` only while one of their browser sessions has shown real activity in the last 10 minutes. Activity is a page visible plus user input (the page's `POST /api/presence` heartbeat, which it sends only under those conditions), or any authenticated user action (any authenticated request that changes something). Plain background polling (every periodic read the web app makes is a GET) and a merely open, idle event-stream socket do not count, so an account nobody has used for a while goes offline. The window is server-side (`HumanPresenceWindow`, 10 minutes); presence is kept in memory, so a server restart starts everybody offline until their next activity. A switch in either direction is announced as a `human_presence_changed` event (payload `{user_id,online}`) in each project the person belongs to, so open member lists refresh live, without a reload. Agent presence is separate and unchanged (the agents' `desired`/`observed` fields below); `agents` are the room's agents as `{id,slug,role_key,role_label,role_emoji,singleton,desired,observed,revoked?,role_deleted?}`; `singletons` (empty for a private chat) lists the project's singleton roles as `{role_key,label,emoji,held_by}`, where vacant roles outside the selected built-in team flow are omitted, held roles remain visible, and Custom shows every singleton; `held_by` is `null` when vacant or `{agent_id,slug,in_this_room}` for the live agent holding it (the room it works in is never named). Same access as the room itself; nothing is changed |
| PATCH | `/api/rooms/{room}/preferences` | `{hidden?:boolean, notify?:"default"\|"all"\|"mentions"\|"none", muted_until?:string}` (at least one) | change the caller's own view of the room and answer it as `{hidden,notify,muted_until?}` (`pinned` is not accepted: nothing is pinned). `notify` is how the room notifies this person: `default` (the Inbox, and messages that address them), `all` (every message from somebody else), `mentions` (only messages that address them, and the room's `unread_count` counts only those) and `none` (no sound or notification from the room, its Inbox items included). `muted_until` is an RFC 3339 time in the future, at most a year ahead, until which the room is muted for them (the client sends the end of "1 hour", "8 hours" or "until tomorrow"); an empty string ends the mute, and it ends by itself at the time. A level or time the server cannot keep is 400 `invalid_request`. A change emits `room_preference_changed` (payload `user_id`) |
| POST | `/api/rooms/{room}/read` | `{seq?:integer}` | advance the caller's server-side read cursor to `seq` (the room's newest message when omitted or above the newest); the cursor never moves backwards. Open Inbox items through that cursor become seen for the caller while remaining open. Returns `{read_cursor,unread_count,unread_addressed,attention_count}` and emits `room_read_changed` (payload `user_id`, seen by the room's members, whose read receipts follow it) and `read_cursor_moved` (payload `user_id`) when the cursor or seen state changes. `read_cursor_moved` is person-scoped: the events stream sends it only to the person whose cursor moved, never to other people or agents, and the web app's other tabs and devices of that person refresh their counts, home bubble and mention dot from it, live |
| POST | `/api/rooms/{room}/unread` | `{seq?:integer}` | mark unread from message `seq` (the room's newest message when omitted or above the newest): the caller's own read cursor moves back to `seq - 1`, so that message and every later one count as unread again. It only moves backwards: a cursor already before `seq` stays, so it never marks anything read. Other people's cursors, agents' seen markers and the Inbox's seen state are not touched. Allowed in an archived project, like `read`. Returns `{read_cursor,unread_count,unread_addressed,attention_count}` and emits `room_read_changed` and the person-scoped `read_cursor_moved` (payload `user_id`; see `read`) when the cursor moved. The web app's refresh after `read_cursor_moved` does not mark the open room read again, so a mark unread made on one device is not undone by another that has the room open |
| GET | `/api/rooms/{room}/read-receipts` | — | each member's read position as `[{member_kind,member_id,label,seq}]`: humans report their stored read cursor; an agent reports the newest message it has dealt with, which is v1's rule (a waiting v1 agent was handed every room message and its cursor moved as it took them): a message a turn took (a note in `in_turn` or `done`), one it wrote itself (so its marker moves past its own reply), or one the router handed it with nothing for it to do (not addressed to it, system chatter, or an unaddressed message for the worker while it is the lead put in the room), which counts the moment it is posted. A queued note does not count (a message meant for a busy or stopped agent stays unread until a turn takes it), and neither does a message meant for it that did not get through (revoked, out of reach, a full agent chat). Members who have read nothing are omitted. The web chat marks every member, the viewer and each sender included as v1 does, on the newest loaded message at or before their position and refreshes on `room_read_changed`, `message_created` and agent turn events |
| POST | `/api/rooms/{room}/close` | `{lifecycle_rev,cascade?}` | 202 with `closing` room; 409 `open_children` unless cascade is confirmed, or `home_room` for the project home room |
| POST | `/api/rooms/{room}/reopen` | `{lifecycle_rev}` | reopened room; no automatic agent respawn |
| DELETE | `/api/rooms/{room}` | — | 204; archives a live room and removes it in the same request when closure can finish immediately. A task room requires its project owner. A private chat between a person and an agent or between two people may be deleted by an active human member, the instance owner, or an admin. A chat between agents may be deleted by the instance owner, an admin, or a person who owns either agent's project; agents have no delete tool. Deletion removes the room for everyone. Messages, reactions, notes, turns, inbox items, schedules, task notes, members and attachment rows and files are deleted. Usage and cost records go with it. Events, memory records and an agent's home room that merely point at it keep existing with the reference cleared. An attachment file that cannot be removed afterwards is logged, not an error. A new chat between the same peers starts empty. 409 `room_not_closed` when closure is still in progress, `home_room`, `has_children` (remove child rooms first), `room_not_removable` (a room with a live worktree); 403 when the caller lacks permission. Emits `room_removed` (payload `{room_id}`, plus `user_ids` for an agent chat so eligible people's streams see its removal after its rows are gone) |
| GET/POST/DELETE | `/api/rooms/{room}/worktree` | `machine_id?` (query; the host when omitted); task rooms only (409 `invalid_kind`); POST `{enabled:true,base_ref?}`; DELETE `{force?,confirm?}` | status (a viewer gets `relative_path` but not the absolute `path` on the machine's disk; a read of a `ready` or `preserved` worktree first checks that its path exists, and a missing one is stored as `removed` with reason `worktree directory is missing` and a `room_worktree_updated` event, once; only this one-room read checks, the room list does not), opt in/retry, cancel a pending or failed opt-in while open, or archive an open room and remove its existing worktree when closure finishes immediately. A supervisor too old to prepare worktrees answers 409 `supervisor_update_needed`; a disconnected machine answers 503 `machine_unavailable`; a missing folder or failed Git preparation answers 409 `worktree_failed` with the reason (the git step and what git said: an unknown `base_ref` is 409 `invalid_base` naming the ref, and a base that differs from the one the room's worktree was made from says which base the room has: omit `base_ref` or send that one; a spawn's `base_ref` takes the same path); a store failure answers 500 `internal_error`. Active agent checkouts block removal even with force (`409 worktree_in_use`); `400 confirmation_required` when `force` lacks the matching `confirm`; `409 room_not_closed` when closure is still in progress. Dirty or unmerged work is preserved unless `force:true` and `confirm:"REMOVE <room-id>"` are supplied |
| GET | `/api/rooms/{room}/agent-name` | `exclude?` (query) | anyone who can open the room: a free name for the next agent of the room's project, from the same pool as an unnamed spawn, that a spawn with `name` accepts, never the `exclude` name when one is given; nothing is reserved, so the spawn can still answer `name_in_use`; the Spawn dialog prefills it and its dice button asks again with the name it shows as `exclude`. Result `{name}` |
| GET/POST | `/api/rooms/{room}/agents` | POST: `{role_key,tool?,variant?,launcher_id?,model?,provider?,machine_id?,name?,reason?,worktree?,base_ref?,resume_legacy?,confirm_v1_idle?,resume_session?,confirm_session_closed?,new_room?,work_on_copy?}` and `Idempotency-Key`; `name` is the agent's name (3 to 21 characters: lowercase letters, digits, `-` and `_`, starting with a letter and ending with a letter or digit, and not a group tag such as `all` or `here`), empty means the server picks a free one from its pool; 400 `invalid_name` for a name that does not fit, 409 `name_in_use` when the project already has an agent or role key of that name, or any person has it as a login (no name is shared by a login and an agent; case-insensitive; the message names the name and what holds it) (checked before anything is prepared, and again in the spawn transaction, so two requests for one name create one agent), and 400 `invalid_request` when combined with `resume_legacy` (a resumed v1 agent keeps its own name); `resume_legacy` (a person only) starts the agent from an imported v1 agent: its name, and its Claude Code session continued in v1's working folder, or, with `resume_mode: fork`, worked on as a copy there (`continue` when omitted, as it always was; a copy needs a supervisor with `supports_session_fork`, 409 `supervisor_update_needed` otherwise, needs no `confirm_v1_idle`, and 400 `invalid_request` when the agent has no session that can be continued; Codex, Pi, OpenCode and Vibe continue a recorded native session by staging it from the machine's user home into the agent's private runtime before the first turn (native launcher and `supports_legacy_session_copy` required; `fork` is Claude-only); a missing usable session id or a harness without a v2 adapter starts fresh under the name); the agent is found by project, so it may have written in any room of this room's project (404 `not_found` when it wrote only in another project), and the room in the path is where it starts; 409 `home_is_lead_only` for a role without `cap_is_lead` in a home room while the project keeps it for the lead (also for `resume_legacy`); refusals `not_found`, `already_started`, `invalid_name`, `name_in_use`, `tool_mismatch`, `v1_may_be_active` (needs `confirm_v1_idle`), `v1_folder_missing`, `session_not_found`, `needs_native` ([import](import-v1.md#start-as-v2-agent)); `resume_session` (a person only; the tool must be `claude` on a native launcher) starts the agent from a Claude Code session the machine already has: its id, found on the chosen machine by the server, never a client-supplied folder; the first turn is `claude --resume <id>` in the folder the session was made in (no worktree, 400 otherwise); `resume_mode` is `continue` (append to the original) or `fork` (work on a copy: `--fork-session`, the original stays as it is), **`fork` when omitted** (the server logs each time it applies that default; earlier builds had no mode and always continued, so a caller that relied on that must now send `continue`), 400 `invalid_request` for another value or without `resume_session`, and 409 `supervisor_update_needed` for a fork through a supervisor that does not advertise `supports_session_fork`; `confirm_session_closed` acknowledges that a live process has the original open when continuing (409 `session_active` without it; a copy needs none), `new_room` starts the agent in a new task room titled after the session instead of the room in the path (that room's id is the card's `room_id`); further refusals `session_not_found` (404), `session_folder_missing`, `session_folder_alias` (the session's folder is not the directory the project resolves to; a link to it is fine: turns start in the session's own spelling, which needs a supervisor with `supports_launch_path`, else `supervisor_update_needed`), `supervisor_update_needed`, `needs_native`, and `already_started` for a session a live agent already continues (not for a copy) ([sessions](claude-sessions.md)) | agent cards / created agent; omit the harness and model to use an available role default, or supply `launcher_id` (the harness) or a `variant` naming one (`native`/`docker` select the harnesses of those names); `launcher_id` takes precedence; omitted machine selects the host unless a matching role default supplies one. The model must be one the harness lists (400 `model_unlisted` otherwise); with a `launcher_id` it may be omitted, which means the harness's default model |
| GET | `/api/rooms/{room}/agents/{agent}/profile` | — | read-only detail of a member agent for the web's Agent profile panel: `native_session_id`, `session_started_at`, `start_mode`, `resume_command` (only for tools whose resume syntax is known, `claude` and `codex`, and only when the session id is known), `cwd` (always the agent's own checkout), `worktree` and `branch` (only when that checkout is the agent's own ready worktree, or else the room's ready worktree, on the agent's machine; an agent working elsewhere, such as a continued v1 session in its old folder, shows neither), `machine_name`, `launcher_name`, `project_name`, `repo_url`, `spawned_by`, `spawned_at`, `turns_today`, `cost_today`, and `cost_today_estimated` when any part of that last-24-hour cost uses the embedded model price list. Empty fields are omitted, never guessed. `resume_command` is offered only for a confirmed native session on the host and a tool with a known resume command (Claude, Codex, Pi, OpenCode or Vibe); it includes the agent's checkout and any private session home or database path needed to find that session. It is absent for remote machines and containerised launchers. A project **viewer** gets no `native_session_id`, `resume_command` or `cwd`. Same access as the room. It also answers for an agent that left the room but spoke in it, and for a sender that exists only in imported v1 history: the profile is then the session v1 recorded (`imported: true`, `native_session_id`, `resume_command`, `cwd`, `project_name`, `spawned_at` as v1's first-seen and `last_seen`), only for an id that wrote in this room or whose imported messages there were claimed by its v2 agent. For an owner or operator it also carries `start` (what "Start as v2 agent" needs: `slug`, `tool`, `model`, `role_key`, `cwd`, `resumable`, `reason`, `v1_active`, `outside_folder`, and `started_agent_id`/`started_agent_slug` once started, and `history_room`: the room of the project that holds most of the agent's imported messages, `{id,title,kind,lifecycle}`, which the dialog starts it in when it is an open task room); a v2 agent started from one carries `resumed_from`. 404 otherwise |
| GET | `/api/rooms/{room}/agents/{agent}/output` | `lines?` (default 200, at most 2000), `stream?` (`stderr`, the default, or `conversation`; anything else is 400 `invalid_request`) | the end of one of the agent's two logs, read from the machine that ran it (see [the supervisor](supervisor.md#agent-output)): what its harness wrote to stderr, or its conversation log (the turn's start, what the agent was asked, the model's text, each tool call with its input, each tool result, errors and the turn's end, one record a line with the rest of a record's text on tab-indented lines); **project owner or admin only** for both (403 otherwise), `Cache-Control: no-store`, at most 30 reads a minute a person for both streams together (429 `rate_limited`, `Retry-After`; the web's log popup follows an agent with at most one read in 4 s and waits out `Retry-After`). `{agent_id, machine_id, stream, text, bytes, files, truncated, updated_at, withheld?}`. **Private chats stay private in the conversation stream:** the log is one per agent whichever room a turn served, so every turn that served a private chat (its own room, the room of any message it was given, or a room whose content a toolbox read returned to it or its context quoted: `message_get`, `room_history`, `memory_recall`, a task room's `memory_list`, `list_schedules`, recoveries, recent history, `#N` references; a turn from before that record (migration 129) with none counts as having served every room the agent was a member of when it started, private chats included) is replaced by one line `<time> withheld private chat turn: content withheld` unless the reader is an active member of that chat, whatever their role; a turn that served a restricted room (routing, advice), in the same sense, is replaced by `<time> withheld restricted room turn: content withheld` unless the reader can read that room (a project owner or operator, or a person seated in it; a global admin is not one by being admin); a turn the server cannot place in a room (its room was removed, the page does not say which turn it begins in, or a turn from before migration 129 whose agent's membership of its own room then is not recorded) is replaced by `<time> withheld turn the server cannot place in a room: content withheld` for everyone; `withheld` counts the replaced turns. The stderr stream is not filtered. 404 `not_found` when the agent is not one of this room's project's (an agent that has left its room is still readable), 404 `no_output` when nothing was captured; 409 `machine_offline` when the machine is not connected, 409 `supervisor_outdated` when its supervisor predates this, or predates the conversation log when that stream is asked for (its stderr stays readable); 502 `output_unavailable` when the machine could not read it. Every read is logged with who asked |
| GET | `/api/spawn-catalog` | `machine_id?` | the machine's harnesses with `launcher_id`, portable `launcher_name`, owner label, availability, `containerized`, `toolbox` capability and that harness's `models:[{id,label,note?,default?}]` (exactly one is the default; empty when the owner has listed none); omitted machine selects the host. Only a machine one of the caller's projects is mapped to (the host belongs to every project) as an owner or operator of that project is shown, and the owner and admins see any; any other machine, one that does not exist included, is 404 `not_found`. A machine with no harness has an empty list; `supervisor_update_needed` is true when the connected supervisor predates harnesses |
| GET | `/api/sounds` | — | `{sounds:[{id,name,kind,size?,uploaded_at?,ext?}]}`: the four bundled sounds (`builtin:chime`, `builtin:ding`, `builtin:beep`, `builtin:knock`, served from `/sounds/<name>.wav`) followed by uploads (`user:<id>`) in upload order |
| POST | `/api/sounds` | multipart `file` (`.mp3` or `.wav`, at most 1 MB; the header must match the extension) | owner or admin; 201 with the new entry. 400 `invalid_sound` (bad extension, header or multipart), 413 `invalid_sound` beyond 1 MB, 409 `sound_limit` beyond 10 uploads |
| GET | `/api/sounds/{sound}` | — | the uploaded file (`audio/mpeg` or `audio/wav`, `nosniff`, `Cache-Control: private, no-store`); any signed-in user |
| DELETE | `/api/sounds/{sound}` | — | owner or admin; 204, removes the file |
| GET | `/api/machines` | `project_id?` | without a project ID, the owner and admins see all machines with approval/revocation state; with a project ID, project members (including the owner) see only approved, unrevoked picker choices; both include host marker, connection, last seen, and supervisor version; with a project ID the host row also carries `project_worktree` (`{ok, code?, reason?}`: the server's own worktree check on the project folder, so the Spawn dialog offers a worktree only where one can be made and says why when not, e.g. `not_git_repo`, `not_git_root`, or `git_unusable` with git's reason such as dubious ownership; absent when not known) |
| GET | `/api/machines/directories` | `path` (absolute), `prefix?`, `machine_id?` (host by default) | owner or admin: up to 200 matching immediate directory names on the named machine, including directory symlinks; hidden names require a `.` prefix. Unreadable or absent folders and older remote supervisors return an empty list; an offline remote machine returns 503. No file contents are read or returned. |
| GET | `/api/machines/{id}/harnesses` | — | legacy compatibility view of the six supervisor-discovered harness rows and recent setting changes |
| PUT | `/api/machines/{id}/harnesses/{tool}/{variant}` | `{enabled,override_path}` | owner only (an `override_path` makes the supervisor run that file); legacy compatibility setting endpoint; it does not change a named launcher on updated supervisors |
| GET/POST | `/api/machines/{id}/launchers` | POST (owner only): `{tool,label?,name?,command,args,env_refs,enabled,containerized,agent_server_url?,path_maps,visible_roots,supports_toolbox}` | owner or admin sees the machine's harnesses (stored as launchers; nothing is seeded): config with `label`, observed availability/version (and, for a Docker launcher of a tool that depends on a launcher feature, the `features` the supervisor probed and a `feature_problem` when the probe failed or the launcher is too old, or, while Codex warm mode is on, a Docker Codex launcher has not reported `codex_home=` (stopped container or old launcher); the launcher stays available; and, for a Docker launcher whose version is unknown, a `version_problem` saying why: no project folder to ask it from, what the launcher itself said on stderr (a folder its container does not mount), or that it did not answer in time; the supervisor promptly retries timeouts and retries unresolved container versions about once a minute when a probe folder exists), active agent count, each harness's `models:[{slug,label,note,default}]` with `models_rev` (a short digest of that list, to send back with an edit), and change activity; POST creates a harness after connected supervisor validation. An omitted `label` is the family's name ("Claude Code", plus " (Docker)"); an omitted `name` is `native` (or `harness-docker`) when the machine has no such harness for the tool, otherwise a unique slug of the label. `docker` is reserved |
| PUT/DELETE | `/api/machines/{id}/launchers/{launcher}` | PUT: full launcher definition; DELETE: — | owner only (a launcher is a command line a machine runs) edits or removes a harness (its models go with it); an active or stopping agent, including a revoked one, or an in-flight switch targeting it returns 409 |
| PUT | `/api/machines/{id}/launchers/{launcher}/models` | `{models:[{slug,label,note?,default?,source?}],expected_rev?}` | owner only replaces a harness's whole model list, atomically (works while the machine is offline); the answer is `{models,models_rev}`. With `expected_rev` (the `models_rev` the edit was based on) an edit over a list that has changed since is refused with `409 stale_models` and changes nothing, so two tabs or two people cannot overwrite each other; without it the list is replaced unconditionally. The Save-models editor in the web app always sends it; the first-run fill of a new harness's models does not. Slugs match `^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$` (OpenCode: `provider/model`), labels are at most 80 characters, notes at most 280, at most 1000 models (2 MiB request body limit), and a non-empty list has exactly one default; otherwise 400 `invalid_models` and nothing changes. A keyed spawn or switch retry still replays its stored result after its model is removed. `source` is `manual` (the default for older rows and requests) or `detected`; the web reload removes detected models absent from a successful current listing and keeps manual models. A removed detected model used by an existing agent remains stored as unavailable: it is omitted from the harness list and spawn catalog, cannot be selected for new spawns or switches, and remains in that agent’s configuration and Switch picker until changed. A manually removed model still falls back to the harness default on the agent’s next dispatch (logged as `model_default`) |
| GET/PUT | `/api/machines/{id}/launchers/{launcher}/providers` | PUT: `{providers:[{id,name,npm,base_url,key_env?,context_tokens?,output_tokens?}],expected_rev?,remove_models?}` | owner only for PUT, owner or admin for GET reads or replaces an OpenCode harness's whole list of custom providers (OpenAI-compatible endpoints), atomically and while the machine is offline; both answer `{providers,providers_rev,models,models_rev}`, and the launchers listing carries `providers` and `providers_rev` for every harness (empty for the other families). `id` is the prefix of the provider's model ids (`<id>/<model>`: lowercase letters, digits and hyphens, starting with a letter, at most 40 characters, and not one OpenCode ships such as `openai`), `name` at most 80 characters, `npm` must be `@ai-sdk/openai-compatible`, at most 16 providers. `base_url` is `https://…`, or `http://…` only for this machine (`localhost`, loopback), `host.docker.internal` or a private-network address; no credentials, query or fragment. `key_env` names the environment variable the key is read from on the machine that runs the turn (upper case, may be empty for a server that takes no key; `PATH`, `HOME`, `LD_*`, `XDG_*`, `HARNESS_DOCKER_*`, `OPENCODE_*`, `AIMEBU*` and the like are refused, and so are the keys of OpenCode's own providers such as `OPENAI_API_KEY`: the host's value of one would be sent to the custom base URL): **only the name is stored, never a key**. The optional token limits come as a pair (`context_tokens` and `output_tokens`, both or neither; 0 or absent leaves them to OpenCode) and apply to all of the provider's models. A refused list is 400 `invalid_providers` and changes nothing; with `expected_rev` an edit over a list that has changed since is 409 `stale_providers`. Dropping a provider (or changing its `id`) while models of the harness start with `<id>/` is 409 `provider_in_use` with `provider_id` and `model_count` (the first such provider) and `in_use:[{provider_id,model_count}]` (all of them, since `remove_models` deletes the models of every dropped provider); `remove_models:true` deletes those models in the same transaction (another model becomes the default if the default went). Changes are logged as `providers` (and `models`) in the machine's launcher activity |
| GET | `/api/machines/{id}/launchers/{launcher}/providers/key-status` | — | owner or admin: for each provider of an OpenCode harness that names a key variable, whether that variable has a value on the machine that runs the turns: `{machine_online, supervisor_supported, keys:[{provider_id,key_env,set}], error?}` with `set` true, false, or null while it cannot be known (machine offline, supervisor without `supports_providers`, no answer). The supervisor answers only whether (through the harness's environment references too), never a value; a name the harness uses itself is never answered. Another family is 400, another machine's harness 404 |
| POST | `/api/machines/{id}/launchers/{launcher}/providers/import` | — | owner only asks the machine's supervisor which custom providers the person's own OpenCode config (`$XDG_CONFIG_HOME` or `~/.config/opencode`, `opencode.json` or `.jsonc`) defines, as suggestions for this harness: `{source, suggestions:[{provider:{id,name,npm,base_url,key_env,context_tokens?,output_tokens?}, models:[{slug,label}], inline_key?, problem?, already_added?}], skipped:[{id,reason}]}`. Only `@ai-sdk/openai-compatible` providers qualify (the others are `skipped` with the reason). **No key is ever copied**: a key written as `{env:NAME}` becomes `key_env: NAME`, a key written into the config as text only sets `inline_key` (the owner names a variable and sets it). Each suggestion is checked against the rules a saved provider must meet (`problem` says why one cannot be taken as it is). Nothing is saved: the owner saves the list through the PUT above. 503 when the machine is offline or its supervisor predates providers, 502 `import_failed` when the config cannot be read |
| POST | `/api/machines/{id}/launchers/detect` | — | owner only (setting harnesses up is the owner's) asks the machine's supervisor which harnesses are installed (PATH plus the usual user-level install directories, once per real file, with versions). Candidates only: `{candidates:[{family,command,containerized,version?,label,already_added}]}`; nothing is saved. 503 when the machine is offline or its supervisor predates detection; 502 `detect_failed` |
| POST | `/api/machines/{id}/launchers/{launcher}/models/sync` | `{dir?}` | owner only: detect the current models and save them in one action, adding new detected models up to the 1000-model limit, removing detected models no longer offered, and preserving manual entries and labels/notes of still-offered models. Uses the model revision read before detection; 409 `stale_models` if another edit arrived meanwhile. Returns `{models,models_rev,added,removed,skipped,endpoint_problems?,provider_warnings?}`. OpenCode sync reads the machine's own config (the host config for Docker), saves valid custom providers with an environment-referenced or absent key, and includes config-listed and endpoint models. A provider with an inline key is skipped with a warning. Detection failures leave the model list unchanged; models of a custom provider whose endpoint failed are retained. The same `dir` and detection errors as `/models/detect` apply |
| POST | `/api/machines/{id}/launchers/{launcher}/models/detect` | — | owner only (it sends a provider key to the provider address) asks for the models a harness offers: Pi `pi --offline --list-models`, OpenCode `opencode models`, Codex `codex debug models --bundled` (the models it lists), Mistral Vibe (no command: the `[[models]]` of its config, by alias, and its built-in default), run by the supervisor on the machine with the harness's own arguments first, native or containerized alike, in a fresh private `detect/run-*` folder under the machine's aimebu config directory (removed after the command when possible; leftover `run-*` folders older than one hour are cleared before the next detection) unless the optional body `{dir}` names an absolute folder on that machine; Claude from the Anthropic Go SDK's published `message.go` (fetched by the server only on this call, 5 s, ETag-cached, with a built-in list when unreachable). Candidates only: `{candidates:[{slug,label,note?,already_listed}],source}` with `source` `harness`, `github`, `cache` or `snapshot`; nothing is saved. 400 `invalid_dir` when `dir` is not absolute or not an existing folder on that machine; 503 `machine_unavailable` when the supervisor is offline, or too old to honour `dir`; 502 `detect_failed` with the run path, the end of the command's stderr, "Add the model ID manually", and `detect_dir` (only the private `detect` folder, or the owner-chosen folder, when the supervisor answered), and for an OpenCode harness `endpoint_problems` when custom endpoints could not be asked (also on the 502 for an empty list, and a harness that reports no models still asks the custom endpoints before it fails) For an OpenCode harness the listing runs in the private config an agent turn gets (the person's own OpenCode config and the project's are ignored, the harness's custom providers are defined), so it shows what an agent can use, and each custom provider's own endpoint (`GET <base_url>/models`, with that provider's key if it is set on the machine; no redirect is followed) is asked which models it serves, all of them at once within a shared 8 second budget (the listing command's own 20 seconds come first, and the server waits 30 for the answer): those come back as candidates `<provider id>/<model>`, and an endpoint that could not be asked, or did not answer in time, is named in `endpoint_problems` (`{provider_id: reason}`, never an answer's text) |
| GET | `/api/doctor` | — | read-only; no session. For `aimebu doctor --live`: authenticated by `Authorization: Bearer <key>` with the key in `server/doctor.key` (0600; written at server start, replaced at every start, removed at shutdown), and only from a same-host connection (peer address loopback, or equal to the address the connection was accepted on) without a forwarding header (`X-Forwarded-For`, `Forwarded`, `Via`, …); anything else is `403`. Answer `{machines:[{id,name,approved,connected,supports:[{flag,supported,degrades}],launchers:[{id,tool,name,label?,enabled,containerized,reported,available,unavailable_reason?,version?,features?,feature_problem?}]}]}` for every non-revoked machine; `supports` (the hello's capability flags, with what degrades without each) is present only while the machine is connected. The answer also includes `database:{integrity_check,schema_version,has_v1_schema_meta,checks:[{name,status,description,hint?}]}` from the server-owned store. `integrity_check` uses a separate read connection with a 30-second limit, reports `timeout` at that limit, and a parallel request receives 429 `busy`. No keys, environment or launcher arguments |
| GET | `/api/doctor/sessions` | — | same doctor key and same-host guard as `/api/doctor`; answer `{sessions:[{id,desired,observed,tool,variant,launcher,generation,checkout,remote,containerized,room,last_turn,when,native_session_id}]}` ordered by ID, read from the server-owned store. The CLI never opens the database while the server holds its startup lock. No keys or environment variables |
| GET | `/api/debug/pprof/` | — | the standard Go pprof handlers (`net/http/pprof`, stdlib), served only when the server was started with `AIMEBU_DEBUG_PPROF=1` (off by default: the paths 404 like any unknown API route, for anyone). On, an owner or admin session is required (401 with no session or with an agent key, 403 for any other signed-in person); `GET /api/debug/pprof/` answers the profile index, `/api/debug/pprof/{profile}` (for example `goroutine`, `heap`) a profile, `profile` the CPU profile for `?seconds=N`, and `cmdline`, `symbol` (GET and POST) and `trace` are served too. See [running the server](reference.md#running-the-server-web-ui-address-log-start-and-stop-bootstrap-network) for taking a 30 s CPU profile with `curl` |
| POST | `/api/machine-pairings` | `{name}` | owner creates a single-use, 10-minute token and copyable supervisor command. When the server is reached over plain http at an address other than this machine's, the answer adds `insecure_transport: true` and a `warning`, and the command carries `--insecure` (the supervisor refuses to pair over plain http to another machine without it) |
| POST | `/api/machine-pairings/redeem` | `{token}` | allowlisted token exchange, rate-limited per submitted token and client IP; returns pending machine ID and key once |
| GET | `/api/machines/{id}/pair-status` | machine bearer key | pending/approved state for the paired supervisor; revoked keys get 401. For an approved machine the answer also lists `provider_keys` (`[{launcher,provider_id,key_env,env_refs?}]`: the key variable names of the custom providers of the machine's enabled OpenCode harnesses, with the harness's environment references, names only), which `aimebu doctor --machine` checks against its own environment |
| POST | `/api/machines/{id}/approve` | — | owner approves a pending machine |
| POST | `/api/machines/{id}/agents/stop` | none | stops every agent that runs on the machine, as a single stop (`POST /api/agents/{id}/control`) does and by the same permission: `{stopped:[agent ids], already_stopped, forbidden, failed:[{agent_id,error}]}`; an agent in a project the caller may not manage is left running and counted as `forbidden` (403 when that is all of them); 404 for an unknown machine. A stopped agent starts again from its card |
| GET | `/api/machines/{id}/instances` | — | owner or admin: a remote machine's supervisor instances, oldest first: `{instances:[{journal_id,build?,environment?,state:"active"|"retiring"|"gone",connected,first_seen_at,last_seen_at,gone_at?,turns,commands,agents:[{id,slug}]}]}`. `turns`, `commands` and `agents` are the open turns and running commands the instance owns and whose agents they are (zero and empty once gone): what a write-off would lose. 403 for anyone else, 404 unknown machine, 409 `host_machine` for the server's own machine |
| POST | `/api/machines/{id}/instances/{journal}/gone` | none | owner or admin marks one disconnected instance of a remote machine gone, for good: `{journal_id,already_gone,turns_lost,commands_ended}`. Its open turns become lost with the recovery path and a `supervisor_journal_changed` event each, its open commands end, and a `supervisor_instance_gone` event records the person, the machine, the journal and the counts. Idempotent: a second call answers 200 with `already_gone:true` and ends and records nothing. 403 for anyone else, 404 unknown machine or instance, 409 `instance_connected` while the instance is connected, 409 `instance_recently_seen` when it said hello or its connection ended in the last minute (decided inside the write-off's own transaction, so a hello or a disconnect that lands after the connected check cannot lose a live instance's work; nothing is lost or recorded), 409 `host_machine` for the server's own machine |
| POST | `/api/machines/{id}/revoke` | — | owner revokes a remote machine and closes its socket immediately |
| GET | `/api/projects/{project}/machine-folders` | — | effective folder per unrevoked machine; host uses the project folder |
| PUT | `/api/projects/{project}/machine-folders/{machine}` | `{folder}` | owner/operator sets an absolute, clean remote folder path |
| POST | `/api/projects/{project}/machine-folders/{machine}/admit` | none | the project's owner (or an admin) admits a paired machine for the project without starting an agent to raise the approval: 204; 403 for anyone else, 404 unknown machine, 409 when the machine is not approved or has no folder mapped for the project. Until a machine is admitted its folder is not read and no worktree is made there (403 `machine_not_admitted`), except for the project's owner and the admins, whose own requests pass |
| GET/POST | `/api/roles` | POST: `{role_key,label,emoji,body,extends?,cardinality,cap_is_lead,cap_can_spawn,cap_singleton_writer,default_agents?,pick_mode?,body_extension?}` | global role definitions (each with `extends`, a read-only `resolved_body`, and an `effective_body`: the resolved body with the saved global extension joined, skipping empty parts) / created definition; built-in entries also expose `default_body`, `default_body_base` (the shipped text saved with an edit, absent for migrated edits), and `default_body_changed` for Settings; `cap_singleton_writer` is retained for compatibility but does not affect checkout access; `default_agents` is an ordered array of at most 32 `{launcher_id,model}` from the models a harness lists (400 `invalid_spawn_defaults` otherwise), and `pick_mode` is `in_turn` (default) or `randomly`; roles are global only, so a `project_id` on a write is `400 invalid_request` and on the list is ignored; `body_extension` (present on the list only when one is saved) is the role's global prompt extension — text added after the instructions without replacing them — and on a write it is the **instance owner's** field: an omitted field keeps what is saved, a present one replaces or clears it (empty clears), anything over 16384 bytes is 400 `invalid_extension`, and another writer (an admin included) sending it is 403; a role write is still guarded by its own `rev` |
| GET | `/api/projects/{project}/role-spawn-defaults` | — | effective defaults for each role with `role_label`, `inherited`, global list/mode, and durable next position |
| PUT | `/api/projects/{project}/role-spawn-defaults/{role}` | `{inherit,default_agents?,pick_mode?}` | project owner or operator sets a complete list/mode override, or deletes it to inherit global settings |
| GET | `/api/projects/{project}/role-spawn-defaults/{role}/next` | — | next currently available default, with machine, launcher, tool, variant, and model for the human Spawn dialog; `available=false` when none can start |
| GET | `/api/projects/{project}/role-prompts` | — | project owner or operator: every live role as this project's role-prompt settings show it — `role_id`, `role_key`, `label`, `emoji`, `builtin`, `resolved_body` (the base the extensions append to), `flow_override` (the project's team flow replaces this role's body with its own text on delivery), `global_body_extension` (the instance owner's, when saved), `project_extension` (`{body,rev,updated_at}`, absent when none), and `delivered_body`, the exact text an agent of that role in this project is told (the composition dispatch uses, the flow override and the extensions included) |
| PUT | `/api/projects/{project}/role-prompts/{role}` | `{body,rev}` | project owner or operator saves (or, with an empty `body`, clears) this project's extension of the role named by `role_key`; `rev` is 0 to create or clear an absent one, else the saved revision (409 `stale_rev` otherwise); over 16384 bytes is 400 `invalid_extension`; returns the saved `{body,rev,updated_at}` |
| PUT/DELETE | `/api/roles/{id}` | PUT: full definition with `rev` (an omitted `extends` clears it); DELETE: `?rev=…` | revised definition / 204; `409 role_extended` while other roles extend it; `body_extension` follows the `POST /api/roles` rule (the instance owner's field; omitted keeps what is saved) |
| POST | `/api/roles/{id}/reset` | `{rev}` | the owner or an admin restores a built-in global role (id ending `_default`) to its shipped name, emoji, instructions, parent, cardinality, capabilities, and empty default-agent list with in-turn mode; returns the definition (with `resolved_body`); `400` for a role that is not built in, `409 stale_rev`, or `409 role_in_use` when live agents hold it with other capabilities or cardinality; the saved extension is left alone |
| POST | `/api/roles/{id}/use-default` | `{rev}` | owner or admin clears only an edited built-in role body, keeping its other settings and extension; returns `{rev}`; `403` for other users, `409 stale_rev` for a stale editor |
| POST | `/api/roles/{id}/keep-default` | `{rev}` | owner or admin acknowledges the current shipped body for an edited built-in role without changing the edit; stores its text and fingerprint, returns `{rev}`; `403` for other users, `409 stale_rev` for a stale editor |
| GET/PUT | `/api/macros` | PUT: complete `{key:body}` map | shared composer macros; any signed-in user reads, the owner or an admin replaces (CSRF required) |
| GET | `/api/settings-backup` | — | owner or admin download of the instance's settings (an admin's file leaves out `quota_switcher` and `limit_guard`: they decide over the owner's logins, and only the owner can apply them) as `{schema:"aimebu-settings-backup", version:1, exported_at, macros, roles, retention, quota_switcher, limit_guard}`; each section is present only when it has content, except `retention`, which is always present. `roles` are the **global** ones only, each with its saved `body_extension` when it has one; `quota_switcher` and `limit_guard` are the saved per-tool configurations (a tool on its defaults is left out) and appear only while the switcher runs. Built field by field, so it never contains credentials, tokens, provider secrets, machines, users or sessions. The web UI adds a `ui_prefs` section of its own (browser preferences, named by what they are) |
| POST | `/api/settings-backup/preview` | `{backup}` | owner-only; saves nothing. Per section `{section, new, overwrite, unchanged, invalid:[{key,reason}], unavailable?}`, `ignored` (top-level members this version does not know) and a `digest` of the plan. 400 `invalid_request` for a wrong `schema`, 400 `unsupported_version` for a `version` other than 1 |
| POST | `/api/settings-backup/apply` | `{backup, digest, sections?}` | owner-only (a backup's `quota_switcher` config that still has `dry_run` imports fine, the field ignored); `409 preview_stale` unless `digest` is the one a preview of the same file answered against the current state: the digest covers every step and the current value of each entry it would overwrite (or that it is absent), so a different file, or an edit of any of those entries after the preview, is refused. `sections` chooses a subset: absent means every section that has changes, and a present but empty list means none. Sections apply in a fixed order (macros, retention, quota_switcher, limit_guard, roles, ui_prefs) and independently; within a section an entry that cannot be saved is reported in `failed` while the rest still apply, so a partial import is possible and is never silent. Returns `{sections:[{section,applied}], failed:[{section,key,error}], complete}`. Matching macros and roles (by key) are overwritten, nothing is deleted; a role entry's `body_extension` restores with the role and an entry without the field leaves the saved extension alone, so an older backup imports unchanged; model lists belong to the harnesses now, so a `spawn_models` section in an older file is not imported and the preview lists it under `ignored`; body limit 4 MiB |
| GET/POST | `/api/rooms/{room}/control` | POST: `{action:"pause"|"stop"|"resume",rev}` and `Idempotency-Key` | room bulk control state and per-agent results |
| GET/POST | `/api/projects/{project}/control` | POST: `{action:"pause"|"stop"|"resume",rev}` and `Idempotency-Key` | project bulk control state and per-agent results |
| GET/PUT | `/api/usage-limit-default` | PUT (owner): `{daily_cost_cap?,daily_turn_cap?}`, null or absent = no limit | the global default daily limits every project inherits (any signed-in user may read it; `can_edit` says whether the caller may change it); changing it re-checks the projects that inherit it: for each field (cost, turns) whose default actually changed, today's Continue raise on a project that inherits that field is cleared (a field the project sets itself, or one whose default is unchanged, keeps its raise), so a raised default lifts the pause the old one caused; saving the values it already has does nothing |
| GET/POST | `/api/rooms/{room}/schedules` | POST: `{target_agent_id? ,target_lead?,body,kind,timezone,at?,interval_seconds?,local_time?,weekday?,cron?}` | task-room or home-room schedules / created schedule; 409 `invalid_scope` for a private chat or the lobby, 400 `invalid_schedule` for a bad target, time or note, 409 `schedule_limit` beyond 20 active |
| GET | `/api/cron/preview?expr=&timezone=` | — | signed-in user; describes a cron expression for the schedule form: `{cron,description,next:[5 RFC3339 UTC instants]}`, or `{error}` (still 200) for a bad expression or zone |
| PATCH/DELETE | `/api/rooms/{room}/schedules/{id}` | PATCH: `{enabled,rev}` | pause/resume or delete a schedule |
| DELETE | `/api/agents/{agent}/worktree` | `{force?,confirm?}` | removes an existing agent-specific worktree (project operator): `{state:"removed"}`, or `{state:"preserved",reason}` when it cannot prove the worktree clean: uncommitted or unmerged work, a changed submodule, a submodule folder that is not empty, a submodule checked out earlier (`modules/` in the worktree's git dir), a `.git` anywhere below the top level, a path it cannot read (`could not read <path>`), more than 200,000 entries to check (`too many files to check`), or a check the request's deadline interrupts (`check interrupted`), while an unpopulated submodule's empty folder does not count; only `force` with `confirm: "REMOVE <name>"` removes it anyway (400 `confirmation_required` otherwise); 409 `worktree_in_use` while its agent runs (a start of the agent waits for a removal under way, and a removal that finds the agent started refuses); worktree preparation errors use 409 `worktree_failed`, an unavailable supervisor uses 503 `machine_unavailable`, and an internal store failure uses 500 `internal_error`; the branch is kept, and the agent goes back to its room's checkout (its ready worktree, else the project folder) |
| GET/PUT | `/api/projects/{project}/usage-limit` | PUT: `{cost_mode?,daily_cost_cap?,turn_mode?,daily_turn_cap?,rev}`; a mode is `inherit`, `custom` (needs its value) or `none`; without modes a value means custom and null means none | the project's daily limits (all its rooms together), the caps in force, `inherited_cost_cap`/`inherited_turn_cap` (the global default), measured totals, and effective state |

| POST | `/api/agents/{id}/control` | `{desired,rev,start_fresh?}` | updated agent card; `desired:"active"` on a stopped agent resumes its last known native session, while `start_fresh:true` explicitly starts a new one with recent room history (`start_fresh` is valid only with active); who may do it is decided first (project owner or operator), so a person who may not, and an id that is no agent, both get the same 403 before anything is loaded or sent to a machine. A `desired:"stopped"` end is deliberate: the agent's queued notes are dropped as undelivered (one `notes_dropped` event per room, reason `agent stopped`; the messages stay in their rooms), a running turn the stop's cancel then ends is recorded as cancelled with no recovery item, Inbox card or lead note, and open recovery and delivery_failed items settle as `agent stopped` |
| POST | `/api/agents/{id}/clear-context` | `{rev}` | owner/operator only, for an active agent; queues a fresh harness session without interrupting the current turn. New dispatch waits for the old turn to settle, then advances generation and retires any warm process. The prior session stays in history. Returns the agent card; stale revision is 409 |
| DELETE | `/api/agents/{id}` | `{}` | removes a **stopped** agent for good (project owner or operator; 409 `agent_not_stopped` unless it is `desired:stopped` and `observed:stopped`, 409 `project_archived` in an archived project, 404 for an agent that is gone or already removed): its worktree of its own, if it has one, is removed first by the rules of `DELETE /api/agents/{agent}/worktree` without `force`: a clean worktree is removed (an unpopulated submodule's empty folder does not count), and anything the removal cannot prove clean (see that endpoint) refuses the removal (409 `worktree_preserved`, with the reason: the agent keeps its name until the person has removed or merged the worktree on purpose, so that a new agent of that name never finds this work in its folder); 409 `worktree_in_use`, or the worktree error, also leaves the agent as it was. The branch it made is kept, set aside as `aimebu/<room>-<slug>-removed-<n>` so that a worktree made later under the freed name starts from the room's base; then in one transaction the agent leaves every room (its queued notes are dropped; a chat between agents left with fewer than two agents is closed), its open Inbox items are withdrawn, and it is revoked, which stops its key and frees its role's slot, its name (the row keeps a tombstone in `slug` and the name it had in `removed_slug`). Schedules aimed at the agent are paused, each with a notice in its room. Answers `{agent_id,slug,worktree}` (`worktree` is `{name,path,branch,state,reason,kept_branch}`, `kept_branch` being the set-aside branch name when there was one, or `{}` when the agent had no worktree). Its messages, turns and history stay: a message carries `author_name` (the name the agent had) and `author_project_id`, and a reply's `reply_preview` the same, so a client writes `<slug>@<project> (removed)`. Events `room_member_left` per room and `agent_revoked` (with `slug` and `removed_by`) |
| POST | `/api/agents/{id}/switch` | `{room_id,tool,variant,launcher_id?,model,machine_id?,rev}` and `Idempotency-Key` | 202 agent card with durable handoff progress; `launcher_id` takes precedence over `variant`; omitted machine keeps the current one; 200 on keyed replay. The current turn finishes first, then one handoff turn runs with a 3-minute deadline and a 15-minute overall cap. Fallback uses selected-room history and is marked `handoff_source:"automatic"` |
| POST | `/api/agents/{id}/switch/preview` | same JSON as Switch; no idempotency key | read-only `{handoff_status,history_window:40,handbook_included,can_cancel,can_retry,can_continue,target}` for the selected room and target; validates project control permission, room membership, adapter, machine, and model |
| GET | `/api/agents/{id}/switch` | — | `{switch,desired_rev}` progress for an owner/operator. Agent-card `switch` includes `handoff_status`, `handoff_source`, `handoff` payload, handoff and overall deadlines, and `can_cancel`, `can_retry`, `can_continue` action flags; after a successful switch it reports `state:"started"` |
| POST | `/api/agents/{id}/switch/{cancel|retry-handoff|continue}` | `{rev}` and `Idempotency-Key` | 200 agent card; cancel before successor start restores the old agent, continue chooses an automatic summary, retry-handoff starts another attempt when the previous turn is quiescent; stale revision returns 409 |
| GET/POST | `/api/rooms/{room}/messages` | GET: `limit=1..500` (default 200), one of `after=<seq>`, `before=<seq-or-id>`, or `around=<seq-or-id>`; `thread_root=<id>` gives the conversation that message belongs to (its first message's replies, any depth) with the same cursor choices. POST: `{body,reply_to?,attachments?:[id]}` (a `thread_root` sent is ignored: the conversation follows `reply_to`); the body is at most 32 KB, and a bigger one is refused with 400 `invalid_answer` "Message is too long (max 32 KB)", or with 413 `request_too_large` and the same message when the whole JSON body is over the route's limit (the body limit plus an 8 KB envelope reserve) — the web composer shows that message as its send error | GET returns ascending messages: latest page by default, exclusive newer or older page with `after` or `before`, a context page containing `around`, or matching thread replies. Each GET message includes reactions and the full `thread_count` for its root, including replies outside the page. Agent `needs_attention`, `proposed_answers`, `open_questions`, `visual_plan`, and `appendix_pages` appear when present. Agent messages include the `author_harness` and `author_model` captured when sent when known; an older uncertain message omits them. A task-result handoff carries `outcome` (`done`, `partial` or `failed`); every other message, and a handoff written before outcomes or in a turn dispatched to a supervisor that does not report `supports_handoff_outcome`, has none. A switch handoff system message includes `switch_handoff` `{agent_slug,old_tool,new_tool,old_model,new_model,source}` with `source` either `written` or `automatic`. A message from a person carries `author_name`, the person's login, next to `author_id`, so a client shows a name rather than the account id. POST creates a user message with attachment metadata and includes `thread_count` for a thread reply when the count lookup succeeds; the saved reply still returns 201 if that lookup fails. A reply (and a created reply) also carries `reply_preview` `{no, author_kind, author_id, author_name?, snippet}` for the message it answers, read for the whole page in one query, so a client that has not loaded that message can still quote it; `snippet` is one line of its text (whitespace collapsed, cut to 96 characters, `[attachment]` for a message that is only files), and a parent that is not in the same room previews nothing. A message an agent wrote also carries `usage` `{turn_id, index, count, tokens_in?, tokens_out?, tokens_cache_read?, tokens_cache_write?, cost?, cost_source?, model?, partial?}`: what the turn that wrote it spent, for anyone who can read the room. Every message of a turn carries the same turn figures with its place (`index` of `count` messages the turn wrote), so a client must never add them up across messages; a figure the harness did not report is absent (never 0), `cost_source` is `estimated` for a dated model price-list value and `reported` for a harness value, and `partial` means the turn ended before it reported its totals, so the figures are a floor. The turn's several usage records are summed once; a message whose turn reported nothing has no `usage`. Read for the whole page in three queries (the turn behind each message, the turn's message count, the turn's totals). `tokens_in` is uncached input for every harness; cache reads and writes appear separately, and a displayed total input adds all three categories. Every message carries `no`, its number: the "#N" shown, copied and written, one sequence across every room counted by messages alone (see the contract), while `seq` is the internal order shared with events that paging cursors (`after`, `before`, `around`) use. A GET also returns `expirable` on every message: true only for AI group chatter that message expiry may leave out of an agent's context (see [message expiry](retention.md)). A message whose body stored agent-ID mentions (see the contract) also carries `mentions` `[{id,name,resolved}]`: what each stored mention resolved to for this reader, in body order, with the display name (a `name-2` ordinal in a duplicate room); `resolved:false` when the agent's row is gone and the body keeps the ID text. A message whose body references rooms (see the contract) also carries `entities` `[{id,name,kind:"room"}]`: one entry per distinct `#room_<id>` the stored body mentions, in body order, `name` the room's title only when the reader may see the room — a room the reader may not see stays unnamed (the web shows `#private-room`), and an unknown or deleted id has no entry (the web shows `#unknown-room`) — while the stored body keeps the ids |
| GET/POST | `/api/rooms/{room}/task-notes` | POST: `{body}` | `{can_edit,records,cap_bytes}` / created task note |
| PUT/DELETE | `/api/rooms/{room}/task-notes/{id}` | PUT: `{body,rev}`; DELETE: `?rev=…` | revised or deleted task note |
| POST | `/api/rooms/{room}/attachments` | multipart `file`, any type ≤10 MB; a person may have at most 20 unsent uploads, 100 MB together (429 `attachment_quota`: send or remove some) | staged `{id,name,mime,size,url,width?,height?}` (`width` and `height` are the pixel size of a PNG, JPEG or GIF, omitted when unknown, and are carried on a message's `attachments` too, so a thumbnail can hold its place); room access and CSRF required |
| (SVG) | upload | — | Go does not sniff SVG, so an uploaded file is checked with the standard library's strict XML decoder: UTF-8 only, at most 2 MB, 50,000 tokens and 64 levels deep, no DOCTYPE or entity declaration, the root an `svg` element in the SVG namespace, and nothing active: no `script`, `foreignObject`, `iframe`, `embed`, `object`, `audio`, `video`, `handler` or `listener` element, no `<?xml-stylesheet?>` instruction, no `on*` attribute or `javascript:` value, no `href`/`src` that leaves the file (only `#fragment` and inline PNG, JPEG, GIF or WebP data are allowed), no CSS `@import`, `image-set()` or `src()`, no `url()` that is not one of those (CSS comments are removed before the check, and the text of a `<style>` is read as one piece), and no `set` or `animate` element that writes an event handler or a reference or CSS that leaves the file. A file that passes gets `mime: image/svg+xml` and its `width`/`height` from the `width`/`height` attributes or the `viewBox`; the web UI shows it as an `<img>` and never puts it into the page as markup. One that fails keeps its sniffed text type and stays download-only, as before. Nothing is rewritten. The `attachment_get` tool gives an agent an SVG as source text (up to 64 KB) |
| DELETE | `/api/rooms/{room}/attachments/{attachment}` | staged upload owned by caller | 204; discards unused file |
| GET | `/api/attachments/{attachment}` | — | file bytes; caller must have access to the file's room (project membership for normal rooms, DM peer membership for DMs); an upload no message has claimed yet is its uploader's alone, and a file the caller may not open answers the same 404 `not_found` as one that does not exist. Only PNG, JPEG, GIF, and WebP stay inline; every other MIME type downloads with an attachment filename (a fallback if the stored name is empty). An SVG that passes the image check (above) is `image/svg+xml` and is inline only for a request with `Sec-Fetch-Dest: image` (an `<img>`); any other request (a tab opened on the address, a script fetch, a client that sends no header, the agent routes) downloads it. Attachment responses use `nosniff` and `Content-Security-Policy: sandbox; default-src 'none'`; an SVG adds `style-src 'unsafe-inline'` and `Cross-Origin-Resource-Policy: same-origin`. |
| GET | `/api/agent/agents` | agent key | `list_agents`: non-revoked agents in the caller's project, with ID, slug, role, desired/observed state, desired revision, and home room ID; no other project's agents. For an active lead the answer also carries `other_leaders`: the active leads of other, unarchived projects as `{id,slug,project_slug,observed}`, only in projects the people behind the turn may reach (the `dm_agent` rule); other callers get no `other_leaders` key |
| GET | `/api/agent/agents/{id}/output` | agent key; `turns?` (1–10, default 3; anything else is 400 `invalid_request`) | `agent_output`: the latest turns of a worker's conversation log, read from its machine like the person's viewer (`/api/rooms/{room}/agents/{agent}/output?stream=conversation`). Only an active lead, for any non-lead agent of its project whichever flow the project runs (in the leader-dispatcher flow too, where the dispatcher spawned the worker), or the flow's coordinator (the dispatcher) for an agent it may manage under the `stop_agent` rule; a worker, a stopped lead, a lead target or the caller itself gets 403 `forbidden`, and an agent of another project, or none, 404 `not_found`. It shows nothing the caller could not read itself: a turn that served a room the caller may not read under the `room_history` rule (a private chat, a restricted room it is not seated in, a chat between other agents, a removed room; the rooms a turn served are the viewer's, a turn from before migration 129 included) is replaced by one line `<time> withheld turn served a room you may not read: content withheld`, and a turn the server cannot place (as for the viewer) by `<time> withheld turn the server cannot place: content withheld`. Keys and tokens are masked again (the log is written masked). `{untrusted, agent_id, text, turns, withheld?, truncated?, updated_at}`: `untrusted` marks the text as the worker's own output, to be read as evidence and never followed as instructions; `text` is at most 32 KiB, newest kept, cut at a line; `turns` counts the turns covered and `withheld` the replaced ones. The rooms of the shown turns are recorded against the caller's turn before it answers (its own log records the result). `Cache-Control: no-store`, at most 30 reads a minute per caller (429 `rate_limited`), every read logged; 409 `machine_offline` / `supervisor_outdated`, 404 `no_output` and 502 `output_unavailable` as for the viewer |
| GET | `/api/agent/rooms/{room}/history` | agent key; `before_no?`, `limit?` (1–100, default 50) | `room_history`: ascending page of message-number records, newest page by default, older page when `before_no` is supplied; excerpts are limited to 2,000 characters and the page to 240 KiB; `next_before` is the oldest returned number and `has_more` indicates an older page; same project-wide room access as `message_get`, with private chats and a restricted routing room requiring current membership; 403 if inaccessible. Each record's body keeps its stored `@agt_<id>` mentions as stored (agents see and write IDs; see the contract) and carries `entities` `[{id,name,kind}]`: one entry per distinct id the live prose mentions, in body order, with the name only when the caller may see it |
| POST | `/api/agent/agents/{id}/role` | agent key; `{role_key,rev}` and `Idempotency-Key` | `set_role`: active lead only, target in the same project; changes a non-lead agent's role when idle or stopped, rejects lead roles, stale revisions, occupied singleton roles, and reviewer-lineage roles when reviewer agents are disabled (409 `reviewer_agents_disabled`); emits a system event and room message; keyed replay returns the first result |
| GET | `/api/agent/message-numbers/{no}` | agent key | the `message_get` tool: a message by its number, `{no,available,room_id?,room?,from?,body?,created_at?,appendix_page_count?,appendix_pages?,outcome?}` (`outcome` on a task-result handoff, as in `room_history` and delivery notes) (the pages in full; history and referenced messages carry only the count), from the agent's project; private chats require membership, and restricted rooms require membership or an exact `#N` reference from a template-authorized role in a room the agent belongs to. A targeted missed-mention system pointer also grants its one cited message to the addressed agent, not other members of the pointer room. The citing author must have direct access to the target room when sending an ordinary citation. A person's citation reaches across projects: when a person writes a message that cites `#N` and can read N's room at that moment (a normal room of a project they belong to; a restricted one needs owner/operator oversight or a seat; never a private chat), the agents of the citing room may read exactly that message, which then carries `project` (its project's name). The grant is per citation, never opens the room or its history, is checked against the person's current access on every read (losing access revokes it), and an agent's cross-project citation grants nothing. A lead may also read a message of another project that this project's lead cited (`#N`) in the chat between the two leads: the cited message must be in a room the citing lead reads (a normal room of its project, or a restricted one it sits in), both must still be leads, and it carries `project`; a non-lead gets nothing from it. Any other message of another project and a number nothing carries both answer `{no,available:false}` with nothing else; 400 for a number that is not a positive integer. An available record's body keeps its stored `@agt_<id>` mentions and carries `entities` `[{id,name,kind}]` as `room_history` does (see the contract) |
| POST | `/api/agent/attachments/{attachment}/save` | agent key | `attachment_get` over the HTTP MCP endpoint, which reads the type and size with a HEAD on the download route and calls this for a file that cannot come back inline, without downloading it: the same access rule as the download (a note delivered in the caller's live turn, room membership); the server asks the agent's supervisor to save the file into the turn's workspace (`attachment_save`, see [the contract](contract.md)) and answers 200 `{saved:true, path, mime, size}` with the path the agent sees. 404 `not_found` outside the rule; 409 `attachment_save_unsupported` for a supervisor from before the frame; 503 `machine_unavailable` when the machine is not connected; 502 `attachment_save_failed` with the supervisor's reason |
| GET | `/api/agent/attachments/{attachment}` | agent key | file bytes only for a note delivered in the caller's live turn while still a room member; used by `attachment_get` (which returns an image or text up to 64 KB inline and, saves any other file up to 10 MB, or any file with `save: true`, to `.aimebu/attachments/<attachment_id>/<name>` in the agent's workspace (mode 0600 from the bridge, the checkout's owner and read access from the supervisor): the stdio bridge of native Claude Code and Codex writes it itself, the HTTP MCP endpoint through the save route below; see [the adapter guide](adapter.md)); same download and security headers as the user attachment route |
| GET | `/api/agent/turns/{turn}/attachments/{attachment}` | turn-scoped attachment bearer | file bytes only for that live turn and current room membership; used by Pi Docker; same download and security headers as the user attachment route |
| POST | `/api/inbox/{item}/answer` | `{body,rev}` and `Idempotency-Key` header | answer message; 200 on keyed replay, 201 on first answer |
| POST | `/api/inbox/{item}/decision` | `{choice,rev}` and `Idempotency-Key` header | recovery, daily usage-limit or pool-change decision (`respawn` or `keep`, with `keep_agents` agent ids to leave as they are; the project owner or an admin, else 403) `{item_id,choice,turn_id?,rev}`; 200 on keyed replay, 201 first time |
| GET | `/api/agent/recoveries` | agent key | active lead's open worker recoveries in its project |
| POST | `/api/agent/recoveries/{item}/decision` | `{choice:"resume"|"retry"|"abandon"|"abandon_stop"|"restart",note?:string}` and `Idempotency-Key` header | active lead's assigned recovery decision, using the same transition and idempotency path as the human decision |
| POST | `/api/inbox/{item}/dismiss` | `{rev}` | close an ordinary open question or approval without answering it, dismiss a delivery-failed notice (owner, operator, or DM peer), or dismiss a `usage_alert` notice (owner or global admin); shared-note and boundary approvals require a decision. 204 on success, 409 on stale revision or closed item, 404 for an absent usage alert |
| GET | `/api/system-events` | `limit?` (1–500, default 100), `notable=1`? | newest-first operational events the caller may see (`events`, same shape as the live stream) and the `kinds` list of the feed. Every event of the feed (here and on the live stream) carries `notable: true` when it is one the bell shows; `notable=1` returns only those, as the newest `limit` of them, so a burst of routine events cannot push them out of a page. What is notable is defined once in the store (`IsNotableSystemEvent`): a fixed list of kinds (spawns and their failures, an agent stopped or removed, lost or unstarted turns, held answers, the loop guard, credential alerts, usage limits and their clears, a rejected usage login, automatic account switches, a session reset, a context clear or compaction, an admin changed, a user deactivated, a room that could not close, a machine revoked) plus two conditional ones, a `turn_process_exit` with a code other than 0 and an `agent_control_requested` that starts, stops or pauses (not the automatic pause of an account switch); the web keeps no list of its own; joins, spawns, recovery, quota, machine and access changes, never chat bookkeeping such as `message_created` or reactions. Each event carries `names` (id → display name: agents as `slug@project`, users by login) for the agent and user ids it mentions, so a client never has to show a raw id; ids that no longer resolve are omitted. Private chats only for their members; events with no project (user, machine and agent-key changes) are visible to the instance owner only, in this list and on the live stream (the per-login session and password-rehash bookkeeping is shown to nobody) |
| GET | `/api/rooms/{room}/export` | `format=json` or `format=markdown` (required; 400 for anything else) | the whole history of the room, oldest first, as a download named after the room (`<title>.json` or `<title>.md`, letters, digits, `-` and `_` only). Whoever may read the room may export it (same access as the messages route). JSON is `{room, exported_at, messages:[…]}` with each message as the messages route returns it (`reactions`, `thread_count`, `expirable` included), plus `author_label` (the login, `slug@project`, or `System`) and, for a reply, `reply_to_no`; an attachment's server path is never included, and neither is `reply_preview` (a reply has `reply_to_no` instead) nor `usage` (what a turn spent is read from the room's usage route, not copied into each message of an export). Markdown has a heading for the room and one per message (`## #no · author · time`), a quote naming the message a reply answers, a line for each attachment (name, type, size; the bytes are not included) and the reactions per emoji. Streamed in pages of 500, so only a small id-to-number map grows with the room. The first page is read before anything is sent (an error there is an ordinary 500); an error after that ends the connection abruptly, so a truncated file is a failed download and never one that ends cleanly |
| GET | `/api/rooms/{room}/usage` | `range=today` (default), `7d` or `all` (400 for anything else) | what the room's turns spent over the range: `{scope, scope_id, range, since?, turns, turns_with_cost, turns_with_estimated_cost, cost_coverage, tokens_in?, tokens_out?, tokens_cache_read?, tokens_cache_write?, cost?, partial?, by_role:[{key,label,agents,turns,cost_estimated?,…}], by_model:[…]}`. It sums `usage_record`, which holds each turn once, so a turn that wrote several messages is counted once. `today` is the server's local day, `7d` the last 168 hours. `cost_coverage` is `known` when every turn had a harness-reported cost, `estimated` when every turn had a cost and at least one used the dated model price list, `partial` when some had no cost, and `unknown` when none had one. `partial: true` means some turn ended before it reported its totals. A figure nobody reported is absent, never 0. Whoever may read the room may read it; the records go with the room when it is removed |
| GET | `/api/projects/{project}/usage` | same | the same for every room of the project; whoever belongs to the project may read it |
| GET | `/api/messages/{message}/debug` | — | debug inspector view of one message: the stored record (`message`, with `ask`, `proposed_answers`, `open_questions`, `visual_plan`, `appendix_pages` as parsed JSON), `addressed_to`, an `addressing` trace (for each @token: how it was read — group tag, full ID, slug, project-qualified, role, revoked, none — whether it was `addressed`, `ambiguous`, `no_match`, `ignored` (inside backticks, a code block, or escaped, and only when it would have reached someone, with `would_reach` naming them; code such as Java annotations is not reported), `self`, or `empty`, who it reached, and why; plus reply and private-chat sources, and a `drift` flag when re-reading the body against the room as it is now would address someone the recorded targets do not include; never set for an inbox answer, whose targets the answer fixes), per-agent wake `notes` (state, `wake_reason`, the turns that carried the note and their receipts), and related `events` (including `message_not_woken` reasons). Same room access as reading the message; long stored text, including the body, is truncated at 8 KiB on a character boundary; related events are those of the message's own room |
| GET | `/api/messages/{message}/reactions` | — | reaction entries |
| PUT/DELETE | `/api/messages/{message}/reactions/{emoji}` | — | 204; add/remove one Unicode emoji sequence for the user; DELETE accepts older stored reactions up to 128 bytes; unchanged reactions emit no event |
| PUT/DELETE | `/api/agent/messages/{message}/reactions/{emoji}` | agent key | `{message}` is a message id or its number (`#12` or `12`); 204; add/remove one Unicode emoji sequence in a joined room; DELETE accepts older stored reactions up to 128 bytes; unchanged reactions emit no event |
| GET | `/api/message-numbers/{no}` | — | where the message numbered `#no` lives: `{id,no,seq,room_id}`; the caller needs access to its room (404 when no message has the number, 400 for a number that is not a positive integer); the web UI uses it for a `#N` link into another room |
| GET | `/api/messages/referenced/{no}` | `source_message_id` query parameter | `{no,available,room_id?,room?,from?,body?,created_at?}` for exactly the target cited by the source message. The caller must be able to read the source room; for a missed-mention system pointer, only project owners and operators may open the cited target this way. Other source-room members receive `{no,available:false}`. A missing grant also returns `{no,available:false}` without identifying the target. This does not grant access to the target room or its history |
| GET | `/api/events?after=<seq>` | WebSocket upgrade | event stream: project events for project members (private chats for their members), events with no project for the instance owner only; owners and global admins also get transient `{type:"usages_updated",alert_revision}` frames (no `seq`, never replayed). `alert_revision` advances when a synthetic usage alert changes or is dismissed, or a limit guard hold changes; the browser refreshes Inbox on those changes and after reconnect. Server shutdown closes this socket with 1001 (server restarting), so clients reconnect quietly |
| GET | `/api/events/cursor` | — | `{seq}` current global event watermark |
| GET | `/api/inbox` | — | open questions, approvals, recovery, `pool_change` items (owner and admins only; each lists the `pool_agents` a pool change would switch) and delivery failures visible to authorized project members and private-chat peers, plus superseded agent asks (with `state: superseded`) so the chat can still answer them, and, for global admins, usage alerts (`usage_alert`, open) and usage reset notices (`usage_notice`, `state: notice`, shown as a toast); the Inbox list and badge count only open entries. Each stored entry includes the caller's `seen` flag, which does not remove it from the Inbox. An `approval` entry with `note_id` is a shared agent note waiting for approval (only the owner and admins who own the agent's project see it; its card sits in the project's home room): `POST /api/inbox/{item}/decision` with choice `approve` switches the note on for every project's agents, `deny` deletes it, and either closes the card, as does any other change to the note |
| GET | `/api/agent/rooms` | active or not-accepting agent key | rooms where the caller is an active member, including archived rooms; no message previews |
| GET/POST | `/api/agent/rooms/{room}/schedules` | joined agent key; POST requires active lead, schedule fields above and `Idempotency-Key` | list or create schedules in a joined task room or the project home room |
| POST | `/api/agent/rooms` | active lead key; `{title,parent_id?,worktree?:{enabled,base_ref?}}` and `Idempotency-Key` | create task room beneath the home room by default (also in the dispatcher flow) or a joined parent, join the lead, and post one parent-room system notice |
| POST | `/api/agent/rooms/{room}/close` | active lead key; `{lifecycle_rev,cascade?}` and `Idempotency-Key` | begin the ordinary closing grace period for a joined task room; parent-room notice and agent actor audit |
| GET | `/api/agent/usage-left` | agent key | project live-agent and spawn-rate headroom plus provider session/weekly percent left and reset time; provider status is `unknown` when no snapshot is available |
| POST | `/api/agent/commands` | agent key; `{cmd,cwd?,timeout_seconds?,label?,room_id?}` and `Idempotency-Key` | `run_command`: start one long command outside the caller's turn, under the supervisor in the agent's own checkout (`cwd` stays inside it; timeout defaults to 3600 s, cap 21600 s). The room (where the completion note goes) defaults to the room the caller's current turn runs in; that room may be one of the agent's project or a chat whose other agent members all belong to that project (its people are members of it, or the instance owner) — the command always runs in the agent's own checkout, the room only decides where the note goes — and the caller must be a member of it. A chat with an agent of another project is refused (`403 forbidden`: "run commands from a room of your own project; this chat includes another project's agent", the note would move one project's output into another's), and so is any other room of another project (`403 forbidden` naming what is wrong and what to do). `cwd` is cleaned and refused when it leaves the checkout (`..`, `./..`, `a/../..`: `400 invalid_command`). 201 returns the job once the machine confirmed the start (already `done` for a command that ended first). When the machine was asked but did not answer (timeout, disconnect), the answer is 202 with the job still `queued`, `start_unconfirmed: true` and a `warning`: it may be running, its end still wakes the agent, and the machine's next hello or the unconfirmed-job sweep (after 60 s, once the request can no longer be answered, and after 10 minutes in any case) settles it (`running`, or `failed` when it never started; a later start or end from the machine still corrects that). At most two run per agent (`409 command_limit`), and a machine whose supervisor predates commands answers `errCommandUnsupported`; a Docker agent's command on a supervisor that does not report `supports_container_commands` is refused (`409 command_refused`, update aimebu on that machine) rather than run on the host; a containerized launcher (or a `docker`-variant agent's Docker harness) runs the command in the agent's container through harness-docker's command mode; one without the `run_command` feature is refused with the update to make (`409 command_refused`, the job recorded `failed`, nothing woken), and so is one whose launcher is not on the machine. A container on an image from before command runs makes the launcher refuse after the start: the job ends `failed` with "rebuild the harness image" and wakes the agent; a Docker kill the launcher cannot confirm ends `cancelled_unconfirmed` ([supervisor](supervisor.md#commands-outside-a-turn)). Keyed replay returns the first job |
| GET | `/api/agent/commands` | agent key | `list_commands`: the caller's own jobs (running and finished), newest first: state, exit code, duration, log path |
| GET | `/api/agent/commands/{id}/log` | agent key; `max_bytes` (1–1 MiB, default 64 KiB) | `command_log`: a bounded tail of one job's log, read on the machine that ran it and redacted there; another agent's job answers 404. For a running job, a repeated read in the same turn with unchanged output adds a `hint` to end the turn and wait for the completion wake; after three unchanged repeats it says to stop polling. New output, a new turn, or a finished job resets the advice. |
| POST | `/api/agent/commands/{id}/cancel` | agent key and `Idempotency-Key` | `cancel_command`: end one of the caller's jobs (SIGTERM, SIGKILL after 8 s); its end still arrives as a wake note. A job already finished is returned unchanged. A job whose start is unconfirmed (`queued`) is returned `cancelled` at once, with no wake note, and the machine is sent a cancel as well; if the command had already finished there, its real end replaces the cancel and wakes the caller |
| GET | `/api/agent/handbook` | agent key | project lead (`cap_is_lead`) reads the full handbook; a dispatcher in the leader-dispatcher flow reads only Current state and Open backlog; other agents receive 403 |
| PUT | `/api/agent/handbook` | agent key; `{section,content?,patch?,rev}` with exactly one of `content` (replace) or `patch` (append) | lead only; section-scoped optimistic edit returns the full document and budget status; stale rev returns 409 `{error:"stale_rev",current_rev}` |
| POST | `/api/agent/memory` | agent key; `{scope:"project"|"task"|"user_profile"|"agent_shared_notes",room_id?,scope_key?,body,source_message_id?}` | created fact, profile, shared note, or task note (a profile only for an active member of the agent's project, else 403; a shared note is created inactive and waits for the owner or an admin to switch it on, as does a change to one; an agent edits or deletes only the shared notes it wrote); source ID is accepted for project/global records only and must belong to a joined, memory-enabled room in the caller's project |
| GET | `/api/agent/memory` | agent key; `scope`, optional `room_id` or `scope_key` | list caller-visible records in one scope; project and global results include active records only |
| PUT/DELETE | `/api/agent/memory/{id}` | agent key; scope query as above; PUT `{body,rev}` or DELETE `?rev=…` | revised or removed record; stale revision returns `current` |
| GET | `/api/agent/recall` | agent key; `query`, optional `limit` (1–10) | ranked matching project facts, joined task notes, and joined room messages |
| GET | `/api/agent/macros` | agent key | read-only shared composer macro map (managed agent `list_macros`) |
| POST | `/api/agent/agents` | agent key; `{room_id,role_key,tool?,variant?,model?,launcher_id?,provider?,machine_id?,reason?,worktree?,base_ref?,approved_by?}` and `Idempotency-Key` | lead-only individual agent spawn; omitted harness/model uses the next available role default, while an explicit deviation from an available default needs `reason` (`400 reason_required`) and appears in the room notice; `launcher_id` names the launcher to run the agent on and is validated like on the person's spawn route (an unknown one answers 409 `launcher_unavailable`); a harness choice (any of `tool`, `variant`, `model`, `launcher_id`) outside the role's configured `default_agents` pool is refused with 409 `model_not_approved` unless `approved_by` names a person's message from the last 7 days in a room the requesting agent may read, and its body names the requested model ID or launcher model label, case-insensitively; any tool or variant it names must match the request (its number `#N` or id may be used; an unrelated, expired, unreadable, agent-authored, unknown, or other-project message is refused with the reason named, and a role with no configured pool has nothing to be outside of and keeps the `reason` rule); the gate covers a lead's own spawns the same way (a lead spawning a researcher outside the pool needs a person's `approved_by` too); an in-pool choice is always allowed, with or without `approved_by`; the validated message id is recorded on the agent and returned on its card as `approved_by`; a person's own spawn (the room route) is never gated; a singleton role already held in the project answers `409 role_occupied`, and the agent or spawn rate limit `429 spawn_limit`. The response includes `default_used` and `default_selection` when applicable, plus `skipped` — the pool entries the default pick passed over, each `{launcher_id,tool,variant,model,provider,reason}` with reason `login_rejected` when the provider's active login is already known rejected in the usage snapshot (`auth_missing`, or an expired token) — and `all_unhealthy` when every configured entry was skipped and the pool fell back to the first skipped entry in rotation order instead. The room notice names the skips the same way ("spawned per role default: … (skipped codex/native/gpt-6-sol: login rejected)"). An explicit `tool`/`variant`/`model` spawn is never affected |
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

A role may also carry a prompt **extension**: text added after the delivered instructions without replacing them, so an update to a
built-in default keeps flowing while the instance or a project adds its own rules. The delivered text is the base (the team flow's
role-text override where one exists, else the role's resolved body), then the role's global extension, then the project's, each
skipped when empty and joined by a blank line; the extension of the role an agent holds is the one that appends, so a parent's
extension does not flow to roles that extend it. The global extension is the instance owner's alone (`body_extension` on a role
write; an omitted field keeps what is saved); a project's is its owner's or operators'
(`PUT /api/projects/{project}/role-prompts/{role}`). An extension never counts as an edit of the body, so the role-body migrations
keep rewriting an unedited default; resetting a built-in role leaves its extension; deleting a role removes its extensions. Roles
have no change event, so an extension change shows in the row's revision and in the next delivery.

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
Settings → Agents → Macros is read-only for other users and editable by the instance
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
is not offered, and when two agents share a slug the form inserted tells them
apart — same-project duplicates (a reused name) as the ordinal `@slug-2` for
the later one in creation order (the first keeps `@slug`), different-project
agents as the full `@slug@<project_id>`. Sent messages highlight a mention only when it resolves under
the same order (group tag, unique slug/login, a slug that one agent shares
only with people's logins — the server addresses the agent and those people —,
`slug-2` for a same-project duplicate, `slug@<project id>`, else role key). This is narrower than the server, which also accepts member-ID tokens and
`slug@<project name>`; those go unhighlighted. Fenced, inline and indented code,
link text, and `\@` escapes are never highlighted. The server stores mentions
by agent ID and returns every message with the display names already rewritten
(a stored mention of an agent that can no longer be named stays ID text), so
the composer and the rows work on names alone.

Rooms are referenced by id (`#room_<id>`). While a `#` token is being typed the
composer offers the visible rooms (the sidebar's list) and inserts the picked
room's id; on send, and again on save, a typed `#<title>` that names exactly one
room the author can see (exact, case-insensitive) is resolved to that id — an
ambiguous title, a room the author cannot see, a bare `#<number>` message
reference and an id typed directly stay as written. A drawn row renders each
stored `#room_<id>` from the message's `entities` table: a named room as
`#<title>` linking to and opening the room (`#<title>-2` when two visible rooms share a
title in the view), an unnamed entry as plain `#private-room`, a missing entry
as plain `#unknown-room`; code and the raw Markdown view keep the ids.

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

The built-in coordinating role is `leader` (ID `role_leader_default`, label
`Leader`); `lead` is not an alias. Migration 102 moves built-in assignments
and role references to the new ID. If a global custom `leader` exists, it
is renamed to an available `leader-v1` key (or numbered variant), preserving
its body and agents; project-scoped collisions are logged and left intact.

Roles are global only and defined by the instance owner or an admin; changing a role's capabilities needs the same authority.
Migration 64 retired project roles: a live project role became global when no global role had its key (the first by id when several
projects defined it) but without its lead, can-spawn or writer capabilities, which the owner sets again in Settings → Agents → Roles; any other was dropped with a log line and its agents moved to the global role of that key, and a deleted one stays
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
then moves the checkout, rotates the agent key, and
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
Inbox entries include `room_lifecycle` so items in a room that is closing can
be labeled "Room is being archived". A **closed** room's items are never
listed (nor counted): closing a room settles its recovery and delivery_failed
items, and the list skips any that remain.
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
0=Sunday through 6=Saturday. `kind=cron` uses `cron`, a five-field expression
`minute hour day-of-month month day-of-week` (or `@hourly`, `@daily`, `@weekly`,
`@monthly`, `@yearly`) with `*`, lists (`1,15`), ranges (`mon-fri`), steps
(`*/15`, `0-30/10`) and `jan`/`mon` names, for example `0 8,12,16,20 * * *`;
sunday is 0 or 7, and when both day-of-month and day-of-week are restricted
either one matches (Vixie cron). It runs in the named `timezone`; a bad field is
a 400 `invalid_schedule` naming it (for example `cron minute: 60 is out of range 0-59`),
and the response carries `cron`. Responses include UTC `next_run_at`, creator,
target, last outcome/reason, and revision. Pausing or resuming uses a matching
revision. Closing a room disables schedules, and reopening does not resume
them; the agents among each schedule's creator and target (the room lead for a
lead schedule) get one system note saying it stopped, in an open room they share
(or each in an open room it is a member of; one in no open room gets none). The
note leaves the schedule's text and the room title out, naming only its ID and
saying its room closed, when the closed room is restricted or a private chat. A delayed server catches up one occurrence per schedule. While a gate
or target prevents delivery, runs are skipped, recorded, and surfaced by a
room notice on each changed skip reason. A spring DST gap advances to the
next valid local minute; a fall overlap runs at its first occurrence only. The same holds for cron:
a local time the gap skips runs at the first valid minute after it (two skipped
slots run once), and a repeated local time runs once; a delayed server fires a
cron schedule once, not once per missed slot.

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

The usage endpoint (the owner's whole snapshot, an admin's redacted one) reads the v2 cache under `AIMEBU_CONFIG_DIR/usages/`.
Set `AIMEBU_USAGES=0` to disable credential reads and background requests;
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
1–99, critical below warning), the three opt-in alert toggles
`alert_pace_enabled`, `alert_reset_notice_enabled` and `alert_expiry_enabled`
(all off by default, server-wide once on), or the
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
reset time; it carries no email or credential. The entry also carries
`provider`, `profile` and `window` (the provider's name, the profile's name and
the window's key, as `GET /api/usages` keys them; empty for an expiry alert) so
the browser can draw that window's usage chart in the card and focus its profile
in the Usages panel. The owner alone can open the Usages panel from that entry. A critical alert supersedes a warning for the
same window, and each threshold fires once per reset cycle. The server stores
this dedupe state in private `usages/alerts.json` rather than SQLite. The
alert closes when its reset time passes, its account disappears, or the
provider is disabled. The Inbox card has a dismiss control; dismissing does not rearm
that threshold in the same cycle. A renamed stored profile keeps its opaque
account identity. Invalid saved thresholds fall back to 20/10 on load while
other saved usage settings and credentials remain intact.
Behind the three opt-in alert toggles, a `usage_alert` row can also carry a
`pace` tier (one per risk episode while a fresh Codex or Claude Code session
or weekly window is projected to run out before its reset, re-arming only
after recovery) or an `expiring`/`expired` tier (from the same expiry data
as the Usages token-expiry badge, one per account and expiry time, replaced
by the stronger wording once the expiry passes; no reset time, closed when
the credential is renewed). A pace alert's question names the window and its
reset time; a reset notice says the allowance started over; an expiry alert
says when the token expires or that it has expired, and names the CLI to run.
An active limit guard hides the threshold and pace rows of its tool, never
the reset or expiry rows; each toggle going off closes the rows its alerts
had opened.
A `usage_alert` row can also carry a `login_rejected` tier, which is not
behind a toggle: one per rejection episode of a profile whose usage poll got
HTTP 401 or 403 (`auth_missing`, or the rejected `scope_missing`). Its
question names the tool, the profile and how to log in again (`codex login`
for Codex, `claude /login` for Claude Code; the other providers their own
rejection message); it closes when the profile reads `ok` again or its
account disappears, an inactive stored profile gets only the
`usage_login_rejected` system event, and the episode state is stored beside
the other alert state in `usages/alerts.json`, so a restart inside an episode
raises nothing. The row and the event say when the poller first saw the
rejection (`first_seen`, RFC 3339, not the time of the last poll) and name
the rejected credential by the last six hex characters of its hash
(`token_tail`), never the credential itself and never a prefix of it.
A window reset (the `alert_reset_notice_enabled` toggle, one per window and
reset cycle) is not an Inbox item: `GET /api/inbox` carries it for the same
readers as a `usage_notice` row with `state: "notice"` for an hour after it
was observed, then drops it on the next usage refresh. The Inbox list and
count leave it out; the browser shows a notice it had not read before once as
a toast, with a silent desktop notification while the tab is hidden. A reset
`usage_alert` an earlier version left open is not listed and is dropped on the
next usage refresh.

Project responses set `repo_url` to `null` when absent. Message responses
expose `targets` as a JSON array and omit the raw `targets_json` column.
For a resolved question, the message view includes `answer_state: "resolved"`,
`answer_body`, `answer_message_no` (the reply's `#N`), and
`answer_author_name` (the answering person's login); the message view keeps `answer_state` for an unanswered one too (`open`, `superseded`, `withdrawn`), and the chat draws the choices of a message off, with the chosen one marked, from `resolved`. `GET /api/inbox` also lists, next to the superseded asks, a `withdrawn` question or approval that has structured choices (proposed answers or open questions, no explicit `ask`), while its room is open and its agent exists; `POST /api/inbox/{item}/answer` resolves it like a superseded one (a withdrawn item of any other kind, a closed room or a removed agent still answers `already_withdrawn` or `invalid_transition`). The Inbox view lists open items only, as before.
Project room lists and single-room details include read-only `member_count`
(active project humans, or a private chat's own people, plus room agents; a disabled
user counts nowhere), `message_count`, `user_message_count` (the messages that are not system notices: a room whose newest such message has only an attachment has an empty preview but a count above zero), and an optional
`last_message_body` preview (at most 160 characters of the newest message that is not a system notice; absent when the room has only notices) and `last_activity_at` (the newest message's time, notices included, else the room's creation, which orders the room list). Both include the
signed-in user's `hidden`, `notify` (`default` until they choose) and `muted_until` (omitted unless a mute is set; it may be in the past) preferences, plus per-user read state:
`read_cursor` (highest read message `seq`), `unread_count` (messages after the
cursor; posting a message advances the author's own cursor to it; in a room whose `notify` is `mentions`, only the messages that address the user, which `POST /api/rooms/{room}/read` answers the same way), and `attention_count` (open Inbox entries
the user can see in that room and has not seen yet). A person's list also carries what the sidebar draws a room's
status from: `unread_addressed` (the part of `unread_count` made of messages that name the user or reply to them, whatever the room's `notify` level),
`attention_open` (open Inbox entries the user can see in the room, seen or not, so a room still reads as waiting for a person after they looked),
`working_agents` (how many distinct agents have a pending or running turn in the room), and `working` (true while `working_agents` is above zero), and `finished_unseen` (true while an agent that is not in a turn has written a message for the person, one that names them or replies to them or asks for attention, after the person's read cursor in the room: the agent finished and they have not seen it) `purpose` says what a room is for: `task` (the default; every
home room, private chat and room made without one), `research` (a read-only investigation that ends in a report) or `general` (the project-wide and
coordination rooms: a team flow's `general`, `routing` and `advice` rooms, and the room every project starts with). It is chosen when the room is made (the
`purpose` of `create_room` or of the create-room request, only for a task room) and never worked out from who is in it; migration 126
backfilled it for existing rooms from the team-flow key, the room's title `general`, and a task room whose only agents besides leads are researchers. The
sidebar shows a task or research room's `unread_addressed` in place of `unread_count`, and a status word (Closing, Waiting, Working, Finished, Done or Idle) in place of
its member count, which moves into the row's tooltip. Read
cursors and per-person Inbox seen flags are stored on the server
(`room_read_cursor` and `inbox_item_seen`), so both counts are shared across
browsers and devices. The web UI marks the active room read
through `POST /api/rooms/{room}/read` (Mark unread in a room row's or a message's context menu moves it back through `POST /api/rooms/{room}/unread`) and shows a 🔔 for unseen open items;
the project home row totals them across its non-DM rooms. Migration 43 baselines existing rooms as read; a room or
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
usage for longer than `AIMEBU_STUCK_MINUTES` (default 10). The UI refreshes
time-derived flags every 30 seconds. It reads limit guard state once at sign-in,
on guard events, and every 30 seconds while the Health panel is open. The top-bar health list shows one plain
sentence per flag ("Ada looks stuck", "An answer to Bert could not be delivered",
"Bert needs a recovery decision"); choosing one opens the matching Inbox item
(variant A) or the room with its inline callout (variant B) for `recovery_waiting`
and `held_answer`, and the agent's room for the other flags.
Listed room agent cards also carry `finished_unseen` (true, omitted otherwise) **for the asking person**: the agent has no pending or running turn and has written a message for that person (one that names them or replies to them, or that asks for attention) after the person's read cursor in the room. It is derived from the read cursor (`room_read_cursor`) on every read, never stored, so it clears exactly when that person's cursor reaches the message (`POST /api/rooms/{room}/read`, shown live through `read_cursor_moved`) and comes back with Mark unread. A reconnect or an event-stream resume never moves the cursor, so it does not clear it; each person in the room has their own cursor and so their own answer. A worker's completion hands off to its coordinator (a lead or the dispatcher) and targets that agent, not the person, so it never sets the state for the person. The agent card says "finished" in place of "idle" (dot and badge stay the idle ones); an agent that is working, paused, stopped, held or waiting on a recovery decision keeps that state.
Listed room agent cards also carry an optional `hold` (`{code, text, detail}`) when an agent that is idle and accepting
work will not start its next turn because the limit guard is holding new turns of its tool: `code` is `limit_guard`, `text` the
short form ("claude session 95%": the tool, the fuller window of the active account and how full it is) and `detail` the sentence
behind it. The guard judges the host machine's account, so only agents on the host machine have it, and an agent that is working,
paused, stopped, lost or waiting on a recovery decision does not. A `hold` with `code` `docker_only` (text "native launcher refused") marks any agent that is not stopped on a native launcher while "Only run agents in docker" is on, whatever its machine or state. The card shows an agent that needs a recovery decision
(`recovery_block`) or has a `hold` as an orange dot with the state "needs decision" or "held: claude session 95%" and the reason on
the card, instead of the green "idle" of an agent that is free; a lost process stays red.
Listed room agent cards also carry an optional `warm` (`{state, sleeps_at, reason?}`) for an active Claude agent or opted-in Codex agent on a machine that keeps processes warm: `state` is `warm` (with
`sleeps_at`, RFC 3339, when its process goes to sleep) while the supervisor reports a live idle process for it, `working` while its live warm process runs a turn, `sleeping` when there is none and the supervisor says it works to a
warm window above 0, and `unknown` when a report lacks `idle_seconds` from an older supervisor, which a card
shows as neither warm nor sleeping. A `sleeping` card may carry `reason` when the last turn could not run warm; project owners and operators see the supervisor's classified configuration reason, while viewers see a generic configuration message. The reason clears on the next turn and when the agent stops. Active agents on harnesses without a warm process (every harness except Claude and opted-in Codex) carry `warm.state: "na"`; their idle cards say “Warm n/a” with a neutral dot. Before the first supervisor report the field is absent and the card stays cold. While an agent is in a turn, the field is absent unless the supervisor reports its live warm process as `working`; it is also absent while the observed state is `interrupting`, `lost`, or any state other than `idle` or `in_turn`, and when warm mode is off (the window the supervisor reports is 0). The machine rows of `GET /api/machines` carry an optional `warm` (`{idle, busy, started, reused,
rss_bytes, container_wide_bytes, unconfirmed_ping_ends?}`; `unconfirmed_ping_ends` counts keep-alive pings in a container whose end was never confirmed
and whose quarantine ran out, present only when above 0) for a connected machine whose supervisor keeps processes warm; the memory is not per agent
(`rss_bytes` is the processes the supervisor runs itself, `container_wide_bytes` everything running in containers).
Listed room agent cards also carry an optional `keep_alive` for an active Claude agent on a machine that keeps processes warm, when the owner
has the cache keep-alive on: `{interval_minutes, pings_per_stretch, estimate_usd?, last_at?, last_ok?, last_error?, cache_read?, cache_write?,
missed?, last_ping_at?, stretch_active}` (`estimate_usd` is the cost of a stretch of pings, measured from the last ping when there is one, else the price list's; `missed` says the
last ping wrote to the cache what it should have read; `last_ping_at` is the last ping's time, and `stretch_active` says more pings are planned: the last ping is under two intervals old and the next falls before the limit of hours without real work; the card shows the miss or failure warning, with the ping's local time, only while it is true). `GET /api/agent-isolation` also returns `warm_idle_minutes` (the warm window: the owner's saved value, else the default of 5; 0 is off) with
`warm_idle_saved` (whether the owner has saved one; the owner changes it with `PUT {warm_idle_minutes}`), `codex_warm_enabled` (owner opt-in for Codex app-server warm mode, default false), and `keep_alive: {enabled, claude_minutes,
codex_minutes, stop_after_hours}` (defaults on, 50, 10, 24), which the owner changes with `PUT {keep_alive: {...}}` (any of the fields; 0 minutes is
off for that harness; the bounds are 0 to 55 minutes for Claude, 0 to 1440 for Codex and 0 to 720 hours; another user gets 403).
Listed room agent cards also include optional `latest_usage` with nullable
`tokens_in`, `tokens_out`, `tokens_cache_read`, `tokens_cache_write`,
`context_used`, `context_window`, and
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
A manual context clear records notable `agent_context_cleared` with the requesting
user. A harness-reported compaction records notable `agent_context_compacted`.
Both appear in the bell and post one system notice in the agent's room.
Inbox entries include `agent_id` when present. Recovery rows include
`turn_id` for uncertain turns and `diagnostic_report` when available. Rows
without a `turn_id` are loop budget items (reason `turn_budget`); a worker's
is routed to its manager like any worker recovery, and only a wake caused by
another agent's message counts toward the budget.
Posting an inbox answer resolves an open question or approval and queues the
requester in the same transaction. Project owners and operators may answer
questions; only project owners may answer ordinary approvals outside private
chats. In a private chat its human peer may answer either ordinary kind
regardless of project role. Boundary admission remains owner-only. A repeated
idempotency key returns the original message; a changed
answer with that key returns `conflict`. A stale revision returns `stale_rev`.
An ordinary agent ask is superseded when a later message from a person or the
asking agent commits in the same room. System messages and messages in other
rooms leave it open. Superseding only removes the ask from the Inbox listing
and the unseen count: its chat answer controls stay active and look like an
open ask's, and `POST /api/inbox/{item}/answer` still accepts the answer,
resolves the item, and reaches the requester as usual. "No longer pending" in
chat marks a resolved or withdrawn item only. Existing open asks with such a
later message are superseded once by migration 117.
A message with `needs_attention`, `proposed_answers` or `open_questions` and no
`ask` creates its item in any room (the chat shows the card's controls).
`GET /api/inbox` returns it either way, with `inbox_hidden: true` outside the
project's home room; the Inbox view omits a hidden item and the per-room
attention counts, the read cursor's attention count and the seen-marking exclude
it. A real `ask`, a recovery and the other kinds are never hidden. A question with no structured `ask` is narrower, because it is a message
in a conversation, and a person usually answers it in the chat: any later
message from a person in the same room supersedes it only when it replies
anywhere in the card's thread (`answered in thread`) or when it is a plain message (a reply to nothing) and the room's only open
card of this kind (`answered in room`); with two or more open, a plain message
settles none of them. A card with structured answers (`proposed_answers` or
`open_questions`) waits for its answer: the plain-message room rule never
settles it, only a reply in its thread, an answer, or a newer card. Only a message as it is written applies the room rule; the
startup sweep settles by thread reply alone. The asking agent supersedes it with its own newer message replying to it, and an
agent's newer card in the same room (an `ask`, or a message with
`needs_attention`, `proposed_answers` or `open_questions`) supersedes every
older open card of that agent in that room, structured or not (`newer card`);
another agent's card, or a card in another room, does not. The periodic sweep
applies the newer-card rule to cards opened before it existed. The
`inbox_item_resolved` event carries `reason` (`answered in thread`,
`answered in room`, `newer card` or `newer message`).

Items nobody can act on settle without a person. Closing a room (when it
becomes `closed`) and stopping an agent resolve the open `recovery` and
`delivery_failed` items concerned: a failed delivery is dismissed, a recovery
is abandoned like a person's Abandon (notes aborted, recovery block lifted,
turn settled) and a recovery waiting for the old process is dropped with its
continuation. `resolved_by` is `system (room closed)` or `system (agent
stopped)` and the event carries `reason`. A recovery for a turn that is `lost`
with nothing proving its process gone stays for a person, since only a
decision can accept that overlap. The dispatcher also runs this settling (and
the answered-question check above) every ten seconds, which catches a turn
whose kill raises its recovery only after the stop, and items opened before
the rule existed. An invalid optional decoration (`proposed_answers`, `open_questions`, `visual_plan`, `appendix_pages`, or an `ask` with attention controls) never holds an answer: it is dropped and the receipt carries a warning naming the field and the rule. A malformed final answer never reaches people first, a lead's included (a lead with nobody above it is told the same way and gets the card and room notice only if its resend is held too). The author gets
the system note "Your previous reply was held: <error>. Send your final answer
again…" — the error names the exact cause (which message, which field, what was
wrong and how to fix it, for example "message 0 is a handoff without an
outcome; set outcome to done, partial or failed"), while
anything that would quote the agent's own text stays out (the note is readable
in the room) — and is woken by it to resend, and no
card is raised. When the note cannot wake the agent into a repair turn at all
(stopped or not accepting, blocked, recorded lost, or its machine unseen for
five minutes), the failure surfaces at once instead — the card for a lead or a
worker with no lead, the lead's `answer_held` recovery for a worker with a
lead — since the second hold can never happen; the note still waits for when
the agent can act again. When that does not settle it (the resent answer is held too)
the failure goes to the project lead as a `delivery_failed` item with phase
`answer_held` (choices Retry and Abandon, for the lead through `resolve_recovery` and for people after escalation), routed and
escalated like any worker recovery; the lead's note carries the same reason
(`Reason: …`) as its evidence, and the lead's Retry sends the worker a
correction note that repeats it. Two empty answers (`invalid_answer: EOF`) in
a row from one agent, with no tool activity in either turn, are a broken
session: the lead gets one `recovery` item with phase `broken_session`
(choices `restart`, for the lead through `resolve_recovery` and for people after escalation, which clears the native session so the next turn starts
fresh, and `abandon`), and no correction note is sent. A turn that ends with a context-length error (OpenCode `ContextOverflowError`, Claude's "Prompt is too long", Codex's "ran out of room in the model's context window", LiteLLM's `ContextWindowExceededError`, the API's `context_length_exceeded`; matched by `adapter.ContextOverflow` on the harness's error text, never on a bare "context") is a broken session too, because Resume and Retry would hit the same limit at once: its recovery item has phase `broken_session` and choices `restart` and `abandon` only, and no diagnostic turn is run. `restart` starts a fresh native session in the same checkout (the turn's notes run again on it) and sends it one system note, "Handoff: …", naming the checkout's last commit and state when the turn ended (the supervisor's `tip <sha> <subject>, tree clean` description, which it reports for these exits like it does for a limit kill) and the agent's commands that are still queued or running; `abandon` releases the agent as any abandoned turn does. Only a lead's own
malformed answer, or a worker's with no lead to hand to, raises the card for
people. Only invalid-answer holds go to the lead: `room_closed` and `not_member` holds are never re-asked and stay with people. A recovery the settle pass left open on purpose stays listed even when its room is closed. Every empty answer logs the harness exit code and the last stderr
lines the server holds for the turn.
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
button label. A lead deciding an item that was resolved while it was unassigned (the sweep had handed it to people) gets `409 already_resolved`
naming who resolved it and how, instead of a bare refusal. A worker recovery first goes to whoever manages the worker — the agent that spawned it (a lead, or a role that can spawn, such as a flow's dispatcher) while it is active and healthy, otherwise an active project lead — when enabled; the manager gets a `worker_recovery` note (with the failed turn's `worker_output`, see the delivery note below) and may use `list_recoveries` and `resolve_recovery` (`restart` is the fresh-session choice of a broken session; a held answer takes `retry` or `abandon`; a loop-budget pause takes `continue` or `abandon`). It stays out of the people's Inbox until the configured interval elapses or the manager becomes unavailable, when a human card and notification appear. The lead's own recovery goes to people immediately. A worker paused by the loop budget (60 turns woken by another agent's message in one room within ten minutes; other wakes, including a finished `run_command` or a person's message, never count) raises the same kind of item (reason `turn_budget`, no `turn_id`) and is routed to its manager the same way, except in the project's home room, where it stays with people. Project owners and operators may decide human-routed recovery in every room.
The human peer can view a private-chat recovery item, but only a project owner
or operator may decide it. Owners and operators can see those recovery items
without joining the private chat because recovery blocks the agent across
rooms. The inbox row's `can_decide` field lets the UI show peer recovery as
read-only. Owners and operators receive recovery inbox events for those
private chats without receiving their ordinary room events or messages.
A folder-boundary approval uses the same decision endpoint with
`choice:"approve"|"deny"`. Only the project owner may decide it. The item
includes `boundary_pending`, `boundary_machine`, `boundary_path`, and
`boundary_requester`; agent cards carry `resumed_from_legacy_id` when the agent continues an imported v1 agent (the web shows that agent's presence on the v1 messages still under the v1 author), and `boundary_pending` until the request
is resolved and `boundary_grants` lists prior machine/path approvals. Agent
cards also carry `approved_by` when the agent was started by another agent on
a harness or model outside its role's pool that a person's message approved
(the `agent_spawn_requested` event carries it too). The
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
notes without changing the agent's desired state. `abandon_stop` (Abandon and stop, offered next to Abandon on every recovery item, and accepted by `POST /api/inbox/{item}/decision` and by the lead's `resolve_recovery`) does the Abandon and sets the agent's desired state to `stopped` in the same transaction, so both happen or neither does; for a lead it needs the permission `stop_agent` needs (`403 forbidden` otherwise, the item stays open). A recovery item lists `withdraw_notes` and `withdraw_rooms`: how many queued notes the Abandon would withdraw (every queued note of the agent, in any room) and in how many rooms, a snapshot taken when the item is read. Continue resets the loop
budget and makes the agent active again. Resume/Retry while the agent is not
active return `invalid_transition` (409); Abandon remains available.
`auto_resume_after_clean_diagnostic` defaults to false. Enabling it can
repeat an action: a model's clean report may miss unobserved effects such as
a network write. The server uses the setting snapshotted at diagnostic
admission and records `system (auto-resume setting)` as the decision actor.
Delivery-failure items link to the room and include `held_reason` and a
redacted `delivery_error` from the parent or correction receipt, plus a
240-character candidate preview with credentials redacted. The originating
room receives a system notice with the fixed held reason when open (when only some messages of a reply were refused for their target, the rest is delivered and only the notice, with no inbox item, says which messages were held);
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
Switch, or Retry now clears a limit without a known reset. An account switch
of the tool on the host (web or automatic) lifts the limits of the host's agents
of that tool, since they belonged to the old login, and a limit in force marks the
host's active profile exhausted for the [quota switcher](quota-switcher.md). A general
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
project live-agent and per-minute spawn limits. Multiple workers can use the
same checkout. A task-room Git worktree is optional; if the folder is not a
Git repository, the agent starts in the project folder without one.
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
an active leader, or an active coordinator allowed by the selected flow's
spawn rules. `spawn_agent` also requires the role's spawn capability and an
allowed actor-to-target role pair. In Custom, the prior lead rules apply.
`spawn_agent` accepts an optional `join_parent: true` (JSON field of `POST /api/agent/agents`): the new agent also joins its room's parent room as a quiet (`auto`) member, so it can post there (a review worker posting findings to `general`). Active lead or dispatcher only (otherwise 403); the flow's join rules apply, and a refusal (parent is home, not open, absent, or excludes the role) fails the spawn with 409 `join_parent_refused`.
`POST /api/agent/dms` (`{target_agent_id,body}`, where `target_agent_id` is an agent ID or `slug@project-slug`, the project's name or ID; `Idempotency-Key` required; the
`dm_agent` tool) sends a private message and returns
`{room_id,message_id,no,created}`. An active lead may message any agent of
its own project and, in another project, only that project's active lead (any other
target there answers 403 "across projects you may message only that project's
lead", after the reach check below). Any other active agent (a worker, reviewer,
researcher...) may message only its own project's lead or dispatcher; every other
target answers 403, and the reach rule below does not apply to it. An active flow coordinator that is not a lead (the dispatcher in the
leader-dispatcher flow) may message only an agent of its own project that it
manages: a role its flow's spawn rules let it target, an agent it spawned, or
an agent whose home room it created; any other target answers 403. Who may is decided by the
server in the same transaction, never the model: every person whose message
caused the lead's current turn (and the turns it continues) must be an owner or
operator of the target's project, or the instance owner; when no person caused
it (an agent or a schedule woke the lead) an owner of the lead's own project
must be. A viewer never counts. A `slug@project` target is resolved only among projects the
people behind the turn may reach: when they reach the named project but it has no
live agent of that slug the answer is 404 `not_found` ("no agent wren in project
aimebu"); when two reachable projects share the name and both have the slug, 409
`ambiguous_agent` lists the agent IDs (only reachable ones). Otherwise unknown or
out-of-reach targets answer 403 alike, so nothing is confirmed about a project the
people cannot reach; 409 `agent_unavailable` (revoked, stopped or not accepting), 409
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
agent stays `dm_agent`.
`create_room` takes an optional `purpose` (`task`, the default, `research` or `general`; see `purpose` above; 400 `invalid_request` for another value), records the agent in `created_by` and joins eligible lead roles atomically (a project's leader is in every open room in Custom and Leader and workers; in Leader and dispatcher, the leader is confined to home and routing, except that it may create rooms with `purpose: research` (under home or general) and is then a member of them, while any other purpose it asks for is refused with 409 `team_flow_rule`, and the dispatcher starts in routing and joins task rooms it creates or explicitly joins; general has no agent by default; private chats are not included), and
posts one system notice in the parent room. Its response carries the room's
real `member_count`, which includes the creating agent. `close_room` accepts a
task room the agent belongs to or created, and uses the same
`lifecycle_rev`, child-room check, cascade option, grace period and control
delivery as human archive. Stopping an agent (`stop_agent`, or the stops a
room close makes) is allowed for a role the flow's spawn rules let the actor
target, and also for an agent the actor spawned or whose home room it
created, so a leader keeps control of its own pre-switch rooms and workers
after a switch to the dispatcher flow; otherwise 409 `team_flow_rule`.
`GET|POST /api/agent/rooms/{room}/schedules` (`list_schedules`,
`create_schedule`) and `DELETE /api/agent/rooms/{room}/schedules/{id}`
(`delete_schedule`, 200 `{deleted}`) require membership of a task or home room
of the agent's project; creating and deleting also require an active lead or
flow coordinator. An agent deletes only schedules agents created (403
otherwise); an unknown schedule is 404 on both the agent and the people's
delete route. Repeated request keys replay the original result
without a second notice. Workers can list their rooms, check usage, and use
memory, but cannot manage rooms and agents.
The built-in lead role coordinates a home-room request through a new task room:
it frames the evidence and plan first, asks the human which agents to use
through a structured inbox question, spawns them one at a time, follows their
handoffs and independent reviews, summarizes in the home room, and archives
the task room. For ordinary requests, the lead delegates implementation through
a task room and `spawn_agent`. When a human explicitly assigns a small, bounded
task to the lead itself, the lead does it directly, including file changes. If
delegated work cannot be staffed, the lead reports the blocker and asks how to
proceed. This is guidance in the role body, not a permission the server enforces.
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
it spawns one agent per call. When the target room already has an active idle
agent of the same role, the agent route returns 409 `idle_agent_exists` with
that agent's slug and id; a non-empty `reason` permits a deliberate duplicate.
The person's spawn route is unaffected. A project
room may also request `worktree:true` and optional `base_ref` on
`create_room` or `spawn_agent`; a new agent then uses the room's worktree
checkout on its selected machine. A dirty base contributes only its changed
file count to the API response and a room notice; uncommitted content stays
in the base checkout.
The lead must currently be `active` to spawn or stop. An agent lead cannot
spawn another lead or stop a different lead; it may stop itself. Notices use
agent slugs and roles, for example `tessa (leader) stopped drew (worker)`.
Provider quota uses the active profile snapshot for the caller's tool. A
cached or failed-refresh snapshot with windows is marked `stale`; missing
windows return `unknown`. It does not return credential or token material.
The `claude` tool uses the `claude-code` provider snapshot.
If spawn delivery fails or its acknowledgment is lost, the same key retries
the original agent's spawn until the supervisor confirms it. The API returns
`machine_unavailable` (503) while that machine is disconnected. A retry with
the same key resumes delivery of the original agent when it reconnects.
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
stays unsettled until journal reconciliation proves it never
started or a process exit confirms it stopped. A lost process cannot be
stopped through this endpoint until recovery resolves it. Reactivation also
checks the project's live-agent limit.

One concept, the reply: a message links to the one it answers with
`reply_to`, any message in the same room (a reply to a reply is fine). Its
`thread_root` is derived, the first message of the conversation, and comes back
in every message view; a writer never chooses it. An agent's answer may also
name the message by its number, as `#N` or a bare JSON number, resolved like a
`#N` reference. A `reply_to` that is missing or in another room returns
`invalid_answer` (400) for a person's post, and an agent's answer never loses
its message over it: the message is posted without the reply link and the
receipt carries a warning saying exactly why (`reply_to 48356 dropped: message
is in another room`, a room the agent is not a member of, or no message carries
the reference). An inbox answer replies to
the ask, so it lands in the ask's conversation.

An agent's single plain answer to the one person's message that woke its turn gets that message as `reply_to` when the agent named none; see the contract's agent I/O rules.

An agent is told what a message it receives answers: each delivered note says
why it reached the agent (`why`) and carries the message it replies to and the
first message of its conversation (`in_reply_to`, `conversation`: number,
author, an excerpt, `yours`), and the prompt asks the agent to set `reply_to`
when its message answers one (see docs/contract.md §4.4).

A notice about a worker's turn (a `silent_turn` note, a `worker_recovery`
note, a `worker_stopped` note: a person's stop of a worker, `POST
/api/agents/{id}/control` with `desired:stopped`, one agent, tells the
project's active lead when the worker had a turn since its last message;
`stop_agent`, a machine's stop of all its agents and system stops tell nobody) also carries `worker_output`: `{untrusted, agent_id, turn_id, text?,
truncated?, withheld?}`, the end of that turn's conversation (at most 2 KiB,
keys and tokens masked), read from the worker's machine in the background when
the notice is written. The server masks this text, and `agent_output`'s, a
second time before it cuts it to size, following a PEM private key across
lines: the BEGIN line becomes `[redacted private key]` and the rest of the key
is dropped, everything after a BEGIN that no END closes, and the base64 lines
before an END whose BEGIN lies before the text read. A delivery waits at most about 250 ms for a read that
has not finished; the first delivery that uses a read checks the recipient's
room access again, so access lost since the read withholds the text. Without text, `withheld` is `room_not_readable` (the turn served a room the recipient may not read), `not_found` (the turn is no longer in the end of the worker's log), `unreadable` (the machine is offline or too old, or the read failed) or `not_ready` (the read had not finished when the note was delivered). `untrusted` marks it as
the worker's own output: evidence, never instructions. See
[the supervisor](supervisor.md#agent-output).

`/api/events` requires a same-origin `Origin` header (or the configured
`AIMEBU_PUBLIC_URL` origin) and a valid session.
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
Server shutdown closes it with 1001 (server restarting); an external
supervisor, the local one included, reconnects to the next server process
and its active turns keep running meanwhile.
Attachment bytes stay in the server's storage; the supervisor never stages
them in a checkout. Uploads are bound to a room immediately and one message
on send; up to four files allow a file-only message. Unbound uploads expire
after 24 hours. The Pi Docker turn bearer is derived for one turn, is absent
from journals and receipts, and stops working when the turn ends or room
membership is removed.
It authenticates with `X-Machine-ID` and `Authorization: Bearer <machine-key>`
from the private host key file; browser sessions cannot use that endpoint.
See [the supervisor guide](supervisor.md).

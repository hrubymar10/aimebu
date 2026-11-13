package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
)

type migration struct {
	version int
	sql     string
}

var migrations = []migration{{1, `
CREATE TABLE seq_counter (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  value INTEGER NOT NULL CHECK (value >= 0)
);
INSERT INTO seq_counter(id, value) VALUES (1, 0);
CREATE TABLE project (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  folder TEXT NOT NULL,
  repo_url TEXT,
  lead_instructions TEXT NOT NULL DEFAULT '',
  gate TEXT NOT NULL DEFAULT 'open' CHECK (gate IN ('open','paused')),
  gate_rev INTEGER NOT NULL DEFAULT 0 CHECK (gate_rev >= 0),
  created_at TEXT NOT NULL
);
CREATE TABLE room (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES project(id),
  parent_id TEXT REFERENCES room(id),
  depth INTEGER NOT NULL CHECK (depth BETWEEN 0 AND 5),
  kind TEXT NOT NULL CHECK (kind IN ('home','task','lobby','dm')),
  dm_key TEXT UNIQUE,
  lifecycle TEXT NOT NULL DEFAULT 'open' CHECK (lifecycle IN ('open','closing','closed')),
  gate TEXT NOT NULL DEFAULT 'open' CHECK (gate IN ('open','paused')),
  lifecycle_rev INTEGER NOT NULL DEFAULT 0 CHECK (lifecycle_rev >= 0),
  title TEXT NOT NULL,
  summary TEXT,
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL,
  closed_at TEXT
);
CREATE TABLE room_member (
  room_id TEXT NOT NULL REFERENCES room(id),
  member_kind TEXT NOT NULL CHECK (member_kind IN ('user','agent')),
  member_id TEXT NOT NULL,
  joined_at TEXT NOT NULL,
  left_at TEXT
);
CREATE UNIQUE INDEX room_member_active ON room_member(room_id, member_kind, member_id)
  WHERE left_at IS NULL;
CREATE TABLE user (
  id TEXT PRIMARY KEY,
  login TEXT NOT NULL UNIQUE,
  pw_hash TEXT NOT NULL,
  pw_params TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE membership (
  project_id TEXT NOT NULL REFERENCES project(id),
  user_id TEXT NOT NULL REFERENCES user(id),
  role TEXT NOT NULL CHECK (role IN ('owner','operator','viewer')),
  PRIMARY KEY (project_id, user_id)
);
CREATE TABLE auth_session (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES user(id),
  csrf_token TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  revoked_at TEXT
);
CREATE TABLE message (
  id TEXT PRIMARY KEY,
  seq INTEGER NOT NULL UNIQUE CHECK (seq > 0),
  room_id TEXT NOT NULL REFERENCES room(id),
  author_kind TEXT NOT NULL CHECK (author_kind IN ('user','agent','system')),
  author_id TEXT NOT NULL,
  body TEXT NOT NULL,
  reply_to TEXT REFERENCES message(id),
  thread_root TEXT REFERENCES message(id),
  targets_json TEXT NOT NULL,
  ask_json TEXT,
  source_out_id TEXT UNIQUE,
  created_at TEXT NOT NULL
);
CREATE TABLE attachment (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES project(id),
  room_id TEXT REFERENCES room(id),
  message_id TEXT REFERENCES message(id),
  sha256 TEXT NOT NULL,
  mime TEXT NOT NULL,
  size INTEGER NOT NULL CHECK (size >= 0),
  path TEXT NOT NULL,
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE reaction (
  message_id TEXT NOT NULL REFERENCES message(id),
  member_kind TEXT NOT NULL CHECK (member_kind IN ('user','agent')),
  member_id TEXT NOT NULL,
  emoji TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (message_id, member_kind, member_id, emoji)
);
CREATE TABLE role (
  id TEXT PRIMARY KEY,
  key TEXT NOT NULL UNIQUE,
  label TEXT NOT NULL,
  body TEXT NOT NULL,
  cap_is_lead INTEGER NOT NULL DEFAULT 0 CHECK (cap_is_lead IN (0,1)),
  cap_can_spawn INTEGER NOT NULL DEFAULT 0 CHECK (cap_can_spawn IN (0,1)),
  cap_singleton_writer INTEGER NOT NULL DEFAULT 0 CHECK (cap_singleton_writer IN (0,1))
);
CREATE TABLE team (
  id TEXT PRIMARY KEY,
  project_id TEXT REFERENCES project(id),
  name TEXT NOT NULL,
  members_json TEXT NOT NULL
);
CREATE TABLE event (
  id TEXT PRIMARY KEY,
  seq INTEGER NOT NULL UNIQUE CHECK (seq > 0),
  kind TEXT NOT NULL,
  project_id TEXT REFERENCES project(id),
  room_id TEXT REFERENCES room(id),
  agent_id TEXT,
  turn_id TEXT,
  payload_json TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE settings_project (
  project_id TEXT PRIMARY KEY REFERENCES project(id),
  max_live_agents INTEGER NOT NULL DEFAULT 10 CHECK (max_live_agents > 0),
  spawn_rate_per_min INTEGER NOT NULL DEFAULT 6 CHECK (spawn_rate_per_min > 0),
  auto_resume_after_clean_diagnostic INTEGER NOT NULL DEFAULT 0
    CHECK (auto_resume_after_clean_diagnostic IN (0,1))
);
`}, {2, `
CREATE INDEX message_room_seq ON message(room_id, seq);
CREATE TABLE machine (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  key_hash TEXT NOT NULL,
  approved INTEGER NOT NULL CHECK (approved IN (0,1)),
  revoked_at TEXT,
  last_seen TEXT,
  supervisor_version TEXT
);
CREATE TABLE checkout (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES project(id),
  machine_id TEXT NOT NULL REFERENCES machine(id),
  canonical_path TEXT NOT NULL,
  writer_agent_id TEXT REFERENCES agent(id),
  writer_state TEXT NOT NULL DEFAULT 'none' CHECK (writer_state IN ('none','reserved','releasing')),
  UNIQUE (machine_id, canonical_path)
);
CREATE TABLE agent (
  id TEXT PRIMARY KEY,
  slug TEXT NOT NULL,
  project_id TEXT NOT NULL REFERENCES project(id),
  role_id TEXT NOT NULL REFERENCES role(id),
  recovery_block INTEGER NOT NULL DEFAULT 0 CHECK (recovery_block IN (0,1)),
  tool TEXT NOT NULL,
  variant TEXT NOT NULL,
  model TEXT NOT NULL,
  provider TEXT NOT NULL,
  machine_id TEXT NOT NULL REFERENCES machine(id),
  checkout_id TEXT NOT NULL REFERENCES checkout(id),
  key_hash TEXT NOT NULL,
  revoked_at TEXT,
  desired TEXT NOT NULL DEFAULT 'active' CHECK (desired IN ('active','not_accepting','stopped')),
  desired_rev INTEGER NOT NULL DEFAULT 0 CHECK (desired_rev >= 0),
  observed TEXT NOT NULL DEFAULT 'idle' CHECK (observed IN ('idle','in_turn','interrupting','stopped','lost')),
  generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (project_id, slug)
);
CREATE TABLE session (
  id TEXT PRIMARY KEY,
  agent_id TEXT NOT NULL REFERENCES agent(id),
  generation INTEGER NOT NULL CHECK (generation >= 0),
  native_session_id TEXT,
  start_mode TEXT NOT NULL CHECK (start_mode IN ('native_resume','fresh_with_history')),
  started_at TEXT NOT NULL,
  ended_at TEXT,
  UNIQUE (agent_id, generation)
);
CREATE TABLE note (
  id TEXT PRIMARY KEY,
  agent_id TEXT NOT NULL REFERENCES agent(id),
  message_id TEXT NOT NULL REFERENCES message(id),
  room_id TEXT NOT NULL REFERENCES room(id),
  state TEXT NOT NULL CHECK (state IN ('queued','dispatched','in_turn','done','aborted','dropped','unknown')),
  wake_reason TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (agent_id, message_id)
);
CREATE TABLE turn (
  id TEXT PRIMARY KEY,
  agent_id TEXT NOT NULL REFERENCES agent(id),
  session_id TEXT NOT NULL REFERENCES session(id),
  generation INTEGER NOT NULL CHECK (generation >= 0),
  room_id TEXT NOT NULL REFERENCES room(id),
  parent_turn_id TEXT REFERENCES turn(id),
  attempt INTEGER NOT NULL CHECK (attempt >= 1),
  mode TEXT NOT NULL CHECK (mode IN ('normal','correction','diagnostic')),
  exec TEXT NOT NULL CHECK (exec IN ('pending','running','exited','killed','lost')),
  outcome TEXT NOT NULL DEFAULT 'none' CHECK (outcome IN ('none','completed','partial_failed','failed','cancelled','unknown')),
  settle TEXT NOT NULL DEFAULT 'pending' CHECK (settle IN ('pending','settled')),
  cancel_intent INTEGER NOT NULL DEFAULT 0 CHECK (cancel_intent IN (0,1)),
  terminal_event INTEGER NOT NULL DEFAULT 0 CHECK (terminal_event IN (0,1)),
  report_json TEXT,
  policy_json TEXT,
  started_at TEXT,
  ended_at TEXT
);
CREATE UNIQUE INDEX turn_one_active_per_agent ON turn(agent_id)
  WHERE exec IN ('pending','running');
CREATE TABLE turn_note (
  turn_id TEXT NOT NULL REFERENCES turn(id),
  note_id TEXT NOT NULL REFERENCES note(id),
  PRIMARY KEY (turn_id, note_id)
);
CREATE TABLE outbox (
  id TEXT PRIMARY KEY,
  turn_id TEXT NOT NULL REFERENCES turn(id),
  answer_rev INTEGER NOT NULL CHECK (answer_rev >= 0),
  idx INTEGER NOT NULL CHECK (idx >= 0),
  agent_id TEXT NOT NULL REFERENCES agent(id),
  room_id TEXT NOT NULL REFERENCES room(id),
  body TEXT NOT NULL,
  reply_to TEXT REFERENCES message(id),
  thread_root TEXT REFERENCES message(id),
  ask_json TEXT,
  attachments_json TEXT,
  digest TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('proposed','validated','invalid','committed','held','superseded')),
  held_reason TEXT CHECK (held_reason IN ('invalid_after_correction','room_closed','not_member','stale_generation','correction_unavailable')),
  dedupe_key TEXT NOT NULL UNIQUE,
  UNIQUE (turn_id, answer_rev, idx)
);
CREATE TABLE receipt (
  id TEXT PRIMARY KEY,
  turn_id TEXT NOT NULL REFERENCES turn(id),
  answer_rev INTEGER NOT NULL CHECK (answer_rev >= 0),
  digest TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (turn_id, answer_rev)
);
CREATE TABLE request_key (
  actor_kind TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('spawn','control','inbox_answer','recovery_decision')),
  key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (actor_kind, actor_id, kind, key)
);
CREATE TABLE inbox_item (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES project(id),
  room_id TEXT NOT NULL REFERENCES room(id),
  message_id TEXT REFERENCES message(id),
  turn_id TEXT REFERENCES turn(id),
  agent_id TEXT REFERENCES agent(id),
  requester_kind TEXT NOT NULL,
  requester_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('question','approval','recovery','delivery_failed')),
  options_json TEXT,
  state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','resolved','withdrawn')),
  rev INTEGER NOT NULL DEFAULT 0 CHECK (rev >= 0),
  answer_message_id TEXT REFERENCES message(id),
  resolved_by TEXT,
  resolved_at TEXT,
  created_at TEXT NOT NULL
);
CREATE TABLE usage_record (
  id TEXT PRIMARY KEY,
  source_event_id TEXT NOT NULL UNIQUE,
  turn_id TEXT NOT NULL REFERENCES turn(id),
  agent_id TEXT NOT NULL REFERENCES agent(id),
  room_id TEXT NOT NULL REFERENCES room(id),
  project_id TEXT NOT NULL REFERENCES project(id),
  tokens_in INTEGER,
  tokens_out INTEGER,
  context_used INTEGER,
  context_window INTEGER,
  cost_est REAL,
  source TEXT NOT NULL CHECK (source IN ('measured','estimated','unavailable')),
  created_at TEXT NOT NULL
);
CREATE TABLE provider_account (
  id TEXT PRIMARY KEY,
  tool TEXT NOT NULL,
  machine_id TEXT NOT NULL REFERENCES machine(id),
  label TEXT NOT NULL,
  owner_user_id TEXT NOT NULL REFERENCES user(id)
);
` + legacyTimestampUpdates()}, {3, `
ALTER TABLE auth_session ADD COLUMN token_hash TEXT;
CREATE UNIQUE INDEX auth_session_token_hash ON auth_session(token_hash);
`}, {4, `
ALTER TABLE machine ADD COLUMN journal_id TEXT;
`}, {5, `
ALTER TABLE machine ADD COLUMN capabilities_json TEXT NOT NULL DEFAULT '[]';
`}, {6, `
CREATE TABLE dispatch_switch_gate (
  machine_id TEXT NOT NULL REFERENCES machine(id),
  tool TEXT NOT NULL,
  gate TEXT NOT NULL CHECK (gate IN ('open','paused')),
  PRIMARY KEY (machine_id, tool)
);
`}, {7, `
INSERT OR IGNORE INTO role(id,key,label,body,cap_is_lead,cap_can_spawn,cap_singleton_writer)
VALUES
 ('role_lead_default','lead','Lead','Coordinate the room, delegate bounded work, and report decisions and progress.',1,1,0),
 ('role_worker_default','worker','Worker','Complete assigned work and report concrete results to the room.',0,0,1),
 ('role_reviewer_default','reviewer','Reviewer','Review assigned work and report findings with evidence.',0,0,0);
`}, {8, `
ALTER TABLE session ADD COLUMN native_session_started_at TEXT;
`}, {9, `
CREATE TABLE agent_switch (
  agent_id TEXT PRIMARY KEY REFERENCES agent(id),
  room_id TEXT NOT NULL REFERENCES room(id),
  tool TEXT NOT NULL,
  variant TEXT NOT NULL,
  model TEXT NOT NULL,
  provider TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('draining','starting','failed')),
  error TEXT NOT NULL DEFAULT '',
  requested_at TEXT NOT NULL
);
ALTER TABLE request_key RENAME TO request_key_old;
CREATE TABLE request_key (
  actor_kind TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('spawn','control','switch','inbox_answer','recovery_decision')),
  key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (actor_kind, actor_id, kind, key)
);
INSERT INTO request_key SELECT * FROM request_key_old;
DROP TABLE request_key_old;
`}, {10, `
ALTER TABLE role ADD COLUMN project_id TEXT REFERENCES project(id);
ALTER TABLE role ADD COLUMN role_key TEXT NOT NULL DEFAULT '';
ALTER TABLE role ADD COLUMN emoji TEXT NOT NULL DEFAULT '';
ALTER TABLE role ADD COLUMN cardinality TEXT NOT NULL DEFAULT 'multi'
  CHECK (cardinality IN ('multi','singleton'));
ALTER TABLE role ADD COLUMN rev INTEGER NOT NULL DEFAULT 0;
UPDATE role SET role_key=key;
UPDATE role SET emoji=CASE role_key WHEN 'lead' THEN '🧭'
  WHEN 'worker' THEN '🛠️' WHEN 'reviewer' THEN '🔎' ELSE '' END;
UPDATE role SET cardinality='singleton' WHERE role_key='lead';
CREATE UNIQUE INDEX role_global_key ON role(role_key) WHERE project_id IS NULL;
CREATE UNIQUE INDEX role_project_key ON role(project_id,role_key)
  WHERE project_id IS NOT NULL;
INSERT INTO role(id,key,label,body,cap_is_lead,cap_can_spawn,
  cap_singleton_writer,project_id,role_key,emoji,cardinality)
SELECT 'role_lead_' || p.id, 'lead@' || p.id, 'Lead',
  p.lead_instructions, 1, 1, 0, p.id, 'lead', '🧭', 'singleton'
FROM project p WHERE trim(p.lead_instructions)<>'';
ALTER TABLE team ADD COLUMN rev INTEGER NOT NULL DEFAULT 0;
ALTER TABLE team ADD COLUMN created_at TEXT NOT NULL DEFAULT '';
ALTER TABLE team ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX team_global_name ON team(name) WHERE project_id IS NULL;
CREATE UNIQUE INDEX team_project_name ON team(project_id,name)
  WHERE project_id IS NOT NULL;
ALTER TABLE request_key RENAME TO request_key_old;
CREATE TABLE request_key (
  actor_kind TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN
    ('spawn','team_spawn','control','switch','inbox_answer','recovery_decision')),
  key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (actor_kind,actor_id,kind,key)
);
INSERT INTO request_key SELECT * FROM request_key_old;
DROP TABLE request_key_old;
`}, {11, `
ALTER TABLE project ADD COLUMN control_desired TEXT NOT NULL DEFAULT 'active'
  CHECK (control_desired IN ('active','paused','stopped'));
ALTER TABLE room ADD COLUMN control_desired TEXT NOT NULL DEFAULT 'active'
  CHECK (control_desired IN ('active','paused','stopped'));
ALTER TABLE room ADD COLUMN gate_rev INTEGER NOT NULL DEFAULT 0 CHECK (gate_rev >= 0);
CREATE TABLE scope_control_agent (
  scope_kind TEXT NOT NULL CHECK (scope_kind IN ('room','project')),
  scope_id TEXT NOT NULL,
  agent_id TEXT NOT NULL REFERENCES agent(id),
  restore_desired TEXT NOT NULL CHECK (restore_desired IN ('active','not_accepting')),
  bulk_desired TEXT NOT NULL CHECK (bulk_desired IN ('not_accepting','stopped')),
  bulk_rev INTEGER NOT NULL,
  PRIMARY KEY (scope_kind,scope_id,agent_id)
);
ALTER TABLE request_key RENAME TO request_key_old;
CREATE TABLE request_key (
  actor_kind TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN
    ('spawn','team_spawn','control','scope_control','switch','inbox_answer','recovery_decision')),
  key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (actor_kind,actor_id,kind,key)
);
INSERT INTO request_key SELECT * FROM request_key_old;
DROP TABLE request_key_old;
`}, {12, `
ALTER TABLE role ADD COLUMN deleted_at TEXT;
DROP INDEX role_global_key;
DROP INDEX role_project_key;
CREATE UNIQUE INDEX role_global_key ON role(role_key)
  WHERE project_id IS NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX role_project_key ON role(project_id,role_key)
  WHERE project_id IS NOT NULL AND deleted_at IS NULL;
`}, {13, `
CREATE TABLE usage_limit (
  scope_kind TEXT NOT NULL CHECK (scope_kind IN ('room','project')),
  scope_id TEXT NOT NULL,
  daily_cost_cap REAL CHECK (daily_cost_cap IS NULL OR daily_cost_cap > 0),
  daily_turn_cap INTEGER CHECK (daily_turn_cap IS NULL OR daily_turn_cap > 0),
  rev INTEGER NOT NULL DEFAULT 0 CHECK (rev >= 0),
  PRIMARY KEY (scope_kind,scope_id)
);
CREATE TABLE usage_limit_day (
  scope_kind TEXT NOT NULL CHECK (scope_kind IN ('room','project')),
  scope_id TEXT NOT NULL,
  day_start TEXT NOT NULL,
  tz TEXT NOT NULL,
  warned INTEGER NOT NULL DEFAULT 0 CHECK (warned IN (0,1)),
  limited INTEGER NOT NULL DEFAULT 0 CHECK (limited IN (0,1)),
  paused INTEGER NOT NULL DEFAULT 0 CHECK (paused IN (0,1)),
  continued INTEGER NOT NULL DEFAULT 0 CHECK (continued IN (0,1)),
  abandoned INTEGER NOT NULL DEFAULT 0 CHECK (abandoned IN (0,1)),
  rolled_over INTEGER NOT NULL DEFAULT 0 CHECK (rolled_over IN (0,1)),
  override_cost REAL,
  override_turns INTEGER,
  PRIMARY KEY (scope_kind,scope_id,day_start)
);
ALTER TABLE inbox_item RENAME TO inbox_item_old;
CREATE TABLE inbox_item (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES project(id),
  room_id TEXT NOT NULL REFERENCES room(id),
  message_id TEXT REFERENCES message(id),
  turn_id TEXT REFERENCES turn(id),
  agent_id TEXT REFERENCES agent(id),
  requester_kind TEXT NOT NULL,
  requester_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('question','approval','recovery','delivery_failed','usage_info','usage_limit')),
  options_json TEXT,
  state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','resolved','withdrawn')),
  rev INTEGER NOT NULL DEFAULT 0 CHECK (rev >= 0),
  answer_message_id TEXT REFERENCES message(id),
  resolved_by TEXT,
  resolved_at TEXT,
  created_at TEXT NOT NULL
);
INSERT INTO inbox_item SELECT * FROM inbox_item_old;
DROP TABLE inbox_item_old;
`}, {14, `
ALTER TABLE agent ADD COLUMN home_room_id TEXT REFERENCES room(id);
CREATE INDEX agent_home_room ON agent(home_room_id);
UPDATE agent SET home_room_id=(SELECT e.room_id FROM event e
  WHERE e.agent_id=agent.id AND e.kind='agent_spawn_requested' AND e.room_id IS NOT NULL
  ORDER BY e.seq LIMIT 1);
UPDATE agent SET home_room_id=(SELECT m.room_id FROM room_member m JOIN room r ON r.id=m.room_id
  WHERE m.member_kind='agent' AND m.member_id=agent.id AND r.kind!='dm'
  ORDER BY m.joined_at,m.rowid LIMIT 1) WHERE home_room_id IS NULL;
ALTER TABLE scope_control_agent ADD COLUMN hold_desired TEXT NOT NULL DEFAULT 'not_accepting'
  CHECK (hold_desired IN ('not_accepting','stopped'));
UPDATE scope_control_agent SET hold_desired=bulk_desired;
CREATE TABLE scope_control_failure (
  scope_kind TEXT NOT NULL CHECK (scope_kind IN ('room','project')),
  scope_id TEXT NOT NULL,
  agent_id TEXT NOT NULL REFERENCES agent(id),
  action TEXT NOT NULL CHECK (action IN ('pause','stop')),
  error TEXT NOT NULL,
  attempted_at TEXT NOT NULL,
  PRIMARY KEY (scope_kind,scope_id,agent_id)
);
`}, {15, `
ALTER TABLE machine ADD COLUMN is_host INTEGER NOT NULL DEFAULT 0 CHECK (is_host IN (0,1));
UPDATE machine SET is_host=1 WHERE id=(SELECT id FROM machine ORDER BY rowid LIMIT 1);
CREATE UNIQUE INDEX machine_one_host ON machine(is_host) WHERE is_host=1;
CREATE TABLE machine_pairing (
  token_hash TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  created_by TEXT NOT NULL REFERENCES user(id),
  expires_at TEXT NOT NULL,
  redeemed_at TEXT,
  machine_id TEXT REFERENCES machine(id)
);
CREATE TABLE project_machine_folder (
  project_id TEXT NOT NULL REFERENCES project(id),
  machine_id TEXT NOT NULL REFERENCES machine(id),
  folder TEXT NOT NULL,
  PRIMARY KEY (project_id,machine_id)
);
ALTER TABLE agent_switch ADD COLUMN machine_id TEXT REFERENCES machine(id);
ALTER TABLE agent_switch ADD COLUMN checkout_path TEXT;
`}, {16, `
ALTER TABLE settings_project ADD COLUMN memory_enabled INTEGER NOT NULL DEFAULT 0
  CHECK (memory_enabled IN (0,1));
CREATE TABLE project_memory (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES project(id),
  body TEXT NOT NULL,
  rev INTEGER NOT NULL DEFAULT 1 CHECK (rev > 0),
  author_kind TEXT NOT NULL CHECK (author_kind IN ('user','agent')),
  author_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX project_memory_scope ON project_memory(project_id,created_at,id);
CREATE TABLE task_note (
  id TEXT PRIMARY KEY,
  room_id TEXT NOT NULL REFERENCES room(id),
  body TEXT NOT NULL,
  rev INTEGER NOT NULL DEFAULT 1 CHECK (rev > 0),
  author_kind TEXT NOT NULL CHECK (author_kind IN ('user','agent')),
  author_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX task_note_scope ON task_note(room_id,created_at,id);
`}, {17, `
ALTER TABLE machine ADD COLUMN harnesses_json TEXT NOT NULL DEFAULT '[]';
CREATE TABLE machine_harness_setting (
  machine_id TEXT NOT NULL REFERENCES machine(id),
  tool TEXT NOT NULL,
  variant TEXT NOT NULL,
  enabled INTEGER NOT NULL CHECK (enabled IN (0,1)),
  override_path TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (machine_id,tool,variant)
);
CREATE TABLE machine_harness_activity (
  id TEXT PRIMARY KEY,
  machine_id TEXT NOT NULL REFERENCES machine(id),
  tool TEXT NOT NULL,
  variant TEXT NOT NULL,
  actor TEXT NOT NULL,
  old_enabled INTEGER NOT NULL,
  new_enabled INTEGER NOT NULL,
  old_path TEXT NOT NULL,
  new_path TEXT NOT NULL,
  created_at TEXT NOT NULL
);
`}, {18, `
ALTER TABLE request_key RENAME TO request_key_old;
CREATE TABLE request_key (
  actor_kind TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN
    ('spawn','team_spawn','control','scope_control','switch','inbox_answer','recovery_decision','room_create','room_close')),
  key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (actor_kind,actor_id,kind,key)
);
INSERT INTO request_key SELECT * FROM request_key_old;
DROP TABLE request_key_old;
UPDATE role SET body='You are the project lead. Coordinate work through room messages and the agent toolbox; do not implement the requested task yourself. When a human brings a new request to the project home room: (1) create a task room with create_room; (2) call list_teams and ask the human in that task room which saved team to use, using a structured question with options equal to the available team names so the question appears in the inbox. If no teams are saved, ask the human to save one before spawning. (3) Once answered, call spawn_agent with the chosen team and the task room ID, then post a clear assignment in the task room. Follow worker updates and handoffs there, resolve blockers with the human, and keep the work moving. When the team finishes, post a concise outcome and next steps in the project home room, then call close_room on the task room using its current lifecycle_rev. Do not close the room while work or a human question is still open.'
WHERE id='role_lead_default' AND body='Coordinate the room, delegate bounded work, and report decisions and progress.';
`}, {19, `
UPDATE role SET body=body || ' After create_room succeeds, keep working in the same turn: call list_teams and return a structured question message in the new room. The returned room ID is valid even though context.rooms was captured before creation. Set ask to {"kind":"question","text":"Which saved team should take this task?","options":["each available team name"],"free_text":false}; replace the example option with the names from list_teams. A plain text question does not create an inbox item. If a tool fails, tell the human in a room message what is blocked. Never end with an empty messages array after creating a task room.'
WHERE id='role_lead_default' AND body='You are the project lead. Coordinate work through room messages and the agent toolbox; do not implement the requested task yourself. When a human brings a new request to the project home room: (1) create a task room with create_room; (2) call list_teams and ask the human in that task room which saved team to use, using a structured question with options equal to the available team names so the question appears in the inbox. If no teams are saved, ask the human to save one before spawning. (3) Once answered, call spawn_agent with the chosen team and the task room ID, then post a clear assignment in the task room. Follow worker updates and handoffs there, resolve blockers with the human, and keep the work moving. When the team finishes, post a concise outcome and next steps in the project home room, then call close_room on the task room using its current lifecycle_rev. Do not close the room while work or a human question is still open.';
`}, {20, `
CREATE TABLE provider_limit (
  agent_id TEXT PRIMARY KEY REFERENCES agent(id),
  turn_id TEXT NOT NULL REFERENCES turn(id),
  detected_at TEXT NOT NULL,
  reset_at TEXT,
  tool TEXT NOT NULL CHECK (tool IN ('claude','codex'))
);
CREATE TABLE provider_limit_retry (
  note_id TEXT PRIMARY KEY REFERENCES note(id),
  source_turn_id TEXT NOT NULL REFERENCES turn(id),
  caution INTEGER NOT NULL CHECK (caution IN (0,1))
);
`}, {21, `
CREATE TABLE snippet (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES user(id),
  name TEXT NOT NULL,
  body TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX snippet_user_name ON snippet(user_id, name);
`}, {22, `
ALTER TABLE attachment ADD COLUMN filename TEXT NOT NULL DEFAULT '';
`}, {23, `
CREATE TABLE schedule (
  id TEXT PRIMARY KEY,
  room_id TEXT NOT NULL REFERENCES room(id),
  project_id TEXT NOT NULL REFERENCES project(id),
  creator_kind TEXT NOT NULL CHECK (creator_kind IN ('user','agent')),
  creator_id TEXT NOT NULL,
  target_agent_id TEXT REFERENCES agent(id),
  target_lead INTEGER NOT NULL CHECK (target_lead IN (0,1)),
  body TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('once','interval','daily','weekly')),
  timezone TEXT NOT NULL,
  interval_seconds INTEGER NOT NULL DEFAULT 0,
  local_time TEXT NOT NULL DEFAULT '',
  weekday INTEGER NOT NULL DEFAULT -1,
  enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  next_run_at TEXT NOT NULL,
  last_outcome TEXT NOT NULL DEFAULT '',
  last_reason TEXT NOT NULL DEFAULT '',
  last_run_at TEXT,
  rev INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK ((target_agent_id IS NULL) = target_lead)
);
CREATE INDEX schedule_due ON schedule(enabled,next_run_at);
CREATE TABLE schedule_run (
  schedule_id TEXT NOT NULL REFERENCES schedule(id) ON DELETE CASCADE,
  due_at TEXT NOT NULL,
  processed_at TEXT NOT NULL,
  outcome TEXT NOT NULL CHECK (outcome IN ('delivered','skipped')),
  reason TEXT NOT NULL DEFAULT '',
  message_id TEXT REFERENCES message(id),
  PRIMARY KEY (schedule_id,due_at)
);
ALTER TABLE request_key RENAME TO request_key_old;
CREATE TABLE request_key (
  actor_kind TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN
    ('spawn','team_spawn','control','scope_control','switch','inbox_answer','recovery_decision','room_create','room_close','schedule_create')),
  key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (actor_kind,actor_id,kind,key)
);
INSERT INTO request_key SELECT * FROM request_key_old;
DROP TABLE request_key_old;
`}, {24, `
CREATE TABLE project_machine_allowed (
  project_id TEXT NOT NULL REFERENCES project(id),
  machine_id TEXT NOT NULL REFERENCES machine(id),
  approved_by TEXT NOT NULL REFERENCES user(id),
  approved_at TEXT NOT NULL,
  PRIMARY KEY (project_id,machine_id)
);
INSERT INTO project_machine_allowed(project_id,machine_id,approved_by,approved_at)
SELECT p.id,m.id,(SELECT user_id FROM membership WHERE project_id=p.id AND role='owner' ORDER BY user_id LIMIT 1),datetime('now')
FROM project p JOIN machine m ON m.is_host=1 AND m.approved=1 AND m.revoked_at IS NULL
WHERE EXISTS(SELECT 1 FROM membership WHERE project_id=p.id AND role='owner');
INSERT OR IGNORE INTO project_machine_allowed(project_id,machine_id,approved_by,approved_at)
SELECT DISTINCT a.project_id,a.machine_id,
  (SELECT user_id FROM membership WHERE project_id=a.project_id AND role='owner' ORDER BY user_id LIMIT 1),datetime('now')
FROM agent a JOIN checkout c ON c.id=a.checkout_id
JOIN project_machine_folder f ON f.project_id=a.project_id AND f.machine_id=a.machine_id
JOIN machine m ON m.id=a.machine_id AND m.approved=1 AND m.revoked_at IS NULL
WHERE EXISTS(SELECT 1 FROM membership WHERE project_id=a.project_id AND role='owner')
  AND (c.canonical_path=f.folder OR substr(c.canonical_path,1,length(f.folder)+1)=f.folder||'/');
CREATE TABLE agent_boundary_grant (
  agent_id TEXT NOT NULL REFERENCES agent(id),
  machine_id TEXT NOT NULL REFERENCES machine(id),
  canonical_path TEXT NOT NULL,
  approved_by TEXT NOT NULL REFERENCES user(id),
  approved_at TEXT NOT NULL,
  PRIMARY KEY (agent_id,machine_id,canonical_path)
);
CREATE TABLE boundary_pending (
  inbox_id TEXT PRIMARY KEY REFERENCES inbox_item(id),
  agent_id TEXT NOT NULL REFERENCES agent(id),
  machine_id TEXT NOT NULL REFERENCES machine(id),
  canonical_path TEXT NOT NULL,
  operation TEXT NOT NULL CHECK (operation IN ('spawn','switch','reactivate')),
  mapped_root TEXT NOT NULL DEFAULT '',
  requested_by_kind TEXT NOT NULL CHECK (requested_by_kind IN ('user','agent')),
  requested_by_id TEXT NOT NULL,
  prior_desired TEXT NOT NULL DEFAULT 'active' CHECK (prior_desired IN ('active','not_accepting')),
  state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','resolved','withdrawn')),
  created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX boundary_pending_agent ON boundary_pending(agent_id) WHERE state='open';
`}, {25, `
ALTER TABLE message ADD COLUMN legacy_payload_json TEXT;
ALTER TABLE message ADD COLUMN legacy_author_label TEXT;
ALTER TABLE attachment ADD COLUMN legacy_metadata_json TEXT;
ALTER TABLE project_memory ADD COLUMN source_message_id TEXT REFERENCES message(id);
ALTER TABLE project_memory ADD COLUMN legacy_source_message_id INTEGER;
ALTER TABLE project_memory ADD COLUMN legacy_payload_json TEXT;
CREATE TABLE global_memory (
  id TEXT PRIMARY KEY,
  scope TEXT NOT NULL CHECK (scope IN ('user_profile','agent_shared_notes')),
  scope_key TEXT NOT NULL,
  body TEXT NOT NULL,
  rev INTEGER NOT NULL CHECK (rev > 0),
  author_kind TEXT NOT NULL CHECK (author_kind IN ('user','agent')),
  author_id TEXT NOT NULL,
  source_message_id TEXT REFERENCES message(id),
  legacy_source_message_id INTEGER,
  legacy_payload_json TEXT,
  active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX global_memory_scope ON global_memory(scope,scope_key,created_at,id);
CREATE TABLE legacy_import_source (
  source_key TEXT PRIMARY KEY,
  map_digest TEXT NOT NULL,
  message_high_water INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE legacy_import_map (
  source_key TEXT NOT NULL REFERENCES legacy_import_source(source_key),
  entity_kind TEXT NOT NULL,
  old_id TEXT NOT NULL,
  new_id TEXT NOT NULL,
  PRIMARY KEY (source_key,entity_kind,old_id),
  UNIQUE (entity_kind,new_id)
);
CREATE TABLE legacy_room_member (
  room_id TEXT NOT NULL REFERENCES room(id),
  legacy_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('user','agent')),
  label TEXT NOT NULL,
  PRIMARY KEY (room_id,legacy_id)
);
CREATE TABLE legacy_room (
  room_id TEXT PRIMARY KEY REFERENCES room(id),
  data_json TEXT NOT NULL
);
`}, {26, `
CREATE TABLE room_worktree (
  room_id TEXT PRIMARY KEY REFERENCES room(id),
  base_ref TEXT NOT NULL,
  base_commit TEXT NOT NULL DEFAULT '',
  branch_name TEXT NOT NULL,
  relative_path TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('pending','ready','failed','preserved','removed')),
  cleanup_reason TEXT NOT NULL DEFAULT '',
  dirty_base_count INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE room_worktree_instance (
  room_id TEXT NOT NULL REFERENCES room_worktree(room_id),
  machine_id TEXT NOT NULL REFERENCES machine(id),
  canonical_path TEXT NOT NULL,
  base_commit TEXT NOT NULL DEFAULT '',
  dirty_base_count INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL CHECK (state IN ('ready','failed','preserved','removed')),
  reason TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (room_id,machine_id)
);
`}, {27, `
ALTER TABLE message ADD COLUMN visual_plan_json TEXT;
ALTER TABLE message ADD COLUMN appendix_pages_json TEXT;
ALTER TABLE outbox ADD COLUMN visual_plan_json TEXT;
ALTER TABLE outbox ADD COLUMN appendix_pages_json TEXT;
UPDATE role SET body=body || ' Use visual_plan blocks for clear plans, diffs, diagrams, and review summaries; place long Markdown detail in appendix_pages so it appears under a collapsed Full plan.'
WHERE id='role_lead_default' AND body='Coordinate the room, delegate bounded work, and report decisions and progress.';
`}, {28, `
CREATE TABLE machine_launcher (
  id TEXT PRIMARY KEY,
  machine_id TEXT NOT NULL REFERENCES machine(id),
  tool TEXT NOT NULL,
  name TEXT NOT NULL,
  command TEXT NOT NULL,
  args_json TEXT NOT NULL DEFAULT '[]',
  env_refs_json TEXT NOT NULL DEFAULT '{}',
  enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0,1)),
  containerized INTEGER NOT NULL DEFAULT 0 CHECK (containerized IN (0,1)),
  agent_server_url TEXT NOT NULL DEFAULT '',
  path_maps_json TEXT NOT NULL DEFAULT '[]',
  visible_roots_json TEXT NOT NULL DEFAULT '[]',
  supports_toolbox INTEGER NOT NULL DEFAULT 1 CHECK (supports_toolbox IN (0,1)),
  preset INTEGER NOT NULL DEFAULT 0 CHECK (preset IN (0,1)),
  UNIQUE(machine_id,tool,name)
);
CREATE TABLE machine_launcher_activity (
  id TEXT PRIMARY KEY,
  machine_id TEXT NOT NULL REFERENCES machine(id),
  launcher_id TEXT,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  before_json TEXT,
  after_json TEXT,
  created_at TEXT NOT NULL
);
ALTER TABLE agent ADD COLUMN launcher_id TEXT REFERENCES machine_launcher(id);
ALTER TABLE agent_switch ADD COLUMN launcher_id TEXT REFERENCES machine_launcher(id);
CREATE INDEX agent_launcher_active ON agent(launcher_id,desired,observed);
INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled,containerized,supports_toolbox,preset)
SELECT 'mln_' || lower(hex(randomblob(16))), m.id, t.tool, t.name,
       COALESCE(NULLIF(h.override_path,''),t.command),
       CASE WHEN t.name='native' THEN 0 ELSE COALESCE(h.enabled,1) END,
       t.containerized, t.supports_toolbox, 1
FROM machine m CROSS JOIN (
  SELECT 'claude' tool,'native' name,'claude' command,0 containerized,1 supports_toolbox
  UNION ALL SELECT 'claude','harness-docker','claude-docker',1,1
  UNION ALL SELECT 'codex','native','codex',0,1
  UNION ALL SELECT 'codex','harness-docker','codex-docker',1,1
  UNION ALL SELECT 'pi','native','pi',0,1
  UNION ALL SELECT 'pi','harness-docker','pi-docker',1,0
) t LEFT JOIN machine_harness_setting h ON h.machine_id=m.id AND h.tool=t.tool
  AND h.variant=CASE WHEN t.name='native' THEN 'native' ELSE 'docker' END;
UPDATE agent SET launcher_id=(SELECT l.id FROM machine_launcher l WHERE l.machine_id=agent.machine_id
  AND l.tool=agent.tool AND l.name=CASE WHEN agent.variant='docker' THEN 'harness-docker' ELSE agent.variant END)
WHERE agent.tool IN ('claude','codex','pi');
UPDATE agent_switch SET launcher_id=(SELECT l.id FROM machine_launcher l
  WHERE l.machine_id=COALESCE(agent_switch.machine_id,(SELECT machine_id FROM agent WHERE id=agent_switch.agent_id))
  AND l.tool=agent_switch.tool
  AND l.name=CASE WHEN agent_switch.variant='docker' THEN 'harness-docker' ELSE agent_switch.variant END)
WHERE agent_switch.tool IN ('claude','codex','pi');
UPDATE team SET members_json=COALESCE((SELECT json_group_array(json_set(member.value,'$.launcher_name',
  CASE json_extract(member.value,'$.variant') WHEN 'docker' THEN 'harness-docker'
  ELSE json_extract(member.value,'$.variant') END)) FROM json_each(team.members_json) member), '[]')
WHERE json_valid(members_json);
`}, {29, `
ALTER TABLE room ADD COLUMN memory_enabled INTEGER CHECK (memory_enabled IS NULL OR memory_enabled=0);
`}, {30, `
UPDATE role SET body=body || ' Use visual_plan blocks to make plans, diffs, diagrams, and review summaries easier to scan. Put long Markdown detail in appendix_pages. Always put the substantive decisions, evidence, and next actions in the message body too; agents receive visual blocks as bounded text.'
WHERE id='role_lead_default'
  AND body LIKE 'You are the project lead. Coordinate work through room messages and the agent toolbox; do not implement the requested task yourself.%Never end with an empty messages array after creating a task room.'
  AND instr(body,'Always put the substantive decisions')=0;
UPDATE role SET body=body || ' Put the substantive decisions, evidence, and next actions in the message body. visual_plan and appendix_pages only add display structure; agents receive them as bounded text.'
WHERE (id='role_worker_default' AND body='Complete assigned work and report concrete results to the room.')
   OR (id='role_reviewer_default' AND body='Review assigned work and report findings with evidence.');
`}, {31, `
CREATE TABLE legacy_import_choice (
  source_key TEXT NOT NULL REFERENCES legacy_import_source(source_key),
  entity_kind TEXT NOT NULL,
  old_id TEXT NOT NULL,
  choice_json TEXT NOT NULL,
  PRIMARY KEY (source_key,entity_kind,old_id)
);
CREATE TABLE legacy_import_config (
  source_key TEXT PRIMARY KEY REFERENCES legacy_import_source(source_key),
  map_json TEXT NOT NULL
);
`}, {32, roleDefaultsMigration()}, {33, `
ALTER TABLE message ADD COLUMN needs_attention INTEGER NOT NULL DEFAULT 0 CHECK (needs_attention IN (0,1));
ALTER TABLE message ADD COLUMN proposed_answers_json TEXT;
ALTER TABLE message ADD COLUMN open_questions_json TEXT;
ALTER TABLE outbox ADD COLUMN needs_attention INTEGER NOT NULL DEFAULT 0 CHECK (needs_attention IN (0,1));
ALTER TABLE outbox ADD COLUMN proposed_answers_json TEXT;
ALTER TABLE outbox ADD COLUMN open_questions_json TEXT;
`}, {34, `
ALTER TABLE user ADD COLUMN disabled_at TEXT;
CREATE TABLE user_invite (
  id TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL CHECK (kind IN ('join','reset')),
  project_id TEXT REFERENCES project(id),
  role TEXT CHECK (role IN ('owner','operator','viewer')),
  target_user_id TEXT REFERENCES user(id),
  created_by TEXT NOT NULL REFERENCES user(id),
  expires_at TEXT NOT NULL,
  used_at TEXT,
  created_at TEXT NOT NULL,
  CHECK ((kind='join' AND project_id IS NOT NULL AND role IS NOT NULL AND target_user_id IS NULL)
    OR (kind='reset' AND project_id IS NULL AND role IS NULL AND target_user_id IS NOT NULL))
);
CREATE INDEX user_invite_expiry ON user_invite(expires_at) WHERE used_at IS NULL;
`}, {35, roleFollowupMigration()}, {36, `
CREATE TABLE room_view_preference (
  user_id TEXT NOT NULL REFERENCES user(id),
  room_id TEXT NOT NULL REFERENCES room(id),
  hidden INTEGER NOT NULL DEFAULT 0 CHECK (hidden IN (0,1)),
  pinned INTEGER NOT NULL DEFAULT 0 CHECK (pinned IN (0,1)),
  PRIMARY KEY (user_id, room_id)
);
`}, {37, `
CREATE TABLE macro (
  key TEXT PRIMARY KEY,
  body TEXT NOT NULL
);
`}, {38, `
ALTER TABLE project_memory ADD COLUMN active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1));
UPDATE project_memory SET active=0 WHERE legacy_payload_json IS NOT NULL;
`}, {39, `
CREATE INDEX message_room_thread_seq ON message(room_id, thread_root, seq);
`}, {40, `
CREATE TABLE macro_default_seen (
  key TEXT PRIMARY KEY
);
`}, {41, `
CREATE TABLE spawn_model (
  tool TEXT NOT NULL CHECK (tool IN ('claude','codex','pi')),
  model_id TEXT NOT NULL,
  label TEXT NOT NULL,
  position INTEGER NOT NULL,
  PRIMARY KEY (tool, model_id)
);
INSERT INTO spawn_model(tool,model_id,label,position) VALUES
  ('claude','haiku','Haiku',0),
  ('claude','sonnet','Sonnet',1),
  ('claude','opus','Opus',2),
  ('codex','gpt-6-sol','GPT-6 Sol',0),
  ('codex','gpt-6-luna','GPT-6 Luna',1),
  ('codex','gpt-6-astra','GPT-6 Astra',2),
  ('pi','ollama-cloud/gemma4:31b','Gemma 4 31B',0);
`}, {42, `
SELECT 1;
`}, {43, `
CREATE TABLE room_read_cursor (
  user_id TEXT NOT NULL REFERENCES user(id),
  room_id TEXT NOT NULL REFERENCES room(id),
  last_read_seq INTEGER NOT NULL CHECK (last_read_seq >= 0),
  PRIMARY KEY (user_id, room_id)
);
INSERT INTO room_read_cursor(user_id, room_id, last_read_seq)
SELECT mb.user_id, r.id, COALESCE((SELECT MAX(m.seq) FROM message m WHERE m.room_id=r.id), 0)
FROM membership mb JOIN room r ON r.project_id=mb.project_id
WHERE r.kind!='dm'
UNION
SELECT rm.member_id, r.id, COALESCE((SELECT MAX(m.seq) FROM message m WHERE m.room_id=r.id), 0)
FROM room_member rm JOIN room r ON r.id=rm.room_id
WHERE r.kind='dm' AND rm.member_kind='user' AND rm.left_at IS NULL;
`}, {44, `
CREATE TABLE sound (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  ext TEXT NOT NULL CHECK (ext IN ('mp3','wav')),
  size INTEGER NOT NULL CHECK (size > 0),
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL
);
`}, {45, `
CREATE INDEX event_room_seq ON event(room_id, seq) WHERE room_id IS NOT NULL;
`}, {46, `
CREATE TABLE provider_limit_new (
  agent_id TEXT PRIMARY KEY REFERENCES agent(id),
  turn_id TEXT NOT NULL REFERENCES turn(id),
  detected_at TEXT NOT NULL,
  reset_at TEXT,
  tool TEXT NOT NULL CHECK (tool IN ('claude','codex','pi','opencode'))
);
INSERT INTO provider_limit_new(agent_id,turn_id,detected_at,reset_at,tool)
SELECT agent_id,turn_id,detected_at,reset_at,tool FROM provider_limit;
DROP TABLE provider_limit;
ALTER TABLE provider_limit_new RENAME TO provider_limit;
`}, {47, `
CREATE TABLE spawn_model_new (
  tool TEXT NOT NULL CHECK (tool IN ('claude','codex','pi','opencode')),
  model_id TEXT NOT NULL,
  label TEXT NOT NULL,
  position INTEGER NOT NULL,
  PRIMARY KEY (tool, model_id)
);
INSERT INTO spawn_model_new(tool,model_id,label,position) SELECT tool,model_id,label,position FROM spawn_model;
INSERT INTO spawn_model_new(tool,model_id,label,position) VALUES ('opencode','ollama-cloud/gemma4:31b','Gemma 4 31B',0);
DROP TABLE spawn_model;
ALTER TABLE spawn_model_new RENAME TO spawn_model;
INSERT INTO machine_launcher(id,machine_id,tool,name,command,enabled,containerized,supports_toolbox,preset)
SELECT 'mln_' || lower(hex(randomblob(16))), m.id, 'opencode', 'native', 'opencode', 0, 0, 1, 1
FROM machine m
WHERE NOT EXISTS (SELECT 1 FROM machine_launcher l WHERE l.machine_id=m.id AND l.tool='opencode' AND l.name='native');
`}, {48, `
CREATE TABLE quota_switcher_config (
  tool TEXT PRIMARY KEY CHECK (tool IN ('claude','codex')),
  config_json TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE quota_switcher_state (
  tool TEXT PRIMARY KEY CHECK (tool IN ('claude','codex')),
  state_json TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE quota_switcher_decision (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tool TEXT NOT NULL CHECK (tool IN ('claude','codex')),
  at TEXT NOT NULL,
  decision_json TEXT NOT NULL
);
CREATE INDEX quota_switcher_decision_tool ON quota_switcher_decision(tool, id);
`}, {49, `
CREATE TABLE global_setting (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
`}, {50, `
ALTER TABLE message ADD COLUMN author_harness TEXT NOT NULL DEFAULT '';
ALTER TABLE message ADD COLUMN author_model TEXT NOT NULL DEFAULT '';
-- Messages imported by an earlier build kept the raw v1 message in legacy_payload_json. Move what v2 has
-- native columns for into them before the column goes, so nothing the old import carried is lost.
UPDATE message SET
  author_harness=COALESCE(json_extract(legacy_payload_json,'$.from_harness'),''),
  author_model=COALESCE(json_extract(legacy_payload_json,'$.from_model'),''),
  needs_attention=CASE WHEN json_extract(legacy_payload_json,'$.needs_human_attention') IN (1,'true') THEN 1 ELSE 0 END,
  proposed_answers_json=CASE WHEN json_type(legacy_payload_json,'$.proposed_answers')='array' AND json_array_length(legacy_payload_json,'$.proposed_answers')>0
    THEN json_extract(legacy_payload_json,'$.proposed_answers') ELSE proposed_answers_json END,
  open_questions_json=CASE WHEN json_type(legacy_payload_json,'$.open_questions') IN ('array','object')
    THEN json_extract(legacy_payload_json,'$.open_questions') ELSE open_questions_json END,
  visual_plan_json=CASE WHEN json_type(legacy_payload_json,'$.visual_plan') IN ('array','object')
    THEN json_extract(legacy_payload_json,'$.visual_plan') ELSE visual_plan_json END,
  appendix_pages_json=CASE WHEN json_type(legacy_payload_json,'$.appendix_pages') IN ('array','object')
    THEN json_extract(legacy_payload_json,'$.appendix_pages') ELSE appendix_pages_json END
WHERE legacy_payload_json IS NOT NULL AND json_valid(legacy_payload_json);
-- A human nobody mapped was stored as an agent called legacy:<name>; the importer now keeps the name
-- as a plain user author instead.
UPDATE message SET author_kind='user', author_id=substr(author_id,8)
WHERE author_kind='agent' AND author_id LIKE 'legacy:%' AND legacy_author_label=substr(author_id,8);
ALTER TABLE message DROP COLUMN legacy_payload_json;
ALTER TABLE message DROP COLUMN legacy_author_label;
ALTER TABLE attachment DROP COLUMN legacy_metadata_json;
CREATE TABLE import_v1_choice (
  id INTEGER PRIMARY KEY CHECK (id=1),
  state TEXT NOT NULL CHECK (state IN ('skipped','done')),
  decided_at TEXT NOT NULL
);
INSERT INTO import_v1_choice(id,state,decided_at)
  SELECT 1,'done',strftime('%Y-%m-%dT%H:%M:%fZ','now')
  WHERE EXISTS (SELECT 1 FROM legacy_import_source);
UPDATE project_memory SET active=1 WHERE active=0;
-- Only imported records the owner has not deliberately switched off: each Deactivate logs a global_knowledge_updated
-- event with active=false, so a record whose latest such event is a deactivation keeps its choice.
UPDATE global_memory SET active=1 WHERE active=0 AND legacy_payload_json IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM event off WHERE off.kind='global_knowledge_updated'
      AND json_extract(off.payload_json,'$.id')=global_memory.id AND json_extract(off.payload_json,'$.active')='false'
      AND NOT EXISTS (
        SELECT 1 FROM event on_ WHERE on_.kind='global_knowledge_updated'
          AND json_extract(on_.payload_json,'$.id')=global_memory.id AND json_extract(on_.payload_json,'$.active')='true'
          AND on_.seq>off.seq));
ALTER TABLE project_memory DROP COLUMN active;
ALTER TABLE project_memory DROP COLUMN legacy_payload_json;
ALTER TABLE project_memory DROP COLUMN legacy_source_message_id;
ALTER TABLE global_memory DROP COLUMN legacy_payload_json;
ALTER TABLE global_memory DROP COLUMN legacy_source_message_id;
`}, {51, `
-- Databases that already ran an earlier migration 50 switched every imported global record back on, including ones the
-- owner had deliberately switched off. The owner's choice survives as the latest global_knowledge_updated event of the
-- record: where that event says active=false but the record is active, put the choice back. A no-op on a database whose
-- records agree with their events, which is every database that ran the corrected migration 50.
UPDATE global_memory SET active=0
WHERE active=1
  AND (SELECT json_extract(e.payload_json,'$.active') FROM event e
       WHERE e.kind='global_knowledge_updated' AND json_extract(e.payload_json,'$.id')=global_memory.id
       ORDER BY e.seq DESC LIMIT 1)='false';
`}, {52, `
-- Agent leaderboards: anonymous peer-review cards. A card keeps only the subject's model and harness at the time, whether
-- the reviewer rated themself, the numbers and when; no reviewer, subject id, room, project or text. They are durable
-- (plain prune keeps them, prune -a removes the database).
CREATE TABLE leaderboard_card (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  subject_model TEXT NOT NULL,
  subject_harness TEXT NOT NULL,
  is_selfreview INTEGER NOT NULL CHECK (is_selfreview IN (0,1)),
  ratings_json TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX leaderboard_card_created ON leaderboard_card(created_at,id);
-- Idempotency for card submissions that survives a plain prune (the request_key table does not): the key is stored only as a
-- hash of actor and key, and kept off the card rows, so a card cannot be tied back to a reviewer.
CREATE TABLE leaderboard_submission (
  key_hash TEXT PRIMARY KEY,
  payload_digest TEXT NOT NULL,
  created_at TEXT NOT NULL
);
-- A lead's start of a voting session carries an Idempotency-Key like the other lead tools, so a retry posts no second note.
ALTER TABLE request_key RENAME TO request_key_old;
CREATE TABLE request_key (
  actor_kind TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN
    ('spawn','team_spawn','control','scope_control','switch','inbox_answer','recovery_decision','room_create','room_close','schedule_create','leaderboard_start')),
  key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (actor_kind,actor_id,kind,key)
);
INSERT INTO request_key SELECT * FROM request_key_old;
DROP TABLE request_key_old;
`}, {53, `
-- The first-run welcome is shown to the owner once. A database that already has people in it predates the welcome, so it counts as
-- seen; a new one has none when its migrations run (the owner is created after them), and a v1 import makes people later, so
-- neither of those is marked here.
INSERT OR IGNORE INTO global_setting(key,value,updated_at)
SELECT 'welcome_seen','1',strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE EXISTS (SELECT 1 FROM user);
`}, {54, roleExtendsMigration()}, {55, `
-- v1's agent sessions, kept for the read-only profile of an imported sender (who has no live agent record), and the size of an image
-- attachment, so a thumbnail has its room before it loads.
CREATE TABLE legacy_agent_session (
  agent_id TEXT PRIMARY KEY,
  harness TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  project TEXT NOT NULL DEFAULT '',
  cwd TEXT NOT NULL DEFAULT '',
  harness_session_id TEXT NOT NULL DEFAULT '',
  resume_command TEXT NOT NULL DEFAULT '',
  registered_at TEXT NOT NULL DEFAULT '',
  last_seen TEXT NOT NULL DEFAULT ''
);
ALTER TABLE attachment ADD COLUMN width INTEGER NOT NULL DEFAULT 0;
ALTER TABLE attachment ADD COLUMN height INTEGER NOT NULL DEFAULT 0;
`}, {56, `
-- Agent leaderboards are removed (rating cards, their idempotency keys and the on/off setting). Idempotent: every statement is
-- a no-op on a database that never had them. The v1 backup keeps v1's cards, so nothing the import could bring is lost.
DROP INDEX IF EXISTS leaderboard_card_created;
DROP TABLE IF EXISTS leaderboard_card;
DROP TABLE IF EXISTS leaderboard_submission;
DELETE FROM global_setting WHERE key='leaderboard_enabled';
DELETE FROM request_key WHERE kind='leaderboard_start';
`}, {57, `
-- Personal interface preferences (theme, notification sound, ...) live with the user so they follow them across devices; the browser
-- keeps only a cache to paint before sign-in.
CREATE TABLE user_preference (
  user_id TEXT NOT NULL REFERENCES user(id) ON DELETE CASCADE,
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (user_id, key)
);
`}, {58, `
-- Cleanup: whether a project's task rooms get their own Git worktree by default, and a journal of what the cleanup removed and why.
ALTER TABLE settings_project ADD COLUMN worktree_mode TEXT NOT NULL DEFAULT 'auto' CHECK (worktree_mode IN ('auto','off'));
-- Automatic worktrees apply only to task rooms created after this moment, so a room that already works in the project folder is never
-- split. Existing projects start counting at the migration; a project made later has no row until it needs one and counts from the start.
ALTER TABLE settings_project ADD COLUMN worktree_auto_since TEXT NOT NULL DEFAULT '';
UPDATE settings_project SET worktree_auto_since=strftime('%Y-%m-%dT%H:%M:%fZ','now');
-- The first spawn into a task room decides where its agents work, and the decision stays: a room is never split between the project
-- folder and a worktree. Empty means not decided yet.
ALTER TABLE room ADD COLUMN checkout_mode TEXT NOT NULL DEFAULT '' CHECK (checkout_mode IN ('','project_folder','worktree'));
UPDATE room SET checkout_mode='worktree' WHERE id IN (SELECT room_id FROM room_worktree);
CREATE TABLE cleanup_log (
  id TEXT PRIMARY KEY,
  seq INTEGER NOT NULL UNIQUE,
  category TEXT NOT NULL,
  target TEXT NOT NULL,
  project_id TEXT,
  reason TEXT NOT NULL,
  bytes INTEGER NOT NULL DEFAULT 0 CHECK (bytes >= 0),
  action TEXT NOT NULL CHECK (action IN ('removed','kept','would_remove')),
  actor TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX cleanup_log_seq ON cleanup_log(seq);
`}, {59, `
-- Only projects are pinned: a person's pinned rooms become a pin of their project, and the per-room flag goes. A pinned direct
-- message has no project in the tree, so its pin is dropped. Every room_view_preference row stays, hidden=0 included: that is an
-- explicit unhide, which a re-import must not turn back into a hide.
CREATE TABLE project_view_preference (
  user_id TEXT NOT NULL REFERENCES user(id),
  project_id TEXT NOT NULL REFERENCES project(id),
  pinned INTEGER NOT NULL DEFAULT 0 CHECK (pinned IN (0,1)),
  PRIMARY KEY (user_id, project_id)
);
INSERT OR IGNORE INTO project_view_preference(user_id,project_id,pinned)
  SELECT v.user_id, r.project_id, 1 FROM room_view_preference v JOIN room r ON r.id=v.room_id WHERE v.pinned=1 AND r.kind<>'dm';
ALTER TABLE room_view_preference DROP COLUMN pinned;
`}, {60, `
-- Attachment retention: an attachment past its retention is removed from disk but its row, and the message it belongs to, stay.
-- removed_at marks it and removed_after_days records the retention in force, so the chat can say "removed after N days".
ALTER TABLE attachment ADD COLUMN removed_at TEXT;
ALTER TABLE attachment ADD COLUMN removed_after_days INTEGER NOT NULL DEFAULT 0;
-- Retention counts from when an attachment arrived in v2, not from when it was first made: an attachment imported from v1, or one that
-- was here before this migration, is kept for the retention from now. Empty means it arrived when it was created.
ALTER TABLE attachment ADD COLUMN arrived_at TEXT NOT NULL DEFAULT '';
UPDATE attachment SET arrived_at=strftime('%Y-%m-%dT%H:%M:%fZ','now');
`}, {61, `
-- Daily limits are per project (all its rooms together), with a global default that projects inherit. A project's cost and turn
-- limits are each "inherit" (the global default, or none when there is none), "custom" (its own value) or "none". Existing project
-- rows keep their meaning: a value is custom, a blank was no limit. Room limits are dropped (room daily limits are dropped here):
-- their rows and day states go, a room one of them had paused is released, and its open notices are resolved.
ALTER TABLE usage_limit ADD COLUMN cost_mode TEXT NOT NULL DEFAULT 'inherit' CHECK (cost_mode IN ('inherit','custom','none'));
ALTER TABLE usage_limit ADD COLUMN turn_mode TEXT NOT NULL DEFAULT 'inherit' CHECK (turn_mode IN ('inherit','custom','none'));
UPDATE usage_limit SET cost_mode=CASE WHEN daily_cost_cap IS NULL THEN 'none' ELSE 'custom' END,
  turn_mode=CASE WHEN daily_turn_cap IS NULL THEN 'none' ELSE 'custom' END WHERE scope_kind='project';
UPDATE room SET gate=CASE WHEN control_desired='active' THEN 'open' ELSE 'paused' END, gate_rev=gate_rev+1
  WHERE id IN (SELECT scope_id FROM usage_limit_day WHERE scope_kind='room' AND paused=1 AND rolled_over=0);
UPDATE inbox_item SET state='resolved', rev=rev+1, resolved_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
  WHERE requester_kind='system' AND kind IN ('usage_info','usage_limit') AND state='open' AND requester_id LIKE 'room:%';
DELETE FROM usage_limit_day WHERE scope_kind='room';
DELETE FROM usage_limit WHERE scope_kind='room';
`}, {62, `
-- Saved teams are gone: a lead spawns single agents. The team spawn request kind stays in the request_key check, since
-- existing rows may carry it and a rebuild of that table is not worth a removed feature.
DROP TABLE team;
` + roleTeamlessLeadMigration()}, {63, `
-- Composer snippets are gone; the quick commands and macros remain.
DROP TABLE snippet;
`}, {64, `
-- Roles are global only. A deleted project role stays as a deleted global row (the history of agents that held it). A live project
-- role becomes global when no live global role has its key (the first by id when several projects define the same key), without its
-- capabilities (lead, can spawn, singleton writer: a project owner's role must not gain instance-wide rights); any other
-- live project role is dropped, and its agents move to the global role with that key. Each is logged by logProjectRoles.
UPDATE role SET project_id=NULL WHERE project_id IS NOT NULL AND deleted_at IS NOT NULL;
UPDATE role SET project_id=NULL, key=role_key, rev=rev+1, cap_is_lead=0, cap_can_spawn=0, cap_singleton_writer=0
 WHERE project_id IS NOT NULL AND deleted_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM role g WHERE g.project_id IS NULL AND g.deleted_at IS NULL AND g.role_key=role.role_key)
   AND id=(SELECT MIN(q.id) FROM role q WHERE q.project_id IS NOT NULL AND q.deleted_at IS NULL AND q.role_key=role.role_key);
UPDATE agent SET role_id=(SELECT g.id FROM role g WHERE g.project_id IS NULL AND g.deleted_at IS NULL
   AND g.role_key=(SELECT r.role_key FROM role r WHERE r.id=agent.role_id))
 WHERE role_id IN (SELECT id FROM role WHERE project_id IS NOT NULL AND deleted_at IS NULL);
UPDATE role SET project_id=NULL, deleted_at=strftime('%Y-%m-%dT%H:%M:%S','now') || '.000000000Z', key='deleted:' || id, rev=rev+1
 WHERE project_id IS NOT NULL AND deleted_at IS NULL;
-- The legacy project lead text only ever mirrored a project lead role; nothing reads it.
ALTER TABLE project DROP COLUMN lead_instructions;
`}, {65, `
-- Prompt overrides: owner-edited replacements for the shared prose sentences of an adapter's turn prompt, global (project_id NULL)
-- or per project. The slot keys are checked in code (internal/adapter/prompt), so a new slot needs no migration.
CREATE TABLE prompt_override (
  id TEXT PRIMARY KEY,
  project_id TEXT REFERENCES project(id) ON DELETE CASCADE,
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  rev INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX prompt_override_scope_key ON prompt_override(COALESCE(project_id,''),key);
`}, {66, `
-- Pins are gone: the manual project order does the job. A person who had pinned projects and no manual order of their own gets one
-- that starts with those projects (oldest first), so nobody loses the order they saw; the rest follow in creation order.
INSERT INTO user_preference(user_id,key,value,updated_at)
  SELECT v.user_id, 'sidebar_project_order', json_group_array(v.project_id ORDER BY p.created_at, p.id), strftime('%Y-%m-%dT%H:%M:%fZ','now')
  FROM project_view_preference v JOIN project p ON p.id=v.project_id
  WHERE v.pinned=1 AND v.user_id NOT IN (SELECT user_id FROM user_preference WHERE key='sidebar_project_order')
  GROUP BY v.user_id;
DROP TABLE project_view_preference;
`}, {67, `
-- A global admin: an account the owner has given the owner's reach over people, projects and global settings (never over the owner,
-- other admins, machines or provider logins). The owner is the first account and is not flagged: it is admin by being the owner.
ALTER TABLE user ADD COLUMN admin INTEGER NOT NULL DEFAULT 0 CHECK (admin IN (0,1));
`}, {68, `
-- "Start as v2 agent": an agent that exists only in imported v1 history can be started in v2, continuing its harness session. The
-- legacy row remembers which v2 agent was started from it, the v2 agent remembers where it came from, and the roles v1 gave the agent in
-- its rooms are kept to suggest one.
ALTER TABLE legacy_agent_session ADD COLUMN started_agent_id TEXT;
ALTER TABLE agent ADD COLUMN resumed_from_legacy_id TEXT;
CREATE TABLE legacy_agent_role (
  agent_id TEXT NOT NULL,
  v1_room_id TEXT NOT NULL,
  role_key TEXT NOT NULL,
  PRIMARY KEY (agent_id, v1_room_id)
);
`}, {69, `
-- Harnesses: a machine's launcher is the harness the owner set up, with a label and its own model list. Nothing is seeded any more.
ALTER TABLE machine_launcher ADD COLUMN label TEXT NOT NULL DEFAULT '';
-- harness_keep (made by the Go step that runs first) names the one disabled launcher per tool kept so that an owner-customised model list
-- survives when every launcher of that tool would otherwise go.
CREATE TEMP TABLE IF NOT EXISTS harness_keep(id TEXT PRIMARY KEY);
-- The seeded rows nobody enabled or used are not worth carrying over as harnesses.
DELETE FROM machine_launcher WHERE preset=1 AND enabled=0
  AND id NOT IN (SELECT launcher_id FROM agent WHERE launcher_id IS NOT NULL)
  AND id NOT IN (SELECT launcher_id FROM agent_switch WHERE launcher_id IS NOT NULL)
  AND id NOT IN (SELECT id FROM harness_keep);
UPDATE machine_launcher SET preset=0,
  label=(CASE tool WHEN 'claude' THEN 'Claude Code' WHEN 'codex' THEN 'Codex' WHEN 'pi' THEN 'Pi' WHEN 'opencode' THEN 'OpenCode' ELSE tool END)
    || (CASE WHEN containerized=1 THEN ' (Docker)' WHEN name<>'native' THEN ' · ' || name ELSE '' END);
CREATE TABLE harness_model (
  launcher_id TEXT NOT NULL REFERENCES machine_launcher(id) ON DELETE CASCADE,
  slug TEXT NOT NULL,
  label TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0,1)),
  position INTEGER NOT NULL,
  PRIMARY KEY (launcher_id, slug)
);
CREATE UNIQUE INDEX harness_model_default ON harness_model(launcher_id) WHERE is_default=1;
-- The old per-tool list becomes every kept harness's own list; its first model is the default.
INSERT INTO harness_model(launcher_id,slug,label,note,is_default,position)
  SELECT l.id, s.model_id, s.label, '', CASE WHEN s.position=(SELECT MIN(position) FROM spawn_model WHERE tool=s.tool) THEN 1 ELSE 0 END, s.position
  FROM machine_launcher l JOIN spawn_model s ON s.tool=l.tool;
-- A model an existing agent is using stays usable even if the old list never had it, so no live agent is switched to the default by this
-- migration; it is added after the copied ones and is never the default. A switch counts only while it is under way (draining or starting):
-- a failed one is never retried and would bring back a model the owner removed.
INSERT OR IGNORE INTO harness_model(launcher_id,slug,label,note,is_default,position)
  SELECT launcher_id, model, substr(model,1,80), 'in use by an agent when harnesses were introduced', 0,
         1000 + ROW_NUMBER() OVER (PARTITION BY launcher_id ORDER BY model)
  FROM (SELECT launcher_id, model FROM agent WHERE launcher_id IS NOT NULL AND revoked_at IS NULL AND model<>''
        UNION SELECT launcher_id, model FROM agent_switch WHERE launcher_id IS NOT NULL AND model<>'' AND state IN ('draining','starting'));
-- A harness that only has such models still needs a default: its first.
UPDATE harness_model SET is_default=1 WHERE rowid IN (
  SELECT m.rowid FROM harness_model m
  WHERE m.position=(SELECT MIN(position) FROM harness_model WHERE launcher_id=m.launcher_id)
    AND NOT EXISTS (SELECT 1 FROM harness_model d WHERE d.launcher_id=m.launcher_id AND d.is_default=1));
DROP TABLE spawn_model;
DROP TABLE IF EXISTS harness_keep;
`}, {70, `
-- Home room: lead only. While on, spawning into a project's home room accepts only roles with the lead capability, so workers go to
-- task rooms (such as general). Every project, existing ones included, starts with it on; agents already in a home room are untouched.
ALTER TABLE settings_project ADD COLUMN home_lead_only INTEGER NOT NULL DEFAULT 1 CHECK (home_lead_only IN (0,1));
`}, {71, `
-- Message numbers: one global sequence for messages alone (system messages included), the "#N" people read, copy and write, as in v1. It
-- is separate from seq_counter, which keeps ordering messages and events for paging, read cursors and the event stream: events never move
-- a message number. A trigger numbers every message row inserted without one, so no insert path can skip it; an import writes the v1 id.
-- This migration refuses to run over existing messages (see the check in migrateUpTo): nothing is renumbered.
CREATE TABLE message_no_counter (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  value INTEGER NOT NULL CHECK (value >= 0)
);
INSERT INTO message_no_counter(id, value) VALUES (1, 0);
ALTER TABLE message ADD COLUMN no INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX message_no_unique ON message(no) WHERE no > 0;
CREATE TRIGGER message_assign_no AFTER INSERT ON message WHEN NEW.no = 0 BEGIN
  UPDATE message_no_counter SET value = value + 1 WHERE id = 1;
  UPDATE message SET no = (SELECT value FROM message_no_counter WHERE id = 1) WHERE id = NEW.id;
END;
`}, {72, `
-- A project's lead is a member of every open room of its project (private chats aside), so it can read a child room it did not make and
-- resolve a #N into it. New rooms and newly spawned leads join in code; this brings the existing rooms up to date. Such a membership is
-- marked auto: it lets the lead read the room, but an unaddressed human message does not wake the lead through it (only a lead that was put
-- in the room on purpose is woken by those).
ALTER TABLE room_member ADD COLUMN auto INTEGER NOT NULL DEFAULT 0 CHECK (auto IN (0,1));
INSERT INTO room_member(room_id, member_kind, member_id, joined_at, auto)
SELECT r.id, 'agent', a.id, strftime('%Y-%m-%dT%H:%M:%S.000000000Z', 'now'), 1
FROM agent a JOIN role ro ON ro.id = a.role_id JOIN room r ON r.project_id = a.project_id
WHERE a.revoked_at IS NULL AND ro.cap_is_lead = 1 AND r.kind <> 'dm' AND r.lifecycle = 'open'
  AND NOT EXISTS (SELECT 1 FROM room_member m WHERE m.room_id = r.id AND m.member_kind = 'agent' AND m.member_id = a.id AND m.left_at IS NULL);
`}, {73, `
-- Per-room notifications, per person: how loudly a room speaks to them (default: the inbox and messages that address them; all: every
-- message; mentions: only messages that address them, and the unread badge counts only those; none: nothing, the inbox chime of that room
-- included) and an optional time until which the room is muted. The mute is an instant in the future, so it ends by itself.
ALTER TABLE room_view_preference ADD COLUMN notify TEXT NOT NULL DEFAULT 'default' CHECK (notify IN ('default','all','mentions','none'));
ALTER TABLE room_view_preference ADD COLUMN muted_until TEXT;
`}, {74, `
-- "Start as v2 agent" asks whether a v1 agent wrote anywhere in a project: look the agent up by author, then check its rooms, instead of
-- reading every message of every room of the project.
CREATE INDEX message_agent_author_room ON message(author_id, room_id) WHERE author_kind = 'agent';
`}, {75, `
-- Every usage limit check, the agent card and the agent profile read usage_record by agent, room or project and a time range, and the
-- limit check counts a scope's turns by room and start time; with no index each of those read the whole table. usage_record gets one
-- index per way it is read, turn gets the room and time one, and room the project one (a project's turns are found through its rooms).
CREATE INDEX usage_record_agent_time ON usage_record(agent_id, created_at);
CREATE INDEX usage_record_room_time ON usage_record(room_id, created_at);
CREATE INDEX usage_record_project_time ON usage_record(project_id, created_at);
CREATE INDEX usage_record_turn ON usage_record(turn_id);
CREATE INDEX turn_room_started ON turn(room_id, started_at);
CREATE INDEX room_project ON room(project_id);
`}, {76, `
-- A message's attachments are looked up by message id (loading a message, and previewing the message a reply answers), and attachment had
-- no index on it, so each lookup read the whole table.
CREATE INDEX attachment_message ON attachment(message_id);
`}, {77, `
-- A role's ordered default harness/model choices are global. Projects may
-- override the complete list and picking mode; a missing row inherits both.
ALTER TABLE role ADD COLUMN spawn_defaults_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE role ADD COLUMN spawn_pick_mode TEXT NOT NULL DEFAULT 'in_turn'
  CHECK (spawn_pick_mode IN ('in_turn','randomly'));
CREATE TABLE project_role_spawn_default (
  project_id TEXT NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  role_id TEXT NOT NULL REFERENCES role(id) ON DELETE CASCADE,
  items_json TEXT NOT NULL DEFAULT '[]',
  pick_mode TEXT NOT NULL DEFAULT 'in_turn' CHECK (pick_mode IN ('in_turn','randomly')),
  PRIMARY KEY (project_id,role_id)
);
CREATE TABLE role_spawn_cursor (
  project_id TEXT NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  role_id TEXT NOT NULL REFERENCES role(id) ON DELETE CASCADE,
  next_position INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (project_id,role_id)
);
CREATE TABLE role_spawn_warning (
  room_id TEXT NOT NULL REFERENCES room(id) ON DELETE CASCADE,
  role_id TEXT NOT NULL REFERENCES role(id) ON DELETE CASCADE,
  last_warned_at TEXT NOT NULL,
  PRIMARY KEY (room_id,role_id)
);
`}, {78, `
-- What a turn spent, in more detail. The cache tokens and the model are what each message's usage shows; partial marks what a turn spent before
-- it ended with no report (killed, crashed); cost_total is the harness session's running total, kept only so the next turn of the session can be
-- priced as the difference (Claude reports a resumed session's whole spend, so cost_est, which every limit sums, is the difference).
ALTER TABLE usage_record ADD COLUMN tokens_cache_read INTEGER;
ALTER TABLE usage_record ADD COLUMN tokens_cache_write INTEGER;
ALTER TABLE usage_record ADD COLUMN model TEXT;
ALTER TABLE usage_record ADD COLUMN partial INTEGER NOT NULL DEFAULT 0 CHECK (partial IN (0,1));
ALTER TABLE usage_record ADD COLUMN cost_total REAL;
-- Claude's cost_est, as stored before this migration, is the session's running total, which is exactly what cost_total remembers: a session that
-- spans the upgrade continues from its last total instead of counting the whole session again on its next turn.
UPDATE usage_record SET cost_total=cost_est WHERE cost_total IS NULL AND cost_est IS NOT NULL AND agent_id IN (SELECT id FROM agent WHERE tool='claude');
`}, {79, `
-- The lead no longer implements anything itself: an unedited built-in lead body gains the ban and the rule for when no worker can be spawned.
` + roleLeadNoImplementMigration()}, {80, `
-- A project can be archived: read-only, its agents stopped, listed apart in the sidebar, and reopened later. NULL is a live project.
ALTER TABLE project ADD COLUMN archived_at TEXT;
`}, {81, `
-- Agent-to-agent private chats belong to no real project. Their rooms hang off one reserved system project: it has no members, no folder, and
-- never appears in a list, but it keeps room.project_id (NOT NULL, referenced by many joins) valid without rebuilding the room table.
ALTER TABLE project ADD COLUMN system INTEGER NOT NULL DEFAULT 0 CHECK (system IN (0,1));
INSERT INTO project(id,name,folder,created_at,system) VALUES ('prj_agentdm','Agent conversations','',strftime('%Y-%m-%dT%H:%M:%fZ','now'),1);
INSERT INTO settings_project(project_id) VALUES ('prj_agentdm');
-- A lead's dm_agent call carries an Idempotency-Key like the other lead tools, so a retry posts no second message.
ALTER TABLE request_key RENAME TO request_key_old;
CREATE TABLE request_key (
  actor_kind TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN
    ('spawn','team_spawn','control','scope_control','switch','inbox_answer','recovery_decision','room_create','room_close','schedule_create','leaderboard_start','dm_agent')),
  key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (actor_kind,actor_id,kind,key)
);
INSERT INTO request_key SELECT * FROM request_key_old;
DROP TABLE request_key_old;
`}, {82, `
-- The reserved project of chats between agents is an implementation detail, not a project: no one may be a member of it, and it can be
-- neither changed nor deleted (archiving, renaming, moving its folder and removing it are all updates or a delete of its row). A later
-- migration that updates every project row has to leave system rows out.
CREATE TRIGGER project_system_unchanged BEFORE UPDATE ON project WHEN OLD.system=1
BEGIN SELECT RAISE(ABORT, 'the reserved project cannot be changed'); END;
CREATE TRIGGER project_system_kept BEFORE DELETE ON project WHEN OLD.system=1
BEGIN SELECT RAISE(ABORT, 'the reserved project cannot be deleted'); END;
CREATE TRIGGER membership_system_none BEFORE INSERT ON membership WHEN (SELECT system FROM project WHERE id=NEW.project_id)=1
BEGIN SELECT RAISE(ABORT, 'nobody is a member of the reserved project'); END;
`}, {83, `
-- A usage-limit hold can be recorded for a Mistral Vibe agent: the tool check lists it.
CREATE TABLE provider_limit_new (
  agent_id TEXT PRIMARY KEY REFERENCES agent(id),
  turn_id TEXT NOT NULL REFERENCES turn(id),
  detected_at TEXT NOT NULL,
  reset_at TEXT,
  tool TEXT NOT NULL CHECK (tool IN ('claude','codex','pi','opencode','vibe'))
);
INSERT INTO provider_limit_new(agent_id,turn_id,detected_at,reset_at,tool)
SELECT agent_id,turn_id,detected_at,reset_at,tool FROM provider_limit;
DROP TABLE provider_limit;
ALTER TABLE provider_limit_new RENAME TO provider_limit;
`}, {84, `
-- Reconnect and project deletion look up the current applied stop revision for
-- each agent. Keep that lookup bounded as the event journal grows.
CREATE INDEX event_control_applied_agent_rev ON event(agent_id, CAST(json_extract(payload_json,'$.desired_rev') AS INTEGER))
  WHERE kind='agent_control_applied';
`}, {85, `
-- A Claude Code session continued as an agent: the machine it lives on, its id, and the one agent that continues it. Two agents on one
-- session would append to one transcript at once, so a session is attached to at most one agent; the row goes with the agent.
CREATE TABLE imported_session (
  machine_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  agent_id TEXT NOT NULL UNIQUE REFERENCES agent(id),
  cwd TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  imported_by TEXT NOT NULL,
  imported_at TEXT NOT NULL,
  PRIMARY KEY (machine_id, session_id)
);
`}, {86, `
-- A Claude Code session can also be worked on as a copy: the agent's first turn forks it (--fork-session), Claude saves the copy under a new
-- id, and the original stays as it was. session.fork_of is the original's id while the first turn has not yet reported the copy's id (the
-- session still names the original then); imported_session keeps the original's id and, once known, the copy's. Copies of one original can be
-- many, so an original is held by at most one agent only for a continued session (a partial unique index), and the table is keyed by agent.
ALTER TABLE session ADD COLUMN fork_of TEXT;
CREATE TABLE imported_session_new (
  agent_id TEXT PRIMARY KEY REFERENCES agent(id),
  machine_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  mode TEXT NOT NULL DEFAULT 'continue' CHECK (mode IN ('continue','fork')),
  forked_session_id TEXT,
  cwd TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  imported_by TEXT NOT NULL,
  imported_at TEXT NOT NULL
);
INSERT INTO imported_session_new(agent_id,machine_id,session_id,mode,cwd,title,imported_by,imported_at)
SELECT agent_id,machine_id,session_id,'continue',cwd,title,imported_by,imported_at FROM imported_session;
DROP TABLE imported_session;
ALTER TABLE imported_session_new RENAME TO imported_session;
CREATE UNIQUE INDEX imported_session_continued ON imported_session(machine_id,session_id) WHERE mode='continue';
CREATE INDEX imported_session_original ON imported_session(machine_id,session_id);
`}, {87, `
-- A Claude Code session saved under a link to its folder is continued with the harness started in that spelling (see launch_path on the
-- delivery); the agent keeps the canonical checkout. Empty for every agent that does not need it.
ALTER TABLE agent ADD COLUMN launch_path TEXT NOT NULL DEFAULT '';
-- An agent's dm_human call carries an Idempotency-Key like the other tools that write, so a retry posts no second message.
ALTER TABLE request_key RENAME TO request_key_old;
CREATE TABLE request_key (
  actor_kind TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN
    ('spawn','team_spawn','control','scope_control','switch','inbox_answer','recovery_decision','room_create','room_close','schedule_create','leaderboard_start','dm_agent','dm_human')),
  key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  result_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (actor_kind,actor_id,kind,key)
);
INSERT INTO request_key SELECT * FROM request_key_old;
DROP TABLE request_key_old;
`}, {88, `
-- Which session wrote a v1 message, and every session a v1 agent name ran in (v1 keeps the latest per name in agent_sessions; later v1
-- builds also keep a history and stamp each message with its sender's session). They let "Start as v2 agent" offer the session that
-- really holds a sender's work. The history table has no row for a backup that predates it.
ALTER TABLE message ADD COLUMN author_session TEXT NOT NULL DEFAULT '';
CREATE TABLE legacy_agent_session_history (
  agent_id TEXT NOT NULL,
  harness_session_id TEXT NOT NULL,
  harness TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  project TEXT NOT NULL DEFAULT '',
  cwd TEXT NOT NULL DEFAULT '',
  origin TEXT NOT NULL DEFAULT '',
  registered_at TEXT NOT NULL DEFAULT '',
  last_seen TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (agent_id, harness_session_id)
);
`}, {89, `
-- The lead role's emoji is a crown, not a compass. Only a role that still carries the compass it was created with changes: an emoji the owner
-- picked (anything else, including the owner choosing the compass again before this ran) is not told apart from the shipped one, but nothing
-- else is touched.
UPDATE role SET emoji='👑' WHERE role_key='lead' AND emoji='🧭' AND deleted_at IS NULL;
`}, {90, `
CREATE INDEX IF NOT EXISTS event_turn_kind ON event(turn_id, kind) WHERE turn_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS event_agent_created ON event(agent_id, created_at) WHERE agent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS turn_agent ON turn(agent_id, id);
`}, {91, `
-- Custom OpenCode providers: an OpenAI-compatible endpoint an OpenCode harness can run models on. Only the NAME of the environment variable the
-- key is read from is stored, never a key; a provider's models are ordinary harness_model rows whose slug starts with "<id>/".
CREATE TABLE harness_provider (
  launcher_id TEXT NOT NULL REFERENCES machine_launcher(id) ON DELETE CASCADE,
  id TEXT NOT NULL,
  name TEXT NOT NULL,
  npm TEXT NOT NULL,
  base_url TEXT NOT NULL,
  key_env TEXT NOT NULL DEFAULT '',
  context_tokens INTEGER NOT NULL DEFAULT 0 CHECK (context_tokens >= 0),
  output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
  position INTEGER NOT NULL,
  PRIMARY KEY (launcher_id, id)
);
`}, {92, `
-- A second writer in a room gets a Git worktree of its own instead of a refusal: one row per agent that works in one. name is the directory
-- and branch suffix under .aimebu/worktrees (<room-id>-<slug>), canonical_path where it is on the machine. The agent's checkout points at
-- that path, so the checkout's writer slot is the agent's own. Cleanup lists these; DELETE /api/agents/{id}/worktree removes one.
CREATE TABLE agent_worktree (
  agent_id TEXT PRIMARY KEY REFERENCES agent(id),
  room_id TEXT NOT NULL REFERENCES room(id),
  machine_id TEXT NOT NULL REFERENCES machine(id),
  name TEXT NOT NULL,
  branch_name TEXT NOT NULL,
  canonical_path TEXT NOT NULL,
  base_ref TEXT NOT NULL DEFAULT '',
  base_commit TEXT NOT NULL DEFAULT '',
  dirty_base_count INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'ready' CHECK (state IN ('ready','preserved','removed')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX agent_worktree_path ON agent_worktree(machine_id, canonical_path);
`}, {93, `
-- Removing an agent frees its name: slug (unique within the project) is rewritten to a tombstone, and removed_slug keeps the name it had, which the
-- messages it wrote are shown under ("<name> (removed)"). Empty for every agent that was not removed.
ALTER TABLE agent ADD COLUMN removed_slug TEXT NOT NULL DEFAULT '';
`}, {94, `
-- A session now also has an absolute lifetime counted from when it was made, on top of the sliding expiry. Sessions that exist when this runs
-- start their lifetime now, since when they were made was not kept.
ALTER TABLE auth_session ADD COLUMN created_at TEXT NOT NULL DEFAULT '';
UPDATE auth_session SET created_at=strftime('%Y-%m-%dT%H:%M:%fZ','now');
`}, {95, `
-- Telemetry: each person has a random fingerprint that identifies their error reports instead of their account (empty until first needed, then
-- generated; not derived from the account id), and may choose to send their login name instead.
ALTER TABLE user ADD COLUMN telemetry_fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE user ADD COLUMN telemetry_send_name INTEGER NOT NULL DEFAULT 0 CHECK (telemetry_send_name IN (0,1));
`}, {96, `
-- An agent's seen marker in a room also counts the messages the router handed it with nothing for it to do (not addressed to it, system
-- chatter, an unaddressed message for the worker while it sits in the room as the lead), as a waiting v1 agent received them. The newest
-- such message per room and agent is kept here as it happens, so reading the markers is a key lookup, not a pass over the room's events.
-- The rows the existing message_not_woken events imply are filled in once.
CREATE TABLE agent_room_seen (
  room_id TEXT NOT NULL REFERENCES room(id),
  agent_id TEXT NOT NULL REFERENCES agent(id),
  seq INTEGER NOT NULL CHECK (seq > 0),
  PRIMARY KEY (room_id, agent_id)
) WITHOUT ROWID;
INSERT INTO agent_room_seen(room_id, agent_id, seq)
SELECT e.room_id, e.agent_id, MAX(m.seq)
FROM event e JOIN message m ON m.id=json_extract(e.payload_json,'$.message_id') AND m.room_id=e.room_id
WHERE e.room_id IS NOT NULL AND e.kind='message_not_woken'
  AND json_extract(e.payload_json,'$.reason') IN ('not_addressed','system_chatter','auto_member')
  AND EXISTS (SELECT 1 FROM agent a WHERE a.id=e.agent_id)
GROUP BY e.room_id, e.agent_id;
`}, {97, `
-- A cache keep-alive ping spends like a turn but is not one: its usage record has no turn (turn_id may be empty), carries the harness session it
-- continued (session_id, so the session's running cost total stays right for the next real turn) and the source 'keep_alive'. SQLite cannot
-- relax a NOT NULL or a CHECK in place, so the table is rebuilt with the same columns and indexes; every existing row is copied unchanged.
CREATE TABLE usage_record_new (
  id TEXT PRIMARY KEY,
  source_event_id TEXT NOT NULL UNIQUE,
  turn_id TEXT REFERENCES turn(id),
  agent_id TEXT NOT NULL REFERENCES agent(id),
  room_id TEXT NOT NULL REFERENCES room(id),
  project_id TEXT NOT NULL REFERENCES project(id),
  tokens_in INTEGER,
  tokens_out INTEGER,
  context_used INTEGER,
  context_window INTEGER,
  cost_est REAL,
  source TEXT NOT NULL CHECK (source IN ('measured','estimated','unavailable','keep_alive')),
  created_at TEXT NOT NULL,
  tokens_cache_read INTEGER,
  tokens_cache_write INTEGER,
  model TEXT,
  partial INTEGER NOT NULL DEFAULT 0 CHECK (partial IN (0,1)),
  cost_total REAL,
  session_id TEXT
);
INSERT INTO usage_record_new(id,source_event_id,turn_id,agent_id,room_id,project_id,tokens_in,tokens_out,context_used,context_window,cost_est,source,created_at,tokens_cache_read,tokens_cache_write,model,partial,cost_total)
SELECT id,source_event_id,turn_id,agent_id,room_id,project_id,tokens_in,tokens_out,context_used,context_window,cost_est,source,created_at,tokens_cache_read,tokens_cache_write,model,partial,cost_total FROM usage_record;
DROP TABLE usage_record;
ALTER TABLE usage_record_new RENAME TO usage_record;
CREATE INDEX usage_record_agent_time ON usage_record(agent_id, created_at);
CREATE INDEX usage_record_room_time ON usage_record(room_id, created_at);
CREATE INDEX usage_record_project_time ON usage_record(project_id, created_at);
CREATE INDEX usage_record_turn ON usage_record(turn_id);
`}, {98, `
-- One concept, the reply. A writer chooses only reply_to; thread_root is the first message of the conversation, derived from the reply chain
-- (see InsertMessage). Existing rows are brought in line once: a message that was only in a thread (thread_root without reply_to) becomes
-- a reply to that thread's first message, and every reply gets the first message of its chain as its thread_root. A reply whose parent is
-- in another room is not followed (a chain stops at a parent that has no in-room parent) and keeps what it had; unchanged rows are not rewritten.
-- The depth limit of 10000 below only guards against a reply loop: a row deeper than that is not reached and keeps its old values.
-- reply_to gets an index for walking chains (here and for the conversation view).
CREATE INDEX message_reply_to ON message(reply_to) WHERE reply_to IS NOT NULL;
UPDATE message SET reply_to=thread_root WHERE (reply_to IS NULL OR reply_to='') AND thread_root IS NOT NULL AND thread_root<>'';
CREATE TEMP TABLE reply_root(id TEXT PRIMARY KEY, root TEXT NOT NULL) WITHOUT ROWID;
WITH RECURSIVE chain(id, room_id, root, depth) AS (
  SELECT m.id, m.room_id, p.id, 1 FROM message m JOIN message p ON p.id=m.reply_to AND p.room_id=m.room_id
  WHERE NOT EXISTS (SELECT 1 FROM message g WHERE g.id=p.reply_to AND g.room_id=p.room_id)
  UNION ALL
  SELECT m.id, m.room_id, chain.root, chain.depth+1 FROM chain JOIN message m ON m.reply_to=chain.id AND m.room_id=chain.room_id
  WHERE chain.depth < 10000
)
INSERT OR IGNORE INTO reply_root(id, root) SELECT id, root FROM chain;
UPDATE message SET thread_root=(SELECT root FROM reply_root WHERE reply_root.id=message.id) WHERE id IN (SELECT id FROM reply_root) AND thread_root IS NOT (SELECT root FROM reply_root WHERE reply_root.id=message.id);
DROP TABLE reply_root;
`}, {99, `
-- Preserve an explicit fresh start while folder or machine admission waits
-- for the owner. Approval must not silently resume the old native session.
ALTER TABLE boundary_pending ADD COLUMN start_fresh INTEGER NOT NULL DEFAULT 0 CHECK (start_fresh IN (0,1));
`}, {100, roleLeadDirectTaskMigration()}, {101, roleRoomMentionMigration()}, {102, `-- Role key and ID rename is applied by migrateLeaderRole.
UPDATE role SET rev=rev WHERE 0;`}, {103, `
-- A physical folder can serve multiple projects. Each project owns its own
-- checkout row and writer slot, while existing checkout IDs remain stable.
CREATE TABLE checkout_new (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES project(id),
  machine_id TEXT NOT NULL REFERENCES machine(id),
  canonical_path TEXT NOT NULL,
  writer_agent_id TEXT REFERENCES agent(id),
  writer_state TEXT NOT NULL DEFAULT 'none' CHECK (writer_state IN ('none','reserved','releasing')),
  UNIQUE (project_id, machine_id, canonical_path)
);
INSERT INTO checkout_new(id,project_id,machine_id,canonical_path,writer_agent_id,writer_state)
  SELECT id,project_id,machine_id,canonical_path,writer_agent_id,writer_state FROM checkout;
DROP TABLE checkout;
ALTER TABLE checkout_new RENAME TO checkout;
`}, {104, `
-- Existing model lists have no reliable provenance, so preserve them as manual.
-- New detected models can be removed on sync; an agent still using one keeps
-- an unavailable row so its configured model survives its next turn.
ALTER TABLE harness_model ADD COLUMN source TEXT NOT NULL DEFAULT 'manual' CHECK (source IN ('manual','detected'));
ALTER TABLE harness_model ADD COLUMN available INTEGER NOT NULL DEFAULT 1 CHECK (available IN (0,1));
`}, {105, `
CREATE TABLE IF NOT EXISTS inbox_item_seen (
  item_id TEXT NOT NULL REFERENCES inbox_item(id) ON DELETE CASCADE,
  user_id TEXT NOT NULL REFERENCES user(id) ON DELETE CASCADE,
  seen_at TEXT NOT NULL,
  PRIMARY KEY (item_id, user_id)
);
`}}

// Migration 2 normalizes timestamps written with RFC3339Nano in migration 1.
// Go omitted trailing zeros there, which made TEXT ordering unreliable.
func legacyTimestampUpdates() string {
	columns := [][2]string{
		{"schema_version", "applied_at"},
		{"project", "created_at"},
		{"room", "created_at"}, {"room", "closed_at"},
		{"room_member", "joined_at"}, {"room_member", "left_at"},
		{"user", "created_at"},
		{"auth_session", "expires_at"}, {"auth_session", "revoked_at"},
		{"message", "created_at"},
		{"attachment", "created_at"},
		{"reaction", "created_at"},
		{"event", "created_at"},
	}
	var sql strings.Builder
	for _, pair := range columns {
		table, column := pair[0], pair[1]
		fmt.Fprintf(&sql, `
UPDATE %s SET %s = substr(%s,1,19) || '.' ||
  substr((CASE WHEN substr(%s,20,1)='.' THEN substr(%s,21,length(%s)-21) ELSE '' END)
    || '000000000',1,9) || 'Z'
WHERE %s GLOB '????-??-??T??:??:??*Z'
  AND substr(%s,20,1) IN ('.','Z');
`, table, column, column, column, column, column, column, column)
	}
	return sql.String()
}

// roomLimitLines describes each room daily limit the migration is about to drop (which room, which caps), so that what was there can
// be read afterwards. migrate writes them to the log once the migration has committed, never for one that rolled back.
func roomLimitLines(ctx context.Context, tx *sql.Tx, version int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT scope_id,daily_cost_cap,daily_turn_cap FROM usage_limit WHERE scope_kind='room' ORDER BY scope_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id string
		var cost sql.NullFloat64
		var turns sql.NullInt64
		if err := rows.Scan(&id, &cost, &turns); err != nil {
			return nil, err
		}
		costText, turnsText := "none", "none"
		if cost.Valid {
			costText = "$" + strconv.FormatFloat(cost.Float64, 'f', -1, 64)
		}
		if turns.Valid {
			turnsText = fmt.Sprintf("%d turns", turns.Int64)
		}
		lines = append(lines, fmt.Sprintf("migration %d: dropped the daily limit of room %s (cost %s, turns %s)", version, id, costText, turnsText))
	}
	return lines, rows.Err()
}

// roomLimitsMarker is in the text of the migration that drops room daily limits, which logs how many it dropped. It is found by its
// text, not its number, so a renumbering at merge does not break it.
const roomLimitsMarker = "room daily limits are dropped here"

func migrate(ctx context.Context, db *sql.DB) error { return migrateUpTo(ctx, db, 0) }

// migrateUpTo applies the pending migrations up to and including version limit (0 means all); tests use it to stop before a migration.
func migrateUpTo(ctx context.Context, db *sql.DB, limit int) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY CHECK (version > 0),
		applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	var current int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}
	if current > migrations[len(migrations)-1].version {
		return fmt.Errorf("database version %d is newer than supported version %d", current, migrations[len(migrations)-1].version)
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if limit > 0 && m.version > limit {
			break
		}
		if m.version == 103 {
			if err := migrateCheckoutPerProject(ctx, db, m); err != nil {
				return fmt.Errorf("migration %d: %w", m.version, err)
			}
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		deactivated := 0
		if m.version == 38 {
			err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_memory WHERE legacy_payload_json IS NOT NULL`).Scan(&deactivated)
		}
		if err == nil && m.version == 71 {
			if ctx.Value(keepHistoryUnnumbered{}) == nil {
				err = refuseMessageNumbersOverHistory(ctx, tx)
			}
		}
		var harnessFacts []string
		if err == nil && m.version == 69 {
			harnessFacts, err = harnessMigrationFacts(ctx, tx)
		}
		if err == nil && m.version == 54 {
			err = ensureRoleExtendsColumn(ctx, tx)
		}
		var droppedRoomLimits []string
		if err == nil && strings.Contains(m.sql, roomLimitsMarker) {
			droppedRoomLimits, err = roomLimitLines(ctx, tx, m.version)
		}
		if err == nil && m.version == 64 {
			err = logProjectRoles(ctx, tx)
		}
		var leaderFacts []string
		if err == nil && m.version == 102 {
			leaderFacts, err = migrateLeaderRole(ctx, tx)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, m.sql)
		}
		if err == nil && m.version == 42 {
			// Seed the shipped default macros once, in the same transaction.
			err = seedDefaultMacros(ctx, tx)
		}
		if err == nil {
			if m.version == 28 && os.Getenv("AIMEBU2_ALLOW_NATIVE") == "1" {
				_, err = tx.ExecContext(ctx, `UPDATE machine_launcher SET enabled=COALESCE((SELECT enabled FROM machine_harness_setting h WHERE h.machine_id=machine_launcher.machine_id AND h.tool=machine_launcher.tool AND h.variant='native'),1) WHERE name='native' AND machine_id IN (SELECT id FROM machine WHERE is_host=1)`)
			}
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_version(version, applied_at) VALUES (?, ?)`, m.version, timestamp())
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", m.version, err)
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
		if m.version == 38 {
			log.Printf("migration 38: %d imported project memory records marked pending review", deactivated)
		}
		if strings.Contains(m.sql, roomLimitsMarker) {
			for _, line := range droppedRoomLimits {
				log.Print(line)
			}
			log.Printf("migration %d: %d room daily limits dropped (limits are per project now)", m.version, len(droppedRoomLimits))
		}
		if m.version == 69 {
			for _, line := range harnessFacts {
				log.Printf("migration 69: %s", line)
			}
		}
		if m.version == 102 {
			for _, line := range leaderFacts {
				log.Printf("migration 102: %s", line)
			}
		}
	}
	return nil
}

// migrateLeaderRole keeps the custom role's identity and assignments if an
// imported v1 `leader` already occupies the new global key. Foreign keys are
// deferred only for this transaction while the built-in ID and its references
// move together.
func migrateLeaderRole(ctx context.Context, tx *sql.Tx) ([]string, error) {
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
		return nil, err
	}
	var facts []string
	var customID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM role WHERE project_id IS NULL AND role_key='leader' AND deleted_at IS NULL`).Scan(&customID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if customID != "" && customID != "role_lead_default" {
		key := "leader-v1"
		for n := 2; ; n++ {
			var occupied bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM role WHERE key=? OR role_key=?)`, key, key).Scan(&occupied); err != nil {
				return nil, err
			}
			if !occupied {
				break
			}
			key = fmt.Sprintf("leader-v1-%d", n)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE role SET key=?,role_key=?,label=label || ' (v1)',rev=rev+1 WHERE id=?`, key, key, customID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE role SET extends=? WHERE extends='leader' AND project_id IS NULL`, key); err != nil {
			return nil, err
		}
		facts = append(facts, fmt.Sprintf("custom global leader role %s moved to %s; its agents and body are preserved", customID, key))
	}
	rows, err := tx.QueryContext(ctx, `SELECT project_id,id FROM role WHERE project_id IS NOT NULL AND role_key='leader' AND deleted_at IS NULL ORDER BY project_id,id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var projectID, roleID string
		if err := rows.Scan(&projectID, &roleID); err != nil {
			rows.Close()
			return nil, err
		}
		facts = append(facts, fmt.Sprintf("project %s has custom leader role %s; project role and assignments left unchanged", projectID, roleID))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE role SET id='role_leader_default',key='leader',role_key='leader',
		label=CASE WHEN label='Lead' THEN 'Leader' ELSE label END,rev=rev+1 WHERE id='role_lead_default'`); err != nil {
		return nil, err
	}
	for _, table := range []string{"agent", "project_role_spawn_default", "role_spawn_cursor", "role_spawn_warning"} {
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET role_id='role_leader_default' WHERE role_id='role_lead_default'`); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE role SET extends='leader' WHERE extends='lead' AND project_id IS NULL`); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE role SET extends='leader' WHERE extends='lead' AND project_id IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM role other WHERE other.project_id=role.project_id AND other.role_key='leader' AND other.deleted_at IS NULL)`); err != nil {
		return nil, err
	}
	return facts, nil
}

// SQLite cannot drop the old UNIQUE autoindex in place. Rebuilding checkout
// requires foreign keys off before the transaction starts because agents
// reference it; reject new foreign-key violations before committing and
// restore the connection's prior setting even when the migration fails.
func migrateCheckoutPerProject(ctx context.Context, db *sql.DB, m migration) (resultErr error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var foreignKeys int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return err
	}
	if foreignKeys != 0 {
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
			return err
		}
		defer func() {
			if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`); err != nil && resultErr == nil {
				resultErr = fmt.Errorf("restore foreign keys: %w", err)
			}
		}()
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	before, err := countForeignKeyViolations(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	after, err := countForeignKeyViolations(ctx, tx)
	if err != nil {
		return err
	}
	if after > before {
		return fmt.Errorf("checkout rebuild added %d foreign key violations", after-before)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version(version, applied_at) VALUES (?, ?)`, m.version, timestamp()); err != nil {
		return err
	}
	return tx.Commit()
}

func countForeignKeyViolations(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	return count, rows.Err()
}

// keepHistoryUnnumbered, as a context value, lets a test run migration 71 over messages (they stay unnumbered): the tests of the old
// migrations seed history at old versions and migrate it all the way up.
type keepHistoryUnnumbered struct{}

// refuseMessageNumbersOverHistory stops migration 71 on a database that already holds messages. Message numbers are assigned from
// that version on and nothing is renumbered, so a development database from before it is reset rather than converted.
func refuseMessageNumbersOverHistory(ctx context.Context, tx *sql.Tx) error {
	var messages int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM message`).Scan(&messages); err != nil {
		return err
	}
	if messages > 0 {
		return fmt.Errorf("this database already has %d messages, and message numbers (#N) are assigned from this version on without renumbering old ones: "+
			"reset the development data and start again (stop the server and run `aimebu prune -a`, or remove the config directory's server/aimebu.sqlite); "+
			"a copy of this database was saved in server/backups before this migration", messages)
	}
	return nil
}

// logProjectRoles reports, before migration 64 rewrites them, what happens to each live project role: it becomes the global role of its
// key, or it is dropped because a global role with that key exists (or another project's role of that key was converted first).
func logProjectRoles(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,project_id,role_key,
 NOT EXISTS (SELECT 1 FROM role g WHERE g.project_id IS NULL AND g.deleted_at IS NULL AND g.role_key=role.role_key)
 AND id=(SELECT MIN(q.id) FROM role q WHERE q.project_id IS NOT NULL AND q.deleted_at IS NULL AND q.role_key=role.role_key)
 FROM role WHERE project_id IS NOT NULL AND deleted_at IS NULL ORDER BY role_key,id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, project, key string
		var converted bool
		if err := rows.Scan(&id, &project, &key, &converted); err != nil {
			return err
		}
		if converted {
			log.Printf("migration 64: project role %q of project %s becomes a global role; its capabilities are not carried over, set them in Settings -> Roles", key, project)
		} else {
			log.Printf("migration 64: project role %q of project %s dropped: a global role with that key exists", key, project)
		}
	}
	return rows.Err()
}

// LatestSchemaVersion returns the newest migration version this binary applies.
// It is a read-only view of the migration list for tools that inspect a
// database without opening the store write path, such as doctor.
func LatestSchemaVersion() int {
	return migrations[len(migrations)-1].version
}

// seededSpawnModels is what the migrations seeded into the old per-tool model list (id and label), in order. A list that differs in an id,
// a label or its length was edited by the owner.
var seededSpawnModels = map[string][][2]string{
	"claude":   {{"haiku", "Haiku"}, {"sonnet", "Sonnet"}, {"opus", "Opus"}},
	"codex":    {{"gpt-6-sol", "GPT-6 Sol"}, {"gpt-6-luna", "GPT-6 Luna"}, {"gpt-6-astra", "GPT-6 Astra"}},
	"pi":       {{"ollama-cloud/gemma4:31b", "Gemma 4 31B"}},
	"opencode": {{"ollama-cloud/gemma4:31b", "Gemma 4 31B"}},
}

// harnessUnused is the SQL for a seeded launcher nobody enabled or uses (alias l), which migration 69 deletes.
const harnessUnused = `(l.preset=1 AND l.enabled=0 AND l.id NOT IN (SELECT launcher_id FROM agent WHERE launcher_id IS NOT NULL)
	AND l.id NOT IN (SELECT launcher_id FROM agent_switch WHERE launcher_id IS NOT NULL))`

// harnessMigrationFacts runs before migration 69's SQL and returns the lines to log once it has committed, so the log shows exactly what
// happened to the owner's setup. It also keeps, per tool whose launchers would all be deleted while its model list was customised, one
// disabled launcher (recorded in the temporary harness_keep table the SQL reads), so that list survives; the lists that are dropped
// anyway are written out in full.
func harnessMigrationFacts(ctx context.Context, tx *sql.Tx) ([]string, error) {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS harness_keep(id TEXT PRIMARY KEY)`); err != nil {
		return nil, err
	}
	type entry struct{ slug, label string }
	lists := map[string][]entry{}
	rows, err := tx.QueryContext(ctx, `SELECT tool,model_id,label FROM spawn_model ORDER BY tool,position`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var tool, slug, label string
		if err := rows.Scan(&tool, &slug, &label); err != nil {
			rows.Close()
			return nil, err
		}
		lists[tool] = append(lists[tool], entry{slug, label})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	kept := map[string]bool{}
	rows, err = tx.QueryContext(ctx, `SELECT DISTINCT l.tool FROM machine_launcher l WHERE NOT `+harnessUnused)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var tool string
		if err := rows.Scan(&tool); err != nil {
			rows.Close()
			return nil, err
		}
		kept[tool] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	var lines []string
	tools := make([]string, 0, len(lists))
	for tool := range lists {
		tools = append(tools, tool)
	}
	sort.Strings(tools)
	for _, tool := range tools {
		if kept[tool] {
			continue
		}
		var parts []string
		customised := len(lists[tool]) != len(seededSpawnModels[tool])
		for i, item := range lists[tool] {
			parts = append(parts, fmt.Sprintf("%d:%s=%q", i, item.slug, item.label))
			customised = customised || i >= len(seededSpawnModels[tool]) || seededSpawnModels[tool][i] != [2]string{item.slug, item.label}
		}
		var keepID string
		if customised {
			err := tx.QueryRowContext(ctx, `SELECT l.id FROM machine_launcher l JOIN machine m ON m.id=l.machine_id WHERE l.tool=? ORDER BY m.is_host DESC, m.id, l.containerized, l.name LIMIT 1`, tool).Scan(&keepID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		if keepID != "" {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO harness_keep(id) VALUES (?)`, keepID); err != nil {
				return nil, err
			}
			lines = append(lines, fmt.Sprintf("the %s model list was edited and no launcher of that tool is enabled or used: one disabled harness is kept so it survives (%s)", tool, strings.Join(parts, ", ")))
		} else {
			lines = append(lines, fmt.Sprintf("the %s model list is dropped with its launchers (position:slug=label): %s", tool, strings.Join(parts, ", ")))
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT m.name,l.tool,l.name FROM machine_launcher l JOIN machine m ON m.id=l.machine_id
		WHERE `+harnessUnused+` AND l.id NOT IN (SELECT id FROM harness_keep) ORDER BY m.name,l.tool,l.name`)
	if err != nil {
		return nil, err
	}
	var removed []string
	for rows.Next() {
		var machine, tool, name string
		if err := rows.Scan(&machine, &tool, &name); err != nil {
			rows.Close()
			return nil, err
		}
		removed = append(removed, machine+" "+tool+"/"+name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	var total, copied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM machine_launcher`).Scan(&total); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM machine_launcher l JOIN spawn_model s ON s.tool=l.tool
		WHERE NOT `+harnessUnused+` OR l.id IN (SELECT id FROM harness_keep)`).Scan(&copied); err != nil {
		return nil, err
	}
	list := "none"
	if len(removed) > 0 {
		list = strings.Join(removed, ", ")
	}
	summary := fmt.Sprintf("%d launchers kept as harnesses with %d model entries copied from the old per-tool list; %d unused seeded launchers deleted (%s)", total-len(removed), copied, len(removed), list)
	// Models agents use that the old list did not have are added to their harness, so no live agent changes model.
	rows, err = tx.QueryContext(ctx, `SELECT DISTINCT m.name,l.tool,l.name,a.model FROM (
			SELECT launcher_id, model FROM agent WHERE launcher_id IS NOT NULL AND revoked_at IS NULL AND model<>''
			UNION SELECT launcher_id, model FROM agent_switch WHERE launcher_id IS NOT NULL AND model<>'' AND state IN ('draining','starting')) a
		JOIN machine_launcher l ON l.id=a.launcher_id JOIN machine m ON m.id=l.machine_id
		WHERE NOT EXISTS (SELECT 1 FROM spawn_model s WHERE s.tool=l.tool AND s.model_id=a.model) ORDER BY m.name,l.tool,l.name,a.model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var added []string
	for rows.Next() {
		var machine, tool, name, model string
		if err := rows.Scan(&machine, &tool, &name, &model); err != nil {
			return nil, err
		}
		added = append(added, fmt.Sprintf("%s %s/%s: %s", machine, tool, name, model))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	lines = append([]string{summary}, lines...)
	if len(added) > 0 {
		lines = append(lines, "models agents already use that the old list lacked are added to their harness (not as the default): "+strings.Join(added, "; "))
	}
	return lines, nil
}

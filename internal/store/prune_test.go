package store

import (
	"context"
	"testing"
)

func TestPruneRuntimeStopsAgentsAndKeepsHistory(t *testing.T) {
	s, _ := testStore(t, Options{})
	project, room, message := fixture(t, s)
	second, err := s.CreateProject(context.Background(), Project{Name: "second", Folder: "/tmp/second"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_prune','viewer','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{project.ID, second.ID} {
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_prune','viewer')`, id); err != nil {
			t.Fatal(err)
		}
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO machine(id,name,key_hash,approved) VALUES ('machine','host','hash',1)`, nil},
		{`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('checkout',?,'machine','/tmp/test')`, []any{project.ID}},
		{`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at) VALUES ('agent','agent',?,'role_worker_default','codex','native','model','provider','machine','checkout','hash','user','now')`, []any{project.ID}},
		{`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('session','agent',0,'fresh_with_history','now')`, nil},
		{`INSERT INTO note(id,agent_id,message_id,room_id,state,wake_reason,created_at) VALUES ('note','agent',?,?,'queued','mention','now')`, []any{message.ID, room.ID}},
		{`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec) VALUES ('turn','agent','session',0,?,1,'normal','pending')`, []any{room.ID}},
		{`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,outcome,settle) VALUES ('finished','agent','session',0,?,2,'normal','exited','completed','settled')`, []any{room.ID}},
		{`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,outcome,settle) VALUES ('exited_pending','agent','session',0,?,3,'normal','exited','completed','pending')`, []any{room.ID}},
		{`INSERT INTO turn_note(turn_id,note_id) VALUES ('turn','note')`, nil},
		{`INSERT INTO usage_record(id,source_event_id,turn_id,agent_id,room_id,project_id,source,created_at) VALUES ('usage','source','turn','agent',?,?,'measured','now')`, []any{room.ID, project.ID}},
		{`INSERT INTO room_member(room_id,member_kind,member_id,joined_at) VALUES (?,'agent','agent','now')`, []any{room.ID}},
		{`INSERT INTO reaction(message_id,member_kind,member_id,emoji,created_at) VALUES (?,'agent','agent','👍','now')`, []any{message.ID}},
	}
	// The checkout's agent FK is immediate, so reserve it after inserting
	// the agent.
	for _, item := range statements {
		if _, err := s.db.Exec(item.query, item.args...); err != nil {
			t.Fatalf("fixture %s: %v", item.query, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE checkout SET writer_agent_id='agent',writer_state='reserved'`); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"agent_switch", "provider_limit", "scope_control_agent", "boundary_pending"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Errorf("%s count = %d, %v", table, count, err)
		}
	}
	for _, table := range []string{"project", "room", "message", "reaction", "machine", "checkout", "agent", "session", "note", "turn", "turn_note", "usage_record"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count == 0 {
			t.Errorf("%s count = %d, %v", table, count, err)
		}
	}
	var writer string
	if err := s.db.QueryRow(`SELECT writer_state FROM checkout WHERE id='checkout'`).Scan(&writer); err != nil || writer != "none" {
		t.Errorf("writer state = %q, %v", writer, err)
	}
	var desired, observed, keyHash string
	if err := s.db.QueryRow(`SELECT desired,observed,key_hash FROM agent WHERE id='agent'`).Scan(&desired, &observed, &keyHash); err != nil || desired != "stopped" || observed != "stopped" || keyHash != "" {
		t.Errorf("agent state = %q/%q, key=%q, %v", desired, observed, keyHash, err)
	}
	var ended string
	if err := s.db.QueryRow(`SELECT ended_at FROM session WHERE id='session'`).Scan(&ended); err != nil || ended == "" {
		t.Errorf("session ended_at = %q, %v", ended, err)
	}
	var noteState, execState string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id='note'`).Scan(&noteState); err != nil || noteState != "aborted" {
		t.Errorf("note state = %q, %v", noteState, err)
	}
	if err := s.db.QueryRow(`SELECT exec FROM turn WHERE id='turn'`).Scan(&execState); err != nil || execState != "killed" {
		t.Errorf("turn exec = %q, %v", execState, err)
	}
	var outcome, settle string
	if err := s.db.QueryRow(`SELECT outcome,settle FROM turn WHERE id='turn'`).Scan(&outcome, &settle); err != nil || outcome != "cancelled" || settle != "settled" {
		t.Errorf("cancelled turn = %q/%q, %v", outcome, settle, err)
	}
	if err := s.db.QueryRow(`SELECT exec,outcome,settle FROM turn WHERE id='finished'`).Scan(&execState, &outcome, &settle); err != nil || execState != "exited" || outcome != "completed" || settle != "settled" {
		t.Errorf("finished turn = %q/%q/%q, %v", execState, outcome, settle, err)
	}
	if err := s.db.QueryRow(`SELECT exec,outcome,settle FROM turn WHERE id='exited_pending'`).Scan(&execState, &outcome, &settle); err != nil || execState != "exited" || outcome != "completed" || settle != "settled" {
		t.Errorf("exited pending turn = %q/%q/%q, %v", execState, outcome, settle, err)
	}
	var leftAt *string
	if err := s.db.QueryRow(`SELECT left_at FROM room_member WHERE room_id=? AND member_id='agent'`, room.ID).Scan(&leftAt); err != nil || leftAt != nil {
		t.Errorf("agent membership left_at = %v, %v", leftAt, err)
	}
	var eventKind string
	if err := s.db.QueryRow(`SELECT kind FROM event ORDER BY seq DESC LIMIT 1`).Scan(&eventKind); err != nil || eventKind != "snapshot_required" {
		t.Errorf("last event = %q, %v", eventKind, err)
	}
	events, err := s.EventsAfter(context.Background(), "usr_prune", 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		if event.Kind == "snapshot_required" {
			seen[event.ProjectID] = true
		}
	}
	if len(seen) != 2 || !seen[project.ID] || !seen[second.ID] {
		t.Errorf("snapshot events visible to member = %v", seen)
	}
	var violations int
	rows, err := s.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		violations++
	}
	if err := rows.Err(); err != nil || violations != 0 {
		t.Errorf("foreign key violations = %d, %v", violations, err)
	}
}

func TestPruneRuntimeRefusesRecentRemoteMachine(t *testing.T) {
	s, _ := testStore(t, Options{})
	if _, err := s.db.Exec(`INSERT INTO machine(id,name,key_hash,approved,is_host,last_seen) VALUES ('remote','remote','hash',1,0,?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneRuntime(context.Background()); err == nil {
		t.Fatal("recent remote machine allowed prune")
	}
	if _, err := s.db.Exec(`UPDATE machine SET last_seen='2000-01-01T00:00:00.000000000Z' WHERE id='remote'`); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
}

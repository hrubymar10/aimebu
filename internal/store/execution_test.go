package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hrubymar10/aimebu/internal/sqliteutil"
)

type executionFixture struct {
	project Project
	room    Room
	message Message
	agent   Agent
	turn    Turn
	note    Note
}

func seedExecution(t *testing.T, s *Store) executionFixture {
	t.Helper()
	project, room, message := fixture(t, s)
	for _, statement := range []string{
		`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_test','worker','Worker','body','worker')`,
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_test','host','hash',1,1)`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path)
		VALUES ('chk_test',?,'mch_test',?)`, project.ID, filepath.Join(project.Folder, "checkout")); err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreateAgent(context.Background(), Agent{
		Slug: "worker", ProjectID: project.ID, RoleID: "role_test", Tool: "codex",
		Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test",
		CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at)
		VALUES ('ses_test',?,0,'fresh_with_history','now')`, agent.ID); err != nil {
		t.Fatal(err)
	}
	note, err := s.InsertNote(context.Background(), Note{AgentID: agent.ID, MessageID: message.ID,
		RoomID: room.ID, WakeReason: "human_message"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := s.RecordTurn(context.Background(), Turn{AgentID: agent.ID, SessionID: "ses_test",
		Generation: 0, RoomID: room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	return executionFixture{project, room, message, agent, turn, note}
}

func TestMigrationOneToTwoPreservesContent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "from-v1.sqlite")
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE schema_version(version INTEGER PRIMARY KEY CHECK (version > 0), applied_at TEXT NOT NULL)`,
		migrations[0].sql,
		`INSERT INTO schema_version VALUES (1,'2026-10-13T12:00:00Z')`,
		`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_old','old','','2026-10-13T12:00:00.1Z')`,
		`INSERT INTO room(id,project_id,depth,kind,title,created_by,created_at) VALUES ('room_old','prj_old',0,'home','old','usr_old','2026-10-13T12:00:00.123456Z')`,
		`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('msg_old',1,'room_old','system','system','kept','[]','2026-10-13T12:00:00.123456789Z')`,
		`UPDATE seq_counter SET value=1 WHERE id=1`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.WithValue(context.Background(), keepHistoryUnnumbered{}, true), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != migrations[len(migrations)-1].version {
		t.Fatalf("version = %d, %v", version, err)
	}
	var body string
	if err := s.db.QueryRow(`SELECT body FROM message WHERE id='msg_old'`).Scan(&body); err != nil || body != "kept" {
		t.Fatalf("message after migration = %q, %v", body, err)
	}
	for _, check := range []struct{ query, want string }{
		{`SELECT applied_at FROM schema_version WHERE version=1`, "2026-10-13T12:00:00.000000000Z"},
		{`SELECT created_at FROM project WHERE id='prj_old'`, "2026-10-13T12:00:00.100000000Z"},
		{`SELECT created_at FROM room WHERE id='room_old'`, "2026-10-13T12:00:00.123456000Z"},
		{`SELECT created_at FROM message WHERE id='msg_old'`, "2026-10-13T12:00:00.123456789Z"},
	} {
		var got string
		if err := s.db.QueryRow(check.query).Scan(&got); err != nil || got != check.want {
			t.Fatalf("normalized timestamp = %q, want %q: %v", got, check.want, err)
		}
	}
	var indexSQL string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name='message_room_seq'`).Scan(&indexSQL); err != nil {
		t.Fatalf("message(room_id, seq) index missing: %v", err)
	}
	if _, err := s.InsertMessage(context.Background(), Message{RoomID: "room_old", AuthorKind: "system", AuthorID: "system", Body: "next"}); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionSchemaConstraints(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO outbox(id,turn_id,answer_rev,idx,agent_id,room_id,body,digest,state,dedupe_key)
		VALUES ('out_test',?,0,0,?,?,'answer','digest','proposed','once')`, f.turn.ID, f.agent.ID, f.room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO receipt(id,turn_id,answer_rev,digest,result_json,created_at)
		VALUES ('rcv_test',?,0,'digest','{}','now')`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO usage_record(id,source_event_id,turn_id,agent_id,room_id,project_id,source,created_at)
		VALUES ('use_test','source_1',?,?,?,?, 'measured','now')`, f.turn.ID, f.agent.ID, f.room.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at)
		VALUES ('usr_test','test','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO turn_note VALUES (?,?)`, f.turn.ID, f.note.ID); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, query string
		args        []any
	}{
		{"machine approved", `UPDATE machine SET approved=2 WHERE id='mch_test'`, nil},
		{"checkout state", `UPDATE checkout SET writer_state='bad' WHERE id='chk_test'`, nil},
		{"checkout path unique", `INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_2',?,'mch_test',?)`, []any{f.project.ID, filepath.Join(f.project.Folder, "checkout")}},
		{"checkout machine FK", `UPDATE checkout SET machine_id='mch_missing' WHERE id='chk_test'`, nil},
		{"checkout project FK", `UPDATE checkout SET project_id='prj_missing' WHERE id='chk_test'`, nil},
		{"checkout writer FK", `UPDATE checkout SET writer_agent_id='agt_missing' WHERE id='chk_test'`, nil},
		{"agent desired", `UPDATE agent SET desired='bad' WHERE id=?`, []any{f.agent.ID}},
		{"agent observed", `UPDATE agent SET observed='bad' WHERE id=?`, []any{f.agent.ID}},
		{"agent recovery flag", `UPDATE agent SET recovery_block=2 WHERE id=?`, []any{f.agent.ID}},
		{"agent role FK", `UPDATE agent SET role_id='role_missing' WHERE id=?`, []any{f.agent.ID}},
		{"agent machine FK", `UPDATE agent SET machine_id='mch_missing' WHERE id=?`, []any{f.agent.ID}},
		{"agent checkout FK", `UPDATE agent SET checkout_id='chk_missing' WHERE id=?`, []any{f.agent.ID}},
		{"agent slug unique", `INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at) VALUES ('agt_2','worker',?,'role_test','codex','native','x','x','mch_test','chk_test','x','u','now')`, []any{f.project.ID}},
		{"session mode", `UPDATE session SET start_mode='bad' WHERE id='ses_test'`, nil},
		{"session agent FK", `UPDATE session SET agent_id='agt_missing' WHERE id='ses_test'`, nil},
		{"session generation unique", `INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_2',?,0,'fresh_with_history','now')`, []any{f.agent.ID}},
		{"note state", `UPDATE note SET state='bad' WHERE id=?`, []any{f.note.ID}},
		{"note uniqueness", `INSERT INTO note(id,agent_id,message_id,room_id,state,wake_reason,created_at) VALUES ('note_2',?,?,?,'queued','x','now')`, []any{f.agent.ID, f.message.ID, f.room.ID}},
		{"note room FK", `UPDATE note SET room_id='room_missing' WHERE id=?`, []any{f.note.ID}},
		{"note message FK", `UPDATE note SET message_id='msg_missing' WHERE id=?`, []any{f.note.ID}},
		{"note agent FK", `UPDATE note SET agent_id='agt_missing' WHERE id=?`, []any{f.note.ID}},
		{"turn mode", `UPDATE turn SET mode='bad' WHERE id=?`, []any{f.turn.ID}},
		{"turn exec", `UPDATE turn SET exec='bad' WHERE id=?`, []any{f.turn.ID}},
		{"turn outcome", `UPDATE turn SET outcome='bad' WHERE id=?`, []any{f.turn.ID}},
		{"turn settle", `UPDATE turn SET settle='bad' WHERE id=?`, []any{f.turn.ID}},
		{"turn cancel flag", `UPDATE turn SET cancel_intent=2 WHERE id=?`, []any{f.turn.ID}},
		{"turn terminal flag", `UPDATE turn SET terminal_event=2 WHERE id=?`, []any{f.turn.ID}},
		{"turn session FK", `UPDATE turn SET session_id='ses_missing' WHERE id=?`, []any{f.turn.ID}},
		{"turn room FK", `UPDATE turn SET room_id='room_missing' WHERE id=?`, []any{f.turn.ID}},
		{"turn parent FK", `UPDATE turn SET parent_turn_id='turn_missing' WHERE id=?`, []any{f.turn.ID}},
		{"turn_note FK", `INSERT INTO turn_note VALUES (?,'note_missing')`, []any{f.turn.ID}},
		{"turn_note unique", `INSERT INTO turn_note VALUES (?,?)`, []any{f.turn.ID, f.note.ID}},
		{"outbox state", `UPDATE outbox SET state='bad' WHERE id='out_test'`, nil},
		{"outbox turn FK", `UPDATE outbox SET turn_id='turn_missing' WHERE id='out_test'`, nil},
		{"outbox reply FK", `UPDATE outbox SET reply_to='msg_missing' WHERE id='out_test'`, nil},
		{"outbox held reason", `UPDATE outbox SET held_reason='bad' WHERE id='out_test'`, nil},
		{"outbox dedupe unique", `INSERT INTO outbox(id,turn_id,answer_rev,idx,agent_id,room_id,body,digest,state,dedupe_key) VALUES ('out_2',?,0,1,?,?,'x','x','proposed','once')`, []any{f.turn.ID, f.agent.ID, f.room.ID}},
		{"outbox index unique", `INSERT INTO outbox(id,turn_id,answer_rev,idx,agent_id,room_id,body,digest,state,dedupe_key) VALUES ('out_3',?,0,0,?,?,'x','x','proposed','other')`, []any{f.turn.ID, f.agent.ID, f.room.ID}},
		{"receipt unique", `INSERT INTO receipt(id,turn_id,answer_rev,digest,result_json,created_at) VALUES ('rcv_2',?,0,'d','{}','now')`, []any{f.turn.ID}},
		{"receipt turn FK", `UPDATE receipt SET turn_id='turn_missing' WHERE id='rcv_test'`, nil},
		{"request kind", `INSERT INTO request_key VALUES ('user','u','bad','key','digest','{}','now')`, nil},
		{"inbox kind", `INSERT INTO inbox_item(id,project_id,room_id,requester_kind,requester_id,kind,created_at) VALUES ('inb_1',?,?,'agent',?,'bad','now')`, []any{f.project.ID, f.room.ID, f.agent.ID}},
		{"inbox state", `INSERT INTO inbox_item(id,project_id,room_id,requester_kind,requester_id,kind,state,created_at) VALUES ('inb_2',?,?,'agent',?,'question','bad','now')`, []any{f.project.ID, f.room.ID, f.agent.ID}},
		{"inbox rev", `INSERT INTO inbox_item(id,project_id,room_id,requester_kind,requester_id,kind,rev,created_at) VALUES ('inb_3',?,?,'agent',?,'question',-1,'now')`, []any{f.project.ID, f.room.ID, f.agent.ID}},
		{"inbox room FK", `INSERT INTO inbox_item(id,project_id,room_id,requester_kind,requester_id,kind,created_at) VALUES ('inb_4',?,'room_missing','agent',?,'question','now')`, []any{f.project.ID, f.agent.ID}},
		{"usage source", `UPDATE usage_record SET source='bad' WHERE id='use_test'`, nil},
		{"usage source event unique", `INSERT INTO usage_record(id,source_event_id,turn_id,agent_id,room_id,project_id,source,created_at) VALUES ('use_2','source_1',?,?,?,?,'measured','now')`, []any{f.turn.ID, f.agent.ID, f.room.ID, f.project.ID}},
		{"usage turn FK", `UPDATE usage_record SET turn_id='turn_missing' WHERE id='use_test'`, nil},
		{"provider account machine FK", `INSERT INTO provider_account VALUES ('pa_1','codex','mch_missing','main','usr_test')`, nil},
		{"provider account owner FK", `INSERT INTO provider_account VALUES ('pa_2','codex','mch_test','main','usr_missing')`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.db.ExecContext(ctx, tc.query, tc.args...); err == nil {
				t.Fatal("invalid row accepted")
			}
		})
	}
}

func TestOneActiveTurnPerAgentUnderConcurrentInserts(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	const attempts = 24
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.RecordTurn(context.Background(), Turn{AgentID: f.agent.ID,
				SessionID: "ses_test", RoomID: f.room.ID, Attempt: 2,
				Mode: "normal", Exec: "pending"})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("%d concurrent turns were admitted, want one", successes)
	}
	var active int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM turn WHERE agent_id=? AND exec IN ('pending','running')`, f.agent.ID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active turns = %d, %v", active, err)
	}
}

func TestExecutionAccessorsRejectCrossScopeReferences(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	otherProject, err := s.CreateProject(ctx, Project{Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	otherRoom, err := s.CreateRoom(ctx, Room{ProjectID: otherProject.ID, Kind: "home", Title: "Other", CreatedBy: "user"})
	if err != nil {
		t.Fatal(err)
	}
	otherMessage, err := s.InsertMessage(ctx, Message{RoomID: otherRoom.ID, AuthorKind: "system", AuthorID: "system"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertNote(ctx, Note{AgentID: f.agent.ID, MessageID: f.message.ID, RoomID: otherRoom.ID}); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("message/room mismatch = %v", err)
	}
	if _, err := s.InsertNote(ctx, Note{AgentID: f.agent.ID, MessageID: otherMessage.ID, RoomID: otherRoom.ID}); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("agent/room mismatch = %v", err)
	}
	if _, err := s.RecordTurn(ctx, Turn{AgentID: f.agent.ID, SessionID: "ses_test", RoomID: otherRoom.ID,
		Attempt: 1, Mode: "normal", Exec: "pending"}); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("turn room mismatch = %v", err)
	}
	if _, err := s.RecordTurn(ctx, Turn{AgentID: f.agent.ID, SessionID: "ses_test", Generation: 1,
		RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "pending"}); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("turn generation mismatch = %v", err)
	}
	otherAgent, err := s.CreateAgent(ctx, Agent{Slug: "other", ProjectID: f.project.ID,
		RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test",
		MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at)
		VALUES ('ses_other',?,0,'fresh_with_history','now')`, otherAgent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordTurn(ctx, Turn{AgentID: f.agent.ID, SessionID: "ses_other", RoomID: f.room.ID,
		Attempt: 1, Mode: "normal", Exec: "pending"}); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("turn session mismatch = %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path)
		VALUES ('chk_other_project',?,'mch_test','/tmp/other-project')`, otherProject.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAgent(ctx, Agent{Slug: "wrongproject", ProjectID: f.project.ID,
		RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test",
		MachineID: "mch_test", CheckoutID: "chk_other_project", KeyHash: "hash", CreatedBy: "user"}); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("agent checkout project mismatch = %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO machine(id,name,key_hash,approved) VALUES ('mch_other','other','hash',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path)
		VALUES ('chk_other_machine',?,'mch_other','/tmp/other-machine')`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAgent(ctx, Agent{Slug: "wrongmachine", ProjectID: f.project.ID,
		RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test",
		MachineID: "mch_test", CheckoutID: "chk_other_machine", KeyHash: "hash", CreatedBy: "user"}); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("agent checkout machine mismatch = %v", err)
	}
}

func TestProposedOutboxCanBeHeldForRoomClose(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	if _, err := s.db.Exec(`INSERT INTO outbox(id,turn_id,answer_rev,idx,agent_id,room_id,body,digest,state,dedupe_key)
		VALUES ('out_closing',?,0,0,?,?,'draft','digest','proposed','close-key')`, f.turn.ID, f.agent.ID, f.room.ID); err != nil {
		t.Fatal(err)
	}
	if err := OutboxTransitions.Transition("proposed", "held"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE outbox SET state='held',held_reason='room_closed' WHERE id='out_closing'`); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	if err := s.db.QueryRow(`SELECT state,held_reason FROM outbox WHERE id='out_closing'`).Scan(&state, &reason); err != nil || state != "held" || reason != "room_closed" {
		t.Fatalf("held outbox = %q/%q, %v", state, reason, err)
	}
}

func TestPendingTurnKilledBeforeAcceptanceFreesActiveSlot(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	if err := TurnExecTransitions.Transition("pending", "killed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='killed', outcome='cancelled' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordTurn(context.Background(), Turn{AgentID: f.agent.ID,
		SessionID: "ses_test", RoomID: f.room.ID, Attempt: 2,
		Mode: "normal", Exec: "pending"}); err != nil {
		t.Fatalf("pending cancellation did not free active-turn slot: %v", err)
	}
}

func TestRequestKeyIdempotencyAndConflict(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	request := RequestKey{ActorKind: "user", ActorID: "usr_1", Kind: "spawn", Key: "key_1", Digest: "digest_1"}
	var calls atomic.Int32
	fn := func(tx *Tx) (string, error) {
		calls.Add(1)
		project, err := tx.CreateProject(Project{Name: "once"})
		return project.ID, err
	}
	const workers = 12
	var wg sync.WaitGroup
	type reply struct {
		result string
		reused bool
		err    error
	}
	results := make(chan reply, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, reused, err := s.WithRequestKey(context.Background(), request, fn)
			results <- reply{result, reused, err}
		}()
	}
	wg.Wait()
	close(results)
	first := ""
	fresh := 0
	for response := range results {
		if response.err != nil {
			t.Fatal(response.err)
		}
		result := response.result
		if first == "" {
			first = result
		}
		if result != first || len(result) < 4 || result[:4] != "prj_" {
			t.Fatalf("inconsistent idempotent result %q vs %q", result, first)
		}
		if !response.reused {
			fresh++
		}
	}
	if calls.Load() != 1 || fresh != 1 {
		t.Fatalf("callback executed %d times with %d fresh results", calls.Load(), fresh)
	}
	request.Digest = "different"
	if _, _, err := s.WithRequestKey(context.Background(), request, fn); !errors.Is(err, ErrConflict) {
		t.Fatalf("different digest = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("conflicting request executed callback")
	}
}

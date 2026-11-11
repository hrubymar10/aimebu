package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const (
	ccSessionA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	ccSessionB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

func ccSpawnFixture(t *testing.T) (*Store, Project, Room, Room) {
	t.Helper()
	s, project, task, _, _ := wakeFixture(t)
	ctx := context.Background()
	home, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "home", Title: project.Name, CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_cc','owner','hash','{}','now')`,
		`INSERT INTO membership(project_id,user_id,role) VALUES ('` + project.ID + `','usr_cc','owner')`,
		`UPDATE settings_project SET max_live_agents=50,spawn_rate_per_min=50 WHERE project_id='` + project.ID + `'`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return s, project, task, home
}

func ccSpawn(s *Store, in SpawnInput) (AgentCard, error) {
	in.ActorKind, in.ActorID, in.MachineID, in.Path = "user", "usr_cc", "mch_wake", "/tmp/wake"
	in.RoleKey, in.Tool, in.Variant, in.Model, in.Provider, in.KeyHash = "worker", "claude", "native", "haiku", "anthropic", "hash"
	var card AgentCard
	err := s.Tx(context.Background(), func(tx *Tx) (err error) {
		card, err = tx.SpawnAgent(in)
		return err
	})
	return card, err
}

func continuing(id, title string) SpawnInput {
	return SpawnInput{ResumeHandle: id, ResumeSession: &ResumeSession{ID: id, CWD: "/tmp/wake", Title: title}}
}

// A session is continued by one agent: its first turn resumes it, the room says so, and a second agent on it is refused.
func TestSpawnContinuesAClaudeSessionOnce(t *testing.T) {
	t.Parallel()
	s, _, task, _ := ccSpawnFixture(t)
	ctx := context.Background()
	in := continuing(ccSessionA, "Fix the login bug")
	in.RoomID = task.ID
	card, err := ccSpawn(s, in)
	if err != nil {
		t.Fatal(err)
	}
	var native, mode string
	var started *string
	if err := s.db.QueryRow(`SELECT native_session_id,start_mode,native_session_started_at FROM session WHERE agent_id=?`, card.ID).Scan(&native, &mode, &started); err != nil ||
		native != ccSessionA || mode != "native_resume" || started == nil {
		t.Fatalf("session=%q %q %v err=%v", native, mode, started, err)
	}
	var cwd, title, by string
	if err := s.db.QueryRow(`SELECT cwd,title,imported_by FROM imported_session WHERE session_id=? AND machine_id='mch_wake' AND agent_id=?`, ccSessionA, card.ID).Scan(&cwd, &title, &by); err != nil ||
		cwd != "/tmp/wake" || title != "Fix the login bug" || by != "usr_cc" {
		t.Fatalf("imported_session=%q %q %q err=%v", cwd, title, by, err)
	}
	messages, err := s.ListMessagesAfter(ctx, task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	said := false
	for _, m := range messages {
		said = said || strings.Contains(m.Body, `continuing the Claude Code session "Fix the login bug" (aaaaaaaa) in /tmp/wake`)
	}
	if !said {
		t.Fatalf("the room was not told: %+v", messages)
	}
	again := continuing(ccSessionA, "Fix the login bug")
	again.RoomID = task.ID
	if _, err := ccSpawn(s, again); !errors.Is(err, ErrSessionAttached) {
		t.Fatalf("a second agent on the session: %v", err)
	}
	// Both ways a session is held show up: the agent started from it, and an agent's own Claude session.
	other, err := ccSpawn(s, SpawnInput{RoomID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	var ownID string
	if err := s.db.QueryRow(`SELECT native_session_id FROM session WHERE agent_id=?`, other.ID).Scan(&ownID); err != nil || ownID == "" {
		t.Fatalf("an agent's own session id %q err=%v", ownID, err)
	}
	held, err := s.ClaudeSessionAgents(ctx, "mch_wake", []string{ccSessionA, ccSessionB, ownID})
	if err != nil || len(held) != 2 || held[ccSessionA].AgentID != card.ID || held[ownID].AgentID != other.ID || held[ccSessionB].AgentID != "" {
		t.Fatalf("held=%+v err=%v", held, err)
	}
	if none, err := s.ClaudeSessionAgents(ctx, "mch_elsewhere", []string{ccSessionA, ownID}); err != nil || len(none) != 0 {
		t.Fatalf("another machine's sessions are not these: %+v %v", none, err)
	}
	// An agent's own session cannot be taken over by starting a new agent from it either.
	steal := continuing(ownID, "taken")
	steal.RoomID = task.ID
	if _, err := ccSpawn(s, steal); !errors.Is(err, ErrSessionAttached) {
		t.Fatalf("starting from an agent's own session: %v", err)
	}
}

// An agent that was revoked no longer touches its session, so the session can be continued again.
func TestSpawnContinuesASessionOfARevokedAgent(t *testing.T) {
	t.Parallel()
	s, _, task, _ := ccSpawnFixture(t)
	in := continuing(ccSessionA, "First")
	in.RoomID = task.ID
	first, err := ccSpawn(s, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET revoked_at=? WHERE id=?`, timestamp(), first.ID); err != nil {
		t.Fatal(err)
	}
	again := continuing(ccSessionA, "Second")
	again.RoomID = task.ID
	second, err := ccSpawn(s, again)
	if err != nil {
		t.Fatalf("a revoked agent's session: %v", err)
	}
	var agent, title string
	if err := s.db.QueryRow(`SELECT agent_id,title FROM imported_session WHERE session_id=?`, ccSessionA).Scan(&agent, &title); err != nil || agent != second.ID || title != "Second" {
		t.Fatalf("imported_session=%q %q err=%v", agent, title, err)
	}
}

// With a room title the agent starts in a task room of its own under the project's home room; a spawn that fails leaves no room behind.
func TestSpawnContinuesASessionInARoomOfItsOwn(t *testing.T) {
	t.Parallel()
	s, project, task, home := ccSpawnFixture(t)
	in := continuing(ccSessionA, "Fix the login bug")
	in.RoomID, in.NewRoomTitle = task.ID, "Fix the login bug"
	card, err := ccSpawn(s, in)
	if err != nil {
		t.Fatal(err)
	}
	if card.RoomID == task.ID || card.RoomID == "" {
		t.Fatalf("card room=%q", card.RoomID)
	}
	var kind, title, parent string
	if err := s.db.QueryRow(`SELECT kind,title,parent_id FROM room WHERE id=?`, card.RoomID).Scan(&kind, &title, &parent); err != nil ||
		kind != "task" || title != "Fix the login bug" || parent != home.ID {
		t.Fatalf("room=%q %q %q err=%v", kind, title, parent, err)
	}
	var member int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, card.RoomID, card.ID).Scan(&member); err != nil || member != 1 {
		t.Fatalf("members=%d err=%v", member, err)
	}
	// The same session again fails after the room was made inside the transaction: no second room is left.
	again := continuing(ccSessionA, "Fix the login bug")
	again.RoomID, again.NewRoomTitle = task.ID, "Retry room"
	if _, err := ccSpawn(s, again); !errors.Is(err, ErrSessionAttached) {
		t.Fatalf("the same session again: %v", err)
	}
	var rooms int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room WHERE project_id=? AND title='Retry room'`, project.ID).Scan(&rooms); err != nil || rooms != 0 {
		t.Fatalf("a failed spawn left %d rooms (err=%v)", rooms, err)
	}
}

func forking(id, title string) SpawnInput {
	in := continuing(id, title)
	in.ResumeSession.Fork = true
	return in
}

// A copy does not hold its original: any number of agents can work on copies of one session, the original can still be continued once, and
// until a copy's first turn names it the agent's own session id (still the original's) is not counted as a holder.
func TestForkedSessionsLeaveTheOriginalFree(t *testing.T) {
	t.Parallel()
	s, _, task, _ := ccSpawnFixture(t)
	ctx := context.Background()
	var forks []AgentCard
	for i := 0; i < 2; i++ {
		in := forking(ccSessionA, "Fix the login bug")
		in.RoomID = task.ID
		card, err := ccSpawn(s, in)
		if err != nil {
			t.Fatalf("fork %d: %v", i, err)
		}
		forks = append(forks, card)
		var native, forkOf, mode string
		if err := s.db.QueryRow(`SELECT se.native_session_id,se.fork_of,i.mode FROM session se JOIN imported_session i ON i.agent_id=se.agent_id WHERE se.agent_id=?`, card.ID).
			Scan(&native, &forkOf, &mode); err != nil || native != ccSessionA || forkOf != ccSessionA || mode != "fork" {
			t.Fatalf("fork session=%q fork_of=%q mode=%q err=%v", native, forkOf, mode, err)
		}
		sessionID := mustSession(t, s, card.ID)
		err = s.Tx(ctx, func(tx *Tx) error {
			handle, started, forking, err := sessionNativeFork(tx, sessionID)
			if handle != ccSessionA || !started || !forking {
				t.Fatalf("the first turn of a copy resumes the original and forks it: %q %v %v", handle, started, forking)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if held, err := s.ClaudeSessionAgents(ctx, "mch_wake", []string{ccSessionA}); err != nil || len(held) != 0 {
		t.Fatalf("a copy holds its original: %+v err=%v", held, err)
	}
	in := continuing(ccSessionA, "Original")
	in.RoomID = task.ID
	continued, err := ccSpawn(s, in)
	if err != nil {
		t.Fatalf("continuing the original beside its copies: %v", err)
	}
	if held, err := s.ClaudeSessionAgents(ctx, "mch_wake", []string{ccSessionA}); err != nil || held[ccSessionA].AgentID != continued.ID {
		t.Fatalf("held=%+v err=%v", held, err)
	}
	again := continuing(ccSessionA, "Original")
	again.RoomID = task.ID
	if _, err := ccSpawn(s, again); !errors.Is(err, ErrSessionAttached) {
		t.Fatalf("a second agent continuing the original: %v", err)
	}
	profile, err := s.AgentProfile(ctx, task.ID, forks[0].ID)
	if err != nil || profile.NativeSessionID != "" || profile.ResumeCommand != "" {
		t.Fatalf("a copy that is not named yet has no session of its own to resume: %+v err=%v", profile, err)
	}
}

func TestPendingClaudeForkSurvivesStopAndStart(t *testing.T) {
	s, _, task, _ := ccSpawnFixture(t)
	in := forking(ccSessionA, "Fix the login bug")
	in.RoomID = task.ID
	card, err := ccSpawn(s, in)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var stopped, restarted AgentCard
	if err := s.Tx(ctx, func(tx *Tx) error {
		stopped, err = tx.SetAgentDesired(card.ID, "usr_cc", "stopped", card.DesiredRev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='stopped' WHERE id=?`, card.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		restarted, err = tx.SetAgentDesired(card.ID, "usr_cc", "active", stopped.DesiredRev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.AgentForkPending(ctx, card.ID)
	if err != nil || !pending {
		t.Fatalf("fork hold after restart: pending=%v err=%v", pending, err)
	}
	var native, forkOf string
	if err := s.db.QueryRow(`SELECT native_session_id,fork_of FROM session WHERE agent_id=? AND generation=?`, card.ID, restarted.Generation).Scan(&native, &forkOf); err != nil || native != ccSessionA || forkOf != ccSessionA {
		t.Fatalf("restarted fork: native=%q fork_of=%q err=%v", native, forkOf, err)
	}
	message, err := s.InsertMessage(ctx, Message{RoomID: task.ID, AuthorKind: "system", AuthorID: "system", Body: "Continue"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO note(id,agent_id,message_id,room_id,state,wake_reason,created_at) VALUES (?,?,?,?,'queued','mention',?)`, "note_fork_restart", card.ID, message.ID, task.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, card.ID, "")
	if err != nil || !ready || delivery.Generation != restarted.Generation || !delivery.ForkSession || delivery.NativeSessionHandle != ccSessionA {
		t.Fatalf("restarted delivery: %+v ready=%v err=%v", delivery, ready, err)
	}
}

func mustSession(t *testing.T, s *Store, agentID string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM session WHERE agent_id=? ORDER BY generation DESC LIMIT 1`, agentID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// The first turn of a copy reports the copy's id: once, replayable, and it ends the pending state (the next turn resumes the copy).
func TestForkedSessionNamesItsCopyOnce(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	for _, statement := range []string{
		`UPDATE agent SET tool='claude' WHERE id='` + f.agent.ID + `'`,
		`UPDATE session SET native_session_id='` + ccSessionA + `',native_session_started_at='now',fork_of='` + ccSessionA + `' WHERE id='` + f.turn.SessionID + `'`,
		`INSERT INTO imported_session(agent_id,machine_id,session_id,mode,cwd,imported_by,imported_at) VALUES ('` + f.agent.ID + `','mch_test','` + ccSessionA + `','fork','/x','u','now')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	const copyID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	// Reporting the original again changes nothing.
	if err := s.RecordNativeSessionStarted(ctx, f.turn.ID, f.agent.ID, 0, "source-0", ccSessionA); err != nil {
		t.Fatalf("the original: %v", err)
	}
	if err := s.RecordNativeSessionStarted(ctx, f.turn.ID, f.agent.ID, 0, "source-1", copyID); err != nil {
		t.Fatalf("the copy: %v", err)
	}
	if err := s.RecordNativeSessionStarted(ctx, f.turn.ID, f.agent.ID, 0, "source-1", copyID); err != nil {
		t.Fatalf("journal replay: %v", err)
	}
	if err := s.RecordNativeSessionStarted(ctx, f.turn.ID, f.agent.ID, 0, "source-2", "dddddddd-dddd-4ddd-8ddd-dddddddddddd"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second copy id: %v", err)
	}
	var native, original, forked string
	if err := s.db.QueryRow(`SELECT se.native_session_id,i.session_id,COALESCE(i.forked_session_id,'') FROM session se JOIN imported_session i ON i.agent_id=se.agent_id WHERE se.id=?`, f.turn.SessionID).
		Scan(&native, &original, &forked); err != nil || native != copyID || original != ccSessionA || forked != copyID {
		t.Fatalf("session=%q original=%q copy=%q err=%v", native, original, forked, err)
	}
	err := s.Tx(ctx, func(tx *Tx) error {
		handle, started, forking, err := sessionNativeFork(tx, f.turn.SessionID)
		if handle != copyID || !started || forking {
			t.Fatalf("after the copy is named the session is an ordinary one: %q %v %v", handle, started, forking)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// The copy is now the agent's own Claude session, so it is held like any other; the original is free.
	if held, err := s.ClaudeSessionAgents(ctx, "mch_test", []string{ccSessionA, copyID}); err != nil || len(held) != 1 || held[copyID].AgentID != f.agent.ID {
		t.Fatalf("held=%+v err=%v", held, err)
	}
}

// Migration 86 rekeys imported_session by agent (copies of one session are many) and keeps what exists as continued sessions, still held.
func TestMigrationEightySixKeepsImportedSessionsAsContinued(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "from-85.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := migrateUpTo(context.Background(), db, 85); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil { // the rows name an agent that does not exist
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO imported_session(machine_id,session_id,agent_id,cwd,title,imported_by,imported_at) VALUES ('mch','` + ccSessionA + `','agt_one','/p','Fix it','usr','2026-10-24T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := migrateUpTo(context.Background(), db, 0); err != nil {
		t.Fatal(err)
	}
	var mode, cwd, title string
	var forked sql.NullString
	if err := db.QueryRow(`SELECT mode,cwd,title,forked_session_id FROM imported_session WHERE agent_id='agt_one' AND session_id=?`, ccSessionA).Scan(&mode, &cwd, &title, &forked); err != nil ||
		mode != "continue" || cwd != "/p" || title != "Fix it" || forked.Valid {
		t.Fatalf("row: %q %q %q %v err=%v", mode, cwd, title, forked, err)
	}
	// Two copies of one original are fine; a second continued row for it is not.
	insert := func(agent, mode string) error {
		_, err := db.Exec(`INSERT INTO imported_session(agent_id,machine_id,session_id,mode,cwd,imported_by,imported_at) VALUES (?,'mch',?,?,'/p','usr','now')`, agent, ccSessionB, mode)
		return err
	}
	if err := insert("agt_a", "fork"); err != nil {
		t.Fatal(err)
	}
	if err := insert("agt_b", "fork"); err != nil {
		t.Fatalf("a second copy: %v", err)
	}
	if err := insert("agt_c", "continue"); err != nil {
		t.Fatalf("a first continued row: %v", err)
	}
	if err := insert("agt_d", "continue"); err == nil {
		t.Fatal("a second agent continuing one session was accepted")
	}
	if err := insert("agt_e", "mirror"); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
}

package store

import (
	"context"
	"errors"
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

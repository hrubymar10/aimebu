package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// turnRoomReadFixture is a wake fixture with a restricted room the agent sits in, a message in it, and two people of the project: its
// owner, who reads restricted rooms, and a viewer, who does not.
func turnRoomReadFixture(t *testing.T) (*Store, Room, Room, Agent, Message) {
	t.Helper()
	s, project, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	restricted, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, ParentID: room.ID, Kind: "task", Title: "routing", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE room SET restricted=1 WHERE id='` + restricted.ID + `'`,
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_rr_owner','rrowner','hash','{}','now'),('usr_rr_viewer','rrviewer','hash','{}','now')`,
		`INSERT INTO membership(project_id,user_id,role) VALUES ('` + project.ID + `','usr_rr_owner','owner'),('` + project.ID + `','usr_rr_viewer','viewer')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddMember(ctx, restricted.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	secret, err := s.InsertMessage(ctx, Message{RoomID: restricted.ID, AuthorKind: "system", AuthorID: "system", Body: "a routing secret"})
	if err != nil {
		t.Fatal(err)
	}
	// Only the note queued below is to be delivered.
	if _, err := s.db.Exec(`DELETE FROM note WHERE agent_id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	return s, room, restricted, agent, secret
}

func queueTurnNote(t *testing.T, s *Store, room Room, agent Agent, body string) {
	t.Helper()
	message, err := s.InsertMessage(context.Background(), Message{RoomID: room.ID, AuthorKind: "system", AuthorID: "system", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO note(id,agent_id,message_id,room_id,state,wake_reason,created_at) VALUES (?,?,?,?,'queued','mention',?)`,
		"note_rr_"+fmt.Sprint(message.No), agent.ID, message.ID, room.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
}

func turnReadRooms(t *testing.T, s *Store, turnID string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT room_id FROM turn_room_read WHERE turn_id=? ORDER BY room_id`, turnID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var rooms []string
	for rows.Next() {
		var room string
		if err := rows.Scan(&room); err != nil {
			t.Fatal(err)
		}
		rooms = append(rooms, room)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return rooms
}

// assertRestrictedFor checks that the turn is withheld from the project's viewer as a restricted-room turn and shown to its owner.
func assertRestrictedFor(t *testing.T, s *Store, turnID string, agent Agent) {
	t.Helper()
	ctx := context.Background()
	if known, private, restricted, err := s.TurnWithheldFrom(ctx, turnID, agent.ID, "usr_rr_viewer"); err != nil || !known || private || !restricted {
		t.Fatalf("the viewer: known=%t private=%t restricted=%t %v", known, private, restricted, err)
	}
	if known, private, restricted, err := s.TurnWithheldFrom(ctx, turnID, agent.ID, "usr_rr_owner"); err != nil || !known || private || restricted {
		t.Fatalf("the owner: known=%t private=%t restricted=%t %v", known, private, restricted, err)
	}
}

// A #N reference in a turn's notes puts the cited message into the turn's context, which the conversation log records with the prompt:
// the cited message's room counts among the rooms the turn served.
func TestTurnContextReferencesRecordTheirRooms(t *testing.T) {
	t.Parallel()
	s, room, restricted, agent, secret := turnRoomReadFixture(t)
	// A native session that has started gets no recent history, so only the reference brings the restricted room in.
	if _, err := s.db.Exec(`UPDATE session SET native_session_id='aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa',native_session_started_at=? WHERE agent_id=?`, timestamp(), agent.ID); err != nil {
		t.Fatal(err)
	}
	queueTurnNote(t, s, room, agent, "see #"+fmt.Sprint(secret.No))
	delivery, ready, err := s.PrepareDispatch(context.Background(), agent.ID, "")
	if err != nil || !ready {
		t.Fatalf("dispatch: ready=%t %v", ready, err)
	}
	if !strings.Contains(string(delivery.Context), "a routing secret") || strings.Contains(string(delivery.Context), "recent_room_history") {
		t.Fatalf("the context does not quote only the reference:\n%s", delivery.Context)
	}
	if got := turnReadRooms(t, s, delivery.TurnID); len(got) != 1 || got[0] != restricted.ID {
		t.Fatalf("rooms read by the turn: %v, want %s", got, restricted.ID)
	}
	assertRestrictedFor(t, s, delivery.TurnID, agent)
}

// A new native session's context quotes recent history of every room the agent is in: those rooms count among the rooms the turn served.
func TestTurnContextRecentHistoryRecordsItsRooms(t *testing.T) {
	t.Parallel()
	s, room, restricted, agent, _ := turnRoomReadFixture(t)
	queueTurnNote(t, s, room, agent, "a plain request")
	delivery, ready, err := s.PrepareDispatch(context.Background(), agent.ID, "")
	if err != nil || !ready {
		t.Fatalf("dispatch: ready=%t %v", ready, err)
	}
	if !strings.Contains(string(delivery.Context), "recent_room_history") || !strings.Contains(string(delivery.Context), "a routing secret") {
		t.Fatalf("the context carries no history of the restricted room:\n%s", delivery.Context)
	}
	if got := turnReadRooms(t, s, delivery.TurnID); !strings.Contains(strings.Join(got, ","), restricted.ID) {
		t.Fatalf("rooms read by the turn: %v, want %s among them", got, restricted.ID)
	}
	assertRestrictedFor(t, s, delivery.TurnID, agent)
}

// A toolbox read is recorded against the agent's active turn, and against its most recently started turn when none is active (the turn
// the log's records then fall under); an agent with no turn records nothing; a turn that read only ordinary rooms is not withheld.
func TestRecordAgentTurnRoomReads(t *testing.T) {
	t.Parallel()
	s, room, restricted, agent, _ := turnRoomReadFixture(t)
	ctx := context.Background()
	if err := s.RecordAgentTurnRoomReads(ctx, agent.ID, restricted.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM turn_room_read`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("an agent with no turn recorded %d reads: %v", count, err)
	}
	insertTurn := func(id, exec, started string) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,started_at) VALUES (?,?,'ses_one',0,?,1,'normal',?,NULLIF(?,''))`,
			id, agent.ID, room.ID, exec, started); err != nil {
			t.Fatal(err)
		}
	}
	insertTurn("trn_rr_old", "exited", "2026-01-01T00:00:01.000000000Z")
	insertTurn("trn_rr_last", "exited", "2026-01-01T00:00:02.000000000Z")
	insertTurn("trn_rr_plain", "running", "2026-01-01T00:00:03.000000000Z")
	// A read of the turn's own, ordinary room: recorded, and the turn stays open to the viewer.
	if err := s.RecordAgentTurnRoomReads(ctx, agent.ID, room.ID, room.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := turnReadRooms(t, s, "trn_rr_plain"); len(got) != 1 || got[0] != room.ID {
		t.Fatalf("the active turn's reads: %v", got)
	}
	if known, private, restricted, err := s.TurnWithheldFrom(ctx, "trn_rr_plain", agent.ID, "usr_rr_viewer"); err != nil || !known || private || restricted {
		t.Fatalf("a turn that read only an ordinary room: known=%t private=%t restricted=%t %v", known, private, restricted, err)
	}
	if err := s.RecordAgentTurnRoomReads(ctx, agent.ID, restricted.ID); err != nil {
		t.Fatal(err)
	}
	assertRestrictedFor(t, s, "trn_rr_plain", agent)
	if got := turnReadRooms(t, s, "trn_rr_last"); len(got) != 0 {
		t.Fatalf("an ended turn got the active turn's reads: %v", got)
	}
	// With no active turn the read falls on the most recently started one.
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',started_at='2026-01-01T00:00:00.500000000Z' WHERE id='trn_rr_plain'`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAgentTurnRoomReads(ctx, agent.ID, restricted.ID); err != nil {
		t.Fatal(err)
	}
	assertRestrictedFor(t, s, "trn_rr_last", agent)
	if got := turnReadRooms(t, s, "trn_rr_old"); len(got) != 0 {
		t.Fatalf("an older turn got the read: %v", got)
	}
}

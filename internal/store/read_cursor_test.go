package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func agentReceipts(t *testing.T, s *Store, roomID string) map[string]int64 {
	t.Helper()
	receipts, err := s.RoomReadReceipts(context.Background(), roomID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int64{}
	for _, r := range receipts {
		if r.MemberKind == "agent" {
			seen[r.MemberID] = r.Seq
		}
	}
	return seen
}

// Two agents say hello one after the other, neither addressing the other. The second hello is not for the first agent, so the router
// hands it over as nothing to do; like an unaddressed message a waiting v1 agent received, it counts as seen at once, and both markers
// end under the second hello.
func TestAgentMarkerFollowsMessagesDeliveredAsNothingToDo(t *testing.T) {
	t.Parallel()
	s, _, room, owen, alice := wakeFixture(t)
	ctx := context.Background()
	first, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: owen.ID, Body: "HI"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: alice.ID, Body: "HI"})
	if err != nil {
		t.Fatal(err)
	}
	seen := agentReceipts(t, s, room.ID)
	if seen[owen.ID] != second.Seq || seen[alice.ID] != second.Seq {
		t.Fatalf("markers = %v, want both at the second hello %d (the first is %d)", seen, second.Seq, first.Seq)
	}
}

// A message meant for an agent waits in a queued note until a turn takes it: the marker stays where it was, and moves once a turn has it.
func TestAgentMarkerSkipsQueuedNotes(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	own, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	asked, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one handle this"})
	if err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE agent_id=? AND message_id=?`, one.ID, asked.ID).Scan(&state); err != nil || state != "queued" {
		t.Fatalf("note state = %q, %v; want queued", state, err)
	}
	if got := agentReceipts(t, s, room.ID)[one.ID]; got != own.Seq {
		t.Fatalf("marker = %d, want it to stay at its own message %d while the note is queued", got, own.Seq)
	}
	if _, err := s.db.Exec(`UPDATE note SET state='in_turn' WHERE agent_id=? AND message_id=?`, one.ID, asked.ID); err != nil {
		t.Fatal(err)
	}
	if got := agentReceipts(t, s, room.ID)[one.ID]; got != asked.Seq {
		t.Fatalf("marker = %d, want %d once a turn took the note", got, asked.Seq)
	}
}

// Chatter after a queued note must not carry the marker past it: the note is still waiting, so the message it carries is unread however many
// messages for nobody came after it. The marker is capped just below the agent's oldest queued note, also when the stored position is past it.
func TestAgentMarkerStaysBelowAQueuedNoteWhenChatterFollows(t *testing.T) {
	t.Parallel()
	s, _, room, one, two := wakeFixture(t)
	ctx := context.Background()
	own, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	asked, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one handle this"})
	if err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE agent_id=? AND message_id=?`, one.ID, asked.ID).Scan(&state); err != nil || state != "queued" {
		t.Fatalf("note state = %q, %v; want queued", state, err)
	}
	var last Message
	for i := 0; i < 3; i++ {
		if last, err = s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: two.ID, Body: fmt.Sprintf("chatter %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	var stored int64
	if err := s.db.QueryRow(`SELECT seq FROM agent_room_seen WHERE room_id=? AND agent_id=?`, room.ID, one.ID).Scan(&stored); err != nil || stored < last.Seq {
		t.Fatalf("stored seen position = %d, %v; the router recorded the chatter up to %d", stored, err, last.Seq)
	}
	if got := agentReceipts(t, s, room.ID)[one.ID]; got != asked.Seq-1 || got < own.Seq {
		t.Fatalf("marker = %d, want %d: just below the queued note at %d, not at the chatter (%d)", got, asked.Seq-1, asked.Seq, last.Seq)
	}
	// A dispatched note holds it as well; once a turn has the note the marker goes on to the newest.
	if _, err := s.db.Exec(`UPDATE note SET state='dispatched' WHERE agent_id=? AND message_id=?`, one.ID, asked.ID); err != nil {
		t.Fatal(err)
	}
	if got := agentReceipts(t, s, room.ID)[one.ID]; got >= asked.Seq {
		t.Fatalf("marker = %d with the note dispatched, want it below %d", got, asked.Seq)
	}
	if _, err := s.db.Exec(`UPDATE note SET state='in_turn' WHERE agent_id=? AND message_id=?`, one.ID, asked.ID); err != nil {
		t.Fatal(err)
	}
	if got := agentReceipts(t, s, room.ID)[one.ID]; got != last.Seq {
		t.Fatalf("marker = %d once a turn took the note, want the newest message %d", got, last.Seq)
	}
}

// A stopped agent reads nothing: messages posted after it stopped do not move its marker, and the position it had when it stopped is kept.
func TestStoppedAgentMarkerStaysWhereItStopped(t *testing.T) {
	t.Parallel()
	s, _, room, one, two := wakeFixture(t)
	ctx := context.Background()
	own, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: one.ID, Body: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	last, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: two.ID, Body: "anyone there"})
	if err != nil {
		t.Fatal(err)
	}
	seen := agentReceipts(t, s, room.ID)
	if seen[one.ID] != own.Seq {
		t.Fatalf("stopped agent's marker = %d, want it kept at %d (the newest message is %d)", seen[one.ID], own.Seq, last.Seq)
	}
	if seen[two.ID] != last.Seq {
		t.Fatalf("a running agent's marker = %d, want %d", seen[two.ID], last.Seq)
	}
}

// The receipts are read on every room open, and event is the biggest table: the markers never read it. Each arm is a lookup by this room on
// an index, and the delivered-as-nothing-to-do arm is a primary-key range of agent_room_seen. This plan is the guard on the read's cost, which
// therefore does not grow with the room's events; a wall-clock bound would only measure how loaded the machine running the tests is.
func TestRoomReadReceiptsReadNoEvents(t *testing.T) {
	t.Parallel()
	s, _, room, _, _ := wakeFixture(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+roomReadReceiptsSQL, room.ID, room.ID, room.ID, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("plan:\n%s", strings.Join(plan, "\n"))
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "SEARCH s USING PRIMARY KEY (room_id=?)") || strings.Contains(joined, " event") || strings.Contains(joined, "SCAN s") {
		t.Fatalf("agent_room_seen must be read by its key for this room, and event not at all:\n%s", joined)
	}
}

// Migration 96 fills agent_room_seen from the routing events already recorded: the newest message each agent was handed with nothing to do,
// per room, and nothing for a skip that was meant for the agent (here a revoked one).
func TestMigrationNinetySixBackfillsSeenFromEvents(t *testing.T) {
	t.Parallel()
	s, project, room, owen, alice := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: owen.ID, Body: "HI"}); err != nil {
		t.Fatal(err)
	}
	hi, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: alice.ID, Body: "HI"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "other", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, other.ID, "agent", owen.ID); err != nil {
		t.Fatal(err)
	}
	// A skip that was meant for owen (it was revoked at the time) is recorded, and must not count.
	blocked, err := s.InsertMessage(ctx, Message{RoomID: other.ID, AuthorKind: "user", AuthorID: "owner", Body: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM agent_room_seen WHERE room_id=?`, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO event(id,seq,kind,project_id,room_id,agent_id,payload_json,created_at) VALUES ('evt_revoked',(SELECT value+1 FROM seq_counter),'message_not_woken',?,?,?,?,?)`,
		project.ID, other.ID, owen.ID, fmt.Sprintf(`{"message_id":%q,"reason":"revoked"}`, blocked.ID), timestamp()); err != nil {
		t.Fatal(err)
	}
	before := agentReceipts(t, s, room.ID)
	// Back to version 95, as a database from before the table, then migrate again.
	for _, statement := range []string{`DROP TABLE agent_room_seen`, `DROP INDEX message_reply_to`, `DELETE FROM schema_version WHERE version>=96`} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateUpTo(ctx, s.db, 0); err != nil {
		t.Fatal(err)
	}
	if after := agentReceipts(t, s, room.ID); after[owen.ID] != hi.Seq || after[alice.ID] != hi.Seq || after[owen.ID] != before[owen.ID] {
		t.Fatalf("after the backfill markers = %v, before = %v, want both at %d", after, before, hi.Seq)
	}
	// In the other room owen was handed its own "joined" line as system chatter, which counts; the blocked message must not.
	var seen int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM agent_room_seen WHERE room_id=? AND agent_id=?`, other.ID, owen.ID).Scan(&seen); err != nil || seen >= blocked.Seq {
		t.Fatalf("a skip meant for the agent was backfilled: seen %d, blocked message %d, %v", seen, blocked.Seq, err)
	}
}

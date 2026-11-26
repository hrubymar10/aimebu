package store

import (
	"context"
	"fmt"
	"testing"
)

// A13-17 regression: a stopped agent ran a turn in a closing room. The wake
// router must not queue an unaddressed note for a stopped agent, and a closing
// or closed room must queue no note at all; every skip is a message_not_woken
// event. A note addressed to a stopped agent (a mention) stays queued in an
// open room for when it is started again, and dispatch still refuses a stopped
// agent either way.
func TestStoppedAgentGetsNoUnaddressedNoteAndAClosingRoomWakesNobody(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_wake','owner','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES ((SELECT project_id FROM room WHERE id=?),'usr_wake','owner')`, room.ID); err != nil {
		t.Fatal(err)
	}
	// The agent is in a turn when it is stopped, like the incident's worker.
	if _, err := s.RecordTurn(ctx, Turn{AgentID: one.ID, SessionID: "ses_one", Generation: 0, RoomID: room.ID, Attempt: 1, Mode: "normal", Exec: "running"}); err != nil {
		t.Fatal(err)
	}
	var card AgentCard
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		card, err = tx.SetAgentDesired(one.ID, "usr_wake", "stopped", 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if card.Desired != "stopped" {
		t.Fatalf("stop card=%+v", card)
	}
	// The machine reports the cancelled turn's exit; the stop is then confirmed.
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited' WHERE id=(SELECT MAX(id) FROM turn WHERE agent_id=?)`, one.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmAgentProcessExit(ctx, one.ID); err != nil {
		t.Fatal(err)
	}
	var observed string
	if err := s.db.QueryRow(`SELECT observed FROM agent WHERE id=?`, one.ID).Scan(&observed); err != nil {
		t.Fatal(err)
	}
	if observed != "stopped" {
		t.Fatalf("observed=%q after the exit", observed)
	}

	// A person's unaddressed message in the still-open room must not queue a
	// note for the stopped agent.
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "usr_wake", Body: "try this zip"}); err != nil {
		t.Fatal(err)
	}
	if err := noQueuedNote(t, s, one.ID, lastRoomMessageID(t, s, room.ID), "open room, unaddressed, stopped agent"); err != nil {
		t.Fatal(err)
	}
	if err := notWokenEvent(t, s, room.ID, one.ID, lastRoomMessageID(t, s, room.ID), "agent_stopped"); err != nil {
		t.Fatal(err)
	}

	// A mention still queues: it stays for when the agent is started again.
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "usr_wake", Body: "@" + one.ID + " when you are back"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NoteForAgentMessage(ctx, one.ID, lastRoomMessageID(t, s, room.ID)); err != nil {
		t.Fatalf("addressed note not queued for a stopped agent: %v", err)
	}

	// Closing the room: nobody gets a note from it any more, addressed or not.
	if _, _, err := s.BeginRoomClose(ctx, room.ID, "usr_wake", 0, false); err != nil {
		t.Fatal(err)
	}
	var lifecycle string
	if err := s.db.QueryRow(`SELECT lifecycle FROM room WHERE id=?`, room.ID).Scan(&lifecycle); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "closing" {
		t.Fatalf("lifecycle=%q", lifecycle)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "usr_wake", Body: "one more thing"}); err != nil {
		t.Fatal(err)
	}
	if err := noQueuedNote(t, s, one.ID, lastRoomMessageID(t, s, room.ID), "closing room, unaddressed"); err != nil {
		t.Fatal(err)
	}
	if err := notWokenEvent(t, s, room.ID, one.ID, lastRoomMessageID(t, s, room.ID), "room_closing"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "usr_wake", Body: "@" + one.ID + " one more thing"}); err != nil {
		t.Fatal(err)
	}
	if err := noQueuedNote(t, s, one.ID, lastRoomMessageID(t, s, room.ID), "closing room, mentioned"); err != nil {
		t.Fatal(err)
	}
	if err := notWokenEvent(t, s, room.ID, one.ID, lastRoomMessageID(t, s, room.ID), "room_closing"); err != nil {
		t.Fatal(err)
	}

	// Fully closed: the same silence, with its own reason. (The fixture's lead
	// agent is stopped by the close too; that is its own, ordinary card.)
	controls, _, err := s.AdvanceRoomClosures(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, card := range controls {
		if card.ID == one.ID {
			t.Fatalf("already-stopped agent got a stop control again: %+v", card)
		}
	}
	if err := s.db.QueryRow(`SELECT lifecycle FROM room WHERE id=?`, room.ID).Scan(&lifecycle); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "closed" {
		t.Fatalf("lifecycle=%q", lifecycle)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "usr_wake", Body: "@" + one.ID + " and one last thing"}); err != nil {
		t.Fatal(err)
	}
	if err := noQueuedNote(t, s, one.ID, lastRoomMessageID(t, s, room.ID), "closed room, mentioned"); err != nil {
		t.Fatal(err)
	}
	if err := notWokenEvent(t, s, room.ID, one.ID, lastRoomMessageID(t, s, room.ID), "room_closed"); err != nil {
		t.Fatal(err)
	}

	// The stop guarantee itself: dispatch never prepares a turn for the
	// stopped agent, whatever is queued.
	if _, ready, err := s.PrepareDispatch(ctx, one.ID, ""); err != nil || ready {
		t.Fatalf("stopped agent dispatchable: ready=%t %v", ready, err)
	}
}

func noQueuedNote(t *testing.T, s *Store, agentID, messageID, stage string) error {
	t.Helper()
	var queued int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND message_id=? AND state='queued'`, agentID, messageID).Scan(&queued); err != nil {
		return err
	}
	if queued != 0 {
		return fmt.Errorf("%s: %d queued note(s) for the stopped agent", stage, queued)
	}
	return nil
}

func notWokenEvent(t *testing.T, s *Store, roomID, agentID, messageID, reason string) error {
	t.Helper()
	var found int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='message_not_woken' AND room_id=? AND agent_id=?
		AND json_extract(payload_json,'$.message_id')=? AND json_extract(payload_json,'$.reason')=?`, roomID, agentID, messageID, reason).Scan(&found); err != nil {
		return err
	}
	if found == 0 {
		return fmt.Errorf("no message_not_woken event with reason %s", reason)
	}
	return nil
}

func lastRoomMessageID(t *testing.T, s *Store, roomID string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM message WHERE room_id=? ORDER BY seq DESC LIMIT 1`, roomID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A person's plain message in their own DM with an agent is addressed to that agent, so a stopped agent keeps
// the note queued and runs it once started again. In a task room the same message is chatter and is skipped.
func TestStoppedAgentKeepsAPlainDMNoteAndRunsItOnRestart(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_dm','owner','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES ((SELECT project_id FROM room WHERE id=?),'usr_dm','owner')`, room.ID); err != nil {
		t.Fatal(err)
	}
	dm, err := s.OpenDM(ctx, "usr_dm", one.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	msg, err := s.InsertMessage(ctx, Message{RoomID: dm.ID, AuthorKind: "user", AuthorID: "usr_dm", Body: "are you there"})
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := s.db.QueryRow(`SELECT wake_reason FROM note WHERE agent_id=? AND message_id=? AND state='queued'`, one.ID, msg.ID).Scan(&reason); err != nil {
		t.Fatalf("plain DM note not queued for the stopped agent: %v", err)
	}
	if reason != "human_message" {
		t.Fatalf("wake reason=%q", reason)
	}
	// The same plain message in the task room stays chatter.
	task, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "usr_dm", Body: "chatter"})
	if err != nil {
		t.Fatal(err)
	}
	if err := noQueuedNote(t, s, one.ID, task.ID, "task room, unaddressed, stopped agent"); err != nil {
		t.Fatal(err)
	}
	// Started again, a turn can be prepared from the queued note.
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesired(one.ID, "usr_dm", "active", 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, one.ID, ""); err != nil || !ready {
		t.Fatalf("no turn after restart: ready=%t %v", ready, err)
	}
}

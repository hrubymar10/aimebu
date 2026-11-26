package store

import (
	"testing"
	"time"
)

// An agent's last activity (the age of its state on the card) is its own doing. A message in its room still names it in a
// message_not_woken event while it is stopped, but must not make its age appear fresh.
func TestAnAgentsLastActivityMovesOnlyForWhatItDoesItself(t *testing.T) {
	t.Parallel()
	s, project, room, agent, _ := wakeFixture(t)
	ctx := t.Context()
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	last := func() string {
		t.Helper()
		cards, err := s.AgentCardsForRoom(ctx, room.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, card := range cards {
			if card.ID == agent.ID {
				return card.LastActivityAt
			}
		}
		t.Fatal("the agent has no card")
		return ""
	}
	record := func(kind string) {
		t.Helper()
		time.Sleep(2 * time.Millisecond) // the timestamps are to the nanosecond, but a clock may tick coarsely
		if err := s.Tx(ctx, func(tx *Tx) error {
			_, err := tx.AppendEvent(Event{Kind: kind, ProjectID: project.ID, RoomID: room.ID, AgentID: agent.ID, PayloadJSON: "{}"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// It stopped once. That is the age on the card.
	record("agent_stopped")
	stopped := last()
	if stopped == "" {
		t.Fatal("a stopped agent has an age")
	}

	// A person's unaddressed message in its room is skipped for it: not its doing.
	time.Sleep(2 * time.Millisecond)
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "usr_owner", Body: "hello everyone"}); err != nil {
		t.Fatal(err)
	}
	var skipped int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE agent_id=? AND kind='message_not_woken' AND json_extract(payload_json,'$.reason')='agent_stopped'`, agent.ID).Scan(&skipped); err != nil || skipped == 0 {
		t.Fatalf("the message should have recorded a skipped wake for the agent (%d, %v)", skipped, err)
	}
	if got := last(); got != stopped {
		t.Fatalf("a message in its room moved a stopped agent's last activity from %s to %s", stopped, got)
	}

	// Nor is anything else that is done to it or about it.
	for _, kind := range []string{"note_queued", "notes_dropped", "message_not_woken", "turn_dispatch_requested", "turn_dispatch_cancelled", "agent_role_changed",
		"agent_control_requested", "agent_switch_requested", "agent_key_rotated", "inbox_item_created", "inbox_item_resolved", "recovery_override", "diagnostic_requested", "loop_guard"} {
		record(kind)
		if got := last(); got != stopped {
			t.Fatalf("%s moved the agent's last activity from %s to %s", kind, stopped, got)
		}
	}

	// What the agent does itself does move it, each time.
	previous := stopped
	for _, kind := range []string{"agent_control_applied", "turn_ack_accepted", "turn_process_exit", "answer_committed", "supervisor_journal_changed", "native_session_started", "agent_spawn_confirmed"} {
		record(kind)
		got := last()
		if got <= previous {
			t.Fatalf("%s did not move the agent's last activity (%s, was %s)", kind, got, previous)
		}
		previous = got
	}
}

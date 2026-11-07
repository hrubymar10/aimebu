package store

import (
	"testing"
	"time"
)

// An agent's last activity (the age of its state on the card) is its own doing. Anyone posting in a room it belongs to queues a note for it, and
// that event names the agent too, so a stopped agent used to show "0:00" after every message and after every spawn next to it.
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

	// A person's message in its room queues a note for it: not its doing.
	time.Sleep(2 * time.Millisecond)
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "usr_owner", Body: "hello everyone"}); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE agent_id=? AND kind='note_queued'`, agent.ID).Scan(&queued); err != nil || queued == 0 {
		t.Fatalf("the message should have queued a note for the agent (%d, %v): the test would prove nothing", queued, err)
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

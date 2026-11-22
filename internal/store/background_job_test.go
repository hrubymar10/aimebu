package store

import (
	"context"
	"errors"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

func backgroundJobNotes(t *testing.T, s *Store, agentID string) (count int, body string) {
	t.Helper()
	rows, err := s.db.Query(`SELECT m.body FROM note n JOIN message m ON m.id=n.message_id WHERE n.agent_id=? AND n.wake_reason='background_job' AND n.state='queued'`, agentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		count++
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
	}
	return count, body
}

// A background job's answer between turns wakes the agent with a system note in the room of its last turn, once per turn: a second (or
// tenth) report after the same turn queues nothing more, and a report after the next turn wakes it again.
func TestBackgroundJobEndedWakesTheAgentOncePerTurn(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	if queued, err := s.RecordBackgroundJobEnded(ctx, f.agent.ID, 0); err != nil || queued {
		t.Fatalf("before any turn ended nothing is queued: queued=%t err=%v", queued, err)
	}
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{TerminalEvent: true}); err != nil {
		t.Fatal(err)
	}
	if queued, err := s.RecordBackgroundJobEnded(ctx, f.agent.ID, 0); err != nil || !queued {
		t.Fatalf("queued=%t err=%v", queued, err)
	}
	for range 3 {
		if queued, err := s.RecordBackgroundJobEnded(ctx, f.agent.ID, 0); err != nil || queued {
			t.Fatalf("a repeat after the same turn must not queue: queued=%t err=%v", queued, err)
		}
	}
	count, body := backgroundJobNotes(t, s, f.agent.ID)
	if count != 1 || body != backgroundJobNoteBody {
		t.Fatalf("notes=%d body=%q", count, body)
	}
	var roomID, turnID, payload string
	if err := s.db.QueryRow(`SELECT room_id,turn_id,payload_json FROM event WHERE kind='background_job_ended'`).Scan(&roomID, &turnID, &payload); err != nil {
		t.Fatal(err)
	}
	if roomID != f.turn.RoomID || turnID != f.turn.ID {
		t.Fatalf("event room=%s turn=%s payload=%s", roomID, turnID, payload)
	}
	// The next turn of the agent: a job that ends after it wakes the agent again.
	var next Turn
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		next, err = tx.RecordTurn(Turn{AgentID: f.agent.ID, SessionID: f.turn.SessionID, Generation: 0, RoomID: f.turn.RoomID, Attempt: 1, Mode: "normal", Exec: "pending"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcessInfo(ctx, next.ID, f.agent.ID, 0, adapter.ExitInfo{TerminalEvent: true}); err != nil {
		t.Fatal(err)
	}
	if queued, err := s.RecordBackgroundJobEnded(ctx, f.agent.ID, 0); err != nil || !queued {
		t.Fatalf("after the next turn: queued=%t err=%v", queued, err)
	}
}

// A stopped agent is not woken (the event says why), and a report from an older generation is refused.
func TestBackgroundJobEndedSkipsAStoppedAgentAndAStaleGeneration(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{TerminalEvent: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordBackgroundJobEnded(ctx, f.agent.ID, 7); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("a stale generation must be refused: %v", err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesired(f.agent.ID, "usr_recovery", "stopped", 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if queued, err := s.RecordBackgroundJobEnded(ctx, f.agent.ID, 0); err != nil || queued {
		t.Fatalf("a stopped agent must not be woken: queued=%t err=%v", queued, err)
	}
	if count, _ := backgroundJobNotes(t, s, f.agent.ID); count != 0 {
		t.Fatalf("notes=%d", count)
	}
	var payload string
	if err := s.db.QueryRow(`SELECT payload_json FROM event WHERE kind='background_job_ended'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != `{"not_woken":"agent_stopped"}` {
		t.Fatalf("payload=%s", payload)
	}
}

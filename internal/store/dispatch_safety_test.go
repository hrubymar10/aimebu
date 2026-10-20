package store

import (
	"context"
	"testing"
	"time"
)

func TestStartupSweepRestoresUndispatchedWork(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "work"})
	if err != nil {
		t.Fatal(err)
	}
	first, ready, err := s.PrepareDispatch(ctx, one.ID, "")
	if err != nil || !ready {
		t.Fatalf("first=%+v %v %v", first, ready, err)
	}
	count, err := s.SweepUndispatchedNormalTurns(ctx)
	if err != nil || count != 1 {
		t.Fatalf("sweep=%d %v", count, err)
	}
	state, err := s.DeliveryState(ctx, first.TurnID)
	if err != nil || state.Exec != "killed" || state.Settle != "settled" || state.NoteState != "queued" {
		t.Fatalf("state=%+v %v", state, err)
	}
	second, ready, err := s.PrepareDispatch(ctx, one.ID, "")
	if err != nil || !ready || second.TurnID == first.TurnID || len(second.Notes) != 1 || second.Notes[0].MessageID != message.ID {
		t.Fatalf("redispatch=%+v %v %v", second, ready, err)
	}
}

func TestAgentTurnBudgetPausesAndRaisesInbox(t *testing.T) {
	t.Parallel()
	s, project, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		turn, err := s.RecordTurn(ctx, Turn{AgentID: one.ID, SessionID: "ses_one", Generation: 0, RoomID: room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE turn SET exec='exited',outcome='completed',settle='settled',ended_at=? WHERE id=?`, timestamp(), turn.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "one more"}); err != nil {
		t.Fatal(err)
	}
	_, ready, err := s.PrepareDispatch(ctx, one.ID, "")
	if err != nil || ready {
		t.Fatalf("budget ready=%v %v", ready, err)
	}
	agent, err := s.AgentRuntimeByID(ctx, one.ID)
	if err != nil || agent.Desired != "not_accepting" {
		t.Fatalf("agent=%+v %v", agent, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE project_id=? AND agent_id=? AND kind='recovery' AND state='open'`, project.ID, one.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("inbox=%d %v", count, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='inbox_item_created' AND agent_id=?`, one.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("inbox event=%d %v", count, err)
	}
}

func TestTurnBudgetResetsOnContinueWithoutDuplicateRecovery(t *testing.T) {
	t.Parallel()
	s, project, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	seedCompletedTurns := func() {
		t.Helper()
		for i := 0; i < 20; i++ {
			turn, err := s.RecordTurn(ctx, Turn{AgentID: one.ID, SessionID: "ses_one", Generation: 0, RoomID: room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE turn SET exec='exited',outcome='completed',settle='settled',ended_at=? WHERE id=?`, timestamp(), turn.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	seedCompletedTurns()
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "queued"}); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, one.ID, ""); err != nil || ready {
		t.Fatalf("initial budget ready=%v %v", ready, err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.Exec(`UPDATE agent SET desired='active',desired_rev=desired_rev+1 WHERE id=?`, one.ID); err != nil {
			return err
		}
		_, err := tx.AppendEvent(Event{Kind: "agent_control_requested", ProjectID: project.ID, AgentID: one.ID, PayloadJSON: `{"desired":"active"}`})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	prepared, ready, err := s.PrepareDispatch(ctx, one.ID, "")
	if err != nil || !ready {
		t.Fatalf("continue ready=%v %v", ready, err)
	}
	if err := s.CancelPreparedDispatch(ctx, prepared.TurnID); err != nil {
		t.Fatal(err)
	}
	seedCompletedTurns()
	if _, ready, err := s.PrepareDispatch(ctx, one.ID, ""); err != nil || ready {
		t.Fatalf("second budget ready=%v %v", ready, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE agent_id=? AND kind='recovery' AND state='open'`, one.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate recovery=%d %v", count, err)
	}
}

func TestTurnBudgetIgnoresCancelledNeverStartedTurns(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		turn, err := s.RecordTurn(ctx, Turn{AgentID: one.ID, SessionID: "ses_one", Generation: 0, RoomID: room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE turn SET exec='killed',outcome='cancelled',settle='settled',ended_at=? WHERE id=?`, timestamp(), turn.ID); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err := s.Tx(ctx, func(tx *Tx) error {
				if _, err := tx.AppendEvent(Event{Kind: "turn_dispatch_requested", ProjectID: one.ProjectID, RoomID: room.ID, AgentID: one.ID, TurnID: turn.ID}); err != nil {
					return err
				}
				_, err := tx.AppendEvent(Event{Kind: "turn_never_started", ProjectID: one.ProjectID, RoomID: room.ID, AgentID: one.ID, TurnID: turn.ID})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "work"}); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, one.ID, ""); err != nil || !ready {
		t.Fatalf("cancelled turns exhausted budget: ready=%v %v", ready, err)
	}
}

func TestAgentTriggeredWakeCoolsDown(t *testing.T) {
	t.Parallel()
	s, _, room, one, lead := wakeFixture(t)
	ctx := context.Background()
	turn, err := s.RecordTurn(ctx, Turn{AgentID: one.ID, SessionID: "ses_one", Generation: 0, RoomID: room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',outcome='completed',settle='settled',ended_at=? WHERE id=?`, timestamp(), turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: lead.ID, Body: "@one review"}); err != nil {
		t.Fatal(err)
	}
	_, ready, err := s.PrepareDispatch(ctx, one.ID, "")
	if err != nil || ready {
		t.Fatalf("cooldown ready=%v %v", ready, err)
	}
	before := time.Now().UTC().Add(-3 * time.Second).Format("2006-01-02T15:04:05.000000000Z")
	if _, err := s.db.Exec(`UPDATE turn SET ended_at=? WHERE id=?`, before, turn.ID); err != nil {
		t.Fatal(err)
	}
	_, ready, err = s.PrepareDispatch(ctx, one.ID, "")
	if err != nil || !ready {
		t.Fatalf("after cooldown ready=%v %v", ready, err)
	}
}

func TestClosedRoomAbandonsPendingCorrection(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, true)
	ctx := context.Background()
	first, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, `{"messages":[`)
	if err != nil || first.Result != "correction_requested" {
		t.Fatalf("first=%+v %v", first, err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	child, err := s.CorrectionTurn(ctx, f.turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, f.room.ID); err != nil {
		t.Fatal(err)
	}
	count, err := s.ExpireBlockedSpecialTurns(ctx)
	if err != nil || count != 1 {
		t.Fatalf("expire=%d %v", count, err)
	}
	state, err := s.DeliveryState(ctx, child.ID)
	if err != nil || state.Exec != "killed" || state.Settle != "settled" {
		t.Fatalf("child=%+v %v", state, err)
	}
	parent, err := s.DeliveryState(ctx, f.turn.ID)
	if err != nil || parent.Settle != "settled" {
		t.Fatalf("parent=%+v %v", parent, err)
	}
}

func TestClosedRoomDoesNotAbandonSentCorrection(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, true)
	ctx := context.Background()
	first, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, `{"messages":[`)
	if err != nil || first.Result != "correction_requested" {
		t.Fatalf("first=%+v %v", first, err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	child, err := s.CorrectionTurn(ctx, f.turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(Event{Kind: "turn_dispatch_requested", AgentID: f.agent.ID, TurnID: child.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, f.room.ID); err != nil {
		t.Fatal(err)
	}
	count, err := s.ExpireBlockedSpecialTurns(ctx)
	if err != nil || count != 0 {
		t.Fatalf("sent expiry=%d %v", count, err)
	}
	state, err := s.DeliveryState(ctx, child.ID)
	if err != nil || state.Exec != "pending" {
		t.Fatalf("sent child=%+v %v", state, err)
	}
}

func TestAdmittedDiagnosticCannotBeRedispatchedOrAbandoned(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, true)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	diagnostic, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || diagnostic.Mode != "diagnostic" {
		t.Fatalf("first diagnostic=%+v ready=%t %v", diagnostic, ready, err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: diagnostic.TurnID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0}); err != nil {
		t.Fatal(err)
	}
	if repeated, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || ready {
		t.Fatalf("admitted diagnostic redispatched: %+v ready=%t %v", repeated, ready, err)
	}
	if err := s.AbandonSpecialTurn(ctx, diagnostic.TurnID, "dispatch_failed"); err != ErrInvalidTransition {
		t.Fatalf("admitted diagnostic abandonment=%v", err)
	}
	state, err := s.DeliveryState(ctx, diagnostic.TurnID)
	if err != nil || state.Exec != "pending" {
		t.Fatalf("admitted diagnostic state=%+v %v", state, err)
	}
	if _, err := s.db.Exec(`UPDATE machine SET journal_id='journal-intact' WHERE id=?`, f.agent.MachineID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileMachineHello(ctx, f.agent.MachineID, "journal-intact", []byte(`[]`), nil); err != nil {
		t.Fatal(err)
	}
	state, err = s.DeliveryState(ctx, diagnostic.TurnID)
	if err != nil || state.Exec != "killed" {
		t.Fatalf("journal-proven never-started diagnostic=%+v %v", state, err)
	}
}

func TestAdmittedNormalTurnCannotBeCancelledAsUnsent(t *testing.T) {
	t.Parallel()
	s, _, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "work"}); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, agent.ID, "")
	if err != nil || !ready || len(delivery.Notes) != 1 {
		t.Fatalf("normal delivery=%+v ready=%t %v", delivery, ready, err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: delivery.TurnID, AgentID: agent.ID, MachineID: agent.MachineID,
		Generation: 0, DesiredRev: 0, NoteIDs: []string{delivery.Notes[0].NoteID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelPreparedDispatch(ctx, delivery.TurnID); err != ErrInvalidTransition {
		t.Fatalf("admitted normal cancellation=%v", err)
	}
	state, err := s.DeliveryState(ctx, delivery.TurnID)
	if err != nil || state.Exec != "pending" {
		t.Fatalf("admitted normal state=%+v %v", state, err)
	}
}

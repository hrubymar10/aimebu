package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestClearContextWaitsForTurnAndKeepsSessionHistory(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE session SET native_session_id='old-thread',native_session_started_at=? WHERE id=?`, timestamp(), f.turn.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='in_turn' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	var requested AgentCard
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		requested, err = tx.RequestAgentContextClear(f.agent.ID, owner, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if requested.Generation != 0 || requested.Desired != "active" {
		t.Fatalf("request changed active turn: %+v", requested)
	}
	if changed, err := s.AdvanceAgentContextClears(ctx); err != nil || len(changed) != 0 {
		t.Fatalf("advanced during turn: %+v %v", changed, err)
	}
	var clearEvents int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE agent_id=? AND kind='agent_context_cleared'`, f.agent.ID).Scan(&clearEvents); err != nil || clearEvents != 0 {
		t.Fatalf("clear event before turn ended = %d, %v", clearEvents, err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || ready {
		t.Fatalf("dispatch while clear pending: ready=%t err=%v", ready, err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	changed, err := s.AdvanceAgentContextClears(ctx)
	if err != nil || len(changed) != 1 || changed[0].Generation != 1 {
		t.Fatalf("advance: %+v %v", changed, err)
	}
	var actor string
	if err := s.db.QueryRow(`SELECT json_extract(payload_json,'$.actor_id') FROM event WHERE agent_id=? AND kind='agent_context_cleared'`, f.agent.ID).Scan(&actor); err != nil || actor != owner {
		t.Fatalf("clear actor = %q, %v", actor, err)
	}
	messages, err := s.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil || len(messages) == 0 || messages[len(messages)-1].Body != "worker's context was cleared" {
		t.Fatalf("clear notice: %+v %v", messages, err)
	}
	var oldEnd, oldHandle, newMode, newHandle sql.NullString
	if err := s.db.QueryRow(`SELECT ended_at,native_session_id FROM session WHERE id=?`, f.turn.SessionID).Scan(&oldEnd, &oldHandle); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT start_mode,native_session_id FROM session WHERE agent_id=? AND generation=1`, f.agent.ID).Scan(&newMode, &newHandle); err != nil {
		t.Fatal(err)
	}
	if !oldEnd.Valid || oldHandle.String != "old-thread" || newMode.String != "fresh_with_history" || newHandle.String == "old-thread" {
		t.Fatalf("sessions: old end=%v handle=%v, new mode=%v handle=%v", oldEnd, oldHandle, newMode, newHandle)
	}
	if pending, ok, err := s.PendingSpawnByID(ctx, f.agent.ID); err != nil || !ok || pending.Generation != 1 {
		t.Fatalf("fresh spawn: %+v %t %v", pending, ok, err)
	}
	_, err = s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: owner, Body: "new work"})
	if err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.Generation != 1 || delivery.NativeSessionStarted || delivery.NativeSessionHandle == "old-thread" {
		t.Fatalf("next turn resumed old context: %+v ready=%t err=%v", delivery, ready, err)
	}
	if again, err := s.AdvanceAgentContextClears(ctx); err != nil || len(again) != 0 {
		t.Fatalf("second advance: %+v %v", again, err)
	}
}

func TestClearContextRequiresOperator(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_viewer','viewer','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_viewer','viewer')`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	err := s.Tx(context.Background(), func(tx *Tx) error { _, err := tx.RequestAgentContextClear(f.agent.ID, "usr_viewer", 0); return err })
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer clear = %v", err)
	}
}

func TestClearContextWaitsThroughPause(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.RequestAgentContextClear(f.agent.ID, owner, 0); return err }); err != nil {
		t.Fatal(err)
	}
	var paused AgentCard
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		paused, err = tx.SetAgentDesired(f.agent.ID, owner, "not_accepting", 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.AdvanceAgentContextClears(ctx); err != nil || len(changed) != 0 {
		t.Fatalf("paused clear advanced: %+v %v", changed, err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesired(f.agent.ID, owner, "active", paused.DesiredRev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.AdvanceAgentContextClears(ctx); err != nil || len(changed) != 1 || changed[0].Generation != 1 {
		t.Fatalf("resumed clear: %+v %v", changed, err)
	}
}

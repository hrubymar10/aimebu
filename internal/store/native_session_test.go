package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestNativeSessionStartPersistsAndRejectsConflicts(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if err := s.RecordNativeSessionStarted(ctx, f.turn.ID, f.agent.ID, 0, "source-1", "thread-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordNativeSessionStarted(ctx, f.turn.ID, f.agent.ID, 0, "source-1", "thread-1"); err != nil {
		t.Fatalf("journal replay: %v", err)
	}
	if err := s.RecordNativeSessionStarted(ctx, f.turn.ID, f.agent.ID, 0, "source-2", "thread-2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed handle: %v", err)
	}
	if err := s.RecordNativeSessionStarted(ctx, f.turn.ID, f.agent.ID, 1, "source-3", "thread-1"); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("wrong generation: %v", err)
	}
	var handle, started sql.NullString
	if err := s.db.QueryRow(`SELECT native_session_id,native_session_started_at FROM session WHERE id=?`, f.turn.SessionID).Scan(&handle, &started); err != nil {
		t.Fatal(err)
	}
	if handle.String != "thread-1" || !started.Valid {
		t.Fatalf("session handle=%v started=%v", handle, started)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE turn_id=? AND kind='native_session_started'`, f.turn.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("event count=%d err=%v", count, err)
	}
}

package store

import (
	"context"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

const threadMissingStderr = "ERROR codex_core::rollout: failed to record rollout items: thread 019a1b2c-3d4e not found"

func TestCodexThreadMissingEvidenceNeedsBothPhrases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tool, stderr, event string
		want                bool
	}{
		{"codex", threadMissingStderr, "", true},
		{"codex", "", threadMissingStderr, true},
		{"codex", "thread abc not found", "", false},
		{"codex", "failed to record rollout items: disk full", "", false},
		{"claude", threadMissingStderr, "", false},
		{"claude", "No conversation found with session ID: aaaa", "", true},
		{"opencode", "Error: Session not found", "", true},
		{"opencode", "", "OpenCode error: Session not found", false},
		{"opencode", "Error: Something else not found", "", false},
		{"codex", "Error: Session not found", "", false},
		{"vibe", "Error: Session not found: 0c3e1f4a-8b2d-4a6e-9f10-5d7c2b1a9e33", "", true},
		{"vibe", "", "Vibe error: Session not found", false},
		{"vibe", "Error: Something else not found", "", false},
	} {
		if got := nativeSessionMissingEvidence(tc.tool, tc.stderr, tc.event); got != tc.want {
			t.Errorf("%s %q %q = %t", tc.tool, tc.stderr, tc.event, got)
		}
	}
}

func TestPiRecreatedSessionResetsStartedNativeSession(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agent SET tool='pi' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	var sessionID string
	if err := s.db.QueryRow(`SELECT session_id FROM turn WHERE id=?`, f.turn.ID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE session SET native_session_id='gone',native_session_started_at=? WHERE id=?`, timestamp(), sessionID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{ExitCode: 1, SessionRecreated: true}); err != nil {
		t.Fatal(err)
	}
	var handle *string
	var resets, recoveries int
	if err := s.db.QueryRow(`SELECT native_session_id FROM session WHERE id=?`, sessionID).Scan(&handle); err != nil || handle != nil {
		t.Fatalf("native session handle = %v, err = %v", handle, err)
	}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='native_session_reset' AND turn_id=? AND json_extract(payload_json,'$.reason')='pi_session_missing'`, f.turn.ID).Scan(&resets)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE kind='recovery'`).Scan(&recoveries)
	if resets != 1 || recoveries != 0 {
		t.Fatalf("resets=%d recoveries=%d", resets, recoveries)
	}
	if delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || !ready || delivery.NativeSessionStarted || delivery.NativeSessionHandle != "" {
		t.Fatalf("next dispatch = %+v, ready=%t, err=%v", delivery, ready, err)
	}
}

func TestPiRecreatedSessionAfterAnswerKeepsAnswerAndResetsContext(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agent SET tool='pi' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	var sessionID string
	if err := s.db.QueryRow(`SELECT session_id FROM turn WHERE id=?`, f.turn.ID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE session SET native_session_id='gone',native_session_started_at=? WHERE id=?`, timestamp(), sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET settle='settled' WHERE id=?`, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO session_context_delivery(session_id,role_revision,role_turn_id,role_confirmed,knowledge_revision,knowledge_turn_id,knowledge_confirmed) VALUES (?,?,?,1,?,?,1)`, sessionID, "role", f.turn.ID, "knowledge", f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{TerminalEvent: true, SessionRecreated: true}); err != nil {
		t.Fatal(err)
	}
	var handle, outcome, noteState *string
	var rows, resets int
	if err := s.db.QueryRow(`SELECT native_session_id FROM session WHERE id=?`, sessionID).Scan(&handle); err != nil || handle != nil {
		t.Fatalf("handle=%v err=%v", handle, err)
	}
	if err := s.db.QueryRow(`SELECT outcome FROM turn WHERE id=?`, f.turn.ID).Scan(&outcome); err != nil || outcome == nil || *outcome != "completed" {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&noteState); err != nil || noteState == nil || *noteState != "done" {
		t.Fatalf("note=%v err=%v", noteState, err)
	}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM session_context_delivery WHERE session_id=?`, sessionID).Scan(&rows)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='native_session_reset' AND turn_id=?`, f.turn.ID).Scan(&resets)
	if rows != 0 || resets != 1 {
		t.Fatalf("context rows=%d resets=%d", rows, resets)
	}
}

func TestPiRecreatedSessionWithoutStartedSessionDoesNotReset(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	if _, err := s.db.Exec(`UPDATE agent SET tool='pi' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcessInfo(context.Background(), f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{ExitCode: 1, SessionRecreated: true}); err != nil {
		t.Fatal(err)
	}
	var resets int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='native_session_reset' AND turn_id=?`, f.turn.ID).Scan(&resets)
	if resets != 0 {
		t.Fatalf("fresh turn reset %d times", resets)
	}
}

func TestCodexThreadMissingStartsFreshSessionWithoutRecovery(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	var sessionID string
	if err := s.db.QueryRow(`SELECT session_id FROM turn WHERE id=?`, f.turn.ID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE session SET native_session_id='019a1b2c-3d4e',native_session_started_at=? WHERE id=?`, timestamp(), sessionID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{ExitCode: 1, StderrTail: threadMissingStderr}); err != nil {
		t.Fatal(err)
	}
	var outcome, settle, noteState string
	var blocked bool
	var handle, started *string
	if err := s.db.QueryRow(`SELECT outcome,settle FROM turn WHERE id=?`, f.turn.ID).Scan(&outcome, &settle); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&noteState); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT recovery_block FROM agent WHERE id=?`, f.agent.ID).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT native_session_id,native_session_started_at FROM session WHERE id=?`, sessionID).Scan(&handle, &started); err != nil {
		t.Fatal(err)
	}
	if outcome != "failed" || settle != "settled" || noteState != "queued" || blocked || handle != nil || started != nil {
		t.Fatalf("turn=%s/%s note=%s recovery=%t handle=%v started=%v", outcome, settle, noteState, blocked, handle, started)
	}
	var resets, recoveries int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='native_session_reset' AND turn_id=?`, f.turn.ID).Scan(&resets)
	var reason string
	_ = s.db.QueryRow(`SELECT json_extract(payload_json,'$.reason') FROM event WHERE kind='native_session_reset' AND turn_id=?`, f.turn.ID).Scan(&reason)
	if reason != "codex_thread_missing" {
		t.Fatalf("Codex keeps its recorded reason string, got %q", reason)
	}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE kind='recovery'`).Scan(&recoveries)
	if resets != 1 || recoveries != 0 {
		t.Fatalf("resets=%d recoveries=%d", resets, recoveries)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.NativeSessionStarted || delivery.NativeSessionHandle != "" || len(delivery.Notes) != 1 {
		t.Fatalf("fresh dispatch=%+v ready=%t error=%v", delivery, ready, err)
	}
}

func TestCodexThreadMissingWithoutStartedSessionStaysRecovery(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{ExitCode: 1, StderrTail: threadMissingStderr}); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].Kind != "recovery" {
		t.Fatalf("a fresh start that fails must not loop: %+v %v", items, err)
	}
}

func TestCodexThreadMissingAfterTheTurnOpenedItsOwnThreadStaysRecovery(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordNativeSessionStarted(ctx, f.turn.ID, f.agent.ID, 0, "src-1", "019a-fresh"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{ExitCode: 1, StderrTail: threadMissingStderr}); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].Kind != "recovery" {
		t.Fatalf("a fresh thread that is lost again must not reset in a loop: %+v %v", items, err)
	}
	var started *string
	if err := s.db.QueryRow(`SELECT native_session_id FROM session WHERE id=(SELECT session_id FROM turn WHERE id=?)`, f.turn.ID).Scan(&started); err != nil || started == nil {
		t.Fatalf("the session handle must be kept: %v %v", started, err)
	}
}

func TestOpenCodeMissingSessionStartsFreshSessionWithoutRecovery(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agent SET tool='opencode' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	var sessionID string
	if err := s.db.QueryRow(`SELECT session_id FROM turn WHERE id=?`, f.turn.ID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE session SET native_session_id='ses_gone',native_session_started_at=? WHERE id=?`, timestamp(), sessionID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{ExitCode: 1, StderrTail: "Error: Session not found"}); err != nil {
		t.Fatal(err)
	}
	var handle *string
	var recoveries, resets int
	if err := s.db.QueryRow(`SELECT native_session_id FROM session WHERE id=?`, sessionID).Scan(&handle); err != nil || handle != nil {
		t.Fatalf("the handle must be forgotten: %v %v", handle, err)
	}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE kind='recovery'`).Scan(&recoveries)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='native_session_reset' AND json_extract(payload_json,'$.reason')='opencode_session_missing'`).Scan(&resets)
	if recoveries != 0 || resets != 1 {
		t.Fatalf("recoveries=%d resets=%d", recoveries, resets)
	}
	if delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || !ready || delivery.NativeSessionStarted || delivery.NativeSessionHandle != "" {
		t.Fatalf("the next delivery must start a fresh session: %+v ready=%t %v", delivery, ready, err)
	}
}

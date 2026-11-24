package store

import (
	"context"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/answer"
)

func decidedContinuation(t *testing.T, choice string) (*Store, executionFixture, string) {
	t.Helper()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("recovery item=%+v %v", items, err)
	}
	decision, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "decide-"+choice, choice, int(items[0].Rev))
	if err != nil || decision.TurnID == "" {
		t.Fatalf("decision=%+v %v", decision, err)
	}
	return s, f, decision.TurnID
}

func TestRecoveryContinuationSurvivesStartupSweep(t *testing.T) {
	t.Parallel()
	s, f, continuationID := decidedContinuation(t, "resume")
	ctx := context.Background()
	if count, err := s.SweepUndispatchedNormalTurns(ctx); err != nil || count != 0 {
		t.Fatalf("swept recovery continuation: %d %v", count, err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.TurnID != continuationID || !strings.Contains(string(delivery.Context), "do not repeat") {
		t.Fatalf("recovered delivery=%+v ready=%t %v", delivery, ready, err)
	}
}

func TestRecoveryContinuationCannotBeCancelledAsUnsent(t *testing.T) {
	t.Parallel()
	s, f, continuationID := decidedContinuation(t, "resume")
	ctx := context.Background()
	if err := s.CancelPreparedDispatch(ctx, continuationID); err != ErrInvalidTransition {
		t.Fatalf("cancelled continuation=%v", err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.TurnID != continuationID || !strings.Contains(string(delivery.Context), "do not repeat") {
		t.Fatalf("recovered delivery=%+v ready=%t %v", delivery, ready, err)
	}
}

func TestPersistedRecoveryContinuationIsNotRedispatched(t *testing.T) {
	t.Parallel()
	s, f, continuationID := decidedContinuation(t, "resume")
	ctx := context.Background()
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.TurnID != continuationID {
		t.Fatalf("first delivery=%+v ready=%t %v", delivery, ready, err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: continuationID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{f.note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AckTurn(ctx, continuationID, f.agent.ID, 0, "persisted"); err != nil {
		t.Fatal(err)
	}
	if repeated, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || ready {
		t.Fatalf("persisted continuation redispatched=%+v ready=%t %v", repeated, ready, err)
	}
}

func TestJournalProvenUnstartedRecoveryContinuationKeepsEvidence(t *testing.T) {
	t.Parallel()
	s, f, continuationID := decidedContinuation(t, "resume")
	ctx := context.Background()
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.TurnID != continuationID {
		t.Fatalf("first continuation=%+v ready=%t %v", delivery, ready, err)
	}
	if _, err := s.db.Exec(`UPDATE machine SET journal_id='intact' WHERE id=?`, f.agent.MachineID); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: continuationID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{f.note.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileMachineHello(ctx, f.agent.MachineID, "intact", []byte(`[]`), nil); err != nil {
		t.Fatal(err)
	}
	replacement, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || replacement.TurnID == continuationID || replacement.ParentTurnID != f.turn.ID || !strings.Contains(string(replacement.Context), "do not repeat") {
		t.Fatalf("replacement=%+v ready=%t %v", replacement, ready, err)
	}
}

func TestStopThenHelloAbortsUnstartedRecoveryContinuation(t *testing.T) {
	t.Parallel()
	s, f, continuationID := decidedContinuation(t, "resume")
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE machine SET journal_id='intact-stop' WHERE id=?`, f.agent.MachineID); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: continuationID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{f.note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.SetAgentDesired(f.agent.ID, "usr_recovery", "stopped", 0); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileMachineHello(ctx, f.agent.MachineID, "intact-stop", []byte(`[]`), nil); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM turn WHERE agent_id=? AND exec='pending'`, f.agent.ID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("stopped pending turns=%d %v", pending, err)
	}
	var state, observed string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&state); err != nil || state != "aborted" {
		t.Fatalf("stopped note=%q %v", state, err)
	}
	if err := s.db.QueryRow(`SELECT observed FROM agent WHERE id=?`, f.agent.ID).Scan(&observed); err != nil || observed != "stopped" {
		t.Fatalf("stopped agent=%q %v", observed, err)
	}
}

func TestStopCancelsUnsentRecoveryContinuationWithoutRawRetry(t *testing.T) {
	t.Parallel()
	s, f, continuationID := decidedContinuation(t, "resume")
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.SetAgentDesired(f.agent.ID, "usr_recovery", "stopped", 0); return err }); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&state); err != nil || state != "aborted" {
		t.Fatalf("stopped continuation note=%q %v", state, err)
	}
	turn, err := s.DeliveryState(ctx, continuationID)
	if err != nil || turn.Exec != "killed" {
		t.Fatalf("stopped continuation=%+v %v", turn, err)
	}
}

func TestStoppedAgentCannotResolveRecoveryByCreatingContinuation(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	// A genuine crash (no cancel intent) of a stopped agent's turn still raises the recovery, exactly as it would for an active one;
	// a deliberate stop is what raises nothing (see TestStopDuringTurnCancelsWithoutRecovery).
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v %v", items, err)
	}
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "stopped-resume", "resume", 0); err != ErrInvalidTransition {
		t.Fatalf("stopped resume=%v", err)
	}
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "stopped-abandon", "abandon", 0); err != nil {
		t.Fatalf("stopped abandon=%v", err)
	}
}

func TestStopOnReadonlyTurnCancelsWithoutItemOrDiagnostic(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, true)
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.SetAgentDesired(f.agent.ID, owner, "stopped", 0); return err }); err != nil {
		t.Fatal(err)
	}
	// The stop itself is why the turn ends, so nothing is recovered and no read-only diagnostic is started.
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, true); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 0 {
		t.Fatalf("stopped recovery items=%+v %v", items, err)
	}
	var diagnostics int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM turn WHERE parent_turn_id=? AND mode='diagnostic'`, f.turn.ID).Scan(&diagnostics); err != nil || diagnostics != 0 {
		t.Fatalf("stopped diagnostics=%d %v", diagnostics, err)
	}
	var outcome, settle string
	if err := s.db.QueryRow(`SELECT outcome,settle FROM turn WHERE id=?`, f.turn.ID).Scan(&outcome, &settle); err != nil || outcome != "cancelled" || settle != "settled" {
		t.Fatalf("stopped readonly turn outcome=%q settle=%q %v", outcome, settle, err)
	}
}

func TestNewRecoveryEvidenceRefreshesItemRevision(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v %v", items, err)
	}
	raw := []byte(`{"messages":[]}`)
	parsed, err := answer.Parse(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAnswer(ctx, AnswerInput{TurnID: f.turn.ID, AgentID: f.agent.ID, Generation: 0, AnswerRev: 0, Digest: "late-evidence", Candidate: raw, Parsed: &parsed}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "old-rev", "abandon", 0); err != ErrStaleInboxRev {
		t.Fatalf("old revision=%v", err)
	}
	refreshed, err := s.InboxForUser(ctx, owner)
	if err != nil || len(refreshed) != 1 || refreshed[0].Rev <= items[0].Rev {
		t.Fatalf("refreshed=%+v %v", refreshed, err)
	}
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "new-rev", "abandon", int(refreshed[0].Rev)); err != nil {
		t.Fatalf("refreshed decision=%v", err)
	}
}

func TestStopAbandonsUnsentDiagnosticThroughParentCleanup(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, true)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.SetAgentDesired(f.agent.ID, owner, "stopped", 0); return err }); err != nil {
		t.Fatal(err)
	}
	// The stop settles the recovery: nobody is left to continue the turn, so there is nothing for a person to decide.
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 0 {
		t.Fatalf("stopped agent's recovery still listed=%+v %v", items, err)
	}
	var state, by string
	if err := s.db.QueryRow(`SELECT state,resolved_by FROM inbox_item WHERE turn_id=? AND kind='recovery'`, f.turn.ID).Scan(&state, &by); err != nil || state != "resolved" || by != "system (agent stopped)" {
		t.Fatalf("recovery state=%q by=%q %v", state, by, err)
	}
}

func TestStopAbandonsUnsentCorrectionThroughParentCleanup(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, true)
	ctx := context.Background()
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, `{"messages":[`)
	if err != nil || receipt.Result != "correction_requested" {
		t.Fatalf("correction request=%+v %v", receipt, err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { _, err := tx.SetAgentDesired(f.agent.ID, "usr_answers", "stopped", 0); return err }); err != nil {
		t.Fatal(err)
	}
	var settle string
	if err := s.db.QueryRow(`SELECT settle FROM turn WHERE id=?`, f.turn.ID).Scan(&settle); err != nil || settle != "settled" {
		t.Fatalf("parent settle=%q %v", settle, err)
	}
}

func TestRecoveryDecisionDoesNotRevealInaccessibleItemState(t *testing.T) {
	t.Parallel()
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	var itemID string
	if err := s.db.QueryRow(`SELECT id FROM inbox_item WHERE turn_id=?`, f.turn.ID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{itemID, "inb_nonexistent"} {
		if _, _, err := s.DecideRecovery(ctx, id, "usr_outsider", "outsider-"+id, "abandon", 0); err != ErrForbidden {
			t.Fatalf("outsider item %q error=%v", id, err)
		}
	}
}

func TestStoppedAgentCleanDiagnosticRequiresOwnerDecision(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, true)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE settings_project SET auto_resume_after_clean_diagnostic=1 WHERE project_id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	diagnostic, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready {
		t.Fatalf("diagnostic=%+v ready=%t %v", diagnostic, ready, err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: diagnostic.TurnID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.AckTurn(ctx, diagnostic.TurnID, f.agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='interrupting' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"messages":[],"reconciliation":{"status":"clean","findings":[]}}`)
	parsed, err := answer.Parse(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAnswer(ctx, AnswerInput{TurnID: diagnostic.TurnID, AgentID: f.agent.ID, Generation: 0, AnswerRev: 0, Digest: "stopped-clean", Candidate: raw, Parsed: &parsed}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, diagnostic.TurnID, f.agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].TurnID != f.turn.ID {
		t.Fatalf("stopped diagnostic item=%+v %v", items, err)
	}
}

func lostRecoveryDecision(t *testing.T) (*Store, executionFixture, string, string) {
	t.Helper()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if _, err := s.ReconcileMachineHello(ctx, f.agent.MachineID, "replacement-journal", []byte(`[]`), nil); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].TurnExec != "lost" {
		t.Fatalf("lost recovery item=%+v %v", items, err)
	}
	decision, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "lost-resume", "resume", int(items[0].Rev))
	if err != nil || decision.TurnID == "" {
		t.Fatalf("lost decision=%+v %v", decision, err)
	}
	return s, f, owner, decision.TurnID
}

func TestLostRecoveryWaitsForPositiveQuiescence(t *testing.T) {
	t.Parallel()
	s, f, owner, continuationID := lostRecoveryDecision(t)
	ctx := context.Background()
	if delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || ready {
		t.Fatalf("lost continuation dispatched=%+v ready=%t %v", delivery, ready, err)
	}
	waiting, err := s.InboxForUser(ctx, owner)
	if err != nil || len(waiting) != 1 || !strings.Contains(strings.ToLower(waiting[0].Question), "waiting for the old process") || waiting[0].Options[0] != "Override" {
		t.Fatalf("waiting item=%+v %v", waiting, err)
	}
	if _, err := s.ReconcileMachineHello(ctx, f.agent.MachineID, "replacement-journal", []byte(`[]`), nil); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || ready {
		t.Fatalf("journal absence released continuation: ready=%t %v", ready, err)
	}
	open := map[string]JournalTurn{f.turn.ID: {TurnID: f.turn.ID, Exec: "exited", ProcessAlive: false}}
	if _, err := s.ReconcileMachineHello(ctx, f.agent.MachineID, "replacement-journal", []byte(`[]`), open); err != nil {
		t.Fatal(err)
	}
	if items, err := s.InboxForUser(ctx, owner); err != nil || len(items) != 0 {
		t.Fatalf("quiescence waiting item=%+v %v", items, err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.TurnID != continuationID {
		t.Fatalf("quiescent continuation=%+v ready=%t %v", delivery, ready, err)
	}
}

func TestLostRecoveryOwnerOverrideNamesOverlapRisk(t *testing.T) {
	t.Parallel()
	s, f, owner, continuationID := lostRecoveryDecision(t)
	ctx := context.Background()
	waiting, err := s.InboxForUser(ctx, owner)
	if err != nil || len(waiting) != 1 {
		t.Fatalf("waiting item=%+v %v", waiting, err)
	}
	if _, _, err := s.DecideRecovery(ctx, waiting[0].ID, owner, "owner-overlap-override", "override", int(waiting[0].Rev)); err != nil {
		t.Fatalf("overlap override=%v", err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.TurnID != continuationID {
		t.Fatalf("override continuation=%+v ready=%t %v", delivery, ready, err)
	}
	var risk, actor string
	if err := s.db.QueryRow(`SELECT json_extract(payload_json,'$.risk'),json_extract(payload_json,'$.actor') FROM event WHERE turn_id=? AND kind='recovery_overlap_override'`, f.turn.ID).Scan(&risk, &actor); err != nil || !strings.Contains(risk, "old process") || actor != owner {
		t.Fatalf("override risk=%q actor=%q %v", risk, actor, err)
	}
}

func TestLostRecoveryWaitingItemCanAbandonContinuation(t *testing.T) {
	t.Parallel()
	s, f, owner, continuationID := lostRecoveryDecision(t)
	ctx := context.Background()
	waiting, err := s.InboxForUser(ctx, owner)
	if err != nil || len(waiting) != 1 {
		t.Fatalf("waiting item=%+v %v", waiting, err)
	}
	if _, _, err := s.DecideRecovery(ctx, waiting[0].ID, owner, "abandon-wait", "abandon", int(waiting[0].Rev)); err != nil {
		t.Fatalf("abandon wait=%v", err)
	}
	turn, err := s.DeliveryState(ctx, continuationID)
	if err != nil || turn.Exec != "killed" {
		t.Fatalf("abandoned continuation=%+v %v", turn, err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&state); err != nil || state != "aborted" {
		t.Fatalf("abandoned note=%q %v", state, err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || ready {
		t.Fatalf("abandoned continuation dispatch ready=%t %v", ready, err)
	}
}

func TestLostRecoverySupervisorExitConfirmsQuiescence(t *testing.T) {
	t.Parallel()
	s, f, owner, continuationID := lostRecoveryDecision(t)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	if items, err := s.InboxForUser(ctx, owner); err != nil || len(items) != 0 {
		t.Fatalf("process exit waiting item=%+v %v", items, err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.TurnID != continuationID {
		t.Fatalf("process exit continuation=%+v ready=%t %v", delivery, ready, err)
	}
}

package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/answer"
)

func recoveryFixture(t *testing.T, readonly bool) (*Store, executionFixture, string) {
	t.Helper()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if err := s.AddMember(ctx, f.room.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_recovery','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_recovery','owner')`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, f.turn.ID, f.note.ID); err != nil {
		t.Fatal(err)
	}
	if readonly {
		capabilities := `[{"tool":"codex","version":"test","environment":{"variant":"native"},"flags":{"readonly_verified":true}}]`
		if _, err := s.db.Exec(`UPDATE machine SET capabilities_json=? WHERE id=?`, capabilities, f.agent.MachineID); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: f.turn.ID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{f.note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AckTurn(ctx, f.turn.ID, f.agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	return s, f, "usr_recovery"
}

func TestFailedProcessStderrAppearsInRecoveryAndAgentCard(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, adapter.ExitInfo{ExitCode: 17, StderrTail: "Bearer aak_secret: Directory /tmp is not mounted", ErrorEvent: `{"type":"turn.failed","message":"aak_secret invalid_grant"}`}); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || !strings.Contains(items[0].Evidence, "code 17") || !strings.Contains(items[0].Evidence, "not mounted") || !strings.Contains(items[0].Evidence, "invalid_grant") || strings.Contains(items[0].Evidence, "aak_secret") {
		t.Fatalf("recovery evidence=%+v err=%v", items, err)
	}
	cards, err := s.AgentCardsForRoom(ctx, f.room.ID)
	if err != nil || len(cards) != 1 || cards[0].LastExitEvidence != items[0].Evidence {
		t.Fatalf("cards=%+v err=%v", cards, err)
	}
}

func TestRecoveryResumeDecisionReusesNotesAndEvidence(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE message SET visual_plan_json=?,appendix_pages_json=? WHERE id=?`,
		`[{"type":"markdown","data":{"markdown":"Visual recovery context"}}]`,
		`[{"title":"Follow-up","body":"Appendix recovery context"}]`, f.note.MessageID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].Kind != "recovery" || items[0].TurnID != f.turn.ID {
		t.Fatalf("recovery items=%+v %v", items, err)
	}
	decision, reused, err := s.DecideRecovery(ctx, items[0].ID, owner, "resume-key", "resume", 0)
	if err != nil || reused || decision.TurnID == "" {
		t.Fatalf("decision=%+v reused=%t %v", decision, reused, err)
	}
	again, reused, err := s.DecideRecovery(ctx, items[0].ID, owner, "resume-key", "resume", 0)
	if err != nil || !reused || again.TurnID != decision.TurnID {
		t.Fatalf("replay=%+v reused=%t %v", again, reused, err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.TurnID != decision.TurnID || len(delivery.Notes) != 1 || delivery.Notes[0].NoteID != f.note.ID {
		t.Fatalf("resume delivery=%+v ready=%t %v", delivery, ready, err)
	}
	if !strings.Contains(delivery.Notes[0].Body, "Visual recovery context") || !strings.Contains(delivery.Notes[0].Body, "Appendix recovery context") {
		t.Fatalf("resume note lost visual context: %q", delivery.Notes[0].Body)
	}
	if !strings.Contains(string(delivery.Context), "do not repeat") {
		t.Fatalf("resume context=%s", delivery.Context)
	}
	var identity struct {
		YouAre     map[string]string   `json:"you_are"`
		TeamRoster []map[string]string `json:"team_roster"`
	}
	if err := json.Unmarshal(delivery.Context, &identity); err != nil {
		t.Fatal(err)
	}
	if identity.YouAre["id"] != f.agent.ID || len(identity.TeamRoster) != 1 || identity.TeamRoster[0]["id"] != f.agent.ID {
		t.Fatalf("resume identity=%+v", identity)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: delivery.TurnID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{f.note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AckTurn(ctx, delivery.TurnID, f.agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, delivery.TurnID, f.agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"messages":[]}`)
	parsed, err := answer.Parse(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAnswer(ctx, AnswerInput{TurnID: delivery.TurnID, AgentID: f.agent.ID, Generation: 0, AnswerRev: 0, Digest: "resume-digest", Candidate: raw, Parsed: &parsed}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&state); err != nil || state != "done" {
		t.Fatalf("note after continuation=%q %v", state, err)
	}
}

func TestRecoveryAbandonPreservesDesiredAndRejectsStaleEvidence(t *testing.T) {
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
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(Event{Kind: "answer_held", AgentID: f.agent.ID, TurnID: f.turn.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "stale", "abandon", 0); err != ErrStaleInboxRev {
		t.Fatalf("new evidence decision=%v", err)
	}
	refreshed, err := s.InboxForUser(ctx, owner)
	if err != nil || len(refreshed) != 1 || refreshed[0].Rev != items[0].Rev+1 {
		t.Fatalf("refreshed item=%+v %v", refreshed, err)
	}
	decision, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "abandon", "abandon", int(refreshed[0].Rev))
	if err != nil || decision.Choice != "abandon" {
		t.Fatalf("abandon=%+v %v", decision, err)
	}
	var desired, state string
	var blocked bool
	if err := s.db.QueryRow(`SELECT desired,recovery_block FROM agent WHERE id=?`, f.agent.ID).Scan(&desired, &blocked); err != nil || desired != "active" || blocked {
		t.Fatalf("agent desired=%q blocked=%t %v", desired, blocked, err)
	}
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&state); err != nil || state != "aborted" {
		t.Fatalf("abandoned note=%q %v", state, err)
	}
	var saved string
	if err := s.db.QueryRow(`SELECT result_json FROM request_key WHERE key='abandon'`).Scan(&saved); err != nil || !json.Valid([]byte(saved)) {
		t.Fatalf("decision replay=%q %v", saved, err)
	}
}

func TestRecoveryDiagnosticReportAppearsInInbox(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, true)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 0 {
		t.Fatalf("premature recovery item=%+v %v", items, err)
	}
	diagnostic, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || diagnostic.Mode != "diagnostic" || diagnostic.ParentTurnID != f.turn.ID || len(diagnostic.AllowedTools) != 0 {
		t.Fatalf("diagnostic delivery=%+v ready=%t %v", diagnostic, ready, err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: diagnostic.TurnID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.AckTurn(ctx, diagnostic.TurnID, f.agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"messages":[],"reconciliation":{"status":"uncertain","findings":["external effect may have happened"]}}`)
	parsed, err := answer.Parse(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAnswer(ctx, AnswerInput{TurnID: diagnostic.TurnID, AgentID: f.agent.ID, Generation: 0, AnswerRev: 0, Digest: "diag-digest", Candidate: raw, Parsed: &parsed}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, diagnostic.TurnID, f.agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	items, err = s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].TurnID != f.turn.ID || !strings.Contains(string(items[0].DiagnosticReport), "external effect") {
		t.Fatalf("diagnostic recovery item=%+v %v", items, err)
	}
}

func TestRecoveryCleanDiagnosticAutoResumesOnlyWhenOptedIn(t *testing.T) {
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
		t.Fatalf("diagnostic ready=%t %v", ready, err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: diagnostic.TurnID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.AckTurn(ctx, diagnostic.TurnID, f.agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	raw := []byte("```json\r\n{\"messages\":[],\"reconciliation\":{\"status\":\"clean\",\"findings\":[]}}\r\n```")
	parsed, err := answer.Parse(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAnswer(ctx, AnswerInput{TurnID: diagnostic.TurnID, AgentID: f.agent.ID, Generation: 0, AnswerRev: 0, Digest: "clean-digest", Candidate: raw, Parsed: &parsed}); err != nil {
		t.Fatal(err)
	}
	var report string
	if err := s.db.QueryRow(`SELECT report_json FROM turn WHERE id=?`, diagnostic.TurnID).Scan(&report); err != nil || !json.Valid([]byte(report)) || strings.Contains(report, "```") {
		t.Fatalf("normalized diagnostic report=%q err=%v", report, err)
	}
	_, receiptJSON, err := s.ReceiptByTurn(ctx, diagnostic.TurnID, 0)
	if err != nil || !strings.Contains(receiptJSON, "```json") {
		t.Fatalf("raw diagnostic receipt=%q err=%v", receiptJSON, err)
	}
	if err := s.RecordProcess(ctx, diagnostic.TurnID, f.agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 0 {
		t.Fatalf("auto resume created item=%+v %v", items, err)
	}
	continuation, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || continuation.Mode != "normal" || len(continuation.Notes) != 1 || !strings.Contains(string(continuation.Context), "do not repeat") || strings.Contains(string(continuation.Context), "```") {
		t.Fatalf("auto continuation=%+v ready=%t %v", continuation, ready, err)
	}
	var actor string
	if err := s.db.QueryRow(`SELECT json_extract(payload_json,'$.actor') FROM event WHERE turn_id=? AND kind='recovery_decision'`, continuation.TurnID).Scan(&actor); err != nil || actor != "system (auto-resume setting)" {
		t.Fatalf("auto actor=%q %v", actor, err)
	}
}

func TestRecoveryCleanDiagnosticWaitsForOwnerByDefault(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, true)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	diagnostic, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || diagnostic.Mode != "diagnostic" {
		t.Fatalf("diagnostic=%+v ready=%t %v", diagnostic, ready, err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: diagnostic.TurnID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.AckTurn(ctx, diagnostic.TurnID, f.agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	raw := []byte("```\n{\"messages\":[],\"reconciliation\":{\"status\":\"clean\",\"findings\":[]}}\n```")
	parsed, err := answer.Parse(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAnswer(ctx, AnswerInput{TurnID: diagnostic.TurnID, AgentID: f.agent.ID, Generation: 0, AnswerRev: 0, Digest: "clean-default", Candidate: raw, Parsed: &parsed}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, diagnostic.TurnID, f.agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || !json.Valid(items[0].DiagnosticReport) || !strings.Contains(string(items[0].DiagnosticReport), `"clean"`) {
		t.Fatalf("clean diagnostic item=%+v %v", items, err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || ready {
		t.Fatalf("auto dispatch while opt-in off: ready=%t %v", ready, err)
	}
}

func TestRecoveryRetryDeliversRawNotes(t *testing.T) {
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
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "retry-key", "retry", 0); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || len(delivery.Notes) != 1 || delivery.Notes[0].NoteID != f.note.ID {
		t.Fatalf("retry=%+v ready=%t %v", delivery, ready, err)
	}
	if strings.Contains(string(delivery.Context), "recovery_evidence") {
		t.Fatalf("retry carried resume evidence: %s", delivery.Context)
	}
}

func TestBudgetDecisionContinueResetsWindow(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_budget','budget-owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_budget','owner')`, one.ProjectID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		turn, err := s.RecordTurn(ctx, Turn{AgentID: one.ID, SessionID: "ses_one", Generation: 0, RoomID: room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE turn SET exec='exited',outcome='completed',settle='settled' WHERE id=?`, turn.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "usr_budget", Body: "new work"}); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, one.ID, ""); err != nil || ready {
		t.Fatalf("budget ready=%t %v", ready, err)
	}
	items, err := s.InboxForUser(ctx, "usr_budget")
	if err != nil || len(items) != 1 || items[0].TurnID != "" || len(items[0].Options) != 2 || items[0].Options[0] != "Continue" {
		t.Fatalf("budget item=%+v %v", items, err)
	}
	decision, _, err := s.DecideRecovery(ctx, items[0].ID, "usr_budget", "budget-continue", "continue", 0)
	if err != nil || decision.TurnID != "" {
		t.Fatalf("continue=%+v %v", decision, err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, one.ID, ""); err != nil || !ready {
		t.Fatalf("continued budget ready=%t %v", ready, err)
	}
}

func TestRecoveryGateAfterUnreportedEffectDoesNotRunTwice(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	effectPath := filepath.Join(t.TempDir(), "effect.txt")
	gate := make(chan struct{})
	fake := adapter.NewFake()
	fake.Script(f.turn.ID,
		adapter.Step{Kind: adapter.StepEffect, FilePath: effectPath, Append: "effect\n"},
		adapter.Step{Kind: adapter.StepGate, Gate: gate},
		adapter.Step{Kind: adapter.StepCrash, Exit: adapter.ExitInfo{ExitCode: 137}},
	)
	turn, err := fake.Start(ctx, adapter.TurnSpec{TurnID: f.turn.ID, Mode: adapter.ModeNormal})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if contents, err := os.ReadFile(effectPath); err == nil && string(contents) == "effect\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake effect did not run")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case event := <-turn.Events():
		t.Fatalf("effect was reported before gate: %+v", event)
	default:
	}
	close(gate)
	for range turn.Events() {
	}
	exit, err := turn.Wait()
	if err != nil || exit.TerminalEvent || exit.ExitCode != 137 {
		t.Fatalf("fake crash=%+v %v", exit, err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, exit.ExitCode, exit.TerminalEvent, exit.CancelIntent); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || ready {
		t.Fatalf("unknown effect dispatched automatically: ready=%t %v", ready, err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].TurnOutcome != "unknown" {
		t.Fatalf("recovery item=%+v %v", items, err)
	}
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "effect-resume", "resume", 0); err != nil {
		t.Fatal(err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.TurnID == f.turn.ID || len(delivery.Notes) != 1 || !strings.Contains(string(delivery.Context), "do not repeat") {
		t.Fatalf("continuation=%+v ready=%t %v", delivery, ready, err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: delivery.TurnID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{f.note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AckTurn(ctx, delivery.TurnID, f.agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"messages":[]}`)
	fake.Script(delivery.TurnID,
		adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: raw}},
		adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}},
	)
	continued, err := fake.Start(ctx, adapter.TurnSpec{TurnID: delivery.TurnID, Mode: adapter.ModeNormal})
	if err != nil {
		t.Fatal(err)
	}
	for range continued.Events() {
	}
	continuedExit, err := continued.Wait()
	if err != nil || !continuedExit.TerminalEvent {
		t.Fatalf("continuation exit=%+v %v", continuedExit, err)
	}
	if err := s.RecordProcess(ctx, delivery.TurnID, f.agent.ID, 0, continuedExit.ExitCode, continuedExit.TerminalEvent, continuedExit.CancelIntent); err != nil {
		t.Fatal(err)
	}
	parsed, err := answer.Parse(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyAnswer(ctx, AnswerInput{TurnID: delivery.TurnID, AgentID: f.agent.ID, Generation: 0, AnswerRev: 0, Digest: "continued", Candidate: raw, Parsed: &parsed}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(effectPath)
	if err != nil || string(contents) != "effect\n" {
		t.Fatalf("side effect repeated: %q %v", contents, err)
	}
}

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/answer"
)

// applyOutcomeCandidate applies an answer to a turn dispatched to a supervisor that reports (require) or lacks supports_handoff_outcome:
// the dispatch records it on the turn (AdmitTurn), which the fixture's turn stands in for.
func applyOutcomeCandidate(t *testing.T, s *Store, f executionFixture, raw string, require bool) AnswerReceipt {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE turn SET handoff_outcome_required=? WHERE id=?`, require, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	return applyTurnOutcomeCandidate(t, s, f.turn, f.agent.ID, raw)
}

// applyTurnOutcomeCandidate applies an answer to a turn as its row stands: whether a handoff needs an outcome is the turn's.
func applyTurnOutcomeCandidate(t *testing.T, s *Store, turn Turn, agentID, raw string) AnswerReceipt {
	t.Helper()
	parsed, parseErr := answer.Parse([]byte(raw), false)
	sum := sha256.Sum256([]byte(raw))
	rev := 0
	if turn.Mode == "correction" {
		rev = 1
	}
	input := AnswerInput{TurnID: turn.ID, AgentID: agentID, Generation: turn.Generation, AnswerRev: rev, Digest: hex.EncodeToString(sum[:]),
		Candidate: []byte(raw)}
	if parseErr != nil {
		input.ParseError = parseErr.Error()
	} else {
		input.Parsed = &parsed
	}
	receipt, err := s.ApplyAnswer(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func storedOutcome(t *testing.T, s *Store, messageID string) (string, bool) {
	t.Helper()
	var outcome *string
	if err := s.db.QueryRow(`SELECT outcome FROM message WHERE id=?`, messageID).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome == nil {
		return "", false
	}
	return *outcome, true
}

// Each allowed outcome of a handoff is stored with its message and read back by the message list the API serves.
func TestHandoffOutcomeIsStoredAndListed(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"done", "partial", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			s, f := answerFixture(t, false)
			receipt := applyOutcomeCandidate(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"report","handoff":true,"outcome":%q}]}`, f.room.ID, outcome), true)
			if receipt.Result != "committed" || len(receipt.MessageIDs) != 1 {
				t.Fatalf("receipt=%+v", receipt)
			}
			if got, ok := storedOutcome(t, s, receipt.MessageIDs[0]); !ok || got != outcome {
				t.Fatalf("stored outcome=%q (%t), want %q", got, ok, outcome)
			}
			messages, err := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, message := range messages {
				if message.ID == receipt.MessageIDs[0] {
					found = message.Outcome == outcome
				}
			}
			if !found {
				t.Fatalf("listed messages do not carry outcome %q: %+v", outcome, messages)
			}
		})
	}
}

// From a supervisor that offers the field, a handoff without an outcome is not committed: the reply is held as invalid and its author is told,
// in a note it can act on, which values are allowed. With a correction turn available it gets one instead.
func TestHandoffWithoutOutcomeIsRejected(t *testing.T) {
	t.Parallel()
	const detail = "message 0 is a handoff without an outcome; set outcome to done, partial or failed"
	for _, raw := range []string{`{"messages":[{"room":%q,"body":"report","handoff":true}]}`, `{"messages":[{"room":%q,"body":"report","handoff":true,"outcome":null}]}`} {
		s, f := answerFixture(t, false)
		receipt := applyOutcomeCandidate(t, s, f, fmt.Sprintf(raw, f.room.ID), true)
		if receipt.Result != "held" || receipt.HeldReason != "correction_unavailable" || receipt.Error != detail || len(receipt.MessageIDs) != 0 {
			t.Fatalf("receipt=%+v", receipt)
		}
		var body string
		if err := s.db.QueryRow(`SELECT m.body FROM message m JOIN note n ON n.message_id=m.id WHERE m.author_kind='system' AND n.agent_id=? AND m.body LIKE ?`,
			f.agent.ID, heldReplyNotePrefix+"%").Scan(&body); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(body, detail) {
			t.Fatalf("held-reply note %q does not name the allowed outcomes", body)
		}
	}
	s, f := answerFixture(t, true)
	receipt := applyOutcomeCandidate(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"report","handoff":true}]}`, f.room.ID), true)
	if receipt.Result != "correction_requested" || receipt.Error != detail {
		t.Fatalf("no-action receipt=%+v", receipt)
	}
}

// An invalid value is refused by the parser before the store, with the allowed values named.
func TestHandoffWithInvalidOutcomeIsRejected(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	receipt := applyOutcomeCandidate(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"report","handoff":true,"outcome":"success"}]}`, f.room.ID), true)
	if receipt.Result != "held" || !strings.Contains(receipt.Error, "has an invalid outcome; use done, partial or failed") {
		t.Fatalf("receipt=%+v", receipt)
	}
	var messages int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE author_kind='agent' AND author_id=?`, f.agent.ID).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("agent messages=%d err=%v", messages, err)
	}
}

// A message that is not a handoff needs no outcome, and one it carries anyway is dropped (with a warning), never stored.
func TestNonHandoffNeedsNoOutcome(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	receipt := applyOutcomeCandidate(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"status"},{"room":%q,"body":"more","outcome":"failed"}]}`, f.room.ID, f.room.ID), true)
	if receipt.Result != "committed" || len(receipt.MessageIDs) != 2 {
		t.Fatalf("receipt=%+v", receipt)
	}
	for _, id := range receipt.MessageIDs {
		if got, ok := storedOutcome(t, s, id); ok {
			t.Fatalf("message %s stored outcome %q", id, got)
		}
	}
	if len(receipt.Warnings) != 1 || receipt.Warnings[0] != "message 1: outcome applies only to a handoff and was dropped" {
		t.Fatalf("warnings=%q", receipt.Warnings)
	}
}

// An agent whose supervisor predates outcomes cannot send one (its response schema has no such field), so its handoff is committed without
// one, stored as NULL like every handoff written before outcomes, and listed with no outcome.
func TestHandoffFromAnOlderSupervisorNeedsNoOutcome(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	receipt := applyOutcomeCandidate(t, s, f, fmt.Sprintf(`{"messages":[{"room":%q,"body":"report","handoff":true}]}`, f.room.ID), false)
	if receipt.Result != "committed" || len(receipt.MessageIDs) != 1 {
		t.Fatalf("receipt=%+v", receipt)
	}
	if got, ok := storedOutcome(t, s, receipt.MessageIDs[0]); ok {
		t.Fatalf("stored outcome %q, want NULL", got)
	}
	messages, err := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Outcome != "" {
			t.Fatalf("message %s lists outcome %q", message.ID, message.Outcome)
		}
	}
}

// The column takes only the three outcomes: a bad value written past the parser is refused by the database itself.
func TestMessageOutcomeColumnCheck(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	if _, err := s.db.Exec(`UPDATE message SET outcome='success' WHERE room_id=?`, f.room.ID); err == nil {
		t.Fatal("an outcome outside done, partial and failed was stored")
	}
	if _, err := s.db.Exec(`UPDATE message SET outcome='partial' WHERE room_id=?`, f.room.ID); err != nil {
		t.Fatal(err)
	}
}

// The lead a handoff wakes reads its outcome on the delivered note, in a new session's recent history, in room_history and in message_get.
func TestHandoffOutcomeReachesTheLead(t *testing.T) {
	t.Parallel()
	s, project, room, worker, lead := wakeFixture(t)
	ctx := context.Background()
	old, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: worker.ID, Body: "an older handoff", Handoff: true})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: worker.ID, Body: "could not do it", Handoff: true, Outcome: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if got := wakeReasons(t, s, failed.ID); got["lead"] != "handoff" {
		t.Fatalf("wakes=%v, want the lead woken by the handoff", got)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, lead.ID, "")
	if err != nil || !ready {
		t.Fatalf("prepare ready=%t err=%v", ready, err)
	}
	if err := s.CancelPreparedDispatch(ctx, delivery.TurnID); err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]string{}
	for _, note := range delivery.Notes {
		outcomes[note.MessageID] = note.Outcome
	}
	if outcomes[failed.ID] != "failed" || outcomes[old.ID] != "" {
		t.Fatalf("note outcomes=%v", outcomes)
	}
	var turnContext struct {
		History []AgentMessage `json:"recent_room_history"`
	}
	if err := json.Unmarshal(delivery.Context, &turnContext); err != nil {
		t.Fatal(err)
	}
	historyOutcome := map[int64]string{}
	for _, item := range turnContext.History {
		historyOutcome[item.No] = item.Outcome
	}
	if historyOutcome[failed.No] != "failed" || historyOutcome[old.No] != "" {
		t.Fatalf("recent_room_history outcomes=%v", historyOutcome)
	}
	page, err := s.AgentRoomHistory(ctx, lead.ID, project.ID, room.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	pageOutcome := map[int64]string{}
	for _, item := range page.Messages {
		pageOutcome[item.No] = item.Outcome
	}
	if pageOutcome[failed.No] != "failed" || pageOutcome[old.No] != "" {
		t.Fatalf("room_history outcomes=%v", pageOutcome)
	}
	got, err := s.AgentMessageByNo(ctx, lead.ID, project.ID, failed.No)
	if err != nil || got.Outcome != "failed" {
		t.Fatalf("message_get=%+v err=%v", got, err)
	}
}

// linkNote gives the fixture's turn its note, as PrepareDispatch does, so the turn can be admitted.
func linkNote(t *testing.T, s *Store, f executionFixture) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, f.turn.ID, f.note.ID); err != nil {
		t.Fatal(err)
	}
}

func turnRequiresOutcome(t *testing.T, s *Store, turnID string) bool {
	t.Helper()
	var required bool
	if err := s.db.QueryRow(`SELECT handoff_outcome_required FROM turn WHERE id=?`, turnID).Scan(&required); err != nil {
		t.Fatal(err)
	}
	return required
}

// The dispatch records the dispatching supervisor's supports_handoff_outcome on the turn, and the turn's answer is judged by it: an answer
// to a turn dispatched to an older supervisor (journaled there, replayed after a swap over a connection that supports outcomes) commits its
// handoff with a NULL outcome; a turn dispatched with the flag holds a handoff without one, as before. A new turn row requires nothing.
func TestHandoffOutcomeRequirementFollowsTheDispatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handoff := `{"messages":[{"room":%q,"body":"report","handoff":true}]}`
	for _, dispatched := range []bool{false, true} {
		s, f := answerFixture(t, false)
		if turnRequiresOutcome(t, s, f.turn.ID) {
			t.Fatal("a turn not yet dispatched requires an outcome")
		}
		linkNote(t, s, f)
		if err := s.AdmitTurn(ctx, Admission{TurnID: f.turn.ID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, NoteIDs: []string{f.note.ID},
			HandoffOutcomeRequired: dispatched}); err != nil {
			t.Fatal(err)
		}
		if got := turnRequiresOutcome(t, s, f.turn.ID); got != dispatched {
			t.Fatalf("dispatched with %t: the turn records %t", dispatched, got)
		}
		receipt := applyTurnOutcomeCandidate(t, s, f.turn, f.agent.ID, fmt.Sprintf(handoff, f.room.ID))
		if !dispatched {
			if receipt.Result != "committed" || len(receipt.MessageIDs) != 1 {
				t.Fatalf("an older supervisor's turn: receipt=%+v", receipt)
			}
			if got, ok := storedOutcome(t, s, receipt.MessageIDs[0]); ok {
				t.Fatalf("stored outcome %q, want NULL", got)
			}
			continue
		}
		if receipt.Result != "held" || !strings.Contains(receipt.Error, "handoff without an outcome") {
			t.Fatalf("a supporting supervisor's turn: receipt=%+v", receipt)
		}
	}
}

// A turn dispatched again takes the flag of the new dispatch: a correction turn (the continuation of a held answer) dispatched to a supervisor
// that supports outcomes requires one even though its parent was dispatched to one that did not, and the other way round.
func TestHandoffOutcomeRequirementOfAContinuation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, child := range []bool{true, false} {
		s, f := answerFixture(t, true)
		linkNote(t, s, f)
		if err := s.AdmitTurn(ctx, Admission{TurnID: f.turn.ID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, NoteIDs: []string{f.note.ID},
			HandoffOutcomeRequired: !child}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE turn SET exec='running' WHERE id=?`, f.turn.ID); err != nil {
			t.Fatal(err)
		}
		first := applyTurnOutcomeCandidate(t, s, f.turn, f.agent.ID, `{"messages":[`)
		if first.Result != "correction_requested" {
			t.Fatalf("first=%+v", first)
		}
		if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 0, true, false); err != nil {
			t.Fatal(err)
		}
		correction, err := s.CorrectionTurn(ctx, f.turn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if turnRequiresOutcome(t, s, correction.ID) {
			t.Fatal("a correction turn not yet dispatched requires an outcome")
		}
		if err := s.AdmitTurn(ctx, Admission{TurnID: correction.ID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, HandoffOutcomeRequired: child}); err != nil {
			t.Fatal(err)
		}
		receipt := applyTurnOutcomeCandidate(t, s, correction, f.agent.ID, fmt.Sprintf(`{"messages":[{"room":%q,"body":"report","handoff":true}]}`, f.room.ID))
		if child && (receipt.Result == "committed" || !strings.Contains(receipt.Error, "handoff without an outcome")) {
			t.Fatalf("a correction dispatched with the flag (parent without): receipt=%+v", receipt)
		}
		if !child && receipt.Result != "committed" {
			t.Fatalf("a correction dispatched without the flag (parent with): receipt=%+v", receipt)
		}
	}
}

// Migration 132 gives every turn row the safe default (no outcome required), on a fresh database and on one upgraded from 131 with turns in
// it, and the column takes only 0 and 1.
func TestHandoffOutcomeRequiredMigration(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	if turnRequiresOutcome(t, s, f.turn.ID) {
		t.Fatal("a fresh database's turn requires an outcome")
	}
	if _, err := s.db.Exec(`UPDATE turn SET handoff_outcome_required=2 WHERE id=?`, f.turn.ID); err == nil {
		t.Fatal("the column took 2")
	}
	for _, statement := range []string{`ALTER TABLE turn DROP COLUMN handoff_outcome_required`, `ALTER TABLE agent DROP COLUMN approved_by`, `DELETE FROM schema_version WHERE version>=132`} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(context.Background(), s.db); err != nil {
		t.Fatal(err)
	}
	if turnRequiresOutcome(t, s, f.turn.ID) {
		t.Fatal("an upgraded turn requires an outcome")
	}
	var version int
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 135 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/answer"
)

// applyToolCandidate applies an invalid answer to the fixture's turn, as a supervisor that reports (report set) or does not
// report (report nil) whether the turn was offered the submit_answer tool.
func applyToolCandidate(t *testing.T, s *Store, f executionFixture, raw string, report *bool) AnswerReceipt {
	t.Helper()
	parsed, parseErr := answer.Parse([]byte(raw), false)
	sum := sha256.Sum256([]byte(raw))
	input := AnswerInput{TurnID: f.turn.ID, AgentID: f.agent.ID, Generation: f.turn.Generation, AnswerRev: 0,
		Digest: hex.EncodeToString(sum[:]), Candidate: []byte(raw), AnswerTool: report}
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

// asLauncherAgent makes the fixture's agent a launcher agent of OpenCode: the store keeps no probed version for a launcher's
// harness, so the probed-version fallback can never name the tool for one.
func asLauncherAgent(t *testing.T, s *Store, f executionFixture) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO machine_launcher(id,machine_id,tool,name,command,enabled,containerized,supports_toolbox,preset)
		VALUES ('mln_test','mch_test','opencode','harness-docker','/bin/true',1,1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET tool='opencode',launcher_id='mln_test' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
}

// The held-reply repair note follows what the turn actually had: the supervisor reports on the answer frame whether the turn was
// offered the submit_answer tool, the turn keeps the fact, and the note names the tool exactly then — a launcher agent's turn
// included, whose harness version the server keeps no probe of.
func TestRepairNoteFollowsTheTurnsAnswerTool(t *testing.T) {
	t.Parallel()
	report := true
	s, f := answerFixture(t, false)
	asLauncherAgent(t, s, f)
	if receipt := applyToolCandidate(t, s, f, `{"messages":[{"room":"room_1","body":123}]}`, &report); receipt.Result != "held" {
		t.Fatalf("receipt=%+v", receipt)
	}
	note := heldNote(t, s, f.room.ID)
	if !strings.Contains(note.Body, "submit_answer") ||
		!strings.Contains(note.Body, "<json-schema>"+string(answer.ResponseSchema)+"</json-schema>") {
		t.Fatalf("a launcher turn with the tool must be asked for submit_answer: %q", note.Body)
	}
}

// A turn the report says had no tool keeps the schema-only note even when the machine's last probed version would say supported:
// the turn's fact wins over the probe.
func TestRepairNoteOmitsTheToolWhenTheTurnHadNone(t *testing.T) {
	t.Parallel()
	report := false
	s, f := answerFixture(t, false)
	setHarnessVersion(t, s, f.agent.ID, "opencode", "1.19.6")
	if receipt := applyToolCandidate(t, s, f, `{"messages":[{"room":"room_1","body":123}]}`, &report); receipt.Result != "held" {
		t.Fatalf("receipt=%+v", receipt)
	}
	note := heldNote(t, s, f.room.ID)
	if strings.Contains(note.Body, "submit_answer") {
		t.Fatalf("a turn without the tool must get the schema only: %q", note.Body)
	}
}

// An answer that carries no report — an older supervisor's — leaves the turn's column NULL and the note falls back to the probed
// version: a supported native harness still names the tool, a launcher agent still gets the schema only.
func TestRepairNoteWithoutTheReportKeepsTheProbedVersionFallback(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	setHarnessVersion(t, s, f.agent.ID, "opencode", "1.19.6")
	if receipt := applyToolCandidate(t, s, f, `{"messages":[{"room":"room_1","body":123}]}`, nil); receipt.Result != "held" {
		t.Fatalf("receipt=%+v", receipt)
	}
	if note := heldNote(t, s, f.room.ID); !strings.Contains(note.Body, "submit_answer") {
		t.Fatalf("a missing report must fall back to the probed version: %q", note.Body)
	}
	launched, fl := answerFixture(t, false)
	asLauncherAgent(t, launched, fl)
	if receipt := applyToolCandidate(t, launched, fl, `{"messages":[{"room":"room_1","body":123}]}`, nil); receipt.Result != "held" {
		t.Fatalf("receipt=%+v", receipt)
	}
	if note := heldNote(t, launched, fl.room.ID); strings.Contains(note.Body, "submit_answer") {
		t.Fatalf("a launcher agent without a report keeps the schema-only note: %q", note.Body)
	}
}

// The answer frame's report is stored on the turn: a committed answer keeps it, and a later held answer of the same turn (its
// correction) finds the parent untouched.
func TestAnswerToolFactIsStoredOnTheTurn(t *testing.T) {
	t.Parallel()
	report := true
	s, f := answerFixture(t, false)
	setHarnessVersion(t, s, f.agent.ID, "opencode", "1.19.6")
	if receipt := applyToolCandidate(t, s, f, `{"messages":[{"room":"room_1","body":123}]}`, &report); receipt.Result != "held" {
		t.Fatalf("receipt=%+v", receipt)
	}
	var stored sql.NullInt64
	if err := s.db.QueryRow(`SELECT answer_tool FROM turn WHERE id=?`, f.turn.ID).Scan(&stored); err != nil || !stored.Valid || stored.Int64 != 1 {
		t.Fatalf("stored answer_tool=%+v err=%v", stored, err)
	}
}

// Migration 145 gives every turn row an unknown answer tool (NULL), on a fresh database and on one upgraded with turns in it, and
// the column takes only 0, 1 and NULL. Replaying from 144 also replays the migrations after it, so the version rows from 144 on
// are deleted and the final version is whatever the binary ships.
func TestAnswerToolMigration(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	var tool sql.NullInt64
	if err := s.db.QueryRow(`SELECT answer_tool FROM turn WHERE id=?`, f.turn.ID).Scan(&tool); err != nil || tool.Valid {
		t.Fatalf("a fresh turn records answer_tool=%+v err=%v", tool, err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET answer_tool=2 WHERE id=?`, f.turn.ID); err == nil {
		t.Fatal("the column took 2")
	}
	for _, statement := range []string{`ALTER TABLE turn DROP COLUMN answer_tool`, `DELETE FROM schema_version WHERE version>=144`} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(context.Background(), s.db); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT answer_tool FROM turn WHERE id=?`, f.turn.ID).Scan(&tool); err != nil || tool.Valid {
		t.Fatalf("an upgraded turn records answer_tool=%+v err=%v", tool, err)
	}
	var version int
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != LatestSchemaVersion() {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

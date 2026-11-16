package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/sqliteutil"
)

func TestExitStderrReplaysAfterJournalReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.sqlite")
	j, err := OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordStart(ctx, StartRecord{TurnID: "turn-stderr", AgentID: "agent-1", Mode: "normal", SpecJSON: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordExitInfo(ctx, "turn-stderr", adapter.ExitInfo{ExitCode: 17, StderrTail: "Bearer aak_secret checkout is not mounted", ErrorEvent: `{"type":"turn.failed","message":"aak_secret invalid_grant"}`}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	exits, err := j.PendingExits(ctx)
	if err != nil || len(exits) != 1 || exits[0].Exit.ExitCode != 17 || !strings.Contains(exits[0].Exit.StderrTail, "checkout is not mounted") || strings.Contains(exits[0].Exit.StderrTail, "aak_secret") || !strings.Contains(exits[0].Exit.ErrorEvent, "invalid_grant") || strings.Contains(exits[0].Exit.ErrorEvent, "aak_secret") {
		t.Fatalf("replayed exits=%+v err=%v", exits, err)
	}
}

func TestNativeSessionEventReplaysUntilTerminalReceipt(t *testing.T) {
	ctx := context.Background()
	j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.RecordStart(ctx, StartRecord{TurnID: "turn-1", AgentID: "agent-1", Generation: 0,
		SpecJSON: json.RawMessage(`{}`), Mode: "normal"}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(adapter.Event{Kind: adapter.EventNativeSessionStarted, SourceEventID: "thread-source",
		NativeSessionHandle: "thread-1"})
	if err := j.RecordEvent(ctx, "turn-1", "thread-source", string(adapter.EventNativeSessionStarted), "", payload); err != nil {
		t.Fatal(err)
	}
	events, err := j.NativeSessionEvents(ctx)
	if err != nil || len(events) != 1 || events[0].Handle != "thread-1" || events[0].SourceEventID != "thread-source" {
		t.Fatalf("replay=%+v err=%v", events, err)
	}
	if _, err := j.RecordAnswer(ctx, "turn-1", 0, "digest", json.RawMessage(`{"messages":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordExit(ctx, "turn-1", 0, true, false); err != nil {
		t.Fatal(err)
	}
	if err := j.AcknowledgeExit(ctx, "turn-1"); err != nil {
		t.Fatal(err)
	}
	events, err = j.NativeSessionEvents(ctx)
	if err != nil || len(events) != 1 {
		t.Fatalf("answered turn awaiting receipt replay=%+v err=%v", events, err)
	}
	if err := j.RecordReceipt(ctx, protocol.ReceiptBody{TurnID: "turn-1", Digest: "digest", Result: "committed"}); err != nil {
		t.Fatal(err)
	}
	events, err = j.NativeSessionEvents(ctx)
	if err != nil || len(events) != 0 {
		t.Fatalf("terminal replay=%+v err=%v", events, err)
	}
}

func TestJournalDurableStartAndTerminalReceipt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "supervisor", "journal.sqlite")
	j, err := OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	journalID := j.ID
	if journalID == "" {
		t.Fatal("new journal has no identity")
	}
	var synchronous int
	if err := j.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("journal synchronous=%d %v", synchronous, err)
	}
	start := StartRecord{TurnID: "turn-1", AgentID: "agent-1", Generation: 2, DesiredRev: 5, NoteIDs: []string{"note-1"}, Mode: "normal", SpecJSON: json.RawMessage(`{"turn_id":"turn-1"}`)}
	if already, err := j.RecordStart(ctx, start); err != nil || already {
		t.Fatalf("start=%v %v", already, err)
	}
	if already, err := j.RecordStart(ctx, start); err != nil || !already {
		t.Fatalf("repeat start=%v %v", already, err)
	}
	changed := start
	changed.Generation = 3
	if _, err := j.RecordStart(ctx, changed); !errors.Is(err, ErrJournalConflict) {
		t.Fatalf("mismatched start=%v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if j.ID != journalID {
		t.Fatalf("journal identity changed on reopen: %q != %q", j.ID, journalID)
	}
	open, err := j.OpenTurns(ctx)
	if err != nil || len(open) != 1 || len(open[0].OpenTurns) != 1 || open[0].OpenTurns[0].HasAnswer {
		t.Fatalf("reopened start=%+v %v", open, err)
	}
	if err := j.RecordEvent(ctx, "turn-1", "source-1", "activity", "effect", json.RawMessage(`{"path":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordExit(ctx, "turn-1", 0, true, false); err != nil {
		t.Fatal(err)
	}
	answer := json.RawMessage(`{"messages":[]}`)
	if already, err := j.RecordAnswer(ctx, "turn-1", 0, "digest-1", answer); err != nil || already {
		t.Fatalf("answer=%v %v", already, err)
	}
	open, err = j.OpenTurns(ctx)
	if err != nil || !open[0].OpenTurns[0].HasAnswer || open[0].OpenTurns[0].Digest != "digest-1" {
		t.Fatalf("pending receipt=%+v %v", open, err)
	}
	if err := j.RecordReceipt(ctx, protocol.ReceiptBody{TurnID: "turn-1", AnswerRev: 0, Digest: "wrong", Result: "committed"}); !errors.Is(err, ErrJournalConflict) {
		t.Fatalf("wrong receipt=%v", err)
	}
	if err := j.RecordReceipt(ctx, protocol.ReceiptBody{TurnID: "turn-1", AnswerRev: 0, Digest: "digest-1", Result: "committed"}); err != nil {
		t.Fatal(err)
	}
	open, err = j.OpenTurns(ctx)
	if err != nil || len(open) != 1 { // Exit remains visible until server acknowledgement.
		t.Fatalf("unacknowledged exit inventory=%+v %v", open, err)
	}
	exits, err := j.PendingExits(ctx)
	if err != nil || len(exits) != 1 {
		t.Fatalf("pending exits=%+v %v", exits, err)
	}
	if err := j.AcknowledgeExit(ctx, "turn-1"); err != nil {
		t.Fatal(err)
	}
	exits, err = j.PendingExits(ctx)
	if err != nil || len(exits) != 0 {
		t.Fatalf("acknowledged exits=%+v %v", exits, err)
	}
	open, err = j.OpenTurns(ctx)
	if err != nil || len(open) != 0 {
		t.Fatalf("terminal inventory=%+v %v", open, err)
	}
	var candidate []byte
	if err := j.db.QueryRowContext(ctx, `SELECT candidate FROM answer WHERE turn_id='turn-1'`).Scan(&candidate); err != nil || candidate != nil {
		t.Fatalf("candidate not pruned: %q %v", candidate, err)
	}
	var starts, receipts int
	if err := j.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn_start`).Scan(&starts); err != nil {
		t.Fatal(err)
	}
	if err := j.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM receipt`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if starts != 1 || receipts != 1 {
		t.Fatalf("tombstones: starts=%d receipts=%d", starts, receipts)
	}
}

func TestJournalRejectsConcurrentSupervisorOwner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "supervisor", "journal.sqlite")
	first, err := OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(ctx, path); !errors.Is(err, ErrJournalInUse) || !strings.Contains(err.Error(), "pid ") || !strings.Contains(err.Error(), "AIMEBU2_EMBEDDED_SUPERVISOR=0") {
		t.Fatalf("second supervisor opened same journal: %v", err)
	}
	if info, err := os.Stat(path + ".lock"); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("journal lock permissions=%v err=%v", info, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(ctx, path)
	if err != nil {
		t.Fatalf("journal stayed locked after exit: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJournalReplacementGetsNewIdentity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first, err := OpenJournal(ctx, filepath.Join(dir, "first.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	firstID := first.ID
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := OpenJournal(ctx, filepath.Join(dir, "replacement.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if replacement.ID == firstID {
		t.Fatal("replacement reused journal identity")
	}
}

func TestExistingJournalGainsExitAcknowledgementColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.sqlite")
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE turn_start(turn_id TEXT PRIMARY KEY,agent_id TEXT NOT NULL,generation INTEGER NOT NULL,
		desired_rev INTEGER NOT NULL,note_ids_json TEXT NOT NULL,mode TEXT NOT NULL,spec_json BLOB NOT NULL,
		started_at TEXT NOT NULL,exec TEXT NOT NULL DEFAULT 'running',exit_code INTEGER,terminal_event INTEGER,cancel_intent INTEGER)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordStart(context.Background(), StartRecord{TurnID: "old-turn", AgentID: "agent", Mode: "normal", SpecJSON: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordExit(context.Background(), "old-turn", 0, true, false); err != nil {
		t.Fatal(err)
	}
	if err := j.AcknowledgeExit(context.Background(), "old-turn"); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	exits, err := reopened.PendingExits(context.Background())
	if err != nil || len(exits) != 0 {
		t.Fatalf("exit replayed after reopen: %+v %v", exits, err)
	}
}

func TestCorrectionRequestedReceiptBecomesSuperseded(t *testing.T) {
	ctx := context.Background()
	j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	start := StartRecord{TurnID: "turn-parent", AgentID: "agent-1", Mode: "normal", SpecJSON: json.RawMessage(`{}`)}
	if _, err := j.RecordStart(ctx, start); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordAnswer(ctx, start.TurnID, 0, "digest", json.RawMessage(`{"messages":[]}`)); err != nil {
		t.Fatal(err)
	}
	for _, result := range []string{"correction_requested", "superseded"} {
		if err := j.RecordReceipt(ctx, protocol.ReceiptBody{TurnID: start.TurnID, AnswerRev: 0, Digest: "digest", Result: result}); err != nil {
			t.Fatal(err)
		}
	}
	var result string
	var terminal bool
	if err := j.db.QueryRowContext(ctx, `SELECT result,terminal FROM receipt WHERE turn_id=?`, start.TurnID).Scan(&result, &terminal); err != nil {
		t.Fatal(err)
	}
	if result != "superseded" || !terminal {
		t.Fatalf("receipt=%s terminal=%t", result, terminal)
	}
	var candidate []byte
	if err := j.db.QueryRowContext(ctx, `SELECT candidate FROM answer WHERE turn_id=?`, start.TurnID).Scan(&candidate); err != nil || candidate != nil {
		t.Fatalf("candidate not pruned: %q %v", candidate, err)
	}
}

func TestAcknowledgedAnswerlessExitLeavesHelloInventory(t *testing.T) {
	ctx := context.Background()
	j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	for _, id := range []string{"turn-crash", "turn-answered"} {
		if _, err := j.RecordStart(ctx, StartRecord{TurnID: id, AgentID: "agent-1", Mode: "normal", SpecJSON: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if err := j.RecordExit(ctx, id, 1, false, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := j.RecordAnswer(ctx, "turn-answered", 0, "digest", json.RawMessage(`{"messages":[]}`)); err != nil {
		t.Fatal(err)
	}
	open, err := j.OpenTurns(ctx)
	if err != nil || len(open) != 1 || len(open[0].OpenTurns) != 2 {
		t.Fatalf("unacknowledged exits must stay in the inventory: %+v %v", open, err)
	}
	for _, id := range []string{"turn-crash", "turn-answered"} {
		if err := j.AcknowledgeExit(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	open, err = j.OpenTurns(ctx)
	if err != nil || len(open) != 1 || len(open[0].OpenTurns) != 1 || open[0].OpenTurns[0].TurnID != "turn-answered" {
		t.Fatalf("an acknowledged answerless exit stayed open, or an answered turn awaiting its receipt left: %+v %v", open, err)
	}
	if err := j.RecordReceipt(ctx, protocol.ReceiptBody{TurnID: "turn-answered", Digest: "digest", Result: "committed"}); err != nil {
		t.Fatal(err)
	}
	if open, err = j.OpenTurns(ctx); err != nil || len(open) != 0 {
		t.Fatalf("settled inventory=%+v %v", open, err)
	}
}

func TestUsageEventsReplayUntilExitAcknowledged(t *testing.T) {
	ctx := context.Background()
	j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.RecordStart(ctx, StartRecord{TurnID: "turn-1", AgentID: "agent-1", Generation: 2, Mode: "normal", SpecJSON: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	cost := 0.5
	for _, event := range []adapter.Event{
		{Kind: adapter.EventActivity, SourceEventID: "a"},
		{Kind: adapter.EventUsage, SourceEventID: "u", Usage: &adapter.Usage{CostEstimate: &cost}},
		{Kind: adapter.EventCompaction, SourceEventID: "c", Compaction: &adapter.Compaction{PreTokens: 9, PostTokens: 1}},
	} {
		payload, _ := json.Marshal(event)
		if err := j.RecordEvent(ctx, "turn-1", event.SourceEventID, string(event.Kind), event.Summary, payload); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want int) {
		t.Helper()
		events, err := j.PendingUsageEvents(ctx)
		if err != nil || len(events) != want {
			t.Fatalf("pending usage=%+v %v, want %d", events, err, want)
		}
		for _, event := range events {
			if event.AgentID != "agent-1" || event.Generation != 2 || event.Event.Kind == adapter.EventActivity {
				t.Fatalf("pending usage event=%+v", event)
			}
		}
	}
	check(2)
	if err := j.RecordExit(ctx, "turn-1", 0, true, false); err != nil {
		t.Fatal(err)
	}
	check(2)
	if err := j.AcknowledgeExit(ctx, "turn-1"); err != nil {
		t.Fatal(err)
	}
	check(0)
}

// The sentence a harness gave and the verdict that the turn failed before it did anything are kept with the exit until the server has
// acknowledged it, so a reconnect resends them rather than losing them.
func TestExitKeepsTheErrorSummaryAndDefiniteVerdictUntilAcknowledged(t *testing.T) {
	ctx := context.Background()
	j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.RecordStart(ctx, StartRecord{TurnID: "turn-definite", AgentID: "agent", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordExitInfo(ctx, "turn-definite", adapter.ExitInfo{ExitCode: 1, ErrorEvent: `{"is_error":true}`, ErrorSummary: "Failed to authenticate", DefiniteFailure: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordStart(ctx, StartRecord{TurnID: "turn-plain", AgentID: "agent2", Mode: "normal", SpecJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordExitInfo(ctx, "turn-plain", adapter.ExitInfo{ExitCode: 1, StderrTail: "boom"}); err != nil {
		t.Fatal(err)
	}
	pending, err := j.PendingExits(ctx)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	byTurn := map[string]adapter.ExitInfo{}
	for _, exit := range pending {
		byTurn[exit.TurnID] = exit.Exit
	}
	if got := byTurn["turn-definite"]; got.ErrorSummary != "Failed to authenticate" || !got.DefiniteFailure || got.ErrorEvent != `{"is_error":true}` {
		t.Fatalf("the definite exit: %+v", got)
	}
	if got := byTurn["turn-plain"]; got.ErrorSummary != "" || got.DefiniteFailure || got.StderrTail != "boom" {
		t.Fatalf("an ordinary exit gained something: %+v", got)
	}
}

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func pruneJournal(t *testing.T) (*Journal, context.Context) {
	t.Helper()
	ctx := context.Background()
	j, err := OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j, ctx
}

func bigSpec() json.RawMessage {
	return json.RawMessage(`{"pad":"` + strings.Repeat("x", 8000) + `"}`)
}

func startTurn(t *testing.T, j *Journal, ctx context.Context, id string, events int) {
	t.Helper()
	if _, err := j.RecordStart(ctx, StartRecord{TurnID: id, AgentID: "agent", Generation: 1, DesiredRev: 3, Mode: "normal", SpecJSON: bigSpec()}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < events; i++ {
		if err := j.RecordEvent(ctx, id, fmt.Sprintf("%s-ev%d", id, i), "activity", "work", json.RawMessage(`{"n":1}`)); err != nil {
			t.Fatal(err)
		}
	}
}

func settle(t *testing.T, j *Journal, ctx context.Context, id string) {
	t.Helper()
	if err := j.RecordExit(ctx, id, 0, true, false); err != nil {
		t.Fatal(err)
	}
	if err := j.AcknowledgeExit(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, j *Journal, query string, args ...any) int {
	t.Helper()
	var n int
	if err := j.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// everything a turn kind of row is, to check the pruner leaves it as it was.
func snapshot(t *testing.T, j *Journal, ctx context.Context, id string) string {
	t.Helper()
	var row string
	if err := j.db.QueryRow(`SELECT agent_id||'/'||generation||'/'||desired_rev||'/'||exec||'/'||exit_acked||'/'||length(spec_json)||'/'||(SELECT count(*) FROM journal_event WHERE turn_id=turn_start.turn_id) FROM turn_start WHERE turn_id=?`, id).Scan(&row); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestPruneSettledDropsHeavyDataOfSettledTurnsOnly(t *testing.T) {
	j, ctx := pruneJournal(t)
	// settled without an answer; settled with a terminally receipted answer
	startTurn(t, j, ctx, "plain", 3)
	settle(t, j, ctx, "plain")
	startTurn(t, j, ctx, "answered", 2)
	settle(t, j, ctx, "answered")
	if _, err := j.RecordAnswer(ctx, "answered", 0, "d", json.RawMessage(`{"m":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordReceipt(ctx, protocol.ReceiptBody{TurnID: "answered", Digest: "d", Result: "committed"}); err != nil {
		t.Fatal(err)
	}
	// never to be touched
	startTurn(t, j, ctx, "running", 2)
	startTurn(t, j, ctx, "unacked", 2)
	if err := j.RecordExit(ctx, "unacked", 0, true, false); err != nil {
		t.Fatal(err)
	}
	startTurn(t, j, ctx, "pending-answer", 2)
	settle(t, j, ctx, "pending-answer")
	if _, err := j.RecordAnswer(ctx, "pending-answer", 0, "d", json.RawMessage(`{"m":1}`)); err != nil {
		t.Fatal(err)
	}
	startTurn(t, j, ctx, "awaiting-receipt", 2) // answered, candidate already null by a non-terminal... kept: no terminal receipt
	settle(t, j, ctx, "awaiting-receipt")
	if _, err := j.RecordAnswer(ctx, "awaiting-receipt", 0, "d", json.RawMessage(`{"m":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordReceipt(ctx, protocol.ReceiptBody{TurnID: "awaiting-receipt", Digest: "d", Result: "correction_requested"}); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordWarmProcess(ctx, WarmProcess{PID: 4242, AgentID: "agent", Started: "s"}); err != nil {
		t.Fatal(err)
	}
	keep := map[string]string{}
	for _, id := range []string{"running", "unacked", "pending-answer", "awaiting-receipt"} {
		keep[id] = snapshot(t, j, ctx, id)
	}
	owesBefore, debtBefore, openBefore, leftBefore := j.Owes(ctx), mustDebt(t, j, ctx), mustOpen(t, j, ctx), j.leftOwed(ctx)
	eventsBefore := count(t, j, `SELECT count(*) FROM journal_event`)
	specBefore := count(t, j, `SELECT sum(length(spec_json)) FROM turn_start WHERE turn_id IN ('plain','answered')`)
	if eventsBefore != 13 {
		t.Fatalf("setup: events=%d", eventsBefore)
	}

	// within the grace nothing goes
	if r, err := j.PruneSettled(ctx, time.Hour); err != nil || r != (PruneResult{}) {
		t.Fatalf("pruned inside the grace: %+v %v", r, err)
	}
	r, err := j.PruneSettled(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Turns != 2 || r.Events != 5 {
		t.Fatalf("pruned %+v, want 2 turns and 5 events", r)
	}
	t.Logf("before: %d events, spec bytes %d; after: %d events, spec bytes %d", eventsBefore, specBefore, count(t, j, `SELECT count(*) FROM journal_event`), count(t, j, `SELECT sum(length(spec_json)) FROM turn_start WHERE turn_id IN ('plain','answered')`))
	for _, id := range []string{"plain", "answered"} {
		if n := count(t, j, `SELECT count(*) FROM journal_event WHERE turn_id=?`, id); n != 0 {
			t.Fatalf("%s kept %d events", id, n)
		}
		if n := count(t, j, `SELECT length(spec_json) FROM turn_start WHERE turn_id=?`, id); n != 0 {
			t.Fatalf("%s kept its spec", id)
		}
		if has, err := j.HasStart(ctx, id); err != nil || !has {
			t.Fatalf("the thin row of %s must stay: %v %v", id, has, err)
		}
	}
	for id, was := range keep {
		if now := snapshot(t, j, ctx, id); now != was {
			t.Fatalf("%s changed: %q -> %q", id, was, now)
		}
	}
	if n := count(t, j, `SELECT count(*) FROM warm_process`); n != 1 {
		t.Fatalf("warm process touched: %d", n)
	}
	if j.Owes(ctx) != owesBefore || j.leftOwed(ctx) != leftBefore || mustDebt(t, j, ctx) != debtBefore || fmt.Sprint(mustOpen(t, j, ctx)) != fmt.Sprint(openBefore) {
		t.Fatalf("pruning changed what the journal owes: %+v -> %+v", debtBefore, mustDebt(t, j, ctx))
	}
	// idempotent
	if r, err := j.PruneSettled(ctx, 0); err != nil || r != (PruneResult{}) {
		t.Fatalf("second prune: %+v %v", r, err)
	}
}

func mustDebt(t *testing.T, j *Journal, ctx context.Context) JournalDebt {
	t.Helper()
	d, err := j.Debt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustOpen(t *testing.T, j *Journal, ctx context.Context) any {
	t.Helper()
	o, err := j.OpenTurns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestPrunedTurnReplayBehavesAsForASettledTurn(t *testing.T) {
	j, ctx := pruneJournal(t)
	startTurn(t, j, ctx, "old", 1)
	settle(t, j, ctx, "old")
	if _, err := j.PruneSettled(ctx, 0); err != nil {
		t.Fatal(err)
	}
	same := StartRecord{TurnID: "old", AgentID: "agent", Generation: 1, DesiredRev: 3, Mode: "normal", SpecJSON: bigSpec()}
	if already, err := j.RecordStart(ctx, same); err != nil || !already {
		t.Fatalf("a replayed start of a pruned turn: already=%v err=%v", already, err)
	}
	other := same
	other.Generation = 2
	if _, err := j.RecordStart(ctx, other); !errors.Is(err, ErrJournalConflict) {
		t.Fatalf("a replay with other identity must conflict: %v", err)
	}
	if spec, err := j.TurnSpecJSON(ctx, "old"); err != nil || len(spec) != 0 {
		t.Fatalf("spec=%q %v", spec, err)
	}
	if n := count(t, j, `SELECT count(*) FROM turn_start`); n != 1 {
		t.Fatalf("a replay created a row: %d", n)
	}
}

func TestPruneSettledCommandsKeepsPendingAndOpen(t *testing.T) {
	j, ctx := pruneJournal(t)
	for _, id := range []string{"acked", "unacked", "open"} {
		if _, err := j.RecordCommandStart(ctx, CommandStartRecord{JobID: id, AgentID: "agent", Pid: 1, PidStart: "p", Cmd: "echo " + id, LogPath: "/l", StartedAt: "2026-01-01T00:00:00.000000000Z"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"acked", "unacked"} {
		if err := j.RecordCommandExit(ctx, protocol.CommandExit{JobID: id, StartedAt: "2026-01-01T00:00:00.000000000Z", EndedAt: "2026-01-01T00:00:01.000000000Z", Tail: "output"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.AcknowledgeCommandExit(ctx, "acked"); err != nil {
		t.Fatal(err)
	}
	debt := mustDebt(t, j, ctx)
	r, err := j.PruneSettled(ctx, time.Hour)
	if err != nil || r.Commands != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if n := count(t, j, `SELECT count(*) FROM command_exit e JOIN command_start s USING(job_id) WHERE job_id='acked' AND tail='' AND cmd=''`); n != 1 {
		t.Fatal("the acknowledged command kept its heavy data")
	}
	if n := count(t, j, `SELECT count(*) FROM command_start WHERE job_id IN ('unacked','open') AND cmd<>''`); n != 2 {
		t.Fatal("a pending or open command was touched")
	}
	if n := count(t, j, `SELECT count(*) FROM command_exit WHERE job_id='unacked' AND tail='output'`); n != 1 {
		t.Fatal("a pending command exit lost its tail")
	}
	if mustDebt(t, j, ctx) != debt {
		t.Fatal("pruning changed the debt")
	}
	if ended, err := j.HasCommandExit(ctx, "acked"); err != nil || !ended {
		t.Fatal("the pruned command no longer reads as ended")
	}
	if already, err := j.RecordCommandStart(ctx, CommandStartRecord{JobID: "acked", AgentID: "agent"}); err != nil || !already {
		t.Fatalf("a replayed start of a pruned command: %v %v", already, err)
	}
}

func TestPruneSettledRunsInBoundedBatches(t *testing.T) {
	j, ctx := pruneJournal(t)
	const bound = 5
	total := bound*2 + 3
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("t%03d", i)
		startTurn(t, j, ctx, id, 1)
		settle(t, j, ctx, id)
	}
	r, err := j.PruneBatch(ctx, 0, bound)
	if err != nil || r.Turns != bound || r.Events != bound {
		t.Fatalf("one batch must handle exactly the bound: %+v %v", r, err)
	}
	if left := count(t, j, `SELECT count(*) FROM journal_event`); left != total-bound {
		t.Fatalf("left=%d", left)
	}
	r, err = j.pruneSettled(ctx, 0, bound)
	if err != nil || r.Turns != total-bound || count(t, j, `SELECT count(*) FROM journal_event`) != 0 || count(t, j, `SELECT count(*) FROM turn_start`) != total {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestShouldVacuum(t *testing.T) {
	const mb = 1 << 20
	for _, tc := range []struct {
		free, pages, size int64
		want              bool
	}{
		{0, 1000, 4096, false},
		{20 * mb / 4096, 40 * mb / 4096, 4096, false},    // 50% free but only 20 MB
		{60 * mb / 4096, 400 * mb / 4096, 4096, false},   // 60 MB but 15%
		{60 * mb / 4096, 200 * mb / 4096, 4096, true},    // 60 MB and 30%
		{51 * mb / 4096, 51 * mb / 4096 * 2, 4096, true}, // just over both
		{10, 0, 4096, false},
	} {
		if got := ShouldVacuum(tc.free, tc.pages, tc.size); got != tc.want {
			t.Errorf("ShouldVacuum(%d,%d,%d)=%v want %v", tc.free, tc.pages, tc.size, got, tc.want)
		}
	}
}

func TestVacuumIfWorthItWaitsWhileTheJournalOwesAndRunsWhenBloated(t *testing.T) {
	j, ctx := pruneJournal(t)
	// a small journal is never worth a rebuild
	if ran, err := j.VacuumIfWorthIt(ctx); err != nil || ran {
		t.Fatalf("small journal: %v %v", ran, err)
	}
	// bloat it: 120 MB of events, then drop them so more than 50 MB and 25% is free
	startTurn(t, j, ctx, "big", 0)
	blob := strings.Repeat("y", 1<<20)
	for i := 0; i < 70; i++ {
		if _, err := j.db.ExecContext(ctx, `INSERT INTO journal_event(source_event_id,turn_id,kind,summary,payload_json,created_at) VALUES (?,?,?,?,?,?)`, fmt.Sprint("b", i), "big", "activity", "", []byte(blob), timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := j.db.ExecContext(ctx, `DELETE FROM journal_event`); err != nil {
		t.Fatal(err)
	}
	// the turn is still running: the journal owes, so no rebuild
	if ran, err := j.VacuumIfWorthIt(ctx); err != nil || ran {
		t.Fatalf("vacuumed while a turn runs: %v %v", ran, err)
	}
	settle(t, j, ctx, "big")
	ran, err := j.VacuumIfWorthIt(ctx)
	if err != nil || !ran {
		t.Fatalf("a bloated, idle journal must be rebuilt: %v %v", ran, err)
	}
	var free int64
	if err := j.db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil || free != 0 {
		t.Fatalf("free pages after vacuum: %d %v", free, err)
	}
}

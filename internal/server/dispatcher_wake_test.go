package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

// A committed change that can create dispatch work wakes the pass, so a new
// message to an idle agent is dispatched without waiting for the fallback
// tick: the tick here is an hour, so only the wake can deliver inside 100 ms.
// Keep this timing assertion serial within the server package: unrelated
// parallel fixtures can delay its goroutine under the race detector.
func TestWakeDispatchesANewMessageInside100ms(t *testing.T) {
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_wake','worker','Worker','worker','worker')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_wake',?,?,?)`, f.project.ID, f.machineID, "/tmp/wake")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := f.db.CreateAgent(ctx, store.Agent{Slug: "wake", ProjectID: f.project.ID, RoleID: "role_wake", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.machineID, CheckoutID: "chk_wake", KeyHash: "key", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(ctx, f.room.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_wake',?,0,'fresh_with_history','2026-11-24T00:00:00.000000000Z')`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	journal, err := supervisor.OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	fake := adapter.NewFake()
	fake.ScriptDefault(adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(`{"messages":[]}`)}}, adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, fake)
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(runCtx) }()
	select {
	case <-ready:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	dispatcher := NewDispatcher(f.srv)
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		dispatcher.Run(runCtx, time.Hour, nil)
	}()
	// Let the startup sweeps and their passes finish, so the message below is
	// the only thing left to do and nothing else can wake the pass.
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	response := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "please wake"}, true)
	if response.StatusCode != 201 {
		t.Fatalf("post status=%d", response.StatusCode)
	}
	var message struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&message); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	for {
		note, err := f.db.NoteForAgentMessage(ctx, agent.ID, message.ID)
		if err != nil {
			t.Fatalf("note for message: %v", err)
		}
		if note.State != "queued" {
			break
		}
		if time.Since(start) > 100*time.Millisecond {
			t.Fatalf("message still queued after %s (note state %q): the wake did not dispatch it", time.Since(start), note.State)
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-dispatchDone
	<-done
}

// An idle server passes at most about once per fallback interval: writes wake
// it, but nothing writes while it is idle, so the counter crawls with the
// fallback tick instead of the old 100 ms busy pass.
func TestIdleServerPassesAtTheFallbackCadence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	dispatcher := NewDispatcher(f.srv)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		dispatcher.Run(runCtx, 250*time.Millisecond, nil)
	}()
	// The startup sweep passes first; count only after they are done.
	time.Sleep(500 * time.Millisecond)
	before := dispatcher.passes.Load()
	time.Sleep(1 * time.Second)
	after := dispatcher.passes.Load()
	// One pass per 250 ms fallback tick, plus slack for the pass that was
	// running when the window opened and the one its own writes woke.
	if ran := after - before; ran > 8 {
		t.Fatalf("idle server ran %d passes in 1 s (fallback 250 ms would run 4): it is not driven by the fallback tick alone", ran)
	}
	if ran := after - before; ran < 2 {
		t.Fatalf("idle server ran %d passes in 1 s: the fallback tick did not run", ran)
	}
	// With no team-flow change under way the pass skips the step entirely.
	if runs := f.db.FlowMigrationAdvanceRuns(); runs != 0 {
		t.Fatalf("idle server advanced team flow migrations %d times", runs)
	}
	if f.db.TeamFlowMigrationBusy() {
		t.Fatal("no team flow change is under way, but the store says one is")
	}
	cancel()
	<-dispatchDone
}

// An idle server commits no write transaction: no pass step, sweep, or
// keep-alive bookkeeping may write when nothing changed. The counter sees the
// transactions the store commits; the unchanged WAL file sees any other write.
func TestIdleServerCommitsNoWrites(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	dispatcher := NewDispatcher(f.srv)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		dispatcher.Run(runCtx, 2*time.Second, nil)
	}()
	// Let the startup sweeps and their passes finish before counting.
	time.Sleep(2500 * time.Millisecond)
	before := f.db.WriteCommits()
	walBefore := walBytes(t, f.db.DBPath())
	// Long enough for the ten-second sweeps to have run inside the window.
	time.Sleep(12 * time.Second)
	if after := f.db.WriteCommits(); after != before {
		t.Fatalf("an idle server committed %d write transactions in 12 s", after-before)
	}
	if walAfter := walBytes(t, f.db.DBPath()); walAfter != walBefore {
		t.Fatalf("the WAL grew by %d bytes while the server was idle", walAfter-walBefore)
	}
	cancel()
	<-dispatchDone
}

func walBytes(t *testing.T, dbPath string) int64 {
	t.Helper()
	info, err := os.Stat(dbPath + "-wal")
	if err != nil {
		return 0
	}
	return info.Size()
}

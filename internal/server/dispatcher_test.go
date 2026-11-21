package server

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

func TestDispatcherBackoffAndStaleSpecialFlight(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		failures int
		want     time.Duration
	}{{1, 200 * time.Millisecond}, {2, 400 * time.Millisecond}, {8, 25600 * time.Millisecond}, {9, 25600 * time.Millisecond}} {
		if got := dispatchBackoff(test.failures); got != test.want {
			t.Fatalf("failures=%d backoff=%s want=%s", test.failures, got, test.want)
		}
	}
	f := newFixture(t)
	d := NewDispatcher(f.srv)
	d.inFlight["missing"] = specialFlight{sentAt: time.Now()}
	if _, err := d.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.inFlight) != 0 {
		t.Fatalf("stale flight retained: %+v", d.inFlight)
	}
}

func TestThirdPreAdmissionFailureKeepsNormalContinuationPending(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, note, _ := f.execution(t)
	ctx := context.Background()
	if err := f.db.AdmitTurn(ctx, store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: f.machineID,
		Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckTurn(ctx, turn.ID, agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordProcess(ctx, turn.ID, agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	me := f.request("GET", "/api/me", nil, false)
	var user struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(me.Body).Decode(&user); err != nil {
		t.Fatal(err)
	}
	me.Body.Close()
	items, err := f.db.InboxForUser(ctx, user.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("recovery items=%+v %v", items, err)
	}
	decision, _, err := f.db.DecideRecovery(ctx, items[0].ID, user.ID, "dispatcher-continuation", "resume", int(items[0].Rev))
	if err != nil || decision.TurnID == "" {
		t.Fatalf("recovery decision=%+v %v", decision, err)
	}
	conn := f.dialMachine(t, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	defer conn.Close(1000, "")
	f.srv.machineMu.Lock()
	machine := f.srv.machines[f.machineID]
	f.srv.machineMu.Unlock()
	machine.boundMu.Lock()
	machine.bound[agent.ID] = 1 // Force a pre-admission stale-generation rejection.
	machine.boundMu.Unlock()
	d := NewDispatcher(f.srv)
	d.failures[agent.ID] = dispatchFailure{count: 2}
	if count, err := d.Pass(ctx); err != nil || count != 0 {
		t.Fatalf("third failure pass=%d %v", count, err)
	}
	state, err := f.db.DeliveryState(ctx, decision.TurnID)
	if err != nil || state.Exec != "pending" || d.failures[agent.ID].count != 3 {
		t.Fatalf("continuation=%+v failures=%+v %v", state, d.failures[agent.ID], err)
	}
}

func TestFakeDispatcherWakesHumanRoomAndTargetedAgent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_dispatch','worker','Worker','worker','worker')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_dispatch',?,?,?)`, f.project.ID, f.machineID, "/tmp/dispatch")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agents := make([]store.Agent, 0, 2)
	for _, slug := range []string{"one", "two"} {
		agent, err := f.db.CreateAgent(ctx, store.Agent{Slug: slug, ProjectID: f.project.ID, RoleID: "role_dispatch", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.machineID, CheckoutID: "chk_dispatch", KeyHash: "key", CreatedBy: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.AddMember(ctx, f.room.ID, "agent", agent.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.db.Tx(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES (?, ?,0,'fresh_with_history',?)`, "ses_dispatch_"+slug, agent.ID, "2026-10-13T00:00:00.000000000Z")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		agents = append(agents, agent)
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
	_, cursor, err := f.db.EventBounds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	events := f.dial(t, cursor, f.http.URL)
	defer events.CloseNow()
	for {
		_, initial, err := events.Read(t.Context())
		if err != nil {
			t.Fatalf("event stream not ready: %v", err)
		}
		var frame struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(initial, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Type == "ready" {
			break
		}
	}
	dispatcher := NewDispatcher(f.srv)
	post := func(body string) string {
		response := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": body}, true)
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
		return message.ID
	}
	waitDone := func(agentID, messageID string) {
		for {
			note, err := f.db.NoteForAgentMessage(ctx, agentID, messageID)
			if err == nil && note.State == "done" {
				return
			}
			_, _, readErr := events.Read(t.Context())
			if readErr != nil {
				t.Fatalf("waiting for note %s for agent %s: %+v %v; event stream: %v", messageID, agentID, note, err, readErr)
			}
			// State transitions and their events commit together. Each event
			// gives us a new point at which to check the note.
		}
	}
	first := post("everyone please work")
	if count, err := dispatcher.Pass(ctx); err != nil || count != 2 {
		t.Fatalf("first dispatch=%d %v", count, err)
	}
	for _, agent := range agents {
		waitDone(agent.ID, first)
	}
	second := post("@one handle this")
	if count, err := dispatcher.Pass(ctx); err != nil || count != 1 {
		t.Fatalf("targeted dispatch=%d %v", count, err)
	}
	waitDone(agents[0].ID, second)
	if _, err := f.db.NoteForAgentMessage(ctx, agents[1].ID, second); err == nil {
		t.Fatal("untargeted agent woke")
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE room SET gate='paused' WHERE id=?`, f.room.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	paused := post("work after resume")
	if count, err := dispatcher.Pass(ctx); err != nil || count != 0 {
		t.Fatalf("paused dispatch=%d %v", count, err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE room SET gate='open' WHERE id=?`, f.room.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if count, err := dispatcher.Pass(ctx); err != nil || count != 2 {
		t.Fatalf("resumed dispatch=%d %v", count, err)
	}
	for _, agent := range agents {
		waitDone(agent.ID, paused)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET desired='stopped',desired_rev=1 WHERE id=?`, agents[0].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stopped := post("work when one stopped")
	if count, err := dispatcher.Pass(ctx); err != nil || count != 1 {
		t.Fatalf("stopped dispatch=%d %v", count, err)
	}
	waitDone(agents[1].ID, stopped)
	if note, err := f.db.NoteForAgentMessage(ctx, agents[0].ID, stopped); err != nil || note.State != "queued" {
		t.Fatalf("stopped note=%+v %v", note, err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET desired='active',desired_rev=2 WHERE id=?`, agents[0].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if count, err := dispatcher.Pass(ctx); err != nil || count != 1 {
		t.Fatalf("reactivated dispatch=%d %v", count, err)
	}
	waitDone(agents[0].ID, stopped)
	cancel()
	<-done
}

// A pass that fails says so to the caller and to Sentry (area `dispatcher`), once per cause although the pass repeats every few milliseconds. Not parallel:
// the reporter is process-wide.
func TestDispatcherRunReportsAFailingPassOnce(t *testing.T) {
	telemetry.ResetCaptured()
	transport := &captureTransport{}
	tel, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v1", TracesSampleRate: 1}, transport)
	if err != nil || tel == nil {
		t.Fatalf("telemetry.Init: %v %v", tel, err)
	}
	t.Cleanup(tel.Close)
	f := newFixture(t)
	if err := f.db.Close(); err != nil { // every pass now fails the same way
		t.Fatal(err)
	}
	var said int
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), machineWaitGuard)
	defer cancel()
	// Stop on the second failed pass: a fixed run time raced a slow start that let fewer than two passes happen.
	NewDispatcher(f.srv).Run(ctx, 5*time.Millisecond, func(error) {
		mu.Lock()
		said++
		if said >= 2 {
			cancel()
		}
		mu.Unlock()
	})
	mu.Lock()
	heard := said
	mu.Unlock()
	if heard < 2 {
		t.Fatalf("the caller should hear of every failed pass, heard %d", heard)
	}
	telemetry.Current().Flush(time.Second)
	transport.mu.Lock()
	defer transport.mu.Unlock()
	var found int
	for _, event := range transport.events {
		if len(event.Exception) > 0 && event.Tags["area"] == "dispatcher" {
			found++
		}
	}
	if found < 1 || found > 2 { // the startup sweep and the passes may fail for two different reasons, never once per pass
		t.Fatalf("want one report per cause (1 or 2), got %d of %d passes", found, heard)
	}
}

// The credential alerts go to Sentry by kind alone (their detail names accounts and paths): what failed is an error, what needs a person a warning, and the
// information (expiring soon, a login imported, another refresher present) nothing. Not parallel: the reporter is process-wide.
func TestCredentialAlertsReachSentryByKindOnly(t *testing.T) {
	telemetry.ResetCaptured()
	transport := &captureTransport{}
	tel, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v1", TracesSampleRate: 1}, transport)
	if err != nil || tel == nil {
		t.Fatalf("telemetry.Init: %v %v", tel, err)
	}
	t.Cleanup(tel.Close)
	f := newFixture(t)
	sink := f.srv.credentialAlertSink(false)
	sink("commit_failed", "claude/main: could not write /Users/alice/.claude/.credentials.json")
	sink("commit_failed", "codex/bkp: another detail") // the same kind again
	sink("reauth_required", "claude/main: sign in again")
	for _, quiet := range []string{"expiring", "login_imported", "refresher_conflict"} {
		sink(quiet, "claude/main: fine")
	}
	telemetry.Current().Flush(time.Second)
	transport.mu.Lock()
	defer transport.mu.Unlock()
	var got []string
	for _, event := range transport.events {
		if len(event.Exception) == 0 || event.Tags["area"] != "credentials" {
			continue
		}
		if strings.Contains(event.Exception[0].Value, "alice") || strings.Contains(event.Exception[0].Value, "claude/main") {
			t.Fatalf("the detail must not leave: %q", event.Exception[0].Value)
		}
		got = append(got, string(event.Level)+": "+event.Exception[0].Value)
	}
	want := []string{"error: credential alert: commit_failed", "warning: credential alert: reauth_required"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// The quota package reports through the hook the server gives it: an error or a warning, once per cause, with the path of a failing file left out. Not parallel:
// the reporter is process-wide.
func TestQuotaReportsReachSentry(t *testing.T) {
	telemetry.ResetCaptured()
	transport := &captureTransport{}
	tel, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v1", TracesSampleRate: 1}, transport)
	if err != nil || tel == nil {
		t.Fatalf("telemetry.Init: %v %v", tel, err)
	}
	t.Cleanup(tel.Close)
	reportQuota("automatic account switch failed", false)
	reportQuota("automatic account switch failed", false) // the same cause
	reportQuota("the stored limit guard is unreadable; using the defaults", true)
	telemetry.Current().Flush(time.Second)
	transport.mu.Lock()
	defer transport.mu.Unlock()
	var levels []string
	for _, event := range transport.events {
		if len(event.Exception) > 0 && event.Tags["area"] == "quota" {
			if event.Exception[0].Value != "automatic account switch failed" && event.Exception[0].Value != "the stored limit guard is unreadable; using the defaults" {
				t.Fatalf("more than the fixed phrase went out: %q", event.Exception[0].Value)
			}
			levels = append(levels, string(event.Level))
		}
	}
	if len(levels) != 2 || levels[0] != "error" || levels[1] != "warning" {
		t.Fatalf("levels = %v", levels)
	}
}

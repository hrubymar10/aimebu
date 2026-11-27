package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/getsentry/sentry-go"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

func (f *fixture) execution(t *testing.T) (store.Agent, store.Turn, store.Note, protocol.Delivery) {
	t.Helper()
	f.seed()
	ctx := context.Background()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_t6','worker','Worker','','worker')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_t6',?,?,?)`, f.project.ID, f.machineID, f.project.Folder)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := f.db.CreateAgent(ctx, store.Agent{Slug: "worker", ProjectID: f.project.ID, RoleID: "role_t6", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.machineID, CheckoutID: "chk_t6", KeyHash: "pending", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(ctx, f.room.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_t6',?,0,'fresh_with_history','2026-10-13T00:00:00.000000000Z')`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "do a task"}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("message status=%d", resp.StatusCode)
	}
	var message store.Message
	json.NewDecoder(resp.Body).Decode(&message)
	resp.Body.Close()
	note, err := f.db.NoteForAgentMessage(ctx, agent.ID, message.ID)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.db.RecordTurn(ctx, store.Turn{AgentID: agent.ID, SessionID: "ses_t6", Generation: 0, RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, turn.ID, note.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	delivery := protocol.Delivery{AgentID: agent.ID, Generation: 0, DesiredRev: 0, TurnID: turn.ID, Mode: adapter.ModeNormal, Agent: adapter.AgentSpec{ID: agent.ID, Tool: "fake", Variant: "test"}, Notes: []protocol.DeliveryNote{{NoteID: note.ID, RoomID: f.room.ID, MessageID: message.ID, Body: "do a task", From: "owner"}}, Context: json.RawMessage(`{"role_body":"worker"}`)}
	return agent, turn, note, delivery
}

// Every wait below is for a state that stays once it is reached (the connection, the settled turn, the stop), so it is bounded by
// machineWaitGuard, which only stops a test that would hang for good, not by a short deadline that a loaded machine can miss.
func TestMachineAuthenticationAndFullFakeTurn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, delivery := f.execution(t)
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer wrong")
	_, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("machine auth=%v %v", err, resp)
	}
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	fake := adapter.NewFake()
	fake.Script(turn.ID, adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventNativeSessionStarted, NativeSessionHandle: "native-thread-1"}}, adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventActivity, Summary: "working"}}, adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(`{"messages":[]}`)}}, adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{ExitCode: 0, TerminalEvent: true}})
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, fake)
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor did not connect")
	}
	if err := f.srv.Dispatch(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(machineWaitGuard)
	for {
		state, err := f.db.DeliveryState(context.Background(), turn.ID)
		if err != nil {
			t.Fatal(err)
		}
		open, err := journal.OpenTurns(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if state.Exec == "exited" && state.Outcome == "completed" && state.Settle == "settled" && state.NoteState == "done" && len(open) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("round trip: state=%+v journal=%+v", state, open)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, result, err := f.db.ReceiptByTurn(context.Background(), turn.ID, 0)
	if err != nil || !strings.Contains(result, `"result":"committed"`) {
		t.Fatalf("receipt=%q %v", result, err)
	}
	if runtime, err := f.db.AgentRuntimeByID(context.Background(), agent.ID); err != nil || runtime.Observed != "idle" {
		t.Fatalf("agent after turn=%+v %v", runtime, err)
	}
	if err := f.db.RecordNativeSessionStarted(context.Background(), turn.ID, agent.ID, 0, "probe", "different-thread"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("native session frame did not persist the original handle: %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor did not stop")
	}
}

// machineWaitGuard is how long the machine-wire tests wait for a state that stays once it is reached. It only stops a test that would
// otherwise hang for good; the tests never depend on how soon a state arrives, so a loaded machine makes them slower, not red.
const machineWaitGuard = 30 * time.Second

func TestMachineReconnectReplaysExitWithoutRerun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, turn, _, delivery := f.execution(t)
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	gate := make(chan struct{})
	fake := adapter.NewFake()
	fake.Script(turn.ID,
		adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(`{"messages":[]}`)}},
		adapter.Step{Kind: adapter.StepGate, Gate: gate},
		adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, fake)
	errorsSeen := make(chan error, 16)
	client.OnError = func(err error) {
		select {
		case errorsSeen <- err:
		default:
		}
	}
	ready := make(chan struct{}, 2)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The two connections are made one at a time (RunOnce), not by Run's loop that reconnects on its own clock. The exit is journaled while
	// nothing is connected and stays pending until the second connection replays it; a loop that reconnected by itself could replay and
	// acknowledge it before a slow test looked, and the test would wait for a state that had already passed. Each wait below is for a state
	// that stays until the test itself moves on.
	waitReady := func(what string) {
		t.Helper()
		select {
		case <-ready:
		case <-time.After(machineWaitGuard):
			t.Fatalf("%s timed out", what)
		}
	}
	first := make(chan error, 1)
	go func() { first <- client.RunOnce(ctx) }()
	waitReady("initial supervisor connect")
	if err := f.srv.Dispatch(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(machineWaitGuard)
	for {
		started, err := journal.HasStart(context.Background(), turn.ID)
		if err != nil {
			t.Fatal(err)
		}
		open, err := journal.OpenTurns(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, _, receiptErr := f.db.ReceiptByTurn(context.Background(), turn.ID, 0)
		if started && receiptErr == nil && len(open) == 1 && open[0].OpenTurns[0].Exec == "running" { // Receipt arrived; process is still gated.
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("answer receipt did not arrive before disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	machine, err := f.srv.machineForID(f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	machine.stop()
	select {
	case <-first:
	case <-time.After(machineWaitGuard):
		t.Fatal("the supervisor did not notice the disconnect")
	}
	close(gate)
	deadline = time.Now().Add(machineWaitGuard)
	for {
		exits, err := journal.PendingExits(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(exits) == 1 {
			break
		}
		if time.Now().After(deadline) {
			open, _ := journal.OpenTurns(context.Background())
			problems := []string{}
			for len(errorsSeen) > 0 {
				problems = append(problems, (<-errorsSeen).Error())
			}
			t.Fatalf("exit not journaled after disconnect: inventory=%+v errors=%v", open, problems)
		}
		time.Sleep(10 * time.Millisecond)
	}
	second := make(chan error, 1)
	go func() { second <- client.RunOnce(ctx) }()
	waitReady("supervisor reconnect")
	deadline = time.Now().Add(machineWaitGuard)
	for {
		state, err := f.db.DeliveryState(context.Background(), turn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state.Exec == "exited" && state.Outcome == "completed" && state.NoteState == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replayed exit did not settle turn: %+v", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline = time.Now().Add(machineWaitGuard)
	for {
		exits, err := journal.PendingExits(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(exits) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("exit not acknowledged: %+v", exits)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-second:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor did not stop")
	}
}

func TestSupervisorRestartAfterReceiptWithoutExitRequiresRecovery(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, note, delivery := f.execution(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.sqlite")
	journal, err := supervisor.OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ReconcileMachineHello(ctx, f.machineID, journal.ID, []byte("[]"), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AdmitTurn(ctx, store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: f.machineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckTurn(ctx, turn.ID, agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(delivery)
	if _, err := journal.RecordStart(ctx, supervisor.StartRecord{TurnID: turn.ID, AgentID: agent.ID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}, Mode: "normal", SpecJSON: spec}); err != nil {
		t.Fatal(err)
	}
	candidate := []byte(`{"messages":[]}`)
	sum := sha256.Sum256(candidate)
	digest := hex.EncodeToString(sum[:])
	if _, err := journal.RecordAnswer(ctx, turn.ID, 0, digest, candidate, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.db.HoldAnswer(ctx, store.HeldAnswer{TurnID: turn.ID, AgentID: agent.ID, Generation: 0, AnswerRev: 0, Digest: digest, Candidate: candidate, Reason: "not_validated"}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordReceipt(ctx, protocol.ReceiptBody{TurnID: turn.ID, AnswerRev: 0, Digest: digest, Result: "held"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ReconcileMachineHello(ctx, f.machineID, journal.ID, []byte("[]"), map[string]store.JournalTurn{turn.ID: {TurnID: turn.ID, Exec: "running", HasAnswer: true, ProcessAlive: true, Digest: digest}}); err != nil {
		t.Fatal(err)
	}
	live, err := f.db.DeliveryState(ctx, turn.ID)
	if err != nil || live.Exec != "running" || live.RecoveryBlock {
		t.Fatalf("live reconnect state=%+v %v", live, err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err = supervisor.OpenJournal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	open, err := journal.OpenTurns(ctx)
	if err != nil || len(open) != 1 || open[0].OpenTurns[0].Exec != "running" {
		t.Fatalf("restarted inventory=%+v %v", open, err)
	}
	if _, err := f.db.ReconcileMachineHello(ctx, f.machineID, journal.ID, []byte("[]"), map[string]store.JournalTurn{turn.ID: {TurnID: turn.ID, Exec: "running", HasAnswer: true, Digest: digest}}); err != nil {
		t.Fatal(err)
	}
	state, err := f.db.DeliveryState(ctx, turn.ID)
	if err != nil || state.Exec != "lost" || state.NoteState != "unknown" || !state.RecoveryBlock {
		t.Fatalf("restart state=%+v %v", state, err)
	}
}

func TestProcessExitKeepsAgentBusyUntilAnswerReceipt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, note, _ := f.execution(t)
	ctx := context.Background()
	if err := f.db.AdmitTurn(ctx, store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: f.machineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckTurn(ctx, turn.ID, agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordProcess(ctx, turn.ID, agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	runtime, err := f.db.AgentRuntimeByID(ctx, agent.ID)
	if err != nil || runtime.Observed != "in_turn" {
		t.Fatalf("agent freed before receipt: %+v %v", runtime, err)
	}
	candidate := []byte(`{"messages":[]}`)
	sum := sha256.Sum256(candidate)
	if _, _, err := f.db.HoldAnswer(ctx, store.HeldAnswer{TurnID: turn.ID, AgentID: agent.ID, Generation: 0, AnswerRev: 0, Digest: hex.EncodeToString(sum[:]), Candidate: candidate, Reason: "not_validated"}); err != nil {
		t.Fatal(err)
	}
	runtime, err = f.db.AgentRuntimeByID(ctx, agent.ID)
	if err != nil || runtime.Observed != "idle" {
		t.Fatalf("agent not freed after receipt: %+v %v", runtime, err)
	}
}

func TestMachineRejectsStaleGeneration(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, _ := f.execution(t)
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID, JournalID: "journal-test", Agents: []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}}})
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Ack, AgentID: agent.ID, Generation: 1, TurnID: turn.ID, Level: "accepted"})
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var answer protocol.Frame
	if err := json.Unmarshal(data, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Type != protocol.Error || answer.Code != "stale_generation" {
		t.Fatalf("stale response=%+v", answer)
	}
	state, err := f.db.DeliveryState(ctx, turn.ID)
	if err != nil || state.Exec != "pending" {
		t.Fatalf("stale frame changed state=%+v %v", state, err)
	}
}

func TestMachineRejectsAnswerForAnotherAgentsTurn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agentA, _, _, _ := f.execution(t)
	ctx := context.Background()
	agentB, err := f.db.CreateAgent(ctx, store.Agent{Slug: "workerb", ProjectID: f.project.ID, RoleID: "role_t6", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.machineID, CheckoutID: "chk_t6", KeyHash: "pending", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(ctx, f.room.ID, "agent", agentB.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_t6b',?,0,'fresh_with_history','2026-10-13T00:00:00.000000000Z')`, agentB.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	turnB, err := f.db.RecordTurn(ctx, store.Turn{AgentID: agentB.ID, SessionID: "ses_t6b", Generation: 0, RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	conn := f.dialMachine(t, []protocol.HelloAgent{{AgentID: agentA.ID, Generation: 0}, {AgentID: agentB.ID, Generation: 0}})
	defer conn.Close(websocket.StatusNormalClosure, "")
	candidate := `{"messages":[]}`
	sum := sha256.Sum256([]byte(candidate))
	digest := hex.EncodeToString(sum[:])
	send := func(agentID string) protocol.Frame {
		t.Helper()
		raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Answer, AgentID: agentID, Generation: 0, TurnID: turnB.ID, AnswerRev: 0, Digest: digest, Candidate: candidate})
		ioCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := conn.Write(ioCtx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
		_, raw, err := conn.Read(ioCtx)
		if err != nil {
			t.Fatal(err)
		}
		var response protocol.Frame
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if response := send(agentA.ID); response.Type != protocol.Error || response.Code != "forbidden" {
		t.Fatalf("foreign answer accepted: %+v", response)
	}
	if _, _, err := f.db.ReceiptByTurn(ctx, turnB.ID, 0); err == nil {
		t.Fatal("foreign answer wrote a receipt")
	}
	if response := send(agentB.ID); response.Type != protocol.Receipt || response.Result != "committed" {
		t.Fatalf("owner answer rejected: %+v", response)
	}
}

// A frame the server refuses, and a store failure behind a frame, were a log line and an error frame to the supervisor and nothing more: they reach Sentry as
// area `machine` (a rejection a warning, once per cause, with no id in it). Not parallel: the reporter is process-wide.
func TestMachineRejectedFrameReachesSentry(t *testing.T) {
	telemetry.ResetCaptured()
	transport := &captureTransport{}
	tel, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v1", TracesSampleRate: 1}, transport)
	if err != nil || tel == nil {
		t.Fatalf("telemetry.Init: %v %v", tel, err)
	}
	t.Cleanup(tel.Close)
	f := newFixture(t)
	agentA, _, _, _ := f.execution(t)
	ctx := context.Background()
	agentB, err := f.db.CreateAgent(ctx, store.Agent{Slug: "workerb", ProjectID: f.project.ID, RoleID: "role_t6", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.machineID, CheckoutID: "chk_t6", KeyHash: "pending", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(ctx, f.room.ID, "agent", agentB.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_t6b',?,0,'fresh_with_history','2026-10-13T00:00:00.000000000Z')`, agentB.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	turnB, err := f.db.RecordTurn(ctx, store.Turn{AgentID: agentB.ID, SessionID: "ses_t6b", Generation: 0, RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	conn := f.dialMachine(t, []protocol.HelloAgent{{AgentID: agentA.ID, Generation: 0}, {AgentID: agentB.ID, Generation: 0}})
	defer conn.Close(websocket.StatusNormalClosure, "")
	candidate := `{"messages":[]}`
	sum := sha256.Sum256([]byte(candidate))
	digest := hex.EncodeToString(sum[:])
	send := func(agentID string) protocol.Frame {
		t.Helper()
		raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Answer, AgentID: agentID, Generation: 0, TurnID: turnB.ID, AnswerRev: 0, Digest: digest, Candidate: candidate})
		ioCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := conn.Write(ioCtx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
		_, raw, err := conn.Read(ioCtx)
		if err != nil {
			t.Fatal(err)
		}
		var response protocol.Frame
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if response := send("agt_not_on_this_machine"); response.Type != protocol.Error {
		t.Fatalf("a frame for an agent that is not this machine's was accepted: %+v", response)
	}
	telemetry.Current().Flush(time.Second)
	transport.mu.Lock()
	var events []*sentry.Event
	for _, event := range transport.events {
		if len(event.Exception) > 0 { // (the server also sends transactions of its own)
			events = append(events, event)
		}
	}
	transport.mu.Unlock()
	if len(events) != 1 || events[0].Level != sentry.LevelWarning || events[0].Tags["area"] != "machine" || !strings.HasPrefix(events[0].Exception[0].Value, "agent frame rejected: type=answer code=") {
		t.Fatalf("the rejected frame must reach Sentry once, as a machine warning: %+v", events)
	}
}

func TestMachineAnswerDuringImportReplaysAfterReconnect(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, _ := f.execution(t)
	conn := f.dialMachine(t, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	defer conn.Close(websocket.StatusNormalClosure, "")
	candidate := `{"messages":[]}`
	sum := sha256.Sum256([]byte(candidate))
	digest := hex.EncodeToString(sum[:])
	frame := protocol.Frame{V: protocol.Version, Type: protocol.Answer, AgentID: agent.ID,
		Generation: 0, TurnID: turn.ID, AnswerRev: 0, Digest: digest, Candidate: candidate}
	raw, _ := json.Marshal(frame)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	f.srv.maintenance.Store(true)
	defer f.srv.maintenance.Store(false)
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
		t.Fatalf("machine socket after import frame: %v", err)
	}
	if _, _, err := f.db.ReceiptByTurn(ctx, turn.ID, 0); err == nil {
		t.Fatal("answer was applied during maintenance")
	}
	f.srv.maintenance.Store(false)
	reconnected := f.dialMachine(t, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	defer reconnected.Close(websocket.StatusNormalClosure, "")
	if err := reconnected.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, reply, err := reconnected.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var receipt protocol.Frame
	if err := json.Unmarshal(reply, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Type != protocol.Receipt || receipt.Result != "committed" {
		t.Fatalf("replayed answer: %+v", receipt)
	}
	stored, _, err := f.db.ReceiptByTurn(ctx, turn.ID, 0)
	if err != nil || stored != digest {
		t.Fatalf("replayed digest=%q err=%v", stored, err)
	}
}

func TestRevokedMachineSocketClosesAndDispatchStops(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, delivery := f.execution(t)
	f.srv.MachinePingInterval = 10 * time.Millisecond
	conn := f.dialMachine(t, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	defer conn.Close(websocket.StatusNormalClosure, "")
	machine, err := f.srv.machineForID(f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	// A heartbeat already in flight may arrive before the revocation close.
	if err := machine.send(protocol.Frame{V: protocol.Version, Type: protocol.Ping, MachineID: f.machineID}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RevokeMachine(context.Background(), f.machineID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, raw, err := conn.Read(ctx)
		if websocket.CloseStatus(err) == websocket.StatusPolicyViolation {
			break
		}
		if err != nil {
			t.Fatalf("revoked machine socket=%v", err)
		}
		var frame protocol.Frame
		if err := json.Unmarshal(raw, &frame); err != nil || frame.Type != protocol.Ping {
			t.Fatalf("unexpected frame before revoked machine closed: %+v, %v", frame, err)
		}
	}
	dispatchCtx, dispatchCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dispatchCancel()
	if err := f.srv.Dispatch(dispatchCtx, delivery); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("revoked machine dispatch=%v", err)
	}
	state, err := f.db.DeliveryState(context.Background(), turn.ID)
	if err != nil || state.Exec != "pending" || state.NoteState != "queued" {
		t.Fatalf("revoked machine changed turn=%+v %v", state, err)
	}
}

func TestMachineReconcilesJournaledStaleGenerationAnswer(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, _ := f.execution(t)
	ctx := context.Background()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET generation=1 WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	candidate := `{"messages":[]}`
	sum := sha256.Sum256([]byte(candidate))
	digest := hex.EncodeToString(sum[:])
	conn := f.dialMachine(t, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0,
		OpenTurns: []protocol.OpenTurn{{TurnID: turn.ID, Exec: "exited", HasAnswer: true, AnswerRev: 0, Digest: digest}}}})
	defer conn.Close(websocket.StatusNormalClosure, "")
	ioCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, frame := range []protocol.Frame{
		{V: protocol.Version, Type: protocol.Ack, AgentID: agent.ID, Generation: 0, TurnID: turn.ID, Level: "accepted"},
		{V: protocol.Version, Type: protocol.Answer, AgentID: agent.ID, Generation: 0, TurnID: turn.ID, AnswerRev: 0, Digest: digest, Candidate: candidate},
	} {
		raw, _ := json.Marshal(frame)
		if err := conn.Write(ioCtx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
		_, raw, err := conn.Read(ioCtx)
		if err != nil {
			t.Fatal(err)
		}
		var response protocol.Frame
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		if frame.Type == protocol.Ack && (response.Type != protocol.Error || response.Code != "stale_generation") {
			t.Fatalf("stale control response=%+v", response)
		}
		if frame.Type == protocol.Answer && (response.Type != protocol.Receipt || response.Result != "committed") {
			t.Fatalf("journaled answer response=%+v", response)
		}
	}
}

func (f *fixture) dialMachine(t *testing.T, agents []protocol.HelloAgent) *websocket.Conn {
	return f.dialMachineWithJournal(t, "journal-test", agents)
}

func (f *fixture) dialMachineWithJournal(t *testing.T, journalID string, agents []protocol.HelloAgent) *websocket.Conn {
	return f.dialMachineWithStopReplay(t, journalID, agents, true)
}

func (f *fixture) dialMachineWithStopReplay(t *testing.T, journalID string, agents []protocol.HelloAgent, supported bool) *websocket.Conn {
	conn, _ := f.dialMachineWithStopReplayAck(t, journalID, agents, supported)
	return conn
}

func (f *fixture) dialMachineWithStopReplayAck(t *testing.T, journalID string, agents []protocol.HelloAgent, supported bool) (*websocket.Conn, protocol.Frame) {
	t.Helper()
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), replayWait)
	defer cancel()
	raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID, JournalID: journalID, SupportsStopReplay: supported, Agents: agents})
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, raw, err = conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err := json.Unmarshal(raw, &ack); err != nil || ack.Type != protocol.HelloAck {
		t.Fatalf("hello_ack=%+v %v", ack, err)
	}
	return conn, ack
}

func TestHelloReconcilesLostAcceptedAckWithoutRequeue(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, note, delivery := f.execution(t)
	ctx := context.Background()
	if err := f.db.AdmitTurn(ctx, store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: f.machineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckTurn(ctx, turn.ID, agent.ID, 0, "persisted"); err != nil {
		t.Fatal(err)
	}
	j, err := supervisor.OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	spec, _ := json.Marshal(delivery)
	if _, err := j.RecordStart(ctx, supervisor.StartRecord{TurnID: turn.ID, AgentID: agent.ID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}, Mode: "normal", SpecJSON: spec}); err != nil {
		t.Fatal(err)
	}
	open, err := j.OpenTurns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn := f.dialMachine(t, open)
	defer conn.Close(websocket.StatusNormalClosure, "")
	state, err := f.db.DeliveryState(ctx, turn.ID)
	if err != nil || state.Exec != "lost" || state.NoteState != "unknown" || !state.RecoveryBlock {
		t.Fatalf("lost ack state=%+v %v", state, err)
	}
}

func TestHelloRequeuesTurnNeverJournaled(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, note, _ := f.execution(t)
	ctx := context.Background()
	if _, err := f.db.ReconcileMachineHello(ctx, f.machineID, "journal-test", []byte("[]"), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AdmitTurn(ctx, store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: f.machineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckTurn(ctx, turn.ID, agent.ID, 0, "persisted"); err != nil {
		t.Fatal(err)
	}
	conn := f.dialMachine(t, nil)
	defer conn.Close(websocket.StatusNormalClosure, "")
	state, err := f.db.DeliveryState(ctx, turn.ID)
	if err != nil || state.Exec != "killed" || state.Settle != "settled" || state.NoteState != "queued" || state.RecoveryBlock {
		t.Fatalf("never started state=%+v %v", state, err)
	}
	redelivery, ready, err := f.db.PrepareDispatch(ctx, agent.ID, "")
	if err != nil || !ready || redelivery.TurnID == turn.ID || len(redelivery.Notes) != 1 || redelivery.Notes[0].NoteID != note.ID || redelivery.Notes[0].No < 1 {
		t.Fatalf("never started redispatch=%+v ready=%v err=%v", redelivery, ready, err)
	}
}

func TestNewJournalPreservesAnotherInstancesOpenTurn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, note, _ := f.execution(t)
	ctx := context.Background()
	if _, err := f.db.ReconcileMachineHello(ctx, f.machineID, "original-journal", []byte("[]"), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AdmitTurn(ctx, store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: f.machineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckTurn(ctx, turn.ID, agent.ID, 0, "persisted"); err != nil {
		t.Fatal(err)
	}
	conn := f.dialMachineWithJournal(t, "replacement-journal", nil)
	defer conn.Close(websocket.StatusNormalClosure, "")
	state, err := f.db.DeliveryState(ctx, turn.ID)
	if err != nil || state.Exec != "pending" || state.Outcome != "none" || state.NoteState != "dispatched" || state.RecoveryBlock {
		t.Fatalf("other instance's turn changed: %+v %v", state, err)
	}
	owner, err := f.db.UserByLogin(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.db.EventsAfter(ctx, owner.ID, 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	visible := false
	for _, event := range events {
		if event.Kind == "supervisor_journal_changed" && event.TurnID == turn.ID {
			visible = true
		}
	}
	if visible {
		t.Fatal("new instance marked another instance's turn lost")
	}
}

func TestFirstJournalHelloWithOpenTurnIsUncertain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, note, _ := f.execution(t)
	ctx := context.Background()
	if err := f.db.AdmitTurn(ctx, store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: f.machineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	conn := f.dialMachineWithJournal(t, "first-journal", nil)
	defer conn.Close(websocket.StatusNormalClosure, "")
	state, err := f.db.DeliveryState(ctx, turn.ID)
	if err != nil || state.Exec != "lost" || state.Outcome != "unknown" || state.NoteState != "unknown" || !state.RecoveryBlock {
		t.Fatalf("first journal with open turn: %+v %v", state, err)
	}
}

// replayWait bounds the hello, replay and stop of the supervisor client below, and the hello and the answer round trip of the raw websocket
// connection the machine tests dial.
// Each is a real websocket, journal and server round trip; under a parallel
// -race gate on a busy host 3s was not always enough, so a slow run is not a
// failure (the waits end as soon as the event happens, so a pass costs nothing).
const replayWait = 20 * time.Second

func TestLostReceiptReplayedAndDigestConflict(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, note, delivery := f.execution(t)
	ctx := context.Background()
	if err := f.db.AdmitTurn(ctx, store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: f.machineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckTurn(ctx, turn.ID, agent.ID, 0, "persisted"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckTurn(ctx, turn.ID, agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	answer := json.RawMessage(`{"messages":[]}`)
	sum := sha256.Sum256(answer)
	digest := hex.EncodeToString(sum[:])
	if err := f.db.RecordProcess(ctx, turn.ID, agent.ID, 0, 0, true, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.db.HoldAnswer(ctx, store.HeldAnswer{TurnID: turn.ID, AgentID: agent.ID, Generation: 0, AnswerRev: 0, Digest: digest, Candidate: answer, Reason: "not_validated"}); err != nil {
		t.Fatal(err)
	}
	j, err := supervisor.OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	spec, _ := json.Marshal(delivery)
	if _, err := j.RecordStart(ctx, supervisor.StartRecord{TurnID: turn.ID, AgentID: agent.ID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}, Mode: "normal", SpecJSON: spec}); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordExit(ctx, turn.ID, 0, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordAnswer(ctx, turn.ID, 0, digest, answer, false); err != nil {
		t.Fatal(err)
	}
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, j, adapter.NewFake())
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(runCtx) }()
	select {
	case <-ready:
	case <-time.After(replayWait):
		t.Fatal("receipt replay hello timed out")
	}
	pending, err := j.PendingAnswers(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("lost receipt not replayed; candidate still pending: %+v %v", pending, err)
	}
	deadline := time.Now().Add(replayWait)
	for {
		exits, err := j.PendingExits(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(exits) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replayed exit not acknowledged: %+v", exits)
		}
		time.Sleep(10 * time.Millisecond)
	}
	open, err := j.OpenTurns(ctx)
	if err != nil || len(open) != 0 {
		t.Fatalf("acknowledged turn still in hello inventory: %+v %v", open, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(replayWait):
		t.Fatal("replay client did not stop")
	}
	conn := f.dialMachine(t, nil)
	defer conn.Close(websocket.StatusNormalClosure, "")
	sendAnswer := func(candidate json.RawMessage, digest string) protocol.Frame {
		t.Helper()
		frame := protocol.Frame{V: protocol.Version, Type: protocol.Answer, AgentID: agent.ID, Generation: 0, TurnID: turn.ID, AnswerRev: 0, Digest: digest, Candidate: string(candidate)}
		raw, _ := json.Marshal(frame)
		ioCtx, cancel := context.WithTimeout(ctx, replayWait)
		defer cancel()
		if err := conn.Write(ioCtx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
		_, raw, err := conn.Read(ioCtx)
		if err != nil {
			t.Fatal(err)
		}
		var response protocol.Frame
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if response := sendAnswer(answer, digest); response.Type != protocol.Receipt || response.Result != "held" {
		t.Fatalf("same digest replay=%+v", response)
	}
	different := json.RawMessage(`{"messages":[{"body":"different"}]}`)
	sum = sha256.Sum256(different)
	otherDigest := hex.EncodeToString(sum[:])
	if response := sendAnswer(different, otherDigest); response.Type != protocol.Receipt || response.Result != "conflict" {
		t.Fatalf("different digest replay=%+v", response)
	}
	stored, _, err := f.db.ReceiptByTurn(ctx, turn.ID, 0)
	if err != nil || stored != digest {
		t.Fatalf("stored digest changed=%s %v", stored, err)
	}
}

func TestSlowMachineQueueDoesNotBlockHTTP(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	conn := f.dialMachine(t, nil)
	defer conn.Close(websocket.StatusNormalClosure, "")
	c, err := f.srv.machineForID(f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 1<<20)
	started := time.Now()
	slow := false
	for i := 0; i < 1000; i++ {
		err := c.send(protocol.Frame{V: protocol.Version, Type: protocol.Ping, Message: payload})
		if errors.Is(err, ErrSlowMachine) {
			slow = true
			break
		}
		if err != nil {
			break
		}
	}
	if !slow {
		t.Fatal("slow machine was not disconnected when its queue filled")
	}
	if elapsed := time.Since(started); elapsed > machineWaitGuard {
		t.Fatalf("enqueue blocked for %s", elapsed)
	}
	reqCtx, cancel := context.WithTimeout(context.Background(), machineWaitGuard)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, "GET", f.http.URL+"/api/me", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP blocked by slow machine: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("HTTP status=%d", resp.StatusCode)
	}
}

func TestOversizeAnswerGetsReceiptInsteadOfDroppingMachineFrame(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, _ := f.execution(t)
	conn := f.dialMachine(t, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	defer conn.Close(websocket.StatusNormalClosure, "")
	digest := strings.Repeat("a", 64)
	frame := protocol.Frame{V: protocol.Version, Type: protocol.Answer, AgentID: agent.ID,
		Generation: 0, TurnID: turn.ID, Digest: digest, AnswerTooLarge: true}
	raw, _ := json.Marshal(frame)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var receipt protocol.Frame
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Type != protocol.Receipt || receipt.Digest != digest || !strings.Contains(string(receipt.Details), "answer too large") {
		t.Fatalf("oversize receipt=%+v", receipt)
	}
}

func TestMalformedFinalCandidateSurvivesMachineWire(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, turn, _, delivery := f.execution(t)
	j, err := supervisor.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	malformed := `{"messages":[`
	fake := adapter.NewFake()
	fake.Script(turn.ID, adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(malformed)}}, adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, j, fake)
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor not ready")
	}
	if err := f.srv.Dispatch(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(machineWaitGuard)
	for {
		_, result, err := f.db.ReceiptByTurn(context.Background(), turn.ID, 0)
		if err == nil {
			var body struct {
				CandidateRaw string `json:"candidate_raw"`
				Result       string `json:"result"`
			}
			if err := json.Unmarshal([]byte(result), &body); err != nil || body.CandidateRaw != malformed || body.Result != "correction_requested" {
				t.Fatalf("raw candidate=%+v %v", body, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("malformed candidate not received: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor did not stop")
	}
}

func TestFakeCorrectionTurnCommitsAfterInvalidBatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, parent, _, delivery := f.execution(t)
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	fake := adapter.NewFake()
	// An unknown room is a shape error no dropped link can fix, so the batch is held and a correction turn is requested.
	bad := fmt.Sprintf(`{"messages":[{"room":%q,"body":"valid"},{"room":"room_unknown","body":"bad"}]}`, f.room.ID)
	fake.Script(parent.ID, adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(bad)}},
		adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, fake)
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor not ready")
	}
	if err := f.srv.Dispatch(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(machineWaitGuard)
	var correction store.Turn
	for {
		correction, err = f.db.CorrectionTurn(context.Background(), parent.ID)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("correction not created: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	messages, err := f.db.ListMessagesAfter(context.Background(), f.room.ID, 0)
	if err != nil || len(messages) != 1 {
		t.Fatalf("invalid batch partly committed: %+v %v", messages, err)
	}
	good := fmt.Sprintf(`{"messages":[{"room":%q,"body":"fixed"}]}`, f.room.ID)
	fake.Script(correction.ID, adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(good)}},
		adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE role SET cap_singleton_writer=1 WHERE id='role_t6'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fixDelivery := protocol.Delivery{AgentID: agent.ID, Generation: 0, DesiredRev: 0, TurnID: correction.ID,
		Mode: adapter.ModeCorrection, ParentTurnID: parent.ID, Agent: delivery.Agent,
		Context: json.RawMessage(`{"correction_error":"invalid reply reference"}`), AllowedTools: []string{}}
	if err := f.srv.Dispatch(ctx, fixDelivery); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(machineWaitGuard)
	for {
		_, childJSON, childErr := f.db.ReceiptByTurn(context.Background(), correction.ID, 1)
		_, parentJSON, parentErr := f.db.ReceiptByTurn(context.Background(), parent.ID, 0)
		state, stateErr := f.db.DeliveryState(context.Background(), correction.ID)
		if childErr == nil && parentErr == nil && stateErr == nil && strings.Contains(childJSON, `"result":"committed"`) && strings.Contains(parentJSON, `"result":"superseded"`) && state.Exec == "exited" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("correction did not settle: child=%q (%v) parent=%q (%v) state=%+v (%v)", childJSON, childErr, parentJSON, parentErr, state, stateErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	messages, err = f.db.ListMessagesAfter(context.Background(), f.room.ID, 0)
	if err != nil || len(messages) != 2 || messages[1].Body != "fixed" {
		t.Fatalf("corrected output=%+v %v", messages, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor did not stop")
	}
}

// An exit without an answer never gets a receipt. Once the server has
// acknowledged it, the journal must stop listing the turn, or a later switch
// to another machine leaves this machine's hello failing its binding check.
func TestSupervisorReconnectsAfterAgentSwitchedAwayFromAnswerlessExit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, delivery := f.execution(t)
	ctx := context.Background()
	journal, err := supervisor.OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	fake := adapter.NewFake()
	fake.Script(turn.ID, adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{ExitCode: 1}})
	connect := func() (context.CancelFunc, chan error, chan struct{}) {
		client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, fake)
		client.OnError = func(error) {}
		ready := make(chan struct{}, 1)
		client.OnReady = func() { ready <- struct{}{} }
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- client.RunOnce(runCtx) }()
		return cancel, done, ready
	}
	cancel, done, ready := connect()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("initial connect: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("initial connect timed out")
	}
	if err := f.srv.Dispatch(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		started, err := journal.HasStart(ctx, turn.ID)
		if err != nil {
			t.Fatal(err)
		}
		exits, err := journal.PendingExits(ctx)
		if err != nil {
			t.Fatal(err)
		}
		state, err := f.db.DeliveryState(ctx, turn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if started && len(exits) == 0 && state.Exec == "exited" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("answerless exit not acknowledged: exits=%+v state=%+v", exits, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	// The cross-machine switch rebinds the agent the same way.
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO machine(id,name,key_hash,approved) VALUES ('mch_switch_target','remote','hash',1)`); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE agent SET machine_id='mch_switch_target' WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cancel, done, ready = connect()
	defer cancel()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("source machine could not reconnect after its agent moved away: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("reconnect timed out")
	}
}

// turnGoroutines is the stacks of the goroutines that run a supervisor's turn (its runTurn and the fake harness under it) when a wait for
// a turn's end times out: whether the turn was stuck, and where, or only slow. Other parallel tests' turns show too; there are few.
func turnGoroutines() string {
	buf := make([]byte, 16<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var kept []string
	for _, stack := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(stack, "supervisor.(*Client).runTurn") || strings.Contains(stack, "adapter.(*fakeTurn)") {
			kept = append(kept, stack)
		}
	}
	if len(kept) == 0 {
		return "(none: no turn of the supervisor is running)"
	}
	return strings.Join(kept, "\n\n")
}

// Usage and compaction events journaled while the machine socket is down
// must reach the server after reconnect, exactly once each.
func TestReconnectReplaysUsageAndCompactionOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, delivery := f.execution(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	journal, err := supervisor.OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	cost := func(v float64) *float64 { return &v }
	gate := make(chan struct{})
	fake := adapter.NewFake()
	fake.Script(turn.ID,
		adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventUsage, Usage: &adapter.Usage{CostEstimate: cost(0.25)}}},
		adapter.Step{Kind: adapter.StepGate, Gate: gate},
		adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventCompaction, Compaction: &adapter.Compaction{PreTokens: 90000, PostTokens: 12000}}},
		adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventUsage, Usage: &adapter.Usage{CostEstimate: cost(0.5)}}},
		adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(`{"messages":[]}`)}},
		adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, fake)
	client.OnError = func(error) {}
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	connect := func() chan error {
		done := make(chan error, 1)
		go func() { done <- client.RunOnce(ctx) }()
		select {
		case <-ready:
		case err := <-done:
			t.Fatalf("connect: %v", err)
		case <-time.After(20 * time.Second):
			t.Fatal("connect timed out")
		}
		return done
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s; the supervisor's turn goroutines of this test binary now:\n%s", what, turnGoroutines())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	costSoFar := func() float64 {
		usage, err := f.db.AgentUsage(ctx, agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if usage == nil || usage.CostSoFar == nil {
			return 0
		}
		return *usage.CostSoFar
	}
	done := connect()
	if err := f.srv.Dispatch(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	waitFor("live usage", func() bool { return costSoFar() == 0.25 })
	machine, err := f.srv.machineForID(f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	machine.stop()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("supervisor did not notice the dropped socket")
	}
	close(gate) // The rest of the turn runs while disconnected.
	waitFor("journaled exit", func() bool {
		exits, err := journal.PendingExits(ctx)
		return err == nil && len(exits) == 1
	})
	done = connect()
	waitFor("acknowledged exit", func() bool {
		exits, err := journal.PendingExits(ctx)
		return err == nil && len(exits) == 0
	})
	waitFor("replayed usage and compaction", func() bool {
		if costSoFar() != 0.75 {
			return false
		}
		messages, err := f.db.ListMessagesAfter(ctx, f.room.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range messages {
			if strings.Contains(message.Body, "context was compacted") {
				return true
			}
		}
		return false
	})
	if got := costSoFar(); got != 0.75 {
		t.Fatalf("cost after replay=%v, want each usage counted once (0.75)", got)
	}
	messages, err := f.db.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, message := range messages {
		if strings.Contains(message.Body, "context was compacted") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("compaction notices=%d, want 1", notices)
	}
	cancel()
	<-done
}

// specTap is the fake adapter that also remembers the spec of each turn it was asked to start.
type specTap struct {
	*adapter.Fake
	specs chan adapter.TurnSpec
}

func (s *specTap) Start(ctx context.Context, spec adapter.TurnSpec) (adapter.Turn, error) {
	s.specs <- spec
	return s.Fake.Start(ctx, spec)
}

// A turn that forks a session is told so on the wire: the supervisor's adapter gets the original as the handle to resume plus the fork
// flag, and the copy's id the turn reports replaces the original in the session.
func TestMachineCarriesAForkedTurnAndItsCopyId(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, delivery := f.execution(t)
	const original, copyID = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		for _, statement := range []string{
			`UPDATE session SET native_session_id='` + original + `',native_session_started_at='2026-10-13T00:00:00.000000000Z',fork_of='` + original + `' WHERE id='ses_t6'`,
			`INSERT INTO imported_session(agent_id,machine_id,session_id,mode,cwd,imported_by,imported_at) VALUES ('` + agent.ID + `','` + f.machineID + `','` + original + `','fork','/p','owner','now')`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	delivery.NativeSessionHandle, delivery.NativeSessionStarted, delivery.ForkSession = original, true, true
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	tap := &specTap{Fake: adapter.NewFake(), specs: make(chan adapter.TurnSpec, 1)}
	tap.Script(turn.ID, adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventNativeSessionStarted, NativeSessionHandle: copyID}},
		adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(`{"messages":[]}`)}},
		adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{ExitCode: 0, TerminalEvent: true}})
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, tap)
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor did not connect")
	}
	if err := f.srv.Dispatch(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	select {
	case spec := <-tap.specs:
		if spec.NativeSessionHandle != original || !spec.NativeSessionStarted || !spec.ForkSession {
			t.Fatalf("the adapter was not told to fork: %+v", spec)
		}
	case <-time.After(machineWaitGuard):
		t.Fatal("no turn started")
	}
	deadline := time.Now().Add(machineWaitGuard)
	for {
		imported, ok, err := f.db.AgentImportedSession(context.Background(), agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ok && imported.ForkedSessionID == copyID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the copy's id was not recorded: %+v", imported)
		}
		time.Sleep(10 * time.Millisecond)
	}
	profile, err := f.db.AgentProfile(context.Background(), f.room.ID, agent.ID)
	if err != nil || profile.NativeSessionID != copyID {
		t.Fatalf("the session is the copy now: %+v err=%v", profile, err)
	}
}

// A turn that forks a session is never sent to a supervisor that does not advertise it, however the machine got there: spawn checked once,
// but the machine can reconnect with an older supervisor before the first turn, and one that ignores the flag would resume (and append
// to) the original. The dispatcher holds such an agent without preparing turns and says so once; Dispatch refuses it as well.
func TestForkedTurnWaitsForASupervisorThatCanFork(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	const original = "11111111-1111-4111-8111-111111111111"
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_fork','worker','Worker','worker','worker')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_fork',?,?,?)`, f.project.ID, f.machineID, "/tmp/fork")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := f.db.CreateAgent(ctx, store.Agent{Slug: "copy", ProjectID: f.project.ID, RoleID: "role_fork", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.machineID, CheckoutID: "chk_fork", KeyHash: "key", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(ctx, f.room.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	// The agent is in two rooms and is asked in the second: the hold is said where the note is queued, not in the room it joined first.
	second, err := f.db.CreateRoom(ctx, store.Room{ProjectID: f.project.ID, ParentID: f.room.ID, Kind: "task", Title: "second", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(ctx, second.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at,native_session_id,native_session_started_at,fork_of) VALUES ('ses_fork',?,0,'native_resume','2026-10-13T00:00:00.000000000Z',?,'2026-10-13T00:00:00.000000000Z',?)`, agent.ID, original, original)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	journal, err := supervisor.OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	tap := &specTap{Fake: adapter.NewFake(), specs: make(chan adapter.TurnSpec, 4)}
	tap.ScriptDefault(adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(`{"messages":[]}`)}}, adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, tap)
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = client.RunOnce(runCtx) }()
	select {
	case <-ready:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor did not connect")
	}
	// The machine comes back with a supervisor that cannot fork.
	f.srv.machineMu.Lock()
	conn := f.srv.machines[f.machineID]
	conn.supportsSessionFork = false
	f.srv.machineMu.Unlock()
	if err := f.srv.Dispatch(ctx, protocol.Delivery{AgentID: agent.ID, Generation: 0, TurnID: "turn_never", Mode: adapter.ModeNormal, NativeSessionHandle: original, NativeSessionStarted: true, ForkSession: true}); !errors.Is(err, ErrSupervisorUpdateNeeded) {
		t.Fatalf("a forking delivery to a supervisor that cannot fork: %v", err)
	}
	response := f.request("POST", "/api/rooms/"+second.ID+"/messages", map[string]string{"body": "@copy please work"}, true)
	if response.StatusCode != 201 {
		t.Fatalf("post status=%d", response.StatusCode)
	}
	response.Body.Close()
	dispatcher := NewDispatcher(f.srv)
	for i := 0; i < 3; i++ {
		if count, err := dispatcher.Pass(ctx); err != nil || count != 0 {
			t.Fatalf("pass %d dispatched %d: %v", i, count, err)
		}
	}
	select {
	case spec := <-tap.specs:
		t.Fatalf("the supervisor that cannot fork was given a turn: %+v", spec)
	case <-time.After(300 * time.Millisecond):
	}
	held := func(room string) int {
		messages, err := f.db.ListMessagesAfter(ctx, room, 0)
		if err != nil {
			t.Fatal(err)
		}
		notes := 0
		for _, message := range messages {
			if message.AuthorKind == "system" && strings.Contains(message.Body, "copy is held") && strings.Contains(message.Body, "predates") {
				notes++
			}
		}
		return notes
	}
	if inSecond, inFirst := held(second.ID), held(f.room.ID); inSecond != 1 || inFirst != 0 {
		t.Fatalf("the hold was said %d times where the note is queued and %d in the first room, want once and never", inSecond, inFirst)
	}
	// With a supervisor that can fork again the turn goes out, forking the original.
	f.srv.machineMu.Lock()
	conn.supportsSessionFork = true
	f.srv.machineMu.Unlock()
	if count, err := dispatcher.Pass(ctx); err != nil || count != 1 {
		t.Fatalf("dispatch after the supervisor can fork: %d %v", count, err)
	}
	select {
	case spec := <-tap.specs:
		if spec.NativeSessionHandle != original || !spec.NativeSessionStarted || !spec.ForkSession {
			t.Fatalf("the turn does not fork the original: %+v", spec)
		}
	case <-time.After(machineWaitGuard):
		t.Fatal("no turn started")
	}
}

// A session made under a link to its folder is continued with the harness started in that spelling: the turn carries it, and a supervisor
// that does not know it is never given the turn (Claude would start in the canonical folder and not find the session).
func TestLaunchSpellingTurnWaitsForASupervisorThatKnowsIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	const original = "11111111-1111-4111-8111-111111111111"
	checkout, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_link','worker','Worker','worker','worker')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_link',?,?,?)`, f.project.ID, f.machineID, checkout)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := f.db.CreateAgent(ctx, store.Agent{Slug: "linked", ProjectID: f.project.ID, RoleID: "role_link", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.machineID, CheckoutID: "chk_link", KeyHash: "key", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(ctx, f.room.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		for _, statement := range []string{
			`INSERT INTO session(id,agent_id,generation,start_mode,started_at,native_session_id,native_session_started_at) VALUES ('ses_link','` + agent.ID + `',0,'native_resume','2026-10-13T00:00:00.000000000Z','` + original + `','2026-10-13T00:00:00.000000000Z')`,
			`UPDATE agent SET tool='claude',launch_path='` + link + `' WHERE id='` + agent.ID + `'`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if pending, err := f.db.AgentLaunchPathPending(ctx, agent.ID); err != nil || !pending {
		t.Fatalf("a session saved under a link does not need its spelling: %v %v", pending, err)
	}
	journal, err := supervisor.OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	tap := &specTap{Fake: adapter.NewFake(), specs: make(chan adapter.TurnSpec, 4)}
	tap.ScriptDefault(adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(`{"messages":[]}`)}}, adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, tap)
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = client.RunOnce(runCtx) }()
	select {
	case <-ready:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor did not connect")
	}
	f.srv.machineMu.Lock()
	conn := f.srv.machines[f.machineID]
	conn.supportsLaunchPath = false
	f.srv.machineMu.Unlock()
	if err := f.srv.Dispatch(ctx, protocol.Delivery{AgentID: agent.ID, Generation: 0, TurnID: "turn_never", Mode: adapter.ModeNormal, NativeSessionHandle: original, NativeSessionStarted: true, LaunchPath: link}); !errors.Is(err, ErrSupervisorUpdateNeeded) {
		t.Fatalf("a delivery with a launch spelling to a supervisor that does not know it: %v", err)
	}
	response := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "@linked please work"}, true)
	if response.StatusCode != 201 {
		t.Fatalf("post status=%d", response.StatusCode)
	}
	response.Body.Close()
	dispatcher := NewDispatcher(f.srv)
	for i := 0; i < 3; i++ {
		if count, err := dispatcher.Pass(ctx); err != nil || count != 0 {
			t.Fatalf("pass %d dispatched %d: %v", i, count, err)
		}
	}
	select {
	case spec := <-tap.specs:
		t.Fatalf("a supervisor that does not know the spelling was given a turn: %+v", spec)
	case <-time.After(300 * time.Millisecond):
	}
	messages, err := f.db.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	held := 0
	for _, message := range messages {
		if message.AuthorKind == "system" && strings.Contains(message.Body, "linked is held") && strings.Contains(message.Body, "link to its folder") {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("the hold was said %d times, want once", held)
	}
	f.srv.machineMu.Lock()
	conn.supportsLaunchPath = true
	f.srv.machineMu.Unlock()
	if count, err := dispatcher.Pass(ctx); err != nil || count != 1 {
		t.Fatalf("dispatch after the supervisor knows the spelling: %d %v", count, err)
	}
	select {
	case spec := <-tap.specs:
		if spec.CheckoutPath != checkout || spec.LaunchPath != link || spec.NativeSessionHandle != original {
			t.Fatalf("the turn does not start in the session's spelling: %+v", spec)
		}
	case <-time.After(machineWaitGuard):
		t.Fatal("no turn started")
	}
	// An agent switched to another tool no longer needs the spelling: with a supervisor that cannot start Claude in it, its turns are neither
	// held nor given one (the stored spelling is a Claude session's, and goes stale with the tool).
	f.srv.machineMu.Lock()
	conn.supportsLaunchPath = false
	f.srv.machineMu.Unlock()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET tool='codex' WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if pending, err := f.db.AgentLaunchPathPending(ctx, agent.ID); err != nil || pending {
		t.Fatalf("an agent switched away from Claude is still held for a spelling: %v %v", pending, err)
	}
}

// A late frame (an ack after a stop) for an agent that is already stopped is refused as unauthorized but is expected: no Sentry report. An unauthorized
// frame for anything else (an agent the store does not know, another machine's agent) is still reported. Not parallel: the reporter is process-wide.
func TestMachineLateFrameForStoppedAgentIsExpected(t *testing.T) {
	telemetry.ResetCaptured()
	transport := &captureTransport{}
	tel, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", Release: "v1", TracesSampleRate: 1}, transport)
	if err != nil || tel == nil {
		t.Fatalf("telemetry.Init: %v %v", tel, err)
	}
	t.Cleanup(tel.Close)
	f := newFixture(t)
	agent, _, _, _ := f.execution(t)
	ctx := context.Background()
	conn := &machineConn{id: f.machineID, bound: map[string]int{agent.ID: 0, "agt_unknown": 0}}
	frame := protocol.Frame{Type: protocol.Ack, AgentID: agent.ID, Generation: 0}
	if code, _ := f.srv.authorizeAgentFrame(ctx, conn, frame); code != "" {
		t.Fatalf("an active agent's frame was refused: %q", code)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET revoked_at='2026-10-13T00:00:00.000000000Z' WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, expected := f.srv.authorizeAgentFrame(ctx, conn, frame); code != "unauthorized" || !expected {
		t.Fatalf("a stopped agent's late frame: code=%q expected=%v", code, expected)
	}
	if code, expected := f.srv.authorizeAgentFrame(ctx, conn, protocol.Frame{Type: protocol.Ack, AgentID: "agt_unknown"}); code != "unauthorized" || expected {
		t.Fatalf("an unknown agent's frame must stay a reported unauthorized: code=%q expected=%v", code, expected)
	}
	other := &machineConn{id: "mch_other", bound: map[string]int{agent.ID: 0}}
	if code, expected := f.srv.authorizeAgentFrame(ctx, other, frame); code != "unauthorized" || expected {
		t.Fatalf("another machine's frame must stay a reported unauthorized: code=%q expected=%v", code, expected)
	}
}

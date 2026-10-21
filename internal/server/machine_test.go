package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/supervisor"
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
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not connect")
	}
	if err := f.srv.Dispatch(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
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
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop")
	}
}

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
	client.ReconnectDelay = 300 * time.Millisecond
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
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("initial supervisor connect timed out")
	}
	if err := f.srv.Dispatch(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
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
	close(gate)
	deadline = time.Now().Add(3 * time.Second)
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
	select {
	case <-ready:
	case <-time.After(4 * time.Second):
		t.Fatal("supervisor did not reconnect")
	}
	deadline = time.Now().Add(3 * time.Second)
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
	deadline = time.Now().Add(3 * time.Second)
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
	case <-done:
	case <-time.After(3 * time.Second):
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
	if _, err := journal.RecordAnswer(ctx, turn.ID, 0, digest, candidate); err != nil {
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
	t.Helper()
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID, JournalID: journalID, Agents: agents})
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
	return conn
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

func TestReplacedJournalNeverRequeuesOpenTurn(t *testing.T) {
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
	if err != nil || state.Exec != "lost" || state.Outcome != "unknown" || state.NoteState != "unknown" || !state.RecoveryBlock {
		t.Fatalf("replaced journal state=%+v %v", state, err)
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
	if !visible {
		t.Fatal("journal replacement produced no visible event")
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
	if _, err := j.RecordAnswer(ctx, turn.ID, 0, digest, answer); err != nil {
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
	case <-time.After(3 * time.Second):
		t.Fatal("receipt replay hello timed out")
	}
	pending, err := j.PendingAnswers(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("lost receipt not replayed; candidate still pending: %+v %v", pending, err)
	}
	deadline := time.Now().Add(3 * time.Second)
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
	case <-time.After(3 * time.Second):
		t.Fatal("replay client did not stop")
	}
	conn := f.dialMachine(t, nil)
	defer conn.Close(websocket.StatusNormalClosure, "")
	sendAnswer := func(candidate json.RawMessage, digest string) protocol.Frame {
		t.Helper()
		frame := protocol.Frame{V: protocol.Version, Type: protocol.Answer, AgentID: agent.ID, Generation: 0, TurnID: turn.ID, AnswerRev: 0, Digest: digest, Candidate: string(candidate)}
		raw, _ := json.Marshal(frame)
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
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("enqueue blocked for %s", elapsed)
	}
	reqCtx, cancel := context.WithTimeout(context.Background(), time.Second)
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
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor not ready")
	}
	if err := f.srv.Dispatch(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
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
	case <-time.After(3 * time.Second):
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
	bad := fmt.Sprintf(`{"messages":[{"room":%q,"body":"valid"},{"room":%q,"body":"bad","reply_to":"msg_missing"}]}`, f.room.ID, f.room.ID)
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
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor not ready")
	}
	if err := f.srv.Dispatch(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
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
	deadline = time.Now().Add(4 * time.Second)
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
	case <-time.After(3 * time.Second):
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
				t.Fatalf("timed out waiting for %s", what)
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
	if got := costSoFar(); got != 0.75 {
		t.Fatalf("cost after replay=%v, want each usage counted once (0.75)", got)
	}
	messages, err := f.db.ListMessagesAfter(ctx, f.room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, message := range messages {
		if strings.Contains(message.Body, "memory was trimmed") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("compaction notices=%d, want 1", notices)
	}
	cancel()
	<-done
}

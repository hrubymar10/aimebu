package server

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestOwnerRestartMarksLegacyCancelledTurnForContinuation(t *testing.T) {
	f := newFixture(t)
	agent, turn, _, _ := f.execution(t)
	f.srv.SetLocalSupervisorID(f.machineID)
	f.srv.LocalSupervisorRestart = func() error { return nil }
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.AppendEvent(store.Event{Kind: "turn_dispatch_requested", ProjectID: f.project.ID, RoomID: f.room.ID, AgentID: agent.ID, TurnID: turn.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	response := f.request("POST", "/api/machines/"+f.machineID+"/supervisor/restart", nil, true)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("restart request: HTTP %d", response.StatusCode)
	}
	exit := f.srv.localSupervisorRestartExit(turn.ID, adapter.ExitInfo{ExitCode: 130, CancelIntent: true})
	if exit.CancelIntent || !exit.Interrupted || exit.ErrorEvent == "" {
		t.Fatalf("legacy exit was not continued: %+v", exit)
	}
	other := f.srv.localSupervisorRestartExit("another-turn", adapter.ExitInfo{ExitCode: 130, CancelIntent: true})
	if !other.CancelIntent || other.Interrupted {
		t.Fatalf("unmarked owner cancel changed: %+v", other)
	}
}

func TestLocalSupervisorMismatchPausesDispatchAndOffersRestart(t *testing.T) {
	f := newFixture(t)
	f.srv.SetLocalSupervisorID(f.machineID)
	f.srv.Version = "new-build"
	f.srv.LocalSupervisorEnvironment = "new-environment"
	var requested atomic.Int32
	f.srv.LocalSupervisorRestart = func() error { requested.Add(1); return nil }
	dial := func(version, environment string, retire bool) *websocket.Conn {
		t.Helper()
		header := http.Header{}
		header.Set("X-Machine-ID", f.machineID)
		header.Set("Authorization", "Bearer "+f.machineKey)
		conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		hello, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID, JournalID: "local-journal", SupervisorVersion: version, SupervisorEnvironment: environment, SupportsRetireAtIdle: retire})
		if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
			t.Fatal(err)
		}
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var frame protocol.Frame
		if err := json.Unmarshal(raw, &frame); err != nil || frame.Type != protocol.HelloAck {
			t.Fatalf("hello acknowledgement = %+v, %v", frame, err)
		}
		return conn
	}
	old := dial("old-build", "old-environment", true)
	defer old.CloseNow()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, raw, err := old.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var retire protocol.Frame
	if err := json.Unmarshal(raw, &retire); err != nil || retire.Type != protocol.RetireAtIdle {
		t.Fatalf("retire request = %+v, %v", retire, err)
	}
	if _, err := f.srv.machineForDispatch(f.machineID); err != ErrMachineUnavailable {
		t.Fatalf("dispatch allowed during drain: %v", err)
	}
	machine, err := f.db.MachineByID(t.Context(), f.machineID)
	if err != nil || !f.srv.machineRow(machine).RestartPending {
		t.Fatalf("pending machine status: %v", err)
	}
	response := f.request("POST", "/api/machines/"+f.machineID+"/supervisor/restart", nil, true)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted || requested.Load() != 1 {
		t.Fatalf("restart action: HTTP %d, requests %d", response.StatusCode, requested.Load())
	}
	current := dial("new-build", "new-environment", true)
	defer current.CloseNow()
	// The server sends the acknowledgement before it lifts the pause, so the dial returning does not mean the matching hello has been applied.
	waitForGuard(t, 10*time.Second, func() bool {
		_, err := f.srv.machineForDispatch(f.machineID)
		return err == nil
	})
	if _, err := f.srv.machineForDispatch(f.machineID); err != nil {
		t.Fatalf("dispatch stayed paused after matching hello: %v", err)
	}
}

func TestUpgradedSupervisorWaitsForMatchingServerBuild(t *testing.T) {
	f := newFixture(t)
	f.srv.SetLocalSupervisorID(f.machineID)
	var cancelled atomic.Int32
	f.srv.LocalSupervisorLegacyDrainCancel = func() { cancelled.Add(1) }
	f.srv.Version = "old-server"
	f.srv.LocalSupervisorEnvironment = "server-environment"
	f.srv.LocalSupervisorDiskVersion = func() (string, error) { return "new-supervisor", nil }
	f.srv.LocalSupervisorRestart = func() error { t.Error("unexpected supervisor restart"); return nil }
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	hello, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID, JournalID: "local-journal", SupervisorVersion: "new-supervisor", SupervisorEnvironment: "new-environment", SupportsRetireAtIdle: true})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err := json.Unmarshal(raw, &ack); err != nil || ack.Type != protocol.HelloAck {
		t.Fatalf("hello acknowledgement = %+v, %v", ack, err)
	}
	if cancelled.Load() != 1 {
		t.Fatal("new supervisor did not cancel the legacy drain")
	}
	if _, err := f.srv.machineForDispatch(f.machineID); err != ErrMachineUnavailable {
		t.Fatalf("dispatch accepted a mismatched build: %v", err)
	}
	machine, err := f.db.MachineByID(t.Context(), f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	row := f.srv.machineRow(machine)
	if !row.UpgradeRequired || !row.RestartPending {
		t.Fatalf("upgrade status: %+v", row)
	}
	response := f.request("POST", "/api/machines/"+f.machineID+"/supervisor/restart", nil, true)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("restart action: HTTP %d", response.StatusCode)
	}
}

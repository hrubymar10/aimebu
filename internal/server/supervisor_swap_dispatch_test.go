package server

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// A swap keeps the previous instance dispatchable only until the replacement's hello is acked: an acked replacement speaks for the machine and
// holds no reference to the previous one (F003), while a replacement whose handshake never reaches the ack restores the previous instance.
func TestSupervisorSwapDropsThePreviousInstanceAfterTheAck(t *testing.T) {
	f := newFixture(t)
	f.srv.Version = "matching"
	old, _ := dialInstance(t, f, "old", "matching", nil, nil)
	f.srv.machineMu.Lock()
	var previous *machineConn
	if c := f.srv.machines[f.machineID]; c != nil && c.journalID == "old" {
		previous = c
	}
	f.srv.machineMu.Unlock()
	if previous == nil {
		t.Fatal("the first instance is not the machine's connection")
	}

	dial := func(journal string) *websocket.Conn {
		t.Helper()
		header := http.Header{}
		header.Set("X-Machine-ID", f.machineID)
		header.Set("Authorization", "Bearer "+f.machineKey)
		conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
		writeCommandFrame(t, conn, protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID,
			JournalID: journal, SupervisorVersion: "matching", SupportsRetireAtIdle: true, SupportsAgentKeys: true})
		return conn
	}
	pendingRetire := func() *machineConn {
		f.srv.machineMu.Lock()
		defer f.srv.machineMu.Unlock()
		if c := f.srv.machines[f.machineID]; c != nil {
			return c.pendingRetire
		}
		return nil
	}

	// A handshake that never reaches the ack leaves the previous instance in place: the closed replacement restores it, dispatchable.
	// spawnMu holds the handshake at its key grant, before the ack; the replacement is stopped there, and the lock released so the
	// handler can fail its ack and run the restore.
	f.srv.spawnMu.Lock()
	locked := true
	defer func() {
		if locked {
			f.srv.spawnMu.Unlock()
		}
	}()
	failed := dial("failed")
	waitForGuard(t, 3*time.Second, func() bool { return pendingRetire() != nil })
	f.srv.machineMu.Lock()
	failedConn := f.srv.machines[f.machineID]
	f.srv.machineMu.Unlock()
	failedConn.stop()
	failed.CloseNow()
	f.srv.spawnMu.Unlock()
	locked = false
	waitForGuard(t, 3*time.Second, func() bool {
		f.srv.machineMu.Lock()
		defer f.srv.machineMu.Unlock()
		return f.srv.machines[f.machineID] == previous && !previous.retiring && f.srv.retiringMachines[f.machineID]["old"] == nil
	})
	if c, err := f.srv.machineForDispatch(f.machineID); err != nil || c != previous {
		t.Fatalf("a failed handshake did not restore the previous instance: %+v %v", c, err)
	}

	// A replacement that is acked takes over: the previous instance is retired, and the reference that keeps it dispatchable is dropped.
	f.srv.spawnMu.Lock()
	locked = true
	current := dial("new")
	waitForGuard(t, 3*time.Second, func() bool { return pendingRetire() == previous })
	f.srv.spawnMu.Unlock()
	locked = false
	readCommandFrame(t, current, func(frame protocol.Frame) bool { return frame.Type == protocol.HelloAck })
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	waitForGuard(t, 3*time.Second, func() bool { return pendingRetire() == nil })
	if c, err := f.srv.machineForDispatch(f.machineID); err != nil || c.journalID != "new" {
		t.Fatalf("the acked replacement does not dispatch: %+v %v", c, err)
	}
}

func TestSupervisorSwapKeepsDispatchAvailable(t *testing.T) {
	f := newFixture(t)
	f.srv.Version = "matching"
	old, _ := dialInstance(t, f, "old", "matching", nil, nil)

	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	current, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = current.CloseNow() })

	// Hold the hello just before its ack so dispatch is exercised across the
	// entire replacement window, including the old instance's retirement.
	f.srv.spawnMu.Lock()
	locked := true
	defer func() {
		if locked {
			f.srv.spawnMu.Unlock()
		}
	}()
	writeCommandFrame(t, current, protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID,
		JournalID: "new", SupervisorVersion: "matching", SupportsRetireAtIdle: true, SupportsAgentKeys: true})
	waitForGuard(t, 3*time.Second, func() bool {
		f.srv.machineMu.Lock()
		defer f.srv.machineMu.Unlock()
		return f.srv.machines[f.machineID] != nil && f.srv.machines[f.machineID].journalID == "new" &&
			f.srv.retiringMachines[f.machineID]["old"] != nil
	})

	var unavailable atomic.Bool
	stop := make(chan struct{})
	started := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if c, err := f.srv.machineForDispatch(f.machineID); err != nil || c == nil {
				unavailable.Store(true)
			}
		}
	}()
	<-started
	time.Sleep(10 * time.Millisecond)
	f.srv.spawnMu.Unlock()
	locked = false
	readCommandFrame(t, current, func(frame protocol.Frame) bool { return frame.Type == protocol.HelloAck })
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	close(stop)
	<-finished
	if unavailable.Load() {
		t.Fatal("dispatch returned machine unavailable while a matching old instance remained connected during the swap")
	}
}

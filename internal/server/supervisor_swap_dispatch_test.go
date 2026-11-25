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

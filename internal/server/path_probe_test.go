package server

import (
	"context"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func TestRemotePathProbeRequiresSupervisorAnswer(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	machine := &machineConn{id: "mch_probe", supportsPathProbe: true, sendQueue: make(chan protocol.Frame, 1), done: make(chan struct{})}
	f.srv.machineMu.Lock()
	f.srv.machines[machine.id] = machine
	f.srv.machineMu.Unlock()
	go func() {
		request := <-machine.sendQueue
		if request.Type != protocol.PathProbe || request.CheckoutPath != "/work/alias" {
			close(machine.done)
			return
		}
		machine.harnessMu.Lock()
		waiter := machine.pathPending[request.ReqID]
		machine.harnessMu.Unlock()
		waiter <- protocol.Frame{V: protocol.Version, Type: protocol.PathResult, ReqID: request.ReqID, CanonicalPath: "/work/real"}
	}()
	got, err := f.srv.probeMachinePath(context.Background(), machine.id, "/work/alias")
	if err != nil || got != "/work/real" {
		t.Fatalf("probe = %q, %v", got, err)
	}
	other := &machineConn{id: "mch_old", sendQueue: make(chan protocol.Frame, 1), done: make(chan struct{})}
	f.srv.machineMu.Lock()
	f.srv.machines[other.id] = other
	f.srv.machineMu.Unlock()
	if _, err := f.srv.probeMachinePath(context.Background(), other.id, "/work/real"); err == nil || !strings.Contains(err.Error(), "path preflight support") {
		t.Fatalf("old supervisor error = %v", err)
	}
	if len(other.sendQueue) != 0 {
		t.Fatal("old supervisor was sent a frame it cannot handle")
	}
}

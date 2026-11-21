package server

import (
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func TestAgentClearContextEndpointPermissionsAndRevision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	status, card, body := f.spawnRequest(t, "clear-context-agent", "worker")
	if status != 201 {
		t.Fatalf("spawn = %d %+v", status, body)
	}
	viewerCookie, viewerCSRF := f.projectMember(t, "contextviewer", f.project.ID, "viewer")
	operatorCookie, operatorCSRF := f.projectMember(t, "contextoperator", f.project.ID, "operator")
	path := "/api/agents/" + card.ID + "/clear-context"
	viewer := f.requestAs(t, viewerCookie, viewerCSRF, "POST", path, map[string]any{"rev": card.DesiredRev})
	viewer.Body.Close()
	if viewer.StatusCode != 403 {
		t.Fatalf("viewer = %d", viewer.StatusCode)
	}
	operator := f.requestAs(t, operatorCookie, operatorCSRF, "POST", path, map[string]any{"rev": card.DesiredRev})
	operator.Body.Close()
	if operator.StatusCode != 200 {
		t.Fatalf("operator = %d", operator.StatusCode)
	}
	stale := f.request("POST", path, map[string]any{"rev": card.DesiredRev}, true)
	stale.Body.Close()
	if stale.StatusCode != 409 {
		t.Fatalf("stale revision = %d", stale.StatusCode)
	}
}

package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

// The interrupt endpoint of a running team flow change answers a request with no agent, and one while no change is running, with the
// documented errors instead of touching any agent.
func TestTeamFlowMigrationInterruptRefusals(t *testing.T) {
	f := newLegacyFixture(t)
	path := "/api/projects/" + f.project.ID + "/team-flow/migration/interrupt"
	if resp := f.request(http.MethodPost, path, map[string]any{}, true); resp.StatusCode != 400 {
		t.Fatalf("no agent_id: %d", resp.StatusCode)
	}
	if resp := f.request(http.MethodPost, path, map[string]any{"agent_id": "agt_none"}, true); resp.StatusCode != 404 {
		t.Fatalf("no migration: %d", resp.StatusCode)
	}
}

// Interrupt now sends the stop to the agent's machine: the turn is cut by the supervisor, not by the database row.
func TestTeamFlowMigrationInterruptSendsTheStopControl(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	ownerID := f.ownerUserID(t)
	target := store.TeamSetupTarget{MachineID: f.machineID, Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}
	var setup store.TeamSetupResult
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		setup, err = tx.CreateTeamSetup(f.project.ID, ownerID, store.TeamSetupInput{FlowKey: "leader-workers", Leader: target})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	leaderID := setup.Agents[0].AgentID
	in := store.TeamFlowMigrationInput{FlowKey: "leader-dispatcher", FlowRev: setup.Flow.Rev, Dispatcher: &target}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE session SET id='ses_flow' WHERE agent_id=? AND generation=0`, leaderID); err != nil {
			return err
		}
		_, err := tx.RecordTurn(store.Turn{AgentID: leaderID, SessionID: "ses_flow", Generation: 0, RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "running"})
		return err
	}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.BeginTeamFlowMigration(f.project.ID, ownerID, in)
		return err
	}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	for len(machine.sendQueue) > 0 { // frames of the setup are not what is asserted
		<-machine.sendQueue
	}
	resp := f.request(http.MethodPost, "/api/projects/"+f.project.ID+"/team-flow/migration/interrupt", map[string]any{"agent_id": leaderID}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("interrupt: %d", resp.StatusCode)
	}
	select {
	case frame := <-machine.sendQueue:
		if frame.Type != protocol.Control || frame.AgentID != leaderID || frame.DesiredState != "stopped" {
			t.Fatalf("frame sent to the machine: %+v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("no stop control reached the agent's machine")
	}
}

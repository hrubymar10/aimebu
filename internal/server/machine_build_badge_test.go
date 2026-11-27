package server

import (
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func TestOlderSupervisorBuildBadgeIsInformational(t *testing.T) {
	f := newFixture(t)
	f.srv.Version = "aimebu v2.0.0-alpha.17"
	agent, turn, _, delivery := f.execution(t)
	old, _ := dialInstance(t, f, "older", "aimebu v2.0.0-alpha.15", nil, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	if err := f.srv.Dispatch(t.Context(), delivery); err != nil {
		t.Fatal(err)
	}
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.Deliver && frame.TurnID == turn.ID })
	machine, err := f.db.MachineByID(t.Context(), f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	if row := f.srv.machineRow(machine); !row.OlderBuild {
		t.Fatalf("older machine has no badge: %+v", row)
	}
	resp := f.request("GET", "/api/machines/"+f.machineID+"/instances", nil, false)
	var listed instancesAnswer
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(listed.Instances) != 1 || !listed.Instances[0].OlderBuild {
		t.Fatalf("older instance badge: %d %+v", resp.StatusCode, listed)
	}
	current, _ := dialInstance(t, f, "current", "aimebu v2.0.0-alpha.17", nil, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	waitForActiveInstance(t, f, "current", "older")
	machine, err = f.db.MachineByID(t.Context(), f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	if row := f.srv.machineRow(machine); row.OlderBuild {
		t.Fatalf("current machine has stale badge: %+v", row)
	}
	resp = f.request("GET", "/api/machines/"+f.machineID+"/instances", nil, false)
	listed = instancesAnswer{}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(listed.Instances) != 2 || !listed.Instances[0].OlderBuild || listed.Instances[1].OlderBuild {
		t.Fatalf("instance badges after current hello: %d %+v", resp.StatusCode, listed)
	}
	if owner, err := f.db.TurnOwner(t.Context(), turn.ID); err != nil || owner != "older" {
		t.Fatalf("old instance lost live turn: %q %v", owner, err)
	}
	_ = current
}

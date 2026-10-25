package server

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

type machineStopResult struct {
	Stopped        []string      `json:"stopped"`
	AlreadyStopped int           `json:"already_stopped"`
	Forbidden      int           `json:"forbidden"`
	Failed         []stopFailure `json:"failed"`
}

func TestStopAllAgentsOnAMachine(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	var cards []store.AgentCard
	// Two roles, because a checkout takes one writer.
	for key, role := range map[string]string{"stop-machine-a": "worker", "stop-machine-b": "reviewer"} {
		status, card, raw := f.spawnRequest(t, key, role)
		if status != 201 {
			t.Fatalf("spawn %d %+v", status, raw)
		}
		cards = append(cards, card)
	}
	for len(machine.sendQueue) > 0 {
		<-machine.sendQueue
	}
	stop := func(machineID string) (int, machineStopResult) {
		resp := f.request("POST", "/api/machines/"+machineID+"/agents/stop", nil, true)
		defer resp.Body.Close()
		var result machineStopResult
		_ = json.NewDecoder(resp.Body).Decode(&result)
		return resp.StatusCode, result
	}

	// Someone who may not manage these agents stops none of them, and the machine is not told anything.
	cookie, csrf := registerSecondUser(t, f, "outsider")
	denied := f.requestAs(t, cookie, csrf, "POST", "/api/machines/"+f.machineID+"/agents/stop", nil)
	denied.Body.Close()
	if denied.StatusCode != 403 || len(machine.sendQueue) != 0 {
		t.Fatalf("outsider: status %d, %d frames", denied.StatusCode, len(machine.sendQueue))
	}
	for _, card := range cards {
		if got, err := f.db.AgentCardByID(t.Context(), card.ID); err != nil || got.Desired != "active" {
			t.Fatalf("an outsider stopped %s: %+v %v", card.ID, got, err)
		}
	}

	// The project's owner stops all of them, and the machine is told to stop each.
	status, result := stop(f.machineID)
	if status != 200 || len(result.Stopped) != 2 || result.Forbidden != 0 || len(result.Failed) != 0 {
		t.Fatalf("stop: %d %+v", status, result)
	}
	told := map[string]bool{}
	for len(told) < 2 {
		select {
		case frame := <-machine.sendQueue:
			if frame.Type == protocol.Control && frame.DesiredState == "stopped" {
				told[frame.AgentID] = true
			}
		case <-time.After(time.Second):
			t.Fatalf("the machine was told about %v", told)
		}
	}
	for _, card := range cards {
		if !told[card.ID] {
			t.Errorf("no stop control for %s", card.ID)
		}
		if got, err := f.db.AgentCardByID(t.Context(), card.ID); err != nil || got.Desired != "stopped" {
			t.Errorf("%s: %+v %v", card.ID, got, err)
		}
	}

	// Doing it again finds nothing to stop.
	if status, again := stop(f.machineID); status != 200 || len(again.Stopped) != 0 || again.AlreadyStopped != 2 {
		t.Fatalf("second stop: %d %+v", status, again)
	}
	if status, _ := stop("mch_nonexistent"); status != 404 {
		t.Fatalf("unknown machine: %d", status)
	}
}

// Someone who is not an owner or admin learns nothing about machines: an unknown id and a real one answer alike.
func TestStopAllOnAMachineTellsOutsidersNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	cookie, csrf := registerSecondUser(t, f, "outsider")
	for _, id := range []string{f.machineID, "mch_nonexistent"} {
		resp := f.requestAs(t, cookie, csrf, "POST", "/api/machines/"+id+"/agents/stop", nil)
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("machine %s for a non-admin: %d, want 403", id, resp.StatusCode)
		}
	}
}

// An admin stops the agents they could stop one by one and no others: a project they operate loses its agent, one they do not keeps it, and the
// answer counts each kind.
func TestStopAllOnAMachineCountsWhatItStoppedAndWhatItLeft(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 64), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	spawn := func(roomID, key, role string) store.AgentCard {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"role_key": role, "tool": "fake", "variant": "test", "model": "test"})
		req, _ := http.NewRequest("POST", f.http.URL+"/api/rooms/"+roomID+"/agents", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", key)
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var card store.AgentCard
		json.NewDecoder(resp.Body).Decode(&card)
		if resp.StatusCode != 201 {
			t.Fatalf("spawn %s: %d", key, resp.StatusCode)
		}
		return card
	}
	// Project A (the fixture's): a worker the admin will stop and a reviewer that is stopped already. Project B: a worker the admin may not touch.
	mine := spawn(f.room.ID, "stop-mine", "worker")
	spent := spawn(f.room.ID, "stop-spent", "reviewer")
	resp := f.request("POST", "/api/agents/"+spent.ID+"/control", map[string]any{"desired": "stopped", "rev": spent.DesiredRev}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stop the reviewer: %d", resp.StatusCode)
	}
	var other store.Project
	resp = f.request("POST", "/api/projects", map[string]string{"name": "other", "folder": t.TempDir()}, true)
	json.NewDecoder(resp.Body).Decode(&other)
	resp.Body.Close()
	var otherRoom store.Room
	resp = f.request("POST", "/api/projects/"+other.ID+"/rooms", map[string]string{"title": "work"}, true)
	json.NewDecoder(resp.Body).Decode(&otherRoom)
	resp.Body.Close()
	foreign := spawn(otherRoom.ID, "stop-foreign", "worker")

	cookie, csrf := registerSecondUser(t, f, "deputy")
	deputy, err := f.db.UserByLogin(t.Context(), "deputy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`UPDATE user SET admin=1 WHERE id=?`, deputy.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'operator')`, f.project.ID, deputy.ID); err != nil {
		t.Fatal(err)
	}
	resp = f.requestAs(t, cookie, csrf, "POST", "/api/machines/"+f.machineID+"/agents/stop", nil)
	var result machineStopResult
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(result.Stopped) != 1 || result.Stopped[0] != mine.ID || result.AlreadyStopped != 1 || result.Forbidden != 1 || len(result.Failed) != 0 {
		t.Fatalf("stop as an admin who operates one project: %d %+v", resp.StatusCode, result)
	}
	for id, want := range map[string]string{mine.ID: "stopped", spent.ID: "stopped", foreign.ID: "active"} {
		if got, err := f.db.AgentCardByID(t.Context(), id); err != nil || got.Desired != want {
			t.Errorf("%s is %q (%v), want %q", id, got.Desired, err, want)
		}
	}
}

// One agent at a time: a revision someone else changed is looked at again, an agent that left the machine is left alone, and a failure
// is reported as one rather than counted as stopped.
func TestStopAgentOnAMachineHandlesAChangedAgent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 64), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	spawn := func(key, role string) store.AgentRuntime {
		t.Helper()
		status, card, raw := f.spawnRequest(t, key, role)
		if status != 201 {
			t.Fatalf("spawn %d %+v", status, raw)
		}
		runtime, err := f.db.AgentRuntimeByID(t.Context(), card.ID)
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	// A stale revision: the first try is refused, the second uses what the agent has now.
	stale := spawn("stale-a", "worker")
	behind := stale
	behind.DesiredRev--
	if outcome, card, err := f.srv.stopAgentOnMachine(t.Context(), owner.ID, f.machineID, behind); outcome != stoppedNow || err != nil || card.ID != stale.ID || card.Desired != "stopped" {
		t.Fatalf("stale revision: outcome %v card %+v err %v", outcome, card, err)
	}
	// Told it is on another machine, an agent that is still running is left alone, not stopped from here.
	elsewhere := spawn("stale-b", "reviewer")
	behind = elsewhere
	behind.DesiredRev--
	if outcome, _, err := f.srv.stopAgentOnMachine(t.Context(), owner.ID, "mch_other", behind); outcome != stoppedAlready || err != nil {
		t.Fatalf("an agent that is not on this machine: outcome %v err %v", outcome, err)
	}
	if got, err := f.db.AgentCardByID(t.Context(), elsewhere.ID); err != nil || got.Desired != "active" {
		t.Fatalf("it was stopped from the wrong machine: %+v %v", got, err)
	}
	// An agent that does not exist is a failure with its reason, never "already stopped".
	ghost := store.AgentRuntime{ID: "agt_ghost", DesiredRev: 1}
	if outcome, _, err := f.srv.stopAgentOnMachine(t.Context(), owner.ID, f.machineID, ghost); outcome != stoppedFailed || err == nil {
		t.Fatalf("a missing agent: outcome %v err %v", outcome, err)
	}
}

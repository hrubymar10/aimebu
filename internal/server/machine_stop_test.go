package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestOfflineStopIsReplayedOnReconnectBeforeProjectDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	status, card, raw := f.spawnRequest(t, "offline-stop", "worker")
	if status != 201 {
		t.Fatalf("spawn %d %+v", status, raw)
	}
	f.srv.machineMu.Lock()
	delete(f.srv.machines, f.machineID)
	f.srv.machineMu.Unlock()
	resp := f.request("POST", "/api/agents/"+card.ID+"/control", map[string]any{"desired": "stopped", "rev": card.DesiredRev}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stop: %d", resp.StatusCode)
	}
	stopped, err := f.db.AgentCardByID(t.Context(), card.ID)
	if err != nil || stopped.Desired != "stopped" || stopped.Observed != "stopped" {
		t.Fatalf("offline stop: %+v %v", stopped, err)
	}
	resp = f.request("POST", "/api/projects/"+f.project.ID+"/archive", nil, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("archive: %d", resp.StatusCode)
	}
	deleteProject := func() (int, string) {
		t.Helper()
		resp := f.request("DELETE", "/api/projects/"+f.project.ID, map[string]string{"name": f.project.Name}, true)
		defer resp.Body.Close()
		var problem struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&problem)
		return resp.StatusCode, problem.Message
	}
	if got, message := deleteProject(); got != 409 || !strings.Contains(message, f.machineID) {
		t.Fatalf("delete before machine applied stop: %d %q", got, message)
	}

	conn := f.dialMachine(t, nil)
	readCtx, cancel := context.WithTimeout(t.Context(), machineWaitGuard)
	_, body, err := conn.Read(readCtx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	var control protocol.Frame
	if err := json.Unmarshal(body, &control); err != nil || control.Type != protocol.Control || control.AgentID != card.ID || control.DesiredState != "stopped" || control.DesiredRev != stopped.DesiredRev {
		t.Fatalf("replayed control: %+v %v", control, err)
	}
	ack, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Ack, AgentID: card.ID, Generation: stopped.Generation, Level: "control", Ref: fmt.Sprint(stopped.DesiredRev)})
	writeCtx, cancel := context.WithTimeout(t.Context(), machineWaitGuard)
	if err := conn.Write(writeCtx, websocket.MessageText, ack); err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(machineWaitGuard)
	for {
		pending, err := f.db.PendingStopControlsForMachine(t.Context(), f.machineID)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stop control was not applied")
		}
		time.Sleep(5 * time.Millisecond)
	}
	conn.Close(websocket.StatusNormalClosure, "")
	conn = f.dialMachine(t, nil)
	readCtx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
	_, _, err = conn.Read(readCtx)
	cancel()
	conn.Close(websocket.StatusNormalClosure, "")
	if err == nil {
		t.Fatal("already applied stop was replayed")
	}
	if got, _ := deleteProject(); got != 204 {
		t.Fatalf("delete after machine applied stop: %d", got)
	}
}

func TestReconnectReplaysMoreStopsThanMachineQueueCapacity(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	if _, err := f.exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_many_stops',?,?,?)`, f.project.ID, f.machineID, f.project.Folder); err != nil {
		t.Fatal(err)
	}
	const count = 120
	for i := range count {
		id := fmt.Sprintf("agt_stop_%03d", i)
		slug := fmt.Sprintf("stop%03d", i)
		if _, err := f.exec(`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,desired,desired_rev,observed,created_by,created_at)
			VALUES (?,?,?,'role_worker_default','fake','test','test','fake',?,'chk_many_stops',?,'stopped',1,'stopped','owner','now')`,
			id, slug, f.project.ID, f.machineID, id); err != nil {
			t.Fatal(err)
		}
	}
	conn := f.dialMachine(t, nil)
	defer conn.Close(websocket.StatusNormalClosure, "")
	seen := make(map[string]bool, count)
	for range count {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		_, raw, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read after %d stops: %v", len(seen), err)
		}
		var control protocol.Frame
		if err := json.Unmarshal(raw, &control); err != nil || control.Type != protocol.Control || control.DesiredState != "stopped" || seen[control.AgentID] {
			t.Fatalf("stop %d: %+v %v", len(seen), control, err)
		}
		seen[control.AgentID] = true
		ack, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Ack, AgentID: control.AgentID, Generation: control.Generation, Level: "control", Ref: fmt.Sprint(control.DesiredRev)})
		ctx, cancel = context.WithTimeout(t.Context(), 10*time.Second)
		if err := conn.Write(ctx, websocket.MessageText, ack); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		pending, err := f.db.PendingStopControlsForMachine(t.Context(), f.machineID)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d stops remain unacknowledged", len(pending))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A replayed stop acked by an older supervisor stays pending even though the agent is already observed stopped.
func TestOldSupervisorReplayAckIsNotProofOfAStop(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	if _, err := f.exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_old_stop',?,?,?)`, f.project.ID, f.machineID, f.project.Folder); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,desired,desired_rev,observed,created_by,created_at)
		VALUES ('agt_old_stop','oldstop',?,'role_worker_default','fake','test','test','fake',?,'chk_old_stop','key','stopped',1,'stopped','owner','now')`, f.project.ID, f.machineID); err != nil {
		t.Fatal(err)
	}
	conn, snapshot := f.dialMachineWithStopReplayAck(t, "old-journal", nil, false)
	defer conn.Close(websocket.StatusNormalClosure, "")
	if len(snapshot.Desired) != 1 || snapshot.Desired[0].DesiredRev != 0 || snapshot.Desired[0].Desired != "stopped" {
		t.Fatalf("legacy snapshot: %+v", snapshot.Desired)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	_, raw, err := conn.Read(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	var control protocol.Frame
	if err := json.Unmarshal(raw, &control); err != nil || control.Type != protocol.Control || control.DesiredRev != 1 || control.DesiredState != "stopped" {
		t.Fatalf("legacy stop replay: %+v %v", control, err)
	}
	// The agent is observed stopped (the server marks it so when it queues the stop), yet this ack answers a replay: an older supervisor acks
	// as soon as it sees a control, possibly while the process lives, so a replay ack proves nothing.
	ack, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Ack, AgentID: "agt_old_stop", Level: "control", Ref: "1"})
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	if err := conn.Write(ctx, websocket.MessageText, ack); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	time.Sleep(20 * time.Millisecond)
	pending, err := f.db.PendingStopControlsForMachine(t.Context(), f.machineID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("old supervisor's unsent stop ack counted: %d %v", len(pending), err)
	}
}

// An older supervisor acks a control as soon as it sees it. The ack of an ordinary live stop counts once the agent is observed stopped
// (its process has been told to end), so a project whose agents were stopped normally can still be deleted.
func TestLegacyLiveStopAckCountsAfterObservedStop(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	stub := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = stub
	f.srv.machineMu.Unlock()
	status, card, raw := f.spawnRequest(t, "legacy-live", "worker")
	if status != 201 {
		t.Fatalf("spawn %d %+v", status, raw)
	}
	f.srv.machineMu.Lock()
	delete(f.srv.machines, f.machineID)
	f.srv.machineMu.Unlock()
	conn := f.dialMachineWithStopReplay(t, "legacy-live-journal", nil, false)
	defer conn.Close(websocket.StatusNormalClosure, "")
	// The stop below must be a live one: wait until the connection has queued what was pending when it connected, or a stop that lands in
	// that window is delivered as a replay (whose legacy ack is deliberately not trusted).
	var connected *machineConn
	for deadline := time.Now().Add(3 * time.Second); connected == nil || connected == stub; {
		f.srv.machineMu.Lock()
		connected = f.srv.machines[f.machineID]
		f.srv.machineMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("the machine connection was not registered")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-connected.replayDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the connection did not finish replaying pending stops")
	}
	resp := f.request("POST", "/api/agents/"+card.ID+"/control", map[string]any{"desired": "stopped", "rev": card.DesiredRev}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("live stop: %d", resp.StatusCode)
	}
	// The machine first gets the spawn it missed; the live stop follows.
	var control protocol.Frame
	for control.Type != protocol.Control {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		_, raw2, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw2, &control); err != nil {
			t.Fatal(err)
		}
	}
	if control.AgentID != card.ID || control.DesiredState != "stopped" {
		t.Fatalf("live control: %+v", control)
	}
	stopped, err := f.db.AgentCardByID(t.Context(), card.ID)
	if err != nil || stopped.Observed != "stopped" {
		t.Fatalf("live stop observed state: %+v %v", stopped, err)
	}
	if pending, err := f.db.PendingStopControlsForMachine(t.Context(), f.machineID); err != nil || len(pending) != 1 {
		t.Fatalf("before the ack the stop is pending: %d %v", len(pending), err)
	}
	ack, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Ack, AgentID: control.AgentID, Generation: control.Generation, Level: "control", Ref: fmt.Sprint(control.DesiredRev)})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	if err := conn.Write(ctx, websocket.MessageText, ack); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for {
		pending, err := f.db.PendingStopControlsForMachine(t.Context(), f.machineID)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("legacy live stop ack was not applied")
		}
		time.Sleep(5 * time.Millisecond)
	}
	archive := f.request("POST", "/api/projects/"+f.project.ID+"/archive", nil, true)
	archive.Body.Close()
	if archive.StatusCode != 200 {
		t.Fatalf("archive: %d", archive.StatusCode)
	}
	deleted := f.request("DELETE", "/api/projects/"+f.project.ID, map[string]string{"name": f.project.Name}, true)
	deleted.Body.Close()
	if deleted.StatusCode != 204 {
		t.Fatalf("delete after a legacy live stop: %d", deleted.StatusCode)
	}
}

func TestReplayReservesQueueSpaceForLiveMachineTraffic(t *testing.T) {
	t.Parallel()
	c := &machineConn{sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{})}
	for range 16 {
		c.sendQueue <- protocol.Frame{Type: protocol.Pong}
	}
	finished := make(chan error, 1)
	go func() {
		finished <- c.sendReplayControl(t.Context(), protocol.Frame{Type: protocol.Control, AgentID: "agent", DesiredRev: 1, DesiredState: "stopped"})
	}()
	for _, frame := range []protocol.Frame{{Type: protocol.Ping}, {Type: protocol.Deliver}} {
		if err := c.send(frame); err != nil {
			t.Fatalf("live frame blocked by replay: %v", err)
		}
	}
	select {
	case err := <-finished:
		t.Fatalf("replay filled the reserved queue space early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	for range 3 {
		<-c.sendQueue
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("replay did not resume after the writer drained the queue")
	}
}

func TestRevokingOfflineMachineUnblocksPendingStopDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	status, card, raw := f.spawnRequest(t, "revoke-offline", "worker")
	if status != 201 {
		t.Fatalf("spawn %d %+v", status, raw)
	}
	f.srv.machineMu.Lock()
	delete(f.srv.machines, f.machineID)
	f.srv.machineMu.Unlock()
	resp := f.request("POST", "/api/agents/"+card.ID+"/control", map[string]any{"desired": "stopped", "rev": card.DesiredRev}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stop: %d", resp.StatusCode)
	}
	resp = f.request("POST", "/api/projects/"+f.project.ID+"/archive", nil, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("archive: %d", resp.StatusCode)
	}
	if _, err := f.exec(`UPDATE machine SET revoked_at='2026-10-24T00:00:00Z' WHERE id=?`, f.machineID); err != nil {
		t.Fatal(err)
	}
	resp = f.request("DELETE", "/api/projects/"+f.project.ID, map[string]string{"name": f.project.Name}, true)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete after machine revoked: %d", resp.StatusCode)
	}
}

func TestArchivedRecoveryAbandonSendsOneStopAndReplaySendsNone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, _ := f.execution(t)
	if _, err := f.exec(`UPDATE turn SET exec='lost',outcome='unknown',settle='pending' WHERE id=?`, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`UPDATE agent SET observed='lost' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,turn_id,requester_kind,requester_id,kind,created_at)
		VALUES ('inb_stop_http',?,?,? ,?,'system','system','recovery','now')`, f.project.ID, f.room.ID, agent.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.AppendEvent(store.Event{Kind: "inbox_item_created", ProjectID: f.project.ID, RoomID: f.room.ID,
			PayloadJSON: `{"inbox_item_id":"inb_stop_http"}`})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	archive := f.request("POST", "/api/projects/"+f.project.ID+"/archive", nil, true)
	archive.Body.Close()
	if archive.StatusCode != 200 {
		t.Fatalf("archive: %d", archive.StatusCode)
	}
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{agent.ID: 0}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	post := func() int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"choice": "abandon", "rev": 0})
		req, _ := http.NewRequest("POST", f.http.URL+"/api/inbox/inb_stop_http/decision", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", "stop-http-once")
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := post(); status != 201 {
		t.Fatalf("abandon: %d", status)
	}
	if len(machine.sendQueue) != 1 {
		t.Fatalf("abandon sent %d frames, want one", len(machine.sendQueue))
	}
	control := <-machine.sendQueue
	if control.Type != protocol.Control || control.AgentID != agent.ID || control.DesiredState != "stopped" {
		t.Fatalf("abandon control: %+v", control)
	}
	if status := post(); status != 200 || len(machine.sendQueue) != 0 {
		t.Fatalf("replay: status %d, %d frames", status, len(machine.sendQueue))
	}
}

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
		case <-time.After(machineWaitGuard):
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

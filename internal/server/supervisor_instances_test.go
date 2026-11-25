package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func dialInstance(t *testing.T, f *fixture, journal, build string, held []string, agents []protocol.HelloAgent) (*websocket.Conn, protocol.Frame) {
	return dialInstanceEnv(t, f, journal, build, "", held, agents)
}

func dialInstanceEnv(t *testing.T, f *fixture, journal, build, environment string, held []string, agents []protocol.HelloAgent) (*websocket.Conn, protocol.Frame) {
	t.Helper()
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	writeCommandFrame(t, conn, protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID, JournalID: journal,
		SupervisorVersion: build, SupervisorEnvironment: environment, SupportsRetireAtIdle: true, SupportsAgentKeys: true, SupportsWarm: true, SupportsStopReplay: true, SupportsCommands: true, SupportsContainerCommands: true,
		HeldKeys: held, Agents: agents})
	ack := readCommandFrame(t, conn, func(frame protocol.Frame) bool { return frame.Type == protocol.HelloAck })
	return conn, ack
}

func TestRemoteSupervisorInstancePrefersNewestMatchingBuild(t *testing.T) {
	f := newFixture(t)
	f.srv.Version = "wanted"
	f.srv.LocalSupervisorEnvironment = "server-only"
	old, _ := dialInstanceEnv(t, f, "old", "older", "remote-a", nil, nil)
	if c, err := f.srv.machineForDispatch(f.machineID); err != nil || c.journalID != "old" {
		t.Fatalf("first remote fallback %+v %v", c, err)
	}
	matching, _ := dialInstanceEnv(t, f, "match", "wanted", "remote-b", nil, nil)
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	newer, _ := dialInstanceEnv(t, f, "newer", "other", "remote-c", nil, nil)
	readCommandFrame(t, newer, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	if c, err := f.srv.machineForDispatch(f.machineID); err != nil || c.journalID != "match" {
		t.Fatalf("matching remote did not stay active %+v %v", c, err)
	}
	_ = matching
}

func TestRemoteSupervisorInstanceUsesNewestWhenNoneMatches(t *testing.T) {
	f := newFixture(t)
	f.srv.Version = "wanted"
	old, _ := dialInstance(t, f, "old", "older", nil, nil)
	newer, _ := dialInstance(t, f, "newer", "newer", nil, nil)
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	if c, err := f.srv.machineForDispatch(f.machineID); err != nil || c.journalID != "newer" {
		t.Fatalf("newest remote fallback %+v %v", c, err)
	}
	_ = newer
}

func TestLocalSupervisorInstanceRequiresBuildAndEnvironment(t *testing.T) {
	f := newFixture(t)
	f.srv.SetLocalSupervisorID(f.machineID)
	f.srv.Version = "wanted"
	f.srv.LocalSupervisorEnvironment = "desired"
	wrong, _ := dialInstanceEnv(t, f, "wrong", "wanted", "different", nil, nil)
	readCommandFrame(t, wrong, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	if _, err := f.srv.machineForDispatch(f.machineID); err != ErrMachineUnavailable {
		t.Fatalf("local mismatch accepted: %v", err)
	}
	correct, _ := dialInstanceEnv(t, f, "correct", "wanted", "desired", nil, nil)
	if c, err := f.srv.machineForDispatch(f.machineID); err != nil || c.journalID != "correct" {
		t.Fatalf("matching local %+v %v", c, err)
	}
	_ = correct
}

func TestSupervisorInstancesRouteActiveOwnerAndControls(t *testing.T) {
	f := newFixture(t)
	f.srv.Version = "first"
	agent, turn, _, delivery := f.execution(t)
	old, _ := dialInstance(t, f, "first-journal", "first", nil, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	if err := f.srv.Dispatch(t.Context(), delivery); err != nil {
		t.Fatal(err)
	}
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.Deliver && frame.TurnID == turn.ID })
	owner, err := f.db.TurnOwner(t.Context(), turn.ID)
	if err != nil || owner != "first-journal" {
		t.Fatalf("turn owner %q: %v", owner, err)
	}
	f.srv.Version = "second"
	current, _ := dialInstance(t, f, "second-journal", "second", nil, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	active, err := f.srv.machineForDispatch(f.machineID)
	if err != nil || active.journalID != "second-journal" {
		t.Fatalf("active instance %+v: %v", active, err)
	}
	if _, err := f.srv.machineForOwner(f.machineID, "first-journal"); err != nil {
		t.Fatal(err)
	}
	writeCommandFrame(t, current, protocol.Frame{V: protocol.Version, Type: protocol.Ack, AgentID: agent.ID, Generation: 0, TurnID: turn.ID, Level: "accepted"})
	denied := readCommandFrame(t, current, func(frame protocol.Frame) bool { return frame.Type == protocol.Error && frame.TurnID == turn.ID })
	if denied.Code != "forbidden" {
		t.Fatalf("wrong-owner ack: %+v", denied)
	}
	writeCommandFrame(t, old, protocol.Frame{V: protocol.Version, Type: protocol.Ack, AgentID: agent.ID, Generation: 0, TurnID: turn.ID, Level: "accepted"})
	if err := f.srv.sendMachineControl(f.machineID, protocol.Frame{V: protocol.Version, Type: protocol.Control, AgentID: agent.ID, Generation: 0, DesiredRev: 1, DesiredState: "stopped"}); err != nil {
		t.Fatal(err)
	}
	for _, conn := range []*websocket.Conn{old, current} {
		control := readCommandFrame(t, conn, func(frame protocol.Frame) bool { return frame.Type == protocol.Control && frame.AgentID == agent.ID })
		if control.DesiredState != "stopped" {
			t.Fatalf("control %+v", control)
		}
	}
	if f.srv.controlAckOwner(t.Context(), active, agent.ID) {
		t.Fatal("idle active instance may not acknowledge the old process stop")
	}
	retired, _ := f.srv.machineForOwner(f.machineID, "first-journal")
	if !f.srv.controlAckOwner(t.Context(), retired, agent.ID) {
		t.Fatal("turn owner cannot acknowledge stop")
	}
}

func TestSupervisorInstancesRouteCommandLogAndEndToOwner(t *testing.T) {
	f := newFixture(t)
	f.seed()
	agent, key := f.createToolAgent(t, "role_worker_default")
	f.srv.Version = "first"
	old, _ := dialInstance(t, f, "first-journal", "first", []string{agent.ID}, nil)
	answer, run := f.startCommandCall(t, old, key, "owner-command")
	writeCommandFrame(t, old, commandStartedFrame(run))
	if call := waitCommandCall(t, answer); call.status != http.StatusCreated {
		t.Fatalf("start %d: %s", call.status, call.body)
	}
	f.srv.Version = "second"
	current, _ := dialInstance(t, f, "second-journal", "second", nil, nil)
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	job, err := f.db.CommandJobByAnyAgent(t.Context(), run.JobID)
	if err != nil || job.JournalID != "first-journal" {
		t.Fatalf("job owner %+v %v", job, err)
	}
	type commandAnswer struct {
		frame protocol.Frame
		err   error
	}
	answered := make(chan commandAnswer, 1)
	go func() {
		frame, err := f.srv.machineCommand(context.Background(), f.machineID,
			protocol.Frame{V: protocol.Version, Type: protocol.CommandLogRead, JobID: run.JobID, ReqID: "owner-log", LogPath: job.LogPath}, 5*time.Second)
		answered <- commandAnswer{frame, err}
	}()
	logRead := readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.CommandLogRead })
	writeCommandFrame(t, old, protocol.Frame{V: protocol.Version, Type: protocol.CommandLogResult, ReqID: logRead.ReqID, Result: "ok", Log: "old output"})
	select {
	case result := <-answered:
		if result.err != nil || result.frame.Log != "old output" {
			t.Fatalf("owner log %+v %v", result.frame, result.err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("owner did not answer log")
	}
	writeCommandFrame(t, current, commandEndedFrame(run, run.Generation, 0))
	denied := readCommandFrame(t, current, func(frame protocol.Frame) bool { return frame.Type == protocol.Error && frame.JobID == run.JobID })
	if denied.Code != "forbidden" {
		t.Fatalf("wrong-owner command end %+v", denied)
	}
	writeCommandFrame(t, old, commandEndedFrame(run, run.Generation, 0))
	waitCommandAck(t, old, run.JobID)
}

func TestSupervisorInstancesGateWarmHandoverAndGrantKeyBeforeDelivery(t *testing.T) {
	f := newFixture(t)
	f.srv.Version = "first"
	agent, _, _, delivery := f.execution(t)
	key, err := f.srv.Auth.IssueAgentKey(t.Context(), agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := dialInstance(t, f, "first-journal", "first", []string{agent.ID}, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	writeCommandFrame(t, old, protocol.Frame{V: protocol.Version, Type: protocol.WarmStateFrame, Warm: &protocol.WarmReport{Seq: 1, Agents: []protocol.WarmAgent{{AgentID: agent.ID, State: "idle"}}, Processes: 1}})
	deadline := time.Now().Add(3 * time.Second)
	for {
		oldConn, _ := f.srv.machineForOwner(f.machineID, "first-journal")
		oldConn.warmMu.Lock()
		seen := oldConn.warm != nil
		oldConn.warmMu.Unlock()
		if seen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("warm state was not received")
		}
		time.Sleep(time.Millisecond)
	}
	f.srv.Version = "second"
	current, ack := dialInstance(t, f, "second-journal", "second", nil, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	if len(ack.AgentKeys) != 0 {
		t.Fatalf("key rotated while the old process is warm: %+v", ack.AgentKeys)
	}
	if err := f.srv.Auth.VerifyAgentKey(t.Context(), agent.ID, key); err != nil {
		t.Fatalf("old process key revoked: %v", err)
	}
	blocked, err := f.srv.handoverBlocked(t.Context(), f.machineID, agent.ID)
	if err != nil || !blocked {
		t.Fatalf("warm handover gate %v: %v", blocked, err)
	}
	writeCommandFrame(t, old, protocol.Frame{V: protocol.Version, Type: protocol.WarmStateFrame, Warm: &protocol.WarmReport{Seq: 2, Agents: []protocol.WarmAgent{}, Processes: 0}})
	deadline = time.Now().Add(3 * time.Second)
	for {
		blocked, err = f.srv.handoverBlocked(t.Context(), f.machineID, agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("warm handover gate did not open")
		}
		time.Sleep(time.Millisecond)
	}
	if err := f.srv.Dispatch(t.Context(), delivery); err != nil {
		t.Fatal(err)
	}
	keys := readCommandFrame(t, current, func(frame protocol.Frame) bool { return frame.Type == protocol.AgentKeys })
	if len(keys.AgentKeys) != 1 || keys.AgentKeys[0].AgentID != agent.ID {
		t.Fatalf("handover keys %+v", keys.AgentKeys)
	}
	readCommandFrame(t, current, func(frame protocol.Frame) bool { return frame.Type == protocol.Deliver })
	if err := f.srv.Auth.VerifyAgentKey(context.Background(), agent.ID, keys.AgentKeys[0].Key); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.Auth.VerifyAgentKey(context.Background(), agent.ID, key); err == nil {
		t.Fatal("old key still authenticates after handover")
	}
}

func TestSupervisorInstancesMachineWideStateComesFromActive(t *testing.T) {
	f := newFixture(t)
	f.srv.Version = "current"
	old, _ := dialInstance(t, f, "old-journal", "old", nil, nil)
	current, _ := dialInstance(t, f, "new-journal", "current", nil, nil)
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	writeCommandFrame(t, current, protocol.Frame{V: protocol.Version, Type: protocol.HarnessStatus,
		Harnesses: []protocol.HarnessRow{{HarnessSetting: protocol.HarnessSetting{Tool: "fake", Variant: "new"}}}})
	waitForGuard(t, 3*time.Second, func() bool {
		rows, err := f.db.MachineHarnesses(t.Context(), f.machineID)
		return err == nil && len(rows) == 1 && rows[0].Variant == "new"
	})
	writeCommandFrame(t, old, protocol.Frame{V: protocol.Version, Type: protocol.HarnessStatus,
		Harnesses: []protocol.HarnessRow{{HarnessSetting: protocol.HarnessSetting{Tool: "fake", Variant: "old"}}}})
	time.Sleep(50 * time.Millisecond)
	rows, err := f.db.MachineHarnesses(t.Context(), f.machineID)
	if err != nil || len(rows) != 1 || rows[0].Variant != "new" {
		t.Fatalf("retiring status replaced harnesses: %+v %v", rows, err)
	}
	machine, err := f.db.MachineByID(t.Context(), f.machineID)
	if err != nil || machine.SupervisorVersion.String != "current" {
		t.Fatalf("retiring hello replaced version: %+v %v", machine.SupervisorVersion, err)
	}
}

func TestSupervisorInstancesSumWarmReports(t *testing.T) {
	f := newFixture(t)
	f.srv.Version = "first"
	old, _ := dialInstance(t, f, "old", "first", nil, nil)
	f.srv.Version = "second"
	current, _ := dialInstance(t, f, "new", "second", nil, nil)
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	writeCommandFrame(t, old, protocol.Frame{V: protocol.Version, Type: protocol.WarmStateFrame,
		Warm: &protocol.WarmReport{Seq: 1, Processes: 1, Agents: []protocol.WarmAgent{{AgentID: "old-agent", State: "idle"}}}})
	writeCommandFrame(t, current, protocol.Frame{V: protocol.Version, Type: protocol.WarmStateFrame,
		Warm: &protocol.WarmReport{Seq: 1, Processes: 2}})
	waitForGuard(t, 3*time.Second, func() bool {
		f.srv.warmSeenMu.Lock()
		defer f.srv.warmSeenMu.Unlock()
		return f.srv.warmSeen[f.machineID].count == 3
	})
}

func TestSupervisorInstancesFrameCodec(t *testing.T) {
	raw, err := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.AgentKeys, DrainOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"drain_only":true`) || !strings.Contains(string(raw), `"type":"agent_keys"`) {
		t.Fatalf("new fields missing: %s", raw)
	}
}

func TestRetiredSupervisorGraceIsAbsentFromIsolationAPI(t *testing.T) {
	f := newFixture(t)
	response := f.request(http.MethodGet, "/api/agent-isolation", nil, false)
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, exists := body["supervisor_drain_grace_minutes"]; exists {
		t.Fatal("removed setting was returned")
	}
	response = f.request(http.MethodPut, "/api/agent-isolation", map[string]any{"supervisor_drain_grace_minutes": 1}, true)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("removed setting was accepted: %d", response.StatusCode)
	}
}

// run_command picks the instance once, when it creates the job: a hello that moves the active instance on before the run request is sent must
// not send the command to the new instance while the job names the old one.
func TestCommandRunGoesToTheInstanceTheJobNames(t *testing.T) {
	f := newFixture(t)
	f.seed()
	agent, _ := f.createToolAgent(t, "role_worker_default")
	f.srv.Version = "first"
	old, _ := dialInstance(t, f, "first-journal", "first", []string{agent.ID}, nil)
	job, _, err := f.db.CreateCommandJob(t.Context(), store.CommandJob{AgentID: agent.ID, MachineID: f.machineID, JournalID: "first-journal", ProjectID: f.project.ID,
		RoomID: f.room.ID, Cmd: "make test", CheckoutPath: "/tmp/toolbox"}, store.RequestKey{ActorKind: "agent", ActorID: agent.ID, Kind: "command_run", Key: "swap", Digest: "swap"})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Version = "second"
	current, _ := dialInstance(t, f, "second-journal", "second", nil, nil)
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	if active, err := f.srv.machineForDispatch(f.machineID); err != nil || active.journalID != "second-journal" {
		t.Fatalf("active instance %+v: %v", active, err)
	}
	answered := make(chan error, 1)
	go func() {
		_, err := f.srv.machineCommand(context.Background(), f.machineID, protocol.Frame{V: protocol.Version, Type: protocol.CommandRun, ReqID: job.ID, JobID: job.ID,
			AgentID: agent.ID, Command: &protocol.CommandSpec{JobID: job.ID, AgentID: agent.ID, Cmd: job.Cmd, CheckoutPath: "/tmp/toolbox"}}, 3*time.Second)
		answered <- err
	}()
	run := readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.CommandRun })
	if run.JobID != job.ID {
		t.Fatalf("run frame %+v", run)
	}
	writeCommandFrame(t, old, commandStartedFrame(run))
	if err := <-answered; err != nil {
		t.Fatal(err)
	}
	// The new instance never got the request: its next frame must not be a run.
	readCtx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	for {
		_, data, err := current.Read(readCtx)
		if err != nil {
			break
		}
		var frame protocol.Frame
		if json.Unmarshal(data, &frame) == nil && frame.Type == protocol.CommandRun {
			t.Fatalf("the new instance was sent the command: %+v", frame)
		}
	}
}

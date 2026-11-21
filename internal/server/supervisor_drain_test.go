package server

import (
	"context"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/supervisor"
)

// countRows counts the rows a condition matches. The store's transaction offers Exec only, so a no-op update counts them.
func (f *fixture) countRows(t *testing.T, table, column, where string, args ...any) int {
	t.Helper()
	var count int64
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		result, err := tx.Exec(`UPDATE `+table+` SET `+column+`=`+column+` WHERE `+where, args...)
		if err != nil {
			return err
		}
		count, err = result.RowsAffected()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return int(count)
}

func (f *fixture) setDrainGrace(t *testing.T, seconds string) {
	t.Helper()
	if err := f.db.SetGlobalSettingValue(t.Context(), supervisorDrainGraceKey, seconds); err != nil {
		t.Fatal(err)
	}
}

// The whole bounded drain, with a real supervisor on each side of the replacement. Two agents run a turn on a supervisor whose build
// then stops matching the server's. It is asked to retire at idle and local dispatch is held; when the grace runs out both turns get a
// turn_interrupt, end interrupted (not cancelled), and the old supervisor, now idle, exits. Each turn is continued by itself on the new
// supervisor, also the one whose chain had already used its one automatic continuation, and no recovery card is raised.
func TestBoundedDrainInterruptsRunningTurnsAndContinuesThemOnTheNewSupervisor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.Version = "new-build"
	f.seed()
	ctx := context.Background()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_drain','worker','Worker','worker','worker')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_drain',?,?,?)`, f.project.ID, f.machineID, "/tmp/drain")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agents := map[string]store.Agent{}
	for _, slug := range []string{"one", "two"} {
		agent, err := f.db.CreateAgent(ctx, store.Agent{Slug: slug, ProjectID: f.project.ID, RoleID: "role_drain", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.machineID, CheckoutID: "chk_drain", KeyHash: "key", CreatedBy: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.AddMember(ctx, f.room.ID, "agent", agent.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.db.Tx(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES (?, ?,0,'fresh_with_history',?)`, "ses_drain_"+slug, agent.ID, "2026-10-13T00:00:00.000000000Z")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		agents[agent.ID] = agent
	}
	journal, err := supervisor.OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()

	// The old supervisor: its turns never end by themselves.
	oldTap := &specTap{Fake: adapter.NewFake(), specs: make(chan adapter.TurnSpec, 4)}
	oldTap.ScriptDefault(adapter.Step{Kind: adapter.StepHang})
	old := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, oldTap)
	old.SupervisorVersion = "old-build"
	old.ReconnectDelay = 50 * time.Millisecond
	old.OnError = func(error) {}
	oldReady := make(chan struct{}, 8)
	old.OnReady = func() { oldReady <- struct{}{} }
	oldCtx, stopOld := context.WithCancel(ctx)
	defer stopOld()
	oldDone := make(chan error, 1)
	go func() { oldDone <- old.Run(oldCtx) }()
	waitReady := func(ready chan struct{}, what string) {
		t.Helper()
		select {
		case <-ready:
		case <-time.After(machineWaitGuard):
			t.Fatalf("%s did not connect", what)
		}
	}
	// Until the server takes this machine for its own local one, a build that differs is nobody's business: the turns start.
	waitReady(oldReady, "the old supervisor")
	response := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "everyone please work"}, true)
	var message store.Message
	if err := json.NewDecoder(response.Body).Decode(&message); err != nil || response.StatusCode != 201 {
		t.Fatalf("post: %d %v", response.StatusCode, err)
	}
	response.Body.Close()
	dispatcher := NewDispatcher(f.srv)
	if count, err := dispatcher.Pass(ctx); err != nil || count != 2 {
		t.Fatalf("first dispatch=%d %v", count, err)
	}
	original := map[string]string{} // turn -> agent
	for range 2 {
		select {
		case spec := <-oldTap.specs:
			original[spec.TurnID] = spec.Agent.ID
		case <-time.After(machineWaitGuard):
			t.Fatal("a turn did not start on the old supervisor")
		}
	}
	// One of the two chains has used its one automatic continuation already: a cut-off nobody asked for would now raise a recovery card.
	var spent string
	for turnID := range original {
		spent = turnID
		break
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.AppendEvent(store.Event{Kind: "turn_limit_continuation", ProjectID: f.project.ID, RoomID: f.room.ID, AgentID: original[spent], TurnID: spent, PayloadJSON: `{}`})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The server now runs another build than its local supervisor, which finds out on its next connection.
	f.setDrainGrace(t, "1")
	f.srv.SetLocalSupervisorID(f.machineID)
	conn, err := f.srv.machineForID(f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	conn.stop()
	waitReady(oldReady, "the old supervisor, again")
	if _, err := f.srv.machineForDispatch(f.machineID); err != ErrMachineUnavailable {
		t.Fatalf("dispatch allowed during the drain: %v", err)
	}
	select {
	case err := <-oldDone:
		if err != nil {
			t.Fatalf("the old supervisor ended with %v", err)
		}
	case <-time.After(machineWaitGuard):
		t.Fatal("the old supervisor did not retire after its turns were interrupted")
	}
	if waited := time.Since(began); waited < time.Second {
		t.Fatalf("the turns were interrupted after %s, before the grace was over", waited)
	}
	for turnID := range original {
		if got := f.countRows(t, "event", "kind", `turn_id=? AND kind='turn_interrupt_requested' AND json_extract(payload_json,'$.purpose')='supervisor_drain' AND json_extract(payload_json,'$.disposition')='continue'`, turnID); got < 1 {
			t.Fatalf("turn %s has no recorded interrupt request", turnID)
		}
		if got := f.countRows(t, "turn", "exec", `id=? AND exec='exited' AND cancel_intent=0 AND terminal_event=0`, turnID); got != 1 {
			t.Fatalf("turn %s did not end interrupted", turnID)
		}
		if got := f.countRows(t, "event", "kind", `kind='turn_interrupted_continuation' AND json_extract(payload_json,'$.origin_turn_id')=? AND json_extract(payload_json,'$.requested')='1'`, turnID); got != 1 {
			t.Fatalf("turn %s has %d requested continuations", turnID, got)
		}
	}
	if items, err := f.db.InboxForUser(ctx, f.ownerUserID(t)); err != nil || len(items) != 0 {
		t.Fatalf("the drain raised inbox items: %+v %v", items, err)
	}

	// The replacement: the server's own build. Dispatch is free again and each turn's continuation runs there.
	newTap := &specTap{Fake: adapter.NewFake(), specs: make(chan adapter.TurnSpec, 4)}
	newTap.ScriptDefault(adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer, Answer: json.RawMessage(`{"messages":[]}`)}}, adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	replacement := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, newTap)
	replacement.SupervisorVersion = "new-build"
	replacement.OnError = func(error) {}
	newReady := make(chan struct{}, 8)
	replacement.OnReady = func() { newReady <- struct{}{} }
	newCtx, stopNew := context.WithCancel(ctx)
	defer stopNew()
	go func() { _ = replacement.RunOnce(newCtx) }()
	waitReady(newReady, "the new supervisor")
	if _, err := f.srv.machineForDispatch(f.machineID); err != nil {
		t.Fatalf("dispatch stayed held after the replacement connected: %v", err)
	}
	if count, err := dispatcher.Pass(ctx); err != nil || count != 2 {
		t.Fatalf("continuation dispatch=%d %v", count, err)
	}
	continued := map[string]bool{}
	for range 2 {
		select {
		case spec := <-newTap.specs:
			if _, old := original[spec.TurnID]; old {
				t.Fatalf("an interrupted turn was started again as itself: %s", spec.TurnID)
			}
			continued[spec.Agent.ID] = true
		case <-time.After(machineWaitGuard):
			t.Fatal("a continuation did not start on the new supervisor")
		}
	}
	if len(continued) != 2 {
		t.Fatalf("continuations ran for %v, want both agents", continued)
	}
	deadline := time.Now().Add(machineWaitGuard)
	for agentID := range agents {
		for {
			note, err := f.db.NoteForAgentMessage(ctx, agentID, message.ID)
			if err == nil && note.State == "done" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the request of agent %s was not finished by its continuation: %+v %v", agentID, note, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// drainSocket connects as the server's local supervisor with a build that does not match, reporting one running turn (and the commands it
// still runs), and returns once it was asked to retire at idle.
func (f *fixture) drainSocket(t *testing.T, turnInterrupt bool, agent store.Agent, turn store.Turn, commands []protocol.CommandOpen) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	writeCommandFrame(t, conn, protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID, JournalID: "journal-commands", SupervisorVersion: "old-build",
		SupportsRetireAtIdle: true, SupportsStopReplay: true, SupportsCommands: true, SupportsContainerCommands: true, SupportsTurnInterrupt: turnInterrupt, CommandOpen: commands,
		Agents: []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0, OpenTurns: []protocol.OpenTurn{{TurnID: turn.ID, Exec: "running", ProcessAlive: true}}}}})
	readCommandFrame(t, conn, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	return conn
}

// framesFor reads what the server sends for a while and returns the frames of the kinds asked for.
func framesFor(t *testing.T, conn *websocket.Conn, wait time.Duration, kinds ...string) []protocol.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	var got []protocol.Frame
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return got // the wait is over (the read's deadline closes the connection: it is the last thing a test does with it)
		}
		var frame protocol.Frame
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatal(err)
		}
		for _, kind := range kinds {
			if frame.Type == kind {
				got = append(got, frame)
			}
		}
	}
}

// runningExecution is the fixture's turn, admitted and running on the machine. The machine has connected once before, so that its journal
// is known to the server and the turn is the journal's own when the draining connection reports it.
func (f *fixture) runningExecution(t *testing.T) (store.Agent, store.Turn) {
	t.Helper()
	agent, turn, note, _ := f.execution(t)
	_ = f.dialCommandMachine(t, nil, nil).CloseNow()
	f.startExecution(t, agent, turn, note)
	return agent, turn
}

func (f *fixture) startExecution(t *testing.T, agent store.Agent, turn store.Turn, note store.Note) {
	t.Helper()
	if err := f.db.AdmitTurn(t.Context(), store.Admission{TurnID: turn.ID, AgentID: agent.ID, MachineID: f.machineID, Generation: 0, DesiredRev: 0, NoteIDs: []string{note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckTurn(t.Context(), turn.ID, agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
}

// When the grace is over the frame goes to a supervisor that can take it, for a turn that still runs, and to nothing else: a grace of 0
// waits without limit as before, an older supervisor is never sent a frame it does not know, and a turn that ended in time is left alone.
func TestBoundedDrainSendsTheInterruptOnlyWhereItApplies(t *testing.T) {
	t.Parallel()
	for name, item := range map[string]struct {
		grace         string
		turnInterrupt bool
		finishFirst   bool
		interrupted   bool
	}{
		"grace over":                      {"1", true, false, true},
		"grace of 0":                      {"0", true, false, false},
		"supervisor without the frame":    {"1", false, false, false},
		"turn finished before the timer":  {"1", true, true, false},
		"a saved value that is not valid": {"-5", true, false, false}, // read as the default of five minutes: nothing within the test
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.Version = "new-build"
			agent, turn := f.runningExecution(t)
			f.setDrainGrace(t, item.grace)
			f.srv.SetLocalSupervisorID(f.machineID)
			conn := f.drainSocket(t, item.turnInterrupt, agent, turn, nil)
			if item.finishFirst {
				writeCommandFrame(t, conn, protocol.Frame{V: protocol.Version, Type: protocol.Process, AgentID: agent.ID, Generation: 0, TurnID: turn.ID, Exit: &adapter.ExitInfo{TerminalEvent: true}})
				readCommandFrame(t, conn, func(frame protocol.Frame) bool {
					return frame.Type == protocol.Ack && frame.Level == "process" && frame.TurnID == turn.ID
				})
			}
			// The case is only worth anything if the turn really is running on that connection (or really has ended).
			wantExec := "running"
			if item.finishFirst {
				wantExec = "exited"
			}
			if f.countRows(t, "turn", "exec", `id=? AND exec=?`, turn.ID, wantExec) != 1 {
				t.Fatalf("the turn is not %s after the hello", wantExec)
			}
			frames := framesFor(t, conn, 2500*time.Millisecond, protocol.TurnInterrupt)
			if !item.interrupted {
				if len(frames) != 0 || f.countRows(t, "event", "kind", `turn_id=? AND kind='turn_interrupt_requested'`, turn.ID) != 0 {
					t.Fatalf("the turn was interrupted: %+v", frames)
				}
				if f.countRows(t, "turn", "exec", `id=? AND exec=?`, turn.ID, wantExec) != 1 {
					t.Fatalf("the turn did not stay %s", wantExec)
				}
				return
			}
			if len(frames) != 1 || frames[0].TurnID != turn.ID || frames[0].AgentID != agent.ID || frames[0].ReqID == "" || frames[0].Reason == "" {
				t.Fatalf("turn_interrupt frames: %+v", frames)
			}
		})
	}
}

// A long command outside a turn is not the drain's to end: when the grace is over the turn is interrupted and the command is left
// running, which keeps the old supervisor up (it exits only when idle) until the command ends.
func TestBoundedDrainLeavesALongCommandRunning(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.Version = "new-build"
	agent, turn, note, _ := f.execution(t)
	_, key := f.createToolAgent(t, "role_t6")
	first := f.dialCommandMachine(t, nil, []protocol.CommandExit{})
	answer, run := f.startCommandCall(t, first, key, "drain-long")
	writeCommandFrame(t, first, commandStartedFrame(run))
	if call := waitCommandCall(t, answer); call.status != 201 {
		t.Fatalf("run_command %d %s", call.status, call.body)
	}
	_ = first.CloseNow()
	f.startExecution(t, agent, turn, note) // the turn starts after that connection: its hello reported no turn
	f.setDrainGrace(t, "1")
	f.srv.SetLocalSupervisorID(f.machineID)
	conn := f.drainSocket(t, true, agent, turn, []protocol.CommandOpen{{JobID: run.JobID, AgentID: run.AgentID, Generation: run.Generation, Pid: 4242, LogPath: run.Command.LogPath, StartedAt: "2026-11-19T10:00:00.000000000Z"}})
	frames := framesFor(t, conn, 2500*time.Millisecond, protocol.TurnInterrupt, protocol.CommandCancel)
	if len(frames) != 1 || frames[0].Type != protocol.TurnInterrupt || frames[0].TurnID != turn.ID {
		t.Fatalf("frames after the grace: %+v", frames)
	}
	job, err := f.db.CommandJobByAnyAgent(t.Context(), run.JobID)
	if err != nil || job.State != "running" {
		t.Fatalf("the command was not left running: %+v %v", job, err)
	}
}

// The drain grace is the owner's: five minutes until saved, in minutes, 0 waits without limit, bounded, and nobody else changes it.
func TestDrainGraceSettingIsOwnerOnly(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	read := func(who adminActor) any {
		t.Helper()
		code, body := w.json(t, who, "GET", "/api/agent-isolation", nil)
		if code != 200 {
			t.Fatalf("%s reads %d", who.name, code)
		}
		return body["supervisor_drain_grace_minutes"]
	}
	if minutes := read(w.viewer); minutes != float64(5) {
		t.Fatalf("default: %v", minutes)
	}
	for _, who := range []adminActor{w.admin, w.pOwner, w.oper, w.viewer, w.out} {
		if code := w.call(t, who, "PUT", "/api/agent-isolation", map[string]int{"supervisor_drain_grace_minutes": 1}); code != 403 {
			t.Errorf("%s changed the drain grace: %d", who.name, code)
		}
	}
	// The last one times sixty wraps around to a small number: the bound is checked before any multiplication.
	for _, bad := range []int{-1, 1441, math.MaxInt64/60 + 2, math.MaxInt64} {
		if code := w.call(t, w.owner, "PUT", "/api/agent-isolation", map[string]int{"supervisor_drain_grace_minutes": bad}); code != 400 {
			t.Errorf("%d minutes: %d", bad, code)
		}
	}
	if seconds, err := w.f.srv.drainGraceSeconds(t.Context()); err != nil || seconds != 300 {
		t.Fatalf("a refused write changed the grace: %d %v", seconds, err)
	}
	for _, minutes := range []int{12, 0} {
		if code := w.call(t, w.owner, "PUT", "/api/agent-isolation", map[string]int{"supervisor_drain_grace_minutes": minutes}); code != 200 {
			t.Fatalf("the owner sets %d: %d", minutes, code)
		}
		if got := read(w.viewer); got != float64(minutes) {
			t.Fatalf("read back %v, want %d", got, minutes)
		}
		if seconds, err := w.f.srv.drainGraceSeconds(t.Context()); err != nil || seconds != minutes*60 {
			t.Fatalf("stored as seconds: %d %v", seconds, err)
		}
	}
	if _, body := w.json(t, w.owner, "GET", "/api/agent-isolation", nil); body["warm_idle_saved"] != false {
		t.Fatalf("saving the grace saved a warm window: %v", body["warm_idle_saved"])
	}
}

func TestWarmIdleMinutesRejectsOverflow(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	// The first value wraps to a small positive number when multiplied by sixty.
	for _, minutes := range []int{math.MaxInt64/30 + 1, math.MaxInt64} {
		if code := w.call(t, w.owner, "PUT", "/api/agent-isolation", map[string]int{"warm_idle_minutes": minutes}); code != 400 {
			t.Errorf("%d minutes: %d, want 400", minutes, code)
		}
	}
	if _, saved, err := w.f.srv.warmIdleSetting(t.Context()); err != nil || saved {
		t.Fatalf("a refused write saved a warm window: %v %v", saved, err)
	}
}

// drainFrames runs the drain's sweep against a machine that takes turn interrupts and answers nothing, and returns the frames it was sent.
func drainFrames(t *testing.T, f *fixture, machine *machineConn) []protocol.Frame {
	t.Helper()
	f.srv.turnInterruptWait = 100 * time.Millisecond
	for len(machine.sendQueue) > 0 {
		<-machine.sendQueue
	}
	f.srv.interruptDrainingTurns(t.Context(), machine)
	var frames []protocol.Frame
	for len(machine.sendQueue) > 0 {
		frames = append(frames, <-machine.sendQueue)
	}
	return frames
}

// The drain does not reach into a team flow change: the turn of an agent the change holds is left to the change's own interrupt.
func TestBoundedDrainLeavesATeamFlowChangesTurnAlone(t *testing.T) {
	t.Parallel()
	f, machine, _, turnID := interruptFlowFixture(t, true)
	if frames := drainFrames(t, f, machine); len(frames) != 0 {
		t.Fatalf("the drain sent frames for a migrating agent's turn: %+v", frames)
	}
	if got := f.countRows(t, "event", "kind", `turn_id=? AND kind='turn_interrupt_requested'`, turnID); got != 0 {
		t.Fatalf("the drain recorded %d requests for a migrating agent's turn", got)
	}
	if got := f.countRows(t, "turn", "exec", `id=? AND exec='running'`, turnID); got != 1 {
		t.Fatal("the turn is not running any more")
	}
}

// A turn a person already asked to be interrupted keeps that request: the drain only sends the frame again, and the requeue stands.
func TestBoundedDrainKeepsAnEarlierInterruptRequest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, note, _ := f.execution(t)
	f.startExecution(t, agent, turn, note)
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{}, supportsTurnInterrupt: true}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	if _, err := f.db.RequestTurnInterrupt(t.Context(), store.TurnInterruptRequest{TurnID: turn.ID, ActorKind: "user", ActorID: f.ownerUserID(t), Purpose: "test", Disposition: store.TurnInterruptRequeue}); err != nil {
		t.Fatal(err)
	}
	frames := drainFrames(t, f, machine)
	if len(frames) != 1 || frames[0].Type != protocol.TurnInterrupt || frames[0].TurnID != turn.ID {
		t.Fatalf("frames: %+v", frames)
	}
	if got := f.countRows(t, "event", "kind", `turn_id=? AND kind='turn_interrupt_requested'`, turn.ID); got != 1 {
		t.Fatalf("%d requests recorded, want the person's alone", got)
	}
	if got := f.countRows(t, "event", "kind", `turn_id=? AND kind='turn_interrupt_requested' AND json_extract(payload_json,'$.disposition')='requeue'`, turn.ID); got != 1 {
		t.Fatal("the person's requeue was replaced")
	}
	// The turn ends interrupted: its notes are queued again and no continuation is made, as the person asked.
	if err := f.db.RecordProcessInfo(t.Context(), turn.ID, agent.ID, 0, adapter.ExitInfo{ExitCode: 130, ErrorEvent: "the turn was interrupted on request", Interrupted: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.countRows(t, "turn", "exec", `parent_turn_id=?`, turn.ID); got != 0 {
		t.Fatalf("%d continuation turns were made", got)
	}
	if got := f.countRows(t, "note", "state", `id=? AND state='queued'`, note.ID); got != 1 {
		t.Fatal("the turn's note was not queued again")
	}
}

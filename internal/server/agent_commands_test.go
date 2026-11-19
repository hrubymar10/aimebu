package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

// dialCommandMachine connects a supervisor that runs commands, with what its hello says it still runs and still owes.
func (f *fixture) dialCommandMachine(t *testing.T, open []protocol.CommandOpen, exits []protocol.CommandExit) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID, JournalID: "journal-commands",
		SupportsStopReplay: true, SupportsCommands: true, CommandOpen: open, CommandExits: exits})
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	if frame := readCommandFrame(t, conn, func(frame protocol.Frame) bool { return frame.Type == protocol.HelloAck }); frame.Type != protocol.HelloAck {
		t.Fatalf("hello_ack=%+v", frame)
	}
	return conn
}

// readCommandFrame reads until a frame the test waits for, skipping the rest (pings, spawns, controls).
func readCommandFrame(t *testing.T, conn *websocket.Conn, want func(protocol.Frame) bool) protocol.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for a frame: %v", err)
		}
		var frame protocol.Frame
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatal(err)
		}
		if want(frame) {
			return frame
		}
	}
}

func writeCommandFrame(t *testing.T, conn *websocket.Conn, frame protocol.Frame) {
	t.Helper()
	raw, _ := json.Marshal(frame)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
}

type commandCall struct {
	status int
	body   []byte
}

// startCommandCall calls run_command in the background and returns the run frame the machine receives.
func (f *fixture) startCommandCall(t *testing.T, conn *websocket.Conn, key, requestKey string) (chan commandCall, protocol.Frame) {
	t.Helper()
	answer := make(chan commandCall, 1)
	go func() {
		resp := f.toolPost(t, "/api/agent/commands", key, requestKey, map[string]any{"cmd": "make test", "room_id": f.room.ID})
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		answer <- commandCall{resp.StatusCode, body}
	}()
	run := readCommandFrame(t, conn, func(frame protocol.Frame) bool { return frame.Type == protocol.CommandRun })
	if run.Command == nil || run.JobID == "" {
		t.Fatalf("run frame %+v", run)
	}
	return answer, run
}

func waitCommandCall(t *testing.T, answer chan commandCall) commandCall {
	t.Helper()
	select {
	case call := <-answer:
		return call
	case <-time.After(20 * time.Second):
		t.Fatal("run_command never answered")
		return commandCall{}
	}
}

func commandStartedFrame(run protocol.Frame) protocol.Frame {
	return protocol.Frame{V: protocol.Version, Type: protocol.CommandStarted, ReqID: run.ReqID, JobID: run.JobID, AgentID: run.AgentID,
		Generation: run.Generation, Result: "ok", VisibleLogPath: run.Command.LogPath, VisibleCwd: run.Command.CheckoutPath}
}

func commandEndedFrame(run protocol.Frame, generation, code int) protocol.Frame {
	exit := protocol.CommandExit{JobID: run.JobID, AgentID: run.AgentID, Generation: generation, ExitCode: &code,
		StartedAt: "2026-11-19T10:00:00.000000000Z", EndedAt: "2026-11-19T10:00:01.000000000Z", Tail: "ok"}
	return protocol.Frame{V: protocol.Version, Type: protocol.CommandEnded, AgentID: run.AgentID, Generation: generation, JobID: run.JobID, CommandExit: &exit}
}

func waitCommandAck(t *testing.T, conn *websocket.Conn, jobID string) {
	t.Helper()
	frame := readCommandFrame(t, conn, func(frame protocol.Frame) bool {
		return (frame.Type == protocol.Ack && frame.Level == "command_exit") || (frame.Type == protocol.Error && frame.JobID == jobID)
	})
	if frame.Type != protocol.Ack || frame.JobID != jobID {
		t.Fatalf("the end was not acknowledged: %+v", frame)
	}
}

func commandNotes(t *testing.T, f *fixture, agentID string) int {
	t.Helper()
	var count int64
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		// The store's transaction offers Exec only: a no-op update counts the rows it matches.
		result, err := tx.Exec(`UPDATE note SET wake_reason=wake_reason WHERE agent_id=? AND wake_reason='command_finished'`, agentID)
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

// A context clear or a harness switch moves the agent's generation on while its command keeps running. The command's end still belongs to
// that agent and this machine, so it is recorded and wakes the agent, instead of being refused as stale forever (the job would stay
// running, count against the agent's cap and be impossible to cancel).
func TestCommandEndIsAcceptedAfterAGenerationChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, key := f.createToolAgent(t, "role_worker_default")
	conn := f.dialCommandMachine(t, nil, nil)
	answer, run := f.startCommandCall(t, conn, key, "gen-change")
	writeCommandFrame(t, conn, commandStartedFrame(run))
	call := waitCommandCall(t, answer)
	if call.status != 201 || !strings.Contains(string(call.body), `"state":"running"`) {
		t.Fatalf("run_command %d %s", call.status, call.body)
	}
	if _, err := f.exec(`UPDATE agent SET generation=generation+1 WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	writeCommandFrame(t, conn, commandEndedFrame(run, run.Generation, 0))
	waitCommandAck(t, conn, run.JobID)
	job, err := f.db.CommandJobByAnyAgent(t.Context(), run.JobID)
	if err != nil || job.State != "done" || job.ExitCode == nil || *job.ExitCode != 0 {
		t.Fatalf("job %+v err=%v", job, err)
	}
	if commandNotes(t, f, agent.ID) != 1 {
		t.Fatal("the agent was not woken by its command's end")
	}
}

// Another machine cannot report the end of a job that is not its own.
func TestCommandEndFromAnotherMachineIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, key := f.createToolAgent(t, "role_worker_default")
	conn := f.dialCommandMachine(t, nil, nil)
	answer, run := f.startCommandCall(t, conn, key, "other-machine")
	writeCommandFrame(t, conn, commandStartedFrame(run))
	if call := waitCommandCall(t, answer); call.status != 201 {
		t.Fatalf("run_command %d %s", call.status, call.body)
	}
	if _, err := f.exec(`UPDATE command_job SET machine_id='mch_elsewhere' WHERE id=?`, run.JobID); err != nil {
		t.Fatal(err)
	}
	writeCommandFrame(t, conn, commandEndedFrame(run, run.Generation, 0))
	frame := readCommandFrame(t, conn, func(frame protocol.Frame) bool {
		return (frame.Type == protocol.Ack && frame.Level == "command_exit") || frame.Type == protocol.Error
	})
	if frame.Type != protocol.Error || frame.Code != "forbidden" {
		t.Fatalf("a foreign job's end must be refused: %+v", frame)
	}
	if job, err := f.db.CommandJobByAnyAgent(t.Context(), run.JobID); err != nil || job.State != "running" || commandNotes(t, f, agent.ID) != 0 {
		t.Fatalf("job %+v err=%v", job, err)
	}
}

// A very fast command can end before its start is recorded: the supervisor's wait goroutine reports the end before the frame loop answers
// the run. The call still succeeds, with the job as it is now.
func TestFastCommandEndBeforeItsStartStillSucceeds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, key := f.createToolAgent(t, "role_worker_default")
	conn := f.dialCommandMachine(t, nil, nil)
	answer, run := f.startCommandCall(t, conn, key, "fast")
	writeCommandFrame(t, conn, commandEndedFrame(run, run.Generation, 0))
	waitCommandAck(t, conn, run.JobID)
	writeCommandFrame(t, conn, commandStartedFrame(run))
	call := waitCommandCall(t, answer)
	if call.status != 201 {
		t.Fatalf("run_command %d %s", call.status, call.body)
	}
	var job store.CommandJob
	if err := json.Unmarshal(call.body, &job); err != nil || job.State != "done" || job.VisibleLogPath == "" {
		t.Fatalf("job %+v err=%v", job, err)
	}
	if commandNotes(t, f, agent.ID) != 1 {
		t.Fatal("the agent was not woken by its command's end")
	}
}

// A run the machine never answered (it went away) is not a refusal: the job stays queued, the call says the start is unconfirmed, and the
// machine's next hello settles it. A job the machine runs becomes running and its end is still recorded; one it never started fails and
// wakes the agent; a running job it knows nothing of is lost.
func TestUnansweredCommandStartIsSettledByTheNextHello(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, key := f.createToolAgent(t, "role_worker_default")
	conn := f.dialCommandMachine(t, nil, nil)
	answer, ran := f.startCommandCall(t, conn, key, "ran")
	answerNever, never := f.startCommandCall(t, conn, key, "never")
	_ = conn.Close(websocket.StatusNormalClosure, "")
	for _, pending := range []chan commandCall{answer, answerNever} {
		call := waitCommandCall(t, pending)
		if call.status != 202 || !strings.Contains(string(call.body), `"start_unconfirmed":true`) || !strings.Contains(string(call.body), `"state":"queued"`) {
			t.Fatalf("an unanswered start must not be a refusal: %d %s", call.status, call.body)
		}
	}
	conn = f.dialCommandMachine(t, []protocol.CommandOpen{{JobID: ran.JobID, AgentID: agent.ID, Pid: 42, StartedAt: "2026-11-19T10:00:00.000000000Z"},
		{JobID: never.JobID, AgentID: agent.ID, Pid: 43}}, nil)
	for _, id := range []string{ran.JobID, never.JobID} {
		if job, err := f.db.CommandJobByAnyAgent(t.Context(), id); err != nil || job.State != "running" {
			t.Fatalf("a job the machine runs is running: %+v err=%v", job, err)
		}
	}
	// Then its supervisor loses one of them (a new journal, say): that running job can never be reported now.
	_ = conn.Close(websocket.StatusNormalClosure, "")
	conn = f.dialCommandMachine(t, []protocol.CommandOpen{{JobID: ran.JobID, AgentID: agent.ID, Pid: 42}}, nil)
	if job, err := f.db.CommandJobByAnyAgent(t.Context(), never.JobID); err != nil || job.State != "lost" {
		t.Fatalf("a running job the machine no longer knows is lost: %+v err=%v", job, err)
	}
	writeCommandFrame(t, conn, commandEndedFrame(ran, ran.Generation, 3))
	waitCommandAck(t, conn, ran.JobID)
	if job, err := f.db.CommandJobByAnyAgent(t.Context(), ran.JobID); err != nil || job.State != "done" || job.ExitCode == nil || *job.ExitCode != 3 {
		t.Fatalf("the late end was not recorded: %+v err=%v", job, err)
	}

	answer, unknown := f.startCommandCall(t, conn, key, "unknown")
	_ = conn.Close(websocket.StatusNormalClosure, "")
	if call := waitCommandCall(t, answer); call.status != 202 {
		t.Fatalf("run_command %d %s", call.status, call.body)
	}
	f.dialCommandMachine(t, nil, nil)
	if job, err := f.db.CommandJobByAnyAgent(t.Context(), unknown.JobID); err != nil || job.State != "failed" || job.Error == "" {
		t.Fatalf("a job the machine never started fails: %+v err=%v", job, err)
	}
	if got := commandNotes(t, f, agent.ID); got != 3 {
		t.Fatalf("each settled job wakes the agent once: %d notes", got)
	}
}

// A job cancelled while its start was unconfirmed is killed when the machine turns out to run it: on a late start, and on the next hello.
func TestCommandCancelledBeforeItsConfirmedStartIsKilled(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, key := f.createToolAgent(t, "role_worker_default")
	conn := f.dialCommandMachine(t, nil, nil)
	answer, run := f.startCommandCall(t, conn, key, "cancel-late")
	cancelResp := f.toolPost(t, "/api/agent/commands/"+run.JobID+"/cancel", key, "cancel-1", map[string]any{})
	cancelResp.Body.Close()
	if cancelResp.StatusCode != 200 {
		t.Fatalf("cancel %d", cancelResp.StatusCode)
	}
	writeCommandFrame(t, conn, commandStartedFrame(run))
	kill := readCommandFrame(t, conn, func(frame protocol.Frame) bool { return frame.Type == protocol.CommandCancel })
	if kill.JobID != run.JobID {
		t.Fatalf("cancel frame %+v", kill)
	}
	if call := waitCommandCall(t, answer); call.status != 201 || !strings.Contains(string(call.body), `"state":"cancelled"`) {
		t.Fatalf("run_command %d %s", call.status, call.body)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	conn = f.dialCommandMachine(t, []protocol.CommandOpen{{JobID: run.JobID, AgentID: run.AgentID, Pid: 42}}, nil)
	kill = readCommandFrame(t, conn, func(frame protocol.Frame) bool { return frame.Type == protocol.CommandCancel })
	if kill.JobID != run.JobID {
		t.Fatalf("cancel frame after hello %+v", kill)
	}
}

package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// dialRetiringInstance dials the machine socket as a second instance of the machine that the server holds as retiring: its hello
// names a build the server does not run, so restartPending is true for it (machine.go) and it lands in the retiring set.
// dialInstanceBuild dials the machine socket as an instance that reports the given build, so the server's matching-build rule sees it.
func (f *fixture) dialInstanceBuild(t *testing.T, journal, build string, agents []protocol.HelloAgent) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID, JournalID: journal,
		SupervisorVersion: build, SupportsRetireAtIdle: true, SupportsStopReplay: true, Agents: agents})
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, raw, err = conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err := json.Unmarshal(raw, &ack); err != nil || ack.Type != protocol.HelloAck {
		t.Fatalf("hello_ack=%+v %v", ack, err)
	}
	return conn
}

func (f *fixture) dialRetiringInstance(t *testing.T, journal, build string, agents []protocol.HelloAgent) *websocket.Conn {
	t.Helper()
	for attempt := 0; ; attempt++ {
		conn := f.dialRetiringInstanceOnce(t, journal, build, agents)
		if conn != nil {
			return conn
		}
		if attempt >= 5 {
			t.Fatal("no hello landed in the retiring set")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (f *fixture) dialRetiringInstanceOnce(t *testing.T, journal, build string, agents []protocol.HelloAgent) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID, JournalID: journal,
		SupervisorVersion: build, SupervisorEnvironment: "old", SupportsRetireAtIdle: true, SupportsStopReplay: true, Agents: agents})
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	for {
		_, raw, err = conn.Read(ctx)
		if err != nil {
			t.Fatalf("no hello_ack: %v", err)
		}
		if json.Unmarshal(raw, &ack) != nil {
			continue // a frame another goroutine streamed onto this connection is not the ack
		}
		if ack.Type == protocol.HelloAck {
			break
		}
	}
	// The server answers its ack with retire_at_idle at once; whatever else the connection carries (a frame another goroutine streamed)
	// is read past until it comes.
	skipCtx, skipCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer skipCancel()
	for {
		_, raw, err = conn.Read(skipCtx)
		if err != nil {
			break // no retire_at_idle within the window: fine, the test that needs it asserts on the refusal instead
		}
		var early protocol.Frame
		if json.Unmarshal(raw, &early) != nil {
			continue
		}
		if early.Type == protocol.RetireAtIdle {
			break
		}
	}
	f.srv.machineMu.Lock()
	retiring := f.srv.retiringMachines[f.machineID][journal]
	f.srv.machineMu.Unlock()
	if retiring == nil {
		f.srv.machineMu.Lock()
		var cur, ret string
		if c := f.srv.machines[f.machineID]; c != nil {
			cur = c.journalID + "/" + c.build
		}
		for j, c := range f.srv.retiringMachines[f.machineID] {
			ret += j + "/" + c.build + " "
		}
		f.srv.machineMu.Unlock()
		t.Logf("dial %s: not retiring; current=%q retiring=[%s]", journal, cur, ret)
		_ = conn.CloseNow()
		return nil // the machine slot had no matching current one this instant; the caller dials again
	}
	return conn
}

// A retiring instance of the machine speaks for an agent whose generation the newer instance has already taken: its process frame
// of the older generation is refused with stale_generation, the refusal still goes back to the supervisor, and nothing is reported
// to telemetry — the swap racing itself is the protocol working (AIMEBU_V2-Z), like the stale_rev ack of the control race.
func TestRetiringInstanceStaleGenerationIsRefusedWithoutAReport(t *testing.T) {
	transport := &captureTransport{}
	telemetry.ResetCaptured()
	if _, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", TracesSampleRate: 1}, transport); err != nil {
		t.Fatal(err)
	}
	defer telemetry.Current().Close()

	f := newFixture(t)
	f.srv.Version = "aimebu vtest"
	agent, turn, _, _ := f.execution(t)
	// The active instance holds the agent and says the matching build; the retiring one says it holds the same agent and an older build.
	// The active instance of the machine, on the matching build, holds the agent.
	f.dialInstanceBuild(t, "journal-new", "aimebu vtest", []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	// The retiring one says it holds the same agent at the same generation, on an older build.
	old := f.dialRetiringInstance(t, "journal-old", "an older build", []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	// The newer instance has taken the agent over: its generation moved past the one the retiring instance still speaks for.
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET generation=generation+1 WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Process, AgentID: agent.ID, TurnID: turn.ID, Generation: 0,
		Exit: &adapter.ExitInfo{ExitCode: 0}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := old.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, data, err := old.Read(ctx)
	if err != nil {
		t.Fatalf("no answer to the stale process frame: %v", err)
	}
	var answer protocol.Frame
	if err := json.Unmarshal(data, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Type != protocol.Error || answer.Code != "stale_generation" {
		t.Fatalf("stale process frame answer=%+v, want error frame stale_generation", answer)
	}
	// The refusal is the protocol working: nothing about it reaches Sentry.
	telemetry.Current().Flush(time.Second)
	transport.mu.Lock()
	defer transport.mu.Unlock()
	for _, event := range transport.events {
		if len(event.Exception) == 0 { // the server also sends transactions of its own
			continue
		}
		if strings.Contains(event.Exception[0].Value, "stale_generation") || strings.Contains(event.Exception[0].Value, "agent frame rejected") {
			t.Fatalf("the retiring instance's stale frame was reported: %q", event.Exception[0].Value)
		}
	}
}

// A stale generation that no retire explains is still a fault: the same frame from the active connection (whose bound generation the
// server has moved past without any retire) is refused and reported.
func TestStaleGenerationFromTheActiveConnectionIsStillReported(t *testing.T) {
	transport := &captureTransport{}
	telemetry.ResetCaptured()
	if _, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", TracesSampleRate: 1}, transport); err != nil {
		t.Fatal(err)
	}
	defer telemetry.Current().Close()

	f := newFixture(t)
	agent, turn, _, _ := f.execution(t)
	conn := f.dialMachine(t, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}})
	defer conn.Close(websocket.StatusNormalClosure, "")
	// The agent's generation has moved past what the connection was bound to, with no retire anywhere: a fault.
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET generation=generation+1 WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Process, AgentID: agent.ID, TurnID: turn.ID, Generation: 0,
		Exit: &adapter.ExitInfo{ExitCode: 0}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("no answer to the stale process frame: %v", err)
	}
	var answer protocol.Frame
	if err := json.Unmarshal(data, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Type != protocol.Error || answer.Code != "stale_generation" {
		t.Fatalf("stale process frame answer=%+v, want error frame stale_generation", answer)
	}
	telemetry.Current().Flush(time.Second)
	transport.mu.Lock()
	defer transport.mu.Unlock()
	reported := false
	for _, event := range transport.events {
		if len(event.Exception) == 0 { // the server also sends transactions of its own
			continue
		}
		if strings.Contains(event.Exception[0].Value, "stale_generation") {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("a stale generation from the active connection was not reported: %+v", transport.events)
	}
}

// The retiring flag is written by a newer instance's hello while the old connection's frames are being read: the stale frames and the
// hello of the newer instance run side by side, so the race detector sees the swap the classification is for. The old connection is
// reconnected until the newer hello wins (the second hello of the same journal replaces, not retires), then one stale frame is answered.
func TestRetiringSwapRacesAStaleFrameAndReportsNothing(t *testing.T) {
	transport := &captureTransport{}
	telemetry.ResetCaptured()
	if _, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", TracesSampleRate: 1}, transport); err != nil {
		t.Fatal(err)
	}
	defer telemetry.Current().Close()

	f := newFixture(t)
	f.srv.Version = "aimebu vtest"
	agent, turn, _, _ := f.execution(t)

	// The agent's generation has moved past what every connection's frames will say: every streamed frame is stale, so each one reaches
	// the retiring read in authorizeAgentFrame.
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET generation=generation+1 WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A matching-build connection is current before the loop starts, and every iteration re-dials one at the end, so the slot never
	// empties: that is what makes each old-build hello land in the retiring set at once.
	f.dialInstanceBuild(t, "journal-stay", "aimebu vtest", []protocol.HelloAgent{{AgentID: agent.ID, Generation: 1}})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var liveMu sync.Mutex
	var live []*websocket.Conn // the hellos' connections, open until their turn ends: the streamer writes to these
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// The old build says hello: its connection lands in the retiring set at once, and the stale frames stream to it while the
			// newer hello below flips its flag.
			oldConn := f.dialRetiringInstance(t, "journal-old-"+strconv.Itoa(i), "an older build", []protocol.HelloAgent{{AgentID: agent.ID, Generation: 1}})
			liveMu.Lock()
			live = append(live, oldConn)
			liveMu.Unlock()
			// The newer instance's hello marks the older connection retiring, and stays current for the next iteration's old hello.
			conn := f.dialInstanceBuild(t, "journal-new-"+strconv.Itoa(i), "aimebu vtest", []protocol.HelloAgent{{AgentID: agent.ID, Generation: 1}})
			liveMu.Lock()
			live = append(live, conn)
			liveMu.Unlock()
		}
	}()
	defer wg.Wait()
	defer close(stop)

	// Frames of the agent at its current generation stream from a connection of the machine the whole time: some land on the active
	// connection, some on one in the retiring set, while the hellos above flip the flag under the server's lock.
	raw, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Process, AgentID: agent.ID, TurnID: turn.ID, Generation: 0,
		Exit: &adapter.ExitInfo{ExitCode: 0}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		liveMu.Lock()
		connections := append([]*websocket.Conn(nil), live...)
		liveMu.Unlock()
		for _, c := range connections {
			_ = c.Write(ctx, websocket.MessageText, raw)
		}
		time.Sleep(2 * time.Millisecond)
	}
	liveMu.Lock()
	for _, c := range live {
		_ = c.CloseNow()
	}
	liveMu.Unlock()

	// The test's assertion is the race detector itself: the frames and the hellos share the retiring flag, and -race fails the run on
	// a read outside the lock. Whether a given frame was classified expected (it had landed on a connection already marked retiring)
	// or not (it had landed on the current one microseconds before its hello) is the swap's own race, both outcomes lawful.
	telemetry.Current().Flush(time.Second)
}

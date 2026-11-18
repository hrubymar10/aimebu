package server

import (
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
	"github.com/hrubymar10/aimebu/internal/telemetry"
)

// TestMachineAnswersStaleControlAckWithStaleRev reproduces the production race of #43362 (AIMEBU_V2-6/V2-7): a stop
// bumps the desired revision while the supervisor is still acking the control before it, so the ack of the older
// revision is refused. The answer is an error frame of its own code, `stale_rev`, and nothing is reported to
// telemetry — the newer control is already on its way, so the machine converges.
func TestMachineAnswersStaleControlAckWithStaleRev(t *testing.T) {
	transport := &captureTransport{}
	if _, err := telemetry.Init(telemetry.Config{DSN: "https://public@example.invalid/1", Environment: "test", TracesSampleRate: 1}, transport); err != nil {
		t.Fatal(err)
	}
	defer telemetry.Current().Close()

	f := newFixture(t)
	f.seed()
	header := http.Header{}
	header.Set("X-Machine-ID", f.machineID)
	header.Set("Authorization", "Bearer "+f.machineKey)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/machine", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hello, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: f.machineID, JournalID: "journal-test",
		SupportsStopReplay: true, Capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	status, card, raw := f.spawnRequest(t, "stale-rev", "worker")
	if status != 201 || card.ID == "" {
		t.Fatalf("spawn %d %+v", status, raw)
	}
	// Bind the agent the way hello does, so the connection accepts its frames.
	f.srv.machineMu.Lock()
	machine := f.srv.machines[f.machineID]
	f.srv.machineMu.Unlock()
	if machine == nil {
		t.Fatal("machine connection not registered")
	}
	machine.boundMu.Lock()
	machine.bound[card.ID] = card.Generation
	machine.boundMu.Unlock()

	control := func(desired string, rev int64) protocol.Frame {
		t.Helper()
		resp := f.request("POST", "/api/agents/"+card.ID+"/control", map[string]any{"desired": desired, "rev": rev}, true)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s control: %d", desired, resp.StatusCode)
		}
		// A spawn frame may be queued before the control; skip what is not the control.
		for {
			readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, body, err := conn.Read(readCtx)
			cancel()
			if err != nil {
				t.Fatalf("%s control not delivered: %v", desired, err)
			}
			var frame protocol.Frame
			if err := json.Unmarshal(body, &frame); err != nil {
				t.Fatal(err)
			}
			if frame.Type == protocol.Control && frame.DesiredState == desired {
				return frame
			}
		}
	}
	// A first change the supervisor is about to ack, then a newer desired revision (the room closed, the agent
	// stopped) that the supervisor has not seen when its ack of the older one arrives.
	pause := control("not_accepting", card.DesiredRev)
	control("stopped", pause.DesiredRev)

	ack, _ := json.Marshal(protocol.Frame{V: protocol.Version, Type: protocol.Ack, AgentID: card.ID,
		Generation: pause.Generation, Level: "control", Ref: fmt.Sprint(pause.DesiredRev)})
	writeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := conn.Write(writeCtx, websocket.MessageText, ack); err != nil {
		t.Fatal(err)
	}
	cancel()
	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, body, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("no answer to the stale ack: %v", err)
	}
	var answer protocol.Frame
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Type != protocol.Error || answer.Code != "stale_rev" {
		t.Fatalf("stale ack answer=%+v, want error frame stale_rev", answer)
	}
	// The refusal is the protocol working: nothing about it reaches Sentry.
	telemetry.Current().Flush(time.Second)
	transport.mu.Lock()
	defer transport.mu.Unlock()
	for _, event := range transport.events {
		if strings.Contains(event.Message, "stale_rev") || strings.Contains(event.Message, "ack failed") {
			t.Fatalf("stale ack was reported: %q", event.Message)
		}
	}
}

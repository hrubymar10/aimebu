package server

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// A previous-build supervisor accepts keys in hello_ack but cannot read the
// standalone frame. Its first connection must not receive a toolbox-less turn.
func TestOldKeyFrameSupervisorReconnectsBeforeHandoverDelivery(t *testing.T) {
	f := newFixture(t)
	f.srv.Version = "first"
	agent, _, _, delivery := f.execution(t)
	oldKey, err := f.srv.Auth.IssueAgentKey(t.Context(), agent.ID)
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
			t.Fatal("old warm process was not observed")
		}
		time.Sleep(time.Millisecond)
	}
	f.srv.Version = "second"
	current, ack := dialInstanceFrames(t, f, "second-journal", "second", "", nil, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}}, false)
	if len(ack.AgentKeys) != 0 {
		t.Fatalf("key granted before handover: %+v", ack.AgentKeys)
	}
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.RetireAtIdle })
	writeCommandFrame(t, old, protocol.Frame{V: protocol.Version, Type: protocol.WarmStateFrame, Warm: &protocol.WarmReport{Seq: 2}})
	deadline = time.Now().Add(3 * time.Second)
	for {
		blocked, err := f.srv.handoverBlocked(t.Context(), f.machineID, agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old warm process did not release")
		}
		time.Sleep(time.Millisecond)
	}
	if err := f.srv.Dispatch(t.Context(), delivery); err != ErrMachineUnavailable {
		t.Fatalf("delivery before key: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		_, raw, err := current.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
				t.Fatalf("expected clean retry close, got %v", err)
			}
			break
		}
		var frame protocol.Frame
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Type == protocol.AgentKeys || frame.Type == protocol.Deliver {
			t.Fatalf("frame sent before toolbox key: %+v", frame)
		}
	}
	if err := f.srv.Auth.VerifyAgentKey(t.Context(), agent.ID, oldKey); err != nil {
		t.Fatalf("old key rotated before reconnect: %v", err)
	}
	reconnected, ack := dialInstanceFrames(t, f, "second-journal", "second", "", nil, []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}}, false)
	if len(ack.AgentKeys) != 1 || ack.AgentKeys[0].AgentID != agent.ID || ack.AgentKeys[0].Key == "" {
		t.Fatalf("reconnect did not grant toolbox key: %+v", ack.AgentKeys)
	}
	if err := f.srv.Auth.VerifyAgentKey(t.Context(), agent.ID, ack.AgentKeys[0].Key); err != nil {
		t.Fatalf("granted toolbox key cannot authenticate: %v", err)
	}
	if err := f.srv.Dispatch(t.Context(), delivery); err != nil {
		t.Fatalf("delivery after key: %v", err)
	}
	readCommandFrame(t, reconnected, func(frame protocol.Frame) bool {
		if frame.Type == protocol.Deliver && len(ack.AgentKeys) == 0 {
			t.Fatal("turn delivered without a toolbox key")
		}
		return frame.Type == protocol.Deliver && frame.TurnID == delivery.TurnID
	})
	if err := f.srv.Auth.VerifyAgentKey(context.Background(), agent.ID, oldKey); err == nil {
		t.Fatal("old key still authenticates after reconnect")
	}
}

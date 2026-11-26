package supervisor

import (
	"context"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func TestDeliveryWithOldRevisionAfterStopIsRejected(t *testing.T) {
	h := newWarmHarness(t)
	h.spawn("agt_a")
	if h.c.handleControl(protocol.Frame{AgentID: "agt_a", Generation: 1, DesiredRev: 2, DesiredState: "stopped"}) {
		t.Fatal("idle agent's stop should finish at once")
	}
	// Keep a key available to exercise delivery admission even if stop has
	// already discarded its own key.
	if err := h.c.setAgentKey("agt_a", "aak_"+"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	conn, _ := frameCatcher(t)
	delivery := protocol.Delivery{AgentID: "agt_a", TurnID: "turn_old", Generation: 1, DesiredRev: 1,
		Mode: adapter.ModeNormal, Agent: adapter.AgentSpec{ID: "agt_a", Tool: "fake", Variant: "native"},
		Context: json.RawMessage(`{"control_changes":{"desired":"active","desired_rev":1}}`)}
	if err := h.c.handleDeliver(context.Background(), conn, delivery); err == nil || !strings.Contains(err.Error(), "delivery admission rejected") {
		t.Fatalf("old delivery admission: %v", err)
	}
	started, err := h.c.Journal.HasStart(context.Background(), delivery.TurnID)
	if err != nil || started || h.f.cold != 0 {
		t.Fatalf("old delivery started: journal=%t processes=%d %v", started, h.f.cold, err)
	}
	if desired := h.c.desired[delivery.AgentID]; desired.Desired != "stopped" || desired.DesiredRev != 2 {
		t.Fatalf("old delivery changed control state: %+v", desired)
	}
}

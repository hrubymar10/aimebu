package supervisor

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// A delivered turn carries the version the supervisor probed for the harness it runs on (the harness row's, or the launcher's for an agent on
// a launcher), so an adapter with a version table (Claude) passes only what that version supports.
func TestDeliveryCarriesTheHarnessVersion(t *testing.T) {
	for _, name := range []string{"harness", "launcher"} {
		t.Run(name, func(t *testing.T) {
			h := newWarmHarness(t)
			h.spawn("agt_a")
			checkout, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			agent := adapter.AgentSpec{ID: "agt_a", Tool: "claude", Variant: "native", Model: "haiku"}
			h.c.mu.Lock()
			for i := range h.c.Harnesses {
				if h.c.Harnesses[i].Tool == "claude" && h.c.Harnesses[i].Variant == "native" {
					h.c.Harnesses[i].Available, h.c.Harnesses[i].LauncherPath, h.c.Harnesses[i].Version = true, "/bin/claude", "2.1.330 (Claude Code)"
				}
			}
			if name == "launcher" {
				agent.LauncherID = "lch_a"
				h.c.Launchers = []protocol.LauncherRow{{LauncherDefinition: protocol.LauncherDefinition{ID: "lch_a", Tool: "claude", Name: "wrapper", Command: "/bin/wrapper", Enabled: true},
					ResolvedCommand: "/bin/wrapper", Version: "2.1.4 (Claude Code)", Available: true}}
			}
			h.c.mu.Unlock()
			want := map[string]string{"harness": "2.1.330 (Claude Code)", "launcher": "2.1.4 (Claude Code)"}[name]
			conn, _ := frameCatcher(t)
			delivery := protocol.Delivery{AgentID: "agt_a", TurnID: "turn_v", Generation: 1, DesiredRev: 1, Mode: adapter.ModeNormal, Agent: agent,
				CheckoutPath: checkout, Context: json.RawMessage(`{}`), Notes: []protocol.DeliveryNote{{NoteID: "note_v", RoomID: "room", MessageID: "msg", Body: "hi", From: "server"}}}
			if err := h.c.handleDeliver(context.Background(), conn, delivery); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the turn starting", func() bool {
				h.f.mu.Lock()
				defer h.f.mu.Unlock()
				return h.f.started+h.f.cold > 0
			})
			h.f.mu.Lock()
			got := h.f.lastSpec.HarnessVersion
			if h.f.cold > 0 {
				got = h.f.coldSpecs[0].HarnessVersion
			}
			h.f.mu.Unlock()
			if got != want {
				t.Fatalf("HarnessVersion = %q, want %q", got, want)
			}
		})
	}
}

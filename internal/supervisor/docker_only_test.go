package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// When the instance runs agents only in docker, a supervisor refuses an agent on a native launcher itself: a spawn is an error that says why and
// keeps no key, and a delivery is refused before anything starts. A Docker launcher and the test agent are not refused by it.
func TestSupervisorRefusesNativeLaunchersWhenDockerOnly(t *testing.T) {
	j, err := OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	c := NewClient("machine", "key", "http://127.0.0.1:9897", j, adapter.NewFake())
	c.OnError = func(error) {}
	key := "aak_" + strings.Repeat("d", 43)
	result := c.applySpawn(protocol.Frame{AgentID: "agt_native", AgentKey: key, CheckoutPath: t.TempDir(), DockerOnly: true, Agent: &adapter.AgentSpec{Tool: "claude", Variant: "native"}})
	if result.Result != "error" || !strings.Contains(result.Message, "Only run agents in docker") {
		t.Fatalf("native spawn: %+v", result)
	}
	if c.keyOf("agt_native") != "" {
		t.Fatal("a refused spawn kept the key")
	}
	if result := c.applySpawn(protocol.Frame{AgentID: "agt_off", AgentKey: key, CheckoutPath: t.TempDir(), Agent: &adapter.AgentSpec{Tool: "fake", Variant: "test"}}); result.Result != "ok" {
		t.Fatalf("a spawn with the setting off: %+v", result)
	}
	if result := c.applySpawn(protocol.Frame{AgentID: "agt_fake", AgentKey: key, CheckoutPath: t.TempDir(), DockerOnly: true, Agent: &adapter.AgentSpec{Tool: "fake", Variant: "test"}}); result.Result != "ok" {
		t.Fatalf("the test agent is not refused: %+v", result)
	}
	delivery := protocol.Delivery{AgentID: "agt_native", TurnID: "turn_1", Mode: adapter.ModeNormal, Agent: adapter.AgentSpec{ID: "agt_native", Tool: "codex", Variant: "native"}, DockerOnly: true}
	if err := c.handleDeliver(context.Background(), nil, delivery); !errors.Is(err, errDockerOnly) {
		t.Fatalf("a native delivery was not refused: %v", err)
	}
}

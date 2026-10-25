package supervisor

import (
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/protocol"
)

// Mistral Vibe is a launcher tool, but only a native one: there is no container variant of it yet.
func TestLauncherToolVibeIsNativeOnly(t *testing.T) {
	native := protocol.LauncherDefinition{ID: "mln_1", Name: "native", Tool: "vibe", Command: "vibe", Toolbox: true}
	if err := validateLauncher(native); err != nil {
		t.Fatalf("a native vibe launcher: %v", err)
	}
	docker := native
	docker.Containerized = true
	if err := validateLauncher(docker); err == nil || !strings.Contains(err.Error(), "no container variant") {
		t.Fatalf("a containerized vibe launcher must be refused: %v", err)
	}
	unknown := native
	unknown.Tool = "vibes"
	if err := validateLauncher(unknown); err == nil {
		t.Fatal("an unknown tool is still refused")
	}
	if command, ok := knownHarness("vibe", "native"); !ok || command != "vibe" {
		t.Fatalf("known harness: %q %v", command, ok)
	}
	if _, ok := knownHarness("vibe", "docker"); ok {
		t.Fatal("vibe has no docker harness")
	}
}

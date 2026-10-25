package supervisor

import (
	"testing"

	"github.com/hrubymar10/aimebu/internal/protocol"
)

// Mistral Vibe is a launcher tool, native or containerized.
func TestLauncherToolVibeIsNativeOrContainerized(t *testing.T) {
	native := protocol.LauncherDefinition{ID: "mln_1", Name: "native", Tool: "vibe", Command: "vibe", Toolbox: true}
	if err := validateLauncher(native); err != nil {
		t.Fatalf("a native vibe launcher: %v", err)
	}
	docker := native
	docker.Containerized, docker.Command = true, "vibe-docker"
	if err := validateLauncher(docker); err != nil {
		t.Fatalf("a containerized vibe launcher: %v", err)
	}
	unknown := native
	unknown.Tool = "vibes"
	if err := validateLauncher(unknown); err == nil {
		t.Fatal("an unknown tool is still refused")
	}
	if command, ok := knownHarness("vibe", "native"); !ok || command != "vibe" {
		t.Fatalf("known harness: %q %v", command, ok)
	}
	if command, ok := knownHarness("vibe", "docker"); !ok || command != "vibe-docker" {
		t.Fatalf("known harness: %q %v", command, ok)
	}
}

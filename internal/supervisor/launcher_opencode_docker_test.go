package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/protocol"
)

// A containerized OpenCode launcher is allowed now, and its checkout preflight asks whether the launcher can forward the environment: an
// older one would start the harness without its configuration, so the folder is not verified and the message says what to update.
func TestContainerizedOpenCodeLauncherNeedsOneThatForwardsTheEnvironment(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "project")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	def := protocol.LauncherDefinition{ID: "mln_oc_docker", Tool: "opencode", Name: "harness-docker", Command: "opencode-docker", Enabled: true, Containerized: true, Toolbox: true}
	if err := validateLauncher(def); err != nil {
		t.Fatalf("a containerized opencode launcher: %v", err)
	}
	command := filepath.Join(root, "opencode-docker")
	write := func(body string) {
		if err := os.WriteFile(command, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	def.Command = command
	write("echo 1.19.4")
	row, err := inspectLauncher(def, checkout)
	if err != nil || !row.Available {
		t.Fatalf("row=%+v err=%v", row, err)
	}
	if _, err := preflightLauncher(t.Context(), def, row, checkout); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("a launcher without the feature probe: %v", err)
	}
	write(`if [ "$HARNESS_DOCKER_PRINT_FEATURES" = 1 ]; then echo forward_env; exit 0; fi
echo 1.19.4`)
	visible, err := preflightLauncher(t.Context(), def, row, checkout)
	if err != nil || visible != checkout {
		t.Fatalf("a launcher with it: visible=%q err=%v", visible, err)
	}
	// A native launcher, and a containerized one of another tool, are not asked.
	native := def
	native.Containerized = false
	nativeRow, err := inspectLauncher(native, checkout)
	if err != nil {
		t.Fatal(err)
	}
	write("echo 1.19.4")
	if _, err := preflightLauncher(t.Context(), native, nativeRow, checkout); err != nil {
		t.Fatalf("a native launcher has no such need: %v", err)
	}
	if known, ok := knownHarness("opencode", "docker"); !ok || known != "opencode-docker" {
		t.Fatalf("known harness: %q %v", known, ok)
	}
}

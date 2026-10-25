package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/protocol"
)

// A Docker launcher whose tool depends on a feature is asked for its features when it is inspected, and the answer lands on the row for
// doctor and Settings; a launcher that predates the probe, or one that fails it, gets a problem and stays available (the turn's preflight
// still refuses it with its own message).
func TestInspectLauncherRecordsDockerFeatures(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "vibe-docker")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(command, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	def := protocol.LauncherDefinition{ID: "mln_v", Tool: "vibe", Name: "docker", Command: command, Enabled: true, Containerized: true, Toolbox: true}

	write(`if [ "$HARNESS_DOCKER_PRINT_FEATURES" = 1 ]; then printf 'forward_env\nother_thing\n'; exit 0; fi
echo 2.0`)
	row, err := inspectLauncher(def, "")
	if err != nil || !row.Available || row.FeatureProblem != "" || strings.Join(row.Features, ",") != "forward_env,other_thing" {
		t.Fatalf("a current launcher: %+v %v", row, err)
	}

	write("echo 2.0") // predates the probe: it answers with a version, which names no feature
	row, err = inspectLauncher(def, "")
	if err != nil || !row.Available || len(row.Features) != 0 || !strings.Contains(row.FeatureProblem, "launcher too old") || !strings.Contains(row.FeatureProblem, "vibe") {
		t.Fatalf("an old launcher: %+v %v", row, err)
	}

	write("echo boom >&2; exit 3")
	row, err = inspectLauncher(def, "")
	if err != nil || !row.Available || !strings.Contains(row.FeatureProblem, "feature probe failed") {
		t.Fatalf("a failing probe: %+v %v", row, err)
	}

	// Native launchers and Docker launchers of a tool that needs no feature are not probed at all.
	write("echo should-not-run; exit 9")
	native := def
	native.Containerized = false
	if row, err = inspectLauncher(native, ""); err != nil || row.FeatureProblem != "" || row.Features != nil {
		t.Fatalf("native: %+v %v", row, err)
	}
	claude := def
	claude.Tool = "claude"
	if row, err = inspectLauncher(claude, ""); err != nil || row.FeatureProblem != "" || row.Features != nil {
		t.Fatalf("claude-docker: %+v %v", row, err)
	}
}

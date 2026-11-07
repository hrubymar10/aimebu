package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/testenv"
)

// A Docker launcher whose tool depends on a feature is asked for its features when it is inspected in a checkout, and the answer lands on
// the row for doctor and Settings; a launcher that predates the probe, or one that fails it, gets a problem and stays available (the turn's preflight
// still refuses it with its own message).
func TestInspectLauncherRecordsDockerFeatures(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "vibe-docker")
	write := func(body string) {
		t.Helper()
		if err := testenv.WriteExecutable(command, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	def := protocol.LauncherDefinition{ID: "mln_v", Tool: "vibe", Name: "docker", Command: command, Enabled: true, Containerized: true, Toolbox: true}

	checkout := filepath.Join(root, "project")
	if err := os.Mkdir(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	write(`if [ "$HARNESS_DOCKER_PRINT_FEATURES" = 1 ]; then printf 'forward_env\nother_thing\n'; exit 0; fi
echo 2.0`)
	row, err := inspectLauncher(def, checkout)
	if err != nil || !row.Available || row.FeatureProblem != "" || strings.Join(row.Features, ",") != "forward_env,other_thing" {
		t.Fatalf("a current launcher: %+v %v", row, err)
	}

	write("echo 2.0") // predates the probe: it answers with a version, which names no feature
	row, err = inspectLauncher(def, checkout)
	if err != nil || !row.Available || len(row.Features) != 0 || !strings.Contains(row.FeatureProblem, "launcher too old") || !strings.Contains(row.FeatureProblem, "vibe") {
		t.Fatalf("an old launcher: %+v %v", row, err)
	}

	write("echo boom >&2; exit 3")
	row, err = inspectLauncher(def, checkout)
	if err != nil || !row.Available || !strings.Contains(row.FeatureProblem, "feature probe failed") {
		t.Fatalf("a failing probe: %+v %v", row, err)
	}

	// With no project folder the probe still runs, from an empty folder of ours: a current launcher answers before it touches Docker, and one
	// that predates the probe checks the mount first and says so, which is read as "too old", not as a failure.
	write(`if [ "$HARNESS_DOCKER_PRINT_FEATURES" = 1 ]; then printf 'forward_env\n'; exit 0; fi
echo 2.0`)
	if row, err = inspectLauncher(def, ""); err != nil || row.FeatureProblem != "" || strings.Join(row.Features, ",") != "forward_env" || !row.Available {
		t.Fatalf("no checkout, a current launcher: %+v %v", row, err)
	}
	write(`echo "Error: Directory '$PWD' is not mounted in the harness container." >&2; exit 1`)
	if row, err = inspectLauncher(def, ""); err != nil || !strings.Contains(row.FeatureProblem, "launcher too old") || !row.Available {
		t.Fatalf("no checkout, an old launcher: %+v %v", row, err)
	}
	write("echo should-not-run; exit 9")
	// Native launchers and Docker launchers of a tool that needs no feature are not probed for one they cannot have.
	native := def
	native.Containerized = false
	if row, err = inspectLauncher(native, checkout); err != nil || row.FeatureProblem != "" || row.Features != nil {
		t.Fatalf("native: %+v %v", row, err)
	}
	// A Docker tool that needs no feature (Pi) is not asked for one, and a launcher that cannot answer is no problem.
	pi := def
	pi.Tool = "pi"
	pi.Toolbox = false
	if row, err = inspectLauncher(pi, checkout); err != nil || row.FeatureProblem != "" || row.Features != nil || !row.Available {
		t.Fatalf("a pi-docker launcher that fails the probe: %+v %v", row, err)
	}
	write("echo 2.0") // predates the probe: nothing is known, and nothing is said
	if row, err = inspectLauncher(pi, checkout); err != nil || row.FeatureProblem != "" || len(row.Features) != 0 || !row.Available {
		t.Fatalf("an old pi-docker launcher: %+v %v", row, err)
	}
	// Claude's Docker turns hand the agent's key over by name, so an old launcher is too old for it, like one for OpenCode.
	claude := def
	claude.Tool = "claude"
	if row, err = inspectLauncher(claude, checkout); err != nil || !strings.Contains(row.FeatureProblem, "launcher too old") || !row.Available {
		t.Fatalf("an old claude-docker launcher: %+v %v", row, err)
	}
	write(`if [ "$HARNESS_DOCKER_PRINT_FEATURES" = 1 ]; then printf 'forward_env\nclaude_home_shared\n'; exit 0; fi
echo 2.0`)
	if row, err = inspectLauncher(claude, checkout); err != nil || row.FeatureProblem != "" || strings.Join(row.Features, ",") != "claude_home_shared,forward_env" {
		t.Fatalf("a claude-docker launcher that shares the Claude home: %+v %v", row, err)
	}
}

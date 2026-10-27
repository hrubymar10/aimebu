package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/protocol"
)

func versionLauncher(t *testing.T, body string) (protocol.LauncherDefinition, string) {
	t.Helper()
	root := t.TempDir()
	command := filepath.Join(root, "claude-docker")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return protocol.LauncherDefinition{ID: "mln_docker", Tool: "claude", Name: "harness-docker", Command: command, Enabled: true, Containerized: true, Toolbox: true}, root
}

// "Version unknown" used to be all a container launcher could say. The row now says why.
func TestContainerLauncherRowSaysWhyItsVersionIsUnknown(t *testing.T) {
	// Asked from a folder its container does not mount, a launcher says so on stderr and fails.
	def, root := versionLauncher(t, `echo "Error: Directory '$PWD' is not mounted in the harness container." >&2; exit 1`)
	row, err := inspectLauncher(def, root)
	if err != nil || row.Version != "" || !strings.Contains(row.VersionProblem, "is not mounted in the harness container") || !row.Available {
		t.Fatalf("a launcher that refuses the folder: %+v %v", row, err)
	}
	// With no folder to ask from, nothing is run, and the row says what is missing.
	row, err = inspectLauncher(def, "")
	if err != nil || row.Version != "" || !strings.Contains(row.VersionProblem, "no project folder") {
		t.Fatalf("no folder: %+v %v", row, err)
	}
	// A launcher that answers has no problem to report.
	ok, root := versionLauncher(t, `echo "2.1.310 (Claude Code)"`)
	row, err = inspectLauncher(ok, root)
	if err != nil || row.Version != "2.1.310 (Claude Code)" || row.VersionProblem != "" {
		t.Fatalf("an answering launcher: %+v %v", row, err)
	}
	// A native launcher is not asked from a folder at all: its probe works without one, and its failure stays "unavailable".
	native, _ := versionLauncher(t, `echo "native 1.0"`)
	native.Containerized = false
	if row, err := inspectLauncher(native, ""); err != nil || row.Version != "native 1.0" || row.VersionProblem != "" {
		t.Fatalf("native: %+v %v", row, err)
	}
}

// A launcher that is slow to answer is told apart from one that refuses, so it can be asked again with more time.
func TestLauncherVersionProbeTellsATimeoutFromARefusal(t *testing.T) {
	def, root := versionLauncher(t, `sleep 5`)
	started := time.Now()
	if version, problem := probeLauncherVersion(def, def.Command, root, 100*time.Millisecond); version != "" || problem != versionTimedOut || time.Since(started) > 3*time.Second {
		t.Fatalf("slow launcher: version=%q problem=%q after %s", version, problem, time.Since(started))
	}
	refusing, root := versionLauncher(t, `echo nope >&2; exit 3`)
	if _, problem := probeLauncherVersion(refusing, refusing.Command, root, 2*time.Second); problem != "nope" {
		t.Fatalf("refusal: %q", problem)
	}
	silent, root := versionLauncher(t, `exit 4`)
	if _, problem := probeLauncherVersion(silent, silent.Command, root, 2*time.Second); !strings.Contains(problem, "exit status 4") {
		t.Fatalf("a launcher that fails without a word: %q", problem)
	}
}

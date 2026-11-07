package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/testenv"
)

func TestLauncherResolvesCommandAndVisibleCheckout(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "project", "task")
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(root, "c-l-a-u-d-e")
	if err := testenv.WriteExecutable(command, []byte("#!/bin/sh\nif [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then echo forward_env; exit 0; fi\necho claude-code 2.0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	def := protocol.LauncherDefinition{ID: "mln_custom", Tool: "claude", Name: "custom", Command: command,
		Args: []string{"--quiet"}, Enabled: true, Containerized: true, Toolbox: true,
		VisibleRoots: []string{filepath.Join(root, "project")},
		PathMaps:     []protocol.LauncherPathMap{{Host: filepath.Join(root, "project"), Visible: "/workspace"}}}
	row, err := inspectLauncher(def, checkout)
	if err != nil || !row.Available || row.Version != "claude-code 2.0" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
	visible, err := preflightLauncher(context.Background(), def, row, checkout)
	if err != nil || visible != "/workspace/task" {
		t.Fatalf("visible=%q err=%v", visible, err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := launcherCheckout(def, outside); err == nil {
		t.Fatal("outside checkout accepted")
	}
}

func TestCancelledLauncherProbeDoesNotWaitForChildOutputPipe(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "launcher-with-child")
	if err := testenv.WriteExecutable(command, []byte("#!/bin/sh\nsleep 5 &\necho ready > .probe-started\nwait\n"), 0700); err != nil {
		t.Fatal(err)
	}
	def := protocol.LauncherDefinition{ID: "slow", Name: "slow", Tool: "codex", Command: command, Enabled: true}
	row := protocol.LauncherRow{LauncherDefinition: def, ResolvedCommand: command, Available: true}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := preflightLauncher(ctx, def, row, root); done <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(root, ".probe-started")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(root, ".probe-started")); err != nil {
		t.Fatal("launcher probe did not start")
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil || time.Since(start) > 3*time.Second {
			t.Fatalf("cancelled probe waited for child pipe: %v, %v", err, time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled probe waited for child pipe")
	}
}

func TestSuccessfulLauncherProbeAcceptsInheritedOutputPipe(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "launcher-with-child")
	if err := testenv.WriteExecutable(command, []byte("#!/bin/sh\nsleep 2 &\necho v1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	def := protocol.LauncherDefinition{ID: "success", Name: "success", Tool: "codex", Command: command, Enabled: true}
	row := protocol.LauncherRow{LauncherDefinition: def, ResolvedCommand: command, Available: true}
	visible, err := preflightLauncher(t.Context(), def, row, root)
	if err != nil || visible != root {
		t.Fatalf("successful version command with inherited pipe: visible=%q err=%v", visible, err)
	}
}

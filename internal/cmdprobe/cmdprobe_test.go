package cmdprobe

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/testenv"
)

func script(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "tool")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// A launcher that exits 0 and leaves a helper holding the pipe is a success: its version is what it printed. Before, the read waited for
// the helper; with only a WaitDelay it would be an error ("WaitDelay expired before I/O complete") and a working launcher would be refused.
func TestASuccessfulCommandWhoseHelperHoldsThePipeIsASuccess(t *testing.T) {
	path := script(t, "echo 'tool 1.0'\nsleep 4 &\nexit 0\n")
	for name, run := range map[string]func(*exec.Cmd) ([]byte, error){"Output": Output, "CombinedOutput": CombinedOutput} {
		t.Run(name, func(t *testing.T) {
			started := time.Now()
			out, err := run(exec.CommandContext(t.Context(), path, "--version"))
			if err != nil || strings.TrimSpace(string(out)) != "tool 1.0" {
				t.Fatalf("out=%q err=%v, want the version and no error", out, err)
			}
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Fatalf("took %v: it waited for the helper", elapsed)
			}
		})
	}
}

func TestAFailedCommandStaysAFailureWhateverItsHelpersDo(t *testing.T) {
	path := script(t, "echo oops >&2\nsleep 4 &\nexit 3\n")
	_, err := CombinedOutput(exec.CommandContext(t.Context(), path))
	if err == nil {
		t.Fatal("a command that exited 3 was a success")
	}
}

// The deadline kills the launcher only. The helper it left behind must not keep the probe waiting past the deadline and the grace.
func TestAKilledCommandIsAnErrorAndDoesNotWaitForItsHelpers(t *testing.T) {
	path := script(t, "sleep 20 &\nsleep 20\n")
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := Output(exec.CommandContext(ctx, path))
	if err == nil {
		t.Fatal("a command killed by its deadline was a success")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("took %v: it waited for the launcher's helpers", elapsed)
	}
}

func TestAPlainSuccessIsUntouched(t *testing.T) {
	path := script(t, "echo 'tool 2.0'\n")
	out, err := Output(exec.CommandContext(t.Context(), path))
	if err != nil || strings.TrimSpace(string(out)) != "tool 2.0" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/usages"
)

func TestPrintUsagesPlainMarksStaleSnapshots(t *testing.T) {
	out := captureStdout(t, func() {
		printUsagesPlain(usages.Response{
			Snapshots: map[string]usages.Snapshot{
				"codex": {
					Provider: usages.ProviderCodex,
					Status:   usages.StatusStaleCache,
					Plan:     "Team Plus",
					Stale:    true,
				},
			},
		})
	})

	if !strings.Contains(out, "stale_cache (stale)") {
		t.Fatalf("plain output did not mark stale snapshot:\n%s", out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("plain output lines = %d, want 2:\n%s", len(lines), out)
	}
	if len(lines[0]) != len(lines[1]) {
		t.Fatalf("plain output is not aligned:\nheader %d: %q\nrow    %d: %q", len(lines[0]), lines[0], len(lines[1]), lines[1])
	}
}

func TestPrintUsagesPlainKeepsLongRowsAligned(t *testing.T) {
	out := captureStdout(t, func() {
		printUsagesPlain(usages.Response{
			Snapshots: map[string]usages.Snapshot{
				"github-copilot": {
					Provider: usages.ProviderGitHubCopilot,
					Status:   usages.StatusOK,
					Plan:     "GitHub Copilot Business Premium",
					Windows: []usages.Window{
						{Key: "premium_interactions", PercentUsed: 18},
						{Key: "chat", PercentUsed: 9},
					},
				},
				"ollama-cloud": {
					Provider: usages.ProviderOllamaCloud,
					Status:   usages.StatusStaleCache,
					Plan:     "Ollama Cloud Max Weekly",
					Stale:    true,
					Windows: []usages.Window{
						{Key: "session", PercentUsed: 3},
						{Key: "weekly", PercentUsed: 25},
					},
				},
			},
		})
	})

	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("plain output lines = %d, want 3:\n%s", len(lines), out)
	}
	for i := 1; i < len(lines); i++ {
		if len(lines[i]) != len(lines[0]) {
			t.Fatalf("line %d width = %d, want %d:\n%s", i, len(lines[i]), len(lines[0]), out)
		}
	}
	if !strings.Contains(out, "premium_interactions=18%, chat=9%") {
		t.Fatalf("long realistic windows were unexpectedly truncated:\n%s", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = old
	}()

	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close stdout pipe writer: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close stdout pipe reader: %v", err)
	}
	return string(data)
}

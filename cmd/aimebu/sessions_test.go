package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/types"
)

func TestMergeSessionRowsLabelsSources(t *testing.T) {
	seenLocal := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	seenServer := seenLocal.Add(time.Minute)
	rows := mergeSessionRows([]agentSession{
		{
			CWD:       "/local",
			Harness:   "codex",
			SessionID: "thread-local",
			Name:      "alice@proj",
			Model:     "gpt5",
			Command:   []string{"codex"},
			LastUsed:  seenLocal,
		},
		{
			CWD:       "/only-local",
			Harness:   "claude-code",
			SessionID: "claude-local",
			Name:      "bravo@proj",
			Command:   []string{"claude"},
			LastUsed:  seenLocal,
		},
	}, []types.AgentSession{
		{
			FullID:           "alice@proj",
			Origin:           "wrapper",
			Harness:          "codex",
			Project:          "proj",
			CWD:              "/server",
			HarnessSessionID: "thread-server",
			ResumeCommand:    "aimebu agent --resume-id thread-server -- codex",
			LastSeen:         seenServer,
		},
		{
			FullID:           "charlie@proj",
			Origin:           "mcp",
			Harness:          "pi",
			Project:          "proj",
			HarnessSessionID: "pi-server",
			LastSeen:         seenServer,
		},
	})
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3: %#v", len(rows), rows)
	}
	byID := map[string]sessionListRow{}
	for _, row := range rows {
		byID[row.FullID] = row
	}
	if got := byID["alice@proj"]; got.Source != "local+server" || got.SessionID != "thread-server" {
		t.Fatalf("merged alice = %#v, want local+server thread-server", got)
	}
	if got := byID["bravo@proj"]; got.Source != "local" || got.SessionID != "claude-local" {
		t.Fatalf("local bravo = %#v, want local claude-local", got)
	}
	if got := byID["charlie@proj"]; got.Source != "server" || got.Origin != "mcp" {
		t.Fatalf("server charlie = %#v, want server mcp", got)
	}

	var out bytes.Buffer
	printSessionRows(&out, rows)
	text := out.String()
	for _, want := range []string{"AGENT", "alice@proj", "local+server", "resume", "aimebu agent --resume-id thread-server", "charlie@proj", "pi-server"} {
		if !strings.Contains(text, want) {
			t.Fatalf("printed rows missing %q:\n%s", want, text)
		}
	}
}

package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/client"
)

func TestDetectHarness(t *testing.T) {
	// Clear all env vars detectHarness may consult, then re-set per case.
	clear := func(t *testing.T) {
		t.Helper()
		for _, k := range []string{
			"AIMEBU_HARNESS",
			"CLAUDECODE",
			"CLAUDE_CODE_ENTRYPOINT",
			"CURSOR_TRACE_ID",
			"CURSOR_SESSION_ID",
			"AIDER_VERSION",
		} {
			t.Setenv(k, "")
		}
	}

	tests := []struct {
		name string
		// env to set after clear; key→value. Empty map means "no env vars".
		env  map[string]string
		want string
	}{
		// Tier 2 — owned AIMEBU_HARNESS env var (post-AI fallback).
		{name: "aimebu_harness codex", env: map[string]string{"AIMEBU_HARNESS": "codex"}, want: "codex"},
		{name: "aimebu_harness cline", env: map[string]string{"AIMEBU_HARNESS": "cline"}, want: "cline"},
		{name: "aimebu_harness pi", env: map[string]string{"AIMEBU_HARNESS": "pi"}, want: "pi"},
		// AIMEBU_HARNESS wins over upstream env sniff.
		{name: "aimebu_harness wins over claudecode", env: map[string]string{"AIMEBU_HARNESS": "codex", "CLAUDECODE": "1"}, want: "codex"},

		// Tier 3 — upstream env sniff for harnesses that actually propagate.
		{name: "claudecode", env: map[string]string{"CLAUDECODE": "1"}, want: "claude-code"},
		{name: "claude_code_entrypoint", env: map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli"}, want: "claude-code"},
		{name: "cursor trace id", env: map[string]string{"CURSOR_TRACE_ID": "abc"}, want: "cursor"},
		{name: "cursor session id", env: map[string]string{"CURSOR_SESSION_ID": "abc"}, want: "cursor"},
		{name: "aider", env: map[string]string{"AIDER_VERSION": "0.1"}, want: "aider"},

		// Tier 4 — nothing set.
		{name: "unknown", env: map[string]string{}, want: "unknown"},

		// Codex env vars are deliberately NOT in tier 3 anymore — codex doesn't
		// propagate them to MCP children, so detection by env was misleading.
		// Setting them alone should yield "unknown".
		{name: "codex session_id alone falls through to unknown", env: map[string]string{"CODEX_SESSION_ID": "abc"}, want: "unknown"},
		{name: "codex thread_id alone falls through to unknown", env: map[string]string{"CODEX_THREAD_ID": "abc"}, want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clear(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if got := detectHarness(); got != tt.want {
				t.Fatalf("detectHarness() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMCP_InitializeReturnsOverriddenEtiquette proves that handle("initialize")
// reads the bus_etiquette prompt from the server rather than the compiled
// constant. This is the end-to-end wiring test: store override → fetchPrompts
// → promptVal → instructions field in the initialize response.
func TestMCP_InitializeReturnsOverriddenEtiquette(t *testing.T) {
	const overrideBody = "custom etiquette for testing"

	// Serve a fake /settings/prompts that returns an override for bus_etiquette.
	overrideEntries := []map[string]any{
		{"key": "bus_etiquette", "body": overrideBody, "overridden": true},
	}
	overrideJSON, _ := json.Marshal(overrideEntries)

	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/settings/prompts" {
			w.Header().Set("Content-Type", "application/json")
			w.Write(overrideJSON)
			return
		}
		http.NotFound(w, r)
	}))
	defer fakeSrv.Close()

	c := &client.Client{BaseURL: fakeSrv.URL}
	resp := handle(c, request{
		JSONRPC: "2.0",
		Method:  "initialize",
		ID:      json.RawMessage(`1`),
	})

	if resp == nil {
		t.Fatal("handle returned nil for initialize")
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", resp.Result)
	}
	instructions, ok := result["instructions"].(string)
	if !ok {
		t.Fatalf("instructions is not a string: %T", result["instructions"])
	}
	if instructions != overrideBody {
		t.Fatalf("instructions = %q, want override %q", instructions, overrideBody)
	}
}

package harnesslog

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Fixtures are fully synthetic — sanitized shapes, no real agent IDs or content.
const piFixture = `{"event":"wrapper_start","harness":"pi","spawn_tag":"t1"}
{"event":"harness_stdout_raw","line":"{\"type\":\"message_update\",\"role\":\"assistant\",\"content\":\"ok\"}","line_index":1,"truncated":false}
{"event":"harness_stdout_raw","line":"{\"type\":\"tool_execution_start\",\"tool\":\"bash\"}","line_index":2,"truncated":false}
{"event":"harness_stdout_raw","line":"{\"type\":\"agent_start\",\"agent_id\":\"agent@example\"}","line_index":3,"truncated":false}
{"event":"harness_stdout_raw","line":"not-json-tui-noise","line_index":4,"truncated":false}
`

const codexFixture = `{"event":"wrapper_start","harness":"codex","spawn_tag":"t2"}
{"event":"harness_stdout_raw","line":"{\"type\":\"item.started\",\"item_id\":\"item_1\"}","line_index":1,"truncated":false}
{"event":"harness_stdout_raw","line":"{\"type\":\"turn.failed\",\"error\":{\"message\":\"credits\"}}","line_index":2,"truncated":false}
`

const claudeCodeFixture = `{"event":"wrapper_start","harness":"claude-code","spawn_tag":"t3"}
{"event":"harness_stdout_raw","line":"{\"type\":\"assistant\",\"message\":{}}","line_index":1,"truncated":false}
{"event":"harness_stdout_raw","line":"{\"type\":\"rate_limit_event\",\"status\":\"rejected\"}","line_index":2,"truncated":false}
{"event":"harness_stdout_raw","line":"{\"type\":\"result\",\"subtype\":\"success\"}","line_index":3,"truncated":false}
`

func TestWalk(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "pi-agent.log"), piFixture)
	mustWrite(t, filepath.Join(dir, "codex-agent.log"), codexFixture)
	mustWrite(t, filepath.Join(dir, "claude-agent.log"), claudeCodeFixture)
	// A .stderr.log file must be ignored.
	mustWrite(t, filepath.Join(dir, "pi-agent.stderr.log"), "aimebu agent: noise\n")

	rep, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rep.ByCount["parsed"], 8; got != want {
		t.Errorf("parsed=%d want %d", got, want)
	}
	if got, want := rep.ByCount["unknown"], 0; got != want {
		t.Errorf("unknown=%d want %d", got, want)
	}
	if got, want := rep.ByCount["nonjson"], 1; got != want {
		t.Errorf("nonjson=%d want %d", got, want)
	}
	wantPi := map[string]int{"message_update": 1, "tool_execution_start": 1, "agent_start": 1}
	if got := rep.Histogram["pi"]; !reflect.DeepEqual(got, wantPi) {
		t.Errorf("pi histogram=%v want %v", got, wantPi)
	}
	wantCodex := map[string]int{"item.started": 1, "turn.failed": 1}
	if got := rep.Histogram["codex"]; !reflect.DeepEqual(got, wantCodex) {
		t.Errorf("codex histogram=%v want %v", got, wantCodex)
	}
	wantClaude := map[string]int{"assistant": 1, "rate_limit_event": 1, "result": 1}
	if got := rep.Histogram["claude-code"]; !reflect.DeepEqual(got, wantClaude) {
		t.Errorf("claude-code histogram=%v want %v", got, wantClaude)
	}
}

func TestClassifyNonJSON(t *testing.T) {
	_, res := ParsePi("   ")
	if res != NonJSON {
		t.Errorf("blank line res=%v want NonJSON", res)
	}
	_, res = ParsePi("\x1b[?25hTUI")
	if res != NonJSON {
		t.Errorf("tui res=%v want NonJSON", res)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

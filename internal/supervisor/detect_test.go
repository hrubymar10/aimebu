package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/protocol"
)

func TestParsePiModels(t *testing.T) {
	t.Parallel()
	out := "provider        model              context  max-out  thinking  images\n" +
		"github-copilot  claude-haiku-5.5   1M       128K     yes       yes   \n" +
		"ollama-cloud    gemma4:31b         256K     32K      no        no\n\n"
	got, err := parsePiModels([]byte(out))
	if err != nil || len(got) != 2 {
		t.Fatalf("models: %+v %v", got, err)
	}
	if got[0].Slug != "github-copilot/claude-haiku-5.5" || got[0].Label != "claude-haiku-5.5" || got[0].Note != "context 1M, max output 128K, thinking yes, images yes" {
		t.Errorf("first: %+v", got[0])
	}
	if got[1].Slug != "ollama-cloud/gemma4:31b" {
		t.Errorf("second: %+v", got[1])
	}
	if _, err := parsePiModels([]byte("provider model context\n")); err == nil {
		t.Error("a header only is not a model list")
	}
}

func TestParseOpencodeModels(t *testing.T) {
	t.Parallel()
	got, err := parseOpencodeModels([]byte("opencode/big-pickle\nnot a model\n\nollama-cloud/gemma4:31b\nnoslash\n"))
	if err != nil || len(got) != 2 || got[0].Slug != "opencode/big-pickle" || got[0].Label != "big-pickle" || got[1].Slug != "ollama-cloud/gemma4:31b" {
		t.Fatalf("models: %+v %v", got, err)
	}
	if _, err := parseOpencodeModels([]byte("")); err == nil {
		t.Error("nothing listed is an error")
	}
}

func TestParseCodexModelsKeepsOnlyListedOnes(t *testing.T) {
	t.Parallel()
	out := `{"models":[{"slug":"gpt-6-astra","display_name":"GPT-6-Astra","description":"Frontier intelligence.","visibility":"list"},
		{"slug":"gpt-hidden","display_name":"Hidden","description":"x","visibility":"hide"},
		{"slug":"gpt-6-luna","display_name":"","description":"` + strings.Repeat("d", 400) + `","visibility":"list"}]}`
	got, err := parseCodexModels([]byte(out))
	if err != nil || len(got) != 2 {
		t.Fatalf("models: %+v %v", got, err)
	}
	if got[0].Slug != "gpt-6-astra" || got[0].Label != "GPT-6-Astra" || got[0].Note != "Frontier intelligence." {
		t.Errorf("first: %+v", got[0])
	}
	if got[1].Label != "gpt-6-luna" || len([]rune(got[1].Note)) != 280 {
		t.Errorf("second: label %q note length %d", got[1].Label, len([]rune(got[1].Note)))
	}
	if _, err := parseCodexModels([]byte("not json")); err == nil {
		t.Error("invalid JSON is an error")
	}
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A harness found on PATH is reported once with its version, a symlink to it is not a second one, and a Docker wrapper has no version.
func TestDetectHarnessesFindsWhatIsInstalled(t *testing.T) {
	dir, second := t.TempDir(), t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+second)
	claude := writeScript(t, dir, "claude", `echo "2.9.1 (Claude Code)"`)
	otherClaude := writeScript(t, second, "claude", `echo "2.8.0 (Claude Code)"`) // a second, different install further down PATH
	writeScript(t, dir, "codex-docker", `echo ignored`)
	if err := os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(claude, filepath.Join(home, ".local", "bin", "claude")); err != nil {
		t.Fatal(err)
	}
	var mine []protocol.HarnessCandidate
	for _, candidate := range detectHarnesses(t.Context()) {
		if strings.HasPrefix(candidate.Command, dir) || strings.HasPrefix(candidate.Command, second) || strings.HasPrefix(candidate.Command, home) {
			mine = append(mine, candidate)
		}
	}
	if len(mine) != 3 {
		t.Fatalf("candidates under the test directories: %+v", mine)
	}
	if mine[0].Family != "claude" || mine[0].Containerized || mine[0].Command != claude || mine[0].Version != "2.9.1 (Claude Code)" {
		t.Errorf("claude: %+v", mine[0])
	}
	if mine[1].Family != "claude" || mine[1].Command != otherClaude || mine[1].Version != "2.8.0 (Claude Code)" {
		t.Errorf("the second claude on PATH is found too, not only the first: %+v", mine[1])
	}
	if mine[2].Family != "codex" || !mine[2].Containerized || mine[2].Version != "" {
		t.Errorf("codex-docker: %+v", mine[2])
	}
}

func TestDetectModelsRunsTheHarnessAndRefusesWhatItCannotDo(t *testing.T) {
	dir := t.TempDir()
	opencode := writeScript(t, dir, "opencode", `[ "$1" = models ] && printf 'opencode/a\nopencode/b\n'`)
	got, err := detectModels(t.Context(), "opencode", opencode)
	if err != nil || len(got) != 2 || got[1].Slug != "opencode/b" {
		t.Fatalf("models: %+v %v", got, err)
	}
	failing := writeScript(t, dir, "pi", `exit 3`)
	if _, err := detectModels(t.Context(), "pi", failing); err == nil {
		t.Error("a failing command is an error")
	}
	if _, err := detectModels(t.Context(), "claude", opencode); err == nil {
		t.Error("claude has no listing command")
	}
	if _, err := detectModels(t.Context(), "pi", "relative/pi"); err == nil {
		t.Error("a relative command is refused")
	}
}

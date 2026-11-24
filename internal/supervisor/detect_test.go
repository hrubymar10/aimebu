package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
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
	for _, out := range []string{
		"No models available. Use /login to authenticate.\n",
		"provider model context max-out thinking images\nNo models available. Use /login to authenticate.\n",
		"models - No/models - context available., max output Use, thinking /login, images to\n",
	} {
		if got, err := parsePiModels([]byte(out)); len(got) != 0 || err == nil || !strings.Contains(err.Error(), "pi reports no models — is it logged in? Run /login in it") {
			t.Errorf("Pi login/help output %q: %+v %v", out, got, err)
		}
	}
	if got, err := parsePiModels([]byte("Use /login to authenticate.\ngithub-copilot claude-haiku-5.5 1M 128K yes yes\n")); err != nil || len(got) != 1 {
		t.Errorf("valid Pi row after prose: %+v %v", got, err)
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
	if got, err := parseOpencodeModels([]byte("No models available. Use /login to authenticate.\nUse/login\n")); len(got) != 0 || err == nil || !strings.Contains(err.Error(), "opencode reports no models") {
		t.Errorf("OpenCode login/help output: %+v %v", got, err)
	}
	if got, err := parseOpencodeModels([]byte("opencode/big-pickle\nNo models available for another account\n")); err != nil || len(got) != 1 || got[0].Slug != "opencode/big-pickle" {
		t.Errorf("a valid listing survives unrelated status prose: %+v %v", got, err)
	}
}

func TestReportsNoModelsKeepsDiagnosticLineBoundaries(t *testing.T) {
	for _, output := range []string{
		"Error: not logged in\n",
		"first diagnostic\nPlease run /login\nlast diagnostic\n",
		"\x1b[31mWarning: no models found\x1b[0m\n",
	} {
		if !reportsNoModels(output) {
			t.Errorf("missed login diagnostic %q", output)
		}
	}
	for _, output := range []string{"opencode/big-pickle\n", "Use/login\n", "first diagnostic not logged in\n"} {
		if reportsNoModels(output) {
			t.Errorf("mistook model or unrelated prose for login diagnostic %q", output)
		}
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
	if got, err := parseCodexModels([]byte(`{"models":[{"slug":"bad slug","visibility":"list"}]}`)); len(got) != 0 || err == nil || !strings.Contains(err.Error(), "codex reports no models") {
		t.Errorf("invalid Codex slug: %+v %v", got, err)
	}
	if got, err := parseCodexModels([]byte("No models available. Use /login to authenticate.\n")); len(got) != 0 || err == nil || !strings.Contains(err.Error(), "codex reports no models") {
		t.Errorf("Codex login/help output: %+v %v", got, err)
	}
	for _, out := range []string{"Error: not logged in\n", "Please run /login\n"} {
		if got, err := parseCodexModels([]byte(out)); len(got) != 0 || err == nil || !strings.Contains(err.Error(), "codex reports no models") {
			t.Errorf("Codex prefixed login output %q: %+v %v", out, got, err)
		}
	}
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
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

func TestRestrictedPathDoesNotSearchMachineInstallDirs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("AIMEBU_TEST_RESTRICT_PATH", "1")
	got := searchDirs(t.Context())
	if len(got) != 1 || got[0] != dir {
		t.Fatalf("restricted search dirs = %v, want only %s", got, dir)
	}
}

func TestDetectModelsRunsTheHarnessAndRefusesWhatItCannotDo(t *testing.T) {
	dir := t.TempDir()
	opencode := writeScript(t, dir, "opencode", `[ "$1" = models ] && printf 'opencode/a\nopencode/b\n'`)
	got, err := detectModels(t.Context(), "opencode", opencode, nil, "")
	if err != nil || len(got) != 2 || got[1].Slug != "opencode/b" {
		t.Fatalf("models: %+v %v", got, err)
	}
	failing := writeScript(t, dir, "pi", `exit 3`)
	if _, err := detectModels(t.Context(), "pi", failing, nil, ""); err == nil {
		t.Error("a failing command is an error")
	}
	login := writeScript(t, dir, "pi-login", `printf 'No models available. Use /login to authenticate.\n'`)
	if _, err := detectModels(t.Context(), "pi", login, nil, ""); err == nil || !strings.Contains(err.Error(), "pi reports no models — is it logged in? Run /login in it") {
		t.Errorf("Pi login prompt: %v", err)
	}
	for name, output := range map[string]string{
		"prefixed": "first diagnostic\\nError: not logged in\\nlast diagnostic\\n",
		"request":  "Please run /login\\n",
	} {
		command := writeScript(t, dir, "pi-"+name, "printf '"+output+"' >&2; exit 3")
		if _, err := detectModels(t.Context(), "pi", command, nil, ""); err == nil || !strings.Contains(err.Error(), "pi reports no models — is it logged in? Run /login in it") {
			t.Errorf("Pi %s stderr login prompt: %v", name, err)
		}
	}
	if _, err := detectModels(t.Context(), "claude", opencode, nil, ""); err == nil {
		t.Error("claude has no listing command")
	}
	if _, err := detectModels(t.Context(), "pi", "relative/pi", nil, ""); err == nil {
		t.Error("a relative command is refused")
	}
}

func TestDetectModelsAcceptsSuccessfulLauncherWithOpenPipe(t *testing.T) {
	harness := writeScript(t, t.TempDir(), "opencode", `sleep 30 &
printf 'opencode/a\n'`)
	start := time.Now()
	models, err := detectModels(t.Context(), "opencode", harness, nil, "")
	if err != nil || len(models) != 1 || models[0].Slug != "opencode/a" {
		t.Fatalf("models = %+v, err = %v", models, err)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("model listing waited for the child: %v", took)
	}
}

// Model detection runs in a fresh private subfolder of the machine's config directory by default and in a folder the person chose, with the
// launcher's own arguments before the listing ones; when the command refuses, what it said on stderr reaches the person instead of a bare
// exit status. The script stands in for any harness that only answers from some folders.
func TestDetectDefaultDirPrunesOnlyStaleRunFolders(t *testing.T) {
	config := t.TempDir()
	base := filepath.Join(config, "detect")
	if err := os.Mkdir(base, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * detectStaleRunAge)
	for _, name := range []string{"run-old", "run-recent", "run-chosen", "other-old"} {
		path := filepath.Join(base, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if name == "run-old" || name == "run-recent" {
			if err := os.WriteFile(filepath.Join(path, ".aimebu-detect-run"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if name != "run-recent" {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	file := filepath.Join(base, "run-old-file")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(base, "run-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	run, err := (&Client{ConfigDir: config}).detectDefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(run, base+string(os.PathSeparator)+"run-") {
		t.Fatalf("fresh run folder: %q", run)
	}
	if _, err := os.Stat(filepath.Join(base, "run-old")); !os.IsNotExist(err) {
		t.Errorf("stale run folder remains: %v", err)
	}
	for _, path := range []string{filepath.Join(base, "run-recent"), filepath.Join(base, "run-chosen"), filepath.Join(base, "other-old"), file, link, outside} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("unrelated or recent path %s was removed: %v", path, err)
		}
	}
}

func TestDetectModelsCleanupFailureKeepsSuccessfulListing(t *testing.T) {
	harness := writeScript(t, t.TempDir(), "codex", `echo '{"models":[{"slug":"gpt-6-astra","display_name":"GPT-6-Astra","visibility":"list"}]}'`)
	called := false
	models, err := detectModelsInRun(t.Context(), "codex", harness, nil, t.TempDir(), false, func(string) error {
		called = true
		return errors.New("cannot remove run folder")
	})
	if err != nil || len(models) != 1 || models[0].Slug != "gpt-6-astra" || !called {
		t.Fatalf("cleanup failure hid the model listing: %+v, %v, cleanup called %t", models, err, called)
	}
}

func TestDetectModelsRunsInPrivateDetectDirOrTheChosenFolder(t *testing.T) {
	resolved := func(dir string) string {
		t.Helper()
		out, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	bin, config, chosen, refused := t.TempDir(), resolved(t.TempDir()), resolved(t.TempDir()), resolved(t.TempDir())
	if err := os.WriteFile(filepath.Join(config, "host-machine.key"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	seen := filepath.Join(t.TempDir(), "seen")
	harness := writeScript(t, bin, "codex", `
if [ "$(pwd -P)" = "`+refused+`" ]; then
  echo "noise first" >&2
  printf '\033[31mcannot answer from %s\033[0m\n\n' "$(pwd -P)" >&2
  exit 1
fi
pwd -P > `+seen+`
[ "$*" = "--profile work debug models --bundled" ] || { echo "unexpected arguments: $*" >&2; exit 2; }
[ ! -e host-machine.key ] || { echo "config secret exposed" >&2; exit 2; }
echo generated > generated.txt
echo '{"models":[{"slug":"gpt-6-astra","display_name":"GPT-6-Astra","visibility":"list"}]}'`)
	client := &Client{ConfigDir: config}
	ask := func(dir string, args ...string) protocol.Frame {
		t.Helper()
		return client.detectResult(t.Context(), protocol.Frame{ReqID: "r", Detect: &protocol.DetectRequest{Kind: "models", Family: "codex", Command: harness, Args: args, Dir: dir}})
	}
	ranIn := func() string {
		t.Helper()
		out, err := os.ReadFile(seen)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	detectDir := filepath.Join(config, "detect")
	if answer := ask("", "--profile", "work"); answer.Code != "" || len(answer.ModelCandidates) != 1 || answer.ModelCandidates[0].Slug != "gpt-6-astra" || !strings.HasPrefix(ranIn(), detectDir+string(os.PathSeparator)+"run-") {
		t.Fatalf("default folder: %+v ran in %q", answer, ranIn())
	}
	firstRun := ranIn()
	if info, err := os.Stat(detectDir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("default folder is not private: %v, %v", info, err)
	}
	if _, err := os.Stat(firstRun); !os.IsNotExist(err) {
		t.Fatalf("the first run folder was not removed: %v", err)
	}
	if answer := ask("", "--profile", "work"); answer.Code != "" || ranIn() == firstRun {
		t.Fatalf("each run needs a fresh folder: %+v ran in %q", answer, ranIn())
	}
	if _, err := os.Stat(ranIn()); !os.IsNotExist(err) {
		t.Fatalf("the second run folder was not removed: %v", err)
	}
	if answer := ask(""); answer.Code != "detect_failed" || answer.DetectDir != detectDir || !strings.Contains(answer.Message, ranIn()) {
		t.Errorf("a failed command must report its temporary working directory: %+v", answer)
	}
	if _, err := os.Stat(ranIn()); !os.IsNotExist(err) {
		t.Fatalf("the failed run folder was not removed: %v", err)
	}
	if answer := ask(chosen, "--profile", "work"); answer.Code != "" || ranIn() != chosen {
		t.Fatalf("a chosen folder wins: %+v ran in %q", answer, ranIn())
	}
	chosenRun := filepath.Join(detectDir, "run-chosen")
	if err := os.Mkdir(chosenRun, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chosenRun, ".aimebu-detect-run"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if answer := ask(chosenRun, "--profile", "work"); answer.Code != "" || ranIn() != chosenRun {
		t.Fatalf("chosen folder under detect: %+v ran in %q", answer, ranIn())
	}
	if _, err := os.Lstat(filepath.Join(chosenRun, ".aimebu-detect-run")); !os.IsNotExist(err) {
		t.Fatalf("owner-chosen run folder retained its cleanup marker: %v", err)
	}
	old := time.Now().Add(-2 * detectStaleRunAge)
	if err := os.Chtimes(chosenRun, old, old); err != nil {
		t.Fatal(err)
	}
	if answer := ask("", "--profile", "work"); answer.Code != "" {
		t.Fatalf("default detection after chosen folder: %+v", answer)
	}
	if _, err := os.Stat(filepath.Join(chosenRun, "generated.txt")); err != nil {
		t.Fatalf("default detection removed the owner-chosen folder: %v", err)
	}
	answer := ask(refused, "--profile", "work")
	if answer.Code != "detect_failed" || !strings.Contains(answer.Message, "noise first cannot answer from "+refused) || strings.Contains(answer.Message, "\033") || !strings.Contains(answer.Message, "exit status 1") {
		t.Fatalf("the harness's stderr reaches the person: %+v", answer)
	}
	if answer := ask(chosen); answer.Code != "detect_failed" || !strings.Contains(answer.Message, "unexpected arguments: debug models --bundled") {
		t.Fatalf("without launcher arguments only the listing ones are passed: %+v", answer)
	}
	if answer := ask("relative/dir"); answer.Code != "invalid_dir" || !strings.Contains(answer.Message, "absolute path") {
		t.Errorf("a relative folder: %+v", answer)
	}
	if answer := ask(filepath.Join(chosen, "missing")); answer.Code != "invalid_dir" || !strings.Contains(answer.Message, "does not exist") {
		t.Errorf("a missing folder: %+v", answer)
	}
	if answer := ask(seen); answer.Code != "invalid_dir" {
		t.Errorf("a file is not a folder: %+v", answer)
	}
	if _, err := detectModels(t.Context(), "codex", harness, []string{"a\x00b"}, chosen); err == nil {
		t.Error("a NUL in a launcher argument is refused")
	}
	if err := os.WriteFile(filepath.Join(detectDir, "leftover"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if answer := ask("", "--profile", "work"); answer.Code != "" || ranIn() == firstRun {
		t.Errorf("a leftover in the parent folder must not affect a new run: %+v", answer)
	}
	if answer := ask(chosen, "--profile", "work"); answer.Code != "" || ranIn() != chosen {
		t.Errorf("an explicit folder still takes precedence: %+v", answer)
	}
	if _, err := (&Client{ConfigDir: filepath.Join(config, "gone")}).detectDefaultDir(); err == nil {
		t.Error("a missing config directory must fail")
	}
	if _, err := (&Client{}).detectDefaultDir(); err == nil {
		t.Error("an unknown config directory must fail")
	}
	if err := os.RemoveAll(detectDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(chosen, detectDir); err != nil {
		t.Fatal(err)
	}
	if answer := ask("", "--profile", "work"); answer.Code != "detect_failed" || !strings.Contains(answer.Message, "private directory") || !strings.Contains(answer.Message, detectDir) {
		t.Errorf("a symlinked default folder must fail: %+v", answer)
	}
	if err := os.Remove(detectDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(detectDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(detectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if answer := ask("", "--profile", "work"); answer.Code != "detect_failed" || !strings.Contains(answer.Message, "private directory") || !strings.Contains(answer.Message, detectDir) {
		t.Errorf("a public default folder must fail: %+v", answer)
	}
}

func TestConcurrentModelDetectionsUseSeparateRunFolders(t *testing.T) {
	config, bin := t.TempDir(), t.TempDir()
	seen := []string{filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")}
	harness := writeScript(t, bin, "codex", `
out="$1"
shift
pwd -P > "$out"
echo generated > generated.txt
sleep 0.1
echo '{"models":[{"slug":"gpt-6-astra","display_name":"GPT-6-Astra","visibility":"list"}]}'`)
	client := &Client{ConfigDir: config}
	results := make(chan protocol.Frame, len(seen))
	for _, output := range seen {
		go func(output string) {
			results <- client.detectResult(t.Context(), protocol.Frame{Detect: &protocol.DetectRequest{Kind: "models", Family: "codex", Command: harness, Args: []string{output}}})
		}(output)
	}
	for range seen {
		if answer := <-results; answer.Code != "" || len(answer.ModelCandidates) != 1 {
			t.Fatalf("concurrent model detection: %+v", answer)
		}
	}
	var paths []string
	for _, output := range seen {
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		path := strings.TrimSpace(string(data))
		if !strings.HasPrefix(path, filepath.Join(config, "detect")+string(os.PathSeparator)+"run-") {
			t.Fatalf("unexpected working directory: %s", path)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("run folder %s was not removed: %v", path, err)
		}
		paths = append(paths, path)
	}
	if paths[0] == paths[1] {
		t.Fatalf("concurrent detections shared a working directory: %v", paths)
	}
}

func TestTailWriterKeepsTheEndOfStderr(t *testing.T) {
	t.Parallel()
	tail := &tailWriter{keep: 16}
	_, _ = tail.Write([]byte(strings.Repeat("x", 40) + "\nfirst\n"))
	_, _ = tail.Write([]byte("second\n\n"))
	if len(tail.buf) != 16 {
		t.Fatalf("kept %d bytes", len(tail.buf))
	}
	if got := tail.lastLines(3, 400); got != "x first second" {
		t.Errorf("last lines: %q", got)
	}
	if got := tail.lastLines(1, 4); got != "…ond" {
		t.Errorf("cut to the end: %q", got)
	}
}

// Vibe has no command that lists models: the config's models are offered by alias with Vibe's built-in default, whatever the executable
// would print, and only the models and providers tables are read (never the .env beside it).
func TestDetectModelsOfVibeReadsTheConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("VIBE_HOME", home)
	vibe := writeScript(t, t.TempDir(), "vibe", `echo must-not-be-run >&2; exit 9`)
	config := "[[providers]]\nname = \"acme\"\n\n[[models]]\nname = \"acme-large\"\nalias = \"large\"\ndisplay_name = \"Acme Large\"\nprovider = \"acme\"\ninput_price = 2\noutput_price = 6\n\n" +
		"[[models]]\nname = \"mistral-medium-3.5\"\nalias = \"mistral-medium-3.5\"\n\n[[mcp_servers]]\nname = \"grafana\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".env"), []byte("MISTRAL_API_KEY=never-read\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	got, err := detectModels(t.Context(), "vibe", vibe, nil, "")
	if err != nil || len(got) != 2 {
		t.Fatalf("models: %+v %v", got, err)
	}
	if got[0].Slug != "large" || got[0].Label != "Acme Large" || !strings.Contains(got[0].Note, "provider acme") || !strings.Contains(got[0].Note, "$2 in / $6 out") {
		t.Errorf("the config's model: %+v", got[0])
	}
	if got[1].Slug != "mistral-medium-3.5" {
		t.Errorf("the built-in default is listed once, not again when the config names it: %+v", got)
	}

	// No config at all: Vibe's default is still offered.
	empty := t.TempDir()
	t.Setenv("VIBE_HOME", empty)
	if got, err := detectModels(t.Context(), "vibe", vibe, nil, ""); err != nil || len(got) != 1 || got[0].Slug != "mistral-medium-3.5" || got[0].Label != "Mistral Medium 3.5" {
		t.Fatalf("no config: %+v %v", got, err)
	}
	// The executable still has to be usable.
	if _, err := detectModels(t.Context(), "vibe", filepath.Join(empty, "missing"), nil, ""); err == nil {
		t.Fatal("a harness that is not there lists nothing")
	}
}

func TestDetectHarnessesFindsVibe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", dir)
	vibe := writeScript(t, dir, "vibe", `echo "vibe 2.25.0"`)
	for _, candidate := range detectHarnesses(t.Context()) {
		if candidate.Command == vibe {
			if candidate.Family != "vibe" || candidate.Containerized || candidate.Version != "vibe 2.25.0" {
				t.Fatalf("vibe: %+v", candidate)
			}
			return
		}
	}
	t.Fatal("the vibe executable on PATH was not found")
}

// The listing command is run with the launcher's built-in flags cleared: a Docker launcher would otherwise put them (OpenCode's --auto)
// before `models`.
func TestDetectModelsClearsTheLaunchersBuiltInFlags(t *testing.T) {
	t.Setenv("OPENCODE_LAUNCH_FLAGS", "--auto")
	launcher := writeScript(t, t.TempDir(), "opencode-docker", `if [ "${OPENCODE_LAUNCH_FLAGS+x}" = x ] && [ -z "$OPENCODE_LAUNCH_FLAGS" ] && [ "$1" = models ]; then printf 'opencode/a\n'; else echo "flags: [$OPENCODE_LAUNCH_FLAGS] first: $1" >&2; exit 3; fi`)
	got, err := detectModels(t.Context(), "opencode", launcher, nil, "")
	if err != nil || len(got) != 1 || got[0].Slug != "opencode/a" {
		t.Fatalf("models: %+v %v", got, err)
	}
}

// A model with no display name is labelled by its model and provider; a container cannot reach a provider on the machine's own loopback
// address, which detection says so the model is not added by accident.
func TestDetectModelsOfVibeLabelsByProviderAndFlagsLoopbackForContainers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("VIBE_HOME", home)
	vibe := writeScript(t, t.TempDir(), "vibe", `exit 9`)
	config := "[[providers]]\nname = \"llamacpp\"\napi_base = \"http://127.0.0.1:8080/v1\"\n\n[[providers]]\nname = \"mistral\"\napi_base = \"https://api.mistral.ai/v1\"\n\n" +
		"[[providers]]\nname = \"lan\"\napi_base = \"http://192.168.1.20:1234/v1\"\n\n" +
		"[[models]]\nname = \"devstral-small-latest\"\nalias = \"local\"\nprovider = \"llamacpp\"\n\n" +
		"[[models]]\nname = \"mistral-large\"\nalias = \"large\"\nprovider = \"mistral\"\n\n[[models]]\nname = \"qwen\"\nalias = \"lan-qwen\"\nprovider = \"lan\"\n\n" +
		"[[models]]\nname = \"shown\"\nalias = \"shown\"\ndisplay_name = \"Shown As\"\nprovider = \"llamacpp\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	bySlug := func(models []protocol.ModelCandidate) map[string]protocol.ModelCandidate {
		out := map[string]protocol.ModelCandidate{}
		for _, model := range models {
			out[model.Slug] = model
		}
		return out
	}
	native, err := detectModelsFor(t.Context(), "vibe", vibe, nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	got := bySlug(native)
	if got["local"].Label != "devstral-small-latest (llamacpp)" || got["large"].Label != "mistral-large (mistral)" || got["shown"].Label != "Shown As" {
		t.Fatalf("labels: %+v", got)
	}
	for slug, model := range got {
		if model.Unreachable != "" {
			t.Fatalf("a native harness reaches its own loopback: %s %+v", slug, model)
		}
	}
	docker, err := detectModelsFor(t.Context(), "vibe", vibe, nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	got = bySlug(docker)
	if !strings.Contains(got["local"].Unreachable, "127.0.0.1:8080") || got["shown"].Unreachable == "" {
		t.Fatalf("loopback providers are flagged for a container: %+v", got)
	}
	if got["large"].Unreachable != "" || got["lan-qwen"].Unreachable != "" {
		t.Fatalf("a provider on the internet or the LAN is reachable from a container: %+v", got)
	}
	for base, want := range map[string]bool{"http://localhost:1/v1": true, "http://[::1]:1/v1": true, "http://0.0.0.0:1": true, "http://host.docker.internal:1/v1": false, "": false, "not a url": false, "https://api.x.com": false} {
		if loopbackBase(base) != want {
			t.Fatalf("loopbackBase(%q) != %v", base, want)
		}
	}
}

// A container launcher in the harness list gets its version from the machine's project folders in turn, or says why it has none.
func TestDetectContainerHarnessesAskTheMachinesFolders(t *testing.T) {
	dir, good, bad := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", dir)
	writeScript(t, dir, "claude-docker", `case "$PWD" in *`+filepath.Base(good)+`) echo "2.1.284 (Claude Code)";; *) echo "Error: Directory '$PWD' is not mounted in the harness container." >&2; exit 1;; esac`)
	writeScript(t, dir, "codex-docker", `echo "Error: Directory '$PWD' is not mounted in the harness container." >&2; exit 1`)
	mine := func(dirs []string) map[string]protocol.HarnessCandidate {
		out := map[string]protocol.HarnessCandidate{}
		for _, candidate := range detectHarnessesIn(t.Context(), dirs) {
			if strings.HasPrefix(candidate.Command, dir) {
				out[candidate.Family] = candidate
			}
		}
		return out
	}
	got := mine([]string{bad, good})
	if claude := got["claude"]; claude.Version != "2.1.284 (Claude Code)" || claude.VersionProblem != "" || !claude.Containerized {
		t.Fatalf("claude-docker: %+v", claude)
	}
	if codex := got["codex"]; codex.Version != "" || !strings.Contains(codex.VersionProblem, "none of this machine's 2 project folders is mounted") {
		t.Fatalf("codex-docker, whose folder is not mounted: %+v", codex)
	}
	got = mine(nil)
	if claude := got["claude"]; claude.Version != "" || !strings.Contains(claude.VersionProblem, "no project folder is set on this machine yet") {
		t.Fatalf("no folder known: %+v", claude)
	}
}

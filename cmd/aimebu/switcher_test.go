package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func TestSwitcherCLIUsesOnlyV2TemporaryRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	t.Setenv("AIMEBU2_CONFIG_DIR", filepath.Join(root, ".aimebu2"))
	call := func(args ...string) string {
		t.Helper()
		var out, errs bytes.Buffer
		if code := run(append([]string{"switcher"}, args...), &out, &errs); code != 0 {
			t.Fatalf("switcher %v exit=%d stderr=%s", args, code, errs.String())
		}
		return out.String()
	}
	call("enable")
	call("add", "codex", "empty")
	if got := call("list"); !strings.Contains(got, "codex") || !strings.Contains(got, "empty") {
		t.Fatalf("list=%s", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".aimebu2", "switcher", "profiles", "codex", "empty")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".aimebu", "switcher")); !os.IsNotExist(err) {
		t.Fatalf("v1 state was created: %v", err)
	}
}

// The fixture uses the version-1 state, profile link, and backup layout
// produced by v1's switcher tests, with synthetic credentials under HOME.
func TestSwitcherCLIUsesV1TreeInPlaceAtCutover(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, ".aimebu")
	t.Setenv("HOME", home)
	t.Setenv("AIMEBU2_CONFIG_DIR", config)
	t.Setenv("AIMEBU_CONFIG_DIR", filepath.Join(home, "unused-v1-override"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	root := filepath.Join(config, "switcher")
	live := filepath.Join(home, ".codex", "auth.json")
	active := filepath.Join(root, "profiles", "codex", "main", "auth.json")
	inactive := filepath.Join(root, "profiles", "codex", "other", "auth.json")
	backup := filepath.Join(root, "backups", "codex", "main-1.json")
	for _, dir := range []string{filepath.Dir(live), filepath.Dir(active), filepath.Dir(inactive), filepath.Dir(backup)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mainCred := []byte(`{"tokens":{"access_token":"main","refresh_token":"main-refresh"}}`)
	otherCred := []byte(`{"tokens":{"access_token":"other","refresh_token":"other-refresh"}}`)
	state := []byte("{\n  \"version\": 1,\n  \"enabled\": true,\n  \"active\": {\"codex\": \"main\"}\n}\n")
	for path, data := range map[string][]byte{live: mainCred, inactive: otherCred, backup: mainCred, filepath.Join(root, "state.json"): state} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(live, active); err != nil {
		t.Fatal(err)
	}
	separate := filepath.Join(home, ".aimebu2", "switcher", "state.json")
	if err := os.MkdirAll(filepath.Dir(separate), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(separate, []byte("separate development state"), 0o600); err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) (string, int) {
		var out, errs bytes.Buffer
		code := run(append([]string{"switcher"}, args...), &out, &errs)
		if code != 0 {
			return errs.String(), code
		}
		return out.String(), code
	}
	if got, code := call("list"); code != 0 || !strings.Contains(got, "main") || !strings.Contains(got, "other") {
		t.Fatalf("v1 profile list: code=%d output=%s", code, got)
	}
	if got, err := os.ReadFile(filepath.Join(root, "state.json")); err != nil || !bytes.Equal(got, state) {
		t.Fatalf("read-only list changed v1 state: %s %v", got, err)
	}
	done := make(chan struct {
		output string
		code   int
	}, 1)
	go func() {
		output, code := call("use", "codex", "other")
		done <- struct {
			output string
			code   int
		}{output, code}
	}()
	select {
	case result := <-done:
		if result.code != 0 {
			t.Fatalf("switch v1 tree: %s", result.output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("switch blocked while locking the same v1 tree")
	}
	var saved struct {
		Version int               `json:"version"`
		Enabled bool              `json:"enabled"`
		Active  map[string]string `json:"active"`
	}
	raw, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Version != 1 || !saved.Enabled || saved.Active["codex"] != "other" {
		t.Fatalf("v1-compatible state: %s %v", raw, err)
	}
	if got, err := os.ReadFile(live); err != nil || !bytes.Equal(got, otherCred) {
		t.Fatalf("live credentials after swap: %s %v", got, err)
	}
	if info, err := os.Lstat(inactive); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("new active profile is not linked: %v %v", info, err)
	}
	if info, err := os.Lstat(active); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("old active profile is not a snapshot: %v %v", info, err)
	}
	if got, err := os.ReadFile(separate); err != nil || string(got) != "separate development state" {
		t.Fatalf("separate profiles changed: %s %v", got, err)
	}
}

// switcherEnv isolates HOME and the v2 config root under a temp directory and
// returns a runner for `aimebu switcher` with optional stdin.
func switcherEnv(t *testing.T) (home string, call func(stdin string, args ...string) (string, string, int)) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("AIMEBU2_CONFIG_DIR", filepath.Join(home, ".aimebu2"))
	call = func(stdin string, args ...string) (string, string, int) {
		t.Helper()
		var out, errs bytes.Buffer
		code := switcherCommand(args, strings.NewReader(stdin), &out, &errs)
		return out.String(), errs.String(), code
	}
	return home, call
}

func writeCodexLogin(t *testing.T, home, token string) {
	t.Helper()
	path := filepath.Join(home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"tokens":{"access_token":"` + token + `","refresh_token":"` + token + `-refresh"}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSwitcherCLIHelpAndUsageErrors(t *testing.T) {
	_, call := switcherEnv(t)
	if out, _, code := call("", "help"); code != 0 || !strings.Contains(out, "rename <tool> <old> <new>") {
		t.Fatalf("help: code=%d out=%q", code, out)
	}
	if _, errs, code := call(""); code != 2 || !strings.Contains(errs, "Usage: aimebu switcher") {
		t.Fatalf("no args: code=%d err=%q", code, errs)
	}
	if _, errs, code := call("", "bogus"); code != 2 || !strings.Contains(errs, "Unknown switcher command: bogus") {
		t.Fatalf("unknown: code=%d err=%q", code, errs)
	}
	if _, errs, code := call("", "list", "--bogus"); code != 2 || !strings.Contains(errs, "unknown flag --bogus") {
		t.Fatalf("bad flag: code=%d err=%q", code, errs)
	}
	if _, errs, code := call("", "rename", "codex", "a"); code != 2 || !strings.Contains(errs, "usage: aimebu switcher rename <tool> <old> <new>") {
		t.Fatalf("rename arity: code=%d err=%q", code, errs)
	}
}

func TestSwitcherCLIEnableDisableMessages(t *testing.T) {
	_, call := switcherEnv(t)
	if out, _, code := call("", "enable"); code != 0 || out != "Switcher enabled.\n" {
		t.Fatalf("enable: code=%d out=%q", code, out)
	}
	if out, _, code := call("", "disable"); code != 0 || out != "Switcher disabled.\n" {
		t.Fatalf("disable: code=%d out=%q", code, out)
	}
	// Extra arguments are a usage error and must not change the flag.
	for _, args := range [][]string{{"enable", "--bogus"}, {"disable", "extra"}} {
		if _, errs, code := call("", args...); code != 2 || !strings.Contains(errs, "usage: aimebu switcher "+args[0]) {
			t.Fatalf("%v: code=%d err=%q", args, code, errs)
		}
	}
	if out, _, _ := call("", "status"); !strings.Contains(out, "Switcher: disabled") {
		t.Fatalf("flag changed by rejected command: %q", out)
	}
}

func TestSwitcherCLIListAndStatusTables(t *testing.T) {
	home, call := switcherEnv(t)
	if out, _, code := call("", "list"); code != 0 || !strings.Contains(out, "No profiles found. Run 'aimebu switcher import <tool> <name>' to get started.") {
		t.Fatalf("empty list: code=%d out=%q", code, out)
	}
	call("", "enable")
	writeCodexLogin(t, home, "main")
	if _, errs, code := call("", "import", "codex", "main"); code != 0 {
		t.Fatalf("import: %s", errs)
	}
	if _, errs, code := call("", "add", "codex", "spare"); code != 0 {
		t.Fatalf("add: %s", errs)
	}
	out, _, code := call("", "list")
	if code != 0 || !strings.Contains(out, "TOOL") || !strings.Contains(out, "ACTIVE") || !strings.Contains(out, "CREDS") {
		t.Fatalf("list table: code=%d out=%q", code, out)
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[1] == "main" && (fields[2] != "*" || fields[3] != "yes") {
			t.Fatalf("main row: %q", line)
		}
		if len(fields) >= 4 && fields[1] == "spare" && (fields[2] != "-" || fields[3] != "no") {
			t.Fatalf("spare row: %q", line)
		}
	}
	out, _, code = call("", "status")
	if code != 0 || !strings.Contains(out, "Switcher: enabled") || !strings.Contains(out, "TOOL") || !strings.Contains(out, "ACTIVE") || !strings.Contains(out, "ELIGIBLE") {
		t.Fatalf("status table: code=%d out=%q", code, out)
	}
	var codexRow []string
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "codex" {
			codexRow = fields
		}
	}
	if len(codexRow) < 3 || codexRow[1] != "main" || codexRow[2] != "yes" {
		t.Fatalf("codex status row: %v in %q", codexRow, out)
	}
	jsonOut, _, code := call("", "list", "--json")
	var listed struct {
		Profiles []struct {
			Tool, Name string
			Active     bool
		} `json:"profiles"`
	}
	if code != 0 || json.Unmarshal([]byte(jsonOut), &listed) != nil || len(listed.Profiles) != 2 {
		t.Fatalf("list --json: code=%d out=%q", code, jsonOut)
	}
	jsonOut, _, code = call("", "status", "--json")
	var status struct {
		Enabled bool              `json:"enabled"`
		Active  map[string]string `json:"active"`
	}
	if code != 0 || json.Unmarshal([]byte(jsonOut), &status) != nil || !status.Enabled || status.Active["codex"] != "main" {
		t.Fatalf("status --json: code=%d out=%q", code, jsonOut)
	}
}

func TestSwitcherCLIUseConfirmsEmptyProfile(t *testing.T) {
	home, call := switcherEnv(t)
	call("", "enable")
	writeCodexLogin(t, home, "main")
	call("", "import", "codex", "main")
	call("", "add", "codex", "blank")
	live := filepath.Join(home, ".codex", "auth.json")

	// A non-terminal stdin cannot confirm, so the switch is refused untouched.
	out, errs, code := call("", "use", "codex", "blank")
	if code != 1 || !strings.Contains(out, `Profile "blank" is empty`) || !strings.Contains(errs, "stdin is not a terminal; use -y") {
		t.Fatalf("unconfirmed: code=%d out=%q err=%q", code, out, errs)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live login removed without confirmation: %v", err)
	}
	if out, errs, code := call("", "use", "codex", "blank", "-y"); code != 0 || !strings.Contains(out, `Switched codex to profile "blank".`) {
		t.Fatalf("confirmed: code=%d out=%q err=%q", code, out, errs)
	}
	if out, _, code := call("", "use", "codex", "blank", "--yes"); code != 0 || !strings.Contains(out, `Already on codex profile "blank"; nothing to do.`) {
		t.Fatalf("repeat: code=%d out=%q", code, out)
	}
}

func TestSwitcherCLIForceRequiresStoredCredentialsAndConfirmation(t *testing.T) {
	home, call := switcherEnv(t)
	call("", "enable")
	writeCodexLogin(t, home, "main")
	call("", "import", "codex", "main")
	call("", "add", "codex", "target")
	live := filepath.Join(home, ".codex", "auth.json")
	if _, errs, code := call("", "use", "codex", "target", "--force", "-y"); code != 1 || !strings.Contains(errs, "requires stored credentials") {
		t.Fatalf("empty target: code=%d err=%q", code, errs)
	}
	if got, err := os.ReadFile(live); err != nil || !strings.Contains(string(got), "main-refresh") {
		t.Fatalf("empty force changed live credentials: %s %v", got, err)
	}
	stored := filepath.Join(home, ".aimebu2", "switcher", "profiles", "codex", "target", "auth.json")
	if err := os.WriteFile(stored, []byte(`{"tokens":{"access_token":"target","refresh_token":"target-refresh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errs, code := call("", "use", "codex", "target", "--force"); code != 1 || !strings.Contains(errs, "stdin is not a terminal") {
		t.Fatalf("force without confirmation: code=%d err=%q", code, errs)
	}
	if out, errs, code := call("", "use", "codex", "target", "--force", "-y"); code != 0 || !strings.Contains(out, "without refreshing") {
		t.Fatalf("force: code=%d out=%q err=%q", code, out, errs)
	}
	if got, err := os.ReadFile(live); err != nil || !strings.Contains(string(got), "target-refresh") {
		t.Fatalf("force did not install target: %s %v", got, err)
	}
}

func TestSwitcherCLIRenameRemoveAndHints(t *testing.T) {
	home, call := switcherEnv(t)
	if _, errs, code := call("", "use", "codex", "x"); code != 1 || !strings.Contains(errs, "switcher is disabled; enable it with 'aimebu switcher enable'") {
		t.Fatalf("disabled hint: code=%d err=%q", code, errs)
	}
	call("", "enable")
	writeCodexLogin(t, home, "main")
	if _, errs, code := call("", "use", "codex", "main"); code != 1 || !strings.Contains(errs, "run 'aimebu switcher import <tool> <name>' first") {
		t.Fatalf("no-active hint: code=%d err=%q", code, errs)
	}
	call("", "import", "codex", "main")
	call("", "add", "codex", "spare")
	if _, errs, code := call("", "add", "codex", "spare"); code != 1 || !strings.Contains(errs, "profile already exists") {
		t.Fatalf("exists: code=%d err=%q", code, errs)
	}
	if out, errs, code := call("", "rename", "codex", "spare", "other"); code != 0 || out != "Renamed codex profile \"spare\" to \"other\".\n" {
		t.Fatalf("rename: code=%d out=%q err=%q", code, out, errs)
	}
	if _, errs, code := call("", "rename", "codex", "spare", "again"); code != 1 || !strings.Contains(errs, "profile not found") {
		t.Fatalf("rename missing: code=%d err=%q", code, errs)
	}
	if out, errs, code := call("", "remove", "codex", "other"); code != 0 || out != "Removed codex profile \"other\".\n" {
		t.Fatalf("remove inactive: code=%d out=%q err=%q", code, out, errs)
	}
	// The active profile needs confirmation; piped stdin cannot give it.
	out, errs, code := call("", "remove", "codex", "main")
	if code != 1 || !strings.Contains(out, `Profile "main" is currently active`) || !strings.Contains(errs, "stdin is not a terminal") {
		t.Fatalf("remove active unconfirmed: code=%d out=%q err=%q", code, out, errs)
	}
	if _, errs, code := call("", "remove", "codex", "main", "-y"); code != 0 {
		t.Fatalf("remove active -y: %s", errs)
	}
}

// migrate-copies converts a link-based tree, and the very same CLI process
// family can then switch: the manager the CLI builds must carry the fan-out
// hooks (a nil hook would fail every switch after migration).
func TestSwitcherCLIMigrateCopiesThenUse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("AIMEBU2_CONFIG_DIR", filepath.Join(home, ".aimebu2"))
	expiry := time.Now().Add(6 * time.Hour).UnixMilli()
	cred := func(access, refresh string) string {
		return `{"claudeAiOauth":{"accessToken":"` + access + `","refreshToken":"` + refresh + `","expiresAt":` + fmt.Sprint(expiry) + `}}`
	}
	live := filepath.Join(home, ".claude", ".credentials.json")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Dir(live), 0o700))
	must(os.WriteFile(live, []byte(cred("tok-a", "ref-a")), 0o600))
	call := func(wantCode int, args ...string) string {
		t.Helper()
		var out, errs bytes.Buffer
		if code := run(append([]string{"switcher"}, args...), &out, &errs); code != wantCode {
			t.Fatalf("switcher %v exit=%d (want %d) stdout=%s stderr=%s", args, code, wantCode, out.String(), errs.String())
		}
		return out.String() + errs.String()
	}
	call(0, "enable")
	call(0, "import", "claude", "a")
	call(0, "add", "claude", "b")
	b := filepath.Join(home, ".aimebu2", "switcher", "profiles", "claude", "b", ".credentials.json")
	must(os.WriteFile(b, []byte(cred("tok-b", "ref-b")), 0o600))

	out := call(0, "migrate-copies", "--tool", "claude")
	if !strings.Contains(out, "claude") {
		t.Fatalf("migrate output = %q", out)
	}
	if got, _ := os.ReadFile(live); strings.Contains(string(got), "refreshToken") || !strings.Contains(string(got), "tok-a") {
		t.Fatalf("live file after migrate = %s", got)
	}
	a := filepath.Join(home, ".aimebu2", "switcher", "profiles", "claude", "a", ".credentials.json")
	if info, err := os.Lstat(a); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("active profile store file is a link or missing: %v %v", info, err)
	}
	if again := call(0, "migrate-copies", "--tool", "claude"); !strings.Contains(again, "Already migrated") {
		t.Fatalf("second migrate = %q", again)
	}

	call(0, "use", "claude", "b", "-y")
	if got, _ := os.ReadFile(live); !strings.Contains(string(got), "tok-b") || strings.Contains(string(got), "refreshToken") {
		t.Fatalf("live file after use = %s", got)
	}
	if got, _ := os.ReadFile(b); !strings.Contains(string(got), "ref-b") {
		t.Fatalf("b lost its refresh token: %s", got)
	}
}

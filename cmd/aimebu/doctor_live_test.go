package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/protocol"
)

func TestReadDoctorKey(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "server", "doctor.key")
	if _, check, ok := readDoctorKey(root); ok || check.Status != statusWarn || check.Hint == "" {
		t.Fatalf("missing: %+v", check)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, check, ok := readDoctorKey(root)
	if !ok || key != "abc123" || check.Status != statusOK || strings.Contains(check.Description, "abc123") {
		t.Fatalf("private: %q %+v", key, check)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if key, check, ok := readDoctorKey(root); ok || key != "" || check.Status != statusWarn {
		t.Fatalf("world-readable key must not be used: %q %+v", key, check)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, check, ok := readDoctorKey(root); ok || check.Status != statusWarn {
		t.Fatalf("empty: %+v", check)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", path); err != nil {
		t.Fatal(err)
	}
	if _, check, ok := readDoctorKey(root); ok || check.Status != statusWarn {
		t.Fatalf("a symlink must not be followed: %+v", check)
	}
}

func TestLiveMachineChecks(t *testing.T) {
	supports := func(missing ...string) []protocol.DoctorSupport {
		var out []protocol.DoctorSupport
		for _, flag := range []string{"supports_path_probe", "supports_worktree", "supports_stop_replay"} {
			supported := true
			for _, m := range missing {
				supported = supported && m != flag
			}
			out = append(out, protocol.DoctorSupport{Flag: flag, Supported: supported, Degrades: "it degrades " + flag})
		}
		return out
	}
	live := protocol.DoctorLive{Machines: []protocol.DoctorMachine{
		{ID: "m1", Name: "current", Approved: true, Connected: true, Supports: supports(), Launchers: []protocol.DoctorLauncher{
			{ID: "l1", Tool: "opencode", Name: "docker", Label: "OpenCode (Docker)", Enabled: true, Reported: true, Available: true, Version: "1.0", Features: []string{"forward_env"}},
			{ID: "l2", Tool: "vibe", Name: "docker", Enabled: true, Reported: true, Available: true, FeatureProblem: "launcher too old: it does not forward environment variables"},
			{ID: "l3", Tool: "claude", Name: "native", Enabled: true, Reported: true, Available: false, UnavailableReason: "command not found on this machine"},
			{ID: "l4", Tool: "pi", Name: "native", Enabled: true},
			{ID: "l5", Tool: "codex", Name: "native"}, // disabled: not judged
		}},
		{ID: "m2", Name: "old", Approved: true, Connected: true, Supports: supports("supports_worktree", "supports_stop_replay")},
		{ID: "m3", Name: "away", Approved: true},
		{ID: "m4", Name: "pending"}, // not approved: left to the offline check
	}}
	byName := map[string]doctorCheck{}
	for _, check := range liveMachineChecks(live) {
		byName[check.Name] = check
	}
	want := map[string]string{
		"machine current":                       statusOK,
		"launcher OpenCode (Docker) on current": statusOK,
		"launcher vibe/docker on current":       statusWarn,
		"launcher claude/native on current":     statusWarn,
		"launcher pi/native on current":         statusWarn,
		"machine old":                           statusWarn,
		"machine away":                          statusOK,
	}
	for name, status := range want {
		if byName[name].Status != status {
			t.Errorf("%s: %+v, want %s", name, byName[name], status)
		}
	}
	if len(byName) != len(want) {
		t.Errorf("unexpected checks: %v", byName)
	}
	old := byName["machine old"]
	if !strings.Contains(old.Description, "supports_worktree (it degrades supports_worktree)") || !strings.Contains(old.Description, "supports_stop_replay") || strings.Contains(old.Description, "path_probe") || old.Hint == "" {
		t.Errorf("old machine: %+v", old)
	}
	if got := byName["launcher claude/native on current"]; !strings.Contains(got.Description, "command not found") {
		t.Errorf("unavailable launcher: %+v", got)
	}
	// With nothing connected no agent can run.
	none := liveMachineChecks(protocol.DoctorLive{Machines: []protocol.DoctorMachine{{ID: "m", Name: "x", Approved: true}}})
	found := false
	for _, check := range none {
		found = found || (check.Name == "connected machines" && check.Status == statusWarn)
	}
	if !found {
		t.Errorf("no connected machine must warn: %+v", none)
	}
}

func TestDoctorLiveCommand(t *testing.T) {
	root := t.TempDir()
	key := strings.Repeat("ab", 32)
	keyPath := filepath.Join(root, "server", "doctor.key")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	createFixtureDatabase(t, filepath.Join(root, "server", "aimebu.sqlite"))
	answer := protocol.DoctorLive{Machines: []protocol.DoctorMachine{{ID: "m1", Name: "host", Approved: true, Connected: true,
		Supports: []protocol.DoctorSupport{{Flag: "supports_worktree", Supported: false, Degrades: "no worktrees"}}}}}
	var sawKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/doctor":
			sawKey = r.Header.Get("Authorization")
			if sawKey != "Bearer "+key {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			json.NewEncoder(w).Encode(answer)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	t.Setenv("AIMEBU2_BIND", "127.0.0.1")
	t.Setenv("AIMEBU2_PORT", strconv.Itoa(testServerPort(t, server.URL)))
	t.Setenv("AIMEBU2_ALLOW", "")
	t.Setenv("AIMEBU2_TLS_CERT", "")
	t.Setenv("AIMEBU2_TLS_KEY", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), ".codex"))
	run := func(args ...string) (int, doctorResult, string) {
		var stdout, stderr bytes.Buffer
		code := doctorCommand(context.Background(), args, &stdout, &stderr)
		var decoded doctorResult
		_ = json.Unmarshal(stdout.Bytes(), &decoded)
		return code, decoded, stdout.String() + stderr.String()
	}
	code, result, out := run("--live", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	scopes := map[string]string{}
	for _, check := range result.Checks {
		scopes[check.Name] = check.Scope
	}
	if scopes["doctor key"] != scopeLive || scopes["machine host"] != scopeLive || scopes["sqlite database"] != scopeServer {
		t.Fatalf("scopes: %v", scopes)
	}
	if sawKey != "Bearer "+key {
		t.Fatalf("the server saw %q", sawKey)
	}
	for _, check := range result.Checks {
		if check.Name == "machine host" && (check.Status != statusWarn || !strings.Contains(check.Description, "no worktrees")) {
			t.Fatalf("host: %+v", check)
		}
		if strings.Contains(check.Description+check.Hint, key) {
			t.Fatalf("the key leaked into %+v", check)
		}
	}
	// A stale key (the server answers 403) is a warning with a way out, and --strict then fails.
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("cd", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, result, out = run("--live", "--json")
	var live doctorCheck
	for _, check := range result.Checks {
		if check.Name == "live server" {
			live = check
		}
	}
	if code != 0 || live.Status != statusWarn || live.Hint == "" {
		t.Fatalf("stale key: exit %d %+v\n%s", code, live, out)
	}
	if code, _, _ := run("--live", "--strict"); code != 1 {
		t.Fatalf("--strict must fail on the warning: %d", code)
	}
	// Without --live the key is never read.
	sawKey = ""
	if _, _, _ = run("--json"); sawKey != "" {
		t.Fatal("doctor without --live asked /api/doctor")
	}
	// --live and --machine do not combine.
	if code, _, out := run("--live", "--machine"); code != 2 || !strings.Contains(out, "usage") {
		t.Fatalf("combined flags: %d %s", code, out)
	}
}

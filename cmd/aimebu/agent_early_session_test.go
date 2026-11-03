package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

// earlySessionServer is a bus that lists one registered agent (or none) and records the session pushes the wrapper makes.
type earlySessionServer struct {
	*httptest.Server
	mu      sync.Mutex
	agentID string // the registered agent; empty while nobody is registered
	pushes  []map[string]string
	pushed  chan struct{}
}

// setAgent registers (or, with "", deletes) the agent the bus lists.
func (s *earlySessionServer) setAgent(id string) {
	s.mu.Lock()
	s.agentID = id
	s.mu.Unlock()
}

func newEarlySessionServer(t *testing.T, spawnTag, agentID string) *earlySessionServer {
	t.Helper()
	s := &earlySessionServer{pushed: make(chan struct{}, 16), agentID: agentID}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/session"):
			body, _ := io.ReadAll(r.Body)
			var push map[string]string
			_ = json.Unmarshal(body, &push)
			push["agent"] = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/agents/"), "/session")
			s.mu.Lock()
			s.pushes = append(s.pushes, push)
			s.mu.Unlock()
			s.pushed <- struct{}{}
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/agents/by-spawn-tag" || r.URL.Path == "/agents":
			s.mu.Lock()
			agentID := s.agentID
			s.mu.Unlock()
			if agentID == "" {
				_, _ = w.Write([]byte(`{"agents":[]}`))
			} else if r.URL.Path == "/agents" {
				_, _ = fmt.Fprintf(w, `{"agents":[{"id":%q,"kind":"ai","meta":{"spawn_tag":%q}}]}`, agentID, spawnTag)
			} else {
				_, _ = fmt.Fprintf(w, `{"agent":{"id":%q,"kind":"ai","meta":{"spawn_tag":%q}}}`, agentID, spawnTag)
			}
		case strings.HasSuffix(r.URL.Path, "/rooms"):
			_, _ = w.Write([]byte(`{"agent":"x","rooms":[]}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *earlySessionServer) waitPush(t *testing.T) map[string]string {
	t.Helper()
	select {
	case <-s.pushed:
	case <-time.After(15 * time.Second):
		t.Fatal("the wrapper never pushed the session while the harness was still running")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pushes[len(s.pushes)-1]
}

func (s *earlySessionServer) pushCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pushes)
}

// A harness that keeps running until it is interrupted, after printing its own lines.
func writeLongRunningHarness(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-harness.sh")
	script := "#!/bin/sh\ntrap 'exit 0' INT TERM\n" + body + "\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitForHarnessArgs waits until the fake harness has written its argument list, a non-empty file ending in a newline.
func waitForHarnessArgs(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 && b[len(b)-1] == '\n' {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the harness never wrote its arguments to %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runInterruptedBootstrap(t *testing.T, harness, harnessPath, spawnTag string, server *earlySessionServer, wait func()) (string, error) {
	t.Helper()
	sigCh := make(chan os.Signal, 1)
	done := make(chan error, 1)
	var name string
	go func() {
		var err error
		_, name, err = agentBootstrapSession(harness, []string{harnessPath}, "register please", "", agentBuildEnv(server.URL, harness, spawnTag), server.URL, spawnTag, "", sigCh, nil,
			agentSessionSaver(nil, server.URL, harness, "", []string{"general"}, "", []string{harnessPath}))
		done <- err
	}()
	wait()
	sigCh <- os.Interrupt
	select {
	case err := <-done:
		return name, err
	case <-time.After(20 * time.Second):
		t.Fatal("bootstrap did not return after the interrupt")
		return "", nil
	}
}

// Claude's session id is known at spawn and the name at registration: the session is saved and pushed then, while the harness runs, and an
// interrupt afterwards loses nothing.
func TestAgentBootstrapPersistsClaudeSessionOnRegistration(t *testing.T) {
	t.Setenv("AIMEBU_CONFIG_DIR", t.TempDir())
	spawnTag := "claudeearly12345"
	server := newEarlySessionServer(t, spawnTag, "clive@aimebu")
	harnessPath := writeLongRunningHarness(t, `printf '%s\n' "$@" > "${0}.args"`)
	var push map[string]string
	// Claude's session id is known before the harness runs, so the push can land before the script has written its arguments; interrupt
	// only once both have happened, or a slow shell start (macOS) is killed before it records what it was spawned with.
	name, err := runInterruptedBootstrap(t, "claude-code", harnessPath, spawnTag, server, func() {
		push = server.waitPush(t)
		waitForHarnessArgs(t, harnessPath+".args")
	})
	if err != agentErrInterrupted || name != "clive@aimebu" {
		t.Fatalf("bootstrap = %q, %v, want the interrupt with the registered name", name, err)
	}
	args, err := os.ReadFile(harnessPath + ".args")
	if err != nil {
		t.Fatal(err)
	}
	var spawnedWith string
	lines := strings.Split(string(args), "\n")
	for i, line := range lines {
		if line == "--session-id" && i+1 < len(lines) {
			spawnedWith = lines[i+1]
		}
	}
	if spawnedWith == "" || push["harness_session_id"] != spawnedWith || push["agent"] != "clive@aimebu" {
		t.Fatalf("pushed %+v, want the session id the harness was spawned with (%q) for clive@aimebu", push, spawnedWith)
	}
	saved, err := agentLoadSessions()
	if err != nil || len(saved) != 1 || saved[0].SessionID != spawnedWith || saved[0].Name != "clive@aimebu" || saved[0].Harness != "claude-code" || len(saved[0].Rooms) != 1 {
		t.Fatalf("saved sessions = %+v, %v, want the one early entry", saved, err)
	}
}

// Codex prints its thread id while it runs; once the name is known too the session is saved, before the run ends and across an interrupt.
func TestAgentBootstrapPersistsCodexSessionOnFirstEvent(t *testing.T) {
	t.Setenv("AIMEBU_CONFIG_DIR", t.TempDir())
	old := agentEarlySessionPollInterval
	agentEarlySessionPollInterval = 10 * time.Millisecond
	defer func() { agentEarlySessionPollInterval = old }()
	spawnTag := "codexearly123456"
	server := newEarlySessionServer(t, spawnTag, "cody@aimebu")
	harnessPath := writeLongRunningHarness(t, `printf '{"type":"thread.started","thread_id":"thread-early"}\n'`)
	var push map[string]string
	name, err := runInterruptedBootstrap(t, "codex", harnessPath, spawnTag, server, func() { push = server.waitPush(t) })
	if err != agentErrInterrupted || name != "cody@aimebu" {
		t.Fatalf("bootstrap = %q, %v", name, err)
	}
	if push["harness_session_id"] != "thread-early" || push["agent"] != "cody@aimebu" {
		t.Fatalf("pushed %+v, want thread-early for cody@aimebu", push)
	}
	if saved, err := agentLoadSessions(); err != nil || len(saved) != 1 || saved[0].SessionID != "thread-early" {
		t.Fatalf("saved sessions = %+v, %v", saved, err)
	}
}

// With the id known but no registration there is no name to save it under: an interrupt then saves nothing, as before.
func TestAgentBootstrapInterruptWithoutRegistrationSavesNothing(t *testing.T) {
	t.Setenv("AIMEBU_CONFIG_DIR", t.TempDir())
	old := agentEarlySessionPollInterval
	agentEarlySessionPollInterval = 10 * time.Millisecond
	defer func() { agentEarlySessionPollInterval = old }()
	spawnTag := "noregister123456"
	server := newEarlySessionServer(t, spawnTag, "")
	harnessPath := writeLongRunningHarness(t, `printf '{"type":"thread.started","thread_id":"thread-orphan"}\n'`)
	_, err := runInterruptedBootstrap(t, "codex", harnessPath, spawnTag, server, func() { time.Sleep(300 * time.Millisecond) })
	if err != agentErrInterrupted {
		t.Fatalf("bootstrap error = %v", err)
	}
	if saved, _ := agentLoadSessions(); len(saved) != 0 || server.pushCount() != 0 {
		t.Fatalf("saved %+v and pushed %d sessions for an agent that never registered", saved, server.pushCount())
	}
}

// The early save and the one after a normal bootstrap are the same entry, and the pair is reported once however often it is learned.
func TestAgentEarlySessionIsIdempotent(t *testing.T) {
	var got []string
	early := &agentEarlySession{observe: func(id, name string) { got = append(got, id+"/"+name) }}
	early.update("s1", "")
	early.update("", "n1@p")
	early.update("s1", "n1@p")
	early.update("", "")
	early.update("s2", "")
	early.update("s2", "n1@p")
	if want := "s1/n1@p,s2/n1@p"; strings.Join(got, ",") != want {
		t.Fatalf("observer calls = %v, want %s", got, want)
	}
	(*agentEarlySession)(nil).update("s", "n") // no observer, and a nil receiver, are harmless

	t.Setenv("AIMEBU_CONFIG_DIR", t.TempDir())
	server := newEarlySessionServer(t, "x", "n1@p")
	save := agentSessionSaver(nil, server.URL, "codex", "", nil, "", []string{"codex"})
	save("thread-1", "n1@p")
	agentPersistSession(nil, server.URL, agentSession{SessionID: "thread-1", Name: "n1@p", Harness: "codex"}) // the normal save after bootstrap
	if saved, err := agentLoadSessions(); err != nil || len(saved) != 1 {
		t.Fatalf("saved sessions = %+v, %v, want one entry", saved, err)
	}
}

// The folder is pushed as the harness saw it: through a symlink it keeps the link's spelling, which harnesses key their sessions by.
func TestAgentSessionSaverKeepsSymlinkedCWDVerbatim(t *testing.T) {
	t.Setenv("AIMEBU_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real", "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "link", "proj")
	t.Chdir(linked) // sets PWD to the spelling given, as a shell does
	server := newEarlySessionServer(t, "x", "n1@p")
	agentSessionSaver(nil, server.URL, "claude-code", "", nil, "", []string{"claude"})("session-1", "n1@p")
	if push := server.waitPush(t); push["cwd"] != linked {
		t.Fatalf("pushed cwd = %q, want the symlinked spelling %q", push["cwd"], linked)
	}
	if saved, err := agentLoadSessions(); err != nil || len(saved) != 1 || saved[0].CWD != linked {
		t.Fatalf("saved = %+v, %v, want cwd %q", saved, err, linked)
	}
}

// A name given up front is a request, not a registration: a bootstrap that ends without the agent ever registering must save and push
// nothing, so a resumable entry the name already has is not replaced by a session that never ran.
func TestAgentBootstrapWithKnownNameSavesNothingWithoutRegistration(t *testing.T) {
	t.Setenv("AIMEBU_CONFIG_DIR", t.TempDir())
	old := agentEarlySessionPollInterval
	agentEarlySessionPollInterval = 10 * time.Millisecond
	defer func() { agentEarlySessionPollInterval = old }()
	existing := agentSession{Name: "known@aimebu", SessionID: "resumable-session", Harness: "claude-code", CWD: "/work", LastUsed: time.Now().UTC()}
	if err := agentSaveSession(existing); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ harness, body string }{
		{"claude-code", "exit 1"},
		{"codex", `printf '{"type":"thread.started","thread_id":"thread-never-registered"}\n'; sleep 0.3; exit 0`},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			spawnTag := "knownname" + tc.harness[:3] + "12345"
			server := newEarlySessionServer(t, spawnTag, "") // nobody ever registers
			path := filepath.Join(t.TempDir(), "fake-harness.sh")
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"+tc.body+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			_, _, err := agentBootstrapSession(tc.harness, []string{path}, "register please", "", agentBuildEnv(server.URL, tc.harness, spawnTag), server.URL, spawnTag, "known@aimebu",
				make(chan os.Signal, 1), nil, agentSessionSaver(nil, server.URL, tc.harness, "", nil, "", []string{path}))
			if err == nil {
				t.Fatal("the bootstrap succeeded without a registration")
			}
			saved, loadErr := agentLoadSessions()
			if loadErr != nil || len(saved) != 1 || saved[0].SessionID != "resumable-session" {
				t.Fatalf("saved sessions = %+v, %v, want the resumable entry untouched", saved, loadErr)
			}
			if server.pushCount() != 0 {
				t.Fatalf("pushed %d sessions for a bootstrap that never registered", server.pushCount())
			}
		})
	}
}

// --resume-name: the agent's registration was deleted when the previous wrapper stopped, so the session push made before the resumed harness
// started is refused, and the harness then registers anew. The wrapper pushes the session again once it sees that registration.
func TestAgentResumePushesTheSessionAfterTheHarnessRegisters(t *testing.T) {
	t.Setenv("AIMEBU_CONFIG_DIR", t.TempDir())
	spawnTag := "resumeregister12"
	server := newEarlySessionServer(t, spawnTag, "") // the registration is gone
	harnessPath := writeLongRunningHarness(t, `printf '{"type":"assistant"}\n'`)
	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		agentResumeLoop("claude-code", []string{harnessPath}, "resume-session", "clive@aimebu", []string{"general"}, "", "", agentBuildEnv(server.URL, "claude-code", spawnTag), server.URL, sigCh, nil)
	}()
	time.Sleep(300 * time.Millisecond)
	if server.pushCount() != 0 {
		t.Fatalf("pushed %d sessions before the harness registered", server.pushCount())
	}
	server.setAgent("clive@aimebu") // the resumed harness registers
	push := server.waitPush(t)
	if push["harness_session_id"] != "resume-session" || push["agent"] != "clive@aimebu" {
		t.Fatalf("pushed %+v, want resume-session for clive@aimebu", push)
	}
	sigCh <- os.Interrupt
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the resume loop did not return after the interrupt")
	}
}

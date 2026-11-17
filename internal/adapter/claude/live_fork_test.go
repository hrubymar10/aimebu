package claude

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
)

// TestLiveForkCanary runs a real first turn on a throwaway session, then a second that resumes it with --fork-session. The copy must get a
// session id of its own, know what the first turn was told, and leave the original transcript byte for byte as it was. It is opt-in
// (AIMEBU_LIVE_CLAUDE_FORK=1), native only (it needs a temporary Claude home, which a container would not use), and spends a little on the
// model. Claude Code runs with the test's temporary CLAUDE_CONFIG_DIR, so the person's own
// sessions are never read or written. Its login is ANTHROPIC_API_KEY, or else a copy of the file named by AIMEBU_LIVE_CLAUDE_CREDENTIALS
// (a login file without a refresh token, so that the copy cannot rotate the real one); the file itself is only read.
func TestLiveForkCanary(t *testing.T) {
	if os.Getenv("AIMEBU_LIVE_CLAUDE_FORK") != "1" {
		t.Skip("set AIMEBU_LIVE_CLAUDE_FORK=1 for the Claude Code fork canary")
	}
	config := os.Getenv("CLAUDE_CONFIG_DIR")
	if !strings.Contains(config, "aimebu-v2-test-home-") {
		t.Fatalf("CLAUDE_CONFIG_DIR %q is not the test's temporary one", config)
	}
	if source := os.Getenv("AIMEBU_LIVE_CLAUDE_CREDENTIALS"); source != "" {
		login, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Oauth struct {
				RefreshToken string `json:"refreshToken"`
			} `json:"claudeAiOauth"`
		}
		if err := json.Unmarshal(login, &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed.Oauth.RefreshToken != "" {
			t.Fatal("the login file has a refresh token: a copy could rotate the real login; use a token-only file or ANTHROPIC_API_KEY")
		}
		if err := os.MkdirAll(config, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(config, ".credentials.json"), login, 0600); err != nil {
			t.Fatal(err)
		}
	} else if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("set ANTHROPIC_API_KEY or AIMEBU_LIVE_CLAUDE_CREDENTIALS")
	}
	root := t.TempDir()
	tools := &adapter.AgentToolsAccess{AgentID: "agt_live_fork", ServerURL: "http://127.0.0.1:9897", Key: "aak_canary", Command: "/usr/bin/aimebu"}
	t.Setenv("AIMEBU_ALLOW_NATIVE", "1")
	implementation := New()
	implementation.AllowNative = true
	implementation.MaxBudgetUSD = "0.25"
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	nonce := "canary-" + hex.EncodeToString(random[:8])
	original := hex.EncodeToString(random[8:12]) + "-" + hex.EncodeToString(random[12:14]) + "-4" + hex.EncodeToString(random[14:16])[1:] + "-8" + hex.EncodeToString(random[16:18])[1:] + "-" + hex.EncodeToString(random[18:])
	run := func(id string, started, fork bool, body string) (answer, session string) {
		notes, _ := json.Marshal([]map[string]string{{"body": body}})
		turn, err := implementation.Start(ctx, adapter.TurnSpec{TurnID: id, Agent: adapter.AgentSpec{ID: "agt_live_fork", Variant: "native", Model: "haiku"}, CheckoutPath: root,
			AgentTools: tools, NativeSessionHandle: original, NativeSessionStarted: started, ForkSession: fork, Mode: adapter.ModeNormal, NotesJSON: notes,
			ContextJSON: json.RawMessage(`{"role_body":"Follow the user note exactly."}`)})
		if err != nil {
			t.Fatal(err)
		}
		for event := range turn.Events() {
			switch event.Kind {
			case adapter.EventFinalAnswer:
				answer = string(event.Answer)
			case adapter.EventNativeSessionStarted:
				session = event.NativeSessionHandle
			}
		}
		exit, err := turn.Wait()
		if err != nil || !exit.TerminalEvent {
			t.Fatalf("turn %s: exit=%+v err=%v", id, exit, err)
		}
		return answer, session
	}
	transcripts := func() map[string][]byte {
		out := map[string][]byte{}
		files, _ := filepath.Glob(filepath.Join(config, "projects", "*", "*.jsonl"))
		for _, file := range files {
			if data, err := os.ReadFile(file); err == nil {
				out[strings.TrimSuffix(filepath.Base(file), ".jsonl")] = data
			}
		}
		return out
	}
	run("canary-first", false, false, "Remember this nonce: "+nonce+". Return a valid empty messages response.")
	before := transcripts()
	if len(before[original]) == 0 {
		t.Fatalf("the first turn left no transcript for %s", original)
	}
	answer, copied := run("canary-fork", true, true, "What nonce did I ask you to remember? Return it in a valid message response.")
	if copied == "" || copied == original {
		t.Fatalf("the fork did not report a session id of its own: %q (original %s)", copied, original)
	}
	if !strings.Contains(answer, nonce) {
		t.Fatalf("the copy did not recall the nonce: %q", answer)
	}
	after := transcripts()
	if string(after[original]) != string(before[original]) {
		t.Fatalf("the fork changed the original transcript: %d bytes before, %d after", len(before[original]), len(after[original]))
	}
	if len(after[copied]) == 0 {
		t.Fatalf("the copy %s has no transcript of its own", copied)
	}
}

package opencode

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

// TestLiveDockerCanary runs two real turns through the opencode-docker launcher: the first hands the agent a nonce, the second resumes the
// session and must give it back. It is opt-in (AIMEBU2_LIVE_OPENCODE_DOCKER=1), spends a little on a real provider, and needs the launcher on PATH with a running
// harness container that mounts this repository. The provider key comes from the environment alone (the test home is a temporary one, so
// nothing is read from the person's own home), and the model from AIMEBU2_LIVE_OPENCODE_MODEL (default ollama-cloud/gemma4:31b). The canary checkout is below this
// repository's ignored tmp/. The toolbox endpoint is a dummy: no tool call is made.
func TestLiveDockerCanary(t *testing.T) {
	if os.Getenv("AIMEBU2_LIVE_OPENCODE_DOCKER") != "1" {
		t.Skip("set AIMEBU2_LIVE_OPENCODE_DOCKER=1 for the two-turn opencode Docker canary")
	}
	if os.Getenv("OLLAMA_API_KEY") == "" && os.Getenv("AIMEBU2_LIVE_OPENCODE_MODEL") == "" {
		t.Skip("set OLLAMA_API_KEY, or AIMEBU2_LIVE_OPENCODE_MODEL with the key of its provider")
	}
	model := os.Getenv("AIMEBU2_LIVE_OPENCODE_MODEL")
	if model == "" {
		model = "ollama-cloud/gemma4:31b"
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(cwd, "..", "..", "..", "tmp")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	checkout, err := os.MkdirTemp(base, "opencode-docker-canary-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(checkout) })
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	nonce := "canary-" + hex.EncodeToString(random[:])
	a := NewDocker()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run := func(id, handle string, started bool, body string) (answer, session string) {
		notes, _ := json.Marshal([]map[string]string{{"body": body}})
		turnSpec := spec(t, checkout)
		turnSpec.TurnID, turnSpec.Agent.Variant, turnSpec.Agent.Model = id, "docker", model
		turnSpec.NativeSessionHandle, turnSpec.NativeSessionStarted, turnSpec.NotesJSON = handle, started, notes
		turnSpec.ContextJSON = json.RawMessage(`{"role_body":"Follow the user note exactly."}`)
		turn, err := a.Start(ctx, turnSpec)
		if err != nil {
			t.Fatal(err)
		}
		usage := false
		for event := range turn.Events() {
			switch event.Kind {
			case adapter.EventFinalAnswer:
				answer = string(event.Answer)
			case adapter.EventNativeSessionStarted:
				session = event.NativeSessionHandle
			case adapter.EventUsage:
				usage = event.Usage != nil && event.Usage.TokensIn != nil
			}
		}
		exit, err := turn.Wait()
		if err != nil || !exit.TerminalEvent || answer == "" {
			t.Fatalf("turn %s: exit=%+v err=%v answer=%q", id, exit, err, answer)
		}
		if !usage {
			t.Fatalf("turn %s reported no token usage", id)
		}
		return answer, session
	}
	_, session := run("canary-first", "", false, "Remember this nonce for the next turn: "+nonce+". Return a valid empty messages response.")
	if session == "" {
		t.Fatal("the first turn did not report its session")
	}
	answer, _ := run("canary-second", session, true, "What nonce did I ask you to remember? Return it in a valid message response.")
	if !strings.Contains(answer, nonce) {
		t.Fatalf("the resumed turn did not recall the nonce: %q", answer)
	}
}

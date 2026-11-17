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
// session and must give it back. It is opt-in (AIMEBU_LIVE_OPENCODE_DOCKER=1), spends a little on a real provider, and needs the launcher on PATH with a running
// harness container that mounts this repository. The provider key comes from the environment alone (the test home is a temporary one, so
// nothing is read from the person's own home), and the model from AIMEBU_LIVE_OPENCODE_MODEL (default ollama-cloud/gemma4:31b). The canary checkout is below this
// repository's ignored tmp/. The toolbox endpoint is a dummy: no tool call is made.
func TestLiveDockerCanary(t *testing.T) {
	if os.Getenv("AIMEBU_LIVE_OPENCODE_DOCKER") != "1" {
		t.Skip("set AIMEBU_LIVE_OPENCODE_DOCKER=1 for the two-turn opencode Docker canary")
	}
	if os.Getenv("OLLAMA_API_KEY") == "" && os.Getenv("AIMEBU_LIVE_OPENCODE_MODEL") == "" {
		t.Skip("set OLLAMA_API_KEY, or AIMEBU_LIVE_OPENCODE_MODEL with the key of its provider")
	}
	model := os.Getenv("AIMEBU_LIVE_OPENCODE_MODEL")
	if model == "" {
		model = "ollama-cloud/gemma4:31b"
	}
	runLiveCanary(t, NewDocker(), "docker", model, nil)
}

// runLiveCanary runs the two real turns of the canaries: the first hands the agent a nonce, the second resumes the session and must give it
// back. providers, when set, are the custom providers the turns carry.
func runLiveCanary(t *testing.T, a *Adapter, variant, model string, providers []adapter.CustomProvider) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(cwd, "..", "..", "..", "tmp")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	checkout, err := os.MkdirTemp(base, "opencode-canary-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(checkout) })
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	nonce := "canary-" + hex.EncodeToString(random[:])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run := func(id, handle string, started bool, body string) (answer, session string) {
		notes, _ := json.Marshal([]map[string]string{{"body": body}})
		turnSpec := spec(t, checkout)
		turnSpec.TurnID, turnSpec.Agent.Variant, turnSpec.Agent.Model = id, variant, model
		turnSpec.Providers = providers
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

// TestLiveCustomProviderCanary is the two-turn canary for a custom provider: the turns carry an OpenAI-compatible endpoint the person names,
// run natively (or through the opencode-docker launcher with AIMEBU_LIVE_OPENCODE_PROVIDER_DOCKER=1), and the key reaches OpenCode only
// through its environment variable. It is opt-in (AIMEBU_LIVE_OPENCODE_PROVIDER=1), never part of the gate, and spends a little on the
// endpoint. AIMEBU_LIVE_OPENCODE_PROVIDER_URL is the base URL, AIMEBU_LIVE_OPENCODE_PROVIDER_MODEL the model id at that endpoint,
// AIMEBU_LIVE_OPENCODE_PROVIDER_KEY_ENV the name of the variable holding the key (default LIVE_OPENCODE_PROVIDER_KEY; a name the harness uses itself,
// such as anything starting with AIMEBU or OPENCODE_, is refused; unset variable: the endpoint takes no key). Natively the OpenCode on PATH is used, with a temporary private home.
func TestLiveCustomProviderCanary(t *testing.T) {
	if os.Getenv("AIMEBU_LIVE_OPENCODE_PROVIDER") != "1" {
		t.Skip("set AIMEBU_LIVE_OPENCODE_PROVIDER=1 for the two-turn custom provider canary")
	}
	baseURL, modelID := os.Getenv("AIMEBU_LIVE_OPENCODE_PROVIDER_URL"), os.Getenv("AIMEBU_LIVE_OPENCODE_PROVIDER_MODEL")
	if baseURL == "" || modelID == "" {
		t.Skip("set AIMEBU_LIVE_OPENCODE_PROVIDER_URL and AIMEBU_LIVE_OPENCODE_PROVIDER_MODEL")
	}
	keyEnv := os.Getenv("AIMEBU_LIVE_OPENCODE_PROVIDER_KEY_ENV")
	if keyEnv == "" {
		keyEnv = "LIVE_OPENCODE_PROVIDER_KEY"
	}
	if adapter.ProtectedKeyEnv(keyEnv) {
		t.Fatalf("AIMEBU_LIVE_OPENCODE_PROVIDER_KEY_ENV=%s names a variable the harness uses itself, which a provider's key cannot be read from: pick another name", keyEnv)
	}
	if os.Getenv(keyEnv) == "" {
		keyEnv = "" // the endpoint takes no key
	}
	provider := adapter.CustomProvider{ID: "canary-llm", Name: "Canary provider", NPM: adapter.CustomProviderNPM, BaseURL: baseURL, KeyEnv: keyEnv,
		Models: []adapter.CustomProviderModel{{ID: modelID, Name: modelID}}}
	a, variant := New(t.TempDir()), "native"
	if os.Getenv("AIMEBU_LIVE_OPENCODE_PROVIDER_DOCKER") == "1" {
		a, variant = NewDocker(), "docker"
	}
	runLiveCanary(t, a, variant, "canary-llm/"+modelID, []adapter.CustomProvider{provider})
}

package pi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// TestLivePiDockerToolbox runs one real Pi Docker turn (AIMEBU_LIVE_PI_DOCKER_TOOLBOX=1, model in AIMEBU_LIVE_PI_MODEL) against a stand-in MCP
// server on an ephemeral port that offers a run_command tool. It proves the turn finds its model in the container's own configuration AND
// loads the aimebu entry from the private directory: the server must see the bearer key, a tools/list and a tools/call for run_command.
func TestLivePiDockerToolbox(t *testing.T) {
	if os.Getenv("AIMEBU_LIVE_PI_DOCKER_TOOLBOX") != "1" {
		t.Skip("set AIMEBU_LIVE_PI_DOCKER_TOOLBOX=1 (and AIMEBU_LIVE_PI_MODEL) for the live Pi Docker toolbox canary")
	}
	model := os.Getenv("AIMEBU_LIVE_PI_MODEL")
	if model == "" {
		t.Fatal("AIMEBU_LIVE_PI_MODEL is required")
	}
	const key = "aak_livecanary"
	var mu sync.Mutex
	var seen []string
	badAuth := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/mcp", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		dec := json.NewDecoder(r.Body)
		_ = dec.Decode(&req)
		mu.Lock()
		if r.Header.Get("Authorization") != "Bearer "+key {
			badAuth = true
		}
		seen = append(seen, req.Method)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"aimebu","version":"canary"}}}`, req.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"run_command","description":"Run a command and report its output.","inputSchema":{"type":"object","properties":{"cmd":{"type":"string"},"request_key":{"type":"string"}},"required":["cmd","request_key"]}}]}}`, req.ID)
		case "tools/call":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"canary-ran"}]}}`, req.ID)
		case "ping":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, req.ID)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	port := listener.Addr().(*net.TCPAddr).Port

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(cwd, "..", "..", "..", "tmp")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	checkout, err := os.MkdirTemp(base, "pi-docker-toolbox-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(checkout) })
	access := &adapter.AgentToolsAccess{AgentID: "agt_live_pi", ServerURL: fmt.Sprintf("http://127.0.0.1:%d", port), Key: key, Command: "/usr/bin/aimebu"}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	notes, _ := json.Marshal([]map[string]string{{"body": "List the names of the MCP tools you can call (mention run_command if you have it), then call run_command with cmd \"echo canary\" and request_key \"canary-1\" and tell me its output."}})
	// Where the container reaches this stand-in server: host.docker.internal by default; AIMEBU_LIVE_PI_VISIBLE_HOST names an address of this
	// machine when the container's host gateway is not it (a launcher in a VM).
	visible := ""
	if host := os.Getenv("AIMEBU_LIVE_PI_VISIBLE_HOST"); host != "" {
		visible = fmt.Sprintf("http://%s:%d", host, port)
	}
	turn, err := NewDocker().Start(ctx, adapter.TurnSpec{TurnID: "toolbox", AgentServerURL: visible, StderrSink: stderrLog{t}, Agent: adapter.AgentSpec{ID: "agt_live_pi", Variant: "docker", Model: model},
		CheckoutPath: checkout, AgentTools: access, NativeSessionHandle: testSession, Mode: adapter.ModeNormal, NotesJSON: notes,
		ContextJSON: json.RawMessage(`{"rooms":[{"room_id":"canary"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	answer := ""
	for event := range turn.Events() {
		t.Logf("event %v answer=%s tools=%+v", event.Kind, event.Answer, event.Tools)
		if event.Kind == adapter.EventFinalAnswer {
			answer = string(event.Answer)
		}
	}
	exit, err := turn.Wait()
	t.Logf("exit=%+v err=%v", exit, err)
	if err != nil || exit.ExitCode != 0 || !exit.TerminalEvent {
		t.Errorf("the turn must end cleanly: exit=%+v err=%v", exit, err)
	}
	if strings.TrimSpace(answer) == "" {
		t.Error("the turn gave no final answer")
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("MCP methods the stand-in server saw: %v", seen)
	if badAuth {
		t.Fatal("a request reached the server without the bearer key")
	}
	if !strings.Contains(strings.Join(seen, ","), "tools/list") {
		t.Fatalf("Pi never listed the aimebu tools: %v", seen)
	}
	if !strings.Contains(strings.Join(seen, ","), "tools/call") {
		t.Fatalf("Pi never called run_command: %v", seen)
	}
}

// stderrLog shows everything the launcher and Pi write to stderr during the canary.
type stderrLog struct{ t *testing.T }

func (l stderrLog) Write(p []byte) (int, error) {
	l.t.Logf("stderr: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// Package live runs the live scenarios of internal/eval on real models, through the same launchers agents run through, and scores them. It spends
// subscription quota: nothing here runs unless the caller asks for it (cmd/evalrun refuses without AIMEBU_EVAL=1).
package live

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

// MCPServer is the stand-in for the aimebu toolbox a turn reaches at /api/agent/mcp. It is not the real server: it holds no state, answers every
// tool call with a canned text, and records which tools each turn called (the scenarios' tool_called checks read that). Each turn has a bearer key
// of its own and sees the tools registered for that key, so the model's toolbox is the scenario's.
type MCPServer struct {
	listener net.Listener
	server   *http.Server
	mu       sync.Mutex
	tools    map[string][]string // bearer key -> tool names offered
	calls    map[string][]string // bearer key -> tool names called, in order
}

// StartMCP listens on an ephemeral port on every interface, since a container reaches the host through its gateway address, not loopback.
func StartMCP() (*MCPServer, error) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}
	m := &MCPServer{listener: listener, tools: map[string][]string{}, calls: map[string][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/mcp", m.handle)
	m.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = m.server.Serve(listener) }()
	return m, nil
}

// Port is the port the server listens on.
func (m *MCPServer) Port() int { return m.listener.Addr().(*net.TCPAddr).Port }

// Close stops the server.
func (m *MCPServer) Close() { _ = m.server.Close() }

// Register offers tools to the turn that holds key.
func (m *MCPServer) Register(key string, tools []string) {
	m.mu.Lock()
	m.tools[key] = append([]string(nil), tools...)
	m.mu.Unlock()
}

// Calls lists the tools the turn that held key called.
func (m *MCPServer) Calls(key string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls[key]...)
}

var toolDescriptions = map[string]string{
	"react":          "Add a reaction (an emoji) to a message instead of sending a message.",
	"run_command":    "Start a long command outside the turn; it wakes you with its exit code when it ends.",
	"dm_agent":       "Send a direct message to another agent.",
	"message_get":    "Read one message of your project by its number.",
	"room_history":   "Read the recent messages of a room.",
	"attachment_get": "Read an attachment of a note by its id.",
}

var toolReplies = map[string]string{
	"react":          "Reaction added.",
	"run_command":    "Started as c_1. You are woken with its exit code when it ends.",
	"dm_agent":       "Sent.",
	"message_get":    "No such message.",
	"room_history":   "No messages.",
	"attachment_get": "No such attachment.",
}

func (m *MCPServer) handle(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	m.mu.Lock()
	offered, known := m.tools[key]
	m.mu.Unlock()
	if !known || key == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	id := string(req.ID)
	if id == "" {
		id = "null"
	}
	switch req.Method {
	case "initialize":
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"aimebu-eval","version":"1"}}}`, id)
	case "tools/list":
		var list []map[string]any
		for _, name := range offered {
			list = append(list, map[string]any{"name": name, "description": toolDescriptions[name],
				"inputSchema": map[string]any{"type": "object", "additionalProperties": true}})
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": map[string]any{"tools": list}})
		_, _ = w.Write(out)
	case "tools/call":
		name := req.Params.Name
		m.mu.Lock()
		m.calls[key] = append(m.calls[key], name)
		m.mu.Unlock()
		reply, ok := toolReplies[name]
		if !ok {
			reply = "Done."
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": map[string]any{"content": []map[string]string{{"type": "text", "text": reply}}}})
		_, _ = w.Write(out)
	case "ping":
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, id)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

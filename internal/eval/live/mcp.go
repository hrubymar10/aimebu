// Package live runs the live scenarios of internal/eval on real models, through the same launchers agents run through, and scores them. It spends
// subscription quota: nothing here runs unless the caller asks for it (cmd/evalrun refuses without AIMEBU_EVAL=1).
package live

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/agenttools"
	"github.com/hrubymar10/aimebu/v2/internal/eval"
)

// MCPServer is the stand-in for the aimebu toolbox a turn reaches at /api/agent/mcp. It is not the real server: it holds no state, answers every
// tool call with a canned text, and records which tools each turn called (the scenarios' tool_called checks read that). Each turn has a bearer key
// of its own and sees the tools registered for that key, so the model's toolbox is the scenario's.
type MCPServer struct {
	listener net.Listener
	server   *http.Server
	mu       sync.Mutex
	tools    map[string][]string              // bearer key -> tool names offered
	calls    map[string][]string              // bearer key -> tool names called, in order
	commands map[string][]string              // bearer key -> the cmd of each run_command call, in order
	history  map[string][]eval.HistoryMessage // bearer key -> the messages room_history and message_get know
	answers  map[string][]byte                // bearer key -> the last answer submit_answer accepted, compacted
}

// StartMCP listens on an ephemeral port on every interface, since a container reaches the host through its gateway address, not loopback.
func StartMCP() (*MCPServer, error) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}
	m := &MCPServer{listener: listener, tools: map[string][]string{}, calls: map[string][]string{}, commands: map[string][]string{}, history: map[string][]eval.HistoryMessage{}, answers: map[string][]byte{}}
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

// SetHistory gives the turn that holds key the messages its room_history and message_get answer from (eval.Input.Messages): the situation the
// scenario describes, so a model that looks for the brief or the thread finds it.
func (m *MCPServer) SetHistory(key string, messages []eval.HistoryMessage) {
	m.mu.Lock()
	m.history[key] = append([]eval.HistoryMessage(nil), messages...)
	m.mu.Unlock()
}

// Submitted is the last answer the turn that held key gave through submit_answer and the toolbox accepted (the same check production applies), compacted;
// nil when it gave none.
func (m *MCPServer) Submitted(key string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.answers[key]...)
}

// Commands lists the commands the turn that held key started with run_command, in order.
func (m *MCPServer) Commands(key string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.commands[key]...)
}

// Calls lists the tools the turn that held key called.
func (m *MCPServer) Calls(key string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls[key]...)
}

var toolReplies = map[string]string{
	"react":          "Reaction added.",
	"run_command":    "Started as c_1. You are woken with its exit code when it ends.",
	"dm_agent":       "Sent.",
	"message_get":    "Message not available.",
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
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	// The connection asks for submit_answer with the same query production reads (internal/server/agent_mcp.go): the adapter adds it only for a
	// turn that OffersAnswerTool (internal/adapter/opencode/opencode.go, pi/pi.go), so the stand-in offers the tool exactly where production does.
	answerTool := r.URL.Query().Get(agenttools.AnswerToolQuery) == "1"
	w.Header().Set("Content-Type", "application/json")
	id := string(req.ID)
	if id == "" {
		id = "null"
	}
	switch req.Method {
	case "initialize":
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"aimebu-eval","version":"1"}}}`, id)
	case "tools/list":
		// Production's own list, descriptions and input schemas, narrowed to the scenario's tools; submit_answer is in it when the connection asked.
		list := []agenttools.Definition{} // an empty toolbox is `"tools":[]`, as the real toolbox answers, never null
		for _, def := range agenttools.Definitions(answerTool) {
			if def.Name == "submit_answer" || contains(offered, def.Name) {
				list = append(list, def)
			}
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": map[string]any{"tools": list}})
		_, _ = w.Write(out)
	case "tools/call":
		name := req.Params.Name
		m.mu.Lock()
		m.calls[key] = append(m.calls[key], name)
		if name == "run_command" {
			var cmd string
			_ = json.Unmarshal(req.Params.Arguments["cmd"], &cmd)
			m.commands[key] = append(m.commands[key], cmd)
		}
		m.mu.Unlock()
		if name == "submit_answer" {
			// Production's own handler answers: the same validator, "unknown tool" when the connection did not ask for the tool.
			response, _ := agenttools.Server{AnswerTool: answerTool}.Process(r.Context(), raw)
			var result struct {
				Result struct {
					IsError bool `json:"isError"`
				} `json:"result"`
			}
			if json.Unmarshal(response, &result) == nil && !result.Result.IsError {
				var compact bytes.Buffer
				if json.Compact(&compact, rawArguments(raw)) == nil {
					m.mu.Lock()
					m.answers[key] = compact.Bytes()
					m.mu.Unlock()
				}
			}
			_, _ = w.Write(response)
			return
		}
		reply, ok := toolReplies[name]
		if !ok {
			reply = "Done."
		}
		m.mu.Lock()
		messages := m.history[key]
		m.mu.Unlock()
		switch name {
		case "room_history":
			reply = roomHistoryReply(messages, req.Params.Arguments)
		case "message_get":
			reply = messageGetReply(messages, req.Params.Arguments, reply)
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": map[string]any{"content": []map[string]string{{"type": "text", "text": reply}}}})
		_, _ = w.Write(out)
	case "ping":
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, id)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

// roomHistoryReply is the room's messages, newest last, as JSON: the same fields a model reads from the real tool that matter to a scenario
// (number, sender, text). before_no and limit page as the real tool does. A room with no messages is an empty list.
func roomHistoryReply(messages []eval.HistoryMessage, args map[string]json.RawMessage) string {
	var room string
	_ = json.Unmarshal(args["room_id"], &room)
	var before, limit int64
	_ = json.Unmarshal(args["before_no"], &before)
	_ = json.Unmarshal(args["limit"], &limit)
	picked := []map[string]any{}
	for _, msg := range messages {
		if msg.Room == room && (before < 1 || msg.No < before) {
			picked = append(picked, map[string]any{"no": msg.No, "from": msg.From, "body": msg.Body})
		}
	}
	if limit > 0 && int64(len(picked)) > limit {
		picked = picked[int64(len(picked))-limit:]
	}
	out, _ := json.Marshal(map[string]any{"room_id": room, "messages": picked})
	return string(out)
}

// messageGetReply is one message by number, or the notAvailable text the real tool gives for a number that does not exist.
func messageGetReply(messages []eval.HistoryMessage, args map[string]json.RawMessage, notAvailable string) string {
	var no int64
	if json.Unmarshal(args["no"], &no) != nil {
		var text string
		_ = json.Unmarshal(args["no"], &text)
		no, _ = strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(text), "#"), 10, 64)
	}
	for _, msg := range messages {
		if msg.No == no {
			out, _ := json.Marshal(map[string]any{"no": msg.No, "room_id": msg.Room, "from": msg.From, "body": msg.Body})
			return string(out)
		}
	}
	return notAvailable
}

// rawArguments is the arguments object of a tools/call request.
func rawArguments(raw []byte) []byte {
	var req struct {
		Params struct {
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(raw, &req)
	return req.Params.Arguments
}

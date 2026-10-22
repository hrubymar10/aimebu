// Package agenttools exposes the supervisor's narrow agent toolbox over stdio MCP.
package agenttools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

type Server struct {
	URL    string
	Key    string
	Client *http.Client
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var empty = map[string]any{"type": "object", "properties": map[string]any{}}
var agentIDPattern = regexp.MustCompile(`^agt_[A-Za-z0-9_-]+$`)
var catalog = []tool{
	{"list_rooms", "List rooms where you are an active member, including archived rooms, without message previews.", empty},
	{"list_macros", "Read the shared composer macro key-to-text map. Macros are expanded only when a human selects one in the web composer.", empty},
	{"list_schedules", "List schedules in a joined task room or the project home room, including their next run and last result.", map[string]any{"type": "object", "properties": map[string]any{"room_id": map[string]string{"type": "string"}}, "required": []string{"room_id"}}},
	{"create_schedule", "Active lead only. Schedule a note to a room agent or the current lead. Kinds: once (at RFC3339), interval (at least 300 seconds), daily (HH:MM), weekly (HH:MM and weekday 0=Sunday). Timezone is an IANA zone. Use a stable request_key.", map[string]any{"type": "object", "properties": map[string]any{
		"room_id": map[string]string{"type": "string"}, "target_agent_id": map[string]string{"type": "string"}, "target_lead": map[string]string{"type": "boolean"}, "body": map[string]string{"type": "string"}, "kind": map[string]string{"type": "string"}, "timezone": map[string]string{"type": "string"}, "interval_seconds": map[string]string{"type": "integer"}, "local_time": map[string]string{"type": "string"}, "weekday": map[string]string{"type": "integer"}, "at": map[string]string{"type": "string"}, "request_key": map[string]string{"type": "string"},
	}, "required": []string{"room_id", "body", "kind", "timezone", "request_key"}}},
	{"usage_left", "Show remaining project agent slots, spawn rate capacity, and available provider quota.", empty},
	{"create_room", "Active lead only. Create a task room in your project. Omit parent_id to place it under the project home room. Join the room and announce it in the parent room. Use a stable request_key when retrying.", map[string]any{"type": "object", "properties": map[string]any{
		"title": map[string]string{"type": "string"}, "parent_id": map[string]string{"type": "string"},
		"worktree":    map[string]string{"type": "boolean", "description": "Create a Git worktree for this task room"},
		"base_ref":    map[string]string{"type": "string", "description": "Committed base branch or ref; defaults to current HEAD"},
		"request_key": map[string]string{"type": "string"},
	}, "required": []string{"title", "request_key"}}},
	{"close_room", "Active lead only. Archive a task room you belong to using its lifecycle_rev; closure includes the normal grace period. Close children first or set cascade. Use a stable request_key when retrying.", map[string]any{"type": "object", "properties": map[string]any{
		"room_id": map[string]string{"type": "string"}, "lifecycle_rev": map[string]string{"type": "integer"},
		"cascade": map[string]string{"type": "boolean"}, "request_key": map[string]string{"type": "string"},
	}, "required": []string{"room_id", "lifecycle_rev", "request_key"}}},
	{"memory_add", "Add a curated project fact, global user profile or shared agent note when project memory is enabled, or a note to a joined task room.", map[string]any{"type": "object", "properties": map[string]any{
		"scope":             map[string]any{"type": "string", "enum": []string{"project", "task", "user_profile", "agent_shared_notes"}},
		"room_id":           map[string]string{"type": "string", "description": "Required for task notes"},
		"scope_key":         map[string]string{"type": "string", "description": "User ID for user_profile; global for agent_shared_notes"},
		"body":              map[string]string{"type": "string"},
		"source_message_id": map[string]string{"type": "string", "description": "Optional for project/global records: message in a joined room with memory enabled"},
	}, "required": []string{"scope", "body"}}},
	{"memory_list", "List curated records in one scope. Global results include active records only.", map[string]any{"type": "object", "properties": map[string]any{
		"scope":   map[string]any{"type": "string", "enum": []string{"project", "task", "user_profile", "agent_shared_notes"}},
		"room_id": map[string]string{"type": "string"}, "scope_key": map[string]string{"type": "string"}}, "required": []string{"scope"}}},
	{"memory_update", "Revise one curated record using its current revision.", map[string]any{"type": "object", "properties": map[string]any{
		"scope": map[string]string{"type": "string"}, "room_id": map[string]string{"type": "string"}, "scope_key": map[string]string{"type": "string"},
		"id": map[string]string{"type": "string"}, "rev": map[string]string{"type": "integer"}, "body": map[string]string{"type": "string"}}, "required": []string{"scope", "id", "rev", "body"}}},
	{"memory_remove", "Delete one curated record using its current revision.", map[string]any{"type": "object", "properties": map[string]any{
		"scope": map[string]string{"type": "string"}, "room_id": map[string]string{"type": "string"}, "scope_key": map[string]string{"type": "string"},
		"id": map[string]string{"type": "string"}, "rev": map[string]string{"type": "integer"}}, "required": []string{"scope", "id", "rev"}}},
	{"memory_recall", "Search curated project memory, task notes in joined rooms, and messages in joined rooms. Does not advance read state.", map[string]any{"type": "object", "properties": map[string]any{
		"query": map[string]string{"type": "string"}, "limit": map[string]string{"type": "integer"},
	}, "required": []string{"query"}}},
	{"attachment_get", "Read a file attached to a note in your current turn while you remain a member of its room.", map[string]any{"type": "object", "properties": map[string]any{
		"attachment_id": map[string]string{"type": "string"},
	}, "required": []string{"attachment_id"}}},
	{"spawn_agent", "Active lead only. Spawn one non-lead agent. Supply role_key and omit tool, variant, and model to use the next available role default. Give a concrete reason when choosing a different harness or model. Use a stable request_key when retrying.", map[string]any{"type": "object", "properties": map[string]any{
		"room_id": map[string]string{"type": "string"}, "role_key": map[string]string{"type": "string"},
		"tool": map[string]string{"type": "string"}, "variant": map[string]string{"type": "string"},
		"model": map[string]string{"type": "string"}, "provider": map[string]string{"type": "string"},
		"reason":      map[string]string{"type": "string", "description": "Required when an explicit choice differs from an available role default"},
		"machine_id":  map[string]string{"type": "string", "description": "Optional target machine; defaults to the host"},
		"worktree":    map[string]string{"type": "boolean", "description": "Create or use this task room's Git worktree"},
		"base_ref":    map[string]string{"type": "string", "description": "Committed base ref for a newly created worktree"},
		"request_key": map[string]string{"type": "string", "description": "Stable idempotency key for this spawn request"},
	}, "required": []string{"room_id", "request_key", "role_key"}}},
	{"stop_agent", "Active lead only. Stop a non-lead agent in your project, or yourself, using the current desired_rev. Use a stable request_key when retrying.", map[string]any{"type": "object", "properties": map[string]any{
		"agent_id": map[string]string{"type": "string"}, "rev": map[string]string{"type": "integer"},
		"request_key": map[string]string{"type": "string", "description": "Stable idempotency key for this stop request"},
	}, "required": []string{"agent_id", "rev", "request_key"}}},
	{"message_get", "Read a message by its number (the #N people write), from any room of your own project. A number from another project, or one that does not exist, is reported as not available.", map[string]any{"type": "object", "properties": map[string]any{
		"no": map[string]string{"type": "integer", "description": "The message number, without the #"},
	}, "required": []string{"no"}}},
	{"react", "Add or remove a single-emoji reaction on an existing message.", map[string]any{"type": "object", "properties": map[string]any{
		"message_id": map[string]string{"type": "string"}, "emoji": map[string]string{"type": "string"}, "remove": map[string]string{"type": "boolean"},
	}, "required": []string{"message_id", "emoji"}}},
}

// Serve uses newline-delimited JSON-RPC, the stdio transport used by MCP.
func (s Server) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	writer := bufio.NewWriter(output)
	for scanner.Scan() {
		response, notification := s.Process(ctx, scanner.Bytes())
		if notification {
			continue
		}
		if _, err := writer.Write(append(response, '\n')); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// Process handles one JSON-RPC request for either stdio or HTTP transport.
func (s Server) Process(ctx context.Context, raw []byte) ([]byte, bool) {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcResponse(nil, nil, map[string]any{"code": -32700, "message": "invalid JSON"}), false
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return nil, true
	}
	var result any
	var rpcError any
	switch req.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "aimebu-agent-tools", "version": "2"}, "instructions": "These tools manage your project agents. Room messaging is provided separately by your turn context and final answer. Final-answer messages may include display-only visual_plan blocks and collapsed Markdown appendix_pages; they do not create a durable plan. Unknown block shapes remain visible as raw text."}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		result = map[string]any{"tools": catalog}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			rpcError = map[string]any{"code": -32602, "message": "invalid tool call"}
		} else {
			result = s.call(ctx, params.Name, params.Arguments)
		}
	default:
		rpcError = map[string]any{"code": -32601, "message": "method not found"}
	}
	return rpcResponse(req.ID, result, rpcError), false
}

func rpcResponse(id, result, rpcError any) []byte {
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcError == nil {
		response["result"] = result
	} else {
		response["error"] = rpcError
	}
	data, _ := json.Marshal(response)
	return data
}

func writeRPC(w *bufio.Writer, id, result, rpcError any) error {
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcError == nil {
		response["result"] = result
	} else {
		response["error"] = rpcError
	}
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return err
	}
	return w.Flush()
}

func toolResult(body string, failed bool) any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": body}}, "isError": failed}
}

func (s Server) call(ctx context.Context, name string, raw json.RawMessage) any {
	var args map[string]json.RawMessage
	if len(raw) > 0 && json.Unmarshal(raw, &args) != nil {
		return toolResult("invalid arguments", true)
	}
	method, path := http.MethodGet, ""
	var body any
	var requestKey string
	getString := func(field string) string {
		var value string
		_ = json.Unmarshal(args[field], &value)
		return value
	}
	getBool := func(field string) bool { var value bool; _ = json.Unmarshal(args[field], &value); return value }
	switch name {
	case "list_rooms":
		path = "/api/agent/rooms"
	case "list_macros":
		path = "/api/agent/macros"
	case "list_schedules":
		roomID := getString("room_id")
		if !strings.HasPrefix(roomID, "room_") {
			return toolResult("invalid room_id", true)
		}
		path = "/api/agent/rooms/" + roomID + "/schedules"
	case "create_schedule":
		roomID := getString("room_id")
		if !strings.HasPrefix(roomID, "room_") {
			return toolResult("invalid room_id", true)
		}
		method, path = http.MethodPost, "/api/agent/rooms/"+roomID+"/schedules"
		requestKey = getString("request_key")
		var input map[string]any
		if err := json.Unmarshal(raw, &input); err != nil {
			return toolResult("invalid arguments", true)
		}
		delete(input, "room_id")
		delete(input, "request_key")
		body = input
	case "usage_left":
		path = "/api/agent/usage-left"
	case "create_room":
		method, path = http.MethodPost, "/api/agent/rooms"
		requestKey = getString("request_key")
		title := strings.TrimSpace(getString("title"))
		if title == "" {
			return toolResult("title is required", true)
		}
		body = map[string]any{"title": title, "parent_id": getString("parent_id"),
			"worktree": map[string]any{"enabled": getBool("worktree"), "base_ref": getString("base_ref")}}
	case "close_room":
		roomID := getString("room_id")
		if !strings.HasPrefix(roomID, "room_") {
			return toolResult("invalid room_id", true)
		}
		var rev int64
		if json.Unmarshal(args["lifecycle_rev"], &rev) != nil {
			return toolResult("lifecycle_rev must be an integer", true)
		}
		var cascade bool
		if len(args["cascade"]) > 0 && json.Unmarshal(args["cascade"], &cascade) != nil {
			return toolResult("cascade must be a boolean", true)
		}
		method, path = http.MethodPost, "/api/agent/rooms/"+roomID+"/close"
		requestKey = getString("request_key")
		body = map[string]any{"lifecycle_rev": rev, "cascade": cascade}
	case "memory_add":
		method, path = http.MethodPost, "/api/agent/memory"
		scope, roomID, scopeKey, content := getString("scope"), getString("room_id"), getString("scope_key"), getString("body")
		if scope != "project" && scope != "task" && scope != "user_profile" && scope != "agent_shared_notes" ||
			scope == "task" && roomID == "" || scope == "user_profile" && scopeKey == "" ||
			scope == "agent_shared_notes" && scopeKey != "global" || strings.TrimSpace(content) == "" {
			return toolResult("scope must be project, task, user_profile, or agent_shared_notes; required scope key or room ID is missing", true)
		}
		body = map[string]string{"scope": scope, "room_id": roomID, "scope_key": scopeKey, "body": content, "source_message_id": getString("source_message_id")}
	case "memory_list", "memory_update", "memory_remove":
		scope, roomID, scopeKey := getString("scope"), getString("room_id"), getString("scope_key")
		if scope != "project" && scope != "task" && scope != "user_profile" && scope != "agent_shared_notes" || scope == "task" && !strings.HasPrefix(roomID, "room_") || scope == "user_profile" && scopeKey == "" || scope == "agent_shared_notes" && scopeKey != "global" {
			return toolResult("invalid memory scope or scope key", true)
		}
		values := url.Values{"scope": []string{scope}}
		if roomID != "" {
			values.Set("room_id", roomID)
		}
		if scopeKey != "" {
			values.Set("scope_key", scopeKey)
		}
		path = "/api/agent/memory?" + values.Encode()
		if name != "memory_list" {
			id := getString("id")
			var rev int64
			if !strings.HasPrefix(id, "knw_") || json.Unmarshal(args["rev"], &rev) != nil || rev < 1 {
				return toolResult("valid id and rev required", true)
			}
			path = "/api/agent/memory/" + url.PathEscape(id) + "?" + values.Encode()
			if name == "memory_update" {
				if strings.TrimSpace(getString("body")) == "" {
					return toolResult("body required", true)
				}
				method, body = http.MethodPut, map[string]any{"rev": rev, "body": getString("body")}
			} else {
				method = http.MethodDelete
				path += "&rev=" + fmt.Sprint(rev)
			}
		}
	case "memory_recall":
		query := getString("query")
		if strings.TrimSpace(query) == "" {
			return toolResult("query is required", true)
		}
		values := url.Values{"query": []string{query}}
		var limit int
		if len(args["limit"]) > 0 {
			if json.Unmarshal(args["limit"], &limit) != nil {
				return toolResult("limit must be an integer", true)
			}
			values.Set("limit", fmt.Sprint(limit))
		}
		path = "/api/agent/recall?" + values.Encode()
	case "message_get":
		var no int64
		if json.Unmarshal(args["no"], &no) != nil || no < 1 {
			if text := strings.TrimPrefix(strings.TrimSpace(getString("no")), "#"); text != "" {
				parsed, err := strconv.ParseInt(text, 10, 64)
				if err != nil || parsed < 1 {
					return toolResult("no must be a positive message number", true)
				}
				no = parsed
			} else {
				return toolResult("no must be a positive message number", true)
			}
		}
		path = "/api/agent/message-numbers/" + strconv.FormatInt(no, 10)
	case "attachment_get":
		id := getString("attachment_id")
		if !strings.HasPrefix(id, "att_") || len(id) > 100 {
			return toolResult("invalid attachment_id", true)
		}
		path = "/api/agent/attachments/" + url.PathEscape(id)
	case "spawn_agent":
		method, path = http.MethodPost, "/api/agent/agents"
		requestKey = getString("request_key")
		roomID := getString("room_id")
		machineID := getString("machine_id")
		role, toolName, variant, model, provider := getString("role_key"), getString("tool"), getString("variant"), getString("model"), getString("provider")
		if roomID == "" {
			return toolResult("room_id is required", true)
		}
		if role == "" {
			return toolResult("role_key required", true)
		}
		body = map[string]any{"room_id": roomID, "role_key": role, "tool": toolName, "variant": variant, "model": model, "provider": provider, "machine_id": machineID, "reason": getString("reason"), "worktree": getBool("worktree"), "base_ref": getString("base_ref")}
	case "stop_agent":
		agentID := getString("agent_id")
		if !agentIDPattern.MatchString(agentID) {
			return toolResult("invalid agent_id", true)
		}
		method, path = http.MethodPost, "/api/agent/agents/"+agentID+"/stop"
		requestKey = getString("request_key")
		var revision int64
		if json.Unmarshal(args["rev"], &revision) != nil {
			return toolResult("rev must be an integer", true)
		}
		body = map[string]any{"rev": revision}
	case "react":
		messageID := getString("message_id")
		emoji := getString("emoji")
		if messageID == "" || emoji == "" {
			return toolResult("message_id and emoji are required", true)
		}
		method = http.MethodPut
		if getBool("remove") {
			method = http.MethodDelete
		}
		path = "/api/agent/messages/" + url.PathEscape(messageID) + "/reactions/" + url.PathEscape(emoji)
	default:
		return toolResult("unknown tool", true)
	}
	if method == http.MethodPost && name != "memory_add" && (requestKey == "" || len(requestKey) > 200) {
		return toolResult("request_key is required (at most 200 characters)", true)
	}
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return toolResult(err.Error(), true)
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.URL, "/")+path, input)
	if err != nil {
		return toolResult(err.Error(), true)
	}
	req.Header.Set("Authorization", "Bearer "+s.Key)
	if method == http.MethodPost || method == http.MethodPut {
		req.Header.Set("Content-Type", "application/json")
		if method == http.MethodPost {
			req.Header.Set("Idempotency-Key", requestKey)
		}
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return toolResult(err.Error(), true)
	}
	defer resp.Body.Close()
	limit := int64(1 << 20)
	if name == "attachment_get" {
		limit = 10<<20 + 1
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return toolResult(err.Error(), true)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return toolResult(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, data), true)
	}
	if name == "attachment_get" {
		if len(data) > 10<<20 {
			return toolResult("attachment exceeds 10 MB", true)
		}
		contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		switch contentType {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			return map[string]any{"content": []any{map[string]string{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": contentType}}, "isError": false}
		}
		const maxInlineText = 64 << 10
		if len(data) <= maxInlineText && utf8.Valid(data) && (strings.HasPrefix(contentType, "text/") || contentType == "application/json" || contentType == "application/xml" || contentType == "image/svg+xml") {
			return toolResult(string(data), false)
		}
		return toolResult(fmt.Sprintf("Attachment: %s, %d bytes. Inline content omitted; download the file through an authenticated room client if you need its bytes.", contentType, len(data)), false)
	}
	return toolResult(string(data), false)
}

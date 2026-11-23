// Package agenttools exposes the supervisor's narrow agent toolbox over stdio MCP.
package agenttools

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

type Server struct {
	URL    string
	Key    string
	Client *http.Client
	// Workdir is the agent's workspace as the agent sees it. Only the stdio bridge, which runs next to the agent's files, sets it; there
	// attachment_get saves a file it cannot return inline under <Workdir>/.aimebu/attachments. The server's HTTP endpoint leaves it empty and
	// asks the agent's supervisor to save the file instead.
	Workdir     string
	retryWindow time.Duration // tests shorten the default minute
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
	{"list_recoveries", "Active project lead only. List worker recoveries awaiting your decision.", empty},
	{"resolve_recovery", "Active project lead only. Resolve a worker recovery with resume, retry, or abandon; retries are idempotent. A broken-session recovery takes restart (a fresh session) or abandon; a held answer takes retry (ask the agent to resend) or abandon.", map[string]any{"type": "object", "properties": map[string]any{"item_id": map[string]string{"type": "string"}, "choice": map[string]any{"type": "string", "enum": []string{"resume", "retry", "abandon", "restart"}}, "note": map[string]string{"type": "string"}}, "required": []string{"item_id", "choice"}}},
	{"list_rooms", "List rooms where you are an active member, including archived rooms, without message previews.", empty},
	{"list_agents", "List agents in your project, including their roles and current state.", empty},
	{"room_history", "Read a page of messages in a project room. Private chats require current membership. Use before_no to page backward.", map[string]any{"type": "object", "properties": map[string]any{"room_id": map[string]string{"type": "string"}, "before_no": map[string]string{"type": "integer"}, "limit": map[string]string{"type": "integer"}}, "required": []string{"room_id"}}},
	{"list_macros", "Read the shared composer macro key-to-text map. Macros are expanded only when a human selects one in the web composer.", empty},
	{"list_schedules", "List schedules in a joined task room or the project home room, including their next run and last result.", map[string]any{"type": "object", "properties": map[string]any{"room_id": map[string]string{"type": "string"}}, "required": []string{"room_id"}}},
	{"create_schedule", "Active lead, or the flow's coordinator (the dispatcher), in a room you belong to. Schedule a note to a room agent or the current lead. Kinds: once (at RFC3339), interval (at least 300 seconds), daily (HH:MM), weekly (HH:MM and weekday 0=Sunday), cron (cron: a 5-field expression `minute hour day-of-month month day-of-week` or @hourly/@daily/@weekly/@monthly/@yearly, with *, lists, ranges, steps and jan/mon names, for example `0 8,12,16,20 * * *`; when both day fields are restricted either one matches). Timezone is an IANA zone and civil times follow its DST: a skipped local time runs at the next valid minute, a repeated one runs once. Use a stable request_key.", map[string]any{"type": "object", "properties": map[string]any{
		"room_id": map[string]string{"type": "string"}, "target_agent_id": map[string]string{"type": "string"}, "target_lead": map[string]string{"type": "boolean"}, "body": map[string]string{"type": "string"}, "kind": map[string]string{"type": "string"}, "timezone": map[string]string{"type": "string"}, "interval_seconds": map[string]string{"type": "integer"}, "local_time": map[string]string{"type": "string"}, "weekday": map[string]string{"type": "integer"}, "cron": map[string]string{"type": "string"}, "at": map[string]string{"type": "string"}, "request_key": map[string]string{"type": "string"},
	}, "required": []string{"room_id", "body", "kind", "timezone", "request_key"}}},
	{"delete_schedule", "Active lead, or the flow's coordinator (the dispatcher), in a room you belong to. Delete a schedule an agent created; a person's schedule stays theirs.", map[string]any{"type": "object", "properties": map[string]any{"room_id": map[string]string{"type": "string"}, "schedule_id": map[string]string{"type": "string"}}, "required": []string{"room_id", "schedule_id"}}},
	{"usage_left", "Show remaining project agent slots, spawn rate capacity, and available provider quota.", empty},
	{"run_command", "Start a long command that runs outside your turn, under the supervisor, in your own checkout: the same rights your own Bash has there, and nothing more. It keeps running while no turn runs; when it ends you are woken with a note carrying the exit code, the duration, the last lines of output and the path to the full log. At most two run at a time for you. Cwd stays inside your checkout. Use a stable request_key when retrying.", map[string]any{"type": "object", "properties": map[string]any{
		"cmd": map[string]string{"type": "string"}, "cwd": map[string]string{"type": "string"},
		"timeout_seconds": map[string]string{"type": "integer"}, "label": map[string]string{"type": "string"},
		"room_id":     map[string]string{"type": "string", "description": "Where the completion note goes; defaults to the room your current turn runs in"},
		"request_key": map[string]string{"type": "string"},
	}, "required": []string{"cmd", "request_key"}}},
	{"list_commands", "List the commands you started outside your turn, running and finished: state, exit code, duration and the log path.", empty},
	{"command_log", "Read a bounded tail of the full log of one of your commands.", map[string]any{"type": "object", "properties": map[string]any{
		"job_id": map[string]string{"type": "string"}, "lines": map[string]string{"type": "integer"}, "max_bytes": map[string]string{"type": "integer"},
	}, "required": []string{"job_id"}}},
	{"cancel_command", "Cancel one of your running commands. Its end still wakes you with a note. Use a stable request_key when retrying.", map[string]any{"type": "object", "properties": map[string]any{
		"job_id": map[string]string{"type": "string"}, "request_key": map[string]string{"type": "string"},
	}, "required": []string{"job_id", "request_key"}}},
	{"create_room", "Active lead, or the flow's coordinator (the dispatcher). Create a task room in your project. Omit parent_id to place it under the project home room (also in the leader-dispatcher flow; pass the general room's ID as parent_id to nest it there). Set purpose to research for a read-only investigation that ends in a report (it shows with a magnifier and counts as done once the report is delivered), general for a project-wide or coordination room, or omit it for an ordinary task room. Join the room and announce it in the parent room. Use a stable request_key when retrying.", map[string]any{"type": "object", "properties": map[string]any{
		"title": map[string]string{"type": "string"}, "parent_id": map[string]string{"type": "string"},
		"purpose":     map[string]any{"type": "string", "enum": []string{"task", "research", "general"}, "description": "What the room is for; defaults to task. Fixed when the room is created"},
		"worktree":    map[string]string{"type": "boolean", "description": "Create a Git worktree for this task room"},
		"base_ref":    map[string]string{"type": "string", "description": "Committed base branch or ref; defaults to current HEAD"},
		"request_key": map[string]string{"type": "string"},
	}, "required": []string{"title", "request_key"}}},
	{"dm_agent", "Active lead: send a private message to any agent, in this project or another. The flow's coordinator (the dispatcher): send one to an agent of your project you manage, such as a worker you spawned. The first message starts a chat with it; the chat is shared by the two of you and its people, and kept apart from every project room. The target is woken under its own project and answers in the chat; its answer wakes you in the chat, where you may also report into your own project's rooms. The server decides whether you may message that agent from the people behind this turn, never the model. There is a limit on turns per chat and on new chats per hour. Use a stable request_key when retrying.", map[string]any{"type": "object", "properties": map[string]any{
		"target_agent_id": map[string]string{"type": "string", "description": "Full agent ID of the agent to message"},
		"body":            map[string]string{"type": "string"},
		"request_key":     map[string]string{"type": "string"},
	}, "required": []string{"target_agent_id", "body", "request_key"}}},
	{"dm_human", "Send a private message to a person who is a member of your own project, starting a chat with them the first time; use it when you need that person's attention or decision and no room is the right place. Any active agent may, a worker as much as a lead. Give the person's login. The chat is between the two of you: the person's reply wakes you in it, and you answer there. The server decides who you may message, never the model; there is a limit on messages per chat and on new chats per hour. Use a stable request_key when retrying.", map[string]any{"type": "object", "properties": map[string]any{
		"target_user": map[string]string{"type": "string", "description": "Login of the person to message"},
		"body":        map[string]string{"type": "string"},
		"request_key": map[string]string{"type": "string"},
	}, "required": []string{"target_user", "body", "request_key"}}},
	{"close_room", "Active lead, or the flow's coordinator (the dispatcher). Archive a task room you belong to or created using its lifecycle_rev; closure includes the normal grace period. Close children first or set cascade. Use a stable request_key when retrying.", map[string]any{"type": "object", "properties": map[string]any{
		"room_id": map[string]string{"type": "string"}, "lifecycle_rev": map[string]string{"type": "integer"},
		"cascade": map[string]string{"type": "boolean"}, "request_key": map[string]string{"type": "string"},
	}, "required": []string{"room_id", "lifecycle_rev", "request_key"}}},
	{"handbook_get", "Read the project handbook. The lead sees all sections; a leader-dispatcher flow dispatcher sees Current state and Open backlog only.", empty},
	{"handbook_update", "Active lead only. Replace one handbook section with content or append patch, using the current revision.", map[string]any{"type": "object", "properties": map[string]any{
		"section": map[string]any{"type": "string", "enum": []string{"current_state", "open_backlog", "conventions", "gotchas", "timeline"}},
		"content": map[string]string{"type": "string"}, "patch": map[string]string{"type": "string"}, "rev": map[string]string{"type": "integer"},
	}, "required": []string{"section", "rev"}}},
	{"memory_add", "Add a curated project fact, the profile of a member of your project, or a shared agent note when project memory is enabled, or a note to a joined task room. A shared note waits for the owner's approval before other agents see it.", map[string]any{"type": "object", "properties": map[string]any{
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
	{"attachment_get", "Read a file attached to a note in your current turn while you remain a member of its room. Images and text up to 64 KB come back inline. Any other file (a zip, a PDF, larger text) up to 10 MB is saved into your workspace at .aimebu/attachments/<attachment_id>/<name> (no more readable than your workspace, ignored by Git, never extracted or run) and the result gives its path as you see it, its type and its size; open it with your own tools. Set save to also save an image or small text. When the file cannot be saved, the result says why.", map[string]any{"type": "object", "properties": map[string]any{
		"attachment_id": map[string]string{"type": "string"},
		"save":          map[string]string{"type": "boolean", "description": "Also save an image or small text to a file; other files are always saved"},
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
	{"stop_agent", "Active lead, or the flow's coordinator (the dispatcher). Stop a non-lead agent in your project that you manage (one the flow lets you manage, one you spawned, or one in a room you created), or yourself, using the current desired_rev. Use a stable request_key when retrying.", map[string]any{"type": "object", "properties": map[string]any{
		"agent_id": map[string]string{"type": "string"}, "rev": map[string]string{"type": "integer"},
		"request_key": map[string]string{"type": "string", "description": "Stable idempotency key for this stop request"},
	}, "required": []string{"agent_id", "rev", "request_key"}}},
	{"set_role", "Active lead only. Assign a non-lead role to an agent in your project using its current desired_rev. Use a stable request_key when retrying.", map[string]any{"type": "object", "properties": map[string]any{"agent_id": map[string]string{"type": "string"}, "role_key": map[string]string{"type": "string"}, "rev": map[string]string{"type": "integer"}, "request_key": map[string]string{"type": "string"}}, "required": []string{"agent_id", "role_key", "rev", "request_key"}}},
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
	var saveAttachmentFile bool
	getString := func(field string) string {
		var value string
		_ = json.Unmarshal(args[field], &value)
		return value
	}
	getBool := func(field string) bool { var value bool; _ = json.Unmarshal(args[field], &value); return value }
	switch name {
	case "list_recoveries":
		path = "/api/agent/recoveries"
	case "resolve_recovery":
		itemID, choice := getString("item_id"), getString("choice")
		if !strings.HasPrefix(itemID, "inb_") || len(itemID) > 100 || choice != "resume" && choice != "retry" && choice != "abandon" && choice != "restart" {
			return toolResult("valid item_id and choice required", true)
		}
		method, path = http.MethodPost, "/api/agent/recoveries/"+url.PathEscape(itemID)+"/decision"
		requestKey = "resolve_recovery:" + itemID + ":" + choice
		body = map[string]any{"choice": choice, "note": getString("note")}
	case "list_rooms":
		path = "/api/agent/rooms"
	case "list_agents":
		path = "/api/agent/agents"
	case "room_history":
		roomID := getString("room_id")
		if !strings.HasPrefix(roomID, "room_") || len(roomID) > 100 {
			return toolResult("invalid room_id", true)
		}
		values := url.Values{}
		for _, field := range []string{"before_no", "limit"} {
			if len(args[field]) == 0 {
				continue
			}
			var number int64
			if json.Unmarshal(args[field], &number) != nil || number < 1 {
				return toolResult(field+" must be a positive integer", true)
			}
			values.Set(field, fmt.Sprint(number))
		}
		path = "/api/agent/rooms/" + roomID + "/history"
		if len(values) > 0 {
			path += "?" + values.Encode()
		}
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
	case "delete_schedule":
		roomID, scheduleID := getString("room_id"), getString("schedule_id")
		if !strings.HasPrefix(roomID, "room_") || !strings.HasPrefix(scheduleID, "sch_") || len(scheduleID) > 100 {
			return toolResult("valid room_id and schedule_id required", true)
		}
		method, path = http.MethodDelete, "/api/agent/rooms/"+roomID+"/schedules/"+url.PathEscape(scheduleID)
	case "usage_left":
		path = "/api/agent/usage-left"
	case "run_command":
		method, path = http.MethodPost, "/api/agent/commands"
		requestKey = getString("request_key")
		if strings.TrimSpace(getString("cmd")) == "" {
			return toolResult("cmd is required", true)
		}
		var timeout int64
		if len(args["timeout_seconds"]) > 0 && (json.Unmarshal(args["timeout_seconds"], &timeout) != nil || timeout < 1) {
			return toolResult("timeout_seconds must be a positive integer", true)
		}
		body = map[string]any{"cmd": getString("cmd"), "cwd": getString("cwd"), "timeout_seconds": timeout,
			"label": getString("label"), "room_id": getString("room_id")}
	case "list_commands":
		path = "/api/agent/commands"
	case "command_log", "cancel_command":
		jobID := getString("job_id")
		if !strings.HasPrefix(jobID, "cmd_") || len(jobID) > 100 {
			return toolResult("invalid job_id", true)
		}
		if name == "command_log" {
			values := url.Values{}
			for _, field := range []string{"lines", "max_bytes"} {
				if len(args[field]) == 0 {
					continue
				}
				var number int64
				if json.Unmarshal(args[field], &number) != nil || number < 1 {
					return toolResult(field+" must be a positive integer", true)
				}
				values.Set(field, fmt.Sprint(number))
			}
			path = "/api/agent/commands/" + url.PathEscape(jobID) + "/log"
			if len(values) > 0 {
				path += "?" + values.Encode()
			}
		} else {
			method, path = http.MethodPost, "/api/agent/commands/"+url.PathEscape(jobID)+"/cancel"
			requestKey = getString("request_key")
			body = map[string]any{}
		}
	case "create_room":
		method, path = http.MethodPost, "/api/agent/rooms"
		requestKey = getString("request_key")
		title := strings.TrimSpace(getString("title"))
		if title == "" {
			return toolResult("title is required", true)
		}
		body = map[string]any{"title": title, "parent_id": getString("parent_id"), "purpose": getString("purpose"),
			"worktree": map[string]any{"enabled": getBool("worktree"), "base_ref": getString("base_ref")}}
	case "dm_agent":
		method, path = http.MethodPost, "/api/agent/dms"
		requestKey = getString("request_key")
		if strings.TrimSpace(getString("target_agent_id")) == "" || strings.TrimSpace(getString("body")) == "" {
			return toolResult("target_agent_id and body are required", true)
		}
		body = map[string]any{"target_agent_id": strings.TrimSpace(getString("target_agent_id")), "body": getString("body")}
	case "dm_human":
		method, path = http.MethodPost, "/api/agent/human-dms"
		requestKey = getString("request_key")
		if strings.TrimSpace(getString("target_user")) == "" || strings.TrimSpace(getString("body")) == "" {
			return toolResult("target_user and body are required", true)
		}
		body = map[string]any{"target_user": strings.TrimSpace(getString("target_user")), "body": getString("body")}
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
	case "handbook_get":
		path = "/api/agent/handbook"
	case "handbook_update":
		section := getString("section")
		if section != "current_state" && section != "open_backlog" && section != "conventions" && section != "gotchas" && section != "timeline" {
			return toolResult("invalid handbook section", true)
		}
		var rev int64
		if json.Unmarshal(args["rev"], &rev) != nil || rev < 0 {
			return toolResult("rev must be a nonnegative integer", true)
		}
		_, hasContent := args["content"]
		_, hasPatch := args["patch"]
		if hasContent == hasPatch {
			return toolResult("provide exactly one of content or patch", true)
		}
		method, path = http.MethodPut, "/api/agent/handbook"
		body = map[string]any{"section": section, "rev": rev}
		if hasContent {
			var content string
			if json.Unmarshal(args["content"], &content) != nil {
				return toolResult("content must be a string", true)
			}
			body.(map[string]any)["content"] = content
		}
		if hasPatch {
			var patch string
			if json.Unmarshal(args["patch"], &patch) != nil {
				return toolResult("patch must be a string", true)
			}
			body.(map[string]any)["patch"] = patch
		}
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
		if len(args["save"]) > 0 && json.Unmarshal(args["save"], &saveAttachmentFile) != nil {
			return toolResult("save must be a boolean", true)
		}
		if s.Workdir == "" {
			if result, done := s.saveWithoutDownload(ctx, id); done {
				return result
			}
		}
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
	case "set_role":
		agentID := getString("agent_id")
		if !agentIDPattern.MatchString(agentID) || getString("role_key") == "" {
			return toolResult("valid agent_id and role_key required", true)
		}
		var revision int64
		if json.Unmarshal(args["rev"], &revision) != nil || revision < 0 {
			return toolResult("rev must be a nonnegative integer", true)
		}
		method, path = http.MethodPost, "/api/agent/agents/"+agentID+"/role"
		requestKey = getString("request_key")
		body = map[string]any{"role_key": getString("role_key"), "rev": revision}
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
	// Reads and keyed writes can be replayed after a restart. An unkeyed
	// memory_add or revisioned write cannot: the first request may have landed.
	retryable := method == http.MethodGet || method == http.MethodPost && requestKey != "" || name == "react"
	var resp *http.Response
	if retryable {
		window := s.retryWindow
		if window <= 0 {
			window = time.Minute
		}
		retryCtx := ctx
		var cancel context.CancelFunc
		lastHTTPError := ""
		for delay := 100 * time.Millisecond; ; {
			attempt := req.Clone(retryCtx)
			if req.GetBody != nil {
				attempt.Body, err = req.GetBody()
				if err != nil {
					return toolResult(err.Error(), true)
				}
			}
			resp, err = client.Do(attempt)
			retryAfter := ""
			if resp != nil {
				retryAfter = resp.Header.Get("Retry-After")
			}
			maintenance := err == nil && resp.StatusCode == http.StatusServiceUnavailable && retryAfter != ""
			if maintenance {
				body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
				_ = resp.Body.Close()
				resp = nil
				if readErr != nil {
					if lastHTTPError != "" {
						return toolResult(lastHTTPError, true)
					}
					return toolResult(readErr.Error(), true)
				}
				if len(body) > 1<<20 {
					return toolResult("tool response exceeds 1 MB", true)
				}
				lastHTTPError = fmt.Sprintf("HTTP %d: %s", http.StatusServiceUnavailable, body)
			} else if err == nil || !retryableConnectionError(err) {
				break
			}
			if cancel == nil {
				retryCtx, cancel = context.WithTimeout(ctx, window)
				defer cancel()
			}
			if retryCtx.Err() != nil {
				if lastHTTPError != "" {
					return toolResult(lastHTTPError, true)
				}
				return toolResult(retryCtx.Err().Error(), true)
			}
			wait := delay
			if maintenance {
				if seconds, parseErr := strconv.Atoi(retryAfter); parseErr == nil && seconds >= 0 {
					wait = max(wait, time.Duration(min(seconds, 3600))*time.Second)
				} else if when, parseErr := http.ParseTime(retryAfter); parseErr == nil {
					wait = max(wait, time.Until(when))
				}
			}
			select {
			case <-retryCtx.Done():
				if lastHTTPError != "" {
					return toolResult(lastHTTPError, true)
				}
				return toolResult(retryCtx.Err().Error(), true)
			case <-time.After(wait):
			}
			if delay < 2*time.Second {
				delay *= 2
			}
		}
	} else {
		resp, err = client.Do(req)
	}
	if err != nil {
		return toolResult(err.Error(), true)
	}
	defer resp.Body.Close()
	limit := int64(1<<20 + 1)
	if name == "attachment_get" {
		limit = MaxAttachmentBytes + 1
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return toolResult(err.Error(), true)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return toolResult(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, data), true)
	}
	if name == "attachment_get" {
		return s.attachmentResult(ctx, client, getString("attachment_id"), resp.Header, data, saveAttachmentFile)
	}
	if len(data) > 1<<20 {
		return toolResult("tool response exceeds 1 MB", true)
	}
	return toolResult(string(data), false)
}

// attachmentResult turns a downloaded attachment into the tool's answer: an image or small text inline, anything else saved into the agent's
// workspace, and an image or small text saved too when the agent asked. The stdio bridge, which runs next to the files, saves it itself;
// elsewhere (the server's HTTP endpoint) the agent's supervisor is asked to save it, through POST /api/agent/attachments/{id}/save.
func (s Server) attachmentResult(ctx context.Context, client *http.Client, id string, header http.Header, data []byte, save bool) any {
	if len(data) > MaxAttachmentBytes {
		return toolResult(fmt.Sprintf("attachment is larger than %d MB, the most attachment_get returns; it was not saved", MaxAttachmentBytes>>20), true)
	}
	contentType, _, _ := mime.ParseMediaType(header.Get("Content-Type"))
	var inline any
	if inlineCandidate(contentType, len(data)) {
		if inlineImage(contentType) {
			inline = map[string]string{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": contentType}
		} else if utf8.Valid(data) {
			inline = map[string]string{"type": "text", "text": string(data)}
		}
	}
	if inline != nil && !save {
		return map[string]any{"content": []any{inline}, "isError": false}
	}
	path, size := "", len(data)
	if s.Workdir == "" {
		var err error
		path, size, err = s.remoteSave(ctx, client, id)
		if err != nil {
			note := fmt.Sprintf("Attachment: %s, %d bytes. It cannot be saved to your workspace here: %v. It is not available as a file.", contentType, len(data), err)
			if inline != nil {
				return map[string]any{"content": []any{inline, map[string]string{"type": "text", "text": note}}, "isError": false}
			}
			return toolResult(note, false)
		}
	} else {
		var err error
		path, err = SaveAttachment(s.Workdir, id, AttachmentFileName(header.Get("Content-Disposition")), data)
		if err != nil {
			return toolResult(fmt.Sprintf("Attachment: %s, %d bytes. It could not be saved: %v", contentType, len(data), err), true)
		}
	}
	saved, _ := json.Marshal(map[string]any{"saved": true, "path": path, "mime": contentType, "size": size})
	note := map[string]string{"type": "text", "text": string(saved) + "\nSaved to your workspace (ignored by Git). Open it with your own tools; it is never extracted or run for you."}
	if inline != nil {
		return map[string]any{"content": []any{inline, note}, "isError": false}
	}
	return map[string]any{"content": []any{note}, "isError": false}
}

func inlineImage(contentType string) bool {
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// inlineCandidate says whether an attachment of this type and size may come back inline: an image, or text up to 64 KB (which must also be
// valid UTF-8, known only from its bytes).
func inlineCandidate(contentType string, size int) bool {
	if inlineImage(contentType) {
		return true
	}
	return size <= maxInlineText && (strings.HasPrefix(contentType, "text/") || contentType == "application/json" || contentType == "application/xml" || contentType == "image/svg+xml")
}

// saveWithoutDownload is attachment_get where the toolbox has no workspace of its own (the server's HTTP endpoint): it reads the attachment's
// type and size with a HEAD request and, for a file that cannot come back inline, has the agent's supervisor save it without downloading the
// bytes here first. done is false when the download path should run instead: the HEAD was refused (the download reports why), the size is
// unknown, or the file may come back inline.
func (s Server) saveWithoutDownload(ctx context.Context, id string) (result any, done bool) {
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, strings.TrimRight(s.URL, "/")+"/api/agent/attachments/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+s.Key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	resp.Body.Close()
	size, err := strconv.Atoi(resp.Header.Get("Content-Length"))
	if resp.StatusCode != http.StatusOK || err != nil || size < 0 {
		return nil, false
	}
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if size > MaxAttachmentBytes {
		return toolResult(fmt.Sprintf("attachment is larger than %d MB, the most attachment_get returns; it was not saved", MaxAttachmentBytes>>20), true), true
	}
	if inlineCandidate(contentType, size) {
		return nil, false
	}
	path, savedSize, err := s.remoteSave(ctx, client, id)
	if err != nil {
		return toolResult(fmt.Sprintf("Attachment: %s, %d bytes. It cannot be saved to your workspace here: %v. It is not available as a file.", contentType, size, err), false), true
	}
	saved, _ := json.Marshal(map[string]any{"saved": true, "path": path, "mime": contentType, "size": savedSize})
	return map[string]any{"content": []any{map[string]string{"type": "text", "text": string(saved) + "\nSaved to your workspace (ignored by Git). Open it with your own tools; it is never extracted or run for you."}}, "isError": false}, true
}

// remoteSave asks the server to have the agent's supervisor save the attachment into the workspace of the agent's running turn, and returns
// the path as the agent sees it. The error is the server's own message (an old supervisor, an offline machine, ...).
func (s Server) remoteSave(ctx context.Context, client *http.Client, id string) (string, int, error) {
	if !savedAttachmentID.MatchString(id) {
		return "", 0, errors.New("invalid attachment_id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.URL, "/")+"/api/agent/attachments/"+url.PathEscape(id)+"/save", nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Key)
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", 0, err
	}
	var answer struct {
		Saved   bool   `json:"saved"`
		Path    string `json:"path"`
		Size    int    `json:"size"`
		Message string `json:"message"` // an error answer's text
	}
	_ = json.Unmarshal(body, &answer)
	if resp.StatusCode != http.StatusOK || !answer.Saved || answer.Path == "" {
		message := cmp.Or(answer.Message, fmt.Sprintf("HTTP %d", resp.StatusCode))
		return "", 0, errors.New(message)
	}
	return answer.Path, answer.Size, nil
}

func retryableConnectionError(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

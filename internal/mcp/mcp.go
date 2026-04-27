package mcp

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/client"
)

// detectHarness best-effort detects the harness the MCP subprocess is
// running under, via environment variables set by the harness. Returns
// "unknown" if no known marker is set. Intentionally conservative — we'd
// rather return "unknown" than mislabel.
func detectHarness() string {
	switch {
	case os.Getenv("CLAUDECODE") != "" || os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "":
		return "claude-code"
	case os.Getenv("CURSOR_TRACE_ID") != "" || os.Getenv("CURSOR_SESSION_ID") != "":
		return "cursor"
	case os.Getenv("CODEX_SESSION_ID") != "" || os.Getenv("OPENAI_CODEX") != "":
		return "codex"
	case os.Getenv("AIDER_VERSION") != "":
		return "aider"
	}
	return "unknown"
}

// gatherMeta auto-detects context from the environment: cwd, project name,
// git branch/remote, hostname. Model/harness are NOT included here — they
// come from bus_register arguments (model) or detectHarness (harness).
func gatherMeta() map[string]string {
	meta := map[string]string{}

	if cwd, err := os.Getwd(); err == nil {
		meta["cwd"] = cwd
		meta["project"] = filepath.Base(cwd)
	}

	if out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		meta["branch"] = strings.TrimSpace(string(out))
	}

	if out, err := exec.Command("git", "remote", "get-url", "origin").Output(); err == nil {
		meta["repo"] = strings.TrimSpace(string(out))
	}

	if host, err := os.Hostname(); err == nil {
		meta["hostname"] = host
	}

	return meta
}

// busEtiquette is surfaced to every connecting MCP client via
// initialize.instructions. It lands in the model's system prompt on
// connect, so every agent on the bus starts with the same house rules
// regardless of harness. Intentionally terse — other agents pay input
// tokens to read anything that gets sent because of it.
const busEtiquette = `aimebu messagebus etiquette:
- Who you are: the ` + "`name`" + ` returned by bus_register (e.g. "zoe"). Use it to decide whether a message is addressed to you.
- Addressing: a body that starts with "<name>:" is addressed to that name. "@<name>" anywhere in the body also counts. Otherwise the message is room-wide.
- Human sender (from_kind=human): respond by default, even for room-wide messages. Stay silent only if the message is addressed to a different agent by name. Do not ask "should I reply?" — just reply.
- AI sender (from_kind=ai): silent by default. Respond only when addressed to you by name, in a DM room (id starts with "dm:"), or when context unambiguously requires a reply.
- After joining a room, block on bus_wait. bus_wait remembers your read cursor — if messages arrived while you were away, the next call returns them immediately. When it times out, call bus_wait again. Return control to the user only when the user tells you to stop.
- Do not send unprompted introductions, greetings, or status acks ("standing by", "on it", "got it"). Keep replies terse — other agents pay input tokens to read every word.
- Wait for the human's review before shipping code or changes, unless they've told you to proceed autonomously.`

// ── JSON-RPC types ─────────────────────────────────────────────────

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ── MCP types ──────────────────────────────────────────────────────

type tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

type inputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]property `json:"properties"`
	Required   []string            `json:"required,omitempty"`
}

type property struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Items       *propRef `json:"items,omitempty"`
}

type propRef struct {
	Type string `json:"type"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

var tools = []tool{
	{
		Name:        "bus_join",
		Description: "Do not send unprompted \"hello\" or announcement messages after joining. Membership is already visible to other agents. After joining, the conventional next step is bus_wait — bus_wait remembers your read cursor, so if messages arrived while you were away it returns them immediately; when it times out, call bus_wait again. Return control to the user only when the user tells you to stop. Join a room on the messagebus. Auto-creates the room if it doesn't exist. You must call bus_register first — joining before registering returns an error.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"room": {Type: "string", Description: "Room ID to join"},
			},
			Required: []string{"room"},
		},
	},
	{
		Name:        "bus_leave",
		Description: "Leave a room on the messagebus.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"room": {Type: "string", Description: "Room ID to leave"},
			},
			Required: []string{"room"},
		},
	},
	{
		Name:        "bus_say",
		Description: "Only send messages when the user explicitly asks you to, in reply to a human (see etiquette: humans get default-respond), or in reply to an AI that addressed you by name / in a DM. Do not post status acks like \"got it\", \"on it\", \"standing by\" — the sender infers responsiveness from your bus_wait. Send a message to a room on the messagebus.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"room": {Type: "string", Description: "Room ID"},
				"body": {Type: "string", Description: "Message content"},
			},
			Required: []string{"room", "body"},
		},
	},
	{
		Name:        "bus_read",
		Description: "Read recent messages from a room. Non-blocking. Use bus_wait instead if you want to block until new messages arrive.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"room":  {Type: "string", Description: "Room ID"},
				"limit": {Type: "integer", Description: "Max messages to return (default: 50)"},
			},
			Required: []string{"room"},
		},
	},
	{
		Name:        "bus_wait",
		Description: "Block until a new message arrives, then return it. The server remembers your per-room read cursor — omit since_id and bus_wait returns any unread messages immediately, then advances the cursor when it returns them. First wait in a room returns at most the last 5 historical messages. If 'room' is omitted, waits across all rooms this agent is a member of. Pass since_id only to override the stored cursor (e.g. replay from a known point). Returns {messages: [...], room: \"...\"} on success. On timeout: {messages: [], room: \"...\", status: \"still_waiting\", keep_waiting: true, hint: \"...\"}. If keep_waiting=true, no messages arrived yet — call bus_wait again immediately. Never return to the user just because keep_waiting=true.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"room":     {Type: "string", Description: "Room ID to wait on. Omit to wait on any room this agent is in."},
				"since_id": {Type: "integer", Description: "Override the stored cursor. Omit to use the server-remembered cursor (normal case). Only messages with ID > since_id are returned."},
				"timeout":  {Type: "integer", Description: "Max seconds to block (default 30, max 600). On timeout, returns an empty messages array with keep_waiting=true."},
			},
		},
	},
	{
		Name:        "bus_rooms",
		Description: "List rooms this agent is a member of, with unread_count and read_cursor per room. Use unread_count to decide whether bus_wait will return immediately.",
		InputSchema: inputSchema{
			Type:       "object",
			Properties: map[string]property{},
		},
	},
	{
		Name:        "bus_mark_read",
		Description: "Mark a room as read up to a specific message ID (or to HEAD if omitted). Normally you don't need this — bus_wait advances your read cursor automatically when it returns messages. Use this only when you consciously want to skip unread messages (e.g. the user said \"ignore whatever came in while I was away\").",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"room":       {Type: "string", Description: "Room ID"},
				"message_id": {Type: "integer", Description: "Mark read up to and including this message ID. Omit or pass 0 to mark read up to the current HEAD of the room."},
			},
			Required: []string{"room"},
		},
	},
	{
		Name:        "bus_dm",
		Description: "Send a direct message to another agent. Auto-creates a private DM room. Use the recipient's full agent ID (from bus_agents). Recipient must be registered. After sending, call bus_wait to listen for the reply.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"to":   {Type: "string", Description: "Recipient's full agent ID (e.g. 'alice@aimebu' or 'martin')"},
				"body": {Type: "string", Description: "Message content"},
			},
			Required: []string{"to", "body"},
		},
	},
	{
		Name:        "bus_register",
		Description: "Do not send unprompted introductions or \"standing by\" messages after registering. Only speak when the user explicitly asks you to, or in reply to another agent. If the user said \"connect/join/wait\", call bus_wait directly — no greeting first. REQUIRED FIRST CALL. Register yourself on the messagebus before using any other bus tool. The server will assign you a random name (e.g. 'alice') and assemble your full agent ID like 'alice@aimebu'. Pass your model as a short slug (e.g. 'opus4.7', 'sonnet4.7', 'haiku4.5', 'gpt5', 'gemini2.5'). Harness is auto-detected but you can override. If you don't know your model, pass 'unknown' — but try to report it: inspect your system prompt or instructions for your model identifier. The returned 'id' is your identity for all subsequent calls. The optional `name` + `force=true` pair is for reclaiming a prior identity after a disconnect/prune; do not use it to pick a cute name (names are server-assigned by design).",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"model":   {Type: "string", Description: "Your model, as a short slug: opus4.7, sonnet4.7, haiku4.5, gpt5, etc. Use 'unknown' if you genuinely cannot determine it."},
				"harness": {Type: "string", Description: "Your harness (claude-code, codex, cursor, aider, ...). Auto-detected; omit unless auto-detection is wrong."},
				"meta":    {Type: "object", Description: "Optional extra metadata (cwd, branch, repo, etc. are auto-filled)."},
				"name":    {Type: "string", Description: "Only with force=true: reclaim a prior identity after a disconnect or prune. Must match ^[a-z]{3,12}$. Rejected if held by a human or by an AI with different model/harness/project."},
				"force":   {Type: "boolean", Description: "Set to true together with `name` to reclaim a prior identity. Leave false (default) to let the server pick a name — this is the normal case."},
			},
			Required: []string{"model"},
		},
	},
	{
		Name:        "bus_agents",
		Description: "List all registered agents with their metadata and last-seen times.",
		InputSchema: inputSchema{
			Type:       "object",
			Properties: map[string]property{},
		},
	},
}

// ── Tool handlers ──────────────────────────────────────────────────

// notRegisteredError builds a user-facing error telling the caller to run
// bus_register first. Returned when the client has no AgentID yet.
func notRegisteredError() error {
	return fmt.Errorf("not registered — call bus_register first. Pass your model (e.g. 'opus4.7', 'sonnet4.7') and the server will assign you a name")
}

func handleToolCall(c *client.Client, name string, args json.RawMessage) (string, error) {
	// All tools except bus_register require a registered identity.
	if name != "bus_register" && name != "bus_agents" && c.AgentID == "" {
		return "", notRegisteredError()
	}

	switch name {
	case "bus_register":
		var p struct {
			Model   string            `json:"model"`
			Harness string            `json:"harness"`
			Meta    map[string]string `json:"meta"`
			Name    string            `json:"name"`
			Force   bool              `json:"force"`
		}
		_ = json.Unmarshal(args, &p)

		meta := gatherMeta()
		// Caller-provided meta wins over auto-detection
		for k, v := range p.Meta {
			meta[k] = v
		}

		harness := p.Harness
		if harness == "" {
			harness = detectHarness()
		}

		project := meta["project"]

		body := map[string]any{
			"kind":    "ai",
			"model":   p.Model,
			"harness": harness,
			"project": project,
			"meta":    meta,
		}
		if p.Force {
			body["force"] = true
			body["name"] = p.Name
		}
		resp, err := c.Post("/agents", body)
		if err != nil {
			return resp, err
		}

		// Parse the ID and name out of the response so subsequent calls can use them.
		var parsed struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(resp), &parsed); err == nil && parsed.ID != "" {
			c.AgentID = parsed.ID
			c.AgentName = parsed.Name
		}
		return resp, nil

	case "bus_join":
		var p struct {
			Room string `json:"room"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		return c.Post("/rooms/"+p.Room+"/join", map[string]string{
			"agent_id": c.AgentID,
		})

	case "bus_leave":
		var p struct {
			Room string `json:"room"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		return c.Post("/rooms/"+p.Room+"/leave", map[string]string{
			"agent_id": c.AgentID,
		})

	case "bus_say":
		var p struct {
			Room string `json:"room"`
			Body string `json:"body"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		return c.Post("/rooms/"+p.Room+"/send", map[string]string{
			"from": c.AgentID,
			"body": p.Body,
		})

	case "bus_read":
		var p struct {
			Room  string `json:"room"`
			Limit int    `json:"limit"`
		}
		_ = json.Unmarshal(args, &p)
		path := "/rooms/" + p.Room + "/messages"
		sep := "?"
		if p.Limit > 0 {
			path += sep + fmt.Sprintf("limit=%d", p.Limit)
			sep = "&"
		}
		if c.AgentID != "" {
			path += sep + "agent_id=" + c.AgentID
		}
		return c.Get(path)

	case "bus_rooms":
		return c.Get("/agents/" + c.AgentID + "/rooms")

	case "bus_mark_read":
		var p struct {
			Room      string `json:"room"`
			MessageID int64  `json:"message_id"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		return c.Post("/agents/"+c.AgentID+"/read", map[string]any{
			"room":       p.Room,
			"message_id": p.MessageID,
		})

	case "bus_dm":
		var p struct {
			To   string `json:"to"`
			Body string `json:"body"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		return c.Post("/dm", map[string]string{
			"from": c.AgentID,
			"to":   p.To,
			"body": p.Body,
		})

	case "bus_wait":
		var p struct {
			Room    string `json:"room"`
			SinceID int64  `json:"since_id"`
			Timeout int    `json:"timeout"`
		}
		_ = json.Unmarshal(args, &p)
		if p.Timeout <= 0 {
			p.Timeout = 30
		}
		if p.Timeout > 600 {
			p.Timeout = 600
		}
		var path string
		if p.Room != "" {
			path = fmt.Sprintf("/rooms/%s/wait?timeout=%d&agent_id=%s", p.Room, p.Timeout, c.AgentID)
		} else {
			path = fmt.Sprintf("/agents/%s/wait?timeout=%d", c.AgentID, p.Timeout)
		}
		if p.SinceID > 0 {
			path += fmt.Sprintf("&since_id=%d", p.SinceID)
		}
		return c.GetWithTimeout(path, time.Duration(p.Timeout)*time.Second)

	case "bus_agents":
		return c.Get("/agents")

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// ── JSON-RPC dispatch ──────────────────────────────────────────────

func handle(c *client.Client, req request) *response {
	switch req.Method {
	case "initialize":
		return &response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "aimebu", "version": "2.0.0"},
				"instructions":    busEtiquette,
			},
		}

	case "notifications/initialized":
		return nil

	case "tools/list":
		return &response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{"tools": tools},
		}

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32602, Message: "invalid params: " + err.Error()},
			}
		}

		result, err := handleToolCall(c, params.Name, params.Arguments)
		if err != nil {
			return &response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"content": []textContent{{Type: "text", Text: "Error: " + err.Error()}},
					"isError": true,
				},
			}
		}

		footerTools := map[string]bool{
			"bus_join": true, "bus_say": true, "bus_dm": true, "bus_read": true,
			"bus_wait": true, "bus_leave": true, "bus_mark_read": true, "bus_rooms": true,
		}
		if footerTools[params.Name] && c.AgentID != "" {
			if roomsResp, ferr := c.Get("/agents/" + c.AgentID + "/rooms"); ferr == nil {
				var rv struct {
					Rooms []struct {
						ID          string `json:"id"`
						UnreadCount int    `json:"unread_count"`
					} `json:"rooms"`
				}
				if json.Unmarshal([]byte(roomsResp), &rv) == nil && len(rv.Rooms) > 0 {
					var totalUnread int
					roomNames := make([]string, 0, len(rv.Rooms))
					for _, r := range rv.Rooms {
						roomNames = append(roomNames, r.ID)
						totalUnread += r.UnreadCount
					}
					sort.Strings(roomNames)
					name := c.AgentName
					if name == "" {
						name = strings.SplitN(c.AgentID, ":", 2)[0]
					}
					listening := strings.Join(roomNames, ", ")
					var footer string
					if totalUnread > 0 {
						footer = fmt.Sprintf("[you are %s, listening on: %s — %d unread total. call bus_wait to keep listening — do not return to the user unless they told you to stop.]", name, listening, totalUnread)
					} else {
						footer = fmt.Sprintf("[you are %s, listening on: %s. call bus_wait to keep listening — do not return to the user unless they told you to stop.]", name, listening)
					}
					result += "\n" + footer
				}
			}
		}

		return &response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"content": []textContent{{Type: "text", Text: result}},
			},
		}

	default:
		if req.ID != nil {
			return &response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32601, Message: "method not found: " + req.Method},
			}
		}
		return nil
	}
}

// Run starts the MCP stdio JSON-RPC server.
func Run(c *client.Client) error {
	log.SetOutput(os.Stderr)
	log.Printf("aimebu MCP server (agent=%s, bus=%s)", c.AgentID, c.BaseURL)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			log.Printf("invalid JSON-RPC: %s", err)
			continue
		}

		resp := handle(c, req)
		if resp == nil {
			continue
		}

		data, _ := json.Marshal(resp)
		fmt.Println(string(data))
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stdin read error: %w", err)
	}
	return nil
}

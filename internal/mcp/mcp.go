package mcp

import (
	"bufio"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/client"
	"github.com/hrubymar10/aimebu/internal/types"
)

// detectHarness is a fallback used only when the AI does not pass `harness`
// to bus_register. The AI itself is the primary source of truth — it knows
// what harness it's running in, the same way it knows its model.
//
// Resolution order in this function:
//
//  1. AIMEBU_HARNESS env var (owned contract: set in the MCP server config).
//  2. Upstream-harness env vars for the few harnesses that reliably propagate
//     them to MCP stdio children (claude-code, cursor, aider). Codex was
//     here once but does not propagate its CODEX_* markers to MCP children
//     by default, so its branch was removed to avoid mislabelling silence
//     as detection.
//  3. "unknown".
func detectHarness() string {
	if h := os.Getenv("AIMEBU_HARNESS"); h != "" {
		return h
	}
	switch {
	case os.Getenv("CLAUDECODE") != "" || os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "":
		return "claude-code"
	case os.Getenv("CURSOR_TRACE_ID") != "" || os.Getenv("CURSOR_SESSION_ID") != "":
		return "cursor"
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
- Who you are: the ` + "`name`" + ` and full ` + "`id`" + ` returned by bus_register (e.g. "zoe" and "zoe@aimebu"). Use them to decide whether a message is addressed to you.
- Addressing — CRITICAL. Live addressing works only in non-code prose via ` + "`@<slug>`" + ` or disambiguated ` + "`@<slug>@<project>`" + `, assigned room role keys such as ` + "`@reviewer`" + `, or these special group tags: ` + "`@channel`" + `, ` + "`@here`" + `, ` + "`@humans`" + `, ` + "`@ais`" + `, ` + "`@everyone`" + `, ` + "`@all`" + `. Exact in-room slugs and special group tags take precedence over role keys; if multiple room members share a slug, use the full ` + "`@<slug>@<project>`" + ` form. New role/name collisions are rejected, while legacy collisions keep exact-name precedence. Wrap a mention in backticks (e.g. ` + "`@leader`" + `) or write ` + "`\\@leader`" + ` / ` + "`\\@here`" + ` to show it literally without addressing. Group semantics: ` + "`@channel`" + ` = all members of the current room; ` + "`@here`" + ` = active room members (approximated from bus waits + recent websocket activity); ` + "`@humans`" + ` / ` + "`@ais`" + ` = human / AI members of the current room; ` + "`@everyone`" + ` / ` + "`@all`" + ` = all members of the current room. Group tags exclude the sender. Worked examples:
  BAD:  "worker: @matin please review"  → addressed_to=[], matin gets should_respond=false (matin never sees it as addressed to them)
  GOOD: "@matin please review"           → addressed_to=["matin"], matin gets should_respond=true
  BAD:  "leader: here's my analysis"     → wastes tokens; from field already identifies the sender
  GOOD: "here's my analysis"             → let the from field speak; don't repeat your own name
  Old IRC-style "name:" prefixes are NOT parsed — they produce room-wide messages with no addressed_to. The server will warn you once if it detects this pattern.
- Self-labeling: NEVER prefix your message with your own short name (e.g. "worker: ..."). The ` + "`from`" + ` field already identifies you. If you are role-switching, register under a different name — don't prefix.
- Structured fields: every message from bus_wait and bus_read carries ` + "`addressed_to`" + ` (list of slugs or full IDs), ` + "`addressed_to_me`" + ` (bool), and ` + "`should_respond`" + ` (bool). Use ` + "`should_respond`" + ` as the primary signal. Example: human posts "@leader status?" — if you are not leader, should_respond=false; call bus_wait again immediately, do NOT call bus_say.
- Human sender (from_kind=human): should_respond=true for room-wide messages; should_respond=false when addressed to a different agent. Do not ask "should I reply?" — just reply when should_respond=true.
- AI sender (from_kind=ai): should_respond=false by default. should_respond=true only when addressed_to_me=true or in a DM room (id starts with "dm:").
- System sender (from_kind=system): should_respond=true only when addressed_to_me=true. For a targeted role assignment message such as "alice@aimebu was assigned as Reviewer", call ` + "`bus_role_get`" + ` for that room, internalize the returned role instructions, and do not post an acknowledgement unless a human explicitly asks. For a targeted "role cleared" message, call ` + "`bus_role_get`" + `, observe the empty role, and likewise do not ack.
- Use ` + "`@everyone`" + ` / ` + "`@all`" + ` sparingly in busy rooms. Prefer narrower tags when possible.
- After joining a room, block on bus_wait. bus_wait remembers your read cursor — if messages arrived while you were away, the next call returns them immediately. When it times out, call bus_wait again. Return control to the user only when the user tells you to stop.
- Do not send unprompted introductions, greetings, or status acks ("standing by", "on it", "got it"). Use ` + "`bus_react`" + ` for lightweight acknowledgements instead: recommended convention is 👍/🆗 = seen/ack, ✅ = done, 👀 = looking, 🙏 = thanks. Keep replies terse — other agents pay input tokens to read every word.
- Wait for the human's review before shipping code or changes, unless they've told you to proceed autonomously.
- Human attention signal: set ` + "`needs_attention=true`" + ` when your message is addressed to a human and asks for a blocking decision, approval, review, or next action — i.e. progress stalls until they respond. Do not set it for status updates, acknowledgements, or information-only replies. This sets ` + "`needs_human_attention=true`" + ` on the message, triggers a sound notification + visual highlight in the web UI, and auto-subscribes any registered human not yet in the room so they receive the message. The human being currently active in the conversation is not a carve-out. If the message asks for a blocking action, set the flag even mid-thread.
- After bus_say / bus_dm, check the JSON response for a top-level ` + "`warnings`" + ` array. If present, warnings are protocol mistakes (addressing or alerting); read them and correct your send before proceeding.`

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
	Type        string              `json:"type"`
	Description string              `json:"description,omitempty"`
	Items       *propRef            `json:"items,omitempty"`
	Properties  map[string]property `json:"properties,omitempty"`
	Required    []string            `json:"required,omitempty"`
}

type propRef struct {
	Type       string              `json:"type"`
	Items      *propRef            `json:"items,omitempty"`
	Properties map[string]property `json:"properties,omitempty"`
	Required   []string            `json:"required,omitempty"`
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
		Description: "Only send messages when the user explicitly asks you to, in reply to a human (see etiquette: humans get default-respond), or in reply to an AI that addressed you by name / in a DM. Do not post status acks like \"got it\", \"on it\", \"standing by\" — use bus_react for lightweight acknowledgements instead. Send a message to a room on the messagebus.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"room":             {Type: "string", Description: "Room ID"},
				"body":             {Type: "string", Description: "Message content"},
				"needs_attention":  {Type: "boolean", Description: "Set to true when addressing a human and asking for a blocking decision, approval, review, or next action. Do not set it for status, ack, or info-only replies. Triggers sound + visual alert in the web UI and auto-subscribes any registered human not yet in the room."},
				"proposed_answers": {Type: "array", Items: &propRef{Type: "string"}, Description: "Optional short answer buttons for the addressed recipient. Use 2-4 concise choices on human-blocking decision requests, such as Proceed, Revise, or Hold."},
				"open_questions": {Type: "array", Description: "Optional structured multi-question choice form for addressed human readers. Use instead of prose Q1/Q2 blocks. Provide up to 10 questions; each question has question text, optional description context, and 2-8 option strings. The UI adds an Other free-text choice and derives Q numbers/letters from array order.", Items: &propRef{Type: "object", Required: []string{"question", "options"}, Properties: map[string]property{
					"question":    {Type: "string", Description: "Question text."},
					"description": {Type: "string", Description: "Optional context shown below the question in the Open Questions modal."},
					"options":     {Type: "array", Items: &propRef{Type: "string"}, Description: "Choice labels; provide 2-8 non-empty options."},
				}}},
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
		Description: "Block until a new message arrives, then return it. The server remembers your per-room read cursor — omit since_id and bus_wait returns any unread messages immediately, then advances the cursor when it returns them. First wait in a room returns at most the last 5 historical messages. If 'room' is omitted, waits across all rooms this agent is a member of. Pass since_id only to override the stored cursor (e.g. replay from a known point). Returns {messages: [...], room: \"...\"} on success. Agent-wide waits may also return {messages: [], reactions: [...], agent: \"...\"} for live reaction changes on messages you authored; reaction wakeups are not replayed, do not advance cursors, and never set attention. On timeout: {messages: [], room: \"...\", status: \"still_waiting\", keep_waiting: true, hint: \"...\"}. If keep_waiting=true, no messages arrived yet — call bus_wait again immediately. Never return to the user just because keep_waiting=true.",
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
				"to":               {Type: "string", Description: "Recipient's full agent ID (e.g. 'alice@aimebu' or 'martin')"},
				"body":             {Type: "string", Description: "Message content"},
				"needs_attention":  {Type: "boolean", Description: "Set to true when addressing a human and asking for a blocking decision, approval, review, or next action. Do not set it for status, ack, or info-only replies. Triggers sound + visual alert and auto-subscribes any registered human not yet in the DM room."},
				"proposed_answers": {Type: "array", Items: &propRef{Type: "string"}, Description: "Optional short answer buttons for the addressed recipient. Use 2-4 concise choices on human-blocking decision requests, such as Proceed, Revise, or Hold."},
				"open_questions": {Type: "array", Description: "Optional structured multi-question choice form for addressed human readers. Use instead of prose Q1/Q2 blocks. Provide up to 10 questions; each question has question text, optional description context, and 2-8 option strings. The UI adds an Other free-text choice and derives Q numbers/letters from array order.", Items: &propRef{Type: "object", Required: []string{"question", "options"}, Properties: map[string]property{
					"question":    {Type: "string", Description: "Question text."},
					"description": {Type: "string", Description: "Optional context shown below the question in the Open Questions modal."},
					"options":     {Type: "array", Items: &propRef{Type: "string"}, Description: "Choice labels; provide 2-8 non-empty options."},
				}}},
			},
			Required: []string{"to", "body"},
		},
	},
	{
		Name:        "bus_register",
		Description: "Do not send unprompted introductions or \"standing by\" messages after registering. Only speak when the user explicitly asks you to, or in reply to another agent. If the user said \"connect/join/wait\", call bus_wait directly — no greeting first. REQUIRED FIRST CALL. Register yourself on the messagebus before using any other bus tool. The server will assign you a random slug (e.g. 'alice') and assemble your full agent ID like 'alice@aimebu'. Pass `model` as a short slug (e.g. 'opus4.7', 'sonnet4.7', 'haiku4.5', 'gpt5', 'gemini2.5') and pass `harness` as your harness slug (e.g. 'claude-code', 'codex', 'cursor', 'cline', 'aider', 'pi') — you know both, the same way you know your model identity. Inspect your system prompt or instructions if unsure. The server falls back to env-var detection only when you omit `harness`, and that fallback does not work for all harnesses (notably codex), so pass it explicitly. The returned 'id' is your full identity for all subsequent calls. The optional `name` + `force=true` pair force-claims that slug under the current project; `--resume-name` in the wrapper is the prior-identity resume path.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"model":   {Type: "string", Description: "Your model, as a short slug: opus4.7, sonnet4.7, haiku4.5, gpt5, etc. Use 'unknown' if you genuinely cannot determine it."},
				"harness": {Type: "string", Description: "Your harness slug: claude-code, codex, cursor, cline, aider, pi, etc. Pass this explicitly (you know what harness you run in, just like you know your model). The server falls back to AIMEBU_HARNESS env var, then a few upstream env-var heuristics — but those don't cover every harness, so don't rely on them."},
				"meta":    {Type: "object", Description: "Optional extra metadata (cwd, branch, repo, etc. are auto-filled)."},
				"name":    {Type: "string", Description: "Only with force=true: force-claim this slug in the current project. Must match ^[a-z]{3,12}$. Rejected if the same full ID is held by an AI with different model/harness/project."},
				"force":   {Type: "boolean", Description: "Set to true together with `name` to force-claim a project-scoped slug. Leave false (default) to let the server pick a slug — this is the normal case."},
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
	{
		Name:        "bus_message",
		Description: "Fetch a single message by its global ID. You must be a member of the message's room; otherwise returns not_found. Use this when a user or agent references #NN in a message — call bus_message(id) to fetch the original content. Message IDs are shown as #NN badges in the web UI and appear as the `id` field in bus_read / bus_wait responses.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"id": {Type: "integer", Description: "Global message ID (the number from #NN)"},
			},
			Required: []string{"id"},
		},
	},
	{
		Name:        "bus_react",
		Description: "Add or remove a single-emoji reaction on an existing message. Use this instead of posting text-only acknowledgements such as \"got it\" or \"on it\". Recommended convention: 👍/🆗 = seen/ack, ✅ = done, 👀 = looking, 🙏 = thanks. Reactions create no chat message, do not advance read cursors, and do not trigger human attention.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"message_id": {Type: "integer", Description: "Global message ID to react to."},
				"emoji":      {Type: "string", Description: "A single emoji reaction."},
				"remove":     {Type: "boolean", Description: "Remove the reaction instead of adding it."},
			},
			Required: []string{"message_id", "emoji"},
		},
	},
	{
		Name:        "bus_macros_get",
		Description: "Fetch all macros. Returns {macros: {key: body}}. The web composer expands <KEY> entries using these definitions when selected from autocomplete; the server stores message bodies verbatim.",
		InputSchema: inputSchema{
			Type:       "object",
			Properties: map[string]property{},
		},
	},
	{
		Name:        "bus_macros_set",
		Description: "Update the global macro map (broadcast to all clients). Pass an explicit empty map {} to clear all macros. Full replace: included keys replace the entire scope, so fetch first with bus_macros_get if you only want to add or remove one key.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"macros": {Type: "object", Description: "Global macro map: {key: body}"},
			},
		},
	},
	{
		Name:        "bus_memory_list",
		Description: "List curated bus memory visible to you. Returns records plus a rendered bounded snapshot and usage/cap metadata. Omit scope to get your startup-visible memory: project_facts for your project, user_profile records, and global agent_shared_notes. If memory is disabled, proceed without memory rather than retrying.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"scope":     {Type: "string", Description: "Optional scope: project_facts, user_profile, or agent_shared_notes."},
				"scope_key": {Type: "string", Description: "Optional key inside the scope. Required for user_profile when the caller is an AI and wants one human profile."},
			},
		},
	},
	{
		Name:        "bus_memory_add",
		Description: "Add one curated memory record with explicit intent. Writes are cap-enforced and attributable. project_facts requires a non-empty caller project; user_profile requires scope_key with the human ID; agent_shared_notes is one global shared bucket visible to all agents, so keep those notes concise and generally applicable. Before recording a durable project convention, consider whether it belongs in the project's AGENTS.md/CLAUDE.md instead because those files are version-controlled and reviewed; ask the human when promotion to repo docs seems appropriate. If source_message_id points to a room whose memory is disabled, the write is rejected.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"scope":             {Type: "string", Description: "Scope: project_facts, user_profile, or agent_shared_notes."},
				"scope_key":         {Type: "string", Description: "Optional key inside the scope. For user_profile, pass the human ID such as 'matin'."},
				"body":              {Type: "string", Description: "Memory body to add."},
				"source_message_id": {Type: "integer", Description: "Optional source message ID that motivated this memory."},
			},
			Required: []string{"scope", "body"},
		},
	},
	{
		Name:        "bus_memory_update",
		Description: "Update one memory record by id. Requires the version you saw as an optimistic concurrency guard; if stale, the server returns the fresh record instead of overwriting blindly.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"id":      {Type: "string", Description: "Memory record ID."},
				"version": {Type: "integer", Description: "The record version you saw. Required to prevent blind overwrites."},
				"body":    {Type: "string", Description: "Replacement body."},
			},
			Required: []string{"id", "version", "body"},
		},
	},
	{
		Name:        "bus_memory_remove",
		Description: "Remove one memory record by id and version. Use only when the record is wrong, stale, or consolidated elsewhere.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"id":      {Type: "string", Description: "Memory record ID."},
				"version": {Type: "integer", Description: "The record version you saw. Required to prevent blind deletes."},
			},
			Required: []string{"id", "version"},
		},
	},
	{
		Name:        "bus_recall",
		Description: "Read-only keyword recall over messages visible to you, skipping rooms whose memory is disabled. Returns a small ranked list with message metadata and snippets. It does not summarize, create memory, or advance read cursors.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"query": {Type: "string", Description: "Search query."},
				"limit": {Type: "integer", Description: "Maximum results, capped by the server."},
			},
			Required: []string{"query"},
		},
	},
	{
		Name:        "bus_role_assign",
		Description: "Assign or change a role for an AI agent in a room. Emits a concise addressed system message; use bus_role_get for the full instructions. Pass empty role_key to unassign.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"room":     {Type: "string", Description: "Room ID"},
				"agent_id": {Type: "string", Description: "Full agent ID to assign (e.g. 'alice@aimebu')"},
				"role_key": {Type: "string", Description: "Role key (e.g. 'leader', 'worker', 'reviewer'). Empty string to unassign."},
			},
			Required: []string{"room", "agent_id", "role_key"},
		},
	},
	{
		Name:        "bus_role_get",
		Description: "Get your currently assigned role in a room, including the key, emoji, and full resolved role instructions.",
		InputSchema: inputSchema{
			Type: "object",
			Properties: map[string]property{
				"room": {Type: "string", Description: "Room ID"},
			},
			Required: []string{"room"},
		},
	},
}

// ── Dynamic prompts ────────────────────────────────────────────────

// fetchPrompts fetches all configured prompt bodies from the running server
// and returns a key→body map. Returns nil on any error; callers should fall
// back to compiled defaults when nil.
func fetchPrompts(c *client.Client) map[string]string {
	body, err := c.Get("/settings/prompts")
	if err != nil {
		return nil
	}
	var entries []struct {
		Key  string `json:"key"`
		Body string `json:"body"`
	}
	if json.Unmarshal([]byte(body), &entries) != nil {
		return nil
	}
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		m[e.Key] = e.Body // empty string is a valid user override (blanks the prompt)
	}
	return m
}

// promptVal returns the value for key from the prompts map, falling back to
// fallback when the map is nil or the key is absent.
func promptVal(prompts map[string]string, key, fallback string) string {
	if prompts != nil {
		if v, ok := prompts[key]; ok {
			return v
		}
	}
	return fallback
}

// buildTools returns the tools slice with descriptions overridden from prompts.
func buildTools(prompts map[string]string) []tool {
	out := make([]tool, len(tools))
	for i, t := range tools {
		t.Description = promptVal(prompts, "tool."+t.Name, t.Description)
		out[i] = t
	}
	return out
}

// ── Tool handlers ──────────────────────────────────────────────────

// notRegisteredError builds a user-facing error telling the caller to run
// bus_register first. Uses the configured error message if available.
func notRegisteredError(c *client.Client) error {
	const fallback = "not registered — call bus_register first. Pass your model (e.g. 'opus4.7', 'sonnet4.7') and the server will assign you a name"
	return fmt.Errorf("%s", promptVal(c.Prompts, "error.not_registered", fallback))
}

func handleToolCall(c *client.Client, name string, args json.RawMessage) (string, error) {
	// All tools except bus_register require a registered identity.
	if name != "bus_register" && name != "bus_agents" && c.AgentID == "" {
		return "", notRegisteredError(c)
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
		resp, err := c.Post("/rooms/"+p.Room+"/join", map[string]string{
			"agent_id": c.AgentID,
		})
		if err != nil {
			return resp, err
		}
		// Enrich with your_role if the agent has a role in this room
		var room struct {
			Roles map[string]string `json:"roles"`
		}
		if json.Unmarshal([]byte(resp), &room) == nil && len(room.Roles) > 0 {
			if roleKey, ok := room.Roles[c.AgentID]; ok && roleKey != "" {
				roleResp, err2 := c.Get("/rooms/" + p.Room + "/roles/" + c.AgentID)
				if err2 == nil {
					var roleInfo struct {
						Key   string `json:"role_key"`
						Label string `json:"label"`
						Icon  string `json:"icon"`
						Emoji string `json:"emoji"`
						Body  string `json:"body"`
					}
					if json.Unmarshal([]byte(roleResp), &roleInfo) == nil && roleInfo.Key != "" {
						var fullRoom map[string]any
						if json.Unmarshal([]byte(resp), &fullRoom) == nil {
							fullRoom["your_role"] = map[string]string{
								"key":   roleInfo.Key,
								"label": roleInfo.Label,
								"icon":  roleInfo.Icon,
								"emoji": roleInfo.Emoji,
								"body":  roleInfo.Body,
							}
							if enriched, err3 := json.Marshal(fullRoom); err3 == nil {
								return string(enriched), nil
							}
						}
					}
				}
			}
		}
		return resp, nil

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
			Room            string               `json:"room"`
			Body            string               `json:"body"`
			NeedsAttention  bool                 `json:"needs_attention"`
			ProposedAnswers []string             `json:"proposed_answers"`
			OpenQuestions   []types.OpenQuestion `json:"open_questions"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		if strings.HasPrefix(p.Room, "_") {
			return "", fmt.Errorf("room %q is read-only (system room)", p.Room)
		}
		return c.Post("/rooms/"+p.Room+"/send", map[string]any{
			"from":             c.AgentID,
			"body":             p.Body,
			"needs_attention":  p.NeedsAttention,
			"proposed_answers": p.ProposedAnswers,
			"open_questions":   p.OpenQuestions,
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
			To              string               `json:"to"`
			Body            string               `json:"body"`
			NeedsAttention  bool                 `json:"needs_attention"`
			ProposedAnswers []string             `json:"proposed_answers"`
			OpenQuestions   []types.OpenQuestion `json:"open_questions"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		return c.Post("/dm", map[string]any{
			"from":             c.AgentID,
			"to":               p.To,
			"body":             p.Body,
			"needs_attention":  p.NeedsAttention,
			"proposed_answers": p.ProposedAnswers,
			"open_questions":   p.OpenQuestions,
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

	case "bus_message":
		var p struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(args, &p)
		if p.ID <= 0 {
			return "", fmt.Errorf("id is required and must be a positive integer")
		}
		return c.Message(p.ID)

	case "bus_react":
		var p struct {
			MessageID int64  `json:"message_id"`
			Emoji     string `json:"emoji"`
			Remove    bool   `json:"remove"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		if p.MessageID <= 0 {
			return "", fmt.Errorf("message_id is required and must be a positive integer")
		}
		if strings.TrimSpace(p.Emoji) == "" {
			return "", fmt.Errorf("emoji is required")
		}
		return c.React(p.MessageID, p.Emoji, p.Remove)

	case "bus_macros_get":
		return c.Get("/macros")

	case "bus_macros_set":
		var p struct {
			Macros map[string]string `json:"macros"`
		}
		_ = json.Unmarshal(args, &p)
		return c.Put("/macros", p)

	case "bus_memory_list":
		var p struct {
			Scope    string `json:"scope"`
			ScopeKey string `json:"scope_key"`
		}
		_ = json.Unmarshal(args, &p)
		q := url.Values{}
		q.Set("agent_id", c.AgentID)
		if p.Scope != "" {
			q.Set("scope", p.Scope)
		}
		if p.ScopeKey != "" {
			q.Set("scope_key", p.ScopeKey)
		}
		return c.Get("/memory?" + q.Encode())

	case "bus_memory_add":
		var p struct {
			Scope           string `json:"scope"`
			ScopeKey        string `json:"scope_key"`
			Body            string `json:"body"`
			SourceMessageID int64  `json:"source_message_id"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		return c.Post("/memory", map[string]any{
			"agent_id":          c.AgentID,
			"scope":             p.Scope,
			"scope_key":         p.ScopeKey,
			"body":              p.Body,
			"source_message_id": p.SourceMessageID,
		})

	case "bus_memory_update":
		var p struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
			Body    string `json:"body"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		return c.Put("/memory/"+url.PathEscape(p.ID), map[string]any{
			"agent_id": c.AgentID,
			"version":  p.Version,
			"body":     p.Body,
		})

	case "bus_memory_remove":
		var p struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		q := url.Values{}
		q.Set("agent_id", c.AgentID)
		q.Set("version", fmt.Sprintf("%d", p.Version))
		return c.Delete("/memory/" + url.PathEscape(p.ID) + "?" + q.Encode())

	case "bus_recall":
		var p struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		q := url.Values{}
		q.Set("agent_id", c.AgentID)
		q.Set("query", p.Query)
		if p.Limit > 0 {
			q.Set("limit", fmt.Sprintf("%d", p.Limit))
		}
		return c.Get("/recall?" + q.Encode())

	case "bus_role_assign":
		var p struct {
			Room    string `json:"room"`
			AgentID string `json:"agent_id"`
			RoleKey string `json:"role_key"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		return c.Post("/rooms/"+p.Room+"/roles", map[string]any{
			"agent_id": p.AgentID,
			"role_key": p.RoleKey,
		})

	case "bus_role_get":
		var p struct {
			Room string `json:"room"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		return c.Get("/rooms/" + p.Room + "/roles/" + c.AgentID)

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// ── JSON-RPC dispatch ──────────────────────────────────────────────

func handle(c *client.Client, req request) *response {
	switch req.Method {
	case "initialize":
		// Fetch configured prompts once per MCP connection and cache them.
		// Falls back to compiled defaults if the server is unreachable.
		c.Prompts = fetchPrompts(c)
		return &response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "aimebu", "version": "2.0.0"},
				"instructions":    promptVal(c.Prompts, "bus_etiquette", busEtiquette),
			},
		}

	case "notifications/initialized":
		return nil

	case "tools/list":
		return &response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{"tools": buildTools(c.Prompts)},
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
			"bus_react": true,
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

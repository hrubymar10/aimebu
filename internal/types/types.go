package types

type Room struct {
	ID        string            `json:"id"`
	Members   []string          `json:"members"`
	CreatedAt string            `json:"created_at"`
	CreatedBy string            `json:"created_by"`
	Roles     map[string]string `json:"roles,omitempty"` // agent_id → role_key
}

// RoleInfo is the role data embedded in bus_join responses when the joining
// agent already has a role assigned in the room.
type RoleInfo struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Emoji string `json:"emoji,omitempty"`
	Icon  string `json:"icon,omitempty"`
	Body  string `json:"body"`
}

type Message struct {
	ID                  int64    `json:"id"`
	RoomID              string   `json:"room_id"`
	From                string   `json:"from"`
	FromKind            string   `json:"from_kind,omitempty"` // "ai", "human", or "system" — empty for legacy persisted messages
	Body                string   `json:"body"`
	CreatedAt           string   `json:"created_at"`
	Targets             []string `json:"targets"`
	NeedsHumanAttention bool     `json:"needs_human_attention,omitempty"`
}

type Agent struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`              // "ai" or "human"
	Model        string            `json:"model,omitempty"`   // always present for kind=ai (may be "unknown")
	Harness      string            `json:"harness,omitempty"` // always present for kind=ai (may be "unknown")
	Project      string            `json:"project,omitempty"`
	Meta         map[string]string `json:"meta,omitempty"`
	Warnings     []string          `json:"warnings,omitempty"`
	RegisteredAt string            `json:"registered_at"`
	LastSeen     string            `json:"last_seen"`
	// ReadCursors tracks the highest message ID this agent has read per room.
	// Populated by bus_wait (implicit, on delivery) and POST /agents/{id}/read
	// (explicit, e.g. frontend "user opened the room"). Persisted with the
	// agent so a pruned-then-reclaimed identity keeps its read state.
	ReadCursors map[string]int64 `json:"read_cursors,omitempty"`
}

// Request types

type CreateRoomRequest struct {
	ID        string `json:"id,omitempty"`
	CreatedBy string `json:"created_by"`
}

type JoinRequest struct {
	AgentID string `json:"agent_id"`
}

type LeaveRequest struct {
	AgentID string `json:"agent_id"`
	Kicked  bool   `json:"kicked,omitempty"`
}

type RoomSendRequest struct {
	From           string `json:"from"`
	Body           string `json:"body"`
	NeedsAttention bool   `json:"needs_attention,omitempty"`
}

type DMRequest struct {
	From           string `json:"from"`
	To             string `json:"to"`
	Body           string `json:"body"`
	NeedsAttention bool   `json:"needs_attention,omitempty"`
}

// RegisterRequest is used by MCP clients. The server assigns the name and
// assembles the ID. Kind must be "ai". Model and Harness may be "unknown"
// but must be present (the server normalizes missing values to "unknown").
//
// For kind=ai, `Name` is normally ignored (server picks from a pool). Setting
// Force=true together with Name force-claims that slug in the current project:
// idempotent if the AI with the same model/harness/project already holds the
// assembled full ID, rejected if that same full ID is held by a different AI.
type RegisterRequest struct {
	Kind    string            `json:"kind"`              // "ai" or "human"
	Name    string            `json:"name,omitempty"`    // always used for human; only honored for ai when Force=true
	Model   string            `json:"model,omitempty"`   // only for kind=ai
	Harness string            `json:"harness,omitempty"` // only for kind=ai
	Project string            `json:"project,omitempty"` // included in ID (e.g. @aimebu)
	Meta    map[string]string `json:"meta,omitempty"`    // additional context (cwd, branch, repo, etc.)
	Force   bool              `json:"force,omitempty"`   // force-claim an ai slug by Name in Project (only honored for kind=ai)
}

// AgentRoomView is a room as seen by a specific agent: the room plus that
// agent's unread-message count and latest-message id for it.
type AgentRoomView struct {
	Room
	UnreadCount          int   `json:"unread_count"`
	AttentionUnreadCount int   `json:"attention_unread_count"`
	LastID               int64 `json:"last_id"`
	ReadCursor           int64 `json:"read_cursor"`
}

// MarkReadRequest is the body for POST /agents/{id}/read.
type MarkReadRequest struct {
	Room      string `json:"room"`
	MessageID int64  `json:"message_id"` // highest ID the agent has now seen; 0 means "current HEAD"
}

// MemberPresence is the per-agent presence state embedded in
// GET /rooms/{id} responses under `members_presence`.
//   - Cursor: highest message ID this agent has received in this room (0 if
//     unknown)
//   - Waiting: whether this agent currently has any open bus_wait long-poll
//     (true → listening live; false → not blocked on the bus)
type MemberPresence struct {
	Agent   string `json:"agent"`
	Cursor  int64  `json:"cursor"`
	Waiting bool   `json:"waiting"`
}

// RegisterResponse carries the assembled ID back to the client.
type RegisterResponse struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Kind      string            `json:"kind"`
	Model     string            `json:"model,omitempty"`
	Harness   string            `json:"harness,omitempty"`
	Project   string            `json:"project,omitempty"`
	Meta      map[string]string `json:"meta,omitempty"`
	Reclaimed bool              `json:"reclaimed"`
	Warnings  []string          `json:"warnings,omitempty"`
}

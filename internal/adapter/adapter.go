package adapter

import (
	"context"
	"errors"
	"io"

	"github.com/goccy/go-json"
)

type Mode string

const (
	ModeNormal     Mode = "normal"
	ModeCorrection Mode = "correction"
	ModeDiagnostic Mode = "diagnostic"
)

type AgentSpec struct {
	ID         string `json:"id"`
	Tool       string `json:"tool"`
	Variant    string `json:"variant"`
	LauncherID string `json:"launcher_id,omitempty"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
}

// TurnSpec is the complete, immutable input to one process-per-wake turn.
// NotesJSON and ContextJSON are wire-shaped JSON; the adapter must not decide
// room access or validate the final answer's target rooms.
type TurnSpec struct {
	TurnID                string
	Agent                 AgentSpec
	NativeSessionHandle   string
	NativeSessionStarted  bool
	CheckoutPath          string
	Mode                  Mode
	NotesJSON             json.RawMessage
	ContextJSON           json.RawMessage
	AllowedTools          []string
	AgentToolsConfigPath  string
	LauncherPath          string
	LauncherArgs          []string
	LauncherEnvRefs       map[string]string
	LauncherContainerized bool
	VisibleCheckoutPath   string
	AgentServerURL        string
	// These are process-only capabilities for Pi Docker. They must never enter
	// the supervisor journal or its persisted turn spec.
	AttachmentBaseURL string `json:"-"`
	AttachmentBearer  string `json:"-"`
	// StderrSink, when set, receives everything the harness writes to stderr, as it is written (the per-agent output log). It is
	// supervisor-owned and process-only like the capabilities above: it is never journaled, and the adapter tees into it beside the tail
	// it keeps as exit evidence. A sink that fails changes nothing for the turn.
	StderrSink io.Writer `json:"-"`
}

type EventKind string

const (
	EventActivity             EventKind = "activity"
	EventUsage                EventKind = "usage"
	EventCompaction           EventKind = "compaction"
	EventFinalAnswer          EventKind = "final_answer"
	EventProcessExit          EventKind = "process_exit"
	EventNativeSessionStarted EventKind = "native_session_started"
)

type Usage struct {
	TokensIn      *int64   `json:"tokens_in,omitempty"`
	TokensOut     *int64   `json:"tokens_out,omitempty"`
	ContextUsed   *int64   `json:"context_used,omitempty"`
	ContextWindow *int64   `json:"context_window,omitempty"`
	CostEstimate  *float64 `json:"cost_estimate,omitempty"`
	// TokensCacheRead and TokensCacheWrite are the tokens served from and written to the prompt cache, where the harness reports them.
	TokensCacheRead  *int64 `json:"tokens_cache_read,omitempty"`
	TokensCacheWrite *int64 `json:"tokens_cache_write,omitempty"`
	// Model is the model the harness says it used, when it says; empty means the agent's configured model.
	Model string `json:"model,omitempty"`
	// CostCumulative says CostEstimate is the harness session's total so far (Claude reports it so, and a resumed session carries the
	// earlier spend), not this turn's: the store turns it into the turn's own cost by subtracting the session's previous total.
	CostCumulative bool `json:"cost_cumulative,omitempty"`
	// Partial marks what a turn spent before it ended without a final report (killed, crashed, cancelled): what was seen, not a total.
	Partial bool `json:"partial,omitempty"`
}

type Compaction struct {
	PreTokens  int64 `json:"pre_tokens"`
	PostTokens int64 `json:"post_tokens"`
}

// ExitInfo is execution evidence, not answer settlement. ExitCode 0 does not
// imply a terminal event or a committed final answer.
type ExitInfo struct {
	ExitCode      int    `json:"exit_code"`
	TerminalEvent bool   `json:"terminal_event"`
	CancelIntent  bool   `json:"cancel_intent"`
	StderrTail    string `json:"stderr_tail,omitempty"`
	ErrorEvent    string `json:"error_event,omitempty"`
}

// ToolEvent marks a tool execution (shell command, MCP call, ...) beginning
// or ending inside a turn. The supervisor uses it to tell a silent but busy
// harness from a hung one; ID pairs a start with its end when the harness
// provides one.
type ToolEvent struct {
	ID   string `json:"id,omitempty"`
	Done bool   `json:"done,omitempty"`
}

type Event struct {
	Kind          EventKind   `json:"kind"`
	SourceEventID string      `json:"source_event_id"`
	Summary       string      `json:"summary,omitempty"`
	Usage         *Usage      `json:"usage,omitempty"`
	Compaction    *Compaction `json:"compaction,omitempty"`
	// Answer is the raw candidate from the tool's dedicated final-answer event.
	// Malformed JSON stays raw for the server's correction/held path.
	Answer              json.RawMessage `json:"answer,omitempty"`
	Exit                *ExitInfo       `json:"exit,omitempty"`
	NativeSessionHandle string          `json:"native_session_handle,omitempty"`
	Tools               []ToolEvent     `json:"tools,omitempty"`
}

type Turn interface {
	Events() <-chan Event
	Cancel() error
	Wait() (ExitInfo, error)
}

type Environment struct {
	Variant   string `json:"variant"`
	OS        string `json:"os"`
	Container bool   `json:"container"`
}

type Capabilities struct {
	SchemaFlag       bool `json:"schema_flag"`
	NoActionMode     bool `json:"no_action_mode"`
	ReadonlyVerified bool `json:"readonly_verified"`
	CompactionEvents bool `json:"compaction_events"`
	ContextWindow    bool `json:"context_window"`
	Cost             bool `json:"cost"`
	CancelConfirmed  bool `json:"cancel_confirmed"`
}

type Adapter interface {
	Start(context.Context, TurnSpec) (Turn, error)
	Catalog() []CatalogEntry
	Capabilities(version string, environment Environment) Capabilities
}

type CatalogEntry struct {
	Tool        string
	Version     string
	Environment Environment
}

var ErrNoScript = errors.New("no fake adapter script for turn")
var ErrTurnAlreadyStarted = errors.New("fake adapter turn already started")

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

// CustomProvider is an OpenAI-compatible endpoint an OpenCode turn runs its model on: the owner's setting for the harness, carried with
// the turn because the supervisor holds none of the server's settings. KeyEnv is the NAME of the environment variable the key is read
// from on the machine that runs the turn; no key value is ever part of it. Models are the harness's models of this provider (their ids
// without the "<id>/" prefix).
type CustomProvider struct {
	ID            string                `json:"id"`
	Name          string                `json:"name"`
	NPM           string                `json:"npm"`
	BaseURL       string                `json:"base_url"`
	KeyEnv        string                `json:"key_env,omitempty"`
	ContextTokens int                   `json:"context_tokens,omitempty"`
	OutputTokens  int                   `json:"output_tokens,omitempty"`
	Models        []CustomProviderModel `json:"models"`
}

// CustomProviderModel is one model of a CustomProvider: its id at the endpoint and the label the owner gave it.
type CustomProviderModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TurnSpec is the complete, immutable input to one process-per-wake turn.
// NotesJSON and ContextJSON are wire-shaped JSON; the adapter must not decide
// room access or validate the final answer's target rooms.
type TurnSpec struct {
	TurnID               string
	Agent                AgentSpec
	NativeSessionHandle  string
	NativeSessionStarted bool
	// ForkSession resumes NativeSessionHandle as a copy (Claude's --fork-session): the harness saves it under a new id, which the turn
	// reports in its native_session_started event.
	ForkSession bool
	// KeepAlive marks a turn that is not work: it keeps the harness's prompt cache warm and nothing else. The adapter asks the model to answer with
	// no messages, and the supervisor throws the answer away; no turn exists for it on the server. Never journaled. Only an adapter that is a
	// KeepAliver runs one.
	KeepAlive    bool `json:"-"`
	CheckoutPath string
	// LaunchPath, when set, is the folder the harness is started in instead of CheckoutPath: another spelling of the same directory, which a
	// Claude Code session was saved under. The supervisor sets it only after checking it resolves to CheckoutPath.
	LaunchPath            string
	Mode                  Mode
	NotesJSON             json.RawMessage
	ContextJSON           json.RawMessage
	AllowedTools          []string
	LauncherPath          string
	LauncherArgs          []string
	LauncherEnvRefs       map[string]string
	LauncherContainerized bool
	// LauncherCodexHome is the running Docker container's CODEX_HOME, reported
	// by the cached launcher feature probe. Empty means warm Codex is unsafe.
	LauncherCodexHome   string
	VisibleCheckoutPath string
	AgentServerURL      string
	// Providers are the custom providers the turn's model needs (OpenCode only): the one provider its model id names, or none.
	Providers []CustomProvider
	// AgentTools is how the turn reaches the bus: the agent's own key, never anyone else's. It is process-only like the capabilities below: it
	// is never journaled and no file holds it; an adapter hands the key to the harness through the harness's environment.
	AgentTools *AgentToolsAccess `json:"-"`
	// These are process-only capabilities for Pi Docker. They must never enter
	// the supervisor journal or its persisted turn spec.
	AttachmentBaseURL string `json:"-"`
	AttachmentBearer  string `json:"-"`
	// StderrSink, when set, receives everything the harness writes to stderr, as it is written (the per-agent output log). It is
	// supervisor-owned and process-only like the capabilities above: it is never journaled, and the adapter tees into it beside the tail
	// it keeps as exit evidence. A sink that fails changes nothing for the turn.
	StderrSink io.Writer `json:"-"`
}

// AgentToolsAccess is what a turn needs to reach the bus as one agent. The supervisor builds it per turn from the key it holds in memory.
type AgentToolsAccess struct {
	AgentID   string
	ServerURL string
	Key       string // the agent's own aak_ key
	// Command is the aimebu executable that runs the stdio bridge (`aimebu agent-tools`).
	Command string
	// Env is the rest of the bridge's environment (the telemetry variables, the insecure-TLS escape hatch); never the key.
	Env map[string]string
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
	// CostSource is "estimated" when the price list, rather than the harness,
	// supplied CostEstimate. An empty value means a harness-reported cost.
	CostSource string `json:"cost_source,omitempty"`
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
	// ErrorSummary is what the harness said was wrong, as one readable sentence (Claude Code's result text of an is_error result), where
	// ErrorEvent is the raw event it came from. DefiniteFailure says the turn failed before it did anything: an explicit error result
	// with no tool run and no output, such as a failed sign-in. Nothing was done, so Retry is safe and a diagnostic or Resume has nothing
	// to find. Both are empty for a harness that does not say.
	ErrorSummary    string `json:"error_summary,omitempty"`
	DefiniteFailure bool   `json:"definite_failure,omitempty"`
	// StartFailure identifies a harness that never launched, which gives recovery a specific action and explanation.
	StartFailure bool `json:"start_failure,omitempty"`
	// ProcessAlive says the turn ended and its process did not: a warm process that was kept idle for the next turn. The exit then is the turn's end
	// (a "turn finished"), not the process's; the process's own end is reported when it happens.
	ProcessAlive bool `json:"process_alive,omitempty"`
	// LimitStall marks an exit the supervisor's own turn-limit watchdog caused (the idle deadline or the wall-clock cap), not a
	// harness failure or an owner cancel. The server then continues the turn once on its own instead of asking a person.
	LimitStall bool `json:"limit_stall,omitempty"`
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

// KeepAliver is implemented by an adapter that can run a keep-alive turn (TurnSpec.KeepAlive): the same process arguments as a real turn (so the same
// cached prefix), and a user turn that only asks the model to reply with nothing.
type KeepAliver interface {
	KeepAliveTool() string
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

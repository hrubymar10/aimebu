package protocol

import (
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
)

const Version = 2
const MaxMachineFrameBytes = 4 << 20

const (
	Hello                = "hello"
	HelloAck             = "hello_ack"
	Ping                 = "ping"
	Pong                 = "pong"
	Deliver              = "deliver"
	Ack                  = "ack"
	Activity             = "activity"
	Usage                = "usage"
	Compaction           = "compaction"
	NativeSessionStarted = "native_session_started"
	Answer               = "answer"
	Process              = "process"
	Receipt              = "receipt"
	Spawn                = "spawn"
	SpawnResult          = "spawn_result"
	Control              = "control"
	Error                = "error"
	HarnessSet           = "harness_set"
	HarnessResult        = "harness_result"
	HarnessStatus        = "harness_status"
	LauncherValidate     = "launcher_validate"
	LauncherSet          = "launcher_set"
	LauncherResult       = "launcher_result"
	LauncherStatus       = "launcher_status"
	Detect               = "detect"
	DetectResult         = "detect_result"
	PathProbe            = "path_probe"
	PathResult           = "path_result"
	WorktreeOp           = "worktree_op"
	WorktreeResult       = "worktree_result"
	AgentLogRead         = "agent_log_read"
	AgentLogResult       = "agent_log_result"
	AgentLogUsage        = "agent_log_usage"
	AgentLogUsageResult  = "agent_log_usage_result"
	SessionScan          = "session_scan"
	SessionScanResult    = "session_scan_result"
)

// Frame is the versioned machine socket envelope. Agent frames carry both
// agent_id and generation; machine frames carry machine_id. The server binds
// the socket from its authenticated machine key, never from frame claims.
type Frame struct {
	V                   int    `json:"v"`
	Type                string `json:"type"`
	MachineID           string `json:"machine_id,omitempty"`
	JournalID           string `json:"journal_id,omitempty"`
	SupervisorVersion   string `json:"supervisor_version,omitempty"`
	SupportsPathProbe   bool   `json:"supports_path_probe,omitempty"`
	SupportsSessionScan bool   `json:"supports_session_scan,omitempty"`
	SupportsSessionFork bool   `json:"supports_session_fork,omitempty"`
	// SupportsSessionAttribution says the supervisor answers a session scan that asks for attribution (ScanAttribution).
	SupportsSessionAttribution bool                 `json:"supports_session_attribution,omitempty"`
	SupportsLaunchPath         bool                 `json:"supports_launch_path,omitempty"`
	SupportsWorktree           bool                 `json:"supports_worktree,omitempty"`
	SupportsLaunchers          bool                 `json:"supports_launchers,omitempty"`
	SupportsDetect             bool                 `json:"supports_detect,omitempty"`
	SupportsDetectDir          bool                 `json:"supports_detect_dir,omitempty"`
	SupportsAgentLogs          bool                 `json:"supports_agent_logs,omitempty"`
	SupportsAgentLogSize       bool                 `json:"supports_agent_log_size,omitempty"`
	SupportsStopReplay         bool                 `json:"supports_stop_replay,omitempty"`
	Capabilities               []CapabilityReport   `json:"capabilities,omitempty"`
	Harnesses                  []HarnessRow         `json:"harnesses,omitempty"`
	HarnessSettings            []HarnessSetting     `json:"harness_settings,omitempty"`
	HarnessProbeDir            string               `json:"harness_probe_dir,omitempty"`
	Harness                    *HarnessRow          `json:"harness,omitempty"`
	HarnessSetting             *HarnessSetting      `json:"harness_setting,omitempty"`
	Launchers                  []LauncherRow        `json:"launchers,omitempty"`
	LauncherDefinitions        []LauncherDefinition `json:"launcher_definitions,omitempty"`
	LauncherDefinition         *LauncherDefinition  `json:"launcher_definition,omitempty"`
	Launcher                   *LauncherRow         `json:"launcher,omitempty"`
	Detect                     *DetectRequest       `json:"detect,omitempty"`
	HarnessCandidates          []HarnessCandidate   `json:"harness_candidates,omitempty"`
	ModelCandidates            []ModelCandidate     `json:"model_candidates,omitempty"`
	DetectDir                  string               `json:"detect_dir,omitempty"`
	Agents                     []HelloAgent         `json:"agents,omitempty"`
	Desired                    []DesiredAgent       `json:"desired,omitempty"`
	Redeliver                  []Delivery           `json:"redeliver,omitempty"`
	ResendReceipts             []ReceiptBody        `json:"resend_receipts,omitempty"`
	AgentID                    string               `json:"agent_id,omitempty"`
	LauncherID                 string               `json:"launcher_id,omitempty"`
	ReqID                      string               `json:"req_id,omitempty"`
	Generation                 int                  `json:"generation,omitempty"`
	DesiredRev                 int64                `json:"desired_rev,omitempty"`
	DesiredState               string               `json:"desired_state,omitempty"`
	AgentKey                   string               `json:"agent_key,omitempty"`
	CheckoutPath               string               `json:"checkout_path,omitempty"`
	CanonicalPath              string               `json:"canonical_path,omitempty"`
	RoomID                     string               `json:"room_id,omitempty"`
	BaseRef                    string               `json:"base_ref,omitempty"`
	WorktreeAction             string               `json:"worktree_action,omitempty"`
	WorktreeForce              bool                 `json:"worktree_force,omitempty"`
	LogLines                   int                  `json:"log_lines,omitempty"`
	LogMaxBytes                int                  `json:"log_max_bytes,omitempty"`
	AttachmentBearer           string               `json:"attachment_bearer,omitempty"`
	TurnID                     string               `json:"turn_id,omitempty"`
	Mode                       adapter.Mode         `json:"mode,omitempty"`
	ParentTurnID               string               `json:"parent_turn_id,omitempty"`
	Notes                      []DeliveryNote       `json:"notes,omitempty"`
	Context                    json.RawMessage      `json:"context,omitempty"`
	AllowedTools               []string             `json:"allowed_tools,omitempty"`
	Agent                      *adapter.AgentSpec   `json:"agent,omitempty"`
	NativeSessionHandle        string               `json:"native_session,omitempty"`
	NativeSessionStarted       bool                 `json:"native_session_started,omitempty"`
	// ForkSession says this turn resumes NativeSessionHandle as a copy: the harness saves it under a new id, which the turn reports as its
	// native session.
	ForkSession bool `json:"fork_session,omitempty"`
	// LaunchPath is the spelling of CheckoutPath a Claude Code session was made under, when that differs from it (a link to the folder):
	// Claude keys its sessions by the folder it is started in, so the harness is started there, not in the canonical path.
	LaunchPath     string              `json:"launch_path,omitempty"`
	Level          string              `json:"level,omitempty"`
	Ref            string              `json:"ref,omitempty"`
	SourceEventID  string              `json:"source_event_id,omitempty"`
	Kind           string              `json:"kind,omitempty"`
	Summary        string              `json:"summary,omitempty"`
	Usage          *adapter.Usage      `json:"usage,omitempty"`
	Compaction     *adapter.Compaction `json:"compaction,omitempty"`
	AnswerRev      int                 `json:"answer_rev,omitempty"`
	Digest         string              `json:"digest,omitempty"`
	Candidate      string              `json:"candidate,omitempty"`
	AnswerTooLarge bool                `json:"answer_too_large,omitempty"`
	Exit           *adapter.ExitInfo   `json:"exit,omitempty"`
	Result         string              `json:"result,omitempty"`
	Details        json.RawMessage     `json:"details,omitempty"`
	// ScanFolders (spellings of one folder: every Claude Code session that worked in it or below) or ScanSessionID (that one session,
	// wherever it worked) asks a machine for its Claude Code sessions; the answer lists them in Sessions.
	ScanFolders   []string `json:"scan_folders,omitempty"`
	ScanSessionID string   `json:"scan_session_id,omitempty"`
	// ScanAttribution asks, for the sessions of ScanFolders written at or after ScanSince (RFC 3339, empty for all), which agent each
	// registered as and which message numbers it sent (only ids are returned, never text), for the sessions of ScanAgent when it is set.
	ScanAttribution bool                 `json:"scan_attribution,omitempty"`
	ScanAgent       string               `json:"scan_agent,omitempty"`
	ScanSince       string               `json:"scan_since,omitempty"`
	Attributions    []SessionAttribution `json:"attributions,omitempty"`
	Sessions        []SessionCandidate   `json:"sessions,omitempty"`
	// Unreadable counts transcripts of the scanned folders that could not be read far enough to say whose they are; ScanTruncated says the
	// scan stopped at its limits and Sessions are the newest. A scan names the launcher whose Claude home it reads in LauncherID.
	Unreadable    int    `json:"unreadable,omitempty"`
	ScanTruncated bool   `json:"scan_truncated,omitempty"`
	Code          string `json:"code,omitempty"`
	Message       string `json:"message,omitempty"`
}

// SessionAttribution is what a session's aimebu tool results say about whose it was.
type SessionAttribution struct {
	ID         string  `json:"id"`
	CWD        string  `json:"cwd"`
	Model      string  `json:"model,omitempty"`
	Sender     string  `json:"sender"`
	MessageNos []int64 `json:"message_nos,omitempty"`
	ModifiedAt string  `json:"modified_at"`
}

// SessionCandidate is one Claude Code session a machine has: what it knows without reading the whole transcript.
type SessionCandidate struct {
	ID         string `json:"id"`
	CWD        string `json:"cwd"`
	Dir        string `json:"dir,omitempty"`
	Title      string `json:"title,omitempty"`
	Model      string `json:"model,omitempty"` // the model of the session's last assistant message, as Claude records it
	GitBranch  string `json:"git_branch,omitempty"`
	Version    string `json:"version,omitempty"`
	Entrypoint string `json:"entrypoint,omitempty"`
	ModifiedAt string `json:"modified_at"`
	Size       int64  `json:"size"`
	Active     bool   `json:"active,omitempty"`
}

type HarnessSetting struct {
	Tool         string `json:"tool"`
	Variant      string `json:"variant"`
	Enabled      bool   `json:"enabled"`
	OverridePath string `json:"override_path"`
}

type LauncherPathMap struct {
	Host    string `json:"host"`
	Visible string `json:"visible"`
}

// LauncherDefinition contains owner configuration only. EnvRefs names local
// supervisor variables; their values never cross the machine connection.
type LauncherDefinition struct {
	ID             string            `json:"id"`
	Tool           string            `json:"tool"`
	Name           string            `json:"name"`
	Command        string            `json:"command"`
	Args           []string          `json:"args"`
	EnvRefs        map[string]string `json:"env_refs"`
	Enabled        bool              `json:"enabled"`
	Containerized  bool              `json:"containerized"`
	AgentServerURL string            `json:"agent_server_url,omitempty"`
	PathMaps       []LauncherPathMap `json:"path_maps"`
	VisibleRoots   []string          `json:"visible_roots"`
	Toolbox        bool              `json:"supports_toolbox"`
}

type LauncherRow struct {
	LauncherDefinition
	ResolvedCommand   string `json:"resolved_command"`
	Version           string `json:"version"`
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	// Features is what a Docker launcher says it can do (its HARNESS_DOCKER_PRINT_FEATURES answer), asked once when the launcher is
	// inspected and only for a tool that depends on one. FeatureProblem says why that answer cannot be used: the probe failed, or the
	// launcher is too old for the tool. It does not make the launcher unavailable; the turn's own preflight still refuses it.
	Features       []string `json:"features,omitempty"`
	FeatureProblem string   `json:"feature_problem,omitempty"`
}

type HarnessRow struct {
	HarnessSetting
	LauncherPath      string `json:"launcher_path"`
	Version           string `json:"version"`
	Available         bool   `json:"available"`
	Source            string `json:"source"`
	PolicyDisabled    bool   `json:"policy_disabled"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type CapabilityReport struct {
	Tool        string               `json:"tool"`
	Version     string               `json:"version"`
	Environment adapter.Environment  `json:"environment"`
	Flags       adapter.Capabilities `json:"flags"`
}

func DeliverFrame(d Delivery) Frame {
	agent := d.Agent
	return Frame{V: Version, Type: Deliver, AgentID: d.AgentID, Generation: d.Generation, DesiredRev: d.DesiredRev, TurnID: d.TurnID, Mode: d.Mode, ParentTurnID: d.ParentTurnID, Notes: d.Notes, Context: d.Context, AllowedTools: d.AllowedTools, Agent: &agent, NativeSessionHandle: d.NativeSessionHandle, NativeSessionStarted: d.NativeSessionStarted, ForkSession: d.ForkSession, CheckoutPath: d.CheckoutPath, LaunchPath: d.LaunchPath, AttachmentBearer: d.AttachmentBearer}
}
func (f Frame) Delivery() Delivery {
	agent := adapter.AgentSpec{}
	if f.Agent != nil {
		agent = *f.Agent
	}
	return Delivery{AgentID: f.AgentID, Generation: f.Generation, DesiredRev: f.DesiredRev, TurnID: f.TurnID, Mode: f.Mode, ParentTurnID: f.ParentTurnID, Notes: f.Notes, Context: f.Context, AllowedTools: f.AllowedTools, Agent: agent, NativeSessionHandle: f.NativeSessionHandle, NativeSessionStarted: f.NativeSessionStarted, ForkSession: f.ForkSession, CheckoutPath: f.CheckoutPath, LaunchPath: f.LaunchPath, AttachmentBearer: f.AttachmentBearer}
}

type HelloAgent struct {
	AgentID    string     `json:"agent_id"`
	Generation int        `json:"generation"`
	OpenTurns  []OpenTurn `json:"open_turns"`
}

type OpenTurn struct {
	TurnID       string `json:"turn_id"`
	Exec         string `json:"exec"`
	ProcessAlive bool   `json:"process_alive,omitempty"`
	HasAnswer    bool   `json:"has_answer"`
	AnswerRev    int    `json:"answer_rev,omitempty"`
	Digest       string `json:"digest,omitempty"`
}

type DesiredAgent struct {
	AgentID    string `json:"agent_id"`
	Generation int    `json:"generation"`
	Desired    string `json:"desired"`
	DesiredRev int64  `json:"desired_rev"`
}

type Delivery struct {
	AgentID              string            `json:"agent_id"`
	Generation           int               `json:"generation"`
	DesiredRev           int64             `json:"desired_rev"`
	TurnID               string            `json:"turn_id"`
	Mode                 adapter.Mode      `json:"mode"`
	ParentTurnID         string            `json:"parent_turn_id,omitempty"`
	Notes                []DeliveryNote    `json:"notes"`
	Context              json.RawMessage   `json:"context"`
	AllowedTools         []string          `json:"allowed_tools,omitempty"`
	Agent                adapter.AgentSpec `json:"agent"`
	NativeSessionHandle  string            `json:"native_session,omitempty"`
	NativeSessionStarted bool              `json:"native_session_started,omitempty"`
	ForkSession          bool              `json:"fork_session,omitempty"`
	CheckoutPath         string            `json:"checkout_path,omitempty"`
	// LaunchPath: see Frame.LaunchPath.
	LaunchPath       string `json:"launch_path,omitempty"`
	AttachmentBearer string `json:"attachment_bearer,omitempty"`
}

type DeliveryNote struct {
	NoteID    string `json:"note_id"`
	RoomID    string `json:"room_id"`
	From      string `json:"from"`
	MessageID string `json:"message_id"`
	// No is the message's number, the "#N" people see; the agent may name a message by it (reply_to, thread_root, reactions).
	No          int64                `json:"no,omitempty"`
	Body        string               `json:"body"`
	RetryNotice string               `json:"retry_notice,omitempty"`
	ReplyTo     string               `json:"reply_to,omitempty"`
	ThreadRoot  string               `json:"thread_root,omitempty"`
	Attachments []DeliveryAttachment `json:"attachments,omitempty"`
}

type DeliveryAttachment struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	MIME   string `json:"mime"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type ReceiptBody struct {
	TurnID    string          `json:"turn_id"`
	AnswerRev int             `json:"answer_rev"`
	Digest    string          `json:"digest"`
	Result    string          `json:"result"`
	Details   json.RawMessage `json:"details,omitempty"`
}

// DetectRequest asks a supervisor to look at its own machine: what harnesses are installed ("harnesses"), or what models one harness
// offers ("models", for the family and the executable given). Nothing it finds is saved by the supervisor. For "models", Args are the
// launcher's own configured arguments, put before the listing arguments, and Dir is an absolute directory on the supervisor's machine
// to run in instead of its aimebu config directory (chosen by the person when a harness will not answer from there). Both are optional:
// a supervisor that does not advertise supports_detect_dir ignores them.
type DetectRequest struct {
	Kind    string   `json:"kind"`
	Family  string   `json:"family,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Dir     string   `json:"dir,omitempty"`
}

// HarnessCandidate is an executable found on the machine that looks like a harness. Command is the absolute path.
type HarnessCandidate struct {
	Family        string `json:"family"`
	Command       string `json:"command"`
	Containerized bool   `json:"containerized"`
	Version       string `json:"version,omitempty"`
}

// ModelCandidate is a model a harness reported. Note is what the harness says about it (context size and the like).
type ModelCandidate struct {
	Slug  string `json:"slug"`
	Label string `json:"label"`
	Note  string `json:"note,omitempty"`
}

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
)

// Frame is the versioned machine socket envelope. Agent frames carry both
// agent_id and generation; machine frames carry machine_id. The server binds
// the socket from its authenticated machine key, never from frame claims.
type Frame struct {
	V                    int                  `json:"v"`
	Type                 string               `json:"type"`
	MachineID            string               `json:"machine_id,omitempty"`
	JournalID            string               `json:"journal_id,omitempty"`
	SupervisorVersion    string               `json:"supervisor_version,omitempty"`
	SupportsPathProbe    bool                 `json:"supports_path_probe,omitempty"`
	SupportsWorktree     bool                 `json:"supports_worktree,omitempty"`
	SupportsLaunchers    bool                 `json:"supports_launchers,omitempty"`
	SupportsDetect       bool                 `json:"supports_detect,omitempty"`
	SupportsDetectDir    bool                 `json:"supports_detect_dir,omitempty"`
	Capabilities         []CapabilityReport   `json:"capabilities,omitempty"`
	Harnesses            []HarnessRow         `json:"harnesses,omitempty"`
	HarnessSettings      []HarnessSetting     `json:"harness_settings,omitempty"`
	HarnessProbeDir      string               `json:"harness_probe_dir,omitempty"`
	Harness              *HarnessRow          `json:"harness,omitempty"`
	HarnessSetting       *HarnessSetting      `json:"harness_setting,omitempty"`
	Launchers            []LauncherRow        `json:"launchers,omitempty"`
	LauncherDefinitions  []LauncherDefinition `json:"launcher_definitions,omitempty"`
	LauncherDefinition   *LauncherDefinition  `json:"launcher_definition,omitempty"`
	Launcher             *LauncherRow         `json:"launcher,omitempty"`
	Detect               *DetectRequest       `json:"detect,omitempty"`
	HarnessCandidates    []HarnessCandidate   `json:"harness_candidates,omitempty"`
	ModelCandidates      []ModelCandidate     `json:"model_candidates,omitempty"`
	Agents               []HelloAgent         `json:"agents,omitempty"`
	Desired              []DesiredAgent       `json:"desired,omitempty"`
	Redeliver            []Delivery           `json:"redeliver,omitempty"`
	ResendReceipts       []ReceiptBody        `json:"resend_receipts,omitempty"`
	AgentID              string               `json:"agent_id,omitempty"`
	LauncherID           string               `json:"launcher_id,omitempty"`
	ReqID                string               `json:"req_id,omitempty"`
	Generation           int                  `json:"generation,omitempty"`
	DesiredRev           int64                `json:"desired_rev,omitempty"`
	DesiredState         string               `json:"desired_state,omitempty"`
	AgentKey             string               `json:"agent_key,omitempty"`
	CheckoutPath         string               `json:"checkout_path,omitempty"`
	CanonicalPath        string               `json:"canonical_path,omitempty"`
	RoomID               string               `json:"room_id,omitempty"`
	BaseRef              string               `json:"base_ref,omitempty"`
	WorktreeAction       string               `json:"worktree_action,omitempty"`
	WorktreeForce        bool                 `json:"worktree_force,omitempty"`
	AttachmentBearer     string               `json:"attachment_bearer,omitempty"`
	TurnID               string               `json:"turn_id,omitempty"`
	Mode                 adapter.Mode         `json:"mode,omitempty"`
	ParentTurnID         string               `json:"parent_turn_id,omitempty"`
	Notes                []DeliveryNote       `json:"notes,omitempty"`
	Context              json.RawMessage      `json:"context,omitempty"`
	AllowedTools         []string             `json:"allowed_tools,omitempty"`
	Agent                *adapter.AgentSpec   `json:"agent,omitempty"`
	NativeSessionHandle  string               `json:"native_session,omitempty"`
	NativeSessionStarted bool                 `json:"native_session_started,omitempty"`
	Level                string               `json:"level,omitempty"`
	Ref                  string               `json:"ref,omitempty"`
	SourceEventID        string               `json:"source_event_id,omitempty"`
	Kind                 string               `json:"kind,omitempty"`
	Summary              string               `json:"summary,omitempty"`
	Usage                *adapter.Usage       `json:"usage,omitempty"`
	Compaction           *adapter.Compaction  `json:"compaction,omitempty"`
	AnswerRev            int                  `json:"answer_rev,omitempty"`
	Digest               string               `json:"digest,omitempty"`
	Candidate            string               `json:"candidate,omitempty"`
	AnswerTooLarge       bool                 `json:"answer_too_large,omitempty"`
	Exit                 *adapter.ExitInfo    `json:"exit,omitempty"`
	Result               string               `json:"result,omitempty"`
	Details              json.RawMessage      `json:"details,omitempty"`
	Code                 string               `json:"code,omitempty"`
	Message              string               `json:"message,omitempty"`
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
	return Frame{V: Version, Type: Deliver, AgentID: d.AgentID, Generation: d.Generation, DesiredRev: d.DesiredRev, TurnID: d.TurnID, Mode: d.Mode, ParentTurnID: d.ParentTurnID, Notes: d.Notes, Context: d.Context, AllowedTools: d.AllowedTools, Agent: &agent, NativeSessionHandle: d.NativeSessionHandle, NativeSessionStarted: d.NativeSessionStarted, CheckoutPath: d.CheckoutPath, AttachmentBearer: d.AttachmentBearer}
}
func (f Frame) Delivery() Delivery {
	agent := adapter.AgentSpec{}
	if f.Agent != nil {
		agent = *f.Agent
	}
	return Delivery{AgentID: f.AgentID, Generation: f.Generation, DesiredRev: f.DesiredRev, TurnID: f.TurnID, Mode: f.Mode, ParentTurnID: f.ParentTurnID, Notes: f.Notes, Context: f.Context, AllowedTools: f.AllowedTools, Agent: agent, NativeSessionHandle: f.NativeSessionHandle, NativeSessionStarted: f.NativeSessionStarted, CheckoutPath: f.CheckoutPath, AttachmentBearer: f.AttachmentBearer}
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
	CheckoutPath         string            `json:"checkout_path,omitempty"`
	AttachmentBearer     string            `json:"attachment_bearer,omitempty"`
}

type DeliveryNote struct {
	NoteID      string               `json:"note_id"`
	RoomID      string               `json:"room_id"`
	From        string               `json:"from"`
	MessageID   string               `json:"message_id"`
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

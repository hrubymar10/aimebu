package protocol

import (
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
)

const Version = 2

// KeepAliveScopeTTL is how long the server keeps a ping's scope without hearing of it again: the crash safety net for a supervisor that died or lost its
// connection and so never lifts it. The supervisor of a ping that is not confirmed gone sends the "on" frame again (KeepAliveScopeRenewEvery) for as
// long as it holds the agent in quarantine, so the quarantine can be any length without the scope lapsing under it; the ping's own run (its time
// limit and the end that follows) must stay well inside one TTL, and a test in internal/supervisor holds the numbers to that.
const KeepAliveScopeTTL = 3 * time.Minute

// KeepAliveScopeRenewEvery is how often a supervisor renews the scope of a ping that is not confirmed gone: a third of the TTL, so two renewals in a row
// can be lost to a dropped connection before it lapses.
const KeepAliveScopeRenewEvery = KeepAliveScopeTTL / 3
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
	TelemetryConfig      = "telemetry_config"
	// RetireWarm asks a supervisor to end the idle warm processes of one tool (an account switch is about to change the login they hold) and to answer
	// with RetireWarmResult: Result "ok" once every one is confirmed gone, "error" (with Message) when one is not.
	RetireWarm       = "retire_warm"
	RetireWarmResult = "retire_warm_result"
	// WarmStateFrame is a supervisor's report of its warm processes (in Frame.Warm), sent when they change and with hello.
	WarmStateFrame = "warm_state"
	// WarmClaimFrame is a supervisor telling the server, before it keeps a process warm, how many processes it has running now (Frame.WarmCount, counted
	// with Frame.WarmSeq from the supervisor's one counter). The server saves the count and answers with WarmClaimAckFrame (the same ReqID); the supervisor
	// keeps the process only once it has that answer, and ends it otherwise. What the server knows of a host can then only be ahead of reality, never
	// behind: a host that drops is judged by the last count it gave, which was given before the process existed. Decreases are not claims; they go in
	// warm_state reports, and a stale higher count only makes the server refuse, which is safe.
	WarmClaimFrame    = "warm_claim"
	WarmClaimAckFrame = "warm_claim_ack"
	// KeepAliveFrame is a supervisor's report of one cache keep-alive ping (Frame.KeepAlive).
	KeepAliveFrame = "keep_alive"
	// KeepAlivePausedFrame tells a supervisor which cache keep-alive pings to hold back (Frame.PausedTools, Frame.PausedAgents): the server sends it when
	// the supervisor connects and whenever it changes. It replaces what it said before.
	KeepAlivePausedFrame = "keep_alive_paused"
	// KeepAliveScopeFrame is a supervisor telling the server that a cache keep-alive ping of an agent is about to run (Frame.AgentID, Frame.ScopeOn true) or
	// has ended and is confirmed gone (false). While it runs the server refuses every agent-tools call made with the agent's key (403 keep_alive_scope):
	// a ping is not work, and the harness is only asked in words not to use the toolbox. The server answers the "on" frame with KeepAliveScopeAckFrame
	// (the same ReqID) and the supervisor starts the ping only once it has it; an "off" frame is answered too, and the supervisor lets go of the agent
	// to a real delivery only after that, so no real tool call is ever refused for a ping that has ended.
	// AgentSettingsFrame carries the instance's agent settings to a supervisor without a delivery: the warm window (Frame.WarmIdleSeconds) and the cache
	// keep-alive settings by tool (Frame.KeepAliveByTool). The server sends it when the supervisor connects, whenever a setting changes and with the
	// periodic keep_alive_paused check, so a setting that is turned off reaches agents that are idle, not only the ones that get another delivery.
	AgentSettingsFrame     = "agent_settings"
	KeepAliveScopeFrame    = "keep_alive_scope"
	KeepAliveScopeAckFrame = "keep_alive_scope_ack"
)

// TelemetrySettings is what the server tells a supervisor about error and performance reporting: whether it is on, where reports go and how many
// requests are traced. It carries no identity of a person. The supervisor reports as the machine it is (and not under a host name); a DSN set in
// the supervisor's own environment (AIMEBU2_SENTRY_DSN, a DSN or "off") wins over it.
type TelemetrySettings struct {
	Enabled          bool    `json:"enabled"`
	DSN              string  `json:"dsn,omitempty"`
	Environment      string  `json:"environment,omitempty"`
	TracesSampleRate float64 `json:"traces_sample_rate,omitempty"`
}

// AgentKeyGrant is a fresh key for one agent. It is in a frame to the supervisor that runs the agent and nowhere else.
type AgentKeyGrant struct {
	AgentID    string `json:"agent_id"`
	Generation int    `json:"generation"`
	Key        string `json:"key"`
}

// Frame is the versioned machine socket envelope. Agent frames carry both
// agent_id and generation; machine frames carry machine_id. The server binds
// the socket from its authenticated machine key, never from frame claims.
type Frame struct {
	V                 int    `json:"v"`
	Type              string `json:"type"`
	MachineID         string `json:"machine_id,omitempty"`
	JournalID         string `json:"journal_id,omitempty"`
	SupervisorVersion string `json:"supervisor_version,omitempty"`
	SupportsPathProbe bool   `json:"supports_path_probe,omitempty"`
	// SupportsWarm says the supervisor keeps agents' harness processes warm and answers a retire_warm request. A server asks only one that does.
	SupportsWarm bool `json:"supports_warm,omitempty"`
	// PausedTools and PausedAgents, on a keep_alive_paused frame, are the tools whose limit guard is holding turns (the account is near its limit) and the
	// agents whose provider limit is in force: no ping for them.
	PausedTools  []string `json:"paused_tools,omitempty"`
	PausedAgents []string `json:"paused_agents,omitempty"`
	// KeepAliveByTool, on an agent_settings frame, is each tool's keep-alive setting (an interval of 0 is off for that tool).
	KeepAliveByTool map[string]KeepAliveSettings `json:"keep_alive_by_tool,omitempty"`
	// ScopeOn, on a keep_alive_scope frame, says a ping begins (true) or has ended (false).
	ScopeOn bool `json:"scope_on,omitempty"`
	// WarmSeq, on a warm_claim and a scope-on frame, is the supervisor's monotonic counter value for the frame (the same counter numbers WarmReport.Seq),
	// and WarmCount, on a warm_claim, the processes (warm and pings) it has running. The server applies counts in this order and never lets an older
	// one overwrite a newer.
	WarmSeq   uint64 `json:"warm_seq,omitempty"`
	WarmCount int    `json:"warm_count,omitempty"`
	// ScopeGen, on a keep_alive_scope frame, names the ping the frame is about (its turn id). The server remembers it with the scope and ignores a lift
	// that names another ping than the one scoped now (an owed lift of an old ping arriving after a new ping's scope began must not clear it); a
	// frame without one (an older supervisor) always applies.
	ScopeGen string `json:"scope_gen,omitempty"`
	// KeepAlive, on a keep_alive frame, is what one ping did.
	KeepAlive *KeepAliveReport `json:"keep_alive_report,omitempty"`
	// Warm, on a warm_state frame, is the supervisor's whole warm picture at that moment.
	Warm *WarmReport `json:"warm,omitempty"`
	// RetireTool, on a retire_warm request, is the tool whose idle warm processes are to end.
	RetireTool          string `json:"retire_tool,omitempty"`
	SupportsSessionScan bool   `json:"supports_session_scan,omitempty"`
	SupportsSessionFork bool   `json:"supports_session_fork,omitempty"`
	// SupportsSessionCopy says the supervisor copies a Claude Code session to another folder's project directory when a session_scan names
	// ScanSessionID and CopyTo. One that does not would ignore CopyTo and answer as a plain lookup, so the server never asks it.
	SupportsSessionCopy bool `json:"supports_session_copy,omitempty"`
	// SupportsSessionAttribution says the supervisor answers a session scan that asks for attribution (ScanAttribution).
	SupportsSessionAttribution bool `json:"supports_session_attribution,omitempty"`
	SupportsLaunchPath         bool `json:"supports_launch_path,omitempty"`
	SupportsWorktree           bool `json:"supports_worktree,omitempty"`
	SupportsLaunchers          bool `json:"supports_launchers,omitempty"`
	SupportsDetect             bool `json:"supports_detect,omitempty"`
	SupportsDetectDir          bool `json:"supports_detect_dir,omitempty"`
	SupportsAgentLogs          bool `json:"supports_agent_logs,omitempty"`
	SupportsAgentLogSize       bool `json:"supports_agent_log_size,omitempty"`
	SupportsStopReplay         bool `json:"supports_stop_replay,omitempty"`
	// SupportsTelemetryConfig says the supervisor takes the server's telemetry settings (a telemetry_config frame, and Telemetry in hello_ack). One that
	// does not never gets the frame, whose type an older supervisor does not know.
	SupportsTelemetryConfig bool `json:"supports_telemetry_config,omitempty"`
	// SupportsAgentKeys says the supervisor keeps each agent's key in memory only, tells the server which keys it holds (HeldKeys) and takes new
	// ones in hello_ack (AgentKeys). The server grants keys only to a supervisor that says so; an older one keeps its keys on disk.
	SupportsAgentKeys bool `json:"supports_agent_keys,omitempty"`
	// HeldKeys, in hello, is the agents whose key the supervisor holds. A restarted supervisor holds none: it lost them with its memory.
	HeldKeys []string `json:"held_keys,omitempty"`
	// AgentKeys, in hello_ack, is a new key for each agent the supervisor should run but holds no key for. Granting one revokes the old key.
	AgentKeys []AgentKeyGrant `json:"agent_keys,omitempty"`
	// Telemetry is the effective telemetry configuration of the server, in hello_ack and in a telemetry_config frame.
	Telemetry           *TelemetrySettings   `json:"telemetry,omitempty"`
	Capabilities        []CapabilityReport   `json:"capabilities,omitempty"`
	Harnesses           []HarnessRow         `json:"harnesses,omitempty"`
	HarnessSettings     []HarnessSetting     `json:"harness_settings,omitempty"`
	HarnessProbeDir     string               `json:"harness_probe_dir,omitempty"`
	HarnessProbeDirs    []string             `json:"harness_probe_dirs,omitempty"`
	Harness             *HarnessRow          `json:"harness,omitempty"`
	HarnessSetting      *HarnessSetting      `json:"harness_setting,omitempty"`
	Launchers           []LauncherRow        `json:"launchers,omitempty"`
	LauncherDefinitions []LauncherDefinition `json:"launcher_definitions,omitempty"`
	LauncherDefinition  *LauncherDefinition  `json:"launcher_definition,omitempty"`
	Launcher            *LauncherRow         `json:"launcher,omitempty"`
	Detect              *DetectRequest       `json:"detect,omitempty"`
	HarnessCandidates   []HarnessCandidate   `json:"harness_candidates,omitempty"`
	ModelCandidates     []ModelCandidate     `json:"model_candidates,omitempty"`
	DetectDir           string               `json:"detect_dir,omitempty"`
	Agents              []HelloAgent         `json:"agents,omitempty"`
	Desired             []DesiredAgent       `json:"desired,omitempty"`
	Redeliver           []Delivery           `json:"redeliver,omitempty"`
	ResendReceipts      []ReceiptBody        `json:"resend_receipts,omitempty"`
	AgentID             string               `json:"agent_id,omitempty"`
	LauncherID          string               `json:"launcher_id,omitempty"`
	ReqID               string               `json:"req_id,omitempty"`
	Generation          int                  `json:"generation,omitempty"`
	DesiredRev          int64                `json:"desired_rev,omitempty"`
	DesiredState        string               `json:"desired_state,omitempty"`
	AgentKey            string               `json:"agent_key,omitempty"`
	// DockerOnly, on a spawn and a delivery, says the instance runs agents only in docker: a supervisor refuses an agent whose launcher is not
	// containerized. An older supervisor ignores the field, which is why the server refuses it too.
	DockerOnly bool `json:"docker_only,omitempty"`
	// WarmIdleSeconds, on a delivery, is the instance's setting for how long a supervisor keeps an agent's harness process up after a turn
	// (0 is off). It is always sent by a server that has the setting; a supervisor that does not know it ignores it and runs every turn cold.
	WarmIdleSeconds *int `json:"warm_idle_seconds,omitempty"`
	// KeepAliveSettings, on a delivery: see Delivery.KeepAliveSettings.
	KeepAliveSettings    *KeepAliveSettings `json:"keep_alive,omitempty"`
	CheckoutPath         string             `json:"checkout_path,omitempty"`
	CanonicalPath        string             `json:"canonical_path,omitempty"`
	RoomID               string             `json:"room_id,omitempty"`
	BaseRef              string             `json:"base_ref,omitempty"`
	WorktreeAction       string             `json:"worktree_action,omitempty"`
	WorktreeForce        bool               `json:"worktree_force,omitempty"`
	LogLines             int                `json:"log_lines,omitempty"`
	LogMaxBytes          int                `json:"log_max_bytes,omitempty"`
	AttachmentBearer     string             `json:"attachment_bearer,omitempty"`
	TurnID               string             `json:"turn_id,omitempty"`
	Mode                 adapter.Mode       `json:"mode,omitempty"`
	ParentTurnID         string             `json:"parent_turn_id,omitempty"`
	Notes                []DeliveryNote     `json:"notes,omitempty"`
	Context              json.RawMessage    `json:"context,omitempty"`
	AllowedTools         []string           `json:"allowed_tools,omitempty"`
	Agent                *adapter.AgentSpec `json:"agent,omitempty"`
	NativeSessionHandle  string             `json:"native_session,omitempty"`
	NativeSessionStarted bool               `json:"native_session_started,omitempty"`
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
	// CopyTo, with ScanSessionID, copies that session (the transcript and its own directory) under the project directory Claude uses for this
	// absolute folder, so a session continued as a copy can run there; the answer carries no Sessions.
	CopyTo string `json:"copy_to,omitempty"`
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

	// SupportsProviders says the supervisor runs an OpenCode turn that carries custom providers (Delivery.Providers). One that does not
	// would start OpenCode without the endpoint, so the server holds such turns instead of sending them.
	SupportsProviders bool `json:"supports_providers,omitempty"`
	// Providers: see Delivery.Providers.
	Providers []adapter.CustomProvider `json:"providers,omitempty"`
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
	// VersionProblem says why Version is empty for a launcher that runs in a container: there was no folder to ask it from, it said so itself
	// (a folder it does not mount, the container stopped), or it did not answer in time. Empty when the version is known.
	VersionProblem string `json:"version_problem,omitempty"`
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
	return Frame{V: Version, Type: Deliver, AgentID: d.AgentID, Generation: d.Generation, DesiredRev: d.DesiredRev, TurnID: d.TurnID, Mode: d.Mode, ParentTurnID: d.ParentTurnID, Notes: d.Notes, Context: d.Context, AllowedTools: d.AllowedTools, Agent: &agent, NativeSessionHandle: d.NativeSessionHandle, NativeSessionStarted: d.NativeSessionStarted, ForkSession: d.ForkSession, CheckoutPath: d.CheckoutPath, LaunchPath: d.LaunchPath, AttachmentBearer: d.AttachmentBearer, Providers: d.Providers, DockerOnly: d.DockerOnly, WarmIdleSeconds: d.WarmIdleSeconds, KeepAliveSettings: d.KeepAliveSettings}
}
func (f Frame) Delivery() Delivery {
	agent := adapter.AgentSpec{}
	if f.Agent != nil {
		agent = *f.Agent
	}
	return Delivery{AgentID: f.AgentID, Generation: f.Generation, DesiredRev: f.DesiredRev, TurnID: f.TurnID, Mode: f.Mode, ParentTurnID: f.ParentTurnID, Notes: f.Notes, Context: f.Context, AllowedTools: f.AllowedTools, Agent: agent, NativeSessionHandle: f.NativeSessionHandle, NativeSessionStarted: f.NativeSessionStarted, ForkSession: f.ForkSession, CheckoutPath: f.CheckoutPath, LaunchPath: f.LaunchPath, AttachmentBearer: f.AttachmentBearer, Providers: f.Providers, DockerOnly: f.DockerOnly, WarmIdleSeconds: f.WarmIdleSeconds, KeepAliveSettings: f.KeepAliveSettings}
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
	// Providers are the custom providers an OpenCode turn's model needs: the one its model id names. Names of key variables only.
	Providers []adapter.CustomProvider `json:"providers,omitempty"`
	// DockerOnly: see Frame.DockerOnly.
	DockerOnly bool `json:"docker_only,omitempty"`
	// WarmIdleSeconds: see Frame.WarmIdleSeconds.
	WarmIdleSeconds *int `json:"warm_idle_seconds,omitempty"`
	// KeepAliveSettings is the instance's cache keep-alive setting for this agent's tool; nil means the server has none, and the supervisor does not ping.
	KeepAliveSettings *KeepAliveSettings `json:"keep_alive,omitempty"`
}

// KeepAliveSettings says when an idle agent's prompt cache is kept warm with a ping: every IntervalSeconds after its last turn (0 is off), until
// StopAfterSeconds have passed without real work.
type KeepAliveSettings struct {
	IntervalSeconds  int `json:"interval_seconds"`
	StopAfterSeconds int `json:"stop_after_seconds"`
}

// KeepAliveReport is what one ping did: whether it ran, and what it read from and wrote to the prompt cache, so a ping that missed shows. Nothing of
// what the model said is in it.
type KeepAliveReport struct {
	AgentID        string  `json:"agent_id"`
	At             string  `json:"at"`
	OK             bool    `json:"ok"`
	Error          string  `json:"error,omitempty"`
	ViaWarm        bool    `json:"via_warm,omitempty"`
	TokensIn       int64   `json:"tokens_in"`
	TokensOut      int64   `json:"tokens_out"`
	CacheRead      int64   `json:"cache_read"`
	CacheWrite     int64   `json:"cache_write"`
	CostCumulative float64 `json:"cost_cumulative"`
	// CostIsTotal says CostCumulative is the harness session's running total (Claude); otherwise Cost is this ping's own cost (Codex).
	CostIsTotal bool `json:"cost_is_total,omitempty"`
	// Cost is what this ping cost: the session's cumulative cost after it less what it was before (0 when that is not known).
	Cost            float64 `json:"cost"`
	DurationSeconds float64 `json:"duration_seconds"`
}

type DeliveryNote struct {
	NoteID    string `json:"note_id"`
	RoomID    string `json:"room_id"`
	From      string `json:"from"`
	MessageID string `json:"message_id"`
	// No is the message's number, the "#N" people see; the agent may name a message by it (reply_to, thread_root, reactions).
	No          int64  `json:"no,omitempty"`
	Body        string `json:"body"`
	RetryNotice string `json:"retry_notice,omitempty"`
	ReplyTo     string `json:"reply_to,omitempty"`
	// ThreadRoot is the first message of the conversation the message is in, derived from its reply chain (never chosen by its writer).
	ThreadRoot  string               `json:"thread_root,omitempty"`
	Attachments []DeliveryAttachment `json:"attachments,omitempty"`
	// Why says in a few words why this message reached the agent: "a reply to your message #12", "you are mentioned", "a person's
	// message to the room"...
	Why string `json:"why,omitempty"`
	// FYI says the message was not addressed to the agent: it is a broadcast an agent wrote to the whole room, with no mention and no reply. The agent
	// answers only if it has to act, and otherwise ends the turn without writing a message.
	FYI bool `json:"fyi,omitempty"`
	// InReplyTo is the message this one replies to, and Conversation the first message of its conversation when that is another one: its
	// number, who wrote it, an excerpt, and whether the agent wrote it itself, so the agent knows what is being answered without looking it up.
	InReplyTo    *NoteParent `json:"in_reply_to,omitempty"`
	Conversation *NoteParent `json:"conversation,omitempty"`
}

// NoteParent is the message a delivered note replies to, or the first message of its conversation.
type NoteParent struct {
	No    int64  `json:"no"`
	From  string `json:"from"`
	Body  string `json:"body"`
	Yours bool   `json:"yours,omitempty"`
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
	// Containerized says the harness runs in a container: a model whose provider listens on the machine's own loopback is then not
	// reachable from it, which detection reports (ModelCandidate.Unreachable).
	Containerized bool `json:"containerized,omitempty"`
	// LauncherID names the harness a provider request is about: its environment references say where a key variable gets its value.
	LauncherID string `json:"launcher_id,omitempty"`
	// KeyNames are the key variables kind "provider_keys" asks about.
	KeyNames []string `json:"key_names,omitempty"`
	// Providers are the harness's custom providers, for kind "models" of an OpenCode harness: the listing runs with them in its private
	// config, and each provider's own endpoint is asked which models it serves.
	Providers []adapter.CustomProvider `json:"providers,omitempty"`
}

// ProviderKeysAnswer is the Details of the answer to kind "provider_keys": for each name asked about, whether it has a value where this
// machine's supervisor starts turns. Only whether, never the value.
type ProviderKeysAnswer struct {
	Set map[string]bool `json:"set"`
}

// ProviderImportAnswer is the Details of the answer to kind "provider_import": the custom providers the machine's own OpenCode config
// defines that a harness could take over. A key is never copied: a provider whose key is a reference to an environment variable carries
// that variable's name, and one whose key sits in the config as text is only flagged (InlineKey).
type ProviderImportAnswer struct {
	// Source is where the providers were read, with the home folder shown as ~.
	Source      string               `json:"source"`
	Suggestions []ProviderSuggestion `json:"suggestions"`
	Skipped     []ProviderSkipped    `json:"skipped,omitempty"`
}

type ProviderSuggestion struct {
	Provider  adapter.CustomProvider `json:"provider"`
	InlineKey bool                   `json:"inline_key,omitempty"`
}

// ProviderSkipped is a provider of the config that cannot become a custom provider, and why.
type ProviderSkipped struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// ProviderModelsAnswer is the Details of a model listing that asked providers' endpoints: what each endpoint that failed to answer said.
type ProviderModelsAnswer struct {
	EndpointProblems map[string]string `json:"endpoint_problems,omitempty"`
}

// HarnessCandidate is an executable found on the machine that looks like a harness. Command is the absolute path.
type HarnessCandidate struct {
	Family        string `json:"family"`
	Command       string `json:"command"`
	Containerized bool   `json:"containerized"`
	Version       string `json:"version,omitempty"`
	// VersionProblem says why Version is empty for a harness that runs in a container: no folder to ask it from, or what the launcher said.
	VersionProblem string `json:"version_problem,omitempty"`
}

// ModelCandidate is a model a harness reported. Note is what the harness says about it (context size and the like).
type ModelCandidate struct {
	Slug  string `json:"slug"`
	Label string `json:"label"`
	Note  string `json:"note,omitempty"`
	// Unreachable says why an agent could not use the model where the harness runs (its provider is at the machine's own loopback address,
	// which inside a container is the container); empty for a model that can be reached.
	Unreachable string `json:"unreachable,omitempty"`
}

// WarmReport is what a supervisor says about the agent processes it keeps warm: which agents have a live process and until when it is kept,
// and what that costs. It is a snapshot, replaced by the next one.
type WarmReport struct {
	// Agents are the agents with a live process: State "idle" (kept until SleepsAt, RFC 3339) or "busy" (running a turn).
	Agents []WarmAgent `json:"agents"`
	// Seq numbers the report from the supervisor's one counter (shared with warm claims and ping scopes): the server ignores a report older than one
	// it has applied. Processes counts every live warm process, whatever its state (busy, idle, or one that could not be confirmed gone); Agents is the
	// picture for the screen and may omit what it cannot show.
	Seq       uint64 `json:"seq,omitempty"`
	Processes int    `json:"processes,omitempty"`
	// PingsActive counts cache keep-alive pings that run or are not confirmed gone yet (a ping in a container held in quarantine counts until its end
	// is confirmed or the quarantine is over). They are processes on the login too, though they are not warm slots: the server counts them with the
	// agents when it asks whether a host that has dropped may still have something running.
	PingsActive int `json:"pings_active,omitempty"`
	// Started and Reused count warm processes started and turns that went into a live one, since the supervisor started.
	Started int `json:"started"`
	Reused  int `json:"reused"`
	// UnconfirmedPingEnds counts keep-alive pings in a container whose end was never confirmed and whose quarantine ran out.
	UnconfirmedPingEnds int `json:"unconfirmed_ping_ends,omitempty"`
	// RSSBytes is the memory of the warm processes the supervisor runs itself; ContainerWideBytes is what every running container uses (the harness
	// container is shared by all agents, and Docker reports no finer), where Docker warm processes live. Neither is measured per agent.
	RSSBytes           int64 `json:"rss_bytes"`
	ContainerWideBytes int64 `json:"container_wide_bytes"`
}

// WarmAgent is one agent's live process.
type WarmAgent struct {
	AgentID  string `json:"agent_id"`
	State    string `json:"state"`
	SleepsAt string `json:"sleeps_at,omitempty"`
}

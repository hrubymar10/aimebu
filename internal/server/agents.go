package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/ccsessions"
	"github.com/hrubymar10/aimebu/v2/internal/pricing"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

type spawnRequest struct {
	RoleKey    string `json:"role_key"`
	MachineID  string `json:"machine_id,omitempty"`
	Tool       string `json:"tool"`
	Variant    string `json:"variant"`
	LauncherID string `json:"launcher_id,omitempty"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Reason     string `json:"reason,omitempty"`
	Worktree   bool   `json:"worktree,omitempty"`
	BaseRef    string `json:"base_ref,omitempty"`
	// JoinParent also joins the new agent, quietly, to its room's parent room (lead or dispatcher only; see store.SpawnInput.JoinParent).
	JoinParent bool `json:"join_parent,omitempty"`
	// ApprovedBy is the validated message of a person that approved this exact harness and model choice outside the role's default pool;
	// set only on an agent-initiated spawn (the agent route validates it, a person's route never sets it) and recorded on the agent.
	ApprovedBy string `json:"approved_by,omitempty"`
	// Name is the agent's name; empty lets the server pick a free one from its pool. A v1 agent that is resumed keeps its own name.
	Name string `json:"name,omitempty"`
	// ResumeLegacy starts the agent from an imported v1 agent: the same name, and its harness session continued when the harness can.
	// ConfirmV1Idle acknowledges the warning that v1 may still be using that session.
	ResumeLegacy  string `json:"resume_legacy,omitempty"`
	ConfirmV1Idle bool   `json:"confirm_v1_idle,omitempty"`
	// ResumeSession starts the agent from a Claude Code session the machine already has: its first turn is `claude --resume <id>` in the
	// folder the session was made in. The server finds the session itself; ConfirmSessionClosed acknowledges the warning that a live
	// process may still have it open. NewRoom starts the agent in a room of its own, titled after the session, instead of the room named
	// in the request.
	ResumeSession        string `json:"resume_session,omitempty"`
	ConfirmSessionClosed bool   `json:"confirm_session_closed,omitempty"`
	NewRoom              bool   `json:"new_room,omitempty"`
	// ResumeMode is how a Claude Code session is taken over: "continue" appends to the original, "fork" works on a copy (the first turn runs
	// with --fork-session) and leaves the original as it is. Whoever does not say gets "fork": the safe choice for a caller that cannot ask.
	ResumeMode string `json:"resume_mode,omitempty"`
	// WorkOnCopy starts a continued session as a copy in a worktree of its own (a copy of the session is put under that folder's Claude
	// project directory and forked there) instead of in the folder it was made in. It implies resume_mode fork.
	WorkOnCopy bool `json:"work_on_copy,omitempty"`
}

const (
	resumeContinue = "continue"
	resumeFork     = "fork"
)

type spawnCatalogModel struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Note    string `json:"note,omitempty"`
	Default bool   `json:"default,omitempty"`
}

type spawnCatalogTool struct {
	Tool              string              `json:"tool"`
	Variant           string              `json:"variant"`
	LauncherID        string              `json:"launcher_id,omitempty"`
	LauncherName      string              `json:"launcher_name,omitempty"`
	Label             string              `json:"label"`
	CLIVersion        string              `json:"cli_version,omitempty"`
	Models            []spawnCatalogModel `json:"models"`
	Available         bool                `json:"available"`
	UnavailableReason string              `json:"unavailable_reason,omitempty"`
	Toolbox           bool                `json:"toolbox"`
	Containerized     bool                `json:"containerized"`
	// SharesClaudeHome says this Docker Claude launcher works in the host's Claude home (its claude_home_shared feature): a session made on
	// the host can be continued or copied through it. The dialogs offer it for a session only when it says so.
	SharesClaudeHome bool   `json:"shares_claude_home,omitempty"`
	ToolboxReason    string `json:"toolbox_reason,omitempty"`
}

// boundedAdapters fixes the tool, variant, and provider pairings; the model
// choices for each tool are owner-editable and live in the store.
var boundedAdapters = []struct {
	tool, variant, label, provider string
}{
	{"claude", "docker", "Claude Code (Docker)", "anthropic"},
	{"codex", "docker", "Codex (Docker)", "openai"},
	{"pi", "docker", "Pi (Docker)", "ollama-cloud"},
	{"claude", "native", "Claude Code", "anthropic"},
	{"codex", "native", "Codex", "openai"},
	{"pi", "native", "Pi", "ollama-cloud"},
	{"opencode", "native", "OpenCode", "opencode"},
	{"opencode", "docker", "OpenCode (Docker)", "opencode"},
	{"vibe", "native", "Mistral Vibe", "mistral"},
	{"vibe", "docker", "Mistral Vibe (Docker)", "mistral"},
}

// spawnCatalog reports bounded server-approved model choices alongside live
// supervisor discovery. A CLI version describes the executable, never a model.
func (s *Server) spawnCatalog(w http.ResponseWriter, r *http.Request) {
	machineID := r.URL.Query().Get("machine_id")
	if machineID == "" {
		host, err := s.Store.HostMachine(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		machineID = host.ID
	}
	// Only a machine one of the person's projects is mapped to (as a project they can start agents in): any other, one that does not exist
	// included, is "not found", so the catalog is no way to list machines or read what they have installed.
	if allowed, err := s.Store.UserMayUseMachine(r.Context(), userID(r), machineID); err != nil {
		fail(w, err)
		return
	} else if !allowed {
		apiError(w, 404, "not_found", "machine not found")
		return
	}
	models, err := s.Store.MachineHarnessModels(r.Context(), machineID)
	if err != nil {
		fail(w, err)
		return
	}
	selected, err := s.Store.MachineByID(r.Context(), machineID)
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "machine not found")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if selected.RevokedAt.Valid {
		apiError(w, 409, "machine_revoked", "machine revoked")
		return
	}
	s.machineMu.Lock()
	machine := s.machines[machineID]
	s.machineMu.Unlock()
	if machine != nil && machine.supportsLaunchers {
		definitions, err := s.Store.MachineLaunchers(r.Context(), machineID)
		if err != nil {
			fail(w, err)
			return
		}
		machine.harnessMu.Lock()
		reports := append([]protocol.LauncherRow(nil), machine.launchers...)
		machine.harnessMu.Unlock()
		items := make([]spawnCatalogTool, 0, len(definitions))
		for _, definition := range definitions {
			var allowed *struct {
				tool, variant, label, provider string
			}
			for i := range boundedAdapters {
				if boundedAdapters[i].tool == definition.Tool &&
					(boundedAdapters[i].variant == "docker") == definition.Containerized {
					allowed = &boundedAdapters[i]
					break
				}
			}
			if allowed == nil {
				continue
			}
			alias := definition.Name
			if alias == "harness-docker" {
				alias = "docker"
			}
			item := spawnCatalogTool{Tool: definition.Tool, Variant: alias, LauncherID: definition.ID, LauncherName: definition.Name,
				Label: harnessLabel(definition, allowed.label), Models: catalogModels(models[definition.ID]),
				Toolbox: definition.Toolbox, Containerized: definition.Containerized, UnavailableReason: "launcher has not reported status"}
			for _, report := range reports {
				if report.ID == definition.ID {
					item.Available, item.CLIVersion, item.UnavailableReason = report.Available, report.Version, report.UnavailableReason
					break
				}
			}
			item.SharesClaudeHome = sharesClaudeHome(machine, &definition)
			if !definition.Enabled {
				item.Available, item.UnavailableReason = false, "disabled by owner; enable it in Settings → Harnesses & models"
			}
			if item.Available && definition.Containerized && definition.AgentServerURL == "" && dockerBindIsLoopback() {
				item.Available, item.UnavailableReason = false, dockerBindUnavailableMessage
			}
			if !definition.Toolbox {
				item.ToolboxReason = "launcher has no private toolbox transport"
			}
			items = append(items, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"machine_id": machineID, "tools": items})
		return
	}
	// A supervisor that predates launchers still has harnesses in the store: its models are those of the harness named like the variant.
	legacyHarnesses, err := s.Store.MachineLaunchers(r.Context(), machineID)
	if err != nil {
		fail(w, err)
		return
	}
	legacyModels := func(tool, variant string) []spawnCatalogModel {
		name := "native"
		if variant == "docker" {
			name = "harness-docker"
		}
		for _, harness := range legacyHarnesses {
			if harness.Tool == tool && harness.Name == name {
				return catalogModels(models[harness.ID])
			}
		}
		return []spawnCatalogModel{}
	}
	s.machineMu.Lock()
	defer s.machineMu.Unlock()
	items := make([]spawnCatalogTool, 0, len(boundedAdapters))
	for _, allowed := range boundedAdapters {
		item := spawnCatalogTool{Tool: allowed.tool, Variant: allowed.variant, Label: allowed.label, Models: legacyModels(allowed.tool, allowed.variant), UnavailableReason: "no connected machine lists this adapter", Containerized: allowed.variant == "docker", Toolbox: allowed.tool != "pi" || allowed.variant == "native"}
		if allowed.tool == "pi" && allowed.variant == "docker" {
			item.ToolboxReason = "pi-docker has no private credential channel"
		}
		if machine := s.machines[machineID]; machine != nil && selected.Approved {
			machine.harnessMu.Lock()
			rows := append([]protocol.HarnessRow(nil), machine.harnesses...)
			reports := append([]protocol.CapabilityReport(nil), machine.capabilities...)
			machine.harnessMu.Unlock()
			if len(rows) > 0 {
				for _, row := range rows {
					if row.Tool == allowed.tool && row.Variant == allowed.variant {
						item.Available, item.CLIVersion, item.UnavailableReason = row.Available, row.Version, row.UnavailableReason
						break
					}
				}
			} else {
				for _, report := range reports {
					if report.Tool == allowed.tool && report.Environment.Variant == allowed.variant {
						item.Available, item.CLIVersion, item.UnavailableReason = true, report.Version, ""
						break
					}
				}
			}
		}
		if item.Available && dockerNeedsReachableBind(allowed.tool, allowed.variant) && dockerBindIsLoopback() {
			item.Available, item.UnavailableReason = false, dockerBindUnavailableMessage
		}
		items = append(items, item)
	}
	// A connected supervisor that predates harnesses cannot be given any, so the dialogs say to update it instead of staying mute.
	writeJSON(w, http.StatusOK, map[string]any{"machine_id": machineID, "tools": items, "supervisor_update_needed": machine != nil && !machine.supportsLaunchers})
}

const dockerBindUnavailableMessage = "AIMEBU_BIND is loopback-only; Docker needs a non-loopback bind and AIMEBU_ALLOW entry"

var errDockerBindUnavailable = errors.New(dockerBindUnavailableMessage)

func dockerNeedsReachableBind(tool, variant string) bool {
	return variant == "docker" && (tool == "claude" || tool == "codex" || tool == "pi" || tool == "opencode" || tool == "vibe")
}

func dockerBindIsLoopback() bool {
	bind := os.Getenv("AIMEBU_BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	ip := net.ParseIP(bind)
	return ip != nil && ip.IsLoopback()
}

var errLauncherUnavailable = errors.New("pass an enabled launcher_id or a launcher name in variant (native/docker for the default names); check Settings → Harnesses & models")

func launcherAlias(name string) string {
	if name == "harness-docker" {
		return "docker"
	}
	return name
}

func (s *Server) resolveLauncher(ctx context.Context, machineID, tool, variant, launcherID string) (store.Launcher, error) {
	var item store.Launcher
	var err error
	if launcherID != "" {
		item, err = s.Store.LauncherByID(ctx, machineID, launcherID)
	} else {
		item, err = s.Store.LauncherByName(ctx, machineID, tool, variant)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.Launcher{}, errLauncherUnavailable
		}
		return store.Launcher{}, fmt.Errorf("look up launcher: %w", err)
	}
	if item.Tool != tool || (launcherID == "" && variant != "" && launcherAlias(item.Name) != variant) {
		return store.Launcher{}, errLauncherUnavailable
	}
	return item, nil
}

func launcherSelectionError(w http.ResponseWriter, err error) {
	if errors.Is(err, errLauncherUnavailable) {
		apiError(w, 409, "launcher_unavailable", errLauncherUnavailable.Error())
	} else {
		fail(w, err)
	}
}

func (s *Server) selectedMachine(ctx context.Context, id, tool, variant string, launcherIDs ...string) (*machineConn, store.Machine, error) {
	if id == "" {
		host, err := s.Store.HostMachine(ctx)
		if err != nil {
			return nil, store.Machine{}, err
		}
		id = host.ID
	}
	m, err := s.Store.MachineByID(ctx, id)
	if err != nil {
		return nil, m, err
	}
	if !m.Approved || m.RevokedAt.Valid {
		return nil, m, ErrMachineUnavailable
	}
	conn, err := s.machineForID(id)
	if err != nil {
		return nil, m, err
	}
	if conn.supportsLaunchers && tool != "fake" {
		launcherID := ""
		if len(launcherIDs) > 0 {
			launcherID = launcherIDs[0]
		}
		launcher, err := s.resolveLauncher(ctx, id, tool, variant, launcherID)
		if err != nil {
			return nil, m, err
		}
		if launcher.Containerized && launcher.AgentServerURL == "" && dockerBindIsLoopback() {
			return nil, m, errDockerBindUnavailable
		}
		conn.harnessMu.Lock()
		defer conn.harnessMu.Unlock()
		for _, row := range conn.launchers {
			if row.ID == launcher.ID && row.Available && launcher.Enabled {
				return conn, m, nil
			}
		}
		return nil, m, errLauncherUnavailable
	}
	if dockerNeedsReachableBind(tool, variant) && dockerBindIsLoopback() {
		return nil, m, errDockerBindUnavailable
	}
	conn.harnessMu.Lock()
	rows := append([]protocol.HarnessRow(nil), conn.harnesses...)
	reports := append([]protocol.CapabilityReport(nil), conn.capabilities...)
	conn.harnessMu.Unlock()
	if len(rows) > 0 && tool != "fake" {
		for _, row := range rows {
			if row.Tool == tool && row.Variant == variant && row.Available {
				return conn, m, nil
			}
		}
		return nil, m, ErrMachineUnavailable
	}
	for _, report := range reports {
		if report.Tool == tool && report.Environment.Variant == variant {
			return conn, m, nil
		}
	}
	return nil, m, ErrMachineUnavailable
}

func machineSelectionError(w http.ResponseWriter, m store.Machine, err error) {
	if errors.Is(err, errLauncherUnavailable) {
		apiError(w, 409, "launcher_unavailable", errLauncherUnavailable.Error())
		return
	}
	if errors.Is(err, errDockerBindUnavailable) {
		apiError(w, 409, "invalid_transition", dockerBindUnavailableMessage)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "machine_not_found", "machine not found")
		return
	}
	if !errors.Is(err, ErrMachineUnavailable) {
		fail(w, err)
		return
	}
	if m.RevokedAt.Valid {
		apiError(w, 409, "machine_revoked", "machine revoked")
		return
	}
	if m.ID != "" && !m.Approved {
		apiError(w, 409, "machine_pending", "machine awaits owner approval")
		return
	}
	apiError(w, 503, "machine_unavailable", "no connected machine lists this adapter")
}

func (s *Server) selectedMachinePath(ctx context.Context, projectID string, m store.Machine) (string, error) {
	path, err := s.Store.ProjectMachineFolder(ctx, projectID, m.ID)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", errMissingMachineFolder
	}
	if m.IsHost {
		path, err = filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return "", fmt.Errorf("project folder must exist: %w", err)
		}
		return filepath.Clean(path), nil
	}
	canonical, err := s.probeMachinePath(ctx, m.ID, path)
	if err != nil {
		return "", err
	}
	if err := s.Store.CanonicalizeMachineFolder(ctx, projectID, m.ID, path, canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

func (s *Server) probeMachinePath(ctx context.Context, machineID, path string) (string, error) {
	return s.probeMachinePathForLauncher(ctx, machineID, path, "")
}

func (s *Server) probeMachinePathForLauncher(ctx context.Context, machineID, path, launcherID string) (string, error) {
	machine, err := s.machineForID(machineID)
	if err != nil {
		return "", fmt.Errorf("remote folder verification requires a connected supervisor: %w", err)
	}
	if !machine.supportsPathProbe {
		return "", errors.New("remote folder verification requires an updated supervisor with path preflight support")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(nonce[:])
	channel := make(chan protocol.Frame, 1)
	machine.harnessMu.Lock()
	if machine.pathPending == nil {
		machine.pathPending = make(map[string]chan protocol.Frame)
	}
	machine.pathPending[id] = channel
	machine.harnessMu.Unlock()
	defer func() {
		machine.harnessMu.Lock()
		delete(machine.pathPending, id)
		machine.harnessMu.Unlock()
	}()
	if err := machine.send(protocol.Frame{V: protocol.Version, Type: protocol.PathProbe, ReqID: id, CheckoutPath: path, LauncherID: launcherID}); err != nil {
		return "", err
	}
	probeTimeout := 5 * time.Second
	if launcherID != "" {
		probeTimeout = 65 * time.Second
	}
	timer := time.NewTimer(probeTimeout)
	defer timer.Stop()
	select {
	case reply := <-channel:
		if reply.Code != "" || !filepath.IsAbs(reply.CanonicalPath) || filepath.Clean(reply.CanonicalPath) != reply.CanonicalPath {
			return "", fmt.Errorf("remote folder verification failed: %s", reply.Message)
		}
		return reply.CanonicalPath, nil
	case <-machine.done:
		return "", errors.New("remote folder verification failed; supervisor disconnected or does not support path preflight")
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return "", errors.New("remote folder verification timed out; update the supervisor if it does not support path preflight")
	}
}

var errMissingMachineFolder = errors.New("missing machine folder")

type agentCardWithUsage struct {
	store.AgentCard
	LatestUsage *store.AgentUsage  `json:"latest_usage,omitempty"`
	HealthFlags []store.HealthFlag `json:"health_flags"`
	// Hold says why this agent's next turn will not start although it is idle and accepting work: the limit guard is holding new turns of its tool.
	Hold *agentHold `json:"hold,omitempty"`
	// Warm says whether the agent's process is being kept up for its next turn: State "warm" (with SleepsAt, RFC 3339, when it goes to sleep) or
	// "sleeping" (warm mode is on for it and there is no live process), or "na" for a harness without warm mode.
	Warm *agentWarm `json:"warm,omitempty"`
	// KeepAlive is about the agent's prompt cache being kept warm with pings: what the last ping did and what a stretch of them costs. Absent when
	// keep-alive is off or does not apply to the agent.
	KeepAlive *agentKeepAlive `json:"keep_alive,omitempty"`
}

// agentKeepAlive: IntervalMinutes is the ping interval; PingsPerStretch how many pings an idle stretch can have before the limit of hours without
// real work stops them; EstimateUSD what that many pings cost at the agent's model and current context (cache reads, the model's price list; absent
// when the model has no price or there is no context figure yet). Last* are the last ping's report as the supervisor sent it; Missed says it wrote to
// the cache what it should have read (the cache had expired, or the ping's arguments differed from a real turn's).
type agentKeepAlive struct {
	IntervalMinutes int      `json:"interval_minutes"`
	PingsPerStretch int      `json:"pings_per_stretch"`
	EstimateUSD     *float64 `json:"estimate_usd,omitempty"`
	LastAt          string   `json:"last_at,omitempty"`
	LastOK          *bool    `json:"last_ok,omitempty"`
	LastError       string   `json:"last_error,omitempty"`
	CacheRead       int64    `json:"cache_read,omitempty"`
	CacheWrite      int64    `json:"cache_write,omitempty"`
	Missed          bool     `json:"missed,omitempty"`
	// LastPingAt is the last ping's time (the same instant as LastAt, named for the card's warning); StretchActive says more pings are planned for the
	// agent: its last ping is recent enough that the supervisor is still pinging, and the next one falls before the limit of hours without real work.
	LastPingAt    string `json:"last_ping_at,omitempty"`
	StretchActive bool   `json:"stretch_active"`
}

// agentKeepAliveState works out what a card says about keep-alive (Claude Code and Codex agents; the supervisor pings both).
func (s *Server) agentKeepAliveState(card store.AgentCard, usage *store.AgentUsage, config keepAliveConfig) *agentKeepAlive {
	if !config.Enabled || card.Desired != "active" || (card.Tool != "claude" && card.Tool != "codex") {
		return nil
	}
	interval := config.Minutes[card.Tool]
	if interval <= 0 {
		return nil
	}
	machine, err := s.machineForID(card.MachineID)
	if err != nil || !machine.supportsWarm {
		return nil
	}
	state := &agentKeepAlive{IntervalMinutes: interval}
	if config.StopAfterHrs > 0 {
		state.PingsPerStretch = config.StopAfterHrs * 60 / interval
	}
	machine.warmMu.Lock()
	ping, ok := machine.pings[card.ID]
	machine.warmMu.Unlock()
	// What a stretch costs: the measured cost of the last ping times the pings in a stretch; before there is a ping, the model's price list on the
	// agent's context (cache reads), when it has the model.
	if state.PingsPerStretch > 0 {
		if ok && ping.OK && ping.Cost > 0 {
			total := ping.Cost * float64(state.PingsPerStretch)
			state.EstimateUSD = &total
		} else if usage != nil && usage.ContextUsed != nil {
			zero, out := int64(0), int64(100)
			if each := pricing.Estimate(card.Model, &zero, &out, usage.ContextUsed, &zero); each != nil {
				total := *each * float64(state.PingsPerStretch)
				state.EstimateUSD = &total
			}
		}
	}
	if ok {
		okValue := ping.OK
		state.LastAt, state.LastOK, state.LastError = ping.At, &okValue, ping.Error
		state.CacheRead, state.CacheWrite = ping.CacheRead, ping.CacheWrite
		state.Missed = ping.OK && ping.CacheWrite > ping.CacheRead
		state.LastPingAt = ping.At
		state.StretchActive = keepAliveStretchActive(ping.At, card.LastActivityAt, interval, config.StopAfterHrs, time.Now())
	}
	return state
}

// keepAliveStretchActive tells whether pings are still coming: the last ping is under two intervals old (the supervisor pings once per interval; a
// stretch that ended stops pinging), and when the last real activity is known and a limit of hours is set, the next ping falls before that limit.
func keepAliveStretchActive(pingAt, lastActivity string, intervalMinutes, stopAfterHours int, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, pingAt)
	if err != nil {
		return false
	}
	interval := time.Duration(intervalMinutes) * time.Minute
	if now.Sub(at) >= 2*interval {
		return false
	}
	if stopAfterHours > 0 {
		if activity, err := time.Parse(time.RFC3339, lastActivity); err == nil && !at.Add(interval).Before(activity.Add(time.Duration(stopAfterHours)*time.Hour)) {
			return false
		}
	}
	return true
}

type agentWarm struct {
	State    string `json:"state"`
	SleepsAt string `json:"sleeps_at,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// agentWarmState uses the supervisor's report for tools whose warm mode is on.
func (s *Server) agentWarmState(card store.AgentCard) *agentWarm {
	if card.Desired != "active" {
		return nil
	}
	if card.Tool != "claude" && (card.Tool != "codex" || !s.codexWarmOn.Load()) {
		return &agentWarm{State: "na"}
	}
	machine, err := s.machineForID(card.MachineID)
	if err != nil || !machine.supportsWarm {
		return nil
	}
	machine.warmMu.Lock()
	report := machine.warm
	machine.warmMu.Unlock()
	// What the supervisor itself says decides, never the server's guess: a live idle process is warm; no process is sleeping only when the supervisor says
	// it works to a window (idle_seconds above 0). Before the first report, the card stays cold. A report without idle_seconds (an older supervisor,
	// whose own default may be off) is unknown; a reported zero window means warm mode is off.
	if report == nil {
		return nil
	}
	if card.Observed != "idle" && card.Observed != "in_turn" {
		return nil
	}
	for _, agent := range report.Agents {
		if agent.AgentID == card.ID {
			if agent.State == "busy" && card.Observed == "in_turn" {
				return &agentWarm{State: "working"}
			}
			if agent.State == "idle" && card.Observed == "idle" {
				return &agentWarm{State: "warm", SleepsAt: agent.SleepsAt}
			}
		}
	}
	if card.Observed == "in_turn" {
		return nil
	}
	if report.IdleSeconds == nil {
		return &agentWarm{State: "unknown"}
	}
	if *report.IdleSeconds <= 0 {
		return nil
	}
	warm := &agentWarm{State: "sleeping"}
	for _, refusal := range report.Refusals {
		if refusal.AgentID == card.ID {
			warm.Reason = refusal.Reason
			break
		}
	}
	return warm
}

// agentHold is a reason an idle agent starts no new turn, for its card: Code names it (limit_guard), Text is the short form the card's state
// shows ("claude session 95%"), Detail the sentence behind it.
type agentHold struct {
	Code   string `json:"code"`
	Text   string `json:"text"`
	Detail string `json:"detail"`
}

// agentHolds works out the hold of each card of a list. Only the limit guard holds an idle agent without anything else showing: it judges the
// host machine's account, so only agents on the host machine wait for it (a remote machine has its own login), and a running turn is never
// touched, so only an agent that is not in a turn shows it.
func (s *Server) agentHolds(ctx context.Context, cards []store.AgentCard) map[string]*agentHold {
	on, err := s.dockerOnly(ctx)
	return s.holdsFor(ctx, cards, err == nil && on,
		func(card store.AgentCard) bool {
			refused, err := s.refusedAgent(ctx, card.MachineID, card.Tool, card.Variant, card.LauncherID)
			return err == nil && refused
		},
		func(card store.AgentCard) bool {
			machine, err := s.Store.MachineByID(ctx, card.MachineID)
			return err == nil && machine.IsHost
		})
}

// holdsFor is agentHolds once the setting and the two per-card judgements (is its native launcher refused, is its machine the host) are
// known: the room's agent list reads them for all its cards in one statement, so it passes them in. The judgements are asked at most once per
// card for a refusal and once per machine for the host.
func (s *Server) holdsFor(ctx context.Context, cards []store.AgentCard, dockerOn bool, refusedNative, onHost func(store.AgentCard) bool) map[string]*agentHold {
	holds := map[string]*agentHold{}
	hostByMachine := map[string]bool{}
	byTool := map[string]*agentHold{}
	// "Only run agents in docker": an agent on a native launcher gets no turns, whatever it is doing, and says why.
	if dockerOn {
		for _, card := range cards {
			if card.Desired == "stopped" {
				continue
			}
			if refusedNative(card) {
				holds[card.ID] = &agentHold{Code: "docker_only", Text: "native launcher refused", Detail: errDockerOnly.Error() + ". Queued messages wait."}
			}
		}
	}
	for _, card := range cards {
		if holds[card.ID] != nil {
			continue
		}
		if card.Desired != "active" || card.Observed != "idle" || card.RecoveryBlock || s.Store.ToolHold(card.Tool) == "" {
			continue
		}
		host, known := hostByMachine[card.MachineID]
		if !known {
			host = onHost(card)
			hostByMachine[card.MachineID] = host
		}
		if !host {
			continue
		}
		hold, done := byTool[card.Tool]
		if !done {
			hold = s.toolHold(ctx, card.Tool)
			byTool[card.Tool] = hold
		}
		holds[card.ID] = hold
	}
	return holds
}

// toolHold describes the limit guard's hold on a tool from the guard's own reading: which window is how full against which threshold.
func (s *Server) toolHold(ctx context.Context, tool string) *agentHold {
	hold := &agentHold{Code: "limit_guard", Text: tool + " limit", Detail: "The limit guard is holding new " + tool + " turns (" + s.Store.ToolHold(tool) + "). Queued messages wait; a turn that is running finishes."}
	guard := s.limitGuard()
	if guard == nil {
		return hold
	}
	view, err := guard.View(ctx, tool)
	if err != nil || view.Result.Percent == nil {
		return hold
	}
	percent := fmt.Sprintf("%.0f%%", *view.Result.Percent)
	hold.Text = tool + " " + view.Result.Window + " " + percent
	hold.Detail = fmt.Sprintf("The limit guard is holding new %s turns: the %s limit of the active account is %s full (it holds from %.0f%%). Queued messages wait and start again when usage falls or the account switches; a turn that is running finishes.",
		tool, view.Result.Window, percent, view.Result.Soft)
	return hold
}

// agentNameSuggestion answers a free name for the next agent of the room's project: what the Spawn dialog prefills, and what its dice
// button asks for again with `exclude` set to the name it already shows, so a new roll is never the same name. Nothing is reserved; the spawn itself decides if the name is still free.
func (s *Server) agentNameSuggestion(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	name, err := s.Store.SuggestAgentSlug(r.Context(), room.ProjectID, r.URL.Query().Get("exclude"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"name": name})
}

func (s *Server) roomAgents(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		// Two statements read every agent of the room (cards, usage, health, grants) and one the settings they depend on, so the cost of
		// the list does not grow with the number of agents.
		list, err := s.Store.AgentRoomStates(r.Context(), room.ID, room.ProjectID, userID(r), time.Now(), stuckAfter())
		if err != nil {
			fail(w, err)
			return
		}
		settings, err := s.Store.GlobalSettingValues(r.Context(), append([]string{agentsDockerOnlyKey}, keepAliveSettingKeys...)...)
		if err != nil {
			fail(w, err)
			return
		}
		cards := make([]store.AgentCard, len(list.Agents))
		byID := make(map[string]store.AgentRoomState, len(list.Agents))
		for i, state := range list.Agents {
			cards[i] = state.Card
			byID[state.Card.ID] = state
		}
		// A viewer is shown what the agents are doing, not where on the machine's disk they work: the absolute paths on a card are blanked.
		if list.ViewerRole == "viewer" {
			for i := range cards {
				cards[i] = cardWithoutPaths(cards[i])
			}
		}
		withUsage := make([]agentCardWithUsage, 0, len(cards))
		holds := s.holdsFor(r.Context(), cards, settings[agentsDockerOnlyKey] == "1",
			func(card store.AgentCard) bool {
				state := byID[card.ID]
				return card.Tool != "fake" && (card.LauncherID == "" && card.Variant != "docker" || card.LauncherID != "" && state.LauncherKnown && !state.LauncherContainerized)
			},
			func(card store.AgentCard) bool { return byID[card.ID].MachineIsHost })
		keepConfig := keepAliveConfigFrom(settings)
		for _, card := range cards {
			state := byID[card.ID]
			flags := state.Flags
			if card.Desired != "stopped" {
				if _, err := s.machineForID(card.MachineID); err != nil {
					flags = append(flags, store.HealthFlag{Code: "machine_offline"})
				}
			}
			warm := s.agentWarmState(card)
			if warm != nil && warm.Reason != "" && list.ViewerRole == "viewer" {
				warm.Reason = "Warm mode is unavailable for this agent's configuration"
			}
			withUsage = append(withUsage, agentCardWithUsage{card, state.Usage, flags, holds[card.ID], warm, s.agentKeepAliveState(card, state.Usage, keepConfig)})
		}
		writeJSON(w, 200, withUsage)
		return
	}
	var req spawnRequest
	if !readJSON(w, r, &req) {
		return
	}
	s.spawnAgentRequest(w, r, room, req, "user", userID(r))
}

// cardWithoutPaths is the card as a viewer sees it: boundary grant paths are hidden.
func cardWithoutPaths(card store.AgentCard) store.AgentCard {
	if len(card.BoundaryGrants) > 0 {
		grants := make([]store.BoundaryGrant, len(card.BoundaryGrants))
		for i, grant := range card.BoundaryGrants {
			grant.Path = ""
			grants[i] = grant
		}
		card.BoundaryGrants = grants
	}
	return card
}

func (s *Server) spawnAgentRequest(w http.ResponseWriter, r *http.Request, room store.Room, req spawnRequest, actorKind, actorID string) {
	if room.Kind == "dm" {
		apiError(w, 400, "invalid_kind", "private chats cannot add agents")
		return
	}
	if req.RoleKey == "" {
		apiError(w, 400, "invalid_request", "role_key required")
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if len(req.Reason) > 500 {
		apiError(w, 400, "invalid_request", "reason is too long")
		return
	}
	if req.Name != "" {
		if req.ResumeLegacy != "" {
			apiError(w, 400, "invalid_request", "a v1 agent keeps its own name; leave name out of resume_legacy")
			return
		}
		if !store.ValidAgentSlug(req.Name) {
			apiError(w, 400, "invalid_name", "a name is 3 to 21 characters: lowercase letters, digits, - and _, starting with a letter, ending with a letter or digit, and not a group tag such as all or here")
			return
		}
	}
	if req.ResumeSession != "" {
		switch {
		case actorKind != "user":
			apiError(w, 403, "forbidden", "only a person continues a Claude Code session")
			return
		case req.ResumeLegacy != "":
			apiError(w, 400, "invalid_request", "continue either a v1 agent or a Claude Code session, not both")
			return
		case !ccsessions.ValidID(req.ResumeSession):
			apiError(w, 400, "invalid_request", "resume_session is not a Claude Code session id")
			return
		case req.Worktree:
			apiError(w, 400, "invalid_request", "a continued session runs where it was made, so it cannot use a worktree")
			return
		case req.ResumeMode != "" && req.ResumeMode != resumeContinue && req.ResumeMode != resumeFork:
			apiError(w, 400, "invalid_request", "resume_mode is continue or fork")
			return
		}
		if req.ResumeMode == "" {
			// Also what the idempotency digest below sees, so leaving it out and asking for fork are one request. Logged, because the
			// default is a copy where an earlier build continued: a caller that meant to continue says so.
			log.Printf("spawn: resume_mode not given for Claude Code session %s by %s %s, defaulting to fork", req.ResumeSession, actorKind, actorID)
			req.ResumeMode = resumeFork
		}
	} else if req.ResumeLegacy != "" && !req.NewRoom && !req.ConfirmSessionClosed {
		// A v1 agent's Claude Code session can be continued (the default, as it always was) or worked on as a copy.
		if req.ResumeMode != "" && req.ResumeMode != resumeContinue && req.ResumeMode != resumeFork {
			apiError(w, 400, "invalid_request", "resume_mode is continue or fork")
			return
		}
	} else if req.NewRoom || req.ConfirmSessionClosed || req.ResumeMode != "" {
		apiError(w, 400, "invalid_request", "new_room, confirm_session_closed and resume_mode belong to resume_session or resume_legacy")
		return
	}
	if req.WorkOnCopy {
		if req.ResumeSession == "" && req.ResumeLegacy == "" {
			apiError(w, 400, "invalid_request", "work_on_copy belongs to resume_session or resume_legacy")
			return
		}
		if req.ResumeMode == resumeContinue || req.Worktree {
			apiError(w, 400, "invalid_request", "working on a copy in a worktree of its own forks the session: use resume_mode fork, and no worktree option")
			return
		}
		req.ResumeMode = resumeFork
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		apiError(w, 400, "invalid_request", "Idempotency-Key required (at most 200 characters)")
		return
	}
	requestBytes, _ := json.Marshal(struct {
		Room    string       `json:"room"`
		Request spawnRequest `json:"request"`
	}{room.ID, req})
	digestBytes := sha256.Sum256(requestBytes)
	digest := hex.EncodeToString(digestBytes[:])
	prior, found, err := s.Store.SpawnRequestResult(r.Context(), actorKind, actorID, key, digest)
	if err != nil {
		spawnError(w, err)
		return
	}
	if found {
		var card store.AgentCard
		if err := json.Unmarshal([]byte(prior), &card); err != nil {
			fail(w, err)
			return
		}
		if !s.ensureSpawnDelivery(w, r, card) {
			return
		}
		writeJSON(w, 201, card)
		return
	}
	if err := s.Store.Tx(r.Context(), func(tx *store.Tx) error { return tx.AuthorizeSpawn(room.ID, actorKind, actorID) }); err != nil {
		spawnError(w, err)
		return
	}
	// The cursor is checked in the successful spawn transaction. Slow checkout
	// preparation does not serialize spawns in unrelated rooms or projects.
	selection, defaultAvailable, err := s.nextRoleSpawnDefault(r.Context(), room.ProjectID, req.RoleKey, req.MachineID)
	if err != nil {
		definitionError(w, err)
		return
	}
	defaultUsed, defaultUnavailable := false, len(selection.Defaults.DefaultAgents) > 0 && !defaultAvailable
	if req.Tool == "" && req.Variant == "" && req.LauncherID == "" && req.Model == "" && defaultAvailable {
		req.MachineID, req.Tool, req.Variant, req.LauncherID, req.Model = selection.Request.MachineID,
			selection.Request.Tool, selection.Request.Variant, selection.Request.LauncherID, selection.Request.Model
	}
	if defaultAvailable {
		if err := s.inheritRoleDefaultMachine(r.Context(), &req, selection); err != nil {
			fail(w, err)
			return
		}
	}
	if req.Tool == "" || (req.Variant == "" && req.LauncherID == "") || (req.Model == "" && req.LauncherID == "") {
		if defaultUnavailable {
			if err := s.Store.WarnRoleSpawnUnavailable(r.Context(), room.ID, req.RoleKey); err != nil {
				fail(w, err)
				return
			}
		}
		if req.Tool != "" && req.Model != "" && req.Variant == "" && req.LauncherID == "" {
			apiError(w, 400, "invalid_request", "add variant (e.g. docker) for the tool and model, or give launcher_id")
			return
		}
		apiError(w, 400, "invalid_request", "tool, and either launcher_id (model optional: the harness default) or variant and model required; no available role default could fill them")
		return
	}
	// A keyed retry replays the stored result above even if the model has
	// since left the harness's list; only a new request is checked against it.
	listed, err := s.listedAdapter(r.Context(), req)
	if err != nil {
		fail(w, err)
		return
	}
	if !listed {
		apiError(w, 400, "adapter_unlisted", "tool and provider must be listed by an adapter")
		return
	}
	if err := s.Store.Tx(r.Context(), func(tx *store.Tx) error { return tx.AuthorizeSpawn(room.ID, actorKind, actorID) }); err != nil {
		spawnError(w, err)
		return
	}
	// Before anything is prepared for the agent (a worktree, a folder check): a name that is already in use is the commonest refusal.
	// The check in the spawn transaction stays the arbiter when two requests race for the same name.
	if req.Name != "" {
		if err := s.Store.SlugInUse(r.Context(), room.ProjectID, req.Name); err != nil {
			spawnError(w, err)
			return
		}
	}
	var legacy store.LegacyStart
	resuming := false
	if req.ResumeLegacy != "" {
		var ok bool
		if legacy, resuming, ok = s.legacyStartChecks(w, r, room, actorKind, &req); !ok {
			return
		}
	}
	machine, machineInfo, err := s.selectedMachine(r.Context(), req.MachineID, req.Tool, req.Variant, req.LauncherID)
	if err != nil {
		if errors.Is(err, ErrMachineUnavailable) && machineInfo.ID != "" && machineInfo.Approved && !machineInfo.RevokedAt.Valid {
			apiError(w, 503, "machine_unavailable", "the selected machine is disconnected or does not list this adapter; reconnect it and retry")
			return
		}
		machineSelectionError(w, machineInfo, err)
		return
	}
	launcherID := ""
	containerized := launcherKeepsOwnSessions(nil, req.Variant)
	var resolvedLauncher *store.Launcher
	if machine.supportsLaunchers && req.Tool != "fake" {
		launcher, err := s.resolveLauncher(r.Context(), machine.id, req.Tool, req.Variant, req.LauncherID)
		if err != nil {
			launcherSelectionError(w, err)
			return
		}
		launcherID = launcher.ID
		req.Variant = launcherAlias(launcher.Name)
		// What matters is whether the launcher runs in a container (its own home), not what it is called.
		containerized = launcherKeepsOwnSessions(&launcher, req.Variant)
		resolvedLauncher = &launcher
	}
	if refused, err := s.refusedNative(r.Context(), req.Tool, req.Variant, resolvedLauncher); err != nil {
		fail(w, err)
		return
	} else if refused {
		apiError(w, 409, "docker_only", errDockerOnly.Error())
		return
	}
	{
		model, ok, err := s.checkSpawnModel(r.Context(), machine.id, req.Tool, req.Variant, launcherID, req.Model)
		if err != nil {
			fail(w, err)
			return
		}
		if !ok {
			apiError(w, 400, "model_unlisted", "the model is not one this harness offers; add it under Settings → Harnesses & models")
			return
		}
		req.Model = model
	}
	if defaultAvailable {
		launcherMatches := launcherID == selection.Request.LauncherID || launcherID == "" &&
			(req.LauncherID == selection.Request.LauncherID || req.LauncherID == "" && req.Variant == selection.Request.Variant)
		defaultUsed = req.Tool == selection.Request.Tool && req.Model == selection.Request.Model &&
			launcherMatches && machine.id == selection.Request.MachineID
		if !defaultUsed && actorKind == "agent" && req.Reason == "" {
			apiError(w, 400, "reason_required", "give a reason for choosing a different harness or model than the role default")
			return
		}
	}
	// A Docker launcher that works in the host's Claude home sees the sessions made there, so they can be continued through it.
	sharedHome := containerized && req.Tool == "claude" && sharesClaudeHome(machine, resolvedLauncher)
	if (resuming || req.ResumeSession != "") && containerized && !sharedHome {
		apiError(w, 409, "needs_native", sessionsMessage(req.Tool, resolvedLauncher))
		return
	}
	if resuming && req.Tool != "claude" && !machine.supportsLegacySessionCopy {
		apiError(w, 409, "supervisor_update_needed", "this machine's supervisor cannot copy v1 sessions into a private agent runtime; update it before continuing")
		return
	}
	var session *protocol.SessionCandidate
	if req.ResumeSession != "" {
		var ok bool
		if session, ok = s.resumableSession(w, r, machine, machineInfo, launcherID, &req); !ok {
			return
		}
	}
	var path string
	var sessionSpelling string // the folder a continued session was saved under, when that is another spelling of path
	var worktreeItem *store.RoomWorktree
	if session != nil {
		// A continued session runs where it was made, not in the room's checkout.
		if path, err = s.resumeCWD(r.Context(), machineInfo, session.CWD, "the folder the session worked in"); err != nil {
			apiError(w, 409, "session_folder_missing", err.Error())
			return
		}
		if !sessionFoundFrom(*session, path) {
			// Claude saved the session under a spelling of this folder (a link to it) and finds it only when started there as spelled. That
			// is allowed when the spelling really is this directory: the agent keeps the canonical path, and its turns start in the spelling.
			if !sameDirectory(machineInfo, session.CWD, path) {
				apiError(w, 409, "session_folder_alias", "Claude saved this session under "+session.CWD+", which is not the folder "+path+" it resolves to here, so it would not find the session; open "+session.CWD+" in Claude once and continue it from the terminal, or start a new agent")
				return
			}
			if !machine.supportsLaunchPath {
				apiError(w, 409, "supervisor_update_needed", "this session was saved under a link to its folder ("+session.CWD+"); the supervisor on "+machineInfo.Name+" predates starting Claude in that spelling, so update it")
				return
			}
			sessionSpelling = filepath.Clean(session.CWD)
		}
	} else if resuming {
		if req.ResumeMode == resumeFork && !machine.supportsSessionFork {
			apiError(w, 409, "supervisor_update_needed", "this machine's supervisor predates working on a copy of a session; update it, or continue the session instead")
			return
		}
		// A resumed session runs where it was made (see legacyResumeCWD), not in the room's checkout.
		if path, err = s.legacyResumeCWD(r.Context(), machineInfo, legacy.CWD); err != nil {
			apiError(w, 409, "v1_folder_missing", err.Error())
			return
		}
		// v1 ran Claude in the folder as it spelled it, and Claude keyed the session by that spelling: when it is a link to the folder
		// (path is its resolved form), the turns start there, exactly as for a session picked from the list.
		launch := filepath.Clean(legacy.CWD)
		if req.Tool == "claude" && launch != path && filepath.IsAbs(launch) && sameDirectory(machineInfo, launch, path) {
			if !machine.supportsLaunchPath {
				apiError(w, 409, "supervisor_update_needed", "v1 worked in this folder through a link ("+launch+"); the supervisor on "+machineInfo.Name+" predates starting Claude in that spelling, so update it")
				return
			}
			sessionSpelling = launch
		}
		if machineInfo.IsHost && (!containerized || sharedHome) {
			look := path
			if sessionSpelling != "" {
				look = sessionSpelling
			}
			if checkHarnessSession(req.Tool, legacy.SessionID, look) == sessionMissing {
				apiError(w, 409, "session_not_found", "the v1 session "+legacy.SessionID+" was not found on this machine, so it cannot be continued")
				return
			}
		}
	} else {
		path, worktreeItem, err = s.roomCheckoutPath(r.Context(), room, machineInfo, req.Worktree, req.BaseRef)
		if errors.Is(err, errMissingMachineFolder) {
			apiError(w, 409, "machine_folder_missing", "set this project's folder on "+machineInfo.Name)
			return
		}
		if err != nil {
			worktreeError(w, err)
			return
		}
	}
	slug := req.Name
	if req.ResumeLegacy != "" {
		slug = legacy.Slug // the agent keeps its v1 name, whatever the start; its worktree is named by it
	}
	var own *store.AgentWorktree
	var newRoomID string // the ID the new room is created with, when a worktree is named after it
	spawned := false
	defer func() { // the worktree's lock is held to here: a failed request cleans up before anyone else can use or remove it
		if !spawned {
			s.discardAgentWorktree(r.Context(), own)
		}
		own.Done()
	}()
	if req.WorkOnCopy && (session != nil || resuming) {
		// The session is copied to a worktree made for this agent and forked there. The original folder stays in place.
		// The agent's own name names the worktree: a continued v1 agent keeps its v1 name.
		handle := req.ResumeSession
		if session == nil {
			handle = legacy.SessionID
		}
		if !machine.supportsSessionCopy {
			apiError(w, 409, "supervisor_update_needed", "this machine's supervisor predates working on a copy in a worktree of its own; update it or continue the session in its current folder")
			return
		}
		// Copying puts the transcript into this project, so a session made outside the project's folder needs the owner's approval first,
		// as continuing it where it is does (the boundary check on its own folder); a copy cannot ask, so it is refused.
		if inside, insideErr := s.Store.ProjectFolderContains(r.Context(), room.ProjectID, machine.id, path); insideErr != nil {
			fail(w, insideErr)
			return
		} else if !inside {
			apiError(w, 409, "session_outside_folder", "this session was made outside the project folder, so it cannot be copied into the project: continue it where it is (the owner approves that), or start a new agent")
			return
		}
		// An agent that starts in a new room is named after that room, so a later start finds the branch it kept: the room's ID is chosen now.
		named, baseRef := room, req.BaseRef
		if req.NewRoom && session != nil {
			if newRoomID, err = store.NewRoomID(); err != nil {
				fail(w, err)
				return
			}
			if baseRef == "" {
				if baseRef, err = s.Store.RoomBaseRef(r.Context(), room.ID); err != nil {
					fail(w, err)
					return
				}
			}
			named.ID = newRoomID
		}
		var reason string
		if own, reason = s.agentWorktreeFor(r.Context(), named, machineInfo, &slug, baseRef); own == nil {
			apiError(w, 409, "worktree_unavailable", "this session cannot be worked on as a copy in a worktree of its own: "+reason)
			return
		}
		if err := s.copyClaudeSession(r.Context(), machine, launcherID, handle, own.Path); err != nil {
			apiError(w, 409, "session_copy_failed", err.Error())
			return
		}
		path, sessionSpelling = own.Path, ""
	}
	if err := s.validateLauncherCheckout(r.Context(), machineInfo, launcherID, path); err != nil {
		apiError(w, 409, "folder_unverified", err.Error())
		return
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		fail(w, err)
		return
	}
	agentKey := "aak_" + base64.RawURLEncoding.EncodeToString(secret)
	keyHashBytes := sha256.Sum256([]byte(agentKey))
	var card store.AgentCard
	// The spawn runs where the room works (path). Workers may share that checkout;
	// an agent-specific worktree is used only for an explicit session copy.
	spawn := func(where string, own *store.AgentWorktree) (string, bool, error) {
		return s.Store.WithRequestKey(r.Context(), store.RequestKey{ActorKind: actorKind, ActorID: actorID, Kind: "spawn", Key: key, Digest: digest}, func(tx *store.Tx) (string, error) {
			input := store.SpawnInput{RoomID: room.ID, ActorKind: actorKind, ActorID: actorID, MachineID: machine.id, Path: where, RoleKey: req.RoleKey, Tool: req.Tool, Variant: req.Variant, LauncherID: launcherID, Model: req.Model, Provider: req.Provider, JoinParent: req.JoinParent, KeyHash: hex.EncodeToString(keyHashBytes[:])}
			if actorKind == "agent" {
				input.ApprovedBy = req.ApprovedBy
			}
			input.Slug = slug
			input.LaunchPath = sessionSpelling // the agent's turns start here; the checkout itself stays the canonical path
			if session != nil {
				input.ResumeHandle = session.ID
				input.ResumeSession = &store.ResumeSession{ID: session.ID, CWD: path, Title: session.Title, Fork: req.ResumeMode == resumeFork}
				if req.NewRoom {
					input.NewRoomTitle, input.NewRoomID = sessionRoomTitle(*session), newRoomID
				}
			}
			if defaultUsed {
				input.SpawnNote = "spawned per role default: " + req.Tool + "/" + req.Variant + "/" + req.Model
			} else if defaultAvailable && actorKind == "agent" {
				input.SpawnNote = "Different from the role default: " + req.Reason
			} else if defaultUnavailable {
				input.SpawnNote = "Role defaults are unavailable; a manual harness and model were used."
			}
			if req.ResumeLegacy != "" {
				input.Slug, input.ResumeLegacyID = legacy.Slug, legacy.AgentID
				if resuming {
					input.ResumeHandle = legacy.SessionID
					if req.ResumeMode == resumeFork {
						// Worked on as a copy: the first turn forks the v1 session, the original stays as it is.
						input.ResumeSession = &store.ResumeSession{ID: legacy.SessionID, CWD: path, Title: "", Fork: true}
					}
				}
			}
			created, err := tx.SpawnAgent(input)
			if err != nil {
				return "", err
			}
			if own != nil {
				own.AgentID = created.ID
				own.RoomID = created.RoomID // the room the agent is in (a continued session may start in a room of its own), not the one asked
				if err := tx.RecordAgentWorktree(*own); err != nil {
					return "", err
				}
			}
			if defaultUsed && selection.Defaults.PickMode == "in_turn" {
				if err := tx.AdvanceRoleSpawnCursor(room.ProjectID, selection.Defaults.RoleID, selection.Defaults.NextPosition, selection.Steps); err != nil {
					return "", err
				}
			}
			created.DefaultUsed = defaultUsed
			if defaultUsed {
				created.DefaultSelection = &selection.Choice
			}
			if worktreeItem != nil && worktreeItem.DirtyBaseCount > 0 {
				created.WorktreeWarning = fmt.Sprintf("the base has %d uncommitted changes, which are not in the worktree", worktreeItem.DirtyBaseCount)
			}
			if own != nil && own.DirtyBase > 0 {
				created.WorktreeWarning = fmt.Sprintf("the base has %d uncommitted changes, which are not in the worktree", own.DirtyBase)
			}
			encoded, err := json.Marshal(created)
			return string(encoded), err
		})
	}
	result, reused, err := spawn(path, own)
	// A replay of an earlier request with the same key stored that request's result: the worktree made for this one is not recorded.
	spawned = err == nil && !reused
	if err != nil {
		spawnError(w, err)
		return
	}
	if err = json.Unmarshal([]byte(result), &card); err != nil {
		fail(w, err)
		return
	}
	if !reused {
		s.spawnMu.Lock()
		cacheID := fmt.Sprintf("%s/%d", card.ID, card.Generation)
		if s.spawnKeys[cacheID] == "" {
			s.spawnKeys[cacheID] = agentKey
		}
		s.spawnMu.Unlock()
	}
	if !s.ensureSpawnDelivery(w, r, card) {
		return
	}
	writeJSON(w, 201, card)
}

func catalogModels(models []store.HarnessModel) []spawnCatalogModel {
	out := make([]spawnCatalogModel, 0, len(models))
	for _, model := range models {
		out = append(out, spawnCatalogModel{ID: model.Slug, Label: model.Label, Note: model.Note, Default: model.Default})
	}
	return out
}

// harnessLabel is what the owner calls a harness; a harness saved before labels existed falls back to the adapter's name.
func harnessLabel(definition store.Launcher, fallback string) string {
	if definition.Label != "" {
		return definition.Label
	}
	return fallback + " · " + definition.Name
}

// listedAdapter reports whether the tool and provider pairing is one the adapters support (fixed in code). The model is checked against
// the list of the harness the agent will run on, once that harness is known (see modelListedByHarness).
func (s *Server) listedAdapter(ctx context.Context, req spawnRequest) (bool, error) {
	if req.Tool == "fake" && req.Variant == "test" && req.Model == "test" {
		return req.Provider == "" || req.Provider == "fake", nil
	}
	for _, allowed := range boundedAdapters {
		if req.Tool == allowed.tool && (req.Provider == "" || req.Provider == allowed.provider) {
			return true, nil
		}
	}
	return false, nil
}

// modelListedByHarness checks that the model is one the harness offers; an empty model means the harness default, which is returned.
// A harness with no models offers nothing to spawn.
func (s *Server) modelListedByHarness(ctx context.Context, launcherID, model string) (string, bool, error) {
	models, err := s.Store.HarnessModels(ctx, launcherID)
	if err != nil {
		return "", false, err
	}
	for _, item := range models {
		if model == "" && item.Default || model != "" && item.Slug == model {
			return item.Slug, true, nil
		}
	}
	return "", false, nil
}

func (s *Server) ensureSpawnDelivery(w http.ResponseWriter, r *http.Request, card store.AgentCard) bool {
	pending, needsSpawn, err := s.Store.PendingSpawnByID(r.Context(), card.ID)
	if err != nil {
		fail(w, err)
		return false
	}
	if !needsSpawn {
		return true
	}
	machine, err := s.machineForDispatch(pending.MachineID)
	if err != nil {
		if errors.Is(err, ErrMachineUnavailable) {
			apiError(w, 503, "machine_unavailable", "agent created but supervisor is disconnected; retry with the same key after it reconnects")
		} else {
			fail(w, fmt.Errorf("find spawn machine: %w", err))
		}
		return false
	}
	if err := s.sendPendingSpawn(r.Context(), machine, pending); err != nil {
		if errors.Is(err, ErrMachineUnavailable) || errors.Is(err, ErrSlowMachine) {
			apiError(w, 503, "machine_unavailable", "agent created but spawn delivery could not reach the supervisor; retry with the same key after it reconnects")
		} else {
			fail(w, fmt.Errorf("deliver spawn to supervisor: %w", err))
		}
		return false
	}
	return true
}

func (s *Server) sendPendingSpawn(ctx context.Context, machine *machineConn, p store.PendingSpawn) error {
	allowed, err := s.Store.EnsureSpawnBoundary(ctx, p.AgentID)
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	cacheID := fmt.Sprintf("%s/%d", p.AgentID, p.Generation)
	s.spawnMu.Lock()
	key := s.spawnKeys[cacheID]
	if key == "" {
		var err error
		key, err = s.Auth.IssueAgentKey(ctx, p.AgentID)
		if err != nil {
			s.spawnMu.Unlock()
			return err
		}
		s.spawnKeys[cacheID] = key
	}
	s.spawnMu.Unlock()
	machine.boundMu.Lock()
	machine.bound[p.AgentID] = p.Generation
	machine.boundMu.Unlock()
	dockerOnly, err := s.dockerOnly(ctx)
	if err != nil {
		return err
	}
	return machine.send(protocol.Frame{V: protocol.Version, Type: protocol.Spawn, ReqID: fmt.Sprintf("%s/%d", p.AgentID, p.Generation), AgentID: p.AgentID, Generation: p.Generation, DesiredRev: p.DesiredRev, AgentKey: key, DockerOnly: dockerOnly, CheckoutPath: p.CheckoutPath, Agent: &adapter.AgentSpec{ID: p.AgentID, Tool: p.Tool, Variant: p.Variant, LauncherID: p.LauncherID, Model: p.Model, Provider: p.Provider}})
}

func (s *Server) enqueuePendingSpawns(ctx context.Context, machine *machineConn) error {
	pending, err := s.Store.PendingSpawnsForMachine(ctx, machine.id)
	if err != nil {
		return err
	}
	for _, p := range pending {
		if err := s.sendPendingSpawn(ctx, machine, p); err != nil {
			return err
		}
	}
	return nil
}

func spawnError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrReviewerAgentsDisabled):
		apiError(w, 409, "reviewer_agents_disabled", "Reviewer agents are disabled for this project")
	case errors.Is(err, store.ErrTeamFlowRule):
		apiError(w, 409, "team_flow_rule", "this team flow does not allow that role to be spawned here by this actor")
	case errors.Is(err, store.ErrJoinParentRefused):
		apiError(w, 409, "join_parent_refused", "join_parent cannot join the new agent to the room's parent: the room has no parent, the parent is the home room or is not open, or the team flow keeps this role out of it")
	case errors.Is(err, store.ErrProjectArchived):
		apiError(w, 409, "project_archived", "this project is archived and read-only; reopen it to change anything")
	case errors.Is(err, store.ErrInvalidScope):
		apiError(w, 409, "invalid_scope", "bulk controls are unavailable in private chats")
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "actor is not allowed to manage this agent")
	case errors.Is(err, store.ErrHomeLeadOnly):
		apiError(w, 409, "home_is_lead_only", "this project's home room accepts only lead roles; spawn other agents into a task room such as general, or turn off Home room: lead only in the project settings")
	case errors.Is(err, store.ErrRoleOccupied):
		apiError(w, 409, "role_occupied", "project already has an active agent in this singleton role")
	case errors.Is(err, store.ErrSpawnLimit):
		apiError(w, 429, "spawn_limit", "agent or spawn rate limit reached")
	case errors.Is(err, store.ErrSlugTaken):
		var inUse *store.NameInUseError
		if errors.As(err, &inUse) {
			apiError(w, 409, "name_in_use", inUse.Error())
		} else {
			apiError(w, 409, "name_in_use", "this project already has an agent, person or role with that name, or a person has that login")
		}
	case errors.Is(err, store.ErrSessionAttached):
		apiError(w, 409, "already_started", "an agent already continues this Claude Code session; use that agent")
	case errors.Is(err, store.ErrLegacyStarted):
		apiError(w, 409, "already_started", "this v1 agent was already started as a v2 agent; restart that agent instead")
	case errors.Is(err, store.ErrRoleSpawnCursorChanged):
		apiError(w, 409, "role_default_changed", "another agent used this role default first; retry the spawn")
	case errors.Is(err, store.ErrConflict):
		apiError(w, 409, "idempotency_conflict", "key reused with different request")
	case errors.Is(err, store.ErrScopeMismatch):
		apiError(w, 409, "checkout_scope_mismatch", "the selected checkout does not belong to this project and machine")
	case errors.Is(err, store.ErrStaleRev):
		apiError(w, 409, "stale_rev", "agent desired revision changed")
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 400, "invalid_request", "role or room does not exist")
	case errors.Is(err, store.ErrInvalidTransition):
		apiError(w, 409, "invalid_transition", "room or agent state disallows this operation")
	default:
		fail(w, err)
	}
}

func (s *Server) agentControl(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Desired    string `json:"desired"`
		Rev        int64  `json:"rev"`
		StartFresh bool   `json:"start_fresh"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Desired != "active" && req.Desired != "not_accepting" && req.Desired != "stopped" {
		apiError(w, 400, "invalid_request", "desired must be active, not_accepting, or stopped")
		return
	}
	if req.StartFresh && req.Desired != "active" {
		apiError(w, 400, "invalid_request", "start_fresh requires desired active")
		return
	}
	id := r.PathValue("id")
	// Who may do this is decided first, so a person who may not (or an id that is no agent) gets the same refusal without anything having been
	// loaded, probed or sent to a machine on their behalf.
	if err := s.Store.AuthorizeAgentControl(r.Context(), id, userID(r)); err != nil {
		fail(w, err)
		return
	}
	// An agent that works in a worktree of its own is started under that worktree's lock, so a removal cannot take the folder from it
	// between the check and the start. It is let go before the fallback below, which takes the same lock itself.
	release := sync.OnceFunc(func() {})
	defer func() { release() }()
	if req.Desired == "active" {
		// Starting is the one control an archived project refuses: say so before the launcher, Docker and remote-folder checks, which
		// would answer about things that cannot matter. (Stopping and pausing pass, to free an agent.) The store refuses it as well.
		if archived, err := s.Store.ArchivedScope(r.Context(), userID(r), "agent", id); err == nil && archived {
			apiError(w, 409, "project_archived", "this project is archived and read-only; reopen it to change anything")
			return
		}
		card, err := s.Store.AgentCardByID(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		if card.Tool != "fake" {
			if card.LauncherID != "" {
				launcher, err := s.Store.LauncherByID(r.Context(), card.MachineID, card.LauncherID)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					fail(w, err)
					return
				}
				if err != nil || !launcher.Enabled {
					apiError(w, 409, "launcher_unavailable", errLauncherUnavailable.Error())
					return
				}
				if launcher.Containerized && launcher.AgentServerURL == "" && dockerBindIsLoopback() {
					apiError(w, 409, "invalid_transition", dockerBindUnavailableMessage)
					return
				}
			} else if dockerNeedsReachableBind(card.Tool, card.Variant) && dockerBindIsLoopback() {
				apiError(w, 409, "invalid_transition", dockerBindUnavailableMessage)
				return
			}
		}
		if card.Desired == "stopped" {
			machine, err := s.Store.MachineByID(r.Context(), card.MachineID)
			if err != nil {
				fail(w, err)
				return
			}
			if !machine.IsHost {
				path, err := s.Store.AgentCheckoutPath(r.Context(), id)
				if err == nil {
					var canonical string
					canonical, err = s.probeMachinePath(r.Context(), machine.ID, path)
					if err == nil && canonical != path {
						err = errors.New("checkout path changed on the remote machine; remap before reactivation")
					}
				}
				if err != nil {
					apiError(w, 409, "folder_unverified", err.Error())
					return
				}
			}
		}
	}
	var updated store.AgentCard
	// The lock is taken here, after the request's own checks and just before the transaction, which is what authorizes the caller: one who
	// may not start the agent holds it only for as long as that refusal takes.
	if req.Desired == "active" {
		release = sync.OnceFunc(s.lockOwnWorktree(r.Context(), id))
	}
	err := s.Store.Tx(r.Context(), func(tx *store.Tx) error {
		var err error
		if req.StartFresh {
			updated, err = tx.StartAgentFresh(id, userID(r), req.Rev)
		} else {
			updated, err = tx.SetAgentDesired(id, userID(r), req.Desired, req.Rev)
		}
		return err
	})
	release()
	if err != nil {
		spawnError(w, err)
		return
	}
	card, err := s.Store.AgentCardByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	machine, err := s.machineForID(card.MachineID)
	if err == nil {
		machine.boundMu.Lock()
		machine.bound[id] = updated.Generation
		machine.boundMu.Unlock()
		_ = machine.sendControl(protocol.Frame{V: protocol.Version, Type: protocol.Control, AgentID: id, Generation: updated.Generation, DesiredRev: updated.DesiredRev, DesiredState: updated.Desired})
		if updated.Desired == "active" {
			if pending, ok, err := s.Store.PendingSpawnByID(r.Context(), id); err == nil && ok {
				_ = s.sendPendingSpawn(r.Context(), machine, pending)
			}
		}
	}
	writeJSON(w, 200, card)
}

func (s *Server) agentSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RoomID     string `json:"room_id"`
		Tool       string `json:"tool"`
		Variant    string `json:"variant"`
		LauncherID string `json:"launcher_id,omitempty"`
		Model      string `json:"model"`
		MachineID  string `json:"machine_id,omitempty"`
		Rev        int64  `json:"rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		apiError(w, 400, "invalid_request", "Idempotency-Key required (at most 200 characters)")
		return
	}
	if req.RoomID == "" {
		apiError(w, 400, "invalid_request", "room_id required")
		return
	}
	id := r.PathValue("id")
	digestBytes := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d", id, req.RoomID, req.Tool, req.Variant, req.LauncherID, req.Model, req.MachineID, req.Rev)))
	requestKey := store.RequestKey{ActorKind: "user", ActorID: userID(r), Kind: "switch", Key: key, Digest: hex.EncodeToString(digestBytes[:])}
	if _, found, err := s.Store.RequestResult(r.Context(), requestKey); err != nil {
		spawnError(w, err)
		return
	} else if found {
		current, err := s.Store.AgentCardByID(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, current)
		return
	}
	// As with spawn, a keyed retry replays above even if the model has since
	// left the harness's list; only a new request is checked against it.
	choice := spawnRequest{Tool: req.Tool, Variant: req.Variant, Model: req.Model}
	listed, err := s.listedAdapter(r.Context(), choice)
	if err != nil {
		fail(w, err)
		return
	}
	if !listed {
		apiError(w, 400, "adapter_unlisted", "tool and provider must be listed by an adapter")
		return
	}
	provider := "fake"
	for _, allowed := range boundedAdapters {
		if allowed.tool == req.Tool {
			provider = allowed.provider
			break
		}
	}
	current, err := s.Store.AgentCardByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	targetID := req.MachineID
	if targetID == "" {
		targetID = current.MachineID
	}
	selectedConn, machineInfo, err := s.selectedMachine(r.Context(), targetID, req.Tool, req.Variant, req.LauncherID)
	if err != nil {
		machineSelectionError(w, machineInfo, err)
		return
	}
	launcherID := ""
	var switchLauncher *store.Launcher
	if selectedConn.supportsLaunchers && req.Tool != "fake" {
		launcher, err := s.resolveLauncher(r.Context(), targetID, req.Tool, req.Variant, req.LauncherID)
		if err != nil {
			launcherSelectionError(w, err)
			return
		}
		launcherID = launcher.ID
		req.Variant = launcherAlias(launcher.Name)
		switchLauncher = &launcher
	}
	if refused, err := s.refusedNative(r.Context(), req.Tool, req.Variant, switchLauncher); err != nil {
		fail(w, err)
		return
	} else if refused {
		apiError(w, 409, "docker_only", errDockerOnly.Error())
		return
	}
	{
		model, ok, err := s.checkSpawnModel(r.Context(), targetID, req.Tool, req.Variant, launcherID, req.Model)
		if err != nil {
			fail(w, err)
			return
		}
		if !ok {
			apiError(w, 400, "model_unlisted", "the model is not one this harness offers; add it under Settings → Harnesses & models")
			return
		}
		req.Model = model
	}
	path := ""
	if targetID != current.MachineID {
		homeID, homeErr := s.Store.AgentHomeRoomID(r.Context(), id)
		if homeErr != nil {
			fail(w, homeErr)
			return
		}
		if homeID == "" {
			path, err = s.selectedMachinePath(r.Context(), current.ProjectID, machineInfo)
		} else {
			homeRoom, homeErr := s.Store.RoomByID(r.Context(), homeID)
			if homeErr != nil {
				fail(w, homeErr)
				return
			}
			path, _, err = s.roomCheckoutPath(r.Context(), homeRoom, machineInfo, false, "")
		}
		if errors.Is(err, errMissingMachineFolder) {
			apiError(w, 409, "machine_folder_missing", "set this project's folder on "+machineInfo.Name)
			return
		}
		if err != nil {
			apiError(w, 409, "folder_unverified", err.Error())
			return
		}
	} else {
		machine, err := s.Store.MachineByID(r.Context(), targetID)
		if err != nil {
			fail(w, err)
			return
		}
		if !machine.IsHost {
			currentPath, err := s.Store.AgentCheckoutPath(r.Context(), id)
			if err == nil {
				var canonical string
				canonical, err = s.probeMachinePath(r.Context(), machine.ID, currentPath)
				if err == nil && canonical != currentPath {
					err = errors.New("checkout path changed on the remote machine; remap before switching")
				}
			}
			if err != nil {
				apiError(w, 409, "folder_unverified", err.Error())
				return
			}
		}
		// A model switch on the same machine keeps the agent's checkout.
		// Opting a room into a worktree affects new agents only.
	}
	checkPath := path
	if checkPath == "" {
		checkPath, err = s.Store.AgentCheckoutPath(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
	}
	if err := s.validateLauncherCheckout(r.Context(), machineInfo, launcherID, checkPath); err != nil {
		apiError(w, 409, "folder_unverified", err.Error())
		return
	}
	var first store.AgentCard
	_, reused, err := s.Store.WithRequestKey(r.Context(), requestKey, func(tx *store.Tx) (string, error) {
		active, err := tx.AgentInActiveFlowMigration(id)
		if err != nil {
			return "", err
		}
		if active {
			return "", store.ErrTeamFlowMigrationActive
		}
		card, err := tx.BeginAgentSwitchWithContinuity(id, userID(r), req.RoomID, req.Tool, req.Variant, req.Model, provider, targetID, path, launcherID, req.Rev)
		if err != nil {
			return "", err
		}
		first = card
		encoded, err := json.Marshal(card)
		return string(encoded), err
	})
	if errors.Is(err, errSwitchMachineUnavailable) {
		apiError(w, 503, "machine_unavailable", "agent's supervisor does not list this adapter")
		return
	}
	if errors.Is(err, store.ErrTeamFlowMigrationActive) {
		apiError(w, 409, "migration_active", "agent is part of an active team flow migration")
		return
	}
	if err != nil {
		spawnError(w, err)
		return
	}
	if reused {
		current, err := s.Store.AgentCardByID(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, current)
		return
	}
	if machine, err := s.machineForID(first.MachineID); err == nil {
		_ = machine.sendControl(protocol.Frame{V: protocol.Version, Type: protocol.Control, AgentID: id, Generation: first.Generation, DesiredRev: first.DesiredRev, DesiredState: first.Desired})
	}
	writeJSON(w, 202, first)
}

var errSwitchMachineUnavailable = errors.New("switch machine unavailable")

// GET /api/rooms/{room}/agents/{agent}/profile: read-only detail for the Agent profile panel
// (native session, resume command, where it runs, who started it, today's use). Same access as
// the room; the agent must be a member of it.
func (s *Server) agentProfile(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	profile, err := s.Store.AgentProfile(r.Context(), room.ID, r.PathValue("agent"))
	historical := false
	if errors.Is(err, sql.ErrNoRows) {
		// A sender that exists only in imported v1 history has the session v1 recorded for it.
		profile, err = s.Store.LegacyAgentProfile(r.Context(), room.ID, r.PathValue("agent"))
		historical = true
	}
	if err != nil {
		fail(w, err)
		return
	}
	// A viewer reads the room but does not operate agents: no session handle, resume command or host path.
	// A chat between agents has no project: the person's role is the one in the agent's own project, and no role there is a viewer's.
	roleProject := room.ProjectID
	if store.IsAgentDM(room) {
		if _, agentProject, perr := s.Store.AgentPlacement(r.Context(), r.PathValue("agent")); perr == nil {
			roleProject = agentProject
		}
	}
	role, err := s.Store.UserProjectRole(r.Context(), userID(r), roleProject)
	if err != nil && store.IsAgentDM(room) {
		role, err = "viewer", nil
	}
	if err != nil {
		userError(w, err)
		return
	}
	if role == "viewer" {
		profile = profile.WithoutHostDetails()
	}
	// An imported sender can be started as a v2 agent by someone who may spawn agents here.
	if historical && (role == "owner" || role == "operator") && room.Kind != "dm" {
		if legacy, lerr := s.resolveLegacyStart(r.Context(), room.ProjectID, r.PathValue("agent")); lerr == nil {
			folder, _ := s.Store.ProjectFolder(r.Context(), room.ProjectID)
			info := legacy.StartInfo(time.Now(), folder)
			profile.Start = &info
			// The profile names the session the dialog would continue, including one a transcript scan found.
			profile.NativeSessionID, profile.Cwd = legacy.SessionID, legacy.CWD
			if legacy.SessionID == "" {
				profile.Cwd = ""
			}
		}
	}
	writeJSON(w, 200, profile)
}

// lockOwnWorktree locks the worktree an agent works in, when it has one in use, and returns its release (nothing to release otherwise).
func (s *Server) lockOwnWorktree(ctx context.Context, agentID string) func() {
	if own, found, err := s.Store.AgentWorktreeOf(ctx, agentID); err == nil && found && own.State == "ready" {
		return s.lockAgentWorktree(own.Name)
	}
	return func() {}
}

// lockAgentWorktree takes the lock of one worktree name and returns its release. It is held from making the worktree through the transaction that
// records it, and by a removal, so the two cannot interleave.
func (s *Server) lockAgentWorktree(name string) func() {
	lock, _ := s.checkoutLocks.LoadOrStore(name, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	return lock.(*sync.Mutex).Unlock
}

// agentWorktreeFor makes an agent-specific Git worktree, <project>/.aimebu/worktrees/<room-id>-<slug> on branch aimebu/<room-id>-<slug>,
// from the room's base ref (the project's current HEAD when the room has none). The slug is the agent's, picked here when it was not named, so
// the worktree and the agent carry the same name. It returns nil and why when it cannot: the project's worktree mode is off, the folder is not
// the root of a Git repository, or the supervisor refuses.
func (s *Server) agentWorktreeFor(ctx context.Context, room store.Room, machine store.Machine, slug *string, baseRef string) (*store.AgentWorktree, string) {
	mode, err := s.Store.ProjectWorktreeMode(ctx, room.ProjectID)
	if err != nil {
		return nil, err.Error()
	}
	if mode == "off" {
		return nil, "the project's worktree mode is off"
	}
	if admitted, err := s.machineAdmitted(ctx, room.ProjectID, machine); err != nil {
		return nil, err.Error()
	} else if !admitted {
		// Nothing is made in the project's folder on a machine the owner has not admitted for it; the agent waits for that approval as it always has.
		return nil, errMachineNotAdmitted.Error()
	}
	root, err := s.selectedMachinePath(ctx, room.ProjectID, machine)
	if err != nil {
		return nil, "the project folder is not set on " + machine.Name
	}
	if _, err := s.machineWorktree(ctx, machine, root, room.ID, "", "validate", false); err != nil {
		return nil, err.Error()
	}
	if *slug == "" {
		if *slug, err = s.Store.SuggestAgentSlug(ctx, room.ProjectID, ""); err != nil {
			return nil, err.Error()
		}
	}
	if baseRef == "" {
		if baseRef, err = s.Store.RoomBaseRef(ctx, room.ID); err != nil {
			return nil, err.Error()
		}
	}
	name := store.AgentWorktreeName(room.ID, *slug)
	unlock := s.lockAgentWorktree(name)
	// A worktree that was already there (a retry, one made before) is not ours to take away if the request fails.
	before, inspectErr := s.machineWorktree(ctx, machine, root, name, baseRef, "inspect", false)
	fresh := inspectErr == nil && before.State != "ready"
	result, err := s.machineWorktree(ctx, machine, root, name, baseRef, "ensure", false)
	if err != nil {
		unlock()
		return nil, err.Error()
	}
	return &store.AgentWorktree{Unlock: unlock, Fresh: fresh, ProjectID: room.ProjectID, RoomID: room.ID, MachineID: machine.ID, Name: name, Branch: "aimebu/" + name, Path: result.Path, BaseRef: result.BaseRef, BaseCommit: result.BaseCommit, DirtyBase: result.DirtyBase}, ""
}

var errSessionCopyUnsupported = errors.New("this machine's supervisor predates copying a Claude Code session; update it")

// copyClaudeSession has the machine put a copy of a Claude Code session under the project directory Claude uses for toCWD, so that
// the session, forked there, is found from the worktree.
func (s *Server) copyClaudeSession(ctx context.Context, conn *machineConn, launcherID, sessionID, toCWD string) error {
	if !conn.supportsSessionCopy {
		return errSessionCopyUnsupported
	}
	_, err := s.requestSessionScan(ctx, conn, protocol.Frame{LauncherID: launcherID, ScanSessionID: sessionID, CopyTo: toCWD}, sessionScanTimeout)
	return err
}

// ownWorktreeUsable checks a worktree made for an agent against the agent's launcher before the agent is moved to it, as a start in any
// other folder is checked, so a launcher whose roots exclude the worktree is refused here and not at start.
func (s *Server) ownWorktreeUsable(ctx context.Context, agentID string, own *store.AgentWorktree) error {
	card, err := s.Store.AgentCardByID(ctx, agentID)
	if err != nil {
		return err
	}
	machine, err := s.Store.MachineByID(ctx, own.MachineID)
	if err != nil {
		return err
	}
	return s.validateLauncherCheckout(ctx, machine, card.LauncherID, own.Path)
}

// discardAgentWorktree takes away a worktree that was made for a request which then failed, so it does not stay on disk with no agent and no
// record. Only one this request made is removed (a clean, new one: nothing is lost); the branch stays, and a later request reuses it.
func (s *Server) discardAgentWorktree(ctx context.Context, own *store.AgentWorktree) {
	if own == nil || !own.Fresh {
		return
	}
	ctx = context.WithoutCancel(ctx)
	projectID := own.ProjectID
	if projectID == "" {
		room, err := s.Store.RoomByID(ctx, own.RoomID)
		if err != nil {
			return
		}
		projectID = room.ProjectID
	}
	machine, err := s.Store.MachineByID(ctx, own.MachineID)
	if err != nil {
		return
	}
	root, err := s.selectedMachinePath(ctx, projectID, machine)
	if err != nil {
		return
	}
	// The branch is kept, as for every removal, and so is the copy of a Claude Code session made for a request that then failed: a failed
	// new-room copy can leave an unused branch and a session copy behind. Both are small, and nothing here may delete work.
	if _, err := s.machineWorktree(ctx, machine, root, own.Name, "", "remove", true); err != nil {
		log.Printf("agent worktree %s: could not remove it after a failed request: %v", own.Name, err)
	}
}

// agentWorktreeRemove (DELETE /api/agents/{id}/worktree) removes the worktree a second writer worked in, by the same rules as a room's: a clean,
// merged one is removed, one with uncommitted or unmerged work is kept (state "preserved", with why) unless force is sent with the confirmation
// "REMOVE <name>", and one an agent still uses is refused. The branch is kept either way, and a removed one sends its agent back to the room's checkout.
func (s *Server) agentWorktreeRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	own, found, err := s.Store.AgentWorktreeOf(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if !found {
		apiError(w, 404, "not_found", "this agent has no worktree of its own")
		return
	}
	room, err := s.Store.RoomByID(r.Context(), own.RoomID)
	if err != nil {
		fail(w, err)
		return
	}
	if !s.projectOperator(w, r, room.ProjectID) {
		return
	}
	var req struct {
		Confirm string `json:"confirm"`
		Force   bool   `json:"force"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Force && req.Confirm != "REMOVE "+own.Name {
		apiError(w, 400, "confirmation_required", "confirm with REMOVE followed by the worktree's name, "+own.Name)
		return
	}
	// The same lock a start holds from making the worktree through recording it: a removal cannot take the folder from an agent that is starting.
	defer s.lockAgentWorktree(own.Name)()
	if own, found, err = s.Store.AgentWorktreeOf(r.Context(), id); err != nil {
		fail(w, err)
		return
	} else if !found {
		apiError(w, 404, "not_found", "this agent has no worktree of its own")
		return
	}
	if own.State == "removed" {
		writeJSON(w, 200, own)
		return
	}
	inUse, err := s.Store.RoomWorktreeInUse(r.Context(), own.MachineID, own.Path)
	if err != nil {
		fail(w, err)
		return
	}
	if inUse {
		apiError(w, 409, "worktree_in_use", "an active agent still uses this worktree: stop it first")
		return
	}
	machine, err := s.Store.MachineByID(r.Context(), own.MachineID)
	if err != nil {
		fail(w, err)
		return
	}
	root, err := s.selectedMachinePath(r.Context(), room.ProjectID, machine)
	if err != nil {
		worktreeError(w, err)
		return
	}
	result, err := s.machineWorktree(r.Context(), machine, root, own.Name, own.BaseRef, "remove", req.Force)
	if err != nil {
		worktreeError(w, err)
		return
	}
	if result.State == "removed" {
		// The agent goes back to its room's checkout, so no way of starting it ever goes to the folder that is gone.
		roomPath, pathErr := s.existingCheckoutPath(r.Context(), room, machine)
		if pathErr == nil {
			pathErr = s.Store.DetachAgentWorktree(r.Context(), id, own.MachineID, roomPath)
		}
		if errors.Is(pathErr, store.ErrConflict) {
			apiError(w, 409, "worktree_in_use", "the agent was started while its worktree was being removed; stop it first")
			return
		}
		if pathErr != nil {
			fail(w, pathErr)
			return
		}
	} else if err := s.Store.SetAgentWorktreeState(r.Context(), id, result.State); err != nil {
		fail(w, err)
		return
	}
	own.State = result.State
	writeJSON(w, 200, map[string]any{"agent_id": id, "name": own.Name, "path": own.Path, "branch": own.Branch, "state": result.State, "reason": result.Reason})
}

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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
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
	// Name is the agent's name; empty lets the server pick a free one from its pool. A v1 agent that is resumed keeps its own name.
	Name string `json:"name,omitempty"`
	// ResumeLegacy starts the agent from an imported v1 agent: the same name, and its harness session continued when the harness can.
	// ConfirmV1Idle acknowledges the warning that v1 may still be using that session.
	ResumeLegacy  string `json:"resume_legacy,omitempty"`
	ConfirmV1Idle bool   `json:"confirm_v1_idle,omitempty"`
}

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
	ToolboxReason     string              `json:"toolbox_reason,omitempty"`
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
	{"vibe", "native", "Mistral Vibe", "mistral"},
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
			if !definition.Enabled {
				item.Available, item.UnavailableReason = false, "disabled by owner; enable it in Settings → Harnesses"
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

const dockerBindUnavailableMessage = "AIMEBU2_BIND is loopback-only; Docker needs a non-loopback bind and AIMEBU2_ALLOW entry"

var errDockerBindUnavailable = errors.New(dockerBindUnavailableMessage)

func dockerNeedsReachableBind(tool, variant string) bool {
	return variant == "docker" && (tool == "claude" || tool == "codex" || tool == "pi")
}

func dockerBindIsLoopback() bool {
	bind := os.Getenv("AIMEBU2_BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	ip := net.ParseIP(bind)
	return ip != nil && ip.IsLoopback()
}

var errLauncherUnavailable = errors.New("pass an enabled launcher_id or a launcher name in variant (native/docker for the default names); check Settings → Harnesses")

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
	if err != nil || item.Tool != tool || (launcherID == "" && variant != "" && launcherAlias(item.Name) != variant) {
		return store.Launcher{}, errLauncherUnavailable
	}
	return item, nil
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
			return nil, m, errLauncherUnavailable
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
		cards, err := s.Store.AgentCardsForRoom(r.Context(), room.ID)
		if err != nil {
			fail(w, err)
			return
		}
		withUsage := make([]agentCardWithUsage, 0, len(cards))
		for _, card := range cards {
			usage, err := s.Store.AgentUsage(r.Context(), card.ID)
			if err != nil {
				fail(w, err)
				return
			}
			flags, err := s.agentHealth(r.Context(), card)
			if err != nil {
				fail(w, err)
				return
			}
			withUsage = append(withUsage, agentCardWithUsage{card, usage, flags})
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
		taken, err := s.Store.AgentSlugTaken(r.Context(), room.ProjectID, req.Name)
		if err != nil {
			fail(w, err)
			return
		}
		if taken {
			spawnError(w, store.ErrSlugTaken)
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
		machineSelectionError(w, machineInfo, err)
		return
	}
	launcherID := ""
	containerized := launcherKeepsOwnSessions(nil, req.Variant)
	if machine.supportsLaunchers && req.Tool != "fake" {
		launcher, err := s.resolveLauncher(r.Context(), machine.id, req.Tool, req.Variant, req.LauncherID)
		if err != nil {
			apiError(w, 409, "launcher_unavailable", errLauncherUnavailable.Error())
			return
		}
		launcherID = launcher.ID
		req.Variant = launcherAlias(launcher.Name)
		// What matters is whether the launcher runs in a container (its own home), not what it is called.
		containerized = launcherKeepsOwnSessions(&launcher, req.Variant)
	}
	{
		model, ok, err := s.checkSpawnModel(r.Context(), machine.id, req.Tool, req.Variant, launcherID, req.Model)
		if err != nil {
			fail(w, err)
			return
		}
		if !ok {
			apiError(w, 400, "model_unlisted", "the model is not one this harness offers; add it under Settings → Harnesses")
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
	if resuming && containerized {
		apiError(w, 409, "needs_native", "a containerized launcher keeps its own Claude sessions, so a v1 session can only be continued with the native claude launcher")
		return
	}
	var path string
	var worktreeItem *store.RoomWorktree
	if resuming {
		// A resumed session runs where it was made (see legacyResumeCWD), not in the room's checkout.
		if path, err = s.legacyResumeCWD(r.Context(), machineInfo, legacy.CWD); err != nil {
			apiError(w, 409, "v1_folder_missing", err.Error())
			return
		}
		if machineInfo.IsHost && !containerized {
			if checkHarnessSession(req.Tool, legacy.SessionID, path) == sessionMissing {
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
	result, reused, err := s.Store.WithRequestKey(r.Context(), store.RequestKey{ActorKind: actorKind, ActorID: actorID, Kind: "spawn", Key: key, Digest: digest}, func(tx *store.Tx) (string, error) {
		input := store.SpawnInput{RoomID: room.ID, ActorKind: actorKind, ActorID: actorID, MachineID: machine.id, Path: path, RoleKey: req.RoleKey, Tool: req.Tool, Variant: req.Variant, LauncherID: launcherID, Model: req.Model, Provider: req.Provider, KeyHash: hex.EncodeToString(keyHashBytes[:])}
		input.Slug = req.Name
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
			}
		}
		created, err := tx.SpawnAgent(input)
		if err != nil {
			return "", err
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
		encoded, err := json.Marshal(created)
		return string(encoded), err
	})
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
	machine, err := s.machineForID(pending.MachineID)
	if err != nil {
		apiError(w, 503, "machine_unavailable", "agent created but supervisor is disconnected; retry with same key")
		return false
	}
	if err := s.sendPendingSpawn(r.Context(), machine, pending); err != nil {
		apiError(w, 503, "machine_unavailable", "agent created but spawn delivery failed; retry with same key")
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
	return machine.send(protocol.Frame{V: protocol.Version, Type: protocol.Spawn, ReqID: fmt.Sprintf("%s/%d", p.AgentID, p.Generation), AgentID: p.AgentID, Generation: p.Generation, DesiredRev: p.DesiredRev, AgentKey: key, CheckoutPath: p.CheckoutPath, Agent: &adapter.AgentSpec{ID: p.AgentID, Tool: p.Tool, Variant: p.Variant, LauncherID: p.LauncherID, Model: p.Model, Provider: p.Provider}})
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
	case errors.Is(err, store.ErrProjectArchived):
		apiError(w, 409, "project_archived", "this project is archived and read-only; reopen it to change anything")
	case errors.Is(err, store.ErrInvalidScope):
		apiError(w, 409, "invalid_scope", "bulk controls are unavailable in private chats")
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "actor is not allowed to manage this agent")
	case errors.Is(err, store.ErrWriterOccupied):
		apiError(w, 409, "writer_occupied", "checkout already has a writer")
	case errors.Is(err, store.ErrHomeLeadOnly):
		apiError(w, 409, "home_is_lead_only", "this project's home room accepts only lead roles; spawn other agents into a task room such as general, or turn off Home room: lead only in the project settings")
	case errors.Is(err, store.ErrRoleOccupied):
		apiError(w, 409, "role_occupied", "project already has an active agent in this singleton role")
	case errors.Is(err, store.ErrSpawnLimit):
		apiError(w, 429, "spawn_limit", "agent or spawn rate limit reached")
	case errors.Is(err, store.ErrSlugTaken):
		apiError(w, 409, "name_in_use", "this project already has an agent, person or role with that name")
	case errors.Is(err, store.ErrLegacyStarted):
		apiError(w, 409, "already_started", "this v1 agent was already started as a v2 agent; restart that agent instead")
	case errors.Is(err, store.ErrRoleSpawnCursorChanged):
		apiError(w, 409, "role_default_changed", "another agent used this role default first; retry the spawn")
	case errors.Is(err, store.ErrConflict):
		apiError(w, 409, "idempotency_conflict", "key reused with different request")
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
		Desired string `json:"desired"`
		Rev     int64  `json:"rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Desired != "active" && req.Desired != "not_accepting" && req.Desired != "stopped" {
		apiError(w, 400, "invalid_request", "desired must be active, not_accepting, or stopped")
		return
	}
	id := r.PathValue("id")
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
	err := s.Store.Tx(r.Context(), func(tx *store.Tx) error {
		var err error
		updated, err = tx.SetAgentDesired(id, userID(r), req.Desired, req.Rev)
		return err
	})
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
	if selectedConn.supportsLaunchers && req.Tool != "fake" {
		launcher, err := s.resolveLauncher(r.Context(), targetID, req.Tool, req.Variant, req.LauncherID)
		if err != nil {
			apiError(w, 409, "launcher_unavailable", errLauncherUnavailable.Error())
			return
		}
		launcherID = launcher.ID
		req.Variant = launcherAlias(launcher.Name)
	}
	{
		model, ok, err := s.checkSpawnModel(r.Context(), targetID, req.Tool, req.Variant, launcherID, req.Model)
		if err != nil {
			fail(w, err)
			return
		}
		if !ok {
			apiError(w, 400, "model_unlisted", "the model is not one this harness offers; add it under Settings → Harnesses")
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
		card, err := tx.BeginAgentSwitchToLauncher(id, userID(r), req.RoomID, req.Tool, req.Variant, req.Model, provider, targetID, path, launcherID, req.Rev)
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
		if legacy, lerr := s.Store.LegacyStart(r.Context(), room.ProjectID, r.PathValue("agent")); lerr == nil {
			folder, _ := s.Store.ProjectFolder(r.Context(), room.ProjectID)
			info := legacy.StartInfo(time.Now(), folder)
			profile.Start = &info
		}
	}
	writeJSON(w, 200, profile)
}

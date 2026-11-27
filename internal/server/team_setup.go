package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

var errTeamSetupFolder = errors.New("team setup folder unavailable")

func teamSetupError(w http.ResponseWriter, err error) {
	var refusal *legacyCheckError
	switch {
	case errors.As(err, &refusal):
		apiError(w, refusal.status, refusal.code, refusal.msg)
	case errors.Is(err, store.ErrTeamFlowRev), errors.Is(err, store.ErrStaleRev):
		apiError(w, 409, "stale_rev", "team flow changed; reload and try again")
	case errors.Is(err, store.ErrTeamFlowSelection):
		apiError(w, 409, "stale_conflict", "selected room membership changed; preview again")
	case errors.Is(err, store.ErrTeamSetupInvalid), errors.Is(err, store.ErrInvalidSpawnDefaults):
		apiError(w, 400, "invalid_request", err.Error())
	case errors.Is(err, store.ErrTeamSetupName):
		apiError(w, 400, "invalid_name", "a name is 3 to 21 characters: lowercase letters, digits, - and _, starting with a letter, ending with a letter or digit, and not a group tag such as all or here")
	case errors.Is(err, store.ErrForbidden):
		apiError(w, 403, "forbidden", "project owner or operator required")
	case errors.Is(err, store.ErrConflict):
		apiError(w, 409, "idempotency_conflict", "key was used for a different team setup")
	case errors.Is(err, errMissingMachineFolder):
		apiError(w, 409, "machine_folder_missing", "set this project's folder on the selected machine")
	case errors.Is(err, errLauncherUnavailable):
		apiError(w, 409, "launcher_unavailable", errLauncherUnavailable.Error())
	case errors.Is(err, errDockerOnly):
		apiError(w, 409, "docker_only", errDockerOnly.Error())
	case errors.Is(err, errDockerBindUnavailable):
		apiError(w, 409, "invalid_transition", dockerBindUnavailableMessage)
	case errors.Is(err, errTeamSetupFolder):
		apiError(w, 409, "folder_unverified", err.Error())
	case errors.Is(err, ErrMachineUnavailable):
		apiError(w, 503, "machine_unavailable", "selected machine is unavailable")
	default:
		spawnError(w, err)
	}
}

func (s *Server) teamSetupPreview(w http.ResponseWriter, r *http.Request) {
	// Who may set the team up is decided before the document is read or validated, as teamSetupCreate does: a person outside the project
	// learns nothing from the shape errors.
	projectID, actorID := r.PathValue("project"), userID(r)
	if role, err := s.Store.UserProjectRole(r.Context(), actorID, projectID); err != nil || role != "owner" && role != "operator" {
		teamSetupError(w, store.ErrForbidden)
		return
	}
	var input store.TeamSetupInput
	if !readJSON(w, r, &input) {
		return
	}
	if err := s.Store.ValidateTeamSetup(r.Context(), projectID, actorID, input); err != nil {
		teamSetupError(w, err)
		return
	}
	if err := s.preflightTeamSetup(r, projectID, &input); err != nil {
		teamSetupError(w, err)
		return
	}
	flow, err := s.Store.PreviewTeamFlow(r.Context(), projectID, actorID, input.FlowKey)
	if err != nil {
		teamSetupError(w, err)
		return
	}
	defaults := make([]string, 0, len(input.RoleDefaults))
	for key := range input.RoleDefaults {
		defaults = append(defaults, key)
	}
	sort.Strings(defaults)
	agents := []store.TeamSetupAgent{{Role: "leader", RoomKey: "home", Target: &input.Leader}}
	if input.Dispatcher != nil {
		agents = append(agents, store.TeamSetupAgent{Role: "dispatcher", RoomKey: "routing", Target: input.Dispatcher})
	}
	if input.Advisor != nil {
		agents = append(agents, store.TeamSetupAgent{Role: "advisor", RoomKey: "advice", Target: input.Advisor})
	}
	current, err := s.Store.GetProjectRecoverySettings(r.Context(), projectID, actorID)
	if err != nil {
		teamSetupError(w, err)
		return
	}
	memory, err := s.Store.ProjectMemoryEnabled(r.Context(), projectID)
	if err != nil {
		teamSetupError(w, err)
		return
	}
	if input.Settings.WorktreeMode != nil {
		current.WorktreeMode = *input.Settings.WorktreeMode
	}
	if input.Settings.MemoryEnabled != nil {
		memory = *input.Settings.MemoryEnabled
	}
	if input.Settings.MaxLiveAgents != nil {
		current.MaxLiveAgents = *input.Settings.MaxLiveAgents
	}
	if input.Settings.ReviewerAgents != nil {
		current.ReviewerAgents = *input.Settings.ReviewerAgents
	}
	settings := map[string]any{"worktree_mode": current.WorktreeMode, "memory_enabled": memory, "max_live_agents": current.MaxLiveAgents, "reviewer_agents": current.ReviewerAgents, "reviewer_agents_global_enabled": current.ReviewerAgentsGlobalEnabled}
	writeJSON(w, 200, map[string]any{"flow": flow, "created_rooms": flow.CreateRooms, "conflicts": flow.Conflicts, "agents": agents, "defaults_saved": defaults, "settings": settings, "can_create": true})
}

func (s *Server) teamSetupCreate(w http.ResponseWriter, r *http.Request) {
	var input store.TeamSetupInput
	if !readJSON(w, r, &input) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		apiError(w, 400, "invalid_request", "Idempotency-Key required (at most 200 characters)")
		return
	}
	projectID, actorID := r.PathValue("project"), userID(r)
	role, err := s.Store.UserProjectRole(r.Context(), actorID, projectID)
	if err != nil || role != "owner" && role != "operator" {
		teamSetupError(w, store.ErrForbidden)
		return
	}
	requestBytes, _ := json.Marshal(struct {
		Project string               `json:"project"`
		Input   store.TeamSetupInput `json:"input"`
	}{projectID, input})
	digestBytes := sha256.Sum256(requestBytes)
	digest := hex.EncodeToString(digestBytes[:])
	prior, found, err := s.Store.TeamSetupRequestResult(r.Context(), actorID, key, digest)
	if err != nil {
		teamSetupError(w, err)
		return
	}
	if found {
		var result store.TeamSetupResult
		if err := json.Unmarshal([]byte(prior), &result); err != nil {
			fail(w, err)
			return
		}
		if !s.deliverTeamSetup(w, r, result) {
			return
		}
		writeJSON(w, 200, result)
		return
	}
	if err := s.Store.ValidateTeamSetup(r.Context(), projectID, actorID, input); err != nil {
		teamSetupError(w, err)
		return
	}
	if err := s.preflightTeamSetup(r, projectID, &input); err != nil {
		teamSetupError(w, err)
		return
	}
	request := store.RequestKey{ActorKind: "user", ActorID: actorID, Kind: "team_spawn", Key: key, Digest: digest}
	resultJSON, reused, err := s.Store.WithRequestKey(r.Context(), request, func(tx *store.Tx) (string, error) {
		result, err := tx.CreateTeamSetup(projectID, actorID, input)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(result)
		return string(encoded), err
	})
	if err != nil {
		teamSetupError(w, err)
		return
	}
	var result store.TeamSetupResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		fail(w, err)
		return
	}
	if !s.deliverTeamSetup(w, r, result) {
		return
	}
	status := 201
	if reused {
		status = 200
	}
	writeJSON(w, status, result)
}

func (s *Server) deliverTeamSetup(w http.ResponseWriter, r *http.Request, result store.TeamSetupResult) bool {
	for _, item := range result.Agents {
		card, err := s.Store.AgentCardByID(r.Context(), item.AgentID)
		if err != nil {
			fail(w, err)
			return false
		}
		if !s.ensureSpawnDelivery(w, r, card) {
			return false
		}
	}
	return true
}

func (s *Server) preflightTeamSetup(r *http.Request, projectID string, input *store.TeamSetupInput) error {
	for _, target := range []*store.TeamSetupTarget{&input.Leader, input.Dispatcher, input.Advisor} {
		if target == nil {
			continue
		}
		choice := spawnRequest{Tool: target.Tool, Variant: target.Variant, LauncherID: target.LauncherID, Model: target.Model}
		listed, err := s.listedAdapter(r.Context(), choice)
		if err != nil {
			return err
		}
		if !listed {
			return store.ErrTeamSetupInvalid
		}
		conn, machine, err := s.selectedMachine(r.Context(), target.MachineID, target.Tool, target.Variant, target.LauncherID)
		if err != nil {
			return err
		}
		target.MachineID = machine.ID
		if err := s.Store.CheckMachineProject(r.Context(), projectID, machine.ID); err != nil {
			return err
		}
		launcherID := ""
		var launcher *store.Launcher
		if conn.supportsLaunchers && target.Tool != "fake" {
			resolved, err := s.resolveLauncher(r.Context(), machine.ID, target.Tool, target.Variant, target.LauncherID)
			if err != nil {
				return err
			}
			launcherID = resolved.ID
			target.Variant = launcherAlias(resolved.Name)
			launcher = &resolved
		}
		if refused, err := s.refusedNative(r.Context(), target.Tool, target.Variant, launcher); err != nil {
			return err
		} else if refused {
			return errDockerOnly
		}
		model, ok, err := s.checkSpawnModel(r.Context(), machine.ID, target.Tool, target.Variant, launcherID, target.Model)
		if err != nil {
			return err
		}
		if !ok {
			return store.ErrTeamSetupInvalid
		}
		target.Model, target.LauncherID = model, launcherID
		resumed := false
		target.ResumeHandle, target.LaunchPath = "", ""
		if target.ResumeLegacy != "" {
			if resumed, err = s.prepareLegacyTarget(r.Context(), projectID, conn, machine, launcher, target); err != nil {
				return err
			}
		}
		path := target.Path // a continued session runs where v1 made it, already resolved
		if !resumed {
			path, err = s.Store.ProjectMachineFolder(r.Context(), projectID, machine.ID)
			if err != nil {
				return err
			}
			if path == "" {
				return errMissingMachineFolder
			}
			if machine.IsHost {
				path, err = filepath.Abs(path)
				if err != nil {
					return err
				}
				path, err = filepath.EvalSymlinks(path)
			} else {
				path, err = s.probeMachinePath(r.Context(), machine.ID, path)
			}
			if err != nil {
				return fmt.Errorf("%w: %v", errTeamSetupFolder, err)
			}
		}
		if err := s.validateLauncherCheckout(r.Context(), machine, launcherID, path); err != nil {
			return fmt.Errorf("%w: %v", errTeamSetupFolder, err)
		}
		target.Path = path
		for _, allowed := range boundedAdapters {
			if allowed.tool == target.Tool {
				target.Provider = allowed.provider
				break
			}
		}
	}
	return nil
}

// prepareLegacyTarget checks and resolves a team flow target that continues a v1 agent, with the checks a spawn from v1 history makes: the
// agent must be found in the project and not started yet, keep its name, and, when its session can be continued, run the tool v1 ran, in
// the folder v1 worked in, on a machine that can see the session. resumed says the session is continued (Path is then its folder); when it
// is not, the agent starts fresh under the v1 name in the project's folder.
func (s *Server) prepareLegacyTarget(ctx context.Context, projectID string, conn *machineConn, machine store.Machine, launcher *store.Launcher, target *store.TeamSetupTarget) (resumed bool, err error) {
	req := spawnRequest{Tool: target.Tool, Variant: target.Variant, ResumeLegacy: target.ResumeLegacy, ConfirmV1Idle: target.ConfirmV1Idle}
	legacy, resuming, err := s.checkLegacyStart(ctx, projectID, "user", &req)
	if err != nil {
		return false, err
	}
	if err := s.Store.SlugInUse(ctx, projectID, legacy.Slug); err != nil {
		var inUse *store.NameInUseError
		if errors.As(err, &inUse) {
			return false, &legacyCheckError{409, "name_in_use", inUse.Error()}
		}
		if errors.Is(err, store.ErrSlugTaken) {
			return false, &legacyCheckError{409, "name_in_use", "the project already has an agent, person or role of the name " + legacy.Slug}
		}
		return false, err
	}
	target.Name, target.ResumeLegacy = legacy.Slug, legacy.AgentID
	if !resuming {
		return false, nil
	}
	containerized := launcherKeepsOwnSessions(launcher, target.Variant)
	sharedHome := containerized && target.Tool == "claude" && sharesClaudeHome(conn, launcher)
	if containerized && !sharedHome {
		return false, &legacyCheckError{409, "needs_native", sessionsMessage(target.Tool, launcher)}
	}
	if target.Tool != "claude" && !conn.supportsLegacySessionCopy {
		return false, &legacyCheckError{409, "supervisor_update_needed", "this machine's supervisor cannot copy v1 sessions into a private agent runtime; update it before continuing"}
	}
	path, err := s.legacyResumeCWD(ctx, machine, legacy.CWD)
	if err != nil {
		return false, &legacyCheckError{409, "v1_folder_missing", err.Error()}
	}
	launch := filepath.Clean(legacy.CWD)
	if target.Tool == "claude" && launch != path && filepath.IsAbs(launch) && sameDirectory(machine, launch, path) {
		if !conn.supportsLaunchPath {
			return false, &legacyCheckError{409, "supervisor_update_needed", "v1 worked in this folder through a link (" + launch + "); the supervisor on " + machine.Name + " predates starting Claude in that spelling, so update it"}
		}
		target.LaunchPath = launch
	}
	if machine.IsHost && (!containerized || sharedHome) {
		look := path
		if target.LaunchPath != "" {
			look = target.LaunchPath
		}
		if checkHarnessSession(target.Tool, legacy.SessionID, look) == sessionMissing {
			return false, &legacyCheckError{409, "session_not_found", "the v1 session " + legacy.SessionID + " was not found on this machine, so it cannot be continued"}
		}
	}
	target.Path, target.ResumeHandle = path, legacy.SessionID
	return true, nil
}

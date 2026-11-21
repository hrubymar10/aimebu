package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

func teamFlowMigrationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrProjectArchived):
		apiError(w, 409, "project_archived", "archived projects cannot change team flow")
	case errors.Is(err, store.ErrTeamFlowRev):
		apiError(w, 409, "stale_rev", "team flow changed; reload and try again")
	case errors.Is(err, store.ErrTeamFlowMigrationActive):
		apiError(w, 409, "migration_active", "a team flow migration is already running")
	case errors.Is(err, store.ErrTeamFlowMigrationFinal):
		apiError(w, 409, "past_point_of_no_return", "the migration has already started applying")
	case errors.Is(err, store.ErrTeamFlowSelection):
		apiError(w, 409, "stale_conflict", "selected room membership changed; preview again")
	case errors.Is(err, store.ErrTeamFlowMigration):
		apiError(w, 400, "invalid_request", err.Error())
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 404, "not_found", "no team flow migration was found")
	default:
		teamSetupError(w, err)
	}
}

func (s *Server) preflightTeamFlowMigration(r *http.Request, projectID string, in *store.TeamFlowMigrationInput) error {
	for _, target := range []*store.TeamSetupTarget{in.Leader, in.Dispatcher, in.Advisor} {
		if target == nil {
			continue
		}
		wrapper := store.TeamSetupInput{Leader: *target}
		if err := s.preflightTeamSetup(r, projectID, &wrapper); err != nil {
			return err
		}
		*target = wrapper.Leader
	}
	for i := range in.Changes {
		wrapper := store.TeamSetupInput{Leader: in.Changes[i].Target}
		if err := s.preflightTeamSetup(r, projectID, &wrapper); err != nil {
			return err
		}
		in.Changes[i].Target = wrapper.Leader
	}
	return nil
}

func (s *Server) teamFlowMigrationPreview(w http.ResponseWriter, r *http.Request) {
	var in store.TeamFlowMigrationInput
	if !readJSON(w, r, &in) {
		return
	}
	projectID, actorID := r.PathValue("project"), userID(r)
	preview, err := s.Store.PreviewTeamFlowMigration(r.Context(), projectID, actorID, in)
	if err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	if err := s.preflightTeamFlowMigration(r, projectID, &in); err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	current, err := s.Store.GetProjectRecoverySettings(r.Context(), projectID, actorID)
	if err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	memory, err := s.Store.ProjectMemoryEnabled(r.Context(), projectID)
	if err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	if in.Settings.WorktreeMode != nil {
		current.WorktreeMode = *in.Settings.WorktreeMode
	}
	if in.Settings.MemoryEnabled != nil {
		memory = *in.Settings.MemoryEnabled
	}
	if in.Settings.MaxLiveAgents != nil {
		current.MaxLiveAgents = *in.Settings.MaxLiveAgents
	}
	if in.Settings.ReviewerAgents != nil {
		current.ReviewerAgents = *in.Settings.ReviewerAgents
	}
	writeJSON(w, 200, map[string]any{"from_key": preview.FromKey, "to_key": preview.ToKey, "roles": preview.Roles,
		"created_rooms": preview.CreatedRooms, "conflicts": preview.Conflicts,
		"settings":  map[string]any{"worktree_mode": current.WorktreeMode, "memory_enabled": memory, "max_live_agents": current.MaxLiveAgents, "reviewer_agents": current.ReviewerAgents, "reviewer_agents_global_enabled": current.ReviewerAgentsGlobalEnabled},
		"can_apply": preview.CanApply})
}

func (s *Server) teamFlowMigrate(w http.ResponseWriter, r *http.Request) {
	var in store.TeamFlowMigrationInput
	if !readJSON(w, r, &in) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		apiError(w, 400, "invalid_request", "Idempotency-Key required (at most 200 characters)")
		return
	}
	projectID, actorID := r.PathValue("project"), userID(r)
	inputBytes, _ := json.Marshal(struct {
		Project string                       `json:"project"`
		Input   store.TeamFlowMigrationInput `json:"input"`
	}{projectID, in})
	digest := sha256.Sum256(inputBytes)
	request := store.RequestKey{ActorKind: "user", ActorID: actorID, Kind: "team_spawn", Key: "flow_migration:" + key, Digest: hex.EncodeToString(digest[:])}
	if prior, found, err := s.Store.RequestResult(r.Context(), request); err != nil {
		teamFlowMigrationError(w, err)
		return
	} else if found {
		var saved struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(prior), &saved); err != nil {
			fail(w, err)
			return
		}
		progress, err := s.Store.TeamFlowMigrationProgressByID(r.Context(), projectID, actorID, saved.ID)
		if err != nil {
			teamFlowMigrationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"migration": progress})
		return
	}
	preview, err := s.Store.PreviewTeamFlowMigration(r.Context(), projectID, actorID, in)
	if err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	if !preview.CanApply {
		apiError(w, 400, "missing_target", "choose a machine, harness, and model for each new role")
		return
	}
	if err := s.preflightTeamFlowMigration(r, projectID, &in); err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	result, _, err := s.Store.WithRequestKey(r.Context(), request, func(tx *store.Tx) (string, error) {
		progress, err := tx.BeginTeamFlowMigration(projectID, actorID, in)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(map[string]string{"id": progress.ID})
		return string(data), err
	})
	if err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	var saved struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(result), &saved); err != nil {
		fail(w, err)
		return
	}
	progress, err := s.Store.TeamFlowMigrationProgressByID(r.Context(), projectID, actorID, saved.ID)
	if err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"migration": progress})
}

func (s *Server) teamFlowMigrationProgress(w http.ResponseWriter, r *http.Request) {
	progress, err := s.Store.TeamFlowMigrationProgress(r.Context(), r.PathValue("project"), userID(r))
	if err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"migration": progress})
}

func (s *Server) teamFlowMigrationCancel(w http.ResponseWriter, r *http.Request) {
	progress, err := s.Store.CancelTeamFlowMigration(r.Context(), r.PathValue("project"), userID(r))
	if err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"migration": progress})
}

// teamFlowMigrationInterrupt cuts the current turn of an agent the running migration is waiting for. A machine whose supervisor cuts one
// turn (supports_turn_interrupt) is sent a turn_interrupt frame and the agent keeps running (see Store.InterruptFlowMigrationTurn); any
// other machine, a turn that cannot be cut that way, and a frame that was not answered take the older path, which stops the agent and
// starts a kept one again (see Store.InterruptFlowMigrationAgent). A stop that follows a frame wins on the supervisor.
func (s *Server) teamFlowMigrationInterrupt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID string `json:"agent_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.AgentID == "" {
		apiError(w, 400, "invalid_request", "agent_id required")
		return
	}
	projectID := r.PathValue("project")
	// Which path is the machine's to say; who may interrupt, and what, is checked by the store on both.
	if known, err := s.Store.AgentCardByID(r.Context(), req.AgentID); err == nil && s.machineInterruptsTurns(known.MachineID) {
		target, err := s.Store.InterruptFlowMigrationTurn(r.Context(), projectID, userID(r), req.AgentID)
		if err != nil {
			teamFlowMigrationError(w, err)
			return
		}
		if target.TurnID != "" {
			result, err := s.interruptTurn(r.Context(), target, "team flow change")
			// not_running: the turn ended by itself in the meantime, and its end is on its way.
			if err == nil && (result == "ok" || result == "not_running") {
				progress, err := s.Store.TeamFlowMigrationProgress(r.Context(), projectID, userID(r))
				if err != nil {
					teamFlowMigrationError(w, err)
					return
				}
				writeJSON(w, 200, map[string]any{"migration": progress})
				return
			}
			log.Printf("team flow migration interrupt: turn %s of agent %s was not interrupted (result %q, error %v); stopping the agent instead", target.TurnID, target.AgentID, result, err)
		}
	}
	updated, err := s.Store.InterruptFlowMigrationAgent(r.Context(), projectID, userID(r), req.AgentID)
	if err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	// The card the transaction returned does not name the machine: reload it, as the agent control endpoint does, and say when the stop cannot be sent.
	card, err := s.Store.AgentCardByID(r.Context(), updated.ID)
	if err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	if machine, err := s.machineForID(card.MachineID); err != nil {
		log.Printf("team flow migration interrupt: stop for agent %s not sent: %v", card.ID, err)
	} else {
		machine.boundMu.Lock()
		machine.bound[card.ID] = card.Generation
		machine.boundMu.Unlock()
		if err := machine.sendControl(protocol.Frame{V: protocol.Version, Type: protocol.Control, AgentID: card.ID, Generation: card.Generation, DesiredRev: card.DesiredRev, DesiredState: card.Desired}); err != nil {
			log.Printf("team flow migration interrupt: stop for agent %s not sent: %v", card.ID, err)
		}
	}
	progress, err := s.Store.TeamFlowMigrationProgress(r.Context(), projectID, userID(r))
	if err != nil {
		teamFlowMigrationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"migration": progress})
}

package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/goccy/go-json"
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

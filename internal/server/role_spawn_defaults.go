package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"math/big"
	"net/http"

	"github.com/hrubymar10/aimebu/internal/store"
)

type roleSpawnSelection struct {
	Choice   store.SpawnDefaultChoice `json:"choice"`
	Request  spawnRequest             `json:"request"`
	Steps    int64                    `json:"-"`
	Defaults store.RoleSpawnDefaults  `json:"-"`
}

// Only an omitted machine may inherit the machine of a matching default.
// Resolve an omitted model before deciding: the selected harness's default
// model may differ from the role's configured model.
func (s *Server) inheritRoleDefaultMachine(ctx context.Context, req *spawnRequest, selection roleSpawnSelection) error {
	if req.Tool != selection.Request.Tool {
		return nil
	}
	if req.LauncherID == selection.Request.LauncherID && req.Variant == "" {
		req.Variant = selection.Request.Variant
	}
	if req.MachineID != "" || !(req.LauncherID == selection.Request.LauncherID ||
		req.LauncherID == "" && req.Variant == selection.Request.Variant) {
		return nil
	}
	model, listed, err := s.checkSpawnModel(ctx, selection.Request.MachineID, req.Tool,
		selection.Request.Variant, selection.Request.LauncherID, req.Model)
	if err != nil {
		return err
	}
	if listed && model == selection.Request.Model {
		req.MachineID = selection.Request.MachineID
	}
	return nil
}

// nextRoleSpawnDefault skips choices that cannot be started now. A successful
// spawn advances the durable cursor in the same transaction as the agent.
func (s *Server) nextRoleSpawnDefault(ctx context.Context, projectID, roleKey, requestedMachineID string) (roleSpawnSelection, bool, error) {
	defaults, err := s.Store.RoleSpawnDefaults(ctx, projectID, roleKey)
	if err != nil {
		return roleSpawnSelection{}, false, err
	}
	n := len(defaults.DefaultAgents)
	if n == 0 {
		return roleSpawnSelection{Defaults: defaults}, false, nil
	}
	start := int(defaults.NextPosition % int64(n))
	if defaults.PickMode == "randomly" {
		start = 0
	}
	available := []roleSpawnSelection{}
	for offset := 0; offset < n; offset++ {
		choice := defaults.DefaultAgents[(start+offset)%n]
		launcher, err := s.Store.SpawnDefaultLauncher(ctx, choice.LauncherID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return roleSpawnSelection{}, false, err
		}
		if !launcher.Enabled {
			continue
		}
		if requestedMachineID != "" && launcher.MachineID != requestedMachineID {
			continue
		}
		variant := launcherAlias(launcher.Name)
		if _, _, err := s.selectedMachine(ctx, launcher.MachineID, launcher.Tool, variant, choice.LauncherID); err != nil {
			if errors.Is(err, ErrMachineUnavailable) || errors.Is(err, errLauncherUnavailable) ||
				errors.Is(err, errDockerBindUnavailable) || errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return roleSpawnSelection{}, false, err
		}
		folder, err := s.Store.ProjectMachineFolder(ctx, projectID, launcher.MachineID)
		if errors.Is(err, sql.ErrNoRows) || folder == "" && err == nil {
			continue
		}
		if err != nil {
			return roleSpawnSelection{}, false, err
		}
		if _, listed, err := s.checkSpawnModel(ctx, launcher.MachineID, launcher.Tool, variant, choice.LauncherID, choice.Model); err != nil {
			return roleSpawnSelection{}, false, err
		} else if !listed {
			continue
		}
		item := roleSpawnSelection{Choice: choice, Defaults: defaults, Steps: int64(offset + 1), Request: spawnRequest{
			MachineID: launcher.MachineID, Tool: launcher.Tool, Variant: variant, LauncherID: choice.LauncherID, Model: choice.Model,
		}}
		if defaults.PickMode != "randomly" {
			return item, true, nil
		}
		available = append(available, item)
	}
	if len(available) > 0 {
		pick, err := rand.Int(rand.Reader, big.NewInt(int64(len(available))))
		if err != nil {
			return roleSpawnSelection{}, false, err
		}
		return available[int(pick.Int64())], true, nil
	}
	return roleSpawnSelection{Defaults: defaults}, false, nil
}

func (s *Server) projectRoleSpawnDefaultNext(w http.ResponseWriter, r *http.Request) {
	projectID, roleKey := r.PathValue("project"), r.PathValue("role")
	if err := s.Store.ProjectAccess(r.Context(), userID(r), projectID); err != nil {
		definitionError(w, err)
		return
	}
	item, available, err := s.nextRoleSpawnDefault(r.Context(), projectID, roleKey, "")
	if err != nil {
		definitionError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"available": available, "configured": len(item.Defaults.DefaultAgents) > 0,
		"choice": item.Choice, "machine_id": item.Request.MachineID, "tool": item.Request.Tool,
		"variant": item.Request.Variant, "launcher_id": item.Request.LauncherID, "model": item.Request.Model})
}

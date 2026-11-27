package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"math/big"
	"net/http"
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

type roleSpawnSelection struct {
	Choice       store.SpawnDefaultChoice `json:"choice"`
	Request      spawnRequest             `json:"request"`
	Steps        int64                    `json:"-"`
	Defaults     store.RoleSpawnDefaults  `json:"-"`
	Skipped      []store.SpawnSkip        `json:"-"`
	AllUnhealthy bool                     `json:"-"`
}

// providerLoginRejected says the provider's active login is known rejected: the usage snapshot reads auth_missing (no usable login, or the
// usage endpoint refused it with a 401) or token_expired for the active profile. A stale, errored or unknown reading is not a rejection:
// the pool skips only a spawn it knows would die at once. Without a usage reader nothing is rejected.
func (s *Server) providerLoginRejected(provider string) bool {
	if s.Usages == nil || provider == "" {
		return false
	}
	for _, item := range s.Usages.Snapshot().Providers {
		if item.ProviderName != provider {
			continue
		}
		for _, profile := range item.Profiles {
			if !profile.Active {
				continue
			}
			return profile.Status == "auth_missing" || profile.Status == "token_expired"
		}
		return false
	}
	return false
}

// skippedSpawnNote is the room notice's tail naming the pool entries a default spawn passed over:
// " (skipped codex/native/gpt-6-sol: login rejected)"; when every entry was skipped it says so instead.
func skippedSpawnNote(selection roleSpawnSelection) string {
	if len(selection.Skipped) == 0 {
		return ""
	}
	parts := make([]string, 0, len(selection.Skipped))
	for _, skip := range selection.Skipped {
		parts = append(parts, skip.Tool+"/"+skip.Variant+"/"+skip.Model+": "+strings.ReplaceAll(skip.Reason, "_", " "))
	}
	if selection.AllUnhealthy {
		return " (every configured default entry is unhealthy: " + strings.Join(parts, "; ") + ")"
	}
	return " (skipped " + strings.Join(parts, "; ") + ")"
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

// nextRoleSpawnDefault skips choices that cannot be started now, and a choice whose provider's active login is known rejected, which it
// reports (reason "login_rejected"): a worker spawned there would die at once and cost a recovery. When every entry the pool could start
// was skipped that way it does not loop or fail: it falls back to the first skipped entry in rotation order and says so (AllUnhealthy).
// A successful spawn advances the durable cursor in the same transaction as the agent.
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
	var skipped []store.SpawnSkip
	var fallback *roleSpawnSelection
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
		if err := s.Store.CheckMachineProject(ctx, projectID, launcher.MachineID); err != nil {
			if errors.Is(err, store.ErrMachineScope) {
				continue
			}
			return roleSpawnSelection{}, false, err
		}
		variant := launcherAlias(launcher.Name)
		if _, _, err := s.selectedMachineForProject(ctx, projectID, launcher.MachineID, launcher.Tool, variant, choice.LauncherID); err != nil {
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
		// A provider login the usage snapshot already knows is rejected would kill the agent at once: pick the next healthy entry in
		// rotation order instead, and remember the first skipped one in case every entry turns out unhealthy.
		if provider := usageProvider(launcher.Tool); s.providerLoginRejected(provider) {
			skipped = append(skipped, store.SpawnSkip{LauncherID: choice.LauncherID, Tool: launcher.Tool,
				Variant: variant, Model: choice.Model, Provider: provider, Reason: "login_rejected"})
			if fallback == nil {
				fallback = &item
			}
			continue
		}
		if defaults.PickMode != "randomly" {
			item.Skipped = skipped
			return item, true, nil
		}
		available = append(available, item)
	}
	if len(available) > 0 {
		pick, err := rand.Int(rand.Reader, big.NewInt(int64(len(available))))
		if err != nil {
			return roleSpawnSelection{}, false, err
		}
		picked := available[int(pick.Int64())]
		picked.Skipped = skipped
		return picked, true, nil
	}
	if fallback != nil {
		// Every entry the pool could start was login-rejected: fall back to today's pick (the first skipped entry in rotation order) and
		// say that everything was unhealthy, instead of looping or refusing a spawn the old code would have made.
		fallback.Skipped = skipped
		fallback.AllUnhealthy = true
		return *fallback, true, nil
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

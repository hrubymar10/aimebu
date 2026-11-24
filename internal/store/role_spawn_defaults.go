package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/goccy/go-json"
)

// SpawnDefaultChoice names one exact model of one machine's harness.
type SpawnDefaultChoice struct {
	LauncherID string `json:"launcher_id"`
	Model      string `json:"model"`
}

// SpawnSkip says which pool entry a default spawn passed over and why. Only a provider login the usage snapshot already knows is rejected
// is recorded (reason "login_rejected"); the other availability skips (an offline machine, an unlisted model) stay as they were.
type SpawnSkip struct {
	LauncherID string `json:"launcher_id"`
	Tool       string `json:"tool,omitempty"`
	Variant    string `json:"variant,omitempty"`
	Model      string `json:"model"`
	Provider   string `json:"provider,omitempty"`
	Reason     string `json:"reason"`
}

type SpawnDefaultLauncher struct {
	MachineID string
	Tool      string
	Name      string
	Enabled   bool
}

func (s *Store) SpawnDefaultLauncher(ctx context.Context, launcherID string) (SpawnDefaultLauncher, error) {
	var result SpawnDefaultLauncher
	err := s.db.QueryRowContext(ctx, `SELECT machine_id,tool,name,enabled FROM machine_launcher WHERE id=?`, launcherID).
		Scan(&result.MachineID, &result.Tool, &result.Name, &result.Enabled)
	return result, err
}

// RoleSpawnDefaults is the effective list for one project and role. An
// override replaces the entire list and mode; an empty list means no default.
type RoleSpawnDefaults struct {
	RoleID        string               `json:"role_id"`
	RoleKey       string               `json:"role_key"`
	RoleLabel     string               `json:"role_label"`
	DefaultAgents []SpawnDefaultChoice `json:"default_agents"`
	PickMode      string               `json:"pick_mode"`
	Inherited     bool                 `json:"inherited"`
	GlobalAgents  []SpawnDefaultChoice `json:"global_agents"`
	GlobalMode    string               `json:"global_mode"`
	NextPosition  int64                `json:"next_position"`
}

var ErrInvalidSpawnDefaults = errors.New("invalid_spawn_defaults")
var ErrRoleSpawnCursorChanged = errors.New("role_spawn_cursor_changed")

// ValidateSpawnDefaults checks a backup's effective role defaults without
// changing the role, so an import preview can show defaults it will omit.
func (s *Store) ValidateSpawnDefaults(ctx context.Context, items []SpawnDefaultChoice, mode string) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.validateSpawnDefaults(items, mode) })
}

func (tx *Tx) validateSpawnDefaults(items []SpawnDefaultChoice, mode string) error {
	if mode != "in_turn" && mode != "randomly" {
		return fmt.Errorf("%w: pick_mode", ErrInvalidSpawnDefaults)
	}
	if len(items) > 32 {
		return fmt.Errorf("%w: at most 32 agents per role", ErrInvalidSpawnDefaults)
	}
	for _, item := range items {
		if item.LauncherID == "" || !ValidHarnessModelSlug(item.Model) {
			return fmt.Errorf("%w: launcher and listed model required", ErrInvalidSpawnDefaults)
		}
		var listed int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM harness_model WHERE launcher_id=? AND slug=? AND available=1`, item.LauncherID, item.Model).Scan(&listed); err != nil {
			return err
		}
		if listed == 0 {
			return fmt.Errorf("%w: model is not listed by its harness", ErrInvalidSpawnDefaults)
		}
	}
	return nil
}

func (s *Store) RoleSpawnDefaults(ctx context.Context, projectID, roleKey string) (RoleSpawnDefaults, error) {
	var result RoleSpawnDefaults
	var globalJSON string
	err := s.db.QueryRowContext(ctx, `SELECT id,role_key,label,spawn_defaults_json,spawn_pick_mode FROM role
  WHERE role_key=? AND deleted_at IS NULL`, roleKey).Scan(&result.RoleID, &result.RoleKey, &result.RoleLabel, &globalJSON, &result.GlobalMode)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(globalJSON), &result.GlobalAgents); err != nil {
		return result, err
	}
	if result.GlobalAgents == nil {
		result.GlobalAgents = []SpawnDefaultChoice{}
	}
	result.Inherited = true
	result.DefaultAgents, result.PickMode = result.GlobalAgents, result.GlobalMode
	var overrideJSON string
	err = s.db.QueryRowContext(ctx, `SELECT items_json,pick_mode FROM project_role_spawn_default WHERE project_id=? AND role_id=?`,
		projectID, result.RoleID).Scan(&overrideJSON, &result.PickMode)
	if err == nil {
		result.Inherited = false
		if err := json.Unmarshal([]byte(overrideJSON), &result.DefaultAgents); err != nil {
			return result, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if result.DefaultAgents == nil {
		result.DefaultAgents = []SpawnDefaultChoice{}
	}
	err = s.db.QueryRowContext(ctx, `SELECT next_position FROM role_spawn_cursor WHERE project_id=? AND role_id=?`,
		projectID, result.RoleID).Scan(&result.NextPosition)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	return result, nil
}

func (s *Store) ProjectRoleSpawnDefaults(ctx context.Context, projectID, actorID string) ([]RoleSpawnDefaults, error) {
	if err := s.ProjectAccess(ctx, actorID, projectID); err != nil {
		return nil, err
	}
	roles, err := s.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]RoleSpawnDefaults, 0, len(roles))
	for _, role := range roles {
		item, err := s.RoleSpawnDefaults(ctx, projectID, role.Key)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (s *Store) SetProjectRoleSpawnDefaults(ctx context.Context, projectID, actorID, roleKey string, inherit bool,
	items []SpawnDefaultChoice, mode string) (RoleSpawnDefaults, error) {
	if mode == "" {
		mode = "in_turn"
	}
	var roleID string
	err := s.Tx(ctx, func(tx *Tx) error {
		if err := tx.authorizeDefinitionActor(projectID, actorID, false); err != nil {
			return err
		}
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM role WHERE role_key=? AND deleted_at IS NULL`, roleKey).Scan(&roleID); err != nil {
			return err
		}
		if inherit {
			_, err := tx.Exec(`DELETE FROM project_role_spawn_default WHERE project_id=? AND role_id=?`, projectID, roleID)
			return err
		}
		if err := tx.validateSpawnDefaults(items, mode); err != nil {
			return err
		}
		encoded, err := json.Marshal(items)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO project_role_spawn_default(project_id,role_id,items_json,pick_mode)
  VALUES(?,?,?,?) ON CONFLICT(project_id,role_id) DO UPDATE SET items_json=excluded.items_json,pick_mode=excluded.pick_mode`,
			projectID, roleID, string(encoded), mode)
		return err
	})
	if err != nil {
		return RoleSpawnDefaults{}, err
	}
	return s.RoleSpawnDefaults(ctx, projectID, roleKey)
}

// AdvanceRoleSpawnCursor belongs in the successful spawn transaction. The
// expected value prevents concurrent callers from committing the same turn.
func (tx *Tx) AdvanceRoleSpawnCursor(projectID, roleID string, expected, steps int64) error {
	if steps < 1 {
		return ErrInvalidTransition
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO role_spawn_cursor(project_id,role_id,next_position) VALUES(?,?,0)`, projectID, roleID); err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE role_spawn_cursor SET next_position=next_position+? WHERE project_id=? AND role_id=? AND next_position=?`,
		steps, projectID, roleID, expected)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrRoleSpawnCursorChanged
	}
	return nil
}

// WarnRoleSpawnUnavailable emits at most one room-local warning per role in
// each hour. A small keyed row avoids scanning message bodies on every retry.
func (s *Store) WarnRoleSpawnUnavailable(ctx context.Context, roomID, roleKey string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		body := "No configured default agent for " + roleKey + " is available. Choose a listed harness and model manually."
		var roleID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM role WHERE role_key=? AND deleted_at IS NULL`, roleKey).Scan(&roleID); err != nil {
			return err
		}
		now := time.Now().UTC()
		result, err := tx.Exec(`INSERT INTO role_spawn_warning(room_id,role_id,last_warned_at) VALUES(?,?,?)
  ON CONFLICT(room_id,role_id) DO UPDATE SET last_warned_at=excluded.last_warned_at
  WHERE role_spawn_warning.last_warned_at<=?`, roomID, roleID, now.Format(timestampLayout), now.Add(-time.Hour).Format(timestampLayout))
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			return nil
		}
		_, err = tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: body})
		return err
	})
}

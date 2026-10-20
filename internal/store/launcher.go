package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/goccy/go-json"
)

// PathMap translates a canonical path on the supervisor into a path visible
// to the launched process. The longest matching host prefix wins.
type PathMap struct {
	Host    string `json:"host"`
	Visible string `json:"visible"`
}

type Launcher struct {
	ID             string            `json:"id"`
	MachineID      string            `json:"machine_id"`
	Tool           string            `json:"tool"`
	Name           string            `json:"name"`
	Label          string            `json:"label"`
	Command        string            `json:"command"`
	Args           []string          `json:"args"`
	EnvRefs        map[string]string `json:"env_refs"`
	Enabled        bool              `json:"enabled"`
	Containerized  bool              `json:"containerized"`
	AgentServerURL string            `json:"agent_server_url,omitempty"`
	PathMaps       []PathMap         `json:"path_maps"`
	VisibleRoots   []string          `json:"visible_roots"`
	Toolbox        bool              `json:"supports_toolbox"`
	Preset         bool              `json:"preset"`
}

type LauncherActivity struct {
	LauncherID string `json:"launcher_id"`
	Actor      string `json:"actor"`
	Action     string `json:"action"`
	CreatedAt  string `json:"created_at"`
}

func (s *Store) LauncherActivity(ctx context.Context, machineID string) ([]LauncherActivity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(launcher_id,''),COALESCE((SELECT login FROM user WHERE user.id=machine_launcher_activity.actor),actor),action,created_at FROM machine_launcher_activity WHERE machine_id=? ORDER BY created_at DESC LIMIT 100`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	activity := []LauncherActivity{}
	for rows.Next() {
		var item LauncherActivity
		if err := rows.Scan(&item.LauncherID, &item.Actor, &item.Action, &item.CreatedAt); err != nil {
			return nil, err
		}
		activity = append(activity, item)
	}
	return activity, rows.Err()
}

func scanLauncher(rows interface{ Scan(...any) error }) (Launcher, error) {
	var l Launcher
	var args, refs, maps, roots string
	err := rows.Scan(&l.ID, &l.MachineID, &l.Tool, &l.Name, &l.Label, &l.Command, &args, &refs,
		&l.Enabled, &l.Containerized, &l.AgentServerURL, &maps, &roots, &l.Toolbox, &l.Preset)
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal([]byte(args), &l.Args); err != nil {
		return l, fmt.Errorf("launcher args: %w", err)
	}
	if err := json.Unmarshal([]byte(refs), &l.EnvRefs); err != nil {
		return l, fmt.Errorf("launcher env references: %w", err)
	}
	if err := json.Unmarshal([]byte(maps), &l.PathMaps); err != nil {
		return l, fmt.Errorf("launcher path mappings: %w", err)
	}
	if err := json.Unmarshal([]byte(roots), &l.VisibleRoots); err != nil {
		return l, fmt.Errorf("launcher visible roots: %w", err)
	}
	return l, nil
}

const launcherColumns = `id,machine_id,tool,name,label,command,args_json,env_refs_json,enabled,containerized,agent_server_url,path_maps_json,visible_roots_json,supports_toolbox,preset`

func (s *Store) MachineLaunchers(ctx context.Context, machineID string) ([]Launcher, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+launcherColumns+` FROM machine_launcher WHERE machine_id=? ORDER BY tool,name`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Launcher{}
	for rows.Next() {
		item, err := scanLauncher(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) LauncherUseCounts(ctx context.Context, machineID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.launcher_id,COUNT(*) FROM agent a
		WHERE a.machine_id=? AND a.launcher_id IS NOT NULL AND a.revoked_at IS NULL
		AND (a.desired!='stopped' OR a.observed!='stopped') GROUP BY a.launcher_id`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

func (s *Store) LauncherByID(ctx context.Context, machineID, id string) (Launcher, error) {
	return scanLauncher(s.db.QueryRowContext(ctx, `SELECT `+launcherColumns+` FROM machine_launcher WHERE machine_id=? AND id=?`, machineID, id))
}

func (s *Store) LauncherByName(ctx context.Context, machineID, tool, name string) (Launcher, error) {
	if name == "docker" { // Old API requests keep their meaning.
		name = "harness-docker"
	}
	return scanLauncher(s.db.QueryRowContext(ctx, `SELECT `+launcherColumns+` FROM machine_launcher WHERE machine_id=? AND tool=? AND name=?`, machineID, tool, name))
}

var ErrLauncherInUse = errors.New("launcher_in_use")

func (s *Store) LauncherInUse(ctx context.Context, id string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, launcherInUseSQL, id, id).Scan(&count)
	return count > 0, err
}

const launcherInUseSQL = `SELECT
  (SELECT COUNT(*) FROM agent WHERE launcher_id=? AND (desired!='stopped' OR observed!='stopped')) +
  (SELECT COUNT(*) FROM agent_switch WHERE launcher_id=? AND state IN ('draining','starting'))`

func (s *Store) SaveLauncher(ctx context.Context, actor string, l Launcher) (Launcher, error) {
	if l.Name == "docker" { // reserved: old API requests use it as an alias for the Docker preset
		return l, ErrConflict
	}
	if l.ID == "" {
		id, err := newID("mln")
		if err != nil {
			return l, err
		}
		l.ID = id
	}
	args, err := json.Marshal(l.Args)
	if err != nil {
		return l, err
	}
	refs, err := json.Marshal(l.EnvRefs)
	if err != nil {
		return l, err
	}
	maps, err := json.Marshal(l.PathMaps)
	if err != nil {
		return l, err
	}
	roots, err := json.Marshal(l.VisibleRoots)
	if err != nil {
		return l, err
	}
	err = s.Tx(ctx, func(tx *Tx) error {
		var owningMachine string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT machine_id FROM machine_launcher WHERE id=?`, l.ID).Scan(&owningMachine); err == nil {
			if owningMachine != l.MachineID {
				return ErrConflict
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var duplicate int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM machine_launcher WHERE machine_id=? AND tool=? AND name=? AND id<>?`, l.MachineID, l.Tool, l.Name, l.ID).Scan(&duplicate); err != nil {
			return err
		}
		if duplicate > 0 {
			return ErrConflict
		}
		var before string
		if old, err := scanLauncher(tx.tx.QueryRowContext(ctx, `SELECT `+launcherColumns+` FROM machine_launcher WHERE id=? AND machine_id=?`, l.ID, l.MachineID)); err == nil {
			used, err := launcherInUseTx(tx, l.ID)
			if err != nil {
				return err
			}
			if used {
				return ErrLauncherInUse
			}
			data, _ := json.Marshal(old)
			before = string(data)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO machine_launcher(`+launcherColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET tool=excluded.tool,name=excluded.name,label=excluded.label,command=excluded.command,args_json=excluded.args_json,
env_refs_json=excluded.env_refs_json,enabled=excluded.enabled,containerized=excluded.containerized,
agent_server_url=excluded.agent_server_url,path_maps_json=excluded.path_maps_json,
visible_roots_json=excluded.visible_roots_json,supports_toolbox=excluded.supports_toolbox`,
			l.ID, l.MachineID, l.Tool, l.Name, l.Label, l.Command, string(args), string(refs), l.Enabled, l.Containerized,
			l.AgentServerURL, string(maps), string(roots), l.Toolbox, l.Preset); err != nil {
			return err
		}
		after, _ := json.Marshal(l)
		id, err := newID("mla")
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO machine_launcher_activity(id,machine_id,launcher_id,actor,action,before_json,after_json,created_at)
VALUES (?,?,?,?,?,?,?,?)`, id, l.MachineID, l.ID, actor, "save", before, string(after), timestamp())
		return err
	})
	return l, err
}

func launcherInUseTx(tx *Tx, id string) (bool, error) {
	var count int
	err := tx.tx.QueryRowContext(tx.ctx, launcherInUseSQL, id, id).Scan(&count)
	return count > 0, err
}

func (s *Store) DeleteLauncher(ctx context.Context, machineID, id, actor string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		used, err := launcherInUseTx(tx, id)
		if err != nil {
			return err
		}
		if used {
			return ErrLauncherInUse
		}
		if _, err := tx.Exec(`UPDATE agent SET launcher_id=NULL WHERE launcher_id=? AND desired='stopped'`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE agent_switch SET launcher_id=NULL WHERE launcher_id=?`, id); err != nil {
			return err
		}
		result, err := tx.Exec(`DELETE FROM machine_launcher WHERE id=? AND machine_id=? AND preset=0`, id, machineID)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			return sql.ErrNoRows
		}
		activityID, err := newID("mla")
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO machine_launcher_activity(id,machine_id,launcher_id,actor,action,created_at) VALUES (?,?,?,?,?,?)`,
			activityID, machineID, id, actor, "delete", timestamp())
		return err
	})
}

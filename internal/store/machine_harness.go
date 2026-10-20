package store

import (
	"context"
	"database/sql"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

func (s *Store) MachineHarnesses(ctx context.Context, machineID string) ([]protocol.HarnessRow, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT harnesses_json FROM machine WHERE id=?`, machineID).Scan(&raw); err != nil {
		return nil, err
	}
	rows := []protocol.HarnessRow{}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Store) RecordMachineHarnesses(ctx context.Context, machineID string, rows []protocol.HarnessRow) error {
	raw, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE machine SET harnesses_json=? WHERE id=?`, string(raw), machineID)
	return err
}

type HarnessActivity struct {
	Tool       string `json:"tool"`
	Variant    string `json:"variant"`
	Actor      string `json:"actor"`
	OldEnabled bool   `json:"old_enabled"`
	NewEnabled bool   `json:"new_enabled"`
	OldPath    string `json:"old_path"`
	NewPath    string `json:"new_path"`
	CreatedAt  string `json:"created_at"`
}

func (s *Store) HarnessSettings(ctx context.Context, machineID string) ([]protocol.HarnessSetting, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tool,variant,enabled,override_path FROM machine_harness_setting WHERE machine_id=?`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	settings := []protocol.HarnessSetting{}
	for rows.Next() {
		var item protocol.HarnessSetting
		if err := rows.Scan(&item.Tool, &item.Variant, &item.Enabled, &item.OverridePath); err != nil {
			return nil, err
		}
		settings = append(settings, item)
	}
	return settings, rows.Err()
}

func (s *Store) HarnessActivity(ctx context.Context, machineID string) ([]HarnessActivity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tool,variant,COALESCE((SELECT login FROM user WHERE user.id=machine_harness_activity.actor),actor),old_enabled,new_enabled,old_path,new_path,created_at FROM machine_harness_activity WHERE machine_id=? ORDER BY created_at DESC LIMIT 100`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	activity := []HarnessActivity{}
	for rows.Next() {
		var item HarnessActivity
		if err := rows.Scan(&item.Tool, &item.Variant, &item.Actor, &item.OldEnabled, &item.NewEnabled, &item.OldPath, &item.NewPath, &item.CreatedAt); err != nil {
			return nil, err
		}
		activity = append(activity, item)
	}
	return activity, rows.Err()
}

func (s *Store) SetHarnessSetting(ctx context.Context, machineID, actor string, next protocol.HarnessSetting) (bool, error) {
	changed := false
	err := s.Tx(ctx, func(tx *Tx) error {
		old := protocol.HarnessSetting{Enabled: true}
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT enabled,override_path FROM machine_harness_setting WHERE machine_id=? AND tool=? AND variant=?`, machineID, next.Tool, next.Variant).Scan(&old.Enabled, &old.OverridePath)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if old.Enabled == next.Enabled && old.OverridePath == next.OverridePath {
			return nil
		}
		if _, err := tx.Exec(`INSERT INTO machine_harness_setting(machine_id,tool,variant,enabled,override_path) VALUES (?,?,?,?,?) ON CONFLICT(machine_id,tool,variant) DO UPDATE SET enabled=excluded.enabled,override_path=excluded.override_path`, machineID, next.Tool, next.Variant, next.Enabled, next.OverridePath); err != nil {
			return err
		}
		id, err := newID("mha")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO machine_harness_activity(id,machine_id,tool,variant,actor,old_enabled,new_enabled,old_path,new_path,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, id, machineID, next.Tool, next.Variant, actor, old.Enabled, next.Enabled, old.OverridePath, next.OverridePath, timestamp()); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

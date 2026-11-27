package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var ErrMachineScope = errors.New("machine_scope")
var ErrMachineScopeActive = errors.New("machine_scope_active")

type MachineScopeError struct{ Machine, Project, Reason string }

func (e *MachineScopeError) Error() string {
	return fmt.Sprintf("machine %s cannot run project %s: %s", e.Machine, e.Project, e.Reason)
}
func (e *MachineScopeError) Is(target error) bool { return target == ErrMachineScope }

type MachineScopeActiveError struct{ Agents []string }

func (e *MachineScopeActiveError) Error() string {
	return "stop or move these active agents first: " + strings.Join(e.Agents, ", ")
}
func (e *MachineScopeActiveError) Is(target error) bool { return target == ErrMachineScopeActive }

// checkMachineProject is the single admission predicate used before a machine
// can acquire new work for a project. Listed mode with no rows admits none.
func checkMachineProject(ctx context.Context, q poolQuerier, projectID, machineID string) error {
	var machine, project, mode string
	var disabled, listed bool
	if err := q.QueryRowContext(ctx, `SELECT m.name,p.name,m.scope_mode,m.disabled,
		EXISTS(SELECT 1 FROM machine_project_scope s WHERE s.machine_id=m.id AND s.project_id=p.id)
		FROM machine m CROSS JOIN project p WHERE p.id=? AND m.id=?`, projectID, machineID).Scan(&machine, &project, &mode, &disabled, &listed); err != nil {
		return err
	}
	if disabled {
		return &MachineScopeError{machine, project, "disabled"}
	}
	if mode == "listed" && !listed {
		return &MachineScopeError{machine, project, "project is outside its allowed projects"}
	}
	return nil
}

func (s *Store) CheckMachineProject(ctx context.Context, projectID, machineID string) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	return checkMachineProject(ctx, s.db, projectID, machineID)
}

func (tx *Tx) checkMachineProject(projectID, machineID string) error {
	return checkMachineProject(tx.ctx, tx.tx, projectID, machineID)
}

// SetMachineScope refuses to drop a project while one of its agents is still
// desired active or observed live. Stopping agents remain protected until both
// states are stopped, as elsewhere in the store.
func (s *Store) SetMachineScope(ctx context.Context, machineID, mode string, allowed []string, disabled bool) error {
	return s.Tx(ctx, func(tx *Tx) error {
		if mode != "all" && mode != "listed" || mode == "all" && len(allowed) != 0 {
			return ErrInvalidTransition
		}
		var host bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT is_host FROM machine WHERE id=? AND revoked_at IS NULL`, machineID).Scan(&host); err != nil {
			return err
		}
		if disabled && !host {
			return ErrInvalidTransition
		}
		seen := map[string]bool{}
		for _, id := range allowed {
			if id == "" || seen[id] {
				return ErrInvalidTransition
			}
			seen[id] = true
			var exists bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM project WHERE id=? AND system=0)`, id).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrInvalidTransition
			}
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT a.slug,p.name,p.id FROM agent a JOIN project p ON p.id=a.project_id
			WHERE a.machine_id=? AND (a.desired!='stopped' OR a.observed!='stopped') ORDER BY p.name,a.slug`, machineID)
		if err != nil {
			return err
		}
		var blocking []string
		for rows.Next() {
			var slug, project, id string
			if err := rows.Scan(&slug, &project, &id); err != nil {
				rows.Close()
				return err
			}
			if disabled || mode == "listed" && !seen[id] {
				blocking = append(blocking, slug+" ("+project+")")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(blocking) > 0 {
			return &MachineScopeActiveError{blocking}
		}
		if _, err := tx.Exec(`UPDATE machine SET scope_mode=?,disabled=? WHERE id=?`, mode, disabled, machineID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM machine_project_scope WHERE machine_id=?`, machineID); err != nil {
			return err
		}
		for _, id := range allowed {
			if _, err := tx.Exec(`INSERT INTO machine_project_scope(machine_id,project_id) VALUES (?,?)`, machineID, id); err != nil {
				return err
			}
		}
		_, err = tx.AppendEvent(Event{Kind: "machine_scope_updated", PayloadJSON: payloadJSON(map[string]string{"machine_id": machineID})})
		return err
	})
}

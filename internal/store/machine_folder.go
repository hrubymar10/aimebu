package store

import (
	"context"
	"database/sql"
	"errors"
)

type MachineFolder struct {
	MachineID string `json:"machine_id"`
	Folder    string `json:"folder"`
}

func (s *Store) ProjectMachineFolders(ctx context.Context, projectID string) ([]MachineFolder, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.id,CASE WHEN m.is_host=1 THEN p.folder ELSE COALESCE(f.folder,'') END
		FROM machine m CROSS JOIN project p LEFT JOIN project_machine_folder f ON f.machine_id=m.id AND f.project_id=p.id
		WHERE p.id=? AND m.revoked_at IS NULL ORDER BY m.is_host DESC,m.name,m.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MachineFolder{}
	for rows.Next() {
		var item MachineFolder
		if err := rows.Scan(&item.MachineID, &item.Folder); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ProjectMachineFolder(ctx context.Context, projectID, machineID string) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var folder string
	err := s.db.QueryRowContext(ctx, `SELECT CASE WHEN m.is_host=1 THEN p.folder ELSE COALESCE(f.folder,'') END
		FROM machine m CROSS JOIN project p LEFT JOIN project_machine_folder f ON f.machine_id=m.id AND f.project_id=p.id
		WHERE p.id=? AND m.id=? AND m.revoked_at IS NULL`, projectID, machineID).Scan(&folder)
	return folder, err
}

// CanonicalizeMachineFolder records the supervisor's resolved path without
// overwriting a concurrent mapping edit. The value is still a project setting;
// this only normalizes its spelling before boundary comparisons.
func (s *Store) CanonicalizeMachineFolder(ctx context.Context, projectID, machineID, prior, canonical string) error {
	if prior == canonical {
		return nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE project_machine_folder SET folder=? WHERE project_id=? AND machine_id=? AND folder=?`,
		canonical, projectID, machineID, prior)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrInvalidTransition
	}
	return nil
}

func (s *Store) AgentCheckoutPath(ctx context.Context, agentID string) (string, error) {
	var path string
	err := s.db.QueryRowContext(ctx, `SELECT c.canonical_path FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.id=?`, agentID).Scan(&path)
	return path, err
}

// FirstMachineFolder provides a neutral mapped directory for launcher version
// probes. Spawn still validates the particular checkout it will use.
func (s *Store) FirstMachineFolder(ctx context.Context, machineID string) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var folder string
	err := s.db.QueryRowContext(ctx, `SELECT CASE WHEN m.is_host=1 THEN p.folder ELSE f.folder END
		FROM machine m JOIN project p LEFT JOIN project_machine_folder f ON f.machine_id=m.id AND f.project_id=p.id
		WHERE m.id=? AND m.revoked_at IS NULL AND
			CASE WHEN m.is_host=1 THEN p.folder ELSE f.folder END IS NOT NULL AND
			CASE WHEN m.is_host=1 THEN p.folder ELSE f.folder END != ''
		ORDER BY p.id LIMIT 1`, machineID).Scan(&folder)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return folder, err
}

func (s *Store) SetProjectMachineFolder(ctx context.Context, projectID, machineID, userID, folder string) (MachineFolder, error) {
	var result MachineFolder
	err := s.Tx(ctx, func(tx *Tx) error {
		var role string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT role FROM membership WHERE project_id=? AND user_id=?`, projectID, userID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) || err == nil && role != "owner" && role != "operator" {
			return ErrForbidden
		}
		if err != nil {
			return err
		}
		var host bool
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT is_host FROM machine WHERE id=? AND revoked_at IS NULL`, machineID).Scan(&host)
		if err != nil {
			return err
		}
		if host {
			return ErrInvalidTransition
		}
		type withdrawnRequest struct {
			agentID, roomID, path, operation, actorKind, actorID, priorDesired string
		}
		var withdrawn []withdrawnRequest
		var oldFolder string
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT folder FROM project_machine_folder WHERE project_id=? AND machine_id=?`, projectID, machineID).Scan(&oldFolder)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && oldFolder != folder {
			rows, err := tx.tx.QueryContext(tx.ctx, `SELECT b.agent_id,i.room_id,b.canonical_path,b.operation,b.requested_by_kind,b.requested_by_id,b.prior_desired
				FROM boundary_pending b JOIN inbox_item i ON i.id=b.inbox_id
				WHERE i.project_id=? AND b.machine_id=? AND b.state='open'`, projectID, machineID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var request withdrawnRequest
				if err := rows.Scan(&request.agentID, &request.roomID, &request.path, &request.operation, &request.actorKind, &request.actorID, &request.priorDesired); err != nil {
					rows.Close()
					return err
				}
				withdrawn = append(withdrawn, request)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, request := range withdrawn {
				if err := tx.withdrawBoundaryPending(request.agentID); err != nil {
					return err
				}
			}
		}
		// A new mapped root needs its own owner admission. Existing exact
		// agent grants remain tied to their original canonical paths.
		if _, err := tx.Exec(`DELETE FROM project_machine_allowed WHERE project_id=? AND machine_id=?
			AND EXISTS(SELECT 1 FROM project_machine_folder WHERE project_id=? AND machine_id=? AND folder<>?)`,
			projectID, machineID, projectID, machineID, folder); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO project_machine_folder(project_id,machine_id,folder) VALUES (?,?,?)
			ON CONFLICT(project_id,machine_id) DO UPDATE SET folder=excluded.folder`, projectID, machineID, folder)
		if err != nil {
			return err
		}
		for _, request := range withdrawn {
			if _, err := tx.createBoundaryPending(projectID, request.roomID, request.agentID, machineID,
				request.path, request.operation, request.actorKind, request.actorID); err != nil {
				return err
			}
			if request.priorDesired == "not_accepting" {
				if _, err := tx.Exec(`UPDATE boundary_pending SET prior_desired='not_accepting' WHERE agent_id=? AND state='open'`, request.agentID); err != nil {
					return err
				}
			}
		}
		_, err = tx.AppendEvent(Event{Kind: "project_machine_folder_set", ProjectID: projectID, PayloadJSON: payloadJSON(map[string]string{"machine_id": machineID, "actor_id": userID})})
		result = MachineFolder{machineID, folder}
		return err
	})
	return result, err
}

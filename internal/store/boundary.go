package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/goccy/go-json"
)

type BoundaryGrant struct {
	MachineID  string `json:"machine_id"`
	Path       string `json:"path"`
	ApprovedAt string `json:"approved_at"`
}

func (s *Store) BoundaryGrants(ctx context.Context, agentID string) ([]BoundaryGrant, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT machine_id,canonical_path,approved_at FROM agent_boundary_grant WHERE agent_id=? ORDER BY approved_at,machine_id,canonical_path`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []BoundaryGrant
	for rows.Next() {
		var grant BoundaryGrant
		if err := rows.Scan(&grant.MachineID, &grant.Path, &grant.ApprovedAt); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (tx *Tx) boundaryGrants(agentID string) ([]BoundaryGrant, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT machine_id,canonical_path,approved_at FROM agent_boundary_grant WHERE agent_id=? ORDER BY approved_at,machine_id,canonical_path`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []BoundaryGrant
	for rows.Next() {
		var grant BoundaryGrant
		if err := rows.Scan(&grant.MachineID, &grant.Path, &grant.ApprovedAt); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// WithinFolder compares path components after both paths have been resolved on
// the machine that owns them. String prefixes would admit /repo-other for /repo.
func WithinFolder(root, path string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) || filepath.Clean(root) != root || filepath.Clean(path) != path {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (tx *Tx) boundaryAllowed(projectID, agentID, machineID, path string) (bool, error) {
	var root string
	var host bool
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT CASE WHEN m.is_host=1 THEN p.folder ELSE COALESCE(f.folder,'') END,m.is_host
		FROM project p JOIN machine m ON m.id=? LEFT JOIN project_machine_folder f ON f.project_id=p.id AND f.machine_id=m.id
		WHERE p.id=? AND m.approved=1 AND m.revoked_at IS NULL`, machineID, projectID).Scan(&root, &host)
	if err != nil {
		return false, err
	}
	if host {
		if canonical, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
			root = filepath.Clean(canonical)
		}
	}
	if agentID != "" {
		var granted bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM agent_boundary_grant WHERE agent_id=? AND machine_id=? AND canonical_path=?)`, agentID, machineID, path).Scan(&granted); err != nil {
			return false, err
		}
		if granted {
			return true, nil
		}
	}
	if !WithinFolder(root, path) {
		return false, nil
	}
	if host {
		return true, nil
	}
	var allowed bool
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM project_machine_allowed WHERE project_id=? AND machine_id=?)`, projectID, machineID).Scan(&allowed)
	return allowed, err
}

func (tx *Tx) createBoundaryPending(projectID, roomID, agentID, machineID, path, operation, actorKind, actorID string) (string, error) {
	var prior string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT inbox_id FROM boundary_pending WHERE agent_id=? AND state='open'`, agentID).Scan(&prior)
	if err == nil {
		return prior, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id, err := newID("inb")
	if err != nil {
		return "", err
	}
	var mappedRoot string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT CASE WHEN m.is_host=1 THEN p.folder ELSE COALESCE(f.folder,'') END
		FROM project p JOIN machine m ON m.id=? LEFT JOIN project_machine_folder f ON f.project_id=p.id AND f.machine_id=m.id
		WHERE p.id=?`, machineID, projectID).Scan(&mappedRoot); err != nil {
		return "", err
	}
	question := fmt.Sprintf("Approve %s for agent %s on machine %s in %s? This also admits the machine for the whole project at mapped folder %s.", operation, agentID, machineID, path, mappedRoot)
	options, err := json.Marshal(inboxAsk{Text: question, Options: []string{"Approve", "Deny"}})
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
		VALUES (?,?,?,?,?,?,'approval',?,?)`, id, projectID, roomID, agentID, actorKind, actorID, string(options), timestamp()); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO boundary_pending(inbox_id,agent_id,machine_id,canonical_path,operation,mapped_root,requested_by_kind,requested_by_id,created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, id, agentID, machineID, path, operation, mappedRoot, actorKind, actorID, timestamp()); err != nil {
		return "", err
	}
	_, err = tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: projectID, RoomID: roomID, AgentID: agentID,
		PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "kind": "boundary_admission"})})
	return id, err
}

func (s *Store) IsBoundaryInbox(ctx context.Context, id string) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM boundary_pending WHERE inbox_id=?)`, id).Scan(&found)
	return found, err
}

func (s *Store) BoundaryAgentID(ctx context.Context, id string) (string, error) {
	var agentID string
	err := s.db.QueryRowContext(ctx, `SELECT agent_id FROM boundary_pending WHERE inbox_id=?`, id).Scan(&agentID)
	return agentID, err
}

func (s *Store) ProjectMachineAllowed(ctx context.Context, projectID, machineID string) (bool, error) {
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_machine_allowed WHERE project_id=? AND machine_id=?)`, projectID, machineID).Scan(&allowed)
	return allowed, err
}

// EnsureSpawnBoundary covers older agents whose unconfirmed spawn is retried
// after this feature is deployed. A live process is never stopped by this
// check; it only gates a new spawn frame.
func (s *Store) EnsureSpawnBoundary(ctx context.Context, agentID string) (bool, error) {
	allowed := false
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID, roomID, machineID, path, createdBy, desired string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,
			COALESCE(a.home_room_id,(SELECT m.room_id FROM room_member m WHERE m.member_kind='agent' AND m.member_id=a.id AND m.left_at IS NULL ORDER BY m.joined_at LIMIT 1),
				(SELECT r.id FROM room r WHERE r.project_id=a.project_id AND r.kind='home' LIMIT 1),''),
			a.machine_id,c.canonical_path,a.created_by,a.desired
			FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.id=?`, agentID).
			Scan(&projectID, &roomID, &machineID, &path, &createdBy, &desired); err != nil {
			return err
		}
		var err error
		allowed, err = tx.boundaryAllowed(projectID, agentID, machineID, path)
		if err != nil || allowed || desired == "stopped" {
			return err
		}
		if _, err := tx.createBoundaryPending(projectID, roomID, agentID, machineID, path, "spawn", "user", createdBy); err != nil {
			return err
		}
		if desired == "not_accepting" {
			if _, err := tx.Exec(`UPDATE boundary_pending SET prior_desired='not_accepting' WHERE agent_id=? AND state='open'`, agentID); err != nil {
				return err
			}
		}
		if desired != "not_accepting" {
			if _, err := tx.Exec(`UPDATE agent SET desired='not_accepting',desired_rev=desired_rev+1 WHERE id=?`, agentID); err != nil {
				return err
			}
		}
		return nil
	})
	return allowed, err
}

func (tx *Tx) withdrawBoundaryPending(agentID string) error {
	var id, projectID, roomID string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT b.inbox_id,i.project_id,i.room_id FROM boundary_pending b JOIN inbox_item i ON i.id=b.inbox_id WHERE b.agent_id=? AND b.state='open'`, agentID).Scan(&id, &projectID, &roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE boundary_pending SET state='withdrawn' WHERE inbox_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE inbox_item SET state='withdrawn',rev=rev+1,resolved_at=? WHERE id=? AND state='open'`, timestamp(), id); err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: projectID, RoomID: roomID, AgentID: agentID,
		PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "choice": "withdrawn"})})
	return err
}

func (s *Store) DecideBoundary(ctx context.Context, itemID, userID, key, choice string, rev int) (RecoveryDecision, bool, error) {
	if choice != "approve" && choice != "deny" {
		return RecoveryDecision{}, false, ErrInvalidTransition
	}
	digest := sha256.Sum256([]byte(itemID + "\x00" + choice + "\x00" + strconv.Itoa(rev)))
	result, reused, err := s.WithRequestKey(ctx, RequestKey{ActorKind: "user", ActorID: userID, Kind: "recovery_decision", Key: key, Digest: hex.EncodeToString(digest[:])}, func(tx *Tx) (string, error) {
		var projectID, roomID, agentID, machineID, path, operation, state, role, priorDesired, requestedRoot string
		var currentRev int
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT i.project_id,i.room_id,b.agent_id,b.machine_id,b.canonical_path,b.operation,i.state,i.rev,m.role,b.prior_desired,b.mapped_root
			FROM boundary_pending b JOIN inbox_item i ON i.id=b.inbox_id
			JOIN membership m ON m.project_id=i.project_id AND m.user_id=? WHERE b.inbox_id=?`, userID, itemID).
			Scan(&projectID, &roomID, &agentID, &machineID, &path, &operation, &state, &currentRev, &role, &priorDesired, &requestedRoot)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrForbidden
		}
		if err != nil {
			return "", err
		}
		if role != "owner" {
			return "", ErrForbidden
		}
		if currentRev != rev {
			return "", ErrStaleInboxRev
		}
		if state == "resolved" {
			return "", ErrAlreadyResolved
		}
		if state == "withdrawn" {
			return "", ErrAlreadyWithdrawn
		}
		if choice == "approve" {
			var machineOK, roomOpen bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT approved=1 AND revoked_at IS NULL FROM machine WHERE id=?`, machineID).Scan(&machineOK); err != nil {
				return "", err
			}
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT lifecycle='open' FROM room WHERE id=?`, roomID).Scan(&roomOpen); err != nil {
				return "", err
			}
			if !machineOK || !roomOpen {
				return "", ErrInvalidTransition
			}
			var mappedRoot string
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT CASE WHEN m.is_host=1 THEN p.folder ELSE COALESCE(f.folder,'') END
				FROM project p JOIN machine m ON m.id=? LEFT JOIN project_machine_folder f ON f.project_id=p.id AND f.machine_id=m.id
				WHERE p.id=?`, machineID, projectID).Scan(&mappedRoot); err != nil {
				return "", err
			}
			if mappedRoot != requestedRoot {
				return "", ErrInvalidTransition
			}
			if _, err := tx.Exec(`INSERT INTO project_machine_allowed(project_id,machine_id,approved_by,approved_at) VALUES (?,?,?,?)
				ON CONFLICT(project_id,machine_id) DO NOTHING`, projectID, machineID, userID, timestamp()); err != nil {
				return "", err
			}
			if _, err := tx.Exec(`INSERT INTO agent_boundary_grant(agent_id,machine_id,canonical_path,approved_by,approved_at) VALUES (?,?,?,?,?)
				ON CONFLICT(agent_id,machine_id,canonical_path) DO NOTHING`, agentID, machineID, path, userID, timestamp()); err != nil {
				return "", err
			}
			switch operation {
			case "spawn":
				var writer bool
				var checkoutID, desired string
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT c.id,a.desired,r.cap_singleton_writer FROM agent a JOIN checkout c ON c.id=a.checkout_id JOIN role r ON r.id=a.role_id WHERE a.id=?`, agentID).Scan(&checkoutID, &desired, &writer); err != nil {
					return "", err
				}
				if desired != "not_accepting" {
					return "", ErrInvalidTransition
				}
				if writer {
					var state string
					var holder sql.NullString
					if err := tx.tx.QueryRowContext(tx.ctx, `SELECT writer_state,writer_agent_id FROM checkout WHERE id=?`, checkoutID).Scan(&state, &holder); err != nil {
						return "", err
					}
					if state != "none" && (state != "reserved" || !holder.Valid || holder.String != agentID) {
						return "", ErrWriterOccupied
					}
					if _, err := tx.Exec(`UPDATE checkout SET writer_agent_id=?,writer_state='reserved' WHERE id=?`, agentID, checkoutID); err != nil {
						return "", err
					}
				}
				if priorDesired != "not_accepting" {
					priorDesired = "active"
				}
				if _, err := tx.Exec(`UPDATE agent SET desired=?,desired_rev=desired_rev+1 WHERE id=?`, priorDesired, agentID); err != nil {
					return "", err
				}
				if _, err := tx.AppendEvent(Event{Kind: "agent_spawn_requested", ProjectID: projectID, RoomID: roomID, AgentID: agentID,
					PayloadJSON: payloadJSON(map[string]string{"agent_id": agentID, "actor_kind": "user", "actor_id": userID})}); err != nil {
					return "", err
				}
			case "switch":
				var targetMachine, targetPath string
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT machine_id,COALESCE(checkout_path,'') FROM agent_switch WHERE agent_id=?`, agentID).Scan(&targetMachine, &targetPath); err != nil {
					return "", err
				}
				if targetPath == "" {
					if err := tx.tx.QueryRowContext(tx.ctx, `SELECT c.canonical_path FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.id=?`, agentID).Scan(&targetPath); err != nil {
						return "", err
					}
				}
				if targetMachine != machineID || targetPath != path {
					return "", ErrInvalidTransition
				}
			case "reactivate":
				var desired string
				var desiredRev int64
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT desired,desired_rev FROM agent WHERE id=?`, agentID).Scan(&desired, &desiredRev); err != nil {
					return "", err
				}
				if desired != "stopped" {
					return "", ErrInvalidTransition
				}
				// The approval itself authorizes this transition. Clear the
				// pending gate before calling the normal desired-state path.
				if _, err := tx.Exec(`UPDATE boundary_pending SET state='resolved' WHERE inbox_id=?`, itemID); err != nil {
					return "", err
				}
				if _, err := tx.setAgentDesired(agentID, "user", userID, "active", desiredRev, true); err != nil {
					return "", err
				}
			}
		} else {
			switch operation {
			case "spawn":
				if _, err := tx.Exec(`UPDATE agent SET desired='stopped',desired_rev=desired_rev+1 WHERE id=?`, agentID); err != nil {
					return "", err
				}
				if err := tx.confirmStoppedIfQuiescent(agentID); err != nil {
					return "", err
				}
				if _, err := tx.Exec(`UPDATE checkout SET writer_state='none',writer_agent_id=NULL
					WHERE writer_agent_id=? AND EXISTS(SELECT 1 FROM agent WHERE id=? AND observed='stopped')`, agentID, agentID); err != nil {
					return "", err
				}
			case "switch":
				resume, err := tx.switchResumeDesired(agentID)
				if err != nil {
					return "", err
				}
				if _, err := tx.Exec(`DELETE FROM agent_switch WHERE agent_id=?`, agentID); err != nil {
					return "", err
				}
				if _, err := tx.Exec(`UPDATE agent SET desired=?,desired_rev=desired_rev+1 WHERE id=?`, resume, agentID); err != nil {
					return "", err
				}
			}
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_by=?,resolved_at=? WHERE id=?`, userID, timestamp(), itemID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`UPDATE boundary_pending SET state='resolved' WHERE inbox_id=?`, itemID); err != nil {
			return "", err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: projectID, RoomID: roomID, AgentID: agentID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": itemID, "choice": choice, "actor": userID})}); err != nil {
			return "", err
		}
		decision := RecoveryDecision{ItemID: itemID, Choice: choice, Rev: rev + 1}
		encoded, err := json.Marshal(decision)
		return string(encoded), err
	})
	if err != nil {
		return RecoveryDecision{}, false, err
	}
	var decision RecoveryDecision
	if err := json.Unmarshal([]byte(result), &decision); err != nil {
		return RecoveryDecision{}, false, err
	}
	return decision, reused, nil
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

var ErrInviteInvalid = errors.New("invalid_invite")
var ErrLastOwner = errors.New("last_owner")

type UserMember struct {
	ID       string `json:"id"`
	Login    string `json:"login"`
	Disabled bool   `json:"disabled"`
	// Owner is the first account (the global owner); Admin is a flag the owner gave. Both are about the account, not the project.
	Owner     bool   `json:"owner"`
	Admin     bool   `json:"admin"`
	ProjectID string `json:"project_id,omitempty"`
	Role      string `json:"role,omitempty"`
}

type UserInvite struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	ProjectID    string `json:"project_id,omitempty"`
	Role         string `json:"role,omitempty"`
	TargetUserID string `json:"target_user_id,omitempty"`
	ExpiresAt    string `json:"expires_at"`
	UsedAt       string `json:"used_at,omitempty"`
}

func (tx *Tx) globalOwnerID() (string, error) {
	var id string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM user ORDER BY created_at,id LIMIT 1`).Scan(&id)
	return id, err
}

// Privilege levels of an account: ordinary people, admins the owner flagged, and the owner (the first account).
const (
	privOrdinary = iota
	privAdmin
	privOwner
)

// revokeInvitesBy spends every unused invitation an account issued (a link made under authority the account no longer has must not
// outlive it). A used invite is left alone: it already did its work.
func (tx *Tx) revokeInvitesBy(userID string) error {
	_, err := tx.Exec(`UPDATE user_invite SET used_at=? WHERE created_by=? AND used_at IS NULL`, timestamp(), userID)
	return err
}

// privilege is the account's level. A disabled account has none, whatever it was flagged.
func (tx *Tx) privilege(userID string) (int, error) {
	owner, err := tx.globalOwnerID()
	if err != nil {
		return privOrdinary, err
	}
	if userID == owner {
		return privOwner, nil
	}
	var admin bool
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT admin=1 AND disabled_at IS NULL FROM user WHERE id=?`, userID).Scan(&admin)
	if errors.Is(err, sql.ErrNoRows) {
		return privOrdinary, nil
	}
	if admin {
		return privAdmin, err
	}
	return privOrdinary, err
}

// mayActOn is the rule for changing another account (deactivating it, resetting its password): the owner may act on anyone, an admin
// only on ordinary accounts (acting on the owner or on a peer admin would be a way to take their account, and a deactivated admin
// keeps its flag, so it still counts as one), anyone else on none.
func (tx *Tx) mayActOn(actorID, targetID string) error {
	actor, err := tx.privilege(actorID)
	if err != nil {
		return err
	}
	switch actor {
	case privOwner:
		return nil
	case privAdmin:
		owner, err := tx.globalOwnerID()
		if err != nil {
			return err
		}
		if targetID == owner {
			return ErrForbidden
		}
		var flagged bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT admin=1 FROM user WHERE id=?`, targetID).Scan(&flagged); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if flagged {
			return ErrForbidden
		}
		return nil
	}
	return ErrForbidden
}

// canManageMembers: the owner and the admins manage the people of every project, a project's owners manage their own.
func (tx *Tx) canManageMembers(actorID, projectID string) error {
	level, err := tx.privilege(actorID)
	if err != nil {
		return err
	}
	if level >= privAdmin {
		return nil
	}
	var role string
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT m.role FROM membership m JOIN user u ON u.id=m.user_id
		WHERE m.user_id=? AND m.project_id=? AND u.disabled_at IS NULL`, actorID, projectID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) || err == nil && role != "owner" {
		return ErrForbidden
	}
	return err
}

// AllProjects lists every project (id and name) for the owner and the admins, who manage people in projects they are not members of.
func (s *Store) AllProjects(ctx context.Context, actorID string) ([]Project, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	admin, err := s.IsGlobalAdmin(ctx, actorID)
	if err != nil {
		return nil, err
	}
	if !admin {
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,folder FROM project WHERE system=0 ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Folder); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (s *Store) ListUsersForManager(ctx context.Context, actorID, projectID string) ([]UserMember, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	owner, err := s.IsGlobalAdmin(ctx, actorID)
	if err != nil {
		return nil, err
	}
	if projectID == "" && !owner {
		return nil, ErrForbidden
	}
	if projectID != "" && !owner {
		role, err := s.UserProjectRole(ctx, actorID, projectID)
		if err != nil || role != "owner" {
			return nil, ErrForbidden
		}
	}
	query := `SELECT u.id,u.login,u.disabled_at IS NOT NULL,u.id=(SELECT id FROM user ORDER BY created_at,id LIMIT 1),u.admin=1,COALESCE(m.project_id,''),COALESCE(m.role,'')
		FROM user u LEFT JOIN membership m ON m.user_id=u.id`
	args := []any{}
	if projectID != "" {
		query += ` WHERE m.project_id=?`
		args = append(args, projectID)
	}
	query += ` ORDER BY u.login,m.project_id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []UserMember{}
	for rows.Next() {
		var item UserMember
		if err := rows.Scan(&item.ID, &item.Login, &item.Disabled, &item.Owner, &item.Admin, &item.ProjectID, &item.Role); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) CreateUserInvite(ctx context.Context, actorID, kind, projectID, role, targetID, tokenHash string, expires time.Time) (UserInvite, error) {
	var invite UserInvite
	err := s.Tx(ctx, func(tx *Tx) error {
		if kind == "join" {
			if projectID == "" || targetID != "" || role != "viewer" && role != "operator" && role != "owner" {
				return ErrInvalidTransition
			}
			if err := tx.canManageMembers(actorID, projectID); err != nil {
				return err
			}
			var found string
			if err := tx.tx.QueryRowContext(ctx, `SELECT id FROM project WHERE id=? AND system=0`, projectID).Scan(&found); err != nil {
				return err
			}
		} else if kind == "reset" {
			if targetID == "" || projectID != "" || role != "" {
				return ErrForbidden
			}
			// The owner may send a reset to anyone; an admin only to an ordinary account (a reset link is a way into that account).
			if err := tx.mayActOn(actorID, targetID); err != nil {
				return err
			}
			var disabled bool
			if err := tx.tx.QueryRowContext(ctx, `SELECT disabled_at IS NOT NULL FROM user WHERE id=?`, targetID).Scan(&disabled); err != nil {
				return err
			}
			if disabled {
				return ErrInvalidTransition
			}
		} else {
			return ErrInvalidTransition
		}
		id, err := newID("inv")
		if err != nil {
			return err
		}
		invite = UserInvite{ID: id, Kind: kind, ProjectID: projectID, Role: role, TargetUserID: targetID, ExpiresAt: expires.UTC().Format(timestampLayout)}
		_, err = tx.Exec(`INSERT INTO user_invite(id,token_hash,kind,project_id,role,target_user_id,created_by,expires_at,created_at)
			VALUES (?,?,?,?,?,?,?,?,?)`, id, tokenHash, kind, nullable(projectID), nullable(role), nullable(targetID), actorID, invite.ExpiresAt, timestamp())
		if err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "user_invite_created", ProjectID: projectID, PayloadJSON: payloadJSON(map[string]string{"invite_id": id, "kind": kind, "by": actorID})})
		return err
	})
	return invite, err
}

func (s *Store) RedeemUserInvite(ctx context.Context, tokenHash, login, passwordHash, passwordParams, existingID string, now time.Time) (UserInvite, string, error) {
	var invite UserInvite
	var userID string
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID, role, targetID, usedAt sql.NullString
		var creatorID string
		err := tx.tx.QueryRowContext(ctx, `SELECT id,kind,project_id,role,target_user_id,created_by,expires_at,used_at
			FROM user_invite WHERE token_hash=?`, tokenHash).Scan(&invite.ID, &invite.Kind, &projectID, &role, &targetID, &creatorID, &invite.ExpiresAt, &usedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInviteInvalid
		}
		if err != nil {
			return err
		}
		invite.ProjectID, invite.Role, invite.TargetUserID, invite.UsedAt = projectID.String, role.String, targetID.String, usedAt.String
		if usedAt.Valid || invite.ExpiresAt <= now.UTC().Format(timestampLayout) {
			return ErrInviteInvalid
		}
		if invite.Kind == "join" {
			if err := tx.canManageMembers(creatorID, invite.ProjectID); err != nil {
				return ErrInviteInvalid
			}
			if existingID != "" {
				var disabled bool
				if err := tx.tx.QueryRowContext(ctx, `SELECT disabled_at IS NOT NULL FROM user WHERE id=?`, existingID).Scan(&disabled); err != nil || disabled {
					return ErrForbidden
				}
				userID = existingID
			} else {
				if login == "" || passwordHash == "" {
					return ErrInvalidTransition
				}
				count, _, err := canonicalLoginMatches(ctx, tx.tx, login)
				if err != nil {
					return err
				}
				if count > 0 {
					return ErrInviteInvalid
				}
				user, err := tx.CreateUser(User{Login: login, PWHash: passwordHash, PWParams: passwordParams})
				if err != nil {
					return err
				}
				userID = user.ID
			}
			var exists bool
			if err := tx.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM membership WHERE user_id=? AND project_id=?)`, userID, invite.ProjectID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return ErrConflict
			}
			if _, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, invite.ProjectID, userID, invite.Role); err != nil {
				return err
			}
		} else if invite.Kind == "reset" {
			if passwordHash == "" {
				return ErrInvalidTransition
			}
			// Like a join, a reset rechecks its issuer's authority when it is used, not only when it was made: an admin who lost the flag
			// or was deactivated since, or whose target was promoted to admin in the meantime, can no longer take that account over.
			if err := tx.mayActOn(creatorID, invite.TargetUserID); err != nil {
				return ErrInviteInvalid
			}
			userID = invite.TargetUserID
			result, err := tx.Exec(`UPDATE user SET pw_hash=?,pw_params=? WHERE id=? AND disabled_at IS NULL`, passwordHash, passwordParams, userID)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed == 0 {
				return ErrInviteInvalid
			}
			if _, err := tx.Exec(`UPDATE auth_session SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, timestamp(), userID); err != nil {
				return err
			}
		} else {
			return ErrInviteInvalid
		}
		if _, err := tx.Exec(`UPDATE user_invite SET used_at=? WHERE id=? AND used_at IS NULL`, timestamp(), invite.ID); err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "user_invite_redeemed", ProjectID: invite.ProjectID, PayloadJSON: payloadJSON(map[string]string{"invite_id": invite.ID, "user_id": userID})})
		return err
	})
	return invite, userID, err
}

// InviteInfo is what a person holding an invitation link is told before using it.
type InviteInfo struct {
	Kind        string `json:"kind"`
	ProjectName string `json:"project_name,omitempty"`
	Role        string `json:"role,omitempty"`
	// Login is the account a reset link belongs to.
	Login string `json:"login,omitempty"`
}

// InspectUserInvite reads a usable invitation without using it: the same checks as redeeming it (unused, unexpired, its issuer still
// allowed), reported as ErrInviteInvalid when any fails.
func (s *Store) InspectUserInvite(ctx context.Context, tokenHash string, now time.Time) (InviteInfo, error) {
	var info InviteInfo
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID, role, targetID, usedAt sql.NullString
		var creatorID, expires string
		err := tx.tx.QueryRowContext(ctx, `SELECT kind,project_id,role,target_user_id,created_by,expires_at,used_at
			FROM user_invite WHERE token_hash=?`, tokenHash).Scan(&info.Kind, &projectID, &role, &targetID, &creatorID, &expires, &usedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInviteInvalid
		}
		if err != nil {
			return err
		}
		if usedAt.Valid || expires <= now.UTC().Format(timestampLayout) {
			return ErrInviteInvalid
		}
		switch info.Kind {
		case "join":
			if err := tx.canManageMembers(creatorID, projectID.String); err != nil {
				return ErrInviteInvalid
			}
			info.Role = role.String
			return tx.tx.QueryRowContext(ctx, `SELECT name FROM project WHERE id=?`, projectID.String).Scan(&info.ProjectName)
		case "reset":
			if err := tx.mayActOn(creatorID, targetID.String); err != nil {
				return ErrInviteInvalid
			}
			return tx.tx.QueryRowContext(ctx, `SELECT login FROM user WHERE id=? AND disabled_at IS NULL`, targetID.String).Scan(&info.Login)
		}
		return ErrInviteInvalid
	})
	if errors.Is(err, sql.ErrNoRows) {
		return InviteInfo{}, ErrInviteInvalid
	}
	return info, err
}

func (s *Store) SetProjectMember(ctx context.Context, actorID, projectID, targetID, role string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		if err := tx.canManageMembers(actorID, projectID); err != nil {
			return err
		}
		if role != "" && role != "owner" && role != "operator" && role != "viewer" {
			return ErrInvalidTransition
		}
		// An admin works on ordinary people and on their own membership, not on the owner's or a peer admin's.
		if level, err := tx.privilege(actorID); err != nil {
			return err
		} else if level == privAdmin && targetID != actorID {
			if err := tx.mayActOn(actorID, targetID); err != nil {
				return err
			}
		}
		var oldRole string
		err := tx.tx.QueryRowContext(ctx, `SELECT role FROM membership WHERE project_id=? AND user_id=?`, projectID, targetID).Scan(&oldRole)
		if err != nil {
			return err
		}
		if role != "" {
			var disabled bool
			if err := tx.tx.QueryRowContext(ctx, `SELECT disabled_at IS NOT NULL FROM user WHERE id=?`, targetID).Scan(&disabled); err != nil {
				return err
			}
			if disabled {
				return ErrForbidden
			}
		}
		if oldRole == "owner" && role != "owner" {
			var others int
			if err := tx.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM membership m JOIN user u ON u.id=m.user_id
				WHERE m.project_id=? AND m.role='owner' AND m.user_id<>? AND u.disabled_at IS NULL`, projectID, targetID).Scan(&others); err != nil {
				return err
			}
			if others == 0 {
				return ErrLastOwner
			}
		}
		if role == "" {
			_, err = tx.Exec(`DELETE FROM membership WHERE project_id=? AND user_id=?`, projectID, targetID)
		}
		if role != "" {
			_, err = tx.Exec(`UPDATE membership SET role=? WHERE project_id=? AND user_id=?`, role, projectID, targetID)
		}
		if err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "project_member_changed", ProjectID: projectID, PayloadJSON: payloadJSON(map[string]string{"user_id": targetID, "role": role})})
		return err
	})
}

func (s *Store) DeactivateUser(ctx context.Context, actorID, targetID string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		owner, err := tx.globalOwnerID()
		if err != nil {
			return err
		}
		if targetID == owner {
			return ErrForbidden
		}
		if err := tx.mayActOn(actorID, targetID); err != nil {
			return err
		}
		var noOtherOwner bool
		err = tx.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM membership m WHERE m.user_id=? AND m.role='owner'
			AND NOT EXISTS(SELECT 1 FROM membership other JOIN user u ON u.id=other.user_id
			WHERE other.project_id=m.project_id AND other.role='owner' AND other.user_id<>? AND u.disabled_at IS NULL))`, targetID, targetID).Scan(&noOtherOwner)
		if err != nil {
			return err
		}
		if noOtherOwner {
			return ErrLastOwner
		}
		result, err := tx.Exec(`UPDATE user SET disabled_at=? WHERE id=? AND disabled_at IS NULL`, timestamp(), targetID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return ErrInvalidTransition
		}
		if _, err := tx.Exec(`UPDATE auth_session SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, timestamp(), targetID); err != nil {
			return err
		}
		if err := tx.revokeInvitesBy(targetID); err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "user_deactivated", PayloadJSON: payloadJSON(map[string]string{"user_id": targetID})})
		return err
	})
}

// SetUserAdmin gives or takes away the admin flag. Only the owner may, never on the owner, and a disabled account cannot be
// made an admin (taking the flag away from one is fine).
func (s *Store) SetUserAdmin(ctx context.Context, actorID, targetID string, admin bool) error {
	return s.Tx(ctx, func(tx *Tx) error {
		level, err := tx.privilege(actorID)
		if err != nil {
			return err
		}
		if level != privOwner {
			return ErrForbidden
		}
		owner, err := tx.globalOwnerID()
		if err != nil {
			return err
		}
		if targetID == owner {
			return ErrInvalidTransition
		}
		var disabled, flagged bool
		if err := tx.tx.QueryRowContext(ctx, `SELECT disabled_at IS NOT NULL,admin=1 FROM user WHERE id=?`, targetID).Scan(&disabled, &flagged); err != nil {
			return err
		}
		if admin && disabled {
			return ErrInvalidTransition
		}
		if flagged == admin {
			return nil
		}
		if _, err := tx.Exec(`UPDATE user SET admin=? WHERE id=?`, admin, targetID); err != nil {
			return err
		}
		// An admin's authority is what its outstanding links were issued under: when it goes, so do they.
		if !admin {
			if err := tx.revokeInvitesBy(targetID); err != nil {
				return err
			}
		}
		_, err = tx.AppendEvent(Event{Kind: "user_admin_changed", PayloadJSON: payloadJSON(map[string]string{"user_id": targetID, "admin": strconv.FormatBool(admin), "by": actorID})})
		return err
	})
}

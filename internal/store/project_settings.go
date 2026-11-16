package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

type ProjectRecoverySettings struct {
	AutoResumeAfterCleanDiagnostic bool `json:"auto_resume_after_clean_diagnostic"`
	// WorktreeMode is "auto" (task rooms get their own Git worktree when the project folder is a Git root) or "off".
	WorktreeMode string `json:"worktree_mode"`
	// HomeLeadOnly limits spawning into the project's home room to roles with the lead capability (default on).
	HomeLeadOnly                bool   `json:"home_lead_only"`
	MaxLiveAgents               int    `json:"max_live_agents"`
	ReviewerAgents              string `json:"reviewer_agents"`
	ReviewerAgentsEnabled       bool   `json:"reviewer_agents_enabled"`
	ReviewerAgentsGlobalEnabled bool   `json:"reviewer_agents_global_enabled"`
}

// ProjectSettingsUpdate is a partial update of the project settings; a nil field is left as it is.
type ProjectSettingsUpdate struct {
	AutoResumeAfterCleanDiagnostic *bool
	WorktreeMode                   *string
	HomeLeadOnly                   *bool
	MaxLiveAgents                  *int
	ReviewerAgents                 *string
}

// RoomCheckoutMode is where a task room's agents work once that has been decided: "worktree", "project_folder", or "" before the first spawn.
func (s *Store) RoomCheckoutMode(ctx context.Context, roomID string) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, `SELECT checkout_mode FROM room WHERE id=?`, roomID).Scan(&mode)
	return mode, err
}

// DecideRoomCheckout records the room's checkout mode if none has been decided and returns the mode in force, which is the earlier
// decision when there was one. With force, an explicit request for a worktree replaces a decision of "project_folder".
func (s *Store) DecideRoomCheckout(ctx context.Context, roomID, mode string, force bool) (string, error) {
	if mode != "project_folder" && mode != "worktree" {
		return "", ErrInvalidTransition
	}
	query := `UPDATE room SET checkout_mode=? WHERE id=? AND checkout_mode=''`
	if force {
		query = `UPDATE room SET checkout_mode=? WHERE id=? AND checkout_mode<>'worktree'`
	}
	if _, err := s.db.ExecContext(ctx, query, mode, roomID); err != nil {
		return "", err
	}
	return s.RoomCheckoutMode(ctx, roomID)
}

// AutoWorktreeApplies says whether a task room gets its worktree by default: the project's mode is "auto" and the room was created at
// or after the moment automatic worktrees took effect, so turning the setting on never splits a room that already works in the project
// folder. A project without a settings row is "auto" from its start.
func (s *Store) AutoWorktreeApplies(ctx context.Context, roomID string) (bool, error) {
	var mode, since, created string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(s.worktree_mode,'auto'),COALESCE(s.worktree_auto_since,''),r.created_at
		FROM room r LEFT JOIN settings_project s ON s.project_id=r.project_id WHERE r.id=?`, roomID).Scan(&mode, &since, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return mode == "auto" && created >= since, err
}

func (s *Store) GetProjectRecoverySettings(ctx context.Context, projectID, userID string) (ProjectRecoverySettings, error) {
	if err := s.ProjectAccess(ctx, userID, projectID); err != nil {
		return ProjectRecoverySettings{}, err
	}
	var settings ProjectRecoverySettings
	err := s.db.QueryRowContext(ctx, `SELECT auto_resume_after_clean_diagnostic,worktree_mode,home_lead_only,max_live_agents,reviewer_agents FROM settings_project WHERE project_id=?`, projectID).
		Scan(&settings.AutoResumeAfterCleanDiagnostic, &settings.WorktreeMode, &settings.HomeLeadOnly, &settings.MaxLiveAgents, &settings.ReviewerAgents)
	if err == nil {
		err = s.fillReviewerSettings(ctx, &settings)
	}
	return settings, err
}

// SetProjectRecoverySettings stores the project settings that were sent; a nil field is left as it is.
func (s *Store) SetProjectRecoverySettings(ctx context.Context, projectID, userID string, update ProjectSettingsUpdate) (ProjectRecoverySettings, error) {
	autoResume, worktreeMode := update.AutoResumeAfterCleanDiagnostic, update.WorktreeMode
	if worktreeMode != nil && *worktreeMode != "auto" && *worktreeMode != "off" {
		return ProjectRecoverySettings{}, ErrInvalidTransition
	}
	if update.MaxLiveAgents != nil && (*update.MaxLiveAgents < 1 || *update.MaxLiveAgents > 100) {
		return ProjectRecoverySettings{}, ErrInvalidTransition
	}
	if update.ReviewerAgents != nil && !validReviewerOverride(*update.ReviewerAgents) {
		return ProjectRecoverySettings{}, ErrInvalidTransition
	}
	var settings ProjectRecoverySettings
	err := s.Tx(ctx, func(tx *Tx) error {
		var role string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT role FROM membership WHERE project_id=? AND user_id=?`, projectID, userID).Scan(&role); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrForbidden
			}
			return err
		}
		if role != "owner" && role != "operator" {
			return ErrForbidden
		}
		if autoResume != nil {
			if _, err := tx.Exec(`UPDATE settings_project SET auto_resume_after_clean_diagnostic=? WHERE project_id=?`, *autoResume, projectID); err != nil {
				return err
			}
		}
		if update.HomeLeadOnly != nil {
			if _, err := tx.Exec(`UPDATE settings_project SET home_lead_only=? WHERE project_id=?`, *update.HomeLeadOnly, projectID); err != nil {
				return err
			}
		}
		if update.MaxLiveAgents != nil {
			if _, err := tx.Exec(`UPDATE settings_project SET max_live_agents=? WHERE project_id=?`, *update.MaxLiveAgents, projectID); err != nil {
				return err
			}
		}
		if update.ReviewerAgents != nil {
			if _, err := tx.Exec(`UPDATE settings_project SET reviewer_agents=? WHERE project_id=?`, *update.ReviewerAgents, projectID); err != nil {
				return err
			}
		}
		if worktreeMode != nil {
			var current string
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT worktree_mode FROM settings_project WHERE project_id=?`, projectID).Scan(&current); err != nil {
				return err
			}
			if current != *worktreeMode {
				// Switching on starts the clock: only rooms created from now on get automatic worktrees.
				if _, err := tx.Exec(`UPDATE settings_project SET worktree_mode=?,worktree_auto_since=? WHERE project_id=?`, *worktreeMode, timestamp(), projectID); err != nil {
					return err
				}
			}
		}
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT auto_resume_after_clean_diagnostic,worktree_mode,home_lead_only,max_live_agents,reviewer_agents FROM settings_project WHERE project_id=?`, projectID).
			Scan(&settings.AutoResumeAfterCleanDiagnostic, &settings.WorktreeMode, &settings.HomeLeadOnly, &settings.MaxLiveAgents, &settings.ReviewerAgents); err != nil {
			return err
		}
		if err := tx.fillReviewerSettings(&settings); err != nil {
			return err
		}
		_, err := tx.AppendEvent(Event{Kind: "project_settings_updated", ProjectID: projectID,
			PayloadJSON: payloadJSON(map[string]string{"auto_resume_after_clean_diagnostic": strconv.FormatBool(settings.AutoResumeAfterCleanDiagnostic), "worktree_mode": settings.WorktreeMode, "home_lead_only": strconv.FormatBool(settings.HomeLeadOnly), "max_live_agents": strconv.Itoa(settings.MaxLiveAgents), "reviewer_agents": settings.ReviewerAgents})})
		return err
	})
	return settings, err
}

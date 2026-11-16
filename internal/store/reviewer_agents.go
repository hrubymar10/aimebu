package store

import (
	"context"
	"errors"
	"strings"
)

const ReviewerAgentsGlobalKey = "reviewer_agents_enabled"

var ErrReviewerAgentsDisabled = errors.New("reviewer agents disabled")

func validReviewerOverride(value string) bool {
	return value == "inherit" || value == "on" || value == "off"
}

func reviewerEnabled(global, override string) bool {
	switch override {
	case "on":
		return true
	case "off":
		return false
	default:
		return global != "0"
	}
}

func (s *Store) fillReviewerSettings(ctx context.Context, settings *ProjectRecoverySettings) error {
	global, _, err := s.GlobalSettingValue(ctx, ReviewerAgentsGlobalKey)
	if err != nil {
		return err
	}
	settings.ReviewerAgentsGlobalEnabled = global != "0"
	settings.ReviewerAgentsEnabled = reviewerEnabled(global, settings.ReviewerAgents)
	return nil
}

func (tx *Tx) fillReviewerSettings(settings *ProjectRecoverySettings) error {
	var global string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE((SELECT value FROM global_setting WHERE key=?),'1')`, ReviewerAgentsGlobalKey).Scan(&global); err != nil {
		return err
	}
	settings.ReviewerAgentsGlobalEnabled = global != "0"
	settings.ReviewerAgentsEnabled = reviewerEnabled(global, settings.ReviewerAgents)
	return nil
}

func (tx *Tx) reviewerSpawnDisabled(projectID, roleKey string) (bool, error) {
	isReviewer, err := tx.roleLineageContains(roleKey, "reviewer")
	if err != nil || !isReviewer {
		return false, err
	}
	enabled, err := tx.projectReviewerEnabled(projectID)
	return !enabled, err
}

func (tx *Tx) roleLineageContains(roleKey string, targets ...string) (bool, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `WITH RECURSIVE lineage(role_key,extends,depth) AS (
		SELECT role_key,extends,0 FROM role WHERE role_key=? AND deleted_at IS NULL
		UNION ALL
		SELECT parent.role_key,parent.extends,child.depth+1 FROM role parent JOIN lineage child ON parent.role_key=child.extends
		WHERE parent.deleted_at IS NULL AND child.depth<?
	)
	SELECT role_key FROM lineage`, roleKey, maxRoleExtendsDepth)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var member string
		if err := rows.Scan(&member); err != nil {
			return false, err
		}
		for _, target := range targets {
			if member == target {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

func (tx *Tx) projectReviewerEnabled(projectID string) (bool, error) {
	var override, global string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT sp.reviewer_agents,COALESCE((SELECT value FROM global_setting WHERE key=?),'1')
		FROM settings_project sp WHERE sp.project_id=?`, ReviewerAgentsGlobalKey, projectID).Scan(&override, &global)
	return reviewerEnabled(global, override), err
}

// The first member is a reviewer-staffing clause in a shipped role or flow;
// the second is the same instruction without reviewer staffing. Tests pin
// every clause to its role-text source so edits cannot silently skip the gate.
var reviewerStaffingClauses = [][2]string{
	{"delegate implementation and independent review", "delegate implementation"},
	{"Follow worker handoffs, request independent reviews, consolidate findings, and direct fixes.", "Follow worker handoffs and direct fixes."},
	{"how many workers and whether a reviewer is needed", "how many workers are needed"},
	{"Do not spawn or manage workers and reviewers.", "Do not spawn or manage workers."},
	{"spawn and brief workers and reviewers, run review, gate, and fix rounds", "spawn and brief workers, run gate and fix rounds"},
	{"Spawn only the workers and reviewers called for by the specification or the leader. Add a reviewer only when the specification or leader asks for one; if the specification does not say, ask the leader in routing before spawning a reviewer.", "Spawn only the workers called for by the specification or the leader."},
	{"Manage workers and reviewers in task rooms.", "Manage workers in task rooms."},
}

// Omit reviewer staffing instructions from leader and dispatcher deliveries while reviewer spawning is disabled.
func withoutReviewerStaffing(body string) string {
	for _, pair := range reviewerStaffingClauses {
		body = strings.ReplaceAll(body, pair[0], pair[1])
	}
	return body
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// A role's prompt extension is text an agent receives after the role's instructions without replacing them: the built-in default
// can keep updating while the instance or a project adds its own rules. The global scope (project_id NULL) sits with the role
// definition and is the instance owner's alone; a project's extension belongs to that project's owner or operators. The role's
// body column is untouched, so the role-body migrations keep treating only the base as edited or unedited: an extension never
// counts as an edit.

// MaxRoleBodyExtensionLen caps one extension in bytes. The role body itself allows 32768; an extension only adds to it, so half
// of that leaves room for the base and the additions dispatch appends.
const MaxRoleBodyExtensionLen = 16384

// ErrRoleBodyExtension rejects an extension longer than MaxRoleBodyExtensionLen.
var ErrRoleBodyExtension = errors.New("invalid_role_body_extension")

// RoleBodyExtension is one saved extension: its text, revision and last update. Body is empty for a scope with none saved.
type RoleBodyExtension struct {
	Body      string `json:"body"`
	Rev       int64  `json:"rev"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type extensionQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// roleBodyExtensionRows reads the saved extensions of one role in one scope (an empty projectID is the global scope).
func roleBodyExtensionRow(ctx context.Context, q extensionQuerier, roleID, projectID string) (RoleBodyExtension, error) {
	var out RoleBodyExtension
	err := q.QueryRowContext(ctx, `SELECT body,rev,updated_at FROM role_body_extension WHERE role_id=? AND COALESCE(project_id,'')=?`,
		roleID, projectID).Scan(&out.Body, &out.Rev, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RoleBodyExtension{}, nil
	}
	return out, err
}

// roleBodyExtensionTexts reads the global and the project extension text of one role for delivery.
func roleBodyExtensionTexts(ctx context.Context, q extensionQuerier, roleID, projectID string) (global, project string, err error) {
	rows, err := q.QueryContext(ctx, `SELECT COALESCE(project_id,''),body FROM role_body_extension WHERE role_id=? AND COALESCE(project_id,'') IN ('',?)`,
		roleID, projectID)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	for rows.Next() {
		var scope, body string
		if err := rows.Scan(&scope, &body); err != nil {
			return "", "", err
		}
		if scope == "" {
			global = body
		} else {
			project = body
		}
	}
	return global, project, rows.Err()
}

// joinBodyParts joins the non-empty parts with a blank line, so an empty part leaves no stray blank line.
func joinBodyParts(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if body := strings.TrimSpace(part); body != "" {
			out = append(out, body)
		}
	}
	return strings.Join(out, "\n\n")
}

// SetRoleBodyExtension saves or clears one extension of a live role. An empty (or blank) body clears the saved one. rev is 0 to
// create (or to clear a scope with nothing saved) or the current revision to change or clear it. The global scope (an empty
// projectID) is the instance owner's alone; a project's belongs to its owner or operators. Resetting a built-in role does not
// touch its extension.
func (s *Store) SetRoleBodyExtension(ctx context.Context, actorID, projectID, roleID, body string, rev int64) (RoleBodyExtension, error) {
	if len(body) > MaxRoleBodyExtensionLen {
		return RoleBodyExtension{}, ErrRoleBodyExtension
	}
	var result RoleBodyExtension
	err := s.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.roleForExtension(roleID); err != nil {
			return err
		}
		if projectID == "" {
			level, err := tx.privilege(actorID)
			if err != nil {
				return err
			}
			if level < privOwner {
				return ErrForbidden
			}
		} else if err := tx.authorizeDefinitionActor(projectID, actorID, false); err != nil {
			return err
		}
		saved, err := tx.saveRoleBodyExtension(roleID, projectID, body, rev, false)
		if err != nil {
			return err
		}
		result = saved
		return nil
	})
	return result, err
}

// saveRoleBodyExtension writes one scope of a role's extension. An empty (or blank) body clears the saved one. Without force, rev
// is 0 to create (or to clear a scope with nothing saved) or the current revision to change or clear it; with force — a write
// guarded elsewhere, such as a role definition's own revision — the saved text is replaced or cleared without a revision check.
func (tx *Tx) saveRoleBodyExtension(roleID, projectID, body string, rev int64, force bool) (RoleBodyExtension, error) {
	body = strings.TrimSpace(body)
	if len(body) > MaxRoleBodyExtensionLen {
		return RoleBodyExtension{}, ErrRoleBodyExtension
	}
	current, err := roleBodyExtensionRow(tx.ctx, tx.tx, roleID, projectID)
	if err != nil {
		return RoleBodyExtension{}, err
	}
	if current.Rev == 0 {
		if !force && rev != 0 {
			return RoleBodyExtension{}, ErrStaleRev
		}
		if body == "" {
			return RoleBodyExtension{}, nil
		}
		var project any
		if projectID != "" {
			project = projectID
		}
		if _, err := tx.Exec(`INSERT INTO role_body_extension(role_id,project_id,body,rev,updated_at) VALUES (?,?,?,1,?)`,
			roleID, project, body, timestamp()); err != nil {
			return RoleBodyExtension{}, err
		}
		return RoleBodyExtension{Body: body, Rev: 1, UpdatedAt: timestamp()}, nil
	}
	if !force && current.Rev != rev {
		return RoleBodyExtension{}, ErrStaleRev
	}
	if body == "" {
		if _, err := tx.Exec(`DELETE FROM role_body_extension WHERE role_id=? AND COALESCE(project_id,'')=?`, roleID, projectID); err != nil {
			return RoleBodyExtension{}, err
		}
		return RoleBodyExtension{}, nil
	}
	if _, err := tx.Exec(`UPDATE role_body_extension SET body=?,rev=rev+1,updated_at=? WHERE role_id=? AND COALESCE(project_id,'')=?`,
		body, timestamp(), roleID, projectID); err != nil {
		return RoleBodyExtension{}, err
	}
	return RoleBodyExtension{Body: body, Rev: current.Rev + 1, UpdatedAt: timestamp()}, nil
}

// RoleIDByKey answers the id of the live role with this key; sql.ErrNoRows when there is none.
func (s *Store) RoleIDByKey(ctx context.Context, roleKey string) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM role WHERE role_key=? AND deleted_at IS NULL`, roleKey).Scan(&id)
	return id, err
}

// roleForExtension answers the live role an extension names; a deleted role takes none.
func (tx *Tx) roleForExtension(roleID string) (RoleDefinition, error) {
	roles, err := allLiveRoles(tx.ctx, tx.tx)
	if err != nil {
		return RoleDefinition{}, err
	}
	for _, r := range roles {
		if r.ID == roleID {
			return r, nil
		}
	}
	return RoleDefinition{}, sql.ErrNoRows
}

// globalRoleBodyExtensions reads every global extension by role id, so the roles list can carry them in one query.
func globalRoleBodyExtensions(ctx context.Context, q extensionQuerier) (map[string]RoleBodyExtension, error) {
	rows, err := q.QueryContext(ctx, `SELECT role_id,body,rev,updated_at FROM role_body_extension WHERE project_id IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RoleBodyExtension{}
	for rows.Next() {
		var roleID string
		var ext RoleBodyExtension
		if err := rows.Scan(&roleID, &ext.Body, &ext.Rev, &ext.UpdatedAt); err != nil {
			return nil, err
		}
		out[roleID] = ext
	}
	return out, rows.Err()
}

// RolePromptSettings is one role as a project's role-prompt settings show it: the base the extensions append to, both saved
// extensions, and the text an agent of this role in this project is delivered (the same composition dispatch uses, routing
// room included).
type RolePromptSettings struct {
	RoleID              string             `json:"role_id"`
	RoleKey             string             `json:"role_key"`
	Label               string             `json:"label"`
	Emoji               string             `json:"emoji"`
	Builtin             bool               `json:"builtin"`
	ResolvedBody        string             `json:"resolved_body"`
	FlowOverride        bool               `json:"flow_override"`
	GlobalBodyExtension string             `json:"global_body_extension,omitempty"`
	Project             *RoleBodyExtension `json:"project_extension,omitempty"`
	DeliveredBody       string             `json:"delivered_body"`
}

// RolePromptSettings lists every live role as seen from one project, for its owner or operators.
func (s *Store) RolePromptSettings(ctx context.Context, actorID, projectID string) ([]RolePromptSettings, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	var result []RolePromptSettings
	err := s.Tx(ctx, func(tx *Tx) error {
		if err := tx.authorizeDefinitionActor(projectID, actorID, false); err != nil {
			return err
		}
		roles, err := allLiveRoles(tx.ctx, tx.tx)
		if err != nil {
			return err
		}
		var flowKey string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT team_flow_key FROM project WHERE id=?`, projectID).Scan(&flowKey); err != nil {
			return err
		}
		flow, _ := teamFlowTemplate(flowKey)
		result = make([]RolePromptSettings, 0, len(roles))
		for _, role := range roles {
			view := RolePromptSettings{
				RoleID: role.ID, RoleKey: role.Key, Label: role.Label, Emoji: role.Emoji, Builtin: role.Builtin,
				ResolvedBody: resolveRoleBody(role, roles),
			}
			_, view.FlowOverride = flow.RoleTextOverrides[role.Key]
			global, project, err := roleBodyExtensionTexts(tx.ctx, tx.tx, role.ID, projectID)
			if err != nil {
				return err
			}
			view.GlobalBodyExtension = global
			if project != "" {
				row, err := roleBodyExtensionRow(tx.ctx, tx.tx, role.ID, projectID)
				if err != nil {
					return err
				}
				view.Project = &row
			}
			delivered, _, err := tx.deliveredRoleBody(role.ID, projectID)
			if err != nil {
				return err
			}
			view.DeliveredBody = delivered
			result = append(result, view)
		}
		return nil
	})
	return result, err
}

// deliveredRoleBody composes what an agent holding roleID in projectID receives as its instructions: the team flow's role text
// override where one exists, else the role's resolved body (parent chain first), then the role's global and project prompt
// extensions, skipping empty parts, then the flow- and settings-driven additions. Dispatch and the settings preview share it, so
// a preview shows the text an agent is actually delivered. The lead flag rides along: dispatch needs it for the handbook it adds
// to a lead's context.
func (tx *Tx) deliveredRoleBody(roleID, projectID string) (string, bool, error) {
	roleBody, err := tx.effectiveRoleBody(roleID)
	if err != nil {
		return "", false, err
	}
	var flowKey, roleKey string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT p.team_flow_key,r.role_key FROM project p JOIN role r ON r.id=? WHERE p.id=?`, roleID, projectID).Scan(&flowKey, &roleKey); err != nil {
		return "", false, err
	}
	if flow, ok := teamFlowTemplate(flowKey); ok {
		if override, ok := flow.RoleTextOverrides[roleKey]; ok {
			roleBody = override
		}
	}
	global, project, err := roleBodyExtensionTexts(tx.ctx, tx.tx, roleID, projectID)
	if err != nil {
		return "", false, err
	}
	roleBody = joinBodyParts(roleBody, global, project)
	if flowKey == "leader-dispatcher" && roleKey == "leader" {
		var routingID, routingTitle string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT id,title FROM room WHERE project_id=? AND team_flow_room_key='routing' AND lifecycle='open' ORDER BY created_at LIMIT 1`, projectID).Scan(&routingID, &routingTitle)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}
		if err == nil {
			roleBody += fmt.Sprintf("\n\nHand specifications over in %s (%s).", routingTitle, routingID)
		}
	}
	staffingRole, err := tx.roleLineageContains(roleKey, "leader", "dispatcher")
	if err != nil {
		return "", false, err
	}
	if staffingRole {
		roleBody += "\n\nBefore spawn_agent, inspect team_roster for an active idle agent of the same role in the target room. The tool returns 409 idle_agent_exists for that duplicate; pass a concrete reason only when a second agent is intentional."
		if enabled, err := tx.projectReviewerEnabled(projectID); err != nil {
			return "", false, err
		} else if !enabled {
			roleBody = withoutReviewerStaffing(roleBody)
		}
	}
	if flowKey == "leader-dispatcher" && (roleKey == "worker" || roleKey == "reviewer" || strings.HasSuffix(roleKey, "-reviewer")) {
		roleBody += "\n\nIn a task room, report to the agent who briefed you (normally the dispatcher) and @mention them; a handoff reaches the dispatcher. Do not @mention the leader there; the leader is not a member of task rooms in this flow."
	}
	if roleBody, err = tx.withInlinePlan(roleID, roleBody); err != nil {
		return "", false, err
	}
	var lead bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT cap_is_lead FROM role WHERE id=?`, roleID).Scan(&lead); err != nil {
		return "", false, err
	}
	if lead {
		roleBody += "\n\nWhen spawning a role, strongly prefer its configured default agent. Call spawn_agent with role_key and omit tool, variant, and model to select the next available default. If you need a different harness or model, provide a concrete reason. If no default is available, choose a listed harness and model yourself and tell the room why."
		if !strings.Contains(roleBody, "Maintain the project handbook") {
			roleBody += "\n\nMaintain the project handbook with handbook_get and handbook_update at decisions and handoffs. At close-out, update Current state and Open backlog and append a concise Timeline entry. Keep canonical details in repository docs. If the handbook exceeds about 1000 lines, compress its oldest Timeline entries."
		}
	}
	return roleBody, lead, nil
}

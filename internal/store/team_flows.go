package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type FlowRoom struct {
	Key        string `json:"key"`
	Title      string `json:"title"`
	Kind       string `json:"kind"`
	Restricted bool   `json:"restricted"`
}
type FlowDiagramRoom struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Roles []string `json:"roles"`
}
type FlowEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type FlowJoinRule struct {
	RoomKind     string   `json:"room_kind"`
	AllowedRoles []string `json:"allowed_roles"`
}
type FlowSpawnRule struct {
	ActorRole    string   `json:"actor_role"`
	AllowedRoles []string `json:"allowed_roles"`
}
type TeamFlowTemplate struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Diagram     struct {
		Rooms []FlowDiagramRoom `json:"rooms"`
		Edges []FlowEdge        `json:"edges"`
	} `json:"diagram"`
	Rooms             []FlowRoom        `json:"rooms"`
	JoinRules         []FlowJoinRule    `json:"join_rules"`
	SpawnRules        []FlowSpawnRule   `json:"spawn_rules"`
	RoleTextOverrides map[string]string `json:"role_text_overrides"`
	AutoJoinRoles     []string          `json:"auto_join_roles"`
	CoreRoles         []string          `json:"core_roles"`
	TaskParentKey     string            `json:"task_parent_key"`
	GrantRoles        []string          `json:"grant_roles"`
}
type FlowConflict struct {
	AgentID         string `json:"agent_id"`
	Reason          string `json:"reason"`
	SuggestedAction string `json:"suggested_action"`
}
type TeamFlowState struct {
	Key           string           `json:"key"`
	Rev           int64            `json:"rev"`
	ReviewPending bool             `json:"review_pending"`
	Template      TeamFlowTemplate `json:"template"`
	Conflicts     []FlowConflict   `json:"conflicts"`
}
type TeamFlowPreview struct {
	Key                      string                     `json:"key"`
	CreateRooms              []FlowRoom                 `json:"create_rooms"`
	PreservedRestrictedRooms []FlowPreservedRestriction `json:"preserved_restricted_rooms"`
	RoleChanges              []any                      `json:"role_changes"`
	Conflicts                []FlowConflict             `json:"conflicts"`
}
type FlowPreservedRestriction struct {
	RoomID string `json:"room_id"`
	Title  string `json:"title"`
}
type TeamFlowApplied struct {
	Key           string         `json:"key"`
	Rev           int64          `json:"rev"`
	ReviewPending bool           `json:"review_pending"`
	CreatedRooms  []Room         `json:"created_rooms"`
	Conflicts     []FlowConflict `json:"conflicts"`
}

var ErrTeamFlowRule = errors.New("team flow rule forbids this action")
var ErrTeamFlowRev = errors.New("team flow revision changed")

func TeamFlowTemplates() []TeamFlowTemplate {
	base := TeamFlowTemplate{Key: "leader-workers", Label: "Leader and workers", Description: "The leader plans with you and manages workers and reviewers."}
	base.Rooms = []FlowRoom{{Key: "home", Title: "Home", Kind: "home"}, {Key: "general", Title: "general", Kind: "task"}}
	base.Diagram.Rooms = []FlowDiagramRoom{{"home", "Home", []string{"leader"}}, {"general", "General and tasks", []string{"leader", "worker", "reviewer"}}}
	base.Diagram.Edges = []FlowEdge{{"home", "general"}}
	base.JoinRules = []FlowJoinRule{{"home", []string{"leader"}}, {"general", []string{"leader", "worker", "reviewer"}}, {"task", []string{"leader", "worker", "reviewer"}}}
	base.SpawnRules = []FlowSpawnRule{{"leader", []string{"worker", "reviewer"}}}
	base.AutoJoinRoles = []string{"leader"}
	base.CoreRoles = []string{"leader"}
	base.TaskParentKey = "home"
	base.GrantRoles = []string{"leader"}
	base.RoleTextOverrides = map[string]string{}
	routed := TeamFlowTemplate{Key: "leader-dispatcher", Label: "Leader and dispatcher", Description: "The leader writes specs; the dispatcher runs workers and reviews."}
	routed.Rooms = []FlowRoom{{Key: "home", Title: "Home", Kind: "home"}, {Key: "routing", Title: "routing", Kind: "task", Restricted: true}, {Key: "general", Title: "general", Kind: "task"}}
	routed.Diagram.Rooms = []FlowDiagramRoom{{"home", "Home", []string{"leader"}}, {"routing", "Routing", []string{"leader", "dispatcher"}}, {"general", "General and tasks", []string{"dispatcher", "worker", "reviewer"}}}
	routed.Diagram.Edges = []FlowEdge{{"home", "routing"}, {"routing", "general"}}
	routed.JoinRules = []FlowJoinRule{{"home", []string{"leader"}}, {"routing", []string{"leader", "dispatcher"}}, {"general", []string{"dispatcher", "worker", "reviewer"}}, {"task", []string{"dispatcher", "worker", "reviewer"}}}
	routed.SpawnRules = []FlowSpawnRule{{"leader", []string{"dispatcher"}}, {"dispatcher", []string{"worker", "reviewer"}}}
	routed.AutoJoinRoles = []string{"leader"}
	routed.CoreRoles = []string{"leader", "dispatcher"}
	routed.TaskParentKey = "general"
	routed.GrantRoles = []string{"leader", "dispatcher"}
	routed.RoleTextOverrides = map[string]string{
		"leader":     "You are the project leader. Talk with the human and write specifications with goal, acceptance criteria, constraints, base, priority, and the team: how many workers and whether a reviewer is needed. Hand each specification to the dispatcher in routing. Do not spawn or manage workers and reviewers. Decide questions escalated by the dispatcher; report the outcome to the human in the home room.\n\n" + leadSafety + "\n\n" + roleCommunication,
		"dispatcher": dispatcherRoleBody + " Spawn only the workers and reviewers called for by the specification or the leader. Add a reviewer only when the specification or leader asks for one; if the specification does not say, ask the leader in routing before spawning a reviewer.\n\n" + roleSafety + "\n\n" + roleCommunication,
	}
	custom := TeamFlowTemplate{Key: "custom", Label: "Custom", Description: "Keep the current rooms and role permissions."}
	custom.Rooms = []FlowRoom{}
	custom.Diagram.Rooms = []FlowDiagramRoom{}
	custom.Diagram.Edges = []FlowEdge{}
	custom.JoinRules = []FlowJoinRule{}
	custom.SpawnRules = []FlowSpawnRule{}
	custom.AutoJoinRoles = []string{"leader"}
	custom.CoreRoles = []string{"leader"}
	custom.TaskParentKey = "home"
	custom.GrantRoles = []string{"leader", "dispatcher"}
	custom.RoleTextOverrides = map[string]string{}
	return []TeamFlowTemplate{base, routed, custom}
}

func teamFlowTemplate(key string) (TeamFlowTemplate, bool) {
	for _, t := range TeamFlowTemplates() {
		if t.Key == key {
			return t, true
		}
	}
	return TeamFlowTemplate{}, false
}

func flowCoreRole(flowKey, roleKey string, lead bool) bool {
	if flowKey == "" || flowKey == "custom" {
		return lead
	}
	flow, ok := teamFlowTemplate(flowKey)
	if !ok {
		return lead
	}
	return flowAllows(flow.CoreRoles, roleKey)
}

// meteredLiveAgents counts only roles that consume the project's worker and
// reviewer slots. The caller supplies the flow read in the same transaction.
func meteredLiveAgents(ctx context.Context, db rowQuerier, projectID, excludeID, flowKey string) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT r.role_key,r.cap_is_lead FROM agent a JOIN role r ON r.id=a.role_id
		WHERE a.project_id=? AND a.id<>? AND a.revoked_at IS NULL
		AND (a.desired!='stopped' OR a.observed!='stopped')`, projectID, excludeID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var roleKey string
		var lead bool
		if err := rows.Scan(&roleKey, &lead); err != nil {
			return 0, err
		}
		if !flowCoreRole(flowKey, roleKey, lead) {
			count++
		}
	}
	return count, rows.Err()
}

func flowAllows(roles []string, role string) bool {
	for _, allowed := range roles {
		if allowed == role || allowed == "reviewer" && (role == "sec-reviewer" || role == "test-reviewer" || role == "ux-reviewer") {
			return true
		}
	}
	return false
}

func flowActorRole(t TeamFlowTemplate, role string) bool {
	for _, rule := range t.SpawnRules {
		if rule.ActorRole == role {
			return true
		}
	}
	return false
}

func flowManages(key, role string, lead, spawn bool) bool {
	if key == "" || key == "custom" {
		return lead
	}
	t, ok := teamFlowTemplate(key)
	return ok && spawn && flowActorRole(t, role)
}

func (tx *Tx) flowAutoJoin(projectID, role string, lead bool) (bool, error) {
	var key string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT team_flow_key FROM project WHERE id=?`, projectID).Scan(&key); err != nil {
		return false, err
	}
	if key == "" || key == "custom" {
		return lead, nil
	}
	t, ok := teamFlowTemplate(key)
	return ok && flowAllows(t.AutoJoinRoles, role), nil
}

func (tx *Tx) flowActorMayTarget(projectID, actorID, targetRole string) error {
	var key, actorRole string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT p.team_flow_key,r.role_key FROM project p JOIN agent a ON a.project_id=p.id JOIN role r ON r.id=a.role_id WHERE p.id=? AND a.id=?`, projectID, actorID).Scan(&key, &actorRole); err != nil {
		return err
	}
	if key == "" || key == "custom" {
		return nil
	}
	t, ok := teamFlowTemplate(key)
	if !ok {
		return ErrTeamFlowRule
	}
	for _, rule := range t.SpawnRules {
		if rule.ActorRole == actorRole && flowAllows(rule.AllowedRoles, targetRole) {
			return nil
		}
	}
	return ErrTeamFlowRule
}

func flowRoomKind(kind, key, title string) string {
	if kind == "home" {
		return "home"
	}
	if key == "routing" {
		return "routing"
	}
	if key == "general" || title == GeneralRoomTitle {
		return "general"
	}
	return "task"
}

func (tx *Tx) flowJoinAllowed(projectID, roomKind, role string) (bool, error) {
	var key string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT team_flow_key FROM project WHERE id=?`, projectID).Scan(&key); err != nil {
		return false, err
	}
	t, ok := teamFlowTemplate(key)
	if !ok || key == "custom" || key == "" {
		return true, nil
	}
	for _, rule := range t.JoinRules {
		if rule.RoomKind == roomKind {
			return flowAllows(rule.AllowedRoles, role), nil
		}
	}
	return false, nil
}

func (s *Store) TeamFlowState(ctx context.Context, projectID, userID string) (TeamFlowState, error) {
	if err := s.ProjectAccess(ctx, userID, projectID); err != nil {
		return TeamFlowState{}, err
	}
	var out TeamFlowState
	err := s.db.QueryRowContext(ctx, `SELECT team_flow_key,team_flow_rev,team_flow_review_pending FROM project WHERE id=? AND system=0`, projectID).Scan(&out.Key, &out.Rev, &out.ReviewPending)
	if err != nil {
		return out, err
	}
	if out.Key != "" {
		out.Template, _ = teamFlowTemplate(out.Key)
	}
	out.Conflicts, err = flowConflicts(ctx, s.db, projectID, out.Template)
	if err != nil {
		return out, err
	}
	return out, nil
}

func (s *Store) PreviewTeamFlow(ctx context.Context, projectID, userID, key string) (TeamFlowPreview, error) {
	if err := s.ProjectAccess(ctx, userID, projectID); err != nil {
		return TeamFlowPreview{}, err
	}
	t, ok := teamFlowTemplate(key)
	if !ok {
		return TeamFlowPreview{}, fmt.Errorf("unknown team flow %q", key)
	}
	out := TeamFlowPreview{Key: key, CreateRooms: []FlowRoom{}, PreservedRestrictedRooms: []FlowPreservedRestriction{}, RoleChanges: []any{}, Conflicts: []FlowConflict{}}
	conflicts, err := flowConflicts(ctx, s.db, projectID, t)
	if err != nil {
		return out, err
	}
	out.Conflicts = conflicts
	rows, err := s.db.QueryContext(ctx, `SELECT id,title FROM room WHERE project_id=? AND restricted=1 ORDER BY created_at,id`, projectID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var room FlowPreservedRestriction
		if err := rows.Scan(&room.RoomID, &room.Title); err != nil {
			rows.Close()
			return out, err
		}
		out.PreservedRestrictedRooms = append(out.PreservedRestrictedRooms, room)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for _, room := range t.Rooms {
		if room.Key == "home" {
			continue
		}
		var count int
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM room WHERE project_id=? AND (team_flow_room_key=? OR title=?) AND kind<>'dm'`, projectID, room.Key, room.Title).Scan(&count)
		if err != nil {
			return out, err
		}
		if count == 0 {
			out.CreateRooms = append(out.CreateRooms, room)
		}
	}
	return out, nil
}

func flowConflicts(ctx context.Context, db rowQuerier, projectID string, t TeamFlowTemplate) ([]FlowConflict, error) {
	out := []FlowConflict{}
	if t.Key == "custom" || t.Key == "" {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT a.id,ro.role_key,r.kind,r.team_flow_room_key,r.title FROM agent a
		JOIN role ro ON ro.id=a.role_id JOIN room_member m ON m.member_kind='agent' AND m.member_id=a.id AND m.left_at IS NULL
		JOIN room r ON r.id=m.room_id WHERE a.project_id=? AND r.kind<>'dm' AND a.revoked_at IS NULL`, projectID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, role, kind, key, title string
		if err := rows.Scan(&id, &role, &kind, &key, &title); err != nil {
			return out, err
		}
		selector := flowRoomKind(kind, key, title)
		allowed := false
		for _, rule := range t.JoinRules {
			if rule.RoomKind == selector {
				allowed = flowAllows(rule.AllowedRoles, role)
				break
			}
		}
		if !allowed {
			out = append(out, FlowConflict{AgentID: id, Reason: "role " + role + " is not allowed in " + title, SuggestedAction: "Move the agent to an allowed room"})
		}
	}
	return out, rows.Err()
}

func (s *Store) ApplyTeamFlow(ctx context.Context, projectID, userID, key string, rev int64) (TeamFlowApplied, error) {
	if _, ok := teamFlowTemplate(key); !ok {
		return TeamFlowApplied{}, fmt.Errorf("unknown team flow %q", key)
	}
	role, err := s.UserProjectRole(ctx, userID, projectID)
	if err != nil {
		return TeamFlowApplied{}, err
	}
	if role == "viewer" {
		return TeamFlowApplied{}, ErrForbidden
	}
	var out TeamFlowApplied
	err = s.Tx(ctx, func(tx *Tx) error {
		var err error
		out, err = tx.applyTeamFlow(projectID, userID, key, rev)
		return err
	})
	return out, err
}

// applyTeamFlow runs inside the caller's transaction so initial setup can
// commit the flow, defaults, settings, and agents together.
func (tx *Tx) applyTeamFlow(projectID, userID, key string, rev int64) (TeamFlowApplied, error) {
	var out TeamFlowApplied
	err := func() error {
		var current string
		var currentRev int64
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT team_flow_key,team_flow_rev FROM project WHERE id=? AND system=0`, projectID).Scan(&current, &currentRev); err != nil {
			return err
		}
		if currentRev != rev {
			return ErrTeamFlowRev
		}
		out = TeamFlowApplied{Key: key, Rev: rev + 1, CreatedRooms: []Room{}, Conflicts: []FlowConflict{}}
		if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE project SET team_flow_key=?,team_flow_rev=?,team_flow_review_pending=0 WHERE id=?`, key, out.Rev, projectID); err != nil {
			return err
		}
		t, _ := teamFlowTemplate(key)
		if conflicts, err := flowConflicts(tx.ctx, tx.tx, projectID, t); err == nil {
			out.Conflicts = conflicts
		} else {
			return err
		}
		for _, spec := range t.Rooms {
			if spec.Key == "home" {
				continue
			}
			var id string
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND kind<>'dm' AND (team_flow_room_key=? OR title=?) LIMIT 1`, projectID, spec.Key, spec.Title).Scan(&id)
			if err == nil {
				if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE room SET team_flow_room_key=?,restricted=restricted OR ? WHERE id=?`, spec.Key, spec.Restricted, id); err != nil {
					return err
				}
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			room, err := tx.CreateRoom(Room{ProjectID: projectID, Kind: spec.Kind, Title: spec.Title, CreatedBy: userID})
			if err != nil {
				return err
			}
			if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE room SET team_flow_room_key=?,restricted=? WHERE id=?`, spec.Key, spec.Restricted, room.ID); err != nil {
				return err
			}
			room.TeamFlowRoomKey = spec.Key
			out.CreatedRooms = append(out.CreatedRooms, room)
		}
		if key != "custom" {
			rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id,kind,team_flow_room_key,title FROM room WHERE project_id=? AND kind<>'dm'`, projectID)
			if err != nil {
				return err
			}
			type roomInfo struct{ id, kind, key, title string }
			var rooms []roomInfo
			for rows.Next() {
				var info roomInfo
				if err := rows.Scan(&info.id, &info.kind, &info.key, &info.title); err != nil {
					rows.Close()
					return err
				}
				rooms = append(rooms, info)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, room := range rooms {
				selector := flowRoomKind(room.kind, room.key, room.title)
				var allowedRoles []string
				for _, rule := range t.JoinRules {
					if rule.RoomKind == selector {
						allowedRoles = rule.AllowedRoles
						break
					}
				}
				members, err := tx.tx.QueryContext(tx.ctx, `SELECT m.member_id,ro.role_key FROM room_member m JOIN agent a ON a.id=m.member_id JOIN role ro ON ro.id=a.role_id WHERE m.room_id=? AND m.member_kind='agent' AND m.left_at IS NULL`, room.id)
				if err != nil {
					return err
				}
				var remove []string
				for members.Next() {
					var id, role string
					if err := members.Scan(&id, &role); err != nil {
						members.Close()
						return err
					}
					if !flowAllows(allowedRoles, role) {
						remove = append(remove, id)
					}
				}
				err = members.Err()
				members.Close()
				if err != nil {
					return err
				}
				for _, id := range remove {
					if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE room_member SET left_at=? WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, timestamp(), room.id, id); err != nil {
						return err
					}
				}
				if err := tx.joinLeadsToRoom(room.id, projectID); err != nil {
					return err
				}
			}
		}
		_, err := tx.AppendEvent(Event{Kind: "project_updated", ProjectID: projectID, PayloadJSON: payloadJSON(map[string]string{"project_id": projectID, "actor_id": userID})})
		return err
	}()
	return out, err
}

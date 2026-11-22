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
	Key            string `json:"key"`
	Label          string `json:"label"`
	Description    string `json:"description"`
	Recommendation string `json:"recommendation"`
	Diagram        struct {
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
	Slug            string `json:"slug"`
	RoleKey         string `json:"role_key"`
	RoleLabel       string `json:"role_label"`
	RoomID          string `json:"room_id"`
	RoomTitle       string `json:"room_title"`
	Reason          string `json:"reason"`
	SuggestedAction string `json:"suggested_action"`
}
type FlowRemoval struct {
	AgentID string `json:"agent_id"`
	RoomID  string `json:"room_id"`
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
var ErrTeamFlowSelection = errors.New("selected room membership is no longer a conflict")

func TeamFlowTemplates() []TeamFlowTemplate {
	base := TeamFlowTemplate{Key: "leader-workers", Label: "Leader and workers", Description: "The leader plans with you and manages workers and reviewers.", Recommendation: "Recommended for small to medium projects."}
	base.Rooms = []FlowRoom{{Key: "home", Title: "Home", Kind: "home"}, {Key: "general", Title: "general", Kind: "task"}}
	base.Diagram.Rooms = []FlowDiagramRoom{{"home", "Home", []string{"leader"}}, {"general", "General and tasks", []string{"leader", "worker", "reviewer"}}}
	base.Diagram.Edges = []FlowEdge{{"home", "general"}}
	base.JoinRules = []FlowJoinRule{{"home", []string{"leader"}}, {"general", []string{"leader", "worker", "reviewer"}}, {"task", []string{"leader", "worker", "reviewer"}}}
	base.SpawnRules = []FlowSpawnRule{{"leader", []string{"worker", "reviewer"}}}
	base.AutoJoinRoles = []string{"leader"}
	base.CoreRoles = []string{"leader"}
	base.TaskParentKey = "home"
	base.GrantRoles = []string{"leader"}
	base.RoleTextOverrides = map[string]string{
		"leader": leadRoleBody + "\n\nGeneral is the shared room for project announcements and coordination. Keep task work in task rooms.",
	}
	routed := TeamFlowTemplate{Key: "leader-dispatcher", Label: "Leader and dispatcher", Description: "The leader writes specs; the dispatcher runs workers and reviews.", Recommendation: "Recommended for large projects; the dispatcher handles routing so the leader's context stays lean."}
	routed.Rooms = []FlowRoom{{Key: "home", Title: "Home", Kind: "home"}, {Key: "routing", Title: "routing", Kind: "task", Restricted: true}, {Key: "general", Title: "general", Kind: "task"}}
	routed.Diagram.Rooms = []FlowDiagramRoom{{"home", "Home", []string{"leader"}}, {"routing", "Routing", []string{"leader", "dispatcher"}}, {"general", "General and tasks", []string{"leader", "dispatcher", "worker", "reviewer"}}}
	routed.Diagram.Edges = []FlowEdge{{"home", "routing"}, {"routing", "general"}}
	// A researcher (read-only, on demand) may be spawned by the leader and by the dispatcher, into general (where the leader can read its report)
	// or a task room.
	routed.JoinRules = []FlowJoinRule{{"home", []string{"leader"}}, {"routing", []string{"leader", "dispatcher"}}, {"general", []string{"leader", "dispatcher", "worker", "reviewer", "researcher"}}, {"task", []string{"dispatcher", "worker", "reviewer", "researcher"}}}
	routed.SpawnRules = []FlowSpawnRule{{"leader", []string{"dispatcher", "researcher"}}, {"dispatcher", []string{"worker", "reviewer", "researcher"}}}
	routed.AutoJoinRoles = []string{"leader", "dispatcher"}
	routed.CoreRoles = []string{"leader", "dispatcher"}
	routed.TaskParentKey = "home"
	routed.GrantRoles = []string{"leader", "dispatcher"}
	routed.RoleTextOverrides = map[string]string{
		"leader":     "You are the project leader. Talk with the human and write specifications with goal, acceptance criteria, constraints, base, priority, and the team: how many workers and whether a reviewer is needed. Hand each specification to the dispatcher in routing. Do not spawn or manage workers and reviewers. Decide questions escalated by the dispatcher; report the outcome to the human in the home room. General is the shared room for project announcements and coordination; specs go through routing and task work stays in task rooms.\n\n" + leadSafety + "\n\n" + roleCommunication,
		"dispatcher": dispatcherRoleBody + " Spawn only the workers and reviewers called for by the specification or the leader. Add a reviewer only when the specification or leader asks for one; if the specification does not say, ask the leader in routing before spawning a reviewer. General is the shared room for project announcements and coordination; specs go through routing and task work stays in task rooms.\n\n" + roleSafety + "\n\n" + roleCommunication,
	}
	custom := TeamFlowTemplate{Key: "custom", Label: "Custom", Description: "Keep the current rooms and role permissions.", Recommendation: "For setting up rooms and roles yourself."}
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
	advised := TeamFlowTemplate{Key: "leader-advisor", Label: "Leader and advisor", Description: "A fast leader runs the work and consults a stronger advisor for plans and decisions.", Recommendation: "Recommended when a fast, cheaper leader should get a stronger model's judgment on key decisions."}
	advised.Rooms = []FlowRoom{{Key: "home", Title: "Home", Kind: "home"}, {Key: "advice", Title: "advice", Kind: "task", Restricted: true}, {Key: "general", Title: "general", Kind: "task"}}
	advised.Diagram.Rooms = []FlowDiagramRoom{{"home", "Home", []string{"leader", "advisor"}}, {"advice", "Advice", []string{"leader", "advisor"}}, {"general", "General and tasks", []string{"leader", "advisor", "worker", "reviewer"}}}
	advised.Diagram.Edges = []FlowEdge{{"home", "advice"}, {"advice", "general"}}
	advised.JoinRules = []FlowJoinRule{{"home", []string{"leader", "advisor"}}, {"advice", []string{"leader", "advisor"}}, {"general", []string{"leader", "advisor", "worker", "reviewer"}}, {"task", []string{"leader", "worker", "reviewer"}}}
	advised.SpawnRules = []FlowSpawnRule{{"leader", []string{"worker", "reviewer"}}}
	advised.AutoJoinRoles = []string{"leader", "advisor"}
	advised.CoreRoles = []string{"leader", "advisor"}
	advised.TaskParentKey = "general"
	advised.GrantRoles = []string{"leader"}
	advised.RoleTextOverrides = map[string]string{
		"leader":  leadRoleBody + "\n\nConsult the advisor in advice before writing a plan for a non-trivial task, making a consequential decision about behavior, data, spend, or irreversible operations, after two failed attempts at the same problem, and before handing a large change to the human. Each consult must state the goal, current state, options considered, a concrete question, and relevant message IDs. Decide cosmetic and routine matters yourself. Report the advisor's input to the human only when it changed the plan. Ask for a review of the final handoff only when needed. General is the shared project room; keep task work in task rooms.",
		"advisor": advisorRoleBody + "\n\n" + roleSafety + "\n\n" + roleCommunication,
	}
	return []TeamFlowTemplate{base, routed, advised, custom}
}

func teamFlowTemplate(key string) (TeamFlowTemplate, bool) {
	for _, t := range TeamFlowTemplates() {
		if t.Key == key {
			return t, true
		}
	}
	return TeamFlowTemplate{}, false
}

func flowCoreRole(flowKey, roleKey string, lead bool, cardinality string) bool {
	if flowKey == "" || flowKey == "custom" {
		return lead && cardinality == "singleton"
	}
	flow, ok := teamFlowTemplate(flowKey)
	if !ok {
		return lead && cardinality == "singleton"
	}
	return flowAllows(flow.CoreRoles, roleKey)
}

// meteredLiveAgents counts only roles that consume the project's worker and
// reviewer slots. The caller supplies the flow read in the same transaction.
func meteredLiveAgents(ctx context.Context, db rowQuerier, projectID, excludeID, flowKey string) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT r.role_key,r.cap_is_lead,r.cardinality FROM agent a JOIN role r ON r.id=a.role_id
		WHERE a.project_id=? AND a.id<>? AND a.revoked_at IS NULL
		AND (a.desired!='stopped' OR a.observed!='stopped')`, projectID, excludeID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var roleKey, cardinality string
		var lead bool
		if err := rows.Scan(&roleKey, &lead, &cardinality); err != nil {
			return 0, err
		}
		if !flowCoreRole(flowKey, roleKey, lead, cardinality) {
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

func (tx *Tx) flowAutoJoin(projectID, role string, lead bool, roomKind, roomKey string) (bool, error) {
	var key string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT team_flow_key FROM project WHERE id=?`, projectID).Scan(&key); err != nil {
		return false, err
	}
	if key == "" || key == "custom" {
		return lead, nil
	}
	if key == "leader-dispatcher" && role == "dispatcher" {
		return roomKind == "routing" || roomKey == "general", nil
	}
	if key == "leader-advisor" {
		if role == "leader" {
			return roomKind == "home" || roomKey == "advice" || roomKey == "general", nil
		}
		if role == "advisor" {
			return roomKey == "advice" || roomKey == "general", nil
		}
		return false, nil
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

// flowActorMayTargetAgent is flowActorMayTarget for an existing agent. An
// actor that may manage agents under the current flow also keeps authority
// over agents it spawned and agents whose home room it created, even when the
// flow's spawn rules would not let it target that role. This keeps a leader's
// pre-switch workers stoppable after a switch to the dispatcher flow, and lets
// a coordinator wind down the task rooms it opened.
func (tx *Tx) flowActorMayTargetAgent(projectID, actorID, targetID, targetRole string) error {
	err := tx.flowActorMayTarget(projectID, actorID, targetRole)
	if !errors.Is(err, ErrTeamFlowRule) {
		return err
	}
	var own bool
	if qerr := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM agent a LEFT JOIN room r ON r.id=a.home_room_id
		WHERE a.id=? AND a.project_id=? AND (a.created_by=? OR r.created_by=?))`, targetID, projectID, actorID, actorID).Scan(&own); qerr != nil {
		return qerr
	}
	if own {
		return nil
	}
	return err
}

func flowRoomKind(kind, key, title string) string {
	if kind == "home" {
		return "home"
	}
	if key == "routing" {
		return "routing"
	}
	if key == "advice" {
		return "advice"
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
	role, err := s.UserProjectRole(ctx, userID, projectID)
	if err != nil {
		return TeamFlowState{}, err
	}
	var out TeamFlowState
	err = s.db.QueryRowContext(ctx, `SELECT team_flow_key,team_flow_rev,team_flow_review_pending FROM project WHERE id=? AND system=0`, projectID).Scan(&out.Key, &out.Rev, &out.ReviewPending)
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
	if role == "viewer" {
		visible := out.Conflicts[:0]
		for _, conflict := range out.Conflicts {
			if err := s.UserRoomAccess(ctx, userID, conflict.RoomID); err == nil {
				visible = append(visible, conflict)
			} else if !errors.Is(err, ErrForbidden) {
				return out, err
			}
		}
		out.Conflicts = visible
	}
	return out, nil
}

func (s *Store) PreviewTeamFlow(ctx context.Context, projectID, userID, key string) (TeamFlowPreview, error) {
	role, err := s.UserProjectRole(ctx, userID, projectID)
	if err != nil {
		return TeamFlowPreview{}, err
	}
	if role == "viewer" {
		return TeamFlowPreview{}, ErrForbidden
	}
	if archived, err := s.ArchivedScope(ctx, userID, "project", projectID); err != nil {
		return TeamFlowPreview{}, err
	} else if archived {
		return TeamFlowPreview{}, ErrProjectArchived
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
		id, err := flowReuseRoomID(ctx, s.db, projectID, room)
		if err != nil {
			return out, err
		}
		if id == "" {
			out.CreateRooms = append(out.CreateRooms, room)
		}
	}
	return out, nil
}

// flowReuseRoomID chooses the same existing room for preview, conflicts, and
// apply. An explicit template key wins over a coincidental title match.
func flowReuseRoomID(ctx context.Context, db rowQuerier, projectID string, spec FlowRoom) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT id FROM room WHERE project_id=? AND lifecycle='open' AND kind<>'dm'
		AND (team_flow_room_key=? OR title=?)
		ORDER BY CASE WHEN team_flow_room_key=? THEN 0 ELSE 1 END,created_at,id LIMIT 1`,
		projectID, spec.Key, spec.Title, spec.Key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func flowConflicts(ctx context.Context, db rowQuerier, projectID string, t TeamFlowTemplate) ([]FlowConflict, error) {
	out := []FlowConflict{}
	if t.Key == "custom" || t.Key == "" {
		return out, nil
	}
	selectedRooms := make(map[string]string, len(t.Rooms))
	for _, spec := range t.Rooms {
		if spec.Key == "home" {
			continue
		}
		id, err := flowReuseRoomID(ctx, db, projectID, spec)
		if err != nil {
			return out, err
		}
		selectedRooms[spec.Key] = id
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT a.id,a.slug,ro.role_key,ro.label,r.id,r.kind,r.team_flow_room_key,r.title FROM agent a
		JOIN role ro ON ro.id=a.role_id JOIN room_member m ON m.member_kind='agent' AND m.member_id=a.id AND m.left_at IS NULL
		JOIN room r ON r.id=m.room_id WHERE a.project_id=? AND r.lifecycle='open' AND r.kind<>'dm' AND a.revoked_at IS NULL ORDER BY a.slug,r.title,r.id`, projectID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, slug, role, roleLabel, roomID, kind, key, title string
		if err := rows.Scan(&id, &slug, &role, &roleLabel, &roomID, &kind, &key, &title); err != nil {
			return out, err
		}
		selector := flowRoomKind(kind, key, title)
		if kind != "home" {
			for _, spec := range t.Rooms {
				if spec.Key != "home" && roomID == selectedRooms[spec.Key] {
					selector = spec.Key
					break
				}
			}
		}
		allowed := false
		for _, rule := range t.JoinRules {
			if rule.RoomKind == selector {
				allowed = flowAllows(rule.AllowedRoles, role)
				break
			}
		}
		if !allowed {
			out = append(out, FlowConflict{AgentID: id, Slug: slug, RoleKey: role, RoleLabel: roleLabel, RoomID: roomID, RoomTitle: title, Reason: "role " + role + " is not allowed in " + title, SuggestedAction: "Remove from this room when applying"})
		}
	}
	return out, rows.Err()
}

func (s *Store) ApplyTeamFlow(ctx context.Context, projectID, userID, key string, rev int64, removals []FlowRemoval) (TeamFlowApplied, error) {
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
		var current string
		_ = tx.tx.QueryRowContext(tx.ctx, `SELECT team_flow_key FROM project WHERE id=?`, projectID).Scan(&current)
		out, err = tx.applyTeamFlow(projectID, userID, key, rev, removals)
		if err != nil || current == key {
			return err
		}
		rooms := []string{}
		for _, room := range out.CreatedRooms {
			rooms = append(rooms, room.Title)
		}
		return tx.postFlowNotice(projectID, flowChangedBody(current, key, nil, nil, rooms))
	})
	return out, err
}

// applyTeamFlow runs inside the caller's transaction so initial setup can
// commit the flow, defaults, settings, and agents together.
func (tx *Tx) applyTeamFlow(projectID, userID, key string, rev int64, removals []FlowRemoval) (TeamFlowApplied, error) {
	var out TeamFlowApplied
	err := func() error {
		if err := tx.refuseArchived(projectID); err != nil {
			return err
		}
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
		conflicts, err := flowConflicts(tx.ctx, tx.tx, projectID, t)
		if err != nil {
			return err
		}
		available := make(map[FlowRemoval]bool, len(conflicts))
		for _, conflict := range conflicts {
			available[FlowRemoval{AgentID: conflict.AgentID, RoomID: conflict.RoomID}] = true
		}
		selected := make(map[FlowRemoval]bool, len(removals))
		for _, removal := range removals {
			if !available[removal] || selected[removal] {
				return ErrTeamFlowSelection
			}
			selected[removal] = true
			if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE room_member SET left_at=? WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, timestamp(), removal.RoomID, removal.AgentID); err != nil {
				return err
			}
		}
		for _, conflict := range conflicts {
			if !selected[FlowRemoval{AgentID: conflict.AgentID, RoomID: conflict.RoomID}] {
				out.Conflicts = append(out.Conflicts, conflict)
			}
		}
		for _, spec := range t.Rooms {
			if spec.Key == "home" {
				continue
			}
			id, err := flowReuseRoomID(tx.ctx, tx.tx, projectID, spec)
			if err != nil {
				return err
			}
			if id != "" {
				if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE room SET team_flow_room_key=?,restricted=restricted OR ?,purpose=CASE WHEN ? THEN 'general' ELSE purpose END WHERE id=?`, spec.Key, spec.Restricted, flowRoomIsGeneral(spec.Key), id); err != nil {
					return err
				}
				continue
			}
			var homeID string
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND kind='home' ORDER BY created_at,id LIMIT 1`, projectID).Scan(&homeID); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			purpose := RoomPurposeTask
			if flowRoomIsGeneral(spec.Key) {
				purpose = RoomPurposeGeneral
			}
			room, err := tx.CreateRoom(Room{ProjectID: projectID, ParentID: homeID, Kind: spec.Kind, Title: spec.Title, CreatedBy: userID, Purpose: purpose})
			if err != nil {
				return err
			}
			if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE room SET team_flow_room_key=?,restricted=? WHERE id=?`, spec.Key, spec.Restricted, room.ID); err != nil {
				return err
			}
			room.TeamFlowRoomKey = spec.Key
			room.Restricted = spec.Restricted
			out.CreatedRooms = append(out.CreatedRooms, room)
		}
		if key != "custom" {
			rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND kind<>'dm'`, projectID)
			if err != nil {
				return err
			}
			var rooms []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				rooms = append(rooms, id)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, roomID := range rooms {
				if err := tx.joinLeadsToRoom(roomID, projectID); err != nil {
					return err
				}
			}
		}
		_, err = tx.AppendEvent(Event{Kind: "project_updated", ProjectID: projectID, PayloadJSON: payloadJSON(map[string]string{"project_id": projectID, "actor_id": userID})})
		return err
	}()
	return out, err
}

// flowRoomIsGeneral says whether a team flow's room key names a project-wide or coordination room (general, routing or advice), which has
// the general purpose rather than being a task room.
func flowRoomIsGeneral(key string) bool {
	return key == "general" || key == "routing" || key == "advice"
}

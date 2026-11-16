package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

type TeamFlowMigrationChange struct {
	AgentID string          `json:"agent_id"`
	Target  TeamSetupTarget `json:"target"`
}

type TeamFlowMigrationInput struct {
	FlowKey           string                          `json:"flow_key"`
	FlowRev           int64                           `json:"flow_rev"`
	Leader            *TeamSetupTarget                `json:"leader,omitempty"`
	Dispatcher        *TeamSetupTarget                `json:"dispatcher,omitempty"`
	Advisor           *TeamSetupTarget                `json:"advisor,omitempty"`
	Changes           []TeamFlowMigrationChange       `json:"changes,omitempty"`
	RoleDefaults      map[string]TeamSetupRoleDefault `json:"role_defaults,omitempty"`
	Settings          TeamSetupSettings               `json:"settings,omitempty"`
	RemoveMemberships []FlowRemoval                   `json:"remove_memberships,omitempty"`
}

type TeamFlowMigrationAgent struct {
	AgentID       string `json:"agent_id"`
	Slug          string `json:"slug"`
	MachineID     string `json:"machine_id"`
	Tool          string `json:"tool"`
	Variant       string `json:"variant"`
	LauncherID    string `json:"launcher_id"`
	Model         string `json:"model"`
	LauncherLabel string `json:"launcher_label"`
	State         string `json:"state"`
}

type TeamFlowMigrationRole struct {
	RoleKey string                   `json:"role_key"`
	Status  string                   `json:"status"`
	Agents  []TeamFlowMigrationAgent `json:"agents"`
}

type TeamFlowMigrationPreview struct {
	FromKey      string                  `json:"from_key"`
	ToKey        string                  `json:"to_key"`
	Roles        []TeamFlowMigrationRole `json:"roles"`
	CreatedRooms []FlowRoom              `json:"created_rooms"`
	Conflicts    []FlowConflict          `json:"conflicts"`
	CanApply     bool                    `json:"can_apply"`
}

type TeamFlowMigrationAgentProgress struct {
	AgentID string `json:"agent_id"`
	Slug    string `json:"slug"`
	RoleKey string `json:"role_key"`
	Action  string `json:"action"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

type TeamFlowMigrationProgress struct {
	ID           string                           `json:"id"`
	State        string                           `json:"state"`
	Error        string                           `json:"error,omitempty"`
	CanCancel    bool                             `json:"can_cancel"`
	StalledSince string                           `json:"stalled_since,omitempty"`
	Agents       []TeamFlowMigrationAgentProgress `json:"agents"`
}

type flowMigrationResolvedTarget struct {
	MachineID  string `json:"machine_id"`
	Name       string `json:"name,omitempty"`
	Tool       string `json:"tool"`
	Variant    string `json:"variant"`
	LauncherID string `json:"launcher_id,omitempty"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Path       string `json:"path"`
}

type flowMigrationStored struct {
	Input   TeamFlowMigrationInput                 `json:"input"`
	Targets map[string]flowMigrationResolvedTarget `json:"targets"`
}

func flowMigrationResolveTarget(target *TeamSetupTarget) flowMigrationResolvedTarget {
	return flowMigrationResolvedTarget{MachineID: target.MachineID, Name: target.Name, Tool: target.Tool, Variant: target.Variant,
		LauncherID: target.LauncherID, Model: target.Model, Provider: target.Provider, Path: target.Path}
}

var ErrTeamFlowMigration = errors.New("invalid team flow migration")
var ErrTeamFlowMigrationActive = errors.New("a team flow migration is already active")
var ErrTeamFlowMigrationFinal = errors.New("team flow migration is past the point of no return")

func (tx *Tx) AgentInActiveFlowMigration(agentID string) (bool, error) {
	var active bool
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM team_flow_migration_agent ma JOIN team_flow_migration m ON m.id=ma.migration_id
		WHERE ma.agent_id=? AND m.state IN ('draining','applying'))`, agentID).Scan(&active)
	return active, err
}

func flowMigrationTarget(in TeamFlowMigrationInput, role string) *TeamSetupTarget {
	switch role {
	case "leader":
		return in.Leader
	case "dispatcher":
		return in.Dispatcher
	case "advisor":
		return in.Advisor
	}
	return nil
}

func countLiveFlowAgents(agents []TeamFlowMigrationAgent) int {
	count := 0
	for _, agent := range agents {
		if agent.State != "stopped" {
			count++
		}
	}
	return count
}

func (s *Store) PreviewTeamFlowMigration(ctx context.Context, projectID, actorID string, in TeamFlowMigrationInput) (TeamFlowMigrationPreview, error) {
	var out TeamFlowMigrationPreview
	flow, err := s.PreviewTeamFlow(ctx, projectID, actorID, in.FlowKey)
	if err != nil {
		return out, err
	}
	out.ToKey, out.CreatedRooms, out.Conflicts, out.Roles, out.CanApply = in.FlowKey, flow.CreateRooms, flow.Conflicts, []TeamFlowMigrationRole{}, true
	err = s.Tx(ctx, func(tx *Tx) error {
		if err := tx.refuseArchived(projectID); err != nil {
			return err
		}
		var rev int64
		if err := tx.tx.QueryRowContext(ctx, `SELECT team_flow_key,team_flow_rev FROM project WHERE id=? AND system=0`, projectID).Scan(&out.FromKey, &rev); err != nil {
			return err
		}
		if rev != in.FlowRev {
			return ErrTeamFlowRev
		}
		if out.FromKey == "" || out.FromKey == in.FlowKey {
			return fmt.Errorf("%w: choose a different existing flow", ErrTeamFlowMigration)
		}
		if in.FlowKey == "custom" {
			return fmt.Errorf("%w: use the team-flow route for custom", ErrTeamFlowMigration)
		}
		if in.Settings.WorktreeMode != nil && *in.Settings.WorktreeMode != "auto" && *in.Settings.WorktreeMode != "off" ||
			in.Settings.MaxLiveAgents != nil && (*in.Settings.MaxLiveAgents < 1 || *in.Settings.MaxLiveAgents > 100) {
			return ErrTeamFlowMigration
		}
		var active bool
		if err := tx.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_flow_migration WHERE project_id=? AND state IN ('draining','applying'))`, projectID).Scan(&active); err != nil {
			return err
		}
		if active {
			return ErrTeamFlowMigrationActive
		}
		rows, err := tx.tx.QueryContext(ctx, `SELECT a.id,a.slug,r.role_key,a.machine_id,a.tool,a.variant,COALESCE(a.launcher_id,''),a.model,
			COALESCE(l.name,''),a.desired FROM agent a JOIN role r ON r.id=a.role_id
			LEFT JOIN machine_launcher l ON l.id=a.launcher_id
			WHERE a.project_id=? AND a.revoked_at IS NULL ORDER BY a.created_at,a.id`, projectID)
		if err != nil {
			return err
		}
		byRole := map[string][]TeamFlowMigrationAgent{}
		byID := map[string]TeamFlowMigrationAgent{}
		roleByID := map[string]string{}
		for rows.Next() {
			var a TeamFlowMigrationAgent
			var role string
			if err := rows.Scan(&a.AgentID, &a.Slug, &role, &a.MachineID, &a.Tool, &a.Variant, &a.LauncherID, &a.Model, &a.LauncherLabel, &a.State); err != nil {
				rows.Close()
				return err
			}
			stepRole := role
			if strings.HasSuffix(role, "-reviewer") {
				stepRole = "reviewer"
			}
			byRole[stepRole] = append(byRole[stepRole], a)
			byID[a.AgentID], roleByID[a.AgentID] = a, role
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		takenNames, err := tx.takenSlugs(projectID)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, change := range in.Changes {
			a, exists := byID[change.AgentID]
			if !exists || seen[change.AgentID] || a.State == "stopped" || change.Target.MachineID == "" || change.Target.Tool == "" || change.Target.Model == "" {
				return ErrTeamFlowMigration
			}
			seen[change.AgentID] = true
			if roleByID[change.AgentID] == "dispatcher" && in.FlowKey != "leader-dispatcher" || roleByID[change.AgentID] == "advisor" && in.FlowKey != "leader-advisor" {
				return ErrTeamFlowMigration
			}
		}
		targetRoles := map[string]bool{"leader": true, "worker": true, "reviewer": true}
		if in.FlowKey == "leader-dispatcher" {
			targetRoles["dispatcher"] = true
		}
		if in.FlowKey == "leader-advisor" {
			targetRoles["advisor"] = true
		}
		if in.FlowKey == "custom" {
			targetRoles["dispatcher"], targetRoles["advisor"] = true, true
		}
		for _, role := range []string{"leader", "dispatcher", "advisor", "worker", "reviewer"} {
			agents := byRole[role]
			if agents == nil {
				agents = []TeamFlowMigrationAgent{}
			}
			live := 0
			for _, agent := range agents {
				if agent.State != "stopped" {
					live++
				}
			}
			if role == "dispatcher" || role == "advisor" {
				if !targetRoles[role] && live == 0 {
					continue
				}
			}
			status := "kept"
			if !targetRoles[role] {
				status = "leaving"
			} else if live == 0 && (role == "leader" || role == "dispatcher" || role == "advisor") {
				status = "new"
			}
			if status == "new" {
				target := flowMigrationTarget(in, role)
				if target == nil {
					out.CanApply = false
				} else if target.MachineID == "" || target.Tool == "" || target.Model == "" {
					return fmt.Errorf("%w: incomplete %s target", ErrTeamFlowMigration, role)
				}
				if target != nil && target.Name != "" && !ValidAgentSlug(target.Name) {
					return ErrTeamSetupName
				}
				if target != nil && target.Name != "" && live == 0 && len(agents) > 0 && target.Name != agents[0].Slug {
					return fmt.Errorf("%w: a stopped core role keeps its name", ErrTeamFlowMigration)
				}
				if target != nil && target.Name != "" && len(agents) == 0 {
					if takenNames[target.Name] {
						return ErrSlugTaken
					}
					takenNames[target.Name] = true
				}
			} else if flowMigrationTarget(in, role) != nil {
				return fmt.Errorf("%w: %s target is for a new agent only", ErrTeamFlowMigration, role)
			}
			out.Roles = append(out.Roles, TeamFlowMigrationRole{RoleKey: role, Status: status, Agents: agents})
		}
		leaving := map[string]bool{}
		for _, role := range out.Roles {
			if role.Status == "leaving" {
				for _, agent := range role.Agents {
					leaving[agent.AgentID] = true
				}
			}
		}
		conflicts := make([]FlowConflict, 0, len(out.Conflicts))
		for _, conflict := range out.Conflicts {
			if !leaving[conflict.AgentID] {
				conflicts = append(conflicts, conflict)
			}
		}
		out.Conflicts = conflicts
		if countLiveFlowAgents(byRole["leader"]) > 1 || countLiveFlowAgents(byRole["dispatcher"]) > 1 || countLiveFlowAgents(byRole["advisor"]) > 1 {
			return ErrTeamFlowMigration
		}
		for role, config := range in.RoleDefaults {
			if role != "worker" && role != "reviewer" && role != "leader" && role != "advisor" || role == "advisor" && in.FlowKey != "leader-advisor" {
				return ErrTeamFlowMigration
			}
			mode := config.PickMode
			if mode == "" {
				mode = "in_turn"
			}
			if err := tx.validateSpawnDefaults(config.DefaultAgents, mode); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) TeamFlowMigrationProgress(ctx context.Context, projectID, actorID string) (TeamFlowMigrationProgress, error) {
	var out TeamFlowMigrationProgress
	role, err := s.UserProjectRole(ctx, actorID, projectID)
	if err != nil {
		return out, err
	}
	if role != "owner" && role != "operator" {
		return out, ErrForbidden
	}
	err = s.readDB.QueryRowContext(ctx, `SELECT id,state FROM team_flow_migration WHERE project_id=? ORDER BY created_at DESC,id DESC LIMIT 1`, projectID).Scan(&out.ID, &out.State)
	if err != nil {
		return out, err
	}
	return flowMigrationProgressByID(ctx, s.readDB, out.ID)
}

// TeamFlowMigrationProgressByID reads a keyed migration replay even when a
// later migration has become the project's latest one.
func (s *Store) TeamFlowMigrationProgressByID(ctx context.Context, projectID, actorID, id string) (TeamFlowMigrationProgress, error) {
	var out TeamFlowMigrationProgress
	role, err := s.UserProjectRole(ctx, actorID, projectID)
	if err != nil {
		return out, err
	}
	if role != "owner" && role != "operator" {
		return out, ErrForbidden
	}
	var actualProject string
	if err := s.readDB.QueryRowContext(ctx, `SELECT project_id FROM team_flow_migration WHERE id=?`, id).Scan(&actualProject); err != nil {
		return out, err
	}
	if actualProject != projectID {
		return out, sql.ErrNoRows
	}
	return flowMigrationProgressByID(ctx, s.readDB, id)
}

func flowMigrationProgressByID(ctx context.Context, db rowQuerier, id string) (TeamFlowMigrationProgress, error) {
	var out TeamFlowMigrationProgress
	var input string
	if err := db.QueryRowContext(ctx, `SELECT id,state,error,input_json FROM team_flow_migration WHERE id=?`, id).Scan(&out.ID, &out.State, &out.Error, &input); err != nil {
		return out, err
	}
	out.CanCancel, out.Agents = out.State == "draining", []TeamFlowMigrationAgentProgress{}
	if out.State == "draining" {
		var since sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT MIN(COALESCE(t.started_at,m.created_at)) FROM team_flow_migration m
			JOIN team_flow_migration_agent ma ON ma.migration_id=m.id JOIN turn t ON t.agent_id=ma.agent_id
			WHERE m.id=? AND `+openFlowTurnPredicate, id).Scan(&since); err != nil {
			return out, err
		}
		if since.Valid && since.String <= time.Now().UTC().Add(-15*time.Minute).Format(timestampLayout) {
			out.StalledSince = since.String
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT agent_id,slug,role_key,action,state,error FROM team_flow_migration_agent WHERE migration_id=? ORDER BY role_key,slug,agent_id`, out.ID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var item TeamFlowMigrationAgentProgress
		if err := rows.Scan(&item.AgentID, &item.Slug, &item.RoleKey, &item.Action, &item.State, &item.Error); err != nil {
			return out, err
		}
		out.Agents = append(out.Agents, item)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if out.State == "draining" {
		var saved flowMigrationStored
		if err := json.Unmarshal([]byte(input), &saved); err != nil {
			return out, err
		}
		for _, role := range []string{"leader", "dispatcher", "advisor"} {
			target, planned := saved.Targets[role]
			if !planned {
				continue
			}
			exists := false
			for _, agent := range out.Agents {
				if agent.RoleKey == role && (agent.Action == "spawn" || agent.Action == "reactivate") {
					exists = true
					break
				}
			}
			if exists {
				continue
			}
			name := target.Name
			if name == "" {
				name = "new " + role
			}
			out.Agents = append(out.Agents, TeamFlowMigrationAgentProgress{AgentID: "planned:" + role, Slug: name,
				RoleKey: role, Action: "spawn", State: "waiting"})
		}
	}
	return out, nil
}

func (tx *Tx) appendFlowMigrationEvent(projectID, id string) error {
	progress, err := flowMigrationProgressByID(tx.ctx, tx.tx, id)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(map[string]any{"migration": progress})
	if err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "team_flow_migration", ProjectID: projectID, PayloadJSON: string(encoded)})
	return err
}

func flowMigrationAction(role, toKey string, changed bool) string {
	if role == "dispatcher" && toKey != "leader-dispatcher" && toKey != "custom" || role == "advisor" && toKey != "leader-advisor" && toKey != "custom" {
		return "handoff_stop"
	}
	if changed {
		return "switch"
	}
	return "notice"
}

func flowMigrationAgentIDs(roles []TeamFlowMigrationRole) []string {
	var ids []string
	for _, role := range roles {
		for _, agent := range role.Agents {
			ids = append(ids, agent.AgentID)
		}
	}
	sort.Strings(ids)
	return ids
}

func flowMigrationJSON(in TeamFlowMigrationInput) (string, error) {
	data, err := json.Marshal(in)
	return string(data), err
}

// BeginTeamFlowMigration records a cancellable drain. Normal turns are held by
// PrepareDispatch while this row is active; running turns and dedicated
// handoff turns may finish before the atomic flow change.
func (tx *Tx) BeginTeamFlowMigration(projectID, actorID string, in TeamFlowMigrationInput) (TeamFlowMigrationProgress, error) {
	var out TeamFlowMigrationProgress
	if err := tx.authorizeDefinitionActor(projectID, actorID, false); err != nil {
		return out, err
	}
	if err := tx.refuseArchived(projectID); err != nil {
		return out, err
	}
	if _, ok := teamFlowTemplate(in.FlowKey); !ok {
		return out, ErrTeamFlowMigration
	}
	var from string
	var rev int64
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT team_flow_key,team_flow_rev FROM project WHERE id=? AND system=0`, projectID).Scan(&from, &rev); err != nil {
		return out, err
	}
	if rev != in.FlowRev {
		return out, ErrTeamFlowRev
	}
	if from == "" || from == in.FlowKey {
		return out, ErrTeamFlowMigration
	}
	if in.FlowKey == "custom" {
		return out, ErrTeamFlowMigration
	}
	if in.Settings.WorktreeMode != nil && *in.Settings.WorktreeMode != "auto" && *in.Settings.WorktreeMode != "off" ||
		in.Settings.MaxLiveAgents != nil && (*in.Settings.MaxLiveAgents < 1 || *in.Settings.MaxLiveAgents > 100) {
		return out, ErrTeamFlowMigration
	}
	for role, config := range in.RoleDefaults {
		if role != "worker" && role != "reviewer" && role != "leader" && role != "advisor" || role == "advisor" && in.FlowKey != "leader-advisor" {
			return out, ErrTeamFlowMigration
		}
		mode := config.PickMode
		if mode == "" {
			mode = "in_turn"
		}
		if err := tx.validateSpawnDefaults(config.DefaultAgents, mode); err != nil {
			return out, err
		}
	}
	var busy bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM team_flow_migration WHERE project_id=? AND state IN ('draining','applying'))
		OR EXISTS(SELECT 1 FROM agent_switch sw JOIN agent a ON a.id=sw.agent_id WHERE a.project_id=? AND sw.state IN ('draining','starting'))`, projectID, projectID).Scan(&busy); err != nil {
		return out, err
	}
	if busy {
		return out, ErrTeamFlowMigrationActive
	}
	resolved := flowMigrationStored{Input: in, Targets: map[string]flowMigrationResolvedTarget{}}
	for _, role := range []string{"leader", "dispatcher", "advisor"} {
		if target := flowMigrationTarget(in, role); target != nil {
			resolved.Targets[role] = flowMigrationResolveTarget(target)
		}
	}
	changed := map[string]bool{}
	for _, change := range in.Changes {
		if changed[change.AgentID] || change.AgentID == "" {
			return out, ErrTeamFlowMigration
		}
		changed[change.AgentID] = true
		resolved.Targets[change.AgentID] = flowMigrationResolveTarget(&change.Target)
	}
	stored, err := json.Marshal(resolved)
	if err != nil {
		return out, err
	}
	id, err := newID("tfm")
	if err != nil {
		return out, err
	}
	if _, err := tx.Exec(`INSERT INTO team_flow_migration(id,project_id,actor_id,from_key,to_key,flow_rev,state,input_json,created_at)
		VALUES (?,?,?,?,?,?,'draining',?,?)`, id, projectID, actorID, from, in.FlowKey, rev, string(stored), timestamp()); err != nil {
		return out, err
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT a.id,a.slug,r.role_key,a.desired,a.observed,a.generation,a.recovery_block,
		m.approved=1 AND m.revoked_at IS NULL FROM agent a JOIN role r ON r.id=a.role_id JOIN machine m ON m.id=a.machine_id
		WHERE a.project_id=? AND a.revoked_at IS NULL ORDER BY a.created_at,a.id`, projectID)
	if err != nil {
		return out, err
	}
	type currentAgent struct {
		id, slug, role, desired, observed string
		generation                        int
		blocked, machineReady             bool
	}
	items := []currentAgent{}
	for rows.Next() {
		var item currentAgent
		if err := rows.Scan(&item.id, &item.slug, &item.role, &item.desired, &item.observed, &item.generation, &item.blocked, &item.machineReady); err != nil {
			rows.Close()
			return out, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	counts := map[string]int{}
	stoppedCore := map[string]string{}
	for _, item := range items {
		if item.desired != "stopped" {
			counts[item.role]++
		} else if item.role == "leader" || item.role == "dispatcher" || item.role == "advisor" {
			if stoppedCore[item.role] == "" {
				stoppedCore[item.role] = item.id
			}
		}
	}
	for _, role := range []string{"leader", "dispatcher", "advisor"} {
		needed := role == "leader" || role == "dispatcher" && in.FlowKey == "leader-dispatcher" || role == "advisor" && in.FlowKey == "leader-advisor"
		target := flowMigrationTarget(in, role)
		if needed && counts[role] == 0 {
			if target == nil || target.MachineID == "" || target.Tool == "" || target.Model == "" || target.Path == "" {
				return out, ErrTeamFlowMigration
			}
		} else if target != nil {
			return out, ErrTeamFlowMigration
		}
	}
	seenChanges := map[string]bool{}
	for _, item := range items {
		action := flowMigrationAction(item.role, in.FlowKey, changed[item.id])
		if changed[item.id] {
			if action != "switch" || item.desired == "stopped" {
				return out, ErrTeamFlowMigration
			}
			seenChanges[item.id] = true
		}
		if counts[item.role] == 0 && stoppedCore[item.role] == item.id && (item.role == "leader" || item.role == "dispatcher" && in.FlowKey == "leader-dispatcher" || item.role == "advisor" && in.FlowKey == "leader-advisor") {
			action = "reactivate"
		}
		if item.desired == "stopped" && action != "reactivate" {
			continue
		}
		state := "draining"
		if action == "handoff_stop" {
			state = "waiting"
		}
		if _, err := tx.Exec(`INSERT INTO team_flow_migration_agent(migration_id,agent_id,slug,role_key,action,state) VALUES (?,?,?,?,?,?)`, id, item.id, item.slug, item.role, action, state); err != nil {
			return out, err
		}
		if action != "handoff_stop" {
			continue
		}
		roomID, err := tx.flowMigrationAgentRoom(projectID, item.id)
		if err != nil {
			return out, err
		}
		if _, err := tx.Exec(`DELETE FROM switch_handoff WHERE agent_id=?`, item.id); err != nil {
			return out, err
		}
		status := "waiting"
		if item.desired != "active" || item.observed == "stopped" || item.observed == "lost" || item.blocked || !item.machineReady {
			status = "automatic"
		}
		var roomMember bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL)`, roomID, item.id).Scan(&roomMember); err != nil {
			return out, err
		}
		if !roomMember {
			status = "automatic"
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(`INSERT INTO switch_handoff(agent_id,room_id,status,requested_at,overall_deadline,prior_desired) VALUES (?,?,?,?,?,?)`, item.id, roomID, status, now.Format(timestampLayout), now.Add(switchOverallWindow).Format(timestampLayout), item.desired); err != nil {
			return out, err
		}
		if status == "automatic" {
			if err := tx.switchAutomaticHandoff(item.id, roomID, "The departing agent was not accepting turns."); err != nil {
				return out, err
			}
		}
	}
	if len(seenChanges) != len(changed) {
		return out, ErrTeamFlowMigration
	}
	if err := tx.appendFlowMigrationEvent(projectID, id); err != nil {
		return out, err
	}
	out.ID, out.State, out.CanCancel, out.Agents = id, "draining", true, []TeamFlowMigrationAgentProgress{}
	return out, nil
}

func (tx *Tx) flowMigrationAgentRoom(projectID, agentID string) (string, error) {
	var id string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.id FROM room r JOIN room_member m ON m.room_id=r.id AND m.member_kind='agent' AND m.member_id=? AND m.left_at IS NULL
		WHERE r.project_id=? AND r.lifecycle='open' ORDER BY CASE WHEN r.team_flow_room_key='general' THEN 0 WHEN r.kind='task' THEN 1 ELSE 2 END,r.created_at,r.id LIMIT 1`, agentID, projectID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND kind='home' AND lifecycle='open' LIMIT 1`, projectID).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) CancelTeamFlowMigration(ctx context.Context, projectID, actorID string) (TeamFlowMigrationProgress, error) {
	var out TeamFlowMigrationProgress
	err := s.Tx(ctx, func(tx *Tx) error {
		if err := tx.authorizeDefinitionActor(projectID, actorID, false); err != nil {
			return err
		}
		var id, state string
		if err := tx.tx.QueryRowContext(ctx, `SELECT id,state FROM team_flow_migration WHERE project_id=? ORDER BY created_at DESC,id DESC LIMIT 1`, projectID).Scan(&id, &state); err != nil {
			return err
		}
		if state != "draining" {
			return ErrTeamFlowMigrationFinal
		}
		if err := tx.flowMigrationCancelHandoffs(id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE team_flow_migration SET state='cancelled',finished_at=? WHERE id=?`, timestamp(), id); err != nil {
			return err
		}
		if err := tx.appendFlowMigrationEvent(projectID, id); err != nil {
			return err
		}
		out.ID, out.State, out.CanCancel = id, "cancelled", false
		return nil
	})
	if err != nil {
		return out, err
	}
	return s.TeamFlowMigrationProgress(ctx, projectID, actorID)
}

func (tx *Tx) flowMigrationCancelHandoffs(id string) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT agent_id FROM team_flow_migration_agent WHERE migration_id=? AND action='handoff_stop'`, id)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var agentID string
		if err := rows.Scan(&agentID); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, agentID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, agentID := range ids {
		var turnID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE(turn_id,'') FROM switch_handoff WHERE agent_id=?`, agentID).Scan(&turnID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if turnID != "" {
			if _, err := tx.Exec(`UPDATE turn SET exec='killed',settle='settled',ended_at=? WHERE id=? AND exec='pending'`, timestamp(), turnID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE switch_handoff SET status='cancelled' WHERE agent_id=?`, agentID); err != nil {
			return err
		}
	}
	return nil
}

// AdvanceTeamFlowMigrations is called by the dispatcher. The drain is
// cancellable until all affected turns and departing-role handoffs settle.
// The flow update and its effects then commit in one transaction.
func (s *Store) AdvanceTeamFlowMigrations(ctx context.Context) ([]AgentCard, error) {
	controls := []AgentCard{}
	err := s.Tx(ctx, func(tx *Tx) error {
		rows, err := tx.tx.QueryContext(ctx, `SELECT id,project_id,actor_id,from_key,to_key,flow_rev,state,input_json FROM team_flow_migration WHERE state IN ('draining','applying') ORDER BY created_at,id`)
		if err != nil {
			return err
		}
		type item struct {
			id, projectID, actorID, fromKey, toKey, state, input string
			rev                                                  int64
		}
		items := []item{}
		for rows.Next() {
			var m item
			if err := rows.Scan(&m.id, &m.projectID, &m.actorID, &m.fromKey, &m.toKey, &m.rev, &m.state, &m.input); err != nil {
				rows.Close()
				return err
			}
			items = append(items, m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, m := range items {
			if m.state == "draining" {
				if _, err := tx.Exec(`SAVEPOINT flow_migration_drain`); err != nil {
					return err
				}
				ready, err := tx.flowMigrationDrainReady(m.id, m.projectID, m.fromKey, m.toKey)
				if err != nil {
					if _, rollbackErr := tx.Exec(`ROLLBACK TO flow_migration_drain`); rollbackErr != nil {
						return rollbackErr
					}
					if _, releaseErr := tx.Exec(`RELEASE flow_migration_drain`); releaseErr != nil {
						return releaseErr
					}
					if err := tx.flowMigrationDrainError(m.id, m.projectID, err); err != nil {
						return err
					}
					continue
				}
				if _, err := tx.Exec(`RELEASE flow_migration_drain`); err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE team_flow_migration SET drain_errors=0 WHERE id=? AND drain_errors<>0`, m.id); err != nil {
					return err
				}
				if !ready {
					continue
				}
				var saved flowMigrationStored
				if err := json.Unmarshal([]byte(m.input), &saved); err != nil {
					return err
				}
				if _, err := tx.Exec(`SAVEPOINT flow_migration_apply`); err != nil {
					return err
				}
				newControls, err := tx.applyFlowMigration(m.id, m.projectID, m.actorID, m.fromKey, m.toKey, m.rev, saved)
				if err != nil {
					if _, rollbackErr := tx.Exec(`ROLLBACK TO flow_migration_apply`); rollbackErr != nil {
						return rollbackErr
					}
					if _, releaseErr := tx.Exec(`RELEASE flow_migration_apply`); releaseErr != nil {
						return releaseErr
					}
					if err := tx.flowMigrationFail(m.id, m.projectID, "Flow change could not be applied: "+err.Error()); err != nil {
						return err
					}
					continue
				}
				if _, err := tx.Exec(`RELEASE flow_migration_apply`); err != nil {
					return err
				}
				controls = append(controls, newControls...)
			}
			if err := tx.flowMigrationSettle(m.id, m.projectID); err != nil {
				return err
			}
		}
		return nil
	})
	return controls, err
}

func (tx *Tx) flowMigrationFail(id, projectID, reason string) error {
	var state string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT state FROM team_flow_migration WHERE id=?`, id).Scan(&state); err != nil {
		return err
	}
	if state == "draining" {
		if err := tx.flowMigrationCancelHandoffs(id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE team_flow_migration SET state='failed',error=?,finished_at=? WHERE id=?`, reason, timestamp(), id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE team_flow_migration_agent SET state='failed',error=? WHERE migration_id=? AND state NOT IN ('done','failed')`, reason, id); err != nil {
		return err
	}
	return tx.appendFlowMigrationEvent(projectID, id)
}

const flowMigrationDrainErrorLimit = 3

func (tx *Tx) flowMigrationDrainError(id, projectID string, cause error) error {
	var attempts int
	if err := tx.tx.QueryRowContext(tx.ctx, `UPDATE team_flow_migration SET drain_errors=drain_errors+1 WHERE id=? RETURNING drain_errors`, id).Scan(&attempts); err != nil {
		return err
	}
	permanent := errors.Is(cause, ErrTeamFlowMigration) || errors.Is(cause, ErrInvalidTransition) || errors.Is(cause, sql.ErrNoRows)
	if !permanent && attempts < flowMigrationDrainErrorLimit {
		return nil
	}
	return tx.flowMigrationFail(id, projectID, "Departing-role handoff could not be completed: "+cause.Error())
}

func (tx *Tx) flowMigrationDrainReady(migrationID, projectID, fromKey, toKey string) (bool, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT ma.agent_id,ma.role_key,ma.action,ma.state,a.desired,a.observed,a.generation,a.recovery_block,
		m.approved=1 AND m.revoked_at IS NULL FROM team_flow_migration_agent ma
		JOIN agent a ON a.id=ma.agent_id JOIN machine m ON m.id=a.machine_id WHERE ma.migration_id=? ORDER BY ma.agent_id`, migrationID)
	if err != nil {
		return false, err
	}
	type item struct {
		id, role, action, state, desired, observed string
		generation                                 int
		blocked, machineReady                      bool
	}
	items := []item{}
	for rows.Next() {
		var a item
		if err := rows.Scan(&a.id, &a.role, &a.action, &a.state, &a.desired, &a.observed, &a.generation, &a.blocked, &a.machineReady); err != nil {
			rows.Close()
			return false, err
		}
		items = append(items, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	ready := true
	readyNotices := []item{}
	for _, a := range items {
		canHandoff := a.desired == "active" && a.observed != "stopped" && a.observed != "lost" && !a.blocked && a.machineReady
		handoffReady := false
		if a.action == "handoff_stop" {
			status, roomID, overall, turnID, err := tx.switchHandoffStatus(a.id)
			if err != nil {
				return false, err
			}
			now := time.Now().UTC().Format(timestampLayout)
			switch status {
			case "waiting":
				if now >= overall || !canHandoff {
					if err := tx.switchAutomaticHandoff(a.id, roomID, "Departing agent unavailable or handoff deadline reached."); err != nil {
						return false, err
					}
				} else {
					var open int
					if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE agent_id=? AND (exec IN ('pending','running') OR settle='pending')`, a.id).Scan(&open); err != nil {
						return false, err
					}
					if open == 0 && a.observed == "idle" && !a.blocked {
						if err := tx.beginSwitchHandoffTurn(a.id, roomID, a.generation); err != nil {
							return false, err
						}
					}
				}
				ready = false
			case "pending", "handing_off":
				var deadline sql.NullString
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT handoff_deadline FROM switch_handoff WHERE agent_id=?`, a.id).Scan(&deadline); err != nil {
					return false, err
				}
				if !canHandoff || now >= overall || status == "handing_off" && deadline.Valid && now >= deadline.String {
					if turnID != "" {
						if _, err := tx.Exec(`UPDATE turn SET exec='killed',settle='settled',ended_at=? WHERE id=? AND exec='pending'`, timestamp(), turnID); err != nil {
							return false, err
						}
					}
					cause := "Departing handoff turn timed out."
					if !canHandoff {
						cause = "Departing agent unavailable for the handoff turn."
					}
					if err := tx.switchAutomaticHandoff(a.id, roomID, cause); err != nil {
						return false, err
					}
				} else {
					ready = false
				}
			case "ready", "automatic":
				handoffReady = true
			case "":
				return false, ErrTeamFlowMigration
			default:
				return false, ErrInvalidTransition
			}
		}
		var open int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn t WHERE t.agent_id=? AND `+openFlowTurnPredicate, a.id).Scan(&open); err != nil {
			return false, err
		}
		if open != 0 {
			ready = false
		}
		if a.action == "notice" && open == 0 && a.state != "done" && a.desired == "active" && a.observed == "idle" && !a.blocked && a.machineReady {
			readyNotices = append(readyNotices, a)
		}
		state := "draining"
		if a.action == "notice" && open == 0 {
			state = "done"
		} else if a.action == "handoff_stop" && handoffReady && open == 0 {
			state = "running"
		}
		if _, err := tx.Exec(`UPDATE team_flow_migration_agent SET state=? WHERE migration_id=? AND agent_id=? AND state<>?`, state, migrationID, a.id, state); err != nil {
			return false, err
		}
	}
	if !ready {
		for _, a := range readyNotices {
			roomID, err := tx.flowMigrationNoticeRoom(projectID, a.id)
			if err != nil {
				return false, err
			}
			if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system",
				Body: fmt.Sprintf("The team flow is changing from %s to %s. Your %s role is ready; the change will finish when the other agents are ready.", fromKey, toKey, a.role), OnlyTargets: []string{a.id}}); err != nil {
				return false, err
			}
		}
	}
	return ready, nil
}

func (tx *Tx) applyFlowMigration(id, projectID, actorID, fromKey, toKey string, rev int64, saved flowMigrationStored) ([]AgentCard, error) {
	controls := []AgentCard{}
	if err := tx.refuseArchived(projectID); err != nil {
		return nil, err
	}
	flow, err := tx.applyTeamFlow(projectID, actorID, toKey, rev, saved.Input.RemoveMemberships)
	if err != nil {
		return nil, err
	}
	_ = flow
	if err := tx.applyTeamSetupSettings(projectID, actorID, saved.Input.Settings); err != nil {
		return nil, err
	}
	for role, config := range saved.Input.RoleDefaults {
		var roleID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM role WHERE role_key=? AND deleted_at IS NULL`, role).Scan(&roleID); err != nil {
			return nil, err
		}
		mode := config.PickMode
		if mode == "" {
			mode = "in_turn"
		}
		items, err := json.Marshal(config.DefaultAgents)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO project_role_spawn_default(project_id,role_id,items_json,pick_mode) VALUES (?,?,?,?)
			ON CONFLICT(project_id,role_id) DO UPDATE SET items_json=excluded.items_json,pick_mode=excluded.pick_mode`, projectID, roleID, string(items), mode); err != nil {
			return nil, err
		}
	}
	var leaderID string
	_ = tx.tx.QueryRowContext(tx.ctx, `SELECT a.id FROM agent a JOIN role r ON r.id=a.role_id WHERE a.project_id=? AND r.role_key='leader' AND a.revoked_at IS NULL
		ORDER BY CASE WHEN a.desired='stopped' THEN 1 ELSE 0 END,a.created_at,a.id LIMIT 1`, projectID).Scan(&leaderID)
	if leaderID == "" {
		if target, ok := saved.Targets["leader"]; ok {
			var homeID string
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND kind='home' AND lifecycle='open' ORDER BY created_at LIMIT 1`, projectID).Scan(&homeID); err != nil {
				return nil, err
			}
			card, err := tx.SpawnAgent(SpawnInput{RoomID: homeID, ActorKind: "user", ActorID: actorID, MachineID: target.MachineID,
				Path: target.Path, Slug: target.Name, RoleKey: "leader", Tool: target.Tool, Variant: target.Variant,
				LauncherID: target.LauncherID, Model: target.Model, Provider: target.Provider})
			if err != nil {
				return nil, err
			}
			leaderID = card.ID
			controls = append(controls, card)
			if _, err := tx.Exec(`INSERT INTO team_flow_migration_agent(migration_id,agent_id,slug,role_key,action,state) VALUES (?,?,?,'leader','spawn','running')`, id, card.ID, card.Slug); err != nil {
				return nil, err
			}
		}
	}
	var generalID string
	_ = tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND team_flow_room_key='general' AND lifecycle='open' ORDER BY created_at LIMIT 1`, projectID).Scan(&generalID)
	if generalID == "" {
		_ = tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND title=? AND lifecycle='open' ORDER BY created_at LIMIT 1`, projectID, GeneralRoomTitle).Scan(&generalID)
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT ma.agent_id,ma.slug,ma.role_key,ma.action,a.machine_id,a.desired_rev,a.tool,a.variant,a.model,COALESCE(a.launcher_id,'') FROM team_flow_migration_agent ma JOIN agent a ON a.id=ma.agent_id WHERE ma.migration_id=? ORDER BY ma.agent_id`, id)
	if err != nil {
		return nil, err
	}
	type item struct {
		agentID, slug, role, action, machineID, tool, variant, model, launcherID string
		desiredRev                                                               int64
	}
	items := []item{}
	for rows.Next() {
		var a item
		if err := rows.Scan(&a.agentID, &a.slug, &a.role, &a.action, &a.machineID, &a.desiredRev, &a.tool, &a.variant, &a.model, &a.launcherID); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, a := range items {
		switch a.action {
		case "notice":
			roomID, err := tx.flowMigrationNoticeRoom(projectID, a.agentID)
			if err != nil {
				return nil, err
			}
			if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system",
				Body: flowMigrationNotice(fromKey, toKey, a.role), OnlyTargets: []string{a.agentID}}); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`UPDATE team_flow_migration_agent SET state='done' WHERE migration_id=? AND agent_id=?`, id, a.agentID); err != nil {
				return nil, err
			}
		case "switch":
			target := saved.Targets[a.agentID]
			roomID, err := tx.flowMigrationAgentRoom(projectID, a.agentID)
			if err != nil {
				return nil, err
			}
			path := target.Path
			if target.MachineID == a.machineID {
				path = ""
			}
			card, err := tx.BeginAgentSwitchWithContinuity(a.agentID, actorID, roomID, target.Tool, target.Variant,
				target.Model, target.Provider, target.MachineID, path, target.LauncherID, a.desiredRev)
			if err != nil {
				return nil, err
			}
			controls = append(controls, card)
			if _, err := tx.Exec(`UPDATE team_flow_migration_agent SET state='running' WHERE migration_id=? AND agent_id=?`, id, a.agentID); err != nil {
				return nil, err
			}
		case "reactivate":
			target := saved.Targets[a.role]
			roomID, err := tx.flowMigrationAgentRoom(projectID, a.agentID)
			if err != nil {
				return nil, err
			}
			roomKey := "home"
			if a.role == "dispatcher" {
				roomKey = "routing"
			} else if a.role == "advisor" {
				roomKey = "advice"
			}
			if roomKey == "home" {
				err = tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND kind='home' AND lifecycle='open' LIMIT 1`, projectID).Scan(&roomID)
			} else {
				err = tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND team_flow_room_key=? AND lifecycle='open' LIMIT 1`, projectID, roomKey).Scan(&roomID)
			}
			if err != nil {
				return nil, err
			}
			if err := tx.addMemberOnly(roomID, "agent", a.agentID, true); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`DELETE FROM agent_switch WHERE agent_id=? AND state='failed'`, a.agentID); err != nil {
				return nil, err
			}
			started, err := tx.setAgentDesired(a.agentID, "user", actorID, "active", a.desiredRev, true, false)
			if err != nil {
				return nil, err
			}
			changed := target.MachineID != a.machineID || target.Tool != a.tool || target.Variant != a.variant || target.Model != a.model || target.LauncherID != a.launcherID
			if changed {
				path := target.Path
				if target.MachineID == a.machineID {
					path = ""
				}
				_, err := tx.BeginAgentSwitchWithContinuity(a.agentID, actorID, roomID, target.Tool, target.Variant,
					target.Model, target.Provider, target.MachineID, path, target.LauncherID, started.DesiredRev)
				if err != nil {
					return nil, err
				}
				if err := tx.switchAutomaticHandoff(a.agentID, roomID, "The prior session was already stopped before reactivation."); err != nil {
					return nil, err
				}
			} else {
				controls = append(controls, started)
			}
			if _, err := tx.Exec(`UPDATE team_flow_migration_agent SET state='running' WHERE migration_id=? AND agent_id=?`, id, a.agentID); err != nil {
				return nil, err
			}
		case "handoff_stop":
			if leaderID != "" {
				if err := tx.flowMigrationJoinTaskRooms(projectID, a.agentID, leaderID); err != nil {
					return nil, err
				}
				var body string
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT body_json FROM switch_handoff WHERE agent_id=?`, a.agentID).Scan(&body); err != nil {
					return nil, err
				}
				var handoff SwitchHandoffBody
				if err := json.Unmarshal([]byte(body), &handoff); err != nil {
					return nil, err
				}
				privateRoom, err := tx.flowMigrationHandoffRoom(projectID, a.agentID, leaderID, a.role)
				if err != nil {
					return nil, err
				}
				if _, err := tx.InsertMessage(Message{RoomID: privateRoom, AuthorKind: "system", AuthorID: "system",
					Body: fmt.Sprintf("%s handed off active work to the leader:\n\n%s", a.slug, formatSwitchHandoffBody(handoff)), OnlyTargets: []string{leaderID}}); err != nil {
					return nil, err
				}
				if generalID != "" {
					if _, err := tx.InsertMessage(Message{RoomID: generalID, AuthorKind: "system", AuthorID: "system",
						Body: fmt.Sprintf("%s handed off to the leader before stopping.", a.slug)}); err != nil {
						return nil, err
					}
				}
			}
			if _, err := tx.Exec(`UPDATE agent SET desired='stopped',desired_rev=desired_rev+1 WHERE id=? AND desired<>'stopped'`, a.agentID); err != nil {
				return nil, err
			}
			if _, err := tx.AppendEvent(Event{Kind: "agent_control_requested", ProjectID: projectID, AgentID: a.agentID,
				PayloadJSON: payloadJSON(map[string]string{"desired": "stopped", "actor_kind": "system", "actor_id": "team_flow_migration"})}); err != nil {
				return nil, err
			}
			card, err := tx.agentCardByID(a.agentID)
			if err != nil {
				return nil, err
			}
			controls = append(controls, card)
			if _, err := tx.Exec(`UPDATE team_flow_migration_agent SET state='running' WHERE migration_id=? AND agent_id=?`, id, a.agentID); err != nil {
				return nil, err
			}
		}
	}
	for _, role := range []string{"dispatcher", "advisor"} {
		target, ok := saved.Targets[role]
		if !ok {
			continue
		}
		var reactivating bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM team_flow_migration_agent WHERE migration_id=? AND role_key=? AND action='reactivate')`, id, role).Scan(&reactivating); err != nil {
			return nil, err
		}
		if reactivating {
			continue
		}
		roomKey := "home"
		if role == "dispatcher" {
			roomKey = "routing"
		} else if role == "advisor" {
			roomKey = "advice"
		}
		var roomID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND team_flow_room_key=? AND lifecycle='open' ORDER BY created_at LIMIT 1`, projectID, roomKey).Scan(&roomID); err != nil {
			return nil, err
		}
		card, err := tx.SpawnAgent(SpawnInput{RoomID: roomID, ActorKind: "user", ActorID: actorID, MachineID: target.MachineID,
			Path: target.Path, Slug: target.Name, RoleKey: role, Tool: target.Tool, Variant: target.Variant,
			LauncherID: target.LauncherID, Model: target.Model, Provider: target.Provider})
		if err != nil {
			return nil, err
		}
		controls = append(controls, card)
		if _, err := tx.Exec(`INSERT INTO team_flow_migration_agent(migration_id,agent_id,slug,role_key,action,state) VALUES (?,?,?,?,'spawn','running')`, id, card.ID, card.Slug, role); err != nil {
			return nil, err
		}
	}
	if generalID != "" {
		if _, err := tx.InsertMessage(Message{RoomID: generalID, AuthorKind: "system", AuthorID: "system",
			Body: fmt.Sprintf("Team flow changed from %s to %s. Kept agents received updated rules; new roles are starting and departing roles are handing off and stopping.", fromKey, toKey)}); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`UPDATE team_flow_migration SET state='applying',applied_at=? WHERE id=?`, timestamp(), id); err != nil {
		return nil, err
	}
	if err := tx.appendFlowMigrationEvent(projectID, id); err != nil {
		return nil, err
	}
	return controls, nil
}

func flowMigrationNotice(fromKey, toKey, role string) string {
	assignment := "Report to the leader. Use general and your task rooms."
	switch {
	case role == "leader" && toKey == "leader-dispatcher":
		assignment = "You remain the project leader. Use home and routing; the dispatcher manages task rooms."
	case role == "leader" && toKey == "leader-advisor":
		assignment = "You remain the project leader. Use home and advice; consult the advisor and manage task rooms."
	case role == "leader":
		assignment = "You remain the project leader. Use home, general, and task rooms."
	case role == "dispatcher":
		assignment = "Report to the leader in routing. Manage workers and reviewers in task rooms."
	case role == "advisor":
		assignment = "Report to the leader in advice. Use advice and general for consultation."
	case toKey == "leader-dispatcher":
		assignment = "Report to the dispatcher. Use general and your task rooms."
	}
	rules := ""
	if template, ok := teamFlowTemplate(toKey); ok {
		rules = template.RoleTextOverrides[role]
		if rules == "" && strings.HasSuffix(role, "-reviewer") {
			rules = template.RoleTextOverrides["reviewer"]
		}
	}
	notice := fmt.Sprintf("The team flow changed from %s to %s for your %s role. %s", fromKey, toKey, role, assignment)
	if rules != "" {
		notice += "\n\nYour role rules on the next turn are:\n" + rules
	}
	return notice
}

func (tx *Tx) flowMigrationNoticeRoom(projectID, agentID string) (string, error) {
	var roomID string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.id FROM agent a JOIN room r ON r.id=a.home_room_id
		JOIN room_member m ON m.room_id=r.id AND m.member_kind='agent' AND m.member_id=a.id AND m.left_at IS NULL
		WHERE a.id=? AND a.project_id=? AND r.lifecycle='open'`, agentID, projectID).Scan(&roomID)
	if err == nil {
		return roomID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return tx.flowMigrationAgentRoom(projectID, agentID)
}

func (tx *Tx) flowMigrationHandoffRoom(projectID, departingID, leaderID, role string) (string, error) {
	key := "routing"
	if role == "advisor" {
		key = "advice"
	}
	var roomID string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.id FROM room r
		JOIN room_member d ON d.room_id=r.id AND d.member_kind='agent' AND d.member_id=? AND d.left_at IS NULL
		JOIN room_member l ON l.room_id=r.id AND l.member_kind='agent' AND l.member_id=? AND l.left_at IS NULL
		WHERE r.project_id=? AND r.team_flow_room_key=? AND r.lifecycle='open' AND r.restricted=1 LIMIT 1`,
		departingID, leaderID, projectID, key).Scan(&roomID)
	if err == nil {
		return roomID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	room, _, err := tx.OpenAgentDM(departingID, leaderID, "team_flow_migration")
	return room.ID, err
}

func (tx *Tx) flowMigrationJoinTaskRooms(projectID, departingID, leaderID string) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT r.id FROM room r JOIN room_member m ON m.room_id=r.id AND m.member_kind='agent' AND m.member_id=? AND m.left_at IS NULL
		WHERE r.project_id=? AND r.kind='task' AND r.team_flow_room_key NOT IN ('routing','advice','general') AND r.title<>? AND r.lifecycle='open'`, departingID, projectID, GeneralRoomTitle)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var roomID string
		if err := rows.Scan(&roomID); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, roomID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, roomID := range ids {
		// A migration handoff grants the leader access to the departing
		// agent's task context even when the target flow normally delegates
		// task-room membership to a dispatcher.
		if _, err := tx.Exec(`INSERT INTO room_member(room_id,member_kind,member_id,joined_at,auto)
			SELECT ?,'agent',?,?,1 WHERE NOT EXISTS (SELECT 1 FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL)`,
			roomID, leaderID, timestamp(), roomID, leaderID); err != nil {
			return err
		}
	}
	return nil
}

func (tx *Tx) flowMigrationSettle(id, projectID string) error {
	var state string
	var appliedAt sql.NullString
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT state,applied_at FROM team_flow_migration WHERE id=?`, id).Scan(&state, &appliedAt); err != nil {
		return err
	}
	if state != "applying" {
		return nil
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT ma.agent_id,ma.action,ma.state,a.desired,a.observed FROM team_flow_migration_agent ma JOIN agent a ON a.id=ma.agent_id WHERE ma.migration_id=?`, id)
	if err != nil {
		return err
	}
	type item struct{ agentID, action, state, desired, observed string }
	items := []item{}
	for rows.Next() {
		var a item
		if err := rows.Scan(&a.agentID, &a.action, &a.state, &a.desired, &a.observed); err != nil {
			rows.Close()
			return err
		}
		items = append(items, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	allDone, anyFailed := true, false
	for _, a := range items {
		state := a.state
		if a.observed == "lost" && state != "done" {
			state = "failed"
		}
		switch a.action {
		case "notice":
			state = "done"
		case "spawn":
			if a.observed == "idle" || a.observed == "in_turn" {
				state = "done"
			} else {
				var failed bool
				_ = tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE agent_id=? AND kind='agent_spawn_failed')`, a.agentID).Scan(&failed)
				if failed {
					state = "failed"
				}
			}
		case "handoff_stop":
			if a.observed == "stopped" {
				state = "done"
			}
		case "reactivate":
			var switchState string
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT state FROM agent_switch WHERE agent_id=?`, a.agentID).Scan(&switchState); err == nil && switchState == "failed" {
				state = "failed"
			} else if errors.Is(err, sql.ErrNoRows) && (a.observed == "idle" || a.observed == "in_turn") {
				state = "done"
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		case "switch":
			var switchState string
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT state FROM agent_switch WHERE agent_id=?`, a.agentID).Scan(&switchState)
			if errors.Is(err, sql.ErrNoRows) {
				state = "done"
			} else if err != nil {
				return err
			} else if switchState == "failed" {
				state = "failed"
			}
		}
		if a.observed == "lost" && state != "done" {
			state = "failed"
		}
		if state != a.state {
			if _, err := tx.Exec(`UPDATE team_flow_migration_agent SET state=? WHERE migration_id=? AND agent_id=?`, state, id, a.agentID); err != nil {
				return err
			}
		}
		allDone = allDone && state == "done"
		anyFailed = anyFailed || state == "failed"
	}
	if !allDone && !anyFailed && appliedAt.Valid && appliedAt.String <= time.Now().UTC().Add(-20*time.Minute).Format(timestampLayout) {
		return tx.flowMigrationFail(id, projectID, "Agent start or stop was not confirmed within twenty minutes.")
	}
	if !allDone && !anyFailed {
		return nil
	}
	if anyFailed {
		return tx.flowMigrationFail(id, projectID, "An agent could not complete its start, stop, or switch.")
	}
	if _, err := tx.Exec(`UPDATE team_flow_migration SET state='done',finished_at=? WHERE id=?`, timestamp(), id); err != nil {
		return err
	}
	return tx.appendFlowMigrationEvent(projectID, id)
}

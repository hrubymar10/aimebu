package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/goccy/go-json"
)

// TeamSetupTarget is a validated, fresh native session to start at the end
// of the wizard. Path and KeyHash are server preflight values, never request
// fields supplied by a browser.
type TeamSetupTarget struct {
	MachineID  string `json:"machine_id"`
	Tool       string `json:"tool"`
	Variant    string `json:"variant"`
	LauncherID string `json:"launcher_id,omitempty"`
	Model      string `json:"model"`
	Provider   string `json:"-"`
	Path       string `json:"-"`
	KeyHash    string `json:"-"`
}

type TeamSetupRoleDefault struct {
	DefaultAgents []SpawnDefaultChoice `json:"default_agents"`
	PickMode      string               `json:"pick_mode"`
}

type TeamSetupSettings struct {
	WorktreeMode  *string `json:"worktree_mode,omitempty"`
	MemoryEnabled *bool   `json:"memory_enabled,omitempty"`
	MaxLiveAgents *int    `json:"max_live_agents,omitempty"`
}

type TeamSetupInput struct {
	FlowKey      string                          `json:"flow_key"`
	FlowRev      int64                           `json:"flow_rev"`
	Leader       TeamSetupTarget                 `json:"leader"`
	Dispatcher   *TeamSetupTarget                `json:"dispatcher,omitempty"`
	RoleDefaults map[string]TeamSetupRoleDefault `json:"role_defaults,omitempty"`
	Settings     TeamSetupSettings               `json:"settings,omitempty"`
}

type TeamSetupAgent struct {
	Role    string           `json:"role"`
	AgentID string           `json:"agent_id,omitempty"`
	RoomID  string           `json:"room_id,omitempty"`
	RoomKey string           `json:"room_key,omitempty"`
	Target  *TeamSetupTarget `json:"target,omitempty"`
}

type TeamSetupResult struct {
	Flow          TeamFlowApplied  `json:"flow"`
	CreatedRooms  []Room           `json:"created_rooms"`
	Agents        []TeamSetupAgent `json:"agents"`
	DefaultsSaved []string         `json:"defaults_saved"`
	Settings      map[string]any   `json:"settings"`
}

var ErrTeamSetupInvalid = errors.New("invalid team setup")

// TeamSetupRequestResult lets a keyed retry return the committed result even
// if a machine went offline after the transaction completed.
func (s *Store) TeamSetupRequestResult(ctx context.Context, actorID, key, digest string) (string, bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", false, err
	}
	var savedDigest, result string
	err := s.db.QueryRowContext(ctx, `SELECT request_digest,result_json FROM request_key WHERE actor_kind='user' AND actor_id=? AND kind='team_spawn' AND key=?`, actorID, key).Scan(&savedDigest, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if savedDigest != digest {
		return "", false, ErrConflict
	}
	return result, true, nil
}

func validateTeamSetupShape(in TeamSetupInput) error {
	if in.FlowKey != "leader-workers" && in.FlowKey != "leader-dispatcher" || in.Leader.Tool == "" || in.Leader.Model == "" {
		return ErrTeamSetupInvalid
	}
	if (in.FlowKey == "leader-dispatcher") != (in.Dispatcher != nil) {
		return fmt.Errorf("%w: dispatcher required only for leader-dispatcher", ErrTeamSetupInvalid)
	}
	if in.Settings.WorktreeMode != nil && *in.Settings.WorktreeMode != "auto" && *in.Settings.WorktreeMode != "off" {
		return fmt.Errorf("%w: worktree_mode", ErrTeamSetupInvalid)
	}
	if in.Settings.MaxLiveAgents != nil && (*in.Settings.MaxLiveAgents < 1 || *in.Settings.MaxLiveAgents > 100) {
		return fmt.Errorf("%w: max_live_agents must be 1..100", ErrTeamSetupInvalid)
	}
	for role := range in.RoleDefaults {
		if role != "worker" && role != "reviewer" {
			return fmt.Errorf("%w: unknown role default %s", ErrTeamSetupInvalid, role)
		}
	}
	return nil
}

// ValidateTeamSetup checks the same database constraints used by commit. The
// caller separately preflights machine and launcher availability.
func (s *Store) ValidateTeamSetup(ctx context.Context, projectID, actorID string, in TeamSetupInput) error {
	if err := validateTeamSetupShape(in); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx *Tx) error { return tx.validateTeamSetup(projectID, actorID, in) })
}

func (tx *Tx) validateTeamSetup(projectID, actorID string, in TeamSetupInput) error {
	if err := tx.authorizeDefinitionActor(projectID, actorID, false); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrForbidden
		}
		return err
	}
	if err := tx.refuseArchived(projectID); err != nil {
		return err
	}
	var rev int64
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT team_flow_rev FROM project WHERE id=? AND system=0`, projectID).Scan(&rev); err != nil {
		return err
	}
	if rev != in.FlowRev {
		return ErrTeamFlowRev
	}
	var occupied int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM agent a JOIN role r ON r.id=a.role_id WHERE a.project_id=? AND a.revoked_at IS NULL AND r.role_key IN ('leader','dispatcher') AND (a.desired!='stopped' OR a.observed!='stopped')`, projectID).Scan(&occupied); err != nil {
		return err
	}
	if occupied != 0 {
		return fmt.Errorf("%w: project already has a leader or dispatcher", ErrTeamSetupInvalid)
	}
	var live, maxLive int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM agent WHERE project_id=? AND revoked_at IS NULL AND (desired!='stopped' OR observed!='stopped')`, projectID).Scan(&live); err != nil {
		return err
	}
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT max_live_agents FROM settings_project WHERE project_id=?`, projectID).Scan(&maxLive); err != nil {
		return err
	}
	if in.Settings.MaxLiveAgents != nil {
		maxLive = *in.Settings.MaxLiveAgents
	}
	wanted := 1
	if in.Dispatcher != nil {
		wanted++
	}
	if live+wanted > maxLive {
		return ErrSpawnLimit
	}
	for role, config := range in.RoleDefaults {
		mode := config.PickMode
		if mode == "" {
			mode = "in_turn"
		}
		if err := tx.validateSpawnDefaults(config.DefaultAgents, mode); err != nil {
			return fmt.Errorf("%s: %w", role, err)
		}
	}
	return nil
}

// CreateTeamSetup is called from WithRequestKey. Every persistent change is
// in that one transaction; the server sends spawn frames after it commits.
func (tx *Tx) CreateTeamSetup(projectID, actorID string, in TeamSetupInput) (TeamSetupResult, error) {
	out := TeamSetupResult{Agents: []TeamSetupAgent{}, DefaultsSaved: []string{}, CreatedRooms: []Room{}}
	if err := validateTeamSetupShape(in); err != nil {
		return out, err
	}
	if err := tx.validateTeamSetup(projectID, actorID, in); err != nil {
		return out, err
	}
	flow, err := tx.applyTeamFlow(projectID, actorID, in.FlowKey, in.FlowRev)
	if err != nil {
		return out, err
	}
	out.Flow, out.CreatedRooms = flow, flow.CreatedRooms
	if err := tx.applyTeamSetupSettings(projectID, actorID, in.Settings); err != nil {
		return out, err
	}
	roles := make([]string, 0, len(in.RoleDefaults))
	for role := range in.RoleDefaults {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		config := in.RoleDefaults[role]
		mode := config.PickMode
		if mode == "" {
			mode = "in_turn"
		}
		var roleID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM role WHERE role_key=? AND deleted_at IS NULL`, role).Scan(&roleID); err != nil {
			return out, err
		}
		encoded, err := json.Marshal(config.DefaultAgents)
		if err != nil {
			return out, err
		}
		if _, err := tx.Exec(`INSERT INTO project_role_spawn_default(project_id,role_id,items_json,pick_mode) VALUES (?,?,?,?) ON CONFLICT(project_id,role_id) DO UPDATE SET items_json=excluded.items_json,pick_mode=excluded.pick_mode`, projectID, roleID, string(encoded), mode); err != nil {
			return out, err
		}
		out.DefaultsSaved = append(out.DefaultsSaved, role)
	}
	for _, spec := range []struct {
		role, roomKey string
		target        *TeamSetupTarget
	}{{"leader", "home", &in.Leader}, {"dispatcher", "routing", in.Dispatcher}} {
		if spec.target == nil {
			continue
		}
		var roomID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND team_flow_room_key=? AND lifecycle='open' ORDER BY created_at LIMIT 1`, projectID, spec.roomKey).Scan(&roomID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) || spec.roomKey != "home" {
				return out, err
			}
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND kind='home' AND lifecycle='open' ORDER BY created_at LIMIT 1`, projectID).Scan(&roomID); err != nil {
				return out, err
			}
		}
		card, err := tx.SpawnAgent(SpawnInput{RoomID: roomID, ActorKind: "user", ActorID: actorID, MachineID: spec.target.MachineID, Path: spec.target.Path, RoleKey: spec.role, Tool: spec.target.Tool, Variant: spec.target.Variant, LauncherID: spec.target.LauncherID, Model: spec.target.Model, Provider: spec.target.Provider, KeyHash: spec.target.KeyHash})
		if err != nil {
			return out, err
		}
		out.Agents = append(out.Agents, TeamSetupAgent{Role: spec.role, AgentID: card.ID, RoomID: card.RoomID, RoomKey: spec.roomKey})
	}
	var worktreeMode string
	var memoryEnabled bool
	var maxLiveAgents int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT worktree_mode,memory_enabled,max_live_agents FROM settings_project WHERE project_id=?`, projectID).Scan(&worktreeMode, &memoryEnabled, &maxLiveAgents); err != nil {
		return out, err
	}
	out.Settings = map[string]any{"worktree_mode": worktreeMode, "memory_enabled": memoryEnabled, "max_live_agents": maxLiveAgents}
	return out, nil
}

func (tx *Tx) applyTeamSetupSettings(projectID, actorID string, update TeamSetupSettings) error {
	if update.WorktreeMode != nil {
		var current string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT worktree_mode FROM settings_project WHERE project_id=?`, projectID).Scan(&current); err != nil {
			return err
		}
		if current != *update.WorktreeMode {
			if _, err := tx.Exec(`UPDATE settings_project SET worktree_mode=?,worktree_auto_since=? WHERE project_id=?`, *update.WorktreeMode, timestamp(), projectID); err != nil {
				return err
			}
		}
	}
	if update.MemoryEnabled != nil {
		if _, err := tx.Exec(`UPDATE settings_project SET memory_enabled=? WHERE project_id=?`, *update.MemoryEnabled, projectID); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(Event{Kind: "project_memory_setting_changed", ProjectID: projectID, PayloadJSON: payloadJSON(map[string]string{"enabled": strconv.FormatBool(*update.MemoryEnabled), "actor_id": actorID})}); err != nil {
			return err
		}
	}
	if update.MaxLiveAgents != nil {
		if _, err := tx.Exec(`UPDATE settings_project SET max_live_agents=? WHERE project_id=?`, *update.MaxLiveAgents, projectID); err != nil {
			return err
		}
	}
	if update.WorktreeMode != nil || update.MaxLiveAgents != nil {
		_, err := tx.AppendEvent(Event{Kind: "project_settings_updated", ProjectID: projectID, PayloadJSON: payloadJSON(map[string]string{"actor_id": actorID})})
		return err
	}
	return nil
}

package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// AgentRoomState is an agent's card with everything the room's agent list shows beside it: its usage, its health flags (without the
// machine's connection, which only the server knows) and what the server needs to judge a hold on it.
type AgentRoomState struct {
	Card  AgentCard
	Usage *AgentUsage
	Flags []HealthFlag
	// MachineIsHost says whether the agent's machine is the host machine.
	MachineIsHost bool
	// LauncherKnown says whether the agent's launcher is a row of its machine; LauncherContainerized is that row's flag.
	LauncherKnown, LauncherContainerized bool
}

// RoomAgentStates is the agent list of a room: the states in the order of the cards, and the asking person's role in the room's project
// (empty when they have none).
type RoomAgentStates struct {
	Agents     []AgentRoomState
	ViewerRole string
}

// AgentRoomStates reads, in two statements however many agents the room has, what AgentCardsForRoom, AgentUsage and AgentHealth read one
// agent at a time: the cards with their switch, usage, health evidence and machine (one statement) and the boundary grants of all of them
// (the other). userID is the person asking, whose project role comes back with the cards.
func (s *Store) AgentRoomStates(ctx context.Context, roomID, projectID, userID string, now time.Time, stuckAfter time.Duration) (RoomAgentStates, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return RoomAgentStates{}, err
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT a.id,a.slug,a.project_id,r.role_key,r.emoji,r.deleted_at IS NOT NULL,
		EXISTS(SELECT 1 FROM role current WHERE current.project_id IS NULL AND current.role_key=r.role_key AND current.id<>r.id AND current.deleted_at IS NULL AND (current.body<>r.body OR current.extends<>r.extends OR current.label<>r.label OR current.emoji<>r.emoji OR current.cardinality<>r.cardinality OR current.cap_is_lead<>r.cap_is_lead OR current.cap_can_spawn<>r.cap_can_spawn OR current.cap_singleton_writer<>r.cap_singleton_writer)),
		a.tool,a.variant,COALESCE(a.launcher_id,''),a.model,a.provider,a.machine_id,a.desired,a.desired_rev,a.observed,a.generation,a.recovery_block,
		COALESCE((SELECT MAX(e.created_at) FROM event e WHERE e.agent_id=a.id AND `+agentOwnActivitySQL+`),''),COALESCE(machine.last_seen,''),
		COALESCE((SELECT json_extract(e.payload_json,'$.evidence') FROM event e WHERE e.turn_id=(SELECT t.id FROM turn t WHERE t.agent_id=a.id AND t.outcome='unknown' ORDER BY t.id DESC LIMIT 1) AND e.kind='turn_process_exit' LIMIT 1),''),
		COALESCE((SELECT b.inbox_id FROM boundary_pending b WHERE b.agent_id=a.id AND b.state='open'),''),
		sw.agent_id IS NOT NULL,COALESCE(sw.state,''),COALESCE(sw.tool,''),COALESCE(sw.variant,''),COALESCE(sw.model,''),COALESCE(sw.machine_id,''),COALESCE(sw.error,''),
		sh.agent_id IS NOT NULL,COALESCE(sh.status,''),COALESCE(sh.source,''),COALESCE(sh.body_json,''),COALESCE(sh.handoff_deadline,''),COALESCE(sh.overall_deadline,''),
		CASE WHEN sh.turn_id IS NULL THEN 0 ELSE EXISTS(SELECT 1 FROM turn WHERE id=sh.turn_id AND (exec IN ('pending','running') OR settle='pending')) END,
		CASE WHEN a.observed='in_turn' THEN COALESCE((SELECT CASE WHEN n.wake_reason='reply' AND wm.author_kind='agent' THEN 'reply' ELSE '' END
			FROM turn t JOIN turn_note tn ON tn.turn_id=t.id JOIN note n ON n.id=tn.note_id JOIN message wm ON wm.id=n.message_id
			WHERE t.agent_id=a.id AND t.exec IN ('pending','running') ORDER BY t.rowid DESC, n.created_at ASC, n.rowid ASC LIMIT 1),'') ELSE '' END,
		u.rowid IS NOT NULL,u.tokens_in,u.tokens_out,u.context_used,u.context_window,
		(SELECT SUM(cost_est) FROM usage_record WHERE agent_id=a.id),
		EXISTS(SELECT 1 FROM usage_record WHERE agent_id=a.id AND cost_est IS NOT NULL AND source='estimated'),
		rt.id IS NOT NULL,COALESCE(rt.started_at,''),
		COALESCE((SELECT MAX(e.created_at) FROM event e WHERE e.turn_id=rt.id AND e.kind IN ('turn_ack_accepted','supervisor_activity','supervisor_usage','supervisor_compaction')),''),
		pl.agent_id IS NOT NULL,pl.reset_at,COALESCE(pl.detected_at,''),
		EXISTS(SELECT 1 FROM inbox_item i WHERE i.agent_id=a.id AND i.state='open' AND i.kind='delivery_failed'),
		EXISTS(SELECT 1 FROM inbox_item i WHERE i.agent_id=a.id AND i.state='open' AND i.kind='recovery'),
		COALESCE(machine.is_host,0),l.id IS NOT NULL,COALESCE(l.containerized,0),
		COALESCE((SELECT pm.role FROM membership pm WHERE pm.user_id=? AND pm.project_id=?),'')
		FROM agent a JOIN role r ON r.id=a.role_id JOIN room_member m ON m.member_id=a.id AND m.member_kind='agent' AND m.left_at IS NULL
		LEFT JOIN machine ON machine.id=a.machine_id
		LEFT JOIN machine_launcher l ON l.machine_id=a.machine_id AND l.id=a.launcher_id
		LEFT JOIN agent_switch sw ON sw.agent_id=a.id
		LEFT JOIN switch_handoff sh ON sh.agent_id=a.id
		LEFT JOIN usage_record u ON u.rowid=(SELECT u2.rowid FROM usage_record u2 JOIN turn ut ON ut.id=u2.turn_id
			WHERE u2.agent_id=a.id AND u2.partial=0 AND ut.generation=a.generation ORDER BY u2.created_at DESC,u2.rowid DESC LIMIT 1)
		LEFT JOIN turn rt ON rt.id=(SELECT t.id FROM turn t WHERE t.agent_id=a.id AND t.exec='running' ORDER BY t.started_at DESC LIMIT 1)
		LEFT JOIN provider_limit pl ON pl.agent_id=a.id AND pl.tool=a.tool
		WHERE m.room_id=? ORDER BY a.created_at,a.id`, userID, projectID, roomID)
	if err != nil {
		return RoomAgentStates{}, err
	}
	defer rows.Close()
	out := RoomAgentStates{Agents: []AgentRoomState{}}
	index := map[string]int{}
	for rows.Next() {
		var st AgentRoomState
		c := &st.Card
		c.RoomID = roomID
		var hasSwitch, hasHandoff, turnOpen, hasUsage, hasRunning, hasLimit, held, recovery bool
		var sw AgentSwitch
		var handoffStatus, handoffSource, handoffBody, handoffDeadline, overallDeadline string
		var input, output, used, window sql.NullInt64
		var cost sql.NullFloat64
		var estimated bool
		var started, lastEvent, detected string
		var reset sql.NullString
		if err := rows.Scan(&c.ID, &c.Slug, &c.ProjectID, &c.RoleKey, &c.RoleEmoji, &c.RoleDeleted, &c.RoleUpdated, &c.Tool, &c.Variant, &c.LauncherID, &c.Model, &c.Provider, &c.MachineID, &c.Desired, &c.DesiredRev, &c.Observed, &c.Generation, &c.RecoveryBlock, &c.LastActivityAt, &c.MachineLastSeen, &c.LastExitEvidence, &c.BoundaryPending,
			&hasSwitch, &sw.State, &sw.Tool, &sw.Variant, &sw.Model, &sw.MachineID, &sw.Error,
			&hasHandoff, &handoffStatus, &handoffSource, &handoffBody, &handoffDeadline, &overallDeadline, &turnOpen, &c.WokenBy,
			&hasUsage, &input, &output, &used, &window, &cost, &estimated,
			&hasRunning, &started, &lastEvent, &hasLimit, &reset, &detected, &held, &recovery,
			&st.MachineIsHost, &st.LauncherKnown, &st.LauncherContainerized, &out.ViewerRole); err != nil {
			return RoomAgentStates{}, err
		}
		if hasSwitch {
			c.Switch = &sw
		}
		if hasHandoff {
			if c.Switch == nil && handoffStatus == "started" {
				c.Switch = &AgentSwitch{State: "started", Tool: c.Tool, Variant: c.Variant, Model: c.Model, MachineID: c.MachineID}
			}
			if c.Switch != nil {
				c.Switch.HandoffStatus = handoffStatus
				c.Switch.HandoffSource = handoffSource
				c.Switch.HandoffDeadline = handoffDeadline
				c.Switch.OverallDeadline = overallDeadline
				if handoffBody != "" {
					c.Switch.Handoff = json.RawMessage(handoffBody)
				}
				c.Switch.setContinuityActions(turnOpen)
			}
		}
		if hasUsage || cost.Valid {
			usage := &AgentUsage{}
			if input.Valid {
				usage.TokensIn = &input.Int64
			}
			if output.Valid {
				usage.TokensOut = &output.Int64
			}
			if used.Valid {
				usage.ContextUsed = &used.Int64
			}
			if window.Valid {
				usage.ContextWindow = &window.Int64
			}
			if cost.Valid {
				usage.CostSoFar = &cost.Float64
				usage.CostEstimated = estimated
			}
			st.Usage = usage
		}
		st.Flags = []HealthFlag{}
		if hasRunning {
			last := started
			if lastEvent > last {
				last = lastEvent
			}
			if at, err := time.Parse(time.RFC3339Nano, last); err == nil && now.Sub(at) > stuckAfter {
				st.Flags = append(st.Flags, HealthFlag{Code: "stuck", Since: last})
			}
		}
		if hasLimit && (!reset.Valid || reset.String > now.UTC().Format("2006-01-02T15:04:05.000000000Z")) {
			st.Flags = append(st.Flags, HealthFlag{Code: "out_of_quota", Since: detected, ResetAt: reset.String})
		}
		if held {
			st.Flags = append(st.Flags, HealthFlag{Code: "held_answer"})
		}
		if recovery {
			st.Flags = append(st.Flags, HealthFlag{Code: "recovery_waiting"})
		}
		index[c.ID] = len(out.Agents)
		out.Agents = append(out.Agents, st)
	}
	if err := rows.Err(); err != nil {
		return RoomAgentStates{}, err
	}
	if err := rows.Close(); err != nil {
		return RoomAgentStates{}, err
	}
	grants, err := s.readDB.QueryContext(ctx, `SELECT g.agent_id,g.machine_id,g.canonical_path,g.approved_at FROM agent_boundary_grant g
		JOIN room_member m ON m.member_id=g.agent_id AND m.member_kind='agent' AND m.left_at IS NULL
		WHERE m.room_id=? ORDER BY g.agent_id,g.approved_at,g.machine_id,g.canonical_path`, roomID)
	if err != nil {
		return RoomAgentStates{}, err
	}
	defer grants.Close()
	for grants.Next() {
		var agentID string
		var grant BoundaryGrant
		if err := grants.Scan(&agentID, &grant.MachineID, &grant.Path, &grant.ApprovedAt); err != nil {
			return RoomAgentStates{}, err
		}
		if i, ok := index[agentID]; ok {
			out.Agents[i].Card.BoundaryGrants = append(out.Agents[i].Card.BoundaryGrants, grant)
		}
	}
	return out, grants.Err()
}

// GlobalSettingValues reads several free-form global settings in one statement; a key that is not saved is missing from the map.
func (s *Store) GlobalSettingValues(ctx context.Context, keys ...string) (map[string]string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	values := map[string]string{}
	if len(keys) == 0 {
		return values, nil
	}
	args := make([]any, len(keys))
	for i, key := range keys {
		args[i] = key
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM global_setting WHERE key IN (`+strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, rows.Err()
}

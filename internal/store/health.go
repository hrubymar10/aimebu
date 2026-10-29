package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/goccy/go-json"
)

type HealthFlag struct {
	Code    string `json:"code"`
	Since   string `json:"since,omitempty"`
	ResetAt string `json:"reset_at,omitempty"`
}

type HealthAgent struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	RoomIDs   []string     `json:"room_ids"`
	Flags     []HealthFlag `json:"flags"`
	MachineID string       `json:"-"`
	Desired   string       `json:"-"`
}

// HealthForUser reads all visible agents and their durable health evidence in
// one snapshot. Private rooms only contribute when the user is a participant.
//
// The agents come back in the order the per-room composition met them: by project, then by the agent's first room in the room list's
// order (depth, creation, id), then by creation and id, which is the order of a room's cards. Each agent's rooms are in the room list's
// order too. json_group_array(... ORDER BY ...) needs SQLite 3.44 or newer.
func (s *Store) HealthForUser(ctx context.Context, userID string, now time.Time, stuckAfter time.Duration) ([]HealthAgent, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT a.id,a.slug,a.machine_id,a.desired,json_group_array(rm.room_id ORDER BY r.depth,r.created_at,r.id),
		COALESCE((SELECT t.started_at FROM turn t WHERE t.agent_id=a.id AND t.exec='running' ORDER BY t.started_at DESC LIMIT 1),''),
		COALESCE((SELECT MAX(e.created_at) FROM event e WHERE e.turn_id=(SELECT t.id FROM turn t WHERE t.agent_id=a.id AND t.exec='running' ORDER BY t.started_at DESC LIMIT 1)
			AND e.kind IN ('turn_ack_accepted','supervisor_activity','supervisor_usage','supervisor_compaction')),''),
		COALESCE(p.detected_at,''),COALESCE(p.reset_at,''),
		EXISTS(SELECT 1 FROM inbox_item i WHERE i.agent_id=a.id AND i.state='open' AND i.kind='recovery'),
		EXISTS(SELECT 1 FROM inbox_item i WHERE i.agent_id=a.id AND i.state='open' AND i.kind='delivery_failed')
		FROM agent a JOIN project proj ON proj.id=a.project_id JOIN membership member ON member.project_id=a.project_id AND member.user_id=?
		JOIN room_member rm ON rm.member_id=a.id AND rm.member_kind='agent' AND rm.left_at IS NULL
		JOIN room r ON r.id=rm.room_id AND r.project_id=a.project_id AND (r.kind!='dm' OR EXISTS(SELECT 1 FROM room_member own
			WHERE own.room_id=r.id AND own.member_kind='user' AND own.member_id=? AND own.left_at IS NULL))
		LEFT JOIN provider_limit p ON p.agent_id=a.id AND p.tool=a.tool
		GROUP BY a.id ORDER BY proj.created_at,proj.id,MIN(printf('%d|%s|%s',r.depth,r.created_at,r.id)),a.created_at,a.id`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := []HealthAgent{}
	for rows.Next() {
		var item HealthAgent
		var rooms, started, lastEvent, detected, reset string
		var recovery, held bool
		if err := rows.Scan(&item.ID, &item.Name, &item.MachineID, &item.Desired, &rooms,
			&started, &lastEvent, &detected, &reset, &recovery, &held); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(rooms), &item.RoomIDs); err != nil {
			return nil, err
		}
		item.Flags = []HealthFlag{}
		last := started
		if lastEvent > last {
			last = lastEvent
		}
		if started != "" {
			if at, err := time.Parse(time.RFC3339Nano, last); err == nil && now.Sub(at) > stuckAfter {
				item.Flags = append(item.Flags, HealthFlag{Code: "stuck", Since: last})
			}
		}
		if detected != "" && (reset == "" || reset > now.UTC().Format("2006-01-02T15:04:05.000000000Z")) {
			item.Flags = append(item.Flags, HealthFlag{Code: "out_of_quota", Since: detected, ResetAt: reset})
		}
		if held {
			item.Flags = append(item.Flags, HealthFlag{Code: "held_answer"})
		}
		if recovery {
			item.Flags = append(item.Flags, HealthFlag{Code: "recovery_waiting"})
		}
		agents = append(agents, item)
	}
	return agents, rows.Err()
}

// AgentHealth derives current flags from durable turn, inbox, and provider
// evidence. Machine connectivity is owned by the server and added there.
func (s *Store) AgentHealth(ctx context.Context, agentID string, now time.Time, stuckAfter time.Duration) ([]HealthFlag, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	flags := []HealthFlag{}
	var started, lastEvent string
	err := s.readDB.QueryRowContext(ctx, `SELECT COALESCE(t.started_at,''),COALESCE((SELECT MAX(e.created_at) FROM event e
		WHERE e.turn_id=t.id AND e.kind IN ('turn_ack_accepted','supervisor_activity','supervisor_usage','supervisor_compaction')),'')
		FROM turn t WHERE t.agent_id=? AND t.exec='running' ORDER BY t.started_at DESC LIMIT 1`, agentID).Scan(&started, &lastEvent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		last := started
		if lastEvent > last {
			last = lastEvent
		}
		if at, parseErr := time.Parse(time.RFC3339Nano, last); parseErr == nil && now.Sub(at) > stuckAfter {
			flags = append(flags, HealthFlag{Code: "stuck", Since: last})
		}
	}
	var reset sql.NullString
	var detected string
	err = s.readDB.QueryRowContext(ctx, `SELECT p.reset_at,p.detected_at FROM provider_limit p JOIN agent a ON a.id=p.agent_id
		WHERE p.agent_id=? AND p.tool=a.tool`, agentID).Scan(&reset, &detected)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && (!reset.Valid || reset.String > now.UTC().Format("2006-01-02T15:04:05.000000000Z")) {
		flags = append(flags, HealthFlag{Code: "out_of_quota", Since: detected, ResetAt: reset.String})
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT DISTINCT kind FROM inbox_item WHERE agent_id=? AND state='open'
		AND kind IN ('recovery','delivery_failed') ORDER BY kind`, agentID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			rows.Close()
			return nil, err
		}
		if kind == "recovery" {
			flags = append(flags, HealthFlag{Code: "recovery_waiting"})
		} else {
			flags = append(flags, HealthFlag{Code: "held_answer"})
		}
	}
	err = rows.Err()
	rows.Close()
	return flags, err
}

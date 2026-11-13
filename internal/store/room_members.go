package store

import (
	"context"
	"database/sql"
)

// RoomMemberHuman is one person in a room's member list. Role is the person's project role
// (owner, operator or viewer); it is empty for someone who is a room member without a project
// membership.
type RoomMemberHuman struct {
	ID    string `json:"id"`
	Login string `json:"login"`
	Role  string `json:"role"`
}

// RoomMemberAgent is one agent in a room's member list, with the role it was spawned under.
type RoomMemberAgent struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	RoleKey     string `json:"role_key"`
	RoleLabel   string `json:"role_label"`
	RoleEmoji   string `json:"role_emoji"`
	RoleDeleted bool   `json:"role_deleted,omitempty"`
	Singleton   bool   `json:"singleton"`
	Desired     string `json:"desired"`
	Observed    string `json:"observed"`
	Revoked     bool   `json:"revoked,omitempty"`
}

// RoomSingletonHolder is the agent currently holding a singleton role. InThisRoom says whether it
// is a member of the room being listed: the room it works in is deliberately not named, because it
// may be a private chat the caller cannot see.
type RoomSingletonHolder struct {
	AgentID    string `json:"agent_id"`
	Slug       string `json:"slug"`
	InThisRoom bool   `json:"in_this_room"`
}

// RoomSingleton is a singleton role of the room's project and who holds it (nil when vacant).
type RoomSingleton struct {
	RoleKey string               `json:"role_key"`
	Label   string               `json:"label"`
	Emoji   string               `json:"emoji"`
	HeldBy  *RoomSingletonHolder `json:"held_by"`
}

// RoomMembers is the read-only member list of a room.
type RoomMembers struct {
	Humans     []RoomMemberHuman `json:"humans"`
	Agents     []RoomMemberAgent `json:"agents"`
	Singletons []RoomSingleton   `json:"singletons"`
}

// RoomMembers lists who is in a room: the people (a private chat's two, or every active member of
// the project for an ordinary room, the set `@login` addresses and the room's member count uses),
// the agents in it with their role label and emoji, and the project's singleton roles with their
// holders. Nothing here changes any state; callers check room access first.
func (s *Store) RoomMembers(ctx context.Context, roomID string) (RoomMembers, error) {
	room, err := s.RoomByID(ctx, roomID)
	if err != nil {
		return RoomMembers{}, err // sql.ErrNoRows for an unknown room, which the API reports as 404
	}
	return s.RoomMembersOf(ctx, room)
}

// RoomMembersOf is RoomMembers for a room the caller has already loaded. It reads the people, the agents, and the singleton roles with
// their holders in three statements, however many of each there are.
func (s *Store) RoomMembersOf(ctx context.Context, room Room) (RoomMembers, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return RoomMembers{}, err
	}
	roomID, projectID, kind := room.ID, room.ProjectID, room.Kind
	out := RoomMembers{Humans: []RoomMemberHuman{}, Agents: []RoomMemberAgent{}, Singletons: []RoomSingleton{}}

	rows, err := s.db.QueryContext(ctx, `SELECT u.id,u.login,COALESCE(pm.role,'') FROM room_member m JOIN user u ON u.id=m.member_id
		LEFT JOIN membership pm ON pm.project_id=? AND pm.user_id=u.id
		WHERE m.room_id=? AND m.member_kind='user' AND m.left_at IS NULL AND u.disabled_at IS NULL
		UNION
		SELECT u.id,u.login,pm.role FROM membership pm JOIN user u ON u.id=pm.user_id
		WHERE pm.project_id=? AND ?!='dm' AND u.disabled_at IS NULL
		ORDER BY 2,1`, projectID, roomID, projectID, kind)
	if err != nil {
		return RoomMembers{}, err
	}
	for rows.Next() {
		var h RoomMemberHuman
		if err := rows.Scan(&h.ID, &h.Login, &h.Role); err != nil {
			rows.Close()
			return RoomMembers{}, err
		}
		out.Humans = append(out.Humans, h)
	}
	if err := rows.Close(); err != nil {
		return RoomMembers{}, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT a.id,a.slug,r.role_key,r.label,r.emoji,r.deleted_at IS NOT NULL,r.cardinality='singleton',
		a.desired,a.observed,a.revoked_at IS NOT NULL
		FROM agent a JOIN role r ON r.id=a.role_id
		JOIN room_member m ON m.member_id=a.id AND m.member_kind='agent' AND m.left_at IS NULL
		WHERE m.room_id=? ORDER BY a.created_at,a.id`, roomID)
	if err != nil {
		return RoomMembers{}, err
	}
	for rows.Next() {
		var a RoomMemberAgent
		if err := rows.Scan(&a.ID, &a.Slug, &a.RoleKey, &a.RoleLabel, &a.RoleEmoji, &a.RoleDeleted, &a.Singleton, &a.Desired, &a.Observed, &a.Revoked); err != nil {
			rows.Close()
			return RoomMembers{}, err
		}
		out.Agents = append(out.Agents, a)
	}
	if err := rows.Close(); err != nil {
		return RoomMembers{}, err
	}
	if kind == "dm" {
		return out, nil // a private chat has no roles to show
	}

	// The holder is the same "occupied" rule spawn applies: the first live agent of that role in the project.
	rows, err = s.db.QueryContext(ctx, `SELECT r.role_key,r.label,r.emoji,h.id,h.slug,
		EXISTS(SELECT 1 FROM room_member m WHERE m.room_id=? AND m.member_kind='agent' AND m.member_id=h.id AND m.left_at IS NULL)
		FROM role r LEFT JOIN agent h ON h.id=(SELECT a.id FROM agent a JOIN role ar ON ar.id=a.role_id
			WHERE a.project_id=? AND ar.role_key=r.role_key AND a.revoked_at IS NULL AND (a.desired!='stopped' OR a.observed!='stopped')
			ORDER BY a.created_at,a.id LIMIT 1)
		WHERE r.cardinality='singleton' AND r.deleted_at IS NULL
		ORDER BY r.label,r.role_key`, roomID, projectID)
	if err != nil {
		return RoomMembers{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var singleton RoomSingleton
		var holderID, holderSlug sql.NullString
		var inRoom bool
		if err := rows.Scan(&singleton.RoleKey, &singleton.Label, &singleton.Emoji, &holderID, &holderSlug, &inRoom); err != nil {
			return RoomMembers{}, err
		}
		if holderID.Valid {
			singleton.HeldBy = &RoomSingletonHolder{AgentID: holderID.String, Slug: holderSlug.String, InThisRoom: inRoom}
		}
		out.Singletons = append(out.Singletons, singleton)
	}
	return out, rows.Err()
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"
)

// AgentDMProjectID is the reserved system project that every chat between two agents hangs off. Such a chat belongs to no real project
// (its two agents usually live in different ones), but room.project_id is required and joined everywhere, so the room carries this
// member-less, never-listed project instead. The room is a private chat (kind dm) whose key starts with agentDMKeyPrefix.
const AgentDMProjectID = "prj_agentdm"

// agentDMProjectSQL is AgentDMProjectID as an SQL literal, for the queries that let an agent see its chats next to its project's rooms.
const agentDMProjectSQL = "'" + AgentDMProjectID + "'"

const agentDMKeyPrefix = "agents:"

// AgentDMTurnsPerHour is how many agent turns one chat between agents may take in an hour before an agent's message stops waking the
// other. A person's message always wakes, and people do not count.
const AgentDMTurnsPerHour = 30

// agentChatCapNotice is the system line posted once an hour when a chat reaches its turn limit.
const agentChatCapNotice = "This chat reached its limit of agent turns for the hour. The agents resume when a person writes here or the hour passes."

// IsAgentDM reports whether the room is a chat between agents.
func IsAgentDM(room Room) bool {
	return room.Kind == "dm" && room.ProjectID == AgentDMProjectID
}

// roomServesAgent says whether an agent may run turns and take notes in a room: the room is in the agent's own project, or it is a chat
// between agents (membership is checked separately where it matters).
func roomServesAgent(roomProject, agentProject string) bool {
	return roomProject == agentProject || roomProject == AgentDMProjectID
}

// agentDMKey is the one key of the chat between two agents, whoever opens it.
func agentDMKey(a, b string) string {
	ids := []string{a, b}
	sort.Strings(ids)
	return agentDMKeyPrefix + ids[0] + ":" + ids[1]
}

// firstUserSQL is the instance owner: the first account.
const firstUserSQL = `(SELECT id FROM user ORDER BY created_at,id LIMIT 1)`

// agentDMEligibleSQL says (for the room as r and the user as one placeholder) that a person still has a footing in a chat between agents:
// they are the instance owner or belong, at any role, to the project of at least one agent still in the chat. Membership of the room alone
// is not enough, so someone removed from every involved project stops seeing it without anyone editing the room.
const agentDMEligibleSQL = `EXISTS (SELECT 1 FROM user ux WHERE ux.id=? AND ux.disabled_at IS NULL AND (ux.id=` + firstUserSQL + `
	OR EXISTS (SELECT 1 FROM room_member am JOIN agent ag ON ag.id=am.member_id JOIN membership mb ON mb.project_id=ag.project_id AND mb.user_id=ux.id
		WHERE am.room_id=r.id AND am.member_kind='agent' AND am.left_at IS NULL)))`

// agentDMWriterSQL is agentDMEligibleSQL for writing: the instance owner or an owner or operator of an agent's project (a viewer reads).
const agentDMWriterSQL = `EXISTS (SELECT 1 FROM user ux WHERE ux.id=? AND ux.disabled_at IS NULL AND (ux.id=` + firstUserSQL + `
	OR EXISTS (SELECT 1 FROM room_member am JOIN agent ag ON ag.id=am.member_id JOIN membership mb ON mb.project_id=ag.project_id AND mb.user_id=ux.id AND mb.role IN ('owner','operator')
		WHERE am.room_id=r.id AND am.member_kind='agent' AND am.left_at IS NULL)))`

// userReaches reports whether a person may direct work at a project: the instance owner, or an owner or operator of it. A viewer does
// not (a chat started there makes its agent work and spend).
func userReaches(ctx context.Context, q singleRowQuerier, userID, projectID string) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM user u WHERE u.id=? AND u.disabled_at IS NULL AND (u.id=`+firstUserSQL+`
		OR EXISTS (SELECT 1 FROM membership m WHERE m.user_id=u.id AND m.project_id=? AND m.role IN ('owner','operator'))))`, userID, projectID).Scan(&ok)
	return ok, err
}

// OpenAgentDM returns the one chat between two agents, creating it (with both agents as members) the first time. created says whether
// this call made it. Both agents must be live (not revoked).
func (tx *Tx) OpenAgentDM(agentA, agentB, createdBy string) (room Room, created bool, err error) {
	if agentA == agentB || agentA == "" || agentB == "" {
		return Room{}, false, ErrInvalidTransition
	}
	type peer struct{ id, slug, project string }
	var peers [2]peer
	ids := []string{agentA, agentB}
	sort.Strings(ids)
	for i, id := range ids {
		peers[i].id = id
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.slug,p.name FROM agent a JOIN project p ON p.id=a.project_id WHERE a.id=? AND a.revoked_at IS NULL`, id).
			Scan(&peers[i].slug, &peers[i].project); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Room{}, false, ErrForbidden
			}
			return Room{}, false, err
		}
	}
	key := agentDMKey(agentA, agentB)
	var roomID string
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE dm_key=?`, key).Scan(&roomID)
	if err == nil {
		room, err = tx.roomByID(roomID)
		if err != nil {
			return Room{}, false, err
		}
		if room.Lifecycle != "open" {
			return room, false, nil
		}
		for _, p := range peers {
			if err := tx.addMemberOnly(room.ID, "agent", p.id, false); err != nil {
				return Room{}, false, err
			}
		}
		return room, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Room{}, false, err
	}
	title := peers[0].slug + "@" + peers[0].project + " ⇄ " + peers[1].slug + "@" + peers[1].project
	room, err = tx.CreateRoom(Room{ProjectID: AgentDMProjectID, Kind: "dm", DMKey: key, Title: title, CreatedBy: createdBy})
	if err != nil {
		return Room{}, false, err
	}
	for _, p := range peers {
		if err := tx.addMemberOnly(room.ID, "agent", p.id, false); err != nil {
			return Room{}, false, err
		}
	}
	return room, true, nil
}

func (tx *Tx) roomByID(id string) (Room, error) {
	var room Room
	var parent, dm sql.NullString
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT id,project_id,parent_id,depth,kind,dm_key,title,created_by,lifecycle,gate,lifecycle_rev FROM room WHERE id=?`, id).
		Scan(&room.ID, &room.ProjectID, &parent, &room.Depth, &room.Kind, &dm, &room.Title, &room.CreatedBy, &room.Lifecycle, &room.Gate, &room.LifecycleRev)
	room.ParentID, room.DMKey = parent.String, dm.String
	return room, err
}

// AgentDMWriteAllowed says whether a person may post in a chat between agents (they are a member and reach an agent's project).
func (s *Store) AgentDMWriteAllowed(ctx context.Context, userID, roomID string) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM room r JOIN room_member own ON own.room_id=r.id AND own.member_kind='user' AND own.member_id=? AND own.left_at IS NULL
		WHERE r.id=? AND r.project_id=`+agentDMProjectSQL+` AND `+agentDMWriterSQL+`)`, userID, roomID, userID).Scan(&ok)
	if err != nil || !ok {
		return false, err
	}
	// Read-only once no agent in it is in a live project.
	archived, known, err := s.agentChatArchived(ctx, userID, "room", roomID)
	return !(known && archived), err
}

// AgentDMsForUser lists the chats between agents the person is a member of and still has a footing in, with the usual list fields.
func (s *Store) AgentDMsForUser(ctx context.Context, userID string) ([]Room, error) {
	rooms, err := s.roomsForUserProject(ctx, AgentDMProjectID, userID, "")
	if err != nil {
		return nil, err
	}
	out := rooms[:0]
	for _, room := range rooms {
		var ok bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM room r WHERE r.id=? AND `+agentDMEligibleSQL+`)`, room.ID, userID).Scan(&ok); err != nil {
			return nil, err
		}
		if ok {
			out = append(out, room)
		}
	}
	return out, nil
}

// agentChatArchived answers ArchivedScope for a chat between agents (ok is false for any other room): it is read-only when no agent
// still in it belongs to a live project, and only a person with a footing in it is told.
func (s *Store) agentChatArchived(ctx context.Context, userID, kind, id string) (archived, ok bool, err error) {
	roomID := id
	if kind == "message" {
		if err := s.db.QueryRowContext(ctx, `SELECT room_id FROM message WHERE id=?`, id).Scan(&roomID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, false, nil
			}
			return false, false, err
		}
	}
	err = s.db.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM room_member am JOIN agent ag ON ag.id=am.member_id JOIN project ap ON ap.id=ag.project_id
		WHERE am.room_id=r.id AND am.member_kind='agent' AND am.left_at IS NULL AND ap.archived_at IS NULL)
		FROM room r WHERE r.id=? AND r.project_id=`+agentDMProjectSQL+` AND `+agentDMEligibleSQL, roomID, userID).Scan(&archived)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return archived, err == nil, err
}

// detachAgentsFromChats takes a project's agents out of every chat between agents before the project is deleted: each chat gets a note,
// the agents leave it, and a chat left with fewer than two agents is closed. The messages stay.
func (tx *Tx) detachAgentsFromChats(projectID, projectName string) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT DISTINCT r.id FROM room r JOIN room_member m ON m.room_id=r.id AND m.member_kind='agent' AND m.left_at IS NULL
		JOIN agent a ON a.id=m.member_id WHERE r.project_id=`+agentDMProjectSQL+` AND a.project_id=? ORDER BY r.id`, projectID)
	if err != nil {
		return err
	}
	var chats []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		chats = append(chats, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range chats {
		if _, err := tx.InsertMessage(Message{RoomID: id, AuthorKind: "system", AuthorID: "system",
			Body: "The agents of project " + projectName + " were removed with it and have left this chat."}); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE room_member SET left_at=? WHERE room_id=? AND member_kind='agent' AND left_at IS NULL
			AND member_id IN (SELECT id FROM agent WHERE project_id=?)`, timestamp(), id, projectID); err != nil {
			return err
		}
		var left int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND left_at IS NULL`, id).Scan(&left); err != nil {
			return err
		}
		if left < 2 {
			if _, err := tx.Exec(`UPDATE room SET lifecycle='closed',closed_at=?,lifecycle_rev=lifecycle_rev+1 WHERE id=?`, timestamp(), id); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(Event{Kind: "room_closed", ProjectID: AgentDMProjectID, RoomID: id, PayloadJSON: payloadJSON(map[string]string{"room_id": id})}); err != nil {
				return err
			}
		}
	}
	return nil
}

// agentChatTurns counts the turns a chat has taken in the last hour, including one that is queued or running.
func (tx *Tx) agentChatTurns(roomID string) (int, error) {
	cutoff := time.Now().UTC().Add(-time.Hour).Format(timestampLayout)
	var turns int
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE room_id=? AND (exec IN ('pending','running') OR started_at>=?)`, roomID, cutoff).Scan(&turns)
	return turns, err
}

// announceAgentChatCap says once an hour that a chat stopped waking its agents.
func (tx *Tx) announceAgentChatCap(roomID string) error {
	cutoff := time.Now().UTC().Add(-time.Hour).Format(timestampLayout)
	var said int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='system' AND body=? AND created_at>=?`, roomID, agentChatCapNotice, cutoff).Scan(&said); err != nil {
		return err
	}
	if said > 0 {
		return nil
	}
	_, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: agentChatCapNotice})
	return err
}

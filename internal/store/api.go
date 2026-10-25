package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

var ErrForbidden = errors.New("forbidden")

// Project access is scoped by the existing membership table; grant management
// arrives with the later membership slice.
func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return User{}, err
	}
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT id,login FROM user WHERE id=?`, id).Scan(&u.ID, &u.Login)
	return u, err
}

func (s *Store) ProjectsForUser(ctx context.Context, userID string) ([]Project, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.id,p.name,p.folder,COALESCE(p.repo_url,''),m.role,COALESCE(p.archived_at,'')
		FROM project p JOIN membership m ON m.project_id=p.id WHERE m.user_id=? ORDER BY p.created_at,p.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Folder, &p.RepoURL, &p.Role, &p.ArchivedAt); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (s *Store) ProjectAccess(ctx context.Context, userID, projectID string) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT role FROM membership WHERE user_id=? AND project_id=?`, userID, projectID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrForbidden
	}
	return err
}

// ProjectByID is one project's own fields (not the caller's role).
func (s *Store) ProjectByID(ctx context.Context, projectID string) (Project, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return Project{}, err
	}
	var project Project
	err := s.db.QueryRowContext(ctx, `SELECT id, name, folder, COALESCE(repo_url,''), COALESCE(archived_at,'') FROM project WHERE id=?`, projectID).Scan(&project.ID, &project.Name, &project.Folder, &project.RepoURL, &project.ArchivedAt)
	return project, err
}

func (s *Store) ProjectFolder(ctx context.Context, projectID string) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var folder string
	err := s.db.QueryRowContext(ctx, `SELECT folder FROM project WHERE id=?`, projectID).Scan(&folder)
	return folder, err
}

func (tx *Tx) AddProjectOwner(projectID, userID string) error {
	_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, projectID, userID)
	if err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "project_member_joined", ProjectID: projectID, PayloadJSON: payloadJSON(map[string]string{"user_id": userID})})
	return err
}

func (s *Store) RoomByID(ctx context.Context, id string) (Room, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return Room{}, err
	}
	var r Room
	var parent, dm, closed sql.NullString
	var memory sql.NullBool
	err := s.db.QueryRowContext(ctx, `SELECT id,project_id,parent_id,depth,kind,dm_key,title,created_by,lifecycle,gate,lifecycle_rev,closed_at,memory_enabled FROM room WHERE id=?`, id).Scan(&r.ID, &r.ProjectID, &parent, &r.Depth, &r.Kind, &dm, &r.Title, &r.CreatedBy, &r.Lifecycle, &r.Gate, &r.LifecycleRev, &closed, &memory)
	r.ParentID = parent.String
	r.DMKey = dm.String
	r.ClosedAt = closed.String
	if memory.Valid {
		r.MemoryEnabled = &memory.Bool
	}
	return r, err
}

func (s *Store) RoomLifecycle(ctx context.Context, id string) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT lifecycle FROM room WHERE id=?`, id).Scan(&state)
	return state, err
}

func (s *Store) ReactionsForMessage(ctx context.Context, id string) ([]map[string]string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.member_kind,r.member_id,r.emoji,
		COALESCE(u.login,a.slug,r.member_id) FROM reaction r
		LEFT JOIN user u ON r.member_kind='user' AND u.id=r.member_id
		LEFT JOIN agent a ON r.member_kind='agent' AND a.id=r.member_id
		WHERE r.message_id=? ORDER BY r.emoji,r.member_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var kind, member, emoji, name string
		if err := rows.Scan(&kind, &member, &emoji, &name); err != nil {
			return nil, err
		}
		out = append(out, map[string]string{"member_kind": kind, "member_id": member, "member_name": name, "emoji": emoji})
	}
	return out, rows.Err()
}

// roomMemberCountSQL counts a room's members for room lists: the two people of
// a private chat, or every active member of the project for an ordinary room
// (they can all read it and are addressable by login), plus the agents in the
// room. A disabled user is nobody's member. It expects the room as r.
const roomMemberCountSQL = `(CASE WHEN r.kind='dm' THEN (SELECT COUNT(*) FROM room_member um JOIN user uu ON uu.id=um.member_id
			WHERE um.room_id=r.id AND um.member_kind='user' AND um.left_at IS NULL AND uu.disabled_at IS NULL)
		ELSE (SELECT COUNT(*) FROM membership mb JOIN user mu ON mu.id=mb.user_id
			WHERE mb.project_id=r.project_id AND mu.disabled_at IS NULL) END) +
		(SELECT COUNT(*) FROM room_member rm WHERE rm.room_id=r.id AND rm.member_kind='agent' AND rm.left_at IS NULL)`

func (s *Store) RoomsForProject(ctx context.Context, projectID string) ([]Room, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.project_id,r.parent_id,r.depth,r.kind,r.dm_key,r.title,r.created_by,
		r.lifecycle,r.gate,r.lifecycle_rev,r.closed_at,r.memory_enabled,
		`+roomMemberCountSQL+`,
		(SELECT COUNT(*) FROM message m WHERE m.room_id=r.id),
		COALESCE((SELECT substr(m.body,1,160) FROM message m WHERE m.room_id=r.id ORDER BY m.seq DESC LIMIT 1),''),
		COALESCE((SELECT m.created_at FROM message m WHERE m.room_id=r.id ORDER BY m.seq DESC LIMIT 1),r.created_at)
		FROM room r WHERE r.project_id=? ORDER BY r.depth,r.created_at,r.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Room{}
	for rows.Next() {
		var r Room
		var parent, dm, closed sql.NullString
		var memory sql.NullBool
		if err := rows.Scan(&r.ID, &r.ProjectID, &parent, &r.Depth, &r.Kind, &dm, &r.Title, &r.CreatedBy,
			&r.Lifecycle, &r.Gate, &r.LifecycleRev, &closed, &memory, &r.MemberCount, &r.MessageCount, &r.LastMessageBody, &r.LastActivityAt); err != nil {
			return nil, err
		}
		r.ParentID = parent.String
		r.DMKey = dm.String
		r.ClosedAt = closed.String
		if memory.Valid {
			r.MemoryEnabled = &memory.Bool
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoomsForUserProject keeps private rooms out of another member's sidebar.
func (s *Store) RoomsForUserProject(ctx context.Context, projectID, userID string) ([]Room, error) {
	return s.roomsForUserProject(ctx, projectID, userID, "")
}

// RoomForUserProject uses the same counts and view preferences as the room list.
func (s *Store) RoomForUserProject(ctx context.Context, projectID, userID, roomID string) (Room, error) {
	rooms, err := s.roomsForUserProject(ctx, projectID, userID, roomID)
	if err != nil {
		return Room{}, err
	}
	if len(rooms) == 0 {
		return Room{}, sql.ErrNoRows
	}
	return rooms[0], nil
}

func (s *Store) roomsForUserProject(ctx context.Context, projectID, userID, roomID string) ([]Room, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.project_id,r.parent_id,r.depth,r.kind,r.dm_key,r.title,r.created_by,
		r.lifecycle,r.gate,r.lifecycle_rev,r.closed_at,r.memory_enabled,
		`+roomMemberCountSQL+`,
		(SELECT COUNT(*) FROM message m WHERE m.room_id=r.id),
		COALESCE((SELECT substr(m.body,1,160) FROM message m WHERE m.room_id=r.id ORDER BY m.seq DESC LIMIT 1),''),
		COALESCE((SELECT m.created_at FROM message m WHERE m.room_id=r.id ORDER BY m.seq DESC LIMIT 1),r.created_at),
		COALESCE(v.hidden,0),COALESCE(v.notify,'default'),COALESCE(v.muted_until,''),COALESCE(c.last_read_seq,0),
		(SELECT COUNT(*) FROM message um WHERE um.room_id=r.id AND um.seq>COALESCE(c.last_read_seq,0)
			AND (COALESCE(v.notify,'default')<>'mentions' OR EXISTS (SELECT 1 FROM json_each(um.targets_json) WHERE value=?)))
		FROM room r LEFT JOIN room_view_preference v ON v.room_id=r.id AND v.user_id=?
		LEFT JOIN room_read_cursor c ON c.room_id=r.id AND c.user_id=?
		WHERE r.project_id=? AND (?='' OR r.id=?) AND (r.kind!='dm' OR EXISTS (
			SELECT 1 FROM room_member own WHERE own.room_id=r.id AND own.member_kind='user'
			AND own.member_id=? AND own.left_at IS NULL)) ORDER BY r.depth,r.created_at,r.id`, userID, userID, userID, projectID, roomID, roomID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Room{}
	for rows.Next() {
		var r Room
		var parent, dm, closed sql.NullString
		var memory sql.NullBool
		if err := rows.Scan(&r.ID, &r.ProjectID, &parent, &r.Depth, &r.Kind, &dm, &r.Title, &r.CreatedBy,
			&r.Lifecycle, &r.Gate, &r.LifecycleRev, &closed, &memory, &r.MemberCount, &r.MessageCount, &r.LastMessageBody, &r.LastActivityAt, &r.Hidden, &r.Notify, &r.MutedUntil, &r.ReadCursor, &r.UnreadCount); err != nil {
			return nil, err
		}
		r.ParentID, r.DMKey, r.ClosedAt = parent.String, dm.String, closed.String
		if memory.Valid {
			r.MemoryEnabled = &memory.Bool
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	attention, err := s.attentionByRoom(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].AttentionCount = attention[out[i].ID]
	}
	if err := s.showPeerTitles(ctx, out, userID); err != nil {
		return nil, err
	}
	return out, nil
}

// showPeerTitles gives each conversation between two people the other person's name for this viewer. The stored title lists both logins
// (it is the same row for both people), so each viewer would otherwise see their own name next to the peer's. One query covers every such
// conversation in the list.
func (s *Store) showPeerTitles(ctx context.Context, rooms []Room, userID string) error {
	wanted := map[string]int{}
	for i, room := range rooms {
		if room.Kind == "dm" && strings.HasPrefix(room.DMKey, "users:") {
			wanted[room.ID] = i
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(wanted)), ",")
	args := []any{userID}
	for id := range wanted {
		args = append(args, id)
	}
	// A peer who has left sorts after one who is still in the room.
	rows, err := s.db.QueryContext(ctx, `SELECT m.room_id, u.login FROM room_member m JOIN user u ON u.id=m.member_id
		WHERE m.member_kind='user' AND m.member_id<>? AND m.room_id IN (`+marks+`) ORDER BY m.left_at IS NOT NULL DESC`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var roomID, login string
		if err := rows.Scan(&roomID, &login); err != nil {
			return err
		}
		rooms[wanted[roomID]].Title = login
	}
	return rows.Err()
}

// RoomForViewer is the room as one person sees it: a conversation between two people is titled with the other person.
func (s *Store) RoomForViewer(ctx context.Context, room Room, userID string) (Room, error) {
	rooms := []Room{room}
	err := s.showPeerTitles(ctx, rooms, userID)
	return rooms[0], err
}

func (s *Store) MessageByID(ctx context.Context, id string) (Message, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return Message{}, err
	}
	var m Message
	var threadRoot sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,room_id,thread_root FROM message WHERE id=?`, id).Scan(&m.ID, &m.RoomID, &threadRoot)
	m.ThreadRoot = threadRoot.String
	return m, err
}

func (tx *Tx) SetReaction(messageID, kind, memberID, emoji string, active bool) error {
	var projectID, roomID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.project_id,r.id FROM message m JOIN room r ON r.id=m.room_id WHERE m.id=?`, messageID).Scan(&projectID, &roomID); err != nil {
		return err
	}
	if active {
		result, err := tx.Exec(`INSERT OR IGNORE INTO reaction(message_id,member_kind,member_id,emoji,created_at) VALUES (?,?,?,?,?)`, messageID, kind, memberID, emoji, timestamp())
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted == 0 {
			return nil
		}
	} else {
		result, err := tx.Exec(`DELETE FROM reaction WHERE message_id=? AND member_kind=? AND member_id=? AND emoji=?`, messageID, kind, memberID, emoji)
		if err != nil {
			return err
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if deleted == 0 {
			return nil
		}
	}
	_, err := tx.AppendEvent(Event{Kind: "reaction_changed", ProjectID: projectID, RoomID: roomID, PayloadJSON: payloadJSON(map[string]string{"message_id": messageID, "emoji": emoji, "member_id": memberID})})
	return err
}

func (s *Store) EventBounds(ctx context.Context) (min, max int64, err error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return 0, 0, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(seq),0),COALESCE(MAX(seq),0) FROM event`).Scan(&min, &max)
	return
}

// eventVisible is the one rule for who may see an event, shared by the live
// stream and the system-events history. It takes three user-ID arguments after
// the membership join's own: the private-chat membership check, the owner
// check, then the removed-project check (a removed project has no members left
// to join on, so its removal event names the people it is for). A project event is visible to project members, with private
// chats limited to their members (owners and operators also see recovery
// inbox changes). An event with no project (user, machine, session, and agent
// key changes) is visible only to the instance owner, the first user, except
// the per-login session and password-rehash bookkeeping, which nobody streams.
const eventVisible = `((m.user_id IS NOT NULL AND (r.kind IS NULL OR r.kind!='dm' OR EXISTS (SELECT 1 FROM room_member own WHERE own.room_id=r.id AND own.member_kind='user' AND own.member_id=? AND own.left_at IS NULL) OR (m.role IN ('owner','operator') AND e.kind IN ('inbox_item_created','inbox_item_updated','inbox_item_resolved') AND EXISTS (SELECT 1 FROM inbox_item i WHERE i.id=json_extract(e.payload_json,'$.inbox_item_id') AND i.kind='recovery')))) OR (COALESCE(e.project_id,'')='' AND e.kind NOT IN ('auth_session_created','auth_session_revoked','user_password_rehashed') AND ?=(SELECT id FROM user ORDER BY created_at,id LIMIT 1))
	OR (e.kind='project_removed' AND e.project_id IS NULL AND EXISTS (SELECT 1 FROM json_each(e.payload_json,'$.user_ids') WHERE value=?)))`

func (s *Store) EventsAfter(ctx context.Context, userID string, after, through int64) ([]Event, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.seq,e.kind,COALESCE(e.project_id,''),COALESCE(e.room_id,''),COALESCE(e.agent_id,''),COALESCE(e.turn_id,''),e.payload_json,e.created_at FROM event e LEFT JOIN membership m ON m.project_id=e.project_id AND m.user_id=? LEFT JOIN room r ON r.id=e.room_id WHERE e.seq>? AND e.seq<=? AND `+eventVisible+` ORDER BY e.seq`, userID, after, through, userID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Seq, &e.Kind, &e.ProjectID, &e.RoomID, &e.AgentID, &e.TurnID, &e.PayloadJSON, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) EventsBatchAfter(ctx context.Context, userID string, after, through int64, limit int) ([]Event, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 5000 {
		return nil, errors.New("invalid event batch limit")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.seq,e.kind,COALESCE(e.project_id,''),COALESCE(e.room_id,''),COALESCE(e.agent_id,''),COALESCE(e.turn_id,''),e.payload_json,e.created_at FROM event e LEFT JOIN membership m ON m.project_id=e.project_id AND m.user_id=? LEFT JOIN room r ON r.id=e.room_id WHERE e.seq>? AND e.seq<=? AND `+eventVisible+` ORDER BY e.seq LIMIT ?`, userID, after, through, userID, userID, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Seq, &e.Kind, &e.ProjectID, &e.RoomID, &e.AgentID, &e.TurnID, &e.PayloadJSON, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RoomUserLogins lists the human logins addressed by `@login` mentions in a
// room: its explicit user members plus, outside private chats, the rest of the
// project's humans. It is the same set server addressing resolves against.
func (s *Store) RoomUserLogins(ctx context.Context, roomID string) ([]string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	users, err := roomUserRows(s.db.QueryContext, ctx, roomID)
	if err != nil {
		return nil, err
	}
	logins := make([]string, 0, len(users))
	for _, user := range users {
		logins = append(logins, user.login)
	}
	return logins, nil
}

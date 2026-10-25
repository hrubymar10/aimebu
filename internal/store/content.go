package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/goccy/go-json"
)

type Project struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Folder  string `json:"folder"`
	RepoURL string `json:"repo_url"`
	// Role is the caller's role in the project (owner, operator, viewer); it is only filled for a user's own project list.
	Role string `json:"role,omitempty"`
	// ArchivedAt is when the project was archived (read-only, agents stopped); empty for a live project.
	ArchivedAt string `json:"archived_at,omitempty"`
}

func (s *Store) CreateProject(ctx context.Context, project Project) (Project, error) {
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		project, err = tx.CreateProject(project)
		return err
	})
	return project, err
}

func (tx *Tx) CreateProject(project Project) (Project, error) {
	var err error
	project.ID, err = newID("prj")
	if err != nil {
		return Project{}, err
	}
	_, err = tx.tx.ExecContext(tx.ctx, `INSERT INTO project
		(id, name, folder, repo_url, created_at)
		VALUES (?, ?, ?, ?, ?)`, project.ID, project.Name, project.Folder,
		nullable(project.RepoURL), timestamp())
	if err != nil {
		return Project{}, err
	}
	if _, err = tx.tx.ExecContext(tx.ctx,
		`INSERT INTO settings_project(project_id) VALUES (?)`, project.ID); err != nil {
		return Project{}, err
	}
	if _, err = tx.AppendEvent(Event{Kind: "project_created", ProjectID: project.ID,
		PayloadJSON: payloadJSON(map[string]string{"project_id": project.ID})}); err != nil {
		return Project{}, err
	}
	return project, nil
}

// ErrProjectFolderInUse is a folder change on a project that already has agents or worktrees: they live under the old folder.
var ErrProjectFolderInUse = errors.New("project has agents or worktrees")

// UpdateProject renames a project and/or moves its folder. The folder can only change while the project has no agents and no live
// worktree. A rename also renames the home room when it still carries the old name (it is created with the project's name; a home room
// somebody retitled keeps its title).
func (s *Store) UpdateProject(ctx context.Context, id string, name, folder *string, actorID string) (Project, error) {
	var project Project
	err := s.Tx(ctx, func(tx *Tx) (err error) {
		project, err = tx.updateProject(id, name, folder, actorID, false)
		return err
	})
	return project, err
}

// updateProject is the one path that changes a project's name or folder, whether it comes from Settings → Project or from the home room's
// title. renameHome sets the home room's title to the new name whatever it was; otherwise the home room follows only when it still carries
// the old name.
func (tx *Tx) updateProject(id string, name, folder *string, actorID string, renameHome bool) (Project, error) {
	var project Project
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id, name, folder, COALESCE(repo_url,'') FROM project WHERE id=? AND system=0`, id).Scan(&project.ID, &project.Name, &project.Folder, &project.RepoURL); err != nil {
		return project, err
	}
	oldName := project.Name
	if folder != nil && *folder != project.Folder {
		var inUse int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT
			(SELECT COUNT(*) FROM agent WHERE project_id=?1) +
			(SELECT COUNT(*) FROM room_worktree w JOIN room r ON r.id=w.room_id WHERE r.project_id=?1 AND w.state<>'removed')`, id).Scan(&inUse); err != nil {
			return project, err
		}
		if inUse > 0 {
			return project, ErrProjectFolderInUse
		}
		project.Folder = *folder
	}
	if name != nil {
		project.Name = *name
	}
	if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE project SET name=?, folder=? WHERE id=?`, project.Name, project.Folder, id); err != nil {
		return project, err
	}
	if project.Name != oldName || renameHome {
		if _, err := tx.tx.ExecContext(tx.ctx, `UPDATE room SET title=? WHERE project_id=? AND kind='home' AND (title=? OR ?)`, project.Name, id, oldName, renameHome); err != nil {
			return project, err
		}
	}
	_, err := tx.AppendEvent(Event{Kind: "project_updated", ProjectID: id, PayloadJSON: payloadJSON(map[string]string{"project_id": id, "actor_id": actorID})})
	return project, err
}

type Room struct {
	ID              string        `json:"id"`
	ProjectID       string        `json:"project_id"`
	ParentID        string        `json:"parent_id,omitempty"`
	Depth           int           `json:"depth"`
	Kind            string        `json:"kind"`
	DMKey           string        `json:"dm_key,omitempty"`
	Title           string        `json:"title"`
	CreatedBy       string        `json:"created_by"`
	Lifecycle       string        `json:"lifecycle"`
	Gate            string        `json:"gate"`
	LifecycleRev    int64         `json:"lifecycle_rev"`
	ClosedAt        string        `json:"closed_at,omitempty"`
	MemberCount     int           `json:"member_count"`
	MessageCount    int           `json:"message_count"`
	MemoryEnabled   *bool         `json:"memory_enabled"`
	LastMessageBody string        `json:"last_message_body,omitempty"`
	LastActivityAt  string        `json:"last_activity_at,omitempty"`
	Hidden          bool          `json:"hidden"`
	Notify          string        `json:"notify"`
	MutedUntil      string        `json:"muted_until,omitempty"`
	ReadCursor      int64         `json:"read_cursor"`
	UnreadCount     int           `json:"unread_count"`
	AttentionCount  int           `json:"attention_count"`
	Worktree        *RoomWorktree `json:"worktree,omitempty"`
	// AgentChat marks a private chat between two agents, which belongs to no project (ProjectID is the reserved one). CanWrite, set when
	// a person's room list is read, says whether that person may post in it: they need a role to write in an agent's project, and the
	// chat must not be archived.
	AgentChat bool  `json:"agent_chat,omitempty"`
	CanWrite  *bool `json:"can_write,omitempty"`
}

// MarshalJSON gives a room that no list has described yet (just created, or answered by a tool) the default notification level, so the
// field never reads as empty.
func (r Room) MarshalJSON() ([]byte, error) {
	type plain Room
	if r.Notify == "" {
		r.Notify = "default"
	}
	r.AgentChat = IsAgentDM(r)
	return json.Marshal(plain(r))
}

func (s *Store) CreateRoom(ctx context.Context, room Room) (Room, error) {
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		room, err = tx.CreateRoom(room)
		return err
	})
	return room, err
}

// GeneralRoomTitle is the task room every project starts with beside its home room.
const GeneralRoomTitle = "general"

func (tx *Tx) CreateRoom(room Room) (Room, error) {
	if room.ParentID != "" {
		var parentProject, parentLifecycle, parentKind string
		if err := tx.tx.QueryRowContext(tx.ctx,
			`SELECT project_id, depth, lifecycle, kind FROM room WHERE id = ?`, room.ParentID).
			Scan(&parentProject, &room.Depth, &parentLifecycle, &parentKind); err != nil {
			return Room{}, err
		}
		if parentProject != room.ProjectID {
			return Room{}, errors.New("parent room belongs to another project")
		}
		if parentLifecycle != "open" || parentKind == "dm" {
			return Room{}, ErrInvalidTransition
		}
		room.Depth++
	} else {
		room.Depth = 0
	}
	if room.Depth > tx.maxDepth || room.Depth > hardMaxDepth {
		return Room{}, ErrDepthExceeded
	}
	var err error
	room.ID, err = newID("room")
	if err != nil {
		return Room{}, err
	}
	_, err = tx.tx.ExecContext(tx.ctx, `INSERT INTO room
		(id, project_id, parent_id, depth, kind, dm_key, title, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, room.ID, room.ProjectID,
		nullable(room.ParentID), room.Depth, room.Kind, nullable(room.DMKey),
		room.Title, room.CreatedBy, timestamp())
	if err != nil {
		return Room{}, err
	}
	if _, err = tx.AppendEvent(Event{Kind: "room_created", ProjectID: room.ProjectID,
		RoomID: room.ID, PayloadJSON: payloadJSON(map[string]string{"room_id": room.ID})}); err != nil {
		return Room{}, err
	}
	room.Lifecycle = "open"
	room.Gate = "open"
	// The project's lead is in every room of its project, so it can read what happens in a child room it did not make.
	if room.Kind != "dm" {
		if err := tx.joinLeadsToRoom(room.ID, room.ProjectID); err != nil {
			return Room{}, err
		}
	}
	return room, nil
}

// joinLeadsToRoom makes every live lead agent of the project a member of the room (each it already belongs to is left alone).
func (tx *Tx) joinLeadsToRoom(roomID, projectID string) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT a.id FROM agent a JOIN role r ON r.id=a.role_id
		WHERE a.project_id=? AND a.revoked_at IS NULL AND r.cap_is_lead=1
		AND NOT EXISTS (SELECT 1 FROM room_member m WHERE m.room_id=? AND m.member_kind='agent' AND m.member_id=a.id AND m.left_at IS NULL)`, projectID, roomID)
	if err != nil {
		return err
	}
	var leads []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		leads = append(leads, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range leads {
		if err := tx.addMemberOnly(roomID, "agent", id, true); err != nil {
			return err
		}
	}
	return nil
}

// joinLeadToProjectRooms puts a lead agent into every open room of its project that it is not in yet (a lead spawned or made lead after
// rooms exist). Private chats are not project rooms for this.
func (tx *Tx) joinLeadToProjectRooms(agentID string) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT r.id FROM agent a JOIN role ro ON ro.id=a.role_id JOIN room r ON r.project_id=a.project_id
		WHERE a.id=? AND a.revoked_at IS NULL AND ro.cap_is_lead=1 AND r.kind<>'dm' AND r.lifecycle='open'
		AND NOT EXISTS (SELECT 1 FROM room_member m WHERE m.room_id=r.id AND m.member_kind='agent' AND m.member_id=a.id AND m.left_at IS NULL)
		ORDER BY r.created_at,r.id`, agentID)
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
	for _, id := range rooms {
		if err := tx.addMemberOnly(id, "agent", agentID, true); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) AddMember(ctx context.Context, roomID, kind, memberID string) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.AddMember(roomID, kind, memberID) })
}

func (tx *Tx) AddMember(roomID, kind, memberID string) error {
	if err := tx.addMemberOnly(roomID, kind, memberID, false); err != nil {
		return err
	}
	if kind == "agent" {
		return tx.joinLeadToProjectRooms(memberID)
	}
	return nil
}

// addMemberOnly is AddMember without the lead's follow-up joins (those use it, so they cannot call each other). An automatic join (auto) is
// the lead being put into a project room it was not asked into: it is quiet (no join event, nothing un-hidden for anyone) and the member
// is marked so that it is not woken by every unaddressed human message (see queueMessageWakes). Joining on purpose a room the lead is
// already in by automatic join makes it an ordinary member.
func (tx *Tx) addMemberOnly(roomID, kind, memberID string, auto bool) error {
	var already, wasAuto int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*),COALESCE(MAX(auto),0) FROM room_member WHERE room_id=? AND member_kind=? AND member_id=? AND left_at IS NULL`, roomID, kind, memberID).Scan(&already, &wasAuto); err != nil {
		return err
	}
	if already > 0 && kind == "agent" {
		if !auto && wasAuto == 1 {
			_, err := tx.tx.ExecContext(tx.ctx, `UPDATE room_member SET auto=0 WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, roomID, memberID)
			return err
		}
		return nil
	}
	var projectID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM room WHERE id=?`, roomID).Scan(&projectID); err != nil {
		return err
	}
	autoValue := 0
	if auto {
		autoValue = 1
	}
	if _, err := tx.tx.ExecContext(tx.ctx, `INSERT INTO room_member
		(room_id, member_kind, member_id, joined_at, auto) VALUES (?, ?, ?, ?, ?)`,
		roomID, kind, memberID, timestamp(), autoValue); err != nil {
		return err
	}
	if auto {
		return nil
	}
	if _, err := tx.Exec(`UPDATE room_view_preference SET hidden=0 WHERE room_id=? AND hidden=1`, roomID); err != nil {
		return err
	}
	if kind == "agent" {
		if _, err := tx.Exec(`UPDATE agent SET home_room_id=? WHERE id=? AND home_room_id IS NULL
			AND (SELECT kind FROM room WHERE id=?)!='dm'`, roomID, memberID, roomID); err != nil {
			return err
		}
	}
	_, err := tx.AppendEvent(Event{Kind: "room_member_joined", ProjectID: projectID, RoomID: roomID,
		PayloadJSON: payloadJSON(map[string]string{"member_kind": kind, "member_id": memberID})})
	return err
}

func (s *Store) RemoveMember(ctx context.Context, roomID, kind, memberID string) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.RemoveMember(roomID, kind, memberID) })
}

func (tx *Tx) RemoveMember(roomID, kind, memberID string) error {
	var projectID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM room WHERE id=?`, roomID).Scan(&projectID); err != nil {
		return err
	}
	result, err := tx.tx.ExecContext(tx.ctx, `UPDATE room_member SET left_at = ?
		WHERE room_id = ? AND member_kind = ? AND member_id = ? AND left_at IS NULL`,
		timestamp(), roomID, kind, memberID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	if kind == "agent" {
		result, err := tx.Exec(`UPDATE note SET state='dropped' WHERE room_id=? AND agent_id=? AND state='queued'`, roomID, memberID)
		if err != nil {
			return err
		}
		dropped, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if dropped > 0 {
			if _, err := tx.AppendEvent(Event{Kind: "notes_dropped", ProjectID: projectID, RoomID: roomID, AgentID: memberID,
				PayloadJSON: payloadJSON(map[string]string{"reason": "left_room", "count": fmt.Sprint(dropped)})}); err != nil {
				return err
			}
		}
	}
	_, err = tx.AppendEvent(Event{Kind: "room_member_left", ProjectID: projectID, RoomID: roomID,
		PayloadJSON: payloadJSON(map[string]string{"member_kind": kind, "member_id": memberID})})
	return err
}

type Message struct {
	ID  string `json:"id"`
	Seq int64  `json:"seq"`
	// No is the message's number, "#N" to people and agents: one sequence across every room, counted by messages alone. Seq is the
	// internal order shared with events (paging, read cursors, the event stream).
	No                  int64        `json:"no"`
	RoomID              string       `json:"room_id"`
	AuthorKind          string       `json:"author_kind"`
	AuthorID            string       `json:"author_id"`
	Body                string       `json:"body"`
	ReplyTo             string       `json:"reply_to,omitempty"`
	ThreadRoot          string       `json:"thread_root,omitempty"`
	TargetsJSON         string       `json:"targets_json"`
	AskJSON             string       `json:"ask_json,omitempty"`
	NeedsAttention      bool         `json:"needs_attention,omitempty"`
	ProposedAnswersJSON string       `json:"-"`
	OpenQuestionsJSON   string       `json:"-"`
	VisualPlanJSON      string       `json:"-"`
	AppendixPagesJSON   string       `json:"-"`
	SourceOutID         string       `json:"source_out_id,omitempty"`
	CreatedAt           string       `json:"created_at"`
	Handoff             bool         `json:"-"`
	OnlyTargets         []string     `json:"-"`
	AttachmentIDs       []string     `json:"-"`
	Attachments         []Attachment `json:"attachments,omitempty"`
	AuthorHarness       string       `json:"author_harness,omitempty"`
	AuthorModel         string       `json:"author_model,omitempty"`
	// AuthorName is the login of a person author, read with the message so a client can show a name instead of the account id.
	AuthorName string `json:"-"`
}

func (s *Store) InsertMessage(ctx context.Context, message Message) (Message, error) {
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		message, err = tx.InsertMessage(message)
		return err
	})
	return message, err
}

func (tx *Tx) InsertMessage(message Message) (Message, error) {
	targets, wake, guard, err := tx.resolveMessageTargets(message)
	if err != nil {
		return Message{}, err
	}
	targetsJSON, err := json.Marshal(targets)
	if err != nil {
		return Message{}, err
	}
	message.TargetsJSON = string(targetsJSON)
	message.ID, err = newID("msg")
	if err != nil {
		return Message{}, err
	}
	message.Seq, err = tx.nextSeq()
	if err != nil {
		return Message{}, err
	}
	if message.TargetsJSON == "" {
		message.TargetsJSON = "[]"
	}
	message.CreatedAt = timestamp()
	_, err = tx.tx.ExecContext(tx.ctx, `INSERT INTO message
		(id, seq, room_id, author_kind, author_id, body, reply_to,
		 thread_root, targets_json, ask_json, source_out_id, created_at,
		 visual_plan_json, appendix_pages_json, needs_attention, proposed_answers_json, open_questions_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		message.ID, message.Seq, message.RoomID, message.AuthorKind, message.AuthorID,
		message.Body, nullable(message.ReplyTo), nullable(message.ThreadRoot),
		message.TargetsJSON, nullable(message.AskJSON), nullable(message.SourceOutID),
		message.CreatedAt, nullable(message.VisualPlanJSON), nullable(message.AppendixPagesJSON),
		message.NeedsAttention, nullable(message.ProposedAnswersJSON), nullable(message.OpenQuestionsJSON))
	if err != nil {
		return Message{}, err
	}
	message.Attachments, err = tx.bindAttachments(message)
	if err != nil {
		return Message{}, err
	}
	// The message_assign_no trigger numbered the row (events do not take message numbers).
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT no FROM message WHERE id = ?`, message.ID).Scan(&message.No); err != nil {
		return Message{}, err
	}
	var projectID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM room WHERE id = ?`,
		message.RoomID).Scan(&projectID); err != nil {
		return Message{}, err
	}
	// The author and the members the message addresses ride along, so a browser decides whether it is worth a sound or a notification
	// without fetching the message first.
	addressed := targets
	if addressed == nil {
		addressed = []string{}
	}
	created, _ := json.Marshal(map[string]any{"message_id": message.ID, "author_kind": message.AuthorKind, "author_id": message.AuthorID, "targets": addressed})
	if _, err = tx.AppendEvent(Event{Kind: "message_created", ProjectID: projectID,
		RoomID: message.RoomID, PayloadJSON: string(created)}); err != nil {
		return Message{}, err
	}
	if message.AuthorKind == "user" {
		// Writing in a room means the author has read up to here, so their
		// own message never shows as unread on another device. The
		// message_created event above already refreshes their sessions.
		if _, err := tx.Exec(`INSERT INTO room_read_cursor(user_id,room_id,last_read_seq)
			SELECT id,?,? FROM user WHERE id=?
			ON CONFLICT(user_id,room_id) DO UPDATE SET last_read_seq=MAX(last_read_seq,excluded.last_read_seq)`,
			message.RoomID, message.Seq, message.AuthorID); err != nil {
			return Message{}, err
		}
	}
	for _, target := range targets {
		if _, err := tx.Exec(`UPDATE room_view_preference SET hidden=0 WHERE room_id=? AND user_id=? AND hidden=1`, message.RoomID, target); err != nil {
			return Message{}, err
		}
	}
	if err := tx.queueMessageWakes(message, wake, projectID); err != nil {
		return Message{}, err
	}
	if guard != nil {
		// Said once, at the reply that first hit the limit: the event for the record and a notice for the people in the room.
		if _, err := tx.AppendEvent(Event{Kind: "loop_guard", ProjectID: projectID, RoomID: message.RoomID, AgentID: message.AuthorID,
			PayloadJSON: payloadJSON(map[string]string{"message_id": message.ID, "reply_to": guard.ReplyTo, "hops": strconv.Itoa(guard.Hops)})}); err != nil {
			return Message{}, err
		}
		if _, err := tx.InsertMessage(Message{RoomID: message.RoomID, AuthorKind: "system", AuthorID: "system",
			Body: fmt.Sprintf("Agents stopped answering each other after %d automatic replies in a row. A message from a person starts the conversation again.", guard.Hops)}); err != nil {
			return Message{}, err
		}
	}
	return message, nil
}

func (s *Store) ListMessagesAfter(ctx context.Context, roomID string, afterSeq int64) ([]Message, error) {
	return s.listMessages(ctx, `WHERE room_id = ? AND seq > ? ORDER BY seq`, roomID, afterSeq)
}

const messageSelect = `SELECT id, seq, no, room_id, author_kind, author_id,
	body, reply_to, thread_root, targets_json, ask_json, source_out_id, created_at,
	COALESCE(author_harness,''), COALESCE(author_model,''),
	COALESCE((SELECT login FROM user WHERE user.id=message.author_id AND message.author_kind='user'),''),
	COALESCE(visual_plan_json,''), COALESCE(appendix_pages_json,''),
	needs_attention, COALESCE(proposed_answers_json,''), COALESCE(open_questions_json,'')
	FROM message `

func (s *Store) listMessages(ctx context.Context, clause string, args ...any) ([]Message, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, messageSelect+clause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		var message Message
		var replyTo, threadRoot, askJSON, sourceOutID sql.NullString
		if err := rows.Scan(&message.ID, &message.Seq, &message.No, &message.RoomID,
			&message.AuthorKind, &message.AuthorID, &message.Body, &replyTo,
			&threadRoot, &message.TargetsJSON, &askJSON, &sourceOutID,
			&message.CreatedAt, &message.AuthorHarness, &message.AuthorModel, &message.AuthorName,
			&message.VisualPlanJSON, &message.AppendixPagesJSON, &message.NeedsAttention,
			&message.ProposedAnswersJSON, &message.OpenQuestionsJSON); err != nil {
			return nil, err
		}
		message.ReplyTo, message.ThreadRoot = replyTo.String, threadRoot.String
		message.AskJSON, message.SourceOutID = askJSON.String, sourceOutID.String
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range messages {
		messages[i].Attachments, err = s.AttachmentsForMessage(ctx, messages[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return messages, nil
}

const timestampLayout = "2006-01-02T15:04:05.000000000Z"

func timestamp() string { return time.Now().UTC().Format(timestampLayout) }

func payloadJSON(fields map[string]string) string {
	data, _ := json.Marshal(fields) // Maps of strings cannot fail to marshal.
	return string(data)
}

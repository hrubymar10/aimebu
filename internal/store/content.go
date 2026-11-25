package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

type Project struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Folder  string `json:"folder"`
	RepoURL string `json:"repo_url"`
	// Role is the caller's role in the project (owner, operator, viewer); it is only filled for a user's own project list.
	Role string `json:"role,omitempty"`
	// FolderInUse means agents or live worktrees still depend on this path.
	FolderInUse bool `json:"folder_in_use,omitempty"`
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
	_, err = tx.Exec(`INSERT INTO project
		(id, name, folder, repo_url, created_at, team_flow_key, team_flow_review_pending)
		VALUES (?, ?, ?, ?, ?, '', 0)`, project.ID, project.Name, project.Folder,
		nullable(project.RepoURL), timestamp())
	if err != nil {
		return Project{}, err
	}
	if _, err = tx.Exec(
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

// updateProject is the one path that changes a project's name or folder, whether it comes from the project settings or from the home room's
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
	if _, err := tx.Exec(`UPDATE project SET name=?, folder=? WHERE id=?`, project.Name, project.Folder, id); err != nil {
		return project, err
	}
	if project.Name != oldName || renameHome {
		if _, err := tx.Exec(`UPDATE room SET title=? WHERE project_id=? AND kind='home' AND (title=? OR ?)`, project.Name, id, oldName, renameHome); err != nil {
			return project, err
		}
	}
	_, err := tx.AppendEvent(Event{Kind: "project_updated", ProjectID: id, PayloadJSON: payloadJSON(map[string]string{"project_id": id, "actor_id": actorID})})
	return project, err
}

type Room struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	ParentID  string `json:"parent_id,omitempty"`
	Depth     int    `json:"depth"`
	Kind      string `json:"kind"`
	// Purpose says what a task room is for: task (the default), research (a read-only investigation that ends in a report) or general
	// (the project-wide and coordination rooms). It is set when the room is made; the sidebar reads it and never infers it from members.
	Purpose         string `json:"purpose"`
	TeamFlowRoomKey string `json:"team_flow_room_key,omitempty"`
	Restricted      bool   `json:"restricted,omitempty"`
	DMKey           string `json:"dm_key,omitempty"`
	Title           string `json:"title"`
	CreatedBy       string `json:"created_by"`
	Lifecycle       string `json:"lifecycle"`
	Gate            string `json:"gate"`
	LifecycleRev    int64  `json:"lifecycle_rev"`
	ClosedAt        string `json:"closed_at,omitempty"`
	MemberCount     int    `json:"member_count"`
	MessageCount    int    `json:"message_count"`
	// UserMessageCount counts the messages that are not system notices: a room whose newest such message has no text (only an attachment) still has them, which the preview cannot show.
	UserMessageCount int    `json:"user_message_count"`
	MemoryEnabled    *bool  `json:"memory_enabled"`
	LastMessageBody  string `json:"last_message_body,omitempty"`
	LastActivityAt   string `json:"last_activity_at,omitempty"`
	Hidden           bool   `json:"hidden"`
	Notify           string `json:"notify"`
	MutedUntil       string `json:"muted_until,omitempty"`
	ReadCursor       int64  `json:"read_cursor"`
	UnreadCount      int    `json:"unread_count"`
	// UnreadAddressed counts the unread messages that name this person or reply to them (the ones in UnreadCount that target them), and
	// Working is true while an agent has a pending or running turn in the room. Both are set only on a person's room list.
	UnreadAddressed int  `json:"unread_addressed"`
	Working         bool `json:"working"`
	// FinishedUnseen is true while an agent that is not in a turn has written a message for this person (it names them, or asks for
	// attention) after the person's read cursor: it finished and they have not seen it. Derived from the read cursor, never stored.
	FinishedUnseen bool `json:"finished_unseen"`
	AttentionCount int  `json:"attention_count"`
	// AttentionOpen counts every open inbox entry this person can see in the room, seen or not (AttentionCount is the unseen ones): the
	// room is waiting for a person while it is above 0.
	AttentionOpen int           `json:"attention_open"`
	Worktree      *RoomWorktree `json:"worktree,omitempty"`
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
	if r.Purpose == "" {
		r.Purpose = RoomPurposeTask
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

// The purposes of a room (see Room.Purpose).
const (
	RoomPurposeTask     = "task"
	RoomPurposeResearch = "research"
	RoomPurposeGeneral  = "general"
)

// ValidRoomPurpose reports whether purpose is one a room can have; the empty string means the default, task.
func ValidRoomPurpose(purpose string) bool {
	return purpose == "" || purpose == RoomPurposeTask || purpose == RoomPurposeResearch || purpose == RoomPurposeGeneral
}

// GeneralRoomTitle is the task room every project starts with beside its home room.
const GeneralRoomTitle = "general"

func (tx *Tx) CreateRoom(room Room) (Room, error) {
	limit := tx.maxDepth
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
		var err error
		limit, err = tx.roomDepthLimit(room.ParentID)
		if err != nil {
			return Room{}, err
		}
	} else {
		room.Depth = 0
	}
	if room.Depth > limit {
		return Room{}, ErrDepthExceeded
	}
	var err error
	// A room whose ID the caller chose beforehand (see NewRoomID) keeps it: no caller takes an ID from a client.
	if room.ID == "" {
		if room.ID, err = newID("room"); err != nil {
			return Room{}, err
		}
	}
	if !ValidRoomPurpose(room.Purpose) {
		return Room{}, fmt.Errorf("invalid room purpose %q", room.Purpose)
	}
	if room.Purpose == "" || room.Kind != "task" {
		room.Purpose = RoomPurposeTask // only a task room has another purpose
	}
	_, err = tx.Exec(`INSERT INTO room
		(id, project_id, parent_id, depth, kind, dm_key, title, created_by, created_at, purpose)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, room.ID, room.ProjectID,
		nullable(room.ParentID), room.Depth, room.Kind, nullable(room.DMKey),
		room.Title, room.CreatedBy, timestamp(), room.Purpose)
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

// roomDepthLimit counts nesting below a home room. The stored depth remains
// the physical depth, so a home descendant gets one additional level within
// the absolute schema cap.
func (tx *Tx) roomDepthLimit(roomID string) (int, error) {
	var rootKind string
	err := tx.tx.QueryRowContext(tx.ctx, `WITH RECURSIVE ancestors(id,parent_id,kind,hops) AS (
		SELECT id,parent_id,kind,0 FROM room WHERE id=?
		UNION ALL
		SELECT parent.id,parent.parent_id,parent.kind,child.hops+1 FROM room parent
		JOIN ancestors child ON parent.id=child.parent_id WHERE child.hops<=?
	)
	SELECT kind FROM ancestors WHERE parent_id IS NULL LIMIT 1`, roomID, hardMaxDepth).Scan(&rootKind)
	if err != nil {
		return 0, err
	}
	if rootKind == "home" && tx.maxDepth < hardMaxDepth {
		return tx.maxDepth + 1, nil
	}
	return tx.maxDepth, nil
}

// joinLeadsToRoom makes every live lead agent of the project a member of the room (each it already belongs to is left alone).
func (tx *Tx) joinLeadsToRoom(roomID, projectID string) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT a.id FROM agent a JOIN role r ON r.id=a.role_id
		WHERE a.project_id=? AND a.revoked_at IS NULL
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
		var roleKey, kind, key, title, purpose string
		var lead bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT ro.role_key,r.kind,r.team_flow_room_key,r.title,r.purpose,ro.cap_is_lead FROM agent a JOIN role ro ON ro.id=a.role_id JOIN room r ON r.id=? WHERE a.id=?`, roomID, id).Scan(&roleKey, &kind, &key, &title, &purpose, &lead); err != nil {
			return err
		}
		autoJoin, err := tx.flowAutoJoin(projectID, roleKey, lead, flowRoomKind(kind, key, title, purpose), key)
		if err != nil {
			return err
		}
		if !autoJoin {
			continue
		}
		allowed, err := tx.flowJoinAllowed(projectID, flowRoomKind(kind, key, title, purpose), roleKey)
		if err != nil {
			return err
		}
		if !allowed {
			continue
		}
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
		WHERE a.id=? AND a.revoked_at IS NULL AND r.kind<>'dm' AND r.lifecycle='open'
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
		var projectID, roleKey, kind, key, title, purpose string
		var lead bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.project_id,ro.role_key,r.kind,r.team_flow_room_key,r.title,r.purpose,ro.cap_is_lead FROM room r JOIN agent a ON a.id=? JOIN role ro ON ro.id=a.role_id WHERE r.id=?`, agentID, id).Scan(&projectID, &roleKey, &kind, &key, &title, &purpose, &lead); err != nil {
			return err
		}
		autoJoin, err := tx.flowAutoJoin(projectID, roleKey, lead, flowRoomKind(kind, key, title, purpose), key)
		if err != nil {
			return err
		}
		if !autoJoin {
			continue
		}
		allowed, err := tx.flowJoinAllowed(projectID, flowRoomKind(kind, key, title, purpose), roleKey)
		if err != nil {
			return err
		}
		if !allowed {
			continue
		}
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
	if kind == "agent" {
		var projectID, flowKey, roomKind string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.project_id,p.team_flow_key,r.kind FROM room r JOIN project p ON p.id=r.project_id WHERE r.id=?`, roomID).Scan(&projectID, &flowKey, &roomKind); err != nil {
			return err
		}
		if roomKind != "dm" && flowKey != "" && flowKey != "custom" {
			var agentProjectID, key, title, role, purpose string
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,r.team_flow_room_key,r.title,ro.role_key,r.purpose FROM room r JOIN agent a ON a.id=? JOIN role ro ON ro.id=a.role_id WHERE r.id=?`, memberID, roomID).Scan(&agentProjectID, &key, &title, &role, &purpose); err != nil {
				return err
			}
			if projectID != agentProjectID {
				return ErrForbidden
			}
			allowed, err := tx.flowJoinAllowed(projectID, flowRoomKind(roomKind, key, title, purpose), role)
			if err != nil {
				return err
			}
			if !allowed {
				return ErrTeamFlowRule
			}
		}
	}
	var already, wasAuto int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*),COALESCE(MAX(auto),0) FROM room_member WHERE room_id=? AND member_kind=? AND member_id=? AND left_at IS NULL`, roomID, kind, memberID).Scan(&already, &wasAuto); err != nil {
		return err
	}
	if already > 0 && kind == "agent" {
		if !auto && wasAuto == 1 {
			_, err := tx.Exec(`UPDATE room_member SET auto=0 WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, roomID, memberID)
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
		// The project lead is a full member of home: an unaddressed human message there is the lead's to read and answer. The join is still
		// quiet (below); only the auto mark, which silences wakes, is left off.
		if kind == "agent" {
			var homeLead bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room r JOIN agent a ON a.id=? JOIN role ro ON ro.id=a.role_id
				WHERE r.id=? AND r.kind='home' AND ro.cap_is_lead=1)`, memberID, roomID).Scan(&homeLead); err != nil {
				return err
			}
			if homeLead {
				autoValue = 0
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO room_member
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
	result, err := tx.Exec(`UPDATE room_member SET left_at = ?
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
	AnswerState         string       `json:"answer_state,omitempty"`
	AnswerBody          string       `json:"answer_body,omitempty"`
	AnswerMessageNo     int64        `json:"answer_message_no,omitempty"`
	AnswerAuthorName    string       `json:"answer_author_name,omitempty"`
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
	SwitchHandoffJSON   string       `json:"-"`
	// AuthorName is the login of a person author, or the slug of an agent author that was removed, read with the message so a client can show a name
	// instead of the account or agent id; AuthorProjectID is the removed agent's project, which names it (slug@project).
	AuthorName      string `json:"-"`
	AuthorProjectID string `json:"-"`
	// Mentions is what each stored agent-ID mention of the body resolved to for this reader (id and display name). It is
	// filled by the read paths (displayMentions); a stored body keeps the IDs themselves. Empty when the body stores no
	// agent-ID mention.
	Mentions []MentionRef `json:"mentions,omitempty"`
	// Entities carries the room references the stored body mentions (`#room_<id>` tokens) for a human reader: one
	// {id,name,kind:"room"} entry per distinct id in body order, the title only when the reader may see the room. Filled
	// by RoomRefsForUser on the user-facing read paths; a stored body keeps the ids, and an agent-facing read carries the
	// same table (agents and people included) from mentionEntities.
	Entities []protocol.Entity `json:"entities,omitempty"`
	// Outcome is what a task-result handoff reports: done, partial or failed. Empty on every other message and on a handoff written
	// before outcomes existed (or by an agent whose supervisor predates them), which a client shows without one.
	Outcome string `json:"outcome,omitempty"`
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
	// One concept, the reply: a writer chooses only reply_to, and thread_root is the first message of the conversation, derived from the reply
	// chain (the parent's thread_root, or the parent itself when it starts one); one a caller passes in is dropped. The parent is looked up in
	// the message's own room: a reply_to in another room, or missing, derives nothing, and every caller (the POST route, an answer's validation,
	// an inbox answer, which uses the ask's own room) has already made sure the parent is in the room.
	message.ThreadRoot = ""
	if message.ReplyTo != "" {
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE(NULLIF(thread_root,''),id) FROM message WHERE id=? AND room_id=?`, message.ReplyTo, message.RoomID).Scan(&message.ThreadRoot)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Message{}, err
		}
	}
	// Mentions are stored by agent ID: every live prose mention that names exactly one agent of the room is rewritten to
	// @agt_<id> before addressing is resolved (which accepts a full member ID as a token), so the mention keeps addressing
	// the same agent when its name is later freed and reused. Readers get the display form back.
	if _, err := tx.storeMentionsByID(&message); err != nil {
		return Message{}, err
	}
	// Rooms are referenced by id: a person's typed `#<title>` that names exactly one room they can see is resolved to the
	// room's id on save, so the reference keeps working when the room is renamed. Agents write the id form themselves.
	if _, err := tx.storeRoomRefsByID(&message); err != nil {
		return Message{}, err
	}
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
	if message.AuthorKind == "agent" && message.AuthorHarness == "" && message.AuthorModel == "" {
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT tool,model FROM agent WHERE id=?`, message.AuthorID).
			Scan(&message.AuthorHarness, &message.AuthorModel); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Message{}, err
		}
	}
	_, err = tx.Exec(`INSERT INTO message
		(id, seq, room_id, author_kind, author_id, body, reply_to,
		 thread_root, targets_json, ask_json, source_out_id, created_at,
		 visual_plan_json, appendix_pages_json, needs_attention, proposed_answers_json, open_questions_json,
		 author_harness, author_model, switch_handoff_json, outcome)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		message.ID, message.Seq, message.RoomID, message.AuthorKind, message.AuthorID,
		message.Body, nullable(message.ReplyTo), nullable(message.ThreadRoot),
		message.TargetsJSON, nullable(message.AskJSON), nullable(message.SourceOutID),
		message.CreatedAt, nullable(message.VisualPlanJSON), nullable(message.AppendixPagesJSON),
		message.NeedsAttention, nullable(message.ProposedAnswersJSON), nullable(message.OpenQuestionsJSON),
		message.AuthorHarness, message.AuthorModel, nullable(message.SwitchHandoffJSON), nullable(message.Outcome))
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
	if message.AuthorKind == "agent" {
		var role, flowKey string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.role_key,p.team_flow_key FROM agent a JOIN role r ON r.id=a.role_id JOIN project p ON p.id=a.project_id WHERE a.id=?`, message.AuthorID).Scan(&role, &flowKey); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Message{}, err
		}
		flow, ok := teamFlowTemplate(flowKey)
		if ok && flowAllows(flow.GrantRoles, role) {
			for _, ref := range extractMessageRefs([]string{message.Body}, nil, 100) {
				if _, err := tx.Exec(`INSERT OR IGNORE INTO message_reference_grant(source_message_id,target_no)
					SELECT ?,target.no FROM message target JOIN room tr ON tr.id=target.room_id JOIN room sr ON sr.id=?
					WHERE target.no=? AND target.no>0 AND tr.project_id=sr.project_id AND tr.kind<>'dm'
					AND (tr.restricted=0 OR EXISTS(SELECT 1 FROM room_member own WHERE own.room_id=tr.id
						AND own.member_kind='agent' AND own.member_id=? AND own.left_at IS NULL))`, message.ID, message.RoomID, ref, message.AuthorID); err != nil {
					return Message{}, err
				}
			}
		}
	}
	if message.AuthorKind == "agent" {
		// A lead's "#N" in a chat with another project's lead lets that lead read the message, if it is one of the author's own project that the
		// author reads (a normal room, or a restricted one it sits in). The read re-checks both leads, so a lead that is no longer one grants nothing.
		for _, ref := range extractMessageRefs([]string{message.Body}, nil, 100) {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO message_reference_grant(source_message_id,target_no)
				SELECT ?,target.no FROM message target JOIN room tr ON tr.id=target.room_id JOIN room sr ON sr.id=?
				JOIN agent au ON au.id=? JOIN role ro ON ro.id=au.role_id
				WHERE target.no=? AND target.no>0 AND sr.kind='dm' AND ro.cap_is_lead=1 AND tr.project_id=au.project_id AND tr.kind<>'dm'
				AND (tr.restricted=0 OR EXISTS(SELECT 1 FROM room_member own WHERE own.room_id=tr.id
					AND own.member_kind='agent' AND own.member_id=au.id AND own.left_at IS NULL))`, message.ID, message.RoomID, message.AuthorID, ref); err != nil {
				return Message{}, err
			}
		}
	}
	if message.AuthorKind == "user" {
		// A person's "#N" into another project is a grant for the agents of this room, if the person reads the target room now. Agents never
		// grant across projects, and the read re-checks the person's access, so losing it revokes the grant.
		for _, ref := range extractMessageRefs([]string{message.Body}, nil, 100) {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO message_reference_grant(source_message_id,target_no)
				SELECT ?,target.no FROM message target JOIN room tr ON tr.id=target.room_id JOIN room sr ON sr.id=?
				WHERE target.no=? AND target.no>0 AND tr.project_id<>sr.project_id AND sr.kind<>'dm' AND `+personReadsRoomSQL("?"),
				message.ID, message.RoomID, ref, message.AuthorID, message.AuthorID, message.AuthorID); err != nil {
				return Message{}, err
			}
		}
	}
	var projectID, roomKind string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,kind FROM room WHERE id = ?`,
		message.RoomID).Scan(&projectID, &roomKind); err != nil {
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
	if err := tx.supersedeAsks(message); err != nil {
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
	if err := tx.queueMessageWakes(message, wake, projectID, roomKind); err != nil {
		return Message{}, err
	}
	if guard != nil {
		// Said once, at the reply that first hit the limit: the event for the record and a notice for the people in the room.
		if _, err := tx.AppendEvent(Event{Kind: "loop_guard", ProjectID: projectID, RoomID: message.RoomID, AgentID: message.AuthorID,
			PayloadJSON: payloadJSON(map[string]string{"message_id": message.ID, "reply_to": guard.ReplyTo, "hops": strconv.Itoa(guard.Hops), "kind": guardKind(guard)})}); err != nil {
			return Message{}, err
		}
		if _, err := tx.InsertMessage(Message{RoomID: message.RoomID, AuthorKind: "system", AuthorID: "system",
			Body: fmt.Sprintf("Agents stopped answering each other after %d automatic replies and broadcasts in a row. A message from a person starts the conversation again.", guard.Hops)}); err != nil {
			return Message{}, err
		}
	}
	return message, nil
}

// guardKind names what a loop guard stopped: a reply that did not wake its parent's author, or a broadcast that woke nobody.
func guardKind(guard *LoopGuard) string {
	if guard.Broadcast {
		return "broadcast"
	}
	return "reply"
}

func (s *Store) ListMessagesAfter(ctx context.Context, roomID string, afterSeq int64) ([]Message, error) {
	return s.listMessages(ctx, `WHERE room_id = ? AND seq > ? ORDER BY seq`, roomID, afterSeq)
}

const messageSelect = `SELECT id, seq, no, room_id, author_kind, author_id,
	body, reply_to, thread_root, targets_json, ask_json, source_out_id, created_at,
	COALESCE(author_harness,''), COALESCE(author_model,''), COALESCE(switch_handoff_json,''),
	COALESCE((SELECT login FROM user WHERE user.id=message.author_id AND message.author_kind='user'),
		(SELECT COALESCE(NULLIF(removed_slug,''),slug) FROM agent WHERE agent.id=message.author_id AND message.author_kind='agent' AND agent.revoked_at IS NOT NULL),''),
	COALESCE((SELECT project_id FROM agent WHERE agent.id=message.author_id AND message.author_kind='agent' AND agent.revoked_at IS NOT NULL),''),
	COALESCE(visual_plan_json,''), COALESCE(appendix_pages_json,''),
	needs_attention, COALESCE(proposed_answers_json,''), COALESCE(open_questions_json,''), COALESCE(outcome,'')
	FROM message `

func (s *Store) listMessages(ctx context.Context, clause string, args ...any) ([]Message, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.readDB.QueryContext(ctx, messageSelect+clause, args...)
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
			&message.CreatedAt, &message.AuthorHarness, &message.AuthorModel, &message.SwitchHandoffJSON, &message.AuthorName, &message.AuthorProjectID,
			&message.VisualPlanJSON, &message.AppendixPagesJSON, &message.NeedsAttention,
			&message.ProposedAnswersJSON, &message.OpenQuestionsJSON, &message.Outcome); err != nil {
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
	// Readers see display names: the stored agent-ID mentions are rewritten per room, and the mentions array names what
	// each one resolved to. The stored rows keep the IDs.
	if err := displayMentions(ctx, s.readDB, messages); err != nil {
		return nil, err
	}
	if err := s.answerStateForMessages(ctx, messages); err != nil {
		return nil, err
	}
	attachments, err := s.attachmentsForMessages(ctx, messages)
	if err != nil {
		return nil, err
	}
	for i := range messages {
		messages[i].Attachments = attachments[messages[i].ID]
	}
	return messages, nil
}

func (s *Store) answerStateForMessages(ctx context.Context, messages []Message) error {
	var ids []any
	for _, message := range messages {
		if message.AskJSON != "" || message.ProposedAnswersJSON != "" || message.OpenQuestionsJSON != "" {
			ids = append(ids, message.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	query := `SELECT item.message_id,item.state,COALESCE(answer.body,''),COALESCE(answer.no,0),COALESCE(answerer.login,'') FROM inbox_item item
		LEFT JOIN message answer ON answer.id=item.answer_message_id
		LEFT JOIN user answerer ON answer.author_kind='user' AND answerer.id=answer.author_id
		WHERE item.kind IN ('question','approval') AND item.message_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
	rows, err := s.readDB.QueryContext(ctx, query, ids...)
	if err != nil {
		return err
	}
	type answerState struct {
		state, body, authorName string
		messageNo               int64
	}
	states := make(map[string]answerState)
	for rows.Next() {
		var id string
		var answer answerState
		if err := rows.Scan(&id, &answer.state, &answer.body, &answer.messageNo, &answer.authorName); err != nil {
			rows.Close()
			return err
		}
		states[id] = answer
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for i := range messages {
		answer := states[messages[i].ID]
		messages[i].AnswerState = answer.state
		messages[i].AnswerBody = answer.body
		messages[i].AnswerMessageNo = answer.messageNo
		messages[i].AnswerAuthorName = answer.authorName
	}
	return nil
}

const timestampLayout = "2006-01-02T15:04:05.000000000Z"

func timestamp() string { return time.Now().UTC().Format(timestampLayout) }

func payloadJSON(fields map[string]string) string {
	data, _ := json.Marshal(fields) // Maps of strings cannot fail to marshal.
	return string(data)
}

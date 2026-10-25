package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

var ErrWriterOccupied = errors.New("writer_occupied")
var ErrRoleOccupied = errors.New("role_occupied")
var ErrSpawnLimit = errors.New("spawn_limit")

type AgentCard struct {
	DefaultUsed      bool                `json:"default_used,omitempty"`
	DefaultSelection *SpawnDefaultChoice `json:"default_selection,omitempty"`
	WorktreeWarning  string              `json:"worktree_warning,omitempty"`
	ID               string              `json:"id"`
	Slug             string              `json:"slug"`
	RoomID           string              `json:"room_id"`
	ProjectID        string              `json:"project_id"`
	RoleKey          string              `json:"role_key"`
	RoleEmoji        string              `json:"role_emoji,omitempty"`
	RoleDeleted      bool                `json:"role_deleted,omitempty"`
	RoleUpdated      bool                `json:"role_updated,omitempty"`
	Tool             string              `json:"tool"`
	Variant          string              `json:"variant"`
	LauncherID       string              `json:"launcher_id,omitempty"`
	Model            string              `json:"model"`
	Provider         string              `json:"provider"`
	MachineID        string              `json:"machine_id"`
	Desired          string              `json:"desired"`
	DesiredRev       int64               `json:"desired_rev"`
	Observed         string              `json:"observed"`
	Generation       int                 `json:"generation"`
	RecoveryBlock    bool                `json:"recovery_block"`
	LastExitEvidence string              `json:"last_exit_evidence,omitempty"`
	LastActivityAt   string              `json:"last_activity_at,omitempty"`
	MachineLastSeen  string              `json:"machine_last_seen,omitempty"`
	Switch           *AgentSwitch        `json:"switch,omitempty"`
	BoundaryPending  string              `json:"boundary_pending,omitempty"`
	BoundaryGrants   []BoundaryGrant     `json:"boundary_grants,omitempty"`
	// WokenBy is why the turn in progress started, when that was an agent's reply to this one ("reply"); the card tags it so a person can
	// see an exchange between agents as it happens. Empty when idle or for any other reason.
	WokenBy string `json:"woken_by,omitempty"`
}

// wokenBy is the wake reason of the turn that is running (or about to) for agentID, only when it is a reply from another agent.
func (s *Store) wokenBy(ctx context.Context, agentID string) (string, error) {
	// The note that started the turn is its earliest one; notes that joined it later do not change why it began.
	var reason, authorKind string
	err := s.db.QueryRowContext(ctx, `SELECT n.wake_reason, m.author_kind FROM turn t JOIN turn_note tn ON tn.turn_id=t.id JOIN note n ON n.id=tn.note_id
		JOIN message m ON m.id=n.message_id
		WHERE t.agent_id=? AND t.exec IN ('pending','running')
		ORDER BY t.rowid DESC, n.created_at ASC, n.rowid ASC LIMIT 1`, agentID).Scan(&reason, &authorKind)
	if errors.Is(err, sql.ErrNoRows) || reason != "reply" || authorKind != "agent" {
		return "", nil
	}
	return reason, err
}

func nativeSessionID(tool string) (string, error) {
	if tool != "claude" && tool != "pi" {
		return "", nil
	}
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:]), nil
}

func nullableString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

type SpawnInput struct {
	SpawnNote                                                                          string
	RoomID, ActorID, MachineID, Path, RoleKey, Tool, Variant, Model, Provider, KeyHash string
	LauncherID                                                                         string
	ActorKind                                                                          string
	// Slug asks for a particular name (the agent being continued keeps its own); empty picks one from the pool.
	Slug string
	// ResumeLegacyID is the imported v1 agent this one is started from; ResumeHandle is that agent's harness session to continue, empty
	// for a harness that cannot resume (the agent then starts fresh with the same name).
	ResumeLegacyID, ResumeHandle string
	// ResumeSession is the Claude Code session this agent continues (ResumeHandle carries its id); NewRoomTitle, when set, starts the agent
	// in a new task room of that title under the project's home room instead of in RoomID.
	ResumeSession *ResumeSession
	NewRoomTitle  string
}

// ErrHomeLeadOnly is a spawn of a role without the lead capability into a home room whose project keeps it for the lead.
var ErrHomeLeadOnly = errors.New("home room is lead only")

// ErrSlugTaken is a requested agent name that a project already uses (an agent, a person or a role key).
var ErrSlugTaken = errors.New("agent name in use")

// ErrLegacyStarted is a v1 agent that another request started as a v2 agent first.
var ErrLegacyStarted = errors.New("v1 agent already started")

func (s *Store) SpawnRequestResult(ctx context.Context, actorKind, actorID, key, digest string) (string, bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", false, err
	}
	var storedDigest, result string
	err := s.db.QueryRowContext(ctx, `SELECT request_digest,result_json FROM request_key WHERE actor_kind=? AND actor_id=? AND kind='spawn' AND key=?`, actorKind, actorID, key).Scan(&storedDigest, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if storedDigest != digest {
		return "", false, ErrConflict
	}
	return result, true, nil
}

func (tx *Tx) SpawnAgent(in SpawnInput) (AgentCard, error) {
	if in.NewRoomTitle != "" {
		actor := in.ActorID
		room, err := tx.roomForSession(in.RoomID, in.NewRoomTitle, actor)
		if err != nil {
			return AgentCard{}, err
		}
		in.RoomID = room.ID
	}
	var projectID, lifecycle string
	var depth int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,lifecycle,depth FROM room WHERE id=?`, in.RoomID).Scan(&projectID, &lifecycle, &depth); err != nil {
		return AgentCard{}, err
	}
	// The route's guard looks before this transaction starts; an archive that commits in between is caught here, so no agent starts in an
	// archived project.
	if err := tx.refuseArchived(projectID); err != nil {
		return AgentCard{}, err
	}
	if lifecycle != "open" || depth > tx.maxDepth {
		return AgentCard{}, ErrInvalidTransition
	}
	actorKind := in.ActorKind
	if actorKind == "" {
		actorKind = "user"
	}
	if err := tx.authorizeSpawnActor(projectID, actorKind, in.ActorID, true); err != nil {
		return AgentCard{}, err
	}
	var roleID string
	var writer, leadRole bool
	var cardinality string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id,cap_singleton_writer,cap_is_lead,cardinality FROM role
		WHERE role_key=? AND deleted_at IS NULL LIMIT 1`, in.RoleKey).Scan(&roleID, &writer, &leadRole, &cardinality); err != nil {
		return AgentCard{}, err
	}
	if actorKind == "agent" && leadRole {
		return AgentCard{}, ErrForbidden
	}
	if !leadRole {
		// "Home room: lead only" (on by default): the home room is the lead's; other roles are spawned into task rooms.
		var leadOnly bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.kind='home' AND COALESCE((SELECT home_lead_only FROM settings_project WHERE project_id=r.project_id),1)=1
			FROM room r WHERE r.id=?`, in.RoomID).Scan(&leadOnly); err != nil {
			return AgentCard{}, err
		}
		if leadOnly {
			return AgentCard{}, ErrHomeLeadOnly
		}
	}
	if cardinality == "singleton" {
		var occupied int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM agent a JOIN role r ON r.id=a.role_id
			WHERE a.project_id=? AND r.role_key=? AND a.revoked_at IS NULL
			AND (a.desired!='stopped' OR a.observed!='stopped')`, projectID, in.RoleKey).Scan(&occupied); err != nil {
			return AgentCard{}, err
		}
		if occupied > 0 {
			return AgentCard{}, ErrRoleOccupied
		}
	}
	var maxLive, rate int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT max_live_agents,spawn_rate_per_min FROM settings_project WHERE project_id=?`, projectID).Scan(&maxLive, &rate); err != nil {
		return AgentCard{}, err
	}
	var live, recent int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM agent WHERE project_id=? AND (desired!='stopped' OR observed!='stopped') AND revoked_at IS NULL`, projectID).Scan(&live); err != nil {
		return AgentCard{}, err
	}
	if live >= maxLive {
		return AgentCard{}, ErrSpawnLimit
	}
	cutoff := time.Now().UTC().Add(-time.Minute).Format("2006-01-02T15:04:05.000000000Z")
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM event WHERE project_id=? AND kind='agent_created' AND created_at>=?`, projectID, cutoff).Scan(&recent); err != nil {
		return AgentCard{}, err
	}
	if recent >= rate {
		return AgentCard{}, ErrSpawnLimit
	}
	var checkoutID, state string
	var currentWriter sql.NullString
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT id,writer_state,writer_agent_id FROM checkout WHERE machine_id=? AND canonical_path=?`, in.MachineID, in.Path).Scan(&checkoutID, &state, &currentWriter)
	if errors.Is(err, sql.ErrNoRows) {
		checkoutID, err = newID("chk")
		if err != nil {
			return AgentCard{}, err
		}
		_, err = tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES (?,?,?,?)`, checkoutID, projectID, in.MachineID, in.Path)
	} else if err == nil {
		var checkoutProject string
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM checkout WHERE id=?`, checkoutID).Scan(&checkoutProject)
		if err == nil && checkoutProject != projectID {
			return AgentCard{}, ErrScopeMismatch
		}
	}
	if err != nil {
		return AgentCard{}, err
	}
	if writer && state != "" && state != "none" {
		return AgentCard{}, ErrWriterOccupied
	}
	// Before the name check: a second start of the same v1 agent finds its name taken by the first one's agent, and must say so as
	// "already started", not as a name clash. The claim further down stays the arbiter if two starts get past this together.
	if in.ResumeLegacyID != "" {
		var started sql.NullString
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT started_agent_id FROM legacy_agent_session WHERE agent_id=?`, in.ResumeLegacyID).Scan(&started); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return AgentCard{}, err
		}
		if started.Valid && started.String != "" {
			return AgentCard{}, ErrLegacyStarted
		}
	}
	var slug string
	if in.Slug != "" {
		if !ValidAgentSlug(in.Slug) {
			return AgentCard{}, ErrInvalidTransition
		}
		taken, err := tx.takenSlugs(projectID)
		if err != nil {
			return AgentCard{}, err
		}
		if taken[in.Slug] {
			return AgentCard{}, ErrSlugTaken
		}
		slug = in.Slug
	} else if slug, err = tx.nextAgentSlug(projectID); err != nil {
		return AgentCard{}, err
	}
	provider := in.Provider
	if provider == "" {
		provider = in.Tool
	}
	agent, err := tx.CreateAgent(Agent{Slug: slug, ProjectID: projectID, RoleID: roleID, Tool: in.Tool, Variant: in.Variant, LauncherID: in.LauncherID, Model: in.Model, Provider: provider, MachineID: in.MachineID, CheckoutID: checkoutID, KeyHash: in.KeyHash, CreatedBy: in.ActorID})
	if err != nil {
		return AgentCard{}, err
	}
	allowed, err := tx.boundaryAllowed(projectID, agent.ID, in.MachineID, in.Path)
	if err != nil {
		return AgentCard{}, err
	}
	if writer && allowed {
		if _, err = tx.Exec(`UPDATE checkout SET writer_agent_id=?,writer_state='reserved' WHERE id=?`, agent.ID, checkoutID); err != nil {
			return AgentCard{}, err
		}
	}
	sessionID, err := newID("ses")
	if err != nil {
		return AgentCard{}, err
	}
	nativeSessionID, err := nativeSessionID(in.Tool)
	if err != nil {
		return AgentCard{}, err
	}
	if in.ResumeHandle != "" {
		// The session already exists in the harness: the first turn continues it (--resume and the like) instead of starting one.
		now := timestamp()
		if _, err = tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at,native_session_id,native_session_started_at) VALUES (?,?,0,'native_resume',?,?,?)`,
			sessionID, agent.ID, now, in.ResumeHandle, now); err != nil {
			return AgentCard{}, err
		}
	} else if _, err = tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at,native_session_id) VALUES (?,?,0,'fresh_with_history',?,?)`, sessionID, agent.ID, timestamp(), nullableString(nativeSessionID)); err != nil {
		return AgentCard{}, err
	}
	if in.ResumeSession != nil {
		if err = tx.attachClaudeSession(in, agent.ID); err != nil {
			return AgentCard{}, err
		}
		if in.ResumeSession.Fork {
			// The session row names the original until the first turn reports the copy's id (RecordNativeSessionStarted).
			if _, err = tx.Exec(`UPDATE session SET fork_of=? WHERE id=?`, in.ResumeSession.ID, sessionID); err != nil {
				return AgentCard{}, err
			}
		}
	}
	if in.ResumeLegacyID != "" {
		// A legacy agent is started once: its name is then the v2 agent's for good (names are unique in a project), and that agent is
		// restarted with the ordinary controls.
		result, err := tx.tx.ExecContext(tx.ctx, `UPDATE legacy_agent_session SET started_agent_id=? WHERE agent_id=? AND (started_agent_id IS NULL OR started_agent_id='')`, agent.ID, in.ResumeLegacyID)
		if err != nil {
			return AgentCard{}, err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return AgentCard{}, ErrLegacyStarted
		}
		if _, err = tx.Exec(`UPDATE agent SET resumed_from_legacy_id=? WHERE id=?`, in.ResumeLegacyID, agent.ID); err != nil {
			return AgentCard{}, err
		}
	}
	if err = tx.AddMember(in.RoomID, "agent", agent.ID); err != nil {
		return AgentCard{}, err
	}
	notice := fmt.Sprintf("%s joined as %s", slug, in.RoleKey)
	if in.ResumeLegacyID != "" {
		if in.ResumeHandle != "" {
			notice = fmt.Sprintf("%s joined as %s, continuing the v1 session of %s in %s", slug, in.RoleKey, in.ResumeLegacyID, in.Path)
		} else {
			notice = fmt.Sprintf("%s joined as %s, started fresh from v1 agent %s (its harness cannot resume a session)", slug, in.RoleKey, in.ResumeLegacyID)
		}
	}
	if in.ResumeSession != nil {
		notice = fmt.Sprintf("%s joined as %s, continuing the Claude Code session %s in %s", slug, in.RoleKey, describeSession(in.ResumeSession), in.Path)
		if in.ResumeSession.Fork {
			notice = fmt.Sprintf("%s joined as %s, working on a copy of the Claude Code session %s in %s", slug, in.RoleKey, describeSession(in.ResumeSession), in.Path)
		}
	}
	if actorKind == "agent" {
		actor, err := tx.agentDisplay(in.ActorID)
		if err != nil {
			return AgentCard{}, err
		}
		notice = fmt.Sprintf("%s spawned %s (%s)", actor, slug, in.RoleKey)
	}
	if !allowed {
		notice = fmt.Sprintf("%s is awaiting owner approval to start as %s", slug, in.RoleKey)
		if in.ResumeHandle != "" {
			kind := "a v1 session"
			if in.ResumeSession != nil {
				kind = "a Claude Code session"
			}
			notice += fmt.Sprintf("; it continues %s and works in %s, outside the project folder", kind, in.Path)
		}
	}
	if in.SpawnNote != "" {
		notice += "; " + in.SpawnNote
	}
	if _, err = tx.InsertMessage(Message{RoomID: in.RoomID, AuthorKind: "system", AuthorID: "system", Body: notice}); err != nil {
		return AgentCard{}, err
	}
	card := AgentCard{ID: agent.ID, Slug: slug, RoomID: in.RoomID, ProjectID: projectID, RoleKey: in.RoleKey, Tool: in.Tool, Variant: in.Variant, LauncherID: in.LauncherID, Model: in.Model, Provider: provider, MachineID: in.MachineID, Desired: "active", Observed: "idle"}
	if !allowed {
		if _, err := tx.Exec(`UPDATE agent SET desired='not_accepting',desired_rev=desired_rev+1 WHERE id=?`, agent.ID); err != nil {
			return AgentCard{}, err
		}
		card.Desired, card.DesiredRev = "not_accepting", 1
		card.BoundaryPending, err = tx.createBoundaryPending(projectID, in.RoomID, agent.ID, in.MachineID, in.Path, "spawn", actorKind, in.ActorID)
		if err != nil {
			return AgentCard{}, err
		}
	} else if _, err = tx.AppendEvent(Event{Kind: "agent_spawn_requested", ProjectID: projectID, RoomID: in.RoomID, AgentID: agent.ID,
		PayloadJSON: payloadJSON(map[string]string{"agent_id": agent.ID, "actor_kind": actorKind, "actor_id": in.ActorID})}); err != nil {
		return AgentCard{}, err
	}
	return card, nil
}

func (tx *Tx) authorizeSpawnActor(projectID, actorKind, actorID string, spawning bool) error {
	if actorKind == "agent" {
		var allowed bool
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.cap_is_lead=1 AND (NOT ? OR r.cap_can_spawn=1) AND a.desired='active'
			FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=? AND a.project_id=? AND a.revoked_at IS NULL`, spawning, actorID, projectID).Scan(&allowed)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !allowed {
			return ErrForbidden
		}
		return err
	}
	if actorKind != "user" {
		return ErrForbidden
	}
	var role string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT role FROM membership WHERE project_id=? AND user_id=?`, projectID, actorID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) || err == nil && role != "owner" && role != "operator" {
		return ErrForbidden
	}
	return err
}

var agentNamePool = []string{"alice", "bob", "carol", "dave", "eve", "frank", "grace", "hugo", "iris", "jack", "kate", "leo", "mia", "nate", "olive", "pete", "quinn", "ruth", "sam", "tina", "vera", "will", "xena", "yara", "zeke", "amy", "ben", "cora", "dan", "ella", "finn", "gina", "hal", "ivy", "jade", "kai", "lena", "max", "nora", "otto", "piper", "rhea", "seth", "theo", "una", "vince", "wren", "xavi", "yuki", "zoe", "arlo", "beth", "cleo", "drew", "elsa", "felix", "gus", "hana", "ian", "june", "kira", "luna", "milo", "nina", "owen", "penny", "rex", "sage", "tara", "uma", "vlad", "wade", "xio", "yann", "zane", "ada", "boris", "cass", "deon", "emma", "ford", "gwen", "hans", "ines", "jonas", "kyra", "lars", "mona", "nils", "opal", "pia", "quin", "remi", "sven", "tessa", "umar", "viv", "wes", "xan", "yael", "zeno"}

// takenSlugs is every name a new agent in the project cannot have: an agent's, a person's, or a role key that @name would clash with.
func (tx *Tx) takenSlugs(projectID string) (map[string]bool, error) {
	return takenSlugs(tx.ctx, tx.tx, projectID)
}

type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func takenSlugs(ctx context.Context, q rowQuerier, projectID string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT slug FROM agent WHERE project_id=? UNION SELECT u.login FROM user u JOIN membership m ON m.user_id=u.id WHERE m.project_id=? UNION SELECT role_key FROM role WHERE deleted_at IS NULL AND (project_id IS NULL OR project_id=?)`, projectID, projectID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	taken := map[string]bool{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		taken[strings.ToLower(slug)] = true
	}
	return taken, rows.Err()
}

func (tx *Tx) nextAgentSlug(projectID string) (string, error) {
	taken, err := tx.takenSlugs(projectID)
	if err != nil {
		return "", err
	}
	return pickAgentSlug(taken)
}

// pickAgentSlug is a random pool name that is not taken; once the pool is used up the names get a number ("alice2").
func pickAgentSlug(taken map[string]bool) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	start := int(binary.LittleEndian.Uint64(raw[:]) % uint64(len(agentNamePool)))
	for suffix := 1; ; suffix++ {
		for i := 0; i < len(agentNamePool); i++ {
			base := agentNamePool[(start+i)%len(agentNamePool)]
			candidate := base
			if suffix > 1 {
				candidate = fmt.Sprintf("%s%d", base, suffix)
			}
			if !taken[candidate] {
				return candidate, nil
			}
		}
	}
}

// SuggestAgentSlug is a free name for the next agent of the project: one SpawnAgent would accept as it is (nothing is reserved, so a
// later spawn can still find it taken). exclude is a name never to suggest, such as the one the asker is already looking at; empty excludes none.
func (s *Store) SuggestAgentSlug(ctx context.Context, projectID, exclude string) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	taken, err := takenSlugs(ctx, s.db, projectID)
	if err != nil {
		return "", err
	}
	if exclude != "" {
		taken[strings.ToLower(exclude)] = true
	}
	return pickAgentSlug(taken)
}

// AgentSlugTaken reports whether the project already has an agent, a person or a role by that name.
func (s *Store) AgentSlugTaken(ctx context.Context, projectID, slug string) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	taken, err := takenSlugs(ctx, s.db, projectID)
	if err != nil {
		return false, err
	}
	return taken[strings.ToLower(slug)], nil
}

func (s *Store) AgentCardsForRoom(ctx context.Context, roomID string) ([]AgentCard, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.slug,a.project_id,r.role_key,r.emoji,r.deleted_at IS NOT NULL,
		EXISTS(SELECT 1 FROM role current WHERE current.project_id IS NULL AND current.role_key=r.role_key AND current.id<>r.id AND current.deleted_at IS NULL AND (current.body<>r.body OR current.extends<>r.extends OR current.label<>r.label OR current.emoji<>r.emoji OR current.cardinality<>r.cardinality OR current.cap_is_lead<>r.cap_is_lead OR current.cap_can_spawn<>r.cap_can_spawn OR current.cap_singleton_writer<>r.cap_singleton_writer)),
		a.tool,a.variant,COALESCE(a.launcher_id,''),a.model,a.provider,a.machine_id,a.desired,a.desired_rev,a.observed,a.generation,a.recovery_block,
		COALESCE((SELECT MAX(e.created_at) FROM event e WHERE e.agent_id=a.id),''),COALESCE(machine.last_seen,''),
		COALESCE((SELECT json_extract(e.payload_json,'$.evidence') FROM event e WHERE e.turn_id=(SELECT t.id FROM turn t WHERE t.agent_id=a.id AND t.outcome='unknown' ORDER BY t.id DESC LIMIT 1) AND e.kind='turn_process_exit' LIMIT 1),''),
		COALESCE((SELECT b.inbox_id FROM boundary_pending b WHERE b.agent_id=a.id AND b.state='open'),'')
		FROM agent a JOIN role r ON r.id=a.role_id JOIN room_member m ON m.member_id=a.id AND m.member_kind='agent' AND m.left_at IS NULL
		LEFT JOIN machine ON machine.id=a.machine_id WHERE m.room_id=? ORDER BY a.created_at,a.id`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cards := []AgentCard{}
	for rows.Next() {
		var c AgentCard
		c.RoomID = roomID
		if err := rows.Scan(&c.ID, &c.Slug, &c.ProjectID, &c.RoleKey, &c.RoleEmoji, &c.RoleDeleted, &c.RoleUpdated, &c.Tool, &c.Variant, &c.LauncherID, &c.Model, &c.Provider, &c.MachineID, &c.Desired, &c.DesiredRev, &c.Observed, &c.Generation, &c.RecoveryBlock, &c.LastActivityAt, &c.MachineLastSeen, &c.LastExitEvidence, &c.BoundaryPending); err != nil {
			return nil, err
		}
		cards = append(cards, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range cards {
		cards[i].Switch, err = s.agentSwitch(ctx, cards[i].ID)
		if err != nil {
			return nil, err
		}
		cards[i].BoundaryGrants, err = s.BoundaryGrants(ctx, cards[i].ID)
		if err != nil {
			return nil, err
		}
		if cards[i].Observed == "in_turn" {
			if cards[i].WokenBy, err = s.wokenBy(ctx, cards[i].ID); err != nil {
				return nil, err
			}
		}
	}
	return cards, nil
}

func (s *Store) AgentCardByID(ctx context.Context, id string) (AgentCard, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return AgentCard{}, err
	}
	var c AgentCard
	err := s.db.QueryRowContext(ctx, `SELECT a.id,a.slug,a.project_id,r.role_key,r.emoji,r.deleted_at IS NOT NULL,
		EXISTS(SELECT 1 FROM role current WHERE current.project_id IS NULL AND current.role_key=r.role_key AND current.id<>r.id AND current.deleted_at IS NULL AND (current.body<>r.body OR current.extends<>r.extends OR current.label<>r.label OR current.emoji<>r.emoji OR current.cardinality<>r.cardinality OR current.cap_is_lead<>r.cap_is_lead OR current.cap_can_spawn<>r.cap_can_spawn OR current.cap_singleton_writer<>r.cap_singleton_writer)),
		a.tool,a.variant,COALESCE(a.launcher_id,''),a.model,a.provider,a.machine_id,a.desired,a.desired_rev,a.observed,a.generation,a.recovery_block, m.room_id,
		COALESCE((SELECT json_extract(e.payload_json,'$.evidence') FROM event e WHERE e.turn_id=(SELECT t.id FROM turn t WHERE t.agent_id=a.id AND t.outcome='unknown' ORDER BY t.id DESC LIMIT 1) AND e.kind='turn_process_exit' LIMIT 1),''),
		COALESCE((SELECT b.inbox_id FROM boundary_pending b WHERE b.agent_id=a.id AND b.state='open'),'')
		FROM agent a JOIN role r ON r.id=a.role_id JOIN room_member m ON m.member_id=a.id AND m.member_kind='agent' AND m.left_at IS NULL WHERE a.id=? ORDER BY m.joined_at LIMIT 1`, id).Scan(&c.ID, &c.Slug, &c.ProjectID, &c.RoleKey, &c.RoleEmoji, &c.RoleDeleted, &c.RoleUpdated, &c.Tool, &c.Variant, &c.LauncherID, &c.Model, &c.Provider, &c.MachineID, &c.Desired, &c.DesiredRev, &c.Observed, &c.Generation, &c.RecoveryBlock, &c.RoomID, &c.LastExitEvidence, &c.BoundaryPending)
	if err == nil {
		c.Switch, err = s.agentSwitch(ctx, id)
		if err == nil {
			c.BoundaryGrants, err = s.BoundaryGrants(ctx, id)
		}
		if err == nil && c.Observed == "in_turn" {
			c.WokenBy, err = s.wokenBy(ctx, id)
		}
	}
	return c, err
}

func (tx *Tx) SetAgentDesired(id, actorID, desired string, rev int64) (AgentCard, error) {
	return tx.setAgentDesired(id, "user", actorID, desired, rev, true)
}

// setAgentDesiredTrusted is SetAgentDesired for a caller that has already decided the actor may do this to every agent of the project
// (archiving a project is the owner's or an admin's, who need not be an operator member): the per-agent membership check is skipped.
func (tx *Tx) setAgentDesiredTrusted(id, actorID, desired string, rev int64) (AgentCard, error) {
	return tx.setAgentDesired(id, "user", actorID, desired, rev, false)
}

func (tx *Tx) SetAgentDesiredByAgent(id, actorID, desired string, rev int64) (AgentCard, error) {
	return tx.setAgentDesired(id, "agent", actorID, desired, rev, true)
}

func (tx *Tx) setAgentDesired(id, actorKind, actorID, desired string, rev int64, authorize bool) (AgentCard, error) {
	var projectID, current, observed, checkoutID, tool, roleKey, roleCardinality string
	var roleDeleted bool
	var oldRev int64
	var generation int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,a.desired,a.desired_rev,a.observed,a.generation,a.checkout_id,a.tool,r.role_key,r.cardinality,r.deleted_at IS NOT NULL
		FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, id).Scan(&projectID, &current, &oldRev, &observed, &generation, &checkoutID, &tool, &roleKey, &roleCardinality, &roleDeleted); err != nil {
		return AgentCard{}, err
	}
	if authorize {
		if err := tx.authorizeSpawnActor(projectID, actorKind, actorID, false); err != nil {
			return AgentCard{}, err
		}
	}
	if rev != oldRev {
		return AgentCard{}, ErrStaleRev
	}
	if desired == "active" {
		// Stopping and pausing an agent of an archived project stay possible (they are how it is freed); starting one is not.
		if err := tx.refuseArchived(projectID); err != nil {
			return AgentCard{}, err
		}
	}
	if desired != "stopped" {
		var pending bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM boundary_pending WHERE agent_id=? AND state='open')`, id).Scan(&pending); err != nil {
			return AgentCard{}, err
		}
		if pending {
			return AgentCard{}, ErrInvalidTransition
		}
	}
	if desired == "stopped" {
		if err := tx.withdrawBoundaryPending(id); err != nil {
			return AgentCard{}, err
		}
	}
	if current == "stopped" && desired == "active" {
		var machineID, path, roomID string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.machine_id,c.canonical_path,
			COALESCE(a.home_room_id,(SELECT m.room_id FROM room_member m WHERE m.member_kind='agent' AND m.member_id=a.id AND m.left_at IS NULL ORDER BY m.joined_at LIMIT 1),
				(SELECT r.id FROM room r WHERE r.project_id=a.project_id AND r.kind='home' LIMIT 1),'')
			FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.id=?`, id).Scan(&machineID, &path, &roomID); err != nil {
			return AgentCard{}, err
		}
		allowed, err := tx.boundaryAllowed(projectID, id, machineID, path)
		if err != nil {
			return AgentCard{}, err
		}
		if !allowed {
			if _, err := tx.createBoundaryPending(projectID, roomID, id, machineID, path, "reactivate", actorKind, actorID); err != nil {
				return AgentCard{}, err
			}
			return tx.agentCardByID(id)
		}
	}
	var switchState string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT state FROM agent_switch WHERE agent_id=?`, id).Scan(&switchState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AgentCard{}, err
	}
	if (switchState == "draining" || switchState == "starting") && desired != "stopped" {
		return AgentCard{}, ErrInvalidTransition
	}
	if switchState != "" && (desired == "stopped" || switchState == "failed" && desired == "active") {
		if _, err := tx.Exec(`DELETE FROM agent_switch WHERE agent_id=?`, id); err != nil {
			return AgentCard{}, err
		}
	}
	if desired != "active" && desired != "not_accepting" && desired != "stopped" {
		return AgentCard{}, ErrInvalidTransition
	}
	if current == desired {
		if desired == "active" {
			var limited bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM provider_limit WHERE agent_id=?)`, id).Scan(&limited); err != nil {
				return AgentCard{}, err
			}
			if limited {
				if err := tx.clearProviderLimit(id); err != nil {
					return AgentCard{}, err
				}
				if _, err := tx.Exec(`UPDATE agent SET desired_rev=desired_rev+1 WHERE id=?`, id); err != nil {
					return AgentCard{}, err
				}
				if _, err := tx.AppendEvent(Event{Kind: "agent_control_requested", ProjectID: projectID, AgentID: id,
					PayloadJSON: payloadJSON(map[string]string{"desired": desired, "actor_kind": actorKind, "actor_id": actorID, "reason": "provider_limit_retry"})}); err != nil {
					return AgentCard{}, err
				}
				return AgentCard{ID: id, ProjectID: projectID, Desired: desired, DesiredRev: oldRev + 1, Observed: observed, Generation: generation}, nil
			}
		}
		return AgentCard{}, ErrInvalidTransition
	}
	if current == "stopped" && desired == "not_accepting" {
		return AgentCard{}, ErrInvalidTransition
	}
	if current == "stopped" && observed != "stopped" {
		return AgentCard{}, ErrInvalidTransition
	}
	if current == "stopped" && desired == "active" {
		if roleDeleted {
			return AgentCard{}, ErrInvalidTransition
		}
		if roleCardinality == "singleton" {
			var occupied int
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM agent other JOIN role r ON r.id=other.role_id
				WHERE other.project_id=? AND other.id<>? AND r.role_key=? AND other.revoked_at IS NULL
				AND (other.desired!='stopped' OR other.observed!='stopped')`, projectID, id, roleKey).Scan(&occupied); err != nil {
				return AgentCard{}, err
			}
			if occupied > 0 {
				return AgentCard{}, ErrRoleOccupied
			}
		}
		var maxLive, live int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT max_live_agents FROM settings_project WHERE project_id=?`, projectID).Scan(&maxLive); err != nil {
			return AgentCard{}, err
		}
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM agent WHERE project_id=? AND id<>? AND (desired!='stopped' OR observed!='stopped') AND revoked_at IS NULL`, projectID, id).Scan(&live); err != nil {
			return AgentCard{}, err
		}
		if live >= maxLive {
			return AgentCard{}, ErrSpawnLimit
		}
		var writer bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT r.cap_singleton_writer FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, id).Scan(&writer); err != nil {
			return AgentCard{}, err
		}
		if writer {
			var state string
			var owner sql.NullString
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT writer_state,writer_agent_id FROM checkout WHERE id=?`, checkoutID).Scan(&state, &owner); err != nil {
				return AgentCard{}, err
			}
			if state != "none" {
				return AgentCard{}, ErrWriterOccupied
			}
			if _, err := tx.Exec(`UPDATE checkout SET writer_state='reserved',writer_agent_id=? WHERE id=?`, id, checkoutID); err != nil {
				return AgentCard{}, err
			}
		}
		generation++
		observed = "idle"
		sessionID, err := newID("ses")
		if err != nil {
			return AgentCard{}, err
		}
		nativeSessionID, err := nativeSessionID(tool)
		if err != nil {
			return AgentCard{}, err
		}
		if _, err = tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at,native_session_id) VALUES (?,?,?,'fresh_with_history',?,?)`, sessionID, id, generation, timestamp(), nullableString(nativeSessionID)); err != nil {
			return AgentCard{}, err
		}
	}
	if desired == "stopped" {
		if observed == "lost" {
			return AgentCard{}, ErrInvalidTransition
		}
		var lost int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn t WHERE t.agent_id=? AND t.exec='lost' AND NOT EXISTS(SELECT 1 FROM event e WHERE e.kind='recovery_override' AND (e.turn_id=t.id OR e.turn_id=t.parent_turn_id))`, id).Scan(&lost); err != nil {
			return AgentCard{}, err
		}
		if lost > 0 {
			return AgentCard{}, ErrInvalidTransition
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT t.id,t.mode,EXISTS(SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind='turn_dispatch_requested') FROM turn t WHERE t.agent_id=? AND t.exec='pending'`, id)
		if err != nil {
			return AgentCard{}, err
		}
		type pendingTurn struct {
			id, mode   string
			dispatched bool
		}
		pending := []pendingTurn{}
		for rows.Next() {
			var x pendingTurn
			if err := rows.Scan(&x.id, &x.mode, &x.dispatched); err != nil {
				rows.Close()
				return AgentCard{}, err
			}
			pending = append(pending, x)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return AgentCard{}, err
		}
		for _, x := range pending {
			if x.dispatched {
				continue
			}
			if x.mode == "correction" || x.mode == "diagnostic" {
				if err := tx.abandonSpecialTurn(x.id, "stop_before_dispatch"); err != nil {
					return AgentCard{}, err
				}
				continue
			}
			var origin sql.NullString
			var continuation bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT parent_turn_id,EXISTS(SELECT 1 FROM event WHERE turn_id=? AND kind='recovery_decision') FROM turn WHERE id=?`, x.id, x.id).
				Scan(&origin, &continuation); err != nil {
				return AgentCard{}, err
			}
			if continuation {
				if _, err := tx.Exec(`UPDATE turn SET exec='killed',outcome='cancelled',settle='settled',ended_at=? WHERE id=?`, timestamp(), x.id); err != nil {
					return AgentCard{}, err
				}
				if _, err := tx.Exec(`UPDATE note SET state='aborted' WHERE id IN(SELECT note_id FROM turn_note WHERE turn_id=?) AND state='queued'`, x.id); err != nil {
					return AgentCard{}, err
				}
				if _, err := tx.AppendEvent(Event{Kind: "recovery_continuation_cancelled", ProjectID: projectID, AgentID: id, TurnID: x.id,
					PayloadJSON: payloadJSON(map[string]string{"reason": "stop_before_dispatch"})}); err != nil {
					return AgentCard{}, err
				}
				if origin.Valid {
					if err := tx.resolveQuiescenceWaitingItems(origin.String, actorID, "stop", "stop_before_dispatch"); err != nil {
						return AgentCard{}, err
					}
				}
				continue
			}
			if _, err := tx.Exec(`UPDATE turn SET exec='killed',outcome='cancelled',settle='settled',ended_at=? WHERE id=?`, timestamp(), x.id); err != nil {
				return AgentCard{}, err
			}
			if _, err := tx.Exec(`UPDATE note SET state='queued' WHERE id IN(SELECT note_id FROM turn_note WHERE turn_id=?) AND state='dispatched'`, x.id); err != nil {
				return AgentCard{}, err
			}
			if _, err := tx.AppendEvent(Event{Kind: "turn_dispatch_cancelled", ProjectID: projectID, AgentID: id, TurnID: x.id, PayloadJSON: payloadJSON(map[string]string{"reason": "stop_before_dispatch"})}); err != nil {
				return AgentCard{}, err
			}
		}
		var running, pendingUncertain int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE agent_id=? AND exec='running'`, id).Scan(&running); err != nil {
			return AgentCard{}, err
		}
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE agent_id=? AND exec='pending'`, id).Scan(&pendingUncertain); err != nil {
			return AgentCard{}, err
		}
		if running > 0 {
			observed = "interrupting"
		} else if pendingUncertain == 0 {
			observed = "stopped"
		} else {
			observed = "idle"
		}
		if _, err := tx.Exec(`UPDATE checkout SET writer_state='releasing' WHERE id=? AND writer_agent_id=?`, checkoutID, id); err != nil {
			return AgentCard{}, err
		}
		if observed == "stopped" {
			if _, err := tx.Exec(`UPDATE checkout SET writer_state='none',writer_agent_id=NULL WHERE id=? AND writer_agent_id=?`, checkoutID, id); err != nil {
				return AgentCard{}, err
			}
		}
	}
	if _, err := tx.Exec(`UPDATE agent SET desired=?,desired_rev=desired_rev+1,observed=?,generation=? WHERE id=?`, desired, observed, generation, id); err != nil {
		return AgentCard{}, err
	}
	if current == "stopped" && desired == "active" {
		if err := tx.clearProviderLimit(id); err != nil {
			return AgentCard{}, err
		}
	}
	if _, err := tx.AppendEvent(Event{Kind: "agent_control_requested", ProjectID: projectID, AgentID: id,
		PayloadJSON: payloadJSON(map[string]string{"desired": desired, "actor_kind": actorKind, "actor_id": actorID})}); err != nil {
		return AgentCard{}, err
	}
	return AgentCard{ID: id, ProjectID: projectID, Desired: desired, DesiredRev: oldRev + 1, Observed: observed, Generation: generation}, nil
}

func (s *Store) ConfirmAgentProcessExit(ctx context.Context, id string) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.confirmStoppedIfQuiescent(id) })
}

func (tx *Tx) confirmStoppedIfQuiescent(id string) error {
	var checkoutID, desired, observed, projectID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT checkout_id,desired,observed,project_id FROM agent WHERE id=?`, id).Scan(&checkoutID, &desired, &observed, &projectID); err != nil {
		return err
	}
	if desired != "stopped" || observed == "stopped" {
		return nil
	}
	var open int
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn t WHERE t.agent_id=? AND (t.exec IN ('pending','running') OR (t.exec='lost' AND NOT EXISTS(SELECT 1 FROM event e WHERE e.kind='recovery_override' AND (e.turn_id=t.id OR e.turn_id=t.parent_turn_id))))`, id).Scan(&open); err != nil {
		return err
	}
	if open > 0 {
		return nil
	}
	if _, err := tx.Exec(`UPDATE agent SET observed='stopped' WHERE id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE checkout SET writer_state='none',writer_agent_id=NULL WHERE id=? AND writer_agent_id=?`, checkoutID, id); err != nil {
		return err
	}
	_, err := tx.AppendEvent(Event{Kind: "agent_stopped", ProjectID: projectID, AgentID: id, PayloadJSON: "{}"})
	return err
}

func (s *Store) ConfirmSpawn(ctx context.Context, id string, generation int) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var projectID string
		var current int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,generation FROM agent WHERE id=?`, id).Scan(&projectID, &current); err != nil {
			return err
		}
		if current != generation {
			return ErrStaleGeneration
		}
		var count int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM event WHERE agent_id=? AND kind='agent_spawn_confirmed' AND json_extract(payload_json,'$.generation')=?`, id, generation).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return nil
		}
		_, err := tx.AppendEvent(Event{Kind: "agent_spawn_confirmed", ProjectID: projectID, AgentID: id, PayloadJSON: fmt.Sprintf(`{"generation":%d}`, generation)})
		return err
	})
}

// FailSpawn records a supervisor rejection as terminal for this generation.
// The agent's desired state is left intact for owner inspection or control.
func (s *Store) FailSpawn(ctx context.Context, id string, generation int, reason string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		var projectID string
		var current int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,generation FROM agent WHERE id=?`, id).Scan(&projectID, &current); err != nil {
			return err
		}
		if current != generation {
			return ErrStaleGeneration
		}
		var already bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE agent_id=? AND kind='agent_spawn_failed' AND json_extract(payload_json,'$.generation')=?)`, id, generation).Scan(&already); err != nil {
			return err
		}
		if already {
			return nil
		}
		payload, err := json.Marshal(map[string]any{"generation": generation, "reason": reason})
		if err != nil {
			return err
		}
		_, err = tx.AppendEvent(Event{Kind: "agent_spawn_failed", ProjectID: projectID, AgentID: id,
			PayloadJSON: string(payload)})
		return err
	})
}

type PendingSpawn struct {
	AgentID                                                             string
	Generation                                                          int
	DesiredRev                                                          int64
	MachineID, CheckoutPath, Tool, Variant, LauncherID, Model, Provider string
}

func (s *Store) AgentOpenTurnCount(ctx context.Context, id string) (int, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn WHERE agent_id=? AND exec IN ('pending','running','lost')`, id).Scan(&count)
	return count, err
}

func (s *Store) CheckoutWriterState(ctx context.Context, id string) (string, string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", "", err
	}
	var state string
	var owner sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT writer_state,writer_agent_id FROM checkout WHERE id=?`, id).Scan(&state, &owner)
	return state, owner.String, err
}

func (s *Store) PendingSpawnByID(ctx context.Context, id string) (PendingSpawn, bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return PendingSpawn{}, false, err
	}
	var p PendingSpawn
	err := s.db.QueryRowContext(ctx, `SELECT a.id,a.generation,a.desired_rev,a.machine_id,c.canonical_path,a.tool,a.variant,COALESCE(a.launcher_id,''),a.model,a.provider FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.id=? AND a.revoked_at IS NULL AND a.desired!='stopped' AND EXISTS(SELECT 1 FROM event e WHERE e.agent_id=a.id AND e.kind='agent_spawn_requested') AND NOT EXISTS(SELECT 1 FROM event e WHERE e.agent_id=a.id AND e.kind IN ('agent_spawn_confirmed','agent_spawn_failed') AND json_extract(e.payload_json,'$.generation')=a.generation)`, id).Scan(&p.AgentID, &p.Generation, &p.DesiredRev, &p.MachineID, &p.CheckoutPath, &p.Tool, &p.Variant, &p.LauncherID, &p.Model, &p.Provider)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingSpawn{}, false, nil
	}
	if err != nil {
		return p, false, err
	}
	pending := []PendingSpawn{p}
	if err := s.settlePendingModels(ctx, pending); err != nil {
		return p, false, err
	}
	return pending[0], true, nil
}

func (s *Store) PendingSpawnsForMachine(ctx context.Context, machineID string) ([]PendingSpawn, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.generation,a.desired_rev,a.machine_id,c.canonical_path,a.tool,a.variant,COALESCE(a.launcher_id,''),a.model,a.provider FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.machine_id=? AND a.revoked_at IS NULL AND a.desired!='stopped' AND EXISTS(SELECT 1 FROM event e WHERE e.agent_id=a.id AND e.kind='agent_spawn_requested') AND NOT EXISTS(SELECT 1 FROM event e WHERE e.agent_id=a.id AND e.kind IN ('agent_spawn_confirmed','agent_spawn_failed') AND json_extract(e.payload_json,'$.generation')=a.generation) ORDER BY a.id LIMIT 16`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingSpawn{}
	for rows.Next() {
		var p PendingSpawn
		if err := rows.Scan(&p.AgentID, &p.Generation, &p.DesiredRev, &p.MachineID, &p.CheckoutPath, &p.Tool, &p.Variant, &p.LauncherID, &p.Model, &p.Provider); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, s.settlePendingModels(ctx, out)
}

func (s *Store) AckAgentControl(ctx context.Context, id string, generation int, ref string) error {
	rev, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return err
	}
	return s.Tx(ctx, func(tx *Tx) error {
		var projectID string
		var currentGeneration int
		var currentRev int64
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,generation,desired_rev FROM agent WHERE id=?`, id).Scan(&projectID, &currentGeneration, &currentRev); err != nil {
			return err
		}
		if generation != currentGeneration || rev != currentRev {
			return ErrStaleRev
		}
		_, err := tx.AppendEvent(Event{Kind: "agent_control_applied", ProjectID: projectID, AgentID: id, PayloadJSON: payloadJSON(map[string]string{"desired_rev": ref})})
		return err
	})
}

// describeSession is how a continued session is named in the room: its title in quotes when it has one, and the start of its id.
func describeSession(session *ResumeSession) string {
	short := session.ID
	if len(short) > 8 {
		short = short[:8]
	}
	if session.Title == "" {
		return short
	}
	return fmt.Sprintf("%q (%s)", session.Title, short)
}

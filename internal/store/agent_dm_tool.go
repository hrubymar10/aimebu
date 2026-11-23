package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// AgentDMNewChatsPerHour is how many new chats one lead may open in an hour: the per-chat limit does not stop a fan-out to many agents.
	AgentDMNewChatsPerHour = 10
	// MaxAgentDMBody bounds one dm_agent message, in bytes.
	MaxAgentDMBody = 20000
)

var (
	// ErrAgentUnavailable is a chat with an agent that is revoked, stopped or not accepting work.
	ErrAgentUnavailable = errors.New("agent_unavailable")
	// ErrDMTurnCap is a chat that already used its hourly agent turns.
	ErrDMTurnCap = errors.New("dm_turn_cap")
	// ErrDMFanOutCap is a lead that already opened its hourly share of new chats.
	ErrDMFanOutCap = errors.New("dm_fanout_cap")
	// ErrCrossProjectLeadOnly is a lead's chat with an agent of another project that is not that project's lead.
	ErrCrossProjectLeadOnly = errors.New("across projects you may message only that project's lead")
)

// AgentNotFoundError is a slug@project target that names no live agent in a project the people behind the turn may reach. It is returned
// only when they reach the named project, so it tells them nothing they could not see; otherwise the answer is ErrForbidden.
type AgentNotFoundError struct{ Slug, Project string }

func (e *AgentNotFoundError) Error() string {
	return fmt.Sprintf("no agent %s in project %s", e.Slug, e.Project)
}

// AgentAmbiguousError is a slug@project target that names more than one live agent the people behind the turn may reach (two projects share
// the name). IDs lists only those agents.
type AgentAmbiguousError struct {
	Slug, Project string
	IDs           []string
}

func (e *AgentAmbiguousError) Error() string {
	return fmt.Sprintf("%s@%s names more than one agent: use the full agent ID: %s", e.Slug, e.Project, strings.Join(e.IDs, ", "))
}

// resolveDMTarget turns a dm_agent target into an agent ID. An agt_ ID passes through unchanged. slug@project (the project's name or ID,
// the slug case-insensitive) is resolved only among projects the people behind the turn may reach (see leadMayReach), so a name never
// confirms an agent or a project the lead's people cannot see: with no reachable project of that name the answer is ErrForbidden, with one
// but no such live agent it is AgentNotFoundError, and with several matches AgentAmbiguousError.
func (tx *Tx) resolveDMTarget(callerID, callerProject string, restrictToOwn, skipReach bool, target string) (string, error) {
	slug, project, ok := strings.Cut(target, "@")
	if !ok {
		return target, nil
	}
	slug, project = strings.ToLower(strings.TrimSpace(slug)), strings.TrimSpace(project)
	if slug == "" || project == "" {
		return "", ErrForbidden
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id,name FROM project WHERE id=? OR LOWER(name)=LOWER(?) ORDER BY id`, project, project)
	if err != nil {
		return "", err
	}
	type named struct{ id, name string }
	var projects []named
	for rows.Next() {
		var p named
		if err := rows.Scan(&p.id, &p.name); err != nil {
			rows.Close()
			return "", err
		}
		projects = append(projects, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	var found []string
	reachable := ""
	for _, p := range projects {
		if restrictToOwn && p.id != callerProject {
			continue
		}
		// An agent that is neither lead nor coordinator reaches its own project by being in it, whoever woke it.
		allowed := skipReach
		if !allowed {
			var err error
			if allowed, err = tx.leadMayReach(callerID, callerProject, p.id); err != nil {
				return "", err
			}
		}
		if !allowed {
			continue
		}
		reachable = p.name
		var id string
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM agent WHERE project_id=? AND slug=? AND revoked_at IS NULL`, p.id, slug).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", err
		}
		found = append(found, id)
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		return "", &AgentAmbiguousError{Slug: slug, Project: project, IDs: found}
	case reachable != "":
		return "", &AgentNotFoundError{Slug: slug, Project: reachable}
	}
	return "", ErrForbidden
}

// AgentDMResult is what dm_agent returns: the chat and the message that went into it.
type AgentDMResult struct {
	RoomID    string `json:"room_id"`
	MessageID string `json:"message_id"`
	No        int64  `json:"no"`
	Created   bool   `json:"created"`
}

// DMAgentAsTool is an agent's dm_agent call: it opens (or finds) the chat with the target agent and posts the message into it, which wakes the
// target. A lead may message any agent of its own project, and in another project only that project's lead; a flow coordinator that is not a
// lead (the dispatcher) only agents of its own project that it may manage; any other agent only its own project's lead or dispatcher.
// Everything is decided here, never by the model: for a lead or coordinator the people who caused the turn must reach the target's project
// (see leadMayReach); the target must be live and its project not archived; and a caller opens only so many new chats an hour and a chat takes
// only so many turns.
func (tx *Tx) DMAgentAsTool(callerID, targetID, body string) (AgentDMResult, error) {
	var callerProject, callerDesired string
	var lead bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,a.desired,r.cap_is_lead FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=? AND a.revoked_at IS NULL`, callerID).
		Scan(&callerProject, &callerDesired, &lead); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentDMResult{}, ErrForbidden
		}
		return AgentDMResult{}, err
	}
	if callerDesired != "active" {
		return AgentDMResult{}, ErrForbidden
	}
	// Three kinds of caller: a lead (its own project's agents, and another project's lead); a coordinator the flow lets manage agents (the
	// dispatcher in the leader-dispatcher flow), which may message agents of its own project that it may manage; and any other agent, which
	// may message only its own project's lead or dispatcher.
	coordinator := false
	if !lead {
		switch err := tx.authorizeSpawnActor(callerProject, "agent", callerID, false); {
		case err == nil:
			coordinator = true
		case errors.Is(err, ErrForbidden):
		default:
			return AgentDMResult{}, err
		}
	}
	member := !lead && !coordinator
	targetID, err := tx.resolveDMTarget(callerID, callerProject, !lead, member, targetID)
	if err != nil {
		return AgentDMResult{}, err
	}
	if targetID == callerID {
		return AgentDMResult{}, ErrInvalidTransition
	}
	var targetProject, targetDesired, targetRole string
	var targetRevoked, targetArchived, targetLead bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,a.desired,a.revoked_at IS NOT NULL,p.archived_at IS NOT NULL,r.role_key,r.cap_is_lead
		FROM agent a JOIN project p ON p.id=a.project_id JOIN role r ON r.id=a.role_id WHERE a.id=?`, targetID).
		Scan(&targetProject, &targetDesired, &targetRevoked, &targetArchived, &targetRole, &targetLead); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// An agent that does not exist answers like one the lead may not reach, so an ID cannot be probed.
			return AgentDMResult{}, ErrForbidden
		}
		return AgentDMResult{}, err
	}
	if member {
		if targetProject != callerProject || !targetLead && targetRole != "dispatcher" {
			return AgentDMResult{}, ErrForbidden
		}
	}
	if coordinator {
		if targetProject != callerProject {
			return AgentDMResult{}, ErrForbidden
		}
		if err := tx.flowActorMayTargetAgent(callerProject, callerID, targetID, targetRole); err != nil {
			if errors.Is(err, ErrTeamFlowRule) {
				return AgentDMResult{}, ErrForbidden
			}
			return AgentDMResult{}, err
		}
	}
	if !member {
		allowed, err := tx.leadMayReach(callerID, callerProject, targetProject)
		if err != nil {
			return AgentDMResult{}, err
		}
		if !allowed {
			return AgentDMResult{}, ErrForbidden
		}
	}
	// Checked after the reach, so the refusal says nothing about an agent in a project the people cannot reach.
	if lead && targetProject != callerProject && !targetLead {
		return AgentDMResult{}, ErrCrossProjectLeadOnly
	}
	if err := tx.refuseArchived(callerProject); err != nil {
		return AgentDMResult{}, err
	}
	if targetArchived {
		return AgentDMResult{}, ErrProjectArchived
	}
	if targetRevoked || targetDesired != "active" {
		return AgentDMResult{}, ErrAgentUnavailable
	}
	var existing string
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE dm_key=?`, agentDMKey(callerID, targetID)).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var opened int
		cutoff := time.Now().UTC().Add(-time.Hour).Format(timestampLayout)
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM room WHERE project_id=`+agentDMProjectSQL+` AND created_by=? AND created_at>=?`, callerID, cutoff).Scan(&opened); err != nil {
			return AgentDMResult{}, err
		}
		if opened >= AgentDMNewChatsPerHour {
			return AgentDMResult{}, ErrDMFanOutCap
		}
	case err != nil:
		return AgentDMResult{}, err
	}
	room, created, err := tx.OpenAgentDM(callerID, targetID, callerID)
	if err != nil {
		return AgentDMResult{}, err
	}
	if room.Lifecycle != "open" {
		return AgentDMResult{}, ErrInvalidTransition
	}
	turns, err := tx.agentChatTurns(room.ID)
	if err != nil {
		return AgentDMResult{}, err
	}
	if turns >= AgentDMTurnsPerHour {
		return AgentDMResult{}, ErrDMTurnCap
	}
	message, err := tx.InsertMessage(Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: callerID, Body: body})
	if err != nil {
		return AgentDMResult{}, err
	}
	return AgentDMResult{RoomID: room.ID, MessageID: message.ID, No: message.No, Created: created}, nil
}

// leadMayReach is the authorization of a chat across projects. The people whose messages caused the lead's current turn (and the turns it
// continues, for a correction or a retry) must every one reach the target's project; if no person caused it (an agent or a schedule woke
// the lead), an owner of the lead's own project must. Reach is owner or operator of the target project, and the instance owner reaches
// everything. A viewer does not: a chat makes the target work and spend.
func (tx *Tx) leadMayReach(leadID, leadProject, targetProject string) (bool, error) {
	var turn string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM turn WHERE agent_id=? AND exec IN ('pending','running') ORDER BY id DESC LIMIT 1`, leadID).Scan(&turn); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `WITH RECURSIVE chain(id) AS (
			SELECT ? UNION SELECT t.parent_turn_id FROM turn t JOIN chain c ON t.id=c.id WHERE t.parent_turn_id IS NOT NULL)
		SELECT DISTINCT m.author_id FROM chain JOIN turn_note tn ON tn.turn_id=chain.id JOIN note n ON n.id=tn.note_id
		JOIN message m ON m.id=n.message_id WHERE m.author_kind='user' ORDER BY 1`, turn)
	if err != nil {
		return false, err
	}
	var people []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		people = append(people, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if len(people) == 0 {
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT user_id FROM membership WHERE project_id=? AND role='owner' ORDER BY user_id`, leadProject)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return false, err
			}
			people = append(people, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, err
		}
		for _, id := range people {
			if ok, err := userReaches(tx.ctx, tx.tx, id, targetProject); err != nil || ok {
				return ok, err
			}
		}
		return false, nil
	}
	for _, id := range people {
		if ok, err := userReaches(tx.ctx, tx.tx, id, targetProject); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// ValidAgentDMBody checks what a lead sends: some text, within the limit.
func ValidAgentDMBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("body is required")
	}
	if len(body) > MaxAgentDMBody {
		return fmt.Errorf("body is at most %d bytes", MaxAgentDMBody)
	}
	return nil
}

// OtherLeader is an active lead of another project, as list_agents shows it to a lead that may message it.
type OtherLeader struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	ProjectSlug string `json:"project_slug"`
	Observed    string `json:"observed"`
}

// OtherLeadersForTool lists the active leads of other, unarchived projects, limited to the projects dm_agent would let the caller reach (the
// people behind its turn: see leadMayReach), so the list names nothing a dm_agent could not already address.
func (s *Store) OtherLeadersForTool(ctx context.Context, callerID, callerProject string) ([]OtherLeader, error) {
	out := []OtherLeader{}
	err := s.Tx(ctx, func(tx *Tx) error {
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT a.id,a.slug,p.id,p.name,a.observed FROM agent a JOIN role r ON r.id=a.role_id JOIN project p ON p.id=a.project_id
			WHERE r.cap_is_lead=1 AND a.desired='active' AND a.revoked_at IS NULL AND p.archived_at IS NULL AND a.project_id<>? ORDER BY p.name,a.slug,a.id`, callerProject)
		if err != nil {
			return err
		}
		type candidate struct {
			OtherLeader
			project string
		}
		var candidates []candidate
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.ID, &c.Slug, &c.project, &c.ProjectSlug, &c.Observed); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		reach := map[string]bool{}
		for _, c := range candidates {
			allowed, seen := reach[c.project]
			if !seen {
				if allowed, err = tx.leadMayReach(callerID, callerProject, c.project); err != nil {
					return err
				}
				reach[c.project] = allowed
			}
			if allowed {
				out = append(out, c.OtherLeader)
			}
		}
		return nil
	})
	return out, err
}

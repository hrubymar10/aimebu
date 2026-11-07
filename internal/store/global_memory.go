package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

func globalKnowledgeCap(scope string) (int, error) {
	switch scope {
	case "user_profile":
		return 2048, nil
	case "agent_shared_notes":
		return 4096, nil
	default:
		return 0, ErrInvalidScope
	}
}

func (s *Store) ListGlobalKnowledge(ctx context.Context, scope, key string) ([]KnowledgeRecord, error) {
	if _, err := globalKnowledgeCap(scope); err != nil {
		return nil, err
	}
	if key == "" || scope == "agent_shared_notes" && key != "global" {
		return nil, ErrInvalidScope
	}
	rows, err := s.db.QueryContext(ctx, "SELECT g.id,g.body,g.rev,g.author_kind,g.author_id,g.created_at,g.updated_at,g.source_message_id,COALESCE(u.login,''),g.active FROM global_memory g LEFT JOIN user u ON u.id=g.scope_key AND g.scope='user_profile' WHERE g.scope=? AND g.scope_key=? ORDER BY g.created_at,g.id", scope, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []KnowledgeRecord{}
	for rows.Next() {
		item := KnowledgeRecord{Scope: scope, ScopeID: key}
		var source sql.NullString
		if err := rows.Scan(&item.ID, &item.Body, &item.Rev, &item.AuthorKind, &item.AuthorID, &item.CreatedAt, &item.UpdatedAt, &source, &item.ScopeLabel, &item.Active); err != nil {
			return nil, err
		}
		item.SourceMessageID = source.String
		records = append(records, item)
	}
	return records, rows.Err()
}

// ManageGlobalKnowledge uses the same revision and scope-budget rules as
// project memory. The caller project gates agent writes; human owners can
// inspect and curate global records even while project memory is disabled.
func (s *Store) ManageGlobalKnowledge(ctx context.Context, scope, key, projectID, actorKind, actorID, id string, rev int64, body string, remove bool) (KnowledgeRecord, error) {
	return s.ManageGlobalKnowledgeWithSource(ctx, scope, key, projectID, actorKind, actorID, id, rev, body, remove, "")
}

func (s *Store) ManageGlobalKnowledgeWithSource(ctx context.Context, scope, key, projectID, actorKind, actorID, id string, rev int64, body string, remove bool, sourceID string) (KnowledgeRecord, error) {
	cap, err := globalKnowledgeCap(scope)
	if err != nil {
		return KnowledgeRecord{}, err
	}
	if key == "" || scope == "agent_shared_notes" && key != "global" {
		return KnowledgeRecord{}, ErrInvalidScope
	}
	item := KnowledgeRecord{Scope: scope, ScopeID: key}
	err = s.Tx(ctx, func(tx *Tx) error {
		if sourceID != "" {
			if actorKind != "agent" || id != "" {
				return ErrMemorySource
			}
			if err := tx.validateMemorySource(projectID, actorID, sourceID); err != nil {
				return err
			}
		}
		if actorKind == "user" {
			if level, err := tx.privilege(actorID); err != nil {
				return err
			} else if level < privAdmin {
				return ErrForbidden
			}
		} else if actorKind == "agent" {
			if scope == "user_profile" {
				if member, err := tx.isProjectMember(projectID, key); err != nil {
					return err
				} else if !member {
					return ErrForbidden
				}
			}
			var enabled bool
			if err := tx.tx.QueryRowContext(ctx, "SELECT p.memory_enabled FROM agent a JOIN settings_project p ON p.project_id=a.project_id WHERE a.id=? AND a.project_id=? AND a.revoked_at IS NULL", actorID, projectID).Scan(&enabled); err != nil {
				return err
			}
			if !enabled {
				return ErrMemoryDisabled
			}
		} else {
			return ErrForbidden
		}
		if scope == "user_profile" {
			var count int
			if err := tx.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM user WHERE id=?", key).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				return ErrInvalidScope
			}
		}
		if id == "" && (remove || rev != 0) || id != "" && rev < 1 {
			return ErrInvalidTransition
		}
		body = strings.TrimSpace(body)
		if !remove && (body == "" || !utf8.ValidString(body) || utf8.RuneCountInString(body) > knowledgeBodyRunes) {
			return ErrKnowledgeLimit
		}
		oldBytes, oldPendingBytes := 0, 0
		if id != "" {
			item.ID = id
			var source sql.NullString
			if err := tx.tx.QueryRowContext(ctx, "SELECT body,rev,author_kind,author_id,created_at,updated_at,active,source_message_id FROM global_memory WHERE id=? AND scope=? AND scope_key=?", id, scope, key).
				Scan(&item.Body, &item.Rev, &item.AuthorKind, &item.AuthorID, &item.CreatedAt, &item.UpdatedAt, &item.Active, &source); err != nil {
				return err
			}
			item.SourceMessageID = source.String
			if actorKind == "agent" {
				if scope == "agent_shared_notes" {
					// The notes every project's agents read belong to whoever wrote them: an agent edits or deletes its own (waiting for approval or not), never
					// the owner's, never another agent's, so no project can vandalise what the owner approved.
					if item.AuthorKind != "agent" || item.AuthorID != actorID {
						return ErrForbidden
					}
				} else if !item.Active {
					return ErrForbidden
				}
			}
			if item.Rev != rev {
				return ErrStaleRev
			}
			// What a note held counted against a cap only if it was live: a note waiting for approval is counted apart (below).
			oldPending := actorKind == "agent" && scope == "agent_shared_notes" && !item.Active
			if !oldPending {
				oldBytes = len(item.Body)
			}
			if oldPending {
				oldPendingBytes = len(item.Body)
			}
		}
		if !remove {
			if actorKind == "agent" && scope == "agent_shared_notes" {
				// What an agent writes waits for approval, so it is not in the shared cap yet. Each agent's waiting notes have a cap of their own, the same
				// size, so one agent can neither fill the cap with notes nobody has read nor store without limit.
				var pending int
				if err := tx.tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(length(CAST(body AS BLOB))),0) FROM global_memory WHERE scope=? AND scope_key=? AND active=0 AND author_kind='agent' AND author_id=?", scope, key, actorID).Scan(&pending); err != nil {
					return err
				}
				if pending-oldPendingBytes+len(body) > cap {
					return ErrKnowledgeLimit
				}
			} else {
				var used int
				if err := tx.tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(length(CAST(body AS BLOB))),0) FROM global_memory WHERE scope=? AND scope_key=? AND (active=1 OR author_kind!='agent')", scope, key).Scan(&used); err != nil {
					return err
				}
				if used-oldBytes+len(body) > cap {
					return ErrKnowledgeLimit
				}
			}
		}
		now := timestamp()
		action := "updated"
		if id == "" {
			id, err = newID("knw")
			if err != nil {
				return err
			}
			// What an agent adds to the notes every project's agents read waits for the owner or an admin to switch it on: nobody has read it yet, and it would
			// otherwise reach every other project's agents at once.
			active := !(actorKind == "agent" && scope == "agent_shared_notes")
			_, err = tx.Exec("INSERT INTO global_memory(id,scope,scope_key,body,rev,author_kind,author_id,created_at,updated_at,source_message_id,active) VALUES (?,?,?,?,1,?,?,?,?,?,?)", id, scope, key, body, actorKind, actorID, now, now, nullable(sourceID), active)
			item = KnowledgeRecord{ID: id, Scope: scope, ScopeID: key, Body: body, Rev: 1, AuthorKind: actorKind, AuthorID: actorID, CreatedAt: now, UpdatedAt: now, Active: active, SourceMessageID: sourceID}
			action = "created"
		} else if remove {
			_, err = tx.Exec("DELETE FROM global_memory WHERE id=? AND scope=? AND scope_key=? AND rev=?", id, scope, key, rev)
			item.Rev++
			item.AuthorKind, item.AuthorID, item.UpdatedAt = actorKind, actorID, now
			action = "deleted"
		} else {
			// A note an agent changes waits for approval again, whoever approved the old words.
			pending := actorKind == "agent" && scope == "agent_shared_notes"
			_, err = tx.Exec("UPDATE global_memory SET body=?,rev=rev+1,author_kind=?,author_id=?,updated_at=?,active=CASE WHEN ? THEN 0 ELSE active END WHERE id=? AND scope=? AND scope_key=? AND rev=?", body, actorKind, actorID, now, pending, id, scope, key, rev)
			item.Body, item.Rev = body, rev+1
			if pending {
				item.Active = false
			}
			item.AuthorKind, item.AuthorID, item.UpdatedAt = actorKind, actorID, now
		}
		if err != nil {
			return err
		}
		// A note that waits for approval has an inbox item for the people who can approve it; any other state of the note closes it.
		if scope == "agent_shared_notes" {
			if err := tx.syncNoteApproval(item.ID); err != nil {
				return err
			}
		}
		// Every project's stream can refresh the global knowledge viewer. The
		// event contains no record body or profile key.
		rows, err := tx.tx.QueryContext(ctx, "SELECT id FROM project WHERE system=0")
		if err != nil {
			return err
		}
		var projects []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			projects = append(projects, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, project := range projects {
			if _, err := tx.AppendEvent(Event{Kind: "global_knowledge_" + action, ProjectID: project,
				PayloadJSON: payloadJSON(map[string]string{"id": id, "scope": scope, "rev": strconv.FormatInt(item.Rev, 10), "actor_kind": actorKind, "actor_id": actorID})}); err != nil {
				return err
			}
		}
		return nil
	})
	return item, err
}

func (s *Store) ListAllGlobalKnowledge(ctx context.Context, scope string) ([]KnowledgeRecord, error) {
	return s.listGlobalKnowledge(ctx, scope, false)
}

func (s *Store) ListActiveGlobalKnowledge(ctx context.Context, scope string) ([]KnowledgeRecord, error) {
	return s.listGlobalKnowledge(ctx, scope, true)
}

func (s *Store) listGlobalKnowledge(ctx context.Context, scope string, activeOnly bool) ([]KnowledgeRecord, error) {
	if _, err := globalKnowledgeCap(scope); err != nil {
		return nil, err
	}
	activeClause := ""
	if activeOnly {
		activeClause = " AND g.active=1"
	}
	rows, err := s.db.QueryContext(ctx, "SELECT g.id,g.scope_key,g.body,g.rev,g.author_kind,g.author_id,g.created_at,g.updated_at,g.source_message_id,COALESCE(u.login,''),g.active FROM global_memory g LEFT JOIN user u ON u.id=g.scope_key AND g.scope='user_profile' WHERE g.scope=?"+activeClause+" ORDER BY g.scope_key,g.created_at,g.id", scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []KnowledgeRecord{}
	for rows.Next() {
		item := KnowledgeRecord{Scope: scope}
		var source sql.NullString
		if err := rows.Scan(&item.ID, &item.ScopeID, &item.Body, &item.Rev, &item.AuthorKind, &item.AuthorID, &item.CreatedAt, &item.UpdatedAt, &source, &item.ScopeLabel, &item.Active); err != nil {
			return nil, err
		}
		item.SourceMessageID = source.String
		records = append(records, item)
	}
	return records, rows.Err()
}

func (s *Store) GlobalKnowledgeAccess(ctx context.Context, userID string) (string, error) {
	var projectID string
	err := s.db.QueryRowContext(ctx, "SELECT project_id FROM membership WHERE user_id=? ORDER BY CASE role WHEN 'owner' THEN 0 WHEN 'operator' THEN 1 ELSE 2 END,project_id LIMIT 1", userID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		// The owner and the admins manage global memory without belonging to a project; the project is only provenance for agents.
		if admin, adminErr := s.IsGlobalAdmin(ctx, userID); adminErr == nil && admin {
			return "", nil
		}
		return "", ErrForbidden
	}
	if err != nil {
		return "", fmt.Errorf("global knowledge access: %w", err)
	}
	return projectID, nil
}

// SetGlobalKnowledgeActive lets the installation owner or an admin keep a record out of
// every agent's cross-project context and recall without deleting it.
func (s *Store) SetGlobalKnowledgeActive(ctx context.Context, scope, key, actorID, id string, rev int64, active bool) (KnowledgeRecord, error) {
	if _, err := globalKnowledgeCap(scope); err != nil {
		return KnowledgeRecord{}, err
	}
	if key == "" || scope == "agent_shared_notes" && key != "global" || id == "" || rev < 1 {
		return KnowledgeRecord{}, ErrInvalidTransition
	}
	var item KnowledgeRecord
	err := s.Tx(ctx, func(tx *Tx) error {
		if level, err := tx.privilege(actorID); err != nil {
			return err
		} else if level < privAdmin {
			return ErrForbidden
		}
		item = KnowledgeRecord{ID: id, Scope: scope, ScopeID: key}
		if err := tx.tx.QueryRowContext(ctx, "SELECT body,rev,author_kind,author_id,created_at,updated_at,active FROM global_memory WHERE id=? AND scope=? AND scope_key=?", id, scope, key).
			Scan(&item.Body, &item.Rev, &item.AuthorKind, &item.AuthorID, &item.CreatedAt, &item.UpdatedAt, &item.Active); err != nil {
			return err
		}
		if item.Rev != rev {
			return ErrStaleRev
		}
		if item.Active == active {
			return nil
		}
		if active {
			// Approving a waiting note makes it live, so it counts against the shared cap from now on.
			cap, err := globalKnowledgeCap(scope)
			if err != nil {
				return err
			}
			var used int
			if err := tx.tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(length(CAST(body AS BLOB))),0) FROM global_memory WHERE scope=? AND scope_key=? AND (active=1 OR author_kind!='agent')", scope, key).Scan(&used); err != nil {
				return err
			}
			if used+len(item.Body) > cap {
				return ErrKnowledgeLimit
			}
		}
		now := timestamp()
		if _, err := tx.Exec("UPDATE global_memory SET active=?,rev=rev+1,updated_at=? WHERE id=? AND rev=?", active, now, id, rev); err != nil {
			return err
		}
		// The author stays who wrote the note (the event below names who approved it): an agent keeps the right to change, or take back, what it wrote.
		item.Active, item.Rev, item.UpdatedAt = active, rev+1, now
		if scope == "agent_shared_notes" {
			if err := tx.closeNoteApprovals(id, "resolved", actorID); err != nil {
				return err
			}
		}
		rows, err := tx.tx.QueryContext(ctx, "SELECT id FROM project WHERE system=0")
		if err != nil {
			return err
		}
		var projects []string
		for rows.Next() {
			var project string
			if err := rows.Scan(&project); err != nil {
				rows.Close()
				return err
			}
			projects = append(projects, project)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, project := range projects {
			if _, err := tx.AppendEvent(Event{Kind: "global_knowledge_updated", ProjectID: project,
				PayloadJSON: payloadJSON(map[string]string{"id": id, "scope": scope, "rev": strconv.FormatInt(item.Rev, 10), "active": strconv.FormatBool(active), "actor_kind": "user", "actor_id": actorID})}); err != nil {
				return err
			}
		}
		return nil
	})
	return item, err
}

// isProjectMember says whether a person is an active member of a project: the people a project's agents may read the profiles of and edit them for.
func (tx *Tx) isProjectMember(projectID, userID string) (bool, error) {
	var member bool
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM membership mb JOIN user u ON u.id=mb.user_id WHERE mb.project_id=? AND mb.user_id=? AND u.disabled_at IS NULL)`, projectID, userID).Scan(&member)
	return member, err
}

// IsProjectMember is isProjectMember outside a transaction.
func (s *Store) IsProjectMember(ctx context.Context, projectID, userID string) (bool, error) {
	var member bool
	err := s.readDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM membership mb JOIN user u ON u.id=mb.user_id WHERE mb.project_id=? AND mb.user_id=? AND u.disabled_at IS NULL)`, projectID, userID).Scan(&member)
	return member, err
}

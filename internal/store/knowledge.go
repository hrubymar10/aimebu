package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	knowledgeScopeBytes = 4096
	knowledgeBodyRunes  = 2000
)

var ErrMemoryDisabled = errors.New("memory_disabled")
var ErrKnowledgeLimit = errors.New("knowledge_limit")
var ErrMemorySource = errors.New("invalid_memory_source")

// KnowledgeRecord is a curated project fact or a note kept with a task room.
// Every edit changes its author and revision; the event log retains edit history.
type KnowledgeRecord struct {
	ID              string `json:"id"`
	Scope           string `json:"scope"`
	ScopeID         string `json:"scope_id"`
	ScopeLabel      string `json:"scope_label,omitempty"`
	Body            string `json:"body"`
	Rev             int64  `json:"rev"`
	AuthorKind      string `json:"author_kind"`
	AuthorID        string `json:"author_id"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	SourceMessageID string `json:"source_message_id,omitempty"`
	Active          bool   `json:"active"`
}

func knowledgeTable(scope string) (table, column string, err error) {
	switch scope {
	case "project":
		return "project_memory", "project_id", nil
	case "task":
		return "task_note", "room_id", nil
	default:
		return "", "", ErrInvalidScope
	}
}

func (s *Store) ProjectMemoryEnabled(ctx context.Context, projectID string) (bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return false, err
	}
	var enabled bool
	err := s.db.QueryRowContext(ctx, `SELECT memory_enabled FROM settings_project WHERE project_id=?`, projectID).Scan(&enabled)
	return enabled, err
}

func (s *Store) UserProjectRole(ctx context.Context, userID, projectID string) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT role FROM membership WHERE user_id=? AND project_id=?`, userID, projectID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrForbidden
	}
	return role, err
}

func (s *Store) SetProjectMemoryEnabled(ctx context.Context, projectID, actorID string, enabled bool) (bool, error) {
	err := s.Tx(ctx, func(tx *Tx) error {
		if err := tx.knowledgeActorAllowed("project", projectID, "user", actorID, true, true); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE settings_project SET memory_enabled=? WHERE project_id=?`, enabled, projectID); err != nil {
			return err
		}
		_, err := tx.AppendEvent(Event{Kind: "project_memory_setting_changed", ProjectID: projectID,
			PayloadJSON: payloadJSON(map[string]string{"enabled": strconv.FormatBool(enabled), "actor_id": actorID})})
		return err
	})
	return enabled, err
}

// SetRoomMemoryEnabled only permits inheritance or a restrictive override.
func (s *Store) SetRoomMemoryEnabled(ctx context.Context, roomID, actorID string, enabled *bool) (*bool, error) {
	if enabled != nil && *enabled {
		return nil, ErrInvalidTransition
	}
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID string
		if err := tx.tx.QueryRowContext(ctx, `SELECT project_id FROM room WHERE id=?`, roomID).Scan(&projectID); err != nil {
			return err
		}
		var role string
		if err := tx.tx.QueryRowContext(ctx, `SELECT role FROM membership WHERE project_id=? AND user_id=?`, projectID, actorID).Scan(&role); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrForbidden
			}
			return err
		}
		if role == "viewer" {
			return ErrForbidden
		}
		if _, err := tx.Exec(`UPDATE room SET memory_enabled=? WHERE id=?`, enabled, roomID); err != nil {
			return err
		}
		_, err := tx.AppendEvent(Event{Kind: "room_memory_setting_changed", ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"actor_id": actorID, "inherit": strconv.FormatBool(enabled == nil)})})
		return err
	})
	return enabled, err
}

func (tx *Tx) validateMemorySource(projectID, actorID, sourceID string) error {
	if sourceID == "" {
		return nil
	}
	var allowed int
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM message m JOIN room r ON r.id=m.room_id
		JOIN room_member rm ON rm.room_id=r.id AND rm.member_kind='agent' AND rm.member_id=? AND rm.left_at IS NULL
		WHERE m.id=? AND r.project_id=? AND r.memory_enabled IS NOT 0`, actorID, sourceID, projectID).Scan(&allowed)
	if err != nil {
		return err
	}
	if allowed == 0 {
		return ErrMemorySource
	}
	return nil
}

func (s *Store) ListKnowledge(ctx context.Context, scope, scopeID string) ([]KnowledgeRecord, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	table, column, err := knowledgeTable(scope)
	if err != nil {
		return nil, err
	}
	sourceColumns := "NULL,1"
	if scope == "project" {
		sourceColumns = "source_message_id,1"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,body,rev,author_kind,author_id,created_at,updated_at,`+sourceColumns+` FROM `+table+` WHERE `+column+`=? ORDER BY created_at,id`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []KnowledgeRecord{}
	for rows.Next() {
		item := KnowledgeRecord{Scope: scope, ScopeID: scopeID}
		var source sql.NullString
		if err := rows.Scan(&item.ID, &item.Body, &item.Rev, &item.AuthorKind, &item.AuthorID, &item.CreatedAt, &item.UpdatedAt, &source, &item.Active); err != nil {
			return nil, err
		}
		item.SourceMessageID = source.String
		records = append(records, item)
	}
	return records, rows.Err()
}

func (s *Store) KnowledgeAgentAccess(ctx context.Context, scope, scopeID, agentID string) error {
	if err := s.checkTxReentry(ctx); err != nil {
		return err
	}
	var projectID string
	if err := s.db.QueryRowContext(ctx, `SELECT project_id FROM agent WHERE id=? AND revoked_at IS NULL`, agentID).Scan(&projectID); err != nil {
		return err
	}
	if scope == "project" {
		if scopeID != projectID {
			return ErrForbidden
		}
		enabled, err := s.ProjectMemoryEnabled(ctx, projectID)
		if err != nil {
			return err
		}
		if !enabled {
			return ErrMemoryDisabled
		}
		return nil
	}
	if scope != "task" {
		return ErrInvalidScope
	}
	var allowed int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM room r JOIN room_member rm ON rm.room_id=r.id AND rm.member_kind='agent' AND rm.member_id=? AND rm.left_at IS NULL WHERE r.id=? AND r.project_id=? AND r.kind='task'`, agentID, scopeID, projectID).Scan(&allowed)
	if err != nil {
		return err
	}
	if allowed == 0 {
		return ErrForbidden
	}
	return nil
}

func (tx *Tx) knowledgeSnapshot(projectID, roomID string) (map[string]any, error) {
	var enabled bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT memory_enabled FROM settings_project WHERE project_id=?`, projectID).Scan(&enabled); err != nil {
		return nil, err
	}
	read := func(scope, scopeID string) ([]KnowledgeRecord, error) {
		table, column, err := knowledgeTable(scope)
		if err != nil {
			return nil, err
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id,body,rev,author_kind,author_id,created_at,updated_at FROM `+table+` WHERE `+column+`=? ORDER BY created_at,id`, scopeID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		items := []KnowledgeRecord{}
		for rows.Next() {
			item := KnowledgeRecord{Scope: scope, ScopeID: scopeID, Active: true}
			if err := rows.Scan(&item.ID, &item.Body, &item.Rev, &item.AuthorKind, &item.AuthorID, &item.CreatedAt, &item.UpdatedAt); err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		return items, rows.Err()
	}
	projectItems := []KnowledgeRecord{}
	profileItems := []KnowledgeRecord{}
	sharedItems := []KnowledgeRecord{}
	if enabled {
		var err error
		projectItems, err = read("project", projectID)
		if err != nil {
			return nil, err
		}
		for _, scope := range []string{"user_profile", "agent_shared_notes"} {
			rows, err := tx.tx.QueryContext(tx.ctx, "SELECT g.id,g.scope_key,g.body,g.rev,g.author_kind,g.author_id,g.created_at,g.updated_at,COALESCE(u.login,'') FROM global_memory g LEFT JOIN user u ON u.id=g.scope_key AND g.scope='user_profile' WHERE g.scope=? AND g.active=1 ORDER BY g.scope_key,g.created_at,g.id", scope)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				item := KnowledgeRecord{Scope: scope, Active: true}
				if err := rows.Scan(&item.ID, &item.ScopeID, &item.Body, &item.Rev, &item.AuthorKind, &item.AuthorID, &item.CreatedAt, &item.UpdatedAt, &item.ScopeLabel); err != nil {
					rows.Close()
					return nil, err
				}
				if scope == "user_profile" {
					profileItems = append(profileItems, item)
				} else {
					sharedItems = append(sharedItems, item)
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
		}
	}
	taskItems := []KnowledgeRecord{}
	var task bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT kind='task' AND memory_enabled IS NOT 0 FROM room WHERE id=? AND project_id IN (?,`+agentDMProjectSQL+`)`, roomID, projectID).Scan(&task); err != nil {
		return nil, err
	}
	if task {
		var err error
		taskItems, err = read("task", roomID)
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{"project_memory_enabled": enabled, "project_memory": projectItems, "user_profiles": profileItems, "agent_shared_notes": sharedItems, "task_notes": taskItems}, nil
}

// ManageKnowledge creates, revises, or removes one record. A stale revision
// fails atomically, including if another edit changes the scope byte budget.
func (s *Store) ManageKnowledge(ctx context.Context, scope, scopeID, actorKind, actorID, id string, rev int64, body string, remove bool) (KnowledgeRecord, error) {
	return s.ManageKnowledgeWithSource(ctx, scope, scopeID, actorKind, actorID, id, rev, body, remove, "")
}

func (s *Store) ManageKnowledgeWithSource(ctx context.Context, scope, scopeID, actorKind, actorID, id string, rev int64, body string, remove bool, sourceID string) (KnowledgeRecord, error) {
	var item KnowledgeRecord
	err := s.Tx(ctx, func(tx *Tx) error {
		table, column, err := knowledgeTable(scope)
		if err != nil {
			return err
		}
		if err := tx.knowledgeActorAllowed(scope, scopeID, actorKind, actorID, true, false); err != nil {
			return err
		}
		if sourceID != "" {
			if scope != "project" || actorKind != "agent" {
				return ErrMemorySource
			}
			if err := tx.validateMemorySource(scopeID, actorID, sourceID); err != nil {
				return err
			}
		}
		if id == "" && remove || id == "" && rev != 0 || id != "" && rev < 1 {
			return ErrInvalidTransition
		}
		body = strings.TrimSpace(body)
		if !remove && (body == "" || utf8.RuneCountInString(body) > knowledgeBodyRunes) {
			return ErrKnowledgeLimit
		}
		oldBytes := 0
		if id != "" {
			item = KnowledgeRecord{ID: id, Scope: scope, ScopeID: scopeID}
			sourceColumn := "NULL,1"
			if scope == "project" {
				sourceColumn = "source_message_id,1"
			}
			var source sql.NullString
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT body,rev,author_kind,author_id,created_at,updated_at,`+sourceColumn+` FROM `+table+` WHERE id=? AND `+column+`=?`, id, scopeID).
				Scan(&item.Body, &item.Rev, &item.AuthorKind, &item.AuthorID, &item.CreatedAt, &item.UpdatedAt, &source, &item.Active)
			if err != nil {
				return err
			}
			item.SourceMessageID = source.String
			if item.Rev != rev {
				return ErrStaleRev
			}
			oldBytes = len([]byte(item.Body))
		}
		if !remove {
			var used int
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE(SUM(length(CAST(body AS BLOB))),0) FROM `+table+` WHERE `+column+`=?`, scopeID).Scan(&used); err != nil {
				return err
			}
			if used-oldBytes+len([]byte(body)) > knowledgeScopeBytes {
				return ErrKnowledgeLimit
			}
		}
		now := timestamp()
		action := "updated"
		if id == "" {
			id, err = newID("knw")
			if err != nil {
				return err
			}
			if scope == "project" {
				_, err = tx.Exec(`INSERT INTO project_memory(id,project_id,body,rev,author_kind,author_id,created_at,updated_at,source_message_id) VALUES (?,?,?,1,?,?,?,?,?)`, id, scopeID, body, actorKind, actorID, now, now, nullable(sourceID))
			} else {
				_, err = tx.Exec(`INSERT INTO task_note(id,room_id,body,rev,author_kind,author_id,created_at,updated_at) VALUES (?,?,?,1,?,?,?,?)`, id, scopeID, body, actorKind, actorID, now, now)
			}
			item = KnowledgeRecord{ID: id, Scope: scope, ScopeID: scopeID, Body: body, Rev: 1, AuthorKind: actorKind, AuthorID: actorID, CreatedAt: now, UpdatedAt: now, SourceMessageID: sourceID, Active: true}
			action = "created"
		} else if remove {
			_, err = tx.Exec(`DELETE FROM `+table+` WHERE id=? AND `+column+`=? AND rev=?`, id, scopeID, rev)
			item.Rev++
			item.AuthorKind, item.AuthorID, item.UpdatedAt = actorKind, actorID, now
			action = "deleted"
		} else {
			_, err = tx.Exec(`UPDATE `+table+` SET body=?,rev=rev+1,author_kind=?,author_id=?,updated_at=? WHERE id=? AND `+column+`=? AND rev=?`,
				body, actorKind, actorID, now, id, scopeID, rev)
			item.Body, item.Rev = body, rev+1
			item.AuthorKind, item.AuthorID, item.UpdatedAt = actorKind, actorID, now
		}
		if err != nil {
			return err
		}
		projectID, roomID := scopeID, ""
		if scope == "task" {
			roomID = scopeID
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM room WHERE id=?`, roomID).Scan(&projectID); err != nil {
				return err
			}
		}
		_, err = tx.AppendEvent(Event{Kind: "knowledge_" + action, ProjectID: projectID, RoomID: roomID,
			PayloadJSON: payloadJSON(map[string]string{"id": id, "scope": scope, "rev": strconv.FormatInt(item.Rev, 10), "actor_kind": actorKind, "actor_id": actorID})})
		return err
	})
	return item, err
}

func (tx *Tx) knowledgeActorAllowed(scope, scopeID, actorKind, actorID string, write, setting bool) error {
	var projectID string
	if scope == "project" {
		projectID = scopeID
	} else if scope == "task" {
		var kind string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,kind FROM room WHERE id=?`, scopeID).Scan(&projectID, &kind); err != nil {
			return err
		}
		if kind != "task" {
			return ErrInvalidScope
		}
	} else {
		return ErrInvalidScope
	}
	if actorKind == "user" {
		var role string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT role FROM membership WHERE project_id=? AND user_id=?`, projectID, actorID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrForbidden
		}
		if err != nil {
			return err
		}
		if write && role == "viewer" {
			return ErrForbidden
		}
	} else if actorKind == "agent" {
		if setting {
			return ErrForbidden
		}
		var member int
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM agent a WHERE a.id=? AND a.project_id=? AND a.revoked_at IS NULL
			AND (?='project' OR EXISTS (SELECT 1 FROM room_member m WHERE m.room_id=? AND m.member_kind='agent' AND m.member_id=a.id AND m.left_at IS NULL))`,
			actorID, projectID, scope, scopeID).Scan(&member)
		if err != nil {
			return err
		}
		if member == 0 {
			return ErrForbidden
		}
	} else {
		return ErrForbidden
	}
	if scope == "project" && write && !setting {
		var enabled bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT memory_enabled FROM settings_project WHERE project_id=?`, projectID).Scan(&enabled); err != nil {
			return err
		}
		if !enabled {
			return ErrMemoryDisabled
		}
	}
	return nil
}

type KnowledgeHit struct {
	Source    string `json:"source"`
	ID        string `json:"id"`
	RoomID    string `json:"room_id,omitempty"`
	Body      string `json:"body"`
	Score     int    `json:"score"`
	CreatedAt string `json:"created_at"`
}

// RecallKnowledge searches curated records and room messages visible to this
// agent. Room membership is rechecked on every call, including for DMs.
func (s *Store) RecallKnowledge(ctx context.Context, agentID, query string, limit int) ([]KnowledgeHit, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, ErrInvalidTransition
	}
	if limit < 1 || limit > 10 {
		limit = 10
	}
	var projectID string
	if err := s.db.QueryRowContext(ctx, `SELECT project_id FROM agent WHERE id=? AND revoked_at IS NULL`, agentID).Scan(&projectID); err != nil {
		return nil, err
	}
	terms := knowledgeTerms(query)
	hits := []KnowledgeHit{}
	enabled, err := s.ProjectMemoryEnabled(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if enabled {
		items, err := s.ListKnowledge(ctx, "project", projectID)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if !item.Active {
				continue
			}
			if score := knowledgeScore(item.Body, query, terms); score > 0 {
				hits = append(hits, KnowledgeHit{"project_memory", item.ID, "", item.Body, score, item.CreatedAt})
			}
		}
		for _, scope := range []string{"user_profile", "agent_shared_notes"} {
			items, err := s.ListActiveGlobalKnowledge(ctx, scope)
			if err != nil {
				return nil, err
			}
			for _, item := range items {
				if score := knowledgeScore(item.Body, query, terms); score > 0 {
					hits = append(hits, KnowledgeHit{scope, item.ID, "", item.Body, score, item.CreatedAt})
				}
			}
		}
	}
	if !enabled {
		return nil, ErrMemoryDisabled
	}
	rows, err := s.db.QueryContext(ctx, `SELECT n.id,n.room_id,n.body,n.created_at FROM task_note n
		JOIN room_member rm ON rm.room_id=n.room_id AND rm.member_kind='agent' AND rm.member_id=? AND rm.left_at IS NULL
		WHERE EXISTS (SELECT 1 FROM room r WHERE r.id=n.room_id AND r.project_id=? AND r.memory_enabled IS NOT 0)`, agentID, projectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, roomID, body, created string
		if err := rows.Scan(&id, &roomID, &body, &created); err != nil {
			rows.Close()
			return nil, err
		}
		if score := knowledgeScore(body, query, terms); score > 0 {
			hits = append(hits, KnowledgeHit{"task_note", id, roomID, body, score, created})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT m.id,m.room_id,m.body,m.created_at FROM message m
		JOIN room r ON r.id=m.room_id AND r.project_id=?
		JOIN room_member rm ON rm.room_id=m.room_id AND rm.member_kind='agent' AND rm.member_id=? AND rm.left_at IS NULL
		WHERE m.author_kind!='system' AND r.memory_enabled IS NOT 0`, projectID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, roomID, body, created string
		if err := rows.Scan(&id, &roomID, &body, &created); err != nil {
			return nil, err
		}
		if score := knowledgeScore(body, query, terms); score > 0 {
			hits = append(hits, KnowledgeHit{"message", id, roomID, knowledgeSnippet(body, terms, query), score, created})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].CreatedAt > hits[j].CreatedAt
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func knowledgeTerms(query string) []string {
	seen := map[string]bool{}
	terms := []string{}
	for _, term := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if term != "" && !seen[term] {
			seen[term] = true
			terms = append(terms, term)
		}
	}
	return terms
}

func knowledgeScore(body, query string, terms []string) int {
	lower := strings.ToLower(body)
	score := 0
	if strings.Contains(lower, strings.ToLower(query)) {
		score += 10
	}
	for _, term := range terms {
		if strings.Contains(lower, term) {
			score += 2
		}
	}
	return score
}

// knowledgeSnippet keeps the 220-rune window that contains the match: the
// full query if present, else the first matching term, else the head. The
// window starts 60 runes before the match so the hit has leading context.
func knowledgeSnippet(body string, terms []string, query string) string {
	const maxRunes = 220
	const lead = 60
	runes := []rune(strings.TrimSpace(body))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	// Match on the rune slice, not the string: lowercasing can change byte
	// lengths, so byte offsets from a lowered copy are not rune offsets.
	lower := make([]rune, len(runes))
	for i, r := range runes {
		lower[i] = unicode.ToLower(r)
	}
	at := runeSliceIndex(lower, []rune(strings.ToLower(query)))
	for _, term := range terms {
		if at >= 0 {
			break
		}
		at = runeSliceIndex(lower, []rune(term))
	}
	if at < 0 {
		at = 0
	}
	start := at - lead
	if start < 0 {
		start = 0
	}
	end := start + maxRunes
	if end > len(runes) {
		end = len(runes)
		start = end - maxRunes
		if start < 0 {
			start = 0
		}
	}
	snippet := string(runes[start:end])
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(runes) {
		snippet += "…"
	}
	return snippet
}

func runeSliceIndex(haystack, needle []rune) int {
	if len(needle) == 0 {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j, r := range needle {
			if haystack[i+j] != r {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// slowHarnessKnowledgeBodyCap bounds the record bodies in one delivery's
// knowledge snapshot for harnesses that stall on a large bootstrap prompt.
const slowHarnessKnowledgeBodyCap = 2048

const slowHarnessTruncationSuffix = "... [truncated for pi bootstrap]"

// shouldTrimKnowledgeForAgent reports whether a pi agent runs a gemma model,
// the combination whose bootstrap stalls when the memory snapshot is large.
func shouldTrimKnowledgeForAgent(tool, model string) bool {
	return tool == "pi" && strings.Contains(strings.ToLower(model), "gemma")
}

// trimKnowledgeSnapshotForSlowHarness keeps record bodies within a shared
// byte budget, in priority order (project memory, user profiles, shared
// notes, task notes). The record that crosses the budget is truncated on a
// rune boundary and every later record is dropped. Records are copied, so the
// caller's snapshot is not modified.
func trimKnowledgeSnapshotForSlowHarness(snapshot map[string]any) map[string]any {
	trimmed := make(map[string]any, len(snapshot))
	for key, value := range snapshot {
		trimmed[key] = value
	}
	remaining := slowHarnessKnowledgeBodyCap
	for _, key := range []string{"project_memory", "user_profiles", "agent_shared_notes", "task_notes"} {
		records, ok := snapshot[key].([]KnowledgeRecord)
		if !ok {
			continue
		}
		kept := make([]KnowledgeRecord, 0, len(records))
		for _, record := range records {
			if remaining <= 0 {
				break
			}
			body, truncated := truncateKnowledgeBytes(record.Body, remaining)
			record.Body = body
			kept = append(kept, record)
			remaining -= len(body)
			if truncated {
				remaining = 0
			}
		}
		trimmed[key] = kept
	}
	return trimmed
}

func truncateKnowledgeBytes(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		return "", s != ""
	}
	if len(s) <= maxBytes {
		return s, false
	}
	limit := maxBytes - len(slowHarnessTruncationSuffix)
	if limit <= 0 {
		return slowHarnessTruncationSuffix[:maxBytes], true
	}
	for limit > 0 && !utf8.ValidString(s[:limit]) {
		limit--
	}
	return strings.TrimSpace(s[:limit]) + slowHarnessTruncationSuffix, true
}

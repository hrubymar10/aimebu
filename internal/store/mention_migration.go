package store

import (
	"context"
	"database/sql"
	"strings"
)

// migrateMentionsToIDs converts the stored bodies of old messages to mention
// storage by agent ID: every live prose `@slug` (and `@slug@project`) that
// names an agent of the message's project is rewritten to `@agt_<id>`. Before
// this change slugs were unique per project, so the reading is unambiguous —
// the plain slug is resolved project-wide (the room's membership at send time
// is not recoverable), and `slug@project` against the project it names. A
// token that resolves to nothing (an unknown name, a person's login, a role
// key, a group tag) is left as it was written, and so is anything inside code.
//
// The pass is idempotent: a token that already is a stored agent ID is left
// alone, so a second run changes nothing. It runs inside the migration's
// transaction, so a crash rolls it back and the next start does the whole work
// again; the message scan walks seq in batches so a large history does not have
// to fit in memory at once.
func migrateMentionsToIDs(ctx context.Context, tx *sql.Tx) error {
	agentsByProject := map[string]map[string]string{} // project ID → lower slug → agent ID (first wins)
	projectNames := map[string]string{}               // lower project name → project ID
	projectIDs := map[string]bool{}
	agentIDs := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.slug,a.project_id,p.name FROM agent a
		JOIN project p ON p.id=a.project_id WHERE a.revoked_at IS NULL ORDER BY a.created_at,a.id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, slug, projectID, projectName string
		if err := rows.Scan(&id, &slug, &projectID, &projectName); err != nil {
			rows.Close()
			return err
		}
		if agentsByProject[projectID] == nil {
			agentsByProject[projectID] = map[string]string{}
		}
		name := strings.ToLower(slug)
		if _, taken := agentsByProject[projectID][name]; !taken {
			agentsByProject[projectID][name] = id
		}
		lower := strings.ToLower(projectName)
		if _, taken := projectNames[lower]; !taken {
			projectNames[lower] = projectID
		}
		projectIDs[projectID] = true
		agentIDs[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	rooms := map[string]string{} // room ID → project ID ("" when not looked up yet, "?" when the room is gone)
	const batch = 500
	var last int64 = -1
	for {
		rows, err := tx.QueryContext(ctx, `SELECT id,room_id,body,seq FROM message
			WHERE seq>? AND author_kind IN ('user','agent') AND body LIKE '%@%' ORDER BY seq LIMIT ?`, last, batch)
		if err != nil {
			return err
		}
		type candidate struct {
			id, roomID, body string
			spans            []MentionSpan
		}
		var candidates []candidate
		count := 0
		for rows.Next() {
			var c candidate
			var seq int64
			if err := rows.Scan(&c.id, &c.roomID, &c.body, &seq); err != nil {
				rows.Close()
				return err
			}
			last = seq
			count++
			c.spans = proseMentionSpans(c.body)
			if len(c.spans) > 0 {
				candidates = append(candidates, c)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if count == 0 {
			return nil
		}
		for _, c := range candidates {
			if _, seen := rooms[c.roomID]; seen {
				continue
			}
			var projectID string
			switch err := tx.QueryRowContext(ctx, `SELECT project_id FROM room WHERE id=?`, c.roomID).Scan(&projectID); {
			case err == sql.ErrNoRows:
				rooms[c.roomID] = "?" // a room the v1 import left behind: nothing resolves there
			case err != nil:
				return err
			default:
				rooms[c.roomID] = projectID
			}
		}
		for _, c := range candidates {
			projectID := rooms[c.roomID]
			if projectID == "?" {
				continue
			}
			texts := make([]string, len(c.spans))
			for i, span := range c.spans {
				id := resolveMentionTokenForMigration(span.Token, projectID, agentsByProject, projectNames, projectIDs, agentIDs)
				if id != "" {
					texts[i] = "@" + id
				}
			}
			rewritten, changed := replaceMentionSpans(c.body, c.spans, texts)
			if !changed {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE message SET body=? WHERE id=?`, rewritten, c.id); err != nil {
				return err
			}
		}
	}
}

// resolveMentionTokenForMigration returns the agent ID a stored token names, or
// "" when it names none. A token that already is an agent ID is left alone.
func resolveMentionTokenForMigration(token, projectID string, agentsByProject map[string]map[string]string,
	projectNames map[string]string, projectIDs, agentIDs map[string]bool) string {
	if strings.HasPrefix(token, "agt_") {
		if agentIDs[token] {
			return "" // already stored as an ID
		}
		return ""
	}
	if at := strings.Index(token, "@"); at >= 0 {
		name, qualifier := token[:at], token[at+1:]
		target := projectNames[qualifier]
		if projectIDs[qualifier] {
			target = qualifier
		}
		if target == "" {
			return ""
		}
		return agentsByProject[target][name]
	}
	return agentsByProject[projectID][token]
}

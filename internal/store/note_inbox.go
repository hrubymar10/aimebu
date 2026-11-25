package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/goccy/go-json"
)

// A shared agent note that waits for approval has an inbox item for the people who can approve it (the owner and the admins who are owners of the
// agent's project). It is an ordinary "approval" item with a system requester named "note:<note id>", so it needs no new kind (the kinds are fixed by
// a CHECK constraint) and the inbox, its count and its events carry it like any other. Approving the note or declining it is the item's two choices;
// changing the note elsewhere (the memory viewer, the agent taking it back) closes the item.
const noteApprovalPrefix = "note:"

// noteApprovalText is what the card says: who wrote what, short. The author is its stored @agt_<id> token (names are
// reusable); the human view translates it.
func noteApprovalText(agent, body string) string {
	body = strings.Join(strings.Fields(body), " ")
	if r := []rune(body); len(r) > 240 {
		body = string(r[:240]) + "…"
	}
	return agent + " wrote a note for the shared agent notes, which every project's agents would read once approved: “" + body + "”"
}

// syncNoteApproval makes the open inbox item for a shared note match the note: none when it is not waiting for approval, one with its current words
// when it is. It runs inside the write that changed the note.
func (tx *Tx) syncNoteApproval(noteID string) error {
	var body, authorKind, authorID string
	var active bool
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT body,author_kind,author_id,active FROM global_memory WHERE id=? AND scope='agent_shared_notes'`, noteID).Scan(&body, &authorKind, &authorID, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.closeNoteApprovals(noteID, "withdrawn", "")
	}
	if err != nil {
		return err
	}
	if err := tx.closeNoteApprovals(noteID, "withdrawn", ""); err != nil {
		return err
	}
	if active || authorKind != "agent" {
		return nil
	}
	var projectID, roomID string
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id FROM agent a WHERE a.id=?`, authorID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// The card sits in the project's home room, which every project has: the note itself belongs to no room.
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM room WHERE project_id=? AND kind='home' ORDER BY created_at,id LIMIT 1`, projectID).Scan(&roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	id, err := newID("inb")
	if err != nil {
		return err
	}
	options, err := json.Marshal(inboxAsk{Text: noteApprovalText("@"+authorID, body), Options: []string{"Approve", "Decline"}})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,options_json,created_at) VALUES (?,?,?,?,'system',?,'approval',?,?)`,
		id, projectID, roomID, authorID, noteApprovalPrefix+noteID, string(options), timestamp()); err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: projectID, RoomID: roomID, AgentID: authorID,
		PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "kind": "approval"})})
	return err
}

// closeNoteApprovals ends the open inbox items of a note: "resolved" when somebody decided it, "withdrawn" when the question went away.
func (tx *Tx) closeNoteApprovals(noteID, state, by string) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT id,project_id,room_id,COALESCE(agent_id,'') FROM inbox_item WHERE kind='approval' AND requester_kind='system' AND requester_id=? AND state='open'`, noteApprovalPrefix+noteID)
	if err != nil {
		return err
	}
	type item struct{ id, project, room, agent string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.project, &it.room, &it.agent); err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, it := range items {
		if _, err := tx.Exec(`UPDATE inbox_item SET state=?,rev=rev+1,resolved_by=?,resolved_at=? WHERE id=?`, state, nullable(by), timestamp(), it.id); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: it.project, RoomID: it.room, AgentID: it.agent,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": it.id})}); err != nil {
			return err
		}
	}
	return nil
}

// NoteApprovalID is the id of the shared note an inbox item asks about, or "" when the item is not a note approval.
func (s *Store) NoteApprovalID(ctx context.Context, itemID string) (string, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", err
	}
	var requester string
	err := s.db.QueryRowContext(ctx, `SELECT requester_id FROM inbox_item WHERE id=? AND kind='approval' AND requester_kind='system'`, itemID).Scan(&requester)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	id, ok := strings.CutPrefix(requester, noteApprovalPrefix)
	if !ok {
		return "", nil
	}
	return id, nil
}

// DecideNoteApproval answers a note's inbox item: "approve" switches the note on for every project's agents, "deny" deletes it. Both need an admin
// (the same rule as the memory viewer's controls); the item is closed by the change itself.
func (s *Store) DecideNoteApproval(ctx context.Context, itemID, userID, choice string, rev int64) error {
	if choice != "approve" && choice != "deny" {
		return ErrChoiceNotOffered
	}
	noteID, err := s.NoteApprovalID(ctx, itemID)
	if err != nil {
		return err
	}
	if noteID == "" {
		return ErrInvalidTransition
	}
	var state string
	var itemRev int64
	if err := s.db.QueryRowContext(ctx, `SELECT state,rev FROM inbox_item WHERE id=?`, itemID).Scan(&state, &itemRev); err != nil {
		return err
	}
	switch state {
	case "resolved":
		return ErrAlreadyResolved
	case "withdrawn":
		return ErrAlreadyWithdrawn
	}
	if itemRev != rev {
		return ErrStaleInboxRev
	}
	var noteRev int64
	if err := s.db.QueryRowContext(ctx, `SELECT rev FROM global_memory WHERE id=? AND scope='agent_shared_notes'`, noteID).Scan(&noteRev); errors.Is(err, sql.ErrNoRows) {
		return ErrAlreadyWithdrawn
	} else if err != nil {
		return err
	}
	if choice == "approve" {
		_, err = s.SetGlobalKnowledgeActive(ctx, "agent_shared_notes", "global", userID, noteID, noteRev, true)
		return err
	}
	_, err = s.ManageGlobalKnowledge(ctx, "agent_shared_notes", "global", "", "user", userID, noteID, noteRev, "", true)
	return err
}

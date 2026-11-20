package store

// supersedeAsks closes ordinary agent asks when a person, or the agent that
// asked, writes a later message in the same room. It runs in InsertMessage's
// transaction so inbox state and message history cannot disagree.
//
// A question that carries no structured ask (a message flagged needs_attention
// or offering proposed answers) is closed more narrowly, because it is a
// message in a conversation rather than a form: only a reply in its thread
// answers it. That is a person's message anywhere in the thread, or the asking
// agent's own newer message replying to it. A message elsewhere in the room says
// nothing about it.
func (tx *Tx) supersedeAsks(message Message) error {
	if message.AuthorKind != "user" && message.AuthorKind != "agent" {
		return nil
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT i.id,i.project_id,i.agent_id,CASE WHEN ask.ask_json IS NULL THEN 'answered in thread' ELSE 'newer message' END
		FROM inbox_item i
		JOIN message ask ON ask.id=i.message_id
		WHERE i.room_id=? AND i.state='open' AND i.kind IN ('question','approval')
		AND i.requester_kind='agent' AND ask.seq<?
		AND (ask.ask_json IS NOT NULL AND (?='user' OR i.requester_id=?)
			OR ask.ask_json IS NULL AND (?<>'' AND (?=ask.id OR ?=COALESCE(NULLIF(ask.thread_root,''),ask.id))
				AND (?='user' OR i.requester_id=? AND ?=ask.id)))
		ORDER BY i.id`,
		message.RoomID, message.Seq, message.AuthorKind, message.AuthorID,
		message.ReplyTo, message.ReplyTo, message.ThreadRoot, message.AuthorKind, message.AuthorID, message.ReplyTo)
	if err != nil {
		return err
	}
	type item struct{ id, projectID, agentID, reason string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.projectID, &it.agentID, &it.reason); err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, it := range items {
		if _, err := tx.Exec(`UPDATE inbox_item SET state='superseded',rev=rev+1,resolved_by=?,resolved_at=? WHERE id=? AND state='open'`,
			message.AuthorID, message.CreatedAt, it.id); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: it.projectID, RoomID: message.RoomID, AgentID: it.agentID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": it.id, "message_id": message.ID, "resolution": "superseded", "reason": it.reason})}); err != nil {
			return err
		}
	}
	return nil
}

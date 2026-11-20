package store

// supersedeAsks closes ordinary agent asks when a person, or the agent that
// asked, writes a later message in the same room. It runs in InsertMessage's
// transaction so inbox state and message history cannot disagree.
//
// A question that carries no structured ask (a message flagged needs_attention
// or offering proposed answers) is a message in a conversation rather than a
// form, so a person answers it by talking. A person's reply in the card's
// thread settles it. A plain message in the room (the usual answer is typed in
// the chat, not on the card) settles it only when it is the room's only open
// card of this kind (a message that replies to something else, such as another card's answer, is not a plain one): with two or more open, a plain message cannot tell which it
// answers and settles none. The asking agent closes it only narrowly, with its
// own newer message replying to it.
// inThreadSQL is whether the message being written (reply_to, reply_to, thread_root as parameters) belongs to the card message's thread.
const inThreadSQL = `(?<>'' AND (?=ask.id OR ?=COALESCE(NULLIF(ask.thread_root,''),ask.id)))`

// onlyOpenAttentionSQL is whether the card is the only open attention card of its room (the alias i is the card).
const onlyOpenAttentionSQL = `(SELECT COUNT(*) FROM inbox_item o JOIN message om ON om.id=o.message_id
	WHERE o.room_id=i.room_id AND o.state='open' AND o.kind IN ('question','approval') AND o.requester_kind='agent' AND om.ask_json IS NULL)=1`

func (tx *Tx) supersedeAsks(message Message) error {
	if message.AuthorKind != "user" && message.AuthorKind != "agent" {
		return nil
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT i.id,i.project_id,i.agent_id,CASE WHEN ask.ask_json IS NULL THEN CASE WHEN ?<>'' AND (?=ask.id OR ?=COALESCE(NULLIF(ask.thread_root,''),ask.id)) THEN 'answered in thread' ELSE 'answered in room' END ELSE 'newer message' END
		FROM inbox_item i
		JOIN message ask ON ask.id=i.message_id
		WHERE i.room_id=? AND i.state='open' AND i.kind IN ('question','approval')
		AND i.requester_kind='agent' AND ask.seq<?
		AND (ask.ask_json IS NOT NULL AND (?='user' OR i.requester_id=?)
			OR ask.ask_json IS NULL AND (?='user' AND (`+inThreadSQL+` OR ?='' AND `+onlyOpenAttentionSQL+`) OR ?<>'' AND i.requester_id=? AND ?=ask.id))
		ORDER BY i.id`,
		message.ReplyTo, message.ReplyTo, message.ThreadRoot,
		message.RoomID, message.Seq, message.AuthorKind, message.AuthorID,
		message.AuthorKind, message.ReplyTo, message.ReplyTo, message.ThreadRoot, message.ReplyTo,
		message.ReplyTo, message.AuthorID, message.ReplyTo)
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

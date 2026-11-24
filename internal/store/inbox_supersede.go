package store

import "database/sql"

// inThreadSQL is whether the message being written (the named reply_to and thread_root) belongs to the card message's thread.
const inThreadSQL = `(:reply_to<>'' AND (:reply_to=ask.id OR :thread_root=COALESCE(NULLIF(ask.thread_root,''),ask.id)))`

// onlyOpenAttentionSQL is whether the card is the only open attention card of its room (the alias i is the card).
const onlyOpenAttentionSQL = `(SELECT COUNT(*) FROM inbox_item o JOIN message om ON om.id=o.message_id
	WHERE o.room_id=i.room_id AND o.state='open' AND o.kind IN ('question','approval') AND o.requester_kind='agent' AND om.ask_json IS NULL)=1`

// structuredAnswersSQL is whether the card message (alias ask) offers structured answers: proposed answers or open questions.
const structuredAnswersSQL = `(COALESCE(TRIM(ask.proposed_answers_json),'') NOT IN ('','null','[]','{}')
	OR COALESCE(TRIM(ask.open_questions_json),'') NOT IN ('','null','[]','{}'))`

// supersedeAsks closes ordinary agent asks when a person, or the agent that
// asked, writes a later message in the same room. It runs in InsertMessage's
// transaction so inbox state and message history cannot disagree.
//
// A question that carries no structured ask (a message flagged needs_attention
// or offering proposed answers) is a message in a conversation rather than a
// form, so a person answers it by talking. A person's reply in the card's
// thread settles it. A plain message in the room (the usual answer is typed in
// the chat, not on the card) settles it only when it is the room's only open
// card of this kind and it offers no structured answers (proposed answers or
// open questions wait for their answer); a message that replies to something
// else, such as another card's answer, is not a plain one, and with two or more
// cards open a plain message cannot tell which it answers and settles none. This
// room rule is decided here, when the message is written, and never by the sweep
// (which cannot tell a card that was unanswered when the message came from one
// answered since). The asking agent closes such a card with its own newer
// message replying to it, or with any newer card of its own (an ask or an
// attention message): its latest card replaces the older ones, structured
// answers or not.
func (tx *Tx) supersedeAsks(message Message) error {
	if message.AuthorKind != "user" && message.AuthorKind != "agent" {
		return nil
	}
	newCard := 0
	if message.AuthorKind == "agent" && asksAPerson(message) {
		newCard = 1
	}
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT i.id,i.project_id,i.agent_id,
		CASE WHEN ask.ask_json IS NOT NULL THEN 'newer message'
			WHEN :author_kind='agent' AND :new_card=1 THEN 'newer card'
			WHEN `+inThreadSQL+` THEN 'answered in thread'
			ELSE 'answered in room' END
		FROM inbox_item i
		JOIN message ask ON ask.id=i.message_id
		WHERE i.room_id=:room_id AND i.state='open' AND i.kind IN ('question','approval')
		AND i.requester_kind='agent' AND ask.seq<:seq
		AND (ask.ask_json IS NOT NULL AND (:author_kind='user' OR i.requester_id=:author_id)
			OR ask.ask_json IS NULL AND (
				:author_kind='user' AND (`+inThreadSQL+` OR :reply_to='' AND NOT `+structuredAnswersSQL+` AND `+onlyOpenAttentionSQL+`)
				OR :author_kind='agent' AND i.requester_id=:author_id AND (:new_card=1 OR :reply_to<>'' AND :reply_to=ask.id)))
		ORDER BY i.id`,
		sql.Named("reply_to", message.ReplyTo), sql.Named("thread_root", message.ThreadRoot),
		sql.Named("room_id", message.RoomID), sql.Named("seq", message.Seq),
		sql.Named("author_kind", message.AuthorKind), sql.Named("author_id", message.AuthorID),
		sql.Named("new_card", newCard))
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

// reanswerableSQL is whether a withdrawn card (alias i, its message ask) can still be answered: an agent's question or approval that offers structured
// answers (proposed answers or open questions, no explicit ask), asked by an agent that still exists, in a room that is open. A card with structured
// choices keeps them until a person picks one (the chat shows them for as long as the answer can be sent), however the card left the Inbox: dismissed, or
// withdrawn by a restart. A closed room or a removed agent cannot receive an answer, so those stay final.
const reanswerableSQL = `i.state='withdrawn' AND i.kind IN ('question','approval') AND i.requester_kind='agent' AND ask.ask_json IS NULL
	AND ` + structuredAnswersSQL + `
	AND NOT EXISTS(SELECT 1 FROM boundary_pending b WHERE b.inbox_id=i.id)
	AND NOT EXISTS(SELECT 1 FROM agent ag WHERE ag.id=i.agent_id AND ag.revoked_at IS NOT NULL)
	AND EXISTS(SELECT 1 FROM room rm WHERE rm.id=i.room_id AND rm.lifecycle='open')`

// reanswerableListSQL is reanswerableSQL for the inbox listing, whose message alias is msg.
const reanswerableListSQL = `(` + `EXISTS(SELECT 1 FROM message ask WHERE ask.id=i.message_id AND ` + reanswerableSQL + `))`

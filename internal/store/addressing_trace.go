package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

type TracePerson struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// MentionTrace explains how one @token in a message was read. Outcome is
// addressed, self, ambiguous, no_match, or ignored.
type MentionTrace struct {
	Token   string        `json:"token"`
	Kind    string        `json:"kind"`
	Outcome string        `json:"outcome"`
	Reason  string        `json:"reason"`
	Reached []TracePerson `json:"reached"`
	// WouldReach is set for an ignored token: who it would have addressed had
	// it been live prose. A token with nobody to reach is not reported.
	WouldReach   []TracePerson `json:"would_reach,omitempty"`
	SelfExcluded bool          `json:"self_excluded,omitempty"`
}

// AddressingTrace re-evaluates a message's addressing against the room's
// current members and compares it with what was recorded when it was sent.
// Replies, private chats, handoffs, and inbox answers add targets beyond
// mentions; their wake reasons are in the per-agent notes.
type AddressingTrace struct {
	Mentions []MentionTrace `json:"mentions"`
	Sources  []string       `json:"sources"`
	Recorded []string       `json:"recorded_targets"`
	Now      []string       `json:"resolved_now"`
	// Drift is true when re-reading the body against the room as it is now
	// would address someone the recorded targets do not include, because
	// membership grew since the message was sent. It does not fire when a
	// recorded target has since left, and never for an inbox answer, whose
	// targets are fixed by the answer rather than by mentions.
	Drift bool `json:"drift"`
}

// messageAddressingTrace builds the trace for one stored message. It only
// reads; the room membership it uses is the current one.
func (s *Store) messageAddressingTrace(ctx context.Context, messageID, roomID, authorID, authorKind, body, replyTo string, recorded []string) (AddressingTrace, error) {
	trace := AddressingTrace{Mentions: []MentionTrace{}, Sources: []string{}, Recorded: recorded, Now: []string{}}
	if trace.Recorded == nil {
		trace.Recorded = []string{}
	}
	members, err := roomRecipientsFrom(s.db.QueryContext, ctx, roomID)
	if err != nil {
		return trace, err
	}
	var projectID, projectName, roomKind string
	if err := s.db.QueryRowContext(ctx, `SELECT p.id,p.name,r.kind FROM room r JOIN project p ON p.id=r.project_id WHERE r.id=?`, roomID).Scan(&projectID, &projectName, &roomKind); err != nil {
		return trace, err
	}
	names := map[string]string{}
	for _, member := range members {
		names[member.id] = member.slug
	}
	targets := resolveMentions(body, authorID, members, projectID, projectName, &trace.Mentions)
	for id := range targets {
		trace.Now = append(trace.Now, id)
	}
	if replyTo != "" {
		var author, parentKind string
		err := s.db.QueryRowContext(ctx, `SELECT author_id,author_kind FROM message WHERE id=? AND room_id=?`, replyTo, roomID).Scan(&author, &parentKind)
		if err != nil && err != sql.ErrNoRows {
			return trace, err
		}
		if err == nil {
			trace.Sources = append(trace.Sources, replySource(members, names, authorID, authorKind, author, parentKind))
		}
	}
	if roomKind == "dm" {
		trace.Sources = append(trace.Sources, "private chat: a message from a person or an agent reaches the people in the chat")
	}
	var answeredItem string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM inbox_item WHERE answer_message_id=? LIMIT 1`, messageID).Scan(&answeredItem)
	if err != nil && err != sql.ErrNoRows {
		return trace, err
	}
	inboxAnswer := err == nil
	if inboxAnswer {
		trace.Sources = append(trace.Sources, "inbox answer: this message was sent as the answer to an inbox item, so it reaches only the agent that asked; the @mentions above do not address anyone")
	}
	sort.Strings(trace.Now)
	// Drift compares mention-addressed targets only: a recorded target that
	// came from a reply, DM, handoff, or inbox answer is not a mention miss.
	recordedSet := map[string]bool{}
	for _, id := range trace.Recorded {
		recordedSet[id] = true
	}
	for _, id := range trace.Now {
		if !recordedSet[id] && !inboxAnswer {
			trace.Drift = true
		}
	}
	return trace, nil
}

// replySource says what a reply to a parent message did, following the rule
// the server applies: the parent's author is addressed only while a member of
// the room (system messages and departed members never count), never the
// replier themself, and an agent replying to another agent wakes it behind the
// hop guard (after ReplyWakeHops such wakes, replies and broadcasts together, in
// a row with no person's message between them, a reply keeps the thread but
// wakes nobody).
func replySource(members []roomRecipient, names map[string]string, authorID, authorKind, parentAuthor, parentKind string) string {
	var parent *roomRecipient
	for i := range members {
		if members[i].id == parentAuthor {
			parent = &members[i]
		}
	}
	who := parentAuthor
	if name := names[parentAuthor]; name != "" {
		who = name
	}
	switch {
	case parentKind == "system":
		return "reply to a system message: system messages have no author to address, so nobody is addressed"
	case parent == nil:
		return fmt.Sprintf("reply to a message by %s: that author is not in this room now, so nobody is addressed", who)
	case parentAuthor == authorID:
		return fmt.Sprintf("reply to a message by %s: a reply to yourself addresses nobody", who)
	case authorKind == "agent" && parent.kind != "user":
		return fmt.Sprintf("reply to a message by %s: an agent's reply to another agent wakes it, but only %d times in a row with no person's message between them, replies and broadcasts counted together (then it keeps the thread and wakes nobody; use a live @mention)", who, ReplyWakeHops)
	default:
		return fmt.Sprintf("reply to a message by %s: a reply addresses its parent's author", who)
	}
}

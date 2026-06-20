package server

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

func (s *store) roomSend(roomID, from, body string, needsAttention bool, proposedAnswers []string, openQuestions []types.OpenQuestion, attachments []types.Attachment, replyTo int64) (int64, error) {
	return s.roomSendWithVisualPlan(roomID, from, body, needsAttention, proposedAnswers, openQuestions, nil, nil, attachments, replyTo)
}

func (s *store) roomSendWithVisualPlan(roomID, from, body string, needsAttention bool, proposedAnswers []string, openQuestions []types.OpenQuestion, visualPlan []types.PlanBlock, appendixPages []types.AppendixPage, attachments []types.Attachment, replyTo int64) (int64, error) {
	if !s.isMember(roomID, from) {
		return 0, fmt.Errorf("agent %s is not a member of room %s", from, roomID)
	}
	if replyTo < 0 {
		return 0, fmt.Errorf("reply_to must be a positive message ID")
	}
	var parentMsg types.Message
	if replyTo != 0 {
		s.mu.RLock()
		var ok bool
		parentMsg, ok = s.messageByIDInRoomLocked(roomID, replyTo)
		s.mu.RUnlock()
		if !ok {
			return 0, fmt.Errorf("reply_to message #%d not found in room %s", replyTo, roomID)
		}
	}
	resolvedAttachments, err := s.resolveAttachments(attachments)
	if err != nil {
		return 0, err
	}
	normalizedVisualPlan, err := normalizeVisualPlanBlocks(visualPlan)
	if err != nil {
		return 0, err
	}
	normalizedAppendixPages, err := normalizeAppendixPages(appendixPages)
	if err != nil {
		return 0, err
	}
	if body == "" && len(resolvedAttachments) == 0 && len(normalizedVisualPlan) == 0 && len(normalizedAppendixPages) == 0 {
		return 0, fmt.Errorf("body, attachments, visual_plan, or appendix_pages are required")
	}

	ctx := s.addressingContext(types.Message{RoomID: roomID, From: from, Body: body})
	rawTargets := parseAddressedTo(body)
	targets := resolveAddressedTokens(rawTargets, ctx)
	if len(rawTargets) > 0 && targets == nil {
		targets = []string{}
	}
	if replyTo != 0 && !strings.EqualFold(parentMsg.From, from) && parentMsg.FromKind != "system" && parentMsg.From != "_system" {
		targets = appendTargetDedup(targets, parentMsg.From)
	}

	// Force-subscribe all registered humans who are not yet room members when
	// the sender signals needs_attention=true. Auto-join is idempotent (joinRoom
	// returns early for existing members). DMs are not exempt — matin confirmed
	// DMs are just narrower-scope rooms and multi-member is fine.
	if needsAttention {
		for _, a := range s.listAgents() {
			if a.Kind == "human" && !s.isMember(roomID, a.ID) {
				_, _ = s.joinRoom(roomID, a.ID)
			}
		}
	}

	// Look up sender kind so readers can apply different response policies
	// (humans → default respond, AIs → default silent) without guessing from
	// the ID shape.
	s.mu.RLock()
	var fromKind string
	if a, ok := s.agents[from]; ok {
		fromKind = a.Kind
	}
	s.mu.RUnlock()

	id := s.nextID.Add(1)
	msg := types.Message{
		ID:                  id,
		RoomID:              roomID,
		From:                from,
		FromKind:            fromKind,
		Body:                body,
		CreatedAt:           now(),
		ReplyTo:             replyTo,
		Targets:             targets,
		NeedsHumanAttention: needsAttention,
	}
	msg.ProposedAnswers = cleanProposedAnswers(proposedAnswers)
	msg.OpenQuestions = cleanOpenQuestions(openQuestions)
	msg.VisualPlan = normalizedVisualPlan
	msg.AppendixPages = normalizedAppendixPages
	msg.Attachments = resolvedAttachments

	s.mu.Lock()
	s.messages[roomID] = append(s.messages[roomID], msg)
	if s.db != nil {
		if err := s.insertMessageSQLiteLocked(msg); err != nil {
			log.Printf("aimebu: persist message sqlite: %v", err)
		}
	} else {
		s.persist()
	}
	s.mu.Unlock()

	// Touch agent last_seen
	s.touchAgent(from)

	// Notify per-room subscribers
	s.subMu.Lock()
	for _, ch := range s.roomSubs[roomID] {
		select {
		case ch <- msg:
		default:
		}
	}
	// Notify global subscribers
	for _, ch := range s.globalSubs {
		select {
		case ch <- msg:
		default:
		}
	}
	// Broadcast attention event to ALL meta subscribers so that humans whose WS
	// is not subscribed to this room still receive the alert signal.
	if needsAttention {
		attEvt := MetaEvent{Type: "attention_event", Data: map[string]any{"room_id": roomID, "message": msg}}
		for _, ch := range s.metaSubs {
			select {
			case ch <- attEvt:
			default:
			}
		}
	}
	s.subMu.Unlock()

	return id, nil
}

func (s *store) roomMessages(roomID string, limit int, sinceID int64) []types.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()

	roomMsgs := s.messages[roomID]

	// Filter by sinceID
	var filtered []types.Message
	for _, m := range roomMsgs {
		if m.ID > sinceID {
			filtered = append(filtered, m)
		}
	}

	// Return newest first, capped by limit
	n := len(filtered)
	if limit > 0 && limit < n {
		filtered = filtered[n-limit:]
	}
	// Reverse to newest-first
	for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
		filtered[i], filtered[j] = filtered[j], filtered[i]
	}
	return filtered
}

// messagesSince returns messages in a single room with ID > sinceID,
// in chronological order (oldest first).
func (s *store) messagesSince(roomID string, sinceID int64) []types.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []types.Message
	for _, m := range s.messages[roomID] {
		if m.ID > sinceID {
			out = append(out, m)
		}
	}
	return out
}

// agentUnreadSinceCursor returns all unread messages across every room the
// agent is a member of, using per-room cursors (initialized to HEAD-5 on
// first use). Returns the messages plus a snapshot of the cursors used.
// Messages are ordered by ID ascending (chronological).
func (s *store) agentUnreadSinceCursor(agentID string) ([]types.Message, map[string]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[agentID]
	if !ok {
		return nil, nil
	}
	if a.ReadCursors == nil {
		a.ReadCursors = make(map[string]int64)
	}
	persisted := false
	cursors := make(map[string]int64)
	var out []types.Message
	for _, r := range s.rooms {
		isMember := false
		for _, m := range r.Members {
			if m == agentID {
				isMember = true
				break
			}
		}
		if !isMember {
			continue
		}
		cur, seen := a.ReadCursors[r.ID]
		if !seen {
			head := s.roomHeadLocked(r.ID)
			start := head - 5
			if start < 0 {
				start = 0
			}
			a.ReadCursors[r.ID] = start
			cur = start
			persisted = true
		}
		cursors[r.ID] = cur
		for _, m := range s.messages[r.ID] {
			if m.ID > cur {
				out = append(out, m)
			}
		}
	}
	if persisted {
		s.persist()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, cursors
}

// agentMessagesSince returns messages across all rooms the agent is a member
// of, with ID > sinceID, in chronological order (oldest first).
func (s *store) agentMessagesSince(agentID string, sinceID int64) []types.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []types.Message
	for _, room := range s.rooms {
		isMember := false
		for _, m := range room.Members {
			if m == agentID {
				isMember = true
				break
			}
		}
		if !isMember {
			continue
		}
		for _, m := range s.messages[room.ID] {
			if m.ID > sinceID {
				out = append(out, m)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// messageByID returns the message with the given global ID and true, or
// the zero value and false if no such message exists.
func (s *store) messageByID(id int64) (types.Message, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, msgs := range s.messages {
		for _, m := range msgs {
			if m.ID == id {
				return m, true
			}
		}
	}
	return types.Message{}, false
}

func (s *store) allMessages(limit int) []types.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Flatten all room messages into one slice
	var all []types.Message
	for _, msgs := range s.messages {
		all = append(all, msgs...)
	}

	// Sort by ID ascending (chronological)
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	n := len(all)
	if limit > n {
		limit = n
	}
	// Return newest first
	result := make([]types.Message, limit)
	for i := 0; i < limit; i++ {
		result[i] = all[n-1-i]
	}
	return result
}

func (s *store) reactionPath() string {
	return filepath.Join(s.dir, "reactions.json")
}

func (s *store) loadReactions() {
	if s.db != nil {
		if _, err := s.loadReactionsSQLite(); err != nil {
			log.Printf("aimebu: loadReactions sqlite: %v", err)
		}
		return
	}
	data, err := os.ReadFile(s.reactionPath())
	if err != nil {
		return
	}
	var env struct {
		Reactions map[int64][]types.Reaction `json:"reactions"`
	}
	if json.Unmarshal(data, &env) != nil || env.Reactions == nil {
		return
	}
	s.reactionsMu.Lock()
	for id, rows := range env.Reactions {
		if id <= 0 {
			continue
		}
		for _, r := range rows {
			if r.AgentID == "" || r.Emoji == "" {
				continue
			}
			s.reactions[id] = append(s.reactions[id], r)
		}
	}
	s.reactionsMu.Unlock()
}

func (s *store) persistReactionsLocked() {
	if s.db != nil {
		if err := s.persistReactionsSQLiteLocked(); err != nil {
			log.Printf("aimebu: persist reactions sqlite: %v", err)
		}
		return
	}
	out := make(map[int64][]types.Reaction, len(s.reactions))
	for id, rows := range s.reactions {
		if len(rows) == 0 {
			continue
		}
		cp := append([]types.Reaction(nil), rows...)
		sort.Slice(cp, func(i, j int) bool {
			if cp[i].Emoji != cp[j].Emoji {
				return cp[i].Emoji < cp[j].Emoji
			}
			if cp[i].AgentID != cp[j].AgentID {
				return cp[i].AgentID < cp[j].AgentID
			}
			return cp[i].CreatedAt < cp[j].CreatedAt
		})
		out[id] = cp
	}
	env := struct {
		Reactions map[int64][]types.Reaction `json:"reactions"`
	}{Reactions: out}
	if data, err := json.MarshalIndent(env, "", "  "); err == nil {
		atomicWrite(s.reactionPath(), data)
	}
}

func (s *store) messageRoomLocked(id int64) (string, bool) {
	for roomID, msgs := range s.messages {
		for _, m := range msgs {
			if m.ID == id {
				return roomID, true
			}
		}
	}
	return "", false
}

func (s *store) messageByIDInRoomLocked(roomID string, id int64) (types.Message, bool) {
	for _, m := range s.messages[roomID] {
		if m.ID == id {
			return m, true
		}
	}
	return types.Message{}, false
}

func (s *store) messageExists(id int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.messageRoomLocked(id)
	return ok
}

func (s *store) liveMessageIDs() map[int64]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	live := make(map[int64]bool)
	for _, msgs := range s.messages {
		for _, m := range msgs {
			live[m.ID] = true
		}
	}
	return live
}

func (s *store) addReaction(messageID int64, agentID, emoji string) (string, []types.ReactionSummary, bool, error) {
	s.mu.RLock()
	roomID, ok := s.messageRoomLocked(messageID)
	if !ok {
		s.mu.RUnlock()
		return "", nil, false, fmt.Errorf("message not found")
	}
	if _, ok := s.agents[agentID]; !ok {
		s.mu.RUnlock()
		return "", nil, false, fmt.Errorf("agent %q is not registered", agentID)
	}
	room := s.rooms[roomID]
	member := false
	if room != nil {
		for _, m := range room.Members {
			if m == agentID {
				member = true
				break
			}
		}
	}
	s.mu.RUnlock()
	if !member {
		return "", nil, false, fmt.Errorf("agent %s is not a member of room %s", agentID, roomID)
	}

	s.reactionsMu.Lock()
	rows := s.reactions[messageID]
	for _, r := range rows {
		if r.AgentID == agentID && r.Emoji == emoji {
			summary := summarizeReactions(rows, agentID)
			s.reactionsMu.Unlock()
			return roomID, summary, false, nil
		}
	}
	rows = append(rows, types.Reaction{AgentID: agentID, Emoji: emoji, CreatedAt: now()})
	s.reactions[messageID] = rows
	summary := summarizeReactions(rows, agentID)
	if s.db != nil {
		if err := s.persistReactionSQLiteLocked(messageID); err != nil {
			log.Printf("aimebu: persist reaction sqlite: %v", err)
		}
	} else {
		s.persistReactionsLocked()
	}
	s.reactionsMu.Unlock()

	s.touchAgent(agentID)
	return roomID, summary, true, nil
}

func (s *store) removeReaction(messageID int64, agentID, emoji string) (string, []types.ReactionSummary, bool, error) {
	s.mu.RLock()
	roomID, ok := s.messageRoomLocked(messageID)
	if !ok {
		s.mu.RUnlock()
		return "", nil, false, fmt.Errorf("message not found")
	}
	if _, ok := s.agents[agentID]; !ok {
		s.mu.RUnlock()
		return "", nil, false, fmt.Errorf("agent %q is not registered", agentID)
	}
	room := s.rooms[roomID]
	member := false
	if room != nil {
		for _, m := range room.Members {
			if m == agentID {
				member = true
				break
			}
		}
	}
	s.mu.RUnlock()
	if !member {
		return "", nil, false, fmt.Errorf("agent %s is not a member of room %s", agentID, roomID)
	}

	s.reactionsMu.Lock()
	rows := s.reactions[messageID]
	filtered := rows[:0]
	removed := false
	for _, r := range rows {
		if r.AgentID == agentID && r.Emoji == emoji {
			removed = true
			continue
		}
		filtered = append(filtered, r)
	}
	if removed {
		if len(filtered) == 0 {
			delete(s.reactions, messageID)
		} else {
			s.reactions[messageID] = filtered
		}
		if s.db != nil {
			if err := s.persistReactionSQLiteLocked(messageID); err != nil {
				log.Printf("aimebu: persist reaction sqlite: %v", err)
			}
		} else {
			s.persistReactionsLocked()
		}
	}
	summary := summarizeReactions(filtered, agentID)
	s.reactionsMu.Unlock()

	s.touchAgent(agentID)
	return roomID, summary, removed, nil
}

func summarizeReactions(rows []types.Reaction, viewerID string) []types.ReactionSummary {
	if len(rows) == 0 {
		return nil
	}
	type acc struct {
		count  int
		agents map[string]bool
		me     bool
	}
	byEmoji := make(map[string]acc)
	for _, r := range rows {
		if r.Emoji == "" || r.AgentID == "" {
			continue
		}
		a := byEmoji[r.Emoji]
		if a.agents == nil {
			a.agents = make(map[string]bool)
		}
		a.count++
		a.agents[r.AgentID] = true
		if viewerID != "" && r.AgentID == viewerID {
			a.me = true
		}
		byEmoji[r.Emoji] = a
	}
	if len(byEmoji) == 0 {
		return nil
	}
	emojis := make([]string, 0, len(byEmoji))
	for emoji := range byEmoji {
		emojis = append(emojis, emoji)
	}
	sort.Strings(emojis)
	out := make([]types.ReactionSummary, 0, len(emojis))
	for _, emoji := range emojis {
		a := byEmoji[emoji]
		agents := make([]string, 0, len(a.agents))
		for agentID := range a.agents {
			agents = append(agents, agentID)
		}
		sort.Strings(agents)
		out = append(out, types.ReactionSummary{Emoji: emoji, Count: a.count, Agents: agents, Me: a.me})
	}
	return out
}

func (s *store) withReactionSummaries(msgs []types.Message, viewerID string) []types.Message {
	if len(msgs) == 0 {
		return msgs
	}
	s.reactionsMu.RLock()
	defer s.reactionsMu.RUnlock()
	out := make([]types.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		out[i].Reactions = summarizeReactions(s.reactions[m.ID], viewerID)
	}
	return out
}

func (s *store) cleanupReactionsForLiveMessages() {
	live := s.liveMessageIDs()
	s.reactionsMu.Lock()
	changed := false
	for id := range s.reactions {
		if !live[id] {
			delete(s.reactions, id)
			changed = true
		}
	}
	if changed {
		s.persistReactionsLocked()
	}
	s.reactionsMu.Unlock()
}

func (s *store) broadcastReactionUpdate(roomID string, messageID int64, agentID, emoji, op string) {
	s.reactionsMu.RLock()
	summary := summarizeReactions(s.reactions[messageID], "")
	s.reactionsMu.RUnlock()
	if summary == nil {
		summary = []types.ReactionSummary{}
	}
	evt := MetaEvent{
		Type: "reaction",
		Data: map[string]any{
			"room_id":    roomID,
			"message_id": messageID,
			"agent_id":   agentID,
			"emoji":      emoji,
			"op":         op,
			"reactions":  summary,
		},
	}
	s.subMu.Lock()
	for _, ch := range s.metaSubs {
		select {
		case ch <- evt:
		default:
		}
	}
	s.subMu.Unlock()
}


// roomHead returns the highest message ID currently in the room, or 0 if
// the room has no messages. Caller must hold s.mu (read or write).
func (s *store) roomHeadLocked(roomID string) int64 {
	msgs := s.messages[roomID]
	if len(msgs) == 0 {
		return 0
	}
	return msgs[len(msgs)-1].ID
}

// roomHead is the lock-acquiring variant of roomHeadLocked.
func (s *store) roomHead(roomID string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.roomHeadLocked(roomID)
}

// ensureRoomCursor returns the agent's cursor for the room, initializing it
// to max(head-5, 0) on first read so a fresh joiner gets at most the last 5
// historical messages. Persists if it initialized a new cursor.
func (s *store) ensureRoomCursor(agentID, roomID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[agentID]
	if !ok {
		return 0
	}
	if a.ReadCursors == nil {
		a.ReadCursors = make(map[string]int64)
	}
	if cur, seen := a.ReadCursors[roomID]; seen {
		return cur
	}
	head := s.roomHeadLocked(roomID)
	start := head - 5
	if start < 0 {
		start = 0
	}
	a.ReadCursors[roomID] = start
	s.persist()
	return start
}

// advanceCursor bumps the agent's cursor for the room to at least `to`.
// Returns true if the cursor advanced (caller may want to broadcast).
func (s *store) advanceCursor(agentID, roomID string, to int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[agentID]
	if !ok {
		return false
	}
	if a.ReadCursors == nil {
		a.ReadCursors = make(map[string]int64)
	}
	if a.ReadCursors[roomID] >= to {
		return false
	}
	a.ReadCursors[roomID] = to
	s.persist()
	return true
}

// cloneAgentLocked returns an agent snapshot whose mutable map fields no longer
// alias live store state, so callers can JSON-marshal it after releasing s.mu
// without risking concurrent map iteration panics. Caller must hold s.mu

func (s *store) subscribeRoom(roomID string) chan types.Message {
	ch := make(chan types.Message, 64)
	s.subMu.Lock()
	s.roomSubs[roomID] = append(s.roomSubs[roomID], ch)
	s.subMu.Unlock()
	return ch
}

func (s *store) unsubscribeRoom(roomID string, ch chan types.Message) {
	s.subMu.Lock()
	subs := s.roomSubs[roomID]
	for i, sub := range subs {
		if sub == ch {
			s.roomSubs[roomID] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	s.subMu.Unlock()
	close(ch)
}

func (s *store) subscribeGlobal() chan types.Message {
	ch := make(chan types.Message, 64)
	s.subMu.Lock()
	s.globalSubs = append(s.globalSubs, ch)
	s.subMu.Unlock()
	return ch
}

func (s *store) unsubscribeGlobal(ch chan types.Message) {
	s.subMu.Lock()
	for i, sub := range s.globalSubs {
		if sub == ch {
			s.globalSubs = append(s.globalSubs[:i], s.globalSubs[i+1:]...)
			break
		}
	}
	s.subMu.Unlock()
	close(ch)
}

// ── Meta-event subscriptions (for WebSocket push) ─────────────────

func (s *store) subscribeMeta() chan MetaEvent {
	ch := make(chan MetaEvent, 64)
	s.subMu.Lock()
	s.metaSubs = append(s.metaSubs, ch)
	s.subMu.Unlock()
	return ch
}

func (s *store) unsubscribeMeta(ch chan MetaEvent) {
	s.subMu.Lock()
	for i, sub := range s.metaSubs {
		if sub == ch {
			s.metaSubs = append(s.metaSubs[:i], s.metaSubs[i+1:]...)
			break
		}
	}
	s.subMu.Unlock()
	close(ch)
}

// broadcastRoomUpdate sends a snapshot of all rooms to meta subscribers.
// Must be called WITHOUT holding s.mu (it acquires read lock internally).
func (s *store) broadcastRoomUpdate() {
	rooms := s.listRooms()
	evt := MetaEvent{Type: "room_update", Data: map[string]any{"rooms": rooms}}
	s.subMu.Lock()
	for _, ch := range s.metaSubs {
		select {
		case ch <- evt:
		default:
		}
	}
	s.subMu.Unlock()
}

// broadcastAgentUpdate sends a snapshot of all agents to meta subscribers.
// Must be called WITHOUT holding s.mu (it acquires read lock internally).
func (s *store) broadcastAgentUpdate() {
	agents := s.listAgents()
	evt := MetaEvent{Type: "agent_update", Data: map[string]any{"agents": agents}}
	s.broadcastMeta(evt)
}

func (s *store) broadcastMeta(evt MetaEvent) {
	s.subMu.Lock()
	for _, ch := range s.metaSubs {
		select {
		case ch <- evt:
		default:
		}
	}
	s.subMu.Unlock()
}

// broadcastReadUpdate notifies meta subscribers that a single agent's read
// cursor changed for a single room. Subscribers use it to update unread
// badges without re-fetching. Called WITHOUT holding s.mu. Also emits a
// `presence` event so the FE can move read-receipt markers for the agent.
//
// Invariant: each emitted presence event's `waiting` reflects ONLY its own
// scope (this specific room). The FE derives the effective waiting state
// by ORing the room-scoped bucket with the agent-wide "" bucket. If we
// emitted the global OR here, a room-scope bucket could latch to true and
// never be cleared (no leaveWait ever runs for that scope).
func (s *store) broadcastReadUpdate(agentID, roomID string, cursor int64) {
	evt := MetaEvent{
		Type: "read_update",
		Data: map[string]any{
			"agent_id":    agentID,
			"room":        roomID,
			"read_cursor": cursor,
		},
	}
	s.subMu.Lock()
	for _, ch := range s.metaSubs {
		select {
		case ch <- evt:
		default:
		}
	}
	s.subMu.Unlock()

	s.waitMu.RLock()
	scopeWaiting := s.openWaits[agentID][roomID] > 0
	s.waitMu.RUnlock()
	s.broadcastPresence(agentID, roomID, cursor, scopeWaiting)
}

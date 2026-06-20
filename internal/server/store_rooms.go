package server

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/hrubymar10/aimebu/internal/types"
)

// ── Rooms ──────────────────────────────────────────────────────────

func (s *store) createRoom(id, createdBy string) (*types.Room, error) {
	if id == "" {
		id = randomID()
	}
	if strings.HasPrefix(id, "_") {
		return nil, fmt.Errorf("room ID %q is reserved", id)
	}

	s.mu.Lock()

	if _, exists := s.rooms[id]; exists {
		r := s.rooms[id]
		s.mu.Unlock()
		return r, nil
	}

	room := &types.Room{
		ID:        id,
		Members:   []string{},
		CreatedAt: now(),
		CreatedBy: createdBy,
	}
	s.rooms[id] = room
	s.persist()
	s.mu.Unlock()

	s.broadcastRoomUpdate()
	if !strings.HasPrefix(id, "_") && !isDM(id) {
		s.emitSystemMessage("_system", "room "+id+" created")
	}
	return room, nil
}

func (s *store) getRoom(id string) *types.Room {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r, ok := s.rooms[id]; ok {
		return copyRoom(r)
	}
	return nil
}

func copyRoom(r *types.Room) *types.Room {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Members = append([]string{}, r.Members...)
	if r.MemoryEnabled != nil {
		v := *r.MemoryEnabled
		cp.MemoryEnabled = &v
	}
	return &cp
}

func (s *store) listRooms() []types.Room {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rooms := make([]types.Room, 0, len(s.rooms))
	for _, r := range s.rooms {
		rooms = append(rooms, *r)
	}
	return rooms
}

func (s *store) deleteRoom(id string) bool {
	s.mu.Lock()
	if _, ok := s.rooms[id]; !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.rooms, id)
	delete(s.messages, id)
	if s.db != nil {
		if err := s.deleteRoomMessagesSQLiteLocked(id); err != nil {
			log.Printf("aimebu: delete room messages sqlite: %v", err)
		}
	}
	s.persist()
	s.mu.Unlock()
	s.cleanupReactionsForLiveMessages()

	s.broadcastRoomUpdate()
	if !strings.HasPrefix(id, "_") && !isDM(id) {
		s.emitSystemMessage("_system", "room "+id+" deleted")
	}
	return true
}

func (s *store) setRoomMemoryOverride(roomID string, enabled *bool) (*types.Room, error) {
	s.mu.Lock()
	room, ok := s.rooms[roomID]
	if !ok {
		s.mu.Unlock()
		return nil, ErrRoomNotFound
	}
	if enabled == nil {
		room.MemoryEnabled = nil
	} else {
		v := *enabled
		room.MemoryEnabled = &v
	}
	cp := copyRoom(room)
	s.persist()
	s.mu.Unlock()

	s.broadcastRoomUpdate()
	return cp, nil
}

// ── Room membership ────────────────────────────────────────────────

func (s *store) joinRoom(roomID, agentID string) (*types.Room, error) {
	if strings.HasPrefix(roomID, "_") {
		return nil, fmt.Errorf("room ID %q is reserved", roomID)
	}

	s.mu.Lock()

	if _, ok := s.agents[agentID]; !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("agent %q is not registered; call POST /agents first", agentID)
	}

	room, exists := s.rooms[roomID]
	newRoom := !exists
	if !exists {
		room = &types.Room{
			ID:        roomID,
			Members:   []string{},
			CreatedAt: now(),
			CreatedBy: agentID,
		}
		s.rooms[roomID] = room
	}

	for _, m := range room.Members {
		if m == agentID {
			cp := *room
			s.mu.Unlock()
			return &cp, nil
		}
	}

	room.Members = append(room.Members, agentID)
	s.persist()
	cp := *room
	s.mu.Unlock()

	s.broadcastRoomUpdate()
	if newRoom && !strings.HasPrefix(roomID, "_") && !isDM(roomID) {
		s.emitSystemMessage("_system", "room "+roomID+" created")
	}
	s.emitSystemMessage(roomID, agentID+" joined")
	return &cp, nil
}

func (s *store) leaveRoom(roomID, agentID string) error {
	return s.leaveRoomWithEvent(roomID, agentID, false)
}

func (s *store) leaveRoomWithEvent(roomID, agentID string, kicked bool) error {
	s.mu.Lock()

	room, exists := s.rooms[roomID]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("room not found: %s", roomID)
	}

	found := false
	for i, m := range room.Members {
		if m == agentID {
			room.Members = append(room.Members[:i], room.Members[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		s.mu.Unlock()
		return fmt.Errorf("agent %s is not in room %s", agentID, roomID)
	}

	// Remove from roles on leave
	if room.Roles != nil {
		delete(room.Roles, agentID)
	}

	s.persist()
	s.mu.Unlock()

	s.broadcastRoomUpdate()
	if kicked {
		s.emitSystemMessage(roomID, agentID+" was kicked")
	} else {
		s.emitSystemMessage(roomID, agentID+" left")
	}
	return nil
}

// ensureSystemRoom creates the _system room if it doesn't exist. Bypasses the
// _ prefix block. Called from newStore after pruneOnStartup.
func (s *store) ensureSystemRoom() {
	s.mu.Lock()
	if _, ok := s.rooms["_system"]; !ok {
		s.rooms["_system"] = &types.Room{
			ID:        "_system",
			Members:   []string{},
			CreatedAt: now(),
			CreatedBy: "_system",
		}
		s.persist()
	}
	s.mu.Unlock()
}

// joinRoomInternal joins an agent to a room without the reserved-name guard
// and without emitting a system event. Used for auto-joins (e.g. _system on
// bus_register). Preserves the same idempotency as joinRoom.
func (s *store) joinRoomInternal(roomID, agentID string) {
	s.mu.Lock()
	changed := s.joinRoomInternalLocked(roomID, agentID)
	if changed {
		s.persist()
	}
	s.mu.Unlock()

	if changed {
		s.broadcastRoomUpdate()
	}
}

// joinRoomInternalLocked joins an agent to a room while s.mu is already held.
// It does not persist or broadcast; callers do that after completing all
// related mutations.
func (s *store) joinRoomInternalLocked(roomID, agentID string) bool {
	if _, ok := s.agents[agentID]; !ok {
		return false
	}

	room, exists := s.rooms[roomID]
	if !exists {
		room = &types.Room{
			ID:        roomID,
			Members:   []string{},
			CreatedAt: now(),
			CreatedBy: agentID,
		}
		s.rooms[roomID] = room
	}

	for _, m := range room.Members {
		if m == agentID {
			return false // already a member
		}
	}

	room.Members = append(room.Members, agentID)
	return true
}

// agentRoomViews lists the rooms an agent is a member of with unread count,
// latest message ID, and current read cursor for each. Used by
// GET /agents/{id}/rooms.
func (s *store) agentRoomViews(agentID string) []types.AgentRoomView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var cursors map[string]int64
	if a, ok := s.agents[agentID]; ok {
		cursors = a.ReadCursors
	}
	var result []types.AgentRoomView
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
		cursor := int64(0)
		if cursors != nil {
			cursor = cursors[r.ID]
		}
		head := s.roomHeadLocked(r.ID)
		unread := 0
		attentionUnread := 0
		for _, m := range s.messages[r.ID] {
			if m.ID > cursor {
				unread++
				if m.NeedsHumanAttention && m.From != agentID {
					attentionUnread++
				}
			}
		}
		result = append(result, types.AgentRoomView{
			Room:                 *r,
			UnreadCount:          unread,
			AttentionUnreadCount: attentionUnread,
			LastID:               head,
			ReadCursor:           cursor,
		})
	}
	return result
}

func (s *store) isMember(roomID, agentID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	room, exists := s.rooms[roomID]
	if !exists {
		return false
	}
	for _, m := range room.Members {
		if m == agentID {
			return true
		}
	}
	return false
}

// ── Messages ───────────────────────────────────────────────────────

// emitSystemMessage appends a system event into roomID and broadcasts it.
// Does not require an agent membership check. Must NOT be called while s.mu is held.
func (s *store) emitSystemMessage(roomID, body string) {
	s.emitSystemMessageTo(roomID, body, nil)
}

func (s *store) emitSystemMessageTo(roomID, body string, targets []string) {
	s.emitSystemMessageFull(roomID, body, targets, false)
}

func (s *store) emitSystemMessageFull(roomID, body string, targets []string, needsAttention bool) {
	id := s.nextID.Add(1)
	msg := types.Message{
		ID:                  id,
		RoomID:              roomID,
		From:                "_system",
		FromKind:            "system",
		Body:                body,
		CreatedAt:           now(),
		Targets:             targets,
		NeedsHumanAttention: needsAttention,
	}

	s.mu.Lock()
	if _, ok := s.rooms[roomID]; !ok {
		s.mu.Unlock()
		return
	}
	s.messages[roomID] = append(s.messages[roomID], msg)
	if s.db != nil {
		if err := s.insertMessageSQLiteLocked(msg); err != nil {
			log.Printf("aimebu: persist system message sqlite: %v", err)
		}
	} else {
		s.persist()
	}
	s.mu.Unlock()

	s.subMu.Lock()
	for _, ch := range s.roomSubs[roomID] {
		select {
		case ch <- msg:
		default:
		}
	}
	for _, ch := range s.globalSubs {
		select {
		case ch <- msg:
		default:
		}
	}
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
}

// ── DM ─────────────────────────────────────────────────────────────

func (s *store) findOrCreateDM(agentA, agentB string) (*types.Room, error) {
	// Deterministic DM room ID
	ids := []string{agentA, agentB}
	sort.Strings(ids)
	dmID := "dm:" + ids[0] + ":" + ids[1]

	s.mu.Lock()

	if _, ok := s.agents[agentA]; !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("sender %q is not registered", agentA)
	}
	if _, ok := s.agents[agentB]; !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("recipient %q is not registered on the bus", agentB)
	}

	// Existing DM room — repair membership if either side was kicked.
	if room, ok := s.rooms[dmID]; ok {
		var joined []string
		if s.joinRoomInternalLocked(dmID, agentA) {
			joined = append(joined, agentA)
		}
		if s.joinRoomInternalLocked(dmID, agentB) {
			joined = append(joined, agentB)
		}
		if len(joined) > 0 {
			s.persist()
		}
		cp := copyRoom(room)
		s.mu.Unlock()
		if len(joined) > 0 {
			s.broadcastRoomUpdate()
			for _, agentID := range joined {
				s.emitSystemMessage(dmID, agentID+" joined")
			}
		}
		return cp, nil
	}

	room := &types.Room{
		ID:        dmID,
		Members:   ids,
		CreatedAt: now(),
		CreatedBy: agentA,
	}
	s.rooms[dmID] = room
	s.persist()
	cp := copyRoom(room)
	s.mu.Unlock()

	// Broadcast room_update so WS clients see the new DM room without a
	// manual refresh. Must be called WITHOUT holding s.mu.
	s.broadcastRoomUpdate()
	return cp, nil
}

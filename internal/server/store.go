package server

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

//go:embed defaults_macros.json
var defaultMacrosJSON []byte

// macrosEnvelope is the canonical on-disk and API shape for macros.json.
// SeenDefaults is internal state (persisted but not exposed via the HTTP API).
// Older files used a per-user map[string]map[string]string (v1) or
// {macros, seen_defaults} without rooms (v2); load() detects and migrates both.
type macrosEnvelope struct {
	Macros       map[string]string            `json:"macros"`
	Rooms        map[string]map[string]string `json:"rooms,omitempty"`
	SeenDefaults []string                     `json:"seen_defaults,omitempty"`
}

// reclaimNamePattern restricts `force`d names to the same shape as the
// server-assigned pool — lowercase letters only, 3-12 chars. Keeps the ID
// space clean without requiring pool membership (pool can shift across
// versions; old names should stay reclaimable).
var reclaimNamePattern = regexp.MustCompile(`^[a-z]{3,12}$`)

// MetaEvent is a push notification for room/agent state changes.
type MetaEvent struct {
	Type string `json:"type"` // "room_update" or "agent_update"
	Data any    `json:"data"`
}

type store struct {
	dir    string
	nextID atomic.Int64

	mu       sync.RWMutex
	rooms    map[string]*types.Room
	messages map[string][]types.Message // keyed by room ID
	agents   map[string]*types.Agent

	// Per-room SSE subscribers
	subMu    sync.Mutex
	roomSubs map[string][]chan types.Message
	// Global SSE subscribers (firehose)
	globalSubs []chan types.Message
	// Meta-event subscribers (room/agent changes for WebSocket)
	metaSubs []chan MetaEvent

	// openWaits tracks currently-open bus_wait long-polls per (agentID, scope)
	// where scope is either a specific roomID or "" for agent-wide waits.
	// Keyed per-scope so interleaved exits don't clobber each other's
	// waiting=false broadcasts. Runtime-only (resets to 0 on restart, which
	// is correct). Guarded by waitMu, not s.mu, because wait entry/exit
	// happens outside the main state lock.
	waitMu    sync.RWMutex
	openWaits map[string]map[string]int

	// Tracks when a room first became empty (for cleanup)
	roomEmptySince map[string]time.Time

	macrosMu     sync.RWMutex
	macros       map[string]string            // global shared macro map
	macroRooms   map[string]map[string]string // per-room macro maps
	seenDefaults map[string]bool              // keys already offered via defaults (write-once)
}

func newStore(dir string) (*store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}

	s := &store{
		dir:            dir,
		rooms:          make(map[string]*types.Room),
		messages:        make(map[string][]types.Message),
		agents:         make(map[string]*types.Agent),
		roomSubs:       make(map[string][]chan types.Message),
		openWaits:      make(map[string]map[string]int),
		roomEmptySince: make(map[string]time.Time),
		macros:         make(map[string]string),
		macroRooms:     make(map[string]map[string]string),
		seenDefaults:   make(map[string]bool),
	}

	if err := s.load(); err != nil {
		log.Printf("Warning: could not load existing state: %v (starting fresh)", err)
	}

	// Apply cleanup rules once at startup so restarts don't accumulate
	// agents/rooms/messages that would have been pruned by the background
	// goroutine. Uses the same windows (30 min agent, 60 min empty room).
	s.pruneOnStartup()

	return s, nil
}

// pruneOnStartup removes agents whose last_seen is past the stale window,
// and rooms that are empty (after the agent prune). Messages attached to
// deleted rooms are also removed. Called once from newStore.
func (s *store) pruneOnStartup() {
	agentCutoff := time.Now().UTC().Add(-30 * time.Minute)

	s.mu.Lock()
	defer s.mu.Unlock()

	removedAgents := make(map[string]bool)
	for id, a := range s.agents {
		lastSeen, err := time.Parse(time.RFC3339, a.LastSeen)
		if err != nil || lastSeen.Before(agentCutoff) {
			removedAgents[id] = true
			delete(s.agents, id)
		}
	}

	// Drop removed agents from room memberships.
	for _, room := range s.rooms {
		filtered := room.Members[:0]
		for _, m := range room.Members {
			if !removedAgents[m] {
				filtered = append(filtered, m)
			}
		}
		room.Members = filtered
	}

	// Drop rooms that are now empty. (We don't have the 15-min grace here
	// since roomEmptySince isn't persisted — on restart, any empty room is
	// considered stale enough to prune. This is slightly more aggressive
	// than the runtime rule, but acceptable on cold start.)
	for id, room := range s.rooms {
		if len(room.Members) == 0 {
			delete(s.rooms, id)
			delete(s.messages, id)
		}
	}

	s.persist()

	if len(removedAgents) > 0 {
		log.Printf("Startup prune: removed %d stale agent(s)", len(removedAgents))
	}
}

// storeSchemaVersion is bumped whenever the on-disk format changes in a way
// that requires a clean slate. On mismatch the server wipes the three data
// files and starts fresh; no migration is attempted.
const storeSchemaVersion = 2

func (s *store) schemaOutdated() bool {
	data, err := os.ReadFile(filepath.Join(s.dir, "schema.json"))
	if err != nil {
		return true // no schema file → assume old data
	}
	var sv struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(data, &sv) != nil {
		return true
	}
	return sv.Version != storeSchemaVersion
}

func (s *store) writeSchema() {
	data, _ := json.Marshal(map[string]int{"version": storeSchemaVersion})
	_ = os.WriteFile(filepath.Join(s.dir, "schema.json"), data, 0o644)
}

func (s *store) wipeDataFiles() {
	for _, name := range []string{"rooms.json", "messages.json", "agents.json"} {
		_ = os.Remove(filepath.Join(s.dir, name))
	}
}

func (s *store) load() error {
	if s.schemaOutdated() {
		log.Printf("Schema version mismatch — wiping data dir for clean start")
		s.wipeDataFiles()
		s.writeSchema()
		return nil
	}
	// Rooms
	data, err := os.ReadFile(filepath.Join(s.dir, "rooms.json"))
	if err == nil {
		var rooms []types.Room
		if err := json.Unmarshal(data, &rooms); err != nil {
			return fmt.Errorf("parse rooms: %w", err)
		}
		for i := range rooms {
			s.rooms[rooms[i].ID] = &rooms[i]
		}
	}

	// Messages — stored as flat list on disk, loaded into per-room map
	data, err = os.ReadFile(filepath.Join(s.dir, "messages.json"))
	if err == nil {
		var flat []types.Message
		if err := json.Unmarshal(data, &flat); err != nil {
			return fmt.Errorf("parse messages: %w", err)
		}
		var maxID int64
		for _, m := range flat {
			s.messages[m.RoomID] = append(s.messages[m.RoomID], m)
			if m.ID > maxID {
				maxID = m.ID
			}
		}
		s.nextID.Store(maxID)
	}

	// Agents
	data, err = os.ReadFile(filepath.Join(s.dir, "agents.json"))
	if err == nil {
		var agents []types.Agent
		if err := json.Unmarshal(data, &agents); err != nil {
			return fmt.Errorf("parse agents: %w", err)
		}
		for i := range agents {
			s.agents[agents[i].ID] = &agents[i]
		}
	}

	// Macros — separate from schema-versioned state; survives schema wipes.
	// Handles three historical shapes:
	//   v1: per-user  {agentID: {key: body}}
	//   v2: global    {macros: {key: body}, seen_defaults: [...]}
	//   v3: global+rooms {macros: {...}, rooms: {roomID: {...}}, seen_defaults: [...]}
	data, err = os.ReadFile(filepath.Join(s.dir, "macros.json"))
	if err == nil {
		var env macrosEnvelope
		if json.Unmarshal(data, &env) == nil && env.Macros != nil {
			// v2 or v3
			s.macros = env.Macros
			if env.Rooms != nil {
				s.macroRooms = env.Rooms
			}
			for _, k := range env.SeenDefaults {
				s.seenDefaults[k] = true
			}
		} else {
			// v1 per-user shape: flatten with first-sorted-wins for conflicts.
			var old map[string]map[string]string
			if json.Unmarshal(data, &old) == nil && len(old) > 0 {
				agentIDs := make([]string, 0, len(old))
				for k := range old {
					agentIDs = append(agentIDs, k)
				}
				sort.Strings(agentIDs)
				for _, aid := range agentIDs {
					for k, v := range old[aid] {
						if _, exists := s.macros[k]; !exists {
							s.macros[k] = v
						} else {
							log.Printf("macros: migration conflict on key %q (agent %s) — keeping first", k, aid)
						}
					}
				}
			}
		}
	}

	return nil
}

func (s *store) persist() {
	// Rooms
	rooms := make([]types.Room, 0, len(s.rooms))
	for _, r := range s.rooms {
		rooms = append(rooms, *r)
	}
	if data, err := json.MarshalIndent(rooms, "", "  "); err == nil {
		atomicWrite(filepath.Join(s.dir, "rooms.json"), data)
	}

	// Messages — flatten map back to a single list for disk
	var allMsgs []types.Message
	for _, msgs := range s.messages {
		allMsgs = append(allMsgs, msgs...)
	}
	if data, err := json.MarshalIndent(allMsgs, "", "  "); err == nil {
		atomicWrite(filepath.Join(s.dir, "messages.json"), data)
	}

	// Agents
	agents := make([]types.Agent, 0, len(s.agents))
	for _, a := range s.agents {
		agents = append(agents, *a)
	}
	if data, err := json.MarshalIndent(agents, "", "  "); err == nil {
		atomicWrite(filepath.Join(s.dir, "agents.json"), data)
	}
}

// atomicWrite writes data to a temp file, flushes to disk, and renames it
// to the target path, preventing data corruption if the process crashes.
func atomicWrite(path string, data []byte) {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		log.Printf("Warning: failed to create %s: %v", tmp, err)
		return
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		log.Printf("Warning: failed to write %s: %v", tmp, err)
		return
	}
	if err := f.Sync(); err != nil {
		f.Close()
		log.Printf("Warning: failed to sync %s: %v", tmp, err)
		return
	}
	f.Close()
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("Warning: failed to rename %s -> %s: %v", tmp, path, err)
	}
}

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
	return room, nil
}

func (s *store) getRoom(id string) *types.Room {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r, ok := s.rooms[id]; ok {
		cp := *r
		cp.Members = append([]string{}, r.Members...)
		return &cp
	}
	return nil
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
	s.persist()
	s.mu.Unlock()

	s.broadcastRoomUpdate()
	return true
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
	s.emitSystemMessage(roomID, agentID+" joined")
	return &cp, nil
}

func (s *store) leaveRoom(roomID, agentID string) error {
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

	s.persist()
	s.mu.Unlock()

	s.broadcastRoomUpdate()
	s.emitSystemMessage(roomID, agentID+" left")
	return nil
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
		for _, m := range s.messages[r.ID] {
			if m.ID > cursor {
				unread++
			}
		}
		result = append(result, types.AgentRoomView{
			Room:        *r,
			UnreadCount: unread,
			LastID:      head,
			ReadCursor:  cursor,
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
	id := s.nextID.Add(1)
	msg := types.Message{
		ID:        id,
		RoomID:    roomID,
		From:      "_system",
		FromKind:  "system",
		Body:      body,
		CreatedAt: now(),
	}

	s.mu.Lock()
	if _, ok := s.rooms[roomID]; !ok {
		s.mu.Unlock()
		return
	}
	s.messages[roomID] = append(s.messages[roomID], msg)
	s.persist()
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
	s.subMu.Unlock()
}

func (s *store) roomSend(roomID, from, body string) (int64, error) {
	if !s.isMember(roomID, from) {
		return 0, fmt.Errorf("agent %s is not a member of room %s", from, roomID)
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
		ID:        id,
		RoomID:    roomID,
		From:      from,
		FromKind:  fromKind,
		Body:      body,
		CreatedAt: now(),
	}

	s.mu.Lock()
	s.messages[roomID] = append(s.messages[roomID], msg)
	// Senders have by definition seen their own message.
	if a, ok := s.agents[from]; ok {
		if a.ReadCursors == nil {
			a.ReadCursors = make(map[string]int64)
		}
		if a.ReadCursors[roomID] < id {
			a.ReadCursors[roomID] = id
		}
	}
	s.persist()
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

	// Existing DM room — return without broadcasting (no state change).
	if room, ok := s.rooms[dmID]; ok {
		s.mu.Unlock()
		return room, nil
	}

	room := &types.Room{
		ID:        dmID,
		Members:   ids,
		CreatedAt: now(),
		CreatedBy: agentA,
	}
	s.rooms[dmID] = room
	s.persist()
	s.mu.Unlock()

	// Broadcast room_update so WS clients see the new DM room without a
	// manual refresh. Must be called WITHOUT holding s.mu.
	s.broadcastRoomUpdate()
	return room, nil
}

// ── Agents ─────────────────────────────────────────────────────────

// registerHuman registers a human agent with the given explicit name. The
// human's ID is simply the name (no suffix, no project). Idempotent: if the
// name is already registered as a human, just touches last_seen. Errors if
// the name is in use by an AI agent.
func (s *store) registerHuman(name, project string, meta map[string]string) (*types.Agent, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required for human registration")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.agents[name]; ok {
		if existing.Kind != "human" {
			return nil, fmt.Errorf("name %q is in use by an AI agent", name)
		}
		existing.LastSeen = now()
		if project != "" {
			existing.Project = project
		}
		if len(meta) > 0 {
			if existing.Meta == nil {
				existing.Meta = make(map[string]string)
			}
			for k, v := range meta {
				existing.Meta[k] = v
			}
		}
		s.persist()
		cp := *existing
		go s.broadcastAgentUpdate()
		return &cp, nil
	}

	// Also reject if any AI agent is currently using this name (their full
	// ID differs but their bare name matches — would cause confusion).
	for _, a := range s.agents {
		if a.Name == name {
			return nil, fmt.Errorf("name %q is in use by agent %q", name, a.ID)
		}
	}

	agent := &types.Agent{
		ID:           name,
		Name:         name,
		Kind:         "human",
		Project:      project,
		Meta:         meta,
		RegisteredAt: now(),
		LastSeen:     now(),
	}
	s.agents[name] = agent
	s.persist()
	cp := *agent
	go s.broadcastAgentUpdate()
	return &cp, nil
}

// registerAI registers an AI agent. Normally the server assigns a random
// name from the pool and assembles the full ID from name/model/harness/
// project. If forceName is non-empty, the caller is asking to reclaim that
// specific name (e.g. after a prune or disconnect). Reclaim rules:
//   - name must match ^[a-z]{3,12}$ (same shape as pool names)
//   - if held by a human → error
//   - if held by an AI with same model+harness+project → idempotent: return
//     the existing agent with last_seen touched
//   - if held by an AI with different model/harness/project → error
//   - otherwise the forced name is used instead of picking from the pool
//
// Returns the created (or reclaimed) agent (caller-safe copy).
func (s *store) registerAI(model, harness, project string, meta map[string]string, forceName string) (*types.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if model == "" {
		model = "unknown"
	}
	if harness == "" {
		harness = "unknown"
	}

	var name string
	if forceName != "" {
		if !reclaimNamePattern.MatchString(forceName) {
			return nil, fmt.Errorf("name %q must match [a-z]{3,12}", forceName)
		}
		for _, a := range s.agents {
			if a.Name != forceName {
				continue
			}
			if a.Kind == "human" {
				return nil, fmt.Errorf("name %q is a human identity", forceName)
			}
			if a.Model == model && a.Harness == harness && a.Project == project {
				a.LastSeen = now()
				if len(meta) > 0 {
					if a.Meta == nil {
						a.Meta = make(map[string]string)
					}
					for k, v := range meta {
						a.Meta[k] = v
					}
				}
				s.persist()
				cp := *a
				go s.broadcastAgentUpdate()
				return &cp, nil
			}
			return nil, fmt.Errorf("name %q is held by another AI agent", forceName)
		}
		name = forceName
	} else {
		taken := make(map[string]bool, len(s.agents))
		for _, a := range s.agents {
			taken[a.Name] = true
		}

		picked, ok := pickRandomName(taken)
		if !ok {
			return nil, fmt.Errorf("name pool exhausted (%d agents registered)", len(s.agents))
		}
		name = picked
	}

	id := assembleID("ai", name, model, harness, project)

	// Extremely unlikely with random names, but guard anyway.
	if _, exists := s.agents[id]; exists {
		return nil, fmt.Errorf("generated ID %q already exists; retry", id)
	}

	agent := &types.Agent{
		ID:           id,
		Name:         name,
		Kind:         "ai",
		Model:        model,
		Harness:      harness,
		Project:      project,
		Meta:         meta,
		RegisteredAt: now(),
		LastSeen:     now(),
	}
	s.agents[id] = agent
	s.persist()
	cp := *agent
	go s.broadcastAgentUpdate()
	return &cp, nil
}

func (s *store) touchAgent(id string) {
	s.mu.Lock()
	broadcast := false
	if a, ok := s.agents[id]; ok {
		prev, _ := time.Parse(time.RFC3339, a.LastSeen)
		a.LastSeen = now()
		// Only broadcast if last_seen changed by more than 30s (throttle)
		if time.Since(prev) > 30*time.Second {
			broadcast = true
		}
	}
	if broadcast {
		s.persist()
	}
	s.mu.Unlock()

	if broadcast {
		s.broadcastAgentUpdate()
	}
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

func (s *store) listAgents() []types.Agent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	agents := make([]types.Agent, 0, len(s.agents))
	for _, a := range s.agents {
		agents = append(agents, *a)
	}
	return agents
}

// ── SSE subscriptions ──────────────────────────────────────────────

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

// enterWait increments the per-scope open-wait counter (scope = roomID, or
// "" for agent-wide) and broadcasts a presence event with waiting=true.
// Pair with a deferred leaveWait in the handler so cancellations still
// decrement.
func (s *store) enterWait(agentID, roomID string) {
	if agentID == "" {
		return
	}
	s.waitMu.Lock()
	scopes := s.openWaits[agentID]
	if scopes == nil {
		scopes = make(map[string]int)
		s.openWaits[agentID] = scopes
	}
	scopes[roomID]++
	s.waitMu.Unlock()
	cur := s.cursorFor(agentID, roomID)
	if roomID == "" {
		// Agent-wide waits aren't tied to a specific room's cursor; use -1
		// as an explicit "not applicable" sentinel so consumers don't
		// mistake it for "has never read anything".
		cur = -1
	}
	s.broadcastPresence(agentID, roomID, cur, true)
}

// leaveWait decrements the per-scope counter and, when that specific scope
// drops to zero, broadcasts waiting=false for it. Other scopes for the same
// agent remain unaffected.
func (s *store) leaveWait(agentID, roomID string) {
	if agentID == "" {
		return
	}
	s.waitMu.Lock()
	scopes := s.openWaits[agentID]
	scopeRemaining := -1
	if scopes != nil && scopes[roomID] > 0 {
		scopes[roomID]--
		scopeRemaining = scopes[roomID]
		if scopeRemaining == 0 {
			delete(scopes, roomID)
		}
		if len(scopes) == 0 {
			delete(s.openWaits, agentID)
		}
	}
	s.waitMu.Unlock()
	if scopeRemaining == 0 {
		cur := s.cursorFor(agentID, roomID)
		if roomID == "" {
			cur = -1
		}
		s.broadcastPresence(agentID, roomID, cur, false)
	}
}

// cursorFor returns the agent's stored read cursor for a room, or 0 if
// unknown. Does not initialize the cursor (unlike ensureRoomCursor).
func (s *store) cursorFor(agentID, roomID string) int64 {
	if agentID == "" {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a, ok := s.agents[agentID]; ok && a.ReadCursors != nil {
		return a.ReadCursors[roomID]
	}
	return 0
}

// broadcastPresence emits a `presence` meta event. For agent-wide waits
// (roomID == ""), the FE treats it as a bulk update across all of that
// agent's rooms. Called WITHOUT holding s.mu or waitMu.
func (s *store) broadcastPresence(agentID, roomID string, cursor int64, waiting bool) {
	evt := MetaEvent{
		Type: "presence",
		Data: map[string]any{
			"agent":   agentID,
			"room":    roomID,
			"cursor":  cursor,
			"waiting": waiting,
			"ts":      now(),
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

// roomPresence returns the presence entry for every member of a room. Used
// by GET /rooms/{id} to include members_presence in the response.
func (s *store) roomPresence(roomID string) []types.MemberPresence {
	s.mu.RLock()
	room, ok := s.rooms[roomID]
	if !ok {
		s.mu.RUnlock()
		return nil
	}
	members := append([]string{}, room.Members...)
	cursors := make(map[string]int64, len(members))
	for _, m := range members {
		if a, ok := s.agents[m]; ok && a.ReadCursors != nil {
			cursors[m] = a.ReadCursors[roomID]
		}
	}
	s.mu.RUnlock()

	s.waitMu.RLock()
	waits := make(map[string]bool, len(members))
	for _, m := range members {
		// Considered waiting for a given room if they have any open scope
		// — the specific room OR an agent-wide wait.
		scopes := s.openWaits[m]
		waits[m] = scopes[roomID] > 0 || scopes[""] > 0
	}
	s.waitMu.RUnlock()

	out := make([]types.MemberPresence, 0, len(members))
	for _, m := range members {
		out = append(out, types.MemberPresence{
			Agent:   m,
			Cursor:  cursors[m],
			Waiting: waits[m],
		})
	}
	return out
}

// ── Clear ──────────────────────────────────────────────────────────

func (s *store) clearAll() {
	s.mu.Lock()
	s.rooms = make(map[string]*types.Room)
	s.messages = make(map[string][]types.Message)
	s.agents = make(map[string]*types.Agent)
	s.persist()
	s.mu.Unlock()
	s.broadcastRoomUpdate()
	s.broadcastAgentUpdate()
}

// ── Cleanup ───────────────────────────────────────────────────────

// startCleanup runs periodic cleanup of stale agents and empty rooms.
// - Agents with no activity for 30 minutes are removed.
// - Rooms with no members for 60 minutes are removed.
func (s *store) startCleanup(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for {
			select {
			case <-ticker.C:
				s.cleanupStaleAgents()
				s.cleanupEmptyRooms()
			case <-ctx.Done():
				ticker.Stop()
				return
			}
		}
	}()
}

func (s *store) cleanupStaleAgents() {
	cutoff := time.Now().UTC().Add(-30 * time.Minute)

	s.mu.Lock()

	var removed []string
	for id, agent := range s.agents {
		lastSeen, err := time.Parse(time.RFC3339, agent.LastSeen)
		if err != nil {
			continue
		}
		if lastSeen.Before(cutoff) {
			removed = append(removed, id)
		}
	}

	if len(removed) == 0 {
		s.mu.Unlock()
		return
	}

	// Capture room memberships before removal so we can emit per-room events.
	removedRooms := make(map[string][]string)
	for _, id := range removed {
		for _, room := range s.rooms {
			for _, m := range room.Members {
				if m == id {
					removedRooms[id] = append(removedRooms[id], room.ID)
					break
				}
			}
		}
	}

	for _, id := range removed {
		delete(s.agents, id)
		for _, room := range s.rooms {
			for i, m := range room.Members {
				if m == id {
					room.Members = append(room.Members[:i], room.Members[i+1:]...)
					break
				}
			}
		}
	}

	log.Printf("Cleaned up %d stale agent(s): %v", len(removed), removed)
	s.persist()
	s.mu.Unlock()

	s.broadcastAgentUpdate()
	s.broadcastRoomUpdate()

	for _, id := range removed {
		for _, roomID := range removedRooms[id] {
			s.emitSystemMessage(roomID, id+" disconnected (stale)")
		}
	}
}

func (s *store) cleanupEmptyRooms() {
	cutoff := time.Now().UTC().Add(-60 * time.Minute)

	s.mu.Lock()

	var toDelete []string
	for id, room := range s.rooms {
		if len(room.Members) == 0 {
			if since, ok := s.roomEmptySince[id]; ok {
				if since.Before(cutoff) {
					toDelete = append(toDelete, id)
				}
			} else {
				s.roomEmptySince[id] = time.Now().UTC()
			}
		} else {
			delete(s.roomEmptySince, id)
		}
	}

	if len(toDelete) == 0 {
		s.mu.Unlock()
		return
	}

	for _, id := range toDelete {
		delete(s.rooms, id)
		delete(s.roomEmptySince, id)
		delete(s.messages, id)
	}

	log.Printf("Cleaned up %d empty room(s): %v", len(toDelete), toDelete)
	s.persist()
	s.mu.Unlock()

	s.broadcastRoomUpdate()
}

// ── Macros ────────────────────────────────────────────────────────

func (s *store) getEnvelope() macrosEnvelope {
	s.macrosMu.RLock()
	defer s.macrosMu.RUnlock()
	globals := make(map[string]string, len(s.macros))
	for k, v := range s.macros {
		globals[k] = v
	}
	rooms := make(map[string]map[string]string, len(s.macroRooms))
	for rid, rm := range s.macroRooms {
		cp := make(map[string]string, len(rm))
		for k, v := range rm {
			cp[k] = v
		}
		rooms[rid] = cp
	}
	return macrosEnvelope{Macros: globals, Rooms: rooms}
}

func (s *store) setEnvelope(env macrosEnvelope) {
	s.macrosMu.Lock()
	// Absent field (nil) = preserve existing; explicit empty map = wipe.
	// This lets MCP callers patch just globals or just rooms without clobbering
	// the other side. The frontend always sends both maps (even as `{}`) so its
	// full-replace semantics are unaffected.
	if env.Macros != nil {
		s.macros = env.Macros
	}
	if env.Rooms != nil {
		s.macroRooms = env.Rooms
	}
	s.macrosMu.Unlock()
	s.persistMacros()
	s.broadcastMacrosUpdated()
}

func (s *store) persistMacros() {
	s.macrosMu.RLock()
	sdKeys := make([]string, 0, len(s.seenDefaults))
	for k := range s.seenDefaults {
		sdKeys = append(sdKeys, k)
	}
	sort.Strings(sdKeys)
	var rooms map[string]map[string]string
	if len(s.macroRooms) > 0 {
		rooms = s.macroRooms
	}
	f := macrosEnvelope{Macros: s.macros, Rooms: rooms, SeenDefaults: sdKeys}
	data, err := json.MarshalIndent(f, "", "  ")
	s.macrosMu.RUnlock()
	if err == nil {
		atomicWrite(filepath.Join(s.dir, "macros.json"), data)
	}
}

// applyDefaultMacros merges defaults_macros.json into the global macro map
// once per key (write-once: keys already in seenDefaults are never touched).
// Called on server startup after load(). Conflicts (key exists in user macros)
// are silently skipped — the user's value wins.
func (s *store) applyDefaultMacros() {
	var defaults map[string]string
	if json.Unmarshal(defaultMacrosJSON, &defaults) != nil || len(defaults) == 0 {
		return
	}
	s.macrosMu.Lock()
	changed := false
	for k, v := range defaults {
		if s.seenDefaults[k] {
			continue
		}
		if _, exists := s.macros[k]; !exists {
			s.macros[k] = v
		}
		s.seenDefaults[k] = true
		changed = true
	}
	var data []byte
	if changed {
		sdKeys := make([]string, 0, len(s.seenDefaults))
		for k := range s.seenDefaults {
			sdKeys = append(sdKeys, k)
		}
		sort.Strings(sdKeys)
		var rooms map[string]map[string]string
		if len(s.macroRooms) > 0 {
			rooms = s.macroRooms
		}
		f := macrosEnvelope{Macros: s.macros, Rooms: rooms, SeenDefaults: sdKeys}
		data, _ = json.MarshalIndent(f, "", "  ")
	}
	s.macrosMu.Unlock()
	if data != nil {
		atomicWrite(filepath.Join(s.dir, "macros.json"), data)
	}
}

func (s *store) broadcastMacrosUpdated() {
	evt := MetaEvent{Type: "macros_updated", Data: map[string]any{}}
	s.subMu.Lock()
	for _, ch := range s.metaSubs {
		select {
		case ch <- evt:
		default:
		}
	}
	s.subMu.Unlock()
}

// ── Helpers ────────────────────────────────────────────────────────

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func randomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// isDM reports whether a room ID is a DM room.
func isDM(roomID string) bool {
	return strings.HasPrefix(roomID, "dm:")
}

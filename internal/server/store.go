package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

var staleDefaultMacroDigests = map[string][]string{
	"do-cr": {
		"b9da1cdbf66c5c2842fb54dfff3188609e376e9bf5a88c32b71dc68bd517d4c8",
	},
}

// macrosEnvelope is the canonical on-disk and API shape for macros.json.
// SeenDefaults is internal state (persisted but not exposed via the HTTP API).
// Older files used a per-user map[string]map[string]string (v1),
// {macros, seen_defaults} without rooms (v2), or included a rooms field (v3).
// load() detects and migrates all three; v3 rooms are merged into globals on
// first load and never written back.
type macrosEnvelope struct {
	Macros       map[string]string            `json:"macros"`
	Rooms        map[string]map[string]string `json:"rooms,omitempty"` // read-only for migration; never written after merge
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

// SoundEntry describes a user-uploaded custom notification sound.
type SoundEntry struct {
	UUID       string `json:"uuid"`
	Name       string `json:"name"`          // sanitized display name from the upload
	Size       int64  `json:"size"`          // bytes on disk
	UploadedAt string `json:"uploaded_at"`   // RFC3339
	Ext        string `json:"ext,omitempty"` // "mp3" or "wav"; empty means legacy mp3
}

type AttachmentEntry struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Mime       string `json:"mime"`
	Size       int64  `json:"size"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	UploadedAt string `json:"uploaded_at"`
	Ext        string `json:"ext"`
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
	cleanupResetCh chan struct{}

	macrosMu     sync.RWMutex
	macros       map[string]string // global shared macro map
	seenDefaults map[string]bool   // keys already offered via defaults (write-once)

	promptsMu sync.RWMutex
	prompts   map[string]string // user overrides only; compiled defaults are the fallback

	settingsMu sync.RWMutex
	settings   Settings

	fleetsMu sync.RWMutex
	fleets   map[string]Fleet

	soundsMu sync.RWMutex
	sounds   []SoundEntry // user-uploaded custom sounds

	attachmentsMu sync.RWMutex
	attachments   map[string]AttachmentEntry

	reactionsMu sync.RWMutex
	reactions   map[int64][]types.Reaction

	memoryMu sync.RWMutex
	memory   map[string]types.MemoryRecord

	rolesMu        sync.RWMutex
	rolesOverrides map[string]roleOverrideEntry // catalog key → metadata/body override
	rolesCustom    map[string]customRoleEntry   // custom key → metadata/body override

	// warnedLegacy tracks agents that have already received the one-time
	// legacy "name:" prefix warning. Runtime-only; resets on server restart.
	warnedLegacy map[string]bool
	// warnedAttention tracks agents that have already received the one-time
	// missing needs_attention warning. Runtime-only; resets on server restart.
	warnedAttention map[string]bool
	// warnedAmbiguousMention tracks agents that have already received the
	// one-time ambiguous @slug warning. Runtime-only; resets on server restart.
	warnedAmbiguousMention map[string]bool
}

func newStore(dir string) (*store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}

	s := &store{
		dir:                    dir,
		rooms:                  make(map[string]*types.Room),
		messages:               make(map[string][]types.Message),
		agents:                 make(map[string]*types.Agent),
		roomSubs:               make(map[string][]chan types.Message),
		openWaits:              make(map[string]map[string]int),
		roomEmptySince:         make(map[string]time.Time),
		cleanupResetCh:         make(chan struct{}, 1),
		macros:                 make(map[string]string),
		seenDefaults:           make(map[string]bool),
		prompts:                make(map[string]string),
		fleets:                 make(map[string]Fleet),
		attachments:            make(map[string]AttachmentEntry),
		reactions:              make(map[int64][]types.Reaction),
		memory:                 make(map[string]types.MemoryRecord),
		rolesOverrides:         make(map[string]roleOverrideEntry),
		rolesCustom:            make(map[string]customRoleEntry),
		warnedLegacy:           make(map[string]bool),
		warnedAttention:        make(map[string]bool),
		warnedAmbiguousMention: make(map[string]bool),
	}

	if err := s.load(); err != nil {
		log.Printf("Warning: could not load existing state: %v (starting fresh)", err)
	}

	// Apply cleanup rules once at startup so restarts don't accumulate
	// agents/rooms/messages that would have been pruned by the background
	// goroutine. Uses the configured retention windows.
	s.pruneOnStartup()
	s.cleanupAttachments(time.Now().UTC())
	s.cleanupReactionsForLiveMessages()

	// Create the _system room if it doesn't exist yet (idempotent).
	s.ensureSystemRoom()

	return s, nil
}

// PruneDataDir applies the same on-disk prune logic used by DELETE /all
// without requiring the HTTP server to be running.
func PruneDataDir(dir string, includeSettings bool) error {
	s, err := newStore(dir)
	if err != nil {
		return err
	}
	s.clearAll(includeSettings)
	return nil
}

// pruneOnStartup removes agents whose last_seen is past the stale window,
// and rooms that are empty (after the agent prune). Messages attached to
// deleted rooms are also removed. Called once from newStore.
func (s *store) pruneOnStartup() {
	now := time.Now().UTC()
	agentCutoff := now.Add(-s.staleAgentWindow())

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

	// Drop removed agents from room memberships and roles.
	for _, room := range s.rooms {
		filtered := room.Members[:0]
		for _, m := range room.Members {
			if !removedAgents[m] {
				filtered = append(filtered, m)
			} else if room.Roles != nil {
				delete(room.Roles, m)
			}
		}
		room.Members = filtered
	}

	// Drop rooms that are now empty. roomEmptySince isn't persisted, so on
	// restart any empty room is considered stale enough to prune.
	for id, room := range s.rooms {
		if len(room.Members) == 0 {
			delete(s.rooms, id)
			delete(s.messages, id)
		}
	}
	s.cleanupMessagesLocked(now)

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
			agents[i].State = ""
			agents[i].StateAt = time.Time{}
			s.agents[agents[i].ID] = &agents[i]
			if isReservedAgentName(agents[i].Name) {
				log.Printf("warning: persisted agent %q has reserved name %q; @%s now resolves to a group token, not this agent", agents[i].ID, agents[i].Name, agents[i].Name)
			}
		}
	}

	// Settings — persisted separately; survives schema wipes and prune (unless include_settings=true).
	s.loadSettings()

	// Prompt overrides — survives schema wipes and conversation prune; wiped by prune -a.
	s.loadPrompts()

	// Role overrides and custom roles — survives schema wipes and conversation prune; wiped by prune -a.
	s.loadRoles()

	// Fleets — named user command bundles; survives schema wipes and conversation prune; wiped by prune -a.
	s.loadFleets()

	// User sounds index — persisted in sounds/sounds.json; wiped when clearAll(includeSettings=true).
	s.loadSounds()

	// Image attachment registry — conversation state; regular prune wipes it.
	s.loadAttachments()

	// Emoji reactions — mutable conversation metadata; regular prune wipes it.
	s.loadReactions()

	// Curated bus memory — durable knowledge; survives conversation prune and
	// is wiped only by prune -a.
	s.loadMemory()

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
			// v3 migration: merge per-room macros into globals (deterministic:
			// room IDs sorted, keys within each room sorted; global-wins on collision).
			// After merge, rooms are never written back.
			if len(env.Rooms) > 0 {
				migrated, skippedGlobal, skippedClaimed := 0, 0, 0
				preExisting := make(map[string]bool, len(s.macros))
				for k := range s.macros {
					preExisting[k] = true
				}
				roomIDs := make([]string, 0, len(env.Rooms))
				for rid := range env.Rooms {
					roomIDs = append(roomIDs, rid)
				}
				sort.Strings(roomIDs)
				for _, rid := range roomIDs {
					rm := env.Rooms[rid]
					keys := make([]string, 0, len(rm))
					for k := range rm {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					for _, k := range keys {
						if preExisting[k] {
							skippedGlobal++
							continue
						}
						if _, exists := s.macros[k]; exists {
							skippedClaimed++
							continue
						}
						s.macros[k] = rm[k]
						migrated++
					}
				}
				log.Printf("macros: migrated %d global macro(s) from legacy per-room scope (skipped: %d global-exists, %d already-claimed-by-earlier-room)", migrated, skippedGlobal, skippedClaimed)
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
		cp := *a
		cp.State = ""
		cp.StateAt = time.Time{}
		agents = append(agents, cp)
	}
	if data, err := json.MarshalIndent(agents, "", "  "); err == nil {
		atomicWrite(filepath.Join(s.dir, "agents.json"), data)
	}
}

// atomicWrite writes data to a temp file, flushes to disk, and renames it
// to the target path, preventing data corruption if the process crashes.
func atomicWrite(path string, data []byte) {
	atomicWriteMode(path, data, 0o644)
}

func atomicWriteMode(path string, data []byte, mode os.FileMode) {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
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
	id := s.nextID.Add(1)
	msg := types.Message{
		ID:        id,
		RoomID:    roomID,
		From:      "_system",
		FromKind:  "system",
		Body:      body,
		CreatedAt: now(),
		Targets:   targets,
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

const (
	maxProposedAnswers              = 4
	maxOpenQuestions                = 10
	maxOpenQuestionOptions          = 8
	maxOpenQuestionTextRunes        = 500
	maxOpenQuestionDescriptionRunes = 1000
)

func cleanProposedAnswers(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, min(len(in), maxProposedAnswers))
	for _, answer := range in {
		answer = strings.TrimSpace(answer)
		if answer == "" {
			continue
		}
		out = append(out, answer)
		if len(out) == maxProposedAnswers {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cleanOpenQuestions(in []types.OpenQuestion) []types.OpenQuestion {
	if len(in) == 0 {
		return nil
	}
	out := make([]types.OpenQuestion, 0, min(len(in), maxOpenQuestions))
	for _, q := range in {
		question := truncateRunes(strings.TrimSpace(q.Question), maxOpenQuestionTextRunes)
		description := truncateRunes(strings.TrimSpace(q.Description), maxOpenQuestionDescriptionRunes)
		if question == "" {
			continue
		}
		options := make([]string, 0, min(len(q.Options), maxOpenQuestionOptions))
		for _, opt := range q.Options {
			opt = truncateRunes(strings.TrimSpace(opt), maxOpenQuestionTextRunes)
			if opt == "" {
				continue
			}
			options = append(options, opt)
			if len(options) == maxOpenQuestionOptions {
				break
			}
		}
		if len(options) < 2 {
			continue
		}
		cleaned := types.OpenQuestion{Question: question, Options: options}
		if description != "" {
			cleaned.Description = description
		}
		out = append(out, cleaned)
		if len(out) == maxOpenQuestions {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func (s *store) roomSend(roomID, from, body string, needsAttention bool, proposedAnswers []string, openQuestions []types.OpenQuestion, attachments []types.Attachment) (int64, error) {
	if !s.isMember(roomID, from) {
		return 0, fmt.Errorf("agent %s is not a member of room %s", from, roomID)
	}
	resolvedAttachments, err := s.resolveAttachments(attachments)
	if err != nil {
		return 0, err
	}

	ctx := s.addressingContext(types.Message{RoomID: roomID, From: from, Body: body})
	rawTargets := parseAddressedTo(body)
	targets := resolveAddressedTokens(rawTargets, ctx)
	if len(rawTargets) > 0 && targets == nil {
		targets = []string{}
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
		Targets:             targets,
		NeedsHumanAttention: needsAttention,
	}
	msg.ProposedAnswers = cleanProposedAnswers(proposedAnswers)
	msg.OpenQuestions = cleanOpenQuestions(openQuestions)
	msg.Attachments = resolvedAttachments

	s.mu.Lock()
	s.messages[roomID] = append(s.messages[roomID], msg)
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
	s.persistReactionsLocked()
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
		s.persistReactionsLocked()
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

// ── Agents ─────────────────────────────────────────────────────────

// registerHuman registers a human agent with the given explicit name. The
// human's ID is simply the name (no suffix, no project). Idempotent: if the
// name is already registered as a human, just touches last_seen. Errors if
// the name is in use by an AI agent.
func (s *store) registerHuman(name, project string, meta map[string]string) (*types.Agent, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required for human registration")
	}
	if isReservedAgentName(name) {
		return nil, fmt.Errorf("name %q is reserved", name)
	}

	s.mu.Lock()

	if existing, ok := s.agents[name]; ok {
		if existing.Kind != "human" {
			s.mu.Unlock()
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
		cp := cloneAgentLocked(existing)
		s.mu.Unlock()
		go s.broadcastAgentUpdate()
		s.joinRoomInternal("_system", name)
		return &cp, nil // re-register: no new event
	}

	// Also reject if any AI agent is currently using this name (their full
	// ID differs but their bare name matches — would cause confusion).
	for _, a := range s.agents {
		if a.Name == name {
			s.mu.Unlock()
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
	cp := cloneAgentLocked(agent)
	s.mu.Unlock()

	go s.broadcastAgentUpdate()
	s.joinRoomInternal("_system", name)
	s.emitSystemMessage("_system", name+" registered (human)")
	return &cp, nil
}

// findBySpawnTagLocked returns the first AI agent whose meta["spawn_tag"]
// matches tag AND whose (model, harness, project) tuple matches. Returns nil
// if no such agent exists. Must be called with s.mu held (read or write).
func (s *store) findBySpawnTagLocked(tag, model, harness, project string) *types.Agent {
	for _, a := range s.agents {
		if a.Kind != "ai" {
			continue
		}
		if a.Meta["spawn_tag"] != tag {
			continue
		}
		if a.Model == model && a.Harness == harness && a.Project == project {
			return a
		}
	}
	return nil
}

// registerAI registers an AI agent. Normally the server assigns a random
// slug from the pool and assembles the full ID from slug/project. If
// forceName is non-empty, the caller is asking to force-claim that slug in
// the current project. Reclaim rules:
//   - name must match ^[a-z]{3,12}$ (same shape as pool names)
//   - if current-project full ID is held by an AI with same model+harness+
//     project → idempotent: return the existing agent with last_seen touched
//   - if current-project full ID is held by an AI with different
//     model/harness/project → error
//   - otherwise the forced name is used instead of picking from the pool
//
// Additionally, if meta["spawn_tag"] is present and forceName is empty, the
// server attempts automatic reclaim: if an existing AI agent has the same
// spawn_tag and (model, harness, project), that agent's identity is returned
// without allocating a new pool name (reclaimed=true). Tag present but no
// match → fresh name as usual (reclaimed=false).
//
// Returns the created (or reclaimed) agent, a reclaimed flag, and any error.
func (s *store) registerAI(model, harness, project string, meta map[string]string, forceName string) (*types.Agent, bool, error) {
	s.mu.Lock()

	if model == "" {
		model = "unknown"
	}
	if harness == "" {
		harness = "unknown"
	}

	var name string
	if forceName != "" {
		if !reclaimNamePattern.MatchString(forceName) {
			s.mu.Unlock()
			return nil, false, fmt.Errorf("name %q must match [a-z]{3,12}", forceName)
		}
		if isReservedAgentName(forceName) {
			s.mu.Unlock()
			return nil, false, fmt.Errorf("name %q is reserved", forceName)
		}
		forceID := assembleID("ai", forceName, model, harness, project)
		if a, ok := s.agents[forceID]; ok {
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
				cp := cloneAgentLocked(a)
				s.mu.Unlock()
				go s.broadcastAgentUpdate()
				s.joinRoomInternal("_system", a.ID)
				return &cp, false, nil // explicit force-reclaim: reclaimed=false (caller already knew the name)
			}
			s.mu.Unlock()
			return nil, false, fmt.Errorf("name %q is held by another AI agent in project %q", forceName, project)
		}
		if s.roleKeyExists(forceName) {
			s.mu.Unlock()
			return nil, false, fmt.Errorf("name %q collides with a role key", forceName)
		}
		name = forceName
	} else {
		// spawn_tag automatic reclaim: if spawn_tag is present in meta and an
		// existing AI agent has the same (spawn_tag, model, harness, project),
		// return that identity without allocating a new pool name.
		if tag := meta["spawn_tag"]; tag != "" {
			if existing := s.findBySpawnTagLocked(tag, model, harness, project); existing != nil {
				existing.LastSeen = now()
				if len(meta) > 0 {
					if existing.Meta == nil {
						existing.Meta = make(map[string]string)
					}
					for k, v := range meta {
						existing.Meta[k] = v
					}
				}
				s.persist()
				cp := cloneAgentLocked(existing)
				s.mu.Unlock()
				go s.broadcastAgentUpdate()
				s.joinRoomInternal("_system", existing.ID)
				return &cp, true, nil // spawn_tag reclaim
			}
			// tag present but no matching agent → fall through to fresh name
		}

		taken := make(map[string]bool, len(s.agents))
		for _, a := range s.agents {
			if a.Kind == "ai" && a.Project == project {
				taken[a.Name] = true
			}
		}
		s.rolesMu.RLock()
		for _, e := range roleCatalog {
			taken[e.Key] = true
		}
		for k := range s.rolesCustom {
			taken[k] = true
		}
		s.rolesMu.RUnlock()

		picked, ok := pickRandomName(taken)
		if !ok {
			s.mu.Unlock()
			return nil, false, fmt.Errorf("name pool exhausted (%d agents registered)", len(s.agents))
		}
		name = picked
	}

	id := assembleID("ai", name, model, harness, project)

	// Extremely unlikely with random names, but guard anyway.
	if _, exists := s.agents[id]; exists {
		s.mu.Unlock()
		return nil, false, fmt.Errorf("generated ID %q already exists; retry", id)
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
	cp := cloneAgentLocked(agent)
	s.mu.Unlock()

	go s.broadcastAgentUpdate()
	s.joinRoomInternal("_system", id)
	s.emitSystemMessage("_system", fmt.Sprintf("%s registered (%s/%s)", id, model, harness))
	return &cp, false, nil
}

func (s *store) addressingContext(msg types.Message) addressingContext {
	ctx := addressingContext{
		SenderID:   msg.From,
		KnownNames: make(map[string]bool),
		RoleNames:  make(map[string][]string),
		Now:        time.Now().UTC(),
	}

	var memberIDs []string
	var roles map[string]string
	s.mu.RLock()
	for _, agent := range s.agents {
		ctx.KnownNames[strings.ToLower(agent.Name)] = true
	}
	if room, ok := s.rooms[msg.RoomID]; ok {
		memberIDs = append(memberIDs, room.Members...)
		if len(room.Roles) > 0 {
			roles = make(map[string]string, len(room.Roles))
			for agentID, roleKey := range room.Roles {
				roles[agentID] = roleKey
			}
		}
	}
	ctx.RoomAgents = make([]roomAgentContext, 0, len(memberIDs))
	for _, memberID := range memberIDs {
		agent, ok := s.agents[memberID]
		if !ok {
			continue
		}
		lastSeen, _ := time.Parse(time.RFC3339, agent.LastSeen)
		ctx.RoomAgents = append(ctx.RoomAgents, roomAgentContext{
			ID:       agent.ID,
			Name:     agent.Name,
			Kind:     agent.Kind,
			LastSeen: lastSeen,
		})
		if agent.Kind == "ai" {
			if roleKey := strings.ToLower(roles[memberID]); roleKey != "" && !isReservedAgentName(roleKey) {
				ctx.RoleNames[roleKey] = append(ctx.RoleNames[roleKey], strings.ToLower(agent.Name))
			}
		}
	}
	s.mu.RUnlock()

	s.waitMu.RLock()
	for i := range ctx.RoomAgents {
		scopes := s.openWaits[ctx.RoomAgents[i].ID]
		ctx.RoomAgents[i].Waiting = scopes[msg.RoomID] > 0 || scopes[""] > 0
	}
	s.waitMu.RUnlock()

	return ctx
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

func (s *store) heartbeatAgent(id string) bool {
	s.mu.Lock()
	a, ok := s.agents[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	prev, _ := time.Parse(time.RFC3339, a.LastSeen)
	a.LastSeen = now()
	broadcast := time.Since(prev) > 30*time.Second
	if broadcast {
		s.persist()
	}
	s.mu.Unlock()

	if broadcast {
		s.broadcastAgentUpdate()
	}
	return true
}

func isValidAgentState(state string) bool {
	switch state {
	case types.AgentStateBootstrapping,
		types.AgentStateIdle,
		types.AgentStateThinking,
		types.AgentStateToolCall,
		types.AgentStateRespawning,
		types.AgentStateError,
		types.AgentStateStopped,
		types.AgentStateStale:
		return true
	default:
		return false
	}
}

func (s *store) setAgentState(agentID, state string) bool {
	now := time.Now().UTC()
	updated := false

	s.mu.Lock()
	if a, ok := s.agents[agentID]; ok {
		if a.Kind != "ai" {
			s.mu.Unlock()
			return true
		}
		if a.State != state {
			updated = true
		}
		a.State = state
		a.StateAt = now
	} else {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()

	if updated {
		s.broadcastAgentUpdate()
	}
	return true
}

func (s *store) deriveAgentStates(now time.Time) {
	changed := false
	s.mu.Lock()
	for _, a := range s.agents {
		if a.Kind != "ai" {
			if a.State != "" || !a.StateAt.IsZero() {
				a.State = ""
				a.StateAt = time.Time{}
				changed = true
			}
			continue
		}
		next := ""
		if lastSeen, err := time.Parse(time.RFC3339, a.LastSeen); err == nil && now.Sub(lastSeen) > time.Minute {
			if a.State != types.AgentStateStopped && a.State != types.AgentStateError {
				next = types.AgentStateStale
			}
		}
		if next != "" && a.State != next {
			a.State = next
			a.StateAt = now
			changed = true
		}
	}
	s.mu.Unlock()

	if changed {
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

// cloneAgentLocked returns an agent snapshot whose mutable map fields no longer
// alias live store state, so callers can JSON-marshal it after releasing s.mu
// without risking concurrent map iteration panics. Caller must hold s.mu
// (read or write) while invoking it.
func cloneAgentLocked(a *types.Agent) types.Agent {
	clone := *a
	if a.Meta != nil {
		clone.Meta = make(map[string]string, len(a.Meta))
		for k, v := range a.Meta {
			clone.Meta[k] = v
		}
	}
	if a.ReadCursors != nil {
		clone.ReadCursors = make(map[string]int64, len(a.ReadCursors))
		for k, v := range a.ReadCursors {
			clone.ReadCursors[k] = v
		}
	}
	if a.Warnings != nil {
		clone.Warnings = append([]string(nil), a.Warnings...)
	}
	return clone
}

func (s *store) listAgents() []types.Agent {
	s.mu.RLock()
	agents := make([]types.Agent, 0, len(s.agents))
	for _, a := range s.agents {
		agents = append(agents, cloneAgentLocked(a))
	}
	s.mu.RUnlock()
	s.applyAgentWaitStateOverlay(agents)
	for i := range agents {
		if agents[i].Kind == "ai" && s.roleKeyExists(agents[i].Name) {
			agents[i].Warnings = append(agents[i].Warnings, roleNameCollisionWarning(agents[i].Name))
		}
	}
	return agents
}

func (s *store) applyAgentWaitStateOverlay(agents []types.Agent) {
	s.waitMu.RLock()
	defer s.waitMu.RUnlock()
	for i := range agents {
		if agents[i].Kind != "ai" {
			continue
		}
		if len(s.openWaits[agents[i].ID]) > 0 {
			agents[i].State = types.AgentStateIdle
		}
	}
}

// deregisterAgent removes an agent from the registry and from every room it is
// a member of. Returns false if the agent does not exist.
func (s *store) deregisterAgent(agentID string) bool {
	type vacatedRole struct {
		roomID  string
		roleKey string
	}

	s.mu.Lock()
	if _, ok := s.agents[agentID]; !ok {
		s.mu.Unlock()
		return false
	}

	var vacated []vacatedRole
	delete(s.agents, agentID)
	for roomID, room := range s.rooms {
		filtered := room.Members[:0]
		for _, member := range room.Members {
			if member != agentID {
				filtered = append(filtered, member)
			}
		}
		room.Members = filtered
		if roleKey, ok := room.Roles[agentID]; ok {
			delete(room.Roles, agentID)
			vacated = append(vacated, vacatedRole{roomID: roomID, roleKey: roleKey})
		}
	}
	s.persist()
	s.mu.Unlock()

	s.broadcastAgentUpdate()
	s.broadcastRoomUpdate()
	for _, role := range vacated {
		s.emitSystemMessage(role.roomID, fmt.Sprintf("role %q vacated: %s deregistered", role.roleKey, agentID))
	}
	s.emitSystemMessage("_system", agentID+" deregistered")
	return true
}

// knownAgentNames returns the short names of all currently-registered agents.
// Used by annotate to filter @mention captures to real agent names.
func (s *store) knownAgentNames() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make(map[string]bool, len(s.agents))
	for id := range s.agents {
		names[agentShortName(id)] = true
	}
	return names
}

// knownHumanNames returns the short names of all currently-registered human
// agents. Used by the needs_attention warning heuristic to distinguish human
// handoffs from AI-to-AI requests.
func (s *store) knownHumanNames() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make(map[string]bool)
	for id, agent := range s.agents {
		if agent.Kind != "human" {
			continue
		}
		names[agentShortName(id)] = true
	}
	return names
}

// legacyPrefixWarn checks whether body starts with a legacy IRC-style "name:"
// prefix matching a registered agent's short name. On first detection per
// sender it returns a one-time guidance string and marks the sender warned.
// Subsequent calls for the same sender, or bodies that don't match, return "".
func (s *store) legacyPrefixWarn(senderAgentID, body string) string {
	names := s.knownAgentNames() // acquires and releases its own RLock
	matchedName, matched := parseLegacyPrefix(body, names)
	var warning string
	if matched {
		warning = fmt.Sprintf("'%s:' prefix detected — this does not address %s; write '@%s ...'. Also drop self-labels: 'from' already identifies you. (one-time warning per session.)", matchedName, matchedName, matchedName)
	} else if matchedNames, ok := parseInlineLegacyPrefix(body, names); ok {
		warning = fmt.Sprintf("'%s, %s —' inline addressing detected — write '@%s @%s ...' so addressed_to fires. (one-time warning per session.)", matchedNames[0], matchedNames[len(matchedNames)-1], matchedNames[0], matchedNames[len(matchedNames)-1])
	} else {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.warnedLegacy[senderAgentID] {
		return ""
	}
	s.warnedLegacy[senderAgentID] = true
	return warning
}

// ambiguousMentionWarn returns a one-time warning when @slug matches multiple
// current-room members. The message is still sent, but that ambiguous slug is
// left unresolved in addressed_to; use @slug@project to disambiguate.
func (s *store) ambiguousMentionWarn(roomID, senderAgentID, body string) string {
	msg := types.Message{RoomID: roomID, From: senderAgentID, Body: body}
	raw := parseAddressedTo(body)
	ambiguous := ambiguousMentionTokens(raw, s.addressingContext(msg))
	if len(ambiguous) == 0 {
		return ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.warnedAmbiguousMention[senderAgentID] {
		return ""
	}
	s.warnedAmbiguousMention[senderAgentID] = true
	return fmt.Sprintf("@%s is ambiguous in room %s because multiple members share that slug; write the full @slug@project form. (one-time warning per session.)", ambiguous[0], roomID)
}

// attentionMissWarn returns a one-time warning when a sender addresses a human
// with a likely blocking request but omits needs_attention=true.
func (s *store) attentionMissWarn(roomID, senderAgentID, body string, needsAttention bool, extraAddressees []string) string {
	if needsAttention {
		return ""
	}
	msg := types.Message{RoomID: roomID, From: senderAgentID, Body: body}
	addressedTo := resolveAddressedTo(body, s.addressingContext(msg))
	knownAgents := s.knownAgentNames()
	if name, matched := parseLegacyPrefix(body, knownAgents); matched {
		addressedTo = append(addressedTo, name)
	}
	if names, matched := parseInlineLegacyPrefix(body, knownAgents); matched {
		addressedTo = append(addressedTo, names...)
	}
	addressedTo = append(addressedTo, extraAddressees...)

	if len(addressedTo) == 0 {
		return ""
	}

	seen := make(map[string]bool, len(addressedTo))
	deduped := addressedTo[:0]
	for _, name := range addressedTo {
		name = strings.ToLower(name)
		if seen[name] {
			continue
		}
		seen[name] = true
		deduped = append(deduped, name)
	}
	addressedTo = deduped

	humanNames := s.knownHumanNames()
	var humanTarget string
	for _, name := range addressedTo {
		if humanNames[name] {
			humanTarget = name
			break
		}
	}
	if humanTarget == "" {
		return ""
	}
	phrase, matched := parseAttentionMiss(body)
	if !matched {
		return ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.warnedAttention[senderAgentID] {
		return ""
	}
	s.warnedAttention[senderAgentID] = true
	return fmt.Sprintf("@%s addressed with a request for action (%q) but needs_attention=false. If this was a blocking handoff, immediately re-send the message with needs_attention=true so the human gets the alert. Do not rationalize this warning away — the human being currently active in the thread is not a carve-out. (one-time warning per session.)", humanTarget, phrase)
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
	hadOpenWait := len(scopes) > 0
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
	if !hadOpenWait {
		s.broadcastAgentUpdate()
	}
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
	hasOpenWait := len(scopes) > 0
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
	hasOpenWait = hasOpenWait && len(s.openWaits[agentID]) > 0
	s.waitMu.Unlock()
	if scopeRemaining == 0 {
		cur := s.cursorFor(agentID, roomID)
		if roomID == "" {
			cur = -1
		}
		s.broadcastPresence(agentID, roomID, cur, false)
		if !hasOpenWait {
			s.broadcastAgentUpdate()
		}
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

func (s *store) hasAgent(agentID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.agents[agentID]
	return ok
}

// ── Clear ──────────────────────────────────────────────────────────

func (s *store) clearAll(includeSettings bool) {
	s.mu.Lock()
	s.rooms = make(map[string]*types.Room)
	s.messages = make(map[string][]types.Message)
	s.agents = make(map[string]*types.Agent)
	s.persist()
	s.mu.Unlock()
	s.attachmentsMu.Lock()
	s.attachments = make(map[string]AttachmentEntry)
	s.attachmentsMu.Unlock()
	_ = os.RemoveAll(s.attachmentsDir())
	s.reactionsMu.Lock()
	s.reactions = make(map[int64][]types.Reaction)
	s.persistReactionsLocked()
	s.reactionsMu.Unlock()
	if includeSettings {
		s.clearMemory()
		s.macrosMu.Lock()
		s.macros = make(map[string]string)
		s.seenDefaults = make(map[string]bool)
		s.macrosMu.Unlock()
		s.applyDefaultMacros() // re-seeds macros.json with embedded defaults
		s.clearPrompts()
		s.clearRoles()
		s.clearSettings()
		s.clearFleets()
		s.soundsMu.Lock()
		s.sounds = nil
		s.soundsMu.Unlock()
		_ = os.RemoveAll(s.soundsDir())
	}
	s.broadcastRoomUpdate()
	s.broadcastAgentUpdate()
}

// ── Cleanup ───────────────────────────────────────────────────────

// startCleanup runs periodic cleanup of stale agents, empty rooms, and old
// messages using the retention settings from settings.json.
func (s *store) startCleanup(ctx context.Context) {
	ticker := time.NewTicker(s.cleanupInterval())
	go func() {
		for {
			select {
			case <-ticker.C:
				s.cleanupStaleAgents()
				s.cleanupEmptyRooms()
				s.cleanupMessages()
				s.deriveAgentStates(time.Now().UTC())
			case <-s.cleanupResetCh:
				ticker.Reset(s.cleanupInterval())
			case <-ctx.Done():
				ticker.Stop()
				return
			}
		}
	}()
}

func (s *store) cleanupStaleAgents() {
	cutoff := time.Now().UTC().Add(-s.staleAgentWindow())

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
			if room.Roles != nil {
				delete(room.Roles, id)
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
		s.emitSystemMessage("_system", id+" pruned (stale)")
	}
}

func (s *store) cleanupEmptyRooms() {
	cutoff := time.Now().UTC().Add(-s.emptyRoomWindow())

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
	s.cleanupReactionsForLiveMessages()

	s.broadcastRoomUpdate()
}

func (s *store) cleanupMessages() {
	now := time.Now().UTC()

	s.mu.Lock()
	changed := s.cleanupMessagesLocked(now)
	if changed {
		s.persist()
	}
	s.mu.Unlock()
	s.cleanupAttachments(now)
	s.cleanupReactionsForLiveMessages()
}

func (s *store) cleanupMessagesLocked(now time.Time) bool {
	retentionWindow := s.messageRetentionWindow()
	retentionCount := s.messageRetentionCount()
	if retentionWindow == 0 && retentionCount == 0 {
		return false
	}

	var cutoff time.Time
	if retentionWindow > 0 {
		cutoff = now.Add(-retentionWindow)
	}

	changed := false
	for roomID, roomMessages := range s.messages {
		start := 0
		if retentionWindow > 0 {
			for start < len(roomMessages) {
				createdAt, err := time.Parse(time.RFC3339, roomMessages[start].CreatedAt)
				if err != nil || !createdAt.Before(cutoff) {
					break
				}
				start++
			}
		}
		if retentionCount > 0 {
			countStart := len(roomMessages) - retentionCount
			if countStart > start {
				start = countStart
			}
		}
		if start > 0 {
			kept := append([]types.Message(nil), roomMessages[start:]...)
			s.messages[roomID] = kept
			changed = true
		}
	}
	return changed
}

func (s *store) requestCleanupReset() {
	select {
	case s.cleanupResetCh <- struct{}{}:
	default:
	}
}

// ── Macros ────────────────────────────────────────────────────────

func (s *store) getEnvelope() macrosEnvelope {
	s.macrosMu.RLock()
	defer s.macrosMu.RUnlock()
	globals := make(map[string]string, len(s.macros))
	for k, v := range s.macros {
		globals[k] = v
	}
	return macrosEnvelope{Macros: globals}
}

func (s *store) setEnvelope(env macrosEnvelope) {
	s.macrosMu.Lock()
	if env.Macros != nil {
		s.macros = env.Macros
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
	f := macrosEnvelope{Macros: s.macros, SeenDefaults: sdKeys}
	data, err := json.MarshalIndent(f, "", "  ")
	s.macrosMu.RUnlock()
	if err == nil {
		atomicWrite(filepath.Join(s.dir, "macros.json"), data)
	}
}

func defaultMacros() map[string]string {
	var defaults map[string]string
	if json.Unmarshal(defaultMacrosJSON, &defaults) != nil {
		return nil
	}
	return defaults
}

func macroSHA256(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// migrateStaleDefaultMacros updates persisted macro defaults only when the
// user's current body matches a known stale shipped digest. Customized macros
// are left untouched because their digests will not match.
func (s *store) migrateStaleDefaultMacros() {
	defaults := defaultMacros()
	if len(defaults) == 0 || len(staleDefaultMacroDigests) == 0 {
		return
	}

	s.macrosMu.Lock()
	changed := false
	for key, staleDigests := range staleDefaultMacroDigests {
		current, exists := s.macros[key]
		if !exists {
			continue
		}
		defaultBody, exists := defaults[key]
		if !exists {
			continue
		}
		currentDigest := macroSHA256(current)
		for _, staleDigest := range staleDigests {
			if currentDigest == staleDigest {
				s.macros[key] = defaultBody
				changed = true
				break
			}
		}
	}
	var data []byte
	if changed {
		sdKeys := make([]string, 0, len(s.seenDefaults))
		for k := range s.seenDefaults {
			sdKeys = append(sdKeys, k)
		}
		sort.Strings(sdKeys)
		f := macrosEnvelope{Macros: s.macros, SeenDefaults: sdKeys}
		data, _ = json.MarshalIndent(f, "", "  ")
	}
	s.macrosMu.Unlock()
	if data != nil {
		atomicWrite(filepath.Join(s.dir, "macros.json"), data)
	}
}

// applyDefaultMacros merges defaults_macros.json into the global macro map
// once per key (write-once: keys already in seenDefaults are never touched).
// Called on server startup after load(). Conflicts (key exists in user macros)
// are silently skipped — the user's value wins.
func (s *store) applyDefaultMacros() {
	defaults := defaultMacros()
	if len(defaults) == 0 {
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
		f := macrosEnvelope{Macros: s.macros, SeenDefaults: sdKeys}
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

// ── User sounds ────────────────────────────────────────────────────

const (
	maxSoundFiles     = 10
	maxSoundTotalSize = 20 * 1024 * 1024 // 20 MB
	maxSoundFileSize  = 1 * 1024 * 1024  // 1 MB per file
)

func (s *store) soundsDir() string {
	return filepath.Join(s.dir, "sounds")
}

func (s *store) soundsIndexPath() string {
	return filepath.Join(s.soundsDir(), "sounds.json")
}

func (s *store) loadSounds() {
	data, err := os.ReadFile(s.soundsIndexPath())
	if err != nil {
		return
	}
	var env struct {
		Sounds []SoundEntry `json:"sounds"`
	}
	if json.Unmarshal(data, &env) == nil {
		s.soundsMu.Lock()
		s.sounds = env.Sounds
		s.soundsMu.Unlock()
	}
}

// persistSoundsLocked writes the sounds index to disk. Must be called with
// soundsMu held (read or write). Does not acquire the lock.
func (s *store) persistSoundsLocked() {
	env := struct {
		Sounds []SoundEntry `json:"sounds"`
	}{Sounds: s.sounds}
	data, err := json.MarshalIndent(env, "", "  ")
	if err == nil {
		atomicWrite(s.soundsIndexPath(), data)
	}
}

func (s *store) listSounds() []SoundEntry {
	s.soundsMu.RLock()
	defer s.soundsMu.RUnlock()
	out := make([]SoundEntry, len(s.sounds))
	copy(out, s.sounds)
	return out
}

// addSound stores an uploaded MP3 file and adds it to the index.
// data must have already been validated (size, header).
func (s *store) addSound(displayName string, data []byte, ext string) (SoundEntry, error) {
	// Cap and total-size checks under write lock before touching the filesystem.
	s.soundsMu.Lock()
	if len(s.sounds) >= maxSoundFiles {
		s.soundsMu.Unlock()
		return SoundEntry{}, fmt.Errorf("sound limit reached (%d files); delete one first", maxSoundFiles)
	}
	var total int64
	for _, e := range s.sounds {
		total += e.Size
	}
	if total+int64(len(data)) > maxSoundTotalSize {
		s.soundsMu.Unlock()
		return SoundEntry{}, fmt.Errorf("total sound storage limit reached (%.0f MB)", float64(maxSoundTotalSize)/1024/1024)
	}
	s.soundsMu.Unlock()

	// Write to disk outside the lock (no lock needed — UUID is unique).
	uuid := randomUUID()
	if err := os.MkdirAll(s.soundsDir(), 0o755); err != nil {
		return SoundEntry{}, fmt.Errorf("create sounds dir: %w", err)
	}
	path := filepath.Join(s.soundsDir(), uuid+"."+ext)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return SoundEntry{}, fmt.Errorf("write sound file: %w", err)
	}

	entry := SoundEntry{
		UUID:       uuid,
		Name:       displayName,
		Size:       int64(len(data)),
		UploadedAt: now(),
		Ext:        ext,
	}

	// Re-acquire write lock to append + persist. Re-check caps in case a concurrent
	// upload raced past the first check between the unlock and this re-lock.
	s.soundsMu.Lock()
	if len(s.sounds) >= maxSoundFiles {
		s.soundsMu.Unlock()
		_ = os.Remove(path)
		return SoundEntry{}, fmt.Errorf("sound limit reached (%d files); delete one first", maxSoundFiles)
	}
	var total2 int64
	for _, e := range s.sounds {
		total2 += e.Size
	}
	if total2+int64(len(data)) > maxSoundTotalSize {
		s.soundsMu.Unlock()
		_ = os.Remove(path)
		return SoundEntry{}, fmt.Errorf("total sound storage limit reached (%.0f MB)", float64(maxSoundTotalSize)/1024/1024)
	}
	s.sounds = append(s.sounds, entry)
	s.persistSoundsLocked()
	s.soundsMu.Unlock()

	return entry, nil
}

// deleteSound removes a user sound by UUID. Returns false if the UUID is unknown.
func (s *store) deleteSound(uuid string) bool {
	s.soundsMu.Lock()
	defer s.soundsMu.Unlock()

	for i, e := range s.sounds {
		if e.UUID != uuid {
			continue
		}
		s.sounds = append(s.sounds[:i], s.sounds[i+1:]...)
		fileExt := e.Ext
		if fileExt == "" {
			fileExt = "mp3"
		}
		_ = os.Remove(filepath.Join(s.soundsDir(), uuid+"."+fileExt))
		s.persistSoundsLocked()
		return true
	}
	return false
}

// soundFilePath returns the on-disk path and extension ("mp3" or "wav") for a
// user sound UUID, or ("", "") if unknown. Legacy entries with empty Ext are
// treated as "mp3".
func (s *store) soundFilePath(uuid string) (string, string) {
	s.soundsMu.RLock()
	defer s.soundsMu.RUnlock()
	for _, e := range s.sounds {
		if e.UUID == uuid {
			ext := e.Ext
			if ext == "" {
				ext = "mp3"
			}
			return filepath.Join(s.soundsDir(), uuid+"."+ext), ext
		}
	}
	return "", ""
}

// ── Image attachments ──────────────────────────────────────────────

const (
	maxAttachmentFileSize    = 5 * 1024 * 1024
	maxMessageAttachments    = 4
	attachmentOrphanGrace    = time.Hour
	attachmentRegistryFile   = "attachments.json"
	attachmentDefaultFileExt = "bin"
)

func (s *store) attachmentsDir() string {
	return filepath.Join(s.dir, "attachments")
}

func (s *store) attachmentsIndexPath() string {
	return filepath.Join(s.attachmentsDir(), attachmentRegistryFile)
}

func attachmentExt(mime string) string {
	switch mime {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return attachmentDefaultFileExt
	}
}

func attachmentMeta(entry AttachmentEntry) types.Attachment {
	return types.Attachment{
		ID:     entry.ID,
		Mime:   entry.Mime,
		Name:   entry.Name,
		Size:   entry.Size,
		Width:  entry.Width,
		Height: entry.Height,
	}
}

func (s *store) loadAttachments() {
	data, err := os.ReadFile(s.attachmentsIndexPath())
	if err != nil {
		return
	}
	var env struct {
		Attachments []AttachmentEntry `json:"attachments"`
	}
	if json.Unmarshal(data, &env) != nil {
		return
	}
	s.attachmentsMu.Lock()
	for _, entry := range env.Attachments {
		if entry.ID == "" {
			continue
		}
		if entry.Ext == "" {
			entry.Ext = attachmentExt(entry.Mime)
		}
		s.attachments[entry.ID] = entry
	}
	s.attachmentsMu.Unlock()
}

func (s *store) persistAttachmentsLocked() {
	entries := make([]AttachmentEntry, 0, len(s.attachments))
	for _, entry := range s.attachments {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].UploadedAt < entries[j].UploadedAt
	})
	env := struct {
		Attachments []AttachmentEntry `json:"attachments"`
	}{Attachments: entries}
	if data, err := json.MarshalIndent(env, "", "  "); err == nil {
		atomicWrite(s.attachmentsIndexPath(), data)
	}
}

func (s *store) addAttachment(displayName, mime string, data []byte, width, height int) (types.Attachment, error) {
	if len(data) > maxAttachmentFileSize {
		return types.Attachment{}, fmt.Errorf("file too large (max %d MB)", maxAttachmentFileSize/1024/1024)
	}
	id := randomUUID()
	ext := attachmentExt(mime)
	if err := os.MkdirAll(s.attachmentsDir(), 0o755); err != nil {
		return types.Attachment{}, fmt.Errorf("create attachments dir: %w", err)
	}
	path := filepath.Join(s.attachmentsDir(), id+"."+ext)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return types.Attachment{}, fmt.Errorf("write attachment file: %w", err)
	}
	entry := AttachmentEntry{
		ID:         id,
		Name:       displayName,
		Mime:       mime,
		Size:       int64(len(data)),
		Width:      width,
		Height:     height,
		UploadedAt: now(),
		Ext:        ext,
	}
	s.attachmentsMu.Lock()
	s.attachments[id] = entry
	s.persistAttachmentsLocked()
	s.attachmentsMu.Unlock()
	return attachmentMeta(entry), nil
}

func (s *store) attachmentFilePath(id string) (AttachmentEntry, string, bool) {
	s.attachmentsMu.RLock()
	defer s.attachmentsMu.RUnlock()
	entry, ok := s.attachments[id]
	if !ok {
		return AttachmentEntry{}, "", false
	}
	ext := entry.Ext
	if ext == "" {
		ext = attachmentExt(entry.Mime)
	}
	return entry, filepath.Join(s.attachmentsDir(), id+"."+ext), true
}

func (s *store) resolveAttachments(in []types.Attachment) ([]types.Attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxMessageAttachments {
		return nil, fmt.Errorf("too many attachments (max %d)", maxMessageAttachments)
	}
	out := make([]types.Attachment, 0, len(in))
	seen := make(map[string]bool, len(in))
	s.attachmentsMu.RLock()
	defer s.attachmentsMu.RUnlock()
	for _, ref := range in {
		id := strings.TrimSpace(ref.ID)
		if !validUUID(id) {
			return nil, fmt.Errorf("invalid attachment id %q", id)
		}
		if seen[id] {
			continue
		}
		entry, ok := s.attachments[id]
		if !ok {
			return nil, fmt.Errorf("attachment %s not found", id)
		}
		out = append(out, attachmentMeta(entry))
		seen[id] = true
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (s *store) attachmentReferenced(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, roomMessages := range s.messages {
		for _, msg := range roomMessages {
			for _, attachment := range msg.Attachments {
				if attachment.ID == id {
					return true
				}
			}
		}
	}
	return false
}

func (s *store) deleteAttachment(id string) (bool, bool) {
	if !validUUID(id) {
		return false, false
	}
	if s.attachmentReferenced(id) {
		return true, false
	}
	s.attachmentsMu.Lock()
	defer s.attachmentsMu.Unlock()
	entry, ok := s.attachments[id]
	if !ok {
		return false, false
	}
	delete(s.attachments, id)
	ext := entry.Ext
	if ext == "" {
		ext = attachmentExt(entry.Mime)
	}
	_ = os.Remove(filepath.Join(s.attachmentsDir(), id+"."+ext))
	s.persistAttachmentsLocked()
	return true, true
}

func (s *store) cleanupAttachments(now time.Time) {
	referenced := make(map[string]bool)
	s.mu.RLock()
	for _, roomMessages := range s.messages {
		for _, msg := range roomMessages {
			for _, attachment := range msg.Attachments {
				referenced[attachment.ID] = true
			}
		}
	}
	s.mu.RUnlock()

	cutoff := now.Add(-attachmentOrphanGrace)
	changed := false
	s.attachmentsMu.Lock()
	for id, entry := range s.attachments {
		if referenced[id] {
			continue
		}
		uploadedAt, err := time.Parse(time.RFC3339, entry.UploadedAt)
		if err == nil && uploadedAt.After(cutoff) {
			continue
		}
		ext := entry.Ext
		if ext == "" {
			ext = attachmentExt(entry.Mime)
		}
		_ = os.Remove(filepath.Join(s.attachmentsDir(), id+"."+ext))
		delete(s.attachments, id)
		changed = true
	}
	if changed {
		s.persistAttachmentsLocked()
	}
	s.attachmentsMu.Unlock()

	entries, err := os.ReadDir(s.attachmentsDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == attachmentRegistryFile {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		stem := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if !referenced[stem] {
			_, _, known := s.attachmentFilePath(stem)
			if !known {
				_ = os.Remove(filepath.Join(s.attachmentsDir(), entry.Name()))
			}
		}
	}
}

// randomUUID returns a random 32-hex-char UUID-like string.
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func validUUID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return true
}

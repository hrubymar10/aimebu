package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// SlugPattern is the canonical AI-agent slug rule: start with a lowercase
// letter, end with a lowercase letter or digit, hyphens and underscores
// allowed in the interior only, total length 3–21 chars.
// Used by both the server (force-claim) and the agent wrapper (--name/--resume-name).
const SlugPatternStr = `^[a-z][a-z0-9_-]{1,19}[a-z0-9]$`

// SlugPattern is the compiled form of SlugPatternStr.
var SlugPattern = regexp.MustCompile(SlugPatternStr)

// reclaimNamePattern is kept as an alias for internal callers that predate
// the export; new code should use SlugPattern directly.
var reclaimNamePattern = SlugPattern

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
	db     *sql.DB
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
	openWS    map[string]int

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

	leaderboardsMu sync.RWMutex
	leaderboards   []types.LeaderboardRatingCard

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

type livenessEvent struct {
	AgentID string
	Kind    string
	Rooms   []livenessRoomEvent
}

type livenessRoomEvent struct {
	RoomID  string
	RoleKey string
	Humans  []string
}

const (
	livenessEventOffline   = "offline"
	livenessEventReconnect = "reconnect"
)

const storeSchemaVersion = sqliteSchemaVersion


// ── Clear ──────────────────────────────────────────────────────────

func (s *store) clearAll(includeSettings bool) {
	s.mu.Lock()
	s.rooms = make(map[string]*types.Room)
	s.messages = make(map[string][]types.Message)
	s.agents = make(map[string]*types.Agent)
	s.persistFullCoreLocked()
	s.mu.Unlock()
	s.attachmentsMu.Lock()
	s.attachments = make(map[string]AttachmentEntry)
	if s.db != nil {
		if err := s.clearTable("attachments"); err != nil {
			log.Printf("aimebu: clear attachments sqlite: %v", err)
		}
	}
	s.attachmentsMu.Unlock()
	_ = os.RemoveAll(s.attachmentsDir())
	s.reactionsMu.Lock()
	s.reactions = make(map[int64][]types.Reaction)
	s.persistReactionsLocked()
	s.reactionsMu.Unlock()
	if includeSettings {
		s.clearMemory()
		s.clearLeaderboards()
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
		if s.db != nil {
			if err := s.clearTable("sounds"); err != nil {
				log.Printf("aimebu: clear sounds sqlite: %v", err)
			}
		}
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
	livenessTicker := time.NewTicker(s.livenessSweepInterval())
	go func() {
		for {
			select {
			case <-ticker.C:
				s.cleanupStaleAgents()
				s.cleanupEmptyRooms()
				s.cleanupMessages()
			case <-livenessTicker.C:
				s.deriveAgentStates(time.Now().UTC())
			case <-s.cleanupResetCh:
				ticker.Reset(s.cleanupInterval())
				livenessTicker.Reset(s.livenessSweepInterval())
			case <-ctx.Done():
				ticker.Stop()
				livenessTicker.Stop()
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
			s.emitSystemMessage(roomID, id+" pruned after stale timeout")
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
	s.persistFullCoreLocked()
	s.mu.Unlock()
	s.cleanupReactionsForLiveMessages()

	s.broadcastRoomUpdate()
}

func (s *store) cleanupMessages() {
	now := time.Now().UTC()

	s.mu.Lock()
	changed := s.cleanupMessagesLocked(now)
	if changed {
		s.persistFullCoreLocked()
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
	if s.db != nil {
		if err := s.persistMacrosSQLiteLocked(); err != nil {
			log.Printf("aimebu: persist macros sqlite: %v", err)
		}
		s.macrosMu.RUnlock()
		return
	}
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
		if s.db != nil {
			if err := s.persistMacrosSQLiteLocked(); err != nil {
				log.Printf("aimebu: migrate default macros sqlite: %v", err)
			}
			s.macrosMu.Unlock()
			return
		}
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
		if s.db != nil {
			if err := s.persistMacrosSQLiteLocked(); err != nil {
				log.Printf("aimebu: apply default macros sqlite: %v", err)
			}
			s.macrosMu.Unlock()
			return
		}
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


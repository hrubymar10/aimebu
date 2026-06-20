package server

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

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
		openWS:                 make(map[string]int),
		roomEmptySince:         make(map[string]time.Time),
		cleanupResetCh:         make(chan struct{}, 1),
		macros:                 make(map[string]string),
		seenDefaults:           make(map[string]bool),
		prompts:                make(map[string]string),
		fleets:                 make(map[string]Fleet),
		attachments:            make(map[string]AttachmentEntry),
		reactions:              make(map[int64][]types.Reaction),
		memory:                 make(map[string]types.MemoryRecord),
		leaderboards:           []types.LeaderboardRatingCard{},
		rolesOverrides:         make(map[string]roleOverrideEntry),
		rolesCustom:            make(map[string]customRoleEntry),
		warnedLegacy:           make(map[string]bool),
		warnedAttention:        make(map[string]bool),
		warnedAmbiguousMention: make(map[string]bool),
	}

	if err := s.openSQLite(); err != nil {
		return nil, err
	}

	if err := s.load(); err != nil {
		return nil, err
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

	s.persistFullCoreLocked()

	if len(removedAgents) > 0 {
		log.Printf("Startup prune: removed %d stale agent(s)", len(removedAgents))
	}
}

func (s *store) load() error {
	if err := s.loadCoreSQLite(); err != nil {
		return err
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

	// Leaderboard cards — durable evaluation records; survives conversation
	// prune and is wiped only by prune -a.
	s.loadLeaderboards()

	// Macros — separate from schema-versioned state; survives schema wipes.
	// Handles three historical shapes:
	//   v1: per-user  {agentID: {key: body}}
	//   v2: global    {macros: {key: body}, seen_defaults: [...]}
	//   v3: global+rooms {macros: {...}, rooms: {roomID: {...}}, seen_defaults: [...]}
	if s.db != nil {
		if err := s.loadMacrosSQLite(); err != nil {
			log.Printf("aimebu: load macros sqlite: %v", err)
		}
		return nil
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "macros.json"))
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
	if s.db != nil {
		if err := s.persistRoomsAgentsSQLiteLocked(); err != nil {
			log.Printf("Warning: failed to persist sqlite room/agent state: %v", err)
		}
		return
	}

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

func (s *store) persistFullCoreLocked() {
	if s.db != nil {
		if err := s.persistCoreSQLiteLocked(); err != nil {
			log.Printf("Warning: failed to persist sqlite core state: %v", err)
		}
		return
	}
	s.persist()
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

package server

import (
	"context"
	"log"
)

// RoomPref is a per-human view preference for a room: Hidden (excluded from the
// sidebar) and/or Pinned (sorted to the top). Stored per (agent_id, room_id);
// only humans hide rooms. Under the solo-only rule, a room can be hidden
// only when the hider is alone in it (enforced at set time via
// roomHasOtherMember); anyone joining unhides it (clearHiddenForRoomLocked
// runs on every join, before the joiner can post).
// Plain prune preserves room_prefs (a view preference is neither conversation
// nor agent state; humans re-register with the same bare ID); prune -a wipes it.
type RoomPref struct {
	Hidden bool
	Pinned bool
}

// roomPref returns the hidden/pinned preference for (agentID, roomID). Zero
// value when unset.
func (s *store) roomPref(agentID, roomID string) RoomPref {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if prefs, ok := s.roomPrefs[agentID]; ok {
		if p, ok := prefs[roomID]; ok {
			return *p
		}
	}
	return RoomPref{}
}

// roomHasOtherMember reports whether roomID has any member other than
// agentID. Used to enforce the solo-only hide rule: you can hide a room
// only when you are alone in it, so hidden=true is rejected if anyone else is
// present (human or AI). The companion rule — anyone joining unhides — is
// enforced in the join paths via clearHiddenForRoomLocked.
func (s *store) roomHasOtherMember(roomID, agentID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	room, ok := s.rooms[roomID]
	if !ok {
		return false
	}
	for _, m := range room.Members {
		if m != agentID {
			return true
		}
	}
	return false
}

// roomHasAIAgent reports whether roomID currently has any AI agent member.
// Used by the kick path (kick-agents is AI-only) and its tests.
func (s *store) roomHasAIAgent(roomID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	room, ok := s.rooms[roomID]
	if !ok {
		return false
	}
	for _, m := range room.Members {
		if a, ok := s.agents[m]; ok && a.Kind == "ai" {
			return true
		}
	}
	return false
}

// setRoomPref sets hidden/pinned for (agentID, roomID). nil fields are left
// unchanged. Entries with both flags false are deleted. The caller must have
// already enforced the solo-only rule for hidden=true — you may hide a room
// only when you are alone in it (the HTTP handler does this via
// roomHasOtherMember; store callers like the sweep do not hide).
func (s *store) setRoomPref(agentID, roomID string, hidden, pinned *bool) RoomPref {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefs, ok := s.roomPrefs[agentID]
	if !ok {
		prefs = make(map[string]*RoomPref)
		s.roomPrefs[agentID] = prefs
	}
	p, ok := prefs[roomID]
	if !ok {
		p = &RoomPref{}
		prefs[roomID] = p
	}
	if hidden != nil {
		p.Hidden = *hidden
	}
	if pinned != nil {
		p.Pinned = *pinned
	}
	if !p.Hidden && !p.Pinned {
		delete(prefs, roomID)
		if len(prefs) == 0 {
			delete(s.roomPrefs, agentID)
		}
	}
	s.persistRoomPrefsLocked()
	return *p
}

// clearHiddenForRoomLocked clears every agent's "hidden" flag for roomID.
// Called on any join, human or AI: you may hide a room only while you are
// alone in it, so a hidden room comes back for everyone the moment anyone
// else arrives — before the joiner can post.
// Caller holds s.mu. Returns whether anything changed.
//
// NOTE: an empty inner map may be left behind when an agent's last pref for
// roomID is cleared here; setRoomPref cleans those up, but this join-path does
// not. Cosmetic only — no correctness impact, no unbounded growth (one entry
// per agent that ever set a pref) — and not worth a branch on this path.
func (s *store) clearHiddenForRoomLocked(roomID string) bool {
	changed := false
	for _, prefs := range s.roomPrefs {
		if p, ok := prefs[roomID]; ok && p.Hidden {
			p.Hidden = false
			if !p.Pinned {
				delete(prefs, roomID)
			}
			changed = true
		}
	}
	return changed
}

// unhideRoomForAddressedLocked clears "hidden" for roomID only for agents whose
// IDs or short names match one of targets. Used when a needs_attention message
// lands in a hidden room: the people it's blocking must see it, but only them —
// one attention flag must not rearrange everyone's sidebar. Group mentions
// (@here/@channel/@humans) already expand to individual agents in targets, so
// the group case reaches everybody addressed. Caller holds s.mu.
func (s *store) unhideRoomForAddressedLocked(roomID string, targets []string) bool {
	if len(targets) == 0 {
		return false
	}
	changed := false
	for aid, prefs := range s.roomPrefs {
		p, ok := prefs[roomID]
		if !ok || !p.Hidden {
			continue
		}
		for _, t := range targets {
			if addressedMatchesAgent(t, aid) {
				p.Hidden = false
				if !p.Pinned {
					delete(prefs, roomID)
				}
				changed = true
				break
			}
		}
	}
	return changed
}

// clearAllRoomPrefsLocked wipes every room preference. Used by prune -a only
// (plain prune preserves view preferences). Caller holds s.mu.
func (s *store) clearAllRoomPrefsLocked() {
	if len(s.roomPrefs) == 0 {
		return
	}
	s.roomPrefs = make(map[string]map[string]*RoomPref)
	s.persistRoomPrefsLocked()
}

func (s *store) persistRoomPrefsLocked() {
	if s.db == nil {
		return
	}
	if err := s.persistRoomPrefsSQLiteLocked(); err != nil {
		log.Printf("aimebu: persist room_prefs sqlite: %v", err)
	}
}

func (s *store) loadRoomPrefs() {
	if s.db == nil {
		return
	}
	if err := s.loadRoomPrefsSQLite(); err != nil {
		log.Printf("aimebu: loadRoomPrefs sqlite: %v", err)
	}
}

func (s *store) loadRoomPrefsSQLite() error {
	rows, err := s.db.Query(`SELECT agent_id, room_id, hidden, pinned FROM room_prefs`)
	if err != nil {
		return err
	}
	defer rows.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for rows.Next() {
		var aid, rid string
		var hidden, pinned int
		if err := rows.Scan(&aid, &rid, &hidden, &pinned); err != nil {
			return err
		}
		if s.roomPrefs[aid] == nil {
			s.roomPrefs[aid] = make(map[string]*RoomPref)
		}
		s.roomPrefs[aid][rid] = &RoomPref{Hidden: hidden != 0, Pinned: pinned != 0}
	}
	return rows.Err()
}

func (s *store) persistRoomPrefsSQLiteLocked() error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM room_prefs`); err != nil {
		_ = tx.Rollback()
		return err
	}
	for aid, prefs := range s.roomPrefs {
		for rid, p := range prefs {
			if !p.Hidden && !p.Pinned {
				continue
			}
			h, pn := 0, 0
			if p.Hidden {
				h = 1
			}
			if p.Pinned {
				pn = 1
			}
			if _, err := tx.Exec(`INSERT INTO room_prefs(agent_id, room_id, hidden, pinned) VALUES(?,?,?,?)`, aid, rid, h, pn); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
}

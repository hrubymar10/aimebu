package server

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/internal/types"
)

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
//   - name must match SlugPatternStr (3–21 chars, alpha start, alphanumeric end)
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
			return nil, false, fmt.Errorf("name %q must match %s (3–21 chars, start with letter, end with letter/digit, hyphens/underscores interior only)", forceName, SlugPatternStr)
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
		types.AgentStateStale,
		types.AgentStateOffline:
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
	// Activity states double as keepalives so heads-down work doesn't age to stale.
	if state == types.AgentStateThinking || state == types.AgentStateToolCall {
		s.touchAgent(agentID)
	}
	return true
}

func (s *store) deriveAgentStates(now time.Time) {
	s.emitLivenessEvents(s.sweepAgentLiveness(now))
}

func (s *store) activeAgentIDs() map[string]bool {
	active := make(map[string]bool)
	s.waitMu.RLock()
	for agentID, scopes := range s.openWaits {
		if len(scopes) > 0 {
			active[agentID] = true
		}
	}
	for agentID, count := range s.openWS {
		if count > 0 {
			active[agentID] = true
		}
	}
	s.waitMu.RUnlock()
	return active
}

func (s *store) sweepAgentLiveness(now time.Time) []livenessEvent {
	active := s.activeAgentIDs()
	staleWindow := s.agentStaleWindow()
	offlineWindow := s.agentOfflineWindow()
	var events []livenessEvent
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

		prev := a.State
		next := prev
		if active[a.ID] {
			if prev == types.AgentStateOffline {
				events = append(events, livenessEvent{
					AgentID: a.ID,
					Kind:    livenessEventReconnect,
					Rooms:   s.livenessRoomsLocked(a.ID),
				})
			}
			if prev == types.AgentStateStale || prev == types.AgentStateOffline {
				next = types.AgentStateIdle
			}
		} else if prev != types.AgentStateStopped && prev != types.AgentStateError {
			if lastSeen, err := time.Parse(time.RFC3339, a.LastSeen); err == nil {
				age := now.Sub(lastSeen)
				switch {
				case age > offlineWindow:
					next = types.AgentStateOffline
				case age > staleWindow:
					next = types.AgentStateStale
				case prev == types.AgentStateStale || prev == types.AgentStateOffline:
					if prev == types.AgentStateOffline {
						events = append(events, livenessEvent{
							AgentID: a.ID,
							Kind:    livenessEventReconnect,
							Rooms:   s.livenessRoomsLocked(a.ID),
						})
					}
					next = types.AgentStateIdle
				}
			}
		}

		if prev != next {
			a.State = next
			a.StateAt = now
			changed = true
			if next == types.AgentStateOffline {
				events = append(events, livenessEvent{
					AgentID: a.ID,
					Kind:    livenessEventOffline,
					Rooms:   s.livenessRoomsLocked(a.ID),
				})
			}
		}
	}
	s.mu.Unlock()

	if changed {
		s.broadcastAgentUpdate()
	}
	return events
}

func (s *store) livenessRoomsLocked(agentID string) []livenessRoomEvent {
	var rooms []livenessRoomEvent
	for roomID, room := range s.rooms {
		if !memberInRoomLocked(room, agentID) {
			continue
		}
		var humans []string
		for _, memberID := range room.Members {
			if a, ok := s.agents[memberID]; ok && a.Kind == "human" {
				humans = append(humans, a.ID)
			}
		}
		roleKey := ""
		if room.Roles != nil {
			roleKey = room.Roles[agentID]
		}
		rooms = append(rooms, livenessRoomEvent{RoomID: roomID, RoleKey: roleKey, Humans: humans})
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].RoomID < rooms[j].RoomID })
	return rooms
}

func (s *store) emitLivenessEvents(events []livenessEvent) {
	for _, evt := range events {
		for _, room := range evt.Rooms {
			switch evt.Kind {
			case livenessEventOffline:
				body := fmt.Sprintf("%s looks offline (no activity for %s)", livenessSubject(room.RoleKey, evt.AgentID), formatDurationForHumans(s.agentOfflineWindow()))
				s.emitSystemMessageFull(room.RoomID, body, room.Humans, len(room.Humans) > 0)
			case livenessEventReconnect:
				s.emitSystemMessage(room.RoomID, evt.AgentID+" reconnected")
			}
		}
	}
}


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
		if len(s.openWaits[agents[i].ID]) > 0 || s.openWS[agents[i].ID] > 0 {
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

func (s *store) enterWS(agentID string) {
	if agentID == "" {
		return
	}
	s.waitMu.Lock()
	hadActive := len(s.openWaits[agentID]) > 0 || s.openWS[agentID] > 0
	s.openWS[agentID]++
	s.waitMu.Unlock()
	if !hadActive {
		s.broadcastAgentUpdate()
	}
}

func (s *store) leaveWS(agentID string) {
	if agentID == "" {
		return
	}
	s.waitMu.Lock()
	if s.openWS[agentID] > 0 {
		s.openWS[agentID]--
		if s.openWS[agentID] == 0 {
			delete(s.openWS, agentID)
		}
	}
	hasActive := len(s.openWaits[agentID]) > 0 || s.openWS[agentID] > 0
	s.waitMu.Unlock()
	if !hasActive {
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

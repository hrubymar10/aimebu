package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

const (
	projectFactsMemoryCap     = 4096
	userProfileMemoryCap      = 2048
	agentSharedNotesMemoryCap = 4096
	agentSharedNotesMemoryKey = "global"
	maxMemoryBodyRunes        = 2000
	maxRecallLimit            = 10
)

type memoryEnvelope struct {
	Records []types.MemoryRecord `json:"records"`
}

type memoryScopeRef struct {
	Scope string
	Key   string
}

type memoryCapError struct {
	Scope   string               `json:"scope"`
	Key     string               `json:"key"`
	Usage   types.MemoryUsage    `json:"usage"`
	Records []types.MemoryRecord `json:"records"`
}

func (e *memoryCapError) Error() string {
	return fmt.Sprintf("memory cap exceeded for %s/%s (%d/%d bytes)", e.Scope, e.Key, e.Usage.Used, e.Usage.Cap)
}

type memoryConflictError struct {
	ID     string             `json:"id"`
	Reason string             `json:"reason"`
	Record types.MemoryRecord `json:"record"`
}

func (e *memoryConflictError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("memory record %s %s", e.ID, e.Reason)
	}
	return fmt.Sprintf("memory record %s version is stale", e.ID)
}

type memoryBodyTooLongError struct {
	MaxRunes int `json:"max_runes"`
	GotRunes int `json:"got_runes"`
}

func (e *memoryBodyTooLongError) Error() string {
	return fmt.Sprintf("memory body exceeds %d runes (%d); shorten or summarize it", e.MaxRunes, e.GotRunes)
}

type memoryDisabledError struct {
	Reason string `json:"reason"`
}

func (e *memoryDisabledError) Error() string {
	if e.Reason != "" {
		return "memory disabled: " + e.Reason
	}
	return "memory disabled"
}

func (s *store) loadMemory() {
	data, err := os.ReadFile(filepath.Join(s.dir, "memory.json"))
	if err != nil {
		return
	}
	var env memoryEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return
	}
	s.memoryMu.Lock()
	s.memory = make(map[string]types.MemoryRecord, len(env.Records))
	for _, r := range env.Records {
		if r.ID == "" || !validMemoryScope(r.Scope) || r.ScopeKey == "" {
			continue
		}
		if r.Version <= 0 {
			r.Version = 1
		}
		s.memory[r.ID] = r
	}
	s.memoryMu.Unlock()
}

func (s *store) persistMemoryLocked() {
	records := make([]types.MemoryRecord, 0, len(s.memory))
	for _, r := range s.memory {
		records = append(records, r)
	}
	sortMemoryRecords(records)
	if data, err := json.MarshalIndent(memoryEnvelope{Records: records}, "", "  "); err == nil {
		atomicWrite(filepath.Join(s.dir, "memory.json"), data)
	}
}

func (s *store) clearMemory() {
	s.memoryMu.Lock()
	s.memory = make(map[string]types.MemoryRecord)
	s.memoryMu.Unlock()
	_ = os.Remove(filepath.Join(s.dir, "memory.json"))
}

func (s *store) memoryGloballyEnabled() bool {
	settings := s.getSettings()
	return settings.MemoryEnabled != nil && *settings.MemoryEnabled
}

func (s *store) memoryAgentKind(agentID string) (string, error) {
	agent, ok := s.memoryAgent(agentID)
	if !ok {
		return "", fmt.Errorf("agent not found")
	}
	return agent.Kind, nil
}

func (s *store) memoryManagementAllowed(agentID string) error {
	if s.memoryGloballyEnabled() {
		return nil
	}
	kind, err := s.memoryAgentKind(agentID)
	if err != nil {
		return err
	}
	if kind == "human" {
		return nil
	}
	return &memoryDisabledError{Reason: "global memory is off"}
}

func (s *store) memoryWriteAllowed(agentID string, sourceMessageID int64) error {
	if _, ok := s.memoryAgent(agentID); !ok {
		return fmt.Errorf("agent not found")
	}
	if !s.memoryGloballyEnabled() {
		return &memoryDisabledError{Reason: "global memory is off"}
	}
	if sourceMessageID == 0 {
		return nil
	}
	roomID, ok := s.sourceMessageRoom(sourceMessageID)
	if !ok {
		return fmt.Errorf("source_message_id %d does not exist", sourceMessageID)
	}
	if !s.roomMemoryContentEnabled(roomID) {
		return &memoryDisabledError{Reason: "source room memory is disabled"}
	}
	return nil
}

func (s *store) sourceMessageRoom(id int64) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.messageRoomLocked(id)
}

func (s *store) roomMemoryContentEnabled(roomID string) bool {
	if !s.memoryGloballyEnabled() {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	room := s.rooms[roomID]
	if room == nil {
		return false
	}
	if room.MemoryEnabled != nil {
		return *room.MemoryEnabled
	}
	return true
}

func validMemoryScope(scope string) bool {
	switch scope {
	case types.MemoryScopeProjectFacts, types.MemoryScopeUserProfile, types.MemoryScopeAgentSharedNotes:
		return true
	default:
		return false
	}
}

func memoryCap(scope string) int {
	switch scope {
	case types.MemoryScopeProjectFacts:
		return projectFactsMemoryCap
	case types.MemoryScopeUserProfile:
		return userProfileMemoryCap
	case types.MemoryScopeAgentSharedNotes:
		return agentSharedNotesMemoryCap
	default:
		return 0
	}
}

func memoryUsage(scope, key string, records []types.MemoryRecord) types.MemoryUsage {
	var used int
	for _, r := range records {
		if r.Scope == scope && r.ScopeKey == key {
			used += len([]byte(r.Body))
		}
	}
	capacity := memoryCap(scope)
	percent := 0
	if capacity > 0 {
		percent = used * 100 / capacity
	}
	return types.MemoryUsage{Scope: scope, Key: key, Used: used, Cap: capacity, Percent: percent}
}

func memoryAllowedRefs(agent *types.Agent) []memoryScopeRef {
	if agent == nil {
		return nil
	}
	refs := []memoryScopeRef{{Scope: types.MemoryScopeAgentSharedNotes, Key: agentSharedNotesMemoryKey}}
	if agent.Project != "" {
		refs = append(refs, memoryScopeRef{Scope: types.MemoryScopeProjectFacts, Key: agent.Project})
	}
	if agent.Kind == "human" {
		refs = append(refs, memoryScopeRef{Scope: types.MemoryScopeUserProfile, Key: agent.ID})
	}
	return refs
}

func (s *store) memoryAgent(agentID string) (*types.Agent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.agents[agentID]
	if !ok {
		return nil, false
	}
	cp := cloneAgentLocked(a)
	return &cp, true
}

func (s *store) memoryScopeKey(agent *types.Agent, scope, requestedKey string, forWrite bool) (string, error) {
	if agent == nil {
		return "", fmt.Errorf("agent not found")
	}
	switch scope {
	case types.MemoryScopeProjectFacts:
		if agent.Kind == "human" && requestedKey != "" {
			return requestedKey, nil
		}
		if agent.Project == "" {
			return "", fmt.Errorf("project_facts memory requires a non-empty project")
		}
		if requestedKey != "" && requestedKey != agent.Project {
			return "", fmt.Errorf("project_facts scope_key must match caller project %q", agent.Project)
		}
		return agent.Project, nil
	case types.MemoryScopeAgentSharedNotes:
		return agentSharedNotesMemoryKey, nil
	case types.MemoryScopeUserProfile:
		key := requestedKey
		if key == "" && agent.Kind == "human" {
			key = agent.ID
		}
		if key == "" {
			return "", fmt.Errorf("user_profile memory requires scope_key with the human id")
		}
		if forWrite && agent.Kind == "human" && key != agent.ID {
			return "", fmt.Errorf("humans may only write their own user_profile")
		}
		return key, nil
	default:
		return "", fmt.Errorf("unknown memory scope %q", scope)
	}
}

func (s *store) listMemory(agentID, scope, requestedKey string) (types.MemorySnapshot, error) {
	if err := s.memoryManagementAllowed(agentID); err != nil {
		return types.MemorySnapshot{}, err
	}
	agent, ok := s.memoryAgent(agentID)
	if !ok {
		return types.MemorySnapshot{}, fmt.Errorf("agent not found")
	}
	var refs []memoryScopeRef
	if scope != "" {
		key, err := s.memoryScopeKey(agent, scope, requestedKey, false)
		if err != nil {
			return types.MemorySnapshot{}, err
		}
		refs = []memoryScopeRef{{Scope: scope, Key: key}}
	} else if agent.Kind == "human" {
		return s.allMemorySnapshot(), nil
	} else {
		refs = memoryAllowedRefs(agent)
		if agent.Kind == "ai" {
			refs = append(refs, s.userProfileRefs()...)
		}
	}
	return s.memorySnapshot(refs), nil
}

func (s *store) userProfileRefs() []memoryScopeRef {
	s.memoryMu.RLock()
	defer s.memoryMu.RUnlock()
	seen := make(map[string]bool)
	for _, r := range s.memory {
		if r.Scope == types.MemoryScopeUserProfile {
			seen[r.ScopeKey] = true
		}
	}
	refs := make([]memoryScopeRef, 0, len(seen))
	for key := range seen {
		refs = append(refs, memoryScopeRef{Scope: types.MemoryScopeUserProfile, Key: key})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Key < refs[j].Key })
	return refs
}

func (s *store) memorySnapshotForAgent(agent *types.Agent) *types.MemorySnapshot {
	if agent == nil {
		return nil
	}
	if !s.memoryGloballyEnabled() {
		return nil
	}
	refs := memoryAllowedRefs(agent)
	if agent.Kind == "ai" {
		refs = append(refs, s.userProfileRefs()...)
	}
	snap := s.memorySnapshot(refs)
	return &snap
}

func (s *store) memorySnapshot(refs []memoryScopeRef) types.MemorySnapshot {
	allowed := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if validMemoryScope(ref.Scope) && ref.Key != "" {
			allowed[ref.Scope+"\x00"+ref.Key] = true
		}
	}
	s.memoryMu.RLock()
	records := make([]types.MemoryRecord, 0)
	for _, r := range s.memory {
		if allowed[r.Scope+"\x00"+r.ScopeKey] {
			records = append(records, r)
		}
	}
	s.memoryMu.RUnlock()
	sortMemoryRecords(records)
	usage := make([]types.MemoryUsage, 0, len(refs))
	for _, ref := range refs {
		if validMemoryScope(ref.Scope) && ref.Key != "" {
			usage = append(usage, memoryUsage(ref.Scope, ref.Key, records))
		}
	}
	return types.MemorySnapshot{Records: records, Usage: usage, Rendered: renderMemory(records)}
}

func (s *store) allMemorySnapshot() types.MemorySnapshot {
	s.memoryMu.RLock()
	records := make([]types.MemoryRecord, 0, len(s.memory))
	seenRefs := make(map[string]memoryScopeRef)
	for _, r := range s.memory {
		records = append(records, r)
		if validMemoryScope(r.Scope) && r.ScopeKey != "" {
			seenRefs[r.Scope+"\x00"+r.ScopeKey] = memoryScopeRef{Scope: r.Scope, Key: r.ScopeKey}
		}
	}
	s.memoryMu.RUnlock()
	sortMemoryRecords(records)
	refs := make([]memoryScopeRef, 0, len(seenRefs))
	for _, ref := range seenRefs {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Scope != refs[j].Scope {
			return refs[i].Scope < refs[j].Scope
		}
		return refs[i].Key < refs[j].Key
	})
	usage := make([]types.MemoryUsage, 0, len(refs))
	for _, ref := range refs {
		usage = append(usage, memoryUsage(ref.Scope, ref.Key, records))
	}
	return types.MemorySnapshot{Records: records, Usage: usage, Rendered: renderMemory(records)}
}

func (s *store) addMemory(agentID, scope, requestedKey, body string, sourceMessageID int64) (types.MemoryRecord, error) {
	if err := s.memoryWriteAllowed(agentID, sourceMessageID); err != nil {
		return types.MemoryRecord{}, err
	}
	agent, ok := s.memoryAgent(agentID)
	if !ok {
		return types.MemoryRecord{}, fmt.Errorf("agent not found")
	}
	key, err := s.memoryScopeKey(agent, scope, requestedKey, true)
	if err != nil {
		return types.MemoryRecord{}, err
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return types.MemoryRecord{}, fmt.Errorf("body is required")
	}
	if got := utf8.RuneCountInString(body); got > maxMemoryBodyRunes {
		return types.MemoryRecord{}, &memoryBodyTooLongError{MaxRunes: maxMemoryBodyRunes, GotRunes: got}
	}
	now := now()
	record := types.MemoryRecord{
		ID:              randomID(),
		Scope:           scope,
		ScopeKey:        key,
		Body:            body,
		Version:         1,
		Author:          agentID,
		SourceMessageID: sourceMessageID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	records := s.recordsForScopeLocked(scope, key)
	records = append(records, record)
	usage := memoryUsage(scope, key, records)
	if usage.Used > usage.Cap {
		return types.MemoryRecord{}, &memoryCapError{Scope: scope, Key: key, Usage: usage, Records: records}
	}
	s.memory[record.ID] = record
	s.persistMemoryLocked()
	return record, nil
}

func (s *store) updateMemory(agentID, id string, version int, body string) (types.MemoryRecord, error) {
	if err := s.memoryManagementAllowed(agentID); err != nil {
		return types.MemoryRecord{}, err
	}
	agent, ok := s.memoryAgent(agentID)
	if !ok {
		return types.MemoryRecord{}, fmt.Errorf("agent not found")
	}
	body = strings.TrimSpace(body)
	if id == "" || body == "" {
		return types.MemoryRecord{}, fmt.Errorf("id and body are required")
	}
	if got := utf8.RuneCountInString(body); got > maxMemoryBodyRunes {
		return types.MemoryRecord{}, &memoryBodyTooLongError{MaxRunes: maxMemoryBodyRunes, GotRunes: got}
	}
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	record, ok := s.memory[id]
	if !ok {
		return types.MemoryRecord{}, fmt.Errorf("memory record not found")
	}
	resolved, err := s.memoryScopeKey(agent, record.Scope, record.ScopeKey, true)
	if err != nil {
		return types.MemoryRecord{}, err
	}
	if resolved != record.ScopeKey {
		return types.MemoryRecord{}, fmt.Errorf("memory scope mismatch")
	}
	if version <= 0 {
		return types.MemoryRecord{}, fmt.Errorf("version is required")
	}
	if version != record.Version {
		return types.MemoryRecord{}, &memoryConflictError{ID: id, Reason: "version is stale", Record: record}
	}
	next := record
	next.Body = body
	next.Version++
	next.UpdatedAt = now()
	records := s.recordsForScopeLocked(record.Scope, record.ScopeKey)
	for i := range records {
		if records[i].ID == id {
			records[i] = next
			break
		}
	}
	usage := memoryUsage(record.Scope, record.ScopeKey, records)
	if usage.Used > usage.Cap {
		return types.MemoryRecord{}, &memoryCapError{Scope: record.Scope, Key: record.ScopeKey, Usage: usage, Records: records}
	}
	s.memory[id] = next
	s.persistMemoryLocked()
	return next, nil
}

func (s *store) removeMemory(agentID, id string, version int) (types.MemoryRecord, error) {
	if err := s.memoryManagementAllowed(agentID); err != nil {
		return types.MemoryRecord{}, err
	}
	agent, ok := s.memoryAgent(agentID)
	if !ok {
		return types.MemoryRecord{}, fmt.Errorf("agent not found")
	}
	if id == "" {
		return types.MemoryRecord{}, fmt.Errorf("id is required")
	}
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	record, ok := s.memory[id]
	if !ok {
		return types.MemoryRecord{}, fmt.Errorf("memory record not found")
	}
	resolved, err := s.memoryScopeKey(agent, record.Scope, record.ScopeKey, true)
	if err != nil {
		return types.MemoryRecord{}, err
	}
	if resolved != record.ScopeKey {
		return types.MemoryRecord{}, fmt.Errorf("memory scope mismatch")
	}
	if version <= 0 {
		return types.MemoryRecord{}, fmt.Errorf("version is required")
	}
	if version != record.Version {
		return types.MemoryRecord{}, &memoryConflictError{ID: id, Reason: "version is stale", Record: record}
	}
	delete(s.memory, id)
	s.persistMemoryLocked()
	return record, nil
}

func (s *store) cleanMemory(agentID, scope, requestedKey string) ([]types.MemoryRecord, error) {
	kind, err := s.memoryAgentKind(agentID)
	if err != nil {
		return nil, err
	}
	if kind != "human" {
		return nil, fmt.Errorf("memory clean requires a registered human")
	}
	var key string
	if scope != "" {
		agent, _ := s.memoryAgent(agentID)
		key, err = s.memoryScopeKey(agent, scope, requestedKey, false)
		if err != nil {
			return nil, err
		}
	}
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	var removed []types.MemoryRecord
	for id, r := range s.memory {
		if scope != "" && (r.Scope != scope || r.ScopeKey != key) {
			continue
		}
		removed = append(removed, r)
		delete(s.memory, id)
	}
	if len(removed) > 0 {
		sortMemoryRecords(removed)
		s.persistMemoryLocked()
	}
	return removed, nil
}

func (s *store) recordsForScopeLocked(scope, key string) []types.MemoryRecord {
	records := make([]types.MemoryRecord, 0)
	for _, r := range s.memory {
		if r.Scope == scope && r.ScopeKey == key {
			records = append(records, r)
		}
	}
	sortMemoryRecords(records)
	return records
}

func sortMemoryRecords(records []types.MemoryRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].Scope != records[j].Scope {
			return records[i].Scope < records[j].Scope
		}
		if records[i].ScopeKey != records[j].ScopeKey {
			return records[i].ScopeKey < records[j].ScopeKey
		}
		if records[i].UpdatedAt != records[j].UpdatedAt {
			return records[i].UpdatedAt < records[j].UpdatedAt
		}
		return records[i].ID < records[j].ID
	})
}

func renderMemory(records []types.MemoryRecord) string {
	if len(records) == 0 {
		return ""
	}
	var b strings.Builder
	current := ""
	for _, r := range records {
		header := r.Scope + "/" + r.ScopeKey
		if header != current {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("## ")
			b.WriteString(header)
			b.WriteByte('\n')
			current = header
		}
		b.WriteString("- [")
		b.WriteString(r.ID)
		b.WriteString("] ")
		b.WriteString(strings.ReplaceAll(r.Body, "\n", " "))
		if r.Author != "" {
			b.WriteString(" (author: ")
			b.WriteString(r.Author)
			b.WriteByte(')')
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func (s *store) recallMessages(agentID, query string, limit int) ([]types.RecallResult, error) {
	if _, ok := s.memoryAgent(agentID); !ok {
		return nil, fmt.Errorf("agent not found")
	}
	if !s.memoryGloballyEnabled() {
		return nil, &memoryDisabledError{Reason: "global memory is off"}
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	if limit <= 0 || limit > maxRecallLimit {
		limit = maxRecallLimit
	}
	terms := tokenizeRecall(query)
	qLower := strings.ToLower(query)

	s.mu.RLock()
	var scored []types.RecallResult
	for _, room := range s.rooms {
		if !roomHasMember(room, agentID) {
			continue
		}
		if room.MemoryEnabled != nil && !*room.MemoryEnabled {
			continue
		}
		for _, msg := range s.messages[room.ID] {
			if msg.FromKind == "system" {
				continue
			}
			bodyLower := strings.ToLower(msg.Body)
			score := 0
			if strings.Contains(bodyLower, qLower) {
				score += 10
			}
			for _, term := range terms {
				if strings.Contains(bodyLower, term) {
					score += 2
				}
			}
			if score == 0 {
				continue
			}
			scored = append(scored, types.RecallResult{
				MessageID: msg.ID,
				RoomID:    msg.RoomID,
				From:      msg.From,
				CreatedAt: msg.CreatedAt,
				Snippet:   recallSnippet(msg.Body, terms, qLower),
				Score:     score,
			})
		}
	}
	s.mu.RUnlock()

	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].MessageID > scored[j].MessageID
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}

func roomHasMember(room *types.Room, agentID string) bool {
	if room == nil {
		return false
	}
	for _, member := range room.Members {
		if member == agentID {
			return true
		}
	}
	return false
}

func tokenizeRecall(query string) []string {
	seen := make(map[string]bool)
	var terms []string
	for _, part := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		terms = append(terms, part)
	}
	return terms
}

func recallSnippet(body string, terms []string, qLower string) string {
	const maxSnippetRunes = 220
	body = strings.TrimSpace(body)
	if utf8.RuneCountInString(body) <= maxSnippetRunes {
		return body
	}
	lower := strings.ToLower(body)
	idx := strings.Index(lower, qLower)
	if idx < 0 {
		for _, term := range terms {
			if found := strings.Index(lower, term); found >= 0 {
				idx = found
				break
			}
		}
	}
	if idx < 0 {
		idx = 0
	}
	runes := []rune(body)
	startRune := utf8.RuneCountInString(body[:idx]) - 60
	if startRune < 0 {
		startRune = 0
	}
	endRune := startRune + maxSnippetRunes
	if endRune > len(runes) {
		endRune = len(runes)
		startRune = endRune - maxSnippetRunes
		if startRune < 0 {
			startRune = 0
		}
	}
	snippet := string(runes[startRune:endRune])
	if startRune > 0 {
		snippet = "..." + snippet
	}
	if endRune < len(runes) {
		snippet += "..."
	}
	return snippet
}

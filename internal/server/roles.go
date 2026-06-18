package server

import (
	_ "embed"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

//go:embed defaults_roles.json
var defaultRoleCatalogJSON []byte

var roleKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// ErrRoomNotFound is returned by assignRole when the target room doesn't exist.
var ErrRoomNotFound = errors.New("room not found")

// ErrRoleAssignmentConflict is returned when a role assignment violates a room
// constraint such as singleton cardinality.
var ErrRoleAssignmentConflict = errors.New("role assignment conflict")

type roleCatalogEntry struct {
	Key         string `json:"key"`
	Description string `json:"description"`
	Emoji       string `json:"emoji,omitempty"`
	Icon        string `json:"icon,omitempty"` // legacy alias for emoji
	Cardinality string `json:"cardinality,omitempty"`
	Extends     string `json:"extends,omitempty"`
}

// RoleEntry is the full shape returned by GET /roles.
type RoleEntry struct {
	Key          string `json:"key"`
	Label        string `json:"label"` // legacy alias; always equals Key
	Description  string `json:"description"`
	Emoji        string `json:"emoji,omitempty"`
	Icon         string `json:"icon,omitempty"`
	Body         string `json:"body"`
	ResolvedBody string `json:"resolved_body,omitempty"`
	DefaultBody  string `json:"default_body"`
	Cardinality  string `json:"cardinality,omitempty"`
	Extends      string `json:"extends,omitempty"`
	Overridden   bool   `json:"overridden"`
	IsCustom     bool   `json:"is_custom"`
}

type rolesEnvelope struct {
	Overrides map[string]json.RawMessage `json:"overrides,omitempty"` // catalog key -> legacy body string or roleOverrideEntry
	Custom    map[string]customRoleEntry `json:"custom,omitempty"`    // custom key -> metadata/body override
}

type roleOverrideEntry struct {
	Label       string `json:"label,omitempty"` // legacy, ignored
	Description string `json:"description,omitempty"`
	Emoji       string `json:"emoji,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Body        string `json:"body"`
	Cardinality string `json:"cardinality,omitempty"`
	Extends     string `json:"extends,omitempty"`
}

type customRoleEntry struct {
	Label       string `json:"label"` // legacy, ignored
	Description string `json:"description"`
	Emoji       string `json:"emoji,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Body        string `json:"body"`
	Cardinality string `json:"cardinality,omitempty"`
	Extends     string `json:"extends,omitempty"`
}

var (
	roleCatalog    []roleCatalogEntry
	roleCatalogSet map[string]bool

	compiledRoleDefaults   map[string]string
	compiledRoleDefaultsMu sync.RWMutex
)

func init() {
	var entries []roleCatalogEntry
	if json.Unmarshal(defaultRoleCatalogJSON, &entries) == nil {
		roleCatalog = entries
		roleCatalogSet = make(map[string]bool, len(entries))
		for _, e := range entries {
			roleCatalogSet[e.Key] = true
		}
	}
}

// SetRoleDefaults registers the compiled-in default bodies. Must be called
// once before the server begins serving requests.
func SetRoleDefaults(defaults map[string]string) {
	compiledRoleDefaultsMu.Lock()
	compiledRoleDefaults = defaults
	compiledRoleDefaultsMu.Unlock()
}

func compiledRoleDefaultFor(key string) string {
	compiledRoleDefaultsMu.RLock()
	v := compiledRoleDefaults[key]
	compiledRoleDefaultsMu.RUnlock()
	return v
}

func (s *store) loadRoles() {
	if s.db != nil {
		overrides := make(map[string]roleOverrideEntry)
		custom := make(map[string]customRoleEntry)
		if err := s.loadJSONRows("role_overrides", "key", func(k string, data []byte) error {
			if !roleCatalogSet[k] {
				log.Printf("aimebu: loadRoles: dropped stale override for key %q", k)
				return nil
			}
			var entry roleOverrideEntry
			if err := json.Unmarshal(data, &entry); err != nil {
				return err
			}
			overrides[k] = entry
			return nil
		}); err == nil {
			if err := s.loadJSONRows("role_custom", "key", func(k string, data []byte) error {
				var entry customRoleEntry
				if err := json.Unmarshal(data, &entry); err != nil {
					return err
				}
				custom[k] = entry
				return nil
			}); err == nil {
				s.rolesMu.Lock()
				s.rolesOverrides = overrides
				s.rolesCustom = custom
				s.rolesMu.Unlock()
				return
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "roles.json"))
	if err != nil {
		return
	}
	var env rolesEnvelope
	if json.Unmarshal(data, &env) != nil {
		return
	}
	s.rolesMu.Lock()
	if env.Overrides != nil {
		// Drop stale overrides for keys no longer in the catalog
		for k, raw := range env.Overrides {
			if !roleCatalogSet[k] {
				log.Printf("aimebu: loadRoles: dropped stale override for key %q", k)
				continue
			}
			var body string
			if json.Unmarshal(raw, &body) == nil {
				s.rolesOverrides[k] = roleOverrideEntry{Body: body}
				continue
			}
			var entry roleOverrideEntry
			if json.Unmarshal(raw, &entry) == nil {
				if emoji, err := roleEntryEmoji(entry.Icon, entry.Emoji); err == nil {
					entry.Emoji = emoji
					entry.Icon = emoji
					if card, err := normalizeRoleCardinality(entry.Cardinality); err == nil {
						entry.Cardinality = card
					}
					s.rolesOverrides[k] = entry
				}
			}
		}
	}
	if env.Custom != nil {
		for k, v := range env.Custom {
			if emoji, err := roleEntryEmoji(v.Icon, v.Emoji); err == nil {
				v.Emoji = emoji
				v.Icon = emoji
			} else {
				v.Emoji = ""
				v.Icon = ""
			}
			if card, err := normalizeRoleCardinality(v.Cardinality); err == nil {
				v.Cardinality = card
			}
			s.rolesCustom[k] = v
		}
	}
	s.rolesMu.Unlock()
}

func (s *store) saveRoles() {
	s.rolesMu.RLock()
	if s.db != nil {
		overrides := make(map[string]any, len(s.rolesOverrides))
		for k, v := range s.rolesOverrides {
			overrides[k] = v
		}
		custom := make(map[string]any, len(s.rolesCustom))
		for k, v := range s.rolesCustom {
			custom[k] = v
		}
		s.rolesMu.RUnlock()
		if err := s.replaceJSONRows("role_overrides", "key", overrides); err != nil {
			log.Printf("aimebu: save role overrides sqlite: %v", err)
		}
		if err := s.replaceJSONRows("role_custom", "key", custom); err != nil {
			log.Printf("aimebu: save custom roles sqlite: %v", err)
		}
		return
	}
	overrides := make(map[string]roleOverrideEntry, len(s.rolesOverrides))
	for k, v := range s.rolesOverrides {
		overrides[k] = v
	}
	custom := make(map[string]customRoleEntry, len(s.rolesCustom))
	for k, v := range s.rolesCustom {
		custom[k] = v
	}
	s.rolesMu.RUnlock()

	var env struct {
		Overrides map[string]roleOverrideEntry `json:"overrides,omitempty"`
		Custom    map[string]customRoleEntry   `json:"custom,omitempty"`
	}
	if len(overrides) > 0 {
		env.Overrides = overrides
	}
	if len(custom) > 0 {
		env.Custom = custom
	}
	if data, err := json.MarshalIndent(env, "", "  "); err == nil {
		atomicWrite(filepath.Join(s.dir, "roles.json"), data)
	}
}

func compiledRoleDefaultExists(key string) bool {
	compiledRoleDefaultsMu.RLock()
	_, ok := compiledRoleDefaults[key]
	compiledRoleDefaultsMu.RUnlock()
	return ok
}

func normalizeRoleIcon(icon string) (string, error) {
	icon = strings.TrimSpace(icon)
	if icon == "" {
		return "", nil
	}
	if !utf8.ValidString(icon) {
		return "", fmt.Errorf("role emoji must be valid UTF-8")
	}
	if utf8.RuneCountInString(icon) > 16 {
		return "", fmt.Errorf("role emoji too long (max 16 characters)")
	}
	if len(icon) > 64 {
		return "", fmt.Errorf("role emoji too long (max 64 bytes)")
	}
	return icon, nil
}

func normalizeRoleEmoji(emoji string) (string, error) {
	return normalizeRoleIcon(emoji)
}

func roleEntryEmoji(icon, emoji string) (string, error) {
	if emoji != "" {
		return normalizeRoleEmoji(emoji)
	}
	return normalizeRoleEmoji(icon)
}

func normalizeRoleCardinality(v string) (string, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return "multi", nil
	}
	switch v {
	case "multi", "singleton":
		return v, nil
	default:
		return "", fmt.Errorf("role cardinality must be \"multi\" or \"singleton\"")
	}
}

func roleCatalogEntryFor(key string) (roleCatalogEntry, bool) {
	for _, e := range roleCatalog {
		if e.Key == key {
			return e, true
		}
	}
	return roleCatalogEntry{}, false
}

func (s *store) getRole(key string) (string, bool) {
	return s.getResolvedRoleBody(key)
}

func (s *store) getRawRoleBodyLocked(key string) (string, bool) {
	if v, ok := s.rolesOverrides[key]; ok {
		return v.Body, true
	}
	if e, ok := s.rolesCustom[key]; ok {
		return e.Body, true
	}
	if roleCatalogSet[key] && compiledRoleDefaultExists(key) {
		return compiledRoleDefaultFor(key), true
	}
	return "", false
}

func (s *store) roleExtendsLocked(key string) string {
	if v, ok := s.rolesOverrides[key]; ok && v.Extends != "" {
		return v.Extends
	}
	if e, ok := s.rolesCustom[key]; ok {
		return e.Extends
	}
	if e, ok := roleCatalogEntryFor(key); ok {
		return e.Extends
	}
	return ""
}

func (s *store) getResolvedRoleBody(key string) (string, bool) {
	s.rolesMu.RLock()
	defer s.rolesMu.RUnlock()
	return s.getResolvedRoleBodyLocked(key, map[string]bool{})
}

func (s *store) getResolvedRoleBodyLocked(key string, seen map[string]bool) (string, bool) {
	if seen[key] {
		return "", false
	}
	seen[key] = true
	body, ok := s.getRawRoleBodyLocked(key)
	if !ok {
		return "", false
	}
	extends := s.roleExtendsLocked(key)
	if extends == "" {
		return body, true
	}
	base, ok := s.getResolvedRoleBodyLocked(extends, seen)
	if !ok {
		return body, true
	}
	return strings.TrimSpace(base) + "\n\n" + strings.TrimSpace(body), true
}

func roleNameCollisionWarning(name string) string {
	return fmt.Sprintf("agent name %q collides with a role key; legacy exact-name mention precedence is preserved, but new collisions are rejected", name)
}

func (s *store) validateRoleExtensionCycles(entries []replaceRolesEntry) error {
	extendsByKey := make(map[string]string, len(roleCatalog)+len(entries))
	for _, entry := range roleCatalog {
		if entry.Extends != "" {
			extendsByKey[entry.Key] = entry.Extends
		}
	}

	s.rolesMu.RLock()
	for k, entry := range s.rolesOverrides {
		if entry.Extends == "" {
			delete(extendsByKey, k)
		} else {
			extendsByKey[k] = entry.Extends
		}
	}
	for k, entry := range s.rolesCustom {
		if entry.Extends == "" {
			delete(extendsByKey, k)
		} else {
			extendsByKey[k] = entry.Extends
		}
	}
	s.rolesMu.RUnlock()

	for _, entry := range entries {
		if entry.extends == "" {
			delete(extendsByKey, entry.key)
		} else {
			extendsByKey[entry.key] = entry.extends
		}
	}

	for _, entry := range entries {
		seen := map[string]bool{entry.key: true}
		for current := extendsByKey[entry.key]; current != ""; current = extendsByKey[current] {
			if seen[current] {
				return fmt.Errorf("role %q creates an extends cycle through %q", entry.key, current)
			}
			seen[current] = true
			if len(seen) > 128 {
				return fmt.Errorf("role %q extends chain is too deep", entry.key)
			}
		}
	}
	return nil
}

// getRoleLabel is retained for legacy call sites; key is now the visible role
// identity, so this always returns key.
func (s *store) getRoleLabel(key string) string {
	return key
}

func (s *store) getRoleIcon(key string) string {
	s.rolesMu.RLock()
	if e, ok := s.rolesOverrides[key]; ok {
		s.rolesMu.RUnlock()
		if e.Emoji != "" {
			return e.Emoji
		}
		if e.Icon != "" {
			return e.Icon
		}
		if catalog, ok := roleCatalogEntryFor(key); ok {
			if catalog.Emoji != "" {
				return catalog.Emoji
			}
			return catalog.Icon
		}
		return ""
	}
	if e, ok := s.rolesCustom[key]; ok {
		s.rolesMu.RUnlock()
		if e.Emoji != "" {
			return e.Emoji
		}
		return e.Icon
	}
	s.rolesMu.RUnlock()
	if e, ok := roleCatalogEntryFor(key); ok {
		if e.Emoji != "" {
			return e.Emoji
		}
		return e.Icon
	}
	return ""
}

func (s *store) getRoleCardinality(key string) string {
	s.rolesMu.RLock()
	if e, ok := s.rolesOverrides[key]; ok && e.Cardinality != "" {
		s.rolesMu.RUnlock()
		return e.Cardinality
	}
	if e, ok := s.rolesCustom[key]; ok && e.Cardinality != "" {
		s.rolesMu.RUnlock()
		return e.Cardinality
	}
	s.rolesMu.RUnlock()
	if e, ok := roleCatalogEntryFor(key); ok {
		card, err := normalizeRoleCardinality(e.Cardinality)
		if err == nil {
			return card
		}
	}
	return "multi"
}

func (s *store) roleKeyExists(key string) bool {
	return s.roleExists(key)
}

func (s *store) agentNameExists(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.agents {
		if a.Kind == "ai" && a.Name == name {
			return true
		}
	}
	return false
}

func (s *store) roleExists(key string) bool {
	if roleCatalogSet[key] {
		return true
	}
	s.rolesMu.RLock()
	_, ok := s.rolesCustom[key]
	s.rolesMu.RUnlock()
	return ok
}

// setRoleOverride sets a catalog override or adds/updates a custom role.
// Validates key format, body size, and total count. On success propagates
// to rooms where the key is currently assigned.
func (s *store) setRoleOverride(key, body string) error {
	if !roleKeyRE.MatchString(key) {
		return fmt.Errorf("invalid role key %q: must match ^[a-z][a-z0-9_-]*$", key)
	}
	if len(body) > 16*1024 {
		return fmt.Errorf("role body too large (max 16KB)")
	}
	if !roleCatalogSet[key] && s.agentNameExists(key) {
		return fmt.Errorf("role key %q collides with an active AI agent name", key)
	}
	s.rolesMu.Lock()
	// Cap applies to catalog overrides + custom roles combined. Adding a new
	// catalog entry in a future release silently shrinks the custom budget — this
	// is intentional; the total cap bounds the roles list size for UI rendering.
	total := len(roleCatalog) + len(s.rolesCustom)
	if !roleCatalogSet[key] {
		if _, exists := s.rolesCustom[key]; !exists {
			if total >= 64 {
				s.rolesMu.Unlock()
				return fmt.Errorf("too many roles (max 64)")
			}
		}
		entry := s.rolesCustom[key]
		entry.Body = body
		entry.Label = key
		if entry.Cardinality == "" {
			entry.Cardinality = "multi"
		}
		s.rolesCustom[key] = entry
	} else {
		entry := s.rolesOverrides[key]
		entry.Body = body
		s.rolesOverrides[key] = entry
	}
	s.rolesMu.Unlock()
	s.saveRoles()
	s.propagateRoleUpdate(key, body)
	return nil
}

// setCustomRole adds or updates a custom role with metadata and body.
// Used internally (tests, replaceRoles). For HTTP, callers go through setRoleOverride
// (body-only) or replaceRoles (structured).
func (s *store) setCustomRole(key, label, description, icon, body string) error {
	if !roleKeyRE.MatchString(key) {
		return fmt.Errorf("invalid role key %q", key)
	}
	if len(body) > 16*1024 {
		return fmt.Errorf("role body too large (max 16KB)")
	}
	normalizedIcon, err := normalizeRoleEmoji(icon)
	if err != nil {
		return err
	}
	if !roleCatalogSet[key] && s.agentNameExists(key) {
		return fmt.Errorf("role key %q collides with an active AI agent name", key)
	}
	s.rolesMu.Lock()
	if !roleCatalogSet[key] {
		if _, exists := s.rolesCustom[key]; !exists {
			if len(roleCatalog)+len(s.rolesCustom) >= 64 {
				s.rolesMu.Unlock()
				return fmt.Errorf("too many roles (max 64)")
			}
		}
	}
	s.rolesCustom[key] = customRoleEntry{Label: key, Description: description, Emoji: normalizedIcon, Icon: normalizedIcon, Body: body, Cardinality: "multi"}
	s.rolesMu.Unlock()
	s.saveRoles()
	s.propagateRoleUpdate(key, body)
	return nil
}

// replaceRolesEntry is used by replaceRoles to carry parsed PUT /roles payload.
type replaceRolesEntry struct {
	key, body, description, emoji, cardinality, extends string
	isCustom                                            bool
}

// ErrRolesConflict is returned by replaceRoles when a removed custom role is
// currently assigned and force is false.
var ErrRolesConflict = errors.New("roles conflict")

// replaceRoles atomically replaces all catalog overrides and custom roles.
// Validates nothing (caller pre-validated); propagates updates to affected rooms.
// Removed custom roles that are assigned return ErrRolesConflict unless force=true.
// Removed catalog overrides that are assigned propagate the default body.
func (s *store) replaceRoles(entries []replaceRolesEntry, force bool) error {
	if err := s.validateRoleExtensionCycles(entries); err != nil {
		return err
	}

	newOverrides := make(map[string]roleOverrideEntry)
	newCustom := make(map[string]customRoleEntry)
	newKeys := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.isCustom && s.agentNameExists(e.key) {
			return fmt.Errorf("role key %q collides with an active AI agent name", e.key)
		}
		if e.extends != "" && e.extends == e.key {
			return fmt.Errorf("role %q cannot extend itself", e.key)
		}
		if e.extends != "" && !s.roleExists(e.extends) && !newKeys[e.extends] {
			found := false
			for _, candidate := range entries {
				if candidate.key == e.extends {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("role %q extends unknown role %q", e.key, e.extends)
			}
		}
		newKeys[e.key] = true
		if e.isCustom {
			newCustom[e.key] = customRoleEntry{
				Label:       e.key,
				Description: e.description,
				Emoji:       e.emoji,
				Icon:        e.emoji,
				Body:        e.body,
				Cardinality: e.cardinality,
				Extends:     e.extends,
			}
		} else {
			newOverrides[e.key] = roleOverrideEntry{
				Description: e.description,
				Emoji:       e.emoji,
				Icon:        e.emoji,
				Body:        e.body,
				Cardinality: e.cardinality,
				Extends:     e.extends,
			}
		}
	}

	// Snapshot old state to detect which keys changed for propagation
	s.rolesMu.RLock()
	old := make(map[string]string, len(s.rolesOverrides)+len(s.rolesCustom))
	oldCustomKeys := make(map[string]bool, len(s.rolesCustom))
	for k, v := range s.rolesOverrides {
		old[k] = v.Body
	}
	for k, v := range s.rolesCustom {
		old[k] = v.Body
		oldCustomKeys[k] = true
	}
	s.rolesMu.RUnlock()

	// First pass: validate and handle removed custom roles (may return early with error)
	var removedCatalogOverrides []string
	for k := range old {
		if newKeys[k] {
			continue
		}
		rooms := s.roomsWithRole(k)
		if len(rooms) == 0 {
			continue
		}
		if oldCustomKeys[k] {
			if !force {
				return fmt.Errorf("%w: custom role %q is assigned in rooms: %v; use force=true to cascade-unassign", ErrRolesConflict, k, rooms)
			}
			for _, roomID := range rooms {
				_ = s.assignRole(roomID, "", k)
			}
		} else {
			// Collect catalog overrides to propagate AFTER the map swap
			removedCatalogOverrides = append(removedCatalogOverrides, k)
		}
	}

	s.rolesMu.Lock()
	s.rolesOverrides = newOverrides
	s.rolesCustom = newCustom
	s.rolesMu.Unlock()
	s.saveRoles()

	// Propagate removed catalog overrides AFTER the swap so concurrent bus_role_get
	// reads the new (default) body and not the stale override.
	for _, k := range removedCatalogOverrides {
		s.propagateRoleUpdate(k, compiledRoleDefaultFor(k))
	}

	// Propagate to rooms for keys whose body changed
	for _, e := range entries {
		if e.body != old[e.key] {
			s.propagateRoleUpdate(e.key, e.body)
		}
	}
	s.broadcastRolesUpdated()
	return nil
}

// deleteRoleOverride reverts a catalog override to default while preserving
// assignments, or deletes a custom role. Assigned custom roles return 409 with
// a room list unless force=true cascade-unassigns them first.
func (s *store) deleteRoleOverride(key string, force bool) error {
	rooms := s.roomsWithRole(key)
	if roleCatalogSet[key] {
		s.rolesMu.Lock()
		delete(s.rolesOverrides, key)
		s.rolesMu.Unlock()
		s.saveRoles()
		if len(rooms) > 0 {
			s.propagateRoleUpdate(key, compiledRoleDefaultFor(key))
		} else {
			s.broadcastRolesUpdated()
		}
		return nil
	}
	if len(rooms) > 0 && !force {
		return fmt.Errorf("role %q is assigned in rooms: %v; use force=true to cascade-unassign", key, rooms)
	}
	if force && len(rooms) > 0 {
		// Cascade-unassign: pass empty agentID to clear all agents with this role
		for _, roomID := range rooms {
			_ = s.assignRole(roomID, "", key)
		}
	}
	s.rolesMu.Lock()
	delete(s.rolesOverrides, key)
	delete(s.rolesCustom, key)
	s.rolesMu.Unlock()
	s.saveRoles()
	s.broadcastRolesUpdated()
	return nil
}

func (s *store) deleteAllRoleOverrides(force bool) error {
	// Collect all rooms with assignments
	if !force {
		s.mu.RLock()
		var assigned []string
		for _, room := range s.rooms {
			if len(room.Roles) > 0 {
				assigned = append(assigned, room.ID)
			}
		}
		s.mu.RUnlock()
		if len(assigned) > 0 {
			return fmt.Errorf("roles are assigned in rooms: %v; use force=true to cascade-unassign", assigned)
		}
	}
	if force {
		// Clear all role assignments across all rooms
		s.mu.Lock()
		for _, room := range s.rooms {
			room.Roles = nil
		}
		s.persist()
		s.mu.Unlock()
		s.broadcastRoomUpdate()
	}
	s.rolesMu.Lock()
	s.rolesOverrides = make(map[string]roleOverrideEntry)
	s.rolesCustom = make(map[string]customRoleEntry)
	s.rolesMu.Unlock()
	s.saveRoles()
	s.broadcastRolesUpdated()
	return nil
}

func (s *store) listRoles() []RoleEntry {
	s.rolesMu.RLock()
	overrides := make(map[string]roleOverrideEntry, len(s.rolesOverrides))
	for k, v := range s.rolesOverrides {
		overrides[k] = v
	}
	custom := make(map[string]customRoleEntry, len(s.rolesCustom))
	for k, v := range s.rolesCustom {
		custom[k] = v
	}
	s.rolesMu.RUnlock()

	out := make([]RoleEntry, 0, len(roleCatalog)+len(custom))
	for _, e := range roleCatalog {
		override, overridden := overrides[e.Key]
		body := override.Body
		if !overridden {
			body = compiledRoleDefaultFor(e.Key)
		}
		description := e.Description
		icon := e.Icon
		if e.Emoji != "" {
			icon = e.Emoji
		}
		cardinality, _ := normalizeRoleCardinality(e.Cardinality)
		extends := e.Extends
		if overridden {
			if override.Description != "" {
				description = override.Description
			}
			if override.Emoji != "" {
				icon = override.Emoji
			} else if override.Icon != "" {
				icon = override.Icon
			}
			if override.Cardinality != "" {
				cardinality = override.Cardinality
			}
			if override.Extends != "" {
				extends = override.Extends
			}
		}
		resolved, _ := s.getResolvedRoleBody(e.Key)
		out = append(out, RoleEntry{
			Key:          e.Key,
			Label:        e.Key,
			Description:  description,
			Emoji:        icon,
			Icon:         icon,
			Body:         body,
			ResolvedBody: resolved,
			DefaultBody:  compiledRoleDefaultFor(e.Key),
			Cardinality:  cardinality,
			Extends:      extends,
			Overridden:   overridden,
			IsCustom:     false,
		})
	}
	// Custom roles sorted alphabetically
	customKeys := make([]string, 0, len(custom))
	for k := range custom {
		customKeys = append(customKeys, k)
	}
	sort.Strings(customKeys)
	for _, k := range customKeys {
		e := custom[k]
		emoji := e.Icon
		if e.Emoji != "" {
			emoji = e.Emoji
		}
		out = append(out, RoleEntry{
			Key:         k,
			Label:       k,
			Description: e.Description,
			Emoji:       emoji,
			Icon:        emoji,
			Body:        e.Body,
			ResolvedBody: func() string {
				body, _ := s.getResolvedRoleBody(k)
				return body
			}(),
			DefaultBody: "",
			Cardinality: func() string {
				card, err := normalizeRoleCardinality(e.Cardinality)
				if err != nil {
					return "multi"
				}
				return card
			}(),
			Extends:    e.Extends,
			Overridden: false,
			IsCustom:   true,
		})
	}
	return out
}

// clearRoles wipes all overrides and custom roles (used by prune -a).
func (s *store) clearRoles() {
	s.rolesMu.Lock()
	s.rolesOverrides = make(map[string]roleOverrideEntry)
	s.rolesCustom = make(map[string]customRoleEntry)
	if s.db != nil {
		if err := s.clearTable("role_overrides"); err != nil {
			log.Printf("aimebu: clear role overrides sqlite: %v", err)
		}
		if err := s.clearTable("role_custom"); err != nil {
			log.Printf("aimebu: clear custom roles sqlite: %v", err)
		}
		s.rolesMu.Unlock()
		return
	}
	s.rolesMu.Unlock()
	_ = os.Remove(filepath.Join(s.dir, "roles.json"))
}

// roomsWithRole returns IDs of rooms where at least one agent has this roleKey assigned.
func (s *store) roomsWithRole(roleKey string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []string
	for id, room := range s.rooms {
		for _, rk := range room.Roles {
			if rk == roleKey {
				result = append(result, id)
				break
			}
		}
	}
	return result
}

// assignRole assigns or unassigns a role for an agent in a room.
// If agentID is empty, clears all agents with roleKey (internal cascade).
// If roleKey is empty, unassigns the agent's role.
func (s *store) assignRole(roomID, agentID, roleKey string) error {
	if roomID == "" {
		return fmt.Errorf("room ID required")
	}
	roleCardinality := ""
	if roleKey != "" {
		roleCardinality = s.getRoleCardinality(roleKey)
	}

	s.mu.Lock()
	room, ok := s.rooms[roomID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrRoomNotFound, roomID)
	}
	if agentID != "" && roleKey != "" {
		// Validate agent is a member
		isMember := false
		for _, m := range room.Members {
			if m == agentID {
				isMember = true
				break
			}
		}
		if !isMember {
			s.mu.Unlock()
			return fmt.Errorf("agent %s is not a member of room %s", agentID, roomID)
		}
	}

	if room.Roles == nil {
		room.Roles = make(map[string]string)
	}

	if agentID == "" {
		// Clear all agents with this role key (used internally by deleteRoleOverride force)
		for aid, rk := range room.Roles {
			if rk == roleKey {
				delete(room.Roles, aid)
			}
		}
	} else if roleKey == "" {
		delete(room.Roles, agentID)
	} else {
		agent, ok := s.agents[agentID]
		if !ok {
			s.mu.Unlock()
			return fmt.Errorf("agent %s is not registered", agentID)
		}
		if agent.Kind == "human" {
			s.mu.Unlock()
			return fmt.Errorf("roles can only be assigned to AI agents")
		}
		if roleCardinality == "singleton" {
			for holderID, assignedKey := range room.Roles {
				if assignedKey == roleKey && holderID != agentID {
					s.mu.Unlock()
					return fmt.Errorf("%w: role %q is already assigned to %s", ErrRoleAssignmentConflict, roleKey, holderID)
				}
			}
		}
		room.Roles[agentID] = roleKey
	}

	s.persist()
	s.mu.Unlock()
	s.broadcastRoomUpdate()

	if agentID == "" {
		return nil // internal cascade, no system message
	}

	if roleKey == "" {
		s.emitSystemMessageTo(roomID, agentID+" role cleared", []string{agentShortName(agentID)})
	} else {
		label := s.getRoleLabel(roleKey)
		if label == "" {
			label = roleKey
		}
		s.emitSystemMessageTo(roomID, agentID+" was assigned as "+label, []string{agentShortName(agentID)})
	}
	return nil
}

// propagateRoleUpdate emits a system message to all rooms where roleKey is
// currently assigned, then broadcasts roles_updated to WS clients.
func (s *store) propagateRoleUpdate(roleKey, body string) {
	rooms := s.roomsWithRole(roleKey)
	for _, roomID := range rooms {
		var msgBody string
		if body != "" {
			msgBody = "role " + roleKey + " updated\n\n" + body
		} else {
			msgBody = "role " + roleKey + " updated"
		}
		s.emitSystemMessage(roomID, msgBody)
	}
	s.broadcastRolesUpdated()
}

// broadcastRolesUpdated notifies WS meta subscribers that roles changed.
func (s *store) broadcastRolesUpdated() {
	evt := MetaEvent{Type: "roles_updated", Data: map[string]any{}}
	s.subMu.Lock()
	for _, ch := range s.metaSubs {
		select {
		case ch <- evt:
		default:
		}
	}
	s.subMu.Unlock()
}

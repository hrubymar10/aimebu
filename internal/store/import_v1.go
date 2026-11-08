package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/sqliteutil"
	_ "modernc.org/sqlite"
)

// V1ImportMap deliberately requires a project and user choice. An imported
// non-DM room is visible to every member of its destination project.
type V1ImportMap struct {
	SourceKey      string                     `json:"source_key"`
	OwnerLogin     string                     `json:"owner_login"`
	DefaultProject string                     `json:"default_project"`
	Projects       map[string]V1ImportProject `json:"projects"`
	Rooms          map[string]V1ImportRoom    `json:"rooms"`
	Humans         map[string]string          `json:"humans"`
	MemoryProjects map[string]string          `json:"memory_projects"`
	ProjectMembers map[string][]string        `json:"project_members,omitempty"`
	SkipProfiles   bool                       `json:"skip_profiles,omitempty"`
	SkipShared     bool                       `json:"skip_shared_notes,omitempty"`
	MacroConflicts map[string]string          `json:"macro_conflicts,omitempty"`
	// SkipMacros are macro keys the owner chose not to bring across. A skipped macro is not recorded in the ledger, so a later run can
	// still import it.
	SkipMacros []string `json:"skip_macros,omitempty"`
	// NewUsers are v1 people who get a new v2 account made by the import (see import_v1_accounts.go), keyed by the v1 name; Humans
	// maps the same name to the new login.
	NewUsers            map[string]V1NewUser `json:"new_users,omitempty"`
	EnableProjectMemory map[string]bool      `json:"enable_project_memory,omitempty"`
	RoleConflicts       map[string]string    `json:"role_conflicts,omitempty"`
	// Teams are gone. A draft saved before that may still carry these keys; they are accepted and ignored.
	LegacyTeamChoices   json.RawMessage `json:"team_choices,omitempty"`
	LegacyTeamConflicts json.RawMessage `json:"team_conflicts,omitempty"`
	Wizard              bool            `json:"-"`
}

type V1ImportProject struct {
	Name       string `json:"name"`
	Folder     string `json:"folder"`
	ExistingID string `json:"existing_id,omitempty"`
}

type V1ImportRoom struct {
	Project   string `json:"project"`
	Lifecycle string `json:"lifecycle,omitempty"`
	Skip      bool   `json:"skip,omitempty"`
	// Target is where the room goes (see import_v1_targets.go): "room" or empty, "root", or "sub"; Title names a "sub" target.
	Target string `json:"target,omitempty"`
	Title  string `json:"title,omitempty"`
}

func skippedV1Room(mapping V1ImportMap, id string) bool  { return mapping.Rooms[id].Skip }
func skippedV1Human(mapping V1ImportMap, id string) bool { return mapping.Humans[id] == "-" }
func v1MemorySkipReason(mapping V1ImportMap, memory v1ImportMemory) string {
	switch memory.Scope {
	case "project_facts":
		if mapping.MemoryProjects[memory.ScopeKey] == "-" {
			return "project facts scope was skipped"
		}
	case "user_profile":
		if mapping.SkipProfiles {
			return "global user profiles were skipped"
		}
		if skippedV1Human(mapping, memory.ScopeKey) {
			return "profile owner was skipped"
		}
	case "agent_shared_notes":
		if mapping.SkipShared {
			return "shared agent notes were skipped"
		}
	}
	return ""
}

func v1MemoryCapError(scope, key string, cap int, wizard bool) error {
	if wizard && scope == "user_profile" {
		return fmt.Errorf("profiles mapped to %q exceed the %d-byte user profile cap; choose \"Skip global user profiles\" in Memory or map people to separate v2 users", key, cap)
	}
	return fmt.Errorf("memory scope %q exceeds %d-byte cap", scope+":"+key, cap)
}

// v1PresenceLine is one of the bookkeeping lines v1's server wrote into rooms in its own name: an agent coming or going ("x joined",
// "x left", "x was kicked", "x reconnected", "x pruned after stale timeout", "x registered (...)", "x deregistered", a role vacated because
// its holder deregistered) and memory housekeeping ("x added memory scope/key [id]", "x updated memory [id]", "x removed memory [id]",
// "memory feed enabled"). They are not conversation, and v2 keeps presence and memory changes elsewhere, so they are not imported (like
// v1's _system room). v1 wrote them as the author "_system" with kind "system"; only that author's own wording is matched, so a person
// typing "bob joined" is untouched, and v1's other system lines (role assignments) are kept.
var v1PresenceLine = regexp.MustCompile(`^(\S+ (joined|left|was kicked|reconnected|deregistered|pruned after stale timeout|pruned \(stale\)|registered \(.*\)|added memory \S+ \[\w+\]|updated memory \[\w+\]|removed memory \[\w+\])|role ".*" vacated: \S+ deregistered|memory feed (enabled|disabled|reset to default))$`)

// isV1SystemAuthor says whether v1 wrote the message as itself: kind "system", or its author _system or system on a backup that has no kind.
func isV1SystemAuthor(message v1ImportMessage) bool {
	return message.FromKind == "system" || message.From == "_system" || message.From == "system"
}

func isV1PresenceLine(message v1ImportMessage) bool {
	return isV1SystemAuthor(message) && v1PresenceLine.MatchString(strings.TrimSpace(message.Body))
}

func skippedV1HumanInRoom(data *v1ImportData, mapping V1ImportMap, room v1ImportRoom) bool {
	for _, member := range room.Members {
		if v1ImportKind(member, data.Agents) == "user" && skippedV1Human(mapping, member) {
			return true
		}
	}
	for _, message := range data.Messages {
		if message.RoomID == room.ID && v1ImportMessageKind(message, data.Agents) == "user" && skippedV1Human(mapping, message.From) {
			return true
		}
	}
	return false
}

type V1ImportReport struct {
	DryRun                   bool               `json:"dry_run"`
	MemoryEnabled            bool               `json:"memory_enabled"`
	Imported                 map[string]int     `json:"imported"`
	AlreadyPresent           map[string]int     `json:"already_present"`
	Skipped                  map[string]int     `json:"skipped"`
	ProjectReaders           map[string]int     `json:"project_readers"`
	ProjectAccessGaps        map[string]int     `json:"project_access_gaps"`
	UnreadableDMParticipants map[string]int     `json:"unreadable_dm_participants"`
	ExistingOwnerGrants      map[string]int     `json:"existing_project_owner_grants"`
	GrantedViewers           map[string]int     `json:"granted_viewers"`
	MacroConflicts           map[string]string  `json:"macro_conflicts"`
	RoleConflicts            map[string]string  `json:"role_conflicts"`
	SkippedSettings          []V1SkippedSetting `json:"skipped_settings"`
	SkippedMemory            []V1SkippedMemory  `json:"skipped_memory"`
	SkippedRoles             map[string]string  `json:"skipped_roles"`
	// OpenedDMs are v1 DMs whose mapping asked for the room default (archived) but which import open, because v2 has no archive for a DM.
	OpenedDMs []string `json:"opened_dms,omitempty"`
	// DMOnlyAccess lists each new account that gets access to a project only because a direct message of theirs goes into it.
	DMOnlyAccess []string `json:"dm_only_access,omitempty"`
	// SkippedNoProject are the v1 rooms and direct messages that have no destination project, so they are not imported.
	SkippedNoProject []string `json:"skipped_no_project,omitempty"`
	// NewAccounts are the accounts the import made for v1 people (an invite link for each is created after the import commits).
	NewAccounts []V1NewAccount `json:"new_accounts,omitempty"`
	// TargetWarnings name the rooms that more than one v1 room was placed in (the project root or a shared sub-room).
	TargetWarnings []string `json:"target_warnings,omitempty"`
	// MemoryOffTargets name the shared rooms (a project's root, a shared sub-room) whose memory is off because a v1 room merged into
	// them had it off.
	MemoryOffTargets []string `json:"memory_off_targets,omitempty"`
	// FlattenedRoles are roles whose link to a parent had to be flattened because the parent changed under the chosen conflict
	// resolutions and the link would have closed a loop or run too deep (see importV1Roles).
	FlattenedRoles []string          `json:"flattened_roles,omitempty"`
	SkippedSounds  map[string]string `json:"skipped_sounds"`
	NotCarried     map[string]int    `json:"not_carried"`
	// Accounting reconciles the backup's room, message, attachment and reaction totals to what the import does with them, a reason per part.
	Accounting       []V1AccountingGroup `json:"accounting"`
	CarriedOverFiles []string            `json:"carried_over_files,omitempty"`
}

type V1SkippedMemory struct {
	ID       string `json:"id"`
	Scope    string `json:"scope"`
	ScopeKey string `json:"scope_key"`
	Reason   string `json:"reason"`
}

type v1ImportRoom struct {
	ID            string   `json:"id"`
	Members       []string `json:"members"`
	CreatedAt     string   `json:"created_at"`
	CreatedBy     string   `json:"created_by"`
	MemoryEnabled *bool    `json:"memory_enabled,omitempty"`
	// Roles is the role v1 gave each agent in the room, by agent id (leader, worker, ...).
	Roles map[string]string `json:"roles,omitempty"`
}

type v1ImportRole struct {
	Key         string `json:"key"`
	Description string `json:"description"`
	Emoji       string `json:"emoji"`
	Icon        string `json:"icon"`
	Body        string `json:"body"`
	Cardinality string `json:"cardinality"`
	Extends     string `json:"extends"`
	Custom      bool   `json:"-"`
}

type v1ImportMessage struct {
	ID              int64                   `json:"id"`
	RoomID          string                  `json:"room_id"`
	From            string                  `json:"from"`
	FromKind        string                  `json:"from_kind"`
	FromHarness     string                  `json:"from_harness"`
	FromModel       string                  `json:"from_model"`
	FromSession     string                  `json:"from_session"`
	Body            string                  `json:"body"`
	CreatedAt       string                  `json:"created_at"`
	ReplyTo         int64                   `json:"reply_to"`
	Targets         []string                `json:"targets"`
	NeedsAttention  bool                    `json:"needs_human_attention"`
	ProposedAnswers []string                `json:"proposed_answers"`
	OpenQuestions   json.RawMessage         `json:"open_questions"`
	VisualPlan      json.RawMessage         `json:"visual_plan"`
	AppendixPages   json.RawMessage         `json:"appendix_pages"`
	Attachments     []v1ImportAttachmentRef `json:"attachments"`
	Raw             json.RawMessage         `json:"-"`
}

type v1ImportAttachmentRef struct {
	ID string `json:"id"`
}

type v1ImportAttachment struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Mime       string          `json:"mime"`
	Size       int64           `json:"size"`
	UploadedAt string          `json:"uploaded_at"`
	Ext        string          `json:"ext"`
	Width      int             `json:"width"`
	Height     int             `json:"height"`
	Raw        json.RawMessage `json:"-"`
}

type v1ImportMemory struct {
	ID              string          `json:"id"`
	Scope           string          `json:"scope"`
	ScopeKey        string          `json:"scope_key"`
	Body            string          `json:"body"`
	Version         int64           `json:"version"`
	Author          string          `json:"author"`
	SourceMessageID int64           `json:"source_message_id"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
	Raw             json.RawMessage `json:"-"`
}

type v1ImportAgent struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Harness string `json:"harness"`
	Model   string `json:"model"`
	// ReadCursors is v1's highest read message ID per room. Only humans'
	// cursors are imported; an agent's cursor is delivery state.
	ReadCursors map[string]int64 `json:"read_cursors,omitempty"`
}

type v1ImportReaction struct {
	AgentID   string `json:"agent_id"`
	Emoji     string `json:"emoji"`
	CreatedAt string `json:"created_at"`
}

type v1ImportData struct {
	Rooms                   []v1ImportRoom
	Messages                []v1ImportMessage
	Agents                  map[string]v1ImportAgent
	Reactions               map[int64][]v1ImportReaction
	Memory                  []v1ImportMemory
	Attachments             map[string]v1ImportAttachment
	Macros                  map[string]string
	MacroSeenDefault        map[string]bool
	RoomRaw                 map[string]string
	Settings                map[string]json.RawMessage
	SkippedSourceSettings   []V1SkippedSetting
	Roles                   map[string]v1ImportRole
	SkippedSourceRoles      map[string]string
	PromptOverrideCount     int
	UnresolvedMemorySources int
	Sounds                  []v1ImportSound
	RoomPrefs               []v1RoomPref
	AgentSessions           []v1AgentSession
	AgentSessionHistory     []v1AgentSessionHistory
	NotCarried              map[string]int
}

// ReserveV1MessageIDs keeps first-run setup events above the history numbers
// that people already use as #references in v1. Call before owner bootstrap.
// The counter is always raised to the highest v1 number, so numbers handed out
// from now on cannot collide with history; it reports whether earlier activity
// already sits at or below that number (the import then refuses rather than
// renumber).
func (s *Store) ReserveV1MessageIDs(ctx context.Context, sourceDir string) (overlap bool, err error) {
	data, err := OpenV1ImportSource(ctx, sourceDir)
	if err != nil {
		return false, err
	}
	var highest int64
	for _, message := range data.Messages {
		if message.ID > highest {
			highest = message.ID
		}
	}
	if highest == 0 {
		return false, nil
	}
	err = s.Tx(ctx, func(tx *Tx) error {
		if err := tx.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE seq<=?) OR EXISTS(SELECT 1 FROM message WHERE seq<=?) OR EXISTS(SELECT 1 FROM message WHERE no>0 AND no<=?)`, highest, highest, highest).Scan(&overlap); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE seq_counter SET value=MAX(value,?) WHERE id=1`, highest); err != nil {
			return err
		}
		// Message numbers too: a v1 message keeps its v1 id as its number, so native messages continue after the highest.
		_, err := tx.Exec(`UPDATE message_no_counter SET value=MAX(value,?) WHERE id=1`, highest)
		return err
	})
	return overlap, err
}

func ReadV1ImportMap(path string) (V1ImportMap, error) {
	var mapping V1ImportMap
	file, err := os.Open(path)
	if err != nil {
		return mapping, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&mapping); err != nil {
		return mapping, fmt.Errorf("parse import map: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return mapping, errors.New("import map contains trailing JSON")
	}
	if mapping.SourceKey == "" || mapping.OwnerLogin == "" {
		return mapping, errors.New("source_key and owner_login are required")
	}
	return mapping, nil
}

// OpenV1ImportSource reads only a caller-supplied copy. It never derives a
// source path from HOME or the v1 default configuration location.
func OpenV1ImportSource(ctx context.Context, dir string) (*v1ImportData, error) {
	path := filepath.Join(dir, "server", "aimebu.sqlite")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		path = filepath.Join(dir, "aimebu.sqlite")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, true))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var integrity string
	if err := sqliteutil.Retry(func() error { return db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity) }); err != nil {
		return nil, err
	}
	if integrity != "ok" {
		return nil, fmt.Errorf("v1 source integrity: %s", integrity)
	}
	var version int
	if err := db.QueryRowContext(ctx, "SELECT version FROM schema_meta LIMIT 1").Scan(&version); err != nil || version != 1 {
		return nil, fmt.Errorf("expected v1 SQLite schema 1, got %d: %v", version, err)
	}
	data := &v1ImportData{Agents: map[string]v1ImportAgent{}, Reactions: map[int64][]v1ImportReaction{}, Attachments: map[string]v1ImportAttachment{}, Macros: map[string]string{}, MacroSeenDefault: map[string]bool{}, RoomRaw: map[string]string{}, Settings: map[string]json.RawMessage{}, Roles: map[string]v1ImportRole{}, SkippedSourceRoles: map[string]string{}}
	load := func(query string, visit func(string, string) error) error {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key, raw string
			if err := rows.Scan(&key, &raw); err != nil {
				return err
			}
			if err := visit(key, raw); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := load("SELECT id,data FROM rooms ORDER BY id", func(id, raw string) error {
		var room v1ImportRoom
		if err := json.Unmarshal([]byte(raw), &room); err != nil {
			return fmt.Errorf("room %s: %w", id, err)
		}
		if room.ID != id {
			return fmt.Errorf("room %s: JSON ID mismatch", id)
		}
		data.Rooms = append(data.Rooms, room)
		data.RoomRaw[id] = raw
		return nil
	}); err != nil {
		return nil, err
	}
	if err := load("SELECT CAST(id AS TEXT),data FROM messages ORDER BY id", func(id, raw string) error {
		var message v1ImportMessage
		if err := json.Unmarshal([]byte(raw), &message); err != nil {
			return fmt.Errorf("message %s: %w", id, err)
		}
		if strconv.FormatInt(message.ID, 10) != id {
			return fmt.Errorf("message %s: JSON ID mismatch", id)
		}
		message.Raw = json.RawMessage(raw)
		data.Messages = append(data.Messages, message)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := load("SELECT id,data FROM agents ORDER BY id", func(id, raw string) error {
		var agent v1ImportAgent
		if err := json.Unmarshal([]byte(raw), &agent); err != nil {
			return err
		}
		data.Agents[id] = agent
		return nil
	}); err != nil {
		return nil, err
	}
	sounds, err := loadV1Sounds(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("v1 sounds: %w", err)
	}
	data.Sounds = sounds
	if data.RoomPrefs, err = loadV1RoomPrefs(ctx, db); err != nil {
		return nil, fmt.Errorf("v1 room preferences: %w", err)
	}
	if data.AgentSessions, err = loadV1AgentSessions(ctx, db); err != nil {
		return nil, fmt.Errorf("v1 agent sessions: %w", err)
	}
	if data.AgentSessionHistory, err = loadV1AgentSessionHistory(ctx, db); err != nil {
		return nil, fmt.Errorf("v1 agent session history: %w", err)
	}
	if err := load("SELECT CAST(message_id AS TEXT),data FROM reactions ORDER BY message_id", func(id, raw string) error {
		var reactions []v1ImportReaction
		if err := json.Unmarshal([]byte(raw), &reactions); err != nil {
			return err
		}
		number, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return err
		}
		data.Reactions[number] = reactions
		return nil
	}); err != nil {
		return nil, err
	}
	if err := load("SELECT id,data FROM memory ORDER BY id", func(id, raw string) error {
		var item v1ImportMemory
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			return err
		}
		if item.ID != id {
			return fmt.Errorf("memory %s: JSON ID mismatch", id)
		}
		item.Raw = json.RawMessage(raw)
		data.Memory = append(data.Memory, item)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := load("SELECT id,data FROM attachments ORDER BY id", func(id, raw string) error {
		var item v1ImportAttachment
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			return err
		}
		if item.ID != id {
			return fmt.Errorf("attachment %s: JSON ID mismatch", id)
		}
		item.Raw = json.RawMessage(raw)
		data.Attachments[id] = item
		return nil
	}); err != nil {
		return nil, err
	}
	var hasMacros bool
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='macros'`).Scan(&hasMacros); err != nil {
		return nil, err
	}
	if hasMacros {
		rows, err := db.QueryContext(ctx, `SELECT key,value,seen_default FROM macros ORDER BY key`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key, body string
			var seenDefault bool
			if err := rows.Scan(&key, &body, &seenDefault); err != nil {
				rows.Close()
				return nil, err
			}
			data.Macros[key] = body
			data.MacroSeenDefault[key] = seenDefault
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	var hasSettings bool
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='settings'").Scan(&hasSettings); err != nil {
		return nil, err
	}
	if hasSettings {
		var settingsRaw string
		if err := db.QueryRowContext(ctx, "SELECT data FROM settings WHERE id=1").Scan(&settingsRaw); err == nil {
			if err := json.Unmarshal([]byte(settingsRaw), &data.Settings); err != nil {
				data.SkippedSourceSettings = append(data.SkippedSourceSettings, V1SkippedSetting{"settings", "invalid v1 JSON"})
				data.Settings = map[string]json.RawMessage{}
			} else if raw, ok := data.Settings["memory_enabled"]; ok && string(raw) != "null" {
				var enabled bool
				if err := json.Unmarshal(raw, &enabled); err != nil {
					data.SkippedSourceSettings = append(data.SkippedSourceSettings, V1SkippedSetting{"memory_enabled", "invalid v1 boolean"})
					delete(data.Settings, "memory_enabled")
				}
			}
			if raw, ok := data.Settings[InlinePlanAppendixKey]; ok && string(raw) != "null" {
				var mode string
				if json.Unmarshal(raw, &mode) != nil || mode != InlinePlanAlways && mode != InlinePlanOptional {
					data.SkippedSourceSettings = append(data.SkippedSourceSettings, V1SkippedSetting{InlinePlanAppendixKey, "invalid v1 value"})
					delete(data.Settings, InlinePlanAppendixKey)
				}
			}
			if raw, ok := data.Settings[v1ExpiryKey]; ok && string(raw) != "null" {
				if _, err := v1ExpirySeconds(raw); err != nil {
					data.SkippedSourceSettings = append(data.SkippedSourceSettings, V1SkippedSetting{v1ExpiryKey, "invalid v1 value"})
					delete(data.Settings, v1ExpiryKey)
				}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	for _, table := range []string{"role_overrides", "role_custom"} {
		var exists bool
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		if err := load("SELECT key,data FROM "+table+" ORDER BY key", func(key, raw string) error {
			var role v1ImportRole
			if err := json.Unmarshal([]byte(raw), &role); err != nil {
				if json.Unmarshal([]byte(raw), &role.Body) != nil {
					data.SkippedSourceRoles[key] = "invalid v1 JSON"
					return nil
				}
			}
			role.Key, role.Custom = key, table == "role_custom"
			data.Roles[key] = role
			return nil
		}); err != nil {
			return nil, err
		}
	}
	var hasPrompts bool
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='prompts'").Scan(&hasPrompts); err != nil {
		return nil, err
	}
	if hasPrompts {
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM prompts").Scan(&data.PromptOverrideCount); err != nil {
			return nil, err
		}
	}
	if data.NotCarried, err = v1NotCarriedTables(ctx, db); err != nil {
		return nil, err
	}
	for _, message := range data.Messages {
		if message.RoomID != V1SystemRoom && isV1PresenceLine(message) {
			data.NotCarried["presence_lines"]++
		}
	}
	knownMessageFields := map[string]bool{
		"id": true, "room_id": true, "from": true, "from_kind": true, "from_harness": true,
		"from_model": true, "from_session": true, "body": true, "created_at": true, "reply_to": true, "targets": true,
		"needs_human_attention": true, "proposed_answers": true, "open_questions": true,
		"visual_plan": true, "appendix_pages": true, "attachments": true, "reactions": true,
	}
	for _, message := range data.Messages {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(message.Raw, &fields); err != nil {
			return nil, err
		}
		for field := range fields {
			if !knownMessageFields[field] {
				data.NotCarried["messages."+field]++
			}
		}
	}
	countUnknown := func(table string, known map[string]bool, raw json.RawMessage) error {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		for field := range fields {
			if !known[field] {
				data.NotCarried[table+"."+field]++
			}
		}
		return nil
	}
	knownMemoryFields := map[string]bool{
		"id": true, "scope": true, "scope_key": true, "body": true, "version": true, "author": true,
		"source_message_id": true, "created_at": true, "updated_at": true,
	}
	for _, memory := range data.Memory {
		if err := countUnknown("memory", knownMemoryFields, memory.Raw); err != nil {
			return nil, err
		}
	}
	knownAttachmentFields := map[string]bool{"id": true, "name": true, "mime": true, "size": true, "uploaded_at": true, "ext": true, "width": true, "height": true}
	for _, attachment := range data.Attachments {
		if err := countUnknown("attachments", knownAttachmentFields, attachment.Raw); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// v1ImportedTables are the v1 tables the importer reads or deliberately handles (prompts become skipped
// settings, room and message JSON carries the rest). The fleets table is not among them: v2 has no teams,
// so it is listed as data v2 does not import.
// Any other non-empty table is data v2 does not import, and is reported rather than dropped silently.
var v1ImportedTables = map[string]bool{
	"schema_meta": true, "rooms": true, "messages": true, "agents": true, "reactions": true, "memory": true,
	"attachments": true, "macros": true, "settings": true, "role_overrides": true, "role_custom": true,
	"prompts": true, "sounds": true, "room_prefs": true,
	"agent_sessions": true, "agent_session_history": true,
}

// v1NotCarriedTables walks the source schema and counts the rows of every table the importer does
// not carry. Empty tables are left out: nothing is lost. Table names come from the file, so they
// are quoted before use.
func v1NotCarriedTables(ctx context.Context, db *sql.DB) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		if !v1ImportedTables[name] {
			names = append(names, name)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, name := range names {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+strings.ReplaceAll(name, `"`, `""`)+`"`).Scan(&count); err != nil {
			return nil, err
		}
		if count > 0 {
			counts[name] = count
		}
	}
	return counts, nil
}

func v1ImportTimestamp(raw string) (string, error) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return "", err
	}
	return t.UTC().Format(timestampLayout), nil
}

func v1ImportDigest(mapping V1ImportMap) string {
	data, _ := json.Marshal(mapping)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func v1ImportKind(id string, agents map[string]v1ImportAgent) string {
	if id == "system" {
		return "system"
	}
	if agent, ok := agents[id]; ok {
		if agent.Kind == "human" {
			return "user"
		}
		if agent.Kind == "ai" {
			return "agent"
		}
	}
	if !strings.Contains(id, "@") {
		return "user"
	}
	return "agent"
}

func v1ImportMessageKind(message v1ImportMessage, agents map[string]v1ImportAgent) string {
	switch message.FromKind {
	case "ai":
		return "agent"
	case "human":
		return "user"
	case "system":
		return "system"
	default:
		return v1ImportKind(message.From, agents)
	}
}

func v1ImportFile(sourceDir string, attachment v1ImportAttachment) string {
	ext := attachment.Ext
	if ext == "" {
		switch attachment.Mime {
		case "image/png":
			ext = "png"
		case "image/jpeg":
			ext = "jpg"
		case "image/gif":
			ext = "gif"
		case "image/webp":
			ext = "webp"
		default:
			ext = "bin"
		}
	}
	return filepath.Join(v1ImportServerRoot(sourceDir), "attachments", attachment.ID+"."+ext)
}

// v1ImportServerRoot is the directory holding a v1 backup's aimebu.sqlite and
// its attachments/ and sounds/ directories: <source>/server when the source is
// a whole v1 root, otherwise the source itself (the flat layout).
func v1ImportServerRoot(sourceDir string) string {
	root := filepath.Join(sourceDir, "server")
	if filepath.Base(sourceDir) == "server" {
		return sourceDir
	}
	if _, err := os.Stat(filepath.Join(root, "aimebu.sqlite")); os.IsNotExist(err) {
		return sourceDir
	}
	return root
}

func v1ImportHash(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func v1ImportMIME(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var header [512]byte
	n, err := io.ReadFull(file, header[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	return http.DetectContentType(header[:n]), nil
}

func sortedImportKeys[V any](items map[string]V) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// rawJSONOrNil stores an absent or null v1 field as NULL rather than the text "null".
func rawJSONOrNil(raw json.RawMessage) any {
	if value := strings.TrimSpace(string(raw)); value != "" && value != "null" {
		return value
	}
	return nil
}

type v1AttachmentCheck struct {
	Entry v1ImportAttachment
	Path  string
	Hash  string
}

func validateV1Import(data *v1ImportData, sourceDir string, mapping V1ImportMap) (map[string]v1AttachmentCheck, error) {
	if err := ValidateMacros(data.Macros); err != nil {
		return nil, fmt.Errorf("v1 macros: %w", err)
	}
	for key, choice := range mapping.MacroConflicts {
		if _, ok := data.Macros[key]; !ok {
			return nil, fmt.Errorf("macro conflict choice names unknown key %q", key)
		}
		if !validMacroChoice(choice) {
			return nil, fmt.Errorf("macro %q: %w", key, ErrMacroConflictChoice)
		}
	}
	for key, choice := range mapping.RoleConflicts {
		if _, ok := data.Roles[key]; !ok {
			return nil, fmt.Errorf("role conflict choice names unknown key %q", key)
		}
		if choice != "keep_v2" && choice != "use_v1" {
			return nil, fmt.Errorf("role %q has invalid conflict choice %q", key, choice)
		}
	}
	if mapping.SourceKey == "" || mapping.OwnerLogin == "" {
		return nil, errors.New("source_key and owner_login are required")
	}
	if mapping.DefaultProject != "" {
		if _, ok := mapping.Projects[mapping.DefaultProject]; !ok {
			return nil, fmt.Errorf("unknown default project %q", mapping.DefaultProject)
		}
	}
	for alias, project := range mapping.Projects {
		if project.ExistingID == "" && (project.Name == "" || !filepath.IsAbs(project.Folder)) {
			return nil, fmt.Errorf("project %q requires a name and absolute folder", alias)
		}
	}
	for alias := range mapping.EnableProjectMemory {
		if _, ok := mapping.Projects[alias]; !ok {
			return nil, fmt.Errorf("enable_project_memory names unknown project %q", alias)
		}
	}
	if err := validateV1NewUsers(data, mapping); err != nil {
		return nil, err
	}
	for alias, logins := range mapping.ProjectMembers {
		if _, ok := mapping.Projects[alias]; !ok {
			return nil, fmt.Errorf("project_members names unknown project %q", alias)
		}
		seen := map[string]bool{}
		for _, login := range logins {
			if login == "" || seen[login] {
				return nil, fmt.Errorf("project_members for %q has an empty or duplicate login", alias)
			}
			seen[login] = true
		}
	}
	rooms := map[string]bool{}
	for _, room := range data.Rooms {
		rooms[room.ID] = true
		choice, ok := mapping.Rooms[room.ID]
		if choice.Skip {
			continue
		}
		if !ok {
			choice.Project = mapping.DefaultProject
		}
		if choice.Project == "" {
			return nil, fmt.Errorf("room %q needs an explicit project or default_project", room.ID)
		}
		if _, ok := mapping.Projects[choice.Project]; !ok {
			return nil, fmt.Errorf("room %q has unknown project %q", room.ID, choice.Project)
		}
		if choice.Lifecycle != "" && choice.Lifecycle != "open" && choice.Lifecycle != "closed" {
			return nil, fmt.Errorf("room %q lifecycle must be open or closed", room.ID)
		}
		if err := validateV1RoomTarget(room.ID, choice); err != nil {
			return nil, err
		}
		created, err := v1ImportTimestamp(room.CreatedAt)
		if err != nil || created == "" {
			return nil, fmt.Errorf("room %q has invalid creation time: %v", room.ID, err)
		}
		for _, member := range room.Members {
			if v1ImportKind(member, data.Agents) == "system" {
				return nil, fmt.Errorf("system cannot be a room member in %q", room.ID)
			}
			if v1ImportKind(member, data.Agents) == "user" && mapping.Humans[member] == "" {
				return nil, fmt.Errorf("human %q in room %q needs a user mapping", member, room.ID)
			}
		}
		if strings.HasPrefix(room.ID, "dm:") && !mapping.Wizard {
			humans := 0
			for _, member := range room.Members {
				if v1ImportKind(member, data.Agents) == "user" {
					humans++
				}
			}
			if humans == 0 {
				return nil, fmt.Errorf("DM %q needs a mapped human participant", room.ID)
			}
		}
	}
	for id := range mapping.Rooms {
		if !rooms[id] {
			return nil, fmt.Errorf("map names unknown room %q", id)
		}
	}
	messageIDs := map[int64]string{}
	attachments := map[string]v1AttachmentCheck{}
	for _, message := range data.Messages {
		if !rooms[message.RoomID] || message.ID < 1 {
			return nil, fmt.Errorf("message %d has invalid room or ID", message.ID)
		}
		if _, err := v1ImportTimestamp(message.CreatedAt); err != nil {
			return nil, fmt.Errorf("message %d has invalid time: %w", message.ID, err)
		}
		if !skippedV1Room(mapping, message.RoomID) && message.From != "system" && v1ImportMessageKind(message, data.Agents) == "user" && mapping.Humans[message.From] == "" {
			return nil, fmt.Errorf("human author %q needs a user mapping", message.From)
		}
		if message.ReplyTo > 0 {
			if messageIDs[message.ReplyTo] != message.RoomID {
				return nil, fmt.Errorf("message %d has missing or cross-room reply %d", message.ID, message.ReplyTo)
			}
		}
		messageIDs[message.ID] = message.RoomID
		if skippedV1Room(mapping, message.RoomID) {
			continue
		}
		for _, ref := range message.Attachments {
			if _, exists := attachments[ref.ID]; exists {
				return nil, fmt.Errorf("attachment %q referenced more than once", ref.ID)
			}
			entry, ok := data.Attachments[ref.ID]
			if !ok || strings.ContainsAny(entry.ID+entry.Ext, "/\\") || entry.ID == "" {
				return nil, fmt.Errorf("message %d has invalid attachment %q", message.ID, ref.ID)
			}
			path := v1ImportFile(sourceDir, entry)
			hash, size, err := v1ImportHash(path)
			if err != nil || size != entry.Size {
				return nil, fmt.Errorf("attachment %q missing or size mismatch: %v", ref.ID, err)
			}
			attachments[ref.ID] = v1AttachmentCheck{entry, path, hash}
		}
	}
	for id := range data.Reactions {
		if _, ok := messageIDs[id]; !ok {
			return nil, fmt.Errorf("reactions reference missing message %d", id)
		}
	}
	for messageID, reactions := range data.Reactions {
		if skippedV1Room(mapping, messageIDs[messageID]) {
			continue
		}
		for _, reaction := range reactions {
			if v1ImportKind(reaction.AgentID, data.Agents) == "system" {
				return nil, errors.New("system cannot be a reaction author")
			}
			if v1ImportKind(reaction.AgentID, data.Agents) == "user" && mapping.Humans[reaction.AgentID] == "" {
				return nil, fmt.Errorf("human reaction author %q needs a user mapping", reaction.AgentID)
			}
			if _, err := v1ImportTimestamp(reaction.CreatedAt); err != nil {
				return nil, fmt.Errorf("reaction by %q has invalid time: %w", reaction.AgentID, err)
			}
		}
	}
	scopeBytes := map[string]int{}
	for _, memory := range data.Memory {
		if v1MemorySkipReason(mapping, memory) != "" {
			continue
		}
		if v1ImportKind(memory.Author, data.Agents) == "user" && mapping.Humans[memory.Author] == "" {
			return nil, fmt.Errorf("memory author %q needs a user mapping", memory.Author)
		}
		switch memory.Scope {
		case "project_facts":
			alias := mapping.MemoryProjects[memory.ScopeKey]
			if alias == "" {
				return nil, fmt.Errorf("memory project scope %q needs mapping", memory.ScopeKey)
			}
			if _, ok := mapping.Projects[alias]; !ok {
				return nil, fmt.Errorf("memory project scope %q names unknown project", memory.ScopeKey)
			}
		case "user_profile":
			if mapping.Humans[memory.ScopeKey] == "" {
				return nil, fmt.Errorf("profile %q needs human mapping", memory.ScopeKey)
			}
		case "agent_shared_notes":
			if memory.ScopeKey != "global" {
				return nil, fmt.Errorf("shared note %q has unknown scope key", memory.ID)
			}
		default:
			return nil, fmt.Errorf("memory %q has unknown scope %q", memory.ID, memory.Scope)
		}
		if !utf8.ValidString(memory.Body) || utf8.RuneCountInString(memory.Body) > 2000 || memory.Version < 1 {
			return nil, fmt.Errorf("memory %q exceeds body limit or has invalid version", memory.ID)
		}
		if _, err := v1ImportTimestamp(memory.CreatedAt); err != nil {
			return nil, fmt.Errorf("memory %q has invalid creation time: %w", memory.ID, err)
		}
		if _, err := v1ImportTimestamp(memory.UpdatedAt); err != nil {
			return nil, fmt.Errorf("memory %q has invalid update time: %w", memory.ID, err)
		}
		if memory.SourceMessageID > 0 {
			room, found := messageIDs[memory.SourceMessageID]
			if !found || skippedV1Room(mapping, room) {
				data.UnresolvedMemorySources++
			}
		}
		scopeKey := memory.ScopeKey
		if memory.Scope == "project_facts" {
			scopeKey = mapping.MemoryProjects[scopeKey]
		} else if memory.Scope == "user_profile" {
			scopeKey = mapping.Humans[scopeKey]
		}
		key := memory.Scope + ":" + scopeKey
		scopeBytes[key] += len(memory.Body)
		cap := 4096
		if memory.Scope == "user_profile" {
			cap = 2048
		}
		if scopeBytes[key] > cap {
			return nil, v1MemoryCapError(memory.Scope, scopeKey, cap, mapping.Wizard)
		}
	}
	return attachments, nil
}

func importMapID(ctx context.Context, tx *sql.Tx, source, kind, old string) (string, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, "SELECT new_id FROM legacy_import_map WHERE source_key=? AND entity_kind=? AND old_id=?", source, kind, old).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	return "", false, nil
}

func addImportMap(ctx context.Context, tx *sql.Tx, source, kind, old, id string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO legacy_import_map(source_key,entity_kind,old_id,new_id) VALUES (?,?,?,?)", source, kind, old, id)
	return err
}

func checkWizardChoice(ctx context.Context, tx *sql.Tx, source, kind, old string, choice any) error {
	raw, err := json.Marshal(choice)
	if err != nil {
		return err
	}
	var previous string
	err = tx.QueryRowContext(ctx, "SELECT choice_json FROM legacy_import_choice WHERE source_key=? AND entity_kind=? AND old_id=?", source, kind, old).Scan(&previous)
	if err == nil {
		if previous != string(raw) {
			return fmt.Errorf("import choice changed for %s %q", kind, old)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO legacy_import_choice(source_key,entity_kind,old_id,choice_json) VALUES (?,?,?,?)", source, kind, old, string(raw))
	return err
}

func checkSavedWizardMap(ctx context.Context, tx *sql.Tx, mapping V1ImportMap) error {
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT map_json FROM legacy_import_config WHERE source_key=?", mapping.SourceKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var old V1ImportMap
	if err := json.Unmarshal([]byte(raw), &old); err != nil {
		return err
	}
	for alias, spec := range old.Projects {
		if !reflect.DeepEqual(mapping.Projects[alias], spec) {
			return fmt.Errorf("imported project %q choice changed", alias)
		}
	}
	for human, login := range old.Humans {
		if mapping.Humans[human] != login {
			return fmt.Errorf("historical human %q choice changed", human)
		}
	}
	for room, spec := range old.Rooms {
		if !spec.Skip && !reflect.DeepEqual(mapping.Rooms[room], spec) {
			return fmt.Errorf("imported room %q choice changed", room)
		}
	}
	for scope, alias := range old.MemoryProjects {
		if alias != "-" && mapping.MemoryProjects[scope] != alias {
			return fmt.Errorf("imported memory scope %q choice changed", scope)
		}
	}
	for alias := range old.Projects {
		if old.EnableProjectMemory[alias] != mapping.EnableProjectMemory[alias] {
			return fmt.Errorf("project %q memory choice changed", alias)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT old_id FROM legacy_import_map WHERE source_key=? AND entity_kind='macro'`, mapping.SourceKey)
	if err != nil {
		return err
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return err
		}
		previous, choice := old.MacroConflicts[key], mapping.MacroConflicts[key]
		if previous == "" {
			previous = "keep_v2"
		}
		if choice == "" {
			choice = "keep_v2"
		}
		if previous != choice {
			rows.Close()
			return fmt.Errorf("imported macro %q choice changed", key)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, `SELECT old_id FROM legacy_import_map WHERE source_key=? AND entity_kind='role'`, mapping.SourceKey)
	if err != nil {
		return err
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return err
		}
		previous, choice := old.RoleConflicts[key], mapping.RoleConflicts[key]
		if previous == "" {
			previous = "keep_v2"
		}
		if choice == "" {
			choice = "keep_v2"
		}
		if previous != choice {
			rows.Close()
			return fmt.Errorf("imported role %q choice changed", key)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if !old.SkipProfiles && mapping.SkipProfiles {
		return errors.New("imported profile choice changed")
	}
	if !old.SkipShared && mapping.SkipShared {
		return errors.New("imported shared notes choice changed")
	}
	return nil
}

func (s *Store) ImportV1(ctx context.Context, sourceDir string, mapping V1ImportMap, dryRun bool) (V1ImportReport, error) {
	report := V1ImportReport{NotCarried: map[string]int{}, DryRun: dryRun, MemoryEnabled: false, Imported: map[string]int{}, AlreadyPresent: map[string]int{}, Skipped: map[string]int{}, ProjectReaders: map[string]int{}, ProjectAccessGaps: map[string]int{}, UnreadableDMParticipants: map[string]int{}, ExistingOwnerGrants: map[string]int{}, GrantedViewers: map[string]int{}, MacroConflicts: map[string]string{}, RoleConflicts: map[string]string{}, SkippedMemory: []V1SkippedMemory{}, SkippedRoles: map[string]string{}, SkippedSounds: map[string]string{}}
	if used, err := s.V1ImportUsed(ctx); err != nil {
		return report, err
	} else if used {
		return report, ErrV1InstallUsed
	}
	data, err := OpenV1ImportSource(ctx, sourceDir)
	if err != nil {
		return report, err
	}
	// A room with no project to go to, and a memory scope with none, are not imported; the report lists what is dropped.
	mapping, report.SkippedNoProject = normalizeV1Mapping(data, mapping)
	report.SkippedSettings = skippedV1Settings(data.Settings, data.PromptOverrideCount, data.SkippedSourceSettings)
	report.Skipped["settings"] = len(report.SkippedSettings)
	report.NotCarried = data.NotCarried
	files, err := validateV1Import(data, sourceDir, mapping)
	if err != nil {
		return report, err
	}
	report.Skipped["orphan_attachments"] = v1UnreferencedAttachments(data) // files no message refers to; an attachment of a room left out is counted with that room's messages
	report.Skipped["unresolved_memory_source_links"] = data.UnresolvedMemorySources
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	digest := v1ImportDigest(mapping)
	var oldDigest string
	var highWater int64
	err = tx.QueryRowContext(ctx, "SELECT map_digest,message_high_water FROM legacy_import_source WHERE source_key=?", mapping.SourceKey).Scan(&oldDigest, &highWater)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, "INSERT INTO legacy_import_source(source_key,map_digest,created_at,updated_at) VALUES (?,?,?,?)", mapping.SourceKey, digest, timestamp(), timestamp())
	} else if err == nil && oldDigest != digest && !mapping.Wizard {
		return report, errors.New("import mapping changed for an existing source_key")
	}
	if err != nil {
		return report, err
	}
	if mapping.Wizard {
		if oldDigest != "" && oldDigest != digest {
			var saved string
			if e := tx.QueryRowContext(ctx, "SELECT map_json FROM legacy_import_config WHERE source_key=?", mapping.SourceKey).Scan(&saved); errors.Is(e, sql.ErrNoRows) {
				return report, errors.New("existing CLI import mapping cannot be changed by the wizard")
			} else if e != nil {
				return report, e
			}
		}
		if err := checkSavedWizardMap(ctx, tx, mapping); err != nil {
			return report, err
		}
	}
	ownerID := ""
	if err := tx.QueryRowContext(ctx, "SELECT id FROM user WHERE login=?", mapping.OwnerLogin).Scan(&ownerID); err != nil {
		return report, fmt.Errorf("destination owner %q: %w", mapping.OwnerLogin, err)
	}
	existingMacros, err := listMacrosTx(ctx, tx)
	if err != nil {
		return report, err
	}
	for _, key := range sortedImportKeys(data.Macros) {
		body := data.Macros[key]
		ledgerID := mapping.SourceKey + ":" + key
		_, found, err := importMapID(ctx, tx, mapping.SourceKey, "macro", key)
		if err != nil {
			return report, err
		}
		if found {
			report.AlreadyPresent["macros"]++
			continue
		}
		if slices.Contains(mapping.SkipMacros, key) {
			report.Skipped["macros"]++
			continue
		}
		if current, exists := existingMacros[key]; exists {
			if current != body {
				choice := mapping.MacroConflicts[key]
				if choice == "" {
					choice = "keep_v2"
				}
				report.MacroConflicts[key] = choice
				if choice == "keep_v2" {
					report.Skipped["macro_conflicts"]++
					if err := addImportMap(ctx, tx, mapping.SourceKey, "macro", key, ledgerID); err != nil {
						return report, err
					}
					continue
				}
				if _, err := tx.ExecContext(ctx, `UPDATE macro SET body=? WHERE key=?`, body, key); err != nil {
					return report, err
				}
				report.Imported["macros"]++
			} else {
				report.AlreadyPresent["macros"]++
			}
		} else {
			if len(existingMacros) >= macroCountMax {
				return report, fmt.Errorf("destination exceeds %d macros; remove a macro before import", macroCountMax)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO macro(key,body) VALUES (?,?)`, key, body); err != nil {
				return report, err
			}
			existingMacros[key] = body
			report.Imported["macros"]++
		}
		if err := addImportMap(ctx, tx, mapping.SourceKey, "macro", key, ledgerID); err != nil {
			return report, err
		}
	}
	if err := importV1Roles(ctx, tx, data, mapping, &report); err != nil {
		return report, err
	}
	newUserGrants, err := importV1NewUsers(ctx, tx, data, mapping, &report)
	if err != nil {
		return report, err
	}
	userIDs := map[string]string{}
	for old, login := range mapping.Humans {
		if mapping.Wizard {
			if err := checkWizardChoice(ctx, tx, mapping.SourceKey, "human", old, login); err != nil {
				return report, err
			}
		}
		if login == "-" {
			continue
		}
		var id string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM user WHERE login=?", login).Scan(&id); err != nil {
			return report, fmt.Errorf("destination user %q for %q: %w", login, old, err)
		}
		userIDs[old] = id
	}
	projectIDs := map[string]string{}
	projectFirstImport := map[string]bool{}
	targets := newV1Targets()
	for _, alias := range sortedImportKeys(mapping.Projects) {
		spec := mapping.Projects[alias]
		if mapping.Wizard {
			if err := checkWizardChoice(ctx, tx, mapping.SourceKey, "project", alias, spec); err != nil {
				return report, err
			}
		}
		id, found, err := importMapID(ctx, tx, mapping.SourceKey, "project", alias)
		if err != nil {
			return report, err
		}
		projectFirstImport[alias] = !found
		if found {
			report.AlreadyPresent["projects"]++
		} else if spec.ExistingID != "" {
			id = spec.ExistingID
			var count int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM project WHERE id=?", id).Scan(&count); err != nil || count != 1 {
				return report, fmt.Errorf("existing project %q missing: %v", id, err)
			}
			if err := addImportMap(ctx, tx, mapping.SourceKey, "project", alias, id); err != nil {
				return report, err
			}
			report.AlreadyPresent["projects"]++
		} else {
			id, err = newID("prj")
			if err != nil {
				return report, err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO project(id,name,folder,created_at) VALUES (?,?,?,?)", id, spec.Name, spec.Folder, timestamp()); err != nil {
				return report, err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO settings_project(project_id) VALUES (?)", id); err != nil {
				return report, err
			}
			if err := addImportMap(ctx, tx, mapping.SourceKey, "project", alias, id); err != nil {
				return report, err
			}
			report.Imported["projects"]++
		}
		projectIDs[alias] = id
		ownerGrant, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO membership(project_id,user_id,role) VALUES (?,?,'owner')", id, ownerID)
		if err != nil {
			return report, err
		}
		if rows, _ := ownerGrant.RowsAffected(); rows > 0 && spec.ExistingID != "" {
			report.ExistingOwnerGrants[alias] = int(rows)
		}
		var ownerRole string
		if err := tx.QueryRowContext(ctx, "SELECT role FROM membership WHERE project_id=? AND user_id=?", id, ownerID).Scan(&ownerRole); err != nil || ownerRole != "owner" {
			return report, fmt.Errorf("destination user must own project %q", alias)
		}
		for _, grant := range newUserGrants[alias] {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO membership(project_id,user_id,role) VALUES (?,?,?)", id, grant.UserID, grant.Role); err != nil {
				return report, err
			}
		}
		for _, login := range mapping.ProjectMembers[alias] {
			var memberID string
			if err := tx.QueryRowContext(ctx, "SELECT id FROM user WHERE login=?", login).Scan(&memberID); err != nil {
				return report, fmt.Errorf("project member %q for %q: %w", login, alias, err)
			}
			grant, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO membership(project_id,user_id,role) VALUES (?,?,'viewer')", id, memberID)
			if err != nil {
				return report, err
			}
			if rows, _ := grant.RowsAffected(); rows > 0 {
				report.GrantedViewers[alias] += int(rows)
			}
		}
		var memoryEnabled bool
		if err := tx.QueryRowContext(ctx, "SELECT memory_enabled FROM settings_project WHERE project_id=?", id).Scan(&memoryEnabled); err != nil {
			return report, err
		}
		if memoryEnabled {
			return report, fmt.Errorf("project %q has memory enabled; turn it off before importing", alias)
		}
		var readers int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM membership WHERE project_id=?", id).Scan(&readers); err != nil {
			return report, err
		}
		report.ProjectReaders[alias] = readers
		_, homePresent, err := importMapID(ctx, tx, mapping.SourceKey, "room", "home:"+alias)
		if err != nil {
			return report, err
		}
		if !homePresent && spec.ExistingID == "" {
			homeID, err := newID("room")
			if err != nil {
				return report, err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO room(id,project_id,depth,kind,title,created_by,created_at) VALUES (?,?,0,'home',?,?,?)", homeID, id, spec.Name, ownerID, timestamp()); err != nil {
				return report, err
			}
			if err := addImportMap(ctx, tx, mapping.SourceKey, "room", "home:"+alias, homeID); err != nil {
				return report, err
			}
			targets.freshHomes[homeID] = true
			report.Imported["home_rooms"]++
			// Like a project made in v2, an imported one starts with a "general" task room beside the home room. It is recorded as the
			// shared "general" sub-room, so v1 rooms mapped to that sub-room land in it rather than in a second room of the same name.
			generalID, err := newID("room")
			if err != nil {
				return report, err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO room(id,project_id,parent_id,depth,kind,title,created_by,created_at) VALUES (?,?,?,1,'task',?,?,?)", generalID, id, homeID, GeneralRoomTitle, ownerID, timestamp()); err != nil {
				return report, err
			}
			if err := addImportMap(ctx, tx, mapping.SourceKey, "room", v1SubRoomKey(alias, GeneralRoomTitle), generalID); err != nil {
				return report, err
			}
			targets.freshSubs[generalID] = true
		} else {
			report.AlreadyPresent["home_rooms"]++
		}
	}
	accessChecked := map[string]bool{}
	for _, room := range data.Rooms {
		if skippedV1Room(mapping, room.ID) {
			continue
		}
		alias := mapping.Rooms[room.ID].Project
		if alias == "" {
			alias = mapping.DefaultProject
		}
		for _, member := range room.Members {
			if v1ImportKind(member, data.Agents) != "user" {
				continue
			}
			memberID := userIDs[member]
			if memberID == "" {
				continue
			}
			key := alias + ":" + memberID
			var count int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM membership WHERE project_id=? AND user_id=?", projectIDs[alias], memberID).Scan(&count); err != nil {
				return report, err
			}
			if count == 0 && !accessChecked[key] {
				report.ProjectAccessGaps[alias]++
				accessChecked[key] = true
			}
			if count == 0 && strings.HasPrefix(room.ID, "dm:") {
				report.UnreadableDMParticipants[alias]++
			}
		}
		if skippedV1HumanInRoom(data, mapping, room) && report.ProjectReaders[alias] > 1 {
			return report, fmt.Errorf("room %q has a skipped person; destination project %q must be owner-only", room.ID, alias)
		}
	}
	roomIDs := map[string]string{}
	newRooms := map[string]bool{}
	for _, room := range data.Rooms {
		spec := mapping.Rooms[room.ID]
		if spec.Skip {
			report.Skipped["rooms"]++
			report.Skipped["historical_members"] += len(room.Members)
			continue
		}
		if spec.Project == "" {
			spec.Project = mapping.DefaultProject
		}
		projectID := projectIDs[spec.Project]
		if mapping.Wizard {
			if err := checkWizardChoice(ctx, tx, mapping.SourceKey, "room", room.ID, spec); err != nil {
				return report, err
			}
		}
		id, found, err := importMapID(ctx, tx, mapping.SourceKey, "room", room.ID)
		if err != nil {
			return report, err
		}
		roomTarget := v1RoomTarget(spec, room.ID)
		if found {
			if roomTarget == V1TargetRoom && room.MemoryEnabled != nil && !*room.MemoryEnabled {
				_, marked, err := importMapID(ctx, tx, mapping.SourceKey, "room_memory", room.ID)
				if err != nil {
					return report, err
				}
				if !marked {
					if _, err := tx.ExecContext(ctx, "UPDATE room SET memory_enabled=0 WHERE id=? AND memory_enabled IS NULL", id); err != nil {
						return report, err
					}
					if err := addImportMap(ctx, tx, mapping.SourceKey, "room_memory", room.ID, id); err != nil {
						return report, err
					}
				}
			}
			report.AlreadyPresent["rooms"]++
			report.AlreadyPresent["historical_members"] += len(room.Members)
			roomIDs[room.ID] = id
			continue
		}
		if roomTarget != V1TargetRoom {
			target, err := targets.place(ctx, tx, data, mapping, &report, room, spec, projectID, mapping.Projects[spec.Project].Name, ownerID, userIDs, newRooms)
			if err != nil {
				return report, err
			}
			roomIDs[room.ID] = target
			continue
		}
		id, err = newID("room")
		if err != nil {
			return report, err
		}
		kind, parent, dmKey := "task", "", ""
		if strings.HasPrefix(room.ID, "dm:") {
			kind, dmKey = "dm", "legacy:"+mapping.SourceKey+":"+room.ID
		} else {
			homeKey := "home:" + spec.Project
			home, present, err := importMapID(ctx, tx, mapping.SourceKey, "room", homeKey)
			if err != nil {
				return report, err
			}
			if !present {
				err = tx.QueryRowContext(ctx, "SELECT id FROM room WHERE project_id=? AND kind='home' ORDER BY created_at LIMIT 1", projectID).Scan(&home)
				if err != nil {
					return report, fmt.Errorf("project %q has no home room: %w", spec.Project, err)
				}
				if err := addImportMap(ctx, tx, mapping.SourceKey, "room", homeKey, home); err != nil {
					return report, err
				}
			}
			parent = home
		}
		createdAt, _ := v1ImportTimestamp(room.CreatedAt)
		lifecycle, closedAt := "closed", any(timestamp())
		if spec.Lifecycle == "open" {
			lifecycle, closedAt = "open", nil
		}
		createdBy := room.CreatedBy
		if mapped := userIDs[createdBy]; mapped != "" {
			createdBy = mapped
		}
		var roomMemory any
		if room.MemoryEnabled != nil && !*room.MemoryEnabled {
			roomMemory = false
		}
		title := room.ID
		if kind == "dm" {
			// A DM has no archive in v2's sidebar, so it always starts open, and it is titled by the other participant.
			if spec.Lifecycle != "open" {
				report.OpenedDMs = append(report.OpenedDMs, room.ID)
			}
			lifecycle, closedAt = "open", nil
			title = v1DMTitle(room, data.Agents, mapping.Humans, mapping.OwnerLogin)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO room(id,project_id,parent_id,depth,kind,dm_key,lifecycle,title,created_by,created_at,closed_at,memory_enabled) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)", id, projectID, nullable(parent), map[bool]int{true: 0, false: 1}[kind == "dm"], kind, nullable(dmKey), lifecycle, title, createdBy, createdAt, closedAt, roomMemory); err != nil {
			return report, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO legacy_room(room_id,data_json) VALUES (?,?)", id, data.RoomRaw[room.ID]); err != nil {
			return report, err
		}
		if err := addImportMap(ctx, tx, mapping.SourceKey, "room", room.ID, id); err != nil {
			return report, err
		}
		if roomMemory != nil {
			if err := addImportMap(ctx, tx, mapping.SourceKey, "room_memory", room.ID, id); err != nil {
				return report, err
			}
		}
		roomIDs[room.ID] = id
		newRooms[id] = true
		report.Imported["rooms"]++
		if roomMemory != nil {
			report.Imported["room_memory_settings"]++
		}
		for _, member := range room.Members {
			kind := v1ImportKind(member, data.Agents)
			label := member
			if agent, ok := data.Agents[member]; ok && agent.Name != "" {
				label = agent.Name
				if kind == "agent" {
					label = member
					if agent.Harness != "" || agent.Model != "" {
						label += " · " + strings.Trim(strings.Join([]string{agent.Harness, agent.Model}, "/"), "/")
					}
				}
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO legacy_room_member(room_id,legacy_id,kind,label) VALUES (?,?,?,?)", id, member, kind, label); err != nil {
				return report, err
			}
			report.Imported["historical_members"]++
			if kind == "user" && userIDs[member] != "" {
				if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO room_member(room_id,member_kind,member_id,joined_at) VALUES (?,'user',?,?)", id, userIDs[member], createdAt); err != nil {
					return report, err
				}
			}
		}
		if mapping.Wizard && kind == "dm" {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO room_member(room_id,member_kind,member_id,joined_at) VALUES (?,'user',?,?)", id, ownerID, createdAt); err != nil {
				return report, err
			}
		}
	}
	report.TargetWarnings = targets.warnings()
	var sourceMaxID int64
	for _, message := range data.Messages {
		if message.ID > sourceMaxID {
			sourceMaxID = message.ID
		}
	}
	occupiedSeq := map[int64]bool{}
	eventRows, err := tx.QueryContext(ctx, `SELECT seq FROM event WHERE seq<=?`, sourceMaxID)
	if err != nil {
		return report, err
	}
	for eventRows.Next() {
		var seq int64
		if err := eventRows.Scan(&seq); err != nil {
			eventRows.Close()
			return report, err
		}
		occupiedSeq[seq] = true
	}
	if err := eventRows.Err(); err != nil {
		eventRows.Close()
		return report, err
	}
	eventRows.Close()
	messageIDs := map[int64]string{}
	skippedPresence := map[int64]bool{}
	for _, message := range data.Messages {
		if skippedV1Room(mapping, message.RoomID) {
			report.Skipped["messages"]++
			report.Skipped["attachments"] += len(message.Attachments)
			report.Skipped["reactions"] += len(data.Reactions[message.ID])
			continue
		}
		if isV1PresenceLine(message) {
			report.Skipped["presence_lines"]++
			report.Skipped["reactions"] += len(data.Reactions[message.ID])
			skippedPresence[message.ID] = true
			continue
		}
		key := strconv.FormatInt(message.ID, 10)
		id, found, err := importMapID(ctx, tx, mapping.SourceKey, "message", key)
		if err != nil {
			return report, err
		}
		if found {
			report.AlreadyPresent["messages"]++
			messageIDs[message.ID] = id
			continue
		}
		// v1 numbers are kept as they are (people refer to them as #N), so a number that is already
		// taken, by an event or by another message, cannot be imported: the import runs once, on a
		// fresh install, with the high-water reserved at startup.
		if occupiedSeq[message.ID] {
			return report, fmt.Errorf("v1 message #%d overlaps existing v2 activity; import requires a fresh first-run reservation", message.ID)
		}
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM message WHERE id=? OR seq=? OR (no=? AND no>0)`, key, message.ID, message.ID).Scan(&taken); err != nil {
			return report, err
		}
		if taken > 0 {
			return report, fmt.Errorf("v1 message #%d collides with a message already in this database; v1 history can only be imported once, into a fresh install", message.ID)
		}
		if message.ID <= highWater && !mapping.Wizard {
			return report, fmt.Errorf("v1 message %d arrived below imported high-water %d", message.ID, highWater)
		}
		id = key
		seq := message.ID
		createdAt, _ := v1ImportTimestamp(message.CreatedAt)
		authorKind := v1ImportMessageKind(message, data.Agents)
		authorID := message.From
		if authorKind == "user" {
			authorID = userIDs[message.From]
			if authorID == "" {
				authorID = message.From
			}
		}
		var reply any
		if message.ReplyTo > 0 && !skippedPresence[message.ReplyTo] {
			reply = messageIDs[message.ReplyTo]
			if reply == "" {
				return report, fmt.Errorf("missing imported reply %d", message.ReplyTo)
			}
		}
		if message.Targets == nil {
			message.Targets = []string{}
		}
		for i, target := range message.Targets {
			if mapped := userIDs[target]; mapped != "" {
				message.Targets[i] = mapped
			}
		}
		targets, _ := json.Marshal(message.Targets)
		var proposed []byte
		if len(message.ProposedAnswers) > 0 {
			proposed, _ = json.Marshal(message.ProposedAnswers)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO message(id,seq,no,room_id,author_kind,author_id,body,reply_to,targets_json,created_at,
			needs_attention,proposed_answers_json,open_questions_json,visual_plan_json,appendix_pages_json,author_harness,author_model,author_session)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, seq, message.ID, roomIDs[message.RoomID], authorKind, authorID, message.Body, reply, string(targets), createdAt,
			message.NeedsAttention, nullable(string(proposed)), rawJSONOrNil(message.OpenQuestions), rawJSONOrNil(message.VisualPlan), rawJSONOrNil(message.AppendixPages), message.FromHarness, message.FromModel, message.FromSession); err != nil {
			return report, err
		}
		if err := addImportMap(ctx, tx, mapping.SourceKey, "message", key, id); err != nil {
			return report, err
		}
		messageIDs[message.ID] = id
		report.Imported["messages"]++
		if message.ID > highWater {
			highWater = message.ID
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE seq_counter SET value=MAX(value,?) WHERE id=1`, sourceMaxID); err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE message_no_counter SET value=MAX(value,?) WHERE id=1`, sourceMaxID); err != nil {
		return report, err
	}
	// Files are copied only after all source and destination checks succeed.
	// The server is stopped, so a staged file cannot be read before commit.
	staged := []string{}
	committed := false
	defer func() {
		if !committed {
			for _, path := range staged {
				_ = os.Remove(path)
			}
		}
	}()
	for _, message := range data.Messages {
		if skippedV1Room(mapping, message.RoomID) || skippedPresence[message.ID] {
			continue
		}
		roomID := roomIDs[message.RoomID]
		for _, ref := range message.Attachments {
			check := files[ref.ID]
			id, found, e := importMapID(ctx, tx, mapping.SourceKey, "attachment", ref.ID)
			if e != nil {
				return report, e
			}
			if found {
				report.AlreadyPresent["attachments"]++
				continue
			}
			id, e = newID("att")
			if e != nil {
				return report, e
			}
			destination := filepath.Join(s.attachmentDir, id+".bin")
			if !dryRun {
				if e = os.MkdirAll(s.attachmentDir, 0700); e != nil {
					return report, e
				}
				input, e := os.Open(check.Path)
				if e != nil {
					return report, e
				}
				output, e := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if e == nil {
					_, e = io.Copy(output, input)
					closeErr := output.Close()
					if e == nil {
						e = closeErr
					}
				}
				_ = input.Close()
				if e != nil {
					return report, e
				}
				staged = append(staged, destination)
				copiedHash, copiedSize, e := v1ImportHash(destination)
				if e != nil || copiedHash != check.Hash || copiedSize != check.Entry.Size {
					return report, fmt.Errorf("copied attachment %q changed during import: %v", ref.ID, e)
				}
			}
			mimePath := check.Path
			if !dryRun {
				mimePath = destination
			}
			mime, e := v1ImportMIME(mimePath)
			if e != nil {
				return report, e
			}
			createdAt, e := v1ImportTimestamp(check.Entry.UploadedAt)
			if e != nil {
				return report, e
			}
			projectAlias := mapping.Rooms[message.RoomID].Project
			if projectAlias == "" {
				projectAlias = mapping.DefaultProject
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO attachment(id,project_id,room_id,message_id,sha256,mime,size,path,created_by,created_at,filename,width,height,arrived_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)", id, projectIDs[projectAlias], roomID, messageIDs[message.ID], check.Hash, mime, check.Entry.Size, destination, ownerID, createdAt, safeAttachmentName(check.Entry.Name), max(check.Entry.Width, 0), max(check.Entry.Height, 0), timestamp()); e != nil {
				return report, e
			}
			if e = addImportMap(ctx, tx, mapping.SourceKey, "attachment", ref.ID, id); e != nil {
				return report, e
			}
			report.Imported["attachments"]++
		}
		for _, reaction := range data.Reactions[message.ID] {
			if skippedV1Human(mapping, reaction.AgentID) {
				report.Skipped["reactions"]++
				continue
			}
			kind := v1ImportKind(reaction.AgentID, data.Agents)
			memberID := reaction.AgentID
			if kind == "user" {
				memberID = userIDs[reaction.AgentID]
				if memberID == "" {
					return report, fmt.Errorf("reaction by unmapped human %q", reaction.AgentID)
				}
			}
			createdAt, e := v1ImportTimestamp(reaction.CreatedAt)
			if e != nil {
				return report, e
			}
			result, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO reaction(message_id,member_kind,member_id,emoji,created_at) VALUES (?,?,?,?,?)", messageIDs[message.ID], kind, memberID, reaction.Emoji, createdAt)
			if e != nil {
				return report, e
			}
			affected, _ := result.RowsAffected()
			if affected > 0 {
				report.Imported["reactions"]++
			} else {
				report.AlreadyPresent["reactions"]++
			}
		}
	}
	if err := importV1ReadCursors(ctx, tx, data, mapping, roomIDs, newRooms, userIDs, &report); err != nil {
		return report, err
	}
	if err := importV1RoomPrefs(ctx, tx, data, mapping, roomIDs, userIDs, &report); err != nil {
		return report, err
	}
	if err := importV1AgentSessions(ctx, tx, data, &report); err != nil {
		return report, err
	}
	if err := importV1AgentSessionHistory(ctx, tx, data, &report); err != nil {
		return report, err
	}
	if err := importV1AgentRoles(ctx, tx, data); err != nil {
		return report, err
	}
	for _, memory := range data.Memory {
		if reason := v1MemorySkipReason(mapping, memory); reason != "" {
			report.Skipped["memory"]++
			report.SkippedMemory = append(report.SkippedMemory, V1SkippedMemory{memory.ID, memory.Scope, memory.ScopeKey, reason})
			continue
		}
		if mapping.Wizard {
			if e := checkWizardChoice(ctx, tx, mapping.SourceKey, "memory", memory.ID, map[string]string{"scope": memory.Scope, "key": memory.ScopeKey, "destination": mapping.MemoryProjects[memory.ScopeKey]}); e != nil {
				return report, e
			}
		}
		id, found, e := importMapID(ctx, tx, mapping.SourceKey, "memory", memory.ID)
		if e != nil {
			return report, e
		}
		if found {
			report.AlreadyPresent["memory"]++
			continue
		}
		var used int
		cap := 4096
		if memory.Scope == "project_facts" {
			projectID := projectIDs[mapping.MemoryProjects[memory.ScopeKey]]
			if e := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(length(CAST(body AS BLOB))),0) FROM project_memory WHERE project_id=?", projectID).Scan(&used); e != nil {
				return report, e
			}
		} else {
			scopeKey := memory.ScopeKey
			if memory.Scope == "user_profile" {
				scopeKey = userIDs[scopeKey]
				cap = 2048
			}
			if e := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(length(CAST(body AS BLOB))),0) FROM global_memory WHERE scope=? AND scope_key=?", memory.Scope, scopeKey).Scan(&used); e != nil {
				return report, e
			}
		}
		if used+len(memory.Body) > cap {
			scopeKey := memory.ScopeKey
			if memory.Scope == "user_profile" {
				scopeKey = mapping.Humans[scopeKey]
			}
			return report, v1MemoryCapError(memory.Scope, scopeKey, cap, mapping.Wizard)
		}
		id, e = newID("knw")
		if e != nil {
			return report, e
		}
		kind := v1ImportKind(memory.Author, data.Agents)
		if kind == "system" {
			kind = "agent"
		}
		authorID := memory.Author
		if kind == "user" {
			authorID = userIDs[memory.Author]
			if authorID == "" {
				kind, authorID = "agent", "legacy:"+memory.Author
			}
		}
		var sourceMessage any
		if memory.SourceMessageID > 0 {
			if linked := messageIDs[memory.SourceMessageID]; linked != "" {
				sourceMessage = linked
			}
		}
		createdAt, _ := v1ImportTimestamp(memory.CreatedAt)
		updatedAt, _ := v1ImportTimestamp(memory.UpdatedAt)
		if memory.Scope == "project_facts" {
			projectID := projectIDs[mapping.MemoryProjects[memory.ScopeKey]]
			_, e = tx.ExecContext(ctx, "INSERT INTO project_memory(id,project_id,body,rev,author_kind,author_id,created_at,updated_at,source_message_id) VALUES (?,?,?,?,?,?,?,?,?)", id, projectID, memory.Body, memory.Version, kind, authorID, createdAt, updatedAt, sourceMessage)
		} else {
			scopeKey := memory.ScopeKey
			if memory.Scope == "user_profile" {
				scopeKey = userIDs[scopeKey]
			}
			_, e = tx.ExecContext(ctx, "INSERT INTO global_memory(id,scope,scope_key,body,rev,author_kind,author_id,source_message_id,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)", id, memory.Scope, scopeKey, memory.Body, memory.Version, kind, authorID, sourceMessage, createdAt, updatedAt)
		}
		if e != nil {
			return report, e
		}
		if e = addImportMap(ctx, tx, mapping.SourceKey, "memory", memory.ID, id); e != nil {
			return report, e
		}
		report.Imported["memory"]++
	}
	for alias, enabled := range mapping.EnableProjectMemory {
		if !enabled {
			continue
		}
		if _, ok := data.Settings["memory_enabled"]; !ok {
			return report, fmt.Errorf("v1 source has no memory setting")
		}
		var v1Enabled bool
		if err := json.Unmarshal(data.Settings["memory_enabled"], &v1Enabled); err != nil || !v1Enabled {
			return report, fmt.Errorf("v1 memory is not enabled")
		}
		projectID := projectIDs[alias]
		if projectID == "" {
			return report, fmt.Errorf("unknown project memory choice %q", alias)
		}
		if !projectFirstImport[alias] {
			continue
		}
		if _, err := tx.ExecContext(ctx, "UPDATE settings_project SET memory_enabled=1 WHERE project_id=?", projectID); err != nil {
			return report, err
		}
		report.Imported["project_memory_settings"]++
		report.MemoryEnabled = true
	}
	if err := importV1MessageExpiry(ctx, tx, data, &report); err != nil {
		return report, err
	}
	if err := importV1Preferences(ctx, tx, data, ownerID, &report); err != nil {
		return report, err
	}
	if err := importV1InlinePlans(ctx, tx, data, &report); err != nil {
		return report, err
	}
	if err := importV1Sounds(ctx, tx, sourceDir, s.soundDir(), ownerID, data, mapping, dryRun, &staged, &report); err != nil {
		return report, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE legacy_import_source SET message_high_water=?,updated_at=? WHERE source_key=?", highWater, timestamp(), mapping.SourceKey); err != nil {
		return report, err
	}
	if mapping.Wizard {
		raw, _ := json.Marshal(mapping)
		if _, err := tx.ExecContext(ctx, "INSERT INTO legacy_import_config(source_key,map_json) VALUES (?,?) ON CONFLICT(source_key) DO UPDATE SET map_json=excluded.map_json", mapping.SourceKey, string(raw)); err != nil {
			return report, err
		}
	}
	report.Accounting = buildV1Accounting(data, mapping, report)
	if dryRun {
		return report, tx.Rollback()
	}
	err = tx.Commit()
	if err != nil {
		return report, err
	}
	committed = true
	return report, nil
}

// importV1ReadCursors maps each mapped human's v1 read cursors (a message ID
// per room) onto room_read_cursor. A v1 cursor is translated to the seq of the
// newest imported message at or below it, so a cursor past the last imported
// message clamps to that message and one before the first imported message
// leaves the room unread for that person, as v1 had it. The upsert only
// raises a cursor: re-running the import, or a user who has since read
// further in v2, never lowers it, and two v1 humans mapped to one login keep
// the larger. Skipped rooms and humans are not touched.
//
// Imported history is not new to anyone, so every other reader of a room this
// run created, with no v1 cursor for it, starts at its newest message, the
// same baseline migration 43 gives existing rooms. Readers are the project's
// members in an ordinary room and the DM's own members in a private chat. A
// rerun does not baseline again, so people added to the project later keep
// the unread state they have.
func importV1ReadCursors(ctx context.Context, tx *sql.Tx, data *v1ImportData, mapping V1ImportMap, roomIDs map[string]string, newRooms map[string]bool, userIDs map[string]string, report *V1ImportReport) error {
	explicit := map[[2]string]bool{}
	v1RoomMessageIDs := map[string][]int64{}
	for _, message := range data.Messages {
		// A read position that lands on a line that was not imported (presence bookkeeping) falls back to the nearest earlier message that
		// was, rather than to nothing: a lookup that finds no imported message would leave the whole room unread.
		if isV1PresenceLine(message) {
			continue
		}
		v1RoomMessageIDs[message.RoomID] = append(v1RoomMessageIDs[message.RoomID], message.ID)
	}
	for _, ids := range v1RoomMessageIDs {
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	for _, human := range sortedImportKeys(data.Agents) {
		agent := data.Agents[human]
		if len(agent.ReadCursors) == 0 || v1ImportKind(human, data.Agents) != "user" {
			continue
		}
		for _, v1Room := range sortedImportKeys(agent.ReadCursors) {
			cursor := agent.ReadCursors[v1Room]
			if cursor <= 0 {
				continue
			}
			roomID := roomIDs[v1Room]
			if skippedV1Human(mapping, human) || skippedV1Room(mapping, v1Room) || userIDs[human] == "" || roomID == "" {
				report.Skipped["read_cursors"]++
				continue
			}
			explicit[[2]string{userIDs[human], roomID}] = true
			// The newest message of this v1 room at or below the cursor, found among that room's own messages: rooms merged into one
			// v2 room keep separate positions, and the larger one wins when they are stored.
			var read int64
			for _, id := range v1RoomMessageIDs[v1Room] {
				if id <= cursor {
					read = id
				}
			}
			var seq int64
			if read > 0 {
				if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(m.seq),0) FROM legacy_import_map lm JOIN message m ON m.id=lm.new_id
					WHERE lm.source_key=? AND lm.entity_kind='message' AND lm.old_id=?`, mapping.SourceKey, strconv.Itoa(int(read))).Scan(&seq); err != nil {
					return err
				}
			}
			if seq == 0 {
				report.Skipped["read_cursors"]++
				continue
			}
			var current int64
			err := tx.QueryRowContext(ctx, `SELECT last_read_seq FROM room_read_cursor WHERE user_id=? AND room_id=?`, userIDs[human], roomID).Scan(&current)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if current >= seq {
				report.AlreadyPresent["read_cursors"]++
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO room_read_cursor(user_id,room_id,last_read_seq) VALUES (?,?,?)
				ON CONFLICT(user_id,room_id) DO UPDATE SET last_read_seq=MAX(last_read_seq,excluded.last_read_seq)`, userIDs[human], roomID, seq); err != nil {
				return err
			}
			report.Imported["read_cursors"]++
		}
	}
	seen := map[string]bool{}
	for _, v1Room := range sortedImportKeys(roomIDs) {
		roomID := roomIDs[v1Room]
		if roomID == "" || seen[roomID] || !newRooms[roomID] || skippedV1Room(mapping, v1Room) {
			continue
		}
		seen[roomID] = true
		rows, err := tx.QueryContext(ctx, `SELECT user_id FROM membership WHERE project_id=(SELECT project_id FROM room WHERE id=?1) AND (SELECT kind FROM room WHERE id=?1)!='dm'
			UNION SELECT member_id FROM room_member WHERE room_id=?1 AND member_kind='user' AND left_at IS NULL AND (SELECT kind FROM room WHERE id=?1)='dm'`, roomID)
		if err != nil {
			return err
		}
		var readers []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			readers = append(readers, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, userID := range readers {
			if explicit[[2]string{userID, roomID}] {
				continue
			}
			result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO room_read_cursor(user_id,room_id,last_read_seq)
				VALUES (?1,?2,COALESCE((SELECT MAX(seq) FROM message WHERE room_id=?2),0))`, userID, roomID)
			if err != nil {
				return err
			}
			if n, _ := result.RowsAffected(); n > 0 {
				report.Imported["read_cursor_baselines"]++
			}
		}
	}
	return nil
}

// v1ExpiryKey is v1's global message-expiry setting. v2 keeps the same name, default
// and bounds (store.MessageExpiryKey), so a v1 value carries over as it is.
const v1ExpiryKey = "messages_considered_expired_after_seconds"

func v1ExpirySeconds(raw json.RawMessage) (int, error) {
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil || n != float64(int(n)) || !ValidMessageExpirySeconds(int(n)) {
		return 0, ErrInvalidMessageExpiry
	}
	return int(n), nil
}

// importV1MessageExpiry carries v1's expiry window into the global setting. An explicit
// v2 value wins: the import never overwrites a window someone already chose here.
func importV1MessageExpiry(ctx context.Context, tx *sql.Tx, data *v1ImportData, report *V1ImportReport) error {
	raw, ok := data.Settings[v1ExpiryKey]
	if !ok || string(raw) == "null" {
		return nil
	}
	seconds, err := v1ExpirySeconds(raw)
	if err != nil {
		return nil // already listed among the skipped settings
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO global_setting(key,value,updated_at) VALUES (?,?,?) ON CONFLICT(key) DO NOTHING`, MessageExpiryKey, strconv.Itoa(seconds), timestamp())
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n > 0 {
		report.Imported["message_expiry"]++
	} else {
		report.AlreadyPresent["message_expiry"]++
	}
	return nil
}

// normalizeV1Mapping makes "no project" mean "not imported" instead of an error. A kept room with no project (and no default project)
// becomes a skipped room, and a memory scope with no project becomes "-". v1's own system room is always skipped, whatever the mapping
// says (the wizard hides it, but a saved draft or a hand-written map can still name it). The caller's maps are not changed. It returns
// the v1 rooms it dropped for want of a project, in room order.
func normalizeV1Mapping(data *v1ImportData, mapping V1ImportMap) (V1ImportMap, []string) {
	rooms := make(map[string]V1ImportRoom, len(mapping.Rooms))
	for id, spec := range mapping.Rooms {
		rooms[id] = spec
	}
	var dropped []string
	for _, room := range data.Rooms {
		spec := rooms[room.ID]
		if room.ID == V1SystemRoom {
			rooms[room.ID] = V1ImportRoom{Skip: true, Lifecycle: spec.Lifecycle}
			continue
		}
		if spec.Skip || spec.Project != "" || mapping.DefaultProject != "" {
			continue
		}
		spec.Skip = true
		rooms[room.ID] = spec
		dropped = append(dropped, room.ID)
	}
	mapping.Rooms = rooms
	scopes := make(map[string]string, len(mapping.MemoryProjects))
	for scope, alias := range mapping.MemoryProjects {
		scopes[scope] = alias
	}
	for _, memory := range data.Memory {
		if memory.Scope == "project_facts" && scopes[memory.ScopeKey] == "" {
			scopes[memory.ScopeKey] = "-"
		}
	}
	mapping.MemoryProjects = scopes
	return mapping, dropped
}

// importV1InlinePlans carries v1's explicit inline-plans choice (always or optional) into the global setting. A value already saved in
// v2 wins, and a v1 install that never chose one has nothing to carry.
func importV1InlinePlans(ctx context.Context, tx *sql.Tx, data *v1ImportData, report *V1ImportReport) error {
	raw, ok := data.Settings[InlinePlanAppendixKey]
	if !ok || string(raw) == "null" {
		return nil
	}
	var mode string
	if json.Unmarshal(raw, &mode) != nil || mode != InlinePlanAlways && mode != InlinePlanOptional {
		return nil // already listed among the skipped settings
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO global_setting(key,value,updated_at) VALUES (?,?,?) ON CONFLICT(key) DO NOTHING`, InlinePlanAppendixKey, mode, timestamp())
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n > 0 {
		report.Imported["inline_plan_appendix"]++
	} else {
		report.AlreadyPresent["inline_plan_appendix"]++
	}
	return nil
}

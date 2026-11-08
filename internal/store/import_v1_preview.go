package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/sqliteutil"
)

// ErrV1BackupChanged means the backup no longer matches the key recorded when it was made: something wrote to it (a v1 server
// started on a copy of it, or a manual edit) after the move.
var ErrV1BackupChanged = errors.New("the v1 backup changed after the manifest was written")

// v1PreviewNow is the clock the preview's room defaults use; tests replace it.
var v1PreviewNow = time.Now

var ErrV1NoRooms = errors.New("v1 source has no rooms")

type V1ImportPreview struct {
	SourceKey        string          `json:"source_key"`
	Rooms            []V1PreviewRoom `json:"rooms"`
	Humans           []string        `json:"humans"`
	DestinationUsers []string        `json:"destination_users"`
	MemoryProjects   []string        `json:"memory_projects"`
	// MemoryCounts says how many records each memory choice covers: per project scope, plus the user profiles and the shared agent notes.
	MemoryCounts       V1MemoryCounts          `json:"memory_counts"`
	Macros             map[string]string       `json:"macros"`
	MacroDefaults      []string                `json:"macro_defaults"`
	ImportedMacros     []string                `json:"imported_macros"`
	ImportedRoles      []string                `json:"imported_roles"`
	V1MemoryEnabled    bool                    `json:"v1_memory_enabled"`
	V1DebugButton      bool                    `json:"v1_debug_button"`
	SkippedSettings    []V1SkippedSetting      `json:"skipped_settings"`
	Roles              []V1PreviewRole         `json:"roles"`
	SourceRoles        map[string]v1ImportRole `json:"-"`
	SkippedSourceRoles map[string]string       `json:"-"`
	Counts             map[string]int          `json:"counts"`
	NotCarried         map[string]int          `json:"not_carried"`
	CarriedOverFiles   []string                `json:"carried_over_files,omitempty"`
	HighestMessageID   int64                   `json:"-"`
	ReservationMissed  bool                    `json:"reservation_missed"`
	Suggested          V1ImportMap             `json:"suggested"`
	// HumanStakes is, for each v1 person, what is lost for good if the owner skips them (maps nobody to them): an import cannot be repeated,
	// and these records belong to the person's account. The wizard says so next to a skipped person.
	HumanStakes map[string]V1HumanStakes `json:"human_stakes"`
}

// V1MemoryCounts are the memory records of the backup by the choice that decides them on the wizard's Memory page.
type V1MemoryCounts struct {
	Projects map[string]int `json:"projects"`
	Profiles int            `json:"profiles"`
	Shared   int            `json:"shared"`
	// ProfileOwners splits Profiles by the v1 person each belongs to: a profile of a person mapped to nobody is not imported.
	ProfileOwners map[string]int `json:"profile_owners"`
}

// V1HumanStakes counts what belongs to one v1 person and is only imported when they are mapped to an account.
type V1HumanStakes struct {
	MemoryProfiles int `json:"memory_profiles"`
	Reactions      int `json:"reactions"`
	ReadPositions  int `json:"read_positions"`
	RoomView       int `json:"room_view"` // rooms they hid or pinned
}

type V1PreviewRoom struct {
	ID               string   `json:"id"`
	Messages         int      `json:"messages"`
	Members          []string `json:"members"`
	SuggestedProject string   `json:"suggested_project"`
	SuggestedSkip    bool     `json:"suggested_skip"`
	SkipReason       string   `json:"skip_reason,omitempty"` // set when the importer will skip the room whatever the mapping says
	MemoryDisabled   bool     `json:"memory_disabled"`
	// DM and Title describe a v1 direct message as v2 will show it (Title assumes the suggested people mapping).
	DM    bool   `json:"dm,omitempty"`
	Title string `json:"title,omitempty"`
	// Participants lets the wizard retitle a DM when the owner remaps a person (the title depends on who maps to the owner).
	Participants []V1DMParticipant `json:"participants,omitempty"`
}

type V1DMParticipant struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // "user" or "agent"
}

type V1SkippedSetting struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

func (preview *V1ImportPreview) ResolveRoles(destination []RoleDefinition) {
	ledgered := map[string]bool{}
	for _, key := range preview.ImportedRoles {
		ledgered[key] = true
	}
	preview.Roles = resolveV1Roles(preview.SourceRoles, destinationRoleMap(destination), ledgered)
	for _, key := range sortedImportKeys(preview.SkippedSourceRoles) {
		preview.Roles = append(preview.Roles, V1PreviewRole{Key: key, Label: key, SkippedReason: preview.SkippedSourceRoles[key]})
	}
}

func skippedV1Settings(settings map[string]json.RawMessage, promptOverrides int, sourceSkips []V1SkippedSetting) []V1SkippedSetting {
	result := append([]V1SkippedSetting{}, sourceSkips...)
	for key := range settings {
		// The personal settings are carried into the owner's preferences; only those v2 cannot take are listed (below).
		if key == "memory_enabled" || key == v1ExpiryKey || key == InlinePlanAppendixKey || v1PersonalKeys[key] {
			continue
		}
		result = append(result, V1SkippedSetting{key, "v2 has no matching server setting"})
	}
	_, personal := v1PersonalPreferences(settings)
	result = append(result, personal...)
	if promptOverrides > 0 {
		result = append(result, V1SkippedSetting{"prompt_overrides", fmt.Sprintf("%d v1 prompt overrides are not imported: v2 overrides different, shorter sentences (Settings → Prompts), so re-enter any you still need", promptOverrides)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

// PreviewV1Import exposes identifiers, counts, and global macro text for
// conflict review. Message and memory bodies remain on disk until commit.
func PreviewV1Import(ctx context.Context, sourceDir, ownerLogin string) (V1ImportPreview, error) {
	out := V1ImportPreview{NotCarried: map[string]int{}, Rooms: []V1PreviewRoom{}, Humans: []string{}, DestinationUsers: []string{}, MemoryProjects: []string{}, Macros: map[string]string{}, MacroDefaults: []string{}, ImportedMacros: []string{}, ImportedRoles: []string{}, SkippedSettings: []V1SkippedSetting{}}
	data, err := OpenV1ImportSource(ctx, sourceDir)
	if err != nil {
		return out, err
	}
	if err := ValidateMacros(data.Macros); err != nil {
		return out, fmt.Errorf("v1 macros: %w", err)
	}
	if raw, ok := data.Settings["memory_enabled"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &out.V1MemoryEnabled); err != nil {
			return out, fmt.Errorf("v1 memory setting: %w", err)
		}
	}
	if raw, ok := data.Settings["debug_button_enabled"]; ok {
		_ = json.Unmarshal(raw, &out.V1DebugButton)
	}
	out.SkippedSettings = skippedV1Settings(data.Settings, data.PromptOverrideCount, data.SkippedSourceSettings)
	path := filepath.Join(sourceDir, "server", "aimebu.sqlite")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		path = filepath.Join(sourceDir, "aimebu.sqlite")
	}
	file, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return out, err
	}
	if wal, err := os.Open(path + "-wal"); err == nil {
		if _, err := io.Copy(hash, wal); err != nil {
			wal.Close()
			return out, err
		}
		wal.Close()
	} else if !os.IsNotExist(err) {
		return out, err
	}
	out.SourceKey = "v1-" + hex.EncodeToString(hash.Sum(nil))[:24]
	out.NotCarried = data.NotCarried
	out.SourceRoles = data.Roles
	out.SkippedSourceRoles = data.SkippedSourceRoles
	if raw, err := os.ReadFile(filepath.Join(sourceDir, "manifest.json")); err == nil {
		var manifest struct {
			SourceKey string `json:"source_key"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return out, fmt.Errorf("backup manifest: %w", err)
		}
		if manifest.SourceKey != "" && manifest.SourceKey != out.SourceKey {
			return out, ErrV1BackupChanged
		}
	} else if !os.IsNotExist(err) {
		return out, err
	}
	humans := map[string]bool{}
	scopes := map[string]bool{}
	paired := map[string]bool{}
	roomCount := map[string]int{}
	for _, room := range data.Rooms {
		if strings.HasSuffix(room.ID, "-leader") {
			paired[strings.TrimSuffix(room.ID, "-leader")] = true
		}
	}
	lastMessage := map[string]time.Time{}
	for _, msg := range data.Messages {
		if msg.ID > out.HighestMessageID {
			out.HighestMessageID = msg.ID
		}
		// v1's presence bookkeeping is not imported, so it is neither a message of the room nor activity in it.
		if isV1PresenceLine(msg) {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, msg.CreatedAt); err == nil && at.After(lastMessage[msg.RoomID]) {
			lastMessage[msg.RoomID] = at
		}
		roomCount[msg.RoomID]++
		if v1ImportMessageKind(msg, data.Agents) == "user" {
			humans[msg.From] = true
		}
	}
	for _, room := range data.Rooms {
		for _, member := range room.Members {
			if v1ImportKind(member, data.Agents) == "user" {
				humans[member] = true
			}
		}
	}
	for _, reactions := range data.Reactions {
		for _, reaction := range reactions {
			if v1ImportKind(reaction.AgentID, data.Agents) == "user" {
				humans[reaction.AgentID] = true
			}
		}
	}
	out.MemoryCounts.Projects = map[string]int{}
	out.MemoryCounts.ProfileOwners = map[string]int{}
	for _, memory := range data.Memory {
		switch memory.Scope {
		case "project_facts":
			out.MemoryCounts.Projects[memory.ScopeKey]++
		case "user_profile":
			out.MemoryCounts.Profiles++
			out.MemoryCounts.ProfileOwners[memory.ScopeKey]++
		case "agent_shared_notes":
			out.MemoryCounts.Shared++
		}
		if memory.Scope == "project_facts" {
			scopes[memory.ScopeKey] = true
		}
		if memory.Scope == "user_profile" {
			humans[memory.ScopeKey] = true
		}
		if v1ImportKind(memory.Author, data.Agents) == "user" {
			humans[memory.Author] = true
		}
	}
	for id := range data.Agents {
		if v1ImportKind(id, data.Agents) == "user" {
			humans[id] = true
		}
	}
	for _, pref := range data.RoomPrefs {
		if v1ImportKind(pref.Agent, data.Agents) == "user" {
			humans[pref.Agent] = true
		}
	}
	delete(humans, "")
	delete(humans, "system")
	readCursors := 0
	for id, agent := range data.Agents {
		if v1ImportKind(id, data.Agents) != "user" {
			continue
		}
		for _, cursor := range agent.ReadCursors {
			if cursor > 0 {
				readCursors++
			}
		}
	}
	roomPrefs := 0
	for _, pref := range data.RoomPrefs {
		if (pref.Hidden || pref.Pinned) && v1ImportKind(pref.Agent, data.Agents) == "user" {
			roomPrefs++
		}
	}
	out.Counts = map[string]int{"rooms": len(data.Rooms), "messages": len(data.Messages), "attachments": len(data.Attachments), "memory": len(data.Memory), "humans": len(humans), "macros": len(data.Macros), "read_cursors": readCursors, "room_prefs": roomPrefs, "sounds": len(data.Sounds), "agent_sessions": len(data.AgentSessions), "agent_session_history": len(data.AgentSessionHistory)}
	out.Macros = data.Macros
	for key, seen := range data.MacroSeenDefault {
		if seen {
			out.MacroDefaults = append(out.MacroDefaults, key)
		}
	}
	sort.Strings(out.MacroDefaults)
	out.Suggested = V1ImportMap{SourceKey: out.SourceKey, OwnerLogin: ownerLogin, Projects: map[string]V1ImportProject{}, Rooms: map[string]V1ImportRoom{}, Humans: map[string]string{}, MemoryProjects: map[string]string{}}
	// Projects are suggested from what v1 itself names: an exact x + x-leader room pair, a room with traffic and no pair, and a memory scope. There is no catch-all
	// project: a room with no project to go to is not imported, and the wizard lists what will be dropped.
	for _, room := range data.Rooms {
		base := strings.TrimSuffix(room.ID, "-leader")
		if paired[base] {
			for _, candidate := range data.Rooms {
				if candidate.ID == base {
					out.Suggested.Projects[base] = V1ImportProject{Name: base}
					break
				}
			}
		}
	}
	// A room that has traffic and no leader pair is a project of its own, going into the root: v1 named such a room after the folder
	// its agents worked in, and an agent that has since left does not make the room any less a project. A DM, an empty room and v1's default "general" room are not.
	for _, room := range data.Rooms {
		if room.ID == V1SystemRoom || isV1DM(room.ID) || strings.HasSuffix(room.ID, "-leader") || room.ID == "general" || roomCount[room.ID] == 0 || paired[room.ID] {
			continue
		}
		if _, ok := out.Suggested.Projects[room.ID]; !ok {
			out.Suggested.Projects[room.ID] = V1ImportProject{Name: room.ID}
		}
	}
	for scope := range scopes {
		if _, ok := out.Suggested.Projects[scope]; !ok {
			out.Suggested.Projects[scope] = V1ImportProject{Name: scope}
		}
	}
	for _, room := range data.Rooms {
		alias := ""
		if _, ok := out.Suggested.Projects[strings.TrimSuffix(room.ID, "-leader")]; ok && (paired[strings.TrimSuffix(room.ID, "-leader")] || room.ID == strings.TrimSuffix(room.ID, "-leader")) {
			alias = strings.TrimSuffix(room.ID, "-leader")
		}
		skip := room.ID == V1SystemRoom || alias == "" || isV1DM(room.ID) // a DM is decided below, once its project is known
		// A room with traffic in the last 30 days stays open; an older one is archived (the owner can flip either, or all at once).
		lifecycle := "closed"
		if last, ok := lastMessage[room.ID]; ok && v1PreviewNow().Sub(last) <= 30*24*time.Hour {
			lifecycle = "open"
		}
		spec := V1ImportRoom{Project: alias, Lifecycle: lifecycle, Skip: skip}
		// v1 "x-leader" was the project's lead room and "x" its working room. In v2 the first is the project root and the second a
		// "general" sub-room of it. A room named like a project that has no leader room is the project's root.
		if alias != "" && room.ID != V1SystemRoom && !isV1DM(room.ID) {
			if strings.HasSuffix(room.ID, "-leader") || room.ID == alias && !paired[alias] {
				spec.Target = V1TargetRoot
			} else if room.ID == alias {
				spec.Target, spec.Title = V1TargetSub, V1DefaultSubRoomTitle
			}
		}
		out.Suggested.Rooms[room.ID] = spec
		members := room.Members
		if members == nil {
			members = []string{}
		}
		skipReason := ""
		if room.ID == V1SystemRoom {
			skipReason = "v1 system events are not imported"
		}
		preview := V1PreviewRoom{ID: room.ID, Messages: roomCount[room.ID], Members: members, SuggestedProject: alias, SuggestedSkip: skip, SkipReason: skipReason, MemoryDisabled: room.MemoryEnabled != nil && !*room.MemoryEnabled}
		preview.DM = isV1DM(room.ID)
		out.Rooms = append(out.Rooms, preview)
	}
	// The owner's v1 counterpart is the person with the owner's own name, or else the one person in the data at all: a single-person
	// install is the owner, and mapping them elsewhere would leave their read positions, memory profile, pins and reactions behind. With
	// several people there is no telling which is the owner (the busiest writer may be a colleague), so nobody is merged into the owner's
	// account without being asked (ResolvePeople gives each of them an account to start from, and the wizard says so).
	counterpart := ""
	if len(humans) == 1 {
		for human := range humans {
			counterpart = human
		}
	} else {
		// The exact spelling wins; otherwise the first of the case-variants in sorted order, so a rerun never flips the suggestion.
		var variants []string
		for human := range humans {
			if human == ownerLogin {
				variants = []string{human}
				break
			}
			if strings.EqualFold(human, ownerLogin) {
				variants = append(variants, human)
			}
		}
		if len(variants) > 0 {
			sort.Strings(variants)
			counterpart = variants[0]
		}
	}
	for human := range humans {
		out.Humans = append(out.Humans, human)
		out.Suggested.Humans[human] = "-"
		if human == counterpart {
			out.Suggested.Humans[human] = ownerLogin
		}
	}
	sort.Strings(out.Humans)
	out.HumanStakes = map[string]V1HumanStakes{}
	for _, human := range out.Humans {
		out.HumanStakes[human] = V1HumanStakes{}
	}
	for _, memory := range data.Memory {
		if stakes, ok := out.HumanStakes[memory.ScopeKey]; ok && memory.Scope == "user_profile" {
			stakes.MemoryProfiles++
			out.HumanStakes[memory.ScopeKey] = stakes
		}
	}
	for _, reactions := range data.Reactions {
		for _, reaction := range reactions {
			if stakes, ok := out.HumanStakes[reaction.AgentID]; ok {
				stakes.Reactions++
				out.HumanStakes[reaction.AgentID] = stakes
			}
		}
	}
	for id, agent := range data.Agents {
		stakes, ok := out.HumanStakes[id]
		if !ok {
			continue
		}
		for _, cursor := range agent.ReadCursors {
			if cursor > 0 {
				stakes.ReadPositions++
			}
		}
		out.HumanStakes[id] = stakes
	}
	for _, pref := range data.RoomPrefs {
		if stakes, ok := out.HumanStakes[pref.Agent]; ok && (pref.Hidden || pref.Pinned) {
			stakes.RoomView++
			out.HumanStakes[pref.Agent] = stakes
		}
	}
	for scope := range scopes {
		out.MemoryProjects = append(out.MemoryProjects, scope)
		out.Suggested.MemoryProjects[scope] = scope
	}
	sort.Strings(out.MemoryProjects)
	// Direct messages are private chats, so none is imported unless the owner ticks it. Each still names the project its AI
	// participant belongs to (when the wizard proposes it), so ticking one needs no further choice, and each is open.
	for i, room := range data.Rooms {
		if !isV1DM(room.ID) {
			continue
		}
		spec := out.Suggested.Rooms[room.ID]
		spec.Project = v1DMSuggestedProject(room, out.Suggested.Projects, spec.Project)
		spec.Lifecycle = "open"
		spec.Skip = true
		out.Suggested.Rooms[room.ID] = spec
		for j := range out.Rooms {
			if out.Rooms[j].ID == data.Rooms[i].ID {
				out.Rooms[j].SuggestedProject = spec.Project
				out.Rooms[j].SuggestedSkip = spec.Skip
				out.Rooms[j].Title = v1DMTitle(room, data.Agents, out.Suggested.Humans, ownerLogin)
				for _, member := range v1DMMembers(room) {
					out.Rooms[j].Participants = append(out.Rooms[j].Participants, V1DMParticipant{ID: member, Name: v1DMName(member, data.Agents), Kind: v1ImportKind(member, data.Agents)})
				}
			}
		}
	}
	for project, folder := range v1WorkspaceSuggestions(sourceDir) {
		if spec, ok := out.Suggested.Projects[project]; ok {
			spec.Folder = folder
			out.Suggested.Projects[project] = spec
		}
	}
	suggestSiblingFolders(out.Suggested.Projects)
	if out.V1MemoryEnabled {
		// v1 memory was on, so each destination project starts with v2 memory on too; the owner can untick any.
		out.Suggested.EnableProjectMemory = map[string]bool{}
		for alias := range out.Suggested.Projects {
			out.Suggested.EnableProjectMemory[alias] = true
		}
	}
	if out.Counts["rooms"] == 0 {
		return out, ErrV1NoRooms
	}
	return out, nil
}

// A project's workspace folder is the working directory most of its v1 agents ran from: sessions in the registry and the cwd each
// agent registered with. A tie goes to the shorter, then the alphabetically first path, so the suggestion is stable. Only
// absolute paths count; a project with none is left blank for the owner to fill in.
func v1WorkspaceSuggestions(sourceDir string) map[string]string {
	counts := map[string]map[string]int{}
	add := func(project, cwd string) {
		if project == "" || !filepath.IsAbs(cwd) {
			return
		}
		if counts[project] == nil {
			counts[project] = map[string]int{}
		}
		counts[project][filepath.Clean(cwd)]++
	}
	path := filepath.Join(sourceDir, "server", "aimebu.sqlite")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		path = filepath.Join(sourceDir, "aimebu.sqlite")
	}
	if db, err := sql.Open("sqlite", sqliteutil.DSN(path, true)); err == nil {
		scan := func(query string, each func(raw string)) {
			var rows *sql.Rows
			if err := sqliteutil.Retry(func() (queryErr error) { rows, queryErr = db.Query(query); return queryErr }); err != nil {
				return
			}
			for rows.Next() {
				var raw string
				if rows.Scan(&raw) == nil {
					each(raw)
				}
			}
			rows.Close()
		}
		scan("SELECT data FROM agent_sessions", func(raw string) {
			var item struct {
				Project string `json:"project"`
				CWD     string `json:"cwd"`
			}
			if json.Unmarshal([]byte(raw), &item) == nil {
				add(item.Project, item.CWD)
			}
		})
		scan("SELECT data FROM agents", func(raw string) {
			var item struct {
				Project string                     `json:"project"`
				Meta    map[string]json.RawMessage `json:"meta"`
			}
			// An odd meta value (not a string) only costs that agent's suggestion, never the preview.
			if json.Unmarshal([]byte(raw), &item) == nil {
				var cwd string
				if json.Unmarshal(item.Meta["cwd"], &cwd) == nil {
					add(item.Project, cwd)
				}
			}
		})
		db.Close()
	}
	if len(counts) == 0 {
		if raw, err := os.ReadFile(filepath.Join(sourceDir, "agents", "agent-sessions.json")); err == nil {
			var items []struct {
				CWD string `json:"cwd"`
			}
			if json.Unmarshal(raw, &items) == nil {
				for _, item := range items {
					add(filepath.Base(item.CWD), item.CWD)
				}
			}
		}
	}
	result := map[string]string{}
	for project, folders := range counts {
		best, bestCount := "", 0
		for folder, n := range folders {
			if n > bestCount || n == bestCount && (len(folder) < len(best) || len(folder) == len(best) && folder < best) {
				best, bestCount = folder, n
			}
		}
		result[project] = best
	}
	return result
}

// V1ImportDigest is used as the short-lived dry-run receipt for the wizard.
func V1ImportDigest(mapping V1ImportMap) string { return v1ImportDigest(mapping) }

func (s *Store) V1ImportedMacroKeys(ctx context.Context, source string) ([]string, error) {
	return s.v1ImportedKeys(ctx, source, "macro")
}

func (s *Store) V1ImportedRoleKeys(ctx context.Context, source string) ([]string, error) {
	return s.v1ImportedKeys(ctx, source, "role")
}

func (s *Store) v1ImportedKeys(ctx context.Context, source, kind string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT old_id FROM legacy_import_map WHERE source_key=? AND entity_kind=? ORDER BY old_id`, source, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) V1ImportLedgerCount(ctx context.Context, source string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM legacy_import_map WHERE source_key=?", source).Scan(&count)
	return count, err
}

func (s *Store) V1ImportOwnerLogin(ctx context.Context, ownerID string) (string, error) {
	var login string
	err := s.db.QueryRowContext(ctx, "SELECT login FROM user WHERE id=?", ownerID).Scan(&login)
	return login, err
}

func (s *Store) V1ImportUserLogins(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT login FROM user ORDER BY login")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logins := []string{}
	for rows.Next() {
		var login string
		if err := rows.Scan(&login); err != nil {
			return nil, err
		}
		logins = append(logins, login)
	}
	return logins, rows.Err()
}

func (s *Store) V1ImportSavedMap(ctx context.Context, source string) (V1ImportMap, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT map_json FROM legacy_import_config WHERE source_key=?", source).Scan(&raw)
	if err == sql.ErrNoRows {
		return V1ImportMap{}, false, nil
	}
	if err != nil {
		return V1ImportMap{}, false, err
	}
	var mapping V1ImportMap
	if err := json.Unmarshal([]byte(raw), &mapping); err != nil {
		return V1ImportMap{}, false, err
	}
	return mapping, true, nil
}

// ResolvePeople gives every v1 person who is not the owner an account to start from (a viewer named after them, for the owner to keep, change
// or skip), which is what the wizard does by default; the preview answers it so a client of the API sees the same suggestion. It needs the
// accounts that exist already, so it runs once the preview knows them.
func (p *V1ImportPreview) ResolvePeople() {
	taken := map[string]bool{strings.ToLower(p.Suggested.OwnerLogin): true}
	for _, login := range p.DestinationUsers {
		taken[strings.ToLower(login)] = true
	}
	for _, human := range p.Humans {
		if p.Suggested.Humans[human] != "-" {
			continue
		}
		login := v1SuggestedLogin(human, taken)
		taken[login] = true
		if p.Suggested.NewUsers == nil {
			p.Suggested.NewUsers = map[string]V1NewUser{}
		}
		p.Suggested.NewUsers[human] = V1NewUser{Login: login, Role: "viewer"}
		p.Suggested.Humans[human] = login
	}
}

// v1SuggestedLogin is a login for a new account from a v1 name: lowercase, 3 to 32 characters, not taken. The wizard derives it the same way
// (newLogin in import-v1.js).
func v1SuggestedLogin(name string, taken map[string]bool) string {
	var b strings.Builder
	dash := false
	for _, ch := range strings.ToLower(name) {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-' {
			b.WriteRune(ch)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	base := b.String()
	for base != "" && (base[0] < 'a' || base[0] > 'z') {
		base = base[1:]
	}
	if len(base) > 28 {
		base = base[:28]
	}
	if len(base) < 3 {
		base = strings.TrimSuffix("user-"+base, "-")
	}
	if len(base) < 3 {
		base = "user"
	}
	login := base
	for n := 2; taken[login]; n++ {
		login = fmt.Sprintf("%s-%d", base, n)
	}
	return login
}

// suggestSiblingFolders looks for a project whose folder v1 never recorded (its agents have left the registry) next to the folders v1 does
// know: v1 named a project after its folder and projects mostly sit side by side (.../a/x, .../b/y). A candidate is an existing directory
// of the project's name beside a known one (the cutover is on the same machine); when several exist, the parent that most known folders
// share wins, then the alphabetically first. A project with no such directory stays blank for the owner to fill in.
func suggestSiblingFolders(projects map[string]V1ImportProject) {
	parents := map[string]int{}
	for _, project := range projects {
		if project.Folder != "" {
			parents[filepath.Dir(project.Folder)]++
		}
	}
	if len(parents) == 0 {
		return
	}
	for alias, project := range projects {
		name := project.Name
		if project.Folder != "" || name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			continue
		}
		best, bestCount := "", 0
		for parent, count := range parents {
			candidate := filepath.Join(parent, name)
			if info, err := os.Stat(candidate); err != nil || !info.IsDir() {
				continue
			}
			if count > bestCount || (count == bestCount && candidate < best) {
				best, bestCount = candidate, count
			}
		}
		if best != "" {
			project.Folder = best
			projects[alias] = project
		}
	}
}

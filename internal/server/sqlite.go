package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
	_ "modernc.org/sqlite"
)

const sqliteSchemaVersion = 1

func (s *store) sqlitePath() string {
	return filepath.Join(s.dir, "aimebu.sqlite")
}

func (s *store) openSQLite() error {
	path := s.sqlitePath()
	_, statErr := os.Stat(path)
	fresh := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !fresh {
		return fmt.Errorf("stat sqlite db: %w", statErr)
	}

	if fresh {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("create sqlite db: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close sqlite db: %w", err)
		}
	} else if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod sqlite db: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open sqlite db: %w", err)
	}
	s.db = db

	legacyCore := s.hasLegacyCoreJSON()
	if err := s.configureSQLite(!legacyCore); err != nil {
		db.Close()
		s.db = nil
		removeSQLiteFiles(path)
		return err
	}
	if legacyCore {
		if err := s.importLegacyCoreJSON(); err != nil {
			db.Close()
			s.db = nil
			removeSQLiteFiles(path)
			return err
		}
	}
	if err := s.importLegacyRemainingJSON(); err != nil {
		db.Close()
		s.db = nil
		return err
	}
	return nil
}

func (s *store) configureSQLite(seedSchemaMeta bool) error {
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
	}
	for _, q := range pragmas {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("sqlite %s: %w", q, err)
		}
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS schema_meta (version INTEGER NOT NULL, applied_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS rooms (id TEXT PRIMARY KEY, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS messages (id INTEGER PRIMARY KEY, room_id TEXT NOT NULL, data TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_room_id ON messages(room_id, id)`,
		`CREATE TABLE IF NOT EXISTS agents (id TEXT PRIMARY KEY, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS reactions (message_id INTEGER PRIMARY KEY, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY CHECK(id=1), data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS memory (id TEXT PRIMARY KEY, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS leaderboards (seq INTEGER PRIMARY KEY AUTOINCREMENT, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS macros (key TEXT PRIMARY KEY, value TEXT NOT NULL, seen_default INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS fleets (name TEXT PRIMARY KEY, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS prompts (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS role_overrides (key TEXT PRIMARY KEY, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS role_custom (key TEXT PRIMARY KEY, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sounds (id TEXT PRIMARY KEY, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS attachments (id TEXT PRIMARY KEY, data TEXT NOT NULL)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("sqlite schema: %w", err)
		}
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_meta`).Scan(&count); err != nil {
		return fmt.Errorf("read schema_meta: %w", err)
	}
	if count == 0 {
		if !seedSchemaMeta {
			return nil
		}
		if _, err := s.db.Exec(`INSERT INTO schema_meta(version, applied_at) VALUES (?, ?)`, sqliteSchemaVersion, now()); err != nil {
			return fmt.Errorf("write schema_meta: %w", err)
		}
		return nil
	}
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_meta ORDER BY rowid DESC LIMIT 1`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > sqliteSchemaVersion {
		return fmt.Errorf("sqlite schema version %d is newer than this binary supports (%d)", version, sqliteSchemaVersion)
	}
	return nil
}

func (s *store) hasLegacyCoreJSON() bool {
	for _, name := range []string{"rooms.json", "messages.json", "agents.json", "reactions.json", "settings.json"} {
		if _, err := os.Stat(filepath.Join(s.dir, name)); err == nil {
			var count int
			if s.db != nil && s.db.QueryRow(`SELECT COUNT(*) FROM schema_meta`).Scan(&count) == nil && count > 0 {
				return false
			}
			return true
		}
	}
	return false
}

func removeSQLiteFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

func (s *store) importLegacyCoreJSON() error {
	paths := []string{"rooms.json", "messages.json", "agents.json", "reactions.json", "settings.json"}
	legacy := false
	for _, name := range paths {
		if _, err := os.Stat(filepath.Join(s.dir, name)); err == nil {
			legacy = true
			break
		}
	}
	if !legacy {
		return nil
	}

	var rooms []types.Room
	var messages []types.Message
	var agents []types.Agent
	var reactions map[int64][]types.Reaction
	var settings *Settings

	if err := readLegacyJSON(filepath.Join(s.dir, "rooms.json"), &rooms); err != nil {
		return err
	}
	if err := readLegacyJSON(filepath.Join(s.dir, "messages.json"), &messages); err != nil {
		return err
	}
	if err := readLegacyJSON(filepath.Join(s.dir, "agents.json"), &agents); err != nil {
		return err
	}
	var reactionsEnv struct {
		Reactions map[int64][]types.Reaction `json:"reactions"`
	}
	if err := readLegacyJSON(filepath.Join(s.dir, "reactions.json"), &reactionsEnv); err != nil {
		return err
	}
	if reactionsEnv.Reactions != nil {
		reactions = reactionsEnv.Reactions
	}
	var set Settings
	if err := readLegacyJSON(filepath.Join(s.dir, "settings.json"), &set); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(s.dir, "settings.json")); err == nil {
		settings = &set
	}

	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("begin legacy import: %w", err)
	}
	if err := importCoreTx(tx, rooms, messages, agents, reactions, settings); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := validateCoreImportTx(tx, len(rooms), len(messages), len(agents), len(reactions), settings != nil); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT INTO schema_meta(version, applied_at) VALUES (?, ?)`, sqliteSchemaVersion, now()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("write schema_meta: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy import: %w", err)
	}
	if err := moveLegacyJSONFiles(s.dir, append(paths, "schema.json")); err != nil {
		return fmt.Errorf("archive legacy json: %w", err)
	}
	return nil
}

func (s *store) importLegacyRemainingJSON() error {
	topLevel := []string{"memory.json", "leaderboards.json", "macros.json", "fleet.json", "prompts.json", "roles.json"}
	hasAny := false
	for _, name := range topLevel {
		if _, err := os.Stat(filepath.Join(s.dir, name)); err == nil {
			hasAny = true
			break
		}
	}
	for _, path := range []string{s.soundsIndexPath(), s.attachmentsIndexPath()} {
		if _, err := os.Stat(path); err == nil {
			hasAny = true
		}
	}
	if !hasAny {
		return nil
	}

	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("begin remaining legacy import: %w", err)
	}
	if err := s.importRemainingTx(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit remaining legacy import: %w", err)
	}
	if err := moveLegacyJSONFiles(s.dir, topLevel); err != nil {
		return fmt.Errorf("archive remaining legacy json: %w", err)
	}
	_ = os.Remove(s.soundsIndexPath())
	_ = os.Remove(s.attachmentsIndexPath())
	return nil
}

func (s *store) importRemainingTx(tx *sql.Tx) error {
	var memory memoryEnvelope
	if err := readLegacyJSON(filepath.Join(s.dir, "memory.json"), &memory); err != nil {
		return err
	}
	for _, record := range memory.Records {
		if record.ID == "" {
			continue
		}
		if err := txInsertJSON(tx, "memory", "id", record.ID, record); err != nil {
			return err
		}
	}

	var leaderboards leaderboardsEnvelope
	if err := readLegacyJSON(filepath.Join(s.dir, "leaderboards.json"), &leaderboards); err != nil {
		return err
	}
	for i, card := range leaderboards.Cards {
		if err := txInsertJSON(tx, "leaderboards", "seq", fmt.Sprintf("%012d", i+1), card); err != nil {
			return err
		}
	}

	if err := s.importLegacyMacrosTx(tx); err != nil {
		return err
	}

	var fleets fleetsEnvelope
	if err := readLegacyJSON(filepath.Join(s.dir, "fleet.json"), &fleets); err != nil {
		return err
	}
	fleets = normalizeFleetEnvelope(fleets)
	for name, fleet := range fleets.Fleets {
		if err := txInsertJSON(tx, "fleets", "name", name, fleet); err != nil {
			return err
		}
	}

	var prompts promptsEnvelope
	if err := readLegacyJSON(filepath.Join(s.dir, "prompts.json"), &prompts); err != nil {
		return err
	}
	for key, value := range prompts.Prompts {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO prompts(key, value) VALUES(?, ?)`, key, value); err != nil {
			return err
		}
	}

	if err := s.importLegacyRolesTx(tx); err != nil {
		return err
	}

	var sounds struct {
		Sounds []SoundEntry `json:"sounds"`
	}
	if err := readLegacyJSON(s.soundsIndexPath(), &sounds); err != nil {
		return err
	}
	for _, entry := range sounds.Sounds {
		if entry.UUID == "" {
			continue
		}
		if err := txInsertJSON(tx, "sounds", "id", entry.UUID, entry); err != nil {
			return err
		}
	}

	var attachments struct {
		Attachments []AttachmentEntry `json:"attachments"`
	}
	if err := readLegacyJSON(s.attachmentsIndexPath(), &attachments); err != nil {
		return err
	}
	for _, entry := range attachments.Attachments {
		if entry.ID == "" {
			continue
		}
		if err := txInsertJSON(tx, "attachments", "id", entry.ID, entry); err != nil {
			return err
		}
	}

	return nil
}

func (s *store) importLegacyMacrosTx(tx *sql.Tx) error {
	data, err := os.ReadFile(filepath.Join(s.dir, "macros.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	macros := make(map[string]string)
	seen := make(map[string]bool)
	var env macrosEnvelope
	if json.Unmarshal(data, &env) == nil && env.Macros != nil {
		for k, v := range env.Macros {
			macros[k] = v
		}
		for _, k := range env.SeenDefaults {
			seen[k] = true
		}
		roomIDs := make([]string, 0, len(env.Rooms))
		for roomID := range env.Rooms {
			roomIDs = append(roomIDs, roomID)
		}
		sort.Strings(roomIDs)
		for _, roomID := range roomIDs {
			rm := env.Rooms[roomID]
			keys := make([]string, 0, len(rm))
			for k := range rm {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if _, exists := macros[k]; !exists {
					macros[k] = rm[k]
				}
			}
		}
	} else {
		var old map[string]map[string]string
		if err := json.Unmarshal(data, &old); err != nil {
			return err
		}
		agentIDs := make([]string, 0, len(old))
		for k := range old {
			agentIDs = append(agentIDs, k)
		}
		sort.Strings(agentIDs)
		for _, aid := range agentIDs {
			for k, v := range old[aid] {
				if _, exists := macros[k]; !exists {
					macros[k] = v
				}
			}
		}
	}
	keys := make(map[string]bool, len(macros)+len(seen))
	for k := range macros {
		keys[k] = true
	}
	for k := range seen {
		keys[k] = true
	}
	for k := range keys {
		seenDefault := 0
		if seen[k] {
			seenDefault = 1
		}
		if _, err := tx.Exec(`INSERT INTO macros(key, value, seen_default) VALUES(?, ?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, seen_default=excluded.seen_default`, k, macros[k], seenDefault); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) importLegacyRolesTx(tx *sql.Tx) error {
	var env rolesEnvelope
	if err := readLegacyJSON(filepath.Join(s.dir, "roles.json"), &env); err != nil {
		return err
	}
	for key, raw := range env.Overrides {
		var body string
		var entry roleOverrideEntry
		if json.Unmarshal(raw, &body) == nil {
			entry = roleOverrideEntry{Body: body}
		} else if err := json.Unmarshal(raw, &entry); err != nil {
			return err
		}
		if err := txInsertJSON(tx, "role_overrides", "key", key, entry); err != nil {
			return err
		}
	}
	for key, entry := range env.Custom {
		if err := txInsertJSON(tx, "role_custom", "key", key, entry); err != nil {
			return err
		}
	}
	return nil
}

func txInsertJSON(tx *sql.Tx, table, keyColumn, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR REPLACE INTO `+table+`(`+keyColumn+`, data) VALUES(?, ?)`, key, string(data))
	return err
}

func readLegacyJSON(path string, dst any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func importCoreTx(tx *sql.Tx, rooms []types.Room, messages []types.Message, agents []types.Agent, reactions map[int64][]types.Reaction, settings *Settings) error {
	for i := range rooms {
		data, err := json.Marshal(rooms[i])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO rooms(id, data) VALUES(?, ?)`, rooms[i].ID, string(data)); err != nil {
			return fmt.Errorf("import room %q: %w", rooms[i].ID, err)
		}
	}
	for i := range messages {
		data, err := json.Marshal(messages[i])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO messages(id, room_id, data) VALUES(?, ?, ?)`, messages[i].ID, messages[i].RoomID, string(data)); err != nil {
			return fmt.Errorf("import message %d: %w", messages[i].ID, err)
		}
	}
	for i := range agents {
		agents[i].State = ""
		agents[i].StateAt = time.Time{}
		data, err := json.Marshal(agents[i])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO agents(id, data) VALUES(?, ?)`, agents[i].ID, string(data)); err != nil {
			return fmt.Errorf("import agent %q: %w", agents[i].ID, err)
		}
	}
	for messageID, rows := range reactions {
		if len(rows) == 0 {
			continue
		}
		data, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO reactions(message_id, data) VALUES(?, ?)`, messageID, string(data)); err != nil {
			return fmt.Errorf("import reactions %d: %w", messageID, err)
		}
	}
	if settings != nil {
		data, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO settings(id, data) VALUES(1, ?)`, string(data)); err != nil {
			return fmt.Errorf("import settings: %w", err)
		}
	}
	return nil
}

func validateCoreImportTx(tx *sql.Tx, rooms, messages, agents, reactions int, hasSettings bool) error {
	checks := []struct {
		table string
		want  int
	}{
		{"rooms", rooms},
		{"messages", messages},
		{"agents", agents},
		{"reactions", reactions},
	}
	for _, check := range checks {
		var got int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + check.table).Scan(&got); err != nil {
			return fmt.Errorf("validate %s: %w", check.table, err)
		}
		if got != check.want {
			return fmt.Errorf("validate %s: got %d rows, want %d", check.table, got, check.want)
		}
	}
	var settingsCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM settings`).Scan(&settingsCount); err != nil {
		return fmt.Errorf("validate settings: %w", err)
	}
	if hasSettings && settingsCount != 1 {
		return fmt.Errorf("validate settings: got %d rows, want 1", settingsCount)
	}
	return nil
}

func moveLegacyJSONFiles(dir string, names []string) error {
	oldDir := filepath.Join(dir, ".old")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		return err
	}
	for _, name := range names {
		src := filepath.Join(dir, name)
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		dst := filepath.Join(oldDir, name)
		_ = os.Remove(dst)
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) loadCoreSQLite() error {
	rows, err := s.db.Query(`SELECT data FROM rooms ORDER BY id`)
	if err != nil {
		return fmt.Errorf("load rooms: %w", err)
	}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			rows.Close()
			return err
		}
		var room types.Room
		if err := json.Unmarshal([]byte(data), &room); err != nil {
			rows.Close()
			return fmt.Errorf("parse room: %w", err)
		}
		s.rooms[room.ID] = &room
	}
	if err := rows.Close(); err != nil {
		return err
	}

	rows, err = s.db.Query(`SELECT data FROM messages ORDER BY id`)
	if err != nil {
		return fmt.Errorf("load messages: %w", err)
	}
	var maxID int64
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			rows.Close()
			return err
		}
		var msg types.Message
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			rows.Close()
			return fmt.Errorf("parse message: %w", err)
		}
		s.messages[msg.RoomID] = append(s.messages[msg.RoomID], msg)
		if msg.ID > maxID {
			maxID = msg.ID
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	s.nextID.Store(maxID)

	rows, err = s.db.Query(`SELECT data FROM agents ORDER BY id`)
	if err != nil {
		return fmt.Errorf("load agents: %w", err)
	}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			rows.Close()
			return err
		}
		var agent types.Agent
		if err := json.Unmarshal([]byte(data), &agent); err != nil {
			rows.Close()
			return fmt.Errorf("parse agent: %w", err)
		}
		agent.State = ""
		agent.StateAt = time.Time{}
		s.agents[agent.ID] = &agent
		if isReservedAgentName(agent.Name) {
			log.Printf("warning: persisted agent %q has reserved name %q; @%s now resolves to a group token, not this agent", agent.ID, agent.Name, agent.Name)
		}
	}
	return rows.Close()
}

func (s *store) persistCoreSQLiteLocked() error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM rooms`); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM messages`); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM agents`); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, room := range s.rooms {
		data, err := json.Marshal(room)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO rooms(id, data) VALUES(?, ?)`, room.ID, string(data)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	var roomIDs []string
	for roomID := range s.messages {
		roomIDs = append(roomIDs, roomID)
	}
	sort.Strings(roomIDs)
	for _, roomID := range roomIDs {
		for _, msg := range s.messages[roomID] {
			data, err := json.Marshal(msg)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			if _, err := tx.Exec(`INSERT INTO messages(id, room_id, data) VALUES(?, ?, ?)`, msg.ID, msg.RoomID, string(data)); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
	}
	for _, agent := range s.agents {
		cp := *agent
		cp.State = ""
		cp.StateAt = time.Time{}
		data, err := json.Marshal(cp)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO agents(id, data) VALUES(?, ?)`, cp.ID, string(data)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *store) insertMessageSQLiteLocked(msg types.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO messages(id, room_id, data) VALUES(?, ?, ?)`, msg.ID, msg.RoomID, string(data))
	return err
}

func (s *store) loadReactionsSQLite() (bool, error) {
	rows, err := s.db.Query(`SELECT message_id, data FROM reactions ORDER BY message_id`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	loaded := false
	s.reactionsMu.Lock()
	defer s.reactionsMu.Unlock()
	for rows.Next() {
		var messageID int64
		var data string
		if err := rows.Scan(&messageID, &data); err != nil {
			return false, err
		}
		var reactions []types.Reaction
		if err := json.Unmarshal([]byte(data), &reactions); err != nil {
			return false, err
		}
		if len(reactions) > 0 {
			s.reactions[messageID] = reactions
			loaded = true
		}
	}
	return loaded, rows.Err()
}

func (s *store) persistReactionsSQLiteLocked() error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM reactions`); err != nil {
		_ = tx.Rollback()
		return err
	}
	for id, rows := range s.reactions {
		if len(rows) == 0 {
			continue
		}
		data, err := json.Marshal(rows)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO reactions(message_id, data) VALUES(?, ?)`, id, string(data)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *store) persistReactionSQLiteLocked(messageID int64) error {
	rows := s.reactions[messageID]
	if len(rows) == 0 {
		_, err := s.db.Exec(`DELETE FROM reactions WHERE message_id=?`, messageID)
		return err
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO reactions(message_id, data) VALUES(?, ?) ON CONFLICT(message_id) DO UPDATE SET data=excluded.data`, messageID, string(data))
	return err
}

func (s *store) loadSettingsSQLite() (bool, error) {
	var data string
	err := s.db.QueryRow(`SELECT data FROM settings WHERE id=1`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if err := json.Unmarshal([]byte(data), &s.settings); err != nil {
		return false, err
	}
	return true, nil
}

func (s *store) persistSettingsSQLiteLocked() error {
	data, err := json.Marshal(s.settings)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO settings(id, data) VALUES(1, ?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, string(data))
	return err
}

func (s *store) loadMacrosSQLite() error {
	rows, err := s.db.Query(`SELECT key, value, seen_default FROM macros ORDER BY key`)
	if err != nil {
		return err
	}
	defer rows.Close()
	macros := make(map[string]string)
	seen := make(map[string]bool)
	for rows.Next() {
		var key, value string
		var seenDefault int
		if err := rows.Scan(&key, &value, &seenDefault); err != nil {
			return err
		}
		macros[key] = value
		if seenDefault != 0 {
			seen[key] = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.macrosMu.Lock()
	s.macros = macros
	s.seenDefaults = seen
	s.macrosMu.Unlock()
	return nil
}

func (s *store) persistMacrosSQLiteLocked() error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM macros`); err != nil {
		_ = tx.Rollback()
		return err
	}
	keys := make(map[string]bool, len(s.macros)+len(s.seenDefaults))
	for key := range s.macros {
		keys[key] = true
	}
	for key := range s.seenDefaults {
		keys[key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		value := s.macros[key]
		seenDefault := 0
		if s.seenDefaults[key] {
			seenDefault = 1
		}
		if _, err := tx.Exec(`INSERT INTO macros(key, value, seen_default) VALUES(?, ?, ?)`, key, value, seenDefault); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *store) replaceJSONRows(table, keyColumn string, rows map[string]any) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
		_ = tx.Rollback()
		return err
	}
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		data, err := json.Marshal(rows[key])
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO `+table+`(`+keyColumn+`, data) VALUES(?, ?)`, key, string(data)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *store) replacePromptRows(rows map[string]any) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM prompts`); err != nil {
		_ = tx.Rollback()
		return err
	}
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, _ := rows[key].(string)
		if _, err := tx.Exec(`INSERT INTO prompts(key, value) VALUES(?, ?)`, key, value); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *store) loadPromptRows(apply func(string, string) error) error {
	rows, err := s.db.Query(`SELECT key, value FROM prompts ORDER BY key`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		if err := apply(key, value); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *store) loadJSONRows(table, keyColumn string, apply func(string, []byte) error) error {
	rows, err := s.db.Query(`SELECT ` + keyColumn + `, data FROM ` + table + ` ORDER BY ` + keyColumn)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, data string
		if err := rows.Scan(&key, &data); err != nil {
			return err
		}
		if err := apply(key, []byte(data)); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *store) clearTable(table string) error {
	_, err := s.db.Exec(`DELETE FROM ` + table)
	return err
}

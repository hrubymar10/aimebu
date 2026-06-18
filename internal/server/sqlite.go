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

	if err := s.configureSQLite(); err != nil {
		db.Close()
		s.db = nil
		if fresh {
			_ = os.Remove(path)
		}
		return err
	}
	if fresh {
		if err := s.importLegacyCoreJSON(); err != nil {
			db.Close()
			s.db = nil
			_ = os.Remove(path)
			return err
		}
	}
	return nil
}

func (s *store) configureSQLite() error {
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy import: %w", err)
	}
	if err := moveLegacyJSONFiles(s.dir, append(paths, "schema.json")); err != nil {
		return fmt.Errorf("archive legacy json: %w", err)
	}
	return nil
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

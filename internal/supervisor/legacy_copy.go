package supervisor

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/runtimeignore"
	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
	_ "modernc.org/sqlite"
)

// copyLegacySession runs on the machine that owns the native harness state. It stages only the selected v1 session in the agent's private
// runtime, before the first resumed turn. A marker makes redelivery harmless; sources are opened read-only and are never renamed.
var legacySessionHandlePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (c *Client) copyLegacySession(ctx context.Context, d protocol.Delivery, containerized bool) error {
	if !d.ImportLegacySession {
		return nil
	}
	if containerized || !d.NativeSessionStarted || d.NativeSessionHandle == "" || c.ConfigDir == "" {
		return errors.New("v1 session copy requires a native resumed turn and a supervisor config directory")
	}
	if !legacySessionHandlePattern.MatchString(d.NativeSessionHandle) || strings.Contains(d.NativeSessionHandle, "..") {
		return errors.New("v1 session handle contains invalid characters")
	}
	sum := sha256.Sum256([]byte(d.AgentID))
	agentHash := hex.EncodeToString(sum[:])
	var private string
	if d.Agent.Tool == "pi" {
		private = filepath.Join(d.CheckoutPath, ".aimebu", "pi-sessions", agentHash)
		if err := runtimeignore.Ensure(d.CheckoutPath); err != nil {
			return err
		}
	} else {
		private = filepath.Join(c.ConfigDir, "supervisor", d.Agent.Tool, agentHash)
	}
	if err := os.MkdirAll(private, 0700); err != nil {
		return err
	}
	if err := os.Chmod(private, 0700); err != nil {
		return err
	}
	handleHash := sha256.Sum256([]byte(d.NativeSessionHandle))
	marker := filepath.Join(private, ".v1-session-"+hex.EncodeToString(handleHash[:8])+".ready")
	if prior, err := os.ReadFile(marker); err == nil {
		if string(prior) == d.NativeSessionHandle+"\n" {
			return nil
		}
		return errors.New("private session marker does not match the requested session")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var err error
	switch d.Agent.Tool {
	case "codex":
		err = copyCodexSession(d.NativeSessionHandle, private)
	case "pi":
		err = copyPiSession(d.NativeSessionHandle, private)
	case "opencode":
		err = copyOpenCodeSession(ctx, d.NativeSessionHandle, private)
	case "vibe":
		err = copyVibeSession(d.NativeSessionHandle, private)
	default:
		err = fmt.Errorf("v1 session copying is unavailable for %s", d.Agent.Tool)
	}
	if err != nil {
		return fmt.Errorf("copy v1 %s session %s: %w", d.Agent.Tool, d.NativeSessionHandle, err)
	}
	return writeLegacyMarker(marker, d.NativeSessionHandle)
}

func writeLegacyMarker(path, handle string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".v1-marker-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.WriteString(handle + "\n"); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func userHomePath(env, suffix string) string {
	if path := os.Getenv(env); path != "" {
		return path
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, suffix)
}

func findLegacyFile(root, suffix string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), suffix) {
			if found != "" {
				return errors.New("more than one native session file matches the id")
			}
			found = path
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", os.ErrNotExist
	}
	return found, nil
}

func copyCodexSession(handle, private string) error {
	sourceRoot := filepath.Join(userHomePath("CODEX_HOME", ".codex"), "sessions")
	source, err := findLegacyFile(sourceRoot, "-"+handle+".jsonl")
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(sourceRoot, source)
	if err != nil {
		return err
	}
	return copyLegacyFile(source, filepath.Join(private, "sessions", rel))
}

func copyPiSession(handle, private string) error {
	sourceRoot := os.Getenv("PI_CODING_AGENT_SESSION_DIR")
	if sourceRoot == "" {
		sourceRoot = filepath.Join(userHomePath("PI_CODING_AGENT_DIR", filepath.Join(".pi", "agent")), "sessions")
	}
	source, err := findLegacyFile(sourceRoot, "_"+handle+".jsonl")
	if err != nil {
		return err
	}
	return copyLegacyFile(source, filepath.Join(private, filepath.Base(source)))
}

func copyVibeSession(handle, private string) error {
	sourceRoot := os.Getenv("VIBE_SESSION_LOGGING__SAVE_DIR")
	if sourceRoot == "" {
		sourceRoot = filepath.Join(userHomePath("VIBE_HOME", ".vibe"), "sessions")
	}
	short := handle
	if len(short) > 8 {
		short = short[:8]
	}
	matches, err := filepath.Glob(filepath.Join(sourceRoot, "session_*_"+short))
	if err != nil {
		return err
	}
	var source string
	for _, candidate := range matches {
		data, err := os.ReadFile(filepath.Join(candidate, "meta.json"))
		if err != nil {
			continue
		}
		var meta struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(data, &meta) == nil && meta.SessionID == handle {
			if source != "" {
				return errors.New("more than one native session folder matches the id")
			}
			source = candidate
		}
	}
	if source == "" {
		return os.ErrNotExist
	}
	dest := filepath.Join(private, "sessions", filepath.Base(source))
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(dest, rel), 0700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("v1 session folder contains a non-regular file")
		}
		return copyLegacyFile(path, filepath.Join(dest, rel))
	})
}

func copyLegacyFile(source, dest string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("v1 session source is not a regular file")
	}
	if prior, err := os.Lstat(dest); err == nil {
		if !prior.Mode().IsRegular() {
			return errors.New("private session destination is not a regular file")
		}
		equal, err := sameLegacyFile(source, dest)
		if err != nil {
			return err
		}
		if equal {
			return nil // a previous copy finished before its ready marker was written
		}
		return errors.New("private session destination differs from v1 source")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".v1-copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

func sameLegacyFile(a, b string) (bool, error) {
	left, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer left.Close()
	right, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer right.Close()
	leftHash, rightHash := sha256.New(), sha256.New()
	leftSize, err := io.Copy(leftHash, left)
	if err != nil {
		return false, err
	}
	rightSize, err := io.Copy(rightHash, right)
	if err != nil {
		return false, err
	}
	return leftSize == rightSize && hex.EncodeToString(leftHash.Sum(nil)) == hex.EncodeToString(rightHash.Sum(nil)), nil
}

var openCodeSessionTables = map[string]bool{"migration": true, "project": true, "project_directory": true, "workspace": true, "session": true,
	"message": true, "part": true, "session_context_epoch": true, "session_input": true, "session_message": true,
	"todo": true, "session_share": true}

func copyOpenCodeSession(ctx context.Context, handle, private string) error {
	source := os.Getenv("OPENCODE_DB")
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = userHomePath("", filepath.Join(".local", "share"))
	}
	if source == "" {
		source = filepath.Join(dataHome, "opencode", "opencode.db")
	} else if !filepath.IsAbs(source) {
		// OpenCode resolves relative OPENCODE_DB values under its data directory.
		source = filepath.Join(dataHome, "opencode", source)
	}
	if info, err := os.Lstat(source); err != nil || !info.Mode().IsRegular() {
		if err != nil {
			return err
		}
		return errors.New("v1 database is not a regular file")
	}
	dest := filepath.Join(private, "opencode.db")
	if _, err := os.Stat(dest); err == nil {
		ready, err := preparedOpenCodeDB(ctx, dest, handle)
		if err != nil {
			return err
		}
		if ready {
			return nil // the snapshot was installed before its ready marker was written
		}
		return errors.New("private session database already exists with different sessions")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(private, ".v1-opencode-*")
	if err != nil {
		return err
	}
	path := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(path)
	readOnly, err := sql.Open("sqlite", sqliteutil.DSN(source, true))
	if err != nil {
		return err
	}
	defer readOnly.Close()
	if err := sqliteutil.Retry(func() error { _, err := readOnly.ExecContext(ctx, `VACUUM INTO ?`, path); return err }); err != nil {
		return err
	}
	copyDB, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
	if err != nil {
		return err
	}
	defer copyDB.Close()
	copyDB.SetMaxOpenConns(1)
	if _, err := copyDB.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return err
	}
	var projectID string
	if err := copyDB.QueryRowContext(ctx, `SELECT project_id FROM session WHERE id=?`, handle).Scan(&projectID); err != nil {
		return err
	}
	tx, err := copyDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM session WHERE id<>?`, handle); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM project WHERE id<>?`, projectID); err != nil {
		return err
	}
	// Only the selected session and the schema it needs may enter the agent's database. Everything else, including accounts and credentials,
	// is removed from the snapshot before it is installed in the private runtime.
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return err
	}
	var clear []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if !openCodeSessionTables[name] {
			clear = append(clear, name)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, table := range clear {
		if _, err := tx.ExecContext(ctx, `DELETE FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// DELETE leaves old pages on SQLite's freelist; compact the private snapshot so other sessions are not recoverable from it.
	if _, err := copyDB.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	fkRows, err := copyDB.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	broken := fkRows.Next()
	err = fkRows.Err()
	fkRows.Close()
	if err != nil {
		return err
	}
	if broken {
		return errors.New("copied database has broken references")
	}
	if err := copyDB.Close(); err != nil {
		return err
	}
	return os.Rename(path, dest)
}

func preparedOpenCodeDB(ctx context.Context, path, handle string) (bool, error) {
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, true))
	if err != nil {
		return false, err
	}
	defer db.Close()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session`).Scan(&count); err != nil || count != 1 {
		return false, err
	}
	var id string
	if err := db.QueryRowContext(ctx, `SELECT id FROM session LIMIT 1`).Scan(&id); err != nil {
		return false, err
	}
	if id != handle {
		return false, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if openCodeSessionTables[name] {
			continue
		}
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+strings.ReplaceAll(name, `"`, `""`)+`"`).Scan(&count); err != nil || count != 0 {
			return false, err
		}
	}
	return true, rows.Err()
}

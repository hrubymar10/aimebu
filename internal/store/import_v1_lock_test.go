package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/sqliteutil"
)

// The v1 source openers read a copy that another process may be opening at that moment. They wait out a brief exclusive
// lock instead of reporting "database is locked".
func TestV1SourceOpenersWaitOutABriefLock(t *testing.T) {
	source := t.TempDir()
	path := filepath.Join(source, "server", "aimebu.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE agent_sessions(data TEXT)`,
		`INSERT INTO agent_sessions VALUES ('{"project":"p","cwd":"` + filepath.ToSlash(source) + `"}')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	holder, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		_, _ = holder.ExecContext(context.Background(), `COMMIT`)
		_ = holder.Close()
		_ = db.Close()
	}()
	// The workspace suggestions read agent_sessions; a lock must not make them silently empty.
	if suggestions := v1WorkspaceSuggestions(source); suggestions["p"] == "" {
		t.Fatalf("the workspace suggestions came back empty during a brief lock: %v", suggestions)
	}
	// The import source reads further: here it stops at the missing v1 schema, and that must be the reported reason, not a lock.
	_, err = OpenV1ImportSource(context.Background(), source)
	if err == nil || strings.Contains(err.Error(), "locked") || strings.Contains(err.Error(), "busy") {
		t.Fatalf("OpenV1ImportSource = %v, want the schema error, not a lock", err)
	}
}

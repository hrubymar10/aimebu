package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/sqliteutil"
	"github.com/hrubymar10/aimebu/internal/store"
)

func v1BackupWithMessage(t *testing.T, root string, id int) {
	t.Helper()
	dir := filepath.Join(root, v1BackupName, "server")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(dir, "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		"CREATE TABLE schema_meta(version INTEGER)", "INSERT INTO schema_meta VALUES (1)",
		"CREATE TABLE rooms(id TEXT PRIMARY KEY,data TEXT)", "CREATE TABLE messages(id INTEGER PRIMARY KEY,room_id TEXT,data TEXT)",
		"CREATE TABLE agents(id TEXT PRIMARY KEY,data TEXT)", "CREATE TABLE reactions(message_id INTEGER PRIMARY KEY,data TEXT)",
		"CREATE TABLE memory(id TEXT PRIMARY KEY,data TEXT)", "CREATE TABLE attachments(id TEXT PRIMARY KEY,data TEXT)",
		`INSERT INTO rooms(id,data) VALUES ('work','{"id":"work","members":["alice"],"created_at":"2026-10-01T12:00:00Z","created_by":"alice"}')`,
		`INSERT INTO agents(id,data) VALUES ('alice','{"id":"alice","kind":"human","name":"alice"}')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO messages(id,room_id,data) VALUES (?, 'work', ?)`, id,
		`{"id":`+strconv.Itoa(id)+`,"room_id":"work","from":"alice","from_kind":"human","body":"hi","created_at":"2026-10-01T12:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
}

func nextEventSeq(t *testing.T, db *store.Store) int64 {
	t.Helper()
	var seq int64
	if err := db.Tx(context.Background(), func(tx *store.Tx) error {
		event, err := tx.AppendEvent(store.Event{Kind: "user_created", PayloadJSON: `{}`})
		seq = event.Seq
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return seq
}

// The first start reserves the v1 numbers before any setup event is written, so the first event
// after it lands above the history.
func TestFirstStartReservesV1NumbersBeforeSetupEvents(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	v1BackupWithMessage(t, root, 700)
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "aimebu.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stderr bytes.Buffer
	if err := reserveV1History(context.Background(), db, root, &stderr); err != nil || stderr.Len() != 0 {
		t.Fatalf("reserve: %v %q", err, stderr.String())
	}
	if seq := nextEventSeq(t, db); seq <= 700 {
		t.Fatalf("first setup event took seq %d inside the v1 range", seq)
	}
}

func TestNoBackupMeansNoReservation(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "aimebu.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := reserveV1History(context.Background(), db, t.TempDir(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if seq := nextEventSeq(t, db); seq > 5 {
		t.Fatalf("an install without a backup reserved numbers: first event seq %d", seq)
	}
}

// An unreadable backup warns and lets the server start; a number range already in use is reported.
func TestUnreadableBackupWarnsButDoesNotStopStartup(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, v1BackupName, "server")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aimebu.sqlite"), []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "aimebu.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stderr bytes.Buffer
	if err := reserveV1History(context.Background(), db, root, &stderr); err != nil || !strings.Contains(stderr.String(), "could not reserve") {
		t.Fatalf("unreadable backup: err=%v stderr=%q", err, stderr.String())
	}
}

func TestOverlappingActivityIsReportedNotRenumbered(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	v1BackupWithMessage(t, root, 5)
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "aimebu.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < 8; i++ {
		nextEventSeq(t, db)
	}
	var stderr bytes.Buffer
	if err := reserveV1History(context.Background(), db, root, &stderr); err != nil || !strings.Contains(stderr.String(), "overlap") {
		t.Fatalf("overlap: err=%v stderr=%q", err, stderr.String())
	}
}

// The real first start (serverCommand) reserves v1's numbers before it creates the owner, so every setup
// event lands above the history.
func TestServerCommandReservesV1NumbersBeforeTheOwnerExists(t *testing.T) {
	t.Setenv("AIMEBU2_USAGES", "0")
	useFreePort(t)
	root := t.TempDir()
	v1BackupWithMessage(t, root, 700)
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	t.Setenv("AIMEBU2_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "correct horse battery staple")
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := serverCommand(ctx, &stdout, &stderr); code != 0 {
		t.Fatalf("first start = %d, %q, %q", code, stdout.String(), stderr.String())
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(root, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var events int
	var lowest sql.NullInt64
	if err := db.QueryRow(`SELECT COUNT(*),MIN(seq) FROM event`).Scan(&events, &lowest); err != nil {
		t.Fatal(err)
	}
	if events == 0 || !lowest.Valid || lowest.Int64 <= 700 {
		t.Fatalf("the owner's setup events must land above v1's highest number (700): %d events, lowest seq %v", events, lowest)
	}
}

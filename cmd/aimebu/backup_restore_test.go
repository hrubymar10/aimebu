package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/server"
	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

func backupFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AIMEBU_CONFIG_DIR", root)
	path := filepath.Join(root, "server", "aimebu.sqlite")
	createFixtureDatabase(t, path)
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE backup_probe(value TEXT); INSERT INTO backup_probe VALUES ('preserved')`); err != nil {
		t.Fatal(err)
	}
	var roleID string
	if err := db.QueryRow(`SELECT id FROM role LIMIT 1`).Scan(&roleID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_backup','backup project',?,'2026-11-27T00:00:00Z')`, []any{root}},
		{`INSERT INTO room(id,project_id,depth,kind,title,created_by,created_at) VALUES ('room_backup','prj_backup',0,'task','backup room','owner','2026-11-27T00:00:00Z')`, nil},
		{`INSERT INTO machine(id,name,key_hash,approved) VALUES ('machine_backup','host','hash',1)`, nil},
		{`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('checkout_backup','prj_backup','machine_backup',?)`, []any{root}},
		{`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at) VALUES ('agent_backup','backup agent','prj_backup',?,'fake','test','test','fake','machine_backup','checkout_backup','hash','owner','2026-11-27T00:00:00Z')`, []any{roleID}},
		{`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('message_backup',1,'room_backup','system','system','backup message','[]','2026-11-27T00:00:00Z')`, nil},
	} {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "backups"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "backups", "older.tar.gz"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server", "secret.key"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(t.TempDir(), "copy.tar.gz")
}

func TestBackupRestoreStopped(t *testing.T) {
	root, archive := backupFixture(t)
	var out bytes.Buffer
	config, err := resolveServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := makeBackup(context.Background(), config, root, archive, &out); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(archive)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("archive mode: %v %v", info, err)
	}
	stage := t.TempDir()
	manifest, err := extractVerified(archive, stage)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != store.LatestSchemaVersion() {
		t.Fatalf("schema = %d", manifest.SchemaVersion)
	}
	for _, f := range manifest.Files {
		if strings.HasPrefix(f.Path, "backups/") {
			t.Fatalf("nested backup: %s", f.Path)
		}
	}
	if got, err := os.ReadFile(filepath.Join(stage, "server", "secret.key")); err != nil || string(got) != "private" {
		t.Fatalf("key: %q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(root, "server", "secret.key"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreArchive(context.Background(), root, archive, true, &out); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "server", "secret.key")); err != nil || string(got) != "private" {
		t.Fatalf("restored key: %q %v", got, err)
	}
	if !strings.Contains(out.String(), ".pre-restore-") {
		t.Fatalf("original path not reported: %s", out.String())
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(root, "server", "aimebu.sqlite"), true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value, integrity string
	if err := db.QueryRow(`SELECT value FROM backup_probe`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if value != "preserved" || integrity != "ok" {
		t.Fatalf("database: value=%q integrity=%q", value, integrity)
	}
	for _, check := range []struct {
		query string
		want  string
	}{
		{`SELECT title FROM room WHERE id='room_backup'`, "backup room"},
		{`SELECT body FROM message WHERE id='message_backup'`, "backup message"},
		{`SELECT slug FROM agent WHERE id='agent_backup'`, "backup agent"},
	} {
		var got string
		if err := db.QueryRow(check.query).Scan(&got); err != nil || got != check.want {
			t.Fatalf("restored %q = %q, %v; want %q", check.query, got, err, check.want)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := store.Open(context.Background(), filepath.Join(root, "server", "aimebu.sqlite"), store.Options{})
	if err != nil {
		t.Fatalf("server cannot open restored data: %v", err)
	}
	defer opened.Close()
	started := httptest.NewServer(server.New(opened).Handler())
	defer started.Close()
	response, err := started.Client().Get(started.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("restored server response = %d", response.StatusCode)
	}
}

func TestRestoreRefusesRunningServerAndBadArchive(t *testing.T) {
	root, archive := backupFixture(t)
	config, err := resolveServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := makeBackup(context.Background(), config, root, archive, io.Discard); err != nil {
		t.Fatal(err)
	}
	lock, err := lockServerDirectory(filepath.Join(root, "server"))
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreArchive(context.Background(), root, archive, true, io.Discard); err == nil || !strings.Contains(err.Error(), "server is running") {
		t.Fatalf("running server: %v", err)
	}
	unlockServerDirectory(lock)
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	rewriteArchive(t, archive, bad, func(m *archiveManifest) { m.Files[0].SHA256 = strings.Repeat("0", 64) })
	if err := restoreArchive(context.Background(), root, bad, true, io.Discard); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad sha: %v", err)
	}
	newer := filepath.Join(t.TempDir(), "newer.tar.gz")
	rewriteArchive(t, archive, newer, func(m *archiveManifest) { m.SchemaVersion = store.LatestSchemaVersion() + 1 })
	if err := restoreArchive(context.Background(), root, newer, true, io.Discard); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer schema: %v", err)
	}
}

func TestBackupRequestsRunningServerSnapshot(t *testing.T) {
	root, archive := backupFixture(t)
	db, err := store.Open(context.Background(), filepath.Join(root, "server", "aimebu.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := server.New(db)
	removeKey, err := srv.EnableDoctorKey(root)
	if err != nil {
		t.Fatal(err)
	}
	defer removeKey()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	parsed, err := url.Parse(httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	bind, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockServerDirectory(filepath.Join(root, "server"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlockServerDirectory(lock)
	var writes sync.WaitGroup
	writes.Add(1)
	writeErrors := make(chan error, 1)
	go func() {
		defer writes.Done()
		for i := 0; i < 100; i++ {
			if err := db.Tx(context.Background(), func(tx *store.Tx) error {
				_, err := tx.Exec(`INSERT INTO backup_probe(value) VALUES (?)`, "running write")
				return err
			}); err != nil {
				writeErrors <- err
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	if err := makeBackup(context.Background(), serverConfig{root: root, bind: bind, port: port}, root, archive, io.Discard); err != nil {
		t.Fatal(err)
	}
	writes.Wait()
	select {
	case err := <-writeErrors:
		t.Fatal(err)
	default:
	}
	stage := t.TempDir()
	if _, err := extractVerified(archive, stage); err != nil {
		t.Fatal(err)
	}
	copy, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(stage, "server", "aimebu.sqlite"), true))
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var integrity string
	if err := copy.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("snapshot integrity = %q, %v", integrity, err)
	}
}

func TestBackupSkipsRunningSupervisorJournal(t *testing.T) {
	root, archive := backupFixture(t)
	journalPath := filepath.Join(root, "supervisor", "journal.sqlite")
	journal, err := supervisor.OpenJournal(context.Background(), journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	config, err := resolveServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := makeBackup(context.Background(), config, root, archive, &out); err != nil {
		t.Fatal(err)
	}
	manifest, err := extractVerified(archive, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.SkippedJournals) != 1 || manifest.SkippedJournals[0] != "supervisor/journal.sqlite" {
		t.Fatalf("skipped journals = %v", manifest.SkippedJournals)
	}
	if !strings.Contains(out.String(), "Skipped 1 running supervisor journal") || !strings.Contains(out.String(), "owner must snapshot") {
		t.Fatalf("missing skip reason: %q", out.String())
	}
	for _, file := range manifest.Files {
		if strings.HasPrefix(file.Path, "supervisor/journal.sqlite") {
			t.Fatalf("running journal included: %s", file.Path)
		}
	}
}

func rewriteArchive(t *testing.T, source, dest string, change func(*archiveManifest)) {
	t.Helper()
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	reader, err := gzip.NewReader(input)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tr := tar.NewReader(reader)
	output, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	gz := gzip.NewWriter(output)
	tw := tar.NewWriter(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "manifest.json" {
			var m archiveManifest
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			change(&m)
			data, err = json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			h.Size = int64(len(data))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRejectsUnsafeTarEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind byte
	}{
		{"../outside", tar.TypeReg},
		{"link", tar.TypeSymlink},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "unsafe.tar.gz")
			file, err := os.Create(archive)
			if err != nil {
				t.Fatal(err)
			}
			gz := gzip.NewWriter(file)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(&tar.Header{Name: tc.name, Typeflag: tc.kind, Linkname: "../outside", Mode: 0600}); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := extractVerified(archive, t.TempDir()); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

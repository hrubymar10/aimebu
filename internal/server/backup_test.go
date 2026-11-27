package server

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestBackupDatabaseUsesServerConnectionDuringWrites(t *testing.T) {
	f := newFixture(t)
	f.seed()
	roleID, err := f.db.RoleIDByKey(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_backup',?,?,?)`, f.project.ID, f.machineID, f.project.Folder); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateAgent(t.Context(), store.Agent{Slug: "backupworker", ProjectID: f.project.ID, RoleID: roleID, Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: f.machineID, CheckoutID: "chk_backup", KeyHash: "backup-key", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	first := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "before backup"}, true)
	first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("message status = %d", first.StatusCode)
	}
	root := t.TempDir()
	remove, err := f.srv.EnableDoctorKey(root)
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	key := readDoctorKeyFile(t, root)
	req, err := http.NewRequest(http.MethodGet, f.http.URL+"/api/backup/database", nil)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthorized = %d", unauthorized.StatusCode)
	}
	var writes sync.WaitGroup
	writes.Add(1)
	go func() {
		defer writes.Done()
		for i := 0; i < 20; i++ {
			response := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": fmt.Sprintf("backup message %d", i)}, true)
			response.Body.Close()
		}
	}()
	req, err = http.NewRequestWithContext(context.Background(), http.MethodGet, f.http.URL+"/api/backup/database", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status = %d", response.StatusCode)
	}
	copyPath := filepath.Join(t.TempDir(), "snapshot.sqlite")
	copyFile, err := os.Create(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(copyFile, response.Body); err != nil {
		t.Fatal(err)
	}
	if err := copyFile.Close(); err != nil {
		t.Fatal(err)
	}
	writes.Wait()
	db, err := sql.Open("sqlite", sqliteutil.DSN(copyPath, true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var result string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil || result != "ok" {
		t.Fatalf("snapshot integrity = %q, %v", result, err)
	}
	for _, table := range []string{"room", "message", "agent"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count == 0 {
			t.Fatalf("snapshot %s count = %d, %v", table, count, err)
		}
	}
}

func TestRestoredCleanupDoesNotDeleteSourceAttachment(t *testing.T) {
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	message, err := f.db.InsertMessage(ctx, store.Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "attachment"})
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot := filepath.Dir(f.db.DatabasePath())
	sourceFile := filepath.Join(f.db.AttachmentDir(), "kept.bin")
	if err := os.MkdirAll(filepath.Dir(sourceFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceFile, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -10).UTC().Format("2006-01-02T15:04:05.000000000Z")
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO attachment(id,project_id,room_id,message_id,sha256,mime,size,path,created_by,created_at,filename) VALUES ('kept',?,?,?,?,?,?,?,?,?,?)`, f.project.ID, f.room.ID, message.ID, "", "text/plain", 6, sourceFile, "owner", old, "kept.bin")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	snapshot := filepath.Join(targetRoot, "api.sqlite")
	if err := f.db.Snapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.RebaseSnapshotAttachments(ctx, snapshot, sourceRoot, targetRoot); err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(targetRoot, "attachments", "kept.bin")
	if err := os.MkdirAll(filepath.Dir(targetFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetFile, []byte("target"), 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Open(ctx, snapshot, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	days := 1
	if _, err := restored.SetCleanupSettings(ctx, nil, nil, nil, &days); err != nil {
		t.Fatal(err)
	}
	if _, err := New(restored).runCleanup(ctx, false, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(targetFile); !os.IsNotExist(err) {
		t.Fatalf("target attachment remains: %v", err)
	}
	if got, err := os.ReadFile(sourceFile); err != nil || string(got) != "source" {
		t.Fatalf("source attachment changed: %q %v", got, err)
	}
}

func TestBackupDatabaseCapsConcurrentSnapshots(t *testing.T) {
	f := newFixture(t)
	root := t.TempDir()
	remove, err := f.srv.EnableDoctorKey(root)
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	key := readDoctorKeyFile(t, root)
	entered := make(chan struct{})
	release := make(chan struct{})
	f.srv.backupSnapshot = func(ctx context.Context, path string) error {
		close(entered)
		<-release
		return f.db.Snapshot(ctx, path)
	}
	request := func() (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, f.http.URL+"/api/backup/database", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		return http.DefaultClient.Do(req)
	}
	first := make(chan *http.Response, 1)
	firstErr := make(chan error, 1)
	go func() {
		response, err := request()
		if err != nil {
			firstErr <- err
		} else {
			first <- response
		}
	}()
	<-entered
	second, err := request()
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("parallel request = %d", second.StatusCode)
	}
	close(release)
	select {
	case response := <-first:
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("first request = %d", response.StatusCode)
		}
	case err := <-firstErr:
		t.Fatal(err)
	}
}

func TestBackupDatabaseCleansSnapshotAfterClientDisconnect(t *testing.T) {
	f := newFixture(t)
	root := t.TempDir()
	remove, err := f.srv.EnableDoctorKey(root)
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	key := readDoctorKeyFile(t, root)
	started := make(chan struct{})
	release := make(chan struct{})
	f.srv.backupStream = func(w io.Writer, _ io.Reader) (int64, error) {
		_, _ = w.Write([]byte("x"))
		if flush, ok := w.(http.Flusher); ok {
			flush.Flush()
		}
		close(started)
		<-release
		return 1, io.ErrClosedPipe
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.http.URL+"/api/backup/database", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	one := make([]byte, 1)
	if _, err := response.Body.Read(one); err != nil {
		t.Fatal(err)
	}
	cancel()
	response.Body.Close()
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for f.srv.backupActive.Load() {
		if time.Now().After(deadline) {
			t.Fatal("snapshot request did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(f.db.DatabasePath()), "backups", ".backup-snapshot-*.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("temporary snapshots remain: %v", paths)
	}
}

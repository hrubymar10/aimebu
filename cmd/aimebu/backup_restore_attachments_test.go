package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func insertBackupAttachment(t *testing.T, root, id, blob string) string {
	t.Helper()
	p := filepath.Join(root, "server", "attachments", id+".bin")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if blob != "" {
		if err := os.WriteFile(p, []byte(blob), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(root, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO attachment(id,project_id,room_id,message_id,sha256,mime,size,path,created_by,created_at,filename) VALUES (?, 'prj_backup', 'room_backup', 'message_backup', '', 'text/plain', ?, ?, 'owner', '2026-11-27T00:00:00Z', ?)`, id, len(blob), p, id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBackupOmitsAttachmentRemovedByRetention(t *testing.T) {
	root, archive := backupFixture(t)
	path := insertBackupAttachment(t, root, "removed", "old data")
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(root, "server", "aimebu.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	deleteFile, err := s.MarkAttachmentRemoved(ctx, "removed", 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !deleteFile {
		t.Fatal("retention did not select the file for deletion")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	config, err := resolveServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := makeBackup(ctx, config, root, archive, &output); err != nil {
		t.Fatal(err)
	}
	manifest, err := extractVerified(archive, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.MissingAttachments) != 0 {
		t.Fatalf("removed attachment reported missing: %v", manifest.MissingAttachments)
	}
	if strings.Contains(output.String(), "Warning:") {
		t.Fatalf("backup warned about removed attachment: %q", output.String())
	}
}

func TestBackupOlderAttachmentSchema(t *testing.T) {
	root, archive := backupFixture(t)
	path := insertBackupAttachment(t, root, "kept", "older data")
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(root, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`DROP TABLE attachment`, `CREATE TABLE attachment(id TEXT PRIMARY KEY, path TEXT NOT NULL)`} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO attachment(id,path) VALUES ('kept', ?)`, path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteSchemaVersion(t, filepath.Join(root, "server", "aimebu.sqlite"), 59)
	config, err := resolveServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := makeBackup(context.Background(), config, root, archive, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	manifest, err := extractVerified(archive, stage)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.MissingAttachments) != 0 {
		t.Fatalf("missing attachments: %v", manifest.MissingAttachments)
	}
	if got, err := os.ReadFile(filepath.Join(stage, "server", "attachments", "kept.bin")); err != nil || string(got) != "older data" {
		t.Fatalf("archived attachment = %q, %v", got, err)
	}
}

func TestBackupAttachmentRowsDetermineArchive(t *testing.T) {
	root, archive := backupFixture(t)
	insertBackupAttachment(t, root, "present", "present data")
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(root, "server", "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO attachment(id,project_id,room_id,message_id,sha256,mime,size,path,created_by,created_at,filename) SELECT 'shared',project_id,room_id,message_id,sha256,mime,size,path,created_by,created_at,filename FROM attachment WHERE id='present'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	missingPath := insertBackupAttachment(t, root, "missing", "gone after snapshot")
	orphan := filepath.Join(root, "server", "attachments", "orphan.bin")
	if err := os.WriteFile(orphan, []byte("orphan data"), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := resolveServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := makeBackupWithSnapshotHook(context.Background(), config, root, archive, &output, func() {
		if err := os.Remove(missingPath); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	manifest, err := extractVerified(archive, stage)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(stage, "server", "attachments", "present.bin")); err != nil || string(got) != "present data" {
		t.Fatalf("present: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(stage, "server", "attachments", "orphan.bin")); !os.IsNotExist(err) {
		t.Fatalf("orphan archived: %v", err)
	}
	if len(manifest.MissingAttachments) != 1 || manifest.MissingAttachments[0] != "missing" {
		t.Fatalf("missing attachments: %v", manifest.MissingAttachments)
	}
	if !strings.Contains(output.String(), "missing") {
		t.Fatalf("no backup warning: %s", output.String())
	}
	target := filepath.Join(t.TempDir(), "restored")
	var restored bytes.Buffer
	if err := restoreArchive(context.Background(), target, archive, true, &restored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(restored.String(), "missing") {
		t.Fatalf("no restore warning: %s", restored.String())
	}
}

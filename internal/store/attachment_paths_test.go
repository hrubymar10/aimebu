package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAttachmentPathMigrationMixedRows(t *testing.T) {
	entries := append([]migration(nil), migrations...)
	entries = append(entries, migration{version: entries[len(entries)-1].version + 1, sql: "SELECT 1;"})
	var migrationSQL string
	found := false
	for _, entry := range entries {
		if entry.version == 146 {
			migrationSQL, found = entry.sql, true
			break
		}
	}
	if !found || migrationSQL != "" {
		t.Fatal("the Go-only migration must have no SQL to execute")
	}
	s, path := attachmentPathStore(t)
	project, _, _ := fixture(t, s)
	root := dataRootForDB(path)
	if err := os.MkdirAll(filepath.Join(root, "server", "attachments"), 0700); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(resolvedRoot, "server", "attachments", "att_inside.bin")
	outside := filepath.Join(t.TempDir(), "att_outside.bin")
	for _, item := range []struct{ id, path string }{
		{"att_inside", inside}, {"att_outside", outside},
		{"att_relative", filepath.Join("server", "attachments", "att_relative.bin")},
	} {
		if _, err := s.db.Exec(`INSERT INTO attachment(id,project_id,sha256,mime,size,path,created_by,created_at) VALUES (?,?,'','text/plain',0,?,'owner','now')`, item.id, project.ID, item.path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version=146`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var applied int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version=146`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("Go-only migration version row count = %d: %v", applied, err)
	}
	for _, item := range []struct{ id, want string }{
		{"att_inside", filepath.Join("server", "attachments", "att_inside.bin")},
		{"att_outside", outside},
		{"att_relative", filepath.Join("server", "attachments", "att_relative.bin")},
	} {
		var got string
		if err := reopened.db.QueryRow(`SELECT path FROM attachment WHERE id=?`, item.id).Scan(&got); err != nil || got != item.want {
			t.Fatalf("%s: path %q, want %q: %v", item.id, got, item.want, err)
		}
	}
}

func TestAttachmentPathMigrationSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	if err := os.MkdirAll(filepath.Join(real, "server", "attachments"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(link, "server", "aimebu.sqlite")
	if err := os.WriteFile(path, blankTestDB, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	project, _, _ := fixture(t, s)
	outside := filepath.Join(base, "outside", "att_outside.bin")
	if err := os.MkdirAll(filepath.Dir(outside), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	escapingFile := filepath.Join(real, "server", "attachments", "att_file_link.bin")
	if err := os.Symlink(outside, escapingFile); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, path string }{
		{"att_link", filepath.Join(link, "server", "attachments", "att_link.bin")},
		{"att_real", filepath.Join(real, "server", "attachments", "att_real.bin")},
		{"att_outside", outside},
		{"att_file_link", escapingFile},
	} {
		if _, err := s.db.Exec(`INSERT INTO attachment(id,project_id,sha256,mime,size,path,created_by,created_at) VALUES (?,?,'','text/plain',0,?,'owner','now')`, item.id, project.ID, item.path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version=146`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, item := range []struct{ id, want string }{
		{"att_link", "server/attachments/att_link.bin"},
		{"att_real", "server/attachments/att_real.bin"},
		{"att_outside", outside},
		{"att_file_link", escapingFile},
	} {
		var got string
		if err := reopened.db.QueryRow(`SELECT path FROM attachment WHERE id=?`, item.id).Scan(&got); err != nil || got != item.want {
			t.Fatalf("%s: path %q, want %q: %v", item.id, got, item.want, err)
		}
	}
}

func TestAttachmentPathMigrationLeavesExternalAttachmentDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(root, "server"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "server", "attachments")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "server", "aimebu.sqlite")
	if err := os.WriteFile(path, blankTestDB, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	project, _, _ := fixture(t, s)
	stored := filepath.Join(root, "server", "attachments", "att_escape.bin")
	if _, err := s.db.Exec(`INSERT INTO attachment(id,project_id,sha256,mime,size,path,created_by,created_at) VALUES ('att_escape',?,'','text/plain',0,?,'owner','now')`, project.ID, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version=146`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var got string
	if err := reopened.db.QueryRow(`SELECT path FROM attachment WHERE id='att_escape'`).Scan(&got); err != nil || got != stored {
		t.Fatalf("path %q, want %q: %v", got, stored, err)
	}
}

func TestAttachmentPathAfterRootCopy(t *testing.T) {
	s, path := attachmentPathStore(t)
	project, _, _ := fixture(t, s)
	root := dataRootForDB(path)
	rel := filepath.Join("server", "attachments", "att_copy.bin")
	original := AttachmentPath(root, rel)
	if err := os.MkdirAll(filepath.Dir(original), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("copied"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO attachment(id,project_id,sha256,mime,size,path,created_by,created_at) VALUES ('att_copy',?,'','text/plain',6,?,'owner','now')`, project.ID, rel); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	copyDB := filepath.Join(target, "server", "aimebu.sqlite")
	if err := os.MkdirAll(filepath.Dir(copyDB), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Snapshot(context.Background(), copyDB); err != nil {
		t.Fatal(err)
	}
	copyBlob := AttachmentPath(target, rel)
	if err := os.MkdirAll(filepath.Dir(copyBlob), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyBlob, []byte("copied"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), copyDB, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var stored string
	if err := reopened.db.QueryRow(`SELECT path FROM attachment WHERE id='att_copy'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != rel || reopened.resolveAttachmentPath(stored) != copyBlob {
		t.Fatalf("copied attachment path %q resolved to %q", stored, reopened.resolveAttachmentPath(stored))
	}
}

func attachmentPathStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server", "aimebu.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blankTestDB, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAttachmentPathMigrationMixedRows(t *testing.T) {
	if migrations[len(migrations)-1].sql != "" {
		t.Fatal("the Go-only migration must have no SQL to execute")
	}
	s, path := attachmentPathStore(t)
	project, _, _ := fixture(t, s)
	root := dataRootForDB(path)
	inside := filepath.Join(root, "server", "attachments", "att_inside.bin")
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

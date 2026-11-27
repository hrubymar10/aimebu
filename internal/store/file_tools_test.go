package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestRebaseSnapshotAttachmentsRefusesOutsideSourceRoot(t *testing.T) {
	s, _ := testStore(t, Options{})
	project, room, message := fixture(t, s)
	outside := filepath.Join(t.TempDir(), "other.bin")
	if _, err := s.db.Exec(`INSERT INTO attachment(id,project_id,room_id,message_id,sha256,mime,size,path,created_by,created_at) VALUES ('external',?,?,?,?,?,0,?,?,?)`, project.ID, room.ID, message.ID, "", "text/plain", outside, "owner", timestamp()); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := s.Snapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	sourceRoot := filepath.Dir(s.dbPath)
	if err := RebaseSnapshotAttachments(context.Background(), snapshot, sourceRoot, filepath.Join(t.TempDir(), "new")); err == nil || !strings.Contains(err.Error(), "outside backup source_root") {
		t.Fatalf("outside path: %v", err)
	}
}

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCleanupSettingsDefaultsAndValidation(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	got, err := s.CleanupSettings(ctx)
	if err != nil || !got.Enabled || got.Hour != 3 || got.OrphanDays != 14 || got.LastRun != "" {
		t.Fatalf("defaults: %+v %v", got, err)
	}
	off, hour, days := false, 22, 30
	got, err = s.SetCleanupSettings(ctx, &off, &hour, &days, nil)
	if err != nil || got.Enabled || got.Hour != 22 || got.OrphanDays != 30 {
		t.Fatalf("saved: %+v %v", got, err)
	}
	for _, bad := range []struct{ hour, days int }{{24, 5}, {-1, 5}, {5, 0}, {5, 366}} {
		if _, err := s.SetCleanupSettings(ctx, nil, &bad.hour, &bad.days, nil); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if again, _ := s.CleanupSettings(ctx); again.Hour != 22 {
		t.Fatalf("a refused change was partly saved: %+v", again)
	}
}

func TestCleanupLogOrdersTrimsAndSurvivesEmpty(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	if err := s.AppendCleanupLog(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendCleanupLog(ctx, []CleanupEntry{
		{Category: "worktree", Target: "/a", Reason: "merged", Bytes: 10, Action: "removed", Actor: "schedule"},
		{Category: "attachment", Target: "/b", Reason: "orphan", Bytes: 5, Action: "removed", Actor: "owner"},
	}); err != nil {
		t.Fatal(err)
	}
	log, err := s.CleanupLog(ctx, 10)
	if err != nil || len(log) != 2 || log[0].Target != "/b" || log[1].Target != "/a" {
		t.Fatalf("newest first: %+v %v", log, err)
	}
	if n, err := s.TrimCleanupLog(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("trim of recent rows: %d %v", n, err)
	}
	if n, err := s.TrimCleanupLog(ctx, time.Now().Add(time.Hour)); err != nil || n != 2 {
		t.Fatalf("trim of old rows: %d %v", n, err)
	}
}

func TestDatabaseFootprintAndCompact(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	before, err := s.DatabaseFootprint(ctx)
	if err != nil || before.Bytes <= 0 || before.FreeRatio < 0 || before.FreeRatio > 1 {
		t.Fatalf("footprint: %+v %v", before, err)
	}
	if err := s.Compact(ctx); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if after, err := s.DatabaseFootprint(ctx); err != nil || after.FreeBytes != 0 {
		t.Fatalf("a compacted database has free pages: %+v %v", after, err)
	}
}

func TestAttachmentRetentionMarksTheRowAndKeepsASharedFile(t *testing.T) {
	t.Parallel()
	s, project, room, worker, _ := wakeFixture(t)
	ctx := context.Background()
	message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: worker.ID, Body: "files"})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-200 * 24 * time.Hour).UTC().Format(timestampLayout)
	for _, id := range []string{"att_a", "att_b"} {
		if _, err := s.db.Exec(`INSERT INTO attachment(id,project_id,room_id,message_id,sha256,mime,size,path,created_by,created_at,filename) VALUES (?,?,?,?,'x','image/png',3,'/data/shared.bin','owner',?,?)`,
			id, project.ID, room.ID, message.ID, old, id+".png"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.AttachmentsOlderThan(ctx, time.Now().Add(-90*24*time.Hour))
	if err != nil || len(got) != 2 {
		t.Fatalf("older than 90 days: %+v %v", got, err)
	}
	// Two records share one file: it goes only with the last of them.
	deleteFile, err := s.MarkAttachmentRemoved(ctx, "att_a", 90)
	if err != nil || deleteFile {
		t.Fatalf("first record: deleteFile=%v err=%v", deleteFile, err)
	}
	deleteFile, err = s.MarkAttachmentRemoved(ctx, "att_b", 90)
	if err != nil || !deleteFile {
		t.Fatalf("last record: deleteFile=%v err=%v", deleteFile, err)
	}
	if got, _ := s.AttachmentsOlderThan(ctx, time.Now()); len(got) != 0 {
		t.Fatalf("removed attachments are listed again: %+v", got)
	}
	attachments, err := s.AttachmentsForMessage(ctx, message.ID)
	if err != nil || len(attachments) != 2 || attachments[0].RemovedAt == "" || attachments[0].RemovedAfterDays != 90 {
		t.Fatalf("the message lost its attachment records: %+v %v", attachments, err)
	}
	if _, _, _, err := s.AttachmentByID(ctx, "att_a", "owner"); !errors.Is(err, ErrAttachmentRemoved) {
		t.Fatalf("a removed attachment is served: %v", err)
	}
	if _, err := s.MarkAttachmentRemoved(ctx, "att_a", 90); err == nil {
		t.Fatal("removing an already removed attachment must not succeed twice")
	}
	if paths, _ := s.RegisteredAttachmentPaths(ctx); len(paths) != 0 {
		t.Fatalf("a removed attachment still protects its path: %v", paths)
	}
	settings, _ := s.CleanupSettings(ctx)
	if settings.AttachmentDays != 90 {
		t.Fatalf("default retention: %+v", settings)
	}
	days := 0
	if got, err := s.SetCleanupSettings(ctx, nil, nil, nil, &days); err != nil || got.AttachmentDays != 0 {
		t.Fatalf("0 keeps attachments for ever: %+v %v", got, err)
	}
	for _, bad := range []int{-1, MaxAttachmentDays + 1} {
		if _, err := s.SetCleanupSettings(ctx, nil, nil, nil, &bad); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%d: %v", bad, err)
		}
	}
}

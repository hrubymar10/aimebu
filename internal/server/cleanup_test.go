package server

import (
	"database/sql"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

type cleanupPlan struct {
	DryRun bool                 `json:"dry_run"`
	Items  []store.CleanupEntry `json:"items"`
	Bytes  int64                `json:"bytes"`
}

func (f *fixture) cleanup(dryRun bool) cleanupPlan {
	f.t.Helper()
	resp := f.request(http.MethodPost, "/api/cleanup", map[string]any{"dry_run": dryRun}, true)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		f.t.Fatalf("cleanup status %d", resp.StatusCode)
	}
	var plan cleanupPlan
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		f.t.Fatal(err)
	}
	return plan
}

// age pushes every file under root, and the branch commit date is left alone, a couple of days into the past, so a grace period has passed.
func ageTree(t *testing.T, root string, by time.Duration) {
	t.Helper()
	old := time.Now().Add(-by)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil {
			_ = os.Chtimes(path, old, old)
		}
		return nil
	})
}

func (f *fixture) newTaskRoom(title string) store.Room {
	f.t.Helper()
	resp := f.request("POST", "/api/projects/"+f.project.ID+"/rooms", map[string]string{"title": title}, true)
	var room store.Room
	if err := json.NewDecoder(resp.Body).Decode(&room); err != nil || resp.StatusCode != 201 {
		f.t.Fatalf("room %s: %d %v", title, resp.StatusCode, err)
	}
	resp.Body.Close()
	return room
}

// Nothing removes a worktree on its own in 2.0: not an open room's, not a closed room's merged one. Health shows their state; removing
// is the owner's action from the room's settings.
func TestCleanupNeverRemovesAWorktree(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	root := f.project.Folder
	initRepo(t, root)
	machine, err := f.db.MachineByID(t.Context(), f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	merged := f.newTaskRoom("merged")
	dirty := f.newTaskRoom("dirty")
	for _, room := range []store.Room{merged, dirty} {
		if _, err := f.srv.ensureRoomWorktree(t.Context(), room, machine, ""); err != nil {
			t.Fatal(err)
		}
	}
	mergedPath := filepath.Join(root, ".aimebu", "worktrees", merged.ID)
	dirtyPath := filepath.Join(root, ".aimebu", "worktrees", dirty.ID)
	if err := os.WriteFile(filepath.Join(mergedPath, "work.txt"), []byte("done\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, mergedPath, "config", "user.name", "Test")
	gitTest(t, mergedPath, "config", "user.email", "test@example.invalid")
	gitTest(t, mergedPath, "add", "work.txt")
	// The commit is two days old and merged into the base: the best case for removing it.
	commit := exec.Command("git", "-C", mergedPath, "commit", "-qm", "work")
	commit.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+time.Now().Add(-48*time.Hour).Format(time.RFC3339))
	if output, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, output)
	}
	gitTest(t, root, "merge", "-q", "--ff-only", "aimebu/"+merged.ID)
	if err := os.WriteFile(filepath.Join(dirtyPath, "wip.txt"), []byte("wip\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ageTree(t, mergedPath, 48*time.Hour)
	ageTree(t, dirtyPath, 48*time.Hour)
	for _, room := range []store.Room{merged, dirty} {
		if _, err := f.exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, room.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, dryRun := range []bool{true, false} {
		for _, item := range f.cleanup(dryRun).Items {
			if item.Category == "worktree" {
				t.Fatalf("the cleanup touched a worktree: %+v", item)
			}
		}
	}
	for _, path := range []string{mergedPath, dirtyPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("a worktree is gone: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dirtyPath, "wip.txt")); err != nil {
		t.Fatalf("uncommitted work was lost: %v", err)
	}
	if log, _ := f.db.CleanupLog(t.Context(), 10); len(log) != 0 {
		t.Fatalf("the cleanup log must be empty: %+v", log)
	}
	// Health still tells them apart.
	resp := f.request("GET", "/api/disk", nil, false)
	var report diskReport
	json.NewDecoder(resp.Body).Decode(&report)
	resp.Body.Close()
	states := map[string]string{}
	for _, tree := range report.Worktrees {
		states[tree.Path] = tree.State
	}
	if states[mergedPath] != "merged" || states[dirtyPath] != "dirty" {
		t.Fatalf("worktree states: %v", states)
	}
}

func TestCleanupRemovesOrphanUploadsAndKeepsRegisteredAndFreshOnes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := f.db.AttachmentDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "orphan.bin")
	fresh := filepath.Join(dir, "fresh.bin")
	for _, path := range []string{orphan, fresh} {
		if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	plan := f.cleanup(true)
	if len(plan.Items) != 1 || plan.Items[0].Target != orphan || plan.Items[0].Action != "would_remove" || plan.Bytes != 4 {
		t.Fatalf("plan: %+v", plan)
	}
	f.cleanup(false)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan remains: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a fresh upload was removed: %v", err)
	}
}

func TestCleanupSettingsScheduleAndOwnerOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cookie, csrf := registerSecondUser(t, f, "second")
	for _, tc := range []struct{ method, path string }{{"GET", "/api/disk"}, {"GET", "/api/cleanup/log"}, {"GET", "/api/cleanup/settings"}} {
		resp := f.requestAs(t, cookie, csrf, tc.method, tc.path, nil)
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("%s %s as a non-owner: %d", tc.method, tc.path, resp.StatusCode)
		}
	}
	resp := f.request("GET", "/api/cleanup/settings", nil, false)
	var settings store.CleanupSettings
	json.NewDecoder(resp.Body).Decode(&settings)
	resp.Body.Close()
	if !settings.Enabled || settings.Hour != 3 || settings.OrphanDays != 14 {
		t.Fatalf("defaults: %+v", settings)
	}
	for _, bad := range []map[string]any{{}, {"hour": 24}, {"orphan_days": 0}} {
		resp = f.request("PUT", "/api/cleanup/settings", bad, true)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("%v: %d", bad, resp.StatusCode)
		}
	}
	now := time.Now()
	resp = f.request("PUT", "/api/cleanup/settings", map[string]any{"hour": now.Hour(), "orphan_days": 7}, true)
	json.NewDecoder(resp.Body).Decode(&settings)
	resp.Body.Close()
	if settings.Hour != now.Hour() || settings.OrphanDays != 7 {
		t.Fatalf("saved: %+v", settings)
	}
	// The daily run happens once: the second call the same day does nothing.
	if err := f.srv.scheduledCleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := f.db.CleanupSettings(t.Context())
	if after.LastRun != now.Format("2006-01-02") {
		t.Fatalf("the run was not recorded: %+v", after)
	}
	if err := f.db.MarkCleanupRun(t.Context(), "1999-01-01"); err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := f.db.SetCleanupSettings(t.Context(), &off, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.scheduledCleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if again, _ := f.db.CleanupSettings(t.Context()); again.LastRun != "1999-01-01" {
		t.Fatalf("a switched-off schedule still ran: %+v", again)
	}
}

func TestDiskReportBreaksDownByProjectAndCategory(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	root := f.project.Folder
	initRepo(t, root)
	machine, _ := f.db.MachineByID(t.Context(), f.machineID)
	if _, err := f.srv.ensureRoomWorktree(t.Context(), f.room, machine, ""); err != nil {
		t.Fatal(err)
	}
	resp := f.request("GET", "/api/disk", nil, false)
	var report diskReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil || resp.StatusCode != 200 {
		t.Fatalf("disk: %d %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	keys := map[string]bool{}
	for _, c := range report.Categories {
		keys[c.Key] = true
	}
	for _, key := range []string{"database", "attachments", "worktrees", "supervisor", "sounds", "v1_backup", "migration_backups"} {
		if !keys[key] {
			t.Fatalf("missing category %q in %+v", key, report.Categories)
		}
	}
	if report.TotalBytes <= 0 || len(report.Worktrees) != 1 || report.Worktrees[0].State != "ready" || report.Worktrees[0].Bytes <= 0 {
		t.Fatalf("report: %+v", report)
	}
	if len(report.Projects) != 1 || report.Projects[0].ProjectID != f.project.ID || report.Projects[0].Name != "p" || report.Projects[0].Worktrees != report.Worktrees[0].Bytes {
		t.Fatalf("projects: %+v", report.Projects)
	}
	if report.FreeBytes <= 0 || report.VolumeBytes <= 0 {
		t.Fatalf("free space missing: %+v", report)
	}
}

func TestScheduledCleanupCatchesUpAcrossMidnightOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	hour := 22
	if _, err := f.db.SetCleanupSettings(t.Context(), nil, &hour, nil, nil); err != nil {
		t.Fatal(err)
	}
	day := func(offset int, h int) time.Time {
		base := time.Date(2030, 3, 10, 0, 0, 0, 0, time.Local)
		return base.AddDate(0, 0, offset).Add(time.Duration(h) * time.Hour)
	}
	if err := f.db.MarkCleanupRun(t.Context(), "2030-03-08"); err != nil {
		t.Fatal(err)
	}
	// 21:00 is before the moment; 01:00 the next morning is inside the six hours after yesterday's 22:00.
	f.srv.CleanupClock = func() time.Time { return day(0, 21) }
	if err := f.srv.scheduledCleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.db.CleanupSettings(t.Context()); got.LastRun != "2030-03-08" {
		t.Fatalf("ran before its moment: %+v", got)
	}
	f.srv.CleanupClock = func() time.Time { return day(1, 1) }
	if err := f.srv.scheduledCleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.db.CleanupSettings(t.Context()); got.LastRun != "2030-03-10" {
		t.Fatalf("the late-evening run was not made after midnight: %+v", got)
	}
	// Once only, and not again later inside the same window.
	if err := f.db.MarkCleanupRun(t.Context(), "2030-03-10"); err != nil {
		t.Fatal(err)
	}
	f.srv.CleanupClock = func() time.Time { return day(1, 3) }
	if err := f.srv.scheduledCleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	// After the window and before the next moment nothing is due.
	if err := f.db.MarkCleanupRun(t.Context(), "2030-03-09"); err != nil {
		t.Fatal(err)
	}
	f.srv.CleanupClock = func() time.Time { return day(1, 12) }
	if err := f.srv.scheduledCleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.db.CleanupSettings(t.Context()); got.LastRun != "2030-03-09" {
		t.Fatalf("ran outside its window: %+v", got)
	}
}

func TestOrphanUploadsAreFoundThroughASymlinkedDataDirectory(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	if err := os.MkdirAll(f.db.AttachmentDir(), 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	registeredFile := filepath.Join(f.db.AttachmentDir(), "known.bin")
	if err := os.WriteFile(registeredFile, []byte("kept"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(registeredFile, old, old); err != nil {
		t.Fatal(err)
	}
	// The registry holds the path through a symlink to the same directory.
	link := filepath.Join(t.TempDir(), "via-link")
	if err := os.Symlink(f.db.AttachmentDir(), link); err != nil {
		t.Skip("symlinks are not available")
	}
	if _, err := f.exec(`INSERT INTO attachment(id,project_id,room_id,message_id,sha256,mime,size,path,created_by,created_at) VALUES ('att_link',?,NULL,NULL,'x','image/png',4,?,'owner','2030-01-01T00:00:00.000000000Z')`, f.project.ID, filepath.Join(link, "known.bin")); err != nil {
		t.Fatalf("attachment insert: %v", err)
	}
	for _, item := range f.cleanup(true).Items {
		if item.Category == "attachment" {
			t.Fatalf("a registered file was called an orphan: %+v", item)
		}
	}
}

func (f *fixture) exec(query string, args ...any) (any, error) {
	f.t.Helper()
	return nil, f.db.Tx(f.t.Context(), func(tx *store.Tx) error { _, err := tx.Exec(query, args...); return err })
}

func TestOrphanSweepMatchesByNameAndSkipsAMismatchedFolder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	dir := f.db.AttachmentDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	for _, name := range []string{"a.bin", "stray.bin"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(id, path string) {
		t.Helper()
		if _, err := f.exec(`INSERT INTO attachment(id,project_id,room_id,message_id,sha256,mime,size,path,created_by,created_at) VALUES (?,?,NULL,NULL,'x','image/png',4,?,'owner','2030-01-01T00:00:00.000000000Z')`, id, f.project.ID, path); err != nil {
			t.Fatal(err)
		}
	}
	// A row that names the file by its name in an earlier location of the folder still protects it; the stray file is the orphan.
	insert("att_a", "/old/location/a.bin")
	var targets []string
	for _, item := range f.cleanup(true).Items {
		if item.Category == "attachment" && item.Action == "would_remove" {
			targets = append(targets, filepath.Base(item.Target))
		}
	}
	if len(targets) != 1 || targets[0] != "stray.bin" {
		t.Fatalf("orphans by name: %v", targets)
	}
	// Rows exist but none of their files is in this folder: it is not the folder the registry describes, so nothing is an orphan.
	if _, err := f.exec(`DELETE FROM attachment`); err != nil {
		t.Fatal(err)
	}
	insert("att_elsewhere", "/old/location/zzz.bin")
	skipped := false
	for _, item := range f.cleanup(false).Items {
		if item.Action == "removed" && item.Category == "attachment" {
			t.Fatalf("a mismatched folder was swept: %+v", item)
		}
		skipped = skipped || (item.Category == "attachment" && item.Action == "kept")
	}
	if !skipped {
		t.Fatal("the skipped sweep was not reported")
	}
	if _, err := os.Stat(filepath.Join(dir, "stray.bin")); err != nil {
		t.Fatalf("stray file removed from a mismatched folder: %v", err)
	}
	resp := f.request("GET", "/api/disk", nil, false)
	var report diskReport
	json.NewDecoder(resp.Body).Decode(&report)
	resp.Body.Close()
	if len(report.Warnings) != 1 {
		t.Fatalf("Health warning missing: %+v", report.Warnings)
	}
}

func TestScheduledRetriesAreCappedAndBackedOff(t *testing.T) {
	t.Parallel()
	var a cleanupAttempts
	now := time.Date(2030, 3, 10, 3, 0, 0, 0, time.UTC)
	if !a.allow("2030-03-10", now) || a.allow("2030-03-10", now.Add(time.Minute)) {
		t.Fatal("a second attempt a minute later must wait")
	}
	if !a.allow("2030-03-10", now.Add(31*time.Minute)) || !a.allow("2030-03-10", now.Add(62*time.Minute)) {
		t.Fatal("attempts after the backoff are allowed")
	}
	if a.allow("2030-03-10", now.Add(95*time.Minute)) {
		t.Fatal("a fourth attempt in one day must be refused")
	}
	if !a.allow("2030-03-11", now.Add(24*time.Hour)) {
		t.Fatal("a new day starts again")
	}
}

// postMessage sends a message through the API and returns its ID.
func (f *fixture) postMessage(room store.Room, body string) string {
	f.t.Helper()
	resp := f.request("POST", "/api/rooms/"+room.ID+"/messages", map[string]any{"body": body}, true)
	defer resp.Body.Close()
	var message struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&message); err != nil || resp.StatusCode != 201 || message.ID == "" {
		f.t.Fatalf("message: %d %v", resp.StatusCode, err)
	}
	return message.ID
}

// addAttachment registers a bound attachment with a file of the given age.
func (f *fixture) addAttachment(id, messageID string, room store.Room, age time.Duration, content string) string {
	f.t.Helper()
	dir := f.db.AttachmentDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(dir, id+".bin")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		f.t.Fatal(err)
	}
	created := time.Now().Add(-age).UTC().Format("2006-01-02T15:04:05.000000000Z")
	if _, err := f.exec(`INSERT INTO attachment(id,project_id,room_id,message_id,sha256,mime,size,path,created_by,created_at,filename) VALUES (?,?,?,?,'x','image/png',?,?,'owner',?,?)`,
		id, f.project.ID, room.ID, messageID, len(content), path, created, id+".png"); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func TestAttachmentRetentionRemovesTheFileAndKeepsTheMessage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	message := f.postMessage(f.room, "here is the screenshot")
	oldPath := f.addAttachment("att_old", message, f.room, 120*24*time.Hour, "old-image")
	freshPath := f.addAttachment("att_fresh", message, f.room, 5*24*time.Hour, "fresh-image")

	// The default is 90 days. A dry run lists the old one and removes nothing.
	plan := f.cleanup(true)
	var listed []string
	for _, item := range plan.Items {
		if item.Category == "attachment" && item.Action == "would_remove" {
			listed = append(listed, item.Target)
		}
	}
	if len(listed) != 1 || listed[0] != "att_old.png · att_old" {
		t.Fatalf("plan: %v", listed)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("a dry run removed the file: %v", err)
	}
	f.cleanup(false)
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("the old attachment's file remains: %v", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("a recent attachment was removed: %v", err)
	}
	log, err := f.db.CleanupLog(t.Context(), 5)
	if err != nil || len(log) != 1 || log[0].Category != "attachment" || log[0].Action != "removed" || log[0].Bytes != int64(len("old-image")) || log[0].ProjectID != f.project.ID {
		t.Fatalf("cleanup log: %+v %v", log, err)
	}
	// The message and the attachment record stay, with the marker; the file is gone (410), and the recent one still downloads.
	resp := f.request("GET", "/api/rooms/"+f.room.ID+"/messages?after=0&limit=10", nil, false)
	var messages []struct {
		ID          string             `json:"id"`
		Body        string             `json:"body"`
		Attachments []store.Attachment `json:"attachments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	found := false
	for _, m := range messages {
		if m.ID != message {
			continue
		}
		found = true
		if m.Body != "here is the screenshot" || len(m.Attachments) != 2 {
			t.Fatalf("message: %+v", m)
		}
		byID := map[string]store.Attachment{}
		for _, a := range m.Attachments {
			byID[a.ID] = a
		}
		if byID["att_old"].RemovedAt == "" || byID["att_old"].RemovedAfterDays != 90 || byID["att_fresh"].RemovedAt != "" {
			t.Fatalf("attachments: %+v", byID)
		}
	}
	if !found {
		t.Fatal("the message is gone")
	}
	resp = f.request("GET", "/api/attachments/att_old", nil, false)
	resp.Body.Close()
	if resp.StatusCode != 410 {
		t.Fatalf("a removed attachment answered %d, want 410", resp.StatusCode)
	}
	resp = f.request("GET", "/api/attachments/att_fresh", nil, false)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("a kept attachment answered %d", resp.StatusCode)
	}
	// A second run finds nothing more; and 0 days keeps everything for ever.
	for _, item := range f.cleanup(false).Items {
		if item.Category == "attachment" {
			t.Fatalf("second run: %+v", item)
		}
	}
	zero := 0
	if _, err := f.db.SetCleanupSettings(t.Context(), nil, nil, nil, &zero); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`UPDATE attachment SET created_at='2000-01-01T00:00:00.000000000Z' WHERE id='att_fresh'`); err != nil {
		t.Fatal(err)
	}
	for _, item := range f.cleanup(false).Items {
		if item.Category == "attachment" {
			t.Fatalf("with retention off: %+v", item)
		}
	}
}

func TestAttachmentRetentionSettingValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	resp := f.request("GET", "/api/cleanup/settings", nil, false)
	var settings store.CleanupSettings
	json.NewDecoder(resp.Body).Decode(&settings)
	resp.Body.Close()
	if settings.AttachmentDays != 90 {
		t.Fatalf("default: %+v", settings)
	}
	for _, bad := range []int{-1, 3651} {
		resp = f.request("PUT", "/api/cleanup/settings", map[string]any{"attachment_days": bad}, true)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("%d: %d", bad, resp.StatusCode)
		}
	}
	resp = f.request("PUT", "/api/cleanup/settings", map[string]any{"attachment_days": 30}, true)
	json.NewDecoder(resp.Body).Decode(&settings)
	resp.Body.Close()
	if settings.AttachmentDays != 30 {
		t.Fatalf("saved: %+v", settings)
	}
}

// destructiveSQL matches a statement that would delete a message or a room, or drop or empty a table. It is used on the cleanup's source.
var destructiveSQL = regexp.MustCompile(`(?i)(delete\s+from\s+(message|room)\b|drop\s+table|truncate\s+table)`)

// The regular expression must really match what it guards against (an earlier one never matched anything), and leave the cleanup's own
// legitimate statements alone.
func TestTheDestructiveSQLCheckCatchesWhatItGuardsAgainst(t *testing.T) {
	t.Parallel()
	for _, planted := range []string{
		"DELETE FROM message WHERE id=?",
		"tx.Exec(`delete   from  room where id=?`)",
		"DELETE\nFROM message",
		"DROP TABLE message",
		"drop  table room",
		"TRUNCATE TABLE message",
	} {
		if !destructiveSQL.MatchString(planted) {
			t.Fatalf("the check misses %q", planted)
		}
	}
	for _, fine := range []string{
		"DELETE FROM cleanup_log WHERE created_at<?",
		"DELETE FROM global_setting WHERE key=?",
		"UPDATE attachment SET removed_at=? WHERE id=?",
		"DELETE FROM message_note WHERE id=?",
	} {
		if destructiveSQL.MatchString(fine) {
			t.Fatalf("the check flags %q", fine)
		}
	}
}

// The cleanup never deletes a message or a room, whatever it removes: every category runs, and the counts of both are the same
// afterwards. The source of the cleanup is also checked for any statement that would.
func TestCleanupNeverDeletesAMessageOrARoom(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	root := f.project.Folder
	initRepo(t, root)
	machine, _ := f.db.MachineByID(t.Context(), f.machineID)
	if _, err := f.srv.ensureRoomWorktree(t.Context(), f.room, machine, ""); err != nil {
		t.Fatal(err)
	}
	f.postMessage(f.newTaskRoom("other"), "hello")
	f.addAttachment("att_ancient", f.postMessage(f.room, "with a file"), f.room, 400*24*time.Hour, "ancient")
	f.exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, f.room.ID)
	orphan := filepath.Join(f.db.AttachmentDir(), "orphan.bin")
	if err := os.WriteFile(orphan, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-5 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", "file:"+f.db.DBPath()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	count := func(table string) int {
		var n int
		if err := reader.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	messagesBefore, roomsBefore := count("message"), count("room")
	f.cleanup(true)
	plan := f.cleanup(false)
	removed := 0
	for _, item := range plan.Items {
		if item.Action == "removed" {
			removed++
		}
	}
	if removed < 2 {
		t.Fatalf("the test did not exercise the cleanup (removed %d): %+v", removed, plan.Items)
	}
	hour := time.Now().Hour()
	if _, err := f.db.SetCleanupSettings(t.Context(), nil, &hour, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.scheduledCleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := count("message"); after != messagesBefore {
		t.Fatalf("messages: %d before, %d after", messagesBefore, after)
	}
	if after := count("room"); after != roomsBefore {
		t.Fatalf("rooms: %d before, %d after", roomsBefore, after)
	}
	for _, name := range []string{"cleanup.go", filepath.Join("..", "store", "cleanup.go")} {
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if forbidden := destructiveSQL.Find(source); forbidden != nil {
			t.Fatalf("%s contains %q: the cleanup must never delete a message or a room", name, forbidden)
		}
	}
}

// Retention counts from when an attachment arrived in v2. One imported from v1 (made a year ago) or one that was here before the
// retention existed is not removed by the first run; it goes the retention after it arrived.
func TestImportedAttachmentsAreJudgedFromTheirArrivalNotTheirAge(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	message := f.postMessage(f.room, "imported history")
	yearAgo := 365 * 24 * time.Hour
	path := f.addAttachment("att_imported", message, f.room, yearAgo, "imported-image")
	arrive := func(age time.Duration) {
		t.Helper()
		when := time.Now().Add(-age).UTC().Format("2006-01-02T15:04:05.000000000Z")
		if _, err := f.exec(`UPDATE attachment SET arrived_at=? WHERE id='att_imported'`, when); err != nil {
			t.Fatal(err)
		}
	}
	arrive(time.Minute) // imported a minute ago
	for _, dryRun := range []bool{true, false} {
		for _, item := range f.cleanup(dryRun).Items {
			if item.Category == "attachment" {
				t.Fatalf("a year-old attachment that arrived a minute ago was planned for removal: %+v", item)
			}
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the imported attachment's file is gone: %v", err)
	}
	arrive(10 * 24 * time.Hour)
	for _, item := range f.cleanup(true).Items {
		if item.Category == "attachment" {
			t.Fatalf("10 days after arrival it is not due: %+v", item)
		}
	}
	arrive(100 * 24 * time.Hour) // retention (90 days) has passed since it arrived
	removed := 0
	for _, item := range f.cleanup(false).Items {
		if item.Category == "attachment" && item.Action == "removed" {
			removed++
		}
	}
	if removed != 1 {
		t.Fatalf("after the retention since arrival it must go: %d", removed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the file remains: %v", err)
	}
}

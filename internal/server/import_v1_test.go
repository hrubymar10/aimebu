package server

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestImportV1SoleHumanOwnerDryRunCommitAndMarker(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	root := t.TempDir()
	f.srv.ImportV1Root = root
	dir := filepath.Join(root, ".old-v1-bkp", "server")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(dir, "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"CREATE TABLE schema_meta(version INTEGER)", "INSERT INTO schema_meta VALUES (1)",
		"CREATE TABLE rooms(id TEXT PRIMARY KEY,data TEXT)", "CREATE TABLE messages(id INTEGER PRIMARY KEY,room_id TEXT,data TEXT)",
		"CREATE TABLE agents(id TEXT PRIMARY KEY,data TEXT)", "CREATE TABLE reactions(message_id INTEGER PRIMARY KEY,data TEXT)",
		"CREATE TABLE memory(id TEXT PRIMARY KEY,data TEXT)", "CREATE TABLE attachments(id TEXT PRIMARY KEY,data TEXT)",
		"CREATE TABLE fleets(name TEXT PRIMARY KEY,data TEXT)",
		`INSERT INTO rooms(id,data) VALUES ('work','{"id":"work","members":["alice"],"created_at":"2026-10-01T12:00:00Z","created_by":"alice"}')`,
		`INSERT INTO agents(id,data) VALUES ('alice','{"id":"alice","kind":"human","name":"alice"}')`,
		`INSERT INTO fleets(name,data) VALUES ('default','{"agents":[{"command":"aimebu agent --auto-room --assume-role leader -- claude-docker --model opus"}]}')`,
		`INSERT INTO fleets(name,data) VALUES ('unknown-role','{"agents":[{"command":"aimebu agent --auto-room --assume-role ghost -- claude-docker --model opus"}]}')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE machine_launcher SET enabled=1 WHERE machine_id=? AND tool='claude' AND name='harness-docker'`, f.machineID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodGet, "/api/import-v1/status", nil, false)
	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || status["state"] != "ready" {
		t.Fatalf("status: %d %+v", resp.StatusCode, status)
	}
	manifestPath := filepath.Join(root, ".old-v1-bkp", "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"entries":["server"],"source_key":"v1-000000000000000000000000"}`), 0600); err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodGet, "/api/import-v1/status", nil, false)
	status = nil
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 409 || status["error"] != "backup_changed" {
		t.Fatalf("changed backup status: %d %+v", resp.StatusCode, status)
	}
	// Preview, dry run and commit name the same condition with the same code, not a generic source error or a bad mapping.
	for _, call := range []struct {
		method, path string
		body         any
	}{{http.MethodGet, "/api/import-v1/preview", nil}, {http.MethodPost, "/api/import-v1/dry-run", map[string]any{"source_key": "x"}}, {http.MethodPost, "/api/import-v1/commit", map[string]any{"mapping": map[string]any{"source_key": "x"}, "digest": "d"}}} {
		resp = f.request(call.method, call.path, call.body, true)
		var problem map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 409 || problem["error"] != "backup_changed" || problem["message"] != status["message"] {
			t.Fatalf("%s %s on a changed backup: %d %+v", call.method, call.path, resp.StatusCode, problem)
		}
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(root, ".v2-migrated")
	if err := os.WriteFile(manifestPath, []byte(`{"entries":["server"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("legacy marker"), 0600); err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodGet, "/api/import-v1/status", nil, false)
	status = nil
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 409 || status["error"] != "inconsistent_migration" {
		t.Fatalf("foreign marker status: %d %+v", resp.StatusCode, status)
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credential-targets.json"), []byte(`{"version":1,"targets":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "server"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server", "credential-targets.json"), []byte(`{"version":1,"targets":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	peerCookie, peerCSRF := registerSecondUser(t, f, "import-peer")
	resp = f.requestAs(t, peerCookie, peerCSRF, http.MethodGet, "/api/import-v1/status", nil)
	if resp.StatusCode != 403 {
		t.Fatalf("nonowner import status: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodGet, "/api/import-v1/preview", nil, false)
	var preview store.V1ImportPreview
	var previewJSON map[string]json.RawMessage
	rawPreview, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawPreview, &preview); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawPreview, &previewJSON); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(preview.Rooms) != 1 || preview.Suggested.Humans["alice"] != "owner" || len(preview.Suggested.NewUsers) != 0 {
		t.Fatalf("preview: %d %+v", resp.StatusCode, preview)
	}
	if len(preview.CarriedOverFiles) != 1 || preview.CarriedOverFiles[0] != "server/credential-targets.json" {
		t.Fatalf("carried files in preview = %v", preview.CarriedOverFiles)
	}
	for _, field := range []string{"rooms", "humans", "destination_users", "memory_projects"} {
		if len(previewJSON[field]) == 0 || previewJSON[field][0] != '[' {
			t.Fatalf("preview %s is not an array: %s", field, previewJSON[field])
		}
	}
	if string(previewJSON["memory_projects"]) != "[]" {
		t.Fatalf("zero-memory preview: %s", previewJSON["memory_projects"])
	}
	if err := os.WriteFile(markerPath, []byte("foreign source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodPost, "/api/import-v1/dry-run", preview.Suggested, true)
	var conflict map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&conflict); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 409 || conflict["error"] != "inconsistent_migration" {
		t.Fatalf("foreign marker dry run: %d %+v", resp.StatusCode, conflict)
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]string{"source_key": preview.SourceKey})
	if err := os.WriteFile(filepath.Join(root, ".old-v1-bkp", "manifest.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	// The preview invents no catch-all project (the room has no leader pair, so it starts as don't import): put the room in a project.
	preview.Suggested.Projects = map[string]store.V1ImportProject{"imported": {Name: "Imported", Folder: t.TempDir()}}
	for id, spec := range preview.Suggested.Rooms {
		spec.Project, spec.Skip = "imported", false
		preview.Suggested.Rooms[id] = spec
	}
	resp = f.request(http.MethodPost, "/api/import-v1/dry-run", preview.Suggested, true)
	var dry struct {
		Digest string               `json:"digest"`
		Report store.V1ImportReport `json:"report"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dry); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || dry.Digest == "" || dry.Report.Imported["rooms"] != 1 {
		t.Fatalf("dry run: %d %+v", resp.StatusCode, dry)
	}
	if len(dry.Report.CarriedOverFiles) != 1 || dry.Report.CarriedOverFiles[0] != "server/credential-targets.json" {
		t.Fatalf("carried files in report = %v", dry.Report.CarriedOverFiles)
	}
	resp = f.request(http.MethodPost, "/api/import-v1/commit", map[string]any{"mapping": preview.Suggested, "digest": dry.Digest}, true)
	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 || accepted.JobID == "" {
		t.Fatalf("commit: %d %+v", resp.StatusCode, accepted)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp = f.request(http.MethodGet, "/api/import-v1/jobs/"+accepted.JobID, nil, false)
		var job importV1JobState
		if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if job.Phase == "done" {
			break
		}
		if job.Phase == "error" {
			t.Fatalf("job: %+v", job)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(root, ".v2-migrated")); err != nil {
		t.Fatalf("migration marker: %v", err)
	}
	resp = f.request(http.MethodGet, "/api/import-v1/status", nil, false)
	status = nil
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if status["state"] != "done" {
		t.Fatalf("final status: %+v", status)
	}
	// Older backups lack source_key in their manifest. The matching marker
	// and import ledger still identify a completed import.
	if err := os.WriteFile(manifestPath, []byte(`{"entries":["server"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodGet, "/api/import-v1/status", nil, false)
	status = nil
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || status["state"] != "done" {
		t.Fatalf("legacy manifest status: %d %+v", resp.StatusCode, status)
	}
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "aimebu.sqlite"), filepath.Join(dir, "aimebu.sqlite.hidden")); err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodGet, "/api/import-v1/status", nil, false)
	status = nil
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || status["state"] != "done" {
		t.Fatalf("completed backup was reloaded: %d %+v", resp.StatusCode, status)
	}
}

func TestImportV1StatusEmptyBackupIsNone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	root := t.TempDir()
	f.srv.ImportV1Root = root
	dir := filepath.Join(root, ".old-v1-bkp", "server")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(dir, "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"CREATE TABLE schema_meta(version INTEGER)", "INSERT INTO schema_meta VALUES (1)",
		"CREATE TABLE rooms(id TEXT PRIMARY KEY,data TEXT)", "CREATE TABLE messages(id INTEGER PRIMARY KEY,room_id TEXT,data TEXT)",
		"CREATE TABLE agents(id TEXT PRIMARY KEY,data TEXT)", "CREATE TABLE reactions(message_id INTEGER PRIMARY KEY,data TEXT)",
		"CREATE TABLE memory(id TEXT PRIMARY KEY,data TEXT)", "CREATE TABLE attachments(id TEXT PRIMARY KEY,data TEXT)",
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodGet, "/api/import-v1/status", nil, false)
	defer resp.Body.Close()
	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || status["state"] != "none" {
		t.Fatalf("empty backup status: %d %+v", resp.StatusCode, status)
	}
	if choice, err := f.db.V1ImportChoice(context.Background()); err != nil || choice != "" {
		t.Fatalf("reading the status of an empty backup closed the offer: %q %v", choice, err)
	}
}

// Without a backup the status is "none" and stays read-only, so a backup copied in afterwards is
// still seen (and, having missed the reservation, offered only the skip).
func TestImportV1StatusWithoutABackupClosesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.ImportV1Root = t.TempDir()
	if code, status := importStatus(t, f, http.MethodGet, "/api/import-v1/status"); code != 200 || status["state"] != "none" {
		t.Fatalf("no backup: %d %+v", code, status)
	}
	if choice, err := f.db.V1ImportChoice(context.Background()); err != nil || choice != "" {
		t.Fatalf("reading the status without a backup closed the offer: %q %v", choice, err)
	}
	tinyV1Backup(t, f.srv.ImportV1Root)
	if code, status := importStatus(t, f, http.MethodGet, "/api/import-v1/status"); code != 200 || status["state"] != "ready" {
		t.Fatalf("a backup that arrives later is still offered: %d %+v", code, status)
	}
}

func TestImportV1MaintenanceReturnsRetryableWrites(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.maintenance.Store(true)
	defer f.srv.maintenance.Store(false)
	resp := f.request(http.MethodPost, "/api/projects", map[string]string{"name": "during import", "folder": "/tmp/work"}, true)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("write during maintenance: %d %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	resp.Body.Close()
	resp = f.request(http.MethodGet, "/api/me", nil, false)
	if resp.StatusCode != 200 {
		t.Fatalf("read during maintenance: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func tinyV1Backup(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, ".old-v1-bkp", "server")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteutil.DSN(filepath.Join(dir, "aimebu.sqlite"), false))
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"CREATE TABLE schema_meta(version INTEGER)", "INSERT INTO schema_meta VALUES (1)",
		"CREATE TABLE rooms(id TEXT PRIMARY KEY,data TEXT)", "CREATE TABLE messages(id INTEGER PRIMARY KEY,room_id TEXT,data TEXT)",
		"CREATE TABLE agents(id TEXT PRIMARY KEY,data TEXT)", "CREATE TABLE reactions(message_id INTEGER PRIMARY KEY,data TEXT)",
		"CREATE TABLE memory(id TEXT PRIMARY KEY,data TEXT)", "CREATE TABLE attachments(id TEXT PRIMARY KEY,data TEXT)",
		`INSERT INTO rooms(id,data) VALUES ('work','{"id":"work","members":["alice"],"created_at":"2026-10-01T12:00:00Z","created_by":"alice"}')`,
		`INSERT INTO agents(id,data) VALUES ('alice','{"id":"alice","kind":"human","name":"alice"}')`,
		`INSERT INTO messages(id,room_id,data) VALUES (40,'work','{"id":40,"room_id":"work","from":"alice","from_kind":"human","body":"see #39","created_at":"2026-10-01T12:00:00Z"}')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func importStatus(t *testing.T, f *fixture, method, path string) (int, map[string]any) {
	t.Helper()
	resp := f.request(method, path, nil, method != http.MethodGet)
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// The import is offered once. After it was completed or skipped every import route is closed (410) and
// a second choice cannot be made; skipping twice is only a repeat of the first answer.
func TestImportV1ChoiceClosesTheOfferForGood(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	root := t.TempDir()
	f.srv.ImportV1Root = root
	tinyV1Backup(t, root)
	if code, status := importStatus(t, f, http.MethodGet, "/api/import-v1/status"); code != 200 || status["state"] != "ready" {
		t.Fatalf("a fresh install with a backup is offered the import: %d %+v", code, status)
	}
	if resp := f.request(http.MethodPost, "/api/import-v1/skip", nil, true); resp.StatusCode != 204 {
		t.Fatalf("skip: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if code, status := importStatus(t, f, http.MethodGet, "/api/import-v1/status"); code != 200 || status["state"] != "skipped" {
		t.Fatalf("status after skip: %d %+v", code, status)
	}
	for _, route := range []struct{ method, path string }{{http.MethodGet, "/api/import-v1/preview"}, {http.MethodPost, "/api/import-v1/dry-run"}, {http.MethodPost, "/api/import-v1/commit"}} {
		if code, _ := importStatus(t, f, route.method, route.path); code != 410 {
			t.Fatalf("%s %s after skip: %d, want 410", route.method, route.path, code)
		}
	}
	if resp := f.request(http.MethodPost, "/api/import-v1/skip", nil, true); resp.StatusCode != 204 {
		t.Fatalf("a repeated skip: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// A completed choice cannot be turned into a skip either.
	g := newFixture(t)
	g.srv.ImportV1Root = t.TempDir()
	if err := g.db.SetV1ImportChoice(t.Context(), "done"); err != nil {
		t.Fatal(err)
	}
	if code, body := importStatus(t, g, http.MethodPost, "/api/import-v1/skip"); code != 410 || body["error"] != "import_closed" {
		t.Fatalf("skip after a completed import: %d %+v", code, body)
	}
}

// An install that already has messages is past its only chance: even with a backup present it is never
// offered the import, and nothing of v1 can overwrite or renumber what exists.
func TestImportV1IsNotOfferedToAnInstallThatHasMessages(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	root := t.TempDir()
	f.srv.ImportV1Root = root
	tinyV1Backup(t, root)
	if resp := f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "native"}, true); resp.StatusCode != 201 {
		t.Fatalf("native message: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// No status call first: every import route enforces the rule itself.
	for _, route := range [][2]string{{http.MethodGet, "/api/import-v1/preview"}, {http.MethodPost, "/api/import-v1/dry-run"}, {http.MethodPost, "/api/import-v1/commit"}} {
		if code, body := importStatus(t, f, route[0], route[1]); code != 410 {
			t.Fatalf("%s %s on a used install: %d %+v, want 410", route[0], route[1], code, body)
		}
	}
	if code, status := importStatus(t, f, http.MethodGet, "/api/import-v1/status"); code != 200 || status["state"] != "skipped" {
		t.Fatalf("used install status: %d %+v", code, status)
	}
}

// Setup events written before the first-run reservation sit inside the v1 number range: status and
// preview say so up front, instead of letting the owner run a check that is bound to fail.
func TestImportV1ReportsAMissedReservationUpFront(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	root := t.TempDir()
	f.srv.ImportV1Root = root
	tinyV1Backup(t, root)
	if code, status := importStatus(t, f, http.MethodGet, "/api/import-v1/status"); code != 200 || status["state"] != "ready" || status["reservation_missed"] != true {
		t.Fatalf("status: %d %+v", code, status)
	}
	if code, preview := importStatus(t, f, http.MethodGet, "/api/import-v1/preview"); code != 200 || preview["reservation_missed"] != true {
		t.Fatalf("preview: %d %+v", code, preview)
	}
}

// Skipping a partly finished import ends it ("done"); skipping one that never began declines it.
func TestImportV1SkipOfAStartedImportRecordsDone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	root := t.TempDir()
	f.srv.ImportV1Root = root
	tinyV1Backup(t, root)
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO legacy_import_source(source_key,map_digest,created_at,updated_at) VALUES ('partial','d','now','now')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if resp := f.request(http.MethodPost, "/api/import-v1/skip", nil, true); resp.StatusCode != 204 {
		t.Fatalf("skip: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if choice, err := f.db.V1ImportChoice(context.Background()); err != nil || choice != "done" {
		t.Fatalf("choice after skipping a started import: %q %v", choice, err)
	}
}

// A skip clears the dry run's receipt, and a commit that was already past its first check when the skip
// landed is refused under the import lock instead of running.
func TestImportV1CommitIsRefusedWhenASkipLandsFirst(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	root := t.TempDir()
	f.srv.ImportV1Root = root
	tinyV1Backup(t, root)
	resp := f.request(http.MethodGet, "/api/import-v1/preview", nil, false)
	var preview store.V1ImportPreview
	if err := json.NewDecoder(resp.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// The backup's "work" room is suggested as a project of its own, and a project needs a folder before an import can check.
	for key, project := range preview.Suggested.Projects {
		project.Folder = t.TempDir()
		preview.Suggested.Projects[key] = project
	}
	dryRun := func() string {
		resp := f.request(http.MethodPost, "/api/import-v1/dry-run", preview.Suggested, true)
		defer resp.Body.Close()
		var dry struct {
			Digest string `json:"digest"`
			Error  string `json:"error"`
			Reason string `json:"message"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&dry); err != nil || resp.StatusCode != 200 || dry.Digest == "" {
			t.Fatalf("dry run: %d %v %s %s", resp.StatusCode, err, dry.Error, dry.Reason)
		}
		return dry.Digest
	}
	digest := dryRun()
	// The skip handler drops the receipt.
	if resp := f.request(http.MethodPost, "/api/import-v1/skip", nil, true); resp.StatusCode != 204 {
		t.Fatalf("skip: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	f.srv.importMu.Lock()
	receipt := f.srv.importReceipt
	f.srv.importMu.Unlock()
	if receipt != "" {
		t.Fatalf("the receipt survived the skip: %q", receipt)
	}

	// The race itself: a commit that has passed its first check waits on the lock; a skip lands; the commit must refuse.
	g := newFixture(t)
	groot := t.TempDir()
	g.srv.ImportV1Root = groot
	tinyV1Backup(t, groot)
	resp = g.request(http.MethodGet, "/api/import-v1/preview", nil, false)
	if err := json.NewDecoder(resp.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for key, project := range preview.Suggested.Projects {
		project.Folder = t.TempDir()
		preview.Suggested.Projects[key] = project
	}
	f = g
	digest = dryRun()
	g.srv.importMu.Lock()
	result := make(chan int, 1)
	go func() {
		resp := g.request(http.MethodPost, "/api/import-v1/commit", map[string]any{"mapping": preview.Suggested, "digest": digest}, true)
		resp.Body.Close()
		result <- resp.StatusCode
	}()
	// Give the request time to pass the first check and block on the lock. If it is slower than that, it is
	// refused by the first check instead, which also fails the commit but weakens what this proves.
	time.Sleep(300 * time.Millisecond)
	if err := g.db.SetV1ImportChoice(context.Background(), "skipped"); err != nil {
		t.Fatal(err)
	}
	g.srv.importMu.Unlock()
	if code := <-result; code != 410 {
		t.Fatalf("a commit that lost the race to a skip must be refused with 410, got %d", code)
	}
	var phase string
	g.srv.importMu.Lock()
	phase = g.srv.importJob.Phase
	g.srv.importMu.Unlock()
	if phase != "" {
		t.Fatalf("the refused commit started an import job: %q", phase)
	}
}

package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/supervisor"
	"github.com/hrubymar10/aimebu/internal/testenv"
)

// agentWorktreeFolders lists the folders under the project's worktrees directory (none when it does not exist yet); any other read error fails
// the test.
func agentWorktreeFolders(t *testing.T, project string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(project, ".aimebu", "worktrees"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func (f *fixture) connectFakeSupervisor(t *testing.T) {
	t.Helper()
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, adapter.NewFake())
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(machineWaitGuard):
		t.Fatal("supervisor did not connect")
	}
}

// A worktree can be removed from an archived room: sending the agent back to the room's checkout only reads, so it works where making
// a room worktree is refused, and the record is detached instead of staying ready and blocking the room's removal.
func TestAgentWorktreeRemovalWorksInAnArchivedRoom(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	initRepo(t, f.project.Folder)
	f.connectFakeSupervisor(t)
	_, first, _ := f.spawnRequest(t, "one", "worker")
	status, second, raw := f.spawnRequest(t, "two", "worker")
	if status != 201 {
		t.Fatalf("second writer %d %+v", status, raw)
	}
	own, found, err := f.db.AgentWorktreeOf(t.Context(), second.ID)
	if err != nil || !found {
		t.Fatalf("worktree %+v found=%v err=%v", own, found, err)
	}
	current, err := f.db.AgentCardByID(t.Context(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp := f.request("POST", "/api/agents/"+second.ID+"/control", map[string]any{"desired": "stopped", "rev": current.DesiredRev}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stop: %d", resp.StatusCode)
	}
	if _, err := f.exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, f.room.ID); err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodDelete, "/api/agents/"+second.ID+"/worktree", map[string]any{}, true)
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || out["state"] != "removed" {
		t.Fatalf("removal from an archived room: %d %v", resp.StatusCode, out)
	}
	if got, _, _ := f.db.AgentWorktreeOf(t.Context(), second.ID); got.State != "removed" {
		t.Fatalf("the record must be detached, not left ready: %q", got.State)
	}
	if path, _ := f.db.AgentCheckoutPath(t.Context(), second.ID); path == own.Path {
		t.Fatalf("the agent must be back on a checkout that exists, got %q", path)
	}
	if _, err := os.Stat(filepath.Join(f.project.Folder, ".aimebu", "worktrees", f.room.ID+"-"+first.Slug)); !os.IsNotExist(err) {
		t.Fatalf("removal must not make a worktree for anyone else: %v", err)
	}
}

// A project viewer cannot resume a scope, and the refusal comes before any worktree or branch is made for a blocked writer.
func TestViewerResumeMakesNoWorktree(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	initRepo(t, f.project.Folder)
	f.connectFakeSupervisor(t)
	cookie, csrf := f.projectMember(t, "lookeronly", f.project.ID, "viewer")
	_, first, _ := f.spawnRequest(t, "one", "worker")
	post := func(c *http.Cookie, token, key, action string) int {
		t.Helper()
		state, err := f.db.ScopeControl(t.Context(), "room", f.room.ID)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"action": action, "rev": state.Rev})
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/control", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", token)
		req.Header.Set("Idempotency-Key", key)
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post(f.cookie, f.csrf, "stop-room", "stop"); got != 200 {
		t.Fatalf("stop: %d", got)
	}
	if status, _, raw := f.spawnRequest(t, "two", "worker"); status != 201 {
		t.Fatalf("late writer %d %+v", status, raw)
	}
	if got := post(cookie, csrf, "viewer-resume", "resume"); got != 403 {
		t.Fatalf("a viewer's resume: %d", got)
	}
	if _, err := os.Stat(filepath.Join(f.project.Folder, ".aimebu", "worktrees", f.room.ID+"-"+first.Slug)); !os.IsNotExist(err) {
		t.Fatalf("a refused resume must not leave a worktree behind: %v", err)
	}
	if out, err := exec.Command("git", "-C", f.project.Folder, "branch", "--list", "aimebu/"+f.room.ID+"-*").Output(); err != nil || len(bytes.TrimSpace(out)) != 0 {
		t.Fatalf("a refused resume must not leave a branch behind: %q err=%v", out, err)
	}
}

// A prepared worktree counts as used only when the agent's record points at it and is ready: an earlier removal leaves a "removed"
// record at the same path (the name is <room>-<slug>), and the unused folder must not survive next to it.
func TestPreparedWorktreeNextToARemovedRecordIsNotUsed(t *testing.T) {
	t.Parallel()
	prepared := store.AgentWorktree{Path: "/p/.aimebu/worktrees/room-olive", State: "ready"}
	for state, want := range map[string]bool{"ready": true, "removed": false, "preserved": false} {
		if got := usesPreparedWorktree(store.AgentWorktree{Path: prepared.Path, State: state}, prepared); got != want {
			t.Fatalf("state %q: used=%v, want %v", state, got, want)
		}
	}
	if usesPreparedWorktree(store.AgentWorktree{Path: "/elsewhere", State: "ready"}, prepared) {
		t.Fatal("another path is not the prepared worktree")
	}
}

// A worktree made for a writer is checked against the agent's launcher, as a start in any folder is: a launcher whose visible roots
// exclude it is refused with folder_unverified, on the fallback spawn and on reactivation, and nothing is left behind.
func TestOwnWorktreeOutsideTheLaunchersRootsIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	initRepo(t, f.project.Folder)
	f.connectFakeSupervisor(t)
	command := filepath.Join(t.TempDir(), "launcher")
	if err := testenv.WriteExecutable(command, []byte("#!/bin/sh\necho fixture-version\n"), 0755); err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodPost, "/api/machines/"+f.machineID+"/launchers", store.Launcher{Tool: "claude", Name: "scratch", Command: command, Enabled: true}, true)
	var created struct {
		Config store.Launcher `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || created.Config.ID == "" {
		t.Fatalf("launcher creation: %d %+v", resp.StatusCode, created)
	}
	launcher := created.Config
	f.setHarnessModels(t, launcher, store.HarnessModel{Slug: "haiku", Label: "Haiku", Default: true})
	spawn := func(key string) (int, store.AgentCard, map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"role_key": "worker", "tool": "claude", "launcher_id": launcher.ID, "model": "haiku"})
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/agents", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", key)
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&raw)
		data, _ := json.Marshal(raw)
		var card store.AgentCard
		_ = json.Unmarshal(data, &card)
		return resp.StatusCode, card, raw
	}
	restrictTo := func(root string) {
		t.Helper()
		roots, _ := json.Marshal([]string{root})
		if _, err := f.exec(`UPDATE machine_launcher SET visible_roots_json=? WHERE id=?`, string(roots), launcher.ID); err != nil {
			t.Fatal(err)
		}
	}
	status, first, raw := spawn("one")
	if status != 201 {
		t.Fatalf("first writer %d %+v", status, raw)
	}
	roomPath, err := f.db.AgentCheckoutPath(t.Context(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	listed := func() []string { return agentWorktreeFolders(t, f.project.Folder) }
	before := listed()
	// Fallback spawn: the room's folder is allowed, a worktree beside it is not.
	restrictTo(roomPath)
	status, _, raw = spawn("two")
	if status != 409 || raw["error"] != "folder_unverified" {
		t.Fatalf("a worktree outside the launcher's roots: %d %v", status, raw)
	}
	if after := listed(); len(after) != len(before) {
		t.Fatalf("the refused spawn must not leave a worktree behind: %v -> %v", before, after)
	}
	// Reactivation: a second writer that has its worktree, loses it and comes back while the first writer holds the folder.
	restrictTo(f.project.Folder)
	status, second, raw := spawn("three")
	if status != 201 {
		t.Fatalf("second writer %d %v", status, raw)
	}
	own, found, err := f.db.AgentWorktreeOf(t.Context(), second.ID)
	if err != nil || !found {
		t.Fatalf("worktree %+v found=%v err=%v", own, found, err)
	}
	control := func(desired string) (int, map[string]any) {
		t.Helper()
		current, err := f.db.AgentCardByID(t.Context(), second.ID)
		if err != nil {
			t.Fatal(err)
		}
		resp := f.request("POST", "/api/agents/"+second.ID+"/control", map[string]any{"desired": desired, "rev": current.DesiredRev}, true)
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if status, _ := control("stopped"); status != 200 {
		t.Fatalf("stop: %d", status)
	}
	resp = f.request(http.MethodDelete, "/api/agents/"+second.ID+"/worktree", map[string]any{}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	restrictTo(roomPath)
	if status, out := control("active"); status != 409 || out["error"] != "folder_unverified" {
		t.Fatalf("a reactivation into a worktree outside the roots: %d %v", status, out)
	}
	if _, err := os.Stat(own.Path); !os.IsNotExist(err) {
		t.Fatalf("the refused reactivation must not leave the worktree behind: %v", err)
	}
}

// A removal and a start of the same agent do not interleave: starting an agent that works in a worktree of its own waits for the worktree's
// lock, which a removal holds from its check to the end, and a removal that finds the agent started refuses instead of repointing it.
func TestReactivationWaitsForAWorktreeRemoval(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	initRepo(t, f.project.Folder)
	f.connectFakeSupervisor(t)
	_, _, _ = f.spawnRequest(t, "one", "worker")
	status, second, raw := f.spawnRequest(t, "two", "worker")
	if status != 201 {
		t.Fatalf("second writer %d %+v", status, raw)
	}
	own, found, err := f.db.AgentWorktreeOf(t.Context(), second.ID)
	if err != nil || !found {
		t.Fatalf("worktree %+v found=%v err=%v", own, found, err)
	}
	control := func(desired string) int {
		t.Helper()
		current, err := f.db.AgentCardByID(t.Context(), second.ID)
		if err != nil {
			t.Fatal(err)
		}
		resp := f.request("POST", "/api/agents/"+second.ID+"/control", map[string]any{"desired": desired, "rev": current.DesiredRev}, true)
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := control("stopped"); got != 200 {
		t.Fatalf("stop: %d", got)
	}
	unlock := f.srv.lockAgentWorktree(own.Name) // a removal is under way
	done := make(chan int, 1)
	go func() { done <- control("active") }()
	select {
	case got := <-done:
		t.Fatalf("a start must wait for the removal that holds the worktree's lock, got %d", got)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case got := <-done:
		if got != 200 {
			t.Fatalf("the start after the removal: %d", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the start never went on once the lock was free")
	}
	// An agent that was started is not detached from its folder: the store refuses, whatever the server saw.
	if err := f.db.DetachAgentWorktree(t.Context(), second.ID, own.MachineID, f.project.Folder); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("detaching an agent that is not stopped: %v", err)
	}
	if got, _, _ := f.db.AgentWorktreeOf(t.Context(), second.ID); got.State != "ready" {
		t.Fatalf("the worktree must stay recorded: %q", got.State)
	}
	if path, _ := f.db.AgentCheckoutPath(t.Context(), second.ID); path != own.Path {
		t.Fatalf("the agent must stay in its worktree, got %q", path)
	}
}

// A bulk resume that skips a writer (its launcher cannot see the worktree made for it) holds no lock on that worktree afterwards: the request
// finishes, and the next start, removal or resume of that name goes straight through instead of waiting for ever.
func TestBulkResumeSkippingAWriterLeavesItsWorktreeUnlocked(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	initRepo(t, f.project.Folder)
	f.connectFakeSupervisor(t)
	command := filepath.Join(t.TempDir(), "launcher")
	if err := testenv.WriteExecutable(command, []byte("#!/bin/sh\necho fixture-version\n"), 0755); err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodPost, "/api/machines/"+f.machineID+"/launchers", store.Launcher{Tool: "claude", Name: "scratch", Command: command, Enabled: true}, true)
	var created struct {
		Config store.Launcher `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	launcher := created.Config
	f.setHarnessModels(t, launcher, store.HarnessModel{Slug: "haiku", Label: "Haiku", Default: true})
	spawn := func(key string) store.AgentCard {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"role_key": "worker", "tool": "claude", "launcher_id": launcher.ID, "model": "haiku"})
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/agents", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", key)
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var card store.AgentCard
		if resp.StatusCode != 201 || json.NewDecoder(resp.Body).Decode(&card) != nil {
			t.Fatalf("spawn %s: %d", key, resp.StatusCode)
		}
		return card
	}
	first := spawn("one")
	post := func(key, action string) int {
		t.Helper()
		state, err := f.db.ScopeControl(t.Context(), "room", f.room.ID)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"action": action, "rev": state.Rev})
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/control", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", key)
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("stop-room", "stop"); got != 200 {
		t.Fatalf("stop: %d", got)
	}
	second := spawn("two")
	roomPath, err := f.db.AgentCheckoutPath(t.Context(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := json.Marshal([]string{roomPath})
	if _, err := f.exec(`UPDATE machine_launcher SET visible_roots_json=? WHERE id=?`, string(roots), launcher.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- post("resume-room", "resume") }()
	select {
	case got := <-done:
		if got != 409 { // no worktree could be made for the second writer, so its refusal stands, as it did before
			t.Fatalf("resume: %d", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the resume hangs on a worktree lock it holds itself")
	}
	// Only the project folder and the worktree the second writer already had are left: the one made ahead of the resume for the skipped writer
	// is taken away.
	listing, err := exec.Command("git", "-C", f.project.Folder, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(listing), "worktree "); got != 2 {
		t.Fatalf("the skipped writer's worktree must be taken away: %d worktrees\n%s", got, listing)
	}
	// Nothing is left locked: the skipped writer's name and the other's can both be taken at once.
	taken := make(chan struct{})
	go func() {
		for _, name := range []string{store.AgentWorktreeName(f.room.ID, second.Slug), store.AgentWorktreeName(f.room.ID, first.Slug)} {
			f.srv.lockAgentWorktree(name)()
		}
		close(taken)
	}()
	select {
	case <-taken:
	case <-time.After(5 * time.Second):
		t.Fatal("a worktree lock was left held after the resume")
	}
}

// An owner's approval of a boundary request starts the agent, so it waits for the worktree's lock like any start: a removal holding it cannot
// have the folder taken out from under an agent that the approval starts.
func TestBoundaryApprovalWaitsForAWorktreeRemoval(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	initRepo(t, f.project.Folder)
	f.connectFakeSupervisor(t)
	_, _, _ = f.spawnRequest(t, "one", "worker")
	status, second, raw := f.spawnRequest(t, "two", "worker")
	if status != 201 {
		t.Fatalf("second writer %d %+v", status, raw)
	}
	own, found, err := f.db.AgentWorktreeOf(t.Context(), second.ID)
	if err != nil || !found {
		t.Fatalf("worktree %+v found=%v err=%v", own, found, err)
	}
	// An open approval for the agent, as a reactivation outside its folder leaves one.
	if _, err := f.exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,options_json,created_at) VALUES ('inb_lock',?,?,?,'user','x','approval','{}','2026-11-05T10:00:00Z')`, f.project.ID, f.room.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`INSERT INTO boundary_pending(inbox_id,agent_id,machine_id,canonical_path,operation,requested_by_kind,requested_by_id,created_at) VALUES ('inb_lock',?,?,?,'reactivate','user','x','2026-11-05T10:00:00Z')`, second.ID, own.MachineID, own.Path); err != nil {
		t.Fatal(err)
	}
	unlock := f.srv.lockAgentWorktree(own.Name) // a removal is under way
	done := make(chan struct{})
	go func() {
		defer close(done)
		body, _ := json.Marshal(map[string]any{"choice": "deny", "rev": 0})
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/inbox/inb_lock/decision", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", "decide-lock")
		req.AddCookie(f.cookie)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-done:
		t.Fatal("an approval must wait for the removal that holds the worktree's lock")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the decision never went on once the lock was free")
	}
}

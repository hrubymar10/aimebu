package server

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

// Removing an agent: only a stopped one, only by the project's owner or an operator. Its worktree of its own is removed the way the worktree's
// own removal does (the branch stays), it leaves its rooms, and what it said stays under its name, marked as removed.
func TestRemoveAgentTakesAStoppedWriterOutAndItsMessagesStay(t *testing.T) {
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
	if _, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: "agent", AuthorID: second.ID, Body: "two was here"}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := f.projectMember(t, "lookeronly", f.project.ID, "viewer")
	del := func(c *http.Cookie, token, id string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodDelete, f.http.URL+"/api/agents/"+id, bytes.NewReader([]byte("{}")))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", token)
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, out := del(f.cookie, f.csrf, second.ID); code != 409 || out["error"] != "agent_not_stopped" {
		t.Fatalf("an agent that is not stopped: %d %v", code, out)
	}
	card, err := f.db.AgentCardByID(t.Context(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp := f.request("POST", "/api/agents/"+second.ID+"/control", map[string]any{"desired": "stopped", "rev": card.DesiredRev}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stop: %d", resp.StatusCode)
	}
	if code, _ := del(cookie, csrf, second.ID); code != 403 {
		t.Fatalf("a viewer may not remove an agent: %d", code)
	}
	code, out := del(f.cookie, f.csrf, second.ID)
	if code != 200 || out["slug"] != second.Slug {
		t.Fatalf("removal: %d %v", code, out)
	}
	tree, _ := out["worktree"].(map[string]any)
	if tree["state"] != "removed" {
		t.Fatalf("a clean worktree is removed with the agent: %v", out)
	}
	if got, _, _ := f.db.AgentWorktreeOf(t.Context(), second.ID); got.State != "removed" {
		t.Fatalf("the record says %q", got.State)
	}
	if _, err := os.Stat(own.Path); !os.IsNotExist(err) {
		t.Fatalf("the folder should be gone: %v", err)
	}
	if out, err := exec.Command("git", "-C", f.project.Folder, "branch", "--list", own.Branch+"-removed-1").Output(); err != nil || len(bytes.TrimSpace(out)) == 0 {
		t.Fatalf("the branch stays, under another name: %q err=%v", out, err)
	}
	// It is out of the room and not found again; the other agent is untouched.
	cards, err := f.db.AgentCardsForRoom(t.Context(), f.room.ID)
	if err != nil || len(cards) != 1 || cards[0].ID != first.ID {
		t.Fatalf("cards after the removal: %+v err=%v", cards, err)
	}
	if code, out := del(f.cookie, f.csrf, second.ID); code != 404 {
		t.Fatalf("a second removal: %d %v", code, out)
	}
	// Its message stays, and the page is told whose it was and that it is removed.
	resp = f.request("GET", "/api/rooms/"+f.room.ID+"/messages", nil, true)
	var messages []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&messages)
	resp.Body.Close()
	var seen bool
	for _, m := range messages {
		if m["author_id"] == second.ID {
			seen = true
			if m["author_name"] != second.Slug || m["author_project_id"] != f.project.ID {
				t.Fatalf("the message should name its removed author: %v", m)
			}
		}
	}
	if !seen {
		t.Fatalf("the message should stay: %v", messages)
	}
	if _, err := os.Stat(filepath.Join(f.project.Folder, ".aimebu", "worktrees", f.room.ID+"-"+first.Slug)); !os.IsNotExist(err) {
		t.Fatalf("nothing was made for anyone else: %v", err)
	}
}

// An export names a removed agent with the project it belonged to, which in a chat between agents is not the project of the room being exported.
func TestExportNamesARemovedAgentByItsOwnProject(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	names := &exportNames{ctx: t.Context(), store: f.db, project: "The room's project", byID: map[string]string{}}
	removed := func(project string) store.Message {
		return store.Message{AuthorKind: "agent", AuthorID: "agt_gone", AuthorName: "opal", AuthorProjectID: project}
	}
	if got, want := names.of(removed(f.project.ID)), "opal@"+f.project.Name+" (removed)"; got != want {
		t.Fatalf("its own project: %q, want %q", got, want)
	}
	if got, want := names.of(removed("")), "opal@The room's project (removed)"; got != want {
		t.Fatalf("no project recorded: %q, want %q", got, want)
	}
	if got, want := names.of(removed("prj_unknown")), "opal@The room's project (removed)"; got != want {
		t.Fatalf("a project that cannot be read: %q, want %q", got, want)
	}
}

// An agent whose worktree has work in it is not removed, so that its name is not freed while its work could be found by the next agent of that
// name. Once the person has cleared the worktree the agent goes, its branch is kept under another name, and a new agent of the same name starts from
// the room's base as it stands now, not from where the old one began.
func TestRemoveAgentKeepsItsWorkAndFreesTheNameForAFreshStart(t *testing.T) {
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
	stop := func(id string) {
		t.Helper()
		card, err := f.db.AgentCardByID(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		resp := f.request("POST", "/api/agents/"+id+"/control", map[string]any{"desired": "stopped", "rev": card.DesiredRev}, true)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("stop: %d", resp.StatusCode)
		}
	}
	remove := func(id string) (int, map[string]any) {
		t.Helper()
		resp := f.request("DELETE", "/api/agents/"+id, map[string]any{}, true)
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	scratch := filepath.Join(own.Path, "scratch.txt")
	if err := os.WriteFile(scratch, []byte("not saved anywhere else\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stop(second.ID)
	if code, out := remove(second.ID); code != 409 || out["error"] != "worktree_preserved" {
		t.Fatalf("work in the worktree must stop the removal: %d %v", code, out)
	}
	if _, err := f.db.AgentRemovalInfo(t.Context(), second.ID); err != nil {
		t.Fatalf("the agent is still there: %v", err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("the work is untouched: %v", err)
	}
	// The person clears the worktree; the room's base moves on; now the agent can go.
	if err := os.Remove(scratch); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.project.Folder, "later.txt"), []byte("the base moved on\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.project.Folder, "add", "later.txt")
	gitTest(t, f.project.Folder, "commit", "-qm", "later")
	code, out := remove(second.ID)
	if code != 200 {
		t.Fatalf("removal after the clean-up: %d %v", code, out)
	}
	tree, _ := out["worktree"].(map[string]any)
	if tree["state"] != "removed" || tree["kept_branch"] != own.Branch+"-removed-1" {
		t.Fatalf("the branch is kept under another name: %v", tree)
	}
	if out, err := exec.Command("git", "-C", f.project.Folder, "branch", "--list", own.Branch).Output(); err != nil || len(bytes.TrimSpace(out)) != 0 {
		t.Fatalf("the old name is free: %q err=%v", out, err)
	}
	if out, err := exec.Command("git", "-C", f.project.Folder, "branch", "--list", own.Branch+"-removed-1").Output(); err != nil || len(bytes.TrimSpace(out)) == 0 {
		t.Fatalf("the work is kept: %q err=%v", out, err)
	}
	// The worktree's name is free again: a worktree made under it now starts from the base as it stands (the worktree package's own test shows the
	// same through Ensure; here the machine is asked the way a spawn asks it).
	machine, root, err := f.srv.ownWorktreeHost(t.Context(), own)
	if err != nil {
		t.Fatal(err)
	}
	made, err := f.srv.machineWorktree(t.Context(), machine, root, own.Name, "", "ensure", false)
	if err != nil {
		t.Fatal(err)
	}
	head := func(dir string) string {
		out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(bytes.TrimSpace(out))
	}
	if head(made.Path) != head(f.project.Folder) {
		t.Fatalf("a worktree under the freed name starts from the base as it is now: %s vs %s", head(made.Path), head(f.project.Folder))
	}
	if _, err := os.Stat(filepath.Join(made.Path, "later.txt")); err != nil {
		t.Fatalf("it has the base's newer file: %v", err)
	}
	_ = first
}

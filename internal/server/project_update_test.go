package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestProjectFolderMustBeAnAbsoluteFolderThatExists(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, folder := range map[string]string{"relative": "work/repo", "dot": ".", "missing": filepath.Join(t.TempDir(), "nope"), "a file": file} {
		resp := f.request("POST", "/api/projects", map[string]string{"name": "p", "folder": folder}, true)
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != 400 || body["error"] != "invalid_folder" {
			t.Fatalf("%s: %d %+v", name, resp.StatusCode, body)
		}
	}
	// A trailing slash is tidied, not refused.
	dir := t.TempDir()
	resp := f.request("POST", "/api/projects", map[string]string{"name": "p", "folder": dir + "/"}, true)
	var created store.Project
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || created.Folder != dir {
		t.Fatalf("create: %d %+v", resp.StatusCode, created)
	}
}

func TestProjectCanBeRenamedAndItsFolderFixedUntilAgentsExist(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	patch := func(body map[string]string) (int, map[string]any) {
		resp := f.request("PATCH", "/api/projects/"+f.project.ID, body, true)
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		return resp.StatusCode, out
	}
	if status, out := patch(map[string]string{"name": "  Renamed  "}); status != 200 || out["name"] != "Renamed" || out["folder"] != f.project.Folder {
		t.Fatalf("rename: %d %+v", status, out)
	}
	if status, out := patch(map[string]string{"name": " "}); status != 400 {
		t.Fatalf("blank name: %d %+v", status, out)
	}
	if status, out := patch(map[string]string{"folder": "relative/path"}); status != 400 || out["error"] != "invalid_folder" {
		t.Fatalf("relative folder: %d %+v", status, out)
	}
	if status, out := patch(map[string]string{}); status != 400 {
		t.Fatalf("empty patch: %d %+v", status, out)
	}
	moved := t.TempDir()
	if status, out := patch(map[string]string{"folder": moved}); status != 200 || out["folder"] != moved {
		t.Fatalf("move: %d %+v", status, out)
	}
	if folder, err := f.db.ProjectFolder(t.Context(), f.project.ID); err != nil || folder != moved {
		t.Fatalf("stored folder %q %v", folder, err)
	}
	// With an agent in the project the folder stays: its checkouts live under it. A rename still works.
	f.createToolAgent(t, "role_worker_default")
	if status, out := patch(map[string]string{"folder": t.TempDir()}); status != 409 || out["error"] != "folder_in_use" {
		t.Fatalf("move with agents: %d %+v", status, out)
	}
	if status, out := patch(map[string]string{"name": "Again"}); status != 200 || out["name"] != "Again" {
		t.Fatalf("rename with agents: %d %+v", status, out)
	}
	// The same folder is not a move, so it is fine even with agents.
	if status, out := patch(map[string]string{"folder": moved}); status != 200 {
		t.Fatalf("same folder: %d %+v", status, out)
	}
}

func TestRenamingAProjectRenamesItsHomeRoom(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	titles := func() map[string]string {
		resp := f.request("GET", "/api/projects/"+f.project.ID+"/rooms", nil, false)
		var rooms []store.Room
		if err := json.NewDecoder(resp.Body).Decode(&rooms); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		out := map[string]string{}
		for _, room := range rooms {
			out[room.Kind] = room.Title
		}
		return out
	}
	if got := titles(); got["home"] != "p" {
		t.Fatalf("before: %v", got)
	}
	resp := f.request("PATCH", "/api/projects/"+f.project.ID, map[string]string{"name": "Renamed"}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("rename: %d", resp.StatusCode)
	}
	if got := titles(); got["home"] != "Renamed" || got["task"] != "task" {
		t.Fatalf("after: %v", got)
	}
	// A home room somebody retitled keeps its title.
	if _, err := f.exec(`UPDATE room SET title='Hand-made' WHERE project_id=? AND kind='home'`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	resp = f.request("PATCH", "/api/projects/"+f.project.ID, map[string]string{"name": "Again"}, true)
	resp.Body.Close()
	if got := titles(); got["home"] != "Hand-made" {
		t.Fatalf("retitled home room: %v", got)
	}
}

func TestProjectFolderDoesNotMoveUnderALiveWorktree(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	if _, err := f.exec(`INSERT INTO room_worktree(room_id,base_ref,branch_name,relative_path,state,created_at,updated_at) VALUES (?,'main','b','.worktrees/b','ready','now','now')`, f.room.ID); err != nil {
		t.Fatal(err)
	}
	resp := f.request("PATCH", "/api/projects/"+f.project.ID, map[string]string{"folder": t.TempDir()}, true)
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 409 || out["error"] != "folder_in_use" {
		t.Fatalf("move under a worktree: %d %+v", resp.StatusCode, out)
	}
	// Once the worktree is removed the folder can move.
	if _, err := f.exec(`UPDATE room_worktree SET state='removed'`); err != nil {
		t.Fatal(err)
	}
	resp = f.request("PATCH", "/api/projects/"+f.project.ID, map[string]string{"folder": t.TempDir()}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("move after removal: %d", resp.StatusCode)
	}
}

func TestSendingTheFolderItAlreadyHasIsNotAChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	// A project from before folders were checked may hold one that no longer exists; renaming it must not trip over that.
	if _, err := f.exec(`UPDATE project SET folder='relative/gone' WHERE id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	resp := f.request("PATCH", "/api/projects/"+f.project.ID, map[string]string{"name": "Kept", "folder": "relative/gone"}, true)
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || out["name"] != "Kept" || out["folder"] != "relative/gone" {
		t.Fatalf("rename with the same folder: %d %+v", resp.StatusCode, out)
	}
}

func TestAProjectOwnerRenamesButOnlyAnAdminMovesTheFolder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	token := readInvite(t, f.request("POST", "/api/invites", map[string]string{"project_id": f.project.ID, "role": "operator"}, true))
	resp := userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": token, "login": "helper", "password": "a secure password"})
	resp.Body.Close()
	resp = userRequest(t, f, nil, "", "POST", "/api/login", map[string]string{"login": "helper", "password": "a secure password"})
	cookie := resp.Cookies()[0]
	var login struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	patch := func(body map[string]string) int {
		resp := userRequest(t, f, cookie, login.CSRF, "PATCH", "/api/projects/"+f.project.ID, body)
		resp.Body.Close()
		return resp.StatusCode
	}
	// An operator may do neither.
	if status := patch(map[string]string{"name": "mine now"}); status != 403 {
		t.Fatalf("operator rename: %d", status)
	}
	if _, err := f.exec(`UPDATE membership SET role='owner' WHERE project_id=? AND user_id=(SELECT id FROM user WHERE login='helper')`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if status := patch(map[string]string{"name": "Helper's name"}); status != 200 {
		t.Fatalf("project owner rename: %d", status)
	}
	if status := patch(map[string]string{"folder": t.TempDir()}); status != 403 {
		t.Fatalf("project owner folder move: %d", status)
	}
	// Naming the folder it already has is not a move.
	if status := patch(map[string]string{"name": "Helper again", "folder": f.project.Folder}); status != 200 {
		t.Fatalf("project owner with an unchanged folder: %d", status)
	}
}

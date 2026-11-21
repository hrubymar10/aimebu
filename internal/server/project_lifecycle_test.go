package server

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestOwnerDeletesLiveProjectDirectly(t *testing.T) {
	f := newFixture(t)
	f.seed()
	path := "/api/projects/" + f.project.ID
	wrong := f.request(http.MethodDelete, path, map[string]string{"name": "wrong"}, true)
	wrong.Body.Close()
	if wrong.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong name before archive: %d", wrong.StatusCode)
	}
	if project, err := f.db.ProjectByID(t.Context(), f.project.ID); err != nil || project.ArchivedAt != "" {
		t.Fatalf("wrong name archived project: %+v %v", project, err)
	}
	resp := f.request(http.MethodDelete, path, map[string]string{"name": f.project.Name}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("direct project delete: %d", resp.StatusCode)
	}
	if _, err := f.db.ProjectByID(t.Context(), f.project.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted project remains: %v", err)
	}
}

func TestProjectArchiveReopenAndDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 64), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	doomed := f.project

	// A second project that must come through untouched, and the global rows a deletion must not turn into someone else's.
	var keep store.Project
	resp := f.request("POST", "/api/projects", map[string]string{"name": "keep", "folder": t.TempDir()}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("second project: %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&keep)
	resp.Body.Close()
	var keepRoom store.Room
	resp = f.request("POST", "/api/projects/"+keep.ID+"/rooms", map[string]string{"title": "keep task"}, true)
	json.NewDecoder(resp.Body).Decode(&keepRoom)
	resp.Body.Close()
	post := func(roomID, body string) {
		t.Helper()
		resp := f.request("POST", "/api/rooms/"+roomID+"/messages", map[string]any{"body": body}, true)
		resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("post %q: %d", body, resp.StatusCode)
		}
	}
	post(keepRoom.ID, "kept message")
	keepUpload := func() store.Attachment {
		resp, a := uploadImage(t, f, keepRoom.ID, testPNG)
		resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("upload: %d", resp.StatusCode)
		}
		return a
	}
	keptUpload := keepUpload()
	readMessages := func(roomID string) int {
		t.Helper()
		resp := f.request("GET", "/api/rooms/"+roomID+"/messages", nil, false)
		defer resp.Body.Close()
		var list []store.Message
		json.NewDecoder(resp.Body).Decode(&list)
		return len(list)
	}
	keepMessages := readMessages(keepRoom.ID)
	var rolesBefore int
	{
		resp := f.request("GET", "/api/roles", nil, false)
		var roles []map[string]any
		json.NewDecoder(resp.Body).Decode(&roles)
		resp.Body.Close()
		rolesBefore = len(roles)
		if rolesBefore == 0 {
			t.Fatal("no global roles to compare")
		}
	}

	// What the doomed project holds: messages, a reaction, an upload, an agent and a schedule.
	post(f.room.ID, "to be deleted")
	var messages []store.Message
	resp = f.request("GET", "/api/rooms/"+f.room.ID+"/messages", nil, false)
	json.NewDecoder(resp.Body).Decode(&messages)
	resp.Body.Close()
	if len(messages) == 0 {
		t.Fatal("no message to react to")
	}
	resp = f.request("PUT", "/api/messages/"+messages[0].ID+"/reactions/"+"%F0%9F%91%8D", nil, true)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("react: %d", resp.StatusCode)
	}
	// Different bytes from the other project's upload: identical files share one blob, which a delete must leave while anyone still uses it.
	resp, doomedUpload := uploadImage(t, f, f.room.ID, append(append([]byte{}, testPNG...), 0))
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("doomed upload: %d", resp.StatusCode)
	}
	status, card, raw := f.spawnRequest(t, "archive-agent", "worker")
	if status != 201 {
		t.Fatalf("spawn %d %+v", status, raw)
	}
	resp = f.request("POST", "/api/rooms/"+f.room.ID+"/schedules", map[string]any{"target_lead": true, "body": "check", "kind": "interval", "timezone": "UTC", "interval_seconds": 300}, true)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("schedule: %d", resp.StatusCode)
	}
	member := func(login, role string) (*http.Cookie, string, string) {
		cookie, csrf := registerSecondUser(t, f, login)
		me, err := f.db.UserByLogin(t.Context(), login)
		if err != nil {
			t.Fatal(err)
		}
		if role != "" {
			if _, err := f.exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, doomed.ID, me.ID, role); err != nil {
				t.Fatal(err)
			}
		}
		return cookie, csrf, me.ID
	}
	viewerCookie, viewerCSRF, viewerID := member("viewer", "viewer")
	outsiderCookie, outsiderCSRF, outsiderID := member("outsider", "")

	archive := func(path string) *http.Response { return f.request("POST", "/api/projects/"+doomed.ID+path, nil, true) }

	// Direct delete archives the live project, but a pending agent stop keeps it for retry.
	resp = f.request("DELETE", "/api/projects/"+doomed.ID, map[string]string{"name": doomed.Name}, true)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("delete of a live project: %d", resp.StatusCode)
	}
	if kept, err := f.db.ProjectByID(t.Context(), doomed.ID); err != nil || kept.ArchivedAt == "" {
		t.Fatalf("busy direct delete should retain the archived project: %+v %v", kept, err)
	}
	for name, who := range map[string][2]any{"viewer": {viewerCookie, viewerCSRF}, "outsider": {outsiderCookie, outsiderCSRF}} {
		resp := f.requestAs(t, who[0].(*http.Cookie), who[1].(string), "POST", "/api/projects/"+doomed.ID+"/archive", nil)
		resp.Body.Close()
		if resp.StatusCode != 403 && resp.StatusCode != 404 {
			t.Fatalf("%s archived the project: %d", name, resp.StatusCode)
		}
	}

	// Archiving stops its agents and switches its schedule off, and every change after it is refused, reading is not.
	resp = archive("/archive")
	var archived map[string]any
	json.NewDecoder(resp.Body).Decode(&archived)
	resp.Body.Close()
	if resp.StatusCode != 200 || archived["archived_at"] == nil {
		t.Fatalf("archive: %d %v", resp.StatusCode, archived)
	}
	if got, err := f.db.AgentCardByID(t.Context(), card.ID); err != nil || got.Desired != "stopped" {
		t.Fatalf("the agent is not stopped: %+v %v", got, err)
	}
	schedules, err := f.db.ListSchedules(t.Context(), f.room.ID)
	if err != nil || len(schedules) == 0 {
		t.Fatalf("schedules: %v %v", schedules, err)
	}
	for _, schedule := range schedules {
		if schedule.Enabled {
			t.Fatalf("a schedule is still on: %+v", schedule)
		}
	}
	refused := func(method, path string, body any) {
		t.Helper()
		resp := f.request(method, path, body, true)
		defer resp.Body.Close()
		var problem struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&problem)
		if resp.StatusCode != 409 || problem.Error != "project_archived" {
			t.Errorf("%s %s on an archived project: %d %q", method, path, resp.StatusCode, problem.Error)
		}
	}
	// Starting an agent is refused with the archive named before the launcher checks, which would answer about something else (a Docker
	// agent on a loopback bind says the bind is unavailable).
	if _, err := f.exec(`UPDATE agent SET tool='codex',variant='docker' WHERE id=?`, card.ID); err != nil {
		t.Fatal(err)
	}
	current, err := f.db.AgentCardByID(t.Context(), card.ID)
	if err != nil {
		t.Fatal(err)
	}
	refused("POST", "/api/agents/"+card.ID+"/control", map[string]any{"desired": "active", "rev": current.DesiredRev})
	refused("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]any{"body": "late"})
	refused("PUT", "/api/messages/"+messages[0].ID+"/reactions/%F0%9F%91%8E", nil)
	refused("POST", "/api/projects/"+doomed.ID+"/rooms", map[string]string{"title": "new"})
	refused("POST", "/api/rooms/"+f.room.ID+"/agents", map[string]string{"role_key": "worker", "tool": "fake", "variant": "test", "model": "test"})
	refused("POST", "/api/agents/"+card.ID+"/control", map[string]any{"desired": "active", "rev": card.DesiredRev + 1})
	refused("PUT", "/api/projects/"+doomed.ID+"/settings", map[string]any{"worktree_mode": "off"})
	for _, path := range []string{"/api/rooms/" + f.room.ID + "/messages", "/api/projects/" + doomed.ID + "/rooms", "/api/rooms/" + f.room.ID} {
		resp := f.request("GET", path, nil, false)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("reading %s while archived: %d", path, resp.StatusCode)
		}
	}
	// Someone signed in who is not a member learns nothing either: the route answers as it does for any room they cannot see.
	hidden := f.requestAs(t, outsiderCookie, outsiderCSRF, "POST", "/api/rooms/"+f.room.ID+"/messages", map[string]any{"body": "peek"})
	var peek struct {
		Error string `json:"error"`
	}
	json.NewDecoder(hidden.Body).Decode(&peek)
	hidden.Body.Close()
	if peek.Error == "project_archived" || hidden.StatusCode == 201 {
		t.Fatalf("a non-member was told the project is archived (or could write): %d %q", hidden.StatusCode, peek.Error)
	}
	// Someone who is not signed in learns nothing about it.
	anon, err := http.Post(f.http.URL+"/api/rooms/"+f.room.ID+"/messages", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode == 409 {
		t.Fatal("an anonymous request was told the project is archived")
	}
	// Reading the room and one's own notification setting stay possible.
	resp = f.request("POST", "/api/rooms/"+f.room.ID+"/read", map[string]any{}, true)
	resp.Body.Close()
	if resp.StatusCode == 409 {
		t.Errorf("marking a room read was refused")
	}
	// Archiving again changes nothing.
	resp = archive("/archive")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("second archive: %d", resp.StatusCode)
	}

	// Reopening makes it writable again; the agent stays stopped.
	resp = archive("/reopen")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("reopen: %d", resp.StatusCode)
	}
	post(f.room.ID, "after reopening")
	if got, err := f.db.AgentCardByID(t.Context(), card.ID); err != nil || got.Desired != "stopped" {
		t.Fatalf("reopening started the agent: %+v %v", got, err)
	}

	// Delete: the name must be typed, a live worktree refuses it, and a running agent refuses it.
	resp = archive("/archive")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("archive again: %d", resp.StatusCode)
	}
	resp = f.request("DELETE", "/api/projects/"+doomed.ID, map[string]string{"name": "not the name"}, true)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("delete with the wrong name: %d", resp.StatusCode)
	}
	if _, err := f.db.RequestRoomWorktree(t.Context(), f.room.ID, "main"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordRoomWorktree(t.Context(), f.room.ID, f.machineID, "/tmp/doomed-worktree", "main", "base", "ready", "", 0); err != nil {
		t.Fatal(err)
	}
	resp = f.request("DELETE", "/api/projects/"+doomed.ID, map[string]string{"name": doomed.Name}, true)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("delete with a live worktree: %d", resp.StatusCode)
	}
	if err := f.db.RecordRoomWorktree(t.Context(), f.room.ID, f.machineID, "/tmp/doomed-worktree", "main", "base", "removed", "", 0); err != nil {
		t.Fatal(err)
	}
	for _, who := range [][2]any{{viewerCookie, viewerCSRF}, {outsiderCookie, outsiderCSRF}} {
		resp := f.requestAs(t, who[0].(*http.Cookie), who[1].(string), "DELETE", "/api/projects/"+doomed.ID, map[string]string{"name": doomed.Name})
		resp.Body.Close()
		if resp.StatusCode != 403 && resp.StatusCode != 404 {
			t.Fatalf("a non-owner deleted the project: %d", resp.StatusCode)
		}
	}
	_, _, doomedPath, err := f.db.AttachmentByID(t.Context(), doomedUpload.ID, f.ownerUserID(t))
	if err != nil {
		t.Fatal(err)
	}
	_, _, keptPath, err := f.db.AttachmentByID(t.Context(), keptUpload.ID, f.ownerUserID(t))
	if err != nil {
		t.Fatal(err)
	}
	if doomedPath == keptPath {
		t.Fatal("the two uploads share a file; the check below would prove nothing")
	}
	if _, err := os.Stat(doomedPath); err != nil {
		t.Fatalf("the upload is gone before the delete: %v", err)
	}
	stoppedCard, err := f.db.AgentCardByID(t.Context(), card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckAgentControl(t.Context(), card.ID, stoppedCard.Generation, fmt.Sprint(stoppedCard.DesiredRev)); err != nil {
		t.Fatal(err)
	}
	resp = f.request("DELETE", "/api/projects/"+doomed.ID, map[string]string{"name": doomed.Name}, true)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}

	// Nothing of it is left to read, the other project is as it was, and the global roles are the same.
	for _, path := range []string{"/api/projects/" + doomed.ID + "/rooms", "/api/rooms/" + f.room.ID, "/api/rooms/" + f.room.ID + "/messages"} {
		resp := f.request("GET", path, nil, false)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("%s still answers after the delete", path)
		}
	}
	if got := readMessages(keepRoom.ID); got != keepMessages {
		t.Errorf("the other project's messages: %d, want %d", got, keepMessages)
	}
	{
		resp := f.request("GET", "/api/roles", nil, false)
		var roles []map[string]any
		json.NewDecoder(resp.Body).Decode(&roles)
		resp.Body.Close()
		if len(roles) != rolesBefore {
			t.Errorf("global roles: %d, want %d", len(roles), rolesBefore)
		}
	}
	if _, err := os.Stat(doomedPath); !os.IsNotExist(err) {
		t.Errorf("the deleted project's upload remains: %v", err)
	}
	if _, err := os.Stat(keptPath); err != nil {
		t.Errorf("the other project's upload was removed: %v", err)
	}
	resp = f.request("GET", "/api/projects", nil, false)
	var listed []map[string]any
	json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	for _, project := range listed {
		if project["id"] == doomed.ID {
			t.Fatal("the deleted project is still listed")
		}
	}
	// The members are told it is gone, though the project they were members of no longer exists; outsiders are not.
	has := func(userID string) bool {
		events, err := f.db.EventsAfter(t.Context(), userID, 0, math.MaxInt64)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Kind == "project_removed" {
				return true
			}
		}
		return false
	}
	if !has(viewerID) {
		t.Error("a member was not told the project was removed")
	}
	if has(outsiderID) {
		t.Error("someone who was never a member saw the removal")
	}
}

// An admin who is not a member of the project can still archive, reopen and delete it: the owner or an admin decides, not the project's
// own membership.
func TestAdminWhoIsNotAMemberManagesAProjectsLifecycle(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 64), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	status, agent, raw := f.spawnRequest(t, "admin-lifecycle-agent", "worker")
	if status != 201 {
		t.Fatalf("spawn %d %+v", status, raw)
	}
	cookie, csrf := registerSecondUser(t, f, "outsideadmin")
	admin, err := f.db.UserByLogin(t.Context(), "outsideadmin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`UPDATE user SET admin=1 WHERE id=?`, admin.ID); err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, body any) int {
		resp := f.requestAs(t, cookie, csrf, method, path, body)
		resp.Body.Close()
		return resp.StatusCode
	}
	project := "/api/projects/" + f.project.ID
	if got := call("POST", project+"/archive", nil); got != 200 {
		t.Fatalf("archive by an admin who is not a member: %d", got)
	}
	if got := call("POST", project+"/reopen", nil); got != 200 {
		t.Fatalf("reopen: %d", got)
	}
	if got := call("POST", project+"/archive", nil); got != 200 {
		t.Fatalf("archive again: %d", got)
	}
	// An admin who may archive and delete may not edit what is archived: the guard refuses them like anyone who could write there.
	code := func(method, path string, body any) (int, string) {
		resp := f.requestAs(t, cookie, csrf, method, path, body)
		defer resp.Body.Close()
		var problem struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&problem)
		return resp.StatusCode, problem.Error
	}
	for name, request := range map[string]struct {
		method, path string
		body         any
	}{
		"rename":        {"PATCH", project, map[string]string{"name": "renamed"}},
		"move folder":   {"PATCH", project, map[string]string{"folder": t.TempDir()}},
		"add member":    {"PUT", project + "/members/" + admin.ID, map[string]string{"role": "viewer"}},
		"remove member": {"DELETE", project + "/members/" + admin.ID, nil},
		"new room":      {"POST", project + "/rooms", map[string]string{"title": "late"}},
	} {
		if status, problem := code(request.method, request.path, request.body); status != 409 || problem != "project_archived" {
			t.Errorf("%s by an admin on an archived project: %d %q", name, status, problem)
		}
	}
	// Stopping an agent stays possible (the store refuses starting one); it is how a stuck agent is freed.
	if status, problem := code("POST", "/api/agents/"+agent.ID+"/control", map[string]any{"desired": "stopped", "rev": 0}); problem == "project_archived" {
		t.Errorf("stopping an agent was refused by the archived guard: %d %q", status, problem)
	}
	// Starting one is refused for the project's own owner too, with the archive named (the control route is open to stopping, not to this).
	current, err := f.db.AgentCardByID(t.Context(), agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp := f.request("POST", "/api/agents/"+agent.ID+"/control", map[string]any{"desired": "active", "rev": current.DesiredRev}, true)
	var started struct {
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&started)
	resp.Body.Close()
	if resp.StatusCode != 409 || started.Error != "project_archived" {
		t.Errorf("starting an agent in an archived project: %d %q", resp.StatusCode, started.Error)
	}
	// An agent on an approved machine that has not reported stopped holds the project, whatever its desired state.
	if _, err := f.exec(`UPDATE agent SET observed='in_turn'`); err != nil {
		t.Fatal(err)
	}
	if status, problem := code("DELETE", project, map[string]string{"name": f.project.Name}); status != 409 || problem != "project_busy" {
		t.Fatalf("delete with an agent not confirmed stopped: %d %q", status, problem)
	}
	if _, err := f.exec(`UPDATE agent SET observed='stopped'`); err != nil {
		t.Fatal(err)
	}
	current, err = f.db.AgentCardByID(t.Context(), agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AckAgentControl(t.Context(), agent.ID, current.Generation, fmt.Sprint(current.DesiredRev)); err != nil {
		t.Fatal(err)
	}
	if got := call("DELETE", project, map[string]string{"name": f.project.Name}); got != 204 {
		t.Fatalf("delete: %d", got)
	}
}

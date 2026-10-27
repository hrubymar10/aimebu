package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func (f *fixture) patchRoom(t *testing.T, cookie *http.Cookie, csrf, roomID string, body any) (int, map[string]any) {
	t.Helper()
	resp := f.requestAs(t, cookie, csrf, http.MethodPatch, "/api/rooms/"+roomID, body)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f *fixture) projectRooms(t *testing.T) (home, task store.Room) {
	t.Helper()
	rooms, err := f.db.RoomsForUserProject(t.Context(), f.project.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, room := range rooms {
		switch {
		case room.Kind == "home":
			home = room
		case room.Kind == "task" && room.ID == f.room.ID:
			task = room
		}
	}
	if home.ID == "" || task.ID == "" {
		t.Fatalf("rooms of the project: %+v", rooms)
	}
	return home, task
}

func (f *fixture) roomTitle(t *testing.T, id string) string {
	t.Helper()
	room, err := f.db.RoomByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return room.Title
}

func (f *fixture) member(t *testing.T, login, role string) (*http.Cookie, string) {
	t.Helper()
	cookie, csrf := registerSecondUser(t, f, login)
	user, err := f.db.UserByLogin(t.Context(), login)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, f.project.ID, user.ID, role); err != nil {
		t.Fatal(err)
	}
	return cookie, csrf
}

func (f *fixture) renamedEvents(t *testing.T, roomID string) []map[string]string {
	t.Helper()
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.db.RecentSystemEvents(t.Context(), owner.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for _, event := range events {
		if event.Kind == "room_renamed" && event.RoomID == roomID {
			var payload map[string]string
			if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
				t.Fatal(err)
			}
			out = append(out, payload)
		}
	}
	return out
}

func TestRenameATaskRoom(t *testing.T) {
	f := newFixture(t)
	f.seed()
	_, task := f.projectRooms(t)
	status, body := f.patchRoom(t, f.cookie, f.csrf, task.ID, map[string]string{"title": "  Release  prep "})
	if status != 200 || body["title"] != "Release  prep" || f.roomTitle(t, task.ID) != "Release  prep" {
		t.Fatalf("rename: %d %+v", status, body)
	}
	events := f.renamedEvents(t, task.ID)
	if len(events) != 1 || events[0]["title"] != "Release  prep" || events[0]["previous_title"] != task.Title || events[0]["room_id"] != task.ID {
		t.Fatalf("room_renamed events: %+v", events)
	}
	// The same title again changes nothing and says nothing.
	if status, _ := f.patchRoom(t, f.cookie, f.csrf, task.ID, map[string]string{"title": "Release  prep"}); status != 200 || len(f.renamedEvents(t, task.ID)) != 1 {
		t.Fatalf("an unchanged title recorded an event: %d %+v", status, f.renamedEvents(t, task.ID))
	}
	// The project name and the home room are not touched by a task room's rename.
	home, _ := f.projectRooms(t)
	if f.roomTitle(t, home.ID) != home.Title {
		t.Fatal("a task room's rename changed the home room")
	}
}

func TestRenameRefusesBadTitles(t *testing.T) {
	f := newFixture(t)
	f.seed()
	_, task := f.projectRooms(t)
	for _, title := range []string{"", "   ", "two\nlines", "tab\there", "carriage\rreturn", strings.Repeat("x", store.MaxRoomTitleRunes+1), "bad\x00nul", "line\u2028separator", "paragraph\u2029separator"} {
		status, body := f.patchRoom(t, f.cookie, f.csrf, task.ID, map[string]string{"title": title})
		if status != 400 || body["error"] != "invalid_title" {
			t.Errorf("title %q: %d %+v, want 400 invalid_title", title, status, body)
		}
	}
	// The longest title and non-Latin text are fine, counted in characters rather than bytes.
	for _, title := range []string{strings.Repeat("x", store.MaxRoomTitleRunes), strings.Repeat("é", store.MaxRoomTitleRunes), "Přehled vydání"} {
		if status, body := f.patchRoom(t, f.cookie, f.csrf, task.ID, map[string]string{"title": title}); status != 200 {
			t.Errorf("title %q: %d %+v, want 200", title, status, body)
		}
	}
	if status, _ := f.patchRoom(t, f.cookie, f.csrf, task.ID, map[string]any{"name": "wrong field"}); status != 400 {
		t.Errorf("a body without a title: %d", status)
	}
	if status, _ := f.patchRoom(t, f.cookie, f.csrf, "room_does_not_exist", map[string]string{"title": "x"}); status != 404 && status != 403 {
		t.Errorf("an unknown room: %d", status)
	}
}

func TestRenameNeedsOwnerOrOperatorForATaskRoom(t *testing.T) {
	f := newFixture(t)
	f.seed()
	_, task := f.projectRooms(t)
	viewer, viewerCSRF := f.member(t, "viewer", "viewer")
	operator, operatorCSRF := f.member(t, "operator", "operator")
	stranger, strangerCSRF := registerSecondUser(t, f, "stranger")
	if status, body := f.patchRoom(t, viewer, viewerCSRF, task.ID, map[string]string{"title": "by viewer"}); status != 403 {
		t.Errorf("a viewer: %d %+v, want 403", status, body)
	}
	if status, body := f.patchRoom(t, stranger, strangerCSRF, task.ID, map[string]string{"title": "by stranger"}); status != 403 && status != 404 {
		t.Errorf("a person outside the project: %d %+v", status, body)
	}
	if got := f.roomTitle(t, task.ID); got != task.Title {
		t.Fatalf("a refused rename changed the title to %q", got)
	}
	if status, body := f.patchRoom(t, operator, operatorCSRF, task.ID, map[string]string{"title": "by operator"}); status != 200 || body["title"] != "by operator" {
		t.Errorf("an operator: %d %+v", status, body)
	}
}

// The home room is named after its project: renaming it renames the project (the path of the project settings), for the project's owner, and
// the title and the name stay one.
func TestRenameTheHomeRoomRenamesTheProject(t *testing.T) {
	f := newFixture(t)
	f.seed()
	home, _ := f.projectRooms(t)
	operator, operatorCSRF := f.member(t, "operator", "operator")
	if status, body := f.patchRoom(t, operator, operatorCSRF, home.ID, map[string]string{"title": "by operator"}); status != 403 {
		t.Fatalf("an operator renaming the project: %d %+v, want 403", status, body)
	}
	status, body := f.patchRoom(t, f.cookie, f.csrf, home.ID, map[string]string{"title": " Mission Control "})
	if status != 200 || body["title"] != "Mission Control" {
		t.Fatalf("rename the home room: %d %+v", status, body)
	}
	project, err := f.db.ProjectByID(t.Context(), f.project.ID)
	if err != nil || project.Name != "Mission Control" || f.roomTitle(t, home.ID) != "Mission Control" {
		t.Fatalf("the project is %q and its home room %q, want both Mission Control (%v)", project.Name, f.roomTitle(t, home.ID), err)
	}
	// Both ways: renaming the project from the home room's project settings names the home room too, as it always did.
	resp := f.requestAs(t, f.cookie, f.csrf, http.MethodPatch, "/api/projects/"+f.project.ID, map[string]string{"name": "Apollo"})
	resp.Body.Close()
	if resp.StatusCode != 200 || f.roomTitle(t, home.ID) != "Apollo" {
		t.Fatalf("project rename: %d, home room %q", resp.StatusCode, f.roomTitle(t, home.ID))
	}
	events := f.renamedEvents(t, home.ID)
	if len(events) != 1 || events[0]["title"] != "Mission Control" {
		t.Fatalf("room_renamed events of the home room: %+v", events)
	}
	// A home room whose title had drifted from the project name (an import can leave one) is brought back in step by renaming it.
	if _, err := f.exec(`UPDATE room SET title='drifted' WHERE id=?`, home.ID); err != nil {
		t.Fatal(err)
	}
	if status, body := f.patchRoom(t, f.cookie, f.csrf, home.ID, map[string]string{"title": "Gemini"}); status != 200 {
		t.Fatalf("rename a drifted home room: %d %+v", status, body)
	}
	project, _ = f.db.ProjectByID(t.Context(), f.project.ID)
	if project.Name != "Gemini" || f.roomTitle(t, home.ID) != "Gemini" {
		t.Fatalf("after the drift: project %q, home room %q", project.Name, f.roomTitle(t, home.ID))
	}
	// Giving a drifted home room the project's own name heals it (it is not "unchanged": the title differs), and giving it its own drifted
	// title renames the project (the title is not the name, so that is not unchanged either).
	if _, err := f.exec(`UPDATE room SET title='drifted' WHERE id=?`, home.ID); err != nil {
		t.Fatal(err)
	}
	before := len(f.renamedEvents(t, home.ID))
	if status, body := f.patchRoom(t, f.cookie, f.csrf, home.ID, map[string]string{"title": "Gemini"}); status != 200 || f.roomTitle(t, home.ID) != "Gemini" || len(f.renamedEvents(t, home.ID)) != before+1 {
		t.Fatalf("healing a drifted home room: %d %+v, title %q, events %d -> %d", status, body, f.roomTitle(t, home.ID), before, len(f.renamedEvents(t, home.ID)))
	}
	if _, err := f.exec(`UPDATE room SET title='drifted' WHERE id=?`, home.ID); err != nil {
		t.Fatal(err)
	}
	if status, body := f.patchRoom(t, f.cookie, f.csrf, home.ID, map[string]string{"title": "drifted"}); status != 200 {
		t.Fatalf("renaming to the drifted title: %d %+v", status, body)
	}
	project, _ = f.db.ProjectByID(t.Context(), f.project.ID)
	if project.Name != "drifted" {
		t.Fatalf("the project is %q, want it renamed to the drifted title", project.Name)
	}
	// Both agree now: the same title again is a no-op and records nothing.
	before = len(f.renamedEvents(t, home.ID))
	if status, _ := f.patchRoom(t, f.cookie, f.csrf, home.ID, map[string]string{"title": "drifted"}); status != 200 || len(f.renamedEvents(t, home.ID)) != before {
		t.Fatalf("an unchanged home title recorded an event: %d", status)
	}
}

func TestPrivateChatsCannotBeRenamed(t *testing.T) {
	f := newFixture(t)
	f.seed()
	alice, aliceCSRF := f.member(t, "alice", "operator")
	f.member(t, "bob", "operator")
	bobUser, err := f.db.UserByLogin(t.Context(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	resp := f.requestAs(t, alice, aliceCSRF, http.MethodPost, "/api/projects/"+f.project.ID+"/dms", map[string]string{"user_id": bobUser.ID})
	var dm store.Room
	if err := json.NewDecoder(resp.Body).Decode(&dm); err != nil || resp.StatusCode != 200 || dm.Kind != "dm" {
		t.Fatalf("open a private chat: %d %+v %v", resp.StatusCode, dm, err)
	}
	resp.Body.Close()
	before := f.roomTitle(t, dm.ID)
	status, body := f.patchRoom(t, alice, aliceCSRF, dm.ID, map[string]string{"title": "secret plans"})
	if status != 409 || body["error"] != "invalid_kind" {
		t.Fatalf("rename a private chat: %d %+v, want 409 invalid_kind", status, body)
	}
	if got := f.roomTitle(t, dm.ID); got != before {
		t.Fatalf("a refused rename changed the chat's title to %q", got)
	}
}

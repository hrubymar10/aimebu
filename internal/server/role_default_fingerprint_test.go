package server

import (
	"net/http"
	"testing"
)

func TestRoleDefaultActionsAndPermissions(t *testing.T) {
	f := newFixture(t)
	f.seed()
	role := f.listedRoles(t)["worker"]
	role.Body = "personal instructions"
	edited, _ := f.roleJSON(t, http.MethodPut, "/api/roles/"+role.ID, role, 200)
	if edited.Rev != role.Rev+1 {
		t.Fatal("edit revision did not advance")
	}
	path := "/api/roles/" + role.ID
	if _, e := f.roleJSON(t, http.MethodPost, path+"/keep-default", map[string]any{"rev": edited.Rev + 1}, 409); e["error"] != "stale_rev" {
		t.Fatalf("stale keep: %v", e)
	}
	f.roleJSON(t, http.MethodPost, path+"/keep-default", map[string]any{"rev": edited.Rev}, 200)
	f.roleJSON(t, http.MethodPost, path+"/use-default", map[string]any{"rev": edited.Rev + 1}, 200)
	if got := f.listedRoles(t)["worker"]; got.Body != role.DefaultBody || got.DefaultBodyChanged {
		t.Fatal("default was not restored")
	}

	w := newAdminWorld(t)
	restricted := w.f.listedRoles(t)["worker"]
	restricted.Body = "admin review text"
	restricted, _ = w.f.roleJSON(t, http.MethodPut, "/api/roles/"+restricted.ID, restricted, 200)
	for _, route := range []string{"/keep-default", "/use-default"} {
		for _, who := range []adminActor{w.viewer, w.pOwner} {
			if got := w.call(t, who, "POST", "/api/roles/"+restricted.ID+route, map[string]any{"rev": restricted.Rev}); got != 403 {
				t.Errorf("%s as %s: %d", route, who.name, got)
			}
		}
	}
	if got := w.call(t, w.admin, "POST", "/api/roles/"+restricted.ID+"/keep-default", map[string]any{"rev": restricted.Rev}); got != 200 {
		t.Errorf("admin keep: %d", got)
	}
	if got := w.call(t, w.admin, "POST", "/api/roles/"+restricted.ID+"/use-default", map[string]any{"rev": restricted.Rev + 1}); got != 200 {
		t.Errorf("admin use: %d", got)
	}
}

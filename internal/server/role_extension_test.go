package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// The role prompt extension API: the global extension is the instance owner's field on a role write, a project's extension belongs
// to its owner or operators, and the project view composes what an agent is delivered.
func TestRoleBodyExtensionAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	roles := f.listedRoles(t)
	worker := roles["worker"]

	// The owner saves the global extension with a role write; the list carries it and the composed preview.
	extension := "Instance rules for every worker."
	worker.BodyExtension = &extension
	saved, errs := f.roleJSON(t, http.MethodPut, "/api/roles/"+worker.ID, worker, 200)
	if errs["error"] != "" || saved.BodyExtension == nil || *saved.BodyExtension != extension {
		t.Fatalf("the owner's write: %+v %v", saved, errs)
	}
	listed := f.listedRoles(t)["worker"]
	if listed.BodyExtension == nil || *listed.BodyExtension != extension {
		t.Fatalf("the list misses the extension: %+v", listed.BodyExtension)
	}
	if !strings.Contains(listed.EffectiveBody, extension) || !strings.HasSuffix(listed.EffectiveBody, extension) {
		t.Fatalf("effective body=%q", listed.EffectiveBody)
	}

	// An oversized extension is refused with its own error.
	tooBig := strings.Repeat("x", store.MaxRoleBodyExtensionLen+1)
	big := listed
	big.BodyExtension = &tooBig
	if _, e := f.roleJSON(t, http.MethodPut, "/api/roles/"+worker.ID, big, 400); e["error"] != "invalid_extension" {
		t.Fatalf("oversized extension error = %v", e)
	}

	// A project's extension: an operator saves it, a viewer is refused, and the project view composes the delivered text.
	cookie, csrf := registerSecondUser(t, f, "operator")
	user, err := f.db.UserByLogin(t.Context(), "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, f.project.ID, user.ID, "operator")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	put := func(path string, body any) *http.Response {
		return userRequest(t, f, cookie, csrf, http.MethodPut, path, body)
	}
	resp := put("/api/projects/"+f.project.ID+"/role-prompts/worker", map[string]any{"body": "Project rules for workers.", "rev": 0})
	if resp.StatusCode != 200 {
		t.Fatalf("the operator's put: %d", resp.StatusCode)
	}
	var savedExt store.RoleBodyExtension
	json.NewDecoder(resp.Body).Decode(&savedExt)
	resp.Body.Close()
	if savedExt.Body != "Project rules for workers." || savedExt.Rev != 1 {
		t.Fatalf("saved extension: %+v", savedExt)
	}
	// A stale revision is refused; the current one changes the text.
	resp = put("/api/projects/"+f.project.ID+"/role-prompts/worker", map[string]any{"body": "second", "rev": 0})
	if resp.StatusCode != 409 {
		t.Fatalf("a stale revision: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = put("/api/projects/"+f.project.ID+"/role-prompts/worker", map[string]any{"body": "second", "rev": 1})
	if resp.StatusCode != 200 {
		t.Fatalf("the current revision: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// An unknown role is not found; an oversized one is refused.
	if code := func() int {
		resp := put("/api/projects/"+f.project.ID+"/role-prompts/nobody", map[string]any{"body": "x"})
		defer resp.Body.Close()
		return resp.StatusCode
	}(); code != 404 {
		t.Fatalf("an unknown role: %d", code)
	}
	if code := func() int {
		resp := put("/api/projects/"+f.project.ID+"/role-prompts/worker", map[string]any{"body": strings.Repeat("x", store.MaxRoleBodyExtensionLen+1), "rev": 2})
		defer resp.Body.Close()
		return resp.StatusCode
	}(); code != 400 {
		t.Fatalf("an oversized project extension: %d", code)
	}

	// The project view shows both extensions and the delivered text.
	get := func(who adminActor, path string) *http.Response {
		return userRequest(t, f, who.cookie, who.csrf, http.MethodGet, path, nil)
	}
	oper := adminActor{cookie: cookie, csrf: csrf}
	resp = get(oper, "/api/projects/"+f.project.ID+"/role-prompts")
	if resp.StatusCode != 200 {
		t.Fatalf("the operator's read: %d", resp.StatusCode)
	}
	var rows []store.RolePromptSettings
	json.NewDecoder(resp.Body).Decode(&rows)
	resp.Body.Close()
	var workerRow *store.RolePromptSettings
	for i := range rows {
		if rows[i].RoleKey == "worker" {
			workerRow = &rows[i]
		}
	}
	if workerRow == nil || workerRow.GlobalBodyExtension != extension || workerRow.Project == nil || workerRow.Project.Body != "second" {
		t.Fatalf("the project view: %+v", workerRow)
	}
	if !strings.Contains(workerRow.DeliveredBody, extension) || !strings.Contains(workerRow.DeliveredBody, "second") {
		t.Fatalf("delivered body=%q", workerRow.DeliveredBody)
	}

	// A viewer reads and writes nothing.
	vcookie, vcsrf := registerSecondUser(t, f, "projectviewer")
	vuser, err := f.db.UserByLogin(t.Context(), "projectviewer")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, f.project.ID, vuser.ID, "viewer")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	viewer := adminActor{cookie: vcookie, csrf: vcsrf}
	if code := func() int {
		resp := get(viewer, "/api/projects/"+f.project.ID+"/role-prompts")
		defer resp.Body.Close()
		return resp.StatusCode
	}(); code != 403 {
		t.Fatalf("a viewer reads the project view: %d", code)
	}
	if code := func() int {
		resp := userRequest(t, f, vcookie, vcsrf, http.MethodPut, "/api/projects/"+f.project.ID+"/role-prompts/worker", map[string]any{"body": "no"})
		defer resp.Body.Close()
		return resp.StatusCode
	}(); code != 403 {
		t.Fatalf("a viewer saves an extension: %d", code)
	}
}

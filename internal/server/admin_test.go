package server

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	json "github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/quota"
	"github.com/hrubymar10/aimebu/internal/store"
)

type adminActor struct {
	name   string
	id     string
	cookie *http.Cookie
	csrf   string
}

// adminWorld is the cast of the permission matrix: the owner (the fixture's account), an admin, a project owner, an operator, a
// viewer and someone with no project at all. The project is the fixture's; `other` is a second project only the owner belongs to.
type adminWorld struct {
	f      *fixture
	owner  adminActor
	admin  adminActor
	peer   adminActor // a second admin
	pOwner adminActor
	oper   adminActor
	viewer adminActor
	out    adminActor
	other  string
}

func (w *adminWorld) member(t *testing.T, name, role string, project string) adminActor {
	t.Helper()
	cookie, csrf := registerSecondUser(t, w.f, name)
	user, err := w.f.db.UserByLogin(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if role != "" {
		if err := w.f.db.Tx(t.Context(), func(tx *store.Tx) error {
			_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, project, user.ID, role)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return adminActor{name: name, id: user.ID, cookie: cookie, csrf: csrf}
}

func newAdminWorld(t *testing.T) *adminWorld {
	t.Helper()
	f := newFixture(t)
	f.seed()
	w := &adminWorld{f: f}
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	w.owner = adminActor{name: "owner", id: owner.ID, cookie: f.cookie, csrf: f.csrf}
	w.admin = w.member(t, "adminone", "viewer", f.project.ID)
	w.peer = w.member(t, "admintwo", "viewer", f.project.ID)
	w.pOwner = w.member(t, "powner", "owner", f.project.ID)
	w.oper = w.member(t, "operator", "operator", f.project.ID)
	w.viewer = w.member(t, "viewer", "viewer", f.project.ID)
	w.out = w.member(t, "outsider", "", "")
	other, err := f.db.CreateProject(t.Context(), store.Project{Name: "other", Folder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, other.ID, owner.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w.other = other.ID
	for _, who := range []adminActor{w.admin, w.peer} {
		if code := w.call(t, w.owner, "PUT", "/api/users/"+who.id+"/admin", map[string]bool{"admin": true}); code != 204 {
			t.Fatalf("appointing %s: %d", who.name, code)
		}
	}
	return w
}

func (w *adminWorld) call(t *testing.T, who adminActor, method, path string, body any) int {
	t.Helper()
	resp := userRequest(t, w.f, who.cookie, who.csrf, method, path, body)
	resp.Body.Close()
	return resp.StatusCode
}

func (w *adminWorld) json(t *testing.T, who adminActor, method, path string, body any) (int, map[string]any) {
	t.Helper()
	resp := userRequest(t, w.f, who.cookie, who.csrf, method, path, body)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The global admin has the owner's reach over projects, people and global settings. Each row says who may call the endpoint; everyone
// else must get 403, and whoever may must not (the call can still fail for another reason, such as a bad body).
func TestGlobalAdminPermissionMatrix(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	everyone := []adminActor{w.owner, w.admin, w.pOwner, w.oper, w.viewer, w.out}
	ownerAdmin := map[string]bool{"owner": true, "adminone": true, "admintwo": true}
	ownerOnly := map[string]bool{"owner": true}
	for _, row := range []struct {
		name         string
		method, path string
		body         any
		allowed      map[string]bool
	}{
		{"create a project", "POST", "/api/projects", map[string]string{"name": "made", "folder": t.TempDir()}, ownerAdmin},
		{"macros", "PUT", "/api/macros", map[string]string{}, ownerAdmin},
		{"message expiry", "PUT", "/api/retention", map[string]int{"messages_considered_expired_after_seconds": 21600}, ownerAdmin},
		{"edit a harness's models", "PUT", "/api/machines/" + w.f.machineID + "/launchers/mln_none/models", map[string]any{"models": []any{}}, ownerAdmin},
		{"list machines", "GET", "/api/machines", nil, ownerAdmin},
		{"machine launchers", "GET", "/api/machines/mch_none/launchers", nil, ownerAdmin},
		{"machine harnesses", "GET", "/api/machines/mch_none/harnesses", nil, ownerAdmin},
		{"detect harnesses", "POST", "/api/machines/" + w.f.machineID + "/launchers/detect", nil, ownerAdmin},
		{"detect a harness's models", "POST", "/api/machines/" + w.f.machineID + "/launchers/mln_none/models/detect", nil, ownerAdmin},
		{"delete a sound", "DELETE", "/api/sounds/snd_none", nil, ownerAdmin},
		{"disk report", "GET", "/api/disk", nil, ownerAdmin},
		{"cleanup plan", "POST", "/api/cleanup", map[string]bool{"dry_run": true}, ownerAdmin},
		{"cleanup settings", "GET", "/api/cleanup/settings", nil, ownerAdmin},
		{"cleanup log", "GET", "/api/cleanup/log", nil, ownerAdmin},
		{"settings export", "GET", "/api/settings-backup", nil, ownerAdmin},
		{"list every account", "GET", "/api/users", nil, ownerAdmin},
		{"a global role", "POST", "/api/roles", map[string]any{"project_id": "", "role_key": "adminrole", "label": "Admin role", "emoji": "", "body": "x", "cardinality": "multi"}, ownerAdmin},
		{"global memory", "POST", "/api/global-memory/user_profile", map[string]string{"scope_key": "adminone", "body": "likes tests"}, ownerAdmin},
		{"invite into any project as owner", "POST", "/api/invites", map[string]string{"kind": "join", "project_id": w.other, "role": "owner"}, ownerAdmin},
		{"invite into the project as operator", "POST", "/api/invites", map[string]string{"kind": "join", "project_id": w.f.project.ID, "role": "operator"}, map[string]bool{"owner": true, "adminone": true, "admintwo": true, "powner": true}},
		// What stays with the owner: appointing admins, pairing and approving machines and the host key, provider logins, the quota rules, the v1 import and
		// replacing the instance's settings.
		{"appoint an admin", "PUT", "/api/users/" + w.viewer.id + "/admin", map[string]bool{"admin": true}, ownerOnly},
		{"pair a machine", "POST", "/api/machine-pairings", map[string]string{"name": "m"}, ownerOnly},
		{"approve a machine", "POST", "/api/machines/mch_none/approve", map[string]any{}, ownerOnly},
		{"credential targets", "GET", "/api/credentials/targets", nil, ownerOnly},
		{"provider usages", "GET", "/api/usages", nil, ownerOnly},
		{"v1 import status", "GET", "/api/import-v1/status", nil, ownerOnly},
		{"settings preview", "POST", "/api/settings-backup/preview", map[string]any{"backup": map[string]any{}}, ownerOnly},
		{"settings apply", "POST", "/api/settings-backup/apply", map[string]any{"backup": map[string]any{}, "digest": "x"}, ownerOnly},
		{"quota switcher", "GET", "/api/quota-switcher", nil, ownerOnly},
		{"limit guard", "GET", "/api/limit-guard", nil, ownerOnly},
	} {
		for _, who := range everyone {
			who := who
			got := w.call(t, who, row.method, row.path, row.body)
			allowed := row.allowed[who.name]
			if allowed && (got == 403 || got == 401) || !allowed && got != 403 {
				t.Errorf("%s as %s: %d, allowed=%t", row.name, who.name, got, allowed)
			}
		}
	}
}

// An admin acts on ordinary accounts only: never on the owner, never on a peer admin. The owner may act on an admin.
func TestAdminActsOnOrdinaryAccountsOnly(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	project := w.f.project.ID
	reset := func(who, target adminActor) int {
		return w.call(t, who, "POST", "/api/invites", map[string]string{"kind": "reset", "target_user_id": target.id})
	}
	if code := reset(w.admin, w.viewer); code != 201 {
		t.Errorf("an admin resets an ordinary account: %d", code)
	}
	for name, target := range map[string]adminActor{"the owner": w.owner, "a peer admin": w.peer} {
		if code := reset(w.admin, target); code != 403 {
			t.Errorf("an admin resets %s: %d, want 403", name, code)
		}
	}
	if code := reset(w.owner, w.peer); code != 201 {
		t.Errorf("the owner resets an admin: %d", code)
	}
	// Memberships: an admin may change an ordinary person's role, never the owner's or a peer admin's, and may change their own.
	member := func(who, target adminActor, role string) int {
		return w.call(t, who, "PUT", "/api/projects/"+project+"/members/"+target.id, map[string]string{"role": role})
	}
	if code := member(w.admin, w.viewer, "operator"); code != 204 {
		t.Errorf("an admin changes an ordinary member's role: %d", code)
	}
	if code := member(w.admin, w.owner, "viewer"); code != 403 {
		t.Errorf("an admin demotes the owner in a project: %d, want 403", code)
	}
	if code := member(w.admin, w.peer, "operator"); code != 403 {
		t.Errorf("an admin changes a peer admin's role: %d, want 403", code)
	}
	if code := member(w.admin, w.admin, "operator"); code != 204 {
		t.Errorf("an admin changes their own membership: %d", code)
	}
	if code := member(w.owner, w.peer, "operator"); code != 204 {
		t.Errorf("the owner changes an admin's role: %d", code)
	}
	// Deactivation.
	deactivate := func(who, target adminActor) int {
		return w.call(t, who, "POST", "/api/users/"+target.id+"/deactivate", nil)
	}
	for name, target := range map[string]adminActor{"the owner": w.owner, "a peer admin": w.peer} {
		if code := deactivate(w.admin, target); code != 403 {
			t.Errorf("an admin deactivates %s: %d, want 403", name, code)
		}
	}
	if code := deactivate(w.admin, w.owner); code != 403 || w.call(t, w.owner, "GET", "/api/users", nil) != 200 {
		t.Errorf("the owner must survive an admin's attempt")
	}
	if code := deactivate(w.oper, w.viewer); code != 403 {
		t.Errorf("an operator deactivates someone: %d, want 403", code)
	}
	if code := deactivate(w.admin, w.out); code != 204 {
		t.Errorf("an admin deactivates an ordinary account: %d", code)
	}
	if code := deactivate(w.owner, w.peer); code != 204 {
		t.Errorf("the owner deactivates an admin: %d", code)
	}
	// A deactivated admin is out at once, flag or no flag.
	if code := w.call(t, w.peer, "GET", "/api/users", nil); code != 401 {
		t.Errorf("a deactivated admin's session still works: %d", code)
	}
	if code := deactivate(w.owner, w.owner); code != 403 {
		t.Errorf("the owner deactivates themself: %d, want 403", code)
	}
}

// The flag: who may set it, on whom, what the account list says, and that taking it away (or deactivating the account) ends the reach at once.
func TestAdminFlagLifecycle(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	adminCall := func(who, target adminActor, on bool) int {
		return w.call(t, who, "PUT", "/api/users/"+target.id+"/admin", map[string]bool{"admin": on})
	}
	if code := adminCall(w.admin, w.viewer, true); code != 403 {
		t.Errorf("an admin appoints an admin: %d, want 403", code)
	}
	if code := adminCall(w.pOwner, w.viewer, true); code != 403 {
		t.Errorf("a project owner appoints an admin: %d, want 403", code)
	}
	if code := adminCall(w.owner, w.owner, true); code != 400 {
		t.Errorf("flagging the owner: %d, want 400 (the owner is already everything)", code)
	}
	if code := w.call(t, w.owner, "PUT", "/api/users/usr_nobody/admin", map[string]bool{"admin": true}); code != 404 {
		t.Errorf("an unknown account: %d, want 404", code)
	}
	if code := w.call(t, w.owner, "PUT", "/api/users/"+w.viewer.id+"/admin", map[string]any{}); code != 400 {
		t.Errorf("a body without admin: %d, want 400", code)
	}
	// What the list and the person's own view say.
	code, out := w.json(t, w.owner, "GET", "/api/users", nil)
	if code != 200 || out["global_owner"] != true || out["global_admin"] != true {
		t.Fatalf("the owner's view: %d %v", code, out)
	}
	flags := map[string][2]bool{}
	for _, item := range out["users"].([]any) {
		row := item.(map[string]any)
		flags[row["login"].(string)] = [2]bool{row["owner"] == true, row["admin"] == true}
	}
	if flags["owner"] != [2]bool{true, false} || flags["adminone"] != [2]bool{false, true} || flags["viewer"] != [2]bool{false, false} {
		t.Fatalf("flags in the list: %v", flags)
	}
	if code, out := w.json(t, w.admin, "GET", "/api/users", nil); code != 200 || out["global_owner"] != false || out["global_admin"] != true {
		t.Fatalf("an admin's view: %d %v", code, out)
	}
	if code, _ := w.json(t, w.viewer, "GET", "/api/users?project_id="+w.f.project.ID, nil); code != 403 {
		t.Fatalf("a viewer lists people: %d", code)
	}
	// An admin has the reach while the flag stands and loses it the moment it goes.
	if code := w.call(t, w.admin, "PUT", "/api/macros", map[string]string{}); code != 200 && code != 204 {
		t.Fatalf("an admin saves macros: %d", code)
	}
	if code := adminCall(w.owner, w.admin, false); code != 204 {
		t.Fatalf("taking the flag away: %d", code)
	}
	if code := w.call(t, w.admin, "PUT", "/api/macros", map[string]string{}); code != 403 {
		t.Errorf("an account whose flag was removed still saves macros: %d, want 403", code)
	}
	if code, out := w.json(t, w.admin, "GET", "/api/users", nil); code != 403 {
		t.Errorf("the account list after the flag went: %d %v", code, out)
	}
	// A deactivated account cannot be made an admin (and a flag can still be taken away from one).
	if code := w.call(t, w.owner, "POST", "/api/users/"+w.viewer.id+"/deactivate", nil); code != 204 {
		t.Fatalf("deactivating: %d", code)
	}
	if code := adminCall(w.owner, w.viewer, true); code != 400 {
		t.Errorf("appointing a deactivated account: %d, want 400", code)
	}
	if code := adminCall(w.owner, w.viewer, false); code != 204 {
		t.Errorf("clearing the flag of a deactivated account: %d", code)
	}
	// Changing the flag is an event only the owner sees, and a no-change is not an event.
	events := func() int {
		_, out := w.json(t, w.owner, "GET", "/api/system-events", nil)
		n := 0
		list, _ := out["events"].([]any)
		for _, item := range list {
			if item.(map[string]any)["kind"] == "user_admin_changed" {
				n++
			}
		}
		return n
	}
	before := events()
	if code := adminCall(w.owner, w.peer, true); code != 204 || events() != before {
		t.Errorf("flagging an account that is already an admin must change nothing: %d, events %d -> %d", code, before, events())
	}
	if code := adminCall(w.owner, w.peer, false); code != 204 || events() != before+1 {
		t.Errorf("an admin change is one event: %d, events %d -> %d", code, before, events())
	}
}

func TestEveryOwnerOnlyGuardStillRefusesAnAdminWithTheRightMessage(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	code, body := w.json(t, w.admin, "POST", "/api/machine-pairings", map[string]string{"name": "m"})
	if code != 403 || fmt.Sprint(body["message"]) != "owner required" {
		t.Fatalf("an admin pairing a machine: %d %v", code, body)
	}
	code, body = w.json(t, w.viewer, "PUT", "/api/macros", map[string]string{})
	if code != 403 || fmt.Sprint(body["message"]) != "admin required" {
		t.Fatalf("a viewer saving macros: %d %v", code, body)
	}
}

// A reset link is authority borrowed from whoever issued it, so it is rechecked when it is used: losing the flag, being deactivated, or
// the target being promoted in the meantime must not leave a way to take the account over.
func TestResetLinksAreRecheckedWhenUsed(t *testing.T) {
	t.Parallel()
	redeem := func(w *adminWorld, token string) int {
		resp := userRequest(t, w.f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": token, "password": "a brand new password"})
		resp.Body.Close()
		return resp.StatusCode
	}
	issue := func(w *adminWorld, who adminActor, body map[string]string) string {
		code, out := w.json(t, who, "POST", "/api/invites", body)
		if code != 201 {
			t.Fatalf("issuing %v as %s: %d %v", body, who.name, code, out)
		}
		return out["token"].(string)
	}
	logsIn := func(w *adminWorld, login, password string) bool {
		resp := userRequest(t, w.f, nil, "", "POST", "/api/login", map[string]string{"login": login, "password": password})
		resp.Body.Close()
		return resp.StatusCode == 200
	}
	t.Run("control: an unchanged admin's link works once", func(t *testing.T) {
		w := newAdminWorld(t)
		token := issue(w, w.admin, map[string]string{"kind": "reset", "target_user_id": w.viewer.id})
		if code := redeem(w, token); code != 201 {
			t.Fatalf("redeem: %d", code)
		}
		if !logsIn(w, "viewer", "a brand new password") || logsIn(w, "viewer", "viewer password") {
			t.Fatal("the reset did not change the password")
		}
		if code := redeem(w, token); code != 401 {
			t.Fatalf("a used link: %d", code)
		}
	})
	t.Run("the issuer loses the flag", func(t *testing.T) {
		w := newAdminWorld(t)
		token := issue(w, w.admin, map[string]string{"kind": "reset", "target_user_id": w.viewer.id})
		if code := w.call(t, w.owner, "PUT", "/api/users/"+w.admin.id+"/admin", map[string]bool{"admin": false}); code != 204 {
			t.Fatal(code)
		}
		if code := redeem(w, token); code != 401 {
			t.Fatalf("a link whose issuer lost the flag: %d, want 401", code)
		}
		if !logsIn(w, "viewer", "viewer password") {
			t.Fatal("the account's password changed anyway")
		}
	})
	t.Run("the issuer is deactivated", func(t *testing.T) {
		w := newAdminWorld(t)
		token := issue(w, w.admin, map[string]string{"kind": "reset", "target_user_id": w.viewer.id})
		if code := w.call(t, w.owner, "POST", "/api/users/"+w.admin.id+"/deactivate", nil); code != 204 {
			t.Fatal(code)
		}
		if code := redeem(w, token); code != 401 || !logsIn(w, "viewer", "viewer password") {
			t.Fatalf("a link whose issuer was deactivated: %d", code)
		}
	})
	t.Run("the target is promoted to admin meanwhile", func(t *testing.T) {
		w := newAdminWorld(t)
		token := issue(w, w.admin, map[string]string{"kind": "reset", "target_user_id": w.viewer.id})
		if code := w.call(t, w.owner, "PUT", "/api/users/"+w.viewer.id+"/admin", map[string]bool{"admin": true}); code != 204 {
			t.Fatal(code)
		}
		if code := redeem(w, token); code != 401 || !logsIn(w, "viewer", "viewer password") {
			t.Fatalf("a peer admin taken over through an old link: %d", code)
		}
	})
	t.Run("an owner's link survives the admin changes", func(t *testing.T) {
		w := newAdminWorld(t)
		token := issue(w, w.owner, map[string]string{"kind": "reset", "target_user_id": w.peer.id})
		if code := w.call(t, w.owner, "PUT", "/api/users/"+w.admin.id+"/admin", map[string]bool{"admin": false}); code != 204 {
			t.Fatal(code)
		}
		if code := redeem(w, token); code != 201 {
			t.Fatalf("the owner's link for an admin: %d, want 201", code)
		}
	})
	// (That the links are spent, not merely refused, is checked on the invite rows in the store tests; a deactivated account cannot be
	// made an admin again, so here its links just stay dead.)
	t.Run("losing the flag or the account spends the issuer's outstanding links", func(t *testing.T) {
		for name, end := range map[string]func(w *adminWorld){
			"the flag goes": func(w *adminWorld) {
				w.call(t, w.owner, "PUT", "/api/users/"+w.admin.id+"/admin", map[string]bool{"admin": false})
			},
			"deactivated": func(w *adminWorld) { w.call(t, w.owner, "POST", "/api/users/"+w.admin.id+"/deactivate", nil) },
		} {
			w := newAdminWorld(t)
			reset := issue(w, w.admin, map[string]string{"kind": "reset", "target_user_id": w.viewer.id})
			join := issue(w, w.admin, map[string]string{"kind": "join", "project_id": w.other, "role": "owner"})
			end(w)
			// Make the issuer an admin again: the links were spent, not merely unauthorised for the moment.
			if name == "the flag goes" {
				if code := w.call(t, w.owner, "PUT", "/api/users/"+w.admin.id+"/admin", map[string]bool{"admin": true}); code != 204 {
					t.Fatal(code)
				}
			}
			if code := redeem(w, reset); code != 401 {
				t.Errorf("%s: an old reset link works again after re-appointing: %d", name, code)
			}
			resp := userRequest(t, w.f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": join, "login": "newcomer", "password": "a secure password"})
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Errorf("%s: an old join link works again after re-appointing: %d", name, resp.StatusCode)
			}
		}
	})
}

// The export an admin downloads leaves out what an admin cannot apply: the quota switcher and the limit guard.
func TestAnAdminsSettingsExportLeavesOutTheOwnersLoginRules(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	// Make both sections exist: the quota switcher runs over a fake source and the guard has saved settings.
	guard := quota.NewGuard(w.f.srv.Store, &quotaFakeSource{}, w.f.srv.Store, nil)
	w.f.srv.guard.Store(guard)
	if _, err := guard.Save(t.Context(), quota.ToolClaude, quota.GuardConfig{Enabled: true, Soft: 70, Hard: 85, Profiles: map[string]quota.GuardLimit{}}); err != nil {
		t.Fatal(err)
	}
	read := func(who adminActor) map[string]any {
		code, out := w.json(t, who, "GET", "/api/settings-backup", nil)
		if code != 200 {
			t.Fatalf("export as %s: %d", who.name, code)
		}
		return out
	}
	if _, has := read(w.owner)["limit_guard"]; !has {
		t.Fatal("the owner's export must carry the limit guard")
	}
	adminExport := read(w.admin)
	for _, section := range []string{"limit_guard", "quota_switcher"} {
		if _, has := adminExport[section]; has {
			t.Errorf("an admin's export carries %s", section)
		}
	}
	if _, has := adminExport["schema"]; !has {
		t.Fatalf("the admin's export is not a backup: %v", adminExport)
	}
}

// The owner and an admin manage people in every project, including one they are not members of, so the account list names them all; a
// project owner's view stays a view of that project.
func TestUsersListOffersEveryProjectToTheOwnerAndAdmins(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	names := func(who adminActor, path string) (int, []string) {
		code, out := w.json(t, who, "GET", path, nil)
		var list []string
		projects, _ := out["projects"].([]any)
		for _, p := range projects {
			list = append(list, p.(map[string]any)["id"].(string))
		}
		return code, list
	}
	for _, who := range []adminActor{w.owner, w.admin} {
		if code, ids := names(who, "/api/users"); code != 200 || len(ids) != 2 {
			t.Fatalf("%s sees projects %v (status %d), want both", who.name, ids, code)
		}
	}
	// The admin belongs to the fixture's project only, and is still offered the other one.
	if _, ids := names(w.admin, "/api/users"); !slices.Contains(ids, w.other) {
		t.Fatalf("an admin who is not a member of the other project is not offered it: %v", ids)
	}
	if code, ids := names(w.pOwner, "/api/users?project_id="+w.f.project.ID); code != 200 || len(ids) != 0 {
		t.Fatalf("a project owner got a project list: %v (status %d)", ids, code)
	}
}

// Global memory is the owner's and the admins' to manage, with or without belonging to a project.
func TestAdminWithoutAProjectManagesGlobalMemory(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	loner := w.member(t, "lonely", "", "")
	if code, _ := w.json(t, loner, "GET", "/api/global-memory/agent_shared_notes", nil); code != 403 {
		t.Fatalf("an ordinary account without a project: %d", code)
	}
	if code := w.call(t, w.owner, "PUT", "/api/users/"+loner.id+"/admin", map[string]bool{"admin": true}); code != 204 {
		t.Fatalf("appointing: %d", code)
	}
	if code, out := w.json(t, loner, "GET", "/api/global-memory/agent_shared_notes", nil); code != 200 || out["can_edit"] != true {
		t.Fatalf("an admin without a project reads global memory: %d %v", code, out)
	}
	if code := w.call(t, loner, "POST", "/api/global-memory/agent_shared_notes", map[string]string{"scope_key": "global", "body": "shared"}); code != 201 {
		t.Fatalf("an admin without a project writes global memory: %d", code)
	}
}

// An admin is trusted like the owner, so harness settings are theirs too: they can list a harness's models, an operator or viewer cannot.
func TestAnAdminMayEditAHarnessAndItsModels(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	harness := w.f.harness(t, "claude", "native")
	path := "/api/machines/" + w.f.machineID + "/launchers/" + harness.ID + "/models"
	models := map[string]any{"models": []store.HarnessModel{{Slug: "claude-sonnet-5-5", Label: "Sonnet 5.5", Default: true}}}
	if code, body := w.json(t, w.admin, "PUT", path, models); code != 200 {
		t.Fatalf("an admin editing a harness's models: %d %v", code, body)
	}
	if saved, err := w.f.db.HarnessModels(t.Context(), harness.ID); err != nil || len(saved) != 1 || saved[0].Slug != "claude-sonnet-5-5" {
		t.Fatalf("the admin's list was saved: %+v %v", saved, err)
	}
	for name, who := range map[string]adminActor{"operator": w.oper, "viewer": w.viewer, "project owner": w.pOwner} {
		if code, _ := w.json(t, who, "PUT", path, models); code != 403 {
			t.Errorf("a %s must not edit a harness: %d", name, code)
		}
	}
}

package server

import (
	"testing"

	json "github.com/goccy/go-json"
)

// A holder of an invitation link is told what it is for before the page asks for anything: which project and role, or whose password.
func TestInspectInviteSaysWhatTheLinkIsFor(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	issue := func(body map[string]string) string {
		code, out := w.json(t, w.owner, "POST", "/api/invites", body)
		if code != 201 {
			t.Fatalf("issuing %v: %d %v", body, code, out)
		}
		return out["token"].(string)
	}
	inspect := func(token string) (int, map[string]any) {
		return w.json(t, adminActor{}, "POST", "/api/invites/inspect", map[string]string{"token": token})
	}
	join := issue(map[string]string{"kind": "join", "project_id": w.f.project.ID, "role": "operator"})
	if code, out := inspect(join); code != 200 || out["kind"] != "join" || out["role"] != "operator" || out["project_name"] != w.f.project.Name {
		t.Fatalf("join link: %d %v", code, out)
	}
	reset := issue(map[string]string{"kind": "reset", "target_user_id": w.viewer.id})
	if code, out := inspect(reset); code != 200 || out["kind"] != "reset" || out["login"] != "viewer" {
		t.Fatalf("reset link: %d %v", code, out)
	}
	// Looking is not using: the link still works afterwards.
	resp := userRequest(t, w.f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": reset, "password": "a secure password"})
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("redeeming after inspecting: %d", resp.StatusCode)
	}
	// A spent link, a made-up one and a malformed one all look the same.
	for name, token := range map[string]string{"spent": reset, "unknown": "aui_0123456789012345678901234567890123456789", "malformed": "nope"} {
		if code, out := inspect(token); code != 401 || out["login"] != nil || out["project_name"] != nil {
			t.Errorf("%s link: %d %v", name, code, out)
		}
	}
}

// The sign-in answers say what the account may do instance-wide, so the page can turn off what would only answer 403.
func TestSessionAnswersCarryTheAccountsRights(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	for _, c := range []struct {
		who          adminActor
		owner, admin bool
	}{{w.owner, true, true}, {w.admin, false, true}, {w.viewer, false, false}} {
		code, out := w.json(t, c.who, "GET", "/api/me", nil)
		if code != 200 || out["owner"] != c.owner || out["admin"] != c.admin {
			t.Errorf("%s: %d owner=%v admin=%v, want owner=%v admin=%v", c.who.name, code, out["owner"], out["admin"], c.owner, c.admin)
		}
	}
}

// The page asks whether anyone is signed in on every load; not being signed in is an answer, not an error.
func TestSessionProbeAnswersWithoutA401(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	resp := userRequest(t, w.f, nil, "", "GET", "/api/session", nil)
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || out["signed_in"] != false {
		t.Fatalf("signed out: %d %v", resp.StatusCode, out)
	}
	code, in := w.json(t, w.viewer, "GET", "/api/session", nil)
	if code != 200 || in["login"] != "viewer" || in["csrf_token"] == nil || in["owner"] != false {
		t.Fatalf("signed in: %d %v", code, in)
	}
}

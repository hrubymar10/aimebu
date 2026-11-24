package server

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// A person's login and an agent's slug never name both: an agent cannot take a login of any person (a member of another project too), and
// a person cannot take a slug of any agent, whatever the case; the 409 names the clashing name and what holds it.
func TestNoNameSharedByALoginAndAnAgent(t *testing.T) {
	f := namedSpawnFixture(t)
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.CreateUser(store.User{Login: "Stranger", PWHash: "h", PWParams: "{}"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Slug vs an existing login that belongs to no project of this one.
	status, body := f.spawnNamed(t, "stranger", map[string]any{"name": "stranger"})
	message, _ := body["message"].(string)
	if status != 409 || body["error"] != "name_in_use" || !strings.Contains(message, `"stranger"`) || !strings.Contains(message, "Stranger") {
		t.Fatalf("slug equal to a login elsewhere: %d %+v", status, body)
	}
	// A slug in a different spelling of the login is a different name: Stranger and stranger2 can live together.
	if status, body := f.spawnNamed(t, "bravo", map[string]any{"name": "bravo-one"}); status != 201 {
		t.Fatalf("agent bravo-one: %d %+v", status, body)
	}
	if status, body := f.spawnNamed(t, "other", map[string]any{"name": "stranger2"}); status != 201 {
		t.Fatalf("a different spelling is allowed: %d %+v", status, body)
	}
	// Login vs an existing slug, in any case, through an invite.
	token := readInvite(t, f.request("POST", "/api/invites", map[string]string{"project_id": f.project.ID, "role": "viewer"}, true))
	for _, login := range []string{"bravo-one", "Bravo-One", "STRANGER2"} {
		resp := userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": token, "login": login, "password": "a secure password"})
		var problem struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 409 || problem.Error != "login_in_use" || problem.Message != "that login is taken" {
			t.Fatalf("login %q equal to a slug: %d %+v", login, resp.StatusCode, problem)
		}
	}
	// The detailed holder text stays where the caller can see it: a direct CreateUser (an authenticated admin path) still names the agent.
	err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.CreateUser(store.User{Login: "bravo-one", PWHash: "h", PWParams: "{}"})
		return err
	})
	var inUse *store.NameInUseError
	if !errors.As(err, &inUse) || !strings.Contains(err.Error(), "the agent bravo-one@") {
		t.Fatalf("CreateUser of an agent's slug: %v", err)
	}
	// The invite is still good, and a legitimate login is accepted.
	resp := userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": token, "login": "bravo", "password": "a secure password"})
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("login bravo beside the agent bravo-one: %d", resp.StatusCode)
	}
}

// An unauthenticated redemption never learns the holder of a login (no "@", no project name), and each such refusal counts toward the redeem
// throttle, so agent names cannot be tried without limit; the earlier test shows a client below the limit still redeems with a free login after refusals.
func TestRedeemLoginInUseIsGenericAndThrottled(t *testing.T) {
	f := namedSpawnFixture(t)
	// The agent lives in another project B with a distinctive name; the invite is for project A.
	resp := f.request("POST", "/api/projects", map[string]string{"name": "zorbwick-secret", "folder": t.TempDir()}, true)
	var projectB store.Project
	if resp.StatusCode != 201 || json.NewDecoder(resp.Body).Decode(&projectB) != nil {
		t.Fatalf("project B: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request("POST", "/api/projects/"+projectB.ID+"/rooms", map[string]string{"title": "task"}, true)
	roomA := f.room
	if resp.StatusCode != 201 || json.NewDecoder(resp.Body).Decode(&f.room) != nil {
		t.Fatalf("room B: %d", resp.StatusCode)
	}
	resp.Body.Close()
	status, body := f.spawnNamed(t, "bravo", map[string]any{"name": "bravo-one"})
	f.room = roomA
	if status != 201 {
		t.Fatalf("agent bravo-one in project B: %d %+v", status, body)
	}
	token := readInvite(t, f.request("POST", "/api/invites", map[string]string{"project_id": f.project.ID, "role": "viewer"}, true))
	redeem := func(login string) (int, string) {
		resp := userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": token, "login": login, "password": "a secure password"})
		defer resp.Body.Close()
		var raw bytes.Buffer
		_, _ = raw.ReadFrom(resp.Body)
		return resp.StatusCode, raw.String()
	}
	limited := 0
	for i := 0; i < 12; i++ {
		status, body := redeem("bravo-one")
		switch status {
		case 409:
			if strings.Contains(body, "@") || strings.Contains(body, "bravo-one") || strings.Contains(body, "zorbwick") || strings.Contains(body, projectB.Name) || !strings.Contains(body, "that login is taken") {
				t.Fatalf("409 body leaks the holder: %s", body)
			}
		case 429:
			limited++
		default:
			t.Fatalf("attempt %d: %d %s", i, status, body)
		}
	}
	if limited == 0 {
		t.Fatal("repeated login_in_use redemptions were never throttled")
	}
}

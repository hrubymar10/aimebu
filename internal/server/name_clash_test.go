package server

import (
	"context"
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
		if resp.StatusCode != 409 || problem.Error != "login_in_use" || !strings.Contains(problem.Message, strings.ToLower(login)) || !strings.Contains(problem.Message, "agent") {
			t.Fatalf("login %q equal to a slug: %d %+v", login, resp.StatusCode, problem)
		}
	}
	// The invite is still good, and a legitimate login is accepted.
	resp := userRequest(t, f, nil, "", "POST", "/api/invites/redeem", map[string]string{"token": token, "login": "bravo", "password": "a secure password"})
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("login bravo beside the agent bravo-one: %d", resp.StatusCode)
	}
}

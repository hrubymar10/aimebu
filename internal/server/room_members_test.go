package server

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func decodeMembers(t *testing.T, response *http.Response) store.RoomMembers {
	t.Helper()
	defer response.Body.Close()
	var members store.RoomMembers
	if err := json.NewDecoder(response.Body).Decode(&members); err != nil {
		t.Fatal(err)
	}
	return members
}

func TestRoomMembersListsPeopleAgentsAndSingletons(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.createToolAgentNamed(t, "role_worker_default", "drew")
	lead, _ := f.createToolAgentNamed(t, "role_leader_default", "tessa")

	response := f.request("GET", "/api/rooms/"+f.room.ID+"/members", nil, false)
	if response.StatusCode != 200 {
		t.Fatalf("members: %d", response.StatusCode)
	}
	got := decodeMembers(t, response)
	if len(got.Humans) != 1 || got.Humans[0].Login != "owner" || got.Humans[0].Role != "owner" {
		t.Fatalf("humans = %+v", got.Humans)
	}
	if len(got.Agents) != 2 || got.Agents[0].Slug != "drew" || got.Agents[0].RoleLabel != "Worker" || got.Agents[0].Singleton ||
		got.Agents[1].Slug != "tessa" || got.Agents[1].RoleLabel != "Leader" || !got.Agents[1].Singleton || got.Agents[1].RoleEmoji == "" {
		t.Fatalf("agents = %+v", got.Agents)
	}
	var held *store.RoomSingletonHolder
	for _, singleton := range got.Singletons {
		if singleton.RoleKey == "leader" {
			held = singleton.HeldBy
		}
	}
	if held == nil || held.AgentID != lead.ID || held.Slug != "tessa" || !held.InThisRoom {
		t.Fatalf("lead holder = %+v (singletons %+v)", held, got.Singletons)
	}
	// Reading it changes nothing and needs no CSRF token; it is a plain GET.
	if again := decodeMembers(t, f.request("GET", "/api/rooms/"+f.room.ID+"/members", nil, false)); len(again.Agents) != 2 {
		t.Fatalf("second read differs: %+v", again)
	}
}

func TestRoomMembersFollowsRoomAccess(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	path := "/api/rooms/" + f.room.ID + "/members"
	if response := f.request("GET", "/api/rooms/missing/members", nil, false); response.StatusCode != 404 {
		t.Fatalf("unknown room: %d", response.StatusCode)
	}
	if response := f.request("GET", path, nil, false); response.StatusCode != 200 {
		t.Fatalf("owner: %d", response.StatusCode)
	}
	// Someone with no membership in the project cannot read who is in its rooms.
	cookie, csrf := registerSecondUser(t, f, "outsider")
	outsider := f.requestAs(t, cookie, csrf, http.MethodGet, path, nil)
	defer outsider.Body.Close()
	if outsider.StatusCode == 200 {
		t.Fatal("a non-member read a room's members")
	}
	// Signed out: refused.
	response, err := http.Get(f.http.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("signed out: %d", response.StatusCode)
	}
}

// A private chat's member list is for its peers only. A project member who is not in
// the chat is refused, and the chat lists its own two people and the agent in it, and no
// roles. The holder of a singleton role is reported without the room it works in: a lead
// that also has a private chat with someone reveals neither that chat's id nor its
// title to anyone else.
func TestRoomMembersOfAPrivateChatAreForItsPeersAndNeverLeakThroughSingletons(t *testing.T) {
	f := newFixture(t)
	f.seed()
	lead, _ := f.createToolAgentNamed(t, "role_leader_default", "tessa")
	peerCookie, peerCSRF := registerSecondUser(t, f, "peer")
	bystanderCookie, bystanderCSRF := registerSecondUser(t, f, "bystander")
	for _, login := range []string{"peer", "bystander"} {
		user, err := f.db.UserByLogin(t.Context(), login)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
			_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'viewer')`, f.project.ID, user.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	peer, err := f.db.UserByLogin(t.Context(), "peer")
	if err != nil {
		t.Fatal(err)
	}
	dm, err := f.db.OpenDM(t.Context(), peer.ID, lead.ID) // the lead's private chat with the peer
	if err != nil {
		t.Fatal(err)
	}
	dmPath := "/api/rooms/" + dm.ID + "/members"

	ownerView := f.request("GET", dmPath, nil, false) // the owner is not a peer of this chat
	ownerView.Body.Close()
	if ownerView.StatusCode == 200 {
		t.Fatal("a project owner who is not a peer read a private chat's members")
	}
	bystander := f.requestAs(t, bystanderCookie, bystanderCSRF, http.MethodGet, dmPath, nil)
	bystander.Body.Close()
	if bystander.StatusCode != 403 {
		t.Fatalf("a project member outside the chat: %d, want 403", bystander.StatusCode)
	}
	own := f.requestAs(t, peerCookie, peerCSRF, http.MethodGet, dmPath, nil)
	if own.StatusCode != 200 {
		t.Fatalf("the chat's own peer: %d", own.StatusCode)
	}
	got := decodeMembers(t, own)
	if len(got.Humans) != 1 || got.Humans[0].Login != "peer" || len(got.Agents) != 1 || got.Agents[0].Slug != "tessa" || len(got.Singletons) != 0 {
		t.Fatalf("private chat members = %+v", got)
	}

	// The ordinary room, read by someone who cannot see the chat: the lead holds its
	// singleton role, and nothing in the answer names the chat.
	room := f.requestAs(t, bystanderCookie, bystanderCSRF, http.MethodGet, "/api/rooms/"+f.room.ID+"/members", nil)
	defer room.Body.Close()
	if room.StatusCode != 200 {
		t.Fatalf("ordinary room: %d", room.StatusCode)
	}
	raw, err := io.ReadAll(room.Body)
	if err != nil {
		t.Fatal(err)
	}
	// (A private chat's title is made of its participants' names, which legitimately
	// appear here as people and agents, so only its id and kind can prove a leak.)
	if bytes.Contains(raw, []byte(dm.ID)) || bytes.Contains(raw, []byte(`"dm"`)) {
		t.Fatalf("the ordinary room's members name the private chat: %s", raw)
	}
	var members store.RoomMembers
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatal(err)
	}
	var held *store.RoomSingletonHolder
	for _, singleton := range members.Singletons {
		if singleton.RoleKey == "leader" {
			held = singleton.HeldBy
		}
	}
	if held == nil || held.Slug != "tessa" {
		t.Fatalf("the lead is not shown as the holder: %+v", members.Singletons)
	}
}

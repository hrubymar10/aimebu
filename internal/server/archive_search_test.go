package server

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestArchiveSearchEndpointProjectAndDMPrivacy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	exec := func(query string, args ...any) {
		t.Helper()
		if err := f.db.Tx(t.Context(), func(tx *store.Tx) error { _, err := tx.Exec(query, args...); return err }); err != nil {
			t.Fatal(err)
		}
	}
	message, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "find archive needle"})
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, f.room.ID)
	path := "/api/projects/" + f.project.ID + "/archive/search?q=" + url.QueryEscape("needle")
	resp := f.request(http.MethodGet, path, nil, false)
	var hits []store.ArchiveHit
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&hits) != nil || len(hits) != 1 || hits[0].MessageID != message.ID {
		t.Fatalf("owner search: %d %+v", resp.StatusCode, hits)
	}
	resp.Body.Close()
	resp = f.request(http.MethodGet, "/api/projects/"+f.project.ID+"/archive/search?q=", nil, false)
	if resp.StatusCode != 400 {
		t.Fatalf("blank query: %d", resp.StatusCode)
	}
	resp.Body.Close()
	cookie, csrf := registerSecondUser(t, f, "archive-peer")
	resp = f.requestAs(t, cookie, csrf, http.MethodGet, path, nil)
	if resp.StatusCode != 403 {
		t.Fatalf("nonmember search: %d", resp.StatusCode)
	}
	resp.Body.Close()
	peer, err := f.db.UserByLogin(t.Context(), "archive-peer")
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'operator')`, f.project.ID, peer.ID)
	dm, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, Kind: "dm", DMKey: "archive-private", Title: "Private", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(t.Context(), dm.ID, "user", owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: dm.ID, AuthorKind: "user", AuthorID: owner.ID, Body: "needle private"}); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, dm.ID)
	resp = f.requestAs(t, cookie, csrf, http.MethodGet, path, nil)
	hits = nil
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&hits) != nil || len(hits) != 1 || hits[0].RoomID != f.room.ID {
		t.Fatalf("other member saw private DM: %d %+v", resp.StatusCode, hits)
	}
	resp.Body.Close()
}

package server

import (
	"net/http"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestPersonMessageRouteRefusesClosingAndClosedRooms(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	path := "/api/rooms/" + f.room.ID + "/messages"
	for _, lifecycle := range []string{"closing", "closed"} {
		if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
			_, err := tx.Exec(`UPDATE room SET lifecycle=? WHERE id=?`, lifecycle, f.room.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		resp := f.request(http.MethodPost, path, map[string]string{"body": "late message"}, true)
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("%s room accepted a person's message: status=%d", lifecycle, resp.StatusCode)
		}
	}
}

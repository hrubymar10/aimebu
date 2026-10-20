package server

import (
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestSystemEventsAndMessageDebugEndpoints(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	otherCookie, otherCSRF := registerSecondUser(t, f, "second")
	resp := f.request(http.MethodGet, "/api/system-events?limit=0", nil, false)
	if resp.StatusCode != 400 {
		t.Fatalf("zero limit: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodGet, "/api/system-events", nil, false)
	var events struct {
		Events []map[string]any `json:"events"`
		Kinds  []string         `json:"kinds"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&events) != nil || events.Events == nil || len(events.Kinds) == 0 {
		t.Fatalf("system events: %d %+v", resp.StatusCode, events)
	}
	resp.Body.Close()
	message, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "debug me `@owner` and @nobody"})
	if err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodGet, "/api/messages/"+message.ID+"/debug", nil, false)
	var debug struct {
		Message    map[string]any `json:"message"`
		Addressed  []string       `json:"addressed_to"`
		Notes      []any          `json:"notes"`
		Events     []any          `json:"events"`
		Addressing struct {
			Mentions []struct {
				Token   string `json:"token"`
				Outcome string `json:"outcome"`
			} `json:"mentions"`
		} `json:"addressing"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&debug) != nil || debug.Message["body"] != "debug me `@owner` and @nobody" || debug.Notes == nil || len(debug.Events) == 0 {
		t.Fatalf("debug: %d %+v", resp.StatusCode, debug)
	}
	outcomes := map[string]string{}
	for _, mention := range debug.Addressing.Mentions {
		outcomes[mention.Token] = mention.Outcome
	}
	if outcomes["owner"] != "ignored" || outcomes["nobody"] != "no_match" {
		t.Fatalf("the trace must say why a backticked mention missed: %+v", debug.Addressing.Mentions)
	}
	resp.Body.Close()
	resp = f.requestAs(t, otherCookie, otherCSRF, http.MethodGet, "/api/messages/"+message.ID+"/debug", nil)
	if resp.StatusCode != 403 && resp.StatusCode != 404 {
		t.Fatalf("a user outside the project must not inspect: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodGet, "/api/messages/msg_missing/debug", nil, false)
	if resp.StatusCode != 404 {
		t.Fatalf("missing: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

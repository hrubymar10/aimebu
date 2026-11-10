package server

import (
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

// ?notable=1 is what the bell reads: the notable events only, marked, as the newest `limit` of those; the plain history keeps everything.
func TestSystemEventsNotableOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		for _, event := range []store.Event{
			{Kind: "agent_spawn_failed", ProjectID: f.project.ID, RoomID: f.room.ID, PayloadJSON: `{"error":"x"}`},
			{Kind: "turn_process_exit", ProjectID: f.project.ID, RoomID: f.room.ID, PayloadJSON: `{"exit_code":"0"}`},
			{Kind: "turn_process_exit", ProjectID: f.project.ID, RoomID: f.room.ID, PayloadJSON: `{"exit_code":"1"}`},
			{Kind: "room_renamed", ProjectID: f.project.ID, RoomID: f.room.ID, PayloadJSON: `{}`},
		} {
			if _, err := tx.AppendEvent(event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	read := func(query string) (kinds []string, marked []bool) {
		resp := f.request(http.MethodGet, "/api/system-events"+query, nil, false)
		defer resp.Body.Close()
		var body struct {
			Events []struct {
				Kind    string `json:"kind"`
				Notable bool   `json:"notable"`
			} `json:"events"`
		}
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&body) != nil {
			t.Fatalf("system events %s: %d", query, resp.StatusCode)
		}
		for _, event := range body.Events {
			kinds = append(kinds, event.Kind)
			marked = append(marked, event.Notable)
		}
		return kinds, marked
	}
	kinds, marked := read("?notable=1")
	if len(kinds) != 2 || kinds[0] != "turn_process_exit" || kinds[1] != "agent_spawn_failed" || !marked[0] || !marked[1] {
		t.Fatalf("the notable feed: %v %v", kinds, marked)
	}
	if one, _ := read("?notable=1&limit=1"); len(one) != 1 || one[0] != "turn_process_exit" {
		t.Fatalf("a page is the newest notable events: %v", one)
	}
	if all, _ := read(""); len(all) < 4 {
		t.Fatalf("the plain history has everything: %v", all)
	}
}

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

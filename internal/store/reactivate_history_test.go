package store

import (
	"context"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func TestStoppedAgentResumesNativeSessionOrStartsFreshWithHistory(t *testing.T) {
	t.Parallel()
	s, project, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_restart','owner','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_restart','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET tool='claude',desired='stopped',observed='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	const native = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	if _, err := s.db.Exec(`UPDATE session SET native_session_id=?,native_session_started_at=? WHERE agent_id=?`, native, timestamp(), agent.ID); err != nil {
		t.Fatal(err)
	}
	start := func(fresh bool, rev int64) AgentCard {
		t.Helper()
		var card AgentCard
		err := s.Tx(ctx, func(tx *Tx) (err error) {
			if fresh {
				card, err = tx.StartAgentFresh(agent.ID, "usr_restart", rev)
			} else {
				card, err = tx.SetAgentDesired(agent.ID, "usr_restart", "active", rev)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return card
	}
	continued := start(false, 0)
	var mode, handle string
	var started *string
	if err := s.db.QueryRow(`SELECT start_mode,native_session_id,native_session_started_at FROM session WHERE agent_id=? AND generation=1`, agent.ID).Scan(&mode, &handle, &started); err != nil || mode != "native_resume" || handle != native || started == nil {
		t.Fatalf("continued session: %q %q %v %v", mode, handle, started, err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	old, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: agent.ID, Body: "Earlier answer from this agent"})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "agent", AuthorID: "legacy@wake", Body: "Imported conversation detail"})
	if err != nil {
		t.Fatal(err)
	}
	newNote, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "system", AuthorID: "system", Body: "New request"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO note(id,agent_id,message_id,room_id,state,wake_reason,created_at) VALUES ('note_restart',?,?,?,'queued','mention',?)`, agent.ID, newNote.ID, room.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	fresh := start(true, continued.DesiredRev)
	if err := s.db.QueryRow(`SELECT start_mode FROM session WHERE agent_id=? AND generation=2`, agent.ID).Scan(&mode); err != nil || mode != "fresh_with_history" {
		t.Fatalf("fresh mode: %q %v", mode, err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, agent.ID, "")
	if err != nil || !ready || delivery.Generation != fresh.Generation || delivery.NativeSessionStarted {
		t.Fatalf("fresh delivery: ready=%t generation=%d err=%v", ready, delivery.Generation, err)
	}
	var contextData struct {
		History []AgentMessage `json:"recent_room_history"`
	}
	if err := json.Unmarshal(delivery.Context, &contextData); err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, message := range contextData.History {
		bodies = append(bodies, message.Body)
	}
	joined := strings.Join(bodies, " | ")
	if !strings.Contains(joined, old.Body) || !strings.Contains(joined, imported.Body) {
		t.Fatalf("fresh history omitted prior conversation: %s", joined)
	}
}

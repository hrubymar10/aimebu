package store

import (
	"context"
	"testing"

	"github.com/goccy/go-json"
)

// blockRoles names what a wake's context carried for the role body and the knowledge snapshot: the full text (a string for the role
// body, an object for the knowledge) or the unchanged marker.
func blockRoles(contextJSON []byte) (fullRole, markedRole, fullKnowledge, markedKnowledge bool) {
	var data struct {
		RoleBody  json.RawMessage `json:"role_body"`
		Knowledge json.RawMessage `json:"knowledge"`
	}
	if err := json.Unmarshal(contextJSON, &data); err != nil {
		return false, false, false, false
	}
	var roleString string
	var roleMarker map[string]any
	var knowledgeObject map[string]any
	var knowledgeMarker map[string]any
	fullRole = json.Unmarshal(data.RoleBody, &roleString) == nil
	markedRole = json.Unmarshal(data.RoleBody, &roleMarker) == nil && roleMarker["unchanged"] == true
	fullKnowledge = len(data.Knowledge) > 0 && json.Unmarshal(data.Knowledge, &knowledgeObject) == nil && knowledgeObject["unchanged"] == nil
	markedKnowledge = json.Unmarshal(data.Knowledge, &knowledgeMarker) == nil && knowledgeMarker["unchanged"] == true
	return fullRole, markedRole, fullKnowledge, markedKnowledge
}

// The role body and the knowledge snapshot go out in full on the first turn of a harness session and as a short unchanged marker on the
// wakes after it, until the revision changes, the session is new, the context was compacted, or the delivery that carried them never
// reached the harness.
func TestDispatchContextCarriesTheFullBlocksOncePerSession(t *testing.T) {
	t.Parallel()
	s, project, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	// The marker is only sent to the harnesses whose adapters report compaction events; claude is one.
	if _, err := s.db.Exec(`UPDATE agent SET tool='claude' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one handle this"}); err != nil {
		t.Fatal(err)
	}
	read := func() (turnID string, contextJSON []byte) {
		t.Helper()
		got, ready, err := s.PrepareDispatch(ctx, one.ID, "")
		if err != nil || !ready {
			t.Fatalf("prepare ready=%t err=%v", ready, err)
		}
		return got.TurnID, got.Context
	}
	settle := func(turnID string) {
		t.Helper()
		if _, err := s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE id=?`, turnID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, one.ID); err != nil {
			t.Fatal(err)
		}
		// A usage event proves the turn got far enough for its harness to have taken the context in, so what the turn delivered
		// in full counts as seen.
		if err := s.RecordTurnEvent(ctx, turnID, one.ID, "act-"+turnID, "usage", "", usageFrame(map[string]any{"tokens_in": 5})); err != nil {
			t.Fatal(err)
		}
	}
	say := func(body string) {
		t.Helper()
		if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	turn1, context1 := read()
	fullRole, _, fullKnowledge, _ := blockRoles(context1)
	if !fullRole || !fullKnowledge {
		t.Fatalf("the first turn carries the full blocks: %s", context1)
	}
	settle(turn1)

	say("@one again")
	turn2, context2 := read()
	_, markedRole, _, markedKnowledge := blockRoles(context2)
	if !markedRole || !markedKnowledge {
		t.Fatalf("the next wake carries the markers: %s", context2)
	}
	settle(turn2)

	// A changed role body is delivered in full again; the knowledge stays a marker.
	if _, err := s.db.Exec(`UPDATE role SET body='work, changed',rev=rev+1 WHERE id='role_worker'`); err != nil {
		t.Fatal(err)
	}
	say("@one third")
	turn3, context3 := read()
	roleFull3, roleMarked3, _, knowledgeMarked3 := blockRoles(context3)
	if !roleFull3 || roleMarked3 || !knowledgeMarked3 {
		t.Fatalf("a changed role body: %s", context3)
	}
	settle(turn3)

	// A changed knowledge snapshot is delivered in full again; the role body stays a marker.
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO settings_project(project_id,memory_enabled) VALUES (?,1)`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE settings_project SET memory_enabled=1 WHERE project_id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO project_memory(id,project_id,body,rev,author_kind,author_id,created_at,updated_at) VALUES ('pm_ctx1',?,'a fact',1,'user','owner','now','now')`, project.ID); err != nil {
		t.Fatal(err)
	}
	say("@one fourth")
	turn4, context4 := read()
	_, roleMarked4, knowledgeFull4, knowledgeMarked4 := blockRoles(context4)
	if !roleMarked4 || !knowledgeFull4 || knowledgeMarked4 {
		t.Fatalf("a changed knowledge snapshot: %s", context4)
	}
	settle(turn4)

	// A new harness session (a restart, a resume, a context reset) gets the full text again.
	if _, err := s.db.Exec(`UPDATE session SET ended_at='now' WHERE id='ses_one'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET generation=1 WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_one_b',?,1,'native_resume','now')`, one.ID); err != nil {
		t.Fatal(err)
	}
	say("@one fifth")
	turn5, context5 := read()
	fullRole, _, fullKnowledge, _ = blockRoles(context5)
	if !fullRole || !fullKnowledge {
		t.Fatalf("a new session carries the full blocks: %s", context5)
	}
	settle(turn5)

	// A compaction inside the session distils what earlier turns delivered: the full text goes out again.
	if err := s.RecordTurnEvent(ctx, turn5, one.ID, "comp-ctx-1", "compaction", "", []byte(`{"type":"compaction","compaction":{"pre_tokens":9000,"post_tokens":900}}`)); err != nil {
		t.Fatal(err)
	}
	say("@one sixth")
	turn6, context6 := read()
	fullRole, _, fullKnowledge, _ = blockRoles(context6)
	if !fullRole || !fullKnowledge {
		t.Fatalf("a wake after a compaction carries the full blocks: %s", context6)
	}
	settle(turn6)

	// A wake whose delivery never reached the harness (it was lost) leaves the block unconfirmed: the next wake carries the full
	// text again rather than a marker the harness could not act on.
	if _, err := s.db.Exec(`UPDATE session_context_delivery SET role_confirmed=0 WHERE session_id='ses_one_b'`); err != nil {
		t.Fatal(err)
	}
	say("@one seventh")
	turn7, context7 := read()
	fullRole, _, _, _ = blockRoles(context7)
	if !fullRole {
		t.Fatalf("a wake after an unconfirmed delivery carries the full role body: %s", context7)
	}
	settle(turn7)
}

// A harness whose adapter reports no compaction events (codex, opencode, vibe, and the test fake) always receives the full role body and
// knowledge, and the store keeps no per-session delivery state for it.
func TestDispatchContextAlwaysFullForHarnessesWithoutCompactionEvents(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	for _, tool := range []string{"codex", "fake"} {
		if _, err := s.db.Exec(`UPDATE agent SET tool=? WHERE id=?`, tool, one.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one handle this"}); err != nil {
			t.Fatal(err)
		}
		first, ready, err := s.PrepareDispatch(ctx, one.ID, "")
		if err != nil || !ready {
			t.Fatalf("%s: prepare ready=%t err=%v", tool, ready, err)
		}
		fullRole, _, fullKnowledge, _ := blockRoles(first.Context)
		if !fullRole || !fullKnowledge {
			t.Fatalf("%s: the first turn carries the full blocks: %s", tool, first.Context)
		}
		if err := s.CancelPreparedDispatch(ctx, first.TurnID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one again"}); err != nil {
			t.Fatal(err)
		}
		second, ready, err := s.PrepareDispatch(ctx, one.ID, "")
		if err != nil || !ready {
			t.Fatalf("%s: second prepare ready=%t err=%v", tool, ready, err)
		}
		fullRole, _, fullKnowledge, marked := blockRoles(second.Context)
		if !fullRole || !fullKnowledge || marked {
			t.Fatalf("%s: a later wake still carries the full blocks: %s", tool, second.Context)
		}
		if err := s.CancelPreparedDispatch(ctx, second.TurnID); err != nil {
			t.Fatal(err)
		}
		var rows int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM session_context_delivery`).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("%s: no delivery state is kept: %d rows, %v", tool, rows, err)
		}
	}
}

// A claude session's context carries role_body_system (the full role body) on every turn, so the system prompt argument stays stable while
// role_body itself shrinks to the marker.
func TestDispatchContextCarriesTheRoleBodyForTheSystemPrompt(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agent SET tool='claude' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one handle this"}); err != nil {
		t.Fatal(err)
	}
	read := func() (turnID string, roleBody, roleBodySystem json.RawMessage) {
		t.Helper()
		got, ready, err := s.PrepareDispatch(ctx, one.ID, "")
		if err != nil || !ready {
			t.Fatalf("prepare ready=%t err=%v", ready, err)
		}
		var data struct {
			RoleBody       json.RawMessage `json:"role_body"`
			RoleBodySystem json.RawMessage `json:"role_body_system"`
		}
		if err := json.Unmarshal(got.Context, &data); err != nil {
			t.Fatal(err)
		}
		return got.TurnID, data.RoleBody, data.RoleBodySystem
	}
	turn1, roleBody, roleBodySystem := read()
	var full string
	if err := json.Unmarshal(roleBody, &full); err != nil || full != "work" {
		t.Fatalf("the first turn's role_body: %s", roleBody)
	}
	if err := json.Unmarshal(roleBodySystem, &full); err != nil || full != "work" {
		t.Fatalf("the first turn's role_body_system: %s", roleBodySystem)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE id=?`, turn1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnEvent(ctx, turn1, one.ID, "act-sys", "usage", "", usageFrame(map[string]any{"tokens_in": 5})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one again"}); err != nil {
		t.Fatal(err)
	}
	_, roleBody, roleBodySystem = read()
	var marker map[string]any
	if err := json.Unmarshal(roleBody, &marker); err != nil || marker["unchanged"] != true {
		t.Fatalf("the marker turn's role_body: %s", roleBody)
	}
	if err := json.Unmarshal(roleBodySystem, &full); err != nil || full != "work" {
		t.Fatalf("the marker turn's role_body_system stays full: %s", roleBodySystem)
	}
}

// A turn that was dispatched and then cancelled (its delivery unconfirmed) never counts as delivered: the next wake carries the full text.
func TestCancelledDispatchLeavesTheBlocksUnconfirmed(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agent SET tool='claude' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one handle this"}); err != nil {
		t.Fatal(err)
	}
	first, ready, err := s.PrepareDispatch(ctx, one.ID, "")
	if err != nil || !ready {
		t.Fatalf("prepare ready=%t err=%v", ready, err)
	}
	if err := s.CancelPreparedDispatch(ctx, first.TurnID); err != nil {
		t.Fatal(err)
	}
	second, ready, err := s.PrepareDispatch(ctx, one.ID, "")
	if err != nil || !ready {
		t.Fatalf("second prepare ready=%t err=%v", ready, err)
	}
	fullRole, _, _, _ := blockRoles(second.Context)
	if !fullRole {
		t.Fatalf("a wake after a cancelled dispatch carries the full role body: %s", second.Context)
	}
}

// A delivery counts as seen only on a usage event or the turn's process exit: an early activity event (a turn that started and died
// before anything was measured) proves nothing about the session's persistence, so the next wake still carries the full text.
func TestDeliveryConfirmationNeedsAUsageOrTerminalEvent(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agent SET tool='claude' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one handle this"}); err != nil {
		t.Fatal(err)
	}
	read := func() (turnID string, contextJSON []byte) {
		t.Helper()
		got, ready, err := s.PrepareDispatch(ctx, one.ID, "")
		if err != nil || !ready {
			t.Fatalf("prepare ready=%t err=%v", ready, err)
		}
		return got.TurnID, got.Context
	}
	say := func(body string) {
		t.Helper()
		if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	settle := func(turnID string) {
		t.Helper()
		if _, err := s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE id=?`, turnID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, one.ID); err != nil {
			t.Fatal(err)
		}
	}
	turn1, _ := read()
	settle(turn1)
	// An activity event confirms nothing.
	if err := s.RecordTurnEvent(ctx, turn1, one.ID, "act-only", "activity", "", []byte(`{"type":"activity"}`)); err != nil {
		t.Fatal(err)
	}
	var confirmed int
	if err := s.db.QueryRow(`SELECT role_confirmed FROM session_context_delivery WHERE session_id='ses_one'`).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed != 0 {
		t.Fatalf("an activity event confirmed the delivery, want it unconfirmed")
	}
	// A usage event confirms it.
	if err := s.RecordTurnEvent(ctx, turn1, one.ID, "use-1", "usage", "", usageFrame(map[string]any{"tokens_in": 5})); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT role_confirmed FROM session_context_delivery WHERE session_id='ses_one'`).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed != 1 {
		t.Fatalf("a usage event did not confirm the delivery")
	}
	// And the next wake carries the marker.
	say("@one again")
	turn2, context2 := read()
	_, markedRole, _, _ := blockRoles(context2)
	if !markedRole {
		t.Fatalf("the wake after a usage event carries the full role body: %s", context2)
	}
	settle(turn2)
}

// The Claude CLI can answer its own query about a background task before it reads the turn's input; that result's spend reaches the store
// as background usage. If the turn is then cancelled or killed, the session never read the role or knowledge it carried: the background
// usage is recorded but confirms nothing, so the next wake on the same session carries the full text again.
func TestBackgroundUsageDoesNotConfirmDelivery(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agent SET tool='claude' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one handle this"}); err != nil {
		t.Fatal(err)
	}
	first, ready, err := s.PrepareDispatch(ctx, one.ID, "")
	if err != nil || !ready {
		t.Fatalf("prepare ready=%t err=%v", ready, err)
	}
	if err := s.RecordTurnEvent(ctx, first.TurnID, one.ID, "use-bg", "usage", "", usageFrame(map[string]any{"tokens_out": 7, "cost_estimate": 0.02, "cost_cumulative": true, "background": true})); err != nil {
		t.Fatal(err)
	}
	var confirmed, records int
	if err := s.db.QueryRow(`SELECT role_confirmed FROM session_context_delivery WHERE session_id='ses_one'`).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_record WHERE turn_id=?`, first.TurnID).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if confirmed != 0 || records != 1 {
		t.Fatalf("background usage: role_confirmed=%d usage records=%d, want unconfirmed and its spend recorded", confirmed, records)
	}
	// The turn is cancelled before the harness reads its input: it ends with no usage of its own.
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE id=?`, first.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@one again"}); err != nil {
		t.Fatal(err)
	}
	second, ready, err := s.PrepareDispatch(ctx, one.ID, "")
	if err != nil || !ready {
		t.Fatalf("second prepare ready=%t err=%v", ready, err)
	}
	if fullRole, markedRole, _, _ := blockRoles(second.Context); !fullRole || markedRole {
		t.Fatalf("the wake after a cancelled turn with only background usage carries the unchanged marker: %s", second.Context)
	}
}

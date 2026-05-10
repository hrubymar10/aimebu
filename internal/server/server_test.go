package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hrubymar10/aimebu/internal/types"
)

func setupTestServer(t *testing.T) (*store, *httptest.Server) {
	t.Helper()
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	setupHandlers(mux, s)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

func TestDeleteAgentDeregistersAndRemovesMemberships(t *testing.T) {
	s, srv := setupTestServer(t)

	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "workerbee")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.registerAI("opus4.7", "claude-code", "test", nil, "reviewpal")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.createRoom("ops", other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("ops", agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("ops", other.ID); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/agents/"+agent.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /agents returned %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	for _, listed := range s.listAgents() {
		if listed.ID == agent.ID {
			t.Fatalf("agent %q still present after deregistration", agent.ID)
		}
	}

	room := s.getRoom("ops")
	if room == nil {
		t.Fatal("room ops missing after deregistration")
	}
	for _, member := range room.Members {
		if member == agent.ID {
			t.Fatalf("agent %q still in room membership after deregistration", agent.ID)
		}
	}

	systemMsgs := s.messages["_system"]
	if len(systemMsgs) == 0 {
		t.Fatal("expected _system deregistration event")
	}
	last := systemMsgs[len(systemMsgs)-1]
	if last.Body != agent.ID+" deregistered" {
		t.Fatalf("last _system message = %q, want %q", last.Body, agent.ID+" deregistered")
	}
}

func TestDeleteAgentReturnsNotFoundForUnknownAgent(t *testing.T) {
	_, srv := setupTestServer(t)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/agents/nope@aimebu", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE /agents for unknown agent returned %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestListAgentsConcurrentMutation(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	agent, _, err := s.registerAI("gpt5", "codex", "test", map[string]string{
		"protocol":  "agent",
		"spawn_tag": "list-agents-race",
	}, "workerbee")
	if err != nil {
		t.Fatal(err)
	}

	// Seed both mutable map fields so the snapshot path must clone them.
	if !s.advanceCursor(agent.ID, "seed-room", 1) {
		t.Fatal("expected initial cursor advance to seed read_cursors")
	}

	deadline := time.Now().Add(150 * time.Millisecond)
	start := make(chan struct{})
	var wg sync.WaitGroup

	marshalWorker := func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			evt := MetaEvent{Type: "agent_update", Data: map[string]any{"agents": s.listAgents()}}
			if _, err := json.Marshal(evt); err != nil {
				t.Errorf("marshal failed: %v", err)
				return
			}
		}
	}
	cursorWorker := func(roomIdx int) {
		defer wg.Done()
		<-start
		var cursor int64 = 2
		roomID := fmt.Sprintf("race-room-%d", roomIdx)
		for time.Now().Before(deadline) {
			s.advanceCursor(agent.ID, roomID, cursor)
			cursor++
		}
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go marshalWorker()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go cursorWorker(i)
	}

	close(start)
	wg.Wait()
}

func TestRegisterResponseConcurrentMetaMutation(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(150 * time.Millisecond)
	start := make(chan struct{})
	var wg sync.WaitGroup

	registerWorker := func(workerIdx int) {
		defer wg.Done()
		<-start
		for iter := 0; time.Now().Before(deadline); iter++ {
			agent, _, err := s.registerAI("gpt5", "codex", "test", map[string]string{
				"protocol":  "agent",
				"spawn_tag": "register-race",
				"seq":       fmt.Sprintf("%d-%d", workerIdx, iter),
			}, "workerbee")
			if err != nil {
				t.Errorf("registerAI failed: %v", err)
				return
			}
			resp := types.RegisterResponse{
				ID:        agent.ID,
				Name:      agent.Name,
				Kind:      agent.Kind,
				Model:     agent.Model,
				Harness:   agent.Harness,
				Project:   agent.Project,
				Meta:      agent.Meta,
				Reclaimed: false,
			}
			if _, err := json.Marshal(resp); err != nil {
				t.Errorf("marshal register response failed: %v", err)
				return
			}
		}
	}

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go registerWorker(i)
	}

	close(start)
	wg.Wait()
}

// TestRoomWaitCursorNotAdvancedOnContextCancel verifies that cancelling the
// request context (simulating a harness timeout) does not advance the agent's
// read cursor. A message posted after the disconnect must be replayed on the
// next bus_wait call.
//
// Note: this test exercises the context.Done() exit path. The tighter race —
// message received on the channel, context cancels before the write — is
// protected by the r.Context().Err() guard added in the same commit and is
// validated by code review.
func TestRoomWaitCursorNotAdvancedOnContextCancel(t *testing.T) {
	s, srv := setupTestServer(t)

	sender, _, err := s.registerAI("gpt5", "codex", "test", nil, "sndrone")
	if err != nil {
		t.Fatal(err)
	}
	receiver, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "rcvrone")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.createRoom("race-room", sender.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("race-room", sender.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("race-room", receiver.ID); err != nil {
		t.Fatal(err)
	}

	// Advance cursor past join-event messages so the room wait enters slow-path.
	s.advanceCursor(receiver.ID, "race-room", s.roomHead("race-room"))
	cursorBefore := s.ensureRoomCursor(receiver.ID, "race-room")

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequestWithContext(ctx, "GET",
			srv.URL+"/rooms/race-room/wait?agent_id="+receiver.ID+"&timeout=5", nil)
		//nolint:errcheck
		http.DefaultClient.Do(req)
	}()

	// Let handler enter the slow-path select loop.
	time.Sleep(50 * time.Millisecond)

	// Simulate harness timeout: cancel the request context.
	cancel()
	time.Sleep(20 * time.Millisecond)

	// Post a message after the disconnect — handler won't deliver it.
	if _, err := s.roomSend("race-room", sender.ID, "@rcvrone hello after disconnect", false); err != nil {
		t.Fatal(err)
	}

	wg.Wait()

	cursorAfter := s.ensureRoomCursor(receiver.ID, "race-room")
	if cursorAfter != cursorBefore {
		t.Errorf("cursor advanced on disconnect: was %d, now %d (message was NOT delivered to client)", cursorBefore, cursorAfter)
	}
}

// TestRoomWaitCursorAdvancedOnDelivery verifies that the read cursor advances
// after a message is successfully delivered to the waiting agent.
func TestRoomWaitCursorAdvancedOnDelivery(t *testing.T) {
	s, srv := setupTestServer(t)

	sender, _, err := s.registerAI("gpt5", "codex", "test", nil, "sndrtwo")
	if err != nil {
		t.Fatal(err)
	}
	receiver, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "rcvrtwo")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.createRoom("deliver-room", sender.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("deliver-room", sender.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("deliver-room", receiver.ID); err != nil {
		t.Fatal(err)
	}

	// Advance cursor past join-event messages so the room wait enters slow-path.
	s.advanceCursor(receiver.ID, "deliver-room", s.roomHead("deliver-room"))
	cursorBefore := s.ensureRoomCursor(receiver.ID, "deliver-room")

	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		req, _ := http.NewRequest("GET",
			srv.URL+"/rooms/deliver-room/wait?agent_id="+receiver.ID+"&timeout=5", nil)
		//nolint:errcheck
		http.DefaultClient.Do(req)
	}()

	// Let handler enter slow-path.
	time.Sleep(50 * time.Millisecond)

	// Post a message — handler should deliver it and advance the cursor.
	if _, err := s.roomSend("deliver-room", sender.ID, "hello rcvrtwo", false); err != nil {
		t.Fatal(err)
	}

	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("wait handler did not complete within timeout")
	}

	cursorAfter := s.ensureRoomCursor(receiver.ID, "deliver-room")
	if cursorAfter <= cursorBefore {
		t.Errorf("cursor did not advance after delivery: was %d, now %d", cursorBefore, cursorAfter)
	}
}

// TestRegisterReclaimsBySpawnTag verifies that registering twice with the same
// (spawn_tag, model, harness, project) returns the same agent name.
func TestRegisterReclaimsBySpawnTag(t *testing.T) {
	s, _ := setupTestServer(t)

	meta := map[string]string{"spawn_tag": "test-tag-abc123"}
	first, reclaimed, err := s.registerAI("opus4.7", "claude-code", "proj", meta, "")
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed {
		t.Error("first registration should not be reclaimed")
	}

	second, reclaimed, err := s.registerAI("opus4.7", "claude-code", "proj", meta, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reclaimed {
		t.Error("second registration should be reclaimed")
	}
	if first.Name != second.Name {
		t.Errorf("expected same name on reclaim: got %q and %q", first.Name, second.Name)
	}
	if first.ID != second.ID {
		t.Errorf("expected same ID on reclaim: got %q and %q", first.ID, second.ID)
	}
}

// TestRegisterRejectsTagWithMismatchedTuple verifies that a spawn_tag present
// in a prior registration does not cause reclaim when model/harness/project
// differs — a fresh name is allocated instead.
func TestRegisterRejectsTagWithMismatchedTuple(t *testing.T) {
	s, _ := setupTestServer(t)

	meta := map[string]string{"spawn_tag": "test-tag-def456"}
	first, _, err := s.registerAI("opus4.7", "claude-code", "proj", meta, "")
	if err != nil {
		t.Fatal(err)
	}

	// Same tag, different model — should NOT reclaim.
	second, reclaimed, err := s.registerAI("gpt5", "claude-code", "proj", meta, "")
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed {
		t.Error("mismatched tuple should not be reclaimed")
	}
	if first.Name == second.Name {
		t.Error("mismatched tuple should allocate a fresh name, got the same name")
	}
}

// TestRegisterWithoutSpawnTagAlwaysFresh verifies that two registrations
// without a spawn_tag always allocate distinct names.
func TestRegisterWithoutSpawnTagAlwaysFresh(t *testing.T) {
	s, _ := setupTestServer(t)

	first, reclaimed, err := s.registerAI("opus4.7", "claude-code", "proj", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed {
		t.Error("first registration without tag should not be reclaimed")
	}

	second, reclaimed, err := s.registerAI("opus4.7", "claude-code", "proj", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed {
		t.Error("second registration without tag should not be reclaimed")
	}
	if first.Name == second.Name {
		t.Error("registrations without tag should get distinct names")
	}
}

// TestGetSettingsDefault verifies that GET /settings returns spec defaults when no
// settings file exists: theme="dark", show_system_events=true.
func TestGetSettingsDefault(t *testing.T) {
	_, srv := setupTestServer(t)

	resp, err := http.Get(srv.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var s Settings
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	if s.Theme != "dark" {
		t.Errorf("expected default theme=dark, got %q", s.Theme)
	}
	if s.AgentIDDefault != "" {
		t.Errorf("expected empty agent_id_default, got %q", s.AgentIDDefault)
	}
	if s.ShowSystemEvents == nil || !*s.ShowSystemEvents {
		t.Error("expected default show_system_events=true")
	}
}

// TestGetSettingsAfterPartialPut verifies that fields not included in a PUT still
// return their defaults.
func TestGetSettingsAfterPartialPut(t *testing.T) {
	_, srv := setupTestServer(t)

	// PUT only agent_id_default — theme and show_system_events omitted.
	body := bytes.NewBufferString(`{"agent_id_default":"martin"}`)
	req, _ := http.NewRequest("PUT", srv.URL+"/settings", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /settings: expected 200, got %d", resp.StatusCode)
	}

	resp2, err := http.Get(srv.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var s Settings
	if err := json.NewDecoder(resp2.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	if s.AgentIDDefault != "martin" {
		t.Errorf("expected agent_id_default=martin, got %q", s.AgentIDDefault)
	}
	// Fields not in the PUT body should still return defaults.
	if s.Theme != "dark" {
		t.Errorf("expected default theme=dark after partial PUT, got %q", s.Theme)
	}
	if s.ShowSystemEvents == nil || !*s.ShowSystemEvents {
		t.Error("expected default show_system_events=true after partial PUT")
	}
}

// TestPutAndGetSettings verifies that PUT /settings stores values and GET returns them.
func TestPutAndGetSettings(t *testing.T) {
	_, srv := setupTestServer(t)

	body := bytes.NewBufferString(`{"theme":"light","agent_id_default":"alice","show_system_events":true}`)
	req, _ := http.NewRequest("PUT", srv.URL+"/settings", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /settings: expected 200, got %d", resp.StatusCode)
	}

	resp2, err := http.Get(srv.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var s Settings
	if err := json.NewDecoder(resp2.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	if s.Theme != "light" {
		t.Errorf("expected theme=light, got %q", s.Theme)
	}
	if s.AgentIDDefault != "alice" {
		t.Errorf("expected agent_id_default=alice, got %q", s.AgentIDDefault)
	}
	if s.ShowSystemEvents == nil || !*s.ShowSystemEvents {
		t.Error("expected show_system_events=true")
	}
}

// TestPutSettingsInvalidTheme verifies that PUT /settings with an unknown theme returns 400.
func TestPutSettingsInvalidTheme(t *testing.T) {
	_, srv := setupTestServer(t)

	body := bytes.NewBufferString(`{"theme":"neon"}`)
	req, _ := http.NewRequest("PUT", srv.URL+"/settings", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid theme, got %d", resp.StatusCode)
	}
}

// TestPutMacrosRejectsNonEmptyRooms verifies that PUT /macros with a non-empty rooms map returns 400.
func TestPutMacrosRejectsNonEmptyRooms(t *testing.T) {
	_, srv := setupTestServer(t)

	body := bytes.NewBufferString(`{"macros":{},"rooms":{"general":{"key":"val"}}}`)
	req, _ := http.NewRequest("PUT", srv.URL+"/macros", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-empty rooms, got %d", resp.StatusCode)
	}
}

// TestPutMacrosAcceptsEmptyRooms verifies that PUT /macros with an empty rooms map is accepted.
func TestPutMacrosAcceptsEmptyRooms(t *testing.T) {
	_, srv := setupTestServer(t)

	body := bytes.NewBufferString(`{"macros":{"hi":"hello"},"rooms":{}}`)
	req, _ := http.NewRequest("PUT", srv.URL+"/macros", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for empty rooms, got %d", resp.StatusCode)
	}
}

// writeSchemaFixture writes a valid schema.json so newStore doesn't early-exit.
func writeSchemaFixture(t *testing.T, dir string) {
	t.Helper()
	schema := []byte(fmt.Sprintf(`{"version":%d}`, storeSchemaVersion))
	if err := os.WriteFile(filepath.Join(dir, "schema.json"), schema, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMacrosMigrationOnLoad verifies that per-room macros from a v3 macros.json
// are merged into globals on store load, and rooms disappear from the envelope.
func TestMacrosMigrationOnLoad(t *testing.T) {
	dir := t.TempDir()
	writeSchemaFixture(t, dir)

	// Write a v3 fixture: one global macro and one room macro with a unique key.
	fixture := `{"macros":{"global-key":"global-val"},"rooms":{"general":{"room-key":"room-val"}}}`
	if err := os.WriteFile(filepath.Join(dir, "macros.json"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	env := s.getEnvelope()
	if env.Macros["global-key"] != "global-val" {
		t.Errorf("global-key not preserved: %v", env.Macros)
	}
	if env.Macros["room-key"] != "room-val" {
		t.Errorf("room-key not migrated into globals: %v", env.Macros)
	}
	if len(env.Rooms) != 0 {
		t.Errorf("rooms should be gone after migration: %v", env.Rooms)
	}
}

// TestMacrosMigrationSkipsCollisions verifies collision handling:
// - global wins over room on key conflict (skippedGlobal path)
// - first-room wins over second-room on inter-room collision (skippedClaimed path)
// - non-colliding keys from both rooms migrate successfully
func TestMacrosMigrationSkipsCollisions(t *testing.T) {
	dir := t.TempDir()
	writeSchemaFixture(t, dir)

	// "clash-global" is in both globals and room "aaa" → global wins.
	// "clash-rooms" is in both "aaa" and "bbb" → "aaa" wins (sorted first).
	// "unique-a" and "unique-b" have no conflicts → both migrate.
	fixture := `{"macros":{"clash-global":"global-wins"},` +
		`"rooms":{` +
		`"aaa":{"clash-global":"room-loses","clash-rooms":"aaa-wins","unique-a":"val-a"},` +
		`"bbb":{"clash-rooms":"bbb-loses","unique-b":"val-b"}` +
		`}}`
	if err := os.WriteFile(filepath.Join(dir, "macros.json"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	env := s.getEnvelope()
	if env.Macros["clash-global"] != "global-wins" {
		t.Errorf("global value should win over room, got %q", env.Macros["clash-global"])
	}
	if env.Macros["clash-rooms"] != "aaa-wins" {
		t.Errorf("first-room (aaa) should win over second-room (bbb), got %q", env.Macros["clash-rooms"])
	}
	if env.Macros["unique-a"] != "val-a" {
		t.Errorf("unique-a should migrate, got %q", env.Macros["unique-a"])
	}
	if env.Macros["unique-b"] != "val-b" {
		t.Errorf("unique-b should migrate, got %q", env.Macros["unique-b"])
	}
	if len(env.Rooms) != 0 {
		t.Errorf("rooms should be gone after migration: %v", env.Rooms)
	}
}

// TestRegisterReclaimedFlagInHTTPResponse verifies that the HTTP register
// endpoint includes reclaimed=true in the JSON response on spawn_tag reclaim.
func TestRegisterReclaimedFlagInHTTPResponse(t *testing.T) {
	_, srv := setupTestServer(t)

	body := func(extra map[string]any) []byte {
		m := map[string]any{
			"kind":    "ai",
			"model":   "opus4.7",
			"harness": "claude-code",
			"project": "proj",
			"meta":    map[string]string{"spawn_tag": "test-tag-http789"},
		}
		for k, v := range extra {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return b
	}

	// First registration.
	resp1, err := http.Post(srv.URL+"/agents", "application/json", bytes.NewReader(body(nil)))
	if err != nil {
		t.Fatal(err)
	}
	var r1 struct {
		Reclaimed bool `json:"reclaimed"`
	}
	if err := json.NewDecoder(resp1.Body).Decode(&r1); err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if r1.Reclaimed {
		t.Error("first registration should return reclaimed=false")
	}

	// Second registration with same spawn_tag — should reclaim.
	resp2, err := http.Post(srv.URL+"/agents", "application/json", bytes.NewReader(body(nil)))
	if err != nil {
		t.Fatal(err)
	}
	var r2 struct {
		Reclaimed bool `json:"reclaimed"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&r2); err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if !r2.Reclaimed {
		t.Error("second registration with same spawn_tag should return reclaimed=true")
	}
}

// ── NeedsHumanAttention round-trip ────────────────────────────────

func TestNeedsHumanAttentionRoundTrip(t *testing.T) {
	s, srv := setupTestServer(t)

	// Register a human and create a room.
	agent, err := s.registerHuman("tester", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}

	// Send a message with needs_attention=true via the explicit field.
	sendBody, _ := json.Marshal(map[string]any{
		"from":            agent.ID,
		"body":            "consensus reached, please review",
		"needs_attention": true,
	})
	resp, err := http.Post(srv.URL+"/rooms/general/send", "application/json", bytes.NewReader(sendBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send returned %d", resp.StatusCode)
	}

	// Fetch via GET /rooms/general/messages and verify the flag.
	resp2, err := http.Get(srv.URL + "/rooms/general/messages")
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Messages []struct {
			Body                string `json:"body"`
			NeedsHumanAttention bool   `json:"needs_human_attention"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	if len(data.Messages) == 0 {
		t.Fatal("expected at least one message")
	}
	if !data.Messages[0].NeedsHumanAttention {
		t.Errorf("expected needs_human_attention=true, got false")
	}

	// Send without needs_attention — flag must be absent/false.
	sendBody2, _ := json.Marshal(map[string]any{
		"from": agent.ID,
		"body": "just a normal message",
	})
	resp3, err := http.Post(srv.URL+"/rooms/general/send", "application/json", bytes.NewReader(sendBody2))
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()

	resp4, err := http.Get(srv.URL + "/rooms/general/messages?limit=1")
	if err != nil {
		t.Fatal(err)
	}
	var data2 struct {
		Messages []struct {
			NeedsHumanAttention bool `json:"needs_human_attention"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp4.Body).Decode(&data2); err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if len(data2.Messages) > 0 && data2.Messages[0].NeedsHumanAttention {
		t.Error("expected needs_human_attention=false for message without needs_attention field")
	}
}

func TestNeedsAttentionForceSubscribesHumans(t *testing.T) {
	s, srv := setupTestServer(t)

	// Register an AI sender and a human who is NOT yet in the room.
	ai, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "sndr")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("humantester", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	// AI joins the room; human does NOT.
	if _, err := s.joinRoom("attention-room", ai.ID); err != nil {
		t.Fatal(err)
	}

	// AI sends with needs_attention=true.
	sendBody, _ := json.Marshal(map[string]any{
		"from":            ai.ID,
		"body":            "please review",
		"needs_attention": true,
	})
	resp, err := http.Post(srv.URL+"/rooms/attention-room/send", "application/json", bytes.NewReader(sendBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send returned %d", resp.StatusCode)
	}

	// Human must now be a member.
	room := s.getRoom("attention-room")
	if room == nil {
		t.Fatal("room not found")
	}
	found := false
	for _, m := range room.Members {
		if m == human.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("human %q not auto-joined to room after needs_attention send", human.ID)
	}
}

func TestNeedsAttentionForceSubscribeIdempotent(t *testing.T) {
	s, srv := setupTestServer(t)

	ai, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "sendtwo")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("humantester2", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.joinRoom("idem-room", ai.ID); err != nil {
		t.Fatal(err)
	}
	// Human is already a member.
	if _, err := s.joinRoom("idem-room", human.ID); err != nil {
		t.Fatal(err)
	}

	// Send with needs_attention=true — should not error or duplicate the member.
	sendBody, _ := json.Marshal(map[string]any{
		"from":            ai.ID,
		"body":            "hey",
		"needs_attention": true,
	})
	resp, err := http.Post(srv.URL+"/rooms/idem-room/send", "application/json", bytes.NewReader(sendBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	room := s.getRoom("idem-room")
	count := 0
	for _, m := range room.Members {
		if m == human.ID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("human appears %d times in members, want exactly 1", count)
	}
}

func TestNeedsAttentionFalseNoForceSubscribe(t *testing.T) {
	s, srv := setupTestServer(t)

	ai, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "sendthree")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("humantester3", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.joinRoom("no-attn-room", ai.ID); err != nil {
		t.Fatal(err)
	}
	_ = human // registered but not in room

	// Send WITHOUT needs_attention — human must NOT be auto-joined.
	sendBody, _ := json.Marshal(map[string]any{
		"from": ai.ID,
		"body": "normal message",
	})
	resp, err := http.Post(srv.URL+"/rooms/no-attn-room/send", "application/json", bytes.NewReader(sendBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	room := s.getRoom("no-attn-room")
	for _, m := range room.Members {
		if m == human.ID {
			t.Errorf("human %q was incorrectly added to room without needs_attention", human.ID)
		}
	}
}

func TestNeedsAttentionWSDelivery(t *testing.T) {
	s, srv := setupTestServer(t)

	// Register a human. The human is NOT joined to any room.
	human, err := s.registerHuman("wshumantester", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Connect human's WebSocket. Send hello but do NOT subscribe to any room.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	hello, _ := json.Marshal(map[string]any{"type": "hello", "agent_id": human.ID})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}

	// Register an AI, join a room, send needs_attention=true.
	ai, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "wsaisender")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("attn-ws-room", ai.ID); err != nil {
		t.Fatal(err)
	}

	sendBody, _ := json.Marshal(map[string]any{
		"from":            ai.ID,
		"body":            "ping humans",
		"needs_attention": true,
	})
	resp, err := http.Post(srv.URL+"/rooms/attn-ws-room/send", "application/json", bytes.NewReader(sendBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Read frames until we receive attention_event or the context times out.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("WS read error before attention_event arrived: %v", err)
		}
		var frame struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		if frame.Type == "attention_event" {
			return // success
		}
	}
}

// ── Sound upload validation ────────────────────────────────────────

func makeMultipartMP3(t *testing.T, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return &buf, w.FormDataContentType()
}

// minimalMP3 is a single valid MPEG1 Layer3 frame header followed by silence.
// Browsers parse this as valid audio. 4-byte header + 413 bytes of zeros = 417 bytes total.
func minimalMP3() []byte {
	data := make([]byte, 417)
	data[0] = 0xFF // MPEG sync
	data[1] = 0xFB // MPEG1, Layer3, 128kbps
	data[2] = 0x90 // 44100Hz, stereo
	data[3] = 0x00 // no copyright, original
	return data
}

func TestSoundUploadValid(t *testing.T) {
	_, srv := setupTestServer(t)

	mp3 := minimalMP3()
	body, ct := makeMultipartMP3(t, "test.mp3", mp3)

	resp, err := http.Post(srv.URL+"/api/sounds", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, b)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["kind"] != "user" {
		t.Errorf("expected kind=user, got %v", result["kind"])
	}
	if result["ext"] != "mp3" {
		t.Errorf("expected ext=mp3 in upload response, got %v", result["ext"])
	}
}

func TestSoundUploadBadExtension(t *testing.T) {
	_, srv := setupTestServer(t)
	body, ct := makeMultipartMP3(t, "sound.ogg", minimalMP3())
	resp, err := http.Post(srv.URL+"/api/sounds", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for bad extension, got %d", resp.StatusCode)
	}
}

// minimalWAV returns the smallest valid RIFF/WAVE header (12 bytes) padded to
// 44 bytes so it looks like a real PCM chunk to lenient parsers.
func minimalWAV() []byte {
	data := make([]byte, 44)
	copy(data[0:4], "RIFF")
	// bytes 4–7: chunk size (little-endian) — arbitrary, we don't validate it
	data[4] = 36
	copy(data[8:12], "WAVE")
	// fmt sub-chunk marker so a real decoder doesn't choke
	copy(data[12:16], "fmt ")
	data[16] = 16 // sub-chunk size = 16
	data[20] = 1  // PCM format
	data[22] = 1  // mono
	data[24] = 0x44
	data[25] = 0xAC // 44100 Hz
	return data
}

func TestSoundUploadWAVValid(t *testing.T) {
	_, srv := setupTestServer(t)

	wav := minimalWAV()
	body, ct := makeMultipartMP3(t, "alert.wav", wav)

	resp, err := http.Post(srv.URL+"/api/sounds", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201 for valid WAV, got %d: %s", resp.StatusCode, b)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["kind"] != "user" {
		t.Errorf("expected kind=user, got %v", result["kind"])
	}
	if result["ext"] != "wav" {
		t.Errorf("expected ext=wav in upload response, got %v", result["ext"])
	}
}

func TestSoundUploadWAVBadHeader(t *testing.T) {
	_, srv := setupTestServer(t)
	garbage := []byte("this is not a wav file at all!!!!")
	body, ct := makeMultipartMP3(t, "fake.wav", garbage)
	resp, err := http.Post(srv.URL+"/api/sounds", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for WAV with bad header, got %d", resp.StatusCode)
	}
}

func TestSoundUploadExtensionMismatch(t *testing.T) {
	_, srv := setupTestServer(t)

	// .mp3 extension but WAV bytes → 400
	body, ct := makeMultipartMP3(t, "sound.mp3", minimalWAV())
	resp, err := http.Post(srv.URL+"/api/sounds", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for .mp3 extension with WAV bytes, got %d", resp.StatusCode)
	}

	// .wav extension but MP3 bytes → 400
	body2, ct2 := makeMultipartMP3(t, "sound.wav", minimalMP3())
	resp2, err := http.Post(srv.URL+"/api/sounds", ct2, body2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for .wav extension with MP3 bytes, got %d", resp2.StatusCode)
	}
}

func TestSoundUploadBadHeader(t *testing.T) {
	_, srv := setupTestServer(t)
	garbage := []byte("this is not an mp3 file at all!!")
	body, ct := makeMultipartMP3(t, "fake.mp3", garbage)
	resp, err := http.Post(srv.URL+"/api/sounds", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for bad header, got %d", resp.StatusCode)
	}
}

func TestSoundUploadCountCap(t *testing.T) {
	s, srv := setupTestServer(t)

	// Pre-fill the sounds index to the cap.
	s.soundsMu.Lock()
	for i := 0; i < maxSoundFiles; i++ {
		s.sounds = append(s.sounds, SoundEntry{
			UUID:       fmt.Sprintf("fake%02d", i),
			Name:       "placeholder",
			Size:       100,
			UploadedAt: now(),
		})
	}
	s.soundsMu.Unlock()

	body, ct := makeMultipartMP3(t, "extra.mp3", minimalMP3())
	resp, err := http.Post(srv.URL+"/api/sounds", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 when at cap, got %d", resp.StatusCode)
	}
}

func TestSoundListAndDelete(t *testing.T) {
	_, srv := setupTestServer(t)

	// Upload one sound.
	body, ct := makeMultipartMP3(t, "beep.mp3", minimalMP3())
	resp, err := http.Post(srv.URL+"/api/sounds", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	var uploaded map[string]any
	json.NewDecoder(resp.Body).Decode(&uploaded)
	resp.Body.Close()
	id := uploaded["id"].(string) // "user:<uuid>"
	uuid := strings.TrimPrefix(id, "user:")

	// List — should contain the uploaded sound.
	resp2, err := http.Get(srv.URL + "/api/sounds")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Sounds []map[string]any `json:"sounds"`
	}
	json.NewDecoder(resp2.Body).Decode(&list)
	resp2.Body.Close()
	found := false
	for _, s := range list.Sounds {
		if s["id"] == id {
			found = true
			if s["ext"] != "mp3" {
				t.Errorf("list entry %q: expected ext=mp3, got %v", id, s["ext"])
			}
		}
	}
	if !found {
		t.Errorf("uploaded sound %q not found in list", id)
	}

	// Delete it.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/sounds/"+uuid, nil)
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204 on delete, got %d", resp3.StatusCode)
	}

	// List again — should be gone.
	resp4, err := http.Get(srv.URL + "/api/sounds")
	if err != nil {
		t.Fatal(err)
	}
	var list2 struct {
		Sounds []map[string]any `json:"sounds"`
	}
	json.NewDecoder(resp4.Body).Decode(&list2)
	resp4.Body.Close()
	for _, s := range list2.Sounds {
		if s["id"] == id {
			t.Errorf("deleted sound %q still in list", id)
		}
	}
}

func TestSoundClearAllWipe(t *testing.T) {
	_, srv := setupTestServer(t)

	// Upload one sound.
	body, ct := makeMultipartMP3(t, "chime.mp3", minimalMP3())
	resp, err := http.Post(srv.URL+"/api/sounds", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload returned %d", resp.StatusCode)
	}

	// DELETE /all?include_settings=true — should wipe sounds.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/all?include_settings=true", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("clear-all returned %d", resp2.StatusCode)
	}

	// List sounds — should have no user sounds.
	resp3, err := http.Get(srv.URL + "/api/sounds")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Sounds []map[string]any `json:"sounds"`
	}
	json.NewDecoder(resp3.Body).Decode(&list)
	resp3.Body.Close()
	for _, s := range list.Sounds {
		if s["kind"] == "user" {
			t.Errorf("user sound %q survived prune -a", s["id"])
		}
	}
}

func TestAttentionUnreadCountInRoomView(t *testing.T) {
	s, srv := setupTestServer(t)

	// Register two agents: watcher (will check the room view) and sender.
	watcher, err := s.registerHuman("watcher", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := s.registerHuman("sender", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Both join two rooms.
	for _, room := range []string{"alpha", "beta"} {
		if _, err := s.joinRoom(room, watcher.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.joinRoom(room, sender.ID); err != nil {
			t.Fatal(err)
		}
	}

	// Sender posts needs_attention=true in alpha, two normal messages in beta.
	for _, tc := range []struct {
		room, body     string
		needsAttention bool
	}{
		{"alpha", "consensus reached", true},
		{"beta", "just a normal message", false},
		{"beta", "another normal message", false},
	} {
		b, _ := json.Marshal(map[string]any{"from": sender.ID, "body": tc.body, "needs_attention": tc.needsAttention})
		resp, err := http.Post(srv.URL+"/rooms/"+tc.room+"/send", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// GET /agents/watcher/rooms — should show attention_unread_count=1 for alpha, 0 for beta.
	resp, err := http.Get(srv.URL + "/agents/" + watcher.ID + "/rooms")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Rooms []struct {
			ID                   string `json:"id"`
			AttentionUnreadCount int    `json:"attention_unread_count"`
		} `json:"rooms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	counts := map[string]int{}
	for _, r := range result.Rooms {
		counts[r.ID] = r.AttentionUnreadCount
	}
	if counts["alpha"] != 1 {
		t.Errorf("alpha attention_unread_count: got %d, want 1", counts["alpha"])
	}
	if counts["beta"] != 0 {
		t.Errorf("beta attention_unread_count: got %d, want 0", counts["beta"])
	}

	// Advance watcher's cursor past the @humans message in alpha — count should drop to 0.
	b, _ := json.Marshal(map[string]any{"room": "alpha", "message_id": 0})
	resp2, err := http.Post(srv.URL+"/agents/"+watcher.ID+"/read", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	resp3, err := http.Get(srv.URL + "/agents/" + watcher.ID + "/rooms")
	if err != nil {
		t.Fatal(err)
	}
	var result2 struct {
		Rooms []struct {
			ID                   string `json:"id"`
			AttentionUnreadCount int    `json:"attention_unread_count"`
		} `json:"rooms"`
	}
	if err := json.NewDecoder(resp3.Body).Decode(&result2); err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()

	for _, r := range result2.Rooms {
		if r.ID == "alpha" && r.AttentionUnreadCount != 0 {
			t.Errorf("alpha attention_unread_count after mark-read: got %d, want 0", r.AttentionUnreadCount)
		}
	}

	// Self-send: watcher sends a needs_attention message to a third room. Their own
	// message must not contribute to their own attention_unread_count.
	if _, err := s.joinRoom("gamma", watcher.ID); err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(map[string]any{"from": watcher.ID, "body": "flagged self-note", "needs_attention": true})
	resp4, err := http.Post(srv.URL+"/rooms/gamma/send", "application/json", bytes.NewReader(b2))
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()

	resp5, err := http.Get(srv.URL + "/agents/" + watcher.ID + "/rooms")
	if err != nil {
		t.Fatal(err)
	}
	var result3 struct {
		Rooms []struct {
			ID                   string `json:"id"`
			AttentionUnreadCount int    `json:"attention_unread_count"`
		} `json:"rooms"`
	}
	if err := json.NewDecoder(resp5.Body).Decode(&result3); err != nil {
		t.Fatal(err)
	}
	resp5.Body.Close()
	for _, r := range result3.Rooms {
		if r.ID == "gamma" && r.AttentionUnreadCount != 0 {
			t.Errorf("gamma attention_unread_count for self-sent needs_attention: got %d, want 0", r.AttentionUnreadCount)
		}
	}
}

func TestLegacyPrefixWarning(t *testing.T) {
	s, srv := setupTestServer(t)

	// Register two AI agents via the store directly.
	agentA, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	agentB, _, err := s.registerAI("opus4.7", "claude-code", "test", nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Both join the room.
	if _, err := s.joinRoom("general", agentA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", agentB.ID); err != nil {
		t.Fatal(err)
	}

	sendAndDecodeWarnings := func(from, body string) []string {
		b, _ := json.Marshal(map[string]string{"from": from, "body": body})
		resp, err := http.Post(srv.URL+"/rooms/general/send", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Warnings []string `json:"warnings"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Warnings
	}

	shortA := agentShortName(agentA.ID)

	// First legacy prefix from agentA → warning returned (lowercase).
	w1 := sendAndDecodeWarnings(agentA.ID, shortA+": here is my analysis")
	if len(w1) == 0 {
		t.Errorf("first legacy prefix: expected a warning, got none")
	}

	// Reset warned state so we can test case-insensitivity end-to-end.
	s.mu.Lock()
	s.warnedLegacy[agentA.ID] = false
	s.mu.Unlock()

	// Mixed-case prefix → same warning (regex is case-insensitive).
	capitalized := strings.ToUpper(shortA[:1]) + shortA[1:]
	w1b := sendAndDecodeWarnings(agentA.ID, capitalized+": mixed case")
	if len(w1b) == 0 {
		t.Errorf("mixed-case legacy prefix: expected a warning, got none")
	}

	// Second legacy prefix from agentA → no warning (already warned).
	w2 := sendAndDecodeWarnings(agentA.ID, shortA+": another one")
	if len(w2) != 0 {
		t.Errorf("second legacy prefix: expected no warning, got %v", w2)
	}

	// First legacy prefix from agentB → its own warning (independent state).
	shortB := agentShortName(agentB.ID)
	w3 := sendAndDecodeWarnings(agentB.ID, shortB+": my turn")
	if len(w3) == 0 {
		t.Errorf("first legacy prefix from agentB: expected a warning, got none")
	}

	// Reset warned state so we can exercise the inline multi-addressee warning.
	s.mu.Lock()
	s.warnedLegacy[agentB.ID] = false
	s.mu.Unlock()

	w3b := sendAndDecodeWarnings(agentB.ID, "Preamble.\n\n"+shortA+", "+shortB+" — your take?")
	if len(w3b) == 0 {
		t.Errorf("inline legacy prefix from agentB: expected a warning, got none")
	}

	// Non-legacy body → no warning.
	w4 := sendAndDecodeWarnings(agentA.ID, "@"+agentShortName(agentB.ID)+" see this")
	if len(w4) != 0 {
		t.Errorf("@-addressed message: unexpected warning %v", w4)
	}

	// False-positive guard: "note:" is not an agent name → no warning.
	w5 := sendAndDecodeWarnings(agentA.ID, "note: this is a plain label")
	if len(w5) != 0 {
		t.Errorf("false positive 'note:': unexpected warning %v", w5)
	}
}

func TestAttentionWarnings(t *testing.T) {
	s, srv := setupTestServer(t)

	sender, _, err := s.registerAI("gpt5", "codex", "test", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	reviewer, _, err := s.registerAI("opus4.7", "claude-code", "test", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("matin", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, member := range []string{sender.ID, reviewer.ID, human.ID} {
		if _, err := s.joinRoom("general", member); err != nil {
			t.Fatal(err)
		}
	}

	sendAndDecodeWarnings := func(payload map[string]any) []string {
		b, _ := json.Marshal(payload)
		resp, err := http.Post(srv.URL+"/rooms/general/send", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Warnings []string `json:"warnings"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Warnings
	}
	resetAttention := func(agentID string) {
		s.mu.Lock()
		s.warnedAttention[agentID] = false
		s.mu.Unlock()
	}

	w1 := sendAndDecodeWarnings(map[string]any{
		"from": sender.ID,
		"body": "@matin please approve this plan",
	})
	if len(w1) != 1 {
		t.Fatalf("first attention miss: got %v, want one warning", w1)
	}
	if !strings.Contains(w1[0], "@matin addressed with a request for action") {
		t.Fatalf("unexpected warning text: %q", w1[0])
	}
	if !strings.Contains(w1[0], "immediately re-send the message with needs_attention=true") {
		t.Fatalf("warning should direct immediate recovery, got %q", w1[0])
	}
	if !strings.Contains(w1[0], "not a carve-out") {
		t.Fatalf("warning should block rationalization, got %q", w1[0])
	}

	w2 := sendAndDecodeWarnings(map[string]any{
		"from": sender.ID,
		"body": "@matin sign off on this",
	})
	if len(w2) != 0 {
		t.Fatalf("second attention miss from same sender: got %v, want none", w2)
	}

	resetAttention(sender.ID)
	w3 := sendAndDecodeWarnings(map[string]any{
		"from":            sender.ID,
		"body":            "@matin please approve this plan",
		"needs_attention": true,
	})
	if len(w3) != 0 {
		t.Fatalf("needs_attention=true should suppress warning, got %v", w3)
	}

	resetAttention(sender.ID)
	w4 := sendAndDecodeWarnings(map[string]any{
		"from": sender.ID,
		"body": "@worker please review the diff",
	})
	if len(w4) != 0 {
		t.Fatalf("AI addressee should not warn, got %v", w4)
	}

	resetAttention(sender.ID)
	w5 := sendAndDecodeWarnings(map[string]any{
		"from": sender.ID,
		"body": "@matin what time is it?",
	})
	if len(w5) != 0 {
		t.Fatalf("question mark alone should not warn, got %v", w5)
	}

	resetAttention(sender.ID)
	w6 := sendAndDecodeWarnings(map[string]any{
		"from": sender.ID,
		"body": "please approve this",
	})
	if len(w6) != 0 {
		t.Fatalf("no addressee should not warn, got %v", w6)
	}

	s.mu.Lock()
	s.warnedAttention[sender.ID] = false
	s.warnedLegacy[sender.ID] = true
	s.mu.Unlock()

	w7 := sendAndDecodeWarnings(map[string]any{
		"from": sender.ID,
		"body": "@matin let me know your call on B",
	})
	if len(w7) != 1 {
		t.Fatalf("legacy warning state should not suppress attention warning, got %v", w7)
	}

	s.mu.Lock()
	s.warnedAttention[reviewer.ID] = false
	s.warnedLegacy[reviewer.ID] = false
	s.mu.Unlock()

	w8 := sendAndDecodeWarnings(map[string]any{
		"from": reviewer.ID,
		"body": "matin: please approve this plan",
	})
	if len(w8) != 2 {
		t.Fatalf("expected both legacy and attention warnings, got %v", w8)
	}
}

func TestAttentionWarningForHumanDM(t *testing.T) {
	s, srv := setupTestServer(t)

	sender, _, err := s.registerAI("gpt5", "codex", "test", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("matin", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"from": sender.ID,
		"to":   human.ID,
		"body": "please approve this plan",
	})
	resp, err := http.Post(srv.URL+"/dm", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Warnings) != 1 {
		t.Fatalf("human DM should warn once, got %v", out.Warnings)
	}
}

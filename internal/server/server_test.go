package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
	if _, err := s.roomSend("race-room", sender.ID, "@rcvrone hello after disconnect"); err != nil {
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
	if _, err := s.roomSend("deliver-room", sender.ID, "hello rcvrtwo"); err != nil {
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

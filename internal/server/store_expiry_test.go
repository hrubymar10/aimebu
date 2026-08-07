package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/types"
)

// fixedNow is a whole-second UTC time used for deterministic boundary tests so
// that RFC3339 round-trip (second precision) doesn't make the edge flaky.
var fixedNow = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func TestExpiryBoundaryAtCutoff(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	window := 6 * time.Hour
	s.mu.Lock()
	s.rooms["r"] = &types.Room{ID: "r", Members: []string{"ai@x"}}
	s.messages["r"] = []types.Message{
		retentionTestMessage(1, "r", fixedNow.Add(-window).Add(time.Second)),  // 1s inside window: live
		retentionTestMessage(2, "r", fixedNow.Add(-window)),                   // exactly cutoff: expired (now-createdAt == window)
		retentionTestMessage(3, "r", fixedNow.Add(-window).Add(-time.Second)), // 1s past cutoff: expired
	}
	s.mu.Unlock()

	msgs, marker := s.roomMessagesWithExpiryAt("r", 0, 0, window, fixedNow)
	if len(msgs) != 1 || msgs[0].ID != 1 {
		t.Fatalf("live msgs = %v, want [1]", msgIDs(msgs))
	}
	if marker == nil {
		t.Fatal("expected expired marker, got nil")
	}
	if marker.HiddenCount != 2 {
		t.Fatalf("hidden_count = %d, want 2", marker.HiddenCount)
	}
	if marker.OldestID != 2 || marker.NewestExpiredID != 3 {
		t.Fatalf("expired ID range = [%d,%d], want [2,3]", marker.OldestID, marker.NewestExpiredID)
	}
	if want := fixedNow.Add(-window).Format(time.RFC3339); marker.ExpiredBefore != want {
		t.Fatalf("expired_before = %q, want %q", marker.ExpiredBefore, want)
	}
}

func TestExpiryMarkerAbsentWhenNothingExpired(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	window := 6 * time.Hour
	s.mu.Lock()
	s.rooms["r"] = &types.Room{ID: "r"}
	s.messages["r"] = []types.Message{
		retentionTestMessage(1, "r", fixedNow.Add(-time.Minute)),
		retentionTestMessage(2, "r", fixedNow),
	}
	s.mu.Unlock()

	msgs, marker := s.roomMessagesWithExpiryAt("r", 0, 0, window, fixedNow)
	if len(msgs) != 2 {
		t.Fatalf("live msgs = %v, want 2", msgIDs(msgs))
	}
	if marker != nil {
		t.Fatalf("expected no marker when nothing expired, got %+v", marker)
	}
}

func TestExpiryWindowZeroMeansNever(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.rooms["r"] = &types.Room{ID: "r"}
	s.messages["r"] = []types.Message{
		retentionTestMessage(1, "r", fixedNow.Add(-365*24*time.Hour)),
		retentionTestMessage(2, "r", fixedNow),
	}
	s.mu.Unlock()

	// window == 0: never expire — no filter, no marker, however old.
	msgs, marker := s.roomMessagesWithExpiryAt("r", 0, 0, 0, fixedNow)
	if len(msgs) != 2 {
		t.Fatalf("live msgs = %v, want 2 (never expire)", msgIDs(msgs))
	}
	if marker != nil {
		t.Fatalf("expected no marker with window==0, got %+v", marker)
	}
}

// TestExpirySettingRescopesImmediatelyNoWrite proves the window is computed at
// read time: changing the setting re-scopes the same room with no message write.
func TestExpirySettingRescopesImmediatelyNoWrite(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h6 := 6 * 60 * 60
	s.putSettings(Settings{MessagesConsideredExpiredAfterSeconds: &h6})
	now := time.Now().UTC()
	s.mu.Lock()
	s.rooms["r"] = &types.Room{ID: "r"}
	s.messages["r"] = []types.Message{
		retentionTestMessage(1, "r", now.Add(-10*time.Hour)), // older than 6h -> expired
		retentionTestMessage(2, "r", now.Add(-time.Minute)),  // live
	}
	s.mu.Unlock()

	// 6h window: message 1 is expired.
	if _, m := s.roomMessagesWithExpiry("r", 0, 0, s.expiredAfterWindow()); m == nil || m.HiddenCount != 1 {
		t.Fatalf("6h window: want hidden_count 1")
	}

	// Change to 0 (never): nothing expired, all visible — no migration, no write.
	zero := 0
	s.putSettings(Settings{MessagesConsideredExpiredAfterSeconds: &zero})
	msgs, m2 := s.roomMessagesWithExpiry("r", 0, 0, s.expiredAfterWindow())
	if len(msgs) != 2 {
		t.Fatalf("never-expire: live msgs = %v, want 2", msgIDs(msgs))
	}
	if m2 != nil {
		t.Fatalf("never-expire: expected no marker, got %+v", m2)
	}

	// Change back to 6h: message 1 is expired again. Re-scope is instant.
	s.putSettings(Settings{MessagesConsideredExpiredAfterSeconds: &h6})
	if _, m3 := s.roomMessagesWithExpiry("r", 0, 0, s.expiredAfterWindow()); m3 == nil || m3.HiddenCount != 1 {
		t.Fatalf("back to 6h: want hidden_count 1 again")
	}
}

func TestExpiryHandlerAIOnlyHumansSeeAll(t *testing.T) {
	s, srv := setupTestServer(t)
	h6 := 6 * 60 * 60
	s.putSettings(Settings{MessagesConsideredExpiredAfterSeconds: &h6})

	ai, _, err := s.registerAI("gpt5", "codex", "test", nil, "alf")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("hank", "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	s.mu.Lock()
	s.rooms["r"] = &types.Room{ID: "r", Members: []string{ai.ID, human.ID}}
	s.messages["r"] = []types.Message{
		retentionTestMessage(1, "r", now.Add(-10*time.Hour)), // expired at 6h
		retentionTestMessage(2, "r", now.Add(-time.Minute)),  // live
	}
	s.mu.Unlock()

	// AI agent: old message hidden, marker present.
	resp := expiryMustGet(t, srv.URL+"/rooms/r/messages?agent_id="+ai.ID)
	var aiOut struct {
		Messages []types.Message `json:"messages"`
		Expired  *ExpiredMarker  `json:"expired"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&aiOut); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(aiOut.Messages) != 1 || aiOut.Messages[0].ID != 2 {
		t.Fatalf("AI saw msgs %v, want only [2]", msgIDs(aiOut.Messages))
	}
	if aiOut.Expired == nil || aiOut.Expired.HiddenCount != 1 {
		t.Fatalf("AI expired marker = %+v, want hidden_count 1", aiOut.Expired)
	}

	// Registered human: sees everything, no marker.
	resp = expiryMustGet(t, srv.URL+"/rooms/r/messages?agent_id="+human.ID)
	var hOut struct {
		Messages []types.Message `json:"messages"`
		Expired  *ExpiredMarker  `json:"expired"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&hOut); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(hOut.Messages) != 2 {
		t.Fatalf("human saw msgs %v, want 2", msgIDs(hOut.Messages))
	}
	if hOut.Expired != nil {
		t.Fatalf("human got marker %+v, want nil", hOut.Expired)
	}

	// No agent_id (web UI): sees everything, no marker.
	resp = expiryMustGet(t, srv.URL+"/rooms/r/messages")
	var rawOut struct {
		Messages []types.Message `json:"messages"`
		Expired  *ExpiredMarker  `json:"expired"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rawOut); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(rawOut.Messages) != 2 {
		t.Fatalf("no-agent saw msgs %v, want 2", msgIDs(rawOut.Messages))
	}
	if rawOut.Expired != nil {
		t.Fatalf("no-agent got marker %+v, want nil", rawOut.Expired)
	}
}

func TestExpiryDoesNotAffectRecallFetchByIDOrWait(t *testing.T) {
	s, srv := setupTestServer(t)
	h6 := 6 * 60 * 60
	memOn := true
	s.putSettings(Settings{MessagesConsideredExpiredAfterSeconds: &h6, MemoryEnabled: &memOn})
	ai, _, err := s.registerAI("gpt5", "codex", "test", nil, "alf")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s.mu.Lock()
	s.rooms["r"] = &types.Room{ID: "r", Members: []string{ai.ID}}
	s.messages["r"] = []types.Message{
		retentionTestMessage(1, "r", now.Add(-10*time.Hour)), // expired for bulk read, but still reachable
	}
	s.mu.Unlock()

	// recall (search) finds the expired message — deliberately unfiltered.
	resp := expiryMustGet(t, srv.URL+"/recall?query=test&agent_id="+ai.ID)
	var rec struct {
		Results []types.RecallResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(rec.Results) == 0 {
		t.Fatal("recall returned no results for an expired message")
	}

	// fetch by ID returns the expired message.
	resp = expiryMustGet(t, srv.URL+"/messages/1?agent_id="+ai.ID)
	var m types.Message
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if m.ID != 1 {
		t.Fatalf("fetch by ID returned id %d, want 1", m.ID)
	}

	// bus_wait path (messagesSince) is unfiltered: the expired message is returned.
	if got := s.messagesSince("r", 0); len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("messagesSince = %v, want [1] (bus_wait must be unaffected)", msgIDs(got))
	}
}

func msgIDs(msgs []types.Message) []int64 {
	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return ids
}

func expiryMustGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", url, resp.StatusCode)
	}
	return resp
}

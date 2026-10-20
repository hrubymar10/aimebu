package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/supervisor"
	"github.com/hrubymar10/aimebu/internal/usages"
)

func (f *fixture) createToolAgent(t *testing.T, roleID string) (store.Agent, string) {
	return f.createToolAgentNamed(t, roleID, "toolagent")
}

func (f *fixture) createToolAgentNamed(t *testing.T, roleID, slug string) (store.Agent, string) {
	t.Helper()
	var agent store.Agent
	err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_tools',?,?,'/tmp/toolbox')`, f.project.ID, f.machineID); err != nil {
			return err
		}
		var err error
		agent, err = tx.CreateAgent(store.Agent{Slug: slug, ProjectID: f.project.ID, RoleID: roleID, Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: f.machineID, CheckoutID: "chk_tools", KeyHash: "placeholder", CreatedBy: "test"})
		if err != nil {
			return err
		}
		return tx.AddMember(f.room.ID, "agent", agent.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := f.srv.Auth.IssueAgentKey(context.Background(), agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.Auth.VerifyAgentKey(t.Context(), agent.ID, key); err != nil {
		t.Fatalf("new agent key is invalid: %v", err)
	}
	if _, err := f.db.AgentToolIdentityByKeyHash(t.Context(), fmt.Sprintf("%x", sha256.Sum256([]byte(key)))); err != nil {
		t.Fatalf("new agent key lookup failed: %v", err)
	}
	return agent, key
}

func (f *fixture) toolRequest(t *testing.T, path, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.http.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (f *fixture) toolPost(t *testing.T, path, key, requestKey string, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, f.http.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", requestKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAgentReactUsesRoomMembershipAndReactionEvent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, key := f.createToolAgent(t, "role_reviewer_default")
	message, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID,
		AuthorKind: "system", AuthorID: "system", Body: "review ready"})
	if err != nil {
		t.Fatal(err)
	}
	otherRoom, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID,
		Kind: "task", Title: "Other task", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	otherMessage, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: otherRoom.ID,
		AuthorKind: "system", AuthorID: "system", Body: "private task"})
	if err != nil {
		t.Fatal(err)
	}
	react := func(method, messageID, emoji string) int {
		t.Helper()
		path := "/api/agent/messages/" + messageID + "/reactions/" + url.PathEscape(emoji)
		req, err := http.NewRequest(method, f.http.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if got := react(http.MethodPut, message.ID, "👍"); got != 204 {
		t.Fatalf("add status=%d", got)
	}
	items, err := f.db.ReactionsForMessage(t.Context(), message.ID)
	if err != nil || len(items) != 1 || items[0]["member_id"] != agent.ID || items[0]["member_kind"] != "agent" {
		t.Fatalf("agent reaction=%v err=%v", items, err)
	}
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.db.EventsAfter(t.Context(), owner.ID, 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "reaction_changed" && event.RoomID == f.room.ID && strings.Contains(event.PayloadJSON, agent.ID) {
			found = true
		}
	}
	if !found {
		t.Fatal("agent reaction did not emit the room reaction event")
	}
	if got := react(http.MethodDelete, message.ID, "👍"); got != 204 {
		t.Fatalf("remove status=%d", got)
	}
	items, err = f.db.ReactionsForMessage(t.Context(), message.ID)
	if err != nil || len(items) != 0 {
		t.Fatalf("reaction after removal=%v err=%v", items, err)
	}
	if got := react(http.MethodPut, otherMessage.ID, "👍"); got != 403 {
		t.Fatalf("non-member status=%d", got)
	}
	if got := react(http.MethodPut, message.ID, "abc"); got != 400 {
		t.Fatalf("invalid emoji status=%d", got)
	}
	if got := react(http.MethodPut, message.ID, "👩‍💻"); got != 204 {
		t.Fatalf("joined emoji status=%d", got)
	}
	longEmoji := "👩🏻‍❤️‍💋‍👨🏼"
	if len(longEmoji) <= 32 {
		t.Fatalf("test emoji has only %d bytes", len(longEmoji))
	}
	if got := react(http.MethodPut, message.ID, longEmoji); got != 204 {
		t.Fatalf("long emoji add status=%d", got)
	}
	_, beforeDuplicate, err := f.db.EventBounds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := react(http.MethodPut, message.ID, longEmoji); got != 204 {
		t.Fatalf("duplicate emoji add status=%d", got)
	}
	_, afterDuplicate, err := f.db.EventBounds(t.Context())
	if err != nil || afterDuplicate != beforeDuplicate {
		t.Fatalf("duplicate PUT emitted event: before=%d after=%d err=%v", beforeDuplicate, afterDuplicate, err)
	}
	if got := react(http.MethodDelete, message.ID, longEmoji); got != 204 {
		t.Fatalf("long emoji remove status=%d", got)
	}
	longUserPath := "/api/messages/" + message.ID + "/reactions/" + url.PathEscape(longEmoji)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		resp := f.request(method, longUserPath, nil, true)
		if resp.StatusCode != 204 {
			t.Fatalf("long user emoji %s status=%d", method, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if err := tx.SetReaction(message.ID, "agent", agent.ID, "legacy-text", true); err != nil {
			return err
		}
		return tx.SetReaction(message.ID, "user", owner.ID, "legacy-text", true)
	}); err != nil {
		t.Fatal(err)
	}
	if got := react(http.MethodDelete, message.ID, "legacy-text"); got != 204 {
		t.Fatalf("remove old agent reaction status=%d", got)
	}
	_, beforeNoop, err := f.db.EventBounds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := react(http.MethodDelete, message.ID, "legacy-text"); got != 204 {
		t.Fatalf("remove absent reaction status=%d", got)
	}
	_, afterNoop, err := f.db.EventBounds(t.Context())
	if err != nil || afterNoop != beforeNoop {
		t.Fatalf("absent DELETE emitted event: before=%d after=%d err=%v", beforeNoop, afterNoop, err)
	}
	if got := react(http.MethodDelete, message.ID, strings.Repeat("x", 129)); got != 400 {
		t.Fatalf("oversized agent DELETE status=%d", got)
	}
	legacyUserPath := "/api/messages/" + message.ID + "/reactions/legacy-text"
	removed := f.request(http.MethodDelete, legacyUserPath, nil, true)
	if removed.StatusCode != 204 {
		t.Fatalf("remove old user reaction status=%d", removed.StatusCode)
	}
	removed.Body.Close()
	tooLong := f.request(http.MethodDelete, "/api/messages/"+message.ID+"/reactions/"+strings.Repeat("x", 129), nil, true)
	if tooLong.StatusCode != 400 {
		t.Fatalf("oversized user DELETE status=%d", tooLong.StatusCode)
	}
	tooLong.Body.Close()
	userPath := "/api/messages/" + message.ID + "/reactions/abc"
	resp := f.request(http.MethodPut, userPath, nil, true)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("user and agent validation differ: %d", resp.StatusCode)
	}
}

func TestAgentToolRoomsAreKeyAuthenticatedAndProjectScoped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, key := f.createToolAgent(t, "role_lead_default")
	privateDM, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, Kind: "dm", DMKey: "private-test", Title: "Private DM", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: privateDM.ID, AuthorKind: "system", AuthorID: "system", Body: "private-dm-secret"}); err != nil {
		t.Fatal(err)
	}
	joinedArchived, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, Kind: "task", Title: "Joined archive", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(t.Context(), joinedArchived.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	leftRoom, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, Kind: "task", Title: "Left room", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(t.Context(), leftRoom.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: joinedArchived.ID, AuthorKind: "system", AuthorID: "system", Body: "joined-archive-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, joinedArchived.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE room_member SET left_at='now' WHERE room_id=? AND member_kind='agent' AND member_id=?`, leftRoom.ID, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	secondResp := f.request(http.MethodPost, "/api/projects", map[string]string{"name": "other", "folder": f.t.TempDir()}, true)
	if secondResp.StatusCode != http.StatusCreated {
		t.Fatalf("second project status=%d", secondResp.StatusCode)
	}
	var second store.Project
	if err := json.NewDecoder(secondResp.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	secondResp.Body.Close()
	secondResp = f.request(http.MethodPost, "/api/projects/"+second.ID+"/rooms", map[string]string{"title": "other task"}, true)
	var otherRoom store.Room
	if err := json.NewDecoder(secondResp.Body).Decode(&otherRoom); err != nil {
		t.Fatal(err)
	}
	secondResp.Body.Close()
	if secondResp.StatusCode != http.StatusCreated {
		t.Fatalf("second room status=%d", secondResp.StatusCode)
	}
	resp := f.toolRequest(t, "/api/agent/rooms", "aak_wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.toolRequest(t, "/api/agent/rooms", key)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Rooms []store.Room `json:"rooms"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(result.Rooms) != 2 {
		t.Fatalf("rooms status=%d result=%+v", resp.StatusCode, result)
	}
	if strings.Contains(string(body), "last_message_body") || strings.Contains(string(body), "private-dm-secret") || strings.Contains(string(body), "joined-archive-secret") {
		t.Fatalf("agent room inventory leaked message preview: %s", body)
	}
	seenArchived := false
	for _, room := range result.Rooms {
		if room.ProjectID != f.project.ID {
			t.Fatalf("cross-project room=%+v", room)
		}
		if room.ID == privateDM.ID {
			t.Fatalf("unjoined DM visible: %+v", room)
		}
		if room.ID == leftRoom.ID {
			t.Fatalf("former membership visible: %+v", room)
		}
		if room.ID == joinedArchived.ID {
			seenArchived = room.Lifecycle == "closed"
		}
	}
	if !seenArchived {
		t.Fatal("joined archived room missing")
	}
	resp = f.toolPost(t, "/api/agent/agents", key, "cross-project", map[string]string{
		"room_id": otherRoom.ID, "role_key": "reviewer", "tool": "fake", "variant": "test", "model": "test",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-project spawn status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	if err := f.db.RevokeAgent(t.Context(), agent.ID); err != nil {
		t.Fatal(err)
	}
	resp = f.toolRequest(t, "/api/agent/rooms", key)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked key status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestWorkerToolCannotSpawnOrStop(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, key := f.createToolAgent(t, "role_worker_default")
	resp := f.toolPost(t, "/api/agent/agents", key, "worker-spawn", map[string]string{
		"room_id": f.room.ID, "role_key": "reviewer", "tool": "fake", "variant": "test", "model": "test",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("worker spawn status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.toolPost(t, "/api/agent/agents/agt_missing/stop", key, "worker-stop", map[string]int{"rev": 0})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("worker stop status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLeadWithoutSpawnCapabilityCanStop(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO role(id,key,label,body,role_key,cap_is_lead,cap_can_spawn)
			VALUES ('role_stop_only','stop-only','Stop only','','stop-only',1,0)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	lead, key := f.createToolAgent(t, "role_stop_only")
	target, _ := f.createToolAgentNamed(t, "role_reviewer_default", "targetagent")
	spawnErr := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.SpawnAgent(store.SpawnInput{RoomID: f.room.ID, ActorKind: "agent", ActorID: lead.ID,
			MachineID: f.machineID, Path: "/tmp/toolbox", RoleKey: "reviewer", Tool: "fake", Variant: "test", Model: "test"})
		return err
	})
	if !errors.Is(spawnErr, store.ErrForbidden) {
		t.Fatalf("stop-only lead spawned: err=%v", spawnErr)
	}
	resp := f.toolPost(t, "/api/agent/agents/"+target.ID+"/stop", key, "stop-only-stop", map[string]int{"rev": 0})
	var stopped store.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&stopped); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || stopped.Desired != "stopped" {
		t.Fatalf("stop-only lead could not stop: status=%d card=%+v", resp.StatusCode, stopped)
	}
}

func TestAgentToolUsageLeftIncludesProviderSnapshot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, key := f.createToolAgent(t, "role_lead_default")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET tool='codex',provider='openai' WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	snapshot := usages.Response{Providers: []usages.Provider{{ProviderName: "codex", Profiles: []usages.Profile{{ProfileName: "local", Active: true, Status: "ok", Windows: []usages.Window{
		{Key: "session", PercentUsed: 75, ResetAt: &reset}, {Key: "weekly", PercentUsed: 40, ResetAt: &reset},
	}}}}}}
	dir := t.TempDir()
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	f.srv.Usages, err = usages.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	resp := f.toolRequest(t, "/api/agent/usage-left", key)
	defer resp.Body.Close()
	var result struct {
		Provider struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Stale   bool   `json:"stale"`
			Session struct {
				PercentLeft float64   `json:"percent_left"`
				ResetAt     time.Time `json:"reset_at"`
			} `json:"session"`
			Weekly struct {
				PercentLeft float64 `json:"percent_left"`
			} `json:"weekly"`
		} `json:"provider"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || result.Provider.Name != "codex" || result.Provider.Status != "stale" || !result.Provider.Stale || result.Provider.Session.PercentLeft != 25 || result.Provider.Weekly.PercentLeft != 60 || !result.Provider.Session.ResetAt.Equal(reset) {
		t.Fatalf("quota status=%d result=%+v", resp.StatusCode, result)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET tool='claude',provider='anthropic' WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot.Providers = append(snapshot.Providers, usages.Provider{ProviderName: "claude-code", Profiles: snapshot.Providers[0].Profiles})
	data, err = json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	f.srv.Usages, err = usages.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	resp = f.toolRequest(t, "/api/agent/usage-left", key)
	var claudeResult struct {
		Provider struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Session struct {
				PercentLeft float64 `json:"percent_left"`
			} `json:"session"`
		} `json:"provider"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&claudeResult); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || claudeResult.Provider.Name != "claude-code" || claudeResult.Provider.Status != "stale" || claudeResult.Provider.Session.PercentLeft != 25 {
		t.Fatalf("claude quota status=%d result=%+v", resp.StatusCode, claudeResult)
	}
}

func TestLeadToolCannotEscalateOrActAfterStop(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgent(t, "role_lead_default")
	other, _ := f.createToolAgentNamed(t, "role_lead_default", "otherlead")
	journal, err := supervisor.OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, adapter.NewFake())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(ctx) }()
	waitFor(t, func() bool { _, err := f.srv.machineForID(f.machineID); return err == nil })
	spawnBody := map[string]string{"room_id": f.room.ID, "role_key": "lead", "tool": "fake", "variant": "test", "model": "test"}
	resp := f.toolPost(t, "/api/agent/agents", key, "escalate", spawnBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("spawn lead status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.toolPost(t, "/api/agent/agents/"+other.ID+"/stop", key, "stop-peer-lead", map[string]int{"rev": 0})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stop another lead status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.toolPost(t, "/api/agent/agents/"+lead.ID+"/stop", key, "self-stop", map[string]int{"rev": 0})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("self stop status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.toolPost(t, "/api/agent/agents", key, "after-stop", map[string]string{"room_id": f.room.ID, "role_key": "reviewer", "tool": "fake", "variant": "test", "model": "test"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stopped lead spawn status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.toolPost(t, "/api/agent/agents/"+other.ID+"/stop", key, "after-stop-control", map[string]int{"rev": 0})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stopped lead stop status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	for _, path := range []string{"/api/agent/rooms", "/api/agent/usage-left"} {
		resp = f.toolRequest(t, path, key)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("stopped agent read %s: status=%d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop")
	}
}

func TestNotAcceptingLeadCannotAct(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgent(t, "role_lead_default")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET desired='not_accepting' WHERE id=?`, lead.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.toolPost(t, "/api/agent/agents", key, "paused-spawn", map[string]string{
		"room_id": f.room.ID, "role_key": "reviewer", "tool": "fake", "variant": "test", "model": "test",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("not-accepting lead spawn status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.toolPost(t, "/api/agent/agents/"+lead.ID+"/stop", key, "paused-stop", map[string]int{"rev": 0})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("not-accepting lead stop status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLeadToolSpawnStopAndUsageHeadroom(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgent(t, "role_lead_default")
	journal, err := supervisor.OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	fake := adapter.NewFake()
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, fake)
	client.OnReady = func() {}
	before, err := f.db.ProjectToolHeadroom(t.Context(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(ctx) }()
	waitFor(t, func() bool { _, err := f.srv.machineForID(f.machineID); return err == nil })
	spawnBody := map[string]string{"room_id": f.room.ID, "role_key": "reviewer", "tool": "fake", "variant": "test", "model": "test"}
	resp := f.toolPost(t, "/api/agent/agents", key, "lead-spawn-1", spawnBody)
	var spawned store.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&spawned); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || spawned.ID == "" || spawned.ProjectID != lead.ProjectID {
		t.Fatalf("spawn status=%d card=%+v", resp.StatusCode, spawned)
	}
	resp = f.toolPost(t, "/api/agent/agents", key, "lead-spawn-1", spawnBody)
	var replay store.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&replay); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || replay.ID != spawned.ID {
		t.Fatalf("spawn replay status=%d card=%+v", resp.StatusCode, replay)
	}
	resp = f.toolRequest(t, "/api/agent/usage-left", key)
	var usage struct {
		Project  store.ProjectToolHeadroom `json:"project"`
		Provider struct {
			Status string `json:"status"`
		} `json:"provider"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&usage); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || usage.Project.LiveAgents != 2 || usage.Project.LiveSlotsLeft != 8 || usage.Project.SpawnsInWindow != before.SpawnsInWindow+1 || usage.Provider.Status != "unknown" {
		t.Fatalf("usage status=%d body=%+v", resp.StatusCode, usage)
	}
	resp = f.toolPost(t, "/api/agent/agents/"+spawned.ID+"/stop", key, "lead-stop-1", map[string]any{"rev": spawned.DesiredRev})
	var stopped store.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&stopped); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || stopped.Desired != "stopped" || stopped.Slug != spawned.Slug || stopped.RoleKey != "reviewer" {
		t.Fatalf("stop status=%d card=%+v", resp.StatusCode, stopped)
	}
	resp = f.toolPost(t, "/api/agent/agents/"+spawned.ID+"/stop", key, "lead-stop-1", map[string]any{"rev": spawned.DesiredRev})
	var stoppedReplay store.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&stoppedReplay); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || stoppedReplay.Desired != "stopped" {
		t.Fatalf("stop replay status=%d card=%+v", resp.StatusCode, stoppedReplay)
	}
	resp.Body.Close()
	resp = f.request(http.MethodPost, "/api/agents/"+spawned.ID+"/control", map[string]any{"desired": "active", "rev": stopped.DesiredRev}, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reactivate status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.toolPost(t, "/api/agent/agents/"+spawned.ID+"/stop", key, "lead-stop-1", map[string]any{"rev": spawned.DesiredRev})
	if err := json.NewDecoder(resp.Body).Decode(&stoppedReplay); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || stoppedReplay.Desired != "stopped" || stoppedReplay.DesiredRev != stopped.DesiredRev {
		t.Fatalf("stop replay after reactivation changed result: status=%d card=%+v", resp.StatusCode, stoppedReplay)
	}
	messages, err := f.db.ListMessagesAfter(t.Context(), f.room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	spawnNotices, stopNotices := 0, 0
	for _, message := range messages {
		if message.AuthorKind != "system" {
			continue
		}
		if message.Body == lead.Slug+" (lead) spawned "+spawned.Slug+" (reviewer)" {
			spawnNotices++
		}
		if message.Body == lead.Slug+" (lead) stopped "+spawned.Slug+" (reviewer)" {
			stopNotices++
		}
	}
	if spawnNotices != 1 || stopNotices != 1 {
		t.Fatalf("system notices spawn=%d stop=%d", spawnNotices, stopNotices)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop")
	}
}

func TestRoomMentionUsersListsAddressableHumans(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	read := func() []string {
		response := f.request("GET", "/api/rooms/"+f.room.ID+"/mention-users", nil, false)
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("mention-users %d", response.StatusCode)
		}
		var body struct {
			Logins []string `json:"logins"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Logins
	}
	// The project's humans are addressable in an ordinary room even without
	// a room_member row.
	if got := read(); len(got) != 1 || got[0] != "owner" {
		t.Fatalf("project humans missing from ordinary room: %v", got)
	}
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(t.Context(), f.room.ID, "user", owner.ID); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) != 1 || got[0] != "owner" {
		t.Fatalf("explicit member listed twice or dropped: %v", got)
	}
	if response := f.request("GET", "/api/rooms/missing/mention-users", nil, false); response.StatusCode == 200 {
		t.Fatal("unknown room served")
	}
}

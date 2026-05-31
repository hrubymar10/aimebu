package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	aimebu "github.com/hrubymar10/aimebu"
	"github.com/hrubymar10/aimebu/internal/types"
)

func setupTestServer(t *testing.T) (*store, *httptest.Server) {
	t.Helper()
	return setupTestServerWithBuild(t, BuildInfo{})
}

func setupTestServerWithBuild(t *testing.T, build BuildInfo) (*store, *httptest.Server) {
	t.Helper()
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	setupHandlers(mux, s, build, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 0x33, G: 0x99, B: 0xcc, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func uploadAttachment(t *testing.T, srv *httptest.Server, filename string, data []byte) (types.Attachment, *http.Response) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/attachments", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		return types.Attachment{}, resp
	}
	defer resp.Body.Close()
	var attachment types.Attachment
	if err := json.NewDecoder(resp.Body).Decode(&attachment); err != nil {
		t.Fatal(err)
	}
	return attachment, resp
}

func findListedAgent(t *testing.T, s *store, agentID string) types.Agent {
	t.Helper()
	for _, agent := range s.listAgents() {
		if agent.ID == agentID {
			return agent
		}
	}
	t.Fatalf("agent %q not found", agentID)
	return types.Agent{}
}

func assertAgentUpdateState(t *testing.T, sub <-chan MetaEvent, agentID, want string) {
	t.Helper()
	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case evt := <-sub:
			if evt.Type != "agent_update" {
				continue
			}
			data, ok := evt.Data.(map[string]any)
			if !ok {
				t.Fatalf("agent_update data = %T, want map[string]any", evt.Data)
			}
			agents, ok := data["agents"].([]types.Agent)
			if !ok {
				t.Fatalf("agent_update agents = %T, want []types.Agent", data["agents"])
			}
			for _, agent := range agents {
				if agent.ID == agentID {
					if agent.State != want {
						t.Fatalf("agent_update state = %q, want %q", agent.State, want)
					}
					return
				}
			}
			t.Fatalf("agent %q not found in agent_update", agentID)
		case <-deadline:
			t.Fatalf("timed out waiting for agent_update state %q", want)
		}
	}
}

func TestAttachmentUploadSendRoundTripAndPersistence(t *testing.T) {
	s, srv := setupTestServer(t)

	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "sender")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}

	uploaded, resp := uploadAttachment(t, srv, "screen.png", testPNG(t, 7, 5))
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("upload returned %d: %s", resp.StatusCode, body)
	}
	if uploaded.Mime != "image/png" || uploaded.Width != 7 || uploaded.Height != 5 || uploaded.Size == 0 {
		t.Fatalf("uploaded metadata = %+v", uploaded)
	}

	sendBody := map[string]any{
		"from": agent.ID,
		"body": "",
		"attachments": []map[string]any{{
			"id":     uploaded.ID,
			"name":   "browser-lie.jpg",
			"mime":   "text/plain",
			"size":   1,
			"width":  99,
			"height": 99,
		}},
	}
	data, _ := json.Marshal(sendBody)
	resp, err = http.Post(srv.URL+"/rooms/general/send", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send returned %d, want 200", resp.StatusCode)
	}

	msgs := s.messagesSince("general", 0)
	got := msgs[len(msgs)-1].Attachments
	if len(got) != 1 {
		t.Fatalf("message attachments = %d, want 1", len(got))
	}
	if got[0].Name != "screen.png" || got[0].Mime != "image/png" || got[0].Width != 7 || got[0].Height != 5 {
		t.Fatalf("message used client metadata, got %+v", got[0])
	}

	reloaded, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	reloadedMsgs := reloaded.messagesSince("general", 0)
	if len(reloadedMsgs) == 0 || len(reloadedMsgs[len(reloadedMsgs)-1].Attachments) != 1 {
		t.Fatalf("reloaded messages missing attachment metadata")
	}
	if _, path, ok := reloaded.attachmentFilePath(uploaded.ID); !ok {
		t.Fatalf("reloaded attachment registry missing %s", uploaded.ID)
	} else if _, err := os.Stat(path); err != nil {
		t.Fatalf("reloaded attachment file missing: %v", err)
	}
}

func TestAttachmentValidationAndCaps(t *testing.T) {
	s, srv := setupTestServer(t)

	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "sender")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}

	_, resp := uploadAttachment(t, srv, "not-an-image.png", []byte("plain text"))
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("text upload returned %d, want 400: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	tooMany := make([]types.Attachment, 5)
	for i := range tooMany {
		tooMany[i] = types.Attachment{ID: fmt.Sprintf("%032x", i+1)}
	}
	if _, err := s.roomSend("general", agent.ID, "too many", false, nil, nil, tooMany); err == nil || !strings.Contains(err.Error(), "too many attachments") {
		t.Fatalf("roomSend too many err = %v, want cap error", err)
	}
}

func TestReactionsRoundTripValidationAndPersistence(t *testing.T) {
	s, srv := setupTestServer(t)

	alice, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.registerHuman("bob", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := s.registerHuman("outsider", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", bob.ID); err != nil {
		t.Fatal(err)
	}
	msgID, err := s.roomSend("general", alice.ID, "ship it", false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(types.ReactRequest{AgentID: bob.ID, Emoji: "👍"})
	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/messages/%d/reactions", srv.URL, msgID), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add reaction returned %d, want 200", resp.StatusCode)
	}

	// Idempotent duplicate add does not increment the count.
	req, _ = http.NewRequest(http.MethodPut, fmt.Sprintf("%s/messages/%d/reactions", srv.URL, msgID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("duplicate add returned %d, want 200", resp.StatusCode)
	}

	resp, err = http.Get(fmt.Sprintf("%s/messages/%d?agent_id=%s", srv.URL, msgID, bob.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var bobView types.Message
	if err := json.NewDecoder(resp.Body).Decode(&bobView); err != nil {
		t.Fatal(err)
	}
	if len(bobView.Reactions) != 1 || bobView.Reactions[0].Emoji != "👍" || bobView.Reactions[0].Count != 1 || !bobView.Reactions[0].Me {
		t.Fatalf("bob reactions = %+v, want one own thumbs-up", bobView.Reactions)
	}
	if !reflect.DeepEqual(bobView.Reactions[0].Agents, []string{bob.ID}) {
		t.Fatalf("bob reaction agents = %+v, want [%s]", bobView.Reactions[0].Agents, bob.ID)
	}

	resp, err = http.Get(fmt.Sprintf("%s/messages/%d?agent_id=%s", srv.URL, msgID, alice.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var aliceView types.Message
	if err := json.NewDecoder(resp.Body).Decode(&aliceView); err != nil {
		t.Fatal(err)
	}
	if len(aliceView.Reactions) != 1 || aliceView.Reactions[0].Me {
		t.Fatalf("alice reactions = %+v, want same count with me=false", aliceView.Reactions)
	}
	if !reflect.DeepEqual(aliceView.Reactions[0].Agents, []string{bob.ID}) {
		t.Fatalf("alice reaction agents = %+v, want [%s]", aliceView.Reactions[0].Agents, bob.ID)
	}

	badBody, _ := json.Marshal(types.ReactRequest{AgentID: bob.ID, Emoji: "ack"})
	req, _ = http.NewRequest(http.MethodPut, fmt.Sprintf("%s/messages/%d/reactions", srv.URL, msgID), bytes.NewReader(badBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("text reaction returned %d, want 400", resp.StatusCode)
	}

	outsiderBody, _ := json.Marshal(types.ReactRequest{AgentID: outsider.ID, Emoji: "👍"})
	req, _ = http.NewRequest(http.MethodPut, fmt.Sprintf("%s/messages/%d/reactions", srv.URL, msgID), bytes.NewReader(outsiderBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("outsider reaction returned %d, want 403", resp.StatusCode)
	}

	reloaded, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	msgs := reloaded.withReactionSummaries(reloaded.messagesSince("general", 0), bob.ID)
	got := msgs[len(msgs)-1].Reactions
	if len(got) != 1 || got[0].Count != 1 || !got[0].Me {
		t.Fatalf("reloaded reactions = %+v, want persisted own reaction", got)
	}

	req, _ = http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/messages/%d/reactions", srv.URL, msgID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove reaction returned %d, want 200", resp.StatusCode)
	}
	var removeResp struct {
		Reactions []types.ReactionSummary `json:"reactions"`
	}
	if err := json.Unmarshal(respBody, &removeResp); err != nil {
		t.Fatal(err)
	}
	if len(removeResp.Reactions) != 0 {
		t.Fatalf("remove response reactions = %+v, want empty", removeResp.Reactions)
	}
	msgs = s.withReactionSummaries(s.messagesSince("general", 0), bob.ID)
	if got := msgs[len(msgs)-1].Reactions; len(got) != 0 {
		t.Fatalf("after remove reactions = %+v, want none", got)
	}
}

func TestReactionMetaIncludesViewerNeutralSummary(t *testing.T) {
	s, srv := setupTestServer(t)

	alice, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.registerHuman("bob", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", bob.ID); err != nil {
		t.Fatal(err)
	}
	msgID, err := s.roomSend("general", alice.ID, "ship it", false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	sub := s.subscribeMeta()
	defer s.unsubscribeMeta(sub)
	reactHTTP(t, srv, http.MethodPut, msgID, bob.ID, "👍")

	deadline := time.After(time.Second)
	for {
		select {
		case evt := <-sub:
			if evt.Type != "reaction" {
				continue
			}
			data, ok := evt.Data.(map[string]any)
			if !ok {
				t.Fatalf("reaction data = %T, want map[string]any", evt.Data)
			}
			reactions, ok := data["reactions"].([]types.ReactionSummary)
			if !ok {
				t.Fatalf("reaction summary = %T, want []types.ReactionSummary", data["reactions"])
			}
			if len(reactions) != 1 {
				t.Fatalf("reaction summary = %+v, want one", reactions)
			}
			if reactions[0].Emoji != "👍" || reactions[0].Count != 1 || reactions[0].Me {
				t.Fatalf("reaction summary = %+v, want viewer-neutral thumbs-up", reactions[0])
			}
			if !reflect.DeepEqual(reactions[0].Agents, []string{bob.ID}) {
				t.Fatalf("reaction agents = %+v, want [%s]", reactions[0].Agents, bob.ID)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for reaction meta event")
		}
	}
}

func TestStorePersistenceReloadRoundTripLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	alice, _, err := s.registerAI("gpt5", "codex", "test", map[string]string{"spawn_tag": "0123456789abcdef"}, "persistalice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.registerHuman("persistbob", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, agentID := range []string{alice.ID, bob.ID} {
		if _, err := s.joinRoom("persist-room", agentID); err != nil {
			t.Fatal(err)
		}
	}
	msgID, err := s.roomSend("persist-room", alice.ID, "@persistbob review this", true, []string{"Approve", "Hold"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.addReaction(msgID, bob.ID, "👍"); err != nil {
		t.Fatal(err)
	}

	reloaded, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if room := reloaded.getRoom("persist-room"); room == nil || len(room.Members) != 2 {
		t.Fatalf("reloaded room = %+v, want two members", room)
	}
	msg, ok := reloaded.messageByID(msgID)
	if !ok {
		t.Fatalf("reloaded message %d not found", msgID)
	}
	if msg.Body != "@persistbob review this" || !msg.NeedsHumanAttention || len(msg.ProposedAnswers) != 2 {
		t.Fatalf("reloaded message = %+v", msg)
	}
	if reactions := reloaded.withReactionSummaries([]types.Message{msg}, bob.ID)[0].Reactions; len(reactions) != 1 || reactions[0].Emoji != "👍" || !reactions[0].Me {
		t.Fatalf("reloaded reactions = %+v, want bob thumbs-up", reactions)
	}
	tmpFiles, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpFiles) != 0 {
		t.Fatalf("leftover temp files = %v, want none", tmpFiles)
	}
}

func TestCleanReactionEmojiAcceptsCompoundEmoji(t *testing.T) {
	for _, emoji := range []string{"👍", "👍🏽", "👩‍💻", "🏳️‍🌈", "👨‍👩‍👧", "🇺🇸"} {
		t.Run(emoji, func(t *testing.T) {
			got, ok := cleanReactionEmoji(emoji)
			if !ok || got != emoji {
				t.Fatalf("cleanReactionEmoji(%q) = %q, %v; want accepted unchanged", emoji, got, ok)
			}
		})
	}
	for _, text := range []string{"ack", "👍👍", "あ"} {
		t.Run(text, func(t *testing.T) {
			if got, ok := cleanReactionEmoji(text); ok {
				t.Fatalf("cleanReactionEmoji(%q) = %q, true; want rejected", text, got)
			}
		})
	}
}

func TestAttachmentCleanupAndPrune(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uploaded, err := s.addAttachment("old.png", "image/png", testPNG(t, 3, 2), 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	s.attachmentsMu.Lock()
	entry := s.attachments[uploaded.ID]
	entry.UploadedAt = time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	s.attachments[uploaded.ID] = entry
	s.persistAttachmentsLocked()
	s.attachmentsMu.Unlock()
	if _, path, ok := s.attachmentFilePath(uploaded.ID); !ok {
		t.Fatal("attachment missing before cleanup")
	} else if _, err := os.Stat(path); err != nil {
		t.Fatalf("attachment file missing before cleanup: %v", err)
	}

	s.cleanupAttachments(time.Now().UTC())
	if _, _, ok := s.attachmentFilePath(uploaded.ID); ok {
		t.Fatal("stale unreferenced attachment still in registry")
	}

	strayID := randomUUID()
	strayPath := filepath.Join(s.attachmentsDir(), strayID+".png")
	if err := os.MkdirAll(s.attachmentsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strayPath, testPNG(t, 1, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-2 * time.Hour)
	if err := os.Chtimes(strayPath, old, old); err != nil {
		t.Fatal(err)
	}
	missingID := randomUUID()
	s.attachmentsMu.Lock()
	s.attachments[missingID] = AttachmentEntry{
		ID:         missingID,
		Name:       "missing.png",
		Mime:       "image/png",
		Size:       12,
		Width:      1,
		Height:     1,
		UploadedAt: old.Format(time.RFC3339),
		Ext:        "png",
	}
	s.persistAttachmentsLocked()
	s.attachmentsMu.Unlock()

	s.cleanupAttachments(time.Now().UTC())
	if _, err := os.Stat(strayPath); !os.IsNotExist(err) {
		t.Fatalf("stale stray file err = %v, want not exist", err)
	}
	if _, _, ok := s.attachmentFilePath(missingID); ok {
		t.Fatal("stale missing-file registry entry survived cleanup")
	}

	kept, err := s.addAttachment("kept.png", "image/png", testPNG(t, 2, 2), 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.attachmentFilePath(kept.ID); !ok {
		t.Fatal("attachment missing before prune")
	}
	s.clearAll(false)
	if _, _, ok := s.attachmentFilePath(kept.ID); ok {
		t.Fatal("attachment registry survived prune")
	}
	if _, err := os.Stat(s.attachmentsDir()); !os.IsNotExist(err) {
		t.Fatalf("attachments dir after prune err = %v, want not exist", err)
	}
}

func TestServerBaseContextCancelsRoomWait(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "waiter")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.createRoom("shutdown-room", agent.ID); err != nil {
		t.Fatal(err)
	}
	s.advanceCursor(agent.ID, "shutdown-room", s.roomHead("shutdown-room"))

	mux := http.NewServeMux()
	setupHandlers(mux, s, BuildInfo{}, nil)

	serverCtx, serverCancel := context.WithCancel(context.Background())
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.BaseContext = serverBaseContext(serverCtx)
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(serverCancel)

	done := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/rooms/shutdown-room/wait?agent_id="+agent.ID+"&timeout=60", nil)
		if err != nil {
			done <- err
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	serverCancel()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("room wait returned after %s, want under 500ms", elapsed)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("room wait did not return after base context cancellation")
	}
}

func hasRoomMember(room *types.Room, agentID string) bool {
	if room == nil {
		return false
	}
	for _, member := range room.Members {
		if member == agentID {
			return true
		}
	}
	return false
}

func TestFrontendManifestServedWithManifestContentType(t *testing.T) {
	registerStaticMimeTypes()

	frontendFS, err := fs.Sub(aimebu.FrontendFS, "frontend")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.FS(frontendFS)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /manifest.webmanifest returned %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/manifest+json") {
		t.Fatalf("Content-Type = %q, want application/manifest+json", got)
	}
}

func TestStaticAssetsHaveNoCacheHeader(t *testing.T) {
	registerStaticMimeTypes()

	frontendFS, err := fs.Sub(aimebu.FrontendFS, "frontend")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /", frontendFileServer(frontendFS))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, path := range []string{"/", "/app.js"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s returned %d, want %d", path, resp.StatusCode, http.StatusOK)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("GET %s Cache-Control = %q, want no-cache", path, got)
		}
		if got := resp.Header.Get("ETag"); !strings.HasPrefix(got, `"fe-`) {
			t.Fatalf("GET %s ETag = %q, want frontend content hash", path, got)
		}
	}

	resp, err := http.Get(srv.URL + "/icons/aimebu-192.png")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /icons/aimebu-192.png returned %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("Cache-Control"); got != "" {
		t.Fatalf("icon Cache-Control = %q, want empty", got)
	}
	if got := resp.Header.Get("ETag"); got != "" {
		t.Fatalf("icon ETag = %q, want empty", got)
	}
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

func TestAgentHeartbeatRefreshesLastSeenOnly(t *testing.T) {
	s, srv := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "claude-code", "test", nil, "beacon")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.createRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.roomSend("general", agent.ID, "before", false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.agents[agent.ID].ReadCursors = map[string]int64{"general": 1}
	s.agents[agent.ID].LastSeen = time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	beforeLastSeen := s.agents[agent.ID].LastSeen
	beforeMessages := len(s.messages["general"])
	beforeCursor := s.agents[agent.ID].ReadCursors["general"]
	s.mu.Unlock()

	resp, err := http.Post(srv.URL+"/agents/"+agent.ID+"/heartbeat", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST heartbeat returned %d, want %d", resp.StatusCode, http.StatusOK)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if got := s.agents[agent.ID].LastSeen; got == beforeLastSeen {
		t.Fatal("heartbeat did not refresh last_seen")
	}
	if got := len(s.messages["general"]); got != beforeMessages {
		t.Fatalf("message count = %d, want %d", got, beforeMessages)
	}
	if got := s.agents[agent.ID].ReadCursors["general"]; got != beforeCursor {
		t.Fatalf("read cursor = %d, want %d", got, beforeCursor)
	}
	if got := s.rooms["general"].Members; len(got) != 1 || got[0] != agent.ID {
		t.Fatalf("room members = %v, want [%s]", got, agent.ID)
	}
}

func TestAgentHeartbeatUnknownAgentReturns404(t *testing.T) {
	_, srv := setupTestServer(t)

	resp, err := http.Post(srv.URL+"/agents/nope@aimebu/heartbeat", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST heartbeat returned %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestSetAgentStateKnownStateUpdatesAndBroadcasts(t *testing.T) {
	s, _ := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "stateful")
	if err != nil {
		t.Fatal(err)
	}
	sub := s.subscribeMeta()
	defer s.unsubscribeMeta(sub)

	if !s.setAgentState(agent.ID, types.AgentStateThinking) {
		t.Fatal("setAgentState returned false")
	}

	agents := s.listAgents()
	var got *types.Agent
	for i := range agents {
		if agents[i].ID == agent.ID {
			got = &agents[i]
			break
		}
	}
	if got == nil {
		t.Fatal("agent not found")
	}
	if got.State != types.AgentStateThinking {
		t.Fatalf("state = %q, want %q", got.State, types.AgentStateThinking)
	}
	if got.StateAt.IsZero() {
		t.Fatal("expected state_at to be set")
	}

	select {
	case evt := <-sub:
		if evt.Type != "agent_update" {
			t.Fatalf("event type = %q, want agent_update", evt.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected agent_update broadcast")
	}
}

func TestSetAgentStateAcceptsNewVocabulary(t *testing.T) {
	s, srv := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "stateok")
	if err != nil {
		t.Fatal(err)
	}

	for _, state := range []string{types.AgentStateIdle, types.AgentStateThinking, types.AgentStateToolCall} {
		resp, err := http.Post(srv.URL+"/agents/"+agent.ID+"/state", "application/json", strings.NewReader(fmt.Sprintf(`{"state":%q}`, state)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST state %q returned %d, want %d", state, resp.StatusCode, http.StatusOK)
		}
	}
}

func TestSetAgentStateUnknownStateRejected(t *testing.T) {
	s, srv := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "statebad")
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(srv.URL+"/agents/"+agent.ID+"/state", "application/json", strings.NewReader(`{"state":"confused"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST state returned %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestSetAgentStateRejectsOldVocabulary(t *testing.T) {
	s, srv := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "stateold")
	if err != nil {
		t.Fatal(err)
	}

	for _, state := range []string{"waiting", "working"} {
		resp, err := http.Post(srv.URL+"/agents/"+agent.ID+"/state", "application/json", strings.NewReader(fmt.Sprintf(`{"state":%q}`, state)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST old state %q returned %d, want %d", state, resp.StatusCode, http.StatusBadRequest)
		}
	}
}

func TestSetAgentStateUnknownAgentReturns404(t *testing.T) {
	_, srv := setupTestServer(t)

	resp, err := http.Post(srv.URL+"/agents/nope@aimebu/state", "application/json", strings.NewReader(`{"state":"idle"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST state returned %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestAgentStateDerivesStaleAfterTimeout(t *testing.T) {
	s, _ := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "staleone")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	s.mu.Lock()
	s.agents[agent.ID].LastSeen = now.Add(-2 * time.Minute).Format(time.RFC3339)
	s.mu.Unlock()

	s.deriveAgentStates(now)
	for _, got := range s.listAgents() {
		if got.ID == agent.ID {
			if got.State != types.AgentStateStale {
				t.Fatalf("state = %q, want %q", got.State, types.AgentStateStale)
			}
			return
		}
	}
	t.Fatal("agent not found")
}

func TestAgentStateNoLongerDerivedFromBusTraffic(t *testing.T) {
	s, _ := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "busstate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.createRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.roomSend("general", agent.ID, "hello", false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	for _, got := range s.listAgents() {
		if got.ID == agent.ID {
			if got.State != "" {
				t.Fatalf("state = %q, want empty", got.State)
			}
			return
		}
	}
	t.Fatal("agent not found")
}

func TestAgentEffectiveStateOverridesToIdleDuringBusWait(t *testing.T) {
	s, _ := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "waitidle")
	if err != nil {
		t.Fatal(err)
	}
	if !s.setAgentState(agent.ID, types.AgentStateThinking) {
		t.Fatal("setAgentState returned false")
	}

	s.enterWait(agent.ID, "")
	got := findListedAgent(t, s, agent.ID)
	if got.State != types.AgentStateIdle {
		t.Fatalf("state during bus_wait = %q, want %q", got.State, types.AgentStateIdle)
	}

	s.mu.RLock()
	storedState := s.agents[agent.ID].State
	s.mu.RUnlock()
	if storedState != types.AgentStateThinking {
		t.Fatalf("stored state = %q, want %q", storedState, types.AgentStateThinking)
	}

	s.leaveWait(agent.ID, "")
	got = findListedAgent(t, s, agent.ID)
	if got.State != types.AgentStateThinking {
		t.Fatalf("state after bus_wait = %q, want %q", got.State, types.AgentStateThinking)
	}
}

func TestAgentEffectiveStateBroadcastsOnBusWaitTransitions(t *testing.T) {
	s, _ := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "waitcast")
	if err != nil {
		t.Fatal(err)
	}
	if !s.setAgentState(agent.ID, types.AgentStateToolCall) {
		t.Fatal("setAgentState returned false")
	}
	sub := s.subscribeMeta()
	defer s.unsubscribeMeta(sub)

	s.enterWait(agent.ID, "")
	assertAgentUpdateState(t, sub, agent.ID, types.AgentStateIdle)

	s.leaveWait(agent.ID, "")
	assertAgentUpdateState(t, sub, agent.ID, types.AgentStateToolCall)
}

func TestAgentEffectiveStateNoBusWaitReturnsStoredState(t *testing.T) {
	s, _ := setupTestServer(t)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "nowait")
	if err != nil {
		t.Fatal(err)
	}
	if !s.setAgentState(agent.ID, types.AgentStateToolCall) {
		t.Fatal("setAgentState returned false")
	}

	got := findListedAgent(t, s, agent.ID)
	if got.State != types.AgentStateToolCall {
		t.Fatalf("state = %q, want %q", got.State, types.AgentStateToolCall)
	}
}

func TestAgentEffectiveStateHumansUnaffectedByBusWait(t *testing.T) {
	s, _ := setupTestServer(t)
	human, err := s.registerHuman("matin", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	s.enterWait(human.ID, "")
	defer s.leaveWait(human.ID, "")

	got := findListedAgent(t, s, human.ID)
	if got.State != "" {
		t.Fatalf("human state = %q, want empty", got.State)
	}
	if !got.StateAt.IsZero() {
		t.Fatal("human state_at should be zero")
	}
}

func TestSetAgentStateNoOpForHuman(t *testing.T) {
	s, _ := setupTestServer(t)
	human, err := s.registerHuman("matin", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if !s.setAgentState(human.ID, types.AgentStateRespawning) {
		t.Fatal("setAgentState returned false for existing human agent")
	}

	for _, got := range s.listAgents() {
		if got.ID == human.ID {
			if got.State != "" {
				t.Fatalf("human state = %q, want empty", got.State)
			}
			if !got.StateAt.IsZero() {
				t.Fatal("human state_at should be zero")
			}
			return
		}
	}
	t.Fatal("human agent not found")
}

func TestDeriveAgentStatesSkipsHumans(t *testing.T) {
	s, _ := setupTestServer(t)
	human, err := s.registerHuman("matin", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	s.mu.Lock()
	s.agents[human.ID].LastSeen = now.Add(-2 * time.Minute).Format(time.RFC3339)
	s.mu.Unlock()

	s.deriveAgentStates(now)

	for _, got := range s.listAgents() {
		if got.ID == human.ID {
			if got.State != "" {
				t.Fatalf("human state = %q, want empty", got.State)
			}
			return
		}
	}
	t.Fatal("human agent not found")
}

func TestDeriveAgentStatesClearsExistingHumanState(t *testing.T) {
	s, _ := setupTestServer(t)
	human, err := s.registerHuman("matin", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	s.mu.Lock()
	s.agents[human.ID].State = types.AgentStateThinking
	s.agents[human.ID].StateAt = time.Now().UTC()
	s.mu.Unlock()

	s.deriveAgentStates(time.Now().UTC())

	for _, got := range s.listAgents() {
		if got.ID == human.ID {
			if got.State != "" {
				t.Fatalf("human state = %q, want empty", got.State)
			}
			if !got.StateAt.IsZero() {
				t.Fatal("human state_at should be zero")
			}
			return
		}
	}
	t.Fatal("human agent not found")
}

func TestKickRoomMemberEmitsKickedSystemMessage(t *testing.T) {
	s, srv := setupTestServer(t)

	human, err := s.registerHuman("matin", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.createRoom("general", human.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"agent_id": agent.ID,
		"kicked":   true,
	})
	resp, err := http.Post(srv.URL+"/rooms/general/leave", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /rooms/general/leave returned %d, want %d", resp.StatusCode, http.StatusOK)
	}

	msgs := s.roomMessages("general", 100, 0)
	if len(msgs) == 0 {
		t.Fatal("expected system message after kick")
	}
	first := msgs[0]
	if first.From != "_system" || first.Body != agent.ID+" was kicked" {
		t.Fatalf("newest message = (%q, %q), want (_system, %q)", first.From, first.Body, agent.ID+" was kicked")
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
	if _, err := s.roomSend("race-room", sender.ID, "@rcvrone hello after disconnect", false, nil, nil, nil); err != nil {
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
	if _, err := s.roomSend("deliver-room", sender.ID, "hello rcvrtwo", false, nil, nil, nil); err != nil {
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

type roomWaitResponse struct {
	Messages    []types.Message `json:"messages"`
	Room        string          `json:"room"`
	Status      string          `json:"status"`
	KeepWaiting bool            `json:"keep_waiting"`
}

func TestRoomWaitTimeoutAndSinceIDNoReplay(t *testing.T) {
	s, srv := setupTestServer(t)

	sender, _, err := s.registerAI("gpt5", "codex", "test", nil, "waitsnd")
	if err != nil {
		t.Fatal(err)
	}
	receiver, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "waitrcv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("wait-contract-room", sender.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("wait-contract-room", receiver.ID); err != nil {
		t.Fatal(err)
	}
	msgID, err := s.roomSend("wait-contract-room", sender.ID, "one message", false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(fmt.Sprintf("%s/rooms/wait-contract-room/wait?agent_id=%s&since_id=%d&timeout=1", srv.URL, receiver.ID, msgID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got roomWaitResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Room != "wait-contract-room" || got.Status != "still_waiting" || !got.KeepWaiting || len(got.Messages) != 0 {
		t.Fatalf("wait response = %+v, want timeout without replay", got)
	}
}

type agentWaitResponse struct {
	Messages    []types.Message       `json:"messages"`
	Reactions   []types.ReactionEvent `json:"reactions"`
	Agent       string                `json:"agent"`
	Status      string                `json:"status"`
	KeepWaiting bool                  `json:"keep_waiting"`
}

func waitForAgentHTTP(t *testing.T, srv *httptest.Server, agentID string, timeout int) <-chan agentWaitResponse {
	t.Helper()
	ch := make(chan agentWaitResponse, 1)
	go func() {
		resp, err := http.Get(fmt.Sprintf("%s/agents/%s/wait?timeout=%d", srv.URL, agentID, timeout))
		if err != nil {
			t.Errorf("wait request failed: %v", err)
			ch <- agentWaitResponse{}
			return
		}
		defer resp.Body.Close()
		var got agentWaitResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Errorf("decode wait response: %v", err)
		}
		ch <- got
	}()
	return ch
}

func reactHTTP(t *testing.T, srv *httptest.Server, method string, messageID int64, agentID, emoji string) {
	t.Helper()
	body, err := json.Marshal(types.ReactRequest{AgentID: agentID, Emoji: emoji})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("%s/messages/%d/reactions", srv.URL, messageID), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s reaction returned %d: %s", method, resp.StatusCode, data)
	}
}

func setupReactionWaitRoom(t *testing.T, s *store, roomID string) (*types.Agent, *types.Agent, *types.Agent) {
	t.Helper()
	author, _, err := s.registerAI("gpt5", "codex", "test", nil, "authr")
	if err != nil {
		t.Fatal(err)
	}
	reactor, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "reactr")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.registerAI("gemini2.5", "aider", "test", nil, "otherr")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.createRoom(roomID, author.ID); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []*types.Agent{author, reactor, other} {
		if _, err := s.joinRoom(roomID, agent.ID); err != nil {
			t.Fatal(err)
		}
	}
	return author, reactor, other
}

func advanceAgentCursorsToHead(s *store, agentID string) {
	for _, room := range s.listRooms() {
		if s.isMember(room.ID, agentID) {
			s.advanceCursor(agentID, room.ID, s.roomHead(room.ID))
		}
	}
}

func TestAgentWaitReturnsReactionForMessageAuthor(t *testing.T) {
	s, srv := setupTestServer(t)
	author, reactor, _ := setupReactionWaitRoom(t, s, "reaction-room")

	msgID, err := s.roomSend("reaction-room", author.ID, "ship it", false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	advanceAgentCursorsToHead(s, author.ID)
	cursorBefore := s.ensureRoomCursor(author.ID, "reaction-room")

	waitCh := waitForAgentHTTP(t, srv, author.ID, 5)
	time.Sleep(50 * time.Millisecond)
	reactHTTP(t, srv, http.MethodPut, msgID, reactor.ID, "👍")

	select {
	case got := <-waitCh:
		if len(got.Messages) != 0 {
			t.Fatalf("messages len = %d, want 0", len(got.Messages))
		}
		if len(got.Reactions) != 1 {
			t.Fatalf("reactions = %+v, want one", got.Reactions)
		}
		reaction := got.Reactions[0]
		if reaction.MessageID != msgID || reaction.RoomID != "reaction-room" || reaction.AgentID != reactor.ID || reaction.Emoji != "👍" || reaction.Op != "add" {
			t.Fatalf("reaction = %+v, want add event for message author", reaction)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent wait did not wake for reaction")
	}

	if cursorAfter := s.ensureRoomCursor(author.ID, "reaction-room"); cursorAfter != cursorBefore {
		t.Fatalf("cursor advanced on reaction: was %d, now %d", cursorBefore, cursorAfter)
	}
	msg, ok := s.messageByID(msgID)
	if !ok {
		t.Fatal("message disappeared")
	}
	if msg.NeedsHumanAttention {
		t.Fatal("reaction wakeup set human attention on message")
	}

	replayCh := waitForAgentHTTP(t, srv, author.ID, 1)
	select {
	case got := <-replayCh:
		if !got.KeepWaiting || got.Status != "still_waiting" || len(got.Reactions) != 0 {
			t.Fatalf("replay wait response = %+v, want timeout without replayed reaction", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent wait did not time out after reaction wakeup")
	}
}

func TestReactionIsolationDoesNotCreateMessagesAdvanceCursorOrAttention(t *testing.T) {
	s, srv := setupTestServer(t)
	author, reactor, _ := setupReactionWaitRoom(t, s, "reaction-isolation-room")

	msgID, err := s.roomSend("reaction-isolation-room", author.ID, "ship it", false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	advanceAgentCursorsToHead(s, author.ID)
	cursorBefore := s.ensureRoomCursor(author.ID, "reaction-isolation-room")
	messageCountBefore := len(s.messagesSince("reaction-isolation-room", 0))

	reactHTTP(t, srv, http.MethodPut, msgID, reactor.ID, "👍")

	if got := len(s.messagesSince("reaction-isolation-room", 0)); got != messageCountBefore {
		t.Fatalf("message count after reaction = %d, want %d", got, messageCountBefore)
	}
	if cursorAfter := s.ensureRoomCursor(author.ID, "reaction-isolation-room"); cursorAfter != cursorBefore {
		t.Fatalf("cursor advanced on reaction: was %d, now %d", cursorBefore, cursorAfter)
	}
	msg, ok := s.messageByID(msgID)
	if !ok {
		t.Fatal("message disappeared")
	}
	if msg.NeedsHumanAttention {
		t.Fatal("reaction set human attention on message")
	}
}

func TestAgentWaitIgnoresReactionOnAnotherAuthorsMessage(t *testing.T) {
	s, srv := setupTestServer(t)
	author, reactor, other := setupReactionWaitRoom(t, s, "other-reaction-room")

	msgID, err := s.roomSend("other-reaction-room", other.ID, "not authored by waiter", false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	advanceAgentCursorsToHead(s, author.ID)

	waitCh := waitForAgentHTTP(t, srv, author.ID, 1)
	time.Sleep(50 * time.Millisecond)
	reactHTTP(t, srv, http.MethodPut, msgID, reactor.ID, "👍")

	select {
	case got := <-waitCh:
		if !got.KeepWaiting || got.Status != "still_waiting" || len(got.Reactions) != 0 {
			t.Fatalf("wait response = %+v, want timeout without reactions", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent wait did not time out")
	}
}

func TestAgentWaitIgnoresSelfReaction(t *testing.T) {
	s, srv := setupTestServer(t)
	author, _, _ := setupReactionWaitRoom(t, s, "self-reaction-room")

	msgID, err := s.roomSend("self-reaction-room", author.ID, "my own reaction", false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	advanceAgentCursorsToHead(s, author.ID)

	waitCh := waitForAgentHTTP(t, srv, author.ID, 1)
	time.Sleep(50 * time.Millisecond)
	reactHTTP(t, srv, http.MethodPut, msgID, author.ID, "👍")

	select {
	case got := <-waitCh:
		if !got.KeepWaiting || got.Status != "still_waiting" || len(got.Reactions) != 0 {
			t.Fatalf("wait response = %+v, want timeout without reactions", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent wait did not time out")
	}
}

func TestAgentWaitReturnsReactionRemove(t *testing.T) {
	s, srv := setupTestServer(t)
	author, reactor, _ := setupReactionWaitRoom(t, s, "remove-reaction-room")

	msgID, err := s.roomSend("remove-reaction-room", author.ID, "remove reaction", false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reactHTTP(t, srv, http.MethodPut, msgID, reactor.ID, "👍")
	advanceAgentCursorsToHead(s, author.ID)

	waitCh := waitForAgentHTTP(t, srv, author.ID, 5)
	time.Sleep(50 * time.Millisecond)
	reactHTTP(t, srv, http.MethodDelete, msgID, reactor.ID, "👍")

	select {
	case got := <-waitCh:
		if len(got.Reactions) != 1 {
			t.Fatalf("reactions = %+v, want one", got.Reactions)
		}
		reaction := got.Reactions[0]
		if reaction.MessageID != msgID || reaction.RoomID != "remove-reaction-room" || reaction.AgentID != reactor.ID || reaction.Emoji != "👍" || reaction.Op != "remove" {
			t.Fatalf("reaction = %+v, want remove event for message author", reaction)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent wait did not wake for remove reaction")
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
	if s.DebugButtonEnabled == nil || *s.DebugButtonEnabled {
		t.Error("expected default debug_button_enabled=false")
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
	if s.DebugButtonEnabled == nil || *s.DebugButtonEnabled {
		t.Error("expected default debug_button_enabled=false after partial PUT")
	}
}

// TestPutAndGetSettings verifies that PUT /settings stores values and GET returns them.
func TestPutAndGetSettings(t *testing.T) {
	_, srv := setupTestServer(t)

	for _, theme := range []string{"light", "red-dark", "red-light", "blue-dark", "blue-light", "green-dark", "green-light", "high-contrast-dark", "high-contrast-light"} {
		t.Run(theme, func(t *testing.T) {
			body := bytes.NewBufferString(`{"theme":"` + theme + `","agent_id_default":"alice","show_system_events":true,"debug_button_enabled":true}`)
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
			if s.Theme != theme {
				t.Errorf("expected theme=%s, got %q", theme, s.Theme)
			}
			if s.AgentIDDefault != "alice" {
				t.Errorf("expected agent_id_default=alice, got %q", s.AgentIDDefault)
			}
			if s.ShowSystemEvents == nil || !*s.ShowSystemEvents {
				t.Error("expected show_system_events=true")
			}
			if s.DebugButtonEnabled == nil || !*s.DebugButtonEnabled {
				t.Error("expected debug_button_enabled=true")
			}
		})
	}
}

// TestLegacyRedThemeMigration verifies that the former red theme loads as red-dark.
func TestLegacyRedThemeMigration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"theme":"red"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	if got := s.getSettings().Theme; got != "red-dark" {
		t.Fatalf("expected legacy red theme to migrate to red-dark, got %q", got)
	}
}

// TestLegacyHighContrastCyanMigration verifies that the test-only cyan variant loads as high-contrast-dark.
func TestLegacyHighContrastCyanMigration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"theme":"high-contrast-dark-cyan"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	if got := s.getSettings().Theme; got != "high-contrast-dark" {
		t.Fatalf("expected legacy cyan high-contrast theme to migrate to high-contrast-dark, got %q", got)
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

func TestStaleDefaultMacroMigration(t *testing.T) {
	oldDigests := staleDefaultMacroDigests
	staleDefaultMacroDigests = map[string][]string{
		"do-cr": {macroSHA256("stale default")},
	}
	t.Cleanup(func() {
		staleDefaultMacroDigests = oldDigests
	})

	defaults := defaultMacros()
	if defaults["do-cr"] == "" {
		t.Fatal("missing do-cr default")
	}

	dir := t.TempDir()
	writeSchemaFixture(t, dir)
	fixture := `{"macros":{"do-cr":"stale default"},"seen_defaults":["do-cr"]}`
	if err := os.WriteFile(filepath.Join(dir, "macros.json"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	s.migrateStaleDefaultMacros()
	env := s.getEnvelope()
	if env.Macros["do-cr"] != defaults["do-cr"] {
		t.Fatalf("stale do-cr macro was not migrated")
	}

	firstPersisted, err := os.ReadFile(filepath.Join(dir, "macros.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.migrateStaleDefaultMacros()
	secondPersisted, err := os.ReadFile(filepath.Join(dir, "macros.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstPersisted, secondPersisted) {
		t.Fatal("migration should be idempotent after replacing the stale body")
	}
}

func TestStaleDefaultMacroMigrationPreservesCustomizedMacro(t *testing.T) {
	oldDigests := staleDefaultMacroDigests
	staleDefaultMacroDigests = map[string][]string{
		"do-cr": {macroSHA256("stale default")},
	}
	t.Cleanup(func() {
		staleDefaultMacroDigests = oldDigests
	})

	dir := t.TempDir()
	writeSchemaFixture(t, dir)
	fixture := `{"macros":{"do-cr":"customized body"},"seen_defaults":["do-cr"]}`
	if err := os.WriteFile(filepath.Join(dir, "macros.json"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	s.migrateStaleDefaultMacros()
	env := s.getEnvelope()
	if env.Macros["do-cr"] != "customized body" {
		t.Fatalf("customized macro should be preserved, got %q", env.Macros["do-cr"])
	}
}

func TestStaleDefaultMacroMigrationLeavesCurrentDefaultAlone(t *testing.T) {
	oldDigests := staleDefaultMacroDigests
	staleDefaultMacroDigests = map[string][]string{
		"do-cr": {macroSHA256("stale default")},
	}
	t.Cleanup(func() {
		staleDefaultMacroDigests = oldDigests
	})

	defaults := defaultMacros()
	if defaults["do-cr"] == "" {
		t.Fatal("missing do-cr default")
	}

	dir := t.TempDir()
	writeSchemaFixture(t, dir)
	env := macrosEnvelope{
		Macros:       map[string]string{"do-cr": defaults["do-cr"]},
		SeenDefaults: []string{"do-cr"},
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "macros.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(filepath.Join(dir, "macros.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.migrateStaleDefaultMacros()
	after, err := os.ReadFile(filepath.Join(dir, "macros.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("already-current default should be a no-op")
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

func TestProposedAnswersRoundTripAndCleanup(t *testing.T) {
	s, srv := setupTestServer(t)

	agent, err := s.registerHuman("tester", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}

	sendBody, _ := json.Marshal(map[string]any{
		"from":             agent.ID,
		"body":             "@tester choose",
		"proposed_answers": []string{" Proceed ", "", "Revise", "Hold", "Extra"},
	})
	resp, err := http.Post(srv.URL+"/rooms/general/send", "application/json", bytes.NewReader(sendBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send returned %d", resp.StatusCode)
	}

	resp2, err := http.Get(srv.URL + "/rooms/general/messages?agent_id=" + agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var data struct {
		Messages []struct {
			ProposedAnswers []string `json:"proposed_answers"`
			AddressedToMe   bool     `json:"addressed_to_me"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if len(data.Messages) == 0 {
		t.Fatal("expected at least one message")
	}
	want := []string{"Proceed", "Revise", "Hold", "Extra"}
	if !reflect.DeepEqual(data.Messages[0].ProposedAnswers, want) {
		t.Fatalf("proposed_answers = %#v, want %#v", data.Messages[0].ProposedAnswers, want)
	}
	if !data.Messages[0].AddressedToMe {
		t.Fatal("expected addressed_to_me=true for addressed proposed-answer message")
	}
}

func TestOpenQuestionsRoundTripCleanupAndPersistence(t *testing.T) {
	s, srv := setupTestServer(t)

	agent, err := s.registerHuman("tester", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}

	longText := strings.Repeat("x", maxOpenQuestionTextRunes+10)
	longDescription := strings.Repeat("d", maxOpenQuestionDescriptionRunes+10)
	sendBody, _ := json.Marshal(map[string]any{
		"from": agent.ID,
		"body": "@tester choose",
		"open_questions": []map[string]any{
			{"question": " First? ", "description": " Context for first. ", "options": []string{" A ", "", "B", "C", "D", "E", "F", "G", "H", "I"}},
			{"question": "drop one option", "options": []string{"only"}},
			{"question": "", "options": []string{"A", "B"}},
			{"question": longText, "description": longDescription, "options": []string{longText, "B"}},
			{"question": "trim blank description", "description": "   ", "options": []string{"A", "B"}},
		},
	})
	resp, err := http.Post(srv.URL+"/rooms/general/send", "application/json", bytes.NewReader(sendBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send returned %d", resp.StatusCode)
	}

	resp2, err := http.Get(srv.URL + "/rooms/general/messages?agent_id=" + agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var data struct {
		Messages []struct {
			OpenQuestions []types.OpenQuestion `json:"open_questions"`
			AddressedToMe bool                 `json:"addressed_to_me"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if len(data.Messages) == 0 {
		t.Fatal("expected at least one message")
	}
	wantFirstOptions := []string{"A", "B", "C", "D", "E", "F", "G", "H"}
	got := data.Messages[0].OpenQuestions
	if len(got) != 3 {
		t.Fatalf("open_questions len = %d, want 3: %#v", len(got), got)
	}
	if got[0].Question != "First?" || got[0].Description != "Context for first." || !reflect.DeepEqual(got[0].Options, wantFirstOptions) {
		t.Fatalf("first open question = %#v, want question First? description and options %#v", got[0], wantFirstOptions)
	}
	if len([]rune(got[1].Question)) != maxOpenQuestionTextRunes || len([]rune(got[1].Description)) != maxOpenQuestionDescriptionRunes || len([]rune(got[1].Options[0])) != maxOpenQuestionTextRunes {
		t.Fatalf("long open question was not truncated to %d runes: %#v", maxOpenQuestionTextRunes, got[1])
	}
	if got[2].Description != "" {
		t.Fatalf("blank open question description = %q, want empty", got[2].Description)
	}
	if !data.Messages[0].AddressedToMe {
		t.Fatal("expected addressed_to_me=true for addressed open-question message")
	}

	reloaded, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	msgs := reloaded.roomMessages("general", 10, 0)
	var persisted []types.OpenQuestion
	for _, msg := range msgs {
		if msg.From == agent.ID {
			persisted = msg.OpenQuestions
			break
		}
	}
	if !reflect.DeepEqual(persisted, got) {
		t.Fatalf("persisted open_questions = %#v, want %#v", msgs, got)
	}
}

func TestLegacyMessagesLoadWithoutProposedAnswers(t *testing.T) {
	dir := t.TempDir()
	schema, _ := json.Marshal(map[string]int{"version": storeSchemaVersion})
	if err := os.WriteFile(filepath.Join(dir, "schema.json"), schema, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "messages.json"), []byte(`[{"id":1,"room_id":"general","from":"alice","body":"legacy","created_at":"2026-01-01T00:00:00Z","targets":null}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	msgs := s.roomMessages("general", 10, 0)
	if len(msgs) != 1 {
		t.Fatalf("loaded messages = %d, want 1", len(msgs))
	}
	if len(msgs[0].ProposedAnswers) != 0 {
		t.Fatalf("legacy proposed_answers = %#v, want empty", msgs[0].ProposedAnswers)
	}
	if len(msgs[0].OpenQuestions) != 0 {
		t.Fatalf("legacy open_questions = %#v, want empty", msgs[0].OpenQuestions)
	}
}

func TestReservedAgentNamesRejected(t *testing.T) {
	s, _ := setupTestServer(t)

	humanReserved := []string{"human", "humans", "ai", "ais", "everyone", "all", "here", "channel", "system", "_system"}
	for _, name := range humanReserved {
		if _, err := s.registerHuman(name, "", nil); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("registerHuman(%q) error = %v, want reserved-name rejection", name, err)
		}
	}

	aiReserved := []string{"human", "humans", "ais", "everyone", "all", "here", "channel", "system"}
	for _, name := range aiReserved {
		if _, _, err := s.registerAI("gpt5", "codex", "test", nil, name); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("registerAI(forceName=%q) error = %v, want reserved-name rejection", name, err)
		}
	}

	for _, name := range namePool {
		if isReservedAgentName(name) {
			t.Fatalf("name pool contains reserved token %q", name)
		}
	}
}

func TestRoomScopedGroupMentionsAnnotateMessages(t *testing.T) {
	s, srv := setupTestServer(t)

	sender, _, err := s.registerAI("gpt5", "codex", "test", nil, "sender")
	if err != nil {
		t.Fatal(err)
	}
	reviewer, _, err := s.registerAI("opus4.7", "claude-code", "test", nil, "reviewpal")
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

	sendBody, _ := json.Marshal(map[string]any{
		"from": sender.ID,
		"body": "@humans please review",
	})
	resp, err := http.Post(srv.URL+"/rooms/general/send", "application/json", bytes.NewReader(sendBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send returned %d", resp.StatusCode)
	}

	type annotated struct {
		Body          string   `json:"body"`
		AddressedTo   []string `json:"addressed_to"`
		AddressedToMe bool     `json:"addressed_to_me"`
		ShouldRespond bool     `json:"should_respond"`
	}
	var humanView struct {
		Messages []annotated `json:"messages"`
	}
	resp, err = http.Get(srv.URL + "/rooms/general/messages?agent_id=" + human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&humanView); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(humanView.Messages) == 0 {
		t.Fatal("expected at least one annotated message for human")
	}
	var gotHuman annotated
	foundHuman := false
	for _, msg := range humanView.Messages {
		if msg.Body == "@humans please review" {
			gotHuman = msg
			foundHuman = true
			break
		}
	}
	if !foundHuman {
		t.Fatalf("human view did not contain target message: %+v", humanView.Messages)
	}
	if !reflect.DeepEqual(gotHuman.AddressedTo, []string{"matin"}) {
		t.Fatalf("human addressed_to = %v, want [matin]", gotHuman.AddressedTo)
	}
	if !gotHuman.AddressedToMe || !gotHuman.ShouldRespond {
		t.Fatalf("human flags = addressed_to_me:%v should_respond:%v, want true/true", gotHuman.AddressedToMe, gotHuman.ShouldRespond)
	}

	var reviewerView struct {
		Messages []annotated `json:"messages"`
	}
	resp, err = http.Get(srv.URL + "/rooms/general/messages?agent_id=" + reviewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&reviewerView); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(reviewerView.Messages) == 0 {
		t.Fatal("expected at least one annotated message for reviewer")
	}
	var gotReviewer annotated
	foundReviewer := false
	for _, msg := range reviewerView.Messages {
		if msg.Body == "@humans please review" {
			gotReviewer = msg
			foundReviewer = true
			break
		}
	}
	if !foundReviewer {
		t.Fatalf("reviewer view did not contain target message: %+v", reviewerView.Messages)
	}
	if len(gotReviewer.AddressedTo) != 1 || gotReviewer.AddressedTo[0] != "matin" {
		t.Fatalf("reviewer addressed_to = %v, want [matin]", gotReviewer.AddressedTo)
	}
	if gotReviewer.AddressedToMe || gotReviewer.ShouldRespond {
		t.Fatalf("reviewer flags = addressed_to_me:%v should_respond:%v, want false/false", gotReviewer.AddressedToMe, gotReviewer.ShouldRespond)
	}
}

func TestDMGroupMentionsResolveWithinDMMembers(t *testing.T) {
	s, srv := setupTestServer(t)

	sender, _, err := s.registerAI("gpt5", "codex", "test", nil, "sender")
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
		"body": "@everyone please review",
	})
	resp, err := http.Post(srv.URL+"/dm", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var sendResp struct {
		Room string `json:"room"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sendResp); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var out struct {
		Messages []struct {
			AddressedTo   []string `json:"addressed_to"`
			AddressedToMe bool     `json:"addressed_to_me"`
			ShouldRespond bool     `json:"should_respond"`
		} `json:"messages"`
	}
	resp, err = http.Get(srv.URL + "/rooms/" + sendResp.Room + "/messages?agent_id=" + human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(out.Messages) == 0 {
		t.Fatal("expected annotated DM message")
	}
	got := out.Messages[0]
	if !reflect.DeepEqual(got.AddressedTo, []string{"matin"}) {
		t.Fatalf("dm addressed_to = %v, want [matin]", got.AddressedTo)
	}
	if !got.AddressedToMe || !got.ShouldRespond {
		t.Fatalf("dm flags = addressed_to_me:%v should_respond:%v, want true/true", got.AddressedToMe, got.ShouldRespond)
	}
}

func TestDMBodylessCreatesRoomWithBothMembers(t *testing.T) {
	s, srv := setupTestServer(t)

	alice, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.registerHuman("bob", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"from": alice.ID,
		"to":   bob.ID,
		// body intentionally omitted (zero value)
	})
	resp, err := http.Post(srv.URL+"/dm", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /dm (bodyless) returned %d, want 200", resp.StatusCode)
	}

	var out struct {
		Room string `json:"room"`
		ID   *int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Room == "" {
		t.Fatal("expected room in response")
	}
	if out.ID != nil {
		t.Fatalf("bodyless DM should not return message id, got %d", *out.ID)
	}

	// Both parties must be members of the returned room.
	room := s.getRoom(out.Room)
	if room == nil {
		t.Fatalf("room %q not found in store", out.Room)
	}
	hasMember := func(id string) bool {
		for _, m := range room.Members {
			if m == id {
				return true
			}
		}
		return false
	}
	if !hasMember(alice.ID) {
		t.Errorf("alice not a member of DM room %q", out.Room)
	}
	if !hasMember(bob.ID) {
		t.Errorf("bob not a member of DM room %q", out.Room)
	}

	// No messages should have been emitted.
	msgs := s.roomMessages(out.Room, 100, 0)
	if len(msgs) != 0 {
		t.Fatalf("bodyless DM emitted %d message(s), want 0", len(msgs))
	}
}

func TestDMReopensAfterKick(t *testing.T) {
	s, srv := setupTestServer(t)

	alice, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.registerHuman("bob", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"from": alice.ID,
		"to":   bob.ID,
	})
	resp, err := http.Post(srv.URL+"/dm", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var openResp struct {
		Room string `json:"room"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&openResp); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if openResp.Room == "" {
		t.Fatal("expected room in response")
	}

	if err := s.leaveRoom(openResp.Room, bob.ID); err != nil {
		t.Fatal(err)
	}
	room := s.getRoom(openResp.Room)
	if room == nil {
		t.Fatalf("room %q not found in store", openResp.Room)
	}
	if hasRoomMember(room, bob.ID) {
		t.Fatalf("bob should have been removed from DM room %q", openResp.Room)
	}

	before := len(s.roomMessages(openResp.Room, 100, 0))
	resp, err = http.Post(srv.URL+"/dm", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var reopenResp struct {
		Room string `json:"room"`
		ID   *int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reopenResp); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if reopenResp.Room != openResp.Room {
		t.Fatalf("reopened room = %q, want %q", reopenResp.Room, openResp.Room)
	}
	if reopenResp.ID != nil {
		t.Fatalf("bodyless DM reopen should not return message id, got %d", *reopenResp.ID)
	}

	room = s.getRoom(reopenResp.Room)
	if !hasRoomMember(room, alice.ID) {
		t.Errorf("alice not a member of reopened DM room %q", reopenResp.Room)
	}
	if !hasRoomMember(room, bob.ID) {
		t.Errorf("bob not a member of reopened DM room %q", reopenResp.Room)
	}
	msgs := s.roomMessages(reopenResp.Room, 100, 0)
	if len(msgs) != before+1 {
		t.Fatalf("bodyless DM reopen emitted %d new message(s), want 1", len(msgs)-before)
	}
	first := msgs[0]
	if first.From != "_system" || first.Body != bob.ID+" joined" {
		t.Fatalf("newest message = (%q, %q), want (_system, %q)", first.From, first.Body, bob.ID+" joined")
	}
}

func TestDMBodylessUnregisteredRecipient(t *testing.T) {
	s, srv := setupTestServer(t)

	alice, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"from": alice.ID,
		"to":   "nobody@ghost",
	})
	resp, err := http.Post(srv.URL+"/dm", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /dm with unregistered recipient returned %d, want 403", resp.StatusCode)
	}
}

func TestDMBodyNonEmptyStillSends(t *testing.T) {
	s, srv := setupTestServer(t)

	alice, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.registerHuman("bob", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"from": alice.ID,
		"to":   bob.ID,
		"body": "hello bob",
	})
	resp, err := http.Post(srv.URL+"/dm", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /dm (with body) returned %d, want 200", resp.StatusCode)
	}

	var out struct {
		Room string `json:"room"`
		ID   int64  `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Room == "" {
		t.Fatal("expected room in response")
	}
	if out.ID == 0 {
		t.Fatal("expected message id in response for non-empty body")
	}

	msgs := s.roomMessages(out.Room, 100, 0)
	if len(msgs) == 0 {
		t.Fatal("expected message in room after DM with body")
	}
	if msgs[len(msgs)-1].Body != "hello bob" {
		t.Fatalf("message body = %q, want %q", msgs[len(msgs)-1].Body, "hello bob")
	}
}

func TestHistoricalGroupTargetsFreezeAtSendTime(t *testing.T) {
	s, srv := setupTestServer(t)

	sender, _, err := s.registerAI("gpt5", "codex", "test", nil, "sender")
	if err != nil {
		t.Fatal(err)
	}
	reviewer, _, err := s.registerAI("opus4.7", "claude-code", "test", nil, "reviewpal")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("matin", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	late, err := s.registerHuman("alex", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, member := range []string{sender.ID, reviewer.ID, human.ID} {
		if _, err := s.joinRoom("general", member); err != nil {
			t.Fatal(err)
		}
	}

	sendBody, _ := json.Marshal(map[string]any{
		"from": sender.ID,
		"body": "@channel frozen targets",
	})
	resp, err := http.Post(srv.URL+"/rooms/general/send", "application/json", bytes.NewReader(sendBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if err := s.leaveRoom("general", reviewer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", late.ID); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Messages []struct {
			Body          string   `json:"body"`
			AddressedTo   []string `json:"addressed_to"`
			AddressedToMe bool     `json:"addressed_to_me"`
			ShouldRespond bool     `json:"should_respond"`
		} `json:"messages"`
	}
	resp, err = http.Get(srv.URL + "/rooms/general/messages?agent_id=" + late.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	type annotatedMessageView struct {
		Body          string
		AddressedTo   []string
		AddressedToMe bool
		ShouldRespond bool
	}
	var frozen annotatedMessageView
	found := false
	for _, msg := range out.Messages {
		if msg.Body == "@channel frozen targets" {
			frozen = annotatedMessageView{
				Body:          msg.Body,
				AddressedTo:   append([]string{}, msg.AddressedTo...),
				AddressedToMe: msg.AddressedToMe,
				ShouldRespond: msg.ShouldRespond,
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("late joiner view did not contain target message: %+v", out.Messages)
	}
	if !reflect.DeepEqual(frozen.AddressedTo, []string{"reviewpal", "matin"}) {
		t.Fatalf("frozen addressed_to = %v, want [reviewpal matin]", frozen.AddressedTo)
	}
	if frozen.AddressedToMe || frozen.ShouldRespond {
		t.Fatalf("late joiner flags = addressed_to_me:%v should_respond:%v, want false/false", frozen.AddressedToMe, frozen.ShouldRespond)
	}
}

func TestGetMessageByIDReturnsViewerAnnotatedFields(t *testing.T) {
	s, srv := setupTestServer(t)

	sender, _, err := s.registerAI("gpt5", "codex", "test", nil, "sender")
	if err != nil {
		t.Fatal(err)
	}
	reviewer, _, err := s.registerAI("opus4.7", "claude-code", "test", nil, "reviewpal")
	if err != nil {
		t.Fatal(err)
	}
	observer, _, err := s.registerAI("haiku4.5", "claude-code", "test", nil, "observer")
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

	sendBody, _ := json.Marshal(map[string]any{
		"from": sender.ID,
		"body": "@humans please review",
	})
	resp, err := http.Post(srv.URL+"/rooms/general/send", "application/json", bytes.NewReader(sendBody))
	if err != nil {
		t.Fatal(err)
	}
	var sendResp struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sendResp); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	type annotatedMessage struct {
		ID             int64    `json:"id"`
		RoomID         string   `json:"room_id"`
		Body           string   `json:"body"`
		AddressedTo    []string `json:"addressed_to"`
		AddressedToMe  bool     `json:"addressed_to_me"`
		ShouldRespond  bool     `json:"should_respond"`
		From           string   `json:"from"`
		NeedsAttention bool     `json:"needs_human_attention"`
		CreatedAt      string   `json:"created_at"`
	}

	fetch := func(agent string) annotatedMessage {
		t.Helper()
		resp, err := http.Get(srv.URL + fmt.Sprintf("/messages/%d?agent_id=%s", sendResp.ID, agent))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /messages/{id} for %s returned %d", agent, resp.StatusCode)
		}
		var out annotatedMessage
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	humanView := fetch(human.ID)
	if humanView.ID != sendResp.ID || humanView.RoomID != "general" || humanView.Body != "@humans please review" {
		t.Fatalf("human view core fields = %+v", humanView)
	}
	if !reflect.DeepEqual(humanView.AddressedTo, []string{"matin"}) {
		t.Fatalf("human addressed_to = %v, want [matin]", humanView.AddressedTo)
	}
	if !humanView.AddressedToMe || !humanView.ShouldRespond {
		t.Fatalf("human flags = addressed_to_me:%v should_respond:%v, want true/true", humanView.AddressedToMe, humanView.ShouldRespond)
	}

	reviewerView := fetch(reviewer.ID)
	if !reflect.DeepEqual(reviewerView.AddressedTo, []string{"matin"}) {
		t.Fatalf("reviewer addressed_to = %v, want [matin]", reviewerView.AddressedTo)
	}
	if reviewerView.AddressedToMe || reviewerView.ShouldRespond {
		t.Fatalf("reviewer flags = addressed_to_me:%v should_respond:%v, want false/false", reviewerView.AddressedToMe, reviewerView.ShouldRespond)
	}

	observerView := fetch(observer.ID)
	if !reflect.DeepEqual(observerView.AddressedTo, []string{"matin"}) {
		t.Fatalf("observer addressed_to = %v, want [matin]", observerView.AddressedTo)
	}
	if observerView.AddressedToMe || observerView.ShouldRespond {
		t.Fatalf("observer flags = addressed_to_me:%v should_respond:%v, want false/false", observerView.AddressedToMe, observerView.ShouldRespond)
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
	msgs := s.messagesSince("attention-room", 0)
	if len(msgs) == 0 {
		t.Fatal("no messages in attention-room")
	}
	if !msgs[len(msgs)-1].NeedsHumanAttention {
		t.Fatalf("latest message NeedsHumanAttention=false, want true")
	}
}

func TestNeedsAttentionDoesNotWidenFrozenGroupTargets(t *testing.T) {
	s, srv := setupTestServer(t)

	ai, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "sndr")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("humantester", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.joinRoom("attention-scope-room", ai.ID); err != nil {
		t.Fatal(err)
	}

	sendBody, _ := json.Marshal(map[string]any{
		"from":            ai.ID,
		"body":            "@humans please review",
		"needs_attention": true,
	})
	resp, err := http.Post(srv.URL+"/rooms/attention-scope-room/send", "application/json", bytes.NewReader(sendBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send returned %d", resp.StatusCode)
	}

	room := s.getRoom("attention-scope-room")
	found := false
	for _, member := range room.Members {
		if member == human.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("human %q not auto-joined to room after needs_attention send", human.ID)
	}

	var out struct {
		Messages []struct {
			Body        string   `json:"body"`
			AddressedTo []string `json:"addressed_to"`
		} `json:"messages"`
	}
	resp, err = http.Get(srv.URL + "/rooms/attention-scope-room/messages?agent_id=" + human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	for _, msg := range out.Messages {
		if msg.Body == "@humans please review" {
			if len(msg.AddressedTo) != 0 {
				t.Fatalf("frozen targets widened after auto-join: got %v, want empty", msg.AddressedTo)
			}
			return
		}
	}
	t.Fatalf("did not find group mention message in room history: %+v", out.Messages)
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

func TestWebSocketSubscribedRoomReceivesMessageAndDisconnectsCleanly(t *testing.T) {
	s, srv := setupTestServer(t)

	ai, _, err := s.registerAI("gpt5", "codex", "test", nil, "wssender")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("wshuman", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, agentID := range []string{ai.ID, human.ID} {
		if _, err := s.joinRoom("ws-room", agentID); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}

	hello, _ := json.Marshal(map[string]any{"type": "hello", "agent_id": human.ID})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	subscribe, _ := json.Marshal(map[string]any{"type": "subscribe", "rooms": []string{"ws-room"}})
	if err := conn.Write(ctx, websocket.MessageText, subscribe); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.subMu.Lock()
		roomSubs := len(s.roomSubs["ws-room"])
		s.subMu.Unlock()
		if roomSubs > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.subMu.Lock()
	roomSubs := len(s.roomSubs["ws-room"])
	s.subMu.Unlock()
	if roomSubs == 0 {
		t.Fatal("websocket did not subscribe to ws-room")
	}

	msgID, err := s.roomSend("ws-room", ai.ID, "ws hello", false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("WS read error before message arrived: %v", err)
		}
		var frame struct {
			Type string        `json:"type"`
			Data types.Message `json:"data"`
		}
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		if frame.Type != "message" {
			continue
		}
		if frame.Data.ID != msgID || frame.Data.RoomID != "ws-room" || frame.Data.Body != "ws hello" {
			t.Fatalf("message frame = %+v, want id %d ws hello", frame.Data, msgID)
		}
		break
	}

	if err := conn.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.subMu.Lock()
		roomSubs = len(s.roomSubs["ws-room"])
		s.subMu.Unlock()
		if roomSubs == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.subMu.Lock()
	roomSubs = len(s.roomSubs["ws-room"])
	s.subMu.Unlock()
	t.Fatalf("room subscriptions after websocket close = %d, want 0", roomSubs)
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

	resetAttention(sender.ID)
	w1b := sendAndDecodeWarnings(map[string]any{
		"from": sender.ID,
		"body": "@humans please approve this plan",
	})
	if len(w1b) != 1 {
		t.Fatalf("@humans attention miss: got %v, want one warning", w1b)
	}
	if !strings.Contains(w1b[0], "@matin addressed with a request for action") {
		t.Fatalf("unexpected @humans warning text: %q", w1b[0])
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

	resetAttention(sender.ID)
	w6b := sendAndDecodeWarnings(map[string]any{
		"from": sender.ID,
		"body": "\\@matin please approve this",
	})
	if len(w6b) != 0 {
		t.Fatalf("escaped addressee should not warn, got %v", w6b)
	}

	resetAttention(sender.ID)
	w6c := sendAndDecodeWarnings(map[string]any{
		"from": sender.ID,
		"body": "@matin `please approve`",
	})
	if len(w6c) != 0 {
		t.Fatalf("attention phrase inside inline code should not warn, got %v", w6c)
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

func TestBuildInfoEndpoint(t *testing.T) {
	build := BuildInfo{Version: "test-v1.2.3", GoVersion: "go1.99.0"}
	_, srv := setupTestServerWithBuild(t, build)

	resp, err := http.Get(srv.URL + "/buildinfo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /buildinfo: status %d", resp.StatusCode)
	}

	var got BuildInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Version != build.Version {
		t.Fatalf("version = %q, want %q", got.Version, build.Version)
	}
	if got.GoVersion != build.GoVersion {
		t.Fatalf("go_version = %q, want %q", got.GoVersion, build.GoVersion)
	}
}

// ── Roles HTTP tests ──────────────────────────────────────────────

func setupRolesServer(t *testing.T) (*store, *httptest.Server) {
	t.Helper()
	SetRoleDefaults(map[string]string{
		"leader":        "lead the team",
		"worker":        "do the work",
		"reviewer":      "review it",
		"sec-reviewer":  "review security",
		"test-reviewer": "review tests",
		"ux-reviewer":   "review UX",
	})
	return setupTestServer(t)
}

func TestRolesHTTP_GetReturnsAllCatalogEntries(t *testing.T) {
	_, srv := setupRolesServer(t)

	resp, err := http.Get(srv.URL + "/roles")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /roles: status %d", resp.StatusCode)
	}
	var roles []RoleEntry
	if err := json.NewDecoder(resp.Body).Decode(&roles); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(roles) != 6 {
		t.Fatalf("expected 6 catalog entries, got %d", len(roles))
	}
	for _, r := range roles {
		if r.Key == "leader" && r.Icon == "" {
			t.Fatalf("expected default leader emoji in GET /roles")
		}
	}
}

func TestRolesHTTP_PutOverrideAndGet(t *testing.T) {
	_, srv := setupRolesServer(t)

	body := bytes.NewBufferString(`{"roles":{"leader":{"label":"Lead","description":"runs the room","icon":"👑","body":"new leader body"}}}`)
	req, _ := http.NewRequest("PUT", srv.URL+"/roles", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /roles: status %d", resp.StatusCode)
	}

	resp2, err := http.Get(srv.URL + "/roles")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var roles []RoleEntry
	if err := json.NewDecoder(resp2.Body).Decode(&roles); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, r := range roles {
		if r.Key == "leader" && r.Overridden && r.Body == "new leader body" && r.Label == "leader" && r.Description == "runs the room" && r.Icon == "👑" {
			found = true
		}
	}
	if !found {
		t.Fatalf("PUT override not reflected in GET /roles; got %+v", roles)
	}
}

func TestRolesHTTP_PutInvalidKeyReturns400(t *testing.T) {
	_, srv := setupRolesServer(t)

	body := bytes.NewBufferString(`{"roles":{"BAD KEY":"body"}}`)
	req, _ := http.NewRequest("PUT", srv.URL+"/roles", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad key, got %d", resp.StatusCode)
	}
}

func TestRolesHTTP_DeleteSingleRevertsOverride(t *testing.T) {
	s, srv := setupRolesServer(t)

	agent, _, _ := s.registerAI("gpt5", "codex", "test", nil, "")
	s.joinRoom("testroom", agent.ID)
	s.assignRole("testroom", agent.ID, "leader")

	// Override leader
	body := bytes.NewBufferString(`{"roles":{"leader":"overridden"}}`)
	req, _ := http.NewRequest("PUT", srv.URL+"/roles", body)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Delete override
	req2, _ := http.NewRequest("DELETE", srv.URL+"/roles/leader", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /roles/leader: status %d", resp2.StatusCode)
	}

	// Verify reverted
	resp3, err := http.Get(srv.URL + "/roles")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	var roles []RoleEntry
	json.NewDecoder(resp3.Body).Decode(&roles)
	for _, r := range roles {
		if r.Key == "leader" && r.Overridden {
			t.Fatalf("leader should no longer be overridden after DELETE")
		}
	}

	room := s.getRoom("testroom")
	if room.Roles[agent.ID] != "leader" {
		t.Fatalf("catalog revert should preserve assignment, got %q", room.Roles[agent.ID])
	}
}

func TestRolesHTTP_DeleteAllRejectsWhenAssigned(t *testing.T) {
	s, srv := setupRolesServer(t)

	// Register an agent, join room, assign role
	agent, _, _ := s.registerAI("gpt5", "codex", "test", nil, "")
	s.joinRoom("testroom", agent.ID)
	s.assignRole("testroom", agent.ID, "worker")

	// DELETE /roles without force should return 409
	req, _ := http.NewRequest("DELETE", srv.URL+"/roles", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 conflict, got %d", resp.StatusCode)
	}

	// DELETE /roles?force=true should succeed
	req2, _ := http.NewRequest("DELETE", srv.URL+"/roles?force=true", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /roles?force=true: status %d", resp2.StatusCode)
	}
}

func TestRolesHTTP_PostRoomRole_HappyPath(t *testing.T) {
	s, srv := setupRolesServer(t)

	agent, _, _ := s.registerAI("gpt5", "codex", "test", nil, "")
	s.joinRoom("testroom", agent.ID)

	body := bytes.NewBufferString(`{"agent_id":"` + agent.ID + `","role_key":"worker"}`)
	req, _ := http.NewRequest("POST", srv.URL+"/rooms/testroom/roles", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /rooms/testroom/roles: status %d", resp.StatusCode)
	}

	var room types.Room
	if err := json.NewDecoder(resp.Body).Decode(&room); err != nil {
		t.Fatalf("decode room: %v", err)
	}
	if room.Roles[agent.ID] != "worker" {
		t.Fatalf("expected worker role, got %q", room.Roles[agent.ID])
	}

	roleResp, err := http.Get(srv.URL + "/rooms/testroom/roles/" + agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer roleResp.Body.Close()
	var roleInfo map[string]any
	if err := json.NewDecoder(roleResp.Body).Decode(&roleInfo); err != nil {
		t.Fatalf("decode role info: %v", err)
	}
	if roleInfo["icon"] == "" {
		t.Fatalf("expected role lookup to include icon, got %#v", roleInfo)
	}
}

func TestRolesHTTP_PostRoomRole_RejectsHuman(t *testing.T) {
	s, srv := setupRolesServer(t)

	if _, err := s.registerHuman("matin", "", nil); err != nil {
		t.Fatal(err)
	}
	joinBody := bytes.NewBufferString(`{"agent_id":"matin"}`)
	joinReq, _ := http.NewRequest("POST", srv.URL+"/rooms/testroom/join", joinBody)
	joinReq.Header.Set("Content-Type", "application/json")
	joinResp, err := http.DefaultClient.Do(joinReq)
	if err != nil {
		t.Fatal(err)
	}
	joinResp.Body.Close()

	body := bytes.NewBufferString(`{"agent_id":"matin","role_key":"leader"}`)
	req, _ := http.NewRequest("POST", srv.URL+"/rooms/testroom/roles", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for human role assignment, got %d", resp.StatusCode)
	}
}

func TestRolesHTTP_PostRoomRole_NonMemberReturns400(t *testing.T) {
	s, srv := setupRolesServer(t)

	// Agent not in testroom
	agent, _, _ := s.registerAI("gpt5", "codex", "test", nil, "")
	s.joinRoom("otherroom", agent.ID)

	body := bytes.NewBufferString(`{"agent_id":"` + agent.ID + `","role_key":"worker"}`)
	req, _ := http.NewRequest("POST", srv.URL+"/rooms/testroom/roles", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Room doesn't exist → 404; agent not member → 400 depending on room creation
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected non-200 for non-member agent, got %d", resp.StatusCode)
	}
}

func TestRolesHTTP_PostRoomRole_BadRoomReturns404(t *testing.T) {
	_, srv := setupRolesServer(t)

	body := bytes.NewBufferString(`{"agent_id":"nobody","role_key":"worker"}`)
	req, _ := http.NewRequest("POST", srv.URL+"/rooms/nonexistent/roles", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for bad room, got %d", resp.StatusCode)
	}
}

func TestRolesHTTP_PutConflictOnRemovedAssignedCustomRole(t *testing.T) {
	s, srv := setupRolesServer(t)

	// Add a custom role and assign it
	s.setCustomRole("my-role", "My Role", "", "", "do stuff")
	agent, _, _ := s.registerAI("gpt5", "codex", "test", nil, "")
	s.joinRoom("testroom", agent.ID)
	s.assignRole("testroom", agent.ID, "my-role")

	// PUT that omits my-role should return 409
	body := bytes.NewBufferString(`{"roles":{}}`)
	req, _ := http.NewRequest("PUT", srv.URL+"/roles", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 conflict, got %d", resp.StatusCode)
	}

	// With force=true should succeed
	body2 := bytes.NewBufferString(`{"roles":{}}`)
	req2, _ := http.NewRequest("PUT", srv.URL+"/roles?force=true", body2)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("PUT /roles?force=true: status %d", resp2.StatusCode)
	}
}

package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestSharedMacrosOwnerEditAndVerbatimSend(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	otherCookie, otherCSRF := registerSecondUser(t, f, "second")
	resp := f.request(http.MethodPut, "/api/macros", map[string]string{"greet": "Hello <name>"}, false)
	if resp.StatusCode != 403 {
		t.Fatalf("missing CSRF: %d", resp.StatusCode)
	}
	resp.Body.Close()
	for _, invalid := range []map[string]string{{"UPPER": "x"}, {"x": strings.Repeat("🙂", 4097)}} {
		resp = f.request(http.MethodPut, "/api/macros", invalid, true)
		if resp.StatusCode != 400 {
			t.Fatalf("invalid map: %d", resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp = f.request(http.MethodPut, "/api/macros", map[string]string{"greet": "Hello <name>"}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("owner save: %d", resp.StatusCode)
	}
	resp.Body.Close()
	large := map[string]string{}
	for _, key := range []string{"one", "two", "three", "four", "five"} {
		large[key] = strings.Repeat("x", 16*1024)
	}
	resp = f.request(http.MethodPut, "/api/macros", large, true)
	if resp.StatusCode != 200 {
		t.Fatalf("valid map above generic JSON limit: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodPut, "/api/macros", map[string]string{"greet": "Hello <name>"}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("restore map: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.requestAs(t, otherCookie, otherCSRF, http.MethodGet, "/api/macros", nil)
	var macros map[string]string
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&macros) != nil || macros["greet"] != "Hello <name>" {
		t.Fatalf("shared read: %d %+v", resp.StatusCode, macros)
	}
	resp.Body.Close()
	resp = f.requestAs(t, otherCookie, otherCSRF, http.MethodPut, "/api/macros", map[string]string{"greet": "stolen"})
	if resp.StatusCode != 403 {
		t.Fatalf("nonowner edit: %d", resp.StatusCode)
	}
	resp.Body.Close()
	f.seed()
	_, agentKey := f.createToolAgent(t, "role_worker_default")
	toolReq, _ := http.NewRequest(http.MethodGet, f.http.URL+"/api/agent/macros", nil)
	toolReq.Header.Set("Authorization", "Bearer "+agentKey)
	toolResp, err := http.DefaultClient.Do(toolReq)
	if err != nil {
		t.Fatal(err)
	}
	var toolMacros map[string]string
	if toolResp.StatusCode != 200 || json.NewDecoder(toolResp.Body).Decode(&toolMacros) != nil || toolMacros["greet"] != "Hello <name>" {
		t.Fatalf("agent read: %d %+v", toolResp.StatusCode, toolMacros)
	}
	toolResp.Body.Close()
	resp = f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "literal <greet> and \\<name>"}, true)
	var message store.Message
	if resp.StatusCode != 201 || json.NewDecoder(resp.Body).Decode(&message) != nil || message.Body != "literal <greet> and \\<name>" {
		t.Fatalf("send: %d %+v", resp.StatusCode, message)
	}
	resp.Body.Close()
}

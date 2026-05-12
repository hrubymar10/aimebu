package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func setupPromptsStore(t *testing.T) *store {
	t.Helper()
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	SetPromptDefaults(map[string]string{
		"bus_etiquette":        "default etiquette",
		"tool.bus_register":    "default register desc",
		"agent.bootstrap":      "default bootstrap",
		"error.not_registered": "not registered default",
	})
	return s
}

func TestPrompts_GetDefault(t *testing.T) {
	s := setupPromptsStore(t)
	v := s.getPrompt("bus_etiquette")
	if v != "default etiquette" {
		t.Fatalf("getPrompt returned %q, want default", v)
	}
}

func TestPrompts_SetOverride(t *testing.T) {
	s := setupPromptsStore(t)
	if !s.setPrompt("bus_etiquette", "custom etiquette") {
		t.Fatal("setPrompt returned false for known key")
	}
	v := s.getPrompt("bus_etiquette")
	if v != "custom etiquette" {
		t.Fatalf("getPrompt after set = %q, want custom", v)
	}
}

func TestPrompts_SetUnknownKeyRejected(t *testing.T) {
	s := setupPromptsStore(t)
	if s.setPrompt("nonexistent.key", "value") {
		t.Fatal("setPrompt should return false for unknown key")
	}
}

// TestPrompts_EmptyOverride verifies that an intentionally blank override
// (e.g. erasing initialize.instructions) is stored and served as "" rather
// than falling back to the compiled default. Empty override != revert.
func TestPrompts_EmptyOverride(t *testing.T) {
	s := setupPromptsStore(t)
	if !s.setPrompt("bus_etiquette", "") {
		t.Fatal("setPrompt returned false for empty-string override")
	}
	if v := s.getPrompt("bus_etiquette"); v != "" {
		t.Fatalf("getPrompt after empty-string override = %q, want empty string", v)
	}
}

func TestPrompts_RevertSinglePreservesOthers(t *testing.T) {
	s := setupPromptsStore(t)
	s.setPrompt("bus_etiquette", "custom etiquette")
	s.setPrompt("agent.bootstrap", "custom bootstrap")

	s.deletePrompt("bus_etiquette")

	if v := s.getPrompt("bus_etiquette"); v != "default etiquette" {
		t.Fatalf("after revert, getPrompt = %q, want default", v)
	}
	if v := s.getPrompt("agent.bootstrap"); v != "custom bootstrap" {
		t.Fatalf("other override should survive revert, got %q", v)
	}
}

func TestPrompts_RevertAll(t *testing.T) {
	s := setupPromptsStore(t)
	s.setPrompt("bus_etiquette", "custom1")
	s.setPrompt("agent.bootstrap", "custom2")

	s.deleteAllPrompts()

	if v := s.getPrompt("bus_etiquette"); v != "default etiquette" {
		t.Fatalf("after reset-all, etiquette = %q, want default", v)
	}
	if v := s.getPrompt("agent.bootstrap"); v != "default bootstrap" {
		t.Fatalf("after reset-all, bootstrap = %q, want default", v)
	}
}

// TestGetPrompt_FallsBackToCompiledDefault verifies that getPrompt returns
// the compiled default for any key registered via SetPromptDefaults, even if
// that key was never stored as an override. This is the runtime-fallback
// property; TestListPrompts_ShowsCatalogKeysWithCompiledDefault covers the
// catalog-listing side of the same guarantee.
func TestGetPrompt_FallsBackToCompiledDefault(t *testing.T) {
	s := setupPromptsStore(t)

	// Existing override is preserved.
	s.setPrompt("bus_etiquette", "user custom etiquette")
	if v := s.getPrompt("bus_etiquette"); v != "user custom etiquette" {
		t.Fatalf("existing override should survive, got %q", v)
	}

	// A key with no stored override falls back to its compiled default.
	if v := s.getPrompt("agent.bootstrap"); v != "default bootstrap" {
		t.Fatalf("unset key should return compiled default, got %q", v)
	}
}

// TestListPrompts_ShowsCatalogKeysWithCompiledDefault verifies the upgrade-path
// property: a catalog key with no stored override appears in listPrompts with
// Body == compiled default and Overridden == false. When a new key is added to
// the catalog JSON and its compiled default is registered via SetPromptDefaults,
// it shows up in listings automatically — no migration step required.
func TestListPrompts_ShowsCatalogKeysWithCompiledDefault(t *testing.T) {
	s := setupPromptsStore(t)
	entries := s.listPrompts()
	for _, e := range entries {
		if e.Key == "bus_etiquette" {
			if e.Overridden {
				t.Fatal("bus_etiquette should not be marked overridden")
			}
			if e.Body != "default etiquette" {
				t.Fatalf("Body = %q, want compiled default", e.Body)
			}
			if e.DefaultBody != "default etiquette" {
				t.Fatalf("DefaultBody = %q, want compiled default", e.DefaultBody)
			}
			return
		}
	}
	t.Fatal("bus_etiquette not found in listPrompts")
}

// TestLoadPrompts_DropsStaleKeys verifies that loadPrompts silently drops
// overrides for keys that are no longer in the catalog (removed or renamed in
// a later release). Without this filter, getPrompt would silently serve stale
// values forever, because it has no catalog gate of its own.
func TestLoadPrompts_DropsStaleKeys(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	SetPromptDefaults(map[string]string{
		"bus_etiquette": "default etiquette",
	})
	// Write a prompts.json that contains a "stale.key" no longer in the catalog.
	staleData := []byte(`{"prompts":{"bus_etiquette":"user custom","stale.key":"orphaned value"}}`)
	if err := os.WriteFile(filepath.Join(s.dir, "prompts.json"), staleData, 0600); err != nil {
		t.Fatal(err)
	}
	s.loadPrompts()

	// Known key must survive.
	if v := s.getPrompt("bus_etiquette"); v != "user custom" {
		t.Fatalf("known key override should survive loadPrompts, got %q", v)
	}
	// Stale key must be dropped (returns empty compiled default).
	if v := s.getPrompt("stale.key"); v != "" {
		t.Fatalf("stale key should be dropped by loadPrompts, got %q", v)
	}
}

func TestPrompts_ListOverriddenFlag(t *testing.T) {
	s := setupPromptsStore(t)
	s.setPrompt("bus_etiquette", "custom")

	entries := s.listPrompts()
	for _, e := range entries {
		if e.Key == "bus_etiquette" {
			if !e.Overridden {
				t.Fatal("bus_etiquette should be marked overridden")
			}
			if e.Body != "custom" {
				t.Fatalf("Body = %q, want custom", e.Body)
			}
			if e.DefaultBody != "default etiquette" {
				t.Fatalf("DefaultBody = %q, want default", e.DefaultBody)
			}
			return
		}
	}
	t.Fatal("bus_etiquette not found in listPrompts")
}

func TestPrompts_HTTPRoundtrip(t *testing.T) {
	_, srv := setupTestServer(t)
	SetPromptDefaults(map[string]string{
		"bus_etiquette": "default etiquette",
	})

	// GET all — should return catalog with defaults
	resp, err := http.Get(srv.URL + "/settings/prompts")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET /settings/prompts: err=%v status=%v", err, resp.StatusCode)
	}
	var entries []PromptEntry
	json.NewDecoder(resp.Body).Decode(&entries)
	resp.Body.Close()
	if len(entries) == 0 {
		t.Fatal("expected at least one prompt entry")
	}

	// PUT override
	req, _ := http.NewRequest("PUT", srv.URL+"/settings/prompts/bus_etiquette",
		strings.NewReader(`{"value":"custom etiquette"}`))
	req.Header.Set("Content-Type", "application/json")
	putResp, err := http.DefaultClient.Do(req)
	if err != nil || putResp.StatusCode != 200 {
		t.Fatalf("PUT /settings/prompts/bus_etiquette: err=%v status=%v", err, putResp.StatusCode)
	}
	putResp.Body.Close()

	// GET again — override should be visible
	resp2, _ := http.Get(srv.URL + "/settings/prompts")
	var entries2 []PromptEntry
	json.NewDecoder(resp2.Body).Decode(&entries2)
	resp2.Body.Close()
	for _, e := range entries2 {
		if e.Key == "bus_etiquette" {
			if !e.Overridden || e.Body != "custom etiquette" {
				t.Fatalf("expected overridden=true body='custom etiquette', got %+v", e)
			}
			break
		}
	}

	// DELETE (revert)
	req2, _ := http.NewRequest("DELETE", srv.URL+"/settings/prompts/bus_etiquette", nil)
	delResp, err := http.DefaultClient.Do(req2)
	if err != nil || delResp.StatusCode != 200 {
		t.Fatalf("DELETE /settings/prompts/bus_etiquette: err=%v status=%v", err, delResp.StatusCode)
	}
	delResp.Body.Close()

	// GET once more — should be back to default
	resp3, _ := http.Get(srv.URL + "/settings/prompts")
	var entries3 []PromptEntry
	json.NewDecoder(resp3.Body).Decode(&entries3)
	resp3.Body.Close()
	for _, e := range entries3 {
		if e.Key == "bus_etiquette" && e.Overridden {
			t.Fatal("after revert, bus_etiquette should not be overridden")
		}
	}
}

func TestPrompts_HTTPDeleteAll(t *testing.T) {
	s, srv := setupTestServer(t)
	SetPromptDefaults(map[string]string{"bus_etiquette": "default"})
	s.setPrompt("bus_etiquette", "custom")

	req, _ := http.NewRequest("DELETE", srv.URL+"/settings/prompts", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("DELETE /settings/prompts: err=%v status=%v", err, resp.StatusCode)
	}
	resp.Body.Close()

	if v := s.getPrompt("bus_etiquette"); v != "default" {
		t.Fatalf("after reset-all, got %q want default", v)
	}
}

func TestPrompts_HTTPUnknownKeyReturns404(t *testing.T) {
	_, srv := setupTestServer(t)

	req, _ := http.NewRequest("PUT", srv.URL+"/settings/prompts/no.such.key",
		strings.NewReader(`{"value":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PUT unknown key: status=%d, want 404", resp.StatusCode)
	}
}

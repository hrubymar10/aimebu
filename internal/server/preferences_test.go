package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func prefsOf(t *testing.T, resp interface {
	Close() error
}, decode func(any) error) map[string]string {
	t.Helper()
	var out struct {
		Preferences map[string]string `json:"preferences"`
	}
	if err := decode(&out); err != nil {
		t.Fatal(err)
	}
	_ = resp.Close()
	return out.Preferences
}

func TestUserPreferencesArePerUserAndValidated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cookie, csrf := registerSecondUser(t, f, "second")
	resp := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"theme": "light", "inbox_volume": "30", "markdown_mode": "raw"}}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	got := prefsOf(t, resp.Body, json.NewDecoder(resp.Body).Decode)
	if got["theme"] != "light" || got["inbox_volume"] != "30" || got["markdown_mode"] != "raw" {
		t.Fatalf("put result: %v", got)
	}
	// A later write replaces a value and keeps the others.
	f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"theme": "dark"}}, true).Body.Close()
	resp = f.request("GET", "/api/me/preferences", nil, false)
	got = prefsOf(t, resp.Body, json.NewDecoder(resp.Body).Decode)
	if got["theme"] != "dark" || got["inbox_volume"] != "30" || got["markdown_mode"] != "raw" {
		t.Fatalf("after replace: %v", got)
	}
	// Another user sees none of it.
	other := f.requestAs(t, cookie, csrf, "GET", "/api/me/preferences", nil)
	if p := prefsOf(t, other.Body, json.NewDecoder(other.Body).Decode); len(p) != 0 {
		t.Fatalf("preferences leaked to another user: %v", p)
	}
	for name, body := range map[string]map[string]string{"unknown name": {"nope": "x"}, "long value": {"theme": strings.Repeat("x", 257)}, "empty": {}} {
		resp := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": body}, true)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("%s: %d", name, resp.StatusCode)
		}
	}
	resp = f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"theme": "light"}}, false)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("missing CSRF: %d", resp.StatusCode)
	}
}

func TestTheManualProjectOrderMayBeLongerThanOtherPreferences(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	order := strings.Repeat("prj_01ABCDEFGHJKMNPQRSTVWXYZ00,", 100) // 3000 bytes: 100 project ids
	resp := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"sidebar_sort": "manual", "sidebar_project_order": order}}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("a long project order: %d", resp.StatusCode)
	}
	got := prefsOf(t, resp.Body, json.NewDecoder(resp.Body).Decode)
	if got["sidebar_sort"] != "manual" || got["sidebar_project_order"] != order {
		t.Fatalf("stored: %v", got)
	}
	for name, body := range map[string]map[string]string{
		"an over-long order": {"sidebar_project_order": strings.Repeat("x", 4097)},
		"an over-long sort":  {"sidebar_sort": strings.Repeat("x", 257)},
	} {
		resp := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": body}, true)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("%s: %d", name, resp.StatusCode)
		}
	}
}

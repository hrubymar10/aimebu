package server

import (
	"context"
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

// Skipping the offer to set up a harness is the account's choice, kept on the server: it is stored, changed back to 0 once a harness
// exists, read back on the next sign-in, and never seen by another person.
func TestHarnessSetupSkipIsAPerAccountPreference(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cookie, csrf := registerSecondUser(t, f, "second")
	for _, value := range []string{"1", "0"} {
		resp := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"harness_setup_skipped": value}}, true)
		if resp.StatusCode != 200 {
			t.Fatalf("store %q: %d", value, resp.StatusCode)
		}
		if got := prefsOf(t, resp.Body, json.NewDecoder(resp.Body).Decode); got["harness_setup_skipped"] != value {
			t.Fatalf("stored %q, read back %v", value, got)
		}
	}
	f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"harness_setup_skipped": "1"}}, true).Body.Close()
	resp := f.request("GET", "/api/me/preferences", nil, false)
	if got := prefsOf(t, resp.Body, json.NewDecoder(resp.Body).Decode); got["harness_setup_skipped"] != "1" {
		t.Fatalf("the skip is not there on the next read: %v", got)
	}
	other := f.requestAs(t, cookie, csrf, "GET", "/api/me/preferences", nil)
	if got := prefsOf(t, other.Body, json.NewDecoder(other.Body).Decode); got["harness_setup_skipped"] != "" {
		t.Fatalf("the skip leaked to another person: %v", got)
	}
}

// Dismissing the bar that offers browser notifications is the account's choice, kept on the server and never seen by another person.
func TestNotificationBannerDismissalIsAPerAccountPreference(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cookie, csrf := registerSecondUser(t, f, "second")
	resp := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"notification_banner_dismissed": "1"}}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("store: %d", resp.StatusCode)
	}
	if got := prefsOf(t, resp.Body, json.NewDecoder(resp.Body).Decode); got["notification_banner_dismissed"] != "1" {
		t.Fatalf("stored, read back %v", got)
	}
	other := f.requestAs(t, cookie, csrf, "GET", "/api/me/preferences", nil)
	if got := prefsOf(t, other.Body, json.NewDecoder(other.Body).Decode); got["notification_banner_dismissed"] != "" {
		t.Fatalf("the dismissal leaked to another person: %v", got)
	}
}

// The harness layout choice is gone, but a value stored while it existed stays in the database: reading must keep working. An older
// page may still send the name; it is accepted and dropped without losing the valid preferences sent with it.
func TestRetiredHarnessLayoutPreference(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	me := f.request("GET", "/api/me", nil, false)
	var user struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(me.Body).Decode(&user); err != nil {
		t.Fatal(err)
	}
	me.Body.Close()
	if err := f.db.SetUserPreferences(context.Background(), user.ID, map[string]string{"harness_layout": "b", "theme": "dark"}); err != nil {
		t.Fatal(err)
	}
	resp := f.request("GET", "/api/me/preferences", nil, false)
	if resp.StatusCode != 200 {
		t.Fatalf("get with a stored retired value: %d", resp.StatusCode)
	}
	if got := prefsOf(t, resp.Body, json.NewDecoder(resp.Body).Decode); got["theme"] != "dark" || got["harness_layout"] != "b" {
		t.Fatalf("read back %v", got)
	}
	// Sent with a valid name: 200, the valid one is stored, the retired one is dropped (the stored "b" is not replaced).
	put := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"harness_layout": "c", "theme": "light"}}, true)
	if put.StatusCode != 200 {
		t.Fatalf("a retired name next to a valid one: %d", put.StatusCode)
	}
	if got := prefsOf(t, put.Body, json.NewDecoder(put.Body).Decode); got["theme"] != "light" || got["harness_layout"] != "b" {
		t.Fatalf("after put %v", got)
	}
	// Sent alone: still 200, nothing changes.
	alone := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"harness_layout": "a"}}, true)
	if alone.StatusCode != 200 {
		t.Fatalf("a retired name alone: %d", alone.StatusCode)
	}
	if got := prefsOf(t, alone.Body, json.NewDecoder(alone.Body).Decode); got["theme"] != "light" || got["harness_layout"] != "b" {
		t.Fatalf("after put alone %v", got)
	}
	// An unknown name is still refused.
	bad := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"nonsense": "x"}}, true)
	defer bad.Body.Close()
	if bad.StatusCode != 400 {
		t.Fatalf("an unknown name: %d", bad.StatusCode)
	}
}

// Whether desktop notifications show the start of a message is a personal preference kept on the server, never seen by another person.
func TestNotificationTextIsAPerAccountPreference(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cookie, csrf := registerSecondUser(t, f, "second")
	resp := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"notification_text": "1"}}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("store: %d", resp.StatusCode)
	}
	if got := prefsOf(t, resp.Body, json.NewDecoder(resp.Body).Decode); got["notification_text"] != "1" {
		t.Fatalf("stored, read back %v", got)
	}
	other := f.requestAs(t, cookie, csrf, "GET", "/api/me/preferences", nil)
	if got := prefsOf(t, other.Body, json.NewDecoder(other.Body).Decode); got["notification_text"] != "" {
		t.Fatalf("the choice leaked to another person: %v", got)
	}
}

// Whether the room list previews the last message under every room is a personal preference kept on the server, never seen by another person.
func TestSidebarPreviewIsAPerAccountPreference(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cookie, csrf := registerSecondUser(t, f, "second")
	resp := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"sidebar_preview": "1"}}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("store: %d", resp.StatusCode)
	}
	if got := prefsOf(t, resp.Body, json.NewDecoder(resp.Body).Decode); got["sidebar_preview"] != "1" {
		t.Fatalf("stored, read back %v", got)
	}
	other := f.requestAs(t, cookie, csrf, "GET", "/api/me/preferences", nil)
	if got := prefsOf(t, other.Body, json.NewDecoder(other.Body).Decode); got["sidebar_preview"] != "" {
		t.Fatalf("the choice leaked to another person: %v", got)
	}
}

// The project status pill had A/B/C/D display variants; A is now the only one. A value stored while the choice existed stays in the
// database and is read back, and a page that still sends the name has it accepted and dropped without losing the valid preferences
// sent with it.
func TestRetiredSidebarStatusVariantPreference(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	me := f.request("GET", "/api/me", nil, false)
	var user struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(me.Body).Decode(&user); err != nil {
		t.Fatal(err)
	}
	me.Body.Close()
	if err := f.db.SetUserPreferences(context.Background(), user.ID, map[string]string{"sidebar_status_variant": "d", "theme": "dark"}); err != nil {
		t.Fatal(err)
	}
	resp := f.request("GET", "/api/me/preferences", nil, false)
	if resp.StatusCode != 200 {
		t.Fatalf("get with a stored retired value: %d", resp.StatusCode)
	}
	if got := prefsOf(t, resp.Body, json.NewDecoder(resp.Body).Decode); got["theme"] != "dark" || got["sidebar_status_variant"] != "d" {
		t.Fatalf("read back %v", got)
	}
	// Sent with a valid name: 200, the valid one is stored, the retired one is dropped (the stored "d" is not replaced).
	put := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"sidebar_status_variant": "b", "theme": "light"}}, true)
	if put.StatusCode != 200 {
		t.Fatalf("a retired name next to a valid one: %d", put.StatusCode)
	}
	if got := prefsOf(t, put.Body, json.NewDecoder(put.Body).Decode); got["theme"] != "light" || got["sidebar_status_variant"] != "d" {
		t.Fatalf("after put %v", got)
	}
	// Sent alone: still 200, nothing changes.
	alone := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"sidebar_status_variant": "a"}}, true)
	if alone.StatusCode != 200 {
		t.Fatalf("a retired name alone: %d", alone.StatusCode)
	}
	if got := prefsOf(t, alone.Body, json.NewDecoder(alone.Body).Decode); got["theme"] != "light" || got["sidebar_status_variant"] != "d" {
		t.Fatalf("after put alone %v", got)
	}
}

// The thread panel's order choice (tree or time) is gone, the tree being the only look, but a value stored while it existed stays in the database and is read back, and a page that
// still sends the name has it accepted and dropped without losing the valid preferences sent with it.
func TestRetiredThreadOrderPreference(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	me := f.request("GET", "/api/me", nil, false)
	var user struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(me.Body).Decode(&user); err != nil {
		t.Fatal(err)
	}
	me.Body.Close()
	if err := f.db.SetUserPreferences(context.Background(), user.ID, map[string]string{"thread_order": "time", "theme": "dark"}); err != nil {
		t.Fatal(err)
	}
	put := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"thread_order": "tree", "theme": "light"}}, true)
	if put.StatusCode != 200 {
		t.Fatalf("a retired name next to a valid one: %d", put.StatusCode)
	}
	if got := prefsOf(t, put.Body, json.NewDecoder(put.Body).Decode); got["theme"] != "light" || got["thread_order"] != "time" {
		t.Fatalf("the valid one is stored, the retired one is dropped (the stored value is not replaced): %v", got)
	}
}

// The message layout preference is retired with layout A (every message now sits on the left): an older page may still send the name, and a
// value stored while the choice existed stays in the database and is ignored harmlessly.
func TestMessageLayoutIsRetired(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	me := f.request("GET", "/api/me", nil, false)
	var user struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(me.Body).Decode(&user); err != nil {
		t.Fatal(err)
	}
	me.Body.Close()
	if err := f.db.SetUserPreferences(context.Background(), user.ID, map[string]string{"message_layout": "a", "theme": "dark"}); err != nil {
		t.Fatal(err)
	}
	// Sent with a valid name: 200, the valid one is stored, the retired one is dropped (the stored "a" is not replaced).
	put := f.request("PUT", "/api/me/preferences", map[string]any{"preferences": map[string]string{"message_layout": "b", "theme": "light"}}, true)
	if put.StatusCode != 200 {
		t.Fatalf("a retired name next to a valid one: %d", put.StatusCode)
	}
	if got := prefsOf(t, put.Body, json.NewDecoder(put.Body).Decode); got["theme"] != "light" || got["message_layout"] != "a" {
		t.Fatalf("the valid one is stored, the retired one is dropped (the stored value is not replaced): %v", got)
	}
}

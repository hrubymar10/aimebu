package server

import (
	"io"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
)

func TestRetentionSettingIsReadableByEveryoneAndWritableByTheOwner(t *testing.T) {
	f := newFixture(t)
	read := func() (int, retentionBody) {
		resp := f.request("GET", "/api/retention", nil, false)
		defer resp.Body.Close()
		var body retentionBody
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &body)
		return resp.StatusCode, body
	}
	code, body := read()
	if code != 200 || body.MessagesConsideredExpiredAfterSeconds != 21600 || body.DefaultSeconds != 21600 || body.MinSeconds != 60 || body.MaxSeconds != 2592000 || !body.CanEdit {
		t.Fatalf("default: %d %+v", code, body)
	}
	put := func(v any) int {
		resp := f.request("PUT", "/api/retention", map[string]any{"messages_considered_expired_after_seconds": v}, true)
		resp.Body.Close()
		return resp.StatusCode
	}
	if put(3600) != 200 {
		t.Fatal("the owner could not save")
	}
	if _, body := read(); body.MessagesConsideredExpiredAfterSeconds != 3600 {
		t.Fatalf("saved value = %d", body.MessagesConsideredExpiredAfterSeconds)
	}
	for _, bad := range []any{59, -1, 2592001, "x", nil} {
		if code := put(bad); code != 400 {
			t.Errorf("PUT %v = %d, want 400", bad, code)
		}
	}
	if put(0) != 200 {
		t.Fatal("0 (never expire) must be accepted")
	}
	// A write without the CSRF token, and an anonymous caller, are refused.
	resp := f.request("PUT", "/api/retention", map[string]any{"messages_considered_expired_after_seconds": 120}, false)
	resp.Body.Close()
	if resp.StatusCode != 403 && resp.StatusCode != 401 {
		t.Fatalf("a write without CSRF = %d", resp.StatusCode)
	}
	anon, err := http.Get(f.http.URL + "/api/retention")
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != 401 {
		t.Fatalf("anonymous = %d, want 401", anon.StatusCode)
	}
}

func TestOnlyTheOwnerCanChangeTheExpiryWindow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	cookie, csrf := registerSecondUser(t, f, "member")
	// A signed-in non-owner may read the window but not write it.
	read := f.requestAs(t, cookie, csrf, http.MethodGet, "/api/retention", nil)
	var body retentionBody
	if err := json.NewDecoder(read.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	read.Body.Close()
	if read.StatusCode != 200 || body.CanEdit {
		t.Fatalf("a non-owner reads the window without edit rights: %d %+v", read.StatusCode, body)
	}
	write := f.requestAs(t, cookie, csrf, http.MethodPut, "/api/retention", map[string]any{"messages_considered_expired_after_seconds": 120})
	write.Body.Close()
	if write.StatusCode != 403 {
		t.Fatalf("a non-owner's PUT must be 403, got %d", write.StatusCode)
	}
	check := f.request(http.MethodGet, "/api/retention", nil, false)
	defer check.Body.Close()
	if err := json.NewDecoder(check.Body).Decode(&body); err != nil || body.MessagesConsideredExpiredAfterSeconds == 120 {
		t.Fatalf("the refused write changed the window: %+v %v", body, err)
	}
}

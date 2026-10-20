package server

import (
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

func TestUsagesUpdatePushedToOwnerSocket(t *testing.T) {
	f := newFixture(t)
	var err error
	f.srv.Usages, err = usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Handler() // installs the update hook
	conn := f.dial(t, 0, f.http.URL)
	defer conn.Close(1000, "")
	for {
		var kind string
		_ = json.Unmarshal(readFrame(t, conn)["type"], &kind)
		if kind == "ready" {
			break
		}
	}
	f.srv.Usages.ConfigChanged(providers.ProviderMistral, true, true)
	var kind string
	_ = json.Unmarshal(readFrame(t, conn)["type"], &kind)
	if kind != "usages_updated" {
		t.Fatalf("frame type = %q, want usages_updated", kind)
	}
}

func TestUsagesSettingsAndNoopToggleAlsoPush(t *testing.T) {
	f := newFixture(t)
	var err error
	f.srv.Usages, err = usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Handler()
	conn := f.dial(t, 0, f.http.URL)
	defer conn.Close(1000, "")
	waitFrame := func(want string) {
		t.Helper()
		for {
			var kind string
			_ = json.Unmarshal(readFrame(t, conn)["type"], &kind)
			if kind == want {
				return
			}
		}
	}
	waitFrame("ready")
	for name, call := range map[string]func() int{
		"display settings": func() int {
			resp := f.request("POST", "/api/usages/settings", map[string]string{"percent_display": "used"}, true)
			resp.Body.Close()
			return resp.StatusCode
		},
		"first write of an unchanged provider entry": func() int {
			resp := f.request("POST", "/api/usages/providers", map[string]any{"provider": providers.ProviderCodex, "enabled": false}, true)
			resp.Body.Close()
			return resp.StatusCode
		},
	} {
		if status := call(); status != 200 {
			t.Fatalf("%s: status %d", name, status)
		}
		waitFrame("usages_updated")
	}
}

func TestUsagesRefreshCooldown(t *testing.T) {
	f := newFixture(t)
	var err error
	f.srv.Usages, err = usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resp := f.request("POST", "/api/usages/refresh", map[string]string{"provider": "bogus"}, true)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("unknown provider status = %d, want 400", resp.StatusCode)
	}
	resp = f.request("POST", "/api/usages/refresh", map[string]string{"provider": providers.ProviderMistral}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("first refresh status = %d, want 200 (a rejected provider must not start the cooldown)", resp.StatusCode)
	}
	resp = f.request("POST", "/api/usages/refresh", map[string]string{}, true)
	defer resp.Body.Close()
	var body struct {
		Error string `json:"error"`
		Retry int    `json:"retry_after_sec"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != 429 || body.Error != "cooldown" || body.Retry < 1 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("second refresh status=%d body=%+v retry-after=%q", resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
}

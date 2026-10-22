package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

func TestUsageAlertInboxIsGlobalButUsagesStayOwnerOnly(t *testing.T) {
	f := newFixture(t)
	root := t.TempDir()
	var err error
	f.srv.Usages, err = usages.New(root)
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := usages.NewAlertTracker(root)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.usageAlerts = tracker
	// The alert closes when its reset time passes (by the real clock), so the times are relative to now: a fixed date made this test fail for
	// good once that date was in the past.
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(24 * time.Hour)
	snapshot := usages.Response{Providers: []usages.Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []usages.Profile{{ProfileName: "work", Email: "private@example.test", Status: "ok", LastRefreshAt: &now,
			Windows: []usages.Window{{Key: "session", PercentUsed: 81, ResetAt: &reset}}}}}}}
	snapshot.Settings.AlertWarningRemaining, snapshot.Settings.AlertCriticalRemaining = 20, 10
	if created, err := tracker.Observe(snapshot); err != nil || !created {
		t.Fatalf("alert: %v %v", created, err)
	}
	adminCookie, adminCSRF := registerSecondUser(t, f, "admin")
	ordinaryCookie, ordinaryCSRF := registerSecondUser(t, f, "ordinary")
	owner, err := f.db.UserByLogin(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := f.db.UserByLogin(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetUserAdmin(context.Background(), owner.ID, admin.ID, true); err != nil {
		t.Fatal(err)
	}
	read := func(cookie *http.Cookie, csrf string) []struct {
		Kind      string `json:"kind"`
		Question  string `json:"question"`
		CreatedAt string `json:"created_at"`
	} {
		t.Helper()
		resp := f.requestAs(t, cookie, csrf, http.MethodGet, "/api/inbox", nil)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("inbox status: %d", resp.StatusCode)
		}
		var items []struct {
			Kind      string `json:"kind"`
			Question  string `json:"question"`
			CreatedAt string `json:"created_at"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
			t.Fatal(err)
		}
		return items
	}
	for _, identity := range []struct {
		cookie *http.Cookie
		csrf   string
	}{{f.cookie, f.csrf}, {adminCookie, adminCSRF}} {
		items := read(identity.cookie, identity.csrf)
		if len(items) != 1 || items[0].Kind != "usage_alert" || !strings.Contains(items[0].Question, "Codex · work · session: 19.0% remaining") || strings.Contains(items[0].Question, "private@example.test") {
			t.Fatalf("global alert: %+v", items)
		}
		if _, err := time.Parse("2006-01-02T15:04:05.000000000Z", items[0].CreatedAt); err != nil || len(items[0].CreatedAt) != len("2006-01-02T15:04:05.000000000Z") {
			t.Fatalf("inbox timestamp should sort with store rows: %q %v", items[0].CreatedAt, err)
		}
	}
	if items := read(ordinaryCookie, ordinaryCSRF); len(items) != 0 {
		t.Fatalf("ordinary account saw usage alert: %+v", items)
	}
	alertID := tracker.OpenAlerts()[0].ID
	resp := f.requestAs(t, ordinaryCookie, ordinaryCSRF, http.MethodPost, "/api/inbox/"+alertID+"/dismiss", map[string]int{"rev": 0})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("ordinary dismiss: %d", resp.StatusCode)
	}
	resp = f.requestAs(t, adminCookie, adminCSRF, http.MethodPost, "/api/inbox/"+alertID+"/dismiss", map[string]int{"rev": 0})
	resp.Body.Close()
	if resp.StatusCode != 204 || len(read(f.cookie, f.csrf)) != 0 {
		t.Fatalf("admin dismiss: %d", resp.StatusCode)
	}
	resp = f.requestAs(t, adminCookie, adminCSRF, http.MethodGet, "/api/usages", nil)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("admin usages status: %d", resp.StatusCode)
	}
}

func TestUsagesUpdateSocketOnlyForGlobalAdmin(t *testing.T) {
	f := newFixture(t)
	var err error
	f.srv.Usages, err = usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Handler()
	adminCookie, _ := registerSecondUser(t, f, "admin")
	ordinaryCookie, _ := registerSecondUser(t, f, "ordinary")
	owner, err := f.db.UserByLogin(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := f.db.UserByLogin(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetUserAdmin(context.Background(), owner.ID, admin.ID, true); err != nil {
		t.Fatal(err)
	}
	dial := func(cookie *http.Cookie) *websocket.Conn {
		t.Helper()
		header := http.Header{}
		header.Set("Origin", f.http.URL)
		header.Set("Cookie", cookie.String())
		conn, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/events?after=0", &websocket.DialOptions{HTTPHeader: header})
		if err != nil {
			t.Fatalf("dial status %v: %v", resp, err)
		}
		return conn
	}
	adminConn, ordinaryConn := dial(adminCookie), dial(ordinaryCookie)
	defer adminConn.Close(1000, "")
	defer ordinaryConn.Close(1000, "")
	for _, conn := range []*websocket.Conn{adminConn, ordinaryConn} {
		for {
			var kind string
			_ = json.Unmarshal(readFrame(t, conn)["type"], &kind)
			if kind == "ready" {
				break
			}
		}
	}
	f.srv.nudgeUsageWatchers()
	var kind string
	_ = json.Unmarshal(readFrame(t, adminConn)["type"], &kind)
	if kind != "usages_updated" {
		t.Fatalf("admin frame: %q", kind)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, _, err := ordinaryConn.Read(ctx); err == nil {
		t.Fatal("ordinary user received usages update")
	}
}

func TestUsageAlertThresholdSettings(t *testing.T) {
	f := newFixture(t)
	var err error
	f.srv.Usages, err = usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodPost, "/api/usages/settings", map[string]int{"alert_warning_remaining": 25, "alert_critical_remaining": 15}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("save thresholds: %d", resp.StatusCode)
	}
	config, err := f.srv.Usages.ExtraStore().LoadConfig()
	if err != nil || config.AlertWarningRemaining != 25 || config.AlertCriticalRemaining != 15 {
		t.Fatalf("saved thresholds: %+v %v", config, err)
	}
	view := f.srv.Usages.Snapshot()
	if view.Settings.AlertWarningRemaining != 25 || view.Settings.AlertCriticalRemaining != 15 {
		t.Fatalf("published thresholds: %+v", view.Settings)
	}
	resp = f.request(http.MethodPost, "/api/usages/settings", map[string]int{"alert_warning_remaining": 10, "alert_critical_remaining": 20}, true)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("reversed thresholds accepted: %d", resp.StatusCode)
	}
	config, err = f.srv.Usages.ExtraStore().LoadConfig()
	if err != nil || config.AlertWarningRemaining != 25 || config.AlertCriticalRemaining != 15 {
		t.Fatalf("invalid settings changed thresholds: %+v %v", config, err)
	}
}

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

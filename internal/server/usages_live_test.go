package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/quota"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
	"github.com/hrubymar10/aimebu/v2/internal/usages/providers"
)

func TestUsageAlertInboxIsGlobalButUsagesChangesStayOwnerOnly(t *testing.T) {
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
		Provider  string `json:"provider"`
		Profile   string `json:"profile"`
		Window    string `json:"window"`
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
			Provider  string `json:"provider"`
			Profile   string `json:"profile"`
			Window    string `json:"window"`
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
		// The row names its window, as the Usages view keys it, so the card can draw that window's chart.
		if items[0].Provider != "codex" || items[0].Profile != "work" || items[0].Window != "session" {
			t.Fatalf("alert row must name provider, profile and window: %+v", items[0])
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
	if resp.StatusCode != 204 || len(read(f.cookie, f.csrf)) != 0 || f.srv.usageAlertRevision.Load() != 1 {
		t.Fatalf("admin dismiss: %d", resp.StatusCode)
	}
	resp = f.requestAs(t, adminCookie, adminCSRF, http.MethodGet, "/api/usages", nil)
	resp.Body.Close()
	if resp.StatusCode == 403 {
		t.Fatalf("an admin reads the usages (the limits, redacted): %d", resp.StatusCode)
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

func TestGuardHoldChangesRefreshNonOwnerAdminInbox(t *testing.T) {
	f := newFixture(t)
	tracker, err := usages.NewAlertTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.srv.usageAlerts = tracker
	now := time.Now().UTC()
	reset := now.Add(24 * time.Hour)
	snapshot := usages.Response{Providers: []usages.Provider{{ProviderName: "claude-code", Enabled: true, Available: true,
		Profiles: []usages.Profile{{ProfileName: "work", Email: "work@example.test", Status: "ok", LastRefreshAt: &now,
			Windows: []usages.Window{{Key: "session", PercentUsed: 81, ResetAt: &reset}}}}}}}
	snapshot.Settings.AlertWarningRemaining, snapshot.Settings.AlertCriticalRemaining = 20, 10
	if changed, err := tracker.Observe(snapshot); err != nil || !changed {
		t.Fatalf("alert: %v %v", changed, err)
	}
	adminCookie, adminCSRF := registerSecondUser(t, f, "admin")
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := f.db.UserByLogin(t.Context(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetUserAdmin(t.Context(), owner.ID, admin.ID, true); err != nil {
		t.Fatal(err)
	}
	f.srv.Handler()
	header := http.Header{}
	header.Set("Origin", f.http.URL)
	header.Set("Cookie", adminCookie.String())
	conn, response, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/events?after=0", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("admin socket %v: %v", response, err)
	}
	defer conn.Close(1000, "")
	for {
		var kind string
		_ = json.Unmarshal(readFrame(t, conn)["type"], &kind)
		if kind == "ready" {
			break
		}
	}
	inboxCount := func() int {
		t.Helper()
		resp := f.requestAs(t, adminCookie, adminCSRF, http.MethodGet, "/api/inbox", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("admin Inbox status = %d", resp.StatusCode)
		}
		var items []struct {
			Kind string `json:"kind"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
			t.Fatal(err)
		}
		return len(items)
	}
	if got := inboxCount(); got != 1 {
		t.Fatalf("initial admin Inbox = %d", got)
	}
	src := &quotaFakeSource{}
	guard := quota.NewGuard(f.srv.Store, src, f.srv.Store, nil)
	guard.OnHoldChange = f.srv.guardHoldChanged
	f.srv.guard.Store(guard)
	readRevision := func() uint64 {
		t.Helper()
		frame := readFrame(t, conn)
		var kind string
		_ = json.Unmarshal(frame["type"], &kind)
		if kind != "usages_updated" {
			t.Fatalf("admin guard frame = %q", kind)
		}
		var revision uint64
		if err := json.Unmarshal(frame["alert_revision"], &revision); err != nil {
			t.Fatal(err)
		}
		return revision
	}
	src.set(quota.Observation{Active: "A", Profiles: []quota.Obs{qobs("A", 96, 10)}, Ready: true})
	guard.Evaluate(t.Context())
	if revision := readRevision(); revision != 1 || inboxCount() != 0 {
		t.Fatalf("admin did not lose guarded alert: revision=%d", revision)
	}
	stale := qobs("A", 96, 10)
	stale.Stale = true
	src.set(quota.Observation{Active: "A", Profiles: []quota.Obs{stale}, Ready: true})
	guard.Evaluate(t.Context())
	if revision := readRevision(); revision != 2 || inboxCount() != 1 {
		t.Fatalf("admin did not regain alert after silent release: revision=%d", revision)
	}
	guard.Evaluate(t.Context())
	if revision := f.srv.usageAlertRevision.Load(); revision != 2 {
		t.Fatalf("unchanged guard sent another hint: revision=%d", revision)
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

func TestWarmupSettingsValidateAndPublish(t *testing.T) {
	f := newFixture(t)
	var err error
	f.srv.Usages, err = usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resp := f.request(http.MethodPost, "/api/usages/settings", map[string]string{"warmup_mode": "scheduled", "warmup_schedule": "30 9 * * 1"}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("save warmup: %d", resp.StatusCode)
	}
	view := f.srv.Usages.Snapshot()
	if view.Settings.WarmupMode != "scheduled" || view.Settings.WarmupSchedule != "30 9 * * 1" {
		t.Fatalf("published warmup: %+v", view.Settings)
	}
	resp = f.request(http.MethodPost, "/api/usages/settings", map[string]string{"warmup_schedule": "*/5 * * * *"}, true)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("invalid schedule accepted: %d", resp.StatusCode)
	}
	config, err := f.srv.Usages.ExtraStore().LoadConfig()
	if err != nil || config.WarmupSchedule != "30 9 * * 1" {
		t.Fatalf("invalid schedule saved: %+v %v", config, err)
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
	frame := readFrame(t, conn)
	_ = json.Unmarshal(frame["type"], &kind)
	if kind != "usages_updated" {
		t.Fatalf("frame type = %q, want usages_updated", kind)
	}
	var revision uint64
	if err := json.Unmarshal(frame["alert_revision"], &revision); err != nil || revision != 0 {
		t.Fatalf("ordinary snapshot changed alert revision: %s %v", frame["alert_revision"], err)
	}
	f.srv.usageAlertRevision.Add(1)
	f.srv.nudgeUsageWatchers()
	frame = readFrame(t, conn)
	if err := json.Unmarshal(frame["alert_revision"], &revision); err != nil || revision != 1 {
		t.Fatalf("changed alert revision = %s %v", frame["alert_revision"], err)
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

// A window reset is good news, not a task: GET /api/inbox carries it as a
// `usage_notice` row in state `notice` (the browser toasts it and the Inbox
// leaves it out), while the threshold and pace alerts stay open `usage_alert`
// Inbox items.
func TestUsageResetNoticeIsNoOpenInboxItem(t *testing.T) {
	f := newFixture(t)
	root := t.TempDir()
	var err error
	f.srv.Usages, err = usages.New(root)
	if err != nil {
		t.Fatal(err)
	}
	// A controllable clock: a pace alert needs ten minutes of continuous risk.
	clock := time.Now().UTC()
	tracker, err := usages.NewAlertTracker(root, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	f.srv.usageAlerts = tracker
	// The real clock: an alert closes once its reset time passes.
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(5 * time.Hour)
	eta := time.Hour.Seconds()
	snapshot := usages.Response{Providers: []usages.Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []usages.Profile{{ProfileName: "work", AlertIdentity: "stable", Status: "ok", LastRefreshAt: &now,
			Windows: []usages.Window{{Key: "session", PercentUsed: 40, ResetAt: &reset}}}}}}}
	snapshot.Settings.AlertWarningRemaining, snapshot.Settings.AlertCriticalRemaining = 20, 10
	snapshot.Settings.AlertResetNoticeEnabled, snapshot.Settings.AlertPaceEnabled = true, true
	if _, err := tracker.Observe(snapshot); err != nil {
		t.Fatal(err)
	}
	// A reset an hour earlier is a new cycle: the reset notice, plus a
	// warning and a pace alert for the new cycle's reading.
	later := now.Add(time.Second)
	earlier := reset.Add(-time.Hour)
	profile := &snapshot.Providers[0].Profiles[0]
	profile.LastRefreshAt = &later
	profile.Windows[0] = usages.Window{Key: "session", PercentUsed: 85, ResetAt: &earlier,
		Pace: &usages.Pace{ExpectedPercent: 40, DeltaPercent: 45, State: "deficit", EtaSeconds: &eta}}
	if changed, err := tracker.Observe(snapshot); err != nil || !changed {
		t.Fatalf("new cycle: %v %v", changed, err)
	}
	// Still at risk after the hysteresis: the pace alert raises.
	clock = clock.Add(11 * time.Minute)
	afterHysteresis := later.Add(11 * time.Minute)
	profile.LastRefreshAt = &afterHysteresis
	if _, err := tracker.Observe(snapshot); err != nil {
		t.Fatal(err)
	}
	resp := f.requestAs(t, f.cookie, f.csrf, http.MethodGet, "/api/inbox", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("inbox status: %d", resp.StatusCode)
	}
	var items []struct {
		Kind     string `json:"kind"`
		State    string `json:"state"`
		Question string `json:"question"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	open, notices := map[string]int{}, 0
	for _, item := range items {
		switch {
		case item.Kind == "usage_alert" && item.State == "open":
			switch {
			case strings.Contains(item.Question, "projected to run out"):
				open["pace"]++
			case strings.Contains(item.Question, "15.0% remaining"):
				open["warning"]++
			default:
				t.Fatalf("unexpected open alert: %+v", item)
			}
		case item.Kind == "usage_notice" && item.State == "notice":
			if !strings.Contains(item.Question, "the window reset") {
				t.Fatalf("notice text: %+v", item)
			}
			notices++
		default:
			t.Fatalf("unexpected inbox row: %+v", item)
		}
	}
	if notices != 1 || open["warning"] != 1 || open["pace"] != 1 {
		t.Fatalf("one reset notice outside the Inbox, the warning and pace alerts in it: %+v", items)
	}
}

func TestLoginRejectedBellEventAndInboxItem(t *testing.T) {
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
	snapshot := usages.Response{Providers: []usages.Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []usages.Profile{{ProfileName: "work", Active: true, AlertIdentity: "ident\x00work", Status: "auth_missing",
			Error: "Codex usage request failed with HTTP 401.", TokenTail: "dead42"}}}}}
	if _, err := tracker.Observe(snapshot); err != nil {
		t.Fatal(err)
	}
	f.srv.recordRejectedLogins(tracker)
	events := func() []store.Event {
		t.Helper()
		resp := f.requestAs(t, f.cookie, f.csrf, http.MethodGet, "/api/system-events?notable=1", nil)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("system events status: %d", resp.StatusCode)
		}
		var page struct {
			Events []store.Event `json:"events"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		return page.Events
	}
	var rejected []store.Event
	for _, event := range events() {
		if event.Kind == "usage_login_rejected" {
			rejected = append(rejected, event)
		}
	}
	if len(rejected) != 1 || !rejected[0].Notable {
		t.Fatalf("one notable bell event: %+v", rejected)
	}
	var payload struct {
		Tool      string `json:"tool"`
		Profile   string `json:"profile"`
		Hint      string `json:"hint"`
		FirstSeen string `json:"first_seen"`
		TokenTail string `json:"token_tail"`
	}
	if err := json.Unmarshal([]byte(rejected[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Tool != "codex" || payload.Profile != "work" || payload.Hint != "Run codex login." {
		t.Fatalf("event payload: %+v", payload)
	}
	// The event says when the poller first saw the rejection, not when it was
	// recorded, and which credential was rejected, by its hash tail only.
	seen, err := time.Parse(time.RFC3339, payload.FirstSeen)
	if err != nil || time.Since(seen) > time.Minute {
		t.Fatalf("event first_seen: %q (%v)", payload.FirstSeen, err)
	}
	if payload.TokenTail != "dead42" {
		t.Fatalf("event token_tail: %+v", payload)
	}
	// A second drain records nothing: once per episode.
	f.srv.recordRejectedLogins(tracker)
	rejected = nil
	for _, event := range events() {
		if event.Kind == "usage_login_rejected" {
			rejected = append(rejected, event)
		}
	}
	if len(rejected) != 1 {
		t.Fatalf("a second drain must not add an event: %+v", rejected)
	}
	// The Inbox item rides with the alert, with the fixed wording and the hint.
	resp := f.requestAs(t, f.cookie, f.csrf, http.MethodGet, "/api/inbox", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("inbox status: %d", resp.StatusCode)
	}
	var items []struct {
		Kind     string `json:"kind"`
		Question string `json:"question"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Kind != "usage_alert" {
		t.Fatalf("inbox item: %+v", items)
	}
	// The item names the fix and says when the login was first rejected and
	// which credential it was, by its hash tail.
	question := items[0].Question
	if !strings.HasPrefix(question, "Codex · work: login rejected: log in again. Run codex login. First rejected at ") ||
		!strings.HasSuffix(question, "(token hash tail dead42).") {
		t.Fatalf("inbox item text: %q", question)
	}
}

// The chart's marker is wired in StartUsageAlerts: the Reader's snapshots
// read the tracker's episode state. The Reader side and the tracker side are
// covered in internal/usages (TestSnapshotMarksARejectedSeriesWithinTheSpan
// with a real tracker).
func TestStartUsageAlertsWiresTheRejectionTimes(t *testing.T) {
	f := newFixture(t)
	root := t.TempDir()
	rejected := usages.Response{Providers: []usages.Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []usages.Profile{{ProfileName: "work", Active: true, Email: "work@example.test", Status: "auth_missing",
			Error: "Codex usage request failed with HTTP 401."}}}}}
	cache, err := json.Marshal(rejected)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cache.json"), cache, 0o600); err != nil {
		t.Fatal(err)
	}
	f.srv.Usages, err = usages.New(root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := f.srv.Usages.Snapshot()
	if got := snapshot.Providers[0].Profiles[0]; got.ProfileName != "work" || got.Status != "auth_missing" || got.Email != "work@example.test" {
		t.Fatalf("cached rejection: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed, unsubscribe := f.srv.watchUsages()
	defer unsubscribe()
	if err := f.srv.StartUsageAlerts(ctx, root); err != nil {
		t.Fatal(err)
	}
	tracker := f.srv.usageAlerts
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatalf("the usage alert observer did not process the cached rejection: snapshot=%+v times=%+v", f.srv.Usages.Snapshot().Providers[0], tracker.RejectionTimes())
	}
	// The server's snapshot answers through the tracker's RejectionTimes
	// without an extra wiring step.
	times := tracker.RejectionTimes()
	if _, seen := times["codex\x00work"]; !seen {
		t.Fatalf("the tracker lists the episode: %+v", times)
	}
}

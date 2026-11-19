package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestScheduleHTTPCRUDAndValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	path := "/api/rooms/" + f.room.ID + "/schedules"
	body := map[string]any{"target_lead": true, "body": "review the room", "kind": "interval", "timezone": "UTC", "interval_seconds": 300}
	resp := f.request(http.MethodPost, path, body, true)
	if resp.StatusCode != 201 {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	var created store.Schedule
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !created.Enabled || created.NextRunAt == "" || created.CreatorKind != "user" {
		t.Fatalf("created=%+v", created)
	}
	resp = f.request(http.MethodGet, path, nil, false)
	var listed struct {
		Schedules []store.Schedule `json:"schedules"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(listed.Schedules) != 1 {
		t.Fatalf("list status=%d items=%d", resp.StatusCode, len(listed.Schedules))
	}
	resp = f.request(http.MethodPatch, path+"/"+created.ID, map[string]any{"enabled": false, "rev": created.Rev}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("pause status=%d", resp.StatusCode)
	}
	var paused store.Schedule
	_ = json.NewDecoder(resp.Body).Decode(&paused)
	resp.Body.Close()
	if paused.Enabled {
		t.Fatal("still enabled")
	}
	resp = f.request(http.MethodPatch, path+"/"+created.ID, map[string]any{"enabled": true, "rev": created.Rev}, true)
	if resp.StatusCode != 409 {
		t.Fatalf("stale revision status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodDelete, path+"/"+created.ID, nil, true)
	if resp.StatusCode != 204 {
		t.Fatalf("delete status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodPost, path, map[string]any{"target_lead": true, "body": "too frequent", "kind": "interval", "timezone": "UTC", "interval_seconds": 60}, true)
	if resp.StatusCode != 400 {
		t.Fatalf("short interval status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodPost, path, map[string]any{"target_lead": true, "body": "once", "kind": "once", "timezone": "UTC", "at": time.Now().Add(time.Hour).Format(time.RFC3339)}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("once status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLeadToolScheduleIsScopedAndIdempotent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgent(t, "role_leader_default")
	path := "/api/agent/rooms/" + f.room.ID + "/schedules"
	body := map[string]any{"target_lead": true, "body": "check status", "kind": "interval", "timezone": "UTC", "interval_seconds": 300}
	first := f.toolPost(t, path, key, "schedule-key", body)
	if first.StatusCode != 201 {
		t.Fatalf("first status=%d", first.StatusCode)
	}
	var item store.Schedule
	_ = json.NewDecoder(first.Body).Decode(&item)
	first.Body.Close()
	if item.CreatorID != lead.ID || item.CreatorKind != "agent" {
		t.Fatalf("creator=%+v", item)
	}
	replay := f.toolPost(t, path, key, "schedule-key", body)
	if replay.StatusCode != 200 {
		t.Fatalf("replay status=%d", replay.StatusCode)
	}
	replay.Body.Close()
	items := f.toolRequest(t, path, key)
	var listed struct {
		Schedules []store.Schedule `json:"schedules"`
	}
	_ = json.NewDecoder(items.Body).Decode(&listed)
	items.Body.Close()
	if items.StatusCode != 200 || len(listed.Schedules) != 1 {
		t.Fatalf("list status=%d count=%d", items.StatusCode, len(listed.Schedules))
	}
	noKey := f.toolPost(t, path, key, "", body)
	if noKey.StatusCode != 400 {
		t.Fatalf("missing key status=%d", noKey.StatusCode)
	}
	noKey.Body.Close()
}

// The project home room is an ordinary open room with the project lead in it,
// so people and the lead schedule notes there exactly as in a task room. A
// private chat never holds schedules.
func TestHomeRoomSchedulesAndPrivateChatRefusal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	resp := f.request(http.MethodGet, "/api/projects/"+f.project.ID+"/rooms", nil, false)
	var rooms []store.Room
	_ = json.NewDecoder(resp.Body).Decode(&rooms)
	resp.Body.Close()
	var home store.Room
	for _, room := range rooms {
		if room.Kind == "home" {
			home = room
		}
	}
	if home.ID == "" {
		t.Fatalf("no home room in %+v", rooms)
	}
	lead, key := f.createToolAgent(t, "role_leader_default")
	if err := f.db.AddMember(t.Context(), home.ID, "agent", lead.ID); err != nil {
		t.Fatal(err)
	}
	path := "/api/rooms/" + home.ID + "/schedules"
	resp = f.request(http.MethodPost, path, map[string]any{"target_lead": true, "body": "morning summary", "kind": "daily", "timezone": "UTC", "local_time": "09:00"}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("home create status=%d", resp.StatusCode)
	}
	var created store.Schedule
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.RoomID != home.ID || created.ProjectID != f.project.ID || !created.TargetLead {
		t.Fatalf("created=%+v", created)
	}
	resp = f.request(http.MethodPost, path, map[string]any{"target_agent_id": lead.ID, "body": "direct note", "kind": "interval", "timezone": "UTC", "interval_seconds": 600}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("home member target status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodGet, path, nil, false)
	var listed struct {
		Schedules []store.Schedule `json:"schedules"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(listed.Schedules) != 2 {
		t.Fatalf("home list status=%d items=%d", resp.StatusCode, len(listed.Schedules))
	}
	resp = f.request(http.MethodPatch, path+"/"+created.ID, map[string]any{"enabled": false, "rev": created.Rev}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("home pause status=%d", resp.StatusCode)
	}
	var paused store.Schedule
	_ = json.NewDecoder(resp.Body).Decode(&paused)
	resp.Body.Close()
	resp = f.request(http.MethodPatch, path+"/"+created.ID, map[string]any{"enabled": true, "rev": paused.Rev}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("home resume status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	toolPath := "/api/agent/rooms/" + home.ID + "/schedules"
	tool := f.toolPost(t, toolPath, key, "home-schedule", map[string]any{"target_lead": true, "body": "check the backlog", "kind": "interval", "timezone": "UTC", "interval_seconds": 300})
	if tool.StatusCode != 201 {
		t.Fatalf("lead tool home create status=%d", tool.StatusCode)
	}
	tool.Body.Close()
	tool = f.toolRequest(t, toolPath, key)
	_ = json.NewDecoder(tool.Body).Decode(&listed)
	tool.Body.Close()
	if tool.StatusCode != 200 || len(listed.Schedules) != 3 {
		t.Fatalf("lead tool home list status=%d items=%d", tool.StatusCode, len(listed.Schedules))
	}

	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	ownerID := owner.ID
	dm, err := f.db.CreateRoom(t.Context(), store.Room{ProjectID: f.project.ID, Kind: "dm", DMKey: "schedule-private", Title: "private", CreatedBy: ownerID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(t.Context(), dm.ID, "user", ownerID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(t.Context(), dm.ID, "agent", lead.ID); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		resp = f.request(method, "/api/rooms/"+dm.ID+"/schedules", map[string]any{"target_lead": true, "body": "nope", "kind": "interval", "timezone": "UTC", "interval_seconds": 300}, true)
		var result map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if resp.StatusCode != 409 || result["error"] != "invalid_scope" {
			t.Fatalf("dm %s status=%d body=%v", method, resp.StatusCode, result)
		}
	}
	tool = f.toolPost(t, "/api/agent/rooms/"+dm.ID+"/schedules", key, "dm-schedule", map[string]any{"target_lead": true, "body": "nope", "kind": "interval", "timezone": "UTC", "interval_seconds": 300})
	if tool.StatusCode != 403 {
		t.Fatalf("lead tool dm status=%d", tool.StatusCode)
	}
	tool.Body.Close()
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.CreateSchedule(store.Schedule{RoomID: dm.ID, CreatorKind: "user", CreatorID: ownerID, TargetLead: true, Body: "nope", Kind: "interval", Timezone: "UTC", IntervalSeconds: 300}, time.Now())
		return err
	}); err == nil {
		t.Fatal("store accepted a schedule in a private chat")
	}
}

func TestCronScheduleHTTPAndToolValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	path := "/api/rooms/" + f.room.ID + "/schedules"
	resp := f.request(http.MethodPost, path, map[string]any{"target_lead": true, "body": "standup", "kind": "cron", "timezone": "Europe/Prague", "cron": "0 8,12,16,20 * * *"}, true)
	var created store.Schedule
	if resp.StatusCode != 201 || json.NewDecoder(resp.Body).Decode(&created) != nil {
		t.Fatalf("create cron status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	next, err := time.Parse(time.RFC3339, created.NextRunAt)
	prague, _ := time.LoadLocation("Europe/Prague")
	if err != nil || !next.After(time.Now()) || next.In(prague).Minute() != 0 || next.In(prague).Hour()%4 != 0 || next.Sub(time.Now()) > 12*time.Hour+time.Minute || created.Kind != "cron" || created.CronExpr != "0 8,12,16,20 * * *" {
		t.Fatalf("created=%+v next=%v err=%v", created, next, err)
	}
	resp = f.request(http.MethodGet, path, nil, false)
	var listed struct {
		Schedules []store.Schedule `json:"schedules"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if len(listed.Schedules) != 1 || listed.Schedules[0].CronExpr != created.CronExpr {
		t.Fatalf("listed=%+v", listed.Schedules)
	}
	for expr, field := range map[string]string{"": "expression", "60 * * * *": "minute", "0 25 * * *": "hour", "0 0 * 13 *": "month", "0 0 * * funday": "day-of-week", "* * * *": "5 fields"} {
		resp = f.request(http.MethodPost, path, map[string]any{"target_lead": true, "body": "bad", "kind": "cron", "timezone": "UTC", "cron": expr}, true)
		var body struct{ Error, Message string }
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != 400 || body.Error != "invalid_schedule" || !strings.Contains(body.Message, field) {
			t.Errorf("cron %q: status=%d body=%+v, want 400 naming %q", expr, resp.StatusCode, body, field)
		}
	}
	// The other kinds keep their short message.
	resp = f.request(http.MethodPost, path, map[string]any{"target_lead": true, "body": "bad", "kind": "interval", "timezone": "UTC", "interval_seconds": 60}, true)
	var body struct{ Error, Message string }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 400 || body.Message != "invalid schedule, target, time, or room" {
		t.Fatalf("interval refusal = %d %+v", resp.StatusCode, body)
	}
	// The lead tool accepts it, idempotently.
	_, key := f.createToolAgent(t, "role_leader_default")
	toolPath := "/api/agent/rooms/" + f.room.ID + "/schedules"
	toolBody := map[string]any{"target_lead": true, "body": "check", "kind": "cron", "timezone": "UTC", "cron": "@daily"}
	first := f.toolPost(t, toolPath, key, "cron-key", toolBody)
	var item store.Schedule
	_ = json.NewDecoder(first.Body).Decode(&item)
	first.Body.Close()
	if first.StatusCode != 201 || item.CronExpr != "@daily" || item.Kind != "cron" {
		t.Fatalf("tool create status=%d item=%+v", first.StatusCode, item)
	}
	replay := f.toolPost(t, toolPath, key, "cron-key", toolBody)
	replay.Body.Close()
	if replay.StatusCode != 200 {
		t.Fatalf("tool replay status=%d", replay.StatusCode)
	}
	refused := f.toolPost(t, toolPath, key, "cron-bad", map[string]any{"target_lead": true, "body": "check", "kind": "cron", "timezone": "UTC", "cron": "99 * * * *"})
	_ = json.NewDecoder(refused.Body).Decode(&body)
	refused.Body.Close()
	if refused.StatusCode != 400 || !strings.Contains(body.Message, "minute") {
		t.Fatalf("tool refusal = %d %+v", refused.StatusCode, body)
	}
}

func TestCronPreviewDescribesAndListsNextRuns(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	get := func(query string) (int, map[string]any) {
		resp := f.request(http.MethodGet, "/api/cron/preview?"+query, nil, false)
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	status, out := get(url.Values{"expr": {"0 8,12,16,20 * * *"}, "timezone": {"Europe/Prague"}}.Encode())
	next, _ := out["next"].([]any)
	if status != 200 || out["description"] != "at 08:00, 12:00, 16:00 and 20:00, every day" || len(next) != 5 || out["cron"] != "0 8,12,16,20 * * *" {
		t.Fatalf("preview = %d %+v", status, out)
	}
	prague, _ := time.LoadLocation("Europe/Prague")
	last := time.Now()
	for _, value := range next {
		at, err := time.Parse(time.RFC3339, value.(string))
		if err != nil || !at.After(last) || at.In(prague).Minute() != 0 {
			t.Fatalf("next runs = %v", next)
		}
		last = at
	}
	for query, want := range map[string]string{
		url.Values{"expr": {"61 * * * *"}, "timezone": {"UTC"}}.Encode():         "cron minute",
		url.Values{"expr": {"0 0 30 feb *"}, "timezone": {"UTC"}}.Encode():       "never",
		url.Values{"expr": {"* * * * *"}, "timezone": {"Nowhere/Zone"}}.Encode(): "time zone",
		url.Values{"expr": {"* * * * *"}}.Encode():                               "time zone",
	} {
		status, out := get(query)
		if message, _ := out["error"].(string); status != 200 || !strings.Contains(message, want) {
			t.Errorf("%s: %d %+v, want error containing %q", query, status, out, want)
		}
	}
}

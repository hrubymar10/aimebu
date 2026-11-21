package server

import (
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestUsageLimitRoutesReturnLocalDayAndRejectStaleRevision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	path := "/api/projects/" + f.project.ID + "/usage-limit"
	response := f.request(http.MethodGet, path, nil, false)
	if response.StatusCode != 200 {
		t.Fatalf("get status=%d", response.StatusCode)
	}
	var initial store.UsageLimit
	if err := json.NewDecoder(response.Body).Decode(&initial); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if initial.DayStart == "" || initial.TZ == "" || initial.Rev != 0 || initial.CostCoverage != "unknown" {
		t.Fatalf("initial=%+v", initial)
	}
	put := map[string]any{"daily_cost_cap": 2.5, "daily_turn_cap": 4, "rev": 0}
	response = f.request(http.MethodPut, path, put, true)
	if response.StatusCode != 200 {
		t.Fatalf("put status=%d", response.StatusCode)
	}
	var updated store.UsageLimit
	if err := json.NewDecoder(response.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if updated.Rev != 1 || updated.DailyCostCap == nil || *updated.DailyCostCap != 2.5 || updated.DailyTurnCap == nil || *updated.DailyTurnCap != 4 {
		t.Fatalf("updated=%+v", updated)
	}
	response = f.request(http.MethodPut, path, put, true)
	response.Body.Close()
	if response.StatusCode != 409 {
		t.Fatalf("stale status=%d", response.StatusCode)
	}
}

func TestProjectLimitModesAndTheGlobalDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	cookie, csrf := registerSecondUser(t, f, "second")
	// The default starts empty, anyone signed in may read it, and only the owner may change it.
	response := f.request(http.MethodGet, "/api/usage-limit-default", nil, false)
	var def store.UsageLimitDefault
	json.NewDecoder(response.Body).Decode(&def)
	response.Body.Close()
	if response.StatusCode != 200 || def.DailyCostCap != nil || def.DailyTurnCap != nil {
		t.Fatalf("initial default: %d %+v", response.StatusCode, def)
	}
	response = f.requestAs(t, cookie, csrf, http.MethodPut, "/api/usage-limit-default", map[string]any{"daily_cost_cap": 5})
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatalf("a non-owner changed the default: %d", response.StatusCode)
	}
	for _, bad := range []map[string]any{{"daily_cost_cap": 0}, {"daily_cost_cap": -2}, {"daily_turn_cap": 0}} {
		response = f.request(http.MethodPut, "/api/usage-limit-default", bad, true)
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("%v: %d", bad, response.StatusCode)
		}
	}
	response = f.request(http.MethodPut, "/api/usage-limit-default", map[string]any{"daily_cost_cap": 7.5, "daily_turn_cap": 30}, true)
	json.NewDecoder(response.Body).Decode(&def)
	response.Body.Close()
	if response.StatusCode != 200 || def.DailyCostCap == nil || *def.DailyCostCap != 7.5 || *def.DailyTurnCap != 30 {
		t.Fatalf("saved default: %d %+v", response.StatusCode, def)
	}
	// A project inherits it and reports both the cap in force and the inherited value.
	path := "/api/projects/" + f.project.ID + "/usage-limit"
	response = f.request(http.MethodGet, path, nil, false)
	var limit store.UsageLimit
	json.NewDecoder(response.Body).Decode(&limit)
	response.Body.Close()
	if limit.CostMode != "inherit" || limit.TurnMode != "inherit" || *limit.DailyCostCap != 7.5 || *limit.InheritedTurnCap != 30 {
		t.Fatalf("inherited project: %+v", limit)
	}
	// Modes: cost custom, turns none.
	response = f.request(http.MethodPut, path, map[string]any{"cost_mode": "custom", "daily_cost_cap": 2, "turn_mode": "none", "rev": limit.Rev}, true)
	json.NewDecoder(response.Body).Decode(&limit)
	response.Body.Close()
	if response.StatusCode != 200 || limit.CostMode != "custom" || *limit.DailyCostCap != 2 || limit.TurnMode != "none" || limit.DailyTurnCap != nil || *limit.InheritedTurnCap != 30 {
		t.Fatalf("modes: %d %+v", response.StatusCode, limit)
	}
	// A mode that contradicts its value is refused.
	response = f.request(http.MethodPut, path, map[string]any{"cost_mode": "custom", "turn_mode": "none", "rev": limit.Rev}, true)
	response.Body.Close()
	if response.StatusCode != 409 {
		t.Fatalf("custom without a value: %d", response.StatusCode)
	}
	// The room routes are gone: limits are per project.
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		response = f.request(method, "/api/rooms/"+f.room.ID+"/usage-limit", map[string]any{"daily_cost_cap": 1, "rev": 0}, true)
		response.Body.Close()
		if response.StatusCode != 404 && response.StatusCode != 405 {
			t.Fatalf("%s the room route answered %d", method, response.StatusCode)
		}
	}
}

package server

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
)

func TestRetentionSettingsDefaults(t *testing.T) {
	_, srv := setupTestServer(t)

	resp, err := http.Get(srv.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings: expected 200, got %d", resp.StatusCode)
	}

	var set Settings
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		t.Fatal(err)
	}

	if set.StaleAgentWindowSeconds == nil || *set.StaleAgentWindowSeconds != defaultStaleAgentWindowSeconds {
		t.Fatalf("stale_agent_window_seconds default = %v, want %d", set.StaleAgentWindowSeconds, defaultStaleAgentWindowSeconds)
	}
	if set.LivenessSweepSeconds == nil || *set.LivenessSweepSeconds != defaultLivenessSweepSeconds {
		t.Fatalf("liveness_sweep_seconds default = %v, want %d", set.LivenessSweepSeconds, defaultLivenessSweepSeconds)
	}
	if set.AgentStaleWindowSeconds == nil || *set.AgentStaleWindowSeconds != defaultAgentStaleWindowSeconds {
		t.Fatalf("agent_stale_window_seconds default = %v, want %d", set.AgentStaleWindowSeconds, defaultAgentStaleWindowSeconds)
	}
	if set.AgentOfflineWindowSeconds == nil || *set.AgentOfflineWindowSeconds != defaultAgentOfflineWindowSeconds {
		t.Fatalf("agent_offline_window_seconds default = %v, want %d", set.AgentOfflineWindowSeconds, defaultAgentOfflineWindowSeconds)
	}
	if set.EmptyRoomWindowSeconds == nil || *set.EmptyRoomWindowSeconds != defaultEmptyRoomWindowSeconds {
		t.Fatalf("empty_room_window_seconds default = %v, want %d", set.EmptyRoomWindowSeconds, defaultEmptyRoomWindowSeconds)
	}
	if set.CleanupIntervalSeconds == nil || *set.CleanupIntervalSeconds != defaultCleanupIntervalSeconds {
		t.Fatalf("cleanup_interval_seconds default = %v, want %d", set.CleanupIntervalSeconds, defaultCleanupIntervalSeconds)
	}
	if set.MessageRetentionSeconds == nil || *set.MessageRetentionSeconds != defaultMessageRetentionSeconds {
		t.Fatalf("message_retention_seconds default = %v, want %d", set.MessageRetentionSeconds, defaultMessageRetentionSeconds)
	}
	if set.MessageRetentionCount == nil || *set.MessageRetentionCount != defaultMessageRetentionCount {
		t.Fatalf("message_retention_count default = %v, want %d", set.MessageRetentionCount, defaultMessageRetentionCount)
	}
}

func TestRetentionSettingsRoundTrip(t *testing.T) {
	_, srv := setupTestServer(t)

	body := bytes.NewBufferString(`{
		"stale_agent_window_seconds": 600,
		"liveness_sweep_seconds": 20,
		"agent_stale_window_seconds": 120,
		"agent_offline_window_seconds": 600,
		"empty_room_window_seconds": 900,
		"cleanup_interval_seconds": 15,
		"message_retention_seconds": 120,
		"message_retention_count": 42
	}`)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/settings", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /settings: expected 200, got %d", resp.StatusCode)
	}

	var set Settings
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		t.Fatal(err)
	}

	assertIntPtr(t, "stale_agent_window_seconds", set.StaleAgentWindowSeconds, 600)
	assertIntPtr(t, "liveness_sweep_seconds", set.LivenessSweepSeconds, 20)
	assertIntPtr(t, "agent_stale_window_seconds", set.AgentStaleWindowSeconds, 120)
	assertIntPtr(t, "agent_offline_window_seconds", set.AgentOfflineWindowSeconds, 600)
	assertIntPtr(t, "empty_room_window_seconds", set.EmptyRoomWindowSeconds, 900)
	assertIntPtr(t, "cleanup_interval_seconds", set.CleanupIntervalSeconds, 15)
	assertIntPtr(t, "message_retention_seconds", set.MessageRetentionSeconds, 120)
	assertIntPtr(t, "message_retention_count", set.MessageRetentionCount, 42)
}

func TestRetentionSettingsValidation(t *testing.T) {
	_, srv := setupTestServer(t)

	for _, tc := range []struct {
		name  string
		body  string
		field string
	}{
		{name: "stale agent below floor", body: `{"stale_agent_window_seconds":59}`, field: "stale_agent_window_seconds"},
		{name: "liveness sweep below floor", body: `{"liveness_sweep_seconds":0}`, field: "liveness_sweep_seconds"},
		{name: "agent stale below floor", body: `{"agent_stale_window_seconds":9}`, field: "agent_stale_window_seconds"},
		{name: "agent offline below floor", body: `{"agent_offline_window_seconds":9}`, field: "agent_offline_window_seconds"},
		{name: "agent stale not below offline", body: `{"agent_stale_window_seconds":300,"agent_offline_window_seconds":300}`, field: "agent_stale_window_seconds"},
		{name: "empty room above ceiling", body: `{"empty_room_window_seconds":2592001}`, field: "empty_room_window_seconds"},
		{name: "cleanup interval below floor", body: `{"cleanup_interval_seconds":9}`, field: "cleanup_interval_seconds"},
		{name: "message seconds below floor", body: `{"message_retention_seconds":1}`, field: "message_retention_seconds"},
		{name: "message count below floor", body: `{"message_retention_count":-1}`, field: "message_retention_count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPut, srv.URL+"/settings", bytes.NewBufferString(tc.body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", resp.StatusCode)
			}
			var payload struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains([]byte(payload.Error), []byte(tc.field)) {
				t.Fatalf("error %q does not mention %q", payload.Error, tc.field)
			}
		})
	}
}

func TestRetentionSettingsAllowUnlimitedMessages(t *testing.T) {
	_, srv := setupTestServer(t)

	body := bytes.NewBufferString(`{"message_retention_seconds":0,"message_retention_count":0}`)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/settings", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for unlimited message retention, got %d", resp.StatusCode)
	}
}

func assertIntPtr(t *testing.T, name string, got *int, want int) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %d", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %d, want %d", name, *got, want)
	}
}

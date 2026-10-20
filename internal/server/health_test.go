package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestHealthEndpointShowsDisconnectedSupervisorInRoomAndOverall(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	status, card, raw := f.spawnRequest(t, "health-spawn", "worker")
	if status != http.StatusCreated || card.ID == "" {
		t.Fatalf("spawn=%d %+v", status, raw)
	}
	read := func() []healthAgent {
		t.Helper()
		response := f.request("GET", "/api/health", nil, false)
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("health status=%d", response.StatusCode)
		}
		var body struct {
			Agents []healthAgent `json:"agents"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Agents
	}
	if agents := read(); len(agents) != 1 || len(agents[0].Flags) != 0 {
		t.Fatalf("connected agents=%+v", agents)
	}
	f.srv.machineMu.Lock()
	delete(f.srv.machines, f.machineID)
	f.srv.machineMu.Unlock()
	if agents := read(); len(agents) != 1 || len(agents[0].Flags) != 1 || agents[0].Flags[0].Code != "machine_offline" || len(agents[0].RoomIDs) == 0 {
		t.Fatalf("offline agents=%+v", agents)
	}
	response := f.request("GET", "/api/rooms/"+f.room.ID+"/agents", nil, false)
	defer response.Body.Close()
	var cards []struct {
		store.AgentCard
		HealthFlags []store.HealthFlag `json:"health_flags"`
	}
	if err := json.NewDecoder(response.Body).Decode(&cards); err != nil || len(cards) != 1 || len(cards[0].HealthFlags) != 1 || cards[0].HealthFlags[0].Code != "machine_offline" {
		t.Fatalf("room cards=%+v error=%v", cards, err)
	}
	unauthorized, err := http.Get(f.http.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.StatusCode)
	}
}

func TestStuckThresholdConfiguration(t *testing.T) {
	if got := stuckAfter().Minutes(); got != 10 {
		t.Fatalf("default threshold=%v", got)
	}
	t.Setenv("AIMEBU2_STUCK_MINUTES", "3")
	if got := stuckAfter().Minutes(); got != 3 {
		t.Fatalf("configured threshold=%v", got)
	}
	t.Setenv("AIMEBU2_STUCK_MINUTES", "0")
	if got := stuckAfter().Minutes(); got != 10 {
		t.Fatalf("invalid threshold=%v", got)
	}
}

func TestProviderLimitRetryNowEndpoint(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	status, card, raw := f.spawnRequest(t, "quota-spawn", "worker")
	if status != 201 || card.ID == "" {
		t.Fatalf("spawn=%d %+v", status, raw)
	}
	created := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`UPDATE agent SET tool='codex' WHERE id=?`, []any{card.ID}},
			{`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,outcome,settle)
				SELECT 'turn_quota_api',?,s.id,0,?,1,'normal','exited','failed','settled' FROM session s WHERE s.agent_id=? LIMIT 1`, []any{card.ID, f.room.ID, card.ID}},
			{`INSERT INTO provider_limit(agent_id,turn_id,detected_at,tool) VALUES (?,'turn_quota_api',?,'codex')`, []any{card.ID, created}},
			{`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
				VALUES ('inb_quota_api',?,?, 'turn_quota_api',?,'system','provider_limit','usage_info','{"text":"Quota reached","options":["Retry now"]}',?)`, []any{f.project.ID, f.room.ID, card.ID, created}},
		} {
			if _, err := tx.Exec(statement.query, statement.args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	call := func(key string) int {
		t.Helper()
		body := []byte(`{"choice":"retry","rev":0}`)
		request, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/inbox/inb_quota_api/decision", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CSRF-Token", f.csrf)
		request.Header.Set("Idempotency-Key", key)
		request.AddCookie(f.cookie)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if status := call("quota-retry"); status != 201 {
		t.Fatalf("first decision=%d", status)
	}
	if status := call("quota-retry"); status != 200 {
		t.Fatalf("replay decision=%d", status)
	}
	response := f.request("GET", "/api/health", nil, false)
	defer response.Body.Close()
	var report struct {
		Agents []healthAgent `json:"agents"`
	}
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil || len(report.Agents) != 1 || len(report.Agents[0].Flags) != 0 {
		t.Fatalf("health after retry=%+v error=%v", report, err)
	}
}

package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/quota"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/switcher"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
)

// A no-wait switch A to B while a host agent's turn runs on A, and the turn then ends on A's usage limit: B stays usable (not limited), the agent
// is not paused on B, and the automatic switcher does not switch off B.
func TestATurnThatSpansASwitchAndMeetsTheOldLoginsLimitLeavesTheNewLoginUsable(t *testing.T) {
	f, m, _ := quotaEnv(t)
	f.seed()
	ctx := context.Background()
	if got := f.srv.Store.HostLogin(switcher.ToolClaude); got != "A" {
		t.Fatalf("the store reads the host's claude login as %q, want A", got)
	}
	host, err := f.srv.Store.HostMachine(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.machineMu.Lock()
	if f.srv.machines[f.machineID] == nil {
		f.srv.machines[f.machineID] = &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
			capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	}
	f.srv.machineMu.Unlock()
	status, card, raw := f.spawnRequest(t, "span-claude", "worker")
	if status != 201 || card.ID == "" {
		t.Fatalf("spawn=%d %+v", status, raw)
	}
	// The turn runs on A: it was admitted while A was live, so its login is A.
	turnID := "turn_span_claude"
	var generation int
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE agent SET tool='claude', machine_id=? WHERE id=?`, host.ID, card.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,login)
			SELECT ?,a.id,s.id,a.generation,?,1,'normal','running',? FROM agent a JOIN session s ON s.agent_id=a.id WHERE a.id=? LIMIT 1`,
			turnID, f.room.ID, f.srv.Store.HostLogin(switcher.ToolClaude), card.ID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if runtime, err := f.srv.Store.AgentRuntimeByID(ctx, card.ID); err != nil {
		t.Fatal(err)
	} else {
		generation = runtime.Generation
	}

	// The switch does not wait for the running turn.
	resp := f.request(http.MethodPost, "/api/switcher/claude/use", map[string]string{"name": "B"}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 || activeProfile(t, m) != "B" {
		t.Fatalf("status=%d active=%s", resp.StatusCode, activeProfile(t, m))
	}
	// The turn ends on A's limit.
	if err := f.srv.Store.RecordProcessInfo(ctx, turnID, card.ID, generation, adapter.ExitInfo{ExitCode: 1, StderrTail: "5-hour limit reached - resets in 3 hours"}); err != nil {
		t.Fatal(err)
	}
	if limited, err := f.srv.Store.AgentProviderLimited(ctx, card.ID); err != nil || limited {
		t.Fatalf("the agent is paused on B by A's limit: %v %v", limited, err)
	}
	if f.srv.hostToolLimited(ctx, switcher.ToolClaude) {
		t.Fatal("A's limit marks B limited")
	}

	// The real source: B is active and not limited.
	exp := time.Now().Add(time.Hour).UTC()
	snapshot := usages.Response{Providers: []usages.Provider{{ProviderName: "claude-code", Profiles: []usages.Profile{
		{ProfileName: "A", Status: "ok", HasCredentials: true, ExpiresAt: &exp, LastRefreshAt: &exp, Windows: []usages.Window{{Key: "session", PercentUsed: 100}, {Key: "weekly", PercentUsed: 40}}},
		{ProfileName: "B", Active: true, Status: "ok", HasCredentials: true, ExpiresAt: &exp, LastRefreshAt: &exp, Windows: []usages.Window{{Key: "session", PercentUsed: 1}, {Key: "weekly", PercentUsed: 5}}},
	}}}}
	dir := t.TempDir()
	data, _ := json.Marshal(snapshot)
	if err := os.WriteFile(filepath.Join(dir, "cache.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := usages.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.useUsages(reader)
	obs, err := quotaSource{f.srv}.Observe(ctx, switcher.ToolClaude)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range obs.Profiles {
		if p.Limited {
			t.Fatalf("profile %s reads limited: %+v", p.Name, obs)
		}
	}
	if obs.Active != "B" {
		t.Fatalf("active=%s", obs.Active)
	}
	runner, _ := runnerOver(t, f, quotaSource{f.srv})
	quotaConfig(t, runner, nil)
	runner.Evaluate(ctx)
	if got := activeProfile(t, m); got != "B" {
		t.Fatalf("the runner switched off a healthy B: active=%s", got)
	}
	_, o, _ := getQuota(t, f)
	if last := o.Tools["claude"].LastDecision; last != nil && last.Action == quota.ActionSwitch {
		t.Fatalf("last decision = %+v", last)
	}
	f.srv.WaitUsageRefreshes()
}

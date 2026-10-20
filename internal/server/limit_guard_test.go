package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/quota"
)

func guardRequest(t *testing.T, f *fixture, method, path string, body any) (int, string) {
	t.Helper()
	resp := f.request(method, path, body, method != "GET")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

func TestLimitGuardRoutesAre503UntilRunningOwnerOnlyAndValidate(t *testing.T) {
	f, _, _ := quotaEnv(t)
	if code, _ := guardRequest(t, f, "GET", "/api/limit-guard", nil); code != 503 {
		t.Fatalf("before the guard: %d, want 503", code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.srv.StartQuotaSwitcher(ctx)
	code, raw := guardRequest(t, f, "GET", "/api/limit-guard", nil)
	if code != 200 || !strings.Contains(raw, `"soft":95`) || !strings.Contains(raw, `"soft":98`) || !strings.Contains(raw, `"hard":99`) || !strings.Contains(raw, `"saved":false`) {
		t.Fatalf("overview: %d %s", code, raw)
	}
	anon, err := http.Get(f.http.URL + "/api/limit-guard")
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != 401 {
		t.Fatalf("anonymous = %d, want 401", anon.StatusCode)
	}
	good := quota.GuardConfig{Enabled: true, Soft: 80, Hard: 90, Profiles: map[string]quota.GuardLimit{"A": {Soft: 50, Hard: 60}}}
	if code, body := guardRequest(t, f, "PUT", "/api/limit-guard/claude", good); code != 200 || !strings.Contains(body, `"saved":true`) {
		t.Fatalf("save: %d %s", code, body)
	}
	if code, raw = guardRequest(t, f, "GET", "/api/limit-guard", nil); code != 200 || !strings.Contains(raw, `"soft":80`) || !strings.Contains(raw, `"A":{"soft":50,"hard":60}`) {
		t.Fatalf("saved config not read back: %d %s", code, raw)
	}
	for name, bad := range map[string]quota.GuardConfig{
		"soft above hard": {Enabled: true, Soft: 95, Hard: 90},
		"out of range":    {Enabled: true, Soft: 0, Hard: 90},
		"unknown profile": {Enabled: true, Soft: 80, Hard: 90, Profiles: map[string]quota.GuardLimit{"ghost": {Soft: 10, Hard: 20}}},
	} {
		if code, body := guardRequest(t, f, "PUT", "/api/limit-guard/claude", bad); code != 400 {
			t.Fatalf("%s: %d %s", name, code, body)
		}
	}
	if code, _ := guardRequest(t, f, "PUT", "/api/limit-guard/pi", good); code != 404 {
		t.Fatalf("unknown tool: %d", code)
	}
}

func TestLimitGuardSoftHoldsTheStoreAndHardIsAnOwnerEvent(t *testing.T) {
	f, _, _ := quotaEnv(t)
	src := &quotaFakeSource{}
	guard := quota.NewGuard(f.srv.Store, src, f.srv.Store, quotaEvents{f.srv})
	f.srv.guard.Store(guard)
	f.srv.Store.SetToolHold(quota.ToolClaude, "")
	src.set(quota.Observation{Active: "A", Profiles: []quota.Obs{qobs("A", 96, 10)}, Ready: true})
	guard.Evaluate(context.Background())
	if f.srv.Store.ToolHold(quota.ToolClaude) == "" {
		t.Fatal("at the soft threshold the store must hold new claude turns")
	}
	src.set(quota.Observation{Active: "A", Profiles: []quota.Obs{qobs("A", 99, 10)}, Ready: true})
	guard.Evaluate(context.Background())
	src.set(quota.Observation{Active: "A", Profiles: []quota.Obs{qobs("A", 5, 5)}, Ready: true})
	guard.Evaluate(context.Background())
	if f.srv.Store.ToolHold(quota.ToolClaude) != "" {
		t.Fatal("below the thresholds the hold must be released")
	}
	var kinds []string
	for _, ev := range systemEvents(t, f) {
		kinds = append(kinds, ev.Kind)
	}
	joined := strings.Join(kinds, ",")
	for _, want := range []string{"limit_guard_soft", "limit_guard_hard", "limit_guard_clear"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("events %v lack %s", kinds, want)
		}
	}
}

func TestLimitGuardSettingsTravelInTheSettingsBackup(t *testing.T) {
	source, _, _ := quotaEnv(t)
	guard := quota.NewGuard(source.srv.Store, &quotaFakeSource{}, source.srv.Store, nil)
	source.srv.guard.Store(guard)
	// An unsaved guard (defaults) exports nothing.
	if _, has := exportBackup(t, source)["limit_guard"]; has {
		t.Fatal("a guard on its defaults must not be in the backup")
	}
	saved := quota.GuardConfig{Enabled: true, Soft: 70, Hard: 85, Profiles: map[string]quota.GuardLimit{"B": {Soft: 50, Hard: 60}}}
	if _, err := guard.Save(context.Background(), quota.ToolClaude, saved); err != nil {
		t.Fatal(err)
	}
	backup := exportBackup(t, source)
	exported, _ := json.Marshal(backup["limit_guard"])
	if string(exported) != `{"claude":{"enabled":true,"hard":85,"profiles":{"B":{"hard":60,"soft":50}},"soft":70}}` {
		t.Fatalf("limit_guard section = %s", exported)
	}

	target, _, _ := quotaEnv(t)
	targetGuard := quota.NewGuard(target.srv.Store, &quotaFakeSource{}, target.srv.Store, nil)
	target.srv.guard.Store(targetGuard)
	status, preview := previewBackup(t, target, backup)
	if got := sectionOf(preview, "limit_guard"); status != 200 || got.New != 1 {
		t.Fatalf("preview: %d %+v", status, got)
	}
	resp := target.request("POST", "/api/settings-backup/apply", map[string]any{"backup": backup, "digest": preview.Digest}, true)
	var result backupResult
	decodeBackupBody(t, resp, &result)
	if resp.StatusCode != 200 || !result.Complete || len(result.Failed) != 0 {
		t.Fatalf("apply: %d %+v", resp.StatusCode, result)
	}
	if view, err := targetGuard.View(context.Background(), quota.ToolClaude); err != nil || !view.Saved || view.Config.Soft != 70 || view.Config.Profiles["B"].Hard != 60 {
		t.Fatalf("applied guard = %+v %v", view, err)
	}
	_, second := previewBackup(t, target, backup)
	if got := sectionOf(second, "limit_guard"); got.New != 0 || got.Overwrite != 0 || got.Unchanged != 1 {
		t.Fatalf("a second preview still has work: %+v", got)
	}
	// An instance without the guard says so instead of silently dropping the section.
	bare := newFixture(t)
	if _, preview := previewBackup(t, bare, backup); sectionOf(preview, "limit_guard").Unavailable == "" {
		t.Fatal("a missing guard must be reported as unavailable")
	}
}

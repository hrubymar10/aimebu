package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/supervisor"
)

func TestMachineHarnessSettingsValidateOnSupervisorAndAudit(t *testing.T) {
	t.Setenv("AIMEBU2_ALLOW_NATIVE", "1")
	f := newFixture(t)
	url := "/api/machines/" + f.machineID + "/harnesses"
	resp := f.request(http.MethodPut, url+"/codex/native", map[string]any{"enabled": true, "override_path": "/bin/echo"}, true)
	if resp.StatusCode != 503 {
		t.Fatalf("offline PUT=%d", resp.StatusCode)
	}
	resp.Body.Close()
	journal, err := supervisor.OpenJournal(context.Background(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, adapter.NewFake())
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(ctx) }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not connect")
	}
	resp = f.request(http.MethodGet, url, nil, false)
	var first struct {
		Harnesses []protocol.HarnessRow `json:"harnesses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(first.Harnesses) != 10 {
		t.Fatalf("harness rows=%d", len(first.Harnesses))
	}
	resp = f.request(http.MethodPut, url+"/codex/native", map[string]any{"enabled": true, "override_path": "relative/path"}, true)
	if resp.StatusCode != 400 {
		t.Fatalf("invalid path PUT=%d", resp.StatusCode)
	}
	resp.Body.Close()
	path := filepath.Join(t.TempDir(), "codex-test")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho codex-cli 1.2.3\n"), 0755); err != nil {
		t.Fatal(err)
	}
	resp = f.request(http.MethodPut, url+"/codex/native", map[string]any{"enabled": true, "override_path": path}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("valid path PUT=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodGet, url, nil, false)
	var after struct {
		Harnesses []protocol.HarnessRow `json:"harnesses"`
		Activity  []struct {
			NewPath string `json:"new_path"`
		} `json:"activity"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(after.Activity) != 1 || after.Activity[0].NewPath != path {
		t.Fatalf("activity=%+v", after.Activity)
	}
	found := false
	for _, row := range after.Harnesses {
		if row.Tool == "codex" && row.Variant == "native" {
			found = true
			if row.LauncherPath != path || !row.Available || row.Source != "user" {
				t.Fatalf("row=%+v", row)
			}
		}
	}
	if !found {
		t.Fatal("codex/native row missing")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop")
	}
}

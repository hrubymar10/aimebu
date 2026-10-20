package adapter_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/adapter/claude"
	"github.com/hrubymar10/aimebu/internal/adapter/codex"
	"github.com/hrubymar10/aimebu/internal/adapter/pi"
)

func TestMultiCatalogIncludesClaude(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '2.1.296\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	implementation := claude.New()
	implementation.Command = path
	multi := adapter.NewMulti(adapter.NewFake(), implementation)
	found := false
	for _, item := range multi.Catalog() {
		if item.Tool == "claude" && item.Version == "2.1.296" && item.Environment.Variant == "native" {
			found = true
			caps := multi.Capabilities(item.Version, item.Environment)
			if !caps.SchemaFlag || !caps.CompactionEvents || caps.NoActionMode || caps.ReadonlyVerified {
				t.Fatalf("caps %+v", caps)
			}
		}
	}
	if !found {
		t.Fatal("Claude Code missing from catalog")
	}
}

func TestMultiPiCapabilitiesAfterMountedLauncherProbe(t *testing.T) {
	multi := adapter.NewMulti(pi.NewDocker())
	env := adapter.Environment{Variant: "docker", OS: "linux", Container: true}
	verified := multi.CapabilitiesFor("pi", "docker", "1.1.0", env)
	if verified.SchemaFlag || !verified.NoActionMode || !verified.CompactionEvents || verified.ReadonlyVerified {
		t.Fatalf("Pi 1.1.0 capabilities=%+v", verified)
	}
	if got := multi.CapabilitiesFor("pi", "docker", "1.2.0", env); got != (adapter.Capabilities{}) {
		t.Fatalf("unverified Pi version gained capabilities: %+v", got)
	}
}

func TestMultiCatalogIncludesCodex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'codex-cli 0.162.1\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	implementation := codex.New(t.TempDir())
	implementation.Command = path
	multi := adapter.NewMulti(adapter.NewFake(), implementation)
	for _, item := range multi.Catalog() {
		if item.Tool == "codex" && item.Version == "0.162.1" && item.Environment.Variant == "native" {
			if caps := multi.Capabilities(item.Version, item.Environment); (runtime.GOOS == "linux" && !caps.SchemaFlag) || caps.ReadonlyVerified || caps.NoActionMode {
				t.Fatalf("Codex capabilities=%+v", caps)
			}
			return
		}
	}
	t.Fatal("Codex missing from catalog")
}

// slowTool stands in for an adapter whose Catalog runs a CLI to read its version.
type slowTool struct {
	tool  string
	delay time.Duration
	calls *atomic.Int32
}

func (s *slowTool) Start(context.Context, adapter.TurnSpec) (adapter.Turn, error) {
	return nil, errors.New("not used")
}
func (s *slowTool) Catalog() []adapter.CatalogEntry {
	s.calls.Add(1)
	time.Sleep(s.delay)
	return []adapter.CatalogEntry{{Tool: s.tool, Version: "1.0", Environment: adapter.Environment{Variant: "native"}}}
}
func (s *slowTool) Capabilities(string, adapter.Environment) adapter.Capabilities {
	return adapter.Capabilities{}
}

// The supervisor asks the catalog once per advertised entry, and each ask used to run every adapter's CLI again, one after the other:
// seconds at start, and the first-run setup waited for them. Each adapter is now asked once, all at the same time.
func TestMultiAsksEachAdapterOnceAndAtTheSameTime(t *testing.T) {
	var calls atomic.Int32
	adapters := make([]adapter.Adapter, 6)
	for i := range adapters {
		adapters[i] = &slowTool{tool: fmt.Sprintf("tool%d", i), delay: 300 * time.Millisecond, calls: &calls}
	}
	start := time.Now()
	multi := adapter.NewMulti(adapters...)
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("six adapters of 300ms took %v: they were asked one by one", elapsed)
	}
	for _, item := range multi.Catalog() {
		multi.Capabilities(item.Version, item.Environment)
	}
	multi.Catalog()
	if got := calls.Load(); got != 6 {
		t.Fatalf("adapters were asked %d times, want once each (6)", got)
	}
	if len(multi.Catalog()) != 6 {
		t.Fatalf("catalog: %+v", multi.Catalog())
	}
}

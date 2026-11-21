package adapter_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/claude"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/codex"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/pi"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func TestMultiCatalogIncludesClaude(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\nprintf '2.1.296\\n'\n"), 0700); err != nil {
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
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\nprintf 'codex-cli 0.162.1\\n'\n"), 0700); err != nil {
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

// slowTool stands in for an adapter whose Catalog runs a CLI to read its version. The first Catalog call
// of every tool waits until all of them are inside it, so the tools only get past that point when they were
// asked at the same time. The wait is bounded only so that a regression fails instead of hanging.
type slowTool struct {
	tool     string
	calls    *atomic.Int32
	entered  *atomic.Int32
	all      chan struct{} // closed when every tool has entered Catalog
	total    int32
	timedOut *atomic.Bool
}

func (s *slowTool) Start(context.Context, adapter.TurnSpec) (adapter.Turn, error) {
	return nil, errors.New("not used")
}
func (s *slowTool) Catalog() []adapter.CatalogEntry {
	if s.calls.Add(1) <= s.total { // only the first round of asks takes part in the rendezvous
		if s.entered.Add(1) == s.total {
			close(s.all)
		}
		select {
		case <-s.all:
		case <-time.After(10 * time.Second):
			s.timedOut.Store(true)
		}
	}
	return []adapter.CatalogEntry{{Tool: s.tool, Version: "1.0", Environment: adapter.Environment{Variant: "native"}}}
}
func (s *slowTool) Capabilities(string, adapter.Environment) adapter.Capabilities {
	return adapter.Capabilities{}
}

// The supervisor asks the catalog once per advertised entry, and each ask used to run every adapter's CLI again, one after the other:
// seconds at start, and the first-run setup waited for them. Each adapter is now asked once, all at the same time.
func TestMultiAsksEachAdapterOnceAndAtTheSameTime(t *testing.T) {
	const n = 6
	var calls, entered atomic.Int32
	var timedOut atomic.Bool
	all := make(chan struct{})
	adapters := make([]adapter.Adapter, n)
	for i := range adapters {
		adapters[i] = &slowTool{tool: fmt.Sprintf("tool%d", i), calls: &calls, entered: &entered, all: all, total: n, timedOut: &timedOut}
	}
	multi := adapter.NewMulti(adapters...)
	if timedOut.Load() {
		t.Fatal("the adapters were asked one by one: not all of them were inside Catalog at the same time")
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

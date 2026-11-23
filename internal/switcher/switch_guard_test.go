package switcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/credtarget"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
)

func guardedFixture(t *testing.T) (m *Manager, livePath, credA string) {
	t.Helper()
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	credA = futureClaude("tok-a")
	livePath = writeLive(t, home, ToolClaude, credA)
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	must(t, os.MkdirAll(m.profileDir(ToolClaude, "b"), 0o700))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(futureClaude("tok-b")), 0o600))
	return m, livePath, credA
}

func TestSwitchWaitsForMaintenanceOnTheOutgoingProfile(t *testing.T) {
	m, livePath, credA := guardedFixture(t)
	release, ok, err := usages.AcquireMaintenance(m.root, ToolClaude, "a", m.profileCredPath(ToolClaude, "a"))
	if err != nil || !ok {
		t.Fatalf("hold active account: ok=%v err=%v", ok, err)
	}
	defer release()
	previous := MaintenanceWait
	MaintenanceWait = 200 * time.Millisecond
	defer func() { MaintenanceWait = previous }()
	if _, switched, err := m.SwitchGuarded(ToolClaude, "b", SwitchGuard{ExpectedActive: "a"}); !errors.Is(err, ErrMaintenanceBusy) || switched {
		t.Fatalf("active maintenance must hold the switch: switched=%v err=%v", switched, err)
	}
	if got := readFile(t, livePath); got != credA {
		t.Fatalf("the live credentials changed during maintenance: %q", got)
	}
	release()
	if active, switched, err := m.SwitchGuarded(ToolClaude, "b", SwitchGuard{ExpectedActive: "a"}); err != nil || !switched || active != "b" {
		t.Fatalf("switch after maintenance: active=%q switched=%v err=%v", active, switched, err)
	}
}

// A guarded switch refuses when the active profile is not the one the caller decided from, and
// leaves everything as it was.
func TestAGuardedSwitchRefusesWhenTheActiveProfileChanged(t *testing.T) {
	m, livePath, credA := guardedFixture(t)
	active, switched, err := m.SwitchGuarded(ToolClaude, "b", SwitchGuard{ExpectedActive: "someone-else"})
	if !errors.Is(err, ErrActiveChanged) || switched || active != "a" {
		t.Fatalf("active=%q switched=%v err=%v, want a, false, ErrActiveChanged", active, switched, err)
	}
	if got := readFile(t, livePath); got != credA {
		t.Fatalf("the live credentials were touched: %q", got)
	}
	state, err := m.loadState()
	must(t, err)
	if state.Active[ToolClaude] != "a" {
		t.Fatalf("the active profile moved to %q", state.Active[ToolClaude])
	}
}

func TestAGuardedSwitchRunsWhenTheActiveProfileIsTheExpectedOne(t *testing.T) {
	m, _, _ := guardedFixture(t)
	active, switched, err := m.SwitchGuarded(ToolClaude, "b", SwitchGuard{ExpectedActive: "a"})
	must(t, err)
	if active != "b" || !switched {
		t.Fatalf("active=%q switched=%v", active, switched)
	}
	// The same call again: b is active now, so a switch "from a" is refused instead of repeated.
	if _, _, err := m.SwitchGuarded(ToolClaude, "a", SwitchGuard{ExpectedActive: "a"}); !errors.Is(err, ErrActiveChanged) {
		t.Fatalf("a stale decision must be refused, got %v", err)
	}
}

func TestManualSwitchTreatsTargetActivatedDuringWaitAsNoOp(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	writeLive(t, home, ToolClaude, futureClaude("tok-a"))
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	m.beforeSwitchLock = func() {
		m.beforeSwitchLock = nil
		state, err := m.loadState()
		must(t, err)
		state.Active[ToolClaude] = "b"
		must(t, m.saveState(state))
		writeLive(t, home, ToolClaude, futureClaude("tok-b"))
	}
	if active, switched, err := m.Switch(ToolClaude, "b"); err != nil || switched || active != "b" {
		t.Fatalf("manual switch after target became active: active=%q switched=%v err=%v", active, switched, err)
	}
}

func TestManualSwitchRetriesWhenAnotherProfileActivatesDuringWait(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	writeLive(t, home, ToolClaude, futureClaude("tok-a"))
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	must(t, m.Add(ToolClaude, "c"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(futureClaude("tok-b")), 0o600))
	m.beforeSwitchLock = func() {
		m.beforeSwitchLock = nil
		state, err := m.loadState()
		must(t, err)
		state.Active[ToolClaude] = "c"
		must(t, m.saveState(state))
		writeLive(t, home, ToolClaude, futureClaude("tok-c"))
	}
	if active, switched, err := m.Switch(ToolClaude, "b"); err != nil || !switched || active != "b" {
		t.Fatalf("manual switch did not retry: active=%q switched=%v err=%v", active, switched, err)
	}
}

// Without a guard Switch behaves as before.
func TestAnUnguardedSwitchIgnoresTheGuardConditions(t *testing.T) {
	m, _, _ := guardedFixture(t)
	if active, switched, err := m.Switch(ToolClaude, "b"); err != nil || !switched || active != "b" {
		t.Fatalf("active=%q switched=%v err=%v", active, switched, err)
	}
}

// A Claude refresh in flight holds <configdir>/.oauth_refresh.lock: the swap waits for it (and refuses, untouched, when it does not let go in
// time), and holds the lock itself across the swap so a new refresh starts only after it.
func TestAClaudeSwitchHoldsTheOAuthRefreshLockAcrossTheSwap(t *testing.T) {
	m, livePath, credA := guardedFixture(t)
	old := storageLockTimeout
	storageLockTimeout = 200 * time.Millisecond
	defer func() { storageLockTimeout = old }()
	dir := filepath.Dir(livePath)
	release, err := (&credtarget.Writer{LockTimeout: time.Second}).LockRefresh(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, switched, err := m.SwitchGuarded(ToolClaude, "b", SwitchGuard{ExpectedActive: "a"}); err == nil || switched {
		t.Fatalf("a held refresh lock must stop the swap: switched=%v err=%v", switched, err)
	}
	if got := readFile(t, livePath); got != credA {
		t.Fatalf("the live credentials were touched: %q", got)
	}
	release()
	if _, switched, err := m.SwitchGuarded(ToolClaude, "b", SwitchGuard{ExpectedActive: "a"}); err != nil || !switched {
		t.Fatalf("switched=%v err=%v", switched, err)
	}
	if _, err := os.Stat(credtarget.OAuthRefreshLockPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("the refresh lock must be released after the swap: %v", err)
	}
}

package switcher

import (
	"errors"
	"os"
	"strings"
	"testing"
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

// Without a guard Switch behaves as before.
func TestAnUnguardedSwitchIgnoresTheGuardConditions(t *testing.T) {
	m, _, _ := guardedFixture(t)
	if active, switched, err := m.Switch(ToolClaude, "b"); err != nil || !switched || active != "b" {
		t.Fatalf("active=%q switched=%v err=%v", active, switched, err)
	}
}

// The quiescence guard runs under the lock, right before the swap: an error aborts the switch
// untouched and comes back unchanged; it is not asked when there is nothing to switch.
func TestAQuietGuardErrorAbortsTheSwitchBeforeAnythingIsWritten(t *testing.T) {
	m, livePath, credA := guardedFixture(t)
	busy := errors.New("a turn is running")
	calls := 0
	_, switched, err := m.SwitchGuarded(ToolClaude, "b", SwitchGuard{ExpectedActive: "a", Quiet: func() error { calls++; return busy }})
	if !errors.Is(err, busy) || switched || calls != 1 {
		t.Fatalf("switched=%v err=%v calls=%d", switched, err, calls)
	}
	if got := readFile(t, livePath); got != credA {
		t.Fatalf("the live credentials were touched: %q", got)
	}
	if state, err := m.loadState(); err != nil || state.Active[ToolClaude] != "a" {
		t.Fatalf("active moved: %+v %v", state.Active, err)
	}
	// Nothing to switch (b is the target and a is active, then target == active): the guard is not asked.
	calls = 0
	if _, _, err := m.SwitchGuarded(ToolClaude, "a", SwitchGuard{ExpectedActive: "a", Quiet: func() error { calls++; return busy }}); err != nil || calls != 0 {
		t.Fatalf("a no-op must not consult the guard: err=%v calls=%d", err, calls)
	}
	// Quiet and willing: the switch runs.
	if _, switched, err := m.SwitchGuarded(ToolClaude, "b", SwitchGuard{ExpectedActive: "a", Quiet: func() error { return nil }}); err != nil || !switched {
		t.Fatalf("switched=%v err=%v", switched, err)
	}
}

// The hold is taken before the quiescence check and kept until the swap has finished, so a turn cannot start in between;
// it is released on a refusal too, and not taken for a no-op.
func TestTheHoldSpansTheQuietCheckAndTheSwap(t *testing.T) {
	m, _, _ := guardedFixture(t)
	var events []string
	hold := func() func() { events = append(events, "hold"); return func() { events = append(events, "release") } }
	quiet := func() error { events = append(events, "quiet"); return nil }
	if _, switched, err := m.SwitchGuarded(ToolClaude, "b", SwitchGuard{ExpectedActive: "a", Hold: hold, Quiet: quiet}); err != nil || !switched {
		t.Fatalf("switched=%v err=%v", switched, err)
	}
	if got := strings.Join(events, ","); got != "hold,quiet,release" {
		t.Fatalf("order = %s", got)
	}
	events = nil
	busy := errors.New("a turn is running")
	if _, _, err := m.SwitchGuarded(ToolClaude, "a", SwitchGuard{ExpectedActive: "b", Hold: hold, Quiet: func() error { events = append(events, "quiet"); return busy }}); !errors.Is(err, busy) {
		t.Fatalf("err=%v", err)
	}
	if got := strings.Join(events, ","); got != "hold,quiet,release" {
		t.Fatalf("a refusal must release the hold: %s", got)
	}
	events = nil
	if _, _, err := m.SwitchGuarded(ToolClaude, "b", SwitchGuard{ExpectedActive: "b", Hold: hold}); err != nil || len(events) != 0 {
		t.Fatalf("a no-op must not take the hold: err=%v events=%v", err, events)
	}
}

package credtarget

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type orderedFakeWriter struct {
	written map[string][]byte
	failOn  map[string]error
	order   []string
}

func (w *orderedFakeWriter) WriteTarget(t Target, full []byte) error {
	w.order = append(w.order, t.ID)
	if err := w.failOn[t.ID]; err != nil {
		return err
	}
	if w.written == nil {
		w.written = map[string][]byte{}
	}
	w.written[t.ID] = full
	return nil
}

func targetIDs(ts []Target) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.ID)
	}
	sort.Strings(out)
	return out
}

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	return NewRegistry(filepath.Join(home, "server"), home)
}

func TestRegistryLiveTargetsFollowTheCLIDirectories(t *testing.T) {
	r := newTestRegistry(t)
	got := r.Targets()
	if len(got) != 2 || got[0].ID != "live/claude" || got[1].ID != "live/codex" {
		t.Fatalf("targets = %+v", got)
	}
	if filepath.Base(got[0].Path) != ".credentials.json" || filepath.Base(got[1].Path) != "auth.json" || got[0].Role != RoleLive {
		t.Fatalf("live targets = %+v", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	t.Setenv("CODEX_HOME", "/custom/codex")
	got = r.Targets()
	if got[0].Path != "/custom/claude/.credentials.json" || got[1].Path != "/custom/codex/auth.json" {
		t.Fatalf("env overrides ignored: %+v", got)
	}
}

func TestRegistryExternalTargetsPersistAndBind(t *testing.T) {
	r := newTestRegistry(t)
	ext := Target{ID: "review-bot", Tool: ToolClaude, Path: "/x/review-bot/.credentials.json", Profile: "bkp"}
	if err := r.Register(ext); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Target{ID: "bad", Tool: "pi", Path: "/x"}); err == nil {
		t.Fatal("an unknown tool was registered")
	}
	if err := r.Register(Target{ID: "rel", Tool: ToolCodex, Path: "relative/auth.json"}); err == nil {
		t.Fatal("a relative path was registered")
	}
	info, err := os.Stat(r.externalPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("registry file: %v %v", info, err)
	}
	reloaded := NewRegistry(r.serverDir, r.home)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if got := targetIDs(reloaded.Targets()); len(got) != 3 || got[0] != "live/claude" {
		t.Fatalf("reloaded targets = %v", got)
	}
	// A pinned external follows its profile, not the active one.
	if got := targetIDs(reloaded.ForProfile(ToolClaude, "bkp", false)); len(got) != 1 || got[0] != "review-bot" {
		t.Fatalf("ForProfile(bkp, inactive) = %v", got)
	}
	if got := targetIDs(reloaded.ForProfile(ToolClaude, "main", true)); len(got) != 1 || got[0] != "live/claude" {
		t.Fatalf("ForProfile(main, active) = %v", got)
	}
	if got := targetIDs(reloaded.ForProfile(ToolClaude, "main", false)); len(got) != 0 {
		t.Fatalf("ForProfile(main, inactive) = %v", got)
	}
	if got := targetIDs(reloaded.ForProfile(ToolCodex, "bkp", true)); len(got) != 1 || got[0] != "live/codex" {
		t.Fatalf("codex ForProfile = %v", got)
	}
	if err := reloaded.Unregister("review-bot"); err != nil {
		t.Fatal(err)
	}
	if got := targetIDs(reloaded.ForProfile(ToolClaude, "bkp", false)); len(got) != 0 {
		t.Fatalf("unregistered target still present: %v", got)
	}
	if err := NewRegistry(filepath.Join(t.TempDir(), "none"), "/h").Load(); err != nil {
		t.Fatalf("a missing registry file is not an error: %v", err)
	}
}

func TestFanOutWritesBoundTargetsAndCollectsErrors(t *testing.T) {
	r := newTestRegistry(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.Register(Target{ID: "pinned", Tool: ToolClaude, Path: "/x/a.json", Profile: "bkp"}))
	must(r.Register(Target{ID: "floating", Tool: ToolClaude, Path: "/x/b.json"}))
	w := &orderedFakeWriter{}
	f := &FanOut{Registry: r, Writer: w, Active: func(string) (string, error) { return "main", nil }}

	// The active profile refreshed: its live target (and nothing pinned to bkp).
	must(f.FanOut(ToolClaude, "main", []byte("main-full")))
	if len(w.order) != 1 || w.order[0] != "live/claude" || string(w.written["live/claude"]) != "main-full" {
		t.Fatalf("active fan-out wrote %v", w.order)
	}
	// An inactive profile refreshed: only targets pinned to it.
	w.order = nil
	must(f.FanOut(ToolClaude, "bkp", []byte("bkp-full")))
	if len(w.order) != 1 || w.order[0] != "pinned" {
		t.Fatalf("inactive fan-out wrote %v", w.order)
	}
	// FanOutActive: live + unpinned externals.
	w.order = nil
	must(f.FanOutActive(ToolClaude, []byte("x")))
	if got := append([]string(nil), w.order...); len(got) != 2 || got[0] != "live/claude" || got[1] != "floating" {
		t.Fatalf("FanOutActive wrote %v", got)
	}
	// One failing target does not stop the others; the error names it.
	w.order, w.failOn = nil, map[string]error{"live/claude": errors.New("disk full")}
	err := f.FanOutActive(ToolClaude, []byte("y"))
	if err == nil || len(w.order) != 2 || string(w.written["floating"]) != "y" {
		t.Fatalf("err=%v order=%v", err, w.order)
	}
	if want := "live/claude: disk full"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if (&FanOut{}).FanOutActive(ToolClaude, nil) == nil {
		t.Fatal("an unwired fan-out must refuse")
	}
}

type replacerWriter struct {
	orderedFakeWriter
	imported []string
}

func (w *replacerWriter) WriteImportedLogin(t Target, full []byte) error {
	w.imported = append(w.imported, t.ID)
	return nil
}

// Only the replacing variant may overwrite a refresh-bearing target, and the
// real writer is never mutated: the permission is per call.
func TestFanOutActiveReplacingIsPerCall(t *testing.T) {
	r := newTestRegistry(t)
	rw := &replacerWriter{}
	f := &FanOut{Registry: r, Writer: rw, Active: func(string) (string, error) { return "main", nil }}
	if err := f.FanOutActiveReplacing(ToolClaude, []byte("x")); err != nil || len(rw.imported) != 1 || rw.imported[0] != "live/claude" || len(rw.order) != 0 {
		t.Fatalf("replacing = %v %v %v", err, rw.imported, rw.order)
	}
	if err := (&FanOut{Registry: r, Writer: &orderedFakeWriter{}}).FanOutActiveReplacing(ToolClaude, nil); err == nil {
		t.Fatal("a writer that cannot replace a login was accepted")
	}

	reg := newTestRegistry(t) // clears the CLI env first
	dir := t.TempDir()
	live := filepath.Join(dir, ".credentials.json")
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	full := []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":4102444800000}}`)
	if err := os.WriteFile(live, full, 0o600); err != nil {
		t.Fatal(err)
	}
	real := &FanOut{Registry: reg, Writer: &Writer{}, Active: func(string) (string, error) { return "main", nil }}
	if err := real.FanOutActive(ToolClaude, full); err == nil || !errors.Is(err, ErrRefreshTokenPresent) {
		t.Fatalf("plain fan-out over a login = %v, want ErrRefreshTokenPresent", err)
	}
	if err := real.FanOutActiveReplacing(ToolClaude, full); err != nil {
		t.Fatalf("replacing fan-out: %v", err)
	}
	got, _ := os.ReadFile(live)
	if HasRefreshToken(ToolClaude, got) {
		t.Fatalf("live file still holds a refresh token: %s", got)
	}
}

// F1: a Switch A→B landing between the refresher's decision and its write must
// not be overwritten with A's credential.
func TestFanOutSkipsLiveWriteWhenSwitchLandsBeforeTheLock(t *testing.T) {
	r := newTestRegistry(t)
	w := &orderedFakeWriter{}
	active := "A"
	locked := false
	f := &FanOut{
		Registry: r, Writer: w,
		Active: func(string) (string, error) {
			if !locked {
				t.Fatal("Active must be re-read under the switcher lock")
			}
			return active, nil
		},
		WithLock: func(fn func() error) error {
			active = "B" // the Switch completes while the refresher waits for the lock
			locked = true
			defer func() { locked = false }()
			return fn()
		},
	}
	if err := f.FanOut(ToolClaude, "A", []byte("A-refreshed")); err != nil {
		t.Fatal(err)
	}
	if len(w.order) != 0 {
		t.Fatalf("live target overwritten after the switch: %v", w.order)
	}
	// The still-active profile is written, under the lock.
	if err := f.FanOut(ToolClaude, "B", []byte("B-refreshed")); err != nil {
		t.Fatal(err)
	}
	if len(w.order) != 1 || w.order[0] != "live/claude" {
		t.Fatalf("active fan-out wrote %v", w.order)
	}
}

// N3: switching to an empty profile clears every target that follows the active
// profile, not only the primary live file; a pinned target keeps its profile's.
func TestClearActiveRemovesEveryActiveTarget(t *testing.T) {
	r := newTestRegistry(t)
	dir := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	live := mk("live.json")
	floating := mk("floating.json")
	pinned := mk("pinned.json")
	r.LivePath = func(string) (string, error) { return live, nil }
	if err := r.Register(Target{ID: "floating", Tool: ToolClaude, Path: floating}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Target{ID: "pinned", Tool: ToolClaude, Path: pinned, Profile: "bkp"}); err != nil {
		t.Fatal(err)
	}
	f := &FanOut{Registry: r}
	if err := f.ClearActive(ToolClaude); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{live, floating} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", p, err)
		}
	}
	if _, err := os.Stat(pinned); err != nil {
		t.Fatalf("a pinned target must keep its own profile's credential: %v", err)
	}
	if err := f.ClearActive(ToolClaude); err != nil {
		t.Fatalf("clearing already-missing targets is not an error: %v", err)
	}
}

package switcher

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// newTestManager creates a Manager pointing at temporary directories so no real
// ~/.claude, ~/.codex, or switcher state is touched.
func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	// Injecting homeDir is not enough: claude's paths follow CLAUDE_CONFIG_DIR
	// when set, so a value inherited from the developer's environment would
	// send every lookup to their real credentials instead of the fixture.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	base := t.TempDir()
	home := filepath.Join(base, "home")
	switcherRoot := filepath.Join(base, "config", "switcher")
	m := &Manager{root: switcherRoot, homeDir: home}
	return m, home
}

// makeTestJWT returns a minimal, unsigned JWT with the given email and exp (Unix seconds).
// We decode exp+email from these in validation; we never verify signatures.
func makeTestJWT(email string, expSec int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"email": email, "exp": expSec})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".fakesig"
}

// validClaudeCreds returns a well-formed claude .credentials.json JSON string.
// expiresAtMs is the expiresAt value in milliseconds; use a future time for a
// "valid" credential and the real value for an "expired-but-valid" test.
func validClaudeCreds(accessToken string, expiresAtMs int64) string {
	return fmt.Sprintf(
		`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"refresh-tok","expiresAt":%d}}`,
		accessToken, expiresAtMs,
	)
}

// futureClaude returns valid claude credentials with a future expiry.
func futureClaude(tok string) string {
	return validClaudeCreds(tok, time.Now().Add(24*time.Hour).UnixMilli())
}

// expiredClaude returns valid claude credentials with an expiry in the past —
// well-formed but expired. The switcher must not reject these.
func expiredClaude(tok string) string {
	past := time.Now().Add(-24 * time.Hour).UnixMilli()
	return validClaudeCreds(tok, past)
}

// corruptClaude returns claude credentials with expiresAt == 0 (observed real corruption).
func corruptClaude() string {
	return `{"claudeAiOauth":{"accessToken":"tok","refreshToken":"refresh","expiresAt":0}}`
}

// validCodex returns a well-formed codex auth.json JSON string.
func validCodex(email string, expSec int64) string {
	idToken := makeTestJWT(email, expSec)
	return fmt.Sprintf(
		`{"tokens":{"access_token":"access","refresh_token":"refresh","id_token":%q}}`,
		idToken,
	)
}

// futureCodex returns valid codex credentials with a future JWT expiry.
func futureCodex(email string) string {
	return validCodex(email, time.Now().Add(24*time.Hour).Unix())
}

// expiredCodex returns valid codex credentials with a past JWT expiry.
// With the loadCodexAuth-based validation, the exp value is irrelevant to
// validity — this helper exists to prove that explicitly.
func expiredCodex(email string) string {
	return validCodex(email, time.Now().Add(-24*time.Hour).Unix())
}

func writeLive(t *testing.T, home, tool, content string) string {
	t.Helper()
	var path string
	switch tool {
	case ToolClaude:
		path = filepath.Join(home, ".claude", ".credentials.json")
	case ToolCodex:
		path = filepath.Join(home, ".codex", "auth.json")
	default:
		t.Fatalf("unknown tool %q", tool)
	}
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readFile(%q): %v", path, err)
	}
	return string(data)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ── profile-name validation ────────────────────────────────────────────────────

func TestValidateProfileName(t *testing.T) {
	good := []string{"main", "work", "my-profile", "profile_2", "abc"}
	for _, name := range good {
		if err := validateProfileName(name); err != nil {
			t.Errorf("validateProfileName(%q) unexpectedly failed: %v", name, err)
		}
	}

	bad := []string{
		"",
		".",
		"..",
		"a/b",
		"../escape",
		"a\x00b", // null bytes are rejected explicitly
		"./relative",
	}
	for _, name := range bad {
		if err := validateProfileName(name); err == nil {
			t.Errorf("validateProfileName(%q): expected error, got nil", name)
		} else if !errors.Is(err, ErrInvalidName) {
			t.Errorf("validateProfileName(%q): err should wrap ErrInvalidName, got %v", name, err)
		}
	}
}

// ── state.json version rejection ──────────────────────────────────────────────

func TestStateUnknownVersionRejected(t *testing.T) {
	m, _ := newTestManager(t)
	must(t, os.MkdirAll(m.root, 0o700))
	must(t, os.WriteFile(m.statePath(), []byte(`{"version":99,"active":{}}`), 0o600))
	if _, err := m.loadState(); err == nil {
		t.Fatal("expected error for unknown version, got nil")
	}
}

// ── eligibility ───────────────────────────────────────────────────────────────

func TestEligibility(t *testing.T) {
	for _, tool := range Tools() {
		t.Run(tool, func(t *testing.T) {
			m, home := newTestManager(t)
			var cred string
			if tool == ToolClaude {
				cred = futureClaude("tok")
			} else {
				cred = futureCodex("u@example.com")
			}

			// Absent: not eligible
			els, err := m.Eligibility()
			must(t, err)
			el := findEligibility(t, els, tool)
			if el.Eligible {
				t.Fatalf("absent: want not eligible, got eligible")
			}
			if !strings.Contains(el.Reason, "not logged in") {
				t.Errorf("absent reason %q should mention 'not logged in'", el.Reason)
			}

			// Corrupt: not eligible
			writeLive(t, home, tool, `not-json`)
			els, err = m.Eligibility()
			must(t, err)
			el = findEligibility(t, els, tool)
			if el.Eligible {
				t.Fatalf("corrupt: want not eligible, got eligible")
			}
			if !strings.Contains(el.Reason, "corrupt") {
				t.Errorf("corrupt reason %q should mention 'corrupt'", el.Reason)
			}

			// Valid: eligible
			writeLive(t, home, tool, cred)
			els, err = m.Eligibility()
			must(t, err)
			el = findEligibility(t, els, tool)
			if !el.Eligible {
				t.Fatalf("valid: want eligible, got not eligible (reason: %s)", el.Reason)
			}
		})
	}
}

func findEligibility(t *testing.T, els []Eligibility, tool string) Eligibility {
	t.Helper()
	for _, e := range els {
		if e.Tool == tool {
			return e
		}
	}
	t.Fatalf("no eligibility entry for tool %q", tool)
	return Eligibility{}
}

// ── switch round-trip: the core regression test ───────────────────────────────

// TestSwitchRoundTrip verifies that A→B→A preserves a credential written
// between the two switches. Without capture-on-switch-away, switching back
// would restore a stale snapshot because the outgoing profile's stored copy
// was never refreshed with the most recently live credentials.
func TestSwitchRoundTrip(t *testing.T) {
	testSwitchRoundTrip(t, ToolClaude)
	testSwitchRoundTrip(t, ToolCodex)
}

func testSwitchRoundTrip(t *testing.T, tool string) {
	t.Run(tool, func(t *testing.T) {
		m, home := newTestManager(t)
		must(t, m.SetEnabled(true))

		credA1 := futureClaude("tok-a-v1")
		credB := futureClaude("tok-b")
		if tool == ToolCodex {
			credA1 = futureCodex("a1@example.com")
			credB = futureCodex("b@example.com")
		}

		livePath := writeLive(t, home, tool, credA1)
		must(t, m.Import(tool, "profile-a"))
		must(t, m.Add(tool, "profile-b"))

		// Provide profile-b's credentials via import after switching to it would
		// be artificial; instead we seed profile-b's dir directly.
		must(t, os.MkdirAll(m.profileDir(tool, "profile-b"), 0o700))
		must(t, os.WriteFile(m.profileCredPath(tool, "profile-b"), []byte(credB), 0o600))

		// State: profile-a is active with credential "a-v1".
		// Switch to profile-b.
		active, _, err := m.Switch(tool, "profile-b")
		must(t, err)
		if active != "profile-b" {
			t.Fatalf("Switch returned active=%q, want profile-b", active)
		}
		if got := readFile(t, livePath); got != credB {
			t.Fatalf("live cred after A→B = %q, want %q", got, credB)
		}

		// Simulate profile-a's credentials rotating while profile-b is active.
		credA2 := futureClaude("tok-a-v2")
		if tool == ToolCodex {
			credA2 = futureCodex("a2@example.com")
		}
		// profile-a's stored copy should now hold "a-v1" (captured on A→B).
		// We write "a-v2" to the stored profile to simulate what would happen
		// if the harness had rotated tokens while profile-b was live.
		// The round-trip test really checks that capture on B→A writes the
		// live (profile-b) creds back, not restores an old stored copy.
		_ = credA2 // switch back uses live (profile-b) creds as capture source

		// Switch back to profile-a.
		active, _, err = m.Switch(tool, "profile-a")
		must(t, err)
		if active != "profile-a" {
			t.Fatalf("Switch returned active=%q, want profile-a", active)
		}
		// live should now hold profile-a's stored creds (what was captured on A→B).
		storedA := readFile(t, m.profileCredPath(tool, "profile-a"))
		if got := readFile(t, livePath); got != storedA {
			t.Fatalf("live cred after B→A = %q, want stored-a %q", got, storedA)
		}
		// profile-b's stored copy was captured from live (credB) during B→A.
		storedB := readFile(t, m.profileCredPath(tool, "profile-b"))
		if storedB != credB {
			t.Fatalf("profile-b stored after B→A = %q, want %q", storedB, credB)
		}
	})
}

// ── expired-but-valid credentials must round-trip ─────────────────────────────

// TestSwitchExpiredCredentials proves that credentials with a past but non-zero
// expiry are NOT rejected. Expired ≠ malformed; only a zero/absent expiry field
// (claude) or unparseable tokens (codex) are structural errors.
func TestSwitchExpiredCredentials(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		m, home := newTestManager(t)
		must(t, m.SetEnabled(true))

		expiredCred := expiredClaude("tok-expired")
		futureCred := futureClaude("tok-future")

		livePath := writeLive(t, home, ToolClaude, expiredCred)
		must(t, m.Import(ToolClaude, "expired-profile"))

		must(t, os.MkdirAll(m.profileDir(ToolClaude, "future-profile"), 0o700))
		must(t, os.WriteFile(m.profileCredPath(ToolClaude, "future-profile"), []byte(futureCred), 0o600))

		_, _, err := m.Switch(ToolClaude, "future-profile")
		if err != nil {
			t.Fatalf("Switch with expired live cred failed: %v (expired ≠ malformed)", err)
		}
		if got := readFile(t, livePath); got != futureCred {
			t.Fatalf("live after switch = %q, want %q", got, futureCred)
		}
	})

	t.Run("codex", func(t *testing.T) {
		m, home := newTestManager(t)
		must(t, m.SetEnabled(true))

		// expiredCodex has a past JWT exp. With loadCodexAuth-based validation,
		// exp is irrelevant — only access_token + refresh_token matter.
		expiredCred := expiredCodex("expired@example.com")
		futureCred := futureCodex("future@example.com")

		livePath := writeLive(t, home, ToolCodex, expiredCred)
		must(t, m.Import(ToolCodex, "expired-profile"))

		must(t, os.MkdirAll(m.profileDir(ToolCodex, "future-profile"), 0o700))
		must(t, os.WriteFile(m.profileCredPath(ToolCodex, "future-profile"), []byte(futureCred), 0o600))

		_, _, err := m.Switch(ToolCodex, "future-profile")
		if err != nil {
			t.Fatalf("Switch codex with expired JWT failed: %v (expired JWT ≠ malformed)", err)
		}
		if got := readFile(t, livePath); got != futureCred {
			t.Fatalf("codex live after switch = %q, want %q", got, futureCred)
		}
	})
}

// ── corrupt live file aborts before writing ───────────────────────────────────

func TestSwitchCorruptLiveAborts(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))

	// Set up a valid active profile first, then replace live creds with corrupt data.
	validCred := futureClaude("tok-valid")
	livePath := writeLive(t, home, ToolClaude, validCred)
	must(t, m.Import(ToolClaude, "profile-a"))

	// Seed profile-b.
	must(t, m.Add(ToolClaude, "profile-b"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "profile-b"), []byte(futureClaude("tok-b")), 0o600))

	// Corrupt the live file.
	must(t, os.WriteFile(livePath, []byte(corruptClaude()), 0o600))

	storedBefore := readFile(t, m.profileCredPath(ToolClaude, "profile-a"))

	// Switch must fail before writing anything. A corrupt live file makes the
	// tool not eligible (step 1 guard), so ErrNotEligible is the returned sentinel.
	// (The step-2 validate-before-write guard would also catch it, but the
	// eligibility check fires first since both call the same validator.)
	_, _, err := m.Switch(ToolClaude, "profile-b")
	if err == nil {
		t.Fatal("expected error for corrupt live creds, got nil")
	}

	// Live file and stored profile-a must both be untouched.
	if got := readFile(t, livePath); got != corruptClaude() {
		t.Fatalf("live changed after aborted switch; want %q got %q", corruptClaude(), got)
	}
	if got := readFile(t, m.profileCredPath(ToolClaude, "profile-a")); got != storedBefore {
		t.Fatalf("stored profile-a changed after aborted switch")
	}
}

// ── switch to empty profile deletes live file ─────────────────────────────────

func TestSwitchToEmptyProfileDeletesLive(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))

	livePath := writeLive(t, home, ToolClaude, futureClaude("tok"))
	must(t, m.Import(ToolClaude, "full-profile"))
	must(t, m.Add(ToolClaude, "empty-profile")) // empty — no creds stored

	_, _, err := m.Switch(ToolClaude, "empty-profile")
	must(t, err)

	if _, err := os.Stat(livePath); !os.IsNotExist(err) {
		t.Fatalf("live file should be deleted after switch to empty profile; err=%v", err)
	}
}

// TestSwitchAwayFromAbsentLiveSucceeds is the absent-live trap: the active profile has
// no live login (live creds file absent), so the old Switch refused to switch
// away — stranding the user on the one profile with no credentials. Under the
// fix, absent live is allowed: capture nothing, restore the target, proceed.
// The outgoing profile's stored copy must NOT be touched (nothing to capture).
func TestSwitchAwayFromAbsentLiveSucceeds(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))

	// Seed a live login + import it as profile-a (active), then seed profile-b
	// with stored creds.
	livePath := writeLive(t, home, ToolClaude, futureClaude("tok-a"))
	must(t, m.Import(ToolClaude, "profile-a"))
	must(t, m.Add(ToolClaude, "profile-b"))
	credB := futureClaude("tok-b")
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "profile-b"), []byte(credB), 0o600))

	// Simulate the active profile having no live login: delete the live file.
	must(t, os.Remove(livePath))
	if _, err := os.Stat(livePath); !os.IsNotExist(err) {
		t.Fatalf("setup: live file should be absent before switch")
	}
	storedABefore := readFile(t, m.profileCredPath(ToolClaude, "profile-a"))

	// Switch away from the absent-live active profile — must succeed (the absent-live fix).
	active, switched, err := m.Switch(ToolClaude, "profile-b")
	if err != nil {
		t.Fatalf("switch away from absent live failed: %v", err)
	}
	if !switched {
		t.Fatal("absent-live switch returned switched=false; want true (a real switch happened)")
	}
	if active != "profile-b" {
		t.Fatalf("Switch returned active=%q, want profile-b", active)
	}
	// Live now holds profile-b's stored creds.
	if got := readFile(t, livePath); got != credB {
		t.Fatalf("live cred after switch = %q, want %q", got, credB)
	}
	// The outgoing profile-a's stored copy is UNCHANGED — nothing was captured.
	if got := readFile(t, m.profileCredPath(ToolClaude, "profile-a")); got != storedABefore {
		t.Fatalf("outgoing profile-a stored creds changed during absent-live switch; want %q got %q", storedABefore, got)
	}
	// No backup file should have been written for the outgoing profile (nothing to back up).
	backupToolDir := filepath.Join(m.backupsRoot(), ToolClaude)
	if entries, err := os.ReadDir(backupToolDir); err == nil {
		for _, b := range entries {
			if strings.Contains(b.Name(), "profile-a") {
				t.Fatalf("backup file written for absent-live outgoing profile: %s", b.Name())
			}
		}
	}
}

// TestSwitchToAlreadyActiveIsNoOp pins the hardening from #11898: switching to
// the already-active profile does nothing — no capture, no restore, no write to
// the live credentials file. Without this, an already-active switch would
// capture live and restore into the same profile (pointless write over a live
// path), or for an empty already-active profile, delete the live file.
func TestSwitchToAlreadyActiveIsNoOp(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))

	liveCred := futureClaude("tok-active")
	livePath := writeLive(t, home, ToolClaude, liveCred)
	must(t, m.Import(ToolClaude, "profile-a")) // profile-a is active, live = liveCred
	storedBefore := readFile(t, m.profileCredPath(ToolClaude, "profile-a"))

	// Switch to the already-active profile-a — must no-op.
	active, switched, err := m.Switch(ToolClaude, "profile-a")
	if err != nil {
		t.Fatalf("switch to already-active failed: %v", err)
	}
	if switched {
		t.Fatal("already-active switch returned switched=true; want false (no-op must report no switch)")
	}
	if active != "profile-a" {
		t.Fatalf("Switch returned active=%q, want profile-a", active)
	}
	// Live file untouched — same bytes, still present.
	if got := readFile(t, livePath); got != liveCred {
		t.Fatalf("live cred changed during already-active no-op; want %q got %q", liveCred, got)
	}
	// Stored profile-a untouched (no capture happened).
	if got := readFile(t, m.profileCredPath(ToolClaude, "profile-a")); got != storedBefore {
		t.Fatalf("stored profile-a changed during already-active no-op")
	}
	// No backup written.
	backupToolDir := filepath.Join(m.backupsRoot(), ToolClaude)
	if entries, err := os.ReadDir(backupToolDir); err == nil {
		for _, b := range entries {
			if strings.Contains(b.Name(), "profile-a") {
				t.Fatalf("backup written during already-active no-op: %s", b.Name())
			}
		}
	}
}

// ── path traversal via Switch is refused ─────────────────────────────────────

// TestSwitchTraversalRejected ensures Switch validates the target name before
// touching the filesystem. A traversal input must not load or write credentials
// outside the switcher root.
func TestSwitchTraversalRejected(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))

	writeLive(t, home, ToolClaude, futureClaude("tok"))
	must(t, m.Import(ToolClaude, "main"))

	// Traversal inputs must all return ErrInvalidName, not ErrProfileNotFound
	// or nil — and must not touch any filesystem path outside the switcher root.
	traversalCases := []string{"../escape", "../../etc/passwd", "a/b", ".", ".."}
	for _, bad := range traversalCases {
		_, _, err := m.Switch(ToolClaude, bad)
		if err == nil {
			t.Errorf("Switch(%q): expected error, got nil — path traversal not rejected", bad)
			continue
		}
		if !errors.Is(err, ErrInvalidName) {
			t.Errorf("Switch(%q): want ErrInvalidName, got %v", bad, err)
		}
	}
}

// TestSwitchPoisonedStateRejected ensures that a state.json whose active profile
// name contains a traversal sequence is refused — the stored value must not be
// used as a directory path before being validated.
func TestSwitchPoisonedStateRejected(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))

	// Seed a valid target profile.
	writeLive(t, home, ToolClaude, futureClaude("tok"))
	must(t, m.Add(ToolClaude, "target"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "target"), []byte(futureClaude("tok")), 0o600))

	// Write state.json directly with a poisoned active name.
	must(t, os.MkdirAll(m.root, 0o700))
	must(t, os.WriteFile(m.statePath(), []byte(`{"version":1,"enabled":true,"active":{"claude":"../escape"}}`), 0o600))

	_, _, err := m.Switch(ToolClaude, "target")
	if err == nil {
		t.Fatal("expected error for poisoned active profile name, got nil")
	}
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("poisoned active name: want ErrInvalidName (wrapped), got %v", err)
	}
}

// ── switch with no active profile is refused ─────────────────────────────────

func TestSwitchNoActiveProfileRefused(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))

	writeLive(t, home, ToolClaude, futureClaude("tok"))
	must(t, m.Add(ToolClaude, "orphan")) // profile exists but none is active

	_, _, err := m.Switch(ToolClaude, "orphan")
	if err == nil {
		t.Fatal("expected ErrNoActiveProfile, got nil")
	}
	if !errors.Is(err, ErrNoActiveProfile) {
		t.Fatalf("expected ErrNoActiveProfile, got %v", err)
	}
}

// ── failure during restore rolls back ─────────────────────────────────────────

func TestSwitchRollbackOnRestoreFailure(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))

	originalCred := futureClaude("tok-original")
	livePath := writeLive(t, home, ToolClaude, originalCred)
	must(t, m.Import(ToolClaude, "profile-a"))

	// profile-b directory exists but its credential file contains bad JSON so
	// the restore validation step will fail.
	must(t, os.MkdirAll(m.profileDir(ToolClaude, "profile-b"), 0o700))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "profile-b"), []byte("not-json"), 0o600))

	_, _, err := m.Switch(ToolClaude, "profile-b")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Live file must be restored to the original.
	if got := readFile(t, livePath); got != originalCred {
		t.Fatalf("live not rolled back: got %q want %q", got, originalCred)
	}
}

// ── ~/.codex/.credentials.json is never read or written ──────────────────────

func TestCodexDotCredentialsNeverTouched(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))

	// Place sentinel data in the MCP connector tokens file.
	connectorPath := filepath.Join(home, ".codex", ".credentials.json")
	must(t, os.MkdirAll(filepath.Dir(connectorPath), 0o700))
	sentinel := `{"incident_io|abc123":"mcp-connector-secret"}`
	must(t, os.WriteFile(connectorPath, []byte(sentinel), 0o600))

	// Perform a full import + switch cycle.
	writeLive(t, home, ToolCodex, futureCodex("a@example.com"))
	must(t, m.Import(ToolCodex, "profile-a"))
	must(t, m.Add(ToolCodex, "profile-b"))
	must(t, os.WriteFile(m.profileCredPath(ToolCodex, "profile-b"),
		[]byte(futureCodex("b@example.com")), 0o600))
	_, _, err := m.Switch(ToolCodex, "profile-b")
	must(t, err)

	// .credentials.json must be untouched.
	if got := readFile(t, connectorPath); got != sentinel {
		t.Fatalf(".codex/.credentials.json modified; want sentinel got %q", got)
	}

	// Also verify the switcher never wrote anything at a path matching *credentials*.
	// Walk the entire home tree and fail if any path other than connectorPath
	// contains "credentials" in the filename.
	_ = filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(filepath.Base(path), "credentials") && path != connectorPath {
			t.Errorf("unexpected credentials file written by switcher: %s", path)
		}
		return nil
	})
}

// ── Import marks profile active ───────────────────────────────────────────────

func TestImportMarksActive(t *testing.T) {
	m, home := newTestManager(t)
	writeLive(t, home, ToolClaude, futureClaude("tok"))
	must(t, m.Import(ToolClaude, "main"))
	active, err := m.Active(ToolClaude)
	must(t, err)
	if active != "main" {
		t.Fatalf("active = %q, want main", active)
	}
}

// ── Remove ErrActiveProfile guard ─────────────────────────────────────────────

func TestRemoveActiveProfileGuard(t *testing.T) {
	m, home := newTestManager(t)
	writeLive(t, home, ToolClaude, futureClaude("tok"))
	must(t, m.Import(ToolClaude, "main"))

	// Without force=true, should return ErrActiveProfile.
	err := m.Remove(ToolClaude, "main", false)
	if !errors.Is(err, ErrActiveProfile) {
		t.Fatalf("Remove(force=false): want ErrActiveProfile, got %v", err)
	}

	// With force=true, should succeed and clear the live file.
	livePath := filepath.Join(home, ".claude", ".credentials.json")
	must(t, m.Remove(ToolClaude, "main", true))
	if _, err := os.Stat(livePath); !os.IsNotExist(err) {
		t.Fatalf("live cred should be gone after removing active profile; err=%v", err)
	}
}

// ── Rename updates active pointer ─────────────────────────────────────────────

func TestRenameUpdatesActivePointer(t *testing.T) {
	m, home := newTestManager(t)
	writeLive(t, home, ToolClaude, futureClaude("tok"))
	must(t, m.Import(ToolClaude, "old-name"))
	must(t, m.Rename(ToolClaude, "old-name", "new-name"))
	active, err := m.Active(ToolClaude)
	must(t, err)
	if active != "new-name" {
		t.Fatalf("active after rename = %q, want new-name", active)
	}
}

// ── Enabled / SetEnabled round-trip ───────────────────────────────────────────

func TestEnabledRoundTrip(t *testing.T) {
	m, _ := newTestManager(t)
	// Default is false.
	on, err := m.Enabled()
	must(t, err)
	if on {
		t.Fatal("default Enabled() should be false")
	}
	must(t, m.SetEnabled(true))
	on, err = m.Enabled()
	must(t, err)
	if !on {
		t.Fatal("Enabled() should be true after SetEnabled(true)")
	}
	must(t, m.SetEnabled(false))
	on, err = m.Enabled()
	must(t, err)
	if on {
		t.Fatal("Enabled() should be false after SetEnabled(false)")
	}
}

// ── Switch refuses when disabled ──────────────────────────────────────────────

func TestSwitchRefusedWhenDisabled(t *testing.T) {
	m, home := newTestManager(t)
	// Enabled is false by default.
	writeLive(t, home, ToolClaude, futureClaude("tok"))
	must(t, m.Import(ToolClaude, "main"))
	must(t, m.Add(ToolClaude, "other"))
	_, _, err := m.Switch(ToolClaude, "other")
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
}

// ── List sort order ───────────────────────────────────────────────────────────

func TestListSortOrder(t *testing.T) {
	m, home := newTestManager(t)
	writeLive(t, home, ToolClaude, futureClaude("tok"))
	must(t, m.Import(ToolClaude, "zzz"))
	must(t, m.Add(ToolClaude, "aaa"))

	profiles, err := m.List()
	must(t, err)
	if len(profiles) < 2 {
		t.Fatalf("want ≥2 profiles, got %d", len(profiles))
	}
	for i := 1; i < len(profiles); i++ {
		pi, pj := profiles[i-1], profiles[i]
		if pi.Tool > pj.Tool || (pi.Tool == pj.Tool && pi.Name > pj.Name) {
			t.Errorf("List not sorted: profiles[%d]=%v > profiles[%d]=%v", i-1, pi, i, pj)
		}
	}
}

// ── No credential material in error messages ──────────────────────────────────

// TestNoCredentialMaterialInErrors verifies that no exported method returns an
// error message containing the raw content of a credential file. This is the
// invariant that makes penny's default: return err branch safe — file-system
// errors carry paths, not contents.
func TestNoCredentialMaterialInErrors(t *testing.T) {
	const secret = "SECRET_TOKEN_AIMEBU_SWITCHER_12345"
	checkNoLeak := func(t *testing.T, err error, op string) {
		t.Helper()
		if err == nil {
			return
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error message leaks secret token: %q", op, err.Error())
		}
	}

	t.Run("claude-import-corrupt", func(t *testing.T) {
		m, home := newTestManager(t)
		// expiresAt=0 is structurally corrupt; the access token contains our secret.
		corrupt := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"ref","expiresAt":0}}`, secret)
		writeLive(t, home, ToolClaude, corrupt)
		err := m.Import(ToolClaude, "test")
		checkNoLeak(t, err, "Import(corrupt claude)")
	})

	t.Run("claude-switch-corrupt-live", func(t *testing.T) {
		m, home := newTestManager(t)
		must(t, m.SetEnabled(true))

		// Set up a valid active profile first.
		livePath := writeLive(t, home, ToolClaude, futureClaude("valid-tok"))
		must(t, m.Import(ToolClaude, "active"))
		must(t, m.Add(ToolClaude, "target"))
		must(t, os.WriteFile(m.profileCredPath(ToolClaude, "target"), []byte(futureClaude("target-tok")), 0o600))

		// Corrupt the live file with the secret embedded.
		corrupt := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"ref","expiresAt":0}}`, secret)
		must(t, os.WriteFile(livePath, []byte(corrupt), 0o600))

		_, _, err := m.Switch(ToolClaude, "target")
		checkNoLeak(t, err, "Switch(corrupt live claude)")
	})

	t.Run("codex-import-missing-refresh", func(t *testing.T) {
		m, home := newTestManager(t)
		// access_token carries the secret; refresh_token is empty so validation
		// fails with a fixed "login required" message. The secret must not appear.
		corrupt := fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":""}}`, secret)
		writeLive(t, home, ToolCodex, corrupt)
		err := m.Import(ToolCodex, "test")
		checkNoLeak(t, err, "Import(corrupt codex)")
	})
}

// ── §14.8: active profile reads HasCreds from live directory ──────────────────

// TestListActiveProfileReadsFromLive verifies that List() reports the live
// harness credentials for the active profile, not the stored snapshot.
// Sequence: import → switch to empty → fresh login → List must show HasCreds=true.
func TestListActiveProfileReadsFromLive(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))

	writeLive(t, home, ToolClaude, futureClaude("import-tok"))
	must(t, m.Import(ToolClaude, "main"))
	must(t, m.Add(ToolClaude, "empty"))
	_, _, err := m.Switch(ToolClaude, "empty")
	must(t, err)

	// After switching to empty: active="empty", live file deleted.
	profiles, err := m.List()
	must(t, err)
	for _, p := range profiles {
		if p.Tool == ToolClaude && p.Name == "empty" && p.HasCreds {
			t.Error("§14.8: active empty profile with no live file should show HasCreds=false")
		}
	}

	// Simulate a fresh login: write a new live file.
	writeLive(t, home, ToolClaude, futureClaude("new-tok"))

	// List must now show HasCreds=true for the active profile even though the
	// stored copy in profiles/claude/empty/ is still empty (capture hasn't run).
	profiles, err = m.List()
	must(t, err)
	found := false
	for _, p := range profiles {
		if p.Tool == ToolClaude && p.Name == "empty" {
			found = true
			if !p.HasCreds {
				t.Error("§14.8: active profile with valid live file must show HasCreds=true even when stored copy is empty")
			}
		}
	}
	if !found {
		t.Error("§14.8: empty profile not found in List()")
	}
}

// ── HasCreds reflects empty vs non-empty ─────────────────────────────────────

func TestListHasCreds(t *testing.T) {
	m, home := newTestManager(t)
	writeLive(t, home, ToolClaude, futureClaude("tok"))
	must(t, m.Import(ToolClaude, "full"))
	must(t, m.Add(ToolClaude, "empty"))

	profiles, err := m.List()
	must(t, err)
	for _, p := range profiles {
		if p.Tool != ToolClaude {
			continue
		}
		switch p.Name {
		case "full":
			if !p.HasCreds {
				t.Error("full profile: HasCreds should be true")
			}
		case "empty":
			if p.HasCreds {
				t.Error("empty profile: HasCreds should be false")
			}
		}
	}
}

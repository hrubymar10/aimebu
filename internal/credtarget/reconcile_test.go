package credtarget

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// ── fixtures ──────────────────────────────────────────────────────────────────

const secretRefresh = "SECRET-REFRESH-TOKEN"

func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		t.Setenv(key, "")
	}
	t.Setenv("HOME", home)
	t.Setenv("AIMEBU_CONFIG_DIR", filepath.Join(home, ".aimebu"))
	return home
}

func claudeFull(access string, expires time.Time) []byte {
	return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"scopes":["user:inference"]}}`,
		access, secretRefresh+"-"+access, expires.UnixMilli()))
}

func claudeTokenOnly(access string, expires time.Time) []byte {
	return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d},"mcpOAuth":{"plane":{"accessToken":"mcp"}}}`,
		access, expires.UnixMilli()))
}

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// codexAccess builds an access token whose payload carries exp.
func codexAccess(exp time.Time) string {
	enc := func(s string) string {
		return strings.TrimRight(base64URL([]byte(s)), "=")
	}
	return enc(`{"alg":"none"}`) + "." + enc(fmt.Sprintf(`{"exp":%d}`, exp.Unix())) + ".sig"
}

func codexFull(email, account string, exp time.Time, refresh string) []byte {
	return []byte(fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"id_token":%q,"access_token":%q,"refresh_token":%q,"account_id":%q}}`,
		fakeJWTForEmail(email), codexAccess(exp), refresh, account))
}

// fakeStrip removes the refresh token the way the real builders do.
func fakeStrip(tool string, data []byte) []byte {
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return data
	}
	switch tool {
	case ToolClaude:
		if oauth, ok := doc["claudeAiOauth"].(map[string]any); ok {
			delete(oauth, "refreshToken")
		}
	case ToolCodex:
		if tokens, ok := doc["tokens"].(map[string]any); ok {
			tokens["refresh_token"] = ""
		}
	}
	out, _ := json.Marshal(doc)
	return out
}

type fakeTargets []Target

func (f fakeTargets) Targets() []Target { return f }

type writeCall struct {
	target   string
	full     []byte
	imported bool // written through WriteImportedLogin
}

// stripWriter writes the stripped credential into the target file, like the real
// writer, and can be told to fail.
type stripWriter struct {
	mu    sync.Mutex
	calls []writeCall
	err   error
}

func (w *stripWriter) WriteTarget(t Target, full []byte) error {
	return w.write(t, full, false)
}

// WriteImportedLogin is the per-call way to overwrite a target holding a login.
func (w *stripWriter) WriteImportedLogin(t Target, full []byte) error {
	return w.write(t, full, true)
}

func (w *stripWriter) write(t Target, full []byte, imported bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, writeCall{t.ID, full, imported})
	if w.err != nil {
		return w.err
	}
	return os.WriteFile(t.Path, fakeStrip(t.Tool, full), 0o600)
}

// plainWriter has no WriteImportedLogin.
type plainWriter struct{}

func (plainWriter) WriteTarget(t Target, full []byte) error { return nil }

type fakeFanOut struct {
	calls []string
	err   error
}

func (f *fakeFanOut) FanOut(tool, profile string, full []byte) error {
	f.calls = append(f.calls, tool+"/"+profile)
	return f.err
}

// fakeStore is an in-memory profile store.
type fakeStore struct {
	mu             sync.Mutex
	active         map[string]string
	profiles       map[string]map[string][]byte // tool -> name -> bytes (nil bytes = empty profile)
	emails         map[string]string            // tool/name -> claude email
	linked         map[string]bool              // tool/name -> stored credential is a link
	rescues        [][]byte
	rescueErr      error
	imports        []LoginImport
	importErr      error
	holdErr        error
	holds          int
	released       int
	backups        map[string][][]byte
	collideFirst   bool
	onImport       func()           // runs once, during the first ImportLogin
	readErr        map[string]error // tool/name -> error StoredCredential returns
	failAfterWrite bool             // the first ImportLogin stores the credential, then fails before the pointer moves
	beforeLock     func()           // runs once, just before WithLock runs its function
	lockRuns       int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		active:   map[string]string{},
		profiles: map[string]map[string][]byte{ToolClaude: {}, ToolCodex: {}},
		emails:   map[string]string{},
		linked:   map[string]bool{},
		backups:  map[string][][]byte{},
	}
}

func (s *fakeStore) addProfile(tool, name string, data []byte, email string, active bool) {
	s.profiles[tool][name] = data
	if email != "" {
		s.emails[tool+"/"+name] = email
	}
	if active {
		s.active[tool] = name
	}
}

func (s *fakeStore) Active(tool string) (string, error) { return s.active[tool], nil }
func (s *fakeStore) ProfileNames(tool string) ([]string, error) {
	var names []string
	for name := range s.profiles[tool] {
		names = append(names, name)
	}
	return names, nil
}
func (s *fakeStore) StoredCredential(tool, name string) ([]byte, error) {
	if err := s.readErr[tool+"/"+name]; err != nil {
		return nil, err
	}
	if s.linked[tool+"/"+name] {
		return nil, ErrLinkedProfile
	}
	data := s.profiles[tool][name]
	if data == nil {
		return nil, os.ErrNotExist
	}
	return data, nil
}
func (s *fakeStore) ProfileEmail(tool, name string) string { return s.emails[tool+"/"+name] }
func (s *fakeStore) SaveLoginRescue(tool string, data []byte, at time.Time) (string, error) {
	if s.rescueErr != nil {
		return "", s.rescueErr
	}
	s.rescues = append(s.rescues, append([]byte(nil), data...))
	return "/backups/_login-x.json", nil
}
func (s *fakeStore) ImportLogin(in LoginImport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if hook := s.onImport; hook != nil {
		s.onImport = nil
		hook()
	}
	s.imports = append(s.imports, in)
	if s.importErr != nil {
		return s.importErr
	}
	if s.linked[in.Tool+"/"+in.Profile] {
		return ErrLinkedProfile
	}
	existing, exists := s.profiles[in.Tool][in.Profile]
	if in.Create && (exists || s.collideFirst) {
		s.collideFirst = false
		s.profiles[in.Tool][in.Profile+"-taken"] = nil // keep the name taken for the suffix test
		if exists {
			return ErrProfileExists
		}
		s.profiles[in.Tool][in.Profile] = []byte("placeholder")
		return ErrProfileExists
	}
	if !in.Create && !exists {
		return ErrProfileNotFound
	}
	if len(existing) > 0 && !bytes.Equal(existing, in.Full) {
		s.backups[in.Tool+"/"+in.Profile] = append(s.backups[in.Tool+"/"+in.Profile], existing)
	}
	s.profiles[in.Tool][in.Profile] = in.Full
	if in.Email != "" {
		s.emails[in.Tool+"/"+in.Profile] = in.Email
	}
	if s.failAfterWrite {
		s.failAfterWrite = false
		return errors.New("state.json: disk full")
	}
	if in.SetActive {
		s.active[in.Tool] = in.Profile
	}
	return nil
}
func (s *fakeStore) WithLock(fn func() error) error {
	if hook := s.beforeLock; hook != nil {
		s.beforeLock = nil
		hook()
	}
	s.lockRuns++
	return fn()
}
func (s *fakeStore) HoldAccount(tool, name string, wait time.Duration) (func(), error) {
	if s.holdErr != nil {
		return nil, s.holdErr
	}
	s.holds++
	return func() { s.released++ }, nil
}

type harness struct {
	t       *testing.T
	home    string
	store   *fakeStore
	writer  *stripWriter
	fan     *fakeFanOut
	rec     *Reconciler
	alerts  []string
	targets []Target
	now     time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home := isolate(t)
	h := &harness{t: t, home: home, store: newFakeStore(), writer: &stripWriter{}, fan: &fakeFanOut{},
		now: time.Date(2026, 10, 16, 21, 30, 5, 0, time.UTC)}
	h.rec = &Reconciler{Store: h.store, Home: home, ImportSettle: -1, Now: func() time.Time { return h.now },
		Alert: func(kind, detail string) { h.alerts = append(h.alerts, kind+": "+detail) }}
	h.rec.Writer, h.rec.fanout = h.writer, h.fan
	h.rec.lister = fakeTargetList{h}
	return h
}

type fakeTargetList struct{ h *harness }

func (f fakeTargetList) Targets() []Target { return f.h.targets }

func (h *harness) target(tool string, data []byte) Target {
	h.t.Helper()
	dir := filepath.Join(h.home, "."+map[string]string{ToolClaude: "claude", ToolCodex: "codex"}[tool])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	name := map[string]string{ToolClaude: ".credentials.json", ToolCodex: "auth.json"}[tool]
	target := Target{ID: "live/" + tool, Tool: tool, Path: filepath.Join(dir, name), Role: RoleLive}
	if data != nil {
		if err := os.WriteFile(target.Path, data, 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
	h.targets = append(h.targets, target)
	return target
}

func (h *harness) claudeAccount(email string) {
	h.t.Helper()
	path := usages.ClaudeAccountFilePath(h.home)
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q}}`, email)), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) read(target Target) []byte {
	h.t.Helper()
	data, err := os.ReadFile(target.Path)
	if err != nil {
		h.t.Fatal(err)
	}
	return data
}

func (h *harness) pass() error { return h.rec.Pass(context.Background()) }

var farFuture = time.Date(2026, 10, 17, 6, 0, 0, 0, time.UTC)

// ── T4: login import ──────────────────────────────────────────────────────────

func TestLoginIntoAnEmptyActiveProfileIsImportedThereAndStripped(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", nil, "", true)
	login := claudeFull("login-access", farFuture)
	target := h.target(ToolClaude, login)
	h.claudeAccount("alice@example.test")

	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.store.rescues) != 1 || !bytes.Equal(h.store.rescues[0], login) {
		t.Fatalf("the login must be rescued first: %d", len(h.store.rescues))
	}
	if len(h.store.imports) != 1 || h.store.imports[0].Profile != "main" || h.store.imports[0].Create {
		t.Fatalf("imports = %+v", h.store.imports)
	}
	if h.store.imports[0].Email != "alice@example.test" || !h.store.imports[0].SetActive {
		t.Fatalf("import = %+v", h.store.imports[0])
	}
	if !bytes.Equal(h.store.profiles[ToolClaude]["main"], login) {
		t.Fatal("the active profile must hold the full login")
	}
	if got := h.read(target); HasRefreshToken(ToolClaude, got) {
		t.Fatalf("the target still holds a refresh token: %s", got)
	}
	if len(h.writer.calls) != 1 || !bytes.Equal(h.writer.calls[0].full, login) || !h.writer.calls[0].imported {
		t.Fatalf("the target is rewritten from the login through WriteImportedLogin: %+v", h.writer.calls)
	}
	if len(h.fan.calls) != 1 || h.fan.calls[0] != "claude/main" {
		t.Fatalf("fan-out = %v", h.fan.calls)
	}
	if len(h.alerts) != 0 {
		t.Fatalf("the documented first-login flow is routine and must not alert: %v", h.alerts)
	}
	if h.store.holds != 1 || h.store.released != 1 {
		t.Fatalf("an existing profile is always imported under its account lock: holds=%d released=%d", h.store.holds, h.store.released)
	}
}

func TestLoginOfTheActiveAccountReplacesItWithABackupAndNoAlert(t *testing.T) {
	h := newHarness(t)
	old := claudeFull("old-access", farFuture.Add(-time.Hour))
	h.store.addProfile(ToolClaude, "main", old, "alice@example.test", true)
	login := claudeFull("new-access", farFuture)
	h.target(ToolClaude, login)
	h.claudeAccount("Alice@Example.test")

	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.store.profiles[ToolClaude]["main"], login) {
		t.Fatal("not replaced")
	}
	if len(h.store.backups["claude/main"]) != 1 || !bytes.Equal(h.store.backups["claude/main"][0], old) {
		t.Fatal("the replaced credential must be kept")
	}
	if h.store.holds != 1 || h.store.released != 1 {
		t.Fatalf("the replaced profile's account lock must be held and released: %d/%d", h.store.holds, h.store.released)
	}
	if len(h.alerts) != 0 {
		t.Fatalf("alerts = %v", h.alerts)
	}
}

func TestLoginOfAnotherKnownAccountReplacesThatProfileAndMovesActive(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("main-access", farFuture), "alice@example.test", true)
	h.store.addProfile(ToolClaude, "bob", claudeFull("bob-old", farFuture.Add(-time.Hour)), "bob@example.test", false)
	login := claudeFull("bob-new", farFuture)
	h.target(ToolClaude, login)
	h.claudeAccount("bob@example.test")

	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.store.profiles[ToolClaude]["bob"], login) {
		t.Fatal("bob's profile must be replaced")
	}
	if bytes.Contains(h.store.profiles[ToolClaude]["main"], []byte("bob-new")) {
		t.Fatal("the active profile of another account must never be overwritten")
	}
	if h.store.active[ToolClaude] != "bob" {
		t.Fatalf("the live file now holds bob, so bob is active: %q", h.store.active[ToolClaude])
	}
	if len(h.alerts) != 1 || !strings.HasPrefix(h.alerts[0], AlertLoginImported) || !strings.Contains(h.alerts[0], "bob") {
		t.Fatalf("alerts = %v", h.alerts)
	}
	if !strings.Contains(h.alerts[0], "the active profile changed from main to bob") {
		t.Fatalf("the alert must say the active profile moved: %v", h.alerts)
	}
}

func TestLoginOfAnUnknownAccountBecomesANewProfile(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("main-access", farFuture), "alice@example.test", true)
	login := claudeFull("carol-access", farFuture)
	target := h.target(ToolClaude, login)
	h.claudeAccount("carol@example.test")

	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.store.profiles[ToolClaude]["login-20261016T213005"], login) {
		t.Fatalf("profiles = %v", h.store.profiles[ToolClaude])
	}
	if !bytes.Contains(h.store.profiles[ToolClaude]["main"], []byte("main-access")) {
		t.Fatal("an existing profile was touched")
	}
	if len(h.alerts) != 1 || !strings.Contains(h.alerts[0], "login-20261016T213005") || !strings.Contains(h.alerts[0], "changed from main to login-20261016T213005") {
		t.Fatalf("alerts = %v", h.alerts)
	}
	if HasRefreshToken(ToolClaude, h.read(target)) {
		t.Fatal("not stripped")
	}
}

func TestCodexLoginsFollowTheSameOrder(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolCodex, "work", codexFull("kim@example.test", "acct_kim", farFuture, secretRefresh), "", true)
	h.store.addProfile(ToolCodex, "home", codexFull("pat@example.test", "acct_pat", farFuture.Add(-time.Hour), secretRefresh), "", false)
	login := codexFull("pat@example.test", "acct_pat", farFuture, secretRefresh+"-2")
	target := h.target(ToolCodex, login)

	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.store.profiles[ToolCodex]["home"], login) || h.store.active[ToolCodex] != "home" {
		t.Fatalf("a codex login of a known account replaces that profile: %v active=%q", len(h.store.profiles[ToolCodex]), h.store.active[ToolCodex])
	}
	if got := h.read(target); HasRefreshToken(ToolCodex, got) || !bytes.Contains(got, []byte(`"refresh_token":""`)) {
		t.Fatalf("a codex target keeps an empty refresh_token key: %s", got)
	}
}

func TestCodexLoginOfAnUnknownAccountIsNew(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolCodex, "work", codexFull("kim@example.test", "acct_kim", farFuture, secretRefresh), "", true)
	h.target(ToolCodex, codexFull("lee@example.test", "acct_lee", farFuture, secretRefresh+"-3"))
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.store.imports) != 1 || !h.store.imports[0].Create {
		t.Fatalf("imports = %+v", h.store.imports)
	}
}

// ── idempotence and failure handling ─────────────────────────────────────────

func TestAnImportedLoginIsNotImportedAgain(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("main-access", farFuture), "alice@example.test", true)
	h.target(ToolClaude, claudeFull("carol-access", farFuture))
	h.claudeAccount("")

	h.writer.err = errors.New("disk full")
	if err := h.pass(); err == nil {
		t.Fatal("a failed strip must be reported")
	}
	if len(h.alerts) != 2 || !strings.HasPrefix(h.alerts[1], AlertLoginStripFailed) {
		t.Fatalf("alerts = %v", h.alerts)
	}
	profilesAfterFirst := len(h.store.profiles[ToolClaude])
	h.writer.err = nil
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.store.profiles[ToolClaude]) != profilesAfterFirst {
		t.Fatalf("an unidentifiable login that was stored but not yet stripped minted another profile: %v", h.store.profiles[ToolClaude])
	}
	// The second pass stores the same bytes again (idempotent) so the active
	// pointer is re-asserted, then strips.
	if len(h.store.imports) != 2 || h.store.imports[1].Create {
		t.Fatalf("imports = %+v", h.store.imports)
	}
	// Third pass: nothing left to do.
	h.alerts = nil
	if err := h.pass(); err != nil || len(h.store.imports) != 2 || len(h.alerts) != 0 {
		t.Fatalf("third pass: %v imports=%d alerts=%v", err, len(h.store.imports), h.alerts)
	}
}

func TestARescueThatCannotBeSavedBlocksTheImportAndTheStrip(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", nil, "", true)
	login := claudeFull("login-access", farFuture)
	target := h.target(ToolClaude, login)
	h.store.rescueErr = errors.New("read-only filesystem")
	if err := h.pass(); err == nil {
		t.Fatal("expected an error")
	}
	if len(h.store.imports) != 0 || len(h.writer.calls) != 0 {
		t.Fatal("nothing may be imported or stripped without a persisted rescue")
	}
	if !bytes.Equal(h.read(target), login) {
		t.Fatal("the target must be left as it was")
	}
	if len(h.alerts) != 1 || !strings.HasPrefix(h.alerts[0], AlertLoginImportFailed) {
		t.Fatalf("alerts = %v", h.alerts)
	}
}

func TestABusyAccountDefersTheImportQuietly(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("old", farFuture), "alice@example.test", true)
	login := claudeFull("new", farFuture)
	target := h.target(ToolClaude, login)
	h.claudeAccount("alice@example.test")
	h.store.holdErr = usages.ErrMaintenanceBusy
	if err := h.pass(); err != nil {
		t.Fatalf("a busy account is not an error: %v", err)
	}
	if len(h.store.imports) != 0 || !bytes.Equal(h.read(target), login) || len(h.alerts) != 0 {
		t.Fatalf("deferred import must change nothing: imports=%d alerts=%v", len(h.store.imports), h.alerts)
	}
	h.store.holdErr = nil
	if err := h.pass(); err != nil || len(h.store.imports) != 1 {
		t.Fatalf("the next pass must finish it: %v imports=%d", err, len(h.store.imports))
	}
}

func TestALinkedProfileIsNeverImportedInto(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("live", farFuture), "alice@example.test", true)
	h.store.linked["claude/main"] = true
	login := claudeFull("new", farFuture)
	target := h.target(ToolClaude, login)
	h.claudeAccount("alice@example.test")
	if err := h.pass(); err == nil {
		t.Fatal("expected the import to fail")
	}
	if !bytes.Equal(h.read(target), login) || len(h.writer.calls) != 0 {
		t.Fatal("a failed import must not strip the target")
	}
	if len(h.alerts) != 1 || !strings.HasPrefix(h.alerts[0], AlertLoginImportFailed) {
		t.Fatalf("alerts = %v", h.alerts)
	}
}

func TestANewLoginNameCollisionGetsASuffix(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("main", farFuture), "alice@example.test", true)
	h.store.addProfile(ToolClaude, "login-20261016T213005", claudeFull("other", farFuture), "dave@example.test", false)
	h.target(ToolClaude, claudeFull("carol", farFuture))
	h.claudeAccount("carol@example.test")
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.store.profiles[ToolClaude]["login-20261016T213005-2"]; !ok {
		t.Fatalf("profiles = %v", h.store.profiles[ToolClaude])
	}
}

func TestNoAlertOrLogCarriesAToken(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("main-access", farFuture), "alice@example.test", true)
	h.store.importErr = fmt.Errorf("write failed")
	h.target(ToolClaude, claudeFull("carol-access", farFuture))
	h.claudeAccount("carol@example.test")
	_ = h.pass()
	for _, alert := range h.alerts {
		if strings.Contains(alert, secretRefresh) || strings.Contains(alert, "carol-access") {
			t.Fatalf("an alert leaks a token: %s", alert)
		}
	}
	if len(h.alerts) == 0 {
		t.Fatal("expected an alert")
	}
}

// ── healing ──────────────────────────────────────────────────────────────────

func TestALaggingTargetIsRewrittenFromItsProfile(t *testing.T) {
	h := newHarness(t)
	fresh := claudeFull("fresh-access", farFuture)
	h.store.addProfile(ToolClaude, "main", fresh, "alice@example.test", true)
	stale := claudeTokenOnly("stale-access", farFuture.Add(-3*time.Hour))
	h.target(ToolClaude, stale)
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.writer.calls) != 1 || !bytes.Equal(h.writer.calls[0].full, fresh) {
		t.Fatalf("writes = %+v", h.writer.calls)
	}
	if len(h.store.imports) != 0 {
		t.Fatal("healing is not an import")
	}
}

func TestATargetHoldingTheProfilesTokenIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("stored", farFuture), "alice@example.test", true)
	h.target(ToolClaude, claudeTokenOnly("stored", farFuture))
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.writer.calls) != 0 {
		t.Fatalf("writes = %+v", h.writer.calls)
	}
	if h.store.lockRuns != 0 {
		t.Fatalf("an up-to-date target must not even take the lock: %d", h.store.lockRuns)
	}
}

func TestATargetWithAnotherTokenIsRewrittenEvenWhenItExpiresLater(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("stored", farFuture), "alice@example.test", true)
	// Identity decides, not expiry order: two Claude accounts are not told apart
	// by their expiry, so a longer-lived but different token is still wrong.
	h.target(ToolClaude, claudeTokenOnly("someone-elses", farFuture.Add(5*time.Hour)))
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.writer.calls) != 1 || h.writer.calls[0].imported {
		t.Fatalf("a healing write goes through WriteTarget, never the login-replacing one: %+v", h.writer.calls)
	}
}

func TestHealRechecksUnderTheSwitcherLockAndNeverOverwritesASwitch(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "a", claudeFull("a-refreshed", farFuture), "alice@example.test", true)
	h.store.addProfile(ToolClaude, "b", claudeFull("b-token", farFuture), "bob@example.test", false)
	target := h.target(ToolClaude, claudeTokenOnly("a-old", farFuture.Add(-time.Hour)))
	// Between the unlocked look (a's token is newer than the target's) and the
	// locked rewrite, a Switch to b completes: the pointer moves and the live
	// file is rewritten with b's token.
	h.store.beforeLock = func() {
		h.store.active[ToolClaude] = "b"
		if err := os.WriteFile(target.Path, claudeTokenOnly("b-token", farFuture), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.writer.calls) != 0 {
		t.Fatalf("heal wrote %d times over a completed switch: %+v", len(h.writer.calls), h.writer.calls)
	}
	if got := h.read(target); !bytes.Contains(got, []byte("b-token")) {
		t.Fatalf("the live file must keep the switched-to account's token: %s", got)
	}
	if h.store.lockRuns != 1 {
		t.Fatalf("the rewrite decision must be made under the switcher lock: %d", h.store.lockRuns)
	}
}

func TestHealSkipsATargetThatReceivedALoginMeanwhile(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("fresh", farFuture), "alice@example.test", true)
	target := h.target(ToolClaude, claudeTokenOnly("stale", farFuture.Add(-time.Hour)))
	h.store.beforeLock = func() {
		if err := os.WriteFile(target.Path, claudeFull("a-login", farFuture), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.writer.calls) != 0 {
		t.Fatalf("a login that lands during a heal belongs to the importer: %+v", h.writer.calls)
	}
}

func TestACodexTargetOfAnotherAccountIsRewrittenEvenWhenItIsNewer(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolCodex, "work", codexFull("kim@example.test", "acct_kim", farFuture, secretRefresh), "", true)
	h.target(ToolCodex, codexFull("pat@example.test", "acct_pat", farFuture.Add(48*time.Hour), ""))
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.writer.calls) != 1 {
		t.Fatalf("a straggler of another account must follow the active profile: %+v", h.writer.calls)
	}
}

func TestAMissingTargetIsNotResurrectedAndASymlinkFreeStoreIsNotRequired(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("stored", farFuture), "alice@example.test", true)
	h.target(ToolClaude, nil) // no file: the user logged out
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.writer.calls) != 0 {
		t.Fatal("a removed credential file is a logout and must not be recreated")
	}
}

func TestAnEmptyOrUnusableProfileHealsNothing(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", nil, "", true)
	h.target(ToolClaude, claudeTokenOnly("target", farFuture.Add(-time.Hour)))
	if err := h.pass(); err != nil || len(h.writer.calls) != 0 {
		t.Fatalf("%v %v", err, h.writer.calls)
	}
	h.store.linked["claude/main"] = true
	h.store.profiles[ToolClaude]["main"] = claudeFull("x", farFuture)
	if err := h.pass(); err != nil || len(h.writer.calls) != 0 {
		t.Fatalf("a linked profile is not a source: %v %v", err, h.writer.calls)
	}
}

func TestALockBusyHealIsRetriedNotReported(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("fresh", farFuture), "alice@example.test", true)
	h.target(ToolClaude, claudeTokenOnly("stale", farFuture.Add(-time.Hour)))
	h.writer.err = ErrLockBusy
	if err := h.pass(); err != nil {
		t.Fatalf("a worker holding the write lock is not an error: %v", err)
	}
	h.writer.err = nil
	if err := h.pass(); err != nil || len(h.writer.calls) != 2 {
		t.Fatalf("the next pass must rewrite: %v calls=%d", err, len(h.writer.calls))
	}
}

func TestTargetModesAreFixedTo0600(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("stored", farFuture), "alice@example.test", true)
	target := h.target(ToolClaude, claudeTokenOnly("current", farFuture))
	if err := os.Chmod(target.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(target.Path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
}

// ── loop ─────────────────────────────────────────────────────────────────────

func TestReadyGateSkipsEveryPass(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", nil, "", true)
	login := claudeFull("login", farFuture)
	target := h.target(ToolClaude, login)
	ready := false
	h.rec.Ready = func() bool { return ready }
	if err := h.pass(); err != nil || !bytes.Equal(h.read(target), login) || len(h.store.imports) != 0 {
		t.Fatal("a not-ready reconciler must do nothing")
	}
	ready = true
	if err := h.pass(); err != nil || len(h.store.imports) != 1 {
		t.Fatalf("%v %d", err, len(h.store.imports))
	}
}

func TestRunPassesImmediatelyAndStopsWithItsContext(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", nil, "", true)
	h.target(ToolClaude, claudeFull("login", farFuture))
	h.rec.Interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.rec.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for {
		h.store.mu.Lock()
		n := len(h.store.imports)
		h.store.mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the first pass did not run")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestAPassWithoutAStoreIsAnError(t *testing.T) {
	if err := (&Reconciler{}).Pass(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestALoginRotatedWhileItIsImportedIsImportedAgainBeforeTheStrip(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", nil, "", true)
	first := claudeFull("login-access", farFuture)
	rotated := []byte(strings.ReplaceAll(string(claudeFull("rotated-access", farFuture.Add(time.Hour))), secretRefresh, secretRefresh+"-ROTATED"))
	target := h.target(ToolClaude, first)
	h.store.onImport = func() {
		if err := os.WriteFile(target.Path, rotated, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.store.imports) != 2 || !bytes.Equal(h.store.profiles[ToolClaude]["main"], rotated) {
		t.Fatalf("the rotated pair must end up in the store, not the spent one: imports=%d", len(h.store.imports))
	}
	if len(h.writer.calls) != 1 || !bytes.Equal(h.writer.calls[0].full, rotated) {
		t.Fatalf("the strip must use the rotated credential: %+v", h.writer.calls)
	}
	if HasRefreshToken(ToolClaude, h.read(target)) {
		t.Fatal("not stripped")
	}
}

func TestATargetStrippedByAnotherWriterDuringTheImportIsNotWrittenAgain(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", nil, "", true)
	target := h.target(ToolClaude, claudeFull("login-access", farFuture))
	h.store.onImport = func() {
		if err := os.WriteFile(target.Path, claudeTokenOnly("login-access", farFuture), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.writer.calls) != 0 {
		t.Fatalf("nothing is left to strip: %+v", h.writer.calls)
	}
}

func TestAWriterThatCannotReplaceALoginFailsTheStripLoudly(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", nil, "", true)
	login := claudeFull("login", farFuture)
	target := h.target(ToolClaude, login)
	h.rec.Writer = plainWriter{}
	if err := h.pass(); err == nil {
		t.Fatal("expected the strip to fail")
	}
	if !bytes.Equal(h.read(target), login) {
		t.Fatal("the target must be untouched")
	}
	if len(h.alerts) != 1 || !strings.HasPrefix(h.alerts[0], AlertLoginStripFailed) {
		t.Fatalf("alerts = %v", h.alerts)
	}
}

// ── account identity (R2) ───────────────────────────────────────────────────

func writeAccountFile(t *testing.T, path, email string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"numStartups":1}`
	if email != "" {
		body = fmt.Sprintf(`{"numStartups":1,"oauthAccount":{"emailAddress":%q}}`, email)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeAccountFilePrefersTheOneBesideTheCredentials(t *testing.T) {
	home := isolate(t)
	creds := filepath.Join(home, ".claude", ".credentials.json")
	inDir := filepath.Join(home, ".claude", ".claude.json")
	atRoot := filepath.Join(home, ".claude.json")
	now := time.Now()
	// The deployment seen on a real host: sessions run with CLAUDE_CONFIG_DIR
	// set, so the real account file is in the config directory and the home-root
	// file is a stub that names nobody. The server runs without the variable.
	writeAccountFile(t, inDir, "alice@example.test", now)
	writeAccountFile(t, atRoot, "", now.Add(-time.Hour))
	if got := ClaudeAccountFileFor(creds, home); got != inDir {
		t.Fatalf("got %s, want the file beside the credentials", got)
	}
	if email := ClaudeAccountEmail(ClaudeAccountFileFor(creds, home)); email != "alice@example.test" {
		t.Fatalf("email = %q", email)
	}
	if _, bad := ClaudeAccountFilesDisagree(creds, home); bad {
		t.Fatal("a stub that names nobody is not a disagreement")
	}
}

func TestClaudeAccountFileFallsBackToTheDefaultLocation(t *testing.T) {
	home := isolate(t)
	creds := filepath.Join(home, ".claude", ".credentials.json")
	atRoot := filepath.Join(home, ".claude.json")
	writeAccountFile(t, atRoot, "bob@example.test", time.Now())
	if got := ClaudeAccountFileFor(creds, home); got != atRoot {
		t.Fatalf("got %s, want the home-root file", got)
	}
	if got := ClaudeAccountFileFor(filepath.Join(home, "nowhere", ".credentials.json"), t.TempDir()); !strings.HasSuffix(got, ".claude.json") {
		t.Fatalf("with no file anywhere the default location is returned: %s", got)
	}
}

func TestClaudeAccountFileTakesTheNewestWhenBothNameAnAccount(t *testing.T) {
	home := isolate(t)
	creds := filepath.Join(home, ".claude", ".credentials.json")
	inDir := filepath.Join(home, ".claude", ".claude.json")
	atRoot := filepath.Join(home, ".claude.json")
	now := time.Now()
	writeAccountFile(t, inDir, "old@example.test", now.Add(-2*time.Hour))
	writeAccountFile(t, atRoot, "new@example.test", now)
	if got := ClaudeAccountFileFor(creds, home); got != atRoot {
		t.Fatalf("a login rewrites the file its process uses, so the newest wins: %s", got)
	}
	sources, bad := ClaudeAccountFilesDisagree(creds, home)
	if !bad || len(sources) != 2 {
		t.Fatalf("two files naming different accounts must be flagged for the doctor: %v %v", sources, bad)
	}
	writeAccountFile(t, atRoot, "OLD@example.test", now)
	if _, bad := ClaudeAccountFilesDisagree(creds, home); bad {
		t.Fatal("the same account in both files, differing only in case, is not a disagreement")
	}
}

func TestTheReconcilerIdentifiesTheLoginFromTheFileBesideTheCredentials(t *testing.T) {
	h := newHarness(t)
	old := claudeFull("old-access", farFuture.Add(-time.Hour))
	h.store.addProfile(ToolClaude, "main", old, "alice@example.test", true)
	login := claudeFull("new-access", farFuture)
	h.target(ToolClaude, login)
	writeAccountFile(t, filepath.Join(h.home, ".claude", ".claude.json"), "alice@example.test", time.Now())
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.store.profiles[ToolClaude]["main"], login) || len(h.store.profiles[ToolClaude]) != 1 {
		t.Fatalf("a same-account login must replace the active profile, not mint a new one: %v", len(h.store.profiles[ToolClaude]))
	}
}

// ── unmigrated installs (R3) ─────────────────────────────────────────────────

func TestANotMigratedInstallAlertsOncePerToolNotEveryPass(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("live", farFuture), "alice@example.test", true)
	h.store.linked["claude/main"] = true
	h.target(ToolClaude, claudeFull("live", farFuture))
	for i := 0; i < 3; i++ {
		if err := h.pass(); !errors.Is(err, ErrNotMigrated) {
			t.Fatalf("pass %d = %v", i, err)
		}
	}
	if len(h.alerts) != 1 {
		t.Fatalf("the same unmigrated state alerted %d times: %v", len(h.alerts), h.alerts)
	}
	if len(h.store.rescues) != 0 {
		t.Fatalf("an unmigrated install must not save a rescue copy of its live login on every pass: %d", len(h.store.rescues))
	}
	// Once the condition clears and returns, it alerts again.
	h.store.linked["claude/main"] = false
	h.store.profiles[ToolClaude]["main"] = nil
	if err := os.WriteFile(h.targets[0].Path, claudeTokenOnly("live", farFuture), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = h.pass()
	h.store.linked["claude/main"] = true
	h.store.profiles[ToolClaude]["main"] = claudeFull("live", farFuture)
	if err := os.WriteFile(h.targets[0].Path, claudeFull("live", farFuture), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = h.pass()
	if len(h.alerts) != 2 {
		t.Fatalf("alerts = %v", h.alerts)
	}
}

func TestAReconcilerWithoutAReadyGateSaysSoOnce(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("stored", farFuture), "alice@example.test", true)
	h.target(ToolClaude, claudeTokenOnly("stored", farFuture))
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	for i := 0; i < 4; i++ {
		if err := h.pass(); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "no Ready gate"); n != 1 {
		t.Fatalf("the missing gate was logged %d times: %q", n, logs.String())
	}
}

// ── review fixes: stale account file, settle, removal, pointer, read errors ───

func TestAStaleAccountFileIsNotTrustedAndNeverReplacesTheActiveProfile(t *testing.T) {
	h := newHarness(t)
	old := claudeFull("alice-access", farFuture.Add(-time.Hour))
	h.store.addProfile(ToolClaude, "main", old, "alice@example.test", true)
	login := claudeFull("carol-access", farFuture) // another account just logged in
	target := h.target(ToolClaude, login)
	// claude has written the credentials but not yet the account file, which
	// still names the previous account.
	writeAccountFile(t, filepath.Join(h.home, ".claude", ".claude.json"), "alice@example.test", time.Now().Add(-time.Hour))
	if err := os.Chtimes(target.Path, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.store.profiles[ToolClaude]["main"], old) {
		t.Fatal("alice's stored login was replaced by another account's")
	}
	if _, ok := h.store.profiles[ToolClaude]["login-20261016T213005"]; !ok {
		t.Fatalf("an unidentifiable login becomes its own profile: %v", h.store.profiles[ToolClaude])
	}
}

func TestATargetThatJustChangedWaitsForTheLoginToSettle(t *testing.T) {
	h := newHarness(t)
	h.rec.ImportSettle = time.Minute
	h.store.addProfile(ToolClaude, "main", nil, "", true)
	login := claudeFull("login", farFuture)
	target := h.target(ToolClaude, login)
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.store.imports) != 0 || len(h.store.rescues) != 0 || !bytes.Equal(h.read(target), login) {
		t.Fatal("a target written moments ago must be left for a later pass")
	}
	past := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(target.Path, past, past); err != nil {
		t.Fatal(err)
	}
	if err := h.pass(); err != nil || len(h.store.imports) != 1 {
		t.Fatalf("a settled login must be filed: %v imports=%d", err, len(h.store.imports))
	}
}

func TestATargetRemovedWhileItsLoginIsImportedStaysRemoved(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", nil, "", true)
	login := claudeFull("login", farFuture)
	target := h.target(ToolClaude, login)
	h.store.onImport = func() {
		if err := os.Remove(target.Path); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the user logged out meanwhile; the file must not come back")
	}
	if len(h.writer.calls) != 0 {
		t.Fatalf("no write may follow a removal: %+v", h.writer.calls)
	}
	if !bytes.Equal(h.store.profiles[ToolClaude]["main"], login) {
		t.Fatal("the login itself is kept in its profile")
	}
}

func TestAnAlreadyImportedLoginReassertsTheActivePointer(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("main", farFuture), "alice@example.test", true)
	h.store.addProfile(ToolClaude, "bob", claudeFull("bob-old", farFuture.Add(-time.Hour)), "bob@example.test", false)
	login := claudeFull("bob-new", farFuture)
	target := h.target(ToolClaude, login)
	h.claudeAccount("bob@example.test")
	// The first attempt stores bob's credential but fails before the pointer
	// moves; the target is not stripped.
	h.store.failAfterWrite = true
	if err := h.pass(); err == nil {
		t.Fatal("expected the first attempt to fail")
	}
	if h.store.active[ToolClaude] != "main" || !HasRefreshToken(ToolClaude, h.read(target)) {
		t.Fatalf("after the failure: active=%q", h.store.active[ToolClaude])
	}
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if h.store.active[ToolClaude] != "bob" {
		t.Fatalf("the next pass must finish the import: active=%q (heal would otherwise rewrite the live file from main)", h.store.active[ToolClaude])
	}
	if HasRefreshToken(ToolClaude, h.read(target)) {
		t.Fatal("not stripped")
	}
}

func TestAnUnreadableActiveProfileIsNotTakenForEmpty(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("main", farFuture), "alice@example.test", true)
	h.store.readErr = map[string]error{"claude/main": errors.New("permission denied")}
	login := claudeFull("login", farFuture)
	target := h.target(ToolClaude, login)
	if err := h.pass(); err == nil {
		t.Fatal("expected the import to be refused")
	}
	if len(h.store.imports) != 0 || !bytes.Equal(h.read(target), login) {
		t.Fatal("a profile that cannot be read must not be overwritten or its target stripped")
	}
	if len(h.alerts) != 1 || !strings.HasPrefix(h.alerts[0], AlertLoginImportFailed) {
		t.Fatalf("alerts = %v", h.alerts)
	}
}

func TestAWorkerAddingAnMCPLoginDuringTheImportIsNotARotation(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", nil, "", true)
	login := claudeFull("login", farFuture)
	target := h.target(ToolClaude, login)
	h.store.onImport = func() {
		withMCP := strings.Replace(string(login), `"scopes"`, `"scopes"`, 1)
		withMCP = strings.TrimSuffix(withMCP, "}") + `,"mcpOAuth":{"plane":{"accessToken":"mcp"}}}`
		if err := os.WriteFile(target.Path, []byte(withMCP), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.store.imports) != 1 {
		t.Fatalf("only the token pair counts as a change: imports=%d", len(h.store.imports))
	}
	if got := h.read(target); HasRefreshToken(ToolClaude, got) {
		t.Fatal("not stripped")
	}
}

func TestAnUnmigratedInstallNeverSavesARescueOrTouchesTheTarget(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", claudeFull("live", farFuture), "alice@example.test", true)
	h.store.linked["claude/main"] = true
	live := claudeFull("live", farFuture)
	target := h.target(ToolClaude, live)
	for i := 0; i < 3; i++ {
		_ = h.pass()
	}
	if len(h.store.rescues) != 0 || len(h.store.imports) != 0 || len(h.writer.calls) != 0 || !bytes.Equal(h.read(target), live) {
		t.Fatalf("rescues=%d imports=%d writes=%d", len(h.store.rescues), len(h.store.imports), len(h.writer.calls))
	}
}

func TestAStripRunsUnderTheSwitcherLockAndYieldsToALoginThatChangedMeanwhile(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolClaude, "main", nil, "", true)
	first := claudeFull("login", farFuture)
	rotated := []byte(strings.ReplaceAll(string(claudeFull("rotated", farFuture.Add(time.Hour))), secretRefresh, secretRefresh+"-2"))
	target := h.target(ToolClaude, first)
	// Just before the strip takes the lock, the session rotates its token pair.
	h.store.beforeLock = func() {
		if err := os.WriteFile(target.Path, rotated, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	// The changed pair is imported again into the same profile within the pass,
	// and only then stripped: the spent pair must not stay in the store.
	if !bytes.Equal(h.store.profiles[ToolClaude]["main"], rotated) || len(h.store.profiles[ToolClaude]) != 1 {
		t.Fatalf("the rotated pair belongs in the same profile: %v", len(h.store.profiles[ToolClaude]))
	}
	if len(h.writer.calls) != 1 || !bytes.Equal(h.writer.calls[0].full, rotated) || HasRefreshToken(ToolClaude, h.read(target)) {
		t.Fatalf("the strip must use the rotated pair: %+v", h.writer.calls)
	}
	if h.store.lockRuns < 2 {
		t.Fatalf("every strip runs under the switcher lock: %d", h.store.lockRuns)
	}
}

// After `migrate-copies --tool claude` the codex live file still holds the full
// credential of the link model: the reconciler must not take it for a login.
func TestReadyToolLeavesAnUnmigratedToolAlone(t *testing.T) {
	h := newHarness(t)
	h.store.addProfile(ToolCodex, "work", codexFull("kim@example.test", "acct_kim", farFuture, secretRefresh), "", true)
	login := codexFull("kim@example.test", "acct_kim", farFuture, secretRefresh+"-2")
	target := h.target(ToolCodex, login)
	h.rec.ReadyTool = func(tool string) bool { return tool == ToolClaude }

	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.read(target), login) || len(h.alerts) != 0 || h.store.holds != 0 {
		t.Fatalf("an unmigrated tool was touched: alerts=%v holds=%d", h.alerts, h.store.holds)
	}
	h.rec.ReadyTool = func(string) bool { return true }
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if HasRefreshToken(ToolCodex, h.read(target)) {
		t.Fatal("once the tool is migrated the login is imported and stripped")
	}
}

// A failed un-migration can leave the live file holding the stored credential's
// own refresh token plus extra keys (MCP logins). That is the same grant, so the
// reconciler stores it back into that profile: no new profile, no pointer move,
// no alert, and the live file is stripped again.
func TestALoginWithAStoredProfilesRefreshTokenIsThatProfile(t *testing.T) {
	h := newHarness(t)
	stored := claudeFull("main-access", farFuture)
	h.store.addProfile(ToolClaude, "main", stored, "", true)
	h.store.addProfile(ToolClaude, "spare", claudeFull("spare-access", farFuture), "", false)
	live := bytes.Replace(stored, []byte("}}"), []byte(`},"mcpOAuth":{"plane":{"accessToken":"mcp"}}`+"}"), 1)
	target := h.target(ToolClaude, live)
	h.claudeAccount("")

	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if len(h.store.profiles[ToolClaude]) != 2 {
		t.Fatalf("the exposed credential was filed as a new profile: %v", h.store.profiles[ToolClaude])
	}
	if h.store.active[ToolClaude] != "main" {
		t.Fatalf("active moved to %q", h.store.active[ToolClaude])
	}
	if !bytes.Contains(h.store.profiles[ToolClaude]["main"], []byte("mcpOAuth")) {
		t.Fatal("the MCP logins were not kept in the stored document")
	}
	if len(h.alerts) != 0 {
		t.Fatalf("alerts = %v", h.alerts)
	}
	if bytes.Contains(h.read(target), []byte("refreshToken")) {
		t.Fatal("the live file still holds the refresh token")
	}
}

// Two profiles holding the same grant (one login stored under two names): the
// active one takes the login even when it is not first in the list, so the
// active pointer never moves to the duplicate.
func TestASameTokenLoginPrefersTheActiveProfile(t *testing.T) {
	h := newHarness(t)
	stored := claudeFull("shared-access", farFuture)
	h.store.addProfile(ToolClaude, "a-dup", stored, "", false)
	h.store.addProfile(ToolClaude, "z-main", stored, "", true)
	live := bytes.Replace(stored, []byte("}}"), []byte(`},"mcpOAuth":{"plane":{"accessToken":"mcp"}}`+"}"), 1)
	h.target(ToolClaude, live)
	h.claudeAccount("")

	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if h.store.active[ToolClaude] != "z-main" {
		t.Fatalf("active moved to %q", h.store.active[ToolClaude])
	}
	if !bytes.Contains(h.store.profiles[ToolClaude]["z-main"], []byte("mcpOAuth")) || bytes.Contains(h.store.profiles[ToolClaude]["a-dup"], []byte("mcpOAuth")) {
		t.Fatalf("the login must land in the active profile only: %v", h.store.profiles[ToolClaude])
	}
}

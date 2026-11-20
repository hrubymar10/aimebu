package server

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/quota"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// The quota switcher over the real server, store, switcher and usage reader. The
// usage snapshots are the one fake: a Reader's own snapshot cannot be made fresh
// without a provider endpoint, so the runner tests feed observations through a fake
// Source while the actuator, the events, the persistence and the API are the real
// ones. All credentials are fake and live under t.TempDir().

// quotaEnv is a migrated claude install with two profiles: A (active) and B, both
// with a six-hour token.
func quotaEnv(t *testing.T) (f *fixture, m *switcher.Manager, live string) {
	t.Helper()
	f, m, live = startupEnv(t, 6*time.Hour)
	if err := m.Add(switcher.ToolClaude, "B"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.StoredCredPath(switcher.ToolClaude, "B"), []byte(startupClaudeCreds("acc-B0", "ref-B0", time.Now().Add(6*time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	migrateClaude(t, m)
	reader, err := usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.useUsages(reader)
	return f, m, live
}

type quotaFakeSource struct {
	mu    sync.Mutex
	obs   quota.Observation
	names []string // the profiles that exist; A and B when empty
}

func (s *quotaFakeSource) set(o quota.Observation) { s.mu.Lock(); s.obs = o; s.mu.Unlock() }
func (s *quotaFakeSource) Observe(context.Context, string) (quota.Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.obs
	o.Profiles = append([]quota.Obs(nil), o.Profiles...)
	return o, nil
}
func (s *quotaFakeSource) Profiles(context.Context, string) ([]string, error) {
	if len(s.names) > 0 {
		return s.names, nil
	}
	return []string{"A", "B"}, nil
}

func qobs(name string, session, weekly float64) quota.Obs {
	exp := time.Now().Add(6 * time.Hour)
	return quota.Obs{Name: name, Status: "ok", Session: &session, Weekly: &weekly, HasCredentials: true, TokenExpiresAt: &exp}
}

// runnerOver wires a runner over the real actuator and events, a fake source and a
// controllable clock, and installs it on the server so the API shows it.
func runnerOver(t *testing.T, f *fixture, src quota.Source) (*quota.Runner, *time.Time) {
	t.Helper()
	runner := quota.NewRunner(f.srv.Store, src, quotaActuator{s: f.srv}, quotaEvents{f.srv})
	now := time.Now()
	runner.SetClock(func() time.Time { return now })
	f.srv.quota.Store(runner)
	return runner, &now
}

func quotaConfig(t *testing.T, runner *quota.Runner, mutate func(*quota.Config)) {
	t.Helper()
	cfg := quota.Default(quota.ToolClaude, []string{"A", "B"})
	cfg.Enabled, cfg.DryRun, cfg.WarmupSeconds, cfg.MinDwellMinutes = true, false, 0, 0
	if mutate != nil {
		mutate(&cfg)
	}
	if _, err := runner.SaveConfig(context.Background(), quota.ToolClaude, cfg); err != nil {
		t.Fatal(err)
	}
}

func getQuota(t *testing.T, f *fixture) (int, quota.Overview, string) {
	t.Helper()
	resp := f.request("GET", "/api/quota-switcher", nil, false)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var o quota.Overview
	_ = json.Unmarshal(raw, &o)
	return resp.StatusCode, o, string(raw)
}

func activeProfile(t *testing.T, m *switcher.Manager) string {
	t.Helper()
	a, err := m.Active(switcher.ToolClaude)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// systemEvents returns the owner's system-events feed (the events, not the list of
// kinds the response also carries, which names every kind whether or not one happened).
func systemEvents(t *testing.T, f *fixture) []struct {
	Kind        string `json:"kind"`
	PayloadJSON string `json:"payload_json"`
} {
	t.Helper()
	resp := f.request("GET", "/api/system-events", nil, false)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var body struct {
		Events []struct {
			Kind        string `json:"kind"`
			PayloadJSON string `json:"payload_json"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("system events: %v %s", err, raw)
	}
	return body.Events
}

// ── the API ─────────────────────────────────────────────────────────────────

// The usages update hook runs under the usage reader's lock and must not wait for refresherMu, which
// StartTokenCopies holds while it takes the reader's lock: that would be a lock-order inversion (a deadlock
// when both run at once).
func TestTheUsagesHookNeverWaitsForTheRefresherLock(t *testing.T) {
	f, _, _ := quotaEnv(t)
	f.srv.quota.Store(quota.NewRunner(f.srv.Store, &quotaFakeSource{}, quotaActuator{s: f.srv}, quotaEvents{f.srv}))
	f.srv.refresherMu.Lock()
	done := make(chan struct{})
	go func() { f.srv.usagesChanged(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		f.srv.refresherMu.Unlock()
		t.Fatal("usagesChanged waits for refresherMu")
	}
	f.srv.refresherMu.Unlock()
}

func TestQuotaRoutesAre503UntilTheRunnerExistsAndOwnerOnly(t *testing.T) {
	f, _, _ := quotaEnv(t)
	if code, _, _ := getQuota(t, f); code != 503 {
		t.Fatalf("before the runner: %d, want 503", code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if f.srv.StartQuotaSwitcher(ctx) == nil {
		t.Fatal("StartQuotaSwitcher built nothing")
	}
	if again := f.srv.StartQuotaSwitcher(ctx); again != f.srv.quotaRunner() {
		t.Fatal("a second start must return the same runner")
	}
	if code, _, _ := getQuota(t, f); code != 200 {
		t.Fatalf("after the runner: %d", code)
	}
	anon, err := http.Get(f.http.URL + "/api/quota-switcher")
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != 401 {
		t.Fatalf("anonymous = %d, want 401", anon.StatusCode)
	}
}

func TestQuotaRoutesRoundTripValidateAndClear(t *testing.T) {
	f, _, _ := quotaEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.srv.StartQuotaSwitcher(ctx)

	code, o, raw := getQuota(t, f)
	if code != 200 || len(o.Profiles["claude"]) != 2 || o.Tools["claude"].Saved || o.Tools["claude"].Config.Enabled {
		t.Fatalf("overview: %d %s", code, raw)
	}
	cl := o.Tools["claude"].Config
	if !cl.DryRun || len(cl.Profiles) != 2 || cl.Profiles[0].SessionMax != 95 || cl.WarmupSeconds != 60 || cl.MinDwellMinutes != 5 {
		t.Fatalf("pre-filled claude config = %+v", cl)
	}
	for _, field := range []string{`"protected":[]`, `"decisions":[]`, `"last_decision":null`, `"after_reset":null`} {
		if !strings.Contains(raw, field) {
			t.Fatalf("the shape lacks %s: %s", field, raw)
		}
	}

	put := func(tool string, body any) (int, string) {
		resp := f.request("PUT", "/api/quota-switcher/"+tool, body, true)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	bad := cl
	bad.Profiles = []quota.Limit{{Name: "ghost", SessionMax: 95, WeeklyMax: 95}, {Name: "A", SessionMax: 95, WeeklyMax: 95}}
	if code, body := put("claude", bad); code != 400 || !strings.Contains(body, "ghost") {
		t.Fatalf("an unknown profile: %d %s", code, body)
	}
	if code, _ := put("pi", cl); code != 404 {
		t.Fatalf("an unknown tool: %d", code)
	}
	good := cl
	good.Enabled, good.DryRun = true, true
	good.Profiles = []quota.Limit{{Name: "B", SessionMax: 90, WeeklyMax: 80}, {Name: "A", SessionMax: 95, WeeklyMax: 95}}
	if code, body := put("claude", good); code != 200 || !strings.Contains(body, `"saved":true`) {
		t.Fatalf("a valid save: %d %s", code, body)
	}
	if _, o, _ := getQuota(t, f); !o.Tools["claude"].Saved || o.Tools["claude"].Config.Profiles[0].Name != "B" || o.Tools["claude"].Config.Profiles[0].WeeklyMax != 80 {
		t.Fatalf("the save did not stick: %+v", o.Tools["claude"].Config)
	}

	for _, action := range []string{"rearm", "reenable"} {
		resp := f.request("POST", "/api/quota-switcher/claude/"+action, map[string]any{}, true)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), `"decisions"`) {
			t.Fatalf("%s: %d %s", action, resp.StatusCode, b)
		}
	}
	if resp := f.request("POST", "/api/quota-switcher/pi/rearm", map[string]any{}, true); resp.StatusCode != 404 {
		t.Fatalf("an unknown tool rearm: %d", resp.StatusCode)
	}
	// A write needs the CSRF token.
	resp := f.request("PUT", "/api/quota-switcher/claude", good, false)
	resp.Body.Close()
	if resp.StatusCode != 403 && resp.StatusCode != 401 {
		t.Fatalf("a write without CSRF = %d", resp.StatusCode)
	}
}

// ── the switch itself ───────────────────────────────────────────────────────

func TestAQuotaSwitchGoesThroughTheRealSwitcherAndIsRecordedAndAnnounced(t *testing.T) {
	f, m, live := quotaEnv(t)
	src := &quotaFakeSource{}
	runner, _ := runnerOver(t, f, src)
	quotaConfig(t, runner, nil)
	// A is exhausted, B is fine: the order is A, B (defaults) so B is wanted.
	src.set(quota.Observation{Active: "A", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 99, 99), qobs("B", 10, 10)}})
	runner.Evaluate(context.Background())

	if got := activeProfile(t, m); got != "B" {
		t.Fatalf("active = %s, want B", got)
	}
	if data := fileString(t, live); !strings.Contains(data, "acc-B0") || refreshTokenInFile.MatchString(data) {
		t.Fatalf("the live file is not B's token-only copy: %s", data)
	}
	_, o, _ := getQuota(t, f)
	last := o.Tools["claude"].LastDecision
	if last == nil || last.Action != quota.ActionSwitch || *last.From != "A" || *last.To != "B" || last.Reason != quota.ReasonOverLimit || last.DryRun {
		t.Fatalf("last decision = %+v", last)
	}
	if o.Tools["claude"].State.LastSwitchAt == nil {
		t.Fatal("the switch time was not persisted")
	}
	var announced bool
	for _, event := range systemEvents(t, f) {
		announced = announced || (event.Kind == "quota_switch" && strings.Contains(event.PayloadJSON, `"to":"B"`) && strings.Contains(event.PayloadJSON, `"from":"A"`))
	}
	if !announced {
		t.Fatalf("the owner's feed lacks the switch event: %+v", systemEvents(t, f))
	}
}

// ownerSwitchesFirst lets the owner move to another profile after the runner has decided and before
// the actuator runs, which is the gap a slow account wait or incoming refresh opens in production.
type ownerSwitchesFirst struct {
	inner quota.Actuator
	m     *switcher.Manager
	to    string
	moved bool
}

func (a *ownerSwitchesFirst) Switch(ctx context.Context, tool, from, to string) (bool, string, error) {
	if !a.moved {
		a.moved = true
		if _, _, err := a.m.Switch(tool, a.to); err != nil {
			return false, "", err
		}
	}
	return a.inner.Switch(ctx, tool, from, to)
}

// A manual switch to a protected profile between the decision and the switch must stand: the actuator
// holds ("the active profile changed") instead of switching C away to the target the runner chose for A.
func TestAManualSwitchBetweenDecisionAndActuatorIsNotOverridden(t *testing.T) {
	f, m, _ := quotaEnv(t)
	if err := m.Add(switcher.ToolClaude, "C"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.StoredCredPath(switcher.ToolClaude, "C"), []byte(startupClaudeCreds("acc-C0", "ref-C0", time.Now().Add(6*time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	migrateClaude(t, m)
	src := &quotaFakeSource{names: []string{"A", "B", "C"}}
	runner := quota.NewRunner(f.srv.Store, src, &ownerSwitchesFirst{inner: quotaActuator{s: f.srv}, m: m, to: "C"}, quotaEvents{f.srv})
	runner.SetClock(time.Now)
	f.srv.quota.Store(runner)
	quotaConfig(t, runner, func(c *quota.Config) { c.Protected = []string{"C"} })
	src.set(quota.Observation{Active: "A", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 99, 99), qobs("B", 10, 10)}})
	runner.Evaluate(context.Background())

	if got := activeProfile(t, m); got != "C" {
		t.Fatalf("active = %s: the owner's switch to the protected profile C was overridden", got)
	}
	_, o, _ := getQuota(t, f)
	if d := o.Tools["claude"].LastDecision; d == nil || d.Action != quota.ActionHold || d.Reason != quota.ReasonActiveChanged {
		t.Fatalf("decision = %+v", d)
	}
	if o.Tools["claude"].State.LastSwitchAt != nil {
		t.Fatal("a refused switch must not move the dwell")
	}
	for _, event := range systemEvents(t, f) {
		if event.Kind == "quota_switch" {
			t.Fatalf("no switch happened, yet %s was announced", event.Kind)
		}
	}
}

func TestADryRunNeverTouchesTheSwitcher(t *testing.T) {
	f, m, live := quotaEnv(t)
	before := fileString(t, live)
	src := &quotaFakeSource{}
	runner, _ := runnerOver(t, f, src)
	quotaConfig(t, runner, func(c *quota.Config) { c.DryRun = true })
	src.set(quota.Observation{Active: "A", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 99, 99), qobs("B", 10, 10)}})
	runner.Evaluate(context.Background())
	if activeProfile(t, m) != "A" || fileString(t, live) != before {
		t.Fatal("a dry run changed the active profile or the live file")
	}
	_, o, _ := getQuota(t, f)
	if d := o.Tools["claude"].LastDecision; d == nil || d.Action != quota.ActionSwitch || !d.DryRun {
		t.Fatalf("decision = %+v", d)
	}
	for _, event := range systemEvents(t, f) {
		if strings.HasPrefix(event.Kind, "quota_") {
			t.Fatalf("a dry run announces nothing, got %s", event.Kind)
		}
	}
}

func TestARunningTurnHoldsTheSwitchUntilTheAgentIsIdle(t *testing.T) {
	f, m, _ := quotaEnv(t)
	agent, turn, _, _ := f.execution(t)
	setAgent := func(observed, exec string) {
		t.Helper()
		if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
			if _, err := tx.Exec(`UPDATE agent SET tool='claude',observed=? WHERE id=?`, observed, agent.ID); err != nil {
				return err
			}
			_, err := tx.Exec(`UPDATE turn SET exec=? WHERE id=?`, exec, turn.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	setAgent("in_turn", "running")
	src := &quotaFakeSource{}
	runner, now := runnerOver(t, f, src)
	quotaConfig(t, runner, nil)
	src.set(quota.Observation{Active: "A", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 99, 99), qobs("B", 10, 10)}})
	runner.Evaluate(context.Background())
	if activeProfile(t, m) != "A" {
		t.Fatal("switched under a running turn")
	}
	_, o, _ := getQuota(t, f)
	if d := o.Tools["claude"].LastDecision; d == nil || d.Action != quota.ActionHold || d.Reason != quota.ReasonAgentsRunning {
		t.Fatalf("decision = %+v", d)
	}
	if o.Tools["claude"].State.Hold != quota.ReasonAgentsRunning {
		t.Fatalf("state.hold = %q", o.Tools["claude"].State.Hold)
	}
	setAgent("idle", "exited")
	*now = now.Add(31 * time.Second) // past the 30 s back-off
	runner.Evaluate(context.Background())
	if got := activeProfile(t, m); got != "B" {
		t.Fatalf("after the turn ended the switch was not made: active = %s", got)
	}
}

// The unlocked pre-check passes, then a turn starts before the switch gets the lock: the switch must be
// refused under the lock (agents running), not swap credentials under the new turn.
func TestATurnThatStartsAfterThePreCheckStillHoldsTheSwitch(t *testing.T) {
	f, m, _ := quotaEnv(t)
	agent, turn, _, _ := f.execution(t)
	setAgent := func(observed, exec string) {
		t.Helper()
		if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
			if _, err := tx.Exec(`UPDATE agent SET tool='claude',observed=? WHERE id=?`, observed, agent.ID); err != nil {
				return err
			}
			_, err := tx.Exec(`UPDATE turn SET exec=? WHERE id=?`, exec, turn.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	setAgent("idle", "exited")
	src := &quotaFakeSource{}
	runner := quota.NewRunner(f.srv.Store, src, quotaActuator{s: f.srv, afterPreCheck: func() { setAgent("in_turn", "running") }}, quotaEvents{f.srv})
	runner.SetClock(time.Now)
	quotaConfig(t, runner, nil)
	src.set(quota.Observation{Active: "A", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 99, 99), qobs("B", 10, 10)}})
	f.srv.quota.Store(runner)
	runner.Evaluate(context.Background())
	if got := activeProfile(t, m); got != "A" {
		t.Fatalf("active = %s: credentials were swapped under a turn that started after the pre-check", got)
	}
	_, o, _ := getQuota(t, f)
	if d := o.Tools["claude"].LastDecision; d == nil || d.Action != quota.ActionHold || d.Reason != quota.ReasonAgentsRunning {
		t.Fatalf("decision = %+v", d)
	}
}

// The web switch has the same gap as the quota actuator: its unlocked check passes, a turn starts, then the
// switch gets the lock. The guard under the lock must refuse it (409, nothing swapped).
func TestTheWebSwitchRechecksRunningTurnsUnderTheLock(t *testing.T) {
	f, m, _ := quotaEnv(t)
	agent, turn, _, _ := f.execution(t)
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE agent SET tool='claude',observed='idle' WHERE id=?`, agent.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE turn SET exec='exited' WHERE id=?`, turn.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.srv.switcherAfterPreCheck = func() {
		if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
			if _, err := tx.Exec(`UPDATE agent SET observed='in_turn' WHERE id=?`, agent.ID); err != nil {
				return err
			}
			_, err := tx.Exec(`UPDATE turn SET exec='running' WHERE id=?`, turn.ID)
			return err
		}); err != nil {
			t.Error(err)
		}
	}
	resp := f.request(http.MethodPost, "/api/switcher/claude/use", map[string]string{"name": "B"}, true)
	var body struct {
		Error  string                `json:"error"`
		Agents []store.SwitcherAgent `json:"agents"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 409 || body.Error != "agents_running" || len(body.Agents) != 1 {
		t.Fatalf("status=%d body=%+v: a turn that started after the pre-check must refuse the switch", resp.StatusCode, body)
	}
	if got := activeProfile(t, m); got != "A" {
		t.Fatalf("active = %s: credentials were swapped under the turn", got)
	}
}

// A failure of the quiescence check itself is a server fault: it answers 500 without echoing the store's text, never a
// 400 carrying raw database wording.
func TestAFailingQuiescenceCheckAnswersAsAnInternalError(t *testing.T) {
	f, m, _ := quotaEnv(t)
	agent, turn, _, _ := f.execution(t)
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE agent SET tool='claude',observed='idle' WHERE id=?`, agent.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE turn SET exec='exited' WHERE id=?`, turn.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.srv.switcherAfterPreCheck = func() {
		if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
			_, err := tx.Exec(`ALTER TABLE agent RENAME TO agent_gone`)
			return err
		}); err != nil {
			t.Error(err)
		}
	}
	resp := f.request(http.MethodPost, "/api/switcher/claude/use", map[string]string{"name": "B"}, true)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 || strings.Contains(string(raw), "no such table") || strings.Contains(string(raw), "agent_gone") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if got := activeProfile(t, m); got != "A" {
		t.Fatalf("active = %s", got)
	}
}

// A login waiting in the live file makes the switcher refuse (ErrLoginPending): a
// retryable hold with a fixed reason, never an error, and nothing about the file
// reaches the recorded decision.
func TestAPendingLoginHoldsTheSwitchWithAFixedReason(t *testing.T) {
	f, m, live := quotaEnv(t)
	if err := os.WriteFile(live, []byte(startupClaudeCreds("acc-login", "ref-login", time.Now().Add(6*time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	src := &quotaFakeSource{}
	runner, _ := runnerOver(t, f, src)
	quotaConfig(t, runner, nil)
	src.set(quota.Observation{Active: "A", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 99, 99), qobs("B", 10, 10)}})
	runner.Evaluate(context.Background())
	if activeProfile(t, m) != "A" {
		t.Fatal("switched over a login waiting in the live file")
	}
	_, o, raw := getQuota(t, f)
	if d := o.Tools["claude"].LastDecision; d == nil || d.Action != quota.ActionHold || d.Reason != quota.ReasonLoginPending {
		t.Fatalf("decision = %+v", d)
	}
	if strings.Contains(raw, "credentials.json") || strings.Contains(raw, "ref-login") {
		t.Fatalf("file or token text leaked into the API: %s", raw)
	}
}

// ── the source ──────────────────────────────────────────────────────────────

// A snapshot read from the usage cache reaches the runner as stale (the reader marks
// anything it did not fetch itself stale), with the windows mapped by key and the
// active profile named.
func TestQuotaSourceMapsTheUsageSnapshotAndTheSwitcher(t *testing.T) {
	f, m, _ := quotaEnv(t)
	exp := time.Now().Add(time.Hour).UTC()
	// The reader restores its cache by position: codex first, then claude-code.
	snapshot := usages.Response{Providers: []usages.Provider{{ProviderName: "codex", Profiles: []usages.Profile{{ProfileName: "local", Active: true, Status: "not_configured"}}}, {ProviderName: "claude-code", Profiles: []usages.Profile{
		{ProfileName: "local", Active: true, Status: "ok", HasCredentials: true, ExpiresAt: &exp, LastRefreshAt: &exp, Windows: []usages.Window{{Key: "session", PercentUsed: 12}, {Key: "weekly", PercentUsed: 34}, {Key: "weekly_opus", PercentUsed: 99}}},
		{ProfileName: "B", Status: "ok", HasCredentials: true, Windows: []usages.Window{{Key: "weekly", PercentUsed: 5}}},
		{ProfileName: "C", Status: "ok", HasCredentials: true, Windows: []usages.Window{{Key: "monthly_spend", PercentUsed: 40}}},
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
	obs, err := quotaSource{f.srv}.Observe(context.Background(), switcher.ToolClaude)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Ready || !obs.SwitcherEnabled || !obs.Migrated || obs.Active != "A" {
		t.Fatalf("observation = %+v", obs)
	}
	byName := map[string]quota.Obs{}
	for _, p := range obs.Profiles {
		byName[p.Name] = p
	}
	a := byName["A"]
	if a.Session == nil || *a.Session != 12 || a.Weekly == nil || *a.Weekly != 34 || !a.Stale || !a.HasCredentials {
		t.Fatalf("active profile = %+v", a)
	}
	if a.ObservedAt == nil || !a.ObservedAt.Equal(exp) || obs.MaxAge < 10*time.Minute {
		t.Fatalf("the reading's own time and a maximum age of at least ten minutes must reach the runner: %+v max=%s", a.ObservedAt, obs.MaxAge)
	}
	if b := byName["B"]; b.Session != nil || b.Weekly == nil || *b.Weekly != 5 {
		t.Fatalf("a profile with no session window must have a nil session: %+v", b)
	}
	// The spend-only monthly_spend window is display-only: the guard and the switcher read
	// only the session and weekly windows and must keep ignoring it.
	if c := byName["C"]; c.Session != nil || c.Weekly != nil {
		t.Fatalf("a spend-only profile reached the runner as a quota window: %+v", c)
	}
	if names, err := (quotaSource{f.srv}).Profiles(context.Background(), switcher.ToolClaude); err != nil || len(names) != 2 {
		t.Fatalf("profiles = %v %v", names, err)
	}
	_ = m
}

// The web switch holds new turns of the tool before it ends the host's warm processes (and for as long as the switch runs), so none can start and park
// an idle process on the login about to go; the hold is released whatever the switch came to.
func TestTheWebSwitchHoldsNewTurnsBeforeRetiringWarmAgents(t *testing.T) {
	f, m, _ := quotaEnv(t)
	machine := connectHost(t, f, true)
	heldWhenAsked := make(chan bool, 1)
	go func() {
		request := <-machine.sendQueue
		for request.Type == protocol.KeepAlivePausedFrame {
			request = <-machine.sendQueue
		}
		heldWhenAsked <- f.srv.Store.DispatchHeldForSwitch("claude")
		machine.harnessMu.Lock()
		waiter := machine.pathPending[request.ReqID]
		machine.harnessMu.Unlock()
		waiter <- protocol.Frame{V: protocol.Version, Type: protocol.RetireWarmResult, ReqID: request.ReqID, Result: "ok"}
	}()
	resp := f.request(http.MethodPost, "/api/switcher/claude/use", map[string]string{"name": "B"}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if !<-heldWhenAsked {
		t.Fatal("new turns were not held when the supervisor was asked to end its warm agents")
	}
	if f.srv.Store.DispatchHeldForSwitch("claude") {
		t.Fatal("the hold outlived the switch")
	}
	if got := activeProfile(t, m); got != "B" {
		t.Fatalf("active = %s", got)
	}
}

// Removing the active profile deletes the live login the host's warm processes hold: it is refused (409 warm_agents, nothing deleted) unless the
// supervisor confirms they are gone, the same as a switch away from it.
func TestRemovingTheActiveProfileRetiresWarmAgentsFirst(t *testing.T) {
	f, m, _ := quotaEnv(t)
	machine := connectHost(t, f, true)
	go answerRetire(machine, "error", "a warm process of the tool is not confirmed gone")
	resp := f.request(http.MethodDelete, "/api/switcher/claude/profiles/A", nil, true)
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 409 || body.Error != "warm_agents" {
		t.Fatalf("status=%d body=%+v", resp.StatusCode, body)
	}
	if got := activeProfile(t, m); got != "A" {
		t.Fatalf("active = %s: the profile was removed under a warm process", got)
	}
	if f.srv.Store.DispatchHeldForSwitch("claude") {
		t.Fatal("a refused removal must release its hold")
	}
	go answerRetire(machine, "ok", "")
	resp = f.request(http.MethodDelete, "/api/switcher/claude/profiles/A", nil, true)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("confirmed: status=%d", resp.StatusCode)
	}
}

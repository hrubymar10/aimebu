package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/credtarget"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/quota"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/switcher"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
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
	mu     sync.Mutex
	obs    quota.Observation
	names  []string // the profiles that exist; A and B when empty
	choice func() (switcher.ManualChoice, error)
}

func (s *quotaFakeSource) set(o quota.Observation) { s.mu.Lock(); s.obs = o; s.mu.Unlock() }
func (s *quotaFakeSource) Observe(context.Context, string) (quota.Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.obs
	o.Profiles = append([]quota.Obs(nil), o.Profiles...)
	if s.choice != nil {
		choice, err := s.choice()
		if err != nil {
			return quota.Observation{}, err
		}
		o.ManualChoiceProfile, o.ManualChoiceAt = choice.Profile, choice.At
	}
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
	cfg.Enabled, cfg.WarmupSeconds, cfg.MinDwellMinutes = true, 0, 0
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
	if len(cl.Profiles) != 2 || cl.Profiles[0].SessionMax != 95 || cl.WarmupSeconds != 60 || cl.MinDwellMinutes != 5 {
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
	good.Enabled = true
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
	var logged strings.Builder
	priorLog := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(priorLog)
	runner.Evaluate(context.Background())
	log.SetOutput(priorLog)
	if got := logged.String(); !strings.Contains(got, "automatic switch") || !strings.Contains(got, quota.ReasonOverLimit) {
		t.Fatalf("automatic switch log = %q", got)
	}

	if got := activeProfile(t, m); got != "B" {
		t.Fatalf("active = %s, want B", got)
	}
	if data := fileString(t, live); !strings.Contains(data, "acc-B0") || refreshTokenInFile.MatchString(data) {
		t.Fatalf("the live file is not B's token-only copy: %s", data)
	}
	_, o, _ := getQuota(t, f)
	last := o.Tools["claude"].LastDecision
	if last == nil || last.Action != quota.ActionSwitch || *last.From != "A" || *last.To != "B" || last.Reason != quota.ReasonOverLimit {
		t.Fatalf("last decision = %+v", last)
	}
	if o.Tools["claude"].State.LastSwitchAt == nil {
		t.Fatal("the switch time was not persisted")
	}
	if choice, err := m.ManualChoice(switcher.ToolClaude); err != nil || choice.Profile != "A" {
		t.Fatalf("automatic switch changed the manual choice: %+v, %v", choice, err)
	}
	var announced bool
	for _, event := range systemEvents(t, f) {
		announced = announced || (event.Kind == "quota_switch" && strings.Contains(event.PayloadJSON, `"to":"B"`) && strings.Contains(event.PayloadJSON, `"from":"A"`) && strings.Contains(event.PayloadJSON, quota.ReasonOverLimit))
	}
	if !announced {
		t.Fatalf("the owner's feed lacks the switch event: %+v", systemEvents(t, f))
	}
}

func TestImportedProfileHoldsItsManualChoiceOnTheNextQuotaPass(t *testing.T) {
	f, m, live := quotaEnv(t)
	src := &quotaFakeSource{names: []string{"A", "B", "C"}, choice: func() (switcher.ManualChoice, error) {
		return m.ManualChoice(switcher.ToolClaude)
	}}
	runner, now := runnerOver(t, f, src)
	quotaConfig(t, runner, func(c *quota.Config) { c.MinDwellMinutes = 5 })
	full := startupClaudeCreds("tok_cccc", "ref_cccc", time.Now().Add(6*time.Hour))
	if err := os.WriteFile(live, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(switcher.ToolClaude, "C"); err != nil {
		t.Fatal(err)
	}
	if observed, err := (quotaSource{f.srv}).Observe(context.Background(), switcher.ToolClaude); err != nil || observed.ManualChoiceProfile != "C" {
		t.Fatalf("quota source did not read the imported choice: %+v, %v", observed, err)
	}
	src.set(quota.Observation{Active: "C", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 10, 10), qobs("B", 10, 10), qobs("C", 10, 10)}})
	runner.Evaluate(context.Background())
	if got := activeProfile(t, m); got != "C" {
		t.Fatalf("imported profile switched away on the next pass: %s", got)
	}
	for _, event := range systemEvents(t, f) {
		if event.Kind == "quota_switch" {
			t.Fatalf("manual import caused automatic switch event: %+v", event)
		}
	}
	*now = now.Add(6 * time.Minute)
	runner.Evaluate(context.Background())
	if got := activeProfile(t, m); got != "A" {
		t.Fatalf("preferred profile did not resume after dwell: %s", got)
	}
}

// A /login the credential reconciler files into an existing, non-preferred profile with SetActive is a person's choice: it starts the
// same dwell as an import or a web switch, so the next quota pass does not take the profile back at once.
func TestReconcilerLoginIntoAnExistingProfileHoldsItsDwell(t *testing.T) {
	f, m, _ := quotaEnv(t)
	src := &quotaFakeSource{names: []string{"A", "B"}, choice: func() (switcher.ManualChoice, error) {
		return m.ManualChoice(switcher.ToolClaude)
	}}
	runner, now := runnerOver(t, f, src)
	quotaConfig(t, runner, func(c *quota.Config) { c.MinDwellMinutes = 5 })
	login := credtarget.LoginImport{Tool: switcher.ToolClaude, Profile: "B", SetActive: true,
		Full: []byte(startupClaudeCreds("tok_bbbb", "ref_bbbb", time.Now().Add(6*time.Hour)))}
	if err := m.ImportLogin(login); err != nil {
		t.Fatal(err)
	}
	src.set(quota.Observation{Active: "B", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 10, 10), qobs("B", 10, 10)}})
	runner.Evaluate(context.Background())
	if got := activeProfile(t, m); got != "B" {
		t.Fatalf("a login filed with SetActive was switched away on the next pass: %s", got)
	}
	*now = now.Add(6 * time.Minute)
	runner.Evaluate(context.Background())
	if got := activeProfile(t, m); got != "A" {
		t.Fatalf("preferred profile did not resume after dwell: %s", got)
	}
}

func TestInvalidManualChoiceDoesNotBlockQuotaSwitch(t *testing.T) {
	cases := map[string]string{
		"garbage JSON":         "not-json",
		"invalid profile name": `{"profile":"bad/name","at":"2026-11-26T10:00:00Z"}`,
		"zero time":            `{"profile":"A","at":"0001-01-01T00:00:00Z"}`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			f, m, _ := quotaEnv(t)
			marker := filepath.Join(os.Getenv("AIMEBU_CONFIG_DIR"), "switcher", "manual-choice-claude.json")
			if err := os.WriteFile(marker, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			src := &quotaFakeSource{choice: func() (switcher.ManualChoice, error) {
				observed, err := (quotaSource{f.srv}).Observe(context.Background(), switcher.ToolClaude)
				return switcher.ManualChoice{Profile: observed.ManualChoiceProfile, At: observed.ManualChoiceAt}, err
			}}
			runner, _ := runnerOver(t, f, src)
			quotaConfig(t, runner, func(c *quota.Config) { c.MinDwellMinutes = 5 })
			src.set(quota.Observation{Active: "A", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 99, 99), qobs("B", 10, 10)}})
			var logged strings.Builder
			priorLog := log.Writer()
			log.SetOutput(&logged)
			defer log.SetOutput(priorLog)
			runner.Evaluate(context.Background())
			if got := activeProfile(t, m); got != "B" {
				t.Fatalf("invalid marker blocked quota switch: active = %s", got)
			}
			src.set(quota.Observation{Active: "B", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 99, 99), qobs("B", 10, 10)}})
			for range 3 {
				runner.Evaluate(context.Background())
			}
			if count := strings.Count(logged.String(), "ignoring manual choice marker"); count != 1 {
				t.Fatalf("manual choice marker error logged %d times, want one: %s", count, logged.String())
			}
		})
	}
}

func TestManualWebSwitchUsesDwellAndZeroDisablesIt(t *testing.T) {
	for _, dwell := range []int{5, 0} {
		t.Run(fmt.Sprint(dwell), func(t *testing.T) {
			f, m, _ := quotaEnv(t)
			if err := m.Add(switcher.ToolClaude, "C"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(m.StoredCredPath(switcher.ToolClaude, "C"), []byte(startupClaudeCreds("tok_cccc", "ref_cccc", time.Now().Add(6*time.Hour))), 0o600); err != nil {
				t.Fatal(err)
			}
			src := &quotaFakeSource{names: []string{"A", "B", "C"}, choice: func() (switcher.ManualChoice, error) {
				return m.ManualChoice(switcher.ToolClaude)
			}}
			runner, now := runnerOver(t, f, src)
			quotaConfig(t, runner, func(c *quota.Config) { c.MinDwellMinutes = dwell })
			resp := f.request("POST", "/api/switcher/claude/use", map[string]string{"name": "C"}, true)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("manual web switch status = %d", resp.StatusCode)
			}
			src.set(quota.Observation{Active: "C", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 10, 10), qobs("B", 10, 10), qobs("C", 10, 10)}})
			runner.Evaluate(context.Background())
			want := "C"
			if dwell == 0 {
				want = "A"
			}
			if got := activeProfile(t, m); got != want {
				t.Fatalf("active before dwell = %s, want %s", got, want)
			}
			if dwell > 0 {
				*now = now.Add(6 * time.Minute)
				runner.Evaluate(context.Background())
				if got := activeProfile(t, m); got != "A" {
					t.Fatalf("preferred profile after dwell = %s", got)
				}
			}
		})
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

// A switch never waits for agents: the automatic switch moves off a profile at its limit while an agent is in a turn, and the turn keeps the
// token it holds.
func TestTheAutomaticSwitchGoesAheadWithARunningTurn(t *testing.T) {
	f, m, _ := quotaEnv(t)
	agent, turn, _, _ := f.execution(t)
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE agent SET tool='claude',observed='in_turn' WHERE id=?`, agent.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE turn SET exec='running' WHERE id=?`, turn.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	src := &quotaFakeSource{}
	runner, _ := runnerOver(t, f, src)
	quotaConfig(t, runner, nil)
	src.set(quota.Observation{Active: "A", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 99, 99), qobs("B", 10, 10)}})
	runner.Evaluate(context.Background())
	if got := activeProfile(t, m); got != "B" {
		t.Fatalf("a running turn held the switch: active = %s", got)
	}
	f.srv.WaitUsageRefreshes()
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

// A switch never waits for agents kept warm: the web switch answers as soon as the swap is made, and the warm processes are ended in the background,
// even when the supervisor cannot confirm it.
func TestTheWebSwitchSucceedsWithAnUnconfirmedWarmAgentAndRetiresInTheBackground(t *testing.T) {
	f, m, _ := quotaEnv(t)
	machine := connectHost(t, f, true)
	asked := make(chan struct{})
	go func() {
		answerRetire(machine, "error", "a warm process of the tool is not confirmed gone")
		close(asked)
	}()
	resp := f.request(http.MethodPost, "/api/switcher/claude/use", map[string]string{"name": "B"}, true)
	var body struct {
		Active   string `json:"active"`
		Switched bool   `json:"switched"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !body.Switched || body.Active != "B" {
		t.Fatalf("status=%d body=%+v", resp.StatusCode, body)
	}
	if got := activeProfile(t, m); got != "B" {
		t.Fatalf("active = %s", got)
	}
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor was not asked to end its warm agents after the switch")
	}
	f.srv.WaitUsageRefreshes() // waits for the background retire too
	if f.srv.kaSwitchingTool("claude") {
		t.Fatal("the cache-ping hold outlived the background retire")
	}
}

// Removing the active profile does not wait for agents either: it is removed at once and the warm processes are ended in the background.
func TestRemovingTheActiveProfileDoesNotWaitForWarmAgents(t *testing.T) {
	f, m, _ := quotaEnv(t)
	machine := connectHost(t, f, true)
	asked := make(chan struct{})
	go func() {
		answerRetire(machine, "error", "a warm process of the tool is not confirmed gone")
		close(asked)
	}()
	resp := f.request(http.MethodDelete, "/api/switcher/claude/profiles/A", nil, true)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := activeProfile(t, m); got == "A" {
		t.Fatal("the profile was not removed")
	}
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor was not asked to end its warm agents after the removal")
	}
	f.srv.WaitUsageRefreshes()
}

// limitHostAgent gives the fixture's room an agent of tool on the host machine whose last turn met the provider's usage limit (a 429
// usage_limit_reached), in force until an hour from now. It returns the agent's id.
func limitHostAgent(t *testing.T, f *fixture, key, tool string) string {
	t.Helper()
	host, err := f.srv.Store.HostMachine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.srv.machineMu.Lock()
	if f.srv.machines[f.machineID] == nil {
		f.srv.machines[f.machineID] = &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
			capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	}
	f.srv.machineMu.Unlock()
	status, card, raw := f.spawnRequest(t, key, "worker")
	if status != 201 || card.ID == "" {
		t.Fatalf("spawn=%d %+v", status, raw)
	}
	stamp := func(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000000000Z") }
	turnID := "turn_" + key
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`UPDATE agent SET tool=?, machine_id=? WHERE id=?`, []any{tool, host.ID, card.ID}},
			{`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,outcome,settle)
				SELECT ?,?,s.id,0,?,1,'normal','exited','failed','settled' FROM session s WHERE s.agent_id=? LIMIT 1`, []any{turnID, card.ID, f.room.ID, card.ID}},
			{`INSERT INTO provider_limit(agent_id,turn_id,detected_at,reset_at,tool) VALUES (?,?,?,?,?)`, []any{card.ID, turnID, stamp(time.Now()), stamp(time.Now().Add(time.Hour)), tool}},
		} {
			if _, err := tx.Exec(statement.query, statement.args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return card.ID
}

// A harness that met the provider's usage limit marks the host's active profile exhausted although the usage snapshot still reads under the
// threshold; the automatic switch moves off it, and lifts the limits the host's agents of the tool met on the old login, so they go on with the new
// one instead of waiting for the old one's reset.
func TestAProviderLimitOnTheHostSwitchesAutomaticallyAndTheSwitchLiftsIt(t *testing.T) {
	f, m, _ := quotaEnv(t)
	f.seed()
	ctx := context.Background()
	if f.srv.hostToolLimited(ctx, "claude") {
		t.Fatal("no limit yet")
	}
	agentID := limitHostAgent(t, f, "limit-claude", "claude")
	if !f.srv.hostToolLimited(ctx, "claude") || f.srv.hostToolLimited(ctx, "codex") {
		t.Fatal("the host's claude login is limited, codex is not")
	}

	// The real source marks only the active profile limited.
	exp := time.Now().Add(time.Hour).UTC()
	snapshot := usages.Response{Providers: []usages.Provider{{ProviderName: "codex", Profiles: []usages.Profile{{ProfileName: "local", Active: true, Status: "not_configured"}}}, {ProviderName: "claude-code", Profiles: []usages.Profile{
		{ProfileName: "local", Active: true, Status: "ok", HasCredentials: true, ExpiresAt: &exp, LastRefreshAt: &exp, Windows: []usages.Window{{Key: "session", PercentUsed: 40}, {Key: "weekly", PercentUsed: 10}}},
		{ProfileName: "B", Status: "ok", HasCredentials: true, Windows: []usages.Window{{Key: "session", PercentUsed: 1}, {Key: "weekly", PercentUsed: 5}}},
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
	obs, err := quotaSource{f.srv}.Observe(ctx, switcher.ToolClaude)
	if err != nil {
		t.Fatal(err)
	}
	limited := map[string]bool{}
	for _, p := range obs.Profiles {
		limited[p.Name] = p.Limited
	}
	if obs.Active != "A" || !limited["A"] || limited["B"] {
		t.Fatalf("only the active profile is limited: active=%s %v", obs.Active, limited)
	}

	// The runner over the real actuator: A reads 40 % but is limited, B is fine.
	src := &quotaFakeSource{}
	runner, _ := runnerOver(t, f, src)
	quotaConfig(t, runner, nil)
	a := qobs("A", 40, 10)
	a.Limited = true
	src.set(quota.Observation{Active: "A", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{a, qobs("B", 1, 5)}})
	runner.Evaluate(ctx)
	if got := activeProfile(t, m); got != "B" {
		t.Fatalf("active = %s, want B", got)
	}
	_, o, _ := getQuota(t, f)
	if last := o.Tools["claude"].LastDecision; last == nil || last.Action != quota.ActionSwitch || last.Reason != quota.ReasonOverLimit {
		t.Fatalf("last decision = %+v", last)
	}
	if still, err := f.srv.Store.AgentProviderLimited(ctx, agentID); err != nil || still {
		t.Fatalf("the switch did not lift the agent's provider limit: %v %v", still, err)
	}
	if f.srv.hostToolLimited(ctx, "claude") {
		t.Fatal("the host's claude login still reads limited after the switch")
	}
	f.srv.WaitUsageRefreshes()
}

// A codex limit is not lifted by a claude switch, and a web switch lifts the claude ones like the automatic one.
func TestAWebSwitchLiftsOnlyTheSwitchedToolsProviderLimits(t *testing.T) {
	f, m, _ := quotaEnv(t)
	f.seed()
	ctx := context.Background()
	claudeAgent := limitHostAgent(t, f, "limit-web-claude", "claude")
	codexAgent := limitHostAgent(t, f, "limit-web-codex", "codex")
	resp := f.request(http.MethodPost, "/api/switcher/claude/use", map[string]string{"name": "B"}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 || activeProfile(t, m) != "B" {
		t.Fatalf("status=%d active=%s", resp.StatusCode, activeProfile(t, m))
	}
	if still, _ := f.srv.Store.AgentProviderLimited(ctx, claudeAgent); still {
		t.Fatal("the web switch did not lift the claude agent's limit")
	}
	if still, _ := f.srv.Store.AgentProviderLimited(ctx, codexAgent); !still {
		t.Fatal("a claude switch lifted a codex limit")
	}
	f.srv.WaitUsageRefreshes()
}

// Warm agents the host's supervisor cannot confirm ended no longer hold the automatic switch: it switches, and retires them in the background.
func TestUnconfirmedWarmAgentsDoNotHoldTheAutomaticSwitch(t *testing.T) {
	f, m, _ := quotaEnv(t)
	machine := connectHost(t, f, true)
	src := &quotaFakeSource{}
	runner, _ := runnerOver(t, f, src)
	quotaConfig(t, runner, nil)
	src.set(quota.Observation{Active: "A", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 99, 99), qobs("B", 10, 10)}})
	asked := make(chan struct{})
	go func() {
		answerRetire(machine, "error", "a warm process of the tool is not confirmed gone")
		close(asked)
	}()
	runner.Evaluate(context.Background())
	if got := activeProfile(t, m); got != "B" {
		t.Fatalf("active = %s: an unconfirmed warm process held the switch", got)
	}
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor was not asked to end its warm agents after the switch")
	}
	f.srv.WaitUsageRefreshes()
}

// The configuration endpoint answers an old client that still sends dry_run as if it had not: 200, a logged warning, and no dry_run in the view.
// A backup written while dry run existed imports the same way, the field ignored.
func TestTheConfigEndpointAndABackupIgnoreTheRemovedDryRunField(t *testing.T) {
	f, _, _ := quotaEnv(t)
	src := &quotaFakeSource{}
	runnerOver(t, f, src)
	var logged strings.Builder
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	old := `{"enabled":true,"dry_run":true,"profiles":[{"name":"A","session_max":95,"weekly_max":95},{"name":"B","session_max":95,"weekly_max":95}],"protected":[],"after_reset":null,"warmup_seconds":0,"min_dwell_minutes":0}`
	resp := f.request("PUT", "/api/quota-switcher/claude", json.RawMessage(old), true)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(string(raw), "dry_run") || !strings.Contains(string(raw), `"saved":true`) {
		t.Fatalf("an old client's save: %d %s", resp.StatusCode, raw)
	}
	if !strings.Contains(logged.String(), "ignoring dry_run") {
		t.Fatalf("no warning logged: %q", logged.String())
	}
	backup := `{"schema":"` + settingsBackupSchema + `","version":1,"quota_switcher":{"claude":` + strings.Replace(old, `"warmup_seconds":0`, `"warmup_seconds":30`, 1) + `}}`
	var file map[string]any
	if err := json.Unmarshal([]byte(backup), &file); err != nil {
		t.Fatal(err)
	}
	status, preview := previewBackup(t, f, file)
	if status != 200 {
		t.Fatalf("an old backup's preview: %d", status)
	}
	resp = f.request("POST", "/api/settings-backup/apply", map[string]any{"backup": file, "digest": preview.Digest}, true)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("an old backup: %d %s", resp.StatusCode, raw)
	}
	if _, o, _ := getQuota(t, f); o.Tools["claude"].Config.WarmupSeconds != 30 {
		t.Fatalf("the backup's quota switcher was not applied: %+v", o.Tools["claude"].Config)
	}
}

// A stored enabled dry-run configuration, in the real store with the real actuator, switches for real once the server starts the switcher's
// upgrade, and the owner's feed carries the one-time event once.
func TestAStoredDryRunConfigSwitchesForRealAndTheOwnerIsToldOnce(t *testing.T) {
	f, m, _ := quotaEnv(t)
	src := &quotaFakeSource{}
	runner, _ := runnerOver(t, f, src)
	old := `{"enabled":true,"dry_run":true,"profiles":[{"name":"B","session_max":95,"weekly_max":95},{"name":"A","session_max":95,"weekly_max":95}],"protected":[],"after_reset":null,"warmup_seconds":0,"min_dwell_minutes":0}`
	if err := f.srv.Store.SaveQuotaConfig(context.Background(), "claude", old); err != nil {
		t.Fatal(err)
	}
	runner.MigrateDryRun(context.Background())
	runner.MigrateDryRun(context.Background())
	src.set(quota.Observation{Active: "A", Migrated: true, SwitcherEnabled: true, Ready: true, Profiles: []quota.Obs{qobs("A", 99, 99), qobs("B", 10, 10)}})
	runner.Evaluate(context.Background())
	if got := activeProfile(t, m); got != "B" {
		t.Fatalf("the old dry-run configuration did not switch for real: active %s", got)
	}
	told := 0
	for _, event := range systemEvents(t, f) {
		if event.Kind == quota.EventDryRunRemoved && strings.Contains(event.PayloadJSON, `"tool":"claude"`) {
			told++
		}
	}
	if told != 1 {
		t.Fatalf("the owner was told %d times, want once", told)
	}
	f.srv.WaitUsageRefreshes()
}

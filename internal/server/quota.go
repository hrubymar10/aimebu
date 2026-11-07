package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/hrubymar10/aimebu/internal/quota"
	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// StartQuotaSwitcher starts the automatic account switcher: the quota Runner over
// this server's usage snapshots, switcher, token refresher and store. It does
// nothing until the owner enables a tool (dry run first), and everything it does is
// an ordinary Switcher.Switch, so lock order, the incoming refresh and the
// quiescence rule are the ones the web switch has. It needs the usage reader and
// the switcher; without them it returns nil. Calling it twice is a no-op.
func (s *Server) StartQuotaSwitcher(ctx context.Context) *quota.Runner {
	if s.Usages == nil || s.Switcher == nil || s.Store == nil {
		return nil
	}
	runner := quota.NewRunner(s.Store, quotaSource{s}, quotaActuator{s: s}, quotaEvents{s})
	if !s.quota.CompareAndSwap(nil, runner) {
		return s.quota.Load()
	}
	guard := quota.NewGuard(s.Store, quotaSource{s}, s.Store, quotaEvents{s})
	guard.CanSwitch = runner.CanSwitch
	guard.OnHold = runner.Trigger
	runner.SetGuard(guard.ConfigOrDefault)
	s.guard.Store(guard)
	go runner.Run(ctx)
	go guard.Run(ctx)
	return runner
}

func (s *Server) limitGuard() *quota.Guard { return s.guard.Load() }

func (s *Server) quotaRunner() *quota.Runner { return s.quota.Load() }

// quotaSource builds the runner's observations from what the web already shows: the
// usage snapshot merged with the switcher's profiles (Switcher.OverlayUsages), plus
// the token refresher's verdict on each account.
type quotaSource struct{ s *Server }

func (q quotaSource) Profiles(_ context.Context, tool string) ([]string, error) {
	profiles, err := q.s.Switcher.List()
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, p := range profiles {
		if p.Tool == tool {
			names = append(names, p.Name)
		}
	}
	return names, nil
}

func (q quotaSource) Observe(_ context.Context, tool string) (quota.Observation, error) {
	response := q.s.Usages.Snapshot()
	if err := q.s.Switcher.OverlayUsages(&response); err != nil {
		return quota.Observation{}, err
	}
	// A reading older than three refresh intervals (at least ten minutes) is stale: a
	// stalled poller must not leave the runner acting on old percentages.
	maxAge := 3 * time.Duration(response.Settings.RefreshIntervalSec) * time.Second
	if maxAge < 10*time.Minute {
		maxAge = 10 * time.Minute
	}
	obs := quota.Observation{SwitcherEnabled: response.SwitcherEnabled, Migrated: q.s.Switcher.TokenCopiesFor(tool), MaxAge: maxAge}
	var parked map[string]string
	if tr := q.s.currentRefresher(); tr != nil && tr.Enabled() {
		parked = tr.AccountStates()
	}
	for _, provider := range response.Providers {
		if provider.ProviderName != usageProvider(tool) {
			continue
		}
		for _, profile := range provider.Profiles {
			solo := profile.ProfileName == "local" && len(provider.Profiles) == 1 // the implicit single entry of a tool with no stored profile
			o := quota.Obs{Name: profile.ProfileName, Status: profile.Status, Stale: profile.Stale, HasCredentials: profile.HasCredentials, TokenExpiresAt: profile.ExpiresAt, ObservedAt: profile.LastRefreshAt}
			for _, w := range profile.Windows {
				p := w.PercentUsed
				switch w.Key {
				case "session":
					o.Session = &p
				case "weekly":
					o.Weekly = &p
				}
			}
			switch parked[tool+"/"+profile.ProfileName] {
			case usages.AccountReauthRequired, usages.AccountNeedsReloginSoon:
				o.Parked = true
			}
			if solo {
				// Not a profile the switcher can move between, so it stays out of Profiles; the limit guard reads it here.
				obs.Solo = &o
				continue
			}
			if profile.Active {
				obs.Active = profile.ProfileName
			}
			obs.Profiles = append(obs.Profiles, o)
		}
		obs.Ready = len(obs.Profiles) > 0
	}
	return obs, nil
}

func (s *Server) currentRefresher() *usages.TargetRefresher {
	s.refresherMu.Lock()
	defer s.refresherMu.Unlock()
	return s.refresher
}

// quotaActuator switches through Switcher.Switch after the shared running-turn check.
type quotaActuator struct {
	s *Server
	// afterPreCheck is a test seam: it runs between the unlocked pre-check and the switch.
	afterPreCheck func()
}

func (a quotaActuator) Switch(ctx context.Context, tool, from, to string) (bool, string, error) {
	running, err := a.s.runningAgents(ctx, tool)
	if err != nil {
		return false, "", err
	}
	if len(running) > 0 {
		return false, quota.ReasonAgentsRunning, nil
	}
	if a.afterPreCheck != nil {
		a.afterPreCheck()
	}
	// The pre-check above is only a cheap early refusal: the switch can wait up to 45s for the account
	// lock and for the incoming refresh, and a turn may start meanwhile and have its credentials swapped
	// under it. So quiescence is checked again, under the switcher lock, right before the swap.
	_, changed, err := a.s.Switcher.SwitchGuarded(tool, to, switcher.SwitchGuard{ExpectedActive: from, Quiet: a.s.quietGuard(ctx, tool), Hold: a.s.Store.HoldTurns})
	var turnRunning agentsRunningError
	switch {
	case err == nil:
		if changed {
			a.s.refreshSwitchedUsage(tool)
		}
		return changed, "", nil
	case errors.Is(err, switcher.ErrMaintenanceBusy), errors.Is(err, switcher.ErrMigrationRaced):
		return false, quota.ReasonAccountBusy, nil
	case errors.Is(err, switcher.ErrIncomingRefresh):
		return false, quota.ReasonRefreshFailed, nil
	case errors.Is(err, switcher.ErrLoginPending):
		return false, quota.ReasonLoginPending, nil
	case errors.Is(err, switcher.ErrDisabled):
		return false, quota.ReasonSwitcherOff, nil
	case errors.Is(err, switcher.ErrActiveChanged):
		return false, quota.ReasonActiveChanged, nil
	case errors.As(err, &turnRunning):
		return false, quota.ReasonAgentsRunning, nil
	}
	return false, "", err
}

// quotaEvents writes the quota events as project-less (owner-only) system events.
type quotaEvents struct{ s *Server }

func (e quotaEvents) Event(kind string, payload map[string]string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.s.Store.RecordQuotaEvent(ctx, kind, payload); err != nil {
		log.Printf("quota: record %s event: %v", kind, err)
	}
}

// quotaReady answers the owner-only quota routes: 503 until the runner exists.
func (s *Server) quotaReady(w http.ResponseWriter, r *http.Request) (*quota.Runner, bool) {
	if !s.ownerMachine(w, r) {
		return nil, false
	}
	runner := s.quotaRunner()
	if runner == nil {
		apiError(w, 503, "unavailable", "the quota switcher is not running (it needs the usage reader and the account switcher)")
		return nil, false
	}
	return runner, true
}

func quotaError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, quota.ErrInvalid):
		apiError(w, 400, "invalid_request", err.Error())
	case errors.Is(err, quota.ErrUnknownTool):
		apiError(w, 404, "not_found", "unknown tool")
	default:
		fail(w, err)
	}
}

// GET /api/quota-switcher
func (s *Server) quotaOverview(w http.ResponseWriter, r *http.Request) {
	runner, ok := s.quotaReady(w, r)
	if !ok {
		return
	}
	overview, err := runner.Overview(r.Context())
	if err != nil {
		quotaError(w, err)
		return
	}
	writeJSON(w, 200, overview)
}

// PUT /api/quota-switcher/{tool}
func (s *Server) quotaSave(w http.ResponseWriter, r *http.Request) {
	runner, ok := s.quotaReady(w, r)
	if !ok {
		return
	}
	var cfg quota.Config
	if !readJSON(w, r, &cfg) {
		return
	}
	view, err := runner.SaveConfig(r.Context(), r.PathValue("tool"), cfg)
	if err != nil {
		quotaError(w, err)
		return
	}
	writeJSON(w, 200, view)
}

// POST /api/quota-switcher/{tool}/rearm and /reenable
func (s *Server) quotaClear(rearm bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runner, ok := s.quotaReady(w, r)
		if !ok {
			return
		}
		var body struct{}
		if !readJSON(w, r, &body) {
			return
		}
		var view quota.ToolView
		var err error
		if rearm {
			view, err = runner.Rearm(r.Context(), r.PathValue("tool"))
		} else {
			view, err = runner.Reenable(r.Context(), r.PathValue("tool"))
		}
		if err != nil {
			quotaError(w, err)
			return
		}
		writeJSON(w, 200, view)
	}
}

// guardReady answers the owner-only limit guard routes: 503 until it is running.
func (s *Server) guardReady(w http.ResponseWriter, r *http.Request) (*quota.Guard, bool) {
	if !s.ownerMachine(w, r) {
		return nil, false
	}
	guard := s.limitGuard()
	if guard == nil {
		apiError(w, 503, "unavailable", "the limit guard is not running (it needs the usage reader and the account switcher)")
		return nil, false
	}
	return guard, true
}

// GET /api/limit-guard
func (s *Server) guardOverview(w http.ResponseWriter, r *http.Request) {
	guard, ok := s.guardReady(w, r)
	if !ok {
		return
	}
	tools, err := guard.Overview(r.Context())
	if err != nil {
		quotaError(w, err)
		return
	}
	profiles := map[string][]string{}
	for _, tool := range quota.Tools() {
		names, err := quotaSource{s}.Profiles(r.Context(), tool)
		if err != nil {
			fail(w, err)
			return
		}
		profiles[tool] = names
	}
	// The owner's percent-left or percent-used choice (Settings -> Usages), so every place that says where a tool stands says it the same way.
	display := "left"
	if s.Usages != nil {
		if shown := s.Usages.Snapshot().Settings.PercentDisplay; shown == "used" {
			display = shown
		}
	}
	writeJSON(w, 200, map[string]any{"profiles": profiles, "tools": tools, "percent_display": display})
}

// PUT /api/limit-guard/{tool}
func (s *Server) guardSave(w http.ResponseWriter, r *http.Request) {
	guard, ok := s.guardReady(w, r)
	if !ok {
		return
	}
	var cfg quota.GuardConfig
	if !readJSON(w, r, &cfg) {
		return
	}
	view, err := guard.Save(r.Context(), r.PathValue("tool"), cfg)
	if err != nil {
		quotaError(w, err)
		return
	}
	writeJSON(w, 200, view)
}

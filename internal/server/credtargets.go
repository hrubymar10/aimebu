package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/credtarget"
	"github.com/hrubymar10/aimebu/v2/internal/switcher"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
)

// credentialAlertDedupe is how long an identical credential alert stays quiet:
// a failing reconcile import or fan-out repeats every pass.
const credentialAlertDedupe = time.Hour

// StartTokenCopies wires the token-copy model into the server and starts its
// goroutines: the live-file fan-out (shared with the Switch hooks), the
// reconciler and the refresher. Every piece is gated on the switcher's
// TokenCopies marker, which is re-read each pass, so an install that has not run
// `aimebu switcher migrate-copies` keeps the link-based behaviour untouched and
// migrating with the server running activates the model without a restart.
//
// The wiring is synchronous (it installs the Switch hook before any request can
// switch) and only the loops run in the background. It returns the refresher, or
// nil when the model is not available. Calling it twice is a no-op.
//
// The refresher has exactly one owner, this function. Its Run is also
// idempotent, so a second start (a test, a future caller) cannot make two journal
// owners. Startup order matters: the refresher runs the reconciler pass
// (SetReconcile) BEFORE its first due pass, so a /login that landed while the
// server was down is imported and stripped before anything fans a refreshed
// credential out over it.
func (s *Server) StartTokenCopies(ctx context.Context, configure ...func(*usages.TargetRefresher)) *usages.TargetRefresher {
	mgr := s.Switcher
	if mgr == nil || mgr.TokenCopyFanOut() == nil {
		return nil
	}
	s.refresherMu.Lock()
	defer s.refresherMu.Unlock()
	if s.refresher != nil {
		return s.refresher
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("credtarget: no home directory, token-copy model not started: %v", err)
		return nil
	}
	fan := mgr.TokenCopyFanOut()
	// A claim file next to each live credential file keeps two refreshers from spending one login, wherever their
	// config directories are. It is re-checked on a timer, so a refresher that appears later is reported and a dead
	// holder's tools are taken over without a restart.
	claims := switcher.NewLiveClaims(mgr)
	s.liveClaims = claims
	// A refresher that predates the claim lock holds no claim, but its switcher root's token-copy marker still says it
	// refreshes the same live file. That check stays as a second, additional block next to the claim: a tool is not ours
	// to refresh while either says another refresher owns it. It is cached and re-read with the claims on the timer.
	var legacyMu sync.Mutex
	legacy := map[string]string{}
	legacyCheck := func() {
		next := map[string]string{}
		if conflicts, err := mgr.TokenCopyConflicts(); err != nil {
			for _, tool := range switcher.Tools() {
				if mgr.TokenCopiesFor(tool) {
					next[tool] = "could not check other token-copy roots: " + err.Error()
				}
			}
		} else {
			for tool, root := range conflicts {
				next[tool] = "another token refresher may own the same live credentials: " + root
			}
		}
		legacyMu.Lock()
		legacy = next
		legacyMu.Unlock()
	}
	legacyCheck()
	blocked := func(tool string) string {
		if reason := claims.Blocked(tool); reason != "" {
			return reason
		}
		legacyMu.Lock()
		defer legacyMu.Unlock()
		return legacy[tool]
	}
	s.refresherBlocked = blocked
	claims.Refresh()
	announced := map[string]string{}
	announce := func() {
		for _, tool := range switcher.Tools() {
			reason := blocked(tool)
			if reason != announced[tool] && reason != "" {
				s.credentialAlertSink(true)("refresher_conflict", tool+": "+reason+"; this tool will not refresh")
			}
			announced[tool] = reason
		}
	}
	announce()
	ownsTool := func(tool string) bool { return mgr.TokenCopiesFor(tool) && blocked(tool) == "" }

	// The refresher dedupes edges itself (and re-arms them on recovery), so its
	// sink must not swallow a repeat of a failure that recovered in between; the
	// reconciler's failures repeat every pass and keep the hour-long dedupe.
	tr := usages.NewTargetRefresher(usages.TargetRefresherConfig{
		Root:      mgr.Root(),
		Profiles:  mgr.UsageProfiles,
		WithLock:  mgr.WithLock,
		FanOut:    fan,
		LivePath:  mgr.LiveCredPath,
		ModelFlag: s.usageModelFlag,
		WarmupSettings: func() (string, string, bool, error) {
			if s.Usages == nil {
				return "off", "", false, nil
			}
			cfg, err := s.Usages.ExtraStore().LoadConfig()
			return cfg.WarmupMode, cfg.WarmupSchedule, cfg.TokenAutoRefresh, err
		},
		UsageProfile: func(provider, name string) (usages.Profile, bool) {
			if s.Usages == nil {
				return usages.Profile{}, false
			}
			return s.Usages.StoredProfile(provider, name)
		},
		WarmupPinned: func(tool, name string) bool {
			if err := fan.Registry.Load(); err != nil {
				return true
			}
			for _, target := range fan.Registry.Targets() {
				if target.Tool == tool && target.Profile == name {
					return true
				}
			}
			return false
		},
	})
	tr.SetEnabled(mgr.TokenCopies)
	tr.SetToolEnabled(ownsTool)
	tr.SetPendingLogin(func(tool string) bool {
		path, err := mgr.LiveCredPath(tool)
		if err != nil {
			return true
		}
		data, err := os.ReadFile(path)
		return (err != nil && !os.IsNotExist(err)) || (err == nil && credtarget.HasRefreshToken(tool, data))
	})
	tr.SetAlertHook(s.credentialAlertSink(false))
	mgr.SetRefreshIncoming(func(ctx context.Context, tool, profile string) error {
		if reason := blocked(tool); reason != "" {
			return fmt.Errorf("credential refresher blocked for %s: %s", tool, reason)
		}
		return tr.RefreshIncoming(ctx, tool, profile)
	})

	rec := &credtarget.Reconciler{
		Registry:  fan.Registry,
		Writer:    fan.Writer,
		FanOut:    fan,
		Store:     switcher.NewCredStore(mgr),
		Home:      home,
		Alert:     s.credentialAlertSink(true),
		Ready:     mgr.TokenCopies,
		ReadyTool: ownsTool,
	}
	// The refresher runs the reconciler pass itself before its first due pass;
	// the reconciler's own loop then keeps ticking every 60 s.
	tr.SetReconcile(rec.Pass)
	for _, fn := range configure { // tests swap the CLI executor before anything runs
		fn(tr)
	}
	s.refresher = tr
	// The claim outlives the context until every in-flight spend has committed: a spend deliberately ignores the
	// cancellation once its CLI has started, and a second refresher that took the claim meanwhile would spend the
	// same login again. WaitTokenCopies joins this goroutine.
	s.claimsDone = make(chan struct{})
	go func() {
		defer telemetry.Recover(ctx)
		defer close(s.claimsDone)
		ticker := time.NewTicker(liveClaimInterval)
		defer ticker.Stop()
		defer claims.Close()
		for {
			select {
			case <-ctx.Done():
				tr.Drain()
				return
			case <-ticker.C:
				legacyCheck()
				claims.Refresh()
				announce()
			}
		}
	}()
	telemetry.Go(ctx, func() { tr.Run(ctx) })
	telemetry.Go(ctx, func() { rec.Run(ctx) })
	if s.Usages != nil {
		s.Usages.SetCredentialAlerts(func() []usages.CredentialAlert {
			if !tr.Enabled() {
				return nil
			}
			return s.credentialAlerts(tr)
		})
		available := s.TokenAutoRefreshAvailable
		if available == nil {
			available = usages.HarnessDockerCtrlAvailable
		}
		ts := usages.NewTokenSetting(s.Usages.ExtraStore(), s.tokenCopyController(mgr, rec, available, blocked), mgr.TokenCopiesFor, available)
		if s.TokenSettingClock != nil {
			ts.SetClock(s.TokenSettingClock)
		}
		s.tokenSetting = ts
		s.Usages.SetTokenSetting(ts)
		telemetry.Go(ctx, func() { ts.Run(ctx) })
	}
	return tr
}

// WaitTokenCopies blocks until the token-copy goroutines have wound down after the context given to StartTokenCopies
// was cancelled: every in-flight refresh has committed and the live-file claims are released. It returns at once when
// the model was never started. The server process calls it before exiting so a refresher that is still spending is not
// cut off, and a second server cannot take the claim while it is.
func (s *Server) WaitTokenCopies() {
	s.refresherMu.Lock()
	done := s.claimsDone
	s.refresherMu.Unlock()
	if done != nil {
		<-done
	}
}

// tokenCopyController is what the "Auto-refresh tokens" setting does to each tool:
// on migrates it to the token-copy model (the existing safety refusals become a
// retried `waiting: <reason>` state), off returns it to the link model with the
// full credential back in the live file. The states it returns are what the
// settings response shows.
func (s *Server) tokenCopyController(mgr *switcher.Manager, rec *credtarget.Reconciler, available func() bool, blocked func(string) string) usages.TokenController {
	last := map[string]usages.TokenAutoRefreshToolState{}
	migrationBlocks := codexBlockLogState{}
	return func(ctx context.Context, on bool) map[string]usages.TokenAutoRefreshToolState {
		states := make(map[string]usages.TokenAutoRefreshToolState, 2)
		for _, tool := range switcher.Tools() {
			state := usages.TokenAutoRefreshToolState{Status: "off"}
			var migrationErr error
			migrated := mgr.TokenCopiesFor(tool)
			switch {
			case blocked(tool) != "":
				state = usages.TokenAutoRefreshToolState{Status: "waiting", Reason: blocked(tool)}
			case on && migrated:
				state.Status = "migrated"
			case on:
				if !available() {
					state = usages.TokenAutoRefreshToolState{Status: "waiting", Reason: "harness-docker-ctrl is unavailable"}
				} else {
					result, err := func() (switcher.MigrateResult, error) {
						if tool == switcher.ToolCodex {
							release, err := s.retireHostWarm(ctx, tool)
							if err != nil {
								return switcher.MigrateResult{}, err
							}
							defer release()
						}
						return mgr.MigrateToTokenCopies(switcher.MigrateOptions{Tools: []string{tool}, RunningCodex: s.tokenSettingRunningCodex})
					}()
					if err != nil {
						migrationErr = err
						state = usages.TokenAutoRefreshToolState{Status: "waiting", Reason: tokenCopyMigrationReason(err)}
					} else if mgr.TokenCopiesFor(tool) {
						state.Status = "migrated"
					} else if len(result.Tools) > 0 {
						if strings.Contains(result.Tools[0].Status, "no active profile") {
							state.Status = "n/a"
						} else {
							state = usages.TokenAutoRefreshToolState{Status: "waiting", Reason: result.Tools[0].Status}
						}
					}
				}
			default:
				err := mgr.UnmigrateTokenCopies(tool)
				if errors.Is(err, switcher.ErrLoginPending) {
					// A /login is waiting in the live file: import it, then retry.
					if reconcileErr := rec.Pass(ctx); reconcileErr == nil {
						err = mgr.UnmigrateTokenCopies(tool)
					} else {
						err = reconcileErr
					}
				}
				if err != nil {
					if migrated {
						state = usages.TokenAutoRefreshToolState{Status: "waiting", Reason: usages.RedactDetail(err.Error())}
					} else {
						// Never migrated: the setting is off, whatever the link repair says.
						log.Printf("credtarget: %s active-link repair: %s", tool, usages.RedactDetail(err.Error()))
						telemetry.WarnCause(context.Background(), "credentials", "active-link repair")
					}
				}
			}
			if migrationBlocks.changed(tool, migrationErr) {
				log.Printf("credtarget: %s migration blocked: %s", tool, usages.RedactDetail(migrationErr.Error()))
				telemetry.WarnCause(context.Background(), "credentials", "migration blocked")
			}
			states[tool] = state
			if state.Status != last[tool].Status {
				log.Printf("credtarget: %s token auto-refresh: %s %s", tool, state.Status, state.Reason)
				last[tool] = state
			}
		}
		return states
	}
}

func tokenCopyMigrationReason(err error) string {
	var running *switcher.CodexRunningError
	if errors.As(err, &running) {
		return fmt.Sprintf("waiting for running codex sessions to end (%d found); retries automatically", len(running.Found))
	}
	return usages.RedactDetail(err.Error())
}

// codexBlockLogState records only the blocking process set. Observing any
// other outcome clears it so a later block is logged again.
type codexBlockLogState map[string]string

func (s codexBlockLogState) changed(tool string, err error) bool {
	var running *switcher.CodexRunningError
	if !errors.As(err, &running) {
		delete(s, tool)
		return false
	}
	found := append([]string(nil), running.Found...)
	sort.Strings(found)
	key := strings.Join(found, "\x00")
	previous, seen := s[tool]
	s[tool] = key
	return !seen || previous != key
}

// liveClaimInterval is how often the refresher claims on the live credential files are re-checked.
const liveClaimInterval = 30 * time.Second

// credentialAlerts is the refresher's own alerts plus one per tool whose live credential file another refresher owns.
func (s *Server) credentialAlerts(tr *usages.TargetRefresher) []usages.CredentialAlert {
	alerts := tr.Alerts()
	if s.refresherBlocked == nil {
		return alerts
	}
	for _, tool := range switcher.Tools() {
		if reason := s.refresherBlocked(tool); reason != "" {
			alerts = append(alerts, usages.CredentialAlert{Kind: "refresher_conflict", Tool: tool, Role: "live", Message: tool + ": " + reason + "; this tool will not refresh"})
		}
	}
	return alerts
}

// RunTokenCopies is StartTokenCopies that blocks until ctx ends.
func (s *Server) RunTokenCopies(ctx context.Context) {
	if s.StartTokenCopies(ctx) == nil {
		return
	}
	<-ctx.Done()
}

// usageModelFlag is the optional model flag the maintenance CLI runs use, from
// the usage settings; empty until the usage reader exists.
func (s *Server) usageModelFlag(tool string) string {
	if s.Usages == nil {
		return ""
	}
	return s.Usages.ModelFlag(tool)
}

// credentialTargets is GET /api/credentials/targets: the token state of every
// credential the token-copy model keeps alive (stored profiles and live files),
// and the sentences a human has to act on. Never a token, and never the row's
// Detail: that is CLI output and error text, which stays in the state file, the
// log and the owner's doctor run.
func (s *Server) credentialTargets(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	body := map[string]any{"enabled": false, "targets": []usages.TargetStatus{}, "alerts": []usages.CredentialAlert{}}
	s.refresherMu.Lock()
	tr := s.refresher
	s.refresherMu.Unlock()
	if tr != nil && tr.Enabled() {
		body["enabled"] = true
		targets := tr.Statuses()
		for i := range targets {
			targets[i].Detail = ""
		}
		body["targets"] = targets
		body["alerts"] = s.credentialAlerts(tr)
	}
	writeJSON(w, 200, body)
}

// credentialAlertSink returns the alert callback of the credential pieces: it
// logs and records a credential_alert system event for the owner. dedupe drops
// an identical (kind, detail) pair seen within credentialAlertDedupe. detail
// can carry CLI output and error text (paths, possibly a token a failing CLI
// echoed), so the log and the persisted event only ever hold it redacted.
func (s *Server) credentialAlertSink(dedupe bool) func(kind, detail string) {
	var mu sync.Mutex
	seen := map[string]time.Time{}
	return func(kind, detail string) {
		if dedupe {
			key := kind + "\x00" + detail
			mu.Lock()
			if at, ok := seen[key]; ok && time.Since(at) < credentialAlertDedupe {
				mu.Unlock()
				return
			}
			seen[key] = time.Now()
			mu.Unlock()
		}
		detail = usages.RedactDetail(detail)
		log.Printf("credential alert: %s: %s", kind, detail)
		// What needs a person is a warning (a login to redo), what failed is an error; the rest (expiring soon, a login imported, another refresher present) is
		// information. The detail names accounts and paths: only the kind goes out.
		switch kind {
		case "refresh_failed", "fanout_failed", "state_save_failed", "login_strip_failed", "login_import_failed", "commit_failed":
			telemetry.ReportCause(context.Background(), "credentials", "credential alert: "+kind)
		case "reauth_required", "expired":
			telemetry.WarnCause(context.Background(), "credentials", "credential alert: "+kind)
		}
		if err := s.Store.RecordCredentialAlert(context.Background(), kind, detail); err != nil {
			log.Printf("credential alert: record event: %v", err)
		}
	}
}

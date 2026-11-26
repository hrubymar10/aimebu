package server

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
)

// StartUsageAlerts watches fresh provider snapshots without blocking the
// Reader's update hook, which runs under the Reader lock.
func (s *Server) StartUsageAlerts(ctx context.Context, root string) error {
	if s.Usages == nil {
		return nil
	}
	tracker, err := usages.NewAlertTracker(root)
	if err != nil {
		return err
	}
	s.usageAlerts = tracker
	// The snapshot's chart markers read the tracker's episode state: when a
	// profile's history has the gap a rejected login left, the chart says so.
	s.Usages.SetRejectionTimes(tracker.RejectionTimes)
	s.usageAlertWake = make(chan struct{}, 1)
	s.Usages.SetUpdateHook(s.usagesChanged)
	go func() {
		defer telemetry.Recover(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.usageAlertWake:
				snapshot := s.Usages.Snapshot()
				if s.Switcher != nil {
					if err := s.Switcher.EnsureUsageAlertIdentities(); err != nil {
						log.Printf("usage alert account identities: %v", err)
						telemetry.ReportCause(context.Background(), "usages", "usage alert account identities")
						continue
					}
					if err := s.Switcher.OverlayUsages(&snapshot); err != nil {
						log.Printf("usage alert account overlay: %v", err)
						telemetry.ReportCause(context.Background(), "usages", "usage alert account overlay")
						continue
					}
				}
				changed, err := tracker.Observe(snapshot)
				if err != nil {
					log.Printf("usage alerts: %v", err)
					telemetry.ReportCause(context.Background(), "usages", "usage alerts")
				} else {
					s.recordRejectedLogins(tracker)
					s.refreshRejectedLogins(ctx, tracker.ActiveRejectedLogins())
					if changed {
						s.usageAlertRevision.Add(1)
						s.nudgeUsageWatchers()
					}
				}
			}
		}
	}()
	s.signalUsageAlerts()
	return nil
}

func (s *Server) signalUsageAlerts() {
	if s.usageAlertWake == nil {
		return
	}
	select {
	case s.usageAlertWake <- struct{}{}:
	default:
	}
}

// usageLoginRejectedEvent is the owner's bell event for a login-rejection
// episode: a usage poll answered 401 or 403 and the account stopped working.
const usageLoginRejectedEvent = "usage_login_rejected"

// recordRejectedLogins writes the login-rejection episodes the last usage
// observation started as project-less owner events (the bell). The Inbox
// item of an episode is the tracker's own; this is only the event.
func (s *Server) recordRejectedLogins(tracker *usages.AlertTracker) {
	logins := tracker.DrainRejectedLogins()
	for _, login := range logins {
		payload := map[string]string{
			"tool": login.Tool, "profile": login.Profile, "hint": login.Hint}
		// The time the poller first saw the rejection, not the time of the
		// last poll, and the tail of the rejected credential's hash: the
		// owner can say when the login stopped working and which login it
		// was.
		if !login.FirstSeen.IsZero() {
			payload["first_seen"] = login.FirstSeen.UTC().Format(time.RFC3339)
		}
		if login.TokenTail != "" {
			payload["token_tail"] = login.TokenTail
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.Store.RecordQuotaEvent(ctx, usageLoginRejectedEvent, payload)
		cancel()
		if err != nil {
			log.Printf("usage alerts: record %s event: %v", usageLoginRejectedEvent, err)
			telemetry.ReportError(context.Background(), "usages", fmt.Errorf("recording a %s event: %w", usageLoginRejectedEvent, err))
		}
	}
}

// watchUsages subscribes to snapshot and visible-alert changes, including
// guard hold edges. The channel holds at most one pending nudge, so a burst
// of refreshes reaches a slow socket as one frame.
// Call the returned function to unsubscribe. With usages disabled the channel
// never fires.
func (s *Server) watchUsages() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.usageWatchMu.Lock()
	if s.usageWatchers == nil {
		s.usageWatchers = map[chan struct{}]struct{}{}
	}
	s.usageWatchers[ch] = struct{}{}
	s.usageWatchMu.Unlock()
	return ch, func() {
		s.usageWatchMu.Lock()
		delete(s.usageWatchers, ch)
		s.usageWatchMu.Unlock()
	}
}

// usagesChanged is the usages Reader's update hook. It never blocks.
func (s *Server) usagesChanged() {
	s.signalUsageAlerts()
	s.kickCodexOverwrite() // a refresh is a moment to look at the live Codex login again (in the background: the reader holds its lock)
	if runner := s.quotaRunner(); runner != nil {
		runner.Trigger() // a new snapshot is a reason to evaluate the quota policy
		if guard := s.limitGuard(); guard != nil {
			guard.Trigger()
		}
	}
	s.nudgeUsageWatchers()
}

func (s *Server) nudgeUsageWatchers() {
	s.usageWatchMu.Lock()
	defer s.usageWatchMu.Unlock()
	for ch := range s.usageWatchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// refreshRejectedLogins starts the reactive refresh for each active profile the usage poll read as rejected. The refresher owns the
// limits (the tools it manages, the active profile, one attempt per access token), so listing the same rejection on every poll is
// cheap; what runs here is only the single-flight per profile, because a spend can take minutes and must not block the poll loop. When
// the login rotated, the usage poll is re-read, so the rejected state clears and spawns and resumes work again without a person.
func (s *Server) refreshRejectedLogins(ctx context.Context, logins []usages.RejectedLogin) {
	tr := s.currentRefresher()
	if tr == nil {
		return
	}
	for _, login := range logins {
		key := login.Tool + "/" + login.Profile
		s.reactiveMu.Lock()
		if s.reactiveBusy == nil {
			s.reactiveBusy = map[string]bool{}
		}
		// A cancelled context starts nothing: once the server (or a test) has cancelled it and joined the runs, none can slip in.
		if ctx.Err() != nil || s.reactiveBusy[key] {
			s.reactiveMu.Unlock()
			continue
		}
		s.reactiveBusy[key] = true
		s.reactiveRuns++
		s.reactiveMu.Unlock()
		tool, profile := login.Tool, login.Profile
		telemetry.Go(ctx, func() {
			defer func() {
				s.reactiveMu.Lock()
				delete(s.reactiveBusy, key)
				s.reactiveRuns--
				if s.reactiveRuns == 0 && s.reactiveIdle != nil {
					s.reactiveIdle.Broadcast()
				}
				s.reactiveMu.Unlock()
			}()
			if res := tr.RefreshRejected(ctx, tool, profile); res.Outcome == usages.ReactiveRotated {
				s.refreshSwitchedUsage(tool)
			}
		})
	}
}

// waitReactiveRefreshes blocks until no reactive refresh is in flight. Callers cancel the context first, so none starts meanwhile.
func (s *Server) waitReactiveRefreshes() {
	s.reactiveMu.Lock()
	defer s.reactiveMu.Unlock()
	if s.reactiveIdle == nil {
		s.reactiveIdle = sync.NewCond(&s.reactiveMu)
	}
	for s.reactiveRuns > 0 {
		s.reactiveIdle.Wait()
	}
}

// clearReactiveStop ends the reactive refresh's stop for tool (see TargetRefresher.ClearReactiveStop): called when a login is imported
// through the switcher or the active profile is switched.
func (s *Server) clearReactiveStop(tool string) {
	if tr := s.currentRefresher(); tr != nil {
		tr.ClearReactiveStop(tool)
	}
}

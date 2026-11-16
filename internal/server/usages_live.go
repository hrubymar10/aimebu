package server

import (
	"context"
	"log"

	"github.com/hrubymar10/aimebu/internal/usages"
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
	s.usageAlertWake = make(chan struct{}, 1)
	s.Usages.SetUpdateHook(s.usagesChanged)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.usageAlertWake:
				snapshot := s.Usages.Snapshot()
				if s.Switcher != nil {
					if err := s.Switcher.EnsureUsageAlertIdentities(); err != nil {
						log.Printf("usage alert account identities: %v", err)
						continue
					}
					if err := s.Switcher.OverlayUsages(&snapshot); err != nil {
						log.Printf("usage alert account overlay: %v", err)
						continue
					}
				}
				changed, err := tracker.Observe(snapshot)
				if err != nil {
					log.Printf("usage alerts: %v", err)
				} else if changed {
					s.usageAlertRevision.Add(1)
					s.nudgeUsageWatchers()
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

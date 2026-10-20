package server

// watchUsages subscribes to snapshot changes. The channel holds at most one
// pending nudge, so a burst of refreshes reaches a slow socket as one frame.
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
	if runner := s.quotaRunner(); runner != nil {
		runner.Trigger() // a new snapshot is a reason to evaluate the quota policy
		if guard := s.limitGuard(); guard != nil {
			guard.Trigger()
		}
	}
	s.usageWatchMu.Lock()
	defer s.usageWatchMu.Unlock()
	for ch := range s.usageWatchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

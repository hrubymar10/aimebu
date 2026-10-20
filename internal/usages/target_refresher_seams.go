package usages

import "context"

// RefreshExecutor runs the provider CLI against a throwaway credential directory.
// It is exported only so an end-to-end test outside this package can stand in
// for the docker executors; production wiring never sets one.
type RefreshExecutor = refreshExecutor

// SetExecutor replaces the CLI executor of one tool ("claude" or "codex").
func (r *TargetRefresher) SetExecutor(tool string, e RefreshExecutor) {
	r.executors[tool] = e
}

// SetPreflight replaces the harness-docker-ctrl and docker reachability checks.
// A nil function keeps the current one.
func (r *TargetRefresher) SetPreflight(available func() bool, reachable func(context.Context) bool) {
	if available != nil {
		r.available = available
	}
	if reachable != nil {
		r.reachable = reachable
	}
}

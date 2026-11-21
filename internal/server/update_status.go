package server

import (
	"context"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/hrubymar10/aimebu/internal/configroot"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/telemetry"
	"github.com/hrubymar10/aimebu/internal/updatecheck"
)

// The server's own update check: it follows the same release-channel rules as
// the bin/aimebu wrapper, never prompts, and only tells the admins there is
// something to do. The web banner carries the copy, remind and skip actions.

const (
	updateCheckInterval = 6 * time.Hour
	updateCheckDelay    = 30 * time.Second
	updateCheckTimeout  = 10 * time.Second
)

// updateStatus holds the latest check's conclusion behind a lock.
type updateStatus struct {
	mu     sync.Mutex
	status updatecheck.Status
	set    bool
}

func (u *updateStatus) snapshot() (updatecheck.Status, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status, u.set
}

func (u *updateStatus) store(status updatecheck.Status) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = status
	u.set = true
}

// StartUpdateCheck runs the periodic check for the life of ctx. It starts 30
// seconds after the call, then every 6 hours with a little jitter so a farm of
// servers does not all knock on the remote in the same second. A clone the
// wrapper did not make (a bare `go install` binary) has nothing to check, and
// AIMEBU_UPDATE_CHECK=0 turns the whole loop off.
func (s *Server) StartUpdateCheck(ctx context.Context) {
	if os.Getenv("AIMEBU_UPDATE_CHECK") == "0" {
		return
	}
	layout, ok := updatecheck.LayoutFromExecutable()
	if !ok {
		return
	}
	configRoot, _ := configroot.FromEnv()
	runningVersion := s.Version
	started := time.Now()
	remote := os.Getenv("AIMEBU_UPDATE_REMOTE")
	go func() {
		defer telemetry.Recover(ctx)
		delay := updateCheckDelay + time.Duration(rand.Float64()*float64(time.Minute))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		s.runUpdateCheck(ctx, layout, runningVersion, started, configRoot, remote)
		ticker := time.NewTicker(updateCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runUpdateCheck(ctx, layout, runningVersion, started, configRoot, remote)
			}
		}
	}()
}

func (s *Server) runUpdateCheck(ctx context.Context, layout updatecheck.CloneLayout, runningVersion string, started time.Time, configRoot, remote string) {
	status, err := updatecheck.Check(layout, runningVersion, started, configRoot, remote)
	if err != nil {
		// An unreachable remote or a lost clone is not an incident: the next
		// tick tries again, quietly.
		return
	}
	s.recordUpdateStatus(ctx, status)
}

// recordUpdateStatus stores one successful check's conclusion and announces a change.
func (s *Server) recordUpdateStatus(ctx context.Context, status updatecheck.Status) {
	previous, hadPrevious := s.updateStatus.snapshot()
	if status.State == "" && previous.State == "" {
		return // nothing to report before and nothing now
	}
	// A later successful check that finds nothing (the update was applied and the
	// server restarted onto it, or the release was withdrawn) clears the stored
	// status and tells the admins' banners to close.
	s.updateStatus.store(status)
	if !hadPrevious || previous.State != status.State || previous.Version != status.Version {
		s.Store.RecordUpdateStatusEvent(ctx, status.State, status.Version)
	}
}

// requireUpdateAdmin answers 403 unless the signed-in person is the owner or an
// admin (IsGlobalAdmin covers both), and reports whether the request may go on.
func (s *Server) requireUpdateAdmin(w http.ResponseWriter, r *http.Request) bool {
	admin, err := s.Store.IsGlobalAdmin(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return false
	}
	if !admin {
		apiError(w, 403, "forbidden", "owner or admin required")
		return false
	}
	return true
}

// updateStatusHandler is GET /api/update-status: the latest check's conclusion
// plus the signed-in person's ack, for admins and the owner.
func (s *Server) updateStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !s.requireUpdateAdmin(w, r) {
		return
	}
	user := userID(r)
	status, set := s.updateStatus.snapshot()
	ack, hasAck, err := s.Store.UpdateStatusAck(r.Context(), user)
	if err != nil {
		fail(w, err)
		return
	}
	response := map[string]any{"checked": set}
	if set {
		response["state"] = status.State
		response["version"] = status.Version
		response["channel"] = status.Channel
		response["checked_at"] = store.TimestampString(status.CheckedAt)
	}
	if hasAck {
		response["ack"] = map[string]any{"version": ack.Version, "state": ack.State, "kind": ack.Kind, "until": ack.Until}
	}
	writeJSON(w, 200, response)
}

// updateStatusAckHandler is POST /api/update-status/skip and
// /api/update-status/remind for the signed-in person.
func (s *Server) updateStatusAckHandler(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireUpdateAdmin(w, r) {
			return
		}
		status, set := s.updateStatus.snapshot()
		if !set || status.State == "" {
			apiError(w, 409, "no_update_status", "there is no update status to answer yet")
			return
		}
		ack := store.UpdateStatusAck{UserID: userID(r), Version: status.Version, State: status.State, Kind: kind}
		if kind == "remind" {
			ack.Until = store.UntilIn24h()
		}
		if err := s.Store.SetUpdateStatusAck(r.Context(), ack); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(204)
	}
}

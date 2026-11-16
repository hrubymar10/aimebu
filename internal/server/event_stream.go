package server

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hrubymar10/aimebu/internal/auth"
	"github.com/hrubymar10/aimebu/internal/store"
	"modernc.org/sqlite"
)

const eventBatchSize = 500

type streamFailure struct {
	status websocket.StatusCode
	reason string
}

func (s *Server) batch(ctx context.Context, user string, after, through int64) ([]store.Event, error) {
	query := s.EventBatchQuery
	if query == nil {
		query = s.Store.EventsBatchAfter
	}
	events, err := retryBusy(ctx, func() ([]store.Event, error) { return query(ctx, user, after, through, eventBatchSize) })
	if err != nil {
		return nil, err
	}
	// The events the system-events feed shows carry who their ids are (see store.Event.Names). Only those: naming every message and receipt of a busy room
	// would be two more queries per poll for nobody.
	var feed []store.Event
	var at []int
	for i, event := range events {
		if store.IsSystemEventKind(event.Kind) {
			feed = append(feed, event)
			at = append(at, i)
		}
	}
	if len(feed) > 0 {
		if err := s.Store.NameEvents(ctx, feed); err != nil {
			return nil, err
		}
		for k, i := range at {
			events[i].Names = feed[k].Names
			events[i].Notable = store.IsNotableSystemEvent(events[i].Kind, events[i].PayloadJSON)
		}
	}
	return events, nil
}

func retryBusy[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var result T
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		result, err = fn()
		if err == nil || !isBusy(err) || attempt == 3 {
			return result, err
		}
		wait := time.Duration(10<<attempt) * time.Millisecond
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
	}
	return result, err
}

func isBusy(err error) bool {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code()&0xff == 5
	}
	return strings.Contains(err.Error(), "SQLITE_BUSY")
}

func (s *Server) socketSessionFailure(ctx context.Context, token string) *streamFailure {
	check := s.SessionCheckQuery
	if check == nil {
		check = func(ctx context.Context, token string) error {
			_, err := s.Auth.CheckSession(ctx, token)
			return err
		}
	}
	_, err := retryBusy(ctx, func() (struct{}, error) { return struct{}{}, check(ctx, token) })
	if err == nil {
		return nil
	}
	if errors.Is(err, auth.ErrInvalidSession) {
		return &streamFailure{websocket.StatusPolicyViolation, "session expired or revoked"}
	}
	return &streamFailure{websocket.StatusInternalError, "session check failed; reconnect"}
}

func (s *Server) streamEvents(ctx context.Context, conn *websocket.Conn, user, token string, after int64) {
	bounds := func() (struct{ min, max int64 }, error) {
		min, max, err := s.Store.EventBounds(ctx)
		return struct{ min, max int64 }{min, max}, err
	}
	initial, err := retryBusy(ctx, bounds)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "event replay failed; reconnect")
		return
	}
	if initial.min > 0 && after < initial.min-1 {
		_ = s.writeFrame(ctx, conn, map[string]any{"type": "snapshot_required", "min_seq": initial.min})
		return
	}
	if after > initial.max {
		_ = s.writeFrame(ctx, conn, map[string]any{"type": "snapshot_required", "max_seq": initial.max})
		return
	}
	if failure := s.socketSessionFailure(ctx, token); failure != nil {
		conn.Close(failure.status, failure.reason)
		return
	}
	checkInterval := s.SessionCheckInterval
	if checkInterval <= 0 {
		checkInterval = 30 * time.Second
	}
	nextCheck := time.Now().Add(checkInterval)
	checkDuringReplay := func() bool {
		if time.Now().Before(nextCheck) {
			return true
		}
		if failure := s.socketSessionFailure(ctx, token); failure != nil {
			conn.Close(failure.status, failure.reason)
			return false
		}
		nextCheck = time.Now().Add(checkInterval)
		return true
	}
	cursor := after
	for {
		if !checkDuringReplay() {
			return
		}
		batch, err := s.batch(ctx, user, cursor, initial.max)
		if err != nil {
			conn.Close(websocket.StatusInternalError, "event replay failed; reconnect")
			return
		}
		for _, event := range batch {
			if !checkDuringReplay() {
				return
			}
			if err := s.writeFrame(ctx, conn, eventFrame(event)); err != nil {
				return
			}
		}
		if len(batch) < eventBatchSize {
			break
		}
		cursor = batch[len(batch)-1].Seq
	}
	if !checkDuringReplay() {
		return
	}
	usageUpdates, unwatchUsages := s.watchUsages()
	defer unwatchUsages()
	heartbeat := s.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = 20 * time.Second
	}
	// `heartbeat` (seconds) tells the page to expect a frame at least that often, and `version` is the build serving it: a page that finds a
	// different version after a reconnect knows it was loaded from an older build.
	if err := s.writeFrame(ctx, conn, map[string]any{"type": "ready", "seq": initial.max, "heartbeat": int(math.Ceil(heartbeat.Seconds())), "version": s.Version}); err != nil {
		return
	}
	beat := time.NewTicker(heartbeat)
	defer beat.Stop()
	interval := s.PollInterval
	if interval <= 0 {
		interval = 50 * time.Millisecond
	}
	size := s.QueueSize
	if size <= 0 {
		size = 32
	}
	queue := make(chan store.Event, size)
	failure := make(chan streamFailure, 1)
	fail := func(status websocket.StatusCode, reason string) {
		select {
		case failure <- streamFailure{status, reason}:
		default:
		}
	}
	go func() {
		poll := time.NewTicker(interval)
		defer poll.Stop()
		session := time.NewTicker(checkInterval)
		defer session.Stop()
		cursor := initial.max
		for {
			select {
			case <-ctx.Done():
				return
			case <-session.C:
				if problem := s.socketSessionFailure(ctx, token); problem != nil {
					fail(problem.status, problem.reason)
					return
				}
			case <-poll.C:
				current, err := retryBusy(ctx, bounds)
				if err != nil {
					fail(websocket.StatusInternalError, "event poll failed; reconnect")
					return
				}
				if current.max <= cursor {
					continue
				}
				for cursor < current.max {
					batch, err := s.batch(ctx, user, cursor, current.max)
					if err != nil {
						fail(websocket.StatusInternalError, "event poll failed; reconnect")
						return
					}
					for _, event := range batch {
						select {
						case queue <- event:
						case <-ctx.Done():
							return
						default:
							fail(websocket.StatusPolicyViolation, "slow client")
							return
						}
					}
					if len(batch) < eventBatchSize {
						cursor = current.max
						break
					}
					cursor = batch[len(batch)-1].Seq
				}
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case problem := <-failure:
			_ = conn.Close(problem.status, problem.reason)
			return
		case event := <-queue:
			if err := s.writeFrame(ctx, conn, eventFrame(event)); err != nil {
				return
			}
		case <-beat.C:
			// A connection nothing has reached for a while (a sleeping laptop, a proxy that dropped an idle socket) can look open to the page for
			// minutes; a frame at a steady pace lets it tell, and keeps idle proxies from closing the socket.
			if err := s.writeFrame(ctx, conn, map[string]string{"type": "heartbeat"}); err != nil {
				return
			}
		case <-usageUpdates:
			// A transient hint, not a logged event: it has no seq and is
			// never replayed. The owner re-reads usages; admins re-read Inbox
			// alerts when their visible state changes, including guard edges.
			if admin, err := s.Store.IsGlobalAdmin(ctx, user); err != nil || !admin {
				continue
			}
			if err := s.writeFrame(ctx, conn, map[string]any{"type": "usages_updated", "alert_revision": s.usageAlertRevision.Load()}); err != nil {
				return
			}
		}
	}
}

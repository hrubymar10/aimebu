package server

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/coder/websocket"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// trackSocket joins an upgraded connection to the server's shutdown. HTTP
// Shutdown does not own hijacked connections, so they need their own drain.
func (s *Server) trackSocket(parent context.Context, conn *websocket.Conn) (context.Context, func(), bool) {
	s.socketMu.Lock()
	if s.socketClosing {
		s.socketMu.Unlock()
		_ = conn.Close(websocket.StatusGoingAway, "server restarting")
		return nil, nil, false
	}
	s.socketWG.Add(1)
	s.sockets[conn] = struct{}{}
	s.socketMu.Unlock()

	ctx, cancel := context.WithCancel(parent)
	closed := make(chan struct{})
	stop := context.AfterFunc(s.socketCtx, func() {
		_ = conn.Close(websocket.StatusGoingAway, "server restarting")
		cancel()
		close(closed)
	})
	release := func() {
		if !stop() {
			<-closed
		}
		cancel()
		s.socketMu.Lock()
		delete(s.sockets, conn)
		s.socketMu.Unlock()
		s.socketWG.Done()
	}
	return ctx, release, true
}

func (s *Server) socketsClosing() bool {
	s.socketMu.Lock()
	defer s.socketMu.Unlock()
	return s.socketClosing
}

// ShutdownSockets closes WebSockets with 1001 and joins their handlers before
// the caller closes the store or an embedded supervisor's journal. If the
// deadline expires, it force closes sockets and gives handlers one final
// bounded interval; a stuck handler is reported and the caller can proceed.
func (s *Server) ShutdownSockets(ctx context.Context) error {
	s.socketMu.Lock()
	if !s.socketClosing {
		s.socketClosing = true
		s.socketCancel()
	}
	s.socketMu.Unlock()
	done := make(chan struct{})
	go func() { s.socketWG.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.socketMu.Lock()
		for conn := range s.sockets {
			_ = conn.CloseNow()
		}
		s.socketMu.Unlock()
		wait := s.socketForceWait
		if wait <= 0 {
			wait = 5 * time.Second
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-done:
			return ctx.Err()
		case <-timer.C:
			err := fmt.Errorf("websocket handlers did not stop after forced close: %w", ctx.Err())
			log.Print(err)
			telemetry.ReportWarning(context.Background(), "server", err)
			return err
		}
	}
}

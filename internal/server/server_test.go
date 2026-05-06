package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func setupTestServer(t *testing.T) (*store, *httptest.Server) {
	t.Helper()
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	setupHandlers(mux, s)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

// TestRoomWaitCursorNotAdvancedOnContextCancel verifies that cancelling the
// request context (simulating a harness timeout) does not advance the agent's
// read cursor. A message posted after the disconnect must be replayed on the
// next bus_wait call.
//
// Note: this test exercises the context.Done() exit path. The tighter race —
// message received on the channel, context cancels before the write — is
// protected by the r.Context().Err() guard added in the same commit and is
// validated by code review.
func TestRoomWaitCursorNotAdvancedOnContextCancel(t *testing.T) {
	s, srv := setupTestServer(t)

	sender, err := s.registerAI("gpt5", "codex", "test", nil, "sndrone")
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "rcvrone")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.createRoom("race-room", sender.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("race-room", sender.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("race-room", receiver.ID); err != nil {
		t.Fatal(err)
	}

	// Advance cursor past join-event messages so the room wait enters slow-path.
	s.advanceCursor(receiver.ID, "race-room", s.roomHead("race-room"))
	cursorBefore := s.ensureRoomCursor(receiver.ID, "race-room")

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequestWithContext(ctx, "GET",
			srv.URL+"/rooms/race-room/wait?agent_id="+receiver.ID+"&timeout=5", nil)
		//nolint:errcheck
		http.DefaultClient.Do(req)
	}()

	// Let handler enter the slow-path select loop.
	time.Sleep(50 * time.Millisecond)

	// Simulate harness timeout: cancel the request context.
	cancel()
	time.Sleep(20 * time.Millisecond)

	// Post a message after the disconnect — handler won't deliver it.
	if _, err := s.roomSend("race-room", sender.ID, "@rcvrone hello after disconnect"); err != nil {
		t.Fatal(err)
	}

	wg.Wait()

	cursorAfter := s.ensureRoomCursor(receiver.ID, "race-room")
	if cursorAfter != cursorBefore {
		t.Errorf("cursor advanced on disconnect: was %d, now %d (message was NOT delivered to client)", cursorBefore, cursorAfter)
	}
}

// TestRoomWaitCursorAdvancedOnDelivery verifies that the read cursor advances
// after a message is successfully delivered to the waiting agent.
func TestRoomWaitCursorAdvancedOnDelivery(t *testing.T) {
	s, srv := setupTestServer(t)

	sender, err := s.registerAI("gpt5", "codex", "test", nil, "sndrtwo")
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "rcvrtwo")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.createRoom("deliver-room", sender.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("deliver-room", sender.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("deliver-room", receiver.ID); err != nil {
		t.Fatal(err)
	}

	// Advance cursor past join-event messages so the room wait enters slow-path.
	s.advanceCursor(receiver.ID, "deliver-room", s.roomHead("deliver-room"))
	cursorBefore := s.ensureRoomCursor(receiver.ID, "deliver-room")

	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		req, _ := http.NewRequest("GET",
			srv.URL+"/rooms/deliver-room/wait?agent_id="+receiver.ID+"&timeout=5", nil)
		//nolint:errcheck
		http.DefaultClient.Do(req)
	}()

	// Let handler enter slow-path.
	time.Sleep(50 * time.Millisecond)

	// Post a message — handler should deliver it and advance the cursor.
	if _, err := s.roomSend("deliver-room", sender.ID, "hello rcvrtwo"); err != nil {
		t.Fatal(err)
	}

	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("wait handler did not complete within timeout")
	}

	cursorAfter := s.ensureRoomCursor(receiver.ID, "deliver-room")
	if cursorAfter <= cursorBefore {
		t.Errorf("cursor did not advance after delivery: was %d, now %d", cursorBefore, cursorAfter)
	}
}

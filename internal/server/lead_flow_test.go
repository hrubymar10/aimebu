package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/supervisor"
)

// The fake lead chooses each toolbox action and answer. The server still
// performs real dispatch, answer validation, inbox wake, spawn, and closure.
func TestFakeLeadCoordinatesTaskFromHomeToArchive(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := t.Context()
	lead, key := f.createToolAgent(t, "role_leader_default")
	allRooms, err := f.db.RoomsForProject(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var homeID string
	for _, room := range allRooms {
		if room.Kind == "home" {
			homeID = room.ID
		}
	}
	if homeID == "" {
		t.Fatal("missing home room")
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if err := tx.AddMember(homeID, "agent", lead.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES (?, ?,0,'fresh_with_history',?)`, "ses_lead_flow", lead.ID, "2026-10-14T00:00:00.000000000Z")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	j, err := supervisor.OpenJournal(ctx, filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	fake := adapter.NewFake()
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, j, fake)
	if err := client.SetAgentKey(lead.ID, key); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RunOnce(runCtx) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor not ready")
	}

	tool := func(ctx context.Context, method, path, requestKey string, body any, out any) error {
		var input bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&input).Encode(body); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, f.http.URL+path, &input)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", requestKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, data)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	waitTurn := func(id string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			state, err := f.db.DeliveryState(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if state.Exec == "exited" && state.Settle == "settled" {
				if state.Outcome != "completed" {
					_, receipt, receiptErr := f.db.ReceiptByTurn(ctx, id, 0)
					t.Fatalf("turn %s: %+v receipt=%s receiptErr=%v", id, state, receipt, receiptErr)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("turn %s did not settle: %+v", id, state)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	request := f.request(http.MethodPost, "/api/rooms/"+homeID+"/messages", map[string]string{"body": "Please build a report"}, true)
	if request.StatusCode != 201 {
		t.Fatalf("home request status=%d", request.StatusCode)
	}
	request.Body.Close()
	first, ok, err := f.db.PrepareDispatch(ctx, lead.ID, "")
	if err != nil || !ok {
		t.Fatalf("first dispatch ready=%v err=%v", ok, err)
	}
	roomCh := make(chan string, 1)
	var taskID string
	fake.Script(first.TurnID,
		adapter.Step{Kind: adapter.StepEffect, Effect: func(ctx context.Context) error {
			var room store.Room
			if err := tool(ctx, http.MethodPost, "/api/agent/rooms", "flow-room", map[string]string{"title": "Build report", "parent_id": homeID}, &room); err != nil {
				return err
			}
			taskID = room.ID
			roomCh <- taskID
			return nil
		}},
		adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer}, AnswerFunc: func() json.RawMessage {
			data, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"room": taskID, "body": "Which worker should take this request?", "ask": map[string]any{"kind": "question", "text": "Choose a worker", "options": []string{"Build worker"}}}}})
			return data
		}},
		adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	if err := f.srv.Dispatch(ctx, first); err != nil {
		t.Fatal(err)
	}
	waitTurn(first.TurnID)
	taskID = <-roomCh

	me := f.request(http.MethodGet, "/api/me", nil, false)
	var owner struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(me.Body).Decode(&owner); err != nil {
		t.Fatal(err)
	}
	me.Body.Close()
	items, err := f.db.InboxForUser(ctx, owner.ID)
	if err != nil || len(items) != 1 || items[0].RoomID != taskID || len(items[0].Options) != 1 || items[0].Options[0] != "Build worker" {
		t.Fatalf("worker question=%+v err=%v", items, err)
	}
	answerBody, _ := json.Marshal(map[string]any{"body": "Build worker", "rev": items[0].Rev})
	answerReq, err := http.NewRequest(http.MethodPost, f.http.URL+"/api/inbox/"+items[0].ID+"/answer", bytes.NewReader(answerBody))
	if err != nil {
		t.Fatal(err)
	}
	answerReq.Header.Set("Content-Type", "application/json")
	answerReq.Header.Set("X-CSRF-Token", f.csrf)
	answerReq.Header.Set("Idempotency-Key", "flow-worker-answer")
	answerReq.AddCookie(f.cookie)
	answer, err := http.DefaultClient.Do(answerReq)
	if err != nil {
		t.Fatal(err)
	}
	if answer.StatusCode != 201 {
		t.Fatalf("worker answer status=%d", answer.StatusCode)
	}
	answer.Body.Close()
	second, ok, err := f.db.PrepareDispatch(ctx, lead.ID, "")
	if err != nil || !ok {
		t.Fatalf("second dispatch ready=%v err=%v", ok, err)
	}
	spawnedCh := make(chan string, 1)
	fake.Script(second.TurnID,
		adapter.Step{Kind: adapter.StepEffect, Effect: func(ctx context.Context) error {
			var card store.AgentCard
			if err := tool(ctx, http.MethodPost, "/api/agent/agents", "flow-worker", map[string]string{"room_id": taskID, "role_key": "worker", "tool": "fake", "variant": "test", "model": "test"}, &card); err != nil {
				return err
			}
			spawnedCh <- card.ID
			return nil
		}},
		adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer}, AnswerFunc: func() json.RawMessage {
			data, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"room": taskID, "body": "@worker Please build the report and hand back the result."}}})
			return data
		}},
		adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	if err := f.srv.Dispatch(ctx, second); err != nil {
		t.Fatal(err)
	}
	waitTurn(second.TurnID)
	workerID := <-spawnedCh
	handoff, err := f.db.InsertMessage(ctx, store.Message{RoomID: taskID, AuthorKind: "agent", AuthorID: workerID, Body: "Report complete: 3 findings.", Handoff: true})
	if err != nil {
		t.Fatal(err)
	}
	var third protocol.Delivery
	ok = false
	deadline := time.Now().Add(4 * time.Second)
	for !ok && time.Now().Before(deadline) {
		third, ok, err = f.db.PrepareDispatch(ctx, lead.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !ok {
		note, noteErr := f.db.NoteForAgentMessage(ctx, lead.ID, handoff.ID)
		t.Fatalf("handoff not dispatched: note=%+v err=%v", note, noteErr)
	}
	closeErrors := make(chan error, 1)
	fake.Script(third.TurnID,
		adapter.Step{Kind: adapter.StepEmit, Event: adapter.Event{Kind: adapter.EventFinalAnswer}, AnswerFunc: func() json.RawMessage {
			data, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"room": homeID, "body": "The report is complete: 3 findings. The task room is being archived."}}})
			return data
		}},
		adapter.Step{Kind: adapter.StepEffect, Effect: func(ctx context.Context) error {
			deadline := time.Now().Add(4 * time.Second)
			for {
				messages, err := f.db.ListMessagesAfter(ctx, homeID, 0)
				if err != nil {
					return err
				}
				for _, message := range messages {
					if strings.Contains(message.Body, "The report is complete") {
						var closed store.Room
						err := tool(ctx, http.MethodPost, "/api/agent/rooms/"+taskID+"/close", "flow-close", map[string]any{"lifecycle_rev": 0}, &closed)
						closeErrors <- err
						return err
					}
				}
				if time.Now().After(deadline) {
					err := fmt.Errorf("home summary was not committed")
					closeErrors <- err
					return err
				}
				time.Sleep(10 * time.Millisecond)
			}
		}},
		adapter.Step{Kind: adapter.StepExit, Exit: adapter.ExitInfo{TerminalEvent: true}})
	if err := f.srv.Dispatch(ctx, third); err != nil {
		t.Fatal(err)
	}
	if err := <-closeErrors; err != nil {
		t.Fatalf("close effect: %v", err)
	}
	waitTurn(third.TurnID)
	task, err := f.db.RoomByID(ctx, taskID)
	if err != nil || task.Lifecycle != "closing" {
		t.Fatalf("task after lead close=%+v err=%v", task, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop")
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const agentGenerationPipeBuffer = 64 * 1024

var agentGenerationRetryInterval = 250 * time.Millisecond

type generationObservation struct {
	GenerationMS int64
	SessionID    string
	Complete     bool
}

// generationDetector extracts the interval from the model beginning a turn
// to its first tool call. The timestamp is the wrapper's observation time;
// not every harness includes timestamps in its own event payload.
type generationDetector interface {
	Observe(line []byte, at time.Time) generationObservation
}

func newGenerationDetector(harness, sessionID string) generationDetector {
	switch harness {
	case "pi":
		return &piGenerationDetector{sessionID: sessionID}
	case "codex":
		return &codexGenerationDetector{sessionID: sessionID}
	case "claude-code":
		return &claudeGenerationDetector{sessionID: sessionID}
	default:
		return nil
	}
}

type piGenerationDetector struct {
	startedAt time.Time
	sessionID string
}

func (d *piGenerationDetector) Observe(line []byte, at time.Time) generationObservation {
	var event struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if json.Unmarshal(line, &event) != nil {
		return generationObservation{}
	}
	switch event.Type {
	case "session":
		if event.ID != "" {
			d.sessionID = event.ID
			return generationObservation{SessionID: event.ID}
		}
	case "turn_start":
		d.startedAt = at
	case "tool_execution_start":
		return d.complete(at)
	case "turn_end":
		d.startedAt = time.Time{}
	}
	return generationObservation{}
}

func (d *piGenerationDetector) complete(at time.Time) generationObservation {
	if d.startedAt.IsZero() || d.sessionID == "" || at.Before(d.startedAt) {
		return generationObservation{}
	}
	ms := at.Sub(d.startedAt).Milliseconds()
	d.startedAt = time.Time{}
	return generationObservation{GenerationMS: ms, SessionID: d.sessionID, Complete: true}
}

type codexGenerationDetector struct {
	startedAt time.Time
	sessionID string
}

func (d *codexGenerationDetector) Observe(line []byte, at time.Time) generationObservation {
	var event map[string]any
	if json.Unmarshal(line, &event) != nil {
		return generationObservation{}
	}
	typ, _ := event["type"].(string)
	switch typ {
	case "thread.started":
		if id, _ := event["thread_id"].(string); id != "" {
			d.sessionID = id
			return generationObservation{SessionID: id}
		}
	case "turn.started":
		d.startedAt = at
	case "item.started":
		if codexGenerationToolItem(event) {
			return d.complete(at)
		}
	case "turn.completed":
		d.startedAt = time.Time{}
	}
	return generationObservation{}
}

func codexGenerationToolItem(event map[string]any) bool {
	item, _ := event["item"].(map[string]any)
	typ, _ := item["type"].(string)
	switch typ {
	case "mcp_tool_call", "command_execution", "file_change":
		return true
	default:
		return false
	}
}

func (d *codexGenerationDetector) complete(at time.Time) generationObservation {
	if d.startedAt.IsZero() || d.sessionID == "" || at.Before(d.startedAt) {
		return generationObservation{}
	}
	ms := at.Sub(d.startedAt).Milliseconds()
	d.startedAt = time.Time{}
	return generationObservation{GenerationMS: ms, SessionID: d.sessionID, Complete: true}
}

type claudeGenerationDetector struct {
	startedAt time.Time
	sessionID string
}

func (d *claudeGenerationDetector) Observe(line []byte, at time.Time) generationObservation {
	var event map[string]any
	if json.Unmarshal(line, &event) != nil {
		return generationObservation{}
	}
	if id, _ := event["session_id"].(string); id != "" {
		d.sessionID = id
	}
	typ, _ := event["type"].(string)
	switch typ {
	case "user":
		if claudeUserHasToolResult(event) {
			d.startedAt = at
		}
	case "assistant":
		if !d.startedAt.IsZero() && d.sessionID != "" && !at.Before(d.startedAt) {
			ms := at.Sub(d.startedAt).Milliseconds()
			d.startedAt = time.Time{}
			return generationObservation{GenerationMS: ms, SessionID: d.sessionID, Complete: true}
		}
	}
	return generationObservation{}
}

func claudeUserHasToolResult(event map[string]any) bool {
	message, _ := event["message"].(map[string]any)
	content, _ := message["content"].([]any)
	for _, value := range content {
		block, _ := value.(map[string]any)
		if typ, _ := block["type"].(string); typ == "tool_result" {
			return true
		}
	}
	return false
}

type generationSample struct {
	GenerationMS int64  `json:"generation_ms"`
	SessionID    string `json:"session_id"`
}

type agentGenerationStream struct {
	mu     sync.Mutex
	ch     chan []byte
	closed bool
}

func newAgentGenerationStream() *agentGenerationStream {
	return &agentGenerationStream{ch: make(chan []byte, agentGenerationPipeBuffer)}
}

func (s *agentGenerationStream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	cp := append([]byte(nil), p...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return len(p), nil
	}
	select {
	case s.ch <- cp:
	default:
	}
	return len(p), nil
}

func (s *agentGenerationStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.ch)
	return nil
}

func startAgentGenerationPusher(ctx context.Context, aimebuURL string, agentID *agentIDProvider, det generationDetector) io.WriteCloser {
	if det == nil {
		return discardWriteCloser{}
	}
	stream := newAgentGenerationStream()
	go agentGenerationPusher(ctx, aimebuURL, agentID, det, stream.ch)
	return stream
}

type agentTelemetryWriter struct {
	writers []io.WriteCloser
}

func startAgentTelemetryWriter(ctx context.Context, aimebuURL string, agentID *agentIDProvider, harness, sessionID string) io.WriteCloser {
	return &agentTelemetryWriter{writers: []io.WriteCloser{
		startAgentStatePusher(ctx, aimebuURL, agentID, newStateDetector(harness)),
		startAgentGenerationPusher(ctx, aimebuURL, agentID, newGenerationDetector(harness, sessionID)),
	}}
}

func (w *agentTelemetryWriter) Write(p []byte) (int, error) {
	for _, writer := range w.writers {
		if _, err := writer.Write(p); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (w *agentTelemetryWriter) Close() error {
	var firstErr error
	for _, writer := range w.writers {
		if err := writer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func agentGenerationPusher(ctx context.Context, aimebuURL string, agentID *agentIDProvider, det generationDetector, in <-chan []byte) {
	var pendingLine []byte
	var pending []generationSample
	ticker := time.NewTicker(agentGenerationRetryInterval)
	defer ticker.Stop()

	flush := func() {
		id := agentID.Get()
		for id != "" && len(pending) > 0 {
			if err := pushGenerationSample(ctx, aimebuURL, id, pending[0]); err != nil {
				return
			}
			pending = pending[1:]
		}
	}
	handleLine := func(line []byte) {
		observation := det.Observe(bytes.TrimRight(line, "\r"), time.Now().UTC())
		if observation.Complete && observation.GenerationMS >= 0 && observation.SessionID != "" {
			pending = append(pending, generationSample{
				GenerationMS: observation.GenerationMS,
				SessionID:    observation.SessionID,
			})
		}
		flush()
	}
	handleChunk := func(chunk []byte) {
		for len(chunk) > 0 {
			idx := bytes.IndexByte(chunk, '\n')
			if idx < 0 {
				pendingLine = append(pendingLine, chunk...)
				return
			}
			pendingLine = append(pendingLine, chunk[:idx]...)
			handleLine(pendingLine)
			pendingLine = pendingLine[:0]
			chunk = chunk[idx+1:]
		}
	}

	for {
		select {
		case chunk, ok := <-in:
			if !ok {
				if len(pendingLine) > 0 {
					handleLine(pendingLine)
				}
				flush()
				return
			}
			handleChunk(chunk)
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			return
		}
	}
}

func pushGenerationSample(ctx context.Context, aimebuURL, agentID string, sample generationSample) error {
	payload, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(aimebuURL, "/")+"/agents/"+url.PathEscape(agentID)+"/generation",
		bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &generationHTTPError{status: resp.Status}
	}
	return nil
}

type generationHTTPError struct{ status string }

func (e *generationHTTPError) Error() string { return "generation sample push: " + e.status }

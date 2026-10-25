package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/agentlog"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

const (
	// outputReadsPerMinute bounds how often one person may read agent output: it can carry secrets, and a script has no reason to poll it.
	outputReadsPerMinute = 30
	// outputWait is how long the machine has to answer.
	outputWait = 15 * time.Second
	// sizeWait bounds the Health size question, which sits in a report that waits for every machine: a silent one adds at most this long.
	sizeWait = 5 * time.Second
)

// agentOutputView is what the route returns: a bounded tail of what the agent's harness wrote to stderr, kept on the machine that ran it.
type agentOutputView struct {
	AgentID   string    `json:"agent_id"`
	Machine   string    `json:"machine_id"`
	Text      string    `json:"text"`
	Bytes     int64     `json:"bytes"`
	Files     int       `json:"files"`
	Truncated bool      `json:"truncated"`
	UpdatedAt time.Time `json:"updated_at"`
}

// agentOutput is GET /api/rooms/{room}/agents/{agent}/output?lines=N: the end of an agent's captured stderr, for a project's owner or an
// admin. The output is whatever the harness printed (its tools' too), so it is never offered to anyone else, is not cached, and every read
// is logged with who asked. The text is read from the machine on demand; it is not stored on the server.
func (s *Server) agentOutput(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	if allowed, _ := s.projectOwnerOrAdmin(w, r, room.ProjectID); !allowed {
		return
	}
	agentID := r.PathValue("agent")
	// The agent must belong to this room's project. It need not be a member now: an agent that left its room, or was stopped there, still
	// has output on its machine, and the person asking is already this project's owner or an admin.
	machineID, projectID, err := s.Store.AgentPlacement(r.Context(), agentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			apiError(w, 404, "not_found", "no such agent in this project")
			return
		}
		fail(w, err)
		return
	}
	if projectID != room.ProjectID {
		apiError(w, 404, "not_found", "no such agent in this project")
		return
	}
	lines := 0
	if raw := r.URL.Query().Get("lines"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			apiError(w, 400, "invalid_request", "lines must be a positive number")
			return
		}
		lines = n
	}
	if !s.allowOutputRead(userID(r)) {
		w.Header().Set("Retry-After", "60")
		apiError(w, 429, "rate_limited", "agent output was read too often; try again in a minute")
		return
	}
	page, err := s.machineAgentLog(r.Context(), machineID, agentID, lines)
	switch {
	case errors.Is(err, ErrMachineUnavailable):
		apiError(w, 409, "machine_offline", "the machine this agent runs on is not connected, and its output is kept there")
		return
	case errors.Is(err, errAgentLogsUnsupported):
		apiError(w, 409, "supervisor_outdated", "this machine's supervisor predates agent output: update the supervisor")
		return
	case errors.Is(err, agentlog.ErrNoLog):
		apiError(w, 404, "no_output", "no output has been captured for this agent")
		return
	case err != nil:
		apiError(w, 502, "output_unavailable", "the machine could not read the agent's output")
		return
	}
	log.Printf("agent output: %s read %d bytes of %s (room %s)", userID(r), len(page.Text), agentID, room.ID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, agentOutputView{AgentID: agentID, Machine: machineID, Text: page.Text, Bytes: page.Bytes, Files: page.Files, Truncated: page.Truncated, UpdatedAt: page.UpdatedAt})
}

// allowOutputRead records a read by a person and says whether they are within the limit.
func (s *Server) allowOutputRead(user string) bool {
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	if s.outputReads == nil {
		s.outputReads = map[string][]time.Time{}
	}
	now := time.Now()
	recent := s.outputReads[user][:0]
	for _, at := range s.outputReads[user] {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	if len(recent) >= outputReadsPerMinute {
		s.outputReads[user] = recent
		return false
	}
	s.outputReads[user] = append(recent, now)
	// Whoever has not read for a minute is forgotten, so the map does not grow with every person who ever did.
	for other, times := range s.outputReads {
		if other != user && (len(times) == 0 || now.Sub(times[len(times)-1]) >= time.Minute) {
			delete(s.outputReads, other)
		}
	}
	return true
}

var errAgentLogsUnsupported = errors.New("supervisor does not keep agent output")

// machineAgentLog asks the machine for the end of an agent's output and waits for its answer.
func (s *Server) machineAgentLog(ctx context.Context, machineID, agentID string, lines int) (agentlog.Page, error) {
	conn, err := s.machineForID(machineID)
	if err != nil {
		return agentlog.Page{}, err
	}
	if !conn.supportsAgentLogs {
		return agentlog.Page{}, errAgentLogsUnsupported
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return agentlog.Page{}, err
	}
	id := hex.EncodeToString(nonce[:])
	channel := make(chan protocol.Frame, 1)
	conn.harnessMu.Lock()
	if conn.pathPending == nil {
		conn.pathPending = make(map[string]chan protocol.Frame)
	}
	conn.pathPending[id] = channel
	conn.harnessMu.Unlock()
	defer func() { conn.harnessMu.Lock(); delete(conn.pathPending, id); conn.harnessMu.Unlock() }()
	if err := conn.send(protocol.Frame{V: protocol.Version, Type: protocol.AgentLogRead, ReqID: id, AgentID: agentID, LogLines: lines, LogMaxBytes: agentlog.MaxTailBytes}); err != nil {
		return agentlog.Page{}, err
	}
	timer := time.NewTimer(outputWait)
	defer timer.Stop()
	select {
	case frame := <-channel:
		if frame.Code == "no_log" {
			return agentlog.Page{}, agentlog.ErrNoLog
		}
		if frame.Code != "" {
			return agentlog.Page{}, errors.New(frame.Message)
		}
		var page agentlog.Page
		if err := json.Unmarshal(frame.Details, &page); err != nil {
			return page, err
		}
		return page, nil
	case <-conn.done:
		return agentlog.Page{}, ErrMachineUnavailable
	case <-ctx.Done():
		return agentlog.Page{}, ctx.Err()
	case <-timer.C:
		return agentlog.Page{}, errors.New("the machine did not answer")
	}
}

// machineAgentLogSize asks a machine how much its agent output logs hold, for the Health disk report. Only the size crosses the wire.
func (s *Server) machineAgentLogSize(ctx context.Context, machineID string) (agentlog.Size, error) {
	conn, err := s.machineForID(machineID)
	if err != nil {
		return agentlog.Size{}, err
	}
	if !conn.supportsAgentLogSize {
		return agentlog.Size{}, errAgentLogsUnsupported
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return agentlog.Size{}, err
	}
	id := hex.EncodeToString(nonce[:])
	channel := make(chan protocol.Frame, 1)
	conn.harnessMu.Lock()
	if conn.pathPending == nil {
		conn.pathPending = make(map[string]chan protocol.Frame)
	}
	conn.pathPending[id] = channel
	conn.harnessMu.Unlock()
	defer func() { conn.harnessMu.Lock(); delete(conn.pathPending, id); conn.harnessMu.Unlock() }()
	if err := conn.send(protocol.Frame{V: protocol.Version, Type: protocol.AgentLogUsage, ReqID: id}); err != nil {
		return agentlog.Size{}, err
	}
	timer := time.NewTimer(sizeWait)
	defer timer.Stop()
	select {
	case frame := <-channel:
		if frame.Code != "" {
			return agentlog.Size{}, errors.New(frame.Message)
		}
		var size agentlog.Size
		if err := json.Unmarshal(frame.Details, &size); err != nil {
			return size, err
		}
		return size, nil
	case <-conn.done:
		return agentlog.Size{}, ErrMachineUnavailable
	case <-ctx.Done():
		return agentlog.Size{}, ctx.Err()
	case <-timer.C:
		return agentlog.Size{}, errors.New("the machine did not answer")
	}
}

package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func (s *Server) machineHarnesses(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	id := r.PathValue("id")
	m, err := s.Store.MachineByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "machine not found")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	rows, err := s.Store.MachineHarnesses(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	connected := s.machineRow(m).Connected
	if connected {
		if conn, err := s.machineForID(id); err == nil {
			conn.harnessMu.Lock()
			rows = append([]protocol.HarnessRow(nil), conn.harnesses...)
			conn.harnessMu.Unlock()
		}
	} else {
		for i := range rows {
			rows[i].Available = false
			rows[i].UnavailableReason = "machine offline"
		}
	}
	if rows == nil {
		rows = []protocol.HarnessRow{}
	}
	activity, err := s.Store.HarnessActivity(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"machine_id": id, "harnesses": rows, "activity": activity})
}

func (s *Server) machineHarness(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	id, tool, variant := r.PathValue("id"), r.PathValue("tool"), r.PathValue("variant")
	if !listedHarness(tool, variant) {
		apiError(w, 404, "not_found", "harness not found")
		return
	}
	m, err := s.Store.MachineByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "machine not found")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if !m.Approved || m.RevokedAt.Valid {
		apiError(w, 409, "machine_unavailable", "machine is not approved")
		return
	}
	var req struct {
		Enabled      *bool  `json:"enabled"`
		OverridePath string `json:"override_path"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		apiError(w, 400, "invalid_request", "enabled required")
		return
	}
	conn, err := s.machineForID(id)
	if err != nil {
		apiError(w, 503, "machine_unavailable", "machine offline")
		return
	}
	setting := protocol.HarnessSetting{Tool: tool, Variant: variant, Enabled: *req.Enabled, OverridePath: req.OverridePath}
	conn.harnessSetMu.Lock()
	defer conn.harnessSetMu.Unlock()
	reqID := fmt.Sprintf("harness-%d", time.Now().UnixNano())
	result := make(chan protocol.Frame, 1)
	conn.harnessMu.Lock()
	conn.harnessPending[reqID] = result
	conn.harnessMu.Unlock()
	defer func() { conn.harnessMu.Lock(); delete(conn.harnessPending, reqID); conn.harnessMu.Unlock() }()
	if err := conn.send(protocol.Frame{V: protocol.Version, Type: protocol.HarnessSet, ReqID: reqID, HarnessSetting: &setting}); err != nil {
		apiError(w, 503, "machine_unavailable", "machine disconnected")
		return
	}
	var answer protocol.Frame
	select {
	case answer = <-result:
	case <-conn.done:
		apiError(w, 503, "machine_unavailable", "machine disconnected")
		return
	case <-r.Context().Done():
		return
	case <-time.After(10 * time.Second):
		apiError(w, 503, "machine_unavailable", "supervisor did not validate harness setting")
		return
	}
	if answer.Code != "" || answer.Harness == nil {
		apiError(w, 400, "invalid_path", answer.Message)
		return
	}
	_, err = s.Store.SetHarnessSetting(r.Context(), id, userID(r), setting)
	if err != nil {
		fail(w, err)
		return
	}
	conn.harnessMu.Lock()
	for i := range conn.harnesses {
		if conn.harnesses[i].Tool == tool && conn.harnesses[i].Variant == variant {
			conn.harnesses[i] = *answer.Harness
			break
		}
	}
	rows := append([]protocol.HarnessRow(nil), conn.harnesses...)
	conn.harnessMu.Unlock()
	if err := s.Store.RecordMachineHarnesses(r.Context(), id, rows); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, answer.Harness)
}

func listedHarness(tool, variant string) bool {
	if variant != "native" && variant != "docker" {
		return false
	}
	return tool == "claude" || tool == "codex" || tool == "pi" || tool == "opencode" || tool == "vibe"
}

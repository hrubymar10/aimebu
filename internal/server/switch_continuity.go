package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

func (s *Server) agentSwitchPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RoomID     string `json:"room_id"`
		Tool       string `json:"tool"`
		Variant    string `json:"variant"`
		LauncherID string `json:"launcher_id"`
		Model      string `json:"model"`
		MachineID  string `json:"machine_id"`
		Rev        int64  `json:"rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := s.Store.AuthorizeAgentControl(r.Context(), id, userID(r)); err != nil {
		spawnError(w, err)
		return
	}
	card, err := s.Store.AgentCardByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if req.Rev != card.DesiredRev {
		apiError(w, 409, "stale_rev", "agent changed; reload it")
		return
	}
	if req.RoomID == "" || req.Tool == "" || req.Model == "" {
		apiError(w, 400, "invalid_request", "room_id, tool, and model are required")
		return
	}
	listed, err := s.listedAdapter(r.Context(), spawnRequest{Tool: req.Tool, Variant: req.Variant, Model: req.Model})
	if err != nil {
		fail(w, err)
		return
	}
	if !listed {
		apiError(w, 400, "adapter_unlisted", "tool is not listed by an adapter")
		return
	}
	machineID := req.MachineID
	if machineID == "" {
		machineID = card.MachineID
	}
	selectedConn, machineInfo, err := s.selectedMachine(r.Context(), machineID, req.Tool, req.Variant, req.LauncherID)
	if err != nil {
		machineSelectionError(w, machineInfo, err)
		return
	}
	launcherID := ""
	var switchLauncher *store.Launcher
	if selectedConn.supportsLaunchers && req.Tool != "fake" {
		launcher, err := s.resolveLauncher(r.Context(), machineID, req.Tool, req.Variant, req.LauncherID)
		if err != nil {
			launcherSelectionError(w, err)
			return
		}
		launcherID = launcher.ID
		req.Variant = launcherAlias(launcher.Name)
		switchLauncher = &launcher
	}
	if refused, err := s.refusedNative(r.Context(), req.Tool, req.Variant, switchLauncher); err != nil {
		fail(w, err)
		return
	} else if refused {
		apiError(w, 409, "docker_only", errDockerOnly.Error())
		return
	}
	model, ok, err := s.checkSpawnModel(r.Context(), machineID, req.Tool, req.Variant, launcherID, req.Model)
	if err != nil {
		fail(w, err)
		return
	}
	if !ok {
		apiError(w, 400, "model_unlisted", "model is not offered by this harness")
		return
	}
	preview, err := s.Store.AgentSwitchPreview(r.Context(), id, userID(r), req.RoomID)
	if err != nil {
		spawnError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"handoff_status": preview.HandoffStatus, "history_window": preview.HistoryWindow, "room_id": req.RoomID, "handbook_included": preview.HandbookIncluded, "can_cancel": preview.CanCancel, "can_retry": preview.CanRetry, "can_continue": preview.CanContinue, "target": map[string]string{"tool": req.Tool, "variant": req.Variant, "model": model, "machine_id": machineID, "launcher_id": launcherID}})
}

func (s *Server) agentSwitchProgress(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.AuthorizeAgentControl(r.Context(), id, userID(r)); err != nil {
		spawnError(w, err)
		return
	}
	card, err := s.Store.AgentCardByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"switch": card.Switch, "desired_rev": card.DesiredRev})
}

func (s *Server) agentSwitchAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "cancel" && action != "retry-handoff" && action != "continue" {
		apiError(w, 404, "not_found", "unknown switch action")
		return
	}
	var req struct {
		Rev int64 `json:"rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		apiError(w, 400, "invalid_request", "Idempotency-Key required (at most 200 characters)")
		return
	}
	id := r.PathValue("id")
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", id, action, req.Rev)))
	request := store.RequestKey{ActorKind: "user", ActorID: userID(r), Kind: "switch", Key: key, Digest: hex.EncodeToString(digest[:])}
	_, _, err := s.Store.WithRequestKey(r.Context(), request, func(tx *store.Tx) (string, error) {
		card, err := tx.SwitchContinuityAction(id, userID(r), action, req.Rev)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(card)
		return string(encoded), err
	})
	if err != nil {
		spawnError(w, err)
		return
	}
	card, err := s.Store.AgentCardByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if machine, err := s.machineForID(card.MachineID); err == nil {
		_ = machine.sendControl(protocol.Frame{V: protocol.Version, Type: protocol.Control, AgentID: id, Generation: card.Generation, DesiredRev: card.DesiredRev, DesiredState: card.Desired})
	}
	writeJSON(w, 200, card)
}

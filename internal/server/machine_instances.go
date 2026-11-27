package server

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/hrubymar10/aimebu/v2/internal/buildversion"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

type instanceAgentRow struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

// instanceRow is one supervisor instance of a machine as Settings → Machines shows it, with what marking it gone would lose.
type instanceRow struct {
	JournalID     string             `json:"journal_id"`
	Build         string             `json:"build,omitempty"`
	ServerVersion string             `json:"server_version,omitempty"`
	OlderBuild    bool               `json:"older_build"`
	Environment   string             `json:"environment,omitempty"`
	State         string             `json:"state"`
	Connected     bool               `json:"connected"`
	FirstSeenAt   string             `json:"first_seen_at"`
	LastSeenAt    string             `json:"last_seen_at"`
	GoneAt        string             `json:"gone_at,omitempty"`
	Turns         int                `json:"turns"`
	Commands      int                `json:"commands"`
	Agents        []instanceAgentRow `json:"agents"`
}

// instanceConnected says whether a supervisor of the machine holding this journal is connected right now.
func (s *Server) instanceConnected(machineID, journalID string) bool {
	for _, c := range s.machineConnections(machineID) {
		if c.journalID == journalID {
			return true
		}
	}
	return false
}

// machineForInstances is the owner-or-admin gate and the machine lookup the two instance routes share.
func (s *Server) machineForInstances(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.adminMachine(w, r) {
		return "", false
	}
	machine, err := s.Store.MachineByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "machine not found")
		return "", false
	}
	if err != nil {
		fail(w, err)
		return "", false
	}
	return machine.ID, true
}

// GET /api/machines/{id}/instances: a remote machine's supervisor instances, oldest first, each with whether it is connected and, for one that
// is not gone, the open turns and commands it owns and their agents: what a write-off would lose. For the owner and the admins.
func (s *Server) machineInstances(w http.ResponseWriter, r *http.Request) {
	machineID, ok := s.machineForInstances(w, r)
	if !ok {
		return
	}
	items, err := s.Store.SupervisorInstances(r.Context(), machineID)
	if err != nil {
		fail(w, err)
		return
	}
	rows := make([]instanceRow, 0, len(items))
	for _, item := range items {
		row := instanceRow{JournalID: item.JournalID, Build: item.Build, ServerVersion: s.Version, OlderBuild: buildversion.Older(item.Build, s.Version), Environment: item.Environment, State: item.State, FirstSeenAt: item.FirstSeenAt,
			LastSeenAt: item.LastSeenAt, GoneAt: item.GoneAt, Connected: s.instanceConnected(machineID, item.JournalID), Agents: []instanceAgentRow{}}
		if item.State != "gone" {
			load, err := s.Store.SupervisorInstanceLoad(r.Context(), machineID, item.JournalID)
			if err != nil {
				fail(w, err)
				return
			}
			row.Turns, row.Commands = load.Turns, load.Commands
			for _, agent := range load.Agents {
				row.Agents = append(row.Agents, instanceAgentRow{ID: agent.ID, Slug: agent.Slug})
			}
		}
		rows = append(rows, row)
	}
	writeJSON(w, 200, map[string]any{"instances": rows})
}

// POST /api/machines/{id}/instances/{journal}/gone: the owner or an admin writes off one supervisor instance of a remote machine whose journal
// is lost for good (a replaced disk, a reinstall). It goes through MarkSupervisorInstanceGone, so the open turns it owns become lost with the
// recovery path and a supervisor_journal_changed event each, and its open commands end. It is refused with 409 instance_connected while the
// instance is connected (it is alive; stop it instead) and with 409 instance_recently_seen when it said hello or its connection ended in the
// last minute, decided in
// the write-off's own transaction so a hello or a disconnect that lands after the connected check cannot lose a live instance's turns, 404 for an instance the machine never had, and is idempotent: a second call answers
// 200 with already_gone and ends nothing. The decision is recorded as a supervisor_instance_gone event with the person's id.
func (s *Server) machineInstanceGone(w http.ResponseWriter, r *http.Request) {
	machineID, ok := s.machineForInstances(w, r)
	if !ok {
		return
	}
	machine, err := s.Store.MachineByID(r.Context(), machineID)
	if err != nil {
		fail(w, err)
		return
	}
	if machine.IsHost {
		apiError(w, 409, "host_machine", "the server's own supervisor instances are kept by the server; only a remote machine's instance is written off by hand")
		return
	}
	journalID := r.PathValue("journal")
	items, err := s.Store.SupervisorInstances(r.Context(), machineID)
	if err != nil {
		fail(w, err)
		return
	}
	known := false
	for _, item := range items {
		known = known || item.JournalID == journalID
	}
	if !known {
		apiError(w, 404, "not_found", "supervisor instance not found")
		return
	}
	if s.instanceConnected(machineID, journalID) {
		apiError(w, 409, "instance_connected", "this instance is connected, so it is not lost: stop it, or wait until it has disconnected")
		return
	}
	result, err := s.Store.MarkSupervisorInstanceGoneBy(r.Context(), machineID, journalID, userID(r), store.InstanceQuietFor)
	if errors.Is(err, store.ErrInstanceRecentlySeen) {
		// It said hello after the check above, or its connection ended just now: it runs (or ran a moment ago), and the write-off was refused inside the transaction.
		apiError(w, 409, "instance_recently_seen", "this instance was seen in the last minute, so it is not lost: wait until it has been silent for a minute")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"journal_id": journalID, "already_gone": result.AlreadyGone, "turns_lost": result.Turns, "commands_ended": result.Jobs})
}

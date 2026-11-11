package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"time"

	"github.com/hrubymar10/aimebu/internal/pathlist"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

// directoryList offers folder names only to people who can create or edit
// projects. The host is local to this server; remote paths go to that machine.
func (s *Server) directoryList(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	path := r.URL.Query().Get("path")
	prefix := r.URL.Query().Get("prefix")
	if !filepath.IsAbs(path) {
		apiError(w, 400, "invalid_path", "an absolute folder path is required")
		return
	}
	if prefix != "" && (prefix == ".." || filepath.Base(prefix) != prefix) {
		apiError(w, 400, "invalid_path", "a single directory prefix is required")
		return
	}
	machines, err := s.Store.Machines(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	machineID := r.URL.Query().Get("machine_id")
	var host bool
	var found bool
	for _, machine := range machines {
		if machineID == "" && machine.IsHost || machine.ID == machineID {
			if !machine.Approved || machine.RevokedAt.Valid {
				break
			}
			host, found = machine.IsHost, true
			machineID = machine.ID
			break
		}
	}
	if !found {
		apiError(w, 404, "not_found", "machine not found")
		return
	}
	if host {
		writeJSON(w, 200, map[string]any{"directories": pathlist.Directories(path, prefix)})
		return
	}
	conn, err := s.machineForID(machineID)
	if err != nil {
		apiError(w, 503, "machine_unavailable", "machine cannot list folders")
		return
	}
	if !conn.supportsDirectoryList {
		writeJSON(w, 200, map[string]any{"directories": []string{}})
		return
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		fail(w, err)
		return
	}
	id := hex.EncodeToString(nonce[:])
	answer := make(chan protocol.Frame, 1)
	conn.harnessMu.Lock()
	if conn.pathPending == nil {
		conn.pathPending = make(map[string]chan protocol.Frame)
	}
	conn.pathPending[id] = answer
	conn.harnessMu.Unlock()
	defer func() { conn.harnessMu.Lock(); delete(conn.pathPending, id); conn.harnessMu.Unlock() }()
	if err := conn.send(protocol.Frame{V: protocol.Version, Type: protocol.DirectoryList, ReqID: id, CheckoutPath: path, DirectoryPrefix: prefix}); err != nil {
		apiError(w, 503, "machine_unavailable", "machine cannot list folders")
		return
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case reply := <-answer:
		writeJSON(w, 200, map[string]any{"directories": reply.DirectoryNames})
	case <-r.Context().Done():
	case <-conn.done:
		apiError(w, 503, "machine_unavailable", "machine cannot list folders")
	case <-timer.C:
		apiError(w, 503, "machine_unavailable", "machine cannot list folders")
	}
}

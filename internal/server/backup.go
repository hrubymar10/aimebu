package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// backupDatabase streams a copy made by the server's live SQLite connection.
// The doctor key is restricted to this host and never accepted via a proxy.
func (s *Server) backupDatabase(w http.ResponseWriter, r *http.Request) {
	if !s.doctorAuthorized(r) {
		apiError(w, http.StatusForbidden, "forbidden", "a same-host request with the doctor key is required")
		return
	}
	if !s.backupActive.CompareAndSwap(false, true) {
		apiError(w, http.StatusTooManyRequests, "backup_busy", "a database snapshot is already running")
		return
	}
	defer s.backupActive.Store(false)
	dir := filepath.Join(filepath.Dir(s.Store.DatabasePath()), "backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		fail(w, err)
		return
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		fail(w, fmt.Errorf("unsafe backup directory %s: %v", dir, err))
		return
	}
	if err := os.Chmod(dir, 0700); err != nil {
		fail(w, err)
		return
	}
	file, err := os.CreateTemp(dir, ".backup-snapshot-*.sqlite")
	if err != nil {
		fail(w, err)
		return
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(path)
		fail(w, err)
		return
	}
	if err := os.Remove(path); err != nil {
		fail(w, err)
		return
	} // VACUUM INTO requires an absent path.
	defer os.Remove(path)
	snapshot := s.backupSnapshot
	if snapshot == nil {
		snapshot = s.Store.Snapshot
	}
	if err := snapshot(r.Context(), path); err != nil {
		fail(w, err)
		return
	}
	if err := os.Chmod(path, 0600); err != nil {
		fail(w, err)
		return
	}
	file, err = os.Open(path)
	if err != nil {
		fail(w, err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(info.Size()))
	stream := s.backupStream
	if stream == nil {
		stream = io.Copy
	}
	_, _ = stream(w, file)
}

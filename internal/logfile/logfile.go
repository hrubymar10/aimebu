// Package logfile is the server's log file: an append-only file that is capped in size by rotation, so a server that runs for months
// does not fill a disk. When a write would take the file past its cap, the file moves to name.1 (the older ones shift to name.2, ...,
// and the oldest is dropped) and a new file starts.
package logfile

import (
	"fmt"
	"os"
	"strconv"
	"sync"
)

// Defaults for the server's log: 10 MiB a file, three older files kept.
const (
	DefaultMaxBytes = 10 << 20
	DefaultBackups  = 3
)

// Names are the file names a log called name can have: the file itself and its backups, newest first.
func Names(name string, backups int) []string {
	names := []string{name}
	for i := 1; i <= backups; i++ {
		names = append(names, name+"."+strconv.Itoa(i))
	}
	return names
}

// Writer is a rotating log file. It is safe for concurrent use, and a failure to write is returned, never fatal: a log that cannot be
// written must not stop the server it reports on.
type Writer struct {
	mu       sync.Mutex
	path     string
	max      int64
	backups  int
	file     *os.File
	size     int64
	rotating bool // false until rotation is enabled: see OpenAppendOnly
}

// Open opens (creating it, mode 0600) the log at path, appending to what is there. A file that is already past maxBytes is rotated first.
func Open(path string, maxBytes int64, backups int) (*Writer, error) {
	if maxBytes <= 0 || backups < 1 {
		return nil, fmt.Errorf("log file %s: need a positive size cap and at least one backup", path)
	}
	w := &Writer{path: path, max: maxBytes, backups: backups, rotating: true}
	if err := w.open(); err != nil {
		return nil, err
	}
	if w.size >= w.max {
		if err := w.rotate(); err != nil {
			w.file.Close()
			return nil, err
		}
	}
	return w, nil
}

// OpenAppendOnly opens the log like Open but never rotates it until EnableRotation: a process that does not yet know whether it is the
// one that owns the log (it may lose a race to start, and exit) may add its first lines but must not move the file another process is
// writing to.
func OpenAppendOnly(path string, maxBytes int64, backups int) (*Writer, error) {
	if maxBytes <= 0 || backups < 1 {
		return nil, fmt.Errorf("log file %s: need a positive size cap and at least one backup", path)
	}
	w := &Writer{path: path, max: maxBytes, backups: backups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// EnableRotation turns rotation on, rotating at once if the file is already past its cap.
func (w *Writer) EnableRotation() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rotating = true
	if w.file != nil && w.size >= w.max {
		return w.rotate()
	}
	return nil
}

func (w *Writer) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("open log file: %w", err)
	}
	w.file, w.size = file, info.Size()
	return nil
}

// rotate moves the file to .1 and starts a new one; the caller holds the lock (or is still constructing the writer).
func (w *Writer) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	names := Names(w.path, w.backups)
	_ = os.Remove(names[len(names)-1])
	for i := len(names) - 2; i >= 0; i-- {
		if err := os.Rename(names[i], names[i+1]); err != nil && !os.IsNotExist(err) {
			// Start a new file anyway: losing the rotation is better than losing the log.
			_ = w.open()
			return fmt.Errorf("rotate log file: %w", err)
		}
	}
	return w.open()
}

// Write appends p, rotating first when it would take the file past its cap. One write is never split across two files.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.rotating && w.size > 0 && w.size+int64(len(p)) > w.max {
		if err := w.rotate(); err != nil && w.file == nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

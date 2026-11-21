package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/logfile"
)

const serverLogName = "aimebu.log"

// serverLogPath is the server's log file inside a config root: server/aimebu.log, capped in size by rotation (see internal/logfile).
func serverLogPath(root string) string { return filepath.Join(root, "server", serverLogName) }

// keptServerLogs are the file names the log can have (the file and its rotated backups), which `aimebu prune -a` keeps like any other
// diagnostics.
func keptServerLogs() []string { return logfile.Names(serverLogName, logfile.DefaultBackups) }

// serverLog is the server's log file attached to what it prints.
type serverLog struct {
	stdout, stderr io.Writer // where the server prints now: where it did, and the file
	// enableRotation starts capping the file. Until then the server only appends to it (see logfile.OpenAppendOnly): it is called once
	// the server holds the writer lock, so a start that loses the race to run never moves the log of the one that won.
	enableRotation func()
	close          func() // stops logging and closes the file
}

// attachServerLog makes the server write what it prints, and what the rest of the process logs through the standard logger, to the log
// file as well as to where it already went (a terminal, or nowhere for a daemon). Lines printed through the returned writers carry a
// timestamp in the file. A log that cannot be opened is reported once and the server runs without one: a diagnostic aid must not stop the
// server.
func attachServerLog(root string, stdout, stderr io.Writer) serverLog {
	none := serverLog{stdout: stdout, stderr: stderr, enableRotation: func() {}, close: func() {}}
	if err := os.MkdirAll(filepath.Dir(serverLogPath(root)), 0700); err != nil {
		fmt.Fprintf(stderr, "warning: no log file: %v\n", err)
		return none
	}
	file, err := logfile.OpenAppendOnly(serverLogPath(root), logfile.DefaultMaxBytes, logfile.DefaultBackups)
	if err != nil {
		fmt.Fprintf(stderr, "warning: no log file: %v\n", err)
		return none
	}
	quiet := tolerantWriter{file}
	stamped := &stampedWriter{w: quiet, now: time.Now}
	previous := log.Writer()
	log.SetOutput(io.MultiWriter(previous, quiet))
	return serverLog{
		stdout: io.MultiWriter(stdout, stamped), stderr: io.MultiWriter(stderr, stamped),
		enableRotation: func() {
			if err := file.EnableRotation(); err != nil {
				fmt.Fprintf(stderr, "warning: log rotation: %v\n", err)
			}
		},
		close: func() {
			log.SetOutput(previous)
			file.Close()
		},
	}
}

// tolerantWriter never reports a failure: one tee target that cannot be written must not cut the others short (io.MultiWriter stops at
// the first error) or fail the print that reached it.
type tolerantWriter struct{ w io.Writer }

func (t tolerantWriter) Write(p []byte) (int, error) {
	_, _ = t.w.Write(p)
	return len(p), nil
}

// stampedWriter puts a timestamp at the start of every line it passes on.
type stampedWriter struct {
	mu      sync.Mutex
	w       io.Writer
	now     func() time.Time
	midLine bool
}

func (s *stampedWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p) // what the caller wrote, not what was written with the timestamps added
	var out []byte
	for len(p) > 0 {
		if !s.midLine {
			out = s.now().UTC().AppendFormat(out, "2006-01-02T15:04:05Z ")
		}
		end := len(p)
		for i, b := range p {
			if b == '\n' {
				end = i + 1
				break
			}
		}
		out = append(out, p[:end]...)
		s.midLine = p[end-1] != '\n'
		p = p[end:]
	}
	_, _ = s.w.Write(out)
	return n, nil
}

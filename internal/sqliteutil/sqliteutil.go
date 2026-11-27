// Package sqliteutil holds the two things every place that opens a SQLite file outside the server store needs: a DSN that
// always carries a busy timeout, and a bounded retry for the "database is locked" answers SQLite gives without consulting
// that timeout.
package sqliteutil

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// BusyTimeout is how long SQLite itself waits for a lock held by another connection.
	BusyTimeout = 5 * time.Second
	// retryAttempts and retryInterval bound Retry: about two seconds in all.
	retryAttempts = 40
	retryInterval = 50 * time.Millisecond
)

// DSN returns a file: DSN for path that sets the busy timeout, read-only when readOnly. Use it for every connection that is
// not opened through store.Open.
func DSN(path string, readOnly bool) string {
	// A file: URI reads the first segment of a relative path as its authority ("file://srv/a.sqlite" is host "srv"), so the path is made absolute first.
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	u := &url.URL{Scheme: "file", Path: path}
	query := u.Query()
	if readOnly {
		query.Set("mode", "ro")
	}
	query.Add("_pragma", "busy_timeout("+strconv.Itoa(int(BusyTimeout/time.Millisecond))+")")
	u.RawQuery = query.Encode()
	return u.String()
}

// ImmutableWhenNoWAL appends immutable=1 to a read-only dsn for path when no write-ahead log with content sits next to the file. A
// read-only connection to a WAL-mode database otherwise creates (and leaves behind) the -shm and -wal files, which a question
// about a database that nothing is writing must not do. Immutable is only right for a reader that knows nothing is changing the
// file, or that reads only what no writer changes (a schema table); a leftover non-empty log is read the ordinary way so its
// pages are not ignored.
func ImmutableWhenNoWAL(dsn, path string) string {
	if info, err := os.Stat(path + "-wal"); os.IsNotExist(err) || (err == nil && info.Size() == 0) {
		return dsn + "&immutable=1"
	}
	return dsn
}

// IsBusy reports whether err is SQLite's "locked" or "busy" answer.
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "database is locked") || strings.Contains(text, "database is busy") || strings.Contains(text, "SQLITE_BUSY") || strings.Contains(text, "SQLITE_LOCKED")
}

// Retry runs fn and repeats it, for about two seconds, while it fails with a locked or busy answer. A read-only connection
// opened while another connection is recovering the write-ahead log gets that answer at once, whatever the busy timeout.
// Any other error, or success, returns immediately.
func Retry(fn func() error) error {
	var err error
	for attempt := 0; attempt < retryAttempts; attempt++ {
		if err = fn(); !IsBusy(err) {
			return err
		}
		time.Sleep(retryInterval)
	}
	return err
}

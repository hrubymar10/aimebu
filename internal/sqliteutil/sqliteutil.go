// Package sqliteutil holds the two things every place that opens a SQLite file outside the server store needs: a DSN that
// always carries a busy timeout, and a bounded retry for the "database is locked" answers SQLite gives without consulting
// that timeout.
package sqliteutil

import (
	"net/url"
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
	u := &url.URL{Scheme: "file", Path: path}
	query := u.Query()
	if readOnly {
		query.Set("mode", "ro")
	}
	query.Add("_pragma", "busy_timeout("+strconv.Itoa(int(BusyTimeout/time.Millisecond))+")")
	u.RawQuery = query.Encode()
	return u.String()
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

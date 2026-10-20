package sqliteutil

import (
	"errors"
	"strings"
	"testing"
)

func TestDSNAlwaysCarriesTheBusyTimeout(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		dsn := DSN("/tmp/a b/x.sqlite", readOnly)
		if !strings.Contains(dsn, "busy_timeout%285000%29") && !strings.Contains(dsn, "busy_timeout(5000)") {
			t.Fatalf("no busy timeout in %q", dsn)
		}
		if strings.Contains(dsn, "mode=ro") != readOnly {
			t.Fatalf("read-only = %v but DSN is %q", readOnly, dsn)
		}
	}
}

func TestRetryRepeatsOnlyBusyAnswers(t *testing.T) {
	calls := 0
	err := Retry(func() error {
		calls++
		if calls < 3 {
			return errors.New("database is locked (261)")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d: a busy answer must be retried until it clears", err, calls)
	}
	calls = 0
	other := errors.New("no such table: x")
	if err := Retry(func() error { calls++; return other }); !errors.Is(err, other) || calls != 1 {
		t.Fatalf("err=%v calls=%d: any other error returns at once", err, calls)
	}
}

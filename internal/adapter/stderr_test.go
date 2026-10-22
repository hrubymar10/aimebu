package adapter

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestStderrTailBoundsAndRedactsCredentials(t *testing.T) {
	var tail StderrTail
	_, _ = tail.Write([]byte(strings.Repeat("x", 5000)))
	_, _ = tail.Write([]byte("\nAuthorization: Bearer aak_secret\nkey=aak_another\nDocker is not running\n"))
	got := tail.String()
	if len(got) > 4096 || strings.Contains(got, "aak_secret") || strings.Contains(got, "aak_another") || !strings.Contains(got, "Docker is not running") {
		t.Fatalf("unsafe stderr tail: %q", got)
	}
}

func TestRedactSecretsKeepsTheRestOfTheText(t *testing.T) {
	got := RedactSecrets("key=aak_abc-123_x done\nAuthorization: Bearer eyJ.tok.en and more\nbearer\tlower-token ok")
	if strings.Contains(got, "aak_abc") || strings.Contains(got, "eyJ.tok.en") || strings.Contains(got, "lower-token") {
		t.Fatalf("a secret survived: %q", got)
	}
	for _, kept := range []string{"key=[redacted] done", "Authorization: Bearer [redacted] and more", "ok"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("%q was not kept in %q", kept, got)
		}
	}
	if RedactSecrets("a\xffb") != "a�b" {
		t.Fatalf("invalid UTF-8 was not replaced")
	}
}

type failingWriter struct{ writes int }

func (f *failingWriter) Write(p []byte) (int, error) { f.writes++; return 0, errors.New("disk full") }

func TestJoinWritersNeverLetsOneFailureStopTheOthers(t *testing.T) {
	if JoinWriters() != nil || JoinWriters(nil, nil) != nil {
		t.Fatal("no writers is nil")
	}
	var kept bytes.Buffer
	failing := &failingWriter{}
	joined := JoinWriters(failing, nil, &kept)
	n, err := io.Copy(joined, strings.NewReader("one\ntwo\n"))
	if err != nil || n != 8 || kept.String() != "one\ntwo\n" || failing.writes == 0 {
		t.Fatalf("copy = %d, %v; kept %q; failing wrote %d times", n, err, kept.String(), failing.writes)
	}
	// A single failing writer still reports success to the copy that feeds it: the harness's stderr pipe must keep being read.
	if _, err := JoinWriters(&failingWriter{}).Write([]byte("x")); err != nil {
		t.Fatalf("a failing capture failed the copy: %v", err)
	}
}

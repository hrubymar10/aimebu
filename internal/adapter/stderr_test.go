package adapter

import (
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

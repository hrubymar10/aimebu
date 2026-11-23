package adapter

import (
	"strings"
	"testing"
)

const testContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// The identity takes the container's ID from the launcher's exec ID line, and takes that line out of the stream.
func TestContainerIdentityReadsTheLaunchersContainer(t *testing.T) {
	stream := "relay healed\nHARNESS_DOCKER_EXEC_ID=" + testContainerID + ":42-7-99\nharness says hi\n"
	for _, size := range []int{1, 5, len(stream)} {
		identity := &ContainerIdentity{}
		var out strings.Builder
		filter := identity.Filter(&out)
		for i := 0; i < len(stream); i += size {
			_, _ = filter.Write([]byte(stream[i:min(i+size, len(stream))]))
		}
		if id, never := identity.Container(); id != testContainerID || never {
			t.Fatalf("chunk %d: before the end of the stream: id %q never started %t", size, id, never)
		}
		_ = filter.Close()
		if id, never := identity.Container(); id != testContainerID || never {
			t.Fatalf("chunk %d: id %q never started %t", size, id, never)
		}
		if want := "relay healed\nharness says hi\n"; out.String() != want {
			t.Fatalf("chunk %d: output %q", size, out.String())
		}
	}
}

// A launcher that was asked and ended without the line never reached its `docker exec`. Anything less certain says nothing: a stream that is
// still open, one the filter gave up on, one that ends in a cut-off line, and a container given by name, which a new container can carry too.
func TestContainerIdentityNeverGuesses(t *testing.T) {
	cases := []struct {
		name, stream string
		close        bool
		id           string
		never        bool
	}{
		{"ended without the line", "Error: Harness container is removing. Run: bin/harness-docker-ctrl start\n", true, "", true},
		{"ended with no output at all", "", true, "", true},
		{"ended on a last line without a newline", "Error: Docker is not running.", true, "", true},
		{"still open", "starting\n", false, "", false},
		{"a short ID", "HARNESS_DOCKER_EXEC_ID=0123456789ab:1-2-3\n", true, "0123456789ab", false},
		{"a container name", "HARNESS_DOCKER_EXEC_ID=harness-docker-g7-harness-1:1-2-3\n", true, "", false},
		{"an overlong line", strings.Repeat("x", 5000), true, "", false},
		{"too much output before it", strings.Repeat("line\n", 14000), true, "", false},
		{"a cut-off exec ID line", "HARNESS_DOCKER_EXEC_ID=0123456789ab", true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			identity := &ContainerIdentity{}
			var out strings.Builder
			filter := identity.Filter(&out)
			_, _ = filter.Write([]byte(tc.stream))
			if tc.close {
				_ = filter.Close()
			}
			if id, never := identity.Container(); id != tc.id || never != tc.never {
				t.Fatalf("id %q never started %t, want %q %t", id, never, tc.id, tc.never)
			}
		})
	}
	// Not asked: nothing is known, and nothing is taken out of the stream.
	var none *ContainerIdentity
	var plain strings.Builder
	filter := none.Filter(&plain)
	_, _ = filter.Write([]byte("HARNESS_DOCKER_EXEC_ID=0123456789ab:1\n"))
	_ = filter.Close()
	if id, never := none.Container(); id != "" || never || plain.String() != "HARNESS_DOCKER_EXEC_ID=0123456789ab:1\n" {
		t.Fatalf("a launcher that was not asked: id %q never started %t output %q", id, never, plain.String())
	}
}

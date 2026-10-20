package usages

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func TestRemovedProfileTombstonesAreBoundedAndHashed(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < maxRemovedProfiles+2; i++ {
		credentials := []byte(fmt.Sprintf(`{"tokens":{"refresh_token":"secret-%d"}}`, i))
		if err := RecordRemovedProfile(root, "codex", fmt.Sprintf("profile-%d", i), credentials); err != nil {
			t.Fatal(err)
		}
	}
	file, err := readRemovedProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Entries) != maxRemovedProfiles {
		t.Fatalf("tombstone count = %d", len(file.Entries))
	}
	if file.Entries[0].Name != "profile-2" {
		t.Fatalf("oldest retained = %q", file.Entries[0].Name)
	}
	data, err := os.ReadFile(filepath.Join(root, "removed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-") {
		t.Fatal("removed.json contains a refresh token")
	}
	if removed, err := WasRefreshTokenRemoved(root, "codex", "secret-51"); err != nil || !removed {
		t.Fatalf("recent removal = %v, %v", removed, err)
	}
	if removed, err := WasRefreshTokenRemoved(root, "codex", "secret-0"); err != nil || removed {
		t.Fatalf("oldest removal = %v, %v", removed, err)
	}
}

// v1 and v2 share switcher/removed.json, so the on-disk bytes must match what
// v1 writes: two-space indent, field order, trailing newline, RFC 3339 times.
func TestRemovedProfilesFileIsByteCompatibleWithV1(t *testing.T) {
	root := t.TempDir()
	file := removedProfilesFile{Entries: []removedProfile{{
		Tool: "claude", Name: "main", TokenHash: refreshTokenHash("tok"),
		RemovedAt: time.Date(2026, 10, 15, 20, 57, 28, 123456789, time.UTC),
	}}}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"entries\": [\n    {\n      \"tool\": \"claude\",\n      \"name\": \"main\",\n" +
		"      \"token_sha256\": \"" + refreshTokenHash("tok") + "\",\n" +
		"      \"removed_at\": \"2026-10-15T20:57:28.123456789Z\"\n    }\n  ]\n}"
	if string(data) != want {
		t.Fatalf("marshal mismatch:\n%s\nwant:\n%s", data, want)
	}
	// A v1-written file must be read back and extended in place.
	if err := os.WriteFile(filepath.Join(root, "removed.json"), []byte(want+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := WasRefreshTokenRemoved(root, "claude", "tok"); err != nil || !removed {
		t.Fatalf("v1 tombstone not honoured: %v, %v", removed, err)
	}
	creds := []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"tok2"}}`)
	if err := RecordRemovedProfile(root, "claude", "other", creds); err != nil {
		t.Fatal(err)
	}
	got, err := readRemovedProfiles(root)
	if err != nil || len(got.Entries) != 2 {
		t.Fatalf("entries = %+v, %v", got.Entries, err)
	}
}

func TestRotatedRescueNamesRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 15, 20, 57, 28, 5, time.UTC)
	name := RotatedRescueName("a", at)
	if name != "a-rotated-20261015T205728.000000005Z.json" {
		t.Fatalf("name = %q", name)
	}
	if got, ok := RotatedRescueTimestamp(name, "a"); !ok || !got.Equal(at) {
		t.Fatalf("parse = %v, %v", got, ok)
	}
	if _, ok := RotatedRescueTimestamp(name, "a-rotated"); ok {
		t.Fatal("similar profile name matched")
	}
}
